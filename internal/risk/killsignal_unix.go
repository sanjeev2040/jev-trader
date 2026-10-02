//go:build unix

package risk

import (
	"os"
	"os/signal"
	"syscall"
)

func notifyKillSignal() (<-chan os.Signal, func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGUSR1)
	return ch, func() { signal.Stop(ch) }
}
