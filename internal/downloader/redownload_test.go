package downloader

import (
	"os"
	"path/filepath"
	"testing"
)

// finalTrackPath must produce the same path downloadAndTag writes to, so the
// on-disk existence check can find an already-downloaded file.
func TestFinalTrackPath(t *testing.T) {
	dir := "/music/album"
	trackMeta := map[string]interface{}{
		"track_number": float64(3),
		"title":        "Song",
	}
	albumMeta := map[string]interface{}{
		"artist": map[string]interface{}{"name": "Artist"},
	}

	got := finalTrackPath(dir, trackMeta, albumMeta, "{tracknumber} - {tracktitle}", false)
	want := filepath.Join(dir, "03 - Song") + ".flac"
	if got != want {
		t.Errorf("finalTrackPath = %q, want %q", got, want)
	}

	gotMP3 := finalTrackPath(dir, trackMeta, albumMeta, "{tracknumber} - {tracktitle}", true)
	if filepath.Ext(gotMP3) != ".mp3" {
		t.Errorf("finalTrackPath MP3 ext = %q, want .mp3", filepath.Ext(gotMP3))
	}
}

// alreadyHave is the heart of the bug fix: a track recorded in the DB must only
// be skipped when its file is still present on disk. If the user deleted the
// album, the file is gone and the track must be re-downloaded.
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
	db.add("in-db")
	d := &Downloader{db: db}

	cases := []struct {
		name      string
		trackID   string
		finalFile string
		want      bool
	}{
		{"in DB and on disk -> skip", "in-db", existing, true},
		{"in DB but deleted -> re-download", "in-db", missing, false},
		{"not in DB -> download", "not-in-db", existing, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := d.alreadyHave(c.trackID, c.finalFile); got != c.want {
				t.Errorf("alreadyHave(%q, exists=%v) = %v, want %v", c.trackID, c.finalFile == existing, got, c.want)
			}
		})
	}

	// With no DB, nothing is ever considered already-downloaded.
	dNoDB := &Downloader{}
	if dNoDB.alreadyHave("in-db", existing) {
		t.Error("alreadyHave with nil db should always be false")
	}
}
