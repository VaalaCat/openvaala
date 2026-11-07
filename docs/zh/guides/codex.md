# Codex

这一页介绍怎么把 Codex 终端界面或桌面版当作 OpenVaala 的前端来用。

OpenVaala 实现了 Codex app-server 协议。Codex 客户端连上它，就像连自己的后端一样，而对话、模型和工具都来自 OpenVaala。在 Codex 里开始的 thread 就是普通的 OpenVaala 对话：可以在终端界面、Web 界面或编辑器里接着用，也可以在 Codex 里恢复 OpenVaala 的对话。

这样用不需要登录 OpenAI 账号。模型来自你在 [OpenVaala 里配置的 Provider](../configuration/models.md)。

## Codex 终端界面

先装好 Codex CLI，让 `codex` 在 `PATH` 里，然后在要工作的目录里运行：

```bash
openvaala codex
```

OpenVaala 会启动一个私有的 app-server，用当前目录启动 `codex --remote` 连上它，并连接 Runtime 服务（没在运行就启动）。`--` 之后的参数原样交给 Codex：

```bash
openvaala codex -- resume --last
```

Codex 的选项 `-c`/`--config key=value`、`--enable`、`--disable` 和 `--strict-config` 也会交给 Codex，只要它们写在 `--` 之前，位置不限。OpenVaala 自己的配置文件用 `--config-file` 指定。

Codex 退出时，私有的 app-server 也跟着退出，所以 Codex 显示给你的 `codex --remote` 命令会失效。之后要继续某个 thread，运行 `openvaala codex -- resume <session id>`（或者 `-- resume --last`）；正常退出后 OpenVaala 会提醒你。

## Codex 桌面版（macOS）

装好 Codex 桌面版后，先完全退出它，再运行：

```bash
openvaala codex app              # 当前目录
openvaala codex app ~/project
```

应用会以 OpenVaala 为后端启动，在那个目录里打开一个新 thread，命令随即返回。你的 Codex 配置不会被修改，也不会自动安装或关闭应用。OpenVaala 的配置参数和 `OPENVAALA_*` 环境变量会传给应用的后端。

## openvaala app-server

`openvaala app-server` 可以直接替换 `codex app-server`。任何启动 `codex app-server ...` 的工具，都可以改成启动 `openvaala app-server ...`；OpenVaala 用不到的选项照收但会忽略。

```bash
openvaala app-server                              # stdio（默认）
openvaala app-server --listen unix://             # ~/.local/state/openvaala/app-server.sock
openvaala app-server --listen unix:///tmp/ov.sock
openvaala app-server --listen ws://127.0.0.1:4500
```

监听 socket 和 WebSocket 时，它会打印用来连接的 `codex --remote <url>` 命令。WebSocket 只接受回环 IP 地址，也没有认证，所以其他机器连不上；WebSocket 认证选项（`--ws-auth` 等）会被拒绝。要连另一台机器上的 Runtime 服务，改用 `--url` 或 `client.url`，app-server 会用那台服务器的 token 连接。见[连接 Runtime 服务](../reference/cli.md#连接-runtime-服务)。

## 哪些能对应，哪些不能

- **Thread** 就是对话；每个子 Agent 显示为单独的 thread。开始、恢复、分叉、归档、改名、删除、压缩、补充和中断都能用，`/btw`、`/side` 旁支对话也能用。
- Codex 里列出的**模型**就是你的 OpenVaala 模型（`provider/model`），带着各自的推理强度。在 Codex 里选的模型或强度只对这条连接生效，不会写进你的配置。
- **审批和沙箱模式**会被忽略。OpenVaala 运行工具不征求确认，并把沙箱报告为完全访问；见[审批](../configuration/tools.md#审批)。
- **图片**可以用 data URL 或本地图片文件附加。
- **不支持：** 动态工具、Codex 插件和应用，以及权限配置档。
- **路径：** 配置了 [`client.path_mappings`](../configuration/overview.md#client) 时，thread 的工作目录会转换；命令和文件改动 Item 里的路径按服务器上的原样显示。

用的 Agent 是 `run.agent_name`（或 `--agent`）；它的工具、skill 和 MCP server 在 OpenVaala 里配置，不在 Codex 里。
