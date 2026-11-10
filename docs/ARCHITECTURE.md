# Architecture

agent-swap is one Go binary with five roles. Each role has a different caller: the user, tmux or Claude Code.

| Role | Started by | Code |
|---|---|---|
| Launcher: `agent-swap [flags]` | the user | `internal/swap/launch.go` |
| Supervisor: `agent-swap run` | tmux, as the pane process | `internal/swap/runner.go` |
| Statusline: `agent-swap statusline` | Claude Code, on each statusline refresh | `internal/swap/helpers.go` |
| Hook: `agent-swap hook <Event>` | Claude Code hooks | `internal/swap/helpers.go` |
| Key helper: `agent-swap key <target>` | Claude Code `apiKeyHelper` | `internal/swap/helpers.go` |

The decision logic is in `internal/swap/logic.go`: `Peak`, `Trigger`, `Decide` and `StatusText`. This file has no I/O. Its tests need no tmux and no Claude Code.

## Design principles

1. **No screen scraping.** Every signal comes from documented Claude Code interfaces: statusline JSON and hooks. agent-swap uses tmux only to type input (`send-keys`) and to set `status-right`.
2. **Never cut a turn.** A switch always waits for an idle point, reported by the `Stop` or `StopFailure` hook.
3. **Secrets stay out of files and the environment.** Claude Code gets the API key on demand through `apiKeyHelper`.
4. **Account-wide state, per-session action.** All sessions share the usage. Thus all sessions share the decision about API billing. Each session does its own handoff.
5. **No daemon.** Each supervisor polls the shared state file. Each writer holds a `flock` and writes a temporary file, then renames it.

## Process model

```
tmux pane
└─ agent-swap run ..................... supervisor, lives as long as the pane
   └─ claude [--resume <id>] [user args] --settings <mode>.settings.json
      ├─ statusLine  → agent-swap statusline   (records usage, then renders the user's statusline)
      ├─ hooks       → agent-swap hook <Event> (records idle/busy/rate-limited)
      └─ apiKeyHelper→ agent-swap key <target> (API mode only)
```

When the pane process exits, the pane closes. Thus the supervisor is the pane process, and `claude` is its child process. The two share the TTY of the pane. When the child exits:

- **If a switch was pending**, the supervisor relaunches the child in the new mode with `--resume`.
- **Otherwise**, the user quit. The supervisor exits with Claude's exit code, and the pane closes as usual.

The supervisor ignores `SIGINT`, so Ctrl-C goes to Claude. The supervisor stops on `SIGTERM` or `SIGHUP`.

## Generated settings

At startup the supervisor writes two files into its session directory. It gives one of them to `claude --settings`. This option comes last in the argument list. Thus a user `--settings` cannot override the hooks or `apiKeyHelper`:

- `sub.settings.json`: `statusLine` and four hooks (`SessionStart`, `UserPromptSubmit`, `Stop`, `StopFailure`).
- `api.settings.json`: the same, plus `apiKeyHelper` and `env.ANTHROPIC_BASE_URL`.

Claude Code **merges** `--settings` hooks with the hooks of the user and the project. A live test showed that both sets fire. The `statusLine` setting replaces the user's setting. But `agent-swap statusline` executes the user's command, from `AGENT_SWAP_USER_STATUSLINE`. Thus the visible statusline does not change.

The child process gets the environment of the supervisor, minus the endpoint and credential variables that [SECURITY.md](../SECURITY.md) lists. It keeps `CLAUDE_CODE_OAUTH_TOKEN`, because that can be the subscription login. These inherited values could override the mode that `--settings` selects. The child also gets these variables:

| Variable | Used by |
|---|---|
| `AGENT_SWAP_SESSION_DIR` | hooks and the statusline: where `session.json` is |
| `AGENT_SWAP_STATE_DIR` | the statusline: where `account.json` is |
| `AGENT_SWAP_CONFIG` | `key`: which config file to read |
| `AGENT_SWAP_USER_STATUSLINE` | the statusline: the command to render |

## State

`account.json` is shared by all sessions:

```json
{
  "five_hour": {"used_percentage": 96, "resets_at": 1794607200},
  "seven_day": {"used_percentage": 32, "resets_at": 1795000000},
  "updated_at": 1794600000,
  "api_until": 1794607800,
  "resets_at": 1794607200
}
```

- `five_hour` and `seven_day` come from `rate_limits` in the statusline input. agent-swap ignores a `null` value, because `rate_limits` is absent on API billing.
- agent-swap merges each window separately. A window in the input replaces the stored window only if it is not older or lower. Thus an old render cannot replace newer data. If the input has no data for a window, agent-swap keeps the stored window until its `resets_at`. After that time, it clears the window.
- `api_until` is the end of the current API period. The desired mode is `API` while `now < api_until`.

`sessions/<id>/session.json` is per supervisor:

```json
{"claude_session_id": "bf8d…", "idle": true, "rate_limited": false, "started_at": 1794600000}
```

| Hook | Effect |
|---|---|
| `SessionStart` | idle, not rate-limited, `started_at = now` |
| `UserPromptSubmit` | busy, not rate-limited |
| `Stop` | idle, not rate-limited |
| `StopFailure` | idle, and rate-limited if `error == "rate_limit"` (`Stop` does **not** fire on API errors) |

## Decision logic

### Trigger (account-wide)

`Trigger` operates only while the desired mode is `SUB`. It uses the first rule that matches:

1. **Any window that has not reset reaches the threshold.** Set `resets_at` to the latest reset among those windows.
2. **Otherwise, a session hit a rate-limit error.** Set `resets_at` to the reset of the fullest window that has not reset. If no such window exists, use now + 5 h. `UserPromptSubmit` clears the flag, so an old rate limit cannot start a new trigger.
3. **Otherwise**, do nothing.

When it triggers, it sets `api_until = resets_at + switch_back_delay`.

`Trigger` ignores a window after its `resets_at`. Thus old data cannot start a new trigger after a switch back.

### Decide (per session)

```
          desired ≠ current                           ready ∧ idle
  ┌──────┐ ─────────────────▶ ┌─────────────────┐ ─────────────▶ ┌─────────┐
  │ Noop │                     │ Handoff pending │                 │ /exit → │
  └──────┘ ◀───────────────── └─────────────────┘                 │ relaunch│
                                                                    └─────────┘
                  desired ≠ handoff target
  Noop ◀──────────────────────────────────── Handoff pending   (Cancel)
```

- **No handoff pending:**
  - If the desired mode equals the current mode, do nothing.
  - If the desired mode is `API` and the session is rate-limited and idle, switch immediately. The session has no work to stop. A message would only cause one more failed turn.
  - Otherwise, start a handoff: type the message, which names a one-time ready file.
- **Handoff pending, but its target is no longer the desired mode:** cancel. The supervisor deletes the ready file and clears the handoff. This occurs also while the session is busy.
- **Handoff pending.** Nothing happens while the session is busy. Once it is idle, it is ready if any of these hold:
  - **Toward API:** the ready file exists, the session is rate-limited, or peak usage is ≥ 100%. This wait has no timeout, because the subscription operates until the limit.
  - **Toward SUB:** the ready file exists, or `switch_back_timeout` has passed since the handoff started.

When ready, the supervisor reads the Claude session ID. If the ID is empty, it does not switch and tries again at the next poll. Otherwise it types `/exit`. If the child does not exit in 30 s, the supervisor types `/exit` again. After the child exits, the supervisor reads the desired mode again and starts the new child in that mode. The relaunch gets only the persistent user arguments. It drops the prompt and the session options (`--continue`, `--resume`, `--session-id`, `--fork-session`, `--print`, `--from-pr`, `--teleport`, `--cloud`, `--tmux`, `--bg`/`--background`, `--desktop`). It keeps `-w`/`--worktree`.. It waits up to 30 s for `SessionStart`, then 2 s more. Then it types the resume message.

## Verified Claude Code behaviors

Live tmux tests on Claude Code 2.1.322 showed these behaviors. If a new Claude Code version changes one of them, agent-swap needs a change too.

| Behavior | Why it matters |
|---|---|
| The statusline input has `rate_limits.{five_hour,seven_day}.{used_percentage,resets_at}` and `session_id`. On API billing `rate_limits` is `null`. | The trigger and the reset time come from it. |
| `apiKeyHelper` with `env.ANTHROPIC_BASE_URL` in `--settings` gives "API Usage Billing" with **no** approval prompt. | Unattended relaunch. A direct `ANTHROPIC_API_KEY` can cause a prompt. |
| `--resume <id>` keeps the session ID and the full transcript across a change of credential and endpoint. | Continuity. |
| `--settings` hooks merge with user hooks. | The user's setup is untouched. |
| A 429 response fires `StopFailure` with `error: "rate_limit"`, and `Stop` does not fire. | Detecting rate limits and idle state. |
| `/exit` exits with code 0 and prints `claude --resume <id>`. | The relaunch path. |
| A directory not yet trusted shows a trust dialog whose default is "No, exit". | agent-swap assumes the directory is already trusted. |

## Testing

- `logic_test.go`: thresholds, windows that have reset, the choice of reset time, the readiness matrix, the switch-back timeout, and status text.
- `helpers_test.go`: statusline recording and pass-through, hook state changes, the settings JSON, key output checks, the config overlay, and session naming.
- `runner_test.go`: a full SUB → API → SUB cycle. Fake `tmux` and `claude` shell scripts replace the real programs, and all timings are below 1 s. The test verifies:
  - the launch arguments
  - that the supervisor sends no `/exit` before the ready file exists
  - the resume messages
  - that a window after its reset does not start a new trigger
  - that the supervisor returns the exit code of Claude
