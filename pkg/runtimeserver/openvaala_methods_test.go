package runtimeserver_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/hook"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/VaalaCat/openvaala/pkg/runtimeserver"
	"github.com/VaalaCat/openvaala/pkg/storage"
	"github.com/VaalaCat/openvaala/pkg/telemetry/telemetrytest"
	"github.com/google/uuid"
)

// TestOpenVaalaMethods 检查对话与 Session 查询、输入、压缩和模型选择。
func TestOpenVaalaMethods(t *testing.T) {
	// 1. 列出对话与 Session 查询、输入、压缩和模型选择的测试场景。
	tests := []struct {
		name string
		run  func(t *testing.T, h *serverHarness, client *testClient)
	}{
		{
			name: "create_read_list_and_title",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				created := client.createConversation(t)
				client.callResult(t, "conversation/setTitle", map[string]any{"conversation_id": created.ID, "title": "周报"}, nil)
				var read struct {
					Conversation defs.Conversation `json:"conversation"`
				}
				client.callResult(t, "conversation/read", map[string]any{"conversation_id": created.ID}, &read)
				if read.Conversation.Title != "周报" || read.Conversation.RootSessionID != created.RootSessionID {
					t.Fatalf("read = %+v", read.Conversation)
				}
				completeFirstRound(t, h, client, created)
				var list struct {
					Conversations []dao.ConversationListEntry `json:"conversations"`
				}
				client.callResult(t, "conversation/list", nil, &list)
				if len(list.Conversations) != 1 || list.Conversations[0].Conversation.ID != created.ID {
					t.Fatalf("list = %+v", list.Conversations)
				}
			},
		},
		{
			name: "create_ephemeral",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				stored := client.createConversation(t)
				completeFirstRound(t, h, client, stored)
				var created struct {
					Conversation defs.Conversation `json:"conversation"`
				}
				client.callResult(t, "conversation/create", map[string]any{"agent_name": "assistant", "ephemeral": true}, &created)
				var list struct {
					Conversations []dao.ConversationListEntry `json:"conversations"`
				}
				client.callResult(t, "conversation/list", nil, &list)
				if len(list.Conversations) != 1 || list.Conversations[0].Conversation.ID != stored.ID {
					t.Fatalf("list = %+v, want only the stored conversation %s", list.Conversations, stored.ID)
				}
				var read struct {
					Conversation defs.Conversation `json:"conversation"`
				}
				client.callResult(t, "conversation/read", map[string]any{"conversation_id": created.Conversation.ID}, &read)
				if read.Conversation.ID != created.Conversation.ID || read.Conversation.RootSessionID != created.Conversation.RootSessionID {
					t.Fatalf("read = %+v, want the ephemeral conversation %+v", read.Conversation, created.Conversation)
				}
			},
		},
		{
			name: "list_by_activity_with_cursor",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				first, second := client.createConversation(t), client.createConversation(t)
				completeFirstRound(t, h, client, first)
				completeFirstRound(t, h, client, second)
				time.Sleep(2 * time.Millisecond) // 让改标题的写入时间严格晚于两次保存
				client.callResult(t, "conversation/setTitle", map[string]any{"conversation_id": first.ID, "title": "最近活跃"}, nil)
				order := map[string]any{"by": "updated_at", "direction": "desc"}
				var page struct {
					Conversations []dao.ConversationListEntry           `json:"conversations"`
					NextCursor    dao.RecordCursor[defs.ConversationID] `json:"next_cursor"`
				}
				client.callResult(t, "conversation/list", map[string]any{"order": order, "limit": 1}, &page)
				if len(page.Conversations) != 1 || page.Conversations[0].Conversation.ID != first.ID || page.Conversations[0].Conversation.UpdatedAt == 0 || page.NextCursor.ID != first.ID {
					t.Fatalf("first page = %+v", page)
				}
				var rest struct {
					Conversations []dao.ConversationListEntry `json:"conversations"`
					NextCursor    json.RawMessage             `json:"next_cursor"`
				}
				client.callResult(t, "conversation/list", map[string]any{"order": order, "cursor": page.NextCursor, "limit": 1}, &rest)
				if len(rest.Conversations) != 1 || rest.Conversations[0].Conversation.ID != second.ID || rest.NextCursor != nil {
					t.Fatalf("second page = %+v", rest)
				}
			},
		},
		{
			name: "empty_list_is_array",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				response := client.callResult(t, "conversation/list", nil, nil)
				if string(response.Result) != `{"conversations":[]}` {
					t.Fatalf("empty list result = %s", response.Result)
				}
			},
		},
		{
			name: "list_order_object",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				first, second := client.createConversation(t), client.createConversation(t)
				completeFirstRound(t, h, client, first)
				completeFirstRound(t, h, client, second)
				var list struct {
					Conversations []dao.ConversationListEntry `json:"conversations"`
				}
				client.callResult(t, "conversation/list", map[string]any{"order": map[string]any{"direction": "desc"}, "limit": 10}, &list)
				if len(list.Conversations) != 2 || list.Conversations[0].Conversation.ID != second.ID || list.Conversations[1].Conversation.ID != first.ID {
					t.Fatalf("descending list = %+v", list.Conversations)
				}
			},
		},
		{
			name: "invalid_order_rejected",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				created := client.createConversation(t)
				calls := []struct {
					method string
					params map[string]any
				}{
					{
						method: "conversation/list",
						params: map[string]any{"order": map[string]any{"by": "title"}},
					},
					{
						method: "skill/list",
						params: map[string]any{},
					},
					{
						method: "conversation/list",
						params: map[string]any{"order": map[string]any{"direction": "ascending"}},
					},
					{
						method: "session/turns/list",
						params: map[string]any{"conversation_id": created.ID, "session_id": created.RootSessionID, "order": map[string]any{"by": "title"}},
					},
					{
						method: "session/items/list",
						params: map[string]any{"conversation_id": created.ID, "session_id": created.RootSessionID, "order": "ascending"},
					},
				}
				for _, call := range calls {
					if response := client.call(t, call.method, call.params); response.Error == nil || response.Error.Code != -32602 {
						t.Fatalf("%s %v response = %+v, want invalid params", call.method, call.params, response)
					}
				}
			},
		},
		{
			name: "submit_and_list_items",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				conversation := client.createConversation(t)
				var submitted struct {
					ItemID defs.ItemID `json:"item_id"`
				}
				client.callResult(t, "conversation/submitInput", map[string]any{
					"conversation_id": conversation.ID,
					"content":         textContent("你好"),
				}, &submitted)
				h.model.receive(t).reply <- serverModelResponse{chunks: []string{"你好！"}}
				items := client.waitItems(t, conversation, conversation.RootSessionID, 2)
				if items[0].Ordinal != 1 || items[0].Item.ID != submitted.ItemID || items[1].Ordinal != 2 || items[1].Item.Kind != defs.ItemMessage {
					t.Fatalf("items = %+v", items)
				}
			},
		},
		{
			// 每条输入约 1.5 MiB：一页的 JSON 累计超过 4 MiB 就在那条 Item 之后截断，next_cursor 指向它，沿它翻完每条恰好出现一次。
			name: "items_page_truncated_by_bytes",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				conversation := client.createConversation(t)
				client.callResult(t, "session/subscribe", sessionTarget(conversation, conversation.RootSessionID), nil)
				for range 5 {
					submitText(t, client, conversation, strings.Repeat("x", 3<<19))
					h.model.receive(t).reply <- serverModelResponse{chunks: []string{"收到"}}
					client.collectNotifications(t, isMethod("turn/finished"))
				}
				const totalItems = 10 // 5 轮，每轮一条用户输入加一条回复
				first, ordinals := client.listItemsPage(t, conversation, 0)
				if len(first.Items) == 0 || len(first.Items) >= totalItems {
					t.Fatalf("first page has %d items, want a truncated page of fewer than %d", len(first.Items), totalItems)
				}
				if beforeLast, whole := getRawBytes(first.Items[:len(first.Items)-1]), getRawBytes(first.Items); beforeLast > 4<<20 || whole <= 4<<20 {
					t.Fatalf("first page bytes before last item = %d, whole page = %d, want <= 4 MiB and > 4 MiB", beforeLast, whole)
				}
				if first.NextCursor != ordinals[len(ordinals)-1] {
					t.Fatalf("first page next_cursor = %d, want last ordinal %d", first.NextCursor, ordinals[len(ordinals)-1])
				}
				for cursor, pages := first.NextCursor, 0; cursor != 0; pages++ {
					if pages == totalItems {
						t.Fatalf("pages did not end after %d pages", pages)
					}
					page, pageOrdinals := client.listItemsPage(t, conversation, cursor)
					ordinals = append(ordinals, pageOrdinals...)
					cursor = page.NextCursor
				}
				if want := []storage.Ordinal{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}; !slices.Equal(ordinals, want) {
					t.Fatalf("ordinals across pages = %v, want %v", ordinals, want)
				}
			},
		},
		{
			// 单条 Item 自己就超过 4 MiB 时，它所在的页仍然返回这 1 条，下一页从它之后继续。
			name: "single_large_item_returned",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				conversation := client.createConversation(t)
				client.callResult(t, "session/subscribe", sessionTarget(conversation, conversation.RootSessionID), nil)
				submitText(t, client, conversation, strings.Repeat("x", 5<<20))
				h.model.receive(t).reply <- serverModelResponse{chunks: []string{"收到"}}
				client.collectNotifications(t, isMethod("turn/finished"))
				first, ordinals := client.listItemsPage(t, conversation, 0)
				if !slices.Equal(ordinals, []storage.Ordinal{1}) || first.NextCursor != 1 {
					t.Fatalf("first page ordinals = %v next_cursor = %d, want only ordinal 1 and next_cursor 1", ordinals, first.NextCursor)
				}
				rest, restOrdinals := client.listItemsPage(t, conversation, first.NextCursor)
				if !slices.Equal(restOrdinals, []storage.Ordinal{2}) || rest.NextCursor != 0 {
					t.Fatalf("second page ordinals = %v next_cursor = %d, want only ordinal 2 and no next_cursor", restOrdinals, rest.NextCursor)
				}
			},
		},
		{
			name: "submit_image_and_context",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				conversation := client.createConversation(t)
				client.callResult(t, "conversation/submitInput", map[string]any{
					"conversation_id": conversation.ID,
					"content": []map[string]any{
						{"kind": "text", "text": map[string]any{"text": "请看这张图"}},
						{"kind": "text", "text": map[string]any{"text": "附带的文件内容", "is_context": true}},
						{"kind": "image", "image": map[string]any{"data_url": "data:image/png;base64,iVBORw0KGgo="}},
					},
				}, nil)
				request := h.model.receive(t)
				body, err := json.Marshal(request.body["messages"])
				if err != nil {
					t.Fatal(err)
				}
				for _, want := range []string{"请看这张图", "附带的文件内容", "data:image/png;base64,iVBORw0KGgo="} {
					if !strings.Contains(string(body), want) {
						t.Fatalf("model messages = %s, want %q", body, want)
					}
				}
				request.reply <- serverModelResponse{chunks: []string{"好"}}
			},
		},
		{
			// 外部提交的内容在 Session 接收前校验：无效内容返回普通错误（-32000），不保存输入、不开始 Turn。
			name: "submit_malformed_content_rejected",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				conversation := client.createConversation(t)
				tests := []struct {
					name    string
					content any
				}{
					{
						name:    "null_entry",
						content: []any{nil},
					},
					{
						name:    "empty_list",
						content: []any{},
					},
					{
						name:    "missing_list",
						content: nil,
					},
					{
						name:    "text_without_payload",
						content: []any{map[string]any{"kind": "text"}},
					},
					{
						name:    "image_without_source",
						content: []any{map[string]any{"kind": "image", "image": map[string]any{}}},
					},
					{
						name:    "unknown_kind",
						content: []any{map[string]any{"kind": "audio"}},
					},
				}
				for _, tt := range tests {
					response := client.call(t, "conversation/submitInput", map[string]any{"conversation_id": conversation.ID, "content": tt.content})
					if response.Error == nil || response.Error.Code != -32000 {
						t.Fatalf("%s response = %+v, want error -32000", tt.name, response)
					}
				}
				h.model.requireNoRequest(t)
				var items struct {
					Items []loggedItemView `json:"items"`
				}
				client.callResult(t, "session/items/list", map[string]any{"conversation_id": conversation.ID, "session_id": conversation.RootSessionID, "limit": 100}, &items)
				if len(items.Items) != 0 {
					t.Fatalf("session log after rejected inputs = %+v, want empty", items.Items)
				}
			},
		},
		{
			name: "busy_error_code",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				conversation := client.createConversation(t)
				submitText(t, client, conversation, "长任务")
				request := h.model.receive(t)
				requireErrorCode(t, client.call(t, "conversation/submitInput", map[string]any{
					"conversation_id": conversation.ID,
					"mode":            "idle_only",
					"content":         textContent("再来一个"),
				}), -32001, "session is busy")
				request.reply <- serverModelResponse{chunks: []string{"好"}}
			},
		},
		{
			name: "not_found_error_code",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				requireErrorCode(t, client.call(t, "conversation/open", map[string]any{"conversation_id": defs.NewConversationID()}), -32002, "conversation not found")
			},
		},
		{
			name: "duplicate_input_id_runs_once",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				conversation := client.createConversation(t)
				params := map[string]any{
					"conversation_id": conversation.ID,
					"input_id":        "retry-1",
					"content":         textContent("只执行一次"),
				}
				var first, second struct {
					ItemID defs.ItemID `json:"item_id"`
				}
				client.callResult(t, "conversation/submitInput", params, &first)
				client.callResult(t, "conversation/submitInput", params, &second)
				if first.ItemID == "" || first.ItemID != second.ItemID {
					t.Fatalf("retry item_id = %s, want %s", second.ItemID, first.ItemID)
				}
				h.model.receive(t).reply <- serverModelResponse{chunks: []string{"好"}}
				client.waitItems(t, conversation, conversation.RootSessionID, 2)
				h.model.requireNoRequest(t)
			},
		},
		{
			name: "interrupt_active_turn",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				conversation := client.createConversation(t)
				client.callResult(t, "conversation/submitInput", map[string]any{
					"conversation_id": conversation.ID,
					"content":         textContent("长任务"),
				}, nil)
				h.model.receive(t).reply <- serverModelResponse{
					chunks:  []string{"处理中"},
					release: make(chan struct{}),
				}
				var interrupted struct {
					TurnID defs.TurnID `json:"turn_id"`
				}
				client.callResult(t, "conversation/interruptTurn", map[string]any{"conversation_id": conversation.ID}, &interrupted)
				if interrupted.TurnID == "" {
					t.Fatal("interruptTurn turn_id is empty for an active Turn")
				}
			},
		},
		{
			// 三轮之后按创建时间倒序、每页 2 条翻完：第二页没有 next_cursor，cursor 原样回传即可继续。
			name: "list_turns_pages_in_descending_order",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				conversation := client.createConversation(t)
				client.callResult(t, "session/subscribe", sessionTarget(conversation, conversation.RootSessionID), nil)
				var turnIDs []defs.TurnID
				for _, text := range []string{"一", "二", "三"} {
					submitText(t, client, conversation, text)
					h.model.receive(t).reply <- serverModelResponse{chunks: []string{"好"}}
					notifications := client.collectNotifications(t, isMethod("turn/finished"))
					turnIDs = append(turnIDs, decodeNotification(t, notifications[len(notifications)-1]).TurnID)
				}
				params := map[string]any{"conversation_id": conversation.ID, "session_id": conversation.RootSessionID, "order": map[string]any{"direction": "desc"}, "limit": 2}
				var first turnsPageView
				client.callResult(t, "session/turns/list", params, &first)
				if got := getTurnIDs(first.Turns); !slices.Equal(got, []defs.TurnID{turnIDs[2], turnIDs[1]}) || first.NextCursor.ID != turnIDs[1] {
					t.Fatalf("first page turns = %v next_cursor = %+v, want %v then cursor %s", got, first.NextCursor, []defs.TurnID{turnIDs[2], turnIDs[1]}, turnIDs[1])
				}
				for _, turn := range first.Turns {
					if turn.SessionID != conversation.RootSessionID || turn.Status != defs.TurnStatusCompleted {
						t.Fatalf("turn = %+v, want a completed Turn of the root Session", turn)
					}
				}
				params["cursor"] = first.NextCursor
				var rest struct {
					Turns      []defs.Turn     `json:"turns"`
					NextCursor json.RawMessage `json:"next_cursor"`
				}
				client.callResult(t, "session/turns/list", params, &rest)
				if got := getTurnIDs(rest.Turns); !slices.Equal(got, []defs.TurnID{turnIDs[0]}) || rest.NextCursor != nil {
					t.Fatalf("second page turns = %v next_cursor = %s, want %v and no next_cursor", got, rest.NextCursor, []defs.TurnID{turnIDs[0]})
				}
			},
		},
		{
			name: "list_turns_empty_is_array",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				conversation := client.createConversation(t)
				response := client.callResult(t, "session/turns/list", map[string]any{"conversation_id": conversation.ID, "session_id": conversation.RootSessionID}, nil)
				if string(response.Result) != `{"turns":[]}` {
					t.Fatalf("empty turns result = %s", response.Result)
				}
			},
		},
		{
			// 按 turn_ids 的顺序返回找到的 Turn，不存在的跳过；一条都没找到时 turns 是空数组。
			name: "get_turns_in_request_order_skipping_missing",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				conversation := client.createConversation(t)
				client.callResult(t, "session/subscribe", sessionTarget(conversation, conversation.RootSessionID), nil)
				var turnIDs []defs.TurnID
				for _, text := range []string{"一", "二", "三"} {
					submitText(t, client, conversation, text)
					h.model.receive(t).reply <- serverModelResponse{chunks: []string{"好"}}
					notifications := client.collectNotifications(t, isMethod("turn/finished"))
					turnIDs = append(turnIDs, decodeNotification(t, notifications[len(notifications)-1]).TurnID)
				}
				params := map[string]any{"conversation_id": conversation.ID, "session_id": conversation.RootSessionID, "turn_ids": []defs.TurnID{turnIDs[2], "missing", turnIDs[0]}}
				var found turnsPageView
				client.callResult(t, "session/turns/get", params, &found)
				if got := getTurnIDs(found.Turns); !slices.Equal(got, []defs.TurnID{turnIDs[2], turnIDs[0]}) {
					t.Fatalf("session/turns/get turns = %v, want %v", got, []defs.TurnID{turnIDs[2], turnIDs[0]})
				}
				for _, turn := range found.Turns {
					if turn.SessionID != conversation.RootSessionID || turn.Status != defs.TurnStatusCompleted {
						t.Fatalf("turn = %+v, want a completed Turn of the root Session", turn)
					}
				}
				params["turn_ids"] = []defs.TurnID{"missing"}
				response := client.callResult(t, "session/turns/get", params, nil)
				if string(response.Result) != `{"turns":[]}` {
					t.Fatalf("session/turns/get of a missing Turn = %s, want an empty array", response.Result)
				}
			},
		},
		{
			// 缺 conversation_id 或 session_id 时，session/turns/list 和 session/turns/get 都回 -32000 和同样的缺失说明。
			name: "turns_require_conversation_id_and_session_id",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				conversation := client.createConversation(t)
				tests := []struct {
					method      string
					params      map[string]any
					wantMessage string
				}{
					{
						method:      "session/turns/list",
						params:      map[string]any{"session_id": conversation.RootSessionID},
						wantMessage: "list Turns requires a conversation ID",
					},
					{
						method:      "session/turns/list",
						params:      map[string]any{"conversation_id": conversation.ID},
						wantMessage: "list Turns requires a session ID",
					},
					{
						method:      "session/turns/get",
						params:      map[string]any{"session_id": conversation.RootSessionID, "turn_ids": []defs.TurnID{"t1"}},
						wantMessage: "get Turns requires a conversation ID",
					},
					{
						method:      "session/turns/get",
						params:      map[string]any{"conversation_id": conversation.ID, "turn_ids": []defs.TurnID{"t1"}},
						wantMessage: "get Turns requires a session ID",
					},
				}
				for _, tt := range tests {
					requireErrorCode(t, client.call(t, tt.method, tt.params), -32000, tt.wantMessage)
				}
			},
		},
		{
			// 空闲的对话手动压缩：立即返回压缩 Turn 的 turn_id，摘要由同一个模型生成，随后 Session 订阅收到这个 Turn 的 turn/finished。
			name: "compact_idle_conversation",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				conversation := client.createConversation(t)
				client.callResult(t, "session/subscribe", sessionTarget(conversation, conversation.RootSessionID), nil)
				submitText(t, client, conversation, "第一轮")
				h.model.receive(t).reply <- serverModelResponse{chunks: []string{"好"}}
				client.collectNotifications(t, isMethod("turn/finished"))

				turnID := compactWhenIdle(t, client, conversation)
				h.model.receive(t).reply <- serverModelResponse{chunks: []string{"摘要内容"}}
				notifications := client.collectNotifications(t, isMethod("turn/finished"))
				if finished := decodeNotification(t, notifications[len(notifications)-1]); finished.TurnID != turnID || finished.Status != defs.TurnStatusCompleted {
					t.Fatalf("turn/finished = %+v, want completed Turn %s", finished, turnID)
				}
				items := client.waitItems(t, conversation, conversation.RootSessionID, 3)
				if summary := items[2].Item; summary.Kind != defs.ItemContextSummary || summary.ContextSummary.Text != "摘要内容" || summary.TurnID != turnID {
					t.Fatalf("last item = %+v, want the context summary of Turn %s", summary, turnID)
				}
			},
		},
		{
			name: "compact_busy_error_code",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				conversation := client.createConversation(t)
				submitText(t, client, conversation, "长任务")
				request := h.model.receive(t)
				requireErrorCode(t, client.call(t, "conversation/compact", map[string]any{"conversation_id": conversation.ID}), -32001, "session is busy")
				request.reply <- serverModelResponse{chunks: []string{"好"}}
			},
		},
		{
			name: "list_sessions",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				conversation := client.createConversation(t)
				var result struct {
					Sessions []defs.Session `json:"sessions"`
				}
				client.callResult(t, "session/list", map[string]any{"conversation_id": conversation.ID}, &result)
				if len(result.Sessions) != 1 || result.Sessions[0].ID != conversation.RootSessionID {
					t.Fatalf("sessions = %+v", result.Sessions)
				}
			},
		},
		{
			// 只凭 SessionID 找到 Session 和它所属的对话；Session 写过 Item 之后才找得到。
			name: "get_session_by_id",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				conversation := client.createConversation(t)
				completeFirstRound(t, h, client, conversation)
				var result struct {
					Session defs.Session `json:"session"`
				}
				client.callResult(t, "session/get", map[string]any{"session_id": conversation.RootSessionID}, &result)
				if result.Session.ID != conversation.RootSessionID || result.Session.ConversationID != conversation.ID {
					t.Fatalf("session/get = %+v, want the root Session of %s", result.Session, conversation.ID)
				}
			},
		},
		{
			name: "get_session_by_id_not_found_error_code",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				requireErrorCode(t, client.call(t, "session/get", map[string]any{"session_id": defs.NewSessionID()}), -32003, "session not found")
			},
		},
		{
			// 刚加载的对话还没有测量：来源 unknown，items 是空数组而不是 null，没有 last_request。
			name: "session_context_usage_before_measurement",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				conversation := client.createConversation(t)
				response := client.callResult(t, "session/contextUsage", sessionTarget(conversation, conversation.RootSessionID), nil)
				var usage map[string]json.RawMessage
				if err := json.Unmarshal(response.Result, &usage); err != nil {
					t.Fatal(err)
				}
				if !hasKeys(usage, "window", "threshold_tokens", "next_request", "system_tokens", "tool_tokens", "items") || hasKeys(usage, "last_request") {
					t.Fatalf("contextUsage result = %s", response.Result)
				}
				var nextRequest struct {
					Source string `json:"source"`
				}
				if err := json.Unmarshal(usage["next_request"], &nextRequest); err != nil || nextRequest.Source != "unknown" || string(usage["items"]) != "[]" {
					t.Fatalf("next_request = %s, items = %s", usage["next_request"], usage["items"])
				}
				h.model.requireNoRequest(t)
			},
		},
		{
			name: "session_context_usage_after_turn",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				conversation := client.createConversation(t)
				client.callResult(t, "conversation/subscribe", map[string]any{"conversation_id": conversation.ID}, nil)
				submitText(t, client, conversation, "你好")
				h.model.receive(t).reply <- serverModelResponse{chunks: []string{"好"}}
				client.collectNotifications(t, func(message clientMessage) bool {
					info := decodeNotification(t, message).Info
					return info["state"] == "idle" && info["last_turn_status"] == "completed"
				})
				var usage struct {
					NextRequest struct {
						Source string `json:"source"`
					} `json:"next_request"`
					SystemTokens int `json:"system_tokens"`
					Items        []struct {
						Category string `json:"category"`
					} `json:"items"`
				}
				client.callResult(t, "session/contextUsage", sessionTarget(conversation, conversation.RootSessionID), &usage)
				if usage.NextRequest.Source != "estimated" || usage.SystemTokens <= 0 || len(usage.Items) < 2 || usage.Items[0].Category != "user_input" {
					t.Fatalf("contextUsage = %+v", usage)
				}
			},
		},
		{
			name: "session_details",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				conversation := client.createConversation(t)
				client.callResult(t, "conversation/setTitle", map[string]any{"conversation_id": conversation.ID, "title": "周报"}, nil)
				response := client.callResult(t, "session/details", sessionTarget(conversation, conversation.RootSessionID), nil)
				var details map[string]json.RawMessage
				if err := json.Unmarshal(response.Result, &details); err != nil {
					t.Fatal(err)
				}
				if !hasKeys(details, "conversation_id", "title", "session", "model", "compact_model", "executors", "skills", "tools") {
					t.Fatalf("details result = %s", response.Result)
				}
				var view struct {
					Title string `json:"title"`
					Model struct {
						Ref defs.ModelRef `json:"ref"`
					} `json:"model"`
					Executors []struct {
						ID                 string `json:"id"`
						Description        string `json:"description"`
						WorkspaceDirectory string `json:"workspace_directory"`
					} `json:"executors"`
					Tools []string `json:"tools"`
				}
				if err := json.Unmarshal(response.Result, &view); err != nil {
					t.Fatal(err)
				}
				if view.Title != "周报" || view.Model.Ref != (defs.ModelRef{
					ProviderID: "test",
					ModelID:    "test",
				}) || len(view.Executors) != 2 || view.Executors[0].ID != "local" ||
					view.Executors[0].Description != "本机" || view.Executors[0].WorkspaceDirectory != h.defaultDirectory ||
					!containsAll(view.Tools, "read_file", "spawn_agent") || string(details["skills"]) != `["`+filepath.Base(h.defaultDirectory)+`"]` {
					t.Fatalf("details = %s", response.Result)
				}
				// 默认测试模型没有推理强度，model 和 compact_model 都应输出 [] 而不是 null。
				if strings.Count(string(response.Result), `"reasoning_efforts":[]`) != 2 {
					t.Fatalf("details reasoning_efforts = %s", response.Result)
				}
				h.model.requireNoRequest(t)
			},
		},
		{
			name: "session_details_removed_model_has_no_model",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				record := defs.Conversation{
					ID:            defs.NewConversationID(),
					RootSessionID: defs.NewSessionID(),
				}
				root := defs.Session{
					ID:             record.RootSessionID,
					ConversationID: record.ID,
					AgentName:      "assistant",
					Model: defs.ModelSelection{Ref: defs.ModelRef{
						ProviderID: "test",
						ModelID:    "gone",
					}},
				}
				if err := h.store.mutation.SetSession(t.Context(), root); err != nil {
					t.Fatal(err)
				}
				if err := h.store.mutation.SetConversation(t.Context(), record); err != nil {
					t.Fatal(err)
				}
				client.callResult(t, "conversation/open", map[string]any{"conversation_id": record.ID}, nil)
				response := client.callResult(t, "session/details", sessionTarget(record, record.RootSessionID), nil)
				var details map[string]json.RawMessage
				if err := json.Unmarshal(response.Result, &details); err != nil {
					t.Fatal(err)
				}
				if string(details["model"]) != "null" || string(details["compact_model"]) != "null" {
					t.Fatalf("details result = %s, want null model and compact_model", response.Result)
				}
				h.model.requireNoRequest(t)
			},
		},
		{
			name: "session_pulls_reject_unknown_session",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				conversation := client.createConversation(t)
				for _, method := range []string{"session/contextUsage", "session/details"} {
					requireErrorCode(t, client.call(t, method, sessionTarget(conversation, defs.NewSessionID())), -32000, "")
				}
			},
		},
		{
			name: "agent_list_hides_instructions",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				response := client.callResult(t, "agent/list", nil, nil)
				const want = `{"agents":[{"name":"assistant","description":"主 Agent","default_model":{"provider_id":"test","model_id":"test"}}]}`
				if string(response.Result) != want {
					t.Fatalf("agent/list = %s, want %s", response.Result, want)
				}
			},
		},
		{
			name: "model_list_hides_connection",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				response := client.callResult(t, "model/list", map[string]any{"agent_name": "assistant"}, nil)
				raw := string(response.Result)
				for _, secret := range []string{h.model.server.URL, "api_key", "base_url", "protocol", "model-b"} {
					if strings.Contains(raw, secret) {
						t.Fatalf("model/list exposes %q: %s", secret, raw)
					}
				}
				if !strings.Contains(raw, `"agent_default_model":{"provider_id":"test","model_id":"test"}`) || !strings.Contains(raw, `"reasoning_efforts":[]`) {
					t.Fatalf("model/list = %s", raw)
				}
				withoutAgent := client.callResult(t, "model/list", nil, nil)
				if strings.Contains(string(withoutAgent.Result), "agent_default_model") {
					t.Fatalf("model/list without agent = %s", withoutAgent.Result)
				}
				requireErrorCode(t, client.call(t, "model/list", map[string]any{"agent_name": "missing"}), -32000, "not configured")
			},
		},
		{
			name: "set_model_and_create_with_model",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				conversation := client.createConversation(t)
				target := map[string]any{"conversation_id": conversation.ID, "session_id": conversation.RootSessionID}
				var result struct {
					Session defs.Session `json:"session"`
				}
				target["model"] = map[string]any{"ref": map[string]any{"provider_id": "test", "model_id": "b"}}
				client.callResult(t, "session/setModel", target, &result)
				if result.Session.Model != (defs.ModelSelection{
					Ref: defs.ModelRef{
						ProviderID: "test",
						ModelID:    "b",
					},
					ReasoningEffort: "low",
				}) {
					t.Fatalf("setModel = %+v", result.Session.Model)
				}
				target["model"] = map[string]any{"ref": map[string]any{"provider_id": "test", "model_id": "b"}, "reasoning_effort": "max"}
				requireErrorCode(t, client.call(t, "session/setModel", target), -32000, "does not support reasoning effort")
				target["model"] = map[string]any{"ref": map[string]any{"provider_id": "test", "model_id": "gone"}}
				requireErrorCode(t, client.call(t, "session/setModel", target), -32000, "not configured")

				var created struct {
					Conversation defs.Conversation `json:"conversation"`
				}
				client.callResult(t, "conversation/create", map[string]any{"agent_name": "assistant",
					"model": map[string]any{"ref": map[string]any{"provider_id": "test", "model_id": "b"}, "reasoning_effort": "high"}}, &created)
				var sessions struct {
					Sessions []defs.Session `json:"sessions"`
				}
				client.callResult(t, "session/list", map[string]any{"conversation_id": created.Conversation.ID}, &sessions)
				if len(sessions.Sessions) != 1 || sessions.Sessions[0].Model != (defs.ModelSelection{
					Ref: defs.ModelRef{
						ProviderID: "test",
						ModelID:    "b",
					},
					ReasoningEffort: "high",
				}) {
					t.Fatalf("created root = %+v", sessions.Sessions)
				}
			},
		},
	}
	// 2. 初始化协议客户端，检查对话与 Session 方法的返回字段、输入去重、压缩结果和业务错误码。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SHELL", "/bin/bash")
			h := startTestServer(t)
			client := h.dial(t)
			client.initialize(t)
			tt.run(t, h, client)
		})
	}
}

// TestOpenVaalaInitialize 检查初始化返回的协议版本和 ServerID。
func TestOpenVaalaInitialize(t *testing.T) {
	// 1. 列出配置了和没配 ServerID 的测试场景。
	tests := []struct {
		name     string
		serverID string
	}{
		{
			name:     "returns_configured_server_id",
			serverID: "server-abc",
		},
		{name: "empty_server_id_when_not_configured"},
	}
	// 2. 配置各场景的 ServerID，检查初始化响应中的服务标识和协议版本。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := startTestServer(t, func(opt *runtimeserver.RuntimeServerOpt) { opt.ServerID = tt.serverID })
			var result struct {
				ProtocolVersion string `json:"protocol_version"`
				ServerID        string `json:"server_id"`
			}
			h.dial(t).callResult(t, "initialize", map[string]any{"client_info": map[string]any{"name": "test"}, "protocol_version": "5"}, &result)
			if result.ServerID != tt.serverID || result.ProtocolVersion != "5" {
				t.Fatalf("initialize result = %+v, want server_id %q and protocol version 4", result, tt.serverID)
			}
		})
	}
}

// TestOpenVaalaConversationManagement 检查协议分叉、归档、删除及对应错误码。
func TestOpenVaalaConversationManagement(t *testing.T) {
	// 1. 列出协议分叉、归档、删除及对应错误码的测试场景。
	tests := []struct {
		name string
		// run 的 pipeline 是 serve 的内存遥测管道，用来查日志。
		run func(t *testing.T, h *serverHarness, client *testClient, pipeline *telemetrytest.Pipeline)
	}{
		{
			name: "fork_assembles_environment_and_opens",
			run: func(t *testing.T, h *serverHarness, client *testClient, _ *telemetrytest.Pipeline) {
				source := client.createConversation(t)
				completeFirstRound(t, h, client, source)
				settings := defs.ExecutorSettings{WorkspaceDirectories: map[string]string{defs.LocalExecutorID: t.TempDir()}}
				var forked struct {
					Conversation defs.Conversation `json:"conversation"`
				}
				client.callResult(t, "conversation/fork", map[string]any{
					"conversation_id": source.ID, "executor_settings": settings, "environment": []string{"FORK_MARK=forked"},
				}, &forked)
				record := forked.Conversation
				if record.ID == source.ID || record.ForkedFromID != source.ID || record.MessageCount != 2 || !reflect.DeepEqual(record.ExecutorSettings, settings) {
					t.Fatalf("forked = %+v, want a new conversation forked from %s with 2 messages and %+v", record, source.ID, settings)
				}
				isOpen, err := h.runtime.IsConversationOpen(t.Context(), &runtime.GetConversationRequest{ConversationID: record.ID})
				if err != nil || !isOpen {
					t.Fatalf("forked conversation open = %v, %v, want open", isOpen, err)
				}
				client.callResult(t, "session/subscribe", sessionTarget(record, record.RootSessionID), nil)
				if result := runToolTurn(t, h, client, record, "bash", `{"command":"echo $FORK_MARK"}`); !strings.Contains(result, `"output":"forked\n"`) {
					t.Fatalf("bash result = %s, want the environment given to fork", result)
				}
			},
		},
		{
			// 来源记录里原本就有、已从配置删除的 Executor 分叉时可以原样带上；新加入的未知 ID 仍然拒绝。
			name: "fork_keeps_source_unknown_executor",
			run: func(t *testing.T, h *serverHarness, client *testClient, _ *telemetrytest.Pipeline) {
				settings := defs.ExecutorSettings{
					ExecutorIDs:          []string{defs.LocalExecutorID, "gone"},
					WorkspaceDirectories: map[string]string{defs.LocalExecutorID: h.defaultDirectory},
				}
				source := saveStoredConversation(t, h, settings)
				// 来源有一轮历史，走复制历史再打开的分叉；没有历史、按新建对话走的见下一个用例。
				client.callResult(t, "conversation/open", map[string]any{"conversation_id": source.ID}, nil)
				client.callResult(t, "session/subscribe", sessionTarget(source, source.RootSessionID), nil)
				completeFirstRound(t, h, client, source)
				var forked struct {
					Conversation defs.Conversation `json:"conversation"`
				}
				client.callResult(t, "conversation/fork", map[string]any{"conversation_id": source.ID, "executor_settings": settings}, &forked)
				if forked.Conversation.ID == "" || forked.Conversation.ID == source.ID || !reflect.DeepEqual(forked.Conversation.ExecutorSettings, settings) {
					t.Fatalf("forked = %+v, want a new conversation with %+v", forked.Conversation, settings)
				}
				added := defs.ExecutorSettings{ExecutorIDs: []string{defs.LocalExecutorID, "gone", "gpu"}}
				response := client.call(t, "conversation/fork", map[string]any{"conversation_id": source.ID, "executor_settings": added})
				if want := `invalid executor settings: unknown executor "gpu"`; response.Error == nil || response.Error.Code != -32602 || response.Error.Message != want {
					t.Fatalf("fork adding an unknown executor error = %+v, want -32602 %q", response.Error, want)
				}
				// 打开失败的分叉不留下复制出的对话：列表里只有来源和第一次分叉出的。
				if got := listConversationIDs(t, client, nil); len(got) != 2 {
					t.Fatalf("list after the rejected fork = %v, want the source and the first fork", got)
				}
			},
		},
		{
			// 截取点之前一轮都没有（回退到开头）时按新建对话打开：来源记录里原本就有、已从配置删除的 Executor 同样原样带上，只记 Warn。
			name: "fork_before_first_turn_keeps_source_unknown_executor",
			run: func(t *testing.T, h *serverHarness, client *testClient, pipeline *telemetrytest.Pipeline) {
				settings := defs.ExecutorSettings{
					ExecutorIDs:          []string{defs.LocalExecutorID, "gone"},
					WorkspaceDirectories: map[string]string{defs.LocalExecutorID: h.defaultDirectory},
				}
				source := saveStoredConversation(t, h, settings)
				var forked struct {
					Conversation defs.Conversation `json:"conversation"`
				}
				client.callResult(t, "conversation/fork", map[string]any{"conversation_id": source.ID, "executor_settings": settings}, &forked)
				if forked.Conversation.ID == "" || forked.Conversation.ID == source.ID || forked.Conversation.ForkedFromID != "" ||
					!reflect.DeepEqual(forked.Conversation.ExecutorSettings, settings) {
					t.Fatalf("forked = %+v, want a new conversation created with %+v", forked.Conversation, settings)
				}
				records := pipeline.WaitLogRecords(t, "executor in conversation record not found", 1)
				if records[0]["level"] != "WARN" || records[0]["openvaala.executor.id"] != "gone" {
					t.Fatalf("warn records = %v, want one for gone", records)
				}
			},
		},
		{
			name: "fork_boundary_errors",
			run: func(t *testing.T, h *serverHarness, client *testClient, _ *telemetrytest.Pipeline) {
				source := client.createConversation(t)
				completeFirstRound(t, h, client, source)
				submitText(t, client, source, "长任务")
				request := h.model.receive(t)
				var turns turnsPageView
				client.callResult(t, "session/turns/list", map[string]any{"conversation_id": source.ID, "session_id": source.RootSessionID}, &turns)
				if len(turns.Turns) != 2 || turns.Turns[1].Status != defs.TurnStatusRunning {
					t.Fatalf("turns = %+v, want a finished turn and a running one", turns.Turns)
				}
				finished, running := turns.Turns[0].ID, turns.Turns[1].ID
				for _, tt := range []struct {
					name   string
					params map[string]any
					want   error
				}{
					{
						name:   "turn_not_found",
						params: map[string]any{"before_turn_id": "missing-turn"},
						want:   conversation.ErrForkTurnNotFound,
					},
					{
						name:   "boundary_conflict",
						params: map[string]any{"before_turn_id": running, "last_turn_id": finished},
						want:   conversation.ErrForkBoundaryConflict,
					},
					{
						name:   "turn_running",
						params: map[string]any{"last_turn_id": running},
						want:   conversation.ErrForkTurnRunning,
					},
				} {
					tt.params["conversation_id"], tt.params["executor_settings"] = source.ID, defs.ExecutorSettings{}
					response := client.call(t, "conversation/fork", tt.params)
					if response.Error == nil || response.Error.Code != -32602 || response.Error.Message != tt.want.Error() {
						t.Fatalf("%s error = %+v, want -32602 %q", tt.name, response.Error, tt.want.Error())
					}
				}
				request.reply <- serverModelResponse{chunks: []string{"好"}}
			},
		},
		{
			name: "archive_and_list",
			run: func(t *testing.T, h *serverHarness, client *testClient, _ *telemetrytest.Pipeline) {
				archived, kept := client.createConversation(t), client.createConversation(t)
				completeFirstRound(t, h, client, archived)
				completeFirstRound(t, h, client, kept)
				var result struct {
					Conversation defs.Conversation `json:"conversation"`
				}
				client.callResult(t, "conversation/setArchived", map[string]any{"conversation_id": archived.ID, "archived": true}, &result)
				if result.Conversation.ID != archived.ID || result.Conversation.ArchivedAt == 0 {
					t.Fatalf("archived = %+v, want a non-zero archived_at", result.Conversation)
				}
				if got := listConversationIDs(t, client, nil); !slices.Equal(got, []defs.ConversationID{kept.ID}) {
					t.Fatalf("list = %v, want only %s", got, kept.ID)
				}
				if got := listConversationIDs(t, client, map[string]any{"archived": true}); !slices.Equal(got, []defs.ConversationID{archived.ID}) {
					t.Fatalf("archived list = %v, want only %s", got, archived.ID)
				}
				var unarchived struct {
					Conversation defs.Conversation `json:"conversation"`
				}
				client.callResult(t, "conversation/setArchived", map[string]any{"conversation_id": archived.ID, "archived": false}, &unarchived)
				if unarchived.Conversation.ID != archived.ID || unarchived.Conversation.ArchivedAt != 0 {
					t.Fatalf("unarchived = %+v, want archived_at 0", unarchived.Conversation)
				}
				if got := listConversationIDs(t, client, nil); !slices.Equal(got, []defs.ConversationID{archived.ID, kept.ID}) {
					t.Fatalf("list after unarchive = %v, want %s and %s", got, archived.ID, kept.ID)
				}
			},
		},
		{
			name: "delete",
			run: func(t *testing.T, h *serverHarness, client *testClient, _ *telemetrytest.Pipeline) {
				created := client.createConversation(t)
				completeFirstRound(t, h, client, created)
				submitText(t, client, created, "长任务")
				request := h.model.receive(t)
				target := map[string]any{"conversation_id": created.ID}
				requireErrorCode(t, client.call(t, "conversation/delete", target), -32001, "session is busy")
				request.reply <- serverModelResponse{chunks: []string{"好"}}
				// 上一轮的结束先于 Session 清掉活动 Turn 可见，遇到 -32001 时重试到空闲。
				deadline := time.Now().Add(5 * time.Second)
				for {
					response := client.call(t, "conversation/delete", target)
					if response.Error == nil {
						if string(response.Result) != "{}" {
							t.Fatalf("conversation/delete result = %s, want {}", response.Result)
						}
						break
					}
					if response.Error.Code != -32001 || time.Now().After(deadline) {
						t.Fatalf("conversation/delete error = %+v", response.Error)
					}
					time.Sleep(10 * time.Millisecond)
				}
				requireErrorCode(t, client.call(t, "conversation/open", target), -32002, "conversation not found")
				if got := listConversationIDs(t, client, nil); len(got) != 0 {
					t.Fatalf("list after delete = %v, want empty", got)
				}
			},
		},
	}
	// 2. 通过协议分叉、归档和删除对话，检查环境、列表分组、错误码及删除后的查询结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pipeline := telemetrytest.NewPipeline(t)
			h := startTestServerWithTelemetry(t, pipeline.Telemetry)
			client := h.dial(t)
			client.initialize(t)
			tt.run(t, h, client, pipeline)
		})
	}
}

// listConversationIDs 按创建顺序返回一页对话 ID。
func listConversationIDs(t *testing.T, client *testClient, params map[string]any) []defs.ConversationID {
	t.Helper()
	// 1. 调用 conversation/list 并解析列表。
	var list struct {
		Conversations []dao.ConversationListEntry `json:"conversations"`
	}
	client.callResult(t, "conversation/list", params, &list)
	// 2. 按结果顺序提取对话 ID。
	ids := make([]defs.ConversationID, len(list.Conversations))
	for index, record := range list.Conversations {
		ids[index] = record.Conversation.ID
	}
	return ids
}

// TestOpenVaalaExecutorSettings 检查 Executor 设置、环境变量和改设置后换目录。
func TestOpenVaalaExecutorSettings(t *testing.T) {
	// 1. 列出 Executor 设置、环境变量和改设置后换目录的测试场景。
	tests := []struct {
		name string
		run  func(t *testing.T, h *serverHarness, client *testClient)
	}{
		{
			name: "create_with_local_directory",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				directory := newMarkerDirectory(t, "A_MARKER")
				settings := defs.ExecutorSettings{WorkspaceDirectories: map[string]string{defs.LocalExecutorID: directory}}
				conversation := createConversationWith(t, client, map[string]any{"executor_settings": settings})
				if !reflect.DeepEqual(conversation.ExecutorSettings, settings) {
					t.Fatalf("executor_settings = %+v, want %+v", conversation.ExecutorSettings, settings)
				}
				if result := runToolTurn(t, h, client, conversation, "read_file", `{"path":"marker.txt"}`); !strings.Contains(result, "A_MARKER") {
					t.Fatalf("read_file result = %s, want the file in the requested directory", result)
				}
			},
		},
		{
			name: "create_without_settings_uses_serve_directory",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				conversation := client.createConversation(t)
				want := defs.ExecutorSettings{WorkspaceDirectories: map[string]string{defs.LocalExecutorID: h.defaultDirectory}}
				if !reflect.DeepEqual(conversation.ExecutorSettings, want) {
					t.Fatalf("executor_settings = %+v, want %+v", conversation.ExecutorSettings, want)
				}
			},
		},
		{
			name: "create_rejects_invalid_settings",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				for _, tt := range []struct {
					settings map[string]any
					want     string
				}{
					{
						settings: map[string]any{"executor_ids": []string{"local", "gone"}},
						want:     `invalid executor settings: unknown executor "gone"`,
					},
					{
						settings: map[string]any{"default_executor_id": "gone"},
						want:     `invalid executor settings: unknown executor "gone"`,
					},
					{
						settings: map[string]any{"default_executor_id": "a", "executor_ids": []string{"local"}},
						want:     `invalid executor settings: default executor "a" is not in executor_ids`,
					},
				} {
					response := client.call(t, "conversation/create", map[string]any{"agent_name": "assistant", "executor_settings": tt.settings})
					if response.Error == nil || response.Error.Code != -32602 || response.Error.Message != tt.want {
						t.Fatalf("create with %v error = %+v, want -32602 %q", tt.settings, response.Error, tt.want)
					}
				}
				var list struct {
					Conversations []dao.ConversationListEntry `json:"conversations"`
				}
				client.callResult(t, "conversation/list", nil, &list)
				if len(list.Conversations) != 0 {
					t.Fatalf("conversations = %+v, want none saved", list.Conversations)
				}
			},
		},
		{
			// 不合法的设置什么都不改，记录不变。
			name: "set_rejects_invalid_settings",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				conversation := createConversationWith(t, client, nil)
				for _, tt := range []struct {
					settings map[string]any
					want     string
				}{
					{
						settings: map[string]any{"executor_ids": []string{"local", "gone"}},
						want:     `invalid executor settings: unknown executor "gone"`,
					},
					{
						settings: map[string]any{"default_executor_id": "a", "executor_ids": []string{"local"}},
						want:     `invalid executor settings: default executor "a" is not in executor_ids`,
					},
				} {
					response := client.call(t, "conversation/setExecutorSettings", map[string]any{"conversation_id": conversation.ID, "executor_settings": tt.settings})
					if response.Error == nil || response.Error.Code != -32602 || response.Error.Message != tt.want {
						t.Fatalf("set %v error = %+v, want -32602 %q", tt.settings, response.Error, tt.want)
					}
				}
				var read struct {
					Conversation defs.Conversation `json:"conversation"`
				}
				client.callResult(t, "conversation/read", map[string]any{"conversation_id": conversation.ID}, &read)
				if !reflect.DeepEqual(read.Conversation.ExecutorSettings, conversation.ExecutorSettings) {
					t.Fatalf("saved executor_settings = %+v, want unchanged %+v", read.Conversation.ExecutorSettings, conversation.ExecutorSettings)
				}
			},
		},
		{
			// 记录里原本就有、已从配置删除的 Executor 改设置时可以保留；新加入的未知 ID、改成未知的默认 Executor 仍然拒绝。
			name: "set_keeps_stored_unknown_executor",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				stored := saveStoredConversation(t, h, defs.ExecutorSettings{
					ExecutorIDs:          []string{defs.LocalExecutorID, "gone"},
					WorkspaceDirectories: map[string]string{defs.LocalExecutorID: h.defaultDirectory},
				})
				for _, tt := range []struct {
					name      string
					settings  defs.ExecutorSettings
					wantError string
				}{
					{
						name:      "add_new_unknown",
						settings:  defs.ExecutorSettings{ExecutorIDs: []string{defs.LocalExecutorID, "gone", "gpu"}},
						wantError: `invalid executor settings: unknown executor "gpu"`,
					},
					{
						name: "unknown_default_changed",
						settings: defs.ExecutorSettings{
							DefaultExecutorID: "gone",
							ExecutorIDs:       []string{defs.LocalExecutorID, "gone"},
						},
						wantError: `invalid executor settings: unknown executor "gone"`,
					},
					{
						name: "change_directory_only",
						settings: defs.ExecutorSettings{
							ExecutorIDs:          []string{defs.LocalExecutorID, "gone"},
							WorkspaceDirectories: map[string]string{defs.LocalExecutorID: t.TempDir()},
						},
					},
				} {
					params := map[string]any{"conversation_id": stored.ID, "executor_settings": tt.settings}
					if tt.wantError != "" {
						response := client.call(t, "conversation/setExecutorSettings", params)
						if response.Error == nil || response.Error.Code != -32602 || response.Error.Message != tt.wantError {
							t.Fatalf("%s error = %+v, want -32602 %q", tt.name, response.Error, tt.wantError)
						}
						continue
					}
					var result struct {
						Conversation defs.Conversation `json:"conversation"`
					}
					client.callResult(t, "conversation/setExecutorSettings", params, &result)
					if !reflect.DeepEqual(result.Conversation.ExecutorSettings, tt.settings) {
						t.Fatalf("%s executor_settings = %+v, want %+v", tt.name, result.Conversation.ExecutorSettings, tt.settings)
					}
				}
			},
		},
		{
			name: "set_while_turn_running_is_busy",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				conversation := createConversationWith(t, client, nil)
				submitText(t, client, conversation, "长任务")
				request := h.model.receive(t)
				client.collectNotifications(t, isMethod("turn/started"))
				settings := defs.ExecutorSettings{WorkspaceDirectories: map[string]string{defs.LocalExecutorID: t.TempDir()}}
				response := client.call(t, "conversation/setExecutorSettings", map[string]any{"conversation_id": conversation.ID, "executor_settings": settings})
				requireErrorCode(t, response, -32001, "session is busy")
				request.reply <- serverModelResponse{chunks: []string{"好"}}
				client.collectNotifications(t, isMethod("turn/finished"))
			},
		},
		{
			// 对话按新设置原地重新装配，返回保存后的记录；旧订阅关闭，重新订阅后的工具在新目录执行。
			name: "set_reconfigures_conversation",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				conversation := createConversationWith(t, client, nil)
				settings := defs.ExecutorSettings{WorkspaceDirectories: map[string]string{defs.LocalExecutorID: newMarkerDirectory(t, "B_MARKER")}}
				var result struct {
					Conversation defs.Conversation `json:"conversation"`
				}
				client.callResult(t, "conversation/setExecutorSettings", map[string]any{"conversation_id": conversation.ID, "executor_settings": settings}, &result)
				if result.Conversation.ID != conversation.ID || !reflect.DeepEqual(result.Conversation.ExecutorSettings, settings) {
					t.Fatalf("set result = %+v, want conversation %s with executor_settings %+v", result.Conversation, conversation.ID, settings)
				}
				client.collectNotifications(t, isMethod("subscription/closed"))
				client.callResult(t, "session/subscribe", sessionTarget(conversation, conversation.RootSessionID), nil)
				if result := runToolTurn(t, h, client, conversation, "read_file", `{"path":"marker.txt"}`); !strings.Contains(result, "B_MARKER") {
					t.Fatalf("read_file result = %s, want the file in the new directory", result)
				}
			},
		},
		{
			// 记录里的 Executor 被删出配置后，打开对话只记 Warn，不报错。
			name: "cold_open_tolerates_unknown_executor",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				settings := defs.ExecutorSettings{
					DefaultExecutorID:    "gone",
					ExecutorIDs:          []string{defs.LocalExecutorID, "gone"},
					WorkspaceDirectories: map[string]string{defs.LocalExecutorID: h.defaultDirectory},
				}
				stored := saveStoredConversation(t, h, settings)
				var opened struct {
					Conversation defs.Conversation `json:"conversation"`
				}
				client.callResult(t, "conversation/open", map[string]any{"conversation_id": stored.ID}, &opened)
				if !reflect.DeepEqual(opened.Conversation.ExecutorSettings, settings) {
					t.Fatalf("opened executor_settings = %+v, want the record unchanged %+v", opened.Conversation.ExecutorSettings, settings)
				}
			},
		},
		{
			name: "open_unknown_conversation_fails",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				requireErrorCode(t, client.call(t, "conversation/open", map[string]any{"conversation_id": defs.NewConversationID()}), -32002, "not found")
			},
		},
		{
			name: "draft_skill_list_by_settings",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				directory := filepath.Join(t.TempDir(), "project-b")
				writeTestSkill(t, filepath.Join(directory, ".agents", "skills"), "project-b")
				for _, tt := range []struct {
					name   string
					params map[string]any
					want   string
				}{
					{
						name:   "with_local_directory",
						params: map[string]any{"agent_name": "assistant", "executor_settings": map[string]any{"workspace_directories": map[string]string{"local": directory}}},
						want:   "project-b",
					},
					{
						name:   "without_settings",
						params: map[string]any{"agent_name": "assistant"},
						want:   filepath.Base(h.defaultDirectory),
					},
				} {
					var skills []struct {
						Name     string `json:"name"`
						Executor string `json:"executor"`
					}
					client.callResult(t, "skill/list", tt.params, &skills)
					if len(skills) != 1 || skills[0].Name != tt.want || skills[0].Executor != defs.LocalExecutorID {
						t.Fatalf("%s skills = %+v, want [%s on local]", tt.name, skills, tt.want)
					}
				}
			},
		},
		{
			name: "create_with_environment",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				conversation := createConversationWith(t, client, map[string]any{"environment": []string{"TOOL_ENV_MARK=first"}})
				if result := runToolTurn(t, h, client, conversation, "bash", `{"command":"echo $TOOL_ENV_MARK"}`); !strings.Contains(result, `"output":"first\n"`) {
					t.Fatalf("bash result = %s, want the requested environment", result)
				}
			},
		},
		{
			name: "open_running_keeps_environment",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				conversation := createConversationWith(t, client, map[string]any{"environment": []string{"TOOL_ENV_MARK=first"}})
				runToolTurn(t, h, client, conversation, "bash", `{"command":"echo $TOOL_ENV_MARK"}`)
				client.callResult(t, "conversation/open", map[string]any{"conversation_id": conversation.ID, "environment": []string{"TOOL_ENV_MARK=second"}}, nil)
				if result := runToolTurn(t, h, client, conversation, "bash", `{"command":"echo $TOOL_ENV_MARK"}`); !strings.Contains(result, `"output":"first\n"`) {
					t.Fatalf("bash result = %s, want the environment of the first open", result)
				}
			},
		},
		{
			name: "multiline_environment_value",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				conversation := createConversationWith(t, client, map[string]any{"environment": []string{"BASH_FUNC_greet%%=() {  echo hi\n}"}})
				if result := runToolTurn(t, h, client, conversation, "bash", `{"command":"greet"}`); !strings.Contains(result, `"output":"hi\n"`) {
					t.Fatalf("bash result = %s, want the exported function to run", result)
				}
			},
		},
	}
	// 2. 创建对话或改设置，运行文件与命令工具，检查工作目录、环境变量、设置保存和无效设置错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SHELL", "/bin/bash")
			h := startTestServer(t)
			client := h.dial(t)
			client.initialize(t)
			tt.run(t, h, client)
		})
	}
}

// TestOpenVaalaExecutorMethods 检查 Executor 列表、目录解析及参数错误。
func TestOpenVaalaExecutorMethods(t *testing.T) {
	// 1. 列出 Executor 列表、目录解析及参数错误的测试场景。
	tests := []struct {
		name   string
		method string
		// params 在测试里按 serve 的工作目录生成。
		params func(defaultDirectory string) map[string]any
		// wantResult 是成功响应的 result 原文。
		wantResult func(defaultDirectory string) string
		// wantErrorMessage 非空时期望 -32602 和这条消息。
		wantErrorMessage string
	}{
		{
			name:   "list",
			method: "executor/list",
			wantResult: func(defaultDirectory string) string {
				return `{"executors":[{"id":"local","description":"本机","workspace_directory":` + strconv.Quote(defaultDirectory) + `},` +
					`{"id":"a","description":"虚拟","workspace_directory":"/workspace"}]}`
			},
		},
		{
			name:   "resolve_with_base_directory",
			method: "executor/resolveDirectory",
			params: func(defaultDirectory string) map[string]any {
				return map[string]any{"executor_id": "local", "directory": "sub", "base_directory": filepath.Join(defaultDirectory, "proj")}
			},
			wantResult: func(defaultDirectory string) string {
				return `{"workspace_directory":` + strconv.Quote(filepath.Join(defaultDirectory, "proj", "sub")) + `}`
			},
		},
		{
			name:   "resolve_without_base_directory",
			method: "executor/resolveDirectory",
			params: func(string) map[string]any { return map[string]any{"executor_id": "local", "directory": "sub"} },
			wantResult: func(defaultDirectory string) string {
				return `{"workspace_directory":` + strconv.Quote(filepath.Join(defaultDirectory, "sub")) + `}`
			},
		},
		{
			name:             "resolve_invalid_executor_settings",
			method:           "executor/resolveDirectory",
			params:           func(string) map[string]any { return map[string]any{"executor_id": "gone", "directory": "sub"} },
			wantErrorMessage: `invalid executor settings: unknown executor "gone"`,
		},
		{
			name:   "directories_without_base_directory",
			method: "executor/listDirectories",
			params: func(string) map[string]any { return map[string]any{"executor_id": "local", "directory": ""} },
			wantResult: func(defaultDirectory string) string {
				return `{"directory":` + strconv.Quote(defaultDirectory) + `,"directories":[".agents","proj","sub"]}`
			},
		},
		{
			name:   "directories_with_base_directory",
			method: "executor/listDirectories",
			params: func(defaultDirectory string) map[string]any {
				return map[string]any{"executor_id": "local", "directory": ".", "base_directory": filepath.Join(defaultDirectory, "proj")}
			},
			wantResult: func(defaultDirectory string) string {
				return `{"directory":` + strconv.Quote(filepath.Join(defaultDirectory, "proj")) + `,"directories":["sub"]}`
			},
		},
		{
			name:   "directories_empty_array",
			method: "executor/listDirectories",
			params: func(string) map[string]any { return map[string]any{"executor_id": "local", "directory": "sub"} },
			wantResult: func(defaultDirectory string) string {
				return `{"directory":` + strconv.Quote(filepath.Join(defaultDirectory, "sub")) + `,"directories":[]}`
			},
		},
		{
			name:             "directories_unknown_executor",
			method:           "executor/listDirectories",
			params:           func(string) map[string]any { return map[string]any{"executor_id": "gone", "directory": ""} },
			wantErrorMessage: `invalid executor settings: unknown executor "gone"`,
		},
		{
			name:             "directories_virtual_rejected",
			method:           "executor/listDirectories",
			params:           func(string) map[string]any { return map[string]any{"executor_id": "a", "directory": ""} },
			wantErrorMessage: `invalid executor settings: executor "a" does not support changing directory`,
		},
	}
	// 2. 创建测试目录，通过协议检查 Executor 列表、目录解析结果与无效 Executor 错误文本。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := startTestServer(t)
			if err := os.MkdirAll(filepath.Join(h.defaultDirectory, "proj", "sub"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(h.defaultDirectory, "sub"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(h.defaultDirectory, "proj"), filepath.Join(h.defaultDirectory, "link")); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(h.defaultDirectory, "file"), []byte("text"), 0600); err != nil {
				t.Fatal(err)
			}
			client := h.dial(t)
			client.initialize(t)
			var params map[string]any
			if tt.params != nil {
				params = tt.params(h.defaultDirectory)
			}
			response := client.call(t, tt.method, params)
			if tt.wantErrorMessage != "" {
				if response.Error == nil || response.Error.Code != -32602 || response.Error.Message != tt.wantErrorMessage {
					t.Fatalf("%s error = %+v, want -32602 %q", tt.method, response.Error, tt.wantErrorMessage)
				}
				return
			}
			if response.Error != nil || string(response.Result) != tt.wantResult(h.defaultDirectory) {
				t.Fatalf("%s response = %s (error %+v), want result %s", tt.method, response.Result, response.Error, tt.wantResult(h.defaultDirectory))
			}
		})
	}
}

// TestOpenVaalaMCPServerList 检查 MCP 连接字段、空列表和未打开对话错误。
func TestOpenVaalaMCPServerList(t *testing.T) {
	// 1. 准备多 Executor 的 MCP 连接记录与预期 JSON。
	servers := []conversation.MCPServerInfo{
		{
			ExecutorID: "local",
			ConnectionInfo: mcpclient.ConnectionInfo{
				ServerID: "fake",
				State:    mcpclient.ConnectionStateConnected,
				Required: true,
				Tools: []mcpclient.ConnectionTool{{
					Name:         "mcp__fake__echo",
					OriginalName: "echo",
					Description:  "回显",
					InputSchema:  json.RawMessage(`{"type":"object"}`),
				}},
			},
		},
		{
			ExecutorID: "extra",
			ConnectionInfo: mcpclient.ConnectionInfo{
				ServerID: "fake",
				State:    mcpclient.ConnectionStateFailed,
				Error:    "dial tcp: i/o timeout",
			},
		},
	}
	tests := []struct {
		name string
		// runtimeServers 非 nil 时 Runtime.GetMCPServers 返回它；为 nil 时用真实 Runtime。
		runtimeServers []conversation.MCPServerInfo
		// isOpen 是否先新建对话再查它？否则查一个从没打开过的对话。
		isOpen            bool
		wantResult        string
		wantMessage       string
		wantRuntimeCalled bool
	}{
		{
			name:           "servers_on_two_executors",
			runtimeServers: servers,
			isOpen:         true,
			wantResult: `{"servers":[` +
				`{"executor_id":"local","server_id":"fake","state":"connected","required":true,"tools":[{"name":"mcp__fake__echo","original_name":"echo","description":"回显","input_schema":{"type":"object"}}]},` +
				`{"executor_id":"extra","server_id":"fake","state":"failed","required":false,"tools":[],"error":"dial tcp: i/o timeout"}]}`,
			wantRuntimeCalled: true,
		},
		{
			name:       "no_servers_is_empty_array",
			isOpen:     true,
			wantResult: `{"servers":[]}`,
		},
		{
			name:        "conversation_not_open",
			wantMessage: "conversation is not open",
		},
	}
	// 2. 按场景打开对话，检查 MCP 连接 JSON、空工具列表及未打开对话错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := make(testCalls, 16)
			recording := &mcpServersRuntime{
				servers: tt.runtimeServers,
				calls:   calls,
			}
			h := startTestServer(t, func(opt *runtimeserver.RuntimeServerOpt) {
				recording.Runtime = opt.Runtime
				if tt.runtimeServers != nil {
					opt.Runtime = recording
				}
			})
			client := h.dial(t)
			client.initialize(t)
			conversationID := defs.ConversationID(uuid.NewString())
			if tt.isOpen {
				conversationID = client.createConversation(t).ID
			}
			response := client.call(t, "mcpServer/list", map[string]any{"conversation_id": conversationID})
			if tt.wantMessage != "" {
				if response.Error == nil || response.Error.Code != -32000 || response.Error.Message != tt.wantMessage {
					t.Fatalf("mcpServer/list error = %+v, want -32000 %q", response.Error, tt.wantMessage)
				}
				return
			}
			if response.Error != nil || string(response.Result) != tt.wantResult {
				t.Fatalf("mcpServer/list response = %s (error %+v), want result %s", response.Result, response.Error, tt.wantResult)
			}
			wantCalls := []testCall(nil)
			if tt.wantRuntimeCalled {
				wantCalls = []testCall{{
					method:   "GetMCPServers",
					argument: conversationID,
				}}
			}
			if got := calls.drain(); !reflect.DeepEqual(got, wantCalls) {
				t.Fatalf("Runtime calls = %+v, want %+v", got, wantCalls)
			}
		})
	}
}

// mcpServersRuntime 是测试用的 Runtime：GetMCPServers 把请求的对话 id 记进 calls 并返回 servers；其余方法用真实 Runtime。
type mcpServersRuntime struct {
	runtime.Runtime
	servers []conversation.MCPServerInfo
	calls   testCalls
}

// GetMCPServers 记录对话查询并返回预设 MCP 连接。
func (r *mcpServersRuntime) GetMCPServers(_ context.Context, request *runtime.GetConversationRequest) ([]conversation.MCPServerInfo, error) {
	// 1. 登记对话 ID 并返回 server 列表。
	r.calls.record("GetMCPServers", request.ConversationID)
	return r.servers, nil
}

// TestOpenVaalaConfigurationMethods 检查模型配置与目录方法的调用顺序和结果编码。
func TestOpenVaalaConfigurationMethods(t *testing.T) {
	// 1. 准备 Provider、目录模型和定义替换的预期调用。
	provider := defs.ProviderConfig{
		ID:       "gw",
		Protocol: "openai_chat",
		BaseURL:  "https://gw.example/v1",
		APIKey:   "sk-test",
		Models: map[string]*defs.ProviderModelConfig{"m1": {
			DisplayName:   "M1",
			ContextWindow: 128000,
		}},
	}
	const providerJSON = `{"id":"gw","protocol":"openai_chat","base_url":"https://gw.example/v1","api_key":"sk-test","models":{"m1":{"display_name":"M1","context_window":128000}}}`
	const modelsJSON = `{"models":{"m1":{"display_name":"M1","context_window":128000}}}`
	defaults := defs.ModelDefaults{
		ContextWindow:          200000,
		MaxOutputTokens:        16000,
		Thinking:               false,
		ReasoningEfforts:       []string{"low"},
		DefaultReasoningEffort: "custom",
		MergeMessageRoles:      []string{},
		Capabilities: defs.ProviderModelCapabilities{
			Input: defs.OptionalStringList{"text", "image"},
		},
	}
	const defaultsJSON = `{"context_window":200000,"max_output_tokens":16000,"thinking":false,"reasoning_efforts":["low"],"default_reasoning_effort":"custom","merge_message_roles":[],"capabilities":{"input":["text","image"]}}`
	agents := []*agent.AgentDefinition{{
		Name:         "assistant",
		Instructions: "reloaded",
	}}
	models := []*defs.ModelDefinition{{Ref: defs.ModelRef{
		ProviderID: "gw",
		ModelID:    "m1",
	}}}
	definitionsSet := testCall{
		method: "SetDefinitions",
		argument: runtime.Definitions{
			Agents: agents,
			Models: models,
		},
	}
	tests := []struct {
		name   string
		method string
		params any
		// busyConversationIDs 是假 Runtime 的 SetDefinitions 返回的忙对话。
		busyConversationIDs []defs.ConversationID
		// defaultModel 是 Configuration.ListProviders 返回的默认模型。
		defaultModel defs.ModelRef
		// wantCalls 是 Configuration、ModelCatalog 和 Runtime.SetDefinitions 按顺序收到的调用。
		wantCalls  []testCall
		wantResult string
	}{
		{
			name:   "provider_list",
			method: "provider/list",
			defaultModel: defs.ModelRef{
				ProviderID: "gw",
				ModelID:    "m1",
			},
			wantCalls:  []testCall{{method: "ListProviders"}},
			wantResult: `{"providers":[` + providerJSON + `],"default_model":"gw/m1","defaults":` + defaultsJSON + `}`,
		},
		{
			// 模型目录为空、没有默认模型时 default_model 是空字符串。
			name:       "provider_list_without_default_model",
			method:     "provider/list",
			wantCalls:  []testCall{{method: "ListProviders"}},
			wantResult: `{"providers":[` + providerJSON + `],"default_model":"","defaults":` + defaultsJSON + `}`,
		},
		{
			name:                "provider_set",
			method:              "provider/set",
			params:              map[string]any{"provider": json.RawMessage(providerJSON)},
			busyConversationIDs: []defs.ConversationID{"busy-1", "busy-2"},
			wantCalls: []testCall{{
				method:   "SetProvider",
				argument: testProviderWrite{provider: provider},
			}, definitionsSet},
			wantResult: `{"busy_conversation_ids":["busy-1","busy-2"]}`,
		},
		{
			// 带 default_model 时 Provider 和默认模型一起交给 Configuration，只替换一次 Runtime。
			name:   "provider_set_with_default_model",
			method: "provider/set",
			params: map[string]any{"provider": json.RawMessage(providerJSON), "default_model": "gw/m1"},
			wantCalls: []testCall{{
				method: "SetProvider",
				argument: testProviderWrite{
					provider: provider,
					defaultModel: defs.ModelRef{
						ProviderID: "gw",
						ModelID:    "m1",
					},
				},
			}, definitionsSet},
			wantResult: `{"busy_conversation_ids":[]}`,
		},
		{
			name:                "provider_delete",
			method:              "provider/delete",
			params:              map[string]any{"provider_id": "gw"},
			busyConversationIDs: []defs.ConversationID{"busy-1"},
			wantCalls: []testCall{{
				method:   "DeleteProvider",
				argument: "gw",
			}, definitionsSet},
			wantResult: `{"busy_conversation_ids":["busy-1"]}`,
		},
		{
			name:                "set_default_model",
			method:              "config/setDefaultModel",
			params:              map[string]any{"model": "gw/m1"},
			busyConversationIDs: []defs.ConversationID{"busy-1"},
			wantCalls: []testCall{{
				method: "SetDefaultModel",
				argument: defs.ModelRef{
					ProviderID: "gw",
					ModelID:    "m1",
				},
			}, definitionsSet},
			wantResult: `{"busy_conversation_ids":["busy-1"]}`,
		},
		{
			// 没有忙的对话时输出 []，不是 null。
			name:       "reload_without_busy_conversations",
			method:     "config/reload",
			wantCalls:  []testCall{{method: "Reload"}, definitionsSet},
			wantResult: `{"busy_conversation_ids":[]}`,
		},
		{
			name:       "catalog_providers_list",
			method:     "catalog/providers/list",
			wantCalls:  []testCall{{method: "ListCatalogProviders"}},
			wantResult: `{"providers":[{"id":"openai","name":"OpenAI","protocol":"openai_responses","base_url":"https://api.openai.com/v1","model_count":1}]}`,
		},
		{
			name:   "catalog_models_list",
			method: "catalog/models/list",
			params: map[string]any{"provider_id": "openai"},
			wantCalls: []testCall{{
				method:   "ListCatalogModels",
				argument: "openai",
			}},
			wantResult: modelsJSON,
		},
		{
			name:   "provider_fetch_models",
			method: "provider/fetchModels",
			params: map[string]any{"provider": json.RawMessage(providerJSON)},
			wantCalls: []testCall{{
				method:   "FetchProviderModels",
				argument: provider,
			}},
			wantResult: modelsJSON,
		},
	}
	// 2. 注入配置与目录夹具，检查协议结果及 Configuration、ModelCatalog、SetDefinitions 的调用顺序和参数。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := make(testCalls, 16)
			recording := &definitionsRecordingRuntime{
				result: runtime.SetDefinitionsResult{BusyConversationIDs: tt.busyConversationIDs},
				calls:  calls,
			}
			h := startTestServer(t, func(opt *runtimeserver.RuntimeServerOpt) {
				recording.Runtime = opt.Runtime
				opt.Runtime = recording
				opt.Configuration = &testConfiguration{
					providers:    []defs.ProviderConfig{provider},
					defaultModel: tt.defaultModel,
					defaults:     defaults,
					agents:       agents,
					models:       models,
					calls:        calls,
				}
				opt.ModelCatalog = &testModelCatalog{
					providers: []defs.CatalogProvider{{
						ID:         "openai",
						Name:       "OpenAI",
						Protocol:   "openai_responses",
						BaseURL:    "https://api.openai.com/v1",
						ModelCount: 1,
					}},
					models: provider.Models,
					calls:  calls,
				}
			})
			client := h.dial(t)
			client.initialize(t)
			response := client.call(t, tt.method, tt.params)
			if response.Error != nil || string(response.Result) != tt.wantResult {
				t.Fatalf("%s response = %s (error %+v), want result %s", tt.method, response.Result, response.Error, tt.wantResult)
			}
			if got := calls.drain(); !reflect.DeepEqual(got, tt.wantCalls) {
				t.Fatalf("%s calls = %+v, want %+v", tt.method, got, tt.wantCalls)
			}
		})
	}
}

// TestOpenVaalaConfigurationErrors 检查配置加载失败和模型引用格式错误。
func TestOpenVaalaConfigurationErrors(t *testing.T) {
	// 1. 列出配置加载失败和模型引用格式错误的测试场景。
	loadErr := errors.New(`agent "assistant" uses model gw/m1, which is not configured`)
	tests := []struct {
		name   string
		method string
		params any
		// wantCalls 是 Configuration 和 Runtime.SetDefinitions 按顺序收到的调用。
		wantCalls   []testCall
		wantCode    int
		wantMessage string
	}{
		{
			name:   "provider_set",
			method: "provider/set",
			params: map[string]any{"provider": map[string]any{"id": "gw"}},
			wantCalls: []testCall{{
				method:   "SetProvider",
				argument: testProviderWrite{provider: defs.ProviderConfig{ID: "gw"}},
			}},
			wantCode:    -32000,
			wantMessage: loadErr.Error(),
		},
		{
			name:   "provider_delete",
			method: "provider/delete",
			params: map[string]any{"provider_id": "gw"},
			wantCalls: []testCall{{
				method:   "DeleteProvider",
				argument: "gw",
			}},
			wantCode:    -32000,
			wantMessage: loadErr.Error(),
		},
		{
			name:   "set_default_model",
			method: "config/setDefaultModel",
			params: map[string]any{"model": "gw/m1"},
			wantCalls: []testCall{{
				method: "SetDefaultModel",
				argument: defs.ModelRef{
					ProviderID: "gw",
					ModelID:    "m1",
				},
			}},
			wantCode:    -32000,
			wantMessage: loadErr.Error(),
		},
		{
			name:        "reload",
			method:      "config/reload",
			wantCalls:   []testCall{{method: "Reload"}},
			wantCode:    -32000,
			wantMessage: loadErr.Error(),
		},
		{
			name:        "provider_set_with_default_model_without_provider",
			method:      "provider/set",
			params:      map[string]any{"provider": map[string]any{"id": "gw"}, "default_model": "m1"},
			wantCode:    -32602,
			wantMessage: `model "m1" must be provider_id/model_id`,
		},
		{
			name:        "set_default_model_without_provider",
			method:      "config/setDefaultModel",
			params:      map[string]any{"model": "m1"},
			wantCode:    -32602,
			wantMessage: `model "m1" must be provider_id/model_id`,
		},
	}
	// 2. 注入加载错误或无效模型引用，检查错误码、原始消息及未发生后续 Runtime 调用。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := make(testCalls, 16)
			recording := &definitionsRecordingRuntime{calls: calls}
			h := startTestServer(t, func(opt *runtimeserver.RuntimeServerOpt) {
				recording.Runtime = opt.Runtime
				opt.Runtime = recording
				opt.Configuration = &testConfiguration{
					err:   loadErr,
					calls: calls,
				}
			})
			client := h.dial(t)
			client.initialize(t)
			response := client.call(t, tt.method, tt.params)
			if response.Error == nil || response.Error.Code != tt.wantCode || response.Error.Message != tt.wantMessage {
				t.Fatalf("%s error = %+v, want %d %q", tt.method, response.Error, tt.wantCode, tt.wantMessage)
			}
			if got := calls.drain(); !reflect.DeepEqual(got, tt.wantCalls) {
				t.Fatalf("%s calls = %+v, want %+v", tt.method, got, tt.wantCalls)
			}
		})
	}
}

// definitionsRecordingRuntime 是测试用的 Runtime：SetDefinitions 只把请求记进 calls 并返回 result，不交给真实 Runtime；
// 其余方法用真实 Runtime。
type definitionsRecordingRuntime struct {
	runtime.Runtime
	result runtime.SetDefinitionsResult
	calls  testCalls
}

// SetDefinitions 记录定义替换请求并返回预设结果。
func (r *definitionsRecordingRuntime) SetDefinitions(_ context.Context, request *runtime.Definitions) (runtime.SetDefinitionsResult, error) {
	// 1. 复制请求到调用记录，返回预设忙碌对话列表。
	r.calls.record("SetDefinitions", *request)
	return r.result, nil
}

// ── Executor 设置测试夹具 ──

// newMarkerDirectory 创建带 marker.txt 的临时目录。
func newMarkerDirectory(t *testing.T, content string) string {
	t.Helper()
	// 1. 分配目录并写入指定标记文本。
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "marker.txt"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return directory
}

// saveStoredConversation 直接保存对话和根 Session，返回尚未打开的记录。
func saveStoredConversation(t *testing.T, h *serverHarness, settings defs.ExecutorSettings) defs.Conversation {
	t.Helper()
	// 1. 生成对话和根 Session ID，填入 Executor 设置及 assistant 模型。
	record := defs.Conversation{
		ID:               defs.NewConversationID(),
		RootSessionID:    defs.NewSessionID(),
		ExecutorSettings: settings,
	}
	root := defs.Session{
		ID:             record.RootSessionID,
		ConversationID: record.ID,
		AgentName:      "assistant",
		Model: defs.ModelSelection{Ref: defs.ModelRef{
			ProviderID: "test",
			ModelID:    "test",
		}},
	}
	// 2. 先保存根 Session，再保存对话记录。
	if err := h.store.mutation.SetSession(t.Context(), root); err != nil {
		t.Fatal(err)
	}
	if err := h.store.mutation.SetConversation(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	return record
}

// createConversationWith 附加指定参数创建对话，并订阅根 Session。
func createConversationWith(t *testing.T, client *testClient, extra map[string]any) defs.Conversation {
	t.Helper()
	// 1. 以 assistant 为 Agent 合并附加参数。
	params := map[string]any{"agent_name": "assistant"}
	for key, value := range extra {
		params[key] = value
	}
	// 2. 发送创建请求，登记根 Session 内容订阅并返回记录。
	var result struct {
		Conversation defs.Conversation `json:"conversation"`
	}
	client.callResult(t, "conversation/create", params, &result)
	client.callResult(t, "session/subscribe", sessionTarget(result.Conversation, result.Conversation.RootSessionID), nil)
	return result.Conversation
}

// runToolTurn 运行指定工具，返回模型请求中的最后一条工具结果文本。
func runToolTurn(t *testing.T, h *serverHarness, client *testClient, conversation defs.Conversation, toolName, arguments string) string {
	t.Helper()
	// 1. 提交输入，让模型调用指定工具。
	submitText(t, client, conversation, "执行工具")
	h.model.receive(t).reply <- serverModelResponse{calls: []llmkit.ToolCall{{
		ID:        "call-" + uuid.NewString(),
		Name:      toolName,
		Arguments: arguments,
	}}}
	// 2. 读取后续模型请求，提取最后一条工具结果文本。
	second := h.model.receive(t)
	messages, _ := second.body["messages"].([]any)
	var result string
	for _, message := range messages {
		if fields, _ := message.(map[string]any); fields["role"] == "tool" {
			result, _ = fields["content"].(string)
		}
	}
	// 3. 回复最终文本，等到 Turn 结束后返回工具结果。
	second.reply <- serverModelResponse{chunks: []string{"好"}}
	client.collectNotifications(t, isMethod("turn/finished"))
	return result
}

// TestOpenVaalaSubscriptions 检查内容与状态订阅、续传、响应顺序和退订。
func TestOpenVaalaSubscriptions(t *testing.T) {
	// 1. 列出内容与状态订阅、续传、响应顺序和退订的测试场景。
	tests := []struct {
		name string
		run  func(t *testing.T, h *serverHarness, client *testClient, conversation defs.Conversation)
	}{
		{
			name: "session_stream_events",
			run: func(t *testing.T, h *serverHarness, client *testClient, conversation defs.Conversation) {
				var subscribed subscribeSessionView
				client.callResult(t, "session/subscribe", sessionTarget(conversation, conversation.RootSessionID), &subscribed)
				if subscribed.LastSavedOrdinal != 0 || len(subscribed.StreamingItems) != 0 {
					t.Fatalf("snapshot = %+v", subscribed)
				}
				submitText(t, client, conversation, "你好")
				h.model.receive(t).reply <- serverModelResponse{chunks: []string{"你好", "！"}}
				notifications := client.collectNotifications(t, isMethod("turn/finished"))
				var methods []string
				var savedOrdinals []storage.Ordinal
				for _, message := range notifications {
					view := decodeNotification(t, message)
					if view.ConversationID != conversation.ID || view.SessionID != conversation.RootSessionID {
						t.Fatalf("%s target = %s/%s", message.Method, view.ConversationID, view.SessionID)
					}
					if view.Subscription != "session" {
						t.Fatalf("%s subscription = %q, want session", message.Method, view.Subscription)
					}
					if message.Method != "session/pendingInputsUpdated" {
						methods = append(methods, message.Method)
					}
					for _, logged := range view.Items {
						savedOrdinals = append(savedOrdinals, logged.Ordinal)
					}
				}
				// 根 Session 异步启动，session/started 若被本次订阅读到，应当位于所有通知之前。
				if methods[0] == "session/started" {
					methods = methods[1:]
				}
				if methods[0] != "turn/started" || !containsAll(methods, "item/started", "item/delta", "item/completed", "items/saved") {
					t.Fatalf("notification methods = %v", methods)
				}
				if len(savedOrdinals) != 2 || savedOrdinals[0] != 1 || savedOrdinals[1] != 2 {
					t.Fatalf("saved ordinals = %v, want [1 2]", savedOrdinals)
				}
			},
		},
		{
			// Agent 的 context_measured 和 model_call_finished 更新 Runtime 运行信息；内容订阅只接收前后的 item/* 等通知。
			name: "step_events_produce_no_content_notification",
			run: func(t *testing.T, h *serverHarness, client *testClient, conversation defs.Conversation) {
				client.callResult(t, "session/subscribe", sessionTarget(conversation, conversation.RootSessionID), nil)
				submitText(t, client, conversation, "你好")
				h.model.receive(t).reply <- serverModelResponse{chunks: []string{"你好", "！"}}
				var methods []string
				for _, message := range client.collectNotifications(t, isMethod("turn/finished")) {
					if message.Method == "" || len(message.Params) == 0 || string(message.Params) == "null" {
						t.Fatalf("notification method %q params %s: want a method and params", message.Method, message.Params)
					}
					if message.Method != "session/pendingInputsUpdated" {
						methods = append(methods, message.Method)
					}
				}
				contentMethods := []string{"session/started", "turn/started", "item/started", "item/delta", "item/completed", "items/saved", "turn/finished"}
				for _, method := range methods {
					if !slices.Contains(contentMethods, method) {
						t.Fatalf("content subscription received unexpected %s in %v", method, methods)
					}
				}
				if !containsAll(methods, "turn/started", "item/started", "item/delta", "item/completed", "items/saved", "turn/finished") {
					t.Fatalf("notification methods = %v", methods)
				}
			},
		},
		{
			name: "subscribe_result_precedes_notifications",
			run: func(t *testing.T, h *serverHarness, client *testClient, conversation defs.Conversation) {
				submitText(t, client, conversation, "长回答")
				release := make(chan struct{})
				h.model.receive(t).reply <- serverModelResponse{
					chunks:  strings.Split(strings.Repeat("字", 3000), ""),
					release: release,
				}
				client.waitItems(t, conversation, conversation.RootSessionID, 1)
				// 在事件持续产生时登记订阅，检查响应先于首条通知到达。
				// 请求循环先入队响应再启动转发，writer 按入队顺序发送。
				close(release)
				response := client.callResult(t, "session/subscribe", sessionTarget(conversation, conversation.RootSessionID), nil)
				// 再提交一轮，读取订阅登记之后的通知。
				submitText(t, client, conversation, "再来")
				h.model.receive(t).reply <- serverModelResponse{chunks: []string{"好"}}
				first := client.collectNotifications(t, func(clientMessage) bool { return true })[0]
				if first.sequence < response.sequence {
					t.Fatalf("notification %s arrived before the subscribe result", first.Method)
				}
			},
		},
		{
			name: "snapshot_mid_generation",
			run: func(t *testing.T, h *serverHarness, client *testClient, conversation defs.Conversation) {
				client.callResult(t, "session/subscribe", sessionTarget(conversation, conversation.RootSessionID), nil)
				submitText(t, client, conversation, "天气怎么样")
				release := make(chan struct{})
				h.model.receive(t).reply <- serverModelResponse{
					chunks:  []string{"今天", "晴"},
					release: release,
				}
				client.collectNotifications(t, isMethod("item/delta"))

				late := h.dial(t)
				late.initialize(t)
				var snapshot subscribeSessionView
				late.callResult(t, "session/subscribe", sessionTarget(conversation, conversation.RootSessionID), &snapshot)
				if len(snapshot.StreamingItems) != 1 || itemText(snapshot.StreamingItems[0].Item) != "今天" {
					t.Fatalf("streaming items = %+v", snapshot.StreamingItems)
				}
				text := itemText(snapshot.StreamingItems[0].Item)
				close(release)
				var savedText string
				for _, message := range late.collectNotifications(t, isMethod("turn/finished")) {
					view := decodeNotification(t, message)
					switch message.Method {
					case "item/delta":
						if view.ItemIndex == snapshot.StreamingItems[0].ItemIndex {
							text += view.Delta.Text
						}
					case "items/saved":
						for _, logged := range view.Items {
							if logged.Item.Kind == defs.ItemMessage {
								savedText = itemText(logged.Item)
							}
						}
					}
				}
				if text != "今天晴" || savedText != "今天晴" {
					t.Fatalf("stream text = %q, saved = %q", text, savedText)
				}
			},
		},
		{
			name: "resume_after_disconnect",
			run: func(t *testing.T, h *serverHarness, client *testClient, conversation defs.Conversation) {
				client.callResult(t, "session/subscribe", sessionTarget(conversation, conversation.RootSessionID), nil)
				submitText(t, client, conversation, "第一轮")
				h.model.receive(t).reply <- serverModelResponse{chunks: []string{"一"}}
				client.collectNotifications(t, isMethod("turn/finished"))
				_ = client.conn.CloseNow()

				resumed := h.dial(t)
				resumed.initialize(t)
				var snapshot subscribeSessionView
				resumed.callResult(t, "session/subscribe", sessionTarget(conversation, conversation.RootSessionID), &snapshot)
				history := resumed.waitItems(t, conversation, conversation.RootSessionID, 2)
				if snapshot.LastSavedOrdinal != 2 || history[len(history)-1].Ordinal != snapshot.LastSavedOrdinal {
					t.Fatalf("resume point = %d, history tail = %d", snapshot.LastSavedOrdinal, history[len(history)-1].Ordinal)
				}
				submitText(t, resumed, conversation, "第二轮")
				h.model.receive(t).reply <- serverModelResponse{chunks: []string{"二"}}
				var ordinals []storage.Ordinal
				for _, message := range resumed.collectNotifications(t, isMethod("turn/finished")) {
					for _, logged := range decodeNotification(t, message).Items {
						ordinals = append(ordinals, logged.Ordinal)
					}
				}
				if len(ordinals) != 2 || ordinals[0] != 3 || ordinals[1] != 4 {
					t.Fatalf("live ordinals = %v, want [3 4]", ordinals)
				}
			},
		},
		{
			name: "duplicate_subscription_rejected",
			run: func(t *testing.T, h *serverHarness, client *testClient, conversation defs.Conversation) {
				target := sessionTarget(conversation, conversation.RootSessionID)
				client.callResult(t, "session/subscribe", target, nil)
				requireErrorCode(t, client.call(t, "session/subscribe", target), -32600, "already subscribed")
			},
		},
		{
			name: "unsubscribe_then_resubscribe",
			run: func(t *testing.T, h *serverHarness, client *testClient, conversation defs.Conversation) {
				target := sessionTarget(conversation, conversation.RootSessionID)
				client.callResult(t, "session/subscribe", target, nil)
				unsubscribed := client.callResult(t, "session/unsubscribe", target, nil)
				closed := client.collectNotifications(t, isMethod("subscription/closed"))
				last := closed[len(closed)-1]
				if view := decodeNotification(t, last); !strings.Contains(view.Reason, "canceled") {
					t.Fatalf("close reason = %q", view.Reason)
				}
				// unsubscribe 等旧转发任务将通知入队后才返回；writer 按入队顺序发送，subscription/closed 先于退订响应到达。
				if last.sequence > unsubscribed.sequence {
					t.Fatalf("subscription/closed (#%d) arrived after the unsubscribe result (#%d)", last.sequence, unsubscribed.sequence)
				}
				// 重新订阅后保留新登记，同一目标再次订阅仍被拒绝。
				client.callResult(t, "session/subscribe", target, nil)
				requireErrorCode(t, client.call(t, "session/subscribe", target), -32600, "already subscribed")
				client.callResult(t, "session/unsubscribe", target, nil)
				client.callResult(t, "session/unsubscribe", target, nil)
			},
		},
		{
			name: "foreign_session_rejected",
			run: func(t *testing.T, h *serverHarness, client *testClient, conversation defs.Conversation) {
				other := client.createConversation(t)
				requireErrorCode(t, client.call(t, "session/subscribe", sessionTarget(conversation, other.RootSessionID)), -32003, "not found")
				client.callResult(t, "session/subscribe", sessionTarget(conversation, conversation.RootSessionID), nil)
			},
		},
		{
			// 对话订阅只推送完整 Session 运行信息和已保存的标题变化，启动、停止与 Turn 事件由内容订阅发送。
			name: "conversation_status_updated",
			run: func(t *testing.T, h *serverHarness, client *testClient, conversation defs.Conversation) {
				var subscribed struct {
					Sessions []map[string]json.RawMessage `json:"sessions"`
				}
				client.callResult(t, "conversation/subscribe", map[string]any{"conversation_id": conversation.ID}, &subscribed)
				if len(subscribed.Sessions) != 1 || !hasKeys(subscribed.Sessions[0], "session", "state", "context", "usage") {
					t.Fatalf("sessions = %v", subscribed.Sessions)
				}
				submitText(t, client, conversation, "你好")
				h.model.receive(t).reply <- serverModelResponse{chunks: []string{"好"}}
				notifications := client.collectNotifications(t, func(message clientMessage) bool {
					info := decodeNotification(t, message).Info
					return info["state"] == "idle" && info["last_turn_status"] == "completed"
				})
				titleCount := 0
				for _, message := range notifications {
					view := decodeNotification(t, message)
					if message.Method == "conversation/titleUpdated" {
						titleCount++
						if view.ConversationID != conversation.ID || view.SessionID != "" || view.Subscription != "conversation" || view.Title != "你好" {
							t.Fatalf("title notification = %s", message.Params)
						}
						continue
					}
					context, _ := view.Info["context"].(map[string]any)
					if message.Method != "session/statusUpdated" || view.Subscription != "conversation" || view.SessionID != conversation.RootSessionID {
						t.Fatalf("conversation subscription received %s %s", message.Method, message.Params)
					}
					if view.Info["state"] == nil || view.Info["usage"] == nil || context["source"] == nil {
						t.Fatalf("session/statusUpdated info = %v", view.Info)
					}
				}
				if titleCount != 1 {
					t.Fatalf("title notifications = %d, want 1", titleCount)
				}
				client.callResult(t, "conversation/unsubscribe", map[string]any{"conversation_id": conversation.ID}, nil)
			},
		},
	}
	// 2. 创建订阅并运行 Turn，检查订阅返回的内容、通知种类与目标、断线续传、退订和响应到达次序。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := startTestServer(t)
			client := h.dial(t)
			client.initialize(t)
			tt.run(t, h, client, client.createConversation(t))
		})
	}
}

// ── 订阅测试夹具 ──

// subscribeSessionView 是 session/subscribe 的结果。
type subscribeSessionView struct {
	LastSavedOrdinal storage.Ordinal `json:"last_saved_ordinal"`
	StreamingItems   []struct {
		ItemIndex int        `json:"item_index"`
		Item      *defs.Item `json:"item"`
	} `json:"streaming_items"`
}

// notificationView 汇集各通知可能带的字段，测试按通知名读取需要的部分。
type notificationView struct {
	Title          string                `json:"title"`
	ConversationID defs.ConversationID   `json:"conversation_id"`
	SessionID      defs.SessionID        `json:"session_id"`
	Subscription   string                `json:"subscription"`
	TurnID         defs.TurnID           `json:"turn_id"`
	Status         defs.TurnStatus       `json:"status"`
	ItemIndex      int                   `json:"item_index"`
	Item           *defs.Item            `json:"item"`
	Delta          *agent.AgentItemDelta `json:"delta"`
	Items          []loggedItemView      `json:"items"`
	Reason         string                `json:"reason"`
	Info           map[string]any        `json:"info"`
	HookRun        *defs.HookRun         `json:"hook_run"`
}

// sessionTarget 创建定位对话内 Session 的请求字段。
func sessionTarget(conversation defs.Conversation, sessionID defs.SessionID) map[string]any {
	// 1. 填入对话 ID 和 Session ID。
	return map[string]any{"conversation_id": conversation.ID, "session_id": sessionID}
}

// submitText 通过协议向对话提交用户文本。
func submitText(t *testing.T, client *testClient, conversation defs.Conversation, text string) {
	t.Helper()
	// 1. 填入对话 ID 和文本 content，检查提交成功。
	client.callResult(t, "conversation/submitInput", map[string]any{
		"conversation_id": conversation.ID,
		"content":         textContent(text),
	}, nil)
}

// completeFirstRound 完成新对话首轮并等待消息数保存。
func completeFirstRound(t *testing.T, h *serverHarness, client *testClient, conversation defs.Conversation) {
	t.Helper()
	// 1. 提交用户文本并让模型回复，新对话随首条输入进入共享库和对话列表。
	submitText(t, client, conversation, "保存")
	h.model.receive(t).reply <- serverModelResponse{chunks: []string{"好"}}
	// 2. 在 5 秒内轮询对话，消息数为 2 后返回。
	deadline := time.Now().Add(5 * time.Second)
	for {
		var read struct {
			Conversation defs.Conversation `json:"conversation"`
		}
		client.callResult(t, "conversation/read", map[string]any{"conversation_id": conversation.ID}, &read)
		if read.Conversation.MessageCount == 2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("conversation = %+v, want message count 2 after the first round", read.Conversation)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// turnsPageView 是 session/turns/list 和 session/turns/get 的结果。
type turnsPageView struct {
	Turns      []defs.Turn                   `json:"turns"`
	NextCursor dao.RecordCursor[defs.TurnID] `json:"next_cursor"`
}

// getTurnIDs 按原顺序提取 Turn ID。
func getTurnIDs(turns []defs.Turn) []defs.TurnID {
	// 1. 逐项复制 TurnID 后返回切片。
	ids := make([]defs.TurnID, len(turns))
	for index, turn := range turns {
		ids[index] = turn.ID
	}
	return ids
}

// compactWhenIdle 等待根 Session 空闲后通过协议启动压缩。
func compactWhenIdle(t *testing.T, client *testClient, conversation defs.Conversation) defs.TurnID {
	t.Helper()
	// 1. 最多等待 5 秒，遇到 -32001 重试；成功解析非空 TurnID 并返回，其余错误结束测试。
	deadline := time.Now().Add(5 * time.Second)
	for {
		response := client.call(t, "conversation/compact", map[string]any{"conversation_id": conversation.ID})
		if response.Error == nil {
			var result struct {
				TurnID defs.TurnID `json:"turn_id"`
			}
			if err := json.Unmarshal(response.Result, &result); err != nil || result.TurnID == "" {
				t.Fatalf("conversation/compact result = %s, %v, want a turn_id", response.Result, err)
			}
			return result.TurnID
		}
		if response.Error.Code != -32001 || time.Now().After(deadline) {
			t.Fatalf("conversation/compact error = %+v", response.Error)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// textContent 创建单条普通文本的 content 参数。
func textContent(text string) []map[string]any {
	// 1. 返回 text 类型及其正文。
	return []map[string]any{{"kind": "text", "text": map[string]any{"text": text}}}
}

// collectNotifications 收集通知直到指定条件成立。
func (c *testClient) collectNotifications(t *testing.T, match func(clientMessage) bool) []clientMessage {
	t.Helper()
	// 1. 最多读取 5 秒，保留全部通知并在匹配时返回，提前断开或超时则结束测试。
	var collected []clientMessage
	timeout := time.After(5 * time.Second)
	for {
		select {
		case message, open := <-c.notifications:
			if !open {
				t.Fatalf("connection closed after %d notifications", len(collected))
			}
			collected = append(collected, message)
			if match(message) {
				return collected
			}
		case <-timeout:
			t.Fatalf("notification did not arrive after %d notifications", len(collected))
		}
	}
}

// isMethod 创建匹配指定通知方法的条件。
func isMethod(method string) func(clientMessage) bool {
	// 1. 返回比较 Method 的匹配函数。
	return func(message clientMessage) bool { return message.Method == method }
}

// decodeNotification 解析测试需要的通知字段。
func decodeNotification(t *testing.T, message clientMessage) notificationView {
	t.Helper()
	// 1. 解码通知参数，失败则结束测试并返回字段视图。
	var view notificationView
	if err := json.Unmarshal(message.Params, &view); err != nil {
		t.Fatal(err)
	}
	return view
}

// hasKeys JSON 对象是否包含全部指定字段？
func hasKeys(object map[string]json.RawMessage, keys ...string) bool {
	// 1. 逐项查找字段，缺少任一项返回 false。
	for _, key := range keys {
		if _, found := object[key]; !found {
			return false
		}
	}
	return true
}

// containsAll 字符串列表是否包含全部目标值？
func containsAll(values []string, wants ...string) bool {
	// 1. 逐项查找目标值，缺少任一项返回 false。
	for _, want := range wants {
		if !slices.Contains(values, want) {
			return false
		}
	}
	return true
}

// itemsPageView 是 session/items/list 的结果；Items 保留每条 Item 的原始 JSON，用来核对一页的字节数。
type itemsPageView struct {
	Items      []json.RawMessage `json:"items"`
	NextCursor storage.Ordinal   `json:"next_cursor"`
}

// listItemsPage 读取根 Session 的一页日志并提取序号。
func (c *testClient) listItemsPage(t *testing.T, conversation defs.Conversation, cursor storage.Ordinal) (itemsPageView, []storage.Ordinal) {
	t.Helper()
	// 1. 从指定游标之后读取最多 100 条日志。
	var page itemsPageView
	c.callResult(t, "session/items/list", map[string]any{
		"conversation_id": conversation.ID,
		"session_id":      conversation.RootSessionID,
		"cursor":          cursor,
		"limit":           100,
	}, &page)
	// 2. 解析每条 Item 的日志序号，返回原始页面与序号列表。
	ordinals := make([]storage.Ordinal, len(page.Items))
	for index, raw := range page.Items {
		var logged loggedItemView
		if err := json.Unmarshal(raw, &logged); err != nil {
			t.Fatal(err)
		}
		ordinals[index] = logged.Ordinal
	}
	return page, ordinals
}

// getRawBytes 累计 JSON 片段的总字节数。
func getRawBytes(items []json.RawMessage) int {
	// 1. 累加各片段长度后返回。
	total := 0
	for _, raw := range items {
		total += len(raw)
	}
	return total
}

// itemText 返回测试消息首段正文。
func itemText(item *defs.Item) string {
	// 1. 读取第一个文本内容块。
	return item.Message.Content[0].Text.Text
}

// TestOpenVaalaHooks 检查 Hook 目录和运行开始、结束通知。
func TestOpenVaalaHooks(t *testing.T) {
	// 1. 准备 Prompt 与工具 Hook 定义及运行通知场景。
	newMatcher := func(pattern string) *hook.HookMatcher {
		matcher, err := hook.NewHookMatcher(pattern)
		if err != nil {
			t.Fatal(err)
		}
		return matcher
	}
	promptHook := &hook.HookDefinition{
		Key:          "go.yaml:UserPromptSubmit:0",
		Event:        defs.HookEventUserPromptSubmit,
		Matcher:      newMatcher(""),
		Command:      `printf '{"systemMessage":"已检查"}'`,
		Timeout:      10 * time.Second,
		SourcePath:   "/hooks/go.yaml",
		DisplayOrder: 1,
	}
	toolHook := &hook.HookDefinition{
		Key:          "go.yaml:PreToolUse:1",
		Event:        defs.HookEventPreToolUse,
		Matcher:      newMatcher("Bash|Read"),
		ScriptPath:   "/hooks/guard.sh",
		Args:         []string{"--strict"},
		Timeout:      90 * time.Second,
		SourcePath:   "/hooks/go.yaml",
		DisplayOrder: 2,
	}
	toolRunHook := &hook.HookDefinition{
		Key:          "go.yaml:PreToolUse:0",
		Event:        defs.HookEventPreToolUse,
		Matcher:      newMatcher("Bash"),
		Command:      `printf '{"systemMessage":"工具已检查"}'`,
		Timeout:      10 * time.Second,
		SourcePath:   "/hooks/go.yaml",
		DisplayOrder: 1,
	}
	tests := []struct {
		name  string
		hooks []*hook.HookDefinition
		run   func(t *testing.T, h *serverHarness, client *testClient, conversation defs.Conversation)
	}{
		{
			name:  "list_returns_configured_hooks",
			hooks: []*hook.HookDefinition{promptHook, toolHook},
			run: func(t *testing.T, h *serverHarness, client *testClient, conversation defs.Conversation) {
				response := client.callResult(t, "hook/list", nil, nil)
				var raw struct {
					Hooks []map[string]any `json:"hooks"`
				}
				if err := json.Unmarshal(response.Result, &raw); err != nil {
					t.Fatal(err)
				}
				wantKeys := []string{"key", "event", "handler", "timeout_seconds", "source_path", "display_order", "current_hash"}
				if len(raw.Hooks) != 2 {
					t.Fatalf("hook/list = %s, want 2 hooks", response.Result)
				}
				for index, fields := range raw.Hooks {
					for _, key := range wantKeys {
						if _, found := fields[key]; !found {
							t.Fatalf("hook %d is missing %q: %v", index, key, fields)
						}
					}
				}
				if _, found := raw.Hooks[0]["matcher"]; found {
					t.Fatalf("hook without matcher carries matcher: %v", raw.Hooks[0])
				}
				var result struct {
					Hooks []hook.HookInfo `json:"hooks"`
				}
				if err := json.Unmarshal(response.Result, &result); err != nil {
					t.Fatal(err)
				}
				if result.Hooks[0].CurrentHash == "" || result.Hooks[0].CurrentHash == result.Hooks[1].CurrentHash {
					t.Fatalf("current hashes = %q and %q, want two different non-empty hashes", result.Hooks[0].CurrentHash, result.Hooks[1].CurrentHash)
				}
				result.Hooks[0].CurrentHash, result.Hooks[1].CurrentHash = "", ""
				want := []hook.HookInfo{
					{
						Key:            "go.yaml:UserPromptSubmit:0",
						Event:          defs.HookEventUserPromptSubmit,
						Handler:        `printf '{"systemMessage":"已检查"}'`,
						TimeoutSeconds: 10,
						SourcePath:     "/hooks/go.yaml",
						DisplayOrder:   1,
					},
					{
						Key:            "go.yaml:PreToolUse:1",
						Event:          defs.HookEventPreToolUse,
						Matcher:        "Bash|Read",
						Handler:        "/hooks/guard.sh --strict",
						TimeoutSeconds: 90,
						SourcePath:     "/hooks/go.yaml",
						DisplayOrder:   2,
					},
				}
				if !reflect.DeepEqual(result.Hooks, want) {
					t.Fatalf("hooks = %+v, want %+v", result.Hooks, want)
				}
			},
		},
		{
			name: "list_without_hooks_is_empty_array",
			run: func(t *testing.T, h *serverHarness, client *testClient, conversation defs.Conversation) {
				response := client.callResult(t, "hook/list", nil, nil)
				if string(response.Result) != `{"hooks":[]}` {
					t.Fatalf("hook/list = %s, want {\"hooks\":[]}", response.Result)
				}
			},
		},
		{
			// UserPromptSubmit 的运行来自 Turn（SessionHookRunEvent）。
			name:  "prompt_run_notifies_started_then_completed",
			hooks: []*hook.HookDefinition{promptHook},
			run: func(t *testing.T, h *serverHarness, client *testClient, conversation defs.Conversation) {
				client.callResult(t, "session/subscribe", sessionTarget(conversation, conversation.RootSessionID), nil)
				submitText(t, client, conversation, "你好")
				h.model.receive(t).reply <- serverModelResponse{chunks: []string{"好"}}
				started, completed := collectHookRuns(t, client, conversation)
				requireHookRunPair(t, started, completed, promptHook, "已检查")
				if completed.CallID != "" {
					t.Fatalf("completed run call_id = %q, want none for a prompt hook", completed.CallID)
				}
			},
		},
		{
			// PreToolUse 的运行来自 Agent（SessionAgentEvent），带触发它的工具调用。
			name:  "tool_run_notifies_started_then_completed",
			hooks: []*hook.HookDefinition{toolRunHook},
			run: func(t *testing.T, h *serverHarness, client *testClient, conversation defs.Conversation) {
				client.callResult(t, "session/subscribe", sessionTarget(conversation, conversation.RootSessionID), nil)
				submitText(t, client, conversation, "执行工具")
				h.model.receive(t).reply <- serverModelResponse{calls: []llmkit.ToolCall{{
					ID:        "call-hook",
					Name:      "bash",
					Arguments: `{"command":"echo hi"}`,
				}}}
				h.model.receive(t).reply <- serverModelResponse{chunks: []string{"好"}}
				started, completed := collectHookRuns(t, client, conversation)
				requireHookRunPair(t, started, completed, toolRunHook, "工具已检查")
				if completed.CallID != "call-hook" {
					t.Fatalf("completed run call_id = %q, want call-hook", completed.CallID)
				}
			},
		},
	}
	// 2. 配置各场景 Hook，检查目录字段及同一运行 ID 的开始、完成通知和工具 CallID。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := startTestServer(t)
			definitions := newTestRuntimeOpt(h.store, h.model, h.defaultDirectory).Definitions
			definitions.Hooks = tt.hooks
			if _, err := h.runtime.SetDefinitions(t.Context(), definitions); err != nil {
				t.Fatal(err)
			}
			client := h.dial(t)
			client.initialize(t)
			tt.run(t, h, client, client.createConversation(t))
		})
	}
}

// collectHookRuns 读取本轮唯一一组 Hook 开始和完成通知。
func collectHookRuns(t *testing.T, client *testClient, conversation defs.Conversation) (started, completed *defs.HookRun) {
	t.Helper()
	// 1. 收集到 turn/finished，检查 Hook 通知属于根 Session 和本轮 Turn。
	var turnID defs.TurnID
	var hookMethods []string
	var runs []*defs.HookRun
	for _, message := range client.collectNotifications(t, isMethod("turn/finished")) {
		view := decodeNotification(t, message)
		switch message.Method {
		case "turn/started":
			turnID = view.TurnID
		case "hook/started", "hook/completed":
			if view.ConversationID != conversation.ID || view.SessionID != conversation.RootSessionID || view.Subscription != "session" || view.TurnID != turnID {
				t.Fatalf("%s target = %s/%s/%s turn %s, want session %s/%s turn %s", message.Method, view.ConversationID, view.SessionID, view.Subscription, view.TurnID,
					conversation.ID, conversation.RootSessionID, turnID)
			}
			hookMethods = append(hookMethods, message.Method)
			runs = append(runs, view.HookRun)
		}
	}
	// 2. 检查顺序恰好是 hook/started、hook/completed，返回两份记录。
	if !reflect.DeepEqual(hookMethods, []string{"hook/started", "hook/completed"}) {
		t.Fatalf("hook notifications = %v, want hook/started then hook/completed", hookMethods)
	}
	return runs[0], runs[1]
}

// requireHookRunPair 检查 Hook 两次通知来自同一次运行。
func requireHookRunPair(t *testing.T, started, completed *defs.HookRun, definition *hook.HookDefinition, systemMessage string) {
	t.Helper()
	// 1. 检查 ID 相同，状态由 running 变为 completed。
	if started.ID == "" || started.ID != completed.ID || started.Status != defs.HookRunRunning || completed.Status != defs.HookRunCompleted {
		t.Fatalf("runs = %+v and %+v, want one run going from running to completed", started, completed)
	}
	// 2. 核对事件、Executor、来源、命令、顺序、输出和起止时间。
	if completed.Event != definition.Event || completed.ExecutorID != defs.LocalExecutorID || completed.SourcePath != definition.SourcePath ||
		completed.Handler != definition.Command || completed.DisplayOrder != definition.DisplayOrder || completed.SystemMessage != systemMessage ||
		completed.StartedAt == 0 || completed.CompletedAt < completed.StartedAt {
		t.Fatalf("completed run = %+v, want a completed %s run of %q with system message %q", completed, definition.Event, definition.Command, systemMessage)
	}
}

// TestOpenVaalaSetClientMCPServers 检查客户端 MCP 登记、连接 ID 和连接失败状态。
func TestOpenVaalaSetClientMCPServers(t *testing.T) {
	// 1. 列出客户端 MCP 登记、连接 ID 和连接失败状态的测试场景。
	tests := []struct {
		name string
		// isRecordOnly Runtime 是否只记录请求、不要求对话存在？否则请求也交给真实 Runtime。
		isRecordOnly bool
		run          func(t *testing.T, h *serverHarness, client *testClient, rt *clientMCPRuntime)
	}{
		{
			name:         "registers_servers_under_the_connection_client_id",
			isRecordOnly: true,
			run: func(t *testing.T, h *serverHarness, client *testClient, rt *clientMCPRuntime) {
				client.initialize(t)
				response := client.call(t, "conversation/setClientMCPServers", map[string]any{"conversation_id": "conv-1", "server_ids": []string{"a", "b"}})
				if response.Error != nil || string(response.Result) != "{}" {
					t.Fatalf("response = %s (error %+v), want {}", response.Result, response.Error)
				}
				request := rt.receive(t)
				var serverIDs []string
				for _, server := range request.Servers {
					if server.Transport == nil {
						t.Fatalf("server %q has no transport", server.ID)
					}
					serverIDs = append(serverIDs, server.ID)
				}
				if request.ConversationID != "conv-1" || request.ClientID == "" || !slices.Equal(serverIDs, []string{"a", "b"}) {
					t.Fatalf("request = %+v (servers %v), want conv-1 with a client id and servers [a b]", request, serverIDs)
				}
			},
		},
		{
			name:         "empty_or_missing_server_ids_remove_all",
			isRecordOnly: true,
			run: func(t *testing.T, h *serverHarness, client *testClient, rt *clientMCPRuntime) {
				client.initialize(t)
				for _, params := range []map[string]any{
					{"conversation_id": "conv-1", "server_ids": []string{}},
					{"conversation_id": "conv-1"},
				} {
					client.callResult(t, "conversation/setClientMCPServers", params, nil)
					if request := rt.receive(t); len(request.Servers) != 0 {
						t.Fatalf("params %v registered servers %+v, want none", params, request.Servers)
					}
				}
			},
		},
		{
			name:         "client_id_is_per_connection",
			isRecordOnly: true,
			run: func(t *testing.T, h *serverHarness, client *testClient, rt *clientMCPRuntime) {
				other := h.dial(t)
				client.initialize(t)
				other.initialize(t)
				var clientIDs []string
				for _, connection := range []*testClient{client, client, other} {
					connection.callResult(t, "conversation/setClientMCPServers", map[string]any{"conversation_id": "conv-1", "server_ids": []string{"a"}}, nil)
					clientIDs = append(clientIDs, rt.receive(t).ClientID)
				}
				if clientIDs[0] == "" || clientIDs[0] != clientIDs[1] || clientIDs[0] == clientIDs[2] {
					t.Fatalf("client ids = %v, want the same id on one connection and a different id on the other", clientIDs)
				}
			},
		},
		{
			name:         "invalid_params",
			isRecordOnly: true,
			run: func(t *testing.T, h *serverHarness, client *testClient, rt *clientMCPRuntime) {
				client.initialize(t)
				requireErrorCode(t, client.call(t, "conversation/setClientMCPServers", map[string]any{"conversation_id": "conv-1", "server_ids": "a"}), -32602, "")
				select {
				case request := <-rt.requests:
					t.Fatalf("Runtime received %+v for invalid params", request)
				default:
				}
			},
		},
		{
			name: "conversation_not_open_is_a_runtime_error",
			run: func(t *testing.T, h *serverHarness, client *testClient, rt *clientMCPRuntime) {
				client.initialize(t)
				response := client.call(t, "conversation/setClientMCPServers", map[string]any{"conversation_id": uuid.NewString(), "server_ids": []string{"fake"}})
				requireErrorCode(t, response, -32000, "conversation is not open")
			},
		},
		{
			name: "real_runtime_asks_the_client_to_connect",
			run: func(t *testing.T, h *serverHarness, client *testClient, rt *clientMCPRuntime) {
				client.initialize(t)
				conv := client.createConversation(t)
				client.callResult(t, "conversation/setClientMCPServers", map[string]any{"conversation_id": conv.ID, "server_ids": []string{"fake"}}, nil)
				connect := client.waitClientMCPConnect(t, "fake")
				// 客户端连不上：clientMCP/closed 带原因，连接状态是 failed 并记下原因。
				client.sendRaw(t, `{"method":"clientMCP/closed","params":{"stream_id":"`+connect.StreamID+`","error":"cannot start the server"}}`)
				server := client.waitClientMCPServer(t, conv.ID, func(server conversation.MCPServerInfo) bool { return server.State == mcpclient.ConnectionStateFailed })
				if server.ServerID != "fake" || !strings.Contains(server.Error, "cannot start the server") {
					t.Fatalf("client MCP server = %+v, want fake failed with the client's reason", server)
				}
				client.callResult(t, "conversation/setClientMCPServers", map[string]any{"conversation_id": conv.ID, "server_ids": []string{}}, nil)
				var servers listedMCPServers
				client.callResult(t, "mcpServer/list", map[string]any{"conversation_id": conv.ID}, &servers)
				for _, server := range servers.Servers {
					if server.ExecutorID == defs.ClientExecutorID {
						t.Fatalf("mcpServer/list = %+v, want no client entries after clearing the registration", servers.Servers)
					}
				}
			},
		},
	}
	// 2. 登记客户端 server，检查 Runtime 请求、连接级 ClientID、空列表移除请求及真实连接失败状态。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &clientMCPRuntime{
				isRecordOnly: tt.isRecordOnly,
				requests:     make(chan *runtime.SetClientMCPServersRequest, 16),
			}
			h := startTestServer(t, func(opt *runtimeserver.RuntimeServerOpt) {
				rt.Runtime = opt.Runtime
				opt.Runtime = rt
			})
			tt.run(t, h, h.dial(t), rt)
		})
	}
}

// clientMCPRuntime 是测试用的 Runtime：SetClientMCPServers 把收到的请求按到达顺序记进 requests，其余方法直接使用真实 Runtime。
type clientMCPRuntime struct {
	runtime.Runtime
	// isRecordOnly 是否只记录请求、不要求对话存在？为 false 时请求也交给真实 Runtime。
	isRecordOnly bool
	// removeErr 是只记录时，移除登记（Servers 为空）的请求返回的错误。
	removeErr error
	requests  chan *runtime.SetClientMCPServersRequest
}

// SetClientMCPServers 记录客户端 MCP 请求，按测试配置决定是否交给真实 Runtime。
func (r *clientMCPRuntime) SetClientMCPServers(ctx context.Context, request *runtime.SetClientMCPServersRequest) error {
	// 1. 将请求写入记录通道。
	r.requests <- request
	// 2. 只记录模式下返回预设移除错误或成功，其余交给真实 Runtime。
	if r.isRecordOnly {
		if len(request.Servers) == 0 {
			return r.removeErr
		}
		return nil
	}
	return r.Runtime.SetClientMCPServers(ctx, request)
}

// receive 按到达顺序读取下一条客户端 MCP 登记请求。
func (r *clientMCPRuntime) receive(t *testing.T) *runtime.SetClientMCPServersRequest {
	t.Helper()
	// 1. 最多等待 5 秒，收到请求后返回，超时则结束测试。
	select {
	case request := <-r.requests:
		return request
	case <-time.After(5 * time.Second):
		t.Fatal("SetClientMCPServers did not reach the Runtime")
		return nil
	}
}

// clientMCPConnectParams 是 serve 发来的 clientMCP/connect 的参数。
type clientMCPConnectParams struct {
	StreamID string `json:"stream_id"`
	ServerID string `json:"server_id"`
}

// waitClientMCPConnect 等到客户端 MCP 连接通知并检查 server ID。
func (c *testClient) waitClientMCPConnect(t *testing.T, serverID string) clientMCPConnectParams {
	t.Helper()
	// 1. 读取到 clientMCP/connect，解析并检查 stream_id 非空且 server_id 匹配。
	notifications := c.collectNotifications(t, isMethod("clientMCP/connect"))
	var params clientMCPConnectParams
	if err := json.Unmarshal(notifications[len(notifications)-1].Params, &params); err != nil {
		t.Fatal(err)
	}
	if params.StreamID == "" || params.ServerID != serverID {
		t.Fatalf("clientMCP/connect params = %+v, want a stream_id for server %q", params, serverID)
	}
	return params
}

// listedMCPServers 是 mcpServer/list 的结果。
type listedMCPServers struct {
	Servers []conversation.MCPServerInfo `json:"servers"`
}

// waitClientMCPServer 等待客户端 MCP 连接条目满足指定条件。
func (c *testClient) waitClientMCPServer(t *testing.T, conversationID defs.ConversationID, match func(conversation.MCPServerInfo) bool) conversation.MCPServerInfo {
	t.Helper()
	// 1. 在 5 秒内轮询 mcpServer/list，筛选 client Executor 的匹配条目并返回，超时则结束测试。
	deadline := time.Now().Add(5 * time.Second)
	for {
		var servers listedMCPServers
		c.callResult(t, "mcpServer/list", map[string]any{"conversation_id": conversationID}, &servers)
		for _, server := range servers.Servers {
			if server.ExecutorID == defs.ClientExecutorID && match(server) {
				return server
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("mcpServer/list = %+v, want a matching client entry", servers.Servers)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestOpenVaalaRunningConversations 检查真实连接上的运行集合订阅和退订顺序。
func TestOpenVaalaRunningConversations(t *testing.T) {
	// 1. 分别检查完整集合推送、主动退订、重复订阅和无订阅连接。
	for _, scenario := range []string{"updates", "unsubscribe", "duplicate", "invalid_params", "not_subscribed"} {
		t.Run(scenario, func(t *testing.T) {
			h := startTestServer(t)
			client := h.dial(t)
			client.initialize(t)
			actor := h.dial(t)
			actor.initialize(t)
			conv := actor.createConversation(t)
			if scenario == "invalid_params" {
				requireErrorCode(t, client.call(t, "conversation/running/subscribe", []string{"wrong"}), -32602, "")
				return
			}
			var response clientMessage
			if scenario != "not_subscribed" {
				response = client.callResult(t, "conversation/running/subscribe", map[string]any{}, nil)
				if string(response.Result) != `{"conversation_ids":[]}` {
					t.Fatalf("initial = %s", response.Result)
				}
			}
			if scenario == "duplicate" {
				requireErrorCode(t, client.call(t, "conversation/running/subscribe", map[string]any{}), -32600, "already subscribed")
				return
			}
			// 2. 另一连接启动对话，本连接无需打开或订阅历史即可看到运行集合。
			submitText(t, actor, conv, "工作")
			request := h.model.receive(t)
			if scenario == "not_subscribed" {
				request.reply <- serverModelResponse{chunks: []string{"结束"}}
				select {
				case message := <-client.notifications:
					t.Fatalf("unsolicited notification = %+v", message)
				case <-time.After(100 * time.Millisecond):
				}
				return
			}
			messages := client.collectNotifications(t, func(message clientMessage) bool {
				var running runtime.RunningConversations
				return message.Method == "conversation/runningChanged" && json.Unmarshal(message.Params, &running) == nil && slices.Equal(running.ConversationIDs, []defs.ConversationID{conv.ID})
			})
			started := messages[len(messages)-1]
			var running runtime.RunningConversations
			if err := json.Unmarshal(started.Params, &running); err != nil || !slices.Equal(running.ConversationIDs, []defs.ConversationID{conv.ID}) || started.sequence <= response.sequence {
				t.Fatalf("running = %s (#%d), response #%d, %v", started.Params, started.sequence, response.sequence, err)
			}
			if scenario == "unsubscribe" {
				unsubscribed := client.callResult(t, "conversation/running/unsubscribe", map[string]any{}, nil)
				request.reply <- serverModelResponse{chunks: []string{"结束"}}
				deadline := time.After(100 * time.Millisecond)
				for checking := true; checking; {
					select {
					case message := <-client.notifications:
						if message.sequence > unsubscribed.sequence || message.Method == "conversation/runningClosed" {
							t.Fatalf("notification after unsubscribe = %+v", message)
						}
					case <-deadline:
						checking = false
					}
				}
				client.callResult(t, "conversation/running/subscribe", map[string]any{}, nil)
				return
			}
			// 3. Turn 结束后收到精确空集合，集合不带历史终态。
			request.reply <- serverModelResponse{chunks: []string{"结束"}}
			messages = client.collectNotifications(t, func(message clientMessage) bool {
				return message.Method == "conversation/runningChanged" && string(message.Params) == `{"conversation_ids":[]}`
			})
			for _, message := range messages {
				var params map[string]json.RawMessage
				if err := json.Unmarshal(message.Params, &params); err != nil || len(params) != 1 {
					t.Fatalf("unexpected fields = %s", message.Params)
				}
			}
		})
	}
}

// TestOpenVaalaRunningConversationsClosed 检查 Runtime 停止时关闭通知和再次订阅的错误。
func TestOpenVaalaRunningConversationsClosed(t *testing.T) {
	// 1. 分别在订阅前和订阅后停止真实 Runtime。
	for _, subscribed := range []bool{false, true} {
		t.Run(strconv.FormatBool(subscribed), func(t *testing.T) {
			store, model := openServerStore(t), newServerModel(t)
			rt, err := runtime.NewRuntime(newTestRuntimeOpt(store, model, t.TempDir()))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			go func() {
				defer close(done)
				_ = rt.Run(ctx)
			}()
			t.Cleanup(func() {
				cancel()
				<-done
			})
			h := startTestServer(t, func(opt *runtimeserver.RuntimeServerOpt) { opt.Runtime = rt })
			client := h.dial(t)
			client.initialize(t)
			if subscribed {
				client.callResult(t, "conversation/running/subscribe", map[string]any{}, nil)
			}
			// 2. 停止 Runtime，已有订阅收到只带原因的关闭通知。
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("Runtime did not stop")
			}
			if subscribed {
				messages := client.collectNotifications(t, isMethod("conversation/runningClosed"))
				closed := messages[len(messages)-1]
				var params map[string]string
				if err := json.Unmarshal(closed.Params, &params); err != nil || len(params) != 1 || params["reason"] == "" {
					t.Fatalf("closed params = %s, %v", closed.Params, err)
				}
				select {
				case message := <-client.notifications:
					t.Fatalf("event after closed = %+v", message)
				case <-time.After(50 * time.Millisecond):
				}
			}
			// 3. 旧登记已移除，再次订阅返回 Runtime 错误，而非重复订阅。
			requireErrorCode(t, client.call(t, "conversation/running/subscribe", map[string]any{}), -32000, "runtime has stopped")
		})
	}
}

// TestOpenVaalaPendingInputs 检查独立连接通过订阅与原子撤回操作共享同一待处理列表。
func TestOpenVaalaPendingInputs(t *testing.T) {
	// 1. 分别检查取回完整内容和永久撤回。
	for _, returnContent := range []bool{true, false} {
		t.Run(strconv.FormatBool(returnContent), func(t *testing.T) {
			h := startTestServer(t)
			client := h.dial(t)
			client.initialize(t)
			conversation := client.createConversation(t)
			client.callResult(t, "session/subscribe", sessionTarget(conversation, conversation.RootSessionID), nil)
			submitText(t, client, conversation, "first")
			h.model.receive(t)
			started := client.collectNotifications(t, isMethod("turn/started"))
			turnID := decodeNotification(t, started[len(started)-1]).TurnID
			content := []*defs.Content{
				{Kind: defs.ContentText, Text: &defs.TextContent{Text: "  " + strings.Repeat("字", 205)}},
				{Kind: defs.ContentText, Text: &defs.TextContent{Text: "context-only", IsContext: true}},
				{Kind: defs.ContentImage, Image: &defs.ImageContent{DataURL: "data:image/png;base64,a", Alt: "one"}},
				{Kind: defs.ContentText, Text: &defs.TextContent{Text: " trailing"}},
				{Kind: defs.ContentImage, Image: &defs.ImageContent{DataURL: "data:image/png;base64,b", Alt: "two"}},
			}
			var submitted runtimeprotocol.SubmitInputResult
			client.callResult(t, "conversation/submitInput", &runtimeprotocol.SubmitInputParams{ConversationID: conversation.ID, Content: content}, &submitted)
			client.collectNotifications(t, func(message clientMessage) bool {
				if message.Method != "session/pendingInputsUpdated" {
					return false
				}
				var params runtimeprotocol.SessionPendingInputsUpdatedParams
				if err := json.Unmarshal(message.Params, &params); err != nil {
					t.Fatal(err)
				}
				return len(params.PendingInputs.Inputs) == 1
			})
			// 2. 新连接直接订阅读到轻量列表，不依赖原连接的提交记录。
			late := h.dial(t)
			late.initialize(t)
			var snapshot runtimeprotocol.SubscribeSessionResult
			response := late.callResult(t, "session/subscribe", sessionTarget(conversation, conversation.RootSessionID), &snapshot)
			if len(snapshot.PendingInputs.Inputs) != 1 || snapshot.PendingInputs.SteerTurnID != turnID {
				t.Fatalf("snapshot = %+v", snapshot.PendingInputs)
			}
			pending := snapshot.PendingInputs.Inputs[0]
			if pending.ItemID != submitted.ItemID || pending.ImageCount != 2 || len([]rune(pending.Preview)) != 200 || !strings.HasSuffix(pending.Preview, "…") {
				t.Fatalf("pending = %+v", pending)
			}
			if strings.Contains(string(response.Result), "data:image") || strings.Contains(string(response.Result), "context-only") {
				t.Fatal("snapshot contains attachments")
			}
			// 3. 新连接取回或撤回；原连接收到删除列表，已移除输入再操作返回业务错误。
			var withdrawn runtimeprotocol.WithdrawInputResult
			withdrawResponse := late.callResult(t, "conversation/withdrawInput", &runtimeprotocol.WithdrawInputParams{ConversationID: conversation.ID, ItemID: submitted.ItemID, ReturnContent: returnContent}, &withdrawn)
			if returnContent {
				if !reflect.DeepEqual(withdrawn.Content, content) {
					t.Fatalf("content = %+v", withdrawn.Content)
				}
			} else if string(withdrawResponse.Result) != "{}" {
				t.Fatalf("withdraw response = %s", withdrawResponse.Result)
			}
			deleted := client.collectNotifications(t, isMethod("session/pendingInputsUpdated"))
			var deletion runtimeprotocol.SessionPendingInputsUpdatedParams
			if err := json.Unmarshal(deleted[len(deleted)-1].Params, &deletion); err != nil {
				t.Fatal(err)
			}
			if deletion.PendingInputs.Inputs == nil || len(deletion.PendingInputs.Inputs) != 0 || deletion.PendingInputs.SteerTurnID != turnID {
				t.Fatalf("deletion = %+v", deletion)
			}
			for _, method := range []string{"conversation/steerInput", "conversation/interruptTurnWithInput", "conversation/withdrawInput"} {
				requireErrorCode(t, late.call(t, method, map[string]any{"conversation_id": conversation.ID, "item_id": submitted.ItemID, "turn_id": turnID}), -32000, "input is no longer pending")
				requireErrorCode(t, late.call(t, method, map[string]any{}), -32602, "required")
			}
		})
	}
}

// TestOpenVaalaTerminal 检查终端的打开、输入输出、尺寸、退出，以及连接关闭后进程被结束。
func TestOpenVaalaTerminal(t *testing.T) {
	// 1. 列出终端各场景：每个场景在一条已初始化的连接和 serve 默认目录上运行。
	tests := []struct {
		name string
		run  func(t *testing.T, h *serverHarness, client *testClient)
	}{
		{
			name: "input_reaches_shell_and_close_sends_exited",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				terminal := openTerminal(t, client, map[string]any{"rows": 24, "cols": 80})
				if terminal.ExecutorID != defs.LocalExecutorID || terminal.Directory != h.defaultDirectory {
					t.Fatalf("terminal = %+v, want local executor in %s", terminal, h.defaultDirectory)
				}
				client.writeTerminal(t, terminal.TerminalID, "echo hi-$((20+22))\n")
				client.waitTerminalOutput(t, terminal.TerminalID, "hi-42")
				client.callResult(t, "terminal/close", map[string]any{"terminal_id": terminal.TerminalID}, nil)
				if exited := client.waitTerminalExited(t, terminal.TerminalID); exited.Error != "" {
					t.Fatalf("exited = %+v, want no error", exited)
				}
			},
		},
		{
			name: "relative_directory_is_resolved",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				directory := filepath.Join(h.defaultDirectory, "sub")
				if err := os.Mkdir(directory, 0o700); err != nil {
					t.Fatal(err)
				}
				terminal := openTerminal(t, client, map[string]any{"directory": "sub"})
				if terminal.Directory != directory {
					t.Fatalf("directory = %q, want %q", terminal.Directory, directory)
				}
				client.writeTerminal(t, terminal.TerminalID, "echo cwd=$(pwd -P)\n")
				resolved, err := filepath.EvalSymlinks(directory)
				if err != nil {
					t.Fatal(err)
				}
				client.waitTerminalOutput(t, terminal.TerminalID, "cwd="+regexp.QuoteMeta(resolved))
			},
		},
		{
			name: "resize_notification_changes_terminal_size",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				terminal := openTerminal(t, client, map[string]any{"rows": 24, "cols": 80})
				client.sendNotification(t, "terminal/resize", map[string]any{"terminal_id": terminal.TerminalID, "rows": 30, "cols": 100})
				client.writeTerminal(t, terminal.TerminalID, "echo size=$(stty size)\n")
				client.waitTerminalOutput(t, terminal.TerminalID, "size=30 100")
			},
		},
		{
			name: "client_environment_reaches_shell",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				terminal := openTerminal(t, client, map[string]any{"environment": []string{"PATH=" + os.Getenv("PATH"), "OVTERM_TEST=abc"}})
				client.writeTerminal(t, terminal.TerminalID, "echo $OVTERM_TEST-ok $TERM $COLORTERM\n")
				client.waitTerminalOutput(t, terminal.TerminalID, "abc-ok xterm-256color truecolor")
			},
		},
		{
			name: "unusable_shell_falls_back_and_home_is_set",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				terminal := openTerminal(t, client, map[string]any{"environment": []string{"PATH=" + os.Getenv("PATH"), "SHELL=/nonexistent/shell"}})
				client.writeTerminal(t, terminal.TerminalID, "printf 'ok-%s home=%s\\n' 42 $HOME\n")
				client.waitTerminalOutput(t, terminal.TerminalID, `ok-42 home=/\S+`)
			},
		},
		{
			name: "shell_exit_sends_exited_with_code",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				terminal := openTerminal(t, client, nil)
				client.writeTerminal(t, terminal.TerminalID, "exit 3\n")
				if exited := client.waitTerminalExited(t, terminal.TerminalID); exited.ExitCode != 3 || exited.Error != "" {
					t.Fatalf("exited = %+v, want code 3", exited)
				}
				// 终端结束后移除：再关闭成功，再输入被丢弃。
				client.callResult(t, "terminal/close", map[string]any{"terminal_id": terminal.TerminalID}, nil)
				client.writeTerminal(t, terminal.TerminalID, "echo late\n")
			},
		},
		{
			name: "close_unknown_terminal_succeeds",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				client.callResult(t, "terminal/close", map[string]any{"terminal_id": "gone"}, nil)
			},
		},
		{
			name: "virtual_executor_is_rejected",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				requireErrorCode(t, client.call(t, "terminal/open", map[string]any{"executor_id": "a"}), -32000, "does not support interactive commands")
			},
		},
		{
			name: "unknown_executor_is_rejected",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				requireErrorCode(t, client.call(t, "terminal/open", map[string]any{"executor_id": "gone"}), -32602, `unknown executor "gone"`)
			},
		},
		{
			name: "missing_directory_is_rejected",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				requireErrorCode(t, client.call(t, "terminal/open", map[string]any{"directory": "missing"}), -32602, "not a directory")
			},
		},
		{
			name: "connection_close_terminates_process",
			run: func(t *testing.T, h *serverHarness, client *testClient) {
				terminal := openTerminal(t, client, nil)
				client.writeTerminal(t, terminal.TerminalID, "echo pid=$$\n")
				match := client.waitTerminalOutput(t, terminal.TerminalID, `pid=(\d+)`)
				pid, err := strconv.Atoi(match[1])
				if err != nil {
					t.Fatal(err)
				}
				if err := syscall.Kill(pid, 0); err != nil {
					t.Fatalf("shell %d is not running before the connection closes: %v", pid, err)
				}
				_ = client.conn.CloseNow()
				deadline := time.Now().Add(5 * time.Second)
				for syscall.Kill(pid, 0) == nil {
					if time.Now().After(deadline) {
						t.Fatalf("shell %d is still running after the connection closed", pid)
					}
					time.Sleep(10 * time.Millisecond)
				}
			},
		},
	}
	// 2. 逐项固定 shell 为 bash（serve 继承它，避免开发机登录 shell 的首次启动向导），启动 serve，初始化连接后运行场景。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SHELL", "/bin/bash")
			h := startTestServer(t)
			client := h.dial(t)
			client.initialize(t)
			tt.run(t, h, client)
		})
	}
}

// openTerminal 通过 terminal/open 在 local Executor 上打开终端；params 补充或覆盖默认参数。
func openTerminal(t *testing.T, client *testClient, params map[string]any) runtimeprotocol.TerminalOpenResult {
	t.Helper()
	// 1. 发送请求并解析终端信息。
	var terminal runtimeprotocol.TerminalOpenResult
	client.callResult(t, "terminal/open", params, &terminal)
	if terminal.TerminalID == "" {
		t.Fatalf("terminal = %+v, want a terminal id", terminal)
	}
	return terminal
}

// sendNotification 发送一条没有 id 的客户端通知。
func (c *testClient) sendNotification(t *testing.T, method string, params any) {
	t.Helper()
	// 1. 编码方法与参数，作为一帧发出，不等待回复。
	payload, err := json.Marshal(map[string]any{"method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	c.sendRaw(t, string(payload))
}

// writeTerminal 通过 terminal/input 通知把文本写进终端。
func (c *testClient) writeTerminal(t *testing.T, terminalID, text string) {
	t.Helper()
	// 1. 发送输入通知，字节在 JSON 里自动编码为 base64。
	c.sendNotification(t, "terminal/input", &runtimeprotocol.TerminalInputParams{TerminalID: terminalID, Data: []byte(text)})
}

// waitTerminalOutput 累计终端输出直到匹配正则 pattern，返回 FindStringSubmatch 的结果。
func (c *testClient) waitTerminalOutput(t *testing.T, terminalID, pattern string) []string {
	t.Helper()
	// 1. 逐条读取 terminal/output 通知并拼接，直到累计输出匹配目标正则。
	expression := regexp.MustCompile(pattern)
	var output strings.Builder
	var match []string
	c.collectNotifications(t, func(message clientMessage) bool {
		if message.Method != "terminal/output" {
			return false
		}
		var params runtimeprotocol.TerminalOutputParams
		if err := json.Unmarshal(message.Params, &params); err != nil {
			t.Fatal(err)
		}
		if params.TerminalID == terminalID {
			output.Write(params.Data)
		}
		match = expression.FindStringSubmatch(output.String())
		return match != nil
	})
	return match
}

// waitTerminalExited 等待终端的 terminal/exited 通知并返回它的参数。
func (c *testClient) waitTerminalExited(t *testing.T, terminalID string) runtimeprotocol.TerminalExitedParams {
	t.Helper()
	// 1. 跳过输出通知，读到该终端的 exited 为止。
	var exited runtimeprotocol.TerminalExitedParams
	c.collectNotifications(t, func(message clientMessage) bool {
		if message.Method != "terminal/exited" {
			return false
		}
		if err := json.Unmarshal(message.Params, &exited); err != nil {
			t.Fatal(err)
		}
		return exited.TerminalID == terminalID
	})
	return exited
}
