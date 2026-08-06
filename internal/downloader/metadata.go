package downloader

// metadata.go — FLAC and MP3 tag writers.
// Go does not have a stdlib mutagen equivalent, so we implement:
//   - FLAC: Vorbis Comment block (native FLAC metadata)
//   - MP3:  ID3v2.3 tags
// Both are pure-Go implementations with no external dependencies.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf16"
)

// ---- FLAC tagging ----

// FLAC metadata block types (the ones we read or replace).
const (
	typeStreamInfo    = 0
	typeVorbisComment = 4
	typePicture       = 6
)

// tagFLAC writes Vorbis Comment metadata — and, when embedArt is set, the
// cover art — to a FLAC file in a single rewrite, then renames tmpFile →
// finalFile.
func tagFLAC(tmpFile, coverDir, finalFile string, track, album map[string]interface{}, isTrack, embedArt bool) error {
	var cover []byte
	if embedArt {
		var err error
		if cover, err = readCover(coverDir); err != nil {
			fmt.Printf("\033[33mWarning: could not embed cover: %v\033[0m\n", err)
		}
	}
	if err := writeFLACMeta(tmpFile, buildFLACTags(track, album, isTrack), cover); err != nil {
		return err
	}
	return os.Rename(tmpFile, finalFile)
}

func buildFLACTags(track, album map[string]interface{}, isTrack bool) map[string]string {
	t := map[string]string{}

	t["TITLE"] = getTitle(track)
	if tn, ok := track["track_number"].(float64); ok {
		t["TRACKNUMBER"] = fmt.Sprintf("%d", int(tn))
	}
	if mn, ok := track["media_number"].(float64); ok && mn > 1 {
		t["DISCNUMBER"] = fmt.Sprintf("%d", int(mn))
	}
	if composer := nestedStr(track, "composer", "name"); composer != "" {
		t["COMPOSER"] = composer
	}

	performer := nestedStr(track, "performer", "name")
	if isTrack {
		if performer == "" {
			performer = nestedStr(track, "album", "artist", "name")
		}
		t["ARTIST"] = performer
		t["GENRE"] = formatGenres(sliceStrings(track, "album", "genres_list"))
		t["ALBUMARTIST"] = nestedStr(track, "album", "artist", "name")
		t["TRACKTOTAL"] = fmt.Sprintf("%v", nestedFloat(track, "album", "tracks_count"))
		t["ALBUM"] = nestedStr(track, "album", "title")
		t["DATE"] = nestedStr(track, "album", "release_date_original")
		t["COPYRIGHT"] = formatCopyright(nestedStr(track, "copyright"))
		t["LABEL"] = nestedStr(track, "album", "label", "name")
	} else {
		if performer == "" {
			performer = nestedStr(album, "artist", "name")
		}
		t["ARTIST"] = performer
		t["GENRE"] = formatGenres(sliceStrings(album, "genres_list"))
		t["ALBUMARTIST"] = nestedStr(album, "artist", "name")
		t["TRACKTOTAL"] = fmt.Sprintf("%v", nestedFloat(album, "tracks_count"))
		t["ALBUM"] = nestedStr(album, "title")
		if rd, _ := album["release_date_original"].(string); rd != "" {
			t["DATE"] = rd
		}
		t["COPYRIGHT"] = formatCopyright(nestedStr(album, "copyright"))
		t["LABEL"] = nestedStr(album, "label", "name")
	}

	return t
}

// flacBlock is a single FLAC metadata block. The last-block flag is not stored
// — writeFLAC recomputes it from the position in the slice.
type flacBlock struct {
	blockType byte
	data      []byte
}

// writeFLACMeta rewrites the metadata of a FLAC file in one pass: the
// VORBIS_COMMENT block is replaced with tags, and when cover is non-nil the
// existing PICTURE blocks are replaced with it. A nil cover leaves whatever
// artwork the file already carries untouched.
func writeFLACMeta(path string, tags map[string]string, cover []byte) error {
	drop := []byte{typeVorbisComment}
	if cover != nil {
		drop = append(drop, typePicture)
	}
	blocks, audio, err := splitFLAC(path, drop...)
	if err != nil {
		return err
	}

	// Vorbis Comment belongs right after STREAMINFO; without one, it goes last.
	vc := flacBlock{typeVorbisComment, buildVorbisComment(tags)}
	if i := slices.IndexFunc(blocks, func(b flacBlock) bool { return b.blockType == typeStreamInfo }); i >= 0 {
		blocks = slices.Insert(blocks, i+1, vc)
	} else {
		blocks = append(blocks, vc)
	}

	if cover != nil {
		blocks = append(blocks, flacBlock{typePicture, buildFLACPictureBlock(cover)})
	}
	return writeFLAC(path, blocks, audio)
}

// splitFLAC parses a FLAC file into its metadata blocks and the audio data
// that follows, discarding every block whose type appears in drop.
// FLAC format: 4-byte magic, then a sequence of metadata blocks.
// Each block: 1-byte type+last_flag, 3-byte length, then data.
func splitFLAC(path string, drop ...byte) ([]flacBlock, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	if len(data) < 4 || string(data[:4]) != "fLaC" {
		return nil, nil, fmt.Errorf("not a FLAC file: %s", path)
	}

	var blocks []flacBlock
	pos := 4
	for pos+4 <= len(data) {
		header := data[pos]
		isLast := (header & 0x80) != 0
		bType := header & 0x7F
		length := int(data[pos+1])<<16 | int(data[pos+2])<<8 | int(data[pos+3])
		pos += 4
		if pos+length > len(data) {
			break
		}
		if !slices.Contains(drop, bType) {
			blocks = append(blocks, flacBlock{bType, data[pos : pos+length]})
		}
		pos += length
		if isLast {
			break
		}
	}
	return blocks, data[pos:], nil
}

// writeFLAC encodes blocks followed by audio back to path, flagging the last
// metadata block as required by the format.
func writeFLAC(path string, blocks []flacBlock, audio []byte) error {
	out := []byte("fLaC")
	for i, b := range blocks {
		header := b.blockType
		if i == len(blocks)-1 {
			header |= 0x80
		}
		length := len(b.data)
		out = append(out, header,
			byte(length>>16), byte(length>>8), byte(length))
		out = append(out, b.data...)
	}
	out = append(out, audio...)
	return os.WriteFile(path, out, 0644)
}

func buildVorbisComment(tags map[string]string) []byte {
	// vendor string
	vendor := "qobuz-dl"
	vendorBytes := []byte(vendor)

	var comments [][]byte
	for k, v := range tags {
		if v == "" {
			continue
		}
		entry := strings.ToUpper(k) + "=" + v
		comments = append(comments, []byte(entry))
	}

	// Layout: uint32le vendor_length, vendor_string, uint32le count, then each: uint32le len, data
	size := 4 + len(vendorBytes) + 4
	for _, c := range comments {
		size += 4 + len(c)
	}
	buf := make([]byte, 0, size)
	buf = appendU32LE(buf, uint32(len(vendorBytes)))
	buf = append(buf, vendorBytes...)
	buf = appendU32LE(buf, uint32(len(comments)))
	for _, c := range comments {
		buf = appendU32LE(buf, uint32(len(c)))
		buf = append(buf, c...)
	}
	return buf
}

func appendU32LE(b []byte, v uint32) []byte {
	return append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

func buildFLACPictureBlock(imgData []byte) []byte {
	mimeType := "image/jpeg"
	desc := ""
	// FLAC picture block layout (all big-endian uint32):
	// picture_type, mime_length, mime, desc_length, desc,
	// width, height, color_depth, color_count, data_length, data
	buf := make([]byte, 0, 32+len(mimeType)+len(imgData))
	buf = appendU32BE(buf, 3) // Front cover
	buf = appendU32BE(buf, uint32(len(mimeType)))
	buf = append(buf, []byte(mimeType)...)
	buf = appendU32BE(buf, uint32(len(desc)))
	buf = append(buf, []byte(desc)...)
	buf = appendU32BE(buf, 0) // width (unknown)
	buf = appendU32BE(buf, 0) // height
	buf = appendU32BE(buf, 0) // color depth
	buf = appendU32BE(buf, 0) // color count
	buf = appendU32BE(buf, uint32(len(imgData)))
	buf = append(buf, imgData...)
	return buf
}

func appendU32BE(b []byte, v uint32) []byte {
	return append(b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

// readCover loads the cover image next to (or one level above) dir.
func readCover(dir string) ([]byte, error) {
	path := findCover(dir)
	if path == "" {
		return nil, fmt.Errorf("cover not found")
	}
	return os.ReadFile(path)
}

func findCover(dir string) string {
	candidates := []string{
		filepath.Join(dir, "cover.jpg"),
		filepath.Join(filepath.Dir(dir), "cover.jpg"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// ---- MP3 ID3v2.3 tagging ----

// tagMP3 writes ID3v2.3 tags to tmpFile, then renames to finalFile.
func tagMP3(tmpFile, coverDir, finalFile string, track, album map[string]interface{}, isTrack, embedArt bool) error {
	tags := buildMP3Tags(track, album, isTrack)
	if err := writeID3v23(tmpFile, tags, embedArt, coverDir); err != nil {
		return err
	}
	return os.Rename(tmpFile, finalFile)
}

func buildMP3Tags(track, album map[string]interface{}, isTrack bool) map[string]string {
	t := map[string]string{}
	t["TIT2"] = getTitle(track)
	t["TCOM"] = nestedStr(track, "composer", "name")

	performer := nestedStr(track, "performer", "name")
	var trackTotal string
	if isTrack {
		if performer == "" {
			performer = nestedStr(track, "album", "artist", "name")
		}
		t["TCON"] = formatGenres(sliceStrings(track, "album", "genres_list"))
		t["TPE2"] = nestedStr(track, "album", "artist", "name")
		t["TALB"] = nestedStr(track, "album", "title")
		t["TDRC"] = nestedStr(track, "album", "release_date_original")
		t["TCOP"] = formatCopyright(nestedStr(track, "copyright"))
		t["TPUB"] = nestedStr(track, "album", "label", "name")
		trackTotal = fmt.Sprintf("%v", nestedFloat(track, "album", "tracks_count"))
	} else {
		if performer == "" {
			performer = nestedStr(album, "artist", "name")
		}
		t["TCON"] = formatGenres(sliceStrings(album, "genres_list"))
		t["TPE2"] = nestedStr(album, "artist", "name")
		if v, ok := album["title"].(string); ok {
			t["TALB"] = v
		}
		if v, ok := album["release_date_original"].(string); ok {
			t["TDRC"] = v
		}
		t["TCOP"] = formatCopyright(nestedStr(album, "copyright"))
		t["TPUB"] = nestedStr(album, "label", "name")
		trackTotal = fmt.Sprintf("%v", nestedFloat(album, "tracks_count"))
	}
	t["TPE1"] = performer
	if t["TDRC"] != "" && len(t["TDRC"]) >= 4 {
		t["TYER"] = t["TDRC"][:4]
	}

	tn := 0
	if v, ok := track["track_number"].(float64); ok {
		tn = int(v)
	}
	t["TRCK"] = fmt.Sprintf("%d/%s", tn, trackTotal)

	if mn, ok := track["media_number"].(float64); ok {
		t["TPOS"] = fmt.Sprintf("%d", int(mn))
	}
	return t
}

// writeID3v23 prepends an ID3v2.3 tag block to the MP3 file.
func writeID3v23(path string, tags map[string]string, embedArt bool, coverDir string) error {
	// Build frames
	var frames []byte
	for frameID, text := range tags {
		if text == "" {
			continue
		}
		frame := buildTextFrame(frameID, text)
		frames = append(frames, frame...)
	}

	if embedArt {
		if imgData, err := readCover(coverDir); err == nil {
			frames = append(frames, buildAPICFrame(imgData)...)
		}
	}

	// ID3v2.3 header: "ID3", version 2.3.0, flags=0, syncsafe size
	size := len(frames)
	syncsafe := toSyncsafe(size)
	header := []byte{
		'I', 'D', '3',
		0x03, 0x00, // version 2.3, revision 0
		0x00, // flags
		syncsafe[0], syncsafe[1], syncsafe[2], syncsafe[3],
	}

	// Read existing MP3 audio (skip any existing ID3 header)
	audioData, err := readMP3Audio(path)
	if err != nil {
		return err
	}

	// Write: header + frames + audio
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(header); err != nil {
		return fmt.Errorf("write ID3 header: %w", err)
	}
	if _, err := f.Write(frames); err != nil {
		return fmt.Errorf("write ID3 frames: %w", err)
	}
	if _, err := f.Write(audioData); err != nil {
		return fmt.Errorf("write MP3 audio: %w", err)
	}
	return nil
}

func buildTextFrame(id, text string) []byte {
	// Frame: 4-byte ID, 4-byte size (big-endian), 2-byte flags, encoding byte, UTF-16LE BOM + text
	encoded := encodeUTF16LE(text)
	frameData := append([]byte{0x01}, encoded...) // encoding: UTF-16 with BOM
	size := len(frameData)
	frame := []byte{
		id[0], id[1], id[2], id[3],
		byte(size >> 24), byte(size >> 16), byte(size >> 8), byte(size),
		0x00, 0x00, // flags
	}
	return append(frame, frameData...)
}

func buildAPICFrame(imgData []byte) []byte {
	// APIC: encoding(1) + mime(ascii+0x00) + pic_type(1) + desc(0x00 0x00) + data
	mime := "image/jpeg\x00"
	content := append([]byte{0x00}, []byte(mime)...)
	content = append(content, 0x03) // front cover
	content = append(content, 0x00) // empty description (Latin-1)
	content = append(content, imgData...)
	size := len(content)
	frame := []byte{
		'A', 'P', 'I', 'C',
		byte(size >> 24), byte(size >> 16), byte(size >> 8), byte(size),
		0x00, 0x00,
	}
	return append(frame, content...)
}

func encodeUTF16LE(s string) []byte {
	runes := []rune(s)
	encoded := utf16.Encode(runes)
	// BOM: FF FE
	b := []byte{0xFF, 0xFE}
	for _, r := range encoded {
		b = append(b, byte(r), byte(r>>8))
	}
	// null terminator
	b = append(b, 0x00, 0x00)
	return b
}

func toSyncsafe(n int) [4]byte {
	var b [4]byte
	b[3] = byte(n & 0x7F)
	b[2] = byte((n >> 7) & 0x7F)
	b[1] = byte((n >> 14) & 0x7F)
	b[0] = byte((n >> 21) & 0x7F)
	return b
}

func readMP3Audio(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Check for existing ID3 header
	hdr := make([]byte, 10)
	if _, err := io.ReadFull(f, hdr); err != nil {
		// File shorter than 10 bytes — treat the whole thing as audio.
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, fmt.Errorf("seek to start: %w", err)
		}
		return io.ReadAll(f)
	}
	if hdr[0] == 'I' && hdr[1] == 'D' && hdr[2] == '3' {
		// Parse syncsafe size to skip the tag
		size := int(hdr[6]&0x7F)<<21 | int(hdr[7]&0x7F)<<14 |
			int(hdr[8]&0x7F)<<7 | int(hdr[9]&0x7F)
		if _, err := f.Seek(int64(10+size), io.SeekStart); err != nil {
			return nil, fmt.Errorf("seek past ID3 tag: %w", err)
		}
	} else {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, fmt.Errorf("seek to start: %w", err)
		}
	}
	return io.ReadAll(f)
}

// ---- shared tag helpers ----

func formatCopyright(s string) string {
	s = strings.ReplaceAll(s, "(P)", "\u2117")
	s = strings.ReplaceAll(s, "(C)", "\u00a9")
	return s
}

func formatGenres(genres []string) string {
	// API returns e.g. ["Pop/Rock", "Pop/Rock→Rock", "Pop/Rock→Rock→Alternatif"]
	// We want unique leaf tokens
	var all []string
	for _, g := range genres {
		parts := strings.FieldsFunc(g, func(r rune) bool {
			return r == '/' || r == '\u2192'
		})
		all = append(all, parts...)
	}
	seen := map[string]bool{}
	var unique []string
	for _, p := range all {
		p = strings.TrimSpace(p)
		if p != "" && !seen[p] {
			seen[p] = true
			unique = append(unique, p)
		}
	}
	return strings.Join(unique, ", ")
}

// sliceStrings walks nested maps and returns the string entries of the list at
// the end of the path, skipping anything that is not a string.
func sliceStrings(m map[string]interface{}, keys ...string) []string {
	var cur interface{} = m
	for _, k := range keys {
		mm, ok := cur.(map[string]interface{})
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	raw, _ := cur.([]interface{})
	var result []string
	for _, r := range raw {
		if s, ok := r.(string); ok {
			result = append(result, s)
		}
	}
	return result
}

// nestedFloat walks nested maps and returns the float64 at the end of the
// path, or 0 if any step is missing or has the wrong type. Counterpart of
// nestedStr.
func nestedFloat(m map[string]interface{}, keys ...string) float64 {
	var cur interface{} = m
	for _, k := range keys {
		mm, ok := cur.(map[string]interface{})
		if !ok {
			return 0
		}
		cur = mm[k]
	}
	v, _ := cur.(float64)
	return v
}
