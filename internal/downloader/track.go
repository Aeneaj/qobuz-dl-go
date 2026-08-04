package downloader

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Aeneaj/qobuz-dl-go/internal/ui"
)

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
