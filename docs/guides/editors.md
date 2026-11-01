# Editors

This page shows how to use OpenVaala as the agent inside an editor that supports the [Agent Client Protocol](https://agentclientprotocol.com) (ACP), such as Zed.

The editor starts `openvaala acp` and talks to it over stdin and stdout. That process connects to the same runtime server as your other clients, so a conversation you start in the editor also shows up in the terminal UI, the web UI and the desktop app, and the other way round.

## Set up Zed

Make sure `openvaala` is on the `PATH` that Zed sees, then add an agent server to Zed's `settings.json` (Agent Settings → External Agents → Add Agent → Add Custom Agent opens it for you):

```json
{
  "agent_servers": {
    "OpenVaala": {
      "type": "custom",
      "command": "openvaala",
      "args": ["acp"],
      "env": {}
    }
  }
}
```

Open the Agent Panel and start a new thread with **OpenVaala**. The thread's working directory is your project directory.

To use a different configuration file, add it to the arguments: `"args": ["--config-file", "/path/to/config.yaml", "acp"]`. Other ACP editors need the same two things: the command `openvaala` and the argument `acp`.

If you have not added a model provider yet, run `openvaala` in a terminal once and follow the setup, or use the web UI; the editor itself has no provider setup.

## What works

- **Conversations** — new sessions, the session list (most recent first, filterable by directory), and loading a past session with its full history.
- **Streaming** — messages, reasoning, and tool calls with their status, input and output. File locations are shown for `read_file` and `apply_patch`; command output streams as it arrives.
- **Cancel** — stops the running turn. If a turn started from another client is running when you send a prompt from the editor, that turn is interrupted and your prompt runs next.
- **Session options** — model, reasoning effort (when the model has effort levels) and the default [executor](../configuration/executors.md).
- **Slash commands** — your [skills](../configuration/extensions.md#skills) appear as slash commands.
- **Context** — images, files and selections you attach. Text resources and images are supported; other attachment types are rejected.
- **Context usage and title** — the conversation's context usage and title are reported to the editor.

The editor's own permission prompts, modes and plans are not used: OpenVaala runs tools without asking (see [Approvals](../configuration/tools.md#approvals)).

## MCP servers from your editor

MCP servers configured in the editor (stdio, http or sse) are made available to the conversation. They run on the editor's machine: stdio servers start in your project directory with the editor's environment, and their messages are relayed to the runtime server over the ACP connection. Their tools appear as `mcp__<name>__<tool>`, with characters other than letters, digits, `-` and `_` in the name replaced by `_`.

These servers belong to the editor connection. When the editor disconnects they go away, and later turns in that conversation no longer see their tools. An agent only receives them when its `mcp_servers` list is left out. MCP servers from your own configuration are available as usual; see [MCP servers](../configuration/extensions.md#mcp-servers).

## When the editor closes

Closing the editor ends `openvaala acp`, but not the work: a running turn keeps going in the runtime server, and you can load the session again later. A runtime server that the editor started on its own exits about ten seconds after its last client disconnects, once nothing is running. Run `openvaala serve stop` to stop it at any time.

## Use a runtime server on another machine

To run conversations on a server while editing locally, point the editor's `openvaala acp` at it with `client.url` and `client.token` in your configuration, or `--url` in the arguments:

```json
"args": ["--url", "ws://10.0.0.5:8790/api/v1/openvaala", "acp"]
```

If the project lives at a different path on the server, add a [`client.path_mappings`](../configuration/overview.md#client) entry. ACP sessions translate the working directory in both directions; without a matching mapping, a new session on a remote server uses the server's `server.workspace_directory`. A session you load keeps the directory it was created with. Tool locations shown in the editor use the server's paths.
