<script setup lang="ts">
// conversation-fork：一条对话的消息沿时间线逐条出现；从第三条分叉出一个新对话；
// 随后回退标记从最新一条退回第二条，后面的消息淡出，再从那里接着写出新的消息。
import { computed } from "vue"
import { useData } from "vitepress"
import SceneCanvas from "../primitives/SceneCanvas.vue"
import { getCaptionStops, getRiseStops, getShowStops, vTrack, type SceneStop } from "../primitives/motion"

const { localeIndex } = useData()
const isZh = computed(() => localeIndex.value === "zh")

const mainY = 62
const forkY = 150
const cardWidth = 46
const reset = 0.95
const rewindAt = 0.62

// 主线上的六条消息：after 为 true 的在回退时被丢弃。
const mainMessages = [0, 1, 2, 3, 4, 5].map((index) => ({
  x: 18 + index * 70,
  isUser: index % 2 === 0,
  appear: 0.03 + index * 0.045,
  isDiscarded: index >= 2,
}))

// 回退后从第二条之后重新写出的两条消息。
const rewrittenMessages = [2, 3].map((index) => ({ x: 18 + index * 70, isUser: index % 2 === 0, appear: 0.76 + (index - 2) * 0.06 }))

const forkMessages = [3, 4].map((index) => ({ x: 18 + index * 70, isUser: index % 2 === 0, appear: 0.42 + (index - 3) * 0.06 }))

function getMainStops(message: { appear: number; isDiscarded: boolean }): SceneStop[] {
  if (!message.isDiscarded) return getRiseStops(message.appear, reset, 5)
  return getRiseStops(message.appear, rewindAt + 0.04, 5)
}

// 分叉线从第三条消息下方弯到下方时间线，用信号蓝淡入。
const forkPath = `M${18 + 2 * 70 + cardWidth / 2} ${mainY + 12} C${18 + 2 * 70 + cardWidth / 2} ${forkY - 10}, ${18 + 3 * 70 - 20} ${forkY}, ${18 + 3 * 70} ${forkY}`
const forkDrawStops: SceneStop[] = [
  [0, { opacity: 0 }],
  [0.34, { opacity: 0, easing: "cubic-bezier(0.4, 0, 0.2, 1)" }],
  [0.41, { opacity: 1 }],
  [reset, { opacity: 1 }],
  [reset + 0.03, { opacity: 0 }],
]

// 回退标记：从最新一条退回到第二条之后。
const rewindStops: SceneStop[] = [
  [0, { opacity: 0, transform: "translateX(0px)" }],
  [rewindAt - 0.05, { opacity: 0, transform: "translateX(0px)" }],
  [rewindAt - 0.03, { opacity: 1, transform: "translateX(0px)", easing: "cubic-bezier(0.5, 0, 0.2, 1)" }],
  [rewindAt + 0.06, { opacity: 1, transform: "translateX(-280px)" }],
  [rewindAt + 0.12, { opacity: 0, transform: "translateX(-280px)" }],
]

const captions = computed(() => [
  { text: isZh.value ? "对话一条条往下写" : "a conversation grows message by message", stops: getCaptionStops(0, 0.33) },
  { text: isZh.value ? "分叉：从任意一条消息开出新对话" : "fork: branch a new conversation from any message", stops: getCaptionStops(0.33, 0.57) },
  { text: isZh.value ? "回退：回到之前某一条，从那里接着写" : "rewind: go back to an earlier message and continue", stops: getCaptionStops(0.57, 0.985) },
])
</script>

<template>
  <SceneCanvas class="scene-canvas--flow" view-box="0 0 440 190" :period="12000" :label="isZh ? '对话的分叉与回退' : 'Forking and rewinding a conversation'">
    <text class="scene-group-label" x="18" y="28">{{ isZh ? "对话" : "conversation" }}</text>
    <text class="scene-group-label" x="228" y="128">{{ isZh ? "分叉出的新对话" : "forked conversation" }}</text>

    <line class="timeline" x1="18" :y1="mainY + 11" x2="432" :y2="mainY + 11" />
    <line class="timeline" x1="228" :y1="forkY + 11" x2="432" :y2="forkY + 11" />
    <path v-track="forkDrawStops" class="fork-path" :d="forkPath" />

    <g v-for="(message, index) in mainMessages" :key="`main-${index}`" v-track="getMainStops(message)">
      <rect class="card" :x="message.x" :y="mainY" :width="cardWidth" height="22" rx="6" />
      <rect class="card-bar" :class="{ 'card-bar--user': message.isUser }" :x="message.x + 8" :y="mainY + 9" :width="message.isUser ? 18 : 30" height="4" rx="2" />
    </g>
    <g v-for="(message, index) in rewrittenMessages" :key="`rewrite-${index}`" v-track="getRiseStops(message.appear, reset, 5)" style="opacity: 0">
      <rect class="card" :x="message.x" :y="mainY" :width="cardWidth" height="22" rx="6" />
      <rect class="card-bar" :class="{ 'card-bar--user': message.isUser }" :x="message.x + 8" :y="mainY + 9" :width="message.isUser ? 22 : 26" height="4" rx="2" />
    </g>
    <g v-for="(message, index) in forkMessages" :key="`fork-${index}`" v-track="getRiseStops(message.appear, reset, 5)">
      <rect class="card" :x="message.x" :y="forkY" :width="cardWidth" height="22" rx="6" />
      <rect class="card-bar" :class="{ 'card-bar--user': message.isUser }" :x="message.x + 8" :y="forkY + 9" :width="message.isUser ? 18 : 30" height="4" rx="2" />
    </g>

    <g v-track="rewindStops" class="rewind" style="opacity: 0">
      <rect class="rewind-bar" :x="18 + 5 * 70 + cardWidth + 6" :y="mainY - 4" width="3" height="30" />
      <text class="scene-label scene-label--hi scene-label--small" :x="18 + 5 * 70 + cardWidth + 2" :y="mainY - 9" text-anchor="end">
        {{ isZh ? "回退" : "rewind" }}
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
.timeline {
  stroke: var(--scene-lo);
  stroke-width: 1;
}

.fork-path {
  fill: none;
  stroke: var(--scene-hi);
  stroke-width: 1.5;
  vector-effect: none;
}

.card {
  fill: var(--scene-plate);
  stroke: var(--scene-edge);
  stroke-width: 1;
}

.card-bar {
  fill: var(--scene-lo);
}

.card-bar--user {
  fill: var(--scene-mid);
}

.rewind-bar {
  fill: var(--scene-hi);
}

.rewind {
  transform-box: view-box;
}
</style>
