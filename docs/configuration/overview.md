# Configuration

This page shows where OpenVaala keeps its configuration, what every field in `config.yaml` does, and how to check and apply your changes.

You can run OpenVaala without writing any configuration by hand: the first-run setup in the terminal or web UI adds a model provider for you. Read on when you want to change defaults, add executors or extensions, or run OpenVaala on more than one machine.

## The configuration directory

By default everything lives in `~/.config/openvaala/`:

```text
~/.config/openvaala/
├── config.yaml      application settings (this page)
├── agents/          one <name>.yaml per agent           → agents.md
├── models/          one <provider-id>.yaml per provider → models.md
├── executors/       one <id>.yaml per extra executor    → executors.md
├── mcp_servers/     one <id>.yaml per MCP server        → extensions.md
├── hooks/           any number of *.yaml hook files     → extensions.md
├── skills/          <name>/SKILL.md                     → extensions.md
└── plugins/         one directory per plugin            → extensions.md
```

A few rules apply to all of these:

- Only files ending in `.yaml` directly inside a definition directory are read. `.yml` files and subdirectories are ignored (except `skills/` and `plugins/`, which are directory-based).
- Files are parsed strictly. An unknown field, a wrong type or invalid YAML is an error that names the file.
- A directory that does not exist is treated as empty.
- Agent, provider, executor and MCP server files carry a `name` or `id` that must match the file name.

To use a different configuration file, pass `--config-file` to any command or set `OPENVAALA_CONFIG`:

```bash
openvaala --config-file ~/work/openvaala/config.yaml
```

Relative directory paths in `config.yaml` are resolved against the directory that contains `config.yaml`, and a leading `~` expands to your home directory. A missing default `config.yaml` is fine (built-in defaults apply); a file you named explicitly must exist.

OpenVaala also writes to a few locations outside the configuration directory:

| Path | Contents |
| --- | --- |
| `~/.local/share/openvaala/` | Conversations and their index. See [Conversations](../guides/conversations.md#where-conversations-are-stored). |
| `~/.local/state/openvaala/runtime-server.yaml` | Address and token of the local runtime server, read by local clients. |
| `~/.local/state/openvaala/openvaala.log` | Default log file. See [Telemetry](telemetry.md). |
| `openvaala/models-dev.json` in your user cache directory (`~/.cache` on Linux, `~/Library/Caches` on macOS) | The model catalog used by provider setup. |
| `~/.cache/openvaala/hooks/`, `~/.cache/openvaala/plugins/` on each executor's machine | Hook scripts and plugin files copied there to run. See [Hooks](extensions.md#hooks). |

## A complete config.yaml

Every field is optional. This example shows each one with its default value, except where a comment says otherwise:

```yaml
agents:
  directory: agents          # agent definitions
models:
  directory: models          # model provider definitions
  default_model: ""          # provider_id/model_id, e.g. openai/gpt-5; empty = first model by provider id, then model id
  defaults:                  # values a model inherits when it omits a field (see models.md)
    context_window: 256000
    max_output_tokens: 32000
    thinking: true
    reasoning_efforts: []
    default_reasoning_effort: ""
    merge_message_roles: []
    capabilities:
      input: []              # empty means [text]
skills:
  directory: skills          # your private skills
executors:
  directory: executors       # extra executors (ssh, virtual, more local directories)
mcp_servers:
  directory: mcp_servers
hooks:
  directory: hooks
plugins:
  directory: plugins
  disabled: []               # plugin names that are not loaded at all
tools:
  max_image_kb: 500          # largest image read_file hands to the model, in KB; larger images are compressed
run:
  agent_name: default        # root agent for the terminal UI, editors and Codex
server:
  listen: 127.0.0.1:8790     # address of `openvaala serve`
  token: ""                  # empty = reuse the token in runtime-server.yaml, or generate one
  workspace_directory: ""    # empty = the directory serve was started in
client:
  url: ""                    # empty = the local runtime server, started on demand
  token: ""
  path_mappings: []          # e.g. [{client: /Users/me/code, server: /srv/code}]
telemetry:
  log_level: info
  log_file: ""               # empty = ~/.local/state/openvaala/openvaala.log
  otlp_endpoint: ""          # e.g. http://127.0.0.1:4318; empty = no export
  otlp_headers: {}
  capture_content: true
```

## Field reference

### Definition directories

`agents.directory`, `models.directory`, `skills.directory`, `executors.directory`, `mcp_servers.directory`, `hooks.directory` and `plugins.directory` point to the directories described above. None of them may be empty. `--agents-dir` and `--models-dir` override the first two for a single command.

### models

- `default_model` — the model used by agents that do not name one, written `provider_id/model_id`. It must exist in the models directory. When empty, OpenVaala picks the first model after sorting by provider id and then model id. The provider settings in the terminal UI (`/provider`), the web UI and the desktop app can set it for you.
- `defaults` — the values every model inherits when it leaves a field out. The fields are documented in [Models](models.md#inherited-defaults). `context_window` and `max_output_tokens` must be positive.

### plugins

- `disabled` — names of plugins to skip entirely: none of their skills, hooks or MCP servers load. Unknown names are ignored. `openvaala plugin disable` and `enable` edit this list for you.

### tools

- `max_image_kb` — the largest image, in KB, that `read_file` passes to a model. Larger images are scaled down to fit. Must be greater than zero. If you raise it, stay below your provider's per-image limit: Claude, for example, rejects images whose base64 form exceeds about 5 MB, so keep this well under 3700.

### run

- `agent_name` — the root agent for conversations started from the terminal UI, editors (ACP) and Codex. `--agent` overrides it for one command. The web UI and the desktop app always start conversations with the `default` agent.

### server

These settings belong to `openvaala serve`, the runtime server.

- `listen` — `host:port` to listen on. The default only accepts connections from the same machine. Use `0.0.0.0:8790` to accept connections from your network; see [Web UI](../guides/web.md#access-from-another-machine) before you do.
- `token` — the access token clients must present, stored in plain text. When empty, serve reuses the token from `~/.local/state/openvaala/runtime-server.yaml` or generates a random one on first start. Clients send it as an `Authorization: Bearer <token>` header or a `?token=` query parameter. Clients on the same machine read it from that file, so you only need to set it when you want a fixed token.
- `workspace_directory` — the working directory the `local` executor uses when a new conversation does not specify one. Must be an absolute path or start with `~/`. When empty, serve uses the directory it was started in. A runtime server started automatically by a client runs in your home directory.

### client

These settings tell client commands — the terminal UI, `web`, `acp`, `app-server` and `codex` — which runtime server to use. The desktop app always uses the local runtime server.

- `url` — the server's WebSocket address, for example `ws://10.0.0.5:8790/api/v1/openvaala`. When empty, clients connect to the local runtime server and start it if it is not running. `--url` overrides it for one command.
- `token` — the token for `url`. If `url` contains a `?token=` query parameter, that value wins. When both are empty and `url` points at the local runtime server, clients use the local token automatically. `client.token` is never read from `server.token`; the two are separate on purpose.
- `path_mappings` — translates directories between a client machine and the server, so a conversation opened from `/Users/me/code/app` on your laptop runs in `/srv/code/app` on the server:

  ```yaml
  client:
    path_mappings:
      - client: /Users/me/code
        server: /srv/code
      - client: 'C:\Users\me\code'    # Windows clients: drive-letter paths, \ and / are equivalent
        server: /mnt/c/Users/me/code
  ```

  Mappings match whole directory names, and the longest matching prefix wins. Both sides must be absolute paths, `server` must be a Unix path, and the same `client` or `server` directory cannot appear twice. Drive-letter mappings only apply to Windows clients. Editors (ACP) and Codex clients translate paths in both directions; the terminal UI translates only the directory it was started in. Path mappings can only be set in the file.

`ws://` traffic is not encrypted. Use `client.url` across machines only on a network you trust, or put the server behind an HTTPS reverse proxy and use `wss://`. A token in a URL can end up in shell history and proxy logs.

### telemetry

`log_level`, `log_file`, `otlp_endpoint`, `otlp_headers` and `capture_content` control logs, traces and metrics. See [Telemetry](telemetry.md).

## Environment variables

Any scalar setting can be overridden with an environment variable named `OPENVAALA_` followed by the field path in upper case, with dots replaced by underscores:

```bash
OPENVAALA_SERVER_LISTEN=0.0.0.0:8790 openvaala serve
OPENVAALA_CLIENT_URL=ws://10.0.0.5:8790/api/v1/openvaala openvaala
OPENVAALA_MODELS_DEFAULT_MODEL=openai/gpt-5 openvaala
```

Settings are applied in this order, later ones winning: built-in defaults, `config.yaml`, environment variables, then command-line flags (`--agents-dir`, `--models-dir`, `--agent`). Maps and lists of objects — `telemetry.otlp_headers` and `client.path_mappings` — can only be set in the file.

`OPENVAALA_CONFIG` selects the configuration file itself; when it is set it must not be empty.

## Check your configuration

Three commands help you work with configuration. None of them contacts a model provider.

```bash
openvaala config init    # create config.yaml and the agents/, models/, plugins/ directories if missing
openvaala config show    # print the effective settings, defaults and resolved paths included
openvaala config check   # load every definition and check the selected agent
```

`config init` never overwrites existing files; it prints the path of `config.yaml`. `config show` prints tokens and OTLP headers as stored, so be careful where you paste its output.

`config check` loads models, executors, MCP servers, hooks, plugins and agents, validates every reference between them, and prints the selected agent and model:

```text
configuration valid: agent=default provider=openai model=gpt-5
```

It does not read skill files; a malformed skill in your `skills/` directory is reported when the runtime server starts or reloads.

## Apply changes

The runtime server reads configuration when it starts. After editing files, reload it:

```bash
openvaala serve reload
```

You can do the same with `/reload` in the terminal UI or **Settings → Reload config** in the web UI. Changes made through the provider settings in the terminal or web UI, and through `openvaala plugin`, reload automatically.

A reload replaces agents, models, executors, MCP servers, hooks, skills, plugins and `server.workspace_directory`. Open conversations that are idle switch to the new definitions immediately, and conversations you open later use them too. A conversation with a running turn or background command keeps its old definitions; the command lists those conversations so you can reload again once they finish. If the new configuration has an error, the reload fails and the server keeps the old one; at startup, the same error stops the runtime server from starting.

Some settings are only read at startup. To change `server.listen`, `server.token`, `tools.max_image_kb` or anything under `telemetry`, stop the server and let it start again:

```bash
openvaala serve stop
```

Client settings (`client.*` and `run.agent_name`) are read each time you start a client command.
