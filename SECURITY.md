# Security

## How agent-swap handles credentials

- **agent-swap never stores the API key.** It does not write the key to the config file, the state files, the generated settings files or the logs.
- **Claude Code gets the key on demand.** In API mode, the `apiKeyHelper` setting executes `agent-swap key <target>`. That command executes your `key_cmd`. It prints the key to Claude Code through a pipe.
- **agent-swap never puts the key in an environment variable.** The key does not go to tmux, the shell or the child `claude` process. agent-swap sets only `ANTHROPIC_BASE_URL`, which is not secret.
- **agent-swap removes inherited endpoint and credential variables from the child.** The child `claude` process does not get these variables from your shell: `ANTHROPIC_BASE_URL`, `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_CUSTOM_HEADERS`, `CLAUDE_CODE_USE_BEDROCK`, `CLAUDE_CODE_USE_VERTEX`, `CLAUDE_CODE_USE_FOUNDRY`, `ANTHROPIC_BEDROCK_BASE_URL`, `ANTHROPIC_VERTEX_BASE_URL`, `ANTHROPIC_FOUNDRY_BASE_URL`, `ANTHROPIC_FOUNDRY_API_KEY` and `ANTHROPIC_FOUNDRY_RESOURCE`. Thus a credential or endpoint in your shell cannot override the selected mode.
- **agent-swap keeps `CLAUDE_CODE_OAUTH_TOKEN`.** This variable can hold your subscription login.
- **agent-swap does not change your subscription login.** The login stays in the credential store of Claude Code. agent-swap only selects the credential: it adds `apiKeyHelper` or omits it.
- **State files are private.** agent-swap creates the files with mode `0600` and creates new directories with mode `0700`. It does not change the mode of a `state_dir` that already exists. They contain usage percentages, reset times, Claude session IDs and idle flags.

## Your responsibilities

- **Use a secret store for `key_cmd`.** Examples are the macOS Keychain, 1Password CLI and `pass`. Do not use `cat` on a plain-text file.
- **`key_cmd` executes through `sh -c` with your user rights.** Treat your config file as code. Make sure that only you can write to it.
- **Verify `base_url` before you add a target.** In API mode, Claude Code sends your full conversation to that endpoint. This includes prompts, code and tool output.
- **Do not set endpoint or credential variables in Claude Code settings.** An `env` block or `apiKeyHelper` in `~/.claude/settings.json` or in project settings still applies in SUB mode. agent-swap cannot remove them.
- **Claude Code stores the session transcripts** in `~/.claude/projects/`. agent-swap does not store them. They hold the same data in both modes.

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
