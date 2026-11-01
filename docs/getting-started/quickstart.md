# Quickstart

This page takes you from installing OpenVaala to a first conversation in the terminal and the same conversation in your browser, in about five minutes.

You need an API key for a model provider (OpenAI, Anthropic, or any service compatible with the OpenAI or Claude APIs), or a local model server.

## 1. Install

Download the `openvaala` binary for your system from [GitHub Releases](https://github.com/VaalaCat/openvaala/releases) and put it on your `PATH`, or see [Install](install.md) for the other options. Check it works:

```bash
openvaala --version
```

## 2. Add a model provider

Go to a project you want to work on and start the terminal UI:

```bash
cd ~/code/my-project
openvaala
```

The first time, OpenVaala starts its runtime server in the background and, because no model is configured yet, opens the provider setup:

1. **Choose a service.** Type to filter the catalog, or choose the custom entry for any compatible endpoint, such as a local model server.
2. **Check the details** — id, protocol and base URL are filled in for catalog services — and **enter your API key**.
3. **Choose the models** you want to use.
4. **Choose the default model.**

The provider is saved to `~/.config/openvaala/models/<id>.yaml`. You can change it later with `/provider`, or edit the file; see [Models](../configuration/models.md).

## 3. Start a conversation

Type a request and press Enter:

```text
Explain what this project does and how to run its tests. Then run them.
```

The agent reads files and runs commands in the current directory, and you see each tool call and its output as it happens. While it works you can type another message and press **Enter** to steer it, press **Tab** to queue a message for afterwards, or press **Esc** twice to interrupt.

> [!WARNING]
> OpenVaala runs tools without asking for confirmation, including shell commands. Start in a directory you are comfortable letting an agent work in, and see [Tools](../configuration/tools.md#approvals) for how to add checks.

Type `/` to see the commands, such as `/model` to switch models and `/resume` to reopen an earlier conversation. See [Terminal UI](../guides/terminal.md) for the rest.

## 4. Open the same conversation in the browser

In another terminal, run:

```bash
openvaala web
```

Your browser opens the web UI, and the conversation from the terminal is in the sidebar — still running, if the agent has not finished. Both clients are connected to the same runtime server, so you can send the next message from either.

Release binaries and `go install ...@latest` include the web UI. Builds from `@main` need a separate web UI build; see [Install](install.md).

## Where to next

- Use OpenVaala from the [desktop app](../guides/desktop.md), [your editor](../guides/editors.md) or [Codex](../guides/codex.md).
- Learn how [conversations](../guides/conversations.md) handle subagents, steering, forks and long contexts.
- Add [SSH machines and sandboxes](../configuration/executors.md) so one conversation can work across several machines.
- Write your own [agents](../configuration/agents.md) and add [skills, MCP servers and hooks](../configuration/extensions.md).
- Ask the agent itself: OpenVaala ships with its documentation as a built-in skill, so "How do I add an SSH executor?" works in any conversation.
