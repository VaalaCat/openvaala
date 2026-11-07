<script setup lang="ts">
// subagents：父 Agent 用 spawn_agent 依次启动 api/、web/、cli/ 三个子 Agent，框和连线出现后任务包沿连线送达，
// 每个子 Agent 的上下文条从任务消息开始，各自并行增长；web/ 中途再启动自己的子 Agent web/ui/，后者做完向 web/ 汇报。
// 父 Agent 在 wait_agent 里等待；子 Agent 每跑完一轮就把汇报包送回父 Agent，汇报依次接到父 Agent 的上下文里，父 Agent 醒来继续工作。
import { computed } from "vue"
import { useData } from "vitepress"
import FlowEdge from "../primitives/FlowEdge.vue"
import FlowNode from "../primitives/FlowNode.vue"
import FlowPacket from "../primitives/FlowPacket.vue"
import SceneCanvas from "../primitives/SceneCanvas.vue"
import { getCaptionStops, getRiseStops, getShowStops, vTrack, type SceneStop } from "../primitives/motion"
import type { Vec2 } from "../primitives/iso"

const props = withDefaults(defineProps<{ layout?: "wide" | "narrow" }>(), { layout: "wide" })

const { localeIndex } = useData()
const isZh = computed(() => localeIndex.value === "zh")

const reset = 0.95
const packetSpan = 0.05
// 第一份汇报到达父 Agent 的时刻：wait_agent 在这时醒来。
const wakeAt = 0.55
// web/ 启动 web/ui/ 的时刻，以及 web/ui/ 跑完送回汇报的时刻。
const nestedSpawnAt = 0.26
const nestedReportAt = 0.43

interface AgentBox {
  x: number
  y: number
  width: number
  height: number
}

// 上下文条里的一段：message 是任务消息或汇报（深色块，到达时出现），work 是 Agent 自己的工作（浅色，匀速长出）。
interface ContextPart {
  kind: "message" | "work"
  from: number
  to: number
  start: number
  end?: number
}

interface SubagentInfo {
  key: string
  label: string
  box: AgentBox
  spawnEdge: Vec2[]
  spawnAt: number
  finishAt: number
  contextParts: ContextPart[]
}

// 宽版：父 Agent 在左，子 Agent 竖排在中间，web/ 的子 Agent 在右。窄版：父 Agent 在上，子 Agent 横排，web/ui/ 在 web/ 下方。
const geometry = computed(() => {
  if (props.layout === "narrow") {
    const subagentY = 122
    const subagentXs = [12, 114, 216]
    return {
      width: 320,
      height: 260,
      parent: { x: 80, y: 12, width: 160, height: 66 },
      subagentBoxes: subagentXs.map((x) => ({ x, y: subagentY, width: 92, height: 54 })),
      spawnEdges: subagentXs.map((x): Vec2[] => [[160, 78], [160, 102], [x + 46, 102], [x + 46, subagentY]]),
      nested: { x: 114, y: 198, width: 92, height: 54 },
      nestedEdge: [[160, subagentY + 54], [160, 198]] as Vec2[],
      groupLabel: { x: 12, y: 91 },
      nestedNote: { x: 106, y: 228, anchor: "end" },
      parentInset: { pad: 12, labelY: 21, statusY: 38, barY: 48, barHeight: 9 },
      subagentInset: { pad: 8, labelY: 21, barY: 33, barHeight: 9 },
    }
  }
  const subagentYs = [22, 84, 146]
  return {
    width: 440,
    height: 204,
    parent: { x: 18, y: 72, width: 124, height: 72 },
    subagentBoxes: subagentYs.map((y) => ({ x: 200, y, width: 110, height: 48 })),
    spawnEdges: subagentYs.map((y): Vec2[] => [[142, 108], [171, 108], [171, y + 24], [200, y + 24]]),
    nested: { x: 340, y: 84, width: 92, height: 48 },
    nestedEdge: [[310, 108], [340, 108]] as Vec2[],
    groupLabel: { x: 200, y: 13 },
    nestedNote: { x: 340, y: 146, anchor: "start" },
    parentInset: { pad: 12, labelY: 20, statusY: 35, barY: 50, barHeight: 8 },
    subagentInset: { pad: 10, labelY: 19, barY: 30, barHeight: 8 },
  }
})

// 三个子 Agent：spawnAt 任务包出发，finishAt 跑完这一轮并送出汇报。
const subagents = computed<SubagentInfo[]>(() => {
  const plans = [
    { key: "api", label: "api/", spawnAt: 0.06, finishAt: 0.5, workTo: 0.82 },
    { key: "web", label: "web/", spawnAt: 0.08, finishAt: 0.62, workTo: 0.86 },
    { key: "cli", label: "cli/", spawnAt: 0.1, finishAt: 0.57, workTo: 0.7 },
  ]
  return plans.map((plan, index) => {
    const arriveAt = plan.spawnAt + packetSpan
    const task: ContextPart = { kind: "message", from: 0, to: 0.16, start: arriveAt }
    // web/ 的上下文中间接着 web/ui/ 的汇报，前后各一段自己的工作。
    const contextParts: ContextPart[] = plan.key === "web"
      ? [
          task,
          { kind: "work", from: 0.18, to: 0.48, start: arriveAt + 0.02, end: 0.45 },
          { kind: "message", from: 0.5, to: 0.62, start: nestedReportAt + packetSpan },
          { kind: "work", from: 0.64, to: plan.workTo, start: nestedReportAt + packetSpan + 0.01, end: plan.finishAt - 0.01 },
        ]
      : [task, { kind: "work", from: 0.18, to: plan.workTo, start: arriveAt + 0.02, end: plan.finishAt - 0.01 }]
    return {
      ...plan,
      box: geometry.value.subagentBoxes[index],
      spawnEdge: geometry.value.spawnEdges[index],
      contextParts,
    }
  })
})

const nestedParts: ContextPart[] = [
  { kind: "message", from: 0, to: 0.18, start: nestedSpawnAt + 0.04 },
  { kind: "work", from: 0.2, to: 0.78, start: nestedSpawnAt + 0.06, end: nestedReportAt - 0.01 },
]

// 父 Agent 的上下文：已有的历史，三份汇报按到达顺序接上，最后是醒来后的工作。
const parentParts = computed<ContextPart[]>(() => {
  const reportArrivals = subagents.value.map((subagent) => subagent.finishAt + packetSpan).sort((p, q) => p - q)
  return [
    { kind: "work", from: 0, to: 0.3, start: 0.02 },
    ...reportArrivals.map((at, index): ContextPart => ({ kind: "message", from: 0.32 + index * 0.14, to: 0.44 + index * 0.14, start: at })),
    { kind: "work", from: 0.74, to: 0.94, start: reportArrivals[2] + 0.02, end: 0.88 },
  ]
})

function getGrowStops(start: number, end: number): SceneStop[] {
  return [
    [0, { opacity: 0, transform: "scaleX(0)" }],
    [start - 0.001, { opacity: 0, transform: "scaleX(0)" }],
    [start, { opacity: 1, transform: "scaleX(0)", easing: "linear" }],
    [end, { opacity: 1, transform: "scaleX(1)" }],
    [reset, { opacity: 1, transform: "scaleX(1)" }],
    [reset + 0.03, { opacity: 0, transform: "scaleX(1)" }],
  ]
}

function getPartStops(part: ContextPart): SceneStop[] {
  return part.end ? getGrowStops(part.start, part.end) : getShowStops(part.start, reset, 0.012)
}

// getPartRect 把上下文条里的一段换成坐标：条内留 1.5 的边。
function getPartRect(box: AgentBox, inset: { pad: number; barY: number; barHeight: number }, part: ContextPart) {
  const innerX = box.x + inset.pad + 1.5
  const innerWidth = box.width - inset.pad * 2 - 3
  return {
    x: innerX + innerWidth * part.from,
    y: box.y + inset.barY + 1.5,
    width: innerWidth * (part.to - part.from),
    height: inset.barHeight - 3,
  }
}

// 父 Agent 的状态行依次是：发出任务、等待汇报、醒来继续。
const parentStatuses = computed(() => [
  { text: "spawn_agent ×3", stops: getShowStops(0.05, 0.17, 0.012) },
  { text: "wait_agent", stops: getShowStops(0.19, wakeAt, 0.012) },
  { text: isZh.value ? "继续工作" : "working", stops: getShowStops(wakeAt + 0.015, reset, 0.012), isFinal: true },
])

const captions = computed(() => [
  { text: isZh.value ? "spawn_agent 把任务的几部分交给子 Agent" : "spawn_agent hands parts of the task to subagents", stops: getCaptionStops(0, 0.19) },
  {
    text: isZh.value ? "子 Agent 并行运行，各有自己的上下文，也能再启动子 Agent" : "they run in parallel, each with its own context, and can start subagents of their own",
    stops: getCaptionStops(0.19, 0.49),
  },
  {
    text: isZh.value ? "每跑完一轮就自动汇报；wait_agent 等到汇报，父 Agent 接着做" : "each reports back when it finishes a turn; wait_agent wakes the parent",
    stops: getCaptionStops(0.49, 0.985),
  },
])
</script>

<template>
  <SceneCanvas
    class="scene-canvas--flow"
    :view-box="`0 0 ${geometry.width} ${geometry.height}`"
    :period="13000"
    :label="isZh ? '子 Agent 并行运行并向父 Agent 汇报' : 'Subagents run in parallel and report back to their parent'"
  >
    <text class="scene-group-label" :x="geometry.groupLabel.x" :y="geometry.groupLabel.y">{{ isZh ? "子 Agent" : "subagents" }}</text>

    <!-- 连线随两端的框一起出现：框先升起，连线紧跟着淡入，任务包随后出发 -->
    <FlowEdge v-for="subagent in subagents" :key="`edge-${subagent.key}`" v-track="getShowStops(subagent.spawnAt - 0.01, reset)" :points="subagent.spawnEdge" tone="lo" />
    <FlowEdge v-track="getShowStops(nestedSpawnAt - 0.01, reset)" :points="geometry.nestedEdge" tone="lo" />

    <!-- 任务包沿连线发出，汇报包在跑完一轮时送回 -->
    <template v-for="subagent in subagents" :key="`packets-${subagent.key}`">
      <FlowPacket :points="subagent.spawnEdge" :at="subagent.spawnAt" :span="packetSpan" />
      <FlowPacket :points="subagent.spawnEdge" :at="subagent.finishAt" :span="packetSpan" is-reverse />
    </template>
    <FlowPacket :points="geometry.nestedEdge" :at="nestedSpawnAt" :span="0.04" />
    <FlowPacket :points="geometry.nestedEdge" :at="nestedReportAt" :span="0.04" is-reverse />

    <!-- 父 Agent：名称、状态行、上下文条，醒来后亮灯 -->
    <FlowNode v-bind="geometry.parent" />
    <text class="scene-label scene-label--ink" :x="geometry.parent.x + geometry.parentInset.pad" :y="geometry.parent.y + geometry.parentInset.labelY">
      {{ isZh ? "父 Agent" : "parent agent" }}
    </text>
    <text
      v-for="status in parentStatuses"
      :key="status.text"
      v-track="status.stops"
      class="scene-label scene-label--small"
      :x="geometry.parent.x + geometry.parentInset.pad"
      :y="geometry.parent.y + geometry.parentInset.statusY"
      :style="{ opacity: status.isFinal ? 1 : 0 }"
    >
      {{ status.text }}
    </text>
    <rect
      class="context-bar"
      :x="geometry.parent.x + geometry.parentInset.pad"
      :y="geometry.parent.y + geometry.parentInset.barY"
      :width="geometry.parent.width - geometry.parentInset.pad * 2"
      :height="geometry.parentInset.barHeight"
      rx="2.5"
    />
    <rect
      v-for="(part, index) in parentParts"
      :key="`parent-${index}`"
      v-track="getPartStops(part)"
      class="context-part"
      :class="`context-part--${part.kind}`"
      v-bind="getPartRect(geometry.parent, geometry.parentInset, part)"
      rx="1"
    />
    <circle v-track="getShowStops(wakeAt + 0.01, 0.9, 0.012)" class="scene-light" :cx="geometry.parent.x + geometry.parent.width - 8" :cy="geometry.parent.y + 8" r="2.4" />

    <!-- 三个子 Agent：spawn_agent 时出现，任务包到达后开始工作并亮灯 -->
    <g v-for="subagent in subagents" :key="subagent.key" v-track="getRiseStops(subagent.spawnAt - 0.02, reset, 4)">
      <FlowNode v-bind="subagent.box" />
      <text class="scene-label scene-label--ink" :x="subagent.box.x + geometry.subagentInset.pad" :y="subagent.box.y + geometry.subagentInset.labelY">{{ subagent.label }}</text>
      <rect
        class="context-bar"
        :x="subagent.box.x + geometry.subagentInset.pad"
        :y="subagent.box.y + geometry.subagentInset.barY"
        :width="subagent.box.width - geometry.subagentInset.pad * 2"
        :height="geometry.subagentInset.barHeight"
        rx="2.5"
      />
      <rect
        v-for="(part, index) in subagent.contextParts"
        :key="index"
        v-track="getPartStops(part)"
        class="context-part"
        :class="`context-part--${part.kind}`"
        v-bind="getPartRect(subagent.box, geometry.subagentInset, part)"
        rx="1"
      />
      <circle
        v-track="getShowStops(subagent.spawnAt + packetSpan + 0.01, subagent.finishAt, 0.012)"
        class="scene-light"
        :cx="subagent.box.x + subagent.box.width - 8"
        :cy="subagent.box.y + 8"
        r="2.4"
      />
    </g>

    <!-- web/ 自己启动的子 Agent -->
    <g v-track="getRiseStops(nestedSpawnAt - 0.02, reset, 4)">
      <FlowNode v-bind="geometry.nested" />
      <text class="scene-label scene-label--ink" :x="geometry.nested.x + geometry.subagentInset.pad" :y="geometry.nested.y + geometry.subagentInset.labelY">web/ui/</text>
      <rect
        class="context-bar"
        :x="geometry.nested.x + geometry.subagentInset.pad"
        :y="geometry.nested.y + geometry.subagentInset.barY"
        :width="geometry.nested.width - geometry.subagentInset.pad * 2"
        :height="geometry.subagentInset.barHeight"
        rx="2.5"
      />
      <rect
        v-for="(part, index) in nestedParts"
        :key="index"
        v-track="getPartStops(part)"
        class="context-part"
        :class="`context-part--${part.kind}`"
        v-bind="getPartRect(geometry.nested, geometry.subagentInset, part)"
        rx="1"
      />
      <circle
        v-track="getShowStops(nestedSpawnAt + 0.05, nestedReportAt, 0.012)"
        class="scene-light"
        :cx="geometry.nested.x + geometry.nested.width - 8"
        :cy="geometry.nested.y + 8"
        r="2.4"
      />
      <text class="scene-label scene-label--small" :x="geometry.nestedNote.x" :y="geometry.nestedNote.y" :text-anchor="geometry.nestedNote.anchor">
        {{ isZh ? "由 web/ 启动" : "started by web/" }}
      </text>
    </g>

    <template #caption>
      <p
        v-for="(caption, index) in captions"
        :key="caption.text"
        v-track="caption.stops"
        class="scene-caption"
        :style="{ opacity: index === captions.length - 1 ? 1 : 0 }"
      >
        {{ caption.text }}
      </p>
    </template>
  </SceneCanvas>
</template>

<style scoped>
.context-bar {
  fill: none;
  stroke: var(--scene-lo);
  stroke-width: 1;
}

.context-part {
  transform-box: fill-box;
  transform-origin: left center;
}

.context-part--message {
  fill: var(--scene-mid);
}

.context-part--work {
  fill: var(--scene-lo);
}
</style>
