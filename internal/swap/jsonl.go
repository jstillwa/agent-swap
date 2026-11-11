package swap

// Shared JSONL session reading and writing helpers (claude, pi; codex reuses turnBuilder).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
)

type jline struct {
	raw        []byte
	start, end int
}

// readJSONL returns the complete lines of a file and their total size; a last line
// without "\n" may still be being written and is ignored.
func readJSONL(path string) ([]jline, int, error) {
	b, err := os.ReadFile(path)
	var out []jline
	off := 0
	for {
		i := bytes.IndexByte(b[off:], '\n')
		if i < 0 {
			return out, off, err
		}
		if l := bytes.TrimSpace(b[off : off+i]); len(l) > 0 {
			out = append(out, jline{l, off, off + i + 1})
		}
		off += i + 1
	}
}

// entry is one line of a chained session file.
type entry struct {
	id, parent string
	start, end int
	leaf       bool // may be the active leaf
	content    bool // a user or assistant message that is rendered
	done       bool // ends a completed turn
	result     bool // only tool results: claude parents each parallel result to its own call, off the chain
}

// chain is the set of ids on the path from the newest leaf to the root.
func chain(es []entry) map[string]bool {
	by, leaf := map[string]int{}, ""
	for i, e := range es {
		if e.id != "" {
			by[e.id] = i
			if e.leaf {
				leaf = e.id
			}
		}
	}
	on := map[string]bool{}
	for id := leaf; id != "" && !on[id]; {
		on[id] = true
		i, ok := by[id]
		if !ok {
			break
		}
		id = es[i].parent
	}
	return on
}

// readTurns renders the active chain's entries after the byte offset cursor, up to
// the last completed turn (final: up to the end, the exit read). The new cursor is
// that turn's end, or the end of the file when no message follows it. errRewound
// (with the turns) means the last entry before the cursor left the active chain:
// a rewind or branch switch.
func readTurns(kind string, es []entry, size int, cursor string, final bool, emit func(int, *turnBuilder)) ([]Turn, string, error) {
	from := 0
	if cursor != "" {
		n, err := strconv.Atoi(cursor)
		if err != nil {
			return nil, cursor, fmt.Errorf("bad %s cursor %q", kind, cursor)
		}
		from = n
	}
	on, stop, last, more := chain(es), -1, -1, false
	live := func(e entry) bool { return on[e.id] || e.result && on[e.parent] }
	for i, e := range es {
		switch {
		case e.end <= from:
			if e.leaf {
				last = i
			}
		case live(e) && (e.done || final):
			stop = i
		}
		more = more || e.start >= from && e.content
	}
	var rewound error
	if last >= 0 && !live(es[last]) {
		rewound = errRewound
	}
	if stop < 0 {
		if (!more || final) && size >= from {
			cursor = strconv.Itoa(size)
		}
		return nil, cursor, rewound
	}
	b := &turnBuilder{kind: kind, calls: map[string][2]string{}}
	for i, e := range es[:stop+1] {
		if e.start >= from && live(e) && e.content {
			emit(i, b)
		}
	}
	end := es[stop].end
	if final || !slices.ContainsFunc(es[stop+1:], func(e entry) bool { return e.content }) {
		end = size
	}
	return b.done(), strconv.Itoa(end), rewound
}

// turnBuilder pairs tool calls with their results. A result whose call precedes
// the cursor (or a writer's synthetic repair) is dropped.
type turnBuilder struct {
	kind  string
	turns []Turn
	calls map[string][2]string // id → name, input
	order []string
}

func (b *turnBuilder) add(t Turn) {
	if t.Text != "" {
		b.turns = append(b.turns, t)
	}
}

func (b *turnBuilder) text(role, s string) {
	if role == "user" {
		b.flush()
	}
	b.add(textTurn(b.kind, role, s))
}

func (b *turnBuilder) call(id, name, input string) {
	b.calls[id] = [2]string{name, input}
	b.order = append(b.order, id)
}

func (b *turnBuilder) result(id, output string) {
	if c, ok := b.calls[id]; ok {
		delete(b.calls, id)
		b.add(toolTurn(b.kind, c[0], c[1], output))
	}
}

// flush renders calls that never got a result as interrupted.
func (b *turnBuilder) flush() {
	for _, id := range b.order {
		if c, ok := b.calls[id]; ok {
			delete(b.calls, id)
			b.add(toolTurn(b.kind, c[0], c[1], ""))
		}
	}
	b.order = nil
}

func (b *turnBuilder) done() []Turn {
	b.flush()
	return b.turns
}

// rawText renders a JSON value: a string as is, text blocks joined, anything else compacted.
func rawText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct{ Type, Text string }
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	var c bytes.Buffer
	_ = json.Compact(&c, raw)
	return c.String()
}

const (
	markInterrupted = "[agent-swap] (turn interrupted)"
	markImported    = "[agent-swap] (imported conversation)"
)

// writable merges same-role turns and keeps roles alternating after the file's
// last message role tail ("" = an empty session, which must start with a user turn).
func writable(turns []Turn, tail string) []Turn {
	turns = merge(turns)
	if len(turns) == 0 {
		return turns
	}
	switch {
	case tail == "user" && turns[0].Role == "user":
		turns = append([]Turn{{Origin: "agent-swap", Role: "assistant", Text: markInterrupted}}, turns...)
	case tail == "" && turns[0].Role == "assistant":
		turns = append([]Turn{{Origin: "agent-swap", Role: "user", Text: markImported}}, turns...)
	}
	return turns
}

// readable is how many of the written turns a reader returns: those up to the
// last assistant turn (a completed-turn boundary), minus any the reader drops as noise.
func readable(written []Turn) int {
	n := 0
	for i := range written {
		if written[i].Role == "assistant" {
			n = i + 1
		}
	}
	k := 0
	for _, t := range written[:n] {
		if textTurn("", t.Role, t.Text).Text != "" {
			k++
		}
	}
	return k
}
