// Package swap runs the most-preferred available coding-agent harness and
// moves the conversation to the next one when it nears its usage limit, and
// back once the preferred harness frees up.
package swap

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Duration accepts TOML strings like "10m".
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	d.Duration = v
	return err
}

// Target is an API billing destination. The key is never stored: KeyCmd runs
// on demand (via Claude Code's apiKeyHelper) and prints it.
type Target struct {
	BaseURL string `toml:"base_url"`
	KeyCmd  string `toml:"key_cmd"`
}

// Kinds are the supported harness CLIs.
var Kinds = []string{"claude", "codex", "pi", "opencode", "agy"}

// Harness is one agent CLI in the preference order.
type Harness struct {
	Name      string   `toml:"name"`
	Kind      string   `toml:"kind"`      // claude|codex|pi|opencode|agy
	Bin       string   `toml:"bin"`       // default: claude → Config.ClaudeBin, else Kind
	Args      []string `toml:"args"`      // always passed
	Target    string   `toml:"target"`    // claude only: API billing via Targets
	Threshold float64  `toml:"threshold"` // 0 → Config.Threshold
	Cooldown  Duration `toml:"cooldown"`  // 0 → Config.Cooldown
}

type Config struct {
	Threshold         float64           `toml:"threshold"`
	SwitchBackDelay   Duration          `toml:"switch_back_delay"`
	SwitchBackTimeout Duration          `toml:"switch_back_timeout"`
	Cooldown          Duration          `toml:"cooldown"`          // block after a limit error with no known reset
	ContextMaxBytes   int               `toml:"context_max_bytes"` // transcript text handed to the next harness
	Poll              Duration          `toml:"poll"`
	DefaultTarget     string            `toml:"default_target"`
	Targets           map[string]Target `toml:"targets"`
	Harnesses         []Harness         `toml:"harness"`

	HandoffMessage string `toml:"handoff_message"`
	ReturnMessage  string `toml:"return_message"`
	ResumeMessage  string `toml:"resume_message"`

	StatuslineCmd string `toml:"statusline_cmd"` // default: statusLine.command from ~/.claude/settings.json
	ClaudeBin     string `toml:"claude_bin"`
	TmuxBin       string `toml:"tmux_bin"`
	StateDir      string `toml:"state_dir"`
	Notify        bool   `toml:"notify"`
}

const (
	defNotes = "Write handoff notes for the next agent to {notes}: the task, current state, next steps, " +
		"files touched, and any running background tasks. When done, run: touch {file}"
	defHandoff = "⚠ EMERGENCY SHUTDOWN PROCEDURE (agent-swap). Leaving {from} ({reason}); this conversation moves to {to}. " +
		"Pause all work immediately at the nearest safe point: finish or revert any half-applied edit and " +
		"do not start new tool calls. " + defNotes
	defReturn = "⚠ ACCOUNT CYCLE (agent-swap). {to} is available again; this conversation moves back to it. " +
		"Pause at the nearest safe point: finish or revert any half-applied edit and do not start new tool calls. " + defNotes
	defResume = "agent-swap moved this conversation from {from} to {to}. Read {context} — it has the handoff " +
		"notes and the conversation so far — then continue the task."
)

func Defaults() Config {
	home, _ := os.UserHomeDir()
	return Config{
		Threshold:         95,
		SwitchBackDelay:   Duration{10 * time.Minute},
		SwitchBackTimeout: Duration{time.Hour},
		Cooldown:          Duration{time.Hour},
		ContextMaxBytes:   60000,
		Poll:              Duration{30 * time.Second},
		Targets:           map[string]Target{}, // no built-in target: the user defines one
		HandoffMessage:    defHandoff,
		ResumeMessage:     defResume,
		ReturnMessage:     defReturn,
		ClaudeBin:         "claude",
		TmuxBin:           "tmux",
		StateDir:          filepath.Join(home, ".local", "state", "agent-swap"),
		Notify:            true,
	}
}

// DefaultConfigPath is ~/.config/agent-swap/config.toml.
func DefaultConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "agent-swap", "config.toml")
}

// LoadConfig reads path over the defaults. A missing file yields the defaults.
func LoadConfig(path string) (Config, error) {
	cfg := Defaults()
	if _, err := toml.DecodeFile(path, &cfg); err != nil && !errors.Is(err, os.ErrNotExist) {
		return cfg, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// ResolveTarget picks the API target: the flag, then default_target, then
// the only configured target.
func (c Config) ResolveTarget(flag string) (string, error) {
	name := flag
	if name == "" {
		name = c.DefaultTarget
	}
	if name == "" {
		switch len(c.Targets) {
		case 0:
			return "", errors.New("no API target configured: add a [targets.<name>] section to the config (see examples/config.toml)")
		case 1:
			for n := range c.Targets {
				return n, nil
			}
		default:
			return "", errors.New("several targets configured: set default_target or pass --target")
		}
	}
	if _, ok := c.Targets[name]; !ok {
		return "", fmt.Errorf("unknown target %q", name)
	}
	return name, nil
}

// Order is the harness preference order: the [[harness]] tables, or by
// default the subscription claude followed by claude on API target.
func (c Config) Order(target string) []Harness {
	if len(c.Harnesses) > 0 {
		return c.Harnesses
	}
	order := []Harness{{Name: "claude", Kind: "claude"}}
	if target != "" {
		order = append(order, Harness{Name: "api", Kind: "claude", Target: target})
	}
	return order
}

// Validate checks the settings agent-swap depends on; errors name the key.
func (c Config) Validate(target string) error {
	switch {
	case !(c.Threshold > 0 && c.Threshold <= 100):
		return fmt.Errorf("threshold must be > 0 and <= 100, got %v", c.Threshold)
	case c.SwitchBackDelay.Duration < 0:
		return errors.New("switch_back_delay must not be negative")
	case c.SwitchBackTimeout.Duration <= 0:
		return errors.New("switch_back_timeout must be positive")
	case c.Cooldown.Duration < 0:
		return errors.New("cooldown must not be negative")
	case c.ContextMaxBytes <= 0:
		return errors.New("context_max_bytes must be positive")
	case c.Poll.Duration <= 0:
		return errors.New("poll must be positive")
	case c.ClaudeBin == "":
		return errors.New("claude_bin must not be empty")
	case c.TmuxBin == "":
		return errors.New("tmux_bin must not be empty")
	case c.StateDir == "":
		return errors.New("state_dir must not be empty")
	case !strings.Contains(c.HandoffMessage, "{file}"):
		return errors.New("handoff_message must contain {file}")
	case !strings.Contains(c.ReturnMessage, "{file}"):
		return errors.New("return_message must contain {file}")
	}
	seen := map[string]bool{}
	for i, h := range c.Order(target) {
		t, ok := c.Targets[h.Target]
		switch {
		case h.Name == "":
			return fmt.Errorf("harness[%d].name must not be empty", i)
		case seen[h.Name]:
			return fmt.Errorf("harness name %q is used twice", h.Name)
		case !slices.Contains(Kinds, h.Kind):
			return fmt.Errorf("harness %s: kind must be one of claude, codex, pi, opencode, agy; got %q", h.Name, h.Kind)
		case h.Cooldown.Duration < 0:
			return fmt.Errorf("harness %s: cooldown must not be negative", h.Name)
		case h.Target == "":
		case h.Kind != "claude":
			return fmt.Errorf("harness %s: target is only valid for kind claude", h.Name)
		case !ok:
			return fmt.Errorf("harness %s: unknown target %q", h.Name, h.Target)
		case strings.TrimSpace(t.KeyCmd) == "":
			return fmt.Errorf("targets.%s.key_cmd must not be empty", h.Target)
		}
		seen[h.Name] = true
	}
	return nil
}

// Render fills {name} placeholders from vars.
func Render(tmpl string, vars map[string]string) string {
	for k, v := range vars {
		tmpl = strings.ReplaceAll(tmpl, "{"+k+"}", v)
	}
	return tmpl
}
