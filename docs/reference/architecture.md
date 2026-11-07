# Architecture

This page explains how OpenVaala is put together, for contributors and anyone curious about what happens behind a conversation.

The core idea is to keep three things apart: the **agent loop** (calling the model, deciding what to do next, keeping history), the **tool execution** (touching files and running commands) and the **interfaces** (what you type into). The agent loop runs in one place, tools run wherever you point them, and any number of interfaces can watch and steer the same work.

```text
  terminal UI   web UI   desktop   editor (ACP)   Codex
       │           │        │           │           │
       │           │        │      openvaala acp   openvaala app-server
       │           │        │           │           │
       └───────────┴────────┴─────┬─────┴───────────┘
                                  │  WebSocket, JSON-RPC
                                  │  /api/v1/openvaala
                     ┌────────────▼────────────┐
                     │   runtime server        │
                     │   (openvaala serve)     │
                     │                         │
                     │   conversations,        │
                     │   agent loop, history   │──── model providers
                     └────────────┬────────────┘
                                  │  tool calls
              ┌───────────────────┼────────────────────┐
              ▼                   ▼                    ▼
           local            ssh executors         virtual sandboxes
     (this machine)       (other machines)     (in memory, e.g. openvaala)
```

## Processes

**The runtime server** (`openvaala serve`) is the only process that owns conversations. It keeps them in memory while they are open, calls model providers, sends tool calls to executors, and writes everything to disk. One runtime server uses a data directory at a time; a lock file enforces this. It serves a JSON-RPC API over WebSocket at `/api/v1/openvaala`, protected by a token, and the web UI as static files at `/web/v1/`.

**Clients** connect to it. The terminal UI, the web UI and the desktop app speak its protocol directly. Editors and Codex speak their own protocols, so `openvaala acp` and `openvaala app-server` run as small adapter processes that translate between ACP or the Codex app-server protocol and the runtime server. Clients on the same machine find the local server through `~/.local/state/openvaala/runtime-server.yaml` and start it when it is not running.

Because all state is in the runtime server, a client can disconnect at any time without affecting the work, and every client sees the same live state: streaming text, pending inputs, tool output, subagents.

## The conversation model

<!-- scene: runtime-layers | From the runtime down to the agent loop -->
```text
Runtime              configuration, open conversations
 └─ Conversation     work location, executors, skills
     └─ Session      one agent: model, history, inputs
         └─ Turn     one run, started by an input
             └─ Agent    model ⇄ tools loop
                  │
                  ├─ writes items: messages, reasoning,
                  │  tool calls and results, summaries
                  └─ sends tool calls to executors
```

From the outside in:

- **Runtime** — the root object in the runtime server. It holds the current configuration (agents, models, executors, MCP servers, hooks, skills), the table of open conversations, and routes each request to the right conversation. Reloading configuration swaps the definitions here and hands them to idle conversations.
- **Conversation** — one task you are working on. It owns its work location (executors and working directories), the connections to those executors and MCP servers, the available skills, and all the agents taking part: one root session and any number of subagent sessions.
- **Session** — one agent's line of work inside a conversation: which agent and model, and its history. A session runs one turn at a time and owns the queue of inputs waiting for it, which is where steering and queuing are decided. Subagents are sessions with a parent.
- **Turn** — one run of the agent loop, started by an input. It loads the history, runs the agent, saves what comes out, and ends as completed, failed or interrupted, with its token usage.
- **Agent** — the loop itself, inside a turn: build the request from the context, call the model, run the requested tools (several at once when the model asks for several), add the results, and repeat. Before each model call it reads steering input and decides whether the context needs compacting.

Everything a session records is an **item**, stored in the order it first appeared: messages (yours, the model's, and between agents), reasoning, tool calls and their results, model errors, context summaries, model and executor changes, skill and `AGENTS.md` instructions, hook runs, and interruption markers. Clients render items; the context sent to the model is built from them.

Each conversation, and each session within it, processes its events in order on its own loop, so its state has a single owner and needs no locking. Different conversations and sessions run in parallel.

## Executors and tools

An **executor** is an environment with a file system and a shell: the runtime server's own machine, a machine reached over SSH, or an in-memory sandbox. Executors come in three layers:

```text
ExecutorProvider   which executors this conversation may use
 └─ Executor       one place that can do work
     └─ Workspace  reads and writes files, runs commands
```

- **`ExecutorProvider`** returns the executors available to a conversation. `openvaala serve` uses the one built from your `executors/` files, which gives every conversation the same list. A program that embeds OpenVaala as a Go library can supply its own, for example a different list per conversation.
- **`Executor`** describes one place (its ID, kind, description and default directory) and opens workspaces on it. Adding a new kind of executor means writing one type; the conversation does not need to know about it.
- **`Workspace`** is what the tools actually talk to. A conversation opens its own workspace on each executor it may use, rooted at that executor's working directory, and closes them when the conversation stops, so SSH connections and MCP servers do not outlive it.

Reloading configuration swaps the provider along with the rest of the definitions: idle conversations pick up the new list, and new conversations use it from the start.

A saved conversation that nobody is viewing and that has no running turn or background command is stopped after `server.conversation_idle_timeout` (default 30 minutes), which releases its workspaces. Opening it again reconnects them. See [configuration overview](../configuration/overview.md).

Tools come in two kinds. **Environment tools** — `read_file`, `apply_patch`, the command tools and MCP tools — are provided by each workspace; when several executors offer the same tool, the model sees one tool with an `executor` argument, and the call is routed to that workspace. **Collaboration tools** — `spawn_agent` and friends — act on sessions inside the runtime server. Hooks run around tool calls on the executor where the call happens.

Background commands belong to the session that started them, so they outlive a single tool call or turn and end with the session. MCP servers are started on, or connected from, the executors they are configured for; servers from an editor are relayed through the ACP connection to the editor's machine.

## Storage

Storage is split into two small interfaces in `pkg/storage`, and everything above them is written against those interfaces rather than a particular database:

- **`LogStorage`** — append-only streams of JSON entries, addressed by a path and a stream id. `Append` writes a batch and syncs it to disk before returning, and assigns each entry a sequence number within its stream, starting at 1. Streams are read in either direction with a cursor, and can be deleted one at a time or by path. `pkg/storage/file` implements it as one JSON Lines file per stream, in one directory per path.
- **`RecordStorage`** — keyed JSON records grouped in namespaces, each with an update time. It offers `Set`, `MSet`, `Get`, `MGet`, `Delete` and `DeleteRange`, and `List` with key ranges, ordering by key or update time, simple filters on declared string fields, and cursor pagination. `pkg/storage/sqlite` implements it, and also provides a `LogStorage` on the same database.

`pkg/dao` builds conversations, sessions, turns and items on top. On disk this looks as follows:

```text
pkg/dao   conversations · sessions · turns · items
  │
  ├─ LogStorage     pkg/storage/file
  │    conversations/<conversation-id>/
  │      metadata.jsonl       conversation, sessions, turns
  │      <session-id>.jsonl   one session's items
  │
  └─ RecordStorage  pkg/storage/sqlite
       metadata.db            index for listing and search
```

The logs are the source of truth. In `metadata.jsonl` each entry carries the full latest state of a conversation, session or turn, so the last entry wins; a deletion is an entry too. Every change is appended and synced before the index is updated, and `openvaala reindex` rebuilds `metadata.db` from the logs at any time. This is what makes conversations portable: a conversation directory is complete on its own and can be copied, backed up or moved. See [Where conversations are stored](../guides/conversations.md#where-conversations-are-stored).

<!-- scene: portable-conversations | The logs are the source of truth; the index can always be rebuilt -->
```text
append    metadata.jsonl, <session-id>.jsonl ──▶ metadata.db
reindex   metadata.jsonl ──▶ metadata.db, rebuilt from the logs
move      conversations/<conversation-id>/ ──▶ another machine, then reindex
```

A new conversation is first written to an in-memory SQLite database through `StagedLogStorage` and `StagedRecordStorage`. When it receives its first message, the staged data is copied to the real storage and all reads and writes switch over, which is why an empty conversation leaves nothing on disk.

More backends are planned behind the same interfaces, such as key-value stores, relational and document databases, and S3-compatible object storage.

## Configuration

Configuration is a directory of YAML files (see [Configuration](../configuration/overview.md)), loaded and validated as a whole. A file that fails validation stops the load with an error naming the file; a reload that fails keeps the previous configuration. The runtime server can also write configuration — provider settings and the default model from the UIs, plugin changes — and does so by editing the files, loading the result, and restoring the old files if the load fails.

## Code map

| Path | What lives there |
| --- | --- |
| `cmd/openvaala`, `cmd/openvaala-desktop` | Entry points for the CLI and the desktop app |
| `internal/cli` | Commands, starting and finding the runtime server, the ACP adapter |
| `internal/config` | Loading and validating configuration, plugins |
| `internal/tui` | The terminal UI |
| `internal/desktop` | The native desktop interface and the WebView window |
| `internal/appserver` | The Codex app-server adapter |
| `internal/modelcatalog` | The models.dev catalog used by provider setup |
| `pkg/runtimeserver`, `pkg/runtimeprotocol` | The WebSocket server and its protocol types |
| `pkg/runtime` | Runtime: definitions and open conversations |
| `pkg/conversation` | Conversations, subagents, work location, collaboration tools |
| `pkg/session`, `pkg/turn` | Input queues and turn execution |
| `pkg/agent`, `pkg/agent/agent_context` | The agent loop, context building and compaction |
| `pkg/executor`, `pkg/executor/tools` | Tool routing and the built-in tools |
| `pkg/workspace`, `pkg/terminal` | Local, SSH and virtual workspaces; background commands |
| `pkg/mcpclient`, `pkg/hook`, `pkg/skill` | MCP connections, hooks, skills |
| `pkg/dao` | Conversations, sessions, turns and items on top of storage |
| `pkg/storage`, `pkg/storage/file`, `pkg/storage/sqlite` | The `LogStorage` and `RecordStorage` interfaces, the JSON Lines and SQLite implementations |
| `pkg/telemetry` | Logs, traces and metrics |
| `www` | The web UI (React, built with Vite) |
| `docs` | This documentation, also embedded as the built-in `openvaala` skill |

## Working on the code

```bash
go test ./...
go test -race ./pkg/...
```

For the web UI, run the Vite dev server; it proxies `/api` to a runtime server at `VITE_SERVE_URL` (default `http://127.0.0.1:8790`):

```bash
pnpm --dir www install
pnpm --dir www dev
```

`pnpm --dir www generate:types` regenerates the TypeScript protocol types from the Go definitions.
