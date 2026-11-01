// 代码高亮配色贴合站点的暖灰纸面：关键字用信号蓝的深浅变体，字符串偏橄榄，常量偏琥珀，
// 函数、类型和配置键偏青；注释和标点退到弱墨色。浅色版在白底、深色版在 #22201e 上都满足 AA 对比度。
interface CodePalette {
  foreground: string
  background: string
  comment: string
  punctuation: string
  keyword: string
  string: string
  constant: string
  entity: string
  variable: string
  invalid: string
}

function createCodeTheme(name: string, type: "light" | "dark", palette: CodePalette) {
  return {
    name,
    type,
    colors: {
      "editor.background": palette.background,
      "editor.foreground": palette.foreground,
    },
    tokenColors: [
      { settings: { foreground: palette.foreground } },
      {
        scope: ["comment", "punctuation.definition.comment", "string.comment"],
        settings: { foreground: palette.comment, fontStyle: "italic" },
      },
      {
        scope: [
          "punctuation",
          "meta.brace",
          "keyword.operator",
          "punctuation.separator",
          "punctuation.terminator",
          "punctuation.definition.tag",
        ],
        settings: { foreground: palette.punctuation },
      },
      {
        scope: [
          "keyword",
          "storage",
          "storage.type",
          "storage.modifier",
          "keyword.control",
          "keyword.operator.new",
          "keyword.operator.expression",
          "keyword.operator.logical",
          "support.function.builtin.shell",
          "entity.name.tag",
          "markup.heading",
        ],
        settings: { foreground: palette.keyword },
      },
      {
        scope: [
          "string",
          "string.quoted",
          "string.template",
          "punctuation.definition.string",
          "markup.inline.raw",
          "string.unquoted.plain.out.yaml",
        ],
        settings: { foreground: palette.string },
      },
      {
        scope: [
          "constant",
          "constant.numeric",
          "constant.language",
          "constant.character.escape",
          "support.constant",
          "variable.language",
          "constant.other.option",
        ],
        settings: { foreground: palette.constant },
      },
      {
        scope: [
          "entity.name.function",
          "support.function",
          "meta.function-call entity.name.function",
          "entity.name.type",
          "entity.name.class",
          "support.type",
          "support.class",
          "entity.other.attribute-name",
          "support.type.property-name",
          "entity.name.tag.yaml",
          "meta.object-literal.key",
          "entity.name.command",
          "markup.underline.link",
        ],
        settings: { foreground: palette.entity },
      },
      {
        scope: ["variable", "variable.other", "variable.parameter", "meta.definition.variable"],
        settings: { foreground: palette.variable },
      },
      { scope: ["markup.bold"], settings: { fontStyle: "bold" } },
      { scope: ["markup.italic"], settings: { fontStyle: "italic" } },
      { scope: ["markup.inserted"], settings: { foreground: palette.string } },
      { scope: ["markup.deleted", "invalid"], settings: { foreground: palette.invalid } },
    ],
  }
}

export const codeThemeLight = createCodeTheme("openvaala-paper", "light", {
  foreground: "#1c1917",
  background: "#ffffff",
  comment: "#78716c",
  punctuation: "#57534e",
  keyword: "#1d4ed8",
  string: "#4d7c0f",
  constant: "#b45309",
  entity: "#0f766e",
  variable: "#292524",
  invalid: "#b91c1c",
})

export const codeThemeDark = createCodeTheme("openvaala-ink", "dark", {
  foreground: "#e7e5e4",
  background: "#22201e",
  comment: "#a8a29e",
  punctuation: "#a8a29e",
  keyword: "#93c5fd",
  string: "#b5cf8f",
  constant: "#f2b872",
  entity: "#7fd4c1",
  variable: "#e7e5e4",
  invalid: "#fca5a5",
})
