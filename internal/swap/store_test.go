package swap

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// realTemp is a temp dir with symlinks resolved (macOS /var → /private/var),
// so paths built under it compare equal to EvalSymlinks results.
func realTemp(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func ino(t *testing.T, p string) (uint64, uint64) {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	return uint64(st.Dev), st.Ino
}

func TestEnvGetLaterWins(t *testing.T) {
	env := []string{"A=1", "B=x=y", "A=2", "C"}
	if envGet(env, "A") != "2" || envGet(env, "B") != "x=y" || envGet(env, "C") != "" || envGet(nil, "A") != "" {
		t.Fatal("envGet")
	}
}

func TestStoreRoot(t *testing.T) {
	rt := Runtime{Home: "/h"}
	want := map[string]string{"claude": "/h/.claude", "pi": "/h/.pi/agent/sessions", "codex": "/h/.codex", "opencode": "", "agy": ""}
	for k, w := range want {
		if got := storeRoot(k, rt); got != w {
			t.Errorf("storeRoot(%s) = %q want %q", k, got, w)
		}
	}
	rt.Env = []string{"CLAUDE_CONFIG_DIR=/c", "PI_CODING_AGENT_SESSION_DIR=/p", "CODEX_HOME=/x"}
	for k, w := range map[string]string{"claude": "/c", "pi": "/p", "codex": "/x"} {
		if got := storeRoot(k, rt); got != w {
			t.Errorf("env storeRoot(%s) = %q want %q", k, got, w)
		}
	}
	rt.Env = []string{"CLAUDE_CONFIG_DIR="} // empty means unset
	if got := storeRoot("claude", rt); got != "/h/.claude" {
		t.Errorf("empty env var: %q", got)
	}
}

func TestSlugs(t *testing.T) {
	if got := claudeSlug("/Users/me/my proj.v2"); got != "-Users-me-my-proj-v2" {
		t.Errorf("claudeSlug = %q", got)
	}
	if got := piSlug("/Users/me/a:b"); got != "--Users-me-a-b--" {
		t.Errorf("piSlug = %q", got)
	}
}

func TestCreateExclWritesPrivateFile(t *testing.T) {
	root := filepath.Join(realTemp(t), "store") // created on first use
	path := filepath.Join(root, "projects", "deep", "s.jsonl")
	dev, ino0, err := createExcl(root, path, []byte("line\n"))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "line\n" {
		t.Fatalf("content %q", b)
	}
	if m := mode(t, path); m != 0o600 {
		t.Errorf("file mode %o, want 600", m)
	}
	for _, d := range []string{filepath.Dir(path), filepath.Dir(filepath.Dir(path))} {
		if m := mode(t, d); m != 0o700 {
			t.Errorf("dir %s mode %o, want 700", d, m)
		}
	}
	if d, i := ino(t, path); d != dev || i != ino0 {
		t.Errorf("identity %d/%d, want %d/%d", dev, ino0, d, i)
	}
	if _, _, err := createExcl(root, path, []byte("again\n")); err == nil {
		t.Fatal("an existing file must be refused")
	}
	if b, _ := os.ReadFile(path); string(b) != "line\n" {
		t.Fatalf("refused create changed the file: %q", b)
	}
}

func TestCreateExclRefusals(t *testing.T) {
	root := realTemp(t)
	outside := realTemp(t)
	victim := filepath.Join(outside, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A symlink where the file goes.
	link := filepath.Join(root, "link.jsonl")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}
	// A symlinked parent dir pointing outside the root.
	if err := os.Symlink(outside, filepath.Join(root, "projects")); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"symlinked file":   link,
		"symlinked parent": filepath.Join(root, "projects", "new.jsonl"),
		"outside the root": filepath.Join(outside, "x", "new.jsonl"),
		"dot-dot escape":   root + "/a/../../" + filepath.Base(outside) + "/new.jsonl",
		"relative path":    "rel/new.jsonl",
	}
	for name, p := range cases {
		if _, _, err := createExcl(root, p, []byte("evil")); err == nil {
			t.Errorf("%s: createExcl(%s) succeeded", name, p)
		}
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Fatalf("victim changed: %q", b)
	}
	for _, p := range []string{filepath.Join(outside, "new.jsonl"), filepath.Join(outside, "x", "new.jsonl"), filepath.Join(outside, "x")} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s was created outside the root", p)
		}
	}
}

// ownedFile creates an owned mirror file under root and returns its mirror.
func ownedFile(t *testing.T, root, native, body string) *Mirror {
	t.Helper()
	path := filepath.Join(root, "s", native+".jsonl")
	dev, ino, err := createExcl(root, path, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return &Mirror{Native: native, Path: path, Cursor: strconv.Itoa(len(body)), Dev: dev, Ino: ino, Owned: true}
}

func TestAppendOwned(t *testing.T) {
	root := realTemp(t)
	m := ownedFile(t, root, "abc", "one\n")
	if err := appendOwned(m, []byte("two\n")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(m.Path); string(b) != "one\ntwo\n" || m.Cursor != "8" {
		t.Fatalf("content %q cursor %s", b, m.Cursor)
	}
	// Anything written past the cursor (not read yet) refuses the append.
	f, _ := os.OpenFile(m.Path, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString("three\n")
	f.Close()
	if err := appendOwned(m, []byte("four\n")); err == nil || m.Cursor != "8" {
		t.Fatalf("grown file must be refused: %v cursor %s", err, m.Cursor)
	}
	if m := mode(t, m.Path); m != 0o600 {
		t.Errorf("mode %o", m)
	}
}

func TestAppendOwnedRefusals(t *testing.T) {
	root := realTemp(t)
	outside := realTemp(t)
	cases := []struct {
		name  string
		setup func(m *Mirror)
	}{
		{"symlink", func(m *Mirror) {
			victim := filepath.Join(outside, "victim")
			_ = os.WriteFile(victim, []byte("one\n"), 0o600)
			_ = os.Remove(m.Path)
			_ = os.Symlink(victim, m.Path)
		}},
		{"hardlink", func(m *Mirror) { _ = os.Link(m.Path, filepath.Join(outside, "hl")) }},
		{"changed inode", func(m *Mirror) {
			_ = os.Remove(m.Path)
			_ = os.WriteFile(m.Path, []byte("one\n"), 0o600)
		}},
		{"shrank below the cursor", func(m *Mirror) { _ = os.Truncate(m.Path, 2) }},
		{"other device/inode recorded", func(m *Mirror) { m.Ino++ }},
		{"bad cursor", func(m *Mirror) { m.Cursor = "x" }},
	}
	for i, c := range cases {
		m := ownedFile(t, root, "s"+strconv.Itoa(i), "one\n")
		c.setup(m)
		before, _ := os.ReadFile(m.Path)
		cursor := m.Cursor
		if err := appendOwned(m, []byte("evil\n")); err == nil {
			t.Errorf("%s: appendOwned succeeded", c.name)
		}
		if after, _ := os.ReadFile(m.Path); string(after) != string(before) {
			t.Errorf("%s: file changed to %q", c.name, after)
		}
		if m.Cursor != cursor {
			t.Errorf("%s: cursor moved to %s", c.name, m.Cursor)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "victim")); string(b) != "one\n" {
		t.Errorf("symlink target written: %q", b)
	}
}

func TestCheckOwned(t *testing.T) {
	root := realTemp(t)
	outside := realTemp(t)
	if err := checkOwned("", &Mirror{}); err != nil {
		t.Fatalf("no file store: %v", err)
	}
	m := ownedFile(t, root, "good", "one\n")
	if err := checkOwned(root, m); err != nil {
		t.Fatalf("intact mirror: %v", err)
	}
	bad := map[string]func(m *Mirror){
		"path not naming the session": func(m *Mirror) { m.Native = "other" },
		"unsafe native":               func(m *Mirror) { m.Native = "../good" },
		"relative path":               func(m *Mirror) { m.Path = "s/good.jsonl" },
		"unclean path":                func(m *Mirror) { m.Path = root + "/s/../s/good.jsonl" },
		"empty path":                  func(m *Mirror) { m.Path = "" },
		"outside the root": func(m *Mirror) {
			p := filepath.Join(outside, "good.jsonl")
			_ = os.WriteFile(p, []byte("one\n"), 0o600)
			m.Path = p
			m.Dev, m.Ino = ino(t, p)
		},
		"symlink": func(m *Mirror) {
			p := filepath.Join(root, "s", "link-good.jsonl")
			_ = os.Symlink(m.Path, p)
			m.Path = p
		},
		"hardlink":      func(m *Mirror) { _ = os.Link(m.Path, filepath.Join(root, "hl-"+filepath.Base(m.Path))) },
		"changed inode": func(m *Mirror) { _ = os.Remove(m.Path); _ = os.WriteFile(m.Path, []byte("one\n"), 0o600) },
		"size drift":    func(m *Mirror) { m.Cursor = "99" },
	}
	i := 0
	for name, mut := range bad {
		i++
		m := ownedFile(t, root, "good"+strconv.Itoa(i), "one\n")
		mut(m)
		if err := checkOwned(root, m); err == nil {
			t.Errorf("%s: checkOwned passed", name)
		}
	}
}

func TestRegularAndWithin(t *testing.T) {
	root := realTemp(t)
	f := filepath.Join(root, "f")
	_ = os.WriteFile(f, nil, 0o600)
	if _, err := regular(f); err != nil {
		t.Fatal(err)
	}
	if _, err := regular(root); err == nil {
		t.Error("a dir is not regular")
	}
	_ = os.Symlink(f, filepath.Join(root, "l"))
	if _, err := regular(filepath.Join(root, "l")); err == nil {
		t.Error("a symlink is not regular")
	}
	if err := within(root, filepath.Join(root, "a")); err != nil {
		t.Error(err)
	}
	if err := within(filepath.Join(root, "sub"), f); err == nil {
		t.Error("root's parent is outside root/sub")
	}
	_ = os.Mkdir(filepath.Join(root, "rootish"), 0o700)
	_ = os.Mkdir(filepath.Join(root, "rootish2"), 0o700)
	if err := within(filepath.Join(root, "rootish"), filepath.Join(root, "rootish2", "x")); err == nil {
		t.Error("a sibling with a common prefix is outside")
	}
}

func TestSessionPath(t *testing.T) {
	home := realTemp(t)
	rt := Runtime{Home: home, Cwd: "/w/proj"}
	put := func(p string) string {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	claudeHere := put(filepath.Join(home, ".claude", "projects", "-w-proj", "c1.jsonl"))
	claudeElse := put(filepath.Join(home, ".claude", "projects", "-hashed-slug", "c2.jsonl"))
	pi := put(filepath.Join(home, ".pi", "agent", "sessions", "--w-proj--", "2026-01-01T00-00-00-000Z_p1.jsonl"))
	codex := put(filepath.Join(home, ".codex", "sessions", "2026", "01", "02", "rollout-2026-01-02T00-00-00-x1.jsonl"))
	for _, c := range []struct{ kind, id, want string }{
		{"claude", "c1", claudeHere}, {"claude", "c2", claudeElse}, {"pi", "p1", pi}, {"codex", "x1", codex},
	} {
		if got, err := sessionPath(c.kind, c.id, rt); err != nil || got != c.want {
			t.Errorf("sessionPath(%s, %s) = %q %v want %q", c.kind, c.id, got, err, c.want)
		}
	}
	for _, c := range []struct{ kind, id string }{
		{"claude", "missing"}, {"claude", "../c1"}, {"claude", ""}, {"claude", "*"}, {"opencode", "ses_1"}, {"agy", "a"},
	} {
		if p, err := sessionPath(c.kind, c.id, rt); err == nil {
			t.Errorf("sessionPath(%s, %q) = %q, want an error", c.kind, c.id, p)
		}
	}
	// A symlinked or hardlinked session file is refused.
	outside := put(filepath.Join(realTemp(t), "x.jsonl"))
	_ = os.Symlink(outside, filepath.Join(home, ".claude", "projects", "-w-proj", "sym.jsonl"))
	_ = os.Link(claudeHere, filepath.Join(home, ".claude", "projects", "-w-proj", "hard.jsonl"))
	for _, id := range []string{"sym", "hard", "c1"} {
		if p, err := sessionPath("claude", id, rt); err == nil {
			t.Errorf("sessionPath(claude, %s) = %q, want a refusal", id, p)
		}
	}
	// A symlinked project dir leading outside the store is refused.
	ext := realTemp(t)
	put(filepath.Join(ext, "esc.jsonl"))
	_ = os.Symlink(ext, filepath.Join(home, ".claude", "projects", "-escape"))
	if p, err := sessionPath("claude", "esc", rt); err == nil {
		t.Errorf("escape via symlinked dir: %q", p)
	}
}

func TestCursorOffset(t *testing.T) {
	if n, err := cursorOffset(""); n != 0 || err != nil {
		t.Fatal(n, err)
	}
	if n, err := cursorOffset("42"); n != 42 || err != nil {
		t.Fatal(n, err)
	}
	if _, err := cursorOffset("msg_1"); err == nil {
		t.Fatal("non-numeric cursor")
	}
}

func TestBornAfter(t *testing.T) {
	dir := realTemp(t)
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		_ = os.WriteFile(p, []byte(body), 0o600)
		return p
	}
	newer := write("n", "{\"type\":\"x\"}\nnot json\n{\"timestamp\":\"2026-01-02T03:04:05.001Z\"}\n")
	older := write("o", "{\"timestamp\":\"2026-01-02T03:04:04.999Z\"}\n{\"timestamp\":\"2030-01-01T00:00:00Z\"}\n")
	equal := write("e", "{\"timestamp\":\"2026-01-02T03:04:05Z\"}\n")
	late := write("z", strings.Repeat("{}\n", 11)+"{\"timestamp\":\"2030-01-01T00:00:00Z\"}\n")
	bad := write("b", "{\"timestamp\":\"yesterday\"}\n")
	for p, want := range map[string]bool{newer: true, older: false, equal: true, late: true, bad: false,
		filepath.Join(dir, "missing"): false} {
		if got := bornAfter(p, at); got != want {
			t.Errorf("bornAfter(%s) = %v want %v", filepath.Base(p), got, want)
		}
	}
}

func TestIDGenerators(t *testing.T) {
	v4 := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	v7 := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		a, b, c := uuid4(), uuid7(), hex8()
		if !v4.MatchString(a) || !v7.MatchString(b) || !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(c) {
			t.Fatalf("formats: %s %s %s", a, b, c)
		}
		if seen[a] || seen[b] {
			t.Fatal("duplicate id")
		}
		seen[a], seen[b] = true, true
		if !safeID(a) || !safeID(b) {
			t.Fatal("generated ids must be safe")
		}
	}
	// uuid7 starts with the current unix ms.
	before := time.Now().UnixMilli()
	u := strings.ReplaceAll(uuid7()[:13], "-", "")
	var ms int64
	for _, ch := range u {
		ms = ms*16 + int64(strings.IndexRune("0123456789abcdef", ch))
	}
	if ms < before || ms > time.Now().UnixMilli() {
		t.Errorf("uuid7 time %d not in [%d, now]", ms, before)
	}
}

func TestOpencodeIDAscending(t *testing.T) {
	re := regexp.MustCompile(`^(ses|msg|prt)_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
	at := time.Now()
	prev := ""
	for i := 0; i < 200; i++ {
		ts := at
		if i%3 == 0 {
			ts = at.Add(-time.Second) // a clock step back must not go back
		}
		id := opencodeID("msg", ts)
		if !re.MatchString(id) {
			t.Fatalf("format: %s", id)
		}
		if id[:16] <= prev {
			t.Fatalf("not ascending: %s after %s", id, prev)
		}
		prev = id[:16]
	}
	if p := opencodeID("prt", at.Add(time.Hour)); !strings.HasPrefix(p, "prt_") || p[4:16] <= prev[4:] {
		t.Fatalf("later time: %s", p)
	}
}
