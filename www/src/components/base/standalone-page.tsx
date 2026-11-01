import { useEffect, type ReactNode } from "react"
import { LanguageSwitcher } from "@/components/base/language-switcher"
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"

// 没有左栏的居中卡片页，用于进入页面前的 token 输入、连接中、版本不一致提示和页面出错
export function StandalonePage({
  title,
  description,
  children,
}: {
  title: string
  description?: string
  children?: ReactNode
}) {
  useEffect(() => {
    document.title = title + " OpenVaala"
  }, [title])
  return (
    <main className="grid min-h-svh place-items-center p-4">
      <Card className="w-full max-w-96">
        <CardHeader>
          <div className="flex items-start justify-between gap-2">
            <CardTitle>
              <h1 className="text-title">{title}</h1>
            </CardTitle>
            <LanguageSwitcher />
          </div>
          {description && (
            <CardDescription className="text-caption wrap-anywhere">
              {description}
            </CardDescription>
          )}
        </CardHeader>
        {children && (
          <CardContent className="text-body">{children}</CardContent>
        )}
      </Card>
    </main>
  )
}
