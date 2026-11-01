import { OverflowTooltip } from "@/components/base/overflow-tooltip"
import type { ParseKeys } from "i18next"
import AnsiModule from "ansi-to-react"
import { useStore } from "@nanostores/react"
import { useTranslation } from "react-i18next"
import { OutputBlock } from "@/components/base/output-block"
import {
  KnownValueLabel,
  ToolCard,
  ToolMeta,
} from "@/components/conversation/tool-call-view"
import type { ToolRendererProps } from "@/lib/item-renderers"
import {
  getArrayField,
  getNumberField,
  getStringField,
  isCommandFailed,
  parseCommandResult,
  parseCommandState,
  stripAnsi,
  type CommandResult,
  type CommandState,
} from "@/lib/tool-arguments"

// ansi-to-react 是 CommonJS 包，打包时按 Node 方式互通，默认导入拿到的是整个 exports 对象
const Ansi =
  (AnsiModule as unknown as { default?: typeof AnsiModule }).default ??
  AnsiModule

// 命令状态和停止原因的文案；serve 新增的取值原样显示
const commandStatusKeys: Record<string, ParseKeys> = {
  running: "conversation.tool.command.status.running",
  exited: "conversation.tool.command.status.exited",
  failed: "conversation.tool.command.status.failed",
}
const stopReasonKeys: Record<string, ParseKeys> = {
  stopped: "conversation.tool.command.stopReason.stopped",
  timed_out: "conversation.tool.command.stopReason.timedOut",
  session_closed: "conversation.tool.command.stopReason.sessionClosed",
}

// bash：详情显示「$ 命令」和带颜色的输出，执行中跟随 item/toolProgress，
// 底部是退出码、停止原因和后台状态。命令非零退出时卡片按失败显示
export function BashTool(props: ToolRendererProps) {
  const { call, result, state, sync } = props
  const progress = useStore(sync.$toolProgress, { keys: [call.call_id] })[
    call.call_id
  ]
  const { value } = props.arguments
  const command = getStringField(value, "command")
  const commandResult = parseCommandResult(result?.content)
  const output =
    commandResult?.output ??
    (typeof progress?.content === "string" ? progress.content : "")
  return (
    <ToolCard
      {...props}
      state={commandResult && isCommandFailed(commandResult) ? "failed" : state}
      argumentSummary={
        command && <span className="text-code">{getFirstLine(command)}</span>
      }
      displayedArguments={["command"]}
    >
      <CommandOutput command={command} output={output} />
      {commandResult && (
        <CommandStatus
          result={commandResult}
          cwd={getStringField(value, "cwd")}
        />
      )}
    </ToolCard>
  )
}

// read_command_output：详情显示命令编号、这一页输出和命令当时的状态。
export function ReadCommandOutputTool(props: ToolRendererProps) {
  const commandResult = parseCommandResult(props.result?.content)
  const commandID = getStringField(props.arguments.value, "command_id")
  return (
    <ToolCard
      {...props}
      state={
        commandResult && isCommandFailed(commandResult) ? "failed" : props.state
      }
      argumentSummary={
        commandID && <span className="text-code">{commandID}</span>
      }
    >
      {commandResult && (
        <>
          <CommandOutput output={commandResult.output} />
          <CommandStatus result={commandResult} />
        </>
      )}
    </ToolCard>
  )
}

// write_stdin：详情显示命令编号、转义后的输入、写入字节数和命令状态。
export function WriteStdinTool(props: ToolRendererProps) {
  const { t } = useTranslation()
  const { result } = props
  const { value } = props.arguments
  const commandID = getStringField(value, "command_id")
  const chars = getStringField(value, "chars")
  const summary = [commandID, chars !== undefined && JSON.stringify(chars)]
    .filter(Boolean)
    .join(" ← ")
  const writtenBytes = getNumberField(result?.content, "written_bytes")
  const status = getStringField(result?.content, "status")
  return (
    <ToolCard
      {...props}
      argumentSummary={summary && <span className="text-code">{summary}</span>}
    >
      {writtenBytes !== undefined && (
        <ToolMeta
          items={[
            t("conversation.tool.command.writtenBytes", {
              count: writtenBytes,
            }),
            status && (
              <KnownValueLabel labelKeys={commandStatusKeys} value={status} />
            ),
          ]}
        />
      )}
    </ToolCard>
  )
}

// stop_command：详情显示命令编号、停下后的状态、退出码和停止原因。
export function StopCommandTool(props: ToolRendererProps) {
  const commandState = parseCommandState(props.result?.content)
  const commandID = getStringField(props.arguments.value, "command_id")
  return (
    <ToolCard
      {...props}
      state={
        commandState && isCommandFailed(commandState) ? "failed" : props.state
      }
      argumentSummary={
        commandID && <span className="text-code">{commandID}</span>
      }
    >
      {commandState && <CommandStatus result={commandState} />}
    </ToolCard>
  )
}

// list_commands：详情按启动顺序列出每条命令的编号、命令和状态。
export function ListCommandsTool(props: ToolRendererProps) {
  const { t } = useTranslation()
  const { result } = props
  // 每条命令有状态字段和命令原文；没有状态的条目跳过
  const commands = getArrayField(result?.content, "commands").flatMap(
    (each) => {
      const state = parseCommandState(each)
      return state
        ? [{ ...state, command: getStringField(each, "command") }]
        : []
    }
  )
  return (
    <ToolCard
      {...props}
      resultSummary={
        result &&
        !result.error &&
        t("conversation.tool.command.listed", { count: commands.length })
      }
    >
      {commands.length > 0 && (
        <ul className="flex flex-col gap-1">
          {commands.map((command, index) => (
            <li
              key={command.command_id || index}
              className="flex min-w-0 items-center gap-2"
            >
              <span className="shrink-0 text-table color-muted">
                {command.command_id}
              </span>
              <OverflowTooltip
                render={<span className="min-w-0 flex-1 truncate text-code" />}
              >
                {getFirstLine(command.command ?? "")}
              </OverflowTooltip>
              <span className="flex shrink-0 items-center gap-2 text-ui">
                <KnownValueLabel
                  labelKeys={commandStatusKeys}
                  value={command.status}
                />
                {command.exit_code !== null && (
                  <span className="text-table">
                    {t("conversation.tool.command.exitCode", {
                      code: command.exit_code,
                    })}
                  </span>
                )}
              </span>
            </li>
          ))}
        </ul>
      )}
    </ToolCard>
  )
}

// 命令输出框：ANSI 颜色按主题显示；收起时限高，贴着底部时跟随新输出，用户往上翻就不再跟随；超出时可展开全部
function CommandOutput({
  command,
  output,
}: {
  // 给出时在第一行显示「$ 命令」
  command?: string
  output: string
}) {
  return (
    <OutputBlock copyValue={stripAnsi(output)} followOutput>
      <pre className="w-max min-w-full px-3 py-2 text-code">
        {command !== undefined && (
          <span className="color-muted">
            {"$ " + command + (output ? "\n" : "")}
          </span>
        )}
        <Ansi useClasses className="ansi-output">
          {output}
        </Ansi>
      </pre>
    </OutputBlock>
  )
}

// 命令结果的状态行：仍在运行（带编号）、退出码、停止原因、输出没读完或被截断；启动失败的原因另起一行
function CommandStatus({
  result,
  cwd,
}: {
  // stop_command 的结果只有状态，没有输出相关的字段
  result: CommandState & Partial<CommandResult>
  // bash 参数里的工作目录
  cwd?: string
}) {
  const { t } = useTranslation()
  return (
    <>
      <ToolMeta
        items={[
          // 已退出的命令由退出码说明结果，不再写「已退出」
          result.status === "running"
            ? t("conversation.tool.command.background", {
                id: result.command_id,
              })
            : result.status !== "exited" && (
                <KnownValueLabel
                  labelKeys={commandStatusKeys}
                  value={result.status}
                />
              ),
          result.exit_code !== null && (
            <span
              data-status-tone={
                result.exit_code === 0 ? "success" : "destructive"
              }
            >
              {t("conversation.tool.command.exitCode", {
                code: result.exit_code,
              })}
            </span>
          ),
          result.stop_reason && (
            <KnownValueLabel
              labelKeys={stopReasonKeys}
              value={result.stop_reason}
            />
          ),
          result.has_more && t("conversation.tool.command.hasMore"),
          result.truncated && t("conversation.tool.command.truncated"),
          cwd && t("conversation.tool.command.cwd", { cwd }),
        ]}
      />
      {result.error && (
        <p className="text-body wrap-anywhere whitespace-pre-wrap color-destructive">
          {result.error}
        </p>
      )}
    </>
  )
}

// 第一个非空行，去掉首尾空白；同 TUI 的 getFirstLine
function getFirstLine(text: string): string {
  return (
    text
      .split("\n")
      .map((line) => line.trim())
      .find(Boolean) ?? ""
  )
}
