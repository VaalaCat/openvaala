import type { ReadableAtom } from "nanostores"
import {
  GlobalSubscriptionSync,
  type GlobalSubscriptionConnection,
  type GlobalSubscriptionDeps,
} from "./global-subscription-sync"
import type { TerminalItem } from "./protocol.gen"

// RunningTerminalsSync 同步服务器上运行中的终端列表，并恢复断线或关闭的订阅。
export class RunningTerminalsSync extends GlobalSubscriptionSync<
  "terminal/subscribe",
  "terminal/listChanged",
  TerminalItem[]
> {
  // undefined 表示尚未同步或连接断开；空列表表示已确认没有运行中的终端。
  readonly $terminals: ReadableAtom<TerminalItem[] | undefined> = this.$value

  constructor(
    connection: GlobalSubscriptionConnection,
    deps?: GlobalSubscriptionDeps
  ) {
    super(
      {
        subscribeMethod: "terminal/subscribe",
        unsubscribeMethod: "terminal/unsubscribe",
        changedMethod: "terminal/listChanged",
        closedMethod: "terminal/listClosed",
        getInitialValue: (result) => result.terminals,
        getChangedValue: (params) => params.terminals,
      },
      connection,
      deps
    )
  }
}
