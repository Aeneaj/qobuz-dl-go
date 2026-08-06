package ui

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// English is the default: anything that is not a language we ship leaves the
// strings exactly as written in the source.
func TestSetLangUnknownFallsBackToEnglish(t *testing.T) {
	t.Cleanup(func() { SetLang("") })

	for _, code := range []string{"", "en", "EN", "fr", "es-419", "nonsense"} {
		SetLang(code)
		if got := T("Search albums"); got != "Search albums" {
			t.Errorf("SetLang(%q): T = %q, want the English source string", code, got)
		}
	}
}

func TestSetLangSpanish(t *testing.T) {
	t.Cleanup(func() { SetLang("") })

	for _, code := range []string{"es", "ES", " es "} {
		SetLang(code)
		if got := T("Search albums"); got != "Buscar álbumes" {
			t.Errorf("SetLang(%q): T = %q, want Spanish", code, got)
		}
	}
}

// A string with no translation must come back as itself. Otherwise a forgotten
// entry renders as a blank gap in the middle of the screen.
func TestTUntranslatedReturnsInput(t *testing.T) {
	SetLang("es")
	t.Cleanup(func() { SetLang("") })

	const missing = "this string is deliberately absent from the table"
	if got := T(missing); got != missing {
		t.Errorf("T(untranslated) = %q, want the input back", got)
	}
}

// The menu is the one table where a missing translation is guaranteed to be
// visible, so every label, hint and section heading must be covered. This is
// what catches a menu entry added without its Spanish text.
func TestSpanishCoversEveryMenuString(t *testing.T) {
	for _, m := range menu {
		for _, s := range []string{m.label, m.hint, m.section} {
			if s == "" {
				continue
			}
			if _, ok := es[s]; !ok {
				t.Errorf("menu string %q has no Spanish translation", s)
			}
		}
	}
}

// Every key in the Spanish table must be reachable from the source. A key that
// matches nothing is a translation that silently never shows — usually a typo
// or a string that was reworded on the English side.
func TestSpanishHasNoOrphanKeys(t *testing.T) {
	src := readUISources(t)

	for key := range es {
		if !strings.Contains(src, quoteForGo(key)) {
			t.Errorf("Spanish key %q appears nowhere in the ui sources — dead translation", key)
		}
	}
}

// Guards the direction of the whole scheme: the source must hold English, with
// Spanish confined to lang.go. A Spanish literal anywhere else is a string that
// can never be shown in English.
func TestNoSpanishLeftInSources(t *testing.T) {
	src := readUISources(t)

	// Characters that do not occur in the English this program writes. Accented
	// vowels are included: in principle they match loan words, in practice this
	// codebase has none, and leaving them out let "(vacío)" sit in widgets.go
	// unnoticed. Spanish with no accent at all ("%d completadas") still slips
	// through here — TestEnglishRenderStaysEnglish is what catches that.
	reSpanish := regexp.MustCompile(`"[^"]*[ñÑ¿¡áéíóúÁÉÍÓÚ][^"]*"`)
	if m := reSpanish.FindAllString(src, -1); m != nil {
		t.Errorf("Spanish literals outside lang.go: %v", m)
	}
}

func quoteForGo(s string) string { return `"` + s + `"` }

// readUISources returns every non-test Go source that can put text on the TUI
// screen: this package plus cmd/qobuz-dl, where the Backend implementation
// builds its own status messages. lang.go is excluded — it is the one place
// Spanish belongs.
//
// Read by directory rather than from a list of filenames: a list silently
// stops covering any file added after it was written, which is how the backend
// messages went unchecked in the first place.
func readUISources(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for _, dir := range []string{".", filepath.Join("..", "..", "cmd", "qobuz-dl")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read dir %s: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "lang.go" {
				continue
			}
			b.WriteString(readFile(t, filepath.Join(dir, name)))
			b.WriteString("\n")
		}
	}
	return b.String()
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// The data tests above check the translation table; this one checks the render
// sites actually consult it. It caught a real bug: the selected menu row was
// built from m.label directly while every other row went through T(), so the
// highlighted entry stayed English in a Spanish menu. A complete table cannot
// detect that — only rendering can.
func TestMenuRendersFullySpanish(t *testing.T) {
	SetLang("es")
	t.Cleanup(func() { SetLang("") })

	s := NewShell(context.Background(), &fakeBackend{})
	s.Update(tea.WindowSizeMsg{Width: 100, Height: 40})

	// Walk the cursor over every entry: the selected row is rendered by a
	// different branch than the rest, which is exactly where the bug hid.
	for i := range menu {
		s.menu.cursor = i
		view := stripANSI(s.View())
		for _, m := range menu {
			if m.label == "" {
				continue
			}
			if strings.Contains(view, m.label) {
				t.Errorf("cursor on %d: English label %q rendered in the Spanish menu", i, m.label)
			}
			if want := es[m.label]; want != "" && !strings.Contains(view, want) {
				t.Errorf("cursor on %d: Spanish label %q missing from the menu", i, want)
			}
		}
	}
}

var reANSI = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

func stripANSI(s string) string { return reANSI.ReplaceAllString(s, "") }

// TestEnglishRenderStaysEnglish is the counterpart to the test above: it walks
// the screens in English and fails if any Spanish text shows up.
//
// This is the only check that sees Spanish carrying no distinctive character.
// "%d completadas" and "Ctrl+C cancelar" sat hardcoded in viewFooter through
// every other test in this file: the table tests only look at the table, and
// the source scan keys on ñ/¿/¡/accents, none of which those two contain.
// A hardcoded string renders identically in both languages — that is the
// signal, and it needs no word list.
func TestEnglishRenderStaysEnglish(t *testing.T) {
	SetLang("")
	t.Cleanup(func() { SetLang("") })

	m := Model{
		done:   3,
		failed: 2,
		tracks: []trackEntry{{state: stateActive}, {state: stateDone}},
	}
	p := newPicker(nil, false)
	rendered := stripANSI(m.viewFooter() + "\n" + p.view(40))

	for key, val := range es {
		if val == key {
			continue
		}
		// Compare on the literal part: "%d completadas" renders as "3 completadas".
		frag := strings.TrimSpace(strings.ReplaceAll(val, "%d", ""))
		if frag == "" {
			continue
		}
		if strings.Contains(rendered, frag) {
			t.Errorf("English render contains Spanish %q (key %q) — that string is hardcoded, not translated:\n%s",
				frag, key, rendered)
		}
	}
}
