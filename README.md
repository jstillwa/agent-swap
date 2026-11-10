# claude-swap

[![License: Hippocratic 3.0](https://img.shields.io/static/v1?label=Hippocratic%20License&message=HL3-FULL&labelColor=5e2751&color=bc8c3d)](https://firstdonoharm.dev/version/3/0/full.html)
![Go](https://img.shields.io/badge/go-1.27%2B-00ADD8)
![Platform](https://img.shields.io/badge/platform-macOS-lightgrey)

claude-swap runs [Claude Code](https://code.claude.com) inside tmux and watches your subscription usage. When any limit window reaches 95%, it does the following:

1. It asks Claude to stop at a safe point.
2. It restarts the **same conversation** on API billing. The API endpoint can be any Anthropic-compatible endpoint, for example an LLM gateway or your own Console key.
3. After the limit resets, it moves the conversation back to the subscription.

```
 SUB 72% (5h) · resets Mon 14:20                                    ← normal
 ⚠ 91% (5h) — TMUX DROPPING YOU IN 4% OF USAGE. Prepare for account cycle!
 🔴 ACCOUNT CYCLE IN PROGRESS — waiting for handoff                 ← 95%
 API (gateway) · back to SUB at Mon 14:30                           ← on API billing
```

## Contents

- [Why](#why)
- [Requirements](#requirements)
- [Install](#install)
- [Quick start](#quick-start)
- [Configuration](#configuration)
- [How a switch works](#how-a-switch-works)
- [Commands](#commands)
- [Files and state](#files-and-state)
- [Caveats](#caveats)
- [Troubleshooting](#troubleshooting)
- [FAQ](#faq)
- [Documentation](#documentation)
- [License](#license)

## Why

Claude Pro, Max and Ultra plans have a 5-hour and a 7-day usage window. When a window is full, Claude Code stops until the window resets. Claude Code has two built-in options:

- **Usage credits.** You keep working past the limit, and your **Claude account** is billed at API rates.
- **Auto-continue.** The session waits and continues after the reset. This works only if the reset is less than 24 hours away.

Use claude-swap when the overflow must go to **a different bill**. Examples are a company LLM gateway or a separate API key. claude-swap moves your live session there and back. Your context stays the same.

## Requirements

| | Version tested | Notes |
|---|---|---|
| Claude Code | 2.1.322 | Needs `rate_limits` in the statusline input (Pro / Max / Ultra plans) and the `StopFailure` hook |
| tmux | 3.8 | |
| Go | 1.27 | To build |
| macOS | 14 | Notifications use `osascript`. Other Unix systems should work without notifications, but this is not tested. |

You also need an Anthropic Messages API endpoint and a command that prints its key. See [Configuration](#configuration).

## Install

```sh
go install github.com/jstillwa/claude-swap/cmd/claude-swap@latest
# or, from a clone:
go install ./cmd/claude-swap
```

The binary goes to `$(go env GOPATH)/bin`, which is usually `~/go/bin`. Make sure that directory is on your `PATH`:

```sh
echo 'export PATH="$HOME/go/bin:$PATH"' >> ~/.zshrc
```

## Quick start

```sh
mkdir -p ~/.config/claude-swap
cp examples/config.toml ~/.config/claude-swap/config.toml   # then edit the target
cd ~/code/my-project
claude-swap                      # same as `claude`, but inside a managed tmux session
claude-swap -- --model opus      # everything after -- goes to claude
```

- **Outside tmux**, claude-swap creates and attaches the session `cs-<dirname>`.
- **Inside tmux**, it opens a new window named `claude-swap`.

Then use Claude Code as usual. Your own statusline, hooks, settings and plugins continue to operate.

## Configuration

Use the file `~/.config/claude-swap/config.toml`. Every key is optional. Command-line flags override the file.

```toml
threshold = 95              # switch to API when any window (5h or 7d) reaches this %
switch_back_delay = "10m"   # how long after the window's reset time to switch back
switch_back_timeout = "1h"  # max wait for Claude's "ready" file when switching back
poll = "30s"                # how often usage is checked
default_target = "gateway"
notify = true               # macOS notification on every switch

[targets.gateway]           # an Anthropic-compatible gateway
base_url = "https://llm-gateway.example.com/anthropic"
key_cmd  = "security find-generic-password -s llm-gateway -w"

[targets.console]           # api.anthropic.com with a Console key
key_cmd = "security find-generic-password -s anthropic-api -w"
```

### Targets

A target is a destination for API billing:

| Key | Meaning |
|---|---|
| `base_url` | Optional. claude-swap sets `ANTHROPIC_BASE_URL` to this value. If you omit it, Claude Code uses `api.anthropic.com`. The endpoint must accept `POST <base_url>/v1/messages`. |
| `key_cmd` | A shell command that prints the API key to stdout. |

**claude-swap never writes your key to a file or an environment variable.** In API mode, claude-swap gives Claude Code the setting `apiKeyHelper = "claude-swap key <target>"`. Each time Claude Code needs a key, that command executes `key_cmd`. Examples of `key_cmd`:

```sh
# macOS Keychain (store it once):
security add-generic-password -s anthropic-api -a "$USER" -w   # prompts for the key
key_cmd = "security find-generic-password -s anthropic-api -w"

# a key another tool already manages:
key_cmd = "jq -r .provider.key ~/.local/share/sometool/auth.json"

# 1Password CLI:
key_cmd = "op read op://Private/Anthropic/credential"
```

claude-swap has no built-in target. Define at least one target. If you define more than one, set `default_target` or use `--target`.

### Messages

You can override the four messages claude-swap types into Claude. Use these placeholders: `{file}`, `{pct}`, `{target}` and `{resets_at}`.

| Key | Sent when | Must include |
|---|---|---|
| `handoff_message` | usage reaches the threshold | `{file}` |
| `resume_message` | after the restart on API billing | |
| `return_message` | after the reset, before going back | `{file}` |
| `return_resume_message` | after the restart on the subscription | |

The default handoff message:

> ⚠ EMERGENCY SHUTDOWN PROCEDURE (claude-swap). Subscription usage at 95%. Pause all work immediately at the nearest safe point: finish or revert any half-applied edit, do not start new tool calls, and note any running background tasks in one line. Your session will be resumed on API billing (gateway) with full context. When ready, run: touch /…/ready-3f9c…

### Other keys

| Key | Default | Meaning |
|---|---|---|
| `statusline_cmd` | `statusLine.command` from `~/.claude/settings.json` | The statusline that claude-swap displays |
| `state_dir` | `~/.local/state/claude-swap` | Shared and per-session state (absolute path) |
| `claude_bin`, `tmux_bin` | `claude`, `tmux` | The executables to run |

### Flags

```
--config PATH              config file (default ~/.config/claude-swap/config.toml)
--target NAME              API target profile (default: default_target, or the only target)
--threshold PCT            switch threshold in percent
--switch-back-delay DUR    e.g. 15m
--switch-back-timeout DUR  e.g. 30m
--poll DUR                 e.g. 10s
```

## How a switch works

```
   ┌──────────────── tmux pane ────────────────┐
   │ claude-swap run   (supervisor)            │
   │   └─ claude --settings <generated> [--resume <id>]
   │        ├─ statusline → claude-swap statusline ─┐
   │        └─ hooks      → claude-swap hook <Ev>  ─┤
   └────────────────────────────────────────────────┘
                     ▲ send-keys          │ writes
                     │                    ▼
             ~/.local/state/claude-swap/account.json   (shared)
                                   sessions/<id>/session.json
```

**Subscription → API:**

1. Claude Code sends `rate_limits` to the statusline on every update. claude-swap records it.
2. When any window that has not reset yet reaches the `threshold`, claude-swap types the handoff message into the pane. The message names a one-time *ready file*.
3. claude-swap then waits, **with no timeout**, for one of these:
   - Claude runs `touch <file>`.
   - A request fails with a rate-limit error (the `StopFailure` hook with `error: rate_limit`).
   - Usage reaches 100%.
4. When Claude is **idle**, claude-swap sends `/exit`. Idle means its last turn ended, as reported by the `Stop` or `StopFailure` hook. A turn is never cut off.
5. claude-swap relaunches `claude --resume <session-id>` with the target's `apiKeyHelper` and `ANTHROPIC_BASE_URL`, then types the resume message.

**API → subscription:**

1. This starts at the stored reset time plus `switch_back_delay`.
2. The steps are the same, except the wait for the ready file is limited to `switch_back_timeout`. After that, the switch happens at the next idle point.
3. The API session continues to operate during the wait.

**With several sessions:** Anthropic counts usage across your whole account. Thus all wrapped sessions share `account.json` and switch together. Unwrapped `claude` sessions also use the same limits, but claude-swap does not manage them.

## Commands

```sh
claude-swap [flags] [-- claude args]   # launch in tmux (the normal entry point)
claude-swap status                     # shared usage state and the desired mode
claude-swap --help
```

Claude Code and tmux call these internal subcommands:

```sh
claude-swap run [flags] [-- claude args]   # the pane supervisor
claude-swap statusline                     # statusLine command
claude-swap hook <Event>                   # SessionStart | UserPromptSubmit | Stop | StopFailure
claude-swap key <target>                   # apiKeyHelper
```

## Files and state

| Path | Contents |
|---|---|
| `~/.config/claude-swap/config.toml` | your configuration |
| `~/.local/state/claude-swap/account.json` | the latest rate limits, `api_until`, `resets_at` (shared and locked) |
| `~/.local/state/claude-swap/sessions/<id>/session.json` | Claude session ID, idle/busy state, rate-limit flag |
| `~/.local/state/claude-swap/sessions/<id>/log` | the supervisor's log: triggers, handoffs, switches |
| `~/.local/state/claude-swap/sessions/<id>/{sub,api}.settings.json` | the generated `--settings` files (no secrets) |

## Caveats

- **Background work stops at a switch.** `/exit` stops Claude's background shell commands and subagents. The handoff message tells Claude to list them. After the resume, Claude can start them again.
- **claude.ai connectors are off on API billing.** Claude Code turns off claude.ai MCP connectors whenever a credential other than your claude.ai login is in use.
- **Built-in auto-continue doesn't run in wrapped sessions.** claude-swap switches billing instead of waiting.
- **Usage is unknown until the first response.** `rate_limits` appears only after the first API response of a subscription session. On API billing it is absent. For this reason, claude-swap stores the reset time at the handoff.
- **`--threshold` and `/usage` can differ.** The threshold uses the percentage Claude Code reports, which can differ slightly from the claude.ai usage page.
- **The tmux status bar is per session.** claude-swap sets `status-right` for the tmux session it runs in.

## Troubleshooting

| Symptom | What to check |
|---|---|
| The status bar shows `SUB · usage unknown` | Send one prompt. `rate_limits` appears after the first response, and only on Pro, Max or Ultra plans. |
| Nothing happens at the threshold | Check `claude-swap status` and the session `log`. Is `poll` very long? |
| The API session fails to authenticate | Run `claude-swap key <target>` yourself. It must print exactly one key. |
| The API session returns 404 or 405 | `base_url` must be the prefix before `/v1/messages`. For example, a gateway may serve it at `/anthropic/v1/messages`. |
| Claude never touches the file | The switch to API also starts at a rate-limit error or 100% usage. The switch back stops the wait after `switch_back_timeout`. |
| `claude-swap run must be started inside tmux` | Start it with `claude-swap`, not `claude-swap run`. |

## FAQ

**Is this against Anthropic's terms?** claude-swap only operates the official Claude Code CLI through its documented features: `--settings`, `--resume`, hooks, `apiKeyHelper` and the statusline. Your subscription and your API key each have their own terms. You must verify that your use obeys them.

**Why not use usage credits?** If your Claude account can pay for the overflow, enable usage credits and do not use this tool. Usage credits need no restart.

**Does the conversation really continue?** Yes. `claude --resume <id>` reloads the full transcript. Each provider has its own prompt cache. Thus the first request after a switch writes the full cache again.

**Can I force a switch?** Yes. Edit `api_until` in `account.json`. Set it to a future Unix time for API billing. Set it to `0` for the subscription.

## Documentation

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md): the design, the state machine, and the Claude Code behaviors it depends on
- [CONTRIBUTING.md](CONTRIBUTING.md): development setup, tests, and pull requests
- [SECURITY.md](SECURITY.md): how keys are handled and how to report vulnerabilities
- [CHANGELOG.md](CHANGELOG.md)

## License

Copyright 2026 John Stillwagen. The license is the [Hippocratic License 3.0](LICENSE.md) (HL3-FULL), an ethical source license. See <https://firstdonoharm.dev>.

claude-swap is not affiliated with or endorsed by Anthropic. "Claude" and "Claude Code" are trademarks of Anthropic, PBC.
