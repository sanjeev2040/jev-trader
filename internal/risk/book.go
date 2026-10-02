package risk

import (
	"math"
	"sort"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/domain"
)

// Book is the risk state the engine owns: positions, last prices, orders in
// flight, the day's realized PnL, the order-rate window and the kill switch.
// It is not safe for concurrent use; the engine loop is its only user.
type Book struct {
	positions map[uint32]domain.Position
	marks     map[uint32]float64
	inflight  map[uint32]domain.OrderIntent // unfilled remainder of each in-flight order

	day        string // trading day, YYYY-MM-DD in the limits' time zone
	realized   float64
	orderTimes []time.Time // approvals within the last minute, oldest first
	dayOrders  int         // decision-driven approvals today
	rejections int         // consecutive broker rejections
	lossHalted bool
	killed     bool
	killReason Reason
}

// NewBook returns an empty book.
func NewBook() *Book {
	return &Book{
		positions: map[uint32]domain.Position{},
		marks:     map[uint32]float64{},
		inflight:  map[uint32]domain.OrderIntent{},
	}
}

// Mark records the last traded price of an instrument.
func (b *Book) Mark(token uint32, price float64) {
	if price > 0 && !math.IsInf(price, 0) {
		b.marks[token] = price
	}
}

// SetPosition replaces the position in an instrument, for example when
// reconciling with the broker at startup. It does not touch realized PnL.
func (b *Book) SetPosition(p domain.Position) {
	if p.Quantity == 0 {
		delete(b.positions, p.Instrument.Token)
		return
	}
	b.positions[p.Instrument.Token] = p
}

// Position returns the open position in an instrument.
func (b *Book) Position(token uint32) (domain.Position, bool) {
	p, ok := b.positions[token]
	return p, ok
}

// Positions returns the open positions ordered by instrument token.
func (b *Book) Positions() []domain.Position {
	out := make([]domain.Position, 0, len(b.positions))
	for _, p := range b.positions {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Instrument.Token < out[j].Instrument.Token })
	return out
}

// ApplyFill updates the position and realized PnL for a fill, net of its
// charges, and reduces the matching order in flight.
func (b *Book) ApplyFill(f domain.Fill) {
	if f.Quantity <= 0 || (f.Side != domain.ActionBuy && f.Side != domain.ActionSell) {
		return
	}
	token := f.Instrument.Token
	signed := f.Quantity
	if f.Side == domain.ActionSell {
		signed = -signed
	}

	p, ok := b.positions[token]
	if !ok {
		p = domain.Position{Instrument: f.Instrument}
	}
	q := p.Quantity
	next := q + signed
	switch {
	case q == 0 || (q > 0) == (signed > 0):
		p.AvgPrice = (float64(abs(q))*p.AvgPrice + float64(abs(signed))*f.Price) / float64(abs(next))
	default:
		closed := min(abs(signed), abs(q))
		b.realized += float64(closed) * (f.Price - p.AvgPrice) * float64(sign(q))
		if next != 0 && (next > 0) != (q > 0) {
			p.AvgPrice = f.Price // flipped through flat
		}
	}
	b.realized -= f.Charges
	p.Quantity = next
	b.SetPosition(p)

	if in, ok := b.inflight[token]; ok && in.Side == f.Side {
		in.Quantity -= f.Quantity
		if in.Quantity <= 0 {
			delete(b.inflight, token)
		} else {
			b.inflight[token] = in
		}
	}
}

// Settle clears the order in flight for an instrument. Call it when the order
// reaches a terminal state: complete, rejected or cancelled.
func (b *Book) Settle(token uint32) { delete(b.inflight, token) }

// record notes an approved order as in flight and against the rate window.
func (b *Book) record(in domain.OrderIntent, now time.Time) {
	b.inflight[in.Instrument.Token] = in
	b.orderTimes = append(b.orderTimes, now)
}

// inflightOrders returns the orders in flight ordered by instrument token.
func (b *Book) inflightOrders() []domain.OrderIntent {
	out := make([]domain.OrderIntent, 0, len(b.inflight))
	for _, in := range b.inflight {
		out = append(out, in)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Instrument.Token < out[j].Instrument.Token })
	return out
}

// InFlight reports whether an approved order for the instrument is still open.
func (b *Book) InFlight(token uint32) bool {
	_, ok := b.inflight[token]
	return ok
}

// NoteAccepted resets the consecutive broker-rejection count.
func (b *Book) NoteAccepted() { b.rejections = 0 }

// Kill trips the kill switch. It stays tripped for the life of the book: new
// entries are refused and Gate.Exits flattens every position.
func (b *Book) Kill(reason Reason) {
	if !b.killed {
		b.killed = true
		b.killReason = reason
	}
}

// Killed reports whether the kill switch is tripped, and why.
func (b *Book) Killed() (bool, Reason) { return b.killed, b.killReason }

// RealizedPnL returns today's realized PnL net of charges.
func (b *Book) RealizedPnL() float64 { return b.realized }

// PnL returns today's realized PnL plus the unrealized PnL of open positions
// at their last marks. A position with no mark counts as flat.
func (b *Book) PnL() float64 {
	pnl := b.realized
	for token, p := range b.positions {
		if m, ok := b.marks[token]; ok {
			pnl += float64(p.Quantity) * (m - p.AvgPrice)
		}
	}
	return pnl
}

// rollDay starts a new trading day when now falls on a later date: it resets
// realized PnL, the daily order budget and the loss halt. Positions, orders
// in flight and the kill switch carry over.
func (b *Book) rollDay(now time.Time) {
	day := now.Format(time.DateOnly)
	if day == b.day {
		return
	}
	if b.day != "" {
		b.realized = 0
		b.dayOrders = 0
		b.lossHalted = false
		b.rejections = 0
	}
	b.day = day
}

// ordersAfter counts approvals strictly after t.
func (b *Book) ordersAfter(t time.Time) int {
	i := sort.Search(len(b.orderTimes), func(i int) bool { return b.orderTimes[i].After(t) })
	return len(b.orderTimes) - i
}

func (b *Book) pruneOrders(now time.Time) {
	cut := now.Add(-time.Minute)
	i := 0
	for i < len(b.orderTimes) && !b.orderTimes[i].After(cut) {
		i++
	}
	b.orderTimes = b.orderTimes[i:]
}

// exposure returns |quantity| × price of an instrument's position plus its
// order in flight, if that order adds to the position.
func (b *Book) exposure(token uint32) float64 {
	price := b.price(token)
	q := abs(b.positions[token].Quantity)
	if in, ok := b.inflight[token]; ok && !in.ReduceOnly {
		q += in.Quantity
	}
	return float64(q) * price
}

func (b *Book) grossExposure() float64 {
	seen := map[uint32]bool{}
	var g float64
	for token := range b.positions {
		seen[token] = true
		g += b.exposure(token)
	}
	for token := range b.inflight {
		if !seen[token] {
			g += b.exposure(token)
		}
	}
	return g
}

// openCount counts instruments with an open position or an entry in flight.
func (b *Book) openCount() int {
	n := len(b.positions)
	for token, in := range b.inflight {
		if _, ok := b.positions[token]; !ok && !in.ReduceOnly {
			n++
		}
	}
	return n
}

// price returns the last mark, falling back to the position's average price.
func (b *Book) price(token uint32) float64 {
	if m, ok := b.marks[token]; ok {
		return m
	}
	if p, ok := b.positions[token]; ok {
		return p.AvgPrice
	}
	if in, ok := b.inflight[token]; ok {
		return in.RefPrice
	}
	return 0
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

func sign(x int64) int64 {
	switch {
	case x > 0:
		return 1
	case x < 0:
		return -1
	}
	return 0
}
