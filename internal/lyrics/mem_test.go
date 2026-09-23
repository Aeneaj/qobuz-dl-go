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

// apicFrame builds an ID3v2.3 APIC frame around n bytes of image data.
func apicFrame(n int) []byte {
	payload := append([]byte("\x00image/jpeg\x00\x03\x00"), make([]byte, n)...)
	size := len(payload)
	hdr := []byte{'A', 'P', 'I', 'C', byte(size >> 24), byte(size >> 16), byte(size >> 8), byte(size), 0, 0}
	return append(hdr, payload...)
}

// bigMP3 writes an MP3 whose tag carries an embedded cover of coverSize bytes
// before the text frames — the order other taggers commonly use.
func bigMP3(tb testing.TB, coverSize int) string {
	frames := apicFrame(coverSize)
	frames = append(frames, id3FrameLatin1("TIT2", "Song")...)
	frames = append(frames, id3FrameLatin1("TPE1", "Artist")...)
	frames = append(frames, id3FrameLatin1("TLEN", "180000")...)
	path := filepath.Join(tb.TempDir(), "big.mp3")
	if err := os.WriteFile(path, fakeMP3(frames), 0644); err != nil {
		tb.Fatal(err)
	}
	return path
}

// Reading an MP3's tags must not load its embedded cover: a library scan
// meets one per file, and the old reader pulled the whole tag into memory.
func TestReadMP3MemoryIndependentOfCover(t *testing.T) {
	path := bigMP3(t, 4<<20)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	info, err := ReadAudio(path)
	runtime.ReadMemStats(&after)
	if err != nil || info.Title != "Song" || info.Artist != "Artist" || info.Duration != 180 {
		t.Fatalf("ReadAudio = %+v, %v", info, err)
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > 64<<10 {
		t.Errorf("reading tags past a 4 MB cover allocated %d KB, want under 64 KB", got>>10)
	}
}

func BenchmarkMemReadMP3(b *testing.B) {
	path := bigMP3(b, 1364034) // a real Qobuz original-size cover
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := ReadAudio(path); err != nil {
			b.Fatal(err)
		}
	}
}
