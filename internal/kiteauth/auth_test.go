package kiteauth

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func savedAuth(t *testing.T, now time.Time) *Auth {
	t.Helper()
	a := &Auth{Store: Store{Dir: t.TempDir()}, Now: func() time.Time { return now }}
	if err := a.Store.Save(testSession()); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAuthToken(t *testing.T) {
	a := savedAuth(t, time.Date(2026, 10, 2, 15, 0, 0, 0, ist))
	tok, err := a.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok.Reveal() != testAccessToken {
		t.Errorf("token = %q, want %q", tok.Reveal(), testAccessToken)
	}
}

func TestAuthTokenExpired(t *testing.T) {
	a := savedAuth(t, time.Date(2026, 10, 3, 6, 0, 0, 0, ist))
	if _, err := a.Token(context.Background()); !errors.Is(err, ErrLoginRequired) {
		t.Errorf("err = %v, want ErrLoginRequired", err)
	}
}

func TestAuthTokenNoSession(t *testing.T) {
	a := &Auth{Store: Store{Dir: t.TempDir()}}
	_, err := a.Token(context.Background())
	if !errors.Is(err, ErrLoginRequired) || !errors.Is(err, ErrNoSession) {
		t.Errorf("err = %v, want ErrLoginRequired wrapping ErrNoSession", err)
	}
}

func TestAuthTokenInsecureFile(t *testing.T) {
	a := savedAuth(t, time.Date(2026, 10, 2, 15, 0, 0, 0, ist))
	if err := os.Chmod(a.Store.Path(), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := a.Token(context.Background())
	if !errors.Is(err, ErrLoginRequired) || !errors.Is(err, ErrInsecureSessionFile) {
		t.Errorf("err = %v, want ErrLoginRequired wrapping ErrInsecureSessionFile", err)
	}
}

func TestAuthTokenCanceled(t *testing.T) {
	a := savedAuth(t, time.Date(2026, 10, 2, 15, 0, 0, 0, ist))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Token(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestAuthVerify(t *testing.T) {
	now := time.Date(2026, 10, 2, 9, 0, 0, 0, ist)

	t.Run("live token", func(t *testing.T) {
		fk := newFakeKite(t)
		p, err := savedAuth(t, now).Verify(context.Background(), fk.client())
		if err != nil || p.UserID != testUserID {
			t.Fatalf("Verify = (%+v, %v)", p, err)
		}
	})
	t.Run("token rejected by kite", func(t *testing.T) {
		fk := newFakeKite(t)
		fk.tokenRejected = true
		_, err := savedAuth(t, now).Verify(context.Background(), fk.client())
		if !errors.Is(err, ErrLoginRequired) || !IsTokenException(err) {
			t.Errorf("err = %v, want ErrLoginRequired wrapping a TokenException", err)
		}
	})
	t.Run("token for another user", func(t *testing.T) {
		fk := newFakeKite(t)
		fk.profileUserID = "ZZ9999"
		if _, err := savedAuth(t, now).Verify(context.Background(), fk.client()); !errors.Is(err, ErrLoginRequired) {
			t.Errorf("err = %v, want ErrLoginRequired", err)
		}
	})
	t.Run("kite unreachable is not a login problem", func(t *testing.T) {
		fk := newFakeKite(t)
		c := fk.client()
		fk.srv.Close()
		_, err := savedAuth(t, now).Verify(context.Background(), c)
		if err == nil || errors.Is(err, ErrLoginRequired) {
			t.Errorf("err = %v, want a transport error without ErrLoginRequired", err)
		}
	})
}
