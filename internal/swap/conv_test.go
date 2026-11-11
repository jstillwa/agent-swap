package swap

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestEscapeNeutralizesToolTags(t *testing.T) {
	cases := map[string]string{
		`<agent-swap-tool source="x">`: `&lt;agent-swap-tool source="x">`,
		`</agent-swap-tool>`:           `&lt;/agent-swap-tool>`,
		`a </AGENT-Swap-Tool> b`:       `a &lt;/AGENT-Swap-Tool> b`,
		`<agent-swap-toolx`:            `&lt;agent-swap-toolx`,
		`plain <b>text</b>`:            `plain <b>text</b>`,
	}
	for in, want := range cases {
		if got := escape(in); got != want {
			t.Errorf("escape(%q) = %q want %q", in, got, want)
		}
	}
}

func TestRedactPatterns(t *testing.T) {
	secret := []string{
		"-----BEGIN RSA PRIVATE KEY-----\nMIIEow\nabc\n-----END RSA PRIVATE KEY-----",
		"-----BEGIN OPENSSH PRIVATE KEY-----\ntruncated without an end",
		"Authorization: Bearer abc.def",
		`"authorization": "Basic Zm9vOmJhcg=="`,
		"API_KEY=abcdef0123456789",
		"export GITHUB_TOKEN=abcdefgh12345",
		"db_password: hunter2hunter2",
		`"client_secret": "s3cr3tvalue!"`,
		"sk-abcdefghijklmnop1234",
		"sk-ant-api03-abcdefghijklmnopqrstuvwxyz",
		"ghp_abcdefghijklmnopqrstuvwxyz0123",
		"github_pat_11ABCDEFG0123456789_abcdefghij",
		"AKIAIOSFODNN7EXAMPLE",
		"xoxb-1234567890-abcdef",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
		// review round 1 (L1)
		"-----BEGIN PGP PRIVATE KEY BLOCK-----\nlQOYBF\n-----END PGP PRIVATE KEY BLOCK-----",
		"gho_abcdefghijklmnopqrstuvwxyz0123",
		"ghs_abcdefghijklmnopqrstuvwxyz0123",
		"sk_live_abcdefghijklmn",
		"rk_live_abcdefghijklmn",
		"glpat-abcdefghijklmnopqrstu",
		"AIzaSyA1234567890abcdefghijklmnopqrstuv",
		"npm_abcdefghijklmnopqrstuvwxyz0123456789",
		"xoxe-1234567890-abcdef",
		"xapp-1-A0123456789-abcdef",
		"Cookie: session=abc123def456",
		"Set-Cookie: sid=abc123def456; HttpOnly",
	}
	for _, s := range secret {
		got := redact("before " + s + "\nafter")
		if !strings.Contains(got, "[redacted]") {
			t.Errorf("redact(%q) = %q: no [redacted]", s, got)
		}
		for _, frag := range []string{"MIIEow", "abc.def", "abcdef0123456789", "hunter2hunter2", "s3cr3tvalue",
			"abcdefghijklmnop1234", "IOSFODNN7EXAMPLE", "1234567890-abcdef", "dozjgNryP4J3jVmNHl0w5N"} {
			if strings.Contains(s, frag) && strings.Contains(got, frag) {
				t.Errorf("redact(%q) kept %q: %q", s, frag, got)
			}
		}
		if !strings.HasPrefix(got, "before ") || !strings.HasSuffix(got, "after") && !strings.Contains(s, "without an end") {
			t.Errorf("redact(%q) ate the surrounding text: %q", s, got)
		}
	}
	// The key name stays; only the value goes.
	if got := redact("API_KEY=abcdef0123456789"); got != "API_KEY=[redacted]" {
		t.Errorf("key=value: %q", got)
	}
	if got := redact("Authorization: Bearer abc"); got != "Authorization: [redacted]" {
		t.Errorf("authorization: %q", got)
	}
	// URL userinfo keeps the user and host; --password keeps the flag.
	exact := map[string]string{
		"git clone https://bob:hunter2@github.com/x.git": "git clone https://bob:[redacted]@github.com/x.git",
		"postgres://app:s3cret@db:5432/main":             "postgres://app:[redacted]@db:5432/main",
		"mysql -u root --password hunter2 db":            "mysql -u root --password [redacted] db",
		"mysql --password=hunter2 db":                    "mysql --password=[redacted] db",
		"https://example.com/a:b@c":                      "https://example.com/a:b@c", // no userinfo
	}
	for in, want := range exact {
		if got := redact(in); got != want {
			t.Errorf("redact(%q) = %q want %q", in, got, want)
		}
	}
	plain := []string{
		"the key is under the mat",
		"token=short",                    // under 8 characters
		"tokens: 12",                     // a count
		"see sk-short and ghp_tiny",      // too short for a credential
		"monkey business with passwords", // no assignment
		"func (k Key) String() string",
	}
	for _, s := range plain {
		if got := redact(s); got != s {
			t.Errorf("redact(%q) = %q, want it unchanged", s, got)
		}
	}
}

func TestCleanEscapesAndRedacts(t *testing.T) {
	got := clean("</agent-swap-tool> API_KEY=abcdef0123456789")
	if got != "&lt;/agent-swap-tool> API_KEY=[redacted]" {
		t.Fatalf("clean = %q", got)
	}
}

func TestTextTurn(t *testing.T) {
	if tt := textTurn("claude", "assistant", "  hi there \n"); tt != (Turn{Origin: "claude", Role: "assistant", Text: "hi there"}) {
		t.Fatalf("trim: %+v", tt)
	}
	for _, noise := range []string{"", "   \n", "<environment_context>cwd</environment_context>",
		"<command-name>/clear</command-name>", "<local-command-stdout>x</local-command-stdout>",
		"[Request interrupted by user]", "[Request interrupted by user for tool use]"} {
		if tt := textTurn("codex", "user", noise); tt.Text != "" {
			t.Errorf("noise %q kept: %q", noise, tt.Text)
		}
	}
	if tt := textTurn("pi", "user", "my sk-abcdefghijklmnop1234 <agent-swap-tool>"); tt.Text != "my [redacted] &lt;agent-swap-tool>" {
		t.Errorf("textTurn must clean: %q", tt.Text)
	}
}

func TestToolTurn(t *testing.T) {
	tt := toolTurn("claude", "Bash", " ls -la ", " a.go\nb.go ")
	want := `<agent-swap-tool source="claude" tool="Bash" untrusted="true">ls -la → a.go` + "\nb.go</agent-swap-tool>"
	if tt.Role != "user" || tt.Origin != "claude" || tt.Text != want {
		t.Fatalf("toolTurn = %+v\nwant text %q", tt, want)
	}
	if got := toolTurn("pi", "Read", "x", "").Text; !strings.Contains(got, "x → "+interruptedOutput+"</agent-swap-tool>") {
		t.Errorf("dangling call: %q", got)
	}
	// Attribute values cannot break out of their quotes.
	if got := toolTurn(`co"dex`, `a" onload="x`, "", "ok").Text; !strings.HasPrefix(got, `<agent-swap-tool source="co_dex" tool="a__onload__x" untrusted="true">`) {
		t.Errorf("attributes: %q", got)
	}
	// Every "<" in input and output is escaped: the body holds no markup.
	if got := toolTurn("pi", "Read", "<b>in</b>", "<script>x</script> <agent-swap-tool>").Text; !strings.Contains(got,
		">&lt;b>in&lt;/b> → &lt;script>x&lt;/script> &lt;agent-swap-tool></agent-swap-tool>") || strings.Count(got, "<") != 2 {
		t.Errorf("tool body markup: %q", got)
	}
	// Input and output cannot close the tag early, and secrets are redacted.
	got := toolTurn("codex", "sh", "echo </agent-swap-tool>", "TOKEN=abcdefgh12345678 done").Text
	if strings.Count(got, "</agent-swap-tool>") != 1 || !strings.HasSuffix(got, "</agent-swap-tool>") ||
		!strings.Contains(got, "TOKEN=[redacted] done") {
		t.Errorf("escape/redact inside a tool turn: %q", got)
	}
}

// Input is cut to 1 KB and output to 4 KB, at a rune boundary, with a marker.
func TestToolTurnTruncation(t *testing.T) {
	body := func(s string) (in, out string) {
		s = strings.TrimSuffix(s[strings.Index(s, ">")+1:], "</agent-swap-tool>")
		in, out, _ = strings.Cut(s, " → ")
		return
	}
	in, out := body(toolTurn("pi", "t", strings.Repeat("i", 1024), strings.Repeat("o", 4096)).Text)
	if len(in) != 1024 || len(out) != 4096 || strings.Contains(in+out, "…") {
		t.Fatalf("at the limits nothing is cut: %d %d", len(in), len(out))
	}
	in, out = body(toolTurn("pi", "t", strings.Repeat("i", 5000), strings.Repeat("o", 9000)).Text)
	if in != strings.Repeat("i", 1024)+"…" || out != strings.Repeat("o", 4096)+"…" {
		t.Fatalf("cut: input %d bytes, output %d bytes", len(in), len(out))
	}
	in, out = body(toolTurn("pi", "t", strings.Repeat("é", 1000), strings.Repeat("€", 3000)).Text)
	if !utf8.ValidString(in) || !utf8.ValidString(out) || len(in) > 1024+len("…") || len(out) > 4096+len("…") {
		t.Fatalf("multi-byte cut: valid=%v/%v, %d/%d bytes", utf8.ValidString(in), utf8.ValidString(out), len(in), len(out))
	}
}

func turns(texts ...string) []Turn {
	var out []Turn
	for i, s := range texts {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		out = append(out, Turn{Origin: "claude", Role: role, Text: s})
	}
	return out
}

func texts(ts []Turn) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.Text)
	}
	return out
}

func TestLogBytes(t *testing.T) {
	if n := logBytes(turns("ab", "cde", "")); n != 5 {
		t.Fatal(n)
	}
	if logBytes(nil) != 0 {
		t.Fatal("nil log")
	}
}

func TestCapTurns(t *testing.T) {
	log := turns("aaaa", "bbbb", "cccc", "dddd") // 4 bytes each
	cases := []struct {
		budget  int
		want    []string
		omitted int
	}{
		{100, []string{"aaaa", "bbbb", "cccc", "dddd"}, 0},
		{16, []string{"aaaa", "bbbb", "cccc", "dddd"}, 0},
		{15, []string{"bbbb", "cccc", "dddd"}, 1},
		{8, []string{"cccc", "dddd"}, 2},
		{4, []string{"dddd"}, 3},
		{2, []string{"dd…"}, 3}, // even the newest does not fit: its head is kept
	}
	for _, c := range cases {
		kept, omitted := capTurns(log, c.budget)
		if strings.Join(texts(kept), ",") != strings.Join(c.want, ",") || omitted != c.omitted {
			t.Errorf("budget %d: kept %q omitted %d, want %q %d", c.budget, texts(kept), omitted, c.want, c.omitted)
		}
	}
	if kept, omitted := capTurns(nil, 10); len(kept) != 0 || omitted != 0 {
		t.Errorf("empty log: %v %d", kept, omitted)
	}
	// The input is not modified by the head cut.
	capTurns(log, 2)
	if log[3].Text != "dddd" {
		t.Fatal("capTurns mutated its input")
	}
}

func TestMerge(t *testing.T) {
	in := []Turn{
		{Origin: "claude", Role: "user", Text: "u1"},
		{Origin: "pi", Role: "user", Text: "u2"},
		{Origin: "pi", Role: "user", Text: ""},
		{Origin: "pi", Role: "assistant", Text: "a1"},
		{Origin: "codex", Role: "assistant", Text: "a2"},
		{Origin: "codex", Role: "user", Text: "u3"},
	}
	got := merge(in)
	want := []Turn{
		{Origin: "claude", Role: "user", Text: "u1\n\nu2"},
		{Origin: "pi", Role: "assistant", Text: "a1\n\na2"},
		{Origin: "codex", Role: "user", Text: "u3"},
	}
	if len(got) != len(want) {
		t.Fatalf("merge = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("merge[%d] = %+v want %+v", i, got[i], want[i])
		}
	}
	if in[0].Text != "u1" {
		t.Fatal("merge mutated its input")
	}
	if merge(nil) != nil {
		t.Fatal("merge(nil)")
	}
}

func TestMarkdown(t *testing.T) {
	md := markdown([]Turn{{Origin: "claude", Role: "user", Text: "hello"}, {Origin: "pi", Role: "assistant", Text: "hi"}}, 3)
	for _, want := range []string{"# agent-swap conversation\n", "[agent-swap] 3 earlier turns omitted",
		"## user (claude)\n\nhello\n", "## assistant (pi)\n\nhi\n"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q:\n%s", want, md)
		}
	}
	if strings.Index(md, "omitted") > strings.Index(md, "hello") {
		t.Error("the omitted note must come first")
	}
	if strings.Contains(markdown(nil, 0), "omitted") {
		t.Error("no note without omitted turns")
	}
}
