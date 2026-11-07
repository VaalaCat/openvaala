package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/executor/tools"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
	"github.com/VaalaCat/openvaala/pkg/prompt"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/skill"
	"github.com/VaalaCat/openvaala/pkg/storage"
	"github.com/VaalaCat/openvaala/pkg/utils/jsonutil"
	"github.com/VaalaCat/openvaala/pkg/workspace"
)

// getHookStartSource 返回 Session 这次启动后第一个普通 Turn 触发 SessionStart 用的来源。
func getHookStartSource(record defs.Session, lastSavedOrdinal storage.Ordinal, isSpawned bool) string {
	// 1. 新建的子 Session 或日志为空的根 Session 返回 startup，已有日志的根 Session 返回 resume，重开的子 Session 返回空。
	switch {
	case isSpawned, record.ParentSessionID == "" && lastSavedOrdinal == 0:
		return "startup"
	case record.ParentSessionID == "":
		return "resume"
	}
	return ""
}

// validateConversationDependencies 检查对话配置带齐了 DAO 和定义，缺了就返回错误。
func validateConversationDependencies(opt *ConversationOpt) error {
	// 1. 配置、Query、Mutation、定义任一为空时返回错误。
	if opt == nil || opt.Query == nil || opt.Mutation == nil || opt.Definitions == nil {
		return errors.New("Conversation requires DAO and definitions")
	}
	return nil
}

// validateConversationDefinitions 检查定义里至少有一个 Agent 和一个模型，且每个 Agent 都有名字。
func validateConversationDefinitions(definitions *ConversationDefinitions) error {
	// 1. 没有 Agent 或模型、有 Agent 定义为空或名字为空白时返回错误。
	if definitions == nil || len(definitions.Agents) == 0 || len(definitions.Models) == 0 {
		return errors.New("Conversation requires at least one agent definition and at least one model")
	}
	for _, definition := range definitions.Agents {
		if definition == nil || strings.TrimSpace(definition.Name) == "" {
			return errors.New("agent definition requires a name")
		}
	}
	return nil
}

// getFilledExecutorSettings 返回补上 local 工作区目录的设置。
func getFilledExecutorSettings(settings defs.ExecutorSettings, executors []executor.Executor) defs.ExecutorSettings {
	// 1. 查找 local Executor；设置里没有 local 目录时复制目录表，填入定义里的目录。
	index := slices.IndexFunc(executors, func(listed executor.Executor) bool { return listed.GetDefinition().ID == defs.LocalExecutorID })
	if index < 0 || settings.WorkspaceDirectories[defs.LocalExecutorID] != "" {
		return settings
	}
	settings.WorkspaceDirectories = maps.Clone(settings.WorkspaceDirectories)
	if settings.WorkspaceDirectories == nil {
		settings.WorkspaceDirectories = make(map[string]string, 1)
	}
	settings.WorkspaceDirectories[defs.LocalExecutorID] = executors[index].GetDefinition().WorkspaceDirectory
	return settings
}

// getAvailableExecutors 返回设置允许使用的 Executor，默认 Executor 排在最前。
func getAvailableExecutors(settings defs.ExecutorSettings, executors []executor.Executor) []executor.Executor {
	// 1. 按列表顺序筛出可用的 Executor，设置了默认 Executor 且它可用时移到最前。
	available := slices.DeleteFunc(slices.Clone(executors), func(listed executor.Executor) bool {
		return !settings.IsExecutorAvailable(listed.GetDefinition().ID)
	})
	index := slices.IndexFunc(available, func(listed executor.Executor) bool { return listed.GetDefinition().ID == settings.DefaultExecutorID })
	if index > 0 {
		defaultExecutor := available[index]
		available = slices.Insert(slices.Delete(available, index, index+1), 0, defaultExecutor)
	}
	return available
}

// getMemoryProjectName 返回默认 Executor 和根目录对应的项目记忆目录，相对记忆根目录：
// projects/<executorID>/<把根目录里的 / 换成 - 之后的名字>。
func getMemoryProjectName(executorID, rootDirectory string) string {
	// 1. 根目录里的 / 换成 -，作为 Executor 目录下的一层目录名。
	return filepath.Join("projects", executorID, strings.ReplaceAll(rootDirectory, "/", "-"))
}

// getUnknownExecutorIDs 返回设置里 Executor 列表中没有的 ID。
func getUnknownExecutorIDs(settings defs.ExecutorSettings, executors []executor.Executor) []string {
	// 1. 先看默认 Executor，再看 executor_ids，收集未知 ID，跳过空值和重复项。
	var unknownIDs []string
	for _, id := range append([]string{settings.DefaultExecutorID}, settings.ExecutorIDs...) {
		isKnown := id == "" || slices.ContainsFunc(executors, func(listed executor.Executor) bool { return listed.GetDefinition().ID == id })
		if !isKnown && !slices.Contains(unknownIDs, id) {
			unknownIDs = append(unknownIDs, id)
		}
	}
	return unknownIDs
}

// getSkillDirectories 返回工作区里 directory 下的两个 Skill 目录。
func getSkillDirectories(executorID string, executorWorkspace workspace.Workspace, directory string) []skill.SkillDirectory {
	// 1. 为 directory 下的 .agents/skills、.claude/skills 填入工作区和 Executor ID。
	return []skill.SkillDirectory{
		{
			Workspace:  executorWorkspace,
			ExecutorID: executorID,
			Path:       filepath.Join(directory, ".agents", "skills"),
		},
		{
			Workspace:  executorWorkspace,
			ExecutorID: executorID,
			Path:       filepath.Join(directory, ".claude", "skills"),
		},
	}
}

// mergeSkills 按组顺序合并 Skill，同名保留先出现的，再按名字排序。
func mergeSkills(groups ...[]*skill.Skill) []*skill.Skill {
	// 1. 按组顺序合并，同名 Skill 保留第一项，再按名字排序。
	var merged []*skill.Skill
	for _, group := range groups {
		for _, listed := range group {
			if !slices.ContainsFunc(merged, func(kept *skill.Skill) bool { return kept.Name == listed.Name }) {
				merged = append(merged, listed)
			}
		}
	}
	slices.SortFunc(merged, func(a, b *skill.Skill) int { return strings.Compare(a.Name, b.Name) })
	return merged
}

// getUsedMCPServers 按定义顺序返回 Agent 使用的 MCP server。
func getUsedMCPServers(agents []*agent.AgentDefinition, servers []*mcpclient.ServerDefinition) []*mcpclient.ServerDefinition {
	// 1. 有 Agent 没限制 MCP server（MCPServerIDs 为 nil）时返回全部，否则按各 Agent 名单的并集筛选。
	usedIDs := make(map[string]bool)
	for _, definition := range agents {
		if definition.MCPServerIDs == nil {
			return slices.Clone(servers)
		}
		for _, id := range definition.MCPServerIDs {
			usedIDs[id] = true
		}
	}
	return slices.DeleteFunc(slices.Clone(servers), func(server *mcpclient.ServerDefinition) bool { return !usedIDs[server.ID] })
}

// validateConversationConfig 校验模型、Agent、工具和 Skill，返回 Agent 与模型索引。
func validateConversationConfig(
	ctx context.Context,
	definitions *ConversationDefinitions,
	skills []*skill.Skill,
	environments []executor.ExecutionEnvironment,
) (map[string]*agent.AgentDefinition, map[defs.ModelRef]*defs.ModelDefinition, error) {
	// 1. 校验各模型带有连接参数，建立模型引用索引。
	models := make(map[defs.ModelRef]*defs.ModelDefinition, len(definitions.Models))
	for _, model := range definitions.Models {
		if model == nil || model.Target == nil {
			return nil, nil, errors.New("model definition requires connection parameters")
		}
		models[model.Ref] = model
	}
	// 2. 校验 Agent 的模型引用和名称重复情况，建立 Agent 名称索引。
	agentDefinitions := make(map[string]*agent.AgentDefinition, len(definitions.Agents))
	for _, definition := range definitions.Agents {
		if _, found := models[definition.Model]; !found {
			return nil, nil, fmt.Errorf("agent %q uses model %s, which is not configured", definition.Name, definition.Model)
		}
		if _, found := models[definition.CompactModel]; definition.CompactModel != (defs.ModelRef{}) && !found {
			return nil, nil, fmt.Errorf("agent %q uses compact model %s, which is not configured", definition.Name, definition.CompactModel)
		}
		if _, found := agentDefinitions[definition.Name]; found {
			return nil, nil, fmt.Errorf("agent %q is defined more than once", definition.Name)
		}
		agentDefinitions[definition.Name] = definition
	}
	// 3. 创建协作工具，和执行环境的内置工具放在一起校验工具定义，MCP 工具不参与。
	collaborationTools, err := newCollaborationTools(nil, defs.Session{}, definitions)
	if err != nil {
		return nil, nil, err
	}
	toolExecutorOpt := executor.NewToolExecutorOpt(environments, collaborationTools...)
	toolExecutorOpt.MCPServerIDs = defs.OptionalStringList{}
	toolExecutor, err := executor.NewToolExecutor(toolExecutorOpt)
	if err != nil {
		return nil, nil, err
	}
	if _, err := toolExecutor.GetTools(ctx); err != nil {
		return nil, nil, err
	}
	// 4. 有执行环境时，校验各 Agent 选的工具名和 Skill 名都存在。
	executorToolNames := getExecutorToolNames(environments)
	if len(environments) > 0 {
		for _, definition := range definitions.Agents {
			for _, toolName := range definition.ToolNames {
				if !slices.Contains(executorToolNames, toolName) {
					return nil, nil, fmt.Errorf("agent %q uses tool %q, which is not configured", definition.Name, toolName)
				}
			}
			for _, skillName := range definition.SkillNames {
				if !slices.ContainsFunc(skills, func(listed *skill.Skill) bool { return listed.Name == skillName }) {
					return nil, nil, fmt.Errorf("agent %q uses skill %q, which is not configured", definition.Name, skillName)
				}
			}
		}
	}
	return agentDefinitions, models, nil
}

// getAgentDefaultModels 返回以 Agent 名为键、默认模型为值的索引。
func getAgentDefaultModels(agents []*agent.AgentDefinition) map[string]defs.ModelRef {
	// 1. 以 Agent 名为键保存各自的默认模型。
	defaultModels := make(map[string]defs.ModelRef, len(agents))
	for _, definition := range agents {
		defaultModels[definition.Name] = definition.Model
	}
	return defaultModels
}

// newClientMCPConnections 按登记顺序创建客户端 MCP 连接。
func newClientMCPConnections(servers []ClientMCPServer) ([]mcpclient.Connection, error) {
	// 1. 为每项登记创建弱依赖连接，启动和工具调用超时取默认值；任一失败时返回错误。
	connections := make([]mcpclient.Connection, 0, len(servers))
	for _, server := range servers {
		definition := &mcpclient.ServerDefinition{
			ID:             server.ID,
			StartupTimeout: mcpclient.DefaultStartupTimeout,
			ToolTimeout:    mcpclient.DefaultToolTimeout,
		}
		connection, err := mcpclient.NewConnection(mcpclient.NewConnectionOpt(definition, nil).WithTransport(server.Transport))
		if err != nil {
			return nil, fmt.Errorf("create client MCP server %q: %w", server.ID, err)
		}
		connections = append(connections, connection)
	}
	return connections, nil
}

// newAgentNotConfiguredError 创建 Session 用的 Agent 不在目录里的错误。
func newAgentNotConfiguredError(record defs.Session) error {
	// 1. 错误里带上 Session ID 和记录里保存的 Agent 名。
	return fmt.Errorf("Session %s uses agent %q, which is not configured", record.ID, record.AgentName)
}

// newModelSelection 为指定模型选出推理强度：requestedEffort 优先，其次是模型也支持的 carriedEffort，最后是模型默认值。
func newModelSelection(model *defs.ModelDefinition, requestedEffort, carriedEffort string) (defs.ModelSelection, error) {
	// 1. 先用模型的默认强度。
	selection := defs.ModelSelection{
		Ref:             model.Ref,
		ReasoningEffort: model.DefaultReasoningEffort,
	}
	// 2. 指定的强度模型不支持时返回错误；没指定时，沿用的强度模型也支持就用它。
	switch {
	case requestedEffort != "":
		if !model.IsReasoningEffortSupported(requestedEffort) {
			return defs.ModelSelection{}, fmt.Errorf("model %s does not support reasoning effort %q", model.Ref, requestedEffort)
		}
		selection.ReasoningEffort = requestedEffort
	case carriedEffort != "" && model.IsReasoningEffortSupported(carriedEffort):
		selection.ReasoningEffort = carriedEffort
	}
	return selection, nil
}

// getRequestedModelSelection 校验创建或分叉对话时请求的模型，返回带推理强度的选择。
func getRequestedModelSelection(models map[defs.ModelRef]*defs.ModelDefinition, requested defs.ModelSelection, agentModel defs.ModelRef) (defs.ModelSelection, error) {
	// 1. 没给模型时用 Agent 默认模型，模型不在目录里时返回错误，再选出推理强度。
	ref := requested.Ref
	if ref == (defs.ModelRef{}) {
		ref = agentModel
	}
	model, found := models[ref]
	if !found {
		return defs.ModelSelection{}, fmt.Errorf("model %s is not configured", ref)
	}
	return newModelSelection(model, requested.ReasoningEffort, "")
}

// getExecutorToolNames 汇总各执行环境的内置工具名，不含 MCP 工具。
func getExecutorToolNames(environments []executor.ExecutionEnvironment) []string {
	// 1. 按环境顺序收集执行环境自带的工具名，有工作区且能跑命令的环境再追加 Session 建的命令工具名。
	var toolNames []string
	for _, environment := range environments {
		for _, tool := range environment.GetTools() {
			toolNames = append(toolNames, tool.GetDefinition().Name)
		}
		if environment.GetWorkspace() != nil && environment.IsCommandSupported() {
			toolNames = append(toolNames, tools.GetCommandToolNames()...)
		}
	}
	return toolNames
}

// newExecutorDetails 按配置顺序取出各执行环境的展示字段，种类取自同 ID 的 Executor。
func newExecutorDetails(environments []executor.ExecutionEnvironment, executors []executor.Executor) []ExecutorDetails {
	// 1. 按环境顺序填写 ID、说明和工作区根目录，没有工作区时目录为空。
	details := make([]ExecutorDetails, len(environments))
	for index, environment := range environments {
		directory := ""
		if environmentWorkspace := environment.GetWorkspace(); environmentWorkspace != nil {
			directory = environmentWorkspace.GetRootDirectory()
		}
		// 2. 在列表里找同 ID 的 Executor，取它的种类。
		var kind defs.ExecutorKind
		if executorIndex := slices.IndexFunc(executors, func(listed executor.Executor) bool { return listed.GetDefinition().ID == environment.GetID() }); executorIndex >= 0 {
			kind = executors[executorIndex].GetDefinition().Kind
		}
		details[index] = ExecutorDetails{
			ID:                 environment.GetID(),
			Kind:               kind,
			Description:        environment.GetDescription(),
			WorkspaceDirectory: directory,
		}
	}
	return details
}

// getSkillNames 按顺序返回各 Skill 的名字。
func getSkillNames(skills []*skill.Skill) []string {
	// 1. 按输入顺序取出 Skill 名。
	names := make([]string, len(skills))
	for index, listed := range skills {
		names[index] = listed.Name
	}
	return names
}

// getSessionToolNames 返回 Agent 可用的内置工具、已连接 MCP 工具和协作工具名。
func getSessionToolNames(environments []executor.ExecutionEnvironment, clientMCPServers executor.ClientMCPServers, definition *agent.AgentDefinition, collaborationTools []executor.Tool) []string {
	// 1. 按 Agent 的 ToolNames 筛选内置工具，名单为空时全要，并去掉重名。
	var names []string
	for _, name := range getExecutorToolNames(environments) {
		if len(definition.ToolNames) > 0 && !slices.Contains(definition.ToolNames, name) {
			continue
		}
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	// 2. 按 MCPServerIDs 筛选执行环境里已连接的 MCP 工具，nil 时全要，空列表时一个不要。
	for _, environment := range environments {
		for _, info := range environment.GetMCPServers() {
			if info.State != mcpclient.ConnectionStateConnected {
				continue
			}
			if definition.MCPServerIDs != nil && !slices.Contains(definition.MCPServerIDs, info.ServerID) {
				continue
			}
			for _, tool := range info.Tools {
				if !slices.Contains(names, tool.Name) {
					names = append(names, tool.Name)
				}
			}
		}
	}
	// 3. MCPServerIDs 为 nil 时追加已连接的客户端 MCP 工具。
	if definition.MCPServerIDs == nil {
		for _, info := range clientMCPServers.GetMCPServers() {
			if info.State != mcpclient.ConnectionStateConnected {
				continue
			}
			for _, tool := range info.Tools {
				if !slices.Contains(names, tool.Name) {
					names = append(names, tool.Name)
				}
			}
		}
	}
	// 4. 追加协作工具名。
	for _, tool := range collaborationTools {
		names = append(names, tool.GetDefinition().Name)
	}
	return names
}

// validateSessionTree 校验各 Session 都属于这个对话，子 Session 的父 Session 存在、创建它的 Turn 和工具调用都有记录。
func validateSessionTree(record defs.Conversation, records []defs.Session) error {
	// 1. 检查各 Session 的 ID 非空且属于这个对话，建立 ID 索引。
	byID := make(map[defs.SessionID]defs.Session)
	for _, s := range records {
		if s.ConversationID != record.ID || s.ID == "" {
			return errors.New("invalid Session conversation relationship")
		}
		byID[s.ID] = s
	}
	// 2. 检查根 Session 没有父 Session，其余 Session 的 ParentTurnID、ParentCallID 齐全且父 Session 存在。
	for _, s := range records {
		if s.ID == record.RootSessionID {
			if s.ParentSessionID != "" {
				return errors.New("root Session must not have a parent")
			}
			continue
		}
		if s.ParentTurnID == "" || s.ParentCallID == "" {
			return errors.New("incomplete Session creation relationship")
		}
		if _, found := byID[s.ParentSessionID]; !found {
			return errors.New("Session parent not found")
		}
	}
	return nil
}

// cloneContent 按顺序复制消息内容。
func cloneContent(contents []*defs.Content) []*defs.Content {
	// 1. 按原顺序逐个复制。
	result := make([]*defs.Content, len(contents))
	for i, content := range contents {
		result[i] = content.Clone()
	}
	return result
}

// newAgentMessage 创建 Agent 之间的文本消息。
func newAgentMessage(source defs.Session, target defs.SessionID, relation defs.AgentRelation, text string) *defs.Item {
	// 1. 创建带来源、目标、来源任务名和双方关系的文本消息。
	return &defs.Item{
		ID:   defs.NewItemID(),
		Kind: defs.ItemMessage,
		Message: &defs.Message{
			From: &defs.Address{
				Kind: defs.IdentityAgent,
				ID:   string(source.ID),
			},
			To: &defs.Address{
				Kind: defs.IdentityAgent,
				ID:   string(target),
			},
			Content: []*defs.Content{{
				Kind: defs.ContentText,
				Text: &defs.TextContent{Text: text},
			}},
			AgentSender: &defs.AgentSender{
				TaskName: source.TaskName,
				Relation: relation,
			},
		},
	}
}

// newSubagentResultMessage 把子 Session 的 Turn 结果转成发给父 Session 的消息。
func newSubagentResultMessage(child defs.Session, finished *session.SessionTurnFinished) *defs.Item {
	// 1. 用终态或错误作说明文字；终态没保存时返回不带任务通知的普通 Agent 消息。
	explanation := string(finished.Status)
	if finished.Err != nil {
		explanation = finished.Err.Error()
	}
	if finished.Status == "" {
		return newAgentMessage(child, child.ParentSessionID, defs.AgentRelationChild,
			fmt.Sprintf("子任务 %s 的 Turn %s 结束但终态未保存：%s", child.ID, finished.TurnID, explanation))
	}
	// 2. 终态已保存时附上任务通知；成功时正文换成最后一次模型调用的全部消息，可以为空。
	message := newAgentMessage(child, child.ParentSessionID, defs.AgentRelationChild, explanation)
	if finished.Status == defs.TurnStatusCompleted {
		message.Message.Content = finalContent(finished)
	}
	message.Message.SubagentNotification = &defs.SubagentNotification{
		SourceTurnID: finished.TurnID,
		Status:       finished.Status,
	}
	return message
}

// finalContent 复制成功的 Turn 里最后一次模型调用输出的消息内容。
func finalContent(finished *session.SessionTurnFinished) []*defs.Content {
	// 1. Turn 没成功或没有最终调用 ID 时返回空，否则收集这次调用的全部消息内容。
	if finished.FinalModelCallID == "" || finished.Status != defs.TurnStatusCompleted {
		return nil
	}
	var content []*defs.Content
	for _, item := range finished.Outputs {
		if item.Kind == defs.ItemMessage && item.Message != nil && item.ModelCallID == finished.FinalModelCallID {
			content = append(content, cloneContent(item.Message.Content)...)
		}
	}
	return content
}

// newWaitAgentResult 汇总唤醒原因和去重后的 Agent 发送方。
func newWaitAgentResult(items []*defs.Item) waitAgentResult {
	// 1. 扫描消息来源，记下有没有用户输入、子任务通知，以及去重后的 Agent SessionID。
	result := waitAgentResult{Reason: waitReasonAgentMessage}
	hasUser, hasNotification := false, false
	for _, item := range items {
		if item.Message == nil || item.Message.From == nil {
			continue
		}
		switch item.Message.From.Kind {
		case defs.IdentityUser:
			hasUser = true
		case defs.IdentityAgent:
			hasNotification = hasNotification || item.Message.SubagentNotification != nil
			if from := defs.SessionID(item.Message.From.ID); !slices.Contains(result.From, from) {
				result.From = append(result.From, from)
			}
		}
	}
	// 2. 按用户输入、子任务通知、其他 Agent 消息的顺序选择唤醒原因。
	switch {
	case hasUser:
		result.Reason = waitReasonUserSteer
	case hasNotification:
		result.Reason = waitReasonSubagentResult
	}
	return result
}

// forkTurnLimit 解析 spawn_agent 的 fork_turns：-1 表示不继承，0 表示全部继承，正数是最多继承的轮数。
func forkTurnLimit(value string) (int, error) {
	// 1. 空值或 none 返回 -1，all 返回 0，其余只接受没有多余字符的正整数字符串。
	switch value {
	case "", "none":
		return -1, nil
	case "all":
		return 0, nil
	}
	count, err := strconv.Atoi(value)
	if err != nil || count <= 0 || strconv.Itoa(count) != value {
		return 0, fmt.Errorf("fork_turns must be none, all, or a positive integer string")
	}
	return count, nil
}

// selectForkHistory 从最近的轮次往前选出要继承的历史，返回按时间正序排列的 Item 副本。
func selectForkHistory(history []*defs.Item, limit int) []*defs.Item {
	// 1. 从新到旧选择轮次，limit 为 0 表示不限；遇到摘要时停止，还没到轮数上限就带上这份摘要。
	seen := make(map[defs.TurnID]bool)
	var selected []*defs.Item
	for i := len(history) - 1; i >= 0; i-- {
		item := history[i]
		if item.Kind == defs.ItemContextSummary {
			if limit > 0 && len(seen) >= limit {
				break
			}
			selected = append(selected, item.Clone())
			break
		}
		if !seen[item.TurnID] {
			if limit > 0 && len(seen) >= limit {
				break
			}
			seen[item.TurnID] = true
		}
		// 跳过推理、模型切换和 Executor 变化。
		if item.Kind == defs.ItemReasoning || item.Kind == defs.ItemModelChange || item.Kind == defs.ItemExecutorsChange {
			continue
		}
		selected = append(selected, item.Clone())
	}
	// 2. 把选出的 Item 改成时间正序。
	slices.Reverse(selected)
	return selected
}

// getSummaryBoundaryIndex 查找最近的摘要覆盖到的最后一个 Item，返回它的下标，找不到时返回 -1。
func getSummaryBoundaryIndex(items []dao.LoggedItem) int {
	// 1. 从后往前找最近的摘要，再在它之前找 CoveredThroughItemID 指向的 Item。
	for index := len(items) - 1; index >= 0; index-- {
		if summary := items[index].Item; summary.Kind == defs.ItemContextSummary {
			return slices.IndexFunc(items[:index], func(logged dao.LoggedItem) bool {
				return logged.Item.ID == summary.ContextSummary.CoveredThroughItemID
			})
		}
	}
	return -1
}

// transformHistoryItems 复制 read_session_history 要返回的历史，去掉加密推理、provider 原始数据和内嵌图片。
func transformHistoryItems(items []dao.LoggedItem) []*defs.Item {
	// 1. 逐条复制，清空加密推理和 RawJSON，去掉消息和工具结果里 data: 开头的图片地址，保留 Alt。
	result := make([]*defs.Item, len(items))
	for i, logged := range items {
		transformed := logged.Item.Clone()
		if transformed.Reasoning != nil {
			transformed.Reasoning.Encrypted = ""
			transformed.Reasoning.RawJSON = nil
		}
		if transformed.Message != nil {
			for _, content := range transformed.Message.Content {
				if content != nil && content.Image != nil && strings.HasPrefix(content.Image.DataURL, "data:") {
					content.Image.DataURL = ""
				}
			}
		}
		if transformed.ToolResult != nil {
			for _, image := range transformed.ToolResult.Images {
				if image != nil && strings.HasPrefix(image.DataURL, "data:") {
					image.DataURL = ""
				}
			}
		}
		result[i] = transformed
	}
	return result
}

// encodeToolResult 把协作操作的返回值和错误编码成工具结果。
func encodeToolResult(callID string, payload any, err error) *executor.ToolExecutionResult {
	// 1. 操作没出错时编码返回值；操作失败时正文留 null；操作或编码失败都填写 collaboration_error。
	result := &defs.ToolResult{
		CallID:  callID,
		Content: json.RawMessage("null"),
	}
	if err == nil {
		result.Content, err = jsonutil.MarshalWithoutHTMLEscape(payload)
	}
	if err != nil {
		result.Error = &defs.ToolError{
			Code:    "collaboration_error",
			Message: err.Error(),
		}
	}
	return &executor.ToolExecutionResult{Result: result}
}

// logConversationStopped 按停止原因记录对话日志。
func logConversationStopped(logger *slog.Logger, err error) {
	// 1. 没有错误或因 ctx 取消停止时记 Info，其余错误记 Error。
	if err == nil || errors.Is(err, context.Canceled) {
		logger.Info("conversation stopped", "reason", err)
		return
	}
	logger.Error("conversation stopped with error", "error", err)
}

// newExecutorPromptEntries 按执行环境生成系统提示里的 Executor 条目。
func newExecutorPromptEntries(environments []executor.ExecutionEnvironment) []prompt.ExecutorEntry {
	// 1. 按环境顺序填写 ID、说明和工作区根目录，第一个仍是默认 Executor。
	entries := make([]prompt.ExecutorEntry, len(environments))
	for index, environment := range environments {
		directory := ""
		if environmentWorkspace := environment.GetWorkspace(); environmentWorkspace != nil {
			directory = environmentWorkspace.GetRootDirectory()
		}
		entries[index] = prompt.ExecutorEntry{
			ID:                 environment.GetID(),
			Description:        environment.GetDescription(),
			WorkspaceDirectory: directory,
		}
	}
	return entries
}

// isMemoryEnvironment 执行环境是不是 memory？
func isMemoryEnvironment(environment executor.ExecutionEnvironment) bool {
	// 1. 按 ID 判断。
	return environment.GetID() == defs.MemoryExecutorID
}

// newAgentPromptEntries 按 Agent 定义生成 spawn_agent 工具说明里的可选 Agent 条目。
func newAgentPromptEntries(agents []*agent.AgentDefinition) []prompt.AgentEntry {
	// 1. 按定义顺序填写 Agent 名和用途说明。
	entries := make([]prompt.AgentEntry, len(agents))
	for index, definition := range agents {
		entries[index] = prompt.AgentEntry{
			Name:        definition.Name,
			Description: definition.Description,
		}
	}
	return entries
}
