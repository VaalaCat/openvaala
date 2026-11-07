<script setup lang="ts">
// ecosystem：运行时接三类插件——模型（OpenAI Chat、OpenAI Responses、Claude Messages）、客户端（终端、Web、桌面端、
// Zed 经 ACP、Codex 经 app-server）和经 MCP 接入的工具。各插件依次滑入接口，连线随插件一起出现，接通时用信号蓝淡入；
// 最后点亮一条 OpenAI Responses → 运行时 → Zed、MCP 的通路，再把模型换成 Claude Messages：
// OpenAI Responses 拔下变淡，Zed 和 MCP 的连线保持不动。
// 宽版：模型在左、客户端在右、MCP 在运行时上方，三组标题在同一条基线上，各组顶部对齐，模型和客户端占同样的高度。
// 窄版：模型和 MCP 在上、客户端在下，运行时是中间的横条，连线从右上和左下两侧接入。
// 两版共用同一张时间表，只有 geometry 里的坐标不同。
import { computed } from "vue"
import { useData } from "vitepress"
import FlowEdge from "../primitives/FlowEdge.vue"
import FlowNode from "../primitives/FlowNode.vue"
import SceneCanvas from "../primitives/SceneCanvas.vue"
import { getCaptionStops, vTrack, type SceneStop } from "../primitives/motion"
import { getRounded2dPath, type Vec2 } from "../primitives/iso"

const props = withDefaults(defineProps<{ layout?: "wide" | "narrow" }>(), { layout: "wide" })

const { localeIndex } = useData()
const isZh = computed(() => localeIndex.value === "zh")

interface NodeSlot {
  x: number
  y: number
  width: number
  height: number
}

// PlugSlot 是一个插件的位置、它到运行时接口的连线和滑入前的位移。
interface PlugSlot extends NodeSlot {
  edge: Vec2[]
  port: Vec2
  from: string
}

// EcosystemGeometry 是一版布局的全部坐标。
interface EcosystemGeometry {
  viewBox: string
  modelsLabel: Vec2
  toolsLabel: Vec2
  clientsLabel: Vec2
  runtimeBox: NodeSlot
  runtimeLabel: Vec2
  modelSlots: PlugSlot[]
  clientSlots: PlugSlot[]
  toolSlot: PlugSlot
}

// getStepEdge 返回先水平、再竖直、再水平的连线；起止同高时直接连成一段。
function getStepEdge(start: Vec2, turnX: number, end: Vec2): Vec2[] {
  if (start[1] === end[1]) return [start, end]
  return [start, [turnX, start[1]], [turnX, end[1]], end]
}

// 宽版：三列顶部对齐在 y=26，客户端每行间隔 36，三个模型与第 1、3、5 个客户端同高，左右连线互为镜像；
// 运行时在 MCP 下方，接口在左右两侧。
function getWideGeometry(): EcosystemGeometry {
  const chip = { width: 124, height: 30 }
  const runtimeBox = { x: 170, y: 80, width: 100, height: 120 }
  const runtimeRight = runtimeBox.x + runtimeBox.width
  return {
    viewBox: "0 0 440 208",
    modelsLabel: [8, 16],
    toolsLabel: [158, 16],
    clientsLabel: [308, 16],
    runtimeBox,
    runtimeLabel: [220, 144],
    modelSlots: [0, 1, 2].map((index) => {
      const y = 26 + index * 72
      const port: Vec2 = [runtimeBox.x, 113 + index * 36]
      return { x: 8, y, ...chip, edge: getStepEdge([8 + chip.width, y + chip.height / 2], 151, port), port, from: "translateX(-14px)" }
    }),
    clientSlots: [0, 1, 2, 3, 4].map((index) => {
      const y = 26 + index * 36
      const port: Vec2 = [runtimeRight, 113 + index * 18]
      return { x: 308, y, ...chip, edge: getStepEdge(port, 289, [308, y + chip.height / 2]), port, from: "translateX(14px)" }
    }),
    toolSlot: { x: 158, y: 26, ...chip, edge: [[220, 56], [220, runtimeBox.y]], port: [220, runtimeBox.y], from: "translateY(-8px)" },
  }
}

// 窄版：模型列在左上、MCP 在右上，连线向右再向下接到运行时顶部；客户端列在右下，连线从运行时底部向下再向右。
// 越靠近运行时的插件，竖线离插件列越近，连线互不交叉。
function getNarrowGeometry(): EcosystemGeometry {
  const chip = { width: 132, height: 32 }
  const runtimeBox = { x: 72, y: 146, width: 208, height: 36 }
  const runtimeBottom = runtimeBox.y + runtimeBox.height
  return {
    viewBox: "0 0 320 396",
    modelsLabel: [4, 12],
    toolsLabel: [200, 12],
    clientsLabel: [184, 204],
    runtimeBox,
    runtimeLabel: [221, 168],
    modelSlots: [0, 1, 2].map((index) => {
      const y = 20 + index * 36
      const dropX = 182 - index * 16
      const port: Vec2 = [dropX, runtimeBox.y]
      return { x: 4, y, ...chip, edge: [[4 + chip.width, y + chip.height / 2], [dropX, y + chip.height / 2], port], port, from: "translateX(-10px)" }
    }),
    clientSlots: [0, 1, 2, 3, 4].map((index) => {
      const y = 212 + index * 36
      const dropX = 164 - index * 18
      const port: Vec2 = [dropX, runtimeBottom]
      return { x: 184, y, ...chip, edge: [port, [dropX, y + chip.height / 2], [184, y + chip.height / 2]], port, from: "translateX(10px)" }
    }),
    toolSlot: { x: 200, y: 20, width: 116, height: 32, edge: [[258, 52], [258, runtimeBox.y]], port: [258, runtimeBox.y], from: "translateY(-8px)" },
  }
}

const geometry = computed(() => (props.layout === "narrow" ? getNarrowGeometry() : getWideGeometry()))

interface Plug extends PlugSlot {
  key: string
  label: string
  sublabel?: string
  at: number
}

const plugs = computed<Plug[]>(() => {
  const { modelSlots, clientSlots, toolSlot } = geometry.value
  const models = [
    { label: "OpenAI", sublabel: "Chat Completions" },
    { label: "OpenAI", sublabel: "Responses" },
    { label: "Claude", sublabel: "Messages" },
  ].map((model, index) => ({ key: `model-${index}`, ...model, ...modelSlots[index], at: 0.04 + index * 0.06 }))
  const clients = [
    { label: isZh.value ? "终端" : "Terminal", sublabel: "TUI" },
    { label: "Web", sublabel: isZh.value ? "浏览器 · 手机" : "browser · phone" },
    { label: isZh.value ? "桌面端" : "Desktop", sublabel: "app" },
    { label: "Zed", sublabel: "ACP" },
    { label: "Codex", sublabel: "app-server" },
  ].map((client, index) => ({ key: `client-${index}`, ...client, ...clientSlots[index], at: 0.28 + index * 0.05 }))
  const tools = { key: "mcp", label: "MCP", sublabel: isZh.value ? "工具服务" : "tool servers", ...toolSlot, at: 0.58 }
  return [...models, ...clients, tools]
})

const reset = 0.94
// 最后一段：routeAt 点亮通路，swapAt 把模型从 replacedKey 换成 swappedInKey，routeKeys 里的其余连线保持点亮。
const routeAt = 0.75
const swapAt = 0.84
const replacedKey = "model-1"
const swappedInKey = "model-2"
const routeKeys = [replacedKey, "client-3", "mcp"]

// getDockStops 让插件从外侧滑入并停住，循环末尾淡出；被换下的模型在 swapAt 时退开半格并变淡。
function getDockStops(plug: Plug): SceneStop[] {
  const dockedStops: SceneStop[] = [
    [0, { opacity: 0, transform: plug.from }],
    [plug.at - 0.001, { opacity: 0, transform: plug.from }],
    [plug.at + 0.04, { opacity: 1, transform: "translate(0px, 0px)" }],
  ]
  if (plug.key === replacedKey) {
    dockedStops.push(
      [swapAt, { opacity: 1, transform: "translate(0px, 0px)" }],
      [swapAt + 0.03, { opacity: 0.4, transform: "translateX(-8px)" }],
      [reset, { opacity: 0.4, transform: "translateX(-8px)" }],
      [reset + 0.04, { opacity: 0, transform: "translateX(-8px)" }],
    )
    return dockedStops
  }
  return [...dockedStops, [reset, { opacity: 1, transform: "translate(0px, 0px)" }], [reset + 0.04, { opacity: 0, transform: "translate(0px, 0px)" }]]
}

// getEdgeStops 让连线和插件同时淡入、淡出，没有插件时不画连线；被换下的模型的连线随插件一起变淡。
function getEdgeStops(plug: Plug): SceneStop[] {
  return getDockStops(plug).map(([at, style]) => [at, { opacity: style.opacity }])
}

// getLightStops 在插件到位时用信号蓝把连线点亮，随后淡回普通线；最后一段只点亮通路上的连线。
function getLightStops(plug: Plug): SceneStop[] {
  const lit = plug.at + 0.04
  const stops: SceneStop[] = [
    [0, { opacity: 0 }],
    [lit, { opacity: 0, easing: "cubic-bezier(0.4, 0, 0.2, 1)" }],
    [lit + 0.05, { opacity: 1 }],
    [lit + 0.12, { opacity: 0 }],
  ]
  // 1. 被换下的模型：通路亮起后在 swapAt 熄灭。
  if (plug.key === replacedKey) {
    return [...stops, [routeAt, { opacity: 0 }], [routeAt + 0.03, { opacity: 1 }], [swapAt, { opacity: 1 }], [swapAt + 0.03, { opacity: 0 }]]
  }
  // 2. 换上的模型：在 swapAt 之后亮起，直到循环末尾。
  if (plug.key === swappedInKey) {
    return [...stops, [swapAt + 0.02, { opacity: 0 }], [swapAt + 0.05, { opacity: 1 }], [reset, { opacity: 1 }], [reset + 0.03, { opacity: 0 }]]
  }
  // 3. 通路上的客户端和工具：一直亮到循环末尾。
  if (routeKeys.includes(plug.key)) {
    return [...stops, [routeAt, { opacity: 0 }], [routeAt + 0.03, { opacity: 1 }], [reset, { opacity: 1 }], [reset + 0.03, { opacity: 0 }]]
  }
  return stops
}

// getPortStops 让接口在插件到位时变实；被换下的模型在 swapAt 时接口变空。
function getPortStops(plug: Plug): SceneStop[] {
  const end = plug.key === replacedKey ? swapAt + 0.02 : reset
  return [
    [0, { opacity: 0 }],
    [plug.at + 0.04, { opacity: 0 }],
    [plug.at + 0.05, { opacity: 1 }],
    [end, { opacity: 1 }],
    [end + 0.04, { opacity: 0 }],
  ]
}

const captions = computed(() => [
  { text: isZh.value ? "模型：三种协议" : "models: three protocols", stops: getCaptionStops(0, 0.26) },
  { text: isZh.value ? "客户端：终端、Web、桌面端、ACP、Codex" : "clients: terminal, web, desktop, ACP, Codex", stops: getCaptionStops(0.26, 0.58) },
  { text: isZh.value ? "工具：MCP 服务" : "tools: MCP servers", stops: getCaptionStops(0.58, routeAt - 0.01) },
  { text: isZh.value ? "换一个模型，客户端和工具不用动" : "switch the model; clients and tools stay as they are", stops: getCaptionStops(routeAt - 0.01, 0.985) },
])
</script>

<template>
  <SceneCanvas class="scene-canvas--flow" :view-box="geometry.viewBox" :period="12000" :label="isZh ? '接入模型、客户端和工具的生态' : 'Plugs into models, clients and tools'">
    <text class="scene-group-label" :x="geometry.modelsLabel[0]" :y="geometry.modelsLabel[1]">{{ isZh ? "模型" : "models" }}</text>
    <text class="scene-group-label" :x="geometry.toolsLabel[0]" :y="geometry.toolsLabel[1]">{{ isZh ? "工具" : "tools" }}</text>
    <text class="scene-group-label" :x="geometry.clientsLabel[0]" :y="geometry.clientsLabel[1]">{{ isZh ? "客户端" : "clients" }}</text>

    <FlowEdge v-for="plug in plugs" :key="`edge-${plug.key}`" v-track="getEdgeStops(plug)" :points="plug.edge" tone="lo" />
    <path
      v-for="plug in plugs"
      :key="`light-${plug.key}`"
      v-track="getLightStops(plug)"
      class="edge-light"
      :d="getRounded2dPath(plug.edge, 10)"
    />

    <FlowNode v-bind="geometry.runtimeBox" :radius="12" />
    <text class="scene-label scene-label--ink" :x="geometry.runtimeLabel[0]" :y="geometry.runtimeLabel[1]" text-anchor="middle">{{ isZh ? "运行时" : "runtime" }}</text>

    <g v-for="plug in plugs" :key="`port-${plug.key}`">
      <rect class="port" :x="plug.port[0] - 3" :y="plug.port[1] - 3" width="6" height="6" rx="1.5" />
      <rect v-track="getPortStops(plug)" class="port port--docked" :x="plug.port[0] - 3" :y="plug.port[1] - 3" width="6" height="6" rx="1.5" />
    </g>

    <g v-for="plug in plugs" :key="plug.key" v-track="getDockStops(plug)">
      <FlowNode :x="plug.x" :y="plug.y" :width="plug.width" :height="plug.height" :label="plug.label" :sublabel="plug.sublabel" :radius="15" />
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
.edge-light {
  fill: none;
  stroke: var(--scene-hi);
  stroke-width: 1.5;
  stroke-linecap: butt;
  opacity: 0;
  vector-effect: none;
}

.port {
  fill: var(--scene-plate);
  stroke: var(--scene-mid);
  stroke-width: 1;
}

.port--docked {
  fill: var(--scene-edge);
  stroke: var(--scene-edge);
}
</style>
