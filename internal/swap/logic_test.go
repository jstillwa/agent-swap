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
	c.SwitchBackTimeout.Duration = time.Hour
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

func TestDecideNoPhase(t *testing.T) {
	blocked := acct(map[string]*Usage{"claude": {BlockedUntil: t0.Unix() + 100}})
	if got := Decide(t0, cfgT(), order3, Account{}, Session{Idle: true}, "claude", nil, false); got != Noop {
		t.Fatalf("on desired: %v", got)
	}
	if got := Decide(t0, cfgT(), order3, blocked, Session{Idle: true}, "claude", nil, false); got != StartHandoff {
		t.Fatalf("desired moved down: %v", got)
	}
	if got := Decide(t0, cfgT(), order3, Account{}, Session{}, "codex", nil, false); got != StartHandoff {
		t.Fatalf("desired moved up: %v", got)
	}
	// Already rate limited and idle: nothing to wrap up, switch at once.
	if got := Decide(t0, cfgT(), order3, blocked, Session{Idle: true, RateLimited: true}, "claude", nil, false); got != Switch {
		t.Fatalf("rate-limited idle: %v", got)
	}
	if got := Decide(t0, cfgT(), order3, blocked, Session{RateLimited: true}, "claude", nil, false); got != StartHandoff {
		t.Fatalf("rate-limited busy: %v", got)
	}
}

func TestDecideDownTheListReadiness(t *testing.T) {
	// claude blocked for 48h, so codex stays desired across the 24h probe.
	a := acct(map[string]*Usage{"claude": {BlockedUntil: t0.Unix() + 48*3600,
		Windows: map[string]*Window{"5h": wnd(97, t0.Add(30*time.Hour).Unix())}}})
	ph := &Phase{To: "codex", Started: t0.Unix()}
	cases := []struct {
		name string
		s    Session
		file bool
		want Action
	}{
		{"waiting, no timeout", Session{Idle: true}, false, Noop},
		{"touched but mid-turn", Session{Idle: false}, true, Noop},
		{"touched and idle", Session{Idle: true}, true, Switch},
		{"rate limited", Session{Idle: true, RateLimited: true}, false, Switch},
	}
	for _, c := range cases {
		for _, at := range []time.Time{t0, t0.Add(24 * time.Hour)} { // no timeout down the list
			if got := Decide(at, cfgT(), order3, a, c.s, "claude", ph, c.file); got != c.want {
				t.Errorf("%s at %v: got %v", c.name, at.Sub(t0), got)
			}
		}
	}
	a.Harnesses["claude"].Windows["5h"].UsedPct = 100
	if got := Decide(t0, cfgT(), order3, a, Session{Idle: true}, "claude", ph, false); got != Switch {
		t.Fatalf("100%% must switch, got %v", got)
	}
}

func TestDecideUpTheListTimeout(t *testing.T) {
	a := Account{} // claude free again
	ph := &Phase{To: "claude", Started: t0.Unix()}
	if got := Decide(t0.Add(59*time.Minute), cfgT(), order3, a, Session{Idle: true}, "codex", ph, false); got != Noop {
		t.Fatalf("before timeout: %v", got)
	}
	if got := Decide(t0.Add(time.Hour), cfgT(), order3, a, Session{Idle: false}, "codex", ph, false); got != Noop {
		t.Fatalf("timeout but busy: %v", got)
	}
	if got := Decide(t0.Add(time.Hour), cfgT(), order3, a, Session{Idle: true}, "codex", ph, false); got != Switch {
		t.Fatalf("timeout and idle: %v", got)
	}
	if got := Decide(t0, cfgT(), order3, a, Session{Idle: true}, "codex", ph, true); got != Switch {
		t.Fatalf("touched: %v", got)
	}
	// Moving up, a rate-limit flag alone is not readiness.
	if got := Decide(t0, cfgT(), order3, a, Session{Idle: true, RateLimited: true}, "codex", ph, false); got != Noop {
		t.Fatalf("up the list, rate limited: %v", got)
	}
}

// A handoff whose target no longer matches the desired harness is cancelled,
// whether or not the session is idle.
func TestDecideCancelsStaleHandoff(t *testing.T) {
	free := Account{}
	claudeBlocked := acct(map[string]*Usage{"claude": {BlockedUntil: t0.Unix() + 3600}})
	cases := []struct {
		name string
		a    Account
		cur  string
		ph   *Phase
	}{
		{"to codex but claude free", free, "claude", &Phase{To: "codex", Started: t0.Unix()}},
		{"to claude but claude blocked", claudeBlocked, "pi", &Phase{To: "claude", Started: t0.Unix()}},
		{"to pi but codex desired", claudeBlocked, "claude", &Phase{To: "pi", Started: t0.Unix()}},
	}
	for _, c := range cases {
		for _, s := range []Session{{Idle: true}, {Idle: false}, {Idle: true, RateLimited: true}} {
			for _, touched := range []bool{false, true} {
				if got := Decide(t0, cfgT(), order3, c.a, s, c.cur, c.ph, touched); got != Cancel {
					t.Errorf("%s (session %+v touched=%v): got %v want Cancel", c.name, s, touched, got)
				}
			}
		}
	}
	if got := Decide(t0, cfgT(), order3, claudeBlocked, Session{Idle: true}, "claude", &Phase{To: "codex", Started: t0.Unix()}, false); got == Cancel {
		t.Fatal("matching phase must not be cancelled")
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
		ph   *Phase
		want []string
	}{
		{"normal", usage(72), "claude", nil, []string{"72% (5h)"}},
		{"warning band", usage(91), "claude", nil, []string{"bg=yellow", "DROPPING YOU IN 4% OF USAGE"}},
		{"below band", usage(89), "claude", nil, []string{"89% (5h)"}},
		{"handoff down", usage(95), "claude", &Phase{To: "codex"}, []string{"bg=red", "SWITCHING claude → codex"}},
		{"handoff up", Account{}, "codex", &Phase{To: "claude"}, []string{"bg=cyan", "claude"}},
		{"not first", blocked, "codex", nil, []string{"bg=blue", "codex · back to claude at " + time.Unix(back, 0).Format("Mon 15:04")}},
	}
	for _, tc := range cases {
		got := StatusText(t0, c, order3, tc.a, tc.cur, tc.ph)
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %q lacks %q", tc.name, got, w)
			}
		}
	}
	if got := StatusText(t0, c, order3, usage(89), "claude", nil); strings.Contains(got, "bg=yellow") {
		t.Errorf("89%% must be below the yellow band: %q", got)
	}
	// "back to" names the first harness above cur that is blocked.
	a := acct(map[string]*Usage{"claude": {BlockedUntil: back}, "codex": {BlockedUntil: back + 60}})
	if got := StatusText(t0, c, order3, a, "pi", nil); !strings.Contains(got, "back to claude at") {
		t.Errorf("pi status: %q", got)
	}
}
