// Package kite is the live Kite Connect market-data source: a KiteTicker
// WebSocket adapter behind marketdata.Source, and the instrument master that
// maps watchlist symbols to instrument tokens.
//
// This is the only package that imports the vendor ticker, so replacing it
// touches nothing else.
package kite

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zerodha/gokiteconnect/v4/models"
	kiteticker "github.com/zerodha/gokiteconnect/v4/ticker"

	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/marketdata"
)

// TokenProvider returns the current Kite access token. kiteauth (#3)
// provides the real implementation.
type TokenProvider interface {
	Token(ctx context.Context) (string, error)
}

// ErrReconnectExhausted is returned by Run when the ticker gives up
// reconnecting.
var ErrReconnectExhausted = errors.New("kite ticker: reconnect attempts exhausted")

// defaultShutdownGrace bounds how long Run waits for the vendor ticker to
// stop after ctx ends. The ticker only notices cancellation between reads
// and reconnect sleeps, so it can lag.
const defaultShutdownGrace = 3 * time.Second

// Config configures a Source.
type Config struct {
	APIKey      string
	Tokens      TokenProvider
	Instruments []domain.Instrument // subscribed in full mode

	// MaxReconnectAttempts caps consecutive reconnect attempts. Zero keeps
	// the vendor default (300). Backoff is exponential, capped at 60 s.
	MaxReconnectAttempts int

	// URL overrides the ticker WebSocket URL; for tests.
	URL *url.URL
	// Now returns the local receive time; defaults to time.Now.
	Now func() time.Time
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Stats are a Source's running counters.
type Stats struct {
	Published  uint64 // ticks sent on the out channel
	Dropped    uint64 // ticks dropped because the out channel was full
	Connects   uint64 // successful connections, including reconnects
	Reconnects uint64 // reconnect attempts
}

// ticker is the subset of the vendor ticker the source drives.
type ticker interface {
	OnConnect(func())
	OnTick(func(models.Tick))
	OnError(func(error))
	OnClose(func(code int, reason string))
	OnReconnect(func(attempt int, delay time.Duration))
	OnNoReconnect(func(attempt int))
	Subscribe(tokens []uint32) error
	SetMode(mode kiteticker.Mode, tokens []uint32) error
	ServeWithContext(ctx context.Context)
}

// Source streams ticks from the KiteTicker WebSocket.
type Source struct {
	cfg       Config
	tokens    []uint32
	symbols   map[uint32]string
	newTicker func(apiKey, accessToken string) ticker
	grace     time.Duration

	published, dropped, connects, reconnects atomic.Uint64
}

var _ marketdata.Source = (*Source)(nil)

// New returns a Source for cfg.
func New(cfg Config) (*Source, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("kite source: API key is required")
	}
	if cfg.Tokens == nil {
		return nil, errors.New("kite source: token provider is required")
	}
	if len(cfg.Instruments) == 0 {
		return nil, errors.New("kite source: no instruments to subscribe")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	s := &Source{cfg: cfg, symbols: make(map[uint32]string, len(cfg.Instruments)), grace: defaultShutdownGrace}
	for _, in := range cfg.Instruments {
		if _, dup := s.symbols[in.Token]; dup {
			continue
		}
		s.symbols[in.Token] = instrumentKey(in)
		s.tokens = append(s.tokens, in.Token)
	}
	s.newTicker = func(apiKey, accessToken string) ticker {
		t := kiteticker.New(apiKey, accessToken)
		if cfg.URL != nil {
			t.SetRootURL(*cfg.URL)
		}
		if cfg.MaxReconnectAttempts > 0 {
			t.SetReconnectMaxRetries(cfg.MaxReconnectAttempts)
		}
		return t
	}
	return s, nil
}

// Stats returns the current counters.
func (s *Source) Stats() Stats {
	return Stats{
		Published:  s.published.Load(),
		Dropped:    s.dropped.Load(),
		Connects:   s.connects.Load(),
		Reconnects: s.reconnects.Load(),
	}
}

// Run connects, subscribes every instrument in full mode, and publishes each
// tick on out. It reconnects with exponential backoff and resubscribes after
// every reconnect.
//
// It never blocks the WebSocket reader: when out is full the tick is dropped
// and counted. Run returns nil when ctx ends, and ErrReconnectExhausted when
// the ticker gives up.
func (s *Source) Run(ctx context.Context, out chan<- marketdata.Event) error {
	accessToken, err := s.cfg.Tokens.Token(ctx)
	if err != nil {
		return fmt.Errorf("kite access token: %w", err)
	}
	log := s.cfg.Logger.With("component", "kite-ticker")
	t := s.newTicker(s.cfg.APIKey, accessToken)

	// stopped guards out: callbacks can still fire after Run returns if the
	// vendor ticker is slow to stop, and the caller may close out by then.
	var (
		mu      sync.RWMutex
		stopped bool
		gaveUp  atomic.Bool
	)

	t.OnConnect(func() {
		n := s.connects.Add(1)
		// Subscribing on every connect makes resubscription explicit
		// rather than relying only on the vendor's own resubscribe.
		if err := t.Subscribe(s.tokens); err != nil {
			log.Error("subscribe failed", "err", err)
			return
		}
		if err := t.SetMode(kiteticker.ModeFull, s.tokens); err != nil {
			log.Error("set full mode failed", "err", err)
			return
		}
		log.Info("connected and subscribed", "instruments", len(s.tokens), "connects", n)
	})
	t.OnTick(func(mt models.Tick) {
		tick := s.convert(mt)
		mu.RLock()
		defer mu.RUnlock()
		if stopped {
			return
		}
		select {
		case out <- marketdata.Event{Tick: &tick}:
			s.published.Add(1)
		default:
			n := s.dropped.Add(1)
			if n == 1 || n%1000 == 0 {
				log.Error("tick dropped: consumer is not keeping up", "dropped_total", n)
			}
		}
	})
	t.OnReconnect(func(attempt int, delay time.Duration) {
		s.reconnects.Add(1)
		log.Warn("reconnecting", "attempt", attempt, "delay", delay)
	})
	t.OnNoReconnect(func(attempt int) {
		gaveUp.Store(true)
		log.Error("giving up reconnecting", "attempts", attempt)
	})
	t.OnError(func(err error) {
		if ctx.Err() == nil {
			log.Warn("ticker error", "err", err)
		}
	})
	t.OnClose(func(code int, reason string) {
		log.Info("connection closed", "code", code, "reason", reason)
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		t.ServeWithContext(ctx)
	}()

	select {
	case <-ctx.Done():
	case <-done:
	}
	mu.Lock()
	stopped = true
	mu.Unlock()

	if ctx.Err() != nil {
		select {
		case <-done:
		case <-time.After(s.grace):
			log.Warn("ticker did not stop within grace period")
		}
		return nil
	}
	if gaveUp.Load() {
		return ErrReconnectExhausted
	}
	return errors.New("kite ticker stopped unexpectedly")
}

// convert maps a vendor tick to a domain tick stamped with the receive time.
func (s *Source) convert(mt models.Tick) domain.Tick {
	t := domain.Tick{
		InstrumentToken:   mt.InstrumentToken,
		Symbol:            s.symbols[mt.InstrumentToken],
		ExchangeTime:      mt.Timestamp.Time,
		ReceivedAt:        s.cfg.Now(),
		LastTradeTime:     mt.LastTradeTime.Time,
		LastPrice:         mt.LastPrice,
		LastQuantity:      int64(mt.LastTradedQuantity),
		AveragePrice:      mt.AverageTradePrice,
		Volume:            int64(mt.VolumeTraded),
		TotalBuyQuantity:  int64(mt.TotalBuyQuantity),
		TotalSellQuantity: int64(mt.TotalSellQuantity),
		Open:              mt.OHLC.Open,
		High:              mt.OHLC.High,
		Low:               mt.OHLC.Low,
		Close:             mt.OHLC.Close,
		OI:                int64(mt.OI),
	}
	for i := range t.Bids {
		b, a := mt.Depth.Buy[i], mt.Depth.Sell[i]
		t.Bids[i] = domain.DepthLevel{Price: b.Price, Quantity: int64(b.Quantity), Orders: int64(b.Orders)}
		t.Asks[i] = domain.DepthLevel{Price: a.Price, Quantity: int64(a.Quantity), Orders: int64(a.Orders)}
	}
	return t
}
