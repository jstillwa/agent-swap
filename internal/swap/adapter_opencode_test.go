package swap

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestOpencodeCommand(t *testing.T) {
	runCmdCases(t, "opencode", []cmdCase{
		{name: "fresh bare", wantBin: "opencode", wantArgs: []string{}},
		{name: "fresh prompt args extra", h: Harness{Args: []string{"-m", "gateway/glm"}}, extra: []string{"--agent", "build"},
			prompt: "read ctx.md", wantBin: "opencode", wantArgs: []string{"-m", "gateway/glm", "--agent", "build", "--prompt", "read ctx.md"}},
		{name: "resume", native: "ses_abc", prompt: "go on", wantBin: "opencode",
			wantArgs: []string{"-s", "ses_abc", "--prompt", "go on"}},
		{name: "bin override", h: Harness{Bin: "/opt/oc"}, wantBin: "/opt/oc", wantArgs: []string{}},
	})
}

func TestOpencodeCommandConfigAndPlugin(t *testing.T) {
	rt := testRT(t)
	l, err := adapterFor(t, "opencode").Command(Harness{Name: "oc", Kind: "opencode"}, "", "", nil, rt)
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(rt.Dir, "opencode.json")
	for _, e := range []string{"OPENCODE_CONFIG=" + cfgPath, "OPENCODE_DISABLE_AUTOUPDATE=1"} {
		if !slices.Contains(l.Env, e) {
			t.Errorf("Env %q missing %q", l.Env, e)
		}
	}
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("opencode.json: %v: %s", err, b)
	}
	var plugins []string
	_ = json.Unmarshal(cfg["plugin"], &plugins)
	plugin := filepath.Join(rt.Dir, "agent-swap-opencode.js")
	if !slices.Equal(plugins, []string{"file://" + plugin}) {
		t.Errorf("plugin = %q, want [file://%s]", plugins, plugin)
	}
	if st, err := os.Stat(plugin); err != nil || st.Size() == 0 {
		t.Errorf("plugin file not written: %v", err)
	}
	if _, ok := cfg["permission"]; ok { // the handoff-era external_directory grant is gone
		t.Errorf("opencode.json still grants permissions: %s", b)
	}
}

// --- fake opencode ---

// TestFakeOpencodeProcess is not a test: tFakeOC's script runs the test binary
// as a fake `opencode` (FAKE_OC_STATE = its state dir). It keeps sessions in
// db.json, imports like opencode (keeps ids, skips existing rows), and records
// every call's args, env and cwd in calls.jsonl.
func TestFakeOpencodeProcess(t *testing.T) {
	dir := os.Getenv("FAKE_OC_STATE")
	if dir == "" {
		t.Skip("helper process for the opencode adapter tests")
	}
	args := os.Args[slices.Index(os.Args, "--")+1:]
	os.Exit(fakeOpencode(dir, args))
}

type ocCall struct {
	Args []string
	Env  []string
	Cwd  string
	File string // import: the saved copy of the import JSON
}

func fakeOpencode(dir string, args []string) int {
	cwd, _ := os.Getwd()
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "FAKE_OC_STATE=") {
			env = append(env, kv)
		}
	}
	call := ocCall{Args: args, Env: env, Cwd: cwd}
	db := map[string]map[string]any{}
	dbPath := filepath.Join(dir, "db.json")
	if b, err := os.ReadFile(dbPath); err == nil {
		_ = json.Unmarshal(b, &db)
	}
	defer func() {
		f, _ := os.OpenFile(filepath.Join(dir, "calls.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		b, _ := json.Marshal(call)
		_, _ = f.Write(append(b, '\n'))
		f.Close()
	}()
	if len(args) < 3 || args[0] != "--pure" {
		fmt.Fprintln(os.Stderr, "fake opencode: unexpected args", args)
		return 2
	}
	switch args[1] {
	case "debug": // debug config: config.json if seeded, else no model
		b, err := os.ReadFile(filepath.Join(dir, "config.json"))
		if err != nil {
			b = []byte(`{"$schema":"https://opencode.ai/config.json"}`)
		}
		os.Stdout.Write(b)
		return 0
	case "export":
		s, ok := db[args[2]]
		if !ok {
			fmt.Fprintln(os.Stderr, "Session not found:", args[2])
			return 1
		}
		b, _ := json.MarshalIndent(s, "", "  ")
		os.Stdout.Write(b)
		return 0
	case "import":
		b, err := os.ReadFile(args[2])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		calls, _ := os.ReadFile(filepath.Join(dir, "calls.jsonl"))
		call.File = filepath.Join(dir, fmt.Sprintf("import-%d.json", strings.Count(string(calls), "\n")))
		_ = os.WriteFile(call.File, b, 0o600)
		if fakeOCCountdown(dir, "fail-imports") {
			fmt.Fprintln(os.Stderr, "database is locked")
			return 1
		}
		if fakeOCCountdown(dir, "drop-imports") {
			return 0 // claims success, stores nothing
		}
		var in map[string]any
		if err := json.Unmarshal(b, &in); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		info := in["info"].(map[string]any)
		id := info["id"].(string)
		s := db[id]
		if s == nil {
			s = map[string]any{"info": info, "messages": []any{}}
		}
		msgs := s["messages"].([]any)
		byID := map[string]map[string]any{}
		for _, m := range msgs {
			m := m.(map[string]any)
			byID[m["info"].(map[string]any)["id"].(string)] = m
		}
		for _, m := range in["messages"].([]any) {
			m := m.(map[string]any)
			mid := m["info"].(map[string]any)["id"].(string)
			old := byID[mid]
			if old == nil {
				msgs = append(msgs, m)
				byID[mid] = m
				continue
			}
			have := map[string]bool{}
			for _, p := range old["parts"].([]any) {
				have[p.(map[string]any)["id"].(string)] = true
			}
			for _, p := range m["parts"].([]any) {
				if !have[p.(map[string]any)["id"].(string)] {
					old["parts"] = append(old["parts"].([]any), p)
				}
			}
		}
		sort.Slice(msgs, func(i, j int) bool {
			return msgs[i].(map[string]any)["info"].(map[string]any)["id"].(string) < msgs[j].(map[string]any)["info"].(map[string]any)["id"].(string)
		})
		s["messages"] = msgs
		db[id] = s
		out, _ := json.Marshal(db)
		_ = os.WriteFile(dbPath, out, 0o600)
		return 0
	}
	fmt.Fprintln(os.Stderr, "fake opencode: unknown command", args[1])
	return 2
}

// fakeOCCountdown reports whether the counter file name in dir is above 0, and decrements it.
func fakeOCCountdown(dir, name string) bool {
	p := filepath.Join(dir, name)
	b, err := os.ReadFile(p)
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n <= 0 {
		return false
	}
	_ = os.WriteFile(p, []byte(strconv.Itoa(n-1)), 0o600)
	return true
}

type fakeOC struct {
	bin, dir string
}

// tFakeOC installs a fake opencode and returns a runtime and harness that use it.
// rt.Env carries variables the import must not pass on.
func tFakeOC(t *testing.T) (fakeOC, Runtime, Harness) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f := fakeOC{dir: t.TempDir()}
	f.bin = tFakeBin(t, "opencode", fmt.Sprintf("FAKE_OC_STATE='%s' exec '%s' -test.run='^TestFakeOpencodeProcess$' -- \"$@\"\n", f.dir, exe))
	rt := tStoreRT(t)
	rt.Env = append(rt.Env, "OPENCODE_CONFIG=/x/opencode.json", "AGENT_SWAP_HARNESS=oc", "AGENT_SWAP_DIR=/x", "KEEP_ME=1")
	h := Harness{Name: "oc", Kind: "opencode", Bin: f.bin, Args: []string{"-m", "gateway/glm-5"}}
	rt.Cfg.Harnesses = []Harness{h}
	return f, rt, h
}

func (f fakeOC) seed(t *testing.T, sessions ...map[string]any) {
	t.Helper()
	db := map[string]any{}
	for _, s := range sessions {
		db[s["info"].(map[string]any)["id"].(string)] = s
	}
	writeFile(t, filepath.Join(f.dir, "db.json"), string(mustJSON(t, db)))
}

func (f fakeOC) calls(t *testing.T) []ocCall {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(f.dir, "calls.jsonl"))
	var out []ocCall
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l == "" {
			continue
		}
		var c ocCall
		if err := json.Unmarshal([]byte(l), &c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

func (f fakeOC) imports(t *testing.T) []ocCall {
	t.Helper()
	var out []ocCall
	for _, c := range f.calls(t) {
		if len(c.Args) > 1 && c.Args[1] == "import" {
			out = append(out, c)
		}
	}
	return out
}

// ocMsg is an export message; completed 0 = an assistant message still running.
func ocMsg(id, role string, completed int64, parts ...map[string]any) map[string]any {
	info := map[string]any{"id": id, "sessionID": "ses_r", "role": role, "time": map[string]any{"created": 1}}
	if completed != 0 {
		info["time"] = map[string]any{"created": 1, "completed": completed}
	}
	return map[string]any{"info": info, "parts": parts}
}

func ocText(id, text string) map[string]any {
	return map[string]any{"id": id, "type": "text", "text": text}
}
func ocTool(id, status, cmd, out string) map[string]any {
	st := map[string]any{"status": status, "input": map[string]any{"command": cmd}}
	if status == "error" {
		st["error"] = out
	} else if out != "" {
		st["output"] = out
	}
	return map[string]any{"id": id, "type": "tool", "tool": "bash", "callID": "c" + id, "state": st}
}

func ocSession(msgs ...map[string]any) map[string]any {
	return map[string]any{"info": map[string]any{"id": "ses_r", "title": "t", "directory": "/work/proj"}, "messages": msgs}
}

func oU(s string) Turn { return uT("opencode", s) }
func oA(s string) Turn { return aT("opencode", s) }
func oTool(cmd, out string) Turn {
	return toolTurn("opencode", "bash", `{"command":"`+cmd+`"}`, out)
}

// --- readers ---

func TestOpencodeTurns(t *testing.T) {
	t.Parallel() // each fake opencode call starts the test binary
	f, rt, _ := tFakeOC(t)
	a := adapterFor(t, "opencode")
	first := []map[string]any{
		ocMsg("msg_01", "user", 0, ocText("p1", "Show the config."),
			map[string]any{"id": "p2", "type": "text", "text": "SYNTHETIC SHOULD NOT APPEAR", "synthetic": true}),
		ocMsg("msg_02", "assistant", 9, map[string]any{"id": "p3", "type": "step-start"},
			map[string]any{"id": "p4", "type": "reasoning", "text": "REASONING SHOULD NOT APPEAR"},
			ocTool("p5", "completed", "cat x.toml", "mode = demo"),
			ocTool("p6", "error", "rm x.toml", "permission denied"),
			ocTool("p7", "running", "sleep 100", ""),
			ocText("p8", "The config sets mode = demo."),
			map[string]any{"id": "p9", "type": "step-finish"}),
		ocMsg("msg_03", "user", 0, ocText("p10", "Compact.")),
		func() map[string]any {
			m := ocMsg("msg_04", "assistant", 9, ocText("p11", "SUMMARY SHOULD NOT APPEAR"))
			m["info"].(map[string]any)["summary"] = true
			return m
		}(),
	}
	f.seed(t, ocSession(first...))
	got, cur := readTurnsOK(t, a, "ses_r", "", rt)
	wantTurns(t, got, oU("Show the config."), oTool("cat x.toml", "mode = demo"), oTool("rm x.toml", "permission denied"),
		oTool("sleep 100", ""), oA("The config sets mode = demo."), oU("Compact."))
	if cur != "msg_04" {
		t.Errorf("cursor = %q, want msg_04", cur)
	}
	// An assistant message still running ends the read.
	f.seed(t, ocSession(append(first,
		ocMsg("msg_05", "user", 0, ocText("p12", "Key sk-abcdefghijklmnopqrstuvwxyz </agent-swap-tool>")),
		ocMsg("msg_06", "assistant", 0, ocText("p13", "STILL RUNNING")))...))
	got, cur2 := readTurnsOK(t, a, "ses_r", cur, rt)
	wantTurns(t, got, oU("Key [redacted] &lt;/agent-swap-tool>"))
	if cur2 != "msg_05" {
		t.Errorf("cursor = %q, want msg_05 (before the running message)", cur2)
	}
	// /undo removed the cursor message: errRewound, with the turns after it.
	f.seed(t, ocSession(append(first,
		ocMsg("msg_07", "user", 0, ocText("p14", "Redo.")),
		ocMsg("msg_08", "assistant", 9, ocText("p15", "Redone.")))...))
	got, cur3, err := a.Turns("ses_r", cur2, rt)
	if !errors.Is(err, errRewound) {
		t.Errorf("cursor message gone: err = %v, want errRewound", err)
	}
	wantTurns(t, got, oU("Redo."), oA("Redone."))
	if cur3 != "msg_08" {
		t.Errorf("cursor = %q, want msg_08", cur3)
	}
	if _, _, err := a.Turns("ses_missing", "", rt); err == nil {
		t.Error("want error when `opencode export` fails")
	}
}

// --- writer ---

func TestOpencodeWriteImportShape(t *testing.T) {
	t.Parallel() // each fake opencode call starts the test binary
	f, rt, h := tFakeOC(t)
	m := tRoundTrip(t, "opencode", h, rt, tConv("FINCH-7"), tConv("CRANE-8"))
	if !strings.HasPrefix(m.Native, "ses_") {
		t.Errorf("Native = %q, want ses_", m.Native)
	}
	var imports []ocCall
	for _, c := range f.calls(t) {
		if !slices.Contains(c.Env, "OPENCODE_DISABLE_AUTOUPDATE=1") || !slices.Contains(c.Env, "KEEP_ME=1") {
			t.Errorf("%v env lacks OPENCODE_DISABLE_AUTOUPDATE=1 or the base env: %q", c.Args, c.Env)
		}
		for _, kv := range c.Env {
			if strings.HasPrefix(kv, "OPENCODE_CONFIG=") || strings.HasPrefix(kv, "AGENT_SWAP_") {
				t.Errorf("%v env passes %q", c.Args, kv)
			}
		}
		if want, _ := filepath.EvalSymlinks(rt.Cwd); c.Cwd != want && c.Cwd != rt.Cwd {
			t.Errorf("%v ran in %q, want %q", c.Args, c.Cwd, rt.Cwd)
		}
		if c.Args[1] == "import" {
			imports = append(imports, c)
			if _, err := os.Stat(c.Args[2]); !os.IsNotExist(err) {
				t.Errorf("import file %s left behind (%v)", c.Args[2], err)
			}
		}
	}
	if len(imports) != 2 {
		t.Fatalf("got %d imports, want 2 (create, append)", len(imports))
	}
	maxMsg, maxPrt := "", ""
	for i, c := range imports {
		var in struct {
			Info     map[string]any
			Messages []struct {
				Info  map[string]any
				Parts []map[string]any
			}
		}
		b, _ := os.ReadFile(c.File)
		if err := json.Unmarshal(b, &in); err != nil {
			t.Fatal(err)
		}
		if in.Info["id"] != m.Native {
			t.Errorf("import %d session id %v, want %s", i, in.Info["id"], m.Native)
		}
		if i == 0 && in.Info["directory"] != rt.Cwd {
			t.Errorf("created session directory %v, want %s", in.Info["directory"], rt.Cwd)
		}
		if len(in.Messages) != 4 {
			t.Errorf("import %d has %d messages, want only the 4 new ones", i, len(in.Messages))
		}
		lastUser := ""
		for _, msg := range in.Messages {
			id, _ := msg.Info["id"].(string)
			if !strings.HasPrefix(id, "msg_") || id <= maxMsg {
				t.Errorf("message id %q is not an ascending msg_ id (after %q)", id, maxMsg)
			}
			maxMsg = id
			if msg.Info["sessionID"] != m.Native {
				t.Errorf("message %s sessionID %v", id, msg.Info["sessionID"])
			}
			switch msg.Info["role"] {
			case "user":
				lastUser = id
				if mm, _ := msg.Info["model"].(map[string]any); mm["providerID"] != "gateway" || mm["modelID"] != "glm-5" {
					t.Errorf("user message model %v, want gateway/glm-5 from -m", msg.Info["model"])
				}
			case "assistant":
				if msg.Info["parentID"] != lastUser {
					t.Errorf("assistant %s parentID %v, want %s", id, msg.Info["parentID"], lastUser)
				}
				if tm, _ := msg.Info["time"].(map[string]any); tm["completed"] == nil {
					t.Errorf("assistant %s not completed", id)
				}
			}
			for _, p := range msg.Parts {
				pid, _ := p["id"].(string)
				if !strings.HasPrefix(pid, "prt_") || pid <= maxPrt {
					t.Errorf("part id %q is not an ascending prt_ id (after %q)", pid, maxPrt)
				}
				maxPrt = pid
				if p["messageID"] != id || p["sessionID"] != m.Native {
					t.Errorf("part %s messageID %v sessionID %v, want %s %s", pid, p["messageID"], p["sessionID"], id, m.Native)
				}
			}
		}
	}
}

func TestOpencodeWriteImportFailures(t *testing.T) {
	t.Parallel() // each fake opencode call starts the test binary
	for _, mode := range []string{"fail-imports", "drop-imports"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			f, rt, h := tFakeOC(t)
			writeFile(t, filepath.Join(f.dir, mode), "9")
			if _, err := adapterFor(t, "opencode").Write(h, &Mirror{}, tConv("X"), rt); err == nil {
				t.Errorf("Write must fail when every import %s", mode)
			}
			if n := len(f.imports(t)); n != 2 {
				t.Errorf("ran %d imports, want 2 (one retry)", n)
			}
		})
	}
}

// A failed import (exit status) or one the id check rejects is rerun once with the same file.
func TestOpencodeWriteImportRetry(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"fail-imports", "drop-imports"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			f, rt, h := tFakeOC(t)
			writeFile(t, filepath.Join(f.dir, mode), "1")
			m := &Mirror{}
			n, err := adapterFor(t, "opencode").Write(h, m, tConv("RETRY"), rt)
			if err != nil {
				t.Fatalf("Write after one failed import: %v", err)
			}
			got, _ := readTurnsOK(t, adapterFor(t, "opencode"), m.Native, "", rt)
			if n != len(got) || n != 4 {
				t.Errorf("Write returned %d, read back %d; want 4", n, len(got))
			}
			im := f.imports(t)
			if len(im) != 2 {
				t.Fatalf("ran %d imports, want 2", len(im))
			}
			a, _ := os.ReadFile(im[0].File)
			b, _ := os.ReadFile(im[1].File)
			if im[0].Args[2] != im[1].Args[2] || string(a) != string(b) || len(a) == 0 {
				t.Errorf("retry used a different file: %s (%d bytes) vs %s (%d bytes)", im[0].Args[2], len(a), im[1].Args[2], len(b))
			}
		})
	}
}

func TestOpencodeWriteAppendToNativeSession(t *testing.T) {
	t.Parallel() // each fake opencode call starts the test binary
	f, rt, h := tFakeOC(t)
	native := ocSession(ocMsg(opencodeID("msg", time.Now()), "user", 0, ocText(opencodeID("prt", time.Now()), "Native q.")))
	m0 := ocMsg(opencodeID("msg", time.Now()), "assistant", 9, ocText(opencodeID("prt", time.Now()), "Native a."))
	m0["info"].(map[string]any)["modelID"] = "kimi"
	native["messages"] = append(native["messages"].([]map[string]any), m0)
	f.seed(t, native)
	a := adapterFor(t, "opencode")
	_, cur := readTurnsOK(t, a, "ses_r", "", rt)
	m := &Mirror{Native: "ses_r", Cursor: cur, Owned: true}
	n, err := a.Write(h, m, []Turn{uT("claude", "From claude."), aT("claude", "Answer.")}, rt)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := readTurnsOK(t, a, "ses_r", cur, rt)
	if n != len(got) {
		t.Errorf("Append returned %d, read back %d", n, len(got))
	}
	wantTurns(t, got, oU("From claude."), oA("Answer."))
	all, _ := readTurnsOK(t, a, "ses_r", "", rt)
	wantTurns(t, all, oU("Native q."), oA("Native a."), oU("From claude."), oA("Answer."))
}

// lastImportUserModels returns the user-message models of the last import.
func (f fakeOC) lastImportUserModels(t *testing.T) []ocModel {
	t.Helper()
	im := f.imports(t)
	if len(im) == 0 {
		t.Fatal("no import ran")
	}
	var in struct {
		Messages []struct {
			Info struct {
				Role  string
				Model *ocModel
			}
		}
	}
	b, _ := os.ReadFile(im[len(im)-1].File)
	if err := json.Unmarshal(b, &in); err != nil {
		t.Fatal(err)
	}
	var out []ocModel
	for _, m := range in.Messages {
		if m.Info.Role == "user" {
			if m.Info.Model == nil {
				t.Fatal("user message without a model")
			}
			if m.Info.Model.ProviderID == "agent-swap" {
				t.Errorf("user message carries the placeholder model %v", *m.Info.Model)
			}
			out = append(out, *m.Info.Model)
		}
	}
	return out
}

func TestOpencodeWriteModel(t *testing.T) {
	t.Parallel()
	userWith := func(id string, mm ocModel) map[string]any {
		m := ocMsg(id, "user", 0, ocText(opencodeID("prt", time.Now()), "Native q."))
		m["info"].(map[string]any)["model"] = mm
		return m
	}
	native := func() map[string]any { // a real model, then an agent-swap placeholder (ignored)
		return ocSession(
			userWith(opencodeID("msg", time.Now()), ocModel{"kimi", "k2"}),
			ocMsg(opencodeID("msg", time.Now()), "assistant", 9, ocText(opencodeID("prt", time.Now()), "Native a.")),
			userWith(opencodeID("msg", time.Now()), ocModel{"agent-swap", "import"}),
			ocMsg(opencodeID("msg", time.Now()), "assistant", 9, ocText(opencodeID("prt", time.Now()), "Native b.")))
	}
	cases := []struct {
		name    string
		args    []string
		config  string // debug config output; "" = none
		append  bool
		want    ocModel
		noModel bool
	}{
		{name: "-m wins over the session model", args: []string{"--model=gateway/glm-5"}, append: true, config: `{"model":"cfg/x"}`, want: ocModel{"gateway", "glm-5"}},
		{name: "append uses the session's last real model", append: true, config: `{"model":"cfg/x"}`, want: ocModel{"kimi", "k2"}},
		{name: "create falls back to debug config", config: `{"model":"anthropic/claude/haiku"}`, want: ocModel{"anthropic", "claude/haiku"}},
		{name: "no model anywhere", noModel: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f, rt, h := tFakeOC(t)
			h.Args = c.args
			if c.config != "" {
				writeFile(t, filepath.Join(f.dir, "config.json"), c.config)
			}
			m := &Mirror{}
			if c.append {
				f.seed(t, native())
				m = &Mirror{Native: "ses_r", Owned: true}
			}
			n, err := adapterFor(t, "opencode").Write(h, m, tConv("MODEL"), rt)
			if c.noModel {
				if !errors.Is(err, errNoMirror) || n != 0 || len(f.imports(t)) != 0 {
					t.Errorf("Write = %d, %v (imports %d); want 0, errNoMirror, no import", n, err, len(f.imports(t)))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, got := range f.lastImportUserModels(t) {
				if got != c.want {
					t.Errorf("user model = %v, want %v", got, c.want)
				}
			}
		})
	}
}

// The spike's real opencode 1.19.5 exports (and import files, same shape) read with the current reader.
func TestOpencodeSpikeFixturesRead(t *testing.T) {
	t.Parallel()
	for file, want := range map[string]int{
		"opencode-1.19.5-native-export.json":                     2,
		"opencode-1.19.5-export-after-create-append-resume.json": 8,
		"opencode-1.19.5-import-create.json":                     2,
		"opencode-1.19.5-adversarial-import.json":                4,
	} {
		t.Run(file, func(t *testing.T) {
			t.Parallel()
			b, err := os.ReadFile(filepath.Join("testdata", "spike", "opencode", file))
			if err != nil {
				t.Fatal(err)
			}
			var s map[string]any
			if err := json.Unmarshal(b, &s); err != nil {
				t.Fatal(err)
			}
			f, rt, _ := tFakeOC(t)
			f.seed(t, s)
			ses := s["info"].(map[string]any)["id"].(string)
			msgs := s["messages"].([]any)
			last := msgs[len(msgs)-1].(map[string]any)["info"].(map[string]any)["id"].(string)
			got, cur := readTurnsOK(t, adapterFor(t, "opencode"), ses, "", rt)
			if len(got) != want || cur != last {
				t.Errorf("read %d turns, cursor %s; want %d, %s", len(got), cur, want, last)
			}
		})
	}
}
