import { OverflowTooltip } from "@/components/base/overflow-tooltip"
import {
  Fragment,
  Suspense,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
} from "react"
import { Link, Outlet, useLocation, useMatch } from "react-router"
import { useTranslation } from "react-i18next"
import { useQuery } from "@tanstack/react-query"
import { useStore } from "@nanostores/react"
import { AnimatePresence, LayoutGroup, motion } from "motion/react"
import { PlusIcon, SettingsIcon } from "lucide-react"
import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbLink,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from "@/components/ui/breadcrumb"
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarHeader,
  SidebarInset,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarProvider,
  SidebarRail,
  SidebarTrigger,
  useSidebar,
} from "@/components/ui/sidebar"
import { Skeleton } from "@/components/ui/skeleton"
import {
  Tooltip,
  TooltipTrigger,
  TooltipContent,
} from "@/components/ui/tooltip"
import {
  PageActionsTargetContext,
  type PageNavigation,
} from "@/components/base/page-layout"
import { ConversationList } from "@/components/conversation-list/conversation-list"
import { useServerConnection } from "@/components/server-connection-provider"
import { ServerConnectionControl } from "@/components/server-connection-control"
import type { ConversationID } from "@/lib/api/protocol.gen"
import { $isSidebarOpen } from "@/lib/appearance/preferences"
import {
  getConversationListSearch,
  getServerPath,
  getSettingsPath,
} from "@/lib/paths"
import { conversationOptions } from "@/lib/query/conversation"
import { cn } from "@/lib/utils"
import {
  getWorkLocationSettings,
  type NewConversationLocationState,
} from "@/lib/work-location"

// 左栏加页面区；页面通过 outlet context 登记标题和面包屑父级，顶栏和文档标题跟着显示；
// 自己排版的整页（对话页）用 PageActions 把控件放进顶栏右侧
export function AppLayout() {
  const { t } = useTranslation()
  const location = useLocation()
  const conversationRoute = useMatch("/s/:serverID/c/:conversationID/*")
  const isSidebarOpen = useStore($isSidebarOpen)
  const viewportRef = useRef<HTMLDivElement>(null)
  const [navigation, setNavigation] = useState<PageNavigation | null>(null)
  const [pageActionsTarget, setPageActionsTarget] =
    useState<HTMLDivElement | null>(null)
  const page = navigation?.locationKey === location.key ? navigation : undefined
  const title = page?.title ?? t("common.loading")
  const parent = page?.parent
  const parents = page?.parents ?? (parent ? [parent] : [])
  useEffect(() => {
    document.title = title + " OpenVaala"
  }, [title])
  useLayoutEffect(() => {
    // 1. 页面框跟随软键盘和浏览器栏留下的可见区域，手动缩放时保留布局。
    const viewport = window.visualViewport
    if (!viewport) return
    const container = viewportRef.current!
    const updateViewport = () => {
      if (viewport.scale !== 1) return
      container.style.height = `${viewport.height}px`
      container.style.top = `${viewport.offsetTop}px`
    }
    updateViewport()
    viewport.addEventListener("resize", updateViewport)
    viewport.addEventListener("scroll", updateViewport)
    // 2. 离开应用布局时停止跟随可见区域。
    return () => {
      viewport.removeEventListener("resize", updateViewport)
      viewport.removeEventListener("scroll", updateViewport)
    }
  }, [])
  return (
    <LayoutGroup id="app-navigation">
      <SidebarProvider
        ref={viewportRef}
        className="fixed inset-x-0 top-0 h-dvh min-h-0"
        open={isSidebarOpen}
        onOpenChange={(open) => $isSidebarOpen.set(open)}
      >
        <AppSidebar conversationID={conversationRoute?.params.conversationID} />
        {/* 页面区填满可见区域，只有下面的内容区滚动。 */}
        <SidebarInset className="h-full min-h-0 min-w-0">
          <header className="relative z-10 flex h-10 shrink-0 items-center gap-2 border-b bg-background px-4">
            <Tooltip>
              <TooltipTrigger
                render={
                  <SidebarTrigger
                    aria-label={t("common.actions.toggleSidebar")}
                    className="shrink-0 max-md:size-8"
                  />
                }
              />
              <TooltipContent className="text-caption">
                {t("common.actions.toggleSidebar")}
              </TooltipContent>
            </Tooltip>
            <MobileServerConnectionControl />
            <motion.div
              layout="position"
              transition={{
                duration: 0.2,
                ease: "easeInOut",
              }}
              className="min-w-0 flex-1"
            >
              <Breadcrumb
                aria-label={t("common.breadcrumb")}
                className="min-w-0 flex-1"
              >
                <BreadcrumbList className="min-w-0 flex-nowrap text-ui">
                  {parents.map((ancestor, index) => (
                    <Fragment key={`${ancestor.label}:${index}`}>
                      {/* 窄屏只留最近的父级 */}
                      <BreadcrumbItem
                        className={cn(
                          index === 0 ? "shrink-0" : "min-w-0",
                          index < parents.length - 1 && "hidden md:inline-flex"
                        )}
                      >
                        {ancestor.to ? (
                          <OverflowTooltip
                            data-slot="breadcrumb-link"
                            render={
                              <BreadcrumbLink
                                className="max-w-48 truncate text-link color-muted"
                                render={<Link to={ancestor.to} />}
                              />
                            }
                          >
                            {ancestor.label}
                          </OverflowTooltip>
                        ) : (
                          <span className="color-muted">{ancestor.label}</span>
                        )}
                      </BreadcrumbItem>
                      <BreadcrumbSeparator
                        className={cn(
                          "shrink-0 color-muted",
                          index < parents.length - 1 && "hidden md:block"
                        )}
                      />
                    </Fragment>
                  ))}
                  <BreadcrumbItem className="min-w-0">
                    {page?.onTitleClick ? (
                      // 能改名的标题是按钮，点开改名弹窗
                      <OverflowTooltip
                        render={
                          <button
                            type="button"
                            aria-current="page"
                            aria-haspopup="dialog"
                            aria-description={t("common.actions.rename")}
                            className="min-w-0 truncate text-foreground outline-none hover:bg-muted focus-visible:ring-1 focus-visible:ring-ring"
                            onClick={page.onTitleClick}
                          />
                        }
                      >
                        {title}
                      </OverflowTooltip>
                    ) : (
                      <OverflowTooltip
                        data-slot="breadcrumb-page"
                        render={<BreadcrumbPage className="truncate" />}
                      >
                        {title}
                      </OverflowTooltip>
                    )}
                  </BreadcrumbItem>
                </BreadcrumbList>
              </Breadcrumb>
            </motion.div>
            <motion.div
              layout="position"
              transition={{
                duration: 0.2,
                ease: "easeInOut",
              }}
              ref={setPageActionsTarget}
              className="flex min-w-0 items-center gap-2 empty:hidden"
            />
          </header>
          {/* relative 让页面里 sr-only 这类绝对定位的文字随页面区滚动，不撑出整页滚动 */}
          <div className="relative flex min-h-0 min-w-0 flex-1 flex-col gap-4 overflow-y-auto">
            {/* 对话之间复用公共布局；其他页面首次加载时立即占位，不留旧页面操作。 */}
            <Suspense
              key={conversationRoute ? "conversation" : location.pathname}
              fallback={<Skeleton className="m-4 h-64" />}
            >
              <PageActionsTargetContext.Provider value={pageActionsTarget}>
                <Outlet context={setNavigation} />
              </PageActionsTargetContext.Provider>
            </Suspense>
          </div>
        </SidebarInset>
      </SidebarProvider>
    </LayoutGroup>
  )
}

// 手机抽屉关闭后，顶栏仍能查看连接并手动重连。
function MobileServerConnectionControl() {
  const { isMobile, openMobile } = useSidebar()
  return (
    <AnimatePresence initial={false} mode="popLayout">
      {isMobile && !openMobile && (
        <ServerConnectionControl
          key="header-server"
          layoutId="server-connection"
        />
      )}
    </AnimatePresence>
  )
}

// 左栏显示 serve 连接状态、新对话入口、未归档对话和设置入口；
// 正在看对话时，新对话入口带上这个对话的工作位置作为新对话页的预填。
function AppSidebar({ conversationID }: { conversationID?: ConversationID }) {
  const { t } = useTranslation()
  const { setOpenMobile, isMobile, openMobile } = useSidebar()
  const connection = useServerConnection()
  const { pathname, search } = useLocation()
  const viewedConversation = useQuery({
    ...conversationOptions(connection, conversationID ?? ""),
    enabled: !!conversationID,
  })
  const newConversationState: NewConversationLocationState | undefined =
    viewedConversation.data
      ? {
          preset: getWorkLocationSettings(viewedConversation.data.conversation),
        }
      : undefined
  const serveName = window.location.host
  // 收起的桌面左栏靠 tooltip 显示名字；手机抽屉里不包 tooltip，
  // 否则抽屉打开时获得焦点的按钮带着隐藏的 tooltip，第一次 Esc 只关掉它
  const getTooltip = (label: string) =>
    isMobile ? undefined : { children: label, className: "text-caption" }
  return (
    <Sidebar collapsible="icon">
      <SidebarHeader
        data-server-connection-header=""
        className="h-10 justify-center px-2 py-0"
      >
        <div className="flex min-w-0 items-center">
          <div className="relative size-8 shrink-0">
            <AnimatePresence initial={false} mode="popLayout">
              {(!isMobile || openMobile) && (
                <ServerConnectionControl
                  key="sidebar-server"
                  layoutId={isMobile ? "server-connection" : undefined}
                />
              )}
            </AnimatePresence>
          </div>
          <SidebarMenuButton
            className="min-w-0 flex-1 pl-0 text-ui group-data-[collapsible=icon]:hidden"
            render={<Link to={getServerPath(connection.getServerID())} />}
            onClick={() => setOpenMobile(false)}
          >
            <span className="truncate">{serveName}</span>
          </SidebarMenuButton>
        </div>
      </SidebarHeader>
      <SidebarContent className="overflow-hidden">
        <SidebarGroup className="shrink-0 pt-0">
          <SidebarMenu>
            <SidebarMenuItem>
              <SidebarMenuButton
                className="text-ui"
                render={
                  <Link
                    to={{
                      pathname: getServerPath(connection.getServerID()),
                      search: getConversationListSearch(search),
                    }}
                    state={newConversationState}
                  />
                }
                tooltip={getTooltip(t("conversationList.newConversation"))}
                onClick={() => setOpenMobile(false)}
              >
                <PlusIcon />
                <span>{t("conversationList.newConversation")}</span>
              </SidebarMenuButton>
            </SidebarMenuItem>
          </SidebarMenu>
        </SidebarGroup>
        <ConversationList />
      </SidebarContent>
      <SidebarFooter>
        <SidebarMenu>
          <SidebarMenuItem>
            <SidebarMenuButton
              className="text-ui"
              render={
                <Link
                  to={{
                    pathname: getSettingsPath(
                      connection.getServerID(),
                      "general"
                    ),
                    search: getConversationListSearch(search),
                  }}
                />
              }
              isActive={pathname.startsWith(
                getServerPath(connection.getServerID()) + "/settings/"
              )}
              tooltip={getTooltip(t("settings.title"))}
              onClick={() => setOpenMobile(false)}
            >
              <SettingsIcon />
              <span>{t("settings.title")}</span>
            </SidebarMenuButton>
          </SidebarMenuItem>
        </SidebarMenu>
      </SidebarFooter>
      <Tooltip>
        <TooltipTrigger
          render={
            <SidebarRail
              aria-label={t("common.actions.toggleSidebar")}
              title={undefined}
            />
          }
        />
        <TooltipContent className="text-caption">
          {t("common.actions.toggleSidebar")}
        </TooltipContent>
      </Tooltip>
    </Sidebar>
  )
}
