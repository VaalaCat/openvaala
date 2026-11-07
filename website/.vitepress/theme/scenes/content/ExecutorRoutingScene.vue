<script setup lang="ts">
// executor-routing：local（本机，默认）、buildbox、staging（ssh）三个 Executor 各自带一套 read_file 和 bash。
// 六个工具复制件飞进上方「模型看到的工具」，合并成一个 read_file 和一个 bash，并出现列出三个 Executor 的 executor 参数；
// 随后同一轮里的三个调用依次发出：不填 executor 的 read_file 落到默认的 local，
// executor 为 buildbox 的 make test 落到 buildbox，executor 为 staging 的 tail app.log 落到 staging，落点的 Executor 亮灯。
import { computed } from "vue"
import { useData } from "vitepress"
import FlowEdge from "../primitives/FlowEdge.vue"
import FlowPacket from "../primitives/FlowPacket.vue"
import SceneCanvas from "../primitives/SceneCanvas.vue"
import { getCaptionStops, getPulseStops, getShowStops, vTrack, type SceneStop } from "../primitives/motion"
import type { Vec2 } from "../primitives/iso"

const props = withDefaults(defineProps<{ layout?: "wide" | "narrow" }>(), { layout: "wide" })

const { localeIndex } = useData()
const isZh = computed(() => localeIndex.value === "zh")

const reset = 0.95
const mergeAt = 0.16
const mergeSpan = 0.08
const mergedAt = mergeAt + mergeSpan + 0.03
const enumAt = mergedAt + 0.04
const callSpan = 0.06
const toolNames = ["read_file", "bash"] as const

// 宽窄两版的尺寸：panel 是模型看到的工具，calls 和 executors 两列按行对齐。
const geometry = computed(() =>
  props.layout === "narrow"
    ? {
        width: 320,
        height: 284,
        panel: { x: 12, y: 24, width: 296, height: 78 },
        panelGroupY: 16,
        toolRows: [
          { nameY: 41, enumY: 55, enumX: 22 },
          { nameY: 80, enumY: 94, enumX: 22 },
        ],
        panelDividerY: 63,
        toolNameX: 22,
        columnGroupY: 124,
        callX: 12,
        edgeStartX: 142,
        executorX: 166,
        executorWidth: 142,
        executorHeight: 44,
        executorYs: [132, 182, 232],
        nameDy: 17,
        chipDy: 24,
        chipHeight: 14,
        chipWidths: [64, 34],
        callLineGap: 13,
        isSublabelInline: false,
        nameCharWidth: 6.9,
        pillWidths: { en: 52, zh: 34 },
      }
    : {
        width: 440,
        height: 246,
        panel: { x: 18, y: 26, width: 414, height: 56 },
        panelGroupY: 18,
        toolRows: [
          { nameY: 45, enumY: 45, enumX: 112 },
          { nameY: 72, enumY: 72, enumX: 112 },
        ],
        panelDividerY: 54,
        toolNameX: 30,
        columnGroupY: 104,
        callX: 18,
        edgeStartX: 132,
        executorX: 262,
        executorWidth: 170,
        executorHeight: 38,
        executorYs: [112, 156, 200],
        nameDy: 16,
        chipDy: 21,
        chipHeight: 12,
        chipWidths: [56, 30],
        callLineGap: 12,
        isSublabelInline: true,
        nameCharWidth: 6.3,
        pillWidths: { en: 46, zh: 30 },
      },
)

// 三个 Executor 和落到它们上面的调用：callAt 是调用出现的时刻，包随后出发。
const executors = computed(() => {
  const size = geometry.value
  const plans = [
    { name: "local", sublabel: isZh.value ? "本机" : "this host", isDefault: true, callAt: 0.45, callLines: ["read_file src/app.go", isZh.value ? "不填 executor → 默认" : "no executor → default"] },
    { name: "buildbox", sublabel: "ssh", isDefault: false, callAt: 0.63, callLines: ["bash make test", "executor: buildbox"] },
    { name: "staging", sublabel: "ssh", isDefault: false, callAt: 0.71, callLines: ["bash tail app.log", "executor: staging"] },
  ]
  return plans.map((plan, index) => {
    const y = size.executorYs[index]
    const centerY = y + size.executorHeight / 2
    let chipX = size.executorX + 8
    const chips = toolNames.map((tool, toolIndex) => {
      const chip = { tool, x: chipX, y: y + size.chipDy, width: size.chipWidths[toolIndex] }
      chipX += chip.width + 4
      return chip
    })
    return {
      ...plan,
      y,
      centerY,
      chips,
      edge: [[size.edgeStartX, centerY], [size.executorX, centerY]] as Vec2[],
      arriveAt: plan.callAt + 0.03 + callSpan,
    }
  })
})

// 「默认」标记的宽度随语言变化。
const pillWidth = computed(() => (isZh.value ? geometry.value.pillWidths.zh : geometry.value.pillWidths.en))

// 宽版名字后面跟着说明。
function getSublabelX(name: string): number {
  return geometry.value.executorX + 10 + name.length * geometry.value.nameCharWidth + 6
}

// 工具复制件从 Executor 的工具块飞到上方合并后的工具名位置，落下即淡出。
function getCopyStops(chip: { tool: string; x: number; y: number }, executorIndex: number): SceneStop[] {
  const size = geometry.value
  const toolIndex = toolNames.indexOf(chip.tool as (typeof toolNames)[number])
  const target: Vec2 = [size.toolNameX - (chip.x + 5), size.toolRows[toolIndex].nameY - (chip.y + size.chipHeight - 3.5)]
  const start = mergeAt + executorIndex * 0.015
  return [
    [0, { opacity: 0, transform: "translate(0px, 0px)" }],
    [start - 0.001, { opacity: 0, transform: "translate(0px, 0px)" }],
    [start, { opacity: 1, transform: "translate(0px, 0px)", easing: "cubic-bezier(0.5, 0, 0.2, 1)" }],
    [start + mergeSpan, { opacity: 1, transform: `translate(${target[0]}px, ${target[1]}px)` }],
    [start + mergeSpan + 0.02, { opacity: 0, transform: `translate(${target[0]}px, ${target[1]}px)` }],
  ]
}

const captions = computed(() => [
  { text: isZh.value ? "每个 Executor 各有一套 read_file 和 bash" : "every executor offers its own read_file and bash", stops: getCaptionStops(0, mergeAt) },
  {
    text: isZh.value ? "模型只看到一个 read_file、一个 bash，多了 executor 参数" : "the model sees one of each, with an executor argument",
    stops: getCaptionStops(mergeAt, 0.43),
  },
  { text: isZh.value ? "不填 executor：在默认 Executor 上运行" : "no executor given: the call runs on the default", stops: getCaptionStops(0.43, 0.61) },
  {
    text: isZh.value ? "填了 executor 就在那台机器上运行：一轮用到三台机器" : "with executor set, it runs there: one turn, three machines",
    stops: getCaptionStops(0.61, 0.985),
  },
])
</script>

<template>
  <SceneCanvas
    class="scene-canvas--flow"
    :view-box="`0 0 ${geometry.width} ${geometry.height}`"
    :period="13000"
    :label="isZh ? '同名工具合并成一个，executor 参数决定在哪台机器执行' : 'Same-name tools merge into one; the executor argument picks the machine'"
  >
    <!-- 模型看到的工具：合并后每个工具一行，后面列出 executor 参数的取值 -->
    <text class="scene-group-label" :x="geometry.panel.x" :y="geometry.panelGroupY">{{ isZh ? "模型看到的工具" : "tools the model sees" }}</text>
    <rect class="panel" v-bind="geometry.panel" rx="8" />
    <line class="scene-rule" :x1="geometry.panel.x" :y1="geometry.panelDividerY" :x2="geometry.panel.x + geometry.panel.width" :y2="geometry.panelDividerY" />
    <g v-for="(tool, index) in toolNames" :key="tool">
      <text v-track="getShowStops(mergedAt, reset, 0.015)" class="scene-label scene-label--ink" :x="geometry.toolNameX" :y="geometry.toolRows[index].nameY">{{ tool }}</text>
      <text v-track="getShowStops(enumAt, reset, 0.015)" class="scene-label scene-label--small" :x="geometry.toolRows[index].enumX" :y="geometry.toolRows[index].enumY">
        executor: local | buildbox | staging
      </text>
    </g>

    <text class="scene-group-label" :x="geometry.callX" :y="geometry.columnGroupY">{{ isZh ? "同一轮里的调用" : "calls in one turn" }}</text>
    <text class="scene-group-label" :x="geometry.executorX" :y="geometry.columnGroupY">{{ isZh ? "Executor" : "executors" }}</text>

    <template v-for="executor in executors" :key="`call-${executor.name}`">
      <text
        v-for="(line, lineIndex) in executor.callLines"
        :key="line"
        v-track="getShowStops(executor.callAt + lineIndex * 0.01, reset, 0.015)"
        class="scene-label scene-label--small"
        :class="{ 'scene-label--ink': lineIndex === 0 }"
        :x="geometry.callX"
        :y="executor.centerY + (lineIndex - 0.5) * geometry.callLineGap + 3"
      >
        {{ line }}
      </text>
      <FlowEdge v-track="getShowStops(executor.callAt, reset, 0.015)" :points="executor.edge" tone="lo" />
      <FlowPacket :points="executor.edge" :at="executor.callAt + 0.03" :span="callSpan" />
    </template>

    <!-- 三个 Executor：名称、说明、各自的一套工具；调用落到上面时亮灯 -->
    <g v-for="executor in executors" :key="executor.name">
      <rect class="executor-box" :x="geometry.executorX" :y="executor.y" :width="geometry.executorWidth" :height="geometry.executorHeight" rx="7" />
      <text class="scene-label scene-label--ink" :x="geometry.executorX + 10" :y="executor.y + geometry.nameDy">{{ executor.name }}</text>
      <text
        v-if="geometry.isSublabelInline"
        class="scene-label scene-label--small"
        :x="getSublabelX(executor.name)"
        :y="executor.y + geometry.nameDy"
      >
        {{ executor.sublabel }}
      </text>
      <text
        v-else-if="!executor.isDefault"
        class="scene-label scene-label--small"
        :x="geometry.executorX + geometry.executorWidth - 10"
        :y="executor.y + geometry.nameDy"
        text-anchor="end"
      >
        {{ executor.sublabel }}
      </text>
      <g
        v-if="executor.isDefault"
        v-track="getPulseStops(executor.arriveAt, 0.06, 1.12)"
        class="default-pill"
        :style="{ transformOrigin: `${geometry.executorX + geometry.executorWidth - 10 - pillWidth / 2}px ${executor.y + geometry.nameDy - 3}px` }"
      >
        <rect
          class="pill"
          :x="geometry.executorX + geometry.executorWidth - 10 - pillWidth"
          :y="executor.y + geometry.nameDy - 9.5"
          :width="pillWidth"
          :height="geometry.chipHeight"
          :rx="geometry.chipHeight / 2"
        />
        <text
          class="scene-label scene-label--small"
          :x="geometry.executorX + geometry.executorWidth - 10 - pillWidth / 2"
          :y="executor.y + geometry.nameDy"
          text-anchor="middle"
        >
          {{ isZh ? "默认" : "default" }}
        </text>
      </g>
      <g v-for="chip in executor.chips" :key="chip.tool">
        <rect class="chip" :x="chip.x" :y="chip.y" :width="chip.width" :height="geometry.chipHeight" rx="3" />
        <text class="scene-label scene-label--small" :x="chip.x + chip.width / 2" :y="chip.y + geometry.chipHeight - 3.5" text-anchor="middle">{{ chip.tool }}</text>
      </g>
      <circle
        v-track="getShowStops(executor.arriveAt, executor.arriveAt + 0.07, 0.012)"
        class="scene-light"
        :cx="geometry.executorX + geometry.executorWidth - 10"
        :cy="executor.y + geometry.chipDy + geometry.chipHeight / 2"
        r="2.4"
      />
    </g>

    <!-- 合并：每个 Executor 的两个工具复制一份飞进上方 -->
    <template v-for="(executor, executorIndex) in executors" :key="`copies-${executor.name}`">
      <g v-for="chip in executor.chips" :key="chip.tool" v-track="getCopyStops(chip, executorIndex)" class="tool-copy" style="opacity: 0">
        <rect class="chip chip--copy" :x="chip.x" :y="chip.y" :width="chip.width" :height="geometry.chipHeight" rx="3" />
        <text class="scene-label scene-label--small scene-label--ink" :x="chip.x + chip.width / 2" :y="chip.y + geometry.chipHeight - 3.5" text-anchor="middle">{{ chip.tool }}</text>
      </g>
    </template>

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
.panel,
.executor-box {
  fill: var(--scene-plate);
  stroke: var(--scene-edge);
  stroke-width: 1;
}

.chip {
  fill: none;
  stroke: var(--scene-mid);
  stroke-width: 1;
}

.chip--copy {
  fill: var(--scene-plate);
  stroke: var(--scene-edge);
}

.pill {
  fill: none;
  stroke: var(--scene-mid);
  stroke-width: 1;
}

.default-pill,
.tool-copy {
  transform-box: view-box;
}
</style>
