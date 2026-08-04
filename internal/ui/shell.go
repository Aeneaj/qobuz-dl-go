package ui

import (
	"context"
	"fmt"
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
	actQueue
	actGo
	actLyrics
	actCSV
	actConfig
	actPurge
	actQuit
)

type menuEntry struct {
	act   action
	label string
	hint  string
}

var menu = []menuEntry{
	{actSearchAlbum, "Buscar álbumes", "busca en Qobuz y añade a la cola"},
	{actSearchTrack, "Buscar canciones", "búsqueda por track"},
	{actSearchArtist, "Buscar artistas", "discografía completa"},
	{actSearchPlaylist, "Buscar playlists", "playlists de Qobuz"},
	{actURL, "Añadir URL", "álbum, track, artista, sello o Last.fm"},
	{actQueue, "Ver la cola", "revisar y quitar elementos"},
	{actGo, "Descargar la cola", "empieza la descarga"},
	{actLyrics, "Letras (.lrc)", "busca letras sincronizadas en LRCLIB"},
	{actCSV, "Importar CSV", "playlist exportada de TuneMyMusic"},
	{actConfig, "Configuración", "ver ajustes actuales"},
	{actPurge, "Borrar historial", "olvida lo ya descargado"},
	{actQuit, "Salir", ""},
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
		rows[i] = m.label
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
		s.status = "espacio marca · enter añade a la cola · esc vuelve"
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
		s.status = msg.summary + "  ·  enter para volver al menú"
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
			s.status = "cancelando…"
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
			s.status = fmt.Sprintf("%d en la cola", len(s.queue))
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
		s.field.reset("Buscar "+searchKind(a)+":", "")
		s.screen = scInput

	case actURL:
		s.pending = a
		s.field.reset("URL de Qobuz o Last.fm:", "")
		s.screen = scInput

	case actLyrics:
		s.pending = a
		s.field.reset("Carpeta que escanear:", s.be.DefaultDir())
		s.screen = scInput

	case actCSV:
		s.pending = a
		s.field.reset("Ruta del CSV:", "")
		s.screen = scInput

	case actQueue:
		s.refreshQueue()
		s.screen = scQueue
		s.status = "d quita · enter descarga · esc vuelve"

	case actGo:
		if len(s.queue) == 0 {
			s.errMsg = "la cola está vacía"
			s.screen = scMenu
			return s, nil
		}
		urls := make([]string, len(s.queue))
		for i, it := range s.queue {
			urls[i] = it.URL
		}
		s.queue = nil
		return s.start(runDownload, func(ctx context.Context) (string, error) {
			return "descarga terminada", s.be.Download(ctx, urls)
		})

	case actConfig:
		s.text = s.be.Config()
		s.screen = scText

	case actPurge:
		if err := s.be.Purge(); err != nil {
			s.errMsg = err.Error()
		} else {
			s.status = "historial de descargas borrado"
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
		s.status = "buscando…"
		s.screen = scRunning
		s.kind = runPlain
		return s, func() tea.Msg {
			items, err := s.be.Search(s.ctx, kind, val, 20)
			return msgResults{items: items, err: err}
		}

	case actURL:
		s.queue = append(s.queue, Item{Label: val, URL: val})
		s.status = fmt.Sprintf("%d en la cola", len(s.queue))
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
	s.status = "trabajando…"
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
	title := sBlue.Render("◈") + "  " + sBold.Foreground(cWhite).Render("QOBUZ-DL")
	right := sDim.Render(fmt.Sprintf("cola: %d", len(s.queue)))

	pad := w - 6 - lipgloss.Width(title) - lipgloss.Width(right)
	if pad < 1 {
		pad = 1
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(cGray).
		Padding(0, 1).
		Width(w - 2).
		Render(title + strings.Repeat(" ", pad) + right)
}

func (s *Shell) viewMenu(w int) string {
	var b strings.Builder
	for i, m := range menu {
		cursor := "   "
		label := sDim.Render(m.label)
		if i == s.menu.cursor {
			cursor = "  " + sBlue.Render("❯")
			label = sBold.Foreground(cWhite).Render(m.label)
		}
		line := cursor + " " + label
		if m.hint != "" && i == s.menu.cursor {
			line += "  " + sDim.Italic(true).Render(m.hint)
		}
		b.WriteString(truncate(line, w+40) + "\n")
	}
	return b.String()
}

func (s *Shell) viewFooter() string {
	if s.errMsg != "" {
		return "  " + sRed.Render("✗ "+s.errMsg)
	}
	if s.status != "" {
		return "  " + sDim.Render(s.status)
	}
	return "  " + sDim.Render("↑↓ moverse · enter elegir · q salir")
}
