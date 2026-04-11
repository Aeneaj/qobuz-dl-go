// Package api wraps the Qobuz API.
// Translated from qopy.py, originally by Sorrow446 (Qo-DL-Reborn).
package api

import (
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	baseURL   = "https://www.qobuz.com/api.json/0.2/"
	userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:83.0) Gecko/20100101 Firefox/83.0"
	resetMsg  = "Reset your credentials with 'qobuz-dl --reset'"
)

// Client is a Qobuz API client.
type Client struct {
	AppID   string
	Secrets []string
	UAT     string // user_auth_token
	UserID  string
	Label   string // subscription tier
	Secret  string // validated app secret
	http    *http.Client
}

// New creates a Client without authenticating.
func New(appID string, secrets []string) *Client {
	return &Client{
		AppID:   appID,
		Secrets: secrets,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) doGet(endpoint string, params url.Values) (map[string]interface{}, error) {
	return c.doRequest("GET", endpoint, params, "")
}

func (c *Client) doPost(endpoint, body string) (map[string]interface{}, error) {
	return c.doRequest("POST", endpoint, nil, body)
}

func (c *Client) doRequest(method, endpoint string, params url.Values, body string) (map[string]interface{}, error) {
	fullURL := baseURL + endpoint
	var reqBody io.Reader
	if body != "" {
		reqBody = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, fullURL, reqBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("X-App-Id", c.AppID)
	if body != "" {
		req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	} else {
		req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	}
	if c.UAT != "" {
		req.Header.Set("X-User-Auth-Token", c.UAT)
	}
	if params != nil {
		req.URL.RawQuery = params.Encode()
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if endpoint == "user/login" {
		switch resp.StatusCode {
		case 401:
			return nil, &AuthenticationError{"Invalid credentials. " + resetMsg}
		case 400:
			return nil, &InvalidAppIDError{"Invalid app id. " + resetMsg}
		}
	} else if (endpoint == "track/getFileUrl" || endpoint == "favorite/getUserFavorites") && resp.StatusCode == 400 {
		return nil, &InvalidAppSecretError{fmt.Sprintf("Invalid app secret: %s. %s", string(respBody), resetMsg)}
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("decode response: %w (body: %s)", err, string(respBody))
	}
	return result, nil
}

func md5hex(s string) string {
	return fmt.Sprintf("%x", md5.Sum([]byte(s)))
}

// AuthWithToken authenticates using user_id + user_auth_token obtained via OAuth.
func (c *Client) AuthWithToken(userID, userAuthToken string) error {
	params := url.Values{
		"user_id":         {userID},
		"user_auth_token": {userAuthToken},
		"app_id":          {c.AppID},
	}
	info, err := c.doGet("user/login", params)
	if err != nil {
		return err
	}
	c.UAT = userAuthToken
	c.UserID = userID
	return c.extractUserInfo(info)
}

func (c *Client) extractUserInfo(info map[string]interface{}) error {
	user, ok := info["user"].(map[string]interface{})
	if !ok {
		return &AuthenticationError{"unexpected response shape"}
	}
	credential, _ := user["credential"].(map[string]interface{})
	params, _ := credential["parameters"].(map[string]interface{})
	if params == nil {
		return &IneligibleError{"Free accounts are not eligible to download tracks."}
	}
	if c.UAT == "" {
		if uat, ok := info["user_auth_token"].(string); ok {
			c.UAT = uat
		}
	}
	if c.UserID == "" {
		if uid, ok := user["id"]; ok {
			c.UserID = fmt.Sprintf("%v", uid)
		}
	}
	if label, ok := params["short_label"].(string); ok {
		c.Label = label
	}
	fmt.Printf("\033[32mMembership: %s\033[0m\n", c.Label)
	return nil
}

// OAuthResult contains the data obtained from a successful OAuth redirect.
type OAuthResult struct {
	// Code is set when Qobuz redirects back with ?code=... or ?code_autorisation=...
	Code string
	// Token is set when Qobuz redirects back with ?user_auth_token=... directly
	Token string
	// UserID is set when Qobuz redirects back with ?user_id=... directly
	UserID string
	// AllParams contains all raw query parameters from the redirect for debugging
	AllParams url.Values
}

// LoginWithOAuthResult handles whatever Qobuz sends back in the OAuth redirect.
// Qobuz may send either:
//   - A code (code= or code_autorisation=) that must be exchanged via /oauth/callback
//   - A token (user_auth_token=) directly usable with /user/login
func (c *Client) LoginWithOAuthResult(result OAuthResult, privateKey string) (map[string]interface{}, error) {
	// Case 1: Qobuz sent a token directly in the redirect
	if result.Token != "" {
		c.UAT = result.Token
		if result.UserID != "" {
			c.UserID = result.UserID
		}
		info, err := c.doPost("user/login", "extra=partner")
		if err != nil {
			return nil, fmt.Errorf("user/login with OAuth token: %w", err)
		}
		if err := c.extractUserInfo(info); err != nil {
			return nil, err
		}
		return info, nil
	}

	// Case 2: We have a code that needs to be exchanged
	if result.Code != "" {
		return c.exchangeOAuthCode(result.Code, privateKey)
	}

	return nil, &AuthenticationError{"OAuth redirect contained neither token nor code"}
}

// exchangeOAuthCode tries to exchange a code for a token via /oauth/callback.
// Qobuz has used different parameter names and HTTP methods over time, so we
// try all combinations: (GET|POST) × ("code"|"code_autorisation").
func (c *Client) exchangeOAuthCode(code, privateKey string) (map[string]interface{}, error) {
	type attempt struct {
		method    string
		paramName string
	}
	// "code" first — Qobuz error messages say "Missing argument: code"
	attempts := []attempt{
		{"GET", "code"},
		{"POST", "code"},
		{"GET", "code_autorisation"},
		{"POST", "code_autorisation"},
	}

	var lastErr error
	for _, a := range attempts {
		params := url.Values{
			a.paramName: {code},
			"app_id":    {c.AppID},
		}
		if privateKey != "" {
			params.Set("private_key", privateKey)
		}

		var (
			resp map[string]interface{}
			err  error
		)
		if a.method == "GET" {
			resp, err = c.doGet("oauth/callback", params)
		} else {
			resp, err = c.doPost("oauth/callback", params.Encode())
		}

		if err != nil {
			lastErr = err
			continue // try next combination
		}

		// Exchange succeeded — extract token
		token, _ := resp["token"].(string)
		if token == "" {
			// Some Qobuz flows return user info directly without a separate token field
			if _, hasUser := resp["user"]; hasUser {
				if err := c.extractUserInfo(resp); err != nil {
					return nil, err
				}
				return resp, nil
			}
			lastErr = &AuthenticationError{"no token in oauth/callback response"}
			continue
		}

		c.UAT = token
		info, err := c.doPost("user/login", "extra=partner")
		if err != nil {
			return nil, fmt.Errorf("user/login after OAuth: %w", err)
		}
		if err := c.extractUserInfo(info); err != nil {
			return nil, err
		}
		return info, nil
	}

	return nil, fmt.Errorf("oauth code exchange failed (tried all GET/POST combinations): %w", lastErr)
}

// CfgSetup validates secrets and picks the first working one.
func (c *Client) CfgSetup() error {
	for _, secret := range c.Secrets {
		if secret == "" {
			continue
		}
		if c.testSecret(secret) {
			c.Secret = secret
			return nil
		}
	}
	return &InvalidAppSecretError{"Can't find any valid app secret. " + resetMsg}
}

func (c *Client) testSecret(secret string) bool {
	_, err := c.GetTrackURL("5966783", 5, secret)
	return err == nil
}

// GetAlbumMeta returns album metadata.
func (c *Client) GetAlbumMeta(id string) (map[string]interface{}, error) {
	return c.doGet("album/get", url.Values{"album_id": {id}})
}

// GetTrackMeta returns track metadata.
func (c *Client) GetTrackMeta(id string) (map[string]interface{}, error) {
	return c.doGet("track/get", url.Values{"track_id": {id}})
}

// GetTrackURL returns a signed download URL for a track.
func (c *Client) GetTrackURL(trackID string, fmtID int, secretOverride string) (map[string]interface{}, error) {
	if fmtID != 5 && fmtID != 6 && fmtID != 7 && fmtID != 27 {
		return nil, &InvalidQualityError{"choose between 5, 6, 7 or 27"}
	}
	secret := secretOverride
	if secret == "" {
		secret = c.Secret
	}
	unix := strconv.FormatInt(time.Now().Unix(), 10)
	rawSig := fmt.Sprintf("trackgetFileUrlformat_id%dintentstreamtrack_id%s%s%s",
		fmtID, trackID, unix, secret)
	sig := md5hex(rawSig)
	return c.doGet("track/getFileUrl", url.Values{
		"request_ts":  {unix},
		"request_sig": {sig},
		"track_id":    {trackID},
		"format_id":   {strconv.Itoa(fmtID)},
		"intent":      {"stream"},
	})
}

// GetFavoriteAlbums returns the user's favorite albums.
func (c *Client) GetFavoriteAlbums(offset, limit int) (map[string]interface{}, error) {
	unix := strconv.FormatInt(time.Now().Unix(), 10)
	rawSig := "favoritegetUserFavorites" + unix + c.Secret
	sig := md5hex(rawSig)
	return c.doGet("favorite/getUserFavorites", url.Values{
		"app_id":          {c.AppID},
		"user_auth_token": {c.UAT},
		"type":            {"albums"},
		"request_ts":      {unix},
		"request_sig":     {sig},
		"limit":           {strconv.Itoa(limit)},
		"offset":          {strconv.Itoa(offset)},
	})
}

// SearchAlbums searches for albums.
func (c *Client) SearchAlbums(query string, limit int) (map[string]interface{}, error) {
	return c.doGet("album/search", url.Values{"query": {query}, "limit": {strconv.Itoa(limit)}})
}

// SearchTracks searches for tracks.
func (c *Client) SearchTracks(query string, limit int) (map[string]interface{}, error) {
	return c.doGet("track/search", url.Values{"query": {query}, "limit": {strconv.Itoa(limit)}})
}

// SearchArtists searches for artists.
func (c *Client) SearchArtists(query string, limit int) (map[string]interface{}, error) {
	return c.doGet("artist/search", url.Values{"query": {query}, "limit": {strconv.Itoa(limit)}})
}

// SearchPlaylists searches for playlists.
func (c *Client) SearchPlaylists(query string, limit int) (map[string]interface{}, error) {
	return c.doGet("playlist/search", url.Values{"query": {query}, "limit": {strconv.Itoa(limit)}})
}

// GetArtistMeta returns paginated artist metadata.
func (c *Client) GetArtistMeta(id string) ([]map[string]interface{}, error) {
	return c.multiMeta("artist/get", "albums_count", url.Values{
		"app_id":    {c.AppID},
		"artist_id": {id},
		"extra":     {"albums"},
	})
}

// GetPlaylistMeta returns paginated playlist metadata.
func (c *Client) GetPlaylistMeta(id string) ([]map[string]interface{}, error) {
	return c.multiMeta("playlist/get", "tracks_count", url.Values{
		"extra":       {"tracks"},
		"playlist_id": {id},
	})
}

// GetLabelMeta returns paginated label metadata.
func (c *Client) GetLabelMeta(id string) ([]map[string]interface{}, error) {
	return c.multiMeta("label/get", "albums_count", url.Values{
		"label_id": {id},
		"extra":    {"albums"},
	})
}

func (c *Client) multiMeta(endpoint, countKey string, baseParams url.Values) ([]map[string]interface{}, error) {
	const pageSize = 500

	fetchPage := func(offset int) (map[string]interface{}, error) {
		params := make(url.Values, len(baseParams)+2)
		for k, v := range baseParams {
			params[k] = v
		}
		params.Set("limit", strconv.Itoa(pageSize))
		params.Set("offset", strconv.Itoa(offset))
		return c.doGet(endpoint, params)
	}

	// First page also gives us the total item count.
	first, err := fetchPage(0)
	if err != nil {
		return nil, err
	}
	results := []map[string]interface{}{first}

	total := 0
	if v, ok := first[countKey].(float64); ok {
		total = int(v)
	}

	for offset := pageSize; offset < total; offset += pageSize {
		page, err := fetchPage(offset)
		if err != nil {
			return results, err
		}
		results = append(results, page)
	}
	return results, nil
}
