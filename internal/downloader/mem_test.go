package downloader

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"sync"
	"testing"
	"time"
)

// Memory checks for the paths that touch whole audio files. Tagging must cost
// the metadata, not the track: a 24/192 album has 150–200 MB tracks and the
// worker pool tags several at once.
//
//	go test ./internal/downloader -run '^$' -bench Mem -benchmem -benchtime 3x

const trackSize = 50 << 20 // a typical 24/96 track

// writeBigAudio writes head followed by size zero bytes, streamed so the
// fixture itself does not inflate the numbers being measured.
func writeBigAudio(tb testing.TB, path string, head []byte, size int64) {
	tb.Helper()
	f, err := os.Create(path)
	if err != nil {
		tb.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(head); err != nil {
		tb.Fatal(err)
	}
	if err := f.Truncate(int64(len(head)) + size); err != nil {
		tb.Fatal(err)
	}
}

// peakHeap runs fn while sampling live heap bytes every millisecond and
// returns the highest value seen above the starting point.
func peakHeap(fn func()) uint64 {
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	read := func() uint64 { metrics.Read(sample); return sample[0].Value.Uint64() }

	runtime.GC()
	base := read()
	var peak uint64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				peak = max(peak, read())
			}
		}
	}()
	fn()
	close(stop)
	wg.Wait()
	peak = max(peak, read())
	if peak < base {
		return 0
	}
	return peak - base
}

// allocatedBy returns the bytes fn allocated on the heap.
func allocatedBy(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// Tagging a 16 MB track must allocate far less than the track: the audio goes
// file to file, only the metadata passes through memory. Reading the file
// into a []byte — the old design — allocates at least its full size.
func TestTaggingMemoryIndependentOfTrackSize(t *testing.T) {
	const size, limit = 16 << 20, 1 << 20
	tags := map[string]interface{}{"title": "Song", "track_number": float64(1)}
	cases := []struct {
		name string
		head []byte
		tag  func(tmp, dir, final string) error
	}{
		{"flac", makeFakeFLAC(), func(tmp, dir, final string) error {
			return tagFLAC(io.Discard, tmp, dir, final, tags, tags, false, false)
		}},
		{"mp3", makeFakeMP3(), func(tmp, dir, final string) error {
			return tagMP3(tmp, dir, final, tags, tags, false, false)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			tmp, final := filepath.Join(dir, ".01.tmp"), filepath.Join(dir, "01."+c.name)
			writeBigAudio(t, tmp, c.head, size)
			var err error
			got := allocatedBy(func() { err = c.tag(tmp, dir, final) })
			if err != nil {
				t.Fatal(err)
			}
			if got > limit {
				t.Errorf("tagging a %d MB track allocated %d KB, want under %d KB", size>>20, got>>10, limit>>10)
			}
			fi, err := os.Stat(final)
			if err != nil || fi.Size() < size {
				t.Errorf("tagged file lost audio: stat %v, err %v", fi, err)
			}
		})
	}
}

func BenchmarkMemTagFLAC(b *testing.B) {
	dir := b.TempDir()
	os.WriteFile(filepath.Join(dir, "cover.jpg"), make([]byte, 300<<10), 0644)
	tags := map[string]interface{}{"title": "Song", "track_number": float64(1)}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		tmp, final := filepath.Join(dir, ".01.tmp"), filepath.Join(dir, "01.flac")
		os.Remove(final)
		writeBigAudio(b, tmp, makeFakeFLAC(), trackSize)
		b.StartTimer()
		if err := tagFLAC(os.Stderr, tmp, dir, final, tags, tags, false, true); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMemTagMP3(b *testing.B) {
	dir := b.TempDir()
	os.WriteFile(filepath.Join(dir, "cover.jpg"), make([]byte, 300<<10), 0644)
	tags := map[string]interface{}{"title": "Song", "track_number": float64(1)}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		tmp, final := filepath.Join(dir, ".01.tmp"), filepath.Join(dir, "01.mp3")
		os.Remove(final)
		writeBigAudio(b, tmp, makeFakeMP3(), trackSize)
		b.StartTimer()
		if err := tagMP3(tmp, dir, final, tags, tags, false, true); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkMemAlbum downloads a 12-track album end to end — HTTP, worker
// pool, tagging, rename — and reports the peak live heap.
func BenchmarkMemAlbum(b *testing.B) {
	var tracks []fakeTrack
	for n := 1; n <= 12; n++ {
		tracks = append(tracks, fakeTrack{ID: 100 + n, Title: fmt.Sprintf("Song %d", n), Number: n, MediaNumber: 1, Performer: "A"})
	}
	var worst uint64
	for i := 0; i < b.N; i++ {
		q := newFakeQobuz(b, tracks)
		q.audioPad = trackSize
		d, dir := newTestDownloader(b, q, nil)
		worst = max(worst, peakHeap(func() {
			if err := d.downloadAlbum(context.Background(), "alb1", dir); err != nil {
				b.Fatal(err)
			}
		}))
	}
	b.ReportMetric(float64(worst)/(1<<20), "peak-heap-MB")
}
