// Package decisionlog is the append-only record of every decision cycle and
// its order events (issue #10), kept in SQLite in WAL mode through the
// pure-Go modernc.org/sqlite driver.
//
// One cycles row is written per decision cycle or risk-driven exit, before any
// order is submitted: the snapshot sent, the decider's answer, the risk
// verdict and the intent. One order_events row follows for each broker update.
// The log is also an input: RecordedDecider answers a replay from the answers
// stored in an earlier run, so everything except the model can be re-tested.
package decisionlog

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/sanjeev2040/jev-trader/internal/domain"
)

// Cycle sources, matching risk.Source: a decider's answer, or a risk-driven
// exit.
const (
	SourceDecision = "jev"
	SourceRisk     = "risk"
)

// Errors returned by the log.
var (
	ErrIntentNotLogged  = errors.New("decisionlog: intent has no cycle row; log the cycle before submitting its order")
	ErrNoRecordedAnswer = errors.New("decisionlog: no recorded answer for this snapshot")
	ErrSchemaVersion    = errors.New("decisionlog: database has an unsupported schema version")
)

// Options configure Open.
type Options struct {
	// Synchronous is SQLite's synchronous pragma: "FULL" (the default) makes
	// each write durable before it returns, which a live run needs so a
	// crash can be reconciled; "NORMAL" is faster and enough for a replay.
	Synchronous string
}

// Log is an open decision log. It is safe for concurrent use, but it holds a
// single connection, so calls are serialized.
type Log struct {
	db *sql.DB
}

// Open opens or creates the log at path. ":memory:" opens a private in-memory
// log.
func Open(path string, opts Options) (*Log, error) {
	sync := opts.Synchronous
	if sync == "" {
		sync = "FULL"
	}
	switch sync {
	case "FULL", "NORMAL":
	default:
		return nil, fmt.Errorf("decisionlog: synchronous %q is not FULL or NORMAL", sync)
	}
	dsn := ":memory:"
	if path != ":memory:" {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("decisionlog: %w", err)
		}
		q := url.Values{}
		q.Add("_pragma", "journal_mode(WAL)")
		q.Add("_pragma", "synchronous("+sync+")")
		q.Add("_pragma", "busy_timeout(5000)")
		q.Add("_pragma", "foreign_keys(1)")
		dsn = (&url.URL{Scheme: "file", Path: filepath.ToSlash(abs), RawQuery: q.Encode()}).String()
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("decisionlog: %w", err)
	}
	// One connection: an in-memory database lives and dies with it, and the
	// engine is the only writer.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	l := &Log{db: db}
	if err := l.init(context.Background(), path == ":memory:"); err != nil {
		db.Close()
		return nil, err
	}
	return l, nil
}

func (l *Log) init(ctx context.Context, memory bool) error {
	if memory {
		if _, err := l.db.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
			return fmt.Errorf("decisionlog: %w", err)
		}
	}
	var version int
	if err := l.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("decisionlog: %w", err)
	}
	switch version {
	case SchemaVersion:
		return nil
	case 0:
	default:
		return fmt.Errorf("%w: %d, want %d", ErrSchemaVersion, version, SchemaVersion)
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("decisionlog: %w", err)
	}
	defer tx.Rollback()
	for _, stmt := range []string{schema, appendOnly, fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion)} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("decisionlog: create schema: %w", err)
		}
	}
	return tx.Commit()
}

// Close closes the log.
func (l *Log) Close() error { return l.db.Close() }

// Run describes one engine run.
type Run struct {
	ID        string
	Mode      string // paper, live or replay
	Decider   string // jev, recorded, hold, momentum, ...
	Source    string // where market data came from: kite, ticks, bars, bars_backfill
	Note      string
	StartedAt time.Time
}

// StartRun records a run. Every cycle and order event belongs to one.
func (l *Log) StartRun(ctx context.Context, r Run) error {
	if r.ID == "" {
		return errors.New("decisionlog: run needs an id")
	}
	_, err := l.db.ExecContext(ctx,
		`INSERT INTO runs (run_id, mode, decider, source, note, started_at) VALUES (?, ?, ?, ?, ?, ?)`,
		r.ID, r.Mode, r.Decider, r.Source, r.Note, nanos(r.StartedAt))
	if err != nil {
		return fmt.Errorf("decisionlog: start run %s: %w", r.ID, err)
	}
	return nil
}

// Runs returns every run, oldest first.
func (l *Log) Runs(ctx context.Context) ([]Run, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT run_id, mode, decider, source, note, started_at FROM runs ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("decisionlog: %w", err)
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var started int64
		if err := rows.Scan(&r.ID, &r.Mode, &r.Decider, &r.Source, &r.Note, &started); err != nil {
			return nil, fmt.Errorf("decisionlog: %w", err)
		}
		r.StartedAt = fromNanos(started)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Cycle is one decision cycle or risk-driven exit.
type Cycle struct {
	Seq        int64 // set when read back
	CycleID    string
	RunID      string
	Source     string // SourceDecision or SourceRisk
	Mode       string
	Instrument domain.Instrument
	Alias      string
	BarClose   time.Time // close of the triggering bar, or the exit time
	// Price is the close of the triggering bar, or the reference price of a
	// risk-driven exit.
	Price     float64
	AppliedAt time.Time // when the answer was judged: BarClose plus latency
	// SchemaVersion and Snapshot are what the decider saw; unset for a
	// risk-driven exit.
	SchemaVersion int
	Snapshot      json.RawMessage
	// Decision is the decider's answer, including a HOLD substituted for a
	// failure. Nil for a risk-driven exit.
	Decision   *domain.Decision
	ExitReason string // why a risk-driven exit happened
	// Verdict is the risk gate's reason code and VerdictDetail its context.
	Verdict       string
	VerdictDetail string
	// Intent is the order the gate approved, if any, and ClientID its
	// deterministic client id.
	Intent   *domain.OrderIntent
	ClientID string
}

// SnapshotHash identifies a snapshot's encoded bytes.
func SnapshotHash(snapshot []byte) string {
	sum := sha256.Sum256(snapshot)
	return hex.EncodeToString(sum[:])
}

// AppendCycle writes one cycle. It must be called, and return, before the
// cycle's order is submitted.
func (l *Log) AppendCycle(ctx context.Context, c Cycle) error {
	if c.CycleID == "" || c.RunID == "" {
		return errors.New("decisionlog: cycle needs a cycle id and a run id")
	}
	var snap, sha, answer, intent sql.NullString
	var model, action, holdReason string
	var latency int64
	if len(c.Snapshot) > 0 {
		snap = sql.NullString{String: string(c.Snapshot), Valid: true}
		sha = sql.NullString{String: SnapshotHash(c.Snapshot), Valid: true}
	}
	if d := c.Decision; d != nil {
		b, err := json.Marshal(encodeAnswer(*d))
		if err != nil {
			return fmt.Errorf("decisionlog: encode answer: %w", err)
		}
		answer = sql.NullString{String: string(b), Valid: true}
		model, action, holdReason, latency = d.Model, string(d.Action), d.HoldReason, int64(d.Latency)
	}
	if c.Intent != nil {
		b, err := json.Marshal(c.Intent)
		if err != nil {
			return fmt.Errorf("decisionlog: encode intent: %w", err)
		}
		intent = sql.NullString{String: string(b), Valid: true}
	}
	_, err := l.db.ExecContext(ctx, `INSERT INTO cycles (
		cycle_id, run_id, source, mode, token, exchange, symbol, alias, bar_close, price, applied_at,
		schema_version, snapshot, snapshot_sha, model, latency_ns, action, hold_reason, answer,
		exit_reason, verdict, verdict_detail, client_id, intent
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.CycleID, c.RunID, c.Source, c.Mode, c.Instrument.Token, c.Instrument.Exchange, c.Instrument.Symbol, c.Alias,
		nanos(c.BarClose), c.Price, nanos(c.AppliedAt),
		c.SchemaVersion, snap, sha, model, latency, action, holdReason, answer,
		c.ExitReason, c.Verdict, c.VerdictDetail, c.ClientID, intent)
	if err != nil {
		return fmt.Errorf("decisionlog: append cycle %s: %w", c.CycleID, err)
	}
	return nil
}

const cycleColumns = `seq, cycle_id, run_id, source, mode, token, exchange, symbol, alias, bar_close, price, applied_at,
	schema_version, snapshot, answer, exit_reason, verdict, verdict_detail, client_id, intent`

func scanCycle(rows *sql.Rows) (Cycle, error) {
	var c Cycle
	var token, barClose, applied int64
	var snap, answer, intent sql.NullString
	if err := rows.Scan(&c.Seq, &c.CycleID, &c.RunID, &c.Source, &c.Mode, &token, &c.Instrument.Exchange, &c.Instrument.Symbol,
		&c.Alias, &barClose, &c.Price, &applied, &c.SchemaVersion, &snap, &answer, &c.ExitReason,
		&c.Verdict, &c.VerdictDetail, &c.ClientID, &intent); err != nil {
		return Cycle{}, fmt.Errorf("decisionlog: %w", err)
	}
	c.Instrument.Token = uint32(token)
	c.BarClose, c.AppliedAt = fromNanos(barClose), fromNanos(applied)
	if snap.Valid {
		c.Snapshot = json.RawMessage(snap.String)
	}
	if answer.Valid {
		var a answerJSON
		if err := json.Unmarshal([]byte(answer.String), &a); err != nil {
			return Cycle{}, fmt.Errorf("decisionlog: cycle %s answer: %w", c.CycleID, err)
		}
		d := a.decode()
		c.Decision = &d
	}
	if intent.Valid {
		var in domain.OrderIntent
		if err := json.Unmarshal([]byte(intent.String), &in); err != nil {
			return Cycle{}, fmt.Errorf("decisionlog: cycle %s intent: %w", c.CycleID, err)
		}
		c.Intent = &in
	}
	return c, nil
}

// Cycles returns a run's cycles in the order they were written.
func (l *Log) Cycles(ctx context.Context, runID string) ([]Cycle, error) {
	return l.queryCycles(ctx, `SELECT `+cycleColumns+` FROM cycles WHERE run_id = ? ORDER BY seq`, runID)
}

func (l *Log) queryCycles(ctx context.Context, query string, args ...any) ([]Cycle, error) {
	rows, err := l.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("decisionlog: %w", err)
	}
	defer rows.Close()
	var out []Cycle
	for rows.Next() {
		c, err := scanCycle(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// IntentsSince returns the order intents written by runs of mode since the
// given time, oldest first. The order manager reconciles them against the
// broker's order book on restart.
func (l *Log) IntentsSince(ctx context.Context, mode string, since time.Time) ([]domain.OrderIntent, error) {
	cycles, err := l.queryCycles(ctx,
		`SELECT `+cycleColumns+` FROM cycles WHERE mode = ? AND intent IS NOT NULL AND applied_at >= ? ORDER BY seq`,
		mode, nanos(since))
	if err != nil {
		return nil, err
	}
	out := make([]domain.OrderIntent, 0, len(cycles))
	for _, c := range cycles {
		out = append(out, *c.Intent)
	}
	return out, nil
}

// Journal is a run's order journal (orders.Journal): the log, scoped to one
// run.
type Journal struct {
	log   *Log
	runID string
}

// Journal returns the order journal of a run.
func (l *Log) Journal(runID string) *Journal { return &Journal{log: l, runID: runID} }

// RecordIntent does not write the intent: the cycle row already holds it. It
// refuses an intent whose cycle row is missing or differs, so an order can
// never be placed ahead of its log entry.
func (j *Journal) RecordIntent(ctx context.Context, intent domain.OrderIntent) error {
	var raw sql.NullString
	err := j.log.db.QueryRowContext(ctx, `SELECT intent FROM cycles WHERE run_id = ? AND cycle_id = ?`, j.runID, intent.CycleID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !raw.Valid) {
		return fmt.Errorf("%w: %s", ErrIntentNotLogged, intent.CycleID)
	}
	if err != nil {
		return fmt.Errorf("decisionlog: %w", err)
	}
	var logged domain.OrderIntent
	if err := json.Unmarshal([]byte(raw.String), &logged); err != nil {
		return fmt.Errorf("decisionlog: cycle %s intent: %w", intent.CycleID, err)
	}
	if logged != intent {
		return fmt.Errorf("%w: %s was logged as %+v, not %+v", ErrIntentNotLogged, intent.CycleID, logged, intent)
	}
	return nil
}

// OrderEvent is one broker update of an order, or a failed placement.
type OrderEvent struct {
	Seq        int64 // set when read back
	RunID      string
	CycleID    string
	ClientID   string
	BrokerID   string
	Instrument domain.Instrument
	Side       domain.Action
	// Status is a domain.OrderStatus, or PLACE_FAILED when the broker could
	// not be reached or refused the request.
	Status string
	// FilledQuantity, AveragePrice and Costs are cumulative for the order.
	FilledQuantity int64
	AveragePrice   float64
	Costs          float64
	Reason         string
	Time           time.Time
	// Fill is the execution this update newly reports, if any.
	Fill *domain.Fill
}

// StatusPlaceFailed marks an order event for a placement that failed.
const StatusPlaceFailed = "PLACE_FAILED"

// AppendOrderEvent writes one order event.
func (l *Log) AppendOrderEvent(ctx context.Context, e OrderEvent) error {
	var fillQty int64
	var fillPrice, fillCharges float64
	if e.Fill != nil {
		fillQty, fillPrice, fillCharges = e.Fill.Quantity, e.Fill.Price, e.Fill.Charges
	}
	_, err := l.db.ExecContext(ctx, `INSERT INTO order_events (
		run_id, cycle_id, client_id, broker_id, token, exchange, symbol, side, status,
		filled_qty, avg_price, costs, reason, event_time, fill_qty, fill_price, fill_charges
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.RunID, e.CycleID, e.ClientID, e.BrokerID, e.Instrument.Token, e.Instrument.Exchange, e.Instrument.Symbol,
		string(e.Side), e.Status, e.FilledQuantity, e.AveragePrice, e.Costs, e.Reason, nanos(e.Time),
		fillQty, fillPrice, fillCharges)
	if err != nil {
		return fmt.Errorf("decisionlog: append order event %s: %w", e.ClientID, err)
	}
	return nil
}

// OrderEvents returns a run's order events in the order they were written.
func (l *Log) OrderEvents(ctx context.Context, runID string) ([]OrderEvent, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT seq, run_id, cycle_id, client_id, broker_id, token, exchange, symbol, side, status,
		filled_qty, avg_price, costs, reason, event_time, fill_qty, fill_price, fill_charges
		FROM order_events WHERE run_id = ? ORDER BY seq`, runID)
	if err != nil {
		return nil, fmt.Errorf("decisionlog: %w", err)
	}
	defer rows.Close()
	var out []OrderEvent
	for rows.Next() {
		var e OrderEvent
		var token, at, fillQty int64
		var side string
		var fillPrice, fillCharges float64
		if err := rows.Scan(&e.Seq, &e.RunID, &e.CycleID, &e.ClientID, &e.BrokerID, &token, &e.Instrument.Exchange, &e.Instrument.Symbol,
			&side, &e.Status, &e.FilledQuantity, &e.AveragePrice, &e.Costs, &e.Reason, &at, &fillQty, &fillPrice, &fillCharges); err != nil {
			return nil, fmt.Errorf("decisionlog: %w", err)
		}
		e.Instrument.Token = uint32(token)
		e.Side = domain.Action(side)
		e.Time = fromNanos(at)
		if fillQty > 0 {
			e.Fill = &domain.Fill{ClientID: e.ClientID, Instrument: e.Instrument, Side: e.Side,
				Quantity: fillQty, Price: fillPrice, Charges: fillCharges, Time: e.Time}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// EquityPoint is the run's cumulative PnL after costs at one time.
type EquityPoint struct {
	Time     time.Time
	PnL      float64 // realized after costs plus unrealized at the last marks
	Realized float64 // realized after costs
}

// AppendEquity records an equity point.
func (l *Log) AppendEquity(ctx context.Context, runID string, p EquityPoint) error {
	_, err := l.db.ExecContext(ctx, `INSERT INTO equity (run_id, at, pnl, realized) VALUES (?, ?, ?, ?)`,
		runID, nanos(p.Time), p.PnL, p.Realized)
	if err != nil {
		return fmt.Errorf("decisionlog: append equity: %w", err)
	}
	return nil
}

// Equity returns a run's equity points in the order they were written.
func (l *Log) Equity(ctx context.Context, runID string) ([]EquityPoint, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT at, pnl, realized FROM equity WHERE run_id = ? ORDER BY seq`, runID)
	if err != nil {
		return nil, fmt.Errorf("decisionlog: %w", err)
	}
	defer rows.Close()
	var out []EquityPoint
	for rows.Next() {
		var p EquityPoint
		var at int64
		if err := rows.Scan(&at, &p.PnL, &p.Realized); err != nil {
			return nil, fmt.Errorf("decisionlog: %w", err)
		}
		p.Time = fromNanos(at)
		out = append(out, p)
	}
	return out, rows.Err()
}

func nanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func fromNanos(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

// answerJSON is the stored form of a domain.Decision. The raw answer is kept
// as a string so its bytes survive exactly.
type answerJSON struct {
	Action            domain.Action             `json:"action"`
	Size              string                    `json:"size,omitempty"`
	Probability       float64                   `json:"probability"`
	Probabilities     map[domain.Action]float64 `json:"probabilities,omitempty"`
	SizeProbabilities map[string]float64        `json:"size_probabilities,omitempty"`
	Confidence        float64                   `json:"confidence"`
	Model             string                    `json:"model,omitempty"`
	InputTokens       int                       `json:"input_tokens,omitempty"`
	OutputTokens      int                       `json:"output_tokens,omitempty"`
	LatencyNS         int64                     `json:"latency_ns"`
	HoldReason        string                    `json:"hold_reason,omitempty"`
	Raw               string                    `json:"raw,omitempty"`
}

func encodeAnswer(d domain.Decision) answerJSON {
	return answerJSON{
		Action: d.Action, Size: d.Size, Probability: d.Probability,
		Probabilities: d.Probabilities, SizeProbabilities: d.SizeProbabilities,
		Confidence: d.Confidence, Model: d.Model, InputTokens: d.InputTokens, OutputTokens: d.OutputTokens,
		LatencyNS: int64(d.Latency), HoldReason: d.HoldReason, Raw: string(d.Raw),
	}
}

func (a answerJSON) decode() domain.Decision {
	d := domain.Decision{
		Action: a.Action, Size: a.Size, Probability: a.Probability,
		Probabilities: a.Probabilities, SizeProbabilities: a.SizeProbabilities,
		Confidence: a.Confidence, Model: a.Model, InputTokens: a.InputTokens, OutputTokens: a.OutputTokens,
		Latency: time.Duration(a.LatencyNS), HoldReason: a.HoldReason,
	}
	if a.Raw != "" {
		d.Raw = json.RawMessage(a.Raw)
	}
	if d.Probabilities == nil {
		d.Probabilities = map[domain.Action]float64{}
	}
	return d
}
