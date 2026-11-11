# Architecture

agent-swap is one Go binary with five roles. Each role has a different caller: the user, tmux or a harness. A harness is one coding-agent CLI: `claude`, `codex`, `pi`, `opencode` or `agy`.

| Role | Started by | Code |
|---|---|---|
| Launcher: `agent-swap [flags]` | the user | `internal/swap/launch.go` |
| Supervisor: `agent-swap run` | tmux, as the pane process | `internal/swap/runner.go` |
| Statusline: `agent-swap statusline` | Claude Code, on each statusline refresh | `internal/swap/helpers.go` |
| Hook: `agent-swap hook <Event>` | Claude Code hooks, the pi extension, the opencode plugin | `internal/swap/helpers.go` |
| Key helper: `agent-swap key <target>` | Claude Code `apiKeyHelper` | `internal/swap/helpers.go` |

The decision logic is in `internal/swap/logic.go`: `Peak`, `Desired`, `Want`, `Trigger`, `Decide` and `StatusText`. This file has no I/O. Its tests need no tmux and no Claude Code.

## Design principles

1. **No screen scraping.** Every signal comes from a harness interface: hooks, plugins, or the session files that the harness writes. agent-swap uses tmux only to type input (`send-keys`) and to set `status-right`.
2. **Never cut a turn.** A switch always waits for an idle point, reported by the hooks, the plugins or `Poll`.
3. **Secrets stay out of files and the environment.** Claude Code gets the API key on demand through `apiKeyHelper`.
4. **Account-wide state, per-session action.** All sessions share the usage of each harness. Thus all sessions agree on the desired harness. Each session does its own switch.
5. **No daemon.** Each supervisor polls the shared state file. Each writer holds a `flock` and writes a temporary file, then renames it.

## Adapters

Each kind has an adapter in `internal/swap/adapter_<kind>.go`. The runner uses only this interface (`adapter.go`):

```go
type Adapter interface {
	Command(h Harness, native, prompt string, extra []string, rt Runtime) (Launch, error)
	Quit() string
	Poll(h Harness, rt Runtime, s *Session, u *Usage) error
	Turns(native, cursor string, rt Runtime) ([]Turn, string, error)
	Write(h Harness, m *Mirror, turns []Turn, rt Runtime) (int, error)
}
```

- `Command` builds the child process. `native` is the harness's own session ID to resume, or empty for a new session. `prompt` is the first message, or empty. `extra` holds the user arguments. `Command` also writes the generated files into the session directory. These are the claude settings, the pi extension, and the opencode config and plugin.
- `Quit` is the text that the supervisor types to exit the harness.
- `Poll` updates the session and usage for kinds that give no hooks (`codex`, `agy`). It reads their session files. For the other kinds it does nothing.
- `Turns` reads the completed turns of a native session after `cursor`. An empty cursor reads from the start. It returns the turns and the new cursor. A cursor is a byte offset for the JSONL kinds and the last message ID for opencode. It returns `errRewound`, with the turns, when the last entry before the cursor is no longer on the active chain.
- `Write` puts turns into a mirror. If `m.Native` is empty, it creates a session. Otherwise it appends to the owned session `m`. It returns how many turns `Turns` must read back. `agy` returns `errNoMirror`. Any other `Write` error gets one Create retry, then the context file.

| Kind | Tested | Resume | Signals | Session source (read and mirror) |
|---|---|---|---|---|
| `claude` | 2.1.322 | `--resume <id>` | hooks and statusline through `--settings` | `~/.claude/projects/<slug>/<id>.jsonl`, read and written |
| `codex` | 0.166 | `codex resume <id>` | `Poll` on the rollout file in `$CODEX_HOME/sessions` | the same rollout file, read and written (rollouts under `$CODEX_HOME/sessions`) |
| `pi` | 1.5.1 | `--session <path>` | extension `agent-swap-pi.ts` through `-e` | `~/.pi/agent/sessions/*/*_<id>.jsonl`, read and written |
| `opencode` | 1.19.5 | `-s <id>` | plugin through `OPENCODE_CONFIG` | `opencode export <id>`, written with `opencode import` |
| `agy` | 1.5.5 | `--conversation <id>` | `Poll` on its own log, `history.jsonl` and `conversation_summaries.db` | `conversations/<id>.db`, read only and lossy |

All five kinds passed a live switch. The tested orders were claude → codex → pi → claude and claude → opencode → agy → pi → claude.

Notes per kind:

- **claude:** `Stop` fires before the final line is flushed, so the runner re-reads while the session file is longer than the cursor.
- **codex:** during a run, agent-swap does not follow a new codex session that `/new` or a codex backtrack starts. It keeps reading the session that it launched, so codex turns after `/new` do not enter the conversation. agent-swap cannot tell `/new` apart from another codex in the same repo, because it does not link a rollout to the child process. The writer makes a rollout in the importer style. Create writes no ordinal. Append continues the ordinal if the file has one. The writer never writes `state_5.sqlite`. Codex indexes the rollout on resume. Usage windows come from `rate_limits`. When a session uses paid credits, its percentages do not block it. Only `rate_limit_reached_type` or a limit error blocks it.
- **opencode:** the writer runs `opencode --pure import`. Imports are not transactional, so the writer runs a failed import once more with the same file. Session IDs (`ses_`) are descending. The plugin reports a retry as a limit only after 3 attempts. `opencode export` and `opencode import` run with `--pure` in the run's cwd. The export goes through a private temp file, because a pipe truncates it at 192 KiB.
- **agy:** each session gets its own `--log-file` (`agy.log`) in the session directory. `Poll` reads the native ID from the last `Created conversation <id>` or `Streaming conversation <id>` line after the launch. If there is none, it uses `history.jsonl`. `Poll` also reads limit errors from that log. `conversation_summaries.db` gives only the idle status. `Turns` keeps only prose runs. A prose run has 8 or more characters with at least 2 spaces, and agent-swap keeps up to 500 bytes of it. The steps after the cursor become one assistant turn that starts with `[lossy agy transcript]`. agy has no `Write`. It receives the context file.

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

- **If a switch was pending**, the supervisor starts the next harness. See [Mirrors](#mirrors).
- **Otherwise**, the user quit. The supervisor exits with the harness's exit code, and the pane closes as usual.

The supervisor ignores `SIGINT`, so Ctrl-C goes to the harness. The supervisor stops on `SIGTERM` or `SIGHUP`.

## Generated settings

For each claude harness, the adapter writes `<name>.settings.json` into the session directory. It gives the file to `claude --settings`. This option comes last in the argument list. Thus a user `--settings` cannot override the hooks or `apiKeyHelper`. The file holds these settings:

- Without `target`: `statusLine` and four hooks (`SessionStart`, `UserPromptSubmit`, `Stop`, `StopFailure`).
- With `target`: the same, plus `apiKeyHelper` and `env.ANTHROPIC_BASE_URL`.

Claude Code **merges** `--settings` hooks with the hooks of the user and the project. A live test showed that both sets fire. The `statusLine` setting replaces the user's setting. But `agent-swap statusline` executes the user's command, from `AGENT_SWAP_USER_STATUSLINE`. Thus the visible statusline does not change.

The child process gets the environment of the supervisor. The supervisor removes the endpoint and credential variables that [SECURITY.md](../SECURITY.md) lists, because they can override the mode that `--settings` selects. It keeps `CLAUDE_CODE_OAUTH_TOKEN`, because that variable can hold the subscription login. The child also gets these variables:

| Variable | Used by |
|---|---|
| `AGENT_SWAP_SESSION_DIR` | hooks and the statusline: where `session.json` is |
| `AGENT_SWAP_STATE_DIR` | the statusline: where `account.json` is |
| `AGENT_SWAP_CONFIG` | `key`: which config file to read |
| `AGENT_SWAP_HARNESS` | hooks and the statusline: the harness name in `account.json` |
| `AGENT_SWAP_EXE` | the pi extension and the opencode plugin: the binary to call |
| `AGENT_SWAP_USER_STATUSLINE` | the statusline: the command to render |

## State

All sessions share `account.json`. It has one entry per harness name:

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
{"native_id": "bf8d…", "idle": true, "rate_limited": false, "turns": 12, "started_at": 1794600000}
```

`turns` counts completed turns. The hooks, the plugins and `Poll` increase it on `Stop` and `StopFailure`. When `turns` changes, the supervisor reads the new turns. `native_id` is the session ID of the running harness. The supervisor resets this file before each launch.

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

### Want and Decide (per session)

`Want` is the harness that this session must run. It is the pin from `agent-swap use` if the pin names a configured harness. Otherwise it is `Desired`. `Decide` has two actions:

- **Switch**, if `Want` differs from the running harness and the session is idle.
- **Noop**, otherwise.

There is no handoff phase. A move down the list happens at the first idle point at or over the threshold. The default of 95% leaves room for the turn in progress. If the session is rate-limited and idle, it switches at once.

`request.json` holds the pin and the sync requests. The supervisor validates the file on every read. These are the rules:

- The pin must name a configured harness. The pin ends when that harness gets a new `blocked_until`. The supervisor then sends a notification.
- A `sync` request names harnesses, or `*` for all. The supervisor materializes the mirrors of the harnesses that are not running.

When the decision is Switch, the supervisor needs the native session ID. If the ID is empty, it does not switch and tries again at the next poll. Otherwise it types the adapter's quit command. If the child does not exit, the supervisor escalates in these steps:

1. After 30 s, it types the quit command again.
2. After 60 s, it sends Ctrl-C twice.
3. After 90 s, it sends `SIGTERM`, and then `SIGKILL`.

The supervisor checks these times on each poll tick, so each step can come up to one `poll` late. If the harness becomes busy before Ctrl-C, the supervisor holds the switch and sends no Ctrl-C. At the next idle point, it calculates `Want` again. If the running harness is wanted again, it drops the switch. Otherwise it starts the quit again.

## Mirrors

A mirror is the session of one harness kind that agent-swap keeps in step with the conversation. The code is in `conv.go`, `mirror.go` and `store.go`. The runner is the only authority. agent-swap does not persist the log. If the runner stops, its child stops too. The runner writes `mirrors.json` for `status` only and never reads it back.

**Log.** The log is `[]Turn{Origin, Role, Text}` in memory. `Origin` is the harness kind that produced the turn. A tool call and its result become one user-role turn: `<agent-swap-tool source=… tool=… untrusted="true">input → output</agent-swap-tool>`. agent-swap cuts the input to 1024 bytes and the output to 4096 bytes. Before text enters the log, `escape` neutralizes the tag and `redact` removes known secret formats. agent-swap skips the prompts that it types itself, because they are not conversation.

**Mirror.** agent-swap keeps one mirror per kind. claude-sub and claude-api share the claude mirror. Two pi harnesses share the pi mirror. A mirror holds these fields:

- the native ID, the path and a cursor
- `Synced`: the log index that the session already holds
- `Base`: the log index where its last Create started
- the device and inode
- `Owned`

**Reading (`ingest`).** The supervisor runs `ingest` at these points:

- when `turns` in `session.json` changes
- once more on the next tick
- while the session file is longer than the cursor
- at child exit

agent-swap reads agy only at exit. When the running harness moves to another native session through `/clear`, `/new` or `/resume`, `ingest` starts an **epoch**. agent-swap does not follow this for codex. See the codex note. If the new session is in the kind's store for this cwd and agent-swap can read it, the log restarts from that session. The other mirrors are then dropped. Otherwise sync from that harness pauses, and agent-swap logs the event. agent-swap logs a rewind (`errRewound`). The other mirrors keep the earlier turns.

**Writing (`materialize`).** It runs at takeover, and for `sync`. The delta is the log after `Synced`, minus the turns whose `Origin` is the target kind, because those are native there.

- **No delta:** nothing to write. The harness resumes as is.
- **Append** if the mirror is owned, `logBytes(log[Base:])` is within the budget, and the identity check passes. The writer first reads the tail of its own file. For claude, each unanswered `tool_use` gets a synthetic error `tool_result`, and the chain starts at the last real entry. If a user turn would follow a user turn, claude and pi insert an assistant turn `[agent-swap] (turn interrupted)`. codex and opencode accept consecutive user messages, so their appends only merge. It merges same-role turns.
- **Create** otherwise: a new session with `capTurns(log, budget)` as text, with `[agent-swap] N earlier turns omitted` in front when capped. One retry follows a failed write.
- **Self-check:** after each write, `Turns` must parse the session and read back the written turns.
- **Fallback:** if Create fails twice, or the kind has no mirror (agy), the supervisor writes `context-<n>.md` (mode 0600) and starts the harness fresh with `resume_message`.

The budget is `context_max_bytes` of the harness, else the top-level value (default 200000).

**Ownership.** agent-swap appends only to a session that agent-swap or the harness created during this run. A session is owned if its first timestamp is not older than the run. A session from a first launch with `--resume` or `--continue` is not owned. claude gets `--fork-session`, so its fork is owned. For the other kinds, agent-swap reads the earlier session but never writes it. The first takeover creates a copy.

The recorded sessions in `internal/swap/testdata/spike/` show the native formats. Each file name gives the harness version.

**Identity check.** Before an append, the path must match the stored kind, root and ID. After symlink resolution, the path must stay under the store root. The file must be a regular file with one link and the same device and inode. It must be exactly as long as the cursor. agent-swap creates files with `O_EXCL|O_NOFOLLOW` and mode 0600. An append is one `O_APPEND` write plus `fsync`.

After a cut-off (`RateLimited` or `StopFailure`) the first prompt tells the next harness to resume. After a normal stop there is no prompt.

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

- `logic_test.go`: thresholds, windows that have reset, the choice of reset time, `Desired` over an ordered list, `Want` with a pin, `Decide`, and status text.
- `adapter_*_test.go`: commands, polling, reading and writing for each kind, from fixtures in `testdata/`.
- `conv`, `store` and `mirror` tests: the log, caps, identity checks, ownership, epochs and fallbacks.
- `helpers_test.go`: statusline recording and pass-through, hook state changes, the settings JSON, key output checks, the config overlay, and session naming.
- `runner_test.go`: full switch cycles. Fake `tmux` and harness shell scripts replace the real programs, and all timings are below 1 s. The tests verify:
  - the launch arguments
  - that the supervisor sends no `/exit` while the session is busy
  - that a switch materializes only the target and resumes its session
  - held, dropped and pinned switches
  - epochs and the context-file fallback
  - that a window after its reset does not start a new trigger
  - that the supervisor returns the exit code of the harness
