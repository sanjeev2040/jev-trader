package jev

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func newsQuestions() map[string]Question {
	return map[string]Question{
		"impact":    ScoreQuestion("How big a move?", "none", "small", "large"),
		"material":  NoulQuestion("Is it material?", "moves the price", "routine"),
		"direction": ChoiceQuestion("Which way?", map[string]string{"UP": "up", "DOWN": "down", "FLAT": "flat"}),
	}
}

const askOK = `{"model":"jev-1.13.0","answers":{
 "impact":{"type":"score","score":1.4,"legend":{"0":"none","1":"small","2":"large"},"probabilities":{"0":0,"1":0.6,"2":0.4},"confidence":0.8},
 "material":{"type":"noul","noul":0.9},
 "direction":{"type":"choice","choice":"UP","probabilities":{"UP":0.7,"DOWN":0.1,"FLAT":0.2},"confidence":0.7}},
 "usage":{"input_tokens":310,"output_tokens":40}}`

func TestAskSendsTypedQuestionsAndParsesAnswers(t *testing.T) {
	var got map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &got); err != nil {
			t.Error(err)
		}
		io.WriteString(w, askOK)
	}))
	defer srv.Close()

	res, err := newClient(t, srv.URL, nil).Ask(context.Background(), map[string]string{"headline": "Order win"}, newsQuestions())
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer "+testKey {
		t.Errorf("Authorization = %q", auth)
	}
	if got["model"] != testModel {
		t.Errorf("model = %v", got["model"])
	}
	qs := got["questions"].(map[string]any)
	if qs["impact"].(map[string]any)["type"] != "score" || qs["material"].(map[string]any)["type"] != "noul" || qs["direction"].(map[string]any)["type"] != "choice" {
		t.Errorf("question types = %v", qs)
	}
	if levels := qs["impact"].(map[string]any)["criteria"].([]any); len(levels) != 3 {
		t.Errorf("score criteria = %v, want an array of 3 levels", levels)
	}
	if c := qs["material"].(map[string]any)["criteria"].(map[string]any); c["true"] != "moves the price" || c["false"] != "routine" {
		t.Errorf("noul criteria = %v", c)
	}
	if res.Answers["impact"].Score != 1.4 || res.Answers["material"].Noul != 0.9 || res.Answers["direction"].Choice != "UP" {
		t.Errorf("answers = %+v", res.Answers)
	}
	if res.Model != testModel || res.InputTokens != 310 || res.OutputTokens != 40 {
		t.Errorf("model/usage = %q %d %d", res.Model, res.InputTokens, res.OutputTokens)
	}
}

func TestAskRejectsInvalidAnswers(t *testing.T) {
	tests := map[string]struct{ from, to string }{
		"noul out of range":        {`"noul":0.9`, `"noul":1.4`},
		"score out of range":       {`"score":1.4`, `"score":7`},
		"unknown choice":           {`"choice":"UP"`, `"choice":"SIDEWAYS"`},
		"probabilities do not sum": {`"UP":0.7`, `"UP":0.2`},
		"type mismatch":            {`"type":"noul"`, `"type":"score"`},
		"missing answer":           {`"material":{"type":"noul","noul":0.9},`, ``},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			body := strings.Replace(askOK, tt.from, tt.to, 1)
			if body == askOK {
				t.Fatal("test did not change the response")
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, body) }))
			defer srv.Close()
			_, err := newClient(t, srv.URL, nil).Ask(context.Background(), "x", newsQuestions())
			if err == nil {
				t.Fatal("Ask accepted an invalid answer")
			}
			if reasonOf(t, err) != "invalid_answer" {
				t.Errorf("reason = %s, want invalid_answer", reasonOf(t, err))
			}
		})
	}
}

func TestAskRetriesOverloaded(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(529)
			return
		}
		io.WriteString(w, askOK)
	}))
	defer srv.Close()
	if _, err := newClient(t, srv.URL, nil).Ask(context.Background(), "x", newsQuestions()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2", calls.Load())
	}
}

func TestAskNeedsQuestions(t *testing.T) {
	if _, err := newClient(t, "http://127.0.0.1:1", nil).Ask(context.Background(), "x", nil); err == nil {
		t.Fatal("Ask with no questions succeeded")
	}
}
