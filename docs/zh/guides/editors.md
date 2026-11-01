# 编辑器

这一页介绍怎么在支持 [Agent Client Protocol](https://agentclientprotocol.com)（ACP）的编辑器里把 OpenVaala 当作 Agent 使用，比如 Zed。

编辑器启动 `openvaala acp`，通过 stdin 和 stdout 和它通信。这个进程连的是和其他客户端同一个 Runtime 服务，所以在编辑器里开始的对话，在终端界面、Web 界面和桌面端里也能看到，反过来也一样。

## 配置 Zed

确认 Zed 能在它看到的 `PATH` 里找到 `openvaala`，然后在 Zed 的 `settings.json` 里加一个 agent server（Agent Settings → External Agents → Add Agent → Add Custom Agent 会帮你打开这个文件）：

```json
{
  "agent_servers": {
    "OpenVaala": {
      "type": "custom",
      "command": "openvaala",
      "args": ["acp"],
      "env": {}
    }
  }
}
```

打开 Agent Panel，用 **OpenVaala** 开一个新 thread。thread 的工作目录就是你的项目目录。

要用别的配置文件，把它加进参数：`"args": ["--config-file", "/path/to/config.yaml", "acp"]`。其他 ACP 编辑器也只需要这两样：命令 `openvaala` 和参数 `acp`。

如果还没有添加模型 Provider，先在终端里运行一次 `openvaala`，按引导完成设置，或者用 Web 界面；编辑器本身没有 Provider 设置。

## 支持哪些功能

- **对话**：新建会话、会话列表（最近的在前，可以按目录筛选）、加载以前的会话和它的完整历史。
- **流式显示**：消息、推理过程，以及工具调用的状态、输入和输出。`read_file` 和 `apply_patch` 会显示文件位置；命令输出边产生边显示。
- **取消**：停下正在运行的这一轮。如果你从编辑器发送消息时，有一个从别的客户端发起的 Turn 正在运行，那一轮会被中断，先运行你这条。
- **会话选项**：模型、推理强度（模型有强度可选时）和默认 [Executor](../configuration/executors.md)。
- **斜杠命令**：你的 [skill](../configuration/extensions.md#skill) 会作为斜杠命令出现。
- **上下文**：你附加的图片、文件和选中的内容。支持文本资源和图片，其他类型的附件会被拒绝。
- **上下文占用和标题**：对话的上下文占用和标题会报告给编辑器。

编辑器自己的权限确认、模式和计划都不会用到：OpenVaala 运行工具不征求确认（见[审批](../configuration/tools.md#审批)）。

## 编辑器里的 MCP server

在编辑器里配置的 MCP server（stdio、http 或 sse）也能在这个对话里用。它们运行在编辑器所在的机器上：stdio server 在你的项目目录里、用编辑器的环境变量启动，它们的消息经过 ACP 连接转发给 Runtime 服务。它们的工具名是 `mcp__<name>__<tool>`，名字里字母、数字、`-` 和 `_` 以外的字符会换成 `_`。

这些 server 属于这条编辑器连接。编辑器断开后它们就没了，这个对话之后的 Turn 也就看不到它们的工具。只有没写 `mcp_servers` 列表的 Agent 才能拿到它们。你自己配置里的 MCP server 照常可用，见 [MCP server](../configuration/extensions.md#mcp-server)。

## 关掉编辑器之后

关掉编辑器会结束 `openvaala acp`，但不会结束工作：正在运行的那一轮会在 Runtime 服务里继续，之后可以再加载这个会话。编辑器自己拉起的 Runtime 服务，在最后一个客户端断开、也没有任务在运行之后，大约十秒自动退出。随时可以运行 `openvaala serve stop` 停掉它。

## 使用另一台机器上的 Runtime 服务

想在本地编辑、在服务器上跑对话，就让编辑器的 `openvaala acp` 连到那台服务器：在配置里设置 `client.url` 和 `client.token`，或者在参数里加 `--url`：

```json
"args": ["--url", "ws://10.0.0.5:8790/api/v1/openvaala", "acp"]
```

如果项目在服务器上的路径不一样，加一条 [`client.path_mappings`](../configuration/overview.md#client)。ACP 会话会在两个方向上转换工作目录；没有匹配的映射时，远端服务器上的新会话用服务器的 `server.workspace_directory`。加载已有会话时，沿用它创建时的目录。编辑器里显示的工具位置用的是服务器上的路径。
