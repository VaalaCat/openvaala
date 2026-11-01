<script setup lang="ts">
// ecosystem：运行时三面各有接口——左边接模型（OpenAI Chat、OpenAI Responses、Claude Messages），
// 右边接客户端（终端、Web、桌面端、Zed 经 ACP、Codex 经 app-server），上边经 MCP 接工具。
// 各插件依次滑入接口，接通时连线用信号蓝淡入；最后所有连线一起亮一次。
import { computed } from "vue"
import { useData } from "vitepress"
import FlowEdge from "../primitives/FlowEdge.vue"
import FlowNode from "../primitives/FlowNode.vue"
import SceneCanvas from "../primitives/SceneCanvas.vue"
import { getCaptionStops, vTrack, type SceneStop } from "../primitives/motion"
import { getRounded2dPath, type Vec2 } from "../primitives/iso"

const { localeIndex } = useData()
const isZh = computed(() => localeIndex.value === "zh")

const runtimeBox = { x: 170, y: 118, width: 100, height: 96 }
const chipWidth = 124
const chipHeight = 30

interface Plug {
  key: string
  label: string
  sublabel?: string
  x: number
  y: number
  edge: Vec2[]
  port: Vec2
  at: number
  // from 是滑入前的位移方向。
  from: string
}

const plugs = computed<Plug[]>(() => {
  const models = [
    { label: "OpenAI", sublabel: "Chat Completions" },
    { label: "OpenAI", sublabel: "Responses" },
    { label: "Claude", sublabel: "Messages" },
  ].map((model, index) => {
    const y = 104 + index * 38
    const portY = 146 + index * 20
    return {
      key: `model-${index}`,
      ...model,
      x: 8,
      y,
      edge: [[8 + chipWidth, y + chipHeight / 2], [151, y + chipHeight / 2], [151, portY], [runtimeBox.x, portY]] as Vec2[],
      port: [runtimeBox.x, portY] as Vec2,
      at: 0.04 + index * 0.06,
      from: "translateX(-14px)",
    }
  })
  const clients = [
    { label: isZh.value ? "终端" : "Terminal", sublabel: "TUI" },
    { label: "Web", sublabel: isZh.value ? "浏览器 · 手机" : "browser · phone" },
    { label: isZh.value ? "桌面端" : "Desktop", sublabel: "app" },
    { label: "Zed", sublabel: "ACP" },
    { label: "Codex", sublabel: "app-server" },
  ].map((client, index) => {
    const y = 66 + index * 36
    const portY = 134 + index * 16
    return {
      key: `client-${index}`,
      ...client,
      x: 308,
      y,
      edge: [[runtimeBox.x + runtimeBox.width, portY], [289, portY], [289, y + chipHeight / 2], [308, y + chipHeight / 2]] as Vec2[],
      port: [runtimeBox.x + runtimeBox.width, portY] as Vec2,
      at: 0.28 + index * 0.05,
      from: "translateX(14px)",
    }
  })
  const tools = {
    key: "mcp",
    label: "MCP",
    sublabel: isZh.value ? "工具服务" : "tool servers",
    x: 158,
    y: 34,
    edge: [[220, 64], [220, runtimeBox.y]] as Vec2[],
    port: [220, runtimeBox.y] as Vec2,
    at: 0.6,
    from: "translateY(-12px)",
  }
  return [...models, ...clients, tools]
})

const reset = 0.94

// getDockStops 让插件从外侧滑入并停住，循环末尾淡出。
function getDockStops(plug: Plug): SceneStop[] {
  return [
    [0, { opacity: 0, transform: plug.from }],
    [plug.at - 0.001, { opacity: 0, transform: plug.from }],
    [plug.at + 0.04, { opacity: 1, transform: "translate(0px, 0px)" }],
    [reset, { opacity: 1, transform: "translate(0px, 0px)" }],
    [reset + 0.04, { opacity: 0, transform: "translate(0px, 0px)" }],
  ]
}

// getLightStops 在插件到位时用信号蓝把连线点亮，随后淡回普通线；0.74 时所有连线再一起亮一次。
function getLightStops(plug: Plug): SceneStop[] {
  const lit = plug.at + 0.04
  return [
    [0, { opacity: 0 }],
    [lit, { opacity: 0, easing: "cubic-bezier(0.4, 0, 0.2, 1)" }],
    [lit + 0.05, { opacity: 1 }],
    [lit + 0.12, { opacity: 0 }],
    [0.74, { opacity: 0 }],
    [0.78, { opacity: 0.85 }],
    [0.86, { opacity: 0 }],
  ]
}

function getPortStops(plug: Plug): SceneStop[] {
  return [
    [0, { opacity: 0 }],
    [plug.at + 0.04, { opacity: 0 }],
    [plug.at + 0.05, { opacity: 1 }],
    [reset, { opacity: 1 }],
    [reset + 0.04, { opacity: 0 }],
  ]
}

const captions = computed(() => [
  { text: isZh.value ? "模型：三种协议" : "models: three protocols", stops: getCaptionStops(0, 0.26) },
  { text: isZh.value ? "客户端：终端、Web、桌面端、ACP、Codex" : "clients: terminal, web, desktop, ACP, Codex", stops: getCaptionStops(0.26, 0.58) },
  { text: isZh.value ? "工具：MCP 服务" : "tools: MCP servers", stops: getCaptionStops(0.58, 0.72) },
  { text: isZh.value ? "换掉其中一块，其余不用动" : "swap one piece, keep the rest", stops: getCaptionStops(0.72, 0.985) },
])
</script>

<template>
  <SceneCanvas class="scene-canvas--flow" view-box="0 0 440 280" :period="12000" :label="isZh ? '接入模型、客户端和工具的生态' : 'Plugs into models, clients and tools'">
    <text class="scene-group-label" x="8" y="94">{{ isZh ? "模型" : "models" }}</text>
    <text class="scene-group-label" x="308" y="56">{{ isZh ? "客户端" : "clients" }}</text>
    <text class="scene-group-label" x="220" y="24" text-anchor="middle">{{ isZh ? "工具" : "tools" }}</text>

    <FlowEdge v-for="plug in plugs" :key="`edge-${plug.key}`" :points="plug.edge" tone="lo" />
    <path
      v-for="plug in plugs"
      :key="`light-${plug.key}`"
      v-track="getLightStops(plug)"
      class="edge-light"
      :d="getRounded2dPath(plug.edge, 10)"
    />

    <FlowNode v-bind="runtimeBox" :radius="12" />
    <text class="scene-label scene-label--ink" x="220" y="170" text-anchor="middle">{{ isZh ? "运行时" : "runtime" }}</text>

    <g v-for="plug in plugs" :key="`port-${plug.key}`">
      <rect class="port" :x="plug.port[0] - 3" :y="plug.port[1] - 3" width="6" height="6" rx="1.5" />
      <rect v-track="getPortStops(plug)" class="port port--docked" :x="plug.port[0] - 3" :y="plug.port[1] - 3" width="6" height="6" rx="1.5" />
    </g>

    <g v-for="plug in plugs" :key="plug.key" v-track="getDockStops(plug)">
      <FlowNode :x="plug.x" :y="plug.y" :width="plug.key === 'mcp' ? 124 : chipWidth" :height="chipHeight" :label="plug.label" :sublabel="plug.sublabel" :radius="15" />
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
