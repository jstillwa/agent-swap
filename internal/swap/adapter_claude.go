package swap

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// claudeAdapter runs Claude Code; its signals come from hooks and the statusline.
type claudeAdapter struct{ noPoll }

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]`)

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
	bin := h.Bin
	if bin == "" {
		bin = rt.Cfg.ClaudeBin
	}
	return Launch{Bin: bin, Args: append(args, "--settings", path)}, nil
}

func (claudeAdapter) Quit() string { return "/exit" }

// Transcript reads ~/.claude/projects/<cwd slug>/<native>.jsonl.
func (claudeAdapter) Transcript(native string, rt Runtime) (string, error) {
	if !safeID(native) {
		return "", fmt.Errorf("bad claude session id %q", native)
	}
	projects := filepath.Join(rt.Home, ".claude", "projects")
	path := filepath.Join(projects, nonAlnum.ReplaceAllString(rt.Cwd, "-"), native+".jsonl")
	if _, err := os.Stat(path); err != nil { // long cwds get a hashed slug
		if m, _ := filepath.Glob(filepath.Join(projects, "*", native+".jsonl")); len(m) > 0 {
			path = m[0]
		}
	}
	var t transcript
	err := eachLine(path, func(l []byte) {
		var r struct {
			Type                string
			IsMeta, IsSidechain bool
			Message             struct{ Content json.RawMessage }
		}
		if json.Unmarshal(l, &r) == nil && (r.Type == "user" || r.Type == "assistant") && !r.IsMeta && !r.IsSidechain {
			t.content(r.Type, r.Message.Content)
		}
	})
	return t.String(), err
}
