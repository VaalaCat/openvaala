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
	"github.com/VaalaCat/openvaala/pkg/skill"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/workspace"
)

// newConversationConfig 装配一套对话配置。
func newConversationConfig(ctx context.Context, settings, storedSettings defs.ExecutorSettings, opt *ConversationOpt,
	definitions *ConversationDefinitions, sessionRecords []defs.Session) (*conversationConfig, error) {
	// 1. 保存设置和定义，装配配置；失败时关闭已经建好的 SSH 工作区。
	config := &conversationConfig{
		settings:    settings,
		definitions: definitions,
	}
	if err := assembleConversationConfig(ctx, config, storedSettings, opt, sessionRecords); err != nil {
		closeSSHWorkspaces(ctx, config.sshWorkspaces)
		return nil, err
	}
	return config, nil
}

// assembleConversationConfig 装配执行环境和 Skill，校验 Agent 和模型，确认各 Session 用的 Agent 都在目录里。
func assembleConversationConfig(ctx context.Context, config *conversationConfig, storedSettings defs.ExecutorSettings,
	opt *ConversationOpt, sessionRecords []defs.Session) error {
	// 1. 校验定义；非临时对话继续装配执行环境和 Skill。
	if err := validateConversationDefinitions(config.definitions); err != nil {
		return err
	}
	if !opt.Ephemeral {
		if err := assembleExecutors(ctx, config, storedSettings, opt); err != nil {
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

// assembleExecutors 按设置创建执行环境并读取 Skill。
func assembleExecutors(ctx context.Context, config *conversationConfig, storedSettings defs.ExecutorSettings, opt *ConversationOpt) error {
	// 1. 补全 local 目录，校验 Executor 设置。
	executors := config.definitions.Executors
	config.settings = getFilledExecutorSettings(config.settings, executors)
	if err := validateExecutorSettings(ctx, config.settings, storedSettings, executors); err != nil {
		return err
	}
	// 2. 按可用 Executor 的顺序创建工作区和执行环境，默认 Executor 排在最前；SSH 工作区另记一份。
	usedServers := getUsedMCPServers(config.definitions.Agents, config.definitions.MCPServers)
	for _, definition := range getAvailableExecutors(config.settings, executors) {
		rootDirectory := cmp.Or(config.settings.WorkspaceDirectories[definition.ID], definition.WorkspaceDirectory)
		executorWorkspace, err := NewExecutorWorkspace(ctx, definition, rootDirectory, opt.Environment)
		if err != nil {
			return fmt.Errorf("executor %q: %w", definition.ID, err)
		}
		if sshWorkspace, ok := executorWorkspace.(workspace.SSHWorkspace); ok {
			config.sshWorkspaces = append(config.sshWorkspaces, sshWorkspace)
		}
		environment, err := newExecutionEnvironment(definition, executorWorkspace, opt.MaxImageBytes, usedServers)
		if err != nil {
			return fmt.Errorf("executor %q: %w", definition.ID, err)
		}
		config.environments = append(config.environments, environment)
	}
	// 3. 读取默认工作区的 Skill，合并共享 Skill 后保存。
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
	workspaceTools, err := newWorkspaceTools(executorWorkspace, maxImageBytes)
	if err != nil {
		return nil, err
	}
	return executor.NewExecutionEnvironment(
		executor.NewExecutionEnvironmentOpt(definition.ID, definition.Description, executorWorkspace, workspaceTools...).WithMCPServers(usedServers))
}

// NewExecutorWorkspace 按 Executor 类型创建工作区；SSH 工作区首次使用时才连接，用完由调用方关闭。
func NewExecutorWorkspace(ctx context.Context, definition *defs.ExecutorDefinition, rootDirectory string, environment []string) (workspace.Workspace, error) {
	// 1. virtual 用定义里的目录和文件，SSH 用指定目录且首次使用时才连接，local 用指定目录和命令环境变量。
	switch definition.Kind {
	case defs.ExecutorKindVirtual:
		return workspace.NewVirtualWorkspace(workspace.NewVirtualWorkspaceOpt(definition.WorkspaceDirectory, getVirtualFiles(definition.Files)))
	case defs.ExecutorKindSSH:
		sshOpt := workspace.NewSSHWorkspaceOpt(rootDirectory, definition.SSH.Address, definition.SSH.IdentityFile)
		sshOpt.Telemetry = telemetry.GetTelemetry(ctx)
		return workspace.NewSSHWorkspace(sshOpt)
	}
	return workspace.NewLocalWorkspace(workspace.NewLocalWorkspaceOpt(rootDirectory).WithEnvironment(environment))
}

// newWorkspaceTools 创建同一工作区的 read_file 和 apply_patch 工具。
func newWorkspaceTools(executorWorkspace workspace.Workspace, maxImageBytes int) ([]executor.Tool, error) {
	// 1. 创建带图片大小限制的读文件工具和修改文件工具。
	readFileTool, err := tools.NewReadFileTool(tools.NewReadFileToolOpt(executorWorkspace).WithMaxImageBytes(maxImageBytes))
	if err != nil {
		return nil, err
	}
	applyPatchTool, err := tools.NewApplyPatchTool(tools.NewApplyPatchToolOpt(executorWorkspace))
	if err != nil {
		return nil, err
	}
	return []executor.Tool{readFileTool, applyPatchTool}, nil
}

// validateExecutorSettings 校验 Executor ID 和默认 Executor 的可用范围。
func validateExecutorSettings(ctx context.Context, settings, storedSettings defs.ExecutorSettings, executors []*defs.ExecutorDefinition) error {
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
	// 1. 补上 local 目录，选出默认 Executor；没有可用 Executor 时只返回共享 Skill。
	settings = getFilledExecutorSettings(settings, definitions.Executors)
	available := getAvailableExecutors(settings, definitions.Executors)
	if len(available) == 0 {
		return mergeSkills(definitions.Skills), nil
	}
	// 2. 创建默认工作区读取 Skill，返回时关闭临时的 SSH 工作区。
	defaultExecutor := available[0]
	rootDirectory := cmp.Or(settings.WorkspaceDirectories[defaultExecutor.ID], defaultExecutor.WorkspaceDirectory)
	defaultWorkspace, err := NewExecutorWorkspace(ctx, defaultExecutor, rootDirectory, nil)
	if err != nil {
		return nil, fmt.Errorf("executor %q: %w", defaultExecutor.ID, err)
	}
	defer closeWorkspace(ctx, defaultWorkspace)
	return loadSkills(ctx, defaultExecutor.ID, defaultWorkspace, definitions.Skills)
}

// ResolveWorkspaceDirectory 解析 Executor 所在机器上的工作区目录。
func ResolveWorkspaceDirectory(ctx context.Context, executors []*defs.ExecutorDefinition, executorID, directory, baseDirectory string) (string, error) {
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
func GetWorkspaceDirectories(ctx context.Context, executors []*defs.ExecutorDefinition,
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

// newDirectoryWorkspace 校验可换目录的 Executor 并创建临时工作区。
func newDirectoryWorkspace(ctx context.Context, executors []*defs.ExecutorDefinition, executorID, baseDirectory string) (workspace.Workspace, error) {
	// 1. 查找 Executor；未知 ID 或 virtual 类型返回 ErrInvalidExecutorSettings。
	index := slices.IndexFunc(executors, func(definition *defs.ExecutorDefinition) bool { return definition.ID == executorID })
	if index < 0 {
		return nil, fmt.Errorf("%w: unknown executor %q", ErrInvalidExecutorSettings, executorID)
	}
	definition := executors[index]
	if definition.Kind == defs.ExecutorKindVirtual {
		return nil, fmt.Errorf("%w: executor %q does not support changing directory", ErrInvalidExecutorSettings, executorID)
	}
	// 2. 以 baseDirectory（为空时用定义里的根目录）为根创建临时工作区。
	return NewExecutorWorkspace(ctx, definition, cmp.Or(baseDirectory, definition.WorkspaceDirectory), nil)
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

// closeSSHWorkspaces 关闭对话创建的 SSH 工作区。
func closeSSHWorkspaces(ctx context.Context, sshWorkspaces []workspace.SSHWorkspace) {
	// 1. 逐个关闭 SSH 工作区，记录关闭错误。
	for _, sshWorkspace := range sshWorkspaces {
		if err := sshWorkspace.Close(); err != nil {
			telemetry.GetLogger(ctx).Warn("close ssh workspace failed", "error", err)
		}
	}
}

// closeWorkspace 临时工作区是 SSH 工作区时关闭它的连接。
func closeWorkspace(ctx context.Context, temporaryWorkspace workspace.Workspace) {
	// 1. 工作区是 SSH 类型时关闭它。
	if sshWorkspace, ok := temporaryWorkspace.(workspace.SSHWorkspace); ok {
		closeSSHWorkspaces(ctx, []workspace.SSHWorkspace{sshWorkspace})
	}
}
