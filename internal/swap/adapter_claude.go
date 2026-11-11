package swap

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// claudeAdapter runs Claude Code; its signals come from hooks and the statusline.
type claudeAdapter struct{ noPoll }

func (claudeAdapter) Command(h Harness, native, prompt string, extra []string, rt Runtime) (Launch, error) {
	if err := checkID(native); err != nil {
		return Launch{}, err
	}
	settings, err := SettingsJSON(rt.Exe, h.Target, rt.Cfg.Targets[h.Target])
	if err != nil {
		return Launch{}, err
	}
	path := filepath.Join(rt.Dir, h.Name+".settings.json")
	if err := os.WriteFile(path, settings, 0o600); err != nil {
		return Launch{}, err
	}
	var args []string
	if native != "" {
		args = []string{"--resume", native}
		extra = ReplayArgs(extra)
	}
	args = append(append(args, h.Args...), extra...)
	if prompt != "" {
		args = append(args, prompt)
	}
	return Launch{Bin: claudeBin(h, rt), Args: append(args, "--settings", path)}, nil
}

func claudeBin(h Harness, rt Runtime) string {
	if h.Bin != "" {
		return h.Bin
	}
	return rt.Cfg.ClaudeBin
}

func (claudeAdapter) Quit() string { return "/exit" }

type claudeLine struct {
	Type           string `json:"type"`
	UUID           string `json:"uuid"`
	Parent         string `json:"parentUuid"`
	LogicalParent  string `json:"logicalParentUuid"` // compact_boundary: the pre-compaction leaf
	IsSidechain    bool   `json:"isSidechain"`
	IsMeta         bool   `json:"isMeta"`
	IsSummary      bool   `json:"isCompactSummary"`
	IsAPIError     bool   `json:"isApiErrorMessage"`
	TranscriptOnly bool   `json:"isVisibleInTranscriptOnly"`
	Message        struct {
		ID         string          `json:"id"`
		Model      string          `json:"model"`
		StopReason string          `json:"stop_reason"`
		Content    json.RawMessage `json:"content"`
	} `json:"message"`
}

type claudeBlock struct {
	Type, Text, ID, Name string
	ToolUseID            string `json:"tool_use_id"`
	Input, Content       json.RawMessage
}

func (l *claudeLine) content() bool {
	return (l.Type == "user" || l.Type == "assistant") && !l.IsSidechain && !l.IsMeta && !l.IsSummary &&
		!l.IsAPIError && !l.TranscriptOnly && l.Message.Model != "<synthetic>"
}

func (l *claudeLine) blocks() []claudeBlock {
	var s string
	if json.Unmarshal(l.Message.Content, &s) == nil {
		return []claudeBlock{{Type: "text", Text: s}}
	}
	var b []claudeBlock
	_ = json.Unmarshal(l.Message.Content, &b)
	return b
}

// results reports whether a message holds only tool results.
func results(bs []claudeBlock) bool {
	return len(bs) > 0 && !slices.ContainsFunc(bs, func(b claudeBlock) bool { return b.Type != "tool_result" })
}

// claudeParse turns the complete lines of a session file into chain entries.
func claudeParse(path string) ([]claudeLine, []entry, int, error) {
	raw, size, err := readJSONL(path)
	var ls []claudeLine
	var es []entry
	for _, r := range raw {
		var l claudeLine
		if json.Unmarshal(r.raw, &l) != nil {
			continue
		}
		parent := l.Parent
		if parent == "" {
			parent = l.LogicalParent
		}
		ls = append(ls, l)
		es = append(es, entry{id: l.UUID, parent: parent, start: r.start, end: r.end,
			leaf: l.UUID != "" && !l.IsSidechain && l.Type != "progress", content: l.content(),
			result: l.Type == "user" && l.content() && results(l.blocks()),
			// a turn ends with a final answer or a failed API call (StopFailure)
			done: l.Type == "assistant" && !l.IsSidechain && (l.IsAPIError || l.content() &&
				l.Message.StopReason != "" && l.Message.StopReason != "tool_use" && l.Message.StopReason != "pause_turn")})
	}
	return ls, es, size, err
}

// Turns reads the active chain's completed turns after the byte offset cursor.
func (claudeAdapter) Turns(native, cursor string, rt Runtime) ([]Turn, string, error) {
	path, err := sessionPath("claude", native, rt)
	if err != nil {
		return nil, cursor, err
	}
	ls, es, size, err := claudeParse(path)
	if err != nil {
		return nil, cursor, err
	}
	return readTurns("claude", es, size, cursor, rt.Final, func(i int, b *turnBuilder) {
		l := &ls[i]
		for _, c := range l.blocks() {
			switch c.Type {
			case "text":
				b.text(l.Type, c.Text)
			case "tool_use":
				b.call(c.ID, c.Name, rawText(c.Input))
			case "tool_result":
				b.result(c.ToolUseID, rawText(c.Content))
			}
		}
	})
}

// Write creates a session holding turns (m.Native == "") or appends them to the owned session m.
func (claudeAdapter) Write(h Harness, m *Mirror, turns []Turn, rt Runtime) (int, error) {
	root, create := storeRoot("claude", rt), m.Native == ""
	native, parent, tail := m.Native, "", ""
	var dangling []string
	if create {
		native = uuid4()
	} else {
		path, err := sessionPath("claude", native, rt)
		if err != nil {
			return 0, err
		}
		ls, es, _, err := claudeParse(path)
		if err != nil {
			return 0, err
		}
		m.Path = path
		parent, tail, dangling = claudeTail(ls, es)
	}
	base := map[string]any{"sessionId": native, "cwd": rt.Cwd, "version": binVersion(claudeBin(h, rt)),
		"gitBranch": "", "isSidechain": false, "userType": "external", "entrypoint": "cli"}
	var buf bytes.Buffer
	now := time.Now().UTC()
	put := func(typ string, msg map[string]any) {
		id := uuid4()
		l := map[string]any{"type": typ, "uuid": id, "parentUuid": nil, "message": msg,
			"timestamp": now.Format("2006-01-02T15:04:05.000Z")}
		if parent != "" {
			l["parentUuid"] = parent
		}
		for k, v := range base {
			l[k] = v
		}
		b, _ := json.Marshal(l) // maps of strings and numbers
		buf.Write(append(b, '\n'))
		parent, now = id, now.Add(time.Millisecond)
	}
	if len(dangling) > 0 { // an unanswered tool_use makes the API reject the conversation
		var res []map[string]any
		for _, id := range dangling {
			res = append(res, map[string]any{"type": "tool_result", "tool_use_id": id,
				"content": "interrupted by agent-swap", "is_error": true})
		}
		put("user", map[string]any{"role": "user", "content": res})
	}
	turns = writable(turns, tail)
	for _, t := range turns {
		msg := map[string]any{"role": t.Role, "content": []map[string]any{{"type": "text", "text": t.Text}}}
		if t.Role == "assistant" {
			msg["id"], msg["type"], msg["model"] = "msg_"+strings.ReplaceAll(uuid4(), "-", ""), "message", "agent-swap-import"
			msg["stop_reason"], msg["stop_sequence"] = "end_turn", nil
			msg["usage"] = map[string]int{"input_tokens": 0, "output_tokens": 0,
				"cache_creation_input_tokens": 0, "cache_read_input_tokens": 0}
		}
		put(t.Role, msg)
	}
	if !create {
		return readable(turns), appendOwned(m, buf.Bytes())
	}
	path := filepath.Join(root, "projects", claudeSlug(rt.Cwd), native+".jsonl")
	dev, ino, err := createExcl(root, path, buf.Bytes())
	if err != nil {
		return 0, err
	}
	m.Native, m.Path, m.Cursor, m.Dev, m.Ino = native, path, strconv.Itoa(buf.Len()), dev, ino
	return readable(turns), nil
}

// claudeTail finds where to append: the last chain entry (never an API error),
// the role of the last message, and the last assistant message's tool_use ids
// that no tool_result line anywhere in the file answers (parallel results hang
// off their own call, not the chain).
func claudeTail(ls []claudeLine, es []entry) (parent, tail string, dangling []string) {
	on, msgID, answered := chain(es), "", map[string]bool{}
	for i := range ls {
		for _, c := range ls[i].blocks() {
			if c.Type == "tool_result" {
				answered[c.ToolUseID] = true
			}
		}
	}
	for i := range ls {
		l := &ls[i]
		if !on[l.UUID] {
			continue
		}
		if !l.IsAPIError {
			parent = l.UUID // often an attachment line after the assistant
		}
		if !l.content() {
			continue
		}
		if tail = l.Type; tail == "assistant" && l.Message.ID != msgID {
			msgID, dangling = l.Message.ID, nil
		}
		for _, c := range l.blocks() {
			if c.Type == "tool_use" && tail == "assistant" && !answered[c.ID] {
				dangling = append(dangling, c.ID)
			}
		}
	}
	return parent, tail, dangling
}
