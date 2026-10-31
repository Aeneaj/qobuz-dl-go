package downloader

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fastStall shrinks stallTimeout for the duration of a test.
func fastStall(t *testing.T) {
	old := stallTimeout
	stallTimeout = 200 * time.Millisecond
	t.Cleanup(func() { stallTimeout = old })
}

// audioServer serves payload, calling pace before each chunk of the body.
// pace gets the request number (1-based) and the offset about to be sent; it
// can sleep to trickle or stall. Range requests are honoured.
func audioServer(t *testing.T, payload []byte, pace func(req int, off int) bool) (*httptest.Server, *atomic.Int32) {
	var reqs atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(reqs.Add(1))
		start := 0
		if rg := r.Header.Get("Range"); rg != "" {
			start, _ = strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rg, "bytes="), "-"))
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(payload)-1, len(payload)))
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)-start))
			w.WriteHeader(http.StatusPartialContent)
		} else {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		}
		const chunk = 4 << 10
		for off := start; off < len(payload); off += chunk {
			if !pace(n, off) {
				<-r.Context().Done() // stall until the client gives up
				return
			}
			w.Write(payload[off:min(off+chunk, len(payload))])
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs
}

func testPayload() []byte {
	p := make([]byte, 64<<10)
	for i := range p {
		p[i] = byte(i * 7)
	}
	return p
}

func TestDownloadWithProgress(t *testing.T) {
	payload := testPayload()
	cases := []struct {
		name     string
		pace     func(req, off int) bool
		cancelIn time.Duration // cancel the context after this long; 0 = never
		client   *http.Client  // overrides newDownloadClient
		leftover []byte        // already at dest when the download starts
		wantErr  error
		wantReqs int32
	}{
		{
			// A kill -9 leaves the temp file behind, and the name is not unique
			// to a track: resuming from it splices another file's bytes in.
			name:     "a leftover file is not resumed",
			pace:     func(_, _ int) bool { return true },
			leftover: bytes.Repeat([]byte("X"), 4<<10),
			wantReqs: 1,
		},
		{
			// The reported bug: a download that takes longer than any fixed
			// limit, but never stops moving, must finish in one request.
			name:     "slow but steady outlasts the stall timeout",
			pace:     func(_, _ int) bool { time.Sleep(40 * time.Millisecond); return true }, // 16 chunks ≈ 640 ms
			wantReqs: 1,
		},
		{
			name:     "stall mid-body resumes by Range",
			pace:     func(req, off int) bool { return req > 1 || off < len(payload)/2 },
			wantReqs: 2,
		},
		{
			// A 200 sends its headers with the first chunk, so holding that
			// chunk back holds the headers back.
			name: "slow headers count as a stall",
			pace: func(req, off int) bool {
				if req == 1 && off == 0 {
					time.Sleep(3 * stallTimeout)
				}
				return true
			},
			wantReqs: 2,
		},
		{
			// net/http's own timeout errors match context.DeadlineExceeded;
			// only the caller's context may mean "stop", or a timeout drops
			// the retry and deletes the partial file.
			name:     "a client timeout is retried, not taken for a cancel",
			pace:     func(req, off int) bool { return req > 1 || off < len(payload)/2 },
			client:   &http.Client{Timeout: 300 * time.Millisecond},
			wantReqs: 2,
		},
		{
			// "failed after 5 attempts" used to be the whole message; the
			// cause of the last attempt has to come with it.
			name:     "giving up keeps the cause",
			pace:     func(_, off int) bool { return off < len(payload)/2 },
			wantErr:  os.ErrDeadlineExceeded,
			wantReqs: maxDownloadRetries,
		},
		{
			name:     "cancel during a stall stops without retrying",
			pace:     func(_, off int) bool { return off < len(payload)/2 },
			cancelIn: 100 * time.Millisecond,
			wantErr:  context.Canceled,
			wantReqs: 1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fastStall(t)
			srv, reqs := audioServer(t, payload, c.pace)

			// The deadline turns a hang — a stall nobody detects — into a
			// failure instead of a test timeout.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if c.cancelIn > 0 {
				time.AfterFunc(c.cancelIn, cancel)
			}
			d := &Downloader{httpClient: newDownloadClient()}
			if c.client != nil {
				d.httpClient = c.client
			}
			dest := filepath.Join(t.TempDir(), ".01.tmp")
			if c.leftover != nil {
				if err := os.WriteFile(dest, c.leftover, 0644); err != nil {
					t.Fatal(err)
				}
			}

			err := d.downloadWithProgress(ctx, srv.URL+"/a.flac", dest, nil)
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("err = %v, want %v", err, c.wantErr)
			}
			// The real URL is signed and carries the user id.
			if err != nil && strings.Contains(err.Error(), srv.URL) {
				t.Errorf("err names the URL: %v", err)
			}
			if got := reqs.Load(); got != c.wantReqs {
				t.Errorf("server saw %d requests, want %d", got, c.wantReqs)
			}
			if c.wantErr != nil {
				return
			}
			got, _ := os.ReadFile(dest)
			if !bytes.Equal(got, payload) {
				t.Errorf("file differs from payload: %d bytes, want %d", len(got), len(payload))
			}
		})
	}
}

// A cover that exists counts as downloaded on every later run and gets embedded
// with --embed-art, so only a complete image may end up under its name.
func TestDownloadExtra(t *testing.T) {
	image := []byte("real image")
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    []byte // content of dest; nil = must not exist
	}{
		{
			name:    "saves the file",
			handler: func(w http.ResponseWriter, r *http.Request) { w.Write(image) },
			want:    image,
		},
		{
			name: "an error page is not the file",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "<html>Not Found</html>", http.StatusNotFound)
			},
		},
		{
			name: "a cut transfer leaves nothing",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", strconv.Itoa(len(image)*2))
				w.Write(image) // short of Content-Length: net/http drops the conn
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(c.handler)
			defer srv.Close()
			dir := t.TempDir()
			dest := filepath.Join(dir, coverFile)

			(&Downloader{httpClient: newDownloadClient()}).downloadExtra(context.Background(), srv.URL, dest)

			got, err := os.ReadFile(dest)
			if c.want == nil {
				if err == nil {
					t.Errorf("%s exists with %q", coverFile, got)
				}
			} else if !bytes.Equal(got, c.want) {
				t.Errorf("content = %q (%v), want %q", got, err, c.want)
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) > 1 || (len(entries) == 1 && c.want == nil) {
				t.Errorf("leftovers in the directory: %v", entries)
			}
		})
	}
}

// downloadExtra cleans up after a failed copy, but a process that dies mid-copy
// runs no cleanup: what is on disk at that moment must not carry the real name.
func TestDownloadExtraHidesThePartialFile(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, coverFile)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("half"))
		w.(http.Flusher).Flush()
		for i := 0; i < 200; i++ { // until the client has created its file
			if entries, _ := os.ReadDir(dir); len(entries) > 0 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if _, err := os.Stat(dest); err == nil {
			t.Errorf("%s exists while the transfer is running", coverFile)
		}
		w.Write([]byte(" and the rest"))
	}))
	defer srv.Close()

	(&Downloader{httpClient: newDownloadClient()}).downloadExtra(context.Background(), srv.URL, dest)

	if got, _ := os.ReadFile(dest); string(got) != "half and the rest" {
		t.Errorf("content = %q", got)
	}
}
