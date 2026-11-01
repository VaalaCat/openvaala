<script setup lang="ts">
// IsoCable：沿世界坐标折线走的线缆或连线，拐角自动倒圆。
// points 依次经过的世界坐标；tone：lo 淡线、mid 普通线、hi 信号蓝、ghost 淡虚线；radius 拐角半径。
// 数据包用同样的 points 交给 IsoPacket，轨迹完全重合。
import { computed } from "vue"
import { getRoundedPolylinePath, type Vec3 } from "./iso"

const props = withDefaults(defineProps<{ points: Vec3[]; tone?: "lo" | "mid" | "hi" | "ghost"; radius?: number }>(), {
  tone: "mid",
  radius: 8,
})

const path = computed(() => getRoundedPolylinePath(props.points, props.radius))
</script>

<template>
  <path class="scene-wire" :class="`scene-wire--${tone}`" :d="path" />
</template>
