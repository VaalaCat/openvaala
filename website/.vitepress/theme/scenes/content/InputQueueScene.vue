<script setup lang="ts">
// input-queue：三行各画一轮正在运行的 Turn（模型调用 → 工具 → …），依次演示运行中发来的消息的三种处理方式。
// 每条消息都在工具运行时到达，先出现在时间线上方。
// 补充：消息等工具结束，落在下一次模型调用之前，属于这一轮；
// 排队：消息停在下一轮的位置，等这一轮的最后一次模型调用结束，作为下一轮开始；
// 插队：消息一到，正在运行的工具被截断并标为已中断，已生成的部分留着，消息作为下一轮马上开始。
import { computed } from "vue"
import { useData } from "vitepress"
import SceneCanvas from "../primitives/SceneCanvas.vue"
import { getCaptionStops, getShowStops, vTrack, type SceneStop } from "../primitives/motion"
import type { Vec2 } from "../primitives/iso"

const props = withDefaults(defineProps<{ layout?: "wide" | "narrow" }>(), { layout: "wide" })

const { localeIndex } = useData()
const isZh = computed(() => localeIndex.value === "zh")

const reset = 0.95
// 三幕的起点：补充、排队、插队。
const steerAt = 0
const queueAt = 0.32
const jumpAt = 0.63
// 插队时工具在长到这个比例时被截断。
const cutFraction = 0.65

// 宽窄两版的尺寸：时间线从 timelineStart 到 timelineEnd，片段和消息等高，boundaryGap 是两轮之间留出的空隙。
const sizes = computed(() =>
  props.layout === "narrow"
    ? { width: 320, height: 238, timelineStart: 12, timelineEnd: 308, firstRowY: 16, rowGap: 80, modelWidth: 56, toolWidth: 56, messageWidth: 24, gap: 4, segmentHeight: 20, boundaryGap: 22 }
    : { width: 440, height: 224, timelineStart: 18, timelineEnd: 432, firstRowY: 18, rowGap: 74, modelWidth: 84, toolWidth: 84, messageWidth: 30, gap: 5, segmentHeight: 18, boundaryGap: 24 },
)

interface TurnSegment {
  key: string
  kind: "model" | "tool"
  x: number
  width: number
  growStart: number
  growEnd: number
}

interface LaneLabel {
  text: string
  x: number
  anchor: "start" | "end"
  at: number
}

interface QueueRow {
  key: string
  title: string
  // labelY 是行标题基线，laneY 是消息到达的上方通道，segmentY 是时间线片段。
  labelY: number
  laneY: number
  segmentY: number
  segments: TurnSegment[]
  boundary?: { x: number; at: number }
  messageX: number
  slotStops: SceneStop[]
  messageStops: SceneStop[]
  laneLabels: LaneLabel[]
}

function getGrowStops(start: number, end: number): SceneStop[] {
  return [
    [0, { opacity: 0, transform: "scaleX(0)" }],
    [start - 0.001, { opacity: 0, transform: "scaleX(0)" }],
    [start, { opacity: 1, transform: "scaleX(0)", easing: "linear" }],
    [end, { opacity: 1, transform: "scaleX(1)" }],
    [reset, { opacity: 1, transform: "scaleX(1)" }],
    [reset + 0.03, { opacity: 0, transform: "scaleX(1)" }],
  ]
}

function getTranslate(point: Vec2): string {
  return `translate(${point[0]}px, ${point[1]}px)`
}

// getMessageStops：消息在 arrive 时从上方落到 start，再按 moves 依次移动，停在最后一个位置直到循环末尾。
function getMessageStops(arrive: number, start: Vec2, moves: { from: number; to: number; point: Vec2 }[]): SceneStop[] {
  const stops: SceneStop[] = [
    [0, { opacity: 0, transform: getTranslate([start[0], start[1] - 6]) }],
    [arrive - 0.015, { opacity: 0, transform: getTranslate([start[0], start[1] - 6]) }],
    [arrive, { opacity: 1, transform: getTranslate(start) }],
  ]
  let point = start
  for (const move of moves) {
    stops.push([move.from, { opacity: 1, transform: getTranslate(point), easing: "cubic-bezier(0.5, 0, 0.2, 1)" }])
    point = move.point
    stops.push([move.to, { opacity: 1, transform: getTranslate(point) }])
  }
  stops.push([reset, { opacity: 1, transform: getTranslate(point) }], [reset + 0.03, { opacity: 0, transform: getTranslate(point) }])
  return stops
}

const rows = computed<QueueRow[]>(() => {
  const size = sizes.value
  const toolX = size.timelineStart + size.modelWidth + size.gap
  const toolEnd = toolX + size.toolWidth
  const getRowY = (index: number) => {
    const labelY = size.firstRowY + index * size.rowGap
    const laneY = labelY + 8
    return { labelY, laneY, segmentY: laneY + size.segmentHeight + 6 }
  }
  // 消息到达时停在工具当前长到的位置上方。
  const getArrivalX = (fraction: number) => toolX + fraction * size.toolWidth - size.messageWidth / 2
  const getFirstSegments = (actAt: number, toolWidth: number, toolGrowEnd: number): TurnSegment[] => [
    { key: "model-1", kind: "model", x: size.timelineStart, width: size.modelWidth, growStart: actAt + 0.015, growEnd: actAt + 0.06 },
    { key: "tool", kind: "tool", x: toolX, width: toolWidth, growStart: actAt + 0.065, growEnd: toolGrowEnd },
  ]

  // 补充：消息停在工具末尾上方，工具结束后落进时间线，下一次模型调用紧接着开始。
  const steer = getRowY(0)
  const steerMessageX = toolEnd + size.gap
  const steerRow: QueueRow = {
    key: "steer",
    title: isZh.value ? "补充" : "steer",
    ...steer,
    segments: [
      ...getFirstSegments(steerAt, size.toolWidth, steerAt + 0.145),
      { key: "model-2", kind: "model", x: steerMessageX + size.messageWidth + size.gap, width: size.modelWidth, growStart: steerAt + 0.17, growEnd: steerAt + 0.225 },
    ],
    messageX: steerMessageX,
    slotStops: getShowStops(steerAt + 0.1, steerAt + 0.15, 0.01),
    messageStops: getMessageStops(steerAt + 0.08, [getArrivalX(0.19), steer.laneY], [
      { from: steerAt + 0.088, to: steerAt + 0.108, point: [steerMessageX, steer.laneY] },
      { from: steerAt + 0.15, to: steerAt + 0.165, point: [steerMessageX, steer.segmentY] },
    ]),
    laneLabels: [{ text: isZh.value ? "加入这一轮" : "joins this turn", x: steerMessageX, anchor: "start", at: steerAt + 0.18 }],
  }

  // 排队：消息停在下一轮的位置上方，这一轮结束后落进去，下一轮开始。
  const queue = getRowY(1)
  const queueModelX = toolEnd + size.gap
  const queueTurnEnd = queueModelX + size.modelWidth
  const queueMessageX = queueTurnEnd + size.boundaryGap
  const queueRow: QueueRow = {
    key: "queue",
    title: isZh.value ? "排队" : "queue",
    ...queue,
    segments: [
      ...getFirstSegments(queueAt, size.toolWidth, queueAt + 0.145),
      { key: "model-2", kind: "model", x: queueModelX, width: size.modelWidth, growStart: queueAt + 0.15, growEnd: queueAt + 0.2 },
      { key: "next-model", kind: "model", x: queueMessageX + size.messageWidth + size.gap, width: size.modelWidth, growStart: queueAt + 0.235, growEnd: queueAt + 0.285 },
    ],
    boundary: { x: queueTurnEnd + size.boundaryGap / 2, at: queueAt + 0.205 },
    messageX: queueMessageX,
    slotStops: getShowStops(queueAt + 0.1, queueAt + 0.21, 0.01),
    messageStops: getMessageStops(queueAt + 0.08, [getArrivalX(0.19), queue.laneY], [
      { from: queueAt + 0.088, to: queueAt + 0.11, point: [queueMessageX, queue.laneY] },
      { from: queueAt + 0.21, to: queueAt + 0.225, point: [queueMessageX, queue.segmentY] },
    ]),
    laneLabels: [{ text: isZh.value ? "下一轮" : "next turn", x: queueMessageX, anchor: "start", at: queueAt + 0.235 }],
  }

  // 插队：消息一到工具就被截断，消息直接落到下一轮的位置。
  const jump = getRowY(2)
  const cutWidth = size.toolWidth * cutFraction
  const cutEnd = toolX + cutWidth
  const jumpMessageX = cutEnd + size.boundaryGap
  const cutAt = jumpAt + 0.065 + 0.08 * cutFraction
  const jumpRow: QueueRow = {
    key: "jump",
    title: isZh.value ? "插队" : "jump the queue",
    ...jump,
    segments: [
      ...getFirstSegments(jumpAt, cutWidth, cutAt),
      { key: "next-model", kind: "model", x: jumpMessageX + size.messageWidth + size.gap, width: size.modelWidth, growStart: jumpAt + 0.16, growEnd: jumpAt + 0.215 },
    ],
    boundary: { x: cutEnd + size.boundaryGap / 2, at: cutAt + 0.006 },
    messageX: jumpMessageX,
    slotStops: getShowStops(cutAt + 0.006, jumpAt + 0.15, 0.01),
    messageStops: getMessageStops(jumpAt + 0.11, [getArrivalX(0.5625), jump.laneY], [
      { from: jumpAt + 0.125, to: jumpAt + 0.15, point: [jumpMessageX, jump.segmentY] },
    ]),
    laneLabels: [
      { text: isZh.value ? "已中断" : "interrupted", x: cutEnd, anchor: "end", at: jumpAt + 0.16 },
      { text: isZh.value ? "下一轮" : "next turn", x: jumpMessageX, anchor: "start", at: jumpAt + 0.165 },
    ],
  }

  return [steerRow, queueRow, jumpRow]
})

function getSegmentLabel(kind: TurnSegment["kind"]): string {
  if (kind === "model") return isZh.value ? "模型" : "model"
  return isZh.value ? "工具" : "tool"
}

function getFinalTransform(row: QueueRow): string {
  return getTranslate([row.messageX, row.segmentY])
}

const captions = computed(() => [
  {
    text: isZh.value ? "补充：不打断正在运行的工具，下一次调用模型前读到" : "steer: the running tool is not interrupted; the next model call reads it",
    stops: getCaptionStops(steerAt, queueAt),
  },
  {
    text: isZh.value ? "排队：等这一轮结束，作为下一轮开始" : "queue: it waits for the turn to end and starts the next one",
    stops: getCaptionStops(queueAt, jumpAt),
  },
  {
    text: isZh.value ? "插队：中断这一轮，已生成的内容保留，接着处理这条消息" : "jump the queue: the turn is interrupted, its output kept, and this message goes next",
    stops: getCaptionStops(jumpAt, 0.985),
  },
])
</script>

<template>
  <SceneCanvas
    class="scene-canvas--flow"
    :view-box="`0 0 ${sizes.width} ${sizes.height}`"
    :period="14000"
    :label="isZh ? '运行中发来的消息：补充、排队、插队' : 'Messages sent during a turn: steer, queue, jump the queue'"
  >
    <g v-for="row in rows" :key="row.key">
      <text class="scene-group-label" :x="sizes.timelineStart" :y="row.labelY">{{ row.title }}</text>
      <line class="timeline" :x1="sizes.timelineStart" :y1="row.segmentY + sizes.segmentHeight / 2" :x2="sizes.timelineEnd" :y2="row.segmentY + sizes.segmentHeight / 2" />

      <template v-for="segment in row.segments" :key="segment.key">
        <rect
          v-track="getGrowStops(segment.growStart, segment.growEnd)"
          class="segment"
          :class="`segment--${segment.kind}`"
          :x="segment.x"
          :y="row.segmentY"
          :width="segment.width"
          :height="sizes.segmentHeight"
          rx="4"
        />
        <text
          v-track="getShowStops(segment.growEnd + 0.01, reset, 0.012)"
          class="scene-label scene-label--small"
          :x="segment.x + segment.width / 2"
          :y="row.segmentY + sizes.segmentHeight / 2 + 3"
          text-anchor="middle"
        >
          {{ getSegmentLabel(segment.kind) }}
        </text>
      </template>

      <!-- 两轮之间的分界线 -->
      <line
        v-if="row.boundary"
        v-track="getShowStops(row.boundary.at, reset, 0.012)"
        class="turn-boundary"
        :x1="row.boundary.x"
        :y1="row.segmentY - 5"
        :x2="row.boundary.x"
        :y2="row.segmentY + sizes.segmentHeight + 5"
      />

      <!-- 消息将要落进的空位，消息落下后被盖住 -->
      <rect
        v-track="row.slotStops"
        class="slot"
        :x="row.messageX"
        :y="row.segmentY"
        :width="sizes.messageWidth"
        :height="sizes.segmentHeight"
        rx="4"
        style="opacity: 0"
      />

      <text
        v-for="laneLabel in row.laneLabels"
        :key="laneLabel.text"
        v-track="getShowStops(laneLabel.at, reset, 0.012)"
        class="scene-label scene-label--small"
        :x="laneLabel.x"
        :y="row.laneY + sizes.segmentHeight / 2 + 3"
        :text-anchor="laneLabel.anchor"
      >
        {{ laneLabel.text }}
      </text>

      <g v-track="row.messageStops" class="message" :style="{ transform: getFinalTransform(row) }">
        <rect class="message-card" x="0" y="0" :width="sizes.messageWidth" :height="sizes.segmentHeight" rx="4" />
        <rect class="message-bar" x="6" :y="sizes.segmentHeight / 2 - 2" :width="sizes.messageWidth - 12" height="4" rx="2" />
      </g>
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

.segment {
  fill: var(--scene-plate);
  stroke: var(--scene-edge);
  stroke-width: 1;
  transform-box: fill-box;
  transform-origin: left center;
}

.segment--tool {
  stroke: var(--scene-mid);
}

.turn-boundary {
  stroke: var(--scene-mid);
  stroke-width: 1;
}

.slot {
  fill: var(--scene-plate);
  stroke: var(--scene-mid);
  stroke-width: 1;
  stroke-dasharray: 2 2;
}

.message {
  transform-box: view-box;
  transform-origin: 0 0;
}

.message-card {
  fill: var(--scene-plate);
  stroke: var(--scene-hi);
  stroke-width: 1;
}

.message-bar {
  fill: var(--scene-mid);
}
</style>
