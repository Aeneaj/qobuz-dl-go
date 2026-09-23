package ui

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// session builds a progress Model as it looks deep into a long run: finished
// tracks plus three active downloads, on a 120×40 terminal.
func session(finished int) Model { return sessionSized(finished, 40) }

func sessionSized(finished, height int) Model {
	var m tea.Model = NewModel()
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: height})
	m, _ = m.Update(MsgAlbum{Title: "Album", Artist: "Artist", Format: "FLAC 24/96", Tracks: finished + 3})
	for i := 0; i < finished+3; i++ {
		id := fmt.Sprint(i)
		c := new(atomic.Int64)
		c.Store(30 << 20)
		m, _ = m.Update(MsgRegisterTrack{ID: id, Num: i%12 + 1, Name: "Some Track Title " + id, Counter: c})
		m, _ = m.Update(MsgSetTotal{ID: id, Total: 50 << 20})
		if i < finished {
			m, _ = m.Update(MsgDone{ID: id})
		}
	}
	return m.(Model)
}

func BenchmarkMemView(b *testing.B) {
	for _, n := range []int{12, 300, 3600} {
		m := session(n)
		b.Run(fmt.Sprintf("finished=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = m.View()
			}
		})
	}
}

// A long run must render what fits on screen, not the whole session: the
// header stays on top, the active downloads stay visible, and the frame never
// outgrows the terminal. Rendering every track made a 3,600-track session
// cost 81 ms and 21 MB per frame and scrolled the header away.
func TestViewFitsTheScreen(t *testing.T) {
	for _, finished := range []int{0, 5, 300} {
		for _, height := range []int{12, 24, 40} {
			out := sessionSized(finished, height).View()
			if got := strings.Count(out, "\n") + 1; got > height {
				t.Errorf("finished=%d height=%d: rendered %d lines", finished, height, got)
			}
			plain := stripANSI(out)
			if !strings.Contains(plain, "QOBUZ-DL") {
				t.Errorf("finished=%d height=%d: header scrolled away", finished, height)
			}
			if height >= 24 && strings.Count(plain, "⬇") != 3 {
				t.Errorf("finished=%d height=%d: %d active tracks shown, want 3", finished, height, strings.Count(plain, "⬇"))
			}
			if finished > 0 && height >= 24 && !strings.Contains(plain, "✓") {
				t.Errorf("finished=%d height=%d: no recently finished track shown", finished, height)
			}
		}
	}
}

// The shell draws its own chrome around the progress Model, so the Model gets
// only what is left of the window.
func TestShellRunningViewFitsTheScreen(t *testing.T) {
	s := NewShell(context.Background(), &fakeBackend{})
	s.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	s.dl = session(300)
	s.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	s.screen, s.kind = scRunning, runDownload
	if got := strings.Count(s.View(), "\n") + 1; got > 30 {
		t.Errorf("running shell rendered %d lines on a 30-line terminal", got)
	}
}

// One tick chain at a time: a track finishing must not let the next one
// start a second chain while the first tick is still pending.
func TestOneTickChain(t *testing.T) {
	var m tea.Model = NewModel()
	for _, id := range []string{"a", "b"} {
		m, _ = m.Update(MsgRegisterTrack{ID: id, Counter: new(atomic.Int64)})
	}
	m, cmd := m.Update(MsgSetTotal{ID: "a", Total: 10})
	if cmd == nil {
		t.Fatal("first active track must start ticking")
	}
	m, _ = m.Update(MsgDone{ID: "a"})
	if _, cmd = m.Update(MsgSetTotal{ID: "b", Total: 10}); cmd != nil {
		t.Error("second track started another tick chain while one was pending")
	}
}
