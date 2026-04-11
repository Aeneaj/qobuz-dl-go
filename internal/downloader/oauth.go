package downloader

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Aeneaj/qobuz-dl-go/internal/api"
)

// OAuthLogin runs the full OAuth flow:
//  1. If codeOrURL is empty → opens a local HTTP server, prints the OAuth URL,
//     waits for the redirect, captures ALL params Qobuz sends back.
//  2. Exchanges the captured result for a user_auth_token and authenticates.
//  3. The token is saved in c.Client.UAT / c.Client.UserID for the caller to persist.
func (d *Downloader) OAuthLogin(appID, privateKey string, codeOrURL string) error {
	var result api.OAuthResult

	if codeOrURL != "" {
		result = parseRedirectURL(codeOrURL)
	} else {
		var err error
		result, err = d.captureOAuthRedirect(appID)
		if err != nil {
			return err
		}
	}

	if result.Token == "" && result.Code == "" {
		return fmt.Errorf(
			"OAuth redirect did not contain a usable token or code.\n" +
				"Received params: %v\n\n" +
				"As a workaround, log in at https://play.qobuz.com, then open\n" +
				"DevTools → Application → Local Storage → find 'localuser' and run:\n" +
				"  qobuz-dl --reset --token",
			result.AllParams,
		)
	}

	if _, err := d.Client.LoginWithOAuthResult(result, privateKey); err != nil {
		return fmt.Errorf("OAuth login: %w\n\nIf this keeps failing, use token auth instead:\n  qobuz-dl --reset --token", err)
	}

	fmt.Println("\033[32mOAuth login successful!\033[0m")
	return nil
}

// captureOAuthRedirect starts a local HTTP server, shows the Qobuz OAuth URL,
// waits for the browser redirect, and captures ALL query parameters.
func (d *Downloader) captureOAuthRedirect(appID string) (api.OAuthResult, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return api.OAuthResult{}, fmt.Errorf("could not open local port: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	oauthURL := fmt.Sprintf(
		"https://www.qobuz.com/signin/oauth?ext_app_id=%s&redirect_url=http://localhost:%d",
		appID, port,
	)
	fmt.Printf("\033[33mOpen this URL in your browser to authenticate with Qobuz:\033[0m\n")
	fmt.Printf("\033[36m%s\033[0m\n\n", oauthURL)
	fmt.Printf("\033[33mA local server on port %d will capture the OAuth redirect automatically.\033[0m\n", port)
	fmt.Printf("\033[33mPress Enter after completing login in your browser (or wait for auto-capture)...\033[0m\n")

	resultCh := make(chan api.OAuthResult, 1)

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			params := r.URL.Query()
			result := parseQueryParams(params)

			if result.Token != "" || result.Code != "" {
				w.Header().Set("Content-Type", "text/html")
				fmt.Fprint(w, successPage)
				select {
				case resultCh <- result:
				default:
				}
			} else {
				// Show all received params to help diagnose
				w.Header().Set("Content-Type", "text/html")
				fmt.Fprintf(w, failurePage, params.Encode())
				select {
				case resultCh <- api.OAuthResult{AllParams: params}:
				default:
				}
			}
		}),
	}

	go func() { srv.Serve(listener) }() //nolint:errcheck

	// Wait for Enter OR for auto-capture (whichever comes first)
	enterCh := make(chan struct{}, 1)
	go func() {
		var s string
		fmt.Scanln(&s)
		enterCh <- struct{}{}
	}()

	select {
	case result := <-resultCh:
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx) //nolint:errcheck
		return result, nil
	case <-enterCh:
		// User pressed Enter before redirect arrived
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx) //nolint:errcheck
		select {
		case result := <-resultCh:
			return result, nil
		default:
			return api.OAuthResult{}, fmt.Errorf("no OAuth redirect received before Enter was pressed")
		}
	case <-time.After(5 * time.Minute):
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx) //nolint:errcheck
		return api.OAuthResult{}, fmt.Errorf("timed out waiting for OAuth redirect")
	}
}

// parseRedirectURL parses all params from a full redirect URL or a bare code/token string.
func parseRedirectURL(s string) api.OAuthResult {
	// Try to parse as a URL with query params
	parsed, err := url.Parse(s)
	if err == nil && parsed.RawQuery != "" {
		return parseQueryParams(parsed.Query())
	}
	// Maybe it's just the raw code/token
	if strings.HasPrefix(s, "http") {
		return api.OAuthResult{}
	}
	// Treat as bare code
	return api.OAuthResult{Code: s}
}

// parseQueryParams extracts token/code/user_id from Qobuz redirect query params.
// Qobuz may use different parameter names depending on the auth flow version.
func parseQueryParams(params url.Values) api.OAuthResult {
	result := api.OAuthResult{AllParams: params}

	// Direct token in redirect (most useful path)
	if t := params.Get("user_auth_token"); t != "" {
		result.Token = t
	}
	if t := params.Get("token"); t != "" && result.Token == "" {
		result.Token = t
	}
	if uid := params.Get("user_id"); uid != "" {
		result.UserID = uid
	}

	// Code that needs exchange via /oauth/callback
	if c := params.Get("code_autorisation"); c != "" { // French spelling — Qobuz's actual param
		result.Code = c
	}
	if c := params.Get("code"); c != "" && result.Code == "" {
		result.Code = c
	}

	return result
}

const successPage = `<html><body style="font-family:system-ui;text-align:center;padding:60px">
<h2>✓ Login successful</h2>
<p>You can close this tab and return to your terminal.</p>
</body></html>`

const failurePage = `<html><body style="font-family:system-ui;text-align:center;padding:60px">
<h2>⚠ Unexpected redirect params</h2>
<pre style="text-align:left;display:inline-block">%s</pre>
<p>Check your terminal for further instructions.</p>
</body></html>`
