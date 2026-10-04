package replay

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/decide"
)

// TestWalkthroughShowsTheStopExit replays the fixture tape (up to 1523, then
// down to 1494) with a 1% stop and an hour's minimum hold: the momentum run's
// long is sold by the stop on the way down, and the walkthrough names the exit
// and the numbers behind it, before the minimum hold would have let it go.
func TestWalkthroughShowsTheStopExit(t *testing.T) {
	cfg := config(t, writeFixture(t), openLog(t))
	cfg.Limits.StopLossPct = 0.01
	cfg.Limits.MinHold, cfg.Limits.RotationMinHold = time.Hour, time.Hour
	cfg.Walkthrough = WalkActions
	rep, err := Run(bg, cfg, []Spec{{Name: "momentum", Decider: momentum(t)}})
	if err != nil {
		t.Fatal(err)
	}
	iw := walkOf(rep, "momentum").Instruments[0]
	var exit *WalkRow
	for i, r := range iw.Rows {
		if r.Kind == RowRiskExit {
			exit = &iw.Rows[i]
		}
	}
	if exit == nil {
		t.Fatalf("no risk exit in the walk: %+v", iw.Rows)
	}
	if exit.ExitReason != "stop_loss" || exit.Verdict != "stop_loss" || exit.Order == nil || exit.Order.Side != "SELL" ||
		!strings.Contains(exit.VerdictDetail, "is through the") || !strings.Contains(exit.VerdictDetail, "stop") {
		t.Errorf("exit row = %+v", exit)
	}
	if iw.Position != 0 || rep.Runs[0].RiskExits != 1 {
		t.Errorf("position %d, risk exits %d; want flat after the one stop", iw.Position, rep.Runs[0].RiskExits)
	}

	var text bytes.Buffer
	if err := rep.WriteText(&text); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"EXIT   risk: stop_loss @", "gate stop_loss (last price", "the minimum hold does not apply"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("text report lacks %q:\n%s", want, text.String())
		}
	}
}

func TestWalkthroughShowsTheTimeExit(t *testing.T) {
	cfg := config(t, writeFixture(t), openLog(t))
	cfg.Limits.MaxHold = 6 * time.Minute
	cfg.Limits.MinHold, cfg.Limits.RotationMinHold = time.Hour, time.Hour
	cfg.Walkthrough = WalkActions
	rep, err := Run(bg, cfg, []Spec{{Name: "momentum", Decider: momentum(t)}, {Name: "hold", Decider: decide.AlwaysHold{}}})
	if err != nil {
		t.Fatal(err)
	}
	var text bytes.Buffer
	if err := rep.WriteText(&text); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"EXIT   risk: time_exit @", "gate time_exit (held", "maximum is 6m0s"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("text report lacks %q:\n%s", want, text.String())
		}
	}
}
