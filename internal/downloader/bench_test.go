package downloader

// Benchmarks for the critical path.
//
// End to end, against the fake Qobuz server (wall time; slow by design):
//
//	go test ./internal/downloader -run '^$' -bench E2E -benchtime 1x -count 6
//
// Micro, CPU and allocations of the per-track and per-collection work:
//
//	go test ./internal/downloader -run '^$' -bench Micro -benchmem -count 10
//
// t.TempDir lives under $TMPDIR, which is tmpfs on many Linux systems: set
// TMPDIR to a directory on a real disk to measure the disk.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/vbauerster/mpb/v8"

	"github.com/Aeneaj/qobuz-dl-go/internal/ui"
)

// ---- end to end -----------------------------------------------------------

// e2eScenario is one network model for the fake server.
type e2eScenario struct {
	name     string
	apiDelay time.Duration // added to every API call
	rate     int64         // bytes/s per audio connection; 0 = unlimited
}

var e2eScenarios = []e2eScenario{
	// Loopback, no limits: what the program costs by itself.
	{"local", 0, 0},
	// The measured API latency (median of 10 warm calls to www.qobuz.com)
	// and a per-connection CDN cap that makes one track take one second.
	{"api180ms-2MiBps", 180 * time.Millisecond, e2eTrackSize},
}

const (
	e2eTracks    = 12
	e2eTrackSize = 2 << 20 // audio bytes per track
)

func benchTracks(n int) []fakeTrack {
	tracks := make([]fakeTrack, n)
	for i := range tracks {
		tracks[i] = fakeTrack{ID: 100 + i + 1, Title: fmt.Sprintf("Song %d", i+1), Number: i + 1, MediaNumber: 1, Performer: "A"}
	}
	return tracks
}

// silenceStdout sends the downloader's console output to /dev/null for the
// rest of the benchmark: it is not what is being measured.
func silenceStdout(b *testing.B) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		b.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = null
	b.Cleanup(func() { os.Stdout = old; null.Close() })
}

// runE2E times fn once per iteration on a fresh fake server and reports, on
// top of ns/op, how long it took for the first audio byte to be requested.
func runE2E(b *testing.B, sc e2eScenario, fn func(d *Downloader, dir string) error) {
	silenceStdout(b)
	var firstAudio time.Duration
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		q := newFakeQobuz(b, benchTracks(e2eTracks))
		q.audioPad, q.apiDelay, q.audioRate = e2eTrackSize, sc.apiDelay, sc.rate
		d, dir := newTestDownloader(b, q, nil)
		start := time.Now()
		b.StartTimer()
		if err := fn(d, dir); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		if got := q.fileHits.Load(); got != e2eTracks {
			b.Fatalf("served %d audio files, want %d", got, e2eTracks)
		}
		firstAudio += time.Duration(q.firstAudio.Load() - start.UnixNano())
		// TempDir is only emptied when the benchmark ends: at 24 MB per
		// iteration, a few hundred fill a tmpfs.
		q.srv.Close()
		os.RemoveAll(dir)
	}
	b.ReportMetric(float64(firstAudio.Milliseconds())/float64(b.N), "first-audio-ms")
}

// BenchmarkE2EAlbum downloads a 12-track album with the default 3 workers.
func BenchmarkE2EAlbum(b *testing.B) {
	for _, sc := range e2eScenarios {
		b.Run(sc.name, func(b *testing.B) {
			runE2E(b, sc, func(d *Downloader, dir string) error {
				return d.downloadAlbum(context.Background(), "alb1", dir)
			})
		})
	}
}

// BenchmarkE2EPlaylist downloads the same 12 tracks as a playlist, which goes
// through downloadTrackByID one track at a time.
func BenchmarkE2EPlaylist(b *testing.B) {
	items := make([]interface{}, e2eTracks)
	for i := range items {
		items[i] = map[string]interface{}{"id": float64(100 + i + 1)}
	}
	pages := []map[string]interface{}{{"name": "Bench Playlist", "tracks": map[string]interface{}{"items": items}}}
	for _, sc := range e2eScenarios {
		b.Run(sc.name, func(b *testing.B) {
			runE2E(b, sc, func(d *Downloader, _ string) error {
				return d.downloadPlaylist(context.Background(), pages)
			})
		})
	}
}

// ---- micro ----------------------------------------------------------------

// benchTrackMeta is a track as album/get lists it, with the fields the
// naming and tagging code read.
func benchTrackMeta() map[string]interface{} {
	return map[string]interface{}{
		"id":                    float64(98439707),
		"title":                 "Café del Mar — Nocturne (Live at Sónar)",
		"version":               "2011 Remaster",
		"track_number":          float64(7),
		"media_number":          float64(2),
		"maximum_bit_depth":     float64(24),
		"maximum_sampling_rate": float64(96),
		"performer":             map[string]interface{}{"name": "Ólafur Arnalds"},
		"composer":              map[string]interface{}{"name": "Ólafur Arnalds"},
		"copyright":             "(P) 2011 Mercury KX (C) 2011 Mercury KX",
	}
}

func benchAlbumMeta() map[string]interface{} {
	return map[string]interface{}{
		"title":                 "Living Room Songs",
		"release_date_original": "2011-10-21",
		"tracks_count":          float64(12),
		"artist":                map[string]interface{}{"name": "Ólafur Arnalds"},
		"label":                 map[string]interface{}{"name": "Mercury KX"},
		"copyright":             "(P) 2011 Mercury KX",
		"genres_list":           []interface{}{"Classique", "Classique→Musique de chambre", "Pop/Rock→Electronic"},
	}
}

// BenchmarkMicroFinalTrackPath is the naming work done twice per track: once
// for the skip probe and once when writing.
func BenchmarkMicroFinalTrackPath(b *testing.B) {
	track, album := benchTrackMeta(), benchAlbumMeta()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := finalTrackPath("/music/Ólafur Arnalds - Living Room Songs", track, album, "{tracknumber}. {tracktitle}", false); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkMicroFLACTags builds the Vorbis Comment block of one track.
func BenchmarkMicroFLACTags(b *testing.B) {
	track, album := benchTrackMeta(), benchAlbumMeta()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = buildVorbisComment(buildFLACTags(track, album, false))
	}
}

// BenchmarkMicroID3Frames builds the ID3v2.3 text frames of one track.
func BenchmarkMicroID3Frames(b *testing.B) {
	track, album := benchTrackMeta(), benchAlbumMeta()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var frames []byte
		for id, text := range buildMP3Tags(track, album, false) {
			if text != "" {
				frames = append(frames, buildTextFrame(id, text)...)
			}
		}
		_ = frames
	}
}

// readOnly hides bytes.Reader's WriteTo, so io.Copy reads in chunks the way it
// does from an HTTP body.
type readOnly struct{ r io.Reader }

func (r readOnly) Read(p []byte) (int, error) { return r.r.Read(p) }

// BenchmarkMicroCopy is the per-byte cost of the progress plumbing around the
// audio copy: no bar, the TUI handle, and an mpb bar.
func BenchmarkMicroCopy(b *testing.B) {
	const size = 16 << 20
	src := make([]byte, size)
	p := mpb.New(mpb.WithOutput(io.Discard))
	b.Cleanup(p.Shutdown)
	bars := []struct {
		name string
		bar  func() ProgressBar
	}{
		{"none", func() ProgressBar { return nil }},
		{"tui", func() ProgressBar { return ui.NewTrackHandle("1", nil) }},
		{"mpb", func() ProgressBar { return p.New(size, mpb.BarStyle()) }},
	}
	for _, c := range bars {
		b.Run(c.name, func(b *testing.B) {
			f, err := os.Create(filepath.Join(b.TempDir(), "out"))
			if err != nil {
				b.Fatal(err)
			}
			defer f.Close()
			b.SetBytes(size)
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				f.Seek(0, io.SeekStart)
				bar := c.bar()
				b.StartTimer()
				if _, err := copyAndCommit(f, readOnly{bytes.NewReader(src)}, bar); err != nil {
					b.Fatal(err)
				}
				if mb, ok := bar.(*mpb.Bar); ok {
					mb.Abort(true) // frees its slot; a TrackHandle has no program to tell
				}
			}
		})
	}
}

// BenchmarkMicroDownloadClient fetches 16 MiB over loopback with the download
// client, whose connections push a read deadline forward on every Read, and
// with a plain clone of the default transport.
func BenchmarkMicroDownloadClient(b *testing.B) {
	const size = 16 << 20
	body := make([]byte, size)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(size))
		w.Write(body)
	}))
	b.Cleanup(srv.Close)
	clients := []struct {
		name string
		c    *http.Client
	}{
		{"stallConn", newDownloadClient()},
		{"plain", &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}},
	}
	for _, c := range clients {
		b.Run(c.name, func(b *testing.B) {
			b.SetBytes(size)
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				resp, err := c.c.Get(srv.URL)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := io.Copy(io.Discard, readOnly{resp.Body}); err != nil {
					b.Fatal(err)
				}
				resp.Body.Close()
			}
		})
	}
}

// BenchmarkMicroDBAdd records one finished track: open, append, close.
func BenchmarkMicroDBAdd(b *testing.B) {
	db, err := openDB(filepath.Join(b.TempDir(), "downloads.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := db.add(strconv.Itoa(i)); err != nil {
			b.Fatal(err)
		}
	}
}

// benchDiscogPage is one 500-album page of a large discography, reduced by
// slimPage the way fetchPages keeps it. Titles repeat in groups of five
// (reissues, remasters, live versions), as they do in real discographies.
func benchDiscogPage() map[string]interface{} {
	items := make([]interface{}, 500)
	versions := []string{"", "Remastered", "Live", "Deluxe Edition", "2011 Remaster"}
	for i := range items {
		items[i] = map[string]interface{}{
			"id":                    fmt.Sprintf("%013d", 600000000000+i),
			"title":                 fmt.Sprintf("Symphony No. %d (Op. %d)", i/5+1, i/5+10),
			"version":               versions[i%5],
			"maximum_bit_depth":     float64(16 + 8*(i%2)),
			"maximum_sampling_rate": []float64{44.1, 96, 48, 192, 44.1}[i%5],
			"artist":                map[string]interface{}{"name": "Johann Sebastian Bach"},
			"label":                 map[string]interface{}{"name": "Deutsche Grammophon", "id": float64(i)},
			"image":                 map[string]interface{}{"large": "https://static.qobuz.com/images/covers/x_600.jpg"},
			"genre":                 map[string]interface{}{"name": "Classique", "path": []interface{}{float64(1), float64(2)}},
			"tracks_count":          float64(12),
			"release_date_original": "1999-01-01",
		}
	}
	return map[string]interface{}{"name": "Johann Sebastian Bach", "albums": map[string]interface{}{"items": items}}
}

// BenchmarkMicroSlimPage reduces one page as it arrives.
func BenchmarkMicroSlimPage(b *testing.B) {
	page := benchDiscogPage()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = slimPage(page, "albums")
	}
}

// BenchmarkMicroSmartDiscog filters one slimmed page.
func BenchmarkMicroSmartDiscog(b *testing.B) {
	items := collectPageItems([]map[string]interface{}{slimPage(benchDiscogPage(), "albums")}, "albums")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = smartDiscogFilter("Johann Sebastian Bach", items)
	}
}
