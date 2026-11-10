# Architecture

agent-swap is one Go binary with five roles. Each role has a different caller: the user, tmux or a harness. A harness is one coding-agent CLI: `claude`, `codex`, `pi`, `opencode` or `agy`.

| Role | Started by | Code |
|---|---|---|
| Launcher: `agent-swap [flags]` | the user | `internal/swap/launch.go` |
| Supervisor: `agent-swap run` | tmux, as the pane process | `internal/swap/runner.go` |
| Statusline: `agent-swap statusline` | Claude Code, on each statusline refresh | `internal/swap/helpers.go` |
| Hook: `agent-swap hook <Event>` | Claude Code hooks, the pi extension, the opencode plugin | `internal/swap/helpers.go` |
| Key helper: `agent-swap key <target>` | Claude Code `apiKeyHelper` | `internal/swap/helpers.go` |

The decision logic is in `internal/swap/logic.go`: `Peak`, `Desired`, `Trigger`, `Decide` and `StatusText`. This file has no I/O. Its tests need no tmux and no Claude Code.

## Design principles

1. **No screen scraping.** Every signal comes from a harness interface: hooks, plugins, or the session files that the harness writes. agent-swap uses tmux only to type input (`send-keys`) and to set `status-right`.
2. **Never cut a turn.** A switch always waits for an idle point, reported by the `Stop` or `StopFailure` hook.
3. **Secrets stay out of files and the environment.** Claude Code gets the API key on demand through `apiKeyHelper`.
4. **Account-wide state, per-session action.** All sessions share the usage of each harness. Thus all sessions agree on the desired harness. Each session does its own handoff.
5. **No daemon.** Each supervisor polls the shared state file. Each writer holds a `flock` and writes a temporary file, then renames it.

## Adapters

Each kind has an adapter in `internal/swap/adapter_<kind>.go`. The runner uses only this interface (`adapter.go`):

```go
type Adapter interface {
	Command(h Harness, native, prompt string, extra []string, rt Runtime) (Launch, error)
	Quit() string
	Poll(h Harness, rt Runtime, s *Session, u *Usage) error
	Transcript(native string, rt Runtime) (string, error)
}
```

- `Command` builds the child process. `native` is the harness's own session ID to resume, or empty for a new session. `prompt` is the first message, or empty. `extra` holds the user arguments. `Command` also writes any generated file (claude settings, pi extension, opencode config and plugin) into the session directory.
- `Quit` is the text that the supervisor types to exit the harness.
- `Poll` updates the session and usage for kinds that give no hooks (`codex`, `agy`). It reads their session files. For the other kinds it does nothing.
- `Transcript` returns the native conversation as plain text, one block per turn: `[user] …`, `[assistant] …`, `[tool] name: input`.

| Kind | Tested | Resume | Signals | Transcript source |
|---|---|---|---|---|
| `claude` | 2.1.322 | `--resume <id>` | hooks and statusline through `--settings` | `~/.claude/projects/<slug>/<id>.jsonl` |
| `codex` | 0.166 | `codex resume <id>` | `Poll` on the rollout file in `$CODEX_HOME/sessions` | the same rollout file |
| `pi` | 1.5.1 | `--session <id>` | extension `agent-swap-pi.ts` through `-e` | `~/.pi/agent/sessions/*/*_<id>.jsonl` |
| `opencode` | 1.19.5 | `-s <id>` | plugin through `OPENCODE_CONFIG` | `opencode export <id>` |
| `agy` | 1.5.5 | `--conversation <id>` | `Poll` on its own log, `history.jsonl` and `conversation_summaries.db` | `conversations/<id>.db`, lossy |

All five kinds passed a live switch in the order claude → codex → pi → claude and claude → opencode → agy → pi → claude.

Notes per kind:

- **codex:** usage windows come from `rate_limits`. A session on paid credits is not limited. Only `rate_limit_reached_type` or a limit error counts.
- **opencode:** the plugin reports a retry as a limit only after 3 attempts. The generated config sets `permission.external_directory` to `allow` for the session directory and the handoff directory (and their symlink-resolved paths) only.
- **agy:** each session gets its own `--log-file` (`agy.log`) in the session directory. `Poll` reads the native ID from the last `Created conversation <id>` or `Streaming conversation <id>` line after the launch. If there is none, it uses `history.jsonl`. `Poll` also reads limit errors from that log. `conversation_summaries.db` gives only the idle status. The transcript keeps only prose runs: 8 or more characters with at least 2 spaces, each up to 500 bytes.

## Process model

```
tmux pane
└─ agent-swap run ..................... supervisor, lives as long as the pane
   └─ one harness at a time, for example:
      claude [--resume <id>] [args] --settings <name>.settings.json
      ├─ statusLine  → agent-swap statusline   (records usage, then renders the user's statusline)
      ├─ hooks       → agent-swap hook <Event> (records idle/busy/rate-limited)
      └─ apiKeyHelper→ agent-swap key <target> (claude with target only)
      codex [resume <id>] [args]               (no hooks: the supervisor polls the rollout file)
      pi -e agent-swap-pi.ts [--session <id>]  (extension → agent-swap hook <Event>)
```

When the pane process exits, the pane closes. Thus the supervisor is the pane process, and the harness is its child process. The two share the TTY of the pane. When the child exits:

- **If a switch was pending**, the supervisor starts the next harness. See [Context building](#context-building).
- **Otherwise**, the user quit. The supervisor exits with the harness's exit code, and the pane closes as usual.

The supervisor ignores `SIGINT`, so Ctrl-C goes to the harness. The supervisor stops on `SIGTERM` or `SIGHUP`.

## Generated settings

For each claude harness, the adapter writes `<name>.settings.json` into the session directory. It gives the file to `claude --settings`. This option comes last in the argument list. Thus a user `--settings` cannot override the hooks or `apiKeyHelper`:

- Without `target`: `statusLine` and four hooks (`SessionStart`, `UserPromptSubmit`, `Stop`, `StopFailure`).
- With `target`: the same, plus `apiKeyHelper` and `env.ANTHROPIC_BASE_URL`.

Claude Code **merges** `--settings` hooks with the hooks of the user and the project. A live test showed that both sets fire. The `statusLine` setting replaces the user's setting. But `agent-swap statusline` executes the user's command, from `AGENT_SWAP_USER_STATUSLINE`. Thus the visible statusline does not change.

The child process gets the environment of the supervisor, minus the endpoint and credential variables that [SECURITY.md](../SECURITY.md) lists. It keeps `CLAUDE_CODE_OAUTH_TOKEN`, because that can be the subscription login. These inherited values could override the mode that `--settings` selects. The child also gets these variables:

| Variable | Used by |
|---|---|
| `AGENT_SWAP_SESSION_DIR` | hooks and the statusline: where `session.json` is |
| `AGENT_SWAP_STATE_DIR` | the statusline: where `account.json` is |
| `AGENT_SWAP_CONFIG` | `key`: which config file to read |
| `AGENT_SWAP_HARNESS` | hooks and the statusline: the harness name in `account.json` |
| `AGENT_SWAP_EXE` | the pi extension and the opencode plugin: the binary to call |
| `AGENT_SWAP_USER_STATUSLINE` | the statusline: the command to render |

## State

`account.json` is shared by all sessions. It has one entry per harness name:

```json
{"harnesses": {
  "claude": {"windows": {"5h": {"used_percentage": 96, "resets_at": 1794607200},
                         "7d": {"used_percentage": 32, "resets_at": 1795000000}},
             "blocked_until": 1794607800, "resets_at": 1794607200, "updated_at": 1794600000},
  "pi-deepseek": {"blocked_until": 1794603600, "resets_at": 1794603600}
}}
```

- `windows` holds the usage windows. Claude gives `5h` and `7d` from the statusline. Codex gives `primary` and `secondary` from its rollout file. Other kinds have no windows.
- agent-swap merges each window separately. A window in the input replaces the stored window only if it is not older or lower. If the input has no data for a window, agent-swap keeps the stored window until its `resets_at`.
- `blocked_until` is the time when the harness is available again. A harness is blocked while `now < blocked_until`.
- An `account.json` from version 0.1 decodes to an empty map.

`sessions/<id>/session.json` is per supervisor. The hooks or `Poll` write it:

```json
{"native_id": "bf8d…", "idle": true, "rate_limited": false, "started_at": 1794600000}
```

`native_id` is the session ID of the running harness. The supervisor resets this file before each launch.

| Hook | Effect |
|---|---|
| `SessionStart` | idle, not rate-limited, `started_at = now`, `native_id` from the input |
| `UserPromptSubmit` | busy, not rate-limited |
| `Stop` | idle, not rate-limited |
| `StopFailure` | idle, and rate-limited if the `error` text is a limit error (`IsLimitError`) |
| `LimitHit` | rate-limited. The harness reports a limit while it retries. The idle state does not change. |

The hook input is JSON on stdin: `{"session_id": "...", "error": ".."}`. Both fields are optional. Every child gets `AGENT_SWAP_SESSION_DIR`, `AGENT_SWAP_STATE_DIR`, `AGENT_SWAP_CONFIG`, `AGENT_SWAP_HARNESS` and `AGENT_SWAP_EXE`.

## Decision logic

The input is the ordered list of harnesses from `Config.Order`. A lower index means a higher preference.

### Desired

`Desired` returns the first harness that is not blocked. If all harnesses are blocked, it returns the harness with the smallest `blocked_until`.

### Trigger (account-wide)

`Trigger` operates on the current harness. It does nothing if that harness is already blocked. It uses the first rule that matches:

1. **Any live window reaches the threshold** (`threshold` of the harness, else the top-level value). Set `resets_at` to the latest reset among those windows. Set `blocked_until = resets_at + switch_back_delay`.
2. **Otherwise, the session hit a limit error.** If a live window exists, use the reset of the fullest window. Else set `blocked_until = now + cooldown` (of the harness, else the top-level value).
3. **Otherwise**, do nothing.

A window is live until its `resets_at`. Thus old data cannot start a new trigger after a switch back. `UserPromptSubmit` clears the rate-limited flag.

### Decide (per session)

```
          desired ≠ current                           ready ∧ idle
  ┌──────┐ ─────────────────▶ ┌─────────────────┐ ─────────────▶ ┌─────────┐
  │ Noop │                     │ Handoff pending │                 │ quit →  │
  └──────┘ ◀───────────────── └─────────────────┘                 │ relaunch│
                                                                    └─────────┘
                  desired ≠ handoff target
  Noop ◀──────────────────────────────────── Handoff pending   (Cancel)
```

- **No handoff pending:**
  - If the desired harness is the current harness, do nothing.
  - If the session is rate-limited and idle, switch immediately. A message would only cause one more failed turn.
  - Otherwise, start a handoff. The message is `handoff_message` for a move down the list and `return_message` for a move up. It names a one-time ready file and a notes file. Both are in `$TMPDIR/agent-swap-<session>/`, so a sandboxed codex can write them. The supervisor removes this directory on a clean exit.
- **Handoff pending, but its target is no longer the desired harness:** cancel. The supervisor deletes the ready file and clears the handoff.
- **Handoff pending.** Nothing happens while the session is busy. Once it is idle, it is ready if any of these hold:
  - **Down the list:** the ready file exists, the session is rate-limited, or peak usage is ≥ 100%. This wait has no timeout.
  - **Up the list:** the ready file exists, or `switch_back_timeout` has passed since the handoff started.

When ready, the supervisor needs the native session ID. If the ID is empty, it does not switch and tries again at the next poll. Otherwise it types the adapter's quit command. If the child does not exit, the supervisor escalates: it types the command again after 30 s, sends Ctrl-C twice after 60 s, sends `SIGTERM` after 90 s, and then sends `SIGKILL`. It checks these times on each poll tick. Thus each step can come up to one `poll` late.

### Context building

After the child exits, the supervisor reads the desired harness again. Then it does these steps:

1. It calls `Transcript` for the old harness and writes `segment-<n>-<name>.md`. An error goes into the segment as a note. It never stops the switch.
2. It writes `context-<n>.md`: a header, the handoff notes (if the file exists), then the segments that the next harness has not seen, newest first. There is one segment per native session. If the next harness never ran, that is all segments. Else it is the segments after its last run. The file is limited to `context_max_bytes` (default 60000). The notes are always kept. The newest segment comes next and is cut from its oldest end if necessary. Older segments that do not fit are dropped.
3. It starts the next harness. It resumes the native session if that harness ran before. The prompt is `resume_message`, which names the context file.

The user arguments go to the first launch. A later launch gets them only when its kind is the kind of the first harness. For claude, `ReplayArgs` drops the session options.

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
| A directory not yet trusted shows a trust dialog whose default is "No, exit". | agent-swap assumes the directory is already trusted. Codex has its own folder-trust dialog. A live test hit it during a switch. |

## Testing

- `logic_test.go`: thresholds, windows that have reset, the choice of reset time, `Desired` over an ordered list, the readiness matrix, the switch-back timeout, and status text.
- `adapter_*_test.go`: commands, polling and transcripts for each kind, from fixtures in `testdata/`.
- `helpers_test.go`: statusline recording and pass-through, hook state changes, the settings JSON, key output checks, the config overlay, and session naming.
- `runner_test.go`: a full SUB → API → SUB cycle. Fake `tmux` and `claude` shell scripts replace the real programs, and all timings are below 1 s. The test verifies:
  - the launch arguments
  - that the supervisor sends no `/exit` before the ready file exists
  - the resume messages
  - that a window after its reset does not start a new trigger
  - that the supervisor returns the exit code of Claude
