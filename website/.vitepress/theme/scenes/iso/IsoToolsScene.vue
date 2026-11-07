<script setup lang="ts">
// iso-tools：一个工具托盘上排着两行按键，每个按键顶面印着一种工具的图标：读文件、glob、grep、改文件、
// 命令、子 Agent、发消息、MCP。Agent 依次调用 read_file、grep、apply_patch、bash、spawn_agent：
// 信号蓝描边从一个按键移到下一个，按键随之按下又弹起，托盘右上方同时显示正在调用的工具名。
// 静态画面是最后一次调用：spawn_agent 的按键亮着，旁边写着它的名字。
import IsoSlab from "../primitives/IsoSlab.vue"
import SceneCanvas from "../primitives/SceneCanvas.vue"
import { getIsoPrism, project, type Vec2, type Vec3 } from "../primitives/iso"
import { vTrack, type SceneStop } from "../primitives/motion"

const trayAt: Vec3 = [-112, -54, 0]
const traySize: Vec3 = [224, 108, 10]
const keySize: Vec3 = [38, 36, 9]
const keyColumns = [16, 68, 120, 172]
const keyRows = [14, 58]
const pressDepth = 3.5

type ToolGlyph = "file" | "glob" | "grep" | "patch" | "command" | "agent" | "message" | "mcp"

// 两行按键的图标，从后排左边开始。
const glyphRows: ToolGlyph[][] = [
  ["file", "glob", "grep", "patch"],
  ["command", "agent", "message", "mcp"],
]

// 依次调用的工具：glyph 对应按键，pressAt 是按下的时刻。
const toolCalls: { glyph: ToolGlyph; name: string; pressAt: number }[] = [
  { glyph: "file", name: "read_file", pressAt: 0.06 },
  { glyph: "grep", name: "grep", pressAt: 0.23 },
  { glyph: "patch", name: "apply_patch", pressAt: 0.4 },
  { glyph: "command", name: "bash", pressAt: 0.57 },
  { glyph: "agent", name: "spawn_agent", pressAt: 0.74 },
]
const lastCall = toolCalls[toolCalls.length - 1]
const callSpan = 0.15

// 图标画在以按键顶面中心为原点的图标坐标 (u, v) 里：u 向右、v 向下，范围约 ±9。
// 图标坐标转 45° 后贴到顶面，投影出来正对读者，竖向按地面的透视压扁；glyphScale 是图标相对图标坐标的放大倍数。
const glyphScale = 1.15

function getGlyphPaths(glyph: ToolGlyph, keyAt: Vec3): string[] {
  const { getFacePoint } = getIsoPrism("floor", keyAt, keySize, 7)
  const toScreen = ([u, v]: [number, number]): Vec2 =>
    project(
      getFacePoint(
        keySize[0] / 2 + ((u + v) * glyphScale) / Math.sqrt(3),
        keySize[1] / 2 + ((v - u) * glyphScale) / Math.sqrt(3),
      ),
    )
  const polyline = (points: [number, number][], isClosed = false) =>
    points.map((point, index) => `${index === 0 ? "M" : "L"}${toScreen(point).map((value) => value.toFixed(2)).join(" ")}`).join("") + (isClosed ? "Z" : "")
  const circle = (u: number, v: number, r: number) =>
    polyline(Array.from({ length: 24 }, (_, index) => [u + r * Math.cos((index * Math.PI) / 12), v + r * Math.sin((index * Math.PI) / 12)]), true)
  const rect = (u: number, v: number, width: number, height: number, r: number) => {
    const corners: [number, number, number][] = [[u + width - r, v + r, -90], [u + width - r, v + height - r, 0], [u + r, v + height - r, 90], [u + r, v + r, 180]]
    return polyline(corners.flatMap(([cu, cv, from]) => Array.from({ length: 4 }, (_, step) => {
      const angle = ((from + step * 30) * Math.PI) / 180
      return [cu + r * Math.cos(angle), cv + r * Math.sin(angle)] as [number, number]
    })), true)
  }
  switch (glyph) {
    case "file":
      return [rect(-6, -8, 12, 16, 2), polyline([[-3, -3], [3, -3]]), polyline([[-3, 1], [3, 1]]), polyline([[-3, 5], [1, 5]])]
    case "glob":
      return [rect(-8, -8, 7, 7, 1.5), rect(1, -8, 7, 7, 1.5), rect(-8, 1, 7, 7, 1.5), rect(1, 1, 7, 7, 1.5)]
    case "grep":
      return [circle(-2, -2, 5.5), polyline([[2, 2], [7, 7]])]
    case "patch":
      return [polyline([[-8, 8], [-8, 4], [4, -8], [8, -4], [-4, 8]], true), polyline([[1, -5], [5, -1]])]
    case "command":
      return [polyline([[-7, -5], [-2, 0], [-7, 5]]), polyline([[0, 5], [7, 5]])]
    case "agent":
      return [circle(0, -6, 2.5), circle(-6, 6, 2.5), circle(6, 6, 2.5), polyline([[0, -3.5], [0, 1], [-6, 1], [-6, 3.5]]), polyline([[0, 1], [6, 1], [6, 3.5]])]
    case "message":
      return [rect(-8, -7, 16, 11, 3.5), polyline([[-4, 4], [-6, 8], [0, 4]])]
    case "mcp":
      return [polyline([[-3, -8], [-3, -4]]), polyline([[3, -8], [3, -4]]), rect(-6, -4, 12, 7, 2), polyline([[0, 3], [0, 8]])]
  }
}

// 每个按键在托盘顶面上的位置；按键下方是略大的凹槽。
const keys = keyRows.flatMap((b, row) =>
  keyColumns.map((a, column) => {
    const at: Vec3 = [trayAt[0] + a, trayAt[1] + b, traySize[2]]
    const glyph = glyphRows[row][column]
    return { key: glyph, at, glyphPaths: getGlyphPaths(glyph, at), call: toolCalls.find((call) => call.glyph === glyph) }
  }),
)

// getPressStops 让按键在 pressAt 后按下、停住再弹起；静态画面停在弹起的位置。
function getPressStops(pressAt: number): SceneStop[] {
  const up = { transform: "translateY(0px)" }
  const down = { transform: `translateY(${pressDepth}px)` }
  return [
    [pressAt + 0.02, up],
    [pressAt + 0.045, down],
    [pressAt + 0.09, down],
    [pressAt + 0.12, up],
  ]
}

// getCallStops 让信号蓝按键和工具名在这次调用期间亮着，前后淡入淡出；最后一次调用一直亮到循环末尾。
function getCallStops(call: (typeof toolCalls)[number]): SceneStop[] {
  const endAt = call === lastCall ? 0.93 : call.pressAt + callSpan
  return [
    [call.pressAt - 0.015, { opacity: 0 }],
    [call.pressAt + 0.015, { opacity: 1 }],
    [endAt - 0.02, { opacity: 1 }],
    [endAt + 0.01, { opacity: 0 }],
  ]
}

// 工具名固定在托盘右上方的空白处。
const callNameAt = { x: 62, y: -58 }
</script>

<template>
  <SceneCanvas view-box="-184 -103 368 196" :period="11000">
    <IsoSlab
      :at="trayAt"
      :size="traySize"
      :radius="14"
      :insets="keys.map((key) => [key.at[0] - trayAt[0] - 3, key.at[1] - trayAt[1] - 3, keySize[0] + 6, keySize[1] + 6, 8] as [number, number, number, number, number])"
    />
    <template v-for="key in keys" :key="key.key">
      <g v-if="key.call" v-track="getPressStops(key.call.pressAt)">
        <IsoSlab :at="key.at" :size="keySize" :radius="7">
          <path v-for="path in key.glyphPaths" :key="path" class="key-glyph" :d="path" />
        </IsoSlab>
        <g v-track="getCallStops(key.call)" :style="{ opacity: key.call === lastCall ? 1 : 0 }">
          <IsoSlab :at="key.at" :size="keySize" :radius="7" tone="hi">
            <path v-for="path in key.glyphPaths" :key="path" class="key-glyph key-glyph--hi" :d="path" />
          </IsoSlab>
        </g>
      </g>
      <IsoSlab v-else :at="key.at" :size="keySize" :radius="7">
        <path v-for="path in key.glyphPaths" :key="path" class="key-glyph" :d="path" />
      </IsoSlab>
    </template>

    <text
      v-for="call in toolCalls"
      :key="call.name"
      v-track="getCallStops(call)"
      class="scene-label scene-label--ink tool-call-name"
      :style="{ opacity: call === lastCall ? 1 : 0 }"
      :x="callNameAt.x"
      :y="callNameAt.y"
    >
      {{ call.name }}
    </text>
  </SceneCanvas>
</template>

<style scoped>
.key-glyph {
  fill: none;
  stroke: var(--scene-mid);
  stroke-width: 1.2;
  stroke-linecap: round;
  stroke-linejoin: round;
}

.key-glyph--hi {
  stroke: var(--scene-hi);
}
</style>
