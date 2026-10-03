package jev

import "encoding/json"

// Wire types for POST /v1/systemone (https://docs.typesafe.ai/api.md). The
// request is {model, state, questions}, where questions is a map keyed by a
// caller-chosen id. The response is {model, answers, usage}, where answers is
// a map keyed by the same ids. A choice answer carries the chosen option,
// per-option probabilities and a confidence.

type request struct {
	Model     string              `json:"model"`
	State     json.RawMessage     `json:"state"`
	Questions map[string]question `json:"questions"`
}

type question struct {
	Type         string       `json:"type"`
	Instructions instructions `json:"instructions"`
	// Criteria maps each option to its description.
	Criteria map[string]criterion `json:"criteria"`
}

// instructions is the structured form of a question's instructions.
type instructions struct {
	Question string `json:"question"`
	Focus    string `json:"focus,omitempty"`
	Inspect  string `json:"inspect,omitempty"`
}

// criterion describes one choice option.
type criterion struct {
	What     string   `json:"what"`
	NotFor   string   `json:"not_for"`
	Examples []string `json:"examples,omitempty"`
}

type response struct {
	Model   string            `json:"model"`
	Answers map[string]answer `json:"answers"`
	Usage   usage             `json:"usage"`
}

type answer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    float64            `json:"confidence"`
}

type usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

const (
	questionAction = "action"
	questionSize   = "size"
	typeChoice     = "choice"
)

// Option sets, in the order they are documented to the model.
var (
	actionOptions = []string{"BUY", "SELL", "HOLD"}
	sizeOptions   = []string{"S", "M", "L"}
)

var questions = map[string]question{
	questionAction: {
		Type: typeChoice,
		Instructions: instructions{
			Question: "Given this market state for one instrument, which action should be taken now?",
			Focus:    "short-term momentum, volume and VWAP distance against the current position and the index",
			Inspect:  "features, position and index in the state",
		},
		Criteria: map[string]criterion{
			"BUY": {
				What:     "Open or add to a long position now.",
				NotFor:   "Unclear or conflicting signals, or when a long position is already held and the signals have not strengthened.",
				Examples: []string{"positive short-term returns with above-average volume and price above VWAP"},
			},
			"SELL": {
				What:     "Reduce or close a long position, or open a short position, now.",
				NotFor:   "Unclear or conflicting signals, or a brief pullback inside an otherwise rising move.",
				Examples: []string{"negative short-term returns with above-average volume and price below VWAP"},
			},
			"HOLD": {
				What:     "Take no action now.",
				NotFor:   "A clear, strong signal in one direction.",
				Examples: []string{"flat returns, thin volume, or mixed signals across timeframes"},
			},
		},
	},
	questionSize: {
		Type: typeChoice,
		Instructions: instructions{
			Question: "If the action is taken, which position size bucket fits it?",
			Focus:    "how strong and how consistent the signals are",
			Inspect:  "features in the state",
		},
		Criteria: map[string]criterion{
			"S": {
				What:     "A small position.",
				NotFor:   "Strong, consistent signals across timeframes.",
				Examples: []string{"a weak or mixed signal"},
			},
			"M": {
				What:     "A medium position.",
				NotFor:   "Weak signals, or exceptionally strong ones.",
				Examples: []string{"a clear signal on one timeframe"},
			},
			"L": {
				What:     "A large position.",
				NotFor:   "Weak, mixed or low-volume signals.",
				Examples: []string{"a strong signal that agrees across timeframes with high volume"},
			},
		},
	},
}
