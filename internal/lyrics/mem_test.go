package lyrics

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Reading tags must cost the header, not the file: `lyrics` walks a whole
// library, and 24/192 tracks run 150–200 MB each.
//
//	go test ./internal/lyrics -run '^$' -bench Mem -benchmem

// bigFLAC writes a tagged FLAC padded with audio up to size bytes.
func bigFLAC(tb testing.TB, size int64) string {
	path := filepath.Join(tb.TempDir(), "big.flac")
	if err := os.WriteFile(path, fakeFLAC(44100, 44100*300, map[string]string{"TITLE": "Song"}), 0644); err != nil {
		tb.Fatal(err)
	}
	if err := os.Truncate(path, size); err != nil {
		tb.Fatal(err)
	}
	return path
}

func TestReadFLACMemoryIndependentOfFileSize(t *testing.T) {
	path := bigFLAC(t, 16<<20)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	info, err := ReadAudio(path)
	runtime.ReadMemStats(&after)
	if err != nil || info.Title != "Song" || info.Duration != 300 {
		t.Fatalf("ReadAudio = %+v, %v", info, err)
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > 1<<20 {
		t.Errorf("reading tags of a 16 MB file allocated %d KB, want under 1024 KB", got>>10)
	}
}

func BenchmarkMemReadFLAC(b *testing.B) {
	path := bigFLAC(b, 50<<20)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := ReadAudio(path); err != nil {
			b.Fatal(err)
		}
	}
}
