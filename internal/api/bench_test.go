package api

// Cost of one album/get: HTTP over loopback plus decoding into
// map[string]interface{}.
//
//	go test ./internal/api -run '^$' -bench . -benchmem -count 10

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// syntheticAlbumJSON is an album/get response with the fields and string
// sizes of a real one: the API now requires a logged-in user even for public
// metadata, so the payload is synthetic, shaped after Qobuz's album object.
func syntheticAlbumJSON(tracks int) []byte {
	performers := strings.Repeat("Ólafur Arnalds, Composer, MainArtist - Arnór Dan, Vocals - ", 6)
	items := make([]interface{}, tracks)
	for i := range items {
		items[i] = map[string]interface{}{
			"id": 98439707 + i, "title": fmt.Sprintf("Track %d", i+1), "version": nil,
			"track_number": i + 1, "media_number": 1, "duration": 241, "isrc": "GBUM71104935",
			"maximum_bit_depth": 24, "maximum_sampling_rate": 96, "maximum_channel_count": 2,
			"performer":   map[string]interface{}{"id": 58291, "name": "Ólafur Arnalds"},
			"composer":    map[string]interface{}{"id": 58291, "name": "Ólafur Arnalds"},
			"performers":  performers,
			"copyright":   "(P) 2011 Mercury KX, a division of Decca Music Group Limited",
			"audio_info":  map[string]interface{}{"replaygain_track_gain": -7.71, "replaygain_track_peak": 0.988525},
			"purchasable": true, "streamable": true, "previewable": true, "sampleable": true,
			"downloadable": true, "displayable": true, "hires": true, "hires_streamable": true,
			"purchasable_at": 1318888800, "streamable_at": 1318888800, "parental_warning": false,
			"release_date_original": "2011-10-21", "release_date_download": "2011-10-21", "release_date_stream": "2011-10-21",
		}
	}
	album := map[string]interface{}{
		"id": "0602527814349", "qobuz_id": 1442378, "title": "Living Room Songs", "version": nil,
		"artist":  map[string]interface{}{"id": 58291, "name": "Ólafur Arnalds", "slug": "olafur-arnalds", "albums_count": 187, "picture": nil, "image": nil},
		"artists": []interface{}{map[string]interface{}{"id": 58291, "name": "Ólafur Arnalds", "roles": []interface{}{"main-artist"}}},
		"image": map[string]interface{}{
			"small": "https://static.qobuz.com/images/covers/49/43/0602527814349_230.jpg", "thumbnail": "https://static.qobuz.com/images/covers/49/43/0602527814349_50.jpg",
			"large": "https://static.qobuz.com/images/covers/49/43/0602527814349_600.jpg", "back": nil,
		},
		"label":       map[string]interface{}{"id": 1065, "name": "Mercury KX", "slug": "mercury-kx", "albums_count": 3012, "supplier_id": 1},
		"genre":       map[string]interface{}{"id": 10, "name": "Classique", "slug": "classique", "color": "#8b8b8b", "path": []interface{}{10}},
		"genres_list": []interface{}{"Classique", "Classique→Musique de chambre"},
		"description": strings.Repeat("<p>Recorded over seven days in his Reykjavík living room, one song a day. </p>", 30),
		"copyright":   "(P) 2011 Mercury KX, a division of Decca Music Group Limited",
		"upc":         "0602527814349", "tracks_count": tracks, "media_count": 1, "duration": 241 * tracks,
		"maximum_bit_depth": 24, "maximum_sampling_rate": 96, "maximum_channel_count": 2,
		"release_date_original": "2011-10-21", "release_date_download": "2011-10-21", "release_date_stream": "2011-10-21",
		"released_at": 1319148000, "streamable": true, "purchasable": true, "hires": true, "hires_streamable": true,
		"awards": []interface{}{}, "articles": []interface{}{}, "goodies": []interface{}{}, "release_type": "album",
		"product_type": "album", "relative_url": "/album/living-room-songs-olafur-arnalds/0602527814349",
		"tracks": map[string]interface{}{"offset": 0, "limit": 50, "total": tracks, "items": items},
	}
	b, _ := json.Marshal(album)
	return b
}

// BenchmarkGetAlbumMeta is the whole call: request, loopback, decode.
func BenchmarkGetAlbumMeta(b *testing.B) {
	for _, n := range []int{12, 100} {
		body := syntheticAlbumJSON(n)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write(body)
		}))
		b.Cleanup(srv.Close)
		c := clientForServer(b, srv)
		b.Run(fmt.Sprintf("tracks=%d", n), func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := c.GetAlbumMeta(context.Background(), "x"); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkDecodeAlbum isolates the decode into map[string]interface{}.
func BenchmarkDecodeAlbum(b *testing.B) {
	for _, n := range []int{12, 100} {
		body := syntheticAlbumJSON(n)
		b.Run(fmt.Sprintf("tracks=%d", n), func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var m map[string]interface{}
				if err := json.Unmarshal(body, &m); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
