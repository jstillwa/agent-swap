package swap

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// testRT is a Runtime whose Home is an empty temp dir laid out like $HOME.
func testRT(t *testing.T) Runtime {
	t.Helper()
	cfg := Defaults()
	cfg.ClaudeBin = "/opt/bin/claude"
	cfg.Targets = map[string]Target{"gw": {BaseURL: "https://gw.example/anthropic", KeyCmd: "echo synthetic"}}
	return Runtime{
		Exe: "/opt/bin/agent-swap", Dir: t.TempDir(), Cwd: "/work/proj", StateDir: t.TempDir(),
		CfgPath: "/etc/agent-swap.toml", Home: t.TempDir(), Cfg: cfg, Launched: time.Now(),
	}
}

// tStoreRT is testRT with a real cwd and every file store moved into its own
// temp dir through rt.Env (the way the runner passes the child's env), so no
// test can reach a real ~/.claude, ~/.pi or ~/.codex.
func tStoreRT(t *testing.T) Runtime {
	t.Helper()
	rt := testRT(t)
	rt.Cwd = t.TempDir()
	rt.Env = []string{"PATH=" + os.Getenv("PATH"), "CLAUDE_CONFIG_DIR=" + t.TempDir(),
		"PI_CODING_AGENT_SESSION_DIR=" + t.TempDir(), "CODEX_HOME=" + t.TempDir()}
	return rt
}

// tNoHomeStores fails if a writer used the default store under rt.Home instead of rt.Env's.
func tNoHomeStores(t *testing.T, rt Runtime) {
	t.Helper()
	for _, d := range []string{".claude", ".pi", ".codex"} {
		if _, err := os.Stat(filepath.Join(rt.Home, d)); err == nil {
			t.Errorf("%s created under Home although rt.Env moves the store", d)
		}
	}
}

// jsonl renders values as JSONL lines, each ending in a newline.
func jsonl(t *testing.T, vs ...any) string {
	t.Helper()
	var b strings.Builder
	for _, v := range vs {
		j, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(append(j, '\n'))
	}
	return b.String()
}

func uT(kind, text string) Turn { return Turn{Origin: kind, Role: "user", Text: text} }
func aT(kind, text string) Turn { return Turn{Origin: kind, Role: "assistant", Text: text} }

// wantTurns compares turns exactly (origin, role, text).
func wantTurns(t *testing.T, got []Turn, want ...Turn) {
	t.Helper()
	if reflect.DeepEqual(got, want) || len(got) == 0 && len(want) == 0 {
		return
	}
	var b strings.Builder
	for i := 0; i < max(len(got), len(want)); i++ {
		var g, w Turn
		if i < len(got) {
			g = got[i]
		}
		if i < len(want) {
			w = want[i]
		}
		mark := "  "
		if g != w {
			mark = "!="
		}
		fmt.Fprintf(&b, "%s %d got  %+q\n   %d want %+q\n", mark, i, g, i, w)
	}
	t.Errorf("turns differ (got %d, want %d):\n%s", len(got), len(want), b.String())
}

// readTurnsOK calls Turns and fails on any error.
func readTurnsOK(t *testing.T, a Adapter, native, cursor string, rt Runtime) ([]Turn, string) {
	t.Helper()
	got, cur, err := a.Turns(native, cursor, rt)
	if err != nil {
		t.Fatalf("Turns(%s, %q): %v", native, cursor, err)
	}
	return got, cur
}

// tRoundTrip creates a session holding first, then appends second. Each Write's
// count must equal what Turns reads back from the cursor before that write,
// and the turns must come back unchanged (origin = kind).
func tRoundTrip(t *testing.T, kind string, h Harness, rt Runtime, first, second []Turn) *Mirror {
	t.Helper()
	a := adapterFor(t, kind)
	// Read back, text passes clean() again: a written tool tag comes back
	// escaped, since a reader cannot tell it from one the model typed.
	as := func(ts []Turn) []Turn {
		var out []Turn
		for _, t := range ts {
			if t = textTurn(kind, t.Role, t.Text); t.Text != "" {
				out = append(out, t)
			}
		}
		return out
	}
	m := &Mirror{}
	n, err := a.Write(h, m, first, rt)
	if err != nil {
		t.Fatalf("%s Create: %v", kind, err)
	}
	if m.Native == "" || m.Cursor == "" {
		t.Fatalf("%s Create left mirror unset: %+v", kind, m)
	}
	got, _ := readTurnsOK(t, a, m.Native, "", rt)
	if n != len(got) {
		t.Errorf("%s Create returned %d, Turns read back %d", kind, n, len(got))
	}
	wantTurns(t, got, as(first)...)
	old := m.Cursor
	n, err = a.Write(h, m, second, rt)
	if err != nil {
		t.Fatalf("%s Append: %v", kind, err)
	}
	got, cur := readTurnsOK(t, a, m.Native, old, rt)
	if n != len(got) {
		t.Errorf("%s Append returned %d, Turns(old cursor) read back %d", kind, n, len(got))
	}
	wantTurns(t, got, as(second)...)
	if cur != m.Cursor {
		t.Errorf("%s reader cursor %q != mirror cursor %q after Append", kind, cur, m.Cursor)
	}
	all, _ := readTurnsOK(t, a, m.Native, "", rt)
	wantTurns(t, all, as(append(append([]Turn{}, first...), second...))...)
	if more, _ := readTurnsOK(t, a, m.Native, m.Cursor, rt); len(more) != 0 {
		t.Errorf("%s Turns at the mirror cursor = %q, want none", kind, more)
	}
	return m
}

// tConv is a short alternating conversation with a tool turn, as the runner writes it.
func tConv(tag string) []Turn {
	return []Turn{
		uT("claude", "Remember the codeword "+tag+"."),
		aT("claude", "Noted: "+tag+"."),
		toolTurn("codex", "exec", `{"cmd":"ls"}`, "a.txt b.txt"),
		aT("codex", "Two files for "+tag+"."),
	}
}

func adapterFor(t *testing.T, kind string) Adapter {
	t.Helper()
	a, err := AdapterFor(kind)
	if err != nil || a == nil {
		t.Fatalf("AdapterFor(%q) = %v, %v", kind, a, err)
	}
	return a
}

// placeFixture copies testdata/src to dst (creating parents), applying old→new replacements.
func placeFixture(t *testing.T, src, dst string, repl ...string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", src))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, dst, strings.NewReplacer(repl...).Replace(string(b)))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setMtime(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// assertTranscript checks that want appear in order and none of deny appear.
func assertTranscript(t *testing.T, got string, want, deny []string) {
	t.Helper()
	pos := 0
	for _, w := range want {
		i := strings.Index(got[pos:], w)
		if i < 0 {
			t.Errorf("transcript missing (or out of order) %q\n--- transcript ---\n%s", w, got)
			continue
		}
		pos += i + len(w)
	}
	for _, d := range deny {
		if strings.Contains(got, d) {
			t.Errorf("transcript must not contain %q\n--- transcript ---\n%s", d, got)
		}
	}
}

type cmdCase struct {
	name           string
	h              Harness
	native, prompt string
	extra          []string
	wantBin        string
	wantArgs       []string
}

// runCmdCases checks Bin and Args exactly; dir-dependent args use the "{dir}" placeholder.
func runCmdCases(t *testing.T, kind string, cases []cmdCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rt := testRT(t)
			c.h.Kind = kind
			if c.h.Name == "" {
				c.h.Name = kind
			}
			l, err := adapterFor(t, kind).Command(c.h, c.native, c.prompt, c.extra, rt)
			if err != nil {
				t.Fatal(err)
			}
			if l.Bin != c.wantBin {
				t.Errorf("Bin = %q, want %q", l.Bin, c.wantBin)
			}
			want := make([]string, len(c.wantArgs))
			for i, a := range c.wantArgs {
				want[i] = strings.ReplaceAll(a, "{dir}", rt.Dir)
			}
			if (len(l.Args) > 0 || len(want) > 0) && !reflect.DeepEqual(l.Args, want) {
				t.Errorf("Args =\n  %q\nwant\n  %q", l.Args, want)
			}
		})
	}
}

func requireSqlite(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 CLI not installed")
	}
}

func sqliteExec(t *testing.T, db, sql string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(db), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sqlite3", db, sql).CombinedOutput(); err != nil {
		t.Fatalf("sqlite3: %v: %s", err, out)
	}
}

func TestAdapterForKinds(t *testing.T) {
	quits := map[string]string{"claude": "/exit", "codex": "/quit", "pi": "/quit", "opencode": "/exit", "agy": "/quit"}
	for kind, q := range quits {
		if got := adapterFor(t, kind).Quit(); got != q {
			t.Errorf("%s Quit() = %q, want %q", kind, got, q)
		}
	}
	for _, kind := range Kinds {
		if _, ok := quits[kind]; !ok {
			t.Errorf("kind %q has no adapter test", kind)
		}
	}
	if a, err := AdapterFor("gemini"); err == nil || a != nil {
		t.Errorf("AdapterFor(gemini) = %v, %v; want nil, error", a, err)
	}
}

func TestHookDrivenPollIsNoop(t *testing.T) {
	for _, kind := range []string{"claude", "pi", "opencode"} {
		rt := testRT(t)
		s := Session{NativeID: "n1", Idle: true, RateLimited: true, StartedAt: 5, UpdatedAt: 6}
		u := Usage{Windows: map[string]*Window{"5h": {UsedPct: 50, ResetsAt: 99}}}
		before, beforeU := s, *u.Windows["5h"]
		if err := adapterFor(t, kind).Poll(Harness{Name: kind, Kind: kind}, rt, &s, &u); err != nil {
			t.Errorf("%s Poll: %v", kind, err)
		}
		if s != before || len(u.Windows) != 1 || *u.Windows["5h"] != beforeU {
			t.Errorf("%s Poll changed state: %+v %+v", kind, s, u)
		}
	}
}

func TestSafeID(t *testing.T) {
	for _, id := range []string{"019a0000-0000-7000-8000-00000000f1f1", "ses_abc123", "conv-1"} {
		if !safeID(id) {
			t.Errorf("safeID(%q) = false, want true", id)
		}
	}
	for _, id := range []string{"-x", "--flag", "..", ".x", "a..b", "a/b", "../x", "a b", "a\nb"} {
		if safeID(id) {
			t.Errorf("safeID(%q) = true, want false", id)
		}
	}
}

func TestUnsafeNativeIDRejected(t *testing.T) {
	for _, kind := range Kinds {
		for _, id := range []string{"--flag", "../x", "a/b"} {
			rt := testRT(t)
			a := adapterFor(t, kind)
			if _, err := a.Command(Harness{Name: kind, Kind: kind}, id, "", nil, rt); err == nil {
				t.Errorf("%s Command(native=%q) = nil error", kind, id)
			}
			if _, _, err := a.Turns(id, "", rt); err == nil {
				t.Errorf("%s Turns(%q) = nil error", kind, id)
			}
		}
	}
}

// TestSpikeFixturesRead reads every live-recorded Phase 0 session (testdata/spike,
// owned by the spike) from the start with the current readers. Counts were
// verified by hand against each file; atEnd = the cursor is the file size.
func TestSpikeFixturesRead(t *testing.T) {
	const id = "019a0000-0000-7000-8000-000000000001"
	cases := []struct {
		file  string
		turns int
		atEnd bool
	}{
		{"claude/claude-2.1.322-created.jsonl", 2, true},
		{"claude/claude-2.1.322-created-after-resume.jsonl", 4, true},
		{"claude/claude-2.1.322-fork-session.jsonl", 6, true},
		{"claude/claude-2.1.322-native-thinking-before-append.jsonl", 2, true},
		{"claude/claude-2.1.322-native-thinking-after-append-resume.jsonl", 10, true},
		{"claude/claude-2.1.322-api-error-leaf-chain-err-after-resume.jsonl", 7, true},
		{"claude/claude-2.1.322-api-error-leaf-chain-real-after-resume.jsonl", 8, true},
		{"claude/claude-2.1.322-dangling-parallel-repaired-after-resume.jsonl", 7, true},
		{"claude/claude-2.1.322-dangling-parallel-unrepaired-after-resume.jsonl", 5, true},
		{"claude/claude-2.1.322-tui-turn-with-stop-hook.jsonl", 2, true},
		{"claude/claude-2.1.322-adversarial-input.jsonl", 4, true},
		{"claude/claude-2.1.322-created-minimal-envelope.jsonl", 0, false}, // no stop_reason: no completed turn
		{"pi/pi-1.5.1-created.jsonl", 2, true},
		{"pi/pi-1.5.1-created-append-after-resume.jsonl", 10, true},
		{"pi/pi-1.5.1-adversarial-input.jsonl", 4, true},
		{"codex/codex-0.166.0-created.jsonl", 2, true},
		{"codex/codex-0.166.0-created-resume-append-resume.jsonl", 10, true},
		{"codex/codex-0.166.0-native-append-resume.jsonl", 6, true},
		{"codex/codex-0.166.0-native-exec.jsonl", 2, true},
		{"codex/codex-0.166.0-adversarial-input.jsonl", 4, true},
	}
	seen := map[string]bool{}
	for _, c := range cases {
		seen[c.file] = true
		t.Run(filepath.Base(c.file), func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("testdata", "spike", c.file))
			if err != nil {
				t.Fatal(err)
			}
			kind, _, _ := strings.Cut(c.file, "/")
			rt := tStoreRT(t)
			root := storeRoot(kind, rt)
			p := map[string]string{
				"claude": filepath.Join(root, "projects", claudeSlug(rt.Cwd), id+".jsonl"),
				"pi":     filepath.Join(root, piSlug(rt.Cwd), "2026-11-10T00-00-00-000Z_"+id+".jsonl"),
				"codex":  filepath.Join(root, "sessions", "2026", "11", "10", "rollout-2026-11-10T00-00-00-"+id+".jsonl"),
			}[kind]
			writeFile(t, p, string(b))
			got, cur := readTurnsOK(t, adapterFor(t, kind), id, "", rt)
			if len(got) != c.turns {
				t.Errorf("read %d turns, want %d", len(got), c.turns)
			}
			for i := 1; i < len(got); i++ {
				if got[i].Role == "assistant" && got[i-1].Role == "assistant" {
					t.Errorf("turns %d and %d are both assistant", i-1, i)
				}
			}
			if want := strconv.Itoa(len(b)); c.atEnd && cur != want {
				t.Errorf("cursor = %s, want the file size %s", cur, want)
			}
			if !c.atEnd && cur != "" {
				t.Errorf("cursor = %q, want unchanged \"\"", cur)
			}
		})
	}
	all, _ := filepath.Glob(filepath.Join("testdata", "spike", "*", "*.jsonl"))
	for _, f := range all {
		if rel, _ := filepath.Rel(filepath.Join("testdata", "spike"), f); !seen[rel] {
			t.Errorf("spike fixture %s has no expected count here", rel)
		}
	}
}
