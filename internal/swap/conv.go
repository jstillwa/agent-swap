package swap

import (
	"fmt"
	"regexp"
	"strings"
)

// Turn is one conversation turn in the run's in-memory log.
type Turn struct {
	Origin string // harness kind that produced the turn ("agent-swap" for the omitted-turns note)
	Role   string // "user" | "assistant"
	Text   string
}

const interruptedOutput = "(no result: interrupted)"

// noise starts harness bookkeeping texts: codex context, claude slash-command
// records and interrupt notices.
var noise = regexp.MustCompile(`^(<(environment_context|command-name|command-message|command-args|local-command-stdout|local-command-stderr)>|\[Request interrupted by user)`)

// textTurn is a prose turn; its Text is empty when s is blank or harness noise (callers skip it).
func textTurn(kind, role, s string) Turn {
	if s = strings.TrimSpace(s); noise.MatchString(s) {
		s = ""
	}
	return Turn{Origin: kind, Role: role, Text: clean(s)}
}

var attrUnsafe = regexp.MustCompile(`[^A-Za-z0-9_.:-]`)

// toolTurn renders a tool call and its result as user-role text inside an
// untrusted tag; every "<" inside is escaped so the body cannot hold markup.
// output "" means the call never got a result.
func toolTurn(kind, name, input, output string) Turn {
	if output == "" {
		output = interruptedOutput
	}
	return Turn{Origin: kind, Role: "user", Text: fmt.Sprintf(`<agent-swap-tool source="%s" tool="%s" untrusted="true">%s → %s</agent-swap-tool>`,
		attrUnsafe.ReplaceAllString(kind, "_"), attrUnsafe.ReplaceAllString(name, "_"),
		cut(body(input), 1024), cut(body(output), 4096))}
}

func body(s string) string {
	return strings.ReplaceAll(clean(strings.TrimSpace(s)), "<", "&lt;")
}

// cut keeps the first n bytes of s without splitting a rune.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}

// clean escapes the tool tag and redacts secrets; all text entering the log goes through it.
func clean(s string) string { return redact(escape(s)) }

var toolTag = regexp.MustCompile(`(?i)<(/?)(agent-swap-tool)`)

// escape neutralizes agent-swap-tool tags so text cannot open or close one.
func escape(s string) string { return toolTag.ReplaceAllString(s, "&lt;$1$2") }

var secrets = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY[A-Z ]*-----[\s\S]*?(-----END [A-Z0-9 ]*PRIVATE KEY[A-Z ]*-----|$)`), "[redacted]"},
	{regexp.MustCompile(`(?i)((?:authorization|set-cookie|cookie)["']?\s*[:=]\s*)[^\r\n]+`), "${1}[redacted]"},
	{regexp.MustCompile(`\b([a-zA-Z][a-zA-Z0-9+.-]*://[^\s/:@]+:)[^\s/@]+@`), "${1}[redacted]@"},
	{regexp.MustCompile(`(--password[= ]\s*)[^\s"']+`), "${1}[redacted]"},
	{regexp.MustCompile(`(?i)\b([a-z0-9_]*(?:key|token|secret|password)[a-z0-9_]*["']?\s*[=:]\s*["']?)[^\s"'(),;]{8,}`), "${1}[redacted]"},
	{regexp.MustCompile(`\b(sk-[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|[sr]k_live_[A-Za-z0-9]{10,}|glpat-[A-Za-z0-9_-]{20,}|AIza[0-9A-Za-z_-]{35}|npm_[A-Za-z0-9]{36}|AKIA[0-9A-Z]{16}|xox[abeprs]-[A-Za-z0-9-]{10,}|xapp-[A-Za-z0-9-]{10,}|eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,})`), "[redacted]"},
}

// redact replaces likely secrets with [redacted]. It is best-effort.
func redact(s string) string {
	for _, p := range secrets {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s
}

func logBytes(turns []Turn) int {
	n := 0
	for _, t := range turns {
		n += len(t.Text)
	}
	return n
}

// capTurns keeps the newest turns that fit in budget bytes. When even the
// newest turn does not fit, it keeps that turn's head cut to the budget.
func capTurns(log []Turn, budget int) (kept []Turn, omitted int) {
	i, size := len(log), 0
	for i > 0 && size+len(log[i-1].Text) <= budget {
		i--
		size += len(log[i].Text)
	}
	if i == len(log) && i > 0 {
		t := log[i-1]
		t.Text = cut(t.Text, budget)
		return []Turn{t}, i - 1
	}
	return log[i:], i
}

// merge joins consecutive turns of the same role.
func merge(turns []Turn) []Turn {
	var out []Turn
	for _, t := range turns {
		if t.Text == "" {
			continue
		}
		if n := len(out); n > 0 && out[n-1].Role == t.Role {
			out[n-1].Text += "\n\n" + t.Text
			continue
		}
		out = append(out, t)
	}
	return out
}

// omittedNote is the user turn put in front of a capped log.
func omittedNote(n int) Turn {
	return Turn{Origin: "agent-swap", Role: "user", Text: fmt.Sprintf("[agent-swap] %d earlier turns omitted", n)}
}

// markdown is the context-file body for a harness without a mirror.
func markdown(turns []Turn, omitted int) string {
	var b strings.Builder
	b.WriteString("# agent-swap conversation\n\n")
	if omitted > 0 {
		b.WriteString(omittedNote(omitted).Text + "\n\n")
	}
	for _, t := range turns {
		fmt.Fprintf(&b, "## %s (%s)\n\n%s\n\n", t.Role, t.Origin, t.Text)
	}
	return b.String()
}
