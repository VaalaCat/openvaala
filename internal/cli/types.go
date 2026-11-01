package cli

import (
	"context"
	"encoding/json"
	"github.com/VaalaCat/openvaala/pkg/defs"
	acp "github.com/coder/acp-go-sdk"
	"io"
	"time"

	"github.com/VaalaCat/openvaala/internal/config"
)

// ── CLI 命令行参数 ──

// CLIFlags 保存 Cobra 直接解析的参数；是否显式传入由 Cobra 的 Changed 状态判断。
type CLIFlags struct {
	ConfigFilePath  string
	AgentsDirectory string
	ModelsDirectory string
	AgentName       string
}

// ── App 启动参数 ──

// AppArgs 保存已加载的配置、命令参数、标准流和版本。
type AppArgs struct {
	Config *config.Config
	Stdin  io.Reader
	Stdout io.Writer
	// RuntimeURL 是 --url 指定的 Runtime Server 地址；为空时 getRuntimeConnection 依次用 client.url、本机 serve（没有就自动拉起）。
	RuntimeURL string
	// Stderr 接收 serve、acp 和 app-server 的文本日志，以及客户端的旧 serve 提示；codex 子进程使用它，正常退出后追加重连提示。TUI 不使用。
	Stderr io.Writer
	// Version 是程序版本，写入遥测的 service.version。
	Version string
	// IdleTimeout 是 serve 的隐藏参数 --idle-timeout：大于 0 时没有连接且没有 Turn 在执行这么久后 serve 自己退出；为 0 时常驻。
	IdleTimeout time.Duration
	// ConfigFlags 保存显式传入的配置文件、Agent 目录、模型目录和 Agent 名称，格式为 --name=value；仅配置文件转为绝对路径。自动启动 serve 时原样传入，不含 Codex 的 -c/--config。
	ConfigFlags []string
	// Executable 是当前进程的可执行文件路径，客户端用它自动拉起 serve；取不到时为空，拉起 serve 会失败。
	Executable string
	// ConfigPath 是读取的配置文件的绝对路径（--config-file、OPENVAALA_CONFIG 或默认位置）；app-server 把它所在目录交给 Codex 客户端。
	ConfigPath string
	// ConfigOpt 是加载 Config 用的选项；serve 把它交给 config.ConfigFiles，重新加载时用同一个配置路径和 flag。
	ConfigOpt *config.LoadConfigOpt
	// AppServerListen 是 app-server 的 --listen，默认 stdio://。
	AppServerListen string
	// IsAppServerStdio 是否传入了 --stdio？
	IsAppServerStdio bool
	// PrintAppServerStartupConfig 是否只输出 Codex 桌面启动检查需要的 JSON，然后退出？
	PrintAppServerStartupConfig bool
	// UnsupportedWebSocketFlags 是 app-server 收到的 codex WebSocket 认证参数名（--ws-auth 等）：OpenVaala 不支持，出现就报错。
	UnsupportedWebSocketFlags []string
	// IgnoredCodexFlags 是用户传入的 codex 参数名（-c/--config、--enable、--disable、--strict-config，
	// 以及 app-server 的分析、code-mode、remote-control 和启动客户端、退出登录参数）；普通 app-server 启动时写一条 Debug 日志。
	IgnoredCodexFlags []string
	// CodexRootArguments 是用户传入的 codex 根命令参数，按 codex 的写法排好（-c k=v、--enable x、--disable y、--strict-config）；
	// openvaala codex 把它们交给 codex，app-server 等命令收下不用。
	CodexRootArguments []string
	// CodexArguments 是 openvaala codex 之后的参数（codex 自己的参数写在 -- 之后），接在 CodexRootArguments 之后交给 codex；
	// codex app 时是可选的一个工作目录。其他命令不收位置参数，为空。
	CodexArguments []string
}

// ── serve 连接信息 ──

// RuntimeServerConnection 是 serve 启动后写入 ~/.local/state/openvaala/runtime-server.yaml 的连接信息，本机客户端读它来连接。
type RuntimeServerConnection struct {
	URL   string `yaml:"url"`
	Token string `yaml:"token"`
	// Executable 是 serve 的可执行文件路径（软链接已解析），取不到时为空；客户端拿它和自己的可执行文件比较，发现升级或重新编译前的旧 serve。
	Executable string `yaml:"executable"`
	// ExecutableModifiedAt 是 serve 启动时 Executable 的修改时间，取不到时为零值。
	ExecutableModifiedAt time.Time `yaml:"executable_modified_at"`
	// ServerID 标识这台机器上的 serve，重启后沿用；客户端读到的和 initialize 返回的一致时，两边用的是同一个文件系统。
	ServerID string `yaml:"server_id"`
}

// ── Runtime Server 连接 ──

// runtimeConnection 是客户端命令（TUI、acp、app-server、codex、web）要连的 serve，由 getRuntimeConnection 决定。
type runtimeConnection struct {
	URL string
	// Token 为空时沿用 URL 里的 ?token=。
	Token string
	// Source 是地址的来源：--url、client.url，还是本机 runtime-server.yaml。
	Source runtimeConnectionSource
	// LocalServerID 是本机 runtime-server.yaml 里的 server_id，文件不存在或读不出来时为空；
	// 连上后和 serve 返回的 server_id 比较，判断连的是不是本机 serve。
	LocalServerID string
	// IsFromOtherExecutable 本机 serve 是否由另一个可执行文件启动？仅本机来源可能为 true。
	IsFromOtherExecutable bool
}

// runtimeConnectionSource 是 runtimeConnection.URL 的来源，值就是给用户看的文字。
type runtimeConnectionSource string

const (
	// runtimeConnectionSourceFlag 地址来自命令行 --url。
	runtimeConnectionSourceFlag runtimeConnectionSource = "--url"
	// runtimeConnectionSourceConfig 地址来自配置 client.url（含 OPENVAALA_CLIENT_URL）。
	runtimeConnectionSourceConfig runtimeConnectionSource = "client.url"
	// runtimeConnectionSourceLocal 地址来自本机 runtime-server.yaml，serve 没在跑时已经拉起。
	runtimeConnectionSourceLocal runtimeConnectionSource = "local"
)

// ── ACP 协议消息 ──

// acpMessage 是一行 JSON-RPC 消息；ID 保留客户端原值。
type acpMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *acpError       `json:"error,omitempty"`
}

// acpError 是回给编辑器的 JSON-RPC 错误。
type acpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Error 返回发送给编辑器的错误消息。
func (e *acpError) Error() string {
	// 1. 返回协议错误消息。
	return e.Message
}

// ── acpAgent 主循环事件 ──

// acpReceived 由 stdin 读取任务每读完一行发给 acpAgent 主循环；解析失败时带着协议错误。
type acpReceived struct {
	message acpMessage
	err     *acpError
}

// acpSessionOpened 由 acpSession 在打开对话、回放历史后发给 acpAgent 主循环，打开失败时带着错误；
// 主循环清除加载标记，回复编辑器的 session/new 或 session/load。
type acpSessionOpened struct {
	// delivered 不为 nil 时，主循环把回复放进输出通道后关闭它。
	delivered chan struct{}
	sessionID defs.ConversationID
	// requestID 是要回复的 session/new 或 session/load 请求 ID。
	requestID json.RawMessage
	result    any
	err       error
}

// acpSessionStopped 由 acpSession 的任务在 Run 返回后发给 acpAgent 主循环，主循环删掉这个会话的记录。
type acpSessionStopped struct {
	session *acpSession
	err     error
}

// ── acpSession 显示记录 ──

// acpStreamingItem 是一个 Item 已经向编辑器显示到哪一步。
type acpStreamingItem struct {
	// callID 是已经发过 tool_call 的工具调用 ID；为空时还没发。
	callID string
	// textBytes 是已经显示的消息文本字节数。
	textBytes int
	// reasoningBytes 是已经显示的推理文字字节数。
	reasoningBytes int
	// reasoningUsesContent 推理显示的是正文而不是摘要？还没显示过推理时，收到的推理增量没有摘要就定为正文。
	reasoningUsesContent bool
	// completed 完整 Item 是否已经显示过？显示过后图片和工具结果不再重发。
	completed bool
}

// ── acpSession 客户端 MCP 转发记录 ──

// acpClientMCPServer 是编辑器在 session/new 或 session/load 里带来的一个 MCP server。
type acpClientMCPServer struct {
	// ID 是登记给 serve 的合法 server id，由编辑器给的名字转换而来。
	ID string
	// Server 是编辑器给的原始配置，只会是 stdio、http 或 sse。
	Server acp.McpServer
}

// acpRelayStream 保存 serve 与编辑器本机 MCP server 之间的一条消息转发流；会话按 stream_id 登记，转发任务结束后删除。
type acpRelayStream struct {
	// id 是 serve 生成的 stream_id，serverID 是 serve 请求连接的 server id（编辑器给的名字转换后的 id）。
	id, serverID string
	// incoming 缓存事件循环收到的 serve 消息；积压写满时结束转发流。
	incoming chan json.RawMessage
	// cancel 结束这条流：serve 要求断开、消息积压写满或会话结束时调用，可重复调用；cause 作为结束原因告知 serve，nil 表示没有特别的原因。
	cancel context.CancelCauseFunc
}
