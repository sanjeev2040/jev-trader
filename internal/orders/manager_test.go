package orders_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"testing"

	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/orders"
	"github.com/sanjeev2040/jev-trader/internal/risk"
)

// fakeBroker records calls and checks that every placed intent was journaled
// first.
type fakeBroker struct {
	journal          *memJournal
	placed           []domain.OrderIntent
	canceled         []string
	placeErr         error
	book             []domain.OrderUpdate
	updates          chan domain.OrderUpdate
	unjournaledPlace bool
}

func (b *fakeBroker) Place(_ context.Context, o domain.OrderIntent) (string, error) {
	if !b.journal.has(orders.ClientID(o.CycleID)) {
		b.unjournaledPlace = true
	}
	b.placed = append(b.placed, o)
	if b.placeErr != nil {
		return "", b.placeErr
	}
	return fmt.Sprintf("B%d", len(b.placed)), nil
}

func (b *fakeBroker) Cancel(_ context.Context, id string) error {
	b.canceled = append(b.canceled, id)
	return nil
}

func (b *fakeBroker) Updates() <-chan domain.OrderUpdate { return b.updates }

func (b *fakeBroker) Orders(context.Context) ([]domain.OrderUpdate, error) { return b.book, nil }

// noBookBroker hides the fake's Orders method.
type noBookBroker struct{ orders.Broker }

type memJournal struct {
	intents []domain.OrderIntent
	err     error
}

func (j *memJournal) RecordIntent(_ context.Context, o domain.OrderIntent) error {
	if j.err != nil {
		return j.err
	}
	j.intents = append(j.intents, o)
	return nil
}

func (j *memJournal) has(clientID string) bool {
	for _, o := range j.intents {
		if orders.ClientID(o.CycleID) == clientID {
			return true
		}
	}
	return false
}

func newManager(t *testing.T) (*orders.Manager, *fakeBroker, *memJournal) {
	t.Helper()
	j := &memJournal{}
	b := &fakeBroker{journal: j}
	m, err := orders.NewManager(b, j)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Reconcile(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	return m, b, j
}

func TestClientID(t *testing.T) {
	a, b := orders.ClientID("cycle-1"), orders.ClientID("cycle-2")
	if a != orders.ClientID("cycle-1") {
		t.Error("ClientID is not deterministic")
	}
	if a == b {
		t.Error("different cycles share a client id")
	}
	// Kite tags are at most 20 alphanumeric characters.
	if !regexp.MustCompile(`^jt[0-9a-f]{18}$`).MatchString(a) {
		t.Errorf("ClientID = %q, want jt + 18 hex characters", a)
	}
}

func TestValidateIntent(t *testing.T) {
	valid := intentOf(t, "c1")
	exit := approveExit(t, "c2").Intent()
	for name, o := range map[string]domain.OrderIntent{"entry": valid, "exit": exit} {
		if err := orders.ValidateIntent(o); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	noPrice := valid
	noPrice.RefPrice = 0
	if err := orders.ValidateIntent(noPrice); err != nil {
		t.Errorf("no reference price: %v", err)
	}
	cases := map[string]func(*domain.OrderIntent){
		"no cycle":       func(o *domain.OrderIntent) { o.CycleID = "" },
		"no symbol":      func(o *domain.OrderIntent) { o.Instrument.Symbol = "" },
		"no exchange":    func(o *domain.OrderIntent) { o.Instrument.Exchange = "" },
		"hold side":      func(o *domain.OrderIntent) { o.Side = domain.ActionHold },
		"zero quantity":  func(o *domain.OrderIntent) { o.Quantity = 0 },
		"negative price": func(o *domain.OrderIntent) { o.RefPrice = -1 },
		"NaN price":      func(o *domain.OrderIntent) { o.RefPrice = math.NaN() },
		"infinite price": func(o *domain.OrderIntent) { o.RefPrice = math.Inf(1) },
		"sub-tick price": func(o *domain.OrderIntent) { o.RefPrice = 0.01 },
	}
	for name, mutate := range cases {
		o := valid
		mutate(&o)
		if err := orders.ValidateIntent(o); !errors.Is(err, orders.ErrInvalidIntent) {
			t.Errorf("%s: err = %v, want ErrInvalidIntent", name, err)
		}
	}
}

func TestSpecFor(t *testing.T) {
	entry := intentOf(t, "c1")
	if got := orders.SpecFor(entry); got != (orders.Spec{Type: domain.OrderTypeLimit, LimitPrice: entryPrice}) {
		t.Errorf("entry spec = %+v", got)
	}
	// A risk exit must get out, so it is a protected market order.
	want := orders.Spec{Type: domain.OrderTypeMarket, MarketProtection: orders.MarketProtectionPct}
	if got := orders.SpecFor(approveExit(t, "c2").Intent()); got != want {
		t.Errorf("exit spec = %+v, want %+v", got, want)
	}
	entry.RefPrice = 0
	if got := orders.SpecFor(entry); got != want {
		t.Errorf("unpriced spec = %+v, want %+v", got, want)
	}
	entry.RefPrice = 1234.5678
	if got := orders.SpecFor(entry); got.LimitPrice != 1234.55 {
		t.Errorf("limit price %v not rounded to the tick", got.LimitPrice)
	}
}

func TestNewManagerNeedsBrokerAndJournal(t *testing.T) {
	if _, err := orders.NewManager(nil, &memJournal{}); err == nil {
		t.Error("nil broker accepted")
	}
	if _, err := orders.NewManager(&fakeBroker{}, nil); err == nil {
		t.Error("nil journal accepted")
	}
}

func TestSubmitBeforeReconcile(t *testing.T) {
	j := &memJournal{}
	b := &fakeBroker{journal: j}
	m, _ := orders.NewManager(b, j)
	_, err := m.Submit(context.Background(), approve(t, "c1"))
	if !errors.Is(err, orders.ErrNotReconciled) {
		t.Fatalf("err = %v, want ErrNotReconciled", err)
	}
	if len(b.placed) != 0 {
		t.Error("placed before reconcile")
	}
}

func TestSubmitZeroApproval(t *testing.T) {
	m, b, _ := newManager(t)
	if _, err := m.Submit(context.Background(), risk.Approval{}); !errors.Is(err, orders.ErrZeroApproval) {
		t.Fatalf("err = %v, want ErrZeroApproval", err)
	}
	if len(b.placed) != 0 {
		t.Error("zero approval placed an order")
	}
}

func TestSubmitRecordsThenPlacesOnce(t *testing.T) {
	m, b, j := newManager(t)
	ctx := context.Background()
	a := approve(t, "c1")

	o, err := m.Submit(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	want := orders.ClientID("c1")
	if orders.ClientID(o.Intent.CycleID) != want || o.BrokerID != "B1" || o.Status != orders.StatusPending {
		t.Fatalf("order = %+v", o)
	}
	if len(j.intents) != 1 || b.unjournaledPlace {
		t.Fatalf("intent not journaled before Place: journal %v", j.intents)
	}
	if orders.ClientID(b.placed[0].CycleID) != want {
		t.Errorf("placed tag %q, want %q", orders.ClientID(b.placed[0].CycleID), want)
	}

	again, err := m.Submit(ctx, a)
	if !errors.Is(err, orders.ErrAlreadySubmitted) {
		t.Fatalf("resubmit err = %v, want ErrAlreadySubmitted", err)
	}
	if again.BrokerID != "B1" || len(b.placed) != 1 || len(j.intents) != 1 {
		t.Fatalf("resubmit placed again: %d placements", len(b.placed))
	}
}

func TestSubmitJournalFailurePlacesNothing(t *testing.T) {
	m, b, j := newManager(t)
	ctx := context.Background()
	a := approve(t, "c1")
	j.err = errors.New("disk full")
	if _, err := m.Submit(ctx, a); err == nil {
		t.Fatal("Submit succeeded without a journal record")
	}
	if len(b.placed) != 0 {
		t.Fatal("placed an unrecorded intent")
	}
	// Nothing was placed, so the cycle may be submitted again.
	j.err = nil
	if _, err := m.Submit(ctx, a); err != nil {
		t.Fatal(err)
	}
	if len(b.placed) != 1 {
		t.Fatalf("%d placements, want 1", len(b.placed))
	}
}

func TestSubmitPlaceFailureIsNotRetried(t *testing.T) {
	m, b, _ := newManager(t)
	ctx := context.Background()
	a := approve(t, "c1")
	b.placeErr = errors.New("timeout")
	o, err := m.Submit(ctx, a)
	if err == nil || o.Status != orders.StatusPlaceFailed {
		t.Fatalf("order %+v, err %v", o, err)
	}
	if open := m.Open(); len(open) != 1 {
		t.Fatalf("Open() = %+v, want the order whose placement is unknown", open)
	}
	// The broker may have the order anyway, so it is never placed twice.
	b.placeErr = nil
	if _, err := m.Submit(ctx, a); !errors.Is(err, orders.ErrAlreadySubmitted) {
		t.Fatalf("err = %v, want ErrAlreadySubmitted", err)
	}
	if len(b.placed) != 1 {
		t.Fatalf("%d placements, want 1", len(b.placed))
	}
	// A later update for the tag revives the order.
	got, _, err := m.Apply(domain.OrderUpdate{ClientID: orders.ClientID(o.Intent.CycleID), BrokerID: "K9", Status: domain.OrderStatusOpen})
	if err != nil || got.Status != orders.StatusOpen || got.BrokerID != "K9" {
		t.Fatalf("revived order %+v, err %v", got, err)
	}
}

func TestSubmitDefiniteRejectionIsTerminal(t *testing.T) {
	m, b, _ := newManager(t)
	ctx := context.Background()
	a := approve(t, "c1")
	b.placeErr = fmt.Errorf("%w: invalid tradingsymbol", orders.ErrNotPlaced)
	o, err := m.Submit(ctx, a)
	if !errors.Is(err, orders.ErrNotPlaced) || o.Status != orders.StatusRejected || o.Reason != b.placeErr.Error() {
		t.Fatalf("order %+v, err %v", o, err)
	}
	if open := m.Open(); len(open) != 0 {
		t.Errorf("Open() = %+v, want none", open)
	}
	if err := m.Cancel(ctx, orders.ClientID(o.Intent.CycleID)); !errors.Is(err, orders.ErrOrderClosed) {
		t.Errorf("cancel: err = %v, want ErrOrderClosed", err)
	}
	b.placeErr = nil
	if _, err := m.Submit(ctx, a); !errors.Is(err, orders.ErrAlreadySubmitted) {
		t.Fatalf("err = %v, want ErrAlreadySubmitted", err)
	}
	if len(b.placed) != 1 {
		t.Fatalf("%d placements, want 1", len(b.placed))
	}
}

func TestApplyTracksFills(t *testing.T) {
	m, _, _ := newManager(t)
	o, _ := m.Submit(context.Background(), approve(t, "c1"))
	id := orders.ClientID(o.Intent.CycleID)
	up := func(status domain.OrderStatus, filled int64, avg, costs float64) domain.OrderUpdate {
		return domain.OrderUpdate{ClientID: id, BrokerID: "B1", Status: status, FilledQuantity: filled, AveragePrice: avg, Costs: costs, Time: t0}
	}

	steps := []struct {
		u        domain.OrderUpdate
		status   orders.Status
		fillQty  int64
		fillPx   float64
		fillCost float64
	}{
		{u: up(domain.OrderStatusOpen, 0, 0, 0), status: orders.StatusOpen},
		{u: up(domain.OrderStatusOpen, 40, 100, 4), status: orders.StatusOpen, fillQty: 40, fillPx: 100, fillCost: 4},
		{u: up(domain.OrderStatusOpen, 40, 100, 4), status: orders.StatusOpen}, // duplicate
		{u: up(domain.OrderStatusFilled, 100, 101, 10), status: orders.StatusFilled, fillQty: 60, fillPx: (101*100 - 100*40) / 60.0, fillCost: 6},
		{u: up(domain.OrderStatusOpen, 40, 100, 4), status: orders.StatusFilled}, // late and stale
	}
	for i, s := range steps {
		got, fill, err := m.Apply(s.u)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if got.Status != s.status {
			t.Errorf("step %d: status %s, want %s", i, got.Status, s.status)
		}
		switch {
		case s.fillQty == 0 && fill != nil:
			t.Errorf("step %d: unexpected fill %+v", i, *fill)
		case s.fillQty != 0 && fill == nil:
			t.Errorf("step %d: missing fill", i)
		case fill != nil && (fill.Quantity != s.fillQty || math.Abs(fill.Price-s.fillPx) > 1e-9 || fill.Charges != s.fillCost || fill.ClientID != id):
			t.Errorf("step %d: fill %+v, want %d @ %v costs %v", i, *fill, s.fillQty, s.fillPx, s.fillCost)
		}
	}
	final, _ := m.Order(id)
	if final.FilledQuantity != 100 || final.AveragePrice != 101 || final.Costs != 10 {
		t.Errorf("final order %+v", final)
	}
	if len(m.Open()) != 0 {
		t.Errorf("Open() = %v, want none", m.Open())
	}
}

func TestApplyMatchesByBrokerID(t *testing.T) {
	m, _, _ := newManager(t)
	o, _ := m.Submit(context.Background(), approve(t, "c1"))
	got, _, err := m.Apply(domain.OrderUpdate{BrokerID: o.BrokerID, Status: domain.OrderStatusRejected, Reason: "margin"})
	if err != nil || got.Status != orders.StatusRejected || got.Reason != "margin" {
		t.Fatalf("order %+v, err %v", got, err)
	}
}

func TestApplyErrors(t *testing.T) {
	m, _, _ := newManager(t)
	o, _ := m.Submit(context.Background(), approve(t, "c1"))
	id := orders.ClientID(o.Intent.CycleID)

	if _, _, err := m.Apply(domain.OrderUpdate{ClientID: "manual", BrokerID: "X"}); !errors.Is(err, orders.ErrUnknownOrder) {
		t.Errorf("unknown: err = %v", err)
	}
	if _, _, err := m.Apply(domain.OrderUpdate{ClientID: id, BrokerID: "other"}); !errors.Is(err, orders.ErrBrokerIDMismatch) {
		t.Errorf("mismatch: err = %v", err)
	}
	got, fill, err := m.Apply(domain.OrderUpdate{ClientID: id, BrokerID: o.BrokerID, Status: domain.OrderStatusFilled, FilledQuantity: 150, AveragePrice: 1500})
	if !errors.Is(err, orders.ErrOverfill) || fill == nil || got.FilledQuantity != 150 {
		t.Errorf("overfill: order %+v, fill %v, err %v", got, fill, err)
	}
}

func TestCancel(t *testing.T) {
	m, b, _ := newManager(t)
	ctx := context.Background()
	o, _ := m.Submit(ctx, approve(t, "c1"))
	id := orders.ClientID(o.Intent.CycleID)
	if err := m.Cancel(ctx, id); err != nil {
		t.Fatal(err)
	}
	if len(b.canceled) != 1 || b.canceled[0] != o.BrokerID {
		t.Fatalf("canceled %v, want [%s]", b.canceled, o.BrokerID)
	}
	m.Apply(domain.OrderUpdate{ClientID: id, BrokerID: o.BrokerID, Status: domain.OrderStatusCancelled})
	if err := m.Cancel(ctx, id); !errors.Is(err, orders.ErrOrderClosed) {
		t.Errorf("terminal: err = %v", err)
	}
	if err := m.Cancel(ctx, "nope"); !errors.Is(err, orders.ErrUnknownOrder) {
		t.Errorf("unknown: err = %v", err)
	}

	b.placeErr = errors.New("timeout")
	failed, _ := m.Submit(ctx, approve(t, "c2"))
	if err := m.Cancel(ctx, orders.ClientID(failed.Intent.CycleID)); !errors.Is(err, orders.ErrNotAcknowledged) {
		t.Errorf("unacknowledged: err = %v", err)
	}
}

func TestReconcile(t *testing.T) {
	j := &memJournal{}
	b := &fakeBroker{journal: j}
	m, _ := orders.NewManager(b, j)
	held, lost := intentOf(t, "held"), intentOf(t, "lost")
	b.book = []domain.OrderUpdate{
		{ClientID: orders.ClientID("held"), BrokerID: "K1", Status: domain.OrderStatusOpen, FilledQuantity: 30, AveragePrice: 1499},
		{ClientID: orders.ClientID("stray"), BrokerID: "K2", Status: domain.OrderStatusOpen},
		{ClientID: "manual", BrokerID: "K3", Status: domain.OrderStatusFilled},
	}

	r, err := m.Reconcile(context.Background(), []domain.OrderIntent{held, lost})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Adopted) != 1 || r.Adopted[0].BrokerID != "K1" || r.Adopted[0].Status != orders.StatusOpen || r.Adopted[0].FilledQuantity != 30 {
		t.Errorf("adopted %+v", r.Adopted)
	}
	if len(r.NotPlaced) != 1 || orders.ClientID(r.NotPlaced[0].Intent.CycleID) != orders.ClientID("lost") || r.NotPlaced[0].Status != orders.StatusNotPlaced {
		t.Errorf("not placed %+v", r.NotPlaced)
	}
	if len(r.Unrecorded) != 1 || r.Unrecorded[0].BrokerID != "K2" {
		t.Errorf("unrecorded %+v", r.Unrecorded)
	}

	// Neither recorded cycle is placed again; a new cycle is.
	ctx := context.Background()
	for _, intent := range []domain.OrderIntent{held, lost} {
		if _, err := m.Submit(ctx, approve(t, intent.CycleID)); !errors.Is(err, orders.ErrAlreadySubmitted) {
			t.Errorf("%s: err = %v, want ErrAlreadySubmitted", intent.CycleID, err)
		}
	}
	// The manager does not know an unrecorded order's cycle, which is why
	// Reconcile reports Unrecorded for an operator to act on.
	if _, err := m.Submit(ctx, approve(t, "stray")); err != nil {
		t.Fatal(err)
	}
	if len(b.placed) != 1 || b.placed[0].CycleID != "stray" {
		t.Errorf("placed %v", b.placed)
	}
	if open := m.Open(); len(open) != 2 {
		t.Errorf("Open() = %+v, want held and stray", open)
	}
}

func TestReconcileRejectsMissingCycleID(t *testing.T) {
	j := &memJournal{}
	m, _ := orders.NewManager(&fakeBroker{journal: j}, j)
	recorded := intentOf(t, "c1")
	recorded.CycleID = ""
	if _, err := m.Reconcile(context.Background(), []domain.OrderIntent{recorded}); !errors.Is(err, orders.ErrInvalidIntent) {
		t.Fatalf("err = %v, want ErrInvalidIntent", err)
	}
}

func TestLateListedOrderLeavesNotPlaced(t *testing.T) {
	j := &memJournal{}
	b := &fakeBroker{journal: j}
	m, _ := orders.NewManager(b, j)
	ctx := context.Background()
	r, err := m.Reconcile(ctx, []domain.OrderIntent{intentOf(t, "late")})
	if err != nil || len(r.NotPlaced) != 1 {
		t.Fatalf("reconcile %+v, err %v", r, err)
	}
	id := orders.ClientID("late")

	got, _, err := m.Apply(domain.OrderUpdate{ClientID: id, BrokerID: "K7", Status: domain.OrderStatusOpen, FilledQuantity: 40, AveragePrice: 1500})
	if err != nil || got.Status != orders.StatusOpen || got.BrokerID != "K7" || got.FilledQuantity != 40 {
		t.Fatalf("order %+v, err %v", got, err)
	}
	if open := m.Open(); len(open) != 1 || orders.ClientID(open[0].Intent.CycleID) != id {
		t.Errorf("Open() = %+v, want the late order", open)
	}
	if err := m.Cancel(ctx, id); err != nil || len(b.canceled) != 1 || b.canceled[0] != "K7" {
		t.Fatalf("cancel: err %v, canceled %v", err, b.canceled)
	}
	got, _, _ = m.Apply(domain.OrderUpdate{ClientID: id, BrokerID: "K7", Status: domain.OrderStatusFilled, FilledQuantity: 100, AveragePrice: 1500})
	if got.Status != orders.StatusFilled || len(m.Open()) != 0 {
		t.Errorf("order %+v, want FILLED and closed", got)
	}
	if len(b.placed) != 0 {
		t.Errorf("placed %v, want nothing", b.placed)
	}
}

func TestReconcileNeedsOrderBook(t *testing.T) {
	j := &memJournal{}
	m, _ := orders.NewManager(noBookBroker{&fakeBroker{journal: j}}, j)
	if _, err := m.Reconcile(context.Background(), nil); !errors.Is(err, orders.ErrNoOrderBook) {
		t.Fatalf("err = %v, want ErrNoOrderBook", err)
	}
}
