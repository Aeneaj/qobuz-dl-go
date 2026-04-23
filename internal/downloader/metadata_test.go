package downloader

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- FLAC tagging tests ----

func TestWriteFLACTags_RoundTrip(t *testing.T) {
	// Create a minimal valid FLAC file (just header + STREAMINFO block)
	flac := makeFakeFLAC()
	tmp := tempFile(t, "*.flac")
	if err := os.WriteFile(tmp, flac, 0644); err != nil {
		t.Fatal(err)
	}

	tags := map[string]string{
		"TITLE":       "Test Track",
		"ARTIST":      "Test Artist",
		"ALBUM":       "Test Album",
		"TRACKNUMBER": "3",
		"DATE":        "2024-04-01",
	}
	if err := writeFLACTags(tmp, tags); err != nil {
		t.Fatalf("writeFLACTags: %v", err)
	}

	// Read back and verify the Vorbis Comment block is present
	data, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if string(data[:4]) != "fLaC" {
		t.Fatal("output is not a FLAC file")
	}

	// Walk blocks looking for VORBIS_COMMENT (type 4)
	found := false
	pos := 4
	for pos < len(data) {
		if pos+4 > len(data) {
			break
		}
		header := data[pos]
		isLast := (header & 0x80) != 0
		bType := header & 0x7F
		length := int(data[pos+1])<<16 | int(data[pos+2])<<8 | int(data[pos+3])
		pos += 4
		if bType == 4 {
			found = true
			// Check that "TITLE=Test Track" is in the block
			block := string(data[pos : pos+length])
			if !contains(block, "TITLE=Test Track") {
				t.Errorf("TITLE tag not found in Vorbis Comment block")
			}
			if !contains(block, "ARTIST=Test Artist") {
				t.Errorf("ARTIST tag not found in Vorbis Comment block")
			}
		}
		pos += length
		if isLast {
			break
		}
	}
	if !found {
		t.Error("no VORBIS_COMMENT block found in output")
	}
}

func TestWriteFLACTags_NotFLAC(t *testing.T) {
	tmp := tempFile(t, "*.flac")
	os.WriteFile(tmp, []byte("not a flac file"), 0644)
	err := writeFLACTags(tmp, map[string]string{"TITLE": "x"})
	if err == nil {
		t.Error("expected error for non-FLAC file")
	}
}

// ---- ID3 tagging tests ----

func TestWriteID3v23_HasHeader(t *testing.T) {
	// A minimal fake MP3 (just some bytes)
	tmp := tempFile(t, "*.mp3")
	os.WriteFile(tmp, []byte("\xff\xfb\x90\x00test audio data"), 0644)

	tags := map[string]string{
		"TIT2": "My Track",
		"TPE1": "My Artist",
		"TALB": "My Album",
		"TRCK": "1/10",
	}
	if err := writeID3v23(tmp, tags, false, ""); err != nil {
		t.Fatalf("writeID3v23: %v", err)
	}

	data, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 3 || string(data[:3]) != "ID3" {
		t.Error("output does not start with ID3 header")
	}
	if data[3] != 0x03 {
		t.Errorf("expected ID3v2.3 (0x03), got 0x%02x", data[3])
	}
}

func TestWriteID3v23_SkipsExistingID3(t *testing.T) {
	// Write a file with a fake existing ID3 header to verify it gets replaced
	tmp := tempFile(t, "*.mp3")
	// Fake ID3: "ID3" + v2.3 + flags + syncsafe size 20 + 20 bytes of padding + audio
	fakeID3 := make([]byte, 30+12)
	copy(fakeID3, []byte{'I', 'D', '3', 0x03, 0x00, 0x00})
	// syncsafe 20 = 0 0 0 20
	fakeID3[6], fakeID3[7], fakeID3[8], fakeID3[9] = 0, 0, 0, 20
	copy(fakeID3[30:], []byte("\xff\xfbtest audio"))
	os.WriteFile(tmp, fakeID3, 0644)

	if err := writeID3v23(tmp, map[string]string{"TIT2": "New"}, false, ""); err != nil {
		t.Fatalf("writeID3v23: %v", err)
	}
	data, _ := os.ReadFile(tmp)
	if string(data[:3]) != "ID3" {
		t.Error("expected ID3 header")
	}
	// Audio should still be present
	if !containsBytes(data, []byte("\xff\xfbtest audio")) {
		t.Error("audio data was lost")
	}
}

// ---- Format helpers tests ----

func TestFormatGenres(t *testing.T) {
	genres := []string{
		"Pop/Rock",
		"Pop/Rock→Rock",
		"Pop/Rock→Rock→Alternatif et Indé",
	}
	result := formatGenres(genres)
	if result != "Pop, Rock, Alternatif et Indé" {
		t.Errorf("unexpected genres: %q", result)
	}
}

func TestFormatGenres_Empty(t *testing.T) {
	result := formatGenres(nil)
	if result != "" {
		t.Errorf("expected empty, got %q", result)
	}
}

func TestFormatCopyright(t *testing.T) {
	tests := []struct{ in, want string }{
		{"(P) 2024 Label", "\u2117 2024 Label"},
		{"(C) 2024 Artist", "\u00a9 2024 Artist"},
		{"no symbols", "no symbols"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := formatCopyright(tt.in); got != tt.want {
			t.Errorf("formatCopyright(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestBuildFLACTags_IsTrack(t *testing.T) {
	track := map[string]interface{}{
		"title":        "My Song",
		"track_number": float64(4),
		"media_number": float64(1),
		"performer":    map[string]interface{}{"name": "Performer"},
		"copyright":    "(P) 2024",
		"album": map[string]interface{}{
			"title":                 "My Album",
			"artist":                map[string]interface{}{"name": "Album Artist"},
			"release_date_original": "2024-01-15",
			"tracks_count":          float64(12),
			"genres_list":           []interface{}{"Pop/Rock"},
			"label":                 map[string]interface{}{"name": "Best Label"},
		},
	}
	tags := buildFLACTags(track, nil, true)

	check := func(key, want string) {
		t.Helper()
		if got := tags[key]; got != want {
			t.Errorf("tag %s = %q, want %q", key, got, want)
		}
	}
	check("TITLE", "My Song")
	check("TRACKNUMBER", "4")
	check("ARTIST", "Performer")
	check("ALBUMARTIST", "Album Artist")
	check("ALBUM", "My Album")
	check("DATE", "2024-01-15")
	check("TRACKTOTAL", "12")
	check("LABEL", "Best Label")
	check("COPYRIGHT", "\u2117 2024")
}

func TestBuildFLACTags_WithVersion(t *testing.T) {
	track := map[string]interface{}{
		"title":   "Track",
		"version": "Remastered",
		"album":   map[string]interface{}{},
	}
	tags := buildFLACTags(track, nil, true)
	if tags["TITLE"] != "Track (Remastered)" {
		t.Errorf("version not appended: %q", tags["TITLE"])
	}
}

// ---- URL parsing tests ----

func TestParseQobuzURL(t *testing.T) {
	tests := []struct {
		url      string
		wantType string
		wantID   string
	}{
		{"https://play.qobuz.com/album/abc123", "album", "abc123"},
		{"https://play.qobuz.com/track/xyz789", "track", "xyz789"},
		{"https://open.qobuz.com/artist/111222", "artist", "111222"},
		{"https://www.qobuz.com/us-en/album/some-title/abc999", "album", "abc999"},
		{"https://play.qobuz.com/playlist/pl001", "playlist", "pl001"},
		{"https://play.qobuz.com/label/lb001", "label", "lb001"},
	}
	for _, tt := range tests {
		gotType, gotID, err := parseQobuzURL(tt.url)
		if err != nil {
			t.Errorf("parseQobuzURL(%q): unexpected error: %v", tt.url, err)
			continue
		}
		if gotType != tt.wantType {
			t.Errorf("parseQobuzURL(%q) type = %q, want %q", tt.url, gotType, tt.wantType)
		}
		if gotID != tt.wantID {
			t.Errorf("parseQobuzURL(%q) id = %q, want %q", tt.url, gotID, tt.wantID)
		}
	}
}

func TestParseQobuzURL_Invalid(t *testing.T) {
	_, _, err := parseQobuzURL("https://spotify.com/track/123") // should fail: not qobuz.com
	if err == nil {
		t.Error("expected error for non-Qobuz URL")
	}
}

// ---- Smart discography filter tests ----

func TestSmartDiscogFilter_RemovesDuplicates(t *testing.T) {
	items := []map[string]interface{}{
		{
			"title":                 "Abbey Road",
			"artist":                map[string]interface{}{"name": "The Beatles"},
			"maximum_bit_depth":     float64(16),
			"maximum_sampling_rate": float64(44.1),
			"id":                    "1",
		},
		{
			"title":                 "Abbey Road (Remastered)",
			"artist":                map[string]interface{}{"name": "The Beatles"},
			"maximum_bit_depth":     float64(24),
			"maximum_sampling_rate": float64(96.0),
			"id":                    "2",
		},
	}
	filtered := smartDiscogFilter("The Beatles", items)
	if len(filtered) != 1 {
		t.Errorf("expected 1 item after filter, got %d", len(filtered))
	}
	// Should keep the hi-res remaster
	if filtered[0]["id"] != "2" {
		t.Errorf("expected hi-res version to win, got id=%v", filtered[0]["id"])
	}
}

func TestSmartDiscogFilter_ExcludesOtherArtists(t *testing.T) {
	items := []map[string]interface{}{
		{
			"title":                 "Solo Album",
			"artist":                map[string]interface{}{"name": "The Beatles"},
			"maximum_bit_depth":     float64(16),
			"maximum_sampling_rate": float64(44.1),
			"id":                    "1",
		},
		{
			"title":                 "Featured Album",
			"artist":                map[string]interface{}{"name": "Other Artist"},
			"maximum_bit_depth":     float64(16),
			"maximum_sampling_rate": float64(44.1),
			"id":                    "2",
		},
	}
	filtered := smartDiscogFilter("The Beatles", items)
	for _, f := range filtered {
		if nestedStr(f, "artist", "name") != "The Beatles" {
			t.Errorf("other artist slipped through: %v", f["title"])
		}
	}
}

// ---- truncateStr test ----

func TestTruncateStr(t *testing.T) {
	// Short string — should be padded to width
	s := truncateStr("Hi", 10)
	if len([]rune(s)) != 10 {
		t.Errorf("expected width 10, got %d: %q", len([]rune(s)), s)
	}
	// Exact length — no change
	s = truncateStr("1234567890", 10)
	if s != "1234567890" {
		t.Errorf("exact length case wrong: %q", s)
	}
	// Too long — should be truncated with ellipsis
	s = truncateStr("This is a long string", 10)
	if len([]rune(s)) != 10 {
		t.Errorf("truncated width expected 10, got %d: %q", len([]rune(s)), s)
	}
	if !strings.HasSuffix(s, "…") {
		t.Errorf("truncated string should end with ellipsis: %q", s)
	}
}

// ---- GetTitle test ----

func TestGetTitle(t *testing.T) {
	tests := []struct {
		item map[string]interface{}
		want string
	}{
		{map[string]interface{}{"title": "Song"}, "Song"},
		{map[string]interface{}{"title": "Song", "version": "Live"}, "Song (Live)"},
		{map[string]interface{}{"title": "Song (Live)", "version": "Live"}, "Song (Live)"},
	}
	for _, tt := range tests {
		if got := getTitle(tt.item); got != tt.want {
			t.Errorf("getTitle(%v) = %q, want %q", tt.item, got, tt.want)
		}
	}
}

// ---- CleanFormatStr test ----

func TestCleanFormatStr(t *testing.T) {
	tests := []struct {
		format, fileFormat, want string
	}{
		{"{artist} - {album}.flac", "FLAC", "{artist} - {album}"},
		{"{artist} - {album}.mp3", "MP3", "{artist} - {album}"},
		{"{artist} - {album} [{bit_depth}B]", "MP3", "{artist} - {album} ({year}) [MP3]"},
		{"{artist} - {album}", "FLAC", "{artist} - {album}"},
	}
	for _, tt := range tests {
		if got := cleanFormatStr(tt.format, tt.fileFormat); got != tt.want {
			t.Errorf("cleanFormatStr(%q, %q) = %q, want %q", tt.format, tt.fileFormat, got, tt.want)
		}
	}
}

// ---- helpers ----

func makeFakeFLAC() []byte {
	// fLaC magic + minimal STREAMINFO block (type=0, last=true, length=34)
	streaminfo := make([]byte, 34) // 34 bytes of zeros is a valid (if empty) STREAMINFO
	header := []byte{
		0x80,             // last block, type 0 (STREAMINFO)
		0x00, 0x00, 0x22, // length = 34
	}
	flac := append([]byte("fLaC"), header...)
	flac = append(flac, streaminfo...)
	// minimal fake audio frame
	flac = append(flac, 0xff, 0xf8, 0x00, 0x00)
	return flac
}

func tempFile(t *testing.T, pattern string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), pattern)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	return f.Name()
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && func() bool {
		for i := 0; i <= len(s)-len(sub); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	}())
}

func containsBytes(data, sub []byte) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i <= len(data)-len(sub); i++ {
		match := true
		for j := range sub {
			if data[i+j] != sub[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// suppress unused import
var _ = filepath.Join
