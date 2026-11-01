<script setup lang="ts">
// IsoSlab：等轴测圆角棱柱，场景里的板、盒子、卡片、按键和竖立屏幕都用它。
// at 最小角的世界坐标；size [宽, 深, 厚]，按 plane 解释：
//   floor 平放（宽沿 x、深沿 y、厚沿 z）——板、托盘、卡片（厚 2–4）、按键；
//   wall-x 竖在 x-z 面（宽沿 x、深为高度、厚沿 y）——朝左下的屏幕、窗口；
//   wall-y 竖在 y-z 面（宽沿 y、深为高度、厚沿 x）——朝右下的屏幕、窗口。
// radius 圆角半径；tone：edge 普通墨线，hi 信号蓝（场景里唯一的"活"物体），ghost 虚线淡出（尚未支持）。
// insets 可见面上的圆角凹槽或屏幕区 [a, b, 宽, 深, 圆角]；rows 可见面上的文字行 [a, b, 长度]，
// rowTone 为 hi 时文字行用信号蓝；hasCornerEdge 为较高的盒子画出最靠近观察者的竖棱。
// 默认插槽画在棱柱之后，用于叠放在它上面的细节。
import { computed } from "vue"
import { getFaceLine, getFaceRoundedRect, getIsoPrism, type IsoPlane, type Vec3 } from "./iso"

const props = withDefaults(
  defineProps<{
    at: Vec3
    size: Vec3
    plane?: IsoPlane
    radius?: number
    tone?: "edge" | "hi" | "ghost"
    insets?: [number, number, number, number, number][]
    rows?: [number, number, number][]
    rowTone?: "lo" | "hi"
    hasCornerEdge?: boolean
  }>(),
  { plane: "floor", radius: 6, tone: "edge", insets: () => [], rows: () => [], rowTone: "lo", hasCornerEdge: false },
)

const prism = computed(() => getIsoPrism(props.plane, props.at, props.size, props.radius))
const insetPaths = computed(() => props.insets.map((inset) => getFaceRoundedRect(prism.value.getFacePoint, inset)))
const rowPaths = computed(() => props.rows.map((row) => getFaceLine(prism.value.getFacePoint, row)))
</script>

<template>
  <g class="iso-slab" :class="`iso-slab--${tone}`">
    <path class="iso-silhouette" :d="prism.silhouette" />
    <path class="iso-rim" :d="prism.rim" />
    <path v-if="hasCornerEdge" class="iso-corner" :d="prism.cornerEdge" />
    <path v-for="(inset, index) in insetPaths" :key="`inset-${index}`" class="iso-inset" :d="inset" />
    <path v-for="(row, index) in rowPaths" :key="`row-${index}`" class="iso-row" :class="`iso-row--${rowTone}`" :d="row" />
    <slot />
  </g>
</template>
