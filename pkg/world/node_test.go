package world

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VaalaCat/llmkit"
	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/storage/sqlite"
	"github.com/VaalaCat/openvaala/pkg/worldserver"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/gin-gonic/gin"
)

const testDomain = "world.example"

// federationModel 用真实 Runtime 执行 Turn，并记录调用次数。
type federationModel struct {
	calls   atomic.Int32
	release chan struct{}
}

func (m *federationModel) Call(ctx context.Context, _ llmkit.Request, _ llmkit.Target, _ llmkit.CallOptions) (<-chan llmkit.Event, error) {
	// 1. 在测试允许时完成模型调用。
	number := m.calls.Add(1)
	if m.release != nil {
		select {
		case <-m.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	stream := make(chan llmkit.Event, 2)
	stream <- llmkit.Event{Type: llmkit.EventContentDelta, Delta: &llmkit.DeltaPayload{Text: fmt.Sprintf("reply-%d", number)}}
	stream <- llmkit.Event{Type: llmkit.EventDone, FinishReason: "stop"}
	close(stream)
	return stream, nil
}

type testFederation struct {
	control worldserver.WorldServer
	url     string
	stop    func()
}
type testWorldNode struct {
	node  WorldNode
	opt   *WorldNodeOpt
	rt    runtime.Runtime
	model *federationModel
	url   string
	stop  func()
}

func awaitWorld(t *testing.T, condition func() bool) {
	t.Helper()
	// 1. 等待可从公开接口或记录观察到的业务状态。
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("world condition did not become true")
}
func worldHTTP(t *testing.T, method, url string, value any, status int) []byte {
	t.Helper()
	// 1. 通过客户端或控制面路由读取完整响应。
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer integration-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != status {
		t.Fatalf("%s %s: %d %s (%v), want %d", method, url, resp.StatusCode, body, err, status)
	}
	return body
}
func decodeWorld[T any](t *testing.T, data []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	return v
}
func newTestFederation(t *testing.T, relayMode ...string) *testFederation {
	t.Helper()
	// 1. 在独立临时数据库运行真实控制面，等待健康检查。
	opt := worldserver.NewWorldServerOpt(testDomain, filepath.Join(t.TempDir(), "server", "world.db"), "integration-test")
	if len(relayMode) > 0 {
		opt.RelayMode = relayMode[0]
	}
	server, err := worldserver.NewWorldServer(opt)
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	server.RegisterRoutes(router)
	httpServer := httptest.NewServer(router)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	stop := sync.OnceFunc(func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
		httpServer.Close()
	})
	t.Cleanup(stop)
	f := &testFederation{server, httpServer.URL, stop}
	awaitWorld(t, func() bool {
		resp, err := http.Get(f.url + "/world/v1/healthz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == 200
	})
	return f
}

func (f *testFederation) register(t *testing.T, name string) NodeIdentity {
	t.Helper()
	// 1. 签发一次性凭据并使用本机真实身份入网。
	identity := testNodeIdentity(t)
	token := decodeWorld[worldserver.CreateJoinTokenResponse](t, worldHTTP(t, "POST", f.url+"/world/v1/admin/tokens", worldserver.CreateJoinTokenRequest{Owner: "integration", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}, 201))
	req := worldserver.RegisterNodeRequest{NodeName: name, JoinToken: token.Token, SignPublic: identity.SignPublic, BoxPublic: identity.BoxPublic}
	req.Signature, _ = identity.Sign(worldserver.RegisterSignBytes(req))
	worldHTTP(t, "POST", f.url+"/world/v1/nodes/register", req, 201)
	return identity
}

func (f *testFederation) node(t *testing.T, name, direct string, relays ...string) *testWorldNode {
	t.Helper()
	// 1. 创建独立的联邦库和 Runtime 库，注册工具后再装配 Runtime。
	db, err := sqlite.OpenDatabase(sqlite.NewMemoryDatabaseOpt(WorldNamespaces()...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	runtimeDB, err := sqlite.OpenDatabase(sqlite.NewMemoryDatabaseOpt(dao.RecordNamespaces()...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtimeDB.Close() })
	router := gin.New()
	httpServer := httptest.NewUnstartedServer(router)
	addr := "http://" + httpServer.Listener.Addr().String()
	opt := NewWorldNodeOpt(db.Record(), f.register(t, name), name, testDomain, f.url)
	opt.Relays = relays
	opt.DefaultTarget = "assistant"
	opt.AdvertiseTargets = []worldserver.AdvertisedTarget{{Name: "assistant", Description: "测试助手", Visibility: "domain"}}
	opt.DefaultWait = 2 * time.Second
	if direct == "self" {
		direct = addr
	}
	if direct != "" {
		opt.DirectURLs = []string{direct}
	}
	node, err := NewWorldNode(opt)
	if err != nil {
		t.Fatal(err)
	}
	RegisterDirectRoutes(router, node, nil)
	ref := defs.ModelRef{ProviderID: "test", ModelID: "test"}
	model := new(federationModel)
	rt, err := runtime.NewRuntime(runtime.NewRuntimeOpt(runtimeDB.Record(), runtimeDB.Log(), &runtime.Definitions{
		Agents: []*agent.AgentDefinition{{Name: "assistant", Model: ref}}, Models: []*defs.ModelDefinition{{Ref: ref, Target: &llmkit.Target{Protocol: llmkit.ProtocolOpenAIChat, Model: "test", BaseURL: "http://unused.invalid"}}},
		ExtraTools: node,
	}).WithModelClient(model))
	if err != nil {
		t.Fatal(err)
	}
	httpServer.Start()
	t.Cleanup(httpServer.Close)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rt.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	})
	return &testWorldNode{node: node, opt: opt, rt: rt, model: model, url: addr}
}
func (w *testWorldNode) start(t *testing.T) {
	t.Helper()
	// 1. 节点退出后再停止 Runtime。
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.node.Run(ctx, NewRuntimeHost(w.rt), nil) }()
	w.stop = sync.OnceFunc(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(6 * time.Second):
			t.Error("world node did not stop")
		}
	})
	t.Cleanup(w.stop)
}
func (w *testWorldNode) waitPeer(t *testing.T, name string, online bool) {
	t.Helper()
	awaitWorld(t, func() bool {
		result, err := w.node.ListPeers(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(result.Peers, func(p WorldPeer) bool {
			return p.NodeName == name && p.Online == online && (!online || len(p.Targets) > 0)
		})
	})
}
func (w *testWorldNode) send(t *testing.T, req SendMessageRequest) WorldMessage {
	t.Helper()
	result, err := w.node.SendMessage(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func (w *testWorldNode) waitOutbox(t *testing.T, id string, status WorldMessageStatus) WorldMessage {
	t.Helper()
	var outboundMessage WorldMessage
	awaitWorld(t, func() bool {
		var err error
		outboundMessage, _, err = getRecord[WorldMessage](t.Context(), w.opt.Records, worldMessages, id)
		if err != nil {
			t.Fatal(err)
		}
		return outboundMessage.Status == status
	})
	return outboundMessage
}

func TestWorldNodeRevocation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		connected bool
	}{
		{"connected", true}, {"before reconnect", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			federation := newTestFederation(t)
			node := federation.node(t, "carol", "")
			if tc.connected {
				node.start(t)
				awaitWorld(t, func() bool {
					status, err := node.node.GetStatus(t.Context(), "")
					return err == nil && status.Connected
				})
			}
			if err := federation.control.RevokeNode(t.Context(), node.opt.Identity.NodeID); err != nil {
				t.Fatal(err)
			}
			if !tc.connected {
				node.start(t)
			}
			awaitWorld(t, func() bool {
				status, err := node.node.GetStatus(t.Context(), "")
				return err == nil && status.Revoked && !status.Connected
			})
		})
	}
}

func TestWorldNodeDelivery(t *testing.T) {
	for _, tc := range []struct{ name, direct string }{
		{"relay", ""}, {"direct survives control loss", "self"}, {"direct timeout falls back", "timeout"}, {"offline queue", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 1. 实际注册两台节点，超时场景让 HTTP 握手保持等待。
			direct := tc.direct
			var attempts atomic.Int32
			if direct == "timeout" {
				stall := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { attempts.Add(1); <-r.Context().Done() }))
				t.Cleanup(stall.Close)
				direct = stall.URL
			}
			f := newTestFederation(t)
			a := f.node(t, "alice", "")
			b := f.node(t, "bob", direct)
			a.start(t)
			a.waitPeer(t, "bob", false)
			// 2. 离线入队可以在上线后用原标识送达。
			req := SendMessageRequest{To: "bob/assistant", Message: "first", Wait: true}
			var sent WorldMessage
			if tc.name == "offline queue" {
				sent = a.send(t, req)
				if sent.Status != WorldMessageStatusPendingOffline {
					t.Fatalf("offline: %+v", sent)
				}
			}
			b.start(t)
			a.waitPeer(t, "bob", true)
			b.waitPeer(t, "alice", true)
			if sent.MessageID == "" {
				req.Wait = false
				sent = a.send(t, req)
			}
			got := a.waitOutbox(t, sent.MessageID, WorldMessageStatusReplied)
			if got.ReplyBody != "reply-1" || b.model.calls.Load() != 1 || got.HopLimit != 5 {
				t.Fatalf("first: %+v calls=%d", got, b.model.calls.Load())
			}
			if tc.direct == "timeout" && attempts.Load() == 0 {
				t.Fatal("no direct attempt")
			}
			// 3. 关闭控制面之后，在原通道继续同一线程。
			if tc.direct == "self" {
				f.stop()
				awaitWorld(t, func() bool {
					status, err := a.node.GetStatus(t.Context(), "")
					return err == nil && !status.Connected
				})
				got = a.send(t, SendMessageRequest{To: req.To, ThreadID: sent.ThreadID, Message: "again", Wait: true})
				if got.ReplyBody != "reply-2" || b.model.calls.Load() != 2 {
					t.Fatalf("direct after control loss: %+v", got)
				}
			}
		})
	}
}

func testDialDirect(t *testing.T, local, remote *testWorldNode) SecureChannel {
	t.Helper()
	// 1. 实际握手后由测试端控制信封次序。
	socket, _, err := websocket.Dial(t.Context(), remote.url+"/world/v1/direct?purpose=world&target="+base64.RawURLEncoding.EncodeToString(remote.opt.Identity.SignPublic),
		&websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://client.example"}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	secure, err := DialSecureChannel(ctx, &directFrameTransport{socket}, &local.opt.Identity, remote.opt.Identity.SignPublic, ChannelPurposeWorld)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { secure.Close() })
	return secure
}
func writeWireMessage(t *testing.T, ch SecureChannel, msg wireMessage) {
	t.Helper()
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if err = ch.WriteMessage(t.Context(), data); err != nil {
		t.Fatal(err)
	}
}
func readWireMessage(t *testing.T, ch SecureChannel) wireMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	data, err := ch.ReadMessage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return decodeWorld[wireMessage](t, data)
}

func TestWorldInboundThread(t *testing.T) {
	for _, tc := range []struct {
		name, target string
		hops         int
		failure      string
	}{
		{"serialized and duplicate", "", 5, ""}, {"exhausted", "", 0, "world_hop_limit_exceeded"}, {"closed target", "private", 2, "world_target_not_exposed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 1. 从已注册的对端通过真实直连投递信封。
			f := newTestFederation(t)
			a := f.node(t, "alice", "")
			b := f.node(t, "bob", "")
			b.model.release = make(chan struct{}, 2)
			b.start(t)
			b.waitPeer(t, "alice", false)
			ch := testDialDirect(t, a, b)
			m := wireMessage{Kind: "message", MessageID: "one", ThreadID: "thread", From: Address{Node: "alice", Domain: testDomain}, To: Address{Node: "bob", Domain: testDomain, Target: tc.target}, Body: "first", HopLimit: tc.hops}
			writeWireMessage(t, ch, m)
			ack := readWireMessage(t, ch)
			if tc.failure != "" {
				if ack.Status != "failed" || ack.Body != tc.failure || b.model.calls.Load() != 0 {
					t.Fatalf("invalid: %+v", ack)
				}
				return
			}
			if ack.Status != "accepted" {
				t.Fatalf("first ack %+v", ack)
			}
			awaitWorld(t, func() bool { return b.model.calls.Load() == 1 })
			// 2. 再发重复包和同线程的下一条；第二条只进入收件箱。
			writeWireMessage(t, ch, m)
			if got := readWireMessage(t, ch); got.Status != "running" {
				t.Fatalf("duplicate: %+v", got)
			}
			second := m
			second.MessageID = "two"
			second.Body = "second"
			writeWireMessage(t, ch, second)
			if got := readWireMessage(t, ch); got.Status != "accepted" {
				t.Fatalf("second: %+v", got)
			}
			queued, _, err := getRecord[WorldMessage](t.Context(), b.opt.Records, worldMessages, "two")
			if err != nil || queued.Status != "accepted" || b.model.calls.Load() != 1 {
				t.Fatalf("queue: %+v / %v", queued, err)
			}
			// 3. 分别释放两轮；已完成的重复包带回原回复。
			for i, id := range []string{"one", "two"} {
				b.model.release <- struct{}{}
				got := readWireMessage(t, ch)
				if got.Kind != "reply" || got.InReplyTo != id || got.Body != fmt.Sprintf("reply-%d", i+1) {
					t.Fatalf("reply: %+v", got)
				}
			}
			writeWireMessage(t, ch, m)
			if got := readWireMessage(t, ch); got.Status != "completed" || got.Body != "reply-1" || b.model.calls.Load() != 2 {
				t.Fatalf("completed duplicate %+v", got)
			}
			threadBinding, _, err := getRecord[ThreadBinding](t.Context(), b.opt.Records, worldThreads, "thread")
			if err != nil {
				t.Fatal(err)
			}
			conv, err := b.rt.GetConversation(t.Context(), &runtime.GetConversationRequest{ConversationID: defs.ConversationID(threadBinding.ConversationID)})
			if err != nil || conv.Title != "first" {
				t.Fatalf("conversation title %q, %v", conv.Title, err)
			}
		})
	}
}

// replaceControlConnection 以相同身份重新认证，使运行中的节点重连并核对发件箱。
func replaceControlConnection(t *testing.T, w *testWorldNode) {
	t.Helper()
	// 1. 等待控制面确认新的 epoch，再断开临时连接。
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	url, _ := GetWorldSocketURL(w.opt.ServerURL, "/world/v1/connect")
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	var envelope worldserver.ControlEnvelope
	if err := wsjson.Read(ctx, conn, &envelope); err != nil {
		t.Fatal(err)
	}
	challenge := decodeWorld[worldserver.ChallengeMessage](t, envelope.Payload)
	response := worldserver.ChallengeResponse{PublicKey: w.opt.Identity.SignPublic, NodeID: w.opt.Identity.NodeID, Timestamp: time.Now().Unix()}
	response.Signature, _ = w.opt.Identity.Sign(worldserver.ChallengeSignBytes(worldserver.ChallengeConnect, challenge.Challenge, response.NodeID, response.Timestamp))
	if err := writeControl(ctx, conn, worldserver.ControlTypeChallengeResponse, response); err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Read(ctx, conn, &envelope); err != nil || envelope.Type != worldserver.ControlTypeRelayAttached && envelope.Type != worldserver.ControlTypeDirectory {
		t.Fatalf("replacement authentication: %+v %v", envelope, err)
	}
}

func TestWorldAsyncReplyWakesConversation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		active bool
	}{
		{name: "steers active turn", active: true},
		{name: "starts turn when idle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 1. 订阅来源对话；活跃场景先让原输入进入模型，保持本轮可接收补充。
			w := newTestFederation(t).node(t, "alice", "")
			release := make(chan struct{})
			w.model.release = release
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			conv, err := w.rt.CreateConversation(ctx, &runtime.CreateConversationRequest{AgentName: "assistant"})
			if err != nil {
				t.Fatal(err)
			}
			sub, err := w.rt.SubscribeSession(ctx, &runtime.SubscribeSessionRequest{ConversationID: conv.ID, SessionID: conv.RootSessionID})
			if err != nil {
				t.Fatal(err)
			}
			var openingID defs.ItemID
			if tc.active {
				openingID, err = w.rt.SubmitInput(ctx, worldInput(conv.ID, "opening", "initial input"))
				if err != nil {
					t.Fatal(err)
				}
				awaitWorld(t, func() bool { return w.model.calls.Load() == 1 })
			}

			// 2. 两次从同一条待通知记录恢复，模拟通知成功后未能持久化清除标记。
			reply := WorldMessage{
				MessageID:          string(defs.NewItemID()),
				ConversationID:     string(conv.ID),
				Direction:          WorldMessageDirectionOutbound,
				Kind:               "message",
				PeerAddr:           "bob@" + testDomain,
				Status:             WorldMessageStatusReplied,
				ReplyBody:          "delayed reply",
				NotifyConversation: true,
			}
			for range 2 {
				if err := putRecord(ctx, w.opt.Records, worldMessages, reply.MessageID, time.Now().UnixMilli(), reply); err != nil {
					t.Fatal(err)
				}
				w.node, err = NewWorldNode(w.opt)
				if err != nil {
					t.Fatal(err)
				}
				w.start(t)
				awaitWorld(t, func() bool {
					record, found, err := getRecord[WorldMessage](ctx, w.opt.Records, worldMessages, reply.MessageID)
					return err == nil && found && !record.NotifyConversation
				})
				w.stop()
			}
			close(release)

			// 3. 在真实 Turn 完成事件中核对开场输入和唯一的回信输入。
			var finished *session.SessionTurnFinished
			for finished == nil {
				select {
				case event, ok := <-sub.Events:
					if !ok {
						t.Fatal("source session subscription closed")
					}
					if e, ok := event.(*conversation.ConversationSessionEvent); ok {
						finished, _ = e.Event.(*session.SessionTurnFinished)
					}
				case <-ctx.Done():
					t.Fatal("source turn did not finish:", ctx.Err())
				}
			}
			if finished.Err != nil || finished.Status != defs.TurnStatusCompleted {
				t.Fatalf("source turn ended with %q: %v", finished.Status, finished.Err)
			}
			items, err := w.rt.ListItems(ctx, &runtime.ListItemsRequest{ConversationID: conv.ID, SessionID: conv.RootSessionID, Limit: 100})
			if err != nil {
				t.Fatal(err)
			}
			var replyIDs []defs.ItemID
			for _, item := range items.Items {
				if messageText(item.Item) == "[回信] "+reply.PeerAddr+"\n"+reply.ReplyBody {
					replyIDs = append(replyIDs, item.Item.ID)
				}
			}
			if len(replyIDs) != 1 {
				t.Fatalf("source history has %d reply inputs, want one", len(replyIDs))
			}
			want := replyIDs
			if tc.active {
				want = []defs.ItemID{openingID, replyIDs[0]}
			}
			if !slices.Equal(finished.InputItemIDs, want) {
				t.Fatalf("first turn inputs = %v, want %v", finished.InputItemIDs, want)
			}
		})
	}
}

func TestWorldNodeReconciliation(t *testing.T) {
	for _, tc := range []struct {
		name              string
		accepted, missing bool
	}{
		{"lost ack", false, false}, {"lost reply", true, false}, {"not found resends original id", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 1. 可控对端同样通过通讯录与 SecureChannel 验签，可逐条断开或重放回信。
			sockets := make(chan *websocket.Conn, 8)
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				socket, err := websocket.Accept(w, r, nil)
				if err == nil {
					sockets <- socket
				}
			}))
			t.Cleanup(endpoint.Close)
			f := newTestFederation(t)
			a := f.node(t, "alice", "")
			b := f.node(t, "bob", endpoint.URL)
			a.start(t)
			b.start(t)
			a.waitPeer(t, "bob", true)
			accept := func() SecureChannel {
				t.Helper()
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				select {
				case socket := <-sockets:
					ch, err := AcceptSecureChannel(ctx, &directFrameTransport{socket}, b.opt.Identity)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { ch.Channel.Close() })
					return ch.Channel
				case <-ctx.Done():
					t.Fatal("expected peer connection")
					return nil
				}
			}
			conv, err := a.rt.CreateConversation(t.Context(), &runtime.CreateConversationRequest{AgentName: "assistant"})
			if err != nil {
				t.Fatal(err)
			}
			tools, err := a.node.GetTools(t.Context(), conv.ID)
			if err != nil {
				t.Fatal(err)
			}
			result := tools[1].Execute(t.Context(), testWorldCall("send_world_message", map[string]any{"to": "bob", "message": "work", "wait": false}), nil)
			if result.Result.Error != nil {
				t.Fatal(result.Result.Error)
			}
			ch := accept()
			original := readWireMessage(t, ch)
			if original.Kind != "message" {
				t.Fatalf("original: %+v", original)
			}
			ack := wireMessage{Kind: "ack", MessageID: "ack", InReplyTo: original.MessageID, ThreadID: original.ThreadID, From: original.To, To: original.From, HopLimit: 1, Status: "accepted"}
			if tc.accepted {
				writeWireMessage(t, ch, ack)
				a.waitOutbox(t, original.MessageID, WorldMessageStatusRemoteRunning)
			}
			ch.Close()
			if !tc.accepted {
				a.waitOutbox(t, original.MessageID, WorldMessageStatusUnknown)
			}
			// 2. 重新认证必须先查原标识；仅 not_found 时允许原信重投。
			replaceControlConnection(t, a)
			ch = accept()
			query := readWireMessage(t, ch)
			if query.Kind != "status_query" || query.InReplyTo != original.MessageID {
				t.Fatalf("recovery first frame: %+v", query)
			}
			if tc.missing {
				ack.Status = "not_found"
				writeWireMessage(t, ch, ack)
				if resent := readWireMessage(t, ch); resent != original {
					t.Fatalf("resend: %+v, original: %+v", resent, original)
				}
			}
			// 3. 完成确认、迟到回信与重复包唤醒同一对话一次。
			ack.Status = "completed"
			ack.Body = "result"
			writeWireMessage(t, ch, ack)
			reply := ack
			reply.Kind = "reply"
			reply.MessageID = "answer"
			for range 2 {
				writeWireMessage(t, ch, reply)
				if got := readWireMessage(t, ch); got.Kind != "ack" || got.Status != "completed" {
					t.Fatalf("reply receipt: %+v", got)
				}
			}
			got := a.waitOutbox(t, original.MessageID, WorldMessageStatusReplied)
			if got.ReplyBody != "result" {
				t.Fatalf("result: %+v", got)
			}
			awaitWorld(t, func() bool {
				outboundMessage, _, err := getRecord[WorldMessage](t.Context(), a.opt.Records, worldMessages, original.MessageID)
				return err == nil && !outboundMessage.NotifyConversation && a.model.calls.Load() == 1
			})
			items, err := a.rt.ListItems(t.Context(), &runtime.ListItemsRequest{ConversationID: conv.ID, SessionID: conv.RootSessionID, Limit: 100})
			if err != nil {
				t.Fatal(err)
			}
			wakes := 0
			for _, item := range items.Items {
				if strings.HasPrefix(messageText(item.Item), "[回信]") {
					wakes++
				}
			}
			if wakes != 1 {
				t.Fatalf("received %d conversation wake inputs", wakes)
			}
		})
	}
}

func TestWorldDirectDirectoryWithdrawal(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rules []worldserver.ACLRule
	}{
		{"remove contact", []worldserver.ACLRule{{From: []string{"*"}, To: []string{"*"}, Action: "deny"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 1. 保持直连打开，再通过真实管理接口移除通讯录授权。
			f := newTestFederation(t)
			a := f.node(t, "alice", "")
			b := f.node(t, "bob", "")
			b.start(t)
			b.waitPeer(t, "alice", false)
			ch := testDialDirect(t, a, b)
			writeWireMessage(t, ch, wireMessage{Kind: "status_query", MessageID: "query", InReplyTo: "missing", ThreadID: "thread", From: Address{Node: "alice", Domain: testDomain}, To: Address{Node: "bob", Domain: testDomain}})
			if got := readWireMessage(t, ch); got.Status != "not_found" {
				t.Fatal(got)
			}
			worldHTTP(t, "PUT", f.url+"/world/v1/admin/acl", tc.rules, 204)
			// 2. 原通道立即结束，之后不能再沿用之前的授权。
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			_, err := ch.ReadMessage(ctx)
			if err == nil || ctx.Err() != nil {
				t.Fatalf("removed contact channel: %v", err)
			}
			visible, err := b.node.ListPeers(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if len(visible.Peers) != 0 {
				t.Fatalf("directory %+v", visible)
			}
		})
	}
}

func TestWorldDeliveryAcrossBrokenConnection(t *testing.T) {
	for _, tc := range []struct {
		name    string
		dropAck bool
		want    WorldMessageStatus
	}{
		{name: "disconnect before ack", dropAck: true, want: WorldMessageStatusUnknown},
		{name: "disconnect before reply", want: WorldMessageStatusRemoteRunning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 1. 透明转发两端的真实密文帧，仅在第一次连接上丢弃确认或回信。
			var destination atomic.Value
			var connections atomic.Int32
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				client, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer client.CloseNow()
				ctx, cancel := context.WithCancel(r.Context())
				defer cancel()

				peer, _, err := websocket.Dial(ctx, destination.Load().(string)+r.URL.RequestURI(), nil)
				if err != nil {
					return
				}
				defer peer.CloseNow()
				first := connections.Add(1) == 1
				done := make(chan struct{})
				go func() {
					defer close(done)
					defer cancel()
					for {
						kind, frame, err := client.Read(ctx)
						if err != nil || peer.Write(ctx, kind, frame) != nil {
							return
						}
					}
				}()
				for i := 1; ; i++ {
					kind, frame, err := peer.Read(ctx)
					if err != nil {
						break
					}
					if first && i == 2 && tc.dropAck {
						continue
					}
					if first && i == 3 {
						break
					}
					if client.Write(ctx, kind, frame) != nil {
						break
					}
				}
				cancel()
				<-done
			}))
			t.Cleanup(proxy.Close)
			f := newTestFederation(t)
			a := f.node(t, "alice", "")
			b := f.node(t, "bob", proxy.URL)
			destination.Store(b.url)
			a.start(t)
			b.start(t)
			a.waitPeer(t, "bob", true)
			// 2. 发件方在指定断点得到可观测的状态，收件方已经实际完成一次 Turn。
			conv, err := a.rt.CreateConversation(t.Context(), &runtime.CreateConversationRequest{AgentName: "assistant"})
			if err != nil {
				t.Fatal(err)
			}
			tools, err := a.node.GetTools(t.Context(), conv.ID)
			if err != nil {
				t.Fatal(err)
			}
			sent := tools[1].Execute(t.Context(), testWorldCall("send_world_message", map[string]any{"to": "bob", "message": "run once", "wait": false}), nil)
			if sent.Result.Error != nil {
				t.Fatal(sent.Result.Error)
			}
			id := decodeWorld[map[string]string](t, sent.Result.Content)["message_id"]
			a.waitOutbox(t, id, tc.want)
			awaitWorld(t, func() bool {
				inboundMessage, _, err := getRecord[WorldMessage](t.Context(), b.opt.Records, worldMessages, id)
				return err == nil && inboundMessage.Status == "completed"
			})
			replaceControlConnection(t, a)
			// 3. 对端收件记录和本机回信一致；真实模型执行与来源对话唤醒都恰好一次。
			got := a.waitOutbox(t, id, WorldMessageStatusReplied)
			if got.ReplyBody != "reply-1" || b.model.calls.Load() != 1 {
				t.Fatalf("remote executions %d, result %+v", b.model.calls.Load(), got)
			}
			awaitWorld(t, func() bool {
				outboundMessage, _, err := getRecord[WorldMessage](t.Context(), a.opt.Records, worldMessages, id)
				return err == nil && !outboundMessage.NotifyConversation && a.model.calls.Load() == 1
			})
		})
	}
}

func TestWorldMessageIDConflicts(t *testing.T) {
	for _, tc := range []struct {
		name      string
		kind      string
		direction WorldMessageDirection
	}{
		{"incoming message shares outgoing ID", "message", WorldMessageDirectionOutbound},
		{"reply shares incoming message ID", "reply", WorldMessageDirectionInbound},
		{"reply shares outgoing message ID", "reply", WorldMessageDirectionOutbound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTestFederation(t)
			a, b := f.node(t, "alice", ""), f.node(t, "bob", "")
			from := Address{Node: "alice", Domain: testDomain, Target: "assistant"}
			to := Address{Node: "bob", Domain: testDomain}
			original := WorldMessage{
				MessageID: "original", ThreadID: "thread", Direction: WorldMessageDirectionOutbound,
				Kind: "message", PeerAddr: from.String(), Status: WorldMessageStatusPendingOffline,
			}
			existing := WorldMessage{
				MessageID: "collision", ThreadID: original.ThreadID, Direction: tc.direction,
				Kind: "message", PeerAddr: from.String(), Status: WorldMessageStatusCompleted, Body: "keep original",
			}
			if tc.direction == WorldMessageDirectionOutbound {
				existing.Status = WorldMessageStatusReplied
			}
			for _, message := range []WorldMessage{original, existing} {
				if err := putRecord(t.Context(), b.opt.Records, worldMessages, message.MessageID, 1, message); err != nil {
					t.Fatal(err)
				}
			}
			b.start(t)
			b.waitPeer(t, "alice", false)
			ch := testDialDirect(t, a, b)
			// 1. 状态查询的响应证明前一个冲突信封已经过节点循环。
			writeWireMessage(t, ch, wireMessage{
				Kind: tc.kind, MessageID: existing.MessageID, InReplyTo: original.MessageID,
				ThreadID: original.ThreadID, From: from, To: to, HopLimit: 1, Body: "conflicting payload",
			})
			writeWireMessage(t, ch, wireMessage{
				Kind: "status_query", MessageID: "query", InReplyTo: original.MessageID,
				ThreadID: original.ThreadID, From: from, To: to, HopLimit: 1,
			})
			if got := readWireMessage(t, ch); got.Kind != "ack" || got.InReplyTo != original.MessageID || got.Status != "not_found" {
				t.Fatalf("collision should not acknowledge or change outgoing status: %+v", got)
			}
			for _, want := range []WorldMessage{original, existing} {
				got, found, err := getRecord[WorldMessage](t.Context(), b.opt.Records, worldMessages, want.MessageID)
				if err != nil || !found || got != want {
					t.Fatalf("message = %+v, found %v, err %v, want %+v", got, found, err, want)
				}
			}
			if b.model.calls.Load() != 0 {
				t.Fatal("collision started an inbound turn")
			}
		})
	}
}

func TestWorldReplyReceiptAfterReconnect(t *testing.T) {
	for _, tc := range []struct{ name, target string }{
		{"node address", ""},
		{"full target address", "assistant"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTestFederation(t)
			a, b := f.node(t, "alice", ""), f.node(t, "bob", "")
			from := Address{Node: "alice", Domain: testDomain, Target: tc.target}
			to := Address{Node: "bob", Domain: testDomain}
			original := WorldMessage{
				MessageID: "original", ThreadID: "thread", Direction: WorldMessageDirectionOutbound,
				Kind: "message", PeerAddr: from.String(), Status: WorldMessageStatusRemoteRunning,
			}
			if err := putRecord(t.Context(), b.opt.Records, worldMessages, original.MessageID, 1, original); err != nil {
				t.Fatal(err)
			}
			b.start(t)
			b.waitPeer(t, "alice", false)
			ch := testDialDirect(t, a, b)
			reply := wireMessage{
				Kind: "reply", MessageID: "reply", InReplyTo: original.MessageID, ThreadID: original.ThreadID,
				From: from, To: to, HopLimit: 1, Body: "reply body",
			}
			writeWireMessage(t, ch, reply)
			awaitWorld(t, func() bool {
				receipt, found, err := getRecord[WorldMessage](t.Context(), b.opt.Records, worldMessages, reply.MessageID)
				return err == nil && found && receipt.Direction == WorldMessageDirectionInbound &&
					receipt.Kind == "reply" && receipt.PeerAddr == from.String() && receipt.InReplyTo == original.MessageID &&
					receipt.Body == reply.Body && receipt.Status == WorldMessageStatusCompleted
			})
			// 1. 不读取回信确认，模拟对端丢失 ACK 后重新认证并查询。
			ch.Close()
			ch = testDialDirect(t, a, b)
			writeWireMessage(t, ch, wireMessage{
				Kind: "status_query", MessageID: "query", InReplyTo: reply.MessageID,
				ThreadID: reply.ThreadID, From: from, To: to, HopLimit: 1,
			})
			if got := readWireMessage(t, ch); got.Kind != "ack" || got.InReplyTo != reply.MessageID || got.Status != "completed" {
				t.Fatalf("recovered reply ACK: %+v", got)
			}
			if got := b.waitOutbox(t, original.MessageID, WorldMessageStatusReplied); got.ReplyBody != reply.Body {
				t.Fatalf("outgoing result = %+v", got)
			}
		})
	}
}

func TestWorldHomeRelayAttachment(t *testing.T) {
	for _, tc := range []struct{ name, mode string }{
		{"duplicate configured address", "domain"}, {"replace control connection", "domain"}, {"home relay disabled", "off"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTestFederation(t, tc.mode)
			alice := f.register(t, "alice")
			base, _ := url.Parse(f.url)
			upstreamProxy := httputil.NewSingleHostReverseProxy(base)
			var connects, attaches, discoveries, notices, unexpected atomic.Int32
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/world/v1/relay/instances":
					discoveries.Add(1)
				case "/world/v1/relay/attach":
					attaches.Add(1)
				case "/world/v1/connect":
					connects.Add(1)
					ctx, cancel := context.WithCancel(r.Context())
					defer cancel()
					upstream, _, err := websocket.Dial(ctx, base.String()+r.URL.Path, nil)
					if err != nil {
						return
					}
					defer upstream.CloseNow()
					node, err := websocket.Accept(w, r, nil)
					if err != nil {
						return
					}
					defer node.CloseNow()
					copyControl := func(from, to *websocket.Conn) {
						defer cancel()
						for {
							kind, data, err := from.Read(ctx)
							if err != nil {
								return
							}
							var frame worldserver.ControlEnvelope
							if kind != websocket.MessageText || json.Unmarshal(data, &frame) != nil {
								unexpected.Add(1)
								return
							}
							switch frame.Type {
							case worldserver.ControlTypeRelayIncoming:
								notices.Add(1)
							case worldserver.ControlTypeRelayAttached, worldserver.ControlTypeChallenge, worldserver.ControlTypeChallengeResponse, worldserver.ControlTypeDirectory, worldserver.ControlTypeNodeAnnounce, worldserver.ControlTypePing, worldserver.ControlTypePong:
							default:
								unexpected.Add(1)
							}
							if to.Write(ctx, kind, data) != nil {
								return
							}
						}
					}
					done := make(chan struct{})
					go func() { defer close(done); copyControl(node, upstream) }()
					copyControl(upstream, node)
					<-done
					return
				}
				upstreamProxy.ServeHTTP(w, r)
			}))
			t.Cleanup(proxy.Close)
			f.url = proxy.URL
			bob := f.node(t, "bob", "", strings.Replace(proxy.URL, "http:", "ws:", 1)+"/", proxy.URL)
			bob.start(t)
			bob.waitPeer(t, "alice", false)
			if connects.Load() != 1 || attaches.Load() != 0 {
				t.Fatalf("connect=%d attach=%d", connects.Load(), attaches.Load())
			}
			if tc.name == "replace control connection" {
				replaceControlConnection(t, bob)
				awaitWorld(t, func() bool {
					status, err := bob.node.GetStatus(t.Context(), "")
					return err == nil && connects.Load() >= 3 && status.Connected
				})
			}
			awaitWorld(t, func() bool {
				status, err := bob.node.GetStatus(t.Context(), "")
				return err == nil && status.Connected && len(status.RelayAttachments) == 1 && status.RelayAttachments[0].Connected == (tc.mode != "off")
			})
			if discoveries.Load() != 0 {
				t.Fatalf("home control connection fetched relay instances %d times", discoveries.Load())
			}
			if tc.mode == "off" {
				return
			}
			ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
			defer cancel()
			channel, err := new(PeerDialer).DialPeer(ctx, "bob@"+testDomain, bob.opt.Identity.SignPublic, worldserver.PeerEntries{Relays: []string{proxy.URL}}, &alice, ChannelPurposeWorld)
			if err != nil {
				t.Fatal(err)
			}
			defer channel.Close()
			query := wireMessage{Kind: "status_query", MessageID: "query", ThreadID: "q", InReplyTo: "missing", From: Address{Node: "alice", Domain: testDomain}, To: Address{Node: "bob", Domain: testDomain}}
			writeWireMessage(t, channel, query)
			if msg := readWireMessage(t, channel); msg.Status != "not_found" || notices.Load() != 1 || unexpected.Load() != 0 || attaches.Load() != 0 {
				t.Fatalf("reply=%+v incoming=%d unexpected=%d attach=%d", msg, notices.Load(), unexpected.Load(), attaches.Load())
			}
		})
	}
}

func TestWorldReceiveACL(t *testing.T) {
	for _, route := range []string{"direct", "home relay"} {
		t.Run(route, func(t *testing.T) {
			f := newTestFederation(t)
			a := f.node(t, "alice", "")
			b := f.node(t, "bob", "self")
			rules := []worldserver.ACLRule{{From: []string{"node:alice"}, To: []string{"node:bob"}, Action: "deny"}, {From: []string{"node:bob"}, To: []string{"node:alice"}, Action: "allow"}}
			worldHTTP(t, "PUT", f.url+"/world/v1/admin/acl", rules, 204)
			a.start(t)
			b.start(t)
			b.waitPeer(t, "alice", true)
			req := SendMessageRequest{To: "bob", Message: "blocked"}
			want := fmt.Sprintf("sender %q is not allowed to message this node", "alice@"+testDomain)
			awaitWorld(t, func() bool {
				_, err := a.node.SendMessage(t.Context(), req)
				return ErrorCode(err) == "world_sender_not_allowed" && err.Error() == want
			})
			entries := worldserver.PeerEntries{Relays: []string{f.url}}
			if route == "direct" {
				entries = worldserver.PeerEntries{DirectURLs: []string{b.url}}
			}
			ch, err := new(PeerDialer).DialPeer(t.Context(), "bob@"+testDomain, b.opt.Identity.SignPublic, entries, &a.opt.Identity, ChannelPurposeWorld)
			if err != nil {
				t.Fatal(err)
			}
			defer ch.Close()
			writeWireMessage(t, ch, wireMessage{Kind: "message", MessageID: "denied", ThreadID: "thread", Body: "blocked", From: Address{Node: "alice", Domain: testDomain}, To: Address{Node: "bob", Domain: testDomain}, HopLimit: 3})
			if ack := readWireMessage(t, ch); ack.Status != "failed" || ack.Body != want {
				t.Fatal(ack)
			}
			page, err := b.node.ListMessages(t.Context(), ListMessagesParams{})
			if err != nil || len(page.Items) != 0 {
				t.Fatalf("inbox: %+v %v", page, err)
			}
		})
	}
}

func TestWorldInboundChannelProof(t *testing.T) {
	for _, scenario := range []string{"first encrypted message", "silent after ready", "silent before hello", "delayed relay accept"} {
		t.Run(scenario, func(t *testing.T) {
			f := newTestFederation(t)
			if scenario == "delayed relay accept" {
				target, _ := url.Parse(f.url)
				upstream := httputil.NewSingleHostReverseProxy(target)
				proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/world/v1/relay/accept" {
						time.Sleep(3 * time.Second)
					}
					upstream.ServeHTTP(w, r)
				}))
				t.Cleanup(proxy.Close)
				f.url = proxy.URL
			}
			a := f.node(t, "alice", "")
			b := f.node(t, "bob", "")
			b.start(t)
			b.waitPeer(t, "alice", false)
			ctx, cancel := context.WithTimeout(t.Context(), 7*time.Second)
			defer cancel()
			if scenario == "delayed relay accept" {
				ch, err := new(PeerDialer).DialPeer(ctx, "bob@"+testDomain, b.opt.Identity.SignPublic, worldserver.PeerEntries{Relays: []string{f.url}}, &a.opt.Identity, ChannelPurposeWorld)
				if err != nil {
					t.Fatal(err)
				}
				defer ch.Close()
				if _, err := ch.ReadMessage(ctx); err == nil || ctx.Err() != nil {
					t.Fatalf("idle first message after relay accept: %v", err)
				}
				return
			}
			if scenario == "silent before hello" {
				socket, _, err := websocket.Dial(ctx, b.url+"/world/v1/direct?purpose=world&target="+base64.RawURLEncoding.EncodeToString(b.opt.Identity.SignPublic), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer socket.CloseNow()
				if _, _, err = socket.Read(ctx); err == nil || ctx.Err() != nil {
					t.Fatalf("idle hello: %v", err)
				}
				return
			}
			ch := testDialDirect(t, a, b)
			if scenario == "silent after ready" {
				if _, err := ch.ReadMessage(ctx); err == nil || ctx.Err() != nil {
					t.Fatalf("idle first message: %v", err)
				}
				return
			}
			// 没有控制连接的发起方在首条密文到达前仍然离线。
			got := b.send(t, SendMessageRequest{To: "alice", Message: "keep queued"})
			if got.Status != WorldMessageStatusPendingOffline {
				t.Fatalf("unproven channel used for delivery: %s", got.Status)
			}
			writeWireMessage(t, ch, wireMessage{Kind: "status_query", MessageID: "q", ThreadID: "t", InReplyTo: "missing", From: Address{Node: "alice", Domain: testDomain}, To: Address{Node: "bob", Domain: testDomain}})
			if reply := readWireMessage(t, ch); reply.Status != "not_found" {
				t.Fatalf("first message: %+v", reply)
			}
			peers, err := b.node.ListPeers(ctx)
			if err != nil || !slices.ContainsFunc(peers.Peers, func(p WorldPeer) bool { return p.NodeName == "alice" && p.Online }) {
				t.Fatalf("proven peer: %+v %v", peers, err)
			}
		})
	}
}

func TestWorldDialAfterControlDisconnect(t *testing.T) {
	for _, targetOnline := range []bool{true, false} {
		t.Run(fmt.Sprintf("target_online_%t", targetOnline), func(t *testing.T) {
			f := newTestFederation(t)
			b := f.node(t, "bob", "")
			target, _ := url.Parse(f.url)
			upstream := httputil.NewSingleHostReverseProxy(target)
			control, cancel := context.WithCancel(t.Context())
			defer cancel()
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/world/v1/connect" {
					if control.Err() != nil {
						http.Error(w, "offline", 503)
						return
					}
					ctx, stop := context.WithCancel(r.Context())
					defer stop()
					unbind := context.AfterFunc(control, stop)
					defer unbind()
					r = r.WithContext(ctx)
				}
				upstream.ServeHTTP(w, r)
			}))
			t.Cleanup(proxy.Close)
			f.url = proxy.URL
			a := f.node(t, "alice", "")
			a.start(t)
			b.start(t)
			a.waitPeer(t, "bob", true)
			b.waitPeer(t, "alice", true)
			cancel()
			awaitWorld(t, func() bool { s, err := a.node.GetStatus(t.Context(), ""); return err == nil && !s.Connected })
			if !targetOnline {
				b.stop()
			}
			msg := a.send(t, SendMessageRequest{To: "bob", Message: "first contact while disconnected", Wait: true})
			if targetOnline && msg.ReplyBody != "reply-1" || !targetOnline && msg.Status != WorldMessageStatusPendingOffline {
				t.Fatal(msg)
			}
		})
	}
}

func TestWorldIdentityCacheAndTrust(t *testing.T) {
	f := newTestFederation(t)
	alice := f.node(t, "alice", "")
	bob := f.node(t, "bob", "self")
	peer := worldserver.PeerDomain{Domain: "other.home", PublicKey: alice.opt.Identity.BoxPublic, Entries: worldserver.PeerEntries{Relays: []string{f.url}}}
	card := func(p worldserver.PeerDomain) worldserver.AddDomainRequest {
		data, _ := json.Marshal(p)
		return worldserver.AddDomainRequest{Card: p.Domain + "#" + base64.RawURLEncoding.EncodeToString(data)}
	}
	key := memberCacheKey{peer.Domain, string(peer.PublicKey), "alice"}
	n := bob.node.(*worldNodeImpl)
	now := time.Now()
	n.memberCache.Store(key, cachedMember{worldserver.DomainMember{PublicKey: alice.opt.Identity.SignPublic, Owner: "team"}, now.Add(time.Hour)})
	url := f.url + "/world/v1/admin/domains"
	worldHTTP(t, "POST", url, card(peer), 201)
	if _, err := f.control.ReceiveTrust(t.Context(), peer.PublicKey, worldserver.DomainTrustMessage{Op: "trust_response", Domain: peer.Domain, Decision: "accepted"}); err != nil {
		t.Fatal(err)
	}
	bob.start(t)
	bob.waitPeer(t, "alice", false)
	query := func() string {
		t.Helper()
		ch := testDialDirect(t, alice, bob)
		defer ch.Close()
		writeWireMessage(t, ch, wireMessage{Kind: "message", MessageID: "check", ThreadID: "trust", From: Address{Node: "alice", Domain: peer.Domain}, To: Address{Node: "bob", Domain: testDomain}, HopLimit: 1, Body: "test"})
		return readWireMessage(t, ch).ErrorCode
	}
	if got := query(); got != "world_target_not_public" {
		t.Fatalf("cached identity: %s", got)
	}
	for _, tc := range []struct {
		name, code string
		change     func()
	}{
		{"deleted", "world_domain_not_trusted", func() { worldHTTP(t, "DELETE", url+"/"+peer.Domain, nil, 204) }},
		{"replaced", "world_identity_unverified", func() {
			peer.PublicKey = bob.opt.Identity.BoxPublic
			worldHTTP(t, "POST", url, card(peer), 201)
			if _, err := f.control.ReceiveTrust(t.Context(), peer.PublicKey, worldserver.DomainTrustMessage{Op: "trust_response", Domain: peer.Domain, Decision: "accepted"}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.change()
			awaitWorld(t, func() bool { return query() == tc.code })
		})
	}
}

func TestWorldMessagePermissions(t *testing.T) {
	f := newTestFederation(t)
	a, b := f.node(t, "alice", ""), f.node(t, "bob", "self")
	trusted := worldserver.PeerDomain{Domain: "remote.test", PublicKey: b.opt.Identity.BoxPublic, Entries: worldserver.PeerEntries{Relays: []string{f.url}}}
	data, _ := json.Marshal(trusted)
	worldHTTP(t, "POST", f.url+"/world/v1/admin/domains", worldserver.AddDomainRequest{Card: trusted.Domain + "#" + base64.RawURLEncoding.EncodeToString(data)}, 201)
	if _, err := f.control.ReceiveTrust(t.Context(), trusted.PublicKey, worldserver.DomainTrustMessage{Op: "trust_response", Domain: trusted.Domain, Decision: "accepted"}); err != nil {
		t.Fatal(err)
	}
	n := b.node.(*worldNodeImpl)
	n.opt.AdvertiseTargets = append(n.opt.AdvertiseTargets, worldserver.AdvertisedTarget{Name: "public", Visibility: "public"})
	n.memberCache.Store(memberCacheKey{trusted.Domain, string(trusted.PublicKey), "valid"}, cachedMember{worldserver.DomainMember{PublicKey: a.opt.Identity.SignPublic}, time.Now().Add(time.Hour)})
	b.start(t)
	b.waitPeer(t, "alice", false)
	for _, tc := range []struct {
		from       Address
		code, text string
	}{
		{Address{Node: "unknown", Domain: testDomain}, "world_sender_not_allowed", `sender "unknown@world.example" is not allowed to message this node`},
		{Address{Node: "alice", Domain: "third.test"}, "world_domain_not_trusted", `domain "third.test" is not trusted`},
		{Address{Node: "unknown", Domain: trusted.Domain}, "world_identity_unverified", `cannot verify "unknown@remote.test": its domain control plane is unreachable`},
		{Address{Node: "valid", Domain: trusted.Domain}, "world_target_not_public", `target "assistant" does not accept messages from other domains`},
	} {
		t.Run(tc.code, func(t *testing.T) {
			ch := testDialDirect(t, a, b)
			defer ch.Close()
			writeWireMessage(t, ch, wireMessage{Kind: "message", MessageID: "m", ThreadID: "t", From: tc.from, To: Address{Node: "bob", Domain: testDomain}, HopLimit: 1, Body: "blocked"})
			ack := readWireMessage(t, ch)
			if ack.Status != "failed" || ack.ErrorCode != tc.code || ack.Body != tc.text {
				t.Fatalf("ack = %+v", ack)
			}
		})
	}
	from := Address{Node: "valid", Domain: trusted.Domain}
	for _, tc := range []struct {
		name, boundPeer, outboundPeer, target, status, code string
	}{
		{"local initiation", "", from.NodeAddress() + "/agent", "", "accepted", ""},
		{"bound peer", from.String(), "", "", "accepted", ""},
		{"different bound peer", "other@remote.test", "", "", "failed", "world_target_not_public"},
		{"different outbound peer", "", "valid@elsewhere.test", "", "failed", "world_target_not_public"},
		{"unknown thread", "", "", "", "failed", "world_target_not_public"},
		{"public target", "", "", "public", "accepted", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			thread := string(defs.NewItemID())
			if tc.boundPeer != "" {
				binding := ThreadBinding{ThreadID: thread, PeerAddr: tc.boundPeer, Target: b.opt.DefaultTarget, Direction: "inbound"}
				if err := putRecord(t.Context(), b.opt.Records, worldThreads, thread, 1, binding); err != nil {
					t.Fatal(err)
				}
			}
			if tc.outboundPeer != "" {
				if err := putRecord(t.Context(), b.opt.Records, worldThreads, thread, 1, ThreadBinding{ThreadID: thread, PeerAddr: tc.outboundPeer, Direction: "outbound"}); err != nil {
					t.Fatal(err)
				}
			}
			ch := testDialDirect(t, a, b)
			msg := wireMessage{Kind: "message", MessageID: string(defs.NewItemID()), ThreadID: thread, From: from, To: Address{Node: "bob", Domain: testDomain, Target: tc.target}, Body: "manual message", HopLimit: 5}
			writeWireMessage(t, ch, msg)
			if ack := readWireMessage(t, ch); ack.InReplyTo != msg.MessageID || ack.Status != tc.status || ack.ErrorCode != tc.code {
				t.Fatalf("ack = %+v", ack)
			}
		})
	}
	_, err := b.node.SendMessage(t.Context(), SendMessageRequest{To: "missing", Message: "hello"})
	if ErrorCode(err) != "world_peer_not_found" || err.Error() != `world peer "missing@world.example" is not in directory` {
		t.Fatal(err)
	}
	_, err = new(PeerDialer).DialPeer(t.Context(), "alice@remote.test", a.opt.Identity.SignPublic, worldserver.PeerEntries{}, &b.opt.Identity, ChannelPurposeWorld)
	if ErrorCode(err) != "world_target_unreachable" || err.Error() != "cannot reach alice@remote.test: direct addresses and all relays are unreachable" {
		t.Fatal(err)
	}
}

// 本域控制连接保留在拨号入口，页面从显式配置的公开入口和直连地址中选择。
func TestWorldRemoteAccessPage(t *testing.T) {
	for _, tc := range []struct{ name, relay, direct, want string }{
		{"skip domain control plane", "https://relay.example", "", "https://relay.example"},
		{"prefer direct over local relay", "http://localhost", "https://direct.example", "https://direct.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newTestFederation(t, "domain").node(t, "studio", tc.direct, tc.relay)
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- w.node.Run(ctx, NewRuntimeHost(w.rt), discardRemoteRPC{}) }()
			defer func() { cancel(); <-done }()
			status, err := w.node.GetStatus(ctx, "runtime-token")
			if err != nil || !strings.HasPrefix(status.RemoteAccessURL, tc.want+"/web/v1/") {
				t.Fatalf("remote page for %s: %v", tc.name, err)
			}
		})
	}
}

type discardRemoteRPC struct{}

func (discardRemoteRPC) ServeWorldRPC(_ context.Context, channel SecureChannel) error {
	channel.Close()
	return nil
}
