# Executors

This page explains where an agent's file and command tools run, and how to add other machines and sandboxes.

An *executor* is an environment with its own file system and shell. The agent loop and the conversation history always stay in the runtime server; only tool calls — reading and patching files, running commands, starting MCP servers — happen on an executor. One conversation can use several executors, so a single task can read code on your laptop, build on a build box and check logs on a staging server.

Executors are a separate layer so that new kinds of environment can be added without changing how agents work. Today there are three kinds: directories on the runtime server's machine (`local`), remote machines over SSH (`ssh`) and in-memory sandboxes (`virtual`). Support for worker processes on other machines and for hosted sandboxes is planned.

## Built-in executors

Two executors exist without any configuration:

- **`local`** — the machine the runtime server runs on. Its working directory is chosen per conversation; see [Work location](#work-location).
- **`openvaala`** — a built-in in-memory sandbox rooted at `/workspace`. Nothing it writes touches your disk, and its contents are reset whenever the conversation is reopened or reconfigured. It also holds the built-in documentation skill under `/skills/openvaala/`. Its shell is an embedded Bash interpreter with common file and text utilities (`cat`, `grep`, `sed`, `awk`, `jq`, `find`, `tar` and so on). It cannot run external programs, has no network access, and runs scripts with `bash <file>` rather than `./script`.

## Add executors

Each file in `~/.config/openvaala/executors/` adds one executor. You can add as many as you like, of any kind:

```yaml
# executors/buildbox.yaml
id: buildbox
kind: ssh
description: Linux build machine with the full toolchain; use it to compile and run tests
workspace_directory: /home/me/src/app
ssh:
  address: me@buildbox.internal:22
  identity_file: ~/.ssh/id_ed25519
```

```yaml
# executors/gpu.yaml
id: gpu
kind: ssh
description: GPU machine for training and benchmarks
workspace_directory: /data/experiments
ssh:
  address: gpu.internal
```

```yaml
# executors/staging.yaml
id: staging
kind: ssh
description: Staging server; read logs and check services, do not deploy from here
workspace_directory: /srv/app
ssh:
  address: deploy@staging.example.com
```

```yaml
# executors/scratch.yaml
id: scratch
kind: virtual
description: Throwaway in-memory workspace for experiments
```

```yaml
# executors/notes.yaml
id: notes
kind: local
description: My notes repository on this machine
workspace_directory: ~/notes
```

The model chooses executors by their `description`, so say what each one is for.

### Fields

- `id` — required, matching the file name. `local`, `client` and `openvaala` are reserved.
- `kind` — `local` (a directory on the runtime server's machine), `virtual` (an in-memory sandbox like `openvaala`) or `ssh` (a remote machine).
- `description` — required. Shown to the model.
- `workspace_directory` — the executor's default root directory:
  - `local`: an existing absolute path, or one starting with `~`.
  - `virtual`: an absolute path inside the sandbox; default `/workspace`.
  - `ssh`: an absolute path on the remote machine.
- `ssh` — connection settings, only for `kind: ssh`:
  - `address` — `[user@]host[:port]`. The user defaults to your local user name and the port to 22.
  - `identity_file` — optional private key on the runtime server's machine. A relative path is resolved against the executor file's directory; `~` is expanded. The file must exist.

## SSH executors

Nothing needs to be installed on the remote machine beyond what most Linux servers already have:

- `sshd` with the SFTP subsystem enabled,
- `bash`, `base64` and `setsid`,
- a writable `/tmp`,
- Linux `/proc` for interactive commands, and `tar` if you use plugins whose hooks run there.

**Authentication** uses public keys only. OpenVaala offers, in order: `identity_file` if set, keys from your ssh-agent (`SSH_AUTH_SOCK`), and — only when `identity_file` is not set — `~/.ssh/id_ed25519`, `~/.ssh/id_ecdsa` and `~/.ssh/id_rsa`. Keys protected by a passphrase cannot be read from a file; add them to ssh-agent instead.

**Connections** open on first use and are kept alive. If one drops, the next tool call reconnects; a command that was running at that moment ends as failed. When an SSH executor is a conversation's default, it connects as soon as the conversation opens, because skills and project instructions are read from it.

> [!WARNING]
> Host keys are not verified. Use SSH executors only with hosts and networks you trust.

## Work location

Every conversation has its own work location:

- a **default executor** — where tools run unless the model picks another,
- a **working directory** on each executor — the root that relative paths resolve against,
- the **available executors** — all of them by default, including ones you add later, or a subset you choose.

By default the default executor is `local`. Its working directory comes from the client: the terminal UI and editors use the directory you started them in when the runtime server runs on the same machine, or its translation through [`client.path_mappings`](overview.md#client) when it runs elsewhere. Without either, it is `server.workspace_directory`, or the directory the runtime server was started in. Other executors start at their `workspace_directory`.

The default executor also decides where [project skills and instructions](extensions.md#skills) are read from.

You choose the work location when you start a conversation and can change it later:

| Client | How |
| --- | --- |
| Terminal UI | `/executor` chooses the default and the available executors; `/cd <dir>` changes the default executor's working directory. |
| Web UI, desktop app | The executor and directory pickers on a new conversation; **Conversation settings → Executor** afterwards. |
| Editors (ACP) | The `executor` session option switches the default executor. |

A change waits until nothing is running in the conversation. It restarts the conversation's agents, stops their background commands, and tells the model about the new work location on the next turn. `/cd` is not available for virtual executors.

## How tool calls pick an executor

File and command tools exist once per available executor. When a tool is offered by two or more executors, the model sees a single tool with an extra `executor` argument listing them, and omitting it uses the default executor:

```json
{"name": "bash", "arguments": {"executor": "buildbox", "command": "make test"}}
```

So within one turn, the agent can read a file on `local`, run the tests on `buildbox` and check a log on `staging`:

<!-- scene: unified-runtime | One conversation, tool calls on three machines -->
```text
          one conversation
   (agent loop in the runtime server)
                  │
     ┌────────────┼─────────────┐
     ▼            ▼             ▼
   local       buildbox       staging
 (this host)    (ssh)          (ssh)
 read_file    bash:          bash:
 src/app.go   make test      tail app.log
```

Subagents share the conversation's executors. To keep a conversation on one machine, make only that executor available.

## What is and isn't restricted

OpenVaala does not ask for approval before running a tool. Plan your setup accordingly:

- `apply_patch` only writes inside the executor's working directory.
- `read_file` can read any file the executor's user can read.
- `bash` runs a real shell on `local` and `ssh` executors. Only its starting directory is checked; the command can do anything the user can.
- Virtual executors are isolated from your disk and the network.

To add checks, use a `PreToolUse` [hook](extensions.md#hooks), which can block or rewrite any tool call. To keep an agent away from commands entirely, leave `bash` out of its [tools list](agents.md).
