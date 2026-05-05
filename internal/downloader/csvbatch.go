package downloader

import (
	"bufio"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strings"
)

// TrackRequest represents a single track parsed from a TuneMyMusic CSV export.
type TrackRequest struct {
	Title  string
	Artist string
	Album  string
	ISRC   string
	Query  string // search query sent to Qobuz
	RowNum int
	Raw    string // original "Track name" field value
}

// ParseCSV reads a TuneMyMusic CSV file and returns a slice of TrackRequests.
// Expected header: Track name,Artist name,Album,Playlist name,Type,ISRC
//
// Handles the common TuneMyMusic anomaly where Artist name is empty and
// Track name contains "Artist - Title" format.
func ParseCSV(path string) ([]TrackRequest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open csv: %w", err)
	}
	defer f.Close()

	// Strip UTF-8 BOM (\xef\xbb\xbf) emitted by TuneMyMusic and Windows editors.
	br := bufio.NewReader(f)
	if bom, err := br.Peek(3); err == nil && bom[0] == 0xEF && bom[1] == 0xBB && bom[2] == 0xBF {
		br.Discard(3)
	}

	r := csv.NewReader(br)
	r.FieldsPerRecord = -1 // tolerate variable column counts
	r.LazyQuotes = true

	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}

	colIdx := make(map[string]int, len(header))
	for i, h := range header {
		colIdx[strings.TrimSpace(h)] = i
	}

	// Warn if the critical column is missing — helps diagnose future format changes.
	if _, ok := colIdx["Track name"]; !ok {
		fmt.Fprintf(os.Stderr, "\033[33mWarning: 'Track name' column not found in header. Got: %v\033[0m\n", header)
	}

	get := func(record []string, name string) string {
		i, ok := colIdx[name]
		if !ok || i >= len(record) {
			return ""
		}
		return strings.TrimSpace(record[i])
	}

	var tracks []TrackRequest
	rowNum := 1
	for {
		record, err := r.Read()
		if err == io.EOF {
			break
		}
		rowNum++
		if err != nil {
			fmt.Fprintf(os.Stderr, "\033[33m  [row %d] skipped: parse error: %v\033[0m\n", rowNum, err)
			continue
		}

		raw := get(record, "Track name")
		if raw == "" {
			fmt.Fprintf(os.Stderr, "\033[33m  [row %d] skipped: empty Track name\033[0m\n", rowNum)
			continue
		}

		artist := get(record, "Artist name")
		title := raw
		album := get(record, "Album")
		isrc := get(record, "ISRC")

		// TuneMyMusic anomaly: artist empty → infer from "Artist - Title" format.
		if artist == "" {
			if parts := strings.SplitN(raw, " - ", 2); len(parts) == 2 {
				artist = strings.TrimSpace(parts[0])
				title = strings.TrimSpace(parts[1])
			}
		}

		var query string
		if artist != "" {
			query = artist + " " + title
		} else {
			query = title
		}

		tracks = append(tracks, TrackRequest{
			Title:  title,
			Artist: artist,
			Album:  album,
			ISRC:   isrc,
			Query:  query,
			RowNum: rowNum,
			Raw:    raw,
		})
	}
	return tracks, nil
}

type failedEntry struct {
	Track  TrackRequest
	Reason string
}

// DownloadCSV parses the CSV at csvPath and downloads each track via Qobuz.
// If failedPath is non-empty, writes a CSV report of skipped/failed tracks there.
func (d *Downloader) DownloadCSV(ctx context.Context, csvPath, failedPath string) {
	tracks, err := ParseCSV(csvPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\033[31mCSV parse error: %v\033[0m\n", err)
		return
	}
	if len(tracks) == 0 {
		fmt.Println("No tracks found in CSV.")
		return
	}

	fmt.Printf("\033[33mCSV loaded: %d tracks to process\033[0m\n\n", len(tracks))

	total := len(tracks)
	downloaded := 0
	var failures []failedEntry
	dir := d.Opts.Directory

	for i, t := range tracks {
		if err := ctx.Err(); err != nil {
			fmt.Printf("\033[33m\nInterrupted at row %d/%d.\033[0m\n", i, total)
			break
		}
		fmt.Printf("\033[33m[%d/%d] %s\033[0m\n", i+1, total, t.Query)

		trackID, err := d.searchFirstTrackID(ctx, t.Query)
		if err != nil {
			fmt.Printf("  \033[31m✗ Search error: %v\033[0m\n", err)
			failures = append(failures, failedEntry{t, fmt.Sprintf("search error: %v", err)})
			continue
		}
		if trackID == "" {
			fmt.Printf("  \033[31m✗ Not found on Qobuz\033[0m\n")
			failures = append(failures, failedEntry{t, "not found"})
			continue
		}

		fmt.Printf("  \033[32m→ id=%s, downloading...\033[0m\n", trackID)
		if err := d.downloadTrackByID(ctx, trackID, dir); err != nil {
			fmt.Printf("  \033[31m✗ Download error: %v\033[0m\n", err)
			failures = append(failures, failedEntry{t, fmt.Sprintf("download error: %v", err)})
			continue
		}
		downloaded++
	}

	printBatchSummary(total, downloaded, failures)

	if failedPath != "" && len(failures) > 0 {
		if err := writeFailedCSV(failedPath, failures); err != nil {
			fmt.Fprintf(os.Stderr, "  \033[31mCould not write failed report: %v\033[0m\n", err)
		} else {
			fmt.Printf("  Failed tracks saved to: %s\n", failedPath)
		}
	}
}

func printBatchSummary(total, downloaded int, failures []failedEntry) {
	notFound, errCount := 0, 0
	for _, f := range failures {
		if f.Reason == "not found" {
			notFound++
		} else {
			errCount++
		}
	}
	fmt.Printf("\n\033[1m=== CSV Batch Summary ===\033[0m\n")
	fmt.Printf("  Total processed: %d\n", total)
	fmt.Printf("  Downloaded:      \033[32m%d\033[0m\n", downloaded)
	fmt.Printf("  Not found:       \033[33m%d\033[0m\n", notFound)
	fmt.Printf("  Errors:          \033[31m%d\033[0m\n", errCount)
}

func writeFailedCSV(path string, entries []failedEntry) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	if err := w.Write([]string{"row", "artist", "title", "query", "reason"}); err != nil {
		return err
	}
	for _, e := range entries {
		if err := w.Write([]string{
			fmt.Sprintf("%d", e.Track.RowNum),
			e.Track.Artist,
			e.Track.Title,
			e.Track.Query,
			e.Reason,
		}); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}
