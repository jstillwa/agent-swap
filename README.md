# agent-swap

[![License: Hippocratic 3.0](https://img.shields.io/static/v1?label=Hippocratic%20License&message=HL3-FULL&labelColor=5e2751&color=bc8c3d)](https://firstdonoharm.dev/version/3/0/full.html)
![Go](https://img.shields.io/badge/go-1.27%2B-00ADD8)
![Platform](https://img.shields.io/badge/platform-macOS-lightgrey)

agent-swap runs [Claude Code](https://code.claude.com) inside tmux and watches your subscription usage. When any limit window reaches 95%, it does the following:

1. It asks Claude to stop at a safe point.
2. It restarts the **same conversation** on API billing. The API endpoint can be any Anthropic-compatible endpoint, for example an LLM gateway or your own Console key.
3. After the limit resets, it moves the conversation back to the subscription.

agent-swap can also use other coding agents as fallbacks: Codex CLI, pi, opencode and Antigravity CLI. See [Harnesses](#harnesses).

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
- [Harnesses](#harnesses)
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

Use agent-swap when the overflow must go to **a different bill**. Examples are a company LLM gateway or a separate API key. agent-swap moves your live session there and back. Your context stays the same.

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
go install github.com/jstillwa/agent-swap/cmd/agent-swap@latest
# or, from a clone:
go install ./cmd/agent-swap
```

The binary goes to `$(go env GOPATH)/bin`, which is usually `~/go/bin`. Make sure that directory is on your `PATH`:

```sh
echo 'export PATH="$HOME/go/bin:$PATH"' >> ~/.zshrc
```

## Quick start

```sh
mkdir -p ~/.config/agent-swap
cp examples/config.toml ~/.config/agent-swap/config.toml   # then edit the target
cd ~/code/my-project
agent-swap                      # same as `claude`, but inside a managed tmux session
agent-swap -- --model opus      # everything after -- goes to claude
```

agent-swap gives all claude arguments to the first launch. A relaunch after a switch gets only the persistent options, such as `--model`. It does not get a prompt, `--continue`, `--resume`, `--session-id`, `--fork-session`, `--print`, `--from-pr`, `--teleport`, `--cloud`, `--tmux`, `--bg`/`--background` or `--desktop`. A relaunch keeps `-w`/`--worktree`. A resume inside a worktree is not tested.

- **Outside tmux**, agent-swap creates and attaches the session `as-<dirname>`.
- **Inside tmux**, it opens a new window named `agent-swap`.

Then use Claude Code as usual. Your own statusline, hooks, settings and plugins continue to operate.

## Configuration

Use the file `~/.config/agent-swap/config.toml`. Every key is optional. Command-line flags override the file.

agent-swap checks the config before it starts tmux or claude. A bad value stops it with exit code 2. The error names the key. These are the limits:

- `threshold` is more than 0 and not more than 100.
- `switch_back_delay` is 0 or more. `switch_back_timeout` and `poll` are more than 0.
- `claude_bin`, `tmux_bin` and `state_dir` are not empty.
- The selected target has a `key_cmd`.

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
| `base_url` | Optional. agent-swap sets `ANTHROPIC_BASE_URL` to this value. If you omit it, Claude Code uses `api.anthropic.com`. The endpoint must accept `POST <base_url>/v1/messages`. |
| `key_cmd` | A shell command that prints the API key to stdout. |

**agent-swap never writes your key to a file or an environment variable.** In API mode, agent-swap gives Claude Code the setting `apiKeyHelper = "agent-swap key <target>"`. Each time Claude Code needs a key, that command executes `key_cmd`. Examples of `key_cmd`:

```sh
# macOS Keychain (store it once):
security add-generic-password -s anthropic-api -a "$USER" -w   # prompts for the key
key_cmd = "security find-generic-password -s anthropic-api -w"

# a key another tool already manages:
key_cmd = "jq -r .provider.key ~/.local/share/sometool/auth.json"

# 1Password CLI:
key_cmd = "op read op://Private/Anthropic/credential"
```

agent-swap has no built-in target. Define at least one target. If you define more than one, set `default_target` or use `--target`.

### Messages

You can override the three messages agent-swap types into a harness. Use these placeholders: `{from}`, `{to}`, `{file}`, `{notes}`, `{context}`, `{reason}`, `{pct}` and `{resets_at}`. `{reason}` is the cause of the switch, for example the usage percentage or a limit error. The default handoff message uses it. `{file}` and `{notes}` are shell-quoted paths.

| Key | Sent when | Must include |
|---|---|---|
| `handoff_message` | before a switch down the list (the current harness nears its limit) | `{file}` |
| `return_message` | before a switch back up the list (a preferred harness is free again) | `{file}` |
| `resume_message` | as the first prompt to the next harness | |

The default handoff message starts with `⚠ EMERGENCY SHUTDOWN PROCEDURE (agent-swap)`. It asks the agent to stop at a safe point, write handoff notes to `{notes}` and then run `touch {file}`. The default resume message:

> agent-swap moved this conversation from {from} to {to}. Read {context} — it has the handoff notes and the conversation so far — then continue the task.

### Other keys

| Key | Default | Meaning |
|---|---|---|
| `statusline_cmd` | `statusLine.command` from `~/.claude/settings.json` | The statusline that agent-swap displays |
| `state_dir` | `~/.local/state/agent-swap` | Shared and per-session state (absolute path) |
| `claude_bin`, `tmux_bin` | `claude`, `tmux` | The executables to run |

### Flags

```
--config PATH              config file (default ~/.config/agent-swap/config.toml)
--target NAME              API target profile (default: default_target, or the only target)
--threshold PCT            switch threshold in percent
--switch-back-delay DUR    e.g. 15m
--switch-back-timeout DUR  e.g. 30m
--poll DUR                 e.g. 10s
```

## Harnesses

A harness is one coding-agent CLI that agent-swap can run in the pane. List harnesses with `[[harness]]` tables. The order is the preference order. agent-swap runs the first harness that is not blocked. When that harness nears or hits its limit, agent-swap moves the conversation to the next harness. When a harness higher in the list is free again, agent-swap moves the conversation back.

Without `[[harness]]` tables, the order is `claude`, then `api` (claude on the target from `default_target` or `--target`). This is the subscription/API behavior that the earlier sections describe.

```toml
[[harness]]
name = "claude"
kind = "claude"

[[harness]]
name = "codex"
kind = "codex"
args = ["-m", "gpt-5.5"]
```

[examples/config.toml](examples/config.toml) has a full example with four harnesses.

| Key | Meaning |
|---|---|
| `name` | Required and unique. State and the status bar use it. |
| `kind` | Required. One of `claude`, `codex`, `pi`, `opencode`, `agy`. |
| `bin` | The executable. Default: `claude_bin` for `claude`, else the kind. |
| `args` | Arguments that agent-swap always passes to this harness. |
| `target` | `claude` only. Use API billing through this target. The target must exist and have a `key_cmd`. |
| `threshold`, `cooldown` | Override the top-level values for this harness. |

Arguments after `--` go to the first launch. A later launch gets them only if its harness has the same kind as the first harness.

### Supported kinds

`claude`, `codex`, `pi`, `opencode` and `agy`. All five passed a live switch. The [kinds table](docs/ARCHITECTURE.md#adapters) gives the tested version, the signal source and the transcript source of each kind.

### Limits

- **`claude` and `codex`** report usage percentages. agent-swap switches at the `threshold`, before the limit. It switches back at the window reset plus `switch_back_delay`. Codex on paid credits is not limited: only `rate_limit_reached_type` or a limit error counts.
- **Other kinds** report no usage. agent-swap switches after a limit error. A limit error is text that contains `rate limit`, `429`, `quota`, `usage limit`, `budget exceeded`, `resource_exhausted`, `insufficient_balance` or `402`. If the error gives no reset time, the harness stays blocked for `cooldown`.
- **If all harnesses are blocked**, agent-swap runs the harness that becomes free first.

### How the conversation carries over

Each harness keeps its own native session. At a switch, the agent writes handoff notes. agent-swap then gives the next harness a context file with the notes and the transcript it has not seen. A switch back resumes the native session of that harness. [Context building](docs/ARCHITECTURE.md#context-building) gives the steps. `context_max_bytes` (default 60000) limits the context file.

### Known limits and unverified items

- **Ping-pong.** A harness with no reset time is blocked only for `cooldown`. If it is still limited after that, agent-swap switches to it and away again once per `cooldown`.
- **`OPENCODE_CONFIG`.** agent-swap sets it for opencode. It replaces any value that you set.
- **Quit command for `codex`** is not tested. If the harness does not exit, agent-swap escalates. See [Decide](docs/ARCHITECTURE.md#decide-per-session).
- **`agy` transcript.** Antigravity stores steps as protobuf in SQLite. agent-swap extracts only the readable text. The segment starts with `[agy transcript, lossy]`.
- **`agy` quota text** is not tested. agent-swap looks for `Quota exhausted` and the limit-error words in the agy log. The real message can differ.

## How a switch works

1. agent-swap records usage and idle state from the hooks, the plugins or the session files of the harness.
2. When the current harness reaches the `threshold` or hits a limit, agent-swap types the handoff message into the pane. The message names a one-time *ready file* and a notes file.
3. agent-swap waits for the ready file, a limit error or 100% usage. This wait has no timeout. A switch back up the list waits at most `switch_back_timeout`.
4. When the harness is **idle**, agent-swap types its quit command. A turn is never cut off.
5. agent-swap starts the next harness with the context file, or resumes its native session.

All wrapped sessions share `account.json` and switch together. [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md#decision-logic) gives the exact rules.

## Commands

```sh
agent-swap [flags] [-- claude args]   # launch in tmux (the normal entry point)
agent-swap status [--config PATH]     # per-harness usage and the desired harness
agent-swap --help
```

Claude Code and tmux call these internal subcommands:

```sh
agent-swap run [flags] [-- claude args]   # the pane supervisor
agent-swap statusline                     # statusLine command
agent-swap hook <Event>                   # SessionStart | UserPromptSubmit | Stop | StopFailure | LimitHit
agent-swap key <target>                   # apiKeyHelper
```

`status` reads the config from `--config`. If you omit the flag, it uses `$AGENT_SWAP_CONFIG`, then the default path.

## Files and state

| Path | Contents |
|---|---|
| `~/.config/agent-swap/config.toml` | your configuration |
| `~/.local/state/agent-swap/account.json` | per harness: usage windows, `blocked_until`, `resets_at` (shared and locked) |
| `~/.local/state/agent-swap/sessions/<id>/session.json` | native session ID of the running harness, idle/busy state, rate-limit flag |
| `$TMPDIR/agent-swap-<session>/` | the ready and notes files. A sandboxed codex can write there. agent-swap removes this directory on a clean exit. |
| `~/.local/state/agent-swap/sessions/<id>/{segment,context}-*.md` | transcript segments and the context file for the next harness |
| `~/.local/state/agent-swap/sessions/<id>/agent-swap-pi.ts`, `opencode.json`, `agent-swap-opencode.js` | the generated pi extension and opencode plugin |
| `~/.local/state/agent-swap/sessions/<id>/log` | the supervisor's log: triggers, handoffs, switches |
| `~/.local/state/agent-swap/sessions/<id>/<name>.settings.json` | the generated claude `--settings` files (no secrets) |

## Caveats

- **Every harness must already trust the project directory.** Claude Code and Codex each show a folder-trust dialog in a new directory. An unattended switch stops at that dialog. Start each harness in the directory once and accept the dialog before you use agent-swap there.
- **Background work stops at a switch.** `/exit` stops Claude's background shell commands and subagents. The handoff message tells Claude to list them. After the resume, Claude can start them again.
- **claude.ai connectors are off on API billing.** Claude Code turns off claude.ai MCP connectors whenever a credential other than your claude.ai login is in use.
- **Built-in auto-continue doesn't run in wrapped sessions.** agent-swap switches billing instead of waiting.
- **Usage is unknown until the first response.** `rate_limits` appears only after the first API response of a subscription session. On API billing it is absent. For this reason, agent-swap stores the reset time at the handoff.
- **`--threshold` and `/usage` can differ.** The threshold uses the percentage Claude Code reports, which can differ slightly from the claude.ai usage page.
- **The tmux status bar is per session.** agent-swap sets `status-right` for the tmux session it runs in.

## Troubleshooting

| Symptom | What to check |
|---|---|
| The status bar shows `SUB · usage unknown` | Send one prompt. `rate_limits` appears after the first response, and only on Pro, Max or Ultra plans. |
| Nothing happens at the threshold | Check `agent-swap status` and the session `log`. Is `poll` very long? |
| The API session fails to authenticate | Run `agent-swap key <target>` yourself. It must print exactly one key. |
| The API session returns 404 or 405 | `base_url` must be the prefix before `/v1/messages`. For example, a gateway may serve it at `/anthropic/v1/messages`. |
| Claude never touches the file | The switch to API also starts at a rate-limit error or 100% usage. The switch back stops the wait after `switch_back_timeout`. |
| `agent-swap run must be started inside tmux` | Start it with `agent-swap`, not `agent-swap run`. |

## FAQ

**Is this against Anthropic's terms?** agent-swap only operates the official Claude Code CLI through its documented features: `--settings`, `--resume`, hooks, `apiKeyHelper` and the statusline. Your subscription and your API key each have their own terms. You must verify that your use obeys them.

**Why not use usage credits?** If your Claude account can pay for the overflow, enable usage credits and do not use this tool. Usage credits need no restart.

**Does the conversation really continue?** Yes. `claude --resume <id>` reloads the full transcript. Each provider has its own prompt cache. Thus the first request after a switch writes the full cache again.

**Can I force a switch?** Yes. Edit `harnesses.<name>.blocked_until` in `account.json`. Set it to a future Unix time to move away from that harness. Set it to `0` to allow it again. A forced return stays only while the harness's usage is below the threshold. If usage is at or above the threshold, the next poll blocks it again.

## Documentation

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md): the design, the state machine, and the Claude Code behaviors it depends on
- [CONTRIBUTING.md](CONTRIBUTING.md): development setup, tests, and pull requests
- [SECURITY.md](SECURITY.md): how keys are handled and how to report vulnerabilities
- [CHANGELOG.md](CHANGELOG.md)

## License

Copyright 2026 John Stillwagen. The license is the [Hippocratic License 3.0](LICENSE.md) (HL3-FULL), an ethical source license. See <https://firstdonoharm.dev>.

agent-swap is not affiliated with or endorsed by Anthropic. "Claude" and "Claude Code" are trademarks of Anthropic, PBC.
