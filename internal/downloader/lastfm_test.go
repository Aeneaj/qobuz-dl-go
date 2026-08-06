package downloader

import (
	"context"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestParseLastFMURL(t *testing.T) {
	tests := []struct {
		rawURL   string
		wantUser string
		wantType string
		wantErr  bool
	}{
		{"https://www.last.fm/user/rj/loved", "rj", "loved", false},
		{"https://last.fm/user/rj/loved", "rj", "loved", false},
		{"https://www.last.fm/user/someuser/library", "someuser", "library", false},
		{"https://www.qobuz.com/album/foo/123", "", "", true}, // not last.fm
		{"https://www.last.fm/user/onlytwo", "", "", true},    // path too short
		{"https://www.last.fm/charts", "", "", true},          // no "user" prefix
	}

	for _, tc := range tests {
		user, listType, err := parseLastFMURL(tc.rawURL)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseLastFMURL(%q) expected error, got nil", tc.rawURL)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseLastFMURL(%q) unexpected error: %v", tc.rawURL, err)
			continue
		}
		if user != tc.wantUser || listType != tc.wantType {
			t.Errorf("parseLastFMURL(%q) = (%q, %q), want (%q, %q)",
				tc.rawURL, user, listType, tc.wantUser, tc.wantType)
		}
	}
}

// lastfmMock serves an XSPF playlist and records what was requested. Paths are
// recorded still escaped, so username escaping stays observable.
type lastfmMock struct {
	srv *httptest.Server

	mu    sync.Mutex
	paths []string
}

func newLastFMMock(t *testing.T, status int, body []byte) *lastfmMock {
	t.Helper()
	l := &lastfmMock{}
	l.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		l.paths = append(l.paths, r.URL.EscapedPath())
		l.mu.Unlock()

		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/xspf+xml")
		w.Write(body)
	}))
	t.Cleanup(l.srv.Close)
	return l
}

// firstPath returns the escaped path of the first request, failing the test if
// nothing was requested at all.
func (l *lastfmMock) firstPath(t *testing.T) string {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.paths) == 0 {
		t.Fatal("no request reached the Last.fm mock")
	}
	return l.paths[0]
}

func xspfBody(t *testing.T, tracks ...xspfTrack) []byte {
	t.Helper()
	body, err := xml.Marshal(xspfPlaylist{Title: "Test Playlist", TrackList: tracks})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// lastfmDownloader returns a Downloader whose Last.fm calls hit srv.
func lastfmDownloader(t *testing.T, l *lastfmMock) *Downloader {
	t.Helper()
	d := testDownloader(t, Options{NoDB: true})
	d.lastfmBase = l.srv.URL + "/user/"
	return d
}

func TestFetchLastFMTracks(t *testing.T) {
	body := xspfBody(t,
		xspfTrack{Title: "Karma Police", Creator: "Radiohead"},
		xspfTrack{Title: "No Surprises", Creator: "Radiohead"},
		xspfTrack{Title: "", Creator: "Empty Artist"}, // skipped: no title
		xspfTrack{Title: "No Creator", Creator: ""},   // skipped: no artist
	)
	l := newLastFMMock(t, http.StatusOK, body)
	d := lastfmDownloader(t, l)

	tracks, err := d.fetchLastFMTracks(context.Background(), "rj", "loved")
	if err != nil {
		t.Fatalf("fetchLastFMTracks: %v", err)
	}
	if len(tracks) != 2 {
		t.Fatalf("got %d tracks, want 2 (incomplete entries dropped): %+v", len(tracks), tracks)
	}
	if tracks[0].Artist != "Radiohead" || tracks[0].Title != "Karma Police" {
		t.Errorf("track[0] = %+v", tracks[0])
	}
	if want := "/user/rj/lovedtracks.xspf"; l.firstPath(t) != want {
		t.Errorf("requested %q, want %q", l.firstPath(t), want)
	}
}

// "library" maps to a different XSPF document than "loved".
func TestFetchLastFMTracks_LibraryUsesRecentTracks(t *testing.T) {
	l := newLastFMMock(t, http.StatusOK, xspfBody(t, xspfTrack{Title: "T", Creator: "A"}))
	d := lastfmDownloader(t, l)

	if _, err := d.fetchLastFMTracks(context.Background(), "rj", "library"); err != nil {
		t.Fatalf("fetchLastFMTracks: %v", err)
	}
	if want := "/user/rj/recenttracks.xspf"; l.firstPath(t) != want {
		t.Errorf("requested %q, want %q", l.firstPath(t), want)
	}
}

// Usernames with characters that need escaping must not corrupt the path.
func TestFetchLastFMTracks_EscapesUsername(t *testing.T) {
	l := newLastFMMock(t, http.StatusOK, xspfBody(t))
	d := lastfmDownloader(t, l)

	if _, err := d.fetchLastFMTracks(context.Background(), "a b/c", "loved"); err != nil {
		t.Fatalf("fetchLastFMTracks: %v", err)
	}
	if want := "/user/a%20b%2Fc/lovedtracks.xspf"; l.firstPath(t) != want {
		t.Errorf("requested %q, want %q", l.firstPath(t), want)
	}
}

func TestFetchLastFMTracks_Errors(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     []byte
		listType string
		wantMsg  string
	}{
		{"unknown user", http.StatusNotFound, nil, "loved", "not found"},
		{"server error", http.StatusInternalServerError, nil, "loved", "HTTP 500"},
		{"malformed xml", http.StatusOK, []byte("<playlist><trackList>"), "loved", "decode"},
		{"unsupported list", http.StatusOK, nil, "playlists", "unsupported"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := newLastFMMock(t, c.status, c.body)
			d := lastfmDownloader(t, l)

			_, err := d.fetchLastFMTracks(context.Background(), "rj", c.listType)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), c.wantMsg) {
				t.Errorf("error %q should mention %q", err, c.wantMsg)
			}
		})
	}
}

// Full flow: XSPF from Last.fm, each track looked up on Qobuz by
// "artist title", top hit downloaded into a playlist directory.
func TestDownloadLastFMPlaylist_EndToEnd(t *testing.T) {
	m := newQobuzMock(t, nil)
	m.album = testAlbum(m.srv.URL+"/cover.jpg", track(901, 5, 1, "Karma Police"))

	lfm := newLastFMMock(t, http.StatusOK,
		xspfBody(t, xspfTrack{Title: "Karma Police", Creator: "Radiohead"}))

	d, dir := m.downloaderFor(t, nil)
	d.lastfmBase = lfm.srv.URL + "/user/"

	if err := d.HandleURL(context.Background(), "https://www.last.fm/user/rj/loved"); err != nil {
		t.Fatalf("HandleURL: %v", err)
	}

	if got := m.searchQueries(); len(got) != 1 || got[0] != "Radiohead Karma Police" {
		t.Errorf("Qobuz search queries = %q, want [\"Radiohead Karma Police\"]", got)
	}

	path := filepath.Join(dir, "Last.fm - rj - loved tracks",
		"Test Artist - Test Album", "05. Karma Police.flac")
	if _, err := os.Stat(path); err != nil {
		t.Errorf("missing %s: %v", path, err)
	}
}

// A track with no Qobuz match is reported and skipped, not fatal.
func TestDownloadLastFMPlaylist_SkipsUnmatchedTracks(t *testing.T) {
	m := newQobuzMock(t, nil)
	m.album = testAlbum(m.srv.URL+"/cover.jpg", track(902, 1, 1, "Findable"))
	m.searchMisses = map[string]bool{"Nobody Unfindable": true}

	lfm := newLastFMMock(t, http.StatusOK, xspfBody(t,
		xspfTrack{Title: "Unfindable", Creator: "Nobody"},
		xspfTrack{Title: "Findable", Creator: "Somebody"},
	))

	d, dir := m.downloaderFor(t, nil)
	d.lastfmBase = lfm.srv.URL + "/user/"

	if err := d.HandleURL(context.Background(), "https://www.last.fm/user/rj/loved"); err != nil {
		t.Fatalf("HandleURL: %v", err)
	}

	path := filepath.Join(dir, "Last.fm - rj - loved tracks",
		"Test Artist - Test Album", "01. Findable.flac")
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the matched track should still be downloaded: %v", err)
	}
}
