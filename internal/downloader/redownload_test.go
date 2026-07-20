package downloader

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// finalTrackPath must produce the same path downloadAndTag writes to, so the
// on-disk existence check can locate an already-downloaded file.
func TestFinalTrackPath(t *testing.T) {
	trackMeta := map[string]interface{}{
		"track_number": float64(3),
		"title":        "Song",
	}
	albumMeta := map[string]interface{}{
		"artist": map[string]interface{}{"name": "Artist"},
	}

	t.Run("FLAC extension for lossless", func(t *testing.T) {
		dir := t.TempDir()
		got, err := finalTrackPath(dir, trackMeta, albumMeta, "{tracknumber} - {tracktitle}", false)
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(dir, "03 - Song") + ".flac"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("MP3 extension when isMP3", func(t *testing.T) {
		dir := t.TempDir()
		got, err := finalTrackPath(dir, trackMeta, albumMeta, "{tracknumber} - {tracktitle}", true)
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Ext(got) != ".mp3" {
			t.Errorf("extension = %q, want .mp3", filepath.Ext(got))
		}
	})

	t.Run("subfolder in track_format is respected", func(t *testing.T) {
		// A user-set track_format like "{albumartist}/{tracknumber} - {tracktitle}"
		// must produce a nested path — not a flattened name.
		dir := t.TempDir()
		got, err := finalTrackPath(dir, trackMeta, albumMeta, "{albumartist}/{tracknumber} - {tracktitle}", false)
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(dir, "Artist", "03 - Song") + ".flac"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("path traversal in track_format is rejected", func(t *testing.T) {
		// An admin config with "../" in the template must be blocked so the
		// caller cannot land the file outside the download tree.
		dir := t.TempDir()
		_, err := finalTrackPath(dir, trackMeta, albumMeta, "../{tracktitle}", false)
		if err == nil {
			t.Fatal("expected traversal to be rejected, got nil")
		}
		if !strings.Contains(err.Error(), "resolve track path") {
			t.Errorf("error should mention path resolution; got: %v", err)
		}
	})
}

// alreadyHave is the heart of the fix: a track recorded in the DB must only be
// skipped when its file is still on disk. Deleting the file must cause a
// re-download instead of a silent skip.
func TestAlreadyHave(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "present.flac")
	if err := os.WriteFile(existing, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "deleted.flac")

	db, err := openDB(filepath.Join(dir, "downloads.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.add("in-db"); err != nil {
		t.Fatal(err)
	}
	d := &Downloader{db: db}

	cases := []struct {
		name      string
		trackID   string
		finalFile string
		want      bool
	}{
		{"in DB and on disk → skip", "in-db", existing, true},
		{"in DB but file deleted → re-download (the bug fix)", "in-db", missing, false},
		{"not in DB → download regardless of file", "not-in-db", existing, false},
		{"not in DB and no file → download", "not-in-db", missing, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := d.alreadyHave(c.trackID, c.finalFile); got != c.want {
				t.Errorf("alreadyHave(%q) = %v, want %v", c.trackID, got, c.want)
			}
		})
	}

	t.Run("nil DB is never a skip", func(t *testing.T) {
		dNoDB := &Downloader{}
		if dNoDB.alreadyHave("in-db", existing) {
			t.Error("alreadyHave with nil db should always be false")
		}
	})
}
