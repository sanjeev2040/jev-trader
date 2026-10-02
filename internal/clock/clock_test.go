package clock

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 5, 9, 15, 0, 0, time.UTC)

func TestSimMovesOnlyForward(t *testing.T) {
	s := NewSim(t0)
	s.Set(t0.Add(time.Minute))
	s.Set(t0) // ignored
	if got := s.Now(); !got.Equal(t0.Add(time.Minute)) {
		t.Errorf("Now = %v, want %v", got, t0.Add(time.Minute))
	}
	s.Advance(time.Second)
	if got := s.Now(); !got.Equal(t0.Add(time.Minute + time.Second)) {
		t.Errorf("after Advance Now = %v", got)
	}
}

func TestSimAfterFiresWhenTimeReachesIt(t *testing.T) {
	s := NewSim(t0)
	ch := s.After(time.Minute)
	select {
	case <-ch:
		t.Fatal("timer fired before the clock moved")
	default:
	}
	s.Set(t0.Add(30 * time.Second))
	select {
	case <-ch:
		t.Fatal("timer fired early")
	default:
	}
	s.Set(t0.Add(2 * time.Minute))
	select {
	case got := <-ch:
		if !got.Equal(t0.Add(2 * time.Minute)) {
			t.Errorf("fired with %v", got)
		}
	default:
		t.Fatal("timer did not fire")
	}
	select {
	case <-s.After(0):
	default:
		t.Error("After(0) should fire at once")
	}
}

func TestWallFollowsRealTime(t *testing.T) {
	var c Clock = Wall{}
	if d := time.Since(c.Now()); d < 0 || d > time.Second {
		t.Errorf("Wall.Now is %v off", d)
	}
	select {
	case <-c.After(time.Millisecond):
	case <-time.After(time.Second):
		t.Fatal("Wall.After did not fire")
	}
}
