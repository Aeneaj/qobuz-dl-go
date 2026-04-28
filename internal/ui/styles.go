package ui

import "github.com/charmbracelet/lipgloss"

// Color palette
var (
	cWhite  = lipgloss.Color("#FFFFFF")
	cBlue   = lipgloss.Color("#00AFFF")
	cGreen  = lipgloss.Color("#00FF87")
	cRed    = lipgloss.Color("#FF5F5F")
	cCyan   = lipgloss.Color("#00FFFF")
	cGray   = lipgloss.Color("#555555")
	cBlack  = lipgloss.Color("#000000")
)

// Base styles
var (
	sBold     = lipgloss.NewStyle().Bold(true)
	sDim      = lipgloss.NewStyle().Foreground(cGray)
	sBlue     = lipgloss.NewStyle().Bold(true).Foreground(cBlue)
	sGreen    = lipgloss.NewStyle().Bold(true).Foreground(cGreen)
	sRed      = lipgloss.NewStyle().Bold(true).Foreground(cRed)
	sCyan     = lipgloss.NewStyle().Foreground(cCyan)
	sBadgeDone = lipgloss.NewStyle().Bold(true).
			Background(cGreen).Foreground(cBlack).Padding(0, 1)
	sBadgeErr = lipgloss.NewStyle().Bold(true).
			Background(cRed).Foreground(cBlack).Padding(0, 1)
)
