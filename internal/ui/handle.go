package ui

import (
	"errors"
	"io"
	"sync/atomic"

	tea "github.com/charmbracelet/bubbletea"
)

// TrackHandle connects a single download goroutine to the bubbletea program.
// Byte progress is written via an atomic counter; the UI tick polls it.
// Only control events (SetTotal, Abort) go through p.Send().
type TrackHandle struct {
	id    string
	prog  *tea.Program
	bytes atomic.Int64 // written by Read()/IncrBy(); read by model on each tick
}

func NewTrackHandle(id string, prog *tea.Program) *TrackHandle {
	return &TrackHandle{id: id, prog: prog}
}

// Counter returns a pointer to the atomic byte counter so the model can poll it.
func (h *TrackHandle) Counter() *atomic.Int64 { return &h.bytes }

func (h *TrackHandle) SetTotal(total int64, triggerComplete bool) {
	h.prog.Send(MsgSetTotal{ID: h.id, Total: total})
	if triggerComplete {
		h.prog.Send(MsgDone{ID: h.id})
	}
}

// IncrBy is called for the resume fast-forward; just update the atomic.
func (h *TrackHandle) IncrBy(n int) {
	h.bytes.Add(int64(n))
}

func (h *TrackHandle) ProxyReader(r io.Reader) io.ReadCloser {
	rc, ok := r.(io.ReadCloser)
	if !ok {
		rc = io.NopCloser(r)
	}
	return &trackProxy{ReadCloser: rc, h: h}
}

// Abort signals the model directly; these are infrequent control events.
func (h *TrackHandle) Abort(drop bool) {
	if drop {
		h.prog.Send(MsgDone{ID: h.id})
	} else {
		h.prog.Send(MsgFailed{ID: h.id, Err: errors.New("download aborted")})
	}
}

// trackProxy wraps the response body. Read() only increments the atomic —
// no channel send, no allocation, no lock contention in the hot path.
type trackProxy struct {
	io.ReadCloser
	h *TrackHandle
}

func (p *trackProxy) Read(buf []byte) (int, error) {
	n, err := p.ReadCloser.Read(buf)
	if n > 0 {
		p.h.bytes.Add(int64(n))
	}
	return n, err
}
