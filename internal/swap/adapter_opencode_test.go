package swap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestOpencodeCommand(t *testing.T) {
	runCmdCases(t, "opencode", []cmdCase{
		{name: "fresh bare", wantBin: "opencode", wantArgs: []string{}},
		{name: "fresh prompt args extra", h: Harness{Args: []string{"-m", "gateway/glm"}}, extra: []string{"--agent", "build"},
			prompt: "read ctx.md", wantBin: "opencode", wantArgs: []string{"-m", "gateway/glm", "--agent", "build", "--prompt", "read ctx.md"}},
		{name: "resume", native: "ses_abc", prompt: "go on", wantBin: "opencode",
			wantArgs: []string{"-s", "ses_abc", "--prompt", "go on"}},
		{name: "bin override", h: Harness{Bin: "/opt/oc"}, wantBin: "/opt/oc", wantArgs: []string{}},
	})
}

func TestOpencodeCommandConfigAndPlugin(t *testing.T) {
	rt := testRT(t)
	// Put Dir and TmpDir behind a symlink (like macOS /var → /private/var): opencode checks
	// resolved paths, so both the given and the resolved form must be allowed.
	real, link := t.TempDir(), filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"dir", "tmp"} {
		if err := os.Mkdir(filepath.Join(real, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	rt.Dir, rt.TmpDir = filepath.Join(link, "dir"), filepath.Join(link, "tmp")
	l, err := adapterFor(t, "opencode").Command(Harness{Name: "oc", Kind: "opencode"}, "", "", nil, rt)
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(rt.Dir, "opencode.json")
	for _, e := range []string{"OPENCODE_CONFIG=" + cfgPath, "OPENCODE_DISABLE_AUTOUPDATE=1"} {
		if !slices.Contains(l.Env, e) {
			t.Errorf("Env %q missing %q", l.Env, e)
		}
	}
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Plugin     []string `json:"plugin"`
		Permission struct {
			ExternalDirectory map[string]string `json:"external_directory"`
		} `json:"permission"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("opencode.json: %v: %s", err, b)
	}
	plugin := filepath.Join(rt.Dir, "agent-swap-opencode.js")
	if !slices.Equal(cfg.Plugin, []string{"file://" + plugin}) {
		t.Errorf("plugin = %q, want [file://%s]", cfg.Plugin, plugin)
	}
	if st, err := os.Stat(plugin); err != nil || st.Size() == 0 {
		t.Errorf("plugin file not written: %v", err)
	}
	// The agent must be able to read the context file and write handoff notes without a permission prompt.
	resolved := func(d string) string {
		r, err := filepath.EvalSymlinks(d)
		if err != nil || r == d {
			t.Fatalf("EvalSymlinks(%s) = %q, %v; want a different path", d, r, err)
		}
		return r
	}
	for _, d := range []string{rt.Dir, rt.TmpDir, resolved(rt.Dir), resolved(rt.TmpDir)} {
		if got := cfg.Permission.ExternalDirectory[filepath.Join(d, "*")]; got != "allow" {
			t.Errorf("external_directory[%s/*] = %q, want allow: %s", d, got, b)
		}
	}
}

func TestOpencodeCommandSkipsEmptyTmpDir(t *testing.T) {
	rt := testRT(t)
	rt.TmpDir = ""
	if _, err := adapterFor(t, "opencode").Command(Harness{Name: "oc", Kind: "opencode"}, "", "", nil, rt); err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Permission struct {
			ExternalDirectory map[string]string `json:"external_directory"`
		} `json:"permission"`
	}
	b, _ := os.ReadFile(filepath.Join(rt.Dir, "opencode.json"))
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	for k := range cfg.Permission.ExternalDirectory {
		if k == "*" || k == "/*" {
			t.Errorf("empty TmpDir must not grant %q", k)
		}
	}
}

// tFakeOpencode puts an `opencode` on PATH that answers `export ses_synthetic` with the fixture.
func tFakeOpencode(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	fixture, err := filepath.Abs(filepath.Join("testdata", "opencode", "export.json"))
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nif [ \"$1\" = export ] && [ \"$2\" = ses_synthetic ]; then cat '" + fixture +
		"'; exit 0; fi\necho \"Session not found: $2\" >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "opencode"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestOpencodeTranscript(t *testing.T) {
	tFakeOpencode(t)
	got, err := adapterFor(t, "opencode").Transcript("ses_synthetic", testRT(t))
	if err != nil {
		t.Fatal(err)
	}
	assertTranscript(t, got, []string{
		"[user] Show the synthetic config.",
		"[tool] bash", "cat synthetic.toml",
		"[assistant] The synthetic config sets mode = demo.",
	}, []string{"REASONING", "TOOL OUTPUT", "step-start"})
}

func TestOpencodeTranscriptExportFails(t *testing.T) {
	tFakeOpencode(t)
	if _, err := adapterFor(t, "opencode").Transcript("ses_missing", testRT(t)); err == nil {
		t.Error("want error when `opencode export` fails")
	}
}
