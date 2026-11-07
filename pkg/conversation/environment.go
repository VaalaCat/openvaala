package conversation

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/executor/tools"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
	"github.com/VaalaCat/openvaala/pkg/prompt"
	"github.com/VaalaCat/openvaala/pkg/skill"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/workspace"
)

// newConversationConfig 装配一套对话配置。
func newConversationConfig(ctx context.Context, conversationID defs.ConversationID, settings, storedSettings defs.ExecutorSettings, opt *ConversationOpt,
	definitions *ConversationDefinitions, sessionRecords []defs.Session) (*conversationConfig, error) {
	// 1. 保存设置和定义，装配配置；失败时关闭已经建好的工作区。
	config := &conversationConfig{
		settings:    settings,
		definitions: definitions,
	}
	if err := assembleConversationConfig(ctx, conversationID, config, storedSettings, opt, sessionRecords); err != nil {
		closeWorkspaces(ctx, config.workspaces)
		return nil, err
	}
	return config, nil
}

// assembleConversationConfig 装配执行环境和 Skill，校验 Agent 和模型，确认各 Session 用的 Agent 都在目录里。
func assembleConversationConfig(ctx context.Context, conversationID defs.ConversationID, config *conversationConfig, storedSettings defs.ExecutorSettings,
	opt *ConversationOpt, sessionRecords []defs.Session) error {
	// 1. 校验定义；需要 Executor 的对话继续装配执行环境和 Skill。
	if err := validateConversationDefinitions(config.definitions); err != nil {
		return err
	}
	if !opt.WithoutExecutors {
		if err := assembleExecutors(ctx, conversationID, config, storedSettings, opt); err != nil {
			return err
		}
	}
	// 2. 校验 Agent 和模型，确认每个 Session 用的 Agent 都在目录里。
	agentDefinitions, models, err := validateConversationConfig(ctx, config.definitions, config.skills, config.environments)
	if err != nil {
		return err
	}
	for _, sessionRecord := range sessionRecords {
		if _, found := agentDefinitions[sessionRecord.AgentName]; !found {
			return newAgentNotConfiguredError(sessionRecord)
		}
	}
	config.agentDefinitions, config.models = agentDefinitions, models
	return nil
}

// assembleExecutors 向 ExecutorProvider 要本对话的 Executor，按设置打开工作区、创建执行环境并读取 Skill。
func assembleExecutors(ctx context.Context, conversationID defs.ConversationID, config *conversationConfig, storedSettings defs.ExecutorSettings, opt *ConversationOpt) error {
	// 1. 向 ExecutorProvider 要本对话的列表，memory 的 ID 和种类对不上时返回 ErrInvalidExecutorSettings，补全 local 目录，校验 Executor 设置。
	executors, err := config.definitions.Executors.GetExecutors(ctx, conversationID)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(executors, func(listed executor.Executor) bool {
		definition := listed.GetDefinition()
		return (definition.ID == defs.MemoryExecutorID) != (definition.Kind == defs.ExecutorKindMemory)
	}) {
		return fmt.Errorf("%w: memory executor requires id %q and kind %q", ErrInvalidExecutorSettings, defs.MemoryExecutorID, defs.ExecutorKindMemory)
	}
	config.executors = executors
	config.settings = getFilledExecutorSettings(config.settings, executors)
	if err := validateExecutorSettings(ctx, config.settings, storedSettings, executors); err != nil {
		return err
	}
	// 2. 按可用 Executor 的顺序打开工作区并创建执行环境，默认 Executor 排在最前；每个工作区另记一份；memory 要用到默认 Executor，留到第 3 步。
	usedServers := getUsedMCPServers(config.definitions.Agents, config.definitions.MCPServers)
	for _, availableExecutor := range getAvailableExecutors(config.settings, executors) {
		definition := availableExecutor.GetDefinition()
		if definition.Kind == defs.ExecutorKindMemory {
			continue
		}
		executorWorkspace, err := availableExecutor.NewWorkspace(ctx, &executor.NewWorkspaceRequest{
			RootDirectory: cmp.Or(config.settings.WorkspaceDirectories[definition.ID], definition.WorkspaceDirectory),
			Environment:   opt.Environment,
		})
		if err != nil {
			return fmt.Errorf("executor %q: %w", definition.ID, err)
		}
		config.workspaces = append(config.workspaces, executorWorkspace)
		environment, err := newExecutionEnvironment(definition, executorWorkspace, opt.MaxImageBytes, usedServers)
		if err != nil {
			return fmt.Errorf("executor %q: %w", definition.ID, err)
		}
		config.environments = append(config.environments, environment)
	}
	// 3. memory 在这个对话里可用时，用默认环境算出项目名，打开 memory 工作区并建执行环境，排在最后，不影响默认 Executor。
	memoryIndex := slices.IndexFunc(executors, func(listed executor.Executor) bool { return listed.GetDefinition().Kind == defs.ExecutorKindMemory })
	if memoryIndex >= 0 && len(config.environments) > 0 && config.settings.IsExecutorAvailable(defs.MemoryExecutorID) {
		defaultEnvironment := config.environments[0]
		memoryWorkspace, err := executors[memoryIndex].NewWorkspace(ctx, &executor.NewWorkspaceRequest{
			MemoryProjectName: getMemoryProjectName(defaultEnvironment.GetID(), defaultEnvironment.GetWorkspace().GetRootDirectory()),
		})
		if err != nil {
			return fmt.Errorf("executor %q: %w", defs.MemoryExecutorID, err)
		}
		config.workspaces = append(config.workspaces, memoryWorkspace)
		memoryEnvironment, err := newMemoryEnvironment(memoryWorkspace, opt.MaxImageBytes)
		if err != nil {
			return fmt.Errorf("executor %q: %w", defs.MemoryExecutorID, err)
		}
		config.environments = append(config.environments, memoryEnvironment)
	}
	// 4. 读取默认工作区的 Skill，合并共享 Skill 后保存。
	var defaultExecutorID string
	var defaultWorkspace workspace.Workspace
	if len(config.environments) > 0 {
		defaultExecutorID, defaultWorkspace = config.environments[0].GetID(), config.environments[0].GetWorkspace()
	}
	skills, err := loadSkills(ctx, defaultExecutorID, defaultWorkspace, config.definitions.Skills)
	if err != nil {
		return err
	}
	config.skills = skills
	return nil
}

// newExecutionEnvironment 用工作区、文件工具和 MCP server 定义创建执行环境。
func newExecutionEnvironment(definition *defs.ExecutorDefinition, executorWorkspace workspace.Workspace, maxImageBytes int,
	usedServers []*mcpclient.ServerDefinition) (executor.ExecutionEnvironment, error) {
	// 1. 创建文件工具，连同 Agent 用到的 MCP server 定义创建执行环境；执行环境只为分给自己的 server 建连接。
	workspaceTools, err := newWorkspaceTools(executorWorkspace, maxImageBytes, definition.Kind == defs.ExecutorKindVirtual)
	if err != nil {
		return nil, err
	}
	return executor.NewExecutionEnvironment(
		executor.NewExecutionEnvironmentOpt(definition.ID, definition.Description, executorWorkspace, workspaceTools...).WithMCPServers(usedServers))
}

// newMemoryEnvironment 校验 memory 工作区并创建执行环境：只读写文件，不跑命令，不挂 MCP。
func newMemoryEnvironment(memoryWorkspace workspace.Workspace, maxImageBytes int) (executor.ExecutionEnvironment, error) {
	// 1. 工作区不是 MemoryWorkspace 时返回错误。
	if _, isMemoryWorkspace := memoryWorkspace.(workspace.MemoryWorkspace); !isMemoryWorkspace {
		return nil, errors.New("workspace is not a MemoryWorkspace")
	}
	// 2. 创建只遍历工作区的文件工具，建不能跑命令的执行环境。
	memoryTools, err := newWorkspaceTools(memoryWorkspace, maxImageBytes, true)
	if err != nil {
		return nil, err
	}
	return executor.NewExecutionEnvironment(
		executor.NewExecutionEnvironmentOpt(defs.MemoryExecutorID, prompt.MemoryExecutorDescription, memoryWorkspace, memoryTools...).WithoutCommands())
}

// newWorkspaceTools 创建同一工作区的 read_file、apply_patch、glob 和 grep 工具；isWalkOnly 为 true 时搜索只遍历工作区，不启动进程。
func newWorkspaceTools(executorWorkspace workspace.Workspace, maxImageBytes int, isWalkOnly bool) ([]executor.Tool, error) {
	// 1. 创建带图片大小限制的读文件工具、修改文件工具和共用一个搜索器的两个搜索工具。
	readFileTool, err := tools.NewReadFileTool(tools.NewReadFileToolOpt(executorWorkspace).WithMaxImageBytes(maxImageBytes))
	if err != nil {
		return nil, err
	}
	applyPatchTool, err := tools.NewApplyPatchTool(tools.NewApplyPatchToolOpt(executorWorkspace))
	if err != nil {
		return nil, err
	}
	searchOpt := tools.NewSearchToolsOpt(executorWorkspace)
	if isWalkOnly {
		searchOpt.WithWalkOnly()
	}
	searchTools, err := tools.NewSearchTools(searchOpt)
	if err != nil {
		return nil, err
	}
	return append([]executor.Tool{readFileTool, applyPatchTool}, searchTools...), nil
}

// validateExecutorSettings 校验 Executor ID、默认 Executor 的可用范围，以及 memory 的限制。
func validateExecutorSettings(ctx context.Context, settings, storedSettings defs.ExecutorSettings, executors []executor.Executor) error {
	// 1. 未知 ID 已在旧记录里时只记警告；新加的未知 ID 返回 ErrInvalidExecutorSettings。
	for _, id := range getUnknownExecutorIDs(settings, executors) {
		isStored := id == storedSettings.DefaultExecutorID ||
			(id != settings.DefaultExecutorID && slices.Contains(storedSettings.ExecutorIDs, id))
		if !isStored {
			return fmt.Errorf("%w: unknown executor %q", ErrInvalidExecutorSettings, id)
		}
		telemetry.GetLogger(ctx).Warn("executor in conversation record not found", "openvaala.executor.id", id)
	}
	// 2. 默认 Executor 不在可用范围里时返回 ErrInvalidExecutorSettings。
	if settings.DefaultExecutorID != "" && !settings.IsExecutorAvailable(settings.DefaultExecutorID) {
		return fmt.Errorf("%w: default executor %q is not in executor_ids", ErrInvalidExecutorSettings, settings.DefaultExecutorID)
	}
	// 3. memory 不能当默认 Executor，也不能设目录；可用的 Executor 里除了 memory 还要有别的，项目记忆才有默认 Executor 可对应。
	if settings.DefaultExecutorID == defs.MemoryExecutorID {
		return fmt.Errorf("%w: memory executor cannot be the default", ErrInvalidExecutorSettings)
	}
	if _, found := settings.WorkspaceDirectories[defs.MemoryExecutorID]; found {
		return fmt.Errorf("%w: memory executor has no workspace directory", ErrInvalidExecutorSettings)
	}
	available := getAvailableExecutors(settings, executors)
	if len(available) > 0 && !slices.ContainsFunc(available, func(listed executor.Executor) bool { return listed.GetDefinition().Kind != defs.ExecutorKindMemory }) {
		return fmt.Errorf("%w: no executor can be the default", ErrInvalidExecutorSettings)
	}
	return nil
}

// checkDefaultExecutor 检查默认工作区的根目录，返回所在机器的用户主目录。
func checkDefaultExecutor(ctx context.Context, defaultWorkspace workspace.Workspace) (string, error) {
	// 1. 读取根目录元数据，访问失败或不是目录时返回错误。
	rootDirectory := defaultWorkspace.GetRootDirectory()
	metadata, err := defaultWorkspace.GetMetadata(ctx, rootDirectory, true)
	if err != nil {
		return "", err
	}
	if !metadata.IsDirectory {
		return "", fmt.Errorf("%s is not a directory", rootDirectory)
	}
	// 2. 查询用户主目录；工作区不支持主目录时返回空字符串。
	homeDirectory, err := defaultWorkspace.GetHomeDirectory(ctx)
	if errors.Is(err, errors.ErrUnsupported) {
		return "", nil
	}
	return homeDirectory, err
}

// loadSkills 读取并合并对话可用的 Skill。
func loadSkills(ctx context.Context, defaultExecutorID string, defaultWorkspace workspace.Workspace, shared []*skill.Skill) ([]*skill.Skill, error) {
	// 1. 没有默认工作区时只返回共享 Skill，否则检查默认工作区的根目录并取得主目录。
	if defaultWorkspace == nil {
		return mergeSkills(shared), nil
	}
	homeDirectory, err := checkDefaultExecutor(ctx, defaultWorkspace)
	if err != nil {
		return nil, fmt.Errorf("executor %q: %w", defaultExecutorID, err)
	}
	// 2. 从默认工作区的项目目录和主目录读取 .agents/skills、.claude/skills。
	projectSkills, err := skill.LoadSkills(ctx, getSkillDirectories(defaultExecutorID, defaultWorkspace, defaultWorkspace.GetRootDirectory()))
	if err != nil {
		return nil, err
	}
	var homeSkills []*skill.Skill
	if homeDirectory != "" {
		if homeSkills, err = skill.LoadSkills(ctx, getSkillDirectories(defaultExecutorID, defaultWorkspace, homeDirectory)); err != nil {
			return nil, err
		}
	}
	// 3. 按项目、共享、主目录的顺序去重，同名保留第一项，再按名称排序。
	return mergeSkills(projectSkills, shared, homeSkills), nil
}

// LoadDraftSkills 为还没创建的草稿对话读取可用的 Skill：按设置建临时的默认工作区，读完关闭。
func LoadDraftSkills(ctx context.Context, settings defs.ExecutorSettings, definitions *ConversationDefinitions) ([]*skill.Skill, error) {
	// 1. 向 ExecutorProvider 要没有对话时的列表，补上 local 目录，选出默认 Executor（memory 不算）；没有可用 Executor 时只返回共享 Skill。
	executors, err := definitions.Executors.GetExecutors(ctx, "")
	if err != nil {
		return nil, err
	}
	settings = getFilledExecutorSettings(settings, executors)
	available := slices.DeleteFunc(getAvailableExecutors(settings, executors), func(listed executor.Executor) bool {
		return listed.GetDefinition().Kind == defs.ExecutorKindMemory
	})
	if len(available) == 0 {
		return mergeSkills(definitions.Skills), nil
	}
	// 2. 打开默认工作区读取 Skill，返回时关闭。
	defaultExecutor := available[0]
	definition := defaultExecutor.GetDefinition()
	defaultWorkspace, err := defaultExecutor.NewWorkspace(ctx, &executor.NewWorkspaceRequest{
		RootDirectory: cmp.Or(settings.WorkspaceDirectories[definition.ID], definition.WorkspaceDirectory),
	})
	if err != nil {
		return nil, fmt.Errorf("executor %q: %w", definition.ID, err)
	}
	defer closeWorkspace(ctx, defaultWorkspace)
	return loadSkills(ctx, definition.ID, defaultWorkspace, definitions.Skills)
}

// ResolveWorkspaceDirectory 解析 Executor 所在机器上的工作区目录。
func ResolveWorkspaceDirectory(ctx context.Context, executors []executor.Executor, executorID, directory, baseDirectory string) (string, error) {
	// 1. 创建临时工作区，返回时关闭连接。
	directoryWorkspace, err := newDirectoryWorkspace(ctx, executors, executorID, baseDirectory)
	if err != nil {
		return "", err
	}
	defer closeWorkspace(ctx, directoryWorkspace)
	// 2. 在临时工作区上解析并校验目录。
	return resolveWorkspaceDirectory(ctx, directoryWorkspace, directory)
}

// GetWorkspaceDirectories 解析并列出 Executor 上一个目录的直接子目录，关闭本次临时工作区。
func GetWorkspaceDirectories(ctx context.Context, executors []executor.Executor,
	executorID, directory, baseDirectory string) (directories *WorkspaceDirectories, err error) {
	// 1. 为连接、路径解析、目录读取和关闭设置十秒期限。
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	directoryWorkspace, err := newDirectoryWorkspace(ctx, executors, executorID, baseDirectory)
	if err != nil {
		return nil, err
	}
	// 2. 取消时关闭本次独占的连接；正常返回也关闭，并等待取消回调结束。
	closed := make(chan struct{})
	stopClosing := context.AfterFunc(ctx, func() {
		closeWorkspace(ctx, directoryWorkspace)
		close(closed)
	})
	defer func() {
		if stopClosing() {
			closeWorkspace(ctx, directoryWorkspace)
		} else {
			<-closed
		}
		if ctx.Err() != nil {
			directories, err = nil, ctx.Err()
		}
	}()
	// 3. 在同一工作区上解析、校验并读取，只保留普通直接子目录。
	directory, err = resolveWorkspaceDirectory(ctx, directoryWorkspace, directory)
	if err != nil {
		return nil, err
	}
	entries, err := directoryWorkspace.ReadDirectory(ctx, directory)
	if err != nil {
		return nil, err
	}
	directories = &WorkspaceDirectories{Directory: directory, Directories: []string{}}
	for _, entry := range entries {
		if entry.IsDirectory {
			directories.Directories = append(directories.Directories, entry.Name)
		}
	}
	return directories, nil
}

// newDirectoryWorkspace 校验可换目录的 Executor 并打开临时工作区。
func newDirectoryWorkspace(ctx context.Context, executors []executor.Executor, executorID, baseDirectory string) (workspace.Workspace, error) {
	// 1. 查找 Executor；未知 ID、virtual 和 memory 类型返回 ErrInvalidExecutorSettings。
	index := slices.IndexFunc(executors, func(listed executor.Executor) bool { return listed.GetDefinition().ID == executorID })
	if index < 0 {
		return nil, fmt.Errorf("%w: unknown executor %q", ErrInvalidExecutorSettings, executorID)
	}
	definition := executors[index].GetDefinition()
	if definition.Kind == defs.ExecutorKindVirtual {
		return nil, fmt.Errorf("%w: executor %q does not support changing directory", ErrInvalidExecutorSettings, executorID)
	}
	if definition.Kind == defs.ExecutorKindMemory {
		return nil, fmt.Errorf("%w: memory executor has no workspace directory", ErrInvalidExecutorSettings)
	}
	// 2. 以 baseDirectory（为空时用定义里的根目录）为根打开临时工作区。
	return executors[index].NewWorkspace(ctx, &executor.NewWorkspaceRequest{RootDirectory: cmp.Or(baseDirectory, definition.WorkspaceDirectory)})
}

// resolveWorkspaceDirectory 在给定工作区上展开路径并校验目录。
func resolveWorkspaceDirectory(ctx context.Context, directoryWorkspace workspace.Workspace, directory string) (string, error) {
	// 1. 按 Executor 所在机器的主目录展开 ~，相对路径接到根目录后面，再清理 . 和 ..，得到绝对路径。
	switch {
	case directory == "~" || strings.HasPrefix(directory, "~/"):
		homeDirectory, err := directoryWorkspace.GetHomeDirectory(ctx)
		if err != nil {
			return "", err
		}
		directory = filepath.Join(homeDirectory, directory[1:])
	case !filepath.IsAbs(directory):
		directory = filepath.Join(directoryWorkspace.GetRootDirectory(), directory)
	}
	directory = filepath.Clean(directory)
	// 2. 确认路径存在且是目录；不存在或不是目录时返回 ErrInvalidExecutorSettings，其他访问错误原样返回。
	metadata, err := directoryWorkspace.GetMetadata(ctx, directory, true)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && !metadata.IsDirectory) {
		return "", fmt.Errorf("%w: not a directory: %s", ErrInvalidExecutorSettings, directory)
	}
	if err != nil {
		return "", err
	}
	return directory, nil
}

// closeWorkspaces 关闭对话创建的工作区。
func closeWorkspaces(ctx context.Context, workspaces []workspace.Workspace) {
	// 1. 逐个关闭工作区，记录关闭错误。
	for _, instance := range workspaces {
		if err := instance.Close(); err != nil {
			telemetry.GetLogger(ctx).Warn("close workspace failed", "error", err)
		}
	}
}

// closeWorkspace 关闭一个临时工作区。
func closeWorkspace(ctx context.Context, temporaryWorkspace workspace.Workspace) {
	// 1. 关闭工作区，记录关闭错误。
	closeWorkspaces(ctx, []workspace.Workspace{temporaryWorkspace})
}
