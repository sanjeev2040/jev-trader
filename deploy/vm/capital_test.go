package vm_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fetch_trading_capital (lib.sh) is what prepare.sh runs to put the Key Vault
// secret trading-capital into the VM's secrets file. These tests run it
// against a stub curl: no network and no Key Vault.
func runFetch(t *testing.T, status, body string) (stdout, stderr string, err error) {
	t.Helper()
	for _, tool := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	dir := t.TempDir()
	stub := `#!/usr/bin/env bash
# stub curl: write $STUB_BODY to the -o file and print $STUB_STATUS
while [ $# -gt 0 ]; do
  case "$1" in -o) out=$2; shift ;; esac
  shift
done
printf '%s' "$STUB_BODY" > "$out"
printf '%s' "$STUB_STATUS"
`
	if err := os.WriteFile(filepath.Join(dir, "curl"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	run := filepath.Join(dir, "run")
	if err := os.Mkdir(run, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", `. lib.sh; RUN_DIR="$1"; kv_token=t; KEY_VAULT_NAME=kv-test; fetch_trading_capital`, "bash", run)
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "STUB_STATUS="+status, "STUB_BODY="+body)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	if left, _ := os.ReadDir(run); len(left) != 0 {
		t.Errorf("temporary files left in the run directory: %v", left)
	}
	return out.String(), errb.String(), err
}

func TestFetchTradingCapital(t *testing.T) {
	for name, tc := range map[string]struct {
		status, body string
		wantOut      string
		wantErr      string // empty means success
		wantLog      string
	}{
		"integer":    {"200", `{"value":"25000"}`, "JEVTRADER_TRADING_CAPITAL=25000\n", "", "trading capital is 25000 rupees"},
		"decimal":    {"200", `{"value":"12500.50"}`, "JEVTRADER_TRADING_CAPITAL=12500.50\n", "", ""},
		"not set":    {"404", `{"error":{"code":"SecretNotFound"}}`, "", "", "trading-capital' is not set"},
		"forbidden":  {"403", `{"error":{"code":"Forbidden"}}`, "", "HTTP 403", ""},
		"not number": {"200", `{"value":"lots"}`, "", "not a positive number of rupees", ""},
		"negative":   {"200", `{"value":"-5"}`, "", "not a positive number of rupees", ""},
		"injection":  {"200", `{"value":"1\nKITE_API_KEY=x"}`, "", "not a positive number of rupees", ""},
		"no value":   {"200", `{}`, "", "has no value", ""},
	} {
		t.Run(name, func(t *testing.T) {
			stdout, stderr, err := runFetch(t, tc.status, tc.body)
			if tc.wantErr == "" {
				if err != nil || stdout != tc.wantOut || !strings.Contains(stderr, tc.wantLog) {
					t.Fatalf("err %v, stdout %q, stderr %q; want stdout %q and log %q", err, stdout, stderr, tc.wantOut, tc.wantLog)
				}
				return
			}
			if err == nil || stdout != "" || !strings.Contains(stderr, tc.wantErr) {
				t.Fatalf("err %v, stdout %q, stderr %q; want a failure saying %q and nothing written", err, stdout, stderr, tc.wantErr)
			}
		})
	}
}
