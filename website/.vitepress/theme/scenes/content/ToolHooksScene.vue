<script setup lang="ts">
// tool-hooks：Agent 依次发出三个 bash 调用，在 Executor 上先经过 PreToolUse hook。
// make test：退出码 0 放行，在 Executor 上运行，经过 PostToolUse 时补充一段上下文，结果送回 Agent；
// rm -rf /：退出码 2 拦下，调用不运行，原因送回 Agent；
// grep -r TODO：hook 用 updatedInput 改写成 rg TODO，按新参数运行，结果送回 Agent。
// 宽版三个调用各占一行、从左往右走；窄版各占一列、从上往下走。
import { computed } from "vue"
import { useData } from "vitepress"
import FlowPacket from "../primitives/FlowPacket.vue"
import SceneCanvas from "../primitives/SceneCanvas.vue"
import { getCaptionStops, getShowStops, vTrack, type SceneStop } from "../primitives/motion"
import type { Vec2 } from "../primitives/iso"

const props = withDefaults(defineProps<{ layout?: "wide" | "narrow" }>(), { layout: "wide" })

const { localeIndex } = useData()
const isZh = computed(() => localeIndex.value === "zh")

const reset = 0.95

interface PlacedLabel {
  x: number
  y: number
  anchor: "start" | "middle"
}

// 每个调用在画面里的位置：lane 是调用走的轨道，从 Agent 接到 PostToolUse（被拦下的那条接到 PreToolUse）；
// emerge 从 Agent 冒出，wait 停在 PreToolUse 前，run 运行，post 经过 PostToolUse 之后。
interface CallPlacement {
  lane: Vec2[]
  returnRoute: Vec2[]
  emerge: Vec2
  wait: Vec2
  run: Vec2
  post: Vec2
  verdict: PlacedLabel
  reason: PlacedLabel
  note: PlacedLabel
  // received 是 Agent 收到的内容（结果、原因）在 Agent 框里的位置。
  received: PlacedLabel
}

const geometry = computed(() => {
  if (props.layout === "narrow") {
    const columnCenters = [62, 160, 258]
    return {
      width: 320,
      height: 270,
      agent: { x: 8, y: 12, width: 304, height: 42 },
      agentLabel: { x: 160, y: 29, anchor: "middle" },
      region: { x: 8, y: 64, width: 304, height: 182 },
      regionLabel: { x: 160, y: 263, anchor: "middle" },
      isReceivedSplit: false,
      cardWidth: 88,
      cardHeight: 22,
      gates: [
        { key: "pre", label: "PreToolUse", rect: { x: 16, y: 106, width: 288, height: 14 }, labelAt: { x: 160, y: 116.5 } },
        { key: "post", label: "PostToolUse", rect: { x: 16, y: 172, width: 288, height: 14 }, labelAt: { x: 160, y: 182.5 } },
      ],
      placements: columnCenters.map((center, index): CallPlacement => ({
        lane: index === 1 ? [[center, 54], [center, 106]] : [[center, 54], [center, 172]],
        returnRoute: index === 1 ? [[center, 54], [center, 74]] : [[center, 54], [center, 172]],
        emerge: [center - 44, 54],
        wait: [center - 44, 74],
        run: [center - 44, 140],
        post: [center - 44, 196],
        verdict: { x: center, y: 132, anchor: "middle" },
        reason: { x: center, y: 145, anchor: "middle" },
        note: { x: center, y: 232, anchor: "middle" },
        received: { x: center, y: 46, anchor: "middle" },
      })),
    }
  }
  const rowCenters = [76, 118, 160]
  return {
    width: 440,
    height: 188,
    agent: { x: 18, y: 30, width: 64, height: 150 },
    agentLabel: { x: 50, y: 48, anchor: "middle" },
    region: { x: 96, y: 30, width: 336, height: 150 },
    regionLabel: { x: 96, y: 22, anchor: "start" },
    isReceivedSplit: true,
    cardWidth: 80,
    cardHeight: 20,
    gates: [
      { key: "pre", label: "PreToolUse", rect: { x: 194, y: 56, width: 6, height: 118 }, labelAt: { x: 197, y: 46 } },
      { key: "post", label: "PostToolUse", rect: { x: 300, y: 56, width: 6, height: 118 }, labelAt: { x: 303, y: 46 } },
    ],
    placements: rowCenters.map((center, index): CallPlacement => ({
      lane: index === 1 ? [[82, center], [194, center]] : [[82, center], [300, center]],
      returnRoute: index === 1 ? [[82, center], [106, center]] : [[82, center], [300, center]],
      emerge: [86, center - 10],
      wait: [106, center - 10],
      run: [210, center - 10],
      post: [316, center - 10],
      verdict: { x: 210, y: index === 1 ? center - 6 : center - 14, anchor: "start" },
      reason: { x: 210, y: center + 5, anchor: "start" },
      note: { x: 316, y: center - 14, anchor: "start" },
      received: { x: 50, y: index === 0 ? center - 3 : center + 3, anchor: "middle" },
    })),
  }
})

// 三个调用的时间表：emergeAt 冒出，checkedAt 得到 hook 结论，之后按结论运行或被拦下，returnAt 结果或原因送回 Agent。
const callPlans = [
  { key: "allow", command: "make test", emergeAt: 0.03, checkedAt: 0.09, runAt: 0.13, postAt: 0.2, returnAt: 0.26, returnSpan: 0.05 },
  { key: "block", command: "rm -rf /", emergeAt: 0.34, checkedAt: 0.4, runAt: 0, postAt: 0, returnAt: 0.44, returnSpan: 0.03 },
  { key: "rewrite", command: "grep -r TODO", rewrittenCommand: "rg TODO", emergeAt: 0.53, checkedAt: 0.59, runAt: 0.64, postAt: 0.72, returnAt: 0.77, returnSpan: 0.05 },
]

function getTranslate(point: Vec2): string {
  return `translate(${point[0]}px, ${point[1]}px)`
}

// getCardStops：调用卡片从 Agent 冒出停在 PreToolUse 前；放行时穿过 hook 去运行，再经过 PostToolUse；拦下时留在原地。
function getCardStops(plan: (typeof callPlans)[number], placement: CallPlacement): SceneStop[] {
  const moveEasing = "cubic-bezier(0.5, 0, 0.2, 1)"
  const stops: SceneStop[] = [
    [0, { opacity: 0, transform: getTranslate(placement.emerge) }],
    [plan.emergeAt - 0.001, { opacity: 0, transform: getTranslate(placement.emerge), easing: moveEasing }],
    [plan.emergeAt + 0.03, { opacity: 1, transform: getTranslate(placement.wait) }],
  ]
  if (plan.key === "block") {
    stops.push(
      [reset, { opacity: 1, transform: getTranslate(placement.wait) }],
      [reset + 0.03, { opacity: 0, transform: getTranslate(placement.wait) }],
    )
    return stops
  }
  stops.push(
    [plan.checkedAt + 0.005, { opacity: 1, transform: getTranslate(placement.wait), easing: moveEasing }],
    [plan.runAt, { opacity: 1, transform: getTranslate(placement.run) }],
    [plan.postAt, { opacity: 1, transform: getTranslate(placement.run), easing: moveEasing }],
    [plan.postAt + 0.03, { opacity: 1, transform: getTranslate(placement.post) }],
    [reset, { opacity: 1, transform: getTranslate(placement.post) }],
    [reset + 0.03, { opacity: 0, transform: getTranslate(placement.post) }],
  )
  return stops
}

function getFinalCardStyle(plan: (typeof callPlans)[number], placement: CallPlacement) {
  return { transform: getTranslate(plan.key === "block" ? placement.wait : placement.post) }
}

// getCardInkStops：被拦下的调用在得到结论后描边和文字变淡，底色不变，轨道线不会透出来。
function getCardInkStops(plan: (typeof callPlans)[number]): SceneStop[] {
  if (plan.key !== "block") return []
  return [
    [0, { opacity: 1 }],
    [plan.checkedAt, { opacity: 1 }],
    [plan.checkedAt + 0.02, { opacity: 0.45 }],
  ]
}

// 改写：卡片穿过 PreToolUse 的一刻旧参数淡出、新参数淡入。
function getRewriteAt(plan: (typeof callPlans)[number]): number {
  return (plan.checkedAt + plan.runAt) / 2
}

// getCommandStops：原参数一直显示；被改写的调用在穿过 PreToolUse 时淡出原参数。
function getCommandStops(plan: (typeof callPlans)[number]): SceneStop[] {
  if (!plan.rewrittenCommand) return []
  return [
    [0, { opacity: 1 }],
    [getRewriteAt(plan), { opacity: 1 }],
    [getRewriteAt(plan) + 0.012, { opacity: 0 }],
  ]
}

const calls = computed(() =>
  callPlans.map((plan, index) => {
    const placement = geometry.value.placements[index]
    let verdict = isZh.value ? "改写参数" : "updatedInput"
    if (plan.key === "allow") verdict = isZh.value ? "退出码 0 · 放行" : "exit 0 · allow"
    if (plan.key === "block") verdict = isZh.value ? "退出码 2 · 拦下" : "exit 2 · block"
    const result = isZh.value ? "结果" : "result"
    const context = isZh.value ? "+ 上下文" : "+ context"
    let receivedLines = [result]
    if (plan.key === "block") receivedLines = [isZh.value ? "原因" : "reason"]
    if (plan.key === "allow") receivedLines = geometry.value.isReceivedSplit ? [result, context] : [`${result} ${context}`]
    return { ...plan, placement, verdict, receivedLines }
  }),
)

const captions = computed(() => [
  {
    text: isZh.value ? "退出码 0：放行，调用照常运行；PostToolUse 可以给模型补充上下文" : "exit 0: the call runs; PostToolUse can add context for the model",
    stops: getCaptionStops(0, 0.33),
  },
  {
    text: isZh.value ? "退出码 2：拦下，调用不运行，stderr 作为原因交给模型" : "exit 2: the call is blocked; stderr is the reason the model sees",
    stops: getCaptionStops(0.33, 0.52),
  },
  {
    text: isZh.value ? "updatedInput：调用按改写后的参数运行" : "updatedInput: the call runs with rewritten arguments",
    stops: getCaptionStops(0.52, 0.985),
  },
])
</script>

<template>
  <SceneCanvas
    class="scene-canvas--flow"
    :view-box="`0 0 ${geometry.width} ${geometry.height}`"
    :period="14000"
    :label="isZh ? 'PreToolUse hook 放行、拦下或改写工具调用' : 'A PreToolUse hook allows, blocks or rewrites tool calls'"
  >
    <!-- Executor 区域：两道 hook 都在调用所在的 Executor 上运行 -->
    <rect class="executor-region" v-bind="geometry.region" rx="10" />
    <text class="scene-group-label" :x="geometry.regionLabel.x" :y="geometry.regionLabel.y" :text-anchor="geometry.regionLabel.anchor">
      {{ isZh ? "在 Executor 上" : "on the executor" }}
    </text>

    <line
      v-for="call in calls"
      :key="`lane-${call.key}`"
      class="lane"
      :x1="call.placement.lane[0][0]"
      :y1="call.placement.lane[0][1]"
      :x2="call.placement.lane[1][0]"
      :y2="call.placement.lane[1][1]"
    />

    <g v-for="gate in geometry.gates" :key="gate.key">
      <rect class="gate" v-bind="gate.rect" rx="3" />
      <text class="scene-label scene-label--small" :x="gate.labelAt.x" :y="gate.labelAt.y" text-anchor="middle">{{ gate.label }}</text>
    </g>

    <rect class="agent-box" v-bind="geometry.agent" rx="8" />
    <text class="scene-label scene-label--ink" :x="geometry.agentLabel.x" :y="geometry.agentLabel.y" :text-anchor="geometry.agentLabel.anchor">{{ isZh ? "Agent" : "agent" }}</text>
    <circle
      v-for="call in calls"
      :key="`agent-light-${call.key}`"
      v-track="getShowStops(call.returnAt + call.returnSpan, call.returnAt + call.returnSpan + 0.03, 0.01)"
      class="scene-light"
      :cx="geometry.agent.x + geometry.agent.width - 8"
      :cy="geometry.agent.y + 8"
      r="2.4"
    />

    <template v-for="call in calls" :key="call.key">
      <!-- Agent 收到的内容：结果（可能带补充的上下文）或拦下的原因 -->
      <text
        v-for="(line, lineIndex) in call.receivedLines"
        :key="line"
        v-track="getShowStops(call.returnAt + call.returnSpan, reset, 0.012)"
        class="scene-label scene-label--small"
        :x="call.placement.received.x"
        :y="call.placement.received.y + lineIndex * 11"
        :text-anchor="call.placement.received.anchor"
      >
        {{ line }}
      </text>
      <!-- hook 的结论；拦下时附上原因交给模型 -->
      <text
        v-track="getShowStops(call.checkedAt, reset, 0.012)"
        class="scene-label scene-label--small scene-label--ink"
        :x="call.placement.verdict.x"
        :y="call.placement.verdict.y"
        :text-anchor="call.placement.verdict.anchor"
      >
        {{ call.verdict }}
      </text>
      <text
        v-if="call.key === 'block'"
        v-track="getShowStops(call.checkedAt + 0.025, reset, 0.012)"
        class="scene-label scene-label--small"
        :x="call.placement.reason.x"
        :y="call.placement.reason.y"
        :text-anchor="call.placement.reason.anchor"
      >
        {{ isZh ? "原因 → 模型" : "reason → model" }}
      </text>
      <text
        v-if="call.key === 'allow'"
        v-track="getShowStops(call.postAt + 0.035, reset, 0.012)"
        class="scene-label scene-label--small"
        :x="call.placement.note.x"
        :y="call.placement.note.y"
        :text-anchor="call.placement.note.anchor"
      >
        {{ isZh ? "+ 补充上下文" : "+ context" }}
      </text>

      <g v-track="getCardStops(call, call.placement)" class="call" :style="getFinalCardStyle(call, call.placement)">
        <rect class="call-plate" x="0" y="0" :width="geometry.cardWidth" :height="geometry.cardHeight" rx="5" />
        <g v-track="getCardInkStops(call)" :style="{ opacity: call.key === 'block' ? 0.45 : 1 }">
          <rect class="call-card" x="0" y="0" :width="geometry.cardWidth" :height="geometry.cardHeight" rx="5" />
          <text
            v-track="getCommandStops(call)"
            class="scene-label scene-label--small scene-label--ink"
            :x="geometry.cardWidth / 2"
            :y="geometry.cardHeight / 2 + 3"
            text-anchor="middle"
            :style="{ opacity: call.rewrittenCommand ? 0 : 1 }"
          >
            {{ call.command }}
          </text>
          <text
            v-if="call.rewrittenCommand"
            v-track="getShowStops(getRewriteAt(call) + 0.012, reset, 0.012)"
            class="scene-label scene-label--small scene-label--ink"
            :x="geometry.cardWidth / 2"
            :y="geometry.cardHeight / 2 + 3"
            text-anchor="middle"
          >
            {{ call.rewrittenCommand }}
          </text>
        </g>
        <circle
          v-if="call.key !== 'block'"
          v-track="getShowStops(call.runAt + 0.005, call.postAt, 0.01)"
          class="scene-light"
          :cx="geometry.cardWidth - 7"
          :cy="geometry.cardHeight / 2"
          r="2.2"
        />
      </g>

      <FlowPacket :points="call.placement.returnRoute" :at="call.returnAt" :span="call.returnSpan" is-reverse />
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
.executor-region {
  fill: none;
  stroke: var(--scene-lo);
  stroke-width: 1;
}

.lane {
  stroke: var(--scene-lo);
  stroke-width: 1;
}

.gate {
  fill: var(--scene-plate);
  stroke: var(--scene-edge);
  stroke-width: 1;
}

.agent-box {
  fill: var(--scene-plate);
  stroke: var(--scene-edge);
  stroke-width: 1;
}

.call-plate {
  fill: var(--scene-plate);
}

.call-card {
  fill: none;
  stroke: var(--scene-edge);
  stroke-width: 1;
}

.call {
  transform-box: view-box;
  transform-origin: 0 0;
}
</style>
