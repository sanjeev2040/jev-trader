package decide

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/snapshot"
)

func snapshotWith(ret5 *float64, side string) json.RawMessage {
	s := snapshot.Snapshot{
		SchemaVersion: snapshot.SchemaVersion,
		Instrument:    "I01",
		Features:      snapshot.Features{ReturnsPct: snapshot.Returns{Bars5: ret5}},
		Position:      snapshot.Position{Side: side, SizeBucket: snapshot.SizeNone},
	}
	b, err := snapshot.Encode(s)
	if err != nil {
		panic(err)
	}
	return b
}

func f(v float64) *float64 { return &v }

func TestAlwaysHold(t *testing.T) {
	d, err := AlwaysHold{}.Decide(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != domain.ActionHold || d.Model != ModelAlwaysHold || d.HoldReason != "" {
		t.Errorf("got %+v", d)
	}
	if err := Validate(d); err != nil {
		t.Errorf("not a valid decision: %v", err)
	}
}

func TestMomentumRule(t *testing.T) {
	m, err := NewMomentum(MomentumConfig{})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		ret  *float64
		side string
		want domain.Action
	}{
		{"flat, strong up", f(0.5), snapshot.SideFlat, domain.ActionBuy},
		{"flat, strong down", f(-0.5), snapshot.SideFlat, domain.ActionSell},
		{"flat, inside the band", f(0.05), snapshot.SideFlat, domain.ActionHold},
		{"flat, exactly at the threshold", f(0.10), snapshot.SideFlat, domain.ActionHold},
		{"flat, return unknown", nil, snapshot.SideFlat, domain.ActionHold},
		{"long, still rising", f(0.5), snapshot.SideLong, domain.ActionHold},
		{"long, turned down", f(-0.01), snapshot.SideLong, domain.ActionSell},
		{"short, still falling", f(-0.5), snapshot.SideShort, domain.ActionHold},
		{"short, turned up", f(0.01), snapshot.SideShort, domain.ActionBuy},
		{"long, return unknown", nil, snapshot.SideLong, domain.ActionHold},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := m.Decide(context.Background(), snapshotWith(tc.ret, tc.side))
			if err != nil {
				t.Fatal(err)
			}
			if d.Action != tc.want {
				t.Fatalf("action = %s, want %s", d.Action, tc.want)
			}
			if d.Model != ModelMomentum {
				t.Errorf("model = %q", d.Model)
			}
			if err := Validate(d); err != nil {
				t.Errorf("not a valid decision: %v", err)
			}
			if tc.want != domain.ActionHold && (d.Size != domain.SizeS || d.Probability != DefaultMomentumProbability) {
				t.Errorf("entry %+v", d)
			}
		})
	}
}

func TestMomentumRejectsBadSnapshots(t *testing.T) {
	m, _ := NewMomentum(MomentumConfig{})
	for name, state := range map[string]string{
		"not json":      `nope`,
		"other version": `{"schema_version": 2}`,
		"no version":    `{}`,
	} {
		_, err := m.Decide(context.Background(), json.RawMessage(state))
		var de *domain.DecideError
		if err == nil || !errors.As(err, &de) || de.Reason != domain.ReasonInvalidAnswer {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// Wrapped, the same failures become HOLD with a reason.
	h := NewHoldOnFailure(m, WithNow(func() time.Time { return time.Time{} }))
	d, err := h.Decide(context.Background(), json.RawMessage(`nope`))
	if err != nil || d.Action != domain.ActionHold || d.HoldReason != domain.ReasonInvalidAnswer {
		t.Errorf("wrapped: %+v, %v", d, err)
	}
}

func TestNewMomentumValidatesConfig(t *testing.T) {
	for name, cfg := range map[string]MomentumConfig{
		"negative threshold": {ThresholdPct: -1},
		"probability over 1": {Probability: 1.5},
		"unknown size":       {Size: "XL"},
	} {
		if _, err := NewMomentum(cfg); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
