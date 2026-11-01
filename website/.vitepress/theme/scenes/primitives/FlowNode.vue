<script setup lang="ts">
// FlowNode：平面流程图里的一个节点，圆角矩形加等宽标签。
// x、y 左上角；width、height 尺寸；label 主标签；sublabel 第二行弱化标签；
// tone：edge 普通，hi 信号蓝描边（当前活动节点），ghost 淡虚线（尚未支持）；radius 圆角。
// 默认插槽画在节点框之后，用于节点内部的小图形。
withDefaults(
  defineProps<{
    x: number
    y: number
    width: number
    height: number
    label?: string
    sublabel?: string
    tone?: "edge" | "hi" | "ghost"
    radius?: number
  }>(),
  { tone: "edge", radius: 8 },
)
</script>

<template>
  <g class="flow-node" :class="`flow-node--${tone}`">
    <rect class="flow-node-box" :x="x" :y="y" :width="width" :height="height" :rx="radius" />
    <text
      v-if="label"
      class="scene-label scene-label--ink"
      :x="x + width / 2"
      :y="y + height / 2 + (sublabel ? -1 : 3.5)"
      text-anchor="middle"
    >
      {{ label }}
    </text>
    <text
      v-if="sublabel"
      class="scene-label scene-label--muted scene-label--small"
      :x="x + width / 2"
      :y="y + height / 2 + 11"
      text-anchor="middle"
    >
      {{ sublabel }}
    </text>
    <slot />
  </g>
</template>
