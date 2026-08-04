package downloader

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Aeneaj/qobuz-dl-go/internal/api"
)

// oauthClient wires a Client to a fake Qobuz whose user/login returns body.
func oauthClient(t *testing.T, body string) *api.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body)) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)

	hc := &http.Client{Transport: &rewriteTransport{
		target:  srv.URL + "/api.json/0.2/",
		wrapped: http.DefaultTransport,
	}}
	return api.NewWithHTTP("appid", []string{"secret"}, hc)
}

// A free account has no credential.parameters, which is how Qobuz signals the
// tier. Anything else is a paid account.
const (
	freeAccount = `{"user":{"id":1,"credential":{"parameters":null}}}`
	paidAccount = `{"user":{"id":1,"credential":{"parameters":{"short_label":"Studio"}}}}`
)

// TestOAuthLogin_IneligibleDoesNotAdviseReset covers the one error where the
// generic "use token auth instead: qobuz-dl --reset" advice is actively wrong.
// The login succeeded; the account tier is the problem. Re-entering the same
// correct credentials cannot fix that, and telling the user to try loops them.
func TestOAuthLogin_IneligibleDoesNotAdviseReset(t *testing.T) {
	d := &Downloader{Client: oauthClient(t, freeAccount)}

	err := d.OAuthLogin(context.Background(), "appid", "", "?user_auth_token=tok")
	if err == nil {
		t.Fatal("free account: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "paid Qobuz subscription") {
		t.Errorf("error should name the real cause, got: %v", err)
	}
	if strings.Contains(err.Error(), "--reset") {
		t.Errorf("error must not send the user to --reset, got: %v", err)
	}
}

// A genuine auth failure keeps the token-auth advice — that is the case where
// --reset actually helps.
func TestOAuthLogin_OtherErrorsKeepResetAdvice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	hc := &http.Client{Transport: &rewriteTransport{
		target:  srv.URL + "/api.json/0.2/",
		wrapped: http.DefaultTransport,
	}}
	d := &Downloader{Client: api.NewWithHTTP("appid", []string{"secret"}, hc)}

	err := d.OAuthLogin(context.Background(), "appid", "", "?user_auth_token=tok")
	if err == nil {
		t.Fatal("401: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "--reset") {
		t.Errorf("a real auth failure should still advise --reset, got: %v", err)
	}
}

// A paid account authenticates cleanly — guards against the eligibility gate
// rejecting everyone.
func TestOAuthLogin_PaidAccountSucceeds(t *testing.T) {
	d := &Downloader{Client: oauthClient(t, paidAccount)}

	if err := d.OAuthLogin(context.Background(), "appid", "", "?user_auth_token=tok"); err != nil {
		t.Fatalf("paid account: %v", err)
	}
}
