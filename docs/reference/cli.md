# CLI

This page lists every `openvaala` command and flag.

```text
openvaala [flags]                  start the terminal UI
openvaala tui                      start the terminal UI
openvaala serve                    run the runtime server
openvaala serve stop               stop the local runtime server
openvaala serve reload             reload configuration in the local runtime server
openvaala web                      open the web UI
openvaala acp                      serve an editor over ACP on stdio
openvaala app-server               serve Codex clients over the Codex app-server protocol
openvaala codex [-- CODEX_ARGS]    open the Codex TUI connected to OpenVaala
openvaala codex app [directory]    open the Codex desktop app connected to OpenVaala (macOS)
openvaala session                  manage sessions
openvaala agent                    list Agents and options
openvaala executor                 list Executors and run tools
openvaala exec                     run a command on an Executor
openvaala terminal                 manage interactive terminals
openvaala skill                    list, search and read skills
openvaala mcp-bridge               relay stdio MCP for Agent processes (normally invoked by the Agent)
openvaala reindex                  rebuild the conversation index
openvaala plugin <subcommand>      install and manage plugins
openvaala config <subcommand>      initialize, inspect and validate configuration
openvaala completion <shell>       generate a shell completion script
openvaala help [command]           show help
```

Run `openvaala <command> --help` for the same information in the terminal.

## Global flags

These work with every command, before or after the command name:

- `--config-file <path>` — the configuration file. Default: `OPENVAALA_CONFIG`, then `~/.config/openvaala/config.yaml`.
- `--data-dir <dir>` — override the local data and runtime state directory.
- `--agents-dir <dir>` — the agent definition directory, overriding `agents.directory`.
- `--models-dir <dir>` — the provider definition directory, overriding `models.directory`.
- `--agent <name>` — the root agent, overriding `run.agent_name`.
- `-h`, `--help` — show help.
- `-v`, `--version` — print the version (root command only).

When a client starts the local runtime server for you, it passes these flags on, so the server uses the same configuration.

## Connecting to a runtime server

`tui` (and `openvaala` without a command), `web`, `acp`, `app-server`, `codex` and `codex app` accept:

- `--url <ws-url>` — the runtime server to connect to, such as `ws://10.0.0.5:8790/api/v1/openvaala`.

Without `--url`, they use `client.url`; without that, they use the local runtime server and start it in the background if it is not running. A server started this way by `tui`, `acp`, `app-server` or `codex` exits on its own about ten seconds after its last client disconnects and nothing is running. A server started by `web` or the desktop app keeps running until you stop it. See [Configuration → client](../configuration/overview.md#client) for tokens and path mappings.

The `session`, `agent`, `executor`, `exec`, `terminal`, and `skill` commands also accept `--url` and `--token`. They read `OPENVAALA_URL` and `OPENVAALA_TOKEN` when those flags are omitted. `OPENVAALA_CONVERSATION_ID` and `OPENVAALA_SESSION_ID` supply the collaboration context. Where a command accepts `--conversation`, it overrides the conversation environment variable.

## tui

```bash
openvaala [--url <ws-url>]
openvaala tui [--url <ws-url>]
```

Starts the terminal UI in the current directory. See [Terminal UI](../guides/terminal.md).

## serve

```bash
openvaala serve
```

Runs the runtime server in the foreground: it owns all conversations and serves the WebSocket API at `/api/v1/openvaala` and the web UI at `/web/v1/` on `server.listen`. Only one runtime server can use the data directory at a time. It prints its address and logs to the terminal; stop it with Ctrl+C or `openvaala serve stop`.

Use `openvaala serve --listen <host:port>` to override `OPENVAALA_SERVER_LISTEN` and `server.listen` for this invocation.

### serve stop

```bash
openvaala serve stop
```

Asks the local runtime server to shut down and waits up to 10 seconds. Prints `stopped`, or `no runtime server is running` if there is none. Turns that are still running stop with it.

### serve reload

```bash
openvaala serve reload
```

Reloads agents, models, executors, MCP servers, hooks, skills and plugins in the local runtime server. Prints `configuration reloaded`, and lists conversations with a prepared configuration waiting to apply automatically when idle; no second reload is needed. See [Apply changes](../configuration/overview.md#apply-changes).

## web

```bash
openvaala web [--url <ws-url>]
```

Prints the web UI address — including the access token — and tries to open it in your browser. When the local runtime server listens on all interfaces, it also prints a `Network:` address for each local IPv4 address. See [Web UI](../guides/web.md).

## acp

```bash
openvaala acp [--url <ws-url>]
```

Serves one editor over the [Agent Client Protocol](https://agentclientprotocol.com) on stdin and stdout. Your editor starts this command; you do not run it by hand. Logs go to stderr. See [Editors](../guides/editors.md).

## app-server

```bash
openvaala app-server [--listen <url>] [--stdio] [--url <ws-url>]
```

A drop-in replacement for `codex app-server`: tools that start `codex app-server ...` can start `openvaala app-server ...` instead.

- `--listen <url>` — where to listen:
  - `stdio://` (default) — stdin and stdout.
  - `unix://` — a Unix socket at `~/.local/state/openvaala/app-server.sock`.
  - `unix://PATH` — a Unix socket at `PATH`.
  - `ws://IP:PORT` — a WebSocket on a loopback IP address such as `127.0.0.1` (host names and other addresses are refused). There is no authentication.
- `--stdio` — same as `--listen stdio://`.

For socket and WebSocket listeners it prints the matching `codex --remote <url>` command. Other `codex app-server` options that OpenVaala does not need are accepted and ignored; the WebSocket authentication options (`--ws-*`) are rejected. See [Codex](../guides/codex.md).

## codex

```bash
openvaala codex [-- CODEX_ARGS...]
openvaala codex -- resume --last
```

Starts the Codex CLI's terminal UI in the current directory, connected to OpenVaala. Requires `codex` on your `PATH`. Arguments after `--` go to `codex`. The Codex options `-c`/`--config key=value`, `--enable`, `--disable` and `--strict-config` are passed to `codex` too, wherever they appear before `--`. (Use `--config-file` for OpenVaala's own configuration.)

### codex app

```bash
openvaala codex app [directory]
```

macOS only. Opens the installed Codex desktop app with OpenVaala as its backend, in `directory` (default: the current directory), and returns. Quit the Codex app first. No Codex configuration is changed and the app is not installed for you.

## session

The session CLI and the local `openvaala` MCP tools provide equivalent collaboration operations. With an injected session token, both use the same conversation and session permissions. If a network sandbox such as Codex's blocks the CLI from connecting to OpenVaala, use the `openvaala` MCP tools. Without a source session, an operator can use `spawn` to create a new conversation.

- `openvaala session spawn <prompt>` (`spawn_agent`). Use `--agent <name>` to choose the Agent and `--model <provider/model>` to choose the model. `--reasoning-effort <value>` selects native model reasoning effort. Repeat `--initial-option <kind:id=value>` for external startup options. Use `--executor <id>`, `--cwd <path>`, `--task <name>`, `--label <key=value>`, and `--background` to set work location and metadata. For a child session, `--fork-turns <none|all|n>` selects completed turns to inherit and `--notify-on-finish=false` suppresses the parent's completion notice.
- `openvaala session send <session-id> <prompt>` (`send_message`). Use `--prompt-file <path>` in place of the prompt to read a local file. `--mode <normal|steer>` queues a turn or supplements the current one. `--interrupt` interrupts the current turn before sending. `--model <provider/model>` and `--reasoning-effort <value>` update a descendant's model selection before sending.
- `spawn` and `send` wait for the submitted turn's final text. Add `--no-wait` to print the session and input IDs as soon as the input is accepted.
- `openvaala session ls [--conversation <id>]` (`list_agents`). Filter by `--session-id <id>`, `--status <idle,running,stopping,stopped>`, `--cwd <path>` (`*` for any directory), `--since-hours <n>` (default 24, 0 for any time), or `--label <key=value>`. Use `--all` to include background sessions and `--limit <n>` to set the page size.
- `openvaala session logs <session-id> [--limit <n>] [--cursor <n>]` (`read_session_history`). Pass the printed `next_cursor` to read older items. With a source session, only the descendant's own history after the fork is included.
- `openvaala session wait [session-id] [--timeout <seconds>] [--wait-for subagent_result]` (`wait_agent`). Omit the ID to wait on the source session. Use `subagent_result` to return immediately when no descendants are running and there is no unread message.
- `openvaala session stop <session-id>` (`interrupt_agent`) requests cancellation of the active turn.
- `openvaala session option <session-id> [--set <kind:id=value>]` reads or sets external options (`get_agent_options`, `set_agent_option`). `openvaala session reload <session-id>` restarts the external driver (`reload_agent`).
- `openvaala session inspect <session-id>` shows session details, `session attach <session-id>` streams its output, and `session open <session-id>` opens it in the Web UI.
- `openvaala session update <session-id> [--task-name <name>] [--label <key=value>] [--background=<bool>]` updates session metadata. `session detach <session-id>` moves an external child into its own conversation.
- `openvaala session import --agent <name> [--executor <id>] [--cwd <path>]` lists external sessions. Use `--query <text>` to search titles and directories and `--cursor <cursor>` for the next page. Add the external session ID as an argument to import it.

## agent

- `openvaala agent ls` lists configured Agent definitions (`list_agent_definitions`).
- `openvaala agent options <agent-name>` reads a definition's available external options (`get_agent_options`).

## executor and exec

- `openvaala executor ls [--conversation <id>] [--refresh-tools]` (`list_executors`) lists Executors. Within a conversation, the working Executor is marked `(default)`. Refreshing a tool table only affects a native model turn.
- `openvaala exec --executor <id> [--conversation <id>] [--cwd <path>] -- <command> [args...]` runs one command and prints its output. Relative working directories start at the selected Executor's workspace root.
- `openvaala executor tool <name> [arguments-json] [--conversation <id>]` invokes the same workspace and command tools the local MCP exposes, using their JSON argument names and defaults. For example, `openvaala executor tool read_file '{"path":"README.md","offset":1,"limit":20}'`. Available tools depend on the source session and its Executors.

## terminal

- `openvaala terminal ls` lists running terminals (`terminal_list`).
- `openvaala terminal open [--executor <id>] [--cwd <path>] [--rows <n>] [--cols <n>]` opens a terminal and prints its ID (`terminal_open`).
- `openvaala terminal send <terminal-id> <keys>` writes keys verbatim (`terminal_send_keys`). Include a newline to submit a command.
- `openvaala terminal capture <terminal-id> [--lines <n>]` prints recent output with ANSI control sequences removed (`terminal_capture`).
- `openvaala terminal close <terminal-id>` closes the terminal (`terminal_close`).

## skill

- `openvaala skill list [--agent <name>]` lists available skills (`list_skills`).
- `openvaala skill search <query> [--agent <name>]` searches names and descriptions (`search_skills`).
- `openvaala skill get <name> [file] [--agent <name>]` reads `SKILL.md` or a relative file in that skill's directory (`get_skill`). Example: `openvaala skill get openvaala reference/cli.md`.

`--agent` selects the skill catalog for an operator. A session token always uses the calling Agent's configured skills plus the built-in `openvaala` skill.

## reindex

```bash
openvaala reindex
```

Rebuilds `~/.local/share/openvaala/metadata.db` from every conversation's `metadata.jsonl`. Run it after copying conversation directories in, while no other OpenVaala process uses the data directory (run `openvaala serve stop` first). See [Conversations](../guides/conversations.md#move-conversations-to-another-machine).

## plugin

```bash
openvaala plugin install <source> [--path <dir>] [--ref <branch-or-tag>]
openvaala plugin update [name...]
openvaala plugin uninstall <name>
openvaala plugin enable <name>
openvaala plugin disable <name>
openvaala plugin list
```

- `install` — installs from a git URL or a local directory. `--path` is the plugin directory inside the repository (default: the root); `--ref` is a git branch or tag (default: the default branch).
- `update` — re-fetches plugins installed with `plugin install`; all of them when no name is given.
- `uninstall` — removes the plugin directory.
- `enable`, `disable` — edit `plugins.disabled` without removing files.
- `list` — one line per plugin: name, version, state and source.

Each change reloads the local runtime server, if one is running. See [Plugins](../configuration/extensions.md#plugins).

## config

```bash
openvaala config init
openvaala config show
openvaala config check
```

- `init` — creates `config.yaml` and the `agents/`, `models/` and `plugins/` directories if missing, without overwriting anything, and prints the path.
- `show` — prints the effective settings as YAML, including defaults and resolved paths. Tokens are printed as stored.
- `check` — loads all definitions, checks the selected agent and prints `configuration valid: agent=… provider=… model=…`. It does not contact model providers.

See [Configuration](../configuration/overview.md#check-your-configuration).

## Desktop app commands

The desktop app's executable contains the full CLI above, plus:

```bash
openvaala                 # opens the desktop window (WebView)
openvaala app             # same
openvaala app --native    # opens the native interface instead
openvaala --native        # same
```

The packaged app names that executable `openvaala` (`OpenVaala` on macOS). `go install github.com/VaalaCat/openvaala/cmd/openvaala-desktop@latest` installs the same commands as a binary named `openvaala-desktop`. See [Install](../getting-started/install.md).

In the desktop build, `tui` starts the terminal UI. `--url` is not accepted for the desktop window, which always uses the local runtime server. See [Desktop](../guides/desktop.md).

## Environment variables

- `OPENVAALA_CONFIG` — the configuration file, like `--config-file`.
- `OPENVAALA_<GROUP>_<KEY>` — overrides a setting in `config.yaml`, such as `OPENVAALA_SERVER_LISTEN` or `OPENVAALA_CLIENT_URL`. See [Environment variables](../configuration/overview.md#environment-variables).
