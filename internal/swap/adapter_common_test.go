package swap

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// testRT is a Runtime whose Home is an empty temp dir laid out like $HOME.
func testRT(t *testing.T) Runtime {
	t.Helper()
	t.Setenv("CODEX_HOME", "")
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
	cfg := Defaults()
	cfg.ClaudeBin = "/opt/bin/claude"
	cfg.Targets = map[string]Target{"gw": {BaseURL: "https://gw.example/anthropic", KeyCmd: "echo synthetic"}}
	return Runtime{
		Exe: "/opt/bin/agent-swap", Dir: t.TempDir(), Cwd: "/work/proj", StateDir: t.TempDir(),
		CfgPath: "/etc/agent-swap.toml", Home: t.TempDir(), Cfg: cfg, Launched: time.Now(),
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

func TestTailBytes(t *testing.T) {
	const pre = "[…earlier conversation truncated…]\n"
	cases := []struct {
		name, in string
		n        int
		want     string
	}{
		{"short", "a\nb\n", 10, "a\nb\n"},
		{"exact", "a\nb\n", 4, "a\nb\n"},
		{"empty", "", 5, ""},
		{"cut mid-line drops partial line", "line1\nline2\nline3\n", 8, pre + "line3\n"},
		{"cut exactly at line start keeps that line", "aaaa\nbbbb\n", 5, pre + "bbbb\n"},
		{"single long line keeps newest bytes", "abcdefghij", 4, pre + "ghij"},
		{"multibyte text", "héllo\nwörld\n", 8, pre + "wörld\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := TailBytes(c.in, c.n); got != c.want {
				t.Errorf("TailBytes(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
			}
		})
	}
}

func TestTailBytesMultibyteLongLine(t *testing.T) {
	s := strings.Repeat("héllo wörld ✓ ", 50) // one long line of 2- and 3-byte runes
	for n := 1; n < 40; n++ {
		if got := TailBytes(s, n); !utf8.ValidString(got) {
			t.Fatalf("TailBytes(_, %d) = %q is not valid UTF-8", n, got)
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
			if _, err := a.Transcript(id, rt); err == nil {
				t.Errorf("%s Transcript(%q) = nil error", kind, id)
			}
		}
	}
}
