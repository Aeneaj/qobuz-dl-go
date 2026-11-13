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

// trackJob is one track of an album as collectTrackJobs leaves it: named,
// checked against the DB and the disk, and with its progress bar — but with
// no link yet. Links are asked for in the pipeline, shortly before use.
type trackJob struct {
	idx      int
	track    map[string]interface{}
	trackDir string
	trackID  string
	bar      ProgressBar
}

// readyJob is a trackJob whose link has been resolved and whose directory
// exists: all that is left is the download.
type readyJob struct {
	trackJob
	trackURL map[string]interface{}
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
	fileFormat, bitDepth, samplingRate, asked := d.resolveFormat(ctx, meta)
	title := getTitle(meta)
	artist := nestedStr(meta, "artist", "name")
	year := releaseYear(meta)

	tracklist, _ := meta["tracks"].(map[string]interface{})
	rawItems, _ := tracklist["items"].([]interface{})

	// resolveFormat leaves bit depth / sampling rate nil for MP3 and for a
	// format it could not resolve; %v would print them as "<nil>/<nil>".
	quality := fileFormat
	if bitDepth != nil && samplingRate != nil {
		quality = fmt.Sprintf("%s %v/%v", fileFormat, bitDepth, samplingRate)
	}
	d.announceAlbum(title, artist, quality, len(rawItems))

	// Build folder name. Individual values are sanitised inside
	// expandPlaceholders, so literal "/" written in FolderFormat survives as a
	// subfolder separator (translated to the OS separator by filepath.FromSlash).
	folderFmt := cleanFormatStr(d.termOut(), d.Opts.FolderFormat, fileFormat)
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
	if tracklist == nil {
		return fmt.Errorf("no tracks in album %s", albumID)
	}

	isMultiDisc := detectMultiDisc(rawItems)
	trackFmt := cleanFormatStr(d.termOut(), d.Opts.TrackFormat, fileFormat)
	// Only for the pre-download skip probe: the delivered format is not known
	// until the file URL comes back, and downloadAndTag reads it from there. A
	// wrong guess here costs one extra track/getFileUrl, never a bad file.
	isMP3 := d.Opts.Quality == 5

	p := d.newProgress(ctx)
	restore := d.withBars(p)
	jobs := d.collectTrackJobs(p, rawItems, albumDir, isMultiDisc, meta, trackFmt, isMP3)
	d.runTrackJobs(ctx, jobs, meta, trackFmt, asked)
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
	d.downloadCover(ctx, nestedStr(meta, "image", "large"), albumDir)
	if goodies, ok := meta["goodies"].([]interface{}); ok && len(goodies) > 0 {
		if g, ok := goodies[0].(map[string]interface{}); ok {
			if pdfURL, _ := g["url"].(string); pdfURL != "" {
				d.downloadExtra(ctx, pdfURL, filepath.Join(albumDir, bookletFile))
			}
		}
	}
}

// downloadCover saves the image at imgURL as dir's cover, in its original size
// with OGCover. It does nothing with NoCover or without an image.
func (d *Downloader) downloadCover(ctx context.Context, imgURL, dir string) {
	if d.Opts.NoCover || imgURL == "" {
		return
	}
	if d.Opts.OGCover {
		imgURL = strings.Replace(imgURL, "_600.", "_org.", 1)
	}
	d.downloadExtra(ctx, imgURL, filepath.Join(dir, coverFile))
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

// collectTrackJobs is Phase 1 of an album download, and touches no network:
// it works out each track's path, drops the ones already downloaded, and
// registers a progress bar on p for the rest, in album order.
//
// The skip check requires BOTH a DB entry and the file present on disk — this
// way, deleting an album from disk causes it to be re-downloaded on the next
// run instead of silently skipped (only the cover would end up recreated).
func (d *Downloader) collectTrackJobs(p *mpb.Progress, rawItems []interface{}, albumDir string, isMultiDisc bool, albumMeta map[string]interface{}, trackFmt string, isMP3 bool) []trackJob {
	var jobs []trackJob
	for idx, t := range rawItems {
		track, ok := t.(map[string]interface{})
		if !ok {
			continue
		}
		trackID := idStr(track["id"])

		// Compute the disc-aware directory before the skip check so the
		// existence probe looks at the exact path downloadAndTag will write.
		// resolveLinks creates it, once the track is known to download.
		trackDir := albumDir
		if isMultiDisc {
			// Checked like detectMultiDisc does: an unchecked assertion
			// crashed the program on a track missing media_number.
			mn, _ := track["media_number"].(float64)
			trackDir = filepath.Join(albumDir, fmt.Sprintf("Disc %d", max(1, int(mn))))
		}

		if finalPath, err := finalTrackPath(trackDir, track, albumMeta, trackFmt, isMP3); err == nil {
			if d.alreadyHave(trackID, finalPath) {
				continue
			}
		}

		trackNum := int(nestedFloat(track, "track_number"))
		bar := d.newBar(p, idx, trackNum, getTitle(track), trackID)
		jobs = append(jobs, trackJob{idx, track, trackDir, trackID, bar})
	}
	return jobs
}

// runTrackJobs is Phase 2 of an album download, a two-stage pipeline:
// resolveLinks asks for each track's link in order, and d.Opts.Workers
// goroutines download, tag and record what it hands them.
//
// Asking for every link first kept the first download waiting on all of them
// — 14 calls of ~180 ms on a 12-track album, 39 % of its time — and the links
// expire, so the last ones aged the most. Now the resolver runs at most
// cap(ready) jobs ahead of the workers.
//
// Cancellation stops the resolver and keeps the workers from starting
// anything new; in-flight downloads observe ctx via downloadAndTag.
func (d *Downloader) runTrackJobs(ctx context.Context, jobs []trackJob, meta map[string]interface{}, trackFmt string, asked urlAnswers) {
	ready := make(chan readyJob, d.Opts.Workers)
	go d.resolveLinks(ctx, jobs, asked, ready)

	var wg sync.WaitGroup
	for range d.Opts.Workers {
		wg.Go(func() {
			for j := range ready {
				d.downloadJob(ctx, j, meta, trackFmt)
			}
		})
	}
	wg.Wait()
}

// resolveLinks is the pipeline's first stage. It hands on each job with its
// link and its directory, and reports the ones that get neither. It closes
// ready when it is done or ctx is cancelled.
func (d *Downloader) resolveLinks(ctx context.Context, jobs []trackJob, asked urlAnswers, ready chan<- readyJob) {
	defer close(ready)
	for _, j := range jobs {
		info, err := asked.fileURL(ctx, d, j.trackID)
		if ctx.Err() != nil {
			return // stopping is not a failure to report
		}
		if err == nil {
			if derr := os.MkdirAll(j.trackDir, 0755); derr != nil {
				err = fmt.Errorf("cannot create directory %q: %w", j.trackDir, derr)
			}
		}
		if err != nil {
			d.skipTrack(j, err)
			continue
		}
		select {
		case ready <- readyJob{j, info}:
		case <-ctx.Done():
			return
		}
	}
}

// downloadJob is the pipeline's second stage, run by each worker.
func (d *Downloader) downloadJob(ctx context.Context, j readyJob, meta map[string]interface{}, trackFmt string) {
	if ctx.Err() != nil {
		return
	}
	if err := d.downloadAndTag(ctx, j.trackDir, j.idx, j.trackURL, j.track, meta, false, trackFmt, j.bar); err != nil {
		d.failTrack(j.bar, j.trackID, err)
		fmt.Fprintf(d.termOut(), "\033[31mTrack %s failed: %v. Skipping...\033[0m\n", j.trackID, err)
		return
	}
	d.recordDownloaded(j.trackID)
}

// skipTrack reports a track that failed before its download started. Under the
// TUI its row says why: otherwise it would sit there pending, with the batch
// counter stuck below its total. An mpb bar is dropped, so the terminal shows
// only the message, as it did when the link was asked for before the bars.
func (d *Downloader) skipTrack(j trackJob, err error) {
	fmt.Fprintf(d.termOut(), "\033[31m%s (track %s): %v. Skipping...\033[0m\n", getTitle(j.track), j.trackID, err)
	if d.tui != nil {
		d.failTrack(j.bar, j.trackID, err)
		return
	}
	j.bar.Abort(true)
}

// resolveFormat reports the format the album will actually be delivered in, by
// asking the API for a real file URL.
//
// It walks the tracklist instead of trusting items[0], and honours the
// configured quality fallback, because "Unknown" is not a harmless answer: it
// makes cleanFormatStr discard the user's whole folder template (issue #22).
// One track that cannot be resolved — unavailable in the region, a sample, or
// simply not offered at the requested quality while the rest are — used to
// rename the entire album.
//
// Every answer it got comes back in asked, so the tracks it looked at are not
// asked for again.
func (d *Downloader) resolveFormat(ctx context.Context, albumMeta map[string]interface{}) (fileFormat string, bitDepth, samplingRate interface{}, asked urlAnswers) {
	if d.Opts.Quality == 5 {
		return "MP3", nil, nil, nil
	}
	tracks, _ := albumMeta["tracks"].(map[string]interface{})
	if tracks == nil {
		return "Unknown", nil, nil, nil
	}
	items, _ := tracks["items"].([]interface{})

	asked = urlAnswers{}
	for _, it := range items {
		track, _ := it.(map[string]interface{})
		if track == nil {
			continue
		}
		trackID := idStr(track["id"])
		// The same lookup collectTrackJobs does: a preview or an unplayable
		// response says nothing about the format the album is served in.
		info, err := d.fileURL(ctx, trackID)
		asked[trackID] = urlAnswer{info, err}
		if err != nil {
			continue
		}

		if restrictions, ok := info["restrictions"].([]interface{}); ok {
			for _, r := range restrictions {
				rm, _ := r.(map[string]interface{})
				if code, _ := rm["code"].(string); code == qlDowngrade {
					fmt.Fprintf(d.termOut(), "\033[90mQuality downgraded for this release\033[0m\n")
				}
			}
		}
		if deliveredIsMP3(info, d.Opts.Quality) {
			return "MP3", nil, nil, asked
		}
		return "FLAC", info["bit_depth"], info["sampling_rate"], asked
	}
	return "Unknown", nil, nil, asked
}

// urlAnswer is the outcome of Downloader.fileURL for one track.
type urlAnswer struct {
	info map[string]interface{}
	err  error
}

// urlAnswers holds the fileURL answers an album already has, by track id.
type urlAnswers map[string]urlAnswer

// fileURL returns the answer already held for trackID, and asks d for it
// otherwise. Each answer is handed out once: a link kept for later only
// grows older.
func (a urlAnswers) fileURL(ctx context.Context, d *Downloader, trackID string) (map[string]interface{}, error) {
	if ans, ok := a[trackID]; ok {
		delete(a, trackID)
		return ans.info, ans.err
	}
	return d.fileURL(ctx, trackID)
}
