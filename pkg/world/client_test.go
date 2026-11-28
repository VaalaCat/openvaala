package world

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
)

func TestWorldClientForward(t *testing.T) {
	// 1. 从来信生成真实对话和助手回复，再通过节点 API 转发。
	f := newTestFederation(t)
	a := f.node(t, "alice", "")
	b := f.node(t, "bob", "")
	b.start(t)
	b.waitPeer(t, "alice", false)
	ch := testDialDirect(t, a, b)
	msg := wireMessage{Kind: "message", MessageID: "seed", ThreadID: "seed-thread", From: Address{Node: "alice", Domain: testDomain}, To: Address{Node: "bob", Domain: testDomain}, Body: "question", HopLimit: 3}
	writeWireMessage(t, ch, msg)
	if got := readWireMessage(t, ch); got.Status != "accepted" {
		t.Fatal(got)
	}
	if got := readWireMessage(t, ch); got.Body != "reply-1" {
		t.Fatal(got)
	}
	ch.Close()
	threadBinding, ok, err := getRecord[ThreadBinding](t.Context(), b.opt.Records, worldThreads, msg.ThreadID)
	if err != nil || !ok {
		t.Fatal(err)
	}
	b.waitPeer(t, "alice", false)
	cid := defs.ConversationID(threadBinding.ConversationID)
	conv, err := b.rt.GetConversation(t.Context(), &runtime.GetConversationRequest{ConversationID: cid})
	if err != nil {
		t.Fatal(err)
	}
	page, err := b.rt.ListItems(t.Context(), &runtime.ListItemsRequest{ConversationID: cid, SessionID: conv.RootSessionID, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var inputID defs.ItemID
	for _, i := range page.Items {
		if messageText(i.Item) == "question" {
			inputID = i.Item.ID
		}
	}
	if inputID == "" {
		t.Fatal("missing input item")
	}
	for _, tc := range []struct {
		name    string
		request any
		want    string
	}{
		{"new message", SendMessageRequest{To: "alice", Message: "manual", SourceConversationID: string(cid)}, "manual"},
		{"latest assistant reply", ForwardRequest{To: "alice", ConversationID: cid, Note: "note"}, "reply-1\n\nnote"},
		{"selected input", ForwardRequest{To: "alice", ConversationID: cid, ItemIDs: []defs.ItemID{inputID}}, "question"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 1. 新发件可以按来源和状态找到，取消只对离线队列生效。
			var outboundMessage WorldMessage
			var err error
			switch request := tc.request.(type) {
			case SendMessageRequest:
				outboundMessage, err = b.node.SendMessage(t.Context(), request)
			case ForwardRequest:
				outboundMessage, err = b.node.Forward(t.Context(), b.rt, request)
			}
			if err != nil {
				t.Fatal(err)
			}
			if outboundMessage.Status != WorldMessageStatusPendingOffline || outboundMessage.Body != tc.want || outboundMessage.HopLimit != 2 {
				t.Fatalf("outbox: %+v", outboundMessage)
			}
			page, err := b.node.ListMessages(t.Context(), ListMessagesParams{Direction: "outbound", Status: "pending_offline", ConversationID: string(cid)})
			if err != nil {
				t.Fatal(err)
			}
			outbox := page.Items
			if len(outbox) != 1 || outbox[0].MessageID != outboundMessage.MessageID {
				t.Fatalf("filter: %+v", outbox)
			}
			status, err := b.node.GetStatus(t.Context(), "")
			if err != nil {
				t.Fatal(err)
			}
			if !status.Enabled || !status.Connected || status.PendingOutboxCount < 1 {
				t.Fatal(status)
			}
			cancelled, err := b.node.CancelMessage(t.Context(), outboundMessage.MessageID)
			if err != nil || cancelled.Status != WorldMessageStatusCancelled {
				t.Fatalf("cancel: %+v %v", cancelled, err)
			}
			_, err = b.node.CancelMessage(t.Context(), outboundMessage.MessageID)
			if ErrorCode(err) != "world_outbox_not_cancellable" {
				t.Fatal(err)
			}
		})
	}

}

func TestWorldClientMessagesAndThreads(t *testing.T) {
	// 1. 存入两条不同线程的来信和一条同线程的续信。
	node := newTestFederation(t).node(t, "bob", "")
	for _, inboundMessage := range []WorldMessage{
		{MessageID: "one", Direction: WorldMessageDirectionInbound, ThreadID: "first", ConversationID: "conversation-1"},
		{MessageID: "two", Direction: WorldMessageDirectionInbound, ThreadID: "first", ConversationID: "conversation-1"},
		{MessageID: "three", Direction: WorldMessageDirectionInbound, ThreadID: "second", ConversationID: "conversation-2"},
	} {
		if err := putRecord(t.Context(), node.opt.Records, worldMessages, inboundMessage.MessageID, 1, inboundMessage); err != nil {
			t.Fatal(err)
		}
	}
	for _, threadBinding := range []ThreadBinding{
		{ThreadID: "first", ConversationID: "conversation-1"},
		{ThreadID: "second", ConversationID: "conversation-2"},
	} {
		if err := putRecord(t.Context(), node.opt.Records, worldThreads, threadBinding.ThreadID, 1, threadBinding); err != nil {
			t.Fatal(err)
		}
	}

	// 2. 验证全量、单项过滤及两个查询条件同时生效。
	for _, tc := range []struct {
		name   string
		params ListMessagesParams
		want   []string
	}{
		{"all", ListMessagesParams{}, []string{"one", "three", "two"}},
		{"conversation", ListMessagesParams{ConversationID: "conversation-1"}, []string{"one", "two"}},
		{"thread", ListMessagesParams{ThreadID: "second"}, []string{"three"}},
		{"both", ListMessagesParams{ConversationID: "conversation-2", ThreadID: "first"}, []string{}},
	} {
		t.Run("messages/"+tc.name, func(t *testing.T) {
			page, err := node.node.ListMessages(t.Context(), tc.params)
			if err != nil {
				t.Fatal(err)
			}
			ids := make([]string, 0, len(page.Items))
			for _, message := range page.Items {
				ids = append(ids, message.MessageID)
			}
			slices.Sort(ids)
			if page.Items == nil || !slices.Equal(ids, tc.want) {
				t.Fatalf("inbox messages = %+v, want IDs %v", page.Items, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		conversation string
		want         []string
	}{
		{"", []string{"first", "second"}},
		{"conversation-2", []string{"second"}},
		{"missing", []string{}},
	} {
		t.Run("threads/"+tc.conversation, func(t *testing.T) {
			page, err := node.node.ListThreads(t.Context(), ListThreadsParams{ConversationID: tc.conversation})
			if err != nil {
				t.Fatal(err)
			}
			ids := make([]string, 0, len(page.Items))
			for _, thread := range page.Items {
				ids = append(ids, thread.ThreadID)
			}
			slices.Sort(ids)
			if page.Items == nil || !slices.Equal(ids, tc.want) {
				t.Fatalf("thread bindings = %+v, want IDs %v", page.Items, tc.want)
			}
		})
	}
}

func TestWorldClientMessageUpdates(t *testing.T) {
	// 1. 先订阅收件端事件流，并暂停模型直到完成对话查询。
	f := newTestFederation(t)
	a := f.node(t, "alice", "")
	b := f.node(t, "bob", "")
	b.model.release = make(chan struct{}, 1)
	b.start(t)
	b.waitPeer(t, "alice", false)
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	updates, err := b.node.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ch := testDialDirect(t, a, b)
	defer ch.Close()
	msg := wireMessage{
		Kind: "message", MessageID: "first-message", ThreadID: "first-thread",
		From: Address{Node: "alice", Domain: testDomain}, To: Address{Node: "bob", Domain: testDomain},
		Subject: "  An inbound   subject  ", Body: "question", HopLimit: 3,
	}
	writeWireMessage(t, ch, msg)
	if got := readWireMessage(t, ch); got.Status != "accepted" {
		t.Fatal(got)
	}

	// 2. 按顺序观察落盘、运行、提交输入与模型完成后的更新。
	var conversationID string
	for _, tc := range []struct {
		name   string
		status WorldMessageStatus
	}{
		{"accepted", "accepted"},
		{"running", "running"},
		{"submitted", "running"},
		{"completed", "completed"},
	} {
		var inboundMessage WorldMessage
		for {
			select {
			case event, ok := <-updates:
				if !ok {
					t.Fatal("subscription closed")
				}
				if event.Event != "message_updated" {
					continue
				}
				inboundMessage = decodeWorld[WorldMessage](t, event.Data)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			break
		}
		if inboundMessage.MessageID != msg.MessageID || inboundMessage.Direction != WorldMessageDirectionInbound || inboundMessage.Status != tc.status {
			t.Fatalf("%s inbox update = %+v, want status %s", tc.name, inboundMessage, tc.status)
		}
		switch tc.name {
		case "running":
			conversationID = inboundMessage.ConversationID
		case "submitted":
			page, err := b.rt.ListConversations(ctx, &dao.ListConversationsRequest{Limit: 10})
			if err != nil || len(page.Items) != 1 {
				t.Fatalf("conversations after submit = %+v, %v", page.Items, err)
			}
			conv := page.Items[0].Conversation
			if string(conv.ID) != conversationID || inboundMessage.ConversationID != conversationID || conv.Title != "An inbound subject" {
				t.Fatalf("conversation after submit = %+v, inbox update = %+v", conv, inboundMessage)
			}
			b.model.release <- struct{}{}
		case "completed":
			if inboundMessage.ReplyBody != "reply-1" {
				t.Fatalf("completed inbox message = %+v", inboundMessage)
			}
		}
	}
}

func TestWorldClientEventShutdown(t *testing.T) {
	for _, stop := range []string{"node", "request", "slow subscriber"} {
		t.Run(stop, func(t *testing.T) {
			f := newTestFederation(t)
			a := f.node(t, "alice", "")
			_ = f.node(t, "bob", "")
			a.start(t)
			a.waitPeer(t, "bob", false)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			events, err := a.node.Subscribe(ctx)
			if err != nil {
				t.Fatal(err)
			}
			switch stop {
			case "node":
				a.stop()
			case "request":
				cancel()
			case "slow subscriber":
				for range 40 {
					a.send(t, SendMessageRequest{To: "bob", Message: "event"})
				}
			}
			deadline := time.After(time.Second)
			for {
				select {
				case _, ok := <-events:
					if !ok {
						return
					}
				case <-deadline:
					t.Fatal("subscription did not close")
				}
			}
		})
	}
}

func TestWorldClientRecordPages(t *testing.T) {
	node := newTestFederation(t).node(t, "alice", "")
	records := node.opt.Records
	for i := 1; i <= 5; i++ {
		id := strconv.Itoa(i)
		for namespace, item := range map[string]any{
			worldMessages: WorldMessage{MessageID: id, Direction: WorldMessageDirectionOutbound, Status: WorldMessageStatusSending},
			worldThreads:  ThreadBinding{ThreadID: id},
		} {
			if err := putRecord(t.Context(), records, namespace, id, int64(4-i/2), item); err != nil {
				t.Fatal(err)
			}
		}
	}

	for _, tc := range []struct {
		route, idField string
		pages          [][]string
		cursorTimes    []int64
	}{
		{"messages", "message_id", [][]string{{"5", "4"}, {"3", "2"}, {"1"}}, []int64{0, 0}},
		{"threads", "thread_id", [][]string{{"1", "3"}, {"2", "5"}, {"4"}}, []int64{3, 2}},
	} {
		t.Run(tc.route, func(t *testing.T) {
			limit := 2
			var cursorID *string
			var cursorTime *int64
			list := func() []byte {
				var page any
				var err error
				if tc.route == "messages" {
					page, err = node.node.ListMessages(t.Context(), ListMessagesParams{Limit: &limit, CursorID: cursorID})
				} else {
					page, err = node.node.ListThreads(t.Context(), ListThreadsParams{Limit: &limit, CursorID: cursorID, CursorUpdatedAt: cursorTime})
				}
				if err != nil {
					t.Fatal(err)
				}
				body, err := json.Marshal(page)
				if err != nil {
					t.Fatal(err)
				}
				return body
			}
			for i, want := range tc.pages {
				body := list()
				page := decodeWorld[struct {
					Items      []map[string]any
					NextCursor *dao.RecordCursor[string] `json:"next_cursor"`
				}](t, body)
				ids := make([]string, 0, len(page.Items))
				for _, item := range page.Items {
					ids = append(ids, item[tc.idField].(string))
				}
				if !slices.Equal(ids, want) {
					t.Fatalf("page %d = %v, want %v", i+1, ids, want)
				}
				if i == len(tc.pages)-1 {
					if _, exists := decodeWorld[map[string]any](t, body)["next_cursor"]; exists {
						t.Fatalf("last page has next_cursor: %s", body)
					}
					continue
				}
				wantCursor := dao.RecordCursor[string]{ID: want[len(want)-1], UpdatedAt: tc.cursorTimes[i]}
				if page.NextCursor == nil || *page.NextCursor != wantCursor {
					t.Fatalf("page %d cursor = %+v, want %+v", i+1, page.NextCursor, wantCursor)
				}
				cursorID = &page.NextCursor.ID
				if page.NextCursor.UpdatedAt != 0 {
					cursorTime = &page.NextCursor.UpdatedAt
				}
				if tc.route == "messages" && i == 0 {
					// 1. 旧消息在第一页返回后收到回信，仍应出现在原来的后续页。
					if err := putRecord(t.Context(), records, worldMessages, "2", 100, WorldMessage{
						MessageID: "2", Direction: WorldMessageDirectionOutbound, Status: WorldMessageStatusReplied, ReplyBody: "late reply", UpdatedAt: 100,
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
			lastID := "1"
			if tc.route == "threads" {
				lastID = "4"
				lastTime := int64(2)
				cursorTime = &lastTime
			}
			cursorID = &lastID
			body := list()
			if items, ok := decodeWorld[map[string]any](t, body)["items"].([]any); !ok || len(items) != 0 {
				t.Fatalf("empty page = %s", body)
			}
		})
	}
}

func TestWorldClientPaginationArguments(t *testing.T) {
	node := newTestFederation(t).node(t, "alice", "")
	empty, id, ts, zero := "", "one", int64(1), int64(0)
	for _, tc := range []struct {
		name   string
		params ListThreadsParams
	}{
		{"missing timestamp", ListThreadsParams{CursorID: &id}},
		{"missing id", ListThreadsParams{CursorUpdatedAt: &ts}},
		{"empty id", ListThreadsParams{CursorID: &empty, CursorUpdatedAt: &ts}},
		{"zero timestamp", ListThreadsParams{CursorID: &id, CursorUpdatedAt: &zero}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := node.node.ListThreads(t.Context(), tc.params)
			if err == nil || ErrorCode(err) != "invalid_arguments" {
				t.Fatalf("error = %v", err)
			}
		})
	}
	for _, limit := range []int{-1, 0, 201} {
		t.Run(strconv.Itoa(limit), func(t *testing.T) {
			_, err := node.node.ListMessages(t.Context(), ListMessagesParams{Limit: &limit})
			if err == nil || ErrorCode(err) != "invalid_arguments" {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestWorldClientMessagesByPeer(t *testing.T) {
	node := newTestFederation(t).node(t, "alice", "")
	const peer = "bob@world.example"
	for _, message := range []WorldMessage{
		{MessageID: "1", Direction: WorldMessageDirectionInbound, Kind: "message", PeerAddr: peer},
		{MessageID: "2", Direction: WorldMessageDirectionOutbound, Kind: "message", PeerAddr: peer + "/default"},
		{MessageID: "3", Direction: WorldMessageDirectionInbound, Kind: "reply", PeerAddr: peer},
		{MessageID: "4", Direction: WorldMessageDirectionInbound, Kind: "message", PeerAddr: peer + "/default"},
		{MessageID: "5", Direction: WorldMessageDirectionOutbound, Kind: "message", PeerAddr: peer},
		{MessageID: "6", Direction: WorldMessageDirectionOutbound, Kind: "message", PeerAddr: "carol@world.example"},
	} {
		if err := putRecord(t.Context(), node.opt.Records, worldMessages, message.MessageID, 1, message); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		name      string
		direction WorldMessageDirection
		pages     [][]string
	}{
		{"both directions", "", [][]string{{"5", "4"}, {"2", "1"}}},
		{"inbound", WorldMessageDirectionInbound, [][]string{{"4", "1"}}},
		{"outbound", WorldMessageDirectionOutbound, [][]string{{"5", "2"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limit := 2
			params := ListMessagesParams{
				PeerAddresses: []string{"", peer, peer + "/default", peer}, Kind: "message", Limit: &limit,
				Direction: string(tc.direction),
			}
			for i, want := range tc.pages {
				page, err := node.node.ListMessages(t.Context(), params)
				if err != nil {
					t.Fatal(err)
				}
				ids := make([]string, 0, len(page.Items))
				for _, message := range page.Items {
					ids = append(ids, message.MessageID)
				}
				if !slices.Equal(ids, want) {
					t.Fatalf("page %d = %v, want %v", i+1, ids, want)
				}
				wantCursor := dao.RecordCursor[string]{}
				if i+1 < len(tc.pages) {
					wantCursor.ID = want[len(want)-1]
					params.CursorID = &page.NextCursor.ID
				}
				if page.NextCursor != wantCursor {
					t.Fatalf("page %d cursor = %+v, want %+v", i+1, page.NextCursor, wantCursor)
				}
			}
		})
	}
}

func TestWorldClientCancelMessageDirection(t *testing.T) {
	node := newTestFederation(t).node(t, "alice", "")
	for _, message := range []WorldMessage{
		{MessageID: "inbound", Direction: WorldMessageDirectionInbound, Status: WorldMessageStatusCompleted, Kind: "message"},
		{MessageID: "reply", Direction: WorldMessageDirectionOutbound, PeerAddr: "offline@world.example", Status: WorldMessageStatusPendingOffline, Kind: "reply"},
		{MessageID: "sent", Direction: WorldMessageDirectionOutbound, Status: WorldMessageStatusReplied, Kind: "message"},
	} {
		if err := putRecord(t.Context(), node.opt.Records, worldMessages, message.MessageID, 1, message); err != nil {
			t.Fatal(err)
		}
	}
	node.start(t)
	status, err := node.node.GetStatus(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if status.PendingOutboxCount != 1 {
		t.Fatalf("pending count = %d, want 1", status.PendingOutboxCount)
	}
	for _, tc := range []struct {
		id   string
		code string
	}{
		{"absent", "world_message_not_found"},
		{"inbound", "world_message_not_found"},
		{"sent", "world_outbox_not_cancellable"},
		{"reply", ""},
	} {
		t.Run(tc.id, func(t *testing.T) {
			got, err := node.node.CancelMessage(t.Context(), tc.id)
			if tc.code == "" {
				if err != nil || got.Status != WorldMessageStatusCancelled || got.Direction != WorldMessageDirectionOutbound {
					t.Fatalf("cancelled: %+v", got)
				}
			} else if got := ErrorCode(err); err == nil || got != tc.code {
				t.Fatalf("code = %q, want %q", got, tc.code)
			}
		})
	}
}

func TestWorldRecentContacts(t *testing.T) {
	for _, total := range []int{0, 3, 22} {
		t.Run(strconv.Itoa(total), func(t *testing.T) {
			node := newTestFederation(t).node(t, "bob", "")
			want := []RecentContact{}
			for i := 0; i < total; i++ {
				name := "peer" + strconv.Itoa(i)
				address := name + "@other"
				thread := ThreadBinding{ThreadID: name, PeerAddr: address, PeerDomain: "other", Direction: "inbound"}
				if err := putRecord(t.Context(), node.opt.Records, worldThreads, name, int64(i+1), thread); err != nil {
					t.Fatal(err)
				}
				want = append([]RecentContact{{Address: address, ThreadID: name}}, want...)
			}
			local := ThreadBinding{ThreadID: "local", PeerAddr: "me@" + testDomain, PeerDomain: testDomain}
			if err := putRecord(t.Context(), node.opt.Records, worldThreads, local.ThreadID, 100, local); err != nil {
				t.Fatal(err)
			}
			node.start(t)
			result, err := node.node.ListPeers(t.Context())
			if err != nil || !slices.Equal(result.Recent, want[:min(total, 20)]) {
				t.Fatal(result.Recent, err)
			}
		})
	}
}
