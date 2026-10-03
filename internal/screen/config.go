// Package screen builds the day's watchlist before the session: it takes the
// universe of mid and small caps, drops what is not liquid enough for
// intraday, scores what is left on news, sector strength, the pre-open gap and
// recent range, and writes the top N with the reasons. Every external system
// (NSE files, Kite, news feeds, Jev) is behind an interface so the whole thing
// runs on fixtures. See reports/0004-watchlist.md.
package screen

import (
	"errors"
	"fmt"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/watchlist"
)

// Signal names, as they appear in Weights and in an Entry's Signals.
const (
	SignalNews   = "news"
	SignalSector = "sector"
	SignalGap    = "gap"
	SignalRange  = "range"
)

// Weights are the relative weights of the signals. Only their ratios matter:
// the score is divided by the sum of the weights of the signals available in
// this run.
type Weights struct {
	News, Sector, Gap, Range float64
}

// DefaultWeights favours news (the one signal about what is new today), then
// sector strength and the pre-open gap, with recent range as a small tiebreak
// that prefers stocks that move enough to trade intraday.
func DefaultWeights() Weights { return Weights{News: 0.40, Sector: 0.25, Gap: 0.25, Range: 0.10} }

func (w Weights) asMap() map[string]float64 {
	return map[string]float64{SignalNews: w.News, SignalSector: w.Sector, SignalGap: w.Gap, SignalRange: w.Range}
}

// Filters decide which universe members are liquid enough for intraday.
type Filters struct {
	// MinPrevValue is the least traded value of the previous session, in
	// rupees. Kite's daily candle has no traded value, so it is volume times
	// the typical price, (high+low+close)/3.
	MinPrevValue float64
	// MinADV is the least average daily volume, in shares, over the last
	// ADVSessions sessions.
	MinADV float64
	// MinPrice and MaxPrice bound the previous close, in rupees.
	MinPrice, MaxPrice float64
	// MinSessions is how many sessions of history a stock needs.
	MinSessions int
}

// DefaultFilters: ₹10 crore traded and 2 lakh shares a day keep a stock tradeable
// for a few lakh rupees of intraday size without moving it; the price band
// leaves out penny stocks and very high-priced ones whose lot is awkward.
func DefaultFilters() Filters {
	return Filters{MinPrevValue: 10e7, MinADV: 200_000, MinPrice: 50, MaxPrice: 5000, MinSessions: 10}
}

// Config configures a screen.
type Config struct {
	// Date is the IST trading day the list is for.
	Date time.Time
	// Now is when the screen runs; news up to Now (or the day's 09:00 for a
	// past day) is read.
	Now   time.Time
	Phase string // watchlist.PhaseEarly, PhaseFinal or PhaseHistoric
	TopN  int
	// Direction is "long" to rank by expected rise, "both" by |score|.
	Direction string
	Weights   Weights
	Filters   Filters
	// MaxPerSector caps entries from one sector so that the list is not one
	// trade; 0 means no cap.
	MaxPerSector int
	// MinScore is the least score (|score| for "both") an entry needs.
	MinScore float64
	// MinSectorMembers is how many liquid members a sector needs for a
	// sector signal.
	MinSectorMembers int
	// GapScale and SectorScale are the percent moves that map to a signal of
	// about 0.76 (tanh 1).
	GapScale, SectorScale float64

	NewsWindow        time.Duration
	NewsHalfLife      time.Duration
	MaxNewsItems      int // scored by Jev at most this many (item, symbol) pairs
	MaxSymbolsPerItem int // an item naming more companies than this is a market wrap
	NewsConcurrency   int

	// SectorIndexes maps a sector (NSE industry) to the Kite key of its index.
	SectorIndexes map[string]string
	// CacheDir holds the day's universe and candle caches.
	CacheDir string
}

// DefaultConfig returns the defaults; the caller sets Date, Now, Phase and CacheDir.
func DefaultConfig() Config {
	return Config{
		TopN: 8, Direction: "long", Weights: DefaultWeights(), Filters: DefaultFilters(),
		MaxPerSector: 3, MinScore: 0.05, MinSectorMembers: 3, GapScale: 2, SectorScale: 1.5,
		NewsWindow: 18 * time.Hour, NewsHalfLife: 24 * time.Hour, MaxNewsItems: 150,
		MaxSymbolsPerItem: 3, NewsConcurrency: 4, SectorIndexes: DefaultSectorIndexes(),
	}
}

// DefaultSectorIndexes maps NSE's industries to the Kite key of the matching
// NIFTY sectoral index. The keys follow Kite's instrument master naming; an
// index that is not found is skipped with a note, so a wrong name costs a
// signal, not the screen.
func DefaultSectorIndexes() map[string]string {
	return map[string]string{
		"Financial Services":                "NSE:NIFTY FIN SERVICE",
		"Information Technology":            "NSE:NIFTY IT",
		"Automobile and Auto Components":    "NSE:NIFTY AUTO",
		"Fast Moving Consumer Goods":        "NSE:NIFTY FMCG",
		"Healthcare":                        "NSE:NIFTY HEALTHCARE",
		"Metals & Mining":                   "NSE:NIFTY METAL",
		"Realty":                            "NSE:NIFTY REALTY",
		"Oil Gas & Consumable Fuels":        "NSE:NIFTY OIL AND GAS",
		"Media Entertainment & Publication": "NSE:NIFTY MEDIA",
		"Consumer Durables":                 "NSE:NIFTY CONSR DURBL",
		"Construction":                      "NSE:NIFTY INFRA",
	}
}

// Validate checks the config.
func (c Config) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	switch c.Phase {
	case watchlist.PhaseEarly, watchlist.PhaseFinal, watchlist.PhaseHistoric:
	default:
		bad("phase %q is not early, final or historic", c.Phase)
	}
	if c.Date.IsZero() {
		bad("date is not set")
	}
	if c.TopN < 1 {
		bad("top must be at least 1")
	}
	if c.Direction != "long" && c.Direction != "both" {
		bad("direction %q is not long or both", c.Direction)
	}
	w := c.Weights
	if w.News < 0 || w.Sector < 0 || w.Gap < 0 || w.Range < 0 || w.News+w.Sector+w.Gap+w.Range <= 0 {
		bad("weights must be non-negative and not all zero")
	}
	f := c.Filters
	if f.MinPrice < 0 || f.MaxPrice <= f.MinPrice || f.MinPrevValue < 0 || f.MinADV < 0 || f.MinSessions < 2 {
		bad("filters: need 0 <= min-price < max-price, non-negative minimums and at least 2 sessions")
	}
	if c.GapScale <= 0 || c.SectorScale <= 0 {
		bad("gap and sector scales must be positive")
	}
	if c.NewsWindow <= 0 || c.NewsHalfLife <= 0 || c.NewsConcurrency < 1 || c.MaxNewsItems < 0 || c.MaxSymbolsPerItem < 1 {
		bad("news settings must be positive")
	}
	if c.CacheDir == "" {
		bad("cache directory is not set")
	}
	return errors.Join(errs...)
}
