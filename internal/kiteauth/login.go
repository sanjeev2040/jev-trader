package kiteauth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// DefaultRedirectURL is the redirect URL `jevtrader login` listens on unless
// told otherwise. It must match the one registered in the Kite developer
// console exactly.
const DefaultRedirectURL = "http://127.0.0.1:8765/kite/callback"

// stateParam carries a per-login nonce through Kite's redirect_params, so the
// callback accepts only the redirect that this login started.
const stateParam = "jevtrader_state"

// ParseRedirectURL checks that raw is a plain-HTTP loopback URL with an
// explicit port, the only kind the login listener serves. The host must be
// 127.0.0.1 so the callback is never reachable from another machine; on a
// remote VM, reach it through an SSH tunnel.
func ParseRedirectURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("redirect URL: %w", err)
	}
	if u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" {
		return nil, fmt.Errorf("redirect URL %q: want http://127.0.0.1:<port>/<path>", raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("redirect URL %q: must not have credentials, a query or a fragment", raw)
	}
	if u.Path == "" {
		u.Path = "/"
	}
	return u, nil
}

// LoginFlow runs one manual login: it serves the redirect URL on loopback,
// waits for Kite to redirect the operator's browser there with a
// request_token, exchanges it and saves the session.
type LoginFlow struct {
	Client      *Client
	Store       Store
	RedirectURL *url.URL
	// Now defaults to time.Now.
	Now func() time.Time
}

// Run serves the callback on ln until a login completes, fails, or ctx ends.
// It calls ready with the Kite login URL once the listener is serving. Run
// closes ln.
func (f *LoginFlow) Run(ctx context.Context, ln net.Listener, ready func(loginURL string)) (Session, error) {
	nonce, err := newNonce()
	if err != nil {
		ln.Close()
		return Session{}, err
	}
	cb := &callback{flow: f, ctx: ctx, nonce: nonce, done: make(chan loginResult, 1)}
	mux := http.NewServeMux()
	mux.Handle(f.RedirectURL.Path, cb)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	ready(f.Client.LoginURL(url.Values{stateParam: {nonce}}))

	select {
	case res := <-cb.done:
		return res.session, res.err
	case err := <-serveErr:
		return Session{}, fmt.Errorf("login callback server: %w", err)
	case <-ctx.Done():
		return Session{}, fmt.Errorf("waiting for kite login: %w", ctx.Err())
	}
}

// complete exchanges the request token and saves the session.
func (f *LoginFlow) complete(ctx context.Context, requestToken string) (Session, error) {
	userID, token, err := f.Client.ExchangeRequestToken(ctx, requestToken)
	if err != nil {
		return Session{}, err
	}
	now := time.Now()
	if f.Now != nil {
		now = f.Now()
	}
	sess := Session{UserID: userID, AccessToken: token, LoginTime: now, ExpiresAt: Expiry(now)}
	if err := f.Store.Save(sess); err != nil {
		return Session{}, err
	}
	return sess, nil
}

type loginResult struct {
	session Session
	err     error
}

// callback handles Kite's redirect. Requests that do not carry this login's
// nonce are refused without ending the flow, so a stray or forged request
// cannot abort a login in progress.
type callback struct {
	flow  *LoginFlow
	ctx   context.Context
	nonce string
	done  chan loginResult

	mu       sync.Mutex
	finished bool
}

func (cb *callback) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The request token is in the URL; keep it out of caches and referrers.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	if subtle.ConstantTimeCompare([]byte(q.Get(stateParam)), []byte(cb.nonce)) != 1 {
		http.Error(w, "This redirect does not belong to the running `jevtrader login`. Start the login from the URL it printed.", http.StatusBadRequest)
		return
	}

	cb.mu.Lock()
	defer cb.mu.Unlock()
	if cb.finished {
		http.Error(w, "This login has already finished. Run `jevtrader login` again if needed.", http.StatusConflict)
		return
	}

	if status := q.Get("status"); status != "success" {
		cb.finish(w, Session{}, fmt.Errorf("kite login was not successful (status %q)", status))
		return
	}
	requestToken := q.Get("request_token")
	if requestToken == "" {
		cb.finish(w, Session{}, errors.New("kite redirect has no request_token"))
		return
	}
	sess, err := cb.flow.complete(cb.ctx, requestToken)
	cb.finish(w, sess, err)
}

// finish reports the outcome to the browser and to Run. The caller holds mu.
func (cb *callback) finish(w http.ResponseWriter, sess Session, err error) {
	cb.finished = true
	cb.done <- loginResult{session: sess, err: err}
	if err != nil {
		http.Error(w, "Kite login failed: "+err.Error()+"\nSee the jevtrader terminal for details.", http.StatusBadGateway)
		return
	}
	fmt.Fprintf(w, "Kite login complete for %s. You can close this tab.\n", sess.UserID)
}

func newNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate login nonce: %w", err)
	}
	return hex.EncodeToString(b), nil
}
