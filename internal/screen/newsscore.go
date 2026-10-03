package screen

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/jev"
	"github.com/sanjeev2040/jev-trader/internal/news"
	"github.com/sanjeev2040/jev-trader/internal/universe"
)

// Asker is the part of the Jev client the news scoring uses.
type Asker interface {
	Ask(ctx context.Context, state any, questions map[string]jev.Question) (jev.Answers, error)
}

// Question ids of the news scoring.
const (
	qImpact    = "impact"
	qMaterial  = "material"
	qDirection = "direction"
)

// Directions Jev chooses between.
const (
	dirUp      = "UP"
	dirDown    = "DOWN"
	dirNeutral = "NEUTRAL"
)

// impactLevels are the levels of the impact Score question, lowest first.
var impactLevels = []string{
	"No effect: routine, procedural, already public or not about the company's own business.",
	"Minor: the share price would move under 1% on it.",
	"Moderate: the share price would move about 1-2%.",
	"Large: the share price would move about 2-4%.",
	"Very large: the share price would move over 4%, for example a major order win, a results surprise, an acquisition, a regulatory action or fraud.",
}

// maxImpact is the top level of impactLevels.
const maxImpact = 4

func newsQuestions() map[string]jev.Question {
	return map[string]jev.Question{
		qImpact: jev.ScoreQuestion(
			"How large a move in this company's share price during today's trading session, in either direction, does this news justify?",
			impactLevels...),
		qMaterial: jev.NoulQuestion(
			"Is this news mainly about this company, and material enough to move its share price during today's session, rather than routine, procedural or already priced in?",
			"About this company and price-moving: new information a trader would act on.",
			"Routine, procedural, already known, only mentioning this company in passing, or not about its business."),
		qDirection: jev.ChoiceQuestion(
			"Which way will this news push this company's share price during today's session?",
			map[string]string{
				dirUp:      "The news is good for the share price: it should rise.",
				dirDown:    "The news is bad for the share price: it should fall.",
				dirNeutral: "No clear effect on the share price, or the effect is unclear.",
			}),
	}
}

// pair is a news item and the stock it is about.
type pair struct {
	item  news.Item
	stock universe.Stock
}

// scored is a pair with what Jev answered.
type scored struct {
	pair
	// Impact is the 0..4 level; Material the probability the item is
	// price-moving; Direction is P(up) - P(down), in [-1, 1].
	Impact, Material, Direction float64
	// InputTokens and OutputTokens are what the call cost.
	InputTokens, OutputTokens int
}

// strength is the item's signed effect before ageing: the direction, scaled
// by the geometric mean of how material it is and how large the impact.
func (s scored) strength() float64 {
	return s.Direction * math.Sqrt(clamp(s.Material, 0, 1)*clamp(s.Impact/maxImpact, 0, 1))
}

func (p pair) state() map[string]any {
	st := map[string]any{
		"company":   p.stock.Name,
		"sector":    p.stock.Industry,
		"source":    p.item.Source,
		"published": p.item.Published.In(news.IST).Format("2006-01-02 15:04 IST"),
		"headline":  p.item.Title,
	}
	if p.item.Category != "" {
		st["filing_category"] = p.item.Category
	}
	if p.item.Summary != "" {
		st["details"] = p.item.Summary
	}
	return st
}

// scoreOne asks Jev the three news questions about one pair.
func scoreOne(ctx context.Context, a Asker, p pair) (scored, error) {
	ans, err := a.Ask(ctx, p.state(), newsQuestions())
	if err != nil {
		return scored{}, err
	}
	dir := ans.Answers[qDirection].Probabilities
	return scored{
		pair:        p,
		Impact:      ans.Answers[qImpact].Score,
		Material:    ans.Answers[qMaterial].Noul,
		Direction:   dir[dirUp] - dir[dirDown],
		InputTokens: ans.InputTokens, OutputTokens: ans.OutputTokens,
	}, nil
}

// failFast is how many failures with no success at all end the news scoring:
// a wrong key or a down service fails every call the same way.
const failFast = 5

// scoreAll scores every pair with up to concurrency calls in flight. A pair
// whose call fails is left out and counted; if the first failFast calls all
// fail it stops and returns the error. Results keep the order of pairs.
func scoreAll(ctx context.Context, a Asker, pairs []pair, concurrency int) (out []scored, failed int, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make([]*scored, len(pairs))
	var (
		wg           sync.WaitGroup
		okN, failN   atomic.Int32
		mu           sync.Mutex
		firstFailure error
	)
	next := make(chan int)
	for range min(concurrency, max(len(pairs), 1)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				s, err := scoreOne(ctx, a, pairs[i])
				if err != nil {
					failN.Add(1)
					mu.Lock()
					if firstFailure == nil {
						firstFailure = err
					}
					mu.Unlock()
					if okN.Load() == 0 && failN.Load() >= failFast {
						cancel()
					}
					continue
				}
				okN.Add(1)
				results[i] = &s
			}
		}()
	}
feed:
	for i := range pairs {
		select {
		case next <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(next)
	wg.Wait()

	for _, r := range results {
		if r != nil {
			out = append(out, *r)
		}
	}
	failed = len(pairs) - len(out)
	if len(out) == 0 && len(pairs) > 0 {
		if firstFailure == nil {
			firstFailure = errors.New("cancelled")
		}
		return nil, failed, fmt.Errorf("jev scored none of %d news items: %w", len(pairs), firstFailure)
	}
	return out, failed, nil
}

// recency is the weight of an item of the given age: 1 when it just
// appeared, halving every halfLife.
func recency(age, halfLife time.Duration) float64 {
	if age <= 0 {
		return 1
	}
	return math.Pow(0.5, float64(age)/float64(halfLife))
}
