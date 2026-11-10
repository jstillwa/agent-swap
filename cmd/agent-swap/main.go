// agent-swap runs Claude Code in tmux and moves the session to API billing
// when subscription usage reaches the threshold, then back after the reset.
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
  agent-swap [flags] [-- claude args]   launch claude in tmux under agent-swap
  agent-swap status [--config PATH]     show shared usage state ($AGENT_SWAP_CONFIG if no flag)

flags:
  --config PATH              config file (default ~/.config/agent-swap/config.toml)
  --target NAME              API target profile (default: default_target, or the only target)
  --threshold PCT            switch to API at this usage percent (default 95)
  --switch-back-delay DUR    wait after the reset before switching back (default 10m)
  --switch-back-timeout DUR  max wait for the touch file when switching back (default 1h)
  --poll DUR                 usage check interval (default 30s)

internal (used by tmux, hooks and Claude Code):
  agent-swap run [flags] [-- claude args]
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
		a, err := swap.Load[swap.Account](swap.AccountPath(cfg.StateDir))
		if err != nil {
			return 1, err
		}
		b, _ := json.MarshalIndent(a, "", "  ")
		fmt.Printf("%s\ndesired mode: %s\n", b, a.Desired(now))
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
	return 1, swap.Launch(cfg.TmuxBin, exe, args)
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
	if target, err = cfg.ResolveTarget(*t); err == nil {
		err = cfg.Validate(target)
	}
	if err != nil {
		err = fmt.Errorf("%w (config: %s)", err, cfgPath)
	}
	return cfg, cfgPath, target, fs.Args(), err
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
