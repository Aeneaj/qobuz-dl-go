package downloader

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Aeneaj/qobuz-dl-go/internal/api"
)

// Integration tests for the album/track download flow: a fake Qobuz API plus a
// file server, driven through the real Downloader. They cover what the unit
// tests cannot — that metadata, path building, the worker pool, tagging and
// the downloads DB actually fit together.

const qobuzBase = "https://www.qobuz.com/api.json/0.2/"

// rewriteTransport redirects Qobuz API calls to the test server. File URLs are
// already absolute test-server URLs, so they pass through untouched.
type rewriteTransport struct {
	target  string // e.g. srv.URL + "/api.json/0.2/"
	wrapped http.RoundTripper
}

func (rt *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if raw := req.URL.String(); strings.HasPrefix(raw, qobuzBase) {
		u, err := url.Parse(rt.target + strings.TrimPrefix(raw, qobuzBase))
		if err != nil {
			return nil, err
		}
		req = req.Clone(req.Context())
		req.URL = u
		req.Host = u.Host
	}
	return rt.wrapped.RoundTrip(req)
}

// fakeTrack is the subset of a Qobuz track object the downloader reads.
type fakeTrack struct {
	ID          int
	Title       string
	Number      int
	MediaNumber int
	Performer   string
}

// fakeQobuz serves album metadata, file URLs, audio bytes and cover art.
type fakeQobuz struct {
	srv    *httptest.Server
	tracks []fakeTrack

	// knobs for the failure-path tests
	noFileURLFor map[int]bool // track IDs that fail track/getFileUrl
	sampleOnly   map[int]bool // track IDs returned as unstreamable samples
	onlyQuality  int          // when set, every other format_id fails (quality fallback path)
	zeroRateFor  map[int]bool // track IDs served with sampling_rate 0 (nothing usable)
	audioPad     int64        // extra audio bytes per file, for the memory benchmarks

	fileHits atomic.Int64 // audio bytes actually served
	mu       sync.Mutex
	seenURLs []string
}

func newFakeQobuz(t testing.TB, tracks []fakeTrack) *fakeQobuz {
	t.Helper()
	q := &fakeQobuz{
		tracks:       tracks,
		noFileURLFor: map[int]bool{},
		sampleOnly:   map[int]bool{},
		zeroRateFor:  map[int]bool{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api.json/0.2/album/get", q.handleAlbumGet)
	mux.HandleFunc("/api.json/0.2/track/getFileUrl", q.handleFileURL)
	mux.HandleFunc("/audio/", q.handleAudio)
	mux.HandleFunc("/img/cover.jpg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte{0xFF, 0xD8, 0xFF, 0xE0, 'J', 'F', 'I', 'F', 0xFF, 0xD9})
	})
	q.srv = httptest.NewServer(mux)
	t.Cleanup(q.srv.Close)
	return q
}

func (q *fakeQobuz) record(path string) {
	q.mu.Lock()
	q.seenURLs = append(q.seenURLs, path)
	q.mu.Unlock()
}

// count returns how many requests hit an endpoint suffix so far.
func (q *fakeQobuz) count(suffix string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := 0
	for _, u := range q.seenURLs {
		if strings.HasSuffix(u, suffix) {
			n++
		}
	}
	return n
}

func (q *fakeQobuz) handleAlbumGet(w http.ResponseWriter, r *http.Request) {
	q.record(r.URL.Path)
	items := make([]interface{}, 0, len(q.tracks))
	for _, tr := range q.tracks {
		items = append(items, map[string]interface{}{
			"id":                    float64(tr.ID),
			"title":                 tr.Title,
			"track_number":          float64(tr.Number),
			"media_number":          float64(tr.MediaNumber),
			"maximum_bit_depth":     float64(24),
			"maximum_sampling_rate": float64(96),
			"streamable":            true,
			"performer":             map[string]interface{}{"name": tr.Performer},
		})
	}
	writeJSON(w, map[string]interface{}{
		"id":                    "alb1",
		"title":                 "Test Album",
		"streamable":            true,
		"release_date_original": "2021-05-04",
		"tracks_count":          float64(len(q.tracks)),
		"artist":                map[string]interface{}{"name": "Test Artist"},
		"image":                 map[string]interface{}{"large": q.srv.URL + "/img/cover.jpg"},
		"tracks":                map[string]interface{}{"items": items},
	})
}

func (q *fakeQobuz) handleFileURL(w http.ResponseWriter, r *http.Request) {
	q.record(r.URL.Path)
	id := r.URL.Query().Get("track_id")
	var n int
	fmt.Sscanf(id, "%d", &n)

	var fid int
	fmt.Sscanf(r.URL.Query().Get("format_id"), "%d", &fid)
	if q.onlyQuality != 0 && fid != q.onlyQuality {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]interface{}{"message": "not available at this quality"})
		return
	}

	if q.noFileURLFor[n] {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]interface{}{"message": "no file url"})
		return
	}
	ext := "flac"
	if fid == 5 {
		ext = "mp3"
	}
	resp := map[string]interface{}{
		"track_id":      float64(n),
		"format_id":     float64(fid),
		"bit_depth":     float64(24),
		"sampling_rate": float64(96),
		"url":           fmt.Sprintf("%s/audio/%d.%s", q.srv.URL, n, ext),
	}
	if q.zeroRateFor[n] {
		resp["bit_depth"], resp["sampling_rate"] = float64(0), float64(0)
	}
	if q.sampleOnly[n] {
		// A preview carries preview-quality numbers, so a caller that reads
		// the format off a sample gets the wrong answer, not just a useless one.
		resp["sample"] = true
		resp["bit_depth"], resp["sampling_rate"] = float64(16), float64(44.1)
	}
	writeJSON(w, resp)
}

func (q *fakeQobuz) handleAudio(w http.ResponseWriter, r *http.Request) {
	q.fileHits.Add(1)
	body, ctype := makeFakeFLAC(), "audio/flac"
	if strings.HasSuffix(r.URL.Path, ".mp3") {
		body, ctype = makeFakeMP3(), "audio/mpeg"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Length", fmt.Sprint(int64(len(body))+q.audioPad))
	w.Write(body)
	// Streamed from one small buffer so the server's own allocations stay
	// out of the client's memory numbers.
	pad := make([]byte, 32<<10)
	for n := q.audioPad; n > 0; n -= int64(len(pad)) {
		w.Write(pad[:min(n, int64(len(pad)))])
	}
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// newTestDownloader wires a Downloader to the fake server. opts is applied on
// top of sensible defaults so each test only states what it cares about.
func newTestDownloader(t testing.TB, q *fakeQobuz, tweak func(*Options)) (*Downloader, string) {
	t.Helper()
	dir := t.TempDir()

	hc := q.srv.Client()
	hc.Transport = &rewriteTransport{target: q.srv.URL + "/api.json/0.2/", wrapped: hc.Transport}
	c := api.NewWithHTTP("appid", []string{"secret"}, hc)
	c.UAT, c.UserID, c.Secret = "token", "1", "secret"

	opts := Options{
		Directory:    dir,
		Quality:      7,
		Workers:      3,
		NoDB:         true,
		NoCover:      false,
		FolderFormat: "{artist} - {album}",
		TrackFormat:  "{tracknumber}. {tracktitle}",
	}
	if tweak != nil {
		tweak(&opts)
	}
	d, err := New(c, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d, dir
}

// relFiles lists every file under root, as slash-separated relative paths.
func relFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(out)
	return out
}

func threeTracks() []fakeTrack {
	return []fakeTrack{
		{ID: 101, Title: "First Song", Number: 1, MediaNumber: 1, Performer: "Test Artist"},
		{ID: 102, Title: "Second Song", Number: 2, MediaNumber: 1, Performer: "Test Artist"},
		{ID: 103, Title: "Third Song", Number: 3, MediaNumber: 1, Performer: "Test Artist"},
	}
}

// ---- tests --------------------------------------------------------------

func TestIntegration_DownloadAlbum(t *testing.T) {
	q := newFakeQobuz(t, threeTracks())
	d, dir := newTestDownloader(t, q, nil)

	if err := d.downloadAlbum(context.Background(), "alb1", dir); err != nil {
		t.Fatalf("downloadAlbum: %v", err)
	}

	want := []string{
		"Test Artist - Test Album/01. First Song.flac",
		"Test Artist - Test Album/02. Second Song.flac",
		"Test Artist - Test Album/03. Third Song.flac",
		"Test Artist - Test Album/cover.jpg",
	}
	got := relFiles(t, dir)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("files on disk:\ngot:\n  %s\nwant:\n  %s",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}

	// No .tmp leftovers: downloadAndTag must rename, not copy.
	for _, f := range got {
		if strings.Contains(f, ".tmp") {
			t.Errorf("temporary file left behind: %s", f)
		}
	}
}

// TestIntegration_TracksAreTagged checks the downloaded file really went
// through tagFLAC — a rename-only path would still produce the right names.
func TestIntegration_TracksAreTagged(t *testing.T) {
	q := newFakeQobuz(t, threeTracks()[:1])
	d, dir := newTestDownloader(t, q, nil)

	if err := d.downloadAlbum(context.Background(), "alb1", dir); err != nil {
		t.Fatalf("downloadAlbum: %v", err)
	}

	path := filepath.Join(dir, "Test Artist - Test Album", "01. First Song.flac")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read track: %v", err)
	}
	if string(data[:4]) != "fLaC" {
		t.Fatalf("not a FLAC file: % x", data[:4])
	}
	for _, want := range []string{"First Song", "Test Album", "Test Artist"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("Vorbis comments missing %q", want)
		}
	}
}

// TestIntegration_SkipsUnavailableTracks covers the two guards in
// collectTrackJobs: a failed track/getFileUrl and a sample-only response.
// Neither may abort the album.
func TestIntegration_SkipsUnavailableTracks(t *testing.T) {
	q := newFakeQobuz(t, threeTracks())
	q.noFileURLFor[102] = true
	q.sampleOnly[103] = true

	d, dir := newTestDownloader(t, q, func(o *Options) {
		o.QualityFallback = false // otherwise 102 retries at other qualities
		o.NoCover = true
	})

	if err := d.downloadAlbum(context.Background(), "alb1", dir); err != nil {
		t.Fatalf("downloadAlbum: %v", err)
	}

	want := []string{"Test Artist - Test Album/01. First Song.flac"}
	if got := relFiles(t, dir); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("files = %v, want %v", got, want)
	}
}

// TestIntegration_MultiDisc pins the "Disc N" layout, which only triggers when
// media_number varies across the tracklist.
func TestIntegration_MultiDisc(t *testing.T) {
	q := newFakeQobuz(t, []fakeTrack{
		{ID: 201, Title: "Opener", Number: 1, MediaNumber: 1, Performer: "Test Artist"},
		{ID: 202, Title: "Closer", Number: 1, MediaNumber: 2, Performer: "Test Artist"},
	})
	d, dir := newTestDownloader(t, q, func(o *Options) { o.NoCover = true })

	if err := d.downloadAlbum(context.Background(), "alb1", dir); err != nil {
		t.Fatalf("downloadAlbum: %v", err)
	}

	want := []string{
		"Test Artist - Test Album/Disc 1/01. Opener.flac",
		"Test Artist - Test Album/Disc 2/01. Closer.flac",
	}
	if got := relFiles(t, dir); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("files = %v, want %v", got, want)
	}
}

// TestIntegration_DBSkipsSecondRun exercises the downloads DB across two full
// runs.
//
// Asserting "no audio re-downloaded" is not enough: downloadAndTag also bails
// out on an os.Stat of the final file, so that holds even with the DB
// disabled. The DB acts earlier, in collectTrackJobs, and its observable
// effect is that no track/getFileUrl call is made at all. resolveFormat still
// makes exactly one for the first track, so that is the expected floor.
func TestIntegration_DBSkipsSecondRun(t *testing.T) {
	q := newFakeQobuz(t, threeTracks())
	dbPath := filepath.Join(t.TempDir(), "downloads.db")
	d, dir := newTestDownloader(t, q, func(o *Options) {
		o.NoDB = false
		o.DBPath = dbPath
		o.NoCover = true
	})

	if err := d.downloadAlbum(context.Background(), "alb1", dir); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if got := q.fileHits.Load(); got != 3 {
		t.Fatalf("first run served %d audio files, want 3", got)
	}
	// 1 from resolveFormat + 3 from collectTrackJobs.
	if got := q.count("/track/getFileUrl"); got != 4 {
		t.Fatalf("first run made %d getFileUrl calls, want 4", got)
	}

	before := q.count("/track/getFileUrl")
	if err := d.downloadAlbum(context.Background(), "alb1", dir); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := q.fileHits.Load(); got != 3 {
		t.Errorf("second run re-downloaded audio: %d files served in total, want 3", got)
	}
	if got := q.count("/track/getFileUrl") - before; got != 1 {
		t.Errorf("second run made %d getFileUrl calls, want 1 (resolveFormat only) — "+
			"the DB did not skip the tracks", got)
	}
}

// TestIntegration_WorkerCounts runs the same album at several pool sizes. All
// must produce identical output; serial (1) is the control.
func TestIntegration_WorkerCounts(t *testing.T) {
	var reference []string
	for _, workers := range []int{1, 2, 3, 8} {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			q := newFakeQobuz(t, threeTracks())
			d, dir := newTestDownloader(t, q, func(o *Options) {
				o.Workers = workers
				o.NoCover = true
			})
			if err := d.downloadAlbum(context.Background(), "alb1", dir); err != nil {
				t.Fatalf("downloadAlbum: %v", err)
			}
			got := relFiles(t, dir)
			if reference == nil {
				reference = got
			} else if strings.Join(got, "\n") != strings.Join(reference, "\n") {
				t.Errorf("workers=%d produced %v, want %v", workers, got, reference)
			}
			if n := q.fileHits.Load(); n != 3 {
				t.Errorf("served %d audio files, want 3", n)
			}
		})
	}
}

// TestIntegration_CancelledContext checks that cancelling before the run
// leaves no partial files behind.
func TestIntegration_CancelledContext(t *testing.T) {
	q := newFakeQobuz(t, threeTracks())
	d, dir := newTestDownloader(t, q, func(o *Options) { o.NoCover = true })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// An aborted run may report an error or return cleanly having done
	// nothing; what matters is that no audio lands on disk.
	_ = d.downloadAlbum(ctx, "alb1", dir)

	for _, f := range relFiles(t, dir) {
		if strings.HasSuffix(f, ".flac") || strings.Contains(f, ".tmp") {
			t.Errorf("cancelled run left %s on disk", f)
		}
	}
}

// TestIntegration_HandleURL covers the dispatch layer: a Qobuz album URL must
// reach downloadAlbum and land files in Options.Directory.
func TestIntegration_HandleURL(t *testing.T) {
	q := newFakeQobuz(t, threeTracks()[:1])
	d, dir := newTestDownloader(t, q, func(o *Options) { o.NoCover = true })

	if err := d.HandleURL(context.Background(), "https://open.qobuz.com/album/alb1"); err != nil {
		t.Fatalf("HandleURL: %v", err)
	}
	want := []string{"Test Artist - Test Album/01. First Song.flac"}
	if got := relFiles(t, dir); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("files = %v, want %v", got, want)
	}
}

// TestIntegration_DownloadURLsLeavesForeignFilesAlone: Directory can be "." or
// "~", so a run must not delete what it did not write. DownloadURLs used to
// finish with a recursive sweep of every ".*.tmp" under Directory, which
// matches Syncthing's in-flight ".syncthing.<name>.tmp" files.
func TestIntegration_DownloadURLsLeavesForeignFilesAlone(t *testing.T) {
	q := newFakeQobuz(t, threeTracks()[:1])
	d, dir := newTestDownloader(t, q, func(o *Options) { o.NoCover = true })

	foreign := []string{
		filepath.Join(dir, ".syncthing.song.flac.tmp"),
		filepath.Join(dir, "Other", ".01.tmp"),
	}
	for _, f := range foreign {
		if err := os.MkdirAll(filepath.Dir(f), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("not ours"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	d.DownloadURLs(context.Background(), []string{"https://open.qobuz.com/album/alb1"})

	for _, f := range foreign {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s was removed: %v", f, err)
		}
	}
}

// TestIntegration_FolderFormatSurvivesUnresolvableTrack is the regression test
// for issue #22: a folder_format made only of valid placeholders was ignored
// and the album got the default naming instead.
//
// resolveFormat used to ask items[0] alone for the delivered format. Any single
// failure there returned "Unknown", which is what makes cleanFormatStr throw
// away the user's whole template — so one track unavailable at the requested
// quality renamed the entire album, even though every other track downloaded
// fine.
func TestIntegration_FolderFormatSurvivesUnresolvableTrack(t *testing.T) {
	tracks := []fakeTrack{
		{ID: 1, Title: "One", Number: 1, MediaNumber: 1, Performer: "P"},
		{ID: 2, Title: "Two", Number: 2, MediaNumber: 1, Performer: "P"},
		{ID: 3, Title: "Three", Number: 3, MediaNumber: 1, Performer: "P"},
	}
	// The reporter's own format, valid placeholders only.
	const folderFmt = "{year}-{album}-{bit_depth}.{sampling_rate}"
	const want = "2021-Test Album-24.96"

	cases := []struct {
		name   string
		break_ func(*fakeQobuz, *Options)
		want   string
	}{
		{"every track resolves", nil, want},
		{"first track has no file URL", func(q *fakeQobuz, _ *Options) { q.noFileURLFor[1] = true }, want},
		{"first track is a sample", func(q *fakeQobuz, _ *Options) { q.sampleOnly[1] = true }, want},
		{"first track has no usable rate", func(q *fakeQobuz, _ *Options) { q.zeroRateFor[1] = true }, want},
		{
			// The issue #12 shape: not offered at the requested quality, fine
			// one step down. The fallback must feed the folder name too.
			"album only available below the requested quality",
			func(q *fakeQobuz, o *Options) { q.onlyQuality = 6; o.QualityFallback = true },
			"2021-Test Album-24.96",
		},
		{
			// Last resort: nothing resolves at all. The template cannot be
			// honoured (there is no bit depth to report) and cleanFormatStr's
			// substitute stands — announced, since 42ae84d.
			"no track resolves",
			func(q *fakeQobuz, _ *Options) {
				q.noFileURLFor[1], q.noFileURLFor[2], q.noFileURLFor[3] = true, true, true
			},
			"Test Artist - Test Album",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := newFakeQobuz(t, tracks)
			d, dir := newTestDownloader(t, q, func(o *Options) {
				o.FolderFormat = folderFmt
				if c.break_ != nil {
					c.break_(q, o)
				}
			})
			if err := d.downloadAlbum(context.Background(), "alb1", dir); err != nil {
				t.Fatalf("downloadAlbum: %v", err)
			}

			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatalf("read dir: %v", err)
			}
			var dirs []string
			for _, e := range entries {
				if e.IsDir() {
					dirs = append(dirs, e.Name())
				}
			}
			if len(dirs) != 1 || dirs[0] != c.want {
				t.Errorf("album folder = %v, want [%q]", dirs, c.want)
			}
		})
	}
}

// A lossless request that the API can only satisfy at 320 comes back as MP3.
// The extension and the tagger used to follow Options.Quality, so those bytes
// were written to a .flac name and run through the FLAC tagger.
func TestIntegration_QualityFallbackToMP3WritesMP3(t *testing.T) {
	tracks := []fakeTrack{
		{ID: 1, Title: "One", Number: 1, MediaNumber: 1, Performer: "P"},
		{ID: 2, Title: "Two", Number: 2, MediaNumber: 1, Performer: "P"},
	}
	q := newFakeQobuz(t, tracks)
	q.onlyQuality = 5 // nothing available losslessly

	d, dir := newTestDownloader(t, q, func(o *Options) {
		o.Quality = 7
		o.QualityFallback = true
		o.NoCover = true
		// {format} makes resolveFormat's answer observable: it must report what
		// was delivered, not what was asked for.
		o.FolderFormat = "{album} [{format}]"
	})
	if err := d.downloadAlbum(context.Background(), "alb1", dir); err != nil {
		t.Fatalf("downloadAlbum: %v", err)
	}

	if entries, _ := os.ReadDir(dir); len(entries) != 1 || entries[0].Name() != "Test Album [MP3]" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("album folder = %v, want [\"Test Album [MP3]\"]", names)
	}

	files := relFiles(t, dir)
	var audio []string
	for _, f := range files {
		if strings.HasSuffix(f, ".mp3") || strings.HasSuffix(f, ".flac") {
			audio = append(audio, f)
		}
	}
	if len(audio) != 2 {
		t.Fatalf("audio files = %v, want 2", audio)
	}
	for _, f := range audio {
		if !strings.HasSuffix(f, ".mp3") {
			t.Errorf("%q: MP3 bytes written under a lossless extension", f)
		}
		head, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f)))
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		// ID3v2 means the MP3 tagger ran; "fLaC" would mean the FLAC one did.
		if len(head) < 3 || string(head[:3]) != "ID3" {
			t.Errorf("%q does not start with an ID3 header: % x", f, head[:min(8, len(head))])
		}
	}
}
