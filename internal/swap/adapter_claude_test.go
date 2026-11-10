package swap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeCommand(t *testing.T) {
	settings := []string{"--settings", "{dir}/claude.settings.json"}
	runCmdCases(t, "claude", []cmdCase{
		{name: "fresh bare", wantBin: "/opt/bin/claude", wantArgs: settings},
		{name: "fresh prompt args extra", h: Harness{Args: []string{"--effort", "high"}}, prompt: "continue the task",
			extra: []string{"--model", "opus"}, wantBin: "/opt/bin/claude",
			wantArgs: append([]string{"--effort", "high", "--model", "opus", "continue the task"}, settings...)},
		{name: "resume", native: "abc-123", prompt: "go on", wantBin: "/opt/bin/claude",
			wantArgs: append([]string{"--resume", "abc-123", "go on"}, settings...)},
		{name: "bin override", h: Harness{Bin: "/usr/local/bin/claude2"}, wantBin: "/usr/local/bin/claude2", wantArgs: settings},
		{name: "settings named after harness", h: Harness{Name: "claude-api", Target: "gw"}, wantBin: "/opt/bin/claude",
			wantArgs: []string{"--settings", "{dir}/claude-api.settings.json"}},
	})
}

func TestClaudeCommandSettingsFile(t *testing.T) {
	for _, c := range []struct {
		name, target string
		api          bool
	}{{"subscription", "", false}, {"api", "gw", true}} {
		t.Run(c.name, func(t *testing.T) {
			rt := testRT(t)
			h := Harness{Name: "c-" + c.name, Kind: "claude", Target: c.target}
			l, err := adapterFor(t, "claude").Command(h, "", "", nil, rt)
			if err != nil {
				t.Fatal(err)
			}
			if len(l.Env) != 0 {
				t.Errorf("claude Env = %q, want none", l.Env)
			}
			b, err := os.ReadFile(filepath.Join(rt.Dir, h.Name+".settings.json"))
			if err != nil {
				t.Fatal(err)
			}
			var s map[string]any
			if err := json.Unmarshal(b, &s); err != nil {
				t.Fatalf("settings not JSON: %v", err)
			}
			if s["hooks"] == nil || s["statusLine"] == nil {
				t.Errorf("settings missing hooks/statusLine: %s", b)
			}
			if !strings.Contains(string(b), rt.Exe) {
				t.Errorf("settings should call %s: %s", rt.Exe, b)
			}
			_, hasKey := s["apiKeyHelper"]
			if hasKey != c.api || strings.Contains(string(b), "https://gw.example/anthropic") != c.api {
				t.Errorf("apiKeyHelper/base URL present = %v, want %v: %s", hasKey, c.api, b)
			}
		})
	}
}

func TestClaudeTranscript(t *testing.T) {
	rt := testRT(t)
	rt.Cwd = "/Users/test/My Project/app.v2"
	const native = "11111111-2222-4333-8444-555555555555"
	// Every non-alphanumeric cwd char becomes "-" (matches real ~/.claude/projects dirs).
	placeFixture(t, "claude/session.jsonl",
		filepath.Join(rt.Home, ".claude", "projects", "-Users-test-My-Project-app-v2", native+".jsonl"))
	got, err := adapterFor(t, "claude").Transcript(native, rt)
	if err != nil {
		t.Fatal(err)
	}
	assertTranscript(t, got, []string{
		"[user] Please list the files in the synthetic project.",
		"[tool] Bash", "ls -la /synthetic",
		"[assistant] There are three synthetic files.",
		"[user] Thanks, now summarize them.",
		"[assistant] Summary: alpha, beta, gamma.",
	}, []string{"META NOISE", "THINKING", "TOOL OUTPUT", "BLOB", "SIDECHAIN",
		"command-name", "/exit", "local-command-stdout", "Goodbye!", "META RECORD"})
}

func TestClaudeTranscriptMissing(t *testing.T) {
	if _, err := adapterFor(t, "claude").Transcript("no-such-session", testRT(t)); err == nil {
		t.Error("want error for a missing transcript")
	}
}
