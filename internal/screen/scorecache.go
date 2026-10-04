package screen

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// scoreCache keeps what Jev answered about each (news item, company) pair for
// the screened day, so that the pass at 09:08 asks only about what appeared
// since the pass before it, and still has the early scores if Jev is down. An
// answer is filed under the exact request that earned it (the model, the state
// sent and the questions asked), so a changed question, or the same item asked
// in the anonymised form, is a different entry and is never mistaken for it.
type scoreCache struct {
	path      string
	m         map[string]scored
	dirty     bool
	anonymize bool
}

type cachedScore struct {
	Impact       float64 `json:"impact"`
	Material     float64 `json:"material"`
	Direction    float64 `json:"direction"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
}

func openScoreCache(dir, date string, anonymize bool) *scoreCache {
	c := &scoreCache{path: filepath.Join(dir, "screen", "news-"+date+".json"), m: map[string]scored{}, anonymize: anonymize}
	body, err := os.ReadFile(c.path)
	if err != nil {
		return c
	}
	var raw map[string]cachedScore
	if json.Unmarshal(body, &raw) != nil {
		return c // a damaged cache is only a cache
	}
	for k, v := range raw {
		c.m[k] = scored{Impact: v.Impact, Material: v.Material, Direction: v.Direction, InputTokens: v.InputTokens, OutputTokens: v.OutputTokens}
	}
	return c
}

// requestKey identifies the exact request about a pair under a model: another
// model's answers do not count either.
func (c *scoreCache) requestKey(model string, p pair) string {
	body, _ := json.Marshal([]any{model, p.request(c.anonymize), newsQuestions()}) // maps marshal in key order
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:16])
}

// get returns the cached scores for the pair, attached to it.
func (c *scoreCache) get(model string, p pair) (scored, bool) {
	s, ok := c.m[c.requestKey(model, p)]
	if !ok {
		return scored{}, false
	}
	s.pair = p
	return s, true
}

func (c *scoreCache) put(model string, s scored) {
	c.m[c.requestKey(model, s.pair)] = s
	c.dirty = true
}

func (c *scoreCache) save() error {
	if !c.dirty {
		return nil
	}
	raw := make(map[string]cachedScore, len(c.m))
	for k, v := range c.m {
		raw[k] = cachedScore{Impact: v.Impact, Material: v.Material, Direction: v.Direction, InputTokens: v.InputTokens, OutputTokens: v.OutputTokens}
	}
	body, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(c.path), ".news-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return errors.Join(err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	c.dirty = false
	return os.Rename(tmp.Name(), c.path)
}
