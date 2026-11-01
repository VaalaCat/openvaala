// 构建索引（Node）和浏览器查询共用同一个切词规则，两边切出的词才能对上。
const wordSegmenter = new Intl.Segmenter("zh", { granularity: "word" })

// tokenizeSearchText 把文本切成词：中文按词典分词，英文和代码标识按单词切开。
export function tokenizeSearchText(text: string): string[] {
  const words: string[] = []
  for (const segment of wordSegmenter.segment(text)) {
    if (segment.isWordLike) words.push(segment.segment)
  }
  return words
}
