// Package ui implements a bubbletea TUI for concurrent download progress.
package ui

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ── message types ────────────────────────────────────────────────────────────

// MsgRegisterTrack registers a new track before its download begins.
// Counter is a pointer to the TrackHandle's atomic byte counter; the model
// polls it on every tick instead of receiving per-read messages.
type MsgRegisterTrack struct {
	ID      string
	Num     int
	Name    string
	Counter *atomic.Int64
}

type MsgSetTotal struct {
	ID    string
	Total int64
}

type MsgDone struct {
	ID string
}

type MsgFailed struct {
	ID  string
	Err error
}

// MsgAlbum sets the header metadata displayed at the top of the TUI.
type MsgAlbum struct {
	Title  string
	Artist string
	Format string
	Tracks int
}

type msgTick time.Time

// ── track state ───────────────────────────────────────────────────────────────

type trackState int

const (
	statePending trackState = iota
	stateActive
	stateDone
	stateFailed
)

type trackEntry struct {
	id         string
	num        int
	name       string
	state      trackState
	current    int64  // last polled byte count
	prevRead   int64  // atomic value at previous tick, used for speed delta
	total      int64
	counter    *atomic.Int64 // points to TrackHandle.bytes; nil for pending tracks
	errMsg     string
	speed      float64 // bytes/s, updated every 500ms
	speedAccum int64
	lastSpeed  time.Time
}

// ── model ────────────────────────────────────────────────────────────────────

// Model is the bubbletea model for the download TUI.
type Model struct {
	tracks      []trackEntry
	index       map[string]int
	album       string
	artist      string
	format      string
	totalTracks int
	done        int
	failed      int
	width       int
	shimmer     int  // 0–19, cycles every tick to animate active bars
	ticking     bool // true while at least one track is active
}

func NewModel() Model {
	return Model{
		index: make(map[string]int),
		width: 80,
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(t time.Time) tea.Msg {
		return msgTick(t)
	})
}

func (m Model) Init() tea.Cmd { return nil }

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width

	case MsgAlbum:
		m.album = msg.Title
		m.artist = msg.Artist
		m.format = msg.Format
		m.totalTracks = msg.Tracks
		m.tracks = nil
		m.index = make(map[string]int)
		m.done = 0
		m.failed = 0

	case MsgRegisterTrack:
		e := trackEntry{
			id:        msg.ID,
			num:       msg.Num,
			name:      msg.Name,
			state:     statePending,
			counter:   msg.Counter,
			lastSpeed: time.Now(),
		}
		m.index[msg.ID] = len(m.tracks)
		m.tracks = append(m.tracks, e)

	case MsgSetTotal:
		if i, ok := m.index[msg.ID]; ok {
			m.tracks[i].total = msg.Total
			if m.tracks[i].state == statePending {
				m.tracks[i].state = stateActive
				m.tracks[i].lastSpeed = time.Now()
			}
		}
		if !m.ticking {
			m.ticking = true
			return m, tickCmd()
		}

	case MsgDone:
		if i, ok := m.index[msg.ID]; ok {
			e := &m.tracks[i]
			// Drain any remaining bytes before marking done.
			if e.counter != nil {
				e.current = e.counter.Load()
			}
			if e.state != stateDone {
				e.state = stateDone
				m.done++
			}
		}
		if !m.hasActive() {
			m.ticking = false
		}

	case MsgFailed:
		if i, ok := m.index[msg.ID]; ok {
			if m.tracks[i].state != stateFailed {
				m.tracks[i].state = stateFailed
				if msg.Err != nil {
					m.tracks[i].errMsg = msg.Err.Error()
				}
				m.failed++
			}
		}
		if !m.hasActive() {
			m.ticking = false
		}

	case msgTick:
		// Poll atomic counters — zero channel pressure on the download hot path.
		m.shimmer = (m.shimmer + 1) % 20
		now := time.Time(msg)
		for i := range m.tracks {
			e := &m.tracks[i]
			if e.state != stateActive || e.counter == nil {
				continue
			}
			newVal := e.counter.Load()
			delta := newVal - e.prevRead
			if delta > 0 {
				e.current = newVal
				e.speedAccum += delta
				e.prevRead = newVal
			}
			dt := now.Sub(e.lastSpeed).Seconds()
			if dt >= 0.5 && e.speedAccum > 0 {
				e.speed = float64(e.speedAccum) / dt
				e.speedAccum = 0
				e.lastSpeed = now
			}
		}
		if m.hasActive() {
			return m, tickCmd()
		}
		m.ticking = false

	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
	}

	return m, nil
}

func (m Model) hasActive() bool {
	for _, e := range m.tracks {
		if e.state == stateActive {
			return true
		}
	}
	return false
}

// ── view ─────────────────────────────────────────────────────────────────────

func (m Model) View() string {
	w := m.width
	if w < 40 {
		w = 40
	}

	var b strings.Builder

	b.WriteString(m.viewHeader(w))
	b.WriteString("\n\n")
	b.WriteString(m.viewGlobal(w))
	b.WriteString("\n\n")
	b.WriteString(sDim.Render(strings.Repeat("─", w)))
	b.WriteString("\n\n")

	for _, e := range m.tracks {
		b.WriteString(m.viewTrack(e, w))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(sDim.Render(strings.Repeat("─", w)))
	b.WriteString("\n")
	b.WriteString(m.viewFooter())

	return b.String()
}

func (m Model) viewHeader(w int) string {
	brand := sBlue.Render("◈") + "  " + sBold.Foreground(cWhite).Render("QOBUZ-DL")

	meta := ""
	if m.album != "" {
		var parts []string
		if m.artist != "" {
			parts = append(parts, m.artist)
		}
		parts = append(parts, m.album)
		if m.format != "" {
			parts = append(parts, m.format)
		}
		meta = sDim.Italic(true).Render(strings.Join(parts, " · "))
	}

	inner := brand
	if meta != "" {
		brandW := lipgloss.Width(brand)
		metaW := lipgloss.Width(meta)
		pad := w - 6 - brandW - metaW
		if pad > 0 {
			inner += strings.Repeat(" ", pad) + meta
		} else {
			inner += "  " + meta
		}
	}

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(cGray).
		Padding(0, 1).
		Width(w - 2).
		Render(inner)
}

func (m Model) viewGlobal(w int) string {
	if m.totalTracks == 0 {
		return ""
	}
	done := m.done
	total := m.totalTracks

	label := sDim.Render("LOTE")
	counter := sBlue.Render(fmt.Sprintf("%d/%d", done, total))
	pct := sBlue.Bold(true).Render(fmt.Sprintf("%3.0f%%", float64(done)/float64(total)*100))

	usedW := lipgloss.Width(label) + lipgloss.Width(counter) + lipgloss.Width(pct) + 6
	barW := w - 2 - usedW
	if barW < 10 {
		barW = 10
	}
	bar := drawBar(int64(done), int64(total), barW, stateActive, 0)

	return fmt.Sprintf("  %s  %s  %s  %s", label, bar, counter, pct)
}

func (m Model) viewTrack(e trackEntry, w int) string {
	var icon string
	switch e.state {
	case statePending:
		icon = sDim.Render("○")
	case stateActive:
		icon = sBlue.Render("⬇")
	case stateDone:
		icon = sGreen.Render("✓")
	case stateFailed:
		icon = sRed.Render("✗")
	}

	nameTxt := e.displayName()
	var nameRend string
	if e.state == stateActive || e.state == statePending {
		nameRend = sBold.Foreground(cWhite).Render(nameTxt)
	} else {
		nameRend = sDim.Render(nameTxt)
	}

	firstLine := "  " + icon + "  " + nameRend

	// Badge and size on first line for terminal states
	switch e.state {
	case stateDone:
		badge := sBadgeDone.Render("LISTO")
		size := sDim.Render(fmtBytes(e.current))
		rightPart := badge + " " + size
		pad := w - lipgloss.Width(firstLine) - lipgloss.Width(rightPart) - 1
		if pad > 0 {
			firstLine += strings.Repeat(" ", pad)
		} else {
			firstLine += " "
		}
		firstLine += rightPart
	case stateFailed:
		badge := sBadgeErr.Render("ERROR")
		pad := w - lipgloss.Width(firstLine) - lipgloss.Width(badge) - 1
		if pad > 0 {
			firstLine += strings.Repeat(" ", pad)
		} else {
			firstLine += " "
		}
		firstLine += badge
	}

	lines := []string{firstLine}

	// Second line: bar or error message
	indent := "     "
	barW := w - len(indent) - 1
	if barW < 10 {
		barW = 10
	}

	switch e.state {
	case stateActive:
		bar := drawBar(e.current, e.total, barW, stateActive, m.shimmer)
		lines = append(lines, indent+bar)

		// Stats line
		var stats []string
		if e.total > 0 {
			stats = append(stats, sBlue.Render(fmt.Sprintf("%3.0f%%", float64(e.current)/float64(e.total)*100)))
		}
		if e.speed > 0 {
			stats = append(stats, sCyan.Render(fmtSpeed(e.speed)))
			if e.total > 0 && e.current < e.total {
				rem := float64(e.total-e.current) / e.speed
				stats = append(stats, sDim.Render("ETA "+fmtETA(rem)))
			}
		}
		if len(stats) > 0 {
			lines = append(lines, indent+strings.Join(stats, "  "))
		}

	case stateDone:
		lines = append(lines, indent+drawBar(1, 1, barW, stateDone, 0))

	case stateFailed:
		if e.errMsg != "" {
			lines = append(lines, indent+sRed.Faint(true).Render(e.errMsg))
		}
	}

	return strings.Join(lines, "\n")
}

func (m Model) viewFooter() string {
	active := 0
	for _, e := range m.tracks {
		if e.state == stateActive {
			active++
		}
	}
	parts := []string{sDim.Render(fmt.Sprintf("%d completadas", m.done))}
	if m.failed > 0 {
		parts = append(parts, sRed.Render(fmt.Sprintf("%d errores", m.failed)))
	}
	if active > 0 {
		parts = append(parts, sBlue.Render(fmt.Sprintf("%d activas", active)))
	}
	parts = append(parts, sDim.Render("Ctrl+C cancelar"))
	return "  " + strings.Join(parts, sDim.Render("  ·  "))
}

func (e trackEntry) displayName() string {
	if e.num > 0 {
		return fmt.Sprintf("%02d · %s", e.num, e.name)
	}
	return e.name
}

// ── progress bar ──────────────────────────────────────────────────────────────

func drawBar(current, total int64, width int, state trackState, shimmer int) string {
	if width <= 0 {
		return ""
	}

	switch state {
	case stateDone:
		return sGreen.Render(strings.Repeat("█", width))
	case stateFailed:
		return sRed.Faint(true).Render(strings.Repeat("░", width))
	}

	var pct float64
	if total > 0 {
		pct = float64(current) / float64(total)
		if pct > 1 {
			pct = 1
		}
	}
	filled := int(pct * float64(width))

	var sb strings.Builder
	for i := 0; i < width; i++ {
		switch {
		case i < filled-1:
			sb.WriteString(sBlue.Render("█"))
		case i == filled-1:
			// Shimmer: tip alternates between white "▌" and blue "█"
			if shimmer%4 < 2 {
				sb.WriteString(lipgloss.NewStyle().Foreground(cWhite).Bold(true).Render("▌"))
			} else {
				sb.WriteString(sBlue.Render("█"))
			}
		default:
			sb.WriteString(sDim.Render("░"))
		}
	}
	return sb.String()
}

// ── format helpers ────────────────────────────────────────────────────────────

func fmtBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func fmtSpeed(bps float64) string {
	return fmtBytes(int64(bps)) + "/s"
}

func fmtETA(secs float64) string {
	if secs < 0 {
		secs = 0
	}
	d := time.Duration(secs) * time.Second
	h := int(d.Hours())
	min := int(d.Minutes()) % 60
	sec := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, min, sec)
	}
	return fmt.Sprintf("%d:%02d", min, sec)
}
