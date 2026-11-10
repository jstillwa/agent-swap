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
	"time"
)

// Runner is the tmux pane process: it runs claude as a child, watches usage,
// drives the handoff through tmux send-keys, and relaunches claude in place.
type Runner struct {
	Cfg        Config
	CfgPath    string
	Target     string
	Exe        string   // absolute path of this binary (hooks, statusline, apiKeyHelper)
	Pane       string   // $TMUX_PANE
	ClaudeArgs []string // extra args for claude
	Dir        string   // per-session state dir
	Log        *log.Logger
	Settle     time.Duration // wait after SessionStart before typing the resume message

	session string // tmux session id for status-right
}

func NewRunner(cfg Config, cfgPath, target, exe, pane string, args []string) (*Runner, error) {
	if _, ok := cfg.Targets[target]; !ok {
		return nil, fmt.Errorf("unknown target %q", target)
	}
	if pane == "" {
		return nil, errors.New("agent-swap run must be started inside tmux (TMUX_PANE unset); use `agent-swap` to launch")
	}
	dir := filepath.Join(cfg.StateDir, "sessions", time.Now().Format("20060102-150405")+"-"+nonce()[:6])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Runner{
		Cfg: cfg, CfgPath: cfgPath, Target: target, Exe: exe, Pane: pane, ClaudeArgs: args, Dir: dir,
		Log: log.New(f, "", log.LstdFlags), Settle: 2 * time.Second,
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

// Run supervises claude until it exits without a pending switch, and returns
// claude's exit code.
func (r *Runner) Run(ctx context.Context) (int, error) {
	acct, err := Load[Account](AccountPath(r.Cfg.StateDir))
	if err != nil {
		return 1, fmt.Errorf("%s: %w; fix or delete it to reset the shared state", AccountPath(r.Cfg.StateDir), err)
	}
	for _, m := range []Mode{Sub, API} {
		b, err := SettingsJSON(r.Exe, m, r.Target, r.Cfg.Targets[r.Target])
		if err != nil {
			return 1, err
		}
		if err := os.WriteFile(r.settingsPath(m), b, 0o600); err != nil {
			return 1, err
		}
	}
	r.session = strings.TrimSpace(r.tmuxOut("display-message", "-p", "-t", r.Pane, "#{session_id}"))
	r.tmux("set-option", "-t", r.session, "status-right-length", "160")
	mode, resume, welcome := acct.Desired(time.Now()), "", ""
	for {
		_ = Update(SessionPath(r.Dir), func(s *Session) { s.Idle, s.RateLimited = true, false })
		args := r.ClaudeArgs
		if resume != "" {
			args = append([]string{"--resume", resume}, ReplayArgs(args)...)
		}
		// Our --settings goes last so a user --settings cannot override the hooks or apiKeyHelper.
		args = append(args[:len(args):len(args)], "--settings", r.settingsPath(mode))
		cmd := exec.CommandContext(ctx, r.Cfg.ClaudeBin, args...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		cmd.Env = append(childEnv(os.Environ()),
			EnvSessionDir+"="+r.Dir, EnvStateDir+"="+r.Cfg.StateDir, EnvConfig+"="+r.CfgPath,
			EnvStatusline+"="+r.userStatusline())
		launched := time.Now()
		r.Log.Printf("launch mode=%s resume=%q", mode, resume)
		if err := cmd.Start(); err != nil {
			return 1, err
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		next, sid, err := r.supervise(ctx, mode, done, launched, welcome)
		if next == "" {
			r.Log.Printf("claude exited: %v", err)
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return ee.ExitCode(), nil
			}
			return 0, err
		}
		if a, err := Load[Account](AccountPath(r.Cfg.StateDir)); err == nil {
			next = a.Desired(time.Now()) // the desired mode may have moved during the exit
		}
		resume, mode, welcome = sid, next, r.Cfg.ReturnResumeMessage
		if mode == API {
			welcome = r.Cfg.ResumeMessage
		}
		welcome = Render(welcome, map[string]string{"target": r.Target})
		r.notify(fmt.Sprintf("Switched to %s", r.modeLabel(mode)))
	}
}

// supervise polls until claude exits. It returns the mode to relaunch in and
// the session id to resume, or "" when claude exited on its own.
func (r *Runner) supervise(ctx context.Context, mode Mode, done <-chan error, launched time.Time, welcome string) (Mode, string, error) {
	tick := time.NewTicker(r.Cfg.Poll.Duration)
	defer tick.Stop()
	var ph *Phase
	var pending Mode
	var sid, quiet string
	logOnce := func(msg string) { // repeat only when the message changes
		if msg != quiet {
			r.Log.Print(msg)
			quiet = msg
		}
	}
	var exitSent time.Time
	for {
		select {
		case err := <-done:
			return pending, sid, err
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-tick.C:
		}
		now := time.Now()
		s, err := Load[Session](SessionPath(r.Dir))
		if err != nil {
			logOnce("skip tick: " + err.Error())
			continue
		}
		if welcome != "" && (s.StartedAt >= launched.Unix() || now.Sub(launched) > 30*time.Second) {
			time.Sleep(r.Settle)
			r.send(welcome)
			welcome = ""
		}
		if pending != "" {
			if now.Sub(exitSent) > 30*time.Second { // /exit got lost: retry
				r.send("/exit")
				exitSent = now
			}
			continue
		}
		var acct Account
		if err := Update(AccountPath(r.Cfg.StateDir), func(a *Account) {
			if Trigger(a, now, r.Cfg, mode == Sub && s.RateLimited) {
				r.Log.Printf("trigger: api until %s", time.Unix(a.APIUntil, 0))
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
		d := Decide(now, r.Cfg, acct, s, mode, ph, touched)
		if d != Switch || s.ClaudeSessionID != "" {
			quiet = "" // a successful tick re-arms the once-only messages
		}
		switch d {
		case StartHandoff:
			ph = r.startHandoff(now, acct)
		case Cancel:
			r.Log.Printf("cancel handoff to %s: desired mode changed", ph.To)
			_ = os.Remove(ph.File)
			ph = nil
		case Switch:
			if s.ClaudeSessionID == "" {
				logOnce("switch blocked: no claude session id yet")
				break
			}
			to := acct.Desired(now)
			r.Log.Printf("switch %s -> %s (touched=%v rate_limited=%v)", mode, to, touched, s.RateLimited)
			if ph != nil {
				_ = os.Remove(ph.File)
			}
			ph, pending, sid, exitSent = nil, to, s.ClaudeSessionID, now
			r.send("/exit")
		}
		r.tmux("set-option", "-t", r.session, "status-right", StatusText(now, r.Cfg, r.Target, acct, mode, ph))
	}
}

func (r *Runner) startHandoff(now time.Time, a Account) *Phase {
	ph := &Phase{To: a.Desired(now), File: filepath.Join(r.Dir, "ready-"+nonce()), Started: now.Unix()}
	pct, _, _ := a.Peak(now)
	tmpl := r.Cfg.HandoffMessage
	if ph.To == Sub {
		tmpl = r.Cfg.ReturnMessage
	}
	r.send(Render(tmpl, map[string]string{
		"file": shellQuote(ph.File), "pct": fmt.Sprintf("%.0f", pct), "target": r.Target,
		"resets_at": time.Unix(a.ResetsAt, 0).Format("Mon 15:04"),
	}))
	r.Log.Printf("handoff to %s, waiting for %s", ph.To, ph.File)
	r.notify(fmt.Sprintf("Account cycle: preparing to switch to %s", r.modeLabel(ph.To)))
	return ph
}

func (r *Runner) modeLabel(m Mode) string {
	if m == API {
		return "API billing (" + r.Target + ")"
	}
	return "subscription"
}

func (r *Runner) settingsPath(m Mode) string {
	return filepath.Join(r.Dir, strings.ToLower(string(m))+".settings.json")
}

func (r *Runner) userStatusline() string {
	if r.Cfg.StatuslineCmd != "" {
		return r.Cfg.StatuslineCmd
	}
	return UserStatusline()
}

// send types text into the claude pane and submits it.
func (r *Runner) send(text string) {
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
