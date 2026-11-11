package swap

// Regression tests for review round 1 (adversary reproductions ported from
// scratchpad/adversary/mod/internal/swap/adv_test.go).

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// cParallel is native claude's parallel tool batch: each tool_result line is
// parented to its own tool_use line, so only the last result is on the
// leaf→root path.
func cParallel(t *testing.T) string {
	return jsonl(t,
		cUser("u1", "", "Run both."),
		cAsst("a1", "u1", "msgP", "tool_use", []any{cUse("t1", "Bash", "echo one")}),
		cAsst("a2", "a1", "msgP", "tool_use", []any{cUse("t2", "Bash", "echo two")}),
		cUser("r1", "a1", []any{cRes("t1", "OUTPUT-ONE")}),
		cUser("r2", "a2", []any{cRes("t2", "OUTPUT-TWO")}),
		cAsst("a3", "r2", "msgQ", "end_turn", []any{cText("Done.")}))
}

// F1: every parallel result is kept, in order, none rendered as interrupted.
func TestClaudeTurnsParallelResultsOwnParents(t *testing.T) {
	rt, a := tStoreRT(t), adapterFor(t, "claude")
	cFile(t, rt, cNative, cParallel(t))
	got, _ := readTurnsOK(t, a, cNative, "", rt)
	wantTurns(t, got, cU("Run both."), cTool("Bash", "echo one", "OUTPUT-ONE"), cTool("Bash", "echo two", "OUTPUT-TWO"), cA("Done."))
	for _, tt := range got {
		if strings.Contains(tt.Text, interruptedOutput) {
			t.Errorf("answered call rendered as interrupted: %q", tt.Text)
		}
	}
}

// F2: an Append after an answered parallel batch and an end_turn adds no tool_result.
func TestClaudeWriteNoDuplicateToolResult(t *testing.T) {
	rt, a := tStoreRT(t), adapterFor(t, "claude")
	p := cFile(t, rt, cNative, cParallel(t))
	m := mirrorOf(t, cNative, p)
	old := m.Cursor
	n, err := a.Write(Harness{Name: "claude", Kind: "claude"}, m, []Turn{uT("codex", "Next."), aT("codex", "OK.")}, rt)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	for _, id := range []string{"t1", "t2"} {
		if c := strings.Count(string(b), `"tool_use_id":"`+id+`"`); c != 1 {
			t.Errorf("%d tool_results for %s, want 1", c, id)
		}
	}
	got, _ := readTurnsOK(t, a, cNative, old, rt)
	if n != len(got) {
		t.Errorf("Append returned %d, read back %d", n, len(got))
	}
	wantTurns(t, got, cU("Next."), cA("OK."))
}

// F9: bookkeeping lines flushed after the last turn still move the cursor to the end.
func TestClaudeTurnsCursorPassesTrailingBookkeeping(t *testing.T) {
	rt, a := tStoreRT(t), adapterFor(t, "claude")
	p := cFile(t, rt, cNative, jsonl(t,
		cUser("u1", "", "Hi."), cAsst("a1", "u1", "m1", "end_turn", []any{cText("Hello.")})))
	_, cur := readTurnsOK(t, a, cNative, "", rt)
	cAppend(t, p, jsonl(t, cLine("system", "s1", "a1", nil, "subtype", "stop_hook_summary"),
		map[string]any{"type": "last-prompt", "sessionId": "s"}))
	got, cur2 := readTurnsOK(t, a, cNative, cur, rt)
	if st, _ := os.Stat(p); len(got) != 0 || cur2 != strconv.FormatInt(st.Size(), 10) {
		t.Errorf("Turns = %q, cursor %s; want none, the file size %d", got, cur2, st.Size())
	}
}

// Q1: a turn ends at an assistant content line whose stop_reason is not
// "", tool_use or pause_turn.
func TestClaudeTurnsStopReasons(t *testing.T) {
	for stop, done := range map[string]bool{"end_turn": true, "stop_sequence": true, "max_tokens": true, "refusal": true,
		"tool_use": false, "pause_turn": false, "": false} {
		t.Run("stop="+stop, func(t *testing.T) {
			rt, a := tStoreRT(t), adapterFor(t, "claude")
			p := cFile(t, rt, cNative, jsonl(t,
				cUser("u1", "", "Write a lot."), cAsst("a1", "u1", "m1", stop, []any{cText("Cut off here")})))
			got, cur := readTurnsOK(t, a, cNative, "", rt)
			if !done {
				if len(got) != 0 || cur != "" {
					t.Errorf("Turns = %q, cursor %q; want none, \"\"", got, cur)
				}
				return
			}
			wantTurns(t, got, cU("Write a lot."), cA("Cut off here"))
			if st, _ := os.Stat(p); cur != strconv.FormatInt(st.Size(), 10) {
				t.Errorf("cursor = %s, want %d", cur, st.Size())
			}
		})
	}
}

// rt.Final (the read after the child exited) returns the trailing incomplete
// turn too, with the cursor at the end of the file.
func TestClaudeTurnsFinalTakesIncompleteTurn(t *testing.T) {
	rt, a := tStoreRT(t), adapterFor(t, "claude")
	p := cFile(t, rt, cNative, jsonl(t,
		cUser("u1", "", "Hi."), cAsst("a1", "u1", "m1", "end_turn", []any{cText("Hello.")})))
	_, cur := readTurnsOK(t, a, cNative, "", rt)
	cAppend(t, p, jsonl(t, cUser("u2", "a1", "IMPORTANT NEW REQUEST"),
		cAsst("a2", "u2", "m2", "tool_use", []any{cUse("t9", "Bash", "make")})))
	if got, c := readTurnsOK(t, a, cNative, cur, rt); len(got) != 0 || c != cur {
		t.Errorf("live read of an incomplete turn: %q, cursor %s; want none, %s", got, c, cur)
	}
	rt.Final = true
	got, c := readTurnsOK(t, a, cNative, cur, rt)
	wantTurns(t, got, cU("IMPORTANT NEW REQUEST"), cTool("Bash", "make", ""))
	if st, _ := os.Stat(p); c != strconv.FormatInt(st.Size(), 10) {
		t.Errorf("final cursor = %s, want the file size %d", c, st.Size())
	}
}

func TestPiTurnsFinalTakesIncompleteTurn(t *testing.T) {
	rt, a := tStoreRT(t), adapterFor(t, "pi")
	p := piFile(t, rt,
		piUser("e1", "", "Hi."), piAsst("e2", "e1", "stop", cText("Hello.")),
		piUser("e3", "e2", "IMPORTANT NEW REQUEST"), piAsst("e4", "e3", "toolUse", piCall("call_1", "make")))
	got, cur := readTurnsOK(t, a, piNative, "", rt)
	wantTurns(t, got, pU("Hi."), pA("Hello."))
	rt.Final = true
	got, c := readTurnsOK(t, a, piNative, cur, rt)
	wantTurns(t, got, pU("IMPORTANT NEW REQUEST"), pTool("make", ""))
	if st, _ := os.Stat(p); c != strconv.FormatInt(st.Size(), 10) {
		t.Errorf("final cursor = %s, want the file size %d", c, st.Size())
	}
}

func TestCodexTurnsFinalTakesIncompleteTurn(t *testing.T) {
	rt, a := tStoreRT(t), adapterFor(t, "codex")
	const id = "019a0000-0000-7000-8000-00000000f1a1"
	p := tWriteRollout(t, envGet(rt.Env, "CODEX_HOME")+"/sessions", "05-00-00", id, rt.Cwd,
		tEv("task_started", "turn_id", "t1"), tMsg("user", "Hi."), tMsg("assistant", "Hello."), tEv("task_complete", "turn_id", "t1"),
		tEv("task_started", "turn_id", "t2"), tMsg("user", "IMPORTANT NEW REQUEST"),
		tCodexLine("response_item", map[string]any{"type": "function_call", "name": "shell", "call_id": "k1", "arguments": `{"cmd":"make"}`}))
	_, cur := readTurnsOK(t, a, id, "", rt)
	rt.Final = true
	got, c := readTurnsOK(t, a, id, cur, rt)
	wantTurns(t, got, uT("codex", "IMPORTANT NEW REQUEST"), toolTurn("codex", "shell", `{"cmd":"make"}`, ""))
	if st, _ := os.Stat(p); c != strconv.FormatInt(st.Size(), 10) {
		t.Errorf("final cursor = %s, want the file size %d", c, st.Size())
	}
}

// F7: an earlier unfinished assistant message (interrupted) does not stall
// the reader; only the last one, while the child runs, is still running.
func TestOpencodeTurnsEarlierIncompleteDoesNotStall(t *testing.T) {
	t.Parallel()
	f, rt, _ := tFakeOC(t)
	a := adapterFor(t, "opencode")
	f.seed(t, ocSession(
		ocMsg("msg_01", "user", 0, ocText("p1", "First.")),
		ocMsg("msg_02", "assistant", 0, ocText("p2", "Interrupted answer.")),
		ocMsg("msg_03", "user", 0, ocText("p3", "Second.")),
		ocMsg("msg_04", "assistant", 9, ocText("p4", "Second answer.")),
		ocMsg("msg_05", "user", 0, ocText("p5", "Third.")),
		ocMsg("msg_06", "assistant", 0, ocText("p6", "Still running."))))
	got, cur := readTurnsOK(t, a, "ses_r", "", rt)
	wantTurns(t, got, oU("First."), oA("Interrupted answer."), oU("Second."), oA("Second answer."), oU("Third."))
	if cur != "msg_05" {
		t.Errorf("cursor = %q, want msg_05", cur)
	}
	// rt.Final: the child exited, so the last unfinished message is read too.
	rt.Final = true
	got, cur = readTurnsOK(t, a, "ses_r", cur, rt)
	wantTurns(t, got, oA("Still running."))
	if cur != "msg_06" {
		t.Errorf("final cursor = %q, want msg_06", cur)
	}
}

func TestOcSessionDir(t *testing.T) {
	t.Parallel()
	f, rt, _ := tFakeOC(t)
	noDir := ocSession(ocMsg("msg_01", "user", 0, ocText("p1", "x")))
	noDir["info"] = map[string]any{"id": "ses_nodir"}
	f.seed(t, ocSession(ocMsg("msg_01", "user", 0, ocText("p1", "x"))), noDir)
	if d, err := ocSessionDir("ses_r", rt); err != nil || d != "/work/proj" {
		t.Errorf("ocSessionDir = %q, %v; want /work/proj", d, err)
	}
	if _, err := ocSessionDir("ses_nodir", rt); err == nil {
		t.Error("want error for a session without a directory")
	}
	if _, err := ocSessionDir("ses_missing", rt); err == nil {
		t.Error("want error for a missing session")
	}
	if _, err := ocSessionDir("../x", rt); err == nil {
		t.Error("want error for an unsafe id")
	}
}

// F5/F10 (round 3 ruling): Poll discovers a rollout only while the current id
// has none. An id whose rollout exists is kept whatever other rollouts appear
// (parallel sessions, /new); first discovery applies the filters.
func TestCodexPollRediscovery(t *testing.T) {
	const cur, cand = "00000000-0000-4000-8000-0000000000e1", "00000000-0000-4000-8000-0000000000e2"
	now := time.Now()
	ts := func(d time.Duration) string { return now.Add(d).UTC().Format(time.RFC3339Nano) }
	cases := []struct {
		name   string
		haveID bool          // s.NativeID = cur, whose rollout exists
		mtime  time.Duration // cur's mtime, relative to now
		meta   map[string]any
		want   string
	}{
		{"kept over a parallel growing rollout", true, 0, map[string]any{"timestamp": ts(-10 * time.Minute)}, cur},
		{"kept after it went quiet and a newer one started", true, -20 * time.Minute, map[string]any{"timestamp": ts(-10 * time.Minute)}, cur},
		{"first discovery adopts an interactive rollout", false, 0, map[string]any{"timestamp": ts(-10 * time.Minute)}, cand},
		{"first discovery: user thread_source", false, 0, map[string]any{"timestamp": ts(-10 * time.Minute), "thread_source": "user"}, cand},
		{"first discovery skips agent-swap's own", false, 0, map[string]any{"timestamp": ts(-10 * time.Minute), "originator": "agent-swap"}, ""},
		{"first discovery skips codex_exec", false, 0, map[string]any{"timestamp": ts(-10 * time.Minute), "originator": "codex_exec"}, ""},
		{"first discovery skips source exec", false, 0, map[string]any{"timestamp": ts(-10 * time.Minute), "source": "exec"}, ""},
		{"first discovery skips a sub-agent", false, 0,
			map[string]any{"timestamp": ts(-10 * time.Minute), "source": map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{}}}}, ""},
		{"first discovery skips non-user thread_source", false, 0, map[string]any{"timestamp": ts(-10 * time.Minute), "thread_source": "review"}, ""},
		{"first discovery skips another cwd", false, 0, map[string]any{"timestamp": ts(-10 * time.Minute), "cwd": "/elsewhere"}, ""},
		{"first discovery skips a start before launch", false, 0, map[string]any{"timestamp": ts(-2 * time.Hour)}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rt := testRT(t)
			rt.Launched = now.Add(-time.Hour)
			root := filepath.Join(rt.Home, ".codex", "sessions")
			s := Session{}
			if c.haveID {
				pa := tWriteRolloutMeta(t, root, "05-00-00", map[string]any{"id": cur, "cwd": rt.Cwd, "timestamp": ts(-50 * time.Minute)})
				setMtime(t, pa, now.Add(c.mtime))
				s.NativeID = cur
			}
			meta := map[string]any{"id": cand, "cwd": rt.Cwd}
			for k, v := range c.meta {
				meta[k] = v
			}
			tWriteRolloutMeta(t, root, "06-00-00", meta)
			if err := adapterFor(t, "codex").Poll(Harness{Name: "codex", Kind: "codex"}, rt, &s, &Usage{}); err != nil {
				t.Fatal(err)
			}
			if s.NativeID != c.want {
				t.Errorf("NativeID = %q, want %q", s.NativeID, c.want)
			}
		})
	}
}

// A rollout that can no longer be found is replaced by the discovered one.
func TestCodexPollCurrentRolloutGone(t *testing.T) {
	rt := testRT(t)
	const cand = "00000000-0000-4000-8000-0000000000e4"
	tWriteRollout(t, filepath.Join(rt.Home, ".codex", "sessions"), "06-00-00", cand, rt.Cwd)
	s := Session{NativeID: "00000000-0000-4000-8000-0000000000ff"}
	if err := adapterFor(t, "codex").Poll(Harness{Name: "codex", Kind: "codex"}, rt, &s, &Usage{}); err != nil {
		t.Fatal(err)
	}
	if s.NativeID != cand {
		t.Errorf("NativeID = %s, want %s", s.NativeID, cand)
	}
}

// codex Append rebuilds the rollout path from the store and id: a bogus
// m.Path (e.g. a file elsewhere) is never written.
func TestCodexWriteAppendIgnoresBogusPath(t *testing.T) {
	rt := tStoreRT(t)
	h := Harness{Name: "codex", Kind: "codex", Bin: tFakeBin(t, "codex", "echo 'codex-cli 1.0.0'\n")}
	a := adapterFor(t, "codex")
	m := &Mirror{}
	if _, err := a.Write(h, m, tConv("A"), rt); err != nil {
		t.Fatal(err)
	}
	real := m.Path
	decoy := filepath.Join(t.TempDir(), "rollout-x-"+m.Native+".jsonl")
	writeFile(t, decoy, "decoy\n")
	m.Path = decoy
	old := m.Cursor
	n, err := a.Write(h, m, tConv("B"), rt)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(decoy); string(b) != "decoy\n" {
		t.Error("Append wrote to the bogus m.Path")
	}
	if m.Path != real {
		t.Errorf("m.Path = %s, want the rebuilt %s", m.Path, real)
	}
	got, _ := readTurnsOK(t, a, m.Native, old, rt)
	if n != len(got) || n != 4 {
		t.Errorf("Append returned %d, read back %d; want 4", n, len(got))
	}
}
