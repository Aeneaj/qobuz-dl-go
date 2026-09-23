package downloader

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// collectPageItems flattens the items of every page under the given section
// key (e.g. "albums", "tracks"), skipping pages that lack the section.
func collectPageItems(pages []map[string]interface{}, key string) []map[string]interface{} {
	var items []map[string]interface{}
	for _, page := range pages {
		section, _ := page[key].(map[string]interface{})
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
	return items
}

// collectionFields are the item fields the collection code reads: the id for
// the download loop, and what smartDiscogFilter judges an album by (plus the
// artist's name, kept by slimPage).
var collectionFields = []string{"id", "title", "version", "maximum_bit_depth", "maximum_sampling_rate"}

// fetchPages runs a paged metadata call and keeps, of each page as it
// arrives, only its name and the items under key reduced to what the
// collection code reads. A decoded page is ~3 MB of which the collection uses
// a few fields; holding every full page peaked at 78 MB on a 10,000-album
// discography.
func fetchPages(ctx context.Context, get func(context.Context, string, func(map[string]interface{})) error, id, key string) ([]map[string]interface{}, error) {
	var pages []map[string]interface{}
	err := get(ctx, id, func(page map[string]interface{}) {
		pages = append(pages, slimPage(page, key))
	})
	return pages, err
}

// slimPage returns page reduced to its name and, under key, the items with
// only collectionFields and the artist's name.
func slimPage(page map[string]interface{}, key string) map[string]interface{} {
	full := collectPageItems([]map[string]interface{}{page}, key)
	items := make([]interface{}, 0, len(full))
	for _, item := range full {
		slim := make(map[string]interface{}, len(collectionFields)+1)
		for _, f := range collectionFields {
			if v, ok := item[f]; ok {
				slim[f] = v
			}
		}
		if name := nestedStr(item, "artist", "name"); name != "" {
			slim["artist"] = map[string]interface{}{"name": name}
		}
		items = append(items, slim)
	}
	return map[string]interface{}{"name": page["name"], key: map[string]interface{}{"items": items}}
}

// collectionIDs returns the ids of the items listed under key in pages,
// passed through filter when it is not nil. Decoded items are 6–10 KB each —
// a 10,000-album discography is 61 MB, measured against the real API — and
// the download loop over them can run for hours; returning only the ids lets
// the maps go as soon as this call returns.
func collectionIDs(pages []map[string]interface{}, key string, filter func([]map[string]interface{}) []map[string]interface{}) []string {
	items := collectPageItems(pages, key)
	if filter != nil {
		items = filter(items)
	}
	ids := make([]string, len(items))
	for i, item := range items {
		ids[i] = idStr(item["id"])
	}
	return ids
}

// downloadAlbumCollection downloads every album listed under itemKey in pages
// into a directory named after the collection. kind names the collection in
// the console output. smartDiscog applies the discography filter, which only
// makes sense when the albums all belong to the collection's own artist — for
// a label it would compare each album's artist against the label name and
// discard everything.
func (d *Downloader) downloadAlbumCollection(ctx context.Context, pages []map[string]interface{}, itemKey, kind string, smartDiscog bool) error {
	if len(pages) == 0 {
		return nil
	}
	name, _ := pages[0]["name"].(string)
	var filter func([]map[string]interface{}) []map[string]interface{}
	if smartDiscog {
		filter = func(items []map[string]interface{}) []map[string]interface{} {
			return smartDiscogFilter(name, items)
		}
	}
	ids := collectionIDs(pages, itemKey, filter)

	dir := filepath.Join(d.Opts.Directory, sanitize(name))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create %s directory %q: %w", kind, dir, err)
	}
	fmt.Fprintf(d.termOut(), "\033[33mDownloading %s: %s (%d albums)\033[0m\n", kind, name, len(ids))

	for _, id := range ids {
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

	ids := collectionIDs(pages, "tracks", nil)
	fmt.Fprintf(d.termOut(), "\033[33mDownloading playlist: %s (%d tracks)\033[0m\n", name, len(ids))
	for _, id := range ids {
		if err := d.downloadTrackByID(ctx, id, dir); err != nil {
			fmt.Fprintf(d.termOut(), "\033[31mError on track %s: %v. Skipping...\033[0m\n", id, err)
		}
	}

	if !d.Opts.NoM3U {
		makeM3U(d.termOut(), dir)
	}
	return nil
}

var (
	reRemaster = regexp.MustCompile(`(?i)(re)?master(ed)?`)
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
	remastered := make([]bool, len(albums))
	for i, a := range albums {
		bd, _ := a["maximum_bit_depth"].(float64)
		sr, _ := a["maximum_sampling_rate"].(float64)
		switch {
		case bd > q.bestBitDepth:
			q.bestBitDepth, q.bestSampleRate = bd, sr // a higher depth resets the sampling-rate race
		case bd == q.bestBitDepth && sr > q.bestSampleRate:
			q.bestSampleRate = sr
		}
		if isRemaster(a) {
			remastered[i] = true
			q.hasRemaster = true
		}
	}

	for i, a := range albums {
		if qualifies(a, q, remastered[i], requestedArtist) {
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

func isRemaster(album map[string]interface{}) bool {
	title, _ := album["title"].(string)
	version, _ := album["version"].(string)
	return reRemaster.MatchString(title + " " + version)
}
