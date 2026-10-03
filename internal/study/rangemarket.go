package study

import (
	"context"
	"sync"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/screen"
	"github.com/sanjeev2040/jev-trader/internal/watchlist"
)

// rangeMarket serves each screened day's daily candles from one fetch per stock
// for the whole study, instead of one per stock and day: the 45-day window of
// consecutive days overlaps almost entirely, and Kite allows three requests a
// second. A request outside the range goes to the inner market.
type rangeMarket struct {
	inner    screen.Market
	from, to string // IST days, YYYY-MM-DD, inclusive

	mu    sync.Mutex
	byKey map[string][]screen.Candle
}

// NewRangeMarket wraps inner for screens of days between first and last
// (inclusive), each of which reads lookbackDays calendar days before it.
func NewRangeMarket(inner screen.Market, first, last time.Time, lookbackDays int) screen.Market {
	return &rangeMarket{
		inner: inner, byKey: map[string][]screen.Candle{},
		from: first.AddDate(0, 0, -lookbackDays).In(watchlist.IST).Format(time.DateOnly),
		to:   last.AddDate(0, 0, -1).In(watchlist.IST).Format(time.DateOnly),
	}
}

func (m *rangeMarket) Daily(ctx context.Context, key string, from, to time.Time) ([]screen.Candle, error) {
	f, t := from.In(watchlist.IST).Format(time.DateOnly), to.In(watchlist.IST).Format(time.DateOnly)
	if f < m.from || t > m.to {
		return m.inner.Daily(ctx, key, from, to)
	}
	m.mu.Lock()
	all, ok := m.byKey[key]
	m.mu.Unlock()
	if !ok {
		first, _ := time.ParseInLocation(time.DateOnly, m.from, watchlist.IST)
		last, _ := time.ParseInLocation(time.DateOnly, m.to, watchlist.IST)
		var err error
		if all, err = m.inner.Daily(ctx, key, first, last.Add(24*time.Hour-time.Second)); err != nil {
			return nil, err
		}
		m.mu.Lock()
		m.byKey[key] = all
		m.mu.Unlock()
	}
	var out []screen.Candle
	for _, c := range all {
		if c.Date >= f && c.Date <= t {
			out = append(out, c)
		}
	}
	return out, nil
}

func (m *rangeMarket) Quotes(ctx context.Context, keys []string) (map[string]screen.Quote, error) {
	return m.inner.Quotes(ctx, keys)
}
