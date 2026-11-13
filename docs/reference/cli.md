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
