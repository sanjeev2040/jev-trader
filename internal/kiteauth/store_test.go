package kiteauth

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/config"
)

func testSession() Session {
	login := time.Date(2026, 10, 2, 8, 45, 0, 0, ist)
	return Session{
		UserID:      testUserID,
		AccessToken: config.NewSecret(testAccessToken),
		LoginTime:   login,
		ExpiresAt:   Expiry(login),
	}
}

func TestStoreRoundTrip(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "state")}
	want := testSession()
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.UserID != want.UserID || got.AccessToken.Reveal() != want.AccessToken.Reveal() ||
		!got.LoginTime.Equal(want.LoginTime) || !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Errorf("Load = %+v, want %+v", got, want)
	}
}

func TestStorePermissions(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "state")}
	if err := s.Save(testSession()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("session file mode = %04o, want 0600", perm)
	}
	dirInfo, err := os.Stat(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("state dir mode = %04o, want 0700", perm)
	}
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("state dir has %d entries, want only the session file", len(entries))
	}
}

func TestStoreOverwriteTightensPermissions(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	if err := os.WriteFile(s.Path(), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(testSession()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); err != nil {
		t.Fatalf("Load after Save over a 0644 file: %v", err)
	}
}

func TestStoreLoadMissing(t *testing.T) {
	_, err := Store{Dir: t.TempDir()}.Load()
	if !errors.Is(err, ErrNoSession) {
		t.Errorf("err = %v, want ErrNoSession", err)
	}
}

func TestStoreLoadInsecure(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	if err := s.Save(testSession()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.Path(), 0o640); err != nil {
		t.Fatal(err)
	}
	_, err := s.Load()
	if !errors.Is(err, ErrInsecureSessionFile) {
		t.Errorf("err = %v, want ErrInsecureSessionFile", err)
	}
}

func TestStoreLoadCorrupt(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	if err := os.WriteFile(s.Path(), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); err == nil || errors.Is(err, ErrNoSession) {
		t.Errorf("err = %v, want a decode error", err)
	}
}

func TestDefaultStateDir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/var/lib/xdg")
	got, err := DefaultStateDir()
	if err != nil || got != filepath.Join("/var/lib/xdg", "jevtrader") {
		t.Errorf("with XDG_STATE_HOME: got (%q, %v)", got, err)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "relative/ignored")
	got, err = DefaultStateDir()
	if err != nil || got != filepath.Join(home, ".local", "state", "jevtrader") {
		t.Errorf("without absolute XDG_STATE_HOME: got (%q, %v)", got, err)
	}
	if strings.Contains(got, "relative") {
		t.Errorf("relative XDG_STATE_HOME should be ignored, got %q", got)
	}
}
