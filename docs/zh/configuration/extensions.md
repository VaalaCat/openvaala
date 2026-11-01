# 扩展

这一页介绍怎么用 skill、项目说明、MCP server、hook 和插件来扩展 Agent。

| 你想要…… | 用 |
| --- | --- |
| 给 Agent 一些可复用的经验，需要时才加载 | [Skill](#skill) |
| 告诉 Agent 一个项目是怎么运作的 | [项目说明](#项目说明) |
| 从外部服务添加工具 | [MCP server](#mcp-server) |
| 在固定时刻运行你自己的脚本，比如拦下某个命令、格式化代码 | [Hook](#hook) |
| 一步装好一组 skill、hook 和 MCP server | [插件](#插件) |

改了这些文件之后，运行 `openvaala config check` 和 `openvaala serve reload`。见[让改动生效](overview.md#让改动生效)。

## Skill

skill 是一个带 `SKILL.md` 文件的目录。Agent 能看到每个 skill 的名字和描述，任务需要时再读完整的文件。

```markdown
---
name: release-notes
description: Use when drafting release notes from merged pull requests.
---
# Release notes

1. List merged pull requests since the last tag ...
```

- 文件必须以 YAML frontmatter 开头。
- `name` 用小写字母、数字和单个连字符（`release-notes`，不能写 `Release_Notes`）。默认是目录名。
- `description` 必填。写成*什么时候用*这个 skill，Agent 就是根据它来决定的。
- frontmatter 里的其他字段会被忽略。超过 1 MB 的文件不会读取。

### 从哪里找 skill

OpenVaala 按下面的顺序查找。两个 skill 同名时，先找到的那个生效。

1. 对话工作目录里的 `.agents/skills/`
2. 对话工作目录里的 `.claude/skills/`
3. 你的私有 skill 目录 `~/.config/openvaala/skills/`（`skills.directory`）
4. `openvaala` Executor 上 `/skills` 里的内置 skill
5. 已启用[插件](#插件)里的 skill，名字是 `<plugin>:<skill>`
6. 主目录里的 `~/.agents/skills/`
7. 主目录里的 `~/.claude/skills/`

这里的工作目录和主目录都是对话默认 Executor 上的，所以在 SSH Executor 上的对话会读取远端机器上的 skill。

目录会递归查找。一个目录里有了 `SKILL.md`，就不再往它的子目录里找。符号链接可以直接指向单个 skill 的目录。私有 skill 目录和内置 skill 里格式错误的 skill 会报错；其他位置的会被跳过。

### 直接使用 skill

在终端界面、Web 界面、桌面端或编辑器里输入 `/`，可以看到能用的 skill，选一个，比如 `/release-notes`。这个 skill 会附在你的消息上，Agent 从一开始就按它来做。插件里的 skill 显示为 `/<plugin>:<skill>`。也可以写 `/skill:<name>`：单写 `/<name>` 时，没有这个 skill 就当普通文字处理；而 `/skill:<name>` 在这个 Agent 用不了这个 skill 时会被拒绝。

### 内置的 OpenVaala skill

OpenVaala 自带一个内置 skill，`openvaala`，内容就是这份文档。问 Agent 怎么配置 OpenVaala，它能读对应的页面，帮你改配置。

skill 在对话打开或重新配置时查找。添加 skill 后，重新加载配置，或者开一个新对话。

## 项目说明

把项目约定（怎么构建、怎么跑测试、哪些地方别动）写进 `AGENTS.md` 文件。OpenVaala 会替你读取，不需要任何配置。

- 每一轮开始时，Agent 会收到默认 Executor 上从工作目录所在 Git 仓库的根目录到工作目录本身，沿途每一层的 `AGENTS.md`。不在 Git 仓库里时，只读工作目录的 `AGENTS.md`。
- Agent 读取更深层目录里的文件时，工作目录到这个文件之间的 `AGENTS.md` 也会加进来。其他 Executor 上的 `AGENTS.md`，在 Agent 第一次在那里用工具之后加进来。
- 越深的文件优先级越高；你直接提出的要求比它们都优先。
- 每个文件只进上下文一次，上下文压缩之后会重新加回来。

只读取名为 `AGENTS.md` 的文件；`CLAUDE.md` 和主目录里的文件都不读。对所有项目都适用的指导，写在 Agent 的 `instructions` 里。

## MCP server

`~/.config/openvaala/mcp_servers/` 里每个文件声明一个 [Model Context Protocol](https://modelcontextprotocol.io) server。它的工具在模型看来名字是 `mcp__<id>__<tool>`。

```yaml
# mcp_servers/serena.yaml：本机进程
id: serena
type: stdio
command: uvx
args: [serena, start-mcp-server]
executors: [local, devbox]   # 在这些 Executor 上运行
required: true               # 连不上时让这一轮失败
```

```yaml
# mcp_servers/context7.yaml：远端服务
id: context7
type: http
url: https://mcp.context7.com/mcp
headers:
  CONTEXT7_API_KEY: ...
```

字段：

- `id`：必填，和文件名一致。只能用字母、数字、`-` 和 `_`，不能包含 `__`。
- `type`：`stdio`、`http` 或 `sse`。
- `command`、`args`、`env`：用于 `stdio`。`command` 必填。`env` 叠加在继承的环境变量上。不能写 `url` 和 `headers`。
- `url`、`headers`：用于 `http` 和 `sse`。`url` 必须是 `http(s)` 地址。不能写 `command`、`args` 和 `env`。
- `executors`：在哪些 Executor 上运行；默认 `[local]`。每个 id 都必须是 `local`，或者 `executors/` 里不是 `virtual` 类型的 Executor。内置的 `openvaala` Executor 不能运行 MCP server。
- `tools`：提供给模型的工具名（用 server 自己的叫法）。默认全部提供。
- `required`：为 `true` 时，连不上会让这一轮失败。默认连不上就跳过这个 server 的工具。
- `disabled`：为 `true` 时保留文件，但不使用这个 server。
- `startup_timeout_seconds`：一次连接最多等多久；默认 30。
- `tool_timeout_seconds`：一次工具调用最多等多久；默认 300。

值都原样使用，不会展开 `${VAR}`。

**在哪里运行。** `stdio` server 在 Executor 所在的机器上、以 Executor 的工作目录启动。`http` 和 `sse` server 从 Executor 所在的机器去连接。所以列上一个 SSH Executor，server 就运行在那台远端主机上。

**哪些 Agent 能用。** Agent 的 `mcp_servers` 列表按 id 选 server；不写就能用全部 server，写 `[]` 就一个都不用。见 [Agent](agents.md)。

**编辑器里的 server。** 通过 ACP 连接时，编辑器里配置的 MCP server（stdio、http 或 sse）在这个对话里也能用。它们运行在编辑器所在的机器上，消息经过这条连接转发，所以编辑器断开后它们就没了。见[编辑器](../guides/editors.md)。

在终端界面用 `/mcp`，或者在 Web 界面 **对话设置** 的 **MCP** 标签页里查看连接状态。

## Hook

hook 在对话的固定时刻运行你的命令。hook 可以拦下工具调用、改写它的参数、给模型补充上下文，或者不让这一轮结束。

在 `~/.config/openvaala/hooks/` 里放任意多个 `*.yaml` 文件，它们全部生效。目录里的其他文件（比如脚本）不会当作配置读取。

```yaml
# hooks/go.yaml
hooks:
  PreToolUse:
    - matcher: bash
      hooks:
        - type: command
          script: guard.sh          # 相对这个 yaml 文件
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
          command: ~/bin/check-tests.sh   # 在目标机器上原样执行
          timeout: 600
```

### 事件

| 事件 | 什么时候运行 | `matcher` 和什么比较 |
| --- | --- | --- |
| `SessionStart` | 对话开始或恢复时 | `source`：`startup` 或 `resume` |
| `UserPromptSubmit` | 你发送消息时 | 无（总是匹配） |
| `PreToolUse` | 工具调用之前 | `tool_name` |
| `PostToolUse` | 工具调用之后 | `tool_name` |
| `Stop` | Agent 即将结束这一轮时 | 无（总是匹配） |
| `SubagentStart` | 子 Agent 启动时 | `agent_type` |
| `SubagentStop` | 子 Agent 即将结束时 | `agent_type` |

hook 在事件发生的那个 Executor 上运行，当前目录是这个 Executor 的工作目录。

### Matcher

- 为空、不写或 `*`：全部匹配。
- 只由字母、数字、`_`、`-` 和 `|` 组成：按准确的名字匹配，例如 `bash|apply_patch`。
- 其他写法：按 Go 正则表达式匹配，不加首尾锚点。

工具事件还认几个别名，这样为其他 Agent 写的 hook 也能接着用：`Bash` → `bash`，`Write` 和 `Edit` → `apply_patch`，`Read` → `read_file`，`Agent` → `spawn_agent`。

### Hook 字段

- `type`：必填；只支持 `command`。
- `command`：在目标机器上交给 `bash -c` 执行的命令行。它用到的程序必须在那台机器上存在。
- `script`：Runtime 服务所在机器上的脚本文件，可以是相对 YAML 文件的路径、绝对路径，或者以 `~/` 开头。加载配置时它必须存在，以 shebang 开头，并且有可执行权限。
- `command` 和 `script` 必须写一个，也只能写一个。
- `args`：跟在 `script` 后面的参数，逐个做 shell 转义。只能和 `script` 一起用。
- `timeout`：秒数；默认 60；必须是正数。
- `on_failure`：`allow`（默认）或 `block`，表示 hook 自己出错（超时、退出码不是 0 也不是 2、输出不合法）时怎么处理。只能用于 `PreToolUse` 和 `UserPromptSubmit`。

每次运行前，`script` 会被拷到目标机器的 `~/.cache/openvaala/hooks/<hash>/<file name>`，本机和 SSH Executor 都一样；只有内容或权限变了才会重新拷贝。只拷这一个文件：它需要的解释器和其他文件必须已经在目标机器上。改了脚本，下次运行就生效；改了 YAML，需要 `openvaala serve reload`。

### 输入和输出

hook 从 stdin 收到一个 JSON 对象，包含 `hook_event_name`、`session_id`（对话 id）、`turn_id` 和 `cwd`，再加上各事件自己的字段：

- `SessionStart`：`source`
- `UserPromptSubmit`：`prompt`
- `PreToolUse`：`tool_name`、`tool_input`、`tool_use_id`；`PostToolUse` 另有 `tool_response`
- `Stop`、`SubagentStop`：`stop_hook_active`、`last_assistant_message`
- 在子 Agent 里：`agent_id` 和 `agent_type`

hook 用退出码和输出来回答：

- **退出码 2**：拦下，stderr 是原因。`SessionStart` 和 `SubagentStart` 不能拦下，所以在这两个事件上退出码 2 算 hook 出错。
- **退出码 0，stdout 为空**：放行。stderr 只写进日志。
- **退出码 0，纯文本**：在 `SessionStart`、`SubagentStart` 和 `UserPromptSubmit` 上作为补充给模型的上下文，其他事件上忽略。
- **退出码 0，stdout 以 `{` 开头**：按 JSON 解析，不合法算 hook 出错。认识的字段：
  - `decision: "block"` 加 `reason`：拦下 `UserPromptSubmit`、`PostToolUse`、`Stop` 和 `SubagentStop`。
  - `hookSpecificOutput.permissionDecision: "deny"` 加 `permissionDecisionReason`：拦下一次 `PreToolUse` 调用。也接受 `allow`；`ask` 和 `defer` 算 hook 出错。
  - `hookSpecificOutput.updatedInput`：整个替换工具参数（只对 `PreToolUse` 有效）。
  - `hookSpecificOutput.additionalContext`：补充给模型的上下文。
  - `continue: false` 加 `stopReason`：结束这一轮。
  - `systemMessage`：显示给你看，不给模型。
- **其他退出码或超时**：hook 出错。

stdout 和 stderr 各自超过 64 KB 的部分会被截掉。比如一个 `PostToolUse` 格式化 hook 可以这样报告它做了什么：

```json
{"hookSpecificOutput": {"hookEventName": "PostToolUse", "additionalContext": "gofmt reformatted main.go"}}
```

Web 界面在 **设置 → Hook** 里列出已加载的 hook，并在对话里显示每次 hook 的运行情况。

## 插件

插件是一个目录，把 skill、hook 和 MCP server 打包在一起。可以从 git 或本机目录安装：

```bash
openvaala plugin install https://github.com/me/my-plugins.git --path plugins/review --ref v1.2.0
openvaala plugin list
openvaala plugin update            # 用 `plugin install` 装的所有插件
openvaala plugin disable review
openvaala plugin enable review
openvaala plugin uninstall review
```

- `--path` 指向仓库里的插件目录（默认是仓库根目录）；`--ref` 选分支或 tag（默认是默认分支，只能用于 git 来源）。从 git 安装需要 `git`。
- `plugin list` 每个插件一行：名字、版本、`enabled` 或 `disabled`，以及来源。
- 这些命令只改插件目录和 `plugins.disabled`，事先不需要配置能加载成功：一个让配置加载失败的插件，照样可以禁用、卸载或更新。改完之后它们会加载完整配置，加载失败就撤回改动。成功后会让本机 Runtime 服务重新加载。新插件的 skill 出现在之后新开或重新打开的对话里。
- 也可以自己把插件目录拷进或软链接到 `plugins/`，然后运行 `openvaala serve reload`。

### Manifest

原生插件用根目录下的 `.openvaala-plugin/plugin.yaml` 描述自己：

```yaml
name: review                     # 必填；必须和目录名一致
version: 1.2.0                   # 可选，显示在 plugin list 里
description: Code review helpers # 可选
skills: ./skills/                # 可选；这就是默认值
hooks:                           # 可选；写法和 hooks/*.yaml 相同
  SessionStart:
    - hooks:
        - type: command
          script: ./hooks/inject
          args: [SessionStart]
mcp_servers:                     # 可选；字段和 mcp_servers/<id>.yaml 相同，但不能写 executors
  - id: review-docs
    type: stdio
    command: node
    args: ["${OPENVAALA_PLUGIN_ROOT}/mcp/server.mjs"]
```

- **名字：** 小写字母、数字和单个连字符，必须和目录名一致。`skill` 是保留名。
- **skill：** 在 `skills` 下查找（相对插件根目录；目录不存在就没有 skill），名字是 `<plugin>:<skill>`，例如 `review:checklist`。在 Agent 的 `skills` 列表里和斜杠命令 `/review:checklist` 里都用这个完整名字。格式错误的插件 skill 会被跳过。
- **hook：** 事件、matcher 和字段都和 `hooks/*.yaml` 相同，但 `script` 必须是插件目录里的相对路径。`hooks/` 里的 hook 先运行，插件的 hook 按插件名排序在后。
- **MCP server：** 只在 `local` 上运行。如果 `mcp_servers/` 里有同 id 的文件，用那个文件，跳过插件里的这一项；想把 server 换到别的 Executor 上，或者加 API key，就用这个办法。两个已启用的插件声明同一个 id 会报错。只要某个 Agent 的 `mcp_servers` 列表还引用着插件里的 server，`plugin disable` 和 `plugin uninstall` 就会拒绝移除它。
- **`${OPENVAALA_PLUGIN_ROOT}`：** 在 stdio MCP server 的 `command`、`args` 和 `env` 里，它会换成插件的绝对目录，server 的环境变量里也会有它。hook 拿到的是环境变量，所以 `command: cat "${OPENVAALA_PLUGIN_ROOT}/core.md"` 能用。在 `local` 上它指向插件目录。在其他 Executor 上，整个插件目录会先拷到那台机器的 `~/.cache/openvaala/plugins/<name>-<hash>/`（那台机器需要有 `tar`），所以脚本能读到插件里的其他文件。
- `plugin install` 会在插件目录里写入 `.openvaala-install.yaml`（来源、ref、路径、commit），`plugin update` 按它更新。不要手动修改。

### Codex 和 Claude 插件

带 `.codex-plugin/plugin.json` 或 `.claude-plugin/plugin.json` 的插件可以原样安装。OpenVaala 只读取它们的名字、版本、描述和 `skills`（一个路径或一组路径，在默认的 `./skills/` 之后查找，每个路径可以是一个 skill 目录集合，也可以是单个 skill 的目录），不加载它们的 hook、MCP server 和其他组件。一个插件有多份 manifest 时，按 OpenVaala、Codex、Claude 的顺序用第一份。不会修改源 manifest。

`plugins/` 里没有任何 manifest 的目录，以及以 `.` 开头的名字，都会被跳过。manifest 写坏了会让配置加载失败：Runtime 服务启动不了，重新加载时则继续用旧配置。
