import { PlusIcon, XIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"

type KeyValue = { name: string; value: string }

// KeyValueRows 绘制可增删的键值输入行。
export function KeyValueRows({
  rows,
  onChange,
  nameLabel,
  valueLabel,
  addLabel,
  removeLabel,
}: {
  rows: KeyValue[]
  onChange(rows: KeyValue[]): void
  nameLabel: string
  valueLabel: string
  addLabel: string
  removeLabel: string
}) {
  return (
    <>
      {rows.map((row, index) => (
        <div
          key={index}
          className="grid min-w-0 grid-cols-[minmax(0,1fr)_auto] items-center gap-2 sm:grid-cols-[minmax(0,1fr)_minmax(0,2fr)_auto]"
        >
          {(["name", "value"] as const).map((key) => (
            <Input
              key={key}
              aria-label={key === "name" ? nameLabel : valueLabel}
              className="text-ui"
              value={row[key]}
              autoComplete="off"
              spellCheck={false}
              onChange={(event) =>
                onChange(
                  rows.map((entry, i) =>
                    i === index
                      ? { ...entry, [key]: event.target.value }
                      : entry
                  )
                )
              }
            />
          ))}
          <Tooltip>
            <TooltipTrigger
              render={
                <Button
                  className="col-start-2 row-span-2 row-start-1 text-ui sm:col-start-3 sm:row-span-1"
                  type="button"
                  variant="ghost"
                  size="icon-sm"
                  aria-label={removeLabel}
                  onClick={() => onChange(rows.filter((_, i) => i !== index))}
                />
              }
            >
              <XIcon />
            </TooltipTrigger>
            <TooltipContent className="text-caption">
              {removeLabel}
            </TooltipContent>
          </Tooltip>
        </div>
      ))}
      <Button
        type="button"
        variant="outline"
        className="self-start text-ui"
        onClick={() => onChange([...rows, { name: "", value: "" }])}
      >
        <PlusIcon data-icon="inline-start" />
        {addLabel}
      </Button>
    </>
  )
}
