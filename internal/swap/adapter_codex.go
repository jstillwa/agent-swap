package swap

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
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

func codexSessions(rt Runtime) string { return filepath.Join(storeRoot("codex", rt), "sessions") }

// codexDiscover returns the id of the newest interactive session started in
// rt.Cwd since launch (`codex exec` runs share the sessions dir). Rollouts agent-swap created are skipped (codex is resumed into them by
// id), and so are sub-agent threads (source is an object, not "cli").
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
				ThreadSource        string `json:"thread_source"`
			}
		}
		p := &r.Payload
		if json.Unmarshal(line, &r) == nil && r.Type == "session_meta" && p.Cwd == rt.Cwd &&
			!p.Timestamp.Before(rt.Launched.Add(-5*time.Second)) &&
			p.Originator != "codex_exec" && p.Originator != "agent-swap" && string(p.Source) != `"exec"` &&
			!bytes.HasPrefix(p.Source, []byte("{")) && (p.ThreadSource == "" || p.ThreadSource == "user") {
			return p.ID
		}
	}
	return ""
}

type codexLine struct {
	Timestamp time.Time
	Type      string
	Payload   struct {
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
	// Discover only until the session is known, or when its rollout is gone:
	// another codex in this cwd must never replace it (so /new is not followed).
	if _, err := sessionPath("codex", s.NativeID, rt); err != nil {
		if id := codexDiscover(rt); id != "" {
			s.NativeID = id
		}
	}
	if s.NativeID == "" {
		return nil // not started yet
	}
	path, err := sessionPath("codex", s.NativeID, rt)
	if err != nil {
		return err
	}
	idle, limitErr, failed, turns := true, false, false, 0
	var last *codexLine
	err = eachLine(path, func(l []byte) {
		var r codexLine
		if !bytes.Contains(l, []byte(`"type":"event_msg"`)) || json.Unmarshal(l, &r) != nil || r.Type != "event_msg" {
			return
		}
		switch r.Payload.Type {
		case "task_started":
			idle, limitErr, failed = false, false, false
		case "task_complete", "turn_aborted":
			idle = true
			if !r.Timestamp.Before(rt.Launched) { // Turns counts since launch
				turns++
			}
		case "error":
			limitErr, failed = limitErr || IsLimitError(r.Payload.Message), true
		case "token_count":
			if r.Payload.RateLimits != nil {
				last = &r
			}
		}
	})
	s.Idle, s.RateLimited, s.Failed, s.Turns = idle, limitErr, failed, turns
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

// codexNoise starts user-role context codex injects itself.
var codexNoise = regexp.MustCompile(`^(<(environment_context|user_instructions|recommended_plugins)>|# AGENTS\.md instructions)`)

// codexText flattens a message content or tool output: a string, [{text}] or {content}.
func codexText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct{ Text string }
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Text != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	var o struct{ Content json.RawMessage }
	if json.Unmarshal(raw, &o) == nil && len(o.Content) > 0 {
		return codexText(o.Content)
	}
	return string(raw)
}

// Turns reads the rollout after byte offset cursor up to the last finished task
// (task_complete or turn_aborted), or to its end when rt.Final. Tool calls pair
// with their output by call_id.
func (codexAdapter) Turns(native, cursor string, rt Runtime) ([]Turn, string, error) {
	path, err := sessionPath("codex", native, rt)
	if err != nil {
		return nil, cursor, err
	}
	off, data, err := readFrom(path, cursor)
	if err != nil {
		return nil, cursor, err
	}
	b := &turnBuilder{kind: "codex", calls: map[string][2]string{}}
	var out []Turn
	at := 0
	for pos := 0; pos < len(data); {
		n := bytes.IndexByte(data[pos:], '\n') + 1
		l := data[pos : pos+n]
		pos += n
		var r struct {
			Type    string
			Payload struct {
				Type, Role, Name         string
				CallID                   string `json:"call_id"`
				Content, Output          json.RawMessage
				Input, Arguments, Action json.RawMessage
			}
		}
		if json.Unmarshal(l, &r) != nil {
			continue
		}
		p := r.Payload
		switch {
		case r.Type == "event_msg" && (p.Type == "task_complete" || p.Type == "turn_aborted"):
			out, b.turns, at = append(out, b.done()...), nil, pos
		case r.Type != "response_item":
		case p.Type == "message" && (p.Role == "user" || p.Role == "assistant"):
			if t := codexText(p.Content); !(p.Role == "user" && codexNoise.MatchString(strings.TrimSpace(t))) {
				b.flush() // a call's output always precedes the next message: still dangling = interrupted
				b.text(p.Role, t)
			}
		case strings.HasSuffix(p.Type, "_call_output"):
			b.result(p.CallID, codexText(p.Output))
		case strings.HasSuffix(p.Type, "_call"):
			in := p.Input
			if len(in) == 0 {
				in = p.Arguments
			}
			if len(in) == 0 {
				in = p.Action
			}
			name := p.Name
			if name == "" {
				name = strings.TrimSuffix(p.Type, "_call")
			}
			b.call(p.CallID, name, codexText(in))
		}
	}
	if rt.Final { // the child exited: keep the unfinished turn, read to the end
		out, at = append(out, b.done()...), len(data)
	}
	return out, strconv.FormatInt(off+int64(at), 10), nil
}

// readFrom returns the complete lines of path after byte offset cursor ("" = 0).
func readFrom(path, cursor string) (int64, []byte, error) {
	off, err := cursorOffset(cursor)
	if err != nil || off < 0 {
		return 0, nil, fmt.Errorf("bad cursor %q", cursor)
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, nil, err
	}
	defer f.Close()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return 0, nil, err
	}
	b, err := io.ReadAll(f)
	return off, b[:bytes.LastIndexByte(b, '\n')+1], err
}

// codexRecords renders turns importer-style: each user turn opens a task, which
// closes (task_complete) before the next user turn and at the end. ordinal < 0
// leaves the field out (rollouts written before codex numbered lines).
func codexRecords(turns []Turn, ordinal int) []byte {
	var buf bytes.Buffer
	emit := func(typ string, payload map[string]any) {
		rec := map[string]any{"timestamp": codexTime(time.Now()), "type": typ, "payload": payload}
		if ordinal >= 0 {
			rec["ordinal"], ordinal = ordinal, ordinal+1
		}
		b, _ := json.Marshal(rec)
		buf.Write(append(b, '\n'))
	}
	turnID, last := "", ""
	closeTask := func() {
		if turnID != "" {
			emit("event_msg", map[string]any{"type": "task_complete", "turn_id": turnID, "last_agent_message": last})
		}
	}
	for _, t := range turns {
		if t.Text == "" {
			continue
		}
		if t.Role == "user" {
			closeTask()
			turnID = ""
		}
		if turnID == "" {
			turnID, last = uuid7(), ""
			emit("event_msg", map[string]any{"type": "task_started", "turn_id": turnID})
		}
		if t.Role == "user" {
			emit("event_msg", map[string]any{"type": "user_message", "message": t.Text, "images": []string{}})
			emit("response_item", map[string]any{"type": "message", "role": "user",
				"content": []map[string]string{{"type": "input_text", "text": t.Text}}})
		} else {
			last = t.Text
			emit("event_msg", map[string]any{"type": "agent_message", "message": t.Text})
			emit("response_item", map[string]any{"type": "message", "role": "assistant",
				"content": []map[string]string{{"type": "output_text", "text": t.Text}}})
		}
	}
	closeTask()
	return buf.Bytes()
}

// Write creates an importer-style rollout or appends to the one agent-swap owns.
// It never touches codex's sqlite index: codex falls back to a file scan.
func (a codexAdapter) Write(h Harness, m *Mirror, turns []Turn, rt Runtime) (int, error) {
	var keep []Turn // only turns Turns reads back, so the count is exact
	for _, t := range turns {
		if textTurn("codex", t.Role, t.Text).Text != "" && !(t.Role == "user" && codexNoise.MatchString(strings.TrimSpace(t.Text))) {
			keep = append(keep, t)
		}
	}
	if err := a.write(h, m, keep, rt); err != nil {
		return 0, err
	}
	return len(keep), nil
}

func (codexAdapter) write(h Harness, m *Mirror, turns []Turn, rt Runtime) error {
	root := storeRoot("codex", rt)
	if m.Native != "" {
		p, err := sessionPath("codex", m.Native, rt) // rebuilt, never trusted from state
		if err != nil {
			return err
		}
		m.Path = p
		if err := checkOwned(root, m); err != nil {
			return err
		}
		ord, err := codexLastOrdinal(m.Path)
		if err != nil {
			return err
		}
		if ord >= 0 {
			ord++
		}
		return appendOwned(m, codexRecords(turns, ord))
	}
	id, now := uuid7(), time.Now()
	// No ordinals and no history_mode: codex keeps such a rollout in its legacy
	// (unnumbered) format when it appends to it.
	meta, _ := json.Marshal(map[string]any{"timestamp": codexTime(now), "type": "session_meta",
		"payload": map[string]any{"id": id, "timestamp": codexTime(now), "cwd": rt.Cwd, "originator": "agent-swap",
			"cli_version": binVersion(binOr(h, "codex")), "source": "cli", "model_provider": "openai"}})
	data := append(append(meta, '\n'), codexRecords(turns, -1)...)
	path := filepath.Join(root, "sessions", now.Format("2006/01/02"), "rollout-"+now.Format("2006-01-02T15-04-05")+"-"+id+".jsonl")
	dev, ino, err := createExcl(root, path, data)
	if err != nil {
		return err
	}
	m.Native, m.Path, m.Cursor, m.Dev, m.Ino = id, path, strconv.Itoa(len(data)), dev, ino
	return nil
}

func codexTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// codexLastOrdinal is the ordinal of the rollout's last line (-1 if none).
func codexLastOrdinal(path string) (int, error) {
	_, b, err := readFrom(path, "")
	if err != nil {
		return 0, err
	}
	b = bytes.TrimRight(b, "\n")
	var r struct{ Ordinal *int }
	if json.Unmarshal(b[bytes.LastIndexByte(b, '\n')+1:], &r) != nil || r.Ordinal == nil {
		return -1, nil
	}
	return *r.Ordinal, nil
}
