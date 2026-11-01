// Package runtimeprotocol 是 serve 与客户端共用的 OpenVaala 线上协议类型、方法表和通知表。
package runtimeprotocol

import (
	"reflect"

	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/skill"
)

// ProtocolVersion 是 OpenVaala 协议版本，initialize 双方必须一致。
const ProtocolVersion = "5"

// InitializeMethod 是建立连接后的第一个请求。
const InitializeMethod = "initialize"

// Method 是一个请求方法的名字和参数、结果类型。
type Method struct {
	Name   string
	Params reflect.Type
	Result reflect.Type
}

// NewMethod 用 P、R 的类型登记一个请求方法。
func NewMethod[P any, R any](name string) Method {
	// 1. 记录方法名和 P、R 的类型。
	return Method{
		Name:   name,
		Params: reflect.TypeFor[P](),
		Result: reflect.TypeFor[R](),
	}
}

// Methods 是全部请求方法，包括 initialize。P 是参数结构体（服务端处理函数收到 *P），
// R 与处理函数的返回类型完全相同：多数是指针，skill/list 是 []skill.SkillInfo。
var Methods = []Method{
	NewMethod[InitializeParams, *InitializeResult](InitializeMethod),
	NewMethod[CreateConversationParams, *ConversationResult]("conversation/create"),
	NewMethod[OpenConversationParams, *ConversationResult]("conversation/open"),
	NewMethod[ConversationParams, *ConversationResult]("conversation/read"),
	NewMethod[ListConversationsParams, *ListConversationsResult]("conversation/list"),
	NewMethod[SetConversationTitleParams, *EmptyResult]("conversation/setTitle"),
	NewMethod[ForkConversationParams, *ConversationResult]("conversation/fork"),
	NewMethod[SetConversationArchivedParams, *ConversationResult]("conversation/setArchived"),
	NewMethod[ConversationParams, *EmptyResult]("conversation/delete"),
	NewMethod[SetExecutorSettingsParams, *ConversationResult]("conversation/setExecutorSettings"),
	NewMethod[SubmitInputParams, *SubmitInputResult]("conversation/submitInput"),
	NewMethod[SteerInputParams, *EmptyResult]("conversation/steerInput"),
	NewMethod[InterruptTurnWithInputParams, *EmptyResult]("conversation/interruptTurnWithInput"),
	NewMethod[WithdrawInputParams, *WithdrawInputResult]("conversation/withdrawInput"),
	NewMethod[ConversationParams, *InterruptTurnResult]("conversation/interruptTurn"),
	NewMethod[ConversationParams, *CompactContextResult]("conversation/compact"),
	NewMethod[GetSessionParams, *SessionResult]("session/get"),
	NewMethod[ListSessionsParams, *ListSessionsResult]("session/list"),
	NewMethod[ListTurnsParams, *ListTurnsResult]("session/turns/list"),
	NewMethod[GetTurnsParams, *GetTurnsResult]("session/turns/get"),
	NewMethod[ListItemsParams, *ListItemsResult]("session/items/list"),
	NewMethod[SessionParams, *runtime.SessionContextUsage]("session/contextUsage"),
	NewMethod[SessionParams, *conversation.SessionDetails]("session/details"),
	NewMethod[SetSessionModelParams, *SessionResult]("session/setModel"),
	NewMethod[ListModelsParams, *runtime.ModelList]("model/list"),
	NewMethod[EmptyParams, *ListAgentsResult]("agent/list"),
	NewMethod[EmptyParams, *ListExecutorsResult]("executor/list"),
	NewMethod[ResolveDirectoryParams, *ResolveDirectoryResult]("executor/resolveDirectory"),
	NewMethod[ListDirectoriesParams, *conversation.WorkspaceDirectories]("executor/listDirectories"),
	NewMethod[EmptyParams, *ListProvidersResult]("provider/list"),
	NewMethod[SetProviderParams, *runtime.SetDefinitionsResult]("provider/set"),
	NewMethod[ProviderIDParams, *runtime.SetDefinitionsResult]("provider/delete"),
	NewMethod[ProviderParams, *ProviderModelsResult]("provider/fetchModels"),
	NewMethod[SetDefaultModelParams, *runtime.SetDefinitionsResult]("config/setDefaultModel"),
	NewMethod[EmptyParams, *runtime.SetDefinitionsResult]("config/reload"),
	NewMethod[EmptyParams, *ListCatalogProvidersResult]("catalog/providers/list"),
	NewMethod[ProviderIDParams, *ProviderModelsResult]("catalog/models/list"),
	NewMethod[ListSkillsParams, []skill.SkillInfo]("skill/list"),
	NewMethod[EmptyParams, *ListHooksResult]("hook/list"),
	NewMethod[ConversationParams, *ListMCPServersResult]("mcpServer/list"),
	NewMethod[SetClientMCPServersParams, *EmptyResult]("conversation/setClientMCPServers"),
	NewMethod[ConversationParams, *SubscribeConversationResult]("conversation/subscribe"),
	NewMethod[ConversationParams, *EmptyResult]("conversation/unsubscribe"),
	NewMethod[EmptyParams, *runtime.RunningConversations]("conversation/running/subscribe"),
	NewMethod[EmptyParams, *EmptyResult]("conversation/running/unsubscribe"),
	NewMethod[SessionParams, *SubscribeSessionResult]("session/subscribe"),
	NewMethod[SessionParams, *EmptyResult]("session/unsubscribe"),
	NewMethod[TerminalOpenParams, *TerminalOpenResult]("terminal/open"),
	NewMethod[TerminalParams, *EmptyResult]("terminal/close"),
}

// Notification 是一种通知的名字和参数类型。
type Notification struct {
	Name   string
	Params reflect.Type
}

// NewNotification 用 P 的类型登记一种通知。
func NewNotification[P any](name string) Notification {
	// 1. 记录通知名和 P 的类型。
	return Notification{
		Name:   name,
		Params: reflect.TypeFor[P](),
	}
}

// ServerNotifications 是 serve 发给客户端的通知。
var ServerNotifications = []Notification{
	NewNotification[runtime.RunningConversations]("conversation/runningChanged"),
	NewNotification[RunningConversationsClosedParams]("conversation/runningClosed"),
	NewNotification[SubscriptionClosedParams]("subscription/closed"),
	NewNotification[SessionPendingInputsUpdatedParams]("session/pendingInputsUpdated"),
	NewNotification[SessionStatusUpdatedParams]("session/statusUpdated"),
	NewNotification[ConversationTitleUpdatedParams]("conversation/titleUpdated"),
	NewNotification[SessionStartedParams]("session/started"),
	NewNotification[SessionStoppedParams]("session/stopped"),
	NewNotification[TurnStartedParams]("turn/started"),
	NewNotification[TurnFinishedParams]("turn/finished"),
	NewNotification[ItemsSavedParams]("items/saved"),
	NewNotification[ItemEventParams]("item/started"),
	NewNotification[ItemEventParams]("item/delta"),
	NewNotification[ItemEventParams]("item/completed"),
	NewNotification[ItemEventParams]("item/interrupted"),
	NewNotification[ItemEventParams]("item/toolProgress"),
	NewNotification[HookRunParams]("hook/started"),
	NewNotification[HookRunParams]("hook/completed"),
	NewNotification[ClientMCPConnectParams]("clientMCP/connect"),
	NewNotification[ClientMCPMessageParams]("clientMCP/message"),
	NewNotification[ClientMCPDisconnectParams]("clientMCP/disconnect"),
	NewNotification[TerminalOutputParams]("terminal/output"),
	NewNotification[TerminalExitedParams]("terminal/exited"),
}

// ClientNotifications 是客户端发给 serve 的通知，共 4 种。
var ClientNotifications = []Notification{
	NewNotification[ClientMCPMessageParams]("clientMCP/message"),
	NewNotification[ClientMCPClosedParams]("clientMCP/closed"),
	NewNotification[TerminalInputParams]("terminal/input"),
	NewNotification[TerminalResizeParams]("terminal/resize"),
}
