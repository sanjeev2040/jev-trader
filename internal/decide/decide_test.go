package decide

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/domain"
)

var state = json.RawMessage(`{"schema_version":1}`)

// fake is a scripted Decider.
type fake struct {
	mu    sync.Mutex
	calls int
	fn    func(ctx context.Context) (domain.Decision, error)
}

func (f *fake) Decide(ctx context.Context, _ json.RawMessage) (domain.Decision, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return f.fn(ctx)
}

func (f *fake) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func buy() domain.Decision {
	return domain.Decision{
		Action:            domain.ActionBuy,
		Size:              domain.SizeM,
		Probability:       0.7,
		Probabilities:     map[domain.Action]float64{domain.ActionBuy: 0.7, domain.ActionSell: 0.1, domain.ActionHold: 0.2},
		SizeProbabilities: map[string]float64{domain.SizeS: 0.2, domain.SizeM: 0.6, domain.SizeL: 0.2},
		Model:             "jev-2026-09-01",
	}
}

func ok() *fake {
	return &fake{fn: func(context.Context) (domain.Decision, error) { return buy(), nil }}
}

func failing(err error) *fake {
	return &fake{fn: func(context.Context) (domain.Decision, error) { return domain.Decision{}, err }}
}

func returning(d domain.Decision) *fake {
	return &fake{fn: func(context.Context) (domain.Decision, error) { return d, nil }}
}

func decide(t *testing.T, d Decider, ctx context.Context) domain.Decision {
	t.Helper()
	got, err := d.Decide(ctx, state)
	if err != nil {
		t.Fatalf("Decide returned error %v; HoldOnFailure must never fail", err)
	}
	return got
}

func TestHoldOnFailurePassesValidDecision(t *testing.T) {
	got := decide(t, NewHoldOnFailure(ok()), context.Background())
	if got.Action != domain.ActionBuy || got.Size != domain.SizeM || got.HoldReason != "" {
		t.Errorf("decision = %+v", got)
	}
}

func TestHoldOnFailureFailures(t *testing.T) {
	bad := func(mod func(*domain.Decision)) *fake {
		d := buy()
		mod(&d)
		return returning(d)
	}
	tests := []struct {
		name   string
		inner  *fake
		reason string
	}{
		{"typed error keeps reason", failing(&domain.DecideError{Reason: domain.ReasonHTTP4xx}), domain.ReasonHTTP4xx},
		{"wrapped typed error", failing(errors.Join(errors.New("x"), &domain.DecideError{Reason: domain.ReasonRateLimited})), domain.ReasonRateLimited},
		{"untyped error", failing(errors.New("boom")), domain.ReasonError},
		{"deadline error", failing(context.DeadlineExceeded), domain.ReasonTimeout},
		{"unknown action", bad(func(d *domain.Decision) { d.Action = "SHORT" }), domain.ReasonInvalidAnswer},
		{"unknown size", bad(func(d *domain.Decision) { d.Size = "XL" }), domain.ReasonInvalidAnswer},
		{"buy without size", bad(func(d *domain.Decision) { d.Size = "" }), domain.ReasonInvalidAnswer},
		{"probability out of range", bad(func(d *domain.Decision) { d.Probability = 1.2 }), domain.ReasonInvalidAnswer},
		{"action probabilities do not sum to 1", bad(func(d *domain.Decision) {
			d.Probabilities[domain.ActionHold] = 0.6
		}), domain.ReasonInvalidAnswer},
		{"size probabilities do not sum to 1", bad(func(d *domain.Decision) {
			d.SizeProbabilities[domain.SizeL] = 0.9
		}), domain.ReasonInvalidAnswer},
		{"probability for unknown action", bad(func(d *domain.Decision) {
			d.Probabilities["WAIT"] = 0
		}), domain.ReasonInvalidAnswer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decide(t, NewHoldOnFailure(tt.inner), context.Background())
			if got.Action != domain.ActionHold || got.HoldReason != tt.reason {
				t.Errorf("got %s/%q, want HOLD/%q", got.Action, got.HoldReason, tt.reason)
			}
			if got.Size != "" || got.Probability != 0 {
				t.Errorf("substituted HOLD carries size %q probability %v", got.Size, got.Probability)
			}
		})
	}
}

func TestHoldOnFailureAcceptsDeciderHoldWithoutSize(t *testing.T) {
	got := decide(t, NewHoldOnFailure(returning(domain.Decision{Action: domain.ActionHold, Probability: 1})), context.Background())
	if got.Action != domain.ActionHold || got.HoldReason != "" {
		t.Errorf("decision = %+v", got)
	}
}

func TestHoldOnFailureDeadline(t *testing.T) {
	t.Run("inner ignores its context", func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		inner := &fake{fn: func(context.Context) (domain.Decision, error) {
			<-release
			return buy(), nil
		}}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		start := time.Now()
		got := decide(t, NewHoldOnFailure(inner), ctx)
		if got.Action != domain.ActionHold || got.HoldReason != domain.ReasonTimeout {
			t.Errorf("got %s/%q, want HOLD/timeout", got.Action, got.HoldReason)
		}
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Errorf("returned after %v; must return at the deadline", elapsed)
		}
	})
	t.Run("answer arrives after the deadline", func(t *testing.T) {
		inner := &fake{fn: func(ctx context.Context) (domain.Decision, error) {
			<-ctx.Done()
			return buy(), nil
		}}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		got := decide(t, NewHoldOnFailure(inner), ctx)
		if got.Action != domain.ActionHold || got.HoldReason != domain.ReasonTimeout {
			t.Errorf("got %s/%q, want HOLD/timeout", got.Action, got.HoldReason)
		}
	})
	t.Run("deadline already passed", func(t *testing.T) {
		inner := ok()
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		got := decide(t, NewHoldOnFailure(inner), ctx)
		if got.HoldReason != domain.ReasonTimeout || inner.Calls() != 0 {
			t.Errorf("got %q after %d calls, want timeout without calling", got.HoldReason, inner.Calls())
		}
	})
}

func TestCircuitBreaker(t *testing.T) {
	now := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	fail := true
	inner := &fake{fn: func(context.Context) (domain.Decision, error) {
		if fail {
			return domain.Decision{}, &domain.DecideError{Reason: domain.ReasonHTTP5xx}
		}
		return buy(), nil
	}}
	h := NewHoldOnFailure(inner, WithCircuitBreaker(3, time.Minute), WithNow(clock))
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if got := decide(t, h, ctx); got.HoldReason != domain.ReasonHTTP5xx {
			t.Fatalf("call %d: reason %q", i, got.HoldReason)
		}
	}
	// Open: the inner decider is not called.
	if got := decide(t, h, ctx); got.HoldReason != domain.ReasonCircuitOpen || inner.Calls() != 3 {
		t.Fatalf("open: reason %q after %d calls", got.HoldReason, inner.Calls())
	}

	// After the cool-down a failed probe reopens the breaker.
	now = now.Add(time.Minute)
	if got := decide(t, h, ctx); got.HoldReason != domain.ReasonHTTP5xx || inner.Calls() != 4 {
		t.Fatalf("probe: reason %q after %d calls", got.HoldReason, inner.Calls())
	}
	if got := decide(t, h, ctx); got.HoldReason != domain.ReasonCircuitOpen || inner.Calls() != 4 {
		t.Fatalf("reopened: reason %q after %d calls", got.HoldReason, inner.Calls())
	}

	// A successful probe closes it.
	now = now.Add(time.Minute)
	fail = false
	if got := decide(t, h, ctx); got.Action != domain.ActionBuy {
		t.Fatalf("probe: %+v", got)
	}
	fail = true
	if got := decide(t, h, ctx); got.HoldReason != domain.ReasonHTTP5xx {
		t.Fatalf("closed: reason %q, want the inner failure, not circuit_open", got.HoldReason)
	}
}

func TestCircuitBreakerSuccessResetsCount(t *testing.T) {
	results := []error{errors.New("a"), errors.New("b"), nil, errors.New("c"), errors.New("d")}
	i := 0
	inner := &fake{fn: func(context.Context) (domain.Decision, error) {
		err := results[i]
		i++
		if err != nil {
			return domain.Decision{}, err
		}
		return buy(), nil
	}}
	h := NewHoldOnFailure(inner, WithCircuitBreaker(3, time.Minute))
	for range results {
		if got := decide(t, h, context.Background()); got.HoldReason == domain.ReasonCircuitOpen {
			t.Fatalf("breaker opened without 3 consecutive failures")
		}
	}
}

func TestCircuitBreakerSingleProbe(t *testing.T) {
	now := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	probing := false
	inner := &fake{fn: func(context.Context) (domain.Decision, error) {
		mu.Lock()
		p := probing
		mu.Unlock()
		if !p {
			return domain.Decision{}, errors.New("down")
		}
		started <- struct{}{}
		<-release
		return buy(), nil
	}}
	h := NewHoldOnFailure(inner, WithCircuitBreaker(1, time.Minute), WithNow(clock))
	decide(t, h, context.Background()) // opens

	mu.Lock()
	now = now.Add(time.Minute)
	probing = true
	mu.Unlock()
	done := make(chan domain.Decision)
	go func() { d, _ := h.Decide(context.Background(), state); done <- d }()
	<-started
	if got := decide(t, h, context.Background()); got.HoldReason != domain.ReasonCircuitOpen {
		t.Errorf("concurrent call during probe: reason %q, want circuit_open", got.HoldReason)
	}
	close(release)
	if got := <-done; got.Action != domain.ActionBuy {
		t.Errorf("probe: %+v", got)
	}
}

func TestHoldOnFailureConcurrentUse(t *testing.T) {
	h := NewHoldOnFailure(ok())
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d, err := h.Decide(context.Background(), state); err != nil || d.Action != domain.ActionBuy {
				t.Errorf("Decide = %+v, %v", d, err)
			}
		}()
	}
	wg.Wait()
}
