// 工具参数解析的结果：参数还在生成或模型写坏了时保留原文，value 为空
export type ParsedToolArguments =
  { ok: true; value: unknown } | { ok: false; raw: string; value?: undefined }

// PatchFileChange 是补丁对一个文件的操作和增删行数，用来在工具卡标题里列出改了哪些文件
export interface PatchFileChange {
  path: string
  kind: "add" | "update" | "delete"
  addedLines: number
  deletedLines: number
}

// 解析工具调用的 arguments（JSON 文本）；解析不了时返回原文
export function parseToolArguments(text: string): ParsedToolArguments {
  try {
    return { ok: true, value: JSON.parse(text) }
  } catch {
    return { ok: false, raw: text }
  }
}

// 对象里名为 name 的字符串字段；不是对象或字段不是字符串时返回 undefined
export function getStringField(
  value: unknown,
  name: string
): string | undefined {
  const field = getFields(value)?.[name]
  return typeof field === "string" ? field : undefined
}

// 对象里名为 name 的数字字段；不是对象或字段不是数字时返回 undefined
export function getNumberField(
  value: unknown,
  name: string
): number | undefined {
  const field = getFields(value)?.[name]
  return typeof field === "number" ? field : undefined
}

// 对象里名为 name 的布尔字段；不是对象或字段不是布尔值时返回 undefined
export function getBooleanField(
  value: unknown,
  name: string
): boolean | undefined {
  const field = getFields(value)?.[name]
  return typeof field === "boolean" ? field : undefined
}

// 对象里名为 name 的数组字段；Go 的 nil 切片编码成 null，不是数组时一律当空数组
export function getArrayField(value: unknown, name: string): unknown[] {
  const field = getFields(value)?.[name]
  return Array.isArray(field) ? field : []
}

// 对象里名为 name 的字符串数组字段，跳过不是字符串的元素
export function getStringArrayField(value: unknown, name: string): string[] {
  return getArrayField(value, name).filter(
    (each): each is string => typeof each === "string"
  )
}

// 对象里按字段顺序的第一个字符串值，用作没有专门显示的工具的摘要；没有时返回 undefined
export function getFirstStringField(value: unknown): string | undefined {
  const fields = getFields(value)
  if (!fields) return undefined
  return Object.values(fields).find(
    (field): field is string => typeof field === "string"
  )
}

// 长路径保留最后两级父目录和文件名；短路径保持原写法。
export function shortenToolPath(path: string): string {
  // 1. 根目录、盘符和相对目录前缀不计入目录层级。
  const parts = path.replace(/\\/g, "/").split("/").filter(Boolean)
  const segments = parts.filter(
    (part, index) => !(index === 0 && /^(?:[a-z]:|~|\.)$/i.test(part))
  )
  return segments.length > 3 ? ".../" + segments.slice(-3).join("/") : path
}

// 内置工具只预览明确的输入字段，未知工具不推测参数含义。
export function getToolArgumentPreview(name: string, value: unknown): string {
  // 1. 路径缩短显示，其余参数保留第一行。
  const firstLine = (text: string | undefined) =>
    text?.split(/\r?\n/, 1)[0].trim() ?? ""
  switch (name) {
    case "read_file":
      return firstLine(shortenToolPath(getStringField(value, "path") ?? ""))
    case "apply_patch":
      return getPatchFileChanges(getStringField(value, "patch") ?? "")
        .map((change) => shortenToolPath(change.path))
        .join(", ")
    case "bash":
      return firstLine(getStringField(value, "command"))
    case "glob":
    case "grep":
      return firstLine(getStringField(value, "pattern"))
    case "read_command_output":
    case "stop_command":
      return firstLine(getStringField(value, "command_id"))
    case "write_stdin": {
      const commandID = firstLine(getStringField(value, "command_id"))
      const chars = getStringField(value, "chars")
      return [commandID, chars !== undefined ? JSON.stringify(chars) : ""]
        .filter(Boolean)
        .join(" ← ")
    }
    case "spawn_agent":
      return (
        firstLine(getStringField(value, "task_name")) ||
        firstLine(getStringField(value, "message"))
      )
    case "send_message":
      return firstLine(getStringField(value, "message"))
    case "interrupt_agent":
    case "read_session_history":
      return firstLine(getStringField(value, "session_id"))
    default:
      return ""
  }
}

const patchFileHeaders: [prefix: string, kind: PatchFileChange["kind"]][] = [
  ["*** Add File: ", "add"],
  ["*** Update File: ", "update"],
  ["*** Delete File: ", "delete"],
]

// 按补丁顺序列出 Begin Patch 补丁涉及的文件和增删行数；不校验格式，生成到一半的补丁也能读出已有的部分
export function getPatchFileChanges(patch: string): PatchFileChange[] {
  const changes: PatchFileChange[] = []
  for (const line of patch.split("\n")) {
    // 1. 文件头开始一个新文件。
    const header = patchFileHeaders.find(([prefix]) => line.startsWith(prefix))
    if (header) {
      const [prefix, kind] = header
      changes.push({
        path: line.slice(prefix.length).trim(),
        kind,
        addedLines: 0,
        deletedLines: 0,
      })
      continue
    }
    // 2. 其余以 + 或 - 开头的行记到当前文件上，其他标记行跳过。
    const change = changes.at(-1)
    if (!change || line.startsWith("***")) continue
    if (line.startsWith("+")) change.addedLines++
    else if (line.startsWith("-")) change.deletedLines++
  }
  return changes
}

// read_file 结果正文去掉每行开头的「行号: 」，剩下文件原文
export function getReadFileText(content: string): string {
  return content.replace(/^\d+: /gm, "")
}

// CommandState 是命令当时的状态，stop_command、write_stdin、list_commands 的结果里也有；同 pkg/executor/tools 的同名字段
export interface CommandState {
  command_id: string
  // running、exited 或 failed
  status: string
  // 只有 exited 有；被信号终止（含停止）时为 -1
  exit_code: number | null
  // stopped（stop_command）、timed_out 或 session_closed
  stop_reason?: string
}

// CommandResult 是 bash、read_command_output 结果的 content：命令状态加从 cursor 起的一页输出；
// 同 pkg/executor/tools 的 CommandResult
export interface CommandResult extends CommandState {
  // failed 的原因
  error?: string
  output: string
  has_more: boolean
  truncated: boolean
}

// 读出命令状态；没有 status 时不是命令状态，返回 undefined
export function parseCommandState(value: unknown): CommandState | undefined {
  const status = getStringField(value, "status")
  if (status === undefined) return undefined
  return {
    command_id: getStringField(value, "command_id") ?? "",
    status,
    exit_code: getNumberField(value, "exit_code") ?? null,
    stop_reason: getStringField(value, "stop_reason"),
  }
}

// 解析 bash、read_command_output 的结果正文；缺少 status 或 output 时不是命令结果，返回 undefined
export function parseCommandResult(
  content: unknown
): CommandResult | undefined {
  const state = parseCommandState(content)
  const output = getStringField(content, "output")
  if (!state || output === undefined) return undefined
  return {
    ...state,
    error: getStringField(content, "error"),
    output,
    has_more: getBooleanField(content, "has_more") === true,
    truncated: getBooleanField(content, "truncated") === true,
  }
}

// 命令是否失败？failed 或非零退出算失败，stop_command 主动停止不算，超时停止仍算失败；同 CommandResult.IsFailed
export function isCommandFailed(result: CommandState): boolean {
  if (result.status === "failed") return true
  return (
    result.exit_code !== null &&
    result.exit_code !== 0 &&
    result.stop_reason !== "stopped"
  )
}

// 去掉终端控制序列（颜色、光标移动），复制命令输出时用
export function stripAnsi(text: string): string {
  // 1. 移除 CSI 与由 BEL 或 ST 结束的 OSC，保留正文。
  return text.replace(terminalControlSequence, "")
}

// CSI 序列（颜色、光标移动）和 OSC 序列（窗口标题、超链接）
/* eslint-disable no-control-regex -- 要匹配的就是终端控制字符 */
const terminalControlSequence =
  /\u001b\[[0-9;?]*[ -/]*[@-~]|\u001b\][\s\S]*?(?:\u0007|\u001b\\)/g
/* eslint-enable no-control-regex */

// 按文件名选着色语言：取扩展名，没有扩展名时用文件名本身（Dockerfile、Makefile）
export function getFileLanguage(path: string): string {
  const name = path.split("/").at(-1)?.toLowerCase() ?? ""
  const dot = name.lastIndexOf(".")
  return dot > 0 ? name.slice(dot + 1) : name
}

// 值是普通对象时返回它的字段，否则返回 undefined
function getFields(value: unknown): Record<string, unknown> | undefined {
  return value && typeof value === "object" && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : undefined
}
