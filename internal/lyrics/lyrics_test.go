package lyrics

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- buildLabel tests ---------------------------------------------------

func TestBuildLabel_Format(t *testing.T) {
	got := buildLabel(5, 50, "Song Title", "Artist Name")
	if !strings.HasPrefix(got, "[5/50] Song Title") {
		t.Errorf("unexpected prefix: %q", got)
	}
	if !strings.Contains(got, "Song Title — Artist Name") {
		t.Errorf("missing title/artist separator: %q", got)
	}
}

func TestBuildLabel_FixedWidth(t *testing.T) {
	cases := []struct{ cur, total int }{
		{1, 10},
		{5, 50},
		{100, 9999},
		{1, 1},
	}
	for _, c := range cases {
		got := buildLabel(c.cur, c.total, "My Song", "My Artist")
		if w := len([]rune(got)); w != labelWidth {
			t.Errorf("buildLabel(%d,%d) width = %d, want %d: %q",
				c.cur, c.total, w, labelWidth, got)
		}
	}
}

func TestBuildLabel_TruncatesLongTitle(t *testing.T) {
	long := strings.Repeat("X", 200)
	got := buildLabel(1, 1, long, "")
	if w := len([]rune(got)); w != labelWidth {
		t.Errorf("truncated width = %d, want %d", w, labelWidth)
	}
	if !strings.HasSuffix(strings.TrimRight(got, " "), "…") {
		t.Errorf("truncated label should end with ellipsis: %q", got)
	}
}

func TestBuildLabel_EmptyTitleAndArtist(t *testing.T) {
	got := buildLabel(1, 10, "", "")
	if w := len([]rune(got)); w != labelWidth {
		t.Errorf("empty-title width = %d, want %d", w, labelWidth)
	}
}

// ---- lrcPathFor tests ---------------------------------------------------

func TestLrcPathFor(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/music/Artist/Album/01 - Song.flac", "/music/Artist/Album/01 - Song.lrc"},
		{"/music/track.mp3", "/music/track.lrc"},
		{"/music/TRACK.FLAC", "/music/TRACK.lrc"},
	}
	for _, c := range cases {
		if got := lrcPathFor(c.in); got != c.want {
			t.Errorf("lrcPathFor(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---- scanAudioFiles tests -----------------------------------------------

func TestScanAudioFiles_FindsFlacAndMP3(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "album")
	os.MkdirAll(sub, 0755)

	// Valid FLAC in a subdirectory.
	flacData := fakeFLAC(44100, 44100, map[string]string{"title": "T", "artist": "A"})
	os.WriteFile(filepath.Join(sub, "track.flac"), flacData, 0644)

	// Valid MP3 in the root.
	mp3Data := fakeMP3(id3FrameLatin1("TIT2", "T"))
	os.WriteFile(filepath.Join(dir, "track.mp3"), mp3Data, 0644)

	// Other files that must be ignored.
	os.WriteFile(filepath.Join(dir, "cover.jpg"), []byte("jpg"), 0644)
	os.WriteFile(filepath.Join(sub, "notes.txt"), []byte("txt"), 0644)

	files, err := scanAudioFiles(context.Background(), dir)
	if err != nil {
		t.Fatalf("scanAudioFiles: %v", err)
	}
	if len(files) != 2 {
		t.Errorf("expected 2 audio files, got %d: %v", len(files), files)
	}
}

func TestScanAudioFiles_SkipsUnparseable(t *testing.T) {
	dir := t.TempDir()
	// File with .flac extension but invalid content — scan must not error,
	// just skip the file.
	os.WriteFile(filepath.Join(dir, "bad.flac"), []byte("garbage"), 0644)

	files, err := scanAudioFiles(context.Background(), dir)
	if err != nil {
		t.Fatalf("scanAudioFiles: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("expected 0 files (bad FLAC skipped), got %d", len(files))
	}
}

func TestScanAudioFiles_FallbackTitle(t *testing.T) {
	dir := t.TempDir()
	// FLAC with no title tag — Title should fall back to the filename (without ext).
	flacData := fakeFLAC(44100, 44100, nil)
	os.WriteFile(filepath.Join(dir, "my track.flac"), flacData, 0644)

	files, err := scanAudioFiles(context.Background(), dir)
	if err != nil {
		t.Fatalf("scanAudioFiles: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	if files[0].Title != "my track" {
		t.Errorf("fallback Title = %q, want %q", files[0].Title, "my track")
	}
}

func TestScanAudioFiles_CancelledContext(t *testing.T) {
	dir := t.TempDir()
	// Put several FLAC files in the directory.
	for i := 0; i < 5; i++ {
		flacData := fakeFLAC(44100, 44100, map[string]string{"title": "T"})
		os.WriteFile(filepath.Join(dir, filepath.Join(dir, strings.Repeat("x", i+1)+".flac")), flacData, 0644)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := scanAudioFiles(ctx, dir)
	if err == nil {
		t.Fatal("expected error from cancelled context, got nil")
	}
}

// ---- runWithClient integration tests ------------------------------------

func TestRunWithClient_FetchesAndSavesLRC(t *testing.T) {
	const lrcContent = "[00:00.00] Hello world"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(lrclibResponse{SyncedLyrics: lrcContent})
	}))
	defer srv.Close()

	dir := t.TempDir()
	flacData := fakeFLAC(44100, 44100, map[string]string{"title": "Song", "artist": "Artist"})
	flacPath := filepath.Join(dir, "song.flac")
	os.WriteFile(flacPath, flacData, 0644)

	if err := runWithClient(context.Background(), dir, testClient(srv.URL)); err != nil {
		t.Fatalf("runWithClient: %v", err)
	}

	lrcPath := filepath.Join(dir, "song.lrc")
	data, err := os.ReadFile(lrcPath)
	if err != nil {
		t.Fatalf("expected .lrc file to be created: %v", err)
	}
	if string(data) != lrcContent {
		t.Errorf(".lrc content = %q, want %q", string(data), lrcContent)
	}
}

func TestRunWithClient_SkipsExistingLRC(t *testing.T) {
	requestCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestCount++
		json.NewEncoder(w).Encode(lrclibResponse{SyncedLyrics: "[00:00.00] New"})
	}))
	defer srv.Close()

	dir := t.TempDir()
	flacData := fakeFLAC(44100, 44100, map[string]string{"title": "Song", "artist": "Artist"})
	os.WriteFile(filepath.Join(dir, "song.flac"), flacData, 0644)
	// Pre-existing .lrc file — must be left untouched, no HTTP request made.
	existingLRC := "[00:00.00] Existing lyrics"
	os.WriteFile(filepath.Join(dir, "song.lrc"), []byte(existingLRC), 0644)

	if err := runWithClient(context.Background(), dir, testClient(srv.URL)); err != nil {
		t.Fatalf("runWithClient: %v", err)
	}

	if requestCount != 0 {
		t.Errorf("expected 0 HTTP requests (lrc already exists), got %d", requestCount)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "song.lrc"))
	if string(data) != existingLRC {
		t.Errorf("existing .lrc was modified")
	}
}

func TestRunWithClient_GracefulOnNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	flacData := fakeFLAC(44100, 44100, map[string]string{"title": "Unknown", "artist": "Nobody"})
	os.WriteFile(filepath.Join(dir, "unknown.flac"), flacData, 0644)

	// Must not return an error — 404 is a soft "not found", not a failure.
	if err := runWithClient(context.Background(), dir, testClient(srv.URL)); err != nil {
		t.Fatalf("runWithClient must not fail on 404; got: %v", err)
	}
	// No .lrc file should be created.
	if _, err := os.Stat(filepath.Join(dir, "unknown.lrc")); err == nil {
		t.Error(".lrc file must not be created when lyrics not found")
	}
}

func TestRunWithClient_EmptyDir(t *testing.T) {
	// No audio files — must return nil without making any HTTP requests.
	requestCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestCount++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	if err := runWithClient(context.Background(), t.TempDir(), testClient(srv.URL)); err != nil {
		t.Fatalf("runWithClient: %v", err)
	}
	if requestCount != 0 {
		t.Errorf("expected 0 HTTP requests for empty dir, got %d", requestCount)
	}
}

func TestRunWithClient_CancelledContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(lrclibResponse{SyncedLyrics: "[00:00.00] ok"})
	}))
	defer srv.Close()

	dir := t.TempDir()
	flacData := fakeFLAC(44100, 44100, map[string]string{"title": "Song", "artist": "A"})
	os.WriteFile(filepath.Join(dir, "song.flac"), flacData, 0644)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before Run

	// Must return nil — interruption is not an error.
	if err := runWithClient(ctx, dir, testClient(srv.URL)); err != nil {
		t.Fatalf("runWithClient with cancelled ctx must return nil, got: %v", err)
	}
}
