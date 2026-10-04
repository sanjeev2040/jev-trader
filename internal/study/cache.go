package study

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/decide"
	"github.com/sanjeev2040/jev-trader/internal/domain"
)

// Usage is what the Jev decider cost over some replays. Tokens are counted
// only for calls that were paid for; a call answered from the store is a cache
// hit and its tokens are what the hit saved.
type Usage struct {
	Calls             int `json:"calls"` // paid calls
	CacheHits         int `json:"cache_hits"`
	InputTokens       int `json:"input_tokens"`
	OutputTokens      int `json:"output_tokens"`
	SavedInputTokens  int `json:"saved_input_tokens"`
	SavedOutputTokens int `json:"saved_output_tokens"`
}

// Add accumulates o into u.
func (u *Usage) Add(o Usage) {
	u.Calls += o.Calls
	u.CacheHits += o.CacheHits
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.SavedInputTokens += o.SavedInputTokens
	u.SavedOutputTokens += o.SavedOutputTokens
}

// storedAnswer is a decision as the store keeps it, one JSON object a line.
type storedAnswer struct {
	Key               string             `json:"k"`
	Action            domain.Action      `json:"action"`
	Size              string             `json:"size,omitempty"`
	Probability       float64            `json:"probability"`
	Probabilities     map[string]float64 `json:"probabilities,omitempty"`
	SizeProbabilities map[string]float64 `json:"size_probabilities,omitempty"`
	Confidence        float64            `json:"confidence"`
	Model             string             `json:"model,omitempty"`
	InputTokens       int                `json:"input_tokens,omitempty"`
	OutputTokens      int                `json:"output_tokens,omitempty"`
	LatencyNS         int64              `json:"latency_ns"`
	Raw               json.RawMessage    `json:"raw,omitempty"`
}

func (a storedAnswer) decision() domain.Decision {
	d := domain.Decision{
		Action: a.Action, Size: a.Size, Probability: a.Probability, Confidence: a.Confidence,
		SizeProbabilities: a.SizeProbabilities, Model: a.Model,
		InputTokens: a.InputTokens, OutputTokens: a.OutputTokens, Latency: time.Duration(a.LatencyNS), Raw: a.Raw,
		Probabilities: make(map[domain.Action]float64, len(a.Probabilities)),
	}
	for k, p := range a.Probabilities {
		d.Probabilities[domain.Action(k)] = p
	}
	return d
}

func storeAnswer(key string, d domain.Decision) storedAnswer {
	a := storedAnswer{
		Key: key, Action: d.Action, Size: d.Size, Probability: d.Probability, Confidence: d.Confidence,
		SizeProbabilities: d.SizeProbabilities, Model: d.Model,
		InputTokens: d.InputTokens, OutputTokens: d.OutputTokens, LatencyNS: int64(d.Latency), Raw: d.Raw,
		Probabilities: make(map[string]float64, len(d.Probabilities)),
	}
	for k, p := range d.Probabilities {
		a.Probabilities[string(k)] = p
	}
	return a
}

// AnswerStore keeps every answer Jev gave, by the model and the exact state it
// was asked about, in an append-only file. The same snapshot asked again, in
// a rerun of a day or of the whole study, costs nothing and answers the same.
type AnswerStore struct {
	mu sync.Mutex
	m  map[string]storedAnswer
	f  *os.File
}

// OpenAnswerStore reads the store at path, creating it if it does not exist. A
// damaged line (a write cut short) is skipped: it is only a cache.
func OpenAnswerStore(path string) (*AnswerStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	s := &AnswerStore{m: map[string]storedAnswer{}}
	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(nil, 4<<20)
		for sc.Scan() {
			var a storedAnswer
			if json.Unmarshal(sc.Bytes(), &a) == nil && a.Key != "" {
				s.m[a.Key] = a
			}
		}
		f.Close()
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	s.f = f
	return s, nil
}

// Len is how many answers the store holds.
func (s *AnswerStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

func (s *AnswerStore) get(key string) (domain.Decision, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.m[key]
	if !ok {
		return domain.Decision{}, false
	}
	return a.decision(), true
}

func (s *AnswerStore) put(key string, d domain.Decision) error {
	a := storeAnswer(key, d)
	line, err := json.Marshal(a)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = a
	_, err = s.f.Write(append(line, '\n'))
	return err
}

// Close closes the file.
func (s *AnswerStore) Close() error { return s.f.Close() }

// answerKey identifies a question: another model's answer does not count.
func answerKey(model string, state json.RawMessage) string {
	h := sha256.New()
	h.Write([]byte(model))
	h.Write([]byte{0})
	h.Write(state)
	return hex.EncodeToString(h.Sum(nil))
}

// CachedDecider answers from the store when it can and asks Inner, then
// stores the answer, when it cannot. A failed call is not stored. It counts
// what each path cost.
type CachedDecider struct {
	Inner decide.Decider
	// Model is the pinned model id the answers are keyed under.
	Model string
	Store *AnswerStore

	mu    sync.Mutex
	usage Usage
}

var _ decide.Decider = (*CachedDecider)(nil)

// Decide implements decide.Decider.
func (c *CachedDecider) Decide(ctx context.Context, state json.RawMessage) (domain.Decision, error) {
	key := answerKey(c.Model, state)
	if d, ok := c.Store.get(key); ok {
		c.mu.Lock()
		c.usage.CacheHits++
		c.usage.SavedInputTokens += d.InputTokens
		c.usage.SavedOutputTokens += d.OutputTokens
		c.mu.Unlock()
		return d, nil
	}
	d, err := c.Inner.Decide(ctx, state)
	if err != nil {
		return d, err
	}
	if err := c.Store.put(key, d); err != nil {
		return d, fmt.Errorf("study: store Jev answer: %w", err)
	}
	c.mu.Lock()
	c.usage.Calls++
	c.usage.InputTokens += d.InputTokens
	c.usage.OutputTokens += d.OutputTokens
	c.mu.Unlock()
	return d, nil
}

// TakeUsage returns what was counted since the last call and starts again.
func (c *CachedDecider) TakeUsage() Usage {
	c.mu.Lock()
	defer c.mu.Unlock()
	u := c.usage
	c.usage = Usage{}
	return u
}
