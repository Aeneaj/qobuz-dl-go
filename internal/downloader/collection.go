package downloader

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

func (d *Downloader) downloadArtist(ctx context.Context, pages []map[string]interface{}) error {
	if len(pages) == 0 {
		return nil
	}
	name, _ := pages[0]["name"].(string)

	var items []map[string]interface{}
	for _, page := range pages {
		section, _ := page["albums"].(map[string]interface{})
		if section == nil {
			continue
		}
		raw, _ := section["items"].([]interface{})
		for _, r := range raw {
			if m, ok := r.(map[string]interface{}); ok {
				items = append(items, m)
			}
		}
	}

	if d.Opts.SmartDiscog {
		items = smartDiscogFilter(name, items)
	}

	dir := filepath.Join(d.Opts.Directory, sanitize(name))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create artist directory %q: %w", dir, err)
	}
	fmt.Fprintf(d.termOut(), "\033[33mDownloading discography: %s (%d albums)\033[0m\n", name, len(items))

	for _, item := range items {
		id := idStr(item["id"])
		if err := d.downloadAlbum(ctx, id, dir); err != nil {
			fmt.Fprintf(d.termOut(), "\033[31mError on album %s: %v. Skipping...\033[0m\n", id, err)
		}
	}
	return nil
}

func (d *Downloader) downloadPlaylist(ctx context.Context, pages []map[string]interface{}) error {
	if len(pages) == 0 {
		return nil
	}
	name, _ := pages[0]["name"].(string)
	dir := filepath.Join(d.Opts.Directory, sanitize(name))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create playlist directory %q: %w", dir, err)
	}

	var items []map[string]interface{}
	for _, page := range pages {
		section, _ := page["tracks"].(map[string]interface{})
		if section == nil {
			continue
		}
		raw, _ := section["items"].([]interface{})
		for _, r := range raw {
			if m, ok := r.(map[string]interface{}); ok {
				items = append(items, m)
			}
		}
	}

	fmt.Fprintf(d.termOut(), "\033[33mDownloading playlist: %s (%d tracks)\033[0m\n", name, len(items))
	for _, item := range items {
		id := idStr(item["id"])
		if err := d.downloadTrackByID(ctx, id, dir); err != nil {
			fmt.Fprintf(d.termOut(), "\033[31mError on track %s: %v. Skipping...\033[0m\n", id, err)
		}
	}

	if !d.Opts.NoM3U {
		makeM3U(d.termOut(), dir)
	}
	return nil
}

func (d *Downloader) downloadLabelOrArtist(ctx context.Context, pages []map[string]interface{}, itemKey, collectionType string) error {
	if len(pages) == 0 {
		return nil
	}
	name, _ := pages[0]["name"].(string)
	dir := filepath.Join(d.Opts.Directory, sanitize(name))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create %s directory %q: %w", collectionType, dir, err)
	}

	var items []map[string]interface{}
	for _, page := range pages {
		section, _ := page[itemKey].(map[string]interface{})
		if section == nil {
			continue
		}
		raw, _ := section["items"].([]interface{})
		for _, r := range raw {
			if m, ok := r.(map[string]interface{}); ok {
				items = append(items, m)
			}
		}
	}

	fmt.Fprintf(d.termOut(), "\033[33mDownloading %s: %s (%d albums)\033[0m\n", collectionType, name, len(items))
	for _, item := range items {
		id := idStr(item["id"])
		if err := d.downloadAlbum(ctx, id, dir); err != nil {
			fmt.Fprintf(d.termOut(), "\033[31mError on album %s: %v. Skipping...\033[0m\n", id, err)
		}
	}
	return nil
}

var (
	reRemaster = regexp.MustCompile(`(?i)(re)?master(ed)?`)
	reExtra    = regexp.MustCompile(`(?i)(anniversary|deluxe|live|collector|demo|expanded)`)
	reEssence  = regexp.MustCompile(`^([^(]+)`)
)

// smartDiscogFilter keeps one release per album title: the highest quality
// one credited to the requested artist, preferring remasters when the
// discography offers any.
func smartDiscogFilter(requestedArtist string, items []map[string]interface{}) []map[string]interface{} {
	grouped, order := groupByEssence(items)

	var result []map[string]interface{}
	for _, key := range order {
		if best, ok := pickBest(requestedArtist, grouped[key]); ok {
			result = append(result, best)
		}
	}
	return result
}

// groupByEssence buckets albums by normalised title, returning the buckets
// plus the keys in first-seen order so the output stays deterministic.
func groupByEssence(items []map[string]interface{}) (map[string][]map[string]interface{}, []string) {
	grouped := map[string][]map[string]interface{}{}
	order := []string{}
	for _, item := range items {
		title, _ := item["title"].(string)
		key := essenceTitle(title)
		if _, exists := grouped[key]; !exists {
			order = append(order, key)
		}
		grouped[key] = append(grouped[key], item)
	}
	return grouped, order
}

// groupQuality is what a group of same-title releases is judged against:
// the best audio quality on offer, and whether any of them is a remaster.
type groupQuality struct {
	bestBitDepth   float64
	bestSampleRate float64 // measured at bestBitDepth, not overall
	hasRemaster    bool
}

// pickBest returns the release representing a group, and whether any
// release qualified at all.
func pickBest(requestedArtist string, albums []map[string]interface{}) (map[string]interface{}, bool) {
	// One pass for the aggregates, caching each album's "is remaster" flag so
	// the regex runs once per album rather than again in the selection loop.
	var q groupQuality
	isRemaster := make([]bool, len(albums))
	for i, a := range albums {
		bd, _ := a["maximum_bit_depth"].(float64)
		sr, _ := a["maximum_sampling_rate"].(float64)
		switch {
		case bd > q.bestBitDepth:
			q.bestBitDepth, q.bestSampleRate = bd, sr // a higher depth resets the sampling-rate race
		case bd == q.bestBitDepth && sr > q.bestSampleRate:
			q.bestSampleRate = sr
		}
		if isAlbumType("remaster", a) {
			isRemaster[i] = true
			q.hasRemaster = true
		}
	}

	for i, a := range albums {
		if qualifies(a, q, isRemaster[i], requestedArtist) {
			return a, true
		}
	}
	return nil, false
}

// qualifies reports whether an album is the one to keep for its group.
func qualifies(a map[string]interface{}, q groupQuality, isRemaster bool, requestedArtist string) bool {
	bd, _ := a["maximum_bit_depth"].(float64)
	sr, _ := a["maximum_sampling_rate"].(float64)
	if bd != q.bestBitDepth || sr != q.bestSampleRate {
		return false
	}
	if nestedStr(a, "artist", "name") != requestedArtist {
		return false
	}
	// Once a group contains a remaster, only remasters are eligible.
	return isRemaster || !q.hasRemaster
}

func essenceTitle(title string) string {
	m := reEssence.FindString(title)
	if m == "" {
		return strings.ToLower(title)
	}
	return strings.ToLower(strings.TrimSpace(m))
}

func isAlbumType(t string, album map[string]interface{}) bool {
	title, _ := album["title"].(string)
	version, _ := album["version"].(string)
	combined := title + " " + version
	switch t {
	case "remaster":
		return reRemaster.MatchString(combined)
	case "extra":
		return reExtra.MatchString(combined)
	}
	return false
}
