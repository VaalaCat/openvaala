<script setup lang="ts">
// iso-architecture：运行时、对话、Session、Turn、Agent 五层由大到小叠在一起，
// 循环开始时紧贴成一摞，随后逐层向上分开成爆炸图并标出层名，最上面信号蓝的 Agent 是正在运行的循环。
import { computed } from "vue"
import { useData } from "vitepress"
import IsoSlab from "../primitives/IsoSlab.vue"
import SceneCanvas from "../primitives/SceneCanvas.vue"
import { project, type Vec3 } from "../primitives/iso"
import { vTrack, type SceneStop } from "../primitives/motion"

const { localeIndex } = useData()
const isZh = computed(() => localeIndex.value === "zh")

// 每层的边长、厚度、分开后的高度和收拢时的高度；收拢时各层直接叠放。
const layerSpecs = [
  { key: "runtime", size: 156, thickness: 8, z: 0, stackedZ: 0 },
  { key: "conversation", size: 124, thickness: 7, z: 26, stackedZ: 8 },
  { key: "session", size: 94, thickness: 6, z: 50, stackedZ: 15 },
  { key: "turn", size: 66, thickness: 6, z: 72, stackedZ: 21 },
  { key: "agent", size: 40, thickness: 6, z: 92, stackedZ: 27 },
]

const layerNames = computed<Record<string, string>>(() =>
  isZh.value
    ? { runtime: "运行时", conversation: "对话", session: "Session", turn: "Turn", agent: "Agent" }
    : { runtime: "Runtime", conversation: "Conversation", session: "Session", turn: "Turn", agent: "Agent" },
)

const layers = computed(() =>
  layerSpecs.map((spec, index) => {
    const half = spec.size / 2
    // 标签贴在每层右角外侧，随层一起移动。
    const [labelX, labelY] = project([half, -half, spec.z + spec.thickness])
    return {
      ...spec,
      at: [-half, -half, spec.z] as Vec3,
      label: layerNames.value[spec.key],
      labelX: labelX + 8,
      labelY: labelY + 2,
      separateAt: 0.1 + index * 0.05,
    }
  }),
)

// getSeparateStops 让一层从收拢的位置升到分开的位置，循环末尾再落回去。
function getSeparateStops(z: number, stackedZ: number, separateAt: number): SceneStop[] {
  const stacked = `translateY(${z - stackedZ}px)`
  return [
    [separateAt, { transform: stacked }],
    [separateAt + 0.14, { transform: "translateY(0px)" }],
    [0.82, { transform: "translateY(0px)", easing: "cubic-bezier(0.4, 0, 0.2, 1)" }],
    [0.94, { transform: stacked }],
  ]
}

function getLabelStops(separateAt: number): SceneStop[] {
  return [
    [separateAt + 0.08, { opacity: 0 }],
    [separateAt + 0.16, { opacity: 1 }],
    [0.8, { opacity: 1 }],
    [0.84, { opacity: 0 }],
  ]
}
</script>

<template>
  <SceneCanvas view-box="-190 -126 380 214" :period="11000">
    <g v-for="layer in layers" :key="layer.key" v-track="getSeparateStops(layer.z, layer.stackedZ, layer.separateAt)">
      <IsoSlab
        :at="layer.at"
        :size="[layer.size, layer.size, layer.thickness]"
        :radius="Math.min(14, layer.size / 5)"
        :tone="layer.key === 'agent' ? 'hi' : 'edge'"
      />
      <text
        v-track="getLabelStops(layer.separateAt)"
        class="scene-label"
        :class="layer.key === 'agent' ? 'scene-label--hi' : 'scene-label--ink'"
        :x="layer.labelX"
        :y="layer.labelY"
      >
        {{ layer.label }}
      </text>
    </g>
  </SceneCanvas>
</template>
