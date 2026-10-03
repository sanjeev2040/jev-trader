package risk

import (
	"time"

	"github.com/sanjeev2040/jev-trader/internal/domain"
)

// Source says what originated an approved order.
type Source string

// Sources of an approved order.
const (
	SourceDecision Source = "jev"  // a decider's answer the gate approved
	SourceRisk     Source = "risk" // a risk-driven exit: square-off, loss limit or kill switch
)

// Approval is the gate's permission to place exactly one order. Only package
// risk can mint a valid Approval: its only field is unexported, so a value
// built anywhere else is the zero Approval, which is never valid. The order
// manager must accept nothing but an Approval and must refuse one for which
// Valid reports false.
type Approval struct {
	a *approval
}

type approval struct {
	intent domain.OrderIntent
	source Source
	reason Reason
	at     time.Time
}

func newApproval(intent domain.OrderIntent, source Source, reason Reason, at time.Time) Approval {
	return Approval{&approval{intent: intent, source: source, reason: reason, at: at}}
}

// Valid reports whether the gate minted a.
func (a Approval) Valid() bool { return a.a != nil }

// Intent returns the approved order. It is the zero intent if a is not valid.
func (a Approval) Intent() domain.OrderIntent {
	if a.a == nil {
		return domain.OrderIntent{}
	}
	return a.a.intent
}

// Source returns what originated the order.
func (a Approval) Source() Source {
	if a.a == nil {
		return ""
	}
	return a.a.source
}

// Reason returns ReasonApproved for a decision-driven order, or the exit
// reason for a risk-driven exit.
func (a Approval) Reason() Reason {
	if a.a == nil {
		return ""
	}
	return a.a.reason
}

// ApprovedAt returns the time the gate approved the order.
func (a Approval) ApprovedAt() time.Time {
	if a.a == nil {
		return time.Time{}
	}
	return a.a.at
}
