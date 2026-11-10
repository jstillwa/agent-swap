# Changelog

This file records all notable changes. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- **Harnesses.** A config can list `[[harness]]` tables in preference order. Supported kinds: `claude`, `codex`, `pi`, `opencode` and `agy`. agent-swap runs the first harness that is not blocked.
- **Switch down and back.** agent-swap moves the conversation to the next harness at the threshold or at a limit error. It moves back when a preferred harness is free again.
- **Conversation carry-over.** At a switch, agent-swap writes handoff notes, transcript segments and a context file for the next harness. A switch back resumes the native session of that harness.
- **Signals per kind.** `pi` gets an extension and `opencode` gets a plugin. Both call `agent-swap hook`. The generated opencode config allows `permission.external_directory` for the session directory and the handoff directory only. agent-swap polls the session files of `codex` and `agy`.
- **`LimitHit` hook event.** A harness reports a limit while it retries.
- **CLI checks.** `agent-swap --config X status` stops with exit code 2 and asks for the subcommand first. `--target` with `[[harness]]` tables prints a warning.
- **Config keys.** `cooldown` (default 1 h) blocks a harness after a limit error with no known reset. `context_max_bytes` (default 60000) limits the context file.
- **`status`** shows usage and `blocked_until` per harness, and the desired harness.

### Fixed

- **Stale handoff.** A pending handoff is cancelled when the desired mode changes. The supervisor deletes the ready file.
- **Relaunch mode.** After the child exits, the supervisor reads the desired mode again and relaunches claude in that mode.
- **Replayed arguments.** A relaunch drops the prompt and the session options (`--continue`, `--resume`, `--session-id`, `--fork-session`, `--print`, `--from-pr`, `--teleport`, `--cloud`, `--tmux`, `--bg`/`--background`, `--desktop`). It keeps `-w`/`--worktree`.
- **State errors.** The supervisor stops at startup if it cannot read `account.json`. A failed read or write during a poll skips that poll.
- **Missing session ID.** The supervisor does not send `/exit` until it knows the Claude session ID. It resumes the ID it read before `/exit`.
- **Config validation.** The launcher and `run` check the config before tmux or claude starts. A bad value gives exit code 2 and names the key.
- **Inherited environment.** The child claude process does not get these variables from the shell: `ANTHROPIC_BASE_URL`, `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_CUSTOM_HEADERS`, `CLAUDE_CODE_USE_BEDROCK`, `CLAUDE_CODE_USE_VERTEX`, `CLAUDE_CODE_USE_FOUNDRY`, `ANTHROPIC_BEDROCK_BASE_URL`, `ANTHROPIC_VERTEX_BASE_URL`, `ANTHROPIC_FOUNDRY_BASE_URL`, `ANTHROPIC_FOUNDRY_API_KEY` and `ANTHROPIC_FOUNDRY_RESOURCE`. It keeps `CLAUDE_CODE_OAUTH_TOKEN`.
- **Settings order.** agent-swap puts its `--settings` after the user arguments. A user `--settings` cannot override the hooks or `apiKeyHelper`.
- **Statusline merge.** A statusline update with one window keeps the other stored window until its reset. An older or lower value does not replace newer data.
- **Rate-limited flag.** `UserPromptSubmit` clears it. Thus an old rate limit cannot start a new API period.
- **`status` config.** `agent-swap status` accepts `--config PATH` and reads `$AGENT_SWAP_CONFIG`. The flag has priority.
- **Ready-file path.** The `{file}` placeholder is shell-quoted when the path has special characters.
- **README.** The FAQ says that a forced return to the subscription holds only while usage is below the threshold.

### Changed

- **State format.** `account.json` holds one entry per harness. A file from 0.1 decodes to empty state.
- **Messages.** `resume_message` names the context file. New placeholders: `{from}`, `{to}`, `{notes}` and `{context}`. `return_resume_message` is removed.
- **Settings files.** The claude settings file is `<name>.settings.json` (was `sub` and `api`).
- **Project name.** The project is now agent-swap (was claude-swap). The binary, module path, config and state directories, and the `AGENT_SWAP_*` environment variables use the new name. The tmux session prefix `cs-` becomes `as-`.

### Planned

- Linux desktop notifications (`notify-send`).
- CI workflow.

## [0.1.0] - 2026-11-10

### Added

- **tmux launcher.** It creates the session `cs-<dirname>`, or a new window when already inside tmux.
- **Pane supervisor.** It runs `claude` as a child process and relaunches it with `--resume` when it switches between subscription and API billing.
- **Threshold trigger.** It fires when any rate-limit window (5-hour or 7-day) reaches the threshold (default 95%), from the statusline's `rate_limits`.
- **Handoff using a one-time ready file:**
  - Toward API: no timeout. A rate-limit error (`StopFailure`) or 100% usage also counts as ready.
  - Toward subscription: the wait stops after `switch_back_timeout` (default 1 h). The switch then occurs at the next idle point.
- **Switch back** at the window reset plus `switch_back_delay` (default 10 min).
- **Shared account state** in `account.json`, with flock and atomic writes. All wrapped sessions switch together.
- **API key handling.** Claude Code gets the key through `apiKeyHelper` and a `key_cmd`. agent-swap never stores or exports the key.
- **Status bar.** The tmux `status-right` shows the usage stages, a warning from 90%, the handoff state and API mode.
- **Notifications.** macOS notifications on each switch.
- **Configuration.** A TOML config with target profiles and message templates, plus CLI flag overrides.

[Unreleased]: https://github.com/jstillwa/agent-swap/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/jstillwa/agent-swap/releases/tag/v0.1.0
