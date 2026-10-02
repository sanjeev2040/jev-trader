package jev

import "encoding/json"

// Wire types for POST /v1/systemone. The request is {state, model, questions};
// a choice answer carries the chosen option, per-option probabilities and a
// confidence (https://docs.typesafe.ai/api.md).

type request struct {
	Model     string          `json:"model"`
	State     json.RawMessage `json:"state"`
	Questions []question      `json:"questions"`
}

type question struct {
	ID      string   `json:"id"`
	Type    string   `json:"type"`
	Prompt  string   `json:"prompt"`
	Options []string `json:"options"`
}

type response struct {
	Model   string   `json:"model"`
	Answers []answer `json:"answers"`
	Usage   usage    `json:"usage"`
}

type answer struct {
	ID            string             `json:"id"`
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

var questions = []question{
	{
		ID:      questionAction,
		Type:    typeChoice,
		Prompt:  "Given this market state for one instrument, which action should be taken now?",
		Options: []string{"BUY", "SELL", "HOLD"},
	},
	{
		ID:      questionSize,
		Type:    typeChoice,
		Prompt:  "If the action is taken, which position size bucket fits it?",
		Options: []string{"S", "M", "L"},
	},
}
