# Contributing

You can send bug reports, fixes and improvements. When you contribute, you agree to license your contribution under the [Hippocratic License 3.0](LICENSE.md).

## Development setup

```sh
git clone https://github.com/jstillwa/claude-swap
cd claude-swap
go test ./...
go install ./cmd/claude-swap
```

Requirements: Go 1.27 or later, and tmux. Only the manual end-to-end test needs Claude Code.

## Layout

```
cmd/claude-swap/main.go     CLI: subcommands and flags
internal/swap/config.go     config file, defaults, message templates
internal/swap/state.go      account.json / session.json, flock + atomic write
internal/swap/logic.go      pure decision logic (Trigger, Decide, StatusText)
internal/swap/helpers.go    statusline, hook, key, generated --settings
internal/swap/runner.go     tmux pane supervisor
internal/swap/launch.go     tmux session/window creation
docs/ARCHITECTURE.md        design and verified Claude Code behaviors
```

## Making a change

1. **Write the test first.** Put decision changes in `logic.go`, tested by `logic_test.go`. Keep I/O out of that file.
2. **If you change the supervisor flow**, update `runner_test.go`. This test operates a real `Runner` with fake `tmux` and `claude` scripts.
3. **Run the checks before you push:**
   ```sh
   gofmt -l .        # must print nothing
   go vet ./...
   go test ./...
   ```
4. **Update the docs** if you change behavior: `README.md` for users, `docs/ARCHITECTURE.md` for design, and `CHANGELOG.md` under *Unreleased*.

## Manual end-to-end test

You can test a real switch before your real limit. Use a threshold below your current usage, and a separate state directory:

```sh
cat > /tmp/cs.toml <<'EOF'
state_dir = "/tmp/cs-state"
poll = "3s"
switch_back_timeout = "20s"
notify = false
EOF
claude-swap --config /tmp/cs.toml --threshold 1 -- --model haiku
```

1. Send one prompt. Claude receives the handoff message, touches the file, and resumes on API billing.
2. To test the switch back, set `api_until` in `/tmp/cs-state/account.json` to the current Unix time.

Note: with `--threshold 1`, the session switches to the API again when the subscription reports usage. This is correct behavior.

The supervisor log is at `/tmp/cs-state/sessions/*/log`.

## Claude Code compatibility

claude-swap depends on the behaviors in [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md#verified-claude-code-behaviors). If a Claude Code release changes one of them, open an issue with:

- the Claude Code version (`claude --version`)
- the supervisor log

## Commit messages

Use [Conventional Commits](https://www.conventionalcommits.org/), for example `fix(runner): retry /exit when the child ignores it`.

## Pull requests

- Keep each pull request to one change.
- Describe the problem and how you tested the change.
- The repository has no CI yet. Paste the output of `go test ./...`.
