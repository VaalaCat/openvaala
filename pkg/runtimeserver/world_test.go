package runtimeserver_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/VaalaCat/openvaala/pkg/runtimeserver"
	"github.com/VaalaCat/openvaala/pkg/sessiontoken"
	"github.com/VaalaCat/openvaala/pkg/world"
	"github.com/VaalaCat/openvaala/pkg/worldserver"
	"github.com/gin-gonic/gin"
)

func TestWorldRPCContacts(t *testing.T) {
	for _, tc := range []struct {
		name        string
		admin       bool
		registered  world.Address
		wantAddress string
	}{
		{name: "disabled"},
		{name: "server only", admin: true},
		{name: "registered with node disabled", registered: world.Address{Node: "bob", Domain: "partner.test"}, wantAddress: "bob@partner.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := startTestServer(t, func(opt *runtimeserver.RuntimeServerOpt) {
				opt.WorldRunning.ServerEnabled = tc.admin
				opt.RegisteredNode = tc.registered
			})
			client := h.dial(t)
			client.initialize(t)

			for _, method := range []string{"world/admin/getACL", "world/admin/listDomains", "world/admin/addDomain", "world/admin/acceptDomain", "world/admin/rejectDomain", "world/admin/deleteDomain"} {
				response := client.call(t, method, nil)
				if response.Error == nil || response.Error.Message != "world administration is not enabled" || response.Error.Data == nil || response.Error.Data.Code != "world_request_failed" {
					t.Fatalf("%s: %+v", method, response)
				}
			}
			var status world.NodeStatus
			client.callResult(t, "world/getStatus", nil, &status)
			if status.Enabled || status.AdminEnabled != tc.admin || status.NodeAddr != tc.wantAddress {
				t.Fatalf("world status = %+v", status)
			}
			for _, method := range []string{
				"world/listPeers", "world/sendMessage", "world/forward", "world/listMessages",
				"world/cancelMessage", "world/listThreads", "world/subscribe",
			} {
				t.Run(method, func(t *testing.T) {
					response := client.call(t, method, nil)
					if response.Error == nil || response.Error.Code != runtimeprotocol.ErrorCodeRuntime ||
						response.Error.Message != "world node is not enabled" ||
						response.Error.Data == nil || response.Error.Data.Code != "world_request_failed" {
						t.Fatalf("response = %+v", response)
					}
				})
			}
		})
	}
}

func TestWorldRPCSubscription(t *testing.T) {
	events := []world.NodeNotification{
		{Event: "status_changed", Data: json.RawMessage(`true`)},
		{Event: "peers_changed", Data: json.RawMessage(`[{"address":"bob@example.test"}]`)},
		{Event: "message_updated", Data: json.RawMessage(`{"conversation_id":"c1","message_id":"m1"}`)},
	}
	node := &worldSubscriptionNode{events: events, released: make(chan struct{}, 2)}
	h := startTestServer(t, func(opt *runtimeserver.RuntimeServerOpt) { opt.WorldNode = node })
	operator := h.dial(t)
	operator.initialize(t)
	record := operator.createConversation(t)
	token := sessiontoken.Issue(testToken, record.ID, record.RootSessionID)
	socket, _, err := h.dialWith(t, "/api/v1/openvaala", http.Header{"Authorization": {"Bearer " + token}})
	if err != nil {
		t.Fatal(err)
	}
	scoped := newTestClient(t, socket)
	scoped.initialize(t)
	for _, method := range runtimeprotocol.Methods {
		if !strings.HasPrefix(method.Name, "world/") {
			continue
		}
		t.Run(method.Name, func(t *testing.T) {
			response := scoped.call(t, method.Name, nil)
			if response.Error == nil || response.Error.Message != "permission denied" {
				t.Fatalf("response = %+v", response)
			}
		})
	}

	for _, stop := range []string{"unsubscribe", "disconnect"} {
		t.Run(stop, func(t *testing.T) {
			response := operator.callResult(t, "world/subscribe", nil, nil)
			for _, want := range events {
				select {
				case update := <-operator.notifications:
					var got world.NodeNotification
					if err := json.Unmarshal(update.Params, &got); err != nil {
						t.Fatal(err)
					}
					if update.sequence <= response.sequence || update.Method != "world/messageUpdated" ||
						got.Event != want.Event || string(got.Data) != string(want.Data) {
						t.Fatalf("world update = %+v, want %+v after response %d", update, want, response.sequence)
					}
				case <-time.After(time.Second):
					t.Fatalf("missing world update %s", want.Event)
				}
			}
			if stop == "unsubscribe" {
				operator.callResult(t, "world/unsubscribe", nil, nil)
				operator.callResult(t, "world/unsubscribe", nil, nil)
				select {
				case update := <-operator.notifications:
					t.Fatalf("world notification after unsubscribe: %+v", update)
				default:
				}
			} else {
				_ = operator.conn.CloseNow()
			}
			select {
			case <-node.released:
			case <-time.After(time.Second):
				t.Fatal("World subscription still active")
			}
			select {
			case update := <-scoped.notifications:
				t.Fatalf("session token received a World notification: %+v", update)
			default:
			}
		})
	}
}

type worldSubscriptionNode struct {
	world.WorldNode
	events   []world.NodeNotification
	released chan struct{}
}

func (n *worldSubscriptionNode) Subscribe(ctx context.Context) (<-chan world.NodeNotification, error) {
	events := make(chan world.NodeNotification, len(n.events))
	for _, event := range n.events {
		events <- event
	}
	context.AfterFunc(ctx, func() {
		close(events)
		n.released <- struct{}{}
	})
	return events, nil
}

func TestWorldRPCSubscriptionOverflow(t *testing.T) {
	for _, name := range []string{"resubscribe after overflow"} {
		t.Run(name, func(t *testing.T) {
			node := &worldOverflowNode{overflow: true}
			h := startTestServer(t, func(opt *runtimeserver.RuntimeServerOpt) { opt.WorldNode = node })
			client := h.dial(t)
			client.initialize(t)
			client.callResult(t, "world/subscribe", nil, nil)
			for _, method := range []string{"world/messageUpdated", "world/messageUpdatedClosed"} {
				select {
				case frame := <-client.notifications:
					if frame.Method != method {
						t.Fatalf("notification = %s, want %s", frame.Method, method)
					}
				case <-time.After(time.Second):
					t.Fatalf("missing %s", method)
				}
			}
			client.callResult(t, "world/subscribe", nil, nil)
			select {
			case frame := <-client.notifications:
				if frame.Method != "world/messageUpdated" {
					t.Fatalf("update after resubscribe = %s", frame.Method)
				}
			case <-time.After(time.Second):
				t.Fatal("missing update after resubscribe")
			}
		})
	}
}

type worldOverflowNode struct {
	world.WorldNode
	overflow bool
}

func (n *worldOverflowNode) Subscribe(ctx context.Context) (<-chan world.NodeNotification, error) {
	events := make(chan world.NodeNotification, 1)
	events <- world.NodeNotification{Event: "status_changed", Data: json.RawMessage(`true`)}
	if n.overflow {
		n.overflow = false
		select {
		case events <- world.NodeNotification{Event: "peers_changed", Data: json.RawMessage(`[]`)}:
		default:
			close(events)
		}
	} else {
		context.AfterFunc(ctx, func() { close(events) })
	}
	return events, nil
}

func TestWorldRPCSendWaitKeepsConnectionAvailable(t *testing.T) {
	for _, tc := range []struct {
		name string
		wait bool
	}{
		{"wait for reply", true},
		{"return after enqueue", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := &worldSendNode{enqueued: make(chan struct{}), finished: make(chan world.WorldMessage, 1)}
			h := startTestServer(t, func(opt *runtimeserver.RuntimeServerOpt) { opt.WorldNode = node })
			client := h.dial(t)
			client.initialize(t)
			client.sendRaw(t, fmt.Sprintf(`{"id":100,"method":"world/sendMessage","params":{"wait":%t}}`, tc.wait))
			select {
			case <-node.enqueued:
			case <-time.After(time.Second):
				t.Fatal("message was not enqueued")
			}
			if !tc.wait {
				response := client.nextResponse(t)
				if string(response.ID) != "100" || response.Error != nil {
					t.Fatalf("send response = %+v", response)
				}
				return
			}
			client.sendRaw(t, `{"id":101,"method":"world/getStatus","params":{}}`)
			select {
			case response := <-client.responses:
				if string(response.ID) != "101" || response.Error != nil {
					t.Fatalf("status response = %+v", response)
				}
			case <-time.After(time.Second):
				t.Fatal("getStatus blocked by world/sendMessage")
			}
			node.finished <- world.WorldMessage{MessageID: "sent", Status: world.WorldMessageStatusReplied}
			response := client.nextResponse(t)
			var result world.WorldMessage
			if err := json.Unmarshal(response.Result, &result); err != nil || string(response.ID) != "100" ||
				response.Error != nil || result.Status != world.WorldMessageStatusReplied {
				t.Fatalf("send response = %+v, message = %+v, decode = %v", response, result, err)
			}
		})
	}
}

type worldSendNode struct {
	world.WorldNode
	enqueued chan struct{}
	finished chan world.WorldMessage
}

func (n *worldSendNode) GetStatus(context.Context, string) (world.NodeStatus, error) {
	return world.NodeStatus{Enabled: true}, nil
}

func (n *worldSendNode) SendMessage(ctx context.Context, req world.SendMessageRequest) (world.WorldMessage, error) {
	message, finished, err := n.EnqueueMessage(ctx, req)
	if err != nil || finished == nil {
		return message, err
	}
	return n.WaitMessage(ctx, message, finished)
}

func (n *worldSendNode) EnqueueMessage(_ context.Context, req world.SendMessageRequest) (world.WorldMessage, <-chan world.WorldMessage, error) {
	close(n.enqueued)
	message := world.WorldMessage{MessageID: "sent", Status: world.WorldMessageStatusSending}
	if req.Wait {
		return message, n.finished, nil
	}
	return message, nil, nil
}

func (n *worldSendNode) WaitMessage(ctx context.Context, _ world.WorldMessage, finished <-chan world.WorldMessage) (world.WorldMessage, error) {
	select {
	case message := <-finished:
		return message, nil
	case <-ctx.Done():
		return world.WorldMessage{}, ctx.Err()
	}
}

func TestWorldAdminQueries(t *testing.T) {
	opt := worldserver.NewWorldServerOpt("team.test", filepath.Join(t.TempDir(), "db", "world.db"), "world-admin")
	opt.StaticACL = []worldserver.ACLRule{{From: []string{"owner:alice"}, To: []string{"*"}, Action: "allow"}}
	control, err := worldserver.NewWorldServer(opt)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- control.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	if _, err := control.GetNodes(ctx); err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	control.RegisterRoutes(router)
	httpServer := httptest.NewServer(router)
	defer httpServer.Close()
	h := startTestServer(t, func(opt *runtimeserver.RuntimeServerOpt) { opt.WorldServer = control })
	operator := h.dial(t)
	operator.initialize(t)
	for _, tc := range []struct{ rpc, method, path, want string }{
		{"listUsers", "GET", "users", `{"users":[{"owner":"alice","node_count":0,"online_count":0,"token_count":0,"rule_count":1}]}`},
		{"getACLMatrix", "POST", "acl/matrix", `{"nodes":[],"cells":[]}`},
	} {
		t.Run(tc.rpc, func(t *testing.T) {
			response := operator.callResult(t, "world/admin/"+tc.rpc, nil, nil)
			var rpc, want any
			if err := json.Unmarshal(response.Result, &rpc); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tc.want), &want); err != nil || !reflect.DeepEqual(rpc, want) {
				t.Fatalf("RPC result = %s, want %s", response.Result, tc.want)
			}
			for _, auth := range []struct {
				token  string
				status int
			}{{"wrong", 401}, {"world-admin", 200}} {
				req, err := http.NewRequestWithContext(ctx, tc.method, httpServer.URL+"/world/v1/admin/"+tc.path, strings.NewReader("{}"))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+auth.token)
				resp, err := httpServer.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				if resp.StatusCode != auth.status {
					t.Fatalf("HTTP %d, want %d", resp.StatusCode, auth.status)
				}
				if auth.status == 200 {
					var result any
					if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || !reflect.DeepEqual(result, rpc) {
						t.Fatalf("HTTP = %+v, RPC = %+v, err = %v", result, rpc, err)
					}
				}
			}
		})
	}
}
