package worldserver

import (
	"bytes"
	"cmp"
	"context"
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/utils/syncmap"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// WorldServer 提供域内联邦的注册、通讯录和接通通知。
type WorldServer interface {
	// RegisterRoutes 将控制面路由挂载到调用方的路由器。
	RegisterRoutes(router gin.IRouter)
	DomainDirectory
	GetRelayAttachments() map[string]bool
	ServeDomainDirect(c *gin.Context)
	GetDomains(ctx context.Context) (ListDomainsResult, error)
	AddDomain(ctx context.Context, card string) (PeerDomain, bool, error)
	DecideDomain(ctx context.Context, domain string, accept bool) error
	DeleteDomain(ctx context.Context, domain string) error
	// ListUsers 汇总节点、有效入网码和规则提及的用户。
	ListUsers(ctx context.Context) ([]WorldUser, error)
	// GetACLMatrix 按已保存的规则或草稿计算节点之间的访问权限。
	GetACLMatrix(ctx context.Context, rules []ACLRule) (ACLMatrix, error)
	GetNodes(ctx context.Context) ([]AdminNode, error)
	RevokeNode(ctx context.Context, nodeID string) error
	RevokeOwner(ctx context.Context, owner string) error
	GetJoinTokens(ctx context.Context) ([]JoinTokenInfo, error)
	CreateJoinToken(ctx context.Context, req CreateJoinTokenRequest) (CreateJoinTokenResponse, error)
	RevokeJoinToken(ctx context.Context, id string) error
	GetACL(ctx context.Context) ([]ACLRule, error)
	ReplaceACL(ctx context.Context, rules []ACLRule) error
	// Run 运行控制面并在取消后等待所有连接退出，只能调用一次。
	Run(ctx context.Context) (err error)
}

type worldServerImpl struct {
	opt   WorldServerOpt
	relay *Relay

	started    atomic.Bool
	requestsMu sync.Mutex
	runCtx     context.Context
	tasks      sync.WaitGroup
	stopped    chan struct{}

	identity    DomainIdentity
	db          *gorm.DB
	events      chan worldServerEvent
	onlineNodes syncmap.SyncMap[string, *worldServerNodeConnection]
	nodeEpochs  map[string]uint64
	acl         []ACLRule
}

// WorldServerOpt 保存域名、数据库路径、管理凭据和初始 ACL。
type WorldServerOpt struct {
	Channels       DomainChannels
	Entries        PeerEntries
	LocalRelayURL  string
	RelayInstances []string
	RelayMode      string
	Domain         string
	DBPath         string
	AdminToken     string
	StaticACL      []ACLRule
	Telemetry      *telemetry.Telemetry
}

const (
	challengeTimeout  = 10 * time.Second
	challengeSkew     = 90 * time.Second
	heartbeatInterval = 30 * time.Second
	heartbeatTimeout  = 90 * time.Second
	controlWriteLimit = 2 * time.Second
	controlSizeLimit  = 1 << 20
)

// NewWorldServerOpt 创建联邦控制面配置。
func NewWorldServerOpt(domain, dbPath, adminToken string) *WorldServerOpt {
	// 1. 保存域名、数据库路径和管理凭据。
	return &WorldServerOpt{
		Domain:     domain,
		DBPath:     dbPath,
		AdminToken: adminToken,
	}
}

// NewWorldServer 校验配置并创建控制面实例。
func NewWorldServer(opt *WorldServerOpt) (WorldServer, error) {
	// 1. 校验域名、管理凭据和初始 ACL。
	if opt == nil || !IsValidDomain(opt.Domain) || strings.TrimSpace(opt.AdminToken) == "" {
		return nil, errors.New("world server requires a valid domain and a non-empty admin token")
	}
	if err := validateACL(opt.StaticACL); err != nil {
		return nil, err
	}

	// 2. 保存配置副本并补齐数据库路径。
	config := *opt
	config.StaticACL = cloneACL(opt.StaticACL)
	if config.DBPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		config.DBPath = filepath.Join(home, ".local", "share", "openvaala", "worldserver", "world.db")
	}
	server := &worldServerImpl{
		opt:        config,
		stopped:    make(chan struct{}),
		events:     make(chan worldServerEvent),
		nodeEpochs: make(map[string]uint64),
	}
	switch config.RelayMode {
	case "", "domain":
		server.relay = NewRelay(nil)
		server.relay.domain = server
	case "public":
		server.relay = NewRelay(config.RelayInstances)
	case "off":
	default:
		return nil, errors.New("invalid world relay mode")
	}
	return server, nil
}

func (s *worldServerImpl) Run(ctx context.Context) (err error) {
	// 1. 初始化域数据库并读取持久化 ACL。
	if !s.started.CompareAndSwap(false, true) {
		return errors.New("WorldServer Run can only be called once")
	}
	ctx = telemetry.ContextWithTelemetry(ctx, s.opt.Telemetry)
	db, closeDB, err := openSQLiteDB(s.opt.DBPath, s.opt.Domain, s.opt.StaticACL)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := closeDB(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	var meta ServerMetaRecord
	if err := db.First(&meta, 1).Error; err != nil {
		return err
	}
	s.identity = meta.DomainIdentity
	s.acl, err = getACL(db.WithContext(ctx))
	if err != nil {
		return err
	}

	// 2. 发布请求上下文，由事件循环串行处理在线节点、ACL 与心跳。
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.requestsMu.Lock()
	s.db = db
	s.runCtx = runCtx
	s.requestsMu.Unlock()
	if s.opt.Channels != nil {
		if len(s.opt.Entries.DirectURLs)+len(s.opt.Entries.Relays) == 0 {
			telemetry.GetLogger(ctx).Info("configure world.node.direct_urls or world.relays to publish a domain card")
		}
		var localAcceptURLs chan string
		if s.relay != nil && s.opt.LocalRelayURL != "" {
			localAcceptURLs = make(chan string)
			s.relay.attach(s.identity.SignPublic, relayAttachment{local: localAcceptURLs, localBase: s.opt.LocalRelayURL})
		}
		s.tasks.Go(func() {
			s.opt.Channels.RunAttachments(runCtx, s.identity, s.opt.Entries.Relays, localAcceptURLs, s)
		})
	}
	if s.relay != nil {
		s.tasks.Go(func() { s.relay.Run(runCtx) })
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for runCtx.Err() == nil {
		select {
		case event := <-s.events:
			s.handleWorldServerEvent(runCtx, event)
		case now := <-ticker.C:
			s.handleHeartbeats(runCtx, now)
		case <-runCtx.Done():
		}
	}

	// 3. 停止接收请求，断开并等待节点连接。
	s.requestsMu.Lock()
	s.runCtx = nil
	close(s.stopped)
	s.requestsMu.Unlock()
	s.onlineNodes.Range(func(name string, connection *worldServerNodeConnection) bool {
		s.onlineNodes.Delete(name)
		_ = connection.socket.CloseNow()
		return true
	})
	s.tasks.Wait()
	return ctx.Err()
}

func (s *worldServerImpl) GetRelayAttachments() map[string]bool {
	if s.opt.Channels == nil {
		return nil
	}
	return s.opt.Channels.GetRelayAttachments()
}

func (s *worldServerImpl) RegisterRoutes(router gin.IRouter) {
	// 1. 注册健康检查、节点注册和常驻连接。
	if s.relay != nil {
		s.relay.RegisterRoutes(router)
	}
	if s.relay == nil || s.relay.domain != nil {
		// 跨域浏览器也能识别并缓存控制面没有公开实例列表的 404。
		router.GET("/world/v1/relay/instances", func(c *gin.Context) {
			c.Header("Access-Control-Allow-Origin", "*")
			c.Status(http.StatusNotFound)
		})
	}
	api := router.Group("/world/v1", s.handleWorldRequest)
	api.GET("/healthz", HandleHealthz)
	api.POST("/nodes/register", s.handleRegisterNode)
	api.GET("/connect", s.handleConnect)

	// 2. 为管理路由统一校验 Bearer 凭据。
	admin := router.Group("/world/v1/admin", handleAdminCORS)
	admin.OPTIONS("/*path", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	admin.Use(s.handleWorldRequest, s.handleAdminToken)
	admin.POST("/tokens", s.handleCreateJoinToken)
	admin.GET("/tokens", s.handleGetJoinTokens)
	admin.DELETE("/tokens/:id", s.handleRevokeJoinToken)
	admin.GET("/users", s.handleListUsers)
	admin.POST("/acl/matrix", s.handleGetACLMatrix)
	admin.GET("/nodes", s.handleGetNodes)
	admin.POST("/nodes/:node_id/revoke", s.handleRevokeNode)
	admin.POST("/owners/:owner/revoke", s.handleRevokeOwner)
	admin.GET("/domains", s.handleGetDomains)
	admin.POST("/domains", s.handleAddDomain)
	for _, decision := range []string{"accept", "reject"} {
		admin.POST("/domains/:domain/"+decision, func(c *gin.Context) {
			writeAdminResponse(c, http.StatusNoContent, nil, s.DecideDomain(c.Request.Context(), c.Param("domain"), decision == "accept"))
		})
	}
	admin.DELETE("/domains/:domain", s.handleDeleteDomain)
	admin.GET("/acl", s.handleGetACL)
	admin.PUT("/acl", s.handleReplaceACL)
}

// HandleHealthz 为 World 控制面和独立 relay 返回空的成功响应。
func HandleHealthz(c *gin.Context) {
	c.Status(http.StatusOK)
}

// handleAdminCORS 为跨源管理请求设置响应头。
func handleAdminCORS(c *gin.Context) {
	c.Header("Access-Control-Allow-Origin", "*")
	c.Header("Access-Control-Allow-Headers", "Authorization, Content-Type")
	c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	c.Next()
}

// handleWorldRequest 为控制面请求绑定关闭信号。
func (s *worldServerImpl) handleWorldRequest(c *gin.Context) {
	// 1. 获取运行中的控制面上下文。
	s.requestsMu.Lock()
	runCtx := s.runCtx
	s.requestsMu.Unlock()
	if runCtx == nil || runCtx.Err() != nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}

	// 2. 将 HTTP 请求与控制面关闭信号绑定。
	ctx, cancel := context.WithCancel(telemetry.ContextWithTelemetry(c.Request.Context(), s.opt.Telemetry))
	defer cancel()
	stop := context.AfterFunc(runCtx, cancel)
	defer stop()
	c.Request = c.Request.WithContext(ctx)
	if c.Request.Body != nil {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, controlSizeLimit)
	}
	c.Next()
}

// handleRegisterNode 验证入网签名并使用入网码登记节点。
func (s *worldServerImpl) handleRegisterNode(c *gin.Context) {
	// 1. 读取请求并校验节点名、两把公钥和签名。
	var req RegisterNodeRequest
	if c.ShouldBindJSON(&req) != nil || !nodeNamePattern.MatchString(req.NodeName) ||
		req.JoinToken == "" || len(req.SignPublic) != ed25519.PublicKeySize || len(req.BoxPublic) != 32 {
		writeHTTPError(c, http.StatusBadRequest, "invalid registration")
		return
	}
	if !ed25519.Verify(req.SignPublic, RegisterSignBytes(req), req.Signature) {
		writeHTTPError(c, http.StatusUnauthorized, "invalid registration signature")
		return
	}

	// 2. 核销入网码并登记节点，通知在线节点刷新通讯录。
	ctx := c.Request.Context()
	node, err := registerNode(s.db.WithContext(ctx), req)
	switch {
	case errors.Is(err, errInvalidJoinToken):
		writeHTTPError(c, http.StatusUnauthorized, err.Error())
		return
	case errors.Is(err, errNodeConflict):
		writeHTTPError(c, http.StatusConflict, err.Error())
		return
	case err != nil:
		writeStoreError(c, err)
		return
	}
	_ = s.submitWorldServerEvent(context.WithoutCancel(ctx), worldServerDirectoryChanged{})
	c.JSON(http.StatusCreated, RegisterNodeResponse{
		NodeID:   node.NodeID,
		NodeName: node.NodeName,
		Domain:   s.opt.Domain,
		Owner:    node.Owner,
		Tags:     node.Tags,
	})
}

// handleConnect 发送挑战并读取认证连接上的控制消息。
func (s *worldServerImpl) handleConnect(c *gin.Context) {
	// 1. 登记 WebSocket 处理任务。
	s.requestsMu.Lock()
	if s.runCtx == nil || s.runCtx.Err() != nil {
		s.requestsMu.Unlock()
		c.Status(http.StatusServiceUnavailable)
		return
	}
	s.tasks.Add(1)
	s.requestsMu.Unlock()
	defer s.tasks.Done()

	// 2. 升级连接，退出时向事件循环提交连接清理。
	socket, err := websocket.Accept(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	socket.SetReadLimit(controlSizeLimit)
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	connection := &worldServerNodeConnection{socket: socket, ctx: ctx}
	defer func() {
		_ = socket.CloseNow()
		_ = s.submitWorldServerEvent(context.WithoutCancel(ctx), worldServerNodeDisconnected{connection: connection})
	}()

	// 3. 发送随机挑战并等待节点提供身份证明。
	if err := s.authenticateConnection(ctx, connection); err != nil {
		if ctx.Err() == nil {
			if errors.Is(err, errNodeRevoked) {
				_ = writeControl(ctx, socket, ControlTypeNodeRevoked, NodeRevoked{Code: "world_node_revoked"})
			}
			_ = socket.Close(websocket.StatusPolicyViolation, "authentication rejected")
		}
		return
	}

	if s.relay != nil {
		if err := writeControl(ctx, socket, ControlTypeRelayAttached, nil); err != nil {
			return
		}
	}
	// 4. 读取控制信封并提交给事件循环。
	for {
		var envelope ControlEnvelope
		if err := wsjson.Read(ctx, socket, &envelope); err != nil {
			return
		}
		if err := s.submitWorldServerEvent(ctx, worldServerControlReceived{
			connection: connection,
			envelope:   envelope,
		}); err != nil {
			return
		}
	}
}

// authenticateConnection 向节点发出挑战并等待控制面验签结果。
func (s *worldServerImpl) authenticateConnection(ctx context.Context, connection *worldServerNodeConnection) error {
	// 1. 发出随机挑战，并在握手期限内读取证明。
	ctx, cancel := context.WithTimeout(ctx, challengeTimeout)
	defer cancel()
	key, err := readChallengeProof(ctx, connection.socket, ChallengeConnect)
	if err != nil {
		return err
	}
	// 2. 提交证明并等待登记结果。
	result := make(chan error, 1)
	err = s.submitWorldServerEvent(ctx, worldServerChallengeAnswered{
		handshakeCtx: ctx,
		connection:   connection,
		publicKey:    key,
		result:       result,
	})
	if err != nil {
		return err
	}
	return awaitWorldServerResult(ctx, result)
}

// handleWorldServerEvent 更新联邦连接和权限状态。
func (s *worldServerImpl) handleWorldServerEvent(ctx context.Context, event worldServerEvent) {
	// 1. 按事件类型处理连接、消息和管理面更新。
	switch event := event.(type) {
	case worldServerChallengeAnswered:
		err := s.handleChallengeAnswered(ctx, event)
		event.result <- err
		if err == nil {
			s.pushDirectories(ctx)
		}
	case worldServerRelayAttachRequested:
		_, err := s.getActiveMember(ctx, event.publicKey)
		if err == nil {
			s.relay.attach(event.publicKey, relayAttachment{socket: event.socket, independent: true})
		}
		event.result <- err
	case worldServerNodeDisconnected:
		connection := event.connection
		if s.relay != nil {
			s.relay.detach(connection.node.SignPublic, connection.socket)
		}
		if s.onlineNodes.DeleteIf(connection.node.NodeName, func(online *worldServerNodeConnection) bool {
			return online.epoch == connection.epoch
		}) {
			s.pushDirectories(ctx)
		}
	case worldServerControlReceived:
		s.handleControlReceived(ctx, event)
	case worldServerDirectoryChanged:
		s.pushDirectories(ctx)
	case worldServerMemberRequested:
		var node NodeRecord
		err := s.db.WithContext(ctx).Where("node_name = ? AND revoked_at = 0", event.node).Take(&node).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			err = PeerNotFoundError(event.node + "@" + s.opt.Domain)
		}
		event.result <- worldServerMemberResult{DomainMember{node.SignPublic, node.Owner, s.directoryNode(node).Entries}, err}
	case worldServerNodesRequested:
		nodes, err := getNodes(s.db.WithContext(event.ctx), true)
		views := make([]AdminNode, len(nodes))
		for i, node := range nodes {
			view := s.directoryNode(node)
			views[i] = AdminNode{NodeRecord: node, Domain: view.Domain, Online: view.Online, Targets: append([]AdvertisedTarget{}, view.Targets...)}
		}
		event.result <- worldServerNodesResult{nodes: views, err: err}
	case worldServerRevocationRequested:
		event.result <- s.handleRevocationRequested(ctx, event)
	case worldServerACLReplacementRequested:
		err := replaceACL(s.db.WithContext(ctx), event.rules)
		if err == nil {
			s.acl = sortedACL(cloneACL(event.rules))
			s.pushDirectories(ctx)
		}
		event.result <- err
	}
}

// handleChallengeAnswered 校验节点持有的身份并登记新连接。
func (s *worldServerImpl) handleChallengeAnswered(ctx context.Context, event worldServerChallengeAnswered) error {
	// 1. 读取未吊销的节点，检查时间戳和本次挑战签名。
	node, err := s.getActiveMember(ctx, event.publicKey)
	if err != nil {
		return err
	}
	now := time.Now()
	// 2. 确认握手仍有效后再递增 epoch、登记新连接并关闭旧连接。
	if event.handshakeCtx != nil && event.handshakeCtx.Err() != nil {
		return event.handshakeCtx.Err()
	}
	connection := event.connection
	s.nodeEpochs[node.NodeID]++
	connection.node = node
	connection.epoch = s.nodeEpochs[node.NodeID]
	connection.lastHeartbeat = now
	connection.lastPing = now
	previous, replaced := s.onlineNodes.Load(node.NodeName)
	s.onlineNodes.Store(node.NodeName, connection)
	if s.relay != nil {
		s.relay.attach(node.SignPublic, relayAttachment{socket: connection.socket})
	}
	if replaced {
		s.tasks.Go(func() {
			_ = previous.socket.Close(websocket.StatusPolicyViolation, "node reconnected")
		})
	}
	return nil
}

// handleControlReceived 处理已登记节点的公告和心跳。
func (s *worldServerImpl) handleControlReceived(ctx context.Context, event worldServerControlReceived) {
	// 1. 检查消息来源是否仍登记在该节点的 epoch 下。
	connection := event.connection
	online, found := s.onlineNodes.Load(connection.node.NodeName)
	if !found || online.epoch != connection.epoch {
		return
	}

	// 2. 按控制帧更新公告或心跳。
	switch event.envelope.Type {
	case ControlTypeDomainLookup:
		var req DomainLookup
		if json.Unmarshal(event.envelope.Payload, &req) != nil || req.RequestID == "" {
			return
		}
		s.tasks.Go(func() {
			result := s.lookupDomainMember(connection.ctx, req)
			_ = writeControl(connection.ctx, connection.socket, ControlTypeDomainLookupResult, result)
		})
	case ControlTypeNodeAnnounce:
		var announcement NodeAnnounce
		if json.Unmarshal(event.envelope.Payload, &announcement) == nil && isValidAnnouncement(announcement) {
			connection.announcement = announcement
			s.pushDirectories(ctx)
		}
	case ControlTypePing:
		connection.lastHeartbeat = time.Now()
		_ = writeControl(ctx, connection.socket, ControlTypePong, nil)
	case ControlTypePong:
		connection.lastHeartbeat = time.Now()
	}
}

// getActiveMember 按签名公钥查找未被吊销的域成员。
func (s *worldServerImpl) getActiveMember(ctx context.Context, key []byte) (NodeRecord, error) {
	node, err := getNode(s.db.WithContext(ctx), GetNodeIDFromSignPublic(key))
	if err != nil {
		return node, err
	}
	if node.RevokedAt != 0 {
		return node, errNodeRevoked
	}
	if !bytes.Equal(node.SignPublic, key) {
		return node, errors.New("invalid node identity")
	}
	return node, nil
}

// pushDirectories 向每个在线节点发送其 ACL 可见的完整通讯录。
func (s *worldServerImpl) pushDirectories(ctx context.Context) {
	// 1. 读取未吊销节点并合并在线公告。
	if s.onlineNodes.Len() == 0 {
		return
	}
	nodes, err := getNodes(s.db.WithContext(ctx), false)
	if err != nil {
		telemetry.GetLogger(ctx).Warn("read world directory", "error", err)
		return
	}
	domains, err := getPeerDomains(s.db.WithContext(ctx).Where("status = ?", DomainMutual))
	if err != nil {
		telemetry.GetLogger(ctx).Warn("read peer domains", "error", err)
		return
	}
	directory := make([]DirectoryNode, len(nodes))
	for i, node := range nodes {
		directory[i] = s.directoryNode(node)
	}

	// 2. 按接收节点的访问权限过滤并发送完整通讯录；允许入站的来源也下发公钥以便验签握手。
	s.onlineNodes.Range(func(_ string, connection *worldServerNodeConnection) bool {
		visible := make([]DirectoryNode, 0, len(directory))
		for i, node := range nodes {
			canSend, _ := isAllowedByACL(s.acl, connection.node, node)
			canReceive, _ := isAllowedByACL(s.acl, node, connection.node)
			if canSend || canReceive {
				directoryNode := directory[i]
				directoryNode.CanSend = canSend
				directoryNode.CanReceive = canReceive
				visible = append(visible, directoryNode)
			}
		}
		_ = writeControl(ctx, connection.socket, ControlTypeDirectory, Directory{Nodes: visible, Domains: domains})
		return ctx.Err() == nil
	})
}

// directoryNode 合并事件循环持有的在线公告。
func (s *worldServerImpl) directoryNode(node NodeRecord) DirectoryNode {
	view := DirectoryNode{
		NodeID: node.NodeID, NodeName: node.NodeName, Domain: s.opt.Domain, Owner: node.Owner, Tags: node.Tags,
		SignPublic: node.SignPublic,
	}
	if connection, online := s.onlineNodes.Load(node.NodeName); online && node.RevokedAt == 0 {
		view.Online = true
		view.Entries = connection.announcement.Entries
		view.Targets = connection.announcement.Targets
	}
	return view
}

// handleHeartbeats 发送心跳并移除超时连接。
func (s *worldServerImpl) handleHeartbeats(ctx context.Context, now time.Time) {
	// 1. 移除超时连接并给到期连接发 ping。
	changed := false
	s.onlineNodes.Range(func(name string, connection *worldServerNodeConnection) bool {
		if now.Sub(connection.lastHeartbeat) >= heartbeatTimeout {
			s.onlineNodes.Delete(name)
			_ = connection.socket.CloseNow()
			changed = true
		} else if now.Sub(connection.lastPing) >= heartbeatInterval {
			connection.lastPing = now
			_ = writeControl(ctx, connection.socket, ControlTypePing, nil)
		}
		return ctx.Err() == nil
	})

	// 2. 有连接被移除时推送新的通讯录。
	if changed {
		s.pushDirectories(ctx)
	}
}

// handleRevocationRequested 持久化吊销并踢下受影响的节点。
func (s *worldServerImpl) handleRevocationRequested(ctx context.Context, event worldServerRevocationRequested) error {
	// 1. 记录吊销并取得受影响的节点名。
	nodes, err := revokeNodes(s.db.WithContext(ctx), event.nodeID, event.owner)
	if err != nil {
		return err
	}

	// 2. 移除在线登记，发送吊销通知并关闭连接。
	for _, node := range nodes {
		if connection, online := s.onlineNodes.LoadAndDelete(node.NodeName); online {
			_ = writeControl(ctx, connection.socket, ControlTypeNodeRevoked, NodeRevoked{NodeID: connection.node.NodeID})
			_ = connection.socket.CloseNow()
		}
		if s.relay != nil {
			s.relay.revokeMember(node.SignPublic)
		}
	}
	if len(nodes) > 0 {
		s.pushDirectories(ctx)
	}
	return nil
}

// handleAdminToken 校验控制面管理凭据。
func (s *worldServerImpl) handleAdminToken(c *gin.Context) {
	// 1. 提取 Bearer 值并与配置凭据作常时比较。
	token, ok := strings.CutPrefix(c.GetHeader("Authorization"), "Bearer ")
	if !ok || subtle.ConstantTimeCompare([]byte(token), []byte(s.opt.AdminToken)) != 1 {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	c.Next()
}

func (s *worldServerImpl) GetNodes(ctx context.Context) ([]AdminNode, error) {
	result := make(chan worldServerNodesResult, 1)
	if err := s.submitWorldServerEvent(ctx, worldServerNodesRequested{ctx: ctx, result: result}); err != nil {
		return nil, err
	}
	select {
	case view := <-result:
		return view.nodes, view.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ListUsers 按人汇总节点在线状态、有效入网码和规则引用。
func (s *worldServerImpl) ListUsers(ctx context.Context) ([]WorldUser, error) {
	// 1. 读取带在线状态的节点、入网码和规则。
	nodes, err := s.GetNodes(ctx)
	if err != nil {
		return nil, err
	}
	tokens, err := s.GetJoinTokens(ctx)
	if err != nil {
		return nil, err
	}
	rules, err := s.GetACL(ctx)
	if err != nil {
		return nil, err
	}

	// 2. 按归属统计节点和入网码，每条规则对同一人只计一次。
	users := make(map[string]*WorldUser)
	user := func(owner string) *WorldUser {
		if users[owner] == nil {
			users[owner] = &WorldUser{Owner: owner}
		}
		return users[owner]
	}
	for _, node := range nodes {
		u := user(node.Owner)
		if node.RevokedAt == 0 {
			u.NodeCount++
		}
		if node.Online {
			u.OnlineCount++
		}
	}
	now := time.Now().UnixMilli()
	for _, token := range tokens {
		u := user(token.Owner)
		if token.RevokedAt == 0 && token.ExpiresAt > now && (token.Reusable || token.UsedCount == 0) {
			u.TokenCount++
		}
	}
	for _, rule := range rules {
		mentioned := make(map[string]bool)
		for _, selectors := range [][]string{rule.From, rule.To} {
			for _, selector := range selectors {
				owner, ok := strings.CutPrefix(selector, "owner:")
				if ok && owner != "*" && !mentioned[owner] {
					user(owner).RuleCount++
					mentioned[owner] = true
				}
			}
		}
	}
	// 3. 按归属名称返回汇总结果。
	result := make([]WorldUser, 0, len(users))
	for _, owner := range slices.Sorted(maps.Keys(users)) {
		result = append(result, *users[owner])
	}
	return result, nil
}

// GetACLMatrix 按已保存的规则或提交的草稿计算未吊销节点之间的访问权限。
func (s *worldServerImpl) GetACLMatrix(ctx context.Context, rules []ACLRule) (ACLMatrix, error) {
	// 1. 读取已保存的规则，或校验草稿并按优先级排序。
	db, err := s.getAdminDB(ctx)
	if err != nil {
		return ACLMatrix{}, err
	}
	var draftIndices []int
	if rules == nil {
		rules, err = getACL(db)
		if err != nil {
			return ACLMatrix{}, err
		}
	} else {
		if err := validateACL(rules); err != nil {
			return ACLMatrix{}, &InvalidAdminRequestError{err}
		}
		// 保留草稿下标，预览结果引用请求中的那条规则。
		draftIndices = make([]int, len(rules))
		for i := range rules {
			draftIndices[i] = i
		}
		slices.SortStableFunc(draftIndices, func(a, b int) int { return cmp.Compare(rules[a].Priority, rules[b].Priority) })
		orderedRules := make([]ACLRule, len(rules))
		for i, index := range draftIndices {
			orderedRules[i] = rules[index]
		}
		rules = orderedRules
	}
	// 2. 计算每一对未吊销节点的访问结果和命中规则。
	nodes, err := getNodes(db, false)
	if err != nil {
		return ACLMatrix{}, err
	}
	matrix := ACLMatrix{
		Nodes: make([]ACLMatrixNode, len(nodes)),
		Cells: make([][]ACLMatrixCell, len(nodes)),
	}
	for i, from := range nodes {
		matrix.Nodes[i] = ACLMatrixNode{Name: from.NodeName, Owner: from.Owner, Tags: append([]string{}, from.Tags...)}
		matrix.Cells[i] = make([]ACLMatrixCell, len(nodes))
		for j, to := range nodes {
			allowed, index := isAllowedByACL(rules, from, to)
			if draftIndices != nil && index >= 0 {
				index = draftIndices[index]
			}
			matrix.Cells[i][j] = ACLMatrixCell{Allowed: allowed, RuleIndex: index}
		}
	}
	return matrix, nil
}

func (s *worldServerImpl) RevokeNode(ctx context.Context, nodeID string) error {
	if nodeID == "" {
		return &InvalidAdminRequestError{errors.New("node_id is required")}
	}
	return s.revokeNodes(ctx, nodeID, "")
}

func (s *worldServerImpl) RevokeOwner(ctx context.Context, owner string) error {
	if owner == "" {
		return &InvalidAdminRequestError{errors.New("owner is required")}
	}
	return s.revokeNodes(ctx, "", owner)
}

func (s *worldServerImpl) revokeNodes(ctx context.Context, nodeID, owner string) error {
	result := make(chan error, 1)
	if err := s.submitWorldServerEvent(ctx, worldServerRevocationRequested{nodeID: nodeID, owner: owner, result: result}); err != nil {
		return err
	}
	return awaitWorldServerResult(ctx, result)
}

func (s *worldServerImpl) GetJoinTokens(ctx context.Context) ([]JoinTokenInfo, error) {
	db, err := s.getAdminDB(ctx)
	if err != nil {
		return nil, err
	}
	return getJoinTokens(db)
}

func (s *worldServerImpl) CreateJoinToken(ctx context.Context, req CreateJoinTokenRequest) (CreateJoinTokenResponse, error) {
	if err := validateJoinTokenRequest(req); err != nil {
		return CreateJoinTokenResponse{}, &InvalidAdminRequestError{err}
	}
	db, err := s.getAdminDB(ctx)
	if err != nil {
		return CreateJoinTokenResponse{}, err
	}
	base := s.opt.LocalRelayURL
	if len(s.opt.Entries.DirectURLs) > 0 {
		base = strings.TrimSuffix(strings.TrimRight(s.opt.Entries.DirectURLs[0], "/"), "/world/v1/direct")
		if strings.HasPrefix(base, "ws") {
			base = "http" + strings.TrimPrefix(base, "ws")
		}
	}
	return createJoinToken(db, req, strings.TrimRight(base, "/"))
}

func (s *worldServerImpl) RevokeJoinToken(ctx context.Context, id string) error {
	if id == "" {
		return &InvalidAdminRequestError{errors.New("id is required")}
	}
	db, err := s.getAdminDB(ctx)
	if err != nil {
		return err
	}
	return revokeJoinToken(db, id)
}

func (s *worldServerImpl) GetACL(ctx context.Context) ([]ACLRule, error) {
	db, err := s.getAdminDB(ctx)
	if err != nil {
		return nil, err
	}
	return getACL(db)
}

const expectedACLRuleArray = "expected an ACL rule array"

func (s *worldServerImpl) ReplaceACL(ctx context.Context, rules []ACLRule) error {
	if rules == nil {
		return &InvalidAdminRequestError{errors.New(expectedACLRuleArray)}
	}
	if err := validateACL(rules); err != nil {
		return &InvalidAdminRequestError{err}
	}
	result := make(chan error, 1)
	if err := s.submitWorldServerEvent(ctx, worldServerACLReplacementRequested{rules: rules, result: result}); err != nil {
		return err
	}
	return awaitWorldServerResult(ctx, result)
}

func (s *worldServerImpl) getAdminDB(ctx context.Context) (*gorm.DB, error) {
	s.requestsMu.Lock()
	defer s.requestsMu.Unlock()
	if s.runCtx == nil || s.runCtx.Err() != nil {
		return nil, context.Canceled
	}
	return s.db.WithContext(telemetry.ContextWithTelemetry(ctx, s.opt.Telemetry)), nil
}

func (s *worldServerImpl) handleCreateJoinToken(c *gin.Context) {
	var req CreateJoinTokenRequest
	if c.ShouldBindJSON(&req) != nil {
		writeHTTPError(c, http.StatusBadRequest, "join token requires an owner and a future expires_at")
		return
	}
	token, err := s.CreateJoinToken(c.Request.Context(), req)
	writeAdminResponse(c, http.StatusCreated, token, err)
}

func (s *worldServerImpl) handleGetJoinTokens(c *gin.Context) {
	tokens, err := s.GetJoinTokens(c.Request.Context())
	writeAdminResponse(c, http.StatusOK, tokens, err)
}

func (s *worldServerImpl) handleRevokeJoinToken(c *gin.Context) {
	err := s.RevokeJoinToken(c.Request.Context(), c.Param("id"))
	writeAdminResponse(c, http.StatusNoContent, nil, err)
}

func (s *worldServerImpl) handleListUsers(c *gin.Context) {
	users, err := s.ListUsers(c.Request.Context())
	writeAdminResponse(c, http.StatusOK, ListUsersResult{Users: users}, err)
}

func (s *worldServerImpl) handleGetACLMatrix(c *gin.Context) {
	var req GetACLMatrixRequest
	if c.ShouldBindJSON(&req) != nil {
		writeHTTPError(c, http.StatusBadRequest, expectedACLRuleArray)
		return
	}
	matrix, err := s.GetACLMatrix(c.Request.Context(), req.Rules)
	writeAdminResponse(c, http.StatusOK, matrix, err)
}

func (s *worldServerImpl) handleGetNodes(c *gin.Context) {
	nodes, err := s.GetNodes(c.Request.Context())
	writeAdminResponse(c, http.StatusOK, nodes, err)
}

func (s *worldServerImpl) handleRevokeNode(c *gin.Context) {
	err := s.RevokeNode(c.Request.Context(), c.Param("node_id"))
	writeAdminResponse(c, http.StatusNoContent, nil, err)
}

func (s *worldServerImpl) handleRevokeOwner(c *gin.Context) {
	err := s.RevokeOwner(c.Request.Context(), c.Param("owner"))
	writeAdminResponse(c, http.StatusNoContent, nil, err)
}

func (s *worldServerImpl) handleGetACL(c *gin.Context) {
	rules, err := s.GetACL(c.Request.Context())
	writeAdminResponse(c, http.StatusOK, rules, err)
}

func (s *worldServerImpl) handleReplaceACL(c *gin.Context) {
	var rules []ACLRule
	if c.ShouldBindJSON(&rules) != nil {
		writeHTTPError(c, http.StatusBadRequest, expectedACLRuleArray)
		return
	}
	err := s.ReplaceACL(c.Request.Context(), rules)
	writeAdminResponse(c, http.StatusNoContent, nil, err)
}

// submitWorldServerEvent 向仍运行的控制面交付一个事件。
func (s *worldServerImpl) submitWorldServerEvent(ctx context.Context, event worldServerEvent) error {
	// 1. 等待事件循环接收、请求取消或控制面停止。
	select {
	case s.events <- event:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-s.stopped:
		return context.Canceled
	}
}

// awaitWorldServerResult 等待事件处理完成。
func awaitWorldServerResult(ctx context.Context, result <-chan error) error {
	// 1. 返回处理结果或请求取消错误。
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// writeControl 发送一条带写入期限的控制面消息。
func writeControl(ctx context.Context, socket *websocket.Conn, messageType string, payload any) error {
	// 1. 编码信封并在期限内发送。
	envelope, err := controlEnvelope(messageType, payload)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, controlWriteLimit)
	defer cancel()
	return wsjson.Write(ctx, socket, envelope)
}
