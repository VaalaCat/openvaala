<script setup lang="ts">
// SceneCanvas：每个场景的 SVG 画布和播放控制。
// viewBox 画布坐标；period 循环周期（毫秒）；label 给读屏软件的整体描述。
// 挂载后为画布内所有 v-track 元素创建同一周期的动画，进入视口播放、离开视口暂停；画布内的 CSS 动画同样随 is-playing 暂停。
import { onMounted, onUnmounted, ref } from "vue"
import { createTrackAnimations } from "./motion"

const props = defineProps<{ viewBox: string; period: number; label?: string }>()

const rootEl = ref<HTMLElement>()
const isPlaying = ref(false)

let animations: Animation[] = []
let visibilityObserver: IntersectionObserver | undefined
let isVisible = false

// updatePlayback 在画布可见且页面处于前台时播放，否则暂停。
function updatePlayback() {
  isPlaying.value = isVisible && !document.hidden
  for (const animation of animations) {
    if (isPlaying.value) animation.play()
    else animation.pause()
  }
}

// handleVisibility 在画布进入或离开视口时一起播放或暂停全部动画。
function handleVisibility(entries: IntersectionObserverEntry[]) {
  isVisible = entries.some((entry) => entry.isIntersecting && entry.intersectionRatio >= 0.2)
  updatePlayback()
}

onMounted(() => {
  if (!rootEl.value) return
  // 1. 创建暂停在起点的动画。
  animations = createTrackAnimations(rootEl.value, props.period)
  // 2. 按页面前后台和画布可见性播放或暂停。
  document.addEventListener("visibilitychange", updatePlayback)
  visibilityObserver = new IntersectionObserver(handleVisibility, { threshold: 0.2 })
  visibilityObserver.observe(rootEl.value)
})

onUnmounted(() => {
  visibilityObserver?.disconnect()
  document.removeEventListener("visibilitychange", updatePlayback)
  for (const animation of animations) animation.cancel()
})
</script>

<template>
  <div ref="rootEl" class="scene-canvas" :class="{ 'is-playing': isPlaying }">
    <svg :viewBox="viewBox" :role="label ? 'img' : undefined" :aria-label="label" :aria-hidden="label ? undefined : 'true'">
      <slot />
    </svg>
    <div v-if="$slots.caption" class="scene-captions"><slot name="caption" /></div>
  </div>
</template>
