// Package downloader orchestrates metadata lookup, file downloads, and tagging.
// Translated from downloader.py + core.py + utils.py.
package downloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Aeneaj/qobuz-dl-go/internal/api"
	"github.com/Aeneaj/qobuz-dl-go/internal/ui"
)

// ProgressBar abstracts mpb.Bar and ui.TrackHandle behind a common interface.
// *mpb.Bar satisfies this interface natively; ui.TrackHandle implements it explicitly.
// ProxyReader matches mpb.Bar's signature (io.Reader → io.ReadCloser).
type ProgressBar interface {
	SetTotal(total int64, triggerComplete bool)
	IncrBy(n int)
	ProxyReader(r io.Reader) io.ReadCloser
	Abort(drop bool)
}

// noopBar is used when uiProg is nil and no mpb context exists (e.g. in tests).
type noopBar struct{}

func (noopBar) SetTotal(int64, bool)                  {}
func (noopBar) IncrBy(int)                            {}
func (noopBar) ProxyReader(r io.Reader) io.ReadCloser { return io.NopCloser(r) }
func (noopBar) Abort(bool)                            {}

const (
	qlDowngrade = "FormatRestrictedByFormatAvailability"
	coverFile   = "cover.jpg"
	bookletFile = "booklet.pdf"
)

var qualities = map[int]string{
	5:  "5 - MP3",
	6:  "6 - 16 bit, 44.1kHz",
	7:  "7 - 24 bit, <96kHz",
	27: "27 - 24 bit, >96kHz",
}

// Options configures the downloader.
type Options struct {
	Directory       string
	Quality         int
	EmbedArt        bool
	IgnoreSingles   bool
	NoM3U           bool
	QualityFallback bool
	OGCover         bool
	NoCover         bool
	FolderFormat    string
	TrackFormat     string
	SmartDiscog     bool
	NoDB            bool
	DBPath          string
	Workers         int // concurrent track downloads per album (0 = default 3)
}

// Downloader handles URL processing and downloads.
type Downloader struct {
	Client     *api.Client
	Opts       Options
	db         *downloadDB
	httpClient *http.Client
	ctx        context.Context
	uiProg     *tea.Program // nil → use mpb progress bars
}

// SetUI wires up the bubbletea program for the TUI download display.
// Must be called before any download methods if TUI mode is desired.
func (d *Downloader) SetUI(p *tea.Program) { d.uiProg = p }

// quiet returns true when the TUI is active; callers suppress fmt.Printf in that case.
func (d *Downloader) quiet() bool { return d.uiProg != nil }

// New creates a Downloader. ctx is used to cancel in-flight downloads on
// Ctrl+C; pass context.Background() if cancellation is not needed.
func New(client *api.Client, opts Options, ctx context.Context) *Downloader {
	if opts.FolderFormat == "" {
		opts.FolderFormat = "{artist} - {album} ({year}) [{bit_depth}B-{sampling_rate}kHz]"
	}
	if opts.TrackFormat == "" {
		opts.TrackFormat = "{tracknumber}. {tracktitle}"
	}
	if opts.Workers <= 0 {
		opts.Workers = 3
	}
	os.MkdirAll(opts.Directory, 0755)

	dl := &Downloader{
		Client:     client,
		Opts:       opts,
		httpClient: &http.Client{Timeout: 10 * time.Minute},
		ctx:        ctx,
	}
	if !opts.NoDB && opts.DBPath != "" {
		db, err := openDB(opts.DBPath)
		if err != nil {
			fmt.Printf("\033[33mWarning: could not open downloads DB: %v\033[0m\n", err)
		} else {
			dl.db = db
		}
	}
	return dl
}

// HandleURL dispatches a URL to the appropriate download flow.
// Supports Qobuz URLs and Last.fm user playlist URLs.
func (d *Downloader) HandleURL(rawURL string) error {
	// Last.fm user playlists (loved tracks, recent tracks)
	if strings.Contains(rawURL, "last.fm") {
		username, listType, err := parseLastFMURL(rawURL)
		if err != nil {
			return err
		}
		return d.downloadLastFMPlaylist(username, listType)
	}

	urlType, itemID, err := parseQobuzURL(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %w", rawURL, err)
	}

	switch urlType {
	case "album":
		return d.downloadAlbum(itemID, d.Opts.Directory)
	case "track":
		return d.downloadTrackByID(itemID, d.Opts.Directory)
	case "artist":
		pages, err := d.Client.GetArtistMeta(itemID)
		if err != nil {
			return err
		}
		return d.downloadArtist(pages)
	case "playlist":
		pages, err := d.Client.GetPlaylistMeta(itemID)
		if err != nil {
			return err
		}
		return d.downloadPlaylist(pages)
	case "label":
		pages, err := d.Client.GetLabelMeta(itemID)
		if err != nil {
			return err
		}
		return d.downloadLabelOrArtist(pages, "albums", "label")
	default:
		return fmt.Errorf("unsupported URL type: %s", urlType)
	}
}

// ---- collection downloads ----

func (d *Downloader) downloadArtist(pages []map[string]interface{}) error {
	if len(pages) == 0 {
		return nil
	}
	name, _ := pages[0]["name"].(string)

	var items []map[string]interface{}
	for _, page := range pages {
		section, _ := page["albums"].(map[string]interface{})
		if section == nil {
			continue
		}
		raw, _ := section["items"].([]interface{})
		for _, r := range raw {
			if m, ok := r.(map[string]interface{}); ok {
				items = append(items, m)
			}
		}
	}

	if d.Opts.SmartDiscog {
		items = smartDiscogFilter(name, items)
	}

	dir := filepath.Join(d.Opts.Directory, sanitize(name))
	os.MkdirAll(dir, 0755)
	fmt.Printf("\033[33mDownloading discography: %s (%d albums)\033[0m\n", name, len(items))

	for _, item := range items {
		id := idStr(item["id"])
		if err := d.downloadAlbum(id, dir); err != nil {
			fmt.Printf("\033[31mError on album %s: %v. Skipping...\033[0m\n", id, err)
		}
	}
	return nil
}

func (d *Downloader) downloadPlaylist(pages []map[string]interface{}) error {
	if len(pages) == 0 {
		return nil
	}
	name, _ := pages[0]["name"].(string)
	dir := filepath.Join(d.Opts.Directory, sanitize(name))
	os.MkdirAll(dir, 0755)

	var items []map[string]interface{}
	for _, page := range pages {
		section, _ := page["tracks"].(map[string]interface{})
		if section == nil {
			continue
		}
		raw, _ := section["items"].([]interface{})
		for _, r := range raw {
			if m, ok := r.(map[string]interface{}); ok {
				items = append(items, m)
			}
		}
	}

	fmt.Printf("\033[33mDownloading playlist: %s (%d tracks)\033[0m\n", name, len(items))
	for _, item := range items {
		id := idStr(item["id"])
		if err := d.downloadTrackByID(id, dir); err != nil {
			fmt.Printf("\033[31mError on track %s: %v. Skipping...\033[0m\n", id, err)
		}
	}

	if !d.Opts.NoM3U {
		makeM3U(dir)
	}
	return nil
}

func (d *Downloader) downloadLabelOrArtist(pages []map[string]interface{}, itemKey, collectionType string) error {
	if len(pages) == 0 {
		return nil
	}
	name, _ := pages[0]["name"].(string)
	dir := filepath.Join(d.Opts.Directory, sanitize(name))
	os.MkdirAll(dir, 0755)

	var items []map[string]interface{}
	for _, page := range pages {
		section, _ := page[itemKey].(map[string]interface{})
		if section == nil {
			continue
		}
		raw, _ := section["items"].([]interface{})
		for _, r := range raw {
			if m, ok := r.(map[string]interface{}); ok {
				items = append(items, m)
			}
		}
	}

	fmt.Printf("\033[33mDownloading %s: %s (%d albums)\033[0m\n", collectionType, name, len(items))
	for _, item := range items {
		id := idStr(item["id"])
		if err := d.downloadAlbum(id, dir); err != nil {
			fmt.Printf("\033[31mError on album %s: %v. Skipping...\033[0m\n", id, err)
		}
	}
	return nil
}

// ---- album download ----

func (d *Downloader) downloadAlbum(albumID, baseDir string) error {
	meta, err := d.Client.GetAlbumMeta(albumID)
	if err != nil {
		return fmt.Errorf("album metadata %s: %w", albumID, err)
	}

	// Streamable check
	if streamable, ok := meta["streamable"].(bool); ok && !streamable {
		fmt.Printf("\033[90mAlbum %s is not streamable, skipping\033[0m\n", albumID)
		return nil
	}

	// albums_only filter
	if d.Opts.IgnoreSingles {
		releaseType, _ := meta["release_type"].(string)
		artistName := nestedStr(meta, "artist", "name")
		if releaseType != "album" || artistName == "Various Artists" {
			title, _ := meta["title"].(string)
			fmt.Printf("\033[90mIgnoring Single/EP/VA: %s\033[0m\n", title)
			return nil
		}
	}

	// Resolve format info from first track
	fileFormat, bitDepth, samplingRate := d.resolveFormat(meta)
	title := getTitle(meta)
	artist := nestedStr(meta, "artist", "name")
	year := releaseYear(meta)

	trackCount := 0
	if items, _ := meta["tracks"].(map[string]interface{}); items != nil {
		if raw, _ := items["items"].([]interface{}); raw != nil {
			trackCount = len(raw)
		}
	}
	if d.uiProg != nil {
		d.uiProg.Send(ui.MsgAlbum{
			Title:  title,
			Artist: artist,
			Format: fmt.Sprintf("%s %v/%v", fileFormat, bitDepth, samplingRate),
			Tracks: trackCount,
		})
	} else {
		fmt.Printf("\n\033[1m♫  %s\033[0m  ·  \033[33m%s %v/%v\033[0m  ·  %d tracks\n\n",
			title, fileFormat, bitDepth, samplingRate, trackCount)
	}

	// Build folder name
	folderFmt := cleanFormatStr(d.Opts.FolderFormat, fileFormat)
	folderName := expandPlaceholders(folderFmt, map[string]string{
		"{artist}":        sanitize(artist),
		"{album}":         sanitize(title),
		"{year}":          year,
		"{bit_depth}":     fmt.Sprintf("%v", bitDepth),
		"{sampling_rate}": fmt.Sprintf("%v", samplingRate),
		"{format}":        fileFormat,
	})
	albumDir := filepath.Join(baseDir, sanitize(folderName))
	os.MkdirAll(albumDir, 0755)

	// Cover art
	if !d.Opts.NoCover {
		if imgURL := nestedStr(meta, "image", "large"); imgURL != "" {
			if d.Opts.OGCover {
				imgURL = strings.Replace(imgURL, "_600.", "_org.", 1)
			}
			d.downloadExtra(imgURL, filepath.Join(albumDir, coverFile))
		}
	}

	// Booklet PDF
	if goodies, ok := meta["goodies"].([]interface{}); ok && len(goodies) > 0 {
		if g, ok := goodies[0].(map[string]interface{}); ok {
			if pdfURL, _ := g["url"].(string); pdfURL != "" {
				d.downloadExtra(pdfURL, filepath.Join(albumDir, bookletFile))
			}
		}
	}

	// Tracks
	tracklist, _ := meta["tracks"].(map[string]interface{})
	if tracklist == nil {
		return fmt.Errorf("no tracks in album %s", albumID)
	}
	rawItems, _ := tracklist["items"].([]interface{})

	// Detect multi-disc
	mediaNumbers := map[float64]bool{}
	for _, t := range rawItems {
		if track, ok := t.(map[string]interface{}); ok {
			if mn, ok := track["media_number"].(float64); ok {
				mediaNumbers[mn] = true
			}
		}
	}
	isMultiDisc := len(mediaNumbers) > 1

	trackFmt := cleanFormatStr(d.Opts.TrackFormat, fileFormat)
	isMP3 := d.Opts.Quality == 5

	// Phase 1: resolve URLs and collect jobs, skipping ineligible tracks.
	type trackJob struct {
		idx      int
		trackURL map[string]interface{}
		track    map[string]interface{}
		trackDir string
		trackID  string
		bar      ProgressBar
	}

	var mpbProg *mpb.Progress
	if d.uiProg == nil {
		mpbProg = mpb.New(mpb.WithRefreshRate(150 * time.Millisecond))
	}

	var jobs []trackJob

	for idx, t := range rawItems {
		track, ok := t.(map[string]interface{})
		if !ok {
			continue
		}
		trackID := idStr(track["id"])

		if d.db != nil && d.db.has(trackID) {
			continue
		}

		trackURL, err := d.Client.GetTrackURL(trackID, d.Opts.Quality, "")
		if err != nil {
			if d.Opts.QualityFallback {
				trackURL, err = d.fallbackQuality(trackID)
			}
			if err != nil {
				if !d.quiet() {
					fmt.Printf("\033[31mTrack %s: cannot get URL: %v. Skipping...\033[0m\n", trackID, err)
				}
				continue
			}
		}
		if _, isSample := trackURL["sample"]; isSample {
			continue
		}
		if sr, _ := trackURL["sampling_rate"].(float64); sr == 0 {
			continue
		}

		trackDir := albumDir
		if isMultiDisc {
			mn := int(track["media_number"].(float64))
			trackDir = filepath.Join(albumDir, fmt.Sprintf("Disc %d", mn))
			os.MkdirAll(trackDir, 0755)
		}

		trackNum := 0
		if tn, ok := track["track_number"].(float64); ok {
			trackNum = int(tn)
		}
		trackTitle := getTitle(track)

		var bar ProgressBar
		if d.uiProg != nil {
			handle := ui.NewTrackHandle(trackID, d.uiProg)
			d.uiProg.Send(ui.MsgRegisterTrack{ID: trackID, Num: trackNum, Name: trackTitle, Counter: handle.Counter()})
			bar = handle
		} else {
			label := barLabel(trackNum, trackTitle)
			bar = mpbProg.New(0,
				mpb.BarStyle().Lbound("╢").Filler("█").Tip("█").Padding("░").Rbound("╟"),
				mpb.BarPriority(idx),
				mpb.PrependDecorators(decor.Name(label)),
				mpb.AppendDecorators(
					decor.Counters(decor.SizeB1024(0), " % .1f / % .1f "),
					decor.EwmaSpeed(decor.SizeB1024(0), "% .1f MiB/s", 30),
					decor.OnComplete(decor.Name(""), " \033[32m✓\033[0m"),
				),
			)
		}

		jobs = append(jobs, trackJob{idx, trackURL, track, trackDir, trackID, bar})
	}

	// Phase 2: download concurrently, each job with its own progress bar.
	sem := make(chan struct{}, d.Opts.Workers)
	var wg sync.WaitGroup

	for _, job := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(j trackJob) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := d.downloadAndTag(j.trackDir, j.idx, j.trackURL, j.track, meta, false, isMP3, trackFmt, j.bar); err != nil {
				if d.uiProg != nil {
					d.uiProg.Send(ui.MsgFailed{ID: j.trackID, Err: err})
				} else {
					j.bar.Abort(false)
					fmt.Printf("\033[31mTrack %s failed: %v. Skipping...\033[0m\n", j.trackID, err)
				}
			} else if d.db != nil {
				if err := d.db.add(j.trackID); err != nil {
					if !d.quiet() {
						fmt.Printf("\033[33mWarning: could not record track in DB: %v\033[0m\n", err)
					}
				}
			}
		}(job)
	}

	wg.Wait()
	if mpbProg != nil {
		mpbProg.Wait()
	}

	if !d.quiet() {
		fmt.Printf("\033[32m✓  Completed: %s\033[0m\n\n", title)
	}
	return nil
}

// ---- track download ----

func (d *Downloader) downloadTrackByID(trackID, baseDir string) error {
	// DB check
	if d.db != nil && d.db.has(trackID) {
		fmt.Printf("\033[90mTrack %s already in DB, skipping\033[0m\n", trackID)
		return nil
	}

	trackURL, err := d.Client.GetTrackURL(trackID, d.Opts.Quality, "")
	if err != nil {
		if d.Opts.QualityFallback {
			trackURL, err = d.fallbackQuality(trackID)
		}
		if err != nil {
			return fmt.Errorf("get track URL: %w", err)
		}
	}

	if _, isSample := trackURL["sample"]; isSample {
		fmt.Printf("\033[90mDemo track, skipping\033[0m\n")
		return nil
	}

	meta, err := d.Client.GetTrackMeta(trackID)
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
		"{artist}":        sanitize(albumArtist),
		"{album}":         sanitize(albumTitle),
		"{year}":          year,
		"{bit_depth}":     fmt.Sprintf("%v", int(bitDepth)),
		"{sampling_rate}": fmt.Sprintf("%v", samplingRate),
	})
	trackDir := filepath.Join(baseDir, sanitize(folderName))
	os.MkdirAll(trackDir, 0755)

	if !d.Opts.NoCover {
		if imgURL := nestedStr(meta, "album", "image", "large"); imgURL != "" {
			if d.Opts.OGCover {
				imgURL = strings.Replace(imgURL, "_600.", "_org.", 1)
			}
			d.downloadExtra(imgURL, filepath.Join(trackDir, coverFile))
		}
	}

	if !d.quiet() {
		fmt.Printf("\n\033[1m♫  %s\033[0m  ·  \033[33m%s — %s\033[0m\n\n", title, performer, fileFormat)
	}

	trackNum := 0
	if tn, ok := meta["track_number"].(float64); ok {
		trackNum = int(tn)
	}

	var bar ProgressBar
	var mpbProg *mpb.Progress
	if d.uiProg != nil {
		d.uiProg.Send(ui.MsgAlbum{
			Title:  title,
			Artist: performer,
			Format: fileFormat,
			Tracks: 1,
		})
		handle := ui.NewTrackHandle(trackID, d.uiProg)
		d.uiProg.Send(ui.MsgRegisterTrack{ID: trackID, Num: trackNum, Name: title, Counter: handle.Counter()})
		bar = handle
	} else {
		mpbProg = mpb.New(mpb.WithRefreshRate(150 * time.Millisecond))
		bar = mpbProg.New(0,
			mpb.BarStyle().Lbound("╢").Filler("█").Tip("█").Padding("░").Rbound("╟"),
			mpb.PrependDecorators(decor.Name(barLabel(trackNum, title))),
			mpb.AppendDecorators(
				decor.Counters(decor.SizeB1024(0), " % .1f / % .1f "),
				decor.EwmaSpeed(decor.SizeB1024(0), "% .1f MiB/s", 30),
				decor.OnComplete(decor.Name(""), " \033[32m✓\033[0m"),
			),
		)
	}

	isMP3 := d.Opts.Quality == 5
	trackFmt := cleanFormatStr(d.Opts.TrackFormat, fileFormat)
	if err := d.downloadAndTag(trackDir, 1, trackURL, meta, meta, true, isMP3, trackFmt, bar); err != nil {
		if d.uiProg != nil {
			d.uiProg.Send(ui.MsgFailed{ID: trackID, Err: err})
		} else {
			bar.Abort(false)
			if mpbProg != nil {
				mpbProg.Wait()
			}
		}
		return err
	}
	if d.db != nil {
		if err := d.db.add(trackID); err != nil {
			if !d.quiet() {
				fmt.Printf("\033[33mWarning: could not record track in DB: %v\033[0m\n", err)
			}
		}
	}
	if mpbProg != nil {
		mpbProg.Wait()
	}

	if !d.quiet() {
		fmt.Printf("\033[32m✓  Completed: %s\033[0m\n\n", title)
	}
	return nil
}

// ---- core download + tag ----

func (d *Downloader) downloadAndTag(
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
		fmt.Printf("\033[90mTrack not available for download\033[0m\n")
		return nil
	}

	ext := ".flac"
	if isMP3 {
		ext = ".mp3"
	}

	// Build filename from track metadata
	trackTitle := getTitle(trackMeta)
	performer := safeGet(trackMeta, "performer", "name")
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
	finalFile := filepath.Join(dir, sanitize(formatted))
	// Trim to 250 runes to stay within filesystem limits without splitting
	// multi-byte UTF-8 characters (e.g. CJK, Arabic, emoji in track titles).
	if runes := []rune(finalFile); len(runes) > 250 {
		finalFile = string(runes[:250])
	}
	finalFile += ext

	if _, err := os.Stat(finalFile); err == nil {
		bar.Abort(true) // file already downloaded: mark as done in TUI or hide in mpb
		return nil
	}

	// Download to .tmp file first
	tmpFile := filepath.Join(dir, fmt.Sprintf(".%02d.tmp", idx))
	if err := d.downloadWithProgress(fileURL, tmpFile, bar); err != nil {
		os.Remove(tmpFile)
		return fmt.Errorf("download: %w", err)
	}

	// Tag and rename
	if isMP3 {
		if err := tagMP3(tmpFile, dir, finalFile, trackMeta, albumMeta, isTrack, d.Opts.EmbedArt); err != nil {
			fmt.Printf("\033[31mWarning: could not tag %s: %v\033[0m\n", filepath.Base(finalFile), err)
			// Still rename even if tagging failed
			os.Rename(tmpFile, finalFile)
		}
	} else {
		if err := tagFLAC(tmpFile, dir, finalFile, trackMeta, albumMeta, isTrack, d.Opts.EmbedArt); err != nil {
			fmt.Printf("\033[31mWarning: could not tag %s: %v\033[0m\n", filepath.Base(finalFile), err)
			os.Rename(tmpFile, finalFile)
		}
	}

	return nil
}

// ---- quality fallback ----

func (d *Downloader) fallbackQuality(trackID string) (map[string]interface{}, error) {
	fallbacks := []int{27, 7, 6, 5}
	for _, q := range fallbacks {
		if q == d.Opts.Quality {
			continue
		}
		info, err := d.Client.GetTrackURL(trackID, q, "")
		if err == nil {
			fmt.Printf("\033[33mQuality fallback to %s for track %s\033[0m\n", qualities[q], trackID)
			return info, nil
		}
	}
	return nil, fmt.Errorf("no quality available for track %s", trackID)
}

// ---- format helpers ----

func (d *Downloader) resolveFormat(albumMeta map[string]interface{}) (fileFormat string, bitDepth, samplingRate interface{}) {
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
	info, err := d.Client.GetTrackURL(trackID, d.Opts.Quality, "")
	if err != nil {
		return "Unknown", nil, nil
	}

	// Check quality restriction
	if restrictions, ok := info["restrictions"].([]interface{}); ok {
		for _, r := range restrictions {
			rm, _ := r.(map[string]interface{})
			if code, _ := rm["code"].(string); code == qlDowngrade {
				fmt.Printf("\033[90mQuality downgraded for this release\033[0m\n")
			}
		}
	}
	return "FLAC", info["bit_depth"], info["sampling_rate"]
}

// ---- download helpers ----

// downloadWithProgress downloads rawURL to dest, updating bar as bytes arrive.
// It uses the Downloader's shared httpClient and respects the context for
// cancellation (e.g. Ctrl+C).
const maxDownloadRetries = 5

func (d *Downloader) downloadWithProgress(rawURL, dest string, bar ProgressBar) error {
	var (
		totalSize   int64 = -1 // full file size, resolved from Content-Length or Content-Range
		barCredited int64      // bytes already reflected in the bar across all attempts
	)

	for attempt := 0; attempt < maxDownloadRetries; attempt++ {
		if attempt > 0 {
			delay := time.Duration(1<<(attempt-1)) * time.Second // 1s, 2s, 4s, 8s
			select {
			case <-d.ctx.Done():
				return d.ctx.Err()
			case <-time.After(delay):
			}
		}

		// Bytes already saved from a previous attempt.
		var offset int64
		if fi, err := os.Stat(dest); err == nil {
			offset = fi.Size()
		}

		req, err := http.NewRequestWithContext(d.ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return err
		}
		if offset > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		}

		resp, err := d.httpClient.Do(req)
		if err != nil {
			if isContextError(err) {
				return err
			}
			continue // network error before response — retry
		}

		// Server ignored Range and sent full file — discard partial data and restart.
		// Must continue so we make a fresh request with the original (non-closed) body.
		if offset > 0 && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			os.Remove(dest)
			barCredited = 0
			continue
		}

		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
			resp.Body.Close()
			return fmt.Errorf("unexpected HTTP status %d", resp.StatusCode)
		}

		// Resolve total file size once.
		if totalSize <= 0 {
			if resp.StatusCode == http.StatusPartialContent {
				// Content-Range: bytes N-M/TOTAL
				if cr := resp.Header.Get("Content-Range"); cr != "" {
					if idx := strings.LastIndex(cr, "/"); idx >= 0 {
						if t, err2 := strconv.ParseInt(cr[idx+1:], 10, 64); err2 == nil && t > 0 {
							totalSize = t
						}
					}
				}
			}
			if totalSize <= 0 && resp.ContentLength > 0 {
				totalSize = offset + resp.ContentLength
			}
		}

		// Set bar total for display only — do NOT trigger auto-completion here.
		// The bar is explicitly completed after io.Copy returns to avoid mpb
		// closing its operateState channel while ProxyReader is still active.
		if totalSize > 0 {
			bar.SetTotal(totalSize, false)
		}

		// Fast-forward bar for bytes already on disk from prior attempts.
		if offset > barCredited {
			bar.IncrBy(int(offset - barCredited))
			barCredited = offset
		}

		// Open file: create fresh on first write, append on resume.
		var f *os.File
		if offset == 0 {
			f, err = os.Create(dest)
		} else {
			f, err = os.OpenFile(dest, os.O_APPEND|os.O_WRONLY, 0644)
		}
		if err != nil {
			resp.Body.Close()
			return err
		}

		pr := bar.ProxyReader(resp.Body)
		n, copyErr := io.Copy(f, pr)

		// Always close all handles before deciding what to do next.
		pr.Close()
		resp.Body.Close()
		f.Close()

		barCredited += n
		written := offset + n

		if copyErr == nil {
			if totalSize > 0 && written != totalSize {
				return fmt.Errorf("incomplete download: got %d of %d bytes", written, totalSize)
			}
			// Explicitly mark bar complete now that io.Copy has fully returned.
			// Doing this here (not during SetTotal) prevents mpb from closing its
			// internal operateState channel while ProxyReader is still reading.
			completedAt := totalSize
			if completedAt <= 0 {
				completedAt = written
			}
			bar.SetTotal(completedAt, true)
			return nil
		}

		if isContextError(copyErr) {
			return copyErr
		}
		if !isRecoverableErr(copyErr) {
			return copyErr
		}
		// Recoverable (EOF / network drop) — next iteration resumes via Range header.
	}

	return fmt.Errorf("download failed after %d attempts", maxDownloadRetries)
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func isRecoverableErr(err error) bool {
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

// downloadExtra fetches a supplementary file (cover art, booklet PDF).
// Uses the shared httpClient and context; logs errors instead of silently
// ignoring them.
func (d *Downloader) downloadExtra(rawURL, dest string) {
	if _, err := os.Stat(dest); err == nil {
		fmt.Printf("\033[90m%s already downloaded\033[0m\n", filepath.Base(dest))
		return
	}
	fmt.Printf("\033[90mDownloading %s...\033[0m\n", filepath.Base(dest))
	req, err := http.NewRequestWithContext(d.ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		fmt.Printf("\033[31mCould not create request for %s: %v\033[0m\n", filepath.Base(dest), err)
		return
	}
	resp, err := d.httpClient.Do(req)
	if err != nil {
		fmt.Printf("\033[31mCould not download %s: %v\033[0m\n", filepath.Base(dest), err)
		return
	}
	defer resp.Body.Close()
	f, err := os.Create(dest)
	if err != nil {
		fmt.Printf("\033[31mCould not create file %s: %v\033[0m\n", filepath.Base(dest), err)
		return
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		fmt.Printf("\033[31mError writing %s: %v\033[0m\n", filepath.Base(dest), err)
	}
}

// ---- M3U playlist ----

func makeM3U(dir string) {
	plName := filepath.Base(dir) + ".m3u"
	plPath := filepath.Join(dir, plName)

	var sb strings.Builder
	sb.WriteString("#EXTM3U")
	entries := 0

	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error { //nolint:errcheck
		if err != nil || info.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".mp3" && ext != ".flac" {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return nil
		}
		fmt.Fprintf(&sb, "\n\n#EXTINF:-1,%s\n%s",
			strings.TrimSuffix(info.Name(), filepath.Ext(info.Name())), rel)
		entries++
		return nil
	})

	if entries == 0 {
		return
	}
	f, err := os.Create(plPath)
	if err != nil {
		fmt.Printf("\033[31mCould not create M3U: %v\033[0m\n", err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(sb.String()); err != nil {
		fmt.Printf("\033[31mCould not write M3U: %v\033[0m\n", err)
		return
	}
	fmt.Printf("\033[32mM3U playlist saved: %s\033[0m\n", plName)
}

// ---- DownloadURLs (batch entry point) ----

func (d *Downloader) DownloadURLs(urls []string) {
	for _, u := range urls {
		if isLocalFile(u) {
			d.downloadFromFile(u)
		} else {
			if err := d.HandleURL(u); err != nil {
				fmt.Printf("\033[31mError: %v\033[0m\n", err)
			}
		}
	}
	// Clean leftover .tmp files
	cleanTmp(d.Opts.Directory)
}

func (d *Downloader) downloadFromFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Printf("\033[31mCannot read file %s: %v\033[0m\n", path, err)
		return
	}
	var urls []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			urls = append(urls, line)
		}
	}
	fmt.Printf("\033[33mDownloading %d URLs from %s\033[0m\n", len(urls), path)
	d.DownloadURLs(urls)
}

func cleanTmp(dir string) {
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasPrefix(info.Name(), ".") && strings.HasSuffix(info.Name(), ".tmp") {
			os.Remove(path)
		}
		return nil
	})
}

// ---- smart discography filter (from utils.py) ----

var (
	reRemaster = regexp.MustCompile(`(?i)(re)?master(ed)?`)
	reExtra    = regexp.MustCompile(`(?i)(anniversary|deluxe|live|collector|demo|expanded)`)
	reEssence  = regexp.MustCompile(`^([^(]+)`)
)

func smartDiscogFilter(requestedArtist string, items []map[string]interface{}) []map[string]interface{} {
	// Group by normalised title
	grouped := map[string][]map[string]interface{}{}
	order := []string{}
	for _, item := range items {
		title, _ := item["title"].(string)
		key := essenceTitle(title)
		if _, exists := grouped[key]; !exists {
			order = append(order, key)
		}
		grouped[key] = append(grouped[key], item)
	}

	var result []map[string]interface{}
	for _, key := range order {
		albums := grouped[key]

		// Find best bit depth
		bestBD := 0.0
		for _, a := range albums {
			bd, _ := a["maximum_bit_depth"].(float64)
			if bd > bestBD {
				bestBD = bd
			}
		}
		// Find best (or most space-saving) sampling rate at that bit depth
		bestSR := 0.0
		for _, a := range albums {
			bd, _ := a["maximum_bit_depth"].(float64)
			sr, _ := a["maximum_sampling_rate"].(float64)
			if bd == bestBD && sr > bestSR {
				bestSR = sr
			}
		}

		remasterExists := false
		for _, a := range albums {
			if isAlbumType("remaster", a) {
				remasterExists = true
				break
			}
		}

		for _, a := range albums {
			bd, _ := a["maximum_bit_depth"].(float64)
			sr, _ := a["maximum_sampling_rate"].(float64)
			aName := nestedStr(a, "artist", "name")
			if bd == bestBD && sr == bestSR && aName == requestedArtist &&
				!(remasterExists && !isAlbumType("remaster", a)) {
				result = append(result, a)
				break
			}
		}
	}
	return result
}

func essenceTitle(title string) string {
	m := reEssence.FindString(title)
	if m == "" {
		return strings.ToLower(title)
	}
	return strings.ToLower(strings.TrimSpace(m))
}

func isAlbumType(t string, album map[string]interface{}) bool {
	title, _ := album["title"].(string)
	version, _ := album["version"].(string)
	combined := title + " " + version
	switch t {
	case "remaster":
		return reRemaster.MatchString(combined)
	case "extra":
		return reExtra.MatchString(combined)
	}
	return false
}

// ---- format string helpers ----

func getTitle(item map[string]interface{}) string {
	title, _ := item["title"].(string)
	version, _ := item["version"].(string)
	if version != "" && !strings.Contains(strings.ToLower(title), strings.ToLower(version)) {
		title = fmt.Sprintf("%s (%s)", title, version)
	}
	return title
}

func cleanFormatStr(format, fileFormat string) string {
	format = strings.TrimSuffix(format, ".mp3")
	format = strings.TrimSuffix(format, ".flac")
	format = strings.TrimSpace(format)

	if fileFormat == "MP3" || fileFormat == "Unknown" {
		if strings.Contains(format, "{bit_depth}") || strings.Contains(format, "{sampling_rate}") {
			if fileFormat == "MP3" {
				return "{artist} - {album} ({year}) [MP3]"
			}
			return "{artist} - {album}"
		}
	}
	return format
}

func expandPlaceholders(format string, attrs map[string]string) string {
	result := format
	for k, v := range attrs {
		if v == "" || v == "<nil>" || v == "%!v(MISSING)" {
			v = "n_a"
		}
		result = strings.ReplaceAll(result, k, v)
	}
	return result
}

// ---- URL parsing ----

// reQobuzURL matches Qobuz URLs in multiple formats:
//
//	https://www.qobuz.com/us-en/{type}/{name}/{id}
//	https://open.qobuz.com/{type}/{id}
//	https://play.qobuz.com/{type}/{id}
var reQobuzURL = regexp.MustCompile(
	`(?:https?://(?:www|open|play)\.qobuz\.com)?(?:/[a-z]{2}-[a-z]{2})?` +
		`/(album|artist|track|playlist|label)(?:/[-\w\d]+)?/([\w\d]+)`,
)

func parseQobuzURL(rawURL string) (string, string, error) {
	// If URL has a scheme, require qobuz.com domain
	if strings.Contains(rawURL, "://") && !strings.Contains(rawURL, "qobuz.com") {
		return "", "", fmt.Errorf("not a recognised Qobuz URL")
	}
	m := reQobuzURL.FindStringSubmatch(rawURL)
	if m == nil {
		return "", "", fmt.Errorf("not a recognised Qobuz URL")
	}
	return m[1], m[2], nil
}

// ---- progress bar helpers ----

const barLabelWidth = 42

// barLabel builds a fixed-width label for a track progress bar.
func barLabel(trackNum int, title string) string {
	var label string
	if trackNum > 0 {
		label = fmt.Sprintf("  %02d. %s", trackNum, title)
	} else {
		label = "  " + title
	}
	return truncateStr(label, barLabelWidth)
}

// truncateStr pads or truncates s to exactly n runes.
func truncateStr(s string, n int) string {
	runes := []rune(s)
	if len(runes) > n {
		return string(runes[:n-1]) + "…"
	}
	return s + strings.Repeat(" ", n-len(runes))
}

// ---- ID helpers ----

// idStr converts a JSON-decoded ID (float64 or string) to its integer string
// representation without scientific notation. JSON numbers are decoded as
// float64 in map[string]interface{}, so large IDs like 98439707 would render
// as "9.8439707e+07" with %v — which the Qobuz API does not recognize.
func idStr(v interface{}) string {
	switch n := v.(type) {
	case float64:
		return strconv.FormatInt(int64(n), 10)
	case string:
		return n
	default:
		return fmt.Sprintf("%v", v)
	}
}

// ---- misc helpers ----

var reUnsafe = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)

func sanitize(s string) string {
	s = reUnsafe.ReplaceAllString(s, "_")
	return strings.TrimSpace(s)
}

func nestedStr(m map[string]interface{}, keys ...string) string {
	var cur interface{} = m
	for _, k := range keys {
		mm, ok := cur.(map[string]interface{})
		if !ok {
			return ""
		}
		cur = mm[k]
	}
	s, _ := cur.(string)
	return s
}

func safeGet(d map[string]interface{}, keys ...string) string {
	return nestedStr(d, keys...)
}

func releaseYear(meta map[string]interface{}) string {
	if rd, ok := meta["release_date_original"].(string); ok && len(rd) >= 4 {
		return rd[:4]
	}
	return "0000"
}

func isLocalFile(s string) bool {
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return false
	}
	_, err := os.Stat(s)
	return err == nil
}

// SearchResults holds items for CLI display.
type SearchResult struct {
	Text string
	URL  string
}

// Search performs a typed search and returns display items.
func Search(client *api.Client, itemType, query string, limit int) ([]SearchResult, error) {
	var rawResults map[string]interface{}
	var err error
	var itemsKey, format string
	requiresExtra := false

	switch itemType {
	case "album":
		rawResults, err = client.SearchAlbums(query, limit)
		itemsKey = "albums"
		format = "{artist[name]} - {title}"
		requiresExtra = true
	case "track":
		rawResults, err = client.SearchTracks(query, limit)
		itemsKey = "tracks"
		format = "{performer[name]} - {title}"
		requiresExtra = true
	case "artist":
		rawResults, err = client.SearchArtists(query, limit)
		itemsKey = "artists"
		format = "{name} - ({albums_count} releases)"
	case "playlist":
		rawResults, err = client.SearchPlaylists(query, limit)
		itemsKey = "playlists"
		format = "{name} - ({tracks_count} releases)"
	default:
		return nil, fmt.Errorf("unknown type: %s", itemType)
	}
	if err != nil {
		return nil, err
	}

	section, _ := rawResults[itemsKey].(map[string]interface{})
	items, _ := section["items"].([]interface{})

	var results []SearchResult
	for _, item := range items {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		text := renderFormat(format, m)
		if requiresExtra {
			dur := formatDuration(int(getFloat(m, "duration")))
			hires := "LOSSLESS"
			if b, _ := m["hires_streamable"].(bool); b {
				hires = "HI-RES"
			}
			text = fmt.Sprintf("%s - %s [%s]", text, dur, hires)
		}
		id := idStr(m["id"])
		results = append(results, SearchResult{
			Text: text,
			URL:  fmt.Sprintf("https://play.qobuz.com/%s/%s", itemType, id),
		})
	}
	return results, nil
}

func renderFormat(format string, m map[string]interface{}) string {
	// Simple key substitution: {key} → m[key], {obj[key]} → m[obj][key]
	reKey := regexp.MustCompile(`\{(\w+)(?:\[(\w+)\])?\}`)
	return reKey.ReplaceAllStringFunc(format, func(match string) string {
		parts := reKey.FindStringSubmatch(match)
		if parts[2] != "" {
			sub, _ := m[parts[1]].(map[string]interface{})
			if sub == nil {
				return "n/a"
			}
			v, _ := sub[parts[2]].(string)
			return v
		}
		switch v := m[parts[1]].(type) {
		case string:
			return v
		case float64:
			return strconv.Itoa(int(v))
		default:
			return fmt.Sprintf("%v", v)
		}
	})
}

func formatDuration(secs int) string {
	h := secs / 3600
	m := (secs % 3600) / 60
	s := secs % 60
	if h > 0 {
		return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

func getFloat(m map[string]interface{}, key string) float64 {
	v, _ := m[key].(float64)
	return v
}

// ---- lucky search helper used by CLI ----

func SearchURLs(client *api.Client, itemType, query string, limit int) ([]string, error) {
	results, err := Search(client, itemType, query, limit)
	if err != nil {
		return nil, err
	}
	urls := make([]string, len(results))
	for i, r := range results {
		urls[i] = r.URL
	}
	return urls, nil
}
