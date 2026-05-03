package downloader

import "testing"

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
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := expandPlaceholders(c.format, c.attrs); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
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

// ---- isAlbumType --------------------------------------------------------

func TestIsAlbumType(t *testing.T) {
	cases := []struct {
		albumType string
		album     map[string]interface{}
		want      bool
	}{
		{"remaster", map[string]interface{}{"title": "Dark Side (Remastered)", "version": ""}, true},
		{"remaster", map[string]interface{}{"title": "OK Computer", "version": ""}, false},
		{"remaster", map[string]interface{}{"title": "OK Computer", "version": "Remastered"}, true},
		{"extra", map[string]interface{}{"title": "Deluxe Edition", "version": ""}, true},
		{"extra", map[string]interface{}{"title": "Anniversary Edition", "version": ""}, true},
		{"extra", map[string]interface{}{"title": "Normal Album", "version": ""}, false},
		{"extra", map[string]interface{}{"title": "Normal", "version": "Live at MSG"}, true},
		{"unknown", map[string]interface{}{"title": "Anything", "version": ""}, false},
	}
	for _, c := range cases {
		got := isAlbumType(c.albumType, c.album)
		if got != c.want {
			t.Errorf("isAlbumType(%q, %v) = %v, want %v", c.albumType, c.album, got, c.want)
		}
	}
}
