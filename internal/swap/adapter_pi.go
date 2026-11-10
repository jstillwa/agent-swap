package swap

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
		args = append(args, "--session", native)
	}
	args = append(append(args, h.Args...), extra...)
	if prompt != "" {
		args = append(args, prompt)
	}
	return Launch{Bin: binOr(h, "pi"), Args: args}, nil
}

func (piAdapter) Quit() string { return "/quit" }

func (piAdapter) Transcript(native string, rt Runtime) (string, error) {
	if !safeID(native) {
		return "", fmt.Errorf("bad pi session id %q", native)
	}
	dir := os.Getenv("PI_CODING_AGENT_SESSION_DIR")
	if dir == "" {
		dir = filepath.Join(rt.Home, ".pi", "agent", "sessions")
	}
	m, _ := filepath.Glob(filepath.Join(dir, "*", "*_"+native+".jsonl"))
	if flat, _ := filepath.Glob(filepath.Join(dir, "*_"+native+".jsonl")); len(m) == 0 {
		m = flat // PI_CODING_AGENT_SESSION_DIR may hold the files directly
	}
	if len(m) == 0 {
		return "", fmt.Errorf("pi session %s not found", native)
	}
	var t transcript
	err := eachLine(m[len(m)-1], func(l []byte) {
		var r struct {
			Type    string
			Message struct {
				Role    string
				Content json.RawMessage
			}
		}
		if json.Unmarshal(l, &r) == nil && r.Type == "message" && (r.Message.Role == "user" || r.Message.Role == "assistant") {
			t.content(r.Message.Role, r.Message.Content)
		}
	})
	return t.String(), err
}
