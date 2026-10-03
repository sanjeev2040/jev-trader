package risk

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// KillFileName is the file whose presence in the state directory trips the
// kill switch.
const KillFileName = "KILL"

// WatchKill reports operator kill requests on out until ctx ends: one
// ReasonKillSignal for each SIGUSR1 (on Unix), and one ReasonKillFile on
// every poll while a KILL file exists in stateDir. The engine passes each
// reason to Book.Kill. The file is checked once immediately, so a KILL file
// left in place before startup trips the switch before trading begins.
func WatchKill(ctx context.Context, stateDir string, poll time.Duration, out chan<- Reason) error {
	if poll <= 0 {
		return errors.New("risk: kill-file poll interval must be positive")
	}
	sig, stop := notifyKillSignal()
	defer stop()
	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	path := filepath.Join(stateDir, KillFileName)
	send := func(r Reason) bool {
		select {
		case out <- r:
			return true
		case <-ctx.Done():
			return false
		}
	}
	for {
		if _, err := os.Stat(path); err == nil {
			if !send(ReasonKillFile) {
				return ctx.Err()
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			// A state directory we cannot read is treated as a kill request:
			// the switch must fail closed.
			if !send(ReasonKillFile) {
				return ctx.Err()
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-sig:
			if !send(ReasonKillSignal) {
				return ctx.Err()
			}
		case <-ticker.C:
		}
	}
}
