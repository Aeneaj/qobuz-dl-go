package downloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// downloadWithProgress downloads rawURL to dest, updating bar as bytes arrive.
// It uses the Downloader's shared httpClient and respects the context for
// cancellation (e.g. Ctrl+C).
const maxDownloadRetries = 5

func (d *Downloader) downloadWithProgress(ctx context.Context, rawURL, dest string, bar ProgressBar) error {
	var (
		totalSize   int64 = -1 // full file size, resolved from Content-Length or Content-Range
		barCredited int64      // bytes already reflected in the bar across all attempts
	)

	for attempt := 0; attempt < maxDownloadRetries; attempt++ {
		if err := waitBeforeRetry(ctx, attempt); err != nil {
			return err
		}

		// Bytes already saved from a previous attempt.
		offset := currentOffset(dest)

		req, err := buildRangeRequest(ctx, rawURL, offset)
		if err != nil {
			return err
		}

		resp, err := d.httpClient.Do(req)
		if err != nil {
			if isContextError(err) {
				return err
			}
			continue // network error before response — retry
		}

		// Server ignored Range and sent full file — discard partial data and restart.
		// Must continue so we make a fresh request with the original (non-closed) body.
		if offset > 0 && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			os.Remove(dest)
			barCredited = 0
			continue
		}

		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
			resp.Body.Close()
			return fmt.Errorf("unexpected HTTP status %d", resp.StatusCode)
		}

		// Resolve total file size once.
		if totalSize <= 0 {
			totalSize = resolveTotalSize(resp, offset)
		}

		// Set bar total for display only — do NOT trigger auto-completion here.
		// The bar is explicitly completed after io.Copy returns to avoid mpb
		// closing its operateState channel while ProxyReader is still active.
		if bar != nil && totalSize > 0 {
			bar.SetTotal(totalSize, false)
		}

		// Fast-forward bar for bytes already on disk from prior attempts.
		if bar != nil && offset > barCredited {
			bar.IncrBy(int(offset - barCredited))
			barCredited = offset
		}

		f, err := openOutput(dest, offset)
		if err != nil {
			resp.Body.Close()
			return err
		}

		n, copyErr := copyAndCommit(f, resp.Body, bar)

		// Always close all handles before deciding what to do next.
		resp.Body.Close()
		f.Close()

		barCredited += n
		written := offset + n

		if copyErr == nil {
			if totalSize > 0 && written != totalSize {
				return fmt.Errorf("incomplete download: got %d of %d bytes", written, totalSize)
			}
			finalizeBar(bar, totalSize, written)
			return nil
		}

		if isContextError(copyErr) {
			return copyErr
		}
		if !isRecoverableErr(copyErr) {
			return copyErr
		}
		// Recoverable (EOF / network drop) — next iteration resumes via Range header.
	}

	return fmt.Errorf("download failed after %d attempts", maxDownloadRetries)
}

// waitBeforeRetry sleeps with exponential backoff (1s, 2s, 4s, 8s) before a
// retry attempt. attempt 0 is the first try and returns immediately. Returns
// ctx.Err() if the context is cancelled while sleeping.
func waitBeforeRetry(ctx context.Context, attempt int) error {
	if attempt == 0 {
		return nil
	}
	delay := time.Duration(1<<(attempt-1)) * time.Second
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(delay):
		return nil
	}
}

// currentOffset reports the size of an existing partial file at dest, or 0 if
// the file does not exist or cannot be stat'd. Used to seed the Range header
// when resuming a download.
func currentOffset(dest string) int64 {
	if fi, err := os.Stat(dest); err == nil {
		return fi.Size()
	}
	return 0
}

// buildRangeRequest builds a GET request for rawURL, adding a "Range:
// bytes=offset-" header when offset > 0 to resume a partial download.
func buildRangeRequest(ctx context.Context, rawURL string, offset int64) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	return req, nil
}

// resolveTotalSize derives the full file size from a response. It prefers the
// "Content-Range: bytes N-M/TOTAL" trailer on a 206 reply, falls back to
// offset + Content-Length, and returns -1 when neither yields a positive
// value.
func resolveTotalSize(resp *http.Response, offset int64) int64 {
	if resp.StatusCode == http.StatusPartialContent {
		if cr := resp.Header.Get("Content-Range"); cr != "" {
			if idx := strings.LastIndex(cr, "/"); idx >= 0 {
				if t, err := strconv.ParseInt(cr[idx+1:], 10, 64); err == nil && t > 0 {
					return t
				}
			}
		}
	}
	if resp.ContentLength > 0 {
		return offset + resp.ContentLength
	}
	return -1
}

// openOutput opens the destination file for writing. When offset == 0 it
// creates a fresh file (truncating any leftover); otherwise it opens the
// existing file in append-only mode to resume.
func openOutput(dest string, offset int64) (*os.File, error) {
	if offset == 0 {
		return os.Create(dest)
	}
	return os.OpenFile(dest, os.O_APPEND|os.O_WRONLY, 0644)
}

// copyAndCommit pipes body into f, optionally wrapping body in bar's
// ProxyReader so the progress bar advances live as bytes arrive. The proxy
// reader is closed before returning so its goroutine settles; the caller
// retains ownership of body and f.
func copyAndCommit(f *os.File, body io.Reader, bar ProgressBar) (int64, error) {
	reader := body
	var pr io.ReadCloser
	if bar != nil {
		pr = bar.ProxyReader(body)
		reader = pr
	}
	n, err := io.Copy(f, reader)
	if pr != nil {
		pr.Close()
	}
	return n, err
}

// finalizeBar marks bar complete after io.Copy has fully returned. Doing it
// here (and not during SetTotal) prevents mpb from closing its internal
// operateState channel while ProxyReader is still active.
func finalizeBar(bar ProgressBar, totalSize, written int64) {
	if bar == nil {
		return
	}
	completedAt := totalSize
	if completedAt <= 0 {
		completedAt = written
	}
	bar.SetTotal(completedAt, true)
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func isRecoverableErr(err error) bool {
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

// downloadExtra fetches a supplementary file (cover art, booklet PDF).
// Uses the shared httpClient and context; logs errors instead of silently
// ignoring them.
func (d *Downloader) downloadExtra(ctx context.Context, rawURL, dest string) {
	if _, err := os.Stat(dest); err == nil {
		fmt.Fprintf(d.termOut(), "\033[90m%s already downloaded\033[0m\n", filepath.Base(dest))
		return
	}
	fmt.Fprintf(d.termOut(), "\033[90mDownloading %s...\033[0m\n", filepath.Base(dest))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		fmt.Fprintf(d.termOut(), "\033[31mCould not create request for %s: %v\033[0m\n", filepath.Base(dest), err)
		return
	}
	resp, err := d.httpClient.Do(req)
	if err != nil {
		fmt.Fprintf(d.termOut(), "\033[31mCould not download %s: %v\033[0m\n", filepath.Base(dest), err)
		return
	}
	defer resp.Body.Close()
	f, err := os.Create(dest)
	if err != nil {
		fmt.Fprintf(d.termOut(), "\033[31mCould not create file %s: %v\033[0m\n", filepath.Base(dest), err)
		return
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		fmt.Fprintf(d.termOut(), "\033[31mError writing %s: %v\033[0m\n", filepath.Base(dest), err)
	}
}
