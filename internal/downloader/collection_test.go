package downloader

import (
	"slices"
	"testing"
)

// collectPageItems is the flattening step shared by the artist, label and
// playlist flows. Paginated responses are the case worth pinning down: the
// Qobuz API splits collections into pages of 500, so an artist with a long
// discography arrives as several pages that must be concatenated in order.
func TestCollectPageItems(t *testing.T) {
	page := func(key string, ids ...string) map[string]interface{} {
		items := make([]interface{}, len(ids))
		for i, id := range ids {
			items[i] = map[string]interface{}{"id": id}
		}
		return map[string]interface{}{key: map[string]interface{}{"items": items}}
	}

	cases := []struct {
		name  string
		pages []map[string]interface{}
		key   string
		want  []string
	}{
		{"single page", []map[string]interface{}{page("albums", "a", "b")}, "albums", []string{"a", "b"}},
		{
			"concatenated in page order",
			[]map[string]interface{}{page("albums", "a"), page("albums", "b", "c")},
			"albums", []string{"a", "b", "c"},
		},
		{
			"page without the section is skipped",
			[]map[string]interface{}{{"name": "x"}, page("albums", "a")},
			"albums", []string{"a"},
		},
		{"wrong key yields nothing", []map[string]interface{}{page("albums", "a")}, "tracks", nil},
		{"no pages", nil, "albums", nil},
		{"section present but empty", []map[string]interface{}{page("tracks")}, "tracks", nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := collectPageItems(c.pages, c.key)
			if len(got) != len(c.want) {
				t.Fatalf("got %d items, want %d", len(got), len(c.want))
			}
			for i, want := range c.want {
				if id := idStr(got[i]["id"]); id != want {
					t.Errorf("item %d = %q, want %q", i, id, want)
				}
			}
		})
	}
}

// slimPage keeps only what the collection code reads, so smart discography
// must choose the same albums from slim pages as from full ones. Each group
// below is decided by a different kept field: remaster status that only the
// version names, the higher bit depth, the higher sampling rate, and the
// requested artist over another one. Dropping any of them from
// collectionFields changes the pick.
func TestSlimPageKeepsWhatSmartDiscogReads(t *testing.T) {
	album := func(id, title, version, artist string, bd, sr float64) map[string]interface{} {
		return map[string]interface{}{
			"id": id, "title": title, "version": version,
			"maximum_bit_depth": bd, "maximum_sampling_rate": sr,
			"artist":      map[string]interface{}{"name": artist, "image": "https://…", "slug": "x"},
			"description": "a long text the collection never reads",
			"image":       map[string]interface{}{"large": "https://…"},
		}
	}
	full := map[string]interface{}{"name": "Bach", "albums": map[string]interface{}{"items": []interface{}{
		album("v1", "Mass in B minor", "", "Bach", 16, 44.1),
		album("v2", "Mass in B minor", "2019 Remastered", "Bach", 16, 44.1),
		album("d1", "Goldberg Variations", "", "Bach", 16, 44.1),
		album("d2", "Goldberg Variations", "", "Bach", 24, 44.1),
		album("r1", "Cello Suites", "", "Bach", 24, 96),
		album("r2", "Cello Suites", "", "Bach", 24, 192),
		album("a1", "Art of Fugue", "", "Somebody Else", 24, 96),
		album("a2", "Art of Fugue", "", "Bach", 24, 96),
	}}}
	smart := func(items []map[string]interface{}) []map[string]interface{} { return smartDiscogFilter("Bach", items) }

	slim := slimPage(full, "albums")
	want := collectionIDs([]map[string]interface{}{full}, "albums", smart)
	got := collectionIDs([]map[string]interface{}{slim}, "albums", smart)
	if !slices.Equal(got, want) || !slices.Equal(want, []string{"v2", "d2", "r2", "a2"}) {
		t.Errorf("slim pick %v, full pick %v, want both [v2 d2 r2 a2]", got, want)
	}
	if slim["name"] != "Bach" {
		t.Errorf("page name = %v, want Bach", slim["name"])
	}
	for _, it := range collectPageItems([]map[string]interface{}{slim}, "albums") {
		for k := range it {
			if k != "artist" && !slices.Contains(collectionFields, k) {
				t.Errorf("slim item kept %q", k)
			}
		}
	}
}
