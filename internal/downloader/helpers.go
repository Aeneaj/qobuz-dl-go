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

func cleanTmp(dir string) {
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error { //nolint:errcheck
		if err == nil && !d.IsDir() {
			name := d.Name()
			if strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".tmp") {
				os.Remove(path)
			}
		}
		return nil
	})
}

func getTitle(item map[string]interface{}) string {
	title, _ := item["title"].(string)
	version, _ := item["version"].(string)
	if version != "" && !strings.Contains(strings.ToLower(title), strings.ToLower(version)) {
		title = fmt.Sprintf("%s (%s)", title, version)
	}
	return title
}

func cleanFormatStr(format, fileFormat string) string {
	format = strings.TrimSuffix(format, ".mp3")
	format = strings.TrimSuffix(format, ".flac")
	format = strings.TrimSpace(format)

	if fileFormat == "MP3" || fileFormat == "Unknown" {
		if strings.Contains(format, "{bit_depth}") || strings.Contains(format, "{sampling_rate}") {
			if fileFormat == "MP3" {
				return "{artist} - {album} ({year}) [MP3]"
			}
			return "{artist} - {album}"
		}
	}
	return format
}

// expandPlaceholders substitutes {placeholder} tokens in format with values
// from attrs. Each value is passed through sanitize so illegal path characters
// (including "/" and "\") are neutralised before insertion — this lets a
// format string like "{artist}/{album}" contain real subfolder separators
// while metadata values (e.g. an artist named "AC/DC") cannot inject them.
func expandPlaceholders(format string, attrs map[string]string) string {
	result := format
	for k, v := range attrs {
		if v == "" || v == "<nil>" || v == "%!v(MISSING)" {
			v = "n_a"
		}
		result = strings.ReplaceAll(result, k, sanitize(v))
	}
	return result
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
	var cur interface{} = m
	for _, k := range keys {
		mm, ok := cur.(map[string]interface{})
		if !ok {
			return ""
		}
		cur = mm[k]
	}
	s, _ := cur.(string)
	return s
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

func getFloat(m map[string]interface{}, key string) float64 {
	v, _ := m[key].(float64)
	return v
}
