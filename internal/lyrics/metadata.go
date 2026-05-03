// Package lyrics scans audio files, reads their metadata, and fetches .lrc
// files from the LRCLIB API. All parsing is pure Go — no external deps.
package lyrics

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf16"
)

// AudioInfo holds the metadata extracted from a single audio file.
type AudioInfo struct {
	Path     string
	Title    string
	Artist   string
	Album    string
	Duration int // seconds; 0 if unknown
}

// ReadAudio reads metadata from a FLAC or MP3 file.
func ReadAudio(path string) (AudioInfo, error) {
	info := AudioInfo{Path: path}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".flac":
		return readFLAC(path, info)
	case ".mp3":
		return readMP3(path, info)
	default:
		return info, fmt.Errorf("unsupported format %q", filepath.Ext(path))
	}
}

// ---- FLAC ---------------------------------------------------------------

func readFLAC(path string, info AudioInfo) (AudioInfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return info, err
	}
	if len(data) < 4 || string(data[:4]) != "fLaC" {
		return info, fmt.Errorf("%s: not a FLAC file", filepath.Base(path))
	}

	pos := 4
	for pos+4 <= len(data) {
		hdr := data[pos]
		isLast := hdr&0x80 != 0
		bType := hdr & 0x7F
		bLen := int(data[pos+1])<<16 | int(data[pos+2])<<8 | int(data[pos+3])
		pos += 4
		if pos+bLen > len(data) {
			break
		}
		block := data[pos : pos+bLen]
		pos += bLen

		switch bType {
		case 0: // STREAMINFO — parse sample_rate and total_samples for duration
			if bLen >= 18 {
				sr := int(block[10])<<12 | int(block[11])<<4 | int(block[12])>>4
				total := int64(block[13]&0x0F)<<32 | int64(binary.BigEndian.Uint32(block[14:18]))
				if sr > 0 {
					info.Duration = int(total / int64(sr))
				}
			}
		case 4: // VORBIS_COMMENT
			parseFLACVorbisComment(block, &info)
		}

		if isLast {
			break
		}
	}
	return info, nil
}

func parseFLACVorbisComment(data []byte, info *AudioInfo) {
	if len(data) < 8 {
		return
	}
	vendorLen := int(binary.LittleEndian.Uint32(data[0:4]))
	pos := 4 + vendorLen
	if pos+4 > len(data) {
		return
	}
	count := int(binary.LittleEndian.Uint32(data[pos : pos+4]))
	pos += 4

	// Collect separately; ARTIST takes precedence over ALBUMARTIST.
	var artist, albumArtist string

	for i := 0; i < count && pos+4 <= len(data); i++ {
		cLen := int(binary.LittleEndian.Uint32(data[pos : pos+4]))
		pos += 4
		if pos+cLen > len(data) {
			break
		}
		comment := string(data[pos : pos+cLen])
		pos += cLen

		eq := strings.IndexByte(comment, '=')
		if eq < 0 {
			continue
		}
		key := strings.ToUpper(comment[:eq])
		val := comment[eq+1:]
		switch key {
		case "TITLE":
			info.Title = val
		case "ARTIST":
			artist = val
		case "ALBUMARTIST":
			albumArtist = val
		case "ALBUM":
			info.Album = val
		}
	}
	if artist != "" {
		info.Artist = artist
	} else {
		info.Artist = albumArtist
	}
}

// ---- MP3 ----------------------------------------------------------------

func readMP3(path string, info AudioInfo) (AudioInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return info, err
	}
	defer f.Close()

	hdr := make([]byte, 10)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return info, nil
	}

	id3End := int64(0)
	if hdr[0] == 'I' && hdr[1] == 'D' && hdr[2] == '3' {
		id3Version := hdr[3]
		size := int(hdr[6]&0x7F)<<21 | int(hdr[7]&0x7F)<<14 |
			int(hdr[8]&0x7F)<<7 | int(hdr[9]&0x7F)
		id3End = 10 + int64(size)
		if id3Version >= 4 && hdr[5]&0x10 != 0 {
			id3End += 10 // ID3v2.4 footer
		}

		tagData := make([]byte, size)
		if _, err := io.ReadFull(f, tagData); err == nil {
			tlenMs := parseID3Frames(tagData, &info, id3Version)
			if tlenMs > 0 {
				info.Duration = tlenMs / 1000
			}
		}
	} else {
		f.Seek(0, io.SeekStart)
	}

	if info.Duration == 0 {
		info.Duration = readXingDuration(f, id3End)
	}
	return info, nil
}

func parseID3Frames(data []byte, info *AudioInfo, version byte) int {
	var title, artist, albumArtist, album, tlenStr string

	pos := 0
	for pos+10 <= len(data) {
		frameID := string(data[pos : pos+4])
		if frameID == "\x00\x00\x00\x00" {
			break // padding reached
		}
		var frameSize int
		if version >= 4 {
			frameSize = int(data[pos+4]&0x7F)<<21 | int(data[pos+5]&0x7F)<<14 |
				int(data[pos+6]&0x7F)<<7 | int(data[pos+7]&0x7F)
		} else {
			frameSize = int(data[pos+4])<<24 | int(data[pos+5])<<16 |
				int(data[pos+6])<<8 | int(data[pos+7])
		}
		pos += 10
		if frameSize <= 0 || pos+frameSize > len(data) {
			break
		}
		fd := data[pos : pos+frameSize]
		pos += frameSize

		switch frameID {
		case "TIT2":
			title = decodeID3Text(fd)
		case "TPE1":
			artist = decodeID3Text(fd)
		case "TPE2":
			albumArtist = decodeID3Text(fd)
		case "TALB":
			album = decodeID3Text(fd)
		case "TLEN":
			tlenStr = decodeID3Text(fd)
		}
	}

	info.Title = title
	if artist != "" {
		info.Artist = artist
	} else {
		info.Artist = albumArtist
	}
	info.Album = album

	ms, _ := strconv.Atoi(strings.TrimSpace(tlenStr))
	return ms
}

func decodeID3Text(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	enc := data[0]
	payload := data[1:]

	stripNulUTF8 := func(s string) string { return strings.TrimRight(s, "\x00") }

	switch enc {
	case 0x01: // UTF-16 with BOM
		if len(payload) < 2 {
			return ""
		}
		bigEndian := payload[0] == 0xFE && payload[1] == 0xFF
		// advance past BOM if present (FF FE or FE FF)
		if (payload[0] == 0xFF && payload[1] == 0xFE) || bigEndian {
			payload = payload[2:]
		}
		// strip trailing UTF-16 null terminator
		for len(payload) >= 2 && payload[len(payload)-2] == 0 && payload[len(payload)-1] == 0 {
			payload = payload[:len(payload)-2]
		}
		if len(payload)%2 != 0 && len(payload) > 0 {
			payload = payload[:len(payload)-1]
		}
		u16 := make([]uint16, len(payload)/2)
		for i := range u16 {
			if bigEndian {
				u16[i] = uint16(payload[2*i])<<8 | uint16(payload[2*i+1])
			} else {
				u16[i] = uint16(payload[2*i]) | uint16(payload[2*i+1])<<8
			}
		}
		return string(utf16.Decode(u16))

	case 0x02: // UTF-16BE without BOM
		for len(payload) >= 2 && payload[len(payload)-2] == 0 && payload[len(payload)-1] == 0 {
			payload = payload[:len(payload)-2]
		}
		if len(payload)%2 != 0 && len(payload) > 0 {
			payload = payload[:len(payload)-1]
		}
		u16 := make([]uint16, len(payload)/2)
		for i := range u16 {
			u16[i] = uint16(payload[2*i])<<8 | uint16(payload[2*i+1])
		}
		return string(utf16.Decode(u16))

	case 0x03: // UTF-8
		return stripNulUTF8(string(payload))

	default: // 0x00 Latin-1
		return stripNulUTF8(string(payload))
	}
}

// ---- MPEG duration helpers ----------------------------------------------

// readXingDuration tries the Xing/Info VBR header in the first MPEG audio
// frame. Falls back to a CBR estimate from the bitrate + file size.
// f must be seekable; id3End is the byte offset where audio data begins.
func readXingDuration(f *os.File, id3End int64) int {
	if _, err := f.Seek(id3End, io.SeekStart); err != nil {
		return 0
	}
	buf := make([]byte, 4096)
	n, _ := f.Read(buf)
	if n < 4 {
		return 0
	}

	// Locate first MPEG sync word: 0xFF followed by 0xE? (top 3 bits set).
	start := -1
	for i := 0; i+1 < n; i++ {
		if buf[i] == 0xFF && buf[i+1]&0xE0 == 0xE0 {
			start = i
			break
		}
	}
	if start < 0 || start+200 > n {
		return 0
	}
	fhdr := buf[start : start+4]

	mpegVer := (fhdr[1] >> 3) & 0x03 // 3=MPEG1, 2=MPEG2, 0=MPEG2.5
	chanMode := (fhdr[3] >> 6) & 0x03 // 3=mono

	// Side information size determines where Xing header sits.
	var sideInfo int
	switch {
	case mpegVer == 3 && chanMode != 3:
		sideInfo = 32 // MPEG1 stereo
	case mpegVer == 3:
		sideInfo = 17 // MPEG1 mono
	case chanMode != 3:
		sideInfo = 17 // MPEG2/2.5 stereo
	default:
		sideInfo = 9 // MPEG2/2.5 mono
	}

	xOff := start + 4 + sideInfo
	if xOff+12 > n {
		return estimateCBRDuration(f, id3End, fhdr)
	}
	tag := string(buf[xOff : xOff+4])
	if tag != "Xing" && tag != "Info" {
		return estimateCBRDuration(f, id3End, fhdr)
	}

	flags := binary.BigEndian.Uint32(buf[xOff+4 : xOff+8])
	if flags&0x01 == 0 {
		return 0 // total frame count not present in Xing header
	}
	totalFrames := int(binary.BigEndian.Uint32(buf[xOff+8 : xOff+12]))

	sr := mpegSampleRate(fhdr)
	if sr == 0 {
		return 0
	}
	spf := 1152 // MPEG1 Layer3: 1152 samples/frame
	if mpegVer != 3 {
		spf = 576 // MPEG2/2.5 Layer3: 576 samples/frame
	}
	return totalFrames * spf / sr
}

func estimateCBRDuration(f *os.File, id3End int64, fhdr []byte) int {
	br := mpegBitrate(fhdr)
	if br == 0 {
		return 0
	}
	fi, err := f.Stat()
	if err != nil {
		return 0
	}
	audioBytes := fi.Size() - id3End
	if audioBytes <= 0 {
		return 0
	}
	return int(audioBytes * 8 / int64(br))
}

// MPEG sample rate table indexed by [version_bits][sr_index].
var mpegSampleRateTable = [4][4]int{
	{11025, 12000, 8000, 0},  // MPEG2.5
	{0, 0, 0, 0},             // reserved
	{22050, 24000, 16000, 0}, // MPEG2
	{44100, 48000, 32000, 0}, // MPEG1
}

func mpegSampleRate(hdr []byte) int {
	if len(hdr) < 4 {
		return 0
	}
	return mpegSampleRateTable[(hdr[1]>>3)&0x03][(hdr[2]>>2)&0x03]
}

// MPEG Layer3 bitrate table (bps) indexed by [mpeg1/mpeg2][index].
var mpegBitrateTable = [2][16]int{
	{0, 32000, 40000, 48000, 56000, 64000, 80000, 96000, 112000, 128000, 160000, 192000, 224000, 256000, 320000, 0},
	{0, 8000, 16000, 24000, 32000, 40000, 48000, 56000, 64000, 80000, 96000, 112000, 128000, 144000, 160000, 0},
}

func mpegBitrate(hdr []byte) int {
	if len(hdr) < 3 {
		return 0
	}
	row := 1
	if (hdr[1]>>3)&0x03 == 3 { // MPEG1
		row = 0
	}
	return mpegBitrateTable[row][(hdr[2]>>4)&0x0F]
}
