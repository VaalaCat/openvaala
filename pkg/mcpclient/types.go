// Package mcpclient 定义 MCP server 配置，在执行环境的工作区里连接 MCP server，并经客户端连接转发客户端侧 MCP server 的消息。
package mcpclient

import (
	"encoding/json"
	"time"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ── MCP server 定义 ──

// ServerTransportType 是 MCP server 的连接方式。
type ServerTransportType string

const (
	ServerTransportStdio ServerTransportType = "stdio"
	ServerTransportHTTP  ServerTransportType = "http"
	ServerTransportSSE   ServerTransportType = "sse"
)

// ServerDefinition 是一个 MCP server 的配置，不含运行位置；Executor 用自己的工作区按它建连接。
type ServerDefinition struct {
	ID      string
	Type    ServerTransportType
	Command string
	Args    []string
	Env     map[string]string
	URL     string
	Headers map[string]string
	// ExecutorIDs 是在哪些 Executor 上运行；客户端 MCP 为空。
	ExecutorIDs []string
	// ToolNames 是提供给模型的工具原名；nil 表示全部。
	ToolNames []string
	// Required 是否强依赖？连不上时让这一轮报错，而不是跳过。
	Required bool
	// StartupTimeout 是一次连接（启动、握手、读工具列表）最多等多久。
	StartupTimeout time.Duration
	// ToolTimeout 是一次工具调用最多等多久。
	ToolTimeout time.Duration
	// Plugin 是声明这个 server 的插件，只读；其他来源为 nil。plugin update 会改 InstalledAt，reload 时这个 server 随之重启。
	Plugin *defs.Plugin
}

// ── MCP 连接超时默认值 ──

const (
	// DefaultStartupTimeout 是 MCP server 没有指定启动超时时，一次连接最多等待的时间；StartupTimeout 为 0 会立即超时。
	DefaultStartupTimeout = 30 * time.Second
	// DefaultToolTimeout 是 MCP server 没有指定工具超时时，一次工具调用最多等待的时间；ToolTimeout 为 0 会立即超时。
	DefaultToolTimeout = 300 * time.Second
)

// ── MCP 连接情况 ──

// ConnectionState 是一个 MCP 连接现在的状态。
type ConnectionState string

const (
	// ConnectionStateConnecting 是 Run 还没开始，或第一次连接还没有结果。
	ConnectionStateConnecting ConnectionState = "connecting"
	// ConnectionStateConnected 是已经连上。
	ConnectionStateConnected ConnectionState = "connected"
	// ConnectionStateFailed 是连接失败或断开；再次连接期间也保持 failed，直到连上。
	ConnectionStateFailed ConnectionState = "failed"
)

// ConnectionInfo 是一个 MCP 连接此刻的情况。
type ConnectionInfo struct {
	ServerID string          `json:"server_id"`
	State    ConnectionState `json:"state"`
	Required bool            `json:"required"`
	// Tools 是 server 当前提供的工具（已按 ToolNames 过滤）；没连上时为空。
	Tools []ConnectionTool `json:"tools"`
	// Error 是最近一次连接失败或断开的原因；连上后清空。
	Error string `json:"error,omitempty"`
	// PluginName 是声明这个 server 的插件名；不是插件声明的为空。
	PluginName string `json:"plugin_name,omitempty"`
}

// ConnectionTool 是 server 提供的一个工具。
type ConnectionTool struct {
	// Name 是模型看到的名字 mcp__<server id>__<原名>：非法字符换成下划线，替换后重名或超过 64 字符时改用带哈希的名字。
	Name string `json:"name"`
	// OriginalName 是 server 里的原名。
	OriginalName string          `json:"original_name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema"`
}

// ── Connection 事件 ──

// connectionEvent 是 Connection 事件循环处理的事件：调用方的请求和连接会话报告的结果。
type connectionEvent interface{ isConnectionEvent() }

// toolsRequested 由 GetTools 查询工具时发送；事件循环立即回复缓存，或登记为连接结果等待者。
type toolsRequested struct {
	reply chan toolsResult
}

// toolCallRequested 由 CallTool 在调用 server 之前触发，向事件循环要已连上的 SDK 会话和工具原名。
type toolCallRequested struct {
	toolName string
	reply    chan toolCallTarget
}

// infoRequested 由 GetInfo 在 Run 开始后发送，事件循环回复此刻的连接信息。
type infoRequested struct {
	reply chan ConnectionInfo
}

// connectionAttemptFinished 由连接会话在一次连接尝试成功（已读到工具列表）或失败（含启动超时）时触发。
type connectionAttemptFinished struct {
	clientSession *mcp.ClientSession
	tools         []ConnectionTool
	err           error
}

// toolListReloadFinished 由已连上的连接会话在收到 tools/list_changed、重读工具列表成功或失败后触发。
type toolListReloadFinished struct {
	tools []ConnectionTool
	err   error
}

// connectionSessionDisconnected 由已连上的连接会话在 server 一侧断开（进程退出、网络断开）时触发；
// Run 结束时的主动断开不触发。
type connectionSessionDisconnected struct {
	err error
}

func (toolsRequested) isConnectionEvent()                {}
func (toolCallRequested) isConnectionEvent()             {}
func (infoRequested) isConnectionEvent()                 {}
func (connectionAttemptFinished) isConnectionEvent()     {}
func (toolListReloadFinished) isConnectionEvent()        {}
func (connectionSessionDisconnected) isConnectionEvent() {}

// ── Connection 请求结果 ──

// toolsResult 是事件循环对 GetTools 的回复。
type toolsResult struct {
	tools []*defs.ToolDefinition
	err   error
}

// toolCallTarget 是事件循环为一次工具调用找到的 SDK 会话和工具原名；没连上或找不到工具时 err 非 nil，errorCode 是结果的错误码。
type toolCallTarget struct {
	clientSession *mcp.ClientSession
	toolName      string
	errorCode     string
	err           error
}
