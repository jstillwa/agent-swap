package swap

import (
	"bytes"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// unitRunner is a Runner for calling its conversation methods directly: a
// private log buffer, a temp run dir, and a temp home holding the stores.
type unitRunner struct {
	*Runner
	out *bytes.Buffer
	rt  Runtime
}

func newUnitRunner(t *testing.T, order ...Harness) *unitRunner {
	t.Helper()
	home := realTemp(t)
	out := &bytes.Buffer{}
	cfg := Defaults()
	cfg.Notify = false
	cfg.StateDir = filepath.Join(home, "state")
	r := &Runner{Cfg: cfg, Order: order, Dir: realTemp(t), Log: log.New(out, "", 0), started: time.Now().Add(-time.Minute)}
	r.c = conv{mirrors: map[string]*Mirror{}, sent: map[string]bool{}, paused: map[string]bool{}}
	return &unitRunner{r, out, Runtime{Home: home, Cwd: "/w/proj", Started: r.started}}
}

func (u *unitRunner) logged(t *testing.T, s string) {
	t.Helper()
	if !strings.Contains(u.out.String(), s) {
		t.Errorf("run log lacks %q:\n%s", s, u.out.String())
	}
}

// scripted is an Adapter whose Turns and Write are canned.
type scripted struct {
	noPoll
	turns map[string][]Turn // native → turns (cursor = count read)
	err   error
	write func(m *Mirror, turns []Turn) (int, error)
}

func (scripted) Command(Harness, string, string, []string, Runtime) (Launch, error) {
	return Launch{}, nil
}
func (scripted) Quit() string { return "/quit" }

func (s *scripted) Turns(native, cursor string, rt Runtime) ([]Turn, string, error) {
	all := s.turns[native]
	from := 0
	if cursor != "" {
		from = int(cursor[0] - '0')
	}
	if s.err != nil && !errors.Is(s.err, errRewound) {
		return nil, cursor, s.err
	}
	return all[min(from, len(all)):], string(rune('0' + len(all))), s.err
}

func (s *scripted) Write(h Harness, m *Mirror, turns []Turn, rt Runtime) (int, error) {
	return s.write(m, turns)
}

func say(origin string, texts ...string) []Turn {
	var out []Turn
	for i, s := range texts {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		out = append(out, Turn{Origin: origin, Role: role, Text: s})
	}
	return out
}

var piH = Harness{Name: "pi", Kind: "pi"}

func TestConvAddSkipsOwnPromptsAndBlanks(t *testing.T) {
	c := conv{sent: map[string]bool{"[agent-swap] continue": true}}
	c.add("pi", []Turn{
		{Origin: "x", Role: "user", Text: "[agent-swap] continue"},
		{Origin: "x", Role: "assistant", Text: "[agent-swap] continue"}, // only user turns are prompts
		{Origin: "x", Role: "user", Text: ""},
		{Origin: "x", Role: "user", Text: "real"},
	})
	if len(c.log) != 2 || c.log[0].Role != "assistant" || c.log[1].Text != "real" || c.log[0].Origin != "pi" || c.log[1].Origin != "pi" {
		t.Fatalf("log %+v", c.log)
	}
}

func TestIngestReadsAfterCursor(t *testing.T) {
	u := newUnitRunner(t)
	ad := &scripted{turns: map[string][]Turn{"ses_1": say("opencode", "q1", "a1")}}
	h := Harness{Name: "oc", Kind: "opencode"}
	u.c.sent["q1"] = true
	u.ingest(h, ad, u.rt, Session{NativeID: "ses_1"}, false)
	m := u.c.mirrors["opencode"]
	if len(u.c.log) != 1 || u.c.log[0].Text != "a1" || m.Native != "ses_1" || m.Cursor != "2" || m.Synced != 1 || !m.Owned {
		t.Fatalf("first ingest: log %+v mirror %+v", u.c.log, m)
	}
	ad.turns["ses_1"] = append(ad.turns["ses_1"], say("opencode", "q2", "a2")...)
	u.ingest(h, ad, u.rt, Session{NativeID: "ses_1"}, false)
	if strings.Join(texts(u.c.log), ",") != "a1,q2,a2" || m.Cursor != "4" || m.Synced != 3 {
		t.Fatalf("second ingest: %q %+v", texts(u.c.log), m)
	}
	before := u.out.String()
	u.ingest(h, ad, u.rt, Session{NativeID: "ses_1"}, false) // nothing new
	if len(u.c.log) != 3 {
		t.Fatalf("re-ingest duplicated turns: %q", texts(u.c.log))
	}
	if u.out.String() != before {
		t.Errorf("an ingest with nothing new must log nothing: %q", strings.TrimPrefix(u.out.String(), before))
	}
	u.logged(t, "ingest oc: +1 turns")
	u.logged(t, "ingest oc: +2 turns")
	if st, _ := Load[map[string]MirrorStatus](MirrorsPath(u.Dir)); st["opencode"].Native != "ses_1" {
		t.Errorf("mirrors.json: %+v", st)
	}
	// No id yet, or a paused kind: nothing is read.
	u.ingest(h, ad, u.rt, Session{}, false)
	u.c.paused["opencode"] = true
	ad.turns["ses_1"] = append(ad.turns["ses_1"], say("opencode", "q3")...)
	u.ingest(h, ad, u.rt, Session{NativeID: "ses_1"}, false)
	if len(u.c.log) != 3 {
		t.Fatalf("paused kind was read: %q", texts(u.c.log))
	}
}

// The opencode session the first launch resumed is not ours to append to.
func TestIngestForeignFirstSessionNotOwned(t *testing.T) {
	u := newUnitRunner(t)
	u.c.foreign = true
	u.ingest(Harness{Name: "oc", Kind: "opencode"}, &scripted{turns: map[string][]Turn{}}, u.rt, Session{NativeID: "ses_9"}, false)
	if m := u.c.mirrors["opencode"]; m.Owned {
		t.Fatalf("a resumed pre-existing session must not be owned: %+v", m)
	}
}

func TestIngestAgyOnlyAtExit(t *testing.T) {
	u := newUnitRunner(t)
	ad := &scripted{turns: map[string][]Turn{"c1": say("agy", "", "[lossy agy transcript] hi")}}
	h := Harness{Name: "agy", Kind: "agy"}
	u.ingest(h, ad, u.rt, Session{NativeID: "c1"}, false)
	if len(u.c.log) != 0 {
		t.Fatal("agy read before exit")
	}
	u.ingest(h, ad, u.rt, Session{NativeID: "c1"}, true)
	if len(u.c.log) != 1 || u.c.log[0].Origin != "agy" {
		t.Fatalf("agy at exit: %+v", u.c.log)
	}
}

func TestIngestErrors(t *testing.T) {
	u := newUnitRunner(t)
	h := Harness{Name: "oc", Kind: "opencode"}
	ad := &scripted{turns: map[string][]Turn{"ses_1": say("opencode", "q1", "a1")}, err: errors.New("boom")}
	u.ingest(h, ad, u.rt, Session{NativeID: "ses_1"}, false)
	if len(u.c.log) != 0 || u.c.mirrors["opencode"].Cursor != "" {
		t.Fatalf("an error must not move the cursor: %+v", u.c.mirrors["opencode"])
	}
	u.logged(t, "ingest oc: boom")
	// A rewind is logged; the turns read still count.
	ad.err = errRewound
	u.ingest(h, ad, u.rt, Session{NativeID: "ses_1"}, false)
	if len(u.c.log) != 2 || u.c.mirrors["opencode"].Cursor != "2" {
		t.Fatalf("rewound: %q %+v", texts(u.c.log), u.c.mirrors["opencode"])
	}
	u.logged(t, "sync: opencode rewound before cursor")
}

// A new native id starts an epoch: the log restarts from the new session and
// the other mirrors are dropped. An unreadable one pauses sync instead.
func TestIngestEpoch(t *testing.T) {
	u := newUnitRunner(t)
	h := Harness{Name: "oc", Kind: "opencode"}
	ad := &scripted{turns: map[string][]Turn{"ses_1": say("opencode", "q1", "a1"), "ses_2": say("opencode", "fresh", "start")}}
	u.ingest(h, ad, u.rt, Session{NativeID: "ses_1"}, false)
	u.c.mirrors["pi"] = &Mirror{Native: "p1", Synced: 2}
	u.ingest(h, ad, u.rt, Session{NativeID: "ses_2"}, false)
	if strings.Join(texts(u.c.log), ",") != "fresh,start" || len(u.c.mirrors) != 1 {
		t.Fatalf("epoch: log %q mirrors %v", texts(u.c.log), u.c.mirrors)
	}
	if m := u.c.mirrors["opencode"]; m.Native != "ses_2" || m.Cursor != "2" || m.Synced != 2 || m.Owned {
		t.Fatalf("epoch mirror %+v (an adopted epoch session is not owned)", m)
	}
	u.logged(t, "epoch: opencode switched to session ses_2")

	ad.err = errors.New("no such session")
	u.ingest(h, ad, u.rt, Session{NativeID: "ses_3"}, false)
	if !u.c.paused["opencode"] || u.c.mirrors["opencode"].Native != "ses_2" || len(u.c.log) != 2 {
		t.Fatalf("rejected epoch must pause and keep state: %v %+v", u.c.paused, u.c.mirrors["opencode"])
	}
	u.logged(t, "sync paused: opencode session ses_3 unreadable")
}

// A file-store epoch needs the session file in the kind's store, recording
// this cwd: a file not written yet is retried, another cwd pauses sync.
func TestIngestEpochNeedsStoreFile(t *testing.T) {
	u := newUnitRunner(t)
	ad := &scripted{turns: map[string][]Turn{"a": say("pi", "q", "a"), "b": say("pi", "q2", "a2"), "c": say("pi", "q3", "a3")}}
	u.c.mirrors["pi"] = &Mirror{Native: "a", Cursor: "2", Synced: 0}
	u.ingest(piH, ad, u.rt, Session{NativeID: "b"}, false)
	if u.c.paused["pi"] || len(u.c.log) != 0 || u.c.mirrors["pi"].Native != "a" {
		t.Fatalf("a session with no file yet must be retried, not adopted or paused: %v %q", u.c.paused, texts(u.c.log))
	}
	u.logged(t, "sync: pi session b not ready")
	dir := filepath.Join(storeRoot("pi", u.rt), piSlug(u.rt.Cwd))
	_ = os.MkdirAll(dir, 0o700)
	_ = os.WriteFile(filepath.Join(dir, "2026-01-01T00-00-00-000Z_c.jsonl"), []byte(`{"type":"session","id":"c","cwd":"/elsewhere"}`+"\n"), 0o600)
	u.ingest(piH, ad, u.rt, Session{NativeID: "c"}, false)
	if !u.c.paused["pi"] || len(u.c.log) != 0 {
		t.Fatalf("a session for another cwd must pause sync: %v %q", u.c.paused, texts(u.c.log))
	}
	u.logged(t, "sync paused: pi session c rejected")
}

func TestWriteSelfCheck(t *testing.T) {
	u := newUnitRunner(t)
	ad := &scripted{turns: map[string][]Turn{}}
	ad.write = func(m *Mirror, turns []Turn) (int, error) {
		m.Native = "n1"
		ad.turns["n1"] = turns[:1] // reads back fewer than written
		return 2, nil
	}
	if err := u.write(ad, piH, &Mirror{}, say("pi", "q", "a"), u.rt); err == nil || !strings.Contains(err.Error(), "self-check") {
		t.Fatalf("short read-back must fail: %v", err)
	}
	ad.write = func(m *Mirror, turns []Turn) (int, error) { m.Native = "n2"; ad.turns["n2"] = turns; return 2, nil }
	if err := u.write(ad, piH, &Mirror{}, say("pi", "q", "a"), u.rt); err != nil {
		t.Fatal(err)
	}
	errDisk := errors.New("disk full")
	ad.write = func(m *Mirror, turns []Turn) (int, error) { return 0, errDisk }
	if err := u.write(ad, piH, &Mirror{Native: "n2"}, nil, u.rt); !errors.Is(err, errDisk) {
		t.Fatalf("writer error: %v", err)
	}
}

func piTurnsOf(t *testing.T, u *unitRunner, native string) []string {
	t.Helper()
	got, _, err := piAdapter{}.Turns(native, "", u.rt)
	if err != nil {
		t.Fatal(err)
	}
	return texts(got)
}

func TestMaterializeEmptyLogWritesNothing(t *testing.T) {
	u := newUnitRunner(t)
	if err := u.materialize(piH, u.rt); err != nil || len(u.c.mirrors) != 0 {
		t.Fatalf("%v %v", err, u.c.mirrors)
	}
	if _, err := os.Stat(storeRoot("pi", u.rt)); err == nil {
		t.Fatal("store touched for an empty log")
	}
}

// No mirror: Create a session with the whole log; then Append only the turns
// it lacks, never the target kind's own.
func TestMaterializeCreateThenAppend(t *testing.T) {
	u := newUnitRunner(t)
	u.c.add("claude", say("claude", "q1", "a1"))
	if err := u.materialize(piH, u.rt); err != nil {
		t.Fatal(err)
	}
	m := u.c.mirrors["pi"]
	if m == nil || !m.Owned || m.Synced != 2 || m.Base != 0 || m.Native == "" {
		t.Fatalf("created mirror %+v", m)
	}
	if got := piTurnsOf(t, u, m.Native); strings.Join(got, ",") != "q1,a1" {
		t.Fatalf("created session: %q", got)
	}
	if mode(t, m.Path) != 0o600 || mode(t, filepath.Dir(m.Path)) != 0o700 {
		t.Errorf("modes %o %o", mode(t, m.Path), mode(t, filepath.Dir(m.Path)))
	}
	u.logged(t, "sync: pi created session "+m.Native)
	native, path := m.Native, m.Path

	// pi's own turn (already native there) and claude's next turns.
	u.c.add("pi", say("pi", "q2", "a2"))
	u.c.add("claude", say("claude", "q3", "a3"))
	if err := u.materialize(piH, u.rt); err != nil {
		t.Fatal(err)
	}
	m = u.c.mirrors["pi"]
	if m.Native != native || m.Path != path || m.Synced != 6 {
		t.Fatalf("append must keep the session: %+v", m)
	}
	if got := piTurnsOf(t, u, native); strings.Join(got, ",") != "q1,a1,q3,a3" {
		t.Fatalf("appended session (pi's own turns not re-written): %q", got)
	}
	u.logged(t, "sync: pi appended 2 turns to "+native)

	// Nothing new from other kinds: nothing written.
	before, _ := os.ReadFile(path)
	u.c.add("pi", say("pi", "q4", "a4"))
	if err := u.materialize(piH, u.rt); err != nil || u.c.mirrors["pi"].Synced != 8 {
		t.Fatalf("no delta: %v %+v", err, u.c.mirrors["pi"])
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatal("no delta must not write")
	}
}

// Create instead of Append when the mirror is not owned, over budget, or its
// file is no longer the one recorded.
func TestMaterializeFallsBackToCreate(t *testing.T) {
	cases := []struct {
		name, log string
		mut       func(u *unitRunner, m *Mirror)
	}{
		{"not owned", "not created during this run", func(u *unitRunner, m *Mirror) { m.Owned = false }},
		{"over budget", "over budget", func(u *unitRunner, m *Mirror) { u.Cfg.ContextMaxBytes = 5 }},
		{"replaced file", "file was replaced", func(u *unitRunner, m *Mirror) {
			b, _ := os.ReadFile(m.Path)
			_ = os.Remove(m.Path)
			_ = os.WriteFile(m.Path, b, 0o600)
		}},
		{"shrunk file", "is not the cursor", func(u *unitRunner, m *Mirror) { _ = os.Truncate(m.Path, 10) }},
		{"grown file", "is not the cursor", func(u *unitRunner, m *Mirror) {
			f, _ := os.OpenFile(m.Path, os.O_APPEND|os.O_WRONLY, 0)
			_, _ = f.WriteString("{}\n")
			f.Close()
		}},
		{"hardlinked file", "single-link", func(u *unitRunner, m *Mirror) { _ = os.Link(m.Path, m.Path+".hl") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := newUnitRunner(t)
			u.c.add("claude", say("claude", "q1", "a1"))
			if err := u.materialize(piH, u.rt); err != nil {
				t.Fatal(err)
			}
			old := *u.c.mirrors["pi"]
			oldBody, _ := os.ReadFile(old.Path)
			c.mut(u, u.c.mirrors["pi"])
			u.c.add("claude", say("claude", "q2", "a2"))
			if err := u.materialize(piH, u.rt); err != nil {
				t.Fatal(err)
			}
			m := u.c.mirrors["pi"]
			if m.Native == old.Native || !m.Owned || m.Synced != 4 {
				t.Fatalf("want a new session: old %+v new %+v", old, m)
			}
			u.logged(t, "cannot append to "+old.Native)
			u.logged(t, c.log)
			if c.name == "not owned" || c.name == "over budget" {
				if now, _ := os.ReadFile(old.Path); !bytes.Equal(now, oldBody) {
					t.Error("the old session must not be written")
				}
			}
		})
	}
}

// A capped Create keeps the newest turns behind an omitted-turns note and
// records Base for the next budget check.
func TestMaterializeCapsCreate(t *testing.T) {
	u := newUnitRunner(t)
	u.c.add("claude", say("claude", "OLD-Q", "OLD-A", "mid-q", "mid-a", "new-q", "new-a"))
	h := piH
	h.ContextMaxBytes = 22 // fits the last four turns (5+5+5+5 bytes)
	if err := u.materialize(h, u.rt); err != nil {
		t.Fatal(err)
	}
	m := u.c.mirrors["pi"]
	got := piTurnsOf(t, u, m.Native)
	if strings.Join(got, ",") != "[agent-swap] 2 earlier turns omitted\n\nmid-q,mid-a,new-q,new-a" || m.Base != 2 {
		t.Fatalf("capped session %q base %d", got, m.Base)
	}
}

// agy has no mirror: it gets a private context file and a resume prompt.
func TestTakeoverContextFileForAgy(t *testing.T) {
	u := newUnitRunner(t)
	u.c.add("claude", say("claude", "q1", "CLAUDE-SAID"))
	agy := Harness{Name: "agy", Kind: "agy"}
	prompt := u.takeover(agy, "claude", u.rt, 3)
	file := filepath.Join(u.Dir, "context-3.md")
	if !strings.Contains(prompt, file) || !strings.Contains(prompt, "from claude to agy") {
		t.Fatalf("prompt %q", prompt)
	}
	if b, _ := os.ReadFile(file); !strings.Contains(string(b), "CLAUDE-SAID") {
		t.Fatalf("context file: %q", b)
	}
	if mode(t, file) != 0o600 {
		t.Errorf("context file mode %o", mode(t, file))
	}
	u.logged(t, "sync: context file (no mirror support)")
	if m := u.c.mirrors["agy"]; m == nil || m.Native != "" || m.Synced != 2 {
		t.Fatalf("the fresh session holds the log so far: %+v", m)
	}
	// A mirror-capable target with a working store gets no prompt.
	if p := u.takeover(piH, "claude", u.rt, 4); p != "" {
		t.Fatalf("pi takeover prompt %q", p)
	}
	if _, err := os.Stat(filepath.Join(u.Dir, "context-4.md")); err == nil {
		t.Fatal("no context file when the mirror was written")
	}
}

// A writer that fails twice falls back to the context file.
func TestTakeoverContextFileOnWriterError(t *testing.T) {
	u := newUnitRunner(t)
	u.c.add("claude", say("claude", "q1", "a1"))
	blocker := filepath.Join(u.rt.Home, "not-a-dir")
	_ = os.WriteFile(blocker, nil, 0o600)
	u.rt.Env = []string{"PI_CODING_AGENT_SESSION_DIR=" + blocker}
	prompt := u.takeover(piH, "claude", u.rt, 1)
	if !strings.Contains(prompt, filepath.Join(u.Dir, "context-1.md")) {
		t.Fatalf("prompt %q", prompt)
	}
	if n := strings.Count(u.out.String(), "sync: pi create:"); n != 2 {
		t.Errorf("want one Create retry (2 attempts), got %d:\n%s", n, u.out.String())
	}
	u.logged(t, "sync: context file (")
}

// claudeLines writes a claude session file in the cwd's project dir (or dir).
func claudeFile(t *testing.T, u *unitRunner, dir, native, body string) string {
	t.Helper()
	if dir == "" {
		dir = filepath.Join(storeRoot("claude", u.rt), "projects", claudeSlug(u.rt.Cwd))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, native+".jsonl")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func cl(typ, id, parent, cwd, ts, content string) string {
	p := `null`
	if parent != "" {
		p = `"` + parent + `"`
	}
	stop := ""
	if typ == "assistant" {
		stop = `,"stop_reason":"end_turn"`
		if strings.HasPrefix(content, "TOOL") {
			stop = `,"stop_reason":"tool_use"`
			return `{"type":"assistant","uuid":"` + id + `","parentUuid":` + p + `,"cwd":"` + cwd + `","timestamp":"` + ts +
				`","message":{"role":"assistant"` + stop + `,"content":[{"type":"tool_use","id":"t9","name":"Bash","input":{"command":"ls"}}]}}` + "\n"
		}
	}
	return `{"type":"` + typ + `","uuid":"` + id + `","parentUuid":` + p + `,"cwd":"` + cwd + `","timestamp":"` + ts +
		`","message":{"role":"` + typ + `"` + stop + `,"content":[{"type":"text","text":"` + content + `"}]}}` + "\n"
}

const oldTS = "2020-01-01T00:00:00.000Z"

// F3b (adversary TestAdvInflightPromptAtExit): after the child exits, the
// read with rt.Final keeps the prompt of the turn it was killed in.
func TestIngestFinalKeepsInflightPrompt(t *testing.T) {
	u := newUnitRunner(t)
	h := Harness{Name: "claude", Kind: "claude"}
	cwd := u.rt.Cwd
	path := claudeFile(t, u, "", "s4", cl("user", "u1", "", cwd, oldTS, "hi")+cl("assistant", "a1", "u1", cwd, oldTS, "hello"))
	u.ingest(h, claudeAdapter{}, u.rt, Session{NativeID: "s4"}, false)
	if strings.Join(texts(u.c.log), ",") != "hi,hello" {
		t.Fatalf("first turn: %q", texts(u.c.log))
	}
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString(cl("user", "u2", "a1", cwd, oldTS, "IMPORTANT NEW REQUEST") + cl("assistant", "a2", "u2", cwd, oldTS, "TOOL"))
	f.Close()
	u.ingest(h, claudeAdapter{}, u.rt, Session{NativeID: "s4"}, false)
	if len(u.c.log) != 2 {
		t.Fatalf("a live read must stop at the completed turn: %q", texts(u.c.log))
	}
	final := u.rt
	final.Final = true
	u.ingest(h, claudeAdapter{}, final, Session{NativeID: "s4"}, true)
	got := strings.Join(texts(u.c.log), "|")
	if !strings.Contains(got, "IMPORTANT NEW REQUEST") || !strings.Contains(got, interruptedOutput) {
		t.Fatalf("the exit read must keep the in-flight prompt and its dangling tool: %q", got)
	}
	if fi, _ := os.Stat(path); u.c.mirrors["claude"].Cursor != fmtSize(fi.Size()) {
		t.Errorf("cursor %s, want the end of the file %d", u.c.mirrors["claude"].Cursor, fi.Size())
	}
}

func fmtSize(n int64) string { return strconv.FormatInt(n, 10) }

// L2: claude's forked first session is owned, but not the session it was
// forked from (when claude reports the --resume id itself).
func TestAdoptForkedOwnership(t *testing.T) {
	u := newUnitRunner(t)
	cwd := u.rt.Cwd
	claudeFile(t, u, "", "forked", cl("user", "u1", "", cwd, oldTS, "hi")) // pre-run timestamps (copied history)
	claudeFile(t, u, "", "orig", cl("user", "u1", "", cwd, oldTS, "hi"))
	u.c.forked, u.c.resumed = true, "orig"
	h := Harness{Name: "claude", Kind: "claude"}
	u.ingest(h, claudeAdapter{}, u.rt, Session{NativeID: "orig"}, false)
	if m := u.c.mirrors["claude"]; m.Native != "orig" || m.Owned {
		t.Fatalf("the --resume session must not be owned: %+v", m)
	}
	u.c.mirrors = map[string]*Mirror{}
	u.ingest(h, claudeAdapter{}, u.rt, Session{NativeID: "forked"}, false)
	if m := u.c.mirrors["claude"]; m.Native != "forked" || !m.Owned {
		t.Fatalf("the forked session must be owned: %+v", m)
	}
	u.c.mirrors, u.c.forked = map[string]*Mirror{}, false
	u.ingest(h, claudeAdapter{}, u.rt, Session{NativeID: "forked"}, false)
	if m := u.c.mirrors["claude"]; m.Owned {
		t.Fatalf("an old session without a fork is not owned: %+v", m)
	}
}

// M1: an epoch rejects a session recorded for another cwd, and (claude) a
// file outside the cwd's own project dir.
func TestEpochVerifiesCwd(t *testing.T) {
	u := newUnitRunner(t)
	h := Harness{Name: "claude", Kind: "claude"}
	cwd := u.rt.Cwd
	claudeFile(t, u, "", "s1", cl("user", "u1", "", cwd, oldTS, "q")+cl("assistant", "a1", "u1", cwd, oldTS, "a"))
	u.ingest(h, claudeAdapter{}, u.rt, Session{NativeID: "s1"}, false)
	claudeFile(t, u, "", "other-cwd", cl("user", "u1", "", "/elsewhere", oldTS, "x")+cl("assistant", "a1", "u1", "/elsewhere", oldTS, "y"))
	u.ingest(h, claudeAdapter{}, u.rt, Session{NativeID: "other-cwd"}, false)
	if !u.c.paused["claude"] || u.c.mirrors["claude"].Native != "s1" {
		t.Fatalf("another cwd must pause: %v %+v", u.c.paused, u.c.mirrors["claude"])
	}
	u.logged(t, "sync paused: claude session other-cwd rejected")

	u.c.paused = map[string]bool{}
	elsewhere := filepath.Join(storeRoot("claude", u.rt), "projects", "-some-other-project")
	claudeFile(t, u, elsewhere, "moved", cl("user", "u1", "", cwd, oldTS, "x")+cl("assistant", "a1", "u1", cwd, oldTS, "y"))
	u.ingest(h, claudeAdapter{}, u.rt, Session{NativeID: "moved"}, false)
	if !u.c.paused["claude"] || len(u.c.log) != 2 {
		t.Fatalf("an epoch outside the cwd's project dir must pause: %v %q", u.c.paused, texts(u.c.log))
	}
	u.logged(t, "not in this cwd's project dir")
}

// Q2: unread compares the file size with the size at the last ingest, and an
// ingest that reads nothing new does not rewrite mirrors.json.
func TestUnreadAndQuietIngest(t *testing.T) {
	u := newUnitRunner(t)
	h := Harness{Name: "claude", Kind: "claude"}
	cwd := u.rt.Cwd
	path := claudeFile(t, u, "", "s1", cl("user", "u1", "", cwd, oldTS, "q")+cl("assistant", "a1", "u1", cwd, oldTS, "a"))
	u.ingest(h, claudeAdapter{}, u.rt, Session{NativeID: "s1"}, false)
	if u.unread("claude") {
		t.Fatal("nothing unread right after an ingest")
	}
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString(`{"type":"system","subtype":"stop_hook_summary","uuid":"s9","parentUuid":"a1"}` + "\n")
	f.Close()
	if !u.unread("claude") {
		t.Fatal("a grown file is unread")
	}
	_ = os.Remove(MirrorsPath(u.Dir))
	u.ingest(h, claudeAdapter{}, u.rt, Session{NativeID: "s1"}, false)
	if u.unread("claude") {
		t.Fatal("bookkeeping lines must not stay unread after an ingest")
	}
	if len(u.c.log) != 2 {
		t.Fatalf("bookkeeping added turns: %q", texts(u.c.log))
	}
	u.ingest(h, claudeAdapter{}, u.rt, Session{NativeID: "s1"}, false)
	if _, err := os.Stat(MirrorsPath(u.Dir)); err == nil && u.c.mirrors["claude"].Cursor == "" {
		t.Fatal("cursor lost")
	}
	_ = os.Remove(MirrorsPath(u.Dir))
	u.ingest(h, claudeAdapter{}, u.rt, Session{NativeID: "s1"}, false)
	if _, err := os.Stat(MirrorsPath(u.Dir)); err == nil {
		t.Error("an ingest with nothing new must not rewrite mirrors.json")
	}
}

// A failed self-check leaves the mirror as it was (the cursor does not advance).
func TestWriteRestoresMirrorOnFailedSelfCheck(t *testing.T) {
	u := newUnitRunner(t)
	ad := &scripted{turns: map[string][]Turn{"n1": say("pi", "q")}}
	ad.write = func(m *Mirror, turns []Turn) (int, error) { m.Cursor, m.Ino = "9", 42; return 3, nil }
	m := &Mirror{Native: "n1", Cursor: "1", Ino: 7, Owned: true}
	if err := u.write(ad, piH, m, say("pi", "q", "a"), u.rt); err == nil {
		t.Fatal("want a self-check error")
	}
	if m.Cursor != "1" || m.Ino != 7 {
		t.Fatalf("mirror changed by a failed write: %+v", m)
	}
}

// Q5: agy goes straight to the context file, without create attempts.
func TestMaterializeAgyNoCreate(t *testing.T) {
	u := newUnitRunner(t)
	u.c.add("claude", say("claude", "q", "a"))
	if err := u.materialize(Harness{Name: "agy", Kind: "agy"}, u.rt); !errors.Is(err, errNoMirror) {
		t.Fatalf("agy: %v", err)
	}
	if strings.Contains(u.out.String(), "create") {
		t.Errorf("agy must not try a create:\n%s", u.out.String())
	}
}

// L3: when the context file cannot be written, the prompt names no path.
func TestTakeoverContextFileWriteFails(t *testing.T) {
	u := newUnitRunner(t)
	u.c.add("claude", say("claude", "q", "a"))
	file := filepath.Join(u.Dir, "context-2.md")
	_ = os.WriteFile(file, []byte("taken"), 0o600) // O_EXCL fails
	if p := u.takeover(Harness{Name: "agy", Kind: "agy"}, "claude", u.rt, 2); p != "" {
		t.Fatalf("prompt %q must not name an unwritten file", p)
	}
	if b, _ := os.ReadFile(file); string(b) != "taken" {
		t.Fatal("an existing file was overwritten")
	}
	u.logged(t, "context file: ")
}
