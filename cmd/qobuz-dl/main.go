package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Aeneaj/qobuz-dl-go/internal/config"
	"github.com/Aeneaj/qobuz-dl-go/internal/downloader"
	"github.com/Aeneaj/qobuz-dl-go/internal/ui"
)

// version is set at build time via -ldflags "-X main.version=v1.x.x".
var version = "v1.5.0"

const usage = `Usage: qobuz-dl [options] <command> [args]

Commands:
  dl  <URL...>       Download by URL (album/track/artist/label/playlist/last.fm)
  lucky <query>      Download first N search results
  csv <file.csv>     Batch download from a TuneMyMusic CSV export
  oauth [code|url]   Login via OAuth (recommended)
  fun                Interactive search and download mode (line based)
  tui                Full-screen interface: menu, search, queue, downloads
  lyrics [path]      Fetch .lrc files from LRCLIB for a music library

Options:
  -r, --reset        Reconfigure credentials (prompts for user_id + token)
  -s, --show-config  Show config file path and contents
  -p, --purge        Delete the downloads database
  -v, --version      Print version and exit
  -d <dir>           Download directory
  -q <quality>       Quality: 5=MP3, 6=LOSSLESS, 7=24B<96k, 27=24B>96k
  --embed-art        Embed cover art in files
  --albums-only      Skip singles/EPs
  --no-m3u           Skip M3U playlist creation
  --no-fallback      Disable quality fallback
  --og-cover         Use original cover quality
  --no-cover         Skip cover art download
  --no-db            Bypass downloads database
  --workers N        Concurrent track downloads per album (overrides 'workers' in config.ini; default 3)
  --tui              Full-screen download UI (dl/lucky/csv)
  --folder-format    Folder naming format string
  --track-format     Track naming format string
  --smart-discog     Smart discography filter
  --lucky-type       Type for lucky command (album|track|artist|playlist)
  --lucky-n          Number of results for lucky command
  --failed <file>    Output CSV for failed/not-found tracks (csv command, default: failed_downloads.csv)
`

func main() {
	fs := flag.NewFlagSet("qobuz-dl", flag.ExitOnError)
	fs.Usage = func() { fmt.Print(usage) }

	// Short and long spellings bind to the same variable, so there is nothing
	// to OR together at the use site.
	var reset, showCfg, purge, showVer bool
	fs.BoolVar(&reset, "r", false, "")
	fs.BoolVar(&reset, "reset", false, "")
	fs.BoolVar(&showCfg, "s", false, "")
	fs.BoolVar(&showCfg, "show-config", false, "")
	fs.BoolVar(&purge, "p", false, "")
	fs.BoolVar(&purge, "purge", false, "")
	fs.BoolVar(&showVer, "v", false, "")
	fs.BoolVar(&showVer, "version", false, "")

	flags := registerDownloadFlags(fs)
	luckyType := fs.String("lucky-type", "album", "")
	luckyN := fs.Int("lucky-n", 1, "")
	failed := fs.String("failed", "failed_downloads.csv", "")

	fs.Parse(os.Args[1:])

	if showVer {
		fmt.Println("qobuz-dl", version)
		return
	}

	// Context cancelled on Ctrl+C / SIGTERM — propagated into all HTTP calls.
	// Created early so even --reset (which calls bundle.Fetch) is cancellable.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go exitOnSecondInterrupt(ctx, stop)

	// Config actions need no credentials and never fall through to a command.
	switch {
	case reset:
		runReset(ctx)
		return
	case showCfg:
		showConfig()
		return
	case purge:
		purgeDB()
		return
	}

	// Both TUI screens (the `tui` shell and the --tui progress view) read the
	// language here rather than each wiring it themselves. It must be set
	// before any bubbletea program starts — ui.SetLang is not safe once a
	// render loop is running. No config yet, or an unknown code, means English.
	if cfg, err := config.Load(); err == nil {
		ui.SetLang(cfg.Lang)
	}

	args := fs.Args()
	if len(args) == 0 {
		fmt.Print(usage)
		os.Exit(0)
	}
	cmdArgs := args[1:]

	switch args[0] {
	case "fun":
		mustDownloader(ctx, flags).Interactive(ctx)
	case "tui":
		runTUI(ctx, flags)
	case "dl":
		runDL(ctx, cmdArgs, flags)
	case "lucky":
		runLucky(ctx, cmdArgs, flags, *luckyType, *luckyN)
	case "csv":
		runCSV(ctx, cmdArgs, flags, *failed)
	case "oauth":
		runOAuth(ctx, cmdArgs)
	case "lyrics":
		runLyrics(ctx, cmdArgs)
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", args[0])
		fmt.Print(usage)
		os.Exit(1)
	}
}

// exitOnSecondInterrupt restores default signal handling once ctx is
// cancelled, so a second Ctrl+C kills the process even if a goroutine
// ignores ctx.
func exitOnSecondInterrupt(ctx context.Context, stop context.CancelFunc) {
	<-ctx.Done()
	stop()
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	<-ch
	os.Exit(1)
}

func runReset(ctx context.Context) {
	if err := config.Reset(ctx); err != nil {
		fatalf("Error: %v", err)
	}
}

func showConfig() {
	cfgFile := config.ConfigDir() + "/config.ini"
	fmt.Printf("Configuration: %s\n---\n", cfgFile)
	data, _ := os.ReadFile(cfgFile)
	fmt.Println(string(data))
}

func purgeDB() {
	os.Remove(config.ConfigDir() + "/qobuz_dl.db")
	fmt.Println("\033[32mThe database was deleted.\033[0m")
}

// requireArgs exits with "<cmd>: <hint>" when a command got no arguments.
func requireArgs(args []string, cmd, hint string) {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "%s: %s\n", cmd, hint)
		os.Exit(1)
	}
}

// mustDownloader builds the downloader shared by dl/lucky/csv/fun, or exits.
func mustDownloader(ctx context.Context, f *cliFlags) *downloader.Downloader {
	dl, err := initDownloader(ctx, f)
	if err != nil {
		fatalf("%v", err)
	}
	return dl
}

// runDisplay runs fn, wrapped in the bubbletea TUI when --tui is set.
//
// The TUI runs in alt-screen raw mode, where Ctrl+C arrives as a key event
// instead of SIGINT — so the signal context in main() never fires and the
// download goroutine would keep running after the screen is gone. A derived
// context cancelled on exit is what actually stops it.
func runDisplay(ctx context.Context, dl *downloader.Downloader, f *cliFlags, fn func(context.Context)) {
	if !f.TUI {
		fn(ctx)
		return
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	p := tea.NewProgram(ui.NewModel(), tea.WithAltScreen(), tea.WithContext(ctx))
	dl.SetUI(p)

	go func() {
		fn(ctx)
		p.Quit()
	}()

	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "TUI error: %v\n", err)
	}
}

func runDL(ctx context.Context, args []string, f *cliFlags) {
	requireArgs(args, "dl", "provide at least one URL")
	dl := mustDownloader(ctx, f)
	runDisplay(ctx, dl, f, func(ctx context.Context) { dl.DownloadURLs(ctx, args) })
}

func runLucky(ctx context.Context, args []string, f *cliFlags, itemType string, n int) {
	requireArgs(args, "lucky", "provide a search query")
	query := strings.Join(args, " ")
	if len(query) < 3 {
		fatalf("search query too short")
	}
	dl := mustDownloader(ctx, f)
	fmt.Printf("\033[33mSearching %ss for \"%s\" (top %d)...\033[0m\n", itemType, query, n)
	urls, err := downloader.SearchURLs(ctx, dl.Client, itemType, query, n)
	if err != nil {
		fatalf("%v", err)
	}
	runDisplay(ctx, dl, f, func(ctx context.Context) { dl.DownloadURLs(ctx, urls) })
}

func runCSV(ctx context.Context, args []string, f *cliFlags, failedCSV string) {
	requireArgs(args, "csv", "provide path to a TuneMyMusic CSV file")
	dl := mustDownloader(ctx, f)
	runDisplay(ctx, dl, f, func(ctx context.Context) { dl.DownloadCSV(ctx, args[0], failedCSV) })
}

func fatalf(format string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, "\033[31m"+format+"\033[0m\n", a...)
	os.Exit(1)
}
