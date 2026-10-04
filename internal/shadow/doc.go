// Package shadow turns the decision log's shadow signals (issue #60) into
// evidence. The engine logs every candidate signal of a run, traded or not:
// the picker's entries and every decider answer with the risk gate's verdict.
// This package fills in what the market did afterwards, at fixed horizons,
// from the recorded bars (Fill), and summarises it per signal as the
// market-neutral return with a day-resampled confidence interval and a hit
// rate (BuildReport). Nothing here calls Kite or Jev, and nothing here is on
// the trading path.
package shadow
