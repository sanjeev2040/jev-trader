package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeCal(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cal.yaml")
	doc := "years_complete: [2026]\nholidays:\n  - {date: \"2026-10-02\", name: Gandhi Jayanti}\n"
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRun(t *testing.T) {
	cal := writeCal(t)
	// 2026-10-01 21:00 UTC is already 2026-10-02 02:30 IST: a holiday.
	now := func() time.Time { return time.Date(2026, 10, 1, 21, 0, 0, 0, time.UTC) }

	for _, tc := range []struct {
		name string
		args []string
		code int
		out  string
		gh   string
	}{
		{"open", []string{"-file", cal, "-date", "2026-10-01"}, 0, "NSE open", "open=true\n"},
		{"holiday", []string{"-file", cal, "-date", "2026-10-02"}, 1, "NSE closed", "open=false\n"},
		{"today in IST", []string{"-file", cal}, 1, "2026-10-02: NSE closed", "open=false\n"},
		{"bad date", []string{"-file", cal, "-date", "x"}, 2, "", ""},
		{"missing file", []string{"-file", "/nonexistent.yaml", "-date", "2026-10-01"}, 2, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh := filepath.Join(t.TempDir(), "out")
			var stdout, stderr bytes.Buffer
			args := append(tc.args, "-github-output", gh)
			if code := run(args, now, &stdout, &stderr); code != tc.code {
				t.Fatalf("code %d, want %d (stderr %q)", code, tc.code, stderr.String())
			}
			if !strings.Contains(stdout.String(), tc.out) {
				t.Errorf("stdout %q lacks %q", stdout.String(), tc.out)
			}
			got, _ := os.ReadFile(gh)
			if string(got) != tc.gh {
				t.Errorf("github output %q, want %q", got, tc.gh)
			}
		})
	}
}
