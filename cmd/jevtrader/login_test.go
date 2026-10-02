package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/config"
	"github.com/sanjeev2040/jev-trader/internal/kiteauth"
)

// syncBuffer is a bytes.Buffer safe to read while runLogin writes to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func testEnv(vars map[string]string) (env, *syncBuffer, *syncBuffer) {
	stdout, stderr := &syncBuffer{}, &syncBuffer{}
	return env{
		lookup: func(k string) (string, bool) { v, ok := vars[k]; return v, ok },
		stdout: stdout,
		stderr: stderr,
	}, stdout, stderr
}

var fixtureKiteEnv = map[string]string{
	config.EnvKiteAPIKey:    "fixture_api_key",
	config.EnvKiteAPISecret: "fixture_api_secret",
}

func TestRunUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"help"}, {"nope"}} {
		e, _, stderr := testEnv(nil)
		if code := run(context.Background(), args, e); code != 2 {
			t.Errorf("run(%q) = %d, want 2", args, code)
		}
		if !strings.Contains(stderr.String(), "login") {
			t.Errorf("run(%q) usage does not list login: %q", args, stderr)
		}
	}
}

func TestLoginRequiresSecrets(t *testing.T) {
	e, _, stderr := testEnv(map[string]string{config.EnvKiteAPIKey: "fixture_api_key"})
	if code := run(context.Background(), []string{"login", "-state-dir", t.TempDir()}, e); code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), config.EnvKiteAPISecret) {
		t.Errorf("stderr = %q, want it to name %s", stderr, config.EnvKiteAPISecret)
	}
}

func TestLoginRejectsNonLoopbackRedirect(t *testing.T) {
	e, _, stderr := testEnv(fixtureKiteEnv)
	code := run(context.Background(), []string{"login", "-state-dir", t.TempDir(), "-redirect-url", "http://0.0.0.0:8765/cb"}, e)
	if code != 2 || !strings.Contains(stderr.String(), "127.0.0.1") {
		t.Errorf("exit = %d, stderr = %q", code, stderr)
	}
}

func TestLoginEndToEnd(t *testing.T) {
	kite := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		want := kiteauth.Checksum("fixture_api_key", "fixtureRequestToken", "fixture_api_secret")
		if r.URL.Path != "/session/token" || r.PostForm.Get("checksum") != want {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "error_type": "TokenException", "message": "invalid"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{
			"user_id": "AB1234", "access_token": "fixture_access_token",
		}})
	}))
	defer kite.Close()
	orig := newKiteClient
	newKiteClient = func(s config.Secrets) *kiteauth.Client {
		c := orig(s)
		c.BaseURL = kite.URL
		return c
	}
	defer func() { newKiteClient = orig }()

	// Reserve a free loopback port for the redirect URL.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	redirect := "http://" + ln.Addr().String() + "/kite/callback"
	ln.Close()

	stateDir := t.TempDir()
	e, stdout, stderr := testEnv(fixtureKiteEnv)
	exit := make(chan int, 1)
	go func() {
		exit <- run(context.Background(), []string{"login", "-state-dir", stateDir, "-redirect-url", redirect, "-timeout", "10s"}, e)
	}()

	loginURLRe := regexp.MustCompile(`https://kite\.zerodha\.com/connect/login\?\S+`)
	var loginURL *url.URL
	for deadline := time.Now().Add(5 * time.Second); loginURL == nil; {
		if m := loginURLRe.FindString(stdout.String()); m != "" {
			if loginURL, err = url.Parse(m); err != nil {
				t.Fatal(err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no login URL printed; stdout=%q stderr=%q", stdout, stderr)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Simulate Kite's redirect after a successful browser login.
	q, err := url.ParseQuery(loginURL.Query().Get("redirect_params"))
	if err != nil {
		t.Fatal(err)
	}
	q.Set("status", "success")
	q.Set("request_token", "fixtureRequestToken")
	resp, err := http.Get(redirect + "?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("callback status = %d", resp.StatusCode)
	}

	select {
	case code := <-exit:
		if code != 0 {
			t.Fatalf("exit = %d; stderr = %q", code, stderr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("login did not finish")
	}
	out := stdout.String() + stderr.String()
	if !strings.Contains(out, "Logged in as AB1234") || !strings.Contains(out, "06:00 IST") {
		t.Errorf("output = %q", out)
	}
	for _, secret := range []string{"fixture_access_token", "fixture_api_secret"} {
		if strings.Contains(out, secret) {
			t.Errorf("output leaks %q", secret)
		}
	}
	info, err := os.Stat(kiteauth.Store{Dir: stateDir}.Path())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("session file mode = %04o", info.Mode().Perm())
	}
}
