# Security

## How claude-swap handles credentials

- **claude-swap never stores the API key.** It does not write the key to the config file, the state files, the generated settings files or the logs.
- **Claude Code gets the key on demand.** In API mode, the `apiKeyHelper` setting executes `claude-swap key <target>`. That command executes your `key_cmd`. It prints the key to Claude Code through a pipe.
- **claude-swap never puts the key in an environment variable.** The key does not go to tmux, the shell or the child `claude` process. claude-swap sets only `ANTHROPIC_BASE_URL`, which is not secret.
- **claude-swap does not change your subscription login.** The login stays in the credential store of Claude Code. claude-swap only selects the credential: it adds `apiKeyHelper` or omits it.
- **State files are private.** claude-swap creates them with mode `0600` in a `0700` directory. They contain usage percentages, reset times, Claude session IDs and idle flags.

## Your responsibilities

- **Use a secret store for `key_cmd`.** Examples are the macOS Keychain, 1Password CLI and `pass`. Do not use `cat` on a plain-text file.
- **`key_cmd` executes through `sh -c` with your user rights.** Treat your config file as code. Make sure that only you can write to it.
- **Verify `base_url` before you add a target.** In API mode, Claude Code sends your full conversation to that endpoint. This includes prompts, code and tool output.
- **Claude Code stores the session transcripts** in `~/.claude/projects/`. claude-swap does not store them. They hold the same data in both modes.

## Reporting a vulnerability

Report vulnerabilities privately through [GitHub Security Advisories](https://github.com/jstillwa/claude-swap/security/advisories/new). Do not open a public issue.

Include:

- the claude-swap version or commit
- the Claude Code version
- your operating system
- steps to reproduce
- the impact

Expect a reply in 7 days or less.

## Supported versions

Only the latest release receives fixes.
