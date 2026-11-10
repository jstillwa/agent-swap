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

// Runner is the tmux pane process: it runs the most-preferred available
// harness as a child, watches its usage, drives the handoff through tmux
// send-keys, and relaunches the next harness in place with the conversation.
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
	TmpDir    string        // ready and notes files, writable from sandboxed harnesses

	session string // tmux session id for status-right
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
	dir := filepath.Join(cfg.StateDir, "sessions", time.Now().Format("20060102-150405")+"-"+nonce()[:6])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	tmp := filepath.Join(os.TempDir(), "agent-swap-"+filepath.Base(dir))
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Runner{
		Cfg: cfg, CfgPath: cfgPath, Order: cfg.Order(target), Exe: exe, Pane: pane, Args: args, Dir: dir,
		Cwd: cwd, Home: home, Log: log.New(f, "", log.LstdFlags), QuitRetry: 30 * time.Second, QuitKill: 90 * time.Second, TmpDir: tmp,
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

// switched is a decided switch: the harness to move to, the native session
// left behind, and the handoff notes file (may not exist).
type switched struct{ to, native, notes string }

// Run supervises harnesses until one exits without a pending switch, and
// returns that harness's exit code.
func (r *Runner) Run(ctx context.Context) (int, error) {
	acct, err := Load[Account](AccountPath(r.Cfg.StateDir))
	if err != nil {
		return 1, fmt.Errorf("%s: %w; fix or delete it to reset the shared state", AccountPath(r.Cfg.StateDir), err)
	}
	r.session = strings.TrimSpace(r.tmuxOut("display-message", "-p", "-t", r.Pane, "#{session_id}"))
	r.tmux("set-option", "-t", r.session, "status-right-length", "160")
	cur, prompt := Desired(acct, r.Order, time.Now()), ""
	natives := map[string]string{} // harness name → its own session id
	seen := map[string]int{}       // harness name → segments it already has
	var segs []segment             // conversation segments, oldest first
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
		rt := Runtime{Exe: r.Exe, Dir: r.Dir, Cwd: r.Cwd, StateDir: r.Cfg.StateDir, CfgPath: r.CfgPath, Home: r.Home, TmpDir: r.TmpDir,
			Cfg: r.Cfg, Launched: time.Now()}
		// A launch with a prompt starts busy; a hook or Poll reports the turn's end.
		_ = Update(SessionPath(r.Dir), func(s *Session) { *s = Session{NativeID: natives[cur], Idle: prompt == ""} })
		l, err := ad.Command(h, natives[cur], prompt, extra, rt)
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
		r.Log.Printf("launch %s (%s) resume=%q", h.Name, h.Kind, natives[cur])
		if err := cmd.Start(); err != nil {
			return 1, err
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		sw, err := r.supervise(ctx, h, ad, rt, cmd, done, n)
		if sw == nil {
			r.Log.Printf("%s exited: %v", h.Name, err)
			_ = os.RemoveAll(r.TmpDir)
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return ee.ExitCode(), nil
			}
			return 0, err
		}
		natives[cur] = sw.native
		next := sw.to
		if a, err := Load[Account](AccountPath(r.Cfg.StateDir)); err == nil {
			next = Desired(a, r.Order, time.Now()) // the desired harness may have moved during the exit
		}
		if next == cur { // no-op relaunch: the harness keeps its own context
			prompt = ""
			continue
		}
		text, err := ad.Transcript(sw.native, rt)
		if err != nil {
			r.Log.Printf("transcript %s: %v", h.Name, err)
			text += fmt.Sprintf("\n\n[agent-swap: transcript export failed: %v]", err)
		}
		seg := filepath.Join(r.Dir, fmt.Sprintf("segment-%d-%s.md", n, cur))
		_ = os.WriteFile(seg, []byte(fmt.Sprintf("## Conversation on %s\n\n%s\n", cur, text)), 0o600)
		segs = append(segs, segment{seg, h.Kind + "/" + sw.native})
		seen[cur] = len(segs) // cur resumes its own session natively
		if h.Kind == "claude" && r.Order[rank(r.Order, next)].Kind == "claude" {
			natives[next], seen[next] = sw.native, len(segs) // billing swap: resume the same conversation
		}
		ctxFile := filepath.Join(r.Dir, fmt.Sprintf("context-%d.md", n))
		if err := os.WriteFile(ctxFile, []byte(r.context(cur, next, sw.notes, segs[seen[next]:])), 0o600); err != nil {
			r.Log.Printf("context: %v", err)
		}
		prompt = Render(r.Cfg.ResumeMessage, map[string]string{"from": cur, "to": next, "context": ctxFile})
		cur = next
		r.notify("Switched to " + cur)
	}
}

// segment is an exported transcript; key = kind/native id, so a later export
// of a resumed session supersedes the earlier one.
type segment struct{ path, key string }

// context is the handoff file for the next harness: the notes, then the
// conversation segments it has not seen, newest first, within
// context_max_bytes: the segment that overflows keeps its latest turns and
// older ones are dropped.
func (r *Runner) context(from, to, notes string, segs []segment) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# agent-swap handoff: %s → %s\n\n", from, to)
	if n, err := os.ReadFile(notes); err == nil {
		fmt.Fprintf(&b, "## Handoff notes from %s\n\n%s\n\n", from, n)
	}
	budget, done := r.Cfg.ContextMaxBytes-b.Len(), map[string]bool{}
	for i := len(segs) - 1; i >= 0 && budget > 0; i-- {
		if done[segs[i].key] {
			continue
		}
		done[segs[i].key] = true
		c, _ := os.ReadFile(segs[i].path)
		seg := string(c) + "\n"
		if len(seg) > budget { // keep the heading and the most recent turns
			head, body, _ := strings.Cut(seg, "\n\n")
			seg = head + "\n\n" + TailBytes(body, max(budget-len(head)-2, 0))
			budget = 0 // older segments are dropped
		}
		budget -= len(seg)
		b.WriteString(seg)
	}
	return b.String()
}

// supervise polls until the child exits. It returns the decided switch, or
// nil when the child exited on its own.
func (r *Runner) supervise(ctx context.Context, h Harness, ad Adapter, rt Runtime, cmd *exec.Cmd, done <-chan error, n int) (*switched, error) {
	tick := time.NewTicker(r.Cfg.Poll.Duration)
	defer tick.Stop()
	var ph *Phase
	var sw *switched
	var quiet string
	logOnce := func(msg string) { // repeat only when the message changes
		if msg != quiet {
			r.Log.Print(msg)
			quiet = msg
		}
	}
	var quitSent time.Time
	step := 0 // quit escalation: 1 resent, 2 C-c, 3 SIGTERM, 4 SIGKILL
	for {
		select {
		case err := <-done:
			return sw, err
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tick.C:
		}
		now := time.Now()
		if sw != nil {
			switch el := now.Sub(quitSent); {
			case step == 0 && el >= r.QuitRetry:
				r.send(ad.Quit())
				step++
			case step == 1 && el >= 2*r.QuitRetry:
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
		touched := false
		if ph != nil {
			_, err := os.Stat(ph.File)
			touched = err == nil
		}
		d := Decide(now, r.Cfg, r.Order, acct, s, h.Name, ph, touched)
		if d != Switch || s.NativeID != "" {
			quiet = "" // a successful tick re-arms the once-only messages
		}
		switch d {
		case StartHandoff:
			ph = r.startHandoff(now, acct, s, h, Desired(acct, r.Order, now), n)
		case Cancel:
			r.Log.Printf("cancel handoff to %s: desired harness changed", ph.To)
			_ = os.Remove(ph.File)
			ph = nil
		case Switch:
			if s.NativeID == "" {
				logOnce("switch blocked: no " + h.Name + " session id yet")
				break
			}
			sw = &switched{to: Desired(acct, r.Order, now), native: s.NativeID}
			r.Log.Printf("switch %s -> %s (touched=%v rate_limited=%v)", h.Name, sw.to, touched, s.RateLimited)
			if ph != nil {
				_ = os.Remove(ph.File)
				sw.notes = ph.Notes
			}
			ph, quitSent = nil, now
			r.send(ad.Quit())
		}
		r.tmux("set-option", "-t", r.session, "status-right", StatusText(now, r.Cfg, r.Order, acct, h.Name, ph))
	}
}

func (r *Runner) startHandoff(now time.Time, a Account, s Session, h Harness, to string, n int) *Phase {
	cur := h.Name
	ph := &Phase{To: to, File: filepath.Join(r.TmpDir, "ready-"+nonce()),
		Notes: filepath.Join(r.TmpDir, fmt.Sprintf("notes-%d.md", n)), Started: now.Unix()}
	u := a.Harnesses[cur]
	pct, window, _ := u.Peak(now)
	tmpl, reason := r.Cfg.HandoffMessage, "agent-swap state changed"
	switch {
	case window != "" && pct >= orDefault(h.Threshold, r.Cfg.Threshold):
		reason = fmt.Sprintf("usage at %.0f%% (%s)", pct, window)
	case s.RateLimited:
		reason = "a rate-limit error"
	case rank(r.Order, to) < rank(r.Order, cur):
		reason = "a higher-priority harness is available"
	}
	if rank(r.Order, to) < rank(r.Order, cur) {
		tmpl = r.Cfg.ReturnMessage
	}
	var resets int64
	if u != nil {
		resets = u.ResetsAt
	}
	r.send(Render(tmpl, map[string]string{
		"from": cur, "to": to, "file": shellQuote(ph.File), "notes": shellQuote(ph.Notes),
		"pct": fmt.Sprintf("%.0f", pct), "reason": reason, "resets_at": time.Unix(resets, 0).Format("Mon 15:04"),
	}))
	r.Log.Printf("handoff %s -> %s, waiting for %s", cur, to, ph.File)
	r.notify(fmt.Sprintf("agent-swap: preparing to switch from %s to %s", cur, to))
	return ph
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
