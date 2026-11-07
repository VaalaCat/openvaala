# Agent

这一页介绍怎么定义 Agent：它的指令、模型、工具、skill 和 MCP server。

*Agent* 是一个有名字的角色。每个对话都从一个根 Agent 开始，根 Agent 可以把任务的一部分交给任何一个已配置 Agent 的子 Agent。不做任何配置也已经有一个：内置的 `default` Agent，一个通用助手，所有工具、skill 和 MCP server 都能用，用默认模型。

## 完整的 Agent 文件

`~/.config/openvaala/agents/` 里每个文件定义一个 Agent：

```yaml
# agents/reviewer.yaml
name: reviewer                       # 必须和文件名一致
description: Reviews a change for bugs and missing tests without editing files
instructions: |
  You review code changes. Read the diff and the surrounding code,
  run the tests, and report concrete problems with file and line.
  Do not modify files.
model:                               # 可选；默认用 models.default_model
  provider: openai
  model: gpt-5
compact_model:                       # 可选；写上下文摘要的模型
  provider: openai
  model: mini
tools: [read_file, bash, read_command_output, write_stdin, stop_command, list_commands]
skills: [code-review]                # 可选；默认全部 skill
mcp_servers: [serena]                # 可选；默认全部；[] 表示不用
max_steps: 50                        # 可选；默认 0，不限
```

## 字段

- `name`：必填，和文件名一致。可以用字母、数字、`-`、`_` 和 `.`。
- `description`：这个 Agent 是做什么的。其他 Agent 决定把子任务交给谁时会读它，所以写清楚它负责什么工作，而不是它的性格。
- `instructions`：必填。Agent 的系统指令。
- `model`：用哪个模型：`provider` 是 `models/` 里的 Provider id，`model` 是这个 Provider 里的模型 id（默认 `default`）。不写时用 `models.default_model`，见[选择模型](models.md#选择模型)。
- `compact_model`：上下文快满时总结对话的模型。默认和 `model` 相同。它的上下文窗口不能比主模型小。用一个更便宜、窗口大的模型很合适。
- `tools`：这个 Agent 能用的文件和命令工具。不写表示全部能用。见[限制 Agent 能用的工具](tools.md#限制-agent-能用的工具)。
- `skills`：这个 Agent 能用的 skill，按名字写（插件里的 skill 写成 `<plugin>:<skill>`）。不写表示全部能用。
- `mcp_servers`：这个 Agent 能用的 MCP server id。不写表示全部能用，包括编辑器提供的；写 `[]` 表示都不用。每个 id 都必须在 `mcp_servers/` 或某个插件里存在（disabled 的也算）。
- `max_steps`：一轮最多调用几次模型。默认 `0` 表示不限，不能为负数。到最后一次调用时，Agent 不能再用工具，模型会总结已经做完的和还剩下的。这一轮正常结束，你说继续就开始新的一轮。

每个模型引用都必须对应一个已配置的模型，否则配置加载失败。

## 自定义默认 Agent

`agents/default.yaml` 比较特殊：它从内置的 `default` Agent 出发，只替换你写了的字段。`name` 可以不写（写了就必须是 `default`），`instructions` 也是可选的。只想把默认 Agent 固定到一个模型，这样写就够了：

```yaml
# agents/default.yaml
model:
  provider: anthropic
  model: claude-sonnet
```

其他 Agent 文件都必须写 `name` 和 `instructions`。

如果一个模型都没配置，Agent 就没有模型，直到你[添加 Provider](models.md#不改文件添加-provider)。

## 用哪个 Agent

- 终端界面、编辑器（ACP）和 Codex 开始对话时用 `run.agent_name`（默认 `default`）；`--agent <name>` 可以为单个命令覆盖它。
- Web 界面和桌面端开始对话时用 `default`。
- 根 Agent 可以用 `spawn_agent` 启动任何一个已配置 Agent 的子 Agent。同一个 Agent 的子 Agent 继承当前模型；不同的 Agent 用它自己的默认模型，除非调用时指定了模型。见[子 Agent](../guides/conversations.md#子-agent)。

用 `openvaala --agent reviewer config check` 检查一个 Agent，再用 `openvaala serve reload` 让改动生效。
