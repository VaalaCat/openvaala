<script setup lang="ts">
// context-compaction：上方是上下文用量条，下方是本轮的 Item。Item 逐个加入，用量条涨向压缩阈值；
// 到阈值时较早的 Item 收拢成一张摘要卡（信号蓝描边），用量条回落，这一轮带着余量继续。
import { computed } from "vue"
import { useData } from "vitepress"
import SceneCanvas from "../primitives/SceneCanvas.vue"
import { getCaptionStops, getRiseStops, vTrack, type SceneStop } from "../primitives/motion"

const { localeIndex } = useData()
const isZh = computed(() => localeIndex.value === "zh")

const reset = 0.95
const compactAt = 0.5
const barX = 18
const barWidth = 404
const threshold = 0.8

// 第一批八个 Item 会被收拢进摘要；之后再加入三个新 Item。
const olderItems = Array.from({ length: 8 }, (_, index) => ({ x: 18 + index * 42, appear: 0.04 + index * 0.05 }))
const newerItems = Array.from({ length: 3 }, (_, index) => ({ x: 102 + index * 42, appear: 0.64 + index * 0.07 }))

// 用量条：随 Item 加入涨到阈值，收拢后回落到三成，再随新 Item 小幅上涨。
const fillStops: SceneStop[] = [
  [0, { transform: "scaleX(0.04)" }],
  [0.03, { transform: "scaleX(0.04)", easing: "linear" }],
  [compactAt - 0.02, { transform: `scaleX(${threshold})` }],
  [compactAt + 0.02, { transform: `scaleX(${threshold})` }],
  [compactAt + 0.08, { transform: "scaleX(0.26)" }],
  [0.64, { transform: "scaleX(0.26)", easing: "linear" }],
  [0.85, { transform: "scaleX(0.44)" }],
  [reset, { transform: "scaleX(0.44)" }],
  [reset + 0.04, { transform: "scaleX(0.04)" }],
]

// 较早的 Item 在压缩时向左收拢并淡出。
function getOlderStops(item: { x: number; appear: number }): SceneStop[] {
  return [
    [0, { opacity: 0, transform: "translate(0px, 6px)" }],
    [item.appear - 0.001, { opacity: 0, transform: "translate(0px, 6px)" }],
    [item.appear + 0.03, { opacity: 1, transform: "translate(0px, 0px)" }],
    [compactAt, { opacity: 1, transform: "translate(0px, 0px)", easing: "cubic-bezier(0.5, 0, 0.3, 1)" }],
    [compactAt + 0.07, { opacity: 0, transform: `translate(${18 - item.x}px, 0px) scale(0.6)` }],
  ]
}

const summaryStops: SceneStop[] = getRiseStops(compactAt + 0.05, reset, 4)
const thresholdStops: SceneStop[] = [
  [0, { opacity: 0.45 }],
  [compactAt - 0.03, { opacity: 0.45 }],
  [compactAt, { opacity: 1 }],
  [compactAt + 0.08, { opacity: 0.45 }],
]

const captions = computed(() => [
  { text: isZh.value ? "Item 加入，上下文用量上涨" : "items arrive and the context fills up", stops: getCaptionStops(0, compactAt - 0.02) },
  { text: isZh.value ? "接近阈值：较早的 Item 收拢成摘要" : "near the threshold, older items fold into a summary", stops: getCaptionStops(compactAt - 0.02, 0.64) },
  { text: isZh.value ? "这一轮带着余量继续" : "the turn continues with room to spare", stops: getCaptionStops(0.64, 0.985) },
])
</script>

<template>
  <SceneCanvas class="scene-canvas--flow" view-box="0 0 440 168" :period="12000" :label="isZh ? '上下文压缩' : 'Context compaction'">
    <text class="scene-group-label" x="18" y="24">{{ isZh ? "上下文用量" : "context window" }}</text>
    <rect class="meter" :x="barX" y="34" :width="barWidth" height="14" rx="7" />
    <rect v-track="fillStops" class="meter-fill" :x="barX + 2" y="36" :width="barWidth - 4" height="10" rx="5" style="transform: scaleX(0.44)" />
    <g v-track="thresholdStops">
      <line class="threshold" :x1="barX + barWidth * threshold" y1="28" :x2="barX + barWidth * threshold" y2="54" />
      <text class="scene-label scene-label--small" :x="barX + barWidth * threshold" y="66" text-anchor="middle">
        {{ isZh ? "压缩阈值" : "compaction threshold" }}
      </text>
    </g>

    <text class="scene-group-label" x="18" y="98">{{ isZh ? "本轮 Item" : "items in this turn" }}</text>
    <g v-for="(item, index) in olderItems" :key="`older-${index}`" v-track="getOlderStops(item)" class="item" style="opacity: 0">
      <rect class="item-card" :x="item.x" y="108" width="36" height="34" rx="6" />
      <rect class="item-bar" :x="item.x + 7" y="118" :width="index % 3 === 0 ? 14 : 22" height="3" rx="1.5" />
      <rect class="item-bar" :x="item.x + 7" y="126" width="18" height="3" rx="1.5" />
    </g>
    <g v-track="summaryStops">
      <rect class="summary-card" x="18" y="108" width="78" height="34" rx="6" />
      <text class="scene-label scene-label--small scene-label--ink" x="57" y="128" text-anchor="middle">{{ isZh ? "摘要" : "summary" }}</text>
    </g>
    <g v-for="(item, index) in newerItems" :key="`newer-${index}`" v-track="getRiseStops(item.appear, reset, 6)">
      <rect class="item-card" :x="item.x" y="108" width="36" height="34" rx="6" />
      <rect class="item-bar" :x="item.x + 7" y="118" width="20" height="3" rx="1.5" />
      <rect class="item-bar" :x="item.x + 7" y="126" width="14" height="3" rx="1.5" />
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
.meter {
  fill: none;
  stroke: var(--scene-edge);
  stroke-width: 1;
}

.meter-fill {
  fill: var(--scene-mid);
  transform-box: fill-box;
  transform-origin: left center;
}

.threshold {
  stroke: var(--scene-hi);
  stroke-width: 1;
  stroke-dasharray: 2 2;
}

.item {
  transform-box: fill-box;
  transform-origin: left center;
}

.item-card {
  fill: var(--scene-plate);
  stroke: var(--scene-edge);
  stroke-width: 1;
}

.item-bar {
  fill: var(--scene-lo);
}

.summary-card {
  fill: var(--scene-plate);
  stroke: var(--scene-hi);
  stroke-width: 1;
}
</style>
