function copyWithSelection(value: string) {
  const focused = document.activeElement
  const selection = document.getSelection()
  const ranges = selection
    ? Array.from({ length: selection.rangeCount }, (_, index) =>
        selection.getRangeAt(index).cloneRange()
      )
    : []
  const textarea = document.createElement("textarea")
  textarea.value = value
  textarea.readOnly = true
  textarea.tabIndex = -1
  textarea.className = "text-body fixed top-0 left-0 size-px opacity-0"
  // 保留在当前弹窗内，避免焦点陷阱阻止临时文本框选中。
  const container = focused?.parentElement ?? document.body
  container.append(textarea)
  try {
    textarea.focus({ preventScroll: true })
    textarea.select()
    textarea.setSelectionRange(0, value.length)
    if (!document.execCommand("copy")) throw new Error()
  } finally {
    textarea.remove()
    if (focused instanceof HTMLElement) focused.focus({ preventScroll: true })
    if (selection) {
      selection.removeAllRanges()
      ranges.forEach((range) => selection.addRange(range))
    }
  }
}

// 先用 navigator.clipboard，不可用或被拒绝时退回临时文本框；两条路都失败则抛出
export async function copyText(value: string): Promise<void> {
  if (navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(value)
      return
    } catch {
      // 退回临时文本框
    }
  }
  copyWithSelection(value)
}
