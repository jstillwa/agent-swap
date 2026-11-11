package swap

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Unix(1_800_000_000, 0)

func cfgT() Config {
	c := Defaults()
	c.SwitchBackDelay.Duration = 10 * time.Minute
	c.Cooldown.Duration = time.Hour
	return c
}

var order3 = []Harness{{Name: "claude", Kind: "claude"}, {Name: "codex", Kind: "codex"}, {Name: "pi", Kind: "pi"}}

// acct builds an Account from name → Usage.
func acct(m map[string]*Usage) Account { return Account{Harnesses: m} }

func wnd(pct float64, reset int64) *Window { return &Window{UsedPct: pct, ResetsAt: reset} }

func TestPeakIgnoresExpiredWindows(t *testing.T) {
	u := &Usage{Windows: map[string]*Window{"5h": wnd(99, t0.Unix()-1), "7d": wnd(40, t0.Unix()+3600)}}
	if pct, name, reset := u.Peak(t0); pct != 40 || name != "7d" || reset != t0.Unix()+3600 {
		t.Fatalf("got %v %v %v", pct, name, reset)
	}
	var nilU *Usage
	if pct, name, reset := nilU.Peak(t0); pct != 0 || name != "" || reset != 0 {
		t.Fatal("nil usage must peak at zero")
	}
}

func TestDesired(t *testing.T) {
	now := t0.Unix()
	cases := []struct {
		name string
		a    Account
		want string
	}{
		{"empty account", Account{}, "claude"},
		{"first free", acct(map[string]*Usage{"codex": {BlockedUntil: now + 9}}), "claude"},
		{"first blocked", acct(map[string]*Usage{"claude": {BlockedUntil: now + 9}}), "codex"},
		{"block expired at now", acct(map[string]*Usage{"claude": {BlockedUntil: now}}), "claude"},
		{"two blocked", acct(map[string]*Usage{"claude": {BlockedUntil: now + 9}, "codex": {BlockedUntil: now + 9}}), "pi"},
		{"all blocked: earliest", acct(map[string]*Usage{
			"claude": {BlockedUntil: now + 300}, "codex": {BlockedUntil: now + 100}, "pi": {BlockedUntil: now + 200},
		}), "codex"},
	}
	for _, c := range cases {
		if got := Desired(c.a, order3, t0); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestTriggerAtThreshold(t *testing.T) {
	h := order3[0]
	a := acct(map[string]*Usage{"claude": {Windows: map[string]*Window{"5h": wnd(94.9, t0.Unix()+600)}}})
	if Trigger(&a, h, cfgT(), t0, false) {
		t.Fatal("94.9 must not trigger")
	}
	a.Harnesses["claude"].Windows["5h"].UsedPct = 95
	if !Trigger(&a, h, cfgT(), t0, false) {
		t.Fatal("95 must trigger")
	}
	u := a.Harnesses["claude"]
	if u.BlockedUntil != t0.Unix()+600+600 || u.ResetsAt != t0.Unix()+600 {
		t.Fatalf("blocked_until %d resets %d", u.BlockedUntil, u.ResetsAt)
	}
	if Desired(a, order3, t0) != "codex" || Desired(a, order3, time.Unix(u.BlockedUntil, 0)) != "claude" {
		t.Fatal("desired harness wrong")
	}
	if Trigger(&a, h, cfgT(), t0, true) {
		t.Fatal("already blocked: no re-trigger")
	}
}

func TestTriggerUsesLatestResetAmongHotWindows(t *testing.T) {
	a := acct(map[string]*Usage{"claude": {Windows: map[string]*Window{
		"5h": wnd(96, t0.Unix()+100), "7d": wnd(97, t0.Unix()+90000),
	}}})
	Trigger(&a, order3[0], cfgT(), t0, false)
	if a.Harnesses["claude"].ResetsAt != t0.Unix()+90000 {
		t.Fatalf("want 7d reset, got %d", a.Harnesses["claude"].ResetsAt)
	}
}

func TestTriggerPerHarnessThreshold(t *testing.T) {
	h := Harness{Name: "codex", Kind: "codex", Threshold: 80}
	a := acct(map[string]*Usage{"codex": {Windows: map[string]*Window{"primary": wnd(80, t0.Unix()+600)}}})
	if !Trigger(&a, h, cfgT(), t0, false) {
		t.Fatal("80% must trigger with a harness threshold of 80")
	}
	h.Threshold = 0 // falls back to cfg.Threshold (95)
	a = acct(map[string]*Usage{"codex": {Windows: map[string]*Window{"primary": wnd(90, t0.Unix()+600)}}})
	if Trigger(&a, h, cfgT(), t0, false) {
		t.Fatal("90% must not trigger at the global 95")
	}
}

func TestTriggerRateLimited(t *testing.T) {
	c := cfgT()
	// A live window below threshold: its reset is the culprit.
	a := acct(map[string]*Usage{"claude": {Windows: map[string]*Window{
		"5h": wnd(80, t0.Unix()+100), "7d": wnd(30, t0.Unix()+90000),
	}}})
	if !Trigger(&a, order3[0], c, t0, true) || a.Harnesses["claude"].ResetsAt != t0.Unix()+100 {
		t.Fatalf("want fullest live window's reset: %+v", a.Harnesses["claude"])
	}
	// No live window → cooldown (global, then per-harness override).
	cases := map[string]Account{
		"no data":    {},
		"both reset": acct(map[string]*Usage{"pi": {Windows: map[string]*Window{"5h": wnd(80, t0.Unix()-1), "7d": wnd(30, t0.Unix())}}}),
	}
	for name, a := range cases {
		if !Trigger(&a, order3[2], c, t0, true) {
			t.Fatalf("%s: rate limit must trigger", name)
		}
		u := a.Harnesses["pi"]
		if want := t0.Add(time.Hour).Unix(); u.BlockedUntil != want || u.ResetsAt != want {
			t.Errorf("%s: want cooldown %d, got %+v", name, want, u)
		}
	}
	a = Account{}
	h := Harness{Name: "pi", Kind: "pi", Cooldown: Duration{30 * time.Minute}}
	Trigger(&a, h, c, t0, true)
	if got := a.Harnesses["pi"].BlockedUntil; got != t0.Add(30*time.Minute).Unix() {
		t.Fatalf("per-harness cooldown: %d", got)
	}
	if Trigger(&a, order3[1], c, t0, false) {
		t.Fatal("not limited, no data: no trigger")
	}
}

// A claude harness with a target (API billing) has no usage windows: only
// errors block it.
func TestTriggerAPIHarnessBlocksOnlyOnErrors(t *testing.T) {
	h := Harness{Name: "api", Kind: "claude", Target: "gw"}
	a := Account{}
	if Trigger(&a, h, cfgT(), t0, false) {
		t.Fatal("API harness without errors must not block")
	}
	if !Trigger(&a, h, cfgT(), t0, true) || a.Harnesses["api"].BlockedUntil != t0.Add(time.Hour).Unix() {
		t.Fatalf("limit error must block for the cooldown: %+v", a.Harnesses["api"])
	}
}

func TestWant(t *testing.T) {
	blocked := acct(map[string]*Usage{"claude": {BlockedUntil: t0.Unix() + 100}})
	cases := []struct {
		name, pin, want string
		a               Account
	}{
		{"no pin", "", "claude", Account{}},
		{"no pin, claude blocked", "", "codex", blocked},
		{"pin wins over desired", "pi", "pi", Account{}},
		{"pin holds while its harness is blocked", "claude", "claude", blocked},
		{"unknown pin ignored", "aider", "codex", blocked},
	}
	for _, c := range cases {
		if got := Want(c.a, order3, t0, c.pin); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

// Decide switches only at idle, to the pin if set, else to Desired.
func TestDecide(t *testing.T) {
	blocked := acct(map[string]*Usage{"claude": {BlockedUntil: t0.Unix() + 100}})
	idle, busy := Session{Idle: true}, Session{}
	cases := []struct {
		name, cur, pin string
		a              Account
		s              Session
		want           Action
	}{
		{"on desired", "claude", "", Account{}, idle, Noop},
		{"desired moved down, idle", "claude", "", blocked, idle, Switch},
		{"desired moved down, busy", "claude", "", blocked, busy, Noop},
		{"desired moved up, idle", "codex", "", Account{}, idle, Switch},
		{"desired moved up, busy", "pi", "", Account{}, busy, Noop},
		{"rate limited but busy", "claude", "", blocked, Session{RateLimited: true}, Noop},
		{"rate limited and idle", "claude", "", blocked, Session{Idle: true, RateLimited: true, Failed: true}, Switch},
		{"pinned elsewhere, idle", "claude", "pi", Account{}, idle, Switch},
		{"pinned elsewhere, busy", "claude", "pi", Account{}, busy, Noop},
		{"on the pin, desired differs", "pi", "pi", Account{}, idle, Noop},
		{"on a blocked pin", "claude", "claude", blocked, idle, Noop},
		{"unknown pin falls back to desired", "codex", "aider", blocked, idle, Noop},
		{"unknown pin, not on desired", "pi", "aider", blocked, idle, Switch},
	}
	for _, c := range cases {
		if got := Decide(t0, order3, c.a, c.s, c.cur, c.pin); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
	if Noop.String() != "Noop" || Switch.String() != "Switch" {
		t.Error("Action.String")
	}
}

// At or over the threshold the harness is blocked by Trigger, and the switch
// happens at the first idle (the threshold leaves room for the turn).
func TestDecideAtThresholdWaitsForIdle(t *testing.T) {
	a := acct(map[string]*Usage{"claude": {Windows: map[string]*Window{"5h": wnd(100, t0.Unix()+600)}}})
	if !Trigger(&a, order3[0], cfgT(), t0, false) {
		t.Fatal("100% must trigger")
	}
	if got := Decide(t0, order3, a, Session{}, "claude", ""); got != Noop {
		t.Fatalf("busy at 100%%: %v", got)
	}
	if got := Decide(t0, order3, a, Session{Idle: true}, "claude", ""); got != Switch {
		t.Fatalf("idle at 100%%: %v", got)
	}
}

func TestStatusText(t *testing.T) {
	c := cfgT()
	back := t0.Unix() + 7200
	usage := func(pct float64) Account {
		return acct(map[string]*Usage{"claude": {Windows: map[string]*Window{"5h": wnd(pct, t0.Unix()+3600)}}})
	}
	blocked := acct(map[string]*Usage{"claude": {BlockedUntil: back}})
	cases := []struct {
		name string
		a    Account
		cur  string
		want []string
	}{
		{"normal", usage(72), "claude", []string{"72% (5h)"}},
		{"warning band", usage(91), "claude", []string{"bg=yellow", "claude 91% (5h): switch at 95%, 4% left"}},
		{"below band", usage(89), "claude", []string{"89% (5h)"}},
		{"unknown", Account{}, "claude", []string{"claude · usage unknown"}},
		{"not first", blocked, "codex", []string{"bg=blue", "codex · back to claude at " + time.Unix(back, 0).Format("Mon 15:04")}},
	}
	for _, tc := range cases {
		got := StatusText(t0, c, order3, tc.a, tc.cur, "")
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %q lacks %q", tc.name, got, w)
			}
		}
	}
	if got := StatusText(t0, c, order3, usage(89), "claude", ""); strings.Contains(got, "bg=yellow") {
		t.Errorf("89%% must be below the yellow band: %q", got)
	}
	// "back to" names the first harness above cur that is blocked.
	a := acct(map[string]*Usage{"claude": {BlockedUntil: back}, "codex": {BlockedUntil: back + 60}})
	if got := StatusText(t0, c, order3, a, "pi", ""); !strings.Contains(got, "back to claude at") {
		t.Errorf("pi status: %q", got)
	}
}

// A pending switch and a pin show in the status line; a pin hides "back to".
func TestStatusTextSwitchAndPin(t *testing.T) {
	c := cfgT()
	blocked := acct(map[string]*Usage{"claude": {BlockedUntil: t0.Unix() + 7200}})
	cases := []struct {
		name, cur, pin, want string
		a                    Account
	}{
		{"switch pending", "claude", "", "claude → codex: switching at the next idle", blocked},
		{"pin pending", "claude", "pi", "claude → pi: switching at the next idle", Account{}},
		{"on the pin", "pi", "pi", "pi · usage unknown · pinned to pi", Account{}},
		{"pinned below a blocked harness", "codex", "codex", "pinned to codex", blocked},
	}
	for _, tc := range cases {
		got := StatusText(t0, c, order3, tc.a, tc.cur, tc.pin)
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q lacks %q", tc.name, got, tc.want)
		}
		if tc.pin != "" && strings.Contains(got, "back to") {
			t.Errorf("%s: a pin must hide back-to: %q", tc.name, got)
		}
	}
	if got := StatusText(t0, c, order3, acct(map[string]*Usage{"claude": {Windows: map[string]*Window{"5h": wnd(97, t0.Unix()+60)}}}), "claude", ""); !strings.Contains(got, "2% left") && !strings.Contains(got, "0% left") {
		t.Errorf("over threshold: %q", got)
	}
}
