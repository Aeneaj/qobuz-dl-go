package downloader

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/vbauerster/mpb/v8"
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
