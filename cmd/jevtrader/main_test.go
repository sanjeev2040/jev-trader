package main

import (
	"context"
	"strings"
	"testing"
)

func TestRecordValidatesInput(t *testing.T) {
	t.Setenv("KITE_API_KEY", "")
	e, _, stderr := testEnv(nil)
	if code := run(context.Background(), []string{"record"}, e); code != 2 || !strings.Contains(stderr.String(), "-watchlist") {
		t.Errorf("no watchlist: code %d, output %q", code, stderr.String())
	}
	e, _, stderr = testEnv(nil)
	if code := run(context.Background(), []string{"record", "-watchlist", "INFY"}, e); code != 2 || !strings.Contains(stderr.String(), "KITE_API_KEY") {
		t.Errorf("no API key: code %d, output %q", code, stderr.String())
	}
}
func TestEnvAccessToken(t *testing.T) {
	t.Setenv("KITE_ACCESS_TOKEN", "")
	if _, err := (envAccessToken{}).Token(t.Context()); err == nil {
		t.Error("expected an error when KITE_ACCESS_TOKEN is unset")
	}
	t.Setenv("KITE_ACCESS_TOKEN", "abc")
	if tok, err := (envAccessToken{}).Token(t.Context()); err != nil || tok != "abc" {
		t.Errorf("got %q, %v", tok, err)
	}
}

func TestSplitList(t *testing.T) {
	got := splitList(" NSE:INFY, ,NSE:NIFTY 50,")
	if len(got) != 2 || got[0] != "NSE:INFY" || got[1] != "NSE:NIFTY 50" {
		t.Errorf("got %q", got)
	}
}
