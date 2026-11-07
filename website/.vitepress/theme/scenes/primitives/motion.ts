import type { Directive } from "vue"
import type { Vec2 } from "./iso"

// 场景动画的时间约定：每个场景有一个循环周期，场景里所有动作都写成周期内的停靠点，
// 由 SceneCanvas 在同一时刻用 Web Animations 创建，进入视口时一起播放、离开时一起暂停，
// 因而始终同步。脚本加载前元素显示模板里写的静态样式，所以模板样式写成有意义的最终状态。

// SceneStop 是轨道上的一个停靠点：at 是循环中的位置（0–1），style 是该时刻的样式。
// style.easing 决定从这个停靠点到下一个停靠点的过渡曲线（默认 ease-out），相同样式相邻即保持不动。
export type SceneStop = [at: number, style: Keyframe]

const trackByElement = new WeakMap<Element, SceneStop[]>()

// vTrack 把一条停靠点轨道挂到元素上：v-track="[[0, { opacity: 0 }], [0.2, { opacity: 1 }]]"。
export const vTrack: Directive<Element, SceneStop[]> = {
  created(element, binding) {
    trackByElement.set(element, binding.value)
    element.setAttribute("data-track", "")
  },
  updated(element, binding) {
    trackByElement.set(element, binding.value)
  },
  getSSRProps() {
    return { "data-track": "" }
  },
}

const defaultEasing = "cubic-bezier(0.2, 0.8, 0.2, 1)"

// getKeyframes 把停靠点补齐到 0 和 1，换成 Web Animations 关键帧。
function getKeyframes(stops: SceneStop[]): Keyframe[] {
  const sorted = [...stops].sort((p, q) => p[0] - q[0])
  if (sorted[0][0] > 0) sorted.unshift([0, sorted[0][1]])
  if (sorted[sorted.length - 1][0] < 1) sorted.push([1, sorted[sorted.length - 1][1]])
  return sorted.map(([at, style]) => ({ easing: defaultEasing, ...style, offset: Math.min(1, Math.max(0, at)) }))
}

// createTrackAnimations 为 root 里所有挂了轨道的元素创建暂停在起点的动画。
export function createTrackAnimations(root: Element, period: number): Animation[] {
  const animations: Animation[] = []
  for (const element of root.querySelectorAll("[data-track]")) {
    const stops = trackByElement.get(element)
    if (!stops || stops.length === 0) continue
    const animation = element.animate(getKeyframes(stops), { duration: period, iterations: Infinity, fill: "both" })
    animation.pause()
    animation.currentTime = 0
    animations.push(animation)
  }
  return animations
}

// ---------- 常用轨道 ----------

// getShowStops 让元素在 [start, end] 之间可见，前后各用 fade 的时长淡入淡出。
export function getShowStops(start: number, end = 0.96, fade = 0.03): SceneStop[] {
  return [
    [start - fade, { opacity: 0 }],
    [start, { opacity: 1 }],
    [end, { opacity: 1 }],
    [end + fade, { opacity: 0 }],
  ]
}

// getCaptionStops 用于依次替换的说明文字：相邻两段共用同一个边界时，前一段淡出后下一段才淡入。
export function getCaptionStops(start: number, end: number): SceneStop[] {
  return getShowStops(start + 0.012, end - 0.012, 0.012)
}

// getRiseStops 让元素在 start 时从下方 rise 个单位处升起淡入，到 end 时淡出。
export function getRiseStops(start: number, end = 0.96, rise = 6, fade = 0.04): SceneStop[] {
  return [
    [start - fade, { opacity: 0, transform: `translateY(${rise}px)` }],
    [start, { opacity: 1, transform: "translateY(0px)" }],
    [end, { opacity: 1, transform: "translateY(0px)" }],
    [end + 0.03, { opacity: 0, transform: "translateY(0px)" }],
  ]
}

// getTravelStops 让数据包按路程匀速经过折线上的每个点，只改变位移和透明度。
export function getTravelStops(start: number, span: number, points: Vec2[], isReverse = false): SceneStop[] {
  const route = isReverse ? [...points].reverse() : points
  const distances = route.map((point, index) => index === 0 ? 0 : Math.hypot(point[0] - route[index - 1][0], point[1] - route[index - 1][1]))
  const total = distances.reduce((sum, distance) => sum + distance, 0)
  let traveled = 0
  const stops: SceneStop[] = route.map(([x, y], index) => {
    traveled += distances[index]
    return [start + span * traveled / total, { opacity: 1, transform: `translate(${x}px, ${y}px)`, easing: "linear" }]
  })
  return [[start - 0.008, { ...stops[0][1], opacity: 0 }], ...stops, [start + span + 0.008, { ...stops[stops.length - 1][1], opacity: 0 }]]
}

// getPulseStops 让元素在 start 附近轻轻放大再回落一次，表示被选中或收到数据。
export function getPulseStops(start: number, span = 0.08, scale = 1.06): SceneStop[] {
  return [
    [start, { transform: "scale(1)" }],
    [start + span / 2, { transform: `scale(${scale})` }],
    [start + span, { transform: "scale(1)" }],
  ]
}
