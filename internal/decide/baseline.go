package decide

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/sanjeev2040/jev-trader/internal/domain"
)

// Baseline model ids, echoed in Decision.Model so the decision log tells a
// baseline's answers from Jev's.
const (
	ModelAlwaysHold = "baseline-always-hold"
	ModelMomentum   = "baseline-momentum"
)

// snapshotSchemaVersion is the snapshot schema the momentum baseline reads.
// decide stays free of a snapshot import, as the architecture requires, so
// the baseline decodes the few fields it needs and refuses any other version.
// A test keeps the field names in step with package snapshot.
const snapshotSchemaVersion = 1

// AlwaysHold is the do-nothing baseline: it answers HOLD to every snapshot.
type AlwaysHold struct{}

var _ Decider = AlwaysHold{}

// Decide returns HOLD with certainty.
func (AlwaysHold) Decide(context.Context, json.RawMessage) (domain.Decision, error) {
	return domain.Decision{
		Action:        domain.ActionHold,
		Probability:   1,
		Probabilities: map[domain.Action]float64{domain.ActionHold: 1},
		Confidence:    1,
		Model:         ModelAlwaysHold,
	}, nil
}

// MomentumConfig tunes the Momentum baseline.
type MomentumConfig struct {
	// ThresholdPct is the 5-bar return, in percent, that opens a position.
	// Zero means DefaultMomentumThresholdPct.
	ThresholdPct float64
	// Probability is the probability the baseline reports for a BUY or SELL.
	// The risk gate's probability threshold applies to it like to any other
	// decider. Zero means DefaultMomentumProbability.
	Probability float64
	// Size is the size bucket of an entry. Empty means SizeS.
	Size string
}

// Defaults for the Momentum baseline.
const (
	DefaultMomentumThresholdPct = 0.10
	DefaultMomentumProbability  = 0.65
)

// Momentum is a simple trend-following baseline on the 5-bar percentage
// return in the snapshot:
//
//   - flat: BUY when the return is above +ThresholdPct, SELL when it is below
//     -ThresholdPct (the risk gate refuses a SELL while shorts are disabled);
//   - long: SELL, which closes the position, once the return turns negative;
//   - short: BUY, which closes the position, once the return turns positive;
//   - otherwise HOLD, and always HOLD while the 5-bar return is unknown.
type Momentum struct {
	cfg MomentumConfig
}

var _ Decider = (*Momentum)(nil)

// NewMomentum returns a Momentum baseline. Out-of-range settings are
// rejected rather than silently replaced.
func NewMomentum(cfg MomentumConfig) (*Momentum, error) {
	if cfg.ThresholdPct == 0 {
		cfg.ThresholdPct = DefaultMomentumThresholdPct
	}
	if cfg.Probability == 0 {
		cfg.Probability = DefaultMomentumProbability
	}
	if cfg.Size == "" {
		cfg.Size = domain.SizeS
	}
	switch {
	case !(cfg.ThresholdPct > 0) || math.IsInf(cfg.ThresholdPct, 0):
		return nil, fmt.Errorf("decide: momentum threshold %v must be a positive number", cfg.ThresholdPct)
	case !(cfg.Probability > 0 && cfg.Probability <= 1):
		return nil, fmt.Errorf("decide: momentum probability %v must be in (0, 1]", cfg.Probability)
	case cfg.Size != domain.SizeS && cfg.Size != domain.SizeM && cfg.Size != domain.SizeL:
		return nil, fmt.Errorf("decide: momentum size %q is not S, M or L", cfg.Size)
	}
	return &Momentum{cfg: cfg}, nil
}

// momentumState is the part of the snapshot the baseline reads.
type momentumState struct {
	SchemaVersion int `json:"schema_version"`
	Features      struct {
		ReturnsPct struct {
			Bars5 *float64 `json:"bars_5"`
		} `json:"returns_pct"`
	} `json:"features"`
	Position struct {
		Side string `json:"side"`
	} `json:"position"`
}

// Decide applies the momentum rule to an encoded snapshot.
func (m *Momentum) Decide(_ context.Context, state json.RawMessage) (domain.Decision, error) {
	var s momentumState
	if err := json.Unmarshal(state, &s); err != nil {
		return domain.Decision{}, &domain.DecideError{Reason: domain.ReasonInvalidAnswer, Err: fmt.Errorf("momentum: decode snapshot: %w", err)}
	}
	if s.SchemaVersion != snapshotSchemaVersion {
		return domain.Decision{}, &domain.DecideError{Reason: domain.ReasonInvalidAnswer, Err: fmt.Errorf("momentum: snapshot schema version %d, want %d", s.SchemaVersion, snapshotSchemaVersion)}
	}

	action := domain.ActionHold
	if r := s.Features.ReturnsPct.Bars5; r != nil {
		th := m.cfg.ThresholdPct
		switch s.Position.Side {
		case "LONG":
			if *r < 0 {
				action = domain.ActionSell
			}
		case "SHORT":
			if *r > 0 {
				action = domain.ActionBuy
			}
		default:
			switch {
			case *r > th:
				action = domain.ActionBuy
			case *r < -th:
				action = domain.ActionSell
			}
		}
	}
	if action == domain.ActionHold {
		d, _ := AlwaysHold{}.Decide(context.Background(), nil)
		d.Model = ModelMomentum
		return d, nil
	}
	p := m.cfg.Probability
	return domain.Decision{
		Action:            action,
		Size:              m.cfg.Size,
		Probability:       p,
		Probabilities:     map[domain.Action]float64{action: p, domain.ActionHold: 1 - p},
		SizeProbabilities: map[string]float64{m.cfg.Size: 1},
		Confidence:        p,
		Model:             ModelMomentum,
	}, nil
}
