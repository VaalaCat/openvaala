import { memo } from "react"
import { Streamdown, type Components } from "streamdown"
import { CodeView } from "@/components/base/code-view"
import { ImagePreview } from "@/components/base/image-preview"

// 显示一段 Markdown（模型回复、思考过程、摘要）：生成中由 streamdown 补齐没闭合的语法；
// 代码块与图片使用共用输出和预览组件
export const MarkdownView = memo(function MarkdownView({
  text,
  isStreaming = false,
}: {
  text: string
  isStreaming?: boolean
}) {
  return (
    <Streamdown
      className="markdown-view text-reading"
      isAnimating={isStreaming}
      controls={false}
      linkSafety={{ enabled: false }}
      lineNumbers={false}
      components={markdownComponents}
    >
      {text}
    </Streamdown>
  )
})

// 标题、链接和代码换成项目的文字角色；其余标签用 streamdown 自带的样式。
// 回复里的标题从 h3 起，不和页面标题抢层级
const markdownComponents: Components = {
  img: ({ src, alt }) =>
    typeof src === "string" ? <ImagePreview src={src} alt={alt} /> : null,
  h1: ({ children }) => <h3 className="mt-4 text-title">{children}</h3>,
  h2: ({ children }) => <h3 className="mt-4 text-title">{children}</h3>,
  h3: ({ children }) => <h4 className="mt-4 text-section">{children}</h4>,
  h4: ({ children }) => <h5 className="mt-3 text-section">{children}</h5>,
  h5: ({ children }) => <h6 className="mt-3 text-section">{children}</h6>,
  h6: ({ children }) => <h6 className="mt-3 text-section">{children}</h6>,
  a: ({ href, children }) => (
    <a
      className="wrap-anywhere text-link"
      // streamdown 给生成到一半的链接一个占位地址，这时还不能点
      href={href === "streamdown:incomplete-link" ? undefined : href}
      target="_blank"
      rel="noreferrer"
    >
      {children}
    </a>
  ),
  inlineCode: ({ children }) => (
    <code className="bg-muted px-1 text-code-reading">{children}</code>
  ),
  code: ({ className, children }) => (
    <CodeView
      code={String(children ?? "")}
      lang={/language-(\S+)/.exec(className ?? "")?.[1] ?? "text"}
    />
  ),
}
