<script setup lang="ts">
// iso-quickstart：快速开始的三步沿一条线缆排开——安装包、模型芯片、对话卡片；芯片右上方是两家模型服务
// （OpenAI、Claude），后面三个小圆点表示其他服务。安装包先落到地上；信号蓝数据包沿线缆走到模型芯片，
// 芯片轻轻弹一下，同时一条分叉线缆把它接到两家模型服务上，表示模型已添加；数据包继续走到对话卡片，
// 卡片上出现用户消息，回复逐行写出。静态画面是三步都已完成。
import { computed } from "vue"
import { useData } from "vitepress"
import IsoCable from "../primitives/IsoCable.vue"
import IsoPacket from "../primitives/IsoPacket.vue"
import IsoSlab from "../primitives/IsoSlab.vue"
import SceneCanvas from "../primitives/SceneCanvas.vue"
import { getFaceLine, getFaceRoundedRect, getIsoPrism, project, type Vec3 } from "../primitives/iso"
import { getShowStops, vTrack, type SceneStop } from "../primitives/motion"

const { localeIndex } = useData()
const isZh = computed(() => localeIndex.value === "zh")

// 三个物体沿 x 轴排开，中心都在 y = 0 上。
const boxAt: Vec3 = [-110, -18, 0]
const boxSize: Vec3 = [36, 36, 28]
const socketAt: Vec3 = [-24, -24, 0]
const socketSize: Vec3 = [48, 48, 4]
const chipAt: Vec3 = [-17, -17, 4]
const chipSize: Vec3 = [34, 34, 6]
const cardAt: Vec3 = [62, -30, 0]
const cardSize: Vec3 = [72, 60, 5]
const cardFace = getIsoPrism("floor", cardAt, cardSize, 9).getFacePoint

// 两家模型服务排在芯片右上方，沿 x 方向错开；providerLinks 从插座背面分叉接到它们的前沿。
const providerSize: Vec3 = [26, 22, 6]
const providers = [
  { label: "OpenAI", center: [-20, -80] as [number, number] },
  { label: "Claude", center: [40, -80] as [number, number] },
].map((provider) => ({
  ...provider,
  at: [provider.center[0] - providerSize[0] / 2, provider.center[1] - providerSize[1] / 2, 0] as Vec3,
  labelAt: project([provider.center[0] - providerSize[0] / 2, provider.center[1] - providerSize[1] / 2, providerSize[2]]),
}))
const providerLinkY = -52
const providerLinks: Vec3[][] = providers.map(({ center }) => [
  [0, socketAt[1], 0],
  [0, providerLinkY, 0],
  [center[0], providerLinkY, 0],
  [center[0], center[1] + providerSize[1] / 2, 0],
])
// 第二家服务后面三个小圆点表示其他服务，沿同一排继续排开。
const moreProviderDots = [62, 69, 76].map((x) => project([x, -80, 0]))

// 线缆只画两个物体之间露出来的两段，数据包沿同样的点走。
const cableToChip: Vec3[] = [
  [boxAt[0] + boxSize[0], 0, 0],
  [socketAt[0], 0, 0],
]
const cableToCard: Vec3[] = [
  [socketAt[0] + socketSize[0], 0, 0],
  [cardAt[0], 0, 0],
]

// 时间线：安装包落地，数据包两段路程各走 packetSpan，到达的时刻物体弹一下。
const boxLandAt = 0.1
const packetSpan = 0.14
const chipPacketAt = 0.16
const chipReadyAt = chipPacketAt + packetSpan
const cardPacketAt = 0.37
const cardReadyAt = cardPacketAt + packetSpan

// 安装包从上方落下，触底后轻轻一顿停稳；循环末尾慢慢升回起点，接上下一轮的下落。
const boxLift = "translateY(-10px)"
const boxDropStops: SceneStop[] = [
  [0.02, { transform: boxLift, easing: "cubic-bezier(0.55, 0, 0.8, 0.3)" }],
  [boxLandAt, { transform: "translateY(0px)" }],
  [boxLandAt + 0.02, { transform: "translateY(-1.5px)" }],
  [boxLandAt + 0.04, { transform: "translateY(0px)" }],
  [0.92, { transform: "translateY(0px)", easing: "cubic-bezier(0.4, 0, 0.2, 1)" }],
  [0.99, { transform: boxLift }],
]

// getArriveStops 让物体在数据包到达时轻轻弹起再落回。
function getArriveStops(at: number): SceneStop[] {
  return [
    [at, { transform: "translateY(0px)" }],
    [at + 0.025, { transform: "translateY(-4px)", easing: "ease-in" }],
    [at + 0.06, { transform: "translateY(0px)" }],
  ]
}

// 对话卡片：用户消息先出现，回复三行依次从左向右写出。
const userBubble = getFaceRoundedRect(cardFace, [34, 9, 30, 11, 5.5])
const replyRows = [
  { b: 30, length: 50, startAt: cardReadyAt + 0.1 },
  { b: 39, length: 38, startAt: cardReadyAt + 0.18 },
  { b: 48, length: 46, startAt: cardReadyAt + 0.26 },
]

function getRowStops(startAt: number): SceneStop[] {
  return [
    [startAt - 0.001, { opacity: 0, transform: "scale(0)" }],
    [startAt, { opacity: 1, transform: "scale(0)", easing: "linear" }],
    [startAt + 0.06, { opacity: 1, transform: "scale(1)" }],
    [0.93, { opacity: 1, transform: "scale(1)" }],
    [0.96, { opacity: 0, transform: "scale(1)" }],
  ]
}

// 每一步的标签右对齐写在物体前角的左下方，三条标签填在斜线左下方的空白里。
const steps = computed(() => [
  { label: isZh.value ? "1 安装" : "1 install", at: project([boxAt[0] + boxSize[0], boxAt[1] + boxSize[1], 0]) },
  { label: isZh.value ? "2 添加模型" : "2 add a model", at: project([socketAt[0] + socketSize[0], socketAt[1] + socketSize[1], 0]) },
  { label: isZh.value ? "3 开始对话" : "3 start a chat", at: project([cardAt[0] + cardSize[0], cardAt[1] + cardSize[1], 0]) },
])
</script>

<template>
  <SceneCanvas view-box="-160 -102 312 208" :period="10000">
    <IsoCable :points="cableToChip" />
    <IsoCable :points="cableToCard" />
    <IsoPacket :points="cableToChip" :at="chipPacketAt" :span="packetSpan" />
    <IsoPacket :points="cableToCard" :at="cardPacketAt" :span="packetSpan" />

    <g v-track="boxDropStops">
      <IsoSlab :at="boxAt" :size="boxSize" :radius="4" has-corner-edge :rows="[[4, 18, 28]]" />
    </g>

    <g v-track="getShowStops(chipReadyAt + 0.02, 0.93, 0.03)">
      <IsoCable v-for="(link, index) in providerLinks" :key="index" :points="link" :radius="6" />
    </g>
    <IsoSlab
      v-for="provider in providers"
      :key="provider.label"
      :at="provider.at"
      :size="providerSize"
      :radius="5"
      :insets="[[6, 6, 14, 10, 3]]"
    />

    <IsoSlab :at="socketAt" :size="socketSize" :radius="8" />
    <g v-track="getArriveStops(chipReadyAt)">
      <IsoSlab :at="chipAt" :size="chipSize" :radius="5" :insets="[[8, 8, 18, 18, 3]]" />
    </g>

    <g v-track="getArriveStops(cardReadyAt)">
      <IsoSlab :at="cardAt" :size="cardSize" :radius="9" />
      <path v-track="getShowStops(cardReadyAt + 0.03, 0.93, 0.03)" class="user-bubble" :d="userBubble" />
      <path
        v-for="row in replyRows"
        :key="row.b"
        v-track="getRowStops(row.startAt)"
        class="reply-row"
        :d="getFaceLine(cardFace, [9, row.b, row.length])"
      />
    </g>

    <text
      v-for="step in steps"
      :key="step.label"
      class="scene-label scene-label--ink"
      :x="step.at[0] - 8"
      :y="step.at[1] + 13"
      text-anchor="end"
    >
      {{ step.label }}
    </text>
    <text
      v-for="provider in providers"
      :key="`label-${provider.label}`"
      class="scene-label"
      :x="provider.labelAt[0]"
      :y="provider.labelAt[1] - 6"
      text-anchor="middle"
    >
      {{ provider.label }}
    </text>
    <circle v-for="(dot, index) in moreProviderDots" :key="index" class="more-provider-dot" :cx="dot[0]" :cy="dot[1]" r="1.3" />
  </SceneCanvas>
</template>

<style scoped>
.more-provider-dot {
  fill: var(--scene-mid);
}

.user-bubble {
  fill: none;
  stroke: var(--scene-mid);
}

.reply-row {
  fill: none;
  stroke: var(--scene-lo);
  stroke-width: 2;
  stroke-linecap: round;
  transform-box: fill-box;
  transform-origin: 0 0;
}
</style>
