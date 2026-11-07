// 输入框的按键和斜杠补全规则

// 输入框里按下的键：Enter 发送、Shift+Enter 换行；主要输入是触屏时（软键盘没有 Shift）Enter 只换行，
// 输入法组字中（Safari 只给 keyCode 229）的 Enter 不发送
export function shouldSubmitOnKeyDown(event: {
  key: string
  shiftKey: boolean
  isComposing: boolean
  keyCode: number
  isTouchPrimary: boolean
}): boolean {
  return (
    event.key === "Enter" &&
    !event.shiftKey &&
    !event.isComposing &&
    event.keyCode !== 229 &&
    !event.isTouchPrimary
  )
}

// SlashToken 是光标所在、以 / 开头的词：在全文中的起止位置，以及 / 到光标之间已经输入的部分
export interface SlashToken {
  start: number
  end: number
  prefix: string
}

// 找光标所在、以 / 开头的词；光标在词首或这个词不以 / 开头时没有。同 TUI 的 getSlashToken
export function getSlashToken(
  text: string,
  cursor: number
): SlashToken | undefined {
  // 1. 从光标向两侧找到空白为止，定出词的范围。
  let start = cursor
  let end = cursor
  while (start > 0 && !/\s/.test(text[start - 1])) start--
  while (end < text.length && !/\s/.test(text[end])) end++
  // 2. 光标在词首，或词不以 / 开头时不补全。
  if (start === cursor || text[start] !== "/") return undefined
  return { start, end, prefix: text.slice(start + 1, cursor) }
}

// 输入以 /btw 命令开头（后面是空白或结束）时返回命令后面的问题，可以为空；其他输入返回 undefined
export function getSideConversationQuestion(text: string): string | undefined {
  return /^\/btw(\s|$)/.test(text)
    ? text.slice("/btw".length).trim()
    : undefined
}
