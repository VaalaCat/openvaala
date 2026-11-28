---
name: openvaala
description: 用户问 OpenVaala 本身怎么工作、怎么安装、运行或接入（终端界面、Web 界面、桌面端、ACP 编辑器、Codex），或者要查看、修改 OpenVaala 的配置（config.yaml、模型 Provider 和模型、Agent、Executor、工具、skill、MCP server、hooks、插件、遥测）时使用。
---
# OpenVaala 文档

这个 skill 就是 OpenVaala 的使用文档，各页和本文件放在同一目录；在 `openvaala` Executor 上位于 `/skills/openvaala/`。

**语言：** 跟着用户的语言走。用户说中文多，读 `zh/` 下的中文页（路径相同，例如 `zh/configuration/models.md`）；说英文多，读根目录下的英文页。某个说法不清楚，或者怀疑一边没跟上更新时，两边对照着看。

## 什么问题读哪一页

- OpenVaala 是什么、各部分怎么配合：`index.md`、`reference/architecture.md`
- 安装、从源码构建：`getting-started/install.md`
- 第一次运行、添加模型 Provider：`getting-started/quickstart.md`、`configuration/models.md`
- 终端界面的命令和按键：`guides/terminal.md`
- Web 界面、远程访问、HTTPS 反向代理、手机上使用：`guides/web.md`
- 桌面端（WebView、原生界面）：`guides/desktop.md`
- Zed 等 ACP 编辑器：`guides/editors.md`
- Codex 终端界面、Codex 桌面版、app-server：`guides/codex.md`
- 子 Agent、插队和排队、分叉与回退、上下文压缩、对话存储、迁移对话：`guides/conversations.md`
- 配置目录、`config.yaml` 每个字段、环境变量、重新加载：`configuration/overview.md`
- Provider、协议、模型字段、推理设置：`configuration/models.md`
- Agent 定义文件、默认 Agent：`configuration/agents.md`
- 本机、SSH、虚拟 Executor，工作位置：`configuration/executors.md`
- 内置工具、限制可用工具：`configuration/tools.md`
- skill、AGENTS.md、MCP server、hooks、插件：`configuration/extensions.md`
- 日志、OTLP 的 trace 和指标：`configuration/telemetry.md`
- 所有命令和参数：`reference/cli.md`

## 修改配置

1. 改之前先读对应的配置页。配置文件按严格模式解析，写了不认识的字段会报错。
2. 改配置目录里的文件。默认目录是 `~/.config/openvaala/`，在运行 Runtime 服务的那台机器上，也就是 `local` Executor 所在的机器。每个文件里的 `id` 或 `name` 要和文件名一致。
3. 运行 `openvaala config check`，把报出的错误改掉。
4. 运行 `openvaala serve reload` 让改动生效。改了 `server.listen`、`server.token`、`tools.max_image_kb` 或 `telemetry` 时，请用户自己运行 `openvaala serve stop` 再重新启动：停掉服务会连同当前对话一起停掉。

配置文件和 `openvaala config show` 的输出里有 API key 和 token，不要原样复述给用户。

## 从外部 Agent 协作

OpenVaala 启动外部 Agent 时提供 `OPENVAALA_URL`、`OPENVAALA_TOKEN`、`OPENVAALA_CONVERSATION_ID` 和 `OPENVAALA_SESSION_ID`。在外部 Agent 运行的机器上使用 `openvaala` CLI，连接和会话身份默认取这些环境变量。CLI 与本地 openvaala MCP 工具提供等价的协作能力，完整参数见 `reference/cli.md` 或 `zh/reference/cli.md`。在 Codex 等带网络沙箱的 Agent 中，如果 CLI 连不上 OpenVaala，请使用 `openvaala` MCP 工具。

- 派发子任务用 `openvaala session spawn --agent <name> "<prompt>"`。可用 `--executor <id>`、`--cwd <path>`、`--task <name>` 和 `--label k=v` 指定工作位置及任务信息。要换模型，用 CLI 的 `--model <provider/model>` 或 MCP 的 `model` 参数。
- 向会话发送消息用 `openvaala session send <session-id> "<prompt>"`，或用 `openvaala session send <session-id> --prompt-file <path>` 从文件读取正文。要中断当前轮再发时加 `--interrupt`。`spawn` 和 `send` 默认等待本轮结束，可加 `--no-wait` 提交后立即返回。
- 查看后代用 `openvaala session ls`，可加 `--label k=v` 或 `--all`。查看详情、日志和实时输出分别用 `openvaala session inspect <session-id>`、`openvaala session logs <session-id> --limit <n>` 和 `openvaala session attach <session-id>`。
- 等待、停止和重新加载会话分别用 `openvaala session wait <session-id> --timeout <sec>`、`openvaala session stop <session-id>` 和 `openvaala session reload <session-id>`。读取选项用 `openvaala session option <session-id>`，修改时加 `--set kind:id=value`。
- 列出 Executor 用 `openvaala executor ls`。运行一次命令用 `openvaala exec --executor <id> --cwd <path> -- <command> <args...>`，可用 `--conversation <id>` 指定对话。读取 `/skills/openvaala/` 下的内置文档时，选择 `openvaala` Executor，或直接运行 `openvaala skill get openvaala <file>`。操作项目文件、运行项目命令时，使用 `executor ls` 标记为 `(default)` 的对话工作 Executor。
- 管理交互终端用 `openvaala terminal ls`、`openvaala terminal open` 和 `openvaala terminal close`。向已打开的终端发送按键用 `openvaala terminal send`，读取终端输出用 `openvaala terminal capture`。
- 用 openvaala MCP 的 `get_skill` 工具读取 `openvaala` 可查看本文（或运行 `openvaala skill get openvaala`）。CLI 列出、搜索和读取 skill 分别用 `openvaala skill list`、`openvaala skill search <词>` 和 `openvaala skill get <name> [file]`。
