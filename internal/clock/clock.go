// Package clock is the time edge shared by live trading, paper trading and
// replay. Wall follows real time; Sim moves only when the replay driver tells
// it to, so a backtest gives the same result on a slow or a fast machine.
package clock

import (
	"sync"
	"time"
)

// Clock tells the time and delivers timers.
type Clock interface {
	Now() time.Time
	// After returns a channel that receives the time once d has passed.
	After(d time.Duration) <-chan time.Time
}

// Settable is implemented by clocks whose time is driven by the caller. The
// engine moves such a clock to the time of each market-data event.
type Settable interface {
	Clock
	// Set moves the clock forward to t. A t at or before Now is ignored, so
	// simulated time never runs backwards.
	Set(t time.Time)
}

// Wall is the real clock.
type Wall struct{}

func (Wall) Now() time.Time                         { return time.Now() }
func (Wall) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Sim is a simulated clock. It starts at the time given to NewSim and moves
// only through Set and Advance. It is safe for concurrent use.
type Sim struct {
	mu     sync.Mutex
	now    time.Time
	timers []timer
}

type timer struct {
	at time.Time
	ch chan time.Time
}

var _ Settable = (*Sim)(nil)

// NewSim returns a simulated clock showing start.
func NewSim(start time.Time) *Sim { return &Sim{now: start} }

// Now returns the simulated time.
func (s *Sim) Now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now
}

// After returns a channel that receives the simulated time at which d has
// passed, once the clock is moved that far. A d of zero or less fires at once.
func (s *Sim) After(d time.Duration) <-chan time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- s.now
		return ch
	}
	s.timers = append(s.timers, timer{at: s.now.Add(d), ch: ch})
	return ch
}

// Set moves the clock forward to t and fires the timers that are due.
func (s *Sim) Set(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !t.After(s.now) {
		return
	}
	s.now = t
	due := s.timers[:0]
	for _, tm := range s.timers {
		if tm.at.After(t) {
			due = append(due, tm)
			continue
		}
		tm.ch <- t
	}
	s.timers = due
}

// Advance moves the clock forward by d.
func (s *Sim) Advance(d time.Duration) { s.Set(s.Now().Add(d)) }
