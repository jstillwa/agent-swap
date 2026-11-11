package swap

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// Window is one rate-limit window (claude: "5h", "7d"; codex: "primary", "secondary").
type Window struct {
	UsedPct  float64 `json:"used_percentage"`
	ResetsAt int64   `json:"resets_at"` // unix seconds
}

// Usage is one harness's limit state.
type Usage struct {
	Windows      map[string]*Window `json:"windows,omitempty"`
	BlockedUntil int64              `json:"blocked_until,omitempty"` // harness unavailable while now < BlockedUntil
	ResetsAt     int64              `json:"resets_at,omitempty"`     // reset that caused the block
	UpdatedAt    int64              `json:"updated_at,omitempty"`
}

// Account is shared by every wrapped session (usage is account-wide), keyed
// by harness name.
type Account struct {
	Harnesses map[string]*Usage `json:"harnesses"`
}

// Get returns the usage entry for a harness, creating it.
func (a *Account) Get(name string) *Usage {
	if a.Harnesses == nil {
		a.Harnesses = map[string]*Usage{}
	}
	if a.Harnesses[name] == nil {
		a.Harnesses[name] = &Usage{}
	}
	return a.Harnesses[name]
}

// Session is per supervised harness process, written by hooks and Poll.
type Session struct {
	NativeID    string `json:"native_id"` // the harness's own session/conversation id
	Idle        bool   `json:"idle"`
	RateLimited bool   `json:"rate_limited"`
	Failed      bool   `json:"failed,omitempty"` // the last turn ended in an error (StopFailure)
	Turns       int    `json:"turns"`            // completed turns (Stop/StopFailure) since launch
	StartedAt   int64  `json:"started_at"`       // last SessionStart hook
	UpdatedAt   int64  `json:"updated_at"`
}

// Request is what `agent-swap use` and `agent-swap sync` ask of a running
// session (request.json in its dir). The runner validates it on every read.
type Request struct {
	Pin  string   `json:"pin,omitempty"`  // harness name to run regardless of usage ("" = auto)
	Sync []string `json:"sync,omitempty"` // harness names whose sessions to bring up to date ("*" = all)
}

// MirrorStatus is one line of mirrors.json, written by the runner for
// `status` only; it is never read back.
type MirrorStatus struct {
	Native string `json:"native"`
	Path   string `json:"path,omitempty"`
	Behind int    `json:"behind"` // log turns from other harnesses not yet written into it
}

// Load reads JSON at path into a T; a missing file yields the zero value.
func Load[T any](path string) (T, error) {
	var v T
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return v, nil
	}
	if err != nil {
		return v, err
	}
	return v, json.Unmarshal(b, &v)
}

// Update applies fn to the JSON at path under an exclusive flock and writes
// it back atomically (temp file + rename), so concurrent hooks and sessions
// never interleave or tear a write.
func Update[T any](path string, fn func(*T)) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	v, err := Load[T](path)
	if err != nil {
		return err
	}
	fn(&v)
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*") // O_EXCL, 0600
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after the rename
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
