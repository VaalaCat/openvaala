import { createElement } from "react"
import type { TFunction } from "i18next"
import { toast } from "sonner"
import type { SetDefinitionsResult } from "@/lib/api/protocol.gen"
import { getRpcErrorDetail, getRpcErrorMessage } from "@/lib/rpc-error"

// 全站唯一调用 sonner 的地方；页面和组件只通过这里发 toast。
export function notifySuccess(message: string, description?: string): void {
  toast.success(message, { duration: 4000, description })
}

// 不是成功也不是失败的普通提示，例如旁支已经结束
export function notifyInfo(message: string): void {
  toast.info(message, { duration: 4000 })
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

// 配置改动接收后的提示：有待用配置时，说明对话空闲后会自动应用
export function notifyDefinitionsUpdated(
  message: string,
  result: SetDefinitionsResult,
  t: TFunction
): void {
  const count = result.pending_conversation_ids.length
  notifySuccess(
    message,
    count > 0 ? t("settings.pendingConversations", { count }) : undefined
  )
}
