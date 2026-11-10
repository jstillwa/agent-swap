package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jstillwa/agent-swap/internal/swap"
)

// writeConfig creates a config whose state dir holds an account in the given
// desired mode, and returns the config path.
func writeConfig(t *testing.T, extra string, apiDesired bool) string {
	t.Helper()
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	p := filepath.Join(dir, "config.toml")
	body := "state_dir = \"" + state + "\"\ntmux_bin = \"/nonexistent/tmux\"\n" + extra +
		"[targets.gw]\nkey_cmd = \"echo k\"\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	until := int64(0)
	if apiDesired {
		until = time.Now().Unix() + 3600
	}
	if err := swap.Update(swap.AccountPath(state), func(a *swap.Account) { a.APIUntil = until }); err != nil {
		t.Fatal(err)
	}
	return p
}

// captureRun runs run(args) with stdout captured.
func captureRun(t *testing.T, args ...string) (string, int, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()
	done := make(chan string, 1)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	code, rerr := run(args)
	w.Close()
	return <-done, code, rerr
}

// isolate points HOME (default config path) and the env override at temp
// locations so the user's real config is never read.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv(swap.EnvConfig, "")
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
}

// F9: status honours --config and $AGENT_SWAP_CONFIG (flag wins).
func TestStatusConfigPrecedence(t *testing.T) {
	isolate(t)
	apiCfg, subCfg := writeConfig(t, "", true), writeConfig(t, "", false)

	out, code, err := captureRun(t, "status", "--config", apiCfg)
	if err != nil || code != 0 || !strings.Contains(out, "desired mode: API") {
		t.Fatalf("status --config: code=%d err=%v out=%q", code, err, out)
	}

	t.Setenv(swap.EnvConfig, apiCfg)
	out, code, err = captureRun(t, "status")
	if err != nil || code != 0 || !strings.Contains(out, "desired mode: API") {
		t.Fatalf("status with $%s: code=%d err=%v out=%q", swap.EnvConfig, code, err, out)
	}

	out, code, err = captureRun(t, "status", "--config", subCfg) // env still points at apiCfg
	if err != nil || code != 0 || !strings.Contains(out, "desired mode: SUB") {
		t.Fatalf("flag must beat env: code=%d err=%v out=%q", code, err, out)
	}
}

// F4: both the launcher and `run` validate the config before tmux/claude.
func TestInvalidConfigRejectedBeforeStart(t *testing.T) {
	isolate(t)
	bad := writeConfig(t, "threshold = 150\n", false)
	good := writeConfig(t, "", false)
	cases := [][]string{
		{"--config", bad},
		{"run", "--config", bad},
		{"--config", good, "--poll", "0s"},
		{"run", "--config", good, "--threshold", "0"},
	}
	for _, args := range cases {
		_, code, err := captureRun(t, args...)
		if err == nil || code != 2 {
			t.Errorf("%q: want exit 2 with a validation error, got %d %v", args, code, err)
			continue
		}
		if !strings.Contains(err.Error(), "threshold") && !strings.Contains(err.Error(), "poll") {
			t.Errorf("%q: error does not name the bad key: %v", args, err)
		}
	}
}
