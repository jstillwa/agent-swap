package swap

import (
	"strings"
	"testing"
	"time"
)

// F4: Validate rejects out-of-range or empty settings and names the key.
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
		{"switch_back_timeout", func(c *Config) { c.SwitchBackTimeout.Duration = -time.Minute }},
		{"poll", func(c *Config) { c.Poll.Duration = 0 }},
		{"poll", func(c *Config) { c.Poll.Duration = -time.Second }},
		{"claude_bin", func(c *Config) { c.ClaudeBin = "" }},
		{"tmux_bin", func(c *Config) { c.TmuxBin = "" }},
		{"state_dir", func(c *Config) { c.StateDir = "" }},
		{"key_cmd", func(c *Config) { c.Targets["gw"] = Target{KeyCmd: ""} }},
		{"key_cmd", func(c *Config) { c.Targets["gw"] = Target{KeyCmd: "  \t\n"} }},
	}
	for _, c := range bad {
		cfg := valid()
		c.mut(&cfg)
		err := cfg.Validate("gw")
		if err == nil {
			t.Errorf("%s: want error", c.key)
			continue
		}
		if !strings.Contains(err.Error(), c.key) {
			t.Errorf("%s: error %q does not name the key", c.key, err)
		}
	}
	if err := valid().Validate("missing"); err == nil {
		t.Error("unknown target must fail validation")
	}
}
