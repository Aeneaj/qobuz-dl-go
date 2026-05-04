package lyrics

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
)

const labelWidth = 55

// Run fetches .lrc files for every .flac and .mp3 found recursively under dir.
// Files that already have a matching .lrc are silently skipped.
// Requests are sent sequentially with a 500 ms pause to respect LRCLIB rate limits.
func Run(ctx context.Context, dir string) error {
	return runWithClient(ctx, dir, NewClient())
}

func runWithClient(ctx context.Context, dir string, client *Client) error {
	fmt.Printf("\033[33mScanning %s...\033[0m\n", dir)
	files, err := scanAudioFiles(ctx, dir)
	if err != nil {
		if ctx.Err() != nil {
			fmt.Println("\n\033[33mInterrupted.\033[0m")
			return nil
		}
		return fmt.Errorf("scan: %w", err)
	}
	if len(files) == 0 {
		fmt.Println("\033[33mNo audio files found.\033[0m")
		return nil
	}

	total := len(files)
	fmt.Printf("\033[33mFound %d audio file(s). Fetching lyrics...\033[0m\n\n", total)

	// currentLabel is read by mpb's refresh goroutine — use atomic to avoid races.
	var currentLabel atomic.Value
	currentLabel.Store(buildLabel(0, total, "", ""))

	p := mpb.NewWithContext(ctx, mpb.WithRefreshRate(150*time.Millisecond))
	bar := p.New(int64(total),
		mpb.BarStyle().Lbound("╢").Filler("█").Tip("█").Padding("░").Rbound("╟"),
		mpb.PrependDecorators(
			decor.Any(func(_ decor.Statistics) string {
				v, _ := currentLabel.Load().(string)
				return v
			}),
		),
		mpb.AppendDecorators(
			decor.OnComplete(decor.Name(""), " \033[32m✓\033[0m"),
		),
	)

	var fetched, skipped int
	var warnings []string
	interrupted := false

loop:
	for i, f := range files {
		if ctx.Err() != nil {
			bar.Abort(false)
			interrupted = true
			break
		}

		currentLabel.Store(buildLabel(i+1, total, f.Title, f.Artist))

		lrcPath := lrcPathFor(f.Path)

		// Skip tracks that already have a .lrc file.
		if _, err := os.Stat(lrcPath); err == nil {
			skipped++
			bar.Increment()
			continue
		}

		content, fetchErr := client.FetchWithRetry(f)
		switch {
		case fetchErr != nil:
			warnings = append(warnings,
				fmt.Sprintf("\033[31mERROR  %s: %v\033[0m", filepath.Base(f.Path), fetchErr))
		case content == "":
			warnings = append(warnings,
				fmt.Sprintf("\033[33mWARN   not found — %s — %s\033[0m", f.Title, f.Artist))
		default:
			if err := os.WriteFile(lrcPath, []byte(content), 0644); err != nil {
				warnings = append(warnings,
					fmt.Sprintf("\033[31mERROR  write %s: %v\033[0m", filepath.Base(lrcPath), err))
			} else {
				fetched++
			}
		}

		bar.Increment()
		select {
		case <-time.After(client.StepDelay):
		case <-ctx.Done():
			bar.Abort(false)
			interrupted = true
			break loop
		}
	}

	p.Wait()

	if len(warnings) > 0 {
		fmt.Println()
		for _, w := range warnings {
			fmt.Println(w)
		}
	}

	if interrupted {
		fmt.Printf("\n\033[33m⚠ Interrupted — fetched: %d  skipped: %d\033[0m\n", fetched, skipped)
		return nil
	}

	notFound := len(warnings)
	fmt.Printf("\n\033[32m✓ Done — fetched: %d  skipped: %d  not found/errors: %d\033[0m\n",
		fetched, skipped, notFound)
	return nil
}

// scanAudioFiles walks dir recursively and returns AudioInfo for every
// .flac and .mp3 file. Read errors are reported as warnings and skipped.
func scanAudioFiles(ctx context.Context, dir string) ([]AudioInfo, error) {
	var files []AudioInfo
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".flac" && ext != ".mp3" {
			return nil
		}
		info, readErr := ReadAudio(path)
		if readErr != nil {
			fmt.Printf("\033[33mWarning: cannot read %s: %v\033[0m\n",
				filepath.Base(path), readErr)
			return nil
		}
		if info.Title == "" {
			info.Title = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		}
		files = append(files, info)
		return nil
	})
	return files, err
}

func lrcPathFor(audioPath string) string {
	return audioPath[:len(audioPath)-len(filepath.Ext(audioPath))] + ".lrc"
}

// buildLabel returns a fixed-width string "[N/M] Title — Artist" for the bar.
func buildLabel(current, total int, title, artist string) string {
	counter := fmt.Sprintf("[%d/%d] ", current, total)
	desc := title
	if artist != "" {
		desc = title + " — " + artist
	}
	counterRunes := []rune(counter)
	maxDesc := labelWidth - len(counterRunes)
	if maxDesc < 8 {
		maxDesc = 8
	}
	descRunes := []rune(desc)
	if len(descRunes) > maxDesc {
		desc = string(descRunes[:maxDesc-1]) + "…"
	} else {
		desc += strings.Repeat(" ", maxDesc-len(descRunes))
	}
	return string(counterRunes) + desc
}
