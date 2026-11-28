package world

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/pkg/storage/sqlite"
	"github.com/VaalaCat/openvaala/pkg/worldserver"
)

type testBridgeHost struct {
	input  *io.PipeWriter
	output *io.PipeReader
	cancel context.CancelFunc
	done   chan error
	frames chan bridgeFrame
}

func newTestBridgeOpt(t *testing.T, f *testFederation) *WorldNodeOpt {
	t.Helper()
	db, err := sqlite.OpenDatabase(sqlite.NewMemoryDatabaseOpt(WorldNamespaces()...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	opt := NewWorldNodeOpt(db.Record(), f.register(t, "bridge"), "bridge", testDomain, f.url)
	opt.DefaultTarget = "host-agent"
	opt.AdvertiseTargets = []worldserver.AdvertisedTarget{{Name: opt.DefaultTarget, Visibility: "domain"}}
	opt.DefaultWait = 5 * time.Second
	return opt
}

func seedBridgeMessage(t *testing.T, opt *WorldNodeOpt, msg WorldMessage) {
	t.Helper()
	msg.CreatedAt = time.Now().UnixMilli()
	msg.UpdatedAt = msg.CreatedAt
	node := &worldNodeImpl{opt: *opt}
	if err := node.saveThreadMessage(t.Context(), &msg); err != nil {
		t.Fatal(err)
	}
}

func startTestBridge(t *testing.T, opt *WorldNodeOpt) *testBridgeHost {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	input, hostWrite := io.Pipe()
	hostRead, output := io.Pipe()
	host := &testBridgeHost{hostWrite, hostRead, cancel, make(chan error, 1), make(chan bridgeFrame, 32)}
	go func() { host.done <- RunBridge(ctx, opt, input, output); close(host.done) }()
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		defer close(host.frames)
		reader := bufio.NewReader(hostRead)
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				return
			}
			var frame bridgeFrame
			if err := json.Unmarshal(line, &frame); err != nil || frame.Version != "2.0" {
				t.Errorf("stdout did not contain a JSON-RPC frame: %q (%v)", line[:min(len(line), 100)], err)
				return
			}
			select {
			case host.frames <- frame:
			case <-ctx.Done():
				return
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		hostWrite.Close()
		hostRead.Close()
		host.wait(t)
		<-readDone
	})
	return host
}

func (h *testBridgeHost) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-h.done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("bridge did not exit")
		return nil
	}
}

func (h *testBridgeHost) write(t *testing.T, value any) {
	t.Helper()
	if err := json.NewEncoder(h.input).Encode(value); err != nil {
		t.Fatal(err)
	}
}

func (h *testBridgeHost) request(t *testing.T, id, method string, params any) {
	t.Helper()
	h.write(t, bridgeRequest{Version: "2.0", ID: json.RawMessage(id), Method: method, Params: params})
}

func (h *testBridgeHost) next(t *testing.T) bridgeFrame {
	t.Helper()
	for {
		select {
		case frame, ok := <-h.frames:
			if !ok {
				t.Fatal("bridge stdout closed")
			}
			if string(frame.Method) != `"world/event"` {
				return frame
			}
			event := decodeWorld[struct {
				Event string          `json:"event"`
				Data  json.RawMessage `json:"data"`
			}](t, frame.Params)
			if frame.ID != nil || event.Data == nil || !slices.Contains([]string{"status_changed", "peers_changed", "message_updated"}, event.Event) {
				t.Fatalf("invalid world/event: %+v", frame)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("timed out waiting for JSON-RPC frame")
		}
	}
}

func (h *testBridgeHost) initialize(t *testing.T) {
	t.Helper()
	h.request(t, `"init"`, "world/initialize", map[string]any{"protocol_version": 1, "host_name": "test host"})
	result := decodeWorld[struct {
		ProtocolVersion int                            `json:"protocol_version"`
		NodeAddr        string                         `json:"node_addr"`
		DefaultTarget   string                         `json:"default_target"`
		Targets         []worldserver.AdvertisedTarget `json:"targets"`
	}](t, expectBridgeResponse(t, h.next(t), `"init"`))
	if result.ProtocolVersion != 1 || result.NodeAddr != "bridge@"+testDomain ||
		result.DefaultTarget != "host-agent" || len(result.Targets) != 1 || result.Targets[0].Name != result.DefaultTarget {
		t.Fatalf("initialize result = %+v", result)
	}
}

func expectBridgeResponse(t *testing.T, frame bridgeFrame, id string) json.RawMessage {
	t.Helper()
	if string(frame.ID) != id || frame.Method != nil || frame.Error != nil || frame.Result == nil {
		t.Fatalf("expected success for id %s, got id=%s method=%s error=%s", id, frame.ID, frame.Method, frame.Error)
	}
	return frame.Result
}

func expectBridgeError(t *testing.T, frame bridgeFrame, id string, code int, businessCode string) {
	t.Helper()
	if frame.Method != nil || string(frame.ID) != id || frame.Result != nil {
		t.Fatalf("expected error for id %s, got %+v", id, frame)
	}
	err := decodeWorld[bridgeError](t, frame.Error)
	if err.Code != code || err.Message == "" {
		t.Fatalf("unexpected RPC error: %+v", err)
	}
	if businessCode != "" && decodeWorld[map[string]string](t, err.Data)["code"] != businessCode {
		t.Fatalf("business error = %s, want %s", err.Data, businessCode)
	}
}

func expectBridgeCallback(t *testing.T, frame bridgeFrame, method string) {
	t.Helper()
	if string(frame.Method) != fmt.Sprintf("%q", method) || frame.ID == nil {
		t.Fatalf("expected %s callback, got method=%s id=%s", method, frame.Method, frame.ID)
	}
}

func TestBridgeMessages(t *testing.T) {
	f := newTestFederation(t)
	opt := newTestBridgeOpt(t, f)
	f.register(t, "offline")
	host := startTestBridge(t, opt)
	host.request(t, "1", "world/status", struct{}{})
	expectBridgeError(t, host.next(t), "1", -32001, "")
	host.request(t, "2", "world/initialize", map[string]int{"protocol_version": 2})
	expectBridgeError(t, host.next(t), "2", -32602, "")
	host.initialize(t)

	awaitWorld(t, func() bool {
		host.request(t, "3", "world/peers", struct{}{})
		result := decodeWorld[struct{ Peers []WorldPeer }](t, expectBridgeResponse(t, host.next(t), "3"))
		return slices.ContainsFunc(result.Peers, func(peer WorldPeer) bool { return peer.NodeName == "offline" && !peer.Online })
	})

	body := strings.Repeat("\x01", maxWorldMessageBodyBytes)
	var sent []WorldMessage
	for _, id := range []string{"4", "5"} {
		host.request(t, id, "world/send", map[string]any{
			"to": "offline", "message": body, "source_conversation_id": "source", "notify": true,
		})
		msg := decodeWorld[WorldMessage](t, expectBridgeResponse(t, host.next(t), id))
		if msg.Body != body || msg.Status != WorldMessageStatusPendingOffline || !msg.NotifyConversation {
			t.Fatalf("large message failed: bytes=%d status=%s notify=%v", len(msg.Body), msg.Status, msg.NotifyConversation)
		}
		sent = append(sent, msg)
	}
	older, newer := sent[0], sent[1]
	inbound := WorldMessage{MessageID: "inbound", ThreadID: "inbound-thread", PeerAddr: "offline@" + testDomain, Direction: WorldMessageDirectionInbound, ConversationID: "source", Status: WorldMessageStatusCompleted}
	seedBridgeMessage(t, opt, inbound)
	for _, tc := range []struct {
		name, messageID string
		source          *string
		found           bool
	}{
		{"unscoped outbound", older.MessageID, nil, true},
		{"matching source", older.MessageID, new("source"), true},
		{"different source", older.MessageID, new("other"), false},
		{"empty source", older.MessageID, new(""), false},
		{"inbound without source", inbound.MessageID, nil, true},
		{"inbound with source", inbound.MessageID, new("source"), false},
		{"missing ID", "absent", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params := map[string]string{"message_id": tc.messageID}
			if tc.source != nil {
				params["source_conversation_id"] = *tc.source
			}
			host.request(t, "6", "world/message", params)
			frame := host.next(t)
			if !tc.found {
				expectBridgeError(t, frame, "6", -32000, "world_message_not_found")
				return
			}
			result := decodeWorld[WorldMessage](t, expectBridgeResponse(t, frame, "6"))
			if result.MessageID != tc.messageID {
				t.Fatalf("message ID = %q, want %q", result.MessageID, tc.messageID)
			}
		})
	}

	params := map[string]any{"direction": "outbound", "peer_addr": []string{"offline@" + testDomain}, "limit": 1}
	host.request(t, "7", "world/messages", params)
	page := decodeWorld[listPage[WorldMessage]](t, expectBridgeResponse(t, host.next(t), "7"))
	if len(page.Items) != 1 || page.Items[0] != newer || page.NextCursor.ID != newer.MessageID {
		t.Fatalf("first page: items=%d, cursor=%q", len(page.Items), page.NextCursor.ID)
	}
	params["cursor_id"] = page.NextCursor.ID
	host.request(t, "8", "world/messages", params)
	page = decodeWorld[listPage[WorldMessage]](t, expectBridgeResponse(t, host.next(t), "8"))
	if len(page.Items) != 1 || page.Items[0] != older || page.NextCursor.ID != "" {
		t.Fatalf("second page: items=%d, cursor=%q", len(page.Items), page.NextCursor.ID)
	}
	host.request(t, "9", "world/messages", map[string]string{"direction": "outbound"})
	page = decodeWorld[listPage[WorldMessage]](t, expectBridgeResponse(t, host.next(t), "9"))
	if !slices.Equal(page.Items, []WorldMessage{newer, older}) || page.NextCursor.ID != "" {
		t.Fatalf("large multi-item page: items=%d, cursor=%q", len(page.Items), page.NextCursor.ID)
	}

	host.request(t, "10", "world/cancel", map[string]string{"message_id": older.MessageID})
	if cancelled := decodeWorld[WorldMessage](t, expectBridgeResponse(t, host.next(t), "10")); cancelled.Status != WorldMessageStatusCancelled {
		t.Fatalf("cancel returned status %s", cancelled.Status)
	}
	host.request(t, "", "world/unknown-notification", struct{}{})
	host.request(t, "11", "world/status", struct{}{})
	if status := decodeWorld[NodeStatus](t, expectBridgeResponse(t, host.next(t), "11")); status.PendingOutboxCount != 1 || status.RemoteAccessURL != "" {
		t.Fatalf("node status: %+v", status)
	}
}

func TestBridgeNestedDelivery(t *testing.T) {
	for _, tc := range []struct {
		name    string
		hops    int
		wantErr string
	}{
		{"decrements inherited budget", 5, ""},
		{"rejects exhausted budget", 1, "world_hop_limit_exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTestFederation(t)
			a, c := f.node(t, "alice", ""), f.node(t, "charlie", "")
			host := startTestBridge(t, newTestBridgeOpt(t, f))
			host.initialize(t)
			a.start(t)
			c.start(t)
			a.waitPeer(t, "bridge", true)
			a.waitPeer(t, "charlie", true)
			c.waitPeer(t, "bridge", true)
			awaitWorld(t, func() bool {
				host.request(t, "90", "world/peers", struct{}{})
				peers := decodeWorld[struct{ Peers []WorldPeer }](t, expectBridgeResponse(t, host.next(t), "90"))
				return slices.ContainsFunc(peers.Peers, func(p WorldPeer) bool { return p.NodeName == "charlie" && p.Online })
			})
			var source string
			if tc.hops == 1 {
				source = "previous-hop"
				binding := ThreadBinding{ThreadID: "previous-thread", ConversationID: source, Direction: "inbound", RemainingHops: 2}
				if err := putRecord(t.Context(), a.opt.Records, worldThreads, binding.ThreadID, 1, binding); err != nil {
					t.Fatal(err)
				}
			}
			msg := a.send(t, SendMessageRequest{To: "bridge", Message: "call the host", SourceConversationID: source})
			open := host.next(t)
			expectBridgeCallback(t, open, "world/open")
			if string(open.ID) != "1" {
				t.Fatalf("first bridge callback ID = %s", open.ID)
			}
			// 宿主的 id=1 请求与尚未完成的 bridge id=1 请求并行。
			host.request(t, "1", "world/peers", struct{}{})
			peers := decodeWorld[struct{ Peers []WorldPeer }](t, expectBridgeResponse(t, host.next(t), "1"))
			if !slices.ContainsFunc(peers.Peers, func(p WorldPeer) bool { return p.NodeName == "charlie" && p.Online }) {
				t.Fatalf("bridge peers = %+v", peers)
			}
			host.write(t, bridgeResponse{Version: "2.0", ID: open.ID, Result: OpenedSession{SessionID: "bridge-session"}})
			deliver := host.next(t)
			expectBridgeCallback(t, deliver, "world/deliver")
			input := decodeWorld[struct {
				SessionID string `json:"session_id"`
				MessageID string `json:"message_id"`
				HopLimit  int    `json:"hop_limit"`
				Target    string `json:"target"`
			}](t, deliver.Params)
			if input.SessionID != "bridge-session" || input.MessageID != msg.MessageID || input.HopLimit != tc.hops || input.Target != "host-agent" {
				t.Fatalf("deliver params = %+v", input)
			}
			host.request(t, "2", "world/message", map[string]string{"message_id": msg.MessageID})
			running := decodeWorld[WorldMessage](t, expectBridgeResponse(t, host.next(t), "2"))
			if running.Status != WorldMessageStatusRunning || running.ConversationID != input.SessionID {
				t.Fatalf("inbound state before execution = %+v", running)
			}
			host.request(t, "3", "world/send", map[string]string{
				"to": "charlie", "message": "call the native runtime", "source_conversation_id": input.SessionID,
			})
			frame := host.next(t)
			if tc.wantErr != "" {
				expectBridgeError(t, frame, "3", -32000, tc.wantErr)
				if c.model.calls.Load() != 0 {
					t.Fatal("exhausted message reached Charlie")
				}
			} else {
				sent := decodeWorld[WorldMessage](t, expectBridgeResponse(t, frame, "3"))
				if sent.Status != WorldMessageStatusReplied || sent.ReplyBody != "reply-1" || sent.HopLimit != tc.hops-1 {
					t.Fatalf("nested send = %+v", sent)
				}
				received, found, err := getRecord[WorldMessage](t.Context(), c.opt.Records, worldMessages, sent.MessageID)
				if err != nil || !found || received.HopLimit != tc.hops-1 || received.Status != WorldMessageStatusCompleted {
					t.Fatalf("nested receive = %+v, found=%v, err=%v", received, found, err)
				}
			}
			host.write(t, bridgeResponse{Version: "2.0", ID: deliver.ID, Result: map[string]string{"reply": "host reply"}})
			if reply := a.waitOutbox(t, msg.MessageID, WorldMessageStatusReplied); reply.ReplyBody != "host reply" {
				t.Fatalf("origin reply = %q", reply.ReplyBody)
			}
		})
	}
}

func TestBridgeRecovery(t *testing.T) {
	f := newTestFederation(t)
	opt := newTestBridgeOpt(t, f)
	for _, msg := range []WorldMessage{
		{MessageID: "a-second", ThreadID: "thread", Direction: WorldMessageDirectionInbound, ReceivedSequence: 2, Status: WorldMessageStatusAccepted},
		{MessageID: "z-first", ThreadID: "thread", Direction: WorldMessageDirectionInbound, ReceivedSequence: 1, Status: WorldMessageStatusAccepted},
		{MessageID: "was-running", ThreadID: "other-thread", Direction: WorldMessageDirectionInbound, ReceivedSequence: 3, Status: WorldMessageStatusRunning},
		{MessageID: "needs-notify", ThreadID: "notify-thread", Direction: WorldMessageDirectionOutbound, Status: WorldMessageStatusReplied, ConversationID: "origin-session", ReplyBody: "previous reply", NotifyConversation: true},
	} {
		msg.PeerAddr = "offline@" + testDomain
		msg.Kind = "message"
		msg.HopLimit = 5
		seedBridgeMessage(t, opt, msg)
	}
	host := startTestBridge(t, opt)
	host.request(t, "1", "world/status", struct{}{})
	expectBridgeError(t, host.next(t), "1", -32001, "")
	host.initialize(t)

	var deliveries []string
	var opens, notifications int
	for len(deliveries) < 2 || notifications < 2 {
		frame := host.next(t)
		switch string(frame.Method) {
		case `"world/open"`:
			req := decodeWorld[OpenSessionRequest](t, frame.Params)
			if req.ThreadID != "thread" || (opens == 0 && req.ExistingSessionID != "") || (opens == 1 && req.ExistingSessionID != "recovered-session") {
				t.Fatalf("recover open = %+v", req)
			}
			opens++
			host.write(t, bridgeResponse{Version: "2.0", ID: frame.ID, Result: OpenedSession{SessionID: "recovered-session"}})
		case `"world/deliver"`:
			req := decodeWorld[map[string]json.RawMessage](t, frame.Params)
			deliveries = append(deliveries, decodeWorld[string](t, req["message_id"]))
			host.write(t, bridgeResponse{Version: "2.0", ID: frame.ID, Result: map[string]string{"reply": "recovered reply"}})
		case `"world/notify"`:
			req := decodeWorld[map[string]string](t, frame.Params)
			if req["message_id"] != "needs-notify" || req["session_id"] != "origin-session" || req["reply_body"] != "previous reply" {
				t.Fatalf("recover notify = %+v", req)
			}
			msg, _, err := getRecord[WorldMessage](t.Context(), opt.Records, worldMessages, req["message_id"])
			if err != nil || !msg.NotifyConversation {
				t.Fatalf("notification cleared before success: %+v, %v", msg, err)
			}
			response := bridgeResponse{Version: "2.0", ID: frame.ID}
			if notifications == 0 {
				response.Error = &bridgeError{Code: -32000, Message: "host temporarily unavailable"}
			} else {
				response.Result = struct{}{}
			}
			notifications++
			host.write(t, response)
		default:
			t.Fatalf("unexpected recovery frame: %+v", frame)
		}
	}
	if opens != 2 || !slices.Equal(deliveries, []string{"z-first", "a-second"}) {
		t.Fatalf("recovery order = %v, opened %d sessions", deliveries, opens)
	}
	awaitWorld(t, func() bool {
		msg, _, err := getRecord[WorldMessage](t.Context(), opt.Records, worldMessages, "needs-notify")
		if err != nil {
			t.Fatal(err)
		}
		return !msg.NotifyConversation
	})
	host.request(t, "2", "world/message", map[string]string{"message_id": "was-running"})
	failed := decodeWorld[WorldMessage](t, expectBridgeResponse(t, host.next(t), "2"))
	if failed.Status != WorldMessageStatusFailed || failed.ErrorText == "" {
		t.Fatalf("running message after restart = %+v", failed)
	}
}

func TestBridgeShutdown(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(t *testing.T, host *testBridgeHost)
		want error
	}{
		{"context cancellation", func(_ *testing.T, h *testBridgeHost) { h.cancel() }, context.Canceled},
		{"stdin EOF", func(_ *testing.T, h *testBridgeHost) { h.input.Close() }, nil},
		{"oversized frame without newline", func(t *testing.T, h *testBridgeHost) {
			_, err := io.WriteString(h.input, strings.Repeat("x", maxBridgeFrameBytes+4096))
			if err != nil && !errors.Is(err, io.ErrClosedPipe) {
				t.Fatal(err)
			}
		}, errBridgeFrameTooLarge},
		{"stdout write failure", func(t *testing.T, h *testBridgeHost) {
			h.output.Close()
			h.request(t, "1", "world/status", struct{}{})
		}, io.ErrClosedPipe},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTestFederation(t)
			opt := newTestBridgeOpt(t, f)
			msg := WorldMessage{
				MessageID: "pending-inbound", ThreadID: "pending-thread", PeerAddr: "offline@" + testDomain,
				Direction: WorldMessageDirectionInbound, Status: WorldMessageStatusAccepted, ReceivedSequence: 1,
			}
			seedBridgeMessage(t, opt, msg)
			host := startTestBridge(t, opt)
			host.initialize(t)
			expectBridgeCallback(t, host.next(t), "world/open")
			tc.stop(t, host)
			if err := host.wait(t); !errors.Is(err, tc.want) {
				t.Fatalf("bridge exit = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestBridgeDeliverReply(t *testing.T) {
	f := newTestFederation(t)
	opt := newTestBridgeOpt(t, f)
	for _, tc := range []struct {
		name   string
		result map[string]any
		status WorldMessageStatus
	}{
		{"missing", map[string]any{}, WorldMessageStatusFailed},
		{"null", map[string]any{"reply": nil}, WorldMessageStatusFailed},
		{"empty string", map[string]any{"reply": ""}, WorldMessageStatusCompleted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := "inbound-" + tc.name
			seedBridgeMessage(t, opt, WorldMessage{MessageID: id, ThreadID: id, PeerAddr: "offline@" + testDomain, Direction: WorldMessageDirectionInbound, Status: WorldMessageStatusAccepted})
			host := startTestBridge(t, opt)
			host.initialize(t)
			open := host.next(t)
			expectBridgeCallback(t, open, "world/open")
			host.write(t, bridgeResponse{Version: "2.0", ID: open.ID, Result: OpenedSession{SessionID: "host-session"}})
			deliver := host.next(t)
			expectBridgeCallback(t, deliver, "world/deliver")
			host.write(t, bridgeResponse{Version: "2.0", ID: deliver.ID, Result: tc.result})
			awaitWorld(t, func() bool {
				msg, _, err := getRecord[WorldMessage](t.Context(), opt.Records, worldMessages, id)
				if err != nil {
					t.Fatal(err)
				}
				if msg.Status == WorldMessageStatusFailed && tc.status == WorldMessageStatusCompleted {
					t.Fatal(msg.ErrorText)
				}
				return msg.Status == tc.status
			})
		})
	}
}
