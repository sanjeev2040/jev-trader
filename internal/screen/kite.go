package screen

import (
	"context"
	"errors"
	"fmt"
	"time"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"
	"golang.org/x/time/rate"

	"github.com/sanjeev2040/jev-trader/internal/watchlist"
)

// Kite's limits: historical data 3 requests a second, quotes 1 a second and at
// most 500 instruments per quote request.
const (
	historicalPerSecond = 3
	quotePerSecond      = 1
	quoteBatch          = 500
)

// KiteAPI is the part of *kiteconnect.Client the screen uses.
type KiteAPI interface {
	GetHistoricalData(token int, interval string, from, to time.Time, continuous, oi bool) ([]kiteconnect.HistoricalData, error)
	GetQuote(instruments ...string) (kiteconnect.Quote, error)
}

var _ KiteAPI = (*kiteconnect.Client)(nil)

// Kite is the Market backed by Kite Connect.
type Kite struct {
	API KiteAPI
	// Tokens maps an EXCHANGE:SYMBOL key to its instrument token.
	Tokens map[string]uint32

	historical *rate.Limiter
	quotes     *rate.Limiter
}

// NewKite returns a Kite market with Kite's rate limits applied.
func NewKite(api KiteAPI, tokens map[string]uint32) *Kite {
	return &Kite{
		API: api, Tokens: tokens,
		historical: rate.NewLimiter(rate.Every(time.Second/historicalPerSecond), 1),
		quotes:     rate.NewLimiter(rate.Every(time.Second/quotePerSecond), 1),
	}
}

var _ Market = (*Kite)(nil)

// Daily returns the daily candles of key. An expired session or a missing
// permission wraps ErrAbort, because every other request would fail the same
// way.
func (k *Kite) Daily(ctx context.Context, key string, from, to time.Time) ([]Candle, error) {
	token, ok := k.Tokens[key]
	if !ok {
		return nil, fmt.Errorf("%s is not in the instrument master", key)
	}
	if err := k.historical.Wait(ctx); err != nil {
		return nil, err
	}
	data, err := k.API.GetHistoricalData(int(token), "day", from, to, false, false)
	if err != nil {
		return nil, classify(key, err)
	}
	out := make([]Candle, 0, len(data))
	for _, d := range data {
		if d.Volume < 0 {
			return nil, fmt.Errorf("%s: negative volume at %v", key, d.Date.Time)
		}
		out = append(out, Candle{
			Date: d.Date.Time.In(watchlist.IST).Format(time.DateOnly),
			Open: d.Open, High: d.High, Low: d.Low, Close: d.Close, Volume: int64(d.Volume),
		})
	}
	return out, nil
}

// Quotes returns quotes for the keys, in batches of 500.
func (k *Kite) Quotes(ctx context.Context, keys []string) (map[string]Quote, error) {
	out := make(map[string]Quote, len(keys))
	for start := 0; start < len(keys); start += quoteBatch {
		batch := keys[start:min(start+quoteBatch, len(keys))]
		if err := k.quotes.Wait(ctx); err != nil {
			return nil, err
		}
		got, err := k.API.GetQuote(batch...)
		if err != nil {
			return nil, classify("quotes", err)
		}
		for key, q := range got {
			out[key] = Quote{Last: q.LastPrice, PrevClose: q.OHLC.Close, Volume: int64(q.Volume)}
		}
	}
	return out, nil
}

// classify marks the errors that make further requests pointless.
func classify(what string, err error) error {
	var kerr kiteconnect.Error
	if errors.As(err, &kerr) && (kerr.ErrorType == kiteconnect.TokenError || kerr.ErrorType == kiteconnect.PermissionError) {
		return fmt.Errorf("%s: kite %s: %v: %w", what, kerr.ErrorType, kerr.Message, ErrAbort)
	}
	return fmt.Errorf("%s: %w", what, err)
}
