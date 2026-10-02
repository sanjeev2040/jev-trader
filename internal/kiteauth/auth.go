package kiteauth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/config"
)

// ErrLoginRequired means there is no usable session: none was saved, it
// expired, or Kite rejected it. The operator must run `jevtrader login`.
var ErrLoginRequired = errors.New("kite login required: run `jevtrader login`")

// Auth hands out the saved access token while it is valid.
type Auth struct {
	Store Store
	// Now defaults to time.Now; tests and replay inject their own clock.
	Now func() time.Time
}

// Session returns the saved session if it is still valid at Now. Every
// failure wraps ErrLoginRequired, because a fresh login replaces the file.
func (a *Auth) Session(ctx context.Context) (Session, error) {
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}
	sess, err := a.Store.Load()
	if err != nil {
		return Session{}, fmt.Errorf("%w: %w", ErrLoginRequired, err)
	}
	if now := a.now(); !sess.Valid(now) {
		return Session{}, fmt.Errorf("%w: session expired at %s", ErrLoginRequired, sess.ExpiresAt.In(ist).Format(time.DateTime+" MST"))
	}
	return sess, nil
}

// Token returns a valid access token or an error wrapping ErrLoginRequired.
func (a *Auth) Token(ctx context.Context) (config.Secret, error) {
	sess, err := a.Session(ctx)
	if err != nil {
		return config.Secret{}, err
	}
	return sess.AccessToken, nil
}

// Verify checks the saved session against Kite with a profile call, as `run`
// does at startup before it will trade. A TokenException wraps
// ErrLoginRequired; other failures (network, Kite outage) are returned as is.
func (a *Auth) Verify(ctx context.Context, c *Client) (Profile, error) {
	sess, err := a.Session(ctx)
	if err != nil {
		return Profile{}, err
	}
	p, err := c.Profile(ctx, sess.AccessToken)
	if IsTokenException(err) {
		return Profile{}, fmt.Errorf("%w: %w", ErrLoginRequired, err)
	}
	if err != nil {
		return Profile{}, err
	}
	if p.UserID != sess.UserID {
		return Profile{}, fmt.Errorf("%w: token belongs to %q, session was saved for %q", ErrLoginRequired, p.UserID, sess.UserID)
	}
	return p, nil
}

func (a *Auth) now() time.Time {
	if a.Now == nil {
		return time.Now()
	}
	return a.Now()
}
