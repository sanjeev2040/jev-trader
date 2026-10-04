package decisionlog

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/domain"
)

// Shadow signal kinds.
const (
	ShadowPick     = "pick"     // a picker entry
	ShadowDecision = "decision" // a decider's answer
)

// ShadowSignal is one candidate signal, logged whether or not anything was
// traded on it (issue #60).
type ShadowSignal struct {
	ID         int64 // set when read back
	RunID      string
	Day        string // the IST trading day, YYYY-MM-DD
	Kind       string // ShadowPick or ShadowDecision
	Signal     string // the report's label, e.g. "pick" or "decision:BUY"
	Instrument domain.Instrument
	// RefTime is when the signal was formed; every horizon counts from it.
	RefTime time.Time
	// RefPrice is the price at RefTime when it was known as the signal was
	// written, else zero: the outcome then reads it from the recorded bars.
	RefPrice float64
	// WindowEnd is the end of the trading window of the day; zero for none.
	WindowEnd time.Time
	// IndexToken is the index the return is netted against; zero for none.
	IndexToken uint32
	// Direction is +1 for a bet on a rise and -1 for a bet on a fall. A HOLD
	// has no bet; it is read as a rise.
	Direction int
	CycleID   string // the cycles row of a decision
	Action    string
	// Probability is the decision's probability of its action.
	Probability   float64
	Verdict       string // the risk gate's reason code
	VerdictDetail string
	Traded        bool // the gate approved an order on it
	// Detail is JSON that only the signal's kind interprets: a pick's rank,
	// score, signals and news scores.
	Detail json.RawMessage
}

// AppendShadowSignal writes one shadow signal and reports whether it was new.
// A signal for the same day, kind, instrument and time is already logged by an
// earlier run of the day (a restart logs the picks again) and is ignored.
func (l *Log) AppendShadowSignal(ctx context.Context, s ShadowSignal) (bool, error) {
	if s.RunID == "" || s.Day == "" || s.Kind == "" || s.Signal == "" || s.RefTime.IsZero() {
		return false, fmt.Errorf("decisionlog: shadow signal needs a run, day, kind, signal and reference time")
	}
	var detail sql.NullString
	if len(s.Detail) > 0 {
		detail = sql.NullString{String: string(s.Detail), Valid: true}
	}
	res, err := l.db.ExecContext(ctx, `INSERT OR IGNORE INTO shadow_signals (
		run_id, day, kind, signal, token, exchange, symbol, ref_time, ref_price, window_end, index_token, direction,
		cycle_id, action, probability, verdict, verdict_detail, traded, detail
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.RunID, s.Day, s.Kind, s.Signal, s.Instrument.Token, s.Instrument.Exchange, s.Instrument.Symbol,
		nanos(s.RefTime), s.RefPrice, nanos(s.WindowEnd), s.IndexToken, s.Direction,
		s.CycleID, s.Action, s.Probability, s.Verdict, s.VerdictDetail, s.Traded, detail)
	if err != nil {
		return false, fmt.Errorf("decisionlog: append shadow signal %s %s: %w", s.Signal, s.Instrument.Symbol, err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ShadowSignals returns the signals of the IST days from..to, both inclusive
// (YYYY-MM-DD), in the order they were written.
func (l *Log) ShadowSignals(ctx context.Context, from, to string) ([]ShadowSignal, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT signal_id, run_id, day, kind, signal, token, exchange, symbol, ref_time, ref_price,
		window_end, index_token, direction, cycle_id, action, probability, verdict, verdict_detail, traded, detail
		FROM shadow_signals WHERE day >= ? AND day <= ? ORDER BY signal_id`, from, to)
	if err != nil {
		return nil, fmt.Errorf("decisionlog: %w", err)
	}
	defer rows.Close()
	var out []ShadowSignal
	for rows.Next() {
		var s ShadowSignal
		var token, ref, window, index int64
		var detail sql.NullString
		if err := rows.Scan(&s.ID, &s.RunID, &s.Day, &s.Kind, &s.Signal, &token, &s.Instrument.Exchange, &s.Instrument.Symbol,
			&ref, &s.RefPrice, &window, &index, &s.Direction, &s.CycleID, &s.Action, &s.Probability,
			&s.Verdict, &s.VerdictDetail, &s.Traded, &detail); err != nil {
			return nil, fmt.Errorf("decisionlog: %w", err)
		}
		s.Instrument.Token, s.IndexToken = uint32(token), uint32(index)
		s.RefTime, s.WindowEnd = fromNanos(ref), fromNanos(window)
		if detail.Valid {
			s.Detail = json.RawMessage(detail.String)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ShadowOutcome is what the market did after a shadow signal, at one horizon,
// read from the recorded bars.
type ShadowOutcome struct {
	SignalID int64
	Horizon  string // "+15m", "+60m", "window_end" or "day_close"
	Target   time.Time
	RefPrice float64
	// Price is the close of the last bar at or before Target.
	Price     float64
	ReturnPct float64 // the stock's move from RefPrice to Price, in percent
	// IndexPct is the signal's index over the same interval and UniversePct
	// the mean move of every recorded stock of the day; nil when unknown.
	IndexPct, UniversePct *float64
	UniverseNames         int
	FilledAt              time.Time
}

// AppendShadowOutcome writes one outcome. An outcome already written for the
// signal and horizon is kept, so a refill is harmless.
func (l *Log) AppendShadowOutcome(ctx context.Context, o ShadowOutcome) error {
	_, err := l.db.ExecContext(ctx, `INSERT OR IGNORE INTO shadow_outcomes (
		signal_id, horizon, target_time, ref_price, price, return_pct, index_pct, universe_pct, universe_names, filled_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.SignalID, o.Horizon, nanos(o.Target), o.RefPrice, o.Price, o.ReturnPct,
		nullFloat(o.IndexPct), nullFloat(o.UniversePct), o.UniverseNames, nanos(o.FilledAt))
	if err != nil {
		return fmt.Errorf("decisionlog: append shadow outcome %d %s: %w", o.SignalID, o.Horizon, err)
	}
	return nil
}

// ShadowOutcomes returns the outcomes of the signals of the IST days
// from..to, both inclusive, by signal id and the order they were written.
func (l *Log) ShadowOutcomes(ctx context.Context, from, to string) ([]ShadowOutcome, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT o.signal_id, o.horizon, o.target_time, o.ref_price, o.price, o.return_pct,
		o.index_pct, o.universe_pct, o.universe_names, o.filled_at
		FROM shadow_outcomes o JOIN shadow_signals s ON s.signal_id = o.signal_id
		WHERE s.day >= ? AND s.day <= ? ORDER BY o.signal_id, o.rowid`, from, to)
	if err != nil {
		return nil, fmt.Errorf("decisionlog: %w", err)
	}
	defer rows.Close()
	var out []ShadowOutcome
	for rows.Next() {
		var o ShadowOutcome
		var target, filled int64
		var index, universe sql.NullFloat64
		if err := rows.Scan(&o.SignalID, &o.Horizon, &target, &o.RefPrice, &o.Price, &o.ReturnPct,
			&index, &universe, &o.UniverseNames, &filled); err != nil {
			return nil, fmt.Errorf("decisionlog: %w", err)
		}
		o.Target, o.FilledAt = fromNanos(target), fromNanos(filled)
		if index.Valid {
			o.IndexPct = &index.Float64
		}
		if universe.Valid {
			o.UniversePct = &universe.Float64
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func nullFloat(p *float64) sql.NullFloat64 {
	if p == nil {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: *p, Valid: true}
}
