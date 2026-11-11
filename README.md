# agent-swap

[![License: Hippocratic 3.0](https://img.shields.io/static/v1?label=Hippocratic%20License&message=HL3-FULL&labelColor=5e2751&color=bc8c3d)](https://firstdonoharm.dev/version/3/0/full.html)
![Go](https://img.shields.io/badge/go-1.27%2B-00ADD8)
![Platform](https://img.shields.io/badge/platform-macOS-lightgrey)

agent-swap runs [Claude Code](https://code.claude.com) inside tmux and watches your subscription usage. When any limit window reaches 95%, it does the following:

1. It waits until Claude is idle, so a turn is never cut off.
2. It restarts the **same conversation** on API billing. The API endpoint can be any Anthropic-compatible endpoint, for example an LLM gateway or your own Console key.
3. After the limit resets, it moves the conversation back to the subscription.

agent-swap can also use other coding agents as fallbacks: Codex CLI, pi, opencode and Antigravity CLI. See [Harnesses](#harnesses).

```
 SUB 72% (5h) · resets Mon 14:20                                    ← normal
 ⚠ 91% (5h) — TMUX DROPPING YOU IN 4% OF USAGE. Prepare for account cycle!
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
- [Harness swap](#harness-swap)
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
| macOS | 14 | Notifications use `osascript`. Other Unix systems are not tested, and they get no notifications. |

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
- `switch_back_delay` is 0 or more. `poll` is more than 0.
- `claude_bin`, `tmux_bin` and `state_dir` are not empty.
- The selected target has a `key_cmd`.

```toml
threshold = 95              # switch to API when any window (5h or 7d) reaches this %
switch_back_delay = "10m"   # how long after the window's reset time to switch back
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

### Resume message

`resume_message` is the first prompt for a harness that gets a context file instead of a session. See [How a switch works](#how-a-switch-works). Placeholders: `{from}`, `{to}` and `{context}`. The default:

> agent-swap moved this conversation from {from} to {to}. Read {context}: it has the conversation so far. Then continue where it left off; if it ended waiting for the user, ask again.

`handoff_message`, `return_message` and `switch_back_timeout` are removed. agent-swap prints a warning if the config sets one of them.

### Other keys

| Key | Default | Meaning |
|---|---|---|
| `statusline_cmd` | `statusLine.command` from `~/.claude/settings.json` | The statusline that agent-swap displays |
| `state_dir` | `~/.local/state/agent-swap` | Shared and per-session state (absolute path) |
| `claude_bin`, `tmux_bin` | `claude`, `tmux` | The executables to run |
| `context_max_bytes` | `200000` | Cap on the conversation text written into one harness's session or context file (newest kept). A `[[harness]]` can set its own value. |

### Flags

```
--config PATH              config file (default $AGENT_SWAP_CONFIG, then ~/.config/agent-swap/config.toml)
--target NAME              API target profile (default: default_target, or the only target)
--threshold PCT            switch threshold in percent
--switch-back-delay DUR    e.g. 15m
--poll DUR                 e.g. 10s
```

## Harnesses

A harness is one coding-agent CLI that agent-swap can run in the pane. List harnesses with `[[harness]]` tables. The order is the preference order. agent-swap runs the first harness that is not blocked. When that harness nears or hits its limit, agent-swap moves the conversation to the next harness. When a harness higher in the list is free again, agent-swap moves the conversation back.

Without `[[harness]]` tables, the order is `claude`, then `api` (claude on the target from `default_target` or `--target`). The earlier sections describe this subscription and API behavior.

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
| `context_max_bytes` | Override the top-level cap for this harness. Use a small value for a model with a small context window. |

Arguments after `--` go to the first launch. A later launch gets them only if its harness has the same kind as the first harness.

### Supported kinds

`claude`, `codex`, `pi`, `opencode` and `agy`. All five passed a live switch. The [kinds table](docs/ARCHITECTURE.md#adapters) gives the tested version, the signal source and the transcript source of each kind.

### Limits

- **`claude` and `codex`** report usage percentages. agent-swap switches at the `threshold`, before the limit. It switches back at the window reset plus `switch_back_delay`. When codex uses paid credits, its percentages do not block it. Only `rate_limit_reached_type` or a limit error blocks it.
- **Other kinds** report no usage. agent-swap switches after a limit error. A limit error is text that contains `rate limit`, `429`, `quota`, `usage limit`, `budget exceeded`, `resource_exhausted`, `insufficient_balance` or `402`. If the error gives no reset time, the harness stays blocked for `cooldown`.
- **If all harnesses are blocked**, agent-swap runs the harness that becomes free first.

### How the conversation carries over

agent-swap reads every completed turn of the running harness into an in-memory log. It keeps one **mirror** per kind. A mirror is a native session that agent-swap writes the log into. At a switch, the next harness resumes its mirror, so it opens its own session with the whole conversation in it. agent-swap writes mirrors for `claude`, `codex`, `pi` and `opencode`. `agy` has no mirror. It gets a context file. [Mirrors](docs/ARCHITECTURE.md#mirrors) gives the details. `context_max_bytes` limits the text in one mirror or context file.

### Known limits and unverified items

- **Ping-pong.** A harness with no reset time is blocked only for `cooldown`. If it is still limited after that, agent-swap switches to it and away again once per `cooldown`.
- **`OPENCODE_CONFIG`.** agent-swap sets it for opencode. It replaces any value that you set.
- **Quit command for `codex`** is not tested. If the harness does not exit, agent-swap escalates. See [Decide](docs/ARCHITECTURE.md#decide-per-session).
- **Rewinds.** A claude rewind, a pi `/tree` branch switch or an opencode `/undo` removes turns from the running session. agent-swap detects this and writes a line to the log. It does not remove those turns from the other mirrors.
- **`agy` is lossy.** Antigravity stores steps as protobuf in SQLite. agent-swap extracts only the readable text, as one assistant turn that starts with `[lossy agy transcript]`. agy receives the context file, not a mirror.
- **Mirrors keep text.** A new mirror holds every turn as text. Tool calls become user-role text in an `agent-swap-tool` tag. A mirror that agent-swap only appends to keeps the native turns of its own kind.
- **Composer draft.** An automatic switch clears an unsent draft in the composer. agent-swap types `C-u` before the quit command.
- **Esc interrupt.** claude fires no `Stop` on an Esc interrupt, so a switch waits for the next completed turn.
- **pi model warning.** When pi resumes a session that agent-swap created, it prints "Warning: Could not restore model agent-swap/import. Using <default>". The warning has no effect. pi uses its configured default model, or the model in the harness `args`.
- **Codex `/new`.** During a run, agent-swap does not follow a new codex session that `/new` or a codex backtrack starts. It keeps reading the session that it launched, so codex turns after `/new` do not enter the conversation. agent-swap cannot tell `/new` apart from another codex in the same repo, because it does not link a rollout to the child process.
- **Rejected session change.** A session change is `/clear`, `/resume` or `/new`. After agent-swap rejects a session change, sync from that harness pauses until the harness starts again.
- **Background tasks.** A background shell command or subagent stops when the harness quits at a switch. The next harness does not see it. Start it again.
- **`agy` quota text** is not tested. agent-swap looks for `Quota exhausted` and the limit-error words in the agy log. The real message can differ.

## How a switch works

1. agent-swap records usage and idle state from the hooks, the plugins or the session files of the harness. After each completed turn it reads the new turns into the log.
2. When the current harness reaches the `threshold` or hits a limit, the wanted harness changes.
3. At the first idle point, agent-swap types the quit command of the harness. A turn is never cut off. The 95% default leaves room for the turn in progress.
4. agent-swap reads the last turns, then writes the turns that the next harness lacks into its mirror. It appends to a mirror that it owns. Otherwise it creates a new session.
5. agent-swap starts the next harness on its mirror. The harness gets no prompt, so a pending question stays a question. After a limit error cut off a turn, the first prompt tells it to resume.
6. If agent-swap cannot write a mirror, or the harness is `agy`, it writes `context-<n>.md` and starts the harness with `resume_message`.

All wrapped sessions share `account.json` and switch together. [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md#decision-logic) gives the exact rules.

## Harness swap

Switch now, without waiting for a limit:

```sh
agent-swap use codex      # run codex now, and keep it until it hits a limit
agent-swap use auto       # return to automatic order
agent-swap sync [harness] # write the conversation into harness's session (default: all)
```

`use` pins the harness. The switch happens at the next idle point. The pin ends when the pinned harness gets a new limit block. `sync` brings the mirror of a harness that is not running up to date. Then you can open that session outside agent-swap, for example with `codex resume <id>`. `status` lists the native IDs.

`use` and `sync` find the run from `--session DIR`, then `$AGENT_SWAP_SESSION_DIR`, then the one agent-swap pane in the current tmux window. Inside a harness, `use` needs `--force`.

## Commands

```sh
agent-swap [flags] [-- claude args]   # launch in tmux (the normal entry point)
agent-swap status [--config PATH] [--session DIR]   # usage, desired and wanted harness, pin, mirrors
agent-swap use <harness|auto> [--session DIR] [--force]
agent-swap sync [harness] [--session DIR]
agent-swap --version
agent-swap --help
```

Claude Code and tmux call these internal subcommands:

```sh
agent-swap run [flags] [-- claude args]   # the pane supervisor
agent-swap statusline                     # statusLine command
agent-swap hook <Event>                   # SessionStart | UserPromptSubmit | Stop | StopFailure | LimitHit
agent-swap key <target>                   # apiKeyHelper
```

`status` lists the pin and the wanted harness after the pin. For each mirror, it lists the native ID and the number of turns that the mirror is behind. Every command, including the launcher and `run`, reads the config from `--config`. If you omit the flag, it uses `$AGENT_SWAP_CONFIG`, then the default path.

## Files and state

| Path | Contents |
|---|---|
| `~/.config/agent-swap/config.toml` | your configuration |
| `~/.local/state/agent-swap/account.json` | per harness: usage windows, `blocked_until`, `resets_at` (shared and locked) |
| `~/.local/state/agent-swap/sessions/<id>/session.json` | native session ID of the running harness, idle/busy state, rate-limit flag |
| `~/.local/state/agent-swap/sessions/<id>/request.json` | the pin and sync requests from `use` and `sync` |
| `~/.local/state/agent-swap/sessions/<id>/mirrors.json` | per kind: native session ID, file path and turns behind (for `status`) |
| `~/.local/state/agent-swap/sessions/<id>/context-<n>.md` | the context file for a harness without a mirror |
| the session stores of the harnesses, for example `~/.claude/projects/` | the mirror sessions that agent-swap writes. They stay after agent-swap exits. |
| `~/.local/state/agent-swap/sessions/<id>/agent-swap-pi.ts`, `opencode.json`, `agent-swap-opencode.js` | the generated pi extension and opencode plugin |
| `~/.local/state/agent-swap/sessions/<id>/log` | the supervisor's log: triggers, switches, mirror IDs |
| `~/.local/state/agent-swap/sessions/<id>/<name>.settings.json` | the generated claude `--settings` files (no secrets) |

## Caveats

- **Every harness must already trust the project directory.** Claude Code and Codex each show a folder-trust dialog in a new directory. An unattended switch stops at that dialog. Start each harness in the directory once and accept the dialog before you use agent-swap there.
- **Background work stops at a switch.** `/exit` stops Claude's background shell commands and subagents. After the switch, start them again.
- **claude.ai connectors are off on API billing.** Claude Code turns off claude.ai MCP connectors whenever a credential other than your claude.ai login is in use.
- **Built-in auto-continue does not run in wrapped sessions.** agent-swap switches billing instead of waiting.
- **Usage is unknown until the first response.** `rate_limits` appears only after the first API response of a subscription session. On API billing it is absent. For this reason, agent-swap stores the reset time when the harness is blocked.
- **`--threshold` and `/usage` can differ.** The threshold uses the percentage that Claude Code reports. That value can differ from the claude.ai usage page.
- **The tmux status bar is per session.** agent-swap sets `status-right` for the tmux session it runs in.

## Troubleshooting

| Symptom | What to check |
|---|---|
| The status bar shows `SUB · usage unknown` | Send one prompt. `rate_limits` appears after the first response, and only on Pro, Max or Ultra plans. |
| Nothing happens at the threshold | Check `agent-swap status` and the session `log`. Verify the `poll` value. A long interval delays the switch. |
| The API session fails to authenticate | Run `agent-swap key <target>` yourself. It must print exactly one key. |
| The API session returns 404 or 405 | `base_url` must be the prefix before `/v1/messages`. For example, a gateway may serve it at `/anthropic/v1/messages`. |
| A switch does not happen | A switch waits for an idle point. Check `agent-swap status` and the session `log`. `agy` and `codex` report idle through polling, so allow one `poll`. |
| `agent-swap run must be started inside tmux` | Start it with `agent-swap`, not `agent-swap run`. |

## FAQ

**Is this against Anthropic's terms?** agent-swap only operates the official Claude Code CLI through its documented features: `--settings`, `--resume`, hooks, `apiKeyHelper` and the statusline. Your subscription and your API key each have their own terms. You must verify that your use obeys them.

**Why not use usage credits?** If your Claude account can pay for the overflow, enable usage credits and do not use this tool. Usage credits need no restart.

**Does the conversation continue?** Yes. Each harness resumes its own session, and that session holds the full conversation. Each provider has its own prompt cache. Thus the first request after a switch writes the full cache again.

**Can I force a switch?** Yes. Use `agent-swap use <harness>`. See [Harness swap](#harness-swap). To block a harness by hand, edit `harnesses.<name>.blocked_until` in `account.json`. Set it to a future Unix time to move away from that harness. Set it to `0` to allow it again. A forced return stays only while the harness's usage is below the threshold. If usage is at or above the threshold, the next poll blocks it again.

## Documentation

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md): the design, the state machine, and the Claude Code behaviors it depends on
- [CONTRIBUTING.md](CONTRIBUTING.md): development setup, tests, and pull requests
- [SECURITY.md](SECURITY.md): how keys are handled and how to report vulnerabilities
- [CHANGELOG.md](CHANGELOG.md)

## License

Copyright 2026 John Stillwagen. The license is the [Hippocratic License 3.0](LICENSE.md) (HL3-FULL), an ethical source license. See <https://firstdonoharm.dev>.

agent-swap is not affiliated with or endorsed by Anthropic. "Claude" and "Claude Code" are trademarks of Anthropic, PBC.
