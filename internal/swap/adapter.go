package swap

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// Runtime is what an adapter needs to know about the running agent-swap session.
type Runtime struct {
	Exe, Dir, Cwd, StateDir, CfgPath, Home string // Home = user home (tests override)
	TmpDir                                 string // handoff dir holding the notes and ready files
	Cfg                                    Config
	Launched                               time.Time
}

// Launch is a child process to start in the pane.
type Launch struct {
	Bin  string
	Args []string
	Env  []string // extra KEY=VALUE appended after the base env
}

// Adapter drives one harness kind.
type Adapter interface {
	// Command builds the child process. native = the harness's own session id to resume ("" = fresh).
	// prompt = initial message ("" = none). extra = user pass-through args, appended after h.Args.
	Command(h Harness, native, prompt string, extra []string, rt Runtime) (Launch, error)
	Quit() string // typed to exit gracefully
	// Poll updates s (and u) for harnesses whose signals come from their own files. Hook-driven: no-op.
	Poll(h Harness, rt Runtime, s *Session, u *Usage) error
	// Transcript returns the native conversation as plain text, one block per turn:
	// "[user] ...", "[assistant] ...", "[tool] name: short input".
	Transcript(native string, rt Runtime) (string, error)
}

func AdapterFor(kind string) (Adapter, error) {
	switch kind {
	case "claude":
		return claudeAdapter{}, nil
	case "codex":
		return codexAdapter{}, nil
	case "pi":
		return piAdapter{}, nil
	case "opencode":
		return opencodeAdapter{}, nil
	case "agy":
		return agyAdapter{}, nil
	}
	return nil, fmt.Errorf("unknown harness kind %q", kind)
}

// TailBytes keeps the newest n bytes of s, cut at a line boundary.
func TailBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	t := s[len(s)-n:]
	if i := strings.IndexByte(t, '\n'); i >= 0 && s[len(s)-n-1] != '\n' { // drop a partial first line
		t = t[i+1:]
	}
	return "[…earlier conversation truncated…]\n" + strings.ToValidUTF8(t, "")
}

var idChars = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// safeID reports whether a harness session id is safe to put in a path or argv.
func safeID(id string) bool { return idChars.MatchString(id) && !strings.Contains(id, "..") }

// checkID rejects an unsafe native id; "" (fresh session) is allowed.
func checkID(native string) error {
	if native != "" && !safeID(native) {
		return fmt.Errorf("unsafe session id %q", native)
	}
	return nil
}

// noPoll is embedded by hook-driven adapters.
type noPoll struct{}

func (noPoll) Poll(Harness, Runtime, *Session, *Usage) error { return nil }

// eachLine calls fn for every non-empty line of a JSONL file.
func eachLine(path string, fn func([]byte)) error {
	b, err := os.ReadFile(path)
	for _, l := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(l)) > 0 {
			fn(l)
		}
	}
	return err
}

// transcript accumulates "[role] text" / "[tool] name: input" entries.
type transcript []string

// noise starts harness bookkeeping texts: codex context, claude slash-command records.
var noise = regexp.MustCompile(`^<(environment_context|command-name|command-message|command-args|local-command-stdout|local-command-stderr)>`)

func (t *transcript) text(role, s string) {
	if s = strings.TrimSpace(s); s != "" && !noise.MatchString(s) {
		*t = append(*t, "["+role+"] "+s)
	}
}

func (t *transcript) tool(name string, input json.RawMessage) {
	var s string
	if json.Unmarshal(input, &s) != nil {
		var c bytes.Buffer
		_ = json.Compact(&c, input)
		s = c.String()
	}
	if len(s) > 100 {
		s = strings.ToValidUTF8(s[:100], "") // drop a rune cut in half
	}
	*t = append(*t, "[tool] "+name+": "+s)
}

// content renders a message body: a string, or blocks of text and tool calls
// (claude tool_use{name,input}, pi toolCall{name,arguments}, codex *_text{text}).
func (t *transcript) content(role string, raw json.RawMessage) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		t.text(role, s)
		return
	}
	var blocks []struct {
		Type, Text, Name string
		Input            json.RawMessage `json:"input"`
		Arguments        json.RawMessage `json:"arguments"`
	}
	_ = json.Unmarshal(raw, &blocks)
	for _, b := range blocks {
		switch b.Type {
		case "tool_use":
			t.tool(b.Name, b.Input)
		case "toolCall":
			t.tool(b.Name, b.Arguments)
		case "text", "input_text", "output_text":
			t.text(role, b.Text)
		}
	}
}

func (t transcript) String() string { return strings.Join(t, "\n\n") }
