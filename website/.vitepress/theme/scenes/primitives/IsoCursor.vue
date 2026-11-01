<script setup lang="ts">
// IsoCursor：信号蓝鼠标指针，尖端落在世界坐标 at 上（平面场景改传画布坐标 point）；指针本身保持屏幕方向，便于辨认。
// label 指针旁的小标签（如 "you"）；移动和点击由外层 <g v-track> 负责。
import { computed } from "vue"
import { project, type Vec2, type Vec3 } from "./iso"

const props = defineProps<{ at?: Vec3; point?: Vec2; label?: string }>()

const position = computed(() => props.point ?? project(props.at ?? [0, 0, 0]))
</script>

<template>
  <g class="iso-cursor" :transform="`translate(${position[0]} ${position[1]})`">
    <path class="iso-cursor-arrow" d="M0 0V13.5L3.4 10.4L5.9 15.8L8.1 14.8L5.6 9.5H10.2Z" />
    <g v-if="label" transform="translate(11 16)">
      <rect class="iso-cursor-tag" :width="label.length * 5.4 + 9" height="11" rx="5.5" />
      <text class="iso-cursor-text" x="4.5" y="7.9">{{ label }}</text>
    </g>
  </g>
</template>
