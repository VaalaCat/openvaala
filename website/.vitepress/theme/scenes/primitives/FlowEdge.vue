<script setup lang="ts">
// FlowEdge：平面流程图里的连线，按 points 折线走并在拐角倒圆。
// tone：lo 淡线、mid 普通线、hi 信号蓝、ghost 淡虚线；radius 拐角半径。数据包用 FlowPacket 走同一组 points。
import { computed } from "vue"
import { getRounded2dPath, type Vec2 } from "./iso"

const props = withDefaults(defineProps<{ points: Vec2[]; tone?: "lo" | "mid" | "hi" | "ghost"; radius?: number }>(), {
  tone: "mid",
  radius: 10,
})

const path = computed(() => getRounded2dPath(props.points, props.radius))
</script>

<template>
  <path class="scene-wire" :class="`scene-wire--${tone}`" :d="path" />
</template>
