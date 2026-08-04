package ui

import (
	"bytes"
	"io"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// recorder is a tea.Model that keeps every message the program's event loop
// receives. State lives behind pointers because bubbletea replaces the model
// with whatever Update returns.
type recorder struct {
	mu   *sync.Mutex
	msgs *[]tea.Msg
}

func (r recorder) Init() tea.Cmd { return nil }
func (r recorder) View() string  { return "" }
func (r recorder) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	r.mu.Lock()
	*r.msgs = append(*r.msgs, msg)
	r.mu.Unlock()
	return r, nil
}

// runRecorder starts a headless program and returns it plus a stop function
// that quits it and yields the messages TrackHandle sent. Only our own message
// types are returned — bubbletea injects its own (window size, etc.) and those
// would make the counts meaningless.
//
// Send blocks on an unbuffered channel until the loop consumes, so the program
// must already be running before any handle touches it.
func runRecorder(t *testing.T) (*tea.Program, func() []tea.Msg) {
	t.Helper()
	var (
		mu   sync.Mutex
		msgs []tea.Msg
	)
	p := tea.NewProgram(recorder{&mu, &msgs}, tea.WithoutRenderer(), tea.WithInput(nil))

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run() //nolint:errcheck
	}()

	return p, func() []tea.Msg {
		p.Quit()
		<-done
		mu.Lock()
		defer mu.Unlock()
		var ours []tea.Msg
		for _, m := range msgs {
			switch m.(type) {
			case MsgSetTotal, MsgDone, MsgFailed, MsgRegisterTrack, MsgAlbum:
				ours = append(ours, m)
			}
		}
		return ours
	}
}

// TestTrackHandleReadSendsNothing is the invariant the whole design rests on:
// Read must only touch the atomic. One p.Send() per Read floods the bubbletea
// event loop — with 6 workers pulling FLACs that is tens of thousands of
// messages a second, and the UI stops repainting.
func TestTrackHandleReadSendsNothing(t *testing.T) {
	p, stop := runRecorder(t)
	h := NewTrackHandle("t1", p)

	payload := bytes.Repeat([]byte("x"), 8192)
	rc := h.ProxyReader(bytes.NewReader(payload))
	n, err := io.Copy(io.Discard, rc)
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	rc.Close()

	if n != int64(len(payload)) {
		t.Errorf("copied %d bytes, want %d", n, len(payload))
	}
	if got := h.Counter().Load(); got != int64(len(payload)) {
		t.Errorf("counter = %d, want %d", got, len(payload))
	}
	if msgs := stop(); len(msgs) != 0 {
		t.Errorf("Read sent %d messages, want 0: %v", len(msgs), msgs)
	}
}

// The model polls Counter() on every tick, so the pointer must observe both
// proxied reads and the IncrBy used to fast-forward a resumed download.
func TestTrackHandleCounterAggregates(t *testing.T) {
	p, stop := runRecorder(t)
	defer stop()
	h := NewTrackHandle("t1", p)

	h.IncrBy(100) // resume fast-forward
	rc := h.ProxyReader(bytes.NewReader(bytes.Repeat([]byte("y"), 50)))
	io.Copy(io.Discard, rc) //nolint:errcheck
	rc.Close()
	h.IncrBy(7)

	if got := h.Counter().Load(); got != 157 {
		t.Errorf("counter = %d, want 157", got)
	}
}

// Six workers is the shipped default, and they all drive their own handle
// concurrently. Run with -race.
func TestTrackHandleConcurrentReads(t *testing.T) {
	p, stop := runRecorder(t)
	defer stop()
	h := NewTrackHandle("t1", p)

	const (
		workers  = 6
		perRead  = 1024
		numReads = 200
	)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rc := h.ProxyReader(bytes.NewReader(bytes.Repeat([]byte("z"), perRead*numReads)))
			io.Copy(io.Discard, rc) //nolint:errcheck
			rc.Close()
		}()
	}
	wg.Wait()

	want := int64(workers * perRead * numReads)
	if got := h.Counter().Load(); got != want {
		t.Errorf("counter = %d, want %d", got, want)
	}
}

// ProxyReader must accept a plain Reader. downloadWithProgress always calls
// Close on what it gets back, so a non-ReadCloser has to be wrapped rather
// than type-asserted.
func TestTrackHandleProxyReaderWrapsPlainReader(t *testing.T) {
	p, stop := runRecorder(t)
	defer stop()
	h := NewTrackHandle("t1", p)

	rc := h.ProxyReader(io.LimitReader(bytes.NewReader([]byte("abc")), 3))
	if _, err := io.Copy(io.Discard, rc); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Errorf("Close on wrapped plain reader: %v", err)
	}
}

// closeSpy reports whether the underlying body was closed through the proxy.
type closeSpy struct {
	io.Reader
	closed bool
}

func (c *closeSpy) Close() error { c.closed = true; return nil }

// When the source is already a ReadCloser the proxy must forward Close, or the
// HTTP response body leaks for the life of the download.
func TestTrackHandleProxyReaderForwardsClose(t *testing.T) {
	p, stop := runRecorder(t)
	defer stop()
	h := NewTrackHandle("t1", p)

	spy := &closeSpy{Reader: bytes.NewReader([]byte("abc"))}
	rc := h.ProxyReader(spy)
	io.Copy(io.Discard, rc) //nolint:errcheck
	if err := rc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !spy.closed {
		t.Error("Close did not reach the underlying ReadCloser")
	}
}

// Control events are the one thing that may go through p.Send(). Each maps to
// exactly one message; getting these wrong leaves a bar stuck on screen.
func TestTrackHandleControlEvents(t *testing.T) {
	cases := []struct {
		name string
		act  func(*TrackHandle)
		want []tea.Msg
	}{
		{
			"SetTotal without complete",
			func(h *TrackHandle) { h.SetTotal(500, false) },
			[]tea.Msg{MsgSetTotal{ID: "t1", Total: 500}},
		},
		{
			"SetTotal with complete also finishes the bar",
			func(h *TrackHandle) { h.SetTotal(500, true) },
			[]tea.Msg{MsgSetTotal{ID: "t1", Total: 500}, MsgDone{ID: "t1"}},
		},
		{
			"Abort(true) hides an already-downloaded track",
			func(h *TrackHandle) { h.Abort(true) },
			[]tea.Msg{MsgDone{ID: "t1"}},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, stop := runRecorder(t)
			c.act(NewTrackHandle("t1", p))
			got := stop()

			if len(got) != len(c.want) {
				t.Fatalf("got %d messages %v, want %d %v", len(got), got, len(c.want), c.want)
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Errorf("message %d = %#v, want %#v", i, got[i], c.want[i])
				}
			}
		})
	}
}

// Abort(false) is the failure path and carries an error, so it cannot be
// compared by value alongside the cases above.
func TestTrackHandleAbortFailureCarriesError(t *testing.T) {
	p, stop := runRecorder(t)
	NewTrackHandle("t1", p).Abort(false)
	got := stop()

	if len(got) != 1 {
		t.Fatalf("got %d messages %v, want 1", len(got), got)
	}
	failed, ok := got[0].(MsgFailed)
	if !ok {
		t.Fatalf("got %#v, want MsgFailed", got[0])
	}
	if failed.ID != "t1" {
		t.Errorf("ID = %q, want %q", failed.ID, "t1")
	}
	if failed.Err == nil {
		t.Error("MsgFailed.Err is nil — the model has nothing to display")
	}
}
