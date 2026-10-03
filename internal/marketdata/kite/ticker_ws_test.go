package kite

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/marketdata"
)

// fullPacket builds a 184-byte full-mode packet for an NSE equity token, as
// documented at https://kite.trade/docs/connect/v3/websocket/. Prices are in
// paise.
func fullPacket(token, ltpPaise, volume, exchangeTS uint32) []byte {
	b := make([]byte, 184)
	be := binary.BigEndian
	be.PutUint32(b[0:], token)
	be.PutUint32(b[4:], ltpPaise)
	be.PutUint32(b[8:], 5)           // last traded quantity
	be.PutUint32(b[12:], ltpPaise-5) // average traded price
	be.PutUint32(b[16:], volume)
	be.PutUint32(b[20:], 100) // total buy quantity
	be.PutUint32(b[24:], 200) // total sell quantity
	be.PutUint32(b[28:], 149000)
	be.PutUint32(b[32:], 151000)
	be.PutUint32(b[36:], 148000)
	be.PutUint32(b[40:], 149500)
	be.PutUint32(b[44:], exchangeTS-1) // last trade time
	be.PutUint32(b[60:], exchangeTS)
	// Best bid and ask.
	be.PutUint32(b[64:], 40)
	be.PutUint32(b[68:], ltpPaise-5)
	be.PutUint16(b[72:], 2)
	be.PutUint32(b[124:], 30)
	be.PutUint32(b[128:], ltpPaise+5)
	be.PutUint16(b[132:], 1)
	return b
}

func frame(packets ...[]byte) []byte {
	b := binary.BigEndian.AppendUint16(nil, uint16(len(packets)))
	for _, p := range packets {
		b = binary.BigEndian.AppendUint16(b, uint16(len(p)))
		b = append(b, p...)
	}
	return b
}

type wsMessage struct {
	A string          `json:"a"`
	V json.RawMessage `json:"v"`
}

// fakeKiteServer accepts ticker connections. On each connection it waits
// for subscribe and full-mode messages, then sends one tick frame per
// connection. The first connection is dropped after its tick to force a
// reconnect.
type fakeKiteServer struct {
	t     *testing.T
	mu    sync.Mutex
	conns int
	query []url.Values
	subs  []string // one entry per connection: the actions received before ticks were sent
}

func (s *fakeKiteServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	s.mu.Lock()
	s.conns++
	n := s.conns
	s.query = append(s.query, r.URL.Query())
	s.mu.Unlock()

	var actions string
	gotSub, gotFull := false, false
	for !gotSub || !gotFull {
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var m wsMessage
		if json.Unmarshal(msg, &m) != nil {
			continue
		}
		actions += m.A + " "
		switch m.A {
		case "subscribe":
			gotSub = true
		case "mode":
			gotFull = gotFull || string(m.V) == `["full",[408065]]`
		}
	}
	s.mu.Lock()
	s.subs = append(s.subs, actions)
	s.mu.Unlock()

	ts := uint32(1790919600 + n)
	if err := conn.WriteMessage(websocket.BinaryMessage, frame(fullPacket(408065, 150000+uint32(n), 1000*uint32(n), ts))); err != nil {
		return
	}
	if n == 1 {
		return // drop the connection; the ticker must reconnect
	}
	// Keep the connection alive with heartbeats, as Kite does.
	for {
		time.Sleep(500 * time.Millisecond)
		if conn.WriteMessage(websocket.BinaryMessage, []byte{0}) != nil {
			return
		}
	}
}

// TestRunAgainstWebSocketReconnects drives the real vendor ticker against a
// local WebSocket server: it must decode full-mode packets, and after the
// server drops the connection it must reconnect with backoff and resubscribe.
// The vendor's dead-connection check and backoff make it take about 10 s.
func TestRunAgainstWebSocketReconnects(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: waits for the vendor ticker's reconnect backoff")
	}
	fake := &fakeKiteServer{t: t}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	u.Scheme = "ws"

	s, err := New(Config{
		APIKey:      "key123",
		Tokens:      staticToken("tok456"),
		Instruments: []domain.Instrument{infy},
		URL:         u,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out := make(chan marketdata.Event, 16)
	errc := make(chan error, 1)
	go func() { errc <- s.Run(ctx, out) }()

	var ticks []domain.Tick
	for len(ticks) < 2 {
		select {
		case ev := <-out:
			ticks = append(ticks, *ev.Tick)
		case <-ctx.Done():
			t.Fatalf("timed out with %d ticks", len(ticks))
		}
	}
	cancel()
	if err := <-errc; err != nil {
		t.Errorf("Run = %v", err)
	}

	first := ticks[0]
	if first.InstrumentToken != 408065 || first.Symbol != "NSE:INFY" || first.LastPrice != 1500.01 ||
		first.Volume != 1000 || first.Bids[0].Price != 1499.96 || first.Asks[0].Quantity != 30 ||
		!first.ExchangeTime.Equal(time.Unix(1790919601, 0)) || first.ReceivedAt.IsZero() {
		t.Errorf("first tick decoded wrong: %+v", first)
	}
	if ticks[1].LastPrice != 1500.02 || ticks[1].Volume != 2000 {
		t.Errorf("second tick, after reconnect: %+v", ticks[1])
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.conns < 2 || len(fake.subs) < 2 {
		t.Fatalf("server saw %d connections and %d subscriptions, want a reconnect and a resubscribe", fake.conns, len(fake.subs))
	}
	if q := fake.query[0]; q.Get("api_key") != "key123" || q.Get("access_token") != "tok456" {
		t.Errorf("connect query = %v", q)
	}
	if st := s.Stats(); st.Connects < 2 || st.Reconnects < 1 || st.Dropped != 0 {
		t.Errorf("stats = %+v", st)
	}
}
