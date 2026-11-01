import { useIsMobile } from "@/hooks/use-mobile"
import {
  Tooltip,
  TooltipTrigger,
  TooltipContent,
} from "@/components/ui/tooltip"
import { useState } from "react"
import { XIcon } from "lucide-react"
import { useTranslation } from "react-i18next"
import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog"
import { cn } from "@/lib/utils"

// 图片缩略图与预览：打开时适应窗口，可切换原尺寸双向滚动，关闭后回到触发处。
export function ImagePreview({
  src,
  alt = "",
  className,
}: {
  src: string
  alt?: string
  className?: string
}) {
  const { t } = useTranslation()
  const isMobile = useIsMobile()
  const [isOpen, setIsOpen] = useState(false)
  const [isActualSize, setIsActualSize] = useState(false)
  return (
    <Dialog
      open={isOpen}
      onOpenChange={(open) => {
        setIsOpen(open)
        if (open) setIsActualSize(false)
      }}
    >
      <PreviewImage
        isThumbnail
        src={src}
        alt={alt}
        className={cn("max-h-40 max-w-full border object-contain", className)}
      />
      <DialogContent
        showCloseButton={false}
        className="flex h-dvh max-h-dvh w-screen max-w-none flex-col gap-0 p-0 text-body sm:h-[90dvh] sm:max-w-[90vw]"
      >
        <div className="flex shrink-0 items-center gap-2 border-b px-3 py-2">
          <DialogTitle className="mr-auto min-w-0 truncate text-section">
            {t("imagePreview.title")}
          </DialogTitle>
          <Button
            className="text-ui"
            variant="outline"
            aria-pressed={!isActualSize}
            onClick={() => setIsActualSize(false)}
          >
            {t("imagePreview.fit")}
          </Button>
          <Button
            className="text-ui"
            variant="outline"
            aria-pressed={isActualSize}
            onClick={() => setIsActualSize(true)}
          >
            {t("imagePreview.actualSize")}
          </Button>
          <Tooltip disabled={isMobile}>
            <TooltipTrigger
              render={
                <DialogClose
                  render={
                    <Button
                      variant="ghost"
                      size="icon"
                      aria-label={t("imagePreview.close")}
                    />
                  }
                />
              }
            >
              <XIcon />
            </TooltipTrigger>
            <TooltipContent className="text-caption">
              {t("imagePreview.close")}
            </TooltipContent>
          </Tooltip>
        </div>
        <div data-image-scroll className="min-h-0 flex-1 overflow-auto">
          <div
            className={cn(
              "flex min-h-full min-w-full items-center justify-center p-4",
              isActualSize ? "w-max" : "h-full"
            )}
          >
            <PreviewImage
              src={src}
              alt={alt}
              className={
                isActualSize
                  ? "max-w-none shrink-0"
                  : "max-h-full max-w-full object-contain"
              }
            />
          </div>
        </div>
      </DialogContent>
    </Dialog>
  )
}

// 加载失败只替换这张图片；重试时重新挂载图片节点。
function PreviewImage({
  src,
  alt,
  className,
  isThumbnail = false,
}: {
  src: string
  alt: string
  className: string
  isThumbnail?: boolean
}) {
  const { t } = useTranslation()
  const [failedSource, setFailedSource] = useState<string>()
  if (failedSource === src)
    return (
      <span
        role="alert"
        className="flex flex-col items-center gap-2 p-4 text-caption color-destructive"
      >
        {t("imagePreview.failed")}
        <Button
          variant="outline"
          onClick={(event) => {
            event.stopPropagation()
            setFailedSource(undefined)
          }}
        >
          {t("common.actions.retry")}
        </Button>
      </span>
    )
  const image = (
    <img
      src={src}
      alt={alt}
      className={className}
      onError={() => setFailedSource(src)}
    />
  )
  return isThumbnail ? (
    <DialogTrigger
      render={
        <Button
          variant="ghost"
          className="h-auto max-w-full p-0"
          aria-label={t("imagePreview.open", { name: alt })}
        />
      }
    >
      {image}
    </DialogTrigger>
  ) : (
    image
  )
}
