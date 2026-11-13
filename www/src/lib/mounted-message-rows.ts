// 一行在列表坐标里的上下边，已量过的用实测值，没量过的用估计值
export interface MessageRowBounds {
  start: number
  end: number
}

// 根据视口位置计算需要挂载的消息行号，返回升序且不重复的行号。
export function getMountedRowIndexes(request: {
  rowBounds: readonly MessageRowBounds[]
  viewTop: number
  viewHeight: number
  overscan: number
  pinnedRecentRowCount: number
  anchorRowIndex?: number
}): number[] {
  const {
    rowBounds,
    viewTop,
    viewHeight,
    overscan,
    pinnedRecentRowCount,
    anchorRowIndex,
  } = request
  if (rowBounds.length === 0) return []

  // 1. 找出视口内的第一行和最后一行。
  const viewBottom = viewTop + viewHeight
  const firstVisible = rowBounds.findIndex((bounds) => bounds.end > viewTop)
  const firstVisibleIndex =
    firstVisible < 0 ? rowBounds.length - 1 : firstVisible
  let lastVisibleIndex = firstVisibleIndex
  while (
    lastVisibleIndex + 1 < rowBounds.length &&
    rowBounds[lastVisibleIndex + 1].start < viewBottom
  ) {
    lastVisibleIndex++
  }

  const mounted = new Set<number>()
  for (let index = firstVisibleIndex; index <= lastVisibleIndex; index++)
    mounted.add(index)

  // 2. 视口上下各补最多 overscan 行，且不超出一屏距离。
  if (viewHeight > 0) {
    const aboveLimit = viewTop - viewHeight
    for (
      let index = firstVisibleIndex - 1, count = 0;
      index >= 0 && count < overscan && rowBounds[index].end > aboveLimit;
      index--, count++
    ) {
      mounted.add(index)
    }
    const belowLimit = viewTop + 2 * viewHeight
    for (
      let index = lastVisibleIndex + 1, count = 0;
      index < rowBounds.length &&
      count < overscan &&
      rowBounds[index].start < belowLimit;
      index++, count++
    ) {
      mounted.add(index)
    }
  }

  // 3. 加入最近的常驻行和锚点行。
  for (
    let index = Math.max(0, rowBounds.length - pinnedRecentRowCount);
    index < rowBounds.length;
    index++
  ) {
    mounted.add(index)
  }
  if (
    anchorRowIndex !== undefined &&
    anchorRowIndex >= 0 &&
    anchorRowIndex < rowBounds.length
  ) {
    mounted.add(anchorRowIndex)
  }

  return [...mounted].sort((a, b) => a - b)
}
