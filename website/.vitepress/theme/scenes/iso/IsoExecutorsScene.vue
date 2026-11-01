<script setup lang="ts">
// iso-executors：中间是运行时，四个已支持的 Executor 沿四条轴排开，工具调用依次发往本机、build-box、gpu-01 和内置沙箱；
// 左右两侧是尚未支持的 Executor 的淡虚线轮廓，越往外越淡，不接收数据包。
import { computed } from "vue"
import { useData } from "vitepress"
import IsoCable from "../primitives/IsoCable.vue"
import IsoPacket from "../primitives/IsoPacket.vue"
import IsoSlab from "../primitives/IsoSlab.vue"
import SceneCanvas from "../primitives/SceneCanvas.vue"
import { getShowStops, vTrack, type SceneStop } from "../primitives/motion"
import { project, type Vec2, type Vec3 } from "../primitives/iso"

const { localeIndex } = useData()
const isZh = computed(() => localeIndex.value === "zh")

// 世界坐标与屏幕坐标的换算：screenToWorld 把画面上想要的位置换回地面上的世界坐标。
function screenToWorld([sx, sy]: Vec2): [number, number] {
  const difference = sx / Math.cos(Math.PI / 6)
  return [(difference + 2 * sy) / 2, (2 * sy - difference) / 2]
}

// 四个已支持的 Executor：call 是工具调用在循环中出发的时刻，labelSide 决定标签在物体上方还是下方。
const executors = computed(() => [
  { key: "build", at: [-16, -106, 0] as Vec3, height: 32, call: 0.28, label: "ssh: build-box", labelSide: "above" },
  { key: "local", at: [-106, -16, 0] as Vec3, height: 8, call: 0.04, label: isZh.value ? "本机" : "this machine", labelSide: "above" },
  { key: "gpu", at: [74, -16, 0] as Vec3, height: 28, call: 0.52, label: "ssh: gpu-01", labelSide: "below" },
  { key: "sandbox", at: [-16, 74, 0] as Vec3, height: 12, call: 0.72, label: isZh.value ? "内置沙箱" : "built-in sandbox", labelSide: "below" },
])

const executorSize = 32

function getCable(executor: { at: Vec3 }): Vec3[] {
  const [x, y] = executor.at
  const center: [number, number] = [x + executorSize / 2, y + executorSize / 2]
  const direction = [Math.sign(Math.round(center[0])), Math.sign(Math.round(center[1]))]
  return [
    [direction[0] * 20, direction[1] * 20, 0],
    [center[0] - direction[0] * 16, center[1] - direction[1] * 16, 0],
  ]
}

// getLabelPosition 返回标签的屏幕坐标：上方取顶面最高点之上，下方取底面最低点之下。
function getLabelPosition(executor: { at: Vec3; height: number; labelSide: string }): Vec2 {
  const [x, y] = executor.at
  if (executor.labelSide === "above") {
    const [sx, sy] = project([x, y, executor.height])
    return [sx, sy - 9]
  }
  const [sx, sy] = project([x + executorSize, y + executorSize, 0])
  return [sx, sy + 15]
}

function getTopCenter(executor: { at: Vec3; height: number }): Vec2 {
  return project([executor.at[0] + executorSize / 2, executor.at[1] + executorSize / 2, executor.height])
}

// 两侧尚未支持的种类：屏幕位置越靠外越淡，偶尔有一个轻轻亮起。
const futureExecutors = computed(() =>
  [
    { screen: [-168, -18] as Vec2, label: "worker", opacity: 0.6, shimmer: 0.9 },
    { screen: [-188, 46] as Vec2, label: isZh.value ? "云沙箱" : "cloud sandbox", opacity: 0.38, shimmer: -1 },
    { screen: [168, -18] as Vec2, label: isZh.value ? "容器" : "container", opacity: 0.6, shimmer: -1 },
    { screen: [188, 46] as Vec2, label: isZh.value ? "远程 VM" : "remote vm", opacity: 0.38, shimmer: 0.4 },
  ].map((future) => {
    const [x, y] = screenToWorld(future.screen)
    return { ...future, at: [x - 12, y - 12, 0] as Vec3 }
  }),
)

// getWorkStops 让 Executor 顶面的工作灯在数据包到达后亮起、结果返回前熄灭。
function getWorkStops(call: number): SceneStop[] {
  return getShowStops(call + 0.09, call + 0.16, 0.015)
}

function getShimmerStops(at: number, opacity: number): SceneStop[] {
  if (at < 0) return [[0, { opacity }]]
  return [
    [at - 0.06, { opacity }],
    [at, { opacity: Math.min(1, opacity + 0.3) }],
    [at + 0.08, { opacity }],
  ]
}
</script>

<template>
  <SceneCanvas view-box="-242 -112 484 220" :period="11000">
    <!-- 两侧：尚未支持的 Executor，只有淡虚线轮廓，不接收数据包 -->
    <g
      v-for="future in futureExecutors"
      :key="future.label"
      v-track="getShimmerStops(future.shimmer, future.opacity)"
      :style="{ opacity: future.opacity }"
    >
      <IsoSlab :at="future.at" :size="[24, 24, 10]" :radius="5" tone="ghost" />
      <text class="scene-label" :x="future.screen[0]" :y="future.screen[1] + 26" text-anchor="middle">{{ future.label }}</text>
    </g>
    <text class="scene-label scene-label--small" x="0" y="104" text-anchor="middle">
      {{ isZh ? "更多 Executor 陆续支持 …" : "more executors coming …" }}
    </text>

    <!-- 线缆与数据包：发出工具调用，再带回结果 -->
    <IsoCable v-for="executor in executors" :key="`cable-${executor.key}`" :points="getCable(executor)" />
    <template v-for="executor in executors" :key="`packet-${executor.key}`">
      <IsoPacket :points="getCable(executor)" :at="executor.call" :span="0.08" />
      <IsoPacket :points="getCable(executor)" :at="executor.call + 0.17" :span="0.07" is-reverse />
    </template>

    <!-- 从后往前画：build-box、本机、运行时、gpu-01、沙箱 -->
    <IsoSlab
      :at="executors[0].at"
      :size="[executorSize, executorSize, 32]"
      :radius="4"
      has-corner-edge
      :rows="[[7, 9, 18], [7, 15, 18], [7, 21, 11]]"
    />
    <IsoSlab :at="executors[1].at" :size="[executorSize, executorSize, 8]" :radius="6" :insets="[[5, 5, 22, 22, 3]]" />
    <IsoSlab :at="[-20, -20, 0]" :size="[40, 40, 22]" :radius="8" has-corner-edge :insets="[[9, 9, 22, 22, 11]]" />
    <IsoSlab
      :at="executors[2].at"
      :size="[executorSize, executorSize, 28]"
      :radius="4"
      has-corner-edge
      :insets="[[5, 5, 9, 9, 4.5], [18, 18, 9, 9, 4.5]]"
    />
    <IsoSlab :at="executors[3].at" :size="[executorSize, executorSize, 12]" :radius="7" tone="edge" :insets="[[6, 6, 20, 20, 4]]" />

    <!-- 工作灯：数据包到达时在对应 Executor 顶上亮起 -->
    <ellipse
      v-for="executor in executors"
      :key="`light-${executor.key}`"
      v-track="getWorkStops(executor.call)"
      class="scene-light"
      :cx="getTopCenter(executor)[0]"
      :cy="getTopCenter(executor)[1]"
      rx="4.5"
      ry="2.6"
    />

    <text
      v-for="executor in executors"
      :key="`label-${executor.key}`"
      class="scene-label scene-label--ink"
      :x="getLabelPosition(executor)[0]"
      :y="getLabelPosition(executor)[1]"
      text-anchor="middle"
    >
      {{ executor.label }}
    </text>
  </SceneCanvas>
</template>
