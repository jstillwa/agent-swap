package swap

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodexCommand(t *testing.T) {
	runCmdCases(t, "codex", []cmdCase{
		{name: "fresh bare", wantBin: "codex", wantArgs: []string{}},
		{name: "fresh prompt args extra", h: Harness{Args: []string{"-m", "gpt-5.5"}}, extra: []string{"--search"},
			prompt: "read ctx.md", wantBin: "codex", wantArgs: []string{"-m", "gpt-5.5", "--search", "read ctx.md"}},
		{name: "resume", native: "019a-uuid", prompt: "go on", h: Harness{Args: []string{"-m", "gpt-5.5"}},
			wantBin: "codex", wantArgs: []string{"resume", "019a-uuid", "-m", "gpt-5.5", "go on"}},
		{name: "resume no prompt", native: "019a-uuid", wantBin: "codex", wantArgs: []string{"resume", "019a-uuid"}},
		{name: "bin override", h: Harness{Bin: "/opt/codex"}, wantBin: "/opt/codex", wantArgs: []string{}},
	})
}

const tCodexDay = "2026/11/10"

// codexLine renders one rollout line.
func tCodexLine(typ string, payload map[string]any) string {
	b, _ := json.Marshal(map[string]any{"timestamp": "2026-11-10T05:37:09.000Z", "type": typ, "payload": payload})
	return string(b)
}

// tWriteRollout writes a fresh interactive rollout for id/cwd plus extra event lines; returns its path.
func tWriteRollout(t *testing.T, sessionsRoot, stamp, id, cwd string, lines ...string) string {
	t.Helper()
	return tWriteRolloutMeta(t, sessionsRoot, stamp, map[string]any{"id": id, "cwd": cwd}, lines...)
}

// tWriteRolloutMeta is tWriteRollout with session_meta overrides (defaults: timestamp now, source cli).
func tWriteRolloutMeta(t *testing.T, sessionsRoot, stamp string, meta map[string]any, lines ...string) string {
	t.Helper()
	m := map[string]any{"timestamp": time.Now().UTC().Format(time.RFC3339Nano), "cli_version": "0.166.0",
		"source": "cli", "originator": "codex_cli_rs"}
	for k, v := range meta {
		m[k] = v
	}
	p := filepath.Join(sessionsRoot, tCodexDay, fmt.Sprintf("rollout-2026-11-10T%s-%s.jsonl", stamp, m["id"]))
	writeFile(t, p, strings.Join(append([]string{tCodexLine("session_meta", m)}, lines...), "\n")+"\n")
	return p
}

func tEv(typ string, kv ...any) string {
	p := map[string]any{"type": typ}
	for i := 0; i+1 < len(kv); i += 2 {
		p[kv[i].(string)] = kv[i+1]
	}
	return tCodexLine("event_msg", p)
}

func tLimits(primary, secondary any) string {
	return tEv("token_count", "info", nil, "rate_limits", map[string]any{"limit_id": "codex", "primary": primary, "secondary": secondary})
}

func tWin(pct float64, reset int64) map[string]any {
	return map[string]any{"used_percent": pct, "window_minutes": 300, "resets_at": reset}
}

func TestCodexPollDiscoversNativeID(t *testing.T) {
	rt := testRT(t)
	root := filepath.Join(rt.Home, ".codex", "sessions")
	old := rt.Launched.Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	// Recent mtime but session started before launch: a resumed/old session, not ours.
	tWriteRolloutMeta(t, root, "07-00-00", map[string]any{"id": "00000000-0000-4000-8000-000000000001", "cwd": rt.Cwd, "timestamp": old})
	tWriteRollout(t, root, "05-00-00", "00000000-0000-4000-8000-000000000002", rt.Cwd)
	tWriteRollout(t, root, "06-00-00", "00000000-0000-4000-8000-000000000003", "/some/other/dir")
	// `codex exec` runs (e.g. other tools) in the same cwd must be ignored.
	tWriteRolloutMeta(t, root, "08-00-00", map[string]any{"id": "00000000-0000-4000-8000-000000000004", "cwd": rt.Cwd, "source": "exec"})
	tWriteRolloutMeta(t, root, "09-00-00", map[string]any{"id": "00000000-0000-4000-8000-000000000005", "cwd": rt.Cwd, "originator": "codex_exec"})
	var s Session
	var u Usage
	if err := adapterFor(t, "codex").Poll(Harness{Name: "codex", Kind: "codex"}, rt, &s, &u); err != nil {
		t.Fatal(err)
	}
	if s.NativeID != "00000000-0000-4000-8000-000000000002" {
		t.Errorf("NativeID = %q, want the fresh interactive rollout for this cwd", s.NativeID)
	}
}

func TestCodexPollHonorsCodexHome(t *testing.T) {
	rt := testRT(t)
	ch := t.TempDir()
	t.Setenv("CODEX_HOME", ch)
	tWriteRollout(t, filepath.Join(ch, "sessions"), "05-00-00", "00000000-0000-4000-8000-0000000000aa", rt.Cwd)
	var s Session
	if err := adapterFor(t, "codex").Poll(Harness{Name: "codex", Kind: "codex"}, rt, &s, &Usage{}); err != nil {
		t.Fatal(err)
	}
	if s.NativeID != "00000000-0000-4000-8000-0000000000aa" {
		t.Errorf("NativeID = %q, want rollout under $CODEX_HOME", s.NativeID)
	}
}

func TestCodexPollNoRollout(t *testing.T) {
	rt := testRT(t)
	tWriteRollout(t, filepath.Join(rt.Home, ".codex", "sessions"), "05-00-00", "00000000-0000-4000-8000-0000000000bb", "/elsewhere")
	s := Session{Idle: true}
	if err := adapterFor(t, "codex").Poll(Harness{Name: "codex", Kind: "codex"}, rt, &s, &Usage{}); err != nil {
		t.Fatalf("Poll with no matching rollout should not fail: %v", err)
	}
	if s.NativeID != "" || !s.Idle || s.RateLimited {
		t.Errorf("session changed without a rollout: %+v", s)
	}
}

func TestCodexPollState(t *testing.T) {
	const id = "00000000-0000-4000-8000-0000000000cc"
	future, past := time.Now().Add(3*time.Hour).Unix(), time.Now().Add(-time.Hour).Unix()
	started, done := tEv("task_started", "turn_id", "t1"), tEv("task_complete", "turn_id", "t1")
	cases := []struct {
		name        string
		lines       []string
		idle, limit bool
		windows     map[string]Window
	}{
		{name: "no task yet", idle: true},
		{name: "task running", lines: []string{started}},
		{name: "task complete", lines: []string{started, done}, idle: true},
		{name: "second task running", lines: []string{started, done, started}},
		{name: "windows from last token_count", idle: true,
			lines:   []string{tLimits(tWin(10, future), tWin(5, future)), tLimits(tWin(70.5, future), tWin(20, future+99))},
			windows: map[string]Window{"primary": {70.5, future}, "secondary": {20, future + 99}}},
		{name: "null secondary skipped", idle: true, lines: []string{tLimits(tWin(30, future), nil)},
			windows: map[string]Window{"primary": {30, future}}},
		{name: "null rate_limits tolerated", idle: true, lines: []string{tEv("token_count", "info", nil, "rate_limits", nil)}},
		{name: "live window at 100", idle: true, limit: true, lines: []string{tLimits(tWin(100, future), nil)},
			windows: map[string]Window{"primary": {100, future}}},
		{name: "expired window at 100 is not limited", idle: true, lines: []string{tLimits(tWin(100, past), nil)}},
		{name: "usage limit error", idle: true, limit: true,
			lines: []string{started, tEv("error", "message", "You've hit your usage limit. Try again later."), done}},
		{name: "credits: 100% is not limited and sets no windows", idle: true, windows: map[string]Window{},
			lines: []string{tEv("token_count", "rate_limits", map[string]any{"primary": tWin(100, future), "secondary": nil,
				"credits": map[string]any{"has_credits": true, "unlimited": false}})}},
		{name: "unlimited credits", idle: true, windows: map[string]Window{},
			lines: []string{tEv("token_count", "rate_limits", map[string]any{"primary": tWin(100, future),
				"credits": map[string]any{"has_credits": false, "unlimited": true}})}},
		{name: "rate_limit_reached_type set", idle: true, limit: true, windows: map[string]Window{},
			lines: []string{tEv("token_count", "rate_limits", map[string]any{"primary": tWin(20, future),
				"credits": map[string]any{"has_credits": true}, "rate_limit_reached_type": "workspace_member_usage_limit_reached"})}},
		{name: "other error", idle: true, lines: []string{started, tEv("error", "message", "stream disconnected before completion"), done}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rt := testRT(t)
			p := tWriteRollout(t, filepath.Join(rt.Home, ".codex", "sessions"), "05-00-00", id, rt.Cwd, c.lines...)
			setMtime(t, p, rt.Launched.Add(-48*time.Hour)) // a known NativeID is found regardless of age
			s := Session{NativeID: id, Idle: !c.idle, RateLimited: !c.limit}
			u := Usage{}
			if err := adapterFor(t, "codex").Poll(Harness{Name: "codex", Kind: "codex"}, rt, &s, &u); err != nil {
				t.Fatal(err)
			}
			if s.NativeID != id || s.Idle != c.idle || s.RateLimited != c.limit {
				t.Errorf("session = %+v, want idle=%v limited=%v", s, c.idle, c.limit)
			}
			for k, w := range c.windows {
				if g := u.Windows[k]; g == nil || *g != w {
					t.Errorf("window %s = %+v, want %+v", k, g, w)
				}
			}
			if c.windows != nil && len(u.Windows) != len(c.windows) {
				t.Errorf("windows = %v, want keys of %v", u.Windows, c.windows)
			}
		})
	}
}

func TestCodexTranscriptAndPollFixture(t *testing.T) {
	rt := testRT(t)
	const id = "019a0000-0000-7000-8000-00000000f1f1"
	placeFixture(t, "codex/rollout.jsonl",
		filepath.Join(rt.Home, ".codex", "sessions", tCodexDay, "rollout-2026-11-10T05-37-09-"+id+".jsonl"),
		"__ID__", id, "__CWD__", rt.Cwd, "__TS__", time.Now().UTC().Format(time.RFC3339Nano))
	a := adapterFor(t, "codex")
	got, err := a.Transcript(id, rt)
	if err != nil {
		t.Fatal(err)
	}
	assertTranscript(t, got, []string{
		"[user] Add a hello file to the synthetic repo.",
		"[tool] apply_patch: *** Begin Patch",
		"[tool] exec: echo yyy",
		"[assistant] Created hello.txt with synthetic content.",
	}, []string{"DEVELOPER", "environment_context", "REASONING", "TOOL OUTPUT", strings.Repeat("y", 100)})

	var s Session
	u := Usage{}
	if err := a.Poll(Harness{Name: "codex", Kind: "codex"}, rt, &s, &u); err != nil {
		t.Fatal(err)
	}
	if s.NativeID != id || !s.Idle || s.RateLimited {
		t.Errorf("session = %+v", s)
	}
	if p, q := u.Windows["primary"], u.Windows["secondary"]; p == nil || q == nil ||
		*p != (Window{61.5, 4102444800}) || *q != (Window{12, 4103049600}) {
		t.Errorf("windows = %+v", u.Windows)
	}
}

func TestCodexTranscriptMissing(t *testing.T) {
	if _, err := adapterFor(t, "codex").Transcript("00000000-0000-4000-8000-00000000dead", testRT(t)); err == nil {
		t.Error("want error for a missing rollout")
	}
}
