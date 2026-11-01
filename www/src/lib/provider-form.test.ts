import assert from "node:assert/strict"
import { test } from "node:test"
import type {
  ModelDefaults,
  ProviderConfig,
  ProviderModelConfig,
} from "./api/protocol.gen"
import {
  addManualModels,
  applyFetchedModelSelection,
  changeProviderModelAlias,
  fromProviderConfig,
  fromProviderModelConfig,
  getProviderIDIssue,
  getProviderModelIssues,
  getProviderModelReasoning,
  groupProviderModels,
  isSameProviderDraft,
  parseReasoningEfforts,
  toProviderConfig,
} from "./provider-form"

const defaults: ModelDefaults = {
  context_window: 256000,
  max_output_tokens: 32000,
  thinking: true,
  reasoning_efforts: ["low", "high"],
  default_reasoning_effort: "bespoke",
  merge_message_roles: ["user"],
  capabilities: { input: ["text"] },
}
function config(
  models: Record<string, ProviderModelConfig | null> = {}
): ProviderConfig {
  return {
    id: "fake",
    protocol: "openai_chat",
    base_url: "https://api.example.com/v1",
    api_key: "sk-test",
    models,
  }
}

test("model fields round trip with inheritance, false and explicit empty lists", () => {
  const cases: {
    name: string
    model: ProviderModelConfig | null
    want: ProviderModelConfig
  }[] = [
    { name: "nil_model_inherits", model: null, want: {} },
    {
      name: "false_and_empty_lists_are_explicit",
      model: {
        thinking: false,
        reasoning_efforts: [],
        merge_message_roles: [],
        capabilities: { input: [] },
      },
      want: {
        thinking: false,
        reasoning_efforts: [],
        merge_message_roles: [],
        capabilities: { input: [] },
      },
    },
    {
      name: "all_fields",
      model: {
        protocol: "claude",
        upstream_model: "vendor/model",
        display_name: "Model",
        context_window: 128000,
        max_output_tokens: 1500,
        thinking: true,
        reasoning_efforts: ["bespoke", "other"],
        default_reasoning_effort: "bespoke",
        merge_message_roles: ["system", "developer", "user", "assistant"],
        capabilities: { input: ["text", "image", "audio", "video", "pdf"] },
      },
      want: {
        protocol: "claude",
        upstream_model: "vendor/model",
        display_name: "Model",
        context_window: 128000,
        max_output_tokens: 1500,
        thinking: true,
        reasoning_efforts: ["bespoke", "other"],
        default_reasoning_effort: "bespoke",
        merge_message_roles: ["system", "developer", "user", "assistant"],
        capabilities: { input: ["text", "image", "audio", "video", "pdf"] },
      },
    },
  ]
  for (const each of cases) {
    const draft = fromProviderConfig(config({ main: each.model }))
    assert.deepEqual(toProviderConfig(draft).models?.main, each.want, each.name)
    assert.equal(
      draft.modelConfigs.main.reasoning_efforts,
      each.model?.reasoning_efforts?.join(", "),
      each.name
    )
  }
})

test("capacity input keeps raw text until submit and invalid input stays dirty", () => {
  const baseline = fromProviderConfig(config({ main: null }))
  const cases = [
    { raw: "1.5K", value: 1500 },
    { raw: "1M", value: 1000000 },
    { raw: "0", value: undefined },
    { raw: " ", value: undefined },
    { raw: "-1", error: "negative" },
    { raw: "12KK", error: "invalid" },
    { raw: "1.0001K", error: "fractional" },
    { raw: "9007199254740992", error: "unsafe" },
  ]
  for (const each of cases) {
    const draft = structuredClone(baseline)
    draft.modelConfigs.main.context_window = each.raw
    assert.equal(isSameProviderDraft(draft, baseline), false, each.raw)
    assert.equal(draft.modelConfigs.main.context_window, each.raw)
    if (each.error) {
      assert.equal(
        getProviderModelIssues(draft, defaults).main.context_window,
        each.error
      )
      assert.throws(() => toProviderConfig(draft))
    } else
      assert.equal(
        toProviderConfig(draft).models?.main?.context_window,
        each.value
      )
  }
})

test("reasoning lists preserve arbitrary words and choose automatic from the effective list", () => {
  const cases = [
    { raw: undefined, efforts: ["low", "high"], automatic: "low" },
    {
      raw: "other, bespoke",
      efforts: ["other", "bespoke"],
      automatic: "bespoke",
    },
    { raw: "first, second", efforts: ["first", "second"], automatic: "first" },
    { raw: "", efforts: [], automatic: "" },
  ]
  for (const each of cases) {
    const model = {
      ...fromProviderModelConfig(null),
      reasoning_efforts: each.raw,
    }
    assert.deepEqual(getProviderModelReasoning(model, defaults), {
      efforts: each.efforts,
      automatic: each.automatic,
    })
  }
  for (const each of [
    { raw: "one, ,two", error: "empty" },
    { raw: "one,one ", error: "duplicate" },
  ])
    assert.deepEqual(parseReasoningEfforts(each.raw), {
      ok: false,
      error: each.error,
    })
})

test("alias edits retain every field, detect conflicts and serialize the new key", () => {
  const draft = fromProviderConfig(
    config({
      main: {
        thinking: false,
        capabilities: { input: [] },
        merge_message_roles: [],
        reasoning_efforts: ["bespoke"],
        default_reasoning_effort: "bespoke",
        display_name: "Main",
      },
      other: null,
    })
  )
  for (const each of [
    { alias: "other", issue: "duplicate" },
    { alias: "a/b", issue: "invalid" },
    { alias: "", issue: "required" },
  ]) {
    draft.modelAliases.main = each.alias
    assert.equal(getProviderModelIssues(draft, defaults).main.alias, each.issue)
  }
  draft.modelAliases.main = " renamed "
  const saved = toProviderConfig(draft)
  assert.equal(saved.models?.main, undefined)
  assert.deepEqual(saved.models?.renamed, {
    thinking: false,
    capabilities: { input: [] },
    merge_message_roles: [],
    reasoning_efforts: ["bespoke"],
    default_reasoning_effort: "bespoke",
    display_name: "Main",
  })
})

test("editing only a local alias keeps the effective upstream model", () => {
  for (const each of [
    { model: null, upstream: "main" },
    { model: { upstream_model: "vendor/main" }, upstream: "vendor/main" },
  ]) {
    const draft = fromProviderConfig(config({ main: each.model }))
    const renamed = changeProviderModelAlias(draft, "main", "friendly")
    const saved = toProviderConfig(renamed)
    assert.equal(saved.models?.friendly?.upstream_model, each.upstream)
    assert.equal(
      fromProviderConfig(saved).modelConfigs.friendly.upstream_model,
      each.upstream
    )
    assert.equal(draft.modelAliases.main, "main")
  }
})

test("fetch selection preserves configured advanced fields and invalid raw input", () => {
  const draft = fromProviderConfig(
    config({
      main: {
        thinking: false,
        reasoning_efforts: [],
        capabilities: { input: [] },
      },
      manual: null,
      remove: null,
    })
  )
  draft.modelConfigs.main.context_window = "1."
  draft.modelAliases.main = "renamed"
  const next = applyFetchedModelSelection(
    draft,
    {
      main: { thinking: true, context_window: 128000 },
      new: { context_window: 64000 },
      remove: null,
    },
    ["main", "new"]
  )
  assert.deepEqual(next.models, ["main", "manual", "new"])
  assert.deepEqual(next.modelConfigs.main, draft.modelConfigs.main)
  assert.equal(next.modelAliases.main, "renamed")
  assert.equal(next.modelConfigs.new.context_window, "64000")
})

test("manual models retain upstream IDs and allocate distinct aliases", () => {
  const cases = [
    { text: "a, b a", aliases: ["a", "b"], upstream: undefined },
    {
      text: "vendor/model",
      aliases: ["vendor-model"],
      upstream: "vendor/model",
    },
    {
      text: "vendor-model vendor/model",
      aliases: ["vendor-model", "vendor-model-2"],
      upstream: undefined,
    },
  ]
  for (const each of cases) {
    const draft = addManualModels(fromProviderConfig(config()), each.text)
    assert.deepEqual(Object.values(draft.modelAliases), each.aliases)
    if (each.upstream)
      assert.equal(
        draft.modelConfigs[draft.models[0]].upstream_model,
        each.upstream
      )
  }
})

test("removed models can be restored without changing aliases, fields or default row references", () => {
  for (const source of ["manual", "fetch"] as const) {
    const draft = fromProviderConfig(
      config({
        main: {
          upstream_model: "vendor/main",
          thinking: false,
          reasoning_efforts: [],
          context_window: 128000,
        },
        other: null,
      })
    )
    const defaultModelID = "main"
    const removed = { ...draft, models: ["other"] }
    const restored =
      source === "manual"
        ? addManualModels(removed, "main")
        : applyFetchedModelSelection(removed, { main: { thinking: true } }, [
            "main",
          ])
    assert.deepEqual(restored.models, ["main", "other"], source)
    assert.equal(restored.modelAliases[defaultModelID], "main", source)
    assert.deepEqual(
      restored.modelConfigs.main,
      draft.modelConfigs.main,
      source
    )
    assert.equal(isSameProviderDraft(restored, draft), true, source)
  }
})

test("provider connection conversion trims values and omits empty endpoint overrides", () => {
  const draft = fromProviderConfig({
    ...config({ b: null, a: null }),
    protocol: undefined,
    endpoints: { claude: " https://other.example/messages " },
  })
  assert.equal(draft.protocol, "openai_responses")
  assert.deepEqual(draft.models, ["a", "b"])
  draft.endpoints.openai_chat = " "
  assert.deepEqual(toProviderConfig(draft).endpoints, {
    claude: "https://other.example/messages",
  })
})

test("provider IDs and model groups use existing public rules", () => {
  for (const each of [
    { id: "", want: undefined },
    { id: "a/b", want: "invalid" },
    { id: "..", want: "invalid" },
    { id: " a ", want: "duplicate" },
    { id: "valid_1.a", want: undefined },
  ])
    assert.equal(getProviderIDIssue(each.id, ["a"]), each.want)
  assert.deepEqual(
    groupProviderModels({ "gpt-a": null, "gpt-b": null, other: null }),
    [
      { name: "gpt", modelIDs: ["gpt-a", "gpt-b"] },
      { name: "", modelIDs: ["other"] },
    ]
  )
})
