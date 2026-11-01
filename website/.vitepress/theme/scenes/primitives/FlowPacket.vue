<script setup lang="ts">
// FlowPacket 沿 FlowEdge 的折线移动数据包；at、span 是循环中的出发时刻和行程占比。
import { computed } from "vue"
import type { Vec2 } from "./iso"
import { getTravelStops, vTrack } from "./motion"

const props = defineProps<{ points: Vec2[]; at: number; span: number; isReverse?: boolean }>()
const stops = computed(() => getTravelStops(props.at, props.span, props.points, props.isReverse))
</script>

<template>
  <circle v-track="stops" class="scene-packet" r="2" />
</template>
