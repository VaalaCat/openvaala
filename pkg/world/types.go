package world

import (
	"context"
	"encoding/json"
	"time"

	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/worldserver"
	"github.com/coder/websocket"
)

// OpenSessionRequest 指定来信线程及其已有的宿主会话。
type OpenSessionRequest struct {
	ThreadID          string `json:"thread_id"`
	Target            string `json:"target"`
	PeerAddr          string `json:"peer_addr"`
	Title             string `json:"title"`
	ExistingSessionID string `json:"existing_session_id,omitempty"`
}

// OpenedSession 保存线程绑定的宿主 ID 和本次执行使用的根 Session ID。
type OpenedSession struct {
	SessionID     string         `json:"session_id"`
	RootSessionID defs.SessionID `json:"-"`
}

// InboundTurnRequest 将已打开的会话和待处理的来信交给宿主。
type InboundTurnRequest struct {
	OpenedSession OpenedSession
	Message       WorldMessage
	Target        string
}

// ── 联邦地址 ──

// Address 标识联邦节点及其可选目标。
type Address struct {
	Node   string `json:"node"`
	Domain string `json:"domain"`
	Target string `json:"target,omitempty"`
}

// ── 节点身份密钥 ──

// NodeIdentity 保存本机持久身份，私钥仅用于本地签名和密钥协商。
type NodeIdentity struct {
	NodeID      string `json:"node_id"`
	SignPublic  []byte `json:"sign_public"`
	SignPrivate []byte `json:"sign_private"`
	BoxPublic   []byte `json:"box_public"`
	BoxPrivate  []byte `json:"box_private"`
}

// ── 联邦信封 ──

// wireMessage 是在 SecureChannel 密文内传输的联邦消息信封。
type wireMessage struct {
	Kind      string  `json:"kind"` // message | reply | ack | status_query
	MessageID string  `json:"message_id"`
	ThreadID  string  `json:"thread_id"`
	From      Address `json:"from"`
	To        Address `json:"to"`
	HopLimit  int     `json:"hop_limit"` // 默认 5，每转发一跳减 1，<=0 拒绝
	Subject   string  `json:"subject,omitempty"`
	Body      string  `json:"body,omitempty"`
	InReplyTo string  `json:"in_reply_to,omitempty"`
	ErrorCode string  `json:"error_code,omitempty"`
	Status    string  `json:"status,omitempty"` // accepted | running | completed | failed | not_found
}

// ── 节点存储记录 ──

// WorldMessageDirection 标记消息相对于本节点的收发方向。
type WorldMessageDirection string

const (
	WorldMessageDirectionOutbound WorldMessageDirection = "outbound"
	WorldMessageDirectionInbound  WorldMessageDirection = "inbound"
)

// WorldMessageStatus 记录发件投递或收件处理的进度。
type WorldMessageStatus string

const (
	WorldMessageStatusPendingOffline WorldMessageStatus = "pending_offline"
	WorldMessageStatusSending        WorldMessageStatus = "sending"
	WorldMessageStatusUnknown        WorldMessageStatus = "unknown"
	WorldMessageStatusRemoteRunning  WorldMessageStatus = "remote_running"
	WorldMessageStatusReplied        WorldMessageStatus = "replied"
	WorldMessageStatusFailed         WorldMessageStatus = "failed"
	WorldMessageStatusCancelled      WorldMessageStatus = "cancelled"
	WorldMessageStatusAccepted       WorldMessageStatus = "accepted"
	WorldMessageStatusRunning        WorldMessageStatus = "running"
	WorldMessageStatusCompleted      WorldMessageStatus = "completed"
)

// WorldMessage 保存本节点收发的消息、对话归属和处理结果。
type WorldMessage struct {
	MessageID          string                `json:"message_id"`
	ThreadID           string                `json:"thread_id"`
	Direction          WorldMessageDirection `json:"direction"`
	PeerAddr           string                `json:"peer_addr"`
	Target             string                `json:"target,omitempty"`
	Kind               string                `json:"kind"` // message | reply
	ConversationID     string                `json:"conversation_id,omitempty"`
	Subject            string                `json:"subject,omitempty"`
	Body               string                `json:"body"`
	InReplyTo          string                `json:"in_reply_to,omitempty"`
	HopLimit           int                   `json:"hop_limit"`
	Status             WorldMessageStatus    `json:"status"`
	ReplyBody          string                `json:"reply_body,omitempty"`
	ErrorCode          string                `json:"error_code,omitempty"`
	ErrorText          string                `json:"error_text,omitempty"`
	NotifyConversation bool                  `json:"notify_conversation"`
	ReceivedSequence   uint64                `json:"received_sequence,omitempty"`
	CreatedAt          int64                 `json:"created_at"`
	UpdatedAt          int64                 `json:"updated_at"`
}

// ThreadBinding 将来信线程绑定到本地对话及该轮的转发预算。
type ThreadBinding struct {
	PeerDomain     string `json:"peer_domain"`
	Target         string `json:"target"`
	ThreadID       string `json:"thread_id"`
	ConversationID string `json:"conversation_id"`
	PeerAddr       string `json:"peer_addr"`
	Direction      string `json:"direction"`      // inbound | outbound
	RemainingHops  int    `json:"remaining_hops"` // 收到时保存的剩余跳数
	CreatedAt      int64  `json:"created_at"`
}

// ── 节点客户端请求与视图 ──

// listPage 是节点只读列表接口的统一游标分页响应。
type listPage[T any] struct {
	Items      []T                      `json:"items"`
	NextCursor dao.RecordCursor[string] `json:"next_cursor,omitzero"`
}

type SendMessageRequest struct {
	To                   string `json:"to"`
	Message              string `json:"message"`
	Subject              string `json:"subject,omitempty"`
	ThreadID             string `json:"thread_id,omitempty"`
	SourceConversationID string `json:"source_conversation_id,omitempty"`
	// 是否等到收到回复？
	Wait bool `json:"wait,omitempty"`
}

type ForwardRequest struct {
	ConversationID defs.ConversationID `json:"conversation_id" binding:"required"`
	To             string              `json:"to" binding:"required"`
	ItemIDs        []defs.ItemID       `json:"item_ids,omitempty"`
	Note           string              `json:"note,omitempty"`
}

// NodeStatus 是节点状态接口返回的运行、连接和离线队列信息。
type NodeStatus struct {
	Revoked bool `json:"revoked"`
	// 本机联邦是否启用？
	Enabled      bool   `json:"enabled"`
	AdminEnabled bool   `json:"admin_enabled"`
	NodeAddr     string `json:"node_addr"`
	ServerURL    string `json:"server_url"`
	// 本机是否已收到控制面的通讯录？
	Connected             bool          `json:"connected"`
	PendingOutboxCount    int           `json:"pending_outbox_count"`
	RemoteAccessLocalOnly bool          `json:"remote_access_local_only,omitempty"`
	RemoteAccessRelays    []RelayStatus `json:"remote_access_relays,omitempty"`
	RemoteAccessURL       string        `json:"remote_access_url,omitempty"`
	RelayAttachments      []RelayStatus `json:"relay_attachments,omitempty"`
}

// RelayStatus 是节点到单个 relay 的挂靠状态。
type RelayStatus struct {
	URL      string `json:"url"`
	Implicit bool   `json:"implicit"`
	// 是否收到 relay 的挂靠确认？
	Connected bool `json:"connected"`
}

type WorldPeer struct {
	Address  string `json:"address"`
	NodeName string `json:"node_name"`
	// 联系人是否可以投递？
	Online      bool                           `json:"online"`
	Description string                         `json:"description,omitempty"`
	Targets     []worldserver.AdvertisedTarget `json:"targets"`
}

// NodeNotification 是节点发布的状态、联系人或消息更新。
type NodeNotification struct {
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
}

// RecentContact 给出外域对端及最近活动的线程。
type RecentContact struct {
	Address  string `json:"address"`
	ThreadID string `json:"thread_id"`
}

type ListPeersResult struct {
	Recent []RecentContact `json:"recent"`
	Peers  []WorldPeer     `json:"peers"`
}

type ListMessagesParams struct {
	Direction      string   `json:"direction,omitempty"`
	PeerAddresses  []string `json:"peer_addr,omitempty"`
	Kind           string   `json:"kind,omitempty"`
	Status         string   `json:"status,omitempty"`
	ConversationID string   `json:"conversation_id,omitempty"`
	ThreadID       string   `json:"thread_id,omitempty"`
	Limit          *int     `json:"limit,omitempty"`
	CursorID       *string  `json:"cursor_id,omitempty"`
}

type ListThreadsParams struct {
	ConversationID  string  `json:"conversation_id,omitempty"`
	Limit           *int    `json:"limit,omitempty"`
	CursorID        *string `json:"cursor_id,omitempty"`
	CursorUpdatedAt *int64  `json:"cursor_updated_at,omitempty"`
}

type MessagePage listPage[WorldMessage]
type ThreadPage listPage[ThreadBinding]

type clientResult struct {
	worldMessage WorldMessage
	status       NodeStatus
	peers        []WorldPeer
	err          error
}

// ── 节点通道状态 ──

// messageWaiter 保存同步等待者及其超时后的通知方式。
type messageWaiter struct {
	finished chan WorldMessage
	// 工具移交等待后是否唤醒来源对话？
	notify bool
}

// peerChannel 保存一次握手及其认证后的有序收发队列。
type peerChannel struct {
	domainKey []byte
	// 此通道是否已核实对端地址，可以向它投递？
	verified bool
	peer     Address
	peerKey  []byte
	ctx      context.Context
	cancel   context.CancelFunc

	secure SecureChannel

	lastUsed time.Time
	outbound []wireMessage
	// 此通道是否有一条正在写出的信封？
	writing     bool
	awaitingAck map[string]struct{}
	queries     map[string]struct{}
}

type memberCacheKey struct{ domain, domainKey, node string }
type memberLookupWaiter struct {
	key    memberCacheKey
	result chan worldserver.DomainLookupResult
}

type cachedMember struct {
	member  worldserver.DomainMember
	expires time.Time
}

// directFrameTransport 将单帧映射成一条二进制 WebSocket 消息。
type directFrameTransport struct{ socket *websocket.Conn }

// ── 节点事件 ──

type nodeEvent interface{ isNodeEvent() }

// controlReceived 由控制连接任务在收到控制帧时提交，断线时提交空信封。
type controlReceived struct {
	socket   *websocket.Conn
	envelope worldserver.ControlEnvelope
}

// ── Relay 挂靠事件 ──

// RelayEvent 是挂靠连接通知所属事件循环的状态变化或拨入请求。
type RelayEvent interface {
	nodeEvent
	isRelayEvent()
}

func (RelayAttachmentChanged) isRelayEvent() {}
func (RelayIncoming) isRelayEvent()          {}

// RelayAttachmentChanged 由挂靠连接收到登记确认或断开时提交。
type RelayAttachmentChanged struct {
	URL       string
	Connected bool
}

func (RelayAttachmentChanged) isNodeEvent() {}

// RelayIncoming 由挂靠读取任务在 relay 通知目标接通时提交。
type RelayIncoming struct {
	AcceptURL string
}

func (RelayIncoming) isNodeEvent() {}

// directAccepted 由直连 HTTP 路由升级 WebSocket 后提交。
type directAccepted struct {
	transport *directFrameTransport
}

// channelNegotiated 由拨号任务在握手结束时提交，接收任务同时交付已解密的首条消息。
type channelNegotiated struct {
	link         *peerChannel
	secure       SecureChannel
	peerKey      []byte
	firstMessage *wireMessage
	err          error
}

// channelRead 由通道读任务收到密文信封或结束时提交。
type channelRead struct {
	link    *peerChannel
	message wireMessage
	err     error
}

// inboundIdentityChecked 由外域来信核对任务交回信封及本地线程的对端节点。
type inboundIdentityChecked struct {
	link       *peerChannel
	message    wireMessage
	threadPeer string
	err        error
}

func (inboundIdentityChecked) isNodeEvent() {}

// channelWritten 由通道写任务完成一条信封时提交。
type channelWritten struct {
	link *peerChannel
	err  error
}

// clientRequested 由客户端或工具发起查询、发信、取消、退订或移交超时等待。
type clientRequested struct {
	operation string
	token     string
	send      SendMessageRequest
	messageID string
	// 异步完成时是否唤醒工具所属对话？
	notify       bool
	result       chan clientResult
	finished     chan WorldMessage
	subscription chan NodeNotification
}

// inboundOpened 由对话打开任务在提交来信前发出。
type inboundOpened struct {
	threadBinding ThreadBinding
	worldMessage  WorldMessage
	openedSession OpenedSession
	err           error
}

// inboundSubmitted 由运行来信的任务在 Runtime 接收输入并将对话写入共享库后发出。
type inboundSubmitted struct {
	worldMessage WorldMessage
}

// inboundFinished 由运行来信的任务在对应 Turn 结束后发出。
type inboundFinished struct {
	worldMessage WorldMessage
	reply        string
	err          error
}

// notificationFinished 由回信任务在来源对话接收输入或返回错误时发出。
type notificationFinished struct {
	worldMessage WorldMessage
	err          error
}

func (controlReceived) isNodeEvent()   {}
func (directAccepted) isNodeEvent()    {}
func (channelNegotiated) isNodeEvent() {}
func (channelRead) isNodeEvent()       {}
func (channelWritten) isNodeEvent()    {}
func (clientRequested) isNodeEvent()   {}
func (inboundOpened) isNodeEvent()     {}
func (inboundSubmitted) isNodeEvent()  {}
func (inboundFinished) isNodeEvent()   {}

func (notificationFinished) isNodeEvent() {}
