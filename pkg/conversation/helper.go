package conversation

import (
	"bytes"
	"cmp"
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
	schemavalidator "github.com/santhosh-tekuri/jsonschema/v6"
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

// validateConversationDefinitions 检查 Agent 定义的名称和执行配置。
func validateConversationDefinitions(definitions *ConversationDefinitions) error {
	// 1. 检查至少有一个 Agent，且每个 Agent 的名称和类型有效。
	if definitions == nil || len(definitions.Agents) == 0 {
		return errors.New("Conversation requires at least one agent definition")
	}
	for _, definition := range definitions.Agents {
		if definition == nil || strings.TrimSpace(definition.Name) == "" {
			return errors.New("agent definition requires a name")
		}
		if err := definition.ValidateExecutionType(); err != nil {
			return err
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
	extraTools ...executor.Tool,
) (map[string]*agent.AgentDefinition, map[defs.ModelRef]*defs.ModelDefinition, error) {
	// 1. 校验各模型带有连接参数，建立模型引用索引。
	models := make(map[defs.ModelRef]*defs.ModelDefinition, len(definitions.Models))
	for _, model := range definitions.Models {
		if model == nil || model.Target == nil {
			return nil, nil, errors.New("model definition requires connection parameters")
		}
		models[model.Ref] = model
	}
	// 2. 校验 Agent 名称没有重复，建立名称索引。
	agentDefinitions := make(map[string]*agent.AgentDefinition, len(definitions.Agents))
	for _, definition := range definitions.Agents {
		if _, found := agentDefinitions[definition.Name]; found {
			return nil, nil, fmt.Errorf("agent %q is defined more than once", definition.Name)
		}
		agentDefinitions[definition.Name] = definition
	}
	// 3. 合并协作工具与附加工具，校验工具定义和重名；执行环境自带的内置工具参与重名校验，MCP 工具不参与。
	collaborationTools, err := newCollaborationTools(nil, defs.Session{}, definitions, nil)
	if err != nil {
		return nil, nil, err
	}
	toolExecutorOpt := executor.NewToolExecutorOpt(nil, append(collaborationTools, extraTools...)...)
	toolExecutorOpt.MCPServerIDs = defs.OptionalStringList{}
	toolExecutor, err := executor.NewToolExecutor(toolExecutorOpt)
	if err != nil {
		return nil, nil, err
	}
	if _, err := toolExecutor.GetToolsForEnvironments(ctx, environments, getDefaultExecutorID(environments)); err != nil {
		return nil, nil, err
	}
	// 4. 校验附加工具的 schema，并补查与 Session 启动时才创建的命令工具是否重名。
	executorToolNames := getExecutorToolNames(environments)
	for _, tool := range extraTools {
		definition := tool.GetDefinition()
		if slices.Contains(executorToolNames, definition.Name) {
			return nil, nil, fmt.Errorf("command tool %q conflicts with an agent tool", definition.Name)
		}
		if err := validateExtraToolSchema(definition.InputSchema); err != nil {
			return nil, nil, fmt.Errorf("tool %q schema: %w", definition.Name, err)
		}
	}
	// 5. 有执行环境时，校验各 Agent 选的工具名和 Skill 名都存在。
	if len(environments) > 0 {
		for _, definition := range definitions.Agents {
			for _, toolName := range definition.ToolNames {
				if !slices.Contains(executorToolNames, toolName) {
					return nil, nil, fmt.Errorf("agent %q uses tool %q, which is not configured", definition.Name, toolName)
				}
			}
			for _, skillName := range definition.SkillNames {
				if skillName != skill.BuiltinSkillName && !slices.ContainsFunc(skills, func(listed *skill.Skill) bool { return listed.Name == skillName }) {
					return nil, nil, fmt.Errorf("agent %q uses skill %q, which is not configured", definition.Name, skillName)
				}
			}
		}
	}
	return agentDefinitions, models, nil
}

// validateExtraToolSchema 校验模型将收到的工具参数 schema。
func validateExtraToolSchema(inputSchema json.RawMessage) error {
	// 1. 用工具自身的 schema 和库内置的元 schema 编译，不读取外部资源。
	document, err := schemavalidator.UnmarshalJSON(bytes.NewReader(inputSchema))
	if err != nil {
		return err
	}
	compiler := schemavalidator.NewCompiler()
	compiler.UseLoader(nil)
	const schemaURL = "urn:openvaala:tool-arguments"
	if err := compiler.AddResource(schemaURL, document); err != nil {
		return err
	}
	_, err = compiler.Compile(schemaURL)
	return err
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

// newSpawnAgentNotConfiguredError 列出可用于创建会话的 Agent。
func newSpawnAgentNotConfiguredError(name string, definitions map[string]*agent.AgentDefinition) error {
	return fmt.Errorf("agent %q is not configured. Configured agents: %s. To change the model, use --model / model",
		name, strings.Join(slices.Sorted(maps.Keys(definitions)), ", "))
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

// getRequestedModelSelection 为原生会话解析指定模型或默认模型及其推理强度。
func getRequestedModelSelection(models map[defs.ModelRef]*defs.ModelDefinition, requested defs.ModelSelection, definition *agent.AgentDefinition) (defs.ModelSelection, error) {
	// 1. 外部 Agent 没有本机模型选择；原生 Agent 取指定模型或默认模型。
	if definition.Type.IsExternal() {
		return defs.ModelSelection{}, nil
	}
	model, err := getAgentModel(models, definition, cmp.Or(requested.Ref, definition.Model))
	if err != nil {
		return defs.ModelSelection{}, err
	}
	// 2. 选择推理强度。
	return newModelSelection(model, requested.ReasoningEffort, "")
}

// getAgentModel 校验 Agent 的默认模型和摘要模型，再返回选中的本机模型。
func getAgentModel(models map[defs.ModelRef]*defs.ModelDefinition, definition *agent.AgentDefinition, ref defs.ModelRef) (*defs.ModelDefinition, error) {
	// 1. 校验默认模型引用；选的是默认模型时直接返回。
	defaultModel, err := getAgentDefaultModel(models, definition)
	if err != nil || ref == definition.Model {
		return defaultModel, err
	}
	// 2. 取显式选中的模型。
	model, found := models[ref]
	if !found {
		return nil, fmt.Errorf("model %s is not configured", ref)
	}
	return model, nil
}

// getAgentDefaultModel 校验原生 Agent 的默认模型和摘要模型，返回默认模型。
func getAgentDefaultModel(models map[defs.ModelRef]*defs.ModelDefinition, definition *agent.AgentDefinition) (*defs.ModelDefinition, error) {
	// 1. 原生 Agent 的默认模型必须在目录里。
	if definition.Type.IsExternal() {
		return nil, nil
	}
	if len(models) == 0 {
		return nil, ErrNoModelConfigured
	}
	model, found := models[definition.Model]
	if !found {
		return nil, fmt.Errorf("agent %q uses model %s, which is not configured", definition.Name, definition.Model)
	}
	// 2. Agent 指定摘要模型时检查它也在目录里。
	if _, found := models[definition.CompactModel]; definition.CompactModel != (defs.ModelRef{}) && !found {
		return nil, fmt.Errorf("agent %q uses compact model %s, which is not configured", definition.Name, definition.CompactModel)
	}
	return model, nil
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
		if environment.IsDynamic() {
			kind = defs.ExecutorKindClient
		} else if executorIndex := slices.IndexFunc(executors, func(listed executor.Executor) bool { return listed.GetDefinition().ID == environment.GetID() }); executorIndex >= 0 {
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

// getSessionToolNames 返回 Agent 可用的内置工具、已连接 MCP 工具和 Agent 侧工具名。
func getSessionToolNames(environments []executor.ExecutionEnvironment, definition *agent.AgentDefinition, agentTools []executor.Tool) []string {
	// 1. 汇总各环境上 Agent 可见的工具并去掉重名。
	var names []string
	for _, environment := range environments {
		for _, name := range getEnvironmentToolNames(environment, environment.GetMCPServers(), definition) {
			if !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	// 2. 追加 Agent 侧工具名。
	for _, tool := range agentTools {
		names = append(names, tool.GetDefinition().Name)
	}
	return names
}

// getEnvironmentToolNames 返回 Agent 在此环境可用的工具名，不等待 MCP 连接。
func getEnvironmentToolNames(environment executor.ExecutionEnvironment, servers []mcpclient.ConnectionInfo, definition *agent.AgentDefinition) []string {
	// 1. 按 ToolNames 筛选内置工具，名单为空时全要。
	names := make([]string, 0)
	for _, name := range getExecutorToolNames([]executor.ExecutionEnvironment{environment}) {
		if len(definition.ToolNames) == 0 || slices.Contains(definition.ToolNames, name) {
			names = append(names, name)
		}
	}
	// 2. 显式选择 MCP server 的 Agent 只使用稳定环境的连接。
	if environment.IsDynamic() && definition.MCPServerIDs != nil {
		return names
	}
	for _, info := range servers {
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
	return names
}

// validateSessionAgentType 检查新定义是否仍能打开这条记录原有的历史。
func validateSessionAgentType(record defs.Session, definition *agent.AgentDefinition) error {
	stored := defs.AgentTypeNative
	if record.External != nil {
		stored = record.External.AgentType
	}
	configured := cmp.Or(definition.Type, defs.AgentTypeNative)
	if stored != configured {
		return fmt.Errorf("agent %s type changed from %s to %s", record.AgentName, stored, configured)
	}
	return nil
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
		if s.ParentTurnID == "" {
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
func newWaitAgentResult(items []*defs.Item) WaitAgentResult {
	// 1. 扫描消息来源，记下有没有用户输入、子任务通知，以及去重后的 Agent SessionID。
	result := WaitAgentResult{Reason: waitReasonAgentMessage}
	hasUser, hasNotification := false, false
	var texts []string
	for _, item := range items {
		if item.Message == nil || item.Message.From == nil {
			continue
		}
		switch item.Message.From.Kind {
		case defs.IdentityUser:
			hasUser = true
		case defs.IdentityAgent:
			if sender := item.Message.AgentSender; sender != nil && sender.DetachedConversationID != "" {
				result.DetachedConversationID = sender.DetachedConversationID
				result.DetachedSessionID = sender.DetachedSessionID
			}
			if n := item.Message.SubagentNotification; n != nil {
				hasNotification = true
				if n.Status == defs.TurnStatusCompleted {
					for _, block := range item.Message.Content {
						if block.Text != nil {
							texts = append(texts, block.Text.Text)
						}
					}
				}
			}
			if from := defs.SessionID(item.Message.From.ID); !slices.Contains(result.From, from) {
				result.From = append(result.From, from)
			}
		}
	}
	result.Text = strings.Join(texts, "\n")
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

// transformHistoryItem 移除协作历史中的加密推理和内联图片数据。
func transformHistoryItem(item *defs.Item) *defs.Item {
	transformed := item.Clone()
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
	return transformed
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

// getDefaultExecutorID 返回配置里的首个执行环境 ID。
func getDefaultExecutorID(environments []executor.ExecutionEnvironment) string {
	if len(environments) == 0 {
		return ""
	}
	return environments[0].GetID()
}

// newExecutorPromptEntries 按稳定执行环境生成系统提示里的 Executor 条目。
func newExecutorPromptEntries(environments []executor.ExecutionEnvironment) []prompt.ExecutorEntry {
	// 1. 按环境顺序填写 ID、说明和工作区根目录，第一个仍是默认 Executor。
	entries := make([]prompt.ExecutorEntry, 0, len(environments))
	for _, environment := range environments {
		if environment.IsDynamic() {
			continue
		}
		directory := ""
		if environmentWorkspace := environment.GetWorkspace(); environmentWorkspace != nil {
			directory = environmentWorkspace.GetRootDirectory()
		}
		entries = append(entries, prompt.ExecutorEntry{
			ID:                 environment.GetID(),
			Description:        environment.GetDescription(),
			WorkspaceDirectory: directory,
		})
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

// hasSessionLabels 会话是否包含全部指定标签，包括值为空字符串的标签？
func hasSessionLabels(labels, required map[string]string) bool {
	for k, v := range required {
		if value, exists := labels[k]; !exists || value != v {
			return false
		}
	}
	return true
}

// renderChatHistorySummary 将选中轮次的消息及工具结果转换成另一后端可读的文本。
func renderChatHistorySummary(items []*defs.Item) string {
	var lines []string
	names := make(map[string]string)
	for _, item := range items {
		switch item.Kind {
		case defs.ItemMessage:
			label := "User"
			if item.ModelCallID != "" {
				label = "Assistant"
			}
			for _, c := range item.Message.Content {
				if c.Text != nil {
					lines = append(lines, "["+label+"] "+c.Text.Text)
				}
				if c.File != nil {
					lines = append(lines, "["+label+"] "+c.File.Path)
				}
			}
		case defs.ItemContextSummary:
			lines = append(lines, item.ContextSummary.Text)
		case defs.ItemToolCall:
			names[item.ToolCall.CallID] = item.ToolCall.Name
			lines = append(lines, "["+item.ToolCall.Name+"] "+item.ToolCall.Arguments)
		case defs.ItemToolResult:
			result := item.ToolResult
			text := string(result.Content)
			if result.Error != nil {
				text = result.Error.Message
			}
			lines = append(lines, "["+cmp.Or(names[result.CallID], "Tool")+"] "+text)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return prompt.FormatChatHistorySummary(strings.Join(lines, "\n"))
}

// NewSessionDetachedError 告知调用方转出后的对话和新根会话 ID。
func NewSessionDetachedError(target defs.Conversation) error {
	return fmt.Errorf("session detached to conversation %s, new session %s. This session has left the conversation. You can no longer operate on it here. To continue, use the new conversation.", target.ID, target.RootSessionID)
}
