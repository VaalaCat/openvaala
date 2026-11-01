<script setup lang="ts">
// IsoLabel：贴在世界坐标 at 旁边的小号等宽标签，文字保持屏幕方向。
// anchor 对齐方式；dy 相对投影点的竖向偏移；tone：muted 弱化、ink 正常、hi 信号蓝。
import { computed } from "vue"
import { project, type Vec3 } from "./iso"

const props = withDefaults(
  defineProps<{ at: Vec3; text: string; anchor?: "start" | "middle" | "end"; dy?: number; tone?: "muted" | "ink" | "hi" }>(),
  { anchor: "middle", dy: 0, tone: "muted" },
)

const position = computed(() => project(props.at))
</script>

<template>
  <text
    class="scene-label"
    :class="`scene-label--${tone}`"
    :x="position[0]"
    :y="position[1] + dy"
    :text-anchor="anchor"
  >
    {{ text }}
  </text>
</template>
