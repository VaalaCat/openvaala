package executor

import (
	"context"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/workspace"
)

// ExecutorProvider 提供一个对话能用的 Executor。
type ExecutorProvider interface {
	// GetExecutors 按顺序返回对话能用的 Executor。
	// conversationID 为空表示还没有对话，比如新对话页和不属于对话的终端；可能被并发调用；返回的列表不含 nil。
	GetExecutors(ctx context.Context, conversationID defs.ConversationID) (executors []Executor, err error)
}

// Executor 是一个能干活的地方，负责打开自己的工作区。可以并发调用。
type Executor interface {
	// GetDefinition 返回 Executor 的定义，不返回 nil。
	GetDefinition() (definition *defs.ExecutorDefinition)
	// NewWorkspace 打开一个新的工作区；没有错误时返回的工作区不为 nil，调用方用完后调 Close。
	NewWorkspace(ctx context.Context, request *NewWorkspaceRequest) (executorWorkspace workspace.Workspace, err error)
}

// NewWorkspaceRequest 是打开工作区的请求。
type NewWorkspaceRequest struct {
	// RootDirectory 是工作区根目录；virtual 和 memory 不使用。
	RootDirectory string
	// Environment 是客户端的环境变量，格式同 os.Environ，只有 local 用；nil 时继承 serve 进程。
	Environment []string
	// MemoryProjectName 是 memory 工作区的项目记忆目录，相对记忆根目录。
	MemoryProjectName string
}

// configExecutorProviderImpl 对所有对话返回同一组 Executor。
type configExecutorProviderImpl struct {
	executors []Executor
}

// NewConfigExecutorProvider 用固定的 Executor 列表创建 ExecutorProvider。
func NewConfigExecutorProvider(executors []Executor) ExecutorProvider {
	// 1. 保存 Executor 列表。
	return &configExecutorProviderImpl{executors: executors}
}

// GetExecutors 按传入顺序返回 Executor 列表。
func (p *configExecutorProviderImpl) GetExecutors(ctx context.Context, conversationID defs.ConversationID) ([]Executor, error) {
	// 1. 不区分对话，直接返回列表。
	return p.executors, nil
}

// localExecutorImpl 是本机 Executor。
type localExecutorImpl struct {
	definition *defs.ExecutorDefinition
}

// NewLocalExecutor 创建本机 Executor。
func NewLocalExecutor(definition *defs.ExecutorDefinition) Executor {
	// 1. 保存定义。
	return &localExecutorImpl{definition: definition}
}

// GetDefinition 返回 Executor 的定义。
func (e *localExecutorImpl) GetDefinition() *defs.ExecutorDefinition {
	return e.definition
}

// NewWorkspace 以请求的根目录和命令环境变量打开本机工作区。
func (e *localExecutorImpl) NewWorkspace(ctx context.Context, request *NewWorkspaceRequest) (workspace.Workspace, error) {
	// 1. 创建本机工作区。
	return workspace.NewLocalWorkspace(workspace.NewLocalWorkspaceOpt(request.RootDirectory).WithEnvironment(request.Environment))
}

// SSHExecutorOpt 是 SSH Executor 的配置。
type SSHExecutorOpt struct {
	Definition   *defs.ExecutorDefinition
	Address      string
	IdentityFile string
}

// NewSSHExecutorOpt 保存 SSH Executor 的定义和连接参数。
func NewSSHExecutorOpt(definition *defs.ExecutorDefinition, address, identityFile string) *SSHExecutorOpt {
	// 1. 创建配置。
	return &SSHExecutorOpt{Definition: definition, Address: address, IdentityFile: identityFile}
}

// sshExecutorImpl 是 SSH Executor。
type sshExecutorImpl struct {
	opt SSHExecutorOpt
}

// NewSSHExecutor 创建 SSH Executor。
func NewSSHExecutor(opt *SSHExecutorOpt) Executor {
	// 1. 保存配置。
	return &sshExecutorImpl{opt: *opt}
}

// GetDefinition 返回 Executor 的定义。
func (e *sshExecutorImpl) GetDefinition() *defs.ExecutorDefinition {
	return e.opt.Definition
}

// NewWorkspace 以请求的根目录打开 SSH 工作区；首次使用时才连接。
func (e *sshExecutorImpl) NewWorkspace(ctx context.Context, request *NewWorkspaceRequest) (workspace.Workspace, error) {
	// 1. 创建 SSH 工作区，遥测取自 ctx。
	sshOpt := workspace.NewSSHWorkspaceOpt(request.RootDirectory, e.opt.Address, e.opt.IdentityFile)
	sshOpt.Telemetry = telemetry.GetTelemetry(ctx)
	return workspace.NewSSHWorkspace(sshOpt)
}

// VirtualExecutorOpt 是 virtual Executor 的配置。
type VirtualExecutorOpt struct {
	Definition *defs.ExecutorDefinition
	// Files 是每个新工作区预先放入的文件，键为绝对路径。
	Files map[string][]byte
}

// NewVirtualExecutorOpt 保存 virtual Executor 的定义和预置文件。
func NewVirtualExecutorOpt(definition *defs.ExecutorDefinition, files map[string][]byte) *VirtualExecutorOpt {
	// 1. 创建配置。
	return &VirtualExecutorOpt{Definition: definition, Files: files}
}

// virtualExecutorImpl 是 virtual Executor。
type virtualExecutorImpl struct {
	opt VirtualExecutorOpt
}

// NewVirtualExecutor 创建 virtual Executor。
func NewVirtualExecutor(opt *VirtualExecutorOpt) Executor {
	// 1. 保存配置。
	return &virtualExecutorImpl{opt: *opt}
}

// GetDefinition 返回 Executor 的定义。
func (e *virtualExecutorImpl) GetDefinition() *defs.ExecutorDefinition {
	return e.opt.Definition
}

// NewWorkspace 以定义里的目录和预置文件打开一个独立的内存工作区。
func (e *virtualExecutorImpl) NewWorkspace(ctx context.Context, request *NewWorkspaceRequest) (workspace.Workspace, error) {
	// 1. 每次都新建工作区，文件内容各拷一份。
	return workspace.NewVirtualWorkspace(workspace.NewVirtualWorkspaceOpt(e.opt.Definition.WorkspaceDirectory, getVirtualFiles(e.opt.Files)))
}

// MemoryExecutorOpt 是 memory Executor 的配置。
type MemoryExecutorOpt struct {
	Definition *defs.ExecutorDefinition
	// MemoryDirectory 是记忆根目录在 serve 机器上的绝对路径。
	MemoryDirectory string
}

// NewMemoryExecutorOpt 保存 memory Executor 的定义和记忆根目录。
func NewMemoryExecutorOpt(definition *defs.ExecutorDefinition, memoryDirectory string) *MemoryExecutorOpt {
	// 1. 创建配置。
	return &MemoryExecutorOpt{Definition: definition, MemoryDirectory: memoryDirectory}
}

// memoryExecutorImpl 是 memory Executor。
type memoryExecutorImpl struct {
	opt MemoryExecutorOpt
}

// NewMemoryExecutor 创建 memory Executor。
func NewMemoryExecutor(opt *MemoryExecutorOpt) Executor {
	// 1. 保存配置。
	return &memoryExecutorImpl{opt: *opt}
}

// GetDefinition 返回 Executor 的定义。
func (e *memoryExecutorImpl) GetDefinition() *defs.ExecutorDefinition {
	return e.opt.Definition
}

// NewWorkspace 以请求的项目记忆目录打开记忆工作区。
func (e *memoryExecutorImpl) NewWorkspace(ctx context.Context, request *NewWorkspaceRequest) (workspace.Workspace, error) {
	// 1. 创建记忆工作区。
	return workspace.NewMemoryWorkspace(workspace.NewMemoryWorkspaceOpt(e.opt.MemoryDirectory, request.MemoryProjectName))
}
