<script setup lang="ts">
// portable-conversations：日志存储（每个对话目录里的 JSONL）和记录存储（SQLite 索引 metadata.db）。
// 记录逐行追加并写进索引 → 索引被清空 → openvaala reindex 按 JSONL 逐行重建 → 把对话目录拷到另一台机器。
// 底部是尚未支持的存储后端，只画淡虚线，不接收数据。
// 宽版：对话目录在左，索引和另一台机器在右上、右下。窄版：对话目录在上，索引和另一台机器并排在下。
// 两版共用同一张时间表，只有 geometry 里的坐标不同。
import { computed } from "vue"
import { useData } from "vitepress"
import FlowEdge from "../primitives/FlowEdge.vue"
import FlowPacket from "../primitives/FlowPacket.vue"
import SceneCanvas from "../primitives/SceneCanvas.vue"
import { getCaptionStops, getRiseStops, getShowStops, vTrack, type SceneStop } from "../primitives/motion"
import type { Vec2 } from "../primitives/iso"

const props = withDefaults(defineProps<{ layout?: "wide" | "narrow" }>(), { layout: "wide" })

const { localeIndex } = useData()
const isZh = computed(() => localeIndex.value === "zh")

const reset = 0.95
const wipeAt = 0.32

interface Frame {
  x: number
  y: number
  width: number
  height: number
}

// PortableGeometry 是一版布局的全部坐标；面板内部的行按面板左上角排布，两版相同。
interface PortableGeometry {
  viewBox: string
  logLabel: Vec2
  recordLabel: Vec2
  logPanel: Frame
  // logRows 是对话目录面板里各行的纵坐标：metadata.jsonl 标题、首条记录和行距、分隔线、Session 日志标题、首个 Item。
  logRows: { metadataY: number; recordY: number; recordGap: number; ruleY: number; sessionY: number; itemY: number }
  indexPanel: Frame
  indexEdge: Vec2[]
  machinePanel: Frame
  // copyPath 是拷贝卡片相对另一台机器里原位的两个位移：起点盖在对话目录的标题栏上，经过 copyPath[1] 再回到原位。
  copyPath: [Vec2, Vec2]
  backendsY: number
  comingLabel: Vec2
}

// 宽版：卡片从对话目录标题处出现，先在目录里往下移，再水平滑到另一台机器，不经过右上角的索引框。
const wideGeometry: PortableGeometry = {
  viewBox: "0 0 440 288",
  logLabel: [8, 22],
  recordLabel: [260, 22],
  logPanel: { x: 8, y: 30, width: 208, height: 198 },
  logRows: { metadataY: 42, recordY: 54, recordGap: 15, ruleY: 122, sessionY: 138, itemY: 150 },
  indexPanel: { x: 260, y: 30, width: 172, height: 104 },
  indexEdge: [[216, 100], [260, 100]],
  machinePanel: { x: 260, y: 148, width: 172, height: 80 },
  copyPath: [[-256, -147], [-256, 0]],
  backendsY: 244,
  comingLabel: [8, 280],
}

// 窄版：卡片从对话目录标题处出现，先在标题栏里右移，再竖直落进右下的另一台机器，不经过左下的索引框。
const narrowGeometry: PortableGeometry = {
  viewBox: "0 0 320 416",
  logLabel: [4, 12],
  recordLabel: [4, 244],
  logPanel: { x: 4, y: 20, width: 312, height: 204 },
  logRows: { metadataY: 45, recordY: 60, recordGap: 16, ruleY: 128, sessionY: 145, itemY: 157 },
  indexPanel: { x: 4, y: 252, width: 186, height: 104 },
  indexEdge: [[176, 224], [176, 252]],
  machinePanel: { x: 198, y: 252, width: 118, height: 104 },
  copyPath: [[-198, -261], [0, -261]],
  backendsY: 372,
  comingLabel: [4, 408],
}

const geometry = computed(() => (props.layout === "narrow" ? narrowGeometry : wideGeometry))

// 四行 metadata.jsonl 记录：append 是首次写入时刻，rebuild 是 reindex 读到这一行的时刻。
const records = [
  { text: '{"kind":"session_saved",…}', append: 0.04, rebuild: 0.47 },
  { text: '{"kind":"conversation_saved",…}', append: 0.1, rebuild: 0.5 },
  { text: '{"kind":"turn_saved",…}', append: 0.16, rebuild: 0.53 },
  { text: '{"kind":"turn_saved",…}', append: 0.22, rebuild: 0.56 },
]

// 同一目录里的 Session 日志：Item 随 Turn 记录一起追加。
const sessionItems = [
  { at: 0.17, width: 150 },
  { at: 0.2, width: 118 },
  { at: 0.23, width: 164 },
  { at: 0.26, width: 96 },
]

// 索引里每行右侧单元格的宽度。
const indexCellWidths = [96, 110, 70, 84]

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

// 拷贝卡片画在另一台机器里；动画从对话目录标题处出现，经 copyPath 两段位移落到原位。
const copyStops = computed<SceneStop[]>(() => {
  const [[startX, startY], [viaX, viaY]] = geometry.value.copyPath
  const start = `translate(${startX}px, ${startY}px)`
  const via = `translate(${viaX}px, ${viaY}px)`
  const docked = "translate(0px, 0px)"
  return [
    [0, { opacity: 0, transform: start }],
    [0.66, { opacity: 0, transform: start }],
    [0.68, { opacity: 1, transform: start, easing: "cubic-bezier(0.5, 0, 0.2, 1)" }],
    [0.72, { opacity: 1, transform: via, easing: "cubic-bezier(0.5, 0, 0.2, 1)" }],
    [0.79, { opacity: 1, transform: docked }],
    [reset, { opacity: 1, transform: docked }],
    [reset + 0.03, { opacity: 0, transform: docked }],
  ]
})

const copyCard = computed<Frame>(() => {
  const machine = geometry.value.machinePanel
  return { x: machine.x + 12, y: machine.y + 26, width: machine.width - 24, height: 34 }
})

const futureBackends = computed(() => {
  const backends = [
    { label: isZh.value ? "KV 存储" : "KV store", width: 62 },
    { label: "Postgres", width: 62 },
    { label: isZh.value ? "文档数据库" : "document DB", width: 76 },
    { label: "S3", width: 40 },
    { label: "…", width: 26 },
  ]
  let x = geometry.value.logPanel.x
  return backends.map((backend) => {
    const pill = { ...backend, x }
    x += backend.width + 8
    return pill
  })
})

const captions = computed(() => [
  { text: isZh.value ? "1  记录追加进 JSONL，同时写入 SQLite 索引" : "1  records append to JSONL and land in the SQLite index", stops: getCaptionStops(0, 0.3) },
  { text: isZh.value ? "2  删掉索引，历史还在" : "2  drop the index, the history is still there", stops: getCaptionStops(0.3, 0.46) },
  { text: isZh.value ? "3  openvaala reindex 按 JSONL 重建索引" : "3  openvaala reindex rebuilds it from the JSONL", stops: getCaptionStops(0.46, 0.64) },
  { text: isZh.value ? "4  把对话目录拷到另一台机器，接着用" : "4  copy the folder to another machine and keep going", stops: getCaptionStops(0.64, 0.985) },
])
</script>

<template>
  <SceneCanvas
    class="scene-canvas--flow"
    :view-box="geometry.viewBox"
    :period="15000"
    :label="isZh ? '对话以 JSONL 保存，索引可重建，目录可拷走' : 'Conversations as JSONL, a rebuildable index, folders you can move'"
  >
    <text class="scene-group-label" :x="geometry.logLabel[0]" :y="geometry.logLabel[1]">{{ isZh ? "日志存储 · JSONL" : "log storage · JSONL" }}</text>
    <text class="scene-group-label" :x="geometry.recordLabel[0]" :y="geometry.recordLabel[1]">{{ isZh ? "记录存储 · SQLite" : "record storage · SQLite" }}</text>

    <!-- 对话目录：标题栏、metadata.jsonl 的记录、Session 日志的 Item -->
    <g :transform="`translate(${geometry.logPanel.x} ${geometry.logPanel.y})`">
      <rect class="panel" x="0" y="0" :width="geometry.logPanel.width" :height="geometry.logPanel.height" rx="10" />
      <path class="folder-glyph" d="M10 10h8l2 2.5h9v8H10z" />
      <text class="scene-label scene-label--ink" x="36" y="18.5">conversations/01a1…/</text>
      <line class="scene-rule" x1="0" y1="28" :x2="geometry.logPanel.width" y2="28" />
      <text class="scene-label scene-label--small" x="10" :y="geometry.logRows.metadataY">metadata.jsonl</text>
      <g v-for="(record, index) in records" :key="index">
        <rect
          v-track="getShowStops(record.rebuild, record.rebuild + 0.035, 0.008)"
          class="scan-mark"
          x="6"
          :y="geometry.logRows.recordY - 6 + index * geometry.logRows.recordGap"
          width="2"
          height="8"
        />
        <text
          v-track="getRiseStops(record.append, reset, 4)"
          class="scene-label scene-label--small record-line"
          x="12"
          :y="geometry.logRows.recordY + index * geometry.logRows.recordGap"
        >
          {{ record.text }}
        </text>
      </g>
      <line class="scene-rule" x1="10" :y1="geometry.logRows.ruleY" :x2="geometry.logPanel.width - 10" :y2="geometry.logRows.ruleY" />
      <text class="scene-label scene-label--small" x="10" :y="geometry.logRows.sessionY">01a1….jsonl</text>
      <rect
        v-for="(item, index) in sessionItems"
        :key="`item-${index}`"
        v-track="getRiseStops(item.at, reset, 3)"
        class="item-bar"
        x="12"
        :y="geometry.logRows.itemY + index * 10"
        :width="item.width"
        height="4"
        rx="2"
      />
    </g>

    <!-- 索引 -->
    <FlowEdge :points="geometry.indexEdge" tone="lo" />
    <template v-for="(record, index) in records" :key="`packet-${index}`">
      <FlowPacket :points="geometry.indexEdge" :at="record.append + 0.005" :span="0.04" />
      <FlowPacket :points="geometry.indexEdge" :at="record.rebuild + 0.002" :span="0.03" />
    </template>
    <g :transform="`translate(${geometry.indexPanel.x} ${geometry.indexPanel.y})`">
      <rect class="panel" x="0" y="0" :width="geometry.indexPanel.width" :height="geometry.indexPanel.height" rx="10" />
      <path class="db-glyph" d="M10 11.5c0-1.4 3.6-2.5 8-2.5s8 1.1 8 2.5v7c0 1.4-3.6 2.5-8 2.5s-8-1.1-8-2.5z M10 11.5c0 1.4 3.6 2.5 8 2.5s8-1.1 8-2.5" />
      <text class="scene-label scene-label--ink" x="34" y="18.5">metadata.db</text>
      <line class="scene-rule" x1="0" y1="28" :x2="geometry.indexPanel.width" y2="28" />
      <g v-for="(record, index) in records" :key="`row-${index}`" v-track="getIndexRowStops(record)">
        <rect class="index-cell" x="12" :y="40 + index * 15" width="30" height="5" rx="2" />
        <rect class="index-cell index-cell--wide" x="50" :y="40 + index * 15" :width="indexCellWidths[index]" height="5" rx="2" />
      </g>
    </g>

    <!-- 另一台机器 -->
    <rect class="machine" v-bind="geometry.machinePanel" rx="10" />
    <text class="scene-label scene-label--small" :x="geometry.machinePanel.x + 12" :y="geometry.machinePanel.y + 16">{{ isZh ? "另一台机器" : "another machine" }}</text>
    <g v-track="copyStops" class="copy-card">
      <rect class="panel" v-bind="copyCard" rx="7" />
      <path class="folder-glyph" :d="`M${copyCard.x + 10} ${copyCard.y + 10}h8l2 2.5h9v8h-19z`" />
      <text class="scene-label scene-label--small scene-label--ink" :x="copyCard.x + 34" :y="copyCard.y + 20">01a1…/</text>
    </g>

    <!-- 尚未支持的存储后端 -->
    <g v-for="(backend, index) in futureBackends" :key="backend.label" :style="{ opacity: 0.75 - index * 0.1 }">
      <rect class="future-pill" :x="backend.x" :y="geometry.backendsY" :width="backend.width" height="18" rx="9" />
      <text class="scene-label scene-label--small" :x="backend.x + backend.width / 2" :y="geometry.backendsY + 12" text-anchor="middle">
        {{ backend.label }}
      </text>
    </g>
    <text class="scene-label scene-label--small" :x="geometry.comingLabel[0]" :y="geometry.comingLabel[1]">
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
