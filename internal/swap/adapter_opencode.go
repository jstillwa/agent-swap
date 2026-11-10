package swap

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

//go:embed plugins/agent-swap-opencode.js
var opencodePlugin []byte

// opencodeAdapter runs opencode with a plugin that calls `agent-swap hook`.
type opencodeAdapter struct{ noPoll }

func (opencodeAdapter) Command(h Harness, native, prompt string, extra []string, rt Runtime) (Launch, error) {
	if err := checkID(native); err != nil {
		return Launch{}, err
	}
	plugin := filepath.Join(rt.Dir, "agent-swap-opencode.js")
	cfg := filepath.Join(rt.Dir, "opencode.json")
	// Let the agent read the context file and write the notes/ready files without a
	// permission prompt. opencode matches the path as given, so allow both the
	// clean path and its symlink-resolved form (/var → /private/var on macOS).
	allow := map[string]string{}
	for _, d := range []string{rt.Dir, rt.TmpDir} {
		if d == "" {
			continue
		}
		allow[filepath.Join(d, "*")] = "allow"
		if r, err := filepath.EvalSymlinks(d); err == nil {
			allow[filepath.Join(r, "*")] = "allow"
		}
	}
	b, _ := json.Marshal(map[string]any{"$schema": "https://opencode.ai/config.json", "plugin": []string{"file://" + plugin},
		"permission": map[string]any{"external_directory": allow}})
	if err := os.WriteFile(plugin, opencodePlugin, 0o600); err != nil {
		return Launch{}, err
	}
	if err := os.WriteFile(cfg, b, 0o600); err != nil {
		return Launch{}, err
	}
	var args []string
	if native != "" {
		args = append(args, "-s", native)
	}
	args = append(append(args, h.Args...), extra...)
	if prompt != "" {
		args = append(args, "--prompt", prompt)
	}
	return Launch{Bin: binOr(h, "opencode"), Args: args,
		Env: []string{"OPENCODE_CONFIG=" + cfg, "OPENCODE_DISABLE_AUTOUPDATE=1"}}, nil
}

func (opencodeAdapter) Quit() string { return "/exit" }

// Transcript runs `opencode export <id>`. Its stdout goes to a file: opencode
// truncates output written to a pipe (observed at 192 KiB in 1.19.5).
func (opencodeAdapter) Transcript(native string, rt Runtime) (string, error) {
	if !safeID(native) {
		return "", fmt.Errorf("bad opencode session id %q", native)
	}
	bin := "opencode"
	for _, h := range rt.Cfg.Harnesses {
		if h.Kind == "opencode" && h.Bin != "" {
			bin = h.Bin
			break
		}
	}
	out, err := os.CreateTemp(rt.Dir, "opencode-export-*.json")
	if err != nil {
		return "", err
	}
	defer os.Remove(out.Name())
	defer out.Close()
	var stderr bytes.Buffer
	cmd := exec.Command(bin, "export", native)
	cmd.Stdout, cmd.Stderr = out, &stderr
	if st, err := os.Stat(rt.Cwd); err == nil && st.IsDir() {
		cmd.Dir = rt.Cwd // sessions are looked up per project
	}
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("opencode export: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	b, err := os.ReadFile(out.Name())
	if err != nil {
		return "", err
	}
	var ex struct {
		Messages []struct {
			Info  struct{ Role string }
			Parts []struct {
				Type, Text, Tool string
				State            struct{ Input json.RawMessage }
			}
		}
	}
	if err := json.Unmarshal(b, &ex); err != nil {
		return "", fmt.Errorf("opencode export: %v", err)
	}
	var t transcript
	for _, m := range ex.Messages {
		for _, p := range m.Parts {
			switch p.Type {
			case "text":
				t.text(m.Info.Role, p.Text)
			case "tool":
				t.tool(p.Tool, p.State.Input)
			}
		}
	}
	return t.String(), nil
}
