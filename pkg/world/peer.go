package world

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/VaalaCat/openvaala/pkg/httpclient"
	"github.com/VaalaCat/openvaala/pkg/utils/syncmap"
	"github.com/VaalaCat/openvaala/pkg/worldserver"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const (
	relayInstanceCount    = 2
	relayInstancesRefresh = 10 * time.Minute
	relayInstancesCache   = time.Minute
	relayInstancesTimeout = 5 * time.Second
	relayDialTimeout      = 15 * time.Second
)

// PeerDialer 拨通目标节点或域，按入口缓存公开 relay 的实例列表。零值可以直接使用。
type PeerDialer struct {
	instances syncmap.SyncMap[string, relayInstancesEntry]
}

type relayInstancesEntry struct {
	instances []string
	fetched   time.Time
}

// DialPeer 在一个直连预算内按序尝试地址，再分别经各个 relay 完成配对与握手。
func (d *PeerDialer) DialPeer(ctx context.Context, address string, peerKey ed25519.PublicKey, entries worldserver.PeerEntries, identity *NodeIdentity, purpose ChannelPurpose) (SecureChannel, error) {
	target := "target=" + base64.RawURLEncoding.EncodeToString(peerKey)

	direct, cancel := context.WithTimeout(ctx, 2*time.Second)
	for _, base := range entries.DirectURLs {
		channel, err := dialPeerChannel(direct, base, "/world/v1/direct", target+"&purpose="+url.QueryEscape(string(purpose)), identity, peerKey, purpose)
		if err == nil {
			cancel()
			return channel, nil
		}
		if direct.Err() != nil {
			break
		}
	}
	cancel()
	for _, base := range entries.Relays {
		if ctx.Err() != nil {
			break
		}
		entry, ok := d.instances.Load(base)
		if !ok || time.Since(entry.fetched) >= relayInstancesCache {
			// 取列表失败时沿用上次的列表，没有就按单实例处理；失败结果同样缓存，避免每次拨号都等超时。
			instances, err := getRelayInstances(ctx, base)
			if err != nil {
				instances = entry.instances
			}
			entry = relayInstancesEntry{instances, time.Now()}
			if err == nil || ctx.Err() == nil {
				d.instances.Store(base, entry)
			}
		}
		selected := selectRelayInstances(entry.instances, peerKey)
		if len(selected) == 0 {
			selected = []string{base}
		}
		for _, instance := range selected {
			attempt, stop := context.WithTimeout(ctx, relayDialTimeout)
			channel, err := dialPeerChannel(attempt, instance, "/world/v1/relay/dial", target, identity, peerKey, purpose)
			stop()
			if err == nil {
				return channel, nil
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, &worldError{"world_target_unreachable", fmt.Sprintf("cannot reach %s: direct addresses and all relays are unreachable", address)}
}

// getRelayInstances 请求入口的公开列表。尚未发布该接口的入口按单实例处理。
func getRelayInstances(ctx context.Context, relayURL string) ([]string, error) {
	endpoint, err := relayInstancesURL(relayURL)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, relayInstancesTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	client := httpclient.NewHTTPClient(httpclient.NewHTTPClientOpt())
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get relay instances: %s", resp.Status)
	}
	var result worldserver.RelayInstancesResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Instances, nil
}

// dialPeerChannel 在同一拨号期限内完成 WebSocket 升级和端到端握手。
func dialPeerChannel(ctx context.Context, base, path, query string, identity *NodeIdentity, peerKey ed25519.PublicKey, purpose ChannelPurpose) (SecureChannel, error) {
	endpoint, err := GetWorldSocketURL(base, path)
	if err != nil {
		return nil, err
	}
	socket, err := dialChannelSocket(ctx, endpoint+"?"+query)
	if err != nil {
		return nil, err
	}
	return DialSecureChannel(ctx, &directFrameTransport{socket}, identity, peerKey, purpose)
}

// AcceptRelayChannel 拨回本次配对地址并用本机身份完成端到端握手。
func AcceptRelayChannel(ctx context.Context, acceptURL string, identity NodeIdentity) (AcceptedChannel, error) {
	socket, err := dialChannelSocket(ctx, acceptURL)
	if err != nil {
		return AcceptedChannel{}, err
	}
	return AcceptSecureChannel(ctx, &directFrameTransport{socket}, identity)
}

func dialChannelSocket(ctx context.Context, endpoint string) (*websocket.Conn, error) {
	socket, _, err := websocket.Dial(ctx, endpoint, nil)
	if err != nil {
		return nil, err
	}
	socket.SetReadLimit(worldserver.ChannelFrameLimit)
	return socket, nil
}

// handleAcceptedPeer 将 RPC 通道交给 Runtime，读出 World 来信后提交给节点循环。
func (n *worldNodeImpl) handleAcceptedPeer(nodeCtx, ctx context.Context, accepted AcceptedChannel) {
	if accepted.Purpose == ChannelPurposeRPC && n.rpc != nil {
		_ = n.rpc.ServeWorldRPC(nodeCtx, accepted.Channel)
		return
	}
	if accepted.Purpose != ChannelPurposeWorld {
		accepted.Channel.Close()
		return
	}
	message, err := readWorldMessage(ctx, accepted.Channel)
	if err != nil || !n.post(ctx, channelNegotiated{secure: accepted.Channel, peerKey: accepted.PeerKey, firstMessage: &message}) {
		accepted.Channel.Close()
	}
}

const inboundChannelTimeout = 5 * time.Second

// runControlConnection 维持节点到本域控制面的连接，断开后按退避间隔重连。
func runControlConnection(ctx context.Context, base string, identity NodeIdentity, announce worldserver.NodeAnnounce, events chan<- nodeEvent) {
	delay := 100 * time.Millisecond
	for ctx.Err() == nil {
		socket, err := dialAttachment(ctx, base, "/world/v1/connect")
		if err == nil {
			if readControlConnection(ctx, socket, base, identity, announce, events) {
				delay = 100 * time.Millisecond
			}
			socket.CloseNow()
			sendAttachmentEvent[nodeEvent](ctx, events, controlReceived{socket: socket})
			sendAttachmentEvent[nodeEvent](ctx, events, RelayAttachmentChanged{base, false})
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, 5*time.Second)
	}
}

// readControlConnection 上报公告并处理本域控制帧，本次连接是否成功？
func readControlConnection(ctx context.Context, socket *websocket.Conn, base string, identity NodeIdentity, announce worldserver.NodeAnnounce, events chan<- nodeEvent) bool {
	handshake, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if answerAttachmentChallenge(handshake, socket, identity, worldserver.ChallengeConnect) != nil ||
		writeControl(handshake, socket, worldserver.ControlTypeNodeAnnounce, announce) != nil {
		return false
	}
	connected := false
	readCtx := handshake
	for {
		var frame worldserver.ControlEnvelope
		if wsjson.Read(readCtx, socket, &frame) != nil {
			return connected
		}
		switch frame.Type {
		case worldserver.ControlTypeRelayAttached:
			connected = true
			readCtx = ctx
			sendAttachmentEvent[nodeEvent](ctx, events, RelayAttachmentChanged{base, true})
		case worldserver.ControlTypeRelayIncoming:
			if e, ok := getRelayIncoming(base, frame.Payload); ok {
				sendAttachmentEvent[nodeEvent](ctx, events, e)
			}
		case worldserver.ControlTypePing:
			writeCtx, stop := context.WithTimeout(ctx, 2*time.Second)
			err := writeControl(writeCtx, socket, worldserver.ControlTypePong, nil)
			stop()
			if err != nil {
				return connected
			}
		case worldserver.ControlTypeDirectory:
			connected = true
			readCtx = ctx
			sendAttachmentEvent[nodeEvent](ctx, events, controlReceived{socket, frame})
		default:
			sendAttachmentEvent[nodeEvent](ctx, events, controlReceived{socket, frame})
		}
	}
}

// relayAttachment 保存一次实例挂靠的取消入口和确认状态。
type relayAttachment struct {
	cancel   context.CancelFunc
	attached bool
}

// relayInstanceUpdate 由实例连接在挂靠确认或断开时发给入口循环。
type relayInstanceUpdate struct {
	instance   string
	attachment *relayAttachment
	attached   bool
}

// RunRelayAttachments 维护一个公开入口的实例挂靠，按入口报告可达状态。
func RunRelayAttachments(ctx context.Context, base string, identity NodeIdentity, events chan<- RelayEvent) {
	var tasks sync.WaitGroup
	defer tasks.Wait()
	attachments := make(map[string]*relayAttachment)
	updates := make(chan relayInstanceUpdate)
	var selected []string
	reportedConnected := false
	delay := 100 * time.Millisecond
	refresh := time.NewTimer(0)
	defer refresh.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-refresh.C:
			// 1. 启动、重连和定期刷新时选实例；发现失败则沿用已有挂靠。
			instances, err := getRelayInstances(ctx, base)
			if err == nil || len(attachments) == 0 {
				selected = selectRelayInstances(instances, identity.SignPublic)
				if len(selected) == 0 {
					selected = []string{base}
				}
			}
			for _, instance := range selected {
				if attachments[instance] != nil {
					continue
				}
				connCtx, cancel := context.WithCancel(ctx)
				a := &relayAttachment{cancel: cancel}
				attachments[instance] = a
				tasks.Go(func() { a.Run(connCtx, instance, identity, events, updates) })
			}
			refresh.Reset(relayInstancesRefresh)
		case update := <-updates:
			// 2. 只处理登记中的这次挂靠。仍被选中的实例断开后，按退避间隔重试。
			a := attachments[update.instance]
			if a != update.attachment {
				continue
			}
			a.attached = update.attached
			if update.attached {
				delay = 100 * time.Millisecond
			} else {
				a.cancel()
				delete(attachments, update.instance)
				if slices.Contains(selected, update.instance) {
					refresh.Reset(delay)
					delay = min(delay*2, 5*time.Second)
				}
			}
		}

		// 3. 等新选择全部确认后撤掉旧挂靠；任一实例已挂上，入口即可达。
		selectedAttached := isRelaySelectionAttached(selected, attachments)
		connected := false
		for instance, a := range attachments {
			if selectedAttached && !slices.Contains(selected, instance) {
				a.cancel()
				delete(attachments, instance)
				continue
			}
			connected = connected || a.attached
		}
		if connected != reportedConnected {
			reportedConnected = connected
			sendAttachmentEvent[RelayEvent](ctx, events, RelayAttachmentChanged{base, connected})
		}
	}
}

// Run 以签名身份挂靠一个公开实例，确认后将拨入通知交给节点或域控制面。
func (a *relayAttachment) Run(ctx context.Context, instance string, identity NodeIdentity, events chan<- RelayEvent, updates chan<- relayInstanceUpdate) {
	report := func(attached bool) {
		select {
		case updates <- relayInstanceUpdate{instance, a, attached}:
		case <-ctx.Done():
		}
	}
	defer report(false)
	socket, err := dialAttachment(ctx, instance, "/world/v1/relay/attach")
	if err != nil {
		return
	}
	defer socket.CloseNow()
	handshake, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if answerAttachmentChallenge(handshake, socket, identity, worldserver.ChallengeRelayAttach) != nil {
		return
	}
	var frame worldserver.ControlEnvelope
	if wsjson.Read(handshake, socket, &frame) != nil || frame.Type != worldserver.ControlTypeRelayAttached {
		return
	}
	report(true)
	for wsjson.Read(ctx, socket, &frame) == nil {
		if frame.Type == worldserver.ControlTypeRelayIncoming {
			if e, ok := getRelayIncoming(instance, frame.Payload); ok {
				sendAttachmentEvent[RelayEvent](ctx, events, e)
			}
		}
	}
}

func dialAttachment(ctx context.Context, base, path string) (*websocket.Conn, error) {
	endpoint, err := GetWorldSocketURL(base, path)
	if err != nil {
		return nil, err
	}
	dialCtx, cancel := context.WithTimeout(ctx, inboundChannelTimeout)
	defer cancel()
	socket, _, err := websocket.Dial(dialCtx, endpoint, nil)
	if err != nil {
		return nil, err
	}
	socket.SetReadLimit(1 << 20)
	return socket, nil
}

func getRelayIncoming(base string, payload json.RawMessage) (RelayIncoming, bool) {
	var incoming worldserver.RelayIncoming
	if json.Unmarshal(payload, &incoming) != nil || incoming.ConnectionID == "" {
		return RelayIncoming{}, false
	}
	return RelayIncoming{incoming.GetAcceptURL(base)}, true
}

// sendAttachmentEvent 将挂靠通知交给本进程的接收方。
func sendAttachmentEvent[T nodeEvent](ctx context.Context, events chan<- T, event T) {
	select {
	case events <- event:
	case <-ctx.Done():
	}
}

// getRelayStatus 按发布的拨号顺序返回最近的挂靠状态。
func (n *worldNodeImpl) getRelayStatus() []RelayStatus {
	status := make([]RelayStatus, len(n.opt.Relays))
	for i, base := range n.opt.Relays {
		status[i] = RelayStatus{URL: base, Connected: n.attachments[base], Implicit: i == 0}
	}
	return status
}

// answerAttachmentChallenge 用挂靠者身份签署本次连接收到的挑战。
func answerAttachmentChallenge(ctx context.Context, socket *websocket.Conn, identity NodeIdentity, purpose worldserver.ChallengePurpose) error {
	var envelope worldserver.ControlEnvelope
	var challenge worldserver.ChallengeMessage
	if err := wsjson.Read(ctx, socket, &envelope); err != nil {
		return err
	}
	if envelope.Type != worldserver.ControlTypeChallenge || json.Unmarshal(envelope.Payload, &challenge) != nil || len(challenge.Challenge) != 32 {
		return fmt.Errorf("invalid relay challenge")
	}
	response := worldserver.ChallengeResponse{NodeID: identity.NodeID, PublicKey: identity.SignPublic, Timestamp: time.Now().Unix()}
	signature, err := identity.Sign(worldserver.ChallengeSignBytes(purpose, challenge.Challenge, response.NodeID, response.Timestamp))
	if err != nil {
		return err
	}
	response.Signature = signature
	return writeControl(ctx, socket, worldserver.ControlTypeChallengeResponse, response)
}
