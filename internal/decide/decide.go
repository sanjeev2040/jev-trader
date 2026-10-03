// Package decide defines the Decider interface and HoldOnFailure, the wrapper
// that turns every decider failure into a HOLD.
package decide

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/domain"
)

// Decider turns an encoded snapshot.Snapshot into a decision.
type Decider interface {
	Decide(ctx context.Context, state json.RawMessage) (domain.Decision, error)
}

const (
	DefaultFailureThreshold = 5
	DefaultCooldown         = 30 * time.Second

	probabilityTolerance = 0.02
)

// HoldOnFailure wraps a Decider and never fails. It returns HOLD, with
// HoldReason set to a domain reason code, when the inner decider:
//   - returns an error (a *domain.DecideError keeps its reason);
//   - answers after the context's deadline, or not before it;
//   - returns an unknown action or size, or probabilities that do not sum to
//     about 1.
//
// After FailureThreshold consecutive failures it opens a circuit breaker: for
// Cooldown it returns HOLD with reason circuit_open without calling the inner
// decider. After the cool-down one probe call is let through; success closes
// the breaker and failure reopens it.
//
// HoldOnFailure is safe for concurrent use.
type HoldOnFailure struct {
	inner     Decider
	threshold int
	cooldown  time.Duration
	now       func() time.Time

	mu        sync.Mutex
	failures  int
	openUntil time.Time
	probing   bool
}

// Option configures HoldOnFailure.
type Option func(*HoldOnFailure)

// WithCircuitBreaker sets the consecutive-failure threshold and the cool-down.
func WithCircuitBreaker(threshold int, cooldown time.Duration) Option {
	return func(h *HoldOnFailure) {
		h.threshold = threshold
		h.cooldown = cooldown
	}
}

// WithNow sets the time source used by the circuit breaker.
func WithNow(now func() time.Time) Option {
	return func(h *HoldOnFailure) { h.now = now }
}

// NewHoldOnFailure wraps inner.
func NewHoldOnFailure(inner Decider, opts ...Option) *HoldOnFailure {
	h := &HoldOnFailure{
		inner:     inner,
		threshold: DefaultFailureThreshold,
		cooldown:  DefaultCooldown,
		now:       time.Now,
	}
	for _, o := range opts {
		o(h)
	}
	if h.threshold <= 0 {
		h.threshold = DefaultFailureThreshold
	}
	return h
}

// Decide always returns a nil error.
func (h *HoldOnFailure) Decide(ctx context.Context, state json.RawMessage) (domain.Decision, error) {
	start := time.Now()
	if !h.allow() {
		return hold(domain.ReasonCircuitOpen, time.Since(start)), nil
	}
	d, err := h.call(ctx, state)
	if err == nil {
		err = Validate(d)
	}
	if err != nil {
		h.record(false)
		return hold(reasonOf(err), time.Since(start)), nil
	}
	h.record(true)
	return d, nil
}

type result struct {
	d   domain.Decision
	err error
}

// call runs the inner decider but returns at the context's deadline even if
// the inner decider ignores its context.
func (h *HoldOnFailure) call(ctx context.Context, state json.RawMessage) (domain.Decision, error) {
	if err := ctx.Err(); err != nil {
		return domain.Decision{}, err
	}
	done := make(chan result, 1)
	go func() {
		d, err := h.inner.Decide(ctx, state)
		done <- result{d, err}
	}()
	select {
	case r := <-done:
		if r.err == nil && ctx.Err() != nil {
			// The answer arrived after the deadline.
			return domain.Decision{}, ctx.Err()
		}
		return r.d, r.err
	case <-ctx.Done():
		return domain.Decision{}, ctx.Err()
	}
}

func (h *HoldOnFailure) allow() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.openUntil.IsZero() {
		return true
	}
	if h.now().Before(h.openUntil) || h.probing {
		return false
	}
	h.probing = true
	return true
}

func (h *HoldOnFailure) record(ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.probing = false
	if ok {
		h.failures = 0
		h.openUntil = time.Time{}
		return
	}
	h.failures++
	if h.failures >= h.threshold {
		h.openUntil = h.now().Add(h.cooldown)
	}
}

func hold(reason string, latency time.Duration) domain.Decision {
	return domain.Decision{
		Action:        domain.ActionHold,
		Probabilities: map[domain.Action]float64{},
		HoldReason:    reason,
		Latency:       latency,
	}
}

func reasonOf(err error) string {
	var derr *domain.DecideError
	if errors.As(err, &derr) && derr.Reason != "" {
		return derr.Reason
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return domain.ReasonTimeout
	}
	return domain.ReasonError
}

// Validate reports a *domain.DecideError with reason invalid_answer when d has
// an unknown action or size, a BUY or SELL without a size, or a probability distribution that is out of
// range or does not sum to about 1.
func Validate(d domain.Decision) error {
	switch d.Action {
	case domain.ActionBuy, domain.ActionSell, domain.ActionHold:
	default:
		return invalidf("unknown action %q", d.Action)
	}
	switch d.Size {
	case domain.SizeS, domain.SizeM, domain.SizeL:
	case "":
		if d.Action != domain.ActionHold {
			return invalidf("%s without a size", d.Action)
		}
	default:
		return invalidf("unknown size %q", d.Size)
	}
	if !validProbability(d.Probability) {
		return invalidf("probability %v out of range", d.Probability)
	}
	actions := make(map[string]float64, len(d.Probabilities))
	for a, p := range d.Probabilities {
		switch a {
		case domain.ActionBuy, domain.ActionSell, domain.ActionHold:
		default:
			return invalidf("probability for unknown action %q", a)
		}
		actions[string(a)] = p
	}
	if err := checkDistribution("action", actions); err != nil {
		return err
	}
	for s := range d.SizeProbabilities {
		switch s {
		case domain.SizeS, domain.SizeM, domain.SizeL:
		default:
			return invalidf("probability for unknown size %q", s)
		}
	}
	return checkDistribution("size", d.SizeProbabilities)
}

// checkDistribution accepts an absent distribution; a present one must have
// in-range values summing to about 1.
func checkDistribution(name string, probs map[string]float64) error {
	if len(probs) == 0 {
		return nil
	}
	sum := 0.0
	for k, p := range probs {
		if !validProbability(p) {
			return invalidf("%s probability %v for %q out of range", name, p, k)
		}
		sum += p
	}
	if math.Abs(sum-1) > probabilityTolerance {
		return invalidf("%s probabilities sum to %v", name, sum)
	}
	return nil
}

func validProbability(p float64) bool {
	return !math.IsNaN(p) && p >= 0 && p <= 1
}

func invalidf(format string, args ...any) error {
	return &domain.DecideError{Reason: domain.ReasonInvalidAnswer, Err: fmt.Errorf("decide: "+format, args...)}
}
