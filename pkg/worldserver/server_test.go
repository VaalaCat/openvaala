package worldserver

import (
	"bytes"
	"cmp"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/gin-gonic/gin"
)

const (
	testDomain     = "world.example.com"
	testAdminToken = "test-admin-token"
)

func TestWorldServerRegistration(t *testing.T) {
	server := serveWorldForTest(t)
	for _, tt := range []struct {
		name   string
		bearer string
		status int
	}{
		{
			name:   "missing token",
			status: http.StatusUnauthorized,
		},
		{
			name:   "wrong token",
			bearer: "invalid",
			status: http.StatusUnauthorized,
		},
		{
			name:   "valid token",
			bearer: testAdminToken,
			status: http.StatusOK,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// 1. 验证管理面只接受配置的凭据。
			worldHTTP(t, server, http.MethodGet, "/admin/tokens", tt.bearer, nil, tt.status)
		})
	}

	// 1. 签发入网码并验证错误签名未消耗入网码。
	token := createWorldToken(t, server, "alice", []string{"trusted"})
	request, _ := signedWorldRegistration(t, "office", token.Token)
	request.NodeName = "tampered"
	worldHTTP(t, server, http.MethodPost, "/nodes/register", "", request, http.StatusUnauthorized)

	// 2. 登记原始节点并检查归属和入网码复用拒绝。
	request.NodeName = "office"
	registration := decodeWorldJSON[RegisterNodeResponse](t,
		worldHTTP(t, server, http.MethodPost, "/nodes/register", "", request, http.StatusCreated))
	if registration.Domain != testDomain || registration.Owner != "alice" ||
		registration.NodeName != "office" || !slices.Equal(registration.Tags, []string{"trusted"}) ||
		registration.NodeID != GetNodeIDFromSignPublic(request.SignPublic) {
		t.Fatalf("unexpected registration: %+v", registration)
	}
	second, _ := signedWorldRegistration(t, "laptop", token.Token)
	worldHTTP(t, server, http.MethodPost, "/nodes/register", "", second, http.StatusUnauthorized)

	// 3. 检查列表隐藏入网明文并验证管理面吊销。
	tokens := worldHTTP(t, server, http.MethodGet, "/admin/tokens", testAdminToken, nil, http.StatusOK)
	list := decodeWorldJSON[[]JoinTokenInfo](t, tokens)
	if len(list) != 1 || list[0].UsedCount != 1 || bytes.Contains(tokens, []byte(token.Token)) {
		t.Fatalf("unexpected token list: %s", tokens)
	}
	unused := createWorldToken(t, server, "alice", nil)
	worldHTTP(t, server, http.MethodDelete, "/admin/tokens/"+unused.ID, testAdminToken, nil, http.StatusNoContent)
	request, _ = signedWorldRegistration(t, "laptop", unused.Token)
	worldHTTP(t, server, http.MethodPost, "/nodes/register", "", request, http.StatusUnauthorized)
}

func TestWorldServerDirectory(t *testing.T) {
	// 1. 连接不同归属的节点，检查默认 ACL。
	server := serveWorldForTest(t)
	officeID, officeKey := registerWorldNode(t, server, "office", "alice", []string{"trusted"})
	homeID, homeKey := registerWorldNode(t, server, "home", "bob", nil)
	office := connectWorldNode(t, server, officeID, officeKey)
	home := connectWorldNode(t, server, homeID, homeKey)
	directory := receiveWorldPayload[Directory](t, office, ControlTypeDirectory)
	if len(directory.Nodes) != 1 || directory.Nodes[0].NodeID != officeID {
		t.Fatalf("default ACL directory: %+v", directory)
	}
	receiveWorldPayload[Directory](t, home, ControlTypeDirectory)
	// 2. 替换 ACL，检查通讯录的两个授权方向。
	rules := []ACLRule{{
		From:   []string{"tag:trusted"},
		To:     []string{"node:home"},
		Action: "allow",
	}}
	worldHTTP(t, server, http.MethodPut, "/admin/acl", testAdminToken, rules, http.StatusNoContent)
	stored := decodeWorldJSON[[]ACLRule](t,
		worldHTTP(t, server, http.MethodGet, "/admin/acl", testAdminToken, nil, http.StatusOK))
	if len(stored) != 1 || stored[0].Action != "allow" {
		t.Fatalf("unexpected ACL: %+v", stored)
	}
	announcement := NodeAnnounce{
		Entries: PeerEntries{DirectURLs: []string{"ws://home.example.com/world/v1/direct"}},
		Targets: []AdvertisedTarget{{
			Name:        "ops",
			Description: "Operations",
			Visibility:  "domain",
		}},
	}
	writeWorldControl(t, home, ControlTypeNodeAnnounce, announcement)
	waitForWorldNode(t, office, homeID, func(node DirectoryNode) bool {
		return node.Online && node.CanSend && !node.CanReceive && slices.Equal(node.Entries.DirectURLs, announcement.Entries.DirectURLs) &&
			slices.Equal(node.Targets, announcement.Targets)
	})

	waitForWorldNode(t, home, officeID, func(node DirectoryNode) bool { return !node.CanSend && node.CanReceive })
	if err := home.Close(websocket.StatusNormalClosure, "offline"); err != nil {
		t.Fatal(err)
	}
	waitForWorldNode(t, office, homeID, func(node DirectoryNode) bool { return !node.Online })

}

func TestWorldServerReconnectAndRevoke(t *testing.T) {
	for _, tt := range []struct {
		name        string
		revokeRoute func(nodeID string) string
	}{
		{
			name:        "by node id",
			revokeRoute: func(nodeID string) string { return "/admin/nodes/" + nodeID + "/revoke" },
		},
		{
			name:        "by owner",
			revokeRoute: func(_ string) string { return "/admin/owners/alice/revoke" },
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// 1. 让同一身份重连，确认旧连接关闭且新连接仍能宣告。
			server := serveWorldForTest(t)
			nodeID, private := registerWorldNode(t, server, "office", "alice", nil)
			old := connectWorldNode(t, server, nodeID, private)
			writeWorldControl(t, old, ControlTypePing, nil)
			readWorldControl(t, old, ControlTypePong)
			next := connectWorldNode(t, server, nodeID, private)
			expectWorldSocketClose(t, old, websocket.StatusPolicyViolation)

			announcement := NodeAnnounce{Targets: []AdvertisedTarget{{
				Name:       "ops",
				Visibility: "public",
			}}}
			writeWorldControl(t, next, ControlTypeNodeAnnounce, announcement)
			waitForWorldNode(t, next, nodeID, func(node DirectoryNode) bool {
				return node.Online && slices.Equal(node.Targets, announcement.Targets)
			})

			// 2. 吊销节点，验证通知、断线和拒绝重新连接。
			worldHTTP(t, server, http.MethodPost, tt.revokeRoute(nodeID), testAdminToken, nil, http.StatusNoContent)
			revoked := receiveWorldPayload[NodeRevoked](t, next, ControlTypeNodeRevoked)
			if revoked.NodeID != nodeID {
				t.Fatalf("revoked node: %+v", revoked)
			}
			expectWorldSocketClose(t, next, -1)
			rejected := connectWorldNode(t, server, nodeID, private)
			if got := receiveWorldPayload[NodeRevoked](t, rejected, ControlTypeNodeRevoked); got.Code != "world_node_revoked" {
				t.Fatal(got)
			}
			expectWorldSocketClose(t, rejected, websocket.StatusPolicyViolation)
			nodes := decodeWorldJSON[[]NodeRecord](t,
				worldHTTP(t, server, http.MethodGet, "/admin/nodes", testAdminToken, nil, http.StatusOK))
			if len(nodes) != 1 || nodes[0].NodeID != nodeID || nodes[0].RevokedAt == 0 {
				t.Fatalf("unexpected revoked node list: %+v", nodes)
			}
		})
	}
}

func TestWorldServerAdminNodes(t *testing.T) {
	server := serveWorldForTest(t)
	nodeID, key := registerWorldNode(t, server, "office", "alice", []string{"team"})
	var socket *websocket.Conn
	announcement := NodeAnnounce{Targets: []AdvertisedTarget{{Name: "assistant", Visibility: "domain"}}}
	for _, tc := range []struct {
		name    string
		change  func()
		online  bool
		revoked bool
		targets []AdvertisedTarget
	}{
		{name: "registered offline"},
		{name: "online announcement", online: true, targets: announcement.Targets, change: func() {
			socket = connectWorldNode(t, server, nodeID, key)
			writeWorldControl(t, socket, ControlTypeNodeAnnounce, announcement)
			waitForWorldNode(t, socket, nodeID, func(node DirectoryNode) bool {
				return slices.Equal(node.Targets, announcement.Targets)
			})
		}},
		{name: "revoked remains listed", revoked: true, change: func() {
			worldHTTP(t, server, http.MethodPost, "/admin/nodes/"+nodeID+"/revoke", testAdminToken, nil, http.StatusNoContent)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.change != nil {
				tc.change()
			}
			nodes := decodeWorldJSON[[]AdminNode](t,
				worldHTTP(t, server, http.MethodGet, "/admin/nodes", testAdminToken, nil, http.StatusOK))
			if len(nodes) != 1 {
				t.Fatalf("nodes = %+v", nodes)
			}
			node := nodes[0]
			if node.NodeID != nodeID || node.Domain != testDomain || node.Online != tc.online ||
				(node.RevokedAt != 0) != tc.revoked || node.Targets == nil || !slices.Equal(node.Targets, tc.targets) {
				t.Fatalf("unexpected admin node: %+v", node)
			}
		})
	}
}

func TestWorldServerRejectsBadChallenge(t *testing.T) {
	server := serveWorldForTest(t)
	nodeID, private := registerWorldNode(t, server, "office", "alice", nil)
	for _, tt := range []struct {
		name      string
		challenge func([]byte) []byte
		timestamp func() int64
	}{
		{
			name:      "signature from a different challenge",
			challenge: func(_ []byte) []byte { return make([]byte, 32) },
			timestamp: func() int64 { return time.Now().Unix() },
		},
		{
			name:      "expired signature",
			challenge: func(challenge []byte) []byte { return challenge },
			timestamp: func() int64 { return time.Now().Add(-time.Hour).Unix() },
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// 1. 回复错误或过期的证明并检查拒绝结果。
			socket := dialWorldSocket(t, server)
			challenge := receiveWorldPayload[ChallengeMessage](t, socket, ControlTypeChallenge)
			sendWorldProof(t, socket, ChallengeConnect, tt.challenge(challenge.Challenge), nodeID, private, tt.timestamp())
			expectWorldSocketClose(t, socket, websocket.StatusPolicyViolation)
		})
	}
}

func serveWorldForTest(t *testing.T) string {
	t.Helper()
	base, _ := serveWorldDirectoryForTest(t)
	return base
}
func serveWorldDirectoryForTest(t *testing.T, directURLs ...string) (string, WorldServer) {
	t.Helper()
	// 1. 装配真实路由并运行控制面。
	opt := NewWorldServerOpt(testDomain, filepath.Join(t.TempDir(), "worldserver", "world.db"), testAdminToken)
	router := gin.New()
	server := httptest.NewUnstartedServer(router)
	opt.LocalRelayURL = "http://" + server.Listener.Addr().String()
	opt.Entries.DirectURLs = directURLs
	world, err := NewWorldServer(opt)
	if err != nil {
		t.Fatal(err)
	}
	world.RegisterRoutes(router)
	server.Start()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- world.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		defer server.Close()
		select {
		case err := <-done:
			if err != context.Canceled {
				t.Errorf("Run returned %v", err)
			}
		case <-time.After(6 * time.Second):
			t.Error("Run did not stop")
		}
	})

	// 2. 等待健康检查可用。
	wait, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	for wait.Err() == nil {
		req, _ := http.NewRequestWithContext(wait, http.MethodGet, server.URL+"/world/v1/healthz", nil)
		if resp, err := server.Client().Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return server.URL, world
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("world health endpoint did not become ready")
	return "", nil
}

func worldHTTP(t *testing.T, server, method, path, bearer string, payload any, status int) []byte {
	t.Helper()
	// 1. 调用控制面 HTTP 路由并核对状态。
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(ctx, method, server+"/world/v1"+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != status {
		t.Fatalf("%s %s returned %d, want %d: %s (%v)", method, path, response.StatusCode, status, body, err)
	}
	return body
}

func decodeWorldJSON[T any](t *testing.T, data []byte) T {
	t.Helper()
	// 1. 解码 HTTP 或 WebSocket 的 JSON 结果。
	var result T
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func createWorldToken(t *testing.T, server, owner string, tags []string) CreateJoinTokenResponse {
	t.Helper()
	// 1. 通过管理路由签发入网码。
	request := CreateJoinTokenRequest{
		Owner:     owner,
		Tags:      tags,
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	}
	return decodeWorldJSON[CreateJoinTokenResponse](t,
		worldHTTP(t, server, http.MethodPost, "/admin/tokens", testAdminToken, request, http.StatusCreated))
}

func signedWorldRegistration(t *testing.T, name, token string) (RegisterNodeRequest, ed25519.PrivateKey) {
	t.Helper()
	// 1. 生成节点密钥并签名入网请求。
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	request := RegisterNodeRequest{
		NodeName:   name,
		JoinToken:  token,
		SignPublic: public,
		BoxPublic:  make([]byte, 32),
	}
	rand.Read(request.BoxPublic)
	request.Signature = ed25519.Sign(private, RegisterSignBytes(request))
	return request, private
}

func registerWorldNode(t *testing.T, server, name, owner string, tags []string) (string, ed25519.PrivateKey) {
	t.Helper()
	// 1. 发起入网并返回节点自身的身份。
	token := createWorldToken(t, server, owner, tags)
	request, private := signedWorldRegistration(t, name, token.Token)
	response := decodeWorldJSON[RegisterNodeResponse](t,
		worldHTTP(t, server, http.MethodPost, "/nodes/register", "", request, http.StatusCreated))
	return response.NodeID, private
}

func dialWorldSocket(t *testing.T, server string) *websocket.Conn {
	t.Helper()
	// 1. 建立控制面 WebSocket。
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	socket, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server, "http")+"/world/v1/connect", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = socket.CloseNow() })
	return socket
}

func connectWorldNode(t *testing.T, server, nodeID string, private ed25519.PrivateKey) *websocket.Conn {
	t.Helper()
	// 1. 连接并用节点身份签署服务端挑战。
	socket := dialWorldSocket(t, server)
	challenge := receiveWorldPayload[ChallengeMessage](t, socket, ControlTypeChallenge)
	if len(challenge.Challenge) != 32 {
		t.Fatalf("challenge length = %d", len(challenge.Challenge))
	}
	sendWorldProof(t, socket, ChallengeConnect, challenge.Challenge, nodeID, private, time.Now().Unix())
	return socket
}

func sendWorldProof(t *testing.T, socket *websocket.Conn, purpose ChallengePurpose, challenge []byte, nodeID string, private ed25519.PrivateKey, timestamp int64) {
	t.Helper()
	// 1. 使用指定的节点、挑战和时间戳发送签名。
	proof := ChallengeResponse{PublicKey: private.Public().(ed25519.PublicKey),
		NodeID:    nodeID,
		Timestamp: timestamp,
		Signature: ed25519.Sign(private, ChallengeSignBytes(purpose, challenge, nodeID, timestamp)),
	}
	writeWorldControl(t, socket, ControlTypeChallengeResponse, proof)
}

func writeWorldControl(t *testing.T, socket *websocket.Conn, messageType string, payload any) {
	t.Helper()
	// 1. 编码并发送测试消息。
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := writeControl(ctx, socket, messageType, payload); err != nil {
		t.Fatal(err)
	}
}

func readWorldControl(t *testing.T, socket *websocket.Conn, messageType string) ControlEnvelope {
	t.Helper()
	// 1. 跳过可独立到达的通讯录更新，读取待验证的消息。
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		var envelope ControlEnvelope
		if err := wsjson.Read(ctx, socket, &envelope); err != nil {
			t.Fatalf("read %s: %v", messageType, err)
		}
		if envelope.Type == messageType {
			return envelope
		}
		if envelope.Type != ControlTypeDirectory && envelope.Type != ControlTypeRelayAttached {
			t.Fatalf("received %s, want %s", envelope.Type, messageType)
		}
	}
}

func receiveWorldPayload[T any](t *testing.T, socket *websocket.Conn, messageType string) T {
	t.Helper()
	// 1. 读取并解码指定类型的控制面消息。
	return decodeWorldJSON[T](t, readWorldControl(t, socket, messageType).Payload)
}

func waitForWorldNode(t *testing.T, socket *websocket.Conn, nodeID string, predicate func(DirectoryNode) bool) {
	t.Helper()
	// 1. 在通讯录更新中等待目标节点出现期望属性。
	for range 8 {
		directory := receiveWorldPayload[Directory](t, socket, ControlTypeDirectory)
		for _, node := range directory.Nodes {
			if node.NodeID == nodeID && predicate(node) {
				return
			}
		}
	}
	t.Fatalf("expected directory entry for %s", nodeID)
}

func expectWorldSocketClose(t *testing.T, socket *websocket.Conn, status websocket.StatusCode) {
	t.Helper()
	// 1. 检查连接关闭及可用的 WebSocket 关闭状态。
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, _, err := socket.Read(ctx)
	if err == nil || ctx.Err() != nil || websocket.CloseStatus(err) != status {
		t.Fatalf("socket close = %v, want status %d", err, status)
	}
}

func TestWorldRelayPairLifecycle(t *testing.T) {
	server := serveWorldForTest(t)
	nodeID, key := registerWorldNode(t, server, "target", "relay-user", nil)
	home := connectWorldNode(t, server, nodeID, key)
	receiveWorldPayload[Directory](t, home, ControlTypeDirectory)
	open := func(t *testing.T, base, path string) *websocket.Conn {
		t.Helper()
		var options *websocket.DialOptions
		if strings.HasPrefix(path, "dial?") {
			options = &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://client.example"}}}
		}
		conn, _, err := websocket.Dial(t.Context(), base+"/world/v1/relay/"+path, options)
		if err != nil {
			t.Fatal(err)
		}
		conn.SetReadLimit(ChannelFrameLimit)
		t.Cleanup(func() { conn.CloseNow() })
		return conn
	}
	dialPath := "dial?target=" + relayKey(key.Public().(ed25519.PublicKey))
	for _, scenario := range []string{"not attached", "unknown id", "dial left", "late accept", "duplicate accept", "accept closes", "dial closes", "frame limit"} {
		t.Run(scenario, func(t *testing.T) {
			switch scenario {
			case "not attached":
				expectWorldSocketClose(t, open(t, server, "dial?target=absent"), 4004)
				return
			case "unknown id":
				expectWorldSocketClose(t, open(t, server, "accept?connection_id=absent"), 4004)
				return
			}
			dial := open(t, server, dialPath)
			incoming := receiveWorldPayload[RelayIncoming](t, home, ControlTypeRelayIncoming)
			id, err := base64.RawURLEncoding.DecodeString(incoming.ConnectionID)
			if err != nil || len(id) != 16 {
				t.Fatal(incoming)
			}
			acceptPath := "accept?connection_id=" + incoming.ConnectionID
			switch scenario {
			case "late accept":
				ctx, cancel := context.WithTimeout(t.Context(), 13*time.Second)
				defer cancel()
				_, _, err := dial.Read(ctx)
				var closeErr websocket.CloseError
				if !errors.As(err, &closeErr) || closeErr.Code != 4004 || closeErr.Reason != "world_target_unreachable" {
					t.Fatal(err)
				}
				expectWorldSocketClose(t, open(t, server, acceptPath), 4004)
				return
			case "dial left":
				dial.Close(websocket.StatusNormalClosure, "left")
				conn := open(t, server, acceptPath)
				ctx, stop := context.WithTimeout(t.Context(), 3*time.Second)
				defer stop()
				if _, _, err := conn.Read(ctx); err == nil || ctx.Err() != nil {
					t.Fatalf("accept after dial left: %v", err)
				}
				expectWorldSocketClose(t, open(t, server, acceptPath), 4004)
				return
			}
			accept := open(t, server, acceptPath)
			payload := []byte("binary handshake")
			if scenario == "frame limit" {
				payload = bytes.Repeat([]byte{0xa5}, ChannelFrameLimit)
			}
			for _, pair := range [][2]*websocket.Conn{{dial, accept}, {accept, dial}} {
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				if err := pair[0].Write(ctx, websocket.MessageBinary, payload); err != nil {
					cancel()
					t.Fatal(err)
				}
				kind, got, err := pair[1].Read(ctx)
				cancel()
				if err != nil || kind != websocket.MessageBinary || !bytes.Equal(got, payload) {
					t.Fatalf("forwarded %d bytes, %v", len(got), err)
				}
			}
			if scenario == "duplicate accept" {
				expectWorldSocketClose(t, open(t, server, acceptPath), 4004)
			}
			if scenario == "accept closes" {
				dial, accept = accept, dial
			}
			dial.CloseNow()
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			if _, _, err := accept.Read(ctx); err == nil || ctx.Err() != nil {
				t.Fatalf("other end: %v", err)
			}
		})
	}
	public := NewRelay(nil)
	router := gin.New()
	public.RegisterRoutes(router)
	publicHTTP := httptest.NewServer(router)
	defer publicHTTP.Close()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); public.Run(ctx) }()
	defer func() { cancel(); <-done }()
	for _, tc := range []struct {
		name, base          string
		registered, allowed bool
	}{
		{"external domain", server, false, false}, {"domain member", server, true, true}, {"public key", publicHTTP.URL, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, private := nodeID, key
			if !tc.registered {
				pub, k, err := ed25519.GenerateKey(rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				id, private = GetNodeIDFromSignPublic(pub), k
			}
			conn := open(t, tc.base, "attach")
			challenge := receiveWorldPayload[ChallengeMessage](t, conn, ControlTypeChallenge)
			sendWorldProof(t, conn, ChallengeRelayAttach, challenge.Challenge, id, private, time.Now().Unix())
			if !tc.allowed {
				expectWorldSocketClose(t, conn, websocket.StatusPolicyViolation)
				return
			}
			readWorldControl(t, conn, ControlTypeRelayAttached)
			path := "dial?target=" + relayKey(private.Public().(ed25519.PublicKey))
			dial := open(t, tc.base, path)
			incoming := receiveWorldPayload[RelayIncoming](t, conn, ControlTypeRelayIncoming)
			if incoming.ConnectionID == "" {
				t.Fatal("missing relay notification")
			}
			accept := open(t, tc.base, "accept?connection_id="+incoming.ConnectionID)
			if tc.registered {
				worldHTTP(t, server, http.MethodPost, "/admin/nodes/"+id+"/revoke", testAdminToken, nil, http.StatusNoContent)
				expectWorldSocketClose(t, open(t, server, path), 4004)
				reconnect := open(t, server, "attach")
				challenge := receiveWorldPayload[ChallengeMessage](t, reconnect, ControlTypeChallenge)
				sendWorldProof(t, reconnect, ChallengeRelayAttach, challenge.Challenge, id, private, time.Now().Unix())
				expectWorldSocketClose(t, reconnect, websocket.StatusPolicyViolation)
			} else {
				expectWorldSocketClose(t, open(t, server, path), 4004)
				cancel()
				<-done
				check, stop := context.WithTimeout(t.Context(), 3*time.Second)
				defer stop()
				for _, end := range []*websocket.Conn{dial, accept} {
					if _, _, err := end.Read(check); err == nil || check.Err() != nil {
						t.Fatalf("shutdown: %v", err)
					}
				}
			}
		})
	}
}

func TestWorldChallengeEndpoints(t *testing.T) {
	server := serveWorldForTest(t)
	id, key := registerWorldNode(t, server, "node", "team", nil)
	for _, tc := range []struct {
		path  string
		proof ChallengePurpose
	}{
		{"connect", ChallengeRelayAttach}, {"relay/attach", ChallengeConnect},
	} {
		t.Run(tc.path, func(t *testing.T) {
			socket, _, err := websocket.Dial(t.Context(), server+"/world/v1/"+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer socket.CloseNow()
			challenge := receiveWorldPayload[ChallengeMessage](t, socket, ControlTypeChallenge)
			sendWorldProof(t, socket, tc.proof, challenge.Challenge, id, key, time.Now().Unix())
			expectWorldSocketClose(t, socket, websocket.StatusPolicyViolation)
		})
	}
}

func TestWorldAdminACLMatrix(t *testing.T) {
	base, server := serveWorldDirectoryForTest(t)
	officeID, officeKey := registerWorldNode(t, base, "office", "alice", nil)
	homeID, homeKey := registerWorldNode(t, base, "home", "bob", nil)
	rules := []ACLRule{{Priority: 10, From: []string{"owner:alice"}, To: []string{"owner:bob"}, Action: "allow"}}
	submitted := cloneACL(rules)
	if err := server.ReplaceACL(t.Context(), submitted); err != nil {
		t.Fatal(err)
	}
	// 调用方复用规则和选择器切片后，新连接按已保存的规则接收通讯录。
	submitted[0].From[0] = "owner:bob"
	submitted[0].To[0] = "owner:alice"
	submitted[0].Action = "deny"
	matrix, err := server.GetACLMatrix(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		name   string
		socket *websocket.Conn
		index  int
	}{
		{"home", connectWorldNode(t, base, homeID, homeKey), 0},
		{"office", connectWorldNode(t, base, officeID, officeKey), 1},
	} {
		t.Run(row.name+" after caller reuses ACL", func(t *testing.T) {
			directory := receiveWorldPayload[Directory](t, row.socket, ControlTypeDirectory)
			if len(directory.Nodes) != 2 {
				t.Fatalf("directory = %+v, matrix = %+v", directory, matrix)
			}
			for j, node := range directory.Nodes {
				if matrix.Nodes[j].Name != node.NodeName || node.CanSend != matrix.Cells[row.index][j].Allowed || node.CanReceive != matrix.Cells[j][row.index].Allowed {
					t.Fatalf("directory = %+v, matrix = %+v", directory, matrix)
				}
			}
		})
	}
	for _, tc := range []struct {
		name    string
		rules   []ACLRule
		allowed bool
		index   int
	}{
		{"saved", nil, true, 0},
		{"empty draft", []ACLRule{}, false, -1},
		{"unordered draft", []ACLRule{
			{Priority: 20, From: []string{"*"}, To: []string{"*"}, Action: "allow"},
			{Priority: 10, From: []string{"*"}, To: []string{"*"}, Action: "deny"},
		}, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			preview, err := server.GetACLMatrix(t.Context(), tc.rules)
			if err != nil || preview.Cells[1][0] != (ACLMatrixCell{tc.allowed, tc.index}) {
				t.Fatalf("preview = %+v, err = %v", preview, err)
			}
			saved, err := server.GetACL(t.Context())
			if err != nil || !reflect.DeepEqual(saved, rules) {
				t.Fatalf("saved = %+v, err = %v", saved, err)
			}
		})
	}
	invalid := []ACLRule{{Action: "deny"}}
	_, previewErr := server.GetACLMatrix(t.Context(), invalid)
	saveErr := server.ReplaceACL(t.Context(), invalid)
	if previewErr == nil || saveErr == nil || previewErr.Error() != saveErr.Error() || reflect.TypeOf(previewErr) != reflect.TypeOf(saveErr) {
		t.Fatalf("preview = %v, save = %v", previewErr, saveErr)
	}
	if err := server.RevokeNode(t.Context(), homeID); err != nil {
		t.Fatal(err)
	}
	matrix, err = server.GetACLMatrix(t.Context(), nil)
	if err != nil || len(matrix.Nodes) != 1 || matrix.Nodes[0].Name != "office" || len(matrix.Cells) != 1 || len(matrix.Cells[0]) != 1 {
		t.Fatalf("after revoke = %+v, err = %v", matrix, err)
	}
}

func TestWorldAdminACLRequestBinding(t *testing.T) {
	base := serveWorldForTest(t)
	rules := json.RawMessage(`[{"priority":"10","from":["*"],"to":["*"],"action":"allow"}]`)
	for _, tc := range []struct {
		name, method, path string
		body               json.RawMessage
	}{
		{"save string priority", http.MethodPut, "/admin/acl", rules},
		{"preview string priority", http.MethodPost, "/admin/acl/matrix", json.RawMessage(`{"rules":` + string(rules) + `}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := worldHTTP(t, base, tc.method, tc.path, testAdminToken, tc.body, http.StatusBadRequest)
			result := decodeWorldJSON[map[string]string](t, body)
			if result["error"] != "expected an ACL rule array" {
				t.Fatalf("binding error = %q", result["error"])
			}
		})
	}
}

func TestWorldAdminListUsers(t *testing.T) {
	base, server := serveWorldDirectoryForTest(t)
	id, key := registerWorldNode(t, base, "office", "alice", nil)
	socket := connectWorldNode(t, base, id, key)
	waitForWorldNode(t, socket, id, func(n DirectoryNode) bool { return n.Online })
	createWorldToken(t, base, "alice", nil)
	createWorldToken(t, base, "bob", nil)
	revoked := createWorldToken(t, base, "bob", nil)
	if err := server.RevokeJoinToken(t.Context(), revoked.ID); err != nil {
		t.Fatal(err)
	}
	expired := createWorldToken(t, base, "bob", nil)
	db, err := server.(*worldServerImpl).getAdminDB(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&JoinTokenRecord{}).Where("id = ?", expired.ID).Update("expires_at", time.Now().Add(-time.Hour).UnixMilli()).Error; err != nil {
		t.Fatal(err)
	}
	if err := server.ReplaceACL(t.Context(), []ACLRule{{From: []string{"owner:carol", "owner:*"}, To: []string{"owner:alice", "owner:carol"}, Action: "allow"}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name          string
		revoke        bool
		nodes, online int
	}{
		{"active", false, 1, 1}, {"revoked", true, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.revoke {
				if err := server.RevokeOwner(t.Context(), "alice"); err != nil {
					t.Fatal(err)
				}
			}
			users, err := server.ListUsers(t.Context())
			want := []WorldUser{{"alice", tc.nodes, tc.online, 1, 1}, {"bob", 0, 0, 1, 0}, {"carol", 0, 0, 0, 1}}
			if err != nil || !slices.Equal(users, want) {
				t.Fatalf("users = %+v, want %+v, err = %v", users, want, err)
			}
		})
	}
}

func TestWorldJoinCodeAddress(t *testing.T) {
	for _, tc := range []struct {
		name, directURL, want string
	}{
		{"local listener", "", ""},
		{"advertised HTTPS", "https://control.example/team", "https://control.example/team"},
		{"advertised socket", "wss://control.example/team/world/v1/direct", "https://control.example/team"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var entries []string
			if tc.directURL != "" {
				entries = []string{tc.directURL}
			}
			address, _ := serveWorldDirectoryForTest(t, entries...)
			code := createWorldToken(t, address, "alice", nil)
			base, err := ParseJoinCode(code.Token)
			if err != nil || base != cmp.Or(tc.want, address) {
				t.Fatalf("issued control URL = %q, %v", base, err)
			}
		})
	}
}

func TestWorldJoinCode(t *testing.T) {
	for _, tc := range []struct{ code, base string }{
		{"https://control.test/team#secret", "https://control.test/team"},
		{"http://[::1]:8899#ipv6", "http://[::1]:8899"},
		{"bare-token", ""}, {"https://control.test#", ""}, {"file:///tmp#code", ""},
		{"https://control.test?a=1#secret", ""}, {"https://control.test#one#two", ""},
	} {
		base, err := ParseJoinCode(tc.code)
		if base != tc.base || (err != nil) != (tc.base == "") {
			t.Errorf("parse %q: %q, %v", tc.code, base, err)
		}
	}
}
