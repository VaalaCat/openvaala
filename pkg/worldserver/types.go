package worldserver

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"time"

	"github.com/coder/websocket"
	"gorm.io/datatypes"
)

// ── 控制面协议消息 ──

const (
	ControlTypeChallenge          = "challenge"
	ControlTypeChallengeResponse  = "challenge.response"
	ControlTypeNodeAnnounce       = "node.announce"
	ControlTypeDirectory          = "directory"
	ControlTypeDomainLookup       = "domain.lookup"
	ControlTypeDomainLookupResult = "domain.lookupResult"
	ControlTypeRelayAttached      = "relay.attached"
	ControlTypeRelayIncoming      = "relay.incoming"
	ControlTypePing               = "ping"
	ControlTypePong               = "pong"
	ControlTypeNodeRevoked        = "node.revoked"
)

// ControlEnvelope 是节点与控制面之间的 WebSocket 消息。
type ControlEnvelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// ChallengeMessage 由控制面在 WebSocket 握手成功后发送，带本次连接的随机挑战。
type ChallengeMessage struct {
	Challenge []byte `json:"challenge"`
}

// ChallengeResponse 由节点收到挑战后发送，Timestamp 是 Unix 秒。
type ChallengeResponse struct {
	PublicKey []byte `json:"public_key"`
	NodeID    string `json:"node_id"`
	Timestamp int64  `json:"timestamp"`
	Signature []byte `json:"signature"`
}

// RelayInstancesResult 是公开 relay 返回的全部实例地址。
type RelayInstancesResult struct {
	Instances []string `json:"instances"`
}

// PeerEntries 保存按拨号顺序公布的直连与中转地址。
type PeerEntries struct {
	DirectURLs []string `json:"direct_urls"`
	Relays     []string `json:"relays"`
}

// NodeAnnounce 由已认证节点在上报可用目标和直连地址时发送。
type NodeAnnounce struct {
	Entries PeerEntries        `json:"entries"`
	Targets []AdvertisedTarget `json:"targets"`
}

// AdvertisedTarget 描述节点通告的一个目标。
type AdvertisedTarget struct {
	Name        string `json:"name" yaml:"name"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
	Visibility  string `json:"visibility" yaml:"visibility"`
}

// DirectoryNode 是通讯录中一个节点的公开身份和可达信息。
type DirectoryNode struct {
	NodeID     string   `json:"node_id"`
	NodeName   string   `json:"node_name"`
	Domain     string   `json:"domain"`
	Owner      string   `json:"owner"`
	Tags       []string `json:"tags"`
	SignPublic []byte   `json:"sign_public"`
	// Online 节点是否有已认证连接？
	Online bool `json:"online"`
	// CanSend 接收本通讯录的节点是否有权主动向该节点发起消息？
	CanSend bool `json:"can_send"`
	// CanReceive 该节点是否有权向本通讯录的接收方发信？
	CanReceive bool               `json:"can_receive"`
	Entries    PeerEntries        `json:"entries"`
	Targets    []AdvertisedTarget `json:"targets"`
}

// Directory 由控制面在通讯录变更时推送给在线节点。
type Directory struct {
	Nodes   []DirectoryNode `json:"nodes"`
	Domains []PeerDomain    `json:"domains"`
}

// RelayIncoming 由 relay 收到拨号请求时发给目标节点。
type RelayIncoming struct {
	ConnectionID string `json:"connection_id"`
}

// NodeRevoked 由控制面在管理员吊销节点后向该节点发送。
type NodeRevoked struct {
	Code   string `json:"code,omitempty"`
	NodeID string `json:"node_id"`
}

// ── 域查询请求与响应 ──

// DomainLookup 由已认证成员通过本域控制连接发起。
type DomainLookup struct {
	RequestID string `json:"request_id"`
	Domain    string `json:"domain"`
	Node      string `json:"node"`
}

// DomainLookupResult 由控制面完成代查后回给原请求连接。
type DomainLookupResult struct {
	RequestID string        `json:"request_id"`
	DomainKey []byte        `json:"domain_key,omitempty"`
	Member    *DomainMember `json:"member,omitempty"`
	Code      string        `json:"code,omitempty"`
	Error     string        `json:"error,omitempty"`
}

// DomainMember 是所属域确认的成员身份及进入方式。
type DomainMember struct {
	PublicKey []byte      `json:"public_key"`
	Owner     string      `json:"owner"`
	Entries   PeerEntries `json:"entries"`
}

// ── 控制面 HTTP 请求与响应 ──

type ListDomainsResult struct {
	Domain   string       `json:"domain"`
	Domains  []PeerDomain `json:"domains"`
	SelfCard string       `json:"self_card"`
}
type AddDomainRequest struct {
	Card string `json:"card"`
}

// RegisterNodeRequest 提交待注册节点的名称、公钥和入网证明。
type RegisterNodeRequest struct {
	NodeName   string `json:"node_name"`
	JoinToken  string `json:"join_token"`
	SignPublic []byte `json:"sign_public"`
	BoxPublic  []byte `json:"box_public"`
	Signature  []byte `json:"signature"`
}

// RegisterNodeResponse 返回注册后的节点身份及归属。
type RegisterNodeResponse struct {
	NodeID   string   `json:"node_id"`
	NodeName string   `json:"node_name"`
	Domain   string   `json:"domain"`
	Owner    string   `json:"owner"`
	Tags     []string `json:"tags"`
}

// CreateJoinTokenRequest 指定入网码归属和过期时间，ExpiresAt 使用 Unix 毫秒。
type CreateJoinTokenRequest struct {
	Owner string   `json:"owner"`
	Tags  []string `json:"tags"`
	// Reusable 入网码是否可供多个节点注册？
	Reusable  bool  `json:"reusable"`
	ExpiresAt int64 `json:"expires_at"`
}

// CreateJoinTokenResponse 在创建时一次性返回入网码明文。
type CreateJoinTokenResponse struct {
	JoinTokenInfo
	Token string `json:"token"`
}

// JoinTokenInfo 返回入网码状态，时间字段使用 Unix 毫秒。
type JoinTokenInfo struct {
	ID    string   `json:"id"`
	Owner string   `json:"owner"`
	Tags  []string `json:"tags"`
	// Reusable 入网码是否可供多个节点注册？
	Reusable  bool  `json:"reusable"`
	UsedCount int   `json:"used_count"`
	ExpiresAt int64 `json:"expires_at"`
	RevokedAt int64 `json:"revoked_at"`
	CreatedAt int64 `json:"created_at"`
}

type ListUsersResult struct {
	Users []WorldUser `json:"users"`
}

// WorldUser 汇总归属于同一个人的节点、有效入网码和规则。
type WorldUser struct {
	Owner       string `json:"owner"`
	NodeCount   int    `json:"node_count"`
	OnlineCount int    `json:"online_count"`
	TokenCount  int    `json:"token_count"`
	RuleCount   int    `json:"rule_count"`
}

type GetACLMatrixRequest struct {
	Rules []ACLRule `json:"rules,omitzero"`
}

// ACLMatrix 以相同的节点顺序排列发送行和接收列。
type ACLMatrix struct {
	Nodes []ACLMatrixNode   `json:"nodes"`
	Cells [][]ACLMatrixCell `json:"cells"`
}

type ACLMatrixNode struct {
	Name  string   `json:"name"`
	Owner string   `json:"owner"`
	Tags  []string `json:"tags"`
}

type ACLMatrixCell struct {
	Allowed bool `json:"allowed"`
	// RuleIndex 指向草稿或已保存列表中的规则，默认规则为 -1。
	RuleIndex int `json:"rule_index"`
}

// ── 控制面权限规则 ──

// ACLRule 按 Priority 从小到大匹配，同优先级按提交顺序匹配。
type ACLRule struct {
	Priority int      `json:"priority" yaml:"priority"`
	From     []string `json:"from" yaml:"from"`
	To       []string `json:"to" yaml:"to"`
	Action   string   `json:"action" yaml:"action"`
}

// ── 域握手身份 ──

// DomainIdentity 是控制面持有的握手身份。
type DomainIdentity struct {
	SignPublic, SignPrivate, BoxPublic, BoxPrivate []byte
}

// ── 域传输与查人接口 ──

// DomainChannels 以本域身份挂靠、接受和发起域查询。
type DomainChannels interface {
	SendTrust(context.Context, DomainIdentity, PeerDomain, DomainTrustMessage, DomainDirectory) error
	GetRelayAttachments() map[string]bool
	RunAttachments(ctx context.Context, local DomainIdentity, relays []string, localAcceptURLs <-chan string, directory DomainDirectory)
	ServeDirect(ctx context.Context, socket *websocket.Conn, local DomainIdentity, directory DomainDirectory) error
	Lookup(ctx context.Context, local DomainIdentity, peer PeerDomain, node string, directory DomainDirectory) (DomainMember, error)
}

// DomainDirectory 按握手公钥授权域查询。
type DomainDirectory interface {
	GetPendingTrustCard(context.Context, ed25519.PublicKey) (string, error)
	ReceiveTrust(context.Context, ed25519.PublicKey, DomainTrustMessage) (DomainTrustMessage, error)
	GetMember(ctx context.Context, caller ed25519.PublicKey, node string) (DomainMember, error)
}

// ── 控制面域通道消息 ──

// DomainTrustMessage 在已验签的域通道上传递邀请或对邀请的决定。
type DomainTrustMessage struct {
	Op       string `json:"op,omitempty"`
	Card     string `json:"card,omitempty"`
	Domain   string `json:"domain,omitempty"`
	Decision string `json:"decision,omitempty"`
}

// ── 控制面持久化记录 ──

const (
	DomainPendingOutbound = "pending_outbound"
	DomainPendingInbound  = "pending_inbound"
	DomainMutual          = "mutual"
	DomainRejected        = "rejected"
)

// PeerDomain 保存管理员导入的域名片和添加时间。
type PeerDomain struct {
	Status    string      `json:"status,omitempty" gorm:"not null;default:mutual"`
	Domain    string      `json:"domain" gorm:"primaryKey"`
	PublicKey []byte      `json:"public_key" gorm:"not null;index"`
	Entries   PeerEntries `json:"entries" gorm:"serializer:json;not null"`
	AddedAt   int64       `json:"added_at,omitempty" gorm:"autoCreateTime:milli"`
}

// ServerMetaRecord 保存本域的签名与密钥协商身份。
type ServerMetaRecord struct {
	ID             uint   `gorm:"primaryKey"`
	Domain         string `gorm:"not null"`
	DomainIdentity `gorm:"embedded"`
	UpdatedAt      int64 `gorm:"autoUpdateTime:milli"`
}

// JoinTokenRecord 保存入网码哈希和使用状态。
type JoinTokenRecord struct {
	ID        string                      `gorm:"primaryKey"`
	TokenHash string                      `gorm:"uniqueIndex;not null"`
	Owner     string                      `gorm:"index;not null"`
	Tags      datatypes.JSONSlice[string] `gorm:"not null"`
	// Reusable 入网码是否可供多个节点注册？
	Reusable  bool  `gorm:"not null"`
	UsedCount int   `gorm:"not null"`
	ExpiresAt int64 `gorm:"index;not null"`
	RevokedAt int64 `gorm:"not null"`
	CreatedAt int64 `gorm:"autoCreateTime:milli"`
}

// NodeRecord 保存已入网节点的身份、归属和吊销状态。
type NodeRecord struct {
	NodeID     string                      `gorm:"primaryKey" json:"node_id"`
	NodeName   string                      `gorm:"uniqueIndex;not null" json:"node_name"`
	Owner      string                      `gorm:"index;not null" json:"owner"`
	Tags       datatypes.JSONSlice[string] `gorm:"not null" json:"tags"`
	SignPublic []byte                      `gorm:"not null" json:"sign_public"`
	BoxPublic  []byte                      `gorm:"not null" json:"box_public"`
	RevokedAt  int64                       `gorm:"index;not null" json:"revoked_at"`
	CreatedAt  int64                       `gorm:"autoCreateTime:milli" json:"created_at"`
}

// ACLRuleRecord 保存一条有序的访问规则。
type ACLRuleRecord struct {
	ID       uint                        `gorm:"primaryKey;autoIncrement"`
	Priority int                         `gorm:"index;not null"`
	From     datatypes.JSONSlice[string] `gorm:"not null"`
	To       datatypes.JSONSlice[string] `gorm:"not null"`
	Action   string                      `gorm:"not null"`
}

// ── 控制面连接状态 ──

type worldServerNodeConnection struct {
	ctx           context.Context
	socket        *websocket.Conn
	node          NodeRecord
	epoch         uint64
	lastHeartbeat time.Time
	lastPing      time.Time
	announcement  NodeAnnounce
}

// AdminNode 将注册记录与当前在线公告合并。
type AdminNode struct {
	NodeRecord
	Domain  string             `json:"domain"`
	Online  bool               `json:"online"`
	Targets []AdvertisedTarget `json:"targets"`
}

// ── 控制面事件 ──

type worldServerEvent interface {
	isWorldServerEvent()
}

// worldServerChallengeAnswered 由连接处理器收到节点签名后提交给事件循环。
type worldServerChallengeAnswered struct {
	handshakeCtx context.Context
	connection   *worldServerNodeConnection
	publicKey    []byte
	result       chan error
}

// worldServerRelayAttachRequested 由域内 relay 验签后发出，控制面串行核对成员并登记挂靠。
type worldServerRelayAttachRequested struct {
	publicKey []byte
	socket    *websocket.Conn
	result    chan error
}

// worldServerNodeDisconnected 由连接处理器退出时提交给事件循环。
type worldServerNodeDisconnected struct {
	connection *worldServerNodeConnection
}

// worldServerControlReceived 由连接处理器读到已认证节点的消息后提交给事件循环。
type worldServerControlReceived struct {
	connection *worldServerNodeConnection
	envelope   ControlEnvelope
}

// worldServerDirectoryChanged 由注册或域管理请求写库后触发，向在线成员推送快照。
type worldServerDirectoryChanged struct{}

// worldServerMemberRequested 由查人入口请求有效成员和在线公告时发出。
type worldServerMemberRequested struct {
	node   string
	result chan worldServerMemberResult
}
type worldServerMemberResult struct {
	member DomainMember
	err    error
}

func (worldServerMemberRequested) isWorldServerEvent() {}

// worldServerNodesRequested 由管理路由请求节点及在线公告快照时发出。
type worldServerNodesRequested struct {
	ctx    context.Context
	result chan worldServerNodesResult
}

type worldServerNodesResult struct {
	nodes []AdminNode
	err   error
}

// worldServerRevocationRequested 由管理路由收到节点或归属吊销请求时发出。
type worldServerRevocationRequested struct {
	nodeID string
	owner  string
	result chan error
}

// worldServerACLReplacementRequested 由管理路由收到完整规则列表时发出。
type worldServerACLReplacementRequested struct {
	rules  []ACLRule
	result chan error
}

func (worldServerRelayAttachRequested) isWorldServerEvent()    {}
func (worldServerChallengeAnswered) isWorldServerEvent()       {}
func (worldServerNodeDisconnected) isWorldServerEvent()        {}
func (worldServerControlReceived) isWorldServerEvent()         {}
func (worldServerDirectoryChanged) isWorldServerEvent()        {}
func (worldServerRevocationRequested) isWorldServerEvent()     {}
func (worldServerACLReplacementRequested) isWorldServerEvent() {}

func (worldServerNodesRequested) isWorldServerEvent() {}

// InvalidAdminRequestError 表示管理请求的字段校验失败。
type InvalidAdminRequestError struct{ error }
