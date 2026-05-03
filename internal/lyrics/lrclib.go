package lyrics

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const lrclibBase = "https://lrclib.net/api/get"

type lrclibResponse struct {
	SyncedLyrics string `json:"syncedLyrics"`
	PlainLyrics  string `json:"plainLyrics"`
}

// Client is an HTTP client for the LRCLIB public API.
type Client struct {
	http       *http.Client
	baseURL    string
	retryDelay time.Duration // backoff after HTTP 429
	StepDelay  time.Duration // pause between consecutive requests
}

// NewClient returns a ready-to-use LRCLIB client with production defaults.
func NewClient() *Client {
	return &Client{
		http:       &http.Client{Timeout: 15 * time.Second},
		baseURL:    lrclibBase,
		retryDelay: 10 * time.Second,
		StepDelay:  500 * time.Millisecond,
	}
}

// Fetch retrieves lyrics for info from LRCLIB.
// Returns ("", nil) when the track is not found (HTTP 404).
// Synced lyrics (with timestamps) are preferred over plain lyrics.
func (c *Client) Fetch(info AudioInfo) (string, error) {
	q := url.Values{}
	q.Set("track_name", info.Title)
	q.Set("artist_name", info.Artist)
	if info.Album != "" {
		q.Set("album_name", info.Album)
	}
	if info.Duration > 0 {
		q.Set("duration", fmt.Sprintf("%d", info.Duration))
	}

	req, err := http.NewRequest(http.MethodGet, c.baseURL+"?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "qobuz-dl-go (github.com/Aeneaj/qobuz-dl-go)")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("lrclib: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// handled below
	case http.StatusNotFound:
		return "", nil
	case http.StatusTooManyRequests:
		return "", fmt.Errorf("rate limited (HTTP 429)")
	default:
		return "", fmt.Errorf("lrclib HTTP %d", resp.StatusCode)
	}

	var result lrclibResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("lrclib decode: %w", err)
	}
	if result.SyncedLyrics != "" {
		return result.SyncedLyrics, nil
	}
	return result.PlainLyrics, nil
}

// FetchWithRetry wraps Fetch with a single retry after retryDelay when
// LRCLIB returns HTTP 429 (rate limited).
func (c *Client) FetchWithRetry(info AudioInfo) (string, error) {
	content, err := c.Fetch(info)
	if err != nil && strings.Contains(err.Error(), "429") {
		time.Sleep(c.retryDelay)
		return c.Fetch(info)
	}
	return content, err
}
