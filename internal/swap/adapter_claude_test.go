package swap

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestClaudeCommand(t *testing.T) {
	settings := []string{"--settings", "{dir}/claude.settings.json"}
	runCmdCases(t, "claude", []cmdCase{
		{name: "fresh bare", wantBin: "/opt/bin/claude", wantArgs: settings},
		{name: "fresh prompt args extra", h: Harness{Args: []string{"--effort", "high"}}, prompt: "continue the task",
			extra: []string{"--model", "opus"}, wantBin: "/opt/bin/claude",
			wantArgs: append([]string{"--effort", "high", "--model", "opus", "continue the task"}, settings...)},
		{name: "resume", native: "abc-123", prompt: "go on", wantBin: "/opt/bin/claude",
			wantArgs: append([]string{"--resume", "abc-123", "go on"}, settings...)},
		{name: "bin override", h: Harness{Bin: "/usr/local/bin/claude2"}, wantBin: "/usr/local/bin/claude2", wantArgs: settings},
		{name: "settings named after harness", h: Harness{Name: "claude-api", Target: "gw"}, wantBin: "/opt/bin/claude",
			wantArgs: []string{"--settings", "{dir}/claude-api.settings.json"}},
	})
}

func TestClaudeCommandSettingsFile(t *testing.T) {
	for _, c := range []struct {
		name, target string
		api          bool
	}{{"subscription", "", false}, {"api", "gw", true}} {
		t.Run(c.name, func(t *testing.T) {
			rt := testRT(t)
			h := Harness{Name: "c-" + c.name, Kind: "claude", Target: c.target}
			l, err := adapterFor(t, "claude").Command(h, "", "", nil, rt)
			if err != nil {
				t.Fatal(err)
			}
			if len(l.Env) != 0 {
				t.Errorf("claude Env = %q, want none", l.Env)
			}
			b, err := os.ReadFile(filepath.Join(rt.Dir, h.Name+".settings.json"))
			if err != nil {
				t.Fatal(err)
			}
			var s map[string]any
			if err := json.Unmarshal(b, &s); err != nil {
				t.Fatalf("settings not JSON: %v", err)
			}
			if s["hooks"] == nil || s["statusLine"] == nil {
				t.Errorf("settings missing hooks/statusLine: %s", b)
			}
			if !strings.Contains(string(b), rt.Exe) {
				t.Errorf("settings should call %s: %s", rt.Exe, b)
			}
			_, hasKey := s["apiKeyHelper"]
			if hasKey != c.api || strings.Contains(string(b), "https://gw.example/anthropic") != c.api {
				t.Errorf("apiKeyHelper/base URL present = %v, want %v: %s", hasKey, c.api, b)
			}
		})
	}
}

// --- claude line builders ---

func cText(s string) map[string]any { return map[string]any{"type": "text", "text": s} }
func cUse(id, name, input string) map[string]any {
	return map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{"command": input}}
}
func cRes(id, out string) map[string]any {
	return map[string]any{"type": "tool_result", "tool_use_id": id, "content": out}
}

// cLine is a claude session line; kv adds top-level fields.
func cLine(typ, uuid, parent string, msg map[string]any, kv ...any) map[string]any {
	l := map[string]any{"type": typ, "uuid": uuid, "parentUuid": nil, "sessionId": "s", "isSidechain": false,
		"timestamp": "2026-11-10T10:00:00.000Z", "message": msg}
	if parent != "" {
		l["parentUuid"] = parent
	}
	for i := 0; i+1 < len(kv); i += 2 {
		l[kv[i].(string)] = kv[i+1]
	}
	return l
}

func cUser(uuid, parent string, content any, kv ...any) map[string]any {
	return cLine("user", uuid, parent, map[string]any{"role": "user", "content": content}, kv...)
}

// cAsst is an assistant line; stop "" = a block of a message still streaming (null stop_reason).
func cAsst(uuid, parent, msgID, stop string, content any, kv ...any) map[string]any {
	msg := map[string]any{"id": msgID, "type": "message", "role": "assistant", "model": "claude-haiku-5-5",
		"stop_reason": nil, "content": content}
	if stop != "" {
		msg["stop_reason"] = stop
	}
	return cLine("assistant", uuid, parent, msg, kv...)
}

// cFile writes a session file into rt's claude store for rt.Cwd and returns its path.
func cFile(t *testing.T, rt Runtime, native, content string) string {
	t.Helper()
	p := filepath.Join(storeRoot("claude", rt), "projects", claudeSlug(rt.Cwd), native+".jsonl")
	writeFile(t, p, content)
	return p
}

func cAppend(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

const cNative = "11111111-2222-4333-8444-555555555555"

func cU(s string) Turn { return uT("claude", s) }
func cA(s string) Turn { return aT("claude", s) }
func cTool(name, cmd, out string) Turn {
	return toolTurn("claude", name, `{"command":"`+cmd+`"}`, out)
}

// --- readers ---

func TestClaudeTurnsFixture(t *testing.T) {
	rt := testRT(t) // default store: Home/.claude
	rt.Cwd = "/Users/test/My Project/app.v2"
	// Every non-alphanumeric cwd char becomes "-" (matches real ~/.claude/projects dirs).
	p := filepath.Join(rt.Home, ".claude", "projects", "-Users-test-My-Project-app-v2", cNative+".jsonl")
	placeFixture(t, "claude/session.jsonl", p)
	got, cur := readTurnsOK(t, adapterFor(t, "claude"), cNative, "", rt)
	wantTurns(t, got,
		cU("Please list the files in the synthetic project."),
		cTool("Bash", "ls -la /synthetic", "alpha beta gamma"),
		cA("There are three synthetic files."),
		cU("Thanks, now summarize them."),
		cU("Please try again."),
		cA("Summary: alpha, beta, gamma."))
	b, _ := os.ReadFile(p)
	if want := strconv.Itoa(strings.LastIndexByte(string(b), '\n') + 1); cur != want {
		t.Errorf("cursor = %s, want %s (end of the last complete line; only bookkeeping follows the turn)", cur, want)
	}
	for _, tt := range got {
		for _, deny := range []string{"SHOULD NOT APPEAR", "command-name", "local-command-stdout"} {
			if strings.Contains(tt.Text, deny) {
				t.Errorf("turn contains %q: %q", deny, tt.Text)
			}
		}
	}
}

func TestClaudeTurnsMissing(t *testing.T) {
	if _, _, err := adapterFor(t, "claude").Turns("no-such-session", "", tStoreRT(t)); err == nil {
		t.Error("want error for a missing session")
	}
	rt := tStoreRT(t)
	cFile(t, rt, cNative, jsonl(t, cUser("u1", "", "hi")))
	if _, _, err := adapterFor(t, "claude").Turns(cNative, "not-a-number", rt); err == nil {
		t.Error("want error for a bad cursor")
	}
}

func TestClaudeTurnsCursorDeltaAndPartialLine(t *testing.T) {
	rt, a := tStoreRT(t), adapterFor(t, "claude")
	p := cFile(t, rt, cNative, jsonl(t,
		cUser("u1", "", "First question."),
		cAsst("a1", "u1", "msg_1", "end_turn", []any{cText("First answer.")})))
	got, c1 := readTurnsOK(t, a, cNative, "", rt)
	wantTurns(t, got, cU("First question."), cA("First answer."))
	if st, _ := os.Stat(p); c1 != strconv.FormatInt(st.Size(), 10) {
		t.Errorf("cursor = %s, want the file size %d", c1, st.Size())
	}
	if got, c := readTurnsOK(t, a, cNative, c1, rt); len(got) != 0 || c != c1 {
		t.Errorf("no new lines: Turns = %q, cursor %s; want none, %s", got, c, c1)
	}
	// A line still being written (no newline yet) is ignored.
	line := jsonl(t, cUser("u2", "a1", "Second question."))
	cAppend(t, p, line[:len(line)-1])
	if got, c := readTurnsOK(t, a, cNative, c1, rt); len(got) != 0 || c != c1 {
		t.Errorf("partial line: Turns = %q, cursor %s; want none, %s", got, c, c1)
	}
	cAppend(t, p, "\n"+jsonl(t, cAsst("a2", "u2", "msg_2", "end_turn", []any{cText("Second answer.")})))
	got, c2 := readTurnsOK(t, a, cNative, c1, rt)
	wantTurns(t, got, cU("Second question."), cA("Second answer."))
	if c2 == c1 {
		t.Error("cursor did not advance")
	}
}

func TestClaudeTurnsStopAtCompletedTurn(t *testing.T) {
	rt, a := tStoreRT(t), adapterFor(t, "claude")
	done := jsonl(t,
		cUser("u1", "", "First question."),
		cAsst("a1", "u1", "msg_1", "end_turn", []any{cText("First answer.")}))
	p := cFile(t, rt, cNative, done+jsonl(t,
		cUser("u2", "a1", "Run the tests."),
		cAsst("a2", "u2", "msg_2", "", []any{cText("Running them.")}),
		cAsst("a3", "a2", "msg_2", "tool_use", []any{cUse("toolu_1", "Bash", "go test")})))
	got, cur := readTurnsOK(t, a, cNative, "", rt)
	wantTurns(t, got, cU("First question."), cA("First answer."))
	if cur != strconv.Itoa(len(done)) {
		t.Errorf("cursor = %s, want %d (end of the completed turn)", cur, len(done))
	}
	cAppend(t, p, jsonl(t,
		cUser("r1", "a3", []any{cRes("toolu_1", "ok")}),
		cAsst("a4", "r1", "msg_3", "end_turn", []any{cText("Tests pass.")})))
	got, _ = readTurnsOK(t, a, cNative, cur, rt)
	wantTurns(t, got, cU("Run the tests."), cA("Running them."), cTool("Bash", "go test", "ok"), cA("Tests pass."))
}

func TestClaudeTurnsCompaction(t *testing.T) {
	rt, a := tStoreRT(t), adapterFor(t, "claude")
	before := jsonl(t,
		cUser("u1", "", "Question about alpha."),
		cAsst("a1", "u1", "msg_1", "end_turn", []any{cText("Alpha answer.")}))
	p := cFile(t, rt, cNative, before)
	_, cur := readTurnsOK(t, a, cNative, "", rt)
	cAppend(t, p, jsonl(t,
		cLine("system", "cb", "", nil, "subtype", "compact_boundary", "logicalParentUuid", "a1"),
		cUser("cs", "cb", "COMPACT SUMMARY SHOULD NOT APPEAR", "isCompactSummary", true),
		cUser("u2", "cs", "Question about beta."),
		cAsst("a2", "u2", "msg_2", "end_turn", []any{cText("Beta answer.")})))
	got, _, err := a.Turns(cNative, cur, rt)
	if err != nil {
		t.Fatalf("delta across compaction: %v (the pre-compaction leaf must stay on the chain)", err)
	}
	wantTurns(t, got, cU("Question about beta."), cA("Beta answer."))
	got, _ = readTurnsOK(t, a, cNative, "", rt)
	wantTurns(t, got, cU("Question about alpha."), cA("Alpha answer."), cU("Question about beta."), cA("Beta answer."))
}

func TestClaudeTurnsAbandonedBranchAndRewind(t *testing.T) {
	rt, a := tStoreRT(t), adapterFor(t, "claude")
	p := cFile(t, rt, cNative, jsonl(t,
		cUser("u1", "", "Root question."),
		cAsst("a1", "u1", "msg_1", "end_turn", []any{cText("Root answer.")}),
		cUser("u2", "a1", "ABANDONED QUESTION"),
		cAsst("a2", "u2", "msg_2", "end_turn", []any{cText("ABANDONED ANSWER")})))
	_, cur := readTurnsOK(t, a, cNative, "", rt)
	// A rewind: the edited prompt branches off a1; u2/a2 leave the active chain.
	cAppend(t, p, jsonl(t,
		cUser("u3", "a1", "Edited question."),
		cAsst("a3", "u3", "msg_3", "end_turn", []any{cText("Edited answer.")})))
	got, _ := readTurnsOK(t, a, cNative, "", rt)
	wantTurns(t, got, cU("Root question."), cA("Root answer."), cU("Edited question."), cA("Edited answer."))
	got, next, err := a.Turns(cNative, cur, rt)
	if !errors.Is(err, errRewound) {
		t.Errorf("Turns after a rewind past the cursor: err = %v, want errRewound", err)
	}
	wantTurns(t, got, cU("Edited question."), cA("Edited answer."))
	if st, _ := os.Stat(p); next != strconv.FormatInt(st.Size(), 10) {
		t.Errorf("cursor after rewind = %s, want the file end %d", next, st.Size())
	}
}

func TestClaudeTurnsDanglingToolInterrupted(t *testing.T) {
	rt, a := tStoreRT(t), adapterFor(t, "claude")
	cFile(t, rt, cNative, jsonl(t,
		cUser("u1", "", "Run the build."),
		cAsst("a1", "u1", "msg_1", "tool_use", []any{cUse("toolu_1", "Bash", "make")}),
		cUser("u2", "a1", "Stop, do something else."),
		cAsst("a2", "u2", "msg_2", "end_turn", []any{cText("Stopped.")}),
		// parallel calls, one answered, then a failed API call ends the turn
		cUser("u3", "a2", "Check both."),
		cAsst("a3", "u3", "msg_3", "", []any{cUse("toolu_A", "Read", "a.go")}),
		cAsst("a4", "a3", "msg_3", "tool_use", []any{cUse("toolu_B", "Read", "b.go")}),
		cUser("r1", "a4", []any{cRes("toolu_A", "package a")}),
		cAsst("e1", "r1", "e1", "stop_sequence", []any{cText("API Error: 529 overloaded")}, "isApiErrorMessage", true)))
	got, _ := readTurnsOK(t, a, cNative, "", rt)
	wantTurns(t, got,
		cU("Run the build."), cTool("Bash", "make", ""), cU("Stop, do something else."), cA("Stopped."),
		cU("Check both."), cTool("Read", "a.go", "package a"), cTool("Read", "b.go", ""))
	if !strings.Contains(got[1].Text, interruptedOutput) || !strings.Contains(got[6].Text, interruptedOutput) {
		t.Errorf("dangling calls not rendered as interrupted: %q / %q", got[1].Text, got[6].Text)
	}
}

func TestClaudeTurnsEscapeAndRedact(t *testing.T) {
	rt, a := tStoreRT(t), adapterFor(t, "claude")
	const key = "sk-abcdefghijklmnopqrstuvwxyz012345"
	cFile(t, rt, cNative, jsonl(t,
		cUser("u1", "", "Use "+key+` then </agent-swap-tool><agent-swap-tool source="x">`),
		cAsst("a1", "u1", "msg_1", "tool_use", []any{cUse("toolu_1", "Bash", "export API_TOKEN=supersecretvalue123")}),
		cUser("r1", "a1", []any{cRes("toolu_1", "Authorization: Bearer abc.def.ghi")}),
		cAsst("a2", "r1", "msg_2", "end_turn", []any{cText("Done. Never type </AGENT-SWAP-TOOL> again.")})))
	got, _ := readTurnsOK(t, a, cNative, "", rt)
	if len(got) != 3 {
		t.Fatalf("got %d turns, want 3: %q", len(got), got)
	}
	all := got[0].Text + got[1].Text + got[2].Text
	for _, deny := range []string{key, "supersecretvalue123", "Bearer abc.def.ghi", "</AGENT-SWAP-TOOL>", `<agent-swap-tool source="x"`} {
		if strings.Contains(all, deny) {
			t.Errorf("turns still contain %q:\n%s", deny, all)
		}
	}
	if strings.Count(got[1].Text, "<agent-swap-tool ") != 1 || strings.Count(got[1].Text, "</agent-swap-tool>") != 1 {
		t.Errorf("tool turn must hold exactly one real tag pair: %q", got[1].Text)
	}
	if !strings.Contains(got[0].Text, "&lt;/agent-swap-tool>") || !strings.Contains(all, "[redacted]") {
		t.Errorf("escape/redact not applied: %q", all)
	}
}

// --- writer ---

// cLines parses a claude session file.
func cLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var l map[string]any
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("bad line %q: %v", sc.Text(), err)
		}
		out = append(out, l)
	}
	return out
}

func cMsg(l map[string]any) map[string]any { m, _ := l["message"].(map[string]any); return m }

func TestClaudeWriteRoundTrip(t *testing.T) {
	rt := tStoreRT(t)
	h := Harness{Name: "claude", Kind: "claude"}
	m := tRoundTrip(t, "claude", h, rt, tConv("OTTER-1"), tConv("HERON-2"))
	tNoHomeStores(t, rt)
	want := filepath.Join(envGet(rt.Env, "CLAUDE_CONFIG_DIR"), "projects", claudeSlug(rt.Cwd), m.Native+".jsonl")
	if m.Path != want {
		t.Errorf("Path = %s, want %s", m.Path, want)
	}
	st, err := os.Stat(m.Path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", st.Mode().Perm())
	}
	if m.Cursor != strconv.FormatInt(st.Size(), 10) {
		t.Errorf("Cursor = %s, want size %d", m.Cursor, st.Size())
	}
	// chain integrity: one root, each line's parent is the line before it.
	ls := cLines(t, m.Path)
	prev, ids := "", map[string]bool{}
	for i, l := range ls {
		if l["sessionId"] != m.Native || l["cwd"] != rt.Cwd || l["isSidechain"] != false || l["userType"] != "external" {
			t.Errorf("line %d envelope: %v", i, l)
		}
		p, _ := l["parentUuid"].(string)
		if p != prev {
			t.Errorf("line %d parentUuid = %q, want %q", i, p, prev)
		}
		prev = l["uuid"].(string)
		mid, _ := cMsg(l)["id"].(string)
		if l["type"] == "assistant" {
			if !strings.HasPrefix(mid, "msg_") || ids[mid] {
				t.Errorf("line %d message.id %q is not a fresh msg_ id", i, mid)
			}
			ids[mid] = true
			if cMsg(l)["stop_reason"] != "end_turn" || cMsg(l)["model"] != "agent-swap-import" {
				t.Errorf("line %d assistant message: %v", i, cMsg(l))
			}
		}
		if strings.Contains(string(mustJSON(t, l)), `"thinking"`) {
			t.Errorf("line %d holds a thinking block", i)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestClaudeWriteRoleRepair(t *testing.T) {
	rt, a := tStoreRT(t), adapterFor(t, "claude")
	h := Harness{Name: "claude", Kind: "claude"}
	// A create must start with a user turn; a trailing user turn is not readable yet.
	m := &Mirror{}
	n, err := a.Write(h, m, []Turn{aT("pi", "I was mid-way."), uT("pi", "Pending question?")}, rt)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := readTurnsOK(t, a, m.Native, "", rt)
	if n != len(got) {
		t.Errorf("Create returned %d, read back %d", n, len(got))
	}
	wantTurns(t, got, cU(markImported), cA("I was mid-way."))
	// The session now ends with a user turn: an appended user turn gets an interruption marker first.
	old := m.Cursor
	n, err = a.Write(h, m, []Turn{uT("codex", "Follow-up."), aT("codex", "Answer.")}, rt)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = readTurnsOK(t, a, m.Native, old, rt)
	if n != len(got) {
		t.Errorf("Append returned %d, read back %d", n, len(got))
	}
	wantTurns(t, got, cA(markInterrupted), cU("Follow-up."), cA("Answer."))
	// same-role turns are merged into one message
	old = m.Cursor
	if n, err = a.Write(h, m, []Turn{uT("pi", "one"), uT("pi", "two"), aT("pi", "three")}, rt); err != nil || n != 2 {
		t.Fatalf("merge append: n = %d, err = %v; want 2", n, err)
	}
	got, _ = readTurnsOK(t, a, m.Native, old, rt)
	wantTurns(t, got, cU("one\n\ntwo"), cA("three"))
}

// mirrorOf is the runner's view of a native session it owns.
func mirrorOf(t *testing.T, native, path string) *Mirror {
	t.Helper()
	st, err := regular(path)
	if err != nil {
		t.Fatal(err)
	}
	return &Mirror{Native: native, Path: path, Cursor: strconv.FormatInt(st.Size, 10), Dev: uint64(st.Dev), Ino: st.Ino, Owned: true}
}

func TestClaudeWriteRepairsParallelDanglingTools(t *testing.T) {
	rt, a := tStoreRT(t), adapterFor(t, "claude")
	p := cFile(t, rt, cNative, jsonl(t,
		cUser("u1", "", "Check both files."),
		cAsst("a1", "u1", "msg_native", "", []any{map[string]any{"type": "thinking", "thinking": "hmm", "signature": "SIG"}}),
		cAsst("a2", "a1", "msg_native", "", []any{cUse("toolu_A", "Read", "a.go")}),
		cAsst("a3", "a2", "msg_native", "tool_use", []any{cUse("toolu_B", "Read", "b.go")}),
		cAsst("a4", "a3", "msg_native", "tool_use", []any{cUse("toolu_C", "Read", "c.go")}),
		cUser("r1", "a4", []any{cRes("toolu_A", "package a")}),
		cAsst("e1", "r1", "e1", "stop_sequence", []any{cText("API Error: rate limited")}, "isApiErrorMessage", true)))
	before, _ := os.ReadFile(p)
	m := mirrorOf(t, cNative, p)
	old := m.Cursor
	n, err := a.Write(Harness{Name: "claude", Kind: "claude"}, m, []Turn{uT("codex", "Continue on codex's findings."), aT("codex", "Both read.")}, rt)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(p)
	if !strings.HasPrefix(string(after), string(before)) {
		t.Fatal("Append rewrote existing lines")
	}
	got, _, err := a.Turns(cNative, old, rt)
	if err != nil && !errors.Is(err, errRewound) { // chaining past the api-error leaf abandons it
		t.Fatal(err)
	}
	if n != len(got) {
		t.Errorf("Append returned %d, read back %d: %q", n, len(got), got)
	}
	wantTurns(t, got, cA(markInterrupted), cU("Continue on codex's findings."), cA("Both read."))

	ls := cLines(t, p)
	added := ls[7:]
	if p0, _ := added[0]["parentUuid"].(string); p0 != "r1" {
		t.Errorf("first appended line chains from %q, want r1 (never the api-error line)", p0)
	}
	repair := cMsg(added[0])["content"].([]any)
	var repaired []string
	for _, c := range repair {
		c := c.(map[string]any)
		if c["type"] != "tool_result" || c["is_error"] != true {
			t.Errorf("repair block = %v", c)
		}
		repaired = append(repaired, c["tool_use_id"].(string))
	}
	if slices.Sort(repaired); !slices.Equal(repaired, []string{"toolu_B", "toolu_C"}) {
		t.Errorf("repaired %q, want the unanswered toolu_B and toolu_C", repaired)
	}
	prev, ts := "", ""
	for i, l := range added {
		if i > 0 && l["parentUuid"] != prev {
			t.Errorf("appended line %d parent %v, want %s", i, l["parentUuid"], prev)
		}
		prev = l["uuid"].(string)
		if s := l["timestamp"].(string); s <= ts {
			t.Errorf("timestamps not increasing: %s after %s", s, ts)
		} else {
			ts = s
		}
		if l["type"] == "assistant" && cMsg(l)["id"] == "msg_native" {
			t.Error("appended assistant reuses the native message.id")
		}
	}
	// Every tool_use on the active chain now has a result.
	ok := map[string]bool{}
	for _, l := range ls {
		if cs, _ := cMsg(l)["content"].([]any); l["isApiErrorMessage"] != true {
			for _, c := range cs {
				c := c.(map[string]any)
				switch c["type"] {
				case "tool_use":
					ok[c["id"].(string)] = ok[c["id"].(string)] || false
				case "tool_result":
					ok[c["tool_use_id"].(string)] = true
				}
			}
		}
	}
	for id, answered := range ok {
		if !answered {
			t.Errorf("tool_use %s still unanswered", id)
		}
	}
}

func TestClaudeWriteAppendRefusesReplacedFile(t *testing.T) {
	rt, a := tStoreRT(t), adapterFor(t, "claude")
	h := Harness{Name: "claude", Kind: "claude"}
	m := &Mirror{}
	if _, err := a.Write(h, m, tConv("X"), rt); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(m.Path)
	if err := os.Remove(m.Path); err != nil {
		t.Fatal(err)
	}
	writeFile(t, m.Path, string(b)) // same content, new inode
	if _, err := a.Write(h, m, tConv("Y"), rt); err == nil {
		t.Error("Append to a replaced file must fail")
	}
	if got, _ := os.ReadFile(m.Path); string(got) != string(b) {
		t.Error("the replaced file was written")
	}
}

func TestClaudeCommandResume(t *testing.T) {
	runCmdCases(t, "claude", []cmdCase{
		{name: "resume replays only safe extras", native: cNative, extra: []string{"--model", "opus"}, wantBin: "/opt/bin/claude",
			wantArgs: []string{"--resume", cNative, "--model", "opus", "--settings", "{dir}/claude.settings.json"}},
	})
}
