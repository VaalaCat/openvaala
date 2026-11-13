import { useMemo } from "react"
import { useStore } from "@nanostores/react"
import { ToolCallView } from "@/components/conversation/tool-call-view"
import { getToolCallState, type ItemRendererProps } from "@/lib/item-renderers"
import { parseToolArguments } from "@/lib/tool-arguments"

// tool_call：和同一 call_id 的 tool_result 合成一张工具卡；状态看调用、结果和这一轮是否还在执行
export function ToolCallItem({ view, toolResult, sync }: ItemRendererProps) {
  const call = view.item.tool_call!
  const result = toolResult?.item.tool_result
  const turns = useStore(sync.$turns, { keys: [view.turnID] })
  const state = getToolCallState(
    call,
    result,
    view.isStreaming,
    turns[view.turnID]?.status === "running"
  )
  const parsedArguments = useMemo(
    () => parseToolArguments(call.arguments),
    [call.arguments]
  )
  return (
    <ToolCallView
      viewKey={view.key}
      call={call}
      arguments={parsedArguments}
      result={result}
      state={state}
      sync={sync}
    />
  )
}
