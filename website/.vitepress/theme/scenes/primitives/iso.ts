// 等轴测几何：世界坐标 x 指向右下、y 指向左下、z 指向上，按 30° 等轴测投影到 SVG 平面。
// 场景用世界坐标摆放物体，组件在这里换算成路径；所有长度单位都是 SVG 用户单位。

export type Vec3 = [number, number, number]
export type Vec2 = [number, number]

// IsoPlane 决定圆角棱柱的朝向：
// floor 平放，轮廓在 x-y 面，厚度沿 z 向上；
// wall-x 竖立，轮廓在 x-z 面，厚度沿 y 朝向观察者左下；
// wall-y 竖立，轮廓在 y-z 面，厚度沿 x 朝向观察者右下。
export type IsoPlane = "floor" | "wall-x" | "wall-y"

const cos30 = Math.cos(Math.PI / 6)

// project 把世界坐标投影成 SVG 坐标。
export function project([x, y, z]: Vec3): Vec2 {
  return [(x - y) * cos30, (x + y) * 0.5 - z]
}

// getIsoTranslate 把世界坐标里的位移换成 CSS translate，供动画轨道移动物体。
export function getIsoTranslate(delta: Vec3): string {
  const [x, y] = project(delta)
  return `translate(${round(x)}px, ${round(y)}px)`
}

function round(value: number): number {
  return Math.round(value * 100) / 100
}

function toPath(points: Vec2[], isClosed: boolean): string {
  if (points.length === 0) return ""
  const commands = points.map(([x, y], index) => `${index === 0 ? "M" : "L"}${round(x)} ${round(y)}`)
  return commands.join("") + (isClosed ? "Z" : "")
}

// mapToWorld 把棱柱局部坐标（a、b 在轮廓面内，c 沿厚度）换成世界坐标。
function mapToWorld(plane: IsoPlane, origin: Vec3, a: number, b: number, c: number): Vec3 {
  const [x, y, z] = origin
  if (plane === "floor") return [x + a, y + b, z + c]
  if (plane === "wall-x") return [x + a, y + c, z + b]
  return [x + c, y + a, z + b]
}

interface ProfilePoint {
  a: number
  b: number
  // 轮廓外法线在 a、b 方向上的分量之和；大于 0 表示这一段侧面朝向观察者。
  facing: number
}

// getRoundedProfile 沿逆时针采样圆角矩形轮廓，圆角各取 7 个点。
function getRoundedProfile(width: number, depth: number, radius: number): ProfilePoint[] {
  const r = Math.max(0, Math.min(radius, width / 2, depth / 2))
  const corners: [number, number, number][] = [
    [width - r, depth - r, 0],
    [r, depth - r, 90],
    [r, r, 180],
    [width - r, r, 270],
  ]
  const points: ProfilePoint[] = []
  for (const [ca, cb, startAngle] of corners) {
    for (let step = 0; step <= 6; step++) {
      const angle = ((startAngle + step * 15) * Math.PI) / 180
      const na = Math.cos(angle)
      const nb = Math.sin(angle)
      points.push({ a: ca + na * r, b: cb + nb * r, facing: na + nb })
    }
  }
  return points
}

function getConvexHull(points: Vec2[]): Vec2[] {
  const sorted = [...points].sort((p, q) => p[0] - q[0] || p[1] - q[1])
  const cross = (o: Vec2, p: Vec2, q: Vec2) => (p[0] - o[0]) * (q[1] - o[1]) - (p[1] - o[1]) * (q[0] - o[0])
  const lower: Vec2[] = []
  for (const point of sorted) {
    while (lower.length >= 2 && cross(lower[lower.length - 2], lower[lower.length - 1], point) <= 0) lower.pop()
    lower.push(point)
  }
  const upper: Vec2[] = []
  for (const point of sorted.reverse()) {
    while (upper.length >= 2 && cross(upper[upper.length - 2], upper[upper.length - 1], point) <= 0) upper.pop()
    upper.push(point)
  }
  return lower.slice(0, -1).concat(upper.slice(0, -1))
}

export interface IsoPrismGeometry {
  // 外轮廓：两个端面投影的凸包，用纸色填充遮住后面的物体。
  silhouette: string
  // 可见端面朝向观察者一侧的边，即正面与侧面的分界线。
  rim: string
  // 最靠近观察者的竖棱，只给较高的盒子使用。
  cornerEdge: string
  // getFacePoint 返回可见端面上局部坐标 (a, b) 的世界坐标，用于摆放面上的细节。
  getFacePoint: (a: number, b: number) => Vec3
}

// getIsoPrism 计算一个圆角棱柱的轮廓、正面分界线和正面竖棱。
export function getIsoPrism(plane: IsoPlane, origin: Vec3, size: Vec3, radius: number): IsoPrismGeometry {
  const [width, depth, height] = size
  const profile = getRoundedProfile(width, depth, radius)
  const back = profile.map((point) => project(mapToWorld(plane, origin, point.a, point.b, 0)))
  const front = profile.map((point) => project(mapToWorld(plane, origin, point.a, point.b, height)))

  // 正面分界线是可见端面上 facing > 0 的连续一段，从第一个由背转正的点开始。
  const startIndex = profile.findIndex(
    (point, index) => point.facing > 1e-6 && profile[(index - 1 + profile.length) % profile.length].facing <= 1e-6,
  )
  const rimPoints: Vec2[] = []
  for (let offset = 0; offset < profile.length; offset++) {
    const index = (startIndex + offset) % profile.length
    if (profile[index].facing <= 1e-6) break
    rimPoints.push(front[index])
  }

  const nearestIndex = profile.reduce((best, point, index) => (point.facing > profile[best].facing ? index : best), 0)

  return {
    silhouette: toPath(getConvexHull(back.concat(front)), true),
    rim: toPath(rimPoints, false),
    cornerEdge: toPath([back[nearestIndex], front[nearestIndex]], false),
    getFacePoint: (a, b) => mapToWorld(plane, origin, a, b, height),
  }
}

// getFaceRoundedRect 返回棱柱可见端面上一块圆角矩形（凹槽、屏幕区域）的闭合路径。
export function getFaceRoundedRect(
  getFacePoint: (a: number, b: number) => Vec3,
  [a, b, width, depth, radius]: [number, number, number, number, number],
): string {
  const profile = getRoundedProfile(width, depth, radius)
  return toPath(
    profile.map((point) => project(getFacePoint(a + point.a, b + point.b))),
    true,
  )
}

// getFaceLine 返回端面上沿 a 方向的一条线段（文字行、刻度）。
export function getFaceLine(
  getFacePoint: (a: number, b: number) => Vec3,
  [a, b, length]: [number, number, number],
): string {
  return toPath([project(getFacePoint(a, b)), project(getFacePoint(a + length, b))], false)
}

// getRoundedPolylinePath 把世界坐标折线投影后在拐角处倒圆，用于线缆和数据包轨迹。
export function getRoundedPolylinePath(points: Vec3[], cornerRadius = 8): string {
  return getRounded2dPath(points.map(project), cornerRadius)
}

// getRounded2dPath 在平面折线的拐角处倒圆，流程图连线和等轴测线缆共用。
export function getRounded2dPath(points: Vec2[], cornerRadius = 8): string {
  if (points.length < 2) return ""
  let path = `M${round(points[0][0])} ${round(points[0][1])}`
  for (let index = 1; index < points.length - 1; index++) {
    const [px, py] = points[index - 1]
    const [cx, cy] = points[index]
    const [nx, ny] = points[index + 1]
    const inLength = Math.hypot(cx - px, cy - py)
    const outLength = Math.hypot(nx - cx, ny - cy)
    const r = Math.min(cornerRadius, inLength / 2, outLength / 2)
    const startX = cx - ((cx - px) / inLength) * r
    const startY = cy - ((cy - py) / inLength) * r
    const endX = cx + ((nx - cx) / outLength) * r
    const endY = cy + ((ny - cy) / outLength) * r
    path += `L${round(startX)} ${round(startY)}Q${round(cx)} ${round(cy)} ${round(endX)} ${round(endY)}`
  }
  const [lx, ly] = points[points.length - 1]
  return `${path}L${round(lx)} ${round(ly)}`
}
