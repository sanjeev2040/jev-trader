package decisionlog

// SchemaVersion is the version of the SQLite schema, stored in
// PRAGMA user_version. Any change to a table must bump it.
const SchemaVersion = 1

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
