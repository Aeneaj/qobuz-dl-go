// Package downloader orchestrates metadata lookup, file downloads, and tagging.
// Translated from downloader.py + core.py + utils.py.
package downloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"

	"github.com/Aeneaj/qobuz-dl-go/internal/api"
)

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
// It does not store a context; callers pass ctx to each method.
type Downloader struct {
	Client     *api.Client
	Opts       Options
	db         *downloadDB
	httpClient *http.Client

	// bars holds the mpb container while it is rendering. See termOut.
	bars atomic.Value
}

// termOut returns the writer for terminal messages. While a progress
// container is live mpb owns the cursor — it repositions and repaints every
// refresh — so writing straight to stdout garbles the bars, and worker
// goroutines make it worse by writing at once. mpb.Progress serialises writes
// against its own render loop, so route through it whenever one is active.
func (d *Downloader) termOut() io.Writer {
	if p, ok := d.bars.Load().(*mpb.Progress); ok && p != nil {
		return p
	}
	return os.Stdout
}

// withBars marks p as the active container and returns the cleanup to defer.
func (d *Downloader) withBars(p *mpb.Progress) func() {
	d.bars.Store(p)
	return func() { d.bars.Store((*mpb.Progress)(nil)) }
}

// New creates a Downloader. Returns an error if the download directory cannot
// be created or the downloads DB cannot be opened. OAuth callers may pass an
// empty Directory.
func New(client *api.Client, opts Options) (*Downloader, error) {
	if opts.FolderFormat == "" {
		opts.FolderFormat = "{artist} - {album} ({year}) [{bit_depth}B-{sampling_rate}kHz]"
	}
	if opts.TrackFormat == "" {
		opts.TrackFormat = "{tracknumber}. {tracktitle}"
	}
	if opts.Workers <= 0 {
		opts.Workers = 3
	}
	if opts.Directory != "" {
		if err := os.MkdirAll(opts.Directory, 0755); err != nil {
			return nil, fmt.Errorf("create download directory %q: %w", opts.Directory, err)
		}
	}

	dl := &Downloader{
		Client:     client,
		Opts:       opts,
		httpClient: &http.Client{Timeout: 10 * time.Minute},
	}
	if !opts.NoDB && opts.DBPath != "" {
		db, err := openDB(opts.DBPath)
		if err != nil {
			return nil, fmt.Errorf("open downloads DB %q: %w (use --no-db to bypass)", opts.DBPath, err)
		}
		dl.db = db
	}
	return dl, nil
}

// HandleURL dispatches a URL to the appropriate download flow.
// Supports Qobuz URLs and Last.fm user playlist URLs.
func (d *Downloader) HandleURL(ctx context.Context, rawURL string) error {
	// Last.fm user playlists (loved tracks, recent tracks)
	if strings.Contains(rawURL, "last.fm") {
		username, listType, err := parseLastFMURL(rawURL)
		if err != nil {
			return err
		}
		return d.downloadLastFMPlaylist(ctx, username, listType)
	}

	urlType, itemID, err := parseQobuzURL(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %w", rawURL, err)
	}

	switch urlType {
	case "album":
		return d.downloadAlbum(ctx, itemID, d.Opts.Directory)
	case "track":
		return d.downloadTrackByID(ctx, itemID, d.Opts.Directory)
	case "artist":
		pages, err := d.Client.GetArtistMeta(ctx, itemID)
		if err != nil {
			return err
		}
		return d.downloadArtist(ctx, pages)
	case "playlist":
		pages, err := d.Client.GetPlaylistMeta(ctx, itemID)
		if err != nil {
			return err
		}
		return d.downloadPlaylist(ctx, pages)
	case "label":
		pages, err := d.Client.GetLabelMeta(ctx, itemID)
		if err != nil {
			return err
		}
		return d.downloadLabelOrArtist(ctx, pages, "albums", "label")
	default:
		return fmt.Errorf("unsupported URL type: %s", urlType)
	}
}

// ---- collection downloads ----

func (d *Downloader) downloadArtist(ctx context.Context, pages []map[string]interface{}) error {
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
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create artist directory %q: %w", dir, err)
	}
	fmt.Printf("\033[33mDownloading discography: %s (%d albums)\033[0m\n", name, len(items))

	for _, item := range items {
		id := idStr(item["id"])
		if err := d.downloadAlbum(ctx, id, dir); err != nil {
			fmt.Printf("\033[31mError on album %s: %v. Skipping...\033[0m\n", id, err)
		}
	}
	return nil
}

func (d *Downloader) downloadPlaylist(ctx context.Context, pages []map[string]interface{}) error {
	if len(pages) == 0 {
		return nil
	}
	name, _ := pages[0]["name"].(string)
	dir := filepath.Join(d.Opts.Directory, sanitize(name))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create playlist directory %q: %w", dir, err)
	}

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
		if err := d.downloadTrackByID(ctx, id, dir); err != nil {
			fmt.Printf("\033[31mError on track %s: %v. Skipping...\033[0m\n", id, err)
		}
	}

	if !d.Opts.NoM3U {
		makeM3U(dir)
	}
	return nil
}

func (d *Downloader) downloadLabelOrArtist(ctx context.Context, pages []map[string]interface{}, itemKey, collectionType string) error {
	if len(pages) == 0 {
		return nil
	}
	name, _ := pages[0]["name"].(string)
	dir := filepath.Join(d.Opts.Directory, sanitize(name))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create %s directory %q: %w", collectionType, dir, err)
	}

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
		if err := d.downloadAlbum(ctx, id, dir); err != nil {
			fmt.Printf("\033[31mError on album %s: %v. Skipping...\033[0m\n", id, err)
		}
	}
	return nil
}

// ---- album download ----

// trackJob bundles per-track info collected in Phase 1 and consumed in Phase 2
// of an album download.
type trackJob struct {
	idx      int
	trackURL map[string]interface{}
	track    map[string]interface{}
	trackDir string
	trackID  string
	bar      *mpb.Bar
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
	fmt.Printf("\n\033[1m♫  %s\033[0m  ·  \033[33m%s %v/%v\033[0m  ·  %d tracks\n\n",
		title, fileFormat, bitDepth, samplingRate, trackCount)

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

	p := mpb.NewWithContext(ctx, mpb.WithRefreshRate(150*time.Millisecond))
	restore := d.withBars(p)
	jobs := d.collectTrackJobs(ctx, p, rawItems, albumDir, isMultiDisc, meta, trackFmt, isMP3)
	d.runTrackJobs(ctx, jobs, meta, isMP3, trackFmt)
	p.Wait()
	restore()

	fmt.Printf("\033[32m✓  Completed: %s\033[0m\n\n", title)
	return nil
}

// shouldSkipAlbum applies the early validation gates (streamable flag and the
// IgnoreSingles filter). When the album must be skipped it prints the reason
// and returns true.
func (d *Downloader) shouldSkipAlbum(meta map[string]interface{}, albumID string) bool {
	if streamable, ok := meta["streamable"].(bool); ok && !streamable {
		fmt.Printf("\033[90mAlbum %s is not streamable, skipping\033[0m\n", albumID)
		return true
	}
	if d.Opts.IgnoreSingles {
		releaseType, _ := meta["release_type"].(string)
		artistName := nestedStr(meta, "artist", "name")
		if releaseType != "album" || artistName == "Various Artists" {
			title, _ := meta["title"].(string)
			fmt.Printf("\033[90mIgnoring Single/EP/VA: %s\033[0m\n", title)
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
		label := barLabel(trackNum, getTitle(track))
		bar := p.New(0,
			mpb.BarStyle().Lbound("╢").Filler("█").Tip("█").Padding("░").Rbound("╟"),
			mpb.BarPriority(idx),
			mpb.PrependDecorators(decor.Name(label)),
			mpb.AppendDecorators(
				decor.Counters(decor.SizeB1024(0), " % .1f / % .1f "),
				decor.EwmaSpeed(decor.SizeB1024(0), "% .1f MiB/s", 30),
				decor.OnComplete(decor.Name(""), " \033[32m✓\033[0m"),
			),
		)

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
		fmt.Printf("\033[90mDemo track, skipping\033[0m\n")
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
			fmt.Printf("\033[90mTrack %s already downloaded, skipping\033[0m\n", trackID)
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

	fmt.Printf("\n\033[1m♫  %s\033[0m  ·  \033[33m%s — %s\033[0m\n\n", title, performer, fileFormat)

	trackNum := 0
	if tn, ok := meta["track_number"].(float64); ok {
		trackNum = int(tn)
	}
	p := mpb.NewWithContext(ctx, mpb.WithRefreshRate(150*time.Millisecond))
	restore := d.withBars(p)
	bar := p.New(0,
		mpb.BarStyle().Lbound("╢").Filler("█").Tip("█").Padding("░").Rbound("╟"),
		mpb.PrependDecorators(decor.Name(barLabel(trackNum, title))),
		mpb.AppendDecorators(
			decor.Counters(decor.SizeB1024(0), " % .1f / % .1f "),
			decor.EwmaSpeed(decor.SizeB1024(0), "% .1f MiB/s", 30),
			decor.OnComplete(decor.Name(""), " \033[32m✓\033[0m"),
		),
	)

	if err := d.downloadAndTag(ctx, trackDir, 1, trackURL, meta, meta, true, isMP3, trackFmt, bar); err != nil {
		bar.Abort(false)
		p.Wait()
		restore()
		return err
	}
	if d.db != nil {
		if err := d.db.add(trackID); err != nil {
			fmt.Fprintf(d.termOut(), "\033[33mWarning: could not record track in DB: %v\033[0m\n", err)
		}
	}
	p.Wait()
	restore()

	fmt.Printf("\033[32m✓  Completed: %s\033[0m\n\n", title)
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
	fmt.Printf("\033[90mTrack %s in DB but file is missing — re-downloading\033[0m\n", trackID)
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
	bar *mpb.Bar,
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

func (d *Downloader) downloadWithProgress(ctx context.Context, rawURL, dest string, bar *mpb.Bar) error {
	var (
		totalSize   int64 = -1 // full file size, resolved from Content-Length or Content-Range
		barCredited int64      // bytes already reflected in the bar across all attempts
	)

	for attempt := 0; attempt < maxDownloadRetries; attempt++ {
		if err := waitBeforeRetry(ctx, attempt); err != nil {
			return err
		}

		// Bytes already saved from a previous attempt.
		offset := currentOffset(dest)

		req, err := buildRangeRequest(ctx, rawURL, offset)
		if err != nil {
			return err
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
			totalSize = resolveTotalSize(resp, offset)
		}

		// Set bar total for display only — do NOT trigger auto-completion here.
		// The bar is explicitly completed after io.Copy returns to avoid mpb
		// closing its operateState channel while ProxyReader is still active.
		if bar != nil && totalSize > 0 {
			bar.SetTotal(totalSize, false)
		}

		// Fast-forward bar for bytes already on disk from prior attempts.
		if bar != nil && offset > barCredited {
			bar.IncrBy(int(offset - barCredited))
			barCredited = offset
		}

		f, err := openOutput(dest, offset)
		if err != nil {
			resp.Body.Close()
			return err
		}

		n, copyErr := copyAndCommit(f, resp.Body, bar)

		// Always close all handles before deciding what to do next.
		resp.Body.Close()
		f.Close()

		barCredited += n
		written := offset + n

		if copyErr == nil {
			if totalSize > 0 && written != totalSize {
				return fmt.Errorf("incomplete download: got %d of %d bytes", written, totalSize)
			}
			finalizeBar(bar, totalSize, written)
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

// waitBeforeRetry sleeps with exponential backoff (1s, 2s, 4s, 8s) before a
// retry attempt. attempt 0 is the first try and returns immediately. Returns
// ctx.Err() if the context is cancelled while sleeping.
func waitBeforeRetry(ctx context.Context, attempt int) error {
	if attempt == 0 {
		return nil
	}
	delay := time.Duration(1<<(attempt-1)) * time.Second
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(delay):
		return nil
	}
}

// currentOffset reports the size of an existing partial file at dest, or 0 if
// the file does not exist or cannot be stat'd. Used to seed the Range header
// when resuming a download.
func currentOffset(dest string) int64 {
	if fi, err := os.Stat(dest); err == nil {
		return fi.Size()
	}
	return 0
}

// buildRangeRequest builds a GET request for rawURL, adding a "Range:
// bytes=offset-" header when offset > 0 to resume a partial download.
func buildRangeRequest(ctx context.Context, rawURL string, offset int64) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	return req, nil
}

// resolveTotalSize derives the full file size from a response. It prefers the
// "Content-Range: bytes N-M/TOTAL" trailer on a 206 reply, falls back to
// offset + Content-Length, and returns -1 when neither yields a positive
// value.
func resolveTotalSize(resp *http.Response, offset int64) int64 {
	if resp.StatusCode == http.StatusPartialContent {
		if cr := resp.Header.Get("Content-Range"); cr != "" {
			if idx := strings.LastIndex(cr, "/"); idx >= 0 {
				if t, err := strconv.ParseInt(cr[idx+1:], 10, 64); err == nil && t > 0 {
					return t
				}
			}
		}
	}
	if resp.ContentLength > 0 {
		return offset + resp.ContentLength
	}
	return -1
}

// openOutput opens the destination file for writing. When offset == 0 it
// creates a fresh file (truncating any leftover); otherwise it opens the
// existing file in append-only mode to resume.
func openOutput(dest string, offset int64) (*os.File, error) {
	if offset == 0 {
		return os.Create(dest)
	}
	return os.OpenFile(dest, os.O_APPEND|os.O_WRONLY, 0644)
}

// copyAndCommit pipes body into f, optionally wrapping body in bar's
// ProxyReader so the progress bar advances live as bytes arrive. The proxy
// reader is closed before returning so its goroutine settles; the caller
// retains ownership of body and f.
func copyAndCommit(f *os.File, body io.Reader, bar *mpb.Bar) (int64, error) {
	reader := body
	var pr io.ReadCloser
	if bar != nil {
		pr = bar.ProxyReader(body)
		reader = pr
	}
	n, err := io.Copy(f, reader)
	if pr != nil {
		pr.Close()
	}
	return n, err
}

// finalizeBar marks bar complete after io.Copy has fully returned. Doing it
// here (and not during SetTotal) prevents mpb from closing its internal
// operateState channel while ProxyReader is still active.
func finalizeBar(bar *mpb.Bar, totalSize, written int64) {
	if bar == nil {
		return
	}
	completedAt := totalSize
	if completedAt <= 0 {
		completedAt = written
	}
	bar.SetTotal(completedAt, true)
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
func (d *Downloader) downloadExtra(ctx context.Context, rawURL, dest string) {
	if _, err := os.Stat(dest); err == nil {
		fmt.Printf("\033[90m%s already downloaded\033[0m\n", filepath.Base(dest))
		return
	}
	fmt.Printf("\033[90mDownloading %s...\033[0m\n", filepath.Base(dest))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
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

	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error { //nolint:errcheck
		if err != nil || d.IsDir() {
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
		name := d.Name()
		fmt.Fprintf(&sb, "\n\n#EXTINF:-1,%s\n%s",
			strings.TrimSuffix(name, filepath.Ext(name)), rel)
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

func (d *Downloader) DownloadURLs(ctx context.Context, urls []string) {
	for _, u := range urls {
		if isLocalFile(u) {
			d.downloadFromFile(ctx, u)
		} else {
			if err := d.HandleURL(ctx, u); err != nil {
				fmt.Printf("\033[31mError: %v\033[0m\n", err)
			}
		}
	}
	// Clean leftover .tmp files
	cleanTmp(d.Opts.Directory)
}

func (d *Downloader) downloadFromFile(ctx context.Context, path string) {
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
	d.DownloadURLs(ctx, urls)
}

func cleanTmp(dir string) {
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error { //nolint:errcheck
		if err == nil && !d.IsDir() {
			name := d.Name()
			if strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".tmp") {
				os.Remove(path)
			}
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

// smartDiscogFilter keeps one release per album title: the highest quality
// one credited to the requested artist, preferring remasters when the
// discography offers any.
func smartDiscogFilter(requestedArtist string, items []map[string]interface{}) []map[string]interface{} {
	grouped, order := groupByEssence(items)

	var result []map[string]interface{}
	for _, key := range order {
		if best, ok := pickBest(requestedArtist, grouped[key]); ok {
			result = append(result, best)
		}
	}
	return result
}

// groupByEssence buckets albums by normalised title, returning the buckets
// plus the keys in first-seen order so the output stays deterministic.
func groupByEssence(items []map[string]interface{}) (map[string][]map[string]interface{}, []string) {
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
	return grouped, order
}

// groupQuality is what a group of same-title releases is judged against:
// the best audio quality on offer, and whether any of them is a remaster.
type groupQuality struct {
	bestBitDepth   float64
	bestSampleRate float64 // measured at bestBitDepth, not overall
	hasRemaster    bool
}

// pickBest returns the release representing a group, and whether any
// release qualified at all.
func pickBest(requestedArtist string, albums []map[string]interface{}) (map[string]interface{}, bool) {
	// One pass for the aggregates, caching each album's "is remaster" flag so
	// the regex runs once per album rather than again in the selection loop.
	var q groupQuality
	isRemaster := make([]bool, len(albums))
	for i, a := range albums {
		bd, _ := a["maximum_bit_depth"].(float64)
		sr, _ := a["maximum_sampling_rate"].(float64)
		switch {
		case bd > q.bestBitDepth:
			q.bestBitDepth, q.bestSampleRate = bd, sr // a higher depth resets the sampling-rate race
		case bd == q.bestBitDepth && sr > q.bestSampleRate:
			q.bestSampleRate = sr
		}
		if isAlbumType("remaster", a) {
			isRemaster[i] = true
			q.hasRemaster = true
		}
	}

	for i, a := range albums {
		if qualifies(a, q, isRemaster[i], requestedArtist) {
			return a, true
		}
	}
	return nil, false
}

// qualifies reports whether an album is the one to keep for its group.
func qualifies(a map[string]interface{}, q groupQuality, isRemaster bool, requestedArtist string) bool {
	bd, _ := a["maximum_bit_depth"].(float64)
	sr, _ := a["maximum_sampling_rate"].(float64)
	if bd != q.bestBitDepth || sr != q.bestSampleRate {
		return false
	}
	if nestedStr(a, "artist", "name") != requestedArtist {
		return false
	}
	// Once a group contains a remaster, only remasters are eligible.
	return isRemaster || !q.hasRemaster
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

// expandPlaceholders substitutes {placeholder} tokens in format with values
// from attrs. Each value is passed through sanitize so illegal path characters
// (including "/" and "\") are neutralised before insertion — this lets a
// format string like "{artist}/{album}" contain real subfolder separators
// while metadata values (e.g. an artist named "AC/DC") cannot inject them.
func expandPlaceholders(format string, attrs map[string]string) string {
	result := format
	for k, v := range attrs {
		if v == "" || v == "<nil>" || v == "%!v(MISSING)" {
			v = "n_a"
		}
		result = strings.ReplaceAll(result, k, sanitize(v))
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

// safeJoin joins base with elem, cleans the result, and verifies it still
// lives under base. Callers must pre-translate user-format separators with
// filepath.FromSlash so subfolder templates work on Windows. Guards against
// path traversal from malicious templates or metadata (e.g. "../../etc").
func safeJoin(base, elem string) (string, error) {
	cleanBase := filepath.Clean(base)
	joined := filepath.Clean(filepath.Join(cleanBase, elem))
	if joined != cleanBase && !strings.HasPrefix(joined, cleanBase+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes base directory %q", elem, cleanBase)
	}
	return joined, nil
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
func Search(ctx context.Context, client *api.Client, itemType, query string, limit int) ([]SearchResult, error) {
	var rawResults map[string]interface{}
	var err error
	var itemsKey, format string
	requiresExtra := false

	switch itemType {
	case "album":
		rawResults, err = client.SearchAlbums(ctx, query, limit)
		itemsKey = "albums"
		format = "{artist[name]} - {title}"
		requiresExtra = true
	case "track":
		rawResults, err = client.SearchTracks(ctx, query, limit)
		itemsKey = "tracks"
		format = "{performer[name]} - {title}"
		requiresExtra = true
	case "artist":
		rawResults, err = client.SearchArtists(ctx, query, limit)
		itemsKey = "artists"
		format = "{name} - ({albums_count} releases)"
	case "playlist":
		rawResults, err = client.SearchPlaylists(ctx, query, limit)
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

func SearchURLs(ctx context.Context, client *api.Client, itemType, query string, limit int) ([]string, error) {
	results, err := Search(ctx, client, itemType, query, limit)
	if err != nil {
		return nil, err
	}
	urls := make([]string, len(results))
	for i, r := range results {
		urls[i] = r.URL
	}
	return urls, nil
}
