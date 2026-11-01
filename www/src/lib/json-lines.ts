// JsonView 自己排版 JSON：每行记下对应的节点和能折叠到哪一行，着色交给 shiki 后按行号对回来

// 节点在值里的位置：对象用键，数组用下标；按 JSON 展开的字符串，子节点接着往下写
export type JsonPath = (string | number)[]

// 排好的一行；text 带两格缩进，所有行用换行拼起来就是交给 shiki 的文本
export interface JsonLine {
  text: string
  depth: number
  // 这一行对应的节点；对象或数组的收尾行与开头行相同
  path: JsonPath
  // 这一行对应的原节点值，按 JSON 展开的字符串仍保留原字符串
  value: unknown
  // 能折叠时，对应的收尾行号
  foldEnd?: number
  // 能折叠时的子项数，组件按 i18n 显示为「12 项」
  childCount?: number
  // 这一行是不是对象或数组的收尾行（`}`、`]`）？
  isClosing?: boolean
  // 字符串值本身是不是 JSON 对象或数组？展开后它的开头行仍为 true
  isNestedJSON?: boolean
  // 字符串是不是超过 maxJsonStringLength 被截断了？
  isTruncated?: boolean
}

export interface JsonFormatOptions {
  // 按 JSON 展开的字符串，值为 getJsonPathKey
  expandedNestedPaths?: Set<string>
  // 显示全文的长字符串，值为 getJsonPathKey
  expandedStringPaths?: Set<string>
}

// 单个字符串超过这个长度时只显示开头
export const maxJsonStringLength = 2000

// 把值排成行；对象里值为 undefined 的字段和 JSON.stringify 一样跳过
export function formatJsonLines(
  value: unknown,
  options: JsonFormatOptions
): JsonLine[] {
  const lines: JsonLine[] = []
  appendJsonNode(lines, options, {
    value,
    path: [],
    depth: 0,
    keyPrefix: "",
    trailingComma: "",
  })
  return lines
}

// 要显示的一行：lineIndex 是它在 formatJsonLines 结果里的行号；isFolded 时这个节点折叠成这一行
export interface JsonRow {
  lineIndex: number
  isFolded: boolean
}

// 按折叠状态挑出要显示的行：层级不小于 defaultExpandDepth 的对象和数组默认折叠，按 JSON 展开的字符串默认展开，
// toggledFoldPaths 里的节点与默认相反；折叠的节点只显示开头行，接着从收尾行之后继续
export function getVisibleJsonRows(
  lines: JsonLine[],
  defaultExpandDepth: number,
  toggledFoldPaths: Set<string>
): JsonRow[] {
  const rows: JsonRow[] = []
  for (let lineIndex = 0; lineIndex < lines.length; lineIndex++) {
    const { depth, path, foldEnd, isNestedJSON } = lines[lineIndex]
    const isFoldedByDefault = depth >= defaultExpandDepth && !isNestedJSON
    const isFolded =
      foldEnd !== undefined &&
      isFoldedByDefault !== toggledFoldPaths.has(getJsonPathKey(path))
    rows.push({ lineIndex, isFolded })
    if (isFolded) lineIndex = foldEnd
  }
  return rows
}

// 路径的字符串形式，用作展开、折叠状态的键
export function getJsonPathKey(path: JsonPath): string {
  return JSON.stringify(path)
}

// 复制节点时的文本：字符串取原文，其余按两格缩进的 JSON
export function getJsonCopyText(value: unknown): string {
  if (typeof value === "string") return value
  return JSON.stringify(value, null, 2) ?? String(value)
}

// 一个待排版的节点
interface JsonNode {
  value: unknown
  path: JsonPath
  depth: number
  // 行首的 `"名字": `，数组元素和根节点为空
  keyPrefix: string
  // 后面还有兄弟节点时为 ","
  trailingComma: string
}

// 按值的类型排一个节点：容器排开头行、子项和收尾行，字符串按需展开或截断，其余写成一行
function appendJsonNode(
  lines: JsonLine[],
  options: JsonFormatOptions,
  node: JsonNode
): void {
  const { value, path, depth, keyPrefix, trailingComma } = node
  const indent = "  ".repeat(depth)
  if (value !== null && typeof value === "object") {
    appendJsonContainer(lines, options, node, value, false)
    return
  }
  if (typeof value !== "string") {
    lines.push({
      text:
        indent +
        keyPrefix +
        (JSON.stringify(value) ?? String(value)) +
        trailingComma,
      depth,
      path,
      value,
    })
    return
  }
  // 1. 能解析成对象或数组的字符串：展开时按容器排版
  const pathKey = getJsonPathKey(path)
  const nested = parseNestedJSON(value)
  if (nested && options.expandedNestedPaths?.has(pathKey)) {
    appendJsonContainer(lines, options, node, nested, true)
    return
  }
  // 2. 其余字符串写成一行，超长且没要求看全文时截断
  const isTruncated =
    value.length > maxJsonStringLength &&
    !options.expandedStringPaths?.has(pathKey)
  const text = isTruncated
    ? JSON.stringify(value.slice(0, maxJsonStringLength)).slice(0, -1) + '…"'
    : JSON.stringify(value)
  lines.push({
    text: indent + keyPrefix + text + trailingComma,
    depth,
    path,
    value,
    ...(nested && { isNestedJSON: true }),
    ...(isTruncated && { isTruncated: true }),
  })
}

// 排一个对象或数组：空容器写成一行；否则开头行记下子项数和收尾行号
function appendJsonContainer(
  lines: JsonLine[],
  options: JsonFormatOptions,
  node: JsonNode,
  container: object,
  isNestedJSON: boolean
): void {
  const { path, depth, keyPrefix, trailingComma } = node
  const indent = "  ".repeat(depth)
  const isArray = Array.isArray(container)
  const entries: [string | number, unknown][] = isArray
    ? container.map((child, index) => [index, child])
    : Object.entries(container).filter(([, child]) => child !== undefined)
  const [open, close] = isArray ? ["[", "]"] : ["{", "}"]
  if (entries.length === 0) {
    lines.push({
      text: indent + keyPrefix + open + close + trailingComma,
      depth,
      path,
      value: node.value,
      ...(isNestedJSON && { isNestedJSON: true }),
    })
    return
  }
  const openLine: JsonLine = {
    text: indent + keyPrefix + open,
    depth,
    path,
    value: node.value,
    childCount: entries.length,
    ...(isNestedJSON && { isNestedJSON: true }),
  }
  lines.push(openLine)
  entries.forEach(([childKey, child], index) =>
    appendJsonNode(lines, options, {
      value: child,
      path: [...path, childKey],
      depth: depth + 1,
      keyPrefix: isArray ? "" : JSON.stringify(childKey) + ": ",
      trailingComma: index < entries.length - 1 ? "," : "",
    })
  )
  openLine.foldEnd = lines.length
  lines.push({
    text: indent + close + trailingComma,
    depth,
    path,
    value: node.value,
    isClosing: true,
  })
}

// 字符串本身是 JSON 对象或数组时返回解析结果
function parseNestedJSON(text: string): object | undefined {
  const first = text.trimStart()[0]
  if (first !== "{" && first !== "[") return undefined
  try {
    const parsed: unknown = JSON.parse(text)
    return parsed !== null && typeof parsed === "object" ? parsed : undefined
  } catch {
    return undefined
  }
}
