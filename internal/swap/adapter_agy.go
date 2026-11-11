package swap

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// agyAdapter runs the Antigravity CLI. It has no hooks: Poll reads its data dir.
type agyAdapter struct{}

func (agyAdapter) Command(h Harness, native, prompt string, extra []string, rt Runtime) (Launch, error) {
	if err := checkID(native); err != nil {
		return Launch{}, err
	}
	args := []string{"--log-file", filepath.Join(rt.Dir, "agy.log")}
	if native != "" {
		args = append(args, "--conversation", native)
	}
	args = append(append(args, h.Args...), extra...)
	if prompt != "" {
		args = append(args, "-i", prompt)
	}
	return Launch{Bin: binOr(h, "agy"), Args: args}, nil
}

func (agyAdapter) Quit() string { return "/quit" }

func agyDir(rt Runtime) string { return filepath.Join(rt.Home, ".gemini", "antigravity-cli") }

// agySQL runs a read-only query with the sqlite3 CLI. A WAL database whose
// -shm file is gone (agy exited) cannot be opened -readonly; open it immutable.
func agySQL(db, query string) (string, error) {
	if _, err := os.Stat(db); err != nil {
		return "", err
	}
	args := []string{"-readonly", db, query}
	if _, err := os.Stat(db + "-shm"); err != nil {
		args = []string{"file:" + db + "?immutable=1", query}
	}
	out, err := exec.Command("sqlite3", args...).Output()
	return strings.TrimSpace(string(out)), err
}

func (agyAdapter) Poll(h Harness, rt Runtime, s *Session, u *Usage) error {
	dir := agyDir(rt)
	id, limited := agyScan(filepath.Join(rt.Dir, "agy.log"), rt.Launched)
	if id != "" {
		s.NativeID = id
	} else if s.NativeID == "" { // newest prompt from this workspace since launch
		_ = eachLine(filepath.Join(dir, "history.jsonl"), func(l []byte) {
			var e struct {
				Timestamp      int64
				Workspace      string
				ConversationID string `json:"conversationId"`
			}
			if json.Unmarshal(l, &e) == nil && e.Workspace == rt.Cwd && e.ConversationID != "" &&
				e.Timestamp >= rt.Launched.UnixMilli() {
				s.NativeID = e.ConversationID
			}
		})
	}
	if safeID(s.NativeID) {
		row, err := agySQL(filepath.Join(dir, "conversation_summaries.db"),
			"select status, not_fully_idle from conversation_summaries where conversation_id='"+s.NativeID+"'")
		if status, nfi, ok := strings.Cut(row, "|"); err == nil && ok {
			idle := status == "CASCADE_RUN_STATUS_IDLE" && (nfi == "0" || nfi == "" || nfi == "false")
			if idle && !s.Idle {
				s.Turns++ // a turn ended
			}
			s.Idle = idle
		}
	}
	s.RateLimited = limited
	return nil
}

// agyConv matches the log line naming the conversation this agy process runs;
// the -i prompt never reaches history.jsonl (agy 1.5.5).
var agyConv = regexp.MustCompile(`^(?:Created|Streaming) conversation ([0-9A-Za-z-]+)`)

// agyScan reads this session's glog-format log after launch: the last conversation
// id agy opened, and whether it hit a quota error. Only the message text counts
// (stamps hold digits such as "402"), and only explicit exhaustion: info lines
// mention "quota" routinely.
func agyScan(path string, since time.Time) (id string, limited bool) {
	if st, err := os.Stat(path); err != nil || st.ModTime().Before(since) {
		return "", false
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		l := sc.Text() // "E1110 11:58:12.191265 pid file:line] msg"
		_, msg, ok := strings.Cut(l, "] ")
		if len(l) < 21 || !ok {
			continue
		}
		at, err := time.ParseInLocation("0102 15:04:05", l[1:14], since.Location())
		if err != nil || at.AddDate(since.Year(), 0, 0).Before(since.Add(-time.Second)) {
			continue
		}
		if m := agyConv.FindStringSubmatch(msg); m != nil {
			id = m[1]
		}
		if msg = strings.ToLower(msg); strings.Contains(msg, "quota exhausted") || strings.Contains(msg, "resource_exhausted") {
			limited = true
		}
	}
	return id, limited
}

// Turns is lossy: steps are protobuf, so it keeps printable runs of 8+ characters
// with 2+ spaces (prose), each capped at 500 bytes (long runs are file contents).
// The steps after the cursor (the last step idx read) become one assistant turn.
func (agyAdapter) Turns(native, cursor string, rt Runtime) ([]Turn, string, error) {
	if !safeID(native) {
		return nil, cursor, fmt.Errorf("bad agy conversation id %q", native)
	}
	from, err := strconv.Atoi(cursor)
	if cursor == "" {
		from, err = -1, nil
	}
	if err != nil {
		return nil, cursor, fmt.Errorf("bad agy cursor %q", cursor)
	}
	out, err := agySQL(filepath.Join(agyDir(rt), "conversations", native+".db"),
		"select idx, hex(step_payload) from steps where idx > "+strconv.Itoa(from)+" order by idx")
	if err != nil {
		return nil, cursor, fmt.Errorf("agy conversation %s: %v", native, err)
	}
	var b strings.Builder
	for _, row := range strings.Split(out, "\n") {
		idx, h, ok := strings.Cut(strings.TrimSpace(row), "|")
		if !ok {
			continue
		}
		cursor = idx
		raw, _ := hex.DecodeString(h)
		for _, run := range strings.FieldsFunc(strings.ToValidUTF8(string(raw), "\x00"), func(r rune) bool {
			return !unicode.IsPrint(r) && r != '\n' && r != '\t'
		}) {
			if run = strings.TrimSpace(run); len([]rune(run)) >= 8 && strings.Count(run, " ") >= 2 { // prose, not ids
				b.WriteString("\n" + strings.ToValidUTF8(run[:min(len(run), 500)], ""))
			}
		}
	}
	if b.Len() == 0 {
		return nil, cursor, nil
	}
	return []Turn{textTurn("agy", "assistant", "[lossy agy transcript]"+b.String())}, cursor, nil
}

// Write: agy's store is protobuf in sqlite; it receives the context file instead.
func (agyAdapter) Write(Harness, *Mirror, []Turn, Runtime) (int, error) { return 0, errNoMirror }
