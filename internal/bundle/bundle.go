// Package bundle fetches app_id, secrets and private_key from the Qobuz web player.
// Translated from bundle.py (originally DashLt's spoofbuz).
package bundle

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const baseURL = "https://play.qobuz.com"

var (
	reSeedTimezone = regexp.MustCompile(
		`[a-z]\.initialSeed\("(?P<seed>[\w=]+)",window\.utimezone\.(?P<timezone>[a-z]+)\)`,
	)
	reAppID = regexp.MustCompile(
		`production:{api:{appId:"(?P<app_id>\d{9})",appSecret:"\w{32}"`,
	)
	// privateKey can be 6-128 chars; allow alphanum, +, /, =, -, _
	// Multiple patterns cover different bundle.js formats Qobuz has shipped.
	rePrivateKeyPatterns = []*regexp.Regexp{
		regexp.MustCompile(`privateKey:\s*"(?P<key>[A-Za-z0-9+/=_\-]{6,128})"`),
		regexp.MustCompile(`private_key:\s*"(?P<key>[A-Za-z0-9+/=_\-]{6,128})"`),
		regexp.MustCompile(`oauthKey:\s*"(?P<key>[A-Za-z0-9+/=_\-]{6,128})"`),
		regexp.MustCompile(`clientSecret:\s*"(?P<key>[A-Za-z0-9+/=_\-]{6,128})"`),
	}
	reBundleURL = regexp.MustCompile(
		`<script src="(/resources/\d+\.\d+\.\d+-[a-z]\d{3}/bundle\.js)"></script>`,
	)
)

// Bundle holds the scraped Qobuz bundle.
type Bundle struct {
	content string
}

// Fetch downloads the Qobuz login page and bundle.js.
func Fetch() (*Bundle, error) {
	client := &http.Client{Timeout: 30 * time.Second}

	resp, err := client.Get(baseURL + "/login")
	if err != nil {
		return nil, fmt.Errorf("get login page: %w", err)
	}
	defer resp.Body.Close()
	page, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	match := reBundleURL.FindSubmatch(page)
	if match == nil {
		return nil, fmt.Errorf("bundle URL not found in login page")
	}
	bundlePath := string(match[1])

	resp2, err := client.Get(baseURL + bundlePath)
	if err != nil {
		return nil, fmt.Errorf("get bundle.js: %w", err)
	}
	defer resp2.Body.Close()
	body, err := io.ReadAll(resp2.Body)
	if err != nil {
		return nil, err
	}

	return &Bundle{content: string(body)}, nil
}

// AppID extracts the application ID from bundle.js.
func (b *Bundle) AppID() (string, error) {
	m := reAppID.FindStringSubmatch(b.content)
	if m == nil {
		return "", fmt.Errorf("app_id not found in bundle")
	}
	idx := reAppID.SubexpIndex("app_id")
	return m[idx], nil
}

// PrivateKey extracts the OAuth private key, returns "" if absent.
// Tries multiple regex patterns to handle different bundle.js formats.
func (b *Bundle) PrivateKey() string {
	for _, re := range rePrivateKeyPatterns {
		m := re.FindStringSubmatch(b.content)
		if m != nil {
			return m[re.SubexpIndex("key")]
		}
	}
	return ""
}

// capitalizeFirst upper-cases the first byte of s (ASCII only — timezone names
// are always ASCII). Replaces the deprecated strings.Title.
func capitalizeFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// Secrets extracts the signing secrets from bundle.js.
func (b *Bundle) Secrets() (map[string]string, error) {
	seeds := map[string][]string{}

	// Collect seed + timezone pairs
	for _, m := range reSeedTimezone.FindAllStringSubmatch(b.content, -1) {
		seed := m[reSeedTimezone.SubexpIndex("seed")]
		tz := m[reSeedTimezone.SubexpIndex("timezone")]
		seeds[tz] = append(seeds[tz], seed)
	}
	if len(seeds) == 0 {
		return nil, fmt.Errorf("no seeds found in bundle")
	}

	// Build ordered timezone list (replicate Python OrderedDict + move_to_end logic)
	tzList := make([]string, 0, len(seeds))
	for tz := range seeds {
		tzList = append(tzList, tz)
	}
	if len(tzList) >= 2 {
		// move second element to front
		tzList[0], tzList[1] = tzList[1], tzList[0]
	}

	// Build regex for info/extras
	capitalised := make([]string, len(tzList))
	for i, tz := range tzList {
		capitalised[i] = capitalizeFirst(tz)
	}
	reInfoExtras := regexp.MustCompile(
		`name:"\w+/(?P<timezone>` + strings.Join(capitalised, "|") + `)",info:"(?P<info>[\w=]+)",extras:"(?P<extras>[\w=]+)"`,
	)

	for _, m := range reInfoExtras.FindAllStringSubmatch(b.content, -1) {
		tz := strings.ToLower(m[reInfoExtras.SubexpIndex("timezone")])
		info := m[reInfoExtras.SubexpIndex("info")]
		extras := m[reInfoExtras.SubexpIndex("extras")]
		seeds[tz] = append(seeds[tz], info, extras)
	}

	secrets := map[string]string{}
	for tz, parts := range seeds {
		joined := strings.Join(parts, "")
		if len(joined) <= 44 {
			continue
		}
		trimmed := joined[:len(joined)-44]
		// Pad to multiple of 4 to satisfy StdEncoding (Python's b64decode does this automatically)
		padded := trimmed + strings.Repeat("=", (4-len(trimmed)%4)%4)
		decoded, err := base64.StdEncoding.DecodeString(padded)
		if err != nil {
			continue
		}
		secrets[tz] = string(decoded)
	}
	return secrets, nil
}
