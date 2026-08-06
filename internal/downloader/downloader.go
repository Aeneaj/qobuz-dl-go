// Package downloader orchestrates metadata lookup, file downloads, and tagging.
// Translated from downloader.py + core.py + utils.py.
package downloader

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"

	"github.com/Aeneaj/qobuz-dl-go/internal/api"
	"github.com/Aeneaj/qobuz-dl-go/internal/ui"
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

// ProgressBar is the per-track progress sink. ProgressBar satisfies it natively;
// ui.TrackHandle implements it explicitly, which is what lets the same
// download code drive either the mpb bars or the bubbletea TUI.
type ProgressBar interface {
	SetTotal(total int64, triggerComplete bool)
	IncrBy(n int)
	ProxyReader(r io.Reader) io.ReadCloser
	Abort(drop bool)
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

	// tui is the bubbletea program when the TUI is driving the display.
	// Non-nil means bubbletea owns the screen and mpb is never created.
	tui *tea.Program
}

// SetUI routes progress to the bubbletea program p instead of mpb. It must be
// called before any download method. Passing nil restores the mpb bars.
func (d *Downloader) SetUI(p *tea.Program) { d.tui = p }

// termOut returns the writer for terminal messages. While a progress
// container is live mpb owns the cursor — it repositions and repaints every
// refresh — so writing straight to stdout garbles the bars, and worker
// goroutines make it worse by writing at once. Under the TUI nothing may reach
// stdout at all; under mpb, mpb.Progress serialises writes against its own
// render loop, so route through it whenever a container is active.
func (d *Downloader) termOut() io.Writer {
	if d.tui != nil {
		return io.Discard
	}
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

// newProgress builds the mpb container for one download run, or nil when the
// TUI is active — bubbletea and mpb both drive the cursor, so only one of them
// may ever be rendering.
func (d *Downloader) newProgress(ctx context.Context) *mpb.Progress {
	if d.tui != nil {
		return nil
	}
	return mpb.NewWithContext(ctx, mpb.WithRefreshRate(150*time.Millisecond))
}

// newBar creates the progress sink for a single track: an mpb bar on p, or a
// handle registered with the TUI model. p is nil in the latter case.
// priority orders mpb bars; the TUI orders by registration.
func (d *Downloader) newBar(p *mpb.Progress, priority, trackNum int, title, trackID string) ProgressBar {
	if d.tui != nil {
		h := ui.NewTrackHandle(trackID, d.tui)
		d.tui.Send(ui.MsgRegisterTrack{
			ID:      trackID,
			Num:     trackNum,
			Name:    title,
			Counter: h.Counter(),
		})
		return h
	}
	return p.New(0,
		mpb.BarStyle().Lbound("╢").Filler("█").Tip("█").Padding("░").Rbound("╟"),
		mpb.BarPriority(priority),
		mpb.PrependDecorators(decor.Name(barLabel(trackNum, title))),
		mpb.AppendDecorators(
			decor.Counters(decor.SizeB1024(0), " % .1f / % .1f "),
			decor.EwmaSpeed(decor.SizeB1024(0), "% .1f MiB/s", 30),
			decor.OnComplete(decor.Name(""), " \033[32m✓\033[0m"),
		),
	)
}

// announceAlbum shows the album header — a stdout line, or the TUI header.
func (d *Downloader) announceAlbum(title, artist, format string, tracks int) {
	if d.tui != nil {
		d.tui.Send(ui.MsgAlbum{Title: title, Artist: artist, Format: format, Tracks: tracks})
		return
	}
	fmt.Fprintf(d.termOut(), "\n\033[1m♫  %s\033[0m  ·  \033[33m%s\033[0m  ·  %d tracks\n\n", title, format, tracks)
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
		return d.downloadAlbumCollection(ctx, pages, "albums", "discography", d.Opts.SmartDiscog)
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
		return d.downloadAlbumCollection(ctx, pages, "albums", "label", false)
	default:
		return fmt.Errorf("unsupported URL type: %s", urlType)
	}
}

func (d *Downloader) DownloadURLs(ctx context.Context, urls []string) {
	for _, u := range urls {
		if isLocalFile(u) {
			d.downloadFromFile(ctx, u)
		} else {
			if err := d.HandleURL(ctx, u); err != nil {
				fmt.Fprintf(d.termOut(), "\033[31mError: %v\033[0m\n", err)
			}
		}
	}
	// Clean leftover .tmp files
	cleanTmp(d.Opts.Directory)
}

func (d *Downloader) downloadFromFile(ctx context.Context, path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(d.termOut(), "\033[31mCannot read file %s: %v\033[0m\n", path, err)
		return
	}
	var urls []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			urls = append(urls, line)
		}
	}
	fmt.Fprintf(d.termOut(), "\033[33mDownloading %d URLs from %s\033[0m\n", len(urls), path)
	d.DownloadURLs(ctx, urls)
}

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
