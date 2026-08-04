package ui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// fakeBackend answers instantly and records what it was asked to do, so the
// shell's state machine can be driven with real key messages and no network.
type fakeBackend struct {
	hits      []Item
	searchErr error
	got       []string // urls handed to Download
	purged    bool
	loggedIn  bool
	loginErr  error
}

func (f *fakeBackend) Login(context.Context) (string, error) {
	if f.loginErr != nil {
		return "", f.loginErr
	}
	f.loggedIn = true
	return "sesión iniciada", nil
}

func (f *fakeBackend) Search(context.Context, string, string, int) ([]Item, error) {
	return f.hits, f.searchErr
}
func (f *fakeBackend) Download(_ context.Context, urls []string) error {
	f.got = append(f.got, urls...)
	return nil
}
func (f *fakeBackend) Lyrics(context.Context, string) (string, error) { return "listo", nil }
func (f *fakeBackend) CSV(context.Context, string) (string, error)    { return "listo", nil }
func (f *fakeBackend) Config() string                                 { return "app_id = 123" }
func (f *fakeBackend) Purge() error                                   { f.purged = true; return nil }
func (f *fakeBackend) DefaultDir() string                             { return "/music" }

// press sends one key and returns any command the shell produced.
func press(t *testing.T, s *Shell, k tea.KeyMsg) tea.Cmd {
	t.Helper()
	m, cmd := s.Update(k)
	if m != tea.Model(s) {
		t.Fatal("Update must keep returning the same *Shell")
	}
	return cmd
}

func typeRunes(t *testing.T, s *Shell, text string) {
	t.Helper()
	press(t, s, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(text)})
}

var (
	keyEnter = tea.KeyMsg{Type: tea.KeyEnter}
	keySpace = tea.KeyMsg{Type: tea.KeySpace}
	keyDown  = tea.KeyMsg{Type: tea.KeyDown}
	keyEsc   = tea.KeyMsg{Type: tea.KeyEsc}
)

// TestShellSearchToQueue walks the path a user actually takes: menu → search →
// type a query → mark two hits → queue them. It fails if any screen transition
// or the marking logic breaks.
func TestShellSearchToQueue(t *testing.T) {
	be := &fakeBackend{hits: []Item{
		{Label: "Radiohead - OK Computer", URL: "u1"},
		{Label: "Radiohead - In Rainbows", URL: "u2"},
		{Label: "Radiohead - Kid A", URL: "u3"},
	}}
	s := NewShell(context.Background(), be)

	press(t, s, keyEnter) // first menu entry: search albums
	if s.screen != scInput {
		t.Fatalf("after enter on 'search albums', screen = %v, want scInput", s.screen)
	}

	typeRunes(t, s, "radiohead")
	if got := s.field.String(); got != "radiohead" {
		t.Fatalf("typed query = %q, want %q", got, "radiohead")
	}

	cmd := press(t, s, keyEnter)
	if cmd == nil {
		t.Fatal("submitting a query must return a search command")
	}
	s.Update(cmd()) // deliver the search result

	if s.screen != scResults {
		t.Fatalf("after results arrive, screen = %v, want scResults", s.screen)
	}

	press(t, s, keySpace) // mark row 0
	press(t, s, keyDown)
	press(t, s, keySpace) // mark row 1
	press(t, s, keyEnter)

	if len(s.queue) != 2 {
		t.Fatalf("queue holds %d items, want 2 (the marked ones)", len(s.queue))
	}
	if s.queue[0].URL != "u1" || s.queue[1].URL != "u2" {
		t.Errorf("queued %q and %q, want u1 and u2", s.queue[0].URL, s.queue[1].URL)
	}
	if s.screen != scMenu {
		t.Errorf("after queueing, screen = %v, want scMenu", s.screen)
	}
}

// TestShellUnmarkedSelectionUsesCursor covers the shortcut: Enter with nothing
// marked queues the highlighted row, so a single pick needs no space bar.
func TestShellUnmarkedSelectionUsesCursor(t *testing.T) {
	be := &fakeBackend{hits: []Item{{Label: "a", URL: "u1"}, {Label: "b", URL: "u2"}}}
	s := NewShell(context.Background(), be)

	press(t, s, keyEnter)
	typeRunes(t, s, "x")
	cmd := press(t, s, keyEnter)
	s.Update(cmd())

	press(t, s, keyDown) // move to the second hit, mark nothing
	press(t, s, keyEnter)

	if len(s.queue) != 1 || s.queue[0].URL != "u2" {
		t.Fatalf("queue = %+v, want just the row under the cursor (u2)", s.queue)
	}
}

// TestShellDownloadsQueue checks the queue reaches the backend and is cleared,
// so a second run cannot re-download the same URLs.
func TestShellDownloadsQueue(t *testing.T) {
	be := &fakeBackend{}
	s := NewShell(context.Background(), be)
	s.queue = []Item{{Label: "a", URL: "u1"}, {Label: "b", URL: "u2"}}

	_, cmd := s.run(actGo)
	if cmd == nil {
		t.Fatal("downloading a non-empty queue must return a command")
	}
	if len(s.queue) != 0 {
		t.Errorf("queue still holds %d items after starting; it must be consumed", len(s.queue))
	}
	if s.screen != scRunning || !s.busy {
		t.Errorf("screen = %v busy = %v, want scRunning and busy", s.screen, s.busy)
	}

	msg := cmd() // runs the fake download synchronously
	if strings.Join(be.got, ",") != "u1,u2" {
		t.Errorf("backend received %v, want [u1 u2]", be.got)
	}

	s.Update(msg)
	if s.busy {
		t.Error("shell still busy after the run reported completion")
	}
}

func TestShellEmptyQueueIsRefused(t *testing.T) {
	be := &fakeBackend{}
	s := NewShell(context.Background(), be)

	_, cmd := s.run(actGo)
	if cmd != nil {
		t.Error("an empty queue must not start a download")
	}
	if s.errMsg == "" {
		t.Error("an empty queue must explain itself in the footer")
	}
	if len(be.got) != 0 {
		t.Errorf("backend was called with %v despite the empty queue", be.got)
	}
}

func TestShellSearchErrorSurfaces(t *testing.T) {
	be := &fakeBackend{searchErr: errors.New("qobuz caído")}
	s := NewShell(context.Background(), be)

	press(t, s, keyEnter)
	typeRunes(t, s, "x")
	cmd := press(t, s, keyEnter)
	s.Update(cmd())

	if s.screen != scMenu {
		t.Errorf("a failed search must return to the menu, got screen %v", s.screen)
	}
	if !strings.Contains(s.errMsg, "qobuz caído") {
		t.Errorf("errMsg = %q, want the backend error", s.errMsg)
	}
}

// TestShellForwardsProgress pins the routing rule: messages the shell does not
// own belong to the embedded download Model. Without this the download screen
// stays blank while tracks are downloading.
func TestShellForwardsProgress(t *testing.T) {
	s := NewShell(context.Background(), &fakeBackend{})

	s.Update(MsgAlbum{Title: "In Rainbows", Artist: "Radiohead", Format: "FLAC", Tracks: 10})
	if s.dl.album != "In Rainbows" {
		t.Fatalf("download model album = %q, want it forwarded from the shell", s.dl.album)
	}

	s.Update(MsgRegisterTrack{ID: "t1", Num: 1, Name: "15 Step"})
	if len(s.dl.tracks) != 1 {
		t.Fatalf("download model holds %d tracks, want 1", len(s.dl.tracks))
	}
}

// TestShellQueueRemoval covers the one destructive key in the queue screen.
func TestShellQueueRemoval(t *testing.T) {
	s := NewShell(context.Background(), &fakeBackend{})
	s.queue = []Item{{Label: "a", URL: "u1"}, {Label: "b", URL: "u2"}}
	s.run(actQueue)

	press(t, s, keyDown)
	press(t, s, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})

	if len(s.queue) != 1 || s.queue[0].URL != "u1" {
		t.Fatalf("queue = %+v, want only u1 left", s.queue)
	}

	press(t, s, keyEsc)
	if s.screen != scMenu {
		t.Errorf("esc must return to the menu, got %v", s.screen)
	}
}

// TestShellLoginReachesBackend covers the entry that exists precisely for the
// user who cannot do anything else yet: no session, so every other Qobuz
// action fails until this one runs.
func TestShellLoginReachesBackend(t *testing.T) {
	be := &fakeBackend{}
	s := NewShell(context.Background(), be)

	_, cmd := s.run(actLogin)
	if cmd == nil {
		t.Fatal("the login entry must start work")
	}
	if !s.busy || s.screen != scRunning {
		t.Errorf("screen = %v busy = %v, want scRunning and busy", s.screen, s.busy)
	}

	msg := cmd()
	if !be.loggedIn {
		t.Error("login never reached the backend")
	}

	s.Update(msg)
	if s.busy {
		t.Error("shell still busy after login returned")
	}
	if s.errMsg != "" {
		t.Errorf("successful login left an error on screen: %q", s.errMsg)
	}
}

func TestShellLoginFailureSurfaces(t *testing.T) {
	be := &fakeBackend{loginErr: errors.New("no llegó el redirect")}
	s := NewShell(context.Background(), be)

	_, cmd := s.run(actLogin)
	s.Update(cmd())

	if !strings.Contains(s.errMsg, "no llegó el redirect") {
		t.Errorf("errMsg = %q, want the login error", s.errMsg)
	}
}

func TestShellPurgeReachesBackend(t *testing.T) {
	be := &fakeBackend{}
	s := NewShell(context.Background(), be)
	s.run(actPurge)
	if !be.purged {
		t.Error("the purge entry must reach the backend")
	}
}

// TestShellViewRendersEveryScreen is a smoke test for the drawing code, which
// is full of width arithmetic that panics if it ever goes negative
// (strings.Repeat with a negative count). A 20-column terminal is the case
// that finds it.
func TestShellViewRendersEveryScreen(t *testing.T) {
	be := &fakeBackend{hits: []Item{{Label: "un título bastante largo para truncar", URL: "u1"}}}

	for _, width := range []int{20, 80, 200} {
		s := NewShell(context.Background(), be)
		s.Update(tea.WindowSizeMsg{Width: width, Height: 24})
		s.queue = []Item{{Label: "en cola", URL: "u1"}}

		for _, sc := range []screen{scMenu, scInput, scResults, scQueue, scRunning, scText} {
			s.screen = sc
			s.refreshQueue()
			if out := s.View(); out == "" {
				t.Errorf("width %d screen %v rendered nothing", width, sc)
			}
		}
	}
}
