package screen

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"

	"github.com/sanjeev2040/jev-trader/internal/jev"
)

type countAsker struct{ n int }

func (c *countAsker) Ask(context.Context, any, map[string]jev.Question) (jev.Answers, error) {
	c.n++
	return jev.Answers{}, nil
}

func TestLimitedAskerSpacesTheCalls(t *testing.T) {
	inner := &countAsker{}
	a := NewLimitedAsker(inner, 50) // one every 20ms
	start := time.Now()
	for range 5 {
		if _, err := a.Ask(context.Background(), nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d < 70*time.Millisecond || inner.n != 5 {
		t.Errorf("5 calls took %v (%d reached Jev); want at least 4 intervals of 20ms", d, inner.n)
	}
	if NewLimitedAsker(inner, 0) != Asker(inner) {
		t.Error("a zero rate should leave the asker alone")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewLimitedAsker(inner, 1).Ask(ctx, nil, nil); err == nil {
		t.Error("a cancelled context waited for a slot")
	}
}

func TestRetryTriesTransientErrorsAgain(t *testing.T) {
	calls := 0
	got, err := Retry(context.Background(), 4, time.Millisecond, func() (int, error) {
		calls++
		if calls < 3 {
			return 0, kiteconnect.NewError(kiteconnect.GeneralError, "Too many requests", nil)
		}
		return 7, nil
	})
	if err != nil || got != 7 || calls != 3 {
		t.Errorf("got %d, %v after %d calls", got, err, calls)
	}

	for name, fatal := range map[string]error{
		"token":      kiteconnect.NewError(kiteconnect.TokenError, "expired", nil),
		"input":      kiteconnect.NewError(kiteconnect.InputError, "bad", nil),
		"abort":      ErrAbort,
		"plain":      errors.New("boom"),
		"cancelled":  context.Canceled,
		"permission": kiteconnect.NewError(kiteconnect.PermissionError, "no", nil),
	} {
		calls = 0
		_, err := Retry(context.Background(), 4, time.Millisecond, func() (int, error) { calls++; return 0, fatal })
		if !errors.Is(err, fatal) && err != fatal || calls != 1 {
			t.Errorf("%s: %d calls, err %v", name, calls, err)
		}
	}

	calls = 0
	_, err = Retry(context.Background(), 3, time.Millisecond, func() (int, error) { calls++; return 0, &net.OpError{Op: "read", Err: errors.New("reset")} })
	if err == nil || calls != 3 {
		t.Errorf("a network error gave %d calls, err %v; want 3 tries then the error", calls, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	calls = 0
	_, err = Retry(ctx, 4, time.Hour, func() (int, error) {
		calls++
		cancel()
		return 0, kiteconnect.NewError(kiteconnect.NetworkError, "down", nil)
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Errorf("cancelled wait: %d calls, err %v", calls, err)
	}
}
