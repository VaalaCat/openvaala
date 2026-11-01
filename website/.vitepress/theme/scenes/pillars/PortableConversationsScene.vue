<script setup lang="ts">
// portable-conversations：左边是日志存储（每个对话目录里的 JSONL），右边是记录存储（SQLite 索引 metadata.db）。
// 记录逐行追加并写进索引 → 索引被清空 → openvaala reindex 按 JSONL 逐行重建 → 把对话目录拷到另一台机器。
// 底部是尚未支持的存储后端，只画淡虚线，不接收数据。
import { computed } from "vue"
import { useData } from "vitepress"
import FlowEdge from "../primitives/FlowEdge.vue"
import FlowPacket from "../primitives/FlowPacket.vue"
import SceneCanvas from "../primitives/SceneCanvas.vue"
import { getCaptionStops, getRiseStops, getShowStops, vTrack, type SceneStop } from "../primitives/motion"
import type { Vec2 } from "../primitives/iso"

const { localeIndex } = useData()
const isZh = computed(() => localeIndex.value === "zh")

const reset = 0.95
const wipeAt = 0.37

// 四行 metadata.jsonl 记录：append 是首次写入时刻，rebuild 是 reindex 读到这一行的时刻。
const records = [
  { text: '{"kind":"session_saved",…}', append: 0.04, rebuild: 0.47 },
  { text: '{"kind":"conversation_saved",…}', append: 0.1, rebuild: 0.51 },
  { text: '{"kind":"turn_saved",…}', append: 0.16, rebuild: 0.55 },
  { text: '{"kind":"turn_saved",…}', append: 0.22, rebuild: 0.59 },
].map((record, index) => ({ ...record, y: 84 + index * 15 }))

// 同一目录里的 Session 日志：Item 随 Turn 记录一起追加。
const sessionItems = [
  { at: 0.17, width: 150 },
  { at: 0.2, width: 118 },
  { at: 0.23, width: 164 },
  { at: 0.26, width: 96 },
]

const indexEdge: Vec2[] = [[216, 100], [260, 100]]

function getIndexRowStops(record: { append: number; rebuild: number }): SceneStop[] {
  const shown = record.append + 0.045
  const rebuilt = record.rebuild + 0.035
  return [
    [0, { opacity: 0 }],
    [shown - 0.001, { opacity: 0 }],
    [shown + 0.015, { opacity: 1 }],
    [wipeAt, { opacity: 1 }],
    [wipeAt + 0.025, { opacity: 0 }],
    [rebuilt - 0.001, { opacity: 0 }],
    [rebuilt + 0.015, { opacity: 1 }],
    [reset, { opacity: 1 }],
    [reset + 0.03, { opacity: 0 }],
  ]
}

// 拷贝卡片画在另一台机器的位置上；动画从左侧对话目录的位置滑过来。
const copyStops: SceneStop[] = [
  [0, { opacity: 0, transform: "translate(-256px, -138px)" }],
  [0.68, { opacity: 0, transform: "translate(-256px, -138px)" }],
  [0.7, { opacity: 1, transform: "translate(-256px, -138px)", easing: "cubic-bezier(0.5, 0, 0.2, 1)" }],
  [0.8, { opacity: 1, transform: "translate(0px, 0px)" }],
  [reset, { opacity: 1, transform: "translate(0px, 0px)" }],
  [reset + 0.03, { opacity: 0, transform: "translate(0px, 0px)" }],
]

const futureBackends = computed(() => [
  { label: isZh.value ? "KV 存储" : "KV store", width: 62 },
  { label: "Postgres", width: 62 },
  { label: isZh.value ? "文档数据库" : "document DB", width: 76 },
  { label: "S3", width: 40 },
  { label: "…", width: 26 },
])

function getBackendX(index: number): number {
  return 8 + futureBackends.value.slice(0, index).reduce((sum, backend) => sum + backend.width + 8, 0)
}

const captions = computed(() => [
  { text: isZh.value ? "1  记录追加进 JSONL，同时写入 SQLite 索引" : "1  records append to JSONL and land in the SQLite index", stops: getCaptionStops(0, 0.35) },
  { text: isZh.value ? "2  删掉索引，历史还在" : "2  drop the index, the history is still there", stops: getCaptionStops(0.35, 0.45) },
  { text: isZh.value ? "3  openvaala reindex 按 JSONL 重建索引" : "3  openvaala reindex rebuilds it from the JSONL", stops: getCaptionStops(0.45, 0.67) },
  { text: isZh.value ? "4  把对话目录拷到另一台机器，接着用" : "4  copy the folder to another machine and keep going", stops: getCaptionStops(0.67, 0.985) },
])
</script>

<template>
  <SceneCanvas class="scene-canvas--flow" view-box="0 0 440 288" :period="15000" :label="isZh ? '对话以 JSONL 保存，索引可重建，目录可拷走' : 'Conversations as JSONL, a rebuildable index, folders you can move'">
    <text class="scene-group-label" x="8" y="22">{{ isZh ? "日志存储 · JSONL" : "log storage · JSONL" }}</text>
    <text class="scene-group-label" x="260" y="22">{{ isZh ? "记录存储 · SQLite" : "record storage · SQLite" }}</text>

    <!-- 对话目录 -->
    <rect class="panel" x="8" y="30" width="208" height="198" rx="10" />
    <path class="folder-glyph" d="M18 40h8l2 2.5h9v8H18z" />
    <text class="scene-label scene-label--ink" x="44" y="48.5">conversations/01a1…/</text>
    <line class="scene-rule" x1="8" y1="58" x2="216" y2="58" />
    <text class="scene-label scene-label--small" x="18" y="72">metadata.jsonl</text>
    <g v-for="record in records" :key="record.y">
      <rect v-track="getShowStops(record.rebuild, record.rebuild + 0.035, 0.008)" class="scan-mark" x="14" :y="record.y - 6" width="2" height="8" />
      <text v-track="getRiseStops(record.append, reset, 4)" class="scene-label scene-label--small record-line" x="20" :y="record.y">
        <tspan class="scene-detail">{{ record.text }}</tspan>
        <tspan class="scene-compact">{{ record.text.replace('"kind":', "") }}</tspan>
      </text>
    </g>
    <line class="scene-rule" x1="18" y1="152" x2="206" y2="152" />
    <text class="scene-label scene-label--small" x="18" y="168">01a1….jsonl</text>
    <rect
      v-for="(item, index) in sessionItems"
      :key="`item-${index}`"
      v-track="getRiseStops(item.at, reset, 3)"
      class="item-bar"
      x="20"
      :y="180 + index * 10"
      :width="item.width"
      height="4"
      rx="2"
    />

    <!-- 索引 -->
    <FlowEdge :points="indexEdge" tone="lo" />
    <template v-for="record in records" :key="`packet-${record.y}`">
      <FlowPacket :points="indexEdge" :at="record.append + 0.005" :span="0.04" />
      <FlowPacket :points="indexEdge" :at="record.rebuild + 0.002" :span="0.03" />
    </template>
    <rect class="panel" x="260" y="30" width="172" height="104" rx="10" />
    <path class="db-glyph" d="M270 41.5c0-1.4 3.6-2.5 8-2.5s8 1.1 8 2.5v7c0 1.4-3.6 2.5-8 2.5s-8-1.1-8-2.5z M270 41.5c0 1.4 3.6 2.5 8 2.5s8-1.1 8-2.5" />
    <text class="scene-label scene-label--ink" x="294" y="48.5">metadata.db</text>
    <line class="scene-rule" x1="260" y1="58" x2="432" y2="58" />
    <g v-for="(record, index) in records" :key="`row-${record.y}`" v-track="getIndexRowStops(record)">
      <rect class="index-cell" x="272" :y="70 + index * 15" width="30" height="5" rx="2" />
      <rect class="index-cell index-cell--wide" x="310" :y="70 + index * 15" :width="[96, 110, 70, 84][index]" height="5" rx="2" />
    </g>

    <!-- 另一台机器 -->
    <rect class="machine" x="260" y="148" width="172" height="80" rx="10" />
    <text class="scene-label scene-label--small" x="272" y="164">{{ isZh ? "另一台机器" : "another machine" }}</text>
    <g v-track="copyStops" class="copy-card">
      <rect class="panel" x="272" y="174" width="148" height="34" rx="7" />
      <path class="folder-glyph" d="M282 184h8l2 2.5h9v8h-19z" />
      <text class="scene-label scene-label--small scene-label--ink" x="306" y="194">01a1…/</text>
    </g>

    <!-- 尚未支持的存储后端 -->
    <g v-for="(backend, index) in futureBackends" :key="backend.label" :style="{ opacity: 0.75 - index * 0.1 }">
      <rect class="future-pill" :x="getBackendX(index)" y="244" :width="backend.width" height="18" rx="9" />
      <text class="scene-label scene-label--small" :x="getBackendX(index) + backend.width / 2" y="256" text-anchor="middle">
        {{ backend.label }}
      </text>
    </g>
    <text class="scene-label scene-label--small" x="8" y="280">
      {{ isZh ? "更多存储后端陆续支持" : "more storage backends coming" }}
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
.panel {
  fill: var(--scene-plate);
  stroke: var(--scene-edge);
  stroke-width: 1;
}

.machine {
  fill: none;
  stroke: var(--scene-mid);
  stroke-width: 1;
  stroke-dasharray: 3 3;
}

.folder-glyph,
.db-glyph {
  fill: none;
  stroke: var(--scene-edge);
  stroke-width: 1;
  stroke-linejoin: round;
}

.record-line {
  fill: var(--ink-soft);
}

.scan-mark {
  fill: var(--scene-hi);
  opacity: 0;
}

.item-bar {
  fill: var(--scene-lo);
}

.index-cell {
  fill: var(--scene-mid);
}

.index-cell--wide {
  fill: var(--scene-lo);
}

.future-pill {
  fill: none;
  stroke: var(--scene-mid);
  stroke-width: 1;
  stroke-dasharray: 2 3;
}

.copy-card {
  transform-box: view-box;
}
</style>
