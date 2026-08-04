package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/Aeneaj/qobuz-dl-go/internal/api"
	"github.com/Aeneaj/qobuz-dl-go/internal/config"
	"github.com/Aeneaj/qobuz-dl-go/internal/downloader"
)

// Wiring between the CLI surface and the downloader: flag registration,
// first-run config bootstrap, and the flag > config > default precedence
// that produces downloader.Options.

func loadOrInitConfig(ctx context.Context, skipCredentials bool) (*config.Config, error) {
	cfgDir := config.ConfigDir()
	cfgFile := cfgDir + "/config.ini"
	if _, err := os.Stat(cfgFile); os.IsNotExist(err) {
		if err := os.MkdirAll(cfgDir, 0755); err != nil {
			return nil, err
		}
		fmt.Println("\033[33mFirst run: setting up config...\033[0m")
		if skipCredentials {
			if err := config.InitConfig(ctx); err != nil {
				return nil, err
			}
		} else {
			if err := config.Reset(ctx); err != nil {
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
	TUI          bool
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
	fs.BoolVar(&f.TUI, "tui", false, "")
	return f
}

func initDownloader(ctx context.Context, f *cliFlags) (*downloader.Downloader, error) {
	cfg, err := loadOrInitConfig(ctx, false)
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
	// Workers hierarchy: CLI flag > config > downloader default (applied in New()).
	workers := f.Workers
	if workers == 0 {
		workers = cfg.Workers
	}

	client := api.New(cfg.AppID, cfg.Secrets)

	if cfg.UserID == "" || cfg.UserAuthToken == "" {
		return nil, fmt.Errorf("no credentials found — run 'qobuz-dl oauth' to log in, or 'qobuz-dl --reset' to set up manually")
	}
	fmt.Println("\033[33mLogging in...\033[0m")
	if err := client.AuthWithToken(ctx, cfg.UserID, cfg.UserAuthToken); err != nil {
		return nil, err
	}

	if err := client.CfgSetup(ctx); err != nil {
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
		Workers:         workers,
	}
	return downloader.New(client, opts)
}
