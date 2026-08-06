package ui

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// The shell is a small state machine: one screen is active at a time, Enter
// moves forward, Esc moves back. Every blocking call goes through a tea.Cmd
// so the render loop keeps running, and the download screen is the existing
// progress Model embedded as one more screen.

type screen int

const (
	scMenu screen = iota
	scInput
	scResults
	scQueue
	scRunning
	scText
)

type action int

const (
	actSearchAlbum action = iota
	actSearchTrack
	actSearchArtist
	actSearchPlaylist
	actURL
	actLogin
	actQueue
	actGo
	actLyrics
	actCSV
	actConfig
	actPurge
	actQuit
)

type menuEntry struct {
	act     action
	icon    string
	label   string
	hint    string
	section string         // rendered as a heading when it changes
	style   lipgloss.Style // colour of the icon
}

// Entries are grouped by section and coloured by purpose: blue to find things,
// cyan to move them through the queue, yellow for tools, purple for the
// session. Sections are drawn from this table, never selectable themselves.
var menu = []menuEntry{
	{actSearchAlbum, "♫", "Search albums", "search Qobuz and add to the queue", "FIND", sBlue},
	{actSearchTrack, "♪", "Search tracks", "search by track", "", sBlue},
	{actSearchArtist, "◈", "Search artists", "full discography", "", sBlue},
	{actSearchPlaylist, "≡", "Search playlists", "Qobuz playlists", "", sBlue},

	{actURL, "+", "Add URL", "album, track, artist, label or Last.fm", "QUEUE", sCyan},
	{actQueue, "▤", "View the queue", "review and remove items", "", sCyan},
	{actGo, "⬇", "Download the queue", "start downloading", "", sCyan},

	{actLyrics, "♬", "Lyrics (.lrc)", "find synced lyrics on LRCLIB", "TOOLS", sYellow},
	{actCSV, "⇪", "Import CSV", "playlist exported from TuneMyMusic", "", sYellow},
	{actConfig, "⚙", "Settings", "show current settings", "", sYellow},
	{actPurge, "✖", "Clear history", "forget what was already downloaded", "", sYellow},

	{actLogin, "⚿", "Log in (OAuth)", "opens Qobuz in your browser", "SESSION", sPurple},
	{actQuit, "⏻", "Quit", "", "", sPurple},
}

// runKind tells the running screen what to draw: per-track bars for the
// download flows, a plain status line for the rest.
type runKind int

const (
	runDownload runKind = iota
	runPlain
)

// ---- messages ---------------------------------------------------------------

// MsgStatus is a one-line progress update from a backend operation that has no
// per-track bars of its own (the lyrics scan, mostly).
type MsgStatus struct{ Text string }

type msgResults struct {
	items []Item
	err   error
}

type msgRunDone struct {
	summary string
	err     error
}

// ---- model ------------------------------------------------------------------

// Shell is the full-program TUI: menu, search, queue, and the download view.
type Shell struct {
	be  Backend
	ctx context.Context

	screen  screen
	prev    screen
	menu    picker
	results picker
	queue   []Item
	hits    []Item
	field   textField
	dl      Model

	pending action  // what the current input screen feeds
	kind    runKind // what the running screen is showing
	cancel  context.CancelFunc
	busy    bool

	status string
	errMsg string
	text   string // scratch buffer for the config screen

	width, height int
}

func NewShell(ctx context.Context, be Backend) *Shell {
	rows := make([]string, len(menu))
	for i, m := range menu {
		rows[i] = T(m.label)
	}
	s := &Shell{
		be:     be,
		ctx:    ctx,
		menu:   newPicker(rows, false),
		dl:     NewModel(),
		width:  80,
		height: 24,
	}
	s.menu.height = len(menu)
	return s
}

func (s *Shell) Init() tea.Cmd { return nil }

func (s *Shell) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		s.width, s.height = msg.Width, msg.Height
		s.results.height = max(5, msg.Height-14)
		m, _ := s.dl.Update(msg)
		s.dl = m.(Model)
		return s, nil

	case tea.KeyMsg:
		return s.onKey(msg)

	case msgResults:
		s.busy = false
		if msg.err != nil {
			s.errMsg = msg.err.Error()
			s.screen = scMenu
			return s, nil
		}
		s.hits = msg.items
		rows := make([]string, len(msg.items))
		for i, it := range msg.items {
			rows[i] = it.Label
		}
		s.results = newPicker(rows, true)
		s.results.height = max(5, s.height-14)
		s.screen = scResults
		s.status = T("space marks · enter adds to the queue · esc goes back")
		return s, nil

	case msgRunDone:
		s.busy = false
		if s.cancel != nil {
			s.cancel()
			s.cancel = nil
		}
		if msg.err != nil {
			s.errMsg = msg.err.Error()
		}
		s.status = msg.summary + T("  ·  enter to return to the menu")
		return s, nil

	case MsgStatus:
		s.status = msg.Text
		return s, nil
	}

	// Anything else is download progress destined for the embedded Model.
	m, cmd := s.dl.Update(msg)
	s.dl = m.(Model)
	return s, cmd
}

func (s *Shell) onKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	// Ctrl+C aborts the running job first; only an idle shell quits on it.
	if key == "ctrl+c" {
		if s.busy && s.cancel != nil {
			s.cancel()
			s.status = T("cancelling…")
			return s, nil
		}
		return s, tea.Quit
	}

	switch s.screen {

	case scMenu:
		if key == "enter" {
			return s.run(menu[s.menu.cursor].act)
		}
		if key == "q" {
			return s, tea.Quit
		}
		s.menu.update(msg)

	case scInput:
		switch key {
		case "esc":
			s.screen = scMenu
		case "enter":
			return s.submit()
		default:
			s.field.update(msg)
		}

	case scResults:
		switch key {
		case "esc":
			s.screen = scMenu
		case "enter":
			for _, i := range s.results.selection() {
				s.queue = append(s.queue, s.hits[i])
			}
			s.status = fmt.Sprintf(T("%d in the queue"), len(s.queue))
			s.screen = scMenu
		default:
			s.results.update(msg)
		}

	case scQueue:
		switch key {
		case "esc":
			s.screen = scMenu
		case "d", "delete", "backspace":
			if i := s.results.cursor; i < len(s.queue) {
				s.queue = append(s.queue[:i], s.queue[i+1:]...)
				s.refreshQueue()
			}
		case "enter":
			return s.run(actGo)
		default:
			s.results.update(msg)
		}

	case scText:
		if key == "esc" || key == "enter" || key == "q" {
			s.screen = scMenu
		}

	case scRunning:
		if !s.busy && (key == "enter" || key == "esc") {
			s.screen = scMenu
			s.status = ""
		}
	}

	return s, nil
}

// run reacts to a menu choice: either it opens an input screen, or it starts
// the work straight away.
func (s *Shell) run(a action) (tea.Model, tea.Cmd) {
	s.errMsg = ""

	switch a {
	case actSearchAlbum, actSearchTrack, actSearchArtist, actSearchPlaylist:
		s.pending = a
		s.field.reset(T("Search ")+searchKind(a)+":", "")
		s.screen = scInput

	case actURL:
		s.pending = a
		s.field.reset(T("Qobuz or Last.fm URL:"), "")
		s.screen = scInput

	case actLyrics:
		s.pending = a
		s.field.reset(T("Folder to scan:"), s.be.DefaultDir())
		s.screen = scInput

	case actCSV:
		s.pending = a
		s.field.reset(T("Path to the CSV:"), "")
		s.screen = scInput

	case actQueue:
		s.refreshQueue()
		s.screen = scQueue
		s.status = T("d removes · enter downloads · esc goes back")

	case actGo:
		if len(s.queue) == 0 {
			s.errMsg = T("the queue is empty")
			s.screen = scMenu
			return s, nil
		}
		urls := make([]string, len(s.queue))
		for i, it := range s.queue {
			urls[i] = it.URL
		}
		s.queue = nil
		return s.start(runDownload, func(ctx context.Context) (string, error) {
			return T("download finished"), s.be.Download(ctx, urls)
		})

	case actLogin:
		// The backend leaves the alt screen to run the CLI login, so the
		// shell must not repaint until it is done: no progress screen here,
		// just a status line that is already on screen when we hand over.
		return s.start(runPlain, s.be.Login)

	case actConfig:
		s.text = s.be.Config()
		s.screen = scText

	case actPurge:
		if err := s.be.Purge(); err != nil {
			s.errMsg = err.Error()
		} else {
			s.status = T("download history cleared")
		}
		s.screen = scMenu

	case actQuit:
		return s, tea.Quit
	}

	return s, nil
}

// submit consumes whatever the input screen collected.
func (s *Shell) submit() (tea.Model, tea.Cmd) {
	val := strings.TrimSpace(s.field.String())
	if val == "" {
		s.screen = scMenu
		return s, nil
	}

	switch s.pending {
	case actSearchAlbum, actSearchTrack, actSearchArtist, actSearchPlaylist:
		kind := searchKind(s.pending)
		s.busy = true
		s.status = T("searching…")
		s.screen = scRunning
		s.kind = runPlain
		return s, func() tea.Msg {
			items, err := s.be.Search(s.ctx, kind, val, 20)
			return msgResults{items: items, err: err}
		}

	case actURL:
		s.queue = append(s.queue, Item{Label: val, URL: val})
		s.status = fmt.Sprintf(T("%d in the queue"), len(s.queue))
		s.screen = scMenu

	case actLyrics:
		return s.start(runPlain, func(ctx context.Context) (string, error) {
			return s.be.Lyrics(ctx, val)
		})

	case actCSV:
		return s.start(runDownload, func(ctx context.Context) (string, error) {
			return s.be.CSV(ctx, val)
		})
	}

	return s, nil
}

// start launches a blocking backend call on its own cancellable context and
// switches to the running screen.
func (s *Shell) start(k runKind, fn func(context.Context) (string, error)) (tea.Model, tea.Cmd) {
	ctx, cancel := context.WithCancel(s.ctx)
	s.cancel = cancel
	s.busy = true
	s.kind = k
	s.screen = scRunning
	s.status = T("working…")
	s.dl = NewModel()
	s.dl.width = s.width

	return s, func() tea.Msg {
		summary, err := fn(ctx)
		return msgRunDone{summary: summary, err: err}
	}
}

func (s *Shell) refreshQueue() {
	rows := make([]string, len(s.queue))
	for i, it := range s.queue {
		rows[i] = it.Label
	}
	s.results = newPicker(rows, false)
	s.results.height = max(5, s.height-14)
}

func searchKind(a action) string {
	switch a {
	case actSearchTrack:
		return "track"
	case actSearchArtist:
		return "artist"
	case actSearchPlaylist:
		return "playlist"
	default:
		return "album"
	}
}

// ---- view -------------------------------------------------------------------

func (s *Shell) View() string {
	w := max(40, s.width)

	var body string
	switch s.screen {
	case scMenu:
		body = s.viewMenu(w)
	case scInput:
		body = s.field.view(w)
	case scResults:
		body = s.results.view(w)
	case scQueue:
		body = s.results.view(w)
	case scText:
		body = sDim.Render(s.text)
	case scRunning:
		if s.kind == runDownload {
			body = s.dl.View()
		} else {
			body = "  " + sBlue.Render(s.status)
		}
	}

	var b strings.Builder
	b.WriteString(s.viewHeader(w))
	b.WriteString("\n\n")
	b.WriteString(body)
	b.WriteString("\n")
	b.WriteString(sDim.Render(strings.Repeat("─", w)))
	b.WriteString("\n")
	b.WriteString(s.viewFooter())
	return b.String()
}

func (s *Shell) viewHeader(w int) string {
	title := sBlue.Render("◈") + " " + sBold.Foreground(cWhite).Render("QOBUZ") +
		sBlue.Render("-") + sBold.Foreground(cWhite).Render("DL")

	// Session state belongs in the chrome: without it the only way to find out
	// there is no token is to run a search and watch it fail.
	session := sRed.Render("○") + " " + sDim.Render(T("not signed in"))
	if s.be.LoggedIn() {
		session = sGreen.Render("●") + " " + sDim.Render(T("signed in"))
	}

	queue := sDim.Render(T("queue "))
	if len(s.queue) > 0 {
		queue += sCyan.Render(strconv.Itoa(len(s.queue)))
	} else {
		queue += sFaint.Render("0")
	}

	right := session + sFaint.Render("  │  ") + queue
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(cFaint).
		Padding(0, 1).
		Width(w - 2).
		Render(rightAlign(title, right, w-6))
}

func (s *Shell) viewMenu(w int) string {
	var b strings.Builder

	for i, m := range menu {
		if m.section != "" {
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString("  " + sSection.Render(T(m.section)) + "\n")
		}

		if i == s.menu.cursor {
			// The selected row is painted edge to edge, hint included, so the
			// cursor is visible at a glance instead of being one bold word.
			text := " " + m.icon + "  " + T(m.label)
			if m.hint != "" {
				text += "  ·  " + T(m.hint)
			}
			text = rightAlign(truncate(text, w-6), "", w-4)
			b.WriteString("  " + sSelected.Render(text) + "\n")
			continue
		}

		b.WriteString("   " + m.style.Render(m.icon) + "  " + sDim.Render(T(m.label)) + "\n")
	}
	return b.String()
}

func (s *Shell) viewFooter() string {
	if s.errMsg != "" {
		return "  " + sBadgeErr.Render("ERROR") + " " + sRed.Render(s.errMsg)
	}
	if s.status != "" {
		return "  " + sYellow.Render("▸ ") + sDim.Render(s.status)
	}

	hints := []string{
		keyHint("↑↓", T("move")),
		keyHint("⏎", T("choose")),
		keyHint("esc", T("back")),
		keyHint("q", T("quit")),
	}
	return "  " + strings.Join(hints, sFaint.Render("   "))
}
