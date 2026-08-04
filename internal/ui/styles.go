package ui

import "github.com/charmbracelet/lipgloss"

// Colour palette. Hex values degrade to the nearest ANSI colour on terminals
// without truecolor, so they stay readable on 256-colour and 16-colour ones.
var (
	cWhite  = lipgloss.Color("#FFFFFF")
	cBlue   = lipgloss.Color("#00AFFF")
	cGreen  = lipgloss.Color("#00FF87")
	cRed    = lipgloss.Color("#FF5F5F")
	cCyan   = lipgloss.Color("#00FFFF")
	cYellow = lipgloss.Color("#FFD75F")
	cPurple = lipgloss.Color("#AF87FF")
	cGray   = lipgloss.Color("#6C6C6C")
	cFaint  = lipgloss.Color("#3A3A3A")
	cBlack  = lipgloss.Color("#000000")
)

// Base styles
var (
	sBold   = lipgloss.NewStyle().Bold(true)
	sDim    = lipgloss.NewStyle().Foreground(cGray)
	sFaint  = lipgloss.NewStyle().Foreground(cFaint)
	sBlue   = lipgloss.NewStyle().Bold(true).Foreground(cBlue)
	sGreen  = lipgloss.NewStyle().Bold(true).Foreground(cGreen)
	sRed    = lipgloss.NewStyle().Bold(true).Foreground(cRed)
	sYellow = lipgloss.NewStyle().Foreground(cYellow)
	sPurple = lipgloss.NewStyle().Foreground(cPurple)
	sCyan   = lipgloss.NewStyle().Foreground(cCyan)

	// sSelected fills the whole row, which is what makes the cursor read as a
	// selection rather than as one more line of text.
	sSelected = lipgloss.NewStyle().Bold(true).Background(cBlue).Foreground(cBlack)

	// sSection labels a group of menu entries.
	sSection = lipgloss.NewStyle().Bold(true).Foreground(cFaint)

	sBadgeDone = lipgloss.NewStyle().Bold(true).
			Background(cGreen).Foreground(cBlack).Padding(0, 1)
	sBadgeErr = lipgloss.NewStyle().Bold(true).
			Background(cRed).Foreground(cBlack).Padding(0, 1)
)

// keyHint renders one "⏎ elegir" pair for the footer: the key stands out, the
// word it maps to stays quiet.
func keyHint(key, desc string) string {
	return sCyan.Render(key) + " " + sDim.Render(desc)
}
