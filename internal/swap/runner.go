package swap

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Runner is the tmux pane process: it runs the wanted harness as a child,
// reads each completed turn into the conversation log, and on a switch quits
// the child, writes the log into the next harness's session and resumes it.
type Runner struct {
	Cfg       Config
	CfgPath   string
	Order     []Harness
	Exe       string   // absolute path of this binary (hooks, statusline, apiKeyHelper)
	Pane      string   // $TMUX_PANE
	Args      []string // user pass-through args
	Dir       string   // per-session state dir
	Cwd, Home string
	Log       *log.Logger
	QuitRetry time.Duration // resend Quit after this, C-c after twice this
	QuitKill  time.Duration // SIGTERM the child after this, SIGKILL after QuitRetry/2 more

	session  string    // tmux session id for status-right
	started  time.Time // run start: sessions created after it are owned
	c        conv
	pin      string // validated pin from request.json ("" = auto)
	pinRaw   string // pin as last read, valid or not (log once per change)
	reqErr   string // last request.json read error (log once)
	pinBlock int64  // the pinned harness's BlockedUntil when the pin was seen
}

func NewRunner(cfg Config, cfgPath, target, exe, pane string, args []string) (*Runner, error) {
	if pane == "" {
		return nil, errors.New("agent-swap run must be started inside tmux (TMUX_PANE unset); use `agent-swap` to launch")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	home, _ := os.UserHomeDir()
	dir, err := filepath.Abs(filepath.Join(cfg.StateDir, "sessions", time.Now().Format("20060102-150405")+"-"+nonce()[:6]))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Runner{
		Cfg: cfg, CfgPath: cfgPath, Order: cfg.Order(target), Exe: exe, Pane: pane, Args: args, Dir: dir,
		Cwd: cwd, Home: home, Log: log.New(f, "", log.LstdFlags), QuitRetry: 30 * time.Second, QuitKill: 90 * time.Second,
	}, nil
}

func nonce() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// childEnv drops inherited Anthropic credential, base-URL and cloud-provider
// variables so only the settings agent-swap passes decide billing.
// CLAUDE_CODE_OAUTH_TOKEN stays: it can be the subscription login.
func childEnv(environ []string) []string {
	var out []string
	for _, kv := range environ {
		switch k, _, _ := strings.Cut(kv, "="); k {
		case "ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_CUSTOM_HEADERS",
			"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
			"ANTHROPIC_BEDROCK_BASE_URL", "ANTHROPIC_VERTEX_BASE_URL", "ANTHROPIC_FOUNDRY_BASE_URL",
			"ANTHROPIC_FOUNDRY_API_KEY", "ANTHROPIC_FOUNDRY_RESOURCE":
		default:
			out = append(out, kv)
		}
	}
	return out
}

// switched is a decided switch: the harness to move to, and whether a limit
// or error cut the last turn off.
type switched struct {
	to     string
	cutoff bool
}

const continueMessage = "[agent-swap] Moved here from {from} because a usage limit interrupted the last turn. " +
	"No new user input. Resume the interrupted work; if you were waiting for the user, ask again."

func (r *Runner) runtime() Runtime {
	return Runtime{Exe: r.Exe, Dir: r.Dir, Cwd: r.Cwd, StateDir: r.Cfg.StateDir, CfgPath: r.CfgPath, Home: r.Home,
		Env: os.Environ(), Cfg: r.Cfg, Launched: time.Now(), Started: r.started}
}

// Run supervises harnesses until one exits without a pending switch, and
// returns that harness's exit code.
func (r *Runner) Run(ctx context.Context) (int, error) {
	acct, err := Load[Account](AccountPath(r.Cfg.StateDir))
	if err != nil {
		return 1, fmt.Errorf("%s: %w; fix or delete it to reset the shared state", AccountPath(r.Cfg.StateDir), err)
	}
	if r.started.IsZero() {
		r.started = time.Now()
	}
	r.c = conv{mirrors: map[string]*Mirror{}, sent: map[string]bool{}, paused: map[string]bool{}}
	r.session = strings.TrimSpace(r.tmuxOut("display-message", "-p", "-t", r.Pane, "#{session_id}"))
	r.tmux("set-option", "-t", r.session, "status-right-length", "160")
	r.tmux("set-option", "-p", "-t", r.Pane, "@agent-swap-dir", r.Dir) // `agent-swap use` finds the run here
	r.request(acct, time.Now(), Harness{}, Runtime{})
	cur, prompt := Want(acct, r.Order, time.Now(), r.pin), ""
	for n := 1; ; n++ {
		h := r.Order[rank(r.Order, cur)]
		ad, err := AdapterFor(h.Kind)
		if err != nil {
			return 1, err
		}
		var extra []string // user args belong to the first harness's kind
		switch {
		case h.Kind != r.Order[0].Kind:
		case n == 1:
			extra = r.Args
		case h.Kind == "claude":
			extra = ReplayArgs(r.Args)
		}
		r.c.forked, r.c.foreign, r.c.resumed = false, n == 1 && resumes(extra), ""
		if r.c.foreign && h.Kind == "claude" { // fork, so the session is ours to append to
			extra, r.c.forked, r.c.foreign, r.c.resumed = append([]string{"--fork-session"}, extra...), true, false, resumeID(extra)
		}
		native := ""
		if m := r.c.mirrors[h.Kind]; m != nil {
			native = m.Native
		}
		r.c.paused[h.Kind] = false
		rt := r.runtime()
		// A launch with a prompt starts busy; a hook or Poll reports the turn's end.
		_ = Update(SessionPath(r.Dir), func(s *Session) { *s = Session{NativeID: native, Idle: prompt == ""} })
		l, err := ad.Command(h, native, prompt, extra, rt)
		if err != nil {
			return 1, err
		}
		cmd := exec.CommandContext(ctx, l.Bin, l.Args...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		env := os.Environ()
		if h.Kind == "claude" {
			env = append(childEnv(env), EnvStatusline+"="+r.userStatusline())
		}
		cmd.Env = append(append(env, EnvSessionDir+"="+r.Dir, EnvStateDir+"="+r.Cfg.StateDir, EnvConfig+"="+r.CfgPath,
			EnvHarness+"="+h.Name, EnvExe+"="+r.Exe), l.Env...)
		r.Log.Printf("launch %s (%s) resume=%q", h.Name, h.Kind, native)
		if prompt != "" {
			r.c.sent[clean(strings.TrimSpace(prompt))] = true
		}
		if err := cmd.Start(); err != nil {
			return 1, err
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		sw, err := r.supervise(ctx, h, ad, rt, cmd, done)
		if s, lerr := Load[Session](SessionPath(r.Dir)); lerr == nil {
			final := rt
			final.Final = true // the child is gone: its trailing turn is complete as it stands
			r.ingest(h, ad, final, s, true)
		}
		if sw == nil {
			r.Log.Printf("%s exited: %v", h.Name, err)
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return ee.ExitCode(), nil
			}
			return 0, err
		}
		next := sw.to
		if a, err := Load[Account](AccountPath(r.Cfg.StateDir)); err == nil {
			r.request(a, time.Now(), h, rt)
			next = Want(a, r.Order, time.Now(), r.pin) // the wanted harness may have moved during the exit
		}
		prompt = ""
		if next == cur { // relaunch: the harness keeps its own session
			continue
		}
		if sw.cutoff {
			prompt = Render(continueMessage, map[string]string{"from": cur})
		}
		if p := r.takeover(r.Order[rank(r.Order, next)], cur, rt, n); p != "" {
			prompt = strings.TrimSpace(p + "\n\n" + prompt)
		}
		cur = next
		r.notify("Switched to " + cur)
	}
}

// supervise polls until the child exits. It returns the decided switch, or
// nil when the child exited on its own.
func (r *Runner) supervise(ctx context.Context, h Harness, ad Adapter, rt Runtime, cmd *exec.Cmd, done <-chan error) (*switched, error) {
	tick := time.NewTicker(r.Cfg.Poll.Duration)
	defer tick.Stop()
	var sw *switched
	var quiet string
	logOnce := func(msg string) { // repeat only when the message changes
		if msg != quiet {
			r.Log.Print(msg)
			quiet = msg
		}
	}
	var quitSent time.Time
	held, quitTyped := false, false      // a pending switch waits for the busy harness to go idle; a Quit was typed
	turnsAtDrop := 0                     // s.Turns when a held switch was dropped
	current := func() (Session, error) { // the session as hooks and (codex, agy) Poll report it
		s, err := Load[Session](SessionPath(r.Dir))
		if err != nil {
			return s, err
		}
		before := s
		if err := ad.Poll(h, rt, &s, &Usage{}); err != nil {
			logOnce("poll: " + err.Error())
		}
		if s != before {
			_ = Update(SessionPath(r.Dir), func(p *Session) { *p = s })
		}
		return s, nil
	}
	step, turns, again := 0, 0, false // quit escalation: 1 resent, 2 C-c, 3 SIGTERM, 4 SIGKILL
	for {
		select {
		case err := <-done:
			if sw == nil && quitTyped { // a dropped switch's queued /exit ran: keep the run going
				r.Log.Printf("relaunch after dropped switch: %s exited", h.Name)
				sw = &switched{to: h.Name}
			}
			return sw, err
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tick.C:
		}
		now := time.Now()
		if sw != nil {
			switch el := now.Sub(quitSent); {
			case held: // the switch stays pending (typed /exit may still run); quit again at the next idle
				if s, err := current(); err == nil && s.Idle {
					a, _ := Load[Account](AccountPath(r.Cfg.StateDir))
					r.request(a, now, h, rt) // the pin may have changed meanwhile
					held, quitSent = false, now
					if sw.to = Want(a, r.Order, now, r.pin); sw.to == h.Name {
						r.Log.Printf("switch dropped: %s is wanted again", h.Name)
						sw, turnsAtDrop = nil, s.Turns
						continue
					}
					r.send(ad.Quit())
				}
			case step == 0 && el >= r.QuitRetry:
				r.send(ad.Quit())
				step++
			case step == 1 && el >= 2*r.QuitRetry:
				if s, err := current(); err == nil && !s.Idle {
					r.Log.Printf("switch held: %s became busy", h.Name)
					held, step = true, 0
					continue
				}
				sw.cutoff = true // C-c or a signal may cut a turn off
				r.tmux("send-keys", "-t", r.Pane, "C-c")
				r.tmux("send-keys", "-t", r.Pane, "C-c")
				step++
			case step == 2 && el >= r.QuitKill:
				r.Log.Printf("%s ignored quit: SIGTERM", h.Name)
				_ = cmd.Process.Signal(syscall.SIGTERM)
				step++
			case step == 3 && el >= r.QuitKill+r.QuitRetry/2:
				r.Log.Printf("%s ignored SIGTERM: SIGKILL", h.Name)
				_ = cmd.Process.Kill()
				step++
			}
			continue
		}
		s, err := Load[Session](SessionPath(r.Dir))
		if err != nil {
			logOnce("skip tick: " + err.Error())
			continue
		}
		polled, before := &Usage{}, s
		if err := ad.Poll(h, rt, &s, polled); err != nil {
			logOnce("poll: " + err.Error())
		}
		if s != before {
			_ = Update(SessionPath(r.Dir), func(p *Session) { *p = s })
		}
		// A turn completed (counters may jump by more than one): read it into the
		// log, and again on the next tick and while the session file grew past
		// the cursor, since claude flushes the last line after its Stop hook.
		if quitTyped && s.Turns > turnsAtDrop+1 { // a later user turn: the dropped switch's /exit is spent
			quitTyped = false
		}
		if s.Turns != turns || again || s.Idle && r.unread(h.Kind) {
			again = s.Turns != turns
			turns = s.Turns
			r.ingest(h, ad, rt, s, false)
		}
		var acct Account
		if err := Update(AccountPath(r.Cfg.StateDir), func(a *Account) {
			MergeWindows(a.Get(h.Name), polled.Windows, now)
			if Trigger(a, h, r.Cfg, now, s.RateLimited) {
				r.Log.Printf("trigger: %s blocked until %s", h.Name, time.Unix(a.Harnesses[h.Name].BlockedUntil, 0))
			}
			acct = *a
		}); err != nil {
			logOnce("skip tick: " + err.Error())
			continue
		}
		r.request(acct, now, h, rt)
		d := Decide(now, r.Order, acct, s, h.Name, r.pin)
		if d != Switch || s.NativeID != "" {
			quiet = "" // a successful tick re-arms the once-only messages
		}
		if d == Switch {
			if s.NativeID == "" {
				logOnce("switch blocked: no " + h.Name + " session id yet")
			} else {
				sw = &switched{to: Want(acct, r.Order, now, r.pin), cutoff: s.RateLimited || s.Failed}
				r.Log.Printf("switch %s -> %s (rate_limited=%v failed=%v pin=%q)", h.Name, sw.to, s.RateLimited, s.Failed, r.pin)
				quitSent, quitTyped = now, true
				r.send(ad.Quit())
			}
		}
		r.tmux("set-option", "-t", r.session, "status-right", StatusText(now, r.Cfg, r.Order, acct, h.Name, r.pin))
	}
}

// request applies request.json: the pin (validated against the order, and
// cleared once the pinned harness gets a new block) and sync requests, which
// bring the named non-running harnesses' sessions up to date. cur is the
// running harness (zero before the first launch: no sync).
func (r *Runner) request(acct Account, now time.Time, cur Harness, rt Runtime) {
	q, err := Load[Request](RequestPath(r.Dir))
	if err != nil {
		if err.Error() != r.reqErr { // a corrupt file is logged once
			r.Log.Printf("request.json: %v", err)
		}
		r.reqErr = err.Error()
		return
	}
	r.reqErr = ""
	if q.Pin != r.pinRaw {
		r.pinRaw, r.pin, r.pinBlock = q.Pin, q.Pin, blockedUntil(acct, q.Pin)
		if q.Pin != "" && rank(r.Order, q.Pin) < 0 {
			r.Log.Printf("pin %q ignored: not a configured harness", q.Pin)
			r.pin = ""
		} else {
			r.Log.Printf("pin %q", q.Pin)
		}
	}
	clear := r.pin != "" && blockedUntil(acct, r.pin) > now.Unix() && blockedUntil(acct, r.pin) != r.pinBlock
	if cur.Name == "" {
		q.Sync = nil // before the first launch: leave sync requests for the first tick
	}
	if !clear && len(q.Sync) == 0 {
		return
	}
	var syncs []string
	if err := Update(RequestPath(r.Dir), func(q *Request) {
		if cur.Name != "" {
			syncs, q.Sync = q.Sync, nil
		}
		if clear && q.Pin == r.pin {
			q.Pin = ""
		}
	}); err != nil {
		r.Log.Printf("request.json: %v", err)
		return
	}
	if clear {
		r.notify(fmt.Sprintf("agent-swap: %s hit its limit; pin cleared", r.pin))
		r.pin = ""
	}
	done := map[string]bool{cur.Kind: true} // the running kind is current already
	for _, name := range syncs {
		for _, h := range r.Order {
			if (name == "*" || name == h.Name) && !done[h.Kind] {
				done[h.Kind] = true
				if err := r.materialize(h, rt); err != nil {
					r.Log.Printf("sync %s: %v", h.Name, err)
				}
			}
		}
	}
}

func (r *Runner) userStatusline() string {
	if r.Cfg.StatuslineCmd != "" {
		return r.Cfg.StatuslineCmd
	}
	return UserStatusline()
}

// send clears the composer line, types text into the harness pane and submits it.
func (r *Runner) send(text string) {
	r.tmux("send-keys", "-t", r.Pane, "C-u")
	r.tmux("send-keys", "-t", r.Pane, "-l", text)
	time.Sleep(300 * time.Millisecond) // let the TUI absorb the paste before Enter
	r.tmux("send-keys", "-t", r.Pane, "Enter")
}

func (r *Runner) tmux(args ...string) {
	if out, err := exec.Command(r.Cfg.TmuxBin, args...).CombinedOutput(); err != nil {
		r.Log.Printf("tmux %s: %v %s", args[0], err, out)
	}
}

func (r *Runner) tmuxOut(args ...string) string {
	out, _ := exec.Command(r.Cfg.TmuxBin, args...).Output()
	return string(out)
}

func (r *Runner) notify(msg string) {
	r.Log.Print(msg)
	if !r.Cfg.Notify {
		return
	}
	esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(msg)
	_ = exec.Command("osascript", "-e", `display notification "`+esc+`" with title "agent-swap"`).Run()
}
