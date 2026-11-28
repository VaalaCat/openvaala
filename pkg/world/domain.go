package world

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/utils/syncmap"
	"github.com/VaalaCat/openvaala/pkg/worldserver"
	"github.com/coder/websocket"
)

type domainChannels struct {
	peerDialer  PeerDialer
	attachments syncmap.SyncMap[string, bool]
}

// NewDomainChannels 提供以域身份挂靠和查询成员的传输。
func NewDomainChannels() worldserver.DomainChannels { return &domainChannels{} }

type domainQuery struct {
	worldserver.DomainTrustMessage
	Node string `json:"node,omitempty"`
}

type domainQueryError struct {
	Code  string `json:"code"`
	Error string `json:"error"`
}

// ServeDirect 验证域握手，处理一次成员查询后关闭通道。
func (d *domainChannels) ServeDirect(ctx context.Context, socket *websocket.Conn, local worldserver.DomainIdentity, directory worldserver.DomainDirectory) error {
	ctx, cancel := context.WithTimeout(ctx, inboundChannelTimeout)
	defer cancel()
	return serveDomainQuery(ctx, socket, local, directory)
}

// serveDomainQuery 通过直连或 relay 接受域握手并回复成员查询。
func serveDomainQuery(ctx context.Context, socket *websocket.Conn, local worldserver.DomainIdentity, directory worldserver.DomainDirectory) error {
	accepted, err := AcceptSecureChannel(ctx, &directFrameTransport{socket}, domainNodeIdentity(local))
	if err != nil {
		return err
	}
	defer accepted.Channel.Close()
	if accepted.Purpose != ChannelPurposeDomain {
		return errors.New("expected domain channel")
	}
	// 两侧都先写自己的操作。握手确认对端身份后即可发出本域待确认的邀请。
	card, err := directory.GetPendingTrustCard(ctx, accepted.PeerKey)
	if err != nil {
		return err
	}
	if card != "" {
		if err := writeDomainTrust(ctx, accepted.Channel, worldserver.DomainTrustMessage{Op: "trust_request", Card: card}); err != nil {
			return err
		}
	}
	data, err := accepted.Channel.ReadMessage(ctx)
	if err != nil {
		return err
	}
	var req domainQuery
	if err := json.Unmarshal(data, &req); err != nil {
		return err
	}
	if req.Op == "trust_request" || req.Op == "trust_response" {
		return replyDomainTrust(ctx, accepted.Channel, directory, accepted.PeerKey, req.DomainTrustMessage)
	}
	if req.Op != "lookup" || !nodeOrTargetPattern.MatchString(req.Node) {
		return errors.New("invalid domain lookup")
	}
	// 对端的信任操作本身就能解决待确认的邀请；查人操作另行回送决定。
	if card != "" {
		reply, err := accepted.Channel.ReadMessage(ctx)
		if err != nil {
			return err
		}
		if err := receiveDomainDecision(ctx, directory, accepted.PeerKey, reply); err != nil {
			return err
		}
	}
	member, err := directory.GetMember(ctx, accepted.PeerKey, req.Node)
	var missing worldserver.PeerNotFoundError
	switch {
	case errors.As(err, &missing):
		data, err = json.Marshal(domainQueryError{"world_peer_not_found", missing.Error()})
	case err == nil:
		data, err = json.Marshal(member)
	}
	if err != nil {
		return err
	}
	return accepted.Channel.WriteMessage(ctx, data)
}

// SendTrust 以控制面身份发送邀请或决定，等对端持久化后结束。
func (d *domainChannels) SendTrust(ctx context.Context, local worldserver.DomainIdentity, peer worldserver.PeerDomain, trust worldserver.DomainTrustMessage, directory worldserver.DomainDirectory) error {
	ctx, cancel := context.WithTimeout(ctx, inboundChannelTimeout)
	defer cancel()
	data, err := d.exchangeDomainQuery(ctx, local, peer, directory, domainQuery{DomainTrustMessage: trust})
	if err != nil {
		return err
	}
	return receiveDomainDecision(ctx, directory, peer.PublicKey, data)
}

func receiveDomainDecision(ctx context.Context, directory worldserver.DomainDirectory, peer ed25519.PublicKey, data []byte) error {
	var reply worldserver.DomainTrustMessage
	if err := json.Unmarshal(data, &reply); err != nil || reply.Op == "" {
		return err
	}
	if reply.Op != "trust_response" {
		return errors.New("expected domain trust response")
	}
	_, err := directory.ReceiveTrust(ctx, peer, reply)
	return err
}

func writeDomainTrust(ctx context.Context, channel SecureChannel, msg worldserver.DomainTrustMessage) error {
	payload, _ := json.Marshal(msg)
	return channel.WriteMessage(ctx, payload)
}

func replyDomainTrust(ctx context.Context, channel SecureChannel, directory worldserver.DomainDirectory, peer ed25519.PublicKey, msg worldserver.DomainTrustMessage) error {
	reply, err := directory.ReceiveTrust(ctx, peer, msg)
	if err != nil {
		return err
	}
	return writeDomainTrust(ctx, channel, reply)
}

// exchangeDomainQuery 在认证通道上执行一次操作。已发出的邀请或决定与对端邀请交叉时，无需再发一份。
func (d *domainChannels) exchangeDomainQuery(ctx context.Context, local worldserver.DomainIdentity, peer worldserver.PeerDomain, directory worldserver.DomainDirectory, req domainQuery) ([]byte, error) {
	identity := domainNodeIdentity(local)
	channel, err := d.peerDialer.DialPeer(ctx, peer.Domain, peer.PublicKey, peer.Entries, &identity, ChannelPurposeDomain)
	if err != nil {
		return nil, err
	}
	defer channel.Close()
	ctx, cancel := context.WithTimeout(ctx, inboundChannelTimeout)
	defer cancel()
	payload, _ := json.Marshal(req)
	if err := channel.WriteMessage(ctx, payload); err != nil {
		return nil, err
	}
	data, err := channel.ReadMessage(ctx)
	if err != nil {
		return nil, err
	}
	var opening worldserver.DomainTrustMessage
	if err := json.Unmarshal(data, &opening); err != nil || opening.Op != "trust_request" {
		return data, err
	}
	if req.Op == "lookup" {
		if err := replyDomainTrust(ctx, channel, directory, peer.PublicKey, opening); err != nil {
			return nil, err
		}
	}
	return channel.ReadMessage(ctx)
}

// Lookup 用名片中的域公钥验证响应方，读取其确认的成员身份。
func (d *domainChannels) Lookup(ctx context.Context, local worldserver.DomainIdentity, peer worldserver.PeerDomain, node string, directory worldserver.DomainDirectory) (worldserver.DomainMember, error) {
	var member worldserver.DomainMember
	query := domainQuery{Node: node}
	query.Op = "lookup"
	data, err := d.exchangeDomainQuery(ctx, local, peer, directory, query)
	if err != nil {
		return member, err
	}
	var failure domainQueryError
	if err = json.Unmarshal(data, &failure); err != nil {
		return member, err
	}
	if failure.Code == "world_peer_not_found" {
		return member, worldserver.PeerNotFoundError(node + "@" + peer.Domain)
	}
	if err = json.Unmarshal(data, &member); err != nil || len(member.PublicKey) != ed25519.PublicKeySize || member.Owner == "" {
		return member, errors.New("invalid domain member")
	}
	return member, nil
}

// RunAttachments 维护域公钥在各 relay 的挂靠，按连接号接通查询。
func (d *domainChannels) RunAttachments(ctx context.Context, local worldserver.DomainIdentity, relays []string, localAcceptURLs <-chan string, directory worldserver.DomainDirectory) {
	var tasks sync.WaitGroup
	defer tasks.Wait()
	identity := domainNodeIdentity(local)
	events := make(chan RelayEvent)
	for _, relay := range relays {
		base, err := GetWorldSocketURL(relay, "")
		if err != nil {
			continue
		}
		if _, exists := d.attachments.LoadOrStore(base, false); exists {
			continue
		}
		tasks.Go(func() { RunRelayAttachments(ctx, base, identity, events) })
	}
	for {
		var acceptURL string
		select {
		case <-ctx.Done():
			return
		case acceptURL = <-localAcceptURLs:
		case event := <-events:
			switch e := event.(type) {
			case RelayIncoming:
				acceptURL = e.AcceptURL
			case RelayAttachmentChanged:
				d.attachments.Store(e.URL, e.Connected)
				continue
			}
		}
		tasks.Go(func() {
			attempt, cancel := context.WithTimeout(ctx, inboundChannelTimeout)
			defer cancel()
			socket, err := dialChannelSocket(attempt, acceptURL)
			if err == nil {
				_ = serveDomainQuery(attempt, socket, local, directory)
			}
		})
	}
}

// GetRelayAttachments 返回本域在各外部 relay 的挂靠状态。
func (d *domainChannels) GetRelayAttachments() map[string]bool {
	return d.attachments.ToMap()
}

// updateDomains 替换域信任快照，并清除已删除或换钥域的缓存。
func (n *worldNodeImpl) updateDomains(cards []worldserver.PeerDomain) {
	domains := make(map[string]worldserver.PeerDomain, len(cards))
	for _, card := range cards {
		domains[card.Domain] = card
	}
	for name, old := range n.domains {
		if bytes.Equal(domains[name].PublicKey, old.PublicKey) {
			continue
		}
		n.memberCache.Range(func(key memberCacheKey, _ cachedMember) bool {
			if key.domain == name {
				n.memberCache.Delete(key)
			}
			return true
		})
	}
	n.domains = domains
}

func (n *worldNodeImpl) getCachedMember(key memberCacheKey, now time.Time) (worldserver.DomainMember, bool) {
	saved, ok := n.memberCache.Load(key)
	return saved.member, ok && now.Before(saved.expires)
}

// lookupMember 在原控制连接上等待本次请求，断连时结束等待。
func (n *worldNodeImpl) lookupMember(ctx context.Context, addr Address, domain worldserver.PeerDomain, control *websocket.Conn, disconnected <-chan struct{}, needEntries bool) (worldserver.DomainMember, error) {
	var member worldserver.DomainMember
	if domain.Domain == "" {
		return member, domainNotTrusted(addr.Domain)
	}
	key := memberCacheKey{addr.Domain, string(domain.PublicKey), addr.Node}
	if saved, ok := n.getCachedMember(key, time.Now()); ok && (!needEntries || len(saved.Entries.DirectURLs)+len(saved.Entries.Relays) > 0) {
		return saved, nil
	}
	if control == nil {
		return member, identityUnverified(addr.NodeAddress())
	}
	req := worldserver.DomainLookup{RequestID: string(defs.NewItemID()), Domain: addr.Domain, Node: addr.Node}
	result := make(chan worldserver.DomainLookupResult, 1)
	n.lookupWaiters.Store(req.RequestID, memberLookupWaiter{key, result})
	defer n.lookupWaiters.Delete(req.RequestID)
	if err := writeControl(ctx, control, worldserver.ControlTypeDomainLookup, req); err != nil {
		return member, identityUnverified(addr.NodeAddress())
	}
	select {
	case reply := <-result:
		if reply.Code != "" {
			return member, &worldError{reply.Code, reply.Error}
		}
		return *reply.Member, nil
	case <-disconnected:
		return member, identityUnverified(addr.NodeAddress())
	case <-ctx.Done():
		return member, ctx.Err()
	}
}

// handleDomainLookupResult 把同一请求的结果交给等待者，并缓存仍受本域信任的身份。
func (n *worldNodeImpl) handleDomainLookupResult(payload json.RawMessage) {
	var reply worldserver.DomainLookupResult
	if json.Unmarshal(payload, &reply) != nil {
		return
	}
	waiter, ok := n.lookupWaiters.LoadAndDelete(reply.RequestID)
	if !ok {
		return
	}
	if reply.Code == "" {
		trusted := string(n.domains[waiter.key.domain].PublicKey) == waiter.key.domainKey
		if !trusted || reply.Member == nil || string(reply.DomainKey) != waiter.key.domainKey {
			err := identityUnverified(waiter.key.node + "@" + waiter.key.domain)
			reply.Code, reply.Error = ErrorCode(err), err.Error()
		} else {
			n.memberCache.Store(waiter.key, cachedMember{*reply.Member, time.Now().Add(time.Hour)})
		}
	}
	waiter.result <- reply
}
