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

// fakeAgent is a harness binary. It logs "<kind> <argv>" (and its ANTHROPIC_*
// and AGENT_SWAP_* env to $FAKE_ENV_LOG), appends one turn saying "<KIND>-SAID"
// to its native transcript, plays the SessionStart hook by writing
// session.json (native id $FAKE_SID; idle $FAKE_IDLE, default true) unless
// $FAKE_SILENT exists, then exits 0 on each new quit command typed into the
// pane (once the file $FAKE_EXIT_GATE exists, if set), or 7 when $FAKE_QUIT exists (the user
// quitting). With $FAKE_STUBBORN set it ignores quit, SIGTERM and SIGINT.
// @KIND@ @QUIT@ @SID@ @TRANSCRIPT@ are filled per kind.
const fakeAgent = `#!/bin/sh
printf '@KIND@ %s\n' "$*" >> "$FAKE_ARGS_LOG"
[ -n "$FAKE_ENV_LOG" ] && { env | grep -E '^(ANTHROPIC_|AGENT_SWAP_HARNESS=|AGENT_SWAP_EXE=)' | sort >> "$FAKE_ENV_LOG"; printf -- '--\n' >> "$FAKE_ENV_LOG"; }
sid=@SID@
if [ -n "$sid" ]; then
@TRANSCRIPT@
fi
[ -f "$FAKE_SILENT" ] || printf '{"native_id":"%s","idle":%s,"started_at":%s,"rate_limited":%s}' "$sid" "${FAKE_IDLE-true}" "$(date +%s)" "${FAKE_LIMITED-false}" > "$AGENT_SWAP_SESSION_DIR/session.json"
if [ -n "$FAKE_STUBBORN" ]; then
  trap '' TERM INT
  while :; do [ -f "$FAKE_QUIT" ] && exit 7; sleep 0.05; done
fi
n=$(grep -c -- '-l @QUIT@$' "$FAKE_TMUX_LOG")
while [ "$(grep -c -- '-l @QUIT@$' "$FAKE_TMUX_LOG")" = "$n" ]; do
  [ -f "$FAKE_QUIT" ] && exit 7
  sleep 0.05
done
[ -n "$FAKE_EXIT_GATE" ] && while [ ! -f "$FAKE_EXIT_GATE" ]; do sleep 0.05; done
exit 0
`

// Claude Code: ~/.claude/projects/<cwd slug>/<session id>.jsonl.
const claudeTranscript = `  d="$HOME/.claude/projects/$(pwd | sed 's/[^A-Za-z0-9]/-/g')"; mkdir -p "$d"
  printf '{"type":"user","message":{"role":"user","content":"CLAUDE-SAID"}}\n' >> "$d/$sid.jsonl"`

// pi: ~/.pi/agent/sessions/<dir>/<ts>_<session id>.jsonl.
const piTranscript = `  d="$HOME/.pi/agent/sessions/--fake--"; mkdir -p "$d"; f="$d/2026-01-01T00-00-00-000Z_$sid.jsonl"
  [ -f "$f" ] || printf '{"type":"session","version":3,"id":"%s","cwd":"%s"}\n' "$sid" "$PWD" > "$f"
  printf '{"type":"message","message":{"role":"user","content":[{"type":"text","text":"PI-SAID"}]}}\n' >> "$f"`

func fakeBin(kind, quit, sid, transcript string) string {
	return strings.NewReplacer("@KIND@", kind, "@QUIT@", quit, "@SID@", sid, "@TRANSCRIPT@", transcript).Replace(fakeAgent)
}

// slow bounds every wait; waits end as soon as their condition holds, so
// only a real failure (or a heavily loaded machine) uses it up.
const slow = 30 * time.Second

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(timeout); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// rig wires a Runner to the fake tmux and harnesses with sub-second timings.
type rig struct {
	t                     *testing.T
	tmp, tmuxLog, argsLog string
	quit, envLog, home    string
	cfg                   Config
	tweak                 func(*Runner) // applied before Run
}

func newRig(t *testing.T, stateDir string) *rig {
	t.Helper()
	tmp := t.TempDir()
	h := &rig{t: t, tmp: tmp, tmuxLog: filepath.Join(tmp, "tmux.log"), argsLog: filepath.Join(tmp, "args.log"),
		quit: filepath.Join(tmp, "quit"), envLog: filepath.Join(tmp, "env.log"), home: filepath.Join(tmp, "home")}
	h.write("tmux.log", "")
	t.Setenv("FAKE_TMUX_LOG", h.tmuxLog)
	t.Setenv("FAKE_ARGS_LOG", h.argsLog)
	t.Setenv("FAKE_QUIT", h.quit)
	t.Setenv("FAKE_SILENT", filepath.Join(tmp, "silent"))
	t.Setenv("TMPDIR", h.mkdir("tmp"))
	t.Setenv("FAKE_ENV_LOG", h.envLog)
	t.Setenv("HOME", h.home)
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
	cfg := Defaults()
	cfg.Targets = map[string]Target{"gateway": {BaseURL: "https://gw.example/anthropic", KeyCmd: "echo k"}}
	cfg.TmuxBin = h.write("tmux", fakeTmux)
	cfg.ClaudeBin = h.write("claude", fakeBin("claude", "/exit", `${FAKE_SID-sid-1}`, claudeTranscript))
	if stateDir == "" {
		stateDir = "state"
	}
	cfg.StateDir, cfg.Notify, cfg.StatuslineCmd = filepath.Join(tmp, stateDir), false, "true"
	cfg.Poll.Duration, cfg.SwitchBackDelay.Duration, cfg.SwitchBackTimeout.Duration = 50*time.Millisecond, time.Second, time.Second
	h.cfg = cfg
	return h
}

func (h *rig) write(name, body string) string {
	h.t.Helper()
	p := filepath.Join(h.tmp, name)
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		h.t.Fatal(err)
	}
	return p
}

func (h *rig) mkdir(name string) string {
	h.t.Helper()
	p := filepath.Join(h.tmp, name)
	if err := os.MkdirAll(p, 0o700); err != nil {
		h.t.Fatal(err)
	}
	return p
}

func (h *rig) setUsage(name string, fn func(*Usage)) {
	h.t.Helper()
	if err := Update(AccountPath(h.cfg.StateDir), func(a *Account) {
		if a.Harnesses == nil {
			a.Harnesses = map[string]*Usage{}
		}
		if a.Harnesses[name] == nil {
			a.Harnesses[name] = &Usage{}
		}
		fn(a.Harnesses[name])
	}); err != nil {
		h.t.Fatal(err)
	}
}

// block blocks every named harness for 10 minutes in ONE account update, so
// the runner never sees a half-applied state.
func (h *rig) block(names ...string) {
	h.t.Helper()
	if err := Update(AccountPath(h.cfg.StateDir), func(a *Account) {
		for _, n := range names {
			a.Get(n).BlockedUntil = time.Now().Unix() + 600
		}
	}); err != nil {
		h.t.Fatal(err)
	}
}

// hot puts claude's 5h window over the threshold, resetting in d.
func (h *rig) hot(d time.Duration) {
	h.setUsage("claude", func(u *Usage) {
		u.Windows = map[string]*Window{"5h": {UsedPct: 96, ResetsAt: time.Now().Add(d).Unix()}}
	})
}

// cool makes claude desired again.
func (h *rig) cool() {
	h.setUsage("claude", func(u *Usage) {
		u.BlockedUntil, u.ResetsAt = 0, 0
		u.Windows = map[string]*Window{"5h": {UsedPct: 10, ResetsAt: time.Now().Unix() + 3600}}
	})
}

type runResult struct {
	code int
	err  error
}

func (h *rig) start(target string, args []string) (*Runner, <-chan runResult) {
	h.t.Helper()
	r, err := NewRunner(h.cfg, "/cfg.toml", target, "/bin/cs", "%1", args)
	if err != nil {
		h.t.Fatal(err)
	}
	if h.tweak != nil {
		h.tweak(r)
	}
	h.t.Cleanup(func() { // on failure, show what the runner saw
		if h.t.Failed() {
			h.t.Logf("runner log:\n%s\ntmux log tail:\n%s", h.read(filepath.Join(r.Dir, "log")), tail(h.read(h.tmuxLog), 1500))
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*slow)
	h.t.Cleanup(cancel)
	out := make(chan runResult, 1)
	go func() { c, e := r.Run(ctx); out <- runResult{c, e} }()
	return r, out
}

func tail(s string, n int) string { return s[max(len(s)-n, 0):] }

func (h *rig) read(p string) string { b, _ := os.ReadFile(p); return string(b) }

func (h *rig) launches() []string {
	s := strings.TrimSpace(h.read(h.argsLog))
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func (h *rig) waitLaunches(n int) []string {
	h.t.Helper()
	waitFor(h.t, "launch", slow, func() bool { return len(h.launches()) >= n })
	return h.launches()
}

// handoff waits for the n-th message containing marker and returns the
// (unquoted) paths it names matching re's first group, in order.
func (h *rig) handoff(marker string, n int, re string) []string {
	h.t.Helper()
	waitFor(h.t, marker, slow, func() bool { return strings.Count(h.read(h.tmuxLog), marker) >= n })
	var lines []string
	for _, l := range strings.Split(h.read(h.tmuxLog), "\n") {
		if strings.Contains(l, marker) {
			lines = append(lines, l)
		}
	}
	m := regexp.MustCompile(re).FindStringSubmatch(lines[n-1])
	if m == nil {
		h.t.Fatalf("no path matching %s in %q", re, lines[n-1])
	}
	return m[1:]
}

const (
	reNotesReady = `([^\s'\x60]+/notes-[^\s'\x60]+\.md).*touch ([^\s'\x60]+/ready-[0-9a-f]+)`
	reContext    = `([^\s'\x60]+/context-[^\s'\x60]+\.md)`
)

func (h *rig) touch(p string, body string) {
	h.t.Helper()
	if err := os.WriteFile(strings.Trim(p, "'"), []byte(body), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// started waits until the fake harness has written its own session.json
// (the runner's seed has no started_at).
func (h *rig) started(r *Runner) {
	h.t.Helper()
	waitFor(h.t, "harness session", slow, func() bool { s, _ := Load[Session](SessionPath(r.Dir)); return s.StartedAt != 0 })
}

// contextOf reads the context file named in a launch line.
func (h *rig) contextOf(launch string) string {
	h.t.Helper()
	m := regexp.MustCompile(reContext).FindStringSubmatch(launch)
	if m == nil {
		h.t.Fatalf("launch has no context file: %s", launch)
	}
	return h.read(m[1])
}

// quitOK makes the fake harness quit on its own and expects Run to return 7.
func (h *rig) quitOK(out <-chan runResult) {
	h.t.Helper()
	h.touch(h.quit, "")
	if got := <-out; got.err != nil || got.code != 7 {
		h.t.Fatalf("run returned %d %v", got.code, got.err)
	}
}

func settings(r *Runner, name string) string { return filepath.Join(r.Dir, name+".settings.json") }

// claude → pi (claude over threshold) → claude (after its reset) → pi2
// (claude and pi blocked), carrying handoff notes and each side's transcript
// segment across.
func TestRunnerMultiHarnessCycle(t *testing.T) {
	h := newRig(t, "")
	h.cfg.Harnesses = []Harness{{Name: "claude", Kind: "claude"},
		{Name: "pi", Kind: "pi", Bin: h.write("pi", fakeBin("pi", "/quit", "pi-1", piTranscript)), Args: []string{"--provider", "ds"}},
		{Name: "pi2", Kind: "pi", Bin: h.write("pi2", fakeBin("pi2", "/quit", "pi-2", strings.ReplaceAll(piTranscript, "PI-SAID", "PI2-SAID")))}}
	h.hot(10 * time.Minute)
	r, out := h.start("", []string{"--model", "haiku"})

	// 1. Handoff down the list names a notes file and a ready file.
	p := h.handoff("EMERGENCY SHUTDOWN", 1, reNotesReady)
	handoffDir := filepath.Dir(p[0])
	if !strings.Contains(h.read(h.tmuxLog), "usage at 96% (5h)") {
		t.Error("handoff message lacks the usage reason")
	}
	if strings.Contains(h.read(h.tmuxLog), "-l /exit") {
		t.Fatal("/exit sent before the touch file existed")
	}
	for _, f := range p { // ready and notes files live in the temp dir, not the state dir
		if !strings.HasPrefix(f, os.Getenv("TMPDIR")+"/") {
			t.Errorf("%s not under TMPDIR %s", f, os.Getenv("TMPDIR"))
		}
	}
	tl := strings.Split(h.read(h.tmuxLog), "\n")
	for i, line := range tl { // send clears the input line first
		if strings.Contains(line, " -l ") && (i == 0 || tl[i-1] != "send-keys -t %1 C-u") {
			t.Errorf("typed text not preceded by C-u: %q after %q", line, tl[max(i-1, 0)])
		}
	}
	h.touch(p[0], "NOTES-DOWN")
	h.touch(p[1], "")

	// 2. pi starts fresh with its own args (not claude's) and a prompt naming
	// a context file holding the notes and claude's transcript.
	l := h.waitLaunches(2)
	if l[0] != "claude --model haiku --settings "+settings(r, "claude") {
		t.Fatalf("first launch: %s", l[0])
	}
	if !strings.HasPrefix(l[1], "pi ") || !strings.Contains(l[1], "--provider ds") || strings.Contains(l[1], "haiku") ||
		strings.Contains(l[1], "--session") {
		t.Fatalf("pi launch: %s", l[1])
	}
	ctx1 := h.contextOf(l[1])
	for _, want := range []string{"NOTES-DOWN", "CLAUDE-SAID"} {
		if !strings.Contains(ctx1, want) {
			t.Errorf("pi context lacks %s:\n%s", want, ctx1)
		}
	}
	a, _ := Load[Account](AccountPath(h.cfg.StateDir))
	if u := a.Harnesses["claude"]; u == nil || u.BlockedUntil != u.ResetsAt+1 {
		t.Fatalf("claude must be blocked until reset+delay: %+v", u)
	}
	waitFor(t, "pi env", slow, func() bool { return strings.Contains(h.read(h.envLog), "AGENT_SWAP_HARNESS=pi") })
	env := h.read(h.envLog)
	if !strings.Contains(env, "AGENT_SWAP_HARNESS=claude") || !strings.Contains(env, "AGENT_SWAP_HARNESS=pi") ||
		!strings.Contains(env, "AGENT_SWAP_EXE=/bin/cs") {
		t.Errorf("child env lacks AGENT_SWAP_HARNESS/EXE:\n%s", env)
	}

	waitFor(t, "fallback status", slow, func() bool { return strings.Contains(h.read(h.tmuxLog), "pi · back to claude at") })
	h.setUsage("claude", func(u *Usage) { // claude's window has reset (still 96%: must not re-trigger)
		u.BlockedUntil, u.ResetsAt = 0, 0
		u.Windows = map[string]*Window{"5h": {UsedPct: 96, ResetsAt: time.Now().Unix() - 1}}
	})

	// 3. After claude's reset: a return handoff in pi; claude resumes its own
	// session with a context holding the new notes and pi's segment only.
	p = h.handoff("ACCOUNT CYCLE (agent-swap)", 1, reNotesReady)
	h.touch(p[0], "NOTES-UP")
	h.touch(p[1], "")
	l = h.waitLaunches(3)
	if !strings.HasPrefix(l[2], "claude --resume sid-1 --model haiku ") || !strings.HasSuffix(l[2], " --settings "+settings(r, "claude")) {
		t.Fatalf("claude resume: %s", l[2])
	}
	ctx2 := h.contextOf(l[2])
	for _, want := range []string{"NOTES-UP", "PI-SAID"} {
		if !strings.Contains(ctx2, want) {
			t.Errorf("claude context lacks %s:\n%s", want, ctx2)
		}
	}
	for _, not := range []string{"NOTES-DOWN", "CLAUDE-SAID"} {
		if strings.Contains(ctx2, not) {
			t.Errorf("claude context repeats %s it already saw:\n%s", not, ctx2)
		}
	}

	// 4. The reset window must not re-trigger.
	time.Sleep(300 * time.Millisecond)
	if strings.Count(h.read(h.tmuxLog), "EMERGENCY SHUTDOWN") != 1 {
		t.Fatal("re-triggered on a reset window")
	}

	// 5. claude and pi blocked: pi2, which never ran, gets the notes first,
	// then the segments newest first, one per native session (claude's
	// second segment already holds its first).
	h.block("claude", "pi")
	p = h.handoff("EMERGENCY SHUTDOWN", 2, reNotesReady)
	h.touch(p[0], "NOTES-3")
	h.touch(p[1], "")
	l = h.waitLaunches(4)
	if !strings.HasPrefix(l[3], "pi2 ") || strings.Contains(l[3], "haiku") {
		t.Fatalf("pi2 launch: %s", l[3])
	}
	ctx3 := h.contextOf(l[3])
	notes, onClaude, onPi := strings.Index(ctx3, "NOTES-3"), strings.Index(ctx3, "## Conversation on claude"), strings.Index(ctx3, "## Conversation on pi")
	if notes < 0 || onClaude < 0 || onPi < 0 || !(notes < onClaude && onClaude < onPi) {
		t.Errorf("want notes, then claude (newest), then pi:\n%s", ctx3)
	}
	if n := strings.Count(ctx3, "## Conversation on claude"); n != 1 {
		t.Errorf("claude's session must appear once, got %d:\n%s", n, ctx3)
	}
	if n := strings.Count(ctx3, "CLAUDE-SAID"); n != 2 {
		t.Errorf("want claude's latest segment (2 turns), got %d turns:\n%s", n, ctx3)
	}
	if strings.Contains(ctx3, "NOTES-DOWN") || strings.Contains(ctx3, "NOTES-UP") {
		t.Errorf("only the latest handoff notes belong in the context:\n%s", ctx3)
	}
	h.quitOK(out)
	if _, err := os.Stat(handoffDir); !os.IsNotExist(err) {
		t.Errorf("handoff dir %s must be removed after a user quit: %v", handoffDir, err)
	}
}

// No [[harness]] tables: claude → api (target) relaunch keeps working. The
// positional prompt is used only on the first launch; the child never
// inherits ANTHROPIC_BASE_URL / ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN.
func TestRunnerLegacyTargetRelaunch(t *testing.T) {
	h := newRig(t, "")
	t.Setenv("ANTHROPIC_BASE_URL", "https://leak.example")
	t.Setenv("ANTHROPIC_API_KEY", "sk-leak")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok-leak")
	t.Setenv("ANTHROPIC_MODEL", "keep-me")
	h.hot(time.Minute)
	r, out := h.start("gateway", []string{"--model", "haiku", "fix bug"})

	p := h.handoff("EMERGENCY SHUTDOWN", 1, reNotesReady)
	h.touch(p[1], "")
	l := h.waitLaunches(2)
	if l[0] != "claude --model haiku fix bug --settings "+settings(r, "claude") {
		t.Fatalf("first launch must keep the prompt: %s", l[0])
	}
	if !strings.HasPrefix(l[1], "claude --resume sid-1 --model haiku ") || strings.Contains(l[1], "fix bug") ||
		!strings.HasSuffix(l[1], " --settings "+settings(r, "api")) {
		t.Fatalf("relaunch must resume on api settings without the prompt: %s", l[1])
	}
	if !strings.Contains(h.read(settings(r, "api")), `"apiKeyHelper": "/bin/cs key gateway"`) {
		t.Fatal("api settings lack apiKeyHelper")
	}
	if strings.Contains(h.read(settings(r, "claude")), "apiKeyHelper") {
		t.Fatal("subscription settings must not set a key")
	}
	waitFor(t, "api env", slow, func() bool { return strings.Count(h.read(h.envLog), "--\n") >= 2 })
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

// An empty native session id blocks the switch (no quit, no relaunch).
func TestRunnerEmptySessionIDBlocksSwitch(t *testing.T) {
	h := newRig(t, "")
	t.Setenv("FAKE_SID", "")
	h.hot(time.Minute)
	r, out := h.start("gateway", nil)
	h.touch(h.handoff("EMERGENCY SHUTDOWN", 1, reNotesReady)[1], "")
	h.started(r)
	waitFor(t, "switch blocked log", slow, func() bool {
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

// An unreadable account.json fails Run before any harness starts.
func TestRunnerCorruptAccountFails(t *testing.T) {
	h := newRig(t, "")
	if err := os.MkdirAll(h.cfg.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(AccountPath(h.cfg.StateDir), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, out := h.start("gateway", nil)
	got := <-out
	if got.err == nil || !strings.Contains(got.err.Error(), AccountPath(h.cfg.StateDir)) {
		t.Fatalf("want error naming %s, got %d %v", AccountPath(h.cfg.StateDir), got.code, got.err)
	}
	if n := len(h.launches()); n != 0 {
		t.Fatalf("no harness may start, got %d launches", n)
	}
}

// A desired-harness flip mid-handoff cancels it and removes the ready file.
func TestRunnerCancelsHandoffOnFlip(t *testing.T) {
	h := newRig(t, "")
	t.Setenv("FAKE_IDLE", "false") // busy: a touched file alone cannot switch
	h.hot(time.Minute)
	r, out := h.start("gateway", nil)
	ready := h.handoff("EMERGENCY SHUTDOWN", 1, reNotesReady)[1]
	h.started(r) // busy, as the fake reports
	h.touch(ready, "")
	time.Sleep(150 * time.Millisecond)
	if _, err := os.Stat(ready); err != nil {
		t.Fatal("ready file removed before the flip")
	}
	h.cool()
	waitFor(t, "ready file removed", slow, func() bool { _, err := os.Stat(ready); return os.IsNotExist(err) })
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

// {file} and {notes} are shell-quoted when the path needs it.
func TestRunnerQuotesHandoffPaths(t *testing.T) {
	h := newRig(t, "")
	tmp := h.mkdir("tmp dir") // space in the path
	t.Setenv("TMPDIR", tmp)
	h.hot(time.Minute)
	_, out := h.start("gateway", nil)
	p := h.handoff("EMERGENCY SHUTDOWN", 1, `'([^']+/notes-[^']+\.md)'.*touch '([^']+/ready-[0-9a-f]+)'`)
	for _, f := range p {
		if !strings.HasPrefix(f, tmp+"/") {
			t.Fatalf("handoff path %q not under %q", f, tmp)
		}
	}
	h.quitOK(out)
}

// childEnv drops inherited credential, base-URL and provider vars only.
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

// After the child exits, the next harness comes from a fresh account load:
// a flip back while the quit is in flight relaunches the same harness.
func TestRunnerRelaunchFromFreshAccount(t *testing.T) {
	h := newRig(t, "")
	gate := filepath.Join(h.tmp, "exit-gate")
	t.Setenv("FAKE_EXIT_GATE", gate)
	h.hot(time.Minute)
	r, out := h.start("gateway", nil)
	h.touch(h.handoff("EMERGENCY SHUTDOWN", 1, reNotesReady)[1], "")
	waitFor(t, "/exit", slow, func() bool { return strings.Contains(h.read(h.tmuxLog), "-l /exit") })
	h.cool()
	h.touch(gate, "")
	l := h.waitLaunches(2)
	if l[1] != "claude --resume sid-1 --settings "+settings(r, "claude") {
		t.Fatalf("relaunch must resume the fresh desired harness (claude) with no prompt: %s", l[1])
	}
	h.quitOK(out)
}

// C3 + C5: a relaunch seeds session.json with the known native id (so a
// switch is possible before the harness reports one) and, since it carries a
// prompt, starts busy.
func TestRunnerRelaunchSeedsSession(t *testing.T) {
	h := newRig(t, "")
	h.hot(time.Minute)
	r, out := h.start("gateway", nil)
	ready := h.handoff("EMERGENCY SHUTDOWN", 1, reNotesReady)[1]
	h.started(r)
	h.touch(os.Getenv("FAKE_SILENT"), "") // the relaunched fake reports nothing
	h.touch(ready, "")
	h.waitLaunches(2)
	time.Sleep(3 * h.cfg.Poll.Duration)
	s, err := Load[Session](SessionPath(r.Dir))
	if err != nil || s.NativeID != "sid-1" || s.Idle {
		t.Fatalf("want native_id sid-1 and busy after a prompted relaunch: %+v %v", s, err)
	}
	h.quitOK(out)
}

// C4: pass-through args go only to an order[0]-kind harness on the first
// launch; later only claude gets them, through ReplayArgs.
func TestRunnerPassThroughArgsOnlyForFirstKind(t *testing.T) {
	h := newRig(t, "")
	h.cfg.Harnesses = []Harness{{Name: "claude", Kind: "claude"},
		{Name: "pi", Kind: "pi", Bin: h.write("pi", fakeBin("pi", "/quit", "pi-1", piTranscript))}}
	h.setUsage("claude", func(u *Usage) { u.BlockedUntil = time.Now().Unix() + 600 })
	r, out := h.start("", []string{"--model", "haiku", "fix bug"})
	l := h.waitLaunches(1)
	h.setUsage("claude", func(u *Usage) { u.BlockedUntil = 0 })
	if !strings.HasPrefix(l[0], "pi ") || strings.Contains(l[0], "haiku") || strings.Contains(l[0], "fix bug") {
		t.Fatalf("pi must not get claude's args: %s", l[0])
	}
	h.touch(h.handoff("ACCOUNT CYCLE (agent-swap)", 1, reNotesReady)[1], "")
	l = h.waitLaunches(2)
	if !strings.HasPrefix(l[1], "claude --model haiku ") || strings.Contains(l[1], "fix bug") || strings.Contains(l[1], "--resume") ||
		!strings.HasSuffix(l[1], " --settings "+settings(r, "claude")) {
		t.Fatalf("claude gets ReplayArgs on a fresh launch: %s", l[1])
	}
	h.quitOK(out)
}

// C6: a harness that ignores the quit command, SIGINT and SIGTERM is killed.
func TestRunnerKillsStubbornHarness(t *testing.T) {
	h := newRig(t, "")
	t.Setenv("FAKE_STUBBORN", "1")
	h.hot(time.Minute)
	h.tweak = func(r *Runner) { r.QuitRetry, r.QuitKill = 100*time.Millisecond, 300*time.Millisecond }
	_, out := h.start("gateway", nil)
	h.touch(h.handoff("EMERGENCY SHUTDOWN", 1, reNotesReady)[1], "")
	h.waitLaunches(2)
	h.quitOK(out)
}

// L2: a rate-limit error (no usage windows) names itself as the reason.
func TestRunnerHandoffReasonRateLimit(t *testing.T) {
	h := newRig(t, "")
	t.Setenv("FAKE_IDLE", "false")
	t.Setenv("FAKE_LIMITED", "true")
	_, out := h.start("gateway", nil)
	h.handoff("EMERGENCY SHUTDOWN", 1, reNotesReady)
	if !strings.Contains(h.read(h.tmuxLog), "a rate-limit error") {
		t.Errorf("handoff message lacks the rate-limit reason:\n%s", h.read(h.tmuxLog))
	}
	h.quitOK(out)
}

// L1: a newest segment over context_max_bytes keeps its last turns; the notes
// stay and older segments are dropped.
func TestRunnerContextCapKeepsNewestTurns(t *testing.T) {
	h := newRig(t, "")
	h.cfg.ContextMaxBytes = 2000
	h.cfg.Harnesses = []Harness{{Name: "claude", Kind: "claude"},
		{Name: "pi", Kind: "pi", Bin: h.write("pi", fakeBin("pi", "/quit", "pi-1", piTranscript))},
		{Name: "pi2", Kind: "pi", Bin: h.write("pi2", fakeBin("pi2", "/quit", "pi-2", piTranscript))}}
	cwd, _ := os.Getwd()
	dir := filepath.Join(h.home, ".claude", "projects", regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(cwd, "-"))
	big := `{"type":"user","message":{"role":"user","content":"FIRST-TURN"}}` + "\n" +
		strings.Repeat(`{"type":"assistant","message":{"role":"assistant","content":"`+strings.Repeat("filler ", 30)+`"}}`+"\n", 50)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	h.write(filepath.Join(dir[len(h.tmp)+1:], "sid-1.jsonl"), big)
	h.setUsage("claude", func(u *Usage) { u.BlockedUntil = time.Now().Unix() + 600 })
	_, out := h.start("", nil)
	if l := h.waitLaunches(1); !strings.HasPrefix(l[0], "pi ") {
		t.Fatalf("pi must start while claude is blocked: %s", l[0])
	}
	h.setUsage("claude", func(u *Usage) { u.BlockedUntil = 0 })
	h.touch(h.handoff("ACCOUNT CYCLE (agent-swap)", 1, reNotesReady)[1], "") // pi → claude
	h.waitLaunches(2)
	h.block("claude", "pi")
	p := h.handoff("EMERGENCY SHUTDOWN", 1, reNotesReady) // claude → pi2
	h.touch(p[0], "NOTES-CAP")
	h.touch(p[1], "")
	l := h.waitLaunches(3)
	ctx := h.contextOf(l[2])
	if !strings.Contains(ctx, "NOTES-CAP") || !strings.Contains(ctx, "CLAUDE-SAID") {
		t.Errorf("context must keep the notes and the newest turn:\n%s", ctx)
	}
	if strings.Contains(ctx, "FIRST-TURN") || strings.Contains(ctx, "PI-SAID") {
		t.Errorf("context must drop the oldest turns and older segments:\n%s", ctx)
	}
	if len(ctx) > 3000 {
		t.Errorf("context is %d bytes, cap 2000", len(ctx))
	}
	h.quitOK(out)
}
