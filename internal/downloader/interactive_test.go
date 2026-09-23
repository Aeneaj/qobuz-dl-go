package downloader

import (
	"context"
	"io"
	"os"
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

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	d.interactive(context.Background(), strings.NewReader("sa test\n1\nq\nexit\n"))
	os.Stdout = stdout
	w.Close()
	out, _ := io.ReadAll(r)

	if strings.Contains(string(out), "Unknown command") {
		t.Errorf("the pick was read as a command:\n%s", out)
	}
	if !strings.Contains(string(out), "1 item(s) added to queue") {
		t.Errorf("pick did not reach the queue:\n%s", out)
	}
}
