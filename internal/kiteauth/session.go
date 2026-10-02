// Package kiteauth handles the daily Kite Connect login: the manual browser
// flow that yields a request_token, its exchange for an access_token, the
// 0600 session file in the state directory, and the expiry check.
//
// There is deliberately no automated credential or TOTP login. It would mean
// storing the owner's password and 2FA seed, and the daily manual login is an
// intended control (reports/0002-architecture.md).
//
// A session is valid from any IP. Kite accepts order placement only from the
// static IP registered in its developer console, so a valid session does not
// by itself mean orders can be placed from this host.
package kiteauth

import (
	"time"

	"github.com/sanjeev2040/jev-trader/internal/config"
)

// ist is India Standard Time. India has no daylight saving, so a fixed zone
// is exact and needs no tzdata on the host.
var ist = time.FixedZone("IST", 5*60*60+30*60)

// expiryHour is the IST hour at which Kite invalidates every access token.
const expiryHour = 6

// Session is a Kite Connect login that lasts until ExpiresAt.
type Session struct {
	UserID      string
	AccessToken config.Secret
	LoginTime   time.Time
	ExpiresAt   time.Time
}

// Valid reports whether the session has a token and has not expired at now.
func (s Session) Valid(now time.Time) bool {
	return !s.AccessToken.IsZero() && now.Before(s.ExpiresAt)
}

// Expiry returns when a token issued at loginTime expires: the first 06:00 IST
// strictly after loginTime. A login at 05:30 IST therefore lasts only until
// 06:00 the same morning.
func Expiry(loginTime time.Time) time.Time {
	t := loginTime.In(ist)
	expiry := time.Date(t.Year(), t.Month(), t.Day(), expiryHour, 0, 0, 0, ist)
	if !expiry.After(t) {
		expiry = expiry.AddDate(0, 0, 1)
	}
	return expiry
}
