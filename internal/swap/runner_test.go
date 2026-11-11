package swap

import (
	"context"
	"os"
	"os/exec"
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

// fakeAgent is a harness binary. It answers --version, logs "<kind> <argv>"
// (and its ANTHROPIC_* and AGENT_SWAP_* env to $FAKE_ENV_LOG), resumes the
// session named by --resume (claude) or --session (pi), and plays one turn
// (user "Q-@SAID@", assistant "@SAID@") into its native session file in the
// real format. It then reports like the Stop hook by writing session.json
// (native id, idle $FAKE_IDLE (default true), the turn count, and failed plus
// rate_limited when $FAKE_FAIL_KIND names its kind) unless $FAKE_SILENT
// exists. While waiting it plays one more turn per $FAKE_TURN file (its
// content is the text). It exits 0 on each new quit command typed into the
// pane (once $FAKE_EXIT_GATE exists, if set), or 7 when $FAKE_QUIT exists (the
// user quitting). With $FAKE_STUBBORN set it ignores quit, SIGTERM and SIGINT.
const fakeAgent = `#!/bin/sh
[ -n "$FAKE_NO_STDERR" ] && exec 2>/dev/null
[ "$1" = --version ] && { echo "2.1.0 (Claude Code)"; exit 0; }
printf '@KIND@ %s\n' "$*" >> "$FAKE_ARGS_LOG"
[ -n "$FAKE_ENV_LOG" ] && { env | grep -E '^(ANTHROPIC_|AGENT_SWAP_HARNESS=|AGENT_SWAP_EXE=)' | sort >> "$FAKE_ENV_LOG"; printf -- '--\n' >> "$FAKE_ENV_LOG"; }
sid=@SID@; f=""; prev=""
for a in "$@"; do
  case "$prev" in --resume) sid=$a;; --session) f=$a;; esac
  [ "$a" = --fork-session ] && fork=1
  prev=$a
done
[ -n "$fork" ] && sid=@SID@
ts() { perl -MTime::HiRes=time -MPOSIX=strftime -e '$t=time; printf "%s.%03dZ", strftime("%Y-%m-%dT%H:%M:%S", gmtime($t)), ($t-int($t))*1000'; }
n=0
@SETUP@
turn() {
  [ -n "$sid" ] || return 0
  now=$(ts); a="u$$-$n-a"; b="u$$-$n-b"
@TURN@
  n=$((n+1))
}
report() {
  failed=false; lim=${FAKE_LIMITED-false}
  [ "$FAKE_FAIL_KIND" = @KIND@ ] && { failed=true; lim=true; }
  [ -f "$FAKE_SILENT" ] || printf '{"native_id":"%s","idle":%s,"started_at":%s,"rate_limited":%s,"failed":%s,"turns":%s}' "$sid" "${FAKE_IDLE-true}" "$(date +%s)" "$lim" "$failed" "$n" > "$AGENT_SWAP_SESSION_DIR/.fake-session" &&
    mv "$AGENT_SWAP_SESSION_DIR/.fake-session" "$AGENT_SWAP_SESSION_DIR/session.json" # atomic, as the hooks' Update
}
q=$(grep -c -- '-l @QUIT@$' "$FAKE_TMUX_LOG") # before the report: a quit may follow it at once
turn "@SAID@"
report
if [ -n "$FAKE_STUBBORN" ]; then
  trap '' TERM INT
  while :; do [ -f "$FAKE_QUIT" ] && exit 7; sleep 0.05; done
fi
while [ "$(grep -c -- '-l @QUIT@$' "$FAKE_TMUX_LOG")" = "$q" ]; do
  [ -f "$FAKE_QUIT" ] && exit 7
  if [ -f "$FAKE_TURN" ]; then t=$(cat "$FAKE_TURN"); turn "$t"; report; rm -f "$FAKE_TURN"; fi
  sleep 0.05
done
[ -n "$FAKE_EXIT_GATE" ] && while [ ! -f "$FAKE_EXIT_GATE" ]; do sleep 0.05; done
exit 0
`

// Claude Code: <root>/projects/<cwd slug>/<session id>.jsonl, lines chained by
// uuid/parentUuid from the last line's uuid.
const (
	claudeSetup = `d="$HOME/.claude/projects/$(pwd | sed 's/[^A-Za-z0-9]/-/g')"`
	claudeTurn  = `  mkdir -p "$d"; f="$d/$sid.jsonl"; p=null
  [ -s "$f" ] && p="\"$(tail -n1 "$f" | sed 's/.*"uuid":"\([^"]*\)".*/\1/')\""
  printf '{"type":"user","uuid":"%s","parentUuid":%s,"timestamp":"%s","sessionId":"%s","cwd":"%s","message":{"role":"user","content":"Q-%s"}}\n' "$a" "$p" "$now" "$sid" "$PWD" "$1" >> "$f"
  printf '{"type":"assistant","uuid":"%s","parentUuid":"%s","timestamp":"%s","sessionId":"%s","cwd":"%s","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"%s"}]}}\n' "$b" "$a" "$now" "$sid" "$PWD" "$1" >> "$f"`
)

// pi: <root>/--<cwd>--/<ts>_<session id>.jsonl, a header then entries chained
// by id/parentId; --session <path> resumes that file.
const (
	piSetup = `root="${PI_CODING_AGENT_SESSION_DIR:-$HOME/.pi/agent/sessions}"
if [ -n "$f" ]; then sid=$(basename "$f" .jsonl); sid=${sid##*_}
else f="$root/--$(pwd | sed 's|^/||; s|[/\\:]|-|g')--/2026-01-01T00-00-00-000Z_$sid.jsonl"; fi`
	piTurn = `  mkdir -p "$(dirname "$f")"
  [ -s "$f" ] || printf '{"type":"session","version":3,"id":"%s","timestamp":"%s","cwd":"%s"}\n' "$sid" "$now" "$PWD" > "$f"
  p=null; tail -n1 "$f" | grep -q '"type":"session"' || p="\"$(tail -n1 "$f" | sed 's/.*"id":"\([^"]*\)".*/\1/')\""
  printf '{"type":"message","id":"%s","parentId":%s,"timestamp":"%s","message":{"role":"user","content":[{"type":"text","text":"Q-%s"}]}}\n' "$a" "$p" "$now" "$1" >> "$f"
  printf '{"type":"message","id":"%s","parentId":"%s","timestamp":"%s","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"%s"}]}}\n' "$b" "$a" "$now" "$1" >> "$f"`
)

// fakeBin fills the fake for one harness. said is the text of its turns.
func fakeBin(kind, quit, sid, said, setup, turn string) string {
	return strings.NewReplacer("@KIND@", kind, "@QUIT@", quit, "@SID@", sid, "@SAID@", said,
		"@SETUP@", setup, "@TURN@", turn).Replace(fakeAgent)
}

func fakeClaude() string {
	return fakeBin("claude", "/exit", `${FAKE_SID-sid-1}`, "CLAUDE-SAID", claudeSetup, claudeTurn)
}
func fakePi(name, sid, said string) string {
	return fakeBin(name, "/quit", sid, said, piSetup, piTurn)
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
	turnFile, cwd         string
	cfg                   Config
	tweak                 func(*Runner) // applied before Run
}

func newRig(t *testing.T, stateDir string) *rig {
	t.Helper()
	tmp := realTemp(t)
	cwd, _ := os.Getwd()
	h := &rig{t: t, tmp: tmp, tmuxLog: filepath.Join(tmp, "tmux.log"), argsLog: filepath.Join(tmp, "args.log"),
		quit: filepath.Join(tmp, "quit"), envLog: filepath.Join(tmp, "env.log"), home: filepath.Join(tmp, "home"),
		turnFile: filepath.Join(tmp, "turn"), cwd: cwd}
	h.write("tmux.log", "")
	t.Setenv("FAKE_TMUX_LOG", h.tmuxLog)
	t.Setenv("FAKE_ARGS_LOG", h.argsLog)
	t.Setenv("FAKE_QUIT", h.quit)
	t.Setenv("FAKE_TURN", h.turnFile)
	t.Setenv("FAKE_SILENT", filepath.Join(tmp, "silent"))
	t.Setenv("TMPDIR", h.mkdir("tmp"))
	t.Setenv("FAKE_ENV_LOG", h.envLog)
	t.Setenv("HOME", h.home)
	for _, k := range []string{"PI_CODING_AGENT_SESSION_DIR", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "FAKE_FAIL_KIND", "FAKE_EXIT_GATE", "FAKE_NO_STDERR"} {
		t.Setenv(k, "")
	}
	cfg := Defaults()
	cfg.Targets = map[string]Target{"gateway": {BaseURL: "https://gw.example/anthropic", KeyCmd: "echo k"}}
	cfg.TmuxBin = h.write("tmux", fakeTmux)
	cfg.ClaudeBin = h.write("claude", fakeClaude())
	if stateDir == "" {
		stateDir = "state"
	}
	cfg.StateDir, cfg.Notify, cfg.StatuslineCmd = filepath.Join(tmp, stateDir), false, "true"
	cfg.Poll.Duration, cfg.SwitchBackDelay.Duration = 50*time.Millisecond, time.Second
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

// pi adds a fake pi harness (session id sid when launched fresh).
func (h *rig) pi(name, sid, said string, args ...string) Harness {
	return Harness{Name: name, Kind: "pi", Bin: h.write(name, fakePi(name, sid, said)), Args: args}
}

func (h *rig) setUsage(name string, fn func(*Usage)) {
	h.t.Helper()
	if err := Update(AccountPath(h.cfg.StateDir), func(a *Account) { fn(a.Get(name)) }); err != nil {
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
			h.t.Logf("runner log:\n%s\ntmux log tail:\n%s\nlaunches:\n%s", h.read(filepath.Join(r.Dir, "log")),
				tail(h.read(h.tmuxLog), 1500), h.read(h.argsLog))
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*slow)
	h.t.Cleanup(cancel)
	out := make(chan runResult, 1)
	go func() { c, e := r.Run(ctx); out <- runResult{c, e} }()
	return r, out
}

// startPinned starts the run with request.json already pinning harness pin.
func (h *rig) startPinned(pin string) (*Runner, <-chan runResult) {
	h.t.Helper()
	tweak := h.tweak
	h.tweak = func(r *Runner) {
		if tweak != nil {
			tweak(r)
		}
		if err := Update(RequestPath(r.Dir), func(q *Request) { q.Pin = pin }); err != nil {
			h.t.Fatal(err)
		}
	}
	return h.start("", nil)
}

func tail(s string, n int) string { return s[max(len(s)-n, 0):] }

func (h *rig) read(p string) string { b, _ := os.ReadFile(p); return string(b) }

func (h *rig) runLog(r *Runner) string { return h.read(filepath.Join(r.Dir, "log")) }

func (h *rig) waitLog(r *Runner, s string) {
	h.t.Helper()
	waitFor(h.t, "run log: "+s, slow, func() bool { return strings.Contains(h.runLog(r), s) })
}

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

func (h *rig) touch(p string, body string) {
	h.t.Helper()
	if err := os.WriteFile(strings.Trim(p, "'"), []byte(body), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// started waits until the fake harness has written its own session.json
// (the runner's seed has no started_at).
func (h *rig) started(r *Runner) Session {
	h.t.Helper()
	var s Session
	waitFor(h.t, "harness session", slow, func() bool { s, _ = Load[Session](SessionPath(r.Dir)); return s.StartedAt != 0 })
	return s
}

// say makes the running fake play one more turn and waits until it reported it.
func (h *rig) say(r *Runner, text string) {
	h.t.Helper()
	before, _ := Load[Session](SessionPath(r.Dir))
	h.touch(h.turnFile, text)
	waitFor(h.t, "turn "+text, slow, func() bool {
		s, _ := Load[Session](SessionPath(r.Dir))
		_, err := os.Stat(h.turnFile)
		return os.IsNotExist(err) && s.Turns > before.Turns
	})
}

// request applies fn to the run's request.json, as `agent-swap use/sync` do.
func (h *rig) request(r *Runner, fn func(*Request)) {
	h.t.Helper()
	if err := Update(RequestPath(r.Dir), fn); err != nil {
		h.t.Fatal(err)
	}
}

func (h *rig) mirrors(r *Runner) map[string]MirrorStatus {
	m, _ := Load[map[string]MirrorStatus](MirrorsPath(r.Dir))
	return m
}

func (h *rig) rt() Runtime { return Runtime{Home: h.home, Cwd: h.cwd} }

// sessionTexts reads a native session from the start through its adapter.
func (h *rig) sessionTexts(kind, native string) []string {
	h.t.Helper()
	ad, _ := AdapterFor(kind)
	got, _, err := ad.Turns(native, "", h.rt())
	if err != nil {
		h.t.Fatalf("%s session %s: %v", kind, native, err)
	}
	return texts(got)
}

const reContext = `([^\s'\x60]+/context-[^\s'\x60]+\.md)`

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

// claude → pi (claude over threshold) → claude (after its reset). Each switch
// writes only the target's session, which then resumes with no prompt (the
// last turn ended normally); claude's own session gets pi's turns appended.
func TestRunnerSwitchMaterializesTarget(t *testing.T) {
	h := newRig(t, "")
	h.cfg.Harnesses = []Harness{{Name: "claude", Kind: "claude"}, h.pi("pi", "pi-1", "PI-SAID", "--provider", "ds"),
		{Name: "codex", Kind: "codex", Bin: "/nonexistent/codex"}}
	h.hot(10 * time.Minute)
	r, out := h.start("", []string{"--model", "haiku"})

	l := h.waitLaunches(2)
	if l[0] != "claude --model haiku --settings "+settings(r, "claude") {
		t.Fatalf("first launch: %s", l[0])
	}
	ms := h.mirrors(r)
	pi := ms["pi"]
	if pi.Native == "" || !strings.HasPrefix(pi.Path, filepath.Join(h.home, ".pi", "agent", "sessions")+"/") {
		t.Fatalf("pi mirror: %+v", ms)
	}
	if want := "pi -e " + filepath.Join(r.Dir, "agent-swap-pi.ts") + " --session " + pi.Path + " --provider ds"; l[1] != want {
		t.Fatalf("pi must resume its new session with no prompt:\n got %s\nwant %s", l[1], want)
	}
	if got := h.sessionTexts("pi", pi.Native); !slices.Equal(got[:2], []string{"Q-CLAUDE-SAID", "CLAUDE-SAID"}) {
		t.Fatalf("pi session lacks claude's turn: %q", got)
	}
	if mode(t, pi.Path) != 0o600 {
		t.Errorf("pi session mode %o", mode(t, pi.Path))
	}
	if ms["claude"].Native != "sid-1" || len(ms) != 2 {
		t.Errorf("mirrors: %+v", ms)
	}
	if _, err := os.Stat(filepath.Join(h.home, ".codex")); err == nil {
		t.Error("codex is not the target: its store must not be written")
	}
	if strings.Contains(h.runLog(r), "context file") {
		t.Error("no context file when the mirror was written")
	}
	a, _ := Load[Account](AccountPath(h.cfg.StateDir))
	if u := a.Harnesses["claude"]; u == nil || u.BlockedUntil != u.ResetsAt+1 {
		t.Fatalf("claude must be blocked until reset+delay: %+v", u)
	}
	waitFor(t, "pi env", slow, func() bool { return strings.Contains(h.read(h.envLog), "AGENT_SWAP_HARNESS=pi") })
	if env := h.read(h.envLog); !strings.Contains(env, "AGENT_SWAP_HARNESS=claude") || !strings.Contains(env, "AGENT_SWAP_EXE=/bin/cs") {
		t.Errorf("child env lacks AGENT_SWAP_HARNESS/EXE:\n%s", env)
	}
	tl := strings.Split(h.read(h.tmuxLog), "\n")
	for i, line := range tl { // send clears the input line first
		if strings.Contains(line, " -l ") && (i == 0 || tl[i-1] != "send-keys -t %1 C-u") {
			t.Errorf("typed text not preceded by C-u: %q after %q", line, tl[max(i-1, 0)])
		}
	}
	if !strings.Contains(h.read(h.tmuxLog), "set-option -p -t %1 @agent-swap-dir "+r.Dir) {
		t.Error("the pane option for `agent-swap use` is not set")
	}

	// pi played its turn; claude's reset passes (still 96%: no re-trigger).
	waitFor(t, "fallback status", slow, func() bool { return strings.Contains(h.read(h.tmuxLog), "pi · back to claude at") })
	h.setUsage("claude", func(u *Usage) {
		u.BlockedUntil, u.ResetsAt = 0, 0
		u.Windows = map[string]*Window{"5h": {UsedPct: 96, ResetsAt: time.Now().Unix() - 1}}
	})
	l = h.waitLaunches(3)
	if l[2] != "claude --resume sid-1 --model haiku --settings "+settings(r, "claude") {
		t.Fatalf("claude must resume its own session with no prompt: %s", l[2])
	}
	waitFor(t, "claude's second turn", slow, func() bool {
		return strings.Count(h.read(filepath.Join(h.home, ".claude", "projects", claudeSlug(h.cwd), "sid-1.jsonl")), "\n") >= 6
	})
	want := []string{"Q-CLAUDE-SAID", "CLAUDE-SAID", "Q-PI-SAID", "PI-SAID", "Q-CLAUDE-SAID", "CLAUDE-SAID"}
	if got := h.sessionTexts("claude", "sid-1"); !slices.Equal(got, want) {
		t.Fatalf("claude's session must hold pi's turns on its chain:\n got %q\nwant %q", got, want)
	}
	h.waitLog(r, "sync: claude appended 2 turns to sid-1")
	time.Sleep(300 * time.Millisecond)
	if n := len(h.launches()); n != 3 {
		t.Fatalf("re-triggered on a reset window: %d launches", n)
	}
	h.quitOK(out)
}

// A turn cut off by a usage limit (StopFailure) moves on with the continue
// prompt; the prompt names no file.
func TestRunnerContinuePromptAfterStopFailure(t *testing.T) {
	h := newRig(t, "")
	t.Setenv("FAKE_FAIL_KIND", "claude")
	h.cfg.Harnesses = []Harness{{Name: "claude", Kind: "claude"}, h.pi("pi", "pi-1", "PI-SAID")}
	r, out := h.start("", nil)
	l := h.waitLaunches(2)
	if !strings.HasPrefix(l[1], "pi -e ") || !strings.Contains(l[1], " --session ") ||
		!strings.HasSuffix(l[1], " [agent-swap] Moved here from claude because a usage limit interrupted the last turn. "+
			"No new user input. Resume the interrupted work; if you were waiting for the user, ask again.") {
		t.Fatalf("pi launch: %s", l[1])
	}
	if strings.Contains(l[1], "context-") {
		t.Fatalf("the continue prompt must not name a file: %s", l[1])
	}
	h.waitLog(r, "rate_limited=true failed=true")
	h.quitOK(out)
}

// `use` (the pin in request.json) switches at the next idle, and holds while
// the unpinned choice differs; `use auto` returns to it.
func TestRunnerUsePin(t *testing.T) {
	h := newRig(t, "")
	h.cfg.Harnesses = []Harness{{Name: "claude", Kind: "claude"}, h.pi("pi", "pi-1", "PI-SAID")}
	r, out := h.start("", nil)
	h.started(r)
	if err := Update(SessionPath(r.Dir), func(s *Session) { s.Idle = false }); err != nil { // mid-turn
		t.Fatal(err)
	}
	time.Sleep(4 * h.cfg.Poll.Duration) // a tick that loaded the idle session must not see the pin
	h.request(r, func(q *Request) { q.Pin = "pi" })
	h.waitLog(r, `pin "pi"`)
	time.Sleep(6 * h.cfg.Poll.Duration)
	if strings.Contains(h.read(h.tmuxLog), "-l /exit") || len(h.launches()) != 1 {
		t.Fatal("a pin must not cut a turn off")
	}
	if err := Update(SessionPath(r.Dir), func(s *Session) { s.Idle = true }); err != nil {
		t.Fatal(err)
	}
	l := h.waitLaunches(2)
	if !strings.HasPrefix(l[1], "pi ") || !strings.Contains(l[1], "--session") {
		t.Fatalf("pi launch: %s", l[1])
	}
	waitFor(t, "pi session", slow, func() bool { s, _ := Load[Session](SessionPath(r.Dir)); return s.Turns > 0 })
	time.Sleep(6 * h.cfg.Poll.Duration) // claude is desired, but the pin holds
	if n := len(h.launches()); n != 2 {
		t.Fatalf("the pin must survive the relaunch: %d launches", n)
	}
	if q, _ := Load[Request](RequestPath(r.Dir)); q.Pin != "pi" {
		t.Fatalf("request.json lost the pin: %+v", q)
	}
	h.request(r, func(q *Request) { q.Pin = "" }) // use auto
	l = h.waitLaunches(3)
	if l[2] != "claude --resume sid-1 --settings "+settings(r, "claude") {
		t.Fatalf("use auto must return to claude's own session: %s", l[2])
	}
	h.quitOK(out)
}

// A turn counter bump reads the new turn into the log; `sync` writes it into
// a harness that is not running.
func TestRunnerTurnBumpAndSync(t *testing.T) {
	h := newRig(t, "")
	h.cfg.Harnesses = []Harness{{Name: "claude", Kind: "claude"}, h.pi("pi", "pi-1", "PI-SAID"), h.pi("pi2", "pi-2", "PI2-SAID")}
	r, out := h.start("", nil)
	h.started(r)
	h.say(r, "FACT-2")
	h.request(r, func(q *Request) { q.Sync = []string{"pi"} })
	waitFor(t, "pi mirror", slow, func() bool { return h.mirrors(r)["pi"].Native != "" })
	pi := h.mirrors(r)["pi"]
	want := []string{"Q-CLAUDE-SAID", "CLAUDE-SAID", "Q-FACT-2", "FACT-2"}
	if got := h.sessionTexts("pi", pi.Native); !slices.Equal(got, want) {
		t.Fatalf("synced pi session %q want %q", got, want)
	}
	if pi.Behind != 0 {
		t.Errorf("pi behind %d", pi.Behind)
	}
	waitFor(t, "sync request cleared", slow, func() bool { q, _ := Load[Request](RequestPath(r.Dir)); return len(q.Sync) == 0 })

	// Another turn, then sync all: the pi mirror is appended to (pi and pi2
	// share it), and the running kind is skipped.
	h.say(r, "FACT-3")
	h.request(r, func(q *Request) { q.Sync = []string{"*"} })
	h.waitLog(r, "sync: pi appended 2 turns to "+pi.Native)
	if got := h.sessionTexts("pi", pi.Native); !slices.Equal(got, append(want, "Q-FACT-3", "FACT-3")) {
		t.Fatalf("second sync: %q", got)
	}
	if n := len(h.launches()); n != 1 {
		t.Fatalf("sync must not switch: %d launches", n)
	}
	if strings.Contains(h.runLog(r), "sync: claude") {
		t.Error("the running harness must not be synced")
	}
	h.quitOK(out)
}

// A native id change (claude /clear) restarts the conversation from the new
// session; an id with no session file pauses sync instead.
func TestRunnerEpochOnNativeIDChange(t *testing.T) {
	h := newRig(t, "")
	h.cfg.Harnesses = []Harness{{Name: "claude", Kind: "claude"}, h.pi("pi", "pi-1", "PI-SAID")}
	r, out := h.start("", nil)
	h.started(r)
	h.request(r, func(q *Request) { q.Sync = []string{"pi"} })
	waitFor(t, "pi mirror", slow, func() bool { return h.mirrors(r)["pi"].Native != "" })

	dir := filepath.Join(h.home, ".claude", "projects", claudeSlug(h.cwd))
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	h.touch(filepath.Join(dir, "sid-2.jsonl"),
		`{"type":"user","uuid":"n1","parentUuid":null,"cwd":"`+h.cwd+`","timestamp":"`+now+`","message":{"role":"user","content":"NEW-Q"}}`+"\n"+
			`{"type":"assistant","uuid":"n2","parentUuid":"n1","timestamp":"`+now+`","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"NEW-EPOCH"}]}}`+"\n")
	if err := Update(SessionPath(r.Dir), func(s *Session) { s.NativeID, s.Turns = "sid-2", s.Turns+1 }); err != nil {
		t.Fatal(err)
	}
	h.waitLog(r, "epoch: claude switched to session sid-2")
	waitFor(t, "mirrors.json after the epoch", slow, func() bool { return h.mirrors(r)["claude"].Native == "sid-2" })
	if ms := h.mirrors(r); ms["pi"].Native != "" || len(ms) != 1 {
		t.Fatalf("the epoch must drop the other mirrors: %+v", ms)
	}
	h.request(r, func(q *Request) { q.Sync = []string{"pi"} })
	waitFor(t, "new pi mirror", slow, func() bool { return h.mirrors(r)["pi"].Native != "" })
	if got := h.sessionTexts("pi", h.mirrors(r)["pi"].Native); !slices.Equal(got, []string{"NEW-Q", "NEW-EPOCH"}) {
		t.Fatalf("pi after the epoch must hold only the new session: %q", got)
	}

	// A session with no file yet is retried; one recorded for another cwd pauses sync.
	if err := Update(SessionPath(r.Dir), func(s *Session) { s.NativeID, s.Turns = "sid-404", s.Turns+1 }); err != nil {
		t.Fatal(err)
	}
	h.waitLog(r, "sync: claude session sid-404 not ready")
	h.touch(filepath.Join(dir, "sid-3.jsonl"), `{"type":"user","uuid":"o1","parentUuid":null,"cwd":"/elsewhere","timestamp":"`+now+`","message":{"role":"user","content":"x"}}`+"\n")
	if err := Update(SessionPath(r.Dir), func(s *Session) { s.NativeID, s.Turns = "sid-3", s.Turns+1 }); err != nil {
		t.Fatal(err)
	}
	h.waitLog(r, "sync paused: claude session sid-3 rejected")
	h.quitOK(out)
}

// agy has no mirror: it starts fresh with a prompt naming a private context file.
func TestRunnerContextFileForAgy(t *testing.T) {
	h := newRig(t, "")
	agy := fakeBin("agy", "/quit", "agy-1", "AGY-SAID", "", "  :")
	h.cfg.Harnesses = []Harness{{Name: "claude", Kind: "claude"}, {Name: "agy", Kind: "agy", Bin: h.write("agy", agy)}}
	h.hot(10 * time.Minute)
	r, out := h.start("", nil)
	l := h.waitLaunches(2)
	file := filepath.Join(r.Dir, "context-1.md")
	if !strings.HasPrefix(l[1], "agy --log-file ") || !strings.Contains(l[1], " -i agent-swap moved this conversation from claude to agy. Read "+file) {
		t.Fatalf("agy launch: %s", l[1])
	}
	if ctx := h.contextOf(l[1]); !strings.Contains(ctx, "CLAUDE-SAID") || !strings.Contains(ctx, "Q-CLAUDE-SAID") {
		t.Fatalf("context file:\n%s", ctx)
	}
	if mode(t, file) != 0o600 {
		t.Errorf("context file mode %o", mode(t, file))
	}
	h.waitLog(r, "sync: context file (no mirror support)")
	h.quitOK(out)
}

// A writer that fails (pi's store cannot be created) falls back to the context file.
func TestRunnerContextFileOnWriterError(t *testing.T) {
	h := newRig(t, "")
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", h.write("pi-store-is-a-file", ""))
	t.Setenv("FAKE_NO_STDERR", "1") // the fake pi cannot write its session either
	h.cfg.Harnesses = []Harness{{Name: "claude", Kind: "claude"}, h.pi("pi", "pi-1", "PI-SAID")}
	h.hot(10 * time.Minute)
	r, out := h.start("", nil)
	l := h.waitLaunches(2)
	if !strings.HasPrefix(l[1], "pi -e ") || strings.Contains(l[1], "--session") || !strings.Contains(l[1], "context-1.md") {
		t.Fatalf("pi must start fresh with the context file: %s", l[1])
	}
	if ctx := h.contextOf(l[1]); !strings.Contains(ctx, "CLAUDE-SAID") {
		t.Fatalf("context file:\n%s", ctx)
	}
	h.waitLog(r, "sync: context file (")
	if n := strings.Count(h.runLog(r), "sync: pi create:"); n != 2 {
		t.Errorf("want 2 create attempts, got %d", n)
	}
	h.quitOK(out)
}

// No [[harness]] tables: claude → api (target) relaunch keeps working. The
// positional prompt is used only on the first launch; the child never
// inherits ANTHROPIC_BASE_URL / ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN. The
// shared claude session needs no write.
func TestRunnerLegacyTargetRelaunch(t *testing.T) {
	h := newRig(t, "")
	t.Setenv("ANTHROPIC_BASE_URL", "https://leak.example")
	t.Setenv("ANTHROPIC_API_KEY", "sk-leak")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok-leak")
	t.Setenv("ANTHROPIC_MODEL", "keep-me")
	h.hot(time.Minute)
	r, out := h.start("gateway", []string{"--model", "haiku", "fix bug"})

	l := h.waitLaunches(2)
	if l[0] != "claude --model haiku fix bug --settings "+settings(r, "claude") {
		t.Fatalf("first launch must keep the prompt: %s", l[0])
	}
	if l[1] != "claude --resume sid-1 --model haiku --settings "+settings(r, "api") {
		t.Fatalf("relaunch must resume on api settings without the prompt: %s", l[1])
	}
	if strings.Contains(h.runLog(r), "sync: api") {
		t.Error("the same kind's session needs no write")
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
	h.started(r)
	h.waitLog(r, "switch blocked")
	time.Sleep(6 * h.cfg.Poll.Duration) // at least 5 more blocked ticks
	if n := strings.Count(h.runLog(r), "switch blocked"); n != 1 {
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
// a flip back while the quit is in flight relaunches the same harness, which
// keeps its own session (seeded into session.json) and gets no prompt.
func TestRunnerRelaunchFromFreshAccount(t *testing.T) {
	h := newRig(t, "")
	gate := filepath.Join(h.tmp, "exit-gate")
	t.Setenv("FAKE_EXIT_GATE", gate)
	h.hot(time.Minute)
	r, out := h.start("gateway", nil)
	waitFor(t, "/exit", slow, func() bool { return strings.Contains(h.read(h.tmuxLog), "-l /exit") })
	h.cool()
	h.touch(os.Getenv("FAKE_SILENT"), "") // the relaunched fake reports nothing
	h.touch(gate, "")
	l := h.waitLaunches(2)
	if l[1] != "claude --resume sid-1 --settings "+settings(r, "claude") {
		t.Fatalf("relaunch must resume the fresh desired harness (claude) with no prompt: %s", l[1])
	}
	time.Sleep(3 * h.cfg.Poll.Duration)
	if s, err := Load[Session](SessionPath(r.Dir)); err != nil || s.NativeID != "sid-1" || !s.Idle || s.Turns != 0 {
		t.Fatalf("want native_id sid-1, idle (no prompt), turns 0: %+v %v", s, err)
	}
	if strings.Contains(h.runLog(r), "sync:") {
		t.Error("a relaunch writes no session")
	}
	h.quitOK(out)
}

// Pass-through args go only to an order[0]-kind harness on the first launch;
// later only claude gets them, through ReplayArgs. claude, never run before,
// resumes a session created from pi's turns.
func TestRunnerPassThroughArgsOnlyForFirstKind(t *testing.T) {
	h := newRig(t, "")
	h.cfg.Harnesses = []Harness{{Name: "claude", Kind: "claude"}, h.pi("pi", "pi-1", "PI-SAID")}
	h.setUsage("claude", func(u *Usage) { u.BlockedUntil = time.Now().Unix() + 600 })
	r, out := h.start("", []string{"--model", "haiku", "fix bug"})
	l := h.waitLaunches(1)
	if !strings.HasPrefix(l[0], "pi ") || strings.Contains(l[0], "haiku") || strings.Contains(l[0], "fix bug") {
		t.Fatalf("pi must not get claude's args: %s", l[0])
	}
	waitFor(t, "pi turn", slow, func() bool { s, _ := Load[Session](SessionPath(r.Dir)); return s.Turns > 0 })
	h.setUsage("claude", func(u *Usage) { u.BlockedUntil = 0 })
	l = h.waitLaunches(2)
	native := h.mirrors(r)["claude"].Native
	if native == "" || l[1] != "claude --resume "+native+" --model haiku --settings "+settings(r, "claude") {
		t.Fatalf("claude gets ReplayArgs and its new session (%q): %s", native, l[1])
	}
	if got := h.sessionTexts("claude", native); !slices.Equal(got[:2], []string{"Q-PI-SAID", "PI-SAID"}) {
		t.Fatalf("claude's created session: %q", got)
	}
	h.quitOK(out)
}

// A pass-through resume of a pre-existing claude session is forked on the
// first launch, so the session agent-swap appends to is its own.
func TestRunnerForksPassThroughResume(t *testing.T) {
	h := newRig(t, "")
	r, out := h.start("gateway", []string{"--resume", "old-1", "--model", "haiku"})
	l := h.waitLaunches(1)
	if l[0] != "claude --fork-session --resume old-1 --model haiku --settings "+settings(r, "claude") {
		t.Fatalf("first launch: %s", l[0])
	}
	h.waitLog(r, "session claude: sid-1 owned=true")
	h.quitOK(out)
}

// A harness that ignores the quit command, SIGINT and SIGTERM is killed.
func TestRunnerKillsStubbornHarness(t *testing.T) {
	h := newRig(t, "")
	t.Setenv("FAKE_STUBBORN", "1")
	h.hot(time.Minute)
	h.tweak = func(r *Runner) { r.QuitRetry, r.QuitKill = 100*time.Millisecond, 300*time.Millisecond }
	r, out := h.start("gateway", nil)
	l := h.waitLaunches(2)
	for _, s := range []string{"claude ignored quit: SIGTERM", "claude ignored SIGTERM: SIGKILL"} {
		if !strings.Contains(h.runLog(r), s) {
			t.Errorf("run log lacks %q", s)
		}
	}
	// C-c or a signal may have cut a turn off: the next harness gets the continue prompt.
	if !strings.Contains(h.read(h.tmuxLog), "send-keys -t %1 C-c") || !strings.Contains(l[1], "Moved here from claude because a usage limit") {
		t.Errorf("after C-c the relaunch must carry the continue prompt: %s", l[1])
	}
	h.quitOK(out)
}

// F3a/NEW-2: when the harness goes busy (the user typed) before the
// escalation reaches C-c, the switch is held: no C-c, Quit is sent again at
// the next idle, and an exit while it is held is that switch (Run goes on).
func TestRunnerSwitchHeldWhenBusy(t *testing.T) {
	h := newRig(t, "")
	t.Setenv("FAKE_STUBBORN", "1") // ignores /exit; exits 7 on $FAKE_QUIT
	h.hot(time.Minute)
	h.tweak = func(r *Runner) { r.QuitRetry, r.QuitKill = 300*time.Millisecond, time.Minute }
	r, out := h.start("gateway", nil)
	waitFor(t, "/exit", slow, func() bool { return strings.Contains(h.read(h.tmuxLog), "-l /exit") })
	if err := Update(SessionPath(r.Dir), func(s *Session) { s.Idle = false }); err != nil {
		t.Fatal(err)
	}
	h.waitLog(r, "switch held: claude became busy")
	held := strings.Count(h.read(h.tmuxLog), "-l /exit")
	time.Sleep(10 * h.cfg.Poll.Duration)
	if strings.Contains(h.read(h.tmuxLog), "C-c") || strings.Count(h.read(h.tmuxLog), "-l /exit") != held || len(h.launches()) != 1 {
		t.Fatalf("a held switch must not escalate while busy: launches %d", len(h.launches()))
	}
	if err := Update(SessionPath(r.Dir), func(s *Session) { s.Idle = true }); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "Quit sent again at idle", slow, func() bool { return strings.Count(h.read(h.tmuxLog), "-l /exit") > held })

	h.touch(h.quit, "") // the child exits on its own while the switch is pending
	l := h.waitLaunches(2)
	if !strings.HasPrefix(l[1], "claude --resume sid-1 ") || !strings.HasSuffix(l[1], "--settings "+settings(r, "api")) {
		t.Fatalf("the exit must complete the switch to api: %s", l[1])
	}
	// The api child sees $FAKE_QUIT too and exits: that one ends the run.
	if got := <-out; got.err != nil || got.code != 7 {
		t.Fatalf("run returned %d %v", got.code, got.err)
	}
}

// A held switch is dropped when, at the next idle, the running harness is
// wanted again (here: pinned): no Quit, no relaunch.
func TestRunnerHeldSwitchDroppedOnPin(t *testing.T) {
	h := newRig(t, "")
	t.Setenv("FAKE_STUBBORN", "1")
	h.hot(time.Minute)
	h.tweak = func(r *Runner) { r.QuitRetry, r.QuitKill = 300*time.Millisecond, time.Minute }
	r, out := h.start("gateway", nil)
	waitFor(t, "/exit", slow, func() bool { return strings.Contains(h.read(h.tmuxLog), "-l /exit") })
	if err := Update(SessionPath(r.Dir), func(s *Session) { s.Idle = false }); err != nil {
		t.Fatal(err)
	}
	h.waitLog(r, "switch held: claude became busy")
	h.request(r, func(q *Request) { q.Pin = "claude" })
	held := strings.Count(h.read(h.tmuxLog), "-l /exit")
	if err := Update(SessionPath(r.Dir), func(s *Session) { s.Idle = true }); err != nil {
		t.Fatal(err)
	}
	h.waitLog(r, "switch dropped: claude is wanted again")
	time.Sleep(10 * h.cfg.Poll.Duration)
	if n := strings.Count(h.read(h.tmuxLog), "-l /exit"); n != held || strings.Contains(h.read(h.tmuxLog), "C-c") || len(h.launches()) != 1 {
		t.Fatalf("a dropped switch must not quit or relaunch: /exit %d -> %d, launches %d", held, n, len(h.launches()))
	}
	// The /exit typed before the drop may still run: that exit relaunches the
	// same harness on its own session, with no prompt, instead of ending the run.
	h.touch(h.quit, "")
	l := h.waitLaunches(2)
	if l[1] != "claude --resume sid-1 --settings "+settings(r, "claude") {
		t.Fatalf("relaunch after a dropped switch: %s", l[1])
	}
	h.waitLog(r, "relaunch after dropped switch: claude exited")
	// The relaunched child quits too (it sees $FAKE_QUIT); no Quit was typed in
	// this launch, so that exit is the user's and ends the run.
	if got := <-out; got.err != nil || got.code != 7 {
		t.Fatalf("run returned %d %v", got.code, got.err)
	}
}

// Once further turns have completed after a dropped switch, its queued Quit
// is stale: a child exit is the user's own and ends the run.
func TestRunnerDroppedSwitchExitAfterTurnsEndsRun(t *testing.T) {
	h := newRig(t, "")
	t.Setenv("FAKE_STUBBORN", "1")
	h.hot(time.Minute)
	h.tweak = func(r *Runner) { r.QuitRetry, r.QuitKill = 300*time.Millisecond, time.Minute }
	r, out := h.start("gateway", nil)
	waitFor(t, "/exit", slow, func() bool { return strings.Contains(h.read(h.tmuxLog), "-l /exit") })
	if err := Update(SessionPath(r.Dir), func(s *Session) { s.Idle = false }); err != nil {
		t.Fatal(err)
	}
	h.waitLog(r, "switch held: claude became busy")
	h.request(r, func(q *Request) { q.Pin = "claude" })
	if err := Update(SessionPath(r.Dir), func(s *Session) { s.Idle = true }); err != nil {
		t.Fatal(err)
	}
	h.waitLog(r, "switch dropped: claude is wanted again")
	for i := 0; i < 2; i++ { // two more completed turns, each seen on a tick
		if err := Update(SessionPath(r.Dir), func(s *Session) { s.Turns++ }); err != nil {
			t.Fatal(err)
		}
		time.Sleep(4 * h.cfg.Poll.Duration)
	}
	h.quitOK(out) // exits 7, and Run returns it
	if n := len(h.launches()); n != 1 || strings.Contains(h.runLog(r), "relaunch after dropped switch") {
		t.Fatalf("a stale Quit must not relaunch: launches %d\n%s", n, h.runLog(r))
	}
}

// A Poll-driven harness (agy: busy/idle from its sqlite summaries) that turns
// busy during the escalation holds the switch too.
func TestRunnerSwitchHeldWhenPollReportsBusy(t *testing.T) {
	h := newRig(t, "")
	t.Setenv("FAKE_STUBBORN", "1")
	db := filepath.Join(h.home, ".gemini", "antigravity-cli", "conversation_summaries.db")
	sql := func(q string) {
		t.Helper()
		if out, err := exec.Command("sqlite3", db, q).CombinedOutput(); err != nil {
			t.Fatalf("sqlite3: %v %s", err, out)
		}
	}
	if err := os.MkdirAll(filepath.Dir(db), 0o700); err != nil {
		t.Fatal(err)
	}
	sql("create table conversation_summaries(conversation_id text, status text, not_fully_idle int);" +
		"insert into conversation_summaries values('agy-1', 'CASCADE_RUN_STATUS_IDLE', 0);")
	agy := fakeBin("agy", "/quit", "agy-1", "AGY-SAID", "", "  :")
	h.cfg.Harnesses = []Harness{{Name: "agy", Kind: "agy", Bin: h.write("agy", agy)}, {Name: "claude", Kind: "claude"}}
	h.block("agy")
	h.tweak = func(r *Runner) { r.QuitRetry, r.QuitKill = 300*time.Millisecond, time.Minute }
	// Start on agy (pinned), then unpin: claude is wanted and agy is idle.
	r, out := h.startPinned("agy")
	waitFor(t, "agy launch", slow, func() bool { return len(h.launches()) >= 1 })
	h.started(r)
	h.request(r, func(q *Request) { q.Pin = "" })
	waitFor(t, "/quit", slow, func() bool { return strings.Contains(h.read(h.tmuxLog), "-l /quit") })
	sql("update conversation_summaries set status = 'CASCADE_RUN_STATUS_RUNNING';")
	h.waitLog(r, "switch held: agy became busy")
	if strings.Contains(h.read(h.tmuxLog), "C-c") || len(h.launches()) != 1 {
		t.Fatalf("a held switch must not escalate: launches %d", len(h.launches()))
	}
	h.quitOK(out)
}

// F12: a pin written while the old harness is quitting decides where it goes.
func TestRunnerPinDuringEscalation(t *testing.T) {
	h := newRig(t, "")
	gate := filepath.Join(h.tmp, "exit-gate")
	t.Setenv("FAKE_EXIT_GATE", gate)
	h.cfg.Harnesses = []Harness{{Name: "claude", Kind: "claude"}, h.pi("pi", "pi-1", "PI-SAID"), h.pi("pi2", "pi-2", "PI2-SAID")}
	h.hot(10 * time.Minute)
	r, out := h.start("", nil)
	waitFor(t, "/exit", slow, func() bool { return strings.Contains(h.read(h.tmuxLog), "-l /exit") })
	h.request(r, func(q *Request) { q.Pin = "pi2" })
	h.touch(gate, "")
	l := h.waitLaunches(2)
	if !strings.HasPrefix(l[1], "pi2 ") {
		t.Fatalf("the pin set during the quit must win: %s", l[1])
	}
	h.quitOK(out)
}

// --- request.json handling (unit) ---

// The pin is validated against the order, survives an old block, and is
// cleared once its harness gets a new one.
func TestRequestPin(t *testing.T) {
	u := newUnitRunner(t, Harness{Name: "claude", Kind: "claude"}, piH)
	now := time.Now()
	cur := Harness{Name: "claude", Kind: "claude"}
	set := func(pin string) {
		t.Helper()
		if err := Update(RequestPath(u.Dir), func(q *Request) { q.Pin = pin }); err != nil {
			t.Fatal(err)
		}
	}
	set("bogus")
	u.request(Account{}, now, cur, u.rt)
	u.request(Account{}, now, cur, u.rt)
	if u.pin != "" {
		t.Fatalf("unknown pin accepted: %q", u.pin)
	}
	if n := strings.Count(u.out.String(), `pin "bogus" ignored`); n != 1 {
		t.Errorf("an invalid pin must be logged once per change, got %d", n)
	}
	_ = os.WriteFile(RequestPath(u.Dir), []byte("{bad"), 0o600)
	for i := 0; i < 3; i++ {
		u.request(Account{}, now, cur, u.rt)
	}
	if n := strings.Count(u.out.String(), "request.json"); n != 1 {
		t.Errorf("a corrupt request.json must be logged once, got %d:\n%s", n, u.out.String())
	}
	_ = os.Remove(RequestPath(u.Dir))

	old := acct(map[string]*Usage{"pi": {BlockedUntil: now.Unix() + 60}})
	set("pi")
	u.request(old, now, cur, u.rt) // pi was already blocked when pinned: the pin holds
	if u.pin != "pi" || u.pinBlock != now.Unix()+60 {
		t.Fatalf("pin %q block %d", u.pin, u.pinBlock)
	}
	u.request(old, now, cur, u.rt)
	if u.pin != "pi" {
		t.Fatal("the same block must not clear the pin")
	}
	fresh := acct(map[string]*Usage{"pi": {BlockedUntil: now.Unix() + 600}})
	u.request(fresh, now, cur, u.rt)
	if u.pin != "" {
		t.Fatal("a new block must clear the pin")
	}
	if q, _ := Load[Request](RequestPath(u.Dir)); q.Pin != "" {
		t.Fatalf("request.json keeps the cleared pin: %+v", q)
	}
	u.logged(t, "agent-swap: pi hit its limit; pin cleared")

	set("claude")
	u.request(Account{}, now, cur, u.rt)
	set("")
	u.request(Account{}, now, cur, u.rt)
	if u.pin != "" {
		t.Fatal("use auto must clear the pin")
	}
}

// Sync requests materialize the named non-running harnesses, once per kind,
// and are cleared; before the first launch they wait.
func TestRequestSync(t *testing.T) {
	claude := Harness{Name: "claude", Kind: "claude"}
	u := newUnitRunner(t, claude, piH, Harness{Name: "pi2", Kind: "pi"}, Harness{Name: "agy", Kind: "agy"})
	u.c.add("claude", say("claude", "q1", "a1"))
	if err := Update(RequestPath(u.Dir), func(q *Request) { q.Sync = []string{"*"} }); err != nil {
		t.Fatal(err)
	}
	u.request(Account{}, time.Now(), Harness{}, u.rt)
	if q, _ := Load[Request](RequestPath(u.Dir)); len(q.Sync) != 1 || len(u.c.mirrors) != 0 {
		t.Fatalf("before the first launch: %+v %v", q, u.c.mirrors)
	}
	u.request(Account{}, time.Now(), claude, u.rt)
	if q, _ := Load[Request](RequestPath(u.Dir)); len(q.Sync) != 0 {
		t.Fatalf("sync request not cleared: %+v", q)
	}
	if m := u.c.mirrors["pi"]; m == nil || m.Native == "" {
		t.Fatalf("pi not synced: %v", u.c.mirrors)
	}
	if n := strings.Count(u.out.String(), "sync: pi"); n != 1 {
		t.Errorf("pi and pi2 share one mirror: %d writes\n%s", n, u.out.String())
	}
	if u.c.mirrors["claude"] != nil {
		t.Error("the running kind must not be synced")
	}
	u.logged(t, "sync agy: no mirror support")
}
