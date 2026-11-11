package swap

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
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
	b, _ := json.Marshal(map[string]any{"$schema": "https://opencode.ai/config.json", "plugin": []string{"file://" + plugin}})
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

// ocExport is the part of `opencode export` (and `opencode import` input) agent-swap reads.
type ocExport struct {
	Info     json.RawMessage `json:"info"`
	Messages []struct {
		Info struct {
			ID, Role string
			Summary  json.RawMessage // assistant: true on a compaction summary; user: an object
			Model    *ocModel        // user messages
			Time     struct{ Completed int64 }
		}
		Parts []struct {
			ID, Type, Text, Tool string
			Synthetic            bool
			State                struct {
				Status        string
				Input         json.RawMessage
				Output, Error string
			}
		}
	}
}

type ocModel struct {
	ProviderID string `json:"providerID"`
	ModelID    string `json:"modelID"`
}

// ocModelOf splits "provider/model" at the first "/" (nil if malformed).
func ocModelOf(s string) *ocModel {
	if p, m, _ := strings.Cut(s, "/"); p != "" && m != "" {
		return &ocModel{p, m}
	}
	return nil
}

// ocArgModel is the model chosen by -m/--model in args (nil if none).
func ocArgModel(args []string) *ocModel {
	for i, a := range args {
		if v, ok := strings.CutPrefix(a, "--model="); ok {
			return ocModelOf(v)
		} else if (a == "-m" || a == "--model") && i+1 < len(args) {
			return ocModelOf(args[i+1])
		}
	}
	return nil
}

// ocConfigModel is the default `model` of opencode's resolved config (nil if unset).
func ocConfigModel(bin string, rt Runtime) *ocModel {
	var cfg struct{ Model string }
	if b, err := ocOutput(bin, rt, "debug", "config"); err == nil && json.Unmarshal(b, &cfg) == nil {
		return ocModelOf(cfg.Model)
	}
	return nil
}

var ocBins sync.Map // name → resolved path

// ocRun runs `opencode --pure <args>` in rt.Cwd with a clean env and a 60 s
// limit (SIGTERM, then SIGKILL), stdout to a file: opencode truncates output
// written to a pipe (observed at 192 KiB in 1.19.5).
func ocRun(bin string, rt Runtime, stdout io.Writer, args ...string) error {
	p, ok := ocBins.Load(bin)
	if !ok {
		lp, err := exec.LookPath(bin)
		if err != nil {
			return err
		}
		p, _ = ocBins.LoadOrStore(bin, lp)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.(string), append([]string{"--pure"}, args...)...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = stdout, &stderr
	cmd.Dir = rt.Cwd // sessions are looked up per project
	for _, kv := range rt.Env {
		if !strings.HasPrefix(kv, "OPENCODE_CONFIG=") && !strings.HasPrefix(kv, "AGENT_SWAP_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "OPENCODE_DISABLE_AUTOUPDATE=1")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("opencode %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func ocBin(rt Runtime) string {
	for _, h := range rt.Cfg.Harnesses {
		if h.Kind == "opencode" {
			return binOr(h, "opencode")
		}
	}
	return "opencode"
}

// ocOutput runs opencode with stdout in a private temp file and returns it.
func ocOutput(bin string, rt Runtime, args ...string) ([]byte, error) {
	dir, err := os.MkdirTemp("", "agent-swap-oc-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	out, err := os.OpenFile(filepath.Join(dir, "out.json"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	if err := ocRun(bin, rt, out, args...); err != nil {
		return nil, err
	}
	return os.ReadFile(out.Name())
}

// ocSessionDir is the project directory opencode recorded for a session.
func ocSessionDir(native string, rt Runtime) (string, error) {
	ex, err := ocExportSession(ocBin(rt), native, rt)
	if err != nil {
		return "", err
	}
	var info struct{ Directory string }
	if err := json.Unmarshal(ex.Info, &info); err != nil || info.Directory == "" {
		return "", fmt.Errorf("opencode session %s: no directory", native)
	}
	return info.Directory, nil
}

// ocExportSession runs `opencode export <id>`.
func ocExportSession(bin, native string, rt Runtime) (*ocExport, error) {
	if !safeID(native) {
		return nil, fmt.Errorf("bad opencode session id %q", native)
	}
	b, err := ocOutput(bin, rt, "export", native)
	if err != nil {
		return nil, err
	}
	var ex ocExport
	if err := json.Unmarshal(b, &ex); err != nil {
		return nil, fmt.Errorf("opencode export: %v", err)
	}
	return &ex, nil
}

// Turns reads messages after the cursor message id, stopping at an assistant
// message still running. Compaction summaries, reasoning and synthetic parts are
// skipped. A cursor message that no longer exists (/undo) also yields errRewound.
func (opencodeAdapter) Turns(native, cursor string, rt Runtime) ([]Turn, string, error) {
	ex, err := ocExportSession(ocBin(rt), native, rt)
	if err != nil {
		return nil, cursor, err
	}
	var out []Turn
	next, found := cursor, cursor == ""
	for i, m := range ex.Messages {
		if m.Info.ID <= cursor {
			found = found || m.Info.ID == cursor
			continue
		}
		// An unfinished assistant message is running only when it is the last
		// one and the child still runs; otherwise it was interrupted.
		if m.Info.Role == "assistant" && m.Info.Time.Completed == 0 && i == len(ex.Messages)-1 && !rt.Final {
			break
		}
		next = m.Info.ID
		if m.Info.Role == "assistant" && string(m.Info.Summary) == "true" {
			continue
		}
		for _, p := range m.Parts {
			switch {
			case p.Type == "text" && !p.Synthetic:
				if t := textTurn("opencode", m.Info.Role, p.Text); t.Text != "" {
					out = append(out, t)
				}
			case p.Type == "tool":
				o := p.State.Output
				if p.State.Status == "error" {
					o = p.State.Error
				} else if p.State.Status != "completed" {
					o = interruptedOutput
				}
				out = append(out, toolTurn("opencode", p.Tool, rawText(p.State.Input), o))
			}
		}
	}
	if !found {
		return out, next, errRewound // /undo removed the cursor message
	}
	return out, next, nil
}

// Write builds an export-format session and loads it with `opencode import`,
// which keeps ids and skips existing rows: an Append re-imports the same ses_
// with only new msg_/prt_ ids, all above the session's current maximum.
func (a opencodeAdapter) Write(h Harness, m *Mirror, turns []Turn, rt Runtime) (int, error) {
	var keep []Turn // only turns Turns reads back, so the count is exact
	for _, t := range turns {
		if textTurn("opencode", t.Role, t.Text).Text != "" {
			keep = append(keep, t)
		}
	}
	if err := a.write(h, m, keep, rt); err != nil {
		return 0, err
	}
	return len(keep), nil
}

func (opencodeAdapter) write(h Harness, m *Mirror, turns []Turn, rt Runtime) error {
	bin := binOr(h, "opencode")
	now := time.Now()
	info := map[string]any{}
	var have []string
	parent, last := "", ""       // parent: the user message an assistant message answers
	maxID := map[string]string{} // "msg"/"prt" → highest existing id
	// opencode resumes with the last user message's model, so it must be a
	// real one: -m from args, else the session's own, else the config default.
	model, sesModel := ocArgModel(h.Args), (*ocModel)(nil)
	if m.Native == "" {
		m2 := opencodeID("ses", now)
		info = map[string]any{"id": m2, "slug": "agent-swap", "projectID": "", "directory": rt.Cwd,
			"title": "agent-swap mirror", "version": "agent-swap",
			"time": map[string]int64{"created": now.UnixMilli(), "updated": now.UnixMilli()}}
	} else {
		ex, err := ocExportSession(bin, m.Native, rt)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(ex.Info, &info); err != nil {
			return err
		}
		for _, msg := range ex.Messages {
			have, maxID["msg"] = append(have, msg.Info.ID), max(maxID["msg"], msg.Info.ID)
			if msg.Info.Role == "user" {
				parent = msg.Info.ID
			}
			if mm := msg.Info.Model; mm != nil && mm.ProviderID != "agent-swap" {
				sesModel = mm
			}
			for _, p := range msg.Parts {
				have, maxID["prt"] = append(have, p.ID), max(maxID["prt"], p.ID)
			}
		}
	}
	if model == nil {
		model = sesModel
	}
	if model == nil {
		if model = ocConfigModel(bin, rt); model == nil {
			return fmt.Errorf("%w: opencode has no model to resume with (set -m in the harness args or model in its config)", errNoMirror)
		}
	}
	ses, _ := info["id"].(string)
	if !safeID(ses) {
		return fmt.Errorf("bad opencode session id %q", ses)
	}
	var msgs []map[string]any
	var want []string
	for _, t := range turns {
		if t.Text == "" {
			continue
		}
		ms := now.UnixMilli()
		id, pid := opencodeID("msg", now), opencodeID("prt", now)
		if id <= maxID["msg"] || pid <= maxID["prt"] {
			return fmt.Errorf("opencode: session %s has ids above %s", ses, id)
		}
		mi := map[string]any{"id": id, "sessionID": ses, "role": t.Role, "time": map[string]int64{"created": ms}, "agent": "build"}
		if t.Role == "user" {
			mi["model"], parent = model, id
		} else {
			if parent == "" {
				parent = id // no user message yet; opencode requires a msg_ id
			}
			mi["time"] = map[string]int64{"created": ms, "completed": ms}
			for k, v := range map[string]any{"parentID": parent, "modelID": model.ModelID, "providerID": model.ProviderID,
				"mode": "build", "path": map[string]string{"cwd": rt.Cwd, "root": rt.Cwd}, "cost": 0, "finish": "stop",
				"tokens": map[string]any{"input": 0, "output": 0, "reasoning": 0, "cache": map[string]int{"read": 0, "write": 0}}} {
				mi[k] = v
			}
		}
		msgs = append(msgs, map[string]any{"info": mi, "parts": []map[string]any{
			{"id": pid, "sessionID": ses, "messageID": id, "type": "text", "text": t.Text}}})
		want, last = append(want, id, pid), id
	}
	all := append(have, want...)
	check := func() error { // the session holds exactly the old and new ids
		ex, err := ocExportSession(bin, ses, rt)
		if err != nil {
			return err
		}
		got := map[string]bool{}
		for _, msg := range ex.Messages {
			got[msg.Info.ID] = true
			for _, p := range msg.Parts {
				got[p.ID] = true
			}
		}
		if len(got) != len(all) {
			return fmt.Errorf("opencode import: session has %d ids, want %d", len(got), len(all))
		}
		for _, id := range all {
			if !got[id] {
				return fmt.Errorf("opencode import: %s missing after import", id)
			}
		}
		return nil
	}
	if err := ocImport(bin, rt, map[string]any{"info": info, "messages": msgs}, check); err != nil {
		return err
	}
	m.Native = ses
	if last != "" {
		m.Cursor = last
	}
	return nil
}

// ocImport writes v to an O_EXCL file in a private temp dir, imports it and
// runs check; on failure it imports the same file once more.
func ocImport(bin string, rt Runtime, v any, check func() error) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "agent-swap-oc-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	p := filepath.Join(dir, "import.json")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	// `import -- <file>` is rejected by opencode's argument parser. An import is
	// not transactional: a killed one leaves a prefix, and rerunning the same
	// file completes it (rows already present are skipped). Write verifies ids.
	for i := 0; ; i++ {
		if err = ocRun(bin, rt, nil, "import", p); err == nil {
			err = check()
		}
		if err == nil || i == 1 {
			return err
		}
	}
}
