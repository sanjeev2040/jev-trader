package screen

import (
	"context"
	"errors"
	"io"
	"net"
	"time"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"
)

// Transient Kite failures are retried: over a 26-week study one request in a
// few thousand meets a rate-limit answer, a gateway timeout or a dropped
// connection, and failing the whole run for it would waste hours of fetches.
const RetryAttempts = 4 // tries a request gets in all

// RetryBackoff is the wait before the second try; it doubles each time. A
// variable so that tests do not wait.
var RetryBackoff = time.Second

// IsTransient reports whether err is worth trying again: Kite's general,
// data and network errors (a rate limit arrives as a general error), a
// network error, or a connection cut short. A token or permission error, bad
// input and a cancelled context are not.
func IsTransient(err error) bool {
	if err == nil || errors.Is(err, ErrAbort) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var kerr kiteconnect.Error
	if errors.As(err, &kerr) {
		switch kerr.ErrorType {
		case kiteconnect.GeneralError, kiteconnect.DataError, kiteconnect.NetworkError:
			return true
		}
		return false
	}
	var nerr net.Error
	return errors.As(err, &nerr) || errors.Is(err, io.ErrUnexpectedEOF)
}

// Retry calls fn, again after a doubling wait while it fails with a transient
// error, up to attempts tries; it returns the last result. A cancelled context
// ends the wait.
func Retry[T any](ctx context.Context, attempts int, backoff time.Duration, fn func() (T, error)) (T, error) {
	var out T
	var err error
	for i := range attempts {
		if out, err = fn(); err == nil || !IsTransient(err) || i == attempts-1 {
			return out, err
		}
		select {
		case <-time.After(backoff << i):
		case <-ctx.Done():
			return out, ctx.Err()
		}
	}
	return out, err
}
