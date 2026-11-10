package swap

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// Window is one subscription rate-limit window from the statusline JSON.
type Window struct {
	UsedPct  float64 `json:"used_percentage"`
	ResetsAt int64   `json:"resets_at"` // unix seconds
}

// Account is shared by every wrapped session (usage is account-wide).
type Account struct {
	FiveHour  *Window `json:"five_hour,omitempty"`
	SevenDay  *Window `json:"seven_day,omitempty"`
	UpdatedAt int64   `json:"updated_at"`
	APIUntil  int64   `json:"api_until"` // API billing desired while now < APIUntil
	ResetsAt  int64   `json:"resets_at"` // reset that caused the current API period
}

// Session is per wrapped Claude process, written by hooks and the statusline.
type Session struct {
	ClaudeSessionID string `json:"claude_session_id"`
	Idle            bool   `json:"idle"`
	RateLimited     bool   `json:"rate_limited"`
	StartedAt       int64  `json:"started_at"` // last SessionStart hook
	UpdatedAt       int64  `json:"updated_at"`
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
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
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
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
