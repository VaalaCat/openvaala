# Executor

这一页介绍 Agent 的文件和命令工具在哪里运行，以及怎么添加其他机器和沙箱。

*Executor* 是一个有自己文件系统和 shell 的环境。Agent 主循环和对话历史始终留在 Runtime 服务里；只有工具调用（读文件、改文件、跑命令、启动 MCP server）发生在 Executor 上。一个对话可以用好几个 Executor，所以同一个任务可以在你的笔记本上读代码、在构建机上编译、在预发布服务器上查日志。

Executor 是单独的一层，以后加入新的执行环境时，Agent 的工作方式不用变。目前有三种：Runtime 服务所在机器上的目录（`local`）、通过 SSH 访问的远端机器（`ssh`），以及内存沙箱（`virtual`）。对其他机器上的 worker 进程和托管沙箱的支持在计划中。

## 内置 Executor

不做任何配置就有三个 Executor：

- **`local`**：Runtime 服务运行的那台机器。它的工作目录按对话选择，见[工作位置](#工作位置)。
- **`openvaala`**：内置的内存沙箱，根目录是 `/workspace`。它写的任何东西都不会落到你的磁盘上，对话重新打开或重新配置时内容会被重置。内置的文档 skill 也放在它的 `/skills/openvaala/` 下。它的 shell 是一个内嵌的 Bash 解释器，带常用的文件和文本工具（`cat`、`grep`、`sed`、`awk`、`jq`、`find`、`tar` 等）。它不能运行外部程序，不能联网，运行脚本要用 `bash <file>`，不能用 `./script`。

- **`memory`**：存在 Runtime 服务所在机器上的跨对话[记忆](extensions.md#记忆)。它只能读写文件，所以没有 `bash`，也不跑 hook。它没有工作目录，不能当默认 Executor。想在某个对话里关掉记忆，到这个对话的可用 Executor 里取消勾选 `memory`。

## 添加 Executor

`~/.config/openvaala/executors/` 里每个文件添加一个 Executor。想加多少都行，什么类型都可以：

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

模型根据 `description` 选择 Executor，所以要写清楚每个 Executor 是干什么用的。

### 字段

- `id`：必填，和文件名一致。`local`、`client`、`openvaala` 和 `memory` 是保留的。
- `kind`：`local`（Runtime 服务所在机器上的一个目录）、`virtual`（和 `openvaala` 一样的内存沙箱）或 `ssh`（远端机器）。
- `description`：必填，会显示给模型。
- `workspace_directory`：Executor 默认的根目录：
  - `local`：已存在的绝对路径，或者以 `~` 开头。
  - `virtual`：沙箱里的绝对路径；默认 `/workspace`。文件只在内存里：重启 serve、重新加载配置和对话空闲关闭（见 `server.conversation_idle_timeout`）都会清空它们。
  - `ssh`：远端机器上的绝对路径。
- `ssh`：连接设置，只用于 `kind: ssh`：
  - `address`：`[user@]host[:port]`。用户名默认是你本机的用户名，端口默认 22。
  - `identity_file`：可选，Runtime 服务所在机器上的私钥。相对路径以这个 Executor 文件所在目录为基准；会展开 `~`。文件必须存在。

## SSH Executor

远端机器上不需要额外安装东西，大多数 Linux 服务器自带的就够了：

- 开启了 SFTP 子系统的 `sshd`，
- `bash`、`base64` 和 `setsid`，
- 可写的 `/tmp`，
- 交互式命令需要 Linux 的 `/proc`；如果插件的 hook 要在那台机器上运行，还需要 `tar`。

**认证**只用公钥。OpenVaala 依次尝试：设置了 `identity_file` 就用它，然后是 ssh-agent（`SSH_AUTH_SOCK`）里的密钥，最后（只在没设置 `identity_file` 时）是 `~/.ssh/id_ed25519`、`~/.ssh/id_ecdsa` 和 `~/.ssh/id_rsa`。带口令的私钥没法直接从文件读取，请先加进 ssh-agent。

**连接**在第一次使用时建立，之后保持连接。连接断开后，下一次工具调用会重新连上；断开那一刻正在运行的命令会以失败结束。如果 SSH Executor 是对话的默认 Executor，对话一打开就会连接，因为要从它上面读取 skill 和项目说明。

> [!WARNING]
> 不校验主机密钥。只对你信任的主机和网络使用 SSH Executor。

## 工作位置

每个对话都有自己的工作位置：

- 一个**默认 Executor**：模型没有另选时，工具在这里运行，
- 每个 Executor 上的**工作目录**：相对路径以它为基准，
- **可用的 Executor**：默认是全部（包括以后添加的），也可以只选其中一部分。

默认 Executor 默认是 `local`。它的工作目录来自客户端：Runtime 服务在同一台机器上时，终端界面和编辑器用你启动它们的目录；在别的机器上时，用这个目录经 [`client.path_mappings`](overview.md#client) 转换后的路径。两者都没有时，用 `server.workspace_directory`，或者 Runtime 服务的启动目录。其他 Executor 从各自的 `workspace_directory` 开始。

[项目 skill 和项目说明](extensions.md#skill)也从默认 Executor 上读取。

开始对话时选择工作位置，之后也可以修改：

| 客户端 | 怎么做 |
| --- | --- |
| 终端界面 | `/executor` 选默认 Executor 和可用的 Executor；`/cd <dir>` 修改默认 Executor 的工作目录。 |
| Web 界面、桌面端 | 新对话页面上的 Executor 和工作目录选择；之后在 **对话设置 → Executor** 里改。 |
| 编辑器（ACP） | 会话选项 `executor` 切换默认 Executor。 |

修改会等到对话里没有任务在运行时才生效。它会重启对话里的 Agent，停掉它们的后台命令，并在下一轮告诉模型新的工作位置。虚拟 Executor 不能用 `/cd`。

## 工具调用怎么选 Executor

每个可用的 Executor 都有一套文件和命令工具。同一个工具由两个或更多 Executor 提供时，模型看到的是一个工具，多了一个列出这些 Executor 的 `executor` 参数，不填就用默认 Executor：

```json
{"name": "bash", "arguments": {"executor": "buildbox", "command": "make test"}}
```

所以在同一轮里，Agent 可以在 `local` 上读文件、在 `buildbox` 上跑测试、在 `staging` 上查日志：

<!-- scene: executor-routing | 同名工具合并成一个，executor 参数决定在哪台机器执行 -->
```text
  local（默认）       buildbox（ssh）     staging（ssh）
  read_file  bash     read_file  bash     read_file  bash
        └───────────────────┼───────────────────┘
                            ▼
  模型看到的是一个 read_file 和一个 bash，都多一个参数
  executor: local | buildbox | staging
                            │
  read_file src/app.go                  ──▶ local      （不写 executor，用默认的）
  bash make test，executor=buildbox     ──▶ buildbox
  bash tail app.log，executor=staging   ──▶ staging
```

子 Agent 和对话共用同一批 Executor。想让一个对话只在一台机器上工作，就只让那个 Executor 可用。

## 哪些有限制，哪些没有

OpenVaala 运行工具前不会征求确认。请据此规划你的环境：

- `apply_patch` 只在 Executor 的工作目录里写文件。
- `read_file` 能读 Executor 上那个用户能读的任何文件。
- `bash` 在 `local` 和 `ssh` Executor 上运行真实的 shell。只检查它的起始目录；命令本身能做那个用户能做的任何事。
- 虚拟 Executor 和你的磁盘、网络都是隔离的。

想加检查，用 `PreToolUse` [hook](extensions.md#hook)，它可以拦下或改写任何工具调用。想让一个 Agent 完全碰不到命令，就别在它的[工具列表](agents.md)里写 `bash`。
