package swap

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// codexAdapter runs the Codex CLI. It has no hooks agent-swap can use, so Poll
// reads the session's rollout file.
type codexAdapter struct{}

func (codexAdapter) Command(h Harness, native, prompt string, extra []string, rt Runtime) (Launch, error) {
	if err := checkID(native); err != nil {
		return Launch{}, err
	}
	var args []string
	if native != "" {
		args = []string{"resume", native}
	}
	args = append(append(args, h.Args...), extra...)
	if prompt != "" {
		args = append(args, prompt)
	}
	return Launch{Bin: binOr(h, "codex"), Args: args}, nil
}

func (codexAdapter) Quit() string { return "/quit" }

func binOr(h Harness, def string) string {
	if h.Bin != "" {
		return h.Bin
	}
	return def
}

func codexSessions(rt Runtime) string {
	if d := os.Getenv("CODEX_HOME"); d != "" {
		return filepath.Join(d, "sessions")
	}
	return filepath.Join(rt.Home, ".codex", "sessions")
}

// codexRollout finds the rollout file of session id.
func codexRollout(id string, rt Runtime) (string, error) {
	if !safeID(id) {
		return "", fmt.Errorf("bad codex session id %q", id)
	}
	m, _ := filepath.Glob(filepath.Join(codexSessions(rt), "*", "*", "*", "rollout-*-"+id+".jsonl"))
	if len(m) == 0 {
		return "", fmt.Errorf("codex rollout for %s not found", id)
	}
	return m[len(m)-1], nil
}

// codexDiscover returns the id of the newest interactive session started in
// rt.Cwd since launch (`codex exec` runs share the sessions dir).
func codexDiscover(rt Runtime) string {
	m, _ := filepath.Glob(filepath.Join(codexSessions(rt), "*", "*", "*", "rollout-*.jsonl"))
	for i := len(m) - 1; i >= 0 && i >= len(m)-300; i-- {
		f, err := os.Open(m[i])
		if err != nil {
			continue
		}
		line, _ := bufio.NewReader(f).ReadBytes('\n')
		f.Close()
		var r struct {
			Type    string
			Payload struct {
				ID, Cwd, Originator string
				Timestamp           time.Time
				Source              json.RawMessage
			}
		}
		p := &r.Payload
		if json.Unmarshal(line, &r) == nil && r.Type == "session_meta" && p.Cwd == rt.Cwd &&
			!p.Timestamp.Before(rt.Launched.Add(-5*time.Second)) &&
			p.Originator != "codex_exec" && string(p.Source) != `"exec"` {
			return p.ID
		}
	}
	return ""
}

type codexLine struct {
	Type    string
	Payload struct {
		Type, Role, Name, Message string
		Content                   json.RawMessage
		Input                     json.RawMessage `json:"input"`
		Arguments                 json.RawMessage `json:"arguments"`
		RateLimits                *struct {
			Primary, Secondary *codexWindow
			Credits            *struct {
				HasCredits bool `json:"has_credits"`
				Unlimited  bool
			}
			ReachedType json.RawMessage `json:"rate_limit_reached_type"`
		} `json:"rate_limits"`
	}
}

type codexWindow struct {
	UsedPercent float64 `json:"used_percent"`
	ResetsAt    int64   `json:"resets_at"`
}

func (codexAdapter) Poll(h Harness, rt Runtime, s *Session, u *Usage) error {
	if s.NativeID == "" {
		if s.NativeID = codexDiscover(rt); s.NativeID == "" {
			return nil // not started yet
		}
	}
	path, err := codexRollout(s.NativeID, rt)
	if err != nil {
		return err
	}
	idle, limitErr := true, false
	var last *codexLine
	err = eachLine(path, func(l []byte) {
		var r codexLine
		if !bytes.Contains(l, []byte(`"type":"event_msg"`)) || json.Unmarshal(l, &r) != nil || r.Type != "event_msg" {
			return
		}
		switch r.Payload.Type {
		case "task_started":
			idle, limitErr = false, false
		case "task_complete", "turn_aborted":
			idle = true
		case "error":
			limitErr = limitErr || IsLimitError(r.Payload.Message)
		case "token_count":
			if r.Payload.RateLimits != nil {
				last = &r
			}
		}
	})
	s.Idle, s.RateLimited = idle, limitErr
	if last == nil {
		return err
	}
	rl := last.Payload.RateLimits
	if len(rl.ReachedType) > 0 && string(rl.ReachedType) != "null" {
		s.RateLimited = true
	}
	if c := rl.Credits; u == nil || c != nil && (c.HasCredits || c.Unlimited) {
		return err // still served from credits: the windows don't limit it
	}
	u.Windows = map[string]*Window{}
	now := time.Now().Unix()
	for name, w := range map[string]*codexWindow{"primary": rl.Primary, "secondary": rl.Secondary} {
		if w != nil {
			u.Windows[name] = &Window{UsedPct: w.UsedPercent, ResetsAt: w.ResetsAt}
			s.RateLimited = s.RateLimited || (w.ResetsAt > now && w.UsedPercent >= 100)
		}
	}
	return err
}

func (codexAdapter) Transcript(native string, rt Runtime) (string, error) {
	path, err := codexRollout(native, rt)
	if err != nil {
		return "", err
	}
	var t transcript
	err = eachLine(path, func(l []byte) {
		var r codexLine
		if json.Unmarshal(l, &r) != nil || r.Type != "response_item" {
			return
		}
		switch p := r.Payload; p.Type {
		case "message":
			if p.Role == "user" || p.Role == "assistant" {
				t.content(p.Role, p.Content)
			}
		case "custom_tool_call":
			t.tool(p.Name, p.Input)
		case "function_call":
			t.tool(p.Name, p.Arguments)
		}
	})
	return t.String(), err
}
