package downloader

import (
	"context"
	"fmt"
	"regexp"
	"strconv"

	"github.com/Aeneaj/qobuz-dl-go/internal/api"
)

// SearchResults holds items for CLI display.
type SearchResult struct {
	Text string
	URL  string
}

// Search performs a typed search and returns display items.
func Search(ctx context.Context, client *api.Client, itemType, query string, limit int) ([]SearchResult, error) {
	var rawResults map[string]interface{}
	var err error
	var itemsKey, format string
	requiresExtra := false

	switch itemType {
	case "album":
		rawResults, err = client.SearchAlbums(ctx, query, limit)
		itemsKey = "albums"
		format = "{artist[name]} - {title}"
		requiresExtra = true
	case "track":
		rawResults, err = client.SearchTracks(ctx, query, limit)
		itemsKey = "tracks"
		format = "{performer[name]} - {title}"
		requiresExtra = true
	case "artist":
		rawResults, err = client.SearchArtists(ctx, query, limit)
		itemsKey = "artists"
		format = "{name} - ({albums_count} releases)"
	case "playlist":
		rawResults, err = client.SearchPlaylists(ctx, query, limit)
		itemsKey = "playlists"
		format = "{name} - ({tracks_count} releases)"
	default:
		return nil, fmt.Errorf("unknown type: %s", itemType)
	}
	if err != nil {
		return nil, err
	}

	section, _ := rawResults[itemsKey].(map[string]interface{})
	items, _ := section["items"].([]interface{})

	var results []SearchResult
	for _, item := range items {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		text := renderFormat(format, m)
		if requiresExtra {
			dur := formatDuration(int(nestedFloat(m, "duration")))
			hires := "LOSSLESS"
			if b, _ := m["hires_streamable"].(bool); b {
				hires = "HI-RES"
			}
			text = fmt.Sprintf("%s - %s [%s]", text, dur, hires)
		}
		id := idStr(m["id"])
		results = append(results, SearchResult{
			Text: text,
			URL:  fmt.Sprintf("https://play.qobuz.com/%s/%s", itemType, id),
		})
	}
	return results, nil
}

// Simple key substitution: {key} → m[key], {obj[key]} → m[obj][key].
// Package-level so it is not recompiled once per search result.
var reKey = regexp.MustCompile(`\{(\w+)(?:\[(\w+)\])?\}`)

func renderFormat(format string, m map[string]interface{}) string {
	return reKey.ReplaceAllStringFunc(format, func(match string) string {
		parts := reKey.FindStringSubmatch(match)
		if parts[2] != "" {
			sub, _ := m[parts[1]].(map[string]interface{})
			if sub == nil {
				return "n/a"
			}
			v, _ := sub[parts[2]].(string)
			return v
		}
		switch v := m[parts[1]].(type) {
		case string:
			return v
		case float64:
			return strconv.Itoa(int(v))
		default:
			return fmt.Sprintf("%v", v)
		}
	})
}

func formatDuration(secs int) string {
	h := secs / 3600
	m := (secs % 3600) / 60
	s := secs % 60
	if h > 0 {
		return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

// ---- lucky search helper used by CLI ----

func SearchURLs(ctx context.Context, client *api.Client, itemType, query string, limit int) ([]string, error) {
	results, err := Search(ctx, client, itemType, query, limit)
	if err != nil {
		return nil, err
	}
	urls := make([]string, len(results))
	for i, r := range results {
		urls[i] = r.URL
	}
	return urls, nil
}
