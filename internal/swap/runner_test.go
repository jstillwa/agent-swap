package swap

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Fake tmux: logs every invocation, one line each.
const fakeTmux = `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_TMUX_LOG"
`

// Fake claude: logs argv, plays the SessionStart hook + statusline by writing
// session.json, then exits 0 on each new "/exit" typed into the pane, or 7
// when $FAKE_QUIT exists (user quitting on their own).
const fakeClaude = `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_ARGS_LOG"
printf '{"claude_session_id":"sid-1","idle":true,"started_at":%s}' "$(date +%s)" > "$AGENT_SWAP_SESSION_DIR/session.json"
n=$(grep -c -- '-l /exit$' "$FAKE_TMUX_LOG")
while [ "$(grep -c -- '-l /exit$' "$FAKE_TMUX_LOG")" = "$n" ]; do
  [ -f "$FAKE_QUIT" ] && exit 7
  sleep 0.05
done
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
	if !strings.HasPrefix(l[0], "--settings "+r.settingsPath(Sub)) || !strings.HasSuffix(l[0], "--model haiku") {
		t.Fatalf("first launch: %s", l[0])
	}
	if l[1] != "--settings "+r.settingsPath(API)+" --resume sid-1 --model haiku" {
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
	if l := lines(argsLog)[2]; l != "--settings "+r.settingsPath(Sub)+" --resume sid-1 --model haiku" {
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
