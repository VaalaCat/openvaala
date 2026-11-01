import { createElement } from "react"
import type { TFunction } from "i18next"
import { toast } from "sonner"
import type { SetDefinitionsResult } from "@/lib/api/protocol.gen"
import { getRpcErrorDetail, getRpcErrorMessage } from "@/lib/rpc-error"

// 全站唯一调用 sonner 的地方；页面和组件只通过这里发 toast。
export function notifySuccess(message: string, description?: string): void {
  toast.success(message, { duration: 4000, description })
}

export function notifyFailure(error: unknown, t: TFunction): void {
  const detail = getRpcErrorDetail(error)
  toast.error(getRpcErrorMessage(error, t), {
    duration: Infinity,
    closeButton: true,
    description: detail
      ? createElement(
          "details",
          null,
          createElement(
            "summary",
            { className: "cursor-pointer" },
            t("common.errors.detail")
          ),
          createElement("p", { className: "wrap-anywhere" }, detail)
        )
      : undefined,
  })
}

// 配置改动生效的提示：有对话正在执行、这次没换上新定义时，说明它们空闲后重新加载配置才生效
export function notifyDefinitionsUpdated(
  message: string,
  result: SetDefinitionsResult,
  t: TFunction
): void {
  const count = result.busy_conversation_ids.length
  notifySuccess(
    message,
    count > 0 ? t("settings.busyConversations", { count }) : undefined
  )
}
