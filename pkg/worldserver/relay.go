package worldserver

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/VaalaCat/openvaala/pkg/utils/syncmap"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/gin-gonic/gin"
)

// Relay 按公钥登记挂靠，为一次拨号配对两条二进制 WebSocket。
type Relay struct {
	instances   []string
	domain      *worldServerImpl
	attachments syncmap.SyncMap[string, relayAttachment]
	waiting     syncmap.SyncMap[string, relayPair]
	connections chan relayConnection
	stopped     chan struct{}
}

// ChannelFrameLimit 包含 16 MiB 明文、序号和 GCM 认证标签。
const ChannelFrameLimit = (16 << 20) + 8 + 16

type relayAttachment struct {
	local     chan<- string
	localBase string
	socket    *websocket.Conn
	// 这是由 relay 负责关闭的独立 attach 连接吗？
	independent bool
}

type relayPair struct {
	ctx    context.Context
	accept chan *websocket.Conn
}

type relayConnection struct {
	socket       *websocket.Conn
	path         string
	target       string
	connectionID string
}

// NewRelay 创建接受持钥者挂靠的 relay。
func NewRelay(instances []string) *Relay {
	return &Relay{instances: append([]string{}, instances...), connections: make(chan relayConnection), stopped: make(chan struct{})}
}

// RegisterRoutes 将三种已升级的连接交给运行中的 relay。
func (r *Relay) RegisterRoutes(router gin.IRouter) {
	if r.domain == nil {
		router.GET("/world/v1/relay/instances", func(c *gin.Context) {
			c.Header("Access-Control-Allow-Origin", "*")
			c.JSON(http.StatusOK, RelayInstancesResult{Instances: r.instances})
		})
	}
	for _, path := range []string{"attach", "dial", "accept"} {
		router.GET("/world/v1/relay/"+path, func(c *gin.Context) {
			socket, err := websocket.Accept(c.Writer, c.Request, &websocket.AcceptOptions{InsecureSkipVerify: path == "dial"})
			if err != nil {
				return
			}
			select {
			case r.connections <- relayConnection{socket, path, c.Query("target"), c.Query("connection_id")}:
			case <-r.stopped:
				socket.CloseNow()
			case <-c.Request.Context().Done():
				socket.CloseNow()
			}
		})
	}
}

// Run 接收挂靠与配对连接，退出前等待它们的读写任务。
func (r *Relay) Run(ctx context.Context) error {
	var tasks sync.WaitGroup
	defer tasks.Wait()
	defer close(r.stopped)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case conn := <-r.connections:
			tasks.Go(func() {
				switch conn.path {
				case "attach":
					r.handleAttach(ctx, conn.socket)
				case "dial":
					r.handleDial(ctx, conn.socket, conn.target)
				case "accept":
					r.handleAccept(conn.socket, conn.connectionID)
				}
			})
		}
	}
}

func relayKey(key []byte) string { return base64.RawURLEncoding.EncodeToString(key) }

func (r *Relay) attach(key []byte, attachment relayAttachment) {
	if previous, ok := r.attachments.Swap(relayKey(key), attachment); ok && previous.independent {
		previous.socket.CloseNow()
	}
}

func (r *Relay) detach(key []byte, socket *websocket.Conn) {
	r.attachments.DeleteIf(relayKey(key), func(attached relayAttachment) bool { return attached.socket == socket })
}

// revokeMember 结束被吊销成员在域内模式下的挂靠。
func (r *Relay) revokeMember(key []byte) {
	if r.domain == nil {
		return
	}
	if attachment, ok := r.attachments.LoadAndDelete(relayKey(key)); ok && attachment.independent {
		attachment.socket.CloseNow()
	}
}

// handleAttach 校验挑战签名，域内模式同时向控制面核对成员资格。
func (r *Relay) handleAttach(ctx context.Context, socket *websocket.Conn) {
	defer socket.CloseNow()
	socket.SetReadLimit(controlSizeLimit)
	proofCtx, cancel := context.WithTimeout(ctx, challengeTimeout)
	key, err := readChallengeProof(proofCtx, socket, ChallengeRelayAttach)
	cancel()
	if err != nil {
		socket.Close(websocket.StatusPolicyViolation, "authentication rejected")
		return
	}
	if r.domain != nil {
		result := make(chan error, 1)
		if err = r.domain.submitWorldServerEvent(ctx, worldServerRelayAttachRequested{key, socket, result}); err != nil {
			return
		}
		if err = <-result; err != nil {
			socket.Close(websocket.StatusPolicyViolation, "authentication rejected")
			return
		}
	} else {
		r.attach(key, relayAttachment{socket: socket, independent: true})
	}
	defer r.detach(key, socket)
	if err := writeControl(ctx, socket, ControlTypeRelayAttached, nil); err != nil {
		return
	}
	<-socket.CloseRead(ctx).Done()
}

// readChallengeProof 核对公钥派生的节点标识及此次连接的挑战签名。
func readChallengeProof(ctx context.Context, socket *websocket.Conn, purpose ChallengePurpose) (ed25519.PublicKey, error) {
	challenge := make([]byte, 32)
	rand.Read(challenge)
	if err := writeControl(ctx, socket, ControlTypeChallenge, ChallengeMessage{challenge}); err != nil {
		return nil, err
	}
	var envelope ControlEnvelope
	if err := wsjson.Read(ctx, socket, &envelope); err != nil {
		return nil, err
	}
	var proof ChallengeResponse
	if envelope.Type != ControlTypeChallengeResponse || json.Unmarshal(envelope.Payload, &proof) != nil {
		return nil, errors.New("invalid challenge response")
	}
	now := time.Now().Unix()
	skew := int64(challengeSkew / time.Second)
	if proof.Timestamp < now-skew || proof.Timestamp > now+skew {
		return nil, errors.New("invalid challenge timestamp")
	}
	if len(proof.PublicKey) != ed25519.PublicKeySize || proof.NodeID != GetNodeIDFromSignPublic(proof.PublicKey) ||
		!ed25519.Verify(proof.PublicKey, ChallengeSignBytes(purpose, challenge, proof.NodeID, proof.Timestamp), proof.Signature) {
		return nil, errors.New("invalid node identity proof")
	}
	return proof.PublicKey, nil
}

// handleAccept 消费一次连接号，成功交接后由拨号任务关闭两端。
func (r *Relay) handleAccept(socket *websocket.Conn, id string) {
	socket.SetReadLimit(ChannelFrameLimit)
	if pair, ok := r.waiting.LoadAndDelete(id); ok && pair.ctx.Err() == nil {
		select {
		case pair.accept <- socket:
			return
		case <-pair.ctx.Done():
		}
	}
	socket.Close(4004, "world_target_unreachable")
}

// handleDial 在配对期间读取发起方首帧，配对成功后按消息边界双向转发。
func (r *Relay) handleDial(parent context.Context, dial *websocket.Conn, target string) {
	defer dial.CloseNow()
	attachment, ok := r.attachments.Load(target)
	if !ok {
		dial.Close(4004, "world_target_unreachable")
		return
	}
	dial.SetReadLimit(ChannelFrameLimit)
	ctx, cancel := context.WithCancel(parent)
	var tasks sync.WaitGroup
	defer tasks.Wait()
	defer cancel()
	frames := make(chan []byte, 1)
	tasks.Go(func() {
		defer cancel()
		for {
			kind, frame, err := dial.Read(ctx)
			if err != nil || kind != websocket.MessageBinary {
				return
			}
			select {
			case frames <- frame:
			case <-ctx.Done():
				return
			}
		}
	})
	pairCtx, stop := context.WithTimeout(ctx, 10*time.Second)
	defer stop()
	pair := relayPair{pairCtx, make(chan *websocket.Conn)}
	var nonce [16]byte
	rand.Read(nonce[:])
	id := base64.RawURLEncoding.EncodeToString(nonce[:])
	r.waiting.Store(id, pair)
	defer r.waiting.Delete(id)
	// 挂靠连接由 relay 持有，通知写入沿用控制帧期限。
	if attachment.local != nil {
		select {
		case attachment.local <- (RelayIncoming{id}).GetAcceptURL(attachment.localBase):
		case <-pairCtx.Done():
			return
		}
	} else if err := writeControl(parent, attachment.socket, ControlTypeRelayIncoming, RelayIncoming{id}); err != nil {
		dial.Close(4004, "world_target_unreachable")
		return
	}
	var accept *websocket.Conn
	select {
	case accept = <-pair.accept:
	case <-pairCtx.Done():
		if ctx.Err() == nil {
			dial.Close(4004, "world_target_unreachable")
		}
		return
	}
	stop()
	defer accept.CloseNow()
	tasks.Go(func() {
		defer cancel()
		for {
			kind, frame, err := accept.Read(ctx)
			if err != nil || kind != websocket.MessageBinary || dial.Write(ctx, kind, frame) != nil {
				return
			}
		}
	})
	for {
		select {
		case <-ctx.Done():
			return
		case frame := <-frames:
			if accept.Write(ctx, websocket.MessageBinary, frame) != nil {
				return
			}
		}
	}
}
