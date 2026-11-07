# 配置

这一页介绍 OpenVaala 把配置放在哪里、`config.yaml` 里每个字段的作用，以及怎么检查和应用你的修改。

不手写任何配置也能运行 OpenVaala：终端界面或 Web 界面的首次引导会帮你添加模型 Provider。想修改默认值、添加 Executor 或扩展，或者在多台机器上运行 OpenVaala 时，再往下看。

## 配置目录

默认所有配置都在 `~/.config/openvaala/`：

```text
~/.config/openvaala/
├── config.yaml      应用设置（本页）
├── agents/          每个 Agent 一个 <name>.yaml           → agents.md
├── models/          每个 Provider 一个 <provider-id>.yaml → models.md
├── executors/       每个额外 Executor 一个 <id>.yaml      → executors.md
├── mcp_servers/     每个 MCP server 一个 <id>.yaml        → extensions.md
├── hooks/           任意多个 hook 文件 *.yaml             → extensions.md
├── skills/          <name>/SKILL.md                       → extensions.md
└── plugins/         每个插件一个目录                      → extensions.md
```

这些目录都遵守几条规则：

- 只读取定义目录下直接放着的 `.yaml` 文件。`.yml` 文件和子目录会被忽略（`skills/` 和 `plugins/` 例外，它们本来就按目录组织）。
- 文件按严格模式解析。不认识的字段、类型不对或 YAML 写错，都会报错并指出是哪个文件。
- 不存在的目录当作空目录。
- Agent、Provider、Executor 和 MCP server 文件里的 `name` 或 `id` 必须和文件名一致。

要用别的配置文件，在任何命令上加 `--config-file`，或者设置 `OPENVAALA_CONFIG`：

```bash
openvaala --config-file ~/work/openvaala/config.yaml
```

`config.yaml` 里的相对目录以 `config.yaml` 所在目录为基准，开头的 `~` 表示你的主目录。默认位置没有 `config.yaml` 没关系（用内置默认值）；你明确指定的文件必须存在。

OpenVaala 还会在配置目录以外的几个地方写文件：

| 路径 | 内容 |
| --- | --- |
| `~/.local/share/openvaala/` | 对话和它们的索引。见[对话](../guides/conversations.md#对话存在哪里)。 |
| `~/.local/state/openvaala/runtime-server.yaml` | 本机 Runtime 服务的地址和 token，本机客户端读它来连接。 |
| `~/.local/state/openvaala/openvaala.log` | 默认的日志文件。见[遥测](telemetry.md)。 |
| 用户缓存目录下的 `openvaala/models-dev.json`（Linux 是 `~/.cache`，macOS 是 `~/Library/Caches`） | 添加 Provider 时用的模型目录。 |
| 每个 Executor 所在机器上的 `~/.cache/openvaala/hooks/`、`~/.cache/openvaala/plugins/` | 拷过去运行的 hook 脚本和插件文件。见 [Hook](extensions.md#hook)。 |

## 完整的 config.yaml

所有字段都可以不写。下面的例子列出每个字段和它的默认值，注释另有说明的除外：

```yaml
agents:
  directory: agents          # Agent 定义
models:
  directory: models          # 模型 Provider 定义
  default_model: ""          # provider_id/model_id，例如 openai/gpt-5；为空时取按 Provider id、模型 id 排序后的第一个模型
  defaults:                  # 模型没写某个字段时继承的值（见 models.md）
    context_window: 256000
    max_output_tokens: 32000
    thinking: true
    reasoning_efforts: []
    default_reasoning_effort: ""
    merge_message_roles: []
    capabilities:
      input: []              # 为空表示 [text]
skills:
  directory: skills          # 你的私有 skill
executors:
  directory: executors       # 额外的 Executor（ssh、virtual、更多本机目录）
mcp_servers:
  directory: mcp_servers
hooks:
  directory: hooks
plugins:
  directory: plugins
  disabled: []               # 完全不加载的插件名
tools:
  max_image_kb: 500          # read_file 交给模型的单张图片上限，单位 KB；更大的图片会被压缩
run:
  agent_name: default        # 终端界面、编辑器和 Codex 用的根 Agent
server:
  listen: 127.0.0.1:8790     # `openvaala serve` 的地址
  token: ""                  # 为空时沿用 runtime-server.yaml 里的 token，没有就生成一个
  workspace_directory: ""    # 为空时用 serve 的启动目录
  conversation_idle_timeout: 30m  # 没人看的已保存对话到时停止；0s 表示不停止
client:
  url: ""                    # 为空时用本机 Runtime 服务，需要时自动启动
  token: ""
  path_mappings: []          # 例如 [{client: /Users/me/code, server: /srv/code}]
http:
  headers: {}                # 发往外部服务的额外请求头，叠加在三条内置默认之上（见下文）
telemetry:
  log_level: info
  log_file: ""               # 为空时是 ~/.local/state/openvaala/openvaala.log
  otlp_endpoint: ""          # 例如 http://127.0.0.1:4318；为空时不导出
  otlp_headers: {}
  capture_content: true
```

## 字段说明

### 定义目录

`agents.directory`、`models.directory`、`skills.directory`、`executors.directory`、`mcp_servers.directory`、`hooks.directory` 和 `plugins.directory` 指向上面介绍的那些目录，都不能为空。`--agents-dir` 和 `--models-dir` 可以为单个命令覆盖前两个。

### models

- `default_model`：没指定模型的 Agent 用的模型，写成 `provider_id/model_id`，必须在模型目录里存在。为空时，OpenVaala 按 Provider id、再按模型 id 排序，取第一个模型。终端界面（`/provider`）、Web 界面和桌面端的模型服务设置都可以帮你设置它。
- `defaults`：模型没写某个字段时继承的值。各字段见[模型](models.md#继承的默认值)。`context_window` 和 `max_output_tokens` 必须是正数。

### plugins

- `disabled`：完全不加载的插件名，它们的 skill、hook 和 MCP server 都不会加载。写了不存在的名字会被忽略。`openvaala plugin disable` 和 `enable` 会帮你改这个列表。

### tools

- `max_image_kb`：`read_file` 交给模型的单张图片上限，单位 KB。更大的图片会缩小到这个大小以内。必须大于 0。调大时要保持在 Provider 的单张图片上限以内：比如 Claude 会拒绝 base64 编码后超过约 5 MB 的图片，所以这个值要远小于 3700。

### run

- `agent_name`：从终端界面、编辑器（ACP）和 Codex 开始的对话用的根 Agent。`--agent` 可以为单个命令覆盖它。Web 界面和桌面端开始对话时总是用 `default` Agent。

### server

这些设置属于 `openvaala serve`，也就是 Runtime 服务。

- `listen`：监听的 `host:port`。默认只接受本机连接。用 `0.0.0.0:8790` 可以接受网络里其他机器的连接；改之前先看 [Web 界面](../guides/web.md#从另一台机器访问)。
- `token`：客户端必须出示的访问 token，以明文保存。为空时，serve 沿用 `~/.local/state/openvaala/runtime-server.yaml` 里的 token，第一次启动时随机生成一个。客户端通过 `Authorization: Bearer <token>` 请求头或 `?token=` 查询参数发送它。本机客户端会从那个文件读取，所以只有想要固定 token 时才需要设置。
- `workspace_directory`：新对话没指定工作目录时，`local` Executor 用的目录。必须是绝对路径，或者以 `~/` 开头。为空时，serve 用它的启动目录。由客户端自动拉起的 Runtime 服务，启动目录是你的主目录。
- `conversation_idle_timeout`：已保存的对话没人查看、没有 Turn 或后台命令在运行时，等多久由 serve 停止。对话记录不受影响，下次打开时重新加载，MCP server 也会重新连接。默认 `30m`，写 `0s` 表示不停止。改了它要重启 serve，重新加载不会生效。
  - 对话停止时会丢掉：已经结束的后台命令的输出、排队还没送达的 Agent 消息，以及默认 Executor 之外的 virtual Executor 里的文件。
  - 默认 Executor 是 virtual 的对话（文件只在内存里）和不带 Executor 的对话，不会被这样停止。

### client

这些设置告诉客户端命令（终端界面、`web`、`acp`、`app-server` 和 `codex`）连哪个 Runtime 服务。桌面端总是用本机的 Runtime 服务。

- `url`：服务的 WebSocket 地址，例如 `ws://10.0.0.5:8790/api/v1/openvaala`。为空时，客户端连接本机的 Runtime 服务，没在运行就启动它。`--url` 可以为单个命令覆盖它。
- `token`：`url` 对应的 token。如果 `url` 里带了 `?token=` 查询参数，以它为准。两者都为空、`url` 又指向本机 Runtime 服务时，客户端自动用本机的 token。`client.token` 从不读取 `server.token`，两者是故意分开的。
- `path_mappings`：在客户端机器和服务器之间转换目录。这样你在笔记本上从 `/Users/me/code/app` 打开的对话，会在服务器的 `/srv/code/app` 里运行：

  ```yaml
  client:
    path_mappings:
      - client: /Users/me/code
        server: /srv/code
      - client: 'C:\Users\me\code'    # Windows 客户端：盘符开头的路径，\ 和 / 等价
        server: /mnt/c/Users/me/code
  ```

  映射按完整的目录名匹配，多条都匹配时取前缀最长的一条。两边都必须是绝对路径，`server` 必须是 Unix 路径，同一个 `client` 或 `server` 目录不能出现两次。盘符写法的映射只对 Windows 客户端生效。编辑器（ACP）和 Codex 客户端在两个方向上转换路径；终端界面只转换它的启动目录。路径映射只能在配置文件里设置。

`ws://` 是明文传输。跨机器用 `client.url` 时，只在可信网络里用，或者把服务放到 HTTPS 反向代理后面，改用 `wss://`。写在 URL 里的 token 可能留在 shell 历史和代理日志里。

### http

`headers` 设置 OpenVaala 发往外部服务的所有请求的请求头：模型请求、Provider 的模型列表、模型目录下载、远程 MCP server（包括编辑器带来的）和桌面端下载的图片。OTLP 导出和只连本机的请求不受影响。

下面三条是内置默认，不改就会发出：

```yaml
http:
  headers:
    User-Agent: "openvaala/${OPENVAALA_VERSION} (${OPENVAALA_OS}; ${OPENVAALA_ARCH})"
    Session-Id: "${OPENVAALA_SESSION_ID}"
    Conversation-Id: "${OPENVAALA_CONVERSATION_ID}"
    X-Team: infra            # 你自己加的，每个外部请求都会带
```

- 请求头名字不分大小写。你写的条目按名字覆盖内置默认；没提到的默认条目照常发送。
- Provider 的 `headers`（见[模型](models.md#provider-字段)）和 MCP server 的 `headers`（见 [MCP server](extensions.md#mcp-server)）只发给这个 Provider 或 server，同名的会覆盖这里的条目。
- 模型请求里，全局和 Provider 的请求头会覆盖协议自己设置的同名请求头，例如 `Authorization`。Provider 的模型列表请求里，Provider 的请求头同样会覆盖；那里的全局请求头，以及 MCP server 等其他请求里的所有请求头，只补上请求本身还没有的。
- 值写成 `""` 表示不再发这个请求头，例如 `Session-Id: ""`。
- 值里可以写这些变量：

  | 变量 | 取值 |
  | --- | --- |
  | `${OPENVAALA_VERSION}` | OpenVaala 的版本号。 |
  | `${OPENVAALA_OS}`、`${OPENVAALA_ARCH}` | 操作系统和 CPU 架构，例如 `linux` 和 `amd64`。 |
  | `${OPENVAALA_CONVERSATION_ID}` | 请求所属对话的 ID。 |
  | `${OPENVAALA_SESSION_ID}` | 发出请求的 Session 的 ID。主 Agent 和每个子 Agent 各有自己的 Session，网关可以据此把它们各自粘到同一个渠道。 |
  | `${OPENVAALA_PARENT_SESSION_ID}` | 启动这个子 Agent 的 Session 的 ID；主 Agent 为空。 |
  | `${OPENVAALA_AGENT_NAME}` | 发出请求的 Agent 的名字。 |

- 一个请求头里的变量在这次请求里取不到值时，这个请求头整条不发。下载模型目录、查询 Provider 的模型列表时没有对话和 Session，对应的变量就没有值。只有 `${OPENVAALA_...}` 当作变量，其余的 `${...}` 原样发送。
- 写了 OpenVaala 不认识的变量名会让配置加载失败，这样写错变量名不会悄悄丢掉请求头：`~/.config/openvaala/config.yaml: http.headers.Session-Id: unknown variable OPENVAALA_SESION_ID`。
- `http.headers` 只能在配置文件里设置。这里的请求头会发给每一个外部请求：所有 Provider、MCP server、模型目录，以及桌面端下载的图片，图片被重定向到别的站点时也一样。凭证请写在 Provider 或 MCP server 自己的 `headers` 里。`config show` 会原样打印 `http.headers`。

`openvaala serve reload` 会让模型请求和 MCP server 用上新的设置。模型目录和 Provider 模型列表（Runtime 服务）、编辑器带来的 MCP server（`openvaala acp`）和桌面端下载图片，都是在进程启动时读取 `http.headers`；改了之后要重启对应的进程。

### telemetry

`log_level`、`log_file`、`otlp_endpoint`、`otlp_headers` 和 `capture_content` 控制日志、trace 和指标。见[遥测](telemetry.md)。

## 环境变量

所有标量设置都可以用环境变量覆盖，变量名是 `OPENVAALA_` 加上大写的字段路径，点换成下划线：

```bash
OPENVAALA_SERVER_LISTEN=0.0.0.0:8790 openvaala serve
OPENVAALA_CLIENT_URL=ws://10.0.0.5:8790/api/v1/openvaala openvaala
OPENVAALA_MODELS_DEFAULT_MODEL=openai/gpt-5 openvaala
```

设置按这个顺序生效，后面的覆盖前面的：内置默认值、`config.yaml`、环境变量、命令行参数（`--agents-dir`、`--models-dir`、`--agent`）。映射和对象列表（`telemetry.otlp_headers`、`http.headers` 和 `client.path_mappings`）只能在配置文件里设置。

`OPENVAALA_CONFIG` 用来指定配置文件本身；设置了就不能为空。

## 检查配置

有三个命令帮你处理配置，都不会连接模型 Provider。

```bash
openvaala config init    # 缺少时创建 config.yaml 和 agents/、models/、plugins/ 目录
openvaala config show    # 打印实际生效的设置，包括默认值和展开后的路径
openvaala config check   # 加载所有定义，检查选中的 Agent
```

`config init` 从不覆盖已有文件，它会打印 `config.yaml` 的路径。`config show` 会原样打印 token、OTLP 请求头和 HTTP 请求头，贴出它的输出时要小心。

`config check` 加载模型、Executor、MCP server、hook、插件和 Agent，校验它们之间的每个引用，然后打印选中的 Agent 和模型：

```text
configuration valid: agent=default provider=openai model=gpt-5
```

它不读取 skill 文件；`skills/` 目录里格式错误的 skill，要到 Runtime 服务启动或重新加载时才会报出来。

## 让改动生效

Runtime 服务在启动时读取配置。改完文件后，让它重新加载：

```bash
openvaala serve reload
```

也可以在终端界面用 `/reload`，或者在 Web 界面点 **设置 → 重新加载配置**。通过终端界面或 Web 界面的模型服务设置、以及 `openvaala plugin` 做的修改，会自动重新加载。

重新加载会替换 Agent、模型、Executor、MCP server、hook、skill、插件和 `server.workspace_directory`。已打开且空闲的对话立即换用新定义，之后打开的对话也用新定义。有 Turn 或后台命令在运行的对话继续用旧定义；命令会列出这些对话，等它们结束后再重新加载一次即可。新配置有错误时，重新加载会失败，服务继续用旧配置；启动时遇到同样的错误，Runtime 服务就启动不了。

有些设置只在启动时读取。要修改 `server.listen`、`server.token`、`server.conversation_idle_timeout`、`tools.max_image_kb` 或 `telemetry` 下的任何设置，先停掉服务，再让它重新启动：

```bash
openvaala serve stop
```

客户端设置（`client.*` 和 `run.agent_name`）在每次启动客户端命令时读取。
