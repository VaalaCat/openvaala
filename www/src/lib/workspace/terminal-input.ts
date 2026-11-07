// TerminalKey 是窄屏按键条上会直接发出一串字节的按键；Ctrl 是修饰键，不在这里。
export type TerminalKey =
  | "esc"
  | "tab"
  | "up"
  | "down"
  | "left"
  | "right"
  | "enter"
  | "ctrl-c"
  | "ctrl-d"
  | "ctrl-z"
  | "home"
  | "end"

const cursorKeyLetters: Partial<Record<TerminalKey, string>> = {
  up: "A",
  down: "B",
  right: "C",
  left: "D",
  home: "H",
  end: "F",
}

const controlKeySequences: Partial<Record<TerminalKey, string>> = {
  esc: "\x1b",
  tab: "\t",
  enter: "\r",
  "ctrl-c": "\x03",
  "ctrl-d": "\x04",
  "ctrl-z": "\x1a",
}

// 返回 Ctrl 加上这个字符对应的控制字节；不是单个可加 Ctrl 的字符（中文、粘贴的整段文字）时返回 undefined
export function getControlCharacter(value: string): string | undefined {
  if (value.length !== 1) return undefined
  const upper = value.toUpperCase()
  if (upper === " ") return "\0"
  if (upper === "?") return "\x7f"
  const code = upper.charCodeAt(0)
  return code >= 64 && code <= 95 ? String.fromCharCode(code & 31) : undefined
}

// 返回按键条上一个按键要发出的字节序列；光标键和 Home、End 在应用光标模式（vim、less）下前缀不同
export function getTerminalKeySequence(
  key: TerminalKey,
  isApplicationCursorMode: boolean
): string {
  const cursorLetter = cursorKeyLetters[key]
  if (cursorLetter)
    return "\x1b" + (isApplicationCursorMode ? "O" : "[") + cursorLetter
  return controlKeySequences[key]!
}
