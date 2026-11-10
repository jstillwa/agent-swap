package swap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPiCommand(t *testing.T) {
	ext := []string{"-e", "{dir}/agent-swap-pi.ts"}
	runCmdCases(t, "pi", []cmdCase{
		{name: "fresh bare", wantBin: "pi", wantArgs: ext},
		{name: "fresh prompt args extra", h: Harness{Args: []string{"--provider", "deepseek", "--model", "deepseek-chat"}},
			extra: []string{"--thinking", "low"}, prompt: "read ctx.md", wantBin: "pi",
			wantArgs: append(append([]string{}, ext...), "--provider", "deepseek", "--model", "deepseek-chat", "--thinking", "low", "read ctx.md")},
		{name: "resume", native: "01a19ad1-uuid", prompt: "go on", wantBin: "pi",
			wantArgs: append(append([]string{}, ext...), "--session", "01a19ad1-uuid", "go on")},
		{name: "bin override", h: Harness{Bin: "/opt/pi"}, wantBin: "/opt/pi", wantArgs: ext},
	})
}

func TestPiCommandWritesExtension(t *testing.T) {
	rt := testRT(t)
	if _, err := adapterFor(t, "pi").Command(Harness{Name: "pi", Kind: "pi"}, "", "", nil, rt); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(rt.Dir, "agent-swap-pi.ts"))
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"AGENT_SWAP_EXE", "SessionStart", "UserPromptSubmit", "StopFailure"} {
		if !strings.Contains(string(b), w) {
			t.Errorf("pi extension missing %q", w)
		}
	}
}

func TestPiTranscript(t *testing.T) {
	rt := testRT(t)
	const native = "01a19ad1-179b-7068-b050-7d0d0948a8fb"
	dir := filepath.Join(rt.Home, ".pi", "agent", "sessions", "--work-proj--")
	placeFixture(t, "pi/session.jsonl", filepath.Join(dir, "2026-11-02T05-52-36-763Z_"+native+".jsonl"), "__ID__", native)
	writeFile(t, filepath.Join(dir, "2026-11-01T00-00-00-000Z_01a19ad1-0000-0000-0000-000000000000.jsonl"),
		`{"type":"message","message":{"role":"user","content":"DECOY SESSION SHOULD NOT APPEAR"}}`+"\n")
	got, err := adapterFor(t, "pi").Transcript(native, rt)
	if err != nil {
		t.Fatal(err)
	}
	assertTranscript(t, got, []string{
		"[user] Rename the synthetic widget.",
		"[tool] bash", "grep -rn widget /synthetic",
		"[assistant] Renamed widget to gadget.",
		"[user] Plain string prompt from the user.",
		"[assistant] Acknowledged the plain prompt.",
	}, []string{"SYSTEM PROMPT", "THINKING", "TOOL OUTPUT", "DECOY"})
}

func TestPiTranscriptMissing(t *testing.T) {
	if _, err := adapterFor(t, "pi").Transcript("no-such-session", testRT(t)); err == nil {
		t.Error("want error for a missing session")
	}
}
