// Package swap moves a Claude Code session between subscription and API
// billing when subscription usage nears its limit, and back after the reset.
package swap

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

type Config struct {
	Threshold         float64           `toml:"threshold"`
	SwitchBackDelay   Duration          `toml:"switch_back_delay"`
	SwitchBackTimeout Duration          `toml:"switch_back_timeout"`
	Poll              Duration          `toml:"poll"`
	DefaultTarget     string            `toml:"default_target"`
	Targets           map[string]Target `toml:"targets"`

	HandoffMessage      string `toml:"handoff_message"`
	ResumeMessage       string `toml:"resume_message"`
	ReturnMessage       string `toml:"return_message"`
	ReturnResumeMessage string `toml:"return_resume_message"`

	StatuslineCmd string `toml:"statusline_cmd"` // default: statusLine.command from ~/.claude/settings.json
	ClaudeBin     string `toml:"claude_bin"`
	TmuxBin       string `toml:"tmux_bin"`
	StateDir      string `toml:"state_dir"`
	Notify        bool   `toml:"notify"`
}

const (
	defHandoff = "⚠ EMERGENCY SHUTDOWN PROCEDURE (claude-swap). Subscription usage at {pct}%. " +
		"Pause all work immediately at the nearest safe point: finish or revert any half-applied edit, " +
		"do not start new tool calls, and note any running background tasks in one line. " +
		"Your session will be resumed on API billing ({target}) with full context. When ready, run: touch {file}"
	defResume = "Session resumed on API billing ({target}) by claude-swap. Background tasks from before " +
		"the restart are gone; re-launch any you noted. You may continue."
	defReturn = "⚠ ACCOUNT CYCLE (claude-swap). Subscription usage has reset. Pause at the nearest safe point: " +
		"finish or revert any half-applied edit, do not start new tool calls, and note any running background " +
		"tasks in one line. Your session will be resumed on the subscription with full context. When ready, run: touch {file}"
	defReturnResume = "Session resumed on subscription by claude-swap. Background tasks from before the " +
		"restart are gone; re-launch any you noted. You may continue."
)

func Defaults() Config {
	home, _ := os.UserHomeDir()
	return Config{
		Threshold:           95,
		SwitchBackDelay:     Duration{10 * time.Minute},
		SwitchBackTimeout:   Duration{time.Hour},
		Poll:                Duration{30 * time.Second},
		Targets:             map[string]Target{}, // no built-in target: the user defines one
		HandoffMessage:      defHandoff,
		ResumeMessage:       defResume,
		ReturnMessage:       defReturn,
		ReturnResumeMessage: defReturnResume,
		ClaudeBin:           "claude",
		TmuxBin:             "tmux",
		StateDir:            filepath.Join(home, ".local", "state", "claude-swap"),
		Notify:              true,
	}
}

// DefaultConfigPath is ~/.config/claude-swap/config.toml.
func DefaultConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "claude-swap", "config.toml")
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

// Render fills {file} {pct} {target} {resets_at} placeholders.
func Render(tmpl string, vars map[string]string) string {
	for k, v := range vars {
		tmpl = strings.ReplaceAll(tmpl, "{"+k+"}", v)
	}
	return tmpl
}
