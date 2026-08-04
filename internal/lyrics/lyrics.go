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

// Result summarises a lyrics run.
type Result struct {
	Total       int
	Fetched     int
	Skipped     int
	Warnings    []string
	Interrupted bool
}

// Step reports progress. It is called once with done == 0 before the first
// file — that call carries the total and nothing else — and then once per
// file, before it is fetched.
type Step func(done, total int, title, artist string)

// FetchAll fetches lyrics for dir and draws nothing: progress goes to step
// (which may be nil) and everything else comes back in Result. This is the
// entry point for callers that own the screen, such as the TUI. Run wraps it
// with mpb bars and terminal output.
func FetchAll(ctx context.Context, dir string, step Step) (Result, error) {
	return fetchAll(ctx, dir, NewClient(), step)
}

func fetchAll(ctx context.Context, dir string, client *Client, step Step) (Result, error) {
	var res Result

	files, err := scanAudioFiles(ctx, dir)
	if err != nil {
		if ctx.Err() != nil {
			res.Interrupted = true
			return res, nil
		}
		return res, fmt.Errorf("scan: %w", err)
	}

	res.Total = len(files)
	if step != nil {
		step(0, res.Total, "", "")
	}
	if res.Total == 0 {
		return res, nil
	}

loop:
	for i, f := range files {
		if ctx.Err() != nil {
			res.Interrupted = true
			break
		}
		if step != nil {
			step(i+1, res.Total, f.Title, f.Artist)
		}

		lrcPath := lrcPathFor(f.Path)

		// Skip tracks that already have a .lrc file.
		if _, err := os.Stat(lrcPath); err == nil {
			res.Skipped++
			continue
		}

		content, fetchErr := client.FetchWithRetry(ctx, f)
		switch {
		case fetchErr != nil:
			res.Warnings = append(res.Warnings,
				fmt.Sprintf("\033[31mERROR  %s: %v\033[0m", filepath.Base(f.Path), fetchErr))
		case content == "":
			res.Warnings = append(res.Warnings,
				fmt.Sprintf("\033[33mWARN   not found — %s — %s\033[0m", f.Title, f.Artist))
		default:
			if err := os.WriteFile(lrcPath, []byte(content), 0644); err != nil {
				res.Warnings = append(res.Warnings,
					fmt.Sprintf("\033[31mERROR  write %s: %v\033[0m", filepath.Base(lrcPath), err))
			} else {
				res.Fetched++
			}
		}

		select {
		case <-time.After(client.StepDelay):
		case <-ctx.Done():
			res.Interrupted = true
			break loop
		}
	}

	return res, nil
}

func runWithClient(ctx context.Context, dir string, client *Client) error {
	fmt.Printf("\033[33mScanning %s...\033[0m\n", dir)

	// currentLabel is read by mpb's refresh goroutine — use atomic to avoid races.
	var currentLabel atomic.Value
	currentLabel.Store(buildLabel(0, 0, "", ""))

	p := mpb.NewWithContext(ctx, mpb.WithRefreshRate(150*time.Millisecond))
	var bar *mpb.Bar

	// The bar cannot exist before the scan finishes, since its total is the
	// file count — so the done == 0 call is what creates it.
	step := func(done, total int, title, artist string) {
		if done == 0 {
			if total == 0 {
				return
			}
			fmt.Printf("\033[33mFound %d audio file(s). Fetching lyrics...\033[0m\n\n", total)
			bar = p.New(int64(total),
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
			return
		}
		currentLabel.Store(buildLabel(done, total, title, artist))
		bar.SetCurrent(int64(done))
	}

	res, err := fetchAll(ctx, dir, client, step)
	if err != nil {
		return err
	}
	if bar != nil {
		if res.Interrupted {
			bar.Abort(false)
		} else {
			bar.SetCurrent(int64(res.Total))
		}
	}
	p.Wait()

	if res.Total == 0 && !res.Interrupted {
		fmt.Println("\033[33mNo audio files found.\033[0m")
		return nil
	}

	if len(res.Warnings) > 0 {
		fmt.Println()
		for _, w := range res.Warnings {
			fmt.Println(w)
		}
	}

	if res.Interrupted {
		fmt.Printf("\n\033[33m⚠ Interrupted — fetched: %d  skipped: %d\033[0m\n", res.Fetched, res.Skipped)
		return nil
	}

	fmt.Printf("\n\033[32m✓ Done — fetched: %d  skipped: %d  not found/errors: %d\033[0m\n",
		res.Fetched, res.Skipped, len(res.Warnings))
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
