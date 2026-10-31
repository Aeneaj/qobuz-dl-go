package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Aeneaj/qobuz-dl-go/internal/config"
	"github.com/Aeneaj/qobuz-dl-go/internal/downloader"
	"github.com/Aeneaj/qobuz-dl-go/internal/lyrics"
	"github.com/Aeneaj/qobuz-dl-go/internal/ui"
)

// errNoSession is what every Qobuz-backed action returns before login. It
// names the menu entry that fixes it, since that is where the user is looking.
var errNoSession = errors.New(`not signed in — choose "Log in (OAuth)" in the menu`)

// runTUI opens the full-program TUI: menu, search, queue, downloads, lyrics,
// CSV import and config.
//
// Missing credentials must not stop the shell from opening — the menu is
// where OAuth lives, so refusing to start would hide the only fix. The
// downloader is therefore optional at boot: nil until a session exists, and
// the actions that need it fail with errNoSession instead.
func runTUI(ctx context.Context, f *cliFlags) {
	be := &tuiBackend{flags: f}

	// initDownloader prints as it logs in, which is safe here: the alt screen
	// is not up yet. An error never stops the shell, but it is kept — it may
	// be a bad directory or a dead network rather than a missing token, and
	// telling the user "log in" would send them down the wrong path.
	if dl, err := initDownloader(ctx, f); err == nil {
		be.dl = dl
	} else {
		be.bootErr = err
	}

	p := tea.NewProgram(ui.NewShell(ctx, be), tea.WithAltScreen(), tea.WithContext(ctx))
	be.prog = p
	if be.dl != nil {
		be.dl.SetUI(p)
	}

	if _, err := p.Run(); err != nil {
		fatalf("tui: %v", err)
	}
}

// tuiBackend is the one real implementation of ui.Backend. It lives here, not
// in internal/ui, because internal/downloader already imports internal/ui.
type tuiBackend struct {
	flags *cliFlags
	prog  *tea.Program

	// Login sets these from a tea.Cmd while View reads them on every frame.
	mu      sync.RWMutex
	dl      *downloader.Downloader // nil until a session exists
	bootErr error                  // why dl is nil, if it was ever attempted
}

// session returns the downloader for Qobuz actions, explaining the real reason
// when there is none.
func (b *tuiBackend) session() (*downloader.Downloader, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	switch {
	case b.dl != nil:
		return b.dl, nil
	case b.bootErr != nil:
		return nil, fmt.Errorf(`%w — or choose "Log in (OAuth)" in the menu`, b.bootErr)
	default:
		return nil, errNoSession
	}
}

func (b *tuiBackend) setSession(dl *downloader.Downloader) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.dl, b.bootErr = dl, nil
}

func (b *tuiBackend) LoggedIn() bool {
	_, err := b.session()
	return err == nil
}

// Login drops out of the alt screen, runs the ordinary CLI OAuth flow, and
// comes back.
//
// The flow prints straight to the terminal, which the alt screen would swallow.
// Passing the program keeps it off stdin: bubbletea reads there, and a reader
// left behind by the flow would steal keystrokes once the screen is back.
func (b *tuiBackend) Login(ctx context.Context) (string, error) {
	if err := b.prog.ReleaseTerminal(); err != nil {
		return "", fmt.Errorf("could not release the terminal: %w", err)
	}
	defer b.prog.RestoreTerminal() //nolint:errcheck

	if err := oauthLogin(ctx, "", b.prog); err != nil {
		return "", err
	}

	// Still outside the alt screen, so initDownloader may print freely.
	dl, err := initDownloader(ctx, b.flags)
	if err != nil {
		return "", err
	}
	dl.SetUI(b.prog)
	b.setSession(dl)
	return ui.T("signed in successfully"), nil
}

func (b *tuiBackend) Search(ctx context.Context, kind, query string, limit int) ([]ui.Item, error) {
	dl, err := b.session()
	if err != nil {
		return nil, err
	}
	hits, err := downloader.Search(ctx, dl.Client, kind, query, limit)
	if err != nil {
		return nil, err
	}
	items := make([]ui.Item, len(hits))
	for i, h := range hits {
		items[i] = ui.Item{Label: h.Text, URL: h.URL}
	}
	return items, nil
}

func (b *tuiBackend) Download(ctx context.Context, urls []string) error {
	dl, err := b.session()
	if err != nil {
		return err
	}
	return dl.DownloadURLs(ctx, urls)
}

func (b *tuiBackend) CSV(ctx context.Context, path string) (string, error) {
	dl, err := b.session()
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("CSV not found: %s", path)
	}
	failed, err := dl.DownloadCSV(ctx, path, defaultFailedCSV)
	if err != nil {
		return "", err
	}
	if failed > 0 {
		return fmt.Sprintf(ui.T("CSV import finished: %d failed, listed in %s"), failed, defaultFailedCSV), nil
	}
	return ui.T("CSV import finished"), nil
}

// Lyrics needs no Qobuz session — LRCLIB is public — so it deliberately skips
// the b.dl check and works before login.
func (b *tuiBackend) Lyrics(ctx context.Context, dir string) (string, error) {
	resolved, err := config.ResolveDir(dir, false)
	if err != nil {
		return "", err
	}

	b.prog.Send(ui.MsgStatus{Text: fmt.Sprintf(ui.T("scanning %s…"), resolved)})
	res, err := lyrics.FetchAll(ctx, resolved, func(done, total int, title, artist string) {
		if done == 0 {
			b.prog.Send(ui.MsgStatus{Text: fmt.Sprintf(ui.T("%d audio files found"), total)})
			return
		}
		b.prog.Send(ui.MsgStatus{Text: fmt.Sprintf("[%d/%d] %s — %s", done, total, title, artist)})
	})
	if err != nil {
		return "", err
	}
	if res.Total == 0 {
		return ui.T("no audio found in that folder"), nil
	}
	return fmt.Sprintf(ui.T("lyrics: %d new · %d already there · %d without a match"),
		res.Fetched, res.Skipped, len(res.Warnings)), nil
}

func (b *tuiBackend) Config() string {
	path := filepath.Join(config.ConfigDir(), "config.ini")
	data, err := os.ReadFile(path)
	if err != nil {
		return ui.T("could not read the settings") + ": " + err.Error()
	}
	return path + "\n\n" + string(data)
}

func (b *tuiBackend) Purge() error {
	if err := os.Remove(filepath.Join(config.ConfigDir(), "qobuz_dl.db")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// DefaultDir falls back to the configured directory when there is no session
// yet, so the lyrics prompt is still prefilled before login.
func (b *tuiBackend) DefaultDir() string {
	if dl, err := b.session(); err == nil {
		return dl.Opts.Directory
	}
	if b.flags != nil && b.flags.Dir != "" {
		return b.flags.Dir
	}
	if cfg, err := config.Load(); err == nil && cfg.DownloadDir != "" {
		return cfg.DownloadDir
	}
	return defaultDownloadDir
}
