package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Aeneaj/qobuz-dl-go/internal/config"
	"github.com/Aeneaj/qobuz-dl-go/internal/lyrics"
)

func runLyrics(args []string) {
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

	resolved, err := resolveScanDir(scanDir)
	if err != nil {
		fatalf("lyrics: %v", err)
	}

	if err := lyrics.Run(resolved); err != nil {
		fatalf("lyrics: %v", err)
	}
}

// resolveScanDir expands ~ and returns an absolute path.
// Unlike config.ResolveDir it does NOT create the directory — the user must
// point lyrics at an existing music library.
func resolveScanDir(dir string) (string, error) {
	if strings.HasPrefix(dir, "~/") || dir == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expand ~: %w", err)
		}
		dir = filepath.Join(home, dir[1:])
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", dir, err)
	}
	if info, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("directory not found: %q", abs)
	} else if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", abs)
	}
	return abs, nil
}
