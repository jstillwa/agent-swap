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

// SessionName is cs-<dirname>, made unique against existing tmux sessions.
func SessionName(dir string, exists func(string) bool) string {
	base := "cs-" + unsafeName.ReplaceAllString(filepath.Base(dir), "-")
	name := base
	for i := 2; exists(name); i++ {
		name = base + "-" + strconv.Itoa(i)
	}
	return name
}

// Launch starts `exe run <args>` in tmux: a new window when already inside
// tmux, otherwise a new attached session. It replaces the current process.
func Launch(tmuxBin, exe string, runArgs []string) error {
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
		return syscall.Exec(path, []string{"tmux", "new-window", "-n", "claude-swap", "-c", cwd, shell}, os.Environ())
	}
	name := SessionName(cwd, func(n string) bool {
		return exec.Command(path, "has-session", "-t", "="+n).Run() == nil
	})
	return syscall.Exec(path, []string{"tmux", "new-session", "-s", name, "-c", cwd, shell}, os.Environ())
}
