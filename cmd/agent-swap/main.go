// agent-swap runs an ordered list of coding-agent harnesses in tmux. It moves
// the conversation to the next harness when one nears its limit, then back.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jstillwa/agent-swap/internal/swap"
)

const usage = `usage:
  agent-swap [flags] [-- args]          launch the first available harness in tmux
  agent-swap status [--config PATH]     show per-harness usage and the desired harness
                                        ($AGENT_SWAP_CONFIG if no flag)

Harnesses ([[harness]] in the config, in preference order) can be claude, codex,
pi, opencode or agy. Without [[harness]], the order is claude, then claude on the
API target. Args after -- go to the first harness.

flags:
  --config PATH              config file (default ~/.config/agent-swap/config.toml)
  --target NAME              API target when no [[harness]] is set (default: default_target)
  --threshold PCT            switch to the next harness at this usage percent (default 95)
  --switch-back-delay DUR    wait after the reset before switching back (default 10m)
  --switch-back-timeout DUR  max wait for the touch file when switching back (default 1h)
  --poll DUR                 usage check interval (default 30s)

internal (used by tmux and the harness hooks/plugins):
  agent-swap run [flags] [-- args]
  agent-swap statusline | hook <Event> | key <target>
`

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
	case "status":
		fs := flag.NewFlagSet("status", flag.ContinueOnError)
		p := fs.String("config", os.Getenv(swap.EnvConfig), "")
		if err := fs.Parse(args[1:]); err != nil {
			return 2, err
		}
		cfg, err := swap.LoadConfig(configPath(*p))
		if err != nil {
			return 1, err
		}
		target, _ := cfg.ResolveTarget("") // "" → order is claude only
		a, err := swap.Load[swap.Account](swap.AccountPath(cfg.StateDir))
		if err != nil {
			return 1, err
		}
		b, _ := json.MarshalIndent(a, "", "  ")
		fmt.Printf("%s\ndesired harness: %s\n", b, swap.Desired(a, cfg.Order(target), now))
		return 0, nil
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
	p := fs.String("config", "", "")
	t := fs.String("target", "", "")
	th := fs.Float64("threshold", 0, "")
	delay := fs.Duration("switch-back-delay", 0, "")
	timeout := fs.Duration("switch-back-timeout", 0, "")
	poll := fs.Duration("poll", 0, "")
	if err = fs.Parse(args); err != nil {
		return
	}
	if rest = fs.Args(); len(rest) > 0 && (len(rest) == len(args) || args[len(args)-len(rest)-1] != "--") {
		switch rest[0] {
		case "status", "run", "hook", "statusline", "key":
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
		case "switch-back-timeout":
			cfg.SwitchBackTimeout.Duration = *timeout
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
