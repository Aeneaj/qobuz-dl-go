package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/Aeneaj/qobuz-dl-go/internal/config"
	"github.com/Aeneaj/qobuz-dl-go/internal/lyrics"
)

func runLyrics(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("lyrics", flag.ExitOnError)
	dir := fs.String("d", "", "directory to scan")
	fs.Usage = func() {
		fmt.Print(`Usage: qobuz-dl lyrics [options] [path]

  Fetch synchronized .lrc files from LRCLIB for all FLAC and MP3 files found
  recursively under the given path. Files that already have a matching .lrc
  are skipped. Requests are rate-limited to 2/s to respect the LRCLIB API.

Arguments:
  path          Directory to scan (default: configured download_dir)

Options:
  -d <dir>      Directory to scan (alternative to positional argument)
`)
	}
	fs.Parse(args)

	// Resolution order: -d flag > positional arg > config download_dir > default.
	scanDir := *dir
	if scanDir == "" && fs.NArg() > 0 {
		scanDir = fs.Arg(0)
	}
	if scanDir == "" {
		if cfg, err := config.Load(); err == nil && cfg.DownloadDir != "" {
			scanDir = cfg.DownloadDir
		}
	}
	if scanDir == "" {
		scanDir = "./qobuz-downloader"
	}

	// create=false: lyrics scans an existing library, it never makes one.
	resolved, err := config.ResolveDir(scanDir, false)
	if err != nil {
		fatalf("lyrics: %v", err)
	}

	if err := lyrics.Run(ctx, resolved); err != nil {
		fatalf("lyrics: %v", err)
	}
}
