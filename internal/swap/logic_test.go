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
	return c
}

func TestPeakIgnoresExpiredWindows(t *testing.T) {
	a := Account{
		FiveHour: &Window{UsedPct: 99, ResetsAt: t0.Unix() - 1},
		SevenDay: &Window{UsedPct: 40, ResetsAt: t0.Unix() + 3600},
	}
	pct, name, _ := a.Peak(t0)
	if pct != 40 || name != "7d" {
		t.Fatalf("got %v %v", pct, name)
	}
}

func TestTriggerAtThresholdSetsAPIUntil(t *testing.T) {
	a := Account{FiveHour: &Window{UsedPct: 94.9, ResetsAt: t0.Unix() + 600}}
	if Trigger(&a, t0, cfgT(), false) {
		t.Fatal("94.9 must not trigger")
	}
	a.FiveHour.UsedPct = 95
	if !Trigger(&a, t0, cfgT(), false) {
		t.Fatal("95 must trigger")
	}
	if a.APIUntil != t0.Unix()+600+600 || a.ResetsAt != t0.Unix()+600 {
		t.Fatalf("api_until %d resets %d", a.APIUntil, a.ResetsAt)
	}
	if a.Desired(t0) != API || a.Desired(time.Unix(a.APIUntil, 0)) != Sub {
		t.Fatal("desired mode wrong")
	}
	if Trigger(&a, t0, cfgT(), false) {
		t.Fatal("already in API period: no re-trigger")
	}
}

func TestTriggerUsesLatestResetAmongHotWindows(t *testing.T) {
	a := Account{
		FiveHour: &Window{UsedPct: 96, ResetsAt: t0.Unix() + 100},
		SevenDay: &Window{UsedPct: 97, ResetsAt: t0.Unix() + 90000},
	}
	Trigger(&a, t0, cfgT(), false)
	if a.ResetsAt != t0.Unix()+90000 {
		t.Fatalf("want 7d reset, got %d", a.ResetsAt)
	}
}

func TestTriggerOnRateLimitWithoutData(t *testing.T) {
	a := Account{}
	if !Trigger(&a, t0, cfgT(), true) {
		t.Fatal("rate limit must trigger")
	}
	if a.ResetsAt != t0.Add(5*time.Hour).Unix() {
		t.Fatalf("fallback reset: %d", a.ResetsAt)
	}
}

func TestDecideStartsHandoffWhenModesDiffer(t *testing.T) {
	a := Account{APIUntil: t0.Unix() + 100}
	if got := Decide(t0, cfgT(), a, Session{Idle: true}, Sub, nil, false); got != StartHandoff {
		t.Fatalf("got %v", got)
	}
	if got := Decide(t0, cfgT(), a, Session{}, API, nil, false); got != Noop {
		t.Fatalf("got %v", got)
	}
}

func TestDecideSkipsMessageWhenAlreadyRateLimited(t *testing.T) {
	a := Account{APIUntil: t0.Unix() + 100}
	if got := Decide(t0, cfgT(), a, Session{Idle: true, RateLimited: true}, Sub, nil, false); got != Switch {
		t.Fatalf("got %v", got)
	}
}

func TestDecideToAPIReadiness(t *testing.T) {
	a := Account{APIUntil: t0.Unix() + 100, FiveHour: &Window{UsedPct: 97, ResetsAt: t0.Unix() + 50}}
	ph := &Phase{To: API, Started: t0.Unix()}
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
		for _, at := range []time.Time{t0, t0.Add(24 * time.Hour)} { // no timeout toward API
			if got := Decide(at, cfgT(), a, c.s, Sub, ph, c.file); got != c.want {
				t.Errorf("%s at %v: got %v", c.name, at.Sub(t0), got)
			}
		}
	}
	a.FiveHour.UsedPct = 100
	if got := Decide(t0, cfgT(), a, Session{Idle: true}, Sub, ph, false); got != Switch {
		t.Fatalf("100%% must switch, got %v", got)
	}
}

func TestDecideToSubTimeout(t *testing.T) {
	a := Account{}
	ph := &Phase{To: Sub, Started: t0.Unix()}
	if got := Decide(t0.Add(59*time.Minute), cfgT(), a, Session{Idle: true}, API, ph, false); got != Noop {
		t.Fatalf("before timeout: %v", got)
	}
	if got := Decide(t0.Add(time.Hour), cfgT(), a, Session{Idle: false}, API, ph, false); got != Noop {
		t.Fatalf("timeout but busy: %v", got)
	}
	if got := Decide(t0.Add(time.Hour), cfgT(), a, Session{Idle: true}, API, ph, false); got != Switch {
		t.Fatalf("timeout and idle: %v", got)
	}
	if got := Decide(t0, cfgT(), a, Session{Idle: true}, API, ph, true); got != Switch {
		t.Fatalf("touched: %v", got)
	}
}

func TestStatusText(t *testing.T) {
	c := cfgT()
	a := Account{FiveHour: &Window{UsedPct: 72, ResetsAt: t0.Unix() + 3600}}
	cases := []struct {
		pct  float64
		mode Mode
		ph   *Phase
		want string
	}{
		{72, Sub, nil, "SUB 72% (5h)"},
		{91, Sub, nil, "DROPPING YOU IN 4% OF USAGE"},
		{95, Sub, &Phase{To: API}, "ACCOUNT CYCLE IN PROGRESS"},
		{0, API, nil, "API (gateway)"},
	}
	for _, tc := range cases {
		a.FiveHour.UsedPct = tc.pct
		a.APIUntil = t0.Unix() + 7200
		if got := StatusText(t0, c, "gateway", a, tc.mode, tc.ph); !strings.Contains(got, tc.want) {
			t.Errorf("pct %v: %q lacks %q", tc.pct, got, tc.want)
		}
	}
}
