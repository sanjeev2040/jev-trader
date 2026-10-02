package decisionlog

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/sanjeev2040/jev-trader/internal/domain"
)

// RecordedDecider answers snapshots with the answers an earlier run stored, so
// a regression replay exercises everything except the model. It matches a
// snapshot by its encoded bytes; snapshots that repeat exactly are answered in
// the order they were recorded.
//
// A snapshot with no recorded answer (the data or the snapshot code changed)
// returns an error, which decide.HoldOnFailure turns into a HOLD. Misses
// counts them, so a replay can tell it diverged.
type RecordedDecider struct {
	mu      sync.Mutex
	answers map[string][]domain.Decision
	misses  int
}

// RecordedDecider loads the decider answers of one run.
func (l *Log) RecordedDecider(ctx context.Context, runID string) (*RecordedDecider, error) {
	cycles, err := l.queryCycles(ctx,
		`SELECT `+cycleColumns+` FROM cycles WHERE run_id = ? AND source = ? AND snapshot IS NOT NULL AND answer IS NOT NULL ORDER BY seq`,
		runID, SourceDecision)
	if err != nil {
		return nil, err
	}
	if len(cycles) == 0 {
		return nil, fmt.Errorf("decisionlog: run %q has no recorded decisions", runID)
	}
	r := &RecordedDecider{answers: make(map[string][]domain.Decision, len(cycles))}
	for _, c := range cycles {
		h := SnapshotHash(c.Snapshot)
		r.answers[h] = append(r.answers[h], *c.Decision)
	}
	return r, nil
}

// Decide returns the next recorded answer for the snapshot.
func (r *RecordedDecider) Decide(_ context.Context, state json.RawMessage) (domain.Decision, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := SnapshotHash(state)
	q := r.answers[h]
	if len(q) == 0 {
		r.misses++
		return domain.Decision{}, &domain.DecideError{Reason: domain.ReasonError, Err: ErrNoRecordedAnswer}
	}
	r.answers[h] = q[1:]
	return q[0], nil
}

// Misses returns how many snapshots had no recorded answer.
func (r *RecordedDecider) Misses() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.misses
}

// Remaining returns how many recorded answers a replay has not used.
func (r *RecordedDecider) Remaining() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, q := range r.answers {
		n += len(q)
	}
	return n
}
