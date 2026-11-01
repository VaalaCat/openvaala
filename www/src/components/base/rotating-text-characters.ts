export type RotatingCharacter = {
  id: number
  value: string
}

const graphemeSegmenter = new Intl.Segmenter(undefined, {
  granularity: "grapheme",
})
const maxVisibleCharacters = 160
const maxMatchingCells = (maxVisibleCharacters + 1) ** 2

// 按完整字素匹配共同文字，为新增文字分配递增标识。
export function reconcileRotatingCharacters(
  previous: readonly RotatingCharacter[],
  text: string,
  nextID: number
) {
  // 1. 展示最多 160 个完整字素，长文字末尾省略，随即停止分词。
  const values: string[] = []
  for (const { segment } of graphemeSegmenter.segment(text)) {
    if (values.length === maxVisibleCharacters) {
      values[maxVisibleCharacters - 1] = "…"
      break
    }
    values.push(segment)
  }

  // 2. 直接复用相同前后缀。
  const matches = new Map<number, RotatingCharacter>()
  let prefixLength = 0
  while (
    prefixLength < previous.length &&
    previous[prefixLength].value === values[prefixLength]
  ) {
    matches.set(prefixLength, previous[prefixLength])
    prefixLength++
  }
  let previousEnd = previous.length
  let nextEnd = values.length
  while (
    previousEnd > prefixLength &&
    nextEnd > prefixLength &&
    previous[previousEnd - 1].value === values[nextEnd - 1]
  ) {
    matches.set(--nextEnd, previous[--previousEnd])
  }

  // 3. 在可见字素数量对应的矩阵内匹配最长公共子序列。
  const previousLength = previousEnd - prefixLength
  const nextLength = nextEnd - prefixLength
  if ((previousLength + 1) * (nextLength + 1) <= maxMatchingCells) {
    const lengths = Array.from(
      { length: previousLength + 1 },
      () => new Uint32Array(nextLength + 1)
    )
    for (let before = previousLength - 1; before >= 0; before--) {
      for (let after = nextLength - 1; after >= 0; after--) {
        lengths[before][after] =
          previous[prefixLength + before].value === values[prefixLength + after]
            ? lengths[before + 1][after + 1] + 1
            : Math.max(lengths[before + 1][after], lengths[before][after + 1])
      }
    }
    let before = 0
    let after = 0
    while (before < previousLength && after < nextLength) {
      if (
        previous[prefixLength + before].value === values[prefixLength + after]
      ) {
        matches.set(prefixLength + after, previous[prefixLength + before])
        before++
        after++
      } else if (lengths[before + 1][after] >= lengths[before][after + 1]) {
        before++
      } else {
        after++
      }
    }
  }

  // 4. 复用旧标识，并按文字顺序列出进入和离开的字素。
  const characters = values.map(
    (value, index) => matches.get(index) ?? { id: nextID++, value }
  )
  const retainedIDs = new Set(Array.from(matches.values(), ({ id }) => id))
  return {
    characters,
    nextID,
    entering: characters.filter(({ id }) => !retainedIDs.has(id)),
    exiting: previous.filter(({ id }) => !retainedIDs.has(id)),
    isAppend: previous.length > 0 && prefixLength === previous.length,
  }
}
