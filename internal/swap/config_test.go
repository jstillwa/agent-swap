package swap

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func names(hs []Harness) []string {
	var out []string
	for _, h := range hs {
		out = append(out, h.Name+"/"+h.Kind+"/"+h.Target)
	}
	return out
}

func TestOrderSynthesizedWithoutHarnessTables(t *testing.T) {
	c := Defaults()
	if got := names(c.Order("gw")); !slices.Equal(got, []string{"claude/claude/", "api/claude/gw"}) {
		t.Fatalf("Order(gw) = %q", got)
	}
	if got := names(c.Order("")); !slices.Equal(got, []string{"claude/claude/"}) {
		t.Fatalf("Order(\"\") = %q", got)
	}
	c.Harnesses = []Harness{{Name: "codex", Kind: "codex"}, {Name: "claude", Kind: "claude"}}
	if got := names(c.Order("gw")); !slices.Equal(got, []string{"codex/codex/", "claude/claude/"}) {
		t.Fatalf("configured order must win: %q", got)
	}
}

func TestLoadConfigHarnessTables(t *testing.T) {
	p := t.TempDir() + "/c.toml"
	body := `threshold = 90
switch_back_delay = "15m"
cooldown = "2h"
context_max_bytes = 5000
[[harness]]
name = "claude"
kind = "claude"
[[harness]]
name = "pi-deepseek"
kind = "pi"
args = ["--provider", "deepseek"]
threshold = 80
cooldown = "30m"
[[harness]]
name = "claude-api"
kind = "claude"
target = "console"
[targets.console]
key_cmd = "echo k"
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Threshold != 90 || c.SwitchBackDelay.Minutes() != 15 || c.Cooldown.Hours() != 2 || c.ContextMaxBytes != 5000 {
		t.Fatalf("globals %+v", c)
	}
	if got := names(c.Order("")); !slices.Equal(got, []string{"claude/claude/", "pi-deepseek/pi/", "claude-api/claude/console"}) {
		t.Fatalf("order %q", got)
	}
	pi := c.Harnesses[1]
	if !slices.Equal(pi.Args, []string{"--provider", "deepseek"}) || pi.Threshold != 80 || pi.Cooldown.Minutes() != 30 {
		t.Fatalf("pi harness %+v", pi)
	}
	if err := c.Validate(""); err != nil {
		t.Fatalf("valid config: %v", err)
	}
}

func TestDefaultsNewKeys(t *testing.T) {
	c := Defaults()
	if c.Cooldown.Duration != time.Hour || c.ContextMaxBytes != 60000 {
		t.Fatalf("cooldown %v context_max_bytes %d", c.Cooldown, c.ContextMaxBytes)
	}
	for _, m := range []string{c.HandoffMessage, c.ReturnMessage} {
		if !strings.Contains(m, "{notes}") || !strings.Contains(m, "{file}") {
			t.Errorf("message must ask for notes and the touch file: %q", m)
		}
	}
	if !strings.Contains(c.HandoffMessage, "EMERGENCY SHUTDOWN PROCEDURE (agent-swap)") ||
		!strings.Contains(c.ReturnMessage, "ACCOUNT CYCLE (agent-swap)") || !strings.Contains(c.ResumeMessage, "{context}") {
		t.Error("default messages lost their markers")
	}
}

func TestLoadConfigOverlaysFile(t *testing.T) {
	p := t.TempDir() + "/c.toml"
	_ = os.WriteFile(p, []byte("threshold = 90\nswitch_back_delay = \"15m\"\n[targets.console]\nkey_cmd = \"echo k\"\n"), 0o600)
	c, err := LoadConfig(p)
	if err != nil || c.Threshold != 90 || c.SwitchBackDelay.Minutes() != 15 || c.Targets["console"].KeyCmd != "echo k" || len(c.Targets) != 1 {
		t.Fatalf("%v %+v", err, c)
	}
	if _, err := LoadConfig(t.TempDir() + "/missing.toml"); err != nil {
		t.Fatal("missing file must yield defaults")
	}
}

func TestResolveTarget(t *testing.T) {
	c := Defaults()
	if len(c.Targets) != 0 || c.DefaultTarget != "" {
		t.Fatal("defaults must ship no built-in target")
	}
	if _, err := c.ResolveTarget(""); err == nil {
		t.Fatal("no targets must be an error")
	}
	c.Targets = map[string]Target{"only": {KeyCmd: "x"}}
	if got, err := c.ResolveTarget(""); err != nil || got != "only" {
		t.Fatalf("single target: %q %v", got, err)
	}
	c.Targets["second"] = Target{KeyCmd: "y"}
	if _, err := c.ResolveTarget(""); err == nil {
		t.Fatal("two targets and no default_target must be an error")
	}
	c.DefaultTarget = "second"
	if got, _ := c.ResolveTarget(""); got != "second" {
		t.Fatal(got)
	}
	if got, _ := c.ResolveTarget("only"); got != "only" {
		t.Fatal("flag must override default_target")
	}
	if _, err := c.ResolveTarget("missing"); err == nil {
		t.Fatal("unknown target must be an error")
	}
}

// Validate rejects out-of-range or empty settings and names the key.
func TestConfigValidate(t *testing.T) {
	valid := func() Config {
		c := Defaults()
		c.Targets = map[string]Target{"gw": {KeyCmd: "echo k"}}
		return c
	}
	if err := valid().Validate("gw"); err != nil {
		t.Fatalf("defaults + target must be valid: %v", err)
	}
	ok := []struct {
		name string
		mut  func(*Config)
	}{
		{"threshold 100", func(c *Config) { c.Threshold = 100 }},
		{"threshold 0.5", func(c *Config) { c.Threshold = 0.5 }},
		{"switch_back_delay 0", func(c *Config) { c.SwitchBackDelay.Duration = 0 }},
		{"cooldown 0", func(c *Config) { c.Cooldown.Duration = 0 }},
	}
	for _, c := range ok {
		cfg := valid()
		c.mut(&cfg)
		if err := cfg.Validate("gw"); err != nil {
			t.Errorf("%s: want valid, got %v", c.name, err)
		}
	}
	bad := []struct {
		key string
		mut func(*Config)
	}{
		{"threshold", func(c *Config) { c.Threshold = 0 }},
		{"threshold", func(c *Config) { c.Threshold = -5 }},
		{"threshold", func(c *Config) { c.Threshold = 100.1 }},
		{"switch_back_delay", func(c *Config) { c.SwitchBackDelay.Duration = -time.Second }},
		{"switch_back_timeout", func(c *Config) { c.SwitchBackTimeout.Duration = 0 }},
		{"poll", func(c *Config) { c.Poll.Duration = 0 }},
		{"claude_bin", func(c *Config) { c.ClaudeBin = "" }},
		{"tmux_bin", func(c *Config) { c.TmuxBin = "" }},
		{"state_dir", func(c *Config) { c.StateDir = "" }},
		{"key_cmd", func(c *Config) { c.Targets["gw"] = Target{KeyCmd: "  \t\n"} }},
		{"cooldown", func(c *Config) { c.Cooldown.Duration = -time.Minute }},
		{"context_max_bytes", func(c *Config) { c.ContextMaxBytes = 0 }},
		{"context_max_bytes", func(c *Config) { c.ContextMaxBytes = -1 }},
		{"handoff_message", func(c *Config) { c.HandoffMessage = "stop now, notes to {notes}" }},
		{"return_message", func(c *Config) { c.ReturnMessage = "switching back" }},
	}
	for _, c := range bad {
		cfg := valid()
		c.mut(&cfg)
		if err := cfg.Validate("gw"); err == nil || !strings.Contains(err.Error(), c.key) {
			t.Errorf("%s: want an error naming the key, got %v", c.key, err)
		}
	}
	if err := valid().Validate("missing"); err == nil {
		t.Error("unknown target must fail validation")
	}
}

func TestConfigValidateHarnesses(t *testing.T) {
	valid := func() Config {
		c := Defaults()
		c.Targets = map[string]Target{"gw": {KeyCmd: "echo k"}}
		c.Harnesses = []Harness{{Name: "claude", Kind: "claude"}, {Name: "codex", Kind: "codex"},
			{Name: "pi", Kind: "pi"}, {Name: "oc", Kind: "opencode"}, {Name: "agy", Kind: "agy"},
			{Name: "api", Kind: "claude", Target: "gw"}}
		return c
	}
	if err := valid().Validate(""); err != nil {
		t.Fatalf("all five kinds + a target must be valid: %v", err)
	}
	bad := []struct {
		name, key string
		mut       func(*Config)
	}{
		{"duplicate name", "codex", func(c *Config) { c.Harnesses[2].Name = "codex" }},
		{"empty name", "name", func(c *Config) { c.Harnesses[1].Name = "" }},
		{"unknown kind", "kind", func(c *Config) { c.Harnesses[1].Kind = "aider" }},
		{"target on non-claude", "target", func(c *Config) { c.Harnesses[1].Target = "gw" }},
		{"missing target", "nope", func(c *Config) { c.Harnesses[5].Target = "nope" }},
		{"target without key_cmd", "key_cmd", func(c *Config) { c.Targets["gw"] = Target{} }},
		{"negative harness cooldown", "cooldown", func(c *Config) { c.Harnesses[2].Cooldown.Duration = -time.Second }},
		{"context_max_bytes", "context_max_bytes", func(c *Config) { c.ContextMaxBytes = 0 }},
	}
	for _, c := range bad {
		cfg := valid()
		c.mut(&cfg)
		if err := cfg.Validate(""); err == nil || !strings.Contains(err.Error(), c.key) {
			t.Errorf("%s: want an error mentioning %q, got %v", c.name, c.key, err)
		}
	}
}
