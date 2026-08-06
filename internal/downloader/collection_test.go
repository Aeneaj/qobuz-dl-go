package downloader

import "testing"

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
