<script setup lang="ts">
// iso-codex：左边是 Codex 的对话面板，右边是 OpenVaala app-server，一条线缆相连；
// 用户消息作为请求发出，app-server 依次推回事件，每个事件到达后面板上多出一行回复。
// 静态画面是请求和三行回复都已出现。
import IsoCable from "../primitives/IsoCable.vue"
import IsoPacket from "../primitives/IsoPacket.vue"
import IsoSlab from "../primitives/IsoSlab.vue"
import SceneCanvas from "../primitives/SceneCanvas.vue"
import { getFaceLine, getFaceRoundedRect, getIsoPrism, project, type Vec3 } from "../primitives/iso"
import { getShowStops, vTrack } from "../primitives/motion"

const panelAt: Vec3 = [-126, 22, 0]
const panelSize: Vec3 = [128, 98, 8]
const panelFace = getIsoPrism("floor", panelAt, panelSize, 11).getFacePoint
const serverAt: Vec3 = [32, -100, 0]
const serverSize: Vec3 = [58, 58, 30]

// 线缆从面板右侧出发，经前方拐角，接到 app-server 的左前侧。
const cable: Vec3[] = [
  [2, 71, 3],
  [61, 71, 3],
  [61, -42, 3],
]

const requestAt = 0.1
const eventsAt = [0.34, 0.47, 0.6]
const packetSpan = 0.13
const replyRows = [
  { b: 42, length: 92 },
  { b: 54, length: 70 },
  { b: 66, length: 104 },
]

const userBubble = getFaceRoundedRect(panelFace, [58, 12, 60, 16, 7])
const codexLabel = project([2, 120, 0])
const serverLabel = project([90, -42, 0])
</script>

<template>
  <SceneCanvas view-box="-222 -96 406 196" :period="10000">
    <IsoCable :points="cable" />
    <IsoPacket :points="cable" :at="requestAt" :span="packetSpan" />
    <IsoPacket v-for="eventAt in eventsAt" :key="eventAt" :points="cable" :at="eventAt" :span="packetSpan" is-reverse />

    <IsoSlab :at="serverAt" :size="serverSize" :radius="9" has-corner-edge :insets="[[14, 14, 30, 30, 15]]" />
    <IsoSlab :at="panelAt" :size="panelSize" :radius="11" />

    <path v-track="getShowStops(0.04, 0.93, 0.03)" class="user-bubble" :d="userBubble" />
    <path
      v-for="(row, index) in replyRows"
      :key="row.b"
      v-track="getShowStops(eventsAt[index] + packetSpan, 0.93, 0.03)"
      class="iso-row"
      :d="getFaceLine(panelFace, [12, row.b, row.length])"
    />

    <text class="scene-label scene-label--ink" :x="codexLabel[0]" :y="codexLabel[1] + 16" text-anchor="middle">Codex</text>
    <text class="scene-label scene-label--ink" :x="serverLabel[0]" :y="serverLabel[1] + 16" text-anchor="middle">app-server</text>
  </SceneCanvas>
</template>

<style scoped>
.user-bubble {
  fill: none;
  stroke: var(--scene-mid);
}
</style>
