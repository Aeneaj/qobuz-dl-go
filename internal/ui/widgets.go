package ui

import (
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Two widgets, hand-rolled rather than pulled from bubbles: the shell needs a
// single-line field and a cursor list, and that is about sixty lines. Runes
// come off tea.KeyMsg already decoded, so non-ASCII input works without any
// extra care.

// textField is a single-line input. Editing is append/backspace only — no
// cursor movement, which is what a search query or a path actually needs.
type textField struct {
	prompt string
	value  []rune
}

func (t *textField) reset(prompt, initial string) {
	t.prompt = prompt
	t.value = []rune(initial)
}

func (t *textField) String() string { return string(t.value) }

func (t *textField) update(msg tea.KeyMsg) {
	switch msg.Type {
	case tea.KeyBackspace, tea.KeyDelete:
		if len(t.value) > 0 {
			t.value = t.value[:len(t.value)-1]
		}
	case tea.KeySpace:
		t.value = append(t.value, ' ')
	case tea.KeyRunes:
		t.value = append(t.value, msg.Runes...)
	}
}

func (t *textField) view(width int) string {
	line := sBold.Render(t.prompt) + "\n\n  " + sCyan.Render(string(t.value)) + sBlue.Render("▌")
	return lipgloss.NewStyle().Width(width - 2).Render(line)
}

// picker is a scrolling cursor list. Rows can be marked when multi is set,
// which is what turns search results into a queue selection.
type picker struct {
	rows   []string
	cursor int
	offset int
	height int
	multi  bool
	marked map[int]bool
}

func newPicker(rows []string, multi bool) picker {
	return picker{rows: rows, height: 10, multi: multi, marked: map[int]bool{}}
}

func (p *picker) update(msg tea.KeyMsg) {
	switch msg.String() {
	case "up", "k":
		if p.cursor > 0 {
			p.cursor--
		}
	case "down", "j":
		if p.cursor < len(p.rows)-1 {
			p.cursor++
		}
	case "home", "g":
		p.cursor = 0
	case "end", "G":
		p.cursor = len(p.rows) - 1
	case " ":
		if p.multi {
			p.marked[p.cursor] = !p.marked[p.cursor]
		}
	}
	// Keep the cursor inside the visible window.
	if p.cursor < p.offset {
		p.offset = p.cursor
	}
	if p.cursor >= p.offset+p.height {
		p.offset = p.cursor - p.height + 1
	}
}

// selection returns the marked indices, or the row under the cursor when
// nothing is marked — so Enter on a single row does the obvious thing.
func (p *picker) selection() []int {
	var out []int
	for i := range p.rows {
		if p.marked[i] {
			out = append(out, i)
		}
	}
	if len(out) == 0 && len(p.rows) > 0 {
		out = append(out, p.cursor)
	}
	return out
}

func (p *picker) view(width int) string {
	if len(p.rows) == 0 {
		return sDim.Render(T("  (empty)"))
	}

	var b strings.Builder
	end := min(p.offset+p.height, len(p.rows))

	for i := p.offset; i < end; i++ {
		mark := " "
		if p.multi {
			mark = sDim.Render("○")
			if p.marked[i] {
				mark = sGreen.Render("●")
			}
		}

		row := truncate(p.rows[i], width-8)
		if i == p.cursor {
			b.WriteString("  " + sBlue.Render("❯") + " " + mark + " " + sBold.Render(row))
		} else {
			b.WriteString("    " + mark + " " + sDim.Render(row))
		}
		b.WriteString("\n")
	}

	if len(p.rows) > p.height {
		b.WriteString(sDim.Render("    " + strconv.Itoa(p.cursor+1) + "/" + strconv.Itoa(len(p.rows))))
	}
	return b.String()
}

// rightAlign packs left and right onto one line of width w, pushing right to
// the far edge and keeping at least one space between them when they overflow.
func rightAlign(left, right string, w int) string {
	pad := w - lipgloss.Width(left) - lipgloss.Width(right)
	if pad < 1 {
		pad = 1
	}
	return left + strings.Repeat(" ", pad) + right
}

func truncate(s string, width int) string {
	if width < 4 {
		width = 4
	}
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	return string(r[:width-1]) + "…"
}
