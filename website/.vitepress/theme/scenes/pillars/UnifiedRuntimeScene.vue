<script setup lang="ts">
// unified-runtime：五个客户端、一个运行时（Agent 循环）、四个 Executor。
// 终端发来一条消息 → 运行时的循环转动 → 工具调用依次发往本机、build-box、gpu-01、沙箱并带回结果
// → 更新同时推送给所有客户端。尚未支持的 Executor 只画淡虚线。
// 宽版从左到右排客户端、运行时、Executor；窄版从上到下排，客户端和 Executor 左右交错挂在中间的竖线上。
// 两版共用同一张时间表，只有 geometry 里的坐标不同。
import { computed } from "vue"
import { useData } from "vitepress"
import FlowEdge from "../primitives/FlowEdge.vue"
import FlowNode from "../primitives/FlowNode.vue"
import FlowPacket from "../primitives/FlowPacket.vue"
import SceneCanvas from "../primitives/SceneCanvas.vue"
import { getCaptionStops, getShowStops, vTrack, type SceneStop } from "../primitives/motion"
import type { Vec2 } from "../primitives/iso"

const props = withDefaults(defineProps<{ layout?: "wide" | "narrow" }>(), { layout: "wide" })

const { localeIndex } = useData()
const isZh = computed(() => localeIndex.value === "zh")

interface NodeSlot {
  x: number
  y: number
  width: number
  height: number
}

// LinkedSlot 是一个节点的位置和它与运行时之间的连线。
interface LinkedSlot {
  slot: NodeSlot
  edge: Vec2[]
}

// UnifiedRuntimeGeometry 是一版布局的全部坐标。
interface UnifiedRuntimeGeometry {
  viewBox: string
  clientsLabel: Vec2
  executorsLabel: Vec2
  runtimeBox: NodeSlot
  runtimeLabelY: number
  loopCenter: Vec2
  loopLabelY: number
  clientSlots: LinkedSlot[]
  executorSlots: LinkedSlot[]
  // futureMarks 是尚未支持的 Executor 小方块的左上角。
  futureMarks: Vec2[]
  comingLabel: Vec2
}

// 宽版：客户端在左、Executor 在右，连线在运行时左右两侧的竖线上汇合。
function getWideGeometry(): UnifiedRuntimeGeometry {
  const node = { width: 104, height: 32 }
  const linkY = 176
  return {
    viewBox: "0 0 440 294",
    clientsLabel: [8, 26],
    executorsLabel: [328, 26],
    runtimeBox: { x: 168, y: 104, width: 104, height: 124 },
    runtimeLabelY: 130,
    loopCenter: [220, 180],
    loopLabelY: 218,
    clientSlots: [0, 1, 2, 3, 4].map((index) => {
      const y = 38 + index * 42
      const middle = y + node.height / 2
      return { slot: { x: 8, y, ...node }, edge: [[112, middle], [140, middle], [140, linkY], [168, linkY]] }
    }),
    executorSlots: [0, 1, 2, 3].map((index) => {
      const y = 38 + index * 42
      const middle = y + node.height / 2
      return { slot: { x: 328, y, ...node }, edge: [[272, linkY], [300, linkY], [300, middle], [328, middle]] }
    }),
    futureMarks: [0, 1, 2, 3].map((index) => [330, 214 + index * 15]),
    comingLabel: [8, 286],
  }
}

// 窄版：客户端在上、Executor 在下，各自左右交错，从内侧接到 x=160 的竖线再进出运行时。
function getNarrowGeometry(): UnifiedRuntimeGeometry {
  const node = { width: 140, height: 32 }
  const spineX = 160
  const runtimeBox = { x: 100, y: 150, width: 120, height: 100 }
  const runtimeBottom = runtimeBox.y + runtimeBox.height
  // isLeft 为 true 时节点在竖线左侧，连线从节点右边出发。
  function getSlot(isLeft: boolean, y: number): NodeSlot {
    return { x: isLeft ? 4 : 176, y, ...node }
  }
  function getInnerX(slot: NodeSlot): number {
    return slot.x < spineX ? slot.x + slot.width : slot.x
  }
  return {
    viewBox: "0 0 320 424",
    clientsLabel: [4, 12],
    executorsLabel: [4, 274],
    runtimeBox,
    runtimeLabelY: 172,
    loopCenter: [spineX, 202],
    loopLabelY: 240,
    clientSlots: [0, 1, 2, 3, 4].map((index) => {
      const slot = getSlot(index % 2 === 0, 20 + index * 20)
      const middle = slot.y + node.height / 2
      return { slot, edge: [[getInnerX(slot), middle], [spineX, middle], [spineX, runtimeBox.y]] }
    }),
    executorSlots: [0, 1, 2, 3].map((index) => {
      const slot = getSlot(index % 2 === 0, 284 + Math.floor(index / 2) * 38)
      const middle = slot.y + node.height / 2
      return { slot, edge: [[spineX, runtimeBottom], [spineX, middle], [getInnerX(slot), middle]] }
    }),
    futureMarks: [0, 1, 2, 3].map((index) => [index % 2 === 0 ? 6 : 178, 372 + Math.floor(index / 2) * 16]),
    comingLabel: [4, 416],
  }
}

const geometry = computed(() => (props.layout === "narrow" ? getNarrowGeometry() : getWideGeometry()))

const clients = computed(() =>
  [
    { label: isZh.value ? "终端" : "Terminal", sublabel: "TUI" },
    { label: "Web", sublabel: isZh.value ? "浏览器 · 手机" : "browser · phone" },
    { label: isZh.value ? "桌面端" : "Desktop", sublabel: "app" },
    { label: "Zed", sublabel: "ACP" },
    { label: "Codex", sublabel: "app-server" },
  ].map((client, index) => ({ ...client, ...geometry.value.clientSlots[index] })),
)

// 已支持的 Executor：call 是工具调用出发的时刻。
const executors = computed(() =>
  [
    { label: isZh.value ? "本机" : "this machine", sublabel: "local", call: 0.2 },
    { label: "build-box", sublabel: "ssh", call: 0.31 },
    { label: "gpu-01", sublabel: "ssh", call: 0.42 },
    { label: isZh.value ? "沙箱" : "sandbox", sublabel: isZh.value ? "内置" : "built-in", call: 0.53 },
  ].map((executor, index) => ({ ...executor, ...geometry.value.executorSlots[index] })),
)

const futureExecutors = computed(() =>
  (isZh.value ? ["worker", "云沙箱", "容器", "远程 VM"] : ["worker", "cloud sandbox", "container", "remote vm"]).map(
    (label, index) => ({ label, mark: geometry.value.futureMarks[index] }),
  ),
)

// updateAt 是更新推给所有客户端的时刻，客户端收到后亮灯到循环末尾。
const updateAt = 0.7
const clientLightStops = getShowStops(updateAt + 0.075, 0.95, 0.012)

// 三段说明依次出现；静态画面只显示最后一段。
const captions = computed(() => [
  { text: isZh.value ? "1  终端发来一条消息" : "1  a message arrives from the terminal", stops: getCaptionStops(0, 0.19) },
  { text: isZh.value ? "2  工具调用分发到多台机器执行" : "2  tool calls run on several machines", stops: getCaptionStops(0.19, updateAt) },
  { text: isZh.value ? "3  所有客户端同时收到更新" : "3  every client receives the update", stops: getCaptionStops(updateAt, 0.985) },
])

function getWorkStops(call: number): SceneStop[] {
  return getShowStops(call + 0.065, call + 0.1, 0.012)
}

// 循环指示点每个周期匀速转三圈，表示 Agent 循环一直在运转。
const loopStops: SceneStop[] = [
  [0, { transform: "rotate(0deg)", easing: "linear" }],
  [1, { transform: "rotate(1080deg)" }],
]
</script>

<template>
  <SceneCanvas class="scene-canvas--flow" :view-box="geometry.viewBox" :period="12000" :label="isZh ? '统一运行时与跨机器执行' : 'One runtime, tools on many machines'">
    <text class="scene-group-label" :x="geometry.clientsLabel[0]" :y="geometry.clientsLabel[1]">{{ isZh ? "客户端" : "clients" }}</text>
    <text class="scene-group-label" :x="geometry.executorsLabel[0]" :y="geometry.executorsLabel[1]">executors</text>

    <FlowEdge v-for="client in clients" :key="`edge-${client.label}`" :points="client.edge" tone="lo" />
    <FlowEdge v-for="executor in executors" :key="`edge-${executor.label}`" :points="executor.edge" tone="lo" />

    <!-- 输入：终端 → 运行时；更新：运行时 → 全部客户端 -->
    <FlowPacket :points="clients[0].edge" :at="0.04" :span="0.1" />
    <FlowPacket v-for="client in clients" :key="`update-${client.label}`" :points="client.edge" :at="updateAt" :span="0.07" is-reverse />
    <template v-for="executor in executors" :key="`calls-${executor.label}`">
      <FlowPacket :points="executor.edge" :at="executor.call" :span="0.07" />
      <FlowPacket :points="executor.edge" :at="executor.call + 0.1" :span="0.06" is-reverse />
    </template>

    <g v-for="client in clients" :key="client.label">
      <FlowNode v-bind="client.slot" :label="client.label" :sublabel="client.sublabel" />
      <circle v-track="clientLightStops" class="scene-light" :cx="client.slot.x + client.slot.width - 6" :cy="client.slot.y + 6" r="2.4" />
    </g>

    <FlowNode v-bind="geometry.runtimeBox" :radius="12" />
    <text class="scene-label scene-label--ink" :x="geometry.loopCenter[0]" :y="geometry.runtimeLabelY" text-anchor="middle">{{ isZh ? "运行时" : "runtime" }}</text>
    <circle class="loop-ring" :cx="geometry.loopCenter[0]" :cy="geometry.loopCenter[1]" r="22" />
    <g v-track="loopStops" class="loop-orbit" :style="{ transformOrigin: `${geometry.loopCenter[0]}px ${geometry.loopCenter[1]}px` }">
      <circle class="loop-dot" :cx="geometry.loopCenter[0]" :cy="geometry.loopCenter[1] - 22" r="3" />
    </g>
    <text class="scene-label scene-label--small" :x="geometry.loopCenter[0]" :y="geometry.loopLabelY" text-anchor="middle">
      {{ isZh ? "Agent 循环" : "agent loop" }}
    </text>

    <g v-for="executor in executors" :key="executor.label">
      <FlowNode v-bind="executor.slot" :label="executor.label" :sublabel="executor.sublabel" />
      <circle v-track="getWorkStops(executor.call)" class="scene-light" :cx="executor.slot.x + executor.slot.width - 6" :cy="executor.slot.y + 6" r="2.4" />
    </g>

    <!-- 尚未支持的 Executor：淡虚线小方块加名称，越往后越淡 -->
    <g v-for="(future, index) in futureExecutors" :key="future.label" :style="{ opacity: 0.7 - index * 0.12 }">
      <rect class="future-mark" :x="future.mark[0]" :y="future.mark[1]" width="7" height="7" rx="1.5" />
      <text class="scene-label scene-label--small" :x="future.mark[0] + 13" :y="future.mark[1] + 6.5">{{ future.label }}</text>
    </g>
    <text class="scene-label scene-label--small" :x="geometry.comingLabel[0]" :y="geometry.comingLabel[1]">
      {{ isZh ? "更多 Executor 陆续支持 …" : "more executors coming …" }}
    </text>

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
.loop-ring {
  fill: none;
  stroke: var(--scene-lo);
  stroke-width: 1;
  stroke-dasharray: 2 3;
}

.loop-dot {
  fill: var(--scene-hi);
}

.loop-orbit {
  transform-box: view-box;
}

.future-mark {
  fill: none;
  stroke: var(--scene-mid);
  stroke-width: 1;
  stroke-dasharray: 2 2;
}
</style>
