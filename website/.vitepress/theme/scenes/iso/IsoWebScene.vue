<script setup lang="ts">
// iso-web：左边一部朝右下的手机，右边一扇朝左下的浏览器窗口，像翻开的书页一样相对而立；
// 浏览器有地址栏、对话列表和输入框，手机是同一页面的窄屏版。用户消息同时出现在两块屏幕上，
// 回复在两边同步逐行写出，信号蓝的光标在两边跟着最新一行前进。静态画面是两边都已写完。
import IsoSlab from "../primitives/IsoSlab.vue"
import SceneCanvas from "../primitives/SceneCanvas.vue"
import { getFaceLine, getFaceRoundedRect, getIsoTranslate, type Vec3 } from "../primitives/iso"
import { getShowStops, vTrack, type SceneStop } from "../primitives/motion"

// 浏览器竖在 x-z 面上、正面朝左下；手机竖在 y-z 面上、正面朝右下。
const browserAt: Vec3 = [-17, 26, -4]
const browserSize: Vec3 = [170, 104, 6]
const phoneAt: Vec3 = [-27, 53, -12]
const phoneSize: Vec3 = [48, 92, 6]

// getBrowserPoint、getPhonePoint 把屏幕正面上从左上角量起的 (u, v)（u 向右、v 向下）换成世界坐标。
function getBrowserPoint(u: number, v: number): Vec3 {
  return [browserAt[0] + u, browserAt[1] + browserSize[2], browserAt[2] + browserSize[1] - v]
}

function getPhonePoint(u: number, v: number): Vec3 {
  return [phoneAt[0] + phoneSize[2], phoneAt[1] + phoneSize[0] - u, phoneAt[2] + phoneSize[1] - v]
}

type FacePoint = (u: number, v: number) => Vec3

// 两块屏幕上的同一段对话：u 是行首位置，rows 是各行的竖向位置和长度。
const screens = [
  {
    key: "phone",
    facePoint: getPhonePoint as FacePoint,
    replyU: 7,
    rows: [[32, 30], [39, 24], [46, 31], [53, 15]],
    userBubble: getFaceRoundedRect(getPhonePoint, [22, 14, 20, 9, 4.5]),
    chrome: [
      getFaceRoundedRect(getPhonePoint, [18, 4, 12, 3.5, 1.75]),
      getFaceRoundedRect(getPhonePoint, [5, 76, 38, 10, 5]),
    ],
    sidebarRows: [] as string[],
    caretHeight: 5,
  },
  {
    key: "browser",
    facePoint: getBrowserPoint as FacePoint,
    replyU: 56,
    rows: [[46, 92], [55, 76], [64, 98], [73, 50]],
    userBubble: getFaceRoundedRect(getBrowserPoint, [118, 24, 40, 11, 5.5]),
    chrome: [
      ...[9, 16, 23].map((u) => getFaceRoundedRect(getBrowserPoint, [u - 2, 5, 4, 4, 2])),
      getFaceRoundedRect(getBrowserPoint, [34, 4, 128, 6, 3]),
      getFaceRoundedRect(getBrowserPoint, [6, 15, 38, 83, 4]),
      getFaceRoundedRect(getBrowserPoint, [52, 84, 110, 12, 6]),
    ],
    sidebarRows: [[24, 24], [32, 18], [40, 22]].map(([v, length]) => getFaceLine(getBrowserPoint, [11, v, length])),
    caretHeight: 6,
  },
]

// 回复逐行写出：两块屏幕上的同一行同时开始、同时写完。
const userAt = 0.08
const replyTimes = [0.2, 0.34, 0.48, 0.62].map((startAt, index) => ({ startAt, span: [0.12, 0.1, 0.12, 0.07][index] }))

function getRowStops(index: number): SceneStop[] {
  const { startAt, span } = replyTimes[index]
  return [
    [startAt - 0.001, { opacity: 0, transform: "scale(0)" }],
    [startAt, { opacity: 1, transform: "scale(0)", easing: "linear" }],
    [startAt + span, { opacity: 1, transform: "scale(1)" }],
    [0.93, { opacity: 1, transform: "scale(1)" }],
    [0.96, { opacity: 0, transform: "scale(1)" }],
  ]
}

// getCaret 返回一块屏幕上光标的路径和轨道：静态位置在最后一行末尾，写每一行时跟着行尾前进。
function getCaret(screen: (typeof screens)[number]) {
  const lastRow = screen.rows[screen.rows.length - 1]
  const homeU = screen.replyU + lastRow[1] + 3
  const homeV = lastRow[0]
  const toRow = (u: number, v: number) => {
    const [x0, y0, z0] = screen.facePoint(homeU, homeV)
    const [x1, y1, z1] = screen.facePoint(u, v)
    return getIsoTranslate([x1 - x0, y1 - y0, z1 - z0])
  }
  const stops: SceneStop[] = [[0, { opacity: 0, transform: toRow(screen.replyU, screen.rows[0][0]) }]]
  screen.rows.forEach(([v, length], index) => {
    const { startAt, span } = replyTimes[index]
    const start = toRow(screen.replyU + 1, v)
    const end = toRow(screen.replyU + length + 3, v)
    stops.push([startAt - 0.001, { opacity: index === 0 ? 0 : 1, transform: start }])
    stops.push([startAt, { opacity: 1, transform: start, easing: "linear" }])
    stops.push([startAt + span, { opacity: 1, transform: end }])
    const next = replyTimes[index + 1]
    if (next) stops.push([next.startAt - 0.002, { opacity: 1, transform: end }])
  })
  stops.push([0.93, { opacity: 1, transform: "translate(0px, 0px)" }], [0.96, { opacity: 0, transform: "translate(0px, 0px)" }])
  const path = getFaceRoundedRect(screen.facePoint, [homeU, homeV - screen.caretHeight / 2, 3, screen.caretHeight, 0.5])
  return { path, stops }
}

const carets = screens.map(getCaret)
</script>

<template>
  <SceneCanvas view-box="-122 -106 244 212" :period="10000">
    <IsoSlab :at="phoneAt" :size="phoneSize" plane="wall-y" :radius="9" />
    <IsoSlab :at="browserAt" :size="browserSize" plane="wall-x" :radius="8" />

    <g v-for="(screen, screenIndex) in screens" :key="screen.key" :class="`web-screen--${screen.key}`">
      <path v-for="path in screen.chrome" :key="path" class="iso-inset" :d="path" />
      <path v-for="path in screen.sidebarRows" :key="path" class="iso-row" :d="path" />
      <path v-track="getShowStops(userAt, 0.93, 0.03)" class="user-bubble" :d="screen.userBubble" />
      <path
        v-for="(row, index) in screen.rows"
        :key="row[0]"
        v-track="getRowStops(index)"
        class="stream-row"
        :d="getFaceLine(screen.facePoint, [screen.replyU, row[0], row[1]])"
      />
      <path v-track="carets[screenIndex].stops" class="caret" :d="carets[screenIndex].path" />
    </g>
  </SceneCanvas>
</template>

<style scoped>
.user-bubble {
  fill: none;
  stroke: var(--scene-mid);
}

.stream-row {
  fill: none;
  stroke: var(--scene-mid);
  stroke-width: 2;
  stroke-linecap: round;
  transform-box: fill-box;
  transform-origin: 0 0;
}

/* 手机朝右下，行的左端在外框的左下角。 */
.web-screen--phone .stream-row {
  transform-origin: 0 100%;
}

.caret {
  fill: var(--scene-hi);
  stroke: none;
}
</style>
