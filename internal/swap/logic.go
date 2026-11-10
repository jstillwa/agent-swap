package swap

import (
	"fmt"
	"time"
)

type Mode string

const (
	Sub Mode = "SUB"
	API Mode = "API"
)

type Action int

const (
	Noop Action = iota
	StartHandoff
	Switch
	Cancel
)

func (a Action) String() string { return [...]string{"Noop", "StartHandoff", "Switch", "Cancel"}[a] }

// Phase is an in-progress handoff for one session.
type Phase struct {
	To      Mode
	File    string // touch file Claude creates when ready
	Started int64
}

// Peak returns the highest usage among windows that have not reset yet.
func (a Account) Peak(now time.Time) (pct float64, name string, resetsAt int64) {
	for _, w := range []struct {
		n string
		w *Window
	}{{"5h", a.FiveHour}, {"7d", a.SevenDay}} {
		if w.w != nil && w.w.ResetsAt > now.Unix() && w.w.UsedPct >= pct {
			pct, name, resetsAt = w.w.UsedPct, w.n, w.w.ResetsAt
		}
	}
	return
}

// Desired is the billing mode every wrapped session should be on.
func (a Account) Desired(now time.Time) Mode {
	if now.Unix() < a.APIUntil {
		return API
	}
	return Sub
}

// Trigger starts an account-wide API period when any live window reaches the
// threshold (or a session hit a rate-limit error). The period ends at the
// latest reset among the hot windows plus the switch-back delay.
func Trigger(a *Account, now time.Time, cfg Config, rateLimited bool) bool {
	if a.Desired(now) == API {
		return false
	}
	var reset int64
	for _, w := range []*Window{a.FiveHour, a.SevenDay} {
		if w == nil || w.ResetsAt <= now.Unix() {
			continue
		}
		if w.UsedPct >= cfg.Threshold && w.ResetsAt > reset {
			reset = w.ResetsAt
		}
	}
	if reset == 0 && !rateLimited {
		return false
	}
	if reset == 0 { // limited below threshold: the fullest window is the culprit
		_, _, reset = a.Peak(now)
	}
	if reset == 0 { // no usable reset time: assume a 5-hour window
		reset = now.Add(5 * time.Hour).Unix()
	}
	a.ResetsAt = reset
	a.APIUntil = reset + int64(cfg.SwitchBackDelay.Seconds())
	return true
}

// Decide is the per-session state machine. A switch always waits for an idle
// session so a turn is never cut off. A handoff the desired mode no longer
// matches is cancelled at once.
func Decide(now time.Time, cfg Config, a Account, s Session, cur Mode, ph *Phase, touched bool) Action {
	if ph == nil {
		switch {
		case a.Desired(now) == cur:
			return Noop
		case a.Desired(now) == API && s.RateLimited && s.Idle:
			return Switch // already blocked: nothing to wrap up
		default:
			return StartHandoff
		}
	}
	if ph.To != a.Desired(now) {
		return Cancel
	}
	if !s.Idle {
		return Noop
	}
	ready := touched
	if ph.To == API {
		pct, _, _ := a.Peak(now)
		ready = ready || s.RateLimited || pct >= 100
	} else {
		ready = ready || now.Unix()-ph.Started >= int64(cfg.SwitchBackTimeout.Seconds())
	}
	if ready {
		return Switch
	}
	return Noop
}

// StatusText is the tmux status-right content, with tmux color markup.
func StatusText(now time.Time, cfg Config, target string, a Account, mode Mode, ph *Phase) string {
	clock := func(unix int64) string { return time.Unix(unix, 0).Format("Mon 15:04") }
	switch {
	case ph != nil && ph.To == API:
		return "#[fg=white,bg=red,bold] 🔴 ACCOUNT CYCLE IN PROGRESS — waiting for handoff #[default]"
	case ph != nil:
		return "#[fg=black,bg=cyan,bold] ACCOUNT CYCLE: returning to SUB — waiting for handoff #[default]"
	case mode == API:
		return fmt.Sprintf("#[fg=white,bg=blue] API (%s) · back to SUB at %s #[default]", target, clock(a.APIUntil))
	}
	pct, name, reset := a.Peak(now)
	if name == "" {
		return "SUB · usage unknown"
	}
	if pct >= cfg.Threshold-5 {
		return fmt.Sprintf("#[fg=black,bg=yellow,bold] ⚠ %.0f%% (%s) — TMUX DROPPING YOU IN %.0f%% OF USAGE. Prepare for account cycle! #[default]",
			pct, name, cfg.Threshold-pct)
	}
	return fmt.Sprintf("SUB %.0f%% (%s) · resets %s", pct, name, clock(reset))
}
