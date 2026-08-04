package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Aeneaj/qobuz-dl-go/internal/config"
	"github.com/Aeneaj/qobuz-dl-go/internal/downloader"
	"github.com/Aeneaj/qobuz-dl-go/internal/lyrics"
	"github.com/Aeneaj/qobuz-dl-go/internal/ui"
)

// runTUI opens the full-program TUI: menu, search, queue, downloads, lyrics,
// CSV import and config, all in one screen.
//
// Everything before p.Run() still prints to stdout (login, quality) because
// the alt screen is not up yet; from there on nothing may write to stdout, so
// the downloader's termOut goes to io.Discard and the backend reports through
// the program instead.
func runTUI(ctx context.Context, f *cliFlags) {
	dl := mustDownloader(ctx, f)

	be := &tuiBackend{dl: dl}
	p := tea.NewProgram(ui.NewShell(ctx, be), tea.WithAltScreen(), tea.WithContext(ctx))
	be.prog = p
	dl.SetUI(p)

	if _, err := p.Run(); err != nil {
		fatalf("tui: %v", err)
	}
}

// tuiBackend is the one real implementation of ui.Backend. It lives here, not
// in internal/ui, because internal/downloader already imports internal/ui.
type tuiBackend struct {
	dl   *downloader.Downloader
	prog *tea.Program
}

func (b *tuiBackend) Search(ctx context.Context, kind, query string, limit int) ([]ui.Item, error) {
	hits, err := downloader.Search(ctx, b.dl.Client, kind, query, limit)
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
	b.dl.DownloadURLs(ctx, urls)
	return nil
}

func (b *tuiBackend) CSV(ctx context.Context, path string) (string, error) {
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("no se encuentra el CSV: %s", path)
	}
	b.dl.DownloadCSV(ctx, path, "failed_downloads.csv")
	return "importación CSV terminada", nil
}

func (b *tuiBackend) Lyrics(ctx context.Context, dir string) (string, error) {
	resolved, err := resolveScanDir(dir)
	if err != nil {
		return "", err
	}

	b.prog.Send(ui.MsgStatus{Text: "escaneando " + resolved + "…"})
	res, err := lyrics.FetchAll(ctx, resolved, func(done, total int, title, artist string) {
		if done == 0 {
			b.prog.Send(ui.MsgStatus{Text: fmt.Sprintf("%d archivos de audio encontrados", total)})
			return
		}
		b.prog.Send(ui.MsgStatus{Text: fmt.Sprintf("[%d/%d] %s — %s", done, total, title, artist)})
	})
	if err != nil {
		return "", err
	}
	if res.Total == 0 {
		return "no se encontró audio en esa carpeta", nil
	}
	return fmt.Sprintf("letras: %d nuevas · %d ya estaban · %d sin resultado",
		res.Fetched, res.Skipped, len(res.Warnings)), nil
}

func (b *tuiBackend) Config() string {
	data, err := os.ReadFile(filepath.Join(config.ConfigDir(), "config.ini"))
	if err != nil {
		return "no se pudo leer la configuración: " + err.Error()
	}
	return filepath.Join(config.ConfigDir(), "config.ini") + "\n\n" + string(data)
}

func (b *tuiBackend) Purge() error {
	if err := os.Remove(filepath.Join(config.ConfigDir(), "qobuz_dl.db")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (b *tuiBackend) DefaultDir() string { return b.dl.Opts.Directory }
