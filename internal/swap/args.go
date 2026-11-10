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
