import {
  createContext,
  useCallback,
  useContext,
  useState,
  type Dispatch,
  type SetStateAction,
} from "react"

// 消息区提供的 Map：按调用方给的 key 保存用户选择的展开状态，组件重新挂载时取回上次的值。
export const KeptViewStateContext = createContext<
  Map<string, unknown> | undefined
>(undefined)

// 和 useState 用法一致；有 key 且在消息区内时，初始值取 Map 里保存的值，每次更新同步写回 Map。
export function useKeptViewState<T>(
  viewStateKey: string | undefined,
  initialValue: T | (() => T)
): [T, Dispatch<SetStateAction<T>>] {
  const keptViewStates = useContext(KeptViewStateContext)
  const [value, setValue] = useState<T>(() => {
    // 1. 优先取上次保存的值。
    if (viewStateKey !== undefined && keptViewStates?.has(viewStateKey)) {
      return keptViewStates.get(viewStateKey) as T
    }
    // 2. 没有保存的值时用初始值。
    return initialValue instanceof Function ? initialValue() : initialValue
  })
  const setKeptValue = useCallback<Dispatch<SetStateAction<T>>>(
    (action) => {
      // 1. 没有 key 或不在消息区内时同 useState。
      if (viewStateKey === undefined || keptViewStates === undefined) {
        setValue(action)
        return
      }
      // 2. 函数式更新在 updater 里算出新值，写入 Map 后交给 React。
      setValue((previous) => {
        const next =
          action instanceof Function
            ? (action as (p: T) => T)(previous)
            : action
        keptViewStates.set(viewStateKey, next)
        return next
      })
    },
    [viewStateKey, keptViewStates]
  )
  return [value, setKeptValue]
}
