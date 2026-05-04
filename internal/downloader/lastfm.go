package downloader

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// xspfPlaylist is the root element returned by the Last.fm 1.0 XSPF endpoint.
type xspfPlaylist struct {
	XMLName   xml.Name    `xml:"playlist"`
	Title     string      `xml:"title"`
	TrackList []xspfTrack `xml:"trackList>track"`
}

type xspfTrack struct {
	Title   string `xml:"title"`
	Creator string `xml:"creator"`
}

// LastFMTrack is an (Artist, Title) pair from a Last.fm playlist.
type LastFMTrack struct {
	Artist string
	Title  string
}

// parseLastFMURL returns (username, listType, nil) for recognised Last.fm user
// playlist URLs, or a non-nil error if the URL is not a supported Last.fm URL.
//
// Recognised path formats:
//
//	https://www.last.fm/user/{user}/loved    — loved tracks
//	https://www.last.fm/user/{user}/library  — recent tracks
func parseLastFMURL(rawURL string) (username, listType string, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", err
	}
	host := strings.ToLower(u.Host)
	if host != "www.last.fm" && host != "last.fm" {
		return "", "", fmt.Errorf("not a last.fm URL")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	// Require /user/{username}/{type}
	if len(parts) < 3 || parts[0] != "user" {
		return "", "", fmt.Errorf("unsupported last.fm URL %q — expected /user/<name>/loved or /user/<name>/library", rawURL)
	}
	return parts[1], parts[2], nil
}

// fetchLastFMTracks downloads and parses the XSPF playlist from the Last.fm
// 1.0 API. No API key is required.
//
// Supported listType values: "loved", "library".
func fetchLastFMTracks(ctx context.Context, username, listType string) ([]LastFMTrack, error) {
	base := "https://ws.audioscrobbler.com/1.0/user/" + url.PathEscape(username) + "/"
	var xspfURL string
	switch listType {
	case "loved":
		xspfURL = base + "lovedtracks.xspf"
	case "library":
		xspfURL = base + "recenttracks.xspf"
	default:
		return nil, fmt.Errorf("unsupported last.fm list type %q — use 'loved' or 'library'", listType)
	}

	hc := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, xspfURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "qobuz-dl-go/1.0")

	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch last.fm tracks: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case 200:
		// ok
	case 404:
		return nil, fmt.Errorf("last.fm user %q not found", username)
	default:
		return nil, fmt.Errorf("last.fm returned HTTP %d", resp.StatusCode)
	}

	var playlist xspfPlaylist
	if err := xml.NewDecoder(resp.Body).Decode(&playlist); err != nil {
		return nil, fmt.Errorf("decode last.fm XSPF: %w", err)
	}

	tracks := make([]LastFMTrack, 0, len(playlist.TrackList))
	for _, t := range playlist.TrackList {
		if t.Creator != "" && t.Title != "" {
			tracks = append(tracks, LastFMTrack{Artist: t.Creator, Title: t.Title})
		}
	}
	return tracks, nil
}

// downloadLastFMPlaylist fetches a Last.fm user playlist (loved or recent
// tracks), searches each track on Qobuz, and downloads the top result.
func (d *Downloader) downloadLastFMPlaylist(username, listType string) error {
	label := listType
	switch listType {
	case "loved":
		label = "loved tracks"
	case "library":
		label = "recent tracks"
	}

	fmt.Printf("\033[33mFetching Last.fm %s for user %q...\033[0m\n", label, username)
	tracks, err := fetchLastFMTracks(d.ctx, username, listType)
	if err != nil {
		return err
	}
	if len(tracks) == 0 {
		fmt.Println("\033[33mNo tracks found in Last.fm playlist.\033[0m")
		return nil
	}
	fmt.Printf("\033[33mFound %d tracks — searching Qobuz...\033[0m\n\n", len(tracks))

	dirName := sanitize(fmt.Sprintf("Last.fm - %s - %s", username, label))
	dir := filepath.Join(d.Opts.Directory, dirName)
	os.MkdirAll(dir, 0755)

	found, skipped := 0, 0
	for i, t := range tracks {
		fmt.Printf("\033[33m[%d/%d] %s — %s\033[0m\n", i+1, len(tracks), t.Artist, t.Title)
		trackID, err := d.searchFirstTrackID(t.Artist + " " + t.Title)
		if err != nil || trackID == "" {
			fmt.Printf("\033[31m  ✗ No Qobuz match found\033[0m\n")
			skipped++
			continue
		}
		if err := d.downloadTrackByID(trackID, dir); err != nil {
			fmt.Printf("\033[31m  ✗ Download error: %v\033[0m\n", err)
			skipped++
		} else {
			found++
		}
	}

	if !d.Opts.NoM3U {
		makeM3U(dir)
	}
	fmt.Printf("\n\033[32m✓  Last.fm playlist complete: %d downloaded, %d skipped\033[0m\n", found, skipped)
	return nil
}

// searchFirstTrackID searches Qobuz for the given query (typically "artist title")
// and returns the track ID of the top result, or "" if nothing was found.
func (d *Downloader) searchFirstTrackID(query string) (string, error) {
	raw, err := d.Client.SearchTracks(query, 1)
	if err != nil {
		return "", err
	}
	section, _ := raw["tracks"].(map[string]interface{})
	items, _ := section["items"].([]interface{})
	if len(items) == 0 {
		return "", nil
	}
	first, _ := items[0].(map[string]interface{})
	return idStr(first["id"]), nil
}
