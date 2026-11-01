# 遥测

这一页介绍 OpenVaala 把日志写在哪里，以及怎么把 trace、指标和日志发到 OpenTelemetry 后端。

## 设置

```yaml
# config.yaml
telemetry:
  log_level: info                       # debug、info、warn 或 error
  log_file: ""                          # 为空时是 ~/.local/state/openvaala/openvaala.log
  otlp_endpoint: http://127.0.0.1:4318  # 为空时不导出
  otlp_headers:
    Authorization: Bearer <token>
  capture_content: true
```

- `log_level`：所有地方（日志文件、终端和 OTLP）写入的最低级别。默认 `info`。
- `log_file`：日志文件路径。相对路径以 `config.yaml` 所在目录为基准。默认 `~/.local/state/openvaala/openvaala.log`。
- `otlp_endpoint`：OTLP/HTTP 接收端的基础地址，例如 `http://127.0.0.1:4318`。OpenVaala 会自己接上 `/v1/traces`、`/v1/metrics` 和 `/v1/logs`，所以不要带信号路径。必须是 `http` 或 `https`。
- `otlp_headers`：每次导出都附带的请求头，一般用于认证。只能在配置文件里设置；名字会统一转成小写。
- `capture_content`：trace 里是否包含对话内容：系统指令、消息、模型输出、工具参数和结果。默认 `true`，只在设置了 `otlp_endpoint` 时生效。日志和指标从不包含对话内容。

> [!WARNING]
> 开着 `capture_content` 时，工具读到的任何东西（包括敏感文件的内容）都可能进到你的 trace 后端。不能接受的话，设为 `false`。

遥测设置在进程启动时读取。改完之后运行 `openvaala serve stop`，Runtime 服务下次启动时就会用上。

## 日志

每个 OpenVaala 进程都往日志文件里写 JSON 行。文件到 10 MB 时轮转，保留五份旧文件。`serve`、`acp` 和 `app-server` 还会往 stderr 打印方便人读的日志；终端界面和 `openvaala codex` 只写文件。

出问题却没有提示时（Runtime 服务起不来、MCP server 连不上、hook 行为不对），先看日志文件：

```bash
tail -f ~/.local/state/openvaala/openvaala.log
```

在前台运行 `openvaala serve` 能直接看到同样的信息。客户端没能在后台启动 Runtime 服务时，它的错误信息会告诉你日志文件在哪。

## Trace 和指标

设置了 `otlp_endpoint` 时，OpenVaala 通过 OTLP/HTTP 导出 trace、指标和日志，服务名是 `openvaala`。没设置时照样会创建 span，让日志行带上 trace id，但不会有任何数据离开这台机器。

trace 和指标遵循 OpenTelemetry 的 GenAI 语义约定。span 覆盖 Agent 的每一轮（`invoke_agent`）、模型调用（`chat`）和工具调用（`execute_tool`），以及 Runtime 服务处理的请求，带有模型、token 用量和结束原因等属性。指标包括：

- `gen_ai.client.inference.duration`、`gen_ai.client.inference.time_to_first_chunk`、`gen_ai.client.inference.time_per_output_chunk`
- `gen_ai.client.inference.usage.input_tokens`、`.output_tokens`、`.cache_read.input_tokens`、`.cache_write.input_tokens`、`.reasoning.output_tokens`
- `gen_ai.invoke_agent.duration`、`gen_ai.invoke_agent.inference_calls`、`gen_ai.invoke_agent.tool_calls`
- `gen_ai.execute_tool.duration`

想在本机试试，运行任意一个监听 4318 端口、支持 OTLP 的 collector 或后端，设置 `otlp_endpoint: http://127.0.0.1:4318`，再重启 Runtime 服务。
