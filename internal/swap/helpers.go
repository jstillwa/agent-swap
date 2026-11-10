package swap

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Environment passed to every wrapped harness (and so to its hooks and plugins).
const (
	EnvSessionDir = "AGENT_SWAP_SESSION_DIR"
	EnvStateDir   = "AGENT_SWAP_STATE_DIR"
	EnvConfig     = "AGENT_SWAP_CONFIG"
	EnvHarness    = "AGENT_SWAP_HARNESS" // harness name
	EnvExe        = "AGENT_SWAP_EXE"     // absolute path of agent-swap
	EnvStatusline = "AGENT_SWAP_USER_STATUSLINE"
)

func AccountPath(stateDir string) string { return filepath.Join(stateDir, "account.json") }
func SessionPath(dir string) string      { return filepath.Join(dir, "session.json") }

// MergeWindows stores fresh window readings in u. Usage in a window never
// drops, so a live stored window yields only to a later window or a higher
// reading of the same one.
func MergeWindows(u *Usage, got map[string]*Window, now time.Time) {
	if len(got) == 0 {
		return
	}
	if u.Windows == nil {
		u.Windows = map[string]*Window{}
	}
	for n, g := range got {
		st := u.Windows[n]
		if g == nil || st != nil && st.ResetsAt > now.Unix() &&
			(g.ResetsAt < st.ResetsAt || g.ResetsAt == st.ResetsAt && g.UsedPct < st.UsedPct) {
			continue
		}
		u.Windows[n] = g
	}
	u.UpdatedAt = now.Unix()
}

// Statusline records claude's rate limits (account-wide, under the harness
// named by $AGENT_SWAP_HARNESS) and the session id, then renders the user's
// own statusline command so its output is unchanged.
func Statusline(in io.Reader, out io.Writer, now time.Time) error {
	raw, err := io.ReadAll(in)
	if err != nil {
		return err
	}
	var p struct {
		SessionID  string `json:"session_id"`
		RateLimits *struct {
			FiveHour *Window `json:"five_hour"`
			SevenDay *Window `json:"seven_day"`
		} `json:"rate_limits"`
	}
	if err := json.Unmarshal(raw, &p); err == nil {
		if dir := os.Getenv(EnvSessionDir); dir != "" && safeID(p.SessionID) {
			_ = Update(SessionPath(dir), func(s *Session) { s.NativeID, s.UpdatedAt = p.SessionID, now.Unix() })
		}
		sd, name := os.Getenv(EnvStateDir), os.Getenv(EnvHarness)
		if sd != "" && name != "" && p.RateLimits != nil { // null on API billing
			_ = Update(AccountPath(sd), func(a *Account) {
				MergeWindows(a.Get(name), map[string]*Window{"5h": p.RateLimits.FiveHour, "7d": p.RateLimits.SevenDay}, now)
			})
		}
	}
	user := os.Getenv(EnvStatusline)
	if user == "" {
		return nil
	}
	cmd := exec.Command("sh", "-c", user)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(string(raw)), out, io.Discard
	return cmd.Run()
}

var limitCode = regexp.MustCompile(`\b(402|429)\b`)

// IsLimitError reports whether an error text means a usage or rate limit.
func IsLimitError(s string) bool {
	s = strings.ToLower(s)
	for _, m := range []string{"rate_limit", "rate limit", "quota", "usage limit", "budget exceeded",
		"resource_exhausted", "insufficient_balance"} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return limitCode.MatchString(s)
}

// Hook records idle/busy/rate-limit state for the session. Every harness's
// hook or plugin calls it with optional JSON {"session_id", "error"}.
func Hook(event string, in io.Reader, now time.Time) error {
	dir := os.Getenv(EnvSessionDir)
	if dir == "" {
		return nil
	}
	var p struct {
		SessionID string `json:"session_id"`
		Error     string `json:"error"`
	}
	_ = json.NewDecoder(in).Decode(&p)
	return Update(SessionPath(dir), func(s *Session) {
		if safeID(p.SessionID) {
			s.NativeID = p.SessionID
		}
		s.UpdatedAt = now.Unix()
		switch event {
		case "SessionStart": // Idle stays as the runner seeded it (busy when launched with a prompt)
			s.RateLimited, s.StartedAt = false, now.Unix()
		case "UserPromptSubmit":
			s.Idle, s.RateLimited = false, false
		case "Stop":
			s.Idle, s.RateLimited = true, false
		case "StopFailure": // Stop does not fire on API errors
			s.Idle = true
			s.RateLimited = IsLimitError(p.Error)
		case "LimitHit": // a retrying limit error (opencode): the turn is still running
			s.RateLimited = true
		}
	})
}

// Key prints the API key for target by running its key_cmd (apiKeyHelper).
func Key(cfg Config, target string, out io.Writer) error {
	t, ok := cfg.Targets[target]
	if !ok || t.KeyCmd == "" {
		return fmt.Errorf("target %q has no key_cmd", target)
	}
	b, err := exec.Command("sh", "-c", t.KeyCmd).Output()
	if err != nil {
		return fmt.Errorf("key_cmd for %q: %w", target, err)
	}
	key := strings.TrimSpace(string(b))
	if key == "" || key == "null" {
		return fmt.Errorf("key_cmd for %q printed no key", target)
	}
	_, err = fmt.Fprintln(out, key)
	return err
}

// SettingsJSON is claude's --settings payload; target "" means subscription
// billing, otherwise API billing via that target. Hooks merge with the
// user's own (verified), so nothing of theirs is replaced except statusLine,
// which Statusline re-renders.
func SettingsJSON(exe, target string, t Target) ([]byte, error) {
	q := shellQuote(exe)
	hooks := map[string]any{}
	for _, ev := range []string{"SessionStart", "UserPromptSubmit", "Stop", "StopFailure"} {
		hooks[ev] = []any{map[string]any{"hooks": []any{map[string]any{
			"type": "command", "command": q + " hook " + ev,
		}}}}
	}
	s := map[string]any{
		"statusLine": map[string]any{"type": "command", "command": q + " statusline"},
		"hooks":      hooks,
	}
	if target != "" {
		s["apiKeyHelper"] = q + " key " + shellQuote(target)
		if t.BaseURL != "" {
			s["env"] = map[string]string{"ANTHROPIC_BASE_URL": t.BaseURL}
		}
	}
	return json.MarshalIndent(s, "", "  ")
}

// UserStatusline returns statusLine.command from ~/.claude/settings.json.
func UserStatusline() string {
	home, _ := os.UserHomeDir()
	b, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		return ""
	}
	var s struct {
		StatusLine struct {
			Command string `json:"command"`
		} `json:"statusLine"`
	}
	_ = json.Unmarshal(b, &s)
	return s.StatusLine.Command
}

func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r == '/' || r == '-' || r == '_' || r == '.' || r == ':' || r == '=' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ShellJoin quotes argv for a tmux shell-command string.
func ShellJoin(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = shellQuote(a)
	}
	return strings.Join(q, " ")
}
