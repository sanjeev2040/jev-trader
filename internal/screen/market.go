package screen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Candle is one daily candle of an NSE equity. Date is the IST day.
type Candle struct {
	Date   string  `json:"date"`
	Open   float64 `json:"open"`
	High   float64 `json:"high"`
	Low    float64 `json:"low"`
	Close  float64 `json:"close"`
	Volume int64   `json:"volume"`
}

// Quote is the part of a Kite quote the screen reads.
type Quote struct {
	Last float64
	// PrevClose is Kite's ohlc.close: the previous session's close.
	PrevClose float64
	Volume    int64
}

// Market is the Kite data the screen reads. The Kite implementation is in
// kite.go; tests use a fake.
type Market interface {
	// Daily returns the daily candles of an EXCHANGE:SYMBOL key between from
	// and to, oldest first.
	Daily(ctx context.Context, key string, from, to time.Time) ([]Candle, error)
	// Quotes returns quotes by key. A key Kite does not know is absent.
	Quotes(ctx context.Context, keys []string) (map[string]Quote, error)
}

// ErrAbort wraps an error that makes the remaining requests pointless, such as
// an expired Kite session; Market.Daily returns it so the screen stops at
// once instead of failing the same way for every stock.
var ErrAbort = errors.New("abort")

// candleCache keeps the day's daily candles on disk, so the second pass (and a
// rerun) does not fetch them again. It is keyed by the screen's date, because
// the candles it holds end the session before.
type candleCache struct {
	path    string
	candles map[string][]Candle
	dirty   bool
}

func openCandleCache(dir, date string) (*candleCache, error) {
	c := &candleCache{path: filepath.Join(dir, "screen", "candles-"+date+".json"), candles: map[string][]Candle{}}
	body, err := os.ReadFile(c.path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(body, &c.candles); err != nil {
		// A damaged cache is only a cache.
		c.candles = map[string][]Candle{}
	}
	return c, nil
}

func (c *candleCache) get(key string) ([]Candle, bool) {
	v, ok := c.candles[key]
	return v, ok
}

func (c *candleCache) put(key string, v []Candle) {
	c.candles[key] = v
	c.dirty = true
}

func (c *candleCache) save() error {
	if !c.dirty {
		return nil
	}
	body, err := json.Marshal(c.candles)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(c.path), ".candles-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	c.dirty = false
	return os.Rename(tmp.Name(), c.path)
}

// stats are what the screen derives from a stock's daily candles before the
// trading day.
type stats struct {
	PrevClose float64
	// PrevReturn is the previous session's close-to-close return, percent.
	PrevReturn float64
	// PrevValue is the previous session's traded value, rupees (volume x
	// typical price).
	PrevValue float64
	// ADV is the mean daily volume over the last advSessions sessions.
	ADV float64
	// RangePct is the mean (high-low)/close over the last rangeSessions
	// sessions, percent: how far the stock typically travels in a day.
	RangePct float64
	Sessions int
	LastDate string
}

const (
	advSessions   = 20
	rangeSessions = 10
	// maxStaleDays is how old the newest candle may be: a long weekend plus a
	// holiday is 5 days; a stock with nothing for longer is suspended.
	maxStaleDays = 7
)

// computeStats derives stats from candles, using only those before date (the
// trading day, YYYY-MM-DD), so that a rerun after the open does not see the
// day's own candle. It reports why a stock cannot be used when it cannot.
func computeStats(candles []Candle, date string, minSessions int) (stats, string) {
	var hist []Candle
	for _, c := range candles {
		if c.Date < date && c.Close > 0 {
			hist = append(hist, c)
		}
	}
	sort.Slice(hist, func(i, j int) bool { return hist[i].Date < hist[j].Date })
	if len(hist) < minSessions {
		return stats{}, fmt.Sprintf("fewer than %d sessions of history", minSessions)
	}
	last := hist[len(hist)-1]
	prev := hist[len(hist)-2]
	day, err1 := time.Parse(time.DateOnly, date)
	lastDay, err2 := time.Parse(time.DateOnly, last.Date)
	if err1 != nil || err2 != nil || day.Sub(lastDay) > maxStaleDays*24*time.Hour {
		return stats{}, "no recent trading (suspended?)"
	}
	s := stats{
		PrevClose:  last.Close,
		PrevReturn: (last.Close/prev.Close - 1) * 100,
		PrevValue:  float64(last.Volume) * (last.High + last.Low + last.Close) / 3,
		Sessions:   len(hist),
		LastDate:   last.Date,
	}
	tail := hist[max(0, len(hist)-advSessions):]
	for _, c := range tail {
		s.ADV += float64(c.Volume)
	}
	s.ADV /= float64(len(tail))
	tail = hist[max(0, len(hist)-rangeSessions):]
	for _, c := range tail {
		s.RangePct += (c.High - c.Low) / c.Close * 100
	}
	s.RangePct /= float64(len(tail))
	return s, ""
}

// liquidity says why a stock is not liquid enough, or "".
func (f Filters) liquidity(s stats) string {
	switch {
	case s.PrevClose < f.MinPrice:
		return fmt.Sprintf("price %.2f below %.0f", s.PrevClose, f.MinPrice)
	case s.PrevClose > f.MaxPrice:
		return fmt.Sprintf("price %.2f above %.0f", s.PrevClose, f.MaxPrice)
	case s.PrevValue < f.MinPrevValue:
		return fmt.Sprintf("previous-day value ₹%.1f cr below ₹%.1f cr", s.PrevValue/1e7, f.MinPrevValue/1e7)
	case s.ADV < f.MinADV:
		return fmt.Sprintf("average volume %.0f below %.0f", s.ADV, f.MinADV)
	}
	return ""
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	if n := len(s); n%2 == 1 {
		return s[n/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

func clamp(x, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, x)) }
