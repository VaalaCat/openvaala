# Agents

This page shows how to define agents: their instructions, model, tools, skills and MCP servers.

An *agent* is a named role. Every conversation starts with one root agent, which can start subagents of any configured agent for parts of the task. Without any configuration you already have one: the built-in `default` agent, a general-purpose assistant with every tool, skill and MCP server available and the default model.

## A complete agent file

Each file in `~/.config/openvaala/agents/` defines one agent:

```yaml
# agents/reviewer.yaml
name: reviewer                       # must match the file name
description: Reviews a change for bugs and missing tests without editing files
instructions: |
  You review code changes. Read the diff and the surrounding code,
  run the tests, and report concrete problems with file and line.
  Do not modify files.
model:                               # optional; default: models.default_model
  provider: openai
  model: gpt-5
compact_model:                       # optional; model that writes context summaries
  provider: openai
  model: mini
tools: [read_file, bash, read_command_output, write_stdin, stop_command, list_commands]
skills: [code-review]                # optional; default: all skills
mcp_servers: [serena]                # optional; default: all; [] for none
```

## Fields

- `name` — required, matching the file name. Letters, digits, `-`, `_` and `.`.
- `description` — what the agent is for. Other agents read it when choosing which agent to hand a subtask to, so describe the job, not the personality.
- `instructions` — required. The agent's system instructions.
- `model` — the model to use: `provider` is a provider id from `models/`, `model` is a model id in that provider (default `default`). When omitted, the agent uses `models.default_model`; see [Choosing a model](models.md#choosing-a-model).
- `compact_model` — the model that summarizes the conversation when the context fills up. Defaults to `model`. Its context window must not be smaller than the main model's. A cheaper model with a large window works well here.
- `tools` — the file and command tools the agent may use. Omit it for all of them. See [Limit the tools an agent can use](tools.md#limit-the-tools-an-agent-can-use).
- `skills` — the skills the agent may use, by name (plugin skills as `<plugin>:<skill>`). Omit it for all skills.
- `mcp_servers` — the MCP server ids the agent may use. Omit it for all servers, including those your editor provides; write `[]` for none. Every id must exist in `mcp_servers/` or a plugin (disabled servers count).

Every model reference must resolve to a configured model, or the configuration fails to load.

## Customize the default agent

`agents/default.yaml` is special: it starts from the built-in `default` agent and replaces only the fields you write. `name` may be left out (if present it must be `default`), and `instructions` is optional. To pin the default agent to one model, this is all you need:

```yaml
# agents/default.yaml
model:
  provider: anthropic
  model: claude-sonnet
```

Every other agent file must set `name` and `instructions`.

If no model is configured at all, agents have no model until you [add a provider](models.md#add-a-provider-without-editing-files).

## Which agent runs

- The terminal UI, editors (ACP) and Codex start conversations with `run.agent_name` (default `default`); `--agent <name>` overrides it for one command.
- The web UI and desktop app start conversations with `default`.
- A root agent can start a subagent of any configured agent with `spawn_agent`. A subagent of the same agent inherits the current model; a different agent uses its own default model unless the call names one. See [Subagents](../guides/conversations.md#subagents).

Check an agent with `openvaala --agent reviewer config check`, then apply changes with `openvaala serve reload`.
