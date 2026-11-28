import { useSearchParams } from "react-router"

const filterKeys = [
  "adminSearch",
  "owner",
  "tag",
  "online",
  "status",
  "ruleOwner",
] as const

// 管理页签和筛选跟随浏览器的历史记录。
export function useWorldAdminFilters() {
  const [search, setSearch] = useSearchParams()
  function update(key: string, value: string) {
    setSearch((previous) => {
      const next = new URLSearchParams(previous)
      if (value) next.set(key, value)
      else next.delete(key)
      return next
    })
  }
  function open(view: string, filters: Record<string, string> = {}) {
    setSearch((previous) => {
      const next = new URLSearchParams(previous)
      for (const key of filterKeys) next.delete(key)
      next.set("view", view)
      for (const [k, v] of Object.entries(filters)) next.set(k, v)
      return next
    })
  }
  return { search, update, open }
}

export function matchesWorldAdminFilters(
  item: { owner: string; tags: string[] | null },
  search: URLSearchParams
) {
  const owner = search.get("owner"),
    tag = search.get("tag")
  return (
    (!owner || item.owner === owner) && (!tag || !!item.tags?.includes(tag))
  )
}

export function matchesWorldAdminSearch(
  search: URLSearchParams,
  values: string[]
) {
  const q = search.get("adminSearch")?.trim().toLowerCase()
  return !q || values.some((value) => value.toLowerCase().includes(q))
}
