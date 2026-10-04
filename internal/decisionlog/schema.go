package decisionlog

// SchemaVersion is the version of the SQLite schema, stored in
// PRAGMA user_version. Any change to a table must bump it, and a log written
// at an older version must open: init migrates it forward in one transaction.
//
//	1  runs, cycles, order_events, equity
//	2  adds shadow_signals and shadow_outcomes (issue #60)
const SchemaVersion = 2

// The log is append-only: rows are inserted and never changed. The triggers
// make that a property of the database rather than of the code that uses it.
// Times are UTC nanoseconds since the Unix epoch, and 0 means unset.
const schema = `
CREATE TABLE runs (
	run_id     TEXT PRIMARY KEY,
	mode       TEXT NOT NULL,
	decider    TEXT NOT NULL,
	source     TEXT NOT NULL,
	note       TEXT NOT NULL DEFAULT '',
	started_at INTEGER NOT NULL
);

-- One row per decision cycle or risk-driven exit, written before any order
-- is submitted.
CREATE TABLE cycles (
	seq            INTEGER PRIMARY KEY AUTOINCREMENT,
	cycle_id       TEXT NOT NULL,            -- unique within a run; a risk exit's id repeats across replay runs
	run_id         TEXT NOT NULL REFERENCES runs(run_id),
	source         TEXT NOT NULL,            -- 'jev' (a decider's answer) or 'risk' (a risk-driven exit)
	mode           TEXT NOT NULL,
	token          INTEGER NOT NULL,
	exchange       TEXT NOT NULL,
	symbol         TEXT NOT NULL,
	alias          TEXT NOT NULL,            -- the opaque per-run alias Jev saw
	bar_close      INTEGER NOT NULL,         -- close time of the triggering bar, or the exit time
	price          REAL NOT NULL,            -- close of the triggering bar, or the exit's reference price
	applied_at     INTEGER NOT NULL,         -- time the answer was judged: bar_close plus latency
	schema_version INTEGER NOT NULL DEFAULT 0,
	snapshot       TEXT,                     -- the encoded snapshot sent to the decider
	snapshot_sha   TEXT,
	model          TEXT NOT NULL DEFAULT '',
	latency_ns     INTEGER NOT NULL DEFAULT 0,
	action         TEXT NOT NULL DEFAULT '',
	hold_reason    TEXT NOT NULL DEFAULT '',
	answer         TEXT,                     -- the decision, as JSON, raw answer included
	exit_reason    TEXT NOT NULL DEFAULT '', -- why a risk-driven exit happened
	verdict        TEXT NOT NULL,            -- the risk gate's reason code
	verdict_detail TEXT NOT NULL DEFAULT '',
	client_id      TEXT NOT NULL DEFAULT '',
	intent         TEXT,                     -- the approved order, as JSON
	UNIQUE (run_id, cycle_id)
);
CREATE INDEX cycles_run ON cycles(run_id, seq);
CREATE INDEX cycles_sha ON cycles(run_id, snapshot_sha);

-- One row per broker update of an order, and per failed placement.
CREATE TABLE order_events (
	seq            INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id         TEXT NOT NULL REFERENCES runs(run_id),
	cycle_id       TEXT NOT NULL,
	client_id      TEXT NOT NULL,
	broker_id      TEXT NOT NULL DEFAULT '',
	token          INTEGER NOT NULL,
	exchange       TEXT NOT NULL,
	symbol         TEXT NOT NULL,
	side           TEXT NOT NULL,
	status         TEXT NOT NULL,
	filled_qty     INTEGER NOT NULL DEFAULT 0,  -- cumulative
	avg_price      REAL NOT NULL DEFAULT 0,
	costs          REAL NOT NULL DEFAULT 0,     -- cumulative
	reason         TEXT NOT NULL DEFAULT '',
	event_time     INTEGER NOT NULL,
	fill_qty       INTEGER NOT NULL DEFAULT 0,  -- newly filled by this update
	fill_price     REAL NOT NULL DEFAULT 0,
	fill_charges   REAL NOT NULL DEFAULT 0,
	FOREIGN KEY (run_id, cycle_id) REFERENCES cycles(run_id, cycle_id)
);
CREATE INDEX order_events_run ON order_events(run_id, seq);

-- The run's cumulative PnL after costs, sampled at each decision bar close.
CREATE TABLE equity (
	seq      INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id   TEXT NOT NULL REFERENCES runs(run_id),
	at       INTEGER NOT NULL,
	pnl      REAL NOT NULL,
	realized REAL NOT NULL
);
CREATE INDEX equity_run ON equity(run_id, seq);
`

// appendOnly makes every table reject UPDATE and DELETE.
const appendOnly = `
CREATE TRIGGER runs_no_update BEFORE UPDATE ON runs BEGIN SELECT RAISE(ABORT, 'decisionlog is append-only'); END;
CREATE TRIGGER runs_no_delete BEFORE DELETE ON runs BEGIN SELECT RAISE(ABORT, 'decisionlog is append-only'); END;
CREATE TRIGGER cycles_no_update BEFORE UPDATE ON cycles BEGIN SELECT RAISE(ABORT, 'decisionlog is append-only'); END;
CREATE TRIGGER cycles_no_delete BEFORE DELETE ON cycles BEGIN SELECT RAISE(ABORT, 'decisionlog is append-only'); END;
CREATE TRIGGER order_events_no_update BEFORE UPDATE ON order_events BEGIN SELECT RAISE(ABORT, 'decisionlog is append-only'); END;
CREATE TRIGGER order_events_no_delete BEFORE DELETE ON order_events BEGIN SELECT RAISE(ABORT, 'decisionlog is append-only'); END;
CREATE TRIGGER equity_no_update BEFORE UPDATE ON equity BEGIN SELECT RAISE(ABORT, 'decisionlog is append-only'); END;
CREATE TRIGGER equity_no_delete BEFORE DELETE ON equity BEGIN SELECT RAISE(ABORT, 'decisionlog is append-only'); END;
`

// shadowSchema is what version 2 adds: the shadow log (issue #60). Every
// candidate signal is a shadow_signals row whether or not anything was traded
// on it; what the market did afterwards is one shadow_outcomes row per signal
// and horizon, appended later from the recorded bars, so both tables stay
// append-only. A fresh log creates them with the rest; a version 1 log gets
// them by migration, and no existing row is touched.
const shadowSchema = `
CREATE TABLE shadow_signals (
	signal_id      INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id         TEXT NOT NULL REFERENCES runs(run_id),
	day            TEXT NOT NULL,            -- the IST trading day, YYYY-MM-DD
	kind           TEXT NOT NULL,            -- 'pick' (a picker entry) or 'decision' (a decider's answer)
	signal         TEXT NOT NULL,            -- the report's label: 'pick', 'decision:BUY', 'decision:SELL', 'decision:HOLD', 'decision:FAILED'
	token          INTEGER NOT NULL,
	exchange       TEXT NOT NULL,
	symbol         TEXT NOT NULL,
	ref_time       INTEGER NOT NULL,         -- when the signal was formed; every horizon counts from here
	ref_price      REAL NOT NULL DEFAULT 0,  -- price at ref_time when known as the signal was written, else 0 (read from the bars)
	window_end     INTEGER NOT NULL DEFAULT 0, -- end of the trading window of the day, 0 if none
	index_token    INTEGER NOT NULL DEFAULT 0, -- the index to net the return against, 0 if none
	direction      INTEGER NOT NULL,         -- +1 a bet on a rise (and a HOLD, read as a rise), -1 a bet on a fall
	cycle_id       TEXT NOT NULL DEFAULT '', -- the cycles row of a decision
	action         TEXT NOT NULL DEFAULT '',
	probability    REAL NOT NULL DEFAULT 0,
	verdict        TEXT NOT NULL DEFAULT '', -- the risk gate's reason code
	verdict_detail TEXT NOT NULL DEFAULT '',
	traded         INTEGER NOT NULL DEFAULT 0, -- the gate approved an order on it
	detail         TEXT,                     -- JSON: a pick's rank, score, signals and news scores
	UNIQUE (day, kind, token, ref_time)
);
CREATE INDEX shadow_signals_day ON shadow_signals(day, signal_id);

CREATE TABLE shadow_outcomes (
	signal_id       INTEGER NOT NULL REFERENCES shadow_signals(signal_id),
	horizon         TEXT NOT NULL,           -- '+15m', '+60m', 'window_end' or 'day_close'
	target_time     INTEGER NOT NULL,
	ref_price       REAL NOT NULL,
	price           REAL NOT NULL,           -- the close of the last bar at or before target_time
	return_pct      REAL NOT NULL,           -- the stock's move from ref_price to price, in percent
	index_pct       REAL,                    -- the signal's index over the same interval, NULL if unknown
	universe_pct    REAL,                    -- the mean move of every recorded stock of the day, NULL if none
	universe_names  INTEGER NOT NULL DEFAULT 0,
	filled_at       INTEGER NOT NULL,
	PRIMARY KEY (signal_id, horizon)
);
`

const shadowAppendOnly = `
CREATE TRIGGER shadow_signals_no_update BEFORE UPDATE ON shadow_signals BEGIN SELECT RAISE(ABORT, 'decisionlog is append-only'); END;
CREATE TRIGGER shadow_signals_no_delete BEFORE DELETE ON shadow_signals BEGIN SELECT RAISE(ABORT, 'decisionlog is append-only'); END;
CREATE TRIGGER shadow_outcomes_no_update BEFORE UPDATE ON shadow_outcomes BEGIN SELECT RAISE(ABORT, 'decisionlog is append-only'); END;
CREATE TRIGGER shadow_outcomes_no_delete BEFORE DELETE ON shadow_outcomes BEGIN SELECT RAISE(ABORT, 'decisionlog is append-only'); END;
`
