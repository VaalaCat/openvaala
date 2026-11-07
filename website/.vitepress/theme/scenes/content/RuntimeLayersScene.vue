<script setup lang="ts">
// runtime-layers：Runtime → Conversation → Session → Turn → Agent 逐层嵌套出现；
// Agent 这一层出现后，循环和通往 Item 列表、Executor 的连线才出现；
// Agent 循环运转时产出的 Item 依次流出到右上的 Item 列表，工具调用发往右下的 Executor 并带回结果。
import { computed } from "vue"
import { useData } from "vitepress"
import FlowEdge from "../primitives/FlowEdge.vue"
import FlowNode from "../primitives/FlowNode.vue"
import FlowPacket from "../primitives/FlowPacket.vue"
import SceneCanvas from "../primitives/SceneCanvas.vue"
import { getRiseStops, getShowStops, vTrack, type SceneStop } from "../primitives/motion"
import type { Vec2 } from "../primitives/iso"

const { localeIndex } = useData()
const isZh = computed(() => localeIndex.value === "zh")

const reset = 0.95

// 五层由外到内，每层比外层缩进 12；appear 是这一层出现的时刻。
const layers = ["Runtime", "Conversation", "Session", "Turn", "Agent"].map((name, index) => ({
  name,
  x: 10 + index * 12,
  y: 14 + index * 24,
  width: 280 - index * 24,
  height: 222 - index * 30,
  appear: 0.02 + index * 0.05,
}))
const agent = layers[4]
const loopCenter: Vec2 = [agent.x + 60, agent.y + 44]
// agentLinkStops 让循环和从 Agent 出发的连线在 Agent 这一层出现之后才显示。
const agentLinkStops = getShowStops(agent.appear + 0.06, reset, 0.03)

// Item 依次流出到列表；工具调用在 0.42 和 0.66 发往不同的 Executor。
const items = [0.34, 0.42, 0.5, 0.6, 0.7, 0.78].map((at, index) => ({ at, y: 44 + index * 13 }))
const itemEdge: Vec2[] = [[agent.x + agent.width, agent.y + 22], [314, agent.y + 22], [314, 70], [326, 70]]

const executors = computed(() => [
  { label: isZh.value ? "本机" : "local", y: 150, call: 0.44 },
  { label: "ssh", y: 178, call: 0.66 },
  { label: isZh.value ? "沙箱" : "sandbox", y: 206, call: 0.78 },
])

function getExecutorEdge(y: number): Vec2[] {
  return [[agent.x + agent.width, agent.y + 64], [314, agent.y + 64], [314, y + 11], [338, y + 11]]
}

function getLayerStops(appear: number): SceneStop[] {
  return [
    [0, { opacity: 0, transform: "scale(0.97)" }],
    [appear, { opacity: 0, transform: "scale(0.97)" }],
    [appear + 0.05, { opacity: 1, transform: "scale(1)" }],
    [reset, { opacity: 1, transform: "scale(1)" }],
    [reset + 0.04, { opacity: 0, transform: "scale(1)" }],
  ]
}

const loopStops: SceneStop[] = [
  [0, { transform: "rotate(0deg)", easing: "linear" }],
  [1, { transform: "rotate(1440deg)" }],
]
</script>

<template>
  <SceneCanvas class="scene-canvas--flow" view-box="0 0 440 246" :period="14000" :label="isZh ? '运行时的分层结构' : 'Runtime layers'">
    <g v-for="layer in layers" :key="layer.name" v-track="getLayerStops(layer.appear)" class="layer">
      <rect
        class="layer-box"
        :class="{ 'layer-box--agent': layer.name === 'Agent' }"
        :x="layer.x"
        :y="layer.y"
        :width="layer.width"
        :height="layer.height"
        rx="10"
      />
      <text class="scene-label scene-label--small scene-label--ink" :x="layer.x + 9" :y="layer.y + 13">{{ layer.name }}</text>
    </g>

    <!-- Agent 出现后才显示的循环和连线 -->
    <g v-track="agentLinkStops">
      <circle class="loop-ring" :cx="loopCenter[0]" :cy="loopCenter[1]" r="16" />
      <g v-track="loopStops" class="loop-orbit" :style="{ transformOrigin: `${loopCenter[0]}px ${loopCenter[1]}px` }">
        <circle class="loop-dot" :cx="loopCenter[0]" :cy="loopCenter[1] - 16" r="2.6" />
      </g>
      <text class="scene-label scene-label--small" :x="loopCenter[0] + 28" :y="loopCenter[1] - 4">model</text>
      <text class="scene-label scene-label--small" :x="loopCenter[0] + 28" :y="loopCenter[1] + 9">⇄ tools</text>
      <FlowEdge :points="itemEdge" tone="lo" />
      <FlowEdge v-for="executor in executors" :key="`edge-${executor.label}`" :points="getExecutorEdge(executor.y)" tone="lo" />
    </g>

    <!-- Item 列表 -->
    <text class="scene-group-label" x="326" y="30">items</text>
    <FlowPacket v-for="item in items" :key="`packet-${item.at}`" :points="itemEdge" :at="item.at - 0.035" :span="0.035" />
    <rect
      v-for="(item, index) in items"
      :key="`item-${item.at}`"
      v-track="getRiseStops(item.at, reset, 4)"
      class="item-bar"
      :class="{ 'item-bar--tool': index === 1 || index === 4 }"
      x="330"
      :y="item.y"
      :width="[86, 64, 100, 72, 64, 92][index]"
      height="5"
      rx="2.5"
    />

    <!-- Executor -->
    <text class="scene-group-label" x="338" y="140">executors</text>
    <template v-for="executor in executors" :key="executor.label">
      <template v-if="executor.call >= 0">
        <FlowPacket :points="getExecutorEdge(executor.y)" :at="executor.call" :span="0.05" />
        <FlowPacket :points="getExecutorEdge(executor.y)" :at="executor.call + 0.1" :span="0.05" is-reverse />
      </template>
      <FlowNode :x="338" :y="executor.y" :width="94" :height="22" :label="executor.label" :radius="6" />
      <circle
        v-if="executor.call >= 0"
        v-track="getShowStops(executor.call + 0.05, executor.call + 0.1, 0.01)"
        class="scene-light"
        cx="422"
        :cy="executor.y + 11"
        r="2.4"
      />
    </template>
  </SceneCanvas>
</template>

<style scoped>
.layer {
  transform-box: fill-box;
  transform-origin: center;
}

.layer-box {
  fill: var(--scene-plate);
  stroke: var(--scene-mid);
  stroke-width: 1;
}

.layer-box--agent {
  stroke: var(--scene-edge);
}

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

.item-bar {
  fill: var(--scene-lo);
}

.item-bar--tool {
  fill: var(--scene-mid);
}
</style>
