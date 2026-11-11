package swap

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//go:embed plugins/agent-swap-pi.ts
var piExtension []byte

// piAdapter runs pi with an extension that calls `agent-swap hook`.
type piAdapter struct{ noPoll }

func (piAdapter) Command(h Harness, native, prompt string, extra []string, rt Runtime) (Launch, error) {
	if err := checkID(native); err != nil {
		return Launch{}, err
	}
	ext := filepath.Join(rt.Dir, "agent-swap-pi.ts")
	if err := os.WriteFile(ext, piExtension, 0o600); err != nil {
		return Launch{}, err
	}
	args := []string{"-e", ext}
	if native != "" {
		session := native
		if path, err := sessionPath("pi", native, rt); err == nil {
			session = path // pi resolves a bare id only in its default session dir
		}
		args = append(args, "--session", session)
	}
	args = append(append(args, h.Args...), extra...)
	if prompt != "" {
		args = append(args, prompt)
	}
	return Launch{Bin: binOr(h, "pi"), Args: args}, nil
}

func (piAdapter) Quit() string { return "/quit" }

type piLine struct {
	Type, ID string
	Parent   string `json:"parentId"`
	Message  struct {
		Role, StopReason, ToolCallID, ToolName string
		Content                                json.RawMessage
	}
}

type piBlock struct {
	Type, Text, ID, Name string
	Arguments            json.RawMessage
}

func (l *piLine) blocks() []piBlock {
	var s string
	if json.Unmarshal(l.Message.Content, &s) == nil {
		return []piBlock{{Type: "text", Text: s}}
	}
	var b []piBlock
	_ = json.Unmarshal(l.Message.Content, &b)
	return b
}

func piParse(path string) ([]piLine, []entry, int, error) {
	raw, size, err := readJSONL(path)
	var ls []piLine
	var es []entry
	for _, r := range raw {
		var l piLine
		if json.Unmarshal(r.raw, &l) != nil || l.Type == "session" {
			continue
		}
		role, stop := l.Message.Role, l.Message.StopReason
		failed := stop == "error" || stop == "aborted" // pi drops these from the context
		ls = append(ls, l)
		es = append(es, entry{id: l.ID, parent: l.Parent, start: r.start, end: r.end, leaf: l.ID != "",
			content: l.Type == "message" && (role == "user" || role == "toolResult" || role == "assistant" && !failed),
			done:    l.Type == "message" && role == "assistant" && stop != "toolUse"})
	}
	return ls, es, size, err
}

// Turns reads the active parentId chain's completed turns after the byte offset cursor.
func (piAdapter) Turns(native, cursor string, rt Runtime) ([]Turn, string, error) {
	path, err := sessionPath("pi", native, rt)
	if err != nil {
		return nil, cursor, err
	}
	ls, es, size, err := piParse(path)
	if err != nil {
		return nil, cursor, err
	}
	return readTurns("pi", es, size, cursor, rt.Final, func(i int, b *turnBuilder) {
		l := &ls[i]
		if l.Message.Role == "toolResult" {
			b.result(l.Message.ToolCallID, rawText(l.Message.Content))
			return
		}
		for _, c := range l.blocks() {
			switch c.Type {
			case "text":
				b.text(l.Message.Role, c.Text)
			case "toolCall":
				b.call(c.ID, c.Name, rawText(c.Arguments))
			}
		}
	})
}

// Write creates a session file holding turns (m.Native == "") or appends them to the owned session m.
func (piAdapter) Write(h Harness, m *Mirror, turns []Turn, rt Runtime) (int, error) {
	root, create := storeRoot("pi", rt), m.Native == ""
	now := time.Now().UTC()
	stamp := func() string { return now.Format("2006-01-02T15:04:05.000Z") }
	var buf bytes.Buffer
	put := func(v any) {
		b, _ := json.Marshal(v) // maps of strings and numbers
		buf.Write(append(b, '\n'))
	}
	native, path, parent, tail := m.Native, "", "", ""
	if create {
		native = uuid7()
		path = filepath.Join(root, piSlug(rt.Cwd), strings.NewReplacer(":", "-", ".", "-").Replace(stamp())+"_"+native+".jsonl")
		put(map[string]any{"type": "session", "version": 3, "id": native, "timestamp": stamp(), "cwd": rt.Cwd})
	} else {
		var err error
		if path, err = sessionPath("pi", native, rt); err != nil {
			return 0, err
		}
		ls, es, _, err := piParse(path)
		if err != nil {
			return 0, err
		}
		on := chain(es)
		for i, e := range es {
			if on[e.id] {
				parent = e.id
				if e.content && ls[i].Message.Role == "assistant" {
					tail = "assistant"
				} else if e.content {
					tail = "user" // user or toolResult
				}
			}
		}
	}
	turns = writable(turns, tail)
	for _, t := range turns {
		msg := map[string]any{"role": t.Role, "content": []map[string]any{{"type": "text", "text": t.Text}},
			"timestamp": now.UnixMilli()}
		if t.Role == "assistant" {
			msg["api"], msg["provider"], msg["model"], msg["stopReason"] = "anthropic-messages", "agent-swap", "import", "stop"
			msg["usage"] = map[string]any{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0, "totalTokens": 0,
				"cost": map[string]int{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0, "total": 0}}
		}
		id := hex8()
		e := map[string]any{"type": "message", "id": id, "parentId": nil, "timestamp": stamp(), "message": msg}
		if parent != "" {
			e["parentId"] = parent
		}
		put(e)
		parent, now = id, now.Add(time.Millisecond)
	}
	if !create {
		m.Path = path
		return readable(turns), appendOwned(m, buf.Bytes())
	}
	dev, ino, err := createExcl(root, path, buf.Bytes())
	if err != nil {
		return 0, err
	}
	m.Native, m.Path, m.Cursor, m.Dev, m.Ino = native, path, strconv.Itoa(buf.Len()), dev, ino
	return readable(turns), nil
}
