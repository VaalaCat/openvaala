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
