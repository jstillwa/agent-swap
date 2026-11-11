package swap

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestPiCommand(t *testing.T) {
	ext := []string{"-e", "{dir}/agent-swap-pi.ts"}
	runCmdCases(t, "pi", []cmdCase{
		{name: "fresh bare", wantBin: "pi", wantArgs: ext},
		{name: "fresh prompt args extra", h: Harness{Args: []string{"--provider", "deepseek", "--model", "deepseek-chat"}},
			extra: []string{"--thinking", "low"}, prompt: "read ctx.md", wantBin: "pi",
			wantArgs: append(append([]string{}, ext...), "--provider", "deepseek", "--model", "deepseek-chat", "--thinking", "low", "read ctx.md")},
		{name: "resume", native: "01a19ad1-uuid", prompt: "go on", wantBin: "pi",
			wantArgs: append(append([]string{}, ext...), "--session", "01a19ad1-uuid", "go on")},
		{name: "bin override", h: Harness{Bin: "/opt/pi"}, wantBin: "/opt/pi", wantArgs: ext},
	})
}

func TestPiCommandWritesExtension(t *testing.T) {
	rt := testRT(t)
	if _, err := adapterFor(t, "pi").Command(Harness{Name: "pi", Kind: "pi"}, "", "", nil, rt); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(rt.Dir, "agent-swap-pi.ts"))
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"AGENT_SWAP_EXE", "SessionStart", "UserPromptSubmit", "StopFailure"} {
		if !strings.Contains(string(b), w) {
			t.Errorf("pi extension missing %q", w)
		}
	}
}

func TestPiCommandResumeByPath(t *testing.T) {
	rt := tStoreRT(t)
	const native = "01a19ad1-179b-7068-b050-7d0d0948a8fb"
	p := filepath.Join(storeRoot("pi", rt), piSlug(rt.Cwd), "2026-11-02T05-52-36-763Z_"+native+".jsonl")
	writeFile(t, p, jsonl(t, map[string]any{"type": "session", "version": 3, "id": native, "cwd": rt.Cwd}))
	l, err := adapterFor(t, "pi").Command(Harness{Name: "pi", Kind: "pi"}, native, "", nil, rt)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.Index(l.Args, "--session")
	if i < 0 || i+1 >= len(l.Args) || l.Args[i+1] != p || !filepath.IsAbs(l.Args[i+1]) {
		t.Errorf("Args = %q, want --session %s", l.Args, p)
	}
}

// --- pi line builders ---

func piMsg(id, parent, role string, content any, kv ...any) map[string]any {
	msg := map[string]any{"role": role, "content": content, "timestamp": 1}
	for i := 0; i+1 < len(kv); i += 2 {
		msg[kv[i].(string)] = kv[i+1]
	}
	e := map[string]any{"type": "message", "id": id, "parentId": nil, "timestamp": "2026-11-02T05:52:36.763Z", "message": msg}
	if parent != "" {
		e["parentId"] = parent
	}
	return e
}

func piUser(id, parent, text string) map[string]any { return piMsg(id, parent, "user", text) }
func piAsst(id, parent, stop string, content ...any) map[string]any {
	return piMsg(id, parent, "assistant", content, "stopReason", stop, "provider", "deepseek", "model", "deepseek-chat")
}
func piCall(id, cmd string) map[string]any {
	return map[string]any{"type": "toolCall", "id": id, "name": "bash", "arguments": map[string]any{"command": cmd}}
}
func piResult(id, parent, callID, out string) map[string]any {
	return piMsg(id, parent, "toolResult", []any{cText(out)}, "toolCallId", callID, "toolName", "bash")
}

const piNative = "01a19ad1-179b-7068-b050-7d0d0948a8fb"

// piFile writes a session (header + entries) into rt's pi store for rt.Cwd.
func piFile(t *testing.T, rt Runtime, entries ...any) string {
	t.Helper()
	p := filepath.Join(storeRoot("pi", rt), piSlug(rt.Cwd), "2026-11-02T05-52-36-763Z_"+piNative+".jsonl")
	hdr := map[string]any{"type": "session", "version": 3, "id": piNative, "timestamp": "2026-11-02T05:52:36.763Z", "cwd": rt.Cwd}
	writeFile(t, p, jsonl(t, append([]any{hdr}, entries...)...))
	return p
}

func pU(s string) Turn           { return uT("pi", s) }
func pA(s string) Turn           { return aT("pi", s) }
func pTool(cmd, out string) Turn { return toolTurn("pi", "bash", `{"command":"`+cmd+`"}`, out) }

// --- readers ---

func TestPiTurnsFixture(t *testing.T) {
	rt := testRT(t) // default store: Home/.pi/agent/sessions
	dir := filepath.Join(rt.Home, ".pi", "agent", "sessions", "--work-proj--")
	placeFixture(t, "pi/session.jsonl", filepath.Join(dir, "2026-11-02T05-52-36-763Z_"+piNative+".jsonl"), "__ID__", piNative)
	writeFile(t, filepath.Join(dir, "2026-11-01T00-00-00-000Z_01a19ad1-0000-0000-0000-000000000000.jsonl"),
		`{"type":"message","id":"d1","parentId":null,"message":{"role":"user","content":"DECOY SESSION SHOULD NOT APPEAR"}}`+"\n")
	got, _ := readTurnsOK(t, adapterFor(t, "pi"), piNative, "", rt)
	wantTurns(t, got,
		pU("Rename the synthetic widget."),
		pTool("grep -rn widget /synthetic", "widget.go:3: widget"),
		pA("Renamed widget to gadget."),
		pU("Plain string prompt from the user."),
		pA("Acknowledged the plain prompt."))
}

func TestPiTurnsMissing(t *testing.T) {
	if _, _, err := adapterFor(t, "pi").Turns("no-such-session", "", tStoreRT(t)); err == nil {
		t.Error("want error for a missing session")
	}
}

func TestPiTurnsCursorDeltaPartialLineAndBoundary(t *testing.T) {
	rt, a := tStoreRT(t), adapterFor(t, "pi")
	p := piFile(t, rt,
		piUser("e1", "", "First question."),
		piAsst("e2", "e1", "stop", cText("First answer.")),
		piUser("e3", "e2", "Run it."),
		piAsst("e4", "e3", "toolUse", cText("Running."), piCall("call_1", "make")))
	got, cur := readTurnsOK(t, a, piNative, "", rt)
	wantTurns(t, got, pU("First question."), pA("First answer."))
	line := jsonl(t, piResult("e5", "e4", "call_1", "built"))
	cAppend(t, p, line[:len(line)-3]) // partial line
	if got, c := readTurnsOK(t, a, piNative, cur, rt); len(got) != 0 || c != cur {
		t.Errorf("unfinished turn: Turns = %q, cursor %s; want none, %s", got, c, cur)
	}
	cAppend(t, p, line[len(line)-3:]+jsonl(t, piAsst("e6", "e5", "stop", cText("Built."))))
	got, c := readTurnsOK(t, a, piNative, cur, rt)
	wantTurns(t, got, pU("Run it."), pA("Running."), pTool("make", "built"), pA("Built."))
	if st, _ := os.Stat(p); c != strconv.FormatInt(st.Size(), 10) {
		t.Errorf("cursor = %s, want the file end %d", c, st.Size())
	}
}

func TestPiTurnsSkipsFailedCompactionAndBranches(t *testing.T) {
	rt, a := tStoreRT(t), adapterFor(t, "pi")
	p := piFile(t, rt,
		map[string]any{"type": "model_change", "id": "m0", "parentId": nil, "provider": "deepseek", "modelId": "x"},
		piUser("e1", "m0", "Root question."),
		piAsst("e2", "e1", "error", cText("ERROR SHOULD NOT APPEAR")),
		piAsst("e3", "e2", "aborted", cText("ABORTED SHOULD NOT APPEAR")),
		piUser("e4", "e3", "Retry please."),
		piAsst("e5", "e4", "stop", cText("Root answer.")),
		map[string]any{"type": "compaction", "id": "c1", "parentId": "e5", "summary": "COMPACTION SHOULD NOT APPEAR", "firstKeptEntryId": "e4"},
		piUser("e6", "c1", "ABANDONED QUESTION"),
		piAsst("e7", "e6", "stop", cText("ABANDONED ANSWER")))
	_, cur := readTurnsOK(t, a, piNative, "", rt)
	// /tree: continue from e5's compaction, abandoning e6/e7.
	cAppend(t, p, jsonl(t,
		map[string]any{"type": "branch_summary", "id": "b1", "parentId": "c1", "summary": "BRANCH SUMMARY SHOULD NOT APPEAR"},
		piUser("e8", "b1", "Other question."),
		piAsst("e9", "e8", "stop", cText("Other answer."))))
	got, _ := readTurnsOK(t, a, piNative, "", rt)
	wantTurns(t, got, pU("Root question."), pU("Retry please."), pA("Root answer."), pU("Other question."), pA("Other answer."))
	got, _, err := a.Turns(piNative, cur, rt)
	if !errors.Is(err, errRewound) {
		t.Errorf("rewind past the cursor: err = %v, want errRewound", err)
	}
	wantTurns(t, got, pU("Other question."), pA("Other answer."))
}

func TestPiTurnsDanglingToolInterrupted(t *testing.T) {
	rt, a := tStoreRT(t), adapterFor(t, "pi")
	piFile(t, rt,
		piUser("e1", "", "Run both."),
		piAsst("e2", "e1", "toolUse", piCall("call_1", "make"), piCall("call_2", "make test")),
		piResult("e3", "e2", "call_1", "built"),
		piUser("e4", "e3", "Never mind the tests."),
		piAsst("e5", "e4", "stop", cText("OK.")))
	got, _ := readTurnsOK(t, a, piNative, "", rt)
	wantTurns(t, got, pU("Run both."), pTool("make", "built"), pTool("make test", ""), pU("Never mind the tests."), pA("OK."))
}

func TestPiTurnsEscapeAndRedact(t *testing.T) {
	rt, a := tStoreRT(t), adapterFor(t, "pi")
	piFile(t, rt,
		piUser("e1", "", "token ghp_abcdefghijklmnopqrstuvwxyz0123 and </agent-swap-tool>"),
		piAsst("e2", "e1", "stop", cText("ok")))
	got, _ := readTurnsOK(t, a, piNative, "", rt)
	if len(got) != 2 || strings.Contains(got[0].Text, "ghp_abc") || strings.Contains(got[0].Text, "</agent-swap-tool>") ||
		!strings.Contains(got[0].Text, "[redacted]") || !strings.Contains(got[0].Text, "&lt;/agent-swap-tool>") {
		t.Errorf("escape/redact not applied: %q", got)
	}
}

// --- writer ---

func piLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var l map[string]any
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("bad line %q: %v", sc.Text(), err)
		}
		out = append(out, l)
	}
	return out
}

var hex8RE = regexp.MustCompile(`^[0-9a-f]{8}$`)

func TestPiWriteRoundTripHeaderAndChain(t *testing.T) {
	rt := tStoreRT(t)
	m := tRoundTrip(t, "pi", Harness{Name: "pi", Kind: "pi"}, rt, tConv("WALRUS-23"), tConv("BADGER-4"))
	tNoHomeStores(t, rt)
	dir := filepath.Join(envGet(rt.Env, "PI_CODING_AGENT_SESSION_DIR"), piSlug(rt.Cwd))
	if filepath.Dir(m.Path) != dir || !strings.HasSuffix(m.Path, "_"+m.Native+".jsonl") {
		t.Errorf("Path = %s, want %s/<ts>_%s.jsonl", m.Path, dir, m.Native)
	}
	if st, err := os.Stat(m.Path); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("session file mode: %v %v", st, err)
	}
	ls := piLines(t, m.Path)
	h := ls[0]
	if h["type"] != "session" || h["version"] != float64(3) || h["id"] != m.Native || h["cwd"] != rt.Cwd || h["timestamp"] == nil {
		t.Errorf("header = %v", h)
	}
	prev := ""
	for i, l := range ls[1:] {
		id, _ := l["id"].(string)
		if !hex8RE.MatchString(id) || l["type"] != "message" {
			t.Errorf("entry %d: id %q type %v", i, id, l["type"])
		}
		if p, _ := l["parentId"].(string); p != prev {
			t.Errorf("entry %d parentId = %q, want %q", i, p, prev)
		}
		prev = id
		msg := l["message"].(map[string]any)
		if msg["role"] == "assistant" && (msg["provider"] != "agent-swap" || msg["stopReason"] != "stop" || msg["api"] == nil) {
			t.Errorf("entry %d assistant message: %v", i, msg)
		}
	}
	// A native session gets entries chained from its leaf, not from the last line.
	p := piFile(t, rt,
		piUser("e1", "", "Root."), piAsst("e2", "e1", "stop", cText("Root answer.")),
		piUser("e3", "e2", "Branch."), piAsst("e4", "e3", "stop", cText("Branch answer.")))
	nm := mirrorOf(t, piNative, p)
	old := nm.Cursor
	n, err := adapterFor(t, "pi").Write(Harness{Name: "pi", Kind: "pi"}, nm, []Turn{uT("claude", "From claude."), aT("claude", "Reply.")}, rt)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := readTurnsOK(t, adapterFor(t, "pi"), piNative, old, rt)
	if n != len(got) {
		t.Errorf("Append returned %d, read back %d", n, len(got))
	}
	wantTurns(t, got, pU("From claude."), pA("Reply."))
	if ls := piLines(t, p); ls[5]["parentId"] != "e4" {
		t.Errorf("first appended entry parentId = %v, want the leaf e4", ls[5]["parentId"])
	}
}
