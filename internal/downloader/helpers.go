package downloader

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

func makeM3U(w io.Writer, dir string) {
	plName := filepath.Base(dir) + ".m3u"
	plPath := filepath.Join(dir, plName)

	var sb strings.Builder
	sb.WriteString("#EXTM3U")
	entries := 0

	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error { //nolint:errcheck
		if err != nil || d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".mp3" && ext != ".flac" {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return nil
		}
		name := d.Name()
		fmt.Fprintf(&sb, "\n\n#EXTINF:-1,%s\n%s",
			strings.TrimSuffix(name, filepath.Ext(name)), rel)
		entries++
		return nil
	})

	if entries == 0 {
		return
	}
	f, err := os.Create(plPath)
	if err != nil {
		fmt.Fprintf(w, "\033[31mCould not create M3U: %v\033[0m\n", err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(sb.String()); err != nil {
		fmt.Fprintf(w, "\033[31mCould not write M3U: %v\033[0m\n", err)
		return
	}
	fmt.Fprintf(w, "\033[32mM3U playlist saved: %s\033[0m\n", plName)
}

func getTitle(item map[string]interface{}) string {
	title, _ := item["title"].(string)
	version, _ := item["version"].(string)
	if version != "" && !strings.Contains(strings.ToLower(title), strings.ToLower(version)) {
		title = fmt.Sprintf("%s (%s)", title, version)
	}
	return title
}

// cleanFormatStr strips a stray file extension from a format string and, when
// the release has no bit depth / sampling rate to report, swaps a format that
// asks for them for one that does not — otherwise those placeholders would
// expand to "n_a" ("[n_aB-n_akHz]").
//
// The swap throws away the user's whole template, subfolder structure
// included, so it is announced on w instead of happening silently: a
// {artist}/{album} layout turning flat on an MP3 download is exactly the kind
// of surprise the user cannot otherwise explain. {format} is the placeholder
// that describes quality in both cases, so the note points at it.
func cleanFormatStr(w io.Writer, format, fileFormat string) string {
	format = strings.TrimSuffix(format, ".mp3")
	format = strings.TrimSuffix(format, ".flac")
	format = strings.TrimSpace(format)

	if fileFormat != "MP3" && fileFormat != "Unknown" {
		return format
	}
	if !strings.Contains(format, "{bit_depth}") && !strings.Contains(format, "{sampling_rate}") {
		return format
	}

	substitute, reason := "{artist} - {album}", "which could not be resolved for this release"
	if fileFormat == "MP3" {
		substitute, reason = "{artist} - {album} ({year}) [MP3]", "which MP3 files do not have"
	}
	fmt.Fprintf(w, "\033[33mNote: format %q asks for {bit_depth}/{sampling_rate}, %s — using %q instead.\n"+
		"      Use {format} rather than {bit_depth}/{sampling_rate} to keep your own template.\033[0m\n",
		format, reason, substitute)
	return substitute
}

// expandPlaceholders substitutes {placeholder} tokens in format with values
// from attrs. Each value is passed through sanitize so illegal path characters
// (including "/" and "\") are neutralised before insertion — this lets a
// format string like "{artist}/{album}" contain real subfolder separators
// while metadata values (e.g. an artist named "AC/DC") cannot inject them.
func expandPlaceholders(format string, attrs map[string]string) string {
	pairs := make([]string, 0, 2*len(attrs))
	for k, v := range attrs {
		if v == "" || v == "<nil>" || v == "%!v(MISSING)" {
			v = "n_a"
		}
		pairs = append(pairs, k, sanitize(v))
	}
	// One pass over the template: a substituted value is never scanned again,
	// so a title containing "{artist}" stays literal. Replacing key by key in
	// map order did rescan them, and the result changed from run to run. No
	// key is a prefix of another ("{x}" never starts "{y}"), so the order of
	// pairs does not matter.
	return strings.NewReplacer(pairs...).Replace(format)
}

// deliveredIsMP3 reports whether the file the API actually handed us is an MP3.
// That is not the same question as "did the user ask for MP3": fallbackQuality
// walks down to 5 when a lossless request fails, so a request for quality 7 can
// come back as a 320 kbps file. Deciding from Options.Quality wrote those bytes
// to a .flac name and ran the FLAC tagger over them.
//
// format_id is the same enum the request sends, so it is the direct answer;
// mime_type covers a response that omits it, and the requested quality is the
// last resort.
func deliveredIsMP3(trackURL map[string]interface{}, requested int) bool {
	if fid, ok := trackURL["format_id"].(float64); ok && fid != 0 {
		return int(fid) == 5
	}
	if mt, _ := trackURL["mime_type"].(string); mt != "" {
		return strings.Contains(mt, "mpeg") || strings.Contains(mt, "mp3")
	}
	return requested == 5
}

// maxNameBytes is the per-component file name limit on ext4, NTFS and APFS.
// It is counted in bytes, not characters, so a 100-character CJK title
// (3 bytes per rune) is already over it.
const maxNameBytes = 255

// limitNameBytes returns name+ext trimmed to fit maxNameBytes.
//
// Two things it must not do. It must not split a multi-byte character, so the
// trim walks back whole runes. And it must not make two different titles
// collapse onto the same file name: they would resolve to the same path, and
// the second track would be skipped as "already downloaded" — the silent
// track loss of issue #23, reached through a different door. So a trimmed name
// carries the track id, which is unique by definition.
//
// Only the file name is limited, never the directories above it: the limit is
// per component, and trimming a whole path would cut into the album folder.
func limitNameBytes(name, ext, trackID string) string {
	if len(name)+len(ext) <= maxNameBytes {
		return name + ext
	}
	suffix := "-" + sanitize(trackID) + ext
	for len(name)+len(suffix) > maxNameBytes {
		_, size := utf8.DecodeLastRuneInString(name)
		if size == 0 {
			break
		}
		name = name[:len(name)-size]
	}
	return name + suffix
}

const barLabelWidth = 42

// barLabel builds a fixed-width label for a track progress bar.
func barLabel(trackNum int, title string) string {
	var label string
	if trackNum > 0 {
		label = fmt.Sprintf("  %02d. %s", trackNum, title)
	} else {
		label = "  " + title
	}
	return truncateStr(label, barLabelWidth)
}

// truncateStr pads or truncates s to exactly n runes.
func truncateStr(s string, n int) string {
	runes := []rune(s)
	if len(runes) > n {
		return string(runes[:n-1]) + "…"
	}
	return s + strings.Repeat(" ", n-len(runes))
}

// ---- ID helpers ----

// idStr converts a JSON-decoded ID (float64 or string) to its integer string
// representation without scientific notation. JSON numbers are decoded as
// float64 in map[string]interface{}, so large IDs like 98439707 would render
// as "9.8439707e+07" with %v — which the Qobuz API does not recognize.
func idStr(v interface{}) string {
	switch n := v.(type) {
	case float64:
		return strconv.FormatInt(int64(n), 10)
	case string:
		return n
	default:
		return fmt.Sprintf("%v", v)
	}
}

// ---- misc helpers ----

var reUnsafe = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)

func sanitize(s string) string {
	s = reUnsafe.ReplaceAllString(s, "_")
	return strings.TrimSpace(s)
}

// safeJoin joins base with elem, cleans the result, and verifies it still
// lives under base. Callers must pre-translate user-format separators with
// filepath.FromSlash so subfolder templates work on Windows. Guards against
// path traversal from malicious templates or metadata (e.g. "../../etc").
func safeJoin(base, elem string) (string, error) {
	cleanBase := filepath.Clean(base)
	joined := filepath.Clean(filepath.Join(cleanBase, elem))
	if joined != cleanBase && !strings.HasPrefix(joined, cleanBase+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes base directory %q", elem, cleanBase)
	}
	return joined, nil
}

func nestedStr(m map[string]interface{}, keys ...string) string {
	s, _ := walkKeys(m, keys).(string)
	return s
}

func nestedFloat(m map[string]interface{}, keys ...string) float64 {
	v, _ := walkKeys(m, keys).(float64)
	return v
}

// walkKeys descends through nested maps, returning nil the moment a level is
// missing or is not a map.
func walkKeys(m map[string]interface{}, keys []string) interface{} {
	var cur interface{} = m
	for _, k := range keys {
		mm, ok := cur.(map[string]interface{})
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	return cur
}

func releaseYear(meta map[string]interface{}) string {
	if rd, ok := meta["release_date_original"].(string); ok && len(rd) >= 4 {
		return rd[:4]
	}
	return "0000"
}

func isLocalFile(s string) bool {
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return false
	}
	_, err := os.Stat(s)
	return err == nil
}

// ---- format string validation ----

// Placeholders expandPlaceholders actually substitutes, per format string.
// Keep in sync with the attrs maps in album.go / track.go / finalTrackPath.
var (
	folderPlaceholders = []string{"{artist}", "{album}", "{year}", "{bit_depth}", "{sampling_rate}", "{format}"}
	trackPlaceholders  = []string{"{tracknumber}", "{tracktitle}", "{artist}", "{albumartist}", "{bit_depth}", "{sampling_rate}", "{version}"}
)

var placeholderRe = regexp.MustCompile(`\{[^{}]*\}`)

// validateFormats rejects folder/track format strings that would not expand.
// An unrecognised token survives expansion literally, and in a track format
// that means every track of an album resolves to the same filename: the first
// download wins and the rest are silently skipped as "already downloaded"
// (issue #23). Checked once in New so every entry point (CLI, TUI, csv, fun)
// is covered.
func validateFormats(folderFmt, trackFmt string) error {
	if bad := unknownPlaceholders(folderFmt, folderPlaceholders); len(bad) > 0 {
		return fmt.Errorf("folder_format: unknown placeholder(s) %s\nsupported: %s\nfix it in config.ini or pass --folder-format",
			strings.Join(bad, " "), strings.Join(folderPlaceholders, " "))
	}
	if bad := unknownPlaceholders(trackFmt, trackPlaceholders); len(bad) > 0 {
		return fmt.Errorf("track_format: unknown placeholder(s) %s\nsupported: %s\nfix it in config.ini or pass --track-format",
			strings.Join(bad, " "), strings.Join(trackPlaceholders, " "))
	}
	// Without one of these every track gets the same name and only one survives.
	if !strings.Contains(trackFmt, "{tracknumber}") && !strings.Contains(trackFmt, "{tracktitle}") {
		return fmt.Errorf("track_format %q must contain {tracknumber} or {tracktitle}, otherwise every track overwrites the previous one", trackFmt)
	}
	return nil
}

// unknownPlaceholders returns the {tokens} in format that are not in allowed.
func unknownPlaceholders(format string, allowed []string) []string {
	var bad []string
	for _, tok := range placeholderRe.FindAllString(format, -1) {
		known := false
		for _, a := range allowed {
			if tok == a {
				known = true
				break
			}
		}
		if !known {
			bad = append(bad, tok)
		}
	}
	return bad
}
