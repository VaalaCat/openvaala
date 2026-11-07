<script setup lang="ts">
// conversation-fork：原对话的消息逐条出现；分叉时每条消息复制出一份，从原处移到下面的新对话，原对话不变，
// 新对话接着写出第七条；回退选中你的第三条消息，只把它之前的两条复制下去，这条消息移进输入框，改完重发。
import { computed } from "vue"
import { useData } from "vitepress"
import SceneCanvas from "../primitives/SceneCanvas.vue"
import { getCaptionStops, getRiseStops, getShowStops, vTrack, type SceneStop } from "../primitives/motion"

const { localeIndex } = useData()
const isZh = computed(() => localeIndex.value === "zh")

const originalY = 36
const forkY = 108
const rewindY = 180
const cardWidth = 40
const cardHeight = 22
const cardGap = 56
const reset = 0.95
const forkAt = 0.28
const rewindAt = 0.53
// rewoundIndex 是回退选中的那条用户消息。
const rewoundIndex = 2

function getCardX(index: number): number {
  return 18 + index * cardGap
}

// 原对话的六条消息，偶数条是用户消息。
const originalMessages = [0, 1, 2, 3, 4, 5].map((index) => ({ index, isUser: index % 2 === 0, appear: 0.03 + index * 0.035 }))

// 分叉：六条依次从原对话复制下来（isCopy），再接着写出第七条。
const forkMessages = [
  ...originalMessages.map((message) => ({ ...message, appear: forkAt + 0.03 + message.index * 0.014, isNew: false, isCopy: true })),
  { index: 6, isUser: true, appear: forkAt + 0.16, isNew: true, isCopy: false },
]

// 回退：只复制选中消息之前的两条，改完重发后再出现这条消息和回复。
const rewindMessages = [
  { index: 0, isUser: true, appear: rewindAt + 0.03, isNew: false, isCopy: true },
  { index: 1, isUser: false, appear: rewindAt + 0.044, isNew: false, isCopy: true },
  { index: 2, isUser: true, appear: rewindAt + 0.27, isNew: true, isCopy: false },
  { index: 3, isUser: false, appear: rewindAt + 0.32, isNew: false, isCopy: false },
]

// getCopyStops 让复制出的消息从原对话里同一位置出现，再移到下面 rowY 那一行。
function getCopyStops(appear: number, rowY: number): SceneStop[] {
  const fromOriginal = `translateY(${originalY - rowY}px)`
  return [
    [0, { opacity: 0, transform: fromOriginal }],
    [appear - 0.001, { opacity: 0, transform: fromOriginal }],
    [appear, { opacity: 1, transform: fromOriginal, easing: "cubic-bezier(0.5, 0, 0.2, 1)" }],
    [appear + 0.07, { opacity: 1, transform: "translateY(0px)" }],
    [reset, { opacity: 1, transform: "translateY(0px)" }],
    [reset + 0.03, { opacity: 0, transform: "translateY(0px)" }],
  ]
}

function getRowMessageStops(message: { appear: number; isCopy: boolean }, rowY: number): SceneStop[] {
  return message.isCopy ? getCopyStops(message.appear, rowY) : getRiseStops(message.appear, reset, 5)
}

// 复制连线：从原对话左侧弯到新对话。
function getCopyPath(rowY: number): string {
  const startY = originalY + cardHeight / 2
  const endY = rowY + cardHeight / 2
  return `M18 ${startY} C4 ${startY}, 4 ${endY}, 18 ${endY}`
}

function getDrawStops(at: number): SceneStop[] {
  return [
    [0, { opacity: 0 }],
    [at, { opacity: 0, easing: "cubic-bezier(0.4, 0, 0.2, 1)" }],
    [at + 0.04, { opacity: 1 }],
    [reset, { opacity: 1 }],
    [reset + 0.03, { opacity: 0 }],
  ]
}

// 输入框：回退时出现在回退行选中消息的位置，装着那条消息，重发后清空。
const composer = { x: getCardX(rewoundIndex), y: rewindY, width: 300 }
const composerAt = rewindAt + 0.08
const resendAt = rewindAt + 0.25

// 选中的消息从原对话移进输入框。
const movingCardStops: SceneStop[] = [
  [0, { opacity: 0, transform: "translate(0px, 0px)" }],
  [composerAt - 0.001, { opacity: 0, transform: "translate(0px, 0px)" }],
  [composerAt, { opacity: 1, transform: "translate(0px, 0px)", easing: "cubic-bezier(0.5, 0, 0.2, 1)" }],
  [composerAt + 0.07, { opacity: 1, transform: `translate(8px, ${rewindY - originalY}px)` }],
  [composerAt + 0.08, { opacity: 0, transform: `translate(8px, ${rewindY - originalY}px)` }],
]

const captions = computed(() => [
  { text: isZh.value ? "对话一条条往下写" : "a conversation grows message by message", stops: getCaptionStops(0, forkAt) },
  { text: isZh.value ? "分叉：整段复制成新对话，接着往下写" : "fork: copy it into a new conversation and keep going", stops: getCaptionStops(forkAt, rewindAt) },
  {
    text: isZh.value ? "回退：复制到你的某条消息之前，这条消息回到输入框，改完重发" : "rewind: copy up to one of your messages, then edit and resend it",
    stops: getCaptionStops(rewindAt, 0.985),
  },
])
</script>

<template>
  <SceneCanvas class="scene-canvas--flow" view-box="0 0 440 214" :period="14000" :label="isZh ? '对话的分叉与回退' : 'Forking and rewinding a conversation'">
    <text class="scene-group-label" x="18" :y="originalY - 10">{{ isZh ? "原对话" : "original" }}</text>
    <text v-track="getShowStops(forkAt + 0.02, reset)" class="scene-group-label" x="18" :y="forkY - 10">{{ isZh ? "分叉 → 新对话" : "fork → new conversation" }}</text>
    <text v-track="getShowStops(rewindAt + 0.02, reset)" class="scene-group-label" x="18" :y="rewindY - 10">{{ isZh ? "回退 → 新对话" : "rewind → new conversation" }}</text>
    <text v-track="getShowStops(forkAt + 0.06, reset)" class="scene-label scene-label--small" x="432" :y="originalY - 10" text-anchor="end">
      {{ isZh ? "原对话不变" : "unchanged" }}
    </text>

    <line class="timeline" x1="18" :y1="originalY + cardHeight / 2" x2="432" :y2="originalY + cardHeight / 2" />
    <line v-track="getShowStops(forkAt + 0.02, reset)" class="timeline" x1="18" :y1="forkY + cardHeight / 2" x2="432" :y2="forkY + cardHeight / 2" />
    <line v-track="getShowStops(rewindAt + 0.02, reset)" class="timeline" x1="18" :y1="rewindY + cardHeight / 2" x2="432" :y2="rewindY + cardHeight / 2" />
    <path v-track="getDrawStops(forkAt)" class="copy-path" :d="getCopyPath(forkY)" />
    <path v-track="getDrawStops(rewindAt)" class="copy-path" :d="getCopyPath(rewindY)" />

    <g v-for="message in originalMessages" :key="`original-${message.index}`" v-track="getRiseStops(message.appear, reset, 5)">
      <rect class="card" :x="getCardX(message.index)" :y="originalY" :width="cardWidth" :height="cardHeight" rx="6" />
      <rect class="card-bar" :class="{ 'card-bar--user': message.isUser }" :x="getCardX(message.index) + 7" :y="originalY + 9" :width="message.isUser ? 16 : 26" height="4" rx="2" />
    </g>
    <!-- 回退选中的那条消息：原位置描一圈信号蓝 -->
    <rect
      v-track="getShowStops(rewindAt + 0.01, reset)"
      class="card-outline"
      :x="getCardX(rewoundIndex)"
      :y="originalY"
      :width="cardWidth"
      :height="cardHeight"
      rx="6"
    />

    <g v-for="message in forkMessages" :key="`fork-${message.index}`" v-track="getRowMessageStops(message, forkY)">
      <rect class="card" :class="{ 'card--hi': message.isNew }" :x="getCardX(message.index)" :y="forkY" :width="cardWidth" :height="cardHeight" rx="6" />
      <rect class="card-bar" :class="{ 'card-bar--user': message.isUser }" :x="getCardX(message.index) + 7" :y="forkY + 9" :width="message.isUser ? 16 : 26" height="4" rx="2" />
    </g>

    <g v-for="message in rewindMessages" :key="`rewind-${message.index}`" v-track="getRowMessageStops(message, rewindY)">
      <rect class="card" :class="{ 'card--hi': message.isNew }" :x="getCardX(message.index)" :y="rewindY" :width="cardWidth" :height="cardHeight" rx="6" />
      <rect class="card-bar" :class="{ 'card-bar--user': message.isUser }" :x="getCardX(message.index) + 7" :y="rewindY + 9" :width="message.isUser ? 20 : 26" height="4" rx="2" />
    </g>

    <!-- 输入框：先空着出现，选中的消息移进来后显示内容，重发后清空并淡出 -->
    <rect v-track="getShowStops(composerAt, resendAt + 0.02, 0.02)" class="composer" :x="composer.x" :y="composer.y" :width="composer.width" :height="cardHeight" rx="11" style="opacity: 0" />
    <g v-track="getShowStops(composerAt + 0.075, resendAt, 0.01)" style="opacity: 0">
      <rect class="card-bar card-bar--user" :x="composer.x + 12" :y="composer.y + 9" width="20" height="4" rx="2" />
      <text class="scene-label scene-label--small" :x="composer.x + 40" :y="composer.y + 14">{{ isZh ? "回到输入框，改完重发" : "back in the composer, edit and resend" }}</text>
    </g>
    <g v-track="movingCardStops" class="moving-card" style="opacity: 0">
      <rect class="card card--hi" :x="getCardX(rewoundIndex)" :y="originalY" :width="cardWidth" :height="cardHeight" rx="6" />
      <rect class="card-bar card-bar--user" :x="getCardX(rewoundIndex) + 7" :y="originalY + 9" width="16" height="4" rx="2" />
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

.copy-path {
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

.card--hi {
  stroke: var(--scene-hi);
}

.card-outline {
  fill: none;
  stroke: var(--scene-hi);
  stroke-width: 1;
}

.card-bar {
  fill: var(--scene-lo);
}

.card-bar--user {
  fill: var(--scene-mid);
}

.composer {
  fill: var(--scene-plate);
  stroke: var(--scene-hi);
  stroke-width: 1;
}

.moving-card {
  transform-box: view-box;
}
</style>
