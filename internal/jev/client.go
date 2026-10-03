// Package jev is a typed HTTP client for Jev's POST /v1/systemone endpoint.
//
// It asks two choice questions, action ∈ {BUY, SELL, HOLD} and size ∈ {S, M, L},
// keyed by id in the request's questions map, and returns the answers as a domain.Decision. Every failure is returned as a
// *domain.DecideError carrying a reason code.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/domain"
)

const (
	DefaultBaseURL = "https://api.typesafe.ai"
	// DefaultModel is the pinned System One model used when none is configured.
	DefaultModel       = "jev-1.13.0"
	DefaultTimeout     = 800 * time.Millisecond
	DefaultMaxAttempts = 3
	DefaultMinBackoff  = 50 * time.Millisecond
	DefaultMaxBackoff  = time.Second

	endpointPath = "/v1/systemone"
	// statusOverloaded is Jev's "overloaded" status.
	statusOverloaded = 529
	// probabilityTolerance is how far a distribution's sum may stray from 1.
	probabilityTolerance = 0.02
	maxResponseBytes     = 1 << 20
)

// Config configures a Client. APIKey is required.
type Config struct {
	// BaseURL defaults to DefaultBaseURL.
	BaseURL string
	// APIKey is sent as a bearer token. Callers read it from JEV_API_KEY.
	APIKey string
	// Model is an explicit, pinned model id. It defaults to DefaultModel;
	// "jev-latest" is rejected so a server-side model upgrade is always a
	// visible config change.
	Model string
	// Timeout bounds a call whose context has no deadline. Defaults to
	// DefaultTimeout.
	Timeout time.Duration
	// MaxAttempts caps attempts per call, including retries of 429 and 529.
	// Defaults to DefaultMaxAttempts.
	MaxAttempts int
	// MinBackoff and MaxBackoff bound the wait between attempts. A server
	// Retry-After is honored up to MaxBackoff.
	MinBackoff time.Duration
	MaxBackoff time.Duration
	// HTTPClient defaults to a new http.Client.
	HTTPClient *http.Client
	// Logger receives the response model id and token usage of each call.
	// Defaults to slog.Default().
	Logger *slog.Logger
}

// Client calls Jev. It is safe for concurrent use.
type Client struct {
	url         string
	apiKey      string
	model       string
	timeout     time.Duration
	maxAttempts int
	minBackoff  time.Duration
	maxBackoff  time.Duration
	http        *http.Client
	log         *slog.Logger
}

// New validates cfg and returns a Client.
func New(cfg Config) (*Client, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("jev: API key is required")
	}
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	if cfg.Model == "jev-latest" {
		return nil, errors.New("jev: pin an explicit model id, not jev-latest")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = DefaultMaxAttempts
	}
	if cfg.MinBackoff <= 0 {
		cfg.MinBackoff = DefaultMinBackoff
	}
	if cfg.MaxBackoff < cfg.MinBackoff {
		cfg.MaxBackoff = max(DefaultMaxBackoff, cfg.MinBackoff)
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Client{
		url:         strings.TrimRight(cfg.BaseURL, "/") + endpointPath,
		apiKey:      cfg.APIKey,
		model:       cfg.Model,
		timeout:     cfg.Timeout,
		maxAttempts: cfg.MaxAttempts,
		minBackoff:  cfg.MinBackoff,
		maxBackoff:  cfg.MaxBackoff,
		http:        cfg.HTTPClient,
		log:         cfg.Logger,
	}, nil
}

// Decide sends the encoded snapshot as Jev's state and returns its decision.
// It retries 429 and 529 only while the call's deadline still has room.
func (c *Client) Decide(ctx context.Context, state json.RawMessage) (domain.Decision, error) {
	start := time.Now()
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	if !json.Valid(state) {
		return domain.Decision{}, &domain.DecideError{Reason: domain.ReasonError, Err: errors.New("jev: state is not valid JSON")}
	}
	body, err := json.Marshal(request{Model: c.model, State: state, Questions: questions})
	if err != nil {
		return domain.Decision{}, &domain.DecideError{Reason: domain.ReasonError, Err: err}
	}

	for attempt := 1; ; attempt++ {
		status, raw, retryAfter, err := c.post(ctx, body)
		if err != nil {
			return domain.Decision{}, transportError(ctx, err)
		}
		if status == http.StatusOK {
			d, err := parse(raw)
			if err != nil {
				return domain.Decision{}, err
			}
			d.Latency = time.Since(start)
			c.log.Info("jev answer", "requested_model", c.model, "model", d.Model,
				"input_tokens", d.InputTokens, "output_tokens", d.OutputTokens, "attempts", attempt)
			return d, nil
		}
		derr := statusError(status, raw)
		if status != http.StatusTooManyRequests && status != statusOverloaded {
			return domain.Decision{}, derr
		}
		if attempt >= c.maxAttempts {
			return domain.Decision{}, derr
		}
		wait := c.backoff(attempt, retryAfter)
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= wait {
			// No room left for another attempt within the deadline.
			return domain.Decision{}, derr
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return domain.Decision{}, transportError(ctx, ctx.Err())
		case <-t.C:
		}
	}
}

func (c *Client) post(ctx context.Context, body []byte) (status int, raw []byte, retryAfter time.Duration, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, 0, err
	}
	defer resp.Body.Close()
	raw, err = io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return 0, nil, 0, err
	}
	return resp.StatusCode, raw, parseRetryAfter(resp.Header.Get("Retry-After")), nil
}

// backoff doubles from MinBackoff per attempt, raised to a server Retry-After,
// and capped at MaxBackoff.
func (c *Client) backoff(attempt int, retryAfter time.Duration) time.Duration {
	wait := c.minBackoff << (attempt - 1)
	if wait <= 0 || wait > c.maxBackoff {
		wait = c.maxBackoff
	}
	if retryAfter > wait {
		wait = min(retryAfter, c.maxBackoff)
	}
	return wait
}

func parseRetryAfter(v string) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || secs < 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

func transportError(ctx context.Context, err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &domain.DecideError{Reason: domain.ReasonTimeout, Err: err}
	}
	var netErr interface{ Timeout() bool }
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &domain.DecideError{Reason: domain.ReasonTimeout, Err: err}
	}
	return &domain.DecideError{Reason: domain.ReasonError, Err: err}
}

func statusError(status int, raw []byte) error {
	reason := domain.ReasonError
	switch {
	case status == http.StatusTooManyRequests:
		reason = domain.ReasonRateLimited
	case status >= 500:
		reason = domain.ReasonHTTP5xx
	case status >= 400:
		reason = domain.ReasonHTTP4xx
	}
	return &domain.DecideError{Reason: reason, Err: fmt.Errorf("jev: HTTP %d: %s", status, snippet(raw))}
}

func snippet(raw []byte) string {
	const n = 200
	s := strings.TrimSpace(string(raw))
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}

// parse decodes and validates a 200 response.
func parse(raw []byte) (domain.Decision, error) {
	var resp response
	if err := json.Unmarshal(raw, &resp); err != nil {
		return domain.Decision{}, invalid("decode response: %v", err)
	}
	act, okA := resp.Answers[questionAction]
	size, okS := resp.Answers[questionSize]
	if !okA || !okS {
		return domain.Decision{}, invalid("missing action or size answer")
	}
	if err := checkChoice(questionAction, &act, actionOptions); err != nil {
		return domain.Decision{}, err
	}
	if err := checkChoice(questionSize, &size, sizeOptions); err != nil {
		return domain.Decision{}, err
	}

	probs := make(map[domain.Action]float64, len(act.Probabilities))
	for k, p := range act.Probabilities {
		probs[domain.Action(k)] = p
	}
	return domain.Decision{
		Action:            domain.Action(act.Choice),
		Size:              size.Choice,
		Probability:       act.Probabilities[act.Choice],
		Probabilities:     probs,
		SizeProbabilities: size.Probabilities,
		Confidence:        act.Confidence,
		Model:             resp.Model,
		InputTokens:       resp.Usage.InputTokens,
		OutputTokens:      resp.Usage.OutputTokens,
		Raw:               json.RawMessage(raw),
	}, nil
}

// checkChoice requires a choice answer whose chosen option and every
// probability key are known options, and whose probabilities sum to about 1.
func checkChoice(id string, a *answer, options []string) error {
	if a.Type != "" && a.Type != typeChoice {
		return invalid("%s: answer type %q, want %q", id, a.Type, typeChoice)
	}
	known := func(o string) bool {
		for _, k := range options {
			if o == k {
				return true
			}
		}
		return false
	}
	if !known(a.Choice) {
		return invalid("%s: unknown option %q", id, a.Choice)
	}
	if _, ok := a.Probabilities[a.Choice]; !ok {
		return invalid("%s: no probability for chosen option %q", id, a.Choice)
	}
	sum := 0.0
	for o, p := range a.Probabilities {
		if !known(o) {
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
	return nil
}

func invalid(format string, args ...any) error {
	return &domain.DecideError{Reason: domain.ReasonInvalidAnswer, Err: fmt.Errorf("jev: "+format, args...)}
}
