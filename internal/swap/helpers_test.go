package swap

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// statuslineEnv points the statusline at temp dirs for harness name.
func statuslineEnv(t *testing.T, harness string) (stateDir, sessDir string) {
	t.Helper()
	stateDir, sessDir = t.TempDir(), t.TempDir()
	t.Setenv(EnvStateDir, stateDir)
	t.Setenv(EnvSessionDir, sessDir)
	t.Setenv(EnvHarness, harness)
	t.Setenv(EnvStatusline, "")
	return
}

func TestStatuslineRecordsAndPassesThrough(t *testing.T) {
	sd, dir := statuslineEnv(t, "claude-max")
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
	u := a.Harnesses["claude-max"]
	if u == nil || u.Windows["5h"] == nil || u.Windows["5h"].UsedPct != 91 || u.Windows["7d"] != nil {
		t.Fatalf("account %+v", a)
	}
	if len(a.Harnesses) != 1 {
		t.Fatalf("only $%s may be written: %+v", EnvHarness, a.Harnesses)
	}
	s, _ := Load[Session](SessionPath(dir))
	if s.NativeID != "abc" {
		t.Fatalf("session %+v", s)
	}
	_ = Statusline(strings.NewReader(`{"session_id":"a b/../c"}`), &out, t0)
	if s, _ = Load[Session](SessionPath(dir)); s.NativeID != "abc" {
		t.Fatalf("statusline must ignore an unsafe session_id: %+v", s)
	}
	// API billing: rate_limits null must not wipe stored data.
	_ = Statusline(strings.NewReader(`{"session_id":"abc","rate_limits":null}`), &out, t0)
	if a, _ = Load[Account](AccountPath(sd)); a.Harnesses["claude-max"].Windows["5h"] == nil {
		t.Fatal("null rate_limits erased account data")
	}
}

// A payload carrying only seven_day keeps a live stored 5h window and clears
// one that has already reset; usage within a window never drops.
func TestStatuslineMergesWindows(t *testing.T) {
	sd, _ := statuslineEnv(t, "claude")
	t.Setenv(EnvSessionDir, "")
	seed := func(five *Window) {
		t.Helper()
		if err := Update(AccountPath(sd), func(a *Account) {
			a.Harnesses = map[string]*Usage{"claude": {Windows: map[string]*Window{"7d": wnd(20, t0.Unix()+90000)}},
				"codex": {Windows: map[string]*Window{"primary": wnd(50, t0.Unix()+90000)}}}
			if five != nil {
				a.Harnesses["claude"].Windows["5h"] = five
			}
		}); err != nil {
			t.Fatal(err)
		}
	}
	only7d := `{"rate_limits":{"seven_day":{"used_percentage":31,"resets_at":1800090000}}}`
	feed := func(in string) map[string]*Window {
		t.Helper()
		if err := Statusline(strings.NewReader(in), &bytes.Buffer{}, t0); err != nil {
			t.Fatal(err)
		}
		a, err := Load[Account](AccountPath(sd))
		if err != nil {
			t.Fatal(err)
		}
		if a.Harnesses["codex"] == nil || a.Harnesses["codex"].Windows["primary"].UsedPct != 50 {
			t.Fatalf("other harnesses must be untouched: %+v", a.Harnesses["codex"])
		}
		return a.Harnesses["claude"].Windows
	}
	is := func(w *Window, pct float64, reset int64) bool {
		return w != nil && w.UsedPct == pct && w.ResetsAt == reset
	}

	seed(wnd(88, t0.Unix()+600))
	w := feed(only7d)
	if !is(w["5h"], 88, t0.Unix()+600) {
		t.Fatalf("live 5h must be kept: %+v", w["5h"])
	}
	if !is(w["7d"], 31, 1800090000) {
		t.Fatalf("7d must be replaced: %+v", w["7d"])
	}

	seed(wnd(88, t0.Unix()+600))
	w = feed(`{"rate_limits":{"five_hour":{"used_percentage":5,"resets_at":1800018000}}}`)
	if !is(w["5h"], 5, 1800018000) || !is(w["7d"], 20, t0.Unix()+90000) {
		t.Fatalf("later 5h must replace, live 7d kept: %+v %+v", w["5h"], w["7d"])
	}

	seed(wnd(88, t0.Unix()+600))
	for _, in := range []string{
		`{"rate_limits":{"five_hour":{"used_percentage":50,"resets_at":1800000600}}}`, // same reset, lower
		`{"rate_limits":{"five_hour":{"used_percentage":99,"resets_at":1800000300}}}`, // older reset
	} {
		if w = feed(in); !is(w["5h"], 88, t0.Unix()+600) {
			t.Fatalf("stale payload %s lowered the stored window: %+v", in, w["5h"])
		}
	}
	if w = feed(`{"rate_limits":{"five_hour":{"used_percentage":90,"resets_at":1800000600}}}`); w["5h"].UsedPct != 90 {
		t.Fatalf("same reset, higher usage must replace: %+v", w["5h"])
	}
	seed(wnd(88, t0.Unix())) // stored window has reset: any payload wins
	if w = feed(`{"rate_limits":{"five_hour":{"used_percentage":3,"resets_at":1800018000}}}`); !is(w["5h"], 3, 1800018000) {
		t.Fatalf("reset stored window must be replaced: %+v", w["5h"])
	}
	if w = feed(`{"rate_limits":null}`); w["5h"] == nil || w["7d"] == nil {
		t.Fatalf("null rate_limits changed data: %+v", w)
	}
}

func TestHookStateTransitions(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvSessionDir, dir)
	step := func(ev, body string) Session {
		t.Helper()
		if err := Hook(ev, strings.NewReader(body), t0); err != nil {
			t.Fatal(err)
		}
		s, _ := Load[Session](SessionPath(dir))
		return s
	}
	// SessionStart keeps Idle as the runner seeded it (busy after a prompted launch).
	for _, idle := range []bool{true, false} {
		if err := Update(SessionPath(dir), func(s *Session) { *s = Session{Idle: idle, RateLimited: true} }); err != nil {
			t.Fatal(err)
		}
		if s := step("SessionStart", `{"session_id":"x"}`); s.Idle != idle || s.RateLimited || s.StartedAt != t0.Unix() || s.NativeID != "x" {
			t.Fatalf("start from idle=%v: %+v", idle, s)
		}
	}
	if s := step("UserPromptSubmit", `{}`); s.Idle || s.NativeID != "x" {
		t.Fatalf("prompt must mark busy and keep the id: %+v", s)
	}
	if s := step("StopFailure", `{"error":"rate_limit"}`); !s.Idle || !s.RateLimited {
		t.Fatalf("stopfailure rate_limit %+v", s)
	}
	if s := step("Stop", ``); !s.Idle || s.RateLimited { // empty stdin is fine
		t.Fatalf("stop %+v", s)
	}
	if s := step("StopFailure", `{"error":"server_error"}`); !s.Idle || s.RateLimited {
		t.Fatalf("non-limit failure %+v", s)
	}
	if s := step("StopFailure", `{"session_id":"y","error":"429 Too Many Requests"}`); !s.RateLimited || s.NativeID != "y" {
		t.Fatalf("429 failure %+v", s)
	}
	if s := step("UserPromptSubmit", `{}`); s.Idle || s.RateLimited {
		t.Fatalf("a new prompt clears the limit flag: %+v", s)
	}
	if s := step("Stop", `{"session_id":"../../etc/x"}`); s.NativeID != "y" {
		t.Fatalf("an unsafe session_id must be ignored: %+v", s)
	}
	// LimitHit flags the limit without touching Idle.
	for _, idle := range []bool{false, true} {
		if err := Update(SessionPath(dir), func(s *Session) { *s = Session{Idle: idle} }); err != nil {
			t.Fatal(err)
		}
		if s := step("LimitHit", `{}`); !s.RateLimited || s.Idle != idle {
			t.Fatalf("LimitHit from idle=%v: %+v", idle, s)
		}
	}
	t.Setenv(EnvSessionDir, "")
	if err := Hook("Stop", strings.NewReader(`{}`), t0); err != nil {
		t.Fatalf("no session dir must be a no-op: %v", err)
	}
}

func TestIsLimitError(t *testing.T) {
	yes := []string{"rate_limit", "Rate limit reached", "HTTP 429", "error 402", "quota exceeded", "You've hit your usage limit",
		"Budget exceeded", "RESOURCE_EXHAUSTED", "insufficient_balance", "status 402"}
	no := []string{"", "server_error", "overloaded", "context length exceeded", "connection reset",
		"1402 tokens", "port 4290", "ts .402102"}
	for _, s := range yes {
		if !IsLimitError(s) {
			t.Errorf("IsLimitError(%q) = false", s)
		}
	}
	for _, s := range no {
		if IsLimitError(s) {
			t.Errorf("IsLimitError(%q) = true", s)
		}
	}
}

func TestSettingsJSON(t *testing.T) {
	var sub, api map[string]any
	b, _ := SettingsJSON("/bin/cs", "", Target{})
	_ = json.Unmarshal(b, &sub)
	if _, ok := sub["apiKeyHelper"]; ok {
		t.Fatal("subscription settings must not set a key")
	}
	if _, ok := sub["env"]; ok {
		t.Fatal("subscription settings must not set env")
	}
	b, _ = SettingsJSON("/bin/cs", "gateway", Target{BaseURL: "https://x/anthropic"})
	_ = json.Unmarshal(b, &api)
	if api["apiKeyHelper"] != "/bin/cs key gateway" || api["env"].(map[string]any)["ANTHROPIC_BASE_URL"] != "https://x/anthropic" {
		t.Fatalf("api settings %v", api)
	}
	for _, s := range []map[string]any{sub, api} {
		if len(s["hooks"].(map[string]any)) != 4 || s["statusLine"].(map[string]any)["command"] != "/bin/cs statusline" {
			t.Fatalf("hooks/statusLine missing: %v", s)
		}
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

// Stop and StopFailure each count a completed turn; StopFailure marks it
// failed until the next prompt or Stop.
func TestHookTurnCounter(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvSessionDir, dir)
	step := func(ev, body string) Session {
		t.Helper()
		if err := Hook(ev, strings.NewReader(body), t0); err != nil {
			t.Fatal(err)
		}
		s, _ := Load[Session](SessionPath(dir))
		return s
	}
	want := []struct {
		ev, body     string
		turns        int
		failed, idle bool
	}{
		{"SessionStart", `{"session_id":"s"}`, 0, false, false},
		{"UserPromptSubmit", `{}`, 0, false, false},
		{"Stop", `{}`, 1, false, true},
		{"UserPromptSubmit", `{}`, 1, false, false},
		{"StopFailure", `{"error":"server_error"}`, 2, true, true},
		{"LimitHit", `{}`, 2, true, true},
		{"SessionStart", `{}`, 2, true, true},
		{"UserPromptSubmit", `{}`, 2, false, false},
		{"StopFailure", `{"error":"rate_limit"}`, 3, true, true},
		{"Stop", `{}`, 4, false, true},
	}
	for i, w := range want {
		s := step(w.ev, w.body)
		if s.Turns != w.turns || s.Failed != w.failed || s.Idle != w.idle {
			t.Fatalf("step %d %s: turns=%d failed=%v idle=%v, want %d %v %v", i, w.ev, s.Turns, s.Failed, s.Idle, w.turns, w.failed, w.idle)
		}
	}
}

func TestStatePaths(t *testing.T) {
	for got, want := range map[string]string{
		AccountPath("/s"): "/s/account.json", SessionPath("/d"): "/d/session.json",
		RequestPath("/d"): "/d/request.json", MirrorsPath("/d"): "/d/mirrors.json",
	} {
		if got != want {
			t.Errorf("%s != %s", got, want)
		}
	}
}
