package world

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/coder/websocket"
)

func testWorldCall(name string, args any) *defs.Item {
	// 1. 使用 Agent 交给工具的标准 Item。
	payload, _ := json.Marshal(args)
	return &defs.Item{Kind: defs.ItemToolCall, ToolCall: &defs.ToolCall{CallID: "test-call", Name: name, Arguments: string(payload)}}
}

func TestWorldTools(t *testing.T) {
	for _, tc := range []struct {
		name string
		hops int
	}{
		{"forward to next contact", 3}, {"forward limit", 1}, {"offline enqueue", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 1. A 投递给 B，取得真实来信线程所绑定的对话。
			f := newTestFederation(t)
			a := f.node(t, "alice", "")
			b := f.node(t, "bob", "")
			c := f.node(t, "carol", "")
			b.start(t)
			b.waitPeer(t, "alice", false)
			var source defs.ConversationID = "outgoing"
			if tc.hops > 0 {
				ch := testDialDirect(t, a, b)
				writeWireMessage(t, ch, wireMessage{Kind: "message", MessageID: "first", ThreadID: "first-thread", From: Address{Node: "alice", Domain: testDomain}, To: Address{Node: "bob", Domain: testDomain}, Body: "forward", HopLimit: tc.hops})
				if got := readWireMessage(t, ch); got.Status != "accepted" {
					t.Fatalf("incoming %+v", got)
				}
				if got := readWireMessage(t, ch); got.Kind != "reply" {
					t.Fatalf("reply %+v", got)
				}
				threadBinding, ok, err := getRecord[ThreadBinding](t.Context(), b.opt.Records, worldThreads, "first-thread")
				if err != nil || !ok {
					t.Fatal(err)
				}
				source = defs.ConversationID(threadBinding.ConversationID)
				c.start(t)
				b.waitPeer(t, "carol", true)
			} else {
				b.waitPeer(t, "carol", false)
			}
			// 2. 通过绑定工具转发到 C，并核对可见联系人与查询结果。
			tools, err := b.node.GetTools(t.Context(), source)
			if err != nil {
				t.Fatal(err)
			}
			byName := map[string]executor.Tool{}
			for _, tool := range tools {
				byName[tool.GetDefinition().Name] = tool
			}
			result := byName["list_world_peers"].Execute(t.Context(), testWorldCall("list_world_peers", struct{}{}), nil)
			if result.Result.Error != nil || len(decodeWorld[struct{ Peers []WorldPeer }](t, result.Result.Content).Peers) != 2 {
				t.Fatalf("list: %+v", result.Result)
			}
			result = byName["send_world_message"].Execute(t.Context(), testWorldCall("send_world_message", map[string]string{"to": "carol", "message": "continue"}), nil)
			if tc.hops == 1 {
				if result.Result.Error == nil || result.Result.Error.Code != "world_hop_limit_exceeded" || c.model.calls.Load() != 0 {
					t.Fatalf("hop: %+v", result.Result)
				}
				return
			}
			if result.Result.Error != nil {
				t.Fatal(result.Result.Error)
			}
			answer := decodeWorld[map[string]string](t, result.Result.Content)
			status := WorldMessageStatusReplied
			if tc.hops == 0 {
				status = WorldMessageStatusPendingOffline
			}
			outboundMessage := b.waitOutbox(t, answer["message_id"], status)
			if answer["status"] != string(status) || (tc.hops > 0 && (outboundMessage.HopLimit != tc.hops-1 || answer["reply"] != "reply-1")) {
				t.Fatalf("forward: %+v, %+v", answer, outboundMessage)
			}
			for _, args := range []map[string]string{{"message_id": outboundMessage.MessageID}, {"thread_id": outboundMessage.ThreadID}} {
				got := byName["get_world_message"].Execute(t.Context(), testWorldCall("get_world_message", args), nil)
				if got.Result.Error != nil {
					t.Fatal(got.Result.Error)
				}
			}
		})
	}
}

func TestWorldGetMessageScope(t *testing.T) {
	node := newTestFederation(t).node(t, "alice", "")
	const conversation = "source"
	const thread = "shared"
	for _, message := range []WorldMessage{
		{MessageID: "own", ThreadID: thread, ConversationID: conversation, Direction: WorldMessageDirectionOutbound, Status: WorldMessageStatusReplied, ReplyBody: "answer"},
		{MessageID: "inbound", ThreadID: thread, ConversationID: conversation, Direction: WorldMessageDirectionInbound, Status: WorldMessageStatusCompleted},
		{MessageID: "other", ThreadID: thread, ConversationID: "other", Direction: WorldMessageDirectionOutbound, Status: WorldMessageStatusReplied},
	} {
		if err := putRecord(t.Context(), node.opt.Records, worldMessages, message.MessageID, 1, message); err != nil {
			t.Fatal(err)
		}
	}
	tools, err := node.node.GetTools(t.Context(), conversation)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		field, value string
		notFound     bool
	}{
		{"message_id", "own", false},
		{"message_id", "inbound", true},
		{"message_id", "other", true},
		{"thread_id", thread, false},
	} {
		t.Run(tc.field+"/"+tc.value, func(t *testing.T) {
			result := tools[2].Execute(t.Context(), testWorldCall("get_world_message", map[string]string{tc.field: tc.value}), nil).Result
			if tc.notFound {
				if result.Error == nil || result.Error.Code != "world_message_not_found" {
					t.Fatalf("result = %+v, want message not found", result)
				}
				return
			}
			if result.Error != nil {
				t.Fatal(result.Error)
			}
			if tc.field == "thread_id" {
				messages := decodeWorld[[]WorldMessage](t, result.Content)
				if len(messages) != 1 || messages[0].MessageID != "own" || messages[0].Direction != WorldMessageDirectionOutbound {
					t.Fatalf("thread messages = %+v", messages)
				}
				return
			}
			message := decodeWorld[map[string]string](t, result.Content)
			if message["message_id"] != "own" || message["thread_id"] != thread || message["status"] != "replied" || message["reply"] != "answer" {
				t.Fatalf("message summary = %+v", message)
			}
		})
	}
}

func TestWorldToolWaitDeadline(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		name := "no ack"
		if accepted {
			name = "accepted"
		}
		t.Run(name, func(t *testing.T) {
			// 1. 对端按场景发送确认，始终保持通道打开。
			sockets := make(chan *websocket.Conn, 1)
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
			tools, err := a.node.GetTools(t.Context(), "source")
			if err != nil {
				t.Fatal(err)
			}
			results := make(chan *executor.ToolExecutionResult, 1)
			go func() {
				results <- tools[1].Execute(t.Context(), testWorldCall("send_world_message", map[string]string{"to": "bob", "message": "test"}), nil)
			}()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var socket *websocket.Conn
			select {
			case socket = <-sockets:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			acceptedChannel, err := AcceptSecureChannel(ctx, &directFrameTransport{socket}, b.opt.Identity)
			ch := acceptedChannel.Channel
			if err != nil {
				t.Fatal(err)
			}
			defer ch.Close()
			msg := readWireMessage(t, ch)
			if accepted {
				writeWireMessage(t, ch, wireMessage{Kind: "ack", MessageID: "ack", ThreadID: msg.ThreadID, From: msg.To, To: msg.From, InReplyTo: msg.MessageID, Status: "accepted", HopLimit: 1})
			}
			// 2. 仅已确认的信可以处于 remote_running。
			want := WorldMessageStatusSending
			if accepted {
				want = WorldMessageStatusRemoteRunning
			}
			select {
			case got := <-results:
				if got.Result.Error != nil || decodeWorld[map[string]string](t, got.Result.Content)["status"] != string(want) {
					t.Fatalf("wait result: %+v", got.Result)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			outboundMessage := a.waitOutbox(t, msg.MessageID, want)
			if !outboundMessage.NotifyConversation {
				t.Fatalf("timeout: %+v", outboundMessage)
			}
		})
	}
}
