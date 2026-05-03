package lyrics

import (
	"encoding/binary"
	"os"
	"testing"
)

// ---- fake-file builders -------------------------------------------------

// fakeFLAC returns a minimal valid FLAC byte slice.
// sampleRate (Hz) and totalSamples determine the reported duration.
// tags are written as a VORBIS_COMMENT block; pass nil for no block.
func fakeFLAC(sampleRate int, totalSamples int64, tags map[string]string) []byte {
	// STREAMINFO (34 bytes): min/max blocksize+framesize = 0, then packed fields.
	si := make([]byte, 34)
	si[10] = byte(sampleRate >> 12)
	si[11] = byte((sampleRate >> 4) & 0xFF)
	si[12] = byte((sampleRate&0x0F)<<4) | 0x02    // channels-1=1 (stereo), bps-1 bit4=0
	si[13] = 0xF0 | byte((totalSamples>>32)&0x0F) // bps-1 bits 3-0=1111 (16 bit), total hi
	binary.BigEndian.PutUint32(si[14:18], uint32(totalSamples))

	var out []byte
	out = append(out, 'f', 'L', 'a', 'C')

	if len(tags) == 0 {
		// Only STREAMINFO, mark it as last block.
		out = append(out, 0x80, 0x00, 0x00, 0x22) // last=1, type=0, len=34
		out = append(out, si...)
	} else {
		// STREAMINFO (not last) + VORBIS_COMMENT (last)
		out = append(out, 0x00, 0x00, 0x00, 0x22)
		out = append(out, si...)

		vc := buildVorbisCommentBytes(tags)
		vcLen := len(vc)
		out = append(out, byte(0x84), byte(vcLen>>16), byte(vcLen>>8), byte(vcLen))
		out = append(out, vc...)
	}
	// Append a fake audio frame so the file isn't obviously truncated.
	out = append(out, 0xFF, 0xF8, 0x00, 0x00)
	return out
}

func buildVorbisCommentBytes(tags map[string]string) []byte {
	vendor := []byte("test")
	var entries [][]byte
	for k, v := range tags {
		entry := make([]byte, 0, len(k)+1+len(v))
		for _, c := range k {
			if c >= 'a' && c <= 'z' {
				entry = append(entry, byte(c-32))
			} else {
				entry = append(entry, byte(c))
			}
		}
		entry = append(entry, '=')
		entry = append(entry, []byte(v)...)
		entries = append(entries, entry)
	}

	var buf []byte
	buf = appendU32LE(buf, uint32(len(vendor)))
	buf = append(buf, vendor...)
	buf = appendU32LE(buf, uint32(len(entries)))
	for _, e := range entries {
		buf = appendU32LE(buf, uint32(len(e)))
		buf = append(buf, e...)
	}
	return buf
}

func appendU32LE(b []byte, v uint32) []byte {
	return append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

// fakeMP3 returns a minimal MP3 file: ID3v2.3 header + provided frames + fake audio.
func fakeMP3(frames []byte) []byte {
	size := len(frames)
	ss := id3Syncsafe(size)
	header := []byte{'I', 'D', '3', 0x03, 0x00, 0x00, ss[0], ss[1], ss[2], ss[3]}
	// MPEG1 Layer3, 128 kbps, 44100 Hz, stereo.
	audio := []byte{0xFF, 0xFB, 0x90, 0x00, 0x00, 0x00, 0x00, 0x00}
	var out []byte
	out = append(out, header...)
	out = append(out, frames...)
	out = append(out, audio...)
	return out
}

func id3Syncsafe(n int) [4]byte {
	var b [4]byte
	b[3] = byte(n & 0x7F)
	b[2] = byte((n >> 7) & 0x7F)
	b[1] = byte((n >> 14) & 0x7F)
	b[0] = byte((n >> 21) & 0x7F)
	return b
}

// id3FrameLatin1 builds an ID3v2.3 text frame with Latin-1 encoding.
func id3FrameLatin1(id, text string) []byte {
	payload := append([]byte{0x00}, []byte(text)...)
	payload = append(payload, 0x00)
	size := len(payload)
	hdr := []byte{id[0], id[1], id[2], id[3],
		byte(size >> 24), byte(size >> 16), byte(size >> 8), byte(size),
		0x00, 0x00}
	return append(hdr, payload...)
}

// id3FrameUTF16LE builds an ID3v2.3 text frame with UTF-16LE + BOM encoding
// (the same encoding qobuz-dl uses when tagging files).
func id3FrameUTF16LE(id, text string) []byte {
	payload := []byte{0x01, 0xFF, 0xFE} // encoding=UTF-16, BOM LE
	for _, r := range []rune(text) {
		u := uint16(r)
		payload = append(payload, byte(u), byte(u>>8))
	}
	payload = append(payload, 0x00, 0x00) // UTF-16 null terminator
	size := len(payload)
	hdr := []byte{id[0], id[1], id[2], id[3],
		byte(size >> 24), byte(size >> 16), byte(size >> 8), byte(size),
		0x00, 0x00}
	return append(hdr, payload...)
}

func writeTmp(t *testing.T, ext string, data []byte) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "test*"+ext)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
	return f.Name()
}

// ---- FLAC tests ---------------------------------------------------------

func TestReadFLAC_TagsAndDuration(t *testing.T) {
	// 44100 Hz × 180 s = 7 938 000 samples
	data := fakeFLAC(44100, 7938000, map[string]string{
		"title":  "Test Song",
		"artist": "Test Artist",
		"album":  "Test Album",
	})
	info, err := ReadAudio(writeTmp(t, ".flac", data))
	if err != nil {
		t.Fatalf("ReadAudio: %v", err)
	}
	if info.Title != "Test Song" {
		t.Errorf("Title = %q", info.Title)
	}
	if info.Artist != "Test Artist" {
		t.Errorf("Artist = %q", info.Artist)
	}
	if info.Album != "Test Album" {
		t.Errorf("Album = %q", info.Album)
	}
	if info.Duration != 180 {
		t.Errorf("Duration = %d, want 180", info.Duration)
	}
}

func TestReadFLAC_AlbumArtistFallback(t *testing.T) {
	data := fakeFLAC(44100, 44100, map[string]string{
		"title":       "Song",
		"albumartist": "Band",
		// no "artist" tag
	})
	info, err := ReadAudio(writeTmp(t, ".flac", data))
	if err != nil {
		t.Fatalf("ReadAudio: %v", err)
	}
	if info.Artist != "Band" {
		t.Errorf("Artist = %q, want ALBUMARTIST fallback", info.Artist)
	}
}

func TestReadFLAC_ArtistBeatsAlbumArtist(t *testing.T) {
	data := fakeFLAC(44100, 44100, map[string]string{
		"title":       "Song",
		"artist":      "Track Artist",
		"albumartist": "Band",
	})
	info, err := ReadAudio(writeTmp(t, ".flac", data))
	if err != nil {
		t.Fatalf("ReadAudio: %v", err)
	}
	if info.Artist != "Track Artist" {
		t.Errorf("ARTIST should beat ALBUMARTIST, got %q", info.Artist)
	}
}

func TestReadFLAC_InvalidMagic(t *testing.T) {
	path := writeTmp(t, ".flac", []byte("not a flac file at all"))
	_, err := ReadAudio(path)
	if err == nil {
		t.Error("expected error for non-FLAC data")
	}
}

func TestReadFLAC_NoTags(t *testing.T) {
	// FLAC with only STREAMINFO, no VORBIS_COMMENT — should not error.
	data := fakeFLAC(48000, 48000, nil)
	info, err := ReadAudio(writeTmp(t, ".flac", data))
	if err != nil {
		t.Fatalf("ReadAudio: %v", err)
	}
	if info.Duration != 1 {
		t.Errorf("Duration = %d, want 1 (48000/48000)", info.Duration)
	}
}

// ---- MP3 tests ----------------------------------------------------------

func TestReadMP3_Latin1Tags(t *testing.T) {
	frames := id3FrameLatin1("TIT2", "Latin Song")
	frames = append(frames, id3FrameLatin1("TPE1", "Latin Artist")...)
	frames = append(frames, id3FrameLatin1("TALB", "Latin Album")...)
	info, err := ReadAudio(writeTmp(t, ".mp3", fakeMP3(frames)))
	if err != nil {
		t.Fatalf("ReadAudio: %v", err)
	}
	if info.Title != "Latin Song" {
		t.Errorf("Title = %q", info.Title)
	}
	if info.Artist != "Latin Artist" {
		t.Errorf("Artist = %q", info.Artist)
	}
	if info.Album != "Latin Album" {
		t.Errorf("Album = %q", info.Album)
	}
}

func TestReadMP3_UTF16LEFrames(t *testing.T) {
	// qobuz-dl tags all text frames with UTF-16LE + BOM.
	frames := id3FrameUTF16LE("TIT2", "UTF-16 Song")
	frames = append(frames, id3FrameUTF16LE("TPE1", "UTF-16 Artist")...)
	frames = append(frames, id3FrameUTF16LE("TALB", "UTF-16 Album")...)
	info, err := ReadAudio(writeTmp(t, ".mp3", fakeMP3(frames)))
	if err != nil {
		t.Fatalf("ReadAudio: %v", err)
	}
	if info.Title != "UTF-16 Song" {
		t.Errorf("Title = %q", info.Title)
	}
	if info.Artist != "UTF-16 Artist" {
		t.Errorf("Artist = %q", info.Artist)
	}
}

func TestReadMP3_TLENDuration(t *testing.T) {
	// TLEN stores duration in milliseconds.
	frames := id3FrameLatin1("TIT2", "Song")
	frames = append(frames, id3FrameLatin1("TLEN", "240000")...) // 240 s
	info, err := ReadAudio(writeTmp(t, ".mp3", fakeMP3(frames)))
	if err != nil {
		t.Fatalf("ReadAudio: %v", err)
	}
	if info.Duration != 240 {
		t.Errorf("Duration = %d, want 240", info.Duration)
	}
}

func TestReadMP3_TPE2Fallback(t *testing.T) {
	// TPE2 (album artist) should be used when TPE1 is absent.
	frames := id3FrameLatin1("TIT2", "Song")
	frames = append(frames, id3FrameLatin1("TPE2", "Album Band")...)
	info, err := ReadAudio(writeTmp(t, ".mp3", fakeMP3(frames)))
	if err != nil {
		t.Fatalf("ReadAudio: %v", err)
	}
	if info.Artist != "Album Band" {
		t.Errorf("Artist = %q, want TPE2 fallback", info.Artist)
	}
}

func TestReadMP3_NoID3Tag(t *testing.T) {
	// Bare MPEG bytes — must not error or crash.
	data := []byte{0xFF, 0xFB, 0x90, 0x00, 0x00, 0x00, 0x00, 0x00}
	info, err := ReadAudio(writeTmp(t, ".mp3", data))
	if err != nil {
		t.Fatalf("ReadAudio: %v", err)
	}
	if info.Title != "" || info.Artist != "" {
		t.Errorf("expected empty tags for untagged file; got title=%q artist=%q",
			info.Title, info.Artist)
	}
}

// ---- decodeID3Text unit tests -------------------------------------------

func TestDecodeID3Text_Latin1(t *testing.T) {
	data := []byte{0x00, 'H', 'e', 'l', 'l', 'o', 0x00}
	if got := decodeID3Text(data); got != "Hello" {
		t.Errorf("got %q", got)
	}
}

func TestDecodeID3Text_UTF16LE(t *testing.T) {
	data := []byte{0x01, 0xFF, 0xFE, 'H', 0x00, 'i', 0x00, 0x00, 0x00}
	if got := decodeID3Text(data); got != "Hi" {
		t.Errorf("got %q", got)
	}
}

func TestDecodeID3Text_UTF16BE(t *testing.T) {
	data := []byte{0x01, 0xFE, 0xFF, 0x00, 'H', 0x00, 'i', 0x00, 0x00}
	if got := decodeID3Text(data); got != "Hi" {
		t.Errorf("got %q", got)
	}
}

func TestDecodeID3Text_UTF8(t *testing.T) {
	data := []byte{0x03, 'H', 'e', 'l', 'l', 'o', 0x00}
	if got := decodeID3Text(data); got != "Hello" {
		t.Errorf("got %q", got)
	}
}

func TestDecodeID3Text_EmptyInput(t *testing.T) {
	if got := decodeID3Text(nil); got != "" {
		t.Errorf("nil input: got %q", got)
	}
	if got := decodeID3Text([]byte{}); got != "" {
		t.Errorf("empty input: got %q", got)
	}
}

// ---- ReadAudio unsupported format ---------------------------------------

func TestReadAudio_UnsupportedFormat(t *testing.T) {
	path := writeTmp(t, ".ogg", []byte("ogg data"))
	_, err := ReadAudio(path)
	if err == nil {
		t.Error("expected error for unsupported format")
	}
}
