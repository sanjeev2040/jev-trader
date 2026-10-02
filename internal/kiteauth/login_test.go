package kiteauth

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestParseRedirectURL(t *testing.T) {
	ok := map[string]string{
		"http://127.0.0.1:8765/kite/callback": "/kite/callback",
		"http://127.0.0.1:5000":               "/",
	}
	for raw, wantPath := range ok {
		u, err := ParseRedirectURL(raw)
		if err != nil {
			t.Errorf("ParseRedirectURL(%q): %v", raw, err)
			continue
		}
		if u.Path != wantPath {
			t.Errorf("ParseRedirectURL(%q).Path = %q, want %q", raw, u.Path, wantPath)
		}
	}
	for _, raw := range []string{
		"https://127.0.0.1:8765/cb",      // the listener serves plain HTTP
		"http://0.0.0.0:8765/cb",         // would listen on every interface
		"http://localhost:8765/cb",       // may resolve to ::1 or beyond loopback
		"http://example.com:8765/cb",     // not loopback
		"http://127.0.0.1/cb",            // no port
		"http://127.0.0.1:8765/cb?x=1",   // Kite appends its own query
		"http://user:pw@127.0.0.1:8765/", // credentials
		"://bad",
	} {
		if _, err := ParseRedirectURL(raw); err == nil {
			t.Errorf("ParseRedirectURL(%q) succeeded, want error", raw)
		}
	}
}

// startFlow runs a LoginFlow against fk on a loopback listener and returns
// the login URL, the callback base URL and the channel Run reports on.
func startFlow(t *testing.T, ctx context.Context, fk *fakeKite, store Store, now time.Time) (loginURL *url.URL, callbackURL string, result <-chan loginResult) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	redirect, err := ParseRedirectURL("http://" + ln.Addr().String() + "/kite/callback")
	if err != nil {
		t.Fatal(err)
	}
	flow := &LoginFlow{Client: fk.client(), Store: store, RedirectURL: redirect, Now: func() time.Time { return now }}
	ready := make(chan string, 1)
	done := make(chan loginResult, 1)
	go func() {
		sess, err := flow.Run(ctx, ln, func(u string) { ready <- u })
		done <- loginResult{session: sess, err: err}
	}()
	select {
	case raw := <-ready:
		loginURL, err = url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
	case res := <-done:
		t.Fatalf("Run returned before ready: %v", res.err)
	}
	return loginURL, redirect.String(), done
}

// kiteRedirect builds the callback URL Kite would redirect the browser to,
// passing the login URL's redirect_params back as query parameters.
func kiteRedirect(t *testing.T, callbackURL string, loginURL *url.URL, params url.Values) string {
	t.Helper()
	q, err := url.ParseQuery(loginURL.Query().Get("redirect_params"))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range params {
		q[k] = v
	}
	return callbackURL + "?" + q.Encode()
}

func get(t *testing.T, rawURL string) (int, string) {
	t.Helper()
	resp, err := http.Get(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("callback response Cache-Control = %q, want no-store", resp.Header.Get("Cache-Control"))
	}
	return resp.StatusCode, string(body)
}

func waitResult(t *testing.T, result <-chan loginResult) loginResult {
	t.Helper()
	select {
	case res := <-result:
		return res
	case <-time.After(5 * time.Second):
		t.Fatal("LoginFlow.Run did not return")
		return loginResult{}
	}
}

func successParams() url.Values {
	return url.Values{"action": {"login"}, "type": {"login"}, "status": {"success"}, "request_token": {testRequestToken}}
}

func TestLoginFlowSuccess(t *testing.T) {
	fk := newFakeKite(t)
	store := Store{Dir: t.TempDir()}
	now := time.Date(2026, 10, 2, 8, 45, 0, 0, ist)
	loginURL, callbackURL, result := startFlow(t, context.Background(), fk, store, now)

	if loginURL.Query().Get("api_key") != testAPIKey {
		t.Errorf("login URL %s has no api_key", loginURL)
	}

	code, body := get(t, kiteRedirect(t, callbackURL, loginURL, successParams()))
	if code != http.StatusOK || !strings.Contains(body, testUserID) {
		t.Errorf("callback = %d %q, want 200 naming the user", code, body)
	}
	if strings.Contains(body, testAccessToken) {
		t.Error("callback page shows the access token")
	}

	res := waitResult(t, result)
	if res.err != nil {
		t.Fatal(res.err)
	}
	if res.session.UserID != testUserID || res.session.AccessToken.Reveal() != testAccessToken {
		t.Errorf("session = %+v", res.session)
	}
	if want := time.Date(2026, 10, 3, 6, 0, 0, 0, ist); !res.session.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v", res.session.ExpiresAt, want)
	}
	saved, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if saved.AccessToken.Reveal() != testAccessToken {
		t.Errorf("saved token = %q", saved.AccessToken.Reveal())
	}

	// The listener is closed once Run returns.
	if _, err := http.Get(callbackURL); err == nil {
		t.Error("callback server still serving after Run returned")
	}
}

func TestLoginFlowIgnoresForeignRedirects(t *testing.T) {
	fk := newFakeKite(t)
	store := Store{Dir: t.TempDir()}
	loginURL, callbackURL, result := startFlow(t, context.Background(), fk, store, time.Now())

	forged := successParams()
	forged.Set(stateParam, "not-the-nonce")
	if code, _ := get(t, callbackURL+"?"+forged.Encode()); code != http.StatusBadRequest {
		t.Errorf("wrong nonce: status %d, want 400", code)
	}
	if code, _ := get(t, callbackURL+"?"+successParams().Encode()); code != http.StatusBadRequest {
		t.Errorf("missing nonce: status %d, want 400", code)
	}
	if n := fk.exchangeCount(); n != 0 {
		t.Errorf("foreign redirects reached the token exchange %d times", n)
	}
	select {
	case res := <-result:
		t.Fatalf("foreign redirect ended the flow: %v", res.err)
	default:
	}

	// The genuine redirect still completes the login.
	if code, body := get(t, kiteRedirect(t, callbackURL, loginURL, successParams())); code != http.StatusOK {
		t.Fatalf("genuine redirect = %d %q", code, body)
	}
	if res := waitResult(t, result); res.err != nil {
		t.Fatal(res.err)
	}
}

func TestLoginFlowKiteFailure(t *testing.T) {
	tests := []struct {
		name   string
		params url.Values
	}{
		{"login cancelled", url.Values{"status": {"cancelled"}}},
		{"no request token", url.Values{"status": {"success"}}},
		{"exchange rejected", url.Values{"status": {"success"}, "request_token": {"expired-token"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fk := newFakeKite(t)
			store := Store{Dir: t.TempDir()}
			loginURL, callbackURL, result := startFlow(t, context.Background(), fk, store, time.Now())
			code, _ := get(t, kiteRedirect(t, callbackURL, loginURL, tt.params))
			if code == http.StatusOK {
				t.Errorf("callback status 200, want an error")
			}
			if res := waitResult(t, result); res.err == nil {
				t.Error("Run succeeded, want error")
			}
			if _, err := store.Load(); !errors.Is(err, ErrNoSession) {
				t.Errorf("a failed login saved a session (Load err = %v)", err)
			}
		})
	}
}

func TestLoginFlowTimeout(t *testing.T) {
	fk := newFakeKite(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, _, result := startFlow(t, ctx, fk, Store{Dir: t.TempDir()}, time.Now())
	if res := waitResult(t, result); !errors.Is(res.err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", res.err)
	}
}
