package e2e

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VaalaCat/llmkit"
	"github.com/VaalaCat/openvaala/internal/config"
	"github.com/VaalaCat/openvaala/internal/modelcatalog"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/VaalaCat/openvaala/pkg/runtimeserver"
	"github.com/VaalaCat/openvaala/pkg/sessiontoken"
	"github.com/VaalaCat/openvaala/pkg/storage/sqlite"
	"github.com/VaalaCat/openvaala/pkg/world"
	"github.com/VaalaCat/openvaala/pkg/worldserver"
	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
)

const (
	federationDomain = "world.example"
	federationToken  = "federation-e2e"
	federationAPI    = "/api/v1/world"
	federationWait   = 10 * time.Second
)

func TestWorldFederation(t *testing.T) {
	for _, scenario := range []struct {
		name string
		run  func(*testing.T, *federationServer)
	}{
		{"relay_chain", testFederationRelayChain},
		{"hop_limit", testFederationHopLimit},
		{"offline_delivery_and_cancel", testFederationOffline},
		{"reconnect_status_query", testFederationReconnect},
		{"direct_during_control_outage", testFederationDirect},
		{"revoke_node", func(t *testing.T, f *federationServer) { testFederationRevocation(t, f, false) }},
		{"revoke_owner", func(t *testing.T, f *federationServer) { testFederationRevocation(t, f, true) }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newFederationServer(t, "domain")
			scenario.run(t, f)
		})
	}
}

func testFederationRelayChain(t *testing.T, f *federationServer) {
	// 三台节点都不公布直连地址，经控制面中转发信和回信。
	a := f.node(t, "alice", "team", "", false)
	b := f.node(t, "bob", "team", "", false)
	c := f.node(t, "carol", "team", "", false)
	for _, n := range []*federationNode{a, b, c} {
		n.start(t)
	}
	a.waitPeer(t, "bob", true)
	b.waitPeer(t, "carol", true)

	source := a.submit(t, "ask Bob")
	a.model.next(t).send(t, "bob", "ask Carol for the answer", true)
	b.model.next(t).send(t, "carol", "what is the answer?", true)
	c.model.next(t).answer("answer from Carol")
	b.model.next(t).result(t, world.WorldMessageStatusReplied, "answer from Carol").answer("Bob relays the answer from Carol")
	a.model.next(t).result(t, world.WorldMessageStatusReplied, "Bob relays the answer from Carol").answer("received the final reply")

	a.assertTurns(t, source, 1)
	b.assertTurns(t, b.inbound(t, "ask Carol for the answer"), 1)
	c.assertTurns(t, c.inbound(t, "what is the answer?"), 1)
	b.assertOutbox(t, "carol", world.WorldMessageStatusReplied, "answer from Carol", 4)
	a.assertOutbox(t, "bob", world.WorldMessageStatusReplied, "Bob relays the answer from Carol", 5)
	a.model.assertCalls(t, 2)
	b.model.assertCalls(t, 2)
	c.model.assertCalls(t, 1)
}

func testFederationHopLimit(t *testing.T, f *federationServer) {
	a := f.node(t, "alice", "team", "", false)
	b := f.node(t, "bob", "team", "", false)
	a.start(t)
	b.start(t)
	a.waitPeer(t, "bob", true)
	b.waitPeer(t, "alice", true)

	// 两台节点交替转发，各轮等待远端结果，逐跳消耗来信的预算。
	initial := a.send(t, "bob", "relay hop 5", "")
	sender, receiver := b, a
	var forwarding []*federationNode
	for hops := 5; hops > 1; hops-- {
		sender.model.next(t).send(t, receiver.name, fmt.Sprintf("relay hop %d", hops-1), true)
		forwarding = append(forwarding, sender)
		sender, receiver = receiver, sender
	}
	sender.model.next(t).send(t, receiver.name, "must not arrive", true)
	failed := sender.model.next(t)
	if result := failed.toolResult(t); result.Error == nil || result.Error.Code != "world_hop_limit_exceeded" {
		t.Fatalf("exhausted hop limit returned %+v", result)
	}
	const reply = "stopped at hop limit"
	failed.answer(reply)
	for _, n := range slices.Backward(forwarding) {
		n.model.next(t).result(t, world.WorldMessageStatusReplied, reply).answer(reply)
	}
	if got := a.waitOutbox(t, initial.MessageID, world.WorldMessageStatusReplied); got.ReplyBody != reply {
		t.Fatalf("reply from final hop = %+v", got)
	}
	sender.assertTurns(t, sender.inbound(t, "relay hop 1"), 1)
	for _, n := range []*federationNode{a, b} {
		for _, outboundMessage := range n.outbox(t) {
			if outboundMessage.Kind == "message" && outboundMessage.Body != fmt.Sprintf("relay hop %d", outboundMessage.HopLimit) {
				t.Fatalf("%s sent a message with an unexpected hop limit: %+v", n.name, outboundMessage)
			}
		}
	}
}

func testFederationOffline(t *testing.T, f *federationServer) {
	a := f.node(t, "alice", "team", "", false)
	b := f.node(t, "bob", "team", "", false)
	a.start(t)
	a.waitPeer(t, "bob", false)

	source := a.submit(t, "send while Bob is offline")
	a.model.next(t).send(t, "bob", "deliver when online", true)
	a.model.next(t).result(t, world.WorldMessageStatusPendingOffline, "").answer("queued")
	a.assertTurns(t, source, 1)
	queued := a.assertOutbox(t, "bob", world.WorldMessageStatusPendingOffline, "", 5)

	cancelled := a.send(t, "bob", "cancel before Bob starts", "")
	if cancelled.Status != world.WorldMessageStatusPendingOffline {
		t.Fatalf("message to cancel = %+v", cancelled)
	}
	cancelled, err := a.client.CancelWorldMessage(t.Context(), cancelled.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != world.WorldMessageStatusCancelled {
		t.Fatalf("cancelled = %+v", cancelled)
	}

	b.start(t)
	b.model.next(t).answer("Bob is back")
	replied := a.waitOutbox(t, queued.MessageID, world.WorldMessageStatusReplied)
	if replied.ReplyBody != "Bob is back" {
		t.Fatalf("queued message = %+v", replied)
	}
	notification := a.model.next(t)
	notification.userInputContains(t, "[回信] bob@"+federationDomain+"\nBob is back")
	notification.answer("received")
	a.assertTurns(t, source, 2)
	b.assertTurns(t, b.inbound(t, "deliver when online"), 1)
	a.waitOutbox(t, cancelled.MessageID, world.WorldMessageStatusCancelled)
	b.model.assertCalls(t, 1)
}

func testFederationReconnect(t *testing.T, f *federationServer) {
	proxy, ackDropped := observePairDisconnect(t, f.url)
	a := f.node(t, "alice", "team", "", false)
	b := f.node(t, "bob", "team", proxy, false)
	a.start(t)
	b.start(t)
	a.waitPeer(t, "bob", true)

	sent := a.send(t, "bob", "complete with the link down", "")
	inbound := b.model.next(t)
	select {
	case <-ackDropped:
	case <-time.After(federationWait):
		t.Fatal("did not observe the first encrypted ACK")
	}
	f.stop()
	a.waitConnected(t, false)
	b.waitConnected(t, false)
	a.waitOutbox(t, sent.MessageID, world.WorldMessageStatusUnknown)

	inbound.answer("Bob's persisted reply")
	var reply world.WorldMessage
	federationEventually(t, "Bob's reply queued while disconnected", func() bool {
		for _, outboundMessage := range b.outbox(t) {
			if outboundMessage.Kind == "reply" && outboundMessage.InReplyTo == sent.MessageID && outboundMessage.Status == world.WorldMessageStatusPendingOffline {
				reply = outboundMessage
				return true
			}
		}
		return false
	})

	// 控制面恢复后核对原消息并收取已落盘的回信。
	if err := a.client.SubscribeWorld(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.start(t)
	got := readFederationOutboxUpdate(t, a.updates, sent.MessageID)
	if got.Status != world.WorldMessageStatusReplied || got.ReplyBody != reply.Body || got.ReplyBody != "Bob's persisted reply" {
		t.Fatalf("reconciled message = %+v, queued reply = %+v", got, reply)
	}
	b.assertTurns(t, b.inbound(t, "complete with the link down"), 1)
	b.model.assertCalls(t, 1)
}

func testFederationDirect(t *testing.T, f *federationServer) {
	a := f.node(t, "alice", "team", "", true)
	b := f.node(t, "bob", "team", "", true)
	a.start(t)
	b.start(t)
	a.waitPeer(t, "bob", true)
	b.waitPeer(t, "alice", true)
	first := a.send(t, "bob", "establish direct channel", "")
	b.model.next(t).answer("direct reply")
	a.waitOutbox(t, first.MessageID, world.WorldMessageStatusReplied)

	f.stop()
	a.waitConnected(t, false)
	b.waitConnected(t, false)
	second := a.send(t, "bob", "continued without WorldServer", first.ThreadID)
	b.model.next(t).answer("reply during outage")
	if got := a.waitOutbox(t, second.MessageID, world.WorldMessageStatusReplied); got.ReplyBody != "reply during outage" {
		t.Fatalf("direct reply during outage = %+v", got)
	}
	b.assertTurns(t, b.inbound(t, "establish direct channel"), 2)
	b.model.assertCalls(t, 2)

	f.start(t)
	a.waitConnected(t, true)
	b.waitConnected(t, true)
	f.register(t, filepath.Join(t.TempDir(), "identity"), "carol", "team")
	for _, n := range []*federationNode{a, b} {
		n.waitPeer(t, "carol", false)
	}
}

func testFederationRevocation(t *testing.T, f *federationServer, owner bool) {
	federationHTTP(t, http.MethodPut, f.url+"/world/v1/admin/acl", nil, []worldserver.ACLRule{
		{From: []string{"owner:team"}, To: []string{"owner:shared"}, Action: "allow"},
		{From: []string{"owner:shared"}, To: []string{"owner:team"}, Action: "allow"},
	}, http.StatusNoContent)
	a := f.node(t, "alice", "team", "", true)
	b := f.node(t, "bob", "shared", "", true)
	a.start(t)
	b.start(t)
	a.waitPeer(t, "bob", true)
	b.waitPeer(t, "alice", true)
	revoked := []*federationNode{b}
	if owner {
		c := f.node(t, "carol", "shared", "", false)
		c.start(t)
		a.waitPeer(t, "carol", true)
		c.waitPeer(t, "alice", true)
		revoked = append(revoked, c)
	}

	initial := a.send(t, "bob", "open direct channel before revocation", "")
	b.model.next(t).answer("received")
	a.waitOutbox(t, initial.MessageID, world.WorldMessageStatusReplied)

	path := "/nodes/" + b.identity.NodeID + "/revoke"
	if owner {
		path = "/owners/shared/revoke"
	}
	federationHTTP(t, http.MethodPost, f.url+"/world/v1/admin"+path, nil, nil, http.StatusNoContent)
	for _, n := range revoked {
		n.waitConnected(t, false)
		federationEventually(t, n.name+" removed from Alice's directory", func() bool {
			_, found := a.peer(t, n.name)
			return !found
		})
		for _, pair := range [][2]*federationNode{{a, n}, {n, a}} {
			_, err := pair[0].client.SendWorldMessage(t.Context(), world.SendMessageRequest{To: pair[1].name, Message: "must be rejected"})
			var result *runtimeclient.ResponseError
			if !errors.As(err, &result) || result.Data == nil {
				t.Fatalf("send to revoked peer: %v", err)
			}
			code := "world_peer_not_found"
			if pair[0] == n {
				code = "world_node_revoked"
			}
			if result.Data.Code != code {
				t.Fatalf("%s → %s after revocation: %+v", pair[0].name, pair[1].name, result)
			}
		}
	}
}

type federationServer struct {
	relayMode string
	url       string
	dbPath    string
	handler   atomic.Pointer[gin.Engine]
	stop      func()
}

func newFederationServer(t *testing.T, relayMode string) *federationServer {
	t.Helper()
	f := &federationServer{relayMode: relayMode, dbPath: filepath.Join(t.TempDir(), "control", "world.db")}
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.handler.Load().ServeHTTP(w, r)
	}))
	t.Cleanup(endpoint.Close)
	f.url = endpoint.URL
	f.start(t)
	return f
}

func (f *federationServer) start(t *testing.T) {
	t.Helper()
	opt := worldserver.NewWorldServerOpt(federationDomain, f.dbPath, federationToken)
	opt.RelayMode = f.relayMode
	opt.LocalRelayURL = f.url
	server, err := worldserver.NewWorldServer(opt)
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	server.RegisterRoutes(router)
	f.handler.Store(router)
	f.stop = runFederationService(t, "WorldServer", server.Run)
	federationEventually(t, "WorldServer health", func() bool {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url+"/world/v1/healthz", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
}

func (f *federationServer) register(t *testing.T, dir, name, owner string) world.NodeIdentity {
	t.Helper()
	identity, err := world.LoadOrCreateIdentity(filepath.Join(dir, "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	token := federationJSON[worldserver.CreateJoinTokenResponse](t, http.MethodPost, f.url+"/world/v1/admin/tokens",
		worldserver.CreateJoinTokenRequest{Owner: owner, ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}, http.StatusCreated)
	req := worldserver.RegisterNodeRequest{NodeName: name, JoinToken: token.Token, SignPublic: identity.SignPublic, BoxPublic: identity.BoxPublic}
	req.Signature, err = identity.Sign(worldserver.RegisterSignBytes(req))
	if err != nil {
		t.Fatal(err)
	}
	federationHTTP(t, http.MethodPost, f.url+"/world/v1/nodes/register", nil, req, http.StatusCreated)
	return identity
}

type federationNode struct {
	name     string
	url      string
	identity world.NodeIdentity
	model    *federationModel
	node     world.WorldNode
	rt       runtime.Runtime
	client   runtimeclient.Client
	updates  chan world.NodeNotification
}

func (f *federationServer) node(t *testing.T, name, owner, controlURL string, direct bool, relays ...string) *federationNode {
	t.Helper()
	dir := t.TempDir()
	identity := f.register(t, filepath.Join(dir, "identity"), name, owner)
	worldDB, err := sqlite.OpenDatabase(sqlite.NewDatabaseOpt(filepath.Join(dir, "world.db"), world.WorldNamespaces()...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { worldDB.Close() })
	rtDB, err := sqlite.OpenDatabase(sqlite.NewDatabaseOpt(filepath.Join(dir, "runtime.db"), dao.RecordNamespaces()...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rtDB.Close() })
	router := gin.New()
	httpServer := httptest.NewUnstartedServer(router)
	t.Cleanup(httpServer.Close)
	baseURL := "http://" + httpServer.Listener.Addr().String()
	if controlURL == "" {
		controlURL = f.url
	}
	nodeOpt := world.NewWorldNodeOpt(worldDB.Record(), identity, name, federationDomain, controlURL)
	nodeOpt.Relays = relays
	nodeOpt.AdvertiseTargets = []worldserver.AdvertisedTarget{{Name: "assistant", Visibility: "domain"}}
	nodeOpt.DefaultTarget = "assistant"
	nodeOpt.DefaultWait = time.Minute
	if direct {
		nodeOpt.DirectURLs = []string{baseURL}
	}
	node, err := world.NewWorldNode(nodeOpt)
	if err != nil {
		t.Fatal(err)
	}
	model := &federationModel{requests: make(chan federationModelCall, 8)}
	ref := defs.ModelRef{ProviderID: "test", ModelID: "test"}
	rt, err := runtime.NewRuntime(runtime.NewRuntimeOpt(rtDB.Record(), rtDB.Log(), &runtime.Definitions{
		Agents: []*agent.AgentDefinition{{Name: "assistant", Model: ref}},
		Models: []*defs.ModelDefinition{{Ref: ref, Target: &llmkit.Target{
			Protocol: llmkit.ProtocolOpenAIChat, Model: "test", BaseURL: "http://127.0.0.1:1",
		}}},
		ExtraTools: node,
	}).WithModelClient(model))
	if err != nil {
		t.Fatal(err)
	}
	serverOpt := runtimeserver.NewRuntimeServerOpt(rt, federationToken)
	serverOpt.WorldNode = node
	serverOpt.Configuration = config.NewConfigFiles(&config.LoadConfigOpt{Home: dir})
	serverOpt.ModelCatalog, err = modelcatalog.NewCatalog(modelcatalog.NewCatalogOpt(filepath.Join(dir, "models.json")))
	if err != nil {
		t.Fatal(err)
	}
	server, err := runtimeserver.NewRuntimeServer(serverOpt)
	if err != nil {
		t.Fatal(err)
	}
	server.RegisterRoutes(router)
	world.RegisterDirectRoutes(router, node, nil)
	httpServer.Start()

	runFederationService(t, name+" Runtime", rt.Run)
	runFederationService(t, name+" RuntimeServer", server.Run)
	client, err := runtimeclient.NewClient(runtimeclient.NewClientOpt("ws"+strings.TrimPrefix(baseURL, "http")+"/api/v1/openvaala", federationToken))
	if err != nil {
		t.Fatal(err)
	}
	updates := make(chan world.NodeNotification, 32)
	runFederationService(t, name+" Runtime client", func(ctx context.Context) error {
		done := make(chan struct{})
		go func() {
			defer close(done)
			for event := range client.Events() {
				if update, ok := event.(*runtimeclient.WorldMessageUpdated); ok {
					updates <- update.Notification
				}
			}
		}()
		err := client.Run(ctx)
		<-done
		return err
	})
	return &federationNode{name: name, url: baseURL, identity: identity, model: model, node: node, rt: rt, client: client, updates: updates}
}

func (n *federationNode) start(t *testing.T) {
	t.Helper()
	runFederationService(t, n.name+" WorldNode", func(ctx context.Context) error { return n.node.Run(ctx, world.NewRuntimeHost(n.rt), nil) })
	n.waitConnected(t, true)
}

func (n *federationNode) submit(t *testing.T, text string) defs.Conversation {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), federationWait)
	defer cancel()
	conv, err := n.client.CreateConversation(ctx, &runtimeclient.CreateConversationRequest{AgentName: "assistant"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = n.client.SubmitInput(ctx, &runtimeclient.SubmitInputRequest{
		ConversationID: conv.ID,
		Content:        []*defs.Content{{Kind: defs.ContentText, Text: &defs.TextContent{Text: text}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return conv
}

func (n *federationNode) inbound(t *testing.T, title string) defs.Conversation {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), federationWait)
	defer cancel()
	result, err := n.client.ListConversations(ctx, &runtimeclient.ListConversationsRequest{SearchTerm: title})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Conversations) != 1 || result.Conversations[0].Conversation.Title != title {
		t.Fatalf("%s: want one inbound conversation %q, got %+v", n.name, title, result)
	}
	return result.Conversations[0].Conversation
}

func (n *federationNode) assertTurns(t *testing.T, conv defs.Conversation, count int) {
	t.Helper()
	federationEventually(t, fmt.Sprintf("%s: %d completed turns in %q", n.name, count, conv.Title), func() bool {
		ctx, cancel := context.WithTimeout(t.Context(), federationWait)
		defer cancel()
		result, err := n.client.ListTurns(ctx, &runtimeclient.ListTurnsRequest{ConversationID: conv.ID, SessionID: conv.RootSessionID})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Turns) > count {
			t.Fatalf("%s: expected %d turns, got %+v", n.name, count, result.Turns)
		}
		for _, turn := range result.Turns {
			if turn.Status != defs.TurnStatusCompleted {
				return false
			}
		}
		return len(result.Turns) == count
	})
}

func (n *federationNode) waitConnected(t *testing.T, connected bool) {
	t.Helper()
	federationEventually(t, fmt.Sprintf("%s connected=%v", n.name, connected), func() bool {
		result, err := n.client.GetWorldStatus(t.Context())
		return err == nil && result.Connected == connected
	})
}

func (n *federationNode) peer(t *testing.T, name string) (world.WorldPeer, bool) {
	t.Helper()
	result, err := n.client.ListWorldPeers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, peer := range result.Peers {
		if peer.NodeName == name {
			return peer, true
		}
	}
	return world.WorldPeer{}, false
}

func (n *federationNode) waitPeer(t *testing.T, peer string, online bool) {
	t.Helper()
	federationEventually(t, fmt.Sprintf("%s sees %s online=%v", n.name, peer, online), func() bool {
		got, ok := n.peer(t, peer)
		return ok && got.Online == online && (!online || len(got.Targets) > 0)
	})
}

func (n *federationNode) send(t *testing.T, to, message, thread string) world.WorldMessage {
	t.Helper()
	result, err := n.client.SendWorldMessage(t.Context(), world.SendMessageRequest{To: to, Message: message, ThreadID: thread})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func (n *federationNode) outbox(t *testing.T) []world.WorldMessage {
	t.Helper()
	page, err := n.client.ListWorldMessages(t.Context(), world.ListMessagesParams{Direction: "outbound"})
	if err != nil {
		t.Fatal(err)
	}
	return page.Items
}

func (n *federationNode) waitOutbox(t *testing.T, id string, status world.WorldMessageStatus) world.WorldMessage {
	t.Helper()
	var last world.WorldMessage
	t.Cleanup(func() {
		if last.Status != status && t.Failed() {
			t.Logf("%s outbox %s last observed: %+v", n.name, id, last)
		}
	})
	federationEventually(t, fmt.Sprintf("%s outbox %s = %s", n.name, id, status), func() bool {
		for _, outboundMessage := range n.outbox(t) {
			if outboundMessage.MessageID == id {
				last = outboundMessage
				return outboundMessage.Status == status
			}
		}
		return false
	})
	return last
}

// readFederationOutboxUpdate 读取第一条对应的更新，以区分状态核对与进入 sending 的重投。
func readFederationOutboxUpdate(t *testing.T, updates <-chan world.NodeNotification, id string) world.WorldMessage {
	t.Helper()
	deadline := time.After(federationWait)
	for {
		select {
		case event := <-updates:
			if event.Event != "message_updated" {
				continue
			}
			var message world.WorldMessage
			if err := json.Unmarshal(event.Data, &message); err != nil {
				t.Fatal(err)
			}
			if message.MessageID == id {
				return message
			}
		case <-deadline:
			t.Fatalf("did not receive an outbox update for %s", id)
		}
	}
}

func (n *federationNode) assertOutbox(t *testing.T, to string, status world.WorldMessageStatus, reply string, hops int) world.WorldMessage {
	t.Helper()
	var messages []world.WorldMessage
	for _, outboundMessage := range n.outbox(t) {
		if outboundMessage.Kind == "message" && outboundMessage.PeerAddr == to+"@"+federationDomain {
			messages = append(messages, outboundMessage)
		}
	}
	if len(messages) != 1 || messages[0].Status != status || messages[0].ReplyBody != reply || messages[0].HopLimit != hops {
		t.Fatalf("%s → %s: want one message (%s, %q, %d hops), got %+v", n.name, to, status, reply, hops, messages)
	}
	return messages[0]
}

func runFederationService(t *testing.T, name string, run func(context.Context) error) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx) }()
	stop := sync.OnceFunc(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("%s: %v", name, err)
			}
		case <-time.After(federationWait):
			t.Errorf("%s did not stop", name)
		}
	})
	t.Cleanup(stop)
	return stop
}

func federationEventually(t *testing.T, description string, check func() bool) {
	t.Helper()
	timeout := time.NewTimer(federationWait)
	defer timeout.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for !check() {
		select {
		case <-ticker.C:
		case <-timeout.C:
			t.Fatalf("timed out waiting for %s", description)
		}
	}
}

func federationHTTP(t *testing.T, method, endpoint string, headers http.Header, body any, want int) (http.Header, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	ctx, cancel := context.WithTimeout(t.Context(), federationWait)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		t.Fatal(err)
	}
	if headers == nil {
		req.Header.Set("Authorization", "Bearer "+federationToken)
	} else {
		req.Header = headers
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != want {
		t.Fatalf("%s %s: got HTTP %d, want %d: %s", method, endpoint, resp.StatusCode, want, data)
	}
	return resp.Header, data
}

func federationJSON[T any](t *testing.T, method, endpoint string, body any, want int) T {
	t.Helper()
	_, data := federationHTTP(t, method, endpoint, nil, body, want)
	var result T
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("%s %s: %v: %s", method, endpoint, err, data)
	}
	return result
}

// federationModel 把每次推理交给场景放行，真实 Agent 负责执行工具及继续模型调用。
type federationModel struct {
	calls    atomic.Int32
	requests chan federationModelCall
}

type federationModelCall struct {
	request llmkit.Request
	reply   chan llmkit.Event
}

func (m *federationModel) Call(ctx context.Context, req llmkit.Request, _ llmkit.Target, _ llmkit.CallOptions) (<-chan llmkit.Event, error) {
	m.calls.Add(1)
	call := federationModelCall{request: req, reply: make(chan llmkit.Event, 2)}
	select {
	case m.requests <- call:
		return call.reply, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m *federationModel) next(t *testing.T) federationModelCall {
	t.Helper()
	select {
	case call := <-m.requests:
		return call
	case <-time.After(federationWait):
		t.Fatalf("timed out waiting for model call after %d calls", m.calls.Load())
		return federationModelCall{}
	}
}

func (m *federationModel) assertCalls(t *testing.T, want int32) {
	t.Helper()
	if got := m.calls.Load(); got != want {
		t.Fatalf("model calls = %d, want %d", got, want)
	}
}

func (c federationModelCall) send(t *testing.T, to, text string, wait bool) {
	t.Helper()
	if !slices.ContainsFunc(c.request.Tools, func(tool llmkit.Tool) bool { return tool.Name == "send_world_message" }) {
		t.Fatal("Agent did not receive the send_world_message tool")
	}
	args := fmt.Sprintf(`{"to":%q,"message":%q,"wait":%t}`, to, text, wait)
	c.finish(llmkit.Event{Type: llmkit.EventToolCallEnd, ToolCall: &llmkit.StreamingToolCall{
		CallID: string(defs.NewItemID()), Name: "send_world_message", Arguments: args,
	}}, "tool_calls")
}

func (c federationModelCall) answer(text string) {
	c.finish(llmkit.Event{Type: llmkit.EventContentDelta, Delta: &llmkit.DeltaPayload{Text: text}}, "stop")
}

func (c federationModelCall) finish(event llmkit.Event, reason string) {
	c.reply <- event
	c.reply <- llmkit.Event{Type: llmkit.EventDone, FinishReason: reason}
	close(c.reply)
}

func (c federationModelCall) result(t *testing.T, status world.WorldMessageStatus, reply string) federationModelCall {
	t.Helper()
	result := c.toolResult(t)
	var output struct {
		MessageID string `json:"message_id"`
		Status    world.WorldMessageStatus
		Reply     string
	}
	if result.Error != nil {
		t.Fatalf("send_world_message failed: %+v", result.Error)
	}
	if err := json.Unmarshal(result.Content, &output); err != nil {
		t.Fatalf("send_world_message returned %s: %v", result.Content, err)
	}
	if output.MessageID == "" || output.Status != status || output.Reply != reply {
		t.Fatalf("send_world_message returned %+v, want a message ID, %s, %q", output, status, reply)
	}
	return c
}

func (c federationModelCall) toolResult(t *testing.T) defs.ToolResult {
	t.Helper()
	for _, message := range slices.Backward(c.request.Messages) {
		if message.Role == llmkit.RoleTool {
			var result defs.ToolResult
			if err := result.FromLLMKit(&message); err != nil {
				t.Fatalf("decode send_world_message result: %v", err)
			}
			return result
		}
	}
	t.Fatal("model did not receive a tool result")
	return defs.ToolResult{}
}

func (c federationModelCall) userInputContains(t *testing.T, text string) {
	t.Helper()
	for _, message := range c.request.Messages {
		if message.Role == llmkit.RoleUser && slices.ContainsFunc(message.Content, func(block llmkit.ContentBlock) bool {
			return strings.Contains(block.Text, text)
		}) {
			return
		}
	}
	t.Fatalf("model did not receive user message containing %q", text)
}

// observePairDisconnect 在响应节点的 accept 连接中丢弃首个密文回执。
func observePairDisconnect(t *testing.T, base string) (string, <-chan struct{}) {
	t.Helper()
	dropped := make(chan struct{})
	var first atomic.Bool
	upstreamURL, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(upstreamURL)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/world/v1/relay/accept" || !first.CompareAndSwap(false, true) {
			proxy.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		upstream, _, err := websocket.Dial(ctx, base+r.URL.RequestURI(), nil)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		defer upstream.CloseNow()
		node, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer node.CloseNow()
		upstream.SetReadLimit(worldserver.ChannelFrameLimit)
		node.SetReadLimit(worldserver.ChannelFrameLimit)
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer cancel()
			for {
				kind, frame, err := upstream.Read(ctx)
				if err != nil || node.Write(ctx, kind, frame) != nil {
					return
				}
			}
		}()
		defer func() { cancel(); <-done }()
		for index := 0; ; index++ {
			kind, frame, err := node.Read(ctx)
			if err != nil || kind != websocket.MessageBinary {
				return
			}
			if index == 1 {
				close(dropped)
				continue
			}
			if upstream.Write(ctx, kind, frame) != nil {
				return
			}
		}
	}))
	t.Cleanup(endpoint.Close)
	return endpoint.URL, dropped
}

func TestWorldSettingsRestart(t *testing.T) {
	f := newConversationManagementFixture(t, buildOpenVaala(t))
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	path := getE2EServerConnectionPath(f.config)
	conn, err := getE2EServerConnection(path)
	if err != nil {
		t.Fatal(err)
	}
	base := strings.TrimSuffix(strings.Replace(conn.URL, "ws:", "http:", 1), "/api/v1/openvaala")
	var invite string
	for _, step := range []struct {
		name, domain  string
		join, enabled bool
	}{
		{"host", "team", false, false}, {"join", "team", true, true}, {"rename", "team2", false, false},
	} {
		t.Run(step.name, func(t *testing.T) {
			got, err := f.control.GetWorldSettings(ctx)
			if err != nil {
				t.Fatal(err)
			}
			params := runtimeprotocol.SetWorldParams{Settings: got.Settings}
			params.Settings.Server.Enabled = true
			params.Settings.Server.Domain = step.domain
			params.Settings.Relays = []string{base + "/"}
			params.Settings.WebURL = base
			params.Settings.RelayMode = "domain"
			if step.join {
				var issued worldserver.CreateJoinTokenResponse
				openExternalWire(t, f.config).call(t, "world/admin/createToken", worldserver.CreateJoinTokenRequest{
					Owner: "settings", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
				}, &issued)
				if issued.Token == "" {
					t.Fatal("missing invite token")
				}
				invite = issued.Token
				params.Join = &runtimeprotocol.WorldJoinParams{InviteCode: invite}
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			result, err := f.control.SetWorldSettings(ctx, params)
			if err != nil || !result.Restarting {
				t.Fatalf("set: %+v %v", result, err)
			}
			next := f.serve.waitServeHealthy(t, path, before, http.DefaultClient, 25*time.Millisecond)
			if next != conn {
				t.Fatal("serve connection address or token changed")
			}
			got, err = f.control.GetWorldSettings(ctx)
			if err != nil || got.Running.NodeEnabled != step.enabled || !got.Running.ServerEnabled || got.Settings.Server.Domain != step.domain {
				t.Fatalf("after restart: %+v %v", got, err)
			}
			if got.Settings.RelayMode != "domain" || !slices.Equal(got.Settings.Relays, params.Settings.Relays) || got.Settings.WebURL != base {
				t.Fatalf("relay settings after restart: %+v", got.Settings)
			}
			if step.join {
				federationEventually(t, "self registered node connected", func() bool {
					status, err := f.control.GetWorldStatus(ctx)
					return err == nil && status.Connected
				})
			}
			if step.name == "rename" {
				bak, _ := filepath.Glob(filepath.Join(filepath.Dir(f.config), ".local/share/openvaala/worldserver.bak-team-*/world.db"))
				if len(bak) != 1 || got.Settings.Node.Enabled || got.Settings.Node.Domain != "team" {
					t.Fatalf("archive %v, node %+v", bak, got.Settings.Node)
				}
			}
		})
		if t.Failed() {
			return
		}
	}
}

func TestWorldRPCMigration(t *testing.T) {
	directory := newFederationServer(t, "domain")
	model := newE2EModel(t)
	f := &conversationManagementFixture{
		binary: buildOpenVaala(t),
		config: writeE2EConfig(t, model.server.URL),
		model:  model,
	}
	const adminToken = "migration-test-admin"
	settings, _, err := config.LoadConfigSettings(&config.LoadConfigOpt{Home: filepath.Dir(f.config), Path: f.config})
	if err != nil {
		t.Fatal(err)
	}
	direct := "http://" + settings.Server.Listen
	directory.register(t, filepath.Join(config.GetDataDirectory(filepath.Dir(f.config)), "world"), "alice", "team")
	directory.register(t, filepath.Join(t.TempDir(), "world"), "bob", "team")
	appendE2EConfig(t, f.config, fmt.Sprintf(`
world:
  node:
    enabled: true
    name: alice
    domain: %s
    server_url: %q
    direct_urls: [%q]
    default_target: assistant
  server:
    enabled: true
    domain: migration.test
    admin_token: %s
`, federationDomain, directory.url, direct, adminToken))
	f.startServe(t)
	conn, err := getE2EServerConnection(getE2EServerConnectionPath(f.config))
	if err != nil {
		t.Fatal(err)
	}
	base := strings.TrimSuffix(strings.Replace(conn.URL, "ws://", "http://", 1), "/api/v1/openvaala")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for _, route := range []struct{ method, path string }{
		{"GET", "/status"}, {"GET", "/peers"}, {"POST", "/messages"}, {"GET", "/messages"},
		{"DELETE", "/messages/id"}, {"POST", "/forward"}, {"GET", "/threads"}, {"GET", "/events"},
		{"GET", "/admin/nodes"}, {"POST", "/admin/nodes/id/revoke"}, {"POST", "/admin/owners/team/revoke"},
		{"GET", "/admin/tokens"}, {"POST", "/admin/tokens"}, {"DELETE", "/admin/tokens/id"},
		{"GET", "/admin/acl"}, {"PUT", "/admin/acl"},
	} {
		t.Run(route.method+route.path, func(t *testing.T) {
			for _, method := range []string{route.method, http.MethodOptions} {
				federationHTTP(t, method, base+federationAPI+route.path, http.Header{
					"Authorization":                 {"Bearer " + conn.Token},
					"Origin":                        {"https://client.example"},
					"Access-Control-Request-Method": {route.method},
				}, nil, http.StatusNotFound)
			}
		})
	}
	wire := openExternalWire(t, f.config)
	federationEventually(t, "co-hosted WorldServer ready", func() bool {
		response := wire.response(t, wire.request(t, "world/admin/getACL", nil))
		return response.Error == nil
	})
	federationHTTP(t, http.MethodGet, base+"/world/v1/admin/nodes", http.Header{}, nil, http.StatusUnauthorized)
	federationHTTP(t, http.MethodGet, base+"/world/v1/admin/nodes", http.Header{"Authorization": {"Bearer " + conn.Token}}, nil, http.StatusUnauthorized)
	federationHTTP(t, http.MethodGet, base+"/world/v1/admin/nodes", http.Header{"Authorization": {"Bearer " + adminToken}}, nil, http.StatusOK)
	_, resp, err := websocket.Dial(ctx, conn.URL, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + adminToken}}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Runtime accepted control-plane token: %v, %v", resp, err)
	}

	client := f.control
	federationEventually(t, "World status connected", func() bool {
		status, err := client.GetWorldStatus(ctx)
		return err == nil && status.Enabled && status.AdminEnabled && status.Connected
	})
	peers, err := client.ListWorldPeers(ctx)
	if err != nil || len(peers.Peers) != 1 || peers.Peers[0].NodeName != "bob" {
		t.Fatalf("peers: %+v, %v", peers, err)
	}
	if err := client.SubscribeWorld(ctx); err != nil {
		t.Fatal(err)
	}
	sent, err := client.SendWorldMessage(ctx, world.SendMessageRequest{To: "bob", Message: "manual"})
	if err != nil || sent.Status != world.WorldMessageStatusPendingOffline {
		t.Fatalf("send: %+v, %v", sent, err)
	}
	if err := client.UnsubscribeWorld(ctx); err != nil {
		t.Fatal(err)
	}

	conversation, err := client.CreateConversation(ctx, &runtimeclient.CreateConversationRequest{AgentName: "assistant"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.SubmitInput(ctx, &runtimeclient.SubmitInputRequest{
		ConversationID: conversation.ID,
		Content:        []*defs.Content{{Kind: defs.ContentText, Text: &defs.TextContent{Text: "question"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	model.next(t).reply <- e2eModelReply{text: "answer"}
	f.waitTurnsCompleted(t, conversation.ID, conversation.RootSessionID, 1)
	history, err := client.ListItems(ctx, &runtimeclient.ListItemsRequest{
		ConversationID: conversation.ID, SessionID: conversation.RootSessionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	var selected []defs.ItemID
	for _, item := range history.Items {
		if item.Item.Kind == defs.ItemMessage {
			selected = append(selected, item.Item.ID)
		}
	}
	if len(selected) != 2 {
		t.Fatalf("expected user and assistant messages, got %v", selected)
	}
	slices.Reverse(selected)
	forwarded, err := client.ForwardWorldConversation(ctx, world.ForwardRequest{
		ConversationID: conversation.ID, To: "bob", ItemIDs: selected, Note: "note",
	})
	if err != nil || forwarded.Body != `question

answer

note` {
		t.Fatalf("forward: %+v, %v", forwarded, err)
	}
	limit := 1
	params := world.ListMessagesParams{Limit: &limit, PeerAddresses: []string{sent.PeerAddr, sent.PeerAddr}}
	page, err := client.ListWorldMessages(ctx, params)
	if err != nil || len(page.Items) != 1 || page.Items[0].MessageID != forwarded.MessageID || page.NextCursor.ID == "" {
		t.Fatalf("first message page: %+v, %v", page, err)
	}
	params.CursorID = &page.NextCursor.ID
	page, err = client.ListWorldMessages(ctx, params)
	if err != nil || len(page.Items) != 1 || page.Items[0].MessageID != sent.MessageID || page.NextCursor.ID != "" {
		t.Fatalf("second message page: %+v, %v", page, err)
	}
	threadPage, err := client.ListWorldThreads(ctx, world.ListThreadsParams{ConversationID: string(conversation.ID), Limit: &limit})
	if err != nil || threadPage.Items == nil {
		t.Fatalf("threads: %+v, %v", threadPage, err)
	}
	cancelled, err := client.CancelWorldMessage(ctx, sent.MessageID)
	if err != nil || cancelled.Status != world.WorldMessageStatusCancelled {
		t.Fatalf("cancel: %+v, %v", cancelled, err)
	}
	_, err = client.CancelWorldMessage(ctx, sent.MessageID)
	var response *runtimeclient.ResponseError
	if !errors.As(err, &response) || response.Data == nil || response.Data.Code != "world_outbox_not_cancellable" ||
		response.Message != fmt.Sprintf("message %q can only be cancelled while pending_offline", sent.MessageID) {
		t.Fatalf("second cancel: %v", err)
	}

	rules := []worldserver.ACLRule{{Priority: 1, From: []string{"*"}, To: []string{"*"}, Action: "allow"}}
	wire.call(t, "world/admin/replaceACL", rules, nil)
	var gotRules []worldserver.ACLRule
	wire.call(t, "world/admin/getACL", nil, &gotRules)
	if len(gotRules) != 1 || gotRules[0].Action != "allow" {
		t.Fatalf("ACL: %+v", gotRules)
	}
	var invite worldserver.CreateJoinTokenResponse
	wire.call(t, "world/admin/createToken", worldserver.CreateJoinTokenRequest{
		Owner: "team", Reusable: true, ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	}, &invite)
	if invite.Token == "" {
		t.Fatalf("issue invite: %+v", invite)
	}
	nodes := make([]worldserver.RegisterNodeResponse, 0, 2)
	for _, name := range []string{"office", "lab"} {
		id, err := world.LoadOrCreateIdentity(filepath.Join(t.TempDir(), "world", "identity.json"))
		if err != nil {
			t.Fatal(err)
		}
		req := worldserver.RegisterNodeRequest{NodeName: name, JoinToken: invite.Token, SignPublic: id.SignPublic, BoxPublic: id.BoxPublic}
		req.Signature, err = id.Sign(worldserver.RegisterSignBytes(req))
		if err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, federationJSON[worldserver.RegisterNodeResponse](t, http.MethodPost, base+"/world/v1/nodes/register", req, http.StatusCreated))
	}
	wire.call(t, "world/admin/revokeToken", runtimeprotocol.WorldTokenParams{ID: invite.ID}, nil)
	var tokens []worldserver.JoinTokenInfo
	wire.call(t, "world/admin/listTokens", nil, &tokens)
	if len(tokens) != 1 || tokens[0].ID != invite.ID || tokens[0].UsedCount != 2 || tokens[0].RevokedAt == 0 {
		t.Fatalf("tokens: %+v", tokens)
	}
	wire.call(t, "world/admin/revokeNode", runtimeprotocol.WorldNodeParams{NodeID: nodes[0].NodeID}, nil)
	wire.call(t, "world/admin/revokeOwner", runtimeprotocol.WorldOwnerParams{Owner: "team"}, nil)
	var list []worldserver.AdminNode
	wire.call(t, "world/admin/listNodes", nil, &list)
	if len(list) != 2 || slices.ContainsFunc(list, func(node worldserver.AdminNode) bool { return node.RevokedAt == 0 }) {
		t.Fatalf("nodes: %+v", list)
	}
	// 同服管理 RPC 导入调用域，分别由直连和同服 relay 查询活动成员。
	caller, err := world.LoadOrCreateIdentity(filepath.Join(t.TempDir(), "caller", "domain.json"))
	if err != nil {
		t.Fatal(err)
	}
	declared := worldserver.PeerDomain{Domain: "caller.test", PublicKey: caller.SignPublic, Entries: worldserver.PeerEntries{DirectURLs: []string{base}}}
	raw, _ := json.Marshal(declared)
	var added worldserver.PeerDomain
	wire.call(t, "world/admin/addDomain", worldserver.AddDomainRequest{Card: declared.Domain + "#" + base64.RawURLEncoding.EncodeToString(raw)}, &added)
	if added.Domain != declared.Domain {
		t.Fatal(added)
	}
	var domains worldserver.ListDomainsResult
	wire.call(t, "world/admin/listDomains", nil, &domains)
	if len(domains.Domains) != 1 {
		t.Fatal(domains)
	}
	_, encoded, _ := strings.Cut(domains.SelfCard, "#")
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	var self worldserver.PeerDomain
	if err != nil || json.Unmarshal(data, &self) != nil || !slices.Equal(self.Entries.DirectURLs, []string{direct}) || !slices.Equal(self.Entries.Relays, []string{direct}) {
		t.Fatalf("self card: %+v %v", self, err)
	}
	channel, err := new(world.PeerDialer).DialPeer(ctx, self.Domain, self.PublicKey, self.Entries, &caller, world.ChannelPurposeDomain)
	if err != nil {
		t.Fatal(err)
	}
	trust, _ := json.Marshal(worldserver.DomainTrustMessage{Op: "trust_response", Domain: declared.Domain, Decision: "accepted"})
	if err = channel.WriteMessage(ctx, trust); err != nil {
		t.Fatal(err)
	}
	if _, err = channel.ReadMessage(ctx); err != nil {
		t.Fatal(err)
	}
	channel.Close()
	for _, entries := range []worldserver.PeerEntries{{DirectURLs: self.Entries.DirectURLs}, {Relays: self.Entries.Relays}} {
		ch, err := new(world.PeerDialer).DialPeer(ctx, self.Domain, self.PublicKey, entries, &caller, world.ChannelPurposeDomain)
		if err != nil {
			t.Fatal(err)
		}
		if err = ch.WriteMessage(ctx, []byte(`{"op":"lookup","node":"office"}`)); err != nil {
			t.Fatal(err)
		}
		data, err = ch.ReadMessage(ctx)
		ch.Close()
		var result map[string]any
		if err != nil || json.Unmarshal(data, &result) != nil || result["code"] != "world_peer_not_found" || result["error"] != `world peer "office@migration.test" is not in directory` {
			t.Fatalf("revoked member: %s, %v", data, err)
		}
	}
	wire.call(t, "world/admin/deleteDomain", runtimeprotocol.WorldDomainParams{Domain: declared.Domain}, nil)
	wire.call(t, "world/admin/listDomains", nil, &domains)
	if len(domains.Domains) != 0 {
		t.Fatal(domains)
	}

}

func TestWorldDialRoutes(t *testing.T) {
	binary := buildOpenVaala(t)
	const address = "bob@" + federationDomain
	for _, tc := range []struct {
		name                         string
		direct, stopHome, stopPublic bool
	}{
		{"direct", true, false, false},
		{"home relay", false, false, false},
		{"second relay", false, true, false},
		{"all unavailable", false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			homeAddress := getFreeAddress(t)
			homeURL := "http://" + homeAddress
			homeConfig := writeE2EConfigWithoutModels(t)
			appendE2EConfig(t, homeConfig, fmt.Sprintf("\nworld:\n  server: {enabled: true, domain: %s, admin_token: %s}\n", federationDomain, federationToken))
			home, _ := startE2EProcess(t, binary, homeConfig, []string{"world", "serve", "--listen", homeAddress}, false)
			federationEventually(t, "home relay", func() bool {
				resp, err := http.Get(homeURL + "/world/v1/healthz")
				if err != nil {
					return false
				}
				resp.Body.Close()
				return resp.StatusCode == 200
			})

			publicConfig := writeE2EConfigWithoutModels(t)
			appendE2EConfig(t, publicConfig, "\nworld: {relay_mode: public}\n")
			public, _ := startE2EServe(t, binary, publicConfig)
			publicConn, err := getE2EServerConnection(getE2EServerConnectionPath(publicConfig))
			if err != nil {
				t.Fatal(err)
			}
			publicURL := strings.TrimSuffix(publicConn.URL, "/api/v1/openvaala")
			var homeDials atomic.Int32
			upstream, _ := url.Parse(homeURL)
			proxy := httputil.NewSingleHostReverseProxy(upstream)
			observed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/world/v1/relay/dial" {
					homeDials.Add(1)
				}
				proxy.ServeHTTP(w, r)
			}))
			t.Cleanup(observed.Close)
			directory := &federationServer{url: observed.URL}
			sender := directory.register(t, filepath.Join(t.TempDir(), "identity"), "alice", "team")
			b := &conversationManagementFixture{binary: binary, config: writeE2EConfig(t, "http://127.0.0.1:1")}
			cfg, _, err := config.LoadConfigSettings(&config.LoadConfigOpt{Home: filepath.Dir(b.config), Path: b.config})
			if err != nil {
				t.Fatal(err)
			}
			direct := "http://" + cfg.Server.Listen
			identity := directory.register(t, filepath.Join(cfg.GetDataDirectory(), "world"), "bob", "team")
			directURLs := "[]"
			if tc.direct {
				directURLs = fmt.Sprintf("[%q]", direct)
			}
			appendE2EConfig(t, b.config, fmt.Sprintf("\nworld:\n  relays: [%q]\n  node: {enabled: true, name: bob, domain: %s, server_url: %q, direct_urls: %s}\n", publicURL, federationDomain, observed.URL, directURLs))
			b.startServe(t)
			t.Cleanup(b.stopControl)
			entries := worldserver.PeerEntries{Relays: []string{observed.URL, publicURL}}
			if tc.direct {
				entries.DirectURLs = []string{direct}
			}
			federationEventually(t, "node attachments", func() bool {
				status, err := b.control.GetWorldStatus(t.Context())
				return err == nil && status.Connected && len(status.RelayAttachments) == 2 && status.RelayAttachments[0].Connected && status.RelayAttachments[1].Connected
			})
			if tc.stopHome {
				home.stop(t)
			}
			if tc.stopPublic {
				public.stop(t)
			}
			before := homeDials.Load()
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			channel, err := new(world.PeerDialer).DialPeer(ctx, address, identity.SignPublic, entries, &sender, world.ChannelPurposeWorld)
			if tc.stopPublic {
				if world.ErrorCode(err) != "world_target_unreachable" || err.Error() != "cannot reach "+address+": direct addresses and all relays are unreachable" {
					t.Fatal(err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer channel.Close()
			want := int32(1)
			if tc.direct {
				want = 0
			}
			if got := homeDials.Load() - before; got != want {
				t.Fatalf("home dial requests %d, want %d", got, want)
			}
			query := map[string]any{"kind": "status_query", "message_id": "query", "in_reply_to": "absent", "thread_id": "example", "from": world.Address{Node: "alice", Domain: federationDomain}, "to": world.Address{Node: "bob", Domain: federationDomain}}
			body, err := json.Marshal(query)
			if err != nil {
				t.Fatal(err)
			}
			if tc.direct || tc.stopHome {
				body = append(body, bytes.Repeat([]byte(" "), (16<<20)-len(body))...)
			}
			if err = channel.WriteMessage(ctx, body); err != nil {
				t.Fatal(err)
			}
			reply, err := channel.ReadMessage(ctx)
			var ack struct{ Status string }
			if err != nil || json.Unmarshal(reply, &ack) != nil || ack.Status != "not_found" {
				t.Fatalf("reply %s, %v", reply, err)
			}
		})
	}
}

func TestWorldControlPlaneRelayStatus(t *testing.T) {
	binary := buildOpenVaala(t)
	publicConfig := writeE2EConfigWithoutModels(t)
	appendE2EConfig(t, publicConfig, "\nworld: {relay_mode: public}\n")
	public, _ := startE2EServe(t, binary, publicConfig)
	conn, err := getE2EServerConnection(getE2EServerConnectionPath(publicConfig))
	if err != nil {
		t.Fatal(err)
	}
	relay := strings.TrimSuffix(conn.URL, "/api/v1/openvaala")
	domain := &conversationManagementFixture{binary: binary, config: writeE2EConfigWithoutModels(t)}
	appendE2EConfig(t, domain.config, fmt.Sprintf("\nworld: {server: {enabled: true, domain: home.test}, relays: [%q]}\n", relay))
	domain.startServe(t)
	t.Cleanup(domain.stopControl)
	for _, connected := range []bool{true, false} {
		if !connected {
			public.stop(t)
		}
		federationEventually(t, fmt.Sprintf("domain attachment connected=%t", connected), func() bool {
			status, err := domain.control.GetWorldStatus(t.Context())
			return err == nil && status.AdminEnabled && !status.Enabled && slices.Equal(status.RelayAttachments, []world.RelayStatus{{URL: relay, Connected: connected}})
		})
	}
}

// TestWorldCrossDomainDelivery 用两个独立域、三个节点和公共 relay 验证双向身份和断线恢复。
func TestWorldCrossDomainDelivery(t *testing.T) {
	binary := buildOpenVaala(t)
	relayConfig := writeE2EConfigWithoutModels(t)
	relayAddr := getFreeAddress(t)
	startE2EProcess(t, binary, relayConfig, []string{"world", "serve", "--listen", relayAddr, "--relay-mode", "public"}, false)
	relayURL := "http://" + relayAddr
	entries := worldserver.PeerEntries{Relays: []string{relayURL}}

	type domainProcess struct {
		config, addr, base, card string
		process                  *e2eProcess
	}
	startDomain := func(d *domainProcess) {
		t.Helper()
		d.process, _ = startE2EProcess(t, binary, d.config, []string{"world", "serve", "--listen", d.addr}, false)
		federationEventually(t, "domain health", func() bool {
			resp, err := http.Get(d.base + "/world/v1/healthz")
			if err != nil {
				return false
			}
			defer resp.Body.Close()
			return resp.StatusCode == 200
		})
	}
	domain := func(name string) *domainProcess {
		t.Helper()
		d := &domainProcess{config: writeE2EConfigWithoutModels(t), addr: getFreeAddress(t)}
		d.base = "http://" + d.addr
		appendE2EConfig(t, d.config, fmt.Sprintf("\nworld:\n  relay_mode: off\n  relays: [%q]\n  server: {enabled: true, domain: %s, admin_token: %s}\n", relayURL, name, federationToken))
		startDomain(d)
		result := federationJSON[worldserver.ListDomainsResult](t, "GET", d.base+"/world/v1/admin/domains", nil, 200)
		if !strings.HasPrefix(result.SelfCard, name+"#") {
			t.Fatal(result)
		}
		d.card = result.SelfCard
		return d
	}
	home, bobDomain := domain("home"), domain("bob.org")
	model := newE2EModel(t)
	var turns atomic.Int32
	modelCtx, stopModel := context.WithCancel(t.Context())
	defer stopModel()
	go func() {
		for {
			select {
			case <-modelCtx.Done():
				return
			case req := <-model.requests:
				turns.Add(1)
				req.reply <- e2eModelReply{text: "received"}
			}
		}
	}()
	node := func(d *domainProcess, name, domain, defaultTarget string) (*conversationManagementFixture, world.NodeIdentity) {
		t.Helper()
		f := &conversationManagementFixture{binary: binary, config: writeE2EConfig(t, model.server.URL)}
		cfg, _, err := config.LoadConfigSettings(&config.LoadConfigOpt{Home: filepath.Dir(f.config), Path: f.config})
		if err != nil {
			t.Fatal(err)
		}
		base := &federationServer{url: d.base}
		id := base.register(t, filepath.Join(cfg.GetDataDirectory(), "world"), name, "team")
		if err := os.WriteFile(filepath.Join(filepath.Dir(f.config), "agents", "public.yaml"), []byte("name: public\ninstructions: Reply.\nmodel: {provider: local, model: default}\n"), 0600); err != nil {
			t.Fatal(err)
		}
		appendE2EConfig(t, f.config, fmt.Sprintf(`
world:
  relays: [%q]
  node:
    enabled: true
    name: %s
    domain: %s
    server_url: %q
    default_target: %s
    advertise_targets: [{name: assistant, visibility: domain}, {name: public, visibility: public}]
`, relayURL, name, domain, d.base, defaultTarget))
		f.startServe(t)
		t.Cleanup(func() { f.stopControl() })
		return f, id
	}
	waitNode := func(f *conversationManagementFixture, connected bool) {
		t.Helper()
		federationEventually(t, "node control and public relay", func() bool {
			st, err := f.control.GetWorldStatus(t.Context())
			return err == nil && st.Connected == connected && len(st.RelayAttachments) == 2 && st.RelayAttachments[1].Connected
		})
	}
	importCard := func(d *domainProcess, card string) worldserver.PeerDomain {
		return federationJSON[worldserver.PeerDomain](t, "POST", d.base+"/world/v1/admin/domains", worldserver.AddDomainRequest{Card: card}, 201)
	}
	bobCard := importCard(home, bobDomain.card)
	alice, aliceID := node(home, "alice", "home", "assistant")
	waitNode(alice, true)
	send := func(f *conversationManagementFixture, to, body string) world.WorldMessage {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		msg, err := f.control.SendWorldMessage(ctx, world.SendMessageRequest{To: to, Message: body, Wait: true})
		if err != nil {
			t.Fatal(err)
		}
		return msg
	}
	domainsPath := "/world/v1/admin/domains"
	state := func(d *domainProcess, want string) {
		t.Helper()
		federationEventually(t, want, func() bool {
			res := federationJSON[worldserver.ListDomainsResult](t, "GET", d.base+domainsPath, nil, 200)
			return len(res.Domains) == 1 && res.Domains[0].Status == want
		})
	}
	denied := func() {
		t.Helper()
		_, err := alice.control.SendWorldMessage(t.Context(), world.SendMessageRequest{To: "bob@bob.org/public", Message: "not connected"})
		var rpc *runtimeclient.ResponseError
		if !errors.As(err, &rpc) || rpc.Data.Code != "world_domain_not_trusted" {
			t.Fatalf("send: %v", err)
		}
	}
	state(home, worldserver.DomainPendingOutbound)
	denied()
	state(bobDomain, worldserver.DomainPendingInbound)
	// 决定送达前发起域离线；重启和本地拒发均保留待确认，重新建域通道时取回已保存的拒绝。
	home.process.stop(t)
	federationHTTP(t, "POST", bobDomain.base+domainsPath+"/home/reject", nil, nil, 204)
	startDomain(home)
	waitNode(alice, true)
	denied()
	state(home, worldserver.DomainPendingOutbound)
	federationHTTP(t, "POST", home.base+domainsPath, nil, worldserver.AddDomainRequest{Card: bobDomain.card}, 200)
	state(home, worldserver.DomainRejected)
	denied()
	clear := func() {
		federationHTTP(t, "DELETE", home.base+domainsPath+"/bob.org", nil, nil, 204)
		federationHTTP(t, "DELETE", bobDomain.base+domainsPath+"/home", nil, nil, 204)
	}
	clear()
	importCard(home, bobDomain.card)
	state(bobDomain, worldserver.DomainPendingInbound)
	other := bobCard
	other.PublicKey = aliceID.SignPublic
	raw, _ := json.Marshal(other)
	conflict := federationJSON[map[string]string](t, "POST", home.base+domainsPath, worldserver.AddDomainRequest{Card: other.Domain + "#" + base64.RawURLEncoding.EncodeToString(raw)}, 409)
	if conflict["code"] != "world_domain_name_taken" {
		t.Fatal(conflict)
	}
	home.process.stop(t)
	federationHTTP(t, "POST", bobDomain.base+domainsPath+"/home/accept", nil, nil, 204)
	startDomain(home)
	state(home, worldserver.DomainPendingOutbound)
	state(bobDomain, worldserver.DomainMutual)
	cold, _ := node(home, "cold", "home", "assistant")
	bob, bobID := node(bobDomain, "bob", "bob.org", "public")
	for _, n := range []*conversationManagementFixture{alice, cold, bob} {
		waitNode(n, true)
	}
	// 已接受的对端发起查人，同一通道先补回接受决定，再返回查人结果。
	if msg := send(bob, "missing@home", "lookup"); msg.ErrorCode != "world_peer_not_found" {
		t.Fatal(msg)
	}
	state(home, worldserver.DomainMutual)
	for _, path := range []string{"/api/v1/openvaala", "/world/v1/connect", "/world/v1/admin/nodes"} {
		federationHTTP(t, http.MethodGet, relayURL+path, nil, nil, http.StatusNotFound)
	}

	for _, tc := range []struct{ name, to, code, text string }{
		{"explicit private", "bob@bob.org/assistant", "world_target_not_public", `target "assistant" does not accept messages from other domains`},
		{"unknown destination", "missing@bob.org/public", "world_peer_not_found", `world peer "missing@bob.org" is not in directory`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := send(cold, tc.to, tc.name)
			if msg.Status != world.WorldMessageStatusFailed || msg.ErrorCode != tc.code || msg.ErrorText != tc.text {
				t.Fatal(msg)
			}
		})
	}
	for _, tc := range []struct {
		name string
		id   *world.NodeIdentity
	}{
		{"member domain query", &aliceID}, {"anonymous domain query", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.id == nil {
				// 直接向域控制面发送 v2 匿名 domain hello，等对端关闭连接。
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
				defer cancel()
				endpoint := bobDomain.base + "/world/v1/direct?purpose=domain&target=" + base64.RawURLEncoding.EncodeToString(bobCard.PublicKey)
				socket, _, err := websocket.Dial(ctx, endpoint, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer socket.CloseNow()
				ephemeral, err := ecdh.X25519().GenerateKey(rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				channelID := make([]byte, 16)
				rand.Read(channelID)
				hello := slices.Concat([]byte{2, 3, 0}, channelID, bobCard.PublicKey, ephemeral.PublicKey().Bytes())
				if err = socket.Write(ctx, websocket.MessageBinary, hello); err != nil {
					t.Fatal(err)
				}
				if _, data, err := socket.Read(ctx); err == nil || ctx.Err() != nil {
					t.Fatalf("anonymous domain hello: response %x, error %v", data, err)
				}
				return
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			channel, err := new(world.PeerDialer).DialPeer(ctx, "bob.org", bobCard.PublicKey, entries, tc.id, world.ChannelPurposeDomain)
			if err != nil {
				t.Fatal(err)
			}
			defer channel.Close()
			if err = channel.WriteMessage(ctx, []byte(`{"op":"lookup","node":"bob"}`)); err != nil {
				t.Fatal(err)
			}
			if data, err := channel.ReadMessage(ctx); err == nil || ctx.Err() != nil {
				t.Fatalf("unexpected domain response %s, %v", data, err)
			}
		})
	}
	sibling := (&federationServer{url: bobDomain.base}).register(t, filepath.Join(t.TempDir(), "identity"), "sibling", "outsider")
	federationHTTP(t, "PUT", bobDomain.base+"/world/v1/admin/acl", nil, []worldserver.ACLRule{{From: []string{"node:sibling"}, To: []string{"node:bob"}, Action: "deny"}}, 204)
	for _, tc := range []struct {
		from       world.Address
		id         world.NodeIdentity
		code, text string
	}{
		{world.Address{Node: "alice", Domain: "third.test"}, aliceID, "world_domain_not_trusted", `domain "third.test" is not trusted`},
		{world.Address{Node: "sibling", Domain: "bob.org"}, sibling, "world_sender_not_allowed", `sender "sibling@bob.org" is not allowed to message this node`},
		{world.Address{Node: "alice", Domain: "home"}, sibling, "world_identity_unverified", `cannot verify "alice@home": its domain control plane is unreachable`},
	} {
		t.Run(tc.code, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			ch, err := new(world.PeerDialer).DialPeer(ctx, "bob@bob.org", bobID.SignPublic, entries, &tc.id, world.ChannelPurposeWorld)
			if err != nil {
				t.Fatal(err)
			}
			defer ch.Close()
			data, _ := json.Marshal(map[string]any{"kind": "message", "message_id": "denied", "thread_id": "denied", "from": tc.from, "to": world.Address{Node: "bob", Domain: "bob.org", Target: "public"}, "hop_limit": 1, "body": "blocked"})
			if err = ch.WriteMessage(ctx, data); err != nil {
				t.Fatal(err)
			}
			data, err = ch.ReadMessage(ctx)
			var ack map[string]any
			if err != nil || json.Unmarshal(data, &ack) != nil || ack["error_code"] != tc.code || ack["body"] != tc.text {
				t.Fatalf("ack %s, %v", data, err)
			}
		})
	}
	// 重启收件节点清空它的缓存，两个发件节点继续保留自己的进程。
	bob.stopServe(t)
	bob.startServe(t)
	waitNode(bob, true)
	if msg := send(alice, "bob@bob.org/public", "warm"); msg.Status != world.WorldMessageStatusReplied || msg.ReplyBody != "received" {
		t.Fatal(msg)
	}
	home.process.stop(t)
	waitNode(alice, false)
	waitNode(cold, false)
	pending := send(cold, "bob@bob.org/public", "after recovery")
	if pending.Status != world.WorldMessageStatusPendingOffline || pending.ErrorCode != "world_identity_unverified" || pending.ErrorText != `cannot verify "cold@home": its domain control plane is unreachable` {
		t.Fatal(pending)
	}
	if msg := send(alice, "bob@bob.org/public", "both cached"); msg.Status != world.WorldMessageStatusReplied || msg.ReplyBody != "received" {
		t.Fatal(msg)
	}
	if turns.Load() != 2 {
		t.Fatalf("model turns before recovery: %d", turns.Load())
	}
	startDomain(home)
	waitNode(cold, true)
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	for {
		page, err := cold.control.ListWorldMessages(ctx, world.ListMessagesParams{Direction: "outbound"})
		if err != nil {
			t.Fatal(err)
		}
		if slices.ContainsFunc(page.Items, func(m world.WorldMessage) bool {
			return m.MessageID == pending.MessageID && m.Status == world.WorldMessageStatusReplied && m.ReplyBody == "received" && m.ErrorCode == ""
		}) {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("recovery: %+v", page)
		case <-time.After(100 * time.Millisecond):
		}
	}
	if turns.Load() != 3 {
		t.Fatalf("model turns after recovery: %d", turns.Load())
	}

	t.Run("manual return in the original thread", func(t *testing.T) {
		initial := send(alice, "bob@bob.org", "question for Bob")
		if initial.Status != world.WorldMessageStatusReplied || initial.ReplyBody != "received" {
			t.Fatal(initial)
		}
		manual, err := bob.control.SendWorldMessage(t.Context(), world.SendMessageRequest{
			To: "alice@home", ThreadID: initial.ThreadID, Message: "manual return from Bob", Wait: true,
		})
		if err != nil || manual.Status != world.WorldMessageStatusReplied {
			t.Fatalf("manual return: %+v, %v", manual, err)
		}
		page, err := alice.control.ListWorldMessages(t.Context(), world.ListMessagesParams{ThreadID: initial.ThreadID, Direction: "inbound", Kind: "message"})
		if err != nil || len(page.Items) != 1 || page.Items[0].MessageID != manual.MessageID || page.Items[0].Body != manual.Body {
			t.Fatalf("Alice's inbox: %+v, %v", page, err)
		}
		if msg := send(bob, "alice@home", "new topic"); msg.Status != world.WorldMessageStatusFailed || msg.ErrorCode != "world_target_not_public" || msg.ErrorText != `target "assistant" does not accept messages from other domains` {
			t.Fatal(msg)
		}
	})
	send(alice, "cold@home", "local contact")
	peers, err := alice.control.ListWorldPeers(t.Context())
	if err != nil || len(peers.Recent) != 1 || !strings.HasPrefix(peers.Recent[0].Address, "bob@bob.org") {
		t.Fatal(peers, err)
	}
	clear()
	importCard(home, bobDomain.card)
	federationHTTP(t, "POST", bobDomain.base+domainsPath, nil, worldserver.AddDomainRequest{Card: home.card}, 200)
	state(home, worldserver.DomainMutual)
	state(bobDomain, worldserver.DomainMutual)
	carol, id := node(home, "carol", "home", "assistant")
	waitNode(carol, true)
	federationHTTP(t, "POST", home.base+"/world/v1/admin/nodes/"+id.NodeID+"/revoke", nil, nil, 204)
	revoked := func() {
		federationEventually(t, "revoked node", func() bool {
			status, err := carol.control.GetWorldStatus(t.Context())
			return err == nil && status.Revoked
		})
	}
	revoked()
	carol.stopServe(t)
	carol.startServe(t)
	revoked()

}

// remoteRuntimeFixture 在独立进程运行节点和公共 relay，观察配对 WebSocket 的二进制帧。
type remoteRuntimeFixture struct {
	node                    *conversationManagementFixture
	link, directLink, token string
	captureMu               sync.Mutex
	sent, received          bytes.Buffer
	cutNext                 atomic.Bool
}

func startRemoteRuntime(t *testing.T) *remoteRuntimeFixture {
	t.Setenv("SHELL", "/bin/sh")
	t.Helper()
	f := &remoteRuntimeFixture{}
	binary := buildOpenVaala(t)
	home := newFederationServer(t, "domain")
	addr, cfg := getFreeAddress(t), writeE2EConfigWithoutModels(t)
	appendE2EConfig(t, cfg, "\nworld: {relay_mode: public, server: {domain: relay.test}}\n")
	startE2EProcess(t, binary, cfg, []string{"world", "serve", "--listen", addr}, false)
	upstream, _ := url.Parse("http://" + addr)
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/world/v1/relay/dial" {
			proxy.ServeHTTP(w, r)
			return
		}
		to, _, err := websocket.Dial(r.Context(), "ws://"+addr+r.URL.RequestURI(), nil)
		if err != nil {
			http.Error(w, "unavailable", 503)
			return
		}
		defer to.CloseNow()
		from, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer from.CloseNow()
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		var tasks sync.WaitGroup
		for _, direction := range []struct {
			src, dst *websocket.Conn
			data     *bytes.Buffer
		}{{from, to, &f.sent}, {to, from, &f.received}} {
			direction.src.SetReadLimit(worldserver.ChannelFrameLimit)
			tasks.Go(func() {
				defer cancel()
				for {
					kind, data, err := direction.src.Read(ctx)
					if err != nil {
						return
					}
					if kind != websocket.MessageBinary {
						t.Error("non-binary relay payload")
						return
					}
					f.captureMu.Lock()
					direction.data.Write(data)
					f.captureMu.Unlock()
					if direction.src == from && f.cutNext.Swap(false) {
						return
					}
					if direction.dst.Write(ctx, kind, data) != nil {
						return
					}
				}
			})
		}
		tasks.Wait()
	}))
	t.Cleanup(endpoint.Close)
	model := newE2EModel(t)
	b := &conversationManagementFixture{binary: binary, config: writeE2EConfig(t, model.server.URL), model: model}
	settings, _, err := config.LoadConfigSettings(&config.LoadConfigOpt{Home: filepath.Dir(b.config), Path: b.config})
	if err != nil {
		t.Fatal(err)
	}
	home.register(t, filepath.Join(settings.GetDataDirectory(), "world"), "bob", "team")
	page := httptest.NewServer(http.RedirectHandler(upstream.String()+"/web/v1/", http.StatusFound))
	t.Cleanup(page.Close)
	appendE2EConfig(t, b.config, fmt.Sprintf("\nworld:\n  remote_access: {enabled: true}\n  web_url: %q\n  relays: [%q]\n  node: {enabled: true, name: bob, domain: %s, server_url: %q}\n", page.URL, endpoint.URL, federationDomain, home.url))
	b.startServe(t)
	t.Cleanup(b.stopControl)
	federationEventually(t, "public attachment", func() bool {
		status, err := b.control.GetWorldStatus(t.Context())
		f.link = status.RemoteAccessURL
		return err == nil && f.link != "" && len(status.RelayAttachments) == 2 && status.RelayAttachments[1].Connected
	})
	home.stop()
	page.Close()
	if response, err := http.Get(f.link); err == nil {
		response.Body.Close()
		t.Fatal("Web page still reachable after its listener stopped")
	}
	conn, err := getE2EServerConnection(getE2EServerConnectionPath(b.config))
	if err != nil {
		t.Fatal(err)
	}
	target, err := world.ParseRemoteAccessURL(f.link)
	if err != nil {
		t.Fatal(err)
	}
	target.Entries = worldserver.PeerEntries{DirectURLs: []string{strings.TrimSuffix(conn.URL, "/api/v1/openvaala")}}
	f.directLink, err = world.FormatRemoteAccessURL(page.URL, target)
	if err != nil {
		t.Fatal(err)
	}
	f.node, f.token = b, conn.Token
	return f
}

func (f *remoteRuntimeFixture) connect(t *testing.T, link, token string) runtimeclient.Client {
	t.Helper()
	client, err := runtimeclient.NewClient(runtimeclient.NewClientOpt(link, token))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	waitWorldConnection(t, client)
	return client
}

func waitWorldConnection(t *testing.T, client runtimeclient.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), e2eTimeout)
	defer cancel()
	for {
		select {
		case event, ok := <-client.Events():
			if !ok {
				t.Fatal("client stopped")
			}
			if e, ok := event.(*runtimeclient.ConnectionChanged); ok && e.State == runtimeclient.ConnectionConnected {
				return
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func remoteTerminalOutput(t *testing.T, client runtimeclient.Client, id, marker string) (string, uint64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), e2eTimeout)
	defer cancel()
	var text strings.Builder
	for {
		select {
		case event := <-client.Events():
			if out, ok := event.(*runtimeclient.TerminalOutput); ok && out.TerminalID == id {
				text.Write(out.Data)
				if strings.Contains(text.String(), marker) {
					return text.String(), out.NextCursor
				}
			}
		case <-ctx.Done():
			t.Fatalf("missing %q in %q", marker, text.String())
		}
	}
}

func TestWorldRemoteRuntimeAccess(t *testing.T) {
	f := startRemoteRuntime(t)
	ctx, cancel := context.WithTimeout(t.Context(), e2eTimeout)
	defer cancel()
	f.connect(t, f.directLink, f.token)
	operator := f.connect(t, f.link, f.token)
	record, err := operator.CreateConversation(ctx, &runtimeclient.CreateConversationRequest{AgentName: "assistant"})
	if err != nil {
		t.Fatal(err)
	}
	observer := runtimeObserver{client: operator, conversation: record}
	observer.startTurn(t, "hello from World")
	f.node.model.next(t).reply <- e2eModelReply{text: "world reply"}
	federationEventually(t, "remote model reply", func() bool {
		items, _ := json.Marshal(listRestartItems(t, operator, record))
		return bytes.Contains(items, []byte("world reply"))
	})
	opened, err := operator.OpenTerminal(ctx, &runtimeclient.TerminalOpenRequest{ConversationID: record.ID})
	if err != nil {
		t.Fatal(err)
	}
	marker := fmt.Sprintf("marker-%x", time.Now().UnixNano())
	if err = operator.WriteTerminal(opened.TerminalID, []byte("echo marker-'"+strings.TrimPrefix(marker, "marker-")+"'\n")); err != nil {
		t.Fatal(err)
	}
	remoteTerminalOutput(t, operator, opened.TerminalID, marker+"\r\n")
	legacyLink, _, _ := strings.Cut(f.link, "&token=")
	bad, err := runtimeclient.NewClient(runtimeclient.NewClientOpt(legacyLink, "invalid"))
	if err != nil {
		t.Fatal(err)
	}
	if err = bad.Run(ctx); !errors.Is(err, runtimeclient.ErrUnauthorized) || err.Error() != "unauthorized" {
		t.Fatalf("bad token: %v", err)
	}
	cmd := exec.CommandContext(ctx, f.node.binary, "--config-file", f.node.config, "world", "status", "--url", legacyLink)
	cmd.Env = append(getE2EEnvironment(filepath.Dir(f.node.config)), "OPENVAALA_TOKEN=invalid")
	if output, err := cmd.CombinedOutput(); err == nil || !bytes.Contains(output, []byte("unauthorized")) {
		t.Fatalf("CLI token rejection: %s (%v)", output, err)
	}
	scoped := f.connect(t, legacyLink, sessiontoken.Issue(f.token, record.ID, record.RootSessionID))
	result, err := scoped.ListSessions(ctx, &runtimeclient.ListSessionsRequest{ConversationID: record.ID})
	if err != nil {
		t.Fatalf("session/list: %+v %v", result, err)
	}
	other, err := operator.CreateConversation(ctx, &runtimeclient.CreateConversationRequest{AgentName: "assistant"})
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range []func() error{func() error { _, err := scoped.GetWorldSettings(ctx); return err }, func() error {
		_, err := scoped.ListSessions(ctx, &runtimeclient.ListSessionsRequest{ConversationID: other.ID})
		return err
	}} {
		if err := call(); err == nil || !(strings.Contains(err.Error(), "permission denied") || strings.Contains(err.Error(), "require operator token")) {
			t.Fatalf("scope error: %v", err)
		}
	}
	f.captureMu.Lock()
	defer f.captureMu.Unlock()
	for _, captured := range []*bytes.Buffer{&f.sent, &f.received} {
		if captured.Len() == 0 || bytes.Contains(captured.Bytes(), []byte(marker)) || bytes.Contains(captured.Bytes(), []byte(f.token)) {
			t.Fatal("unexpected relay traffic")
		}
	}
}

func TestWorldRemoteTerminalReconnect(t *testing.T) {
	f := startRemoteRuntime(t)
	client := f.connect(t, f.link, f.token)
	ctx, cancel := context.WithTimeout(t.Context(), e2eTimeout)
	defer cancel()
	directory := t.TempDir()
	opened, err := client.OpenTerminal(ctx, &runtimeclient.TerminalOpenRequest{Directory: directory})
	if err != nil {
		t.Fatal(err)
	}
	id := opened.TerminalID
	if err = client.WriteTerminal(id, []byte("printf 'entry-%d\\n' 1; while [ ! -f resume ]; do sleep 0.05; done; printf 'entry-%d\\n' 2 3\n")); err != nil {
		t.Fatal(err)
	}
	before, cursor := remoteTerminalOutput(t, client, id, "entry-1\r\n")
	f.cutNext.Store(true)
	if _, err = client.GetWorldSettings(ctx); !errors.Is(err, runtimeclient.ErrConnectionLost) {
		t.Fatalf("in-flight request: %v", err)
	}
	if err = os.WriteFile(filepath.Join(directory, "resume"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitWorldConnection(t, client)
	if _, err = client.AttachTerminal(ctx, id, cursor); err != nil {
		t.Fatal(err)
	}
	after, _ := remoteTerminalOutput(t, client, id, "entry-3\r\n")
	for _, line := range []string{"entry-1\r\n", "entry-2\r\n", "entry-3\r\n"} {
		if strings.Count(before+after, line) != 1 {
			t.Fatalf("terminal output: %q", before+after)
		}
	}
}

// federationBinarySocket 让测试直接与指定实例上的目标完成真实握手。
type federationBinarySocket struct{ *websocket.Conn }

func (s federationBinarySocket) ReadFrame(ctx context.Context) ([]byte, error) {
	_, data, err := s.Read(ctx)
	return data, err
}
func (s federationBinarySocket) WriteFrame(ctx context.Context, data []byte) error {
	return s.Write(ctx, websocket.MessageBinary, data)
}
func (s federationBinarySocket) Close() error { return s.CloseNow() }

func TestWorldRelayInstances(t *testing.T) {
	home := newFederationServer(t, "off")
	const count = 3
	servers := make([]*httptest.Server, count)
	instances := make([]string, count)
	stops := make([]func(), count)
	var dials [count]atomic.Int32
	for i := range servers {
		servers[i] = httptest.NewUnstartedServer(nil)
		instances[i] = "ws://" + servers[i].Listener.Addr().String()
	}
	for i, server := range servers {
		router := gin.New()
		relay := worldserver.NewRelay(instances)
		relay.RegisterRoutes(router)
		server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/world/v1/relay/dial" {
				dials[i].Add(1)
			}
			router.ServeHTTP(w, r)
		})
		server.Start()
		t.Cleanup(server.Close)
		stops[i] = runFederationService(t, instances[i], relay.Run)
	}
	// 一个独立入口轮询发现请求，配对必须直达选中的实例。
	var queries atomic.Uint32
	entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/world/v1/relay/instances" {
			http.NotFound(w, r)
			return
		}
		i := (queries.Add(1) - 1) % count
		upstream, _ := url.Parse(servers[i].URL)
		httputil.NewSingleHostReverseProxy(upstream).ServeHTTP(w, r)
	}))
	t.Cleanup(entry.Close)
	a := home.node(t, "alice", "team", home.url, false, entry.URL)
	b := home.node(t, "bob", "team", home.url, false, entry.URL)
	a.start(t)
	b.start(t)
	a.waitPeer(t, "bob", true)

	order := []int{0, 1, 2}
	score := func(i int) []byte {
		sum := sha256.Sum256(append([]byte("openvaala-relay-hrw/v1\x00"+instances[i]+"\x00"), b.identity.SignPublic...))
		return sum[:]
	}
	slices.SortFunc(order, func(i, j int) int { return bytes.Compare(score(j), score(i)) })
	target := base64.RawURLEncoding.EncodeToString(b.identity.SignPublic)
	for rank, i := range order {
		federationEventually(t, fmt.Sprintf("B attachment at rank %d", rank), func() bool {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			socket, _, err := websocket.Dial(ctx, instances[i]+"/world/v1/relay/dial?target="+target, nil)
			if err != nil {
				return false
			}
			ch, err := world.DialSecureChannel(ctx, federationBinarySocket{socket}, nil, b.identity.SignPublic, world.ChannelPurposeRPC)
			if err != nil {
				return rank == 2 && websocket.CloseStatus(err) == 4004
			}
			ch.Close()
			return rank < 2
		})
	}
	for rank, text := range []string{"before outage", "after outage"} {
		if rank == 1 {
			servers[order[0]].Close()
			stops[order[0]]()
		}
		i := order[rank]
		before := dials[i].Load()
		msg := a.send(t, "bob", text, "")
		// 下线与发信可以同时发生，已交给旧通道的信按节点原有的 15 秒周期核对并补投。
		select {
		case call := <-b.model.requests:
			call.answer(text)
		case <-time.After(2*15*time.Second + federationWait):
			t.Fatalf("rank %d: no delivery, outbox=%+v, instance dials=%d", rank, a.outbox(t), dials[i].Load())
		}
		got := a.waitOutbox(t, msg.MessageID, world.WorldMessageStatusReplied)
		if got.ReplyBody != text || dials[i].Load() <= before {
			t.Fatalf("rank %d: reply=%q, dials %d -> %d", rank, got.ReplyBody, before, dials[i].Load())
		}
	}
}

// TestRemoteAccessOnboarding 验证同一身份先独立挂靠，再入域，关闭开关后仍保留节点挂靠。
func TestRemoteAccessOnboarding(t *testing.T) {
	router := gin.New()
	relay := worldserver.NewRelay(nil)
	relay.RegisterRoutes(router)
	var attachments atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/world/v1/relay/attach" {
			attachments.Add(1)
		}
		router.ServeHTTP(w, r)
	}))
	defer endpoint.Close()
	stop := runFederationService(t, "public", relay.Run)
	defer stop()
	model := newE2EModel(t)
	b := &conversationManagementFixture{binary: buildOpenVaala(t), config: writeE2EConfig(t, model.server.URL), model: model}
	appendE2EConfig(t, b.config, fmt.Sprintf("\nworld:\n  remote_access: {enabled: true, name: studio}\n  relays: [%q]\n", endpoint.URL))
	b.startServe(t)
	defer func() { b.stopServe(t) }()
	var original string
	waitStatus := func(joined bool, expectedAttempts int32) {
		federationEventually(t, "remote access attachment", func() bool {
			status, err := b.control.GetWorldStatus(t.Context())
			if err != nil || status.Enabled != joined || (joined && !status.Connected) || status.RemoteAccessURL == "" || attachments.Load() != expectedAttempts {
				return false
			}
			if !strings.HasPrefix(status.RemoteAccessURL, endpoint.URL+"/web/v1/") {
				return false
			}
			for _, r := range status.RemoteAccessRelays {
				if r.Connected && !r.Implicit {
					if original == "" {
						original = status.RemoteAccessURL
					}
					return true
				}
			}
			return false
		})
	}
	waitStatus(false, 1)
	target, err := world.ParseRemoteAccessURL(original)
	if err != nil || target.Name != "studio" || target.Domain != "" || target.Token == "" {
		t.Fatal("standalone target", err)
	}
	verifyList := func() {
		c := (&remoteRuntimeFixture{}).connect(t, original, "")
		if _, err := c.ListConversations(t.Context(), &runtimeclient.ListConversationsRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	verifyList()
	home := newFederationServer(t, "domain")
	opt := config.LoadConfigOpt{Home: filepath.Dir(b.config), Path: b.config}
	cfg, _, err := config.LoadConfigSettings(&opt)
	if err != nil {
		t.Fatal(err)
	}
	id := home.register(t, filepath.Join(cfg.GetDataDirectory(), "world"), "studio", "team")
	if !bytes.Equal(id.SignPublic, target.PublicKey) {
		t.Fatal("joining changed the local key")
	}
	files := config.NewConfigFiles(&opt)
	if err := files.SetWorldNodeRegistration("studio", federationDomain, home.url); err != nil {
		t.Fatal(err)
	}
	b.stopServe(t)
	b.startServe(t)
	waitStatus(true, 2)
	verifyList()
	wire := openExternalWire(t, b.config)
	var reset runtimeprotocol.ResetTokenResult
	wire.call(t, "server/resetToken", nil, &reset)
	old, err := runtimeclient.NewClient(runtimeclient.NewClientOpt(original, ""))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := old.Run(ctx); !errors.Is(err, runtimeclient.ErrUnauthorized) {
		t.Fatalf("old link: %v", err)
	}
	settings, err := files.GetWorldSettings()
	if err != nil {
		t.Fatal(err)
	}
	settings.RemoteAccess.Enabled = false
	update, err := files.PrepareWorldSettings(settings, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := files.SaveWorldSettings(update, nil, ""); err != nil {
		t.Fatal(err)
	}
	b.stopServe(t)
	b.startServe(t)
	federationEventually(t, "node-only attachment", func() bool {
		s, err := b.control.GetWorldStatus(t.Context())
		return err == nil && s.Enabled && s.RemoteAccessURL == "" && len(s.RelayAttachments) == 2 && s.RelayAttachments[1].Connected && attachments.Load() == 3
	})
	var dialer world.PeerDialer
	channel, err := dialer.DialPeer(t.Context(), "studio", target.PublicKey, target.Entries, nil, world.ChannelPurposeRPC)
	if err == nil {
		defer channel.Close()
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if err := channel.WriteMessage(ctx, []byte(fmt.Sprintf(`{"token":%q}`, reset.Token))); err == nil {
			if _, err := channel.ReadMessage(ctx); err == nil {
				t.Fatal("disabled RPC accepted token")
			}
		}
	}
}
