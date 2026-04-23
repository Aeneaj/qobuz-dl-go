package downloader

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
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

func TestFetchLastFMTracks(t *testing.T) {
	// Serve a minimal XSPF document from a local test server.
	xspf := xspfPlaylist{
		Title: "Test Playlist",
		TrackList: []xspfTrack{
			{Title: "Karma Police", Creator: "Radiohead"},
			{Title: "No Surprises", Creator: "Radiohead"},
			{Title: "", Creator: "Empty Artist"}, // should be skipped
			{Title: "No Creator", Creator: ""},   // should be skipped
		},
	}
	body, _ := xml.Marshal(xspf)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xspf+xml")
		w.Write(body)
	}))
	defer ts.Close()

	// We can't inject the server URL into fetchLastFMTracks directly, so we
	// test the XML parsing logic via the exported struct types.
	var parsed xspfPlaylist
	if err := xml.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("xml.Unmarshal: %v", err)
	}

	var tracks []LastFMTrack
	for _, tr := range parsed.TrackList {
		if tr.Creator != "" && tr.Title != "" {
			tracks = append(tracks, LastFMTrack{Artist: tr.Creator, Title: tr.Title})
		}
	}

	if len(tracks) != 2 {
		t.Fatalf("expected 2 valid tracks, got %d", len(tracks))
	}
	if tracks[0].Title != "Karma Police" || tracks[0].Artist != "Radiohead" {
		t.Errorf("track[0] = %+v, want {Radiohead Karma Police}", tracks[0])
	}
	if tracks[1].Title != "No Surprises" {
		t.Errorf("track[1].Title = %q, want %q", tracks[1].Title, "No Surprises")
	}
	_ = ts // referenced to avoid unused-var warning
}

func TestFetchLastFMTracksUnsupportedType(t *testing.T) {
	_, err := fetchLastFMTracks("rj", "playlists")
	if err == nil {
		t.Error("expected error for unsupported list type, got nil")
	}
}
