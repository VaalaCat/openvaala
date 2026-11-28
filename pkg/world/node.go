package world

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/storage"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/utils/syncmap"
	"github.com/VaalaCat/openvaala/pkg/worldserver"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/gin-gonic/gin"
)

// WorldRPCServer 接受节点已完成握手的 RPC 通道。
type WorldRPCServer interface {
	ServeWorldRPC(ctx context.Context, conn SecureChannel) error
}

// WorldNode 把本机对话接入联邦通讯录和端到端消息通道。
type WorldNode interface {
	executor.ToolProvider
	GetStatus(ctx context.Context, token string) (NodeStatus, error)
	ListPeers(ctx context.Context) (ListPeersResult, error)
	SendMessage(ctx context.Context, req SendMessageRequest) (WorldMessage, error)
	// EnqueueMessage 入队后返回消息和可选的结果通道。
	EnqueueMessage(ctx context.Context, req SendMessageRequest) (WorldMessage, <-chan WorldMessage, error)
	// WaitMessage 等待入队消息的投递结果。
	WaitMessage(ctx context.Context, message WorldMessage, finished <-chan WorldMessage) (WorldMessage, error)
	Forward(ctx context.Context, rt runtime.Runtime, req ForwardRequest) (WorldMessage, error)
	ListMessages(ctx context.Context, params ListMessagesParams) (MessagePage, error)
	CancelMessage(ctx context.Context, messageID string) (WorldMessage, error)
	ListThreads(ctx context.Context, params ListThreadsParams) (ThreadPage, error)
	Subscribe(ctx context.Context) (<-chan NodeNotification, error)
	// ServeNodeDirect 接受以节点身份握手的直连。
	ServeNodeDirect(c *gin.Context)
	// Run 运行节点，退出时取消并等待连接、握手和来信任务。
	Run(ctx context.Context, host WorldHost, rpc WorldRPCServer) error
}

type worldNodeImpl struct {
	peerDialer        PeerDialer
	opt               WorldNodeOpt
	address           Address
	events            chan nodeEvent
	stopped           chan struct{}
	started           atomic.Bool
	tasks             sync.WaitGroup
	host              WorldHost
	rpc               WorldRPCServer
	homeIsPublicEntry bool

	revoked              bool
	control              *websocket.Conn
	controlDone          chan struct{}
	domains              map[string]worldserver.PeerDomain
	memberCache          syncmap.SyncMap[memberCacheKey, cachedMember]
	lookupWaiters        syncmap.SyncMap[string, memberLookupWaiter]
	replyChannels        map[string]*peerChannel
	directory            map[string]worldserver.DirectoryNode
	channels             map[*peerChannel]struct{}
	attachments          map[string]bool
	lastReceivedSequence uint64
	threadQueues         map[string][]WorldMessage
	notificationRunning  map[string]bool
	waiters              map[string]messageWaiter
	subscribers          map[chan NodeNotification]struct{}
}

// WorldNodeOpt 保存本机身份、通告目标、控制面地址与独立记录库。
type WorldNodeOpt struct {
	Records          storage.RecordStorage
	Identity         NodeIdentity
	NodeName         string
	Domain           string
	ServerURL        string
	WebURL           string
	RemoteAccessName string
	Relays           []string
	DirectURLs       []string
	DefaultTarget    string
	AdvertiseTargets []worldserver.AdvertisedTarget
	DefaultWait      time.Duration
	Telemetry        *telemetry.Telemetry
}

// NewWorldNodeOpt 设置节点的必需依赖。
func NewWorldNodeOpt(records storage.RecordStorage, identity NodeIdentity, nodeName, domain, serverURL string) *WorldNodeOpt {
	// 1. 保存地址和持久身份。
	return &WorldNodeOpt{
		Records:   records,
		Identity:  identity,
		NodeName:  nodeName,
		Domain:    domain,
		ServerURL: serverURL,
	}
}

// NewWorldNode 校验节点配置并准备事件循环。
func NewWorldNode(opt *WorldNodeOpt) (WorldNode, error) {
	// 1. 校验本地依赖、控制面地址和通告目标。
	if opt == nil || opt.Records == nil {
		return nil, errors.New("world node requires record storage")
	}
	if _, err := opt.Identity.validateKeys(); err != nil {
		return nil, err
	}
	address := Address{Node: opt.NodeName, Domain: opt.Domain}
	if !nodeOrTargetPattern.MatchString(address.Node) || !worldserver.IsValidDomain(address.Domain) {
		return nil, ErrInvalidWorldAddress
	}

	config := *opt
	config.WebURL = SelectRemoteAccessPage(opt.WebURL, opt.Relays, opt.DirectURLs)
	config.Relays = nil
	homeIsPublicEntry := false
	for i, base := range append([]string{opt.ServerURL}, opt.Relays...) {
		endpoint, err := GetWorldSocketURL(base, "")
		if err != nil {
			return nil, err
		}
		if i > 0 && endpoint == config.Relays[0] {
			homeIsPublicEntry = true
		}
		if !slices.Contains(config.Relays, endpoint) {
			config.Relays = append(config.Relays, endpoint)
		}
	}
	config.DirectURLs = slices.Clone(opt.DirectURLs)
	config.AdvertiseTargets = slices.Clone(opt.AdvertiseTargets)
	if err := ValidateAdvertisedTargets(config.DefaultTarget, config.AdvertiseTargets); err != nil {
		return nil, err
	}
	if config.DefaultWait <= 0 {
		config.DefaultWait = 2 * time.Minute
	}
	// 2. 创建由节点循环独占的连接、收信队列与等待者集合。
	return &worldNodeImpl{
		opt: config, address: address, homeIsPublicEntry: homeIsPublicEntry,
		events:              make(chan nodeEvent),
		stopped:             make(chan struct{}),
		replyChannels:       make(map[string]*peerChannel),
		channels:            make(map[*peerChannel]struct{}),
		attachments:         make(map[string]bool),
		threadQueues:        make(map[string][]WorldMessage),
		notificationRunning: make(map[string]bool),
		waiters:             make(map[string]messageWaiter),
		subscribers:         make(map[chan NodeNotification]struct{}),
	}, nil
}

// Run 恢复消息记录并依次处理节点事件。
func (n *worldNodeImpl) Run(ctx context.Context, host WorldHost, rpc WorldRPCServer) error {
	// 1. 绑定运行期依赖及所有短任务的退出信号。
	if !n.started.CompareAndSwap(false, true) {
		return errors.New("WorldNode Run can only be called once")
	}
	ctx, cancel := context.WithCancel(telemetry.ContextWithTelemetry(ctx, n.opt.Telemetry))
	defer func() {
		close(n.stopped)
		for sub := range n.subscribers {
			close(sub)
		}
		cancel()
		for link := range n.channels {
			link.cancel()
			if link.secure != nil {
				n.tasks.Go(func() { link.secure.Close() })
			}
		}
		n.tasks.Wait()
	}()
	if host == nil {
		return errors.New("world node requires a host")
	}
	n.host, n.rpc = host, rpc
	notifications, err := listRecords[WorldMessage](ctx, n.opt.Records, worldMessages, matchField("direction", string(WorldMessageDirectionOutbound)), matchField("status", "replied", "failed"))
	if err != nil {
		return err
	}
	for _, outboundMessage := range notifications {
		if outboundMessage.NotifyConversation {
			n.wakeConversation(ctx, outboundMessage)
		}
	}
	// 2. 上次发出但未获确认的记录必须核对，已进入运行中的来信不得重复提交。
	sending, err := listRecords[WorldMessage](ctx, n.opt.Records, worldMessages, matchField("direction", string(WorldMessageDirectionOutbound)), matchField("status", "sending"))
	if err != nil {
		return err
	}
	for _, outboundMessage := range sending {
		outboundMessage.Status = WorldMessageStatusUnknown
		if err = n.saveMessage(ctx, &outboundMessage); err != nil {
			return err
		}
	}
	inboundMessages, err := listRecords[WorldMessage](ctx, n.opt.Records, worldMessages, matchField("direction", string(WorldMessageDirectionInbound)))
	if err != nil {
		return err
	}
	slices.SortStableFunc(inboundMessages, func(a, b WorldMessage) int { return cmp.Compare(a.ReceivedSequence, b.ReceivedSequence) })
	for _, inboundMessage := range inboundMessages {
		n.lastReceivedSequence = max(n.lastReceivedSequence, inboundMessage.ReceivedSequence)
		switch inboundMessage.Status {
		case WorldMessageStatusAccepted:
			n.threadQueues[inboundMessage.ThreadID] = append(n.threadQueues[inboundMessage.ThreadID], inboundMessage)
		case WorldMessageStatusRunning:
			inboundMessage.Status = WorldMessageStatusFailed
			inboundMessage.ErrorText = "world node stopped during remote turn"
			if err = n.saveMessage(ctx, &inboundMessage); err != nil {
				return err
			}
		}
	}
	for thread := range n.threadQueues {
		if err = n.startInbound(ctx, n.threadQueues[thread][0]); err != nil {
			return err
		}
	}
	// 3. 控制连接维护在独立任务里，通讯录、通道及收发状态由本循环更新。
	announce := worldserver.NodeAnnounce{
		Entries: worldserver.PeerEntries{DirectURLs: n.opt.DirectURLs, Relays: n.opt.Relays},
		Targets: n.opt.AdvertiseTargets,
	}
	n.tasks.Go(func() { runControlConnection(ctx, n.opt.Relays[0], n.opt.Identity, announce, n.events) })
	relayEvents := make(chan RelayEvent)
	for _, base := range n.opt.Relays[1:] {
		n.tasks.Go(func() { RunRelayAttachments(ctx, base, n.opt.Identity, relayEvents) })
	}
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case now := <-tick.C:
			for id, running := range n.notificationRunning {
				if running {
					continue
				}
				outboundMessage, _, err := getRecord[WorldMessage](ctx, n.opt.Records, worldMessages, id)
				if err != nil {
					return err
				}
				n.wakeConversation(ctx, outboundMessage)
			}
			for link := range n.channels {
				if now.Sub(link.lastUsed) >= 5*time.Minute {
					if err = n.dropChannel(ctx, link, nil); err != nil {
						return err
					}
				}
			}
			if err = n.reconcile(ctx, ""); err != nil {
				return err
			}
		case event := <-relayEvents:
			if err = n.handleNodeEvent(ctx, event); err != nil {
				return err
			}
		case event := <-n.events:
			if err = n.handleNodeEvent(ctx, event); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return err
			}
		}
	}
}

func (n *worldNodeImpl) post(ctx context.Context, event nodeEvent) bool {
	// 1. 事件是否已经移交给节点？
	select {
	case n.events <- event:
		return true
	case <-ctx.Done():
		return false
	case <-n.stopped:
		return false
	}
}

// handleNodeEvent 处理控制帧、通道读写、客户端请求和对话执行结果。
func (n *worldNodeImpl) handleNodeEvent(ctx context.Context, event nodeEvent) error {
	// 1. 已移除的连接产生的迟到事件不再修改消息状态。
	switch e := event.(type) {
	case controlReceived:
		return n.handleControl(ctx, e)
	case directAccepted:
		n.tasks.Go(func() {
			handshake, cancel := context.WithTimeout(ctx, inboundChannelTimeout)
			defer cancel()
			accepted, err := AcceptSecureChannel(handshake, e.transport, n.opt.Identity)
			if err == nil {
				n.handleAcceptedPeer(ctx, handshake, accepted)
			}
		})
	case RelayIncoming:
		n.tasks.Go(func() {
			handshake, cancel := context.WithTimeout(ctx, inboundChannelTimeout)
			defer cancel()
			accepted, err := AcceptRelayChannel(handshake, e.AcceptURL, n.opt.Identity)
			if err == nil {
				n.handleAcceptedPeer(ctx, handshake, accepted)
			}
		})
	case RelayAttachmentChanged:
		n.attachments[e.URL] = e.Connected
		n.publish("status_changed", n.control != nil)
	case channelNegotiated:
		if e.link == nil {
			from := e.firstMessage.From
			e.link = n.newChannel(ctx, from, e.peerKey)
		}
		if _, ok := n.channels[e.link]; !ok {
			if e.secure != nil {
				n.tasks.Go(func() { e.secure.Close() })
			}
			return nil
		}
		if e.err != nil {
			return n.dropChannel(ctx, e.link, e.err)
		}
		e.link.secure = e.secure
		if e.firstMessage != nil {
			return n.receiveMessage(ctx, e.link, *e.firstMessage)
		}
		e.link.peerKey = e.peerKey
		e.link.verified = true
		n.writeNext(e.link)
		n.readNext(e.link)
	case channelRead:
		if _, ok := n.channels[e.link]; !ok {
			return nil
		}
		if e.err != nil {
			return n.dropChannel(ctx, e.link, nil)
		}
		e.link.lastUsed = time.Now()
		return n.receiveMessage(ctx, e.link, e.message)
	case inboundIdentityChecked:
		if _, ok := n.channels[e.link]; !ok {
			return nil
		}
		return n.handleVerifiedMessage(ctx, e.link, e.message, e.threadPeer, e.err)
	case channelWritten:
		if _, ok := n.channels[e.link]; !ok {
			return nil
		}
		if e.err != nil {
			return n.dropChannel(ctx, e.link, nil)
		}
		e.link.writing = false
		n.writeNext(e.link)
	case clientRequested:
		result := n.handleClient(ctx, e)
		e.result <- result
	case notificationFinished:
		if e.err != nil {
			telemetry.GetLogger(ctx).Warn("deliver world reply to conversation", "message_id", e.worldMessage.MessageID, "error", e.err)
			n.notificationRunning[e.worldMessage.MessageID] = false
			return nil
		}
		delete(n.notificationRunning, e.worldMessage.MessageID)
		e.worldMessage.NotifyConversation = false
		return n.saveMessage(ctx, &e.worldMessage)
	case inboundOpened:
		return n.handleInboundOpened(ctx, e)
	case inboundSubmitted:
		n.publish("message_updated", e.worldMessage)
	case inboundFinished:
		return n.finishInbound(ctx, e)
	}
	return nil
}

// handleControl 按完整通讯录更新可达节点和来信授权。
func (n *worldNodeImpl) handleControl(ctx context.Context, e controlReceived) error {
	// 1. 控制面断线保留通讯录，吊销时清除全部授权。
	switch e.envelope.Type {
	case "", worldserver.ControlTypeNodeRevoked:
		revoked := e.envelope.Type == worldserver.ControlTypeNodeRevoked
		if !revoked && n.control != e.socket {
			return nil
		}
		if n.controlDone != nil {
			close(n.controlDone)
		}
		n.controlDone = nil
		n.control = nil
		if revoked {
			n.revoked = true
			n.directory = nil
			n.updateDomains(nil)
			for link := range n.channels {
				if err := n.dropChannel(ctx, link, nil); err != nil {
					return err
				}
			}
		}
		n.publish("status_changed", n.control != nil)
		n.publish("peers_changed", n.getPeers())
	case worldserver.ControlTypeDomainLookupResult:
		if n.control != e.socket {
			return nil
		}
		n.handleDomainLookupResult(e.envelope.Payload)
	case worldserver.ControlTypeDirectory:
		var directory worldserver.Directory
		if json.Unmarshal(e.envelope.Payload, &directory) != nil {
			return nil
		}
		previous := n.directory
		reconnected := n.control != e.socket
		if reconnected {
			if n.controlDone != nil {
				close(n.controlDone)
			}
			n.controlDone = make(chan struct{})
		}
		n.control = e.socket
		n.updateDomains(directory.Domains)
		n.directory = make(map[string]worldserver.DirectoryNode, len(directory.Nodes))
		for _, peer := range directory.Nodes {
			if peer.Domain == n.address.Domain && peer.NodeID != n.opt.Identity.NodeID {
				addr := peer.NodeName + "@" + peer.Domain
				if last := previous[addr]; !peer.Online && slices.Equal(last.SignPublic, peer.SignPublic) {
					peer.Entries = last.Entries
				}
				n.directory[addr] = peer
			}
		}
		for link := range n.channels {
			domain := link.peer.Domain
			peer, ok := n.directory[link.peer.NodeAddress()]
			trusted := ok && slices.Equal(peer.SignPublic, link.peerKey)
			if domain != n.address.Domain {
				card, ok := n.domains[domain]
				trusted = ok && slices.Equal(card.PublicKey, link.domainKey)
			}
			if !trusted {
				if err := n.dropChannel(ctx, link, nil); err != nil {
					return err
				}
			}
		}
		n.publish("peers_changed", n.getPeers())
		if reconnected {
			n.publish("status_changed", true)
			return n.reconcile(ctx, "")
		}
		for addr, peer := range n.directory {
			if peer.Online && len(peer.Entries.Relays) > 0 && (!previous[addr].Online || len(previous[addr].Entries.Relays) == 0) {
				if err := n.reconcile(ctx, addr); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// RegisterDirectRoutes 按握手提示选择同一监听器上的域或节点身份。
func RegisterDirectRoutes(router gin.IRouter, node WorldNode, domain worldserver.WorldServer) {
	if node == nil && domain == nil {
		return
	}
	router.GET("/world/v1/direct", func(c *gin.Context) {
		purpose := ChannelPurpose(c.Query("purpose"))
		if purpose == ChannelPurposeDomain && domain != nil {
			domain.ServeDomainDirect(c)
			return
		}
		if node == nil || (purpose != ChannelPurposeWorld && purpose != ChannelPurposeRPC) {
			c.Status(http.StatusNotFound)
			return
		}
		node.ServeNodeDirect(c)
	})
}

// ServeNodeDirect 将节点的直连交给握手任务。
func (n *worldNodeImpl) ServeNodeDirect(c *gin.Context) {
	if c.Query("target") != base64.RawURLEncoding.EncodeToString(n.opt.Identity.SignPublic) {
		c.Status(http.StatusNotFound)
		return
	}
	socket, err := websocket.Accept(c.Writer, c.Request, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	socket.SetReadLimit(worldserver.ChannelFrameLimit)
	transport := &directFrameTransport{socket}
	if !n.post(c.Request.Context(), directAccepted{transport}) {
		transport.Close()
	}
}

func (n *worldNodeImpl) newChannel(ctx context.Context, addr Address, key []byte) *peerChannel {
	// 1. 独立管理此次握手和读写的生命周期。
	linkCtx, cancel := context.WithCancel(ctx)
	link := &peerChannel{
		ctx:         linkCtx,
		cancel:      cancel,
		peer:        Address{Node: addr.Node, Domain: addr.Domain},
		peerKey:     key,
		domainKey:   n.domains[addr.Domain].PublicKey,
		lastUsed:    time.Now(),
		awaitingAck: make(map[string]struct{}),
		queries:     make(map[string]struct{}),
	}
	n.channels[link] = struct{}{}
	return link
}

func (n *worldNodeImpl) dialPeer(ctx context.Context, to Address) *peerChannel {
	peer := n.directory[to.NodeAddress()]
	domain := n.domains[to.Domain]
	control, done := n.control, n.controlDone
	link := n.newChannel(ctx, to, peer.SignPublic)
	n.tasks.Go(func() {
		key, entries := peer.SignPublic, peer.Entries
		var channel SecureChannel
		var err error
		if to.Domain != n.address.Domain {
			var member worldserver.DomainMember
			member, err = n.lookupMember(link.ctx, to, domain, control, done, true)
			key, entries = member.PublicKey, member.Entries
		}
		if err == nil {
			channel, err = n.peerDialer.DialPeer(link.ctx, to.NodeAddress(), key, entries, &n.opt.Identity, ChannelPurposeWorld)
		}
		if !n.post(link.ctx, channelNegotiated{link: link, secure: channel, peerKey: key, err: err}) && channel != nil {
			channel.Close()
		}
	})
	return link
}

func (n *worldNodeImpl) getChannel(peer string) *peerChannel {
	// 1. 优先复用已认证通道。
	var candidate *peerChannel
	for link := range n.channels {
		if link.peer.NodeAddress() != peer || (link.secure != nil && !link.verified) {
			continue
		}
		if link.secure != nil {
			if len(link.domainKey) > 0 {
				member, ok := n.getCachedMember(memberCacheKey{link.peer.Domain, string(link.domainKey), link.peer.Node}, time.Now())
				if !ok || !slices.Equal(link.peerKey, member.PublicKey) {
					continue
				}
			}
			return link
		}
		candidate = link
	}
	return candidate
}

func (n *worldNodeImpl) isReachable(addr string) bool {
	// 对端是否有已建立的通道、独立入口或在线的本域挂靠？
	_, domain, _ := strings.Cut(addr, "@")
	if domain != n.address.Domain {
		return n.domains != nil
	}
	peer, ok := n.directory[addr]
	if !ok {
		return false
	}
	link := n.getChannel(addr)
	if link != nil && link.secure != nil {
		return true
	}
	if len(peer.Entries.DirectURLs) > 0 || len(peer.Entries.Relays) > 1 {
		return true
	}
	return peer.Online && len(peer.Entries.Relays) > 0
}

// queueMessage 按顺序把信封交给对端通道。
func (n *worldNodeImpl) queueMessage(ctx context.Context, msg wireMessage, link *peerChannel) {
	// 1. 回复优先使用来信通道，主动发送复用已有通道。
	if link == nil {
		addr := msg.To.NodeAddress()
		link = n.getChannel(addr)
		if link == nil {
			link = n.dialPeer(ctx, msg.To)
		}
	}
	link.outbound = append(link.outbound, msg)
	n.writeNext(link)
}

func (n *worldNodeImpl) writeNext(link *peerChannel) {
	// 1. 每次只在同一个 SecureChannel 上启动一次写操作。
	if link.secure == nil || link.writing || len(link.outbound) == 0 {
		return
	}
	msg := link.outbound[0]
	link.outbound = link.outbound[1:]
	link.writing = true
	link.lastUsed = time.Now()
	if msg.Kind == "message" || msg.Kind == "reply" {
		link.awaitingAck[msg.MessageID] = struct{}{}
	}
	if msg.Kind == "status_query" {
		link.queries[msg.InReplyTo] = struct{}{}
	}
	secure := link.secure
	n.tasks.Go(func() {
		ctx, cancel := context.WithTimeout(link.ctx, 5*time.Second)
		defer cancel()
		payload, err := json.Marshal(msg)
		if err == nil {
			err = secure.WriteMessage(ctx, payload)
		}
		n.post(link.ctx, channelWritten{link, err})
	})
}

// dropChannel 区分仍在本地队列中的信与已交给网络的信。
func (n *worldNodeImpl) dropChannel(ctx context.Context, link *peerChannel, failure error) error {
	// 1. 撤销路由并唤醒读写任务。
	delete(n.channels, link)
	link.cancel()
	if link.secure != nil {
		n.tasks.Go(func() { link.secure.Close() })
	}
	// 2. 发出但未确认的信进入核对，尚未发送的信留在离线队列。
	statuses := make(map[string]WorldMessageStatus, len(link.awaitingAck))
	for _, msg := range link.outbound {
		if msg.Kind == "message" || msg.Kind == "reply" {
			statuses[msg.MessageID] = WorldMessageStatusPendingOffline
		} else if msg.Kind == "status_query" && failure != nil {
			statuses[msg.InReplyTo] = WorldMessageStatusUnknown
		}
	}
	for id := range link.awaitingAck {
		statuses[id] = WorldMessageStatusUnknown
	}
	for id, status := range statuses {
		outboundMessage, found, err := getRecord[WorldMessage](ctx, n.opt.Records, worldMessages, id)
		if err != nil {
			return err
		}
		if found && outboundMessage.Direction == WorldMessageDirectionOutbound && !isTerminalOutboundStatus(outboundMessage.Status) {
			if failure != nil {
				if err := n.recordDeliveryFailure(ctx, &outboundMessage, ErrorCode(failure), failure.Error(), status); err != nil {
					return err
				}
				continue
			}
			if outboundMessage.Status != WorldMessageStatusSending {
				continue
			}
			outboundMessage.Status = status
			if status == WorldMessageStatusPendingOffline && n.waiters[id].notify {
				outboundMessage.NotifyConversation = outboundMessage.ConversationID != ""
			}
			if err = n.saveMessage(ctx, &outboundMessage); err != nil {
				return err
			}
		}
	}
	return nil
}

// reconcile 对可达节点补投未发消息，核对已发消息。
func (n *worldNodeImpl) reconcile(ctx context.Context, peer string) error {
	// 1. 核对原标识，只有明确未找到时才补发。
	outboundMessages, err := listRecords[WorldMessage](ctx, n.opt.Records, worldMessages, matchField("direction", string(WorldMessageDirectionOutbound)), matchField("status", "pending_offline", "unknown", "remote_running", "sending"))
	if err != nil {
		return err
	}
	for _, outboundMessage := range outboundMessages {
		if outboundMessage.Kind == "reply" {
			continue
		}
		to, err := ParseAddress(outboundMessage.PeerAddr, n.opt.Domain)
		if err != nil {
			return err
		}
		if (peer != "" && peer != to.NodeAddress()) || !n.isReachable(to.NodeAddress()) {
			continue
		}
		if outboundMessage.Status == WorldMessageStatusPendingOffline {
			if err = n.deliver(ctx, &outboundMessage, to); err != nil {
				return err
			}
		} else {
			if outboundMessage.Status == WorldMessageStatusSending && time.Since(time.UnixMilli(outboundMessage.UpdatedAt)) < 15*time.Second {
				continue
			}
			n.queueMessage(ctx, wireMessage{
				Kind:      "status_query",
				MessageID: string(defs.NewItemID()),
				InReplyTo: outboundMessage.MessageID,
				ThreadID:  outboundMessage.ThreadID,
				From:      n.address,
				To:        to,
				HopLimit:  outboundMessage.HopLimit,
			}, nil)
		}
	}
	return nil
}

// recordThreadActivity 在收到或发出新消息时记录会话及其最近活动时间。
func (n *worldNodeImpl) recordThreadActivity(ctx context.Context, msg *WorldMessage) error {
	thread, found, err := getRecord[ThreadBinding](ctx, n.opt.Records, worldThreads, msg.ThreadID)
	if err != nil {
		return err
	}
	if !found {
		thread = ThreadBinding{
			ThreadID: msg.ThreadID, PeerAddr: msg.PeerAddr,
			Direction: string(msg.Direction), CreatedAt: msg.CreatedAt,
		}
		if msg.Direction == WorldMessageDirectionInbound {
			thread.Target = cmp.Or(msg.Target, n.opt.DefaultTarget)
		}
	}
	bound, err := ParseAddress(thread.PeerAddr, n.address.Domain)
	if err != nil {
		return err
	}
	thread.PeerDomain = bound.Domain
	return putRecord(ctx, n.opt.Records, worldThreads, thread.ThreadID, msg.CreatedAt, thread)
}

func (n *worldNodeImpl) saveThreadMessage(ctx context.Context, msg *WorldMessage) error {
	if err := n.recordThreadActivity(ctx, msg); err != nil {
		return err
	}
	return n.saveMessage(ctx, msg)
}

func (n *worldNodeImpl) saveMessage(ctx context.Context, worldMessage *WorldMessage) error {
	// 1. 先持久化状态，再通知客户端和等待工具。
	worldMessage.UpdatedAt = time.Now().UnixMilli()
	if err := putRecord(ctx, n.opt.Records, worldMessages, worldMessage.MessageID, worldMessage.UpdatedAt, worldMessage); err != nil {
		return err
	}
	n.publish("message_updated", worldMessage)
	if worldMessage.Direction == WorldMessageDirectionOutbound &&
		(isTerminalOutboundStatus(worldMessage.Status) || worldMessage.Status == WorldMessageStatusPendingOffline) {
		if waiter, ok := n.waiters[worldMessage.MessageID]; ok {
			waiter.finished <- *worldMessage
			delete(n.waiters, worldMessage.MessageID)
		}
	}
	return nil
}

func (n *worldNodeImpl) deliver(ctx context.Context, outboundMessage *WorldMessage, to Address) error {
	// 1. 确定可达性，写入发件箱后才允许发出。
	outboundMessage.Status = WorldMessageStatusPendingOffline
	if n.isReachable(to.NodeAddress()) {
		outboundMessage.Status = WorldMessageStatusSending
	}
	if err := n.saveMessage(ctx, outboundMessage); err != nil {
		return err
	}
	if outboundMessage.Status == WorldMessageStatusSending {
		n.queueMessage(ctx, outboundEnvelope(n.address, to, *outboundMessage), nil)
	}
	return nil
}

// readNext 等待下一条密文；前一条完成身份核对后才再次读取。
func (n *worldNodeImpl) readNext(link *peerChannel) {
	n.tasks.Go(func() {
		msg, err := readWorldMessage(link.ctx, link.secure)
		n.post(link.ctx, channelRead{link, msg, err})
	})
}

// receiveMessage 检查信封地址并按本域或外域身份核实来信。
func (n *worldNodeImpl) receiveMessage(ctx context.Context, link *peerChannel, msg wireMessage) error {
	if msg.MessageID == "" || msg.ThreadID == "" || msg.To.NodeAddress() != n.address.NodeAddress() ||
		msg.From.NodeAddress() != link.peer.NodeAddress() || !nodeOrTargetPattern.MatchString(msg.From.Node) || !worldserver.IsValidDomain(msg.From.Domain) ||
		(msg.From.Target != "" && !nodeOrTargetPattern.MatchString(msg.From.Target)) {
		return n.dropChannel(ctx, link, nil)
	}
	if len(msg.Body) > maxWorldMessageBodyBytes {
		return n.dropChannel(ctx, link, nil)
	}
	if msg.From.Domain == n.address.Domain {
		peer := n.directory[link.peer.NodeAddress()]
		var err error
		if !slices.Equal(peer.SignPublic, link.peerKey) || (msg.Kind == "message" && !peer.CanReceive) {
			err = senderNotAllowed(link.peer.NodeAddress())
		}
		return n.handleVerifiedMessage(ctx, link, msg, "", err)
	}
	domain, ok := n.domains[msg.From.Domain]
	if !ok {
		return n.handleVerifiedMessage(ctx, link, msg, "", domainNotTrusted(msg.From.Domain))
	}
	if link.verified && (msg.Kind == "ack" || msg.Kind == "reply") {
		return n.handleVerifiedMessage(ctx, link, msg, "", nil)
	}
	control, done := n.control, n.controlDone
	n.tasks.Go(func() {
		member, err := n.lookupMember(link.ctx, msg.From, domain, control, done, false)
		var threadPeer string
		if err != nil || !slices.Equal(member.PublicKey, link.peerKey) {
			err = identityUnverified(link.peer.NodeAddress())
		} else if msg.Kind == "message" {
			threadPeer, err = n.getThreadPeerNode(link.ctx, msg.ThreadID)
		}
		n.post(link.ctx, inboundIdentityChecked{link, msg, threadPeer, err})
	})
	return nil
}

// handleVerifiedMessage 处理身份核对结果，仅向已建立线程的外域对端开放非公开目标。
func (n *worldNodeImpl) handleVerifiedMessage(ctx context.Context, link *peerChannel, msg wireMessage, threadPeer string, failure error) error {
	if failure == nil {
		link.verified = true
		if msg.Kind == "message" && msg.From.Domain != n.address.Domain && threadPeer != msg.From.NodeAddress() {
			target := cmp.Or(msg.To.Target, n.opt.DefaultTarget)
			if !slices.ContainsFunc(n.opt.AdvertiseTargets, func(t worldserver.AdvertisedTarget) bool { return t.Name == target && t.Visibility == "public" }) {
				failure = &worldError{"world_target_not_public", fmt.Sprintf("target %q does not accept messages from other domains", target)}
			}
		}
	}
	if failure != nil {
		if msg.Kind == "message" || msg.Kind == "status_query" {
			id := msg.MessageID
			if msg.Kind == "status_query" {
				id = msg.InReplyTo
			}
			n.queueMessage(ctx, wireMessage{
				Kind: "ack", MessageID: string(defs.NewItemID()), InReplyTo: id, ThreadID: msg.ThreadID,
				From: msg.To, To: msg.From, HopLimit: 1,
				Status: "failed", ErrorCode: ErrorCode(failure), Body: failure.Error(),
			}, link)
		}
	} else if err := n.handleMessage(ctx, link, msg); err != nil {
		return err
	}
	n.readNext(link)
	return nil
}

// handleMessage 只按已认证来源处理发往本节点的信封。
func (n *worldNodeImpl) handleMessage(ctx context.Context, link *peerChannel, msg wireMessage) error {
	ack := wireMessage{
		Kind:      "ack",
		MessageID: string(defs.NewItemID()),
		ThreadID:  msg.ThreadID,
		From:      msg.To,
		To:        msg.From,
		InReplyTo: msg.MessageID,
		HopLimit:  1,
	}
	switch msg.Kind {
	case "message", "status_query":
		// 2. 查询仅回放收到的记录，来信与既有记录冲突时保留原行。
		key := msg.MessageID
		if msg.Kind == "status_query" {
			key = msg.InReplyTo
			ack.InReplyTo = key
		}
		if key == "" {
			return nil
		}
		inboundMessage, found, err := getRecord[WorldMessage](ctx, n.opt.Records, worldMessages, key)
		if err != nil {
			return err
		}
		if msg.Kind == "status_query" && (!found || inboundMessage.Direction != WorldMessageDirectionInbound) {
			ack.Status = "not_found"
			n.queueMessage(ctx, ack, link)
			return nil
		}
		if found {
			if inboundMessage.Direction != WorldMessageDirectionInbound ||
				(msg.Kind == "message" && inboundMessage.Kind != "message") ||
				inboundMessage.PeerAddr != msg.From.String() || inboundMessage.ThreadID != msg.ThreadID {
				return nil
			}
			ack.Status = string(inboundMessage.Status)
			ack.Body = inboundMessage.ReplyBody
			if inboundMessage.Status == WorldMessageStatusFailed {
				ack.Body = inboundMessage.ErrorText
				ack.ErrorCode = inboundMessage.ErrorCode
			}
			n.queueMessage(ctx, ack, link)
			if msg.Kind == "status_query" && inboundMessage.Kind == "message" {
				if inboundMessage.Status == WorldMessageStatusAccepted || inboundMessage.Status == WorldMessageStatusRunning {
					n.replyChannels[key] = link
				}
				if inboundMessage.Status == WorldMessageStatusCompleted {
					replies, err := listRecords[WorldMessage](ctx, n.opt.Records, worldMessages, matchField("in_reply_to", key), matchField("direction", "outbound"), matchField("kind", "reply"))
					if err != nil {
						return err
					}
					for _, reply := range replies {
						if isTerminalOutboundStatus(reply.Status) {
							continue
						}
						reply.Status = WorldMessageStatusSending
						if err := n.saveMessage(ctx, &reply); err != nil {
							return err
						}
						n.queueMessage(ctx, outboundEnvelope(n.address, msg.From, reply), link)
					}
				}
			}
			return nil
		}
		if msg.HopLimit <= 0 {
			ack.ErrorCode, ack.Body = "world_hop_limit_exceeded", "world_hop_limit_exceeded"
		} else if msg.From.Domain == n.address.Domain && !isTargetAdvertised(n.opt.AdvertiseTargets, cmp.Or(msg.To.Target, n.opt.DefaultTarget)) {
			ack.ErrorCode, ack.Body = "world_target_not_exposed", "world_target_not_exposed"
		}
		if ack.Body != "" {
			ack.Status = "failed"
			n.queueMessage(ctx, ack, link)
			return nil
		}
		n.lastReceivedSequence++
		inboundMessage = WorldMessage{
			MessageID:        key,
			ThreadID:         msg.ThreadID,
			Direction:        WorldMessageDirectionInbound,
			PeerAddr:         msg.From.String(),
			Target:           msg.To.Target,
			Kind:             msg.Kind,
			Subject:          msg.Subject,
			Body:             msg.Body,
			InReplyTo:        msg.InReplyTo,
			HopLimit:         msg.HopLimit,
			Status:           WorldMessageStatusAccepted,
			ReceivedSequence: n.lastReceivedSequence,
			CreatedAt:        time.Now().UnixMilli(),
		}
		if err = n.saveThreadMessage(ctx, &inboundMessage); err != nil {
			telemetry.GetLogger(ctx).Warn("save world inbound message", "message_id", inboundMessage.MessageID, "error", err)
			ack.Status = "failed"
			ack.Body = "failed to persist inbound message"
			n.queueMessage(ctx, ack, link)
			return nil
		}
		n.replyChannels[key] = link
		ack.Status = "accepted"
		n.queueMessage(ctx, ack, link)
		n.threadQueues[msg.ThreadID] = append(n.threadQueues[msg.ThreadID], inboundMessage)
		if len(n.threadQueues[msg.ThreadID]) == 1 {
			return n.startInbound(ctx, inboundMessage)
		}
	case "ack", "reply":
		if msg.InReplyTo == "" {
			return nil
		}
		outboundMessage, found, err := getRecord[WorldMessage](ctx, n.opt.Records, worldMessages, msg.InReplyTo)
		if err != nil {
			return err
		}
		if !found || outboundMessage.Direction != WorldMessageDirectionOutbound || outboundMessage.ThreadID != msg.ThreadID {
			return nil
		}
		to, err := ParseAddress(outboundMessage.PeerAddr, n.opt.Domain)
		if err != nil {
			return err
		}
		if to.NodeAddress() != link.peer.NodeAddress() {
			return nil
		}
		if msg.Kind == "ack" {
			switch msg.Status {
			case "not_found":
				if _, ok := link.queries[outboundMessage.MessageID]; !ok {
					return nil
				}
			case "accepted", "running", "completed", "failed":
			default:
				return nil
			}
		}
		// 3. 回信和完成确认走同一个终态入口，重复投递仍可获得完成确认。
		if msg.Kind == "reply" {
			if outboundMessage.Kind != "message" {
				return nil
			}
			old, exists, err := getRecord[WorldMessage](ctx, n.opt.Records, worldMessages, msg.MessageID)
			if err != nil {
				return err
			}
			if exists && (old.Direction != WorldMessageDirectionInbound || old.Kind != "reply" ||
				old.InReplyTo != msg.InReplyTo || old.PeerAddr != msg.From.String() || old.ThreadID != msg.ThreadID) {
				return nil
			}
			if err = n.completeOutbound(ctx, &outboundMessage, msg.Body, ""); err != nil {
				return err
			}
			if !exists {
				receipt := WorldMessage{
					MessageID: msg.MessageID,
					ThreadID:  msg.ThreadID,
					Direction: WorldMessageDirectionInbound,
					PeerAddr:  msg.From.String(),
					Target:    msg.To.Target,
					Kind:      msg.Kind,
					Subject:   msg.Subject,
					Body:      msg.Body,
					InReplyTo: msg.InReplyTo,
					HopLimit:  msg.HopLimit,
					Status:    WorldMessageStatusCompleted,
					CreatedAt: time.Now().UnixMilli(),
				}
				if err = n.saveThreadMessage(ctx, &receipt); err != nil {
					return err
				}
			}
			ack.Status = "completed"
			n.queueMessage(ctx, ack, link)
		}
		_, queried := link.queries[outboundMessage.MessageID]
		for channel := range n.channels {
			delete(channel.awaitingAck, outboundMessage.MessageID)
			delete(channel.queries, outboundMessage.MessageID)
		}
		if msg.Kind == "reply" {
			return nil
		}
		if isTerminalOutboundStatus(outboundMessage.Status) || outboundMessage.Status == WorldMessageStatusPendingOffline {
			return nil
		}
		switch msg.Status {
		case "accepted", "running":
			if outboundMessage.Status != WorldMessageStatusRemoteRunning {
				outboundMessage.ErrorCode, outboundMessage.ErrorText = "", ""
				outboundMessage.Status = WorldMessageStatusRemoteRunning
				return n.saveMessage(ctx, &outboundMessage)
			}
		case "completed":
			return n.completeOutbound(ctx, &outboundMessage, msg.Body, "")
		case "failed":
			retry := WorldMessageStatusPendingOffline
			if queried {
				retry = WorldMessageStatusUnknown
			}
			return n.recordDeliveryFailure(ctx, &outboundMessage, msg.ErrorCode, cmp.Or(msg.Body, "world delivery failed"), retry)
		case "not_found":
			return n.deliver(ctx, &outboundMessage, to)
		}
	}
	return nil
}

func (n *worldNodeImpl) completeOutbound(ctx context.Context, outboundMessage *WorldMessage, reply, failure string) error {
	// 1. 单次保存终态，保留通知责任直到来源对话接收回信。
	if isTerminalOutboundStatus(outboundMessage.Status) {
		return nil
	}
	notify := outboundMessage.NotifyConversation && outboundMessage.ConversationID != "" && outboundMessage.Kind == "message"
	outboundMessage.ErrorText = ""
	if failure == "" {
		outboundMessage.ErrorCode = ""
	}
	outboundMessage.Status = WorldMessageStatusReplied
	outboundMessage.ReplyBody = reply
	if failure != "" {
		outboundMessage.Status = WorldMessageStatusFailed
		outboundMessage.ErrorText = failure
	}
	if err := n.saveMessage(ctx, outboundMessage); err != nil {
		return err
	}
	if notify {
		n.wakeConversation(ctx, *outboundMessage)
	}
	return nil
}

// startInbound 为线程队首的来信打开或创建本地对话。
func (n *worldNodeImpl) startInbound(ctx context.Context, inboundMessage WorldMessage) error {
	// 1. 线程归属同时绑定对端和本地目标。
	target := cmp.Or(inboundMessage.Target, n.opt.DefaultTarget)
	threadBinding, _, err := getRecord[ThreadBinding](ctx, n.opt.Records, worldThreads, inboundMessage.ThreadID)
	if err != nil {
		return err
	}
	peerNode, _, _ := strings.Cut(threadBinding.PeerAddr, "/")
	incomingNode, _, _ := strings.Cut(inboundMessage.PeerAddr, "/")
	if peerNode != incomingNode || threadBinding.Direction == "inbound" && (threadBinding.Target != target || threadBinding.PeerAddr != inboundMessage.PeerAddr) {
		return n.finishInbound(ctx, inboundFinished{worldMessage: inboundMessage, err: errors.New("world thread belongs to another peer or target")})
	}
	threadBinding.PeerAddr = inboundMessage.PeerAddr
	threadBinding.Direction = "inbound"
	threadBinding.Target = target
	threadBinding.RemainingHops = inboundMessage.HopLimit
	// 2. 打开对话后交回循环登记映射，此时才可提交第一条输入。
	n.tasks.Go(func() {
		summary := []rune(strings.Join(strings.Fields(cmp.Or(inboundMessage.Subject, inboundMessage.Body)), " "))
		opened, err := n.host.OpenSession(ctx, OpenSessionRequest{
			ThreadID:          inboundMessage.ThreadID,
			Target:            target,
			PeerAddr:          inboundMessage.PeerAddr,
			Title:             string(summary[:min(48, len(summary))]),
			ExistingSessionID: threadBinding.ConversationID,
		})
		if err == nil && opened.SessionID == "" {
			err = errors.New("host returned an empty session ID")
		}
		n.post(ctx, inboundOpened{worldMessage: inboundMessage, openedSession: opened, threadBinding: threadBinding, err: err})
	})
	return nil
}

func (n *worldNodeImpl) handleInboundOpened(ctx context.Context, e inboundOpened) error {
	// 1. 保存本条消息的转发预算，使本轮工具可以继承。
	if e.err != nil {
		return n.finishInbound(ctx, inboundFinished{worldMessage: e.worldMessage, err: e.err})
	}
	inboundMessage := e.worldMessage
	inboundMessage.ConversationID = e.openedSession.SessionID
	threadBinding := e.threadBinding
	threadBinding.ConversationID = inboundMessage.ConversationID
	if err := putRecord(ctx, n.opt.Records, worldThreads, threadBinding.ThreadID, inboundMessage.CreatedAt, threadBinding); err != nil {
		return err
	}
	inboundMessage.Status = WorldMessageStatusRunning
	if err := n.saveMessage(ctx, &inboundMessage); err != nil {
		return err
	}
	// 2. 在任务里等且只等读入本消息的 Turn。
	n.tasks.Go(func() {
		req := InboundTurnRequest{OpenedSession: e.openedSession, Message: inboundMessage, Target: threadBinding.Target}
		reply, err := n.host.RunInboundTurn(ctx, req, func() {
			n.post(ctx, inboundSubmitted{worldMessage: inboundMessage})
		})
		n.post(ctx, inboundFinished{inboundMessage, reply, err})
	})
	return nil
}

func (n *worldNodeImpl) finishInbound(ctx context.Context, e inboundFinished) error {
	// 1. 保存终态，之后的重复来信或查询都可以直接恢复同一份结果。
	inboundMessage := e.worldMessage
	inboundMessage.Status = WorldMessageStatusCompleted
	inboundMessage.ReplyBody = e.reply
	if e.err != nil {
		inboundMessage.Status = WorldMessageStatusFailed
		inboundMessage.ErrorText = e.err.Error()
		if errors.Is(e.err, conversation.ErrNoModelConfigured) {
			inboundMessage.ErrorCode = "world_agent_no_model"
		}
	} else if len(e.reply) > maxWorldMessageBodyBytes {
		inboundMessage.Status = WorldMessageStatusFailed
		inboundMessage.ReplyBody = ""
		inboundMessage.ErrorText = fmt.Sprintf("world reply exceeds %d bytes", maxWorldMessageBodyBytes)
	}
	if err := n.saveMessage(ctx, &inboundMessage); err != nil {
		telemetry.GetLogger(ctx).Warn("save world inbound completion", "message_id", inboundMessage.MessageID, "error", err)
		return nil
	}
	// 2. 正常回信也经过持久发件箱，失败立即回复确认。
	to, err := ParseAddress(inboundMessage.PeerAddr, n.opt.Domain)
	if err != nil {
		return err
	}
	link := n.replyChannels[inboundMessage.MessageID]
	delete(n.replyChannels, inboundMessage.MessageID)
	if _, open := n.channels[link]; !open {
		link = nil
	}
	if inboundMessage.Status == WorldMessageStatusCompleted {
		reply := WorldMessage{
			Direction: WorldMessageDirectionOutbound,
			MessageID: string(defs.NewItemID()),
			ThreadID:  inboundMessage.ThreadID,
			PeerAddr:  inboundMessage.PeerAddr,
			Kind:      "reply",
			Body:      e.reply,
			InReplyTo: inboundMessage.MessageID,
			HopLimit:  1,
			CreatedAt: time.Now().UnixMilli(),
		}
		reply.Status = WorldMessageStatusPendingOffline
		if link != nil {
			reply.Status = WorldMessageStatusSending
		}
		if err := n.saveThreadMessage(ctx, &reply); err != nil {
			return err
		}
		if link != nil {
			n.queueMessage(ctx, outboundEnvelope(n.address, to, reply), link)
		}
	} else if link != nil {
		n.queueMessage(ctx, wireMessage{Kind: "ack", MessageID: string(defs.NewItemID()), ThreadID: inboundMessage.ThreadID, InReplyTo: inboundMessage.MessageID, From: n.address, To: to, Status: "failed", ErrorCode: inboundMessage.ErrorCode, Body: inboundMessage.ErrorText, HopLimit: 1}, link)
	}
	// 3. 前一条来信的回复已保存，此时才开始线程内的下一条。
	rest := n.threadQueues[inboundMessage.ThreadID][1:]
	if len(rest) == 0 {
		delete(n.threadQueues, inboundMessage.ThreadID)
		return nil
	}
	n.threadQueues[inboundMessage.ThreadID] = rest
	return n.startInbound(ctx, rest[0])
}

const maxWorldMessageBodyBytes = 512 << 10

func (n *worldNodeImpl) getPeers() []WorldPeer {
	// 1. 以允许主动发信的节点地址为联系人，保留可选目标的说明。
	peers := make([]WorldPeer, 0, len(n.directory))
	for addr, node := range n.directory {
		if !node.CanSend {
			continue
		}
		link := n.getChannel(addr)
		peer := WorldPeer{
			Address:  addr,
			NodeName: node.NodeName,
			Targets:  node.Targets,
			Online:   link != nil && link.secure != nil || n.control != nil && node.Online,
		}
		descriptions := make([]string, 0, len(node.Targets))
		for _, target := range node.Targets {
			if target.Description != "" {
				descriptions = append(descriptions, target.Description)
			}
		}
		peer.Description = strings.Join(descriptions, ", ")
		peers = append(peers, peer)
	}
	slices.SortFunc(peers, func(a, b WorldPeer) int { return strings.Compare(a.Address, b.Address) })
	return peers
}

// handleClient 将发信、取消和等待移交与远端确认串行化。
func (n *worldNodeImpl) handleClient(ctx context.Context, req clientRequested) clientResult {
	// 1. 查询本节点状态或登记事件订阅。
	switch req.operation {
	case "peers":
		return clientResult{peers: n.getPeers()}
	case "status":
		pending, err := listRecords[WorldMessage](ctx, n.opt.Records, worldMessages, matchField("direction", string(WorldMessageDirectionOutbound)), matchField("status", "pending_offline"))
		status := NodeStatus{
			Enabled: true, Revoked: n.revoked, NodeAddr: n.address.String(), ServerURL: n.opt.ServerURL,
			Connected: n.control != nil, RelayAttachments: n.getRelayStatus(), PendingOutboxCount: len(pending),
		}
		if n.rpc != nil {
			target := RemoteAccessTarget{Token: req.token, Name: n.opt.RemoteAccessName, Node: n.address.Node, Domain: n.address.Domain, PublicKey: n.opt.Identity.SignPublic, Entries: worldserver.PeerEntries{DirectURLs: n.opt.DirectURLs, Relays: n.opt.Relays}}
			link, err := FormatRemoteAccessURL(n.opt.WebURL, target)
			if err != nil {
				return clientResult{err: err}
			}
			status.RemoteAccessURL = link
			status.RemoteAccessLocalOnly = IsLocalPageURL(link)
			status.RemoteAccessRelays = status.RelayAttachments
			if !n.homeIsPublicEntry {
				status.RemoteAccessRelays = status.RemoteAccessRelays[1:]
			}
		}
		return clientResult{status: status, err: err}
	case "subscribe":
		n.subscribers[req.subscription] = struct{}{}
		return clientResult{}
	case "unsubscribe":
		if _, subscribed := n.subscribers[req.subscription]; subscribed {
			delete(n.subscribers, req.subscription)
			close(req.subscription)
		}
		return clientResult{}
	case "send":
		if n.revoked {
			return clientResult{err: &worldError{"world_node_revoked", "node was removed from its domain"}}
		}
		// 发信在下方完成地址校验和持久化。
	case "cancel", "detach":
		outboundMessage, found, err := getRecord[WorldMessage](ctx, n.opt.Records, worldMessages, req.messageID)
		if err != nil {
			return clientResult{err: err}
		}
		if !found || outboundMessage.Direction != WorldMessageDirectionOutbound {
			return clientResult{err: &worldError{"world_message_not_found", "world message not found"}}
		}
		if req.operation == "cancel" {
			if outboundMessage.Status != WorldMessageStatusPendingOffline {
				return clientResult{err: &worldError{"world_outbox_not_cancellable", fmt.Sprintf("message %q can only be cancelled while pending_offline", outboundMessage.MessageID)}}
			}
			outboundMessage.Status = WorldMessageStatusCancelled
		} else {
			delete(n.waiters, outboundMessage.MessageID)
			if isTerminalOutboundStatus(outboundMessage.Status) {
				return clientResult{worldMessage: outboundMessage}
			}
			outboundMessage.NotifyConversation = req.notify && outboundMessage.ConversationID != ""
		}
		err = n.saveMessage(ctx, &outboundMessage)
		return clientResult{
			worldMessage: outboundMessage,
			err:          err,
		}
	default:
		panic("unknown world client operation")
	}
	// 2. 校验联系人与转发预算，离线节点仍可接收入队的信。
	send := req.send
	to, err := ParseAddress(send.To, n.address.Domain)
	if err != nil {
		return clientResult{err: err}
	}
	peer, ok := n.directory[to.NodeAddress()]
	if ok && !peer.CanSend {
		return clientResult{err: senderNotAllowed(n.address.String())}
	}
	if to.Domain != n.address.Domain {
		if _, trusted := n.domains[to.Domain]; !trusted {
			return clientResult{err: domainNotTrusted(to.Domain)}
		}
	} else if !ok {
		return clientResult{err: &worldError{"world_peer_not_found", fmt.Sprintf("world peer %q is not in directory", to.NodeAddress())}}
	}
	if to.Target != "" && peer.Online && len(peer.Targets) > 0 && !isTargetAdvertised(peer.Targets, to.Target) {
		return clientResult{err: &worldError{"world_target_not_exposed", fmt.Sprintf("target %q is not exposed on node %q", to.Target, to.Node)}}
	}
	if strings.TrimSpace(send.Message) == "" {
		return clientResult{err: &worldError{"invalid_arguments", "world message is empty"}}
	}
	if len(send.Message) > maxWorldMessageBodyBytes {
		return clientResult{err: &worldError{"invalid_arguments", fmt.Sprintf("world message exceeds %d bytes", maxWorldMessageBodyBytes)}}
	}
	hops := 5
	if send.SourceConversationID != "" {
		threadBindings, err := listRecords[ThreadBinding](ctx, n.opt.Records, worldThreads, matchField("conversation_id", send.SourceConversationID))
		if err != nil {
			return clientResult{err: err}
		}
		for _, threadBinding := range threadBindings {
			if threadBinding.Direction == "inbound" {
				hops = threadBinding.RemainingHops - 1
				break
			}
		}
	}
	if hops <= 0 {
		return clientResult{err: &worldError{"world_hop_limit_exceeded", "world forward hop limit exceeded"}}
	}
	if send.ThreadID == "" {
		send.ThreadID = string(defs.NewItemID())
	}
	outboundMessage := WorldMessage{
		Direction:          WorldMessageDirectionOutbound,
		MessageID:          string(defs.NewItemID()),
		ThreadID:           send.ThreadID,
		ConversationID:     send.SourceConversationID,
		PeerAddr:           to.String(),
		Kind:               "message",
		Subject:            send.Subject,
		Body:               send.Message,
		HopLimit:           hops,
		NotifyConversation: req.notify && send.SourceConversationID != "" && (!send.Wait || !n.isReachable(to.NodeAddress())),
		CreatedAt:          time.Now().UnixMilli(),
	}
	// 3. 先落盘再排入发送队列，同一次循环内登记等待者。
	if err := n.recordThreadActivity(ctx, &outboundMessage); err != nil {
		return clientResult{err: err}
	}
	err = n.deliver(ctx, &outboundMessage, to)
	if err == nil && send.Wait && outboundMessage.Status == WorldMessageStatusSending {
		n.waiters[outboundMessage.MessageID] = messageWaiter{req.finished, req.notify}
	}
	return clientResult{
		worldMessage: outboundMessage,
		err:          err,
	}
}

func (n *worldNodeImpl) request(ctx context.Context, req clientRequested) (clientResult, error) {
	// 1. 交给循环的操作在回复缓冲区完成，调用方取消不会阻塞节点。
	req.result = make(chan clientResult, 1)
	if !n.post(ctx, req) {
		return clientResult{}, cmp.Or(ctx.Err(), errors.New("world node stopped"))
	}
	select {
	case result := <-req.result:
		return result, result.err
	case <-n.stopped:
		return clientResult{}, errors.New("world node stopped")
	}
}

func (n *worldNodeImpl) sendMessage(ctx context.Context, send SendMessageRequest, notify bool) (WorldMessage, error) {
	message, finished, err := n.enqueueMessage(ctx, send, notify)
	if finished == nil {
		return message, err
	}
	return n.waitMessage(ctx, message, finished, notify)
}

func (n *worldNodeImpl) enqueueMessage(ctx context.Context, send SendMessageRequest, notify bool) (WorldMessage, <-chan WorldMessage, error) {
	finished := make(chan WorldMessage, 1)
	result, err := n.request(ctx, clientRequested{
		operation: "send",
		send:      send,
		notify:    notify,
		finished:  finished,
	})
	if err != nil || !send.Wait || result.worldMessage.Status != WorldMessageStatusSending {
		return result.worldMessage, nil, err
	}
	return result.worldMessage, finished, nil
}

func (n *worldNodeImpl) waitMessage(ctx context.Context, message WorldMessage, finished <-chan WorldMessage, notify bool) (WorldMessage, error) {
	// 超时只移交后续通知责任，保持最近一次持久化的投递状态。
	timer := time.NewTimer(n.opt.DefaultWait)
	defer timer.Stop()
	select {
	case outboundMessage := <-finished:
		return outboundMessage, nil
	case <-n.stopped:
		return WorldMessage{}, errors.New("world node stopped")
	case <-timer.C:
	case <-ctx.Done():
	}
	result, err := n.request(context.WithoutCancel(ctx), clientRequested{
		operation: "detach",
		messageID: message.MessageID,
		notify:    notify,
	})
	if ctx.Err() != nil {
		return result.worldMessage, ctx.Err()
	}
	return result.worldMessage, err
}

// wakeConversation 将带来源的回信交给原对话，输入标识在重试时保持稳定。
func (n *worldNodeImpl) wakeConversation(ctx context.Context, outboundMessage WorldMessage) {
	// 1. 同一消息只启动一个通知任务，交付结果由循环确认。
	n.notificationRunning[outboundMessage.MessageID] = true
	n.tasks.Go(func() {
		err := n.host.NotifyOutbound(ctx, outboundMessage)
		n.post(ctx, notificationFinished{outboundMessage, err})
	})
}

func writeControl(ctx context.Context, socket *websocket.Conn, kind string, value any) error {
	// 1. 将载荷放入控制面的协议封装。
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return wsjson.Write(ctx, socket, worldserver.ControlEnvelope{
		Type:    kind,
		Payload: payload,
	})
}

func (t *directFrameTransport) ReadFrame(ctx context.Context) ([]byte, error) {
	// 1. 只接收二进制帧。
	kind, data, err := t.socket.Read(ctx)
	if err == nil && kind != websocket.MessageBinary {
		return nil, errors.New("expected binary world frame")
	}
	return data, err
}
func (t *directFrameTransport) WriteFrame(ctx context.Context, frame []byte) error {
	// 1. 写入完整二进制帧。
	return t.socket.Write(ctx, websocket.MessageBinary, frame)
}
func (t *directFrameTransport) Close() error {
	// 1. 立即唤醒阻塞的收发。
	return t.socket.CloseNow()
}

// ValidateAdvertisedTargets 校验通告项以及裸节点地址对应的默认目标。
func ValidateAdvertisedTargets(defaultTarget string, targets []worldserver.AdvertisedTarget) error {
	for _, target := range targets {
		if !nodeOrTargetPattern.MatchString(target.Name) || (target.Visibility != "domain" && target.Visibility != "public") {
			return fmt.Errorf("invalid exposed world target %q", target.Name)
		}
	}
	if defaultTarget != "" && !isTargetAdvertised(targets, defaultTarget) {
		return errors.New("world default target must be exposed")
	}
	return nil
}

// recordDeliveryFailure 保存可重试原因，或结束这次投递并通知等待者。
func (n *worldNodeImpl) recordDeliveryFailure(ctx context.Context, message *WorldMessage, code, reason string, retryStatus WorldMessageStatus) error {
	message.ErrorCode = code
	if code != "world_target_unreachable" && code != "world_identity_unverified" {
		return n.completeOutbound(ctx, message, "", reason)
	}
	message.Status = retryStatus
	message.ErrorText = reason
	if n.waiters[message.MessageID].notify {
		message.NotifyConversation = message.ConversationID != ""
	}
	return n.saveMessage(ctx, message)
}
