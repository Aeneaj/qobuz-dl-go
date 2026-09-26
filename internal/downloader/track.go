package downloader

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Aeneaj/qobuz-dl-go/internal/ui"
)

func (d *Downloader) downloadTrackByID(ctx context.Context, trackID, baseDir string) error {
	// The skip check is deferred until trackDir + trackFmt are known so it
	// can look at the exact output path; see the alreadyHave call below.
	trackURL, err := d.fileURL(ctx, trackID)
	if err != nil {
		return fmt.Errorf("get track URL: %w", err)
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
	isMP3 := deliveredIsMP3(trackURL, d.Opts.Quality)
	fileFormat := "FLAC"
	if isMP3 {
		fileFormat = "MP3"
	}

	albumTitle := nestedStr(meta, "album", "title")
	albumArtist := nestedStr(meta, "album", "artist", "name")
	year := ""
	if rd := nestedStr(meta, "album", "release_date_original"); len(rd) >= 4 {
		year = rd[:4]
	}

	folderFmt := cleanFormatStr(d.termOut(), d.Opts.FolderFormat, fileFormat)
	folderName := expandPlaceholders(folderFmt, map[string]string{
		"{artist}":        albumArtist,
		"{album}":         albumTitle,
		"{year}":          year,
		"{bit_depth}":     fmt.Sprintf("%v", int(bitDepth)),
		"{sampling_rate}": fmt.Sprintf("%v", samplingRate),
		"{format}":        fileFormat,
	})
	trackDir, err := safeJoin(baseDir, filepath.FromSlash(folderName))
	if err != nil {
		return fmt.Errorf("resolve track directory: %w", err)
	}
	if err := os.MkdirAll(trackDir, 0755); err != nil {
		return fmt.Errorf("create track directory %q: %w", trackDir, err)
	}

	trackFmt := cleanFormatStr(d.termOut(), d.Opts.TrackFormat, fileFormat)

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

	if err := d.downloadAndTag(ctx, trackDir, 1, trackURL, meta, meta, true, trackFmt, bar); err != nil {
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

	// The length limit is per path component, so it applies to the file name
	// alone — any subfolder segments the template produced are left as they
	// are. Trimming the joined path instead (as this did until 2026-09-20)
	// mangled every file name once dir grew past the cap.
	prefix, name := "", formatted
	if i := strings.LastIndex(formatted, "/"); i >= 0 {
		prefix, name = formatted[:i+1], formatted[i+1:]
	}
	name = limitNameBytes(name, ext, idStr(trackMeta["id"]))

	finalFile, err := safeJoin(dir, filepath.FromSlash(prefix+name))
	if err != nil {
		return "", fmt.Errorf("resolve track path: %w", err)
	}
	return finalFile, nil
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
	trackFmt string,
	bar ProgressBar,
) error {
	tmpFile := filepath.Join(dir, fmt.Sprintf(".%02d.tmp", idx))
	// Each fallback asks strictly below the last quality asked for, so the
	// loop ends whatever format_id the responses claim.
	ceiling := d.Opts.Quality
	if fid, _ := trackURLDict["format_id"].(float64); fid != 0 {
		ceiling = min(ceiling, int(fid))
	}
	for {
		isMP3, finalFile, err := d.trackTarget(dir, trackURLDict, trackMeta, albumMeta, trackFmt)
		if err != nil {
			return err
		}
		if _, err := os.Stat(finalFile); err == nil {
			if bar != nil {
				bar.Abort(true) // hide already-downloaded bars
			}
			return nil
		}

		fileURL, _ := trackURLDict["url"].(string)
		err = d.downloadWithProgress(ctx, fileURL, tmpFile, bar)
		if err == nil {
			d.tagAndRename(tmpFile, dir, finalFile, trackMeta, albumMeta, isTrack, isMP3)
			return nil
		}
		os.Remove(tmpFile)
		err = fmt.Errorf("download: %w", err)

		// getFileUrl can answer fine for a file the CDN cannot serve: album
		// 0060254736219, track 11 at 24 bit, sends 1 byte of 54 MB and hangs
		// up every time, while its 16-bit file downloads. So a quality whose
		// bytes never arrive is treated like one getFileUrl turns down.
		if ctx.Err() != nil || !d.Opts.QualityFallback {
			return err
		}
		lower, q, ferr := d.fileURLBelow(ctx, idStr(trackMeta["id"]), ceiling)
		if ferr != nil {
			return fmt.Errorf("%w, and no lower quality is available", err)
		}
		ceiling = q
		if bar != nil {
			bar.SetCurrent(0)
		}
		trackURLDict = lower
	}
}

// trackTarget reports where a track goes and whether it is MP3. It reads the
// format off the response, not Options.Quality: a fallback to 5 delivers MP3
// bytes for a lossless request, and the extension and the tagger have to
// follow the bytes.
func (d *Downloader) trackTarget(dir string, trackURLDict, trackMeta, albumMeta map[string]interface{}, trackFmt string) (bool, string, error) {
	isMP3 := deliveredIsMP3(trackURLDict, d.Opts.Quality)
	finalFile, err := finalTrackPath(dir, trackMeta, albumMeta, trackFmt, isMP3)
	if err != nil {
		return false, "", err
	}

	// Support subfolder templates in track_format (e.g. "{albumartist}/{album}/...").
	// safeJoin (inside finalTrackPath) already guaranteed the parent is inside dir.
	if parent := filepath.Dir(finalFile); parent != dir {
		if err := os.MkdirAll(parent, 0755); err != nil {
			return false, "", fmt.Errorf("create track parent directory %q: %w", parent, err)
		}
	}

	return isMP3, finalFile, nil
}

// tagAndRename tags the downloaded tmpFile and moves it to finalFile. A
// tagging failure is reported, and the untagged audio still kept.
func (d *Downloader) tagAndRename(tmpFile, dir, finalFile string, trackMeta, albumMeta map[string]interface{}, isTrack, isMP3 bool) {
	if isMP3 {
		if err := tagMP3(tmpFile, dir, finalFile, trackMeta, albumMeta, isTrack, d.Opts.EmbedArt); err != nil {
			fmt.Fprintf(d.termOut(), "\033[31mWarning: could not tag %s: %v\033[0m\n", filepath.Base(finalFile), err)
			// Still rename even if tagging failed
			os.Rename(tmpFile, finalFile)
		}
	} else {
		if err := tagFLAC(d.termOut(), tmpFile, dir, finalFile, trackMeta, albumMeta, isTrack, d.Opts.EmbedArt); err != nil {
			fmt.Fprintf(d.termOut(), "\033[31mWarning: could not tag %s: %v\033[0m\n", filepath.Base(finalFile), err)
			os.Rename(tmpFile, finalFile)
		}
	}
}

// fileURL asks for the track at the requested quality and, when that cannot
// be served in full, at each lower one — never higher, which would overrule
// the cap the user set. Qobuz seldom says "not at this quality" with an HTTP
// error: it answers 200 with a 30-second preview, a zero sampling rate or no
// URL. Those used to skip the track silently without trying the quality below,
// so a track that failed at 7 downloaded fine at 6 (issue #12).
func (d *Downloader) fileURL(ctx context.Context, trackID string) (map[string]interface{}, error) {
	info, err := d.Client.GetTrackURL(ctx, trackID, d.Opts.Quality, "")
	if err == nil {
		err = unservable(info)
	}
	if err == nil {
		return info, nil
	}
	err = fmt.Errorf("at %s: %w", Qualities[d.Opts.Quality], err)
	if !d.Opts.QualityFallback {
		return nil, err
	}
	info, _, ferr := d.fileURLBelow(ctx, trackID, d.Opts.Quality)
	if ferr != nil {
		return nil, fmt.Errorf("%w, and no lower quality is available", err)
	}
	return info, nil
}

// fileURLBelow returns the track at the best quality under ceiling that
// Qobuz serves in full, and the quality it asked for; it says so on the
// terminal.
func (d *Downloader) fileURLBelow(ctx context.Context, trackID string, ceiling int) (map[string]interface{}, int, error) {
	for _, q := range []int{27, 7, 6, 5} {
		if q >= ceiling {
			continue
		}
		info, qerr := d.Client.GetTrackURL(ctx, trackID, q, "")
		if qerr == nil {
			qerr = unservable(info)
		}
		if qerr == nil {
			fmt.Fprintf(d.termOut(), "\033[33mQuality fallback to %s for track %s\033[0m\n", Qualities[q], trackID)
			return info, q, nil
		}
	}
	return nil, 0, errors.New("no lower quality is available")
}

// unservable says why a successful track/getFileUrl response still has no
// full track to download, or returns nil.
func unservable(info map[string]interface{}) error {
	if _, isSample := info["sample"]; isSample {
		return errors.New("only a 30-second preview is offered")
	}
	if sr, _ := info["sampling_rate"].(float64); sr == 0 {
		return errors.New("no playable format is offered")
	}
	if u, _ := info["url"].(string); u == "" {
		return errors.New("no download URL is offered")
	}
	return nil
}
