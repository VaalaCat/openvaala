import type { ReadableAtom } from "nanostores"
import {
  GlobalSubscriptionSync,
  type GlobalSubscriptionConnection,
  type GlobalSubscriptionDeps,
} from "./global-subscription-sync"
import type { ConversationID } from "./protocol.gen"

// RunningConversationsSync 同步服务器当前执行 Turn 的对话集合，并恢复断线或关闭的订阅。
export class RunningConversationsSync extends GlobalSubscriptionSync<
  "conversation/running/subscribe",
  "conversation/runningChanged",
  ReadonlySet<ConversationID>
> {
  // undefined 表示尚未同步或连接断开；空集合表示已确认没有运行中的对话。
  readonly $conversationIDs: ReadableAtom<
    ReadonlySet<ConversationID> | undefined
  > = this.$value

  constructor(
    connection: GlobalSubscriptionConnection,
    deps?: GlobalSubscriptionDeps
  ) {
    super(
      {
        subscribeMethod: "conversation/running/subscribe",
        unsubscribeMethod: "conversation/running/unsubscribe",
        changedMethod: "conversation/runningChanged",
        closedMethod: "conversation/runningClosed",
        getInitialValue: (result) => new Set(result.conversation_ids),
        getChangedValue: (params) => new Set(params.conversation_ids),
      },
      connection,
      deps
    )
  }
}
