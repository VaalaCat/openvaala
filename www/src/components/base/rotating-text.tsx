import { AnimatePresence, motion, useIsPresent } from "motion/react"
import { forwardRef, useEffect, useState, useSyncExternalStore } from "react"
import { cn } from "@/lib/utils"
import {
  reconcileRotatingCharacters,
  type RotatingCharacter,
} from "./rotating-text-characters"

type RotatingTextProps = {
  text: string
  transitionKey: string
  className?: string
  exitDelayMs?: number
}

type TextPresentation = {
  inputText: string
  key: string
  text: string
  characters: RotatingCharacter[]
  exiting: RotatingCharacter[]
  nextID: number
  enterDelays: Map<number, number>
  exitDelays: Map<number, number>
  exitText: string
  isStreamingAppend: boolean
}

const reducedMotionQuery = "(prefers-reduced-motion: reduce)"
const characterDuration = 0.2
const ease = [0.22, 1, 0.36, 1] as const

// 复用未变化的字素，只将变化文字逐字上推；空值可延迟视觉退场。
export function RotatingText({
  text,
  transitionKey,
  className,
  exitDelayMs = 0,
}: RotatingTextProps) {
  // 1. 输入立即更新，延迟期间只保留最后一段展示文字。
  const reducedMotion = useSyncExternalStore(
    subscribeReducedMotion,
    () => window.matchMedia(reducedMotionQuery).matches
  )
  const [presentation, setPresentation] = useState<TextPresentation>(() => {
    const { characters, nextID } = reconcileRotatingCharacters([], text, 0)
    return {
      inputText: text,
      key: transitionKey,
      text,
      characters,
      exiting: [],
      nextID,
      enterDelays: new Map(),
      exitDelays: new Map(),
      exitText: "",
      isStreamingAppend: false,
    }
  })
  if (
    presentation.inputText !== text ||
    presentation.key !== transitionKey ||
    (!text && presentation.text && exitDelayMs <= 0)
  ) {
    setPresentation(
      !text && exitDelayMs > 0
        ? { ...presentation, inputText: text, key: transitionKey }
        : updatePresentation(presentation, text, transitionKey)
    )
  }
  useEffect(() => {
    if (text || !presentation.text || exitDelayMs <= 0) return
    const timer = window.setTimeout(() => {
      setPresentation((latest) =>
        latest.inputText === "" && latest.text
          ? updatePresentation(latest, "", latest.key)
          : latest
      )
    }, exitDelayMs)
    return () => window.clearTimeout(timer)
  }, [text, exitDelayMs, presentation.text])

  // 2. 变化字同步进入和退出，共同文字只做位置移动。
  if (!presentation.text && presentation.exitDelays.size === 0) return null
  const instant = reducedMotion || presentation.isStreamingAppend
  const layoutTransition = { duration: instant ? 0 : characterDuration, ease }
  return (
    <motion.span
      data-rotating-text=""
      layout={instant ? false : "size"}
      transition={{ layout: layoutTransition }}
      className={cn(
        "relative inline-block h-lh max-w-full min-w-0 overflow-hidden align-bottom text-ui",
        className
      )}
    >
      <span data-rotating-text-label="" className="sr-only select-none">
        {text}
      </span>
      <motion.span
        data-rotating-text-current={presentation.text ? "" : undefined}
        aria-hidden="true"
        layout={instant ? false : "position"}
        transition={{ layout: layoutTransition }}
        className={cn(
          "relative block h-full min-w-0 overflow-hidden text-start text-ellipsis whitespace-pre",
          !text && "pointer-events-none select-none"
        )}
      >
        {!presentation.text && (
          <span className="invisible select-none">{presentation.exitText}</span>
        )}
        {[...presentation.characters, ...presentation.exiting].map(
          (character) => (
            <AnimatePresence
              key={character.id}
              mode="popLayout"
              custom={{ delays: presentation.exitDelays, reducedMotion }}
              onExitComplete={() => {
                setPresentation((latest) => {
                  if (!latest.exitDelays.has(character.id)) return latest
                  const exitDelays = new Map(latest.exitDelays)
                  exitDelays.delete(character.id)
                  return {
                    ...latest,
                    exiting: latest.exiting.filter(
                      ({ id }) => id !== character.id
                    ),
                    exitDelays,
                    exitText: exitDelays.size === 0 ? "" : latest.exitText,
                  }
                })
              }}
            >
              {!presentation.exitDelays.has(character.id) && (
                <RotatingLetter
                  key={character.id}
                  character={character}
                  enterDelay={presentation.enterDelays.get(character.id)}
                  instant={instant}
                />
              )}
            </AnimatePresence>
          )
        )}
      </motion.span>
    </motion.span>
  )
}

// 字素在原位退出，保留文字节点由 Motion 继续调整位置。
const RotatingLetter = forwardRef<
  HTMLSpanElement,
  { character: RotatingCharacter; enterDelay?: number; instant: boolean }
>(function RotatingLetter({ character, enterDelay, instant }, ref) {
  // 1. 退出字素从选择和交互中移除，动画结束后由 Motion 卸载。
  const isPresent = useIsPresent()
  return (
    <motion.span
      ref={ref}
      data-rotating-text-character=""
      data-rotating-text-exit={isPresent ? undefined : ""}
      aria-hidden="true"
      layout={instant ? false : "position"}
      initial={
        instant || enterDelay === undefined ? false : { y: "100%", opacity: 0 }
      }
      animate={{ y: 0, opacity: 1 }}
      exit="exit"
      variants={{
        exit: ({
          delays,
          reducedMotion,
        }: {
          delays: Map<number, number>
          reducedMotion: boolean
        }) => ({
          y: reducedMotion ? 0 : "-100%",
          opacity: 0,
          transition: {
            duration: reducedMotion ? 0 : characterDuration,
            delay: reducedMotion ? 0 : (delays.get(character.id) ?? 0),
            ease,
          },
        }),
      }}
      transition={{
        duration: instant ? 0 : characterDuration,
        delay: instant ? 0 : (enterDelay ?? 0),
        ease,
        layout: { duration: instant ? 0 : characterDuration, delay: 0, ease },
      }}
      className={cn(
        "inline-block",
        !isPresent && "pointer-events-none select-none"
      )}
    >
      {character.value}
    </motion.span>
  )
})

// 匹配展示文字并计算本次进入、退出字素的错开时间。
function updatePresentation(
  previous: TextPresentation,
  text: string,
  key: string
): TextPresentation {
  // 1. 相同标识的纯尾部追加原位补字，其余变化按字素旋转。
  const diff = reconcileRotatingCharacters(
    previous.characters,
    text,
    previous.nextID
  )
  const isStreamingAppend = previous.key === key && diff.isAppend
  const stagger = Math.min(
    0.015,
    0.12 / Math.max(1, diff.entering.length - 1, diff.exiting.length - 1)
  )
  const enterDelays = new Map<number, number>()
  if (!isStreamingAppend) {
    diff.entering.forEach(({ id }, index) =>
      enterDelays.set(id, index * stagger)
    )
  }
  const exitDelays = new Map<number, number>()
  diff.exiting.forEach(({ id }, index) => exitDelays.set(id, index * stagger))
  return {
    inputText: text,
    key,
    text,
    characters: diff.characters,
    exiting: diff.exiting,
    nextID: diff.nextID,
    enterDelays,
    exitDelays,
    exitText: text
      ? ""
      : previous.characters.map(({ value }) => value).join(""),
    isStreamingAppend,
  }
}

// 系统减少动画设置变化时更新文字动画。
function subscribeReducedMotion(onChange: () => void): () => void {
  // 1. 监听媒体查询，卸载时取消监听。
  const mediaQuery = window.matchMedia(reducedMotionQuery)
  mediaQuery.addEventListener("change", onChange)
  return () => mediaQuery.removeEventListener("change", onChange)
}
