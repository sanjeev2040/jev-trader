package kiteauth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/sanjeev2040/jev-trader/internal/config"
)

// Fixture credentials. They are not real Kite values.
const (
	testAPIKey       = "fixture_api_key"
	testAPISecret    = "fixture_api_secret"
	testRequestToken = "fixtureRequestToken123"
	testAccessToken  = "fixture_access_token"
	testUserID       = "AB1234"
)

// fakeKite serves the two Kite session endpoints kiteauth uses.
type fakeKite struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	exchanges int
	// profileUserID is returned by /user/profile; tokenRejected makes it
	// fail with a TokenException instead.
	profileUserID string
	tokenRejected bool
}

func newFakeKite(t *testing.T) *fakeKite {
	t.Helper()
	fk := &fakeKite{t: t, profileUserID: testUserID}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /session/token", fk.sessionToken)
	mux.HandleFunc("GET /user/profile", fk.profile)
	fk.srv = httptest.NewServer(mux)
	t.Cleanup(fk.srv.Close)
	return fk
}

func (fk *fakeKite) client() *Client {
	return &Client{
		APIKey:       config.NewSecret(testAPIKey),
		APISecret:    config.NewSecret(testAPISecret),
		BaseURL:      fk.srv.URL,
		LoginBaseURL: "https://kite.example/connect/login",
		HTTPClient:   fk.srv.Client(),
	}
}

func (fk *fakeKite) exchangeCount() int {
	fk.mu.Lock()
	defer fk.mu.Unlock()
	return fk.exchanges
}

func (fk *fakeKite) sessionToken(w http.ResponseWriter, r *http.Request) {
	fk.mu.Lock()
	fk.exchanges++
	fk.mu.Unlock()
	if r.Header.Get("X-Kite-Version") != "3" {
		writeKiteError(w, http.StatusBadRequest, "InputException", "missing X-Kite-Version")
		return
	}
	if err := r.ParseForm(); err != nil {
		writeKiteError(w, http.StatusBadRequest, "InputException", err.Error())
		return
	}
	want := Checksum(testAPIKey, testRequestToken, testAPISecret)
	if r.PostForm.Get("api_key") != testAPIKey || r.PostForm.Get("request_token") != testRequestToken || r.PostForm.Get("checksum") != want {
		writeKiteError(w, http.StatusForbidden, "TokenException", "Token is invalid or has expired.")
		return
	}
	writeKiteData(w, map[string]any{
		"user_id":      testUserID,
		"user_name":    "Fixture User",
		"access_token": testAccessToken,
		"public_token": "fixture_public_token",
		"login_time":   "2026-10-02 08:45:12",
	})
}

func (fk *fakeKite) profile(w http.ResponseWriter, r *http.Request) {
	fk.mu.Lock()
	rejected, userID := fk.tokenRejected, fk.profileUserID
	fk.mu.Unlock()
	if rejected || r.Header.Get("Authorization") != "token "+testAPIKey+":"+testAccessToken {
		writeKiteError(w, http.StatusForbidden, "TokenException", "Incorrect `api_key` or `access_token`.")
		return
	}
	writeKiteData(w, map[string]any{"user_id": userID, "user_name": "Fixture User"})
}

func writeKiteData(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": data})
}

func writeKiteError(w http.ResponseWriter, code int, errorType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "error_type": errorType, "message": message})
}
