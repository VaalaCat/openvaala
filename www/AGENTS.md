# 前端开发规范

适用于 `www` 下的前端工作。仓库根目录的 `AGENTS.md` 同样适用；这里只写前端自己的约定。

## 目录与生成文件

- `src/components/ui/` 只放 `pnpm dlx shadcn@<版本> add` 生成的文件，不手改。升级用 `pnpm dlx shadcn@latest add <名字> --overwrite`，再看 `git diff`。样式只通过 `index.css`、`className`、props 调整，组合出来的组件放 `components/base`。
- `src/lib/api/protocol.gen.ts` 由 `pnpm generate:types`（`internal/protocolgen`）生成，不手改；serve 的协议类型变了就重新生成。
- `components/base` 不引用 `lib/api`、`lib/query`，不写业务文案：文案用 `common.*` 或组件自己的命名空间，业务含义由调用方传进来。
- 新路由在 `router.tsx` 里用 `lazyPage` 按需加载页面模块。

## 分页

- 四个分页接口（`conversation/list`、`session/list`、`session/turns/list`、`session/items/list`）一律 `useInfiniteQuery` 按 `next_cursor` 翻页。
- 列表滚到底、消息区滚到顶时自动加载下一页，不做页码。
- 搜索词、归档、排序放进 query key。
- queryFn 里不循环拉完所有页，只有 `ConversationSync` 断线补齐例外。

## 共用缓存

- 同一种数据只有一个 `xxxOptions` 工厂，key 以 `serverID` 开头。
- 推送触发的失效只写在 `lib/query/invalidation.ts`。

## 共用组件

- shadcn 有的组件一律 `shadcn add`，不自己写同类组件，也不给每个官方组件加包装层。
- 从 frp-panel `arch-refactor` 拷来的基础组件和 lib（`components/base`、`lib/clipboard.ts`、`lib/draft-baseline.ts` 等）已归本项目维护，按本项目需要直接改。
- 同一种展示只做一个组件，第二处要用时先提取，不复制。
- 消息显示由 `itemRenderers`、`toolRenderers` 两张表决定：新增 Item 类型或工具只在 `items/`、`tools/` 加一个文件，并在表里登记一行。
- 加载转圈统一改用 `base/lattice-loader.tsx`；思考／过程聚合仅保留已有文字 shimmer，不叠加格点，不引入计时器或成功标记。
- 服务器与对话共用 `base/status-dot.tsx`；运行波纹用 `motion/react`，服务器手动重连期间原位旋转 Refresh。图标变形仍用 Morphicons。
- 不提供「减少动画」功能，也不按系统偏好关闭动画；保留流式追加、历史恢复等业务场景原有的即时更新。

## 边距和换行

- 控件 32px 高，尺寸和间距用 4 的倍数；1px 边框照常占位，不用 `calc`、负外边距去补齐。
- 间距按关系选值：紧凑控件和列表行内边距 8，大容器（输入区、代码块、气泡、面板）内边距 12；有边框的相邻控件之间 8；组与组、段落之间 16；页面边距桌面 24、手机 12。
- 输入框底栏这类无边框工具栏按钮紧挨排列：图标按钮 28px 宽，文字按钮左右内边距 4，实心按钮与旁边留 4。
- 要对齐的区域放进同一个容器共享宽度，例如输入区和消息共用消息列，不按滚动条宽度单独补偿。
- 文字只用文字角色类（见下节）。
- Markdown 正文 `overflow-wrap: anywhere`；代码、工具输出、JSON 不折行，横向滚动。
- 横向滚动只留在代码、JSON、图片条等内容块内，不带动页面和消息列；复制提示等绝对定位的隐藏内容由控件自身承接定位。
- ID、路径、模型名单行省略，真被截断时才用 `OverflowTooltip` 显示完整文本；工具标题后的参数摘要是例外：始终单行省略、不加 Tooltip／原生 title，完整参数在展开详情阅读。
- 过程标题的类型／展开图标共用 16px 位置，用 Morphicons 原位变形；异常状态独立，不显示时不占位，摘要直接接上。触屏不做特化：收起默认业务图标，hover／键盘聚焦时右箭头，展开后下箭头；调试／链接／状态说明不嵌进展开按钮。
- 工具结果图片在收起的过程聚合下方横向排列；展开后归入各自条目。复用 `ImagePreviewStrip`，缩略图最高 160px，不换行堆高；实际读到图片时在 read_file 文件名前显示图片图标。
- 模型回复复制按 Turn 选择最后一条有文字且已保存的回复；运行中的轮次隐藏本轮入口，不移除旧轮复制。用户消息的复制独立保留。

## 文字角色

字号、行高、字体和字重集中在 `src/index.css` 维护。页面和组件按用途选文字角色，不写 `text-xs`、`text-sm`、`text-[…]`、`fontSize`、`font-family`，也不用零散的 `leading-*` 调排版。
可调阅读和终端数值的默认值、范围由 `lib/appearance/preferences.ts` 维护；CSS 角色读取变量，xterm 使用同一份终端偏好。

- `text-title`：页面主标题，16px / 24px，标题字体、中等字重。
- `text-section`：内部章节、卡片、弹窗、空状态标题，14px / 24px，标题字体、中等字重。
- `text-body`：段落正文，14px / 24px，正文字体。
- `text-ui`：导航、按钮、字段标签、输入值，14px / 20px，正文字体。
- `text-table`：列表和详情字段的值、名称、地址，14px / 20px，正文字体。
- `text-caption`：时间、辅助说明、校验说明、浮层提示、一般辅助状态，12px / 16px，正文字体。
- `text-token`：紧凑 token 用量，12px / 20px，正文字体、等宽数字。
- `text-code`：文件名、路径摘要、快捷键等固定界面文字，12px / 20px，等宽字体。
- `text-reading`：用户消息、模型回复和展开的正文内容，默认 14px / 24px，跟随「外观 → 正文」。
- `text-code-reading`：代码、工具原始输出和 JSON 阅读内容，默认 12px / 20px，跟随「外观 → 代码」；代码块内边距用 `code-padding`。文件名摘要、快捷键等固定界面文字仍用 `text-code`。

- 移动端保持小字：输入框、文本域、按钮和选项与桌面统一 14px，不按屏宽单独放大；辅助文字、token 和代码仍用各自角色。
- iOS 在入口设置 `maximum-scale=1` 抑制输入聚焦时的自动缩放；不全局设置 `user-scalable=no`，不限制 Android 和桌面的手动缩放。
- 外观调整即时生效并在当前浏览器保存；正文、代码与终端分别调整，不改变普通控件字号。终端默认 13px / 1.1 倍行高，修改后更新 xterm 并重新计算行列，不重启 shell。
- 官方组件也要显式接入文字角色：`CardTitle`、`DialogTitle`、`AlertDialogTitle`、`AlertTitle`、`EmptyTitle` 用 `text-section`；`DialogContent` 和段落正文容器用 `text-body`；`FieldLabel` 用 `text-ui`；各类 `Description`、`FieldError` 和 `TooltipContent` 用 `text-caption`。工具动作与主要状态用 `text-ui`，不缩成辅助字号。
- token 展示只用 `components/base/token-number-value.tsx` 的 `TokenNumberValue`；非 token 数量、数字输入框、正文、输出和原始数据不因此格式化。按钮及选项内用 `interactive={false}`，完整值在对应详情提供。

- 页面主标题（`h1`）用 `text-title`，内部章节标题（`h2`、`h3`）用 `text-section`。
- 通过公开的 `className` 接入。`index.css` 的 `[data-slot].text-*` 规则保证项目角色优先于组件默认文字样式；不要堆具体字号或 `!important` 补偿。
- 输入框、文本域、选择框及输入附属内容用常规字重 400；标题和操作按钮沿用对应角色的字重。
- 指标字形（上下文环、用量柱）用 `.font-datatype`，只在 `components/base/data-glyph.tsx` 里画。

## 链接与 UI 组件

- 文字跳转用 `text-link`（1px 实线下划线、2px 偏移）；术语解释用 `text-help`（1px 点线下划线）。悬停只变颜色。
- 链接悬停和焦点颜色适配实际背景；反转色容器用 `--link-interaction-color: currentColor` 保持对比。
- 下划线必须对应真实可用的跳转或解释；不能点的名称不保留链接外观。
- 悬停提示必须提供新信息：状态说明、错误原因、缩略值的完整内容可以保留；已完整显示的内容不重复弹出。
- 简短悬停说明使用官方 `Tooltip`，保留无障碍名称，不再叠加原生 `title`；需要点击、触摸或阅读详情的内容保留 `Popover`。
- 鼠标提示停留 600ms，不共享跨目标立即打开；点击取消待弹提示。时间提示仅文字触发，侧栏「⋯」不加 Tooltip，菜单打开时不叠加入口提示；键盘仍可获取必要说明。
- Item／Turn 调试入口统一用「调试信息」说明；不据此改写详情中的原始数据标题。
- 链接类继承所在容器的文字角色。
- 文字颜色用项目 `color-*` 语义类（`color-muted`、`color-destructive`、`color-success`、`color-warning` 等），交互、背景和边框用官方组件 variants。
- 普通文字分正文／次要 `color-subtle`／辅助 `color-muted`；过程收起时标题次要、参数辅助，hover／focus-visible／展开时标题正文、参数次要。不对整行设置透明度，不覆盖异常语义色。
- 类名合并统一用项目的 `cn`（`lib/utils.ts`），官方组件的 `import { cn } from "cn"` 由 Vite 精确别名接到这里。新增语义类时检查 `cn` 的分组，避免 `text-ui` 这类角色被误当成颜色删掉。

## 控件与布局

- Avoid unnecessary secondary labels and descriptions. Let the layout do the explaining.
  布局、控件状态、占位符或相邻元素已经说明的，不再写副标题、说明段落或重复标签；禁用原因、校验错误和布局表达不了的去向提示保留。
- 常规输入框、选择框、筛选框和操作按钮统一 32px 高，用官方 `Button` 默认尺寸；同一操作区不混用 `sm`、`xs`。行内和输入附属按钮按紧凑场景用对应尺寸。
- 按钮宽度按文字和图标自然展开；可用、禁用和加载状态尺寸不变。
- 页级草稿的「撤销 → 应用」放在 `PageLayout` 页头右侧、其他操作之前；子编辑区用 `PageActions` 把按钮放进页头。
- 页签用 `TabsList` 默认样式；会横向溢出的页签栏用 `ScrollableTabsList`。
- 创建、新增用左侧 `PlusIcon`，刷新用左侧 `RefreshCwIcon`，图标标 `data-icon="inline-start"`。
- 列表搜索用 `ListSearch`；空状态用 `ListEmpty`：没有数据时，下一步操作不在眼前才写出下一步；有筛选但没结果时显示「没有匹配的 X」和「清除筛选」。
- 列表筛选条件由框内文字表达，不额外加重复标题，保留 `aria-label`。
- 多字段表单统一字段起点和控件边缘，横向排列时顶部对齐。
- 焦点描边、错误边框和输入附属按钮的焦点状态完整可见，不被相邻边框或 `overflow` 裁切。
- 禁用的图标按钮需要说明原因时用 Base UI `focusableWhenDisabled` 保留悬停和键盘提示，不改官方组件。
- 模型选择的弹层形态按视口决定：手机用 Drawer，桌面用锚定浮层；容器宽度只决定入口合并、文字省略和排布，桌面窄 Tab 不切手机抽屉。
- JSON 用一个按钮切换「全部展开／全部折叠」，按实际节点状态显示下一步动作；字符串的解析与全文展开仍各自独立，不改变复制的原始值。调试记录使用右栏剩余高度、内部滚动；消息里的代码和工具输出保留默认限高。

## 操作反馈

操作结果按下面 7 条决定提示位置：

1. 列表行菜单操作的成功或失败 → toast；待处理消息操作成功由队列或输入框变化反馈，不重复弹成功 toast。
2. 保存或删除成功后跳转了页面 → toast。
3. 请求返回时编辑器已经卸载（用户已离开）→ `notifyFailure`。
4. 字段校验 → 显示在字段旁边。
5. 确认弹窗内的提交失败 → 弹窗内的 `RequestError`，弹窗不关闭。
6. 页头「应用」失败，且编辑器仍在 → 表单顶部的 `RequestError`。
7. 页面或区块的数据加载失败 → `RequestError` 加「重试」；纯连接错误集中在服务器入口，保留已有数据，无数据时局部占位。业务错误和未确认的写操作结果不隐藏。

- toast 只能通过 `lib/notify.ts` 发出，页面和组件不直接 import `sonner`。
- 对话内容和 serve 返回的错误原文不翻译；错误标题翻译，原文放进可展开的详情。

## 操作确认

- 删除数据的操作要确认；可以恢复的操作（归档、取消归档、改名）不确认。
- 确认弹窗用 `ConfigurationChangeDialog`，不在业务组件里直接写 `AlertDialog`。同一种资源的删除弹窗只有一个组件，行菜单和设置页复用它。
- 对单个资源的操作，弹窗标题写出资源名称：「动作 + 资源类型 + 「名称」？」，如「删除对话「重构登录」？」，文案带 `{{name}}`。
- 行菜单：资源自己的操作在前；最后一组只有「删除」，用 `variant="destructive"`，前面加分隔线；分组为空时不画分隔线。
- 设置页的最后一节是 `DangerZone`：只放按钮，按钮用 outline 加红色文字，后果只在确认弹窗里写一遍；有未保存的修改时禁用并用 `disabledReason` 说明原因。

## 面包屑

- 页面用 `PageLayout` 的 `parent` 提供父级，面包屑就是返回入口，页内不再加返回链接。`AppLayout` 只显示页面给的父级，不按 URL 前缀推断业务名称。
- 没有父级页面的分组只用 `parent` 显示文字，不生成链接。
- 面包屑用 `text-ui`，可跳转父级用 `text-link`；单行显示，长名称省略，实际截断时用 `OverflowTooltip` 显示完整文本。顶栏保持 40px，窄屏保留父级入口。
- 页面标题由 `PageLayout` 登记，页面不自己写 `document.title`；页面模块加载期间页头显示「加载中」。

## 验证

- 前端命令在 `www` 目录执行；Node ≥ 24，pnpm 用 `package.json` 的 `packageManager` 版本。
- 完成前运行 `pnpm typecheck`、`pnpm lint`、`pnpm test`、`pnpm check:i18n` 和 `pnpm build`；有失败时说明实际结果。端到端测试用 `pnpm test:e2e`。
- 行为测试用 `node --import tsx --test`，放在被测文件旁边的 `<名字>.test.ts`，table-driven；只测解析、状态规则、订阅生命周期这类有意义的行为，不写源码、AST、类名或组件存在性测试。
- UI 变更用 `agent-browser` 验证真实页面、交互和计算后的布局：检查计算后的字号、行高、高度、内边距、文字起点和图标位置，覆盖可用、禁用、加载、选中和展开状态，以及浅色、深色和窄屏（390px）。
- 检查浮层叠加的实际场景，例如错误提示与下拉菜单同时打开；修复共用样式时同时检查用到它的其他位置。

## i18n

- 所有用户可见文案走 i18next，`zh-CN` 和 `en` 两份都写；`resources.d.ts` 以 `zh-CN.json` 为类型来源。
- `pnpm check:i18n` 检查两份 key 齐全、插值一致，并报出没有源码引用的 key。
- 翻译 key 写完整的字符串字面量（可以放在对照表里）；必须拼接时只用 `scripts/check-i18n.mjs` 的 `dynamicKeys` 里声明的窄前缀；不要用 `+` 拼 key，也不要让 key 的前缀来自变量。
