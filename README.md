# <img src="docs/icon.svg" alt="" width="40" align="top"> agent-swap

[![License: Hippocratic 3.0](https://img.shields.io/static/v1?label=Hippocratic%20License&message=HL3-FULL&labelColor=5e2751&color=bc8c3d)](https://firstdonoharm.dev/version/3/0/full.html)
![Go](https://img.shields.io/badge/go-1.27%2B-00ADD8)
![Platform](https://img.shields.io/badge/platform-macOS-lightgrey)

agent-swap runs coding agents inside tmux. When one agent reaches a usage limit, agent-swap moves the same conversation to the next agent. It supports [Claude Code](https://code.claude.com), [Codex CLI](https://github.com/openai/codex), pi, opencode and Antigravity CLI.

You list the agents in order of preference. Each entry in that list is a **harness**. When the running agent nears or hits its limit, agent-swap does the following:

1. It waits until the agent is idle, so it does not interrupt a turn.
2. It starts the next agent on the **same conversation**. The next agent resumes its own native session, which holds the whole conversation.
3. After the limit resets, it moves the conversation back to the preferred agent.

You can also switch by hand with `agent-swap use <harness>`.

With no harness list, agent-swap uses only Claude Code. It moves the conversation from your Claude subscription to API billing, and back after the reset. The API endpoint can be any Anthropic-compatible endpoint, for example an LLM gateway or your own Console key.

The tmux status bar shows the state of the running harness:

```
 claude 72% (5h) · resets Mon 14:20             ← normal
 claude 91% (5h): switch at 95%, 4% left        ← within 5% of the threshold
 claude → codex: switching at the next idle     ← a switch is waiting
 codex · back to claude at Mon 14:30            ← claude is blocked until its reset
```

## Contents

- [Why](#why)
- [Requirements](#requirements)
- [Install](#install)
- [Quick start](#quick-start)
- [Harnesses](#harnesses)
- [Configuration](#configuration)
- [How a switch works](#how-a-switch-works)
- [Switch by hand](#switch-by-hand)
- [Commands](#commands)
- [Files and state](#files-and-state)
- [Known limits](#known-limits)
- [Caveats](#caveats)
- [Troubleshooting](#troubleshooting)
- [FAQ](#faq)
- [Documentation](#documentation)
- [License](#license)

## Why

agent-swap helps when an agent stops at a usage limit. Claude Code on a Pro, Max or Ultra plan and Codex CLI on a ChatGPT plan have a short usage window and a weekly one. Other agents have quotas or a budget. When a limit is full, the agent stops until it resets.

Use agent-swap in these cases:

- **You use several agents.** When one agent reaches its limit, you continue the same conversation in the next agent. You do not paste context by hand.
- **You want the overflow on a different bill.** List a harness that bills a pay-per-use API after your subscription harnesses. The API can be a company LLM gateway or your own API key. When the subscription agent stops, the conversation continues there. After the reset, it moves back.

These agents can bill an API:

- **pi and opencode.** Choose the provider and the model in the harness `args`, for example `["--provider", "deepseek", "--model", "deepseek-chat"]` for pi or `["-m", "<provider>/<model>"]` for opencode. Both passed a live switch on an API gateway.
- **Claude Code.** Add a `target`. agent-swap then moves the same Claude Code conversation from your subscription to API billing. See [Targets](#targets-claude-api-billing).
- **Codex CLI.** A second codex harness can select an API provider with a profile, for example `args = ["-p", "api"]`. The profile file `$CODEX_HOME/api.config.toml` defines a provider that reads its key from an environment variable. Set that variable in your shell. This setup is not tested.
- **Antigravity CLI.** agent-swap has no tested way to select API billing for agy.

Some agents have their own options for a full window:

- **Credits.** Claude Code usage credits and Codex CLI credits let you keep working past the limit. The **same account** pays for the overflow.
- **Auto-continue.** A Claude Code session can wait and continue after the reset. This works only if the reset is less than 24 hours away.

agent-swap moves the conversation to a different bill or a different agent. Your context stays the same.

## Requirements

You always need these:

| Tool | Version tested | Notes |
|---|---|---|
| tmux | 3.8 | |
| macOS | 14 | Notifications use `osascript`. Other Unix systems are not tested, and they get no notifications. |
| Go | 1.27 | Only to build from source. The [releases](https://github.com/jstillwa/agent-swap/releases) have binaries. |

You also need at least one supported agent. Install only the agents that you list in the config. Log in to each agent before you use agent-swap, because agent-swap does not log in for you.

| Agent | Kind | Version tested | Notes |
|---|---|---|---|
| Claude Code | `claude` | 2.1.322 | Usage percentages need a Pro, Max or Ultra plan. |
| Codex CLI | `codex` | 0.166 | |
| pi | `pi` | 1.5.1 | Needs Node.js on `PATH`. |
| opencode | `opencode` | 1.19.5 | |
| Antigravity CLI | `agy` | 1.5.5 | Needs `sqlite3` on `PATH`. Gets the conversation as a context file, not a native session. |

The **kind** is the value of `kind` in a harness entry.

Claude Code is optional. You need it only for the default setup, which has no harness list, and for a `claude` harness. The default setup also needs an Anthropic-compatible API endpoint and a command that prints its key. A `claude` harness with a `target` needs the same. See [Targets](#targets-claude-api-billing).

## Install

Download the archive for your Mac from the [releases](https://github.com/jstillwa/agent-swap/releases): `darwin_arm64` for Apple silicon, `darwin_amd64` for Intel. The binaries are not signed. After you extract the archive, remove the quarantine flag. Then move the binary to a directory on your `PATH`:

```sh
xattr -d com.apple.quarantine ./agent-swap
mkdir -p ~/.local/bin && mv ./agent-swap ~/.local/bin/
```

Or build from source with Go:

```sh
go install github.com/jstillwa/agent-swap/cmd/agent-swap@latest
```

`go install` puts the binary in `$(go env GOPATH)/bin`, usually `~/go/bin`. Make sure that this directory is on your `PATH`:

```sh
echo 'export PATH="$HOME/go/bin:$PATH"' >> ~/.zshrc
```

## Quick start

Before the first run, start each agent once in your project directory. Log in, and accept any folder-trust dialog. An unattended switch stops at that dialog.

### Several agents: codex, then pi

1. Create `~/.config/agent-swap/config.toml` with two harnesses. You need no target:

   ```toml
   [[harness]]
   name = "codex"
   kind = "codex"

   [[harness]]
   name = "pi"
   kind = "pi"
   args = ["--provider", "deepseek", "--model", "deepseek-chat"]   # billed by the DeepSeek API
   ```

2. Run `agent-swap` in your project directory. It starts the first harness in the list, here codex on your ChatGPT plan. When codex reaches a limit, agent-swap moves the conversation to pi on the DeepSeek API.

### Claude Code with API overflow

1. Create `~/.config/agent-swap/config.toml` with one target:

   ```toml
   [targets.gateway]
   base_url = "https://llm-gateway.example.com/anthropic"
   key_cmd  = "security find-generic-password -s llm-gateway -w"
   ```

2. Run `agent-swap` in your project directory. It works like `claude`, inside a managed tmux session. Arguments after `--` go to claude, for example `agent-swap -- --model opus`.

### After the start

- **Outside tmux**, agent-swap creates and attaches the session `as-<dirname>`.
- **Inside tmux**, it opens a new window named `agent-swap`.

Then use the agent as usual. Your own statusline, hooks, settings and plugins continue to operate.

To check the setup, run `agent-swap status` in another tmux pane. It lists the harnesses in order, their usage or blocked state, and the harness that runs. To test a switch without a limit, run `agent-swap use <harness>`. To return to the automatic order, run `agent-swap use auto`.

[examples/config.toml](examples/config.toml) has a full example with four harnesses. Delete the harnesses that you do not use.

## Harnesses

A harness is one entry in the config. It runs one agent in the tmux pane. List harnesses with `[[harness]]` tables. The order is the preference order:

- agent-swap runs the first harness that is not blocked.
- When that harness nears or hits its limit, agent-swap moves the conversation to the next harness.
- When a harness higher in the list is free again, agent-swap moves the conversation back.

Without `[[harness]]` tables, the order is `claude`, then `api`. The `api` harness is claude on the target from `default_target` or `--target`.

```toml
[[harness]]
name = "claude"
kind = "claude"

[[harness]]
name = "codex"
kind = "codex"
args = ["-m", "gpt-5.5"]
```

| Key | Default | Meaning |
|---|---|---|
| `name` | | Required and unique. State and the tmux status bar use it. |
| `kind` | | Required. One of `claude`, `codex`, `pi`, `opencode`, `agy`. |
| `bin` | `claude_bin` for `claude`, else the kind | The executable. |
| `args` | | Arguments that agent-swap always passes to this harness. |
| `target` | | `claude` only. Use API billing through this target. The target must exist and have a `key_cmd`. |
| `threshold`, `cooldown` | the top-level values | Override the top-level values for this harness. |
| `context_max_bytes` | the top-level value | Override the top-level cap for this harness. Use a small value for a model with a small context window. |

Arguments after `--` belong to the kind of the first harness in the order:

- The first launch gets all of them if its harness has that kind.
- A later launch gets them only if it is a `claude` harness of that kind. It then gets only the persistent options, such as `--model`.
- A later launch of another kind gets none of them.

A later `claude` launch does not get a prompt, `--continue`, `--resume`, `--session-id`, `--fork-session`, `--print`, `--from-pr`, `--teleport`, `--cloud`, `--tmux`, `--bg`/`--background` or `--desktop`. It keeps `-w`/`--worktree`. A resume inside a worktree is not tested.

### Supported kinds

`claude`, `codex`, `pi`, `opencode` and `agy`. All five passed a live switch. The [kinds table](docs/ARCHITECTURE.md#adapters) gives the tested version, the signal source and the transcript source of each kind.

### Limit detection

- **`claude` and `codex`** report usage percentages. agent-swap switches at the `threshold`, before the limit. It switches back at the window reset plus `switch_back_delay`. When Codex CLI uses paid credits, its percentages do not block it. Only `rate_limit_reached_type` or a limit error blocks it.
- **Other kinds** report no usage. agent-swap switches after a limit error. A limit error is text that contains `rate limit`, `rate_limit`, `quota`, `usage limit`, `budget exceeded`, `resource_exhausted` or `insufficient_balance`, or the number `429` or `402`. Case does not matter. These kinds report no reset time, so the harness stays blocked for `cooldown`.
- **A `claude` or `codex` harness with no live usage window** also stays blocked for `cooldown` after a limit error.
- **If all harnesses are blocked**, agent-swap runs the harness that becomes free first.

### Mirrors and context files

agent-swap reads every completed turn of the running harness into an in-memory log. It keeps one **mirror** per kind. A mirror is a native session that agent-swap writes the log into. At a switch, the next harness resumes its mirror, so it opens its own session with the whole conversation in it.

agent-swap writes mirrors for `claude`, `codex`, `pi` and `opencode`. `agy` has no mirror. It gets a **context file**, a Markdown file with the conversation. `context_max_bytes` limits the text in one mirror or context file. [Mirrors](docs/ARCHITECTURE.md#mirrors) gives the details.

## Configuration

Use the file `~/.config/agent-swap/config.toml`. Every key is optional. Command-line flags override the file. The file has two separate parts:

- **Harnesses** (`[[harness]]` tables) choose which agents run, and in what order. See [Harnesses](#harnesses).
- **Targets** (`[targets.<name>]` tables) set up Claude Code API billing. See [Targets](#targets-claude-api-billing).

agent-swap checks the config before it starts tmux or an agent. A bad value stops it with exit code 2, and the error names the key. These rules apply:

- `threshold` is more than 0 and not more than 100.
- `switch_back_delay` and `cooldown` are 0 or more. `poll` and `context_max_bytes` are more than 0.
- `claude_bin`, `tmux_bin` and `state_dir` are not empty.
- Every harness has a unique `name` and a supported `kind`. Its `cooldown` and `context_max_bytes` are 0 or more.
- A harness `target` is set only on a `claude` harness. It names a defined target, and that target has a `key_cmd`.
- Without `[[harness]]` tables, a target must resolve. See [Targets](#targets-claude-api-billing).

```toml
threshold = 95              # switch when any usage window reaches this %
switch_back_delay = "10m"   # how long after the window's reset time to switch back
poll = "30s"                # how often agent-swap reads usage
notify = true               # macOS notification on every switch
default_target = "gateway"

[targets.gateway]           # an Anthropic-compatible gateway
base_url = "https://llm-gateway.example.com/anthropic"
key_cmd  = "security find-generic-password -s llm-gateway -w"

[targets.console]           # api.anthropic.com with a Console key
key_cmd = "security find-generic-password -s anthropic-api -w"
```

### Targets (Claude API billing)

A target is a destination for Claude Code API billing:

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

agent-swap has no built-in target. Without `[[harness]]` tables, define at least one target. If you define more than one, set `default_target` or use `--target`. With `[[harness]]` tables, only a `claude` harness that has a `target` needs one. agent-swap then ignores `--target` and prints a warning.

### Resume message

`resume_message` is the first prompt for a harness that gets a context file instead of a mirror. Placeholders: `{from}`, `{to}` and `{context}`. The default:

> agent-swap moved this conversation from {from} to {to}. Read {context}: it has the conversation so far. Then continue where it left off; if it ended waiting for the user, ask again.

agent-swap no longer reads `handoff_message`, `return_message` and `switch_back_timeout`. It prints a warning if the config sets one of them.

### Other keys

| Key | Default | Meaning |
|---|---|---|
| `cooldown` | `1h` | How long a harness stays blocked after a limit error with no known reset. A `[[harness]]` can set its own value. |
| `context_max_bytes` | `200000` | Cap on the conversation text in one mirror or context file. agent-swap keeps the newest turns. A `[[harness]]` can set its own value. |
| `notify` | `true` | macOS notification on each switch, and when a pin ends. |
| `statusline_cmd` | `statusLine.command` from `~/.claude/settings.json` | The Claude Code statusline that agent-swap displays. |
| `state_dir` | `~/.local/state/agent-swap` | Shared and per-session state, as an absolute path. |
| `claude_bin`, `tmux_bin` | `claude`, `tmux` | The executables to run. |

### Flags

```
--config PATH              config file (default $AGENT_SWAP_CONFIG, then ~/.config/agent-swap/config.toml)
--target NAME              target for API billing (default: default_target, or the only target)
--threshold PCT            switch threshold in percent
--switch-back-delay DUR    e.g. 15m
--poll DUR                 e.g. 10s
```

## How a switch works

1. agent-swap records usage and idle state from the hooks, the plugins or the session files of the harness. After each completed turn, it reads the new turns into the log.
2. When the current harness reaches the `threshold` or hits a limit, the wanted harness changes.
3. At the first idle point, agent-swap types the quit command of the harness. The 95% default leaves room for the turn in progress. If the harness does not exit, agent-swap types the command again, then sends Ctrl-C, then `SIGTERM` and `SIGKILL`. If the harness becomes busy before Ctrl-C, the switch waits for the next idle point. agent-swap drops the switch if the current harness is wanted again. See [Want and Decide](docs/ARCHITECTURE.md#want-and-decide-per-session).
4. agent-swap reads the last turns. Then it writes the turns that the next harness lacks into its mirror. It appends to a mirror that it owns. Otherwise it creates a new session.
5. agent-swap starts the next harness on its mirror. The harness gets no prompt, so a pending question stays a question. If a limit error cut off a turn, the first prompt tells the harness to resume.
6. If agent-swap cannot write a mirror, or the harness is `agy`, it writes `context-<n>.md` and starts the harness with `resume_message`.

All agent-swap sessions share `account.json` and switch together. [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md#decision-logic) gives the exact rules.

## Switch by hand

Switch now, without waiting for a limit:

```sh
agent-swap use codex      # run codex now, and keep it until it hits a limit
agent-swap use auto       # return to the automatic order
agent-swap sync [harness] # write the conversation into the harness's mirror (default: all)
```

`use` pins the harness. The switch happens at the next idle point. The pin ends when the pinned harness gets a new limit block. `sync` brings the mirror of a harness that is not running up to date. Then you can open that session outside agent-swap, for example with `codex resume <id>`. Run `agent-swap status` to get the native ID.

`use` and `sync` find the run from `--session DIR`, then `$AGENT_SWAP_SESSION_DIR`, then the one agent-swap pane in the current tmux window. Inside a harness, `use` needs `--force`.

## Commands

```sh
agent-swap [flags] [-- args]   # launch in tmux (the normal entry point)
agent-swap status [--config PATH] [--session DIR]   # usage, desired and wanted harness, pin, mirrors
agent-swap use <harness|auto> [--session DIR] [--force]
agent-swap sync [harness] [--session DIR]
agent-swap --version
agent-swap --help
```

`status` lists the pin and the wanted harness after the pin. For each mirror, it lists the native ID and the number of turns that the mirror is behind.

The launcher, `run`, `status`, `use` and `sync` read the config from `--config`. If you omit the flag, they use `$AGENT_SWAP_CONFIG`, then the default path.

Claude Code, the pi extension, the opencode plugin and tmux call these internal subcommands:

```sh
agent-swap run [flags] [-- args]   # the pane supervisor
agent-swap statusline              # Claude Code statusline command
agent-swap hook <Event>            # SessionStart | UserPromptSubmit | Stop | StopFailure | LimitHit
agent-swap key <target>            # Claude Code apiKeyHelper; reads $AGENT_SWAP_CONFIG
```

## Files and state

| Path | Contents |
|---|---|
| `~/.config/agent-swap/config.toml` | Your configuration. |
| `~/.local/state/agent-swap/account.json` | Per harness: usage windows, `blocked_until`, `resets_at`. Shared and locked. |
| `~/.local/state/agent-swap/sessions/<id>/session.json` | Native session ID of the running harness, idle or busy state, rate-limit flag. |
| `~/.local/state/agent-swap/sessions/<id>/request.json` | The pin and sync requests from `use` and `sync`. |
| `~/.local/state/agent-swap/sessions/<id>/mirrors.json` | Per kind: native session ID, file path and turns behind, for `status`. |
| `~/.local/state/agent-swap/sessions/<id>/context-<n>.md` | The context file for a harness without a mirror. |
| The session stores of the agents, for example `~/.claude/projects/` | The mirrors that agent-swap writes. They stay after agent-swap exits. |
| `~/.local/state/agent-swap/sessions/<id>/agent-swap-pi.ts`, `opencode.json`, `agent-swap-opencode.js` | The generated pi extension and opencode plugin. |
| `~/.local/state/agent-swap/sessions/<id>/agy.log` | The agy log. agent-swap reads the conversation ID and quota errors from it. |
| `~/.local/state/agent-swap/sessions/<id>/log` | The supervisor's log: triggers, switches, mirror IDs. |
| `~/.local/state/agent-swap/sessions/<id>/<name>.settings.json` | The generated Claude Code `--settings` files. They hold no secrets. |

## Known limits

- **Ping-pong.** A harness with no reset time is blocked only for `cooldown`. If it is still limited after that, agent-swap switches to it and away again once per `cooldown`.
- **`OPENCODE_CONFIG`.** agent-swap sets it for opencode. It replaces any value that you set.
- **Quit command for `codex`** is not tested. If the harness does not exit, agent-swap escalates. See [Want and Decide](docs/ARCHITECTURE.md#want-and-decide-per-session).
- **Rewinds.** A Claude Code rewind, a pi `/tree` branch switch or an opencode `/undo` removes turns from the running session. agent-swap detects this and writes a line to the log. It does not remove those turns from the other mirrors.
- **`agy` is lossy.** Antigravity CLI stores steps as protobuf in SQLite. agent-swap extracts only the readable text, as one assistant turn that starts with `[lossy agy transcript]`. agy receives the context file, not a mirror.
- **Mirrors keep text.** A new mirror holds every turn as text. Tool calls become user-role text in an `agent-swap-tool` tag. A mirror that agent-swap only appends to keeps the native turns of its own kind.
- **Composer draft.** An automatic switch clears an unsent draft in the composer. agent-swap types `C-u` before the quit command.
- **Esc interrupt.** Claude Code fires no `Stop` on an Esc interrupt, so a switch waits for the next completed turn.
- **pi model warning.** When pi resumes a session that agent-swap created, it prints "Warning: Could not restore model agent-swap/import. Using <default>". The warning has no effect. pi uses its configured default model, or the model in the harness `args`.
- **Codex `/new`.** During a run, agent-swap does not follow a new Codex CLI session that `/new` or a backtrack starts. It keeps reading the session that it launched, so codex turns after `/new` do not enter the conversation. agent-swap cannot tell `/new` apart from another Codex CLI in the same repo, because it does not link a rollout to the child process.
- **Rejected session change.** A session change is `/clear`, `/resume` or `/new`. agent-swap accepts the new session only if it belongs to the same project directory. After agent-swap rejects a session change, sync from that harness pauses until the harness starts again.
- **Background tasks.** A background shell command or subagent stops when the harness quits at a switch. The next harness does not see it. Start it again.
- **`agy` quota text** is not tested. agent-swap looks only for `quota exhausted` or `resource_exhausted`, in any case, in the agy log. The real message can differ.

## Caveats

- **Every agent must already trust the project directory.** Some agents, for example Claude Code and Codex CLI, show a folder-trust dialog in a new directory. An unattended switch stops at that dialog. Start each agent in the directory once and accept the dialog before you use agent-swap there.
- **Every agent must already be logged in.** agent-swap does not log in for you. A login prompt stops an unattended switch in the same way.
- **Usage is unknown until the first response.** Claude Code and Codex CLI report usage only after their first response. On API billing, Claude Code reports none. Other agents report no usage, so agent-swap reacts to their limit errors. When a harness is blocked, agent-swap stores the reset time.
- **The tmux status bar is per session.** agent-swap sets `status-right` for the tmux session that it runs in.
- **Agents other than Claude Code get your shell environment unchanged.** A provider key or endpoint variable in your shell, for example `OPENAI_API_KEY`, can change which account an agent bills.

### Claude Code

- **Your Anthropic environment variables do not reach Claude Code.** Before agent-swap starts claude, it removes `ANTHROPIC_API_KEY`, `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_CUSTOM_HEADERS` and the Bedrock, Vertex and Foundry variables. Only the selected target decides billing. `CLAUDE_CODE_OAUTH_TOKEN` stays. [SECURITY.md](SECURITY.md) has the full list.
- **claude.ai connectors are off on API billing.** Claude Code turns off claude.ai MCP connectors when you use a credential other than your claude.ai login.
- **Auto-continue does not run.** agent-swap switches at the threshold, before Claude Code's auto-continue starts.
- **`--threshold` and `/usage` can differ.** The threshold uses the percentage that Claude Code reports. That value can differ from the claude.ai usage page.

## Troubleshooting

| Symptom | What to check |
|---|---|
| The harness does not start, or the shell reports "command not found" | Install the agent and put it on `PATH`. If the executable has another name, set `bin` in the `[[harness]]` table. |
| agent-swap exits with code 2 | The config has a bad value. The error names the key. See [Configuration](#configuration). |
| A switch stops, and the new agent waits for input | The agent shows a login prompt or a folder-trust dialog. Attach to the pane and finish the dialog. Start each agent once in this directory before the next run. |
| The status bar shows `<harness> · usage unknown` | Send one prompt. See [Caveats](#caveats). Kinds other than `claude` and `codex` always show this. |
| Nothing happens at the threshold | Check `agent-swap status` and the session `log`. Verify the `poll` value. A long interval delays the switch. |
| A switch does not happen | A switch waits for an idle point. Check `agent-swap status` and the session `log`. `agy` and `codex` report idle through polling, so allow one `poll`. |
| The API session fails to authenticate | Run `agent-swap key <target>` yourself. It must print exactly one key. |
| The API session returns 404 or 405 | `base_url` must be the prefix before `/v1/messages`. For example, a gateway can serve it at `/anthropic/v1/messages`. |
| `agent-swap run must be started inside tmux` | Start it with `agent-swap`, not `agent-swap run`. |

## FAQ

**Is this against the terms of an agent or a provider?** agent-swap runs only the official CLI of each agent, through its documented features: resume options, hooks, plugins and extensions, and the session files that the agent writes. For Claude Code it also uses `--settings`, `apiKeyHelper` and the statusline. Each subscription and each API key has its own terms. You must verify that your use obeys them.

**Why not use the agent's own credits?** If the same account can pay for the overflow, for example with Claude Code usage credits or Codex CLI credits, use them for that. They need no restart. Use agent-swap when the overflow must go to a different bill or a different agent.

**Does the conversation continue?** Yes. Each agent resumes its own session, and that session holds the full conversation. Each provider has its own prompt cache. So the first request after a switch writes the full cache again.

**Can I force a switch?** Yes. Use `agent-swap use <harness>`. See [Switch by hand](#switch-by-hand). To block a harness by hand, edit `harnesses.<name>.blocked_until` in `account.json`. Set it to a future Unix time to move away from that harness. Set it to `0` to allow it again. A forced return holds only while the harness's stored usage is below the threshold. If a live window is still at or above it, agent-swap switches back to that harness. Its first poll there blocks the harness again, and agent-swap switches away again.

## Documentation

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md): the design, the state machine, and the agent behaviors that agent-swap depends on
- [CONTRIBUTING.md](CONTRIBUTING.md): development setup, tests, and pull requests
- [SECURITY.md](SECURITY.md): how agent-swap handles keys and conversations, and how to report vulnerabilities
- [CHANGELOG.md](CHANGELOG.md)

## License

Copyright 2026 John Stillwagen. The license is the [Hippocratic License 3.0](LICENSE.md) (HL3-FULL), an ethical source license. See <https://firstdonoharm.dev>.

agent-swap is an independent project. It is not affiliated with or endorsed by Anthropic, OpenAI, Google, or the authors of pi or opencode. Claude, Claude Code, Codex, Antigravity and the other product names are trademarks of their owners.
