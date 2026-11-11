// agent-swap runs an ordered list of coding-agent harnesses in tmux. It moves
// the conversation to the next harness when one nears its limit, then back.
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/jstillwa/agent-swap/internal/swap"
)

const usage = `usage:
  agent-swap [flags] [-- args]          launch the first available harness in tmux
  agent-swap status [--config PATH] [--session DIR]
                                        show per-harness usage, the desired harness and,
                                        for a running session, its pin and mirrors
  agent-swap use <harness|auto> [--session DIR] [--force]
                                        move the running conversation to harness now and
                                        keep it there until that harness hits a limit
  agent-swap sync [harness] [--session DIR]
                                        write the conversation into harness's session
                                        (default: all), to open it outside agent-swap
  agent-swap --version                  print the version

The running session is found from the tmux window's agent-swap pane, or
--session (a dir under state_dir/sessions). --config defaults to
$AGENT_SWAP_CONFIG. Inside a harness, use needs --force.

Harnesses ([[harness]] in the config, in preference order) can be claude, codex,
pi, opencode or agy. Without [[harness]], the order is claude, then claude on the
API target. Args after -- go to the first harness.

flags:
  --config PATH              config file (default $AGENT_SWAP_CONFIG, else ~/.config/agent-swap/config.toml)
  --target NAME              API target when no [[harness]] is set (default: default_target)
  --threshold PCT            switch to the next harness at this usage percent (default 95)
  --switch-back-delay DUR    wait after the reset before switching back (default 10m)
  --poll DUR                 usage check interval (default 30s)

internal (used by tmux and the harness hooks/plugins):
  agent-swap run [flags] [-- args]
  agent-swap statusline | hook <Event> | key <target>
`

// version is set at release build time with -ldflags "-X main.version=vX.Y.Z".
var version = ""

// buildVersion returns the release version, else the module version that
// `go install …@vX.Y.Z` or a build in a git checkout records, else "devel".
func buildVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "devel"
}

func main() {
	code, err := run(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "agent-swap:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func run(args []string) (int, error) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	now := time.Now()
	switch sub {
	case "statusline":
		return 0, swap.Statusline(os.Stdin, os.Stdout, now)
	case "hook":
		if len(args) < 2 {
			return 2, fmt.Errorf("hook needs an event name")
		}
		_ = swap.Hook(args[1], os.Stdin, now) // never fail Claude's hook chain
		return 0, nil
	case "key":
		if len(args) < 2 {
			return 2, fmt.Errorf("key needs a target")
		}
		cfg, err := swap.LoadConfig(configPath(os.Getenv(swap.EnvConfig)))
		if err != nil {
			return 1, err
		}
		return 0, swap.Key(cfg, args[1], os.Stdout)
	case "status", "use", "sync":
		return control(sub, args[1:], now)
	case "run":
		cfg, cfgPath, target, rest, err := parse(args[1:])
		if err != nil {
			return 2, err
		}
		exe, err := self()
		if err != nil {
			return 1, err
		}
		r, err := swap.NewRunner(cfg, cfgPath, target, exe, os.Getenv("TMUX_PANE"), rest)
		if err != nil {
			return 1, err
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGHUP)
		defer stop()
		signal.Ignore(syscall.SIGINT) // Ctrl-C belongs to claude
		return r.Run(ctx)
	case "-h", "--help", "help":
		fmt.Print(usage)
		return 0, nil
	case "-v", "--version", "version":
		fmt.Println("agent-swap", buildVersion())
		return 0, nil
	}
	cfg, _, _, _, err := parse(args) // validate before handing off to tmux
	if err != nil {
		return 2, err
	}
	exe, err := self()
	if err != nil {
		return 1, err
	}
	return 1, swap.StartTmux(cfg.TmuxBin, exe, args)
}

func parse(args []string) (cfg swap.Config, cfgPath, target string, rest []string, err error) {
	fs := flag.NewFlagSet("agent-swap", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	p := fs.String("config", os.Getenv(swap.EnvConfig), "")
	t := fs.String("target", "", "")
	th := fs.Float64("threshold", 0, "")
	delay := fs.Duration("switch-back-delay", 0, "")
	poll := fs.Duration("poll", 0, "")
	if err = fs.Parse(args); err != nil {
		return
	}
	if rest = fs.Args(); len(rest) > 0 && (len(rest) == len(args) || args[len(args)-len(rest)-1] != "--") {
		switch rest[0] {
		case "status", "run", "hook", "statusline", "key", "use", "sync":
			err = fmt.Errorf("put the subcommand first: agent-swap %s --config X", rest[0])
			return
		}
	}
	cfgPath = configPath(*p)
	if cfg, err = swap.LoadConfig(cfgPath); err != nil {
		return
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "threshold":
			cfg.Threshold = *th
		case "switch-back-delay":
			cfg.SwitchBackDelay.Duration = *delay
		case "poll":
			cfg.Poll.Duration = *poll
		}
	})
	if len(cfg.Harnesses) == 0 { // the synthesized order needs a target
		target, err = cfg.ResolveTarget(*t)
	} else if *t != "" {
		fmt.Fprintln(os.Stderr, "agent-swap: warning: --target is ignored when [[harness]] is configured")
	}
	if err == nil {
		err = cfg.Validate(target)
	}
	if err != nil {
		err = fmt.Errorf("%w (config: %s)", err, cfgPath)
	}
	return cfg, cfgPath, target, rest, err
}

// control runs the commands that read or steer a running session.
func control(sub string, args []string, now time.Time) (int, error) {
	fs := flag.NewFlagSet(sub, flag.ContinueOnError)
	p := fs.String("config", os.Getenv(swap.EnvConfig), "")
	dir := fs.String("session", "", "")
	force := fs.Bool("force", false, "")
	var pos []string
	for rest := args; len(rest) > 0; rest = fs.Args()[1:] { // flags may follow the harness name
		if err := fs.Parse(rest); err != nil {
			return 2, err
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
	}
	cfg, err := swap.LoadConfig(configPath(*p))
	if err != nil {
		return 1, err
	}
	target, _ := cfg.ResolveTarget("") // "" → order is claude only
	order := cfg.Order(target)
	name := ""
	if len(pos) > 0 {
		name = pos[0]
	}
	switch {
	case len(pos) > 1 || sub == "status" && len(pos) > 0 || sub == "use" && len(pos) == 0:
		return 2, fmt.Errorf("usage: agent-swap %s; see agent-swap help", sub)
	// cfg.Order with any target lists every configured name: the [[harness]] tables, else claude and api.
	case name != "" && name != "auto" && !slices.ContainsFunc(cfg.Order("api"), func(h swap.Harness) bool { return h.Name == name }):
		return 2, fmt.Errorf("unknown harness %q", name)
	case sub == "sync" && name == "auto":
		return 2, fmt.Errorf("sync takes a harness name")
	case sub == "use" && os.Getenv(swap.EnvHarness) != "" && !*force:
		return 2, fmt.Errorf("refusing to switch from inside a harness; pass --force")
	}
	run, derr := runDir(cfg, *dir)
	switch sub {
	case "use", "sync":
		if derr != nil {
			return 1, derr
		}
		return 0, swap.Update(swap.RequestPath(run), func(q *swap.Request) {
			switch {
			case sub == "sync" && name == "":
				q.Sync = append(q.Sync, "*")
			case sub == "sync":
				q.Sync = append(q.Sync, name)
			case name == "auto":
				q.Pin = ""
			default:
				q.Pin = name
			}
		})
	}
	a, err := swap.Load[swap.Account](swap.AccountPath(cfg.StateDir))
	if err != nil {
		return 1, err
	}
	b, _ := json.MarshalIndent(a, "", "  ")
	fmt.Printf("%s\ndesired harness: %s\n", b, swap.Desired(a, order, now))
	if derr != nil {
		return 0, nil // no running session found: usage only
	}
	q, _ := swap.Load[swap.Request](swap.RequestPath(run))
	ms, _ := swap.Load[map[string]swap.MirrorStatus](swap.MirrorsPath(run))
	fmt.Printf("session: %s\npin: %s\nwanted harness (pin applied): %s\n", run, cmp.Or(q.Pin, "auto"), swap.Want(a, order, now, q.Pin))
	for _, k := range slices.Sorted(maps.Keys(ms)) {
		fmt.Printf("mirror %s: %s (%d turns behind) %s\n", k, ms[k].Native, ms[k].Behind, ms[k].Path)
	}
	return 0, nil
}

// runDir finds the running session's dir: --session, then the harness's own
// $AGENT_SWAP_SESSION_DIR, then the agent-swap pane in this tmux window. It
// must be a real dir (no symlink) directly under state_dir/sessions.
func runDir(cfg swap.Config, dir string) (string, error) {
	if dir == "" {
		dir = os.Getenv(swap.EnvSessionDir)
	}
	if pane := os.Getenv("TMUX_PANE"); dir == "" && pane != "" {
		out, _ := exec.Command(cfg.TmuxBin, "list-panes", "-t", pane, "-F", "#{@agent-swap-dir}").Output()
		var found []string
		for _, d := range strings.Fields(string(out)) {
			if !slices.Contains(found, d) {
				found = append(found, d)
			}
		}
		if len(found) > 1 {
			return "", fmt.Errorf("several agent-swap panes in this window; pass --session")
		}
		if len(found) == 1 {
			dir = found[0]
		}
	}
	if dir == "" {
		return "", fmt.Errorf("no running agent-swap session found; pass --session DIR")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	parent, err1 := filepath.EvalSymlinks(filepath.Dir(abs))
	want, err2 := filepath.EvalSymlinks(filepath.Join(cfg.StateDir, "sessions"))
	fi, err3 := os.Lstat(abs)
	if err := cmp.Or(err1, err2, err3); err != nil {
		return "", fmt.Errorf("session %s: %w", dir, err)
	}
	if parent != want || !fi.IsDir() {
		return "", fmt.Errorf("session %s is not a session dir under %s", dir, want)
	}
	return abs, nil
}

func configPath(p string) string {
	if p == "" {
		return swap.DefaultConfigPath()
	}
	abs, _ := filepath.Abs(p)
	return abs
}

func self() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}
