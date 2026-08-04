package ui

import "context"

// Item is one row the shell can act on: a search hit or a pasted URL.
type Item struct {
	Label string
	URL   string
}

// Backend is everything the shell needs from the rest of the program.
//
// It exists to break an import cycle, not to abstract for its own sake:
// internal/downloader already imports this package for MsgAlbum and
// TrackHandle, so the shell cannot import the downloader back. cmd/qobuz-dl
// owns the one real implementation and wires it in; tests pass a fake.
//
// Long-running methods block, so the shell always calls them from a tea.Cmd.
// They report progress by sending Msg* values into the program — which is why
// the implementation, not the shell, holds the *tea.Program.
type Backend interface {
	// LoggedIn reports whether a Qobuz session exists. The header shows it, so
	// "no session" is visible before an action fails rather than after.
	LoggedIn() bool

	// Login authenticates with Qobuz. The implementation may take over the
	// terminal while it runs — the shell must not draw until it returns.
	Login(ctx context.Context) (string, error)

	// Search returns up to limit hits. kind is album|track|artist|playlist.
	Search(ctx context.Context, kind, query string, limit int) ([]Item, error)

	// Download fetches every URL, reporting per-track progress via MsgAlbum,
	// MsgRegisterTrack, MsgSetTotal, MsgDone and MsgFailed.
	Download(ctx context.Context, urls []string) error

	// Lyrics scans dir for audio files and fetches .lrc siblings, reporting
	// progress via MsgStatus. The returned string is a one-line summary.
	Lyrics(ctx context.Context, dir string) (string, error)

	// CSV batch-downloads a TuneMyMusic export, reporting per-track progress
	// the same way Download does.
	CSV(ctx context.Context, path string) (string, error)

	// Config returns the current configuration as displayable text.
	Config() string

	// Purge deletes the downloads database.
	Purge() error

	// DefaultDir is the configured download directory, used to prefill the
	// lyrics path prompt.
	DefaultDir() string
}
