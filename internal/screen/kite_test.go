package screen

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"
	"github.com/zerodha/gokiteconnect/v4/models"
)

type fakeKite struct {
	histCalls  []int
	quoteCalls [][]string
	histErr    error
	quoteErr   error
}

func (f *fakeKite) GetHistoricalData(token int, interval string, from, to time.Time, continuous, oi bool) ([]kiteconnect.HistoricalData, error) {
	f.histCalls = append(f.histCalls, token)
	if interval != "day" || continuous || oi {
		return nil, fmt.Errorf("unexpected request: %s continuous=%v oi=%v", interval, continuous, oi)
	}
	if f.histErr != nil {
		return nil, f.histErr
	}
	return []kiteconnect.HistoricalData{
		{Date: models.Time{Time: time.Date(2026, 10, 1, 0, 0, 0, 0, time.FixedZone("IST", 19800))}, Open: 10, High: 12, Low: 9, Close: 11, Volume: 1000},
	}, nil
}

func (f *fakeKite) GetQuote(instruments ...string) (kiteconnect.Quote, error) {
	f.quoteCalls = append(f.quoteCalls, instruments)
	if f.quoteErr != nil {
		return nil, f.quoteErr
	}
	q := kiteconnect.Quote{}
	for _, in := range instruments {
		v := q[in]
		v.LastPrice = 105
		v.OHLC.Close = 100
		v.Volume = 7
		q[in] = v
	}
	return q, nil
}

func fastKite(api KiteAPI, tokens map[string]uint32) *Kite {
	k := NewKite(api, tokens)
	k.historical.SetLimit(1e9)
	k.historical.SetBurst(1000)
	k.quotes.SetLimit(1e9)
	k.quotes.SetBurst(1000)
	return k
}

func TestKiteDailyMapsCandles(t *testing.T) {
	api := &fakeKite{}
	k := fastKite(api, map[string]uint32{"NSE:INFY": 408065})
	got, err := k.Daily(context.Background(), "NSE:INFY", time.Now(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != (Candle{Date: "2026-10-01", Open: 10, High: 12, Low: 9, Close: 11, Volume: 1000}) {
		t.Errorf("candles = %+v", got)
	}
	if len(api.histCalls) != 1 || api.histCalls[0] != 408065 {
		t.Errorf("token = %v", api.histCalls)
	}
	if _, err := k.Daily(context.Background(), "NSE:NOPE", time.Now(), time.Now()); err == nil {
		t.Error("an instrument outside the master was fetched")
	}
}

func TestKiteDailyAbortsOnTokenAndPermissionErrors(t *testing.T) {
	for _, etype := range []string{kiteconnect.TokenError, kiteconnect.PermissionError} {
		api := &fakeKite{histErr: kiteconnect.NewError(etype, "Incorrect api_key or access_token", nil)}
		k := fastKite(api, map[string]uint32{"NSE:INFY": 1})
		_, err := k.Daily(context.Background(), "NSE:INFY", time.Now(), time.Now())
		if !errors.Is(err, ErrAbort) {
			t.Errorf("%s: err = %v, want ErrAbort", etype, err)
		}
	}
	api := &fakeKite{histErr: kiteconnect.NewError(kiteconnect.InputError, "bad token", nil)}
	if _, err := fastKite(api, map[string]uint32{"NSE:INFY": 1}).Daily(context.Background(), "NSE:INFY", time.Now(), time.Now()); err == nil || errors.Is(err, ErrAbort) {
		t.Errorf("input error: %v, want a plain error", err)
	}
}

func TestKiteQuotesBatchesOf500(t *testing.T) {
	api := &fakeKite{}
	k := fastKite(api, nil)
	keys := make([]string, 1200)
	for i := range keys {
		keys[i] = fmt.Sprintf("NSE:S%d", i)
	}
	got, err := k.Quotes(context.Background(), keys)
	if err != nil {
		t.Fatal(err)
	}
	if len(api.quoteCalls) != 3 || len(api.quoteCalls[0]) != 500 || len(api.quoteCalls[2]) != 200 {
		t.Errorf("batches = %d (first %d)", len(api.quoteCalls), len(api.quoteCalls[0]))
	}
	if len(got) != 1200 || got["NSE:S7"] != (Quote{Last: 105, PrevClose: 100, Volume: 7}) {
		t.Errorf("quotes = %d, %+v", len(got), got["NSE:S7"])
	}
	api.quoteErr = errors.New("503")
	if _, err := k.Quotes(context.Background(), keys[:1]); err == nil {
		t.Error("quote error swallowed")
	}
}

func TestKiteRespectsContext(t *testing.T) {
	k := NewKite(&fakeKite{}, map[string]uint32{"NSE:A": 1})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := k.Daily(ctx, "NSE:A", time.Now(), time.Now()); err == nil {
		t.Error("cancelled context ignored")
	}
}
