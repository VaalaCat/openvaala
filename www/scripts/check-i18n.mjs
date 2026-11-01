import { readdir, readFile } from "node:fs/promises"
import ts from "typescript"

const languages = ["zh-CN", "en"]
// 拼接出来的 key 只能用窄前缀：首个 * 之前至少两段，或 * 之后带固定后缀；声明的整族都算有人用
const dynamicKeys = []
const pluralSuffix = /_(zero|one|two|few|many|other)$/
function flatten(resource, prefix = "", entries = new Map()) {
  for (const [key, value] of Object.entries(resource)) {
    const path = prefix ? prefix + "." + key : key
    if (typeof value === "string") entries.set(path, value)
    else if (value && typeof value === "object" && !Array.isArray(value))
      flatten(value, path, entries)
    else throw new Error(path + ": expected a translation string or object")
  }
  return entries
}
const resources = new Map(
  await Promise.all(
    languages.map(async (language) => [
      language,
      flatten(
        JSON.parse(
          await readFile(
            new URL(
              "../src/i18n/locales/" + language + ".json",
              import.meta.url
            ),
            "utf8"
          )
        )
      ),
    ])
  )
)
const keys = new Set(
  [...resources.values()].flatMap((resource) =>
    [...resource.keys()].map((key) => key.replace(pluralSuffix, ""))
  )
)
const errors = []
for (const key of keys) {
  let expectedVariables
  const plural = [...resources.values()].some((resource) =>
    [...resource.keys()].some(
      (path) => path !== key && path.replace(pluralSuffix, "") === key
    )
  )
  for (const [language, resource] of resources) {
    const paths = plural
      ? new Intl.PluralRules(language)
          .resolvedOptions()
          .pluralCategories.map((category) => key + "_" + category)
      : [key]
    if (plural && resource.has(key + "_zero") && !paths.includes(key + "_zero"))
      paths.push(key + "_zero")
    for (const path of paths) {
      const text = resource.get(path)
      if (!text?.trim()) {
        errors.push(language + ": missing or empty " + path)
        continue
      }
      const variables = [
        ...new Set(
          [...text.matchAll(/{{-?\s*([^},\s]+)(?:[^}]*)}}/g)].map(
            (match) => match[1]
          )
        ),
      ]
        .sort()
        .join(",")
      expectedVariables ??= variables
      if (variables !== expectedVariables)
        errors.push(language + ": inconsistent interpolation in " + path)
    }
  }
}

// 哪些 key 有源码引用：完整的字符串字面量，或声明过的拼接模式（* 匹配一段）。
const sourceRoot = new URL("../src/", import.meta.url)
const skippedSource =
  /^(lib\/api\/protocol\.gen\.ts$|i18n\/locales\/)|\.test\.ts$|\.d\.ts$/
const namespaces = [...new Set([...keys].map((key) => key.split(".")[0]))]
const literals = new Set()
const templates = []
const sourceFiles = (await readdir(sourceRoot, { recursive: true }))
  .map((file) => file.replaceAll("\\", "/"))
  .filter((file) => /\.tsx?$/.test(file) && !skippedSource.test(file))
  .sort()
for (const file of sourceFiles) {
  const source = ts.createSourceFile(
    file,
    await readFile(new URL(file, sourceRoot), "utf8"),
    ts.ScriptTarget.Latest,
    true,
    file.endsWith("x") ? ts.ScriptKind.TSX : ts.ScriptKind.TS
  )
  const visit = (node) => {
    if (ts.isStringLiteral(node) || ts.isNoSubstitutionTemplateLiteral(node))
      literals.add(node.text)
    else if (ts.isTemplateExpression(node)) {
      const pattern = node.templateSpans.reduce(
        (text, span) => text + "*" + span.literal.text,
        node.head.text
      )
      if (namespaces.some((namespace) => pattern.startsWith(namespace + "."))) {
        const { line } = source.getLineAndCharacterOfPosition(
          node.getStart(source)
        )
        templates.push({ pattern, at: "src/" + file + ":" + (line + 1) })
      }
    }
    ts.forEachChild(node, visit)
  }
  visit(source)
}
const isNarrow = (pattern) => {
  const wildcard = pattern.indexOf("*")
  return (
    pattern.slice(0, wildcard).split(".").filter(Boolean).length >= 2 ||
    pattern.slice(wildcard + 1).replaceAll(/[.*]/g, "") !== ""
  )
}
const families = dynamicKeys.map(
  (pattern) =>
    new RegExp(
      "^" +
        pattern
          .split("*")
          .map((part) => part.replaceAll(/[.+?^${}()|[\]\\]/g, "\\$&"))
          .join("[^.]+") +
        "$"
    )
)
const found = new Set(templates.map(({ pattern }) => pattern))
for (const { pattern, at } of templates)
  if (!dynamicKeys.includes(pattern))
    errors.push("undeclared dynamic key " + pattern + " at " + at)
for (const pattern of new Set([...found, ...dynamicKeys]))
  if (!isNarrow(pattern))
    errors.push("dynamic key pattern too broad: " + pattern)
for (const pattern of dynamicKeys)
  if (!found.has(pattern))
    errors.push("stale dynamic key declaration: " + pattern)
for (const key of keys)
  if (!literals.has(key) && !families.some((family) => family.test(key)))
    errors.push("unused key: " + key)

if (errors.length) {
  console.error(errors.join("\n"))
  process.exitCode = 1
} else
  console.log(
    "i18n: " +
      languages.length +
      " languages, " +
      keys.size +
      " keys checked, " +
      dynamicKeys.length +
      " dynamic key families"
  )
