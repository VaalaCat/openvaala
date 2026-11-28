# CLI

这一页列出 `openvaala` 的所有命令和参数。

```text
openvaala [flags]                  启动终端界面
openvaala tui                      启动终端界面
openvaala serve                    运行 Runtime 服务
openvaala serve stop               停止本机 Runtime 服务
openvaala serve reload             让本机 Runtime 服务重新加载配置
openvaala web                      打开 Web 界面
openvaala acp                      通过 stdio 以 ACP 协议服务一个编辑器
openvaala app-server               以 Codex app-server 协议服务 Codex 客户端
openvaala codex [-- CODEX_ARGS]    打开连着 OpenVaala 的 Codex 终端界面
openvaala codex app [directory]    打开连着 OpenVaala 的 Codex 桌面版（macOS）
openvaala session                  管理会话
openvaala agent                    列举 Agent 和选项
openvaala executor                 列举 Executor 和运行工具
openvaala exec                     在 Executor 上运行命令
openvaala terminal                 管理交互终端
openvaala skill                    列举、搜索及读取 skill
openvaala mcp-bridge               为 Agent 进程转发 stdio MCP，通常由 Agent 自动调用
openvaala reindex                  重建对话索引
openvaala plugin <subcommand>      安装和管理插件
openvaala config <subcommand>      初始化、查看和校验配置
openvaala completion <shell>       生成 shell 补全脚本
openvaala help [command]           显示帮助
```

在终端里运行 `openvaala <command> --help` 可以看到同样的信息。

## 全局参数

这些参数对每个命令都有效，写在命令名前后都行：

- `--config-file <path>`：配置文件。默认先看 `OPENVAALA_CONFIG`，再用 `~/.config/openvaala/config.yaml`。
- `--data-dir <dir>`：覆盖本机数据与运行状态目录。
- `--agents-dir <dir>`：Agent 定义目录，覆盖 `agents.directory`。
- `--models-dir <dir>`：Provider 定义目录，覆盖 `models.directory`。
- `--agent <name>`：根 Agent，覆盖 `run.agent_name`。
- `-h`、`--help`：显示帮助。
- `-v`、`--version`：打印版本（只在根命令上）。

客户端替你启动本机 Runtime 服务时，会把这些参数传过去，所以服务用的是同一份配置。

## 连接 Runtime 服务

`tui`（以及不带命令的 `openvaala`）、`web`、`acp`、`app-server`、`codex` 和 `codex app` 都接受：

- `--url <ws-url>`：要连接的 Runtime 服务，例如 `ws://10.0.0.5:8790/api/v1/openvaala`。

没有 `--url` 时用 `client.url`；也没有时用本机的 Runtime 服务，没在运行就在后台启动它。由 `tui`、`acp`、`app-server` 或 `codex` 这样启动的服务，在最后一个客户端断开、也没有任务在运行之后，大约十秒自动退出。由 `web` 或桌面端启动的服务会一直运行，直到你停掉它。token 和路径映射见[配置 → client](../configuration/overview.md#client)。

`session`、`agent`、`executor`、`exec`、`terminal` 和 `skill` 也接受 `--url` 和 `--token`。省略参数时分别读取 `OPENVAALA_URL` 和 `OPENVAALA_TOKEN`。`OPENVAALA_CONVERSATION_ID` 和 `OPENVAALA_SESSION_ID` 提供协作上下文。支持 `--conversation` 的命令可以用它覆盖环境变量中的对话 ID。

## tui

```bash
openvaala [--url <ws-url>]
openvaala tui [--url <ws-url>]
```

在当前目录启动终端界面。见[终端界面](../guides/terminal.md)。

## serve

```bash
openvaala serve
```

在前台运行 Runtime 服务：它拥有所有对话，在 `server.listen` 上提供 `/api/v1/openvaala` 的 WebSocket API 和 `/web/v1/` 的 Web 界面。同一时间只能有一个 Runtime 服务使用数据目录。它会在终端里打印地址和日志；用 Ctrl+C 或 `openvaala serve stop` 停止它。

用 `openvaala serve --listen <host:port>` 为本次启动覆盖 `OPENVAALA_SERVER_LISTEN` 和 `server.listen`。

### serve stop

```bash
openvaala serve stop
```

让本机 Runtime 服务停止，最多等 10 秒。打印 `stopped`；没有服务在运行时打印 `no runtime server is running`。还在运行的 Turn 会跟着停下。

### serve reload

```bash
openvaala serve reload
```

让本机 Runtime 服务重新加载 Agent、模型、Executor、MCP server、hook、skill 和插件。打印 `configuration reloaded`，并列出已接收新配置、等待空闲后自动应用的对话，无需再执行一次。见[让改动生效](../configuration/overview.md#让改动生效)。

## web

```bash
openvaala web [--url <ws-url>]
```

打印 Web 界面的地址（带访问 token），并尝试用浏览器打开。本机 Runtime 服务监听全部网卡时，还会为本机的每个 IPv4 地址打印一行 `Network:` 地址。见 [Web 界面](../guides/web.md)。

## acp

```bash
openvaala acp [--url <ws-url>]
```

通过 stdin 和 stdout，以 [Agent Client Protocol](https://agentclientprotocol.com) 服务一个编辑器。这个命令由编辑器启动，你不用手动运行。日志写到 stderr。见[编辑器](../guides/editors.md)。

## app-server

```bash
openvaala app-server [--listen <url>] [--stdio] [--url <ws-url>]
```

可以直接替换 `codex app-server`：启动 `codex app-server ...` 的工具，都可以改成启动 `openvaala app-server ...`。

- `--listen <url>`：监听在哪里：
  - `stdio://`（默认）：stdin 和 stdout。
  - `unix://`：位于 `~/.local/state/openvaala/app-server.sock` 的 Unix socket。
  - `unix://PATH`：位于 `PATH` 的 Unix socket。
  - `ws://IP:PORT`：回环 IP 地址（例如 `127.0.0.1`）上的 WebSocket（主机名和其他地址会被拒绝）。没有认证。
- `--stdio`：等同于 `--listen stdio://`。

监听 socket 和 WebSocket 时，它会打印对应的 `codex --remote <url>` 命令。OpenVaala 用不到的其他 `codex app-server` 选项照收但会忽略；WebSocket 认证选项（`--ws-*`）会被拒绝。见 [Codex](../guides/codex.md)。

## codex

```bash
openvaala codex [-- CODEX_ARGS...]
openvaala codex -- resume --last
```

在当前目录启动连着 OpenVaala 的 Codex CLI 终端界面。需要 `codex` 在 `PATH` 里。`--` 之后的参数交给 `codex`。Codex 的选项 `-c`/`--config key=value`、`--enable`、`--disable` 和 `--strict-config` 也会交给 `codex`，只要写在 `--` 之前，位置不限。（OpenVaala 自己的配置用 `--config-file`。）

### codex app

```bash
openvaala codex app [directory]
```

只支持 macOS。在 `directory`（默认当前目录）里，以 OpenVaala 为后端打开已安装的 Codex 桌面版，然后返回。请先退出 Codex 应用。不会修改任何 Codex 配置，也不会替你安装应用。

## session

会话 CLI 与本地 `openvaala` MCP 工具提供等价的协作能力。使用注入的会话 Token 时，两种方式采用相同的对话和会话权限。在 Codex 等带网络沙箱的 Agent 中，如果 CLI 连不上 OpenVaala，请使用 `openvaala` MCP 工具。没有来源会话时，操作者可用 `spawn` 创建独立对话。

- `openvaala session spawn <prompt>` 对应 `spawn_agent`。用 `--agent <name>` 选 Agent，`--model <provider/model>` 选模型，`--reasoning-effort <value>` 指定原生模型的推理强度。可重复传 `--initial-option <kind:id=value>` 设置外部 Agent 的启动选项。用 `--executor <id>`、`--cwd <path>`、`--task <name>`、`--label <key=value>`、`--background` 设置工作位置和会话信息。派生子会话时，`--fork-turns <none|all|n>` 选择继承的已结束轮次，`--notify-on-finish=false` 关闭完成回报。
- `openvaala session send <session-id> <prompt>` 对应 `send_message`。用 `--prompt-file <path>` 可改从本机文件读取正文。`--mode <normal|steer>` 选择排队开轮或补充活动轮，`--interrupt` 中断活动轮后再发送。`--model <provider/model>` 和 `--reasoning-effort <value>` 在发送前更新后代的模型选择。
- `spawn` 和 `send` 默认等到本轮结束并打印最终正文。加 `--no-wait` 时，输入一被接收就打印会话和输入 ID。
- `openvaala session ls [--conversation <id>]` 对应 `list_agents`。用 `--session-id <id>`、`--status <idle,running,stopping,stopped>`、`--cwd <path>`（`*` 表示所有目录）、`--since-hours <n>`（默认 24，0 表示不限）或 `--label <key=value>` 筛选。用 `--all` 包括后台会话，`--limit <n>` 设置条数。
- `openvaala session logs <session-id> [--limit <n>] [--cursor <n>]` 对应 `read_session_history`。把输出的 `next_cursor` 传给 `--cursor` 可读更早一页。有来源会话时，只查看后代在分叉之后产生的历史。
- `openvaala session wait [session-id] [--timeout <秒数>] [--wait-for subagent_result]` 对应 `wait_agent`。省略 ID 时等待来源会话。指定 `subagent_result` 后，如果没有未读消息也没有运行中的后代，会立即返回。
- `openvaala session stop <session-id>` 对应 `interrupt_agent`，请求取消活动轮。
- `openvaala session option <session-id> [--set <kind:id=value>]` 查看或修改外部选项，对应 `get_agent_options`、`set_agent_option`。`openvaala session reload <session-id>` 重启外部驱动，对应 `reload_agent`。
- `openvaala session inspect <session-id>` 看详情，`session attach <session-id>` 跟随实时输出，`session open <session-id>` 在 Web 界面打开会话。
- `openvaala session update <session-id> [--task-name <name>] [--label <key=value>] [--background=<bool>]` 更新会话信息。`session detach <session-id>` 将外部子会话转为独立对话。
- `openvaala session import --agent <name> [--executor <id>] [--cwd <path>]` 列出外部会话。用 `--query <文本>` 搜索标题和目录，用 `--cursor <游标>` 读取下一页。传入外部会话 ID 时导入该会话。

## agent

- `openvaala agent ls` 列出已配置的 Agent，对应 `list_agent_definitions`。
- `openvaala agent options <agent-name>` 查看定义支持的外部选项，对应 `get_agent_options`。

## executor 与 exec

- `openvaala executor ls [--conversation <id>] [--refresh-tools]` 对应 `list_executors`，列出 Executor 并给对话的默认工作 Executor 标上 `(default)`。刷新工具表只对原生模型的活动轮生效。
- `openvaala exec --executor <id> [--conversation <id>] [--cwd <path>] -- <command> [args...]` 运行一次命令并打印输出。相对工作目录从所选 Executor 的工作区根目录解析。
- `openvaala executor tool <name> [arguments-json] [--conversation <id>]` 调用本地 MCP 暴露的同一套工作区和命令工具，使用相同的 JSON 参数和默认值。例如 `openvaala executor tool read_file '{"path":"README.md","offset":1,"limit":20}'`。工具是否可用取决于来源会话及其 Executor。

## terminal

- `openvaala terminal ls` 列出运行中的终端，对应 `terminal_list`。
- `openvaala terminal open [--executor <id>] [--cwd <path>] [--rows <n>] [--cols <n>]` 打开终端并打印 ID，对应 `terminal_open`。
- `openvaala terminal send <terminal-id> <keys>` 原样发送按键文本，对应 `terminal_send_keys`。提交命令时要包含换行。
- `openvaala terminal capture <terminal-id> [--lines <n>]` 输出最近若干行并去除 ANSI 控制序列，对应 `terminal_capture`。
- `openvaala terminal close <terminal-id>` 关闭终端，对应 `terminal_close`。

## skill

- `openvaala skill list [--agent <name>]` 列出可用 skill，对应 `list_skills`。
- `openvaala skill search <query> [--agent <name>]` 按名称和描述搜索，对应 `search_skills`。
- `openvaala skill get <name> [file] [--agent <name>]` 读取 `SKILL.md` 或该 skill 目录内的相对路径，对应 `get_skill`。例如 `openvaala skill get openvaala zh/reference/cli.md`。

操作者可以用 `--agent` 选择可用 skill 的范围。会话 Token 始终使用调用方 Agent 配置的 skill 和内置的 `openvaala` skill。

## reindex

```bash
openvaala reindex
```

从每个对话的 `metadata.jsonl` 重建 `~/.local/share/openvaala/metadata.db`。拷入对话目录后，在没有其他 OpenVaala 进程使用数据目录时运行（先运行 `openvaala serve stop`）。见[对话](../guides/conversations.md#把对话迁移到另一台机器)。

## plugin

```bash
openvaala plugin install <source> [--path <dir>] [--ref <branch-or-tag>]
openvaala plugin update [name...]
openvaala plugin uninstall <name>
openvaala plugin enable <name>
openvaala plugin disable <name>
openvaala plugin list
```

- `install`：从 git 地址或本机目录安装。`--path` 是仓库里的插件目录（默认是根目录）；`--ref` 是 git 分支或 tag（默认是默认分支）。
- `update`：重新拉取用 `plugin install` 装的插件；不写名字时更新全部。
- `uninstall`：删除插件目录。
- `enable`、`disable`：修改 `plugins.disabled`，不删除文件。
- `list`：每个插件一行：名字、版本、状态和来源。

每次改动后，如果有本机 Runtime 服务在运行，会让它重新加载。见[插件](../configuration/extensions.md#插件)。

## config

```bash
openvaala config init
openvaala config show
openvaala config check
```

- `init`：缺少时创建 `config.yaml` 以及 `agents/`、`models/` 和 `plugins/` 目录，不覆盖任何东西，然后打印路径。
- `show`：以 YAML 打印实际生效的设置，包括默认值和展开后的路径。token 会原样打印。
- `check`：加载所有定义，检查选中的 Agent，打印 `configuration valid: agent=… provider=… model=…`。不会连接模型 Provider。

见[配置](../configuration/overview.md#检查配置)。

## 桌面端命令

桌面端的可执行文件包含上面完整的 CLI，另外还有：

```bash
openvaala                 # 打开桌面窗口（WebView）
openvaala app             # 同上
openvaala app --native    # 改为打开原生界面
openvaala --native        # 同上
```

安装包里的这个可执行文件叫 `openvaala`（macOS 上是 `OpenVaala`）。`go install github.com/VaalaCat/openvaala/cmd/openvaala-desktop@latest` 装上的是同一套命令，程序名叫 `openvaala-desktop`。见[安装](../getting-started/install.md)。

在桌面端版本里，`tui` 启动终端界面。桌面窗口不接受 `--url`，它总是用本机的 Runtime 服务。见[桌面端](../guides/desktop.md)。

## 环境变量

- `OPENVAALA_CONFIG`：配置文件，作用和 `--config-file` 相同。
- `OPENVAALA_<GROUP>_<KEY>`：覆盖 `config.yaml` 里的某个设置，例如 `OPENVAALA_SERVER_LISTEN` 或 `OPENVAALA_CLIENT_URL`。见[环境变量](../configuration/overview.md#环境变量)。
