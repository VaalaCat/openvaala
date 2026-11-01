<script setup lang="ts">
// unified-runtime：左边五个客户端、中间一个运行时（Agent 循环）、右边 Executor。
// 终端发来一条消息 → 运行时的循环转动 → 工具调用依次发往本机、build-box、gpu-01 并带回结果
// → 更新同时推送给所有客户端。右下角是尚未支持的 Executor，只画淡虚线。
import { computed } from "vue"
import { useData } from "vitepress"
import FlowEdge from "../primitives/FlowEdge.vue"
import FlowNode from "../primitives/FlowNode.vue"
import FlowPacket from "../primitives/FlowPacket.vue"
import SceneCanvas from "../primitives/SceneCanvas.vue"
import { getCaptionStops, getShowStops, vTrack, type SceneStop } from "../primitives/motion"
import type { Vec2 } from "../primitives/iso"

const { localeIndex } = useData()
const isZh = computed(() => localeIndex.value === "zh")

const nodeWidth = 104
const nodeHeight = 32
const runtimeBox = { x: 168, y: 104, width: 104, height: 124 }
const runtimeCenter: Vec2 = [runtimeBox.x + runtimeBox.width / 2, 176]

const clients = computed(() => [
  { label: isZh.value ? "终端" : "Terminal", sublabel: "TUI" },
  { label: "Web", sublabel: isZh.value ? "浏览器 · 手机" : "browser · phone" },
  { label: isZh.value ? "桌面端" : "Desktop", sublabel: "app" },
  { label: "Zed", sublabel: "ACP" },
  { label: "Codex", sublabel: "app-server" },
].map((client, index) => ({ ...client, y: 38 + index * 42 })))

// 已支持的 Executor：call 是工具调用出发的时刻。
const executors = computed(() => [
  { label: isZh.value ? "本机" : "this machine", sublabel: "local", call: 0.22 },
  { label: "build-box", sublabel: "ssh", call: 0.38 },
  { label: "gpu-01", sublabel: "ssh", call: 0.54 },
  { label: isZh.value ? "沙箱" : "sandbox", sublabel: isZh.value ? "内置" : "built-in", call: 0.70 },
].map((executor, index) => ({ ...executor, y: 38 + index * 42 })))

const futureExecutors = computed(() =>
  isZh.value ? ["worker", "云沙箱", "容器", "远程 VM"] : ["worker", "cloud sandbox", "container", "remote vm"],
)

function getClientEdge(y: number): Vec2[] {
  const middle = y + nodeHeight / 2
  return [[8 + nodeWidth, middle], [140, middle], [140, runtimeCenter[1]], [runtimeBox.x, runtimeCenter[1]]]
}

function getExecutorEdge(y: number): Vec2[] {
  const middle = y + nodeHeight / 2
  return [[runtimeBox.x + runtimeBox.width, runtimeCenter[1]], [300, runtimeCenter[1]], [300, middle], [328, middle]]
}

// 三段说明依次出现；静态画面只显示最后一段。
const captions = computed(() => [
  { text: isZh.value ? "1  终端发来一条消息" : "1  a message arrives from the terminal", stops: getCaptionStops(0, 0.21) },
  { text: isZh.value ? "2  工具调用分发到多台机器执行" : "2  tool calls run on several machines", stops: getCaptionStops(0.21, 0.9) },
  { text: isZh.value ? "3  所有客户端同时收到更新" : "3  every client receives the update", stops: getCaptionStops(0.9, 0.985) },
])

function getWorkStops(call: number): SceneStop[] {
  return getShowStops(call + 0.07, call + 0.12, 0.012)
}

// 循环指示点每个周期匀速转三圈，表示 Agent 循环一直在运转。
const loopStops: SceneStop[] = [
  [0, { transform: "rotate(0deg)", easing: "linear" }],
  [1, { transform: "rotate(1080deg)" }],
]
</script>

<template>
  <SceneCanvas class="scene-canvas--flow" view-box="0 0 440 294" :period="12000" :label="isZh ? '统一运行时与跨机器执行' : 'One runtime, tools on many machines'">
    <text class="scene-group-label" x="8" y="26">{{ isZh ? "客户端" : "clients" }}</text>
    <text class="scene-group-label" x="328" y="26">executors</text>

    <FlowEdge v-for="client in clients" :key="`edge-${client.label}`" :points="getClientEdge(client.y)" tone="lo" />
    <FlowEdge v-for="executor in executors" :key="`edge-${executor.label}`" :points="getExecutorEdge(executor.y)" tone="lo" />

    <!-- 输入：终端 → 运行时；更新：运行时 → 全部客户端 -->
    <FlowPacket :points="getClientEdge(clients[0].y)" :at="0.04" :span="0.1" />
    <FlowPacket
      v-for="client in clients"
      :key="`update-${client.label}`"
      :points="getClientEdge(client.y)"
      :at="0.9"
      :span="0.07"
      is-reverse
    />
    <template v-for="executor in executors" :key="`calls-${executor.label}`">
      <template v-if="executor.call >= 0">
        <FlowPacket :points="getExecutorEdge(executor.y)" :at="executor.call" :span="0.07" />
        <FlowPacket :points="getExecutorEdge(executor.y)" :at="executor.call + 0.13" :span="0.06" is-reverse />
      </template>
    </template>

    <FlowNode
      v-for="client in clients"
      :key="client.label"
      :x="8"
      :y="client.y"
      :width="nodeWidth"
      :height="nodeHeight"
      :label="client.label"
      :sublabel="client.sublabel"
    />

    <FlowNode v-bind="runtimeBox" :radius="12" />
    <text class="scene-label scene-label--ink" :x="runtimeCenter[0]" y="130" text-anchor="middle">{{ isZh ? "运行时" : "runtime" }}</text>
    <circle class="loop-ring" :cx="runtimeCenter[0]" :cy="runtimeCenter[1] + 4" r="22" />
    <g v-track="loopStops" class="loop-orbit" :style="{ transformOrigin: `${runtimeCenter[0]}px ${runtimeCenter[1] + 4}px` }">
      <circle class="loop-dot" :cx="runtimeCenter[0]" :cy="runtimeCenter[1] - 18" r="3" />
    </g>
    <text class="scene-label scene-label--small" :x="runtimeCenter[0]" y="218" text-anchor="middle">
      {{ isZh ? "Agent 循环" : "agent loop" }}
    </text>

    <g v-for="executor in executors" :key="executor.label">
      <FlowNode :x="328" :y="executor.y" :width="nodeWidth" :height="nodeHeight" :label="executor.label" :sublabel="executor.sublabel" />
      <circle
        v-if="executor.call >= 0"
        v-track="getWorkStops(executor.call)"
        class="scene-light"
        :cx="328 + nodeWidth - 10"
        :cy="executor.y + 10"
        r="2.6"
      />
    </g>

    <!-- 尚未支持的 Executor：淡虚线小方块加名称，越往下越淡 -->
    <g v-for="(future, index) in futureExecutors" :key="future" :style="{ opacity: 0.7 - index * 0.12 }">
      <rect class="future-mark" :x="330" :y="214 + index * 15" width="7" height="7" rx="1.5" />
      <text class="scene-label scene-label--small" :x="343" :y="220.5 + index * 15">{{ future }}</text>
    </g>
    <text class="scene-label scene-label--small" x="8" y="286">
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
