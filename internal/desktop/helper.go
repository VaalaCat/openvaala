package desktop

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"time"

	"github.com/VaalaCat/mygo-kit"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/skill"
)

func conversationTitle(conv defs.Conversation) string {
	if conv.Title != "" {
		return conv.Title
	}
	return string(conv.ID)
}
func tokenNumber(n int) string {
	if n >= 1000000 {
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	}
	if n >= 1000 {
		return fmt.Sprintf("%.1fK", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

// marshalJSONView 把值编码成缩进 JSON 供 JSON 视图显示和复制：不转义 HTML 字符；json.RawMessage 直接用原字节缩进，保留模型给出的原样参数。
func marshalJSONView(value any) ([]byte, error) {
	// 1. 原始 JSON 不经过 Marshal（它会把 & 转成 \u0026），只做缩进。
	if raw, ok := value.(json.RawMessage); ok {
		var indented bytes.Buffer
		if err := json.Indent(&indented, raw, "", "  "); err != nil {
			return nil, err
		}
		return indented.Bytes(), nil
	}
	// 2. 其余值用不转义 HTML 的编码器，去掉末尾换行。
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buffer.Bytes(), "\n"), nil
}

// getOutputHeight 返回输出框的高度：边框、标题栏、上下内边距加每行 20 的行高，超过 maxHeight 时由框内滚动。
func getOutputHeight(text string, maxHeight float32, t *kit.Theme) float32 {
	// 1. 末尾换行产生的空行也占一行，和输出框的行数一致。
	lines := float32(strings.Count(text, "\n") + 1)
	chrome := 2 + t.Metrics.ControlHeight + 2*t.Space(1)
	return min(lines*t.Space(5)+chrome, maxHeight)
}

// countOutputLines 返回输出文本显示的行数，末尾的换行不算一行。
func countOutputLines(text string) int {
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return 0
	}
	return strings.Count(text, "\n") + 1
}

// getFirstStringArgument 按 JSON 书写顺序返回工具参数里第一个字符串值；没有字符串参数时返回空。
func getFirstStringArgument(arguments string) string {
	// 1. 逐个键值对读取顶层对象，遇到第一个字符串值即返回。
	decoder := json.NewDecoder(strings.NewReader(arguments))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return ""
	}
	for decoder.More() {
		if _, err := decoder.Token(); err != nil {
			return ""
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return ""
		}
		var text string
		if json.Unmarshal(value, &text) == nil {
			return text
		}
	}
	return ""
}
func contentText(content []*defs.Content) string {
	var b strings.Builder
	for _, part := range content {
		if part != nil && part.Kind == defs.ContentText && part.Text != nil {
			b.WriteString(part.Text.Text)
		}
	}
	return b.String()
}
func draftText(content []*defs.Content) string {
	var b strings.Builder
	for _, part := range content {
		if isDraftText(part) {
			b.WriteString(part.Text.Text)
		}
	}
	return b.String()
}

// isDraftText 内容是否为用户输入的草稿文本（不含上下文材料）？
func isDraftText(content *defs.Content) bool {
	return content != nil && content.Kind == defs.ContentText && content.Text != nil && !content.Text.IsContext
}

// isContentPresent 内容里是否有非空文本或任何非文本部分？
func isContentPresent(content []*defs.Content) bool {
	for _, part := range content {
		if part != nil && (part.Kind != defs.ContentText || part.Text != nil && strings.TrimSpace(part.Text.Text) != "") {
			return true
		}
	}
	return false
}
func replaceDraftText(content []*defs.Content, text string) []*defs.Content {
	out := make([]*defs.Content, 0, len(content)+1)
	inserted := false
	for _, part := range content {
		if isDraftText(part) {
			if !inserted && text != "" {
				out = append(out, &defs.Content{Kind: defs.ContentText, Text: &defs.TextContent{Text: text}})
			}
			inserted = true
		} else {
			out = append(out, part)
		}
	}
	if !inserted && text != "" {
		out = append([]*defs.Content{{Kind: defs.ContentText, Text: &defs.TextContent{Text: text}}}, out...)
	}
	return out
}
func removeSentContent(content, sent []*defs.Content) []*defs.Content {
	out := []*defs.Content{}
	for _, part := range content {
		remove := false
		if !isDraftText(part) {
			for _, original := range sent {
				if part == original {
					remove = true
					break
				}
			}
		}
		if !remove {
			out = append(out, part)
		}
	}
	text, submitted := draftText(out), draftText(sent)
	if text == submitted {
		text = ""
	} else if strings.TrimSpace(submitted) != "" && strings.HasPrefix(text, submitted) {
		text = strings.TrimLeft(text[len(submitted):], " \t\r\n")
	}
	return replaceDraftText(out, text)
}

// selectOptions 把业务字段的选项值及显示名组成控件选项。
func selectOptions(values, labels []string) []kit.SelectOption {
	// 1. 保留独立值与本地化显示名，不保存选择器状态。
	options := make([]kit.SelectOption, len(values))
	for index, value := range values {
		options[index] = kit.SelectOption{Value: value, Label: labels[index]}
	}
	return options
}

// formatRelativeTime 返回按界面语言本地化的相对时间，例如「刚刚」「3 分钟前」「昨天」；translate 取当前语言的文案。
func formatRelativeTime(updatedAt int64, now time.Time, translate func(string) string) string {
	// 1. 按经过时间选档，时钟偏差造成的负值按「刚刚」处理。
	elapsed := now.Sub(time.UnixMilli(updatedAt))
	switch {
	case elapsed < time.Minute:
		return translate("time.just.now")
	case elapsed < time.Hour:
		return fmt.Sprintf(translate("time.minutes.ago"), int(elapsed/time.Minute))
	case elapsed < 24*time.Hour:
		return fmt.Sprintf(translate("time.hours.ago"), int(elapsed/time.Hour))
	case elapsed < 48*time.Hour:
		return translate("time.yesterday")
	case elapsed < 7*24*time.Hour:
		return fmt.Sprintf(translate("time.days.ago"), int(elapsed/(24*time.Hour)))
	case elapsed < 365*24*time.Hour:
		return fmt.Sprintf(translate("time.weeks.ago"), int(elapsed/(7*24*time.Hour)))
	default:
		return fmt.Sprintf(translate("time.years.ago"), int(elapsed/(365*24*time.Hour)))
	}
}

// getErrorMessage 把后端或连接错误转成本地化的一句说明；没有对应说明时返回通用的「操作失败」，原文由调用方放进详情。
func (d *Desktop) getErrorMessage(err error) string {
	// 1. 先按哨兵错误和超时判断，再按 serve 返回的已知原文判断。
	switch {
	case errors.Is(err, runtimeclient.ErrConnectionLost), errors.Is(err, runtimeclient.ErrClientStopped):
		return d.t("error.connection.lost")
	case errors.Is(err, runtimeclient.ErrUnauthorized):
		return d.t("error.unauthorized")
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return d.t("error.timeout")
	case errors.Is(err, runtime.ErrConversationNotFound):
		return d.t("error.conversation.not.found")
	case errors.Is(err, runtime.ErrSessionNotFound):
		return d.t("error.session.not.found")
	case errors.Is(err, session.ErrSessionBusy):
		return d.t("error.session.busy")
	}
	text := err.Error()
	switch {
	case isNoModelError(err):
		return d.t("error.no.model")
	case strings.Contains(text, "executor") && (strings.Contains(text, "not configured") || strings.Contains(text, "not found")):
		return d.t("error.executor.not.found")
	}
	return d.t("error.operation.failed")
}

// isUserMessage Item 是否为用户消息？
func isUserMessage(item *defs.Item) bool {
	return item.Kind == defs.ItemMessage && item.Message != nil && item.Message.From != nil && item.Message.From.Kind == defs.IdentityUser
}

// getFacts 按顺序取出 source 中存在且非空的字段，标签是字段名，值按 %v 格式化。
func getFacts(source map[string]any, keys ...string) []kit.Fact {
	facts := []kit.Fact{}
	for _, key := range keys {
		if value, found := source[key]; found && value != nil {
			facts = append(facts, kit.Fact{Label: key, Value: fmt.Sprint(value)})
		}
	}
	return facts
}

// getSlashQuery 返回光标所在的词去掉前导 / 后、到光标为止的部分；光标所在的词不以 / 开头时 isSlash 为 false。
func getSlashQuery(text string, caret int) (query string, isSlash bool) {
	// 1. 只看光标所在的词，路径里的 a/b 不以 / 开头，不触发。
	start, end := slashWordRange(text, caret)
	if start < 0 {
		return "", false
	}
	runes := []rune(text)
	return string(runes[start+1 : max(start+1, min(end, caret))]), true
}

// getSkillMatches 返回名称或冒号后的短名以 query 开头的技能，不区分大小写，保持目录顺序。
func getSkillMatches(skills []skill.SkillInfo, query string) []skill.SkillInfo {
	// 1. 带命名空间的技能（plugin:name）也能直接输短名匹配。
	query = strings.ToLower(query)
	matches := []skill.SkillInfo{}
	for _, info := range skills {
		name := strings.ToLower(info.Name)
		if strings.HasPrefix(name, query) || strings.HasPrefix(name[strings.Index(name, ":")+1:], query) {
			matches = append(matches, info)
		}
	}
	return matches
}

// insertSlashSkillAt 把光标所在的斜杠词换成 /name 加一个空格，保留后续正文，返回新文本和补全后的光标位置。
func insertSlashSkillAt(text, name string, caret int) (string, int) {
	// 1. 替换光标所在词，保留后续正文并把光标放到补全后的空格后。
	start, end := slashWordRange(text, caret)
	if start < 0 {
		return text, caret
	}
	runes := []rune(text)
	replacement := []rune("/" + name + " ")
	return string(runes[:start]) + string(replacement) + string(runes[end:]), start + len(replacement)
}

// slashWordRange 返回光标所在词的 rune 范围；该词不以 / 开头时返回 -1。
func slashWordRange(text string, caret int) (int, int) {
	// 1. 按 rune 范围查找斜杠词，不把中文 UTF-8 字节当光标位置。
	runes := []rune(text)
	caret = max(0, min(caret, len(runes)))
	start, end := caret, caret
	for start > 0 && runes[start-1] != ' ' && runes[start-1] != '\n' && runes[start-1] != '\t' {
		start--
	}
	for end < len(runes) && runes[end] != ' ' && runes[end] != '\n' && runes[end] != '\t' {
		end++
	}
	if start == end || runes[start] != '/' {
		return -1, -1
	}
	return start, end
}

// getSkillCatalogKey 返回一组可用技能的缓存键：已有对话由对话决定，草稿由 Agent、Executor 和目录决定。
func getSkillCatalogKey(conversationID defs.ConversationID, agentName, executorID, directory string) skillCatalogKey {
	// 1. 已有对话的技能不再随草稿字段变化。
	if conversationID != "" {
		return skillCatalogKey{ConversationID: conversationID}
	}
	return skillCatalogKey{AgentName: agentName, ExecutorID: executorID, Directory: directory}
}

// splitDirectoryInput 把用户输入的目录拆成要列出的上级目录和同层的名称前缀，规则与 WebUI 一致。
func splitDirectoryInput(input string) (parent, prefix string) {
	// 1. 显式的当前位置、上层和主目录按完整目录读取，其余只取最后一个斜线前的路径。
	path := strings.TrimSpace(input)
	if path == "." || path == ".." || path == "~" {
		return path + "/", ""
	}
	separator := strings.LastIndex(path, "/")
	if separator < 0 {
		return ".", path
	}
	return path[:separator+1], path[separator+1:]
}

// getWorkLocationLabel 返回工作位置按钮上的文字：Executor 加目录名；目录名超过 maxDirectory 个字符时截断（maxDirectory 为 0 不截断）。
func getWorkLocationLabel(executorID, directory string, maxDirectory int) string {
	// 1. 没有目录时只显示 Executor。
	if directory == "" {
		return executorID
	}
	name := []rune(filepath.Base(directory))
	if maxDirectory > 0 && len(name) > maxDirectory {
		name = append(name[:maxDirectory-1], '…')
	}
	return executorID + " · " + string(name)
}

// getDefaultDirectory 返回 Executor 在配置里的默认根目录；找不到 Executor 时为空。
func getDefaultDirectory(executors []conversation.ExecutorDetails, executorID string) string {
	// 1. 目录列表不大，直接按 ID 查找。
	index := slices.IndexFunc(executors, func(executor conversation.ExecutorDetails) bool { return executor.ID == executorID })
	if index < 0 {
		return ""
	}
	return executors[index].WorkspaceDirectory
}

// getModelOptions 返回模型下拉的选项：第一项是 Agent 默认，其后按服务分组，每组前有一个分组标题；每个模型的说明是它所属的服务，搜索服务名也能命中。
func getModelOptions(models []*defs.ModelDefinition, defaultLabel string) []kit.SelectOption {
	// 1. 服务按首次出现的顺序分组。
	options := []kit.SelectOption{{Value: "", Label: defaultLabel}}
	providers := []string{}
	for _, model := range models {
		if model != nil && !slices.Contains(providers, model.Ref.ProviderID) {
			providers = append(providers, model.Ref.ProviderID)
		}
	}
	// 2. 分组标题不是选项，不参与选择。
	for _, provider := range providers {
		options = append(options, kit.SelectOption{Label: provider, Header: true})
		for _, model := range models {
			if model == nil || model.Ref.ProviderID != provider {
				continue
			}
			label := model.DisplayName
			if label == "" {
				label = model.Ref.ModelID
			}
			options = append(options, kit.SelectOption{Value: model.Ref.String(), Label: label, Description: provider})
		}
	}
	return options
}

// getReasoningEfforts 返回所选模型支持的推理强度；model 为空时取 Agent 默认模型，没有可选档位时返回空。
func getReasoningEfforts(list runtime.ModelList, model string) []string {
	// 1. 空选择就是 Agent 默认模型。
	if model == "" && list.AgentDefaultModel != (defs.ModelRef{}) {
		model = list.AgentDefaultModel.String()
	}
	for _, definition := range list.Models {
		if definition != nil && definition.Ref.String() == model {
			return definition.ReasoningEfforts
		}
	}
	return nil
}

// getQueueShortcut 返回排队快捷键的提示文字。
func getQueueShortcut() string {
	// 1. macOS 用 Command，其余平台用 Ctrl。
	if goruntime.GOOS == "darwin" {
		return "⌘+Enter"
	}
	return "Ctrl+Enter"
}

// getPreviousUserMessage 返回第 offset 条（从最近一条往前数，1 为最近）用户文字消息；没有更早的返回 false。
func getPreviousUserMessage(items []viewItem, offset int) (string, bool) {
	// 1. 只算有文字的用户消息，图片和上下文材料不进输入框。
	for index := len(items) - 1; index >= 0; index-- {
		if !isUserMessage(items[index].Item) {
			continue
		}
		text := contentText(items[index].Item.Message.Content)
		if text == "" {
			continue
		}
		if offset--; offset == 0 {
			return text, true
		}
	}
	return "", false
}

// getSingleLine 把多行文字折成一行，连续空白合并成一个空格。
func getSingleLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// getModelTooltip 返回模型按钮的提示：「服务 / 模型」；没选模型时是 Agent 默认的说明。
func getModelTooltip(models []*defs.ModelDefinition, model, defaultLabel string) string {
	// 1. 目录里找不到的模型（例如服务已被删除）显示原始引用。
	if model == "" {
		return defaultLabel
	}
	for _, definition := range models {
		if definition != nil && definition.Ref.String() == model {
			name := definition.DisplayName
			if name == "" {
				name = definition.Ref.ModelID
			}
			return definition.Ref.ProviderID + " / " + name
		}
	}
	return model
}

// getDirectoryErrorDetail 返回目录解析失败的原因：去掉 RPC 方法名和错误类别前缀，只留「不是目录：路径」这类原文。
func getDirectoryErrorDetail(err error) string {
	// 1. serve 把校验原因放在 "invalid executor settings: " 之后。
	_, detail, found := strings.Cut(err.Error(), "invalid executor settings: ")
	if !found {
		return err.Error()
	}
	return detail
}

// getSessionCrumbs 返回从主 Agent 到 readingSessionID 的面包屑：沿父 Session 链向上，根 Session 名为 rootName，
// 子 Session 依次用任务名、Agent 名、Session ID；父链断开时从能找到的最上一级开始。readingSessionID 未知时返回 nil。
func getSessionCrumbs(infos []runtime.SessionRuntimeInfo, rootSessionID, readingSessionID defs.SessionID, rootName string) []sessionCrumb {
	// 1. 从当前 Session 逐级取父 Session，用 seen 防止父链成环。
	byID := make(map[defs.SessionID]defs.Session, len(infos))
	for _, info := range infos {
		byID[info.Session.ID] = info.Session
	}
	crumbs := []sessionCrumb{}
	seen := map[defs.SessionID]bool{}
	for id := readingSessionID; id != "" && !seen[id]; {
		seen[id] = true
		info, found := byID[id]
		if !found {
			break
		}
		name := rootName
		if id != rootSessionID {
			name = cmp.Or(info.TaskName, info.AgentName, string(id))
		}
		crumbs = append(crumbs, sessionCrumb{SessionID: id, Name: name})
		id = info.ParentSessionID
	}
	// 2. 链是从当前向根收集的，反转成「主 Agent › … › 当前」。
	slices.Reverse(crumbs)
	return crumbs
}

// getUniqueTitles 返回与 titles 等长的标题：第 n 个重复的标题追加「 (n)」，第一个保持原样。
func getUniqueTitles(titles []string) []string {
	// 1. 按出现顺序取第一个未被占用的名字，已经带后缀的标题也占位，结果互不相同。
	used := make(map[string]bool, len(titles))
	unique := make([]string, len(titles))
	for index, title := range titles {
		candidate := title
		for number := 2; used[candidate]; number++ {
			candidate = fmt.Sprintf("%s (%d)", title, number)
		}
		used[candidate] = true
		unique[index] = candidate
	}
	return unique
}

// getTurnInputIndex 返回 Turn 开始时用户输入在 items 里的下标，找不到时返回 -1。
// 输入取 Turn 已保存范围内第一条用户消息，范围内没有（排队插入在 Turn 之前）时取它之前最近的一条。
func getTurnInputIndex(turn defs.Turn, items []viewItem) int {
	// 1. 没有保存 Item 的 Turn（刚创建）没有可对应的消息。
	first, last := uint64(turn.ItemsRange.FirstOrdinal), uint64(turn.ItemsRange.LastOrdinal)
	if first == 0 {
		return -1
	}
	// 2. 范围内的优先，其次是之前最近的一条。
	inRange, before := -1, -1
	for index, view := range items {
		if !isUserMessage(view.Item) || view.Ordinal == 0 {
			continue
		}
		switch ordinal := uint64(view.Ordinal); {
		case ordinal < first:
			before = index
		case last == 0 || ordinal <= last:
			if inRange < 0 {
				inRange = index
			}
		}
	}
	if inRange >= 0 {
		return inRange
	}
	return before
}

// getTurnInputSummary 返回 Turn 开始时用户输入的单行摘要，最多 maxRunes 个字符；找不到输入时返回空串。
func getTurnInputSummary(turn defs.Turn, items []viewItem, maxRunes int) string {
	index := getTurnInputIndex(turn, items)
	if index < 0 {
		return ""
	}
	summary := getSingleLine(contentText(items[index].Item.Message.Content))
	if runes := []rune(summary); len(runes) > maxRunes {
		summary = string(runes[:maxRunes]) + "…"
	}
	return summary
}

// getProtocolLabel 返回模型协议的可读名称；未知协议原样返回。
func getProtocolLabel(protocol string) string {
	switch protocol {
	case "openai_chat":
		return "OpenAI Chat"
	case "openai_responses":
		return "OpenAI Responses"
	case "claude":
		return "Claude"
	}
	return protocol
}

// isNoModelError 错误是不是 serve 返回的「还没有配置任何模型」？
func isNoModelError(err error) bool {
	return strings.Contains(err.Error(), "no model is configured")
}

// joinConversationIDs 把对话 ID 用逗号连接成可直接显示的文字。
func joinConversationIDs(ids []defs.ConversationID) string {
	texts := make([]string, len(ids))
	for i, id := range ids {
		texts[i] = string(id)
	}
	return strings.Join(texts, ", ")
}

// shortcutHint 返回菜单加速键在命令面板里显示的文字，macOS 用 ⌘，其他平台用 Ctrl。
func shortcutHint(key string, shift bool) string {
	if goruntime.GOOS == "darwin" {
		if shift {
			return "⇧⌘" + key
		}
		return "⌘" + key
	}
	if shift {
		return "Ctrl+Shift+" + key
	}
	return "Ctrl+" + key
}

// previousConversationHint 返回「上一个对话」快捷键在命令面板里显示的文字；macOS 的 Cmd+Tab 是系统切换应用，所以两个平台都用 Ctrl。
func previousConversationHint() string {
	if goruntime.GOOS == "darwin" {
		return "⌃Tab"
	}
	return "Ctrl+Tab"
}
