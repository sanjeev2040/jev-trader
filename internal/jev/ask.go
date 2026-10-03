package jev

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/domain"
)

// DefaultAskTimeout bounds an Ask whose context has no deadline. It is much
// longer than DefaultTimeout, which is tuned for the trading loop: Ask serves
// the pre-market screen, where nothing is waiting on a single answer.
const DefaultAskTimeout = 30 * time.Second

// Question types of POST /v1/systemone.
const (
	TypeChoice = "choice"
	TypeNoul   = "noul"
	TypeScore  = "score"
)

// Question is one typed question of an Ask. Build it with NoulQuestion,
// ScoreQuestion or ChoiceQuestion.
type Question struct {
	Type string `json:"type"`
	// Instructions is a string, or an object holding the question and the
	// data it refers to by name.
	Instructions any `json:"instructions"`
	Criteria     any `json:"criteria,omitempty"`
}

// NoulQuestion asks a yes/no question. yes and no describe the two ends of
// the 0..1 answer and may be empty.
func NoulQuestion(instructions any, yes, no string) Question {
	q := Question{Type: TypeNoul, Instructions: instructions}
	if yes != "" || no != "" {
		q.Criteria = map[string]string{"true": yes, "false": no}
	}
	return q
}

// ScoreQuestion rates the state along levels, ordered from the lowest to the
// highest. The answer lands between level 0 and len(levels)-1.
func ScoreQuestion(instructions any, levels ...string) Question {
	return Question{Type: TypeScore, Instructions: instructions, Criteria: levels}
}

// ChoiceQuestion picks one option; criteria maps each option to its meaning.
func ChoiceQuestion(instructions any, criteria map[string]string) Question {
	return Question{Type: TypeChoice, Instructions: instructions, Criteria: criteria}
}

// Answer is the answer to one question. Only the fields of its Type are set.
type Answer struct {
	Type string `json:"type"`
	// Noul is the yes probability, 0..1.
	Noul float64 `json:"noul"`
	// Score is the probability-weighted level, 0..len(levels)-1.
	Score float64 `json:"score"`
	// Choice is the most probable option.
	Choice string `json:"choice"`
	// Probabilities maps each option (choice) or level (score) to its
	// probability.
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    float64            `json:"confidence"`
}

// Answers is the response to an Ask.
type Answers struct {
	Model        string
	Answers      map[string]Answer
	InputTokens  int
	OutputTokens int
}

type askRequest struct {
	Model     string              `json:"model"`
	State     any                 `json:"state"`
	Questions map[string]Question `json:"questions"`
}

// Ask evaluates state against the typed questions in one request and returns
// every answer, validated against its question. It uses the same retries as
// Decide, under DefaultAskTimeout unless ctx has a deadline. Every failure is
// a *domain.DecideError.
func (c *Client) Ask(ctx context.Context, state any, questions map[string]Question) (Answers, error) {
	if len(questions) == 0 {
		return Answers{}, &domain.DecideError{Reason: domain.ReasonError, Err: errors.New("jev: no questions")}
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultAskTimeout)
		defer cancel()
	}
	body, err := json.Marshal(askRequest{Model: c.model, State: state, Questions: questions})
	if err != nil {
		return Answers{}, &domain.DecideError{Reason: domain.ReasonError, Err: err}
	}
	raw, attempts, err := c.exchange(ctx, body)
	if err != nil {
		return Answers{}, err
	}
	out, err := parseAnswers(raw, questions)
	if err != nil {
		return Answers{}, err
	}
	c.log.Info("jev ask", "requested_model", c.model, "model", out.Model, "questions", len(questions),
		"input_tokens", out.InputTokens, "output_tokens", out.OutputTokens, "attempts", attempts)
	return out, nil
}

// parseAnswers decodes a 200 response and checks each answer against the
// question it answers.
func parseAnswers(raw []byte, questions map[string]Question) (Answers, error) {
	var resp struct {
		Model   string                     `json:"model"`
		Answers map[string]json.RawMessage `json:"answers"`
		Usage   usage                      `json:"usage"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return Answers{}, invalid("decode response: %v", err)
	}
	out := Answers{Model: resp.Model, Answers: make(map[string]Answer, len(questions)),
		InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens}
	for id, q := range questions {
		rawAnswer, ok := resp.Answers[id]
		if !ok {
			return Answers{}, invalid("missing answer %q", id)
		}
		var a Answer
		if err := json.Unmarshal(rawAnswer, &a); err != nil {
			return Answers{}, invalid("%s: decode answer: %v", id, err)
		}
		if err := checkAnswer(id, q, a); err != nil {
			return Answers{}, err
		}
		out.Answers[id] = a
	}
	return out, nil
}

func checkAnswer(id string, q Question, a Answer) error {
	if a.Type != "" && a.Type != q.Type {
		return invalid("%s: answer type %q, want %q", id, a.Type, q.Type)
	}
	switch q.Type {
	case TypeNoul:
		if math.IsNaN(a.Noul) || a.Noul < 0 || a.Noul > 1 {
			return invalid("%s: noul %v out of range", id, a.Noul)
		}
	case TypeScore:
		levels, _ := q.Criteria.([]string)
		top := float64(len(levels) - 1)
		if math.IsNaN(a.Score) || a.Score < -probabilityTolerance || a.Score > top+probabilityTolerance {
			return invalid("%s: score %v outside 0..%v", id, a.Score, top)
		}
	case TypeChoice:
		opts, _ := q.Criteria.(map[string]string)
		if _, ok := opts[a.Choice]; !ok {
			return invalid("%s: unknown option %q", id, a.Choice)
		}
		sum := 0.0
		for o, p := range a.Probabilities {
			if _, ok := opts[o]; !ok {
				return invalid("%s: probability for unknown option %q", id, o)
			}
			if math.IsNaN(p) || p < 0 || p > 1 {
				return invalid("%s: probability %v for %q out of range", id, p, o)
			}
			sum += p
		}
		if math.Abs(sum-1) > probabilityTolerance {
			return invalid("%s: probabilities sum to %v", id, sum)
		}
	default:
		return invalid("%s: unsupported question type %q", id, q.Type)
	}
	return nil
}
