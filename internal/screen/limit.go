package screen

import (
	"context"

	"golang.org/x/time/rate"

	"github.com/sanjeev2040/jev-trader/internal/jev"
)

// limitedAsker spaces the calls of an Asker.
type limitedAsker struct {
	inner   Asker
	limiter *rate.Limiter
}

// NewLimitedAsker returns an Asker that makes at most perSecond calls a second
// however many goroutines use it; a long study scores tens of thousands of
// items and should not meet the service's rate limit. perSecond of zero or
// less returns a unchanged.
func NewLimitedAsker(a Asker, perSecond float64) Asker {
	if perSecond <= 0 {
		return a
	}
	return &limitedAsker{inner: a, limiter: rate.NewLimiter(rate.Limit(perSecond), 1)}
}

func (l *limitedAsker) Ask(ctx context.Context, state any, questions map[string]jev.Question) (jev.Answers, error) {
	if err := l.limiter.Wait(ctx); err != nil {
		return jev.Answers{}, err
	}
	return l.inner.Ask(ctx, state, questions)
}
