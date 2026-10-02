//go:build unix

package risk

import (
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

func TestWatchKillSignal(t *testing.T) {
	// Catch SIGUSR1 here too, so a signal sent before WatchKill registers
	// cannot terminate the test binary.
	guard := make(chan os.Signal, 1)
	signal.Notify(guard, syscall.SIGUSR1)
	defer signal.Stop(guard)

	out, stop := startWatch(t, t.TempDir())
	defer stop()
	deadline := time.After(2 * time.Second)
	for {
		if err := syscall.Kill(syscall.Getpid(), syscall.SIGUSR1); err != nil {
			t.Fatal(err)
		}
		select {
		case r := <-out:
			if r != ReasonKillSignal {
				t.Fatalf("reason = %s, want %s", r, ReasonKillSignal)
			}
			return
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			t.Fatal("no kill request received")
		}
	}
}
