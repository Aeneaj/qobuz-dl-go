package downloader

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- sanitize -----------------------------------------------------------

func TestSanitize(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Hello World", "Hello World"},
		{"AC/DC", "AC_DC"},
		{"file:name", "file_name"},
		{`back\slash`, "back_slash"},
		{"con<trol>", "con_trol_"},
		{`"quoted"`, "_quoted_"},
		{"pipe|char", "pipe_char"},
		{"ques?tion", "ques_tion"},
		{"star*fish", "star_fish"},
		{"  spaces  ", "spaces"},
		{"control\x00char", "control_char"},
		{"control\x1fchar", "control_char"},
		{"", ""},
	}
	for _, c := range cases {
		if got := sanitize(c.in); got != c.want {
			t.Errorf("sanitize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---- expandPlaceholders -------------------------------------------------

func TestExpandPlaceholders(t *testing.T) {
	cases := []struct {
		name   string
		format string
		attrs  map[string]string
		want   string
	}{
		{
			name:   "basic substitution",
			format: "{artist} - {album} ({year})",
			attrs:  map[string]string{"{artist}": "Radiohead", "{album}": "OK Computer", "{year}": "1997"},
			want:   "Radiohead - OK Computer (1997)",
		},
		{
			name:   "empty value replaced with n_a",
			format: "{artist} - {album}",
			attrs:  map[string]string{"{artist}": "Band", "{album}": ""},
			want:   "Band - n_a",
		},
		{
			name:   "<nil> value replaced with n_a",
			format: "{title}",
			attrs:  map[string]string{"{title}": "<nil>"},
			want:   "n_a",
		},
		{
			name:   "missing placeholder stays as-is",
			format: "{artist} - {unknown}",
			attrs:  map[string]string{"{artist}": "Band"},
			want:   "Band - {unknown}",
		},
		{
			name:   "no placeholders",
			format: "literal string",
			attrs:  map[string]string{"{key}": "value"},
			want:   "literal string",
		},
		{
			// Subfolder support: literal "/" in the template must survive so the
			// caller can build a directory tree from it.
			name:   "template slash preserved",
			format: "{artist}/{album}",
			attrs:  map[string]string{"{artist}": "Radiohead", "{album}": "OK Computer"},
			want:   "Radiohead/OK Computer",
		},
		{
			// Adversarial value: a "/" coming from metadata must be neutralised
			// so it cannot smuggle in an extra path segment.
			name:   "slash inside value neutralised",
			format: "{artist}",
			attrs:  map[string]string{"{artist}": "AC/DC"},
			want:   "AC_DC",
		},
		{
			// Combined: template separators kept, in-value separators stripped.
			name:   "subfolder template with dirty value",
			format: "{artist}/{album}",
			attrs:  map[string]string{"{artist}": "GZA/Genius", "{album}": "Liquid Swords"},
			want:   "GZA_Genius/Liquid Swords",
		},
		{
			name:   "backslash inside value neutralised",
			format: "{title}",
			attrs:  map[string]string{"{title}": `back\slash`},
			want:   "back_slash",
		},
		{
			// Colons, quotes, pipes, wildcards, controls — all covered by sanitize.
			name:   "assorted unsafe chars inside value",
			format: "{title}",
			attrs:  map[string]string{"{title}": `a:b"c|d?e*f`},
			want:   "a_b_c_d_e_f",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := expandPlaceholders(c.format, c.attrs); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// ---- safeJoin -----------------------------------------------------------

func TestSafeJoin(t *testing.T) {
	sep := string(filepath.Separator)
	base := filepath.Join(sep+"tmp", "base")

	t.Run("success cases", func(t *testing.T) {
		cases := []struct {
			name string
			elem string
			want string
		}{
			{"simple subdir", "sub", filepath.Join(base, "sub")},
			{"nested subdirs", filepath.Join("a", "b", "c"), filepath.Join(base, "a", "b", "c")},
			{"dot resolves to base", ".", base},
			{"empty elem resolves to base", "", base},
			{"trailing separator normalised", "sub" + sep, filepath.Join(base, "sub")},
			{"internal double dots that stay inside", filepath.Join("a", "..", "b"), filepath.Join(base, "b")},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				got, err := safeJoin(base, c.elem)
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got != c.want {
					t.Errorf("got %q, want %q", got, c.want)
				}
			})
		}
	})

	t.Run("path traversal is blocked", func(t *testing.T) {
		attacks := []struct {
			name string
			elem string
		}{
			{"direct parent escape", filepath.Join("..", "etc")},
			{"chained parents", filepath.Join("..", "..", "..", "etc", "passwd")},
			{"indirect via valid subdir", filepath.Join("sub", "..", "..", "etc")},
			// Prefix trick: joined path shares a prefix with base but is a
			// sibling directory. HasPrefix(joined, base) alone would miss it —
			// the trailing separator in the check is what catches it.
			{"sibling prefix trick", filepath.Join("..", filepath.Base(base)+"_evil")},
		}
		for _, a := range attacks {
			t.Run(a.name, func(t *testing.T) {
				got, err := safeJoin(base, a.elem)
				if err == nil {
					t.Fatalf("expected traversal to be blocked; got %q", got)
				}
				if !strings.Contains(err.Error(), "escapes") {
					t.Errorf("error should mention escape; got: %v", err)
				}
			})
		}
	})

	t.Run("absolute-looking elem is treated as relative", func(t *testing.T) {
		// filepath.Join drops leading separators on subsequent args, so an
		// elem like "/etc/passwd" is safely re-rooted under base.
		got, err := safeJoin(base, sep+filepath.Join("etc", "passwd"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := filepath.Join(base, "etc", "passwd")
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("base with trailing separator is normalised", func(t *testing.T) {
		got, err := safeJoin(base+sep, "sub")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := filepath.Join(base, "sub"); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

// ---- folder_format end-to-end (subfolder flow) --------------------------

// TestFolderFormatSubfolder exercises the full pipeline the downloader uses
// to turn a user format string into a real, safe directory on disk:
// expandPlaceholders → filepath.FromSlash → safeJoin → MkdirAll.
func TestFolderFormatSubfolder(t *testing.T) {
	baseDir := t.TempDir()

	t.Run("template with subfolder creates nested tree", func(t *testing.T) {
		attrs := map[string]string{
			"{artist}": "Radiohead",
			"{album}":  "OK Computer",
			"{year}":   "1997",
		}
		folderName := expandPlaceholders("{artist}/{album} ({year})", attrs)
		if folderName != "Radiohead/OK Computer (1997)" {
			t.Fatalf("expandPlaceholders produced %q", folderName)
		}

		got, err := safeJoin(baseDir, filepath.FromSlash(folderName))
		if err != nil {
			t.Fatalf("safeJoin returned error: %v", err)
		}
		want := filepath.Join(baseDir, "Radiohead", "OK Computer (1997)")
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}

		if err := os.MkdirAll(got, 0755); err != nil {
			t.Fatalf("MkdirAll failed: %v", err)
		}
		if info, err := os.Stat(got); err != nil || !info.IsDir() {
			t.Fatalf("expected directory at %q (err=%v)", got, err)
		}
		// The intermediate artist directory must also exist as a real folder,
		// not flattened into a single name with an underscore.
		if info, err := os.Stat(filepath.Join(baseDir, "Radiohead")); err != nil || !info.IsDir() {
			t.Errorf("intermediate artist directory missing — subfolder was flattened")
		}
	})

	t.Run("dirty value cannot inject an extra subfolder", func(t *testing.T) {
		attrs := map[string]string{
			"{artist}": "GZA/Genius", // legitimate-looking but contains "/"
			"{album}":  "Liquid Swords",
		}
		folderName := expandPlaceholders("{artist}/{album}", attrs)
		// The "/" from the value must have been replaced with "_" so the
		// resulting path has exactly two segments, not three.
		got, err := safeJoin(baseDir, filepath.FromSlash(folderName))
		if err != nil {
			t.Fatalf("safeJoin returned error: %v", err)
		}
		want := filepath.Join(baseDir, "GZA_Genius", "Liquid Swords")
		if got != want {
			t.Fatalf("got %q, want %q — value should have been flattened", got, want)
		}
	})

	t.Run("traversal in format string is blocked", func(t *testing.T) {
		// A malicious admin config with ".." in the folder template.
		folderName := expandPlaceholders("../{album}", map[string]string{"{album}": "Escape"})
		if _, err := safeJoin(baseDir, filepath.FromSlash(folderName)); err == nil {
			t.Fatal("expected traversal to be blocked, got nil")
		}
	})

	t.Run("traversal in value is neutralised before reaching safeJoin", func(t *testing.T) {
		// A dirty value cannot express traversal because sanitize strips "/".
		// safeJoin therefore accepts it — the result is just an oddly-named
		// (but contained) folder.
		folderName := expandPlaceholders("{artist}", map[string]string{"{artist}": "../evil"})
		got, err := safeJoin(baseDir, filepath.FromSlash(folderName))
		if err != nil {
			t.Fatalf("safeJoin should accept the sanitised value: %v", err)
		}
		if !strings.HasPrefix(got, baseDir+string(filepath.Separator)) {
			t.Errorf("resolved path %q escaped base %q", got, baseDir)
		}
	})
}

// ---- renderFormat -------------------------------------------------------

func TestRenderFormat(t *testing.T) {
	cases := []struct {
		name   string
		format string
		m      map[string]interface{}
		want   string
	}{
		{
			name:   "simple string key",
			format: "{title}",
			m:      map[string]interface{}{"title": "OK Computer"},
			want:   "OK Computer",
		},
		{
			name:   "nested key obj[field]",
			format: "{artist[name]} - {title}",
			m: map[string]interface{}{
				"artist": map[string]interface{}{"name": "Radiohead"},
				"title":  "Karma Police",
			},
			want: "Radiohead - Karma Police",
		},
		{
			name:   "float64 key renders as integer",
			format: "{count}",
			m:      map[string]interface{}{"count": float64(12)},
			want:   "12",
		},
		{
			name:   "missing nested parent returns n/a",
			format: "{artist[name]}",
			m:      map[string]interface{}{},
			want:   "n/a",
		},
		{
			name:   "nested key missing field returns empty string",
			format: "{artist[name]}",
			m:      map[string]interface{}{"artist": map[string]interface{}{}},
			want:   "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := renderFormat(c.format, c.m); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// ---- formatDuration -----------------------------------------------------

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		secs int
		want string
	}{
		{0, "00:00"},
		{30, "00:30"},
		{65, "01:05"},
		{3600, "01:00:00"},
		{3661, "01:01:01"},
		{7322, "02:02:02"},
	}
	for _, c := range cases {
		if got := formatDuration(c.secs); got != c.want {
			t.Errorf("formatDuration(%d) = %q, want %q", c.secs, got, c.want)
		}
	}
}

// ---- idStr --------------------------------------------------------------

func TestIdStr(t *testing.T) {
	cases := []struct {
		in   interface{}
		want string
	}{
		{float64(12345678), "12345678"},
		{float64(98439707), "98439707"},
		{"abc123", "abc123"},
		{"", ""},
	}
	for _, c := range cases {
		if got := idStr(c.in); got != c.want {
			t.Errorf("idStr(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---- nestedStr ----------------------------------------------------------

func TestNestedStr(t *testing.T) {
	m := map[string]interface{}{
		"title": "OK Computer",
		"artist": map[string]interface{}{
			"name": "Radiohead",
		},
	}
	cases := []struct {
		keys []string
		want string
	}{
		{[]string{"title"}, "OK Computer"},
		{[]string{"artist", "name"}, "Radiohead"},
		{[]string{"missing"}, ""},
		{[]string{"artist", "missing"}, ""},
		{[]string{"title", "deep"}, ""}, // title is a string, not a map
	}
	for _, c := range cases {
		if got := nestedStr(m, c.keys...); got != c.want {
			t.Errorf("nestedStr(%v) = %q, want %q", c.keys, got, c.want)
		}
	}
}

// ---- releaseYear --------------------------------------------------------

func TestReleaseYear(t *testing.T) {
	cases := []struct {
		meta map[string]interface{}
		want string
	}{
		{map[string]interface{}{"release_date_original": "2023-06-01"}, "2023"},
		{map[string]interface{}{"release_date_original": "1997-05-21"}, "1997"},
		{map[string]interface{}{"release_date_original": "20"}, "0000"},
		{map[string]interface{}{}, "0000"},
		{map[string]interface{}{"release_date_original": nil}, "0000"},
	}
	for _, c := range cases {
		if got := releaseYear(c.meta); got != c.want {
			t.Errorf("releaseYear(%v) = %q, want %q", c.meta, got, c.want)
		}
	}
}

// ---- essenceTitle -------------------------------------------------------

func TestEssenceTitle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"OK Computer", "ok computer"},
		{"The Bends (Remastered)", "the bends"},
		{"Kid A (Collector's Edition)", "kid a"},
		{"(Brackets First)", "(brackets first)"},
		{"", ""},
	}
	for _, c := range cases {
		if got := essenceTitle(c.in); got != c.want {
			t.Errorf("essenceTitle(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---- isRemaster ---------------------------------------------------------

func TestIsRemaster(t *testing.T) {
	cases := []struct {
		album map[string]interface{}
		want  bool
	}{
		{map[string]interface{}{"title": "Dark Side (Remastered)", "version": ""}, true},
		{map[string]interface{}{"title": "OK Computer", "version": ""}, false},
		{map[string]interface{}{"title": "OK Computer", "version": "Remastered"}, true},
		{map[string]interface{}{"title": "Master of Puppets", "version": ""}, true}, // known false positive
		{map[string]interface{}{"title": "Deluxe Edition", "version": ""}, false},
		{map[string]interface{}{"title": "Normal", "version": "Live at MSG"}, false},
		{map[string]interface{}{}, false},
	}
	for _, c := range cases {
		if got := isRemaster(c.album); got != c.want {
			t.Errorf("isRemaster(%v) = %v, want %v", c.album, got, c.want)
		}
	}
}
