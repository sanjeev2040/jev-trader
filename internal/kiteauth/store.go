package kiteauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/config"
)

// SessionFileName is the session file's name inside the state directory.
const SessionFileName = "kite-session.json"

// ErrNoSession means no session file exists yet.
var ErrNoSession = errors.New("no kite session saved")

// ErrInsecureSessionFile means the session file is readable or writable by
// users other than its owner, so its token is not trusted.
var ErrInsecureSessionFile = errors.New("kite session file permissions are not 0600")

// Store keeps the session in a mode-0600 file under Dir. The file is
// replaced atomically, so a crash never leaves a half-written token.
type Store struct {
	Dir string
}

// sessionFile is the on-disk form. It is separate from Session because
// Session.AccessToken redacts itself when marshalled.
type sessionFile struct {
	UserID      string    `json:"user_id"`
	AccessToken string    `json:"access_token"`
	LoginTime   time.Time `json:"login_time"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// Path is the session file's path.
func (s Store) Path() string { return filepath.Join(s.Dir, SessionFileName) }

// Save writes sess to the session file with mode 0600, creating Dir with
// mode 0700 if needed.
func (s Store) Save(sess Session) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	data, err := json.MarshalIndent(sessionFile{
		UserID:      sess.UserID,
		AccessToken: sess.AccessToken.Reveal(),
		LoginTime:   sess.LoginTime,
		ExpiresAt:   sess.ExpiresAt,
	}, "", "  ")
	if err != nil {
		return err
	}
	// CreateTemp creates the file with mode 0600.
	tmp, err := os.CreateTemp(s.Dir, SessionFileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("save kite session: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("save kite session: %w", err)
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("save kite session: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("save kite session: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("save kite session: %w", err)
	}
	if err := os.Rename(tmp.Name(), s.Path()); err != nil {
		return fmt.Errorf("save kite session: %w", err)
	}
	return nil
}

// Load reads the session file. It returns ErrNoSession when there is none and
// ErrInsecureSessionFile when group or other users can access it.
func (s Store) Load() (Session, error) {
	f, err := os.Open(s.Path())
	if errors.Is(err, fs.ErrNotExist) {
		return Session{}, ErrNoSession
	}
	if err != nil {
		return Session{}, fmt.Errorf("load kite session: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Session{}, fmt.Errorf("load kite session: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Session{}, fmt.Errorf("load kite session: %s is not a regular file", s.Path())
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return Session{}, fmt.Errorf("%w: %s has mode %04o", ErrInsecureSessionFile, s.Path(), perm)
	}
	var sf sessionFile
	if err := json.NewDecoder(f).Decode(&sf); err != nil {
		return Session{}, fmt.Errorf("load kite session: %w", err)
	}
	return Session{
		UserID:      sf.UserID,
		AccessToken: config.NewSecret(sf.AccessToken),
		LoginTime:   sf.LoginTime,
		ExpiresAt:   sf.ExpiresAt,
	}, nil
}

// DefaultStateDir is $XDG_STATE_HOME/jevtrader, or ~/.local/state/jevtrader
// when XDG_STATE_HOME is unset.
func DefaultStateDir() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "jevtrader"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find state directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "jevtrader"), nil
}
