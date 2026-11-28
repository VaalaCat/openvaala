import { useLayoutEffect, useRef, useState } from "react"
import { AnimatePresence, motion } from "motion/react"
import {
  FileIcon,
  FileText,
  FileCode,
  FileSpreadsheet,
  FileArchive,
  XIcon,
} from "lucide-react"
import { useTranslation } from "react-i18next"
import { ImagePreview } from "@/components/base/image-preview"
import { LatticeLoader } from "@/components/base/lattice-loader"
import {
  Tooltip,
  TooltipTrigger,
  TooltipContent,
} from "@/components/ui/tooltip"
import { Button } from "@/components/ui/button"
import type { Content } from "@/lib/api/protocol.gen"
import { cn, formatByteSize } from "@/lib/utils"

import { attachmentStatus } from "@/lib/attachments"

function fileIcon(mime: string) {
  if (/csv|spreadsheet|excel/.test(mime)) return FileSpreadsheet
  if (/zip|gzip|tar|compressed|archive|7z/.test(mime)) return FileArchive
  if (/javascript|json|xml|html|css|typescript|python|yaml|x-sh/.test(mime))
    return FileCode
  if (/^text\/|pdf|document|msword/.test(mime)) return FileText
  return FileIcon
}

export function AttachmentStrip({
  contents,
  onRemove,
}: {
  contents: Content[]
  onRemove?: (item: Content) => void
}) {
  const { t, i18n } = useTranslation()
  const ref = useRef<HTMLDivElement>(null)
  const followEnd = useRef(false)
  const [overflow, setOverflow] = useState("")
  const files = contents.filter((item) => item.image || item.file)
  const last = files.at(-1)
  const lastKey =
    last &&
    (attachmentStatus.get(last)?.id ?? last.file?.path ?? last.image?.data_url)
  const editable = !!onRemove
  const count = files.length
  const previousCount = useRef(count)
  useLayoutEffect(() => {
    const node = ref.current
    if (!node) return
    followEnd.current = editable && count > previousCount.current
    previousCount.current = count
    const measure = () => {
      const left = node.scrollLeft > 1
      const right = node.scrollWidth - node.clientWidth - node.scrollLeft > 1
      setOverflow(left && right ? "both" : left ? "left" : right ? "right" : "")
    }
    const wheel = (e: WheelEvent) => {
      if (
        !e.deltaY ||
        Math.abs(e.deltaX) > Math.abs(e.deltaY) ||
        node.scrollWidth <= node.clientWidth
      )
        return
      e.preventDefault()
      followEnd.current = false
      node.scrollLeft += e.deltaY
    }
    const observer = new ResizeObserver(measure)
    observer.observe(node)
    for (const child of node.children) observer.observe(child)
    node.addEventListener("scroll", measure)
    node.addEventListener("wheel", wheel, { passive: false })
    measure()
    return () => {
      observer.disconnect()
      node.removeEventListener("scroll", measure)
      node.removeEventListener("wheel", wheel)
    }
  }, [count, lastKey, editable])
  const mask = overflow
    ? `linear-gradient(to right, ${overflow === "left" || overflow === "both" ? "transparent,black 16px" : "black"}, ${overflow === "right" || overflow === "both" ? "black calc(100% - 16px), transparent" : "black"})`
    : undefined
  return (
    <div
      ref={ref}
      data-attachment-strip=""
      data-overflow={overflow}
      style={{ maskImage: mask }}
      className={cn(
        "flex max-w-full min-w-0 [scrollbar-width:none] items-center overflow-x-auto [&::-webkit-scrollbar]:hidden",
        onRemove && "mx-3"
      )}
    >
      <AnimatePresence initial={false}>
        {files.map((item, index) => {
          const status = attachmentStatus.get(item)
          const name = item.file?.name || item.image?.alt || t("composer.image")
          const size = item.file?.size ?? 0
          const Icon = fileIcon(item.file?.mime ?? "")
          const card = (
            <span
              className={cn(
                "flex h-10 max-w-50 items-center gap-2 rounded-sm bg-muted px-2",
                !editable && "bg-background",
                status?.error && "border border-destructive color-destructive"
              )}
            >
              {status?.loading ? (
                <LatticeLoader />
              ) : (
                <Icon className="size-4 shrink-0" />
              )}
              <span className="min-w-0">
                <span className="block truncate text-caption">{name}</span>
                <span
                  className={cn(
                    "block truncate text-caption",
                    !status?.error && "color-muted"
                  )}
                >
                  {status?.error || formatByteSize(size, i18n.language)}
                </span>
              </span>
            </span>
          )
          return (
            <motion.div
              key={status?.id ?? item.file?.path ?? index}
              initial={{ width: 0, opacity: 0 }}
              animate={{ width: "auto", opacity: 1 }}
              exit={{ width: 0, opacity: 0 }}
              transition={{ duration: 0.15, ease: "easeOut" }}
              className="shrink-0 overflow-hidden"
              onAnimationComplete={() => {
                if (followEnd.current && ref.current && index === count - 1) {
                  ref.current.scrollLeft = ref.current.scrollWidth
                  followEnd.current = false
                }
              }}
            >
              <div
                className={cn("group/attachment relative", index > 0 && "ml-2")}
              >
                {item.image ? (
                  <ImagePreview
                    src={item.image.data_url}
                    alt={name}
                    className="size-10 object-cover"
                  />
                ) : (
                  <Tooltip disabled={!status?.error && !item.file?.path}>
                    <TooltipTrigger render={<span className="block" />}>
                      {card}
                    </TooltipTrigger>
                    <TooltipContent className="text-caption wrap-anywhere">
                      {status?.error || item.file?.path}
                    </TooltipContent>
                  </Tooltip>
                )}
                {onRemove && (
                  <Button
                    type="button"
                    variant="secondary"
                    size="icon-xs"
                    className="absolute top-0 right-0 size-4 p-0 opacity-0 group-hover/attachment:opacity-100 focus-visible:opacity-100"
                    aria-label={
                      item.image
                        ? t("composer.removeImage", { name })
                        : t("externalSession.remove", { name })
                    }
                    onClick={() => onRemove(item)}
                  >
                    <XIcon />
                  </Button>
                )}
              </div>
            </motion.div>
          )
        })}
      </AnimatePresence>
    </div>
  )
}
