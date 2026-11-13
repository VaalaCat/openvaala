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
