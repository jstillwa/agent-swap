package swap

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodexCommand(t *testing.T) {
	runCmdCases(t, "codex", []cmdCase{
		{name: "fresh bare", wantBin: "codex", wantArgs: []string{}},
		{name: "fresh prompt args extra", h: Harness{Args: []string{"-m", "gpt-5.5"}}, extra: []string{"--search"},
			prompt: "read ctx.md", wantBin: "codex", wantArgs: []string{"-m", "gpt-5.5", "--search", "read ctx.md"}},
		{name: "resume", native: "019a-uuid", prompt: "go on", h: Harness{Args: []string{"-m", "gpt-5.5"}},
			wantBin: "codex", wantArgs: []string{"resume", "019a-uuid", "-m", "gpt-5.5", "go on"}},
		{name: "resume no prompt", native: "019a-uuid", wantBin: "codex", wantArgs: []string{"resume", "019a-uuid"}},
		{name: "bin override", h: Harness{Bin: "/opt/codex"}, wantBin: "/opt/codex", wantArgs: []string{}},
	})
}

const tCodexDay = "2026/11/10"

// codexLine renders one rollout line.
func tCodexLine(typ string, payload map[string]any) string {
	b, _ := json.Marshal(map[string]any{"timestamp": "2026-11-10T05:37:09.000Z", "type": typ, "payload": payload})
	return string(b)
}

// tWriteRollout writes a fresh interactive rollout for id/cwd plus extra event lines; returns its path.
func tWriteRollout(t *testing.T, sessionsRoot, stamp, id, cwd string, lines ...string) string {
	t.Helper()
	return tWriteRolloutMeta(t, sessionsRoot, stamp, map[string]any{"id": id, "cwd": cwd}, lines...)
}

// tWriteRolloutMeta is tWriteRollout with session_meta overrides (defaults: timestamp now, source cli).
func tWriteRolloutMeta(t *testing.T, sessionsRoot, stamp string, meta map[string]any, lines ...string) string {
	t.Helper()
	m := map[string]any{"timestamp": time.Now().UTC().Format(time.RFC3339Nano), "cli_version": "0.166.0",
		"source": "cli", "originator": "codex_cli_rs"}
	for k, v := range meta {
		m[k] = v
	}
	p := filepath.Join(sessionsRoot, tCodexDay, fmt.Sprintf("rollout-2026-11-10T%s-%s.jsonl", stamp, m["id"]))
	writeFile(t, p, strings.Join(append([]string{tCodexLine("session_meta", m)}, lines...), "\n")+"\n")
	return p
}

func tEv(typ string, kv ...any) string {
	p := map[string]any{"type": typ}
	for i := 0; i+1 < len(kv); i += 2 {
		p[kv[i].(string)] = kv[i+1]
	}
	return tCodexLine("event_msg", p)
}

func tLimits(primary, secondary any) string {
	return tEv("token_count", "info", nil, "rate_limits", map[string]any{"limit_id": "codex", "primary": primary, "secondary": secondary})
}

func tWin(pct float64, reset int64) map[string]any {
	return map[string]any{"used_percent": pct, "window_minutes": 300, "resets_at": reset}
}

func TestCodexPollDiscoversNativeID(t *testing.T) {
	rt := testRT(t)
	root := filepath.Join(rt.Home, ".codex", "sessions")
	old := rt.Launched.Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	// Recent mtime but session started before launch: a resumed/old session, not ours.
	tWriteRolloutMeta(t, root, "07-00-00", map[string]any{"id": "00000000-0000-4000-8000-000000000001", "cwd": rt.Cwd, "timestamp": old})
	tWriteRollout(t, root, "05-00-00", "00000000-0000-4000-8000-000000000002", rt.Cwd)
	tWriteRollout(t, root, "06-00-00", "00000000-0000-4000-8000-000000000003", "/some/other/dir")
	// `codex exec` runs (e.g. other tools) in the same cwd must be ignored.
	tWriteRolloutMeta(t, root, "08-00-00", map[string]any{"id": "00000000-0000-4000-8000-000000000004", "cwd": rt.Cwd, "source": "exec"})
	tWriteRolloutMeta(t, root, "09-00-00", map[string]any{"id": "00000000-0000-4000-8000-000000000005", "cwd": rt.Cwd, "originator": "codex_exec"})
	var s Session
	var u Usage
	if err := adapterFor(t, "codex").Poll(Harness{Name: "codex", Kind: "codex"}, rt, &s, &u); err != nil {
		t.Fatal(err)
	}
	if s.NativeID != "00000000-0000-4000-8000-000000000002" {
		t.Errorf("NativeID = %q, want the fresh interactive rollout for this cwd", s.NativeID)
	}
}

func TestCodexPollHonorsCodexHome(t *testing.T) {
	rt := testRT(t)
	ch := t.TempDir()
	rt.Env = []string{"CODEX_HOME=" + ch}
	tWriteRollout(t, filepath.Join(ch, "sessions"), "05-00-00", "00000000-0000-4000-8000-0000000000aa", rt.Cwd)
	var s Session
	if err := adapterFor(t, "codex").Poll(Harness{Name: "codex", Kind: "codex"}, rt, &s, &Usage{}); err != nil {
		t.Fatal(err)
	}
	if s.NativeID != "00000000-0000-4000-8000-0000000000aa" {
		t.Errorf("NativeID = %q, want rollout under $CODEX_HOME", s.NativeID)
	}
}

func TestCodexPollNoRollout(t *testing.T) {
	rt := testRT(t)
	tWriteRollout(t, filepath.Join(rt.Home, ".codex", "sessions"), "05-00-00", "00000000-0000-4000-8000-0000000000bb", "/elsewhere")
	s := Session{Idle: true}
	if err := adapterFor(t, "codex").Poll(Harness{Name: "codex", Kind: "codex"}, rt, &s, &Usage{}); err != nil {
		t.Fatalf("Poll with no matching rollout should not fail: %v", err)
	}
	if s.NativeID != "" || !s.Idle || s.RateLimited {
		t.Errorf("session changed without a rollout: %+v", s)
	}
}

func TestCodexPollState(t *testing.T) {
	const id = "00000000-0000-4000-8000-0000000000cc"
	future, past := time.Now().Add(3*time.Hour).Unix(), time.Now().Add(-time.Hour).Unix()
	started, done := tEv("task_started", "turn_id", "t1"), tEv("task_complete", "turn_id", "t1")
	cases := []struct {
		name        string
		lines       []string
		idle, limit bool
		windows     map[string]Window
	}{
		{name: "no task yet", idle: true},
		{name: "task running", lines: []string{started}},
		{name: "task complete", lines: []string{started, done}, idle: true},
		{name: "second task running", lines: []string{started, done, started}},
		{name: "windows from last token_count", idle: true,
			lines:   []string{tLimits(tWin(10, future), tWin(5, future)), tLimits(tWin(70.5, future), tWin(20, future+99))},
			windows: map[string]Window{"primary": {70.5, future}, "secondary": {20, future + 99}}},
		{name: "null secondary skipped", idle: true, lines: []string{tLimits(tWin(30, future), nil)},
			windows: map[string]Window{"primary": {30, future}}},
		{name: "null rate_limits tolerated", idle: true, lines: []string{tEv("token_count", "info", nil, "rate_limits", nil)}},
		{name: "live window at 100", idle: true, limit: true, lines: []string{tLimits(tWin(100, future), nil)},
			windows: map[string]Window{"primary": {100, future}}},
		{name: "expired window at 100 is not limited", idle: true, lines: []string{tLimits(tWin(100, past), nil)}},
		{name: "usage limit error", idle: true, limit: true,
			lines: []string{started, tEv("error", "message", "You've hit your usage limit. Try again later."), done}},
		{name: "credits: 100% is not limited and sets no windows", idle: true, windows: map[string]Window{},
			lines: []string{tEv("token_count", "rate_limits", map[string]any{"primary": tWin(100, future), "secondary": nil,
				"credits": map[string]any{"has_credits": true, "unlimited": false}})}},
		{name: "unlimited credits", idle: true, windows: map[string]Window{},
			lines: []string{tEv("token_count", "rate_limits", map[string]any{"primary": tWin(100, future),
				"credits": map[string]any{"has_credits": false, "unlimited": true}})}},
		{name: "rate_limit_reached_type set", idle: true, limit: true, windows: map[string]Window{},
			lines: []string{tEv("token_count", "rate_limits", map[string]any{"primary": tWin(20, future),
				"credits": map[string]any{"has_credits": true}, "rate_limit_reached_type": "workspace_member_usage_limit_reached"})}},
		{name: "other error", idle: true, lines: []string{started, tEv("error", "message", "stream disconnected before completion"), done}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rt := testRT(t)
			p := tWriteRollout(t, filepath.Join(rt.Home, ".codex", "sessions"), "05-00-00", id, rt.Cwd, c.lines...)
			setMtime(t, p, rt.Launched.Add(-48*time.Hour)) // a known NativeID is found regardless of age
			s := Session{NativeID: id, Idle: !c.idle, RateLimited: !c.limit}
			u := Usage{}
			if err := adapterFor(t, "codex").Poll(Harness{Name: "codex", Kind: "codex"}, rt, &s, &u); err != nil {
				t.Fatal(err)
			}
			if s.NativeID != id || s.Idle != c.idle || s.RateLimited != c.limit {
				t.Errorf("session = %+v, want idle=%v limited=%v", s, c.idle, c.limit)
			}
			for k, w := range c.windows {
				if g := u.Windows[k]; g == nil || *g != w {
					t.Errorf("window %s = %+v, want %+v", k, g, w)
				}
			}
			if c.windows != nil && len(u.Windows) != len(c.windows) {
				t.Errorf("windows = %v, want keys of %v", u.Windows, c.windows)
			}
		})
	}
}

func TestCodexTurnsAndPollFixture(t *testing.T) {
	rt := testRT(t)
	const id = "019a0000-0000-7000-8000-00000000f1f1"
	placeFixture(t, "codex/rollout.jsonl",
		filepath.Join(rt.Home, ".codex", "sessions", tCodexDay, "rollout-2026-11-10T05-37-09-"+id+".jsonl"),
		"__ID__", id, "__CWD__", rt.Cwd, "__TS__", time.Now().UTC().Format(time.RFC3339Nano))
	a := adapterFor(t, "codex")
	got, _ := readTurnsOK(t, a, id, "", rt)
	wantTurns(t, got,
		uT("codex", "Add a hello file to the synthetic repo."),
		toolTurn("codex", "apply_patch", "*** Begin Patch\n*** Add File: hello.txt\n+synthetic\n*** End Patch", "Success. Updated hello.txt"),
		toolTurn("codex", "exec", "echo "+strings.Repeat("y", 200), ""),
		aT("codex", "Created hello.txt with synthetic content."))
	for _, tt := range got {
		if strings.Contains(tt.Text, "SHOULD NOT APPEAR") || strings.Contains(tt.Text, "environment_context") {
			t.Errorf("noise in turn: %q", tt.Text)
		}
	}

	var s Session
	u := Usage{}
	if err := a.Poll(Harness{Name: "codex", Kind: "codex"}, rt, &s, &u); err != nil {
		t.Fatal(err)
	}
	if s.NativeID != id || !s.Idle || s.RateLimited {
		t.Errorf("session = %+v", s)
	}
	if p, q := u.Windows["primary"], u.Windows["secondary"]; p == nil || q == nil ||
		*p != (Window{61.5, 4102444800}) || *q != (Window{12, 4103049600}) {
		t.Errorf("windows = %+v", u.Windows)
	}
}

func TestCodexTurnsMissing(t *testing.T) {
	if _, _, err := adapterFor(t, "codex").Turns("00000000-0000-4000-8000-00000000dead", "", tStoreRT(t)); err == nil {
		t.Error("want error for a missing rollout")
	}
}

func tMsg(role, text string) string {
	kind := "input_text"
	if role == "assistant" {
		kind = "output_text"
	}
	return tCodexLine("response_item", map[string]any{"type": "message", "role": role,
		"content": []map[string]string{{"type": kind, "text": text}}})
}

func TestCodexTurnsCursorBoundaryAndSkips(t *testing.T) {
	rt, a := tStoreRT(t), adapterFor(t, "codex")
	const id = "019a0000-0000-7000-8000-00000000c0de"
	p := tWriteRollout(t, filepath.Join(envGet(rt.Env, "CODEX_HOME"), "sessions"), "05-00-00", id, rt.Cwd,
		tEv("task_started", "turn_id", "t1"), tMsg("developer", "DEVELOPER SHOULD NOT APPEAR"),
		tMsg("user", "<environment_context>\n<cwd>/x</cwd>\n</environment_context>"),
		tMsg("user", "# AGENTS.md instructions for /x\nINSTRUCTIONS SHOULD NOT APPEAR"),
		tMsg("user", "First question."), tMsg("assistant", "First answer."),
		tEv("task_complete", "turn_id", "t1"))
	got, cur := readTurnsOK(t, a, id, "", rt)
	wantTurns(t, got, uT("codex", "First question."), aT("codex", "First answer."))
	if st, _ := os.Stat(p); cur != fmt.Sprint(st.Size()) {
		t.Errorf("cursor = %s, want the file end %d", cur, st.Size())
	}
	// A task still running is not read; neither is a partial last line.
	cAppend(t, p, strings.Join([]string{tEv("task_started", "turn_id", "t2"), tMsg("user", "Second question."),
		tCodexLine("response_item", map[string]any{"type": "function_call", "name": "shell", "call_id": "k1", "arguments": `{"cmd":"ls"}`}),
		tCodexLine("response_item", map[string]any{"type": "function_call", "name": "shell", "call_id": "k2", "arguments": `{"cmd":"pwd"}`}),
		tCodexLine("response_item", map[string]any{"type": "function_call_output", "call_id": "k1", "output": "a b"}),
	}, "\n")+"\n")
	if got, c := readTurnsOK(t, a, id, cur, rt); len(got) != 0 || c != cur {
		t.Errorf("running task: Turns = %q, cursor %s; want none, %s", got, c, cur)
	}
	ab := tEv("turn_aborted", "turn_id", "t2", "reason", "interrupted")
	cAppend(t, p, ab[:10])
	if got, c := readTurnsOK(t, a, id, cur, rt); len(got) != 0 || c != cur {
		t.Errorf("partial line: Turns = %q, cursor %s; want none, %s", got, c, cur)
	}
	cAppend(t, p, ab[10:]+"\n"+strings.Join([]string{
		tCodexLine("compacted", map[string]any{"message": "COMPACTED SHOULD NOT APPEAR", "replacement_history": []any{}}),
		tEv("task_started", "turn_id", "t3"), tMsg("user", "Third </agent-swap-tool> sk-abcdefghijklmnopqrstuvwxyz"),
		tMsg("assistant", "Third answer."), tEv("task_complete", "turn_id", "t3")}, "\n")+"\n")
	got, _ = readTurnsOK(t, a, id, cur, rt)
	wantTurns(t, got,
		uT("codex", "Second question."),
		toolTurn("codex", "shell", `{"cmd":"ls"}`, "a b"),
		toolTurn("codex", "shell", `{"cmd":"pwd"}`, ""), // dangling: interrupted
		uT("codex", "Third &lt;/agent-swap-tool> [redacted]"),
		aT("codex", "Third answer."))
}

// tFakeBin writes an executable shell script and returns its path.
func tFakeBin(t *testing.T, name, script string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCodexWriteRoundTripAndMeta(t *testing.T) {
	rt := tStoreRT(t)
	h := Harness{Name: "codex", Kind: "codex", Bin: tFakeBin(t, "codex", "echo 'codex-cli 9.8.7'\n")}
	m := tRoundTrip(t, "codex", h, rt, tConv("LYNX-5"), tConv("MOLE-6"))
	tNoHomeStores(t, rt)
	root := envGet(rt.Env, "CODEX_HOME")
	if !strings.HasPrefix(m.Path, filepath.Join(root, "sessions")+"/") || !strings.HasSuffix(m.Path, "-"+m.Native+".jsonl") ||
		!strings.HasPrefix(filepath.Base(m.Path), "rollout-") {
		t.Errorf("Path = %s", m.Path)
	}
	if st, err := os.Stat(m.Path); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("rollout mode: %v %v", st, err)
	}
	f, err := os.Open(m.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	line, _ := bufio.NewReader(f).ReadBytes('\n')
	var meta struct {
		Timestamp, Type string
		Payload         map[string]any
	}
	if err := json.Unmarshal(line, &meta); err != nil || meta.Type != "session_meta" {
		t.Fatalf("first line %s: %v", line, err)
	}
	want := map[string]any{"id": m.Native, "cwd": rt.Cwd, "originator": "agent-swap", "cli_version": "9.8.7",
		"source": "cli", "model_provider": "openai"}
	for k, v := range want {
		if meta.Payload[k] != v {
			t.Errorf("session_meta.%s = %v, want %v", k, meta.Payload[k], v)
		}
	}
	if ts, _ := meta.Payload["timestamp"].(string); ts == "" {
		t.Error("session_meta.timestamp missing")
	}
	// a native or Poll lookup finds it by id
	if p, err := sessionPath("codex", m.Native, rt); err != nil || p != m.Path {
		t.Errorf("sessionPath = %s, %v", p, err)
	}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if strings.Contains(d.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", p)
		}
		return nil
	})
}

func TestCodexWriteDropsNoiseFromCount(t *testing.T) {
	rt := tStoreRT(t)
	h := Harness{Name: "codex", Kind: "codex", Bin: tFakeBin(t, "codex", "echo 'codex-cli 1.0.0'\n")}
	m := &Mirror{}
	n, err := adapterFor(t, "codex").Write(h, m, []Turn{uT("claude", "<environment_context>x</environment_context>"),
		uT("claude", "Real question."), aT("claude", "  "), aT("claude", "Real answer.")}, rt)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := readTurnsOK(t, adapterFor(t, "codex"), m.Native, "", rt)
	if n != len(got) || n != 2 {
		t.Errorf("Write returned %d, read back %d (%q); want 2", n, len(got), got)
	}
}

func TestCodexWriteAppendContinuesOrdinal(t *testing.T) {
	rt := tStoreRT(t)
	const id = "019a0000-0000-7000-8000-0000000000dd"
	ord := func(n int, line string) string {
		var v map[string]any
		_ = json.Unmarshal([]byte(line), &v)
		v["ordinal"] = n
		b, _ := json.Marshal(v)
		return string(b)
	}
	root := filepath.Join(envGet(rt.Env, "CODEX_HOME"), "sessions")
	p := filepath.Join(root, tCodexDay, "rollout-2026-11-10T05-00-00-"+id+".jsonl")
	writeFile(t, p, strings.Join([]string{
		ord(0, tCodexLine("session_meta", map[string]any{"id": id, "cwd": rt.Cwd, "originator": "codex_cli_rs"})),
		ord(1, tEv("task_started", "turn_id", "t1")), ord(2, tMsg("user", "Native q.")), ord(3, tMsg("assistant", "Native a.")),
		ord(4, tEv("task_complete", "turn_id", "t1"))}, "\n")+"\n")
	m := mirrorOf(t, id, p)
	old := m.Cursor
	h := Harness{Name: "codex", Kind: "codex", Bin: tFakeBin(t, "codex", "echo 'codex-cli 1.0.0'\n")}
	n, err := adapterFor(t, "codex").Write(h, m, []Turn{uT("pi", "From pi."), aT("pi", "Pi answer.")}, rt)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := readTurnsOK(t, adapterFor(t, "codex"), id, old, rt)
	if n != len(got) {
		t.Errorf("Append returned %d, read back %d", n, len(got))
	}
	wantTurns(t, got, uT("codex", "From pi."), aT("codex", "Pi answer."))
	b, _ := os.ReadFile(p)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	for i, l := range lines {
		var v struct{ Ordinal *int }
		if json.Unmarshal([]byte(l), &v) != nil || v.Ordinal == nil || *v.Ordinal != i {
			t.Errorf("line %d ordinal = %v, want %d", i, v.Ordinal, i)
		}
	}
	// a mirror whose file was swapped is refused
	m2 := *m
	m2.Ino++
	if _, err := adapterFor(t, "codex").Write(h, &m2, []Turn{uT("pi", "x"), aT("pi", "y")}, rt); err == nil {
		t.Error("Append with a stale identity must fail")
	}
}
