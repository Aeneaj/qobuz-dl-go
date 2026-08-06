package downloader

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Aeneaj/qobuz-dl-go/internal/ui"
)

// headlessTUI builds a bubbletea program that processes messages but draws
// nothing, so it runs under `go test` with no TTY attached.
func headlessTUI() *tea.Program {
	return tea.NewProgram(ui.NewModel(), tea.WithoutRenderer(), tea.WithInput(nil))
}

// TestIntegration_TUIDownloadsSameFiles runs a full album download with the
// bubbletea TUI driving progress instead of mpb, over the same fake Qobuz
// server as the mpb tests.
//
// Two things can only break on this path. The mpb container is nil under the
// TUI, so any surviving p.New / p.Wait panics; and each track's progress sink
// is a ui.TrackHandle rather than an *mpb.Bar, so the ProgressBar interface
// has to carry the whole download. Both failures are loud, but only if
// something actually exercises the branch — asserting the same files land on
// disk as in TestIntegration_DownloadAlbum keeps it honest.
func TestIntegration_TUIDownloadsSameFiles(t *testing.T) {
	q := newFakeQobuz(t, threeTracks())
	d, dir := newTestDownloader(t, q, nil)

	p := headlessTUI()
	d.SetUI(p)

	// Both draw to the cursor, so an mpb container under the TUI means two
	// renderers fighting over the screen. Wrong files are not the symptom —
	// a garbled display is — so assert the container is never built.
	if got := d.newProgress(context.Background()); got != nil {
		t.Errorf("newProgress under TUI = %v, want nil (mpb must stay dormant)", got)
	}

	done := make(chan error, 1)
	go func() {
		done <- d.downloadAlbum(context.Background(), "alb1", dir)
		p.Quit()
	}()

	if _, err := p.Run(); err != nil {
		t.Fatalf("tui program: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("downloadAlbum under TUI: %v", err)
	}

	want := []string{
		"Test Artist - Test Album/01. First Song.flac",
		"Test Artist - Test Album/02. Second Song.flac",
		"Test Artist - Test Album/03. Third Song.flac",
		"Test Artist - Test Album/cover.jpg",
	}
	got := relFiles(t, dir)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("files on disk:\ngot:\n  %s\nwant:\n  %s",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// TestTermOutDiscardsUnderTUI pins the rule that keeps the screen intact:
// while bubbletea owns the terminal nothing may reach stdout. Every status
// message in the package routes through termOut, so this one assertion covers
// all of them.
func TestTermOutDiscardsUnderTUI(t *testing.T) {
	d := &Downloader{}
	if w := d.termOut(); w == nil {
		t.Fatal("termOut returned nil")
	}
	if d.termOut() != os.Stdout {
		t.Error("without a TUI or bars, termOut must write to stdout")
	}

	d.SetUI(headlessTUI())
	n, err := d.termOut().Write([]byte("this must not reach the screen"))
	if err != nil || n == 0 {
		t.Fatalf("discard write: n=%d err=%v", n, err)
	}
	if d.termOut() == os.Stdout {
		t.Error("with a TUI active, termOut must not write to stdout")
	}
}

// TestNoDirectStdoutWrites guards the invariant that TestTermOutDiscardsUnderTUI
// cannot: that one proves termOut() returns io.Discard under the TUI, this one
// proves the package actually goes through it.
//
// It found three live leaks when it was written — tagFLAC's missing-cover
// warning plus the empty-playlist notices in csvbatch.go and lastfm.go — each
// sitting a couple of lines from code that did use termOut(). A bare fmt.Printf
// corrupts mpb's redraw and, under the TUI, paints over the alt screen.
//
// Static on purpose: reaching these branches for real needs a live download.
func TestNoDirectStdoutWrites(t *testing.T) {
	// interactive.go is the `fun` REPL: it owns the terminal and never runs
	// with bars or the TUI up. oauth.go prints only with the terminal
	// released, which is what makes reusing the CLI flow correct.
	allowed := map[string]bool{"interactive.go": true, "oauth.go": true}

	reDirect := regexp.MustCompile(`fmt\.Print(f|ln)?\(|fmt\.Fprint(f|ln)?\(os\.Stdout`)

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || allowed[name] {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		scanned++
		for _, m := range reDirect.FindAllString(string(src), -1) {
			t.Errorf("%s writes straight to stdout with %s — use d.termOut(), or take an io.Writer parameter", name, m)
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no files — the walk is broken")
	}
}
