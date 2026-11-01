package tui

import (
	"fmt"
	"image/color"
	"io"
	"os"
	"strings"

	"charm.land/glamour/v2"
	glamourstyles "charm.land/glamour/v2/styles"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extensionast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
	"golang.org/x/term"
)

// glamour 样式：auto 按终端背景选择深色或浅色，notty 不带颜色（测试与非终端输出）。
const (
	markdownStyleAuto  = "auto"
	markdownStyleNoTTY = glamourstyles.NoTTYStyle
)

// isMarkdownStyleValid 样式是否为 auto 或 glamour 的标准样式名？
func isMarkdownStyleValid(style string) bool {
	// 1. 查询标准样式表并检查 auto。
	_, found := glamourstyles.DefaultStyles[style]
	return style == markdownStyleAuto || found
}

// resolveTerminalAppearance 在界面接管输入前确定 Markdown 样式和终端背景色。
func resolveTerminalAppearance(style string, input io.Reader, output io.Writer) (markdownStyle string, background color.Color) {
	// 1. 检查输入输出是否都是终端；非终端的 auto 使用无颜色样式。
	inputFile, isInputFile := input.(*os.File)
	outputFile, isOutputFile := output.(*os.File)
	if !isInputFile || !isOutputFile || !term.IsTerminal(int(inputFile.Fd())) || !term.IsTerminal(int(outputFile.Fd())) {
		if style == markdownStyleAuto {
			return markdownStyleNoTTY, nil
		}
		return style, nil
	}
	// 2. 查询一次背景色，出错或无应答时不使用底色。
	background, err := lipgloss.BackgroundColor(inputFile, outputFile)
	if err != nil {
		background = nil
	}
	// 3. auto 按背景亮度选择深浅样式，背景未知时使用深色。
	if style == markdownStyleAuto {
		style = glamourstyles.DarkStyle
		if background != nil && !isDarkColor(background) {
			style = glamourstyles.LightStyle
		}
	}
	// 4. 颜色能力低于 256 色时只返回文字样式，否则同时返回背景。
	if colorprofile.Detect(outputFile, os.Environ()) < colorprofile.ANSI256 {
		return style, nil
	}
	return style, background
}

// ── tuiStyles 配色 ──

// tuiPalette 保存一种终端背景下的角色、状态和强调颜色。
type tuiPalette struct {
	user, agent, warning, success, failure, accent string
}

var (
	darkPalette = tuiPalette{
		user:    "#87afff",
		agent:   "#d7afff",
		warning: "#ffd75f",
		success: "#87d787",
		failure: "#ff8787",
		accent:  "#5fd7ff",
	}
	lightPalette = tuiPalette{
		user:    "#005faf",
		agent:   "#875faf",
		warning: "#875f00",
		success: "#00875f",
		failure: "#af0000",
		accent:  "#005f87",
	}
)

// newTUIStyles 按 Markdown 样式和背景色生成 TUI 配色。
func newTUIStyles(markdownStyle string, background color.Color) tuiStyles {
	// 1. 无颜色模式下返回不带修饰的样式。
	plain := lipgloss.NewStyle()
	if markdownStyle == markdownStyleNoTTY {
		return tuiStyles{
			user:        plain,
			agent:       plain,
			faint:       plain,
			warning:     plain,
			success:     plain,
			failure:     plain,
			accent:      plain,
			heading:     plain,
			hintKey:     plain,
			surface:     plain,
			selection:   plain,
			diffAdded:   plain,
			diffRemoved: plain,
		}
	}
	// 2. 按深浅样式选配色，设置文字、标题、按键和状态样式；选中项和 diff 先用文字颜色，背景未知时直接返回。
	isDark := markdownStyle != glamourstyles.LightStyle
	palette := darkPalette
	if !isDark {
		palette = lightPalette
	}
	styles := tuiStyles{
		user:    plain.Foreground(lipgloss.Color(palette.user)),
		agent:   plain.Foreground(lipgloss.Color(palette.agent)),
		faint:   plain.Faint(true),
		warning: plain.Foreground(lipgloss.Color(palette.warning)),
		success: plain.Foreground(lipgloss.Color(palette.success)),
		failure: plain.Foreground(lipgloss.Color(palette.failure)),
		accent:  plain.Foreground(lipgloss.Color(palette.accent)),
		heading: plain.Foreground(lipgloss.Color(palette.accent)).Bold(true),
		hintKey: plain.Bold(true),
		surface: plain,
	}
	styles.selection, styles.diffAdded, styles.diffRemoved = styles.accent, styles.success, styles.failure
	if background == nil {
		return styles
	}
	// 3. 背景已知时生成输入、选中项和 diff 的底色。
	surface, selection := lipgloss.Darken(background, 0.04), lipgloss.Darken(background, 0.10)
	added, removed := lipgloss.Color("#dafbe1"), lipgloss.Color("#ffebe9")
	if isDark {
		surface, selection = lipgloss.Lighten(background, 0.08), lipgloss.Lighten(background, 0.16)
		added, removed = lipgloss.Color("#213A2B"), lipgloss.Color("#4A221D")
	}
	styles.hasBackground = true
	styles.surface = plain.Background(surface)
	styles.selection = styles.accent.Background(selection)
	styles.diffAdded = styles.success.Background(added)
	styles.diffRemoved = styles.failure.Background(removed)
	return styles
}

// ── Markdown 渲染 ──

// markdownRenderer 保存终端显示宽度和正文、表格渲染器；同一实例不支持并发调用。
type markdownRenderer struct {
	width int
	// textRenderer 不折行，渲染表格之外的块。
	textRenderer *glamour.TermRenderer
	// tableRenderer 按 width 折行，glamour 依据它限制表格列宽。
	tableRenderer *glamour.TermRenderer
}

// newMarkdownRenderer 创建按给定宽度显示 Markdown 的渲染器。
func newMarkdownRenderer(style string, width int) (*markdownRenderer, error) {
	// 1. 按标准样式创建不折行的正文渲染器。
	textRenderer, err := glamour.NewTermRenderer(glamour.WithStandardStyle(style), glamour.WithWordWrap(0))
	if err != nil {
		return nil, err
	}
	// 2. 创建按显示宽度排版的表格渲染器，保存宽度和两个渲染器。
	tableRenderer, err := glamour.NewTermRenderer(glamour.WithStandardStyle(style), glamour.WithWordWrap(width))
	if err != nil {
		return nil, err
	}
	return &markdownRenderer{
		width:         width,
		textRenderer:  textRenderer,
		tableRenderer: tableRenderer,
	}, nil
}

// mustNewMarkdownRenderer 创建已校验样式的 Markdown 渲染器。
func mustNewMarkdownRenderer(style string, width int) *markdownRenderer {
	// 1. 创建渲染器；已校验的样式仍无法加载时 panic。
	renderer, err := newMarkdownRenderer(style, width)
	if err != nil {
		panic(fmt.Sprintf("markdown style %q was validated but cannot be loaded: %v", style, err))
	}
	return renderer
}

// render 把 Markdown 原文按顶层块逐个渲染，返回显示行。
func (r *markdownRenderer) render(source string) []string {
	// 1. 切分顶层块，跳过空输出并按顺序连接渲染行，块间留一个空行。
	var lines []string
	for _, block := range splitMarkdownBlocks(source) {
		blockLines := r.renderBlock(block)
		if len(blockLines) == 0 {
			continue
		}
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, blockLines...)
	}
	return lines
}

// renderBlock 渲染并折行一个 Markdown 块。
func (r *markdownRenderer) renderBlock(block markdownBlock) []string {
	// 1. 清理原文里的终端控制序列，空白块不输出。
	source := stripTerminalControls(block.source)
	if strings.TrimSpace(source) == "" {
		return nil
	}
	// 2. 表格用按宽度排版的渲染器，代码块硬折行，其余折行时保留列表和引用缩进。
	renderer, wrap := r.textRenderer, wrapKeepingIndent
	switch block.kind {
	case markdownBlockTable:
		renderer = r.tableRenderer
	case markdownBlockCode:
		wrap = wrapCodeLine
	}
	// 3. 调用渲染器，失败时使用清理后的原文。
	rendered, err := renderer.Render(source)
	if err != nil {
		rendered = source
	}
	// 4. 展开制表符、逐行折行并去掉填充和首尾空行。
	var lines []string
	for line := range strings.SplitSeq(expandTabs(rendered), "\n") {
		for _, wrapped := range wrap(line, r.width) {
			lines = append(lines, trimLinePadding(wrapped))
		}
	}
	return trimBlankEdges(lines)
}

// ── Markdown 顶层块 ──

// markdownBlockKind 是顶层块的渲染方式。
type markdownBlockKind int

const (
	// markdownBlockText 由不折行的 glamour 渲染，再经 wrapKeepingIndent 折行。
	markdownBlockText markdownBlockKind = iota
	// markdownBlockTable 由 glamour 按宽度排版。
	markdownBlockTable
	// markdownBlockCode 是代码块，渲染后经 wrapCodeLine 按宽度硬折。
	markdownBlockCode
)

// markdownBlock 是切分出的一个顶层 Markdown 块。
type markdownBlock struct {
	// source 是块的原文：从块所在行的行首到下一个块所在行的行首，包括块后的空行。
	source string
	kind   markdownBlockKind
}

// maxFenceIndent 是代码围栏行首最多的空格数（CommonMark）；更深的缩进不是围栏。
const maxFenceIndent = 3

// markdownSpaces 是 goldmark 识别的 ASCII 空白字符，不包含 NBSP 和 U+3000 等 Unicode 空格。
const markdownSpaces = " \t\n\r"

// markdownParser 使用与 glamour 相同的扩展解析顶层块。
var markdownParser = goldmark.New(goldmark.WithExtensions(extension.GFM, extension.DefinitionList)).Parser()

// splitMarkdownBlocks 按出现顺序切分顶层 Markdown 块。
func splitMarkdownBlocks(source string) []markdownBlock {
	// 1. 读取块的行首位置和类型，再截取源文本；首块包含开头空行。
	starts, kinds := getTopLevelBlockStarts(source)
	blocks := make([]markdownBlock, len(starts))
	for index := range starts {
		start, end := starts[index], len(source)
		if index == 0 {
			start = 0
		}
		if index+1 < len(starts) {
			end = starts[index+1]
		}
		blocks[index] = markdownBlock{
			source: source[start:end],
			kind:   kinds[index],
		}
	}
	return blocks
}

// getTopLevelBlockStarts 返回顶层块的行首字节位置和渲染类型。
func getTopLevelBlockStarts(source string) ([]int, []markdownBlockKind) {
	// 1. 解析 Markdown，依次读取文档直接子节点的位置和类型。
	document := markdownParser.Parse(text.NewReader([]byte(source)))
	var starts []int
	var kinds []markdownBlockKind
	for node := document.FirstChild(); node != nil; node = node.NextSibling() {
		starts = append(starts, strings.LastIndexByte(source[:getMarkdownBlockPos(node)], '\n')+1)
		kinds = append(kinds, getMarkdownBlockKind(node))
	}
	return starts, kinds
}

// getMarkdownBlockPos 返回顶层块起始位置。
func getMarkdownBlockPos(node ast.Node) int {
	// 1. 表格有有效表头位置时使用表头，否则返回节点位置。
	if node.Kind() == extensionast.KindTable {
		if header := node.FirstChild(); header != nil && header.Pos() >= 0 {
			return header.Pos()
		}
	}
	return node.Pos()
}

// getMarkdownBlockKind 返回顶层块使用的渲染方式。
func getMarkdownBlockKind(node ast.Node) markdownBlockKind {
	// 1. 识别代码和表格节点，其他节点作为正文。
	switch node.Kind() {
	case ast.KindFencedCodeBlock, ast.KindCodeBlock:
		return markdownBlockCode
	case extensionast.KindTable:
		return markdownBlockTable
	}
	return markdownBlockText
}

// ── Markdown 流式定稿 ──

// markdownStream 保存生成中回复的未定稿原文、已定稿长度和代码围栏扫描位置；最后一个顶层块留在实时区域，闭合围栏可直接定稿。
type markdownStream struct {
	// tail 是还没定稿的原文。
	tail strings.Builder
	// finalizedLength 是整条回复中已定稿的字节数，总在两个顶层块之间的行首（块后的空行可能还留在未定稿部分开头）。
	finalizedLength int
	// openFence 是未定稿部分开头那个还没闭合的代码围栏，例如 "```"；为空时开头没有未闭合的围栏。
	openFence string
	// fenceBodyStart 是围栏行之后的位置，相对未定稿部分。
	fenceBodyStart int
	// fenceScannedLength 是已确认不含闭合围栏的长度，闭合时是闭合围栏行末尾的位置；相对未定稿部分。
	fenceScannedLength int
}

// newMarkdownStream 从回复原文和已定稿长度创建流式记录。
func newMarkdownStream(source string, finalizedLength int) *markdownStream {
	// 1. 将已定稿长度限制在原文范围，保存剩余正文并检查完整行中的围栏。
	finalizedLength = min(finalizedLength, len(source))
	stream := &markdownStream{finalizedLength: finalizedLength}
	stream.tail.WriteString(source[finalizedLength:])
	stream.trackOpenFence(getCompleteLines(source[finalizedLength:]))
	return stream
}

// appendText 追加尚未定稿的回复增量。
func (s *markdownStream) appendText(text string) {
	// 1. 把新增文字写入尾部。
	s.tail.WriteString(text)
}

// getFinalizedLength 返回整条回复已经定稿的字节数。
func (s *markdownStream) getFinalizedLength() int {
	// 1. 读取累计定稿长度。
	return s.finalizedLength
}

// takeStableBlocks 取出全部可定稿源文本。
func (s *markdownStream) takeStableBlocks() string {
	// 1. 反复取出可定稿的一段并拼接，直到没有可取的内容。
	var stable strings.Builder
	for {
		block := s.takeStableBlock()
		if block == "" {
			return stable.String()
		}
		stable.WriteString(block)
	}
}

// takeStableBlock 取出尾部开头可定稿的一段并累计定稿长度；没有可定稿的内容时返回空。
func (s *markdownStream) takeStableBlock() string {
	// 1. 只看完整行：没记录围栏时重新识别开头围栏，已有围栏时继续找闭合行；围栏没闭合时不定稿，整段留在实时区域。
	tail := s.tail.String()
	complete := getCompleteLines(tail)
	closedFenceEnd := 0
	if s.openFence == "" {
		closedFenceEnd = s.trackOpenFence(complete)
	} else if s.scanFenceClosing(complete) {
		closedFenceEnd = s.fenceScannedLength
	}
	if s.openFence != "" {
		return ""
	}
	// 2. 围栏已闭合时截到闭合行末，否则截到最后一个顶层块之前；不到两个顶层块时不定稿。
	cut := closedFenceEnd
	if cut == 0 {
		starts, _ := getTopLevelBlockStarts(complete)
		if len(starts) < 2 {
			return ""
		}
		cut = starts[len(starts)-1]
	}
	// 3. 保留剩余尾部，累计定稿字节数并返回截取文本。
	s.resetTail(tail[cut:])
	s.finalizedLength += cut
	return tail[:cut]
}

// takeRest 取出全部剩余文本并结束流式定稿。
func (s *markdownStream) takeRest() string {
	// 1. 累计剩余长度，清空尾部和围栏记录后返回原文。
	rest := s.tail.String()
	s.finalizedLength += len(rest)
	s.resetTail("")
	s.openFence = ""
	return rest
}

// getLiveSource 返回实时区域要渲染的原文；未闭合的代码块只保留正文最后 maxLines 行。
func (s *markdownStream) getLiveSource(maxLines int) string {
	// 1. 普通尾部原样返回；未闭合围栏截取正文末尾并补上闭合围栏。
	tail := s.tail.String()
	if s.openFence == "" {
		return tail
	}
	body := strings.TrimSuffix(tail[s.fenceBodyStart:], "\n")
	return tail[:s.fenceBodyStart] + getLastLines(body, maxLines) + "\n" + s.openFence + "\n"
}

// resetTail 替换尚未定稿的尾部文本。
func (s *markdownStream) resetTail(text string) {
	// 1. 清空尾部后写入新文本。
	s.tail.Reset()
	s.tail.WriteString(text)
}

// trackOpenFence 从完整行中识别开头的代码围栏，返回闭合围栏行末尾的位置；没有围栏或还没闭合时返回 0。
func (s *markdownStream) trackOpenFence(complete string) int {
	// 1. 跳过 ASCII 空白行；第一个非空行不是缩进不超过三格的围栏时直接返回。
	s.openFence = ""
	for position := 0; position < len(complete); {
		end := position + strings.IndexByte(complete[position:], '\n') + 1
		line := complete[position:end]
		position = end
		// 只去掉 goldmark 识别的 ASCII 空白。
		trimmed := strings.Trim(line, markdownSpaces)
		if trimmed == "" {
			continue
		}
		fence := getCodeFenceOpening(trimmed)
		if fence == "" || getLeadingSpaceCount(line) > maxFenceIndent {
			return 0
		}
		// 2. 记录围栏和扫描位置，再往下找闭合行。
		s.openFence, s.fenceBodyStart, s.fenceScannedLength = fence, position, position
		if s.scanFenceClosing(complete) {
			return s.fenceScannedLength
		}
		return 0
	}
	return 0
}

// scanFenceClosing 从上次扫描位置往下找闭合围栏，找到时清除围栏记录；返回是否找到。
func (s *markdownStream) scanFenceClosing(complete string) bool {
	// 1. 从上次位置扫描新增完整行；缩进不超过三格且符合闭合标记时清除围栏，并停在该行末尾。
	for s.fenceScannedLength < len(complete) {
		end := s.fenceScannedLength + strings.IndexByte(complete[s.fenceScannedLength:], '\n') + 1
		line := complete[s.fenceScannedLength:end]
		s.fenceScannedLength = end
		if getLeadingSpaceCount(line) <= maxFenceIndent && isCodeFenceClosing(strings.Trim(line, markdownSpaces), s.openFence) {
			s.openFence = ""
			return true
		}
	}
	return false
}
