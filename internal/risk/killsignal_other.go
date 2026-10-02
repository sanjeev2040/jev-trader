//go:build !unix

package risk

import "os"

// notifyKillSignal returns a channel that never fires: SIGUSR1 exists only on
// Unix. The KILL file still works.
func notifyKillSignal() (<-chan os.Signal, func()) {
	return nil, func() {}
}
