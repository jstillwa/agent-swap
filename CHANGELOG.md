# Changelog

This file records all notable changes. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses [Semantic Versioning](https://semver.org/).

## [Unreleased]

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
- **API key handling.** Claude Code gets the key through `apiKeyHelper` and a `key_cmd`. claude-swap never stores or exports the key.
- **Status bar.** The tmux `status-right` shows the usage stages, a warning from 90%, the handoff state and API mode.
- **Notifications.** macOS notifications on each switch.
- **Configuration.** A TOML config with target profiles and message templates, plus CLI flag overrides.

[Unreleased]: https://github.com/jstillwa/claude-swap/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/jstillwa/claude-swap/releases/tag/v0.1.0
