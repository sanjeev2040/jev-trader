package kiteauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sanjeev2040/jev-trader/internal/config"
)

func TestChecksum(t *testing.T) {
	// sha256("abc") split across the three inputs.
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got := Checksum("a", "b", "c"); got != want {
		t.Errorf("Checksum = %s, want %s", got, want)
	}
}

func TestLoginURL(t *testing.T) {
	c := &Client{APIKey: config.NewSecret(testAPIKey)}
	u, err := url.Parse(c.LoginURL(url.Values{"jevtrader_state": {"n0nce"}}))
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Scheme + "://" + u.Host + u.Path; got != DefaultLoginURL {
		t.Errorf("login base = %s, want %s", got, DefaultLoginURL)
	}
	q := u.Query()
	if q.Get("v") != "3" || q.Get("api_key") != testAPIKey {
		t.Errorf("query = %v, want v=3 and api_key", q)
	}
	if got := q.Get("redirect_params"); got != "jevtrader_state=n0nce" {
		t.Errorf("redirect_params = %q", got)
	}
}

func TestExchangeRequestToken(t *testing.T) {
	fk := newFakeKite(t)
	userID, token, err := fk.client().ExchangeRequestToken(context.Background(), testRequestToken)
	if err != nil {
		t.Fatal(err)
	}
	if userID != testUserID || token.Reveal() != testAccessToken {
		t.Errorf("got (%q, %q), want (%q, %q)", userID, token.Reveal(), testUserID, testAccessToken)
	}
}

func TestExchangeRequestTokenRejected(t *testing.T) {
	fk := newFakeKite(t)
	_, _, err := fk.client().ExchangeRequestToken(context.Background(), "wrong-token")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.HTTPStatus != http.StatusForbidden {
		t.Fatalf("err = %v, want 403 APIError", err)
	}
	if !IsTokenException(err) {
		t.Errorf("IsTokenException(%v) = false", err)
	}
}

func TestExchangeRequestTokenEmpty(t *testing.T) {
	fk := newFakeKite(t)
	if _, _, err := fk.client().ExchangeRequestToken(context.Background(), ""); err == nil {
		t.Fatal("want error for empty request token")
	}
	if n := fk.exchangeCount(); n != 0 {
		t.Errorf("empty token reached Kite %d times", n)
	}
}

func TestClientNonJSONError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "<html>bad gateway</html>", http.StatusBadGateway)
	}))
	defer srv.Close()
	c := &Client{APIKey: config.NewSecret(testAPIKey), APISecret: config.NewSecret(testAPISecret), BaseURL: srv.URL}
	_, _, err := c.ExchangeRequestToken(context.Background(), testRequestToken)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.HTTPStatus != http.StatusBadGateway || apiErr.ErrorType != "" {
		t.Fatalf("err = %#v, want untyped 502 APIError", err)
	}
	if IsTokenException(err) {
		t.Error("a 502 is not a TokenException")
	}
}

func TestClientSuccessWithoutToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeKiteData(w, map[string]any{"user_id": testUserID})
	}))
	defer srv.Close()
	c := &Client{APIKey: config.NewSecret(testAPIKey), APISecret: config.NewSecret(testAPISecret), BaseURL: srv.URL}
	if _, _, err := c.ExchangeRequestToken(context.Background(), testRequestToken); err == nil {
		t.Fatal("want error when the response has no access token")
	}
}

func TestProfile(t *testing.T) {
	fk := newFakeKite(t)
	p, err := fk.client().Profile(context.Background(), config.NewSecret(testAccessToken))
	if err != nil {
		t.Fatal(err)
	}
	if p.UserID != testUserID {
		t.Errorf("UserID = %q, want %q", p.UserID, testUserID)
	}
	_, err = fk.client().Profile(context.Background(), config.NewSecret("stale"))
	if !IsTokenException(err) {
		t.Errorf("stale token: err = %v, want TokenException", err)
	}
}

func TestAPIErrorDoesNotLeakCredentials(t *testing.T) {
	fk := newFakeKite(t)
	_, err := fk.client().Profile(context.Background(), config.NewSecret("stale"))
	if err == nil {
		t.Fatal("want error")
	}
	for _, s := range []string{testAPISecret, "stale"} {
		if strings.Contains(err.Error(), s) {
			t.Errorf("error %q contains credential %q", err, s)
		}
	}
}
