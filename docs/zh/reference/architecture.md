# 架构

这一页介绍 OpenVaala 是怎么搭起来的，写给贡献者，以及想知道一个对话背后发生了什么的人。

核心想法是把三样东西分开：**Agent 主循环**（调用模型、决定下一步、保存历史）、**工具执行**（读写文件、运行命令）和**界面**（你在哪里输入）。Agent 主循环在一处运行，工具在你指定的地方运行，任意多个界面都可以查看和引导同一份工作。

```text
  terminal UI   web UI   desktop   editor (ACP)   Codex
       │           │        │           │           │
       │           │        │      openvaala acp   openvaala app-server
       │           │        │           │           │
       └───────────┴────────┴─────┬─────┴───────────┘
                                  │  WebSocket, JSON-RPC
                                  │  /api/v1/openvaala
                     ┌────────────▼────────────┐
                     │   runtime server        │
                     │   (openvaala serve)     │
                     │                         │
                     │   conversations,        │
                     │   agent loop, history   │──── 模型 Provider
                     └────────────┬────────────┘
                                  │  工具调用
              ┌───────────────────┼────────────────────┐
              ▼                   ▼                    ▼
           local            ssh executors         virtual sandboxes
     (this machine)       (other machines)     (in memory, e.g. openvaala)
```

## 进程

**Runtime 服务**（`openvaala serve`）是唯一拥有对话的进程。对话打开期间它把对话放在内存里，调用模型 Provider，把工具调用发给 Executor，并把所有东西写到磁盘。同一时间只有一个 Runtime 服务使用一个数据目录，由一个锁文件保证。它在 `/api/v1/openvaala` 通过 WebSocket 提供 JSON-RPC API，用 token 保护；在 `/web/v1/` 以静态文件提供 Web 界面。

**客户端**连接它。终端界面、Web 界面和桌面端直接用它的协议。编辑器和 Codex 用各自的协议，所以 `openvaala acp` 和 `openvaala app-server` 作为小的适配进程运行，在 ACP 或 Codex app-server 协议和 Runtime 服务之间转换。同一台机器上的客户端通过 `~/.local/state/openvaala/runtime-server.yaml` 找到本机服务，服务没在运行就启动它。

因为所有状态都在 Runtime 服务里，客户端随时断开都不影响工作，每个客户端看到的都是同样的实时状态：正在生成的文字、待处理的输入、工具输出、子 Agent。

## 对话模型

<!-- scene: runtime-layers | 从 Runtime 一直到 Agent 主循环 -->
```text
Runtime              配置、已打开的对话
 └─ Conversation     工作位置、Executor、skill
     └─ Session      一个 Agent：模型、历史、输入
         └─ Turn     一次运行，由一条输入发起
             └─ Agent    模型  ⇄ 工具循环
                  │
                  ├─ 写入 Item：消息、推理、
                  │  工具调用和结果、摘要
                  └─ 把工具调用发给 Executor
```

从外到内：

- **Runtime**：Runtime 服务里的根对象。它持有当前配置（Agent、模型、Executor、MCP server、hook、skill）和已打开对话的表，把每个请求转给对应的对话。重新加载配置时，在这里替换定义，再交给空闲的对话。
- **Conversation**：你正在做的一个任务，也就是一个对话。它拥有自己的工作位置（Executor 和工作目录）、到这些 Executor 和 MCP server 的连接、可用的 skill，以及参与其中的所有 Agent：一个根 Session 和任意多个子 Agent 的 Session。
- **Session**：对话里一个 Agent 的工作线：用哪个 Agent、哪个模型，以及它的历史。一个 Session 同一时间只运行一个 Turn，并拥有等着它处理的输入队列，补充和排队就在这里决定。子 Agent 就是有父 Session 的 Session。
- **Turn**：Agent 主循环的一次运行，由一条输入发起。它加载历史、运行 Agent、保存产出，结束时的状态是完成、失败或中断，并记下 token 用量。
- **Agent**：Turn 里的循环本身：根据上下文组装请求，调用模型，运行模型要求的工具（模型一次要多个时就同时运行），把结果加进来，如此反复。每次调用模型之前，它会读取补充的输入，并判断上下文是否需要压缩。

Session 记录的每样东西都是一个 **Item**，按首次出现的顺序保存：消息（你的、模型的、Agent 之间的）、推理、工具调用和结果、模型错误、上下文摘要、模型和 Executor 的变更、skill 和 `AGENTS.md` 说明、hook 运行记录，以及中断标记。客户端渲染的是 Item；发给模型的上下文也由它们组装。

每个对话、对话里的每个 Session，都在自己的循环里按顺序处理事件，所以每份状态只有一个拥有者，不需要加锁。不同的对话和 Session 并行运行。

## Executor 和工具

**Executor** 是一个有文件系统和 shell 的环境：Runtime 服务自己所在的机器、通过 SSH 访问的机器，或者内存沙箱。Executor 分三层：

```text
ExecutorProvider   这个对话能用哪些 Executor
 └─ Executor       一个能干活的地方
     └─ Workspace  读写文件、运行命令
```

- **`ExecutorProvider`**：给出一个对话能用的 Executor 列表。`openvaala serve` 用的是由 `executors/` 下的文件建出来的那一份，所有对话看到的列表相同。把 OpenVaala 当 Go 库嵌入的程序可以提供自己的实现，比如每个对话给出不同的列表。
- **`Executor`**：说明一个地方是什么（ID、种类、说明、默认目录），并在上面打开工作区。新增一种 Executor 只需要写一个类型，对话不用知道它的细节。
- **`Workspace`**：工具真正打交道的对象。对话在它能用的每个 Executor 上打开自己的工作区，根目录是这个 Executor 的工作目录，对话停止时关闭，所以 SSH 连接和 MCP server 不会比对话活得更久。

重新加载配置时，Provider 和其他定义一起被替换：空闲的对话换用新的列表，新对话从一开始就用新的。

已保存的对话如果没人查看，也没有在运行的 Turn 或后台命令，超过 `server.conversation_idle_timeout`（默认 30 分钟）后会被停止，它的工作区随之释放；再次打开时重新连接。见[配置总览](../configuration/overview.md)。

工具分两类。**环境工具**（`read_file`、`apply_patch`、命令工具和 MCP 工具）由各个工作区提供；多个 Executor 提供同一个工具时，模型看到的是一个带 `executor` 参数的工具，调用会转给对应的工作区。**协作工具**（`spawn_agent` 等）作用于 Runtime 服务里的 Session。hook 在工具调用所在的 Executor 上、围绕工具调用运行。

后台命令属于启动它的 Session，所以它的生命比单次工具调用或单个 Turn 更长，随 Session 一起结束。MCP server 在配置它的 Executor 上启动，或者从那里连接；编辑器提供的 server 经 ACP 连接转发到编辑器所在的机器。

## 存储

存储层在 `pkg/storage` 里拆成两个小接口，上面的代码都面向这两个接口写，不绑定某个具体的数据库：

- **`LogStorage`**：只追加的 JSON 条目流，用路径和流 id 定位。`Append` 写入一批条目，返回前刷到磁盘，并在流内给每个条目分配一个从 1 开始的序号。可以带游标朝任一方向读取，可以逐个删除流，也可以按路径删除。`pkg/storage/file` 实现了它：每条流一个 JSON Lines 文件，每个路径一个目录。
- **`RecordStorage`**：按 namespace 分组、按键存取的 JSON 记录，每条记录带更新时间。它提供 `Set`、`MSet`、`Get`、`MGet`、`Delete` 和 `DeleteRange`，以及支持键范围、按键或更新时间排序、按声明过的字符串字段做简单筛选和游标分页的 `List`。`pkg/storage/sqlite` 实现了它，并在同一个数据库上也提供一个 `LogStorage`。

`pkg/dao` 在这之上构建对话、Session、Turn 和 Item。落到磁盘上是这样：

```text
pkg/dao   对话 · Session · Turn · Item
  │
  ├─ LogStorage     pkg/storage/file
  │    conversations/<conversation-id>/
  │      metadata.jsonl       对话、Session、Turn
  │      <session-id>.jsonl   一个 Session 的 Item
  │
  └─ RecordStorage  pkg/storage/sqlite
       metadata.db            用于列表和搜索的索引
```

日志是唯一的事实来源。`metadata.jsonl` 里每个条目都带着一个对话、Session 或 Turn 最新的完整状态，所以以最后一条为准；删除也是一个条目。每次改动都先追加并刷盘，再更新索引，`openvaala reindex` 随时可以从日志重建 `metadata.db`。对话能搬走就是因为这一点：一个对话目录本身就是完整的，可以拷贝、备份或迁移。见[对话存在哪里](../guides/conversations.md#对话存在哪里)。

<!-- scene: portable-conversations | 日志是唯一的事实来源，索引随时可以重建 -->
```text
追加      metadata.jsonl、<session-id>.jsonl ──▶ metadata.db
重建      metadata.jsonl ──▶ metadata.db，由 openvaala reindex 按日志重建
迁移      conversations/<conversation-id>/ ──▶ 另一台机器，再 reindex
```

新对话先通过 `StagedLogStorage` 和 `StagedRecordStorage` 写进一个内存里的 SQLite 数据库。收到第一条消息时，暂存的数据被拷到真正的存储，之后所有读写都切换过去；所以空对话不会在磁盘上留下任何东西。

同样的接口后面计划接入更多后端，比如键值存储、关系型和文档型数据库，以及兼容 S3 的对象存储。

## 配置

配置是一个 YAML 文件目录（见[配置](../configuration/overview.md)），作为一个整体加载和校验。有文件校验失败时，加载停止，错误里写明是哪个文件；重新加载失败时，保留之前的配置。Runtime 服务也会写配置（界面里的模型服务设置和默认模型、插件改动），做法是改文件、加载结果，加载失败就恢复原文件。

## 代码地图

| 路径 | 里面有什么 |
| --- | --- |
| `cmd/openvaala`、`cmd/openvaala-desktop` | CLI 和桌面端的入口 |
| `internal/cli` | 各个命令、启动和查找 Runtime 服务、ACP 适配 |
| `internal/config` | 加载和校验配置、插件 |
| `internal/tui` | 终端界面 |
| `internal/desktop` | 桌面端的原生界面和 WebView 窗口 |
| `internal/appserver` | Codex app-server 适配 |
| `internal/modelcatalog` | 添加 Provider 时用的 models.dev 目录 |
| `pkg/runtimeserver`、`pkg/runtimeprotocol` | WebSocket 服务和它的协议类型 |
| `pkg/runtime` | Runtime：定义和已打开的对话 |
| `pkg/conversation` | 对话、子 Agent、工作位置、协作工具 |
| `pkg/session`、`pkg/turn` | 输入队列和 Turn 的执行 |
| `pkg/agent`、`pkg/agent/agent_context` | Agent 主循环、上下文组装和压缩 |
| `pkg/executor`、`pkg/executor/tools` | 工具路由和内置工具 |
| `pkg/workspace`、`pkg/terminal` | 本机、SSH 和虚拟工作区；后台命令 |
| `pkg/mcpclient`、`pkg/hook`、`pkg/skill` | MCP 连接、hook、skill |
| `pkg/dao` | 建在存储之上的对话、Session、Turn 和 Item |
| `pkg/storage`、`pkg/storage/file`、`pkg/storage/sqlite` | `LogStorage` 和 `RecordStorage` 接口，以及 JSON Lines 和 SQLite 实现 |
| `pkg/telemetry` | 日志、trace 和指标 |
| `www` | Web 界面（React，用 Vite 构建） |
| `docs` | 这份文档，同时作为内置的 `openvaala` skill 打包进程序 |

## 开发

```bash
go test ./...
go test -race ./pkg/...
```

开发 Web 界面时运行 Vite 开发服务器，它把 `/api` 转发到 `VITE_SERVE_URL` 上的 Runtime 服务（默认 `http://127.0.0.1:8790`）：

```bash
pnpm --dir www install
pnpm --dir www dev
```

`pnpm --dir www generate:types` 根据 Go 的定义重新生成 TypeScript 协议类型。
