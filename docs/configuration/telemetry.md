# Telemetry

This page explains where OpenVaala writes its logs and how to send traces, metrics and logs to an OpenTelemetry backend.

## Settings

```yaml
# config.yaml
telemetry:
  log_level: info                       # debug, info, warn or error
  log_file: ""                          # empty = ~/.local/state/openvaala/openvaala.log
  otlp_endpoint: http://127.0.0.1:4318  # empty = no export
  otlp_headers:
    Authorization: Bearer <token>
  capture_content: true
```

- `log_level` — the lowest level written everywhere: the log file, the terminal and OTLP. Default `info`.
- `log_file` — the log file path. A relative path is resolved against the directory of `config.yaml`. Default `~/.local/state/openvaala/openvaala.log`.
- `otlp_endpoint` — the base URL of an OTLP/HTTP receiver, such as `http://127.0.0.1:4318`. OpenVaala appends `/v1/traces`, `/v1/metrics` and `/v1/logs` itself, so do not include a signal path. Must be `http` or `https`.
- `otlp_headers` — headers sent with every export, typically for authentication. Only settable in the file; names are lower-cased.
- `capture_content` — whether traces include conversation content: system instructions, messages, model output, tool arguments and results. Default `true`, and only in effect when `otlp_endpoint` is set. Logs and metrics never contain conversation content.

> [!WARNING]
> With `capture_content` on, anything a tool reads — including the contents of sensitive files — can end up in your tracing backend. Set it to `false` if that is not acceptable.

Telemetry settings are read when a process starts. After changing them, run `openvaala serve stop` so the runtime server picks them up on its next start.

## Logs

Every OpenVaala process writes JSON lines to the log file. The file rotates at 10 MB and five old files are kept. `serve`, `acp` and `app-server` also print human-readable logs to stderr; the terminal UI and `openvaala codex` write only to the file.

When something fails silently — a runtime server that will not start, an MCP server that does not connect, a hook that misbehaves — the log file is the first place to look:

```bash
tail -f ~/.local/state/openvaala/openvaala.log
```

Running `openvaala serve` in the foreground shows the same messages directly. If a client cannot start the runtime server in the background, its error message points you to the log file.

## Traces and metrics

When `otlp_endpoint` is set, OpenVaala exports traces, metrics and logs over OTLP/HTTP with the service name `openvaala`. Without it, spans are still created so that log lines carry trace ids, but nothing leaves the machine.

Traces and metrics follow the OpenTelemetry GenAI semantic conventions. Spans cover agent turns (`invoke_agent`), model calls (`chat`) and tool calls (`execute_tool`), as well as runtime server requests, with attributes such as model, token usage and finish reason. Metrics include:

- `gen_ai.client.inference.duration`, `gen_ai.client.inference.time_to_first_chunk`, `gen_ai.client.inference.time_per_output_chunk`
- `gen_ai.client.inference.usage.input_tokens`, `.output_tokens`, `.cache_read.input_tokens`, `.cache_write.input_tokens`, `.reasoning.output_tokens`
- `gen_ai.invoke_agent.duration`, `gen_ai.invoke_agent.inference_calls`, `gen_ai.invoke_agent.tool_calls`
- `gen_ai.execute_tool.duration`

To try it locally, run any OTLP-capable collector or backend that listens on port 4318, set `otlp_endpoint: http://127.0.0.1:4318`, and restart the runtime server.
