package study

import (
	"github.com/sanjeev2040/jev-trader/internal/replay"
)

// DayResult is what one studied day came to; it is what the study stores per
// day, and what Summarize aggregates.
type DayResult struct {
	Version int      `json:"version"`
	Date    string   `json:"date"`
	Status  string   `json:"status"`
	Reason  string   `json:"reason,omitempty"` // why a skipped day was skipped
	Phase   string   `json:"phase,omitempty"`
	Notes   []string `json:"notes,omitempty"` // the screen's notes for the day

	Picks  ListResult  `json:"picks"`
	Random *ListResult `json:"random,omitempty"`
	// Stocks is every studied stock of the day, picked or random, with the
	// signals the screen gave it and what it did over the window.
	Stocks []StockDay `json:"stocks,omitempty"`

	// Jev is what the replays' Jev decider cost; ScreenInputTokens and
	// ScreenOutputTokens what scoring the day's news cost.
	Jev                Usage `json:"jev"`
	ScreenInputTokens  int   `json:"screen_input_tokens,omitempty"`
	ScreenOutputTokens int   `json:"screen_output_tokens,omitempty"`
}

// ListResult is the replays of one list of stocks on one day.
type ListResult struct {
	Names   int         `json:"names"`
	Missing []string    `json:"missing,omitempty"` // names with no instrument or no candles
	BuyHold BuyHold     `json:"buy_hold"`
	Runs    []RunResult `json:"runs"`
}

// BuyHold is buying every name of the list at the window's open and holding it
// to the window's end.
type BuyHold struct {
	Net           float64 `json:"net"`
	Gross         float64 `json:"gross"`
	Costs         float64 `json:"costs"`
	NamesWithData int     `json:"names_with_data"`
	NamesUp       int     `json:"names_up"`        // names whose price rose over the window
	MeanReturnPct float64 `json:"mean_return_pct"` // mean over names, before costs
}

// RunResult is one decider's run over a list.
type RunResult struct {
	Decider  string  `json:"decider"`
	NetPnL   float64 `json:"net_pnl"`
	GrossPnL float64 `json:"gross_pnl"`
	Costs    float64 `json:"costs"`
	Buys     int     `json:"buys"`
	Sells    int     `json:"sells"`
	Holds    int     `json:"holds"`
	// HoldSubstitutions are the HOLDs that stand in for a decider failure
	// (timeout, rate limit, invalid answer); a result with many is not the
	// decider's.
	HoldSubstitutions int     `json:"hold_substitutions,omitempty"`
	Trades            int     `json:"trades"` // closed round trips
	Wins              int     `json:"wins"`
	MaxDrawdown       float64 `json:"max_drawdown"`
	Turnover          float64 `json:"turnover"`

	Calibration replay.Calibration `json:"calibration"`
	// Calls scores the run's BUY and SELL answers against the price after.
	Calls []replay.CallSummary `json:"calls,omitempty"`
}

// StockDay is one stock on one day.
type StockDay struct {
	Key    string `json:"key"`
	Sector string `json:"sector,omitempty"`
	Picked bool   `json:"picked"`
	Rank   int    `json:"rank,omitempty"` // 0 for a random stock
	// Score and Signals are the screen's, as of that morning. A signal the
	// screen could not read for the stock is absent.
	Score   float64            `json:"score"`
	Signals map[string]float64 `json:"signals,omitempty"`
	// ReturnPct is the move from the open of the window's first bar to the
	// close of its last; DayReturnPct runs to the day's close.
	ReturnPct    float64  `json:"return_pct"`
	DayReturnPct *float64 `json:"day_return_pct,omitempty"`
}

func (r *DayResult) setList(name string, l ListResult) {
	if name == ListRandom {
		r.Random = &l
		return
	}
	r.Picks = l
}

// fill takes a replay report's results into the list.
func (l *ListResult) fill(rep *replay.Report) {
	for _, m := range rep.Runs {
		run := RunResult{
			Decider: m.Decider, NetPnL: m.NetPnL, GrossPnL: m.GrossPnL, Costs: m.Costs,
			Buys: m.Buys, Sells: m.Sells, Holds: m.Holds, HoldSubstitutions: m.HoldSubstitutions, Trades: m.Trades, Wins: m.Wins,
			MaxDrawdown: m.MaxDrawdown, Turnover: m.Turnover, Calibration: m.Calibration,
		}
		if rep.Comparison != nil {
			for _, s := range rep.Comparison.Summary {
				if s.RunID == m.RunID {
					run.Calls = append(run.Calls, s)
				}
			}
		}
		l.Runs = append(l.Runs, run)
	}
	if rep.Comparison == nil {
		return
	}
	bh := rep.Comparison.BuyHold
	l.BuyHold = BuyHold{Net: bh.Net, Gross: bh.Gross, Costs: bh.Costs, NamesWithData: len(bh.Rows)}
	sum := 0.0
	for _, row := range bh.Rows {
		sum += row.ReturnPct
		if row.ReturnPct > 0 {
			l.BuyHold.NamesUp++
		}
	}
	if n := len(bh.Rows); n > 0 {
		l.BuyHold.MeanReturnPct = sum / float64(n)
	}
}

// Run returns the list's run of the decider, if it has one.
func (l ListResult) Run(decider string) (RunResult, bool) {
	for _, r := range l.Runs {
		if r.Decider == decider {
			return r, true
		}
	}
	return RunResult{}, false
}
