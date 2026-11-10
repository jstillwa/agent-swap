package swap

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// Fake tmux: logs every invocation, one line each.
const fakeTmux = `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_TMUX_LOG"
`

// Fake claude: logs argv (and its ANTHROPIC_* env to $FAKE_ENV_LOG if set),
// plays the SessionStart hook + statusline by writing session.json (session
// id $FAKE_SID, default sid-1; idle $FAKE_IDLE, default true), then exits 0
// on each new "/exit" typed into the pane (after $FAKE_EXIT_DELAY), or 7 when $FAKE_QUIT exists (user
// quitting on their own).
const fakeClaude = `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_ARGS_LOG"
[ -n "$FAKE_ENV_LOG" ] && { env | grep '^ANTHROPIC_' >> "$FAKE_ENV_LOG"; printf -- '--\n' >> "$FAKE_ENV_LOG"; }
printf '{"claude_session_id":"%s","idle":%s,"started_at":%s}' "${FAKE_SID-sid-1}" "${FAKE_IDLE-true}" "$(date +%s)" > "$AGENT_SWAP_SESSION_DIR/session.json"
n=$(grep -c -- '-l /exit$' "$FAKE_TMUX_LOG")
while [ "$(grep -c -- '-l /exit$' "$FAKE_TMUX_LOG")" = "$n" ]; do
  [ -f "$FAKE_QUIT" ] && exit 7
  sleep 0.05
done
[ -n "$FAKE_EXIT_DELAY" ] && sleep "$FAKE_EXIT_DELAY"
exit 0
`

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(timeout); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRunnerFullCycle(t *testing.T) {
	tmp := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(tmp, name)
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	tmuxLog, argsLog, quit := filepath.Join(tmp, "tmux.log"), filepath.Join(tmp, "args.log"), filepath.Join(tmp, "quit")
	write("tmux.log", "")
	t.Setenv("FAKE_TMUX_LOG", tmuxLog)
	t.Setenv("FAKE_ARGS_LOG", argsLog)
	t.Setenv("FAKE_QUIT", quit)

	cfg := Defaults()
	cfg.Targets = map[string]Target{"gateway": {BaseURL: "https://gw.example/anthropic", KeyCmd: "echo k"}}
	cfg.TmuxBin, cfg.ClaudeBin = write("tmux", fakeTmux), write("claude", fakeClaude)
	cfg.StateDir, cfg.Notify, cfg.StatuslineCmd = filepath.Join(tmp, "state"), false, "true"
	cfg.Poll.Duration, cfg.SwitchBackDelay.Duration, cfg.SwitchBackTimeout.Duration = 50*time.Millisecond, time.Second, time.Second
	reset := time.Now().Unix() + 3
	if err := Update(AccountPath(cfg.StateDir), func(a *Account) {
		a.FiveHour = &Window{UsedPct: 96, ResetsAt: reset}
	}); err != nil {
		t.Fatal(err)
	}

	r, err := NewRunner(cfg, "/cfg.toml", "gateway", "/bin/cs", "%1", []string{"--model", "haiku"})
	if err != nil {
		t.Fatal(err)
	}
	r.Settle = 0
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	type res struct {
		code int
		err  error
	}
	out := make(chan res, 1)
	go func() { c, e := r.Run(ctx); out <- res{c, e} }()

	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }
	lines := func(p string) []string { return strings.Split(strings.TrimSpace(read(p)), "\n") }

	// 1. SUB at 96%: handoff message names a touch file. Claude touches it.
	waitFor(t, "handoff message", 5*time.Second, func() bool { return strings.Contains(read(tmuxLog), "EMERGENCY SHUTDOWN") })
	m := regexp.MustCompile(`touch (\S+ready-[0-9a-f]+)`).FindStringSubmatch(read(tmuxLog))
	if m == nil {
		t.Fatal("no touch file in message")
	}
	if strings.Contains(read(tmuxLog), "-l /exit") {
		t.Fatal("/exit sent before the touch file existed")
	}
	if err := os.WriteFile(m[1], nil, 0o600); err != nil {
		t.Fatal(err)
	}

	// 2. Relaunched on API with --resume and the API settings, then told to continue.
	waitFor(t, "API relaunch", 5*time.Second, func() bool { return len(lines(argsLog)) >= 2 })
	l := lines(argsLog)
	if l[0] != "--model haiku --settings "+r.settingsPath(Sub) {
		t.Fatalf("first launch: %s", l[0])
	}
	if l[1] != "--resume sid-1 --model haiku --settings "+r.settingsPath(API) {
		t.Fatalf("api launch: %s", l[1])
	}
	if !strings.Contains(read(r.settingsPath(API)), `"apiKeyHelper": "/bin/cs key gateway"`) {
		t.Fatal("api settings lack apiKeyHelper")
	}
	waitFor(t, "API resume message", 5*time.Second, func() bool { return strings.Contains(read(tmuxLog), "Session resumed on API billing (gateway)") })
	a, _ := Load[Account](AccountPath(cfg.StateDir))
	if a.APIUntil != reset+1 {
		t.Fatalf("api_until %d want %d", a.APIUntil, reset+1)
	}

	// 3. After reset+delay: return handoff; nobody touches; 1s timeout at idle switches back.
	waitFor(t, "return handoff", 8*time.Second, func() bool { return strings.Contains(read(tmuxLog), "ACCOUNT CYCLE (agent-swap)") })
	waitFor(t, "SUB relaunch", 5*time.Second, func() bool { return len(lines(argsLog)) >= 3 })
	if l := lines(argsLog)[2]; l != "--resume sid-1 --model haiku --settings "+r.settingsPath(Sub) {
		t.Fatalf("sub launch: %s", l)
	}
	waitFor(t, "SUB resume message", 5*time.Second, func() bool { return strings.Contains(read(tmuxLog), "Session resumed on subscription") })

	// 4. Stale 96% window (already reset) must not re-trigger; user quits.
	time.Sleep(300 * time.Millisecond)
	if strings.Count(read(tmuxLog), "EMERGENCY SHUTDOWN") != 1 {
		t.Fatal("re-triggered on a reset window")
	}
	if err := os.WriteFile(quit, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got := <-out
	if got.err != nil || got.code != 7 {
		t.Fatalf("run returned %d %v", got.code, got.err)
	}
	if !strings.Contains(read(tmuxLog), "status-right #[fg=white,bg=blue] API (gateway)") {
		t.Fatal("status bar never showed API mode")
	}
}

// harness wires a Runner to the fake tmux and claude with sub-second timings.
type harness struct {
	t                     *testing.T
	tmp, tmuxLog, argsLog string
	quit, envLog          string
	cfg                   Config
}

func newHarness(t *testing.T, stateDir string) *harness {
	t.Helper()
	tmp := t.TempDir()
	h := &harness{t: t, tmp: tmp, tmuxLog: filepath.Join(tmp, "tmux.log"), argsLog: filepath.Join(tmp, "args.log"),
		quit: filepath.Join(tmp, "quit"), envLog: filepath.Join(tmp, "env.log")}
	write := func(name, body string) string {
		p := filepath.Join(tmp, name)
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("tmux.log", "")
	t.Setenv("FAKE_TMUX_LOG", h.tmuxLog)
	t.Setenv("FAKE_ARGS_LOG", h.argsLog)
	t.Setenv("FAKE_QUIT", h.quit)
	t.Setenv("FAKE_ENV_LOG", h.envLog)
	cfg := Defaults()
	cfg.Targets = map[string]Target{"gateway": {BaseURL: "https://gw.example/anthropic", KeyCmd: "echo k"}}
	cfg.TmuxBin, cfg.ClaudeBin = write("tmux", fakeTmux), write("claude", fakeClaude)
	if stateDir == "" {
		stateDir = "state"
	}
	cfg.StateDir, cfg.Notify, cfg.StatuslineCmd = filepath.Join(tmp, stateDir), false, "true"
	cfg.Poll.Duration, cfg.SwitchBackDelay.Duration, cfg.SwitchBackTimeout.Duration = 50*time.Millisecond, time.Second, time.Second
	h.cfg = cfg
	return h
}

func (h *harness) setAccount(fn func(*Account)) {
	h.t.Helper()
	if err := Update(AccountPath(h.cfg.StateDir), fn); err != nil {
		h.t.Fatal(err)
	}
}

type runResult struct {
	code int
	err  error
}

func (h *harness) start(args []string) (*Runner, <-chan runResult) {
	h.t.Helper()
	r, err := NewRunner(h.cfg, "/cfg.toml", "gateway", "/bin/cs", "%1", args)
	if err != nil {
		h.t.Fatal(err)
	}
	r.Settle = 0
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	h.t.Cleanup(cancel)
	out := make(chan runResult, 1)
	go func() { c, e := r.Run(ctx); out <- runResult{c, e} }()
	return r, out
}

func (h *harness) read(p string) string { b, _ := os.ReadFile(p); return string(b) }

func (h *harness) launches() []string {
	s := strings.TrimSpace(h.read(h.argsLog))
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func (h *harness) readyFile(re string) string {
	h.t.Helper()
	waitFor(h.t, "handoff message", 5*time.Second, func() bool { return strings.Contains(h.read(h.tmuxLog), "EMERGENCY SHUTDOWN") })
	m := regexp.MustCompile(re).FindStringSubmatch(h.read(h.tmuxLog))
	if m == nil {
		h.t.Fatalf("no ready file matching %s in tmux log:\n%s", re, h.read(h.tmuxLog))
	}
	return m[1]
}

// touchReady waits for the handoff message and creates its ready file.
func (h *harness) touchReady() string {
	h.t.Helper()
	p := h.readyFile(`touch (\S+ready-[0-9a-f]+)`)
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		h.t.Fatal(err)
	}
	return p
}

// quitOK makes the fake claude quit on its own and expects Run to return 7.
func (h *harness) quitOK(out <-chan runResult) {
	h.t.Helper()
	if err := os.WriteFile(h.quit, nil, 0o600); err != nil {
		h.t.Fatal(err)
	}
	if got := <-out; got.err != nil || got.code != 7 {
		h.t.Fatalf("run returned %d %v", got.code, got.err)
	}
}

func (h *harness) hot() {
	h.setAccount(func(a *Account) { a.FiveHour = &Window{UsedPct: 96, ResetsAt: time.Now().Unix() + 60} })
}

// F2 + F5: the positional prompt is used only on the first launch; the child
// never inherits ANTHROPIC_BASE_URL / ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN.
func TestRunnerRelaunchReplaysOnlyPersistentArgs(t *testing.T) {
	h := newHarness(t, "")
	t.Setenv("ANTHROPIC_BASE_URL", "https://leak.example")
	t.Setenv("ANTHROPIC_API_KEY", "sk-leak")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok-leak")
	t.Setenv("ANTHROPIC_MODEL", "keep-me")
	h.hot()
	r, out := h.start([]string{"--model", "haiku", "fix bug"})

	h.touchReady()
	waitFor(t, "API relaunch", 5*time.Second, func() bool { return len(h.launches()) >= 2 })
	l := h.launches()
	if l[0] != "--model haiku fix bug --settings "+r.settingsPath(Sub) {
		t.Fatalf("first launch must keep the prompt: %s", l[0])
	}
	if l[1] != "--resume sid-1 --model haiku --settings "+r.settingsPath(API) {
		t.Fatalf("relaunch must drop the prompt: %s", l[1])
	}
	env := h.read(h.envLog)
	for _, k := range []string{"ANTHROPIC_BASE_URL=", "ANTHROPIC_API_KEY=", "ANTHROPIC_AUTH_TOKEN="} {
		if strings.Contains(env, k) {
			t.Errorf("child inherited %s:\n%s", k, env)
		}
	}
	if !strings.Contains(env, "ANTHROPIC_MODEL=keep-me") {
		t.Errorf("unrelated ANTHROPIC_* vars must pass through:\n%s", env)
	}
	h.quitOK(out)
}

// F3: an empty claude session id blocks the switch (no /exit, no relaunch).
func TestRunnerEmptySessionIDBlocksSwitch(t *testing.T) {
	h := newHarness(t, "")
	t.Setenv("FAKE_SID", "")
	h.hot()
	r, out := h.start(nil)
	h.touchReady()
	waitFor(t, "switch blocked log", 3*time.Second, func() bool {
		return strings.Contains(h.read(filepath.Join(r.Dir, "log")), "switch blocked")
	})
	time.Sleep(6 * h.cfg.Poll.Duration) // at least 5 more blocked ticks
	if n := strings.Count(h.read(filepath.Join(r.Dir, "log")), "switch blocked"); n != 1 {
		t.Fatalf("switch blocked logged %d times, want once per blocked run", n)
	}
	if strings.Contains(h.read(h.tmuxLog), "-l /exit") {
		t.Fatal("/exit sent although the session id is empty")
	}
	if n := len(h.launches()); n != 1 {
		t.Fatalf("want 1 launch, got %d", n)
	}
	h.quitOK(out)
}

// F3: an unreadable account.json fails Run before claude starts.
func TestRunnerCorruptAccountFails(t *testing.T) {
	h := newHarness(t, "")
	if err := os.MkdirAll(h.cfg.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(AccountPath(h.cfg.StateDir), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, out := h.start(nil)
	got := <-out
	if got.err == nil || !strings.Contains(got.err.Error(), AccountPath(h.cfg.StateDir)) {
		t.Fatalf("want error naming %s, got %d %v", AccountPath(h.cfg.StateDir), got.code, got.err)
	}
	if n := len(h.launches()); n != 0 {
		t.Fatalf("claude must not start, got %d launches", n)
	}
}

// F1: a desired-mode flip mid-handoff cancels it and removes the ready file.
func TestRunnerCancelsHandoffOnModeFlip(t *testing.T) {
	h := newHarness(t, "")
	t.Setenv("FAKE_IDLE", "false") // busy: a touched file alone cannot switch
	h.hot()
	_, out := h.start(nil)
	ready := h.touchReady()
	time.Sleep(150 * time.Millisecond)
	if _, err := os.Stat(ready); err != nil {
		t.Fatal("ready file removed before the flip")
	}
	h.setAccount(func(a *Account) { // usage reset: SUB is desired again
		a.APIUntil, a.ResetsAt = 0, 0
		a.FiveHour = &Window{UsedPct: 10, ResetsAt: time.Now().Unix() + 3600}
	})
	waitFor(t, "ready file removed", 3*time.Second, func() bool { _, err := os.Stat(ready); return os.IsNotExist(err) })
	time.Sleep(200 * time.Millisecond)
	log := h.read(h.tmuxLog)
	if strings.Contains(log, "-l /exit") || len(h.launches()) != 1 {
		t.Fatalf("cancel must not switch: launches=%d", len(h.launches()))
	}
	if strings.Count(log, "EMERGENCY SHUTDOWN") != 1 {
		t.Fatal("handoff must not restart after cancel")
	}
	h.quitOK(out)
}

// F10: the {file} placeholder is shell-quoted when the path needs it.
func TestRunnerQuotesReadyFilePath(t *testing.T) {
	h := newHarness(t, "state dir") // space in the path
	h.hot()
	_, out := h.start(nil)
	ready := h.readyFile(`touch '([^']+/ready-[0-9a-f]+)'`)
	if !strings.HasPrefix(ready, h.cfg.StateDir+"/") {
		t.Fatalf("ready file %q not under %q", ready, h.cfg.StateDir)
	}
	h.quitOK(out)
}

// F5: childEnv drops inherited credential, base-URL and provider vars only.
func TestChildEnv(t *testing.T) {
	keep := []string{"PATH=/bin", "ANTHROPIC_MODEL=m", "ANTHROPIC_BASE_URL_EXTRA=keep", "CLAUDE_CODE_OAUTH_TOKEN=o", "HOME=/h"}
	in := append([]string{}, keep[:2]...)
	for _, k := range []string{
		"ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_CUSTOM_HEADERS",
		"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
		"ANTHROPIC_BEDROCK_BASE_URL", "ANTHROPIC_VERTEX_BASE_URL",
		"ANTHROPIC_FOUNDRY_BASE_URL", "ANTHROPIC_FOUNDRY_API_KEY", "ANTHROPIC_FOUNDRY_RESOURCE",
	} {
		in = append(in, k+"=x")
	}
	in = append(in, keep[2:]...)
	if got := childEnv(in); !slices.Equal(got, keep) {
		t.Fatalf("childEnv = %q want %q", got, keep)
	}
}

// After the child exits, the relaunch mode comes from a fresh account load:
// a flip back to SUB while /exit is in flight relaunches on SUB.
func TestRunnerRelaunchModeFromFreshAccount(t *testing.T) {
	h := newHarness(t, "")
	t.Setenv("FAKE_EXIT_DELAY", "0.4")
	h.hot()
	r, out := h.start(nil)
	h.touchReady()
	waitFor(t, "/exit", 3*time.Second, func() bool { return strings.Contains(h.read(h.tmuxLog), "-l /exit") })
	h.setAccount(func(a *Account) {
		a.APIUntil, a.ResetsAt = 0, 0
		a.FiveHour = &Window{UsedPct: 10, ResetsAt: time.Now().Unix() + 3600}
	})
	waitFor(t, "relaunch", 3*time.Second, func() bool { return len(h.launches()) >= 2 })
	if l := h.launches()[1]; l != "--resume sid-1 --settings "+r.settingsPath(Sub) {
		t.Fatalf("relaunch must use the fresh desired mode (SUB): %s", l)
	}
	h.quitOK(out)
}
