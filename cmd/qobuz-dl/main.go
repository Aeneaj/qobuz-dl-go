package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/Aeneaj/qobuz-dl-go/internal/api"
	"github.com/Aeneaj/qobuz-dl-go/internal/config"
	"github.com/Aeneaj/qobuz-dl-go/internal/downloader"
)

// version is set at build time via -ldflags "-X main.version=v1.x.x".
var version = "v1.3.0"

const usage = `Usage: qobuz-dl [options] <command> [args]

Commands:
  dl  <URL...>       Download by URL (album/track/artist/label/playlist/last.fm)
  lucky <query>      Download first N search results
  csv <file.csv>     Batch download from a TuneMyMusic CSV export
  oauth [code|url]   Login via OAuth (recommended)
  fun                Interactive search and download mode
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
  --workers N        Concurrent track downloads per album (default 3)
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

	reset := fs.Bool("r", false, "")
	resetLong := fs.Bool("reset", false, "")
	showCfg := fs.Bool("s", false, "")
	showCfgLong := fs.Bool("show-config", false, "")
	purge := fs.Bool("p", false, "")
	purgeLong := fs.Bool("purge", false, "")
	showVer := fs.Bool("v", false, "")
	showVerLong := fs.Bool("version", false, "")

	flags := registerDownloadFlags(fs)
	luckyType := fs.String("lucky-type", "album", "")
	luckyN := fs.Int("lucky-n", 1, "")
	failed := fs.String("failed", "failed_downloads.csv", "")

	fs.Parse(os.Args[1:])

	doReset := *reset || *resetLong
	doShow := *showCfg || *showCfgLong
	doPurge := *purge || *purgeLong

	if *showVer || *showVerLong {
		fmt.Println("qobuz-dl", version)
		return
	}

	if doReset {
		if err := config.Reset(); err != nil {
			fmt.Fprintf(os.Stderr, "\033[31mError: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if doShow {
		cfgFile := config.ConfigDir() + "/config.ini"
		fmt.Printf("Configuration: %s\n---\n", cfgFile)
		data, _ := os.ReadFile(cfgFile)
		fmt.Println(string(data))
		return
	}

	if doPurge {
		dbPath := config.ConfigDir() + "/qobuz_dl.db"
		os.Remove(dbPath)
		fmt.Println("\033[32mThe database was deleted.\033[0m")
		return
	}

	args := fs.Args()
	if len(args) == 0 {
		fmt.Print(usage)
		os.Exit(0)
	}

	// Context cancelled on Ctrl+C / SIGTERM — propagated into all HTTP calls.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Second Ctrl+C → immediate exit (in case a goroutine ignores ctx).
	go func() {
		<-ctx.Done()
		stop() // restore default signal behavior
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt)
		<-ch
		os.Exit(1)
	}()

	cmd := args[0]
	cmdArgs := args[1:]

	switch cmd {
	case "fun":
		dl, err := initDownloader(ctx, flags)
		if err != nil {
			fatalf("%v", err)
		}
		dl.Interactive()

	case "dl":
		if len(cmdArgs) == 0 {
			fmt.Fprintln(os.Stderr, "dl: provide at least one URL")
			os.Exit(1)
		}
		dl, err := initDownloader(ctx, flags)
		if err != nil {
			fatalf("%v", err)
		}
		dl.DownloadURLs(cmdArgs)

	case "lucky":
		if len(cmdArgs) == 0 {
			fmt.Fprintln(os.Stderr, "lucky: provide a search query")
			os.Exit(1)
		}
		query := strings.Join(cmdArgs, " ")
		if len(query) < 3 {
			fatalf("search query too short")
		}
		dl, err := initDownloader(ctx, flags)
		if err != nil {
			fatalf("%v", err)
		}
		fmt.Printf("\033[33mSearching %ss for \"%s\" (top %d)...\033[0m\n", *luckyType, query, *luckyN)
		urls, err := searchByType(dl.Client, *luckyType, query, *luckyN)
		if err != nil {
			fatalf("%v", err)
		}
		dl.DownloadURLs(urls)

	case "csv":
		if len(cmdArgs) == 0 {
			fmt.Fprintln(os.Stderr, "csv: provide path to a TuneMyMusic CSV file")
			os.Exit(1)
		}
		dl, err := initDownloader(ctx, flags)
		if err != nil {
			fatalf("%v", err)
		}
		dl.DownloadCSV(cmdArgs[0], *failed)

	case "oauth":
		codeOrURL := ""
		if len(cmdArgs) > 0 {
			codeOrURL = cmdArgs[0]
		}
		runOAuth(ctx, codeOrURL)

	case "lyrics":
		runLyrics(ctx, cmdArgs)

	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", cmd)
		fmt.Print(usage)
		os.Exit(1)
	}
}

func fatalf(format string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, "\033[31m"+format+"\033[0m\n", a...)
	os.Exit(1)
}

func loadOrInitConfig(skipCredentials bool) (*config.Config, error) {
	cfgDir := config.ConfigDir()
	cfgFile := cfgDir + "/config.ini"
	if _, err := os.Stat(cfgFile); os.IsNotExist(err) {
		if err := os.MkdirAll(cfgDir, 0755); err != nil {
			return nil, err
		}
		fmt.Println("\033[33mFirst run: setting up config...\033[0m")
		if skipCredentials {
			if err := config.InitConfig(); err != nil {
				return nil, err
			}
		} else {
			if err := config.Reset(); err != nil {
				return nil, err
			}
		}
	}
	return config.Load()
}

// cliFlags groups the download-related flags shared by dl/lucky/csv/fun.
// Lucky/CSV-specific flags (lucky-type, lucky-n, failed) stay separate.
type cliFlags struct {
	Dir          string
	Quality      int
	EmbedArt     bool
	AlbumsOnly   bool
	NoM3U        bool
	NoFallback   bool
	OGCover      bool
	NoCover      bool
	NoDB         bool
	Workers      int
	FolderFormat string
	TrackFormat  string
	SmartDiscog  bool
}

func registerDownloadFlags(fs *flag.FlagSet) *cliFlags {
	f := &cliFlags{}
	fs.StringVar(&f.Dir, "d", "", "download directory")
	fs.IntVar(&f.Quality, "q", 0, "quality")
	fs.BoolVar(&f.EmbedArt, "embed-art", false, "")
	fs.BoolVar(&f.AlbumsOnly, "albums-only", false, "")
	fs.BoolVar(&f.NoM3U, "no-m3u", false, "")
	fs.BoolVar(&f.NoFallback, "no-fallback", false, "")
	fs.BoolVar(&f.OGCover, "og-cover", false, "")
	fs.BoolVar(&f.NoCover, "no-cover", false, "")
	fs.BoolVar(&f.NoDB, "no-db", false, "")
	fs.IntVar(&f.Workers, "workers", 0, "")
	fs.StringVar(&f.FolderFormat, "folder-format", "", "")
	fs.StringVar(&f.TrackFormat, "track-format", "", "")
	fs.BoolVar(&f.SmartDiscog, "smart-discog", false, "")
	return f
}

func initDownloader(ctx context.Context, f *cliFlags) (*downloader.Downloader, error) {
	cfg, err := loadOrInitConfig(false)
	if err != nil {
		return nil, err
	}

	// Directory resolution hierarchy: flag -d → config download_dir → default.
	dir := f.Dir
	if dir == "" {
		dir = cfg.DownloadDir
	}
	if dir == "" {
		dir = "./qobuz-downloader"
	}
	resolvedDir, err := config.ResolveDir(dir)
	if err != nil {
		return nil, fmt.Errorf("download directory: %w", err)
	}
	dir = resolvedDir

	quality := f.Quality
	if quality == 0 {
		quality = cfg.DefaultQuality
	}
	folderFmt := f.FolderFormat
	if folderFmt == "" {
		folderFmt = cfg.FolderFormat
	}
	trackFmt := f.TrackFormat
	if trackFmt == "" {
		trackFmt = cfg.TrackFormat
	}

	client := api.New(cfg.AppID, cfg.Secrets, ctx)

	if cfg.UserID == "" || cfg.UserAuthToken == "" {
		return nil, fmt.Errorf("no credentials found — run 'qobuz-dl oauth' to log in, or 'qobuz-dl --reset' to set up manually")
	}
	fmt.Println("\033[33mLogging in...\033[0m")
	if err := client.AuthWithToken(cfg.UserID, cfg.UserAuthToken); err != nil {
		return nil, err
	}

	if err := client.CfgSetup(); err != nil {
		return nil, err
	}

	qualityNames := map[int]string{5: "5 - MP3", 6: "6 - 16 bit, 44.1kHz", 7: "7 - 24 bit, <96kHz", 27: "27 - 24 bit, >96kHz"}
	fmt.Printf("\033[33mSet max quality: %s\033[0m\n", qualityNames[quality])

	opts := downloader.Options{
		Directory:       dir,
		Quality:         quality,
		EmbedArt:        f.EmbedArt || cfg.EmbedArt,
		IgnoreSingles:   f.AlbumsOnly || cfg.AlbumsOnly,
		NoM3U:           f.NoM3U || cfg.NoM3U,
		QualityFallback: !f.NoFallback && !cfg.NoFallback,
		OGCover:         f.OGCover || cfg.OGCover,
		NoCover:         f.NoCover || cfg.NoCover,
		FolderFormat:    folderFmt,
		TrackFormat:     trackFmt,
		SmartDiscog:     f.SmartDiscog || cfg.SmartDiscog,
		NoDB:            f.NoDB || cfg.NoDatabase,
		DBPath:          cfg.DBPath,
		Workers:         f.Workers,
	}
	return downloader.New(client, opts, ctx), nil
}

func searchByType(client *api.Client, itemType, query string, limit int) ([]string, error) {
	return downloader.SearchURLs(client, itemType, query, limit)
}
