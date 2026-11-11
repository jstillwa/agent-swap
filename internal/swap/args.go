package swap

import "strings"

// Claude Code 2.1.x flags (from `claude --help`, plus the hidden *-file and
// --permission-prompt-tool variants), grouped by how commander parses them.
const (
	valueFlags = " --agent --agents --append-system-prompt --append-system-prompt-file --autocompact" +
		" --debug-file --effort --environment --fallback-model --input-format --json-schema --max-budget-usd" +
		" --model -n --name --output-format --permission-mode --permission-prompt-tool --permission-prompts" +
		" --plugin-dir --plugin-url --remote-control-session-name-prefix --session-id --setting-sources" +
		" --settings --system-prompt --system-prompt-file --system-prompt-snapshot "
	optionalFlags = " -r --resume --cloud -d --debug --from-pr --prompt-suggestions --remote-control --teleport -w --worktree "
	variadicFlags = " --add-dir --allowedTools --allowed-tools --betas --disallowedTools --disallowed-tools --file --mcp-config --tools "
	launchFlags   = " -c --continue -r --resume --session-id --fork-session -p --print --from-pr --teleport --cloud" +
		" --tmux --bg --background --desktop "
)

func isDash(s string) bool { return strings.HasPrefix(s, "-") }

// ReplayArgs returns the claude args worth repeating on a relaunch: it drops
// positional arguments (the prompt), everything after "--", and the
// launch-only flags that pick or print a session.
func ReplayArgs(args []string) []string {
	var out []string
	for i := 0; i < len(args) && args[i] != "--"; i++ {
		if !isDash(args[i]) {
			continue // positional
		}
		name, _, inline := strings.Cut(args[i], "=")
		end, has := i+1, func(set string) bool { return !inline && strings.Contains(set, " "+name+" ") }
		switch {
		case has(valueFlags):
			end = min(i+2, len(args))
		case has(optionalFlags):
			if end < len(args) && !isDash(args[end]) {
				end++
			}
		case has(variadicFlags):
			for end < len(args) && !isDash(args[end]) {
				end++
			}
		}
		if !strings.Contains(launchFlags, " "+name+" ") {
			out = append(out, args[i:end]...)
		}
		i = end - 1
	}
	return out
}

// resumes reports whether pass-through args pick an existing session
// (claude/codex/pi/opencode resume, continue and session flags).
func resumes(args []string) bool {
	for _, a := range args {
		if a == "--" {
			break
		}
		name, _, _ := strings.Cut(a, "=")
		switch name {
		case "-r", "--resume", "-c", "--continue", "-s", "--session": // not a "resume" positional: it may be the prompt
			return true
		}
	}
	return false
}

// resumeID is the session id given to -r/--resume ("" for none or a picker).
func resumeID(args []string) string {
	for i, a := range args {
		if a == "--" {
			break
		}
		if name, v, ok := strings.Cut(a, "="); ok && (name == "--resume" || name == "-r") {
			return v
		}
		if (a == "-r" || a == "--resume") && i+1 < len(args) && !isDash(args[i+1]) {
			return args[i+1]
		}
	}
	return ""
}
