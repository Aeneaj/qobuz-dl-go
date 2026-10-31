package downloader

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

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

// fakeTerminal points stdin and stdout at pipes for one test, and returns the
// end to type into and the port captureOAuthRedirect announces once it is
// listening.
func fakeTerminal(t *testing.T) (stdin *os.File, stdinRead *os.File, port func() string) {
	t.Helper()
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inR, outW
	t.Cleanup(func() { os.Stdin, os.Stdout = oldIn, oldOut })

	re := regexp.MustCompile(`on port (\d+)`)
	return inW, inR, func() string {
		lines := bufio.NewScanner(outR)
		for lines.Scan() {
			if m := re.FindStringSubmatch(lines.Text()); m != nil {
				return m[1]
			}
		}
		t.Fatal("no port announced")
		return ""
	}
}

// Under `tui` the redirect lands and the screen comes back. A reader still
// blocked on stdin then takes the next keystrokes from bubbletea.
func TestCaptureOAuthRedirectLeavesStdinToTheTUI(t *testing.T) {
	stdin, stdinRead, port := fakeTerminal(t)
	d := &Downloader{}
	d.SetUI(headlessTUI())

	token := make(chan string, 1)
	go func() {
		res, _ := d.captureOAuthRedirect(context.Background(), "appid")
		token <- res.Token
	}()

	resp, err := http.Get("http://127.0.0.1:" + port() + "/?user_auth_token=tok")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := <-token; got != "tok" {
		t.Fatalf("token = %q", got)
	}

	if _, err := stdin.WriteString("x\n"); err != nil {
		t.Fatal(err)
	}
	stdinRead.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 8)
	if n, err := stdinRead.Read(buf); err != nil || string(buf[:n]) != "x\n" {
		t.Errorf("read %q, %v: something else took the keystrokes", buf[:n], err)
	}
}

// Without a TUI, Enter is still how to give up on the wait.
func TestCaptureOAuthRedirectEnterGivesUp(t *testing.T) {
	stdin, _, port := fakeTerminal(t)
	d := &Downloader{}

	errc := make(chan error, 1)
	go func() {
		_, err := d.captureOAuthRedirect(context.Background(), "appid")
		errc <- err
	}()

	port()
	if _, err := stdin.WriteString("\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errc:
		if err == nil || !strings.Contains(err.Error(), "before Enter") {
			t.Errorf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Enter did not end the wait")
	}
}
