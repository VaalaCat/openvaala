import { useState } from "react"

import {
  adoptLatestSaved,
  createDraftBaseline,
  isDraftDirty,
  isDraftStale,
  replaceBaseline,
  type DraftBaselineRules,
} from "@/lib/draft-baseline"

// 未改的草稿跟随最新已保存快照；改过的草稿保留基线，已保存快照前进后即 stale。
// pending 或 localDirty 为真时不采纳新快照；pending 时也不算 stale。
// rules 须为模块级常量；isSameSaved 须自反，readDraft 须纯且廉价（每次渲染都跑）。
// 采纳、撤销、提交都会递增 revision，供表单用 `key` 重挂载。
export function useDraftBaseline<Saved, Draft>({
  saved,
  pending,
  localDirty,
  rules,
}: {
  saved: Saved
  pending: boolean
  localDirty?: boolean
  rules: DraftBaselineRules<Saved, Draft>
}) {
  const [state, setState] = useState(() => createDraftBaseline(saved, rules))
  const next = adoptLatestSaved(state, saved, pending || !!localDirty, rules)
  if (next !== state) setState(next)
  const dirty = isDraftDirty(state, rules) || !!localDirty
  return {
    baseline: state.baseline,
    draft: state.draft,
    revision: state.revision,
    dirty,
    stale: isDraftStale(state, saved, dirty, pending, rules),
    setDraft: (draft: Draft) => setState((s) => ({ ...s, draft })),
    revert: () => setState((s) => replaceBaseline(s, saved, rules)),
    commit: (committed: Saved) =>
      setState((s) => replaceBaseline(s, committed, rules)),
  }
}
