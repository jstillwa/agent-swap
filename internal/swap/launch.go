package swap

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
)

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// SessionName is as-<dirname>, made unique against existing tmux sessions.
func SessionName(dir string, exists func(string) bool) string {
	base := "as-" + unsafeName.ReplaceAllString(filepath.Base(dir), "-")
	name := base
	for i := 2; exists(name); i++ {
		name = base + "-" + strconv.Itoa(i)
	}
	return name
}

// StartTmux starts `exe run <args>` in tmux: a new window when already inside
// tmux, otherwise a new attached session. It replaces the current process.
func StartTmux(tmuxBin, exe string, runArgs []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	path, err := exec.LookPath(tmuxBin)
	if err != nil {
		return err
	}
	shell := ShellJoin(append([]string{exe, "run"}, runArgs...))
	if os.Getenv("TMUX") != "" {
		return syscall.Exec(path, []string{"tmux", "new-window", "-n", "agent-swap", "-c", cwd, shell}, os.Environ())
	}
	name := SessionName(cwd, func(n string) bool {
		return exec.Command(path, "has-session", "-t", "="+n).Run() == nil
	})
	return syscall.Exec(path, []string{"tmux", "new-session", "-s", name, "-c", cwd, shell}, os.Environ())
}
