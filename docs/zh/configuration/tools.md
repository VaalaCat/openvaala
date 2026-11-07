# 工具

这一页列出 Agent 能调用的工具、每个工具做什么，以及怎么限制它们。

Agent 的工具分四组：

- **文件工具**：`read_file`、`apply_patch`、`glob`、`grep`
- **命令工具**：`bash`、`read_command_output`、`write_stdin`、`stop_command`、`list_commands`
- **协作工具**：`spawn_agent`、`send_message`、`wait_agent`、`interrupt_agent`、`list_agents`、`read_session_history`
- **MCP 工具**：名字是 `mcp__<server>__<tool>`，见 [MCP server](extensions.md#mcp-server)

文件和命令工具在 [Executor](executors.md) 上运行。有多个 Executor 可用时，它们多一个 `executor` 参数，不填就用对话的默认 Executor。协作工具在 Runtime 服务里运行。

没有专门的 skill 工具：Agent 用 `read_file` 读 skill 的 `SKILL.md`。

## 文件工具

### read_file

按行读文本文件，或者读图片。

- `path`：相对工作目录的路径，或者绝对路径。
- `offset`：从第几行开始读，从 1 算起。默认 1。
- `limit`：读多少行，1 到 2000。默认 200。

每行返回时前面带着行号。一次结果最多包含 64 KB 的完整行；提前截断时，结果会说明从哪里接着读。不是 UTF-8 文本的文件、超过 64 KB 的单行，都会报错。读文件的同时，`read_file` 会把工作目录到这个文件所在目录之间、还不在 Agent 上下文里的 `AGENTS.md` 一起带上，见[项目说明](extensions.md#项目说明)。

**图片。** PNG、JPEG、GIF 和 WebP 文件按内容识别，作为图片返回（忽略 `offset` 和 `limit`）。接受最大 20 MB、8000 × 8000 像素的文件。超过 [`tools.max_image_kb`](overview.md#tools)（默认 500 KB）的图片会转成 JPEG 并缩小到符合大小；结果里会注明压缩过，并给出原始尺寸。一次请求最多带最近的 20 张图片，更早的换成一段说明，告诉模型需要时重新读取。如果模型不能接收图片（它的 [`capabilities.input`](models.md#模型字段) 里没有 `image`），工具照样运行，但模型看到的是占位文字，不是图片。

### apply_patch

用一个补丁创建、修改和删除文件，格式和 Codex 用的一样：

```text
*** Begin Patch
*** Add File: docs/notes.md
+# Notes
+First line.
*** Update File: src/main.go
@@
 func main() {
-	fmt.Println("hello")
+	fmt.Println("hello, world")
 }
*** Delete File: old.txt
*** End Patch
```

- 路径相对工作目录，并且不能超出它。
- 每个修改块按上下文行和删除行精确匹配来定位，至少要有其中一种。
- 新增要求文件不存在；修改和删除要求是普通文件。不支持重命名（`*** Move to:`）。
- 一个补丁最多涉及 128 个文件、1024 个修改块，最大 4 MB。
- 文件一个接一个地写。如果后面的文件失败，前面的已经改了，结果会说明补丁只应用了一部分。

### glob

按通配符列出文件路径，不读文件内容。

- `pattern`：相对 `path` 的通配符，例如 `**/*.go`、`src/**/test_*.py`。不含 `/` 的通配符按文件名匹配任意深度，所以 `*.go` 能找到所有 Go 文件。
- `path`：从哪个目录开始找，相对工作目录或者绝对路径。默认是工作目录。
- `limit`：返回多少条路径，1 到 1000。默认 100。

路径按顺序排好，写成相对工作目录的路径（`path` 指到工作目录之外时是绝对路径）。匹配数超过 `limit` 时，结果里 `truncated` 为 `true`。

### grep

用正则逐行搜索文件内容。

- `pattern`：正则表达式。
- `path`：目录或文件，相对工作目录或者绝对路径。默认是工作目录。
- `glob`：只搜路径匹配这个通配符的文件，相对 `path`，例如 `*.go`。
- `ignore_case`：忽略大小写。默认 false。
- `limit`：返回多少条匹配行，1 到 1000。默认 100。

每条匹配有 `path`、`line`（从 1 算起）和这一行的 `text`（超过 500 个字符截断）。二进制文件会跳过。结果按路径和行号排序，`truncated` 为 `true` 表示匹配数超过了 `limit`。

### glob 和 grep 怎么搜

隐藏的文件和目录（名字以 `.` 开头，包括 `.git`）一律跳过。在 `local` 和 `ssh` Executor 上，工具按下面的顺序，用机器上找到的第一种：

1. `rg`（[ripgrep](https://github.com/BurntSushi/ripgrep)）。
2. `git ls-files` 和 `git grep`，要求目录在 git 仓库里。`.gitignore` 排除的文件会跳过。
3. `find` 和 `grep`。它们不读 `.gitignore`。

不管用哪一种，结果都一样，所以正则只能用 ripgrep 和 POSIX 扩展正则两边都认的写法：`\d`、`\w`、`\s` 和它们的大写形式可以用，`(?i)` 这类内联标记会被拒绝，忽略大小写请用 `ignore_case`。`virtual` Executor 里没有命令可跑，直接遍历内存里的文件。匹配数超过 `limit` 时，三种命令保留下来的是哪几条，不保证相同。

## 命令工具

工具调用返回后命令还会继续运行，所以 Agent 可以启动一个构建或服务，先去做别的，再回来看。

### bash

在 Executor 上用 `bash -c` 启动一个命令。

- `command`：必填。
- `cwd`：起始目录，相对工作目录，或者工作目录里的绝对路径。默认 `.`。
- `wait_seconds`：返回前最多等多久。默认 10。命令提前结束时，`bash` 立即返回。
- `timeout_seconds`：超过这么久就停掉命令。默认 600；0 表示不限时。
- `interactive`：在终端（PTY）里运行，这样 `write_stdin` 可以往里输入。默认 false。

结果里有 `command_id`、状态（`running`、`exited` 或 `failed`）、结束后的退出码，以及第一页输出（stdout 和 stderr 合在一起，按到达顺序）。非 0 退出码是正常结果，不算工具出错。

### read_command_output

读取命令的更多输出：`command_id`、`cursor`（字节位置；从 0 开始，或者用上一次返回的 `next_cursor`）和 `wait_seconds`（默认 10，等待新输出的时间）。每页最多 64 KB。每个命令只保留最后 1 MB 输出；从已经丢弃的位置读时，会从最早保留的字节开始，并注明这一点。

### write_stdin

向交互式命令发送字符：`command_id` 和 `chars`。不会自动加换行，要按 Enter 就带上 `\n`。只对用 `interactive: true` 启动的命令有效。

### stop_command

停掉一个命令和它的子进程：`command_id`。

### list_commands

列出 Agent 在某个 Executor 上启动的命令，带着状态和退出码。

命令属于启动它的 Agent，这个 Agent 的 Session 结束时命令会被停掉，比如对话重新配置或 Runtime 服务停止的时候。每个 Agent 最多保留 64 个命令，最早结束的先被清掉。

内置的 `openvaala` Executor 和其他虚拟 Executor 在内嵌解释器里运行命令，没有外部程序，也不能联网，不支持 `interactive`。

## 协作工具

这组工具让 Agent 把工作交给子 Agent，并和它们协调。实际用起来是什么样，见[子 Agent](../guides/conversations.md#子-agent)。

- `spawn_agent`：启动一个子 Agent。参数：`task_name` 和 `message`（必填）；`agent_name`（任何一个已配置的 Agent；默认和当前 Agent 相同）；`model`（`provider/model`）和 `reasoning_effort`；`fork_turns`：继承多少已完成的历史，可以是 `none`（默认）、`all`，或者最近几个 Turn 的数量。子 Agent 每完成一轮都会自动汇报。
- `send_message`：给子 Agent 或者自己的直接父 Agent 发消息。`mode` 是 `normal`（开始新一轮，忙时排队）或 `steer`（补充进正在进行的这一轮）。发给子 Agent 时，可以用 `model` 和 `reasoning_effort` 换掉它的模型。
- `wait_agent`：等到子 Agent 汇报，或者有补充消息到达（来自你或其他 Agent）。`timeout_seconds` 是 1 到 600；默认 30。它返回被唤醒的原因；消息本身在下一次请求时交给模型。
- `interrupt_agent`：取消子 Agent 正在运行的那一轮。
- `list_agents`：列出所有子 Agent，带着状态、token 用量和上下文窗口。
- `read_session_history`：分页读取子 Agent 的历史，最新的在前。

## 限制 Agent 能用的工具

Agent 的 `tools` 列表只在文件和命令工具里挑：

```yaml
# agents/reviewer.yaml
tools: [read_file, bash, read_command_output, write_stdin, stop_command, list_commands]
```

- 合法的名字是 `read_file`、`apply_patch`、`glob`、`grep`、`bash`、`read_command_output`、`write_stdin`、`stop_command` 和 `list_commands`。写了其他名字，对话启动时会报错。
- 不写 `tools` 表示全部都能用。
- `bash` 只负责启动命令，最多等 `wait_seconds`；把其他命令工具一起列上，Agent 才能读输出、输入内容、停掉命令。
- 协作工具总是可用。MCP 工具改用 Agent 的 `mcp_servers` 列表来选。

## 审批

工具调用不征求确认就直接运行。想在某些调用之前加检查（拒绝 `rm -rf`、保护某个目录、要求填工单号），就写一个 `PreToolUse` [hook](extensions.md#hook)。hook 可以拦下调用并给出模型能看到的原因，也可以改写它的参数。
