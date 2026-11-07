package runtime

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/hook"
	"github.com/VaalaCat/openvaala/pkg/skill"
)

// getTitleFromInput 从用户原始正文生成对话标题。
func getTitleFromInput(content []*defs.Content) string {
	// 1. 跳过附带的上下文材料，取第一行非空正文，合并连续空白后截取前 40 个 Unicode 字符。
	for _, block := range content {
		if block.Kind != defs.ContentText || block.Text.IsContext {
			continue
		}
		for _, line := range strings.Split(block.Text.Text, "\n") {
			if words := strings.Fields(line); len(words) > 0 {
				runes := []rune(strings.Join(words, " "))
				return string(runes[:min(len(runes), 40)])
			}
		}
	}
	return ""
}

// newRuntimeDefinitions 整理对话使用的定义和模型索引。
func newRuntimeDefinitions(source *Definitions) *runtimeDefinitions {
	// 1. 没有 ExecutorProvider 时换成空列表。
	executors := source.Executors
	if executors == nil {
		executors = executor.NewConfigExecutorProvider(nil)
	}
	// 2. 复制各类定义，用这份 hook 定义新建 Hooks，并创建空的模型索引。
	definitions := &runtimeDefinitions{
		conversationDefinitions: &conversation.ConversationDefinitions{
			Agents:     slices.Clone(source.Agents),
			Models:     slices.Clone(source.Models),
			MCPServers: slices.Clone(source.MCPServers),
			Hooks:      hook.NewHooks(hook.NewHooksOpt(slices.Clone(source.Hooks))),
			Executors:  executors,
			Skills:     slices.Clone(source.Skills),
		},
		modelsByRef:        make(map[defs.ModelRef]*defs.ModelDefinition, len(source.Models)),
		agentDefaultModels: make(map[string]defs.ModelRef, len(source.Agents)),
	}
	// 3. 按模型引用和 Agent 名称登记模型。
	for _, model := range source.Models {
		definitions.modelsByRef[model.Ref] = model
	}
	for _, definition := range source.Agents {
		definitions.agentDefaultModels[definition.Name] = definition.Model
	}
	return definitions
}

// isSameExecutorSettings 两份 Executor 设置是否相同？
func isSameExecutorSettings(a, b defs.ExecutorSettings) bool {
	// 1. 比较默认 Executor、可用列表和工作目录；nil 与空列表、空目录表算相同。
	return a.DefaultExecutorID == b.DefaultExecutorID && slices.Equal(a.ExecutorIDs, b.ExecutorIDs) &&
		maps.Equal(a.WorkspaceDirectories, b.WorkspaceDirectories)
}

// newSkillInfos 取出各 skill 的展示字段。
func newSkillInfos(skills []*skill.Skill) []skill.SkillInfo {
	// 1. 按原顺序提取名称、说明、路径和 Executor。
	infos := make([]skill.SkillInfo, 0, len(skills))
	for _, item := range skills {
		infos = append(infos, skill.SkillInfo{
			Name:        item.Name,
			Description: item.Description,
			Path:        item.Path,
			Executor:    item.Executor,
		})
	}
	return infos
}

// newAgentInfos 按定义的顺序取出各 Agent 的名称、说明和默认模型。
func newAgentInfos(agents []*agent.AgentDefinition) []AgentInfo {
	// 1. 默认模型未指定时保持零值，序列化时省略。
	infos := make([]AgentInfo, len(agents))
	for index, definition := range agents {
		infos[index] = AgentInfo{Name: definition.Name, Description: definition.Description, DefaultModel: definition.Model}
	}
	return infos
}

// newExecutorDetails 按列表顺序取出各 Executor 的种类、说明和默认根目录。
func newExecutorDetails(executors []executor.Executor) []conversation.ExecutorDetails {
	// 1. 按列表顺序提取 Executor 的标识、种类、说明和工作目录。
	details := make([]conversation.ExecutorDetails, len(executors))
	for index, listed := range executors {
		definition := listed.GetDefinition()
		details[index] = conversation.ExecutorDetails{
			ID:                 definition.ID,
			Kind:               definition.Kind,
			Description:        definition.Description,
			WorkspaceDirectory: definition.WorkspaceDirectory,
		}
	}
	return details
}

// getEventSessionID 返回对话事件所属的 SessionID。
func getEventSessionID(event conversation.ConversationEvent) defs.SessionID {
	// 1. 从 Session 启动、更新或停止事件中取出 SessionID。
	switch event := event.(type) {
	case *conversation.ConversationSessionStarted:
		return event.Session.ID
	case *conversation.ConversationSessionEvent:
		return event.SessionID
	case *conversation.ConversationSessionStopped:
		return event.SessionID
	}
	panic(fmt.Sprintf("unsupported Conversation event %T", event))
}

// newConversationStoppedError 包装对话退出原因。
func newConversationStoppedError(conversationID defs.ConversationID, reason error) error {
	// 1. Conversation.Run 返回 nil 时只说明已退出，有退出原因时一并包装。
	if reason == nil {
		return fmt.Errorf("Conversation %s has stopped", conversationID)
	}
	return fmt.Errorf("Conversation %s has stopped: %w", conversationID, reason)
}

// countConversationMessages 统计根 Session 已保存 Item 中的对话消息数。
func countConversationMessages(items []dao.LoggedItem) int {
	// 1. 按 defs.IsConversationMessage 累加用户消息和主 Agent 回复的条数。
	count := 0
	for _, logged := range items {
		if defs.IsConversationMessage(logged.Item) {
			count++
		}
	}
	return count
}

// getArchivedConversation 返回归档状态更新后的记录，以及记录是否变化。
func getArchivedConversation(record defs.Conversation, archived bool) (archivedRecord defs.Conversation, isChanged bool) {
	// 1. 状态相同时返回原记录和 false。
	if (record.ArchivedAt != 0) == archived {
		return record, false
	}
	// 2. 归档写当前 Unix 毫秒，取消归档写 0。
	record.ArchivedAt = 0
	if archived {
		record.ArchivedAt = time.Now().UnixMilli()
	}
	return record, true
}
