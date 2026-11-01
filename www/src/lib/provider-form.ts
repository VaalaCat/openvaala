import type { ParseKeys } from "i18next"
import type {
  ModelDefaults,
  ProviderConfig,
  ProviderModelConfig,
} from "./api/protocol.gen"
import { parseNumberUnit, type NumberUnitError } from "./number-unit"

// ProviderModelDraft 保存容量和推理强度输入原文，未设置的可选字段沿用默认。
export type ProviderModelDraft = Omit<
  ProviderModelConfig,
  "context_window" | "max_output_tokens" | "reasoning_efforts"
> & {
  context_window: string
  max_output_tokens: string
  reasoning_efforts?: string
}

// ProviderDraft 保存整页输入；models 中的稳定行 ID 对应 modelAliases 中的别名原文。
export type ProviderDraft = {
  id: string
  protocol: string
  baseURL: string
  apiKey: string
  models: string[]
  modelAliases: Record<string, string>
  modelConfigs: Record<string, ProviderModelDraft>
  endpoints: Record<string, string>
}

export type ProviderModelIssues = {
  alias?: "required" | "invalid" | "duplicate"
  context_window?: NumberUnitError
  max_output_tokens?: NumberUnitError
  reasoning_efforts?: "empty" | "duplicate"
  default_reasoning_effort?: "unavailable"
}

// ProviderModelGroup 是拉取模型弹窗里的一组模型；name 为空的是「其他」
export type ProviderModelGroup = { name: string; modelIDs: string[] }

// 表单可选的协议，按显示顺序；新服务默认第一个，和 TUI 一致
export const providerProtocols = [
  "openai_chat",
  "openai_responses",
  "claude",
] as const
export type ProviderProtocol = (typeof providerProtocols)[number]

// 协议的显示名
export const providerProtocolLabelKeys = {
  openai_chat: "settings.providerEditor.protocols.openaiChat",
  openai_responses: "settings.providerEditor.protocols.openaiResponses",
  claude: "settings.providerEditor.protocols.claude",
} as const satisfies Record<ProviderProtocol, ParseKeys>

// 是表单认识的协议吗？
export function isProviderProtocol(
  protocol: string
): protocol is ProviderProtocol {
  return (providerProtocols as readonly string[]).includes(protocol)
}

// Provider 文件没写 protocol 时 serve 用的协议
export const omittedProviderProtocol = "openai_responses"

// 新服务的初始配置
export const newProviderConfig: ProviderConfig = {
  id: "",
  protocol: providerProtocols[0],
  base_url: "",
  api_key: "",
  models: null,
}

// 将模型配置转成保留输入原文的草稿。
export function fromProviderModelConfig(
  config: ProviderModelConfig | null
): ProviderModelDraft {
  // 1. 数值转十进制原文，可选列表保留继承与显式空列表。
  const { context_window, max_output_tokens, reasoning_efforts, ...fields } =
    config ?? {}
  return {
    ...structuredClone(fields),
    context_window: context_window ? String(context_window) : "",
    max_output_tokens: max_output_tokens ? String(max_output_tokens) : "",
    ...(reasoning_efforts != null && {
      reasoning_efforts: reasoning_efforts.join(", "),
    }),
  }
}

// 读出整页草稿，模型按稳定行 ID 排序。
export function fromProviderConfig(config: ProviderConfig): ProviderDraft {
  // 1. 每个模型只有一份字段草稿，别名从配置 key 读取。
  const models = Object.keys(config.models ?? {}).sort()
  return {
    id: config.id,
    protocol: config.protocol || omittedProviderProtocol,
    baseURL: config.base_url,
    apiKey: config.api_key,
    models,
    modelAliases: Object.fromEntries(models.map((id) => [id, id])),
    modelConfigs: Object.fromEntries(
      models.map((id) => [id, fromProviderModelConfig(config.models![id])])
    ),
    endpoints: { ...config.endpoints },
  }
}

// 解析逗号分隔的自定义推理强度，保留词的顺序。
export function parseReasoningEfforts(
  raw: string
):
  { ok: true; values: string[] } | { ok: false; error: "empty" | "duplicate" } {
  // 1. 空原文表示显式空列表，其他输入逐项检查。
  if (!raw.trim()) return { ok: true, values: [] }
  const values = raw.split(",").map((value) => value.trim())
  if (values.some((value) => !value)) return { ok: false, error: "empty" }
  if (new Set(values).size !== values.length)
    return { ok: false, error: "duplicate" }
  return { ok: true, values }
}

// 读取模型当前有效的推理强度列表和自动默认值。
export function getProviderModelReasoning(
  model: ProviderModelDraft,
  defaults: ModelDefaults
) {
  // 1. 显式列表覆盖默认列表，自动值与当前有效列表匹配。
  const parsed =
    model.reasoning_efforts === undefined
      ? undefined
      : parseReasoningEfforts(model.reasoning_efforts)
  const efforts = parsed
    ? parsed.ok
      ? parsed.values
      : []
    : (defaults.reasoning_efforts ?? [])
  const automatic = efforts.includes(defaults.default_reasoning_effort)
    ? defaults.default_reasoning_effort
    : (efforts[0] ?? "")
  return { efforts, automatic }
}

// 检查整页模型的别名、容量和推理输入。
export function getProviderModelIssues(
  draft: ProviderDraft,
  defaults: ModelDefaults
): Record<string, ProviderModelIssues> {
  // 1. 对稳定行逐项检查，错误仍关联原行。
  const issues: Record<string, ProviderModelIssues> = {}
  const aliases = draft.models.map((id) => draft.modelAliases[id].trim())
  for (const id of draft.models) {
    const model = draft.modelConfigs[id]
    const alias = draft.modelAliases[id].trim()
    const issue: ProviderModelIssues = {}
    if (!alias) issue.alias = "required"
    else if (getProviderIDIssue(alias, []) === "invalid")
      issue.alias = "invalid"
    else if (aliases.filter((value) => value === alias).length > 1)
      issue.alias = "duplicate"
    for (const field of ["context_window", "max_output_tokens"] as const) {
      const result = parseNumberUnit(model[field])
      if (!result.ok) issue[field] = result.error
    }
    if (model.reasoning_efforts !== undefined) {
      const result = parseReasoningEfforts(model.reasoning_efforts)
      if (!result.ok) issue.reasoning_efforts = result.error
    }
    if (
      model.default_reasoning_effort &&
      !getProviderModelReasoning(model, defaults).efforts.includes(
        model.default_reasoning_effort
      )
    )
      issue.default_reasoning_effort = "unavailable"
    if (Object.keys(issue).length) issues[id] = issue
  }
  return issues
}

// 将已校验的草稿写成完整 Provider，容量空白和 0 沿用默认。
export function toProviderConfig(draft: ProviderDraft): ProviderConfig {
  // 1. 按提交时的别名写出全部模型字段。
  const models: Record<string, ProviderModelConfig> = {}
  for (const id of draft.models) {
    const { context_window, max_output_tokens, reasoning_efforts, ...fields } =
      draft.modelConfigs[id]
    const context = parseNumberUnit(context_window)
    const output = parseNumberUnit(max_output_tokens)
    const efforts =
      reasoning_efforts === undefined
        ? undefined
        : parseReasoningEfforts(reasoning_efforts)
    if (!context.ok || !output.ok || (efforts && !efforts.ok))
      throw new Error("Invalid model draft")
    models[draft.modelAliases[id].trim()] = {
      ...structuredClone(fields),
      ...(context.value && { context_window: context.value }),
      ...(output.value && { max_output_tokens: output.value }),
      ...(efforts?.ok && { reasoning_efforts: efforts.values }),
    }
  }
  // 2. 协议地址留空时不覆盖 Base URL。
  const endpoints = Object.fromEntries(
    Object.entries(draft.endpoints)
      .map(([protocol, url]) => [protocol, url.trim()])
      .filter(([, url]) => url)
  )
  return {
    id: draft.id.trim(),
    ...(draft.protocol.trim() && { protocol: draft.protocol.trim() }),
    base_url: draft.baseURL.trim(),
    ...(Object.keys(endpoints).length && { endpoints }),
    api_key: draft.apiKey.trim(),
    models,
  }
}

// 两份草稿的输入原文是否相同？
export function isSameProviderDraft(a: ProviderDraft, b: ProviderDraft) {
  // 1. 只比较仍在列表的模型，保留非法输入和空白的差异。
  const comparable = (draft: ProviderDraft) => ({
    ...draft,
    models: [...draft.models].sort(),
    modelAliases: draft.models
      .slice()
      .sort()
      .map((id) => [id, draft.modelAliases[id]]),
    modelConfigs: draft.models
      .slice()
      .sort()
      .map((id) => [id, draft.modelConfigs[id]]),
    endpoints: Object.entries(draft.endpoints)
      .filter(([, value]) => value !== "")
      .sort(([a], [b]) => a.localeCompare(b)),
  })
  return JSON.stringify(comparable(a)) === JSON.stringify(comparable(b))
}

// 修改本地别名，未显式配置上游名时保留修改前实际请求的模型名。
export function changeProviderModelAlias(
  draft: ProviderDraft,
  modelID: string,
  alias: string
): ProviderDraft {
  // 1. 别名原文与完整模型字段仍保存在同一份草稿。
  const model = draft.modelConfigs[modelID]
  const previousAlias = draft.modelAliases[modelID].trim()
  const preserveUpstream =
    !model.upstream_model && alias.trim() !== previousAlias
  return {
    ...draft,
    modelAliases: { ...draft.modelAliases, [modelID]: alias },
    modelConfigs: preserveUpstream
      ? {
          ...draft.modelConfigs,
          [modelID]: { ...model, upstream_model: previousAlias },
        }
      : draft.modelConfigs,
  }
}

// 将手动模型 ID 加进草稿，已有别名保留原字段。
export function addManualModels(
  draft: ProviderDraft,
  text: string
): ProviderDraft {
  // 1. 新上游模型分配本地别名，转换字符冲突时追加序号。
  const models = [...draft.models]
  const modelAliases = { ...draft.modelAliases }
  const modelConfigs = { ...draft.modelConfigs }
  for (const upstreamModel of text.split(/[\s,]+/).filter(Boolean)) {
    if (
      models.some(
        (id) =>
          modelConfigs[id].upstream_model === upstreamModel ||
          modelAliases[id].trim() === upstreamModel
      )
    )
      continue
    const removedModelID = Object.keys(modelConfigs).find(
      (id) =>
        !models.includes(id) &&
        (modelAliases[id].trim() === upstreamModel ||
          modelConfigs[id].upstream_model === upstreamModel)
    )
    if (removedModelID) {
      models.push(removedModelID)
      continue
    }
    const base = upstreamModel.replace(/[^A-Za-z0-9._-]/g, "-")
    let alias = base
    for (
      let suffix = 2;
      models.some((id) => modelAliases[id].trim() === alias);
      suffix++
    )
      alias = base + "-" + suffix
    let id = alias
    for (let suffix = 2; id in modelConfigs; suffix++) id = alias + "-" + suffix
    models.push(id)
    modelAliases[id] = alias
    modelConfigs[id] = fromProviderModelConfig(
      alias === upstreamModel ? {} : { upstream_model: upstreamModel }
    )
  }
  return { ...draft, models: models.sort(), modelAliases, modelConfigs }
}

// 合并拉取选择；已配置模型保留整份草稿，新增模型经过统一转换。
export function applyFetchedModelSelection(
  draft: ProviderDraft,
  fetched: Record<string, ProviderModelConfig | null>,
  selected: string[]
): ProviderDraft {
  // 1. 拉取结果按稳定行 ID 关联已有模型，未出现在结果里的手动模型不变。
  const selectedIDs = new Set(selected)
  const models = draft.models.filter(
    (id) => !(id in fetched) || selectedIDs.has(id)
  )
  const modelAliases = { ...draft.modelAliases }
  const modelConfigs = { ...draft.modelConfigs }
  for (const id of selectedIDs) {
    if (models.includes(id)) continue
    models.push(id)
    if (!(id in modelConfigs)) {
      modelAliases[id] = id
      modelConfigs[id] = fromProviderModelConfig(fetched[id] ?? null)
    }
  }
  return { ...draft, models: models.sort(), modelAliases, modelConfigs }
}

// 按厂商前缀给模型分组：上游 id 有 / 时取 / 前面一段，否则取第一个 - 前面一段；
// 组按名字排序，只有一个模型的组并进最后的「其他」
export function groupProviderModels(
  models: Record<string, ProviderModelConfig | null>
): ProviderModelGroup[] {
  const modelIDsByGroup = new Map<string, string[]>()
  for (const modelID of Object.keys(models).sort()) {
    const name = getModelGroupName(models[modelID]?.upstream_model || modelID)
    modelIDsByGroup.set(name, [...(modelIDsByGroup.get(name) ?? []), modelID])
  }
  const groups: ProviderModelGroup[] = []
  const otherModelIDs: string[] = []
  for (const name of [...modelIDsByGroup.keys()].sort()) {
    const modelIDs = modelIDsByGroup.get(name)!
    if (modelIDs.length > 1) groups.push({ name, modelIDs })
    else otherModelIDs.push(...modelIDs)
  }
  if (otherModelIDs.length > 0)
    groups.push({ name: "", modelIDs: otherModelIDs.sort() })
  return groups
}

function getModelGroupName(upstreamModel: string): string {
  const slash = upstreamModel.indexOf("/")
  return (
    slash > 0 ? upstreamModel.slice(0, slash) : upstreamModel.split("-")[0]
  ).toLowerCase()
}

// 新服务 ID 的问题：只能用字母、数字、-、_、.（同 Go 的 isValidID），不能和已有的重名；空 ID 交给必填校验
export function getProviderIDIssue(
  id: string,
  existingIDs: string[]
): "invalid" | "duplicate" | undefined {
  const providerID = id.trim()
  if (!providerID) return undefined
  if (
    providerID === "." ||
    providerID === ".." ||
    !/^[A-Za-z0-9._-]+$/.test(providerID)
  )
    return "invalid"
  return existingIDs.includes(providerID) ? "duplicate" : undefined
}
