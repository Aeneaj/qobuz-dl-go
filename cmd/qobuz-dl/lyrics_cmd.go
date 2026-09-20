package main

import (
	"context"

	"github.com/Aeneaj/qobuz-dl-go/internal/config"
	"github.com/Aeneaj/qobuz-dl-go/internal/lyrics"
)

// dir is the -d value from the top-level FlagSet. Options are parsed wherever
// they appear now, so "lyrics -d X" is consumed up there and this command needs
// no FlagSet of its own — its former one only duplicated -d and carried a usage
// block that -h could no longer reach.
func runLyrics(ctx context.Context, args []string, dir string) {
	// Resolution order: -d flag > positional arg > config download_dir > default.
	scanDir := dir
	if scanDir == "" && len(args) > 0 {
		scanDir = args[0]
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
