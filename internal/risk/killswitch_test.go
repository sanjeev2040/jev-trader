package risk

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func startWatch(t *testing.T, dir string) (<-chan Reason, func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan Reason)
	done := make(chan error, 1)
	go func() { done <- WatchKill(ctx, dir, 10*time.Millisecond, out) }()
	return out, func() error { cancel(); return <-done }
}

func recv(t *testing.T, ch <-chan Reason) Reason {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("no kill request received")
		return ""
	}
}

func TestWatchKillFile(t *testing.T) {
	dir := t.TempDir()
	out, stop := startWatch(t, dir)

	select {
	case r := <-out:
		t.Fatalf("kill request %s with no KILL file", r)
	case <-time.After(50 * time.Millisecond):
	}
	if err := os.WriteFile(filepath.Join(dir, KillFileName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if r := recv(t, out); r != ReasonKillFile {
		t.Fatalf("reason = %s, want %s", r, ReasonKillFile)
	}
	if err := stop(); err != context.Canceled {
		t.Fatalf("WatchKill returned %v, want context.Canceled", err)
	}
}

func TestWatchKillFilePresentAtStart(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, KillFileName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	out, stop := startWatch(t, dir)
	defer stop()
	if r := recv(t, out); r != ReasonKillFile {
		t.Fatalf("reason = %s, want %s", r, ReasonKillFile)
	}
}

func TestWatchKillRejectsBadPoll(t *testing.T) {
	if err := WatchKill(context.Background(), t.TempDir(), 0, make(chan Reason)); err == nil {
		t.Fatal("WatchKill accepted a zero poll interval")
	}
}
