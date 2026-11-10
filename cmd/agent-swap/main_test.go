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

// writeConfig creates a config whose state dir holds an account where claude
// is blocked (so "api" is desired) or free, and returns the config path.
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
	if err := swap.Update(swap.AccountPath(state), func(a *swap.Account) {
		a.Harnesses = map[string]*swap.Usage{"claude": {BlockedUntil: until}}
	}); err != nil {
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

// status honours --config and $AGENT_SWAP_CONFIG (flag wins).
func TestStatusConfigPrecedence(t *testing.T) {
	isolate(t)
	apiCfg, subCfg := writeConfig(t, "", true), writeConfig(t, "", false)

	out, code, err := captureRun(t, "status", "--config", apiCfg)
	if err != nil || code != 0 || !strings.Contains(out, "desired harness: api") {
		t.Fatalf("status --config: code=%d err=%v out=%q", code, err, out)
	}

	t.Setenv(swap.EnvConfig, apiCfg)
	out, code, err = captureRun(t, "status")
	if err != nil || code != 0 || !strings.Contains(out, "desired harness: api") {
		t.Fatalf("status with $%s: code=%d err=%v out=%q", swap.EnvConfig, code, err, out)
	}

	out, code, err = captureRun(t, "status", "--config", subCfg) // env still points at apiCfg
	if err != nil || code != 0 || !strings.Contains(out, "desired harness: claude") {
		t.Fatalf("flag must beat env: code=%d err=%v out=%q", code, err, out)
	}
}

// Both the launcher and `run` validate the config before tmux/claude.
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

// [[harness]] errors are config errors too: exit 2 naming the problem.
func TestInvalidHarnessRejected(t *testing.T) {
	isolate(t)
	cases := map[string]string{
		"[[harness]]\nname = \"a\"\nkind = \"aider\"\n":                                           "kind",
		"[[harness]]\nname = \"a\"\nkind = \"pi\"\n[[harness]]\nname = \"a\"\nkind = \"codex\"\n": "a",
		"[[harness]]\nname = \"a\"\nkind = \"codex\"\ntarget = \"gw\"\n":                          "target",
		"context_max_bytes = 0\n": "context_max_bytes",
	}
	for extra, key := range cases {
		_, code, err := captureRun(t, "run", "--config", writeConfig(t, extra, false))
		if err == nil || code != 2 || !strings.Contains(err.Error(), key) {
			t.Errorf("%q: want exit 2 naming %q, got %d %v", extra, key, code, err)
		}
	}
	// A valid [[harness]] list with no target passes validation (fails later
	// only because TMUX_PANE is unset: exit 1, not 2).
	ok := writeConfig(t, "[[harness]]\nname = \"codex\"\nkind = \"codex\"\n", false)
	if _, code, _ := captureRun(t, "run", "--config", ok); code == 2 {
		t.Errorf("valid [[harness]] config rejected with exit 2")
	}
}

// Flags before the subcommand are a usage error, not a launch.
func TestFlagsBeforeSubcommandRejected(t *testing.T) {
	isolate(t)
	_, code, err := captureRun(t, "--config", writeConfig(t, "", false), "status")
	if code != 2 || err == nil || !strings.Contains(err.Error(), "subcommand first") {
		t.Fatalf("want exit 2 with \"subcommand first\", got %d %v", code, err)
	}
}
