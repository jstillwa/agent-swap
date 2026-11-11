package swap

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// Runtime is what an adapter needs to know about the running agent-swap session.
type Runtime struct {
	Exe, Dir, Cwd, StateDir, CfgPath, Home string   // Home = user home (tests override)
	Env                                    []string // the child's base env (store root lookups)
	Cfg                                    Config
	Launched                               time.Time // this child's launch
	Started                                time.Time // the run's start (session ownership)
	// Final is set only for the read after the child exited: readers then also
	// return the trailing incomplete turn and a cursor at the end of the session.
	Final bool
}

// Launch is a child process to start in the pane.
type Launch struct {
	Bin  string
	Args []string
	Env  []string // extra KEY=VALUE appended after the base env
}

// Adapter drives one harness kind.
type Adapter interface {
	// Command builds the child process. native = the harness's own session id to resume ("" = fresh).
	// prompt = initial message ("" = none). extra = user pass-through args, appended after h.Args.
	Command(h Harness, native, prompt string, extra []string, rt Runtime) (Launch, error)
	Quit() string // typed to exit gracefully
	// Poll updates s (and u) for harnesses whose signals come from their own files. Hook-driven: no-op.
	Poll(h Harness, rt Runtime, s *Session, u *Usage) error
	// Turns reads the completed turns after cursor ("" = from the start) of
	// native session, and returns the new cursor.
	Turns(native, cursor string, rt Runtime) ([]Turn, string, error)
	// Write: m.Native == "" creates a new session holding turns (sets m.Native,
	// m.Path, m.Cursor, m.Dev, m.Ino); otherwise it appends turns to the owned
	// session m (tail repair), updating m.Cursor/Dev/Ino. Kinds without mirror
	// support return errNoMirror.
	// It returns how many turns Turns(m.Native, cursor before the write) must
	// read back (the self-check).
	Write(h Harness, m *Mirror, turns []Turn, rt Runtime) (int, error)
}

// errRewound comes with valid turns and cursor from Turns: the entry at the
// cursor left the active conversation (rewind, /undo, branch switch). The
// runner logs it; mirrors keep the rewound turns (not retracted in v1).
var errRewound = errors.New("rewound before cursor")

func AdapterFor(kind string) (Adapter, error) {
	switch kind {
	case "claude":
		return claudeAdapter{}, nil
	case "codex":
		return codexAdapter{}, nil
	case "pi":
		return piAdapter{}, nil
	case "opencode":
		return opencodeAdapter{}, nil
	case "agy":
		return agyAdapter{}, nil
	}
	return nil, fmt.Errorf("unknown harness kind %q", kind)
}

var idChars = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// safeID reports whether a harness session id is safe to put in a path or argv.
func safeID(id string) bool { return idChars.MatchString(id) && !strings.Contains(id, "..") }

// checkID rejects an unsafe native id; "" (fresh session) is allowed.
func checkID(native string) error {
	if native != "" && !safeID(native) {
		return fmt.Errorf("unsafe session id %q", native)
	}
	return nil
}

// noPoll is embedded by hook-driven adapters.
type noPoll struct{}

func (noPoll) Poll(Harness, Runtime, *Session, *Usage) error { return nil }

// eachLine calls fn for every non-empty line of a JSONL file.
func eachLine(path string, fn func([]byte)) error {
	b, err := os.ReadFile(path)
	for _, l := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(l)) > 0 {
			fn(l)
		}
	}
	return err
}
