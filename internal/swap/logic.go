package swap

import (
	"fmt"
	"slices"
	"time"
)

type Action int

const (
	Noop Action = iota
	Switch
)

func (a Action) String() string { return [...]string{"Noop", "Switch"}[a] }

// Peak returns the highest usage among windows that have not reset yet.
func (u *Usage) Peak(now time.Time) (pct float64, name string, resetsAt int64) {
	if u == nil {
		return
	}
	for n, w := range u.Windows {
		if w != nil && w.ResetsAt > now.Unix() && (w.UsedPct > pct || w.UsedPct == pct && (name == "" || n < name)) {
			pct, name, resetsAt = w.UsedPct, n, w.ResetsAt
		}
	}
	return
}

func rank(order []Harness, name string) int {
	return slices.IndexFunc(order, func(h Harness) bool { return h.Name == name })
}

func blockedUntil(a Account, name string) int64 {
	if u := a.Harnesses[name]; u != nil {
		return u.BlockedUntil
	}
	return 0
}

// Desired is the harness every wrapped session should run: the first one
// not blocked, or, when all are, the one that frees up first.
func Desired(a Account, order []Harness, now time.Time) string {
	best := ""
	for _, h := range order {
		b := blockedUntil(a, h.Name)
		if b <= now.Unix() {
			return h.Name
		}
		if best == "" || b < blockedUntil(a, best) {
			best = h.Name
		}
	}
	return best
}

func orDefault[T comparable](v, def T) T {
	var zero T
	if v == zero {
		return def
	}
	return v
}

// Trigger blocks harness h when any live window reaches its threshold (or
// the session hit a limit error). The block ends at the latest reset among
// the hot windows plus the switch-back delay; a limit error without any
// usage window blocks for the cooldown.
func Trigger(a *Account, h Harness, cfg Config, now time.Time, rateLimited bool) bool {
	if blockedUntil(*a, h.Name) > now.Unix() {
		return false
	}
	u := a.Get(h.Name)
	var reset int64
	for _, w := range u.Windows {
		if w != nil && w.ResetsAt > now.Unix() && w.UsedPct >= orDefault(h.Threshold, cfg.Threshold) && w.ResetsAt > reset {
			reset = w.ResetsAt
		}
	}
	if reset == 0 && !rateLimited {
		return false
	}
	if reset == 0 { // limited below threshold: the fullest window is the culprit
		_, _, reset = u.Peak(now)
	}
	if reset == 0 { // no window: wait out the cooldown
		u.BlockedUntil = now.Add(orDefault(h.Cooldown, cfg.Cooldown).Duration).Unix()
		u.ResetsAt = u.BlockedUntil
		return true
	}
	u.ResetsAt = reset
	u.BlockedUntil = reset + int64(cfg.SwitchBackDelay.Seconds())
	return true
}

// Want is the harness this session should run: the pin if set, else Desired.
func Want(a Account, order []Harness, now time.Time, pin string) string {
	if pin != "" && rank(order, pin) >= 0 {
		return pin
	}
	return Desired(a, order, now)
}

// Decide switches when the wanted harness differs from the running one, and
// only at idle so a turn is never cut off.
func Decide(now time.Time, order []Harness, a Account, s Session, cur, pin string) Action {
	if Want(a, order, now, pin) != cur && s.Idle {
		return Switch
	}
	return Noop
}

// StatusText is the tmux status-right content, with tmux color markup.
func StatusText(now time.Time, cfg Config, order []Harness, a Account, cur, pin string) string {
	clock := func(unix int64) string { return time.Unix(unix, 0).Format("Mon 15:04") }
	if w := Want(a, order, now, pin); w != cur {
		return fmt.Sprintf("#[fg=white,bg=red,bold] %s → %s: switching at the next idle #[default]", cur, w)
	}
	pinned := ""
	if pin != "" {
		pinned = " · pinned to " + pin
	}
	for _, h := range order[:max(rank(order, cur), 0)] {
		if b := blockedUntil(a, h.Name); b > now.Unix() && pin == "" {
			return fmt.Sprintf("#[fg=white,bg=blue] %s · back to %s at %s #[default]", cur, h.Name, clock(b))
		}
	}
	threshold := cfg.Threshold
	if i := rank(order, cur); i >= 0 {
		threshold = orDefault(order[i].Threshold, cfg.Threshold)
	}
	pct, name, reset := a.Harnesses[cur].Peak(now)
	if name == "" {
		return cur + " · usage unknown" + pinned
	}
	if pct >= threshold-5 {
		return fmt.Sprintf("#[fg=black,bg=yellow,bold] %s %.0f%% (%s): switch at %.0f%%, %.0f%% left%s #[default]",
			cur, pct, name, threshold, max(threshold-pct, 0), pinned)
	}
	return fmt.Sprintf("%s %.0f%% (%s) · resets %s%s", cur, pct, name, clock(reset), pinned)
}
