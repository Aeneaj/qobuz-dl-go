package downloader

// integration_test.go — end-to-end tests that drive the downloader against a
// mock Qobuz API and a mock CDN, plus focused tests for the resume/retry
// machinery in downloadWithProgress.
//
// Everything is stdlib: net/http/httptest for the servers, no fixtures on
// disk beyond t.TempDir().

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aeneaj/qobuz-dl-go/internal/api"
)

// ---- CDN mock: a file server that can fail in the ways real ones do -------

// flakyCDN serves payload over HTTP with controllable misbehaviour, so each
// branch of the resume logic can be driven deliberately.
type flakyCDN struct {
	payload []byte

	// failAfter > 0 drops the connection after writing that many bytes of the
	// body, for the first failTimes requests. This is what a mid-download
	// server drop looks like to the client: io.ErrUnexpectedEOF.
	failAfter int
	failTimes int

	// ignoreRange makes the server answer 200 with the whole body even when
	// the client asked for a byte range — some CDNs really do this.
	ignoreRange bool

	// status, when non-zero, is returned instead of serving the body.
	status int

	mu     sync.Mutex
	ranges []string // Range header of every request received, in order
}

func (f *flakyCDN) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.ranges = append(f.ranges, r.Header.Get("Range"))
	n := len(f.ranges)
	f.mu.Unlock()

	if f.status != 0 {
		w.WriteHeader(f.status)
		return
	}

	start := 0
	if rng := r.Header.Get("Range"); rng != "" && !f.ignoreRange {
		fmt.Sscanf(rng, "bytes=%d-", &start)
		if start > len(f.payload) {
			start = len(f.payload)
		}
		w.Header().Set("Content-Range",
			fmt.Sprintf("bytes %d-%d/%d", start, len(f.payload)-1, len(f.payload)))
		w.Header().Set("Content-Length", strconv.Itoa(len(f.payload)-start))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.Header().Set("Content-Length", strconv.Itoa(len(f.payload)))
		w.WriteHeader(http.StatusOK)
	}

	body := f.payload[start:]
	if f.failAfter > 0 && n <= f.failTimes {
		if f.failAfter < len(body) {
			body = body[:f.failAfter]
		}
		w.Write(body)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		// Abort without finishing the declared Content-Length. net/http
		// recognises this sentinel and drops the connection quietly.
		panic(http.ErrAbortHandler)
	}
	w.Write(body)
}

func (f *flakyCDN) rangeHeaders() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ranges...)
}

// testDownloader builds a Downloader wired for tests: near-zero retry backoff
// and progress bars sent to io.Discard instead of the terminal.
func testDownloader(t *testing.T, opts Options) *Downloader {
	t.Helper()
	if opts.Directory == "" {
		opts.Directory = t.TempDir()
	}
	d, err := New(nil, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.retryDelay = time.Millisecond
	d.progressOut = io.Discard
	return d
}

// ---- downloadWithProgress ------------------------------------------------

func TestDownloadWithProgress_Plain(t *testing.T) {
	cdn := &flakyCDN{payload: bytes.Repeat([]byte("abcd"), 64)}
	srv := httptest.NewServer(http.HandlerFunc(cdn.handler))
	defer srv.Close()

	d := testDownloader(t, Options{})
	dest := filepath.Join(t.TempDir(), "out.bin")
	if err := d.downloadWithProgress(context.Background(), srv.URL, dest, nil); err != nil {
		t.Fatalf("downloadWithProgress: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, cdn.payload) {
		t.Errorf("got %d bytes, want %d", len(got), len(cdn.payload))
	}
	if n := len(cdn.rangeHeaders()); n != 1 {
		t.Errorf("made %d requests, want 1", n)
	}
}

// A drop mid-body must be resumed with a Range request, not restarted, and
// the bytes already on disk must not be duplicated.
func TestDownloadWithProgress_ResumesAfterDrop(t *testing.T) {
	cdn := &flakyCDN{
		payload:   bytes.Repeat([]byte("xyz!"), 64), // 256 bytes
		failAfter: 100,
		failTimes: 1,
	}
	srv := httptest.NewServer(http.HandlerFunc(cdn.handler))
	defer srv.Close()

	d := testDownloader(t, Options{})
	dest := filepath.Join(t.TempDir(), "out.bin")
	if err := d.downloadWithProgress(context.Background(), srv.URL, dest, nil); err != nil {
		t.Fatalf("downloadWithProgress: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, cdn.payload) {
		t.Fatalf("file is %d bytes, want %d — resume corrupted it", len(got), len(cdn.payload))
	}

	reqs := cdn.rangeHeaders()
	if len(reqs) != 2 {
		t.Fatalf("made %d requests, want 2 (initial + resume): %v", len(reqs), reqs)
	}
	if reqs[0] != "" {
		t.Errorf("first request sent Range %q, want none", reqs[0])
	}
	if reqs[1] != "bytes=100-" {
		t.Errorf("resume asked for %q, want %q", reqs[1], "bytes=100-")
	}
}

// A server that answers a Range request with 200 and the whole body would
// duplicate the partial bytes if appended. The partial file must be discarded.
func TestDownloadWithProgress_ServerIgnoresRange(t *testing.T) {
	cdn := &flakyCDN{
		payload:     bytes.Repeat([]byte("0123456789"), 30), // 300 bytes
		failAfter:   120,
		failTimes:   1,
		ignoreRange: true,
	}
	srv := httptest.NewServer(http.HandlerFunc(cdn.handler))
	defer srv.Close()

	d := testDownloader(t, Options{})
	dest := filepath.Join(t.TempDir(), "out.bin")
	if err := d.downloadWithProgress(context.Background(), srv.URL, dest, nil); err != nil {
		t.Fatalf("downloadWithProgress: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, cdn.payload) {
		t.Errorf("file is %d bytes, want %d — partial data was not discarded",
			len(got), len(cdn.payload))
	}
}

func TestDownloadWithProgress_GivesUpAfterMaxRetries(t *testing.T) {
	cdn := &flakyCDN{
		payload:   bytes.Repeat([]byte("z"), 200),
		failAfter: 10,
		failTimes: 99, // never succeeds
	}
	srv := httptest.NewServer(http.HandlerFunc(cdn.handler))
	defer srv.Close()

	d := testDownloader(t, Options{})
	dest := filepath.Join(t.TempDir(), "out.bin")
	err := d.downloadWithProgress(context.Background(), srv.URL, dest, nil)
	if err == nil {
		t.Fatal("expected an error once the retries ran out")
	}
	if n := len(cdn.rangeHeaders()); n != maxDownloadRetries {
		t.Errorf("made %d attempts, want %d", n, maxDownloadRetries)
	}
}

// A 4xx is not a transient failure: it must surface immediately.
func TestDownloadWithProgress_HTTPErrorIsNotRetried(t *testing.T) {
	cdn := &flakyCDN{status: http.StatusNotFound}
	srv := httptest.NewServer(http.HandlerFunc(cdn.handler))
	defer srv.Close()

	d := testDownloader(t, Options{})
	dest := filepath.Join(t.TempDir(), "out.bin")
	err := d.downloadWithProgress(context.Background(), srv.URL, dest, nil)
	if err == nil {
		t.Fatal("expected an error for HTTP 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should mention the status: %v", err)
	}
	if n := len(cdn.rangeHeaders()); n != 1 {
		t.Errorf("made %d requests, want 1 — a 404 must not be retried", n)
	}
}

func TestDownloadWithProgress_ContextCancelled(t *testing.T) {
	cdn := &flakyCDN{
		payload:   bytes.Repeat([]byte("q"), 200),
		failAfter: 10,
		failTimes: 99,
	}
	srv := httptest.NewServer(http.HandlerFunc(cdn.handler))
	defer srv.Close()

	d := testDownloader(t, Options{})
	d.retryDelay = time.Hour // the wait must be interrupted, not waited out

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	done := make(chan error, 1)
	go func() {
		done <- d.downloadWithProgress(ctx, srv.URL, filepath.Join(t.TempDir(), "out.bin"), nil)
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error after cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not interrupt the retry backoff")
	}
}

// ---- Qobuz API mock ------------------------------------------------------

// qobuzMock stands in for both the Qobuz API and the CDN it hands URLs out
// for. Album metadata is supplied by the test; track audio is a valid minimal
// FLAC so the real tagging code runs.
type qobuzMock struct {
	srv   *httptest.Server
	album map[string]interface{}
	audio []byte

	// searchMisses marks queries that track/search should answer with no
	// results, so the "not on Qobuz" path can be exercised.
	searchMisses map[string]bool

	mu       sync.Mutex
	fileHits int
	searches []string
}

func (m *qobuzMock) searchQueries() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.searches...)
}

func newQobuzMock(t *testing.T, album map[string]interface{}) *qobuzMock {
	t.Helper()
	m := &qobuzMock{album: album, audio: makeFakeFLAC()}
	m.srv = httptest.NewServer(http.HandlerFunc(m.route))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *qobuzMock) route(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/album/get":
		json.NewEncoder(w).Encode(m.album)

	case r.URL.Path == "/track/get":
		json.NewEncoder(w).Encode(m.trackByID(r.URL.Query().Get("track_id")))

	case r.URL.Path == "/track/getFileUrl":
		id := r.URL.Query().Get("track_id")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"url":           m.srv.URL + "/audio/" + id,
			"bit_depth":     16,
			"sampling_rate": 44.1,
			"format_id":     6,
		})

	case strings.HasPrefix(r.URL.Path, "/audio/"):
		m.mu.Lock()
		m.fileHits++
		m.mu.Unlock()
		w.Header().Set("Content-Length", strconv.Itoa(len(m.audio)))
		w.Write(m.audio)

	case r.URL.Path == "/track/search":
		query := r.URL.Query().Get("query")
		m.mu.Lock()
		m.searches = append(m.searches, query)
		m.mu.Unlock()

		items := []interface{}{}
		if !m.searchMisses[query] && len(m.tracks()) > 0 {
			items = append(items, m.tracks()[0])
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"tracks": map[string]interface{}{"items": items},
		})

	case r.URL.Path == "/artist/get":
		json.NewEncoder(w).Encode(map[string]interface{}{
			"name":         "Test Artist",
			"albums_count": 1,
			"albums": map[string]interface{}{"items": []interface{}{
				map[string]interface{}{"id": "alb1", "title": "Test Album"},
			}},
		})

	case r.URL.Path == "/playlist/get":
		items := make([]interface{}, 0, len(m.tracks()))
		for _, tr := range m.tracks() {
			items = append(items, tr)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"name":         "My Playlist",
			"tracks_count": len(items),
			"tracks":       map[string]interface{}{"items": items},
		})

	case r.URL.Path == "/cover.jpg":
		w.Write([]byte("\xff\xd8\xff\xe0JFIF-ish cover bytes"))

	default:
		http.NotFound(w, r)
	}
}

// trackByID finds a track in the album tracklist and returns it with the
// album attached, which is the shape track/get returns.
func (m *qobuzMock) trackByID(id string) map[string]interface{} {
	for _, raw := range m.tracks() {
		if idStr(raw["id"]) == id {
			out := map[string]interface{}{}
			for k, v := range raw {
				out[k] = v
			}
			out["album"] = m.album
			return out
		}
	}
	return map[string]interface{}{}
}

func (m *qobuzMock) tracks() []map[string]interface{} {
	section, _ := m.album["tracks"].(map[string]interface{})
	raw, _ := section["items"].([]interface{})
	var out []map[string]interface{}
	for _, r := range raw {
		if t, ok := r.(map[string]interface{}); ok {
			out = append(out, t)
		}
	}
	return out
}

// downloaderFor wires a Downloader to the mock, with predictable name formats
// so the assertions can name exact paths.
func (m *qobuzMock) downloaderFor(t *testing.T, mutate func(*Options)) (*Downloader, string) {
	t.Helper()
	dir := t.TempDir()
	opts := Options{
		Directory:    dir,
		Quality:      6,
		FolderFormat: "{artist} - {album}",
		TrackFormat:  "{tracknumber}. {tracktitle}",
		DBPath:       filepath.Join(t.TempDir(), "downloads.db"),
		Workers:      2,
		NoM3U:        true,
	}
	if mutate != nil {
		mutate(&opts)
	}
	d := testDownloader(t, opts)
	d.Client = apiClientFor(m.srv)
	return d, dir
}

func track(id int, num int, disc int, title string) map[string]interface{} {
	return map[string]interface{}{
		"id":           float64(id),
		"track_number": float64(num),
		"media_number": float64(disc),
		"title":        title,
		"performer":    map[string]interface{}{"name": "Test Artist"},
	}
}

func testAlbum(coverURL string, tracks ...map[string]interface{}) map[string]interface{} {
	items := make([]interface{}, len(tracks))
	for i, t := range tracks {
		items[i] = t
	}
	return map[string]interface{}{
		"id":                    "alb1",
		"title":                 "Test Album",
		"streamable":            true,
		"release_date_original": "2024-03-01",
		"tracks_count":          float64(len(tracks)),
		"artist":                map[string]interface{}{"name": "Test Artist"},
		"image":                 map[string]interface{}{"large": coverURL},
		"genres_list":           []interface{}{"Pop/Rock"},
		"tracks":                map[string]interface{}{"items": items},
	}
}

// ---- full album flow -----------------------------------------------------

func TestDownloadAlbum_EndToEnd(t *testing.T) {
	m := newQobuzMock(t, nil)
	m.album = testAlbum(m.srv.URL+"/cover.jpg",
		track(101, 1, 1, "Song One"),
		track(102, 2, 1, "Song Two"),
	)
	d, dir := m.downloaderFor(t, nil)

	if err := d.downloadAlbum(context.Background(), "alb1", dir); err != nil {
		t.Fatalf("downloadAlbum: %v", err)
	}

	albumDir := filepath.Join(dir, "Test Artist - Test Album")
	for _, name := range []string{"01. Song One.flac", "02. Song Two.flac", "cover.jpg"} {
		if _, err := os.Stat(filepath.Join(albumDir, name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}

	// The downloaded file must be a tagged FLAC, not the raw bytes we served.
	data, err := os.ReadFile(filepath.Join(albumDir, "01. Song One.flac"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data[:4]) != "fLaC" {
		t.Error("output is not a FLAC file")
	}
	for _, want := range []string{"TITLE=Song One", "ARTIST=Test Artist", "ALBUM=Test Album"} {
		if !bytes.Contains(data, []byte(want)) {
			t.Errorf("tag %q missing from the written file", want)
		}
	}

	// Both tracks recorded in the downloads DB.
	db, err := os.ReadFile(d.Opts.DBPath)
	if err != nil {
		t.Fatalf("downloads DB not written: %v", err)
	}
	for _, id := range []string{"101", "102"} {
		if !strings.Contains(string(db), id) {
			t.Errorf("track %s not recorded in the DB: %q", id, db)
		}
	}

	assertNoTmpFiles(t, dir)
}

func TestDownloadAlbum_MultiDiscUsesSubdirs(t *testing.T) {
	m := newQobuzMock(t, nil)
	m.album = testAlbum(m.srv.URL+"/cover.jpg",
		track(201, 1, 1, "Disc One Opener"),
		track(202, 1, 2, "Disc Two Opener"),
	)
	d, dir := m.downloaderFor(t, nil)

	if err := d.downloadAlbum(context.Background(), "alb1", dir); err != nil {
		t.Fatalf("downloadAlbum: %v", err)
	}

	albumDir := filepath.Join(dir, "Test Artist - Test Album")
	for _, rel := range []string{
		filepath.Join("Disc 1", "01. Disc One Opener.flac"),
		filepath.Join("Disc 2", "01. Disc Two Opener.flac"),
	} {
		if _, err := os.Stat(filepath.Join(albumDir, rel)); err != nil {
			t.Errorf("missing %s: %v", rel, err)
		}
	}
}

func TestDownloadAlbum_SkipsTracksAlreadyInDB(t *testing.T) {
	m := newQobuzMock(t, nil)
	m.album = testAlbum(m.srv.URL+"/cover.jpg",
		track(301, 1, 1, "Already Have It"),
		track(302, 2, 1, "Fetch This One"),
	)

	dbPath := filepath.Join(t.TempDir(), "downloads.db")
	if err := os.WriteFile(dbPath, []byte("301\n"), 0600); err != nil {
		t.Fatal(err)
	}
	d, dir := m.downloaderFor(t, func(o *Options) { o.DBPath = dbPath })

	if err := d.downloadAlbum(context.Background(), "alb1", dir); err != nil {
		t.Fatalf("downloadAlbum: %v", err)
	}

	albumDir := filepath.Join(dir, "Test Artist - Test Album")
	if _, err := os.Stat(filepath.Join(albumDir, "01. Already Have It.flac")); err == nil {
		t.Error("track listed in the DB was downloaded again")
	}
	if _, err := os.Stat(filepath.Join(albumDir, "02. Fetch This One.flac")); err != nil {
		t.Errorf("new track was not downloaded: %v", err)
	}
}

func TestDownloadAlbum_SkipsNonStreamable(t *testing.T) {
	m := newQobuzMock(t, nil)
	m.album = testAlbum(m.srv.URL+"/cover.jpg", track(401, 1, 1, "Blocked"))
	m.album["streamable"] = false
	d, dir := m.downloaderFor(t, nil)

	if err := d.downloadAlbum(context.Background(), "alb1", dir); err != nil {
		t.Fatalf("downloadAlbum: %v", err)
	}
	if m.fileHits != 0 {
		t.Errorf("fetched %d files from a non-streamable album, want 0", m.fileHits)
	}
}

// An existing output file short-circuits the download entirely.
func TestDownloadAlbum_SkipsExistingFiles(t *testing.T) {
	m := newQobuzMock(t, nil)
	m.album = testAlbum(m.srv.URL+"/cover.jpg", track(501, 1, 1, "On Disk"))
	d, dir := m.downloaderFor(t, func(o *Options) { o.NoDB = true })

	albumDir := filepath.Join(dir, "Test Artist - Test Album")
	if err := os.MkdirAll(albumDir, 0755); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(albumDir, "01. On Disk.flac")
	if err := os.WriteFile(existing, []byte("untouched"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := d.downloadAlbum(context.Background(), "alb1", dir); err != nil {
		t.Fatalf("downloadAlbum: %v", err)
	}

	got, err := os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "untouched" {
		t.Error("an already-downloaded file was overwritten")
	}
}

func TestHandleURL_TrackDownloadsSingleFile(t *testing.T) {
	m := newQobuzMock(t, nil)
	m.album = testAlbum(m.srv.URL+"/cover.jpg", track(601, 3, 1, "Lonely Track"))
	d, dir := m.downloaderFor(t, nil)

	if err := d.HandleURL(context.Background(), "https://open.qobuz.com/track/601"); err != nil {
		t.Fatalf("HandleURL: %v", err)
	}

	path := filepath.Join(dir, "Test Artist - Test Album", "03. Lonely Track.flac")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("track not downloaded: %v", err)
	}
	if !bytes.Contains(data, []byte("TITLE=Lonely Track")) {
		t.Error("single-track download was not tagged")
	}
	assertNoTmpFiles(t, dir)
}

// ---- MP3 flow ------------------------------------------------------------

// utf16le encodes s the way ID3v2.3 text frames carry it (BOM + LE units).
// Deliberately independent of the production encoder.
func utf16le(s string) []byte {
	out := []byte{0xFF, 0xFE}
	for _, r := range s {
		out = append(out, byte(r), byte(r>>8))
	}
	return out
}

// Quality 5 takes the MP3 branch: .mp3 extension, ID3v2.3 tags and, with
// EmbedArt, an APIC frame carrying the cover downloaded alongside the album.
func TestDownloadAlbum_MP3TaggingEndToEnd(t *testing.T) {
	m := newQobuzMock(t, nil)
	m.album = testAlbum(m.srv.URL+"/cover.jpg", track(1001, 7, 2, "Mp3 Song"))
	m.audio = []byte("\xff\xfb\x90\x00fake mpeg audio frames")

	d, dir := m.downloaderFor(t, func(o *Options) {
		o.Quality = 5
		o.EmbedArt = true
	})

	if err := d.downloadAlbum(context.Background(), "alb1", dir); err != nil {
		t.Fatalf("downloadAlbum: %v", err)
	}

	path := filepath.Join(dir, "Test Artist - Test Album", "07. Mp3 Song.mp3")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("MP3 not written: %v", err)
	}

	if string(data[:3]) != "ID3" || data[3] != 0x03 {
		t.Fatalf("not an ID3v2.3 tag: % x", data[:5])
	}
	// Text frames are UTF-16LE with a BOM.
	for frame, text := range map[string]string{
		"TIT2": "Mp3 Song",
		"TPE1": "Test Artist",
		"TALB": "Test Album",
		"TRCK": "7/1",
		"TPOS": "2",
	} {
		if !bytes.Contains(data, []byte(frame)) {
			t.Errorf("frame %s missing", frame)
		}
		if !bytes.Contains(data, utf16le(text)) {
			t.Errorf("frame %s does not carry %q", frame, text)
		}
	}
	// Cover embedded as APIC, and the audio payload survived the rewrite.
	if !bytes.Contains(data, []byte("APIC")) {
		t.Error("no APIC frame — cover was not embedded")
	}
	if !bytes.Contains(data, []byte("JFIF-ish cover bytes")) {
		t.Error("APIC frame does not contain the downloaded cover")
	}
	if !bytes.HasSuffix(data, m.audio) {
		t.Error("MPEG audio payload was lost or reordered")
	}
	assertNoTmpFiles(t, dir)
}

// buildMP3Tags has an album branch and a single-track branch that read their
// fields from different places; only the album one runs during an album
// download.
func TestBuildMP3Tags_TrackBranch(t *testing.T) {
	trackMeta := map[string]interface{}{
		"title":        "Solo Song",
		"track_number": float64(4),
		"media_number": float64(1),
		"performer":    map[string]interface{}{"name": "Performer"},
		"composer":     map[string]interface{}{"name": "Composer"},
		"copyright":    "(C) 2024",
		"album": map[string]interface{}{
			"title":                 "Parent Album",
			"artist":                map[string]interface{}{"name": "Album Artist"},
			"release_date_original": "2024-07-09",
			"tracks_count":          float64(11),
			"genres_list":           []interface{}{"Jazz"},
			"label":                 map[string]interface{}{"name": "Label Co"},
		},
	}
	tags := buildMP3Tags(trackMeta, nil, true)

	want := map[string]string{
		"TIT2": "Solo Song",
		"TPE1": "Performer",
		"TPE2": "Album Artist",
		"TALB": "Parent Album",
		"TCOM": "Composer",
		"TDRC": "2024-07-09",
		"TYER": "2024",
		"TRCK": "4/11",
		"TPOS": "1",
		"TCON": "Jazz",
		"TPUB": "Label Co",
		"TCOP": "© 2024",
	}
	for k, v := range want {
		if tags[k] != v {
			t.Errorf("tag %s = %q, want %q", k, tags[k], v)
		}
	}
}

// findCover looks next to the track and then one directory up, which is where
// the cover lives for multi-disc albums.
func TestFindCover(t *testing.T) {
	base := t.TempDir()
	discDir := filepath.Join(base, "Disc 1")
	if err := os.MkdirAll(discDir, 0755); err != nil {
		t.Fatal(err)
	}

	if got := findCover(discDir); got != "" {
		t.Errorf("found %q when there is no cover anywhere", got)
	}

	parentCover := filepath.Join(base, "cover.jpg")
	if err := os.WriteFile(parentCover, []byte("parent"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := findCover(discDir); got != parentCover {
		t.Errorf("findCover = %q, want the parent cover %q", got, parentCover)
	}

	ownCover := filepath.Join(discDir, "cover.jpg")
	if err := os.WriteFile(ownCover, []byte("own"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := findCover(discDir); got != ownCover {
		t.Errorf("findCover = %q, want the local cover %q", got, ownCover)
	}

	if _, err := readCover(t.TempDir()); err == nil {
		t.Error("readCover should fail when there is no cover")
	}
}

// ---- collection flows ----------------------------------------------------

// collectPageItems is the flattening step shared by the artist, label and
// playlist flows; paginated responses are the case worth pinning down.
func TestCollectPageItems(t *testing.T) {
	page := func(key string, ids ...string) map[string]interface{} {
		items := make([]interface{}, len(ids))
		for i, id := range ids {
			items[i] = map[string]interface{}{"id": id}
		}
		return map[string]interface{}{key: map[string]interface{}{"items": items}}
	}

	cases := []struct {
		name  string
		pages []map[string]interface{}
		key   string
		want  []string
	}{
		{"single page", []map[string]interface{}{page("albums", "a", "b")}, "albums", []string{"a", "b"}},
		{
			"across pages",
			[]map[string]interface{}{page("albums", "a"), page("albums", "b", "c")},
			"albums", []string{"a", "b", "c"},
		},
		{"missing section is skipped", []map[string]interface{}{{"name": "x"}, page("albums", "a")}, "albums", []string{"a"}},
		{"wrong key", []map[string]interface{}{page("albums", "a")}, "tracks", nil},
		{"no pages", nil, "albums", nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := collectPageItems(c.pages, c.key)
			if len(got) != len(c.want) {
				t.Fatalf("got %d items, want %d", len(got), len(c.want))
			}
			for i, want := range c.want {
				if id := idStr(got[i]["id"]); id != want {
					t.Errorf("item %d = %q, want %q", i, id, want)
				}
			}
		})
	}
}

func TestHandleURL_ArtistDownloadsDiscography(t *testing.T) {
	m := newQobuzMock(t, nil)
	m.album = testAlbum(m.srv.URL+"/cover.jpg", track(701, 1, 1, "Discog Track"))
	d, dir := m.downloaderFor(t, nil)

	if err := d.HandleURL(context.Background(), "https://open.qobuz.com/artist/55"); err != nil {
		t.Fatalf("HandleURL: %v", err)
	}

	// Discography goes under a directory named after the artist, and each
	// album keeps its own folder inside it.
	path := filepath.Join(dir, "Test Artist", "Test Artist - Test Album", "01. Discog Track.flac")
	if _, err := os.Stat(path); err != nil {
		t.Errorf("missing %s: %v", path, err)
	}
}

func TestHandleURL_PlaylistWritesM3U(t *testing.T) {
	m := newQobuzMock(t, nil)
	m.album = testAlbum(m.srv.URL+"/cover.jpg",
		track(801, 1, 1, "Playlist Track"),
	)
	d, dir := m.downloaderFor(t, func(o *Options) { o.NoM3U = false })

	if err := d.HandleURL(context.Background(), "https://open.qobuz.com/playlist/77"); err != nil {
		t.Fatalf("HandleURL: %v", err)
	}

	playlistDir := filepath.Join(dir, "My Playlist")
	m3u := filepath.Join(playlistDir, "My Playlist.m3u")
	data, err := os.ReadFile(m3u)
	if err != nil {
		t.Fatalf("M3U not written: %v", err)
	}
	if !strings.HasPrefix(string(data), "#EXTM3U") {
		t.Errorf("M3U missing its header: %q", data)
	}
	if !strings.Contains(string(data), "Playlist Track") {
		t.Errorf("M3U does not list the downloaded track: %q", data)
	}
}

// ---- helpers -------------------------------------------------------------

// apiClientFor returns a real api.Client pointed at the mock server.
func apiClientFor(srv *httptest.Server) *api.Client {
	c := api.New("123456789", []string{"testsecret"})
	c.BaseURL = srv.URL + "/"
	c.Secret = "testsecret"
	return c
}

func assertNoTmpFiles(t *testing.T, dir string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover temp file: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
