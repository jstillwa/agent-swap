package swap

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestStatuslineRecordsAndPassesThrough(t *testing.T) {
	sd, dir := t.TempDir(), t.TempDir()
	t.Setenv(EnvStateDir, sd)
	t.Setenv(EnvSessionDir, dir)
	t.Setenv(EnvStatusline, "jq -r .model.id")
	in := `{"session_id":"abc","model":{"id":"m1"},"rate_limits":{"five_hour":{"used_percentage":91,"resets_at":1800000600}}}`
	var out bytes.Buffer
	if err := Statusline(strings.NewReader(in), &out, t0); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "m1" {
		t.Fatalf("user statusline output %q", out.String())
	}
	a, _ := Load[Account](AccountPath(sd))
	if a.FiveHour == nil || a.FiveHour.UsedPct != 91 || a.SevenDay != nil {
		t.Fatalf("account %+v", a)
	}
	s, _ := Load[Session](SessionPath(dir))
	if s.ClaudeSessionID != "abc" {
		t.Fatalf("session %+v", s)
	}
	// API billing: rate_limits null must not wipe stored data.
	_ = Statusline(strings.NewReader(`{"session_id":"abc","rate_limits":null}`), &out, t0)
	if a, _ = Load[Account](AccountPath(sd)); a.FiveHour == nil {
		t.Fatal("null rate_limits erased account data")
	}
}

func TestHookStateTransitions(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvSessionDir, dir)
	step := func(ev, body string) Session {
		if err := Hook(ev, strings.NewReader(body), t0); err != nil {
			t.Fatal(err)
		}
		s, _ := Load[Session](SessionPath(dir))
		return s
	}
	if s := step("SessionStart", `{"session_id":"x"}`); !s.Idle || s.StartedAt != t0.Unix() || s.ClaudeSessionID != "x" {
		t.Fatalf("start %+v", s)
	}
	if s := step("UserPromptSubmit", `{}`); s.Idle {
		t.Fatal("prompt must mark busy")
	}
	if s := step("StopFailure", `{"error":"rate_limit"}`); !s.Idle || !s.RateLimited {
		t.Fatalf("stopfailure %+v", s)
	}
	if s := step("Stop", `{}`); !s.Idle || s.RateLimited {
		t.Fatalf("stop %+v", s)
	}
}

func TestSettingsJSON(t *testing.T) {
	var sub, api map[string]any
	b, _ := SettingsJSON("/bin/cs", Sub, "gateway", Target{BaseURL: "https://x/anthropic"})
	_ = json.Unmarshal(b, &sub)
	if _, ok := sub["apiKeyHelper"]; ok {
		t.Fatal("sub settings must not set a key")
	}
	b, _ = SettingsJSON("/bin/cs", API, "gateway", Target{BaseURL: "https://x/anthropic"})
	_ = json.Unmarshal(b, &api)
	if api["apiKeyHelper"] != "/bin/cs key gateway" || api["env"].(map[string]any)["ANTHROPIC_BASE_URL"] != "https://x/anthropic" {
		t.Fatalf("api settings %v", api)
	}
	if len(api["hooks"].(map[string]any)) != 4 {
		t.Fatal("want 4 hooks")
	}
}

func TestKeyRejectsEmptyOutput(t *testing.T) {
	cfg := Defaults()
	cfg.Targets = map[string]Target{"t": {KeyCmd: "echo null"}, "ok": {KeyCmd: "printf ' k1 \n'"}}
	var out bytes.Buffer
	if Key(cfg, "t", &out) == nil || Key(cfg, "missing", &out) == nil {
		t.Fatal("want errors")
	}
	if err := Key(cfg, "ok", &out); err != nil || out.String() != "k1\n" {
		t.Fatalf("%v %q", err, out.String())
	}
}

func TestTriggerRateLimitedUsesFullestWindow(t *testing.T) {
	a := Account{
		FiveHour: &Window{UsedPct: 80, ResetsAt: t0.Unix() + 100},
		SevenDay: &Window{UsedPct: 30, ResetsAt: t0.Unix() + 90000},
	}
	Trigger(&a, t0, cfgT(), true)
	if a.ResetsAt != t0.Unix()+100 {
		t.Fatalf("got %d", a.ResetsAt)
	}
}

func TestShellJoin(t *testing.T) {
	if got := ShellJoin([]string{"/a b/cs", "run", "--", "it's"}); got != `'/a b/cs' run -- 'it'\''s'` {
		t.Fatal(got)
	}
}

func TestSessionNameUnique(t *testing.T) {
	taken := map[string]bool{"as-my-proj": true, "as-my-proj-2": true}
	if got := SessionName("/x/my proj", func(n string) bool { return taken[n] }); got != "as-my-proj-3" {
		t.Fatal(got)
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

// F6: a payload carrying only seven_day keeps a live stored five_hour and
// clears one that has already reset.
func TestStatuslineMergesWindows(t *testing.T) {
	sd := t.TempDir()
	t.Setenv(EnvStateDir, sd)
	t.Setenv(EnvSessionDir, "")
	t.Setenv(EnvStatusline, "")
	seed := func(five *Window) {
		t.Helper()
		if err := Update(AccountPath(sd), func(a *Account) {
			a.FiveHour, a.SevenDay = five, &Window{UsedPct: 20, ResetsAt: t0.Unix() + 90000}
		}); err != nil {
			t.Fatal(err)
		}
	}
	only7d := `{"rate_limits":{"seven_day":{"used_percentage":31,"resets_at":1800090000}}}`
	feed := func(in string) Account {
		t.Helper()
		if err := Statusline(strings.NewReader(in), &bytes.Buffer{}, t0); err != nil {
			t.Fatal(err)
		}
		a, err := Load[Account](AccountPath(sd))
		if err != nil {
			t.Fatal(err)
		}
		return a
	}

	seed(&Window{UsedPct: 88, ResetsAt: t0.Unix() + 600})
	a := feed(only7d)
	if a.FiveHour == nil || a.FiveHour.UsedPct != 88 || a.FiveHour.ResetsAt != t0.Unix()+600 {
		t.Fatalf("live five_hour must be kept: %+v", a.FiveHour)
	}
	if a.SevenDay == nil || a.SevenDay.UsedPct != 31 || a.SevenDay.ResetsAt != 1800090000 {
		t.Fatalf("seven_day must be replaced: %+v", a.SevenDay)
	}

	seed(&Window{UsedPct: 88, ResetsAt: t0.Unix()}) // reset at now: no longer live
	if a = feed(only7d); a.FiveHour != nil {
		t.Fatalf("reset five_hour must be cleared: %+v", a.FiveHour)
	}

	// A non-nil window in the payload always replaces the stored one.
	seed(&Window{UsedPct: 88, ResetsAt: t0.Unix() + 600})
	a = feed(`{"rate_limits":{"five_hour":{"used_percentage":5,"resets_at":1800018000}}}`)
	if a.FiveHour == nil || a.FiveHour.UsedPct != 5 || a.FiveHour.ResetsAt != 1800018000 {
		t.Fatalf("five_hour must be replaced: %+v", a.FiveHour)
	}
	if a.SevenDay == nil || a.SevenDay.UsedPct != 20 {
		t.Fatalf("live seven_day must be kept: %+v", a.SevenDay)
	}

	// Monotonic: a payload with an older reset, or the same reset and lower
	// usage, does not lower a live stored window; equal reset and higher does.
	seed(&Window{UsedPct: 88, ResetsAt: t0.Unix() + 600})
	for _, in := range []string{
		`{"rate_limits":{"five_hour":{"used_percentage":50,"resets_at":1800000600}}}`, // same reset, lower
		`{"rate_limits":{"five_hour":{"used_percentage":99,"resets_at":1800000300}}}`, // older reset
	} {
		if a = feed(in); a.FiveHour == nil || a.FiveHour.UsedPct != 88 || a.FiveHour.ResetsAt != t0.Unix()+600 {
			t.Fatalf("stale payload %s lowered the stored window: %+v", in, a.FiveHour)
		}
	}
	if a = feed(`{"rate_limits":{"five_hour":{"used_percentage":90,"resets_at":1800000600}}}`); a.FiveHour.UsedPct != 90 {
		t.Fatalf("same reset, higher usage must replace: %+v", a.FiveHour)
	}
	seed(&Window{UsedPct: 88, ResetsAt: t0.Unix()}) // stored window has reset: any payload wins
	if a = feed(`{"rate_limits":{"five_hour":{"used_percentage":3,"resets_at":1800018000}}}`); a.FiveHour == nil || a.FiveHour.UsedPct != 3 {
		t.Fatalf("reset stored window must be replaced: %+v", a.FiveHour)
	}

	// rate_limits null still leaves everything untouched.
	before := feed(`{"rate_limits":null}`)
	if before.FiveHour == nil || before.SevenDay == nil {
		t.Fatalf("null rate_limits changed data: %+v", before)
	}
}

// F7: a new prompt clears a stale rate-limited flag.
func TestHookUserPromptSubmitClearsRateLimited(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvSessionDir, dir)
	for _, ev := range []string{"StopFailure", "UserPromptSubmit"} {
		if err := Hook(ev, strings.NewReader(`{"error":"rate_limit"}`), t0); err != nil {
			t.Fatal(err)
		}
	}
	s, _ := Load[Session](SessionPath(dir))
	if s.Idle || s.RateLimited {
		t.Fatalf("after UserPromptSubmit want busy and not rate-limited: %+v", s)
	}
}
