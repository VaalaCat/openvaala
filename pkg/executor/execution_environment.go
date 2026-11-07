package executor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/workspace"
	"github.com/sourcegraph/conc/pool"
)

// ExecutionEnvironment 是对话里供模型选择的一个执行环境，包含工作区、各 Session 共用的无状态工具和它自己的 MCP 连接；
// 由 Conversation 创建、运行和停止，Session 只借用。系统提示里列出它的 ID 和说明。
type ExecutionEnvironment interface {
	// GetID 返回模型选择执行环境时使用的 ID。
	GetID() string
	// GetDescription 返回执行环境说明。
	GetDescription() string
	// GetWorkspace 返回环境的工作区；测试里没有文件系统的环境返回 nil。
	GetWorkspace() workspace.Workspace
	// GetTools 返回环境共享的内置工具，这些工具不带 Session 状态。
	GetTools() []Tool
	// GetMCPTools 返回 serverIDs 选中的 MCP 工具，每个绑定自己的连接：没连上的等，最多等启动超时；
	// 弱依赖连不上跳过并记 Warn，强依赖连不上返回错误。nil 表示全部，[] 表示不用。
	GetMCPTools(ctx context.Context, serverIDs defs.OptionalStringList) ([]Tool, error)
	// GetMCPServers 返回各 MCP 连接此刻的情况；还没连上的连接也立刻返回。
	GetMCPServers() []mcpclient.ConnectionInfo
	// IsCommandSupported 这个环境能不能跑命令？不能时 Session 不给它建命令工具，工具调用也不跑 hook。
	IsCommandSupported() bool
	// Run 运行各 MCP 连接直到 ctx 结束，全部断开后返回 ctx.Err()；某个连接运行出错时返回它的错误，没有 MCP 连接时直接返回 nil。只能调用一次。
	Run(ctx context.Context) error
}

// executionEnvironmentImpl 构造后只有 MCP 连接自己的状态会变，多个 Session 可以并发使用。
type executionEnvironmentImpl struct {
	opt *ExecutionEnvironmentOpt
	// mcpConnections 是分给本环境的 MCP server 的连接，按 server ID 排序；由 Run 运行。
	mcpConnections []mcpclient.Connection
}

// ExecutionEnvironmentOpt 保存环境标识、工作区、内置工具和 MCP server 配置，构造后只读。
type ExecutionEnvironmentOpt struct {
	ID          string
	Description string
	Workspace   workspace.Workspace
	Tools       []Tool
	// MCPServers 由 Conversation 填入本对话 Agent 会用到的 server；只取 ExecutorIDs 里含本环境 ID 的，
	// NewExecutionEnvironment 按本环境的工作区为它们创建连接，Run 之后才开始连。
	MCPServers []*mcpclient.ServerDefinition
	// IsCommandSupported 这个环境能不能跑命令？默认能；memory 这类只能读写文件的环境不能。
	IsCommandSupported bool
}

// NewExecutionEnvironmentOpt 保存执行环境标识、说明、工作区和工具。
func NewExecutionEnvironmentOpt(id, description string, environmentWorkspace workspace.Workspace, tools ...Tool) *ExecutionEnvironmentOpt {
	// 1. 保存环境信息并复制工具列表；默认能跑命令。
	return &ExecutionEnvironmentOpt{
		ID:                 id,
		Description:        description,
		Workspace:          environmentWorkspace,
		Tools:              slices.Clone(tools),
		IsCommandSupported: true,
	}
}

// WithMCPServers 设置 MCP server 列表并返回本配置。
func (o *ExecutionEnvironmentOpt) WithMCPServers(servers []*mcpclient.ServerDefinition) *ExecutionEnvironmentOpt {
	// 1. 复制并保存 server 列表。
	o.MCPServers = slices.Clone(servers)
	return o
}

// WithoutCommands 标记这个环境不能跑命令并返回本配置；memory 环境使用。
func (o *ExecutionEnvironmentOpt) WithoutCommands() *ExecutionEnvironmentOpt {
	// 1. 关闭命令支持。
	o.IsCommandSupported = false
	return o
}

// NewExecutionEnvironment 校验环境配置，并为分给本环境的 MCP server 创建连接。
func NewExecutionEnvironment(opt *ExecutionEnvironmentOpt) (ExecutionEnvironment, error) {
	// 1. 校验环境 ID 和内置工具定义。
	if opt == nil || strings.TrimSpace(opt.ID) == "" {
		return nil, errors.New("execution environment requires an ID")
	}
	if err := validateToolDefinitions(opt.Tools); err != nil {
		return nil, err
	}
	// 2. 为分配给本环境的 server 创建连接，再保存环境配置。
	var connections []mcpclient.Connection
	for _, definition := range getAssignedMCPServers(opt.ID, opt.MCPServers) {
		connection, err := mcpclient.NewConnection(mcpclient.NewConnectionOpt(definition, opt.Workspace))
		if err != nil {
			return nil, err
		}
		connections = append(connections, connection)
	}
	savedOpt := NewExecutionEnvironmentOpt(opt.ID, opt.Description, opt.Workspace, opt.Tools...).WithMCPServers(opt.MCPServers)
	savedOpt.IsCommandSupported = opt.IsCommandSupported
	return &executionEnvironmentImpl{
		opt:            savedOpt,
		mcpConnections: connections,
	}, nil
}

// Run 运行全部 MCP 连接并等待结束。
func (e *executionEnvironmentImpl) Run(ctx context.Context) error {
	// 1. 没有 MCP 连接时直接返回。
	if len(e.mcpConnections) == 0 {
		return nil
	}
	// 2. 并发运行连接，等待全部断开后返回运行错误或 ctx.Err()。
	tasks := pool.New().WithErrors()
	for _, connection := range e.mcpConnections {
		tasks.Go(func() error {
			err := connection.Run(ctx)
			if errors.Is(err, ctx.Err()) {
				return nil
			}
			return err
		})
	}
	if err := tasks.Wait(); err != nil {
		return fmt.Errorf("run MCP connections of executor %q: %w", e.opt.ID, err)
	}
	return ctx.Err()
}

// GetID 返回执行环境 ID。
func (e *executionEnvironmentImpl) GetID() string {
	// 1. 读取配置中的环境 ID。
	return e.opt.ID
}

// GetDescription 返回执行环境说明。
func (e *executionEnvironmentImpl) GetDescription() string {
	// 1. 读取配置中的环境说明。
	return e.opt.Description
}

// GetWorkspace 返回执行环境的工作区。
func (e *executionEnvironmentImpl) GetWorkspace() workspace.Workspace {
	// 1. 读取配置中的工作区。
	return e.opt.Workspace
}

// GetTools 返回内置工具列表的副本。
func (e *executionEnvironmentImpl) GetTools() []Tool {
	// 1. 复制内置工具列表。
	return slices.Clone(e.opt.Tools)
}

// GetMCPTools 读取本轮选中的 MCP 工具。
func (e *executionEnvironmentImpl) GetMCPTools(ctx context.Context, serverIDs defs.OptionalStringList) ([]Tool, error) {
	// 1. 空列表表示不使用 MCP 工具，直接返回。
	if serverIDs != nil && len(serverIDs) == 0 {
		return nil, nil
	}
	// 2. 按 server ID 选择连接，再读取工具；nil 表示全部。
	var selected []mcpclient.Connection
	for _, connection := range e.mcpConnections {
		if serverIDs == nil || slices.Contains(serverIDs, connection.GetInfo().ServerID) {
			selected = append(selected, connection)
		}
	}
	return getMCPTools(telemetry.ContextWithAttrs(ctx, telemetry.ExecutorID(e.opt.ID)), selected)
}

// GetMCPServers 返回全部 MCP 连接的状态。
func (e *executionEnvironmentImpl) GetMCPServers() []mcpclient.ConnectionInfo {
	// 1. 读取各连接的状态。
	return getConnectionInfos(e.mcpConnections)
}

// IsCommandSupported 返回环境能不能跑命令。
func (e *executionEnvironmentImpl) IsCommandSupported() bool {
	// 1. 读取配置中的命令支持。
	return e.opt.IsCommandSupported
}

// getMCPTools 并发读取各连接提供的 MCP 工具。
func getMCPTools(ctx context.Context, connections []mcpclient.Connection) ([]Tool, error) {
	// 1. 并发读取各连接的工具列表，等待本次读取结束。
	type connectionTools struct {
		definitions []*defs.ToolDefinition
		err         error
	}
	readings := make([]connectionTools, len(connections))
	tasks := pool.New()
	for index, connection := range connections {
		tasks.Go(func() { readings[index].definitions, readings[index].err = connection.GetTools(ctx) })
	}
	tasks.Wait()

	// 2. 把读到的工具绑定到各自的连接；强依赖失败返回错误，弱依赖失败记 Warn 后跳过。
	var tools []Tool
	for index, reading := range readings {
		connection := connections[index]
		if reading.err == nil {
			for _, definition := range reading.definitions {
				tools = append(tools, &mcpTool{
					definition: definition,
					connection: connection,
				})
			}
			continue
		}
		serverID := connection.GetInfo().ServerID
		if connection.IsRequired() {
			return nil, fmt.Errorf("mcp server %q is unavailable: %w", serverID, reading.err)
		}
		telemetry.GetLogger(ctx).Warn("mcp server skipped", "mcp_server_id", serverID, "error", reading.err)
	}
	return tools, nil
}
