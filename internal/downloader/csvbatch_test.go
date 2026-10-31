package downloader

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCSV(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "playlist.csv")
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestParseCSVWarningsGoToWriter pins where the per-row warnings land. They
// used to go to os.Stderr, which paints over the alt screen when the TUI runs
// an import — the writer is the whole point of the parameter, so a warning
// that skips it is the failure this catches.
func TestParseCSVWarningsGoToWriter(t *testing.T) {
	path := writeCSV(t, "Track name,Artist name,Album\n"+
		"Karma Police,Radiohead,OK Computer\n"+
		",Nobody,Empty Title\n"+ // skipped: empty Track name
		"Paranoid Android,Radiohead,OK Computer\n")

	var buf bytes.Buffer
	tracks, err := ParseCSV(&buf, path)
	if err != nil {
		t.Fatalf("ParseCSV: %v", err)
	}

	if len(tracks) != 2 {
		t.Errorf("parsed %d tracks, want 2 (the empty-title row is skipped)", len(tracks))
	}
	if !strings.Contains(buf.String(), "empty Track name") {
		t.Errorf("the skipped-row warning did not reach the writer, got: %q", buf.String())
	}
}

// A missing "Track name" column is the anomaly worth shouting about, and its
// warning has the same destination requirement.
func TestParseCSVMissingColumnWarnsToWriter(t *testing.T) {
	path := writeCSV(t, "Song,Artist name\nKarma Police,Radiohead\n")

	var buf bytes.Buffer
	if _, err := ParseCSV(&buf, path); err != nil {
		t.Fatalf("ParseCSV: %v", err)
	}
	if !strings.Contains(buf.String(), "'Track name' column not found") {
		t.Errorf("the missing-column warning did not reach the writer, got: %q", buf.String())
	}
}

// TestDownloadCSVReturnsParseError covers the other half: an unreadable CSV is
// reported to the caller instead of printed. Under the TUI the caller is the
// shell, which puts it on its own status line; printing it would land on the
// alt screen.
func TestDownloadCSVReturnsParseError(t *testing.T) {
	d := &Downloader{}
	missing := filepath.Join(t.TempDir(), "does-not-exist.csv")

	_, err := d.DownloadCSV(context.Background(), missing, "")
	if err == nil {
		t.Fatal("DownloadCSV on a missing file returned nil, want an error")
	}
	if !strings.Contains(err.Error(), "CSV parse error") {
		t.Errorf("error = %q, want it to name the parse failure", err)
	}
}

// An empty but valid CSV is not an error — there is simply nothing to do.
func TestDownloadCSVEmptyIsNotAnError(t *testing.T) {
	path := writeCSV(t, "Track name,Artist name\n")

	if _, err := (&Downloader{}).DownloadCSV(context.Background(), path, ""); err != nil {
		t.Errorf("DownloadCSV on an empty CSV = %v, want nil", err)
	}
}

// The printed summary never reaches the TUI, so what failed has to come back:
// an import where nothing was found used to end in "CSV import finished".
func TestDownloadCSVReportsFailures(t *testing.T) {
	csv := writeCSV(t, "Track name,Artist name,Album\nKarma Police,Radiohead,OK Computer\n")
	d, _ := newTestDownloader(t, newFakeQobuz(t, nil), nil)

	t.Run("failures are counted and listed", func(t *testing.T) {
		report := filepath.Join(t.TempDir(), "failed.csv")
		failed, err := d.DownloadCSV(context.Background(), csv, report)
		if err != nil || failed != 1 {
			t.Fatalf("DownloadCSV = %d, %v; want 1, nil", failed, err)
		}
		if _, err := os.Stat(report); err != nil {
			t.Error(err)
		}
	})

	t.Run("a report that cannot be written is an error", func(t *testing.T) {
		report := filepath.Join(t.TempDir(), "no", "such", "dir", "failed.csv")
		if _, err := d.DownloadCSV(context.Background(), csv, report); err == nil {
			t.Error("the report was not written and nothing said so")
		}
	})
}
