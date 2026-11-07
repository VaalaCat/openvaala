# Codex

This page shows how to use the Codex terminal UI or desktop app as a front end for OpenVaala.

OpenVaala implements the Codex app-server protocol. Codex clients connect to it as if it were their own backend, while conversations, models and tools come from OpenVaala. Threads you start in Codex are ordinary OpenVaala conversations: you can continue them in the terminal UI, the web UI or an editor, and resume OpenVaala conversations from Codex.

You do not need an OpenAI login for this. Models come from your [OpenVaala providers](../configuration/models.md).

## Codex terminal UI

Install the Codex CLI so that `codex` is on your `PATH`, then run this in the directory you want to work in:

```bash
openvaala codex
```

OpenVaala starts a private app-server, launches `codex --remote` against it with your current directory, and connects to the runtime server (starting it if needed). Arguments after `--` go to Codex unchanged:

```bash
openvaala codex -- resume --last
```

The Codex options `-c`/`--config key=value`, `--enable`, `--disable` and `--strict-config` are passed to Codex as well, wherever they appear before `--`. OpenVaala's own configuration file is set with `--config-file`.

When Codex exits, the private app-server goes away with it, so `codex --remote` commands Codex showed you stop working. To continue a thread later, run `openvaala codex -- resume <session id>` (or `-- resume --last`); OpenVaala reminds you of this after a normal exit.

## Codex desktop app (macOS)

With the Codex desktop app installed, quit it completely, then run:

```bash
openvaala codex app              # current directory
openvaala codex app ~/project
```

The app starts with OpenVaala as its backend, opened on a new thread in that directory, and the command returns. Your Codex configuration is not modified, and the app is neither installed nor closed for you. OpenVaala's configuration flags and `OPENVAALA_*` environment variables are passed on to the app's backend.

## openvaala app-server

`openvaala app-server` is a drop-in replacement for `codex app-server`. Any tool that starts `codex app-server ...` can start `openvaala app-server ...` instead; options OpenVaala does not need are accepted and ignored.

```bash
openvaala app-server                              # stdio (default)
openvaala app-server --listen unix://             # ~/.local/state/openvaala/app-server.sock
openvaala app-server --listen unix:///tmp/ov.sock
openvaala app-server --listen ws://127.0.0.1:4500
```

For socket and WebSocket listeners it prints the `codex --remote <url>` command to connect with. WebSocket listeners only accept loopback IP addresses and have no authentication, so other machines cannot reach them; the WebSocket authentication options (`--ws-auth` and friends) are rejected. To reach a runtime server on another machine, use `--url` or `client.url` instead — the app-server connects to it with the server's token. See [Connecting to a runtime server](../reference/cli.md#connecting-to-a-runtime-server).

## What maps and what doesn't

- **Threads** are conversations; a subagent appears as its own thread. Starting, resuming, forking, archiving, renaming, deleting, compacting, steering and interrupting all work, and so do the `/btw` and `/side` side conversations.
- **Models** listed in Codex are your OpenVaala models (`provider/model`), with their reasoning efforts. Choosing a model or effort in Codex applies to that connection and is not written to your configuration.
- **Approvals and sandbox modes** are ignored. OpenVaala runs tools without asking and reports its sandbox as full access; see [Approvals](../configuration/tools.md#approvals).
- **Images** can be attached as data URLs or local image files.
- **Not supported:** dynamic tools, Codex plugins and apps, and permission profiles.
- **Paths:** with [`client.path_mappings`](../configuration/overview.md#client), the thread's working directory is translated; paths inside command and file-change items are shown as they are on the server.

The agent is `run.agent_name` (or `--agent`); its tools, skills and MCP servers are configured in OpenVaala, not in Codex.
