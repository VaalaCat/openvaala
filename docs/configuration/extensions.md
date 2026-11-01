# Extensions

This page shows how to extend agents with skills, project instructions, MCP servers, hooks and plugins.

| You want to… | Use |
| --- | --- |
| Give the agent reusable know-how it loads only when relevant | [Skills](#skills) |
| Tell the agent how a project works | [Project instructions](#project-instructions) |
| Add tools from an external server | [MCP servers](#mcp-servers) |
| Run your own scripts at fixed points, for example to block a command or format code | [Hooks](#hooks) |
| Install a bundle of skills, hooks and MCP servers in one step | [Plugins](#plugins) |

After changing any of these files, run `openvaala config check` and `openvaala serve reload`. See [Apply changes](overview.md#apply-changes).

## Skills

A skill is a directory with a `SKILL.md` file. The agent sees each skill's name and description, and reads the full file when a task calls for it.

```markdown
---
name: release-notes
description: Use when drafting release notes from merged pull requests.
---
# Release notes

1. List merged pull requests since the last tag ...
```

- The file must start with YAML frontmatter.
- `name` uses lowercase letters, digits and single hyphens (`release-notes`, not `Release_Notes`). It defaults to the directory name.
- `description` is required. Write it as *when to use* the skill; that is what the agent decides on.
- Other frontmatter fields are ignored. Files above 1 MB are not read.

### Where skills are found

OpenVaala looks in these places, in this order. When two skills share a name, the first one wins.

1. `.agents/skills/` in the conversation's working directory
2. `.claude/skills/` in the conversation's working directory
3. Your private skills directory, `~/.config/openvaala/skills/` (`skills.directory`)
4. Built-in skills on the `openvaala` executor, under `/skills`
5. Skills from enabled [plugins](#plugins), named `<plugin>:<skill>`
6. `~/.agents/skills/` in your home directory
7. `~/.claude/skills/` in your home directory

The working directory and home directory are those of the conversation's default executor, so a conversation on an SSH executor picks up skills from the remote machine.

Directories are searched recursively. Once a directory contains `SKILL.md`, its subdirectories are not searched further. A symbolic link can point straight at a single skill directory. A malformed skill in your private skills directory or the built-in set is an error; elsewhere it is skipped.

### Using a skill directly

Type `/` in the terminal UI, web UI, desktop app or your editor to see the available skills, then pick one, for example `/release-notes`. The skill is attached to your message so the agent follows it from the start. Plugin skills appear as `/<plugin>:<skill>`. You can also write `/skill:<name>`: unlike a bare `/<name>`, which stays plain text when no such skill exists, it is rejected if the skill is not available to the agent.

### The built-in OpenVaala skill

OpenVaala ships one built-in skill, `openvaala`: this documentation. Ask an agent how to configure OpenVaala and it can read the relevant page and edit your configuration for you.

Skills are discovered when a conversation opens or is reconfigured. After adding a skill, reload the configuration or start a new conversation.

## Project instructions

Put project conventions — how to build, how to run tests, what not to touch — in `AGENTS.md` files. OpenVaala reads them for you; there is nothing to configure.

- At the start of every turn, the agent receives each `AGENTS.md` from the root of the Git repository that contains the working directory down to the working directory itself, on the default executor. Outside a Git repository, only the working directory's `AGENTS.md` is read.
- When the agent reads a file deeper in the tree, `AGENTS.md` files between the working directory and that file are added too. On other executors, their `AGENTS.md` files are added after the agent first uses a tool there.
- Deeper files take precedence over higher ones, and what you ask for directly takes precedence over all of them.
- Each file enters the context once, and comes back after the context is compacted.

Only files named `AGENTS.md` are read; `CLAUDE.md` and files in your home directory are not. Use an agent's `instructions` for guidance that applies everywhere.

## MCP servers

Each file in `~/.config/openvaala/mcp_servers/` declares one [Model Context Protocol](https://modelcontextprotocol.io) server. Its tools appear to the model as `mcp__<id>__<tool>`.

```yaml
# mcp_servers/serena.yaml — a local process
id: serena
type: stdio
command: uvx
args: [serena, start-mcp-server]
executors: [local, devbox]   # run it on these executors
required: true               # fail the turn if it cannot connect
```

```yaml
# mcp_servers/context7.yaml — a remote server
id: context7
type: http
url: https://mcp.context7.com/mcp
headers:
  CONTEXT7_API_KEY: ...
```

Fields:

- `id` — required, matching the file name. Letters, digits, `-` and `_` only, and no `__`.
- `type` — `stdio`, `http` or `sse`.
- `command`, `args`, `env` — for `stdio`. `command` is required. `env` is added on top of the inherited environment. `url` and `headers` are not allowed.
- `url`, `headers` — for `http` and `sse`. `url` must be an `http(s)` URL. `command`, `args` and `env` are not allowed.
- `executors` — the executors the server runs on; defaults to `[local]`. Each id must be `local` or an executor from `executors/` that is not `virtual`. The built-in `openvaala` executor cannot run MCP servers.
- `tools` — the tool names (as the server calls them) to offer the model. Defaults to all.
- `required` — when `true`, a connection failure fails the turn. By default the server's tools are skipped instead.
- `disabled` — `true` keeps the file but does not use the server.
- `startup_timeout_seconds` — how long one connection attempt may take; default 30.
- `tool_timeout_seconds` — how long one tool call may take; default 300.

Values are used as written; `${VAR}` is not expanded.

**Where it runs.** A `stdio` server is started in the executor's working directory, on the executor's machine. `http` and `sse` servers are contacted from the executor's machine. Listing an SSH executor therefore runs the server on that remote host.

**Which agents get it.** An agent's `mcp_servers` list chooses servers by id; leaving it out gives the agent every server, and `[]` gives it none. See [Agents](agents.md).

**Servers from your editor.** When you connect through ACP, the MCP servers configured in your editor (stdio, http or sse) are also available in that conversation. They run on the editor's machine and their messages are relayed through the connection, so they disappear when the editor disconnects. See [Editors](../guides/editors.md).

Check connection status with `/mcp` in the terminal UI or the **MCP** tab of the conversation settings in the web UI.

## Hooks

Hooks run your commands at fixed points in a conversation. A hook can block a tool call, rewrite its arguments, add context for the model, or stop a turn from ending.

Put any number of `*.yaml` files in `~/.config/openvaala/hooks/`; all of them apply. Other files there, such as scripts, are not read as configuration.

```yaml
# hooks/go.yaml
hooks:
  PreToolUse:
    - matcher: bash
      hooks:
        - type: command
          script: guard.sh          # relative to this yaml file
          args: [--strict]
          timeout: 10
          on_failure: block
  PostToolUse:
    - matcher: apply_patch
      hooks:
        - type: command
          script: gofmt.sh
  Stop:
    - hooks:
        - type: command
          command: ~/bin/check-tests.sh   # run as-is on the target machine
          timeout: 600
```

### Events

| Event | When it runs | `matcher` is compared with |
| --- | --- | --- |
| `SessionStart` | A conversation starts or is resumed | `source`: `startup` or `resume` |
| `UserPromptSubmit` | You send a message | — (always matches) |
| `PreToolUse` | Before a tool call | `tool_name` |
| `PostToolUse` | After a tool call | `tool_name` |
| `Stop` | The agent is about to finish its turn | — (always matches) |
| `SubagentStart` | A subagent starts | `agent_type` |
| `SubagentStop` | A subagent is about to finish | `agent_type` |

Hooks run on the executor where the event happens, with the executor's working directory as the current directory.

### Matchers

- Empty, missing or `*` matches everything.
- A pattern made only of letters, digits, `_`, `-` and `|` is a list of exact names: `bash|apply_patch`.
- Anything else is a Go regular expression, not anchored.

For tool events a few aliases are understood, so hooks written for other agents keep working: `Bash` → `bash`, `Write` and `Edit` → `apply_patch`, `Read` → `read_file`, `Agent` → `spawn_agent`.

### Hook fields

- `type` — required; only `command` is supported.
- `command` — a command line passed to `bash -c` on the target machine. Programs it uses must exist there.
- `script` — a script file on the runtime server's machine: relative to the YAML file, absolute, or starting with `~/`. It must exist when the configuration loads, start with a shebang and be executable.
- Write exactly one of `command` and `script`.
- `args` — arguments appended to `script`, each shell-quoted. Only valid with `script`.
- `timeout` — seconds; default 60; must be positive.
- `on_failure` — `allow` (default) or `block`: what to do when the hook itself fails (times out, exits with a code other than 0 or 2, or prints invalid output). Only valid for `PreToolUse` and `UserPromptSubmit`.

Before each run, a `script` is copied to `~/.cache/openvaala/hooks/<hash>/<file name>` on the target machine — local and SSH executors alike — and copied again only when its content or permissions change. Only that one file is copied: interpreters and other files it needs must already exist on the target. Script edits take effect on the next run; YAML edits need `openvaala serve reload`.

### Input and output

The hook receives a JSON object on stdin with `hook_event_name`, `session_id` (the conversation id), `turn_id` and `cwd`, plus event fields:

- `SessionStart`: `source`
- `UserPromptSubmit`: `prompt`
- `PreToolUse`: `tool_name`, `tool_input`, `tool_use_id`; `PostToolUse` adds `tool_response`
- `Stop`, `SubagentStop`: `stop_hook_active`, `last_assistant_message`
- In a subagent: `agent_id` and `agent_type`

The hook answers with its exit code and output:

- **Exit 2** blocks; stderr is the reason. `SessionStart` and `SubagentStart` cannot block, so exit 2 there counts as a hook failure.
- **Exit 0, empty stdout** lets things proceed. stderr only goes to the log.
- **Exit 0, plain text** is added as context for the model on `SessionStart`, `SubagentStart` and `UserPromptSubmit`, and ignored on other events.
- **Exit 0, stdout starting with `{`** is parsed as JSON; invalid JSON is a hook failure. Recognized fields:
  - `decision: "block"` with `reason` blocks `UserPromptSubmit`, `PostToolUse`, `Stop` and `SubagentStop`.
  - `hookSpecificOutput.permissionDecision: "deny"` with `permissionDecisionReason` blocks a `PreToolUse` call. `allow` is accepted; `ask` and `defer` are treated as hook failures.
  - `hookSpecificOutput.updatedInput` replaces the tool arguments entirely (`PreToolUse` only).
  - `hookSpecificOutput.additionalContext` adds context for the model.
  - `continue: false` with `stopReason` ends the turn.
  - `systemMessage` is shown to you, not the model.
- **Any other exit code or a timeout** is a hook failure.

stdout and stderr are each truncated at 64 KB. For example, a `PostToolUse` formatter can report what it did:

```json
{"hookSpecificOutput": {"hookEventName": "PostToolUse", "additionalContext": "gofmt reformatted main.go"}}
```

The web UI lists loaded hooks under **Settings → Hooks** and shows each hook run in the conversation.

## Plugins

A plugin is a directory that bundles skills, hooks and MCP servers. Install one from git or a local directory:

```bash
openvaala plugin install https://github.com/me/my-plugins.git --path plugins/review --ref v1.2.0
openvaala plugin list
openvaala plugin update            # all plugins installed with `plugin install`
openvaala plugin disable review
openvaala plugin enable review
openvaala plugin uninstall review
```

- `--path` points at the plugin inside the repository (default: the repository root); `--ref` picks a branch or tag (default: the default branch, git sources only). Installing from git needs `git`.
- `plugin list` prints one line per plugin: name, version, `enabled` or `disabled`, and source.
- These commands change only the plugins directory and `plugins.disabled`, and they do not need the configuration to load beforehand: you can disable, uninstall or update a plugin that keeps the configuration from loading. After the change they load the full configuration and undo the change if it fails to load. On success they reload the local runtime server. New plugin skills appear in conversations you start or reopen afterwards.
- You can also copy or symlink a plugin directory into `plugins/` yourself, then run `openvaala serve reload`.

### The manifest

A native plugin describes itself in `.openvaala-plugin/plugin.yaml` at its root:

```yaml
name: review                     # required; must equal the directory name
version: 1.2.0                   # optional, shown in plugin list
description: Code review helpers # optional
skills: ./skills/                # optional; this is the default
hooks:                           # optional; same format as hooks/*.yaml
  SessionStart:
    - hooks:
        - type: command
          script: ./hooks/inject
          args: [SessionStart]
mcp_servers:                     # optional; same fields as mcp_servers/<id>.yaml, without executors
  - id: review-docs
    type: stdio
    command: node
    args: ["${OPENVAALA_PLUGIN_ROOT}/mcp/server.mjs"]
```

- **Name:** lowercase letters, digits and single hyphens, equal to the directory name. `skill` is reserved.
- **Skills:** found under `skills` (relative to the plugin root; missing means none) and named `<plugin>:<skill>`, such as `review:checklist`. Use that full name in an agent's `skills` list and as `/review:checklist`. A malformed plugin skill is skipped.
- **Hooks:** same events, matchers and fields as `hooks/*.yaml`, but a `script` must be a relative path inside the plugin directory. Hooks from `hooks/` run first, then plugin hooks in plugin-name order.
- **MCP servers:** run on `local` only. If `mcp_servers/` has a file with the same id, that file wins and the plugin's entry is skipped — use this to move the server to another executor or add an API key. Two enabled plugins declaring the same id is an error. While an agent's `mcp_servers` list names a plugin's server, `plugin disable` and `plugin uninstall` refuse to remove it.
- **`${OPENVAALA_PLUGIN_ROOT}`:** in a stdio MCP server's `command`, `args` and `env`, it is replaced with the plugin's absolute directory, and the server also gets it as an environment variable. Hooks receive it as an environment variable, so `command: cat "${OPENVAALA_PLUGIN_ROOT}/core.md"` works. On `local` it points at the plugin directory. On other executors the whole plugin directory is first copied to `~/.cache/openvaala/plugins/<name>-<hash>/` on that machine (this needs `tar` there), so scripts can read the plugin's other files.
- `plugin install` writes `.openvaala-install.yaml` (source, ref, path, commit) into the plugin directory; `plugin update` uses it. Do not edit it.

### Codex and Claude plugins

Plugins that ship `.codex-plugin/plugin.json` or `.claude-plugin/plugin.json` can be installed as they are. OpenVaala reads only their name, version, description and `skills` — one path or a list of paths, searched after the default `./skills/`, each either a directory of skills or a single skill's directory — and does not load their hooks, MCP servers or other components. If a plugin has more than one manifest, the first of OpenVaala, Codex, Claude is used. Source manifests are never modified.

Directories in `plugins/` without any manifest, and names starting with `.`, are skipped. A broken manifest makes the configuration fail to load, so the runtime server will not start, and a reload keeps the old configuration.
