<script setup lang="ts">
// IsoPacket 沿 IsoCable 的折线移动数据包；静态画面里隐藏。
import { computed } from "vue"
import { project, type Vec3 } from "./iso"
import { getTravelStops, vTrack } from "./motion"

const props = defineProps<{ points: Vec3[]; at: number; span: number; isReverse?: boolean }>()
const stops = computed(() => getTravelStops(props.at, props.span, props.points.map(project), props.isReverse))
</script>

<template>
  <circle v-track="stops" class="scene-packet" r="2" />
</template>
