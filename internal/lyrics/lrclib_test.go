package lyrics

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testClient returns a Client that points at serverURL with zero delays.
func testClient(serverURL string) *Client {
	return &Client{
		http:       &http.Client{Timeout: 5 * time.Second},
		baseURL:    serverURL,
		retryDelay: 0,
		StepDelay:  0,
	}
}

func serveLRCLIB(t *testing.T, handler http.HandlerFunc) (*Client, func()) {
	t.Helper()
	srv := httptest.NewServer(handler)
	return testClient(srv.URL), srv.Close
}

// ---- Fetch tests --------------------------------------------------------

func TestFetch_SyncedLyricsPreferred(t *testing.T) {
	client, close := serveLRCLIB(t, func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(lrclibResponse{
			SyncedLyrics: "[00:00.00] Synced line",
			PlainLyrics:  "Plain line",
		})
	})
	defer close()

	got, err := client.Fetch(context.Background(), AudioInfo{Title: "T", Artist: "A"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !strings.Contains(got, "[00:00.00]") {
		t.Errorf("synced lyrics not returned; got %q", got)
	}
}

func TestFetch_PlainFallback(t *testing.T) {
	client, close := serveLRCLIB(t, func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(lrclibResponse{
			SyncedLyrics: "",
			PlainLyrics:  "Just plain text",
		})
	})
	defer close()

	got, err := client.Fetch(context.Background(), AudioInfo{Title: "T", Artist: "A"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got != "Just plain text" {
		t.Errorf("got %q", got)
	}
}

func TestFetch_NeitherLyricsFieldSet(t *testing.T) {
	client, close := serveLRCLIB(t, func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(lrclibResponse{})
	})
	defer close()

	got, err := client.Fetch(context.Background(), AudioInfo{Title: "T", Artist: "A"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty string for response with no lyrics; got %q", got)
	}
}

func TestFetch_NotFound(t *testing.T) {
	client, close := serveLRCLIB(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	defer close()

	got, err := client.Fetch(context.Background(), AudioInfo{Title: "Unknown", Artist: "Nobody"})
	if err != nil {
		t.Fatalf("404 must not produce an error; got %v", err)
	}
	if got != "" {
		t.Errorf("expected empty string on 404; got %q", got)
	}
}

func TestFetch_RateLimited(t *testing.T) {
	client, close := serveLRCLIB(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	defer close()

	_, err := client.Fetch(context.Background(), AudioInfo{Title: "T", Artist: "A"})
	if err == nil {
		t.Fatal("expected error for HTTP 429")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("error should mention 429; got %q", err.Error())
	}
}

func TestFetch_QueryParams(t *testing.T) {
	var gotQuery map[string]string
	client, close := serveLRCLIB(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		gotQuery = map[string]string{
			"track_name":  q.Get("track_name"),
			"artist_name": q.Get("artist_name"),
			"album_name":  q.Get("album_name"),
			"duration":    q.Get("duration"),
		}
		w.WriteHeader(http.StatusNotFound)
	})
	defer close()

	client.Fetch(context.Background(), AudioInfo{
		Title:    "My Song",
		Artist:   "My Artist",
		Album:    "My Album",
		Duration: 213,
	})

	checks := map[string]string{
		"track_name":  "My Song",
		"artist_name": "My Artist",
		"album_name":  "My Album",
		"duration":    "213",
	}
	for param, want := range checks {
		if got := gotQuery[param]; got != want {
			t.Errorf("query param %q = %q, want %q", param, got, want)
		}
	}
}

func TestFetch_OmitsDurationWhenZero(t *testing.T) {
	var durationParam string
	client, close := serveLRCLIB(t, func(w http.ResponseWriter, r *http.Request) {
		durationParam = r.URL.Query().Get("duration")
		w.WriteHeader(http.StatusNotFound)
	})
	defer close()

	client.Fetch(context.Background(), AudioInfo{Title: "T", Artist: "A", Duration: 0})

	if durationParam != "" {
		t.Errorf("duration param should be absent when Duration=0; got %q", durationParam)
	}
}

// ---- FetchWithRetry tests -----------------------------------------------

func TestFetchWithRetry_RetriesOn429(t *testing.T) {
	call := 0
	client, close := serveLRCLIB(t, func(w http.ResponseWriter, _ *http.Request) {
		call++
		if call == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		json.NewEncoder(w).Encode(lrclibResponse{SyncedLyrics: "[00:00.00] OK"})
	})
	defer close()

	got, err := client.FetchWithRetry(context.Background(), AudioInfo{Title: "T", Artist: "A"})
	if err != nil {
		t.Fatalf("FetchWithRetry: %v", err)
	}
	if !strings.Contains(got, "[00:00.00]") {
		t.Errorf("expected synced lyrics after retry; got %q", got)
	}
	if call != 2 {
		t.Errorf("expected 2 HTTP calls, got %d", call)
	}
}

func TestFetchWithRetry_NoRetryOn404(t *testing.T) {
	call := 0
	client, close := serveLRCLIB(t, func(w http.ResponseWriter, _ *http.Request) {
		call++
		w.WriteHeader(http.StatusNotFound)
	})
	defer close()

	got, err := client.FetchWithRetry(context.Background(), AudioInfo{Title: "T", Artist: "A"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty string on 404; got %q", got)
	}
	if call != 1 {
		t.Errorf("404 should not retry; expected 1 call, got %d", call)
	}
}
