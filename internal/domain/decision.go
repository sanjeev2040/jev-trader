// Package domain holds the small value types shared across jev-trader's
// packages. It has no logic and imports only the standard library.
package domain

import (
	"encoding/json"
	"time"
)

// Action is the trading action chosen for one decision cycle.
type Action string

const (
	ActionBuy  Action = "BUY"
	ActionSell Action = "SELL"
	ActionHold Action = "HOLD"
)

// Size buckets for a decision. The risk gate maps a bucket to a quantity.
const (
	SizeS = "S"
	SizeM = "M"
	SizeL = "L"
)

// Reason codes carried by DecideError and by a substituted HOLD.
const (
	ReasonTimeout       = "timeout"
	ReasonHTTP4xx       = "http_4xx"
	ReasonHTTP5xx       = "http_5xx"
	ReasonRateLimited   = "rate_limited"
	ReasonInvalidAnswer = "invalid_answer"
	ReasonCircuitOpen   = "circuit_open"
	// ReasonError covers failures that fit no other code, such as a refused
	// connection or a Decider returning an untyped error.
	ReasonError = "error"
)

// Decision is one typed answer from a decider.
type Decision struct {
	Action Action
	// Size is the chosen size bucket: SizeS, SizeM or SizeL. It is empty on a
	// substituted HOLD.
	Size string
	// Probability is the probability the decider assigned to Action.
	Probability float64
	// Probabilities is the full distribution over actions.
	Probabilities map[Action]float64
	// SizeProbabilities is the full distribution over size buckets.
	SizeProbabilities map[string]float64
	// Confidence is the decider's confidence in the action answer.
	Confidence float64
	// Model is the model id echoed by the server.
	Model        string
	InputTokens  int
	OutputTokens int
	Latency      time.Duration
	// HoldReason is set to a Reason code when the action is a HOLD substituted
	// for a failure. It is empty for a decision the decider actually made.
	HoldReason string
	// Raw is the decider's raw answer, kept for the decision log.
	Raw json.RawMessage
}

// DecideError is a failed decision with a reason code.
type DecideError struct {
	Reason string
	Err    error
}

func (e *DecideError) Error() string {
	if e.Err == nil {
		return "decide: " + e.Reason
	}
	return "decide: " + e.Reason + ": " + e.Err.Error()
}

func (e *DecideError) Unwrap() error { return e.Err }
