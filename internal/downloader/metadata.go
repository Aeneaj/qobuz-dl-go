package downloader

// metadata.go — FLAC and MP3 tag writers.
// Go does not have a stdlib mutagen equivalent, so we implement:
//   - FLAC: Vorbis Comment block (native FLAC metadata)
//   - MP3:  ID3v2.3 tags
// Both are pure-Go implementations with no external dependencies.

import (
	"bufio"
	"encoding/binary"
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
//
// w is where the missing-cover warning goes. It must be the downloader's
// termOut(), never os.Stdout: this runs from the worker pool with progress
// bars live, and under the TUI nothing may reach the terminal at all.
func tagFLAC(w io.Writer, tmpFile, coverDir, finalFile string, track, album map[string]interface{}, isTrack, embedArt bool) error {
	var cover []byte
	if embedArt {
		var err error
		cover, err = readCover(coverDir)
		if err == nil && len(cover) > maxFLACPicture {
			// The block length is 24 bits: a bigger cover would be written
			// with a wrapped length and corrupt the file.
			err = fmt.Errorf("%d bytes is over the %d a FLAC block holds", len(cover), maxFLACPicture)
			cover = nil
		}
		if err != nil {
			fmt.Fprintf(w, "\033[33mWarning: could not embed cover: %v\033[0m\n", err)
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
		t["GENRE"] = formatGenres(sliceStrings(album, "", "genres_list"))
		t["ALBUMARTIST"] = nestedStr(album, "artist", "name")
		t["TRACKTOTAL"] = fmt.Sprintf("%v", nestedFloat(album, "tracks_count"))
		t["ALBUM"] = nestedStr(album, "", "title")
		if t["ALBUM"] == "" {
			t["ALBUM"], _ = album["title"].(string)
		}
		if rd, _ := album["release_date_original"].(string); rd != "" {
			t["DATE"] = rd
		}
		t["COPYRIGHT"] = formatCopyright(nestedStr(album, "copyright"))
		t["LABEL"] = nestedStr(album, "label", "name")
	}

	return t
}

// flacBlock is a single FLAC metadata block. The last-block flag is not stored
// — encodeFLACHead recomputes it from the position in the slice.
type flacBlock struct {
	blockType byte
	data      []byte
	tail      []byte // written after data but never copied into it: the cover image
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
	blocks, audioAt, err := readFLACBlocks(path, drop...)
	if err != nil {
		return err
	}

	// Vorbis Comment belongs right after STREAMINFO; without one, it goes last.
	vc := flacBlock{blockType: typeVorbisComment, data: buildVorbisComment(tags)}
	if i := slices.IndexFunc(blocks, func(b flacBlock) bool { return b.blockType == typeStreamInfo }); i >= 0 {
		blocks = slices.Insert(blocks, i+1, vc)
	} else {
		blocks = append(blocks, vc)
	}

	if cover != nil {
		blocks = append(blocks, flacBlock{typePicture, flacPictureHeader(len(cover)), cover})
	}
	return replaceHead(path, audioAt, encodeFLACHead(blocks)...)
}

// readFLACBlocks reads the metadata blocks at the head of a FLAC file,
// discarding every block whose type appears in drop, and returns the offset
// where the audio frames begin. Only the metadata is read, never the audio.
// FLAC format: 4-byte magic, then a sequence of metadata blocks.
// Each block: 1-byte type+last_flag, 3-byte length, then data.
func readFLACBlocks(path string, drop ...byte) ([]flacBlock, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	r := bufio.NewReader(f)

	var magic [4]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil || string(magic[:]) != "fLaC" {
		return nil, 0, fmt.Errorf("not a FLAC file: %s", path)
	}
	var blocks []flacBlock
	offset := int64(len(magic))
	for {
		var hdr [4]byte
		if _, err := io.ReadFull(r, hdr[:]); err == io.EOF {
			break // metadata with no audio after it
		} else if err != nil {
			return nil, 0, fmt.Errorf("read FLAC block header: %w", err)
		}
		bType := hdr[0] & 0x7F
		length := int(hdr[1])<<16 | int(hdr[2])<<8 | int(hdr[3])
		if slices.Contains(drop, bType) {
			if _, err := r.Discard(length); err != nil {
				return nil, 0, fmt.Errorf("skip FLAC block: %w", err)
			}
		} else {
			data := make([]byte, length)
			if _, err := io.ReadFull(r, data); err != nil {
				return nil, 0, fmt.Errorf("read FLAC block: %w", err)
			}
			blocks = append(blocks, flacBlock{blockType: bType, data: data})
		}
		offset += int64(len(hdr) + length)
		if hdr[0]&0x80 != 0 {
			break
		}
	}
	return blocks, offset, nil
}

// encodeFLACHead encodes the magic and blocks, flagging the last metadata
// block as required by the format. A block's tail comes back as its own part,
// so the cover image reaches the file without being copied.
func encodeFLACHead(blocks []flacBlock) [][]byte {
	size := 4
	for _, b := range blocks {
		size += 4 + len(b.data)
	}
	out := make([]byte, 0, size)
	out = append(out, "fLaC"...)
	var parts [][]byte
	for i, b := range blocks {
		header := b.blockType
		if i == len(blocks)-1 {
			header |= 0x80
		}
		length := len(b.data) + len(b.tail)
		out = append(out, header,
			byte(length>>16), byte(length>>8), byte(length))
		out = append(out, b.data...)
		if len(b.tail) > 0 {
			parts = append(parts, out, b.tail)
			out = out[len(out):]
		}
	}
	return append(parts, out)
}

// replaceHead replaces everything before audioAt in path with the head parts,
// written in order. The audio
// goes file to file through io.Copy — on Linux copy_file_range, so it never
// enters user space — and memory stays at the size of the metadata however
// big the track is. The result is built next to path and renamed over it
// only once complete: a failed rewrite leaves the original untouched.
func replaceHead(path string, audioAt int64, head ...[]byte) error {
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()
	if _, err := src.Seek(audioAt, io.SeekStart); err != nil {
		return fmt.Errorf("seek to audio: %w", err)
	}

	tmp := path + ".tag"
	dst, err := os.Create(tmp)
	if err != nil {
		return err
	}
	for _, part := range head {
		if err == nil {
			_, err = dst.Write(part)
		}
	}
	if err == nil {
		_, err = io.Copy(dst, src)
	}
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	src.Close() // Windows refuses to rename over an open file
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rewrite %s: %w", filepath.Base(path), err)
	}
	return os.Rename(tmp, path)
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
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(vendorBytes)))
	buf = append(buf, vendorBytes...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(comments)))
	for _, c := range comments {
		buf = binary.LittleEndian.AppendUint32(buf, uint32(len(c)))
		buf = append(buf, c...)
	}
	return buf
}

// maxFLACPicture is the largest cover that fits a PICTURE block: its length
// field is 24 bits and the header fields before the image take 42 bytes.
const maxFLACPicture = 1<<24 - 1 - 42

// flacPictureHeader returns the PICTURE block fields that precede n bytes of
// image data; the image itself is written after it, uncopied.
func flacPictureHeader(n int) []byte {
	mimeType := "image/jpeg"
	desc := ""
	// FLAC picture block layout (all big-endian uint32):
	// picture_type, mime_length, mime, desc_length, desc,
	// width, height, color_depth, color_count, data_length, data
	be := binary.BigEndian
	buf := make([]byte, 0, 32+len(mimeType))
	buf = be.AppendUint32(buf, 3) // Front cover
	buf = be.AppendUint32(buf, uint32(len(mimeType)))
	buf = append(buf, mimeType...)
	buf = be.AppendUint32(buf, uint32(len(desc)))
	buf = append(buf, desc...)
	buf = be.AppendUint32(buf, 0) // width (unknown)
	buf = be.AppendUint32(buf, 0) // height
	buf = be.AppendUint32(buf, 0) // color depth
	buf = be.AppendUint32(buf, 0) // color count
	buf = be.AppendUint32(buf, uint32(n))
	return buf
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
		t["TCON"] = formatGenres(sliceStrings(album, "", "genres_list"))
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

	// The APIC frame goes last so the image can follow the header parts
	// uncopied.
	var img []byte
	if embedArt {
		if data, err := readCover(coverDir); err == nil {
			img = data
			frames = append(frames, apicFrameHeader(len(img))...)
		}
	}

	// ID3v2.3 header: "ID3", version 2.3.0, flags=0, syncsafe size
	size := len(frames) + len(img)
	syncsafe := toSyncsafe(size)
	header := []byte{
		'I', 'D', '3',
		0x03, 0x00, // version 2.3, revision 0
		0x00, // flags
		syncsafe[0], syncsafe[1], syncsafe[2], syncsafe[3],
	}

	audioAt, err := mp3AudioOffset(path)
	if err != nil {
		return err
	}
	return replaceHead(path, audioAt, append(header, frames...), img)
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

// apicFrameHeader returns an APIC frame up to where its n bytes of image
// data begin; the image itself is written after it, uncopied.
func apicFrameHeader(n int) []byte {
	// APIC: encoding(1) + mime(ascii+0x00) + pic_type(1) + desc(0x00) + data
	const prefix = "\x00image/jpeg\x00\x03\x00" // Latin-1, front cover, empty description
	size := len(prefix) + n
	frame := []byte{
		'A', 'P', 'I', 'C',
		byte(size >> 24), byte(size >> 16), byte(size >> 8), byte(size),
		0x00, 0x00,
	}
	return append(frame, prefix...)
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

// mp3AudioOffset returns where the audio starts: past an existing ID3v2 tag,
// or 0 when there is none.
func mp3AudioOffset(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	hdr := make([]byte, 10)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return 0, nil // shorter than an ID3 header — it is all audio
	}
	if hdr[0] != 'I' || hdr[1] != 'D' || hdr[2] != '3' {
		return 0, nil
	}
	size := int64(hdr[6]&0x7F)<<21 | int64(hdr[7]&0x7F)<<14 |
		int64(hdr[8]&0x7F)<<7 | int64(hdr[9]&0x7F)
	return 10 + size, nil
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

func sliceStrings(m map[string]interface{}, subKey, key string) []string {
	var src interface{} = m
	if subKey != "" {
		sub, _ := m[subKey].(map[string]interface{})
		if sub == nil {
			return nil
		}
		src = sub
	}
	mm, _ := src.(map[string]interface{})
	raw, _ := mm[key].([]interface{})
	var result []string
	for _, r := range raw {
		if s, ok := r.(string); ok {
			result = append(result, s)
		}
	}
	return result
}
