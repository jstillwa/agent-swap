# Security

## How agent-swap handles credentials

- **agent-swap never stores the API key.** It does not write the key to the config file, the state files, the generated settings files or the logs.
- **Claude Code gets the key on demand.** In API mode, the `apiKeyHelper` setting executes `agent-swap key <target>`. That command executes your `key_cmd`. It prints the key to Claude Code through a pipe.
- **agent-swap never puts the key in an environment variable.** The key does not go to tmux, the shell or the child `claude` process. agent-swap sets only `ANTHROPIC_BASE_URL`, which is not secret.
- **agent-swap removes inherited endpoint and credential variables from the child.** The child `claude` process does not get these variables from your shell: `ANTHROPIC_BASE_URL`, `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_CUSTOM_HEADERS`, `CLAUDE_CODE_USE_BEDROCK`, `CLAUDE_CODE_USE_VERTEX`, `CLAUDE_CODE_USE_FOUNDRY`, `ANTHROPIC_BEDROCK_BASE_URL`, `ANTHROPIC_VERTEX_BASE_URL`, `ANTHROPIC_FOUNDRY_BASE_URL`, `ANTHROPIC_FOUNDRY_API_KEY` and `ANTHROPIC_FOUNDRY_RESOURCE`. Thus a credential or endpoint in your shell cannot override the selected mode.
- **agent-swap keeps `CLAUDE_CODE_OAUTH_TOKEN`.** This variable can hold your subscription login.
- **agent-swap does not change your subscription login.** The login stays in the credential store of Claude Code. agent-swap only selects the credential. It adds `apiKeyHelper` or omits it.
- **Each harness keeps its own credentials.** Codex, pi, opencode and agy use their own login stores, for example `~/.codex/auth.json` or `~/.pi/agent/auth.json`. agent-swap does not read, copy or change them. It does not remove inherited variables from the environment of these harnesses.
- **agent-swap writes code into the session directory.** For `pi` it writes the extension `agent-swap-pi.ts`. For `opencode` it writes `opencode.json` and the plugin `agent-swap-opencode.js`. `OPENCODE_CONFIG` points to that file and replaces any value that you set. The config grants no extra permission. The harness loads these files and runs them with your user rights. They only call `$AGENT_SWAP_EXE hook <Event>`. agent-swap writes them again at each launch.
- **Mirrors persist outside agent-swap.** To carry a conversation over, agent-swap writes it as a session into the native store of `claude`, `codex`, `pi` and `opencode`, for example `~/.claude/projects/`. These files hold your prompts, the replies and tool inputs and outputs. They have mode `0600`. They stay after agent-swap exits. agent-swap never deletes them. Each harness sends the conversation to its own provider when it resumes.
- **Tool output becomes user-role text.** A tool call and its result enter the other harnesses as a user-role turn inside an `<agent-swap-tool … untrusted="true">` tag. Tool output can contain instructions from a file or a web page, and the next harness reads them as part of the conversation. agent-swap escapes every `<` in tool input and output and cuts input to 1024 bytes and output to 4096 bytes. It cannot make the content safe. Command-mode output of a harness (for example claude `<bash-stdout>` and task notifications) enters as ordinary user text.
- **Redaction is best-effort.** Before text enters the log, agent-swap replaces these items with `[redacted]`:
  - PEM and PGP private-key blocks
  - `Authorization`, `Cookie` and `Set-Cookie` values
  - `key`, `token`, `secret` and `password` assignments
  - the password in URL userinfo, and `--password` values
  - these token formats: `sk-`, `ghp_`, `gho_`, `ghu_`, `ghs_`, `ghr_`, `github_pat_`, `sk_live_`, `rk_live_`, `glpat-`, `AIza`, `AKIA`, `xox?-`, `xapp-`, `npm_` and JWT

  agent-swap does not find other forms, for example `-p value` or a URL password that contains `/`. Other secrets in a conversation go to every mirror.
- **Ownership limits what agent-swap appends to.** agent-swap appends only to sessions that it created, sessions that the harness created during this run, and the claude `--fork-session` fork of a session you resumed. Before an append it requires that the file size equals the cursor. agent-swap does no file checks for opencode, because opencode keeps its sessions in SQLite. agent-swap uses the opencode CLI and exports the session again to verify the IDs.
- **opencode runs in the project directory.** agent-swap runs `opencode --pure export`, `import` and `debug config` in the project cwd. It removes `OPENCODE_CONFIG` and `AGENT_SWAP_*` from their environment. Its temp files hold the conversation JSON and the `debug config` output, which can contain provider API keys. The files have mode `0600` in a `0700` temp directory, and agent-swap removes them after use. If agent-swap stops before it removes them, they stay in `$TMPDIR/agent-swap-oc-*`.
- **An epoch replaces the conversation.** When you run `/clear`, `/resume` or `/new` in a harness, the new session replaces the conversation. agent-swap then copies its content into the other stores. agent-swap does not follow `/new` in codex. It accepts a new session only if the session's recorded cwd matches the project directory.
- **Only mirrors and context files use `O_EXCL|O_NOFOLLOW`.** Other state files do not.
- **The threat model is the same user.** agent-swap trusts the files of your own user. It checks that a mirror is the regular, single-link file that it created (path, symlinks, device, inode and size) before it appends. This stops accidents and stale links. It is not a boundary against a hostile agent or another process that runs as you.
- **`agent-swap use` is not a security control.** It changes which harness runs. It does not limit what any harness can read, write or send. Any process that can write to the session directory can change the pin or request a sync.
- **To delete a conversation,** remove its mirror sessions. `agent-swap status` lists the native session ID and path of each mirror, and the session `log` records every ID that agent-swap created. Remove those files, or use the delete command of the harness. Also remove `context-<n>.md` and `mirrors.json` in the session directory.
- **The context file contains conversation content.** For `agy`, or when a mirror cannot be written, agent-swap writes `context-<n>.md` into the session directory (file mode `0600`, directory mode `0700`). The next harness reads it and sends it to its own provider. agent-swap does not delete it.
- **The `agy` transcript can include text from tool output.** agent-swap keeps prose runs of 8 or more characters with at least 2 spaces, each up to 500 bytes. Such a run can hold part of a file or a command result. It goes to every mirror as one assistant turn.
- **State files are private.** agent-swap creates the files with mode `0600` and creates new directories with mode `0700`. It does not change the mode of a `state_dir` that already exists. The state files contain usage percentages, reset times, native session IDs and idle flags.

## Your responsibilities

- **Use a secret store for `key_cmd`.** Examples are the macOS Keychain, 1Password CLI and `pass`. Do not use `cat` on a plain-text file.
- **`key_cmd` executes through `sh -c` with your user rights.** Treat your config file as code. Make sure that only you can write to it.
- **Verify `base_url` before you add a target.** In API mode, Claude Code sends your full conversation to that endpoint. This includes prompts, code and tool output.
- **Do not set endpoint or credential variables in Claude Code settings.** An `env` block or `apiKeyHelper` in `~/.claude/settings.json` or in project settings still applies in SUB mode. agent-swap cannot remove them.
- **Each harness stores its own session transcripts**, for example in `~/.claude/projects/` or `~/.codex/sessions/`. agent-swap reads them after each turn. It copies them in part to the other harnesses' stores.
- **Verify the provider of each harness before you add it.** A switch sends the conversation so far to the next harness's provider.

## Reporting a vulnerability

Report vulnerabilities privately through [GitHub Security Advisories](https://github.com/jstillwa/agent-swap/security/advisories/new). Do not open a public issue.

Include:

- the agent-swap version or commit
- the Claude Code version
- your operating system
- steps to reproduce
- the impact

Expect a reply in 7 days or less.

## Supported versions

Only the latest release receives fixes.
