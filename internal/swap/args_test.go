package swap

import (
	"strings"
	"testing"
)

// F2: launch-only args (prompt, continue/resume, session selection, print)
// are dropped on relaunch; everything else is replayed verbatim.
func TestReplayArgs(t *testing.T) {
	cases := []struct {
		in, want []string
	}{
		{nil, nil},
		{[]string{"--model", "haiku"}, []string{"--model", "haiku"}},
		{[]string{"--model", "haiku", "fix bug"}, []string{"--model", "haiku"}},
		{[]string{"fix bug", "--model", "haiku"}, []string{"--model", "haiku"}},
		{[]string{"--model=haiku", "fix bug"}, []string{"--model=haiku"}},
		{[]string{"--dangerously-skip-permissions", "fix bug"}, []string{"--dangerously-skip-permissions"}},
		{[]string{"--append-system-prompt", "be terse", "fix"}, []string{"--append-system-prompt", "be terse"}},
		{[]string{"--permission-mode", "plan", "--effort", "high"}, []string{"--permission-mode", "plan", "--effort", "high"}},
		// continue / resume
		{[]string{"-c", "--model", "haiku"}, []string{"--model", "haiku"}},
		{[]string{"--continue"}, nil},
		{[]string{"-r", "--model", "x"}, []string{"--model", "x"}},
		{[]string{"-r", "abc", "--model", "x"}, []string{"--model", "x"}},
		{[]string{"--resume", "abc", "--model", "x"}, []string{"--model", "x"}},
		{[]string{"--resume=abc", "--model", "x"}, []string{"--model", "x"}},
		{[]string{"--model", "x", "--resume"}, []string{"--model", "x"}},
		// session selection
		{[]string{"--session-id", "1111-2222", "--model", "x"}, []string{"--model", "x"}},
		{[]string{"--session-id=1111-2222"}, nil},
		{[]string{"--fork-session", "-r", "abc"}, nil},
		// print mode and its prompt
		{[]string{"-p", "hello", "--output-format", "json"}, []string{"--output-format", "json"}},
		{[]string{"--print", "--output-format=json", "hello"}, []string{"--output-format=json"}},
		// variadic flags consume following non-dash tokens
		{[]string{"--add-dir", "a", "b", "--model", "x", "prompt"}, []string{"--add-dir", "a", "b", "--model", "x"}},
		{[]string{"--allowedTools", "Bash", "Edit"}, []string{"--allowedTools", "Bash", "Edit"}},
		{[]string{"--mcp-config", "a.json", "b.json", "--strict-mcp-config"}, []string{"--mcp-config", "a.json", "b.json", "--strict-mcp-config"}},
		{[]string{"--add-dir=a", "fix bug"}, []string{"--add-dir=a"}},
		// other launch-only flags with an optional value
		{[]string{"--from-pr", "123", "--model", "x"}, []string{"--model", "x"}},
		{[]string{"--teleport", "--model", "x"}, []string{"--model", "x"}},
		{[]string{"--cloud=desc", "--model", "x"}, []string{"--model", "x"}},
		{[]string{"--tmux", "--model", "x"}, []string{"--model", "x"}},
		{[]string{"--tmux=classic", "-w", "feat"}, []string{"-w", "feat"}},
		{[]string{"--bg", "--background", "--desktop", "fix"}, nil},
		// optional-value flags that persist are replayed with their value
		{[]string{"-w", "feat", "--model", "x"}, []string{"-w", "feat", "--model", "x"}},
		{[]string{"--remote-control", "nm", "fix"}, []string{"--remote-control", "nm"}},
		// "--" terminator: everything after it is positional
		{[]string{"--allowedTools", "Bash", "Edit", "--", "fix bug"}, []string{"--allowedTools", "Bash", "Edit"}},
		{[]string{"--model", "x", "--", "--not-a-flag", "prompt"}, []string{"--model", "x"}},
	}
	for _, c := range cases {
		in := append([]string(nil), c.in...)
		got := ReplayArgs(c.in)
		if strings.Join(got, "\x00") != strings.Join(c.want, "\x00") || len(got) != len(c.want) {
			t.Errorf("ReplayArgs(%q) = %q want %q", c.in, got, c.want)
		}
		if strings.Join(in, "\x00") != strings.Join(c.in, "\x00") {
			t.Errorf("ReplayArgs mutated its input: %q -> %q", in, c.in)
		}
	}
}
