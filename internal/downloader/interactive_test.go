package downloader

import (
	"context"
	"strings"
	"testing"
)

// Piped input — scripts, `printf ... | qobuz-dl fun` — reaches the result
// picker. A second bufio.Reader on stdin used to find the pick already
// swallowed by the first one's buffer: the "1" came back as an unknown
// command and the queue stayed empty.
func TestInteractivePipedPickReachesQueue(t *testing.T) {
	q := newFakeQobuz(t, nil)
	d, _ := newTestDownloader(t, q, nil)

	out := captureStdout(t, func() {
		d.interactive(context.Background(), strings.NewReader("sa test\n1\nq\nexit\n"))
	})

	if strings.Contains(out, "Unknown command") {
		t.Errorf("the pick was read as a command:\n%s", out)
	}
	if !strings.Contains(out, "1 item(s) added to queue") {
		t.Errorf("pick did not reach the queue:\n%s", out)
	}
}

// The usage line was built from the first letter of the type, so "sr" told the
// user to type "sa", which searches albums.
func TestInteractiveSearchUsageNamesItsCommand(t *testing.T) {
	d, _ := newTestDownloader(t, newFakeQobuz(t, nil), nil)

	for _, cmd := range []string{"sa", "st", "sr", "sp"} {
		t.Run(cmd, func(t *testing.T) {
			out := captureStdout(t, func() {
				d.interactive(context.Background(), strings.NewReader(cmd+"\nexit\n"))
			})
			if want := "Usage: " + cmd + " <query>"; !strings.Contains(out, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
		})
	}
}
