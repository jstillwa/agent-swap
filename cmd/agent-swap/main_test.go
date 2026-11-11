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
	t.Setenv(swap.EnvHarness, "")
	t.Setenv(swap.EnvSessionDir, "")
}

const twoHarnesses = "[[harness]]\nname = \"claude\"\nkind = \"claude\"\n[[harness]]\nname = \"pi\"\nkind = \"pi\"\n"

// runSession creates a run dir under the config's state_dir/sessions.
func runSession(t *testing.T, cfgPath, name string) string {
	t.Helper()
	cfg, err := swap.LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	d := filepath.Join(cfg.StateDir, "sessions", name)
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}

func request(t *testing.T, dir string) swap.Request {
	t.Helper()
	q, err := swap.Load[swap.Request](swap.RequestPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	return q
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

// The launcher and run read $AGENT_SWAP_CONFIG when --config is absent; --config wins.
func TestRunConfigFromEnv(t *testing.T) {
	isolate(t)
	bad := writeConfig(t, "threshold = 150\n", false)
	good := writeConfig(t, "", false)
	t.Setenv(swap.EnvConfig, bad)
	for _, args := range [][]string{{"run"}, {}} {
		_, code, err := captureRun(t, args...)
		if code != 2 || err == nil || !strings.Contains(err.Error(), "threshold") || !strings.Contains(err.Error(), bad) {
			t.Errorf("%q with $%s: want exit 2 naming threshold and %s, got %d %v", args, swap.EnvConfig, bad, code, err)
		}
	}
	// --config overrides the env: valid config, so `run` fails later (no TMUX_PANE: exit 1, not 2).
	if _, code, err := captureRun(t, "run", "--config", good); code == 2 {
		t.Errorf("--config must win over $%s: %d %v", swap.EnvConfig, code, err)
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

// use and sync write request.json in the run dir; flags may follow the name.
func TestUseAndSyncWriteRequest(t *testing.T) {
	isolate(t)
	cfg := writeConfig(t, twoHarnesses, false)
	dir := runSession(t, cfg, "run-1")
	steps := []struct {
		args []string
		pin  string
		sync []string
	}{
		{[]string{"use", "pi", "--config", cfg, "--session", dir}, "pi", nil},
		{[]string{"use", "--config", cfg, "--session", dir, "claude"}, "claude", nil},
		{[]string{"use", "auto", "--config", cfg, "--session", dir}, "", nil},
		{[]string{"sync", "pi", "--config", cfg, "--session", dir}, "", []string{"pi"}},
		{[]string{"sync", "--config", cfg, "--session", dir}, "", []string{"pi", "*"}},
	}
	for _, s := range steps {
		if _, code, err := captureRun(t, s.args...); err != nil || code != 0 {
			t.Fatalf("%q: %d %v", s.args, code, err)
		}
		q := request(t, dir)
		if q.Pin != s.pin || strings.Join(q.Sync, ",") != strings.Join(s.sync, ",") {
			t.Fatalf("%q: request %+v, want pin %q sync %q", s.args, q, s.pin, s.sync)
		}
	}
	// The run dir also comes from $AGENT_SWAP_SESSION_DIR (inside a harness, with --force).
	t.Setenv(swap.EnvSessionDir, dir)
	t.Setenv(swap.EnvHarness, "claude")
	if _, code, err := captureRun(t, "use", "pi", "--force", "--config", cfg); err != nil || code != 0 || request(t, dir).Pin != "pi" {
		t.Fatalf("use --force from inside a harness: %d %v %+v", code, err, request(t, dir))
	}
}

func TestUseSyncStatusArgumentErrors(t *testing.T) {
	isolate(t)
	cfg := writeConfig(t, twoHarnesses, false)
	dir := runSession(t, cfg, "run-1")
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"use", "--config", cfg, "--session", dir}, "usage"},
		{[]string{"use", "pi", "claude", "--config", cfg, "--session", dir}, "usage"},
		{[]string{"use", "codex", "--config", cfg, "--session", dir}, `unknown harness "codex"`},
		{[]string{"sync", "auto", "--config", cfg, "--session", dir}, "sync takes a harness name"},
		{[]string{"sync", "nope", "--config", cfg, "--session", dir}, "unknown harness"},
		{[]string{"status", "pi", "--config", cfg}, "usage"},
		{[]string{"use", "pi", "--bogus-flag"}, "bogus-flag"},
	}
	for _, c := range cases {
		_, code, err := captureRun(t, c.args...)
		if code != 2 || err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: want exit 2 with %q, got %d %v", c.args, c.want, code, err)
		}
	}
	if q := request(t, dir); q.Pin != "" || len(q.Sync) != 0 {
		t.Fatalf("a rejected command wrote request.json: %+v", q)
	}
}

// Without [[harness]] tables, the names are claude and api (the target harness).
func TestUseValidatesAgainstAllHarnessNames(t *testing.T) {
	isolate(t)
	cfg := writeConfig(t, "", false)
	dir := runSession(t, cfg, "run-1")
	if _, code, err := captureRun(t, "use", "api", "--config", cfg, "--session", dir); err != nil || code != 0 || request(t, dir).Pin != "api" {
		t.Fatalf("use api: %d %v %+v", code, err, request(t, dir))
	}
	if _, code, err := captureRun(t, "use", "pi", "--config", cfg, "--session", dir); code != 2 || err == nil {
		t.Fatalf("pi is not configured: %d %v", code, err)
	}
}

// Inside a harness (AGENT_SWAP_HARNESS set), use needs --force.
func TestUseRefusedInsideHarness(t *testing.T) {
	isolate(t)
	cfg := writeConfig(t, twoHarnesses, false)
	dir := runSession(t, cfg, "run-1")
	t.Setenv(swap.EnvHarness, "claude")
	_, code, err := captureRun(t, "use", "pi", "--config", cfg, "--session", dir)
	if code != 2 || err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("want exit 2 asking for --force, got %d %v", code, err)
	}
	if request(t, dir).Pin != "" {
		t.Fatal("refused use wrote the pin")
	}
	if _, code, err := captureRun(t, "use", "pi", "--force", "--config", cfg, "--session", dir); err != nil || code != 0 {
		t.Fatalf("--force: %d %v", code, err)
	}
	if request(t, dir).Pin != "pi" {
		t.Fatal("use --force did not pin")
	}
	// sync is not a switch: allowed from inside a harness.
	if _, code, err := captureRun(t, "sync", "--config", cfg, "--session", dir); err != nil || code != 0 {
		t.Fatalf("sync inside a harness: %d %v", code, err)
	}
}

// The run dir must be a real dir directly under state_dir/sessions.
func TestUseRejectsBadSessionDir(t *testing.T) {
	isolate(t)
	cfg := writeConfig(t, twoHarnesses, false)
	good := runSession(t, cfg, "run-1")
	other := t.TempDir()
	link := filepath.Join(filepath.Dir(good), "link")
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(filepath.Dir(good), "file")
	_ = os.WriteFile(file, nil, 0o600)
	nested := filepath.Join(good, "nested")
	_ = os.Mkdir(nested, 0o700)
	for _, d := range []string{other, link, file, nested, filepath.Join(filepath.Dir(good), "missing"), good + "/../../sessions-x"} {
		_, code, err := captureRun(t, "use", "pi", "--config", cfg, "--session", d)
		if code == 0 || err == nil {
			t.Errorf("--session %s accepted", d)
		}
	}
	if _, err := os.Stat(swap.RequestPath(other)); err == nil {
		t.Error("request.json written through the symlink")
	}
	// No --session, no env, no tmux pane: nothing to steer.
	_, code, err := captureRun(t, "use", "pi", "--config", cfg)
	if code != 1 || err == nil || !strings.Contains(err.Error(), "no running agent-swap session") {
		t.Fatalf("no session: %d %v", code, err)
	}
}

// Without --session, the run dir comes from the agent-swap pane option in this tmux window.
func TestUseFindsRunFromTmuxPane(t *testing.T) {
	isolate(t)
	base := writeConfig(t, twoHarnesses, false)
	dir := runSession(t, base, "run-1")
	bin := filepath.Join(t.TempDir(), "tmux")
	script := "#!/bin/sh\nprintf '\\n%s\\n\\n' \"$FAKE_PANE_DIRS\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(filepath.Dir(base), "pane.toml")
	b, _ := os.ReadFile(base)
	_ = os.WriteFile(cfg, []byte(strings.Replace(string(b), "/nonexistent/tmux", bin, 1)), 0o600)
	t.Setenv("TMUX_PANE", "%3")
	t.Setenv("FAKE_PANE_DIRS", dir+"\n"+dir) // the same run seen twice is one run
	if _, code, err := captureRun(t, "use", "pi", "--config", cfg); err != nil || code != 0 || request(t, dir).Pin != "pi" {
		t.Fatalf("pane discovery: %d %v %+v", code, err, request(t, dir))
	}
	t.Setenv("FAKE_PANE_DIRS", dir+"\n"+runSession(t, base, "run-2"))
	if _, code, err := captureRun(t, "use", "claude", "--config", cfg); code != 1 || err == nil || !strings.Contains(err.Error(), "several agent-swap panes") {
		t.Fatalf("two runs in the window: %d %v", code, err)
	}
}

// status shows the pin and the mirrors of a running session.
func TestStatusShowsPinAndMirrors(t *testing.T) {
	isolate(t)
	cfg := writeConfig(t, twoHarnesses, false)
	dir := runSession(t, cfg, "run-1")
	if err := swap.Update(swap.MirrorsPath(dir), func(m *map[string]swap.MirrorStatus) {
		*m = map[string]swap.MirrorStatus{"pi": {Native: "p-1", Path: "/s/p.jsonl", Behind: 2}, "claude": {Native: "c-1", Path: "/s/c.jsonl"}}
	}); err != nil {
		t.Fatal(err)
	}
	out, code, err := captureRun(t, "status", "--config", cfg, "--session", dir)
	if err != nil || code != 0 {
		t.Fatalf("status: %d %v", code, err)
	}
	for _, want := range []string{"desired harness: claude", "session: " + dir, "pin: auto",
		"mirror claude: c-1 (0 turns behind) /s/c.jsonl\nmirror pi: p-1 (2 turns behind) /s/p.jsonl"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	if _, _, err := captureRun(t, "use", "pi", "--config", cfg, "--session", dir); err != nil {
		t.Fatal(err)
	}
	if out, _, _ = captureRun(t, "status", "--config", cfg, "--session", dir); !strings.Contains(out, "pin: pi\nwanted harness (pin applied): pi") ||
		!strings.Contains(out, "desired harness: claude") {
		t.Errorf("status after use: %s", out)
	}
	// Without a running session, status shows usage only.
	out, code, err = captureRun(t, "status", "--config", cfg)
	if err != nil || code != 0 || strings.Contains(out, "pin:") || !strings.Contains(out, "desired harness") {
		t.Fatalf("status without a session: %d %v %q", code, err, out)
	}
}
