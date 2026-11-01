# Tools

This page lists the tools an agent can call, what each one does, and how to limit them.

Agents get four groups of tools:

- **File tools** — `read_file`, `apply_patch`
- **Command tools** — `bash`, `read_command_output`, `write_stdin`, `stop_command`, `list_commands`
- **Collaboration tools** — `spawn_agent`, `send_message`, `wait_agent`, `interrupt_agent`, `list_agents`, `read_session_history`
- **MCP tools** — named `mcp__<server>__<tool>`; see [MCP servers](extensions.md#mcp-servers)

File and command tools run on an [executor](executors.md). When more than one executor is available, they take an extra `executor` argument, and omitting it uses the conversation's default executor. Collaboration tools run inside the runtime server.

There is no skill tool: agents read a skill's `SKILL.md` with `read_file`.

## File tools

### read_file

Reads a text file by line, or an image.

- `path` — relative to the working directory, or absolute.
- `offset` — first line to read, from 1. Default 1.
- `limit` — number of lines, 1 to 2000. Default 200.

Each line comes back prefixed with its number. A single result holds at most 64 KB of whole lines; when it stops early, the result says where to continue. Files that are not UTF-8 text, and lines longer than 64 KB, are reported as errors. Along with the file, `read_file` picks up any `AGENTS.md` files between the working directory and the file's directory that are not already in the agent's context; see [Project instructions](extensions.md#project-instructions).

**Images.** PNG, JPEG, GIF and WebP files are recognized by their content and returned as images (`offset` and `limit` are ignored). Files up to 20 MB and 8000 × 8000 pixels are accepted. An image larger than [`tools.max_image_kb`](overview.md#tools) (500 KB by default) is converted to JPEG and scaled down until it fits; the result notes that it was compressed and reports the original size. A request carries at most the 20 most recent images; older ones are replaced by a note telling the model to read them again if needed. If the model does not accept images (no `image` in its [`capabilities.input`](models.md#model-fields)), the tool still runs, but the model sees a placeholder instead of the picture.

### apply_patch

Creates, changes and deletes files with one patch, in the same format Codex uses:

```text
*** Begin Patch
*** Add File: docs/notes.md
+# Notes
+First line.
*** Update File: src/main.go
@@
 func main() {
-	fmt.Println("hello")
+	fmt.Println("hello, world")
 }
*** Delete File: old.txt
*** End Patch
```

- Paths are relative to the working directory and must stay inside it.
- Each update hunk is located by matching its context and removed lines exactly; it needs at least one of them.
- Adding requires that the file does not exist; updating and deleting require a regular file. Renames (`*** Move to:`) are not supported.
- A patch may touch up to 128 files and 1024 hunks, and may be up to 4 MB.
- Files are written one after another. If a later file fails, the earlier ones stay changed and the result says the patch was only partly applied.

## Command tools

Commands keep running after the tool call returns, so the agent can start a build or a server, do other work, and come back to it.

### bash

Starts a command with `bash -c` on the executor.

- `command` — required.
- `cwd` — starting directory, relative to the working directory or an absolute path inside it. Default `.`.
- `wait_seconds` — how long to wait before returning. Default 10. If the command finishes sooner, `bash` returns right away.
- `timeout_seconds` — stop the command after this long. Default 600; 0 means no limit.
- `interactive` — run in a terminal (PTY) so `write_stdin` can type into it. Default false.

The result has a `command_id`, the status (`running`, `exited` or `failed`), the exit code once it has finished, and the first page of output (stdout and stderr together, in arrival order). A non-zero exit code is a normal result, not a tool error.

### read_command_output

Reads more output from a command: `command_id`, `cursor` (a byte position; start with 0 or the `next_cursor` from the last call) and `wait_seconds` (default 10, how long to wait for new output). Pages are up to 64 KB. Each command keeps its last 1 MB of output; reading from a position that has been dropped starts at the oldest kept byte and says so.

### write_stdin

Sends characters to an interactive command: `command_id` and `chars`. No newline is added, so include `\n` to press Enter. Only works for commands started with `interactive: true`.

### stop_command

Stops a command and its child processes: `command_id`.

### list_commands

Lists the commands the agent started on an executor, with their status and exit codes.

Commands belong to the agent that started them and are stopped when that agent's session ends — for example when the conversation is reconfigured or the runtime server stops. Each agent can keep up to 64 commands; the oldest finished ones are forgotten first.

The built-in `openvaala` executor and other virtual executors run commands in an embedded interpreter without external programs or network access, and do not support `interactive`.

## Collaboration tools

These let an agent hand work to subagents and coordinate with them. See [Subagents](../guides/conversations.md#subagents) for how this looks in practice.

- `spawn_agent` — start a subagent. Arguments: `task_name` and `message` (required); `agent_name` (any configured agent; default: the same agent); `model` (`provider/model`) and `reasoning_effort`; `fork_turns` — how much finished history to inherit: `none` (default), `all`, or a number of recent turns. The subagent reports back automatically each time it finishes a turn.
- `send_message` — send a message to a subagent, or to the agent's direct parent. `mode` is `normal` (start a new turn, queued if busy) or `steer` (add to the turn in progress). When sending to a subagent, `model` and `reasoning_effort` can switch its model.
- `wait_agent` — wait until a subagent reports back or a steering message arrives, from you or another agent. `timeout_seconds` from 1 to 600; default 30. It returns why it woke; the message itself reaches the model with its next request.
- `interrupt_agent` — cancel a subagent's running turn.
- `list_agents` — list all subagents with their status, token usage and context window.
- `read_session_history` — page through a subagent's history, newest first.

## Limit the tools an agent can use

An agent's `tools` list chooses among the file and command tools only:

```yaml
# agents/reviewer.yaml
tools: [read_file, bash, read_command_output, write_stdin, stop_command, list_commands]
```

- Valid names are `read_file`, `apply_patch`, `bash`, `read_command_output`, `write_stdin`, `stop_command` and `list_commands`. Any other name is an error when a conversation starts.
- Leaving `tools` out gives the agent all of them.
- `bash` only starts commands and waits up to `wait_seconds`; list the other command tools with it so the agent can read output, type input and stop commands.
- Collaboration tools are always available. MCP tools are chosen with the agent's `mcp_servers` list instead.

## Approvals

Tool calls run without asking. If you want a check before certain calls — refusing `rm -rf`, protecting a directory, requiring a ticket number — write a `PreToolUse` [hook](extensions.md#hooks). A hook can block a call with a reason the model sees, or rewrite its arguments.
