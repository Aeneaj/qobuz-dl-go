package downloader

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/vbauerster/mpb/v8"

	"github.com/Aeneaj/qobuz-dl-go/internal/ui"
)

// trackJob bundles per-track info collected in Phase 1 and consumed in Phase 2
// of an album download.
type trackJob struct {
	idx      int
	trackURL map[string]interface{}
	track    map[string]interface{}
	trackDir string
	trackID  string
	bar      ProgressBar
}

func (d *Downloader) downloadAlbum(ctx context.Context, albumID, baseDir string) error {
	meta, err := d.Client.GetAlbumMeta(ctx, albumID)
	if err != nil {
		return fmt.Errorf("album metadata %s: %w", albumID, err)
	}

	if d.shouldSkipAlbum(meta, albumID) {
		return nil
	}

	// Resolve format info from first track
	fileFormat, bitDepth, samplingRate := d.resolveFormat(ctx, meta)
	title := getTitle(meta)
	artist := nestedStr(meta, "artist", "name")
	year := releaseYear(meta)

	trackCount := 0
	if items, _ := meta["tracks"].(map[string]interface{}); items != nil {
		if raw, _ := items["items"].([]interface{}); raw != nil {
			trackCount = len(raw)
		}
	}
	d.announceAlbum(title, artist, fmt.Sprintf("%s %v/%v", fileFormat, bitDepth, samplingRate), trackCount)

	// Build folder name. Individual values are sanitised inside
	// expandPlaceholders, so literal "/" written in FolderFormat survives as a
	// subfolder separator (translated to the OS separator by filepath.FromSlash).
	folderFmt := cleanFormatStr(d.Opts.FolderFormat, fileFormat)
	folderName := expandPlaceholders(folderFmt, map[string]string{
		"{artist}":        artist,
		"{album}":         title,
		"{year}":          year,
		"{bit_depth}":     fmt.Sprintf("%v", bitDepth),
		"{sampling_rate}": fmt.Sprintf("%v", samplingRate),
		"{format}":        fileFormat,
	})
	albumDir, err := safeJoin(baseDir, filepath.FromSlash(folderName))
	if err != nil {
		return fmt.Errorf("resolve album directory: %w", err)
	}
	if err := os.MkdirAll(albumDir, 0755); err != nil {
		return fmt.Errorf("create album directory %q: %w", albumDir, err)
	}

	d.downloadAlbumExtras(ctx, meta, albumDir)

	// Tracks
	tracklist, _ := meta["tracks"].(map[string]interface{})
	if tracklist == nil {
		return fmt.Errorf("no tracks in album %s", albumID)
	}
	rawItems, _ := tracklist["items"].([]interface{})

	isMultiDisc := detectMultiDisc(rawItems)
	trackFmt := cleanFormatStr(d.Opts.TrackFormat, fileFormat)
	isMP3 := d.Opts.Quality == 5

	p := d.newProgress(ctx)
	restore := d.withBars(p)
	jobs := d.collectTrackJobs(ctx, p, rawItems, albumDir, isMultiDisc, meta, trackFmt, isMP3)
	d.runTrackJobs(ctx, jobs, meta, isMP3, trackFmt)
	if p != nil {
		p.Wait()
	}
	restore()

	fmt.Fprintf(d.termOut(), "\033[32m✓  Completed: %s\033[0m\n\n", title)
	return nil
}

// shouldSkipAlbum applies the early validation gates (streamable flag and the
// IgnoreSingles filter). When the album must be skipped it prints the reason
// and returns true.
func (d *Downloader) shouldSkipAlbum(meta map[string]interface{}, albumID string) bool {
	if streamable, ok := meta["streamable"].(bool); ok && !streamable {
		fmt.Fprintf(d.termOut(), "\033[90mAlbum %s is not streamable, skipping\033[0m\n", albumID)
		return true
	}
	if d.Opts.IgnoreSingles {
		releaseType, _ := meta["release_type"].(string)
		artistName := nestedStr(meta, "artist", "name")
		if releaseType != "album" || artistName == "Various Artists" {
			title, _ := meta["title"].(string)
			fmt.Fprintf(d.termOut(), "\033[90mIgnoring Single/EP/VA: %s\033[0m\n", title)
			return true
		}
	}
	return false
}

// downloadAlbumExtras fetches the cover image and booklet PDF when present and
// not disabled by Options.
func (d *Downloader) downloadAlbumExtras(ctx context.Context, meta map[string]interface{}, albumDir string) {
	if !d.Opts.NoCover {
		if imgURL := nestedStr(meta, "image", "large"); imgURL != "" {
			if d.Opts.OGCover {
				imgURL = strings.Replace(imgURL, "_600.", "_org.", 1)
			}
			d.downloadExtra(ctx, imgURL, filepath.Join(albumDir, coverFile))
		}
	}
	if goodies, ok := meta["goodies"].([]interface{}); ok && len(goodies) > 0 {
		if g, ok := goodies[0].(map[string]interface{}); ok {
			if pdfURL, _ := g["url"].(string); pdfURL != "" {
				d.downloadExtra(ctx, pdfURL, filepath.Join(albumDir, bookletFile))
			}
		}
	}
}

// detectMultiDisc reports whether the tracklist spans more than one disc.
func detectMultiDisc(rawItems []interface{}) bool {
	mediaNumbers := map[float64]bool{}
	for _, t := range rawItems {
		if track, ok := t.(map[string]interface{}); ok {
			if mn, ok := track["media_number"].(float64); ok {
				mediaNumbers[mn] = true
			}
		}
	}
	return len(mediaNumbers) > 1
}

// collectTrackJobs is Phase 1 of an album download: it resolves track URLs,
// filters ineligible items (already downloaded, samples, zero-rate), creates
// per-track disc subdirectories on multi-disc albums, and registers a progress
// bar on p for each surviving track. Tracks whose URL cannot be resolved or
// whose disc directory cannot be created are reported and skipped.
//
// The skip check requires BOTH a DB entry and the file present on disk — this
// way, deleting an album from disk causes it to be re-downloaded on the next
// run instead of silently skipped (only the cover would end up recreated).
func (d *Downloader) collectTrackJobs(ctx context.Context, p *mpb.Progress, rawItems []interface{}, albumDir string, isMultiDisc bool, albumMeta map[string]interface{}, trackFmt string, isMP3 bool) []trackJob {
	var jobs []trackJob
	for idx, t := range rawItems {
		track, ok := t.(map[string]interface{})
		if !ok {
			continue
		}
		trackID := idStr(track["id"])

		// Compute the disc-aware directory before the skip check so the
		// existence probe looks at the exact path downloadAndTag will write.
		// MkdirAll for multi-disc dirs is deferred until we know we need it.
		trackDir := albumDir
		if isMultiDisc {
			mn := int(track["media_number"].(float64))
			trackDir = filepath.Join(albumDir, fmt.Sprintf("Disc %d", mn))
		}

		if finalPath, err := finalTrackPath(trackDir, track, albumMeta, trackFmt, isMP3); err == nil {
			if d.alreadyHave(trackID, finalPath) {
				continue
			}
		}

		trackURL, err := d.Client.GetTrackURL(ctx, trackID, d.Opts.Quality, "")
		if err != nil {
			if d.Opts.QualityFallback {
				trackURL, err = d.fallbackQuality(ctx, trackID)
			}
			if err != nil {
				fmt.Fprintf(d.termOut(), "\033[31mTrack %s: cannot get URL: %v. Skipping...\033[0m\n", trackID, err)
				continue
			}
		}
		if _, isSample := trackURL["sample"]; isSample {
			continue
		}
		if sr, _ := trackURL["sampling_rate"].(float64); sr == 0 {
			continue
		}

		if isMultiDisc {
			if err := os.MkdirAll(trackDir, 0755); err != nil {
				fmt.Fprintf(d.termOut(), "\033[31mTrack %s: cannot create disc directory %q: %v. Skipping...\033[0m\n", trackID, trackDir, err)
				continue
			}
		}

		trackNum := 0
		if tn, ok := track["track_number"].(float64); ok {
			trackNum = int(tn)
		}
		bar := d.newBar(p, idx, trackNum, getTitle(track), trackID)

		jobs = append(jobs, trackJob{idx, trackURL, track, trackDir, trackID, bar})
	}
	return jobs
}

// runTrackJobs is Phase 2 of an album download: it dispatches the collected
// jobs to a worker pool of size d.Opts.Workers, tags each track and records it
// in the DB on success. Cancellation aborts the dispatch loop without
// launching new goroutines; in-flight downloads observe ctx via downloadAndTag.
func (d *Downloader) runTrackJobs(ctx context.Context, jobs []trackJob, meta map[string]interface{}, isMP3 bool, trackFmt string) {
	sem := make(chan struct{}, d.Opts.Workers)
	var wg sync.WaitGroup

jobLoop:
	for _, job := range jobs {
		select {
		case <-ctx.Done():
			break jobLoop
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(j trackJob) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := d.downloadAndTag(ctx, j.trackDir, j.idx, j.trackURL, j.track, meta, false, isMP3, trackFmt, j.bar); err != nil {
				j.bar.Abort(false)
				fmt.Fprintf(d.termOut(), "\033[31mTrack %s failed: %v. Skipping...\033[0m\n", j.trackID, err)
			} else if d.db != nil {
				if err := d.db.add(j.trackID); err != nil {
					fmt.Fprintf(d.termOut(), "\033[33mWarning: could not record track in DB: %v\033[0m\n", err)
				}
			}
		}(job)
	}

	wg.Wait()
}

// ---- track download ----

func (d *Downloader) downloadTrackByID(ctx context.Context, trackID, baseDir string) error {
	// The skip check is deferred until trackDir + trackFmt are known so it
	// can look at the exact output path; see the alreadyHave call below.
	trackURL, err := d.Client.GetTrackURL(ctx, trackID, d.Opts.Quality, "")
	if err != nil {
		if d.Opts.QualityFallback {
			trackURL, err = d.fallbackQuality(ctx, trackID)
		}
		if err != nil {
			return fmt.Errorf("get track URL: %w", err)
		}
	}

	if _, isSample := trackURL["sample"]; isSample {
		fmt.Fprintf(d.termOut(), "\033[90mDemo track, skipping\033[0m\n")
		return nil
	}

	meta, err := d.Client.GetTrackMeta(ctx, trackID)
	if err != nil {
		return err
	}

	title := getTitle(meta)
	performer := nestedStr(meta, "performer", "name")
	if performer == "" {
		performer = nestedStr(meta, "album", "artist", "name")
	}

	bitDepth, _ := trackURL["bit_depth"].(float64)
	samplingRate, _ := trackURL["sampling_rate"].(float64)
	fileFormat := "FLAC"
	if d.Opts.Quality == 5 {
		fileFormat = "MP3"
	}

	albumTitle := nestedStr(meta, "album", "title")
	albumArtist := nestedStr(meta, "album", "artist", "name")
	year := ""
	if rd := nestedStr(meta, "album", "release_date_original"); len(rd) >= 4 {
		year = rd[:4]
	}

	folderFmt := cleanFormatStr(d.Opts.FolderFormat, fileFormat)
	folderName := expandPlaceholders(folderFmt, map[string]string{
		"{artist}":        albumArtist,
		"{album}":         albumTitle,
		"{year}":          year,
		"{bit_depth}":     fmt.Sprintf("%v", int(bitDepth)),
		"{sampling_rate}": fmt.Sprintf("%v", samplingRate),
	})
	trackDir, err := safeJoin(baseDir, filepath.FromSlash(folderName))
	if err != nil {
		return fmt.Errorf("resolve track directory: %w", err)
	}
	if err := os.MkdirAll(trackDir, 0755); err != nil {
		return fmt.Errorf("create track directory %q: %w", trackDir, err)
	}

	isMP3 := d.Opts.Quality == 5
	trackFmt := cleanFormatStr(d.Opts.TrackFormat, fileFormat)

	// Skip only if recorded in the DB AND the file is still on disk. Done
	// here (after trackDir + trackFmt are known) so the on-disk probe hits
	// the exact path downloadAndTag will write.
	if finalPath, err := finalTrackPath(trackDir, meta, meta, trackFmt, isMP3); err == nil {
		if d.alreadyHave(trackID, finalPath) {
			fmt.Fprintf(d.termOut(), "\033[90mTrack %s already downloaded, skipping\033[0m\n", trackID)
			return nil
		}
	}

	if !d.Opts.NoCover {
		if imgURL := nestedStr(meta, "album", "image", "large"); imgURL != "" {
			if d.Opts.OGCover {
				imgURL = strings.Replace(imgURL, "_600.", "_org.", 1)
			}
			d.downloadExtra(ctx, imgURL, filepath.Join(trackDir, coverFile))
		}
	}

	trackNum := 0
	if tn, ok := meta["track_number"].(float64); ok {
		trackNum = int(tn)
	}
	if d.tui != nil {
		d.tui.Send(ui.MsgAlbum{Title: title, Artist: performer, Format: fileFormat, Tracks: 1})
	} else {
		fmt.Fprintf(d.termOut(), "\n\033[1m♫  %s\033[0m  ·  \033[33m%s — %s\033[0m\n\n", title, performer, fileFormat)
	}

	p := d.newProgress(ctx)
	restore := d.withBars(p)
	bar := d.newBar(p, 0, trackNum, title, trackID)

	if err := d.downloadAndTag(ctx, trackDir, 1, trackURL, meta, meta, true, isMP3, trackFmt, bar); err != nil {
		bar.Abort(false)
		if p != nil {
			p.Wait()
		}
		restore()
		return err
	}
	if d.db != nil {
		if err := d.db.add(trackID); err != nil {
			fmt.Fprintf(d.termOut(), "\033[33mWarning: could not record track in DB: %v\033[0m\n", err)
		}
	}
	if p != nil {
		p.Wait()
	}
	restore()

	fmt.Fprintf(d.termOut(), "\033[32m✓  Completed: %s\033[0m\n\n", title)
	return nil
}

// ---- core download + tag ----

// finalTrackPath computes the on-disk path a track will be written to. It is
// the single source of truth for track output naming: downloadAndTag calls it
// when writing, and callers call it before the skip check so alreadyHave looks
// at the same path. Uses safeJoin + filepath.FromSlash so subfolder templates
// (e.g. "{albumartist}/{album}/{tracktitle}") work portably and cannot escape
// dir via path traversal.
func finalTrackPath(dir string, trackMeta, albumMeta map[string]interface{}, trackFmt string, isMP3 bool) (string, error) {
	ext := ".flac"
	if isMP3 {
		ext = ".mp3"
	}

	trackTitle := getTitle(trackMeta)
	performer := nestedStr(trackMeta, "performer", "name")
	if performer == "" {
		performer = nestedStr(albumMeta, "artist", "name")
	}
	trackNum := 0
	if tn, ok := trackMeta["track_number"].(float64); ok {
		trackNum = int(tn)
	}

	filenameAttrs := map[string]string{
		"{tracknumber}":   fmt.Sprintf("%02d", trackNum),
		"{tracktitle}":    trackTitle,
		"{artist}":        performer,
		"{albumartist}":   nestedStr(albumMeta, "artist", "name"),
		"{bit_depth}":     fmt.Sprintf("%v", trackMeta["maximum_bit_depth"]),
		"{sampling_rate}": fmt.Sprintf("%v", trackMeta["maximum_sampling_rate"]),
		"{version}":       fmt.Sprintf("%v", trackMeta["version"]),
	}
	formatted := expandPlaceholders(trackFmt, filenameAttrs)
	finalFile, err := safeJoin(dir, filepath.FromSlash(formatted))
	if err != nil {
		return "", fmt.Errorf("resolve track path: %w", err)
	}
	// Trim to 250 runes to stay within filesystem limits without splitting
	// multi-byte UTF-8 characters (e.g. CJK, Arabic, emoji in track titles).
	if runes := []rune(finalFile); len(runes) > 250 {
		finalFile = string(runes[:250])
	}
	return finalFile + ext, nil
}

// alreadyHave reports whether a track may be skipped: it must be recorded in
// the downloads DB AND its file must still be present on disk. When the DB
// has the entry but the file is gone (e.g. the user deleted the album), we
// print a note so it is clear why a "previously downloaded" track is being
// re-fetched, then return false so the caller re-downloads it.
func (d *Downloader) alreadyHave(trackID, finalFile string) bool {
	if d.db == nil || !d.db.has(trackID) {
		return false
	}
	if _, err := os.Stat(finalFile); err == nil {
		return true
	}
	fmt.Fprintf(d.termOut(), "\033[90mTrack %s in DB but file is missing — re-downloading\033[0m\n", trackID)
	return false
}

func (d *Downloader) downloadAndTag(
	ctx context.Context,
	dir string,
	idx int,
	trackURLDict map[string]interface{},
	trackMeta map[string]interface{},
	albumMeta map[string]interface{},
	isTrack bool,
	isMP3 bool,
	trackFmt string,
	bar ProgressBar,
) error {
	fileURL, _ := trackURLDict["url"].(string)
	if fileURL == "" {
		fmt.Fprintf(d.termOut(), "\033[90mTrack not available for download\033[0m\n")
		return nil
	}

	finalFile, err := finalTrackPath(dir, trackMeta, albumMeta, trackFmt, isMP3)
	if err != nil {
		return err
	}

	// Support subfolder templates in track_format (e.g. "{albumartist}/{album}/...").
	// safeJoin (inside finalTrackPath) already guaranteed the parent is inside dir.
	if parent := filepath.Dir(finalFile); parent != dir {
		if err := os.MkdirAll(parent, 0755); err != nil {
			return fmt.Errorf("create track parent directory %q: %w", parent, err)
		}
	}

	if _, err := os.Stat(finalFile); err == nil {
		if bar != nil {
			bar.Abort(true) // hide already-downloaded bars
		}
		return nil
	}

	// Download to .tmp file first
	tmpFile := filepath.Join(dir, fmt.Sprintf(".%02d.tmp", idx))
	if err := d.downloadWithProgress(ctx, fileURL, tmpFile, bar); err != nil {
		os.Remove(tmpFile)
		return fmt.Errorf("download: %w", err)
	}

	// Tag and rename
	if isMP3 {
		if err := tagMP3(tmpFile, dir, finalFile, trackMeta, albumMeta, isTrack, d.Opts.EmbedArt); err != nil {
			fmt.Fprintf(d.termOut(), "\033[31mWarning: could not tag %s: %v\033[0m\n", filepath.Base(finalFile), err)
			// Still rename even if tagging failed
			os.Rename(tmpFile, finalFile)
		}
	} else {
		if err := tagFLAC(tmpFile, dir, finalFile, trackMeta, albumMeta, isTrack, d.Opts.EmbedArt); err != nil {
			fmt.Fprintf(d.termOut(), "\033[31mWarning: could not tag %s: %v\033[0m\n", filepath.Base(finalFile), err)
			os.Rename(tmpFile, finalFile)
		}
	}

	return nil
}

// ---- quality fallback ----

func (d *Downloader) fallbackQuality(ctx context.Context, trackID string) (map[string]interface{}, error) {
	fallbacks := []int{27, 7, 6, 5}
	for _, q := range fallbacks {
		if q == d.Opts.Quality {
			continue
		}
		info, err := d.Client.GetTrackURL(ctx, trackID, q, "")
		if err == nil {
			fmt.Fprintf(d.termOut(), "\033[33mQuality fallback to %s for track %s\033[0m\n", qualities[q], trackID)
			return info, nil
		}
	}
	return nil, fmt.Errorf("no quality available for track %s", trackID)
}

// ---- format helpers ----

func (d *Downloader) resolveFormat(ctx context.Context, albumMeta map[string]interface{}) (fileFormat string, bitDepth, samplingRate interface{}) {
	if d.Opts.Quality == 5 {
		return "MP3", nil, nil
	}
	tracks, _ := albumMeta["tracks"].(map[string]interface{})
	if tracks == nil {
		return "Unknown", nil, nil
	}
	items, _ := tracks["items"].([]interface{})
	if len(items) == 0 {
		return "Unknown", nil, nil
	}
	firstTrack, _ := items[0].(map[string]interface{})
	if firstTrack == nil {
		return "Unknown", nil, nil
	}
	trackID := idStr(firstTrack["id"])
	info, err := d.Client.GetTrackURL(ctx, trackID, d.Opts.Quality, "")
	if err != nil {
		return "Unknown", nil, nil
	}

	// Check quality restriction
	if restrictions, ok := info["restrictions"].([]interface{}); ok {
		for _, r := range restrictions {
			rm, _ := r.(map[string]interface{})
			if code, _ := rm["code"].(string); code == qlDowngrade {
				fmt.Fprintf(d.termOut(), "\033[90mQuality downgraded for this release\033[0m\n")
			}
		}
	}
	return "FLAC", info["bit_depth"], info["sampling_rate"]
}
