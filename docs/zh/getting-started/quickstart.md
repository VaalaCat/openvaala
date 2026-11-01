# 快速开始

这一页带你从安装 OpenVaala 开始，在终端里完成第一个对话，再在浏览器里打开同一个对话，大约五分钟。

你需要一个模型 Provider 的 API key（OpenAI、Anthropic，或者任何兼容 OpenAI、Claude API 的服务），或者一个本地模型服务。

## 1. 安装

从 [GitHub Releases](https://github.com/VaalaCat/openvaala/releases) 下载适合你系统的 `openvaala` 二进制，放进 `PATH`；其他安装方式见[安装](install.md)。确认能运行：

```bash
openvaala --version
```

## 2. 添加模型 Provider

进到你要处理的项目目录，启动终端界面：

```bash
cd ~/code/my-project
openvaala
```

第一次运行时，OpenVaala 会在后台启动 Runtime 服务；因为还没有配置模型，它会打开「添加模型服务」：

1. **选服务。** 输入文字筛选目录；任何兼容的接口（比如本地模型服务）选「自定义」。
2. **核对信息**（目录里的服务会自动填好 ID、协议和 base URL），然后**填 API key**。
3. **选要用的模型。**
4. **选默认模型。**

Provider 保存在 `~/.config/openvaala/models/<id>.yaml`。以后可以用 `/provider` 修改，也可以直接改文件，见[模型](../configuration/models.md)。

## 3. 开始对话

输入一个请求，按 Enter：

```text
Explain what this project does and how to run its tests. Then run them.
```

Agent 会在当前目录里读文件、跑命令，每个工具调用和它的输出都会实时显示。它工作的时候，你可以继续输入：按 **Enter** 把消息补充进当前这一轮，按 **Tab** 排到这一轮之后，连按两次 **Esc** 中断。

> [!WARNING]
> OpenVaala 运行工具前不会征求确认，shell 命令也一样。请从一个你放心让 Agent 动手的目录开始；想加检查，见[工具](../configuration/tools.md#审批)。

输入 `/` 可以看到所有命令，比如 `/model` 切换模型、`/resume` 打开以前的对话。其他用法见[终端界面](../guides/terminal.md)。

## 4. 在浏览器里打开同一个对话

在另一个终端里运行：

```bash
openvaala web
```

浏览器会打开 Web 界面，终端里的那个对话就在侧边栏里；如果 Agent 还没做完，它仍在运行。两个客户端连的是同一个 Runtime 服务，下一条消息从哪边发都行。

Release 二进制和 `go install ...@latest` 都带 Web 界面。使用 `@main` 的源码时需要另外构建页面，见[安装](install.md)。

## 接下来

- 在[桌面端](../guides/desktop.md)、[编辑器](../guides/editors.md)或 [Codex](../guides/codex.md) 里使用 OpenVaala。
- 了解[对话](../guides/conversations.md)怎么处理子 Agent、补充消息、分叉和长上下文。
- 添加 [SSH 机器和沙箱](../configuration/executors.md)，让一个对话同时在多台机器上工作。
- 编写自己的 [Agent](../configuration/agents.md)，添加 [skill、MCP server 和 hook](../configuration/extensions.md)。
- 直接问 Agent：OpenVaala 自带这份文档作为内置 skill，在任何对话里问「怎么添加 SSH Executor？」都行。
