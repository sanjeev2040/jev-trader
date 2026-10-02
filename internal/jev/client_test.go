package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/domain"
)

const (
	testKey   = "test-key-not-a-secret"
	testModel = "jev-1.13.0"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newClient(t *testing.T, url string, mod func(*Config)) *Client {
	t.Helper()
	cfg := Config{
		BaseURL:    url,
		APIKey:     testKey,
		Model:      testModel,
		Timeout:    time.Second,
		MinBackoff: time.Millisecond,
		MaxBackoff: 5 * time.Millisecond,
	}
	if mod != nil {
		mod(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// answerWith returns answer_ok.json with the action answer replaced.
func answerWith(t *testing.T, action string) []byte {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal(fixture(t, "answer_ok.json"), &resp); err != nil {
		t.Fatal(err)
	}
	var a any
	if err := json.Unmarshal([]byte(action), &a); err != nil {
		t.Fatal(err)
	}
	answers := resp["answers"].(map[string]any)
	answers["action"] = a
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func reasonOf(t *testing.T, err error) string {
	t.Helper()
	var derr *domain.DecideError
	if !errors.As(err, &derr) {
		t.Fatalf("error %v (%T) is not a *domain.DecideError", err, err)
	}
	return derr.Reason
}

func TestDecideSendsTypedRequestAndParsesAnswer(t *testing.T) {
	state := fixture(t, "state.json")
	answer := fixture(t, "answer_ok.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/systemone" {
			t.Errorf("got %s %s, want POST /v1/systemone", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+testKey {
			t.Errorf("Authorization = %q", got)
		}
		raw, _ := io.ReadAll(r.Body)
		var req request
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if req.Model != testModel {
			t.Errorf("model = %q, want %q", req.Model, testModel)
		}
		if !jsonEqual(t, req.State, state) {
			t.Errorf("state = %s, want %s", req.State, state)
		}
		// questions is a map keyed by id, not an array.
		var shape struct {
			Questions map[string]struct {
				Type         string                    `json:"type"`
				Instructions map[string]any            `json:"instructions"`
				Criteria     map[string]map[string]any `json:"criteria"`
			} `json:"questions"`
		}
		if err := json.Unmarshal(raw, &shape); err != nil {
			t.Fatalf("questions are not a map: %v\n%s", err, raw)
		}
		for id, want := range map[string][]string{"action": {"BUY", "SELL", "HOLD"}, "size": {"S", "M", "L"}} {
			q, ok := shape.Questions[id]
			if !ok || q.Type != "choice" || q.Instructions["question"] == "" || q.Instructions["question"] == nil {
				t.Errorf("question %q = %+v", id, q)
			}
			if len(q.Criteria) != len(want) {
				t.Errorf("question %q criteria = %v, want options %v", id, q.Criteria, want)
			}
			for _, opt := range want {
				c, ok := q.Criteria[opt]
				if !ok || c["what"] == nil || c["not_for"] == nil {
					t.Errorf("question %q option %q criterion = %v, want what and not_for", id, opt, c)
				}
			}
		}
		if len(shape.Questions) != 2 {
			t.Errorf("questions = %d, want 2", len(shape.Questions))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(answer)
	}))
	defer srv.Close()

	d, err := newClient(t, srv.URL, nil).Decide(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != domain.ActionBuy || d.Size != domain.SizeM || d.Probability != 0.62 || d.Confidence != 0.62 {
		t.Errorf("decision = %+v", d)
	}
	if d.Probabilities[domain.ActionHold] != 0.30 || d.SizeProbabilities[domain.SizeL] != 0.20 {
		t.Errorf("distributions = %v %v", d.Probabilities, d.SizeProbabilities)
	}
	if d.Model != testModel || d.InputTokens != 187 || d.OutputTokens != 0 || d.Latency <= 0 || d.HoldReason != "" {
		t.Errorf("metadata = model %q tokens %d latency %v hold %q", d.Model, d.InputTokens, d.Latency, d.HoldReason)
	}
	if !jsonEqual(t, d.Raw, answer) {
		t.Errorf("raw answer not kept")
	}
}

func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return string(xb) == string(yb)
}

func TestDecideInvalidAnswers(t *testing.T) {
	tests := map[string][]byte{
		"not json":            []byte(`{"answers":`),
		"missing answer":      []byte(`{"model":"m","answers":{}}`),
		"missing size answer": []byte(`{"model":"m","answers":{"action":{"type":"choice","choice":"HOLD","probabilities":{"HOLD":1}}}}`),
		"answers as an array": []byte(`{"model":"m","answers":[{"id":"action","type":"choice","choice":"BUY","probabilities":{"BUY":1}}]}`),
		"unknown option":      answerWith(t, `{"type":"choice","choice":"SHORT","probabilities":{"SHORT":1}}`),
		"unknown probability key": answerWith(t,
			`{"type":"choice","choice":"BUY","probabilities":{"BUY":0.5,"SELL":0.2,"WAIT":0.3}}`),
		"chosen option without probability": answerWith(t,
			`{"type":"choice","choice":"BUY","probabilities":{"SELL":0.5,"HOLD":0.5}}`),
		"probabilities do not sum to 1": answerWith(t,
			`{"type":"choice","choice":"BUY","probabilities":{"BUY":0.6,"SELL":0.3,"HOLD":0.3}}`),
		"probability out of range": answerWith(t,
			`{"type":"choice","choice":"BUY","probabilities":{"BUY":1.5,"SELL":-0.5,"HOLD":0}}`),
		"wrong answer type": answerWith(t,
			`{"type":"score","choice":"BUY","probabilities":{"BUY":1}}`),
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write(body)
			}))
			defer srv.Close()
			_, err := newClient(t, srv.URL, nil).Decide(context.Background(), fixture(t, "state.json"))
			if got := reasonOf(t, err); got != domain.ReasonInvalidAnswer {
				t.Errorf("reason = %q, want %q", got, domain.ReasonInvalidAnswer)
			}
		})
	}
}

func TestDecideStatusErrors(t *testing.T) {
	tests := []struct {
		status    int
		reason    string
		wantCalls int32
	}{
		{http.StatusUnauthorized, domain.ReasonHTTP4xx, 1},
		{http.StatusUnprocessableEntity, domain.ReasonHTTP4xx, 1},
		{http.StatusInternalServerError, domain.ReasonHTTP5xx, 1},
		{http.StatusTooManyRequests, domain.ReasonRateLimited, 3},
		{529, domain.ReasonHTTP5xx, 3},
	}
	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				http.Error(w, `{"error":"nope"}`, tt.status)
			}))
			defer srv.Close()
			_, err := newClient(t, srv.URL, nil).Decide(context.Background(), fixture(t, "state.json"))
			if got := reasonOf(t, err); got != tt.reason {
				t.Errorf("reason = %q, want %q", got, tt.reason)
			}
			if got := calls.Load(); got != tt.wantCalls {
				t.Errorf("calls = %d, want %d", got, tt.wantCalls)
			}
		})
	}
}

func TestDecideRetriesRateLimitThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	answer := fixture(t, "answer_ok.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.WriteHeader(http.StatusTooManyRequests)
		case 2:
			w.WriteHeader(529)
		default:
			w.Write(answer)
		}
	}))
	defer srv.Close()
	d, err := newClient(t, srv.URL, nil).Decide(context.Background(), fixture(t, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != domain.ActionBuy || calls.Load() != 3 {
		t.Errorf("action %q after %d calls", d.Action, calls.Load())
	}
}

func TestDecideDoesNotRetryPastDeadline(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := newClient(t, srv.URL, func(cfg *Config) {
		cfg.MaxAttempts = 10
		cfg.MaxBackoff = time.Second
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Decide(ctx, fixture(t, "state.json"))
	if got := reasonOf(t, err); got != domain.ReasonRateLimited {
		t.Errorf("reason = %q, want %q", got, domain.ReasonRateLimited)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1: a 1s Retry-After does not fit a 200ms deadline", calls.Load())
	}
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Errorf("returned after %v; should give up at once", elapsed)
	}
}

func TestDecideTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	t.Run("context deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, err := newClient(t, srv.URL, nil).Decide(ctx, fixture(t, "state.json"))
		if got := reasonOf(t, err); got != domain.ReasonTimeout {
			t.Errorf("reason = %q, want %q", got, domain.ReasonTimeout)
		}
	})
	t.Run("default timeout without deadline", func(t *testing.T) {
		c := newClient(t, srv.URL, func(cfg *Config) { cfg.Timeout = 50 * time.Millisecond })
		start := time.Now()
		_, err := c.Decide(context.Background(), fixture(t, "state.json"))
		if got := reasonOf(t, err); got != domain.ReasonTimeout {
			t.Errorf("reason = %q, want %q", got, domain.ReasonTimeout)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("returned after %v", elapsed)
		}
	})
}

func TestDecideConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	_, err := newClient(t, url, nil).Decide(context.Background(), fixture(t, "state.json"))
	if got := reasonOf(t, err); got != domain.ReasonError {
		t.Errorf("reason = %q, want %q", got, domain.ReasonError)
	}
}

func TestDecideRejectsInvalidState(t *testing.T) {
	c := newClient(t, "http://127.0.0.1:1", nil)
	_, err := c.Decide(context.Background(), json.RawMessage(`{not json`))
	if got := reasonOf(t, err); got != domain.ReasonError {
		t.Errorf("reason = %q, want %q", got, domain.ReasonError)
	}
}

func TestNewValidatesConfig(t *testing.T) {
	tests := map[string]Config{
		"no key":     {Model: testModel},
		"jev-latest": {APIKey: testKey, Model: "jev-latest"},
	}
	for name, cfg := range tests {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: New succeeded, want error", name)
		}
	}
	c, err := New(Config{APIKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	if DefaultModel != "jev-1.13.0" || c.model != DefaultModel {
		t.Errorf("default model = %q, want jev-1.13.0", c.model)
	}
	if c.url != DefaultBaseURL+"/v1/systemone" || c.timeout != DefaultTimeout || c.maxAttempts != DefaultMaxAttempts {
		t.Errorf("defaults = %+v", c)
	}
}

func TestErrorsDoNotLeakAPIKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()
	_, err := newClient(t, srv.URL, nil).Decide(context.Background(), fixture(t, "state.json"))
	if err == nil || strings.Contains(err.Error(), testKey) {
		t.Errorf("err = %v", err)
	}
}

func TestDecideLogsResponseModelAndUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(fixture(t, "answer_ok.json"))
	}))
	defer srv.Close()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	c := newClient(t, srv.URL, func(cfg *Config) { cfg.Logger = logger })
	if _, err := c.Decide(context.Background(), fixture(t, "state.json")); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"model=jev-1.13.0", "input_tokens=187", "output_tokens=0"} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q missing %q", out, want)
		}
	}
	if strings.Contains(out, testKey) {
		t.Errorf("log leaks the API key: %q", out)
	}
}
