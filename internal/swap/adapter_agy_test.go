package swap

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAgyCommand(t *testing.T) {
	lf := []string{"--log-file", "{dir}/agy.log"}
	with := func(a ...string) []string { return append(append([]string{}, lf...), a...) }
	runCmdCases(t, "agy", []cmdCase{
		{name: "fresh bare", wantBin: "agy", wantArgs: lf},
		{name: "fresh prompt args extra", h: Harness{Args: []string{"--model", "gemini-pro"}}, extra: []string{"--yolo"},
			prompt: "read ctx.md", wantBin: "agy", wantArgs: with("--model", "gemini-pro", "--yolo", "-i", "read ctx.md")},
		{name: "resume", native: "conv-1", prompt: "go on", wantBin: "agy",
			wantArgs: with("--conversation", "conv-1", "-i", "go on")},
		{name: "bin override", h: Harness{Bin: "/opt/agy"}, wantBin: "/opt/agy", wantArgs: lf},
	})
}

func tAgyDir(rt Runtime) string { return filepath.Join(rt.Home, ".gemini", "antigravity-cli") }

func tAgyHistory(t *testing.T, rt Runtime, entries ...[3]any) { // {workspace, time, conversationId}
	t.Helper()
	var b strings.Builder
	for _, e := range entries {
		j, _ := json.Marshal(map[string]any{"display": "synthetic prompt", "timestamp": e[1].(time.Time).UnixMilli(),
			"workspace": e[0], "conversationId": e[2]})
		b.Write(append(j, '\n'))
	}
	writeFile(t, filepath.Join(tAgyDir(rt), "history.jsonl"), b.String())
}

// tAgySummary creates conversation_summaries.db with one row for id (status "" → no row).
func tAgySummary(t *testing.T, rt Runtime, id, status string, notFullyIdle int) {
	t.Helper()
	sql := "create table conversation_summaries (conversation_id text primary key, title text not null default '', " +
		"status text not null default '', not_fully_idle numeric not null default false);"
	if status != "" {
		sql += fmt.Sprintf("insert into conversation_summaries(conversation_id,status,not_fully_idle) values('%s','%s',%d);",
			id, status, notFullyIdle)
	}
	sqliteExec(t, filepath.Join(tAgyDir(rt), "conversation_summaries.db"), sql)
}

// tAgyLog writes the session's --log-file (rt.Dir/agy.log) in glog format, stamped and modified at `at`.
func tAgyLog(t *testing.T, rt Runtime, at time.Time, msgs ...string) {
	t.Helper()
	lines := make([]tAgyLine, len(msgs))
	for i, m := range msgs {
		lines[i] = tAgyLine{at, m}
	}
	tAgyLogLines(t, rt, at, lines...)
}

type tAgyLine struct {
	at  time.Time
	msg string // first char is the glog level
}

func tAgyLogLines(t *testing.T, rt Runtime, mtime time.Time, lines ...tAgyLine) {
	t.Helper()
	var b strings.Builder
	for _, l := range lines {
		fmt.Fprintf(&b, "%s%s     392 server.go:1233] %s\n", l.msg[:1], l.at.Format("0102 15:04:05.000000"), l.msg[1:])
	}
	p := filepath.Join(rt.Dir, "agy.log")
	writeFile(t, p, b.String())
	setMtime(t, p, mtime)
}

func tAgyPoll(t *testing.T, rt Runtime, s *Session) {
	t.Helper()
	if err := adapterFor(t, "agy").Poll(Harness{Name: "agy", Kind: "agy"}, rt, s, &Usage{}); err != nil {
		t.Fatalf("Poll: %v", err)
	}
}

func TestAgyPollNativeID(t *testing.T) {
	rt := testRT(t)
	tAgyHistory(t, rt,
		[3]any{rt.Cwd, rt.Launched.Add(-time.Hour), "conv-old"},
		[3]any{rt.Cwd, rt.Launched.Add(time.Second), "conv-new"},
		[3]any{"/other/workspace", rt.Launched.Add(2 * time.Second), "conv-other"})
	var s Session
	tAgyPoll(t, rt, &s)
	if s.NativeID != "conv-new" {
		t.Errorf("NativeID = %q, want conv-new", s.NativeID)
	}

	rt2 := testRT(t)
	tAgyHistory(t, rt2, [3]any{rt2.Cwd, rt2.Launched.Add(-time.Hour), "conv-old"})
	var s2 Session
	tAgyPoll(t, rt2, &s2)
	if s2.NativeID != "" {
		t.Errorf("NativeID = %q, want none (only pre-launch entries)", s2.NativeID)
	}
}

func TestAgyPollNativeIDFromLog(t *testing.T) {
	cases := []struct {
		name, start, want string
		history           bool
	}{
		{"created after launch wins over pre-launch line", "", "conv-new-1", false},
		{"log overrides existing id", "conv-stale", "conv-new-1", false},
		{"log beats history", "", "conv-new-1", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rt := testRT(t)
			now := rt.Launched.Add(2 * time.Second)
			tAgyLogLines(t, rt, now,
				tAgyLine{rt.Launched.Add(-time.Hour), "ICreated conversation conv-old-0 (backend=BACKEND_TYPE_UNSPECIFIED)"},
				tAgyLine{now, "IStarting language server process with pid 688"},
				tAgyLine{now, "ICreated conversation conv-new-1 (backend=BACKEND_TYPE_UNSPECIFIED)"},
				tAgyLine{now, "Ithis line mentions Created conversation conv-mid-x but does not start with it"})
			if c.history {
				tAgyHistory(t, rt, [3]any{rt.Cwd, now, "conv-from-history"})
			}
			s := Session{NativeID: c.start}
			tAgyPoll(t, rt, &s)
			if s.NativeID != c.want {
				t.Errorf("NativeID = %q, want %q", s.NativeID, c.want)
			}
		})
	}
	t.Run("streaming line is newest", func(t *testing.T) {
		rt := testRT(t)
		now := rt.Launched.Add(2 * time.Second)
		tAgyLogLines(t, rt, now,
			tAgyLine{now, "ICreated conversation conv-a"},
			tAgyLine{now, "IStreaming conversation conv-b"})
		var s Session
		tAgyPoll(t, rt, &s)
		if s.NativeID != "conv-b" {
			t.Errorf("NativeID = %q, want conv-b (last matching line wins)", s.NativeID)
		}
	})
}

func TestAgyPollIdle(t *testing.T) {
	requireSqlite(t)
	cases := []struct {
		name, status string
		notIdle      int
		start, want  bool
	}{
		{"idle", "CASCADE_RUN_STATUS_IDLE", 0, false, true},
		{"running", "CASCADE_RUN_STATUS_RUNNING", 0, true, false},
		{"idle but not fully", "CASCADE_RUN_STATUS_IDLE", 1, true, false},
		{"no row keeps idle", "", 0, true, true},
		{"no row keeps busy", "", 0, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rt := testRT(t)
			tAgySummary(t, rt, "conv-1", c.status, c.notIdle)
			s := Session{NativeID: "conv-1", Idle: c.start}
			tAgyPoll(t, rt, &s)
			if s.Idle != c.want || s.NativeID != "conv-1" {
				t.Errorf("session = %+v, want Idle=%v", s, c.want)
			}
		})
	}
}

func TestAgyPollNoDataDir(t *testing.T) {
	rt := testRT(t)
	s := Session{NativeID: "conv-1", Idle: true}
	tAgyPoll(t, rt, &s)
	if !s.Idle || s.RateLimited {
		t.Errorf("session changed with no agy data: %+v", s)
	}
}

func TestAgyPollRateLimited(t *testing.T) {
	benign := []string{
		"IStarting language server process with pid 688",
		"IapplyAuthResult: email=user@example.com, authMethod=gcp, quotaProject=synthetic-project-123",
		"IdoRefreshQuota: starting reload (force=true)",
	}
	cases := []struct {
		name  string
		age   time.Duration // log stamped this long before Launched (negative = after)
		lines []string
		want  bool
	}{
		{"normal startup noise", -time.Second, benign, false},
		{"benign warning", -time.Second, append(benign, "Wfailed to refresh model list: connection reset by peer"), false},
		{"quota exhausted", -time.Second, append(benign, "EQuota exhausted for model gemini-pro; retry after reset"), true},
		{"quota exhausted warning", -time.Second, append(benign, "WRequest failed: Quota exhausted, backing off"), true},
		{"resource exhausted", -time.Second, append(benign, "Wrpc error: code = ResourceExhausted desc = RESOURCE_EXHAUSTED"), true},
		{"old quota error before launch", 2 * time.Hour, append(benign, "EQuota exhausted for model gemini-pro"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rt := testRT(t)
			tAgyLog(t, rt, rt.Launched.Add(-c.age), c.lines...)
			s := Session{NativeID: "conv-1", RateLimited: false}
			tAgyPoll(t, rt, &s)
			if s.RateLimited != c.want {
				t.Errorf("RateLimited = %v, want %v", s.RateLimited, c.want)
			}
		})
	}
}

func TestAgyTranscript(t *testing.T) {
	requireSqlite(t)
	rt := testRT(t)
	blob := func(parts ...string) string { return "X'" + hex.EncodeToString([]byte(strings.Join(parts, ""))) + "'" }
	sqliteExec(t, filepath.Join(tAgyDir(rt), "conversations", "conv-1.db"),
		"create table steps (idx integer primary key, step_type integer not null default 0, step_payload blob);"+
			"insert into steps(idx, step_payload) values (1, "+blob("\x12\x05", "Assistant answers with synthetic gizmos", "\x01\x02xyz\x03", strings.Repeat("q q ", 500),
			"\x04NoSpacesTokenIdentifier\x05onespace identifier\x06")+");"+
			"insert into steps(idx, step_payload) values (0, "+blob("\x0a\x1f", "Synthetic user asks about widgets", "\x00\x00")+");")
	got, err := adapterFor(t, "agy").Transcript("conv-1", rt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "[agy transcript, lossy]") {
		t.Errorf("missing lossy prefix: %q", got)
	}
	assertTranscript(t, got, []string{"Synthetic user asks about widgets", "Assistant answers with synthetic gizmos"},
		[]string{"xyz", "\x00", "\x12", "NoSpacesTokenIdentifier", "onespace identifier"})
	long := strings.Repeat("q q ", 500)
	kept := 0
	for n := len(long); n > 0; n-- {
		if strings.Contains(got, long[:n]) {
			kept = n
			break
		}
	}
	if kept < 400 || kept > 500 {
		t.Errorf("long prose run kept %d bytes, want it capped at 500", kept)
	}
}

func TestAgyTranscriptMissing(t *testing.T) {
	requireSqlite(t)
	if _, err := adapterFor(t, "agy").Transcript("no-such-conv", testRT(t)); err == nil {
		t.Error("want error for a missing conversation db")
	}
}

// Digits in the glog stamp (".402102", ".429000") must not read as HTTP 402/429.
func TestAgyPollStampDigitsNotLimited(t *testing.T) {
	rt := testRT(t)
	at := rt.Launched.Truncate(time.Second).Add(time.Second + 402102*time.Microsecond)
	tAgyLog(t, rt, at, "Wstream reconnect scheduled", "Ehandshake retry 1 of 3")
	s := Session{NativeID: "conv-1"}
	tAgyPoll(t, rt, &s)
	if s.RateLimited {
		t.Error("stamp digits must not count as a limit error")
	}
}

func TestOpencodePluginGuards(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("plugins", "agent-swap-opencode.js"))
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"info?.parentID", "children.add(info.id)", "main ??= id", "if (id !== main) return;",
		`hook("LimitHit", id, p.status.message)`, "(p.status.attempt || 0) >= 3", "AGENT_SWAP_EXE"} {
		if !strings.Contains(string(b), w) {
			t.Errorf("opencode plugin missing %q", w)
		}
	}
}
