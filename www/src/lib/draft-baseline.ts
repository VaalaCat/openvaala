export type DraftBaselineRules<Saved, Draft> = {
  readDraft(saved: Saved): Draft
  isSameSaved(a: Saved, b: Saved): boolean
  isSameDraft(a: Draft, b: Draft): boolean
}

export type DraftBaselineState<Saved, Draft> = {
  baseline: Saved
  draft: Draft
  revision: number
}

export function createDraftBaseline<Saved, Draft>(
  saved: Saved,
  rules: DraftBaselineRules<Saved, Draft>
): DraftBaselineState<Saved, Draft> {
  return { baseline: saved, draft: rules.readDraft(saved), revision: 0 }
}

export function isDraftDirty<Saved, Draft>(
  state: DraftBaselineState<Saved, Draft>,
  rules: DraftBaselineRules<Saved, Draft>
) {
  return !rules.isSameDraft(state.draft, rules.readDraft(state.baseline))
}

// 保存中不算冲突：保存会同时刷新多个查询，详情可能先于提交新基线返回
export function isDraftStale<Saved, Draft>(
  state: DraftBaselineState<Saved, Draft>,
  saved: Saved,
  dirty: boolean,
  pending: boolean,
  rules: DraftBaselineRules<Saved, Draft>
) {
  return dirty && !pending && !rules.isSameSaved(saved, state.baseline)
}

export function replaceBaseline<Saved, Draft>(
  state: DraftBaselineState<Saved, Draft>,
  next: Saved,
  rules: DraftBaselineRules<Saved, Draft>
): DraftBaselineState<Saved, Draft> {
  return {
    baseline: next,
    draft: rules.readDraft(next),
    revision: state.revision + 1,
  }
}

export function adoptLatestSaved<Saved, Draft>(
  state: DraftBaselineState<Saved, Draft>,
  saved: Saved,
  pending: boolean,
  rules: DraftBaselineRules<Saved, Draft>
) {
  if (
    pending ||
    isDraftDirty(state, rules) ||
    rules.isSameSaved(saved, state.baseline)
  ) {
    return state
  }
  return replaceBaseline(state, saved, rules)
}
