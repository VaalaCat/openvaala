package appserver

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/VaalaCat/openvaala/internal/config"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/VaalaCat/openvaala/pkg/storage"
	storagefile "github.com/VaalaCat/openvaala/pkg/storage/file"
	storagesqlite "github.com/VaalaCat/openvaala/pkg/storage/sqlite"
)

const (
	historyConversationID defs.ConversationID = "00000000-0000-7000-8000-0000000000c1"
	historyRootSessionID  defs.SessionID      = "00000000-0000-7000-8000-0000000000a0"
	historyChildOneID     defs.SessionID      = "00000000-0000-7000-8000-0000000000a1"
	historyChildTwoID     defs.SessionID      = "00000000-0000-7000-8000-0000000000a2"
	historyGrandchildID   defs.SessionID      = "00000000-0000-7000-8000-0000000000a3"
)

// fakeHistoryClient 提供测试历史查询：游标排除自身，日志游标超过末尾时返回错误，日志页最多 pageSize 条。
type fakeHistoryClient struct {
	runtimeclient.Client
	pageSize      int
	conversations []defs.Conversation
	sessions      []defs.Session
	turns         []defs.Turn
	logs          map[defs.SessionID][]*defs.Item
	// defaultModel 是 model/list 标出的 Agent 默认模型。
	defaultModel defs.ModelRef
	// sessionsReads 是 ListSessions 被调用的次数。
	sessionsReads int
	// itemsReads 是 ListItems 被调用的次数。
	itemsReads int
	// itemsReturned 是 ListItems 累计返回的 Item 条数。
	itemsReturned int
	// itemsRequests 按调用顺序记录每次 ListItems 的请求。
	itemsRequests []runtimeclient.ListItemsRequest
	// turnsReads 是 ListTurns 被调用的次数。
	turnsReads int
	// getTurnsRequests 按调用顺序记录每次 GetTurns 要的 TurnID。
	getTurnsRequests [][]defs.TurnID
	// getSessionByIDErr 非 nil 时 GetSessionByID 不查 sessions，直接返回它，模拟 serve 查找出错。
	getSessionByIDErr error
	// conversationRequests 按调用顺序记录每次 ListConversations 的请求。
	conversationRequests []runtimeclient.ListConversationsRequest
	// modelReads 是 ListModels 被调用的次数。
	modelReads int
}

// getPageLimit 返回测试查询页大小。
func (f *fakeHistoryClient) getPageLimit(limit int, isCapped bool) int {
	// 1. 未指定时使用 pageSize 或 1000，日志短页模式再限制到 pageSize。
	if limit <= 0 {
		return cmp.Or(f.pageSize, 1000)
	}
	if isCapped && f.pageSize > 0 {
		return min(limit, f.pageSize)
	}
	return limit
}

// GetConversation 从测试记录中查找对话。
func (f *fakeHistoryClient) GetConversation(_ context.Context, id defs.ConversationID) (defs.Conversation, error) {
	// 1. 按 ID 返回记录，未找到时返回 ErrConversationNotFound。
	for _, record := range f.conversations {
		if record.ID == id {
			return record, nil
		}
	}
	return defs.Conversation{}, fmt.Errorf("%w: %s", runtime.ErrConversationNotFound, id)
}

// GetSessionByID 从测试记录中查找 Session。
func (f *fakeHistoryClient) GetSessionByID(_ context.Context, id defs.SessionID) (defs.Session, error) {
	// 1. 优先返回预设错误，否则按 ID 查找，缺少记录返回 ErrSessionNotFound。
	if f.getSessionByIDErr != nil {
		return defs.Session{}, f.getSessionByIDErr
	}
	for _, record := range f.sessions {
		if record.ID == id {
			return record, nil
		}
	}
	return defs.Session{}, fmt.Errorf("%w: %s", runtime.ErrSessionNotFound, id)
}

// ListConversations 按测试请求筛选和分页对话，并附带根 Session。
func (f *fakeHistoryClient) ListConversations(_ context.Context, request *runtimeclient.ListConversationsRequest) (runtimeclient.ListConversationsResult, error) {
	// 1. 记录请求，按更新时间和 ID 排序。
	f.conversationRequests = append(f.conversationRequests, *request)
	records := slices.SortedFunc(slices.Values(f.conversations), func(a, b defs.Conversation) int {
		return cmp.Or(cmp.Compare(a.UpdatedAt, b.UpdatedAt), cmp.Compare(a.ID, b.ID))
	})
	if request.Order.Direction == storage.DirectionDesc {
		slices.Reverse(records)
	}
	// 2. 按归档、目录、标题和排除式游标筛选。
	records = slices.DeleteFunc(records, func(record defs.Conversation) bool {
		if (record.ArchivedAt != 0) != request.Archived {
			return true
		}
		if len(request.Directories) > 0 && !slices.Contains(request.Directories, record.ExecutorSettings.GetConversationDirectory()) {
			return true
		}
		if !strings.Contains(record.Title, request.SearchTerm) {
			return true
		}
		if request.Cursor.ID == "" {
			return false
		}
		ordering := cmp.Or(cmp.Compare(record.UpdatedAt, request.Cursor.UpdatedAt), cmp.Compare(record.ID, request.Cursor.ID))
		if request.Order.Direction == storage.DirectionDesc {
			return ordering >= 0
		}
		return ordering <= 0
	})
	// 3. 按页大小截取并填写游标，查找每个对话的根 Session。
	var result runtimeclient.ListConversationsResult
	limit := f.getPageLimit(request.Limit, false)
	if len(records) > limit {
		records = records[:limit]
		result.NextCursor = dao.RecordCursor[defs.ConversationID]{
			ID:        records[limit-1].ID,
			UpdatedAt: records[limit-1].UpdatedAt,
		}
	}
	for _, record := range records {
		entry := dao.ConversationListEntry{Conversation: record}
		for _, session := range f.sessions {
			if session.ID == record.RootSessionID && session.ConversationID == record.ID {
				entry.RootSession = session
				break
			}
		}
		result.Conversations = append(result.Conversations, entry)
	}
	return result, nil
}

// ListModels 返回预设的 Agent 默认模型。
func (f *fakeHistoryClient) ListModels(context.Context, string) (runtime.ModelList, error) {
	// 1. 累计模型查询次数，返回默认模型引用。
	f.modelReads++
	return runtime.ModelList{AgentDefaultModel: f.defaultModel}, nil
}

// ListSessions 返回对话内游标之后的 Session 页。
func (f *fakeHistoryClient) ListSessions(_ context.Context, request *runtimeclient.ListSessionsRequest) (runtimeclient.ListSessionsResult, error) {
	// 1. 累计查询次数，按对话 ID 和排除式游标筛选，再按页大小截取。
	f.sessionsReads++
	var sessions []defs.Session
	for _, record := range f.sessions {
		if record.ConversationID == request.ConversationID && record.ID > request.Cursor.ID {
			sessions = append(sessions, record)
		}
	}
	var result runtimeclient.ListSessionsResult
	if limit := f.getPageLimit(request.Limit, false); len(sessions) > limit {
		sessions = sessions[:limit]
		result.NextCursor = dao.RecordCursor[defs.SessionID]{ID: sessions[limit-1].ID}
	}
	result.Sessions = sessions
	return result, nil
}

// GetTurns 按请求顺序返回存在的测试 Turn。
func (f *fakeHistoryClient) GetTurns(_ context.Context, request *runtimeclient.GetTurnsRequest) ([]defs.Turn, error) {
	// 1. 记录 Turn ID 请求，逐项查找指定 Session 的 Turn，跳过不存在记录。
	f.getTurnsRequests = append(f.getTurnsRequests, slices.Clone(request.TurnIDs))
	var turns []defs.Turn
	for _, id := range request.TurnIDs {
		index := slices.IndexFunc(f.turns, func(record defs.Turn) bool { return record.SessionID == request.SessionID && record.ID == id })
		if index >= 0 {
			turns = append(turns, f.turns[index])
		}
	}
	return turns, nil
}

// ListTurns 按测试请求排序和分页 Session Turn。
func (f *fakeHistoryClient) ListTurns(_ context.Context, request *runtimeclient.ListTurnsRequest) (runtimeclient.ListTurnsResult, error) {
	// 1. 累计查询次数，按 Session 和游标筛选，排序后截取并返回下一页游标。
	f.turnsReads++
	turns := slices.DeleteFunc(slices.Clone(f.turns), func(record defs.Turn) bool {
		if record.SessionID != request.SessionID {
			return true
		}
		if request.Cursor.ID == "" {
			return false
		}
		if request.Order.Direction == storage.DirectionDesc {
			return record.ID >= request.Cursor.ID
		}
		return record.ID <= request.Cursor.ID
	})
	slices.SortFunc(turns, func(a, b defs.Turn) int { return cmp.Compare(a.ID, b.ID) })
	if request.Order.Direction == storage.DirectionDesc {
		slices.Reverse(turns)
	}
	var result runtimeclient.ListTurnsResult
	if limit := f.getPageLimit(request.Limit, false); len(turns) > limit {
		turns = turns[:limit]
		result.NextCursor = dao.RecordCursor[defs.TurnID]{ID: turns[limit-1].ID}
	}
	result.Turns = turns
	return result, nil
}

// ListItems 分页读取测试 Session 日志并记录查询情况。
func (f *fakeHistoryClient) ListItems(_ context.Context, request *runtimeclient.ListItemsRequest) (runtimeclient.ListItemsResult, error) {
	// 1. 记录请求，按方向和排除式游标取得日志页，越界时返回错误，收集已返回条数。
	f.itemsReads++
	f.itemsRequests = append(f.itemsRequests, *request)
	log := f.logs[request.SessionID]
	count := len(log)
	if request.Cursor > storage.Ordinal(count) {
		return runtimeclient.ListItemsResult{}, fmt.Errorf("ordinal %d is beyond log tail %d", request.Cursor, count)
	}
	var ordinals []int
	limit := f.getPageLimit(request.Limit, true)
	if request.Order == storage.DirectionDesc {
		end := count
		if request.Cursor > 0 {
			end = int(request.Cursor) - 1
		}
		for ordinal := end; ordinal >= 1 && len(ordinals) < limit; ordinal-- {
			ordinals = append(ordinals, ordinal)
		}
	} else {
		for ordinal := int(request.Cursor) + 1; ordinal <= count && len(ordinals) < limit; ordinal++ {
			ordinals = append(ordinals, ordinal)
		}
	}
	var result runtimeclient.ListItemsResult
	for _, ordinal := range ordinals {
		result.Items = append(result.Items, runtimeprotocol.LoggedItem{
			Ordinal: storage.Ordinal(ordinal),
			Item:    log[ordinal-1],
		})
	}
	if len(ordinals) > 0 {
		last := ordinals[len(ordinals)-1]
		if (request.Order == storage.DirectionDesc && last > 1) || (request.Order != storage.DirectionDesc && last < count) {
			result.NextCursor = storage.Ordinal(last)
		}
	}
	f.itemsReturned += len(result.Items)
	return result, nil
}

// newHistoryConnection 创建未加载 Thread 的历史测试连接。
func newHistoryConnection(client runtimeclient.Client) *appServerConnection {
	// 1. 保存控制 Client，设置本机 serve、版本和空 Thread 登记。
	return &appServerConnection{
		client:                   client,
		isLocalServer:            true,
		version:                  "1.2.3",
		loadedThreads:            map[string]*connectionThread{},
		agentName:                "assistant",
		childThreadConversations: map[defs.SessionID]defs.ConversationID{},
	}
}

// newHistoryMessage 创建指定 Turn 的测试用户消息。
func newHistoryMessage(id string, turnID defs.TurnID) *defs.Item {
	// 1. 创建用户消息并设置 Turn ID。
	item := newTestMessage(id, testUser, newTestText(id))
	item.TurnID = turnID
	return item
}

// newHistoryReply 创建指定 Turn 的测试模型回复。
func newHistoryReply(id string, turnID defs.TurnID) *defs.Item {
	// 1. 使用根 Agent 来源创建回复并设置 Turn ID。
	item := newTestReply(id, &defs.Address{
		Kind: defs.IdentityAgent,
		ID:   string(historyRootSessionID),
	}, newTestText(id))
	item.TurnID = turnID
	return item
}

// newHistoryCall 创建指定 Turn 的测试 bash 调用。
func newHistoryCall(id string, turnID defs.TurnID, callID string) *defs.Item {
	// 1. 使用 echo 命令创建调用并设置 Turn ID。
	call := newTestCall(id, callID, "bash", map[string]any{"command": "echo " + callID})
	call.TurnID = turnID
	return call
}

// newHistoryResult 创建指定 Turn 的成功命令结果。
func newHistoryResult(turnID defs.TurnID, callID string) *defs.Item {
	// 1. 创建带输出和零退出码的结果，设置 Turn ID。
	result := newTestResult(callID, "local", `{"command_id":"cmd_000000000001","status":"exited","exit_code":0,"output":"out\n","next_cursor":4,"has_more":false}`, nil)
	result.TurnID = turnID
	return result
}

// newHistoryFork 创建从根 Session 分叉的测试 Item。
func newHistoryFork() *defs.Item {
	// 1. 填写 session_fork 和来源 Session ID。
	return &defs.Item{
		ID:          "fork",
		Kind:        defs.ItemSessionFork,
		SessionFork: &defs.SessionFork{SourceSessionID: historyRootSessionID},
	}
}

// newFollowStart 创建历史读取终点和接入时活动 Turn。
func newFollowStart(ordinal storage.Ordinal, activeTurnID defs.TurnID) *historyReadEnd {
	// 1. 保存流水号和活动 Turn ID。
	return &historyReadEnd{
		ordinal:      ordinal,
		activeTurnID: activeTurnID,
	}
}

// getItemIDs 按原顺序返回 Item ID。
func getItemIDs(items []*defs.Item) []string {
	// 1. 遍历 Item 列表收集 ID。
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, string(item.ID))
	}
	return ids
}

// TestReadItemsPage 检查日志分页、工具配对、分叉分界和 Turn 过滤。
func TestReadItemsPage(t *testing.T) {
	// 1. 准备普通、并行工具、中断、分叉和多轮日志。
	messages := []*defs.Item{
		newHistoryMessage("m1", "t1"), newHistoryMessage("m2", "t1"), newHistoryMessage("m3", "t1"),
		newHistoryMessage("m4", "t1"), newHistoryMessage("m5", "t1"), newHistoryMessage("m6", "t1"),
	}
	withToolCall := []*defs.Item{
		newHistoryMessage("u1", "t1"), newHistoryCall("c1", "t1", "call-1"), newHistoryResult("t1", "call-1"),
		newHistoryReply("a1", "t1"), newHistoryMessage("u2", "t2"), newHistoryReply("a2", "t2"),
	}
	parallelCalls := []*defs.Item{
		newHistoryCall("ca", "t1", "call-a"), newHistoryCall("cb", "t1", "call-b"),
		newHistoryResult("t1", "call-a"), newHistoryResult("t1", "call-b"), newHistoryReply("a1", "t1"),
	}
	interrupted := []*defs.Item{newHistoryMessage("u1", "t1"), newInterruptedTestCall("c1", "call-1", "bash", `{"comm`), newHistoryReply("a1", "t1")}
	interrupted[1].TurnID = "t1"
	forked := []*defs.Item{newHistoryMessage("p1", "t0"), newHistoryReply("p2", "t0"), newHistoryFork(), newHistoryMessage("k1", "t1"), newHistoryReply("k2", "t1")}
	turns := []*defs.Item{
		newHistoryMessage("m1", "t1"), newHistoryMessage("m2", "t1"), newHistoryMessage("m3", "t2"),
		newHistoryMessage("m4", "t2"), newHistoryMessage("m5", "t3"),
	}
	tests := []struct {
		name       string
		log        []*defs.Item
		query      itemsPageQuery
		wantIDs    []string
		wantCursor storage.Ordinal
		wantError  string
	}{
		{
			name: "desc_first_page",
			log:  messages,
			query: itemsPageQuery{
				order: storage.DirectionDesc,
				limit: 3,
			},
			wantIDs:    []string{"m6", "m5", "m4"},
			wantCursor: 4,
		},
		{
			name: "desc_last_page",
			log:  messages,
			query: itemsPageQuery{
				order:  storage.DirectionDesc,
				limit:  3,
				cursor: 4,
			},
			wantIDs: []string{"m3", "m2", "m1"},
		},
		{
			name: "asc_first_page",
			log:  messages,
			query: itemsPageQuery{
				order: storage.DirectionAsc,
				limit: 4,
			},
			wantIDs:    []string{"m1", "m2", "m3", "m4"},
			wantCursor: 4,
		},
		{
			name: "asc_last_page",
			log:  messages,
			query: itemsPageQuery{
				order:  storage.DirectionAsc,
				limit:  4,
				cursor: 4,
			},
			wantIDs: []string{"m5", "m6"},
		},
		{
			name: "desc_result_pulls_in_its_call",
			log:  withToolCall,
			query: itemsPageQuery{
				order:  storage.DirectionDesc,
				limit:  2,
				cursor: 5,
			},
			wantIDs:    []string{"a1", "result-call-1", "c1"},
			wantCursor: 2,
		},
		{
			name: "desc_page_before_the_pair_resumes_after_it",
			log:  withToolCall,
			query: itemsPageQuery{
				order:  storage.DirectionDesc,
				limit:  2,
				cursor: 2,
			},
			wantIDs: []string{"u1"},
		},
		{
			name: "asc_call_pulls_in_its_result",
			log:  withToolCall,
			query: itemsPageQuery{
				order: storage.DirectionAsc,
				limit: 2,
			},
			wantIDs:    []string{"u1", "c1", "result-call-1"},
			wantCursor: 3,
		},
		{
			name: "desc_parallel_results_pull_in_both_calls",
			log:  parallelCalls,
			query: itemsPageQuery{
				order:  storage.DirectionDesc,
				limit:  1,
				cursor: 5,
			},
			wantIDs: []string{"result-call-b", "result-call-a", "cb", "ca"},
		},
		{
			name: "asc_running_call_at_log_end_stays_alone",
			log:  withToolCall[:2],
			query: itemsPageQuery{
				order: storage.DirectionAsc,
				limit: 5,
			},
			wantIDs: []string{"u1", "c1"},
		},
		{
			name: "asc_interrupted_call_is_not_split",
			log:  interrupted,
			query: itemsPageQuery{
				order: storage.DirectionAsc,
				limit: 2,
			},
			wantIDs:    []string{"u1", "c1"},
			wantCursor: 2,
		},
		{
			name: "desc_stops_at_session_fork",
			log:  forked,
			query: itemsPageQuery{
				order: storage.DirectionDesc,
				limit: 10,
			},
			wantIDs: []string{"k2", "k1"},
		},
		{
			name: "desc_session_fork_ends_paging_even_when_the_page_is_full",
			log:  forked,
			query: itemsPageQuery{
				order:  storage.DirectionDesc,
				limit:  1,
				cursor: 5,
			},
			wantIDs: []string{"k1"},
		},
		{
			name: "asc_sub_agent_starts_after_the_session_fork",
			log:  forked,
			query: itemsPageQuery{
				order:             storage.DirectionAsc,
				limit:             10,
				isSubAgentSession: true,
			},
			wantIDs: []string{"k1", "k2"},
		},
		{
			name: "asc_sub_agent_skips_the_copied_history_even_with_a_small_limit",
			log:  forked,
			query: itemsPageQuery{
				order:             storage.DirectionAsc,
				limit:             1,
				isSubAgentSession: true,
			},
			wantIDs:    []string{"k1"},
			wantCursor: 4,
		},
		{
			name: "asc_sub_agent_second_page_already_past_the_fork",
			log:  forked,
			query: itemsPageQuery{
				order:             storage.DirectionAsc,
				limit:             1,
				cursor:            4,
				isSubAgentSession: true,
			},
			wantIDs: []string{"k2"},
		},
		{
			name: "asc_sub_agent_turn_filter_skips_the_copied_history",
			log:  forked,
			query: itemsPageQuery{
				order: storage.DirectionAsc,
				limit: 10,
				turn: &defs.Turn{
					ID: "t1",
					ItemsRange: defs.TurnItemRange{
						FirstOrdinal: 4,
						LastOrdinal:  5,
					},
				},
				isSubAgentSession: true,
			},
			wantIDs: []string{"k1", "k2"},
		},
		{
			name: "asc_root_session_reads_from_the_head",
			log:  forked,
			query: itemsPageQuery{
				order: storage.DirectionAsc,
				limit: 2,
			},
			wantIDs:    []string{"p1", "p2"},
			wantCursor: 2,
		},
		{
			name: "turn_filter_desc_stops_before_older_turns",
			log:  turns,
			query: itemsPageQuery{
				order: storage.DirectionDesc,
				limit: 10,
				turn: &defs.Turn{
					ID: "t2",
					ItemsRange: defs.TurnItemRange{
						FirstOrdinal: 3,
						LastOrdinal:  4,
					},
				},
			},
			wantIDs: []string{"m4", "m3"},
		},
		{
			name: "turn_filter_desc_page_cursor",
			log:  turns,
			query: itemsPageQuery{
				order: storage.DirectionDesc,
				limit: 1,
				turn: &defs.Turn{
					ID: "t2",
					ItemsRange: defs.TurnItemRange{
						FirstOrdinal: 3,
						LastOrdinal:  4,
					},
				},
			},
			wantIDs:    []string{"m4"},
			wantCursor: 4,
		},
		{
			name: "turn_filter_desc_last_page_has_no_cursor",
			log:  turns,
			query: itemsPageQuery{
				order: storage.DirectionDesc,
				limit: 1,
				turn: &defs.Turn{
					ID: "t2",
					ItemsRange: defs.TurnItemRange{
						FirstOrdinal: 3,
						LastOrdinal:  4,
					},
				},
				cursor: 4,
			},
			wantIDs: []string{"m3"},
		},
		{
			name: "turn_filter_asc_stops_after_the_turn",
			log:  turns,
			query: itemsPageQuery{
				order: storage.DirectionAsc,
				limit: 10,
				turn: &defs.Turn{
					ID: "t2",
					ItemsRange: defs.TurnItemRange{
						FirstOrdinal: 3,
						LastOrdinal:  4,
					},
				},
			},
			wantIDs: []string{"m3", "m4"},
		},
		{
			name: "cursor_beyond_tail_is_invalid",
			log:  messages,
			query: itemsPageQuery{
				order:  storage.DirectionDesc,
				limit:  3,
				cursor: 99,
			},
			wantError: "invalid cursor: 99",
		},
		{
			name: "cursor_on_empty_log_is_invalid",
			query: itemsPageQuery{
				order:  storage.DirectionAsc,
				limit:  3,
				cursor: 1,
			},
			wantError: "invalid cursor: 1",
		},
	}
	// 2. 读取各页，核对 Item ID、原始流水号游标和非法游标错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newHistoryConnection(&fakeHistoryClient{
				pageSize: 2,
				logs:     map[defs.SessionID][]*defs.Item{historyRootSessionID: tt.log},
			})
			page, err := c.readItemsPage(t.Context(), historyConversationID, historyRootSessionID, tt.query, nil)
			if tt.wantError != "" {
				var protocolErr *codexError
				if !errors.As(err, &protocolErr) || protocolErr.Code != errorCodeInvalidParams || protocolErr.Message != tt.wantError {
					t.Fatalf("readItemsPage error = %v, want -32602 %q", err, tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := getItemIDs(page.items); !slices.Equal(got, tt.wantIDs) || page.nextCursor != tt.wantCursor {
				t.Fatalf("readItemsPage = %v with cursor %d, want %v with cursor %d", got, page.nextCursor, tt.wantIDs, tt.wantCursor)
			}
		})
	}
}

// TestReadItemsPageWalk 检查连续翻页完整且不拆分工具调用。
func TestReadItemsPageWalk(t *testing.T) {
	// 1. 准备两轮含并行工具的完整日志。
	log := []*defs.Item{
		newHistoryMessage("u1", "t1"), newHistoryCall("ca", "t1", "call-a"), newHistoryCall("cb", "t1", "call-b"),
		newHistoryResult("t1", "call-a"), newHistoryResult("t1", "call-b"), newHistoryReply("a1", "t1"),
		newHistoryMessage("u2", "t2"), newHistoryCall("cc", "t2", "call-c"), newHistoryResult("t2", "call-c"), newHistoryReply("a2", "t2"),
	}
	// 2. 遍历方向和页大小，沿游标读完日志，核对每页配对和整体顺序。
	for _, order := range []storage.Direction{storage.DirectionDesc, storage.DirectionAsc} {
		for _, limit := range []int{1, 2, 3, 4, 7, 20} {
			t.Run(fmt.Sprintf("%s_limit_%d", order, limit), func(t *testing.T) {
				c := newHistoryConnection(&fakeHistoryClient{
					pageSize: 3,
					logs:     map[defs.SessionID][]*defs.Item{historyRootSessionID: log},
				})
				var walked []*defs.Item
				query := itemsPageQuery{
					order: order,
					limit: limit,
				}
				for pages := 0; ; pages++ {
					page, err := c.readItemsPage(t.Context(), historyConversationID, historyRootSessionID, query, nil)
					if err != nil {
						t.Fatal(err)
					}
					if isToolCallSplit(page.items, order) {
						t.Fatalf("page %d %v splits a tool call from its result", pages, getItemIDs(page.items))
					}
					walked = append(walked, page.items...)
					if page.nextCursor == 0 {
						break
					}
					if pages > len(log) {
						t.Fatalf("walk did not end after %d pages", pages)
					}
					query.cursor = page.nextCursor
				}
				want := slices.Clone(log)
				if order == storage.DirectionDesc {
					slices.Reverse(want)
				}
				if got := getItemIDs(walked); !slices.Equal(got, getItemIDs(want)) {
					t.Fatalf("pages concatenate to %v, want every item once in order: %v", got, getItemIDs(want))
				}
			})
		}
	}
}

// newHistoryConversationClient 创建含两轮不同目录的历史测试对话。
func newHistoryConversationClient(log []*defs.Item) *fakeHistoryClient {
	// 1. 设置根模型 local/default、两轮 /old 和 /new 目录及日志，补齐 Turn 的 Item 范围。
	rootModel := defs.ModelSelection{
		Ref: defs.ModelRef{
			ProviderID: "local",
			ModelID:    "default",
		},
		ReasoningEffort: "high",
	}
	client := &fakeHistoryClient{
		pageSize: 2,
		conversations: []defs.Conversation{{
			ID:               historyConversationID,
			RootSessionID:    historyRootSessionID,
			Title:            "History",
			UpdatedAt:        5000,
			ExecutorSettings: defs.ExecutorSettings{WorkspaceDirectories: map[string]string{defs.LocalExecutorID: "/new"}},
		}},
		sessions: []defs.Session{{
			ID:             historyRootSessionID,
			ConversationID: historyConversationID,
			AgentName:      "assistant",
			Model:          rootModel,
		}},
		turns: []defs.Turn{
			{
				ID:        "t1",
				SessionID: historyRootSessionID,
				Status:    defs.TurnStatusCompleted,
				Executors: []defs.TurnExecutor{{
					ID:                 "local",
					WorkspaceDirectory: "/old",
				}},
			},
			{
				ID:        "t2",
				SessionID: historyRootSessionID,
				Status:    defs.TurnStatusRunning,
				Executors: []defs.TurnExecutor{{
					ID:                 "local",
					WorkspaceDirectory: "/new",
				}},
			},
		},
		logs: map[defs.SessionID][]*defs.Item{historyRootSessionID: log},
	}
	setHistoryTurnRanges(client)
	return client
}

// summarizeEntries 将条目概括为 Turn、类型和 ID。
func summarizeEntries(entries []codexThreadItemEntry) []string {
	// 1. 按条目类型输出摘要，命令额外包含状态和 cwd。
	summary := make([]string, 0, len(entries))
	for _, entry := range entries {
		switch item := entry.Item.(type) {
		case *codexUserMessageItem:
			summary = append(summary, entry.TurnID+":user:"+item.ID)
		case *codexAgentMessageItem:
			summary = append(summary, entry.TurnID+":agent:"+item.ID)
		case *codexCommandExecutionItem:
			summary = append(summary, fmt.Sprintf("%s:command:%s:%s@%s", entry.TurnID, item.ID, item.Status, item.Cwd))
		default:
			summary = append(summary, fmt.Sprintf("%s:%T", entry.TurnID, item))
		}
	}
	return summary
}

// TestListThreadItems 检查 Item 列表翻译、分页及每页 Turn 记录查询。
func TestListThreadItems(t *testing.T) {
	// 1. 准备已完成轮次和执行中调用，列出请求与预期结果。
	log := []*defs.Item{
		newHistoryMessage("u1", "t1"), newHistoryCall("c1", "t1", "call-1"), newHistoryResult("t1", "call-1"), newHistoryReply("a1", "t1"),
		newHistoryMessage("u2", "t2"), newHistoryCall("c2", "t2", "call-2"),
	}
	thread := `"threadId":"` + string(historyConversationID) + `"`
	tests := []struct {
		name       string
		params     string
		want       []string
		wantCursor string
		// wantTurnIDs 是这一页向 serve 要的 Turn 记录：一次 GetTurns，只要本页 Item 所属的 Turn，按页内出现的先后。
		wantTurnIDs []defs.TurnID
		wantError   *codexError
	}{
		{
			// 默认方向照 Codex 0.163.0：正序，最早的在前。
			name:        "default_is_oldest_first_with_the_running_call",
			params:      `{` + thread + `}`,
			want:        []string{"t1:user:u1", "t1:command:c1:completed@/old", "t1:agent:a1", "t2:user:u2", "t2:command:c2:inProgress@/new"},
			wantTurnIDs: []defs.TurnID{"t1", "t2"},
		},
		{
			name:        "descending_is_newest_first",
			params:      `{` + thread + `,"sortDirection":"desc"}`,
			want:        []string{"t2:command:c2:inProgress@/new", "t2:user:u2", "t1:agent:a1", "t1:command:c1:completed@/old", "t1:user:u1"},
			wantTurnIDs: []defs.TurnID{"t1", "t2"},
		},
		{
			name:        "ascending_page_pulls_in_the_tool_result",
			params:      `{` + thread + `,"limit":2}`,
			want:        []string{"t1:user:u1", "t1:command:c1:completed@/old"},
			wantCursor:  "3",
			wantTurnIDs: []defs.TurnID{"t1"},
		},
		{
			name:        "turn_filter",
			params:      `{` + thread + `,"turnId":"t1"}`,
			want:        []string{"t1:user:u1", "t1:command:c1:completed@/old", "t1:agent:a1"},
			wantTurnIDs: []defs.TurnID{"t1"},
		},
		{
			name:        "limit_counts_raw_items_and_the_cursor_continues_after_the_last_one_read",
			params:      `{` + thread + `,"sortDirection":"desc","limit":2}`,
			want:        []string{"t2:command:c2:inProgress@/new", "t2:user:u2"},
			wantCursor:  "5",
			wantTurnIDs: []defs.TurnID{"t2"},
		},
		{
			// 这一页全在 t1 里，离日志末尾（t2）隔着一整个 Turn：只要 t1 的记录，不读 t2 的。
			name:        "page_far_from_the_log_end_asks_only_for_its_own_turn",
			params:      `{` + thread + `,"sortDirection":"desc","limit":2,"cursor":"4"}`,
			want:        []string{"t1:command:c1:completed@/old"},
			wantCursor:  "2",
			wantTurnIDs: []defs.TurnID{"t1"},
		},
		{
			name:   "unknown_thread",
			params: `{"threadId":"missing"}`,
			wantError: &codexError{
				errorCodeInvalidRequest,
				"thread not loaded: missing",
			},
		},
		{
			name:   "cursor_is_not_a_number",
			params: `{` + thread + `,"cursor":"abc"}`,
			wantError: &codexError{
				errorCodeInvalidParams,
				"invalid cursor: abc",
			},
		},
		{
			name:   "cursor_is_zero",
			params: `{` + thread + `,"cursor":"0"}`,
			wantError: &codexError{
				errorCodeInvalidParams,
				"invalid cursor: 0",
			},
		},
		{
			name:   "cursor_is_past_the_log",
			params: `{` + thread + `,"cursor":"7"}`,
			wantError: &codexError{
				errorCodeInvalidParams,
				"invalid cursor: 7",
			},
		},
		{
			name:   "unknown_sort_direction",
			params: `{` + thread + `,"sortDirection":"up"}`,
			wantError: &codexError{
				errorCodeInvalidParams,
				"invalid sortDirection: up",
			},
		},
	}
	// 2. 转换协议请求，核对条目、游标和仅查询本页 Turn 的调用记录。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newHistoryConversationClient(log)
			c := newHistoryConnection(client)
			var params codexThreadItemsListParams
			if err := decodeCodexParams(json.RawMessage(tt.params), &params); err != nil {
				t.Fatal(err)
			}
			result, err := c.listThreadItems(t.Context(), params)
			if tt.wantError != nil {
				var protocolErr *codexError
				if !errors.As(err, &protocolErr) || *protocolErr != *tt.wantError {
					t.Fatalf("listThreadItems error = %v, want %+v", err, *tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.EqualFunc(client.getTurnsRequests, [][]defs.TurnID{tt.wantTurnIDs}, slices.Equal[[]defs.TurnID]) || client.turnsReads != 0 {
				t.Fatalf("listThreadItems asked for Turns %v and listed Turns %d times, want one request for %v and no listing", client.getTurnsRequests, client.turnsReads, tt.wantTurnIDs)
			}
			page := result.(codexThreadItemsListResult)
			var gotCursor string
			if page.NextCursor != nil {
				gotCursor = *page.NextCursor
			}
			if got := summarizeEntries(page.Data); !slices.Equal(got, tt.want) || gotCursor != tt.wantCursor || page.BackwardsCursor != nil {
				t.Fatalf("listThreadItems = %v cursor %q backwards %v, want %v cursor %q and no backwards cursor", got, gotCursor, page.BackwardsCursor, tt.want, tt.wantCursor)
			}
		})
	}
}

// TestListThreadTurns 检查 Turn 列表状态、分页和 Item 展示方式。
func TestListThreadTurns(t *testing.T) {
	// 1. 准备完成、中断、失败和运行中的 Turn，设置正倒序与展示选项。
	const (
		turn1 = "00000000-0000-7000-8000-000000000001"
		turn2 = "00000000-0000-7000-8000-000000000002"
		turn3 = "00000000-0000-7000-8000-000000000003"
		turn4 = "00000000-0000-7000-8000-000000000004"
	)
	log := []*defs.Item{
		newHistoryMessage("u1", turn1), newHistoryReply("a1", turn1), newHistoryMessage("u2", turn2), newHistoryReply("a2", turn2),
		newHistoryMessage("u3", turn3), newHistoryReply("a3", turn3), newHistoryMessage("u4", turn4),
	}
	records := []defs.Turn{
		{
			ID:        turn1,
			SessionID: historyRootSessionID,
			Status:    defs.TurnStatusCompleted,
		},
		{
			ID:        turn2,
			SessionID: historyRootSessionID,
			Status:    defs.TurnStatusInterrupted,
		},
		{
			ID:        turn3,
			SessionID: historyRootSessionID,
			Status:    defs.TurnStatusFailed,
			Error:     "model refused",
		},
		{
			ID:        turn4,
			SessionID: historyRootSessionID,
			Status:    defs.TurnStatusRunning,
		},
	}
	thread := `"threadId":"` + string(historyConversationID) + `"`
	tests := []struct {
		name       string
		params     string
		want       []string
		wantCursor string
		wantError  *codexError
	}{
		{
			name:       "desc_notLoaded_first_page",
			params:     `{` + thread + `,"limit":2,"sortDirection":"desc","itemsView":"notLoaded"}`,
			want:       []string{turn4 + ":inProgress:notLoaded:0", turn3 + ":failed:notLoaded:0"},
			wantCursor: turn3,
		},
		{
			name:   "desc_notLoaded_last_page",
			params: `{` + thread + `,"limit":2,"itemsView":"notLoaded","cursor":"` + turn3 + `"}`,
			want:   []string{turn2 + ":interrupted:notLoaded:0", turn1 + ":completed:notLoaded:0"},
		},
		{
			name:       "asc_full",
			params:     `{` + thread + `,"limit":2,"sortDirection":"asc","itemsView":"full"}`,
			want:       []string{turn1 + ":completed:full:2", turn2 + ":interrupted:full:2"},
			wantCursor: turn2,
		},
		{
			name:       "summary_is_the_default_and_reads_like_full",
			params:     `{` + thread + `,"limit":3}`,
			want:       []string{turn4 + ":inProgress:full:1", turn3 + ":failed:full:2", turn2 + ":interrupted:full:2"},
			wantCursor: turn2,
		},
		{
			name:   "unknown_thread",
			params: `{"threadId":"missing"}`,
			wantError: &codexError{
				errorCodeInvalidRequest,
				"thread not loaded: missing",
			},
		},
		{
			name:   "cursor_is_not_a_turn_id",
			params: `{` + thread + `,"cursor":"abc"}`,
			wantError: &codexError{
				errorCodeInvalidParams,
				"invalid cursor: abc",
			},
		},
		{
			name:   "unknown_items_view",
			params: `{` + thread + `,"itemsView":"detailed"}`,
			wantError: &codexError{
				errorCodeInvalidParams,
				"invalid itemsView: detailed",
			},
		},
		{
			name:   "unknown_sort_direction",
			params: `{` + thread + `,"sortDirection":"up"}`,
			wantError: &codexError{
				errorCodeInvalidParams,
				"invalid sortDirection: up",
			},
		},
	}
	// 2. 读取 Turn 列表，核对状态、Item 展示方式、条数、游标和参数错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newHistoryConversationClient(log)
			client.turns = slices.Clone(records)
			setHistoryTurnRanges(client)
			c := newHistoryConnection(client)
			var params codexThreadTurnsListParams
			if err := decodeCodexParams(json.RawMessage(tt.params), &params); err != nil {
				t.Fatal(err)
			}
			result, err := c.listThreadTurns(t.Context(), params)
			if tt.wantError != nil {
				var protocolErr *codexError
				if !errors.As(err, &protocolErr) || *protocolErr != *tt.wantError {
					t.Fatalf("listThreadTurns error = %v, want %+v", err, *tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			page := result.(codexThreadTurnsListResult)
			got := make([]string, 0, len(page.Data))
			for _, turn := range page.Data {
				got = append(got, fmt.Sprintf("%s:%s:%s:%d", turn.ID, turn.Status, turn.ItemsView, len(turn.Items)))
			}
			var gotCursor string
			if page.NextCursor != nil {
				gotCursor = *page.NextCursor
			}
			if !slices.Equal(got, tt.want) || gotCursor != tt.wantCursor {
				t.Fatalf("listThreadTurns = %v cursor %q, want %v cursor %q", got, gotCursor, tt.want, tt.wantCursor)
			}
		})
	}
	// 3. 失败的 Turn 带上记录里的错误。
	t.Run("failed_turn_carries_its_error", func(t *testing.T) {
		client := newHistoryConversationClient(log)
		client.turns = slices.Clone(records)
		setHistoryTurnRanges(client)
		result, err := newHistoryConnection(client).listThreadTurns(t.Context(), codexThreadTurnsListParams{ThreadID: string(historyConversationID)})
		if err != nil {
			t.Fatal(err)
		}
		failed := result.(codexThreadTurnsListResult).Data[1]
		if failed.ID != turn3 || failed.Error == nil || failed.Error.Message != "model refused" {
			t.Fatalf("turn %s error = %+v, want the Turn record's error", failed.ID, failed.Error)
		}
	})
}

// TestListThreadHistoryOfFollowedThread 检查已跟随 Thread 的历史终点和实时内容衔接。
func TestListThreadHistoryOfFollowedThread(t *testing.T) {
	// 1. 准备完整历史和不同跟随状态、起点游标、终点。
	// 日志：t1 一轮完整的；t2 里 c2 在位置 6 保存，结果和回复在位置 7、8 才保存。
	log := []*defs.Item{
		newHistoryMessage("u1", "t1"), newHistoryCall("c1", "t1", "call-1"), newHistoryResult("t1", "call-1"), newHistoryReply("a1", "t1"),
		newHistoryMessage("u2", "t2"), newHistoryCall("c2", "t2", "call-2"), newHistoryResult("t2", "call-2"), newHistoryReply("a2", "t2"),
	}
	resumeCursor := codexNewestHistoryCursor
	tests := []struct {
		name string
		// isFollowed 本连接是否跟随着这个 Thread？
		isFollowed bool
		// isFinished 跟随的 Thread 是否已经结束？
		isFinished bool
		// historyEnd 是 Thread 回答的 historyEndOrdinal。
		historyEnd storage.Ordinal
		cursor     *string
		wantItems  []string
		// wantTurns 是 thread/turns/list 的 summary，从新到旧，每个写成「TurnID:状态:Item 数」。
		wantTurns []string
	}{
		{
			name:      "not_followed_reads_to_the_log_end",
			wantItems: []string{"t1:user:u1", "t1:command:c1:completed@/old", "t1:agent:a1", "t2:user:u2", "t2:command:c2:completed@/new", "t2:agent:a2"},
			wantTurns: []string{"t2:inProgress:3", "t1:completed:3"},
		},
		{
			// 没有跟随的 Thread 不会有这个位置。
			name:       "not_followed_ignores_the_position",
			historyEnd: 4,
			cursor:     &resumeCursor,
			wantItems:  []string{"t1:user:u1", "t1:command:c1:completed@/old", "t1:agent:a1", "t2:user:u2", "t2:command:c2:completed@/new", "t2:agent:a2"},
			wantTurns:  []string{"t2:inProgress:3", "t1:completed:3"},
		},
		{
			name:       "followed_without_resume_cursor_reads_to_the_log_end",
			isFollowed: true,
			historyEnd: 4,
			wantItems:  []string{"t1:user:u1", "t1:command:c1:completed@/old", "t1:agent:a1", "t2:user:u2", "t2:command:c2:completed@/new", "t2:agent:a2"},
			wantTurns:  []string{"t2:inProgress:3", "t1:completed:3"},
		},
		{
			name:       "followed_from_an_empty_log_without_cursor_reads_to_the_log_end",
			isFollowed: true,
			wantItems:  []string{"t1:user:u1", "t1:command:c1:completed@/old", "t1:agent:a1", "t2:user:u2", "t2:command:c2:completed@/new", "t2:agent:a2"},
			wantTurns:  []string{"t2:inProgress:3", "t1:completed:3"},
		},
		{
			name:       "followed_stops_at_the_position_resume_delivered",
			isFollowed: true,
			historyEnd: 4,
			cursor:     &resumeCursor,
			wantItems:  []string{"t1:user:u1", "t1:command:c1:completed@/old", "t1:agent:a1"},
			wantTurns:  []string{"t2:inProgress:0", "t1:completed:3"},
		},
		{
			// 位置之前保存、还在执行的工具调用照常给出（inProgress），它的结果之后走实时推送。
			name:       "followed_keeps_the_tool_call_still_running_at_the_position",
			isFollowed: true,
			historyEnd: 6,
			cursor:     &resumeCursor,
			wantItems:  []string{"t1:user:u1", "t1:command:c1:completed@/old", "t1:agent:a1", "t2:user:u2", "t2:command:c2:inProgress@/new"},
			wantTurns:  []string{"t2:inProgress:2", "t1:completed:3"},
		},
		{
			name:       "followed_at_the_log_end_reads_everything",
			isFollowed: true,
			historyEnd: 8,
			cursor:     &resumeCursor,
			wantItems:  []string{"t1:user:u1", "t1:command:c1:completed@/old", "t1:agent:a1", "t2:user:u2", "t2:command:c2:completed@/new", "t2:agent:a2"},
			wantTurns:  []string{"t2:inProgress:3", "t1:completed:3"},
		},
		{
			// 开始跟随时日志是空的：之后保存的都走实时推送。
			name:       "followed_from_an_empty_log_reads_nothing",
			isFollowed: true,
			cursor:     &resumeCursor,
			wantItems:  []string{},
			wantTurns:  []string{"t2:inProgress:0", "t1:completed:0"},
		},
		{
			// Thread 已经结束时，照没有跟随处理。
			name:       "followed_but_already_stopped_reads_to_the_log_end",
			isFollowed: true,
			isFinished: true,
			historyEnd: 4,
			cursor:     &resumeCursor,
			wantItems:  []string{"t1:user:u1", "t1:command:c1:completed@/old", "t1:agent:a1", "t2:user:u2", "t2:command:c2:completed@/new", "t2:agent:a2"},
			wantTurns:  []string{"t2:inProgress:3", "t1:completed:3"},
		},
	}
	// 2. 分别读取 Item 页和 Turn 页：跟随中且带起点游标时截到历史终点，其他情况读到日志末尾；终点前还在执行的工具显示 inProgress。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newHistoryConversationClient(log)
			c := newHistoryConnection(client)
			if tt.isFollowed {
				thread := &connectionThread{
					threadID:          string(historyConversationID),
					historyEndOrdinal: tt.historyEnd,
					requests:          make(chan connectionThreadRequest),
					done:              make(chan struct{}),
				}
				c.loadedThreads[thread.threadID] = thread
				if tt.isFinished {
					close(thread.done)
				} else {
					// 像 connectionThread.Run 一样在自己的任务里串行处理连接交来的请求。
					go func() {
						for request := range thread.requests {
							thread.handleRequest(t.Context(), request)
						}
					}()
					t.Cleanup(func() { close(thread.requests) })
				}
			}
			items, err := c.listThreadItems(t.Context(), codexThreadItemsListParams{
				ThreadID: string(historyConversationID),
				Cursor:   tt.cursor,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := summarizeEntries(items.(codexThreadItemsListResult).Data); !slices.Equal(got, tt.wantItems) {
				t.Fatalf("listThreadItems = %v, want %v", got, tt.wantItems)
			}
			turns, err := c.listThreadTurns(t.Context(), codexThreadTurnsListParams{
				ThreadID: string(historyConversationID),
				Cursor:   tt.cursor,
			})
			if err != nil {
				t.Fatal(err)
			}
			gotTurns := []string{}
			for _, turn := range turns.(codexThreadTurnsListResult).Data {
				gotTurns = append(gotTurns, fmt.Sprintf("%s:%s:%d", turn.ID, turn.Status, len(turn.Items)))
			}
			if !slices.Equal(gotTurns, tt.wantTurns) {
				t.Fatalf("listThreadTurns = %v, want %v", gotTurns, tt.wantTurns)
			}
		})
	}
}

// TestListThreads 检查根 Thread 排序、归档、目录、标题筛选和分页。
func TestListThreads(t *testing.T) {
	// 1. 准备多个目录、标题和归档对话，设置本机与远端路径映射。
	const (
		conversationA defs.ConversationID = "00000000-0000-7000-8000-0000000000d1"
		conversationB defs.ConversationID = "00000000-0000-7000-8000-0000000000d2"
		conversationC defs.ConversationID = "00000000-0000-7000-8000-0000000000d3"
		conversationD defs.ConversationID = "00000000-0000-7000-8000-0000000000d4"
	)
	newClient := func() *fakeHistoryClient {
		newConversation := func(id defs.ConversationID, updatedAt int64, title, directory string) defs.Conversation {
			return defs.Conversation{
				ID:               id,
				RootSessionID:    defs.SessionID("root-" + id),
				Title:            title,
				UpdatedAt:        updatedAt,
				ExecutorSettings: defs.ExecutorSettings{WorkspaceDirectories: map[string]string{defs.LocalExecutorID: directory}},
			}
		}
		// d4 已归档，只出现在 archived 为 true 的列表里。
		archived := newConversation(conversationD, 4000, "Delta", "/w1")
		archived.ArchivedAt = 4500
		client := &fakeHistoryClient{
			pageSize: 2,
			conversations: []defs.Conversation{
				newConversation(conversationA, 1000, "Alpha", "/w1"), newConversation(conversationB, 2000, "beta report", "/w1"), newConversation(conversationC, 3000, "Gamma", "/w2"), archived,
			},
		}
		for _, record := range client.conversations {
			client.sessions = append(client.sessions, defs.Session{
				ID:             record.RootSessionID,
				ConversationID: record.ID,
				Model: defs.ModelSelection{Ref: defs.ModelRef{
					ProviderID: "local",
					ModelID:    "default",
				}},
			})
		}
		return client
	}
	getIDs := func(threads []codexThread) []string {
		ids := make([]string, 0, len(threads))
		for _, thread := range threads {
			ids = append(ids, thread.ID[len(thread.ID)-2:])
		}
		return ids
	}
	// remoteMappings 只映射 /w1：连的是远端 serve 时 /w2 换不了。
	remoteMappings := config.PathMappings{{
		Client: "/c/w1",
		Server: "/w1",
	}}
	tests := []struct {
		name   string
		params string
		// isRemoteServer 连接连的是远端 serve 吗？
		isRemoteServer bool
		// pathMappings 是连接的路径映射。
		pathMappings config.PathMappings
		want         []string
		// wantCwd 非空时，列出的每个 Thread 的 cwd 都是它。
		wantCwd    string
		wantCursor bool
		wantError  *codexError
	}{
		{
			name:   "newest_update_first",
			params: `{}`,
			want:   []string{"d3", "d2", "d1"},
		},
		{
			name:   "oldest_update_first",
			params: `{"sortDirection":"asc"}`,
			want:   []string{"d1", "d2", "d3"},
		},
		{
			name:   "one_cwd",
			params: `{"cwd":"/w1"}`,
			want:   []string{"d2", "d1"},
		},
		{
			name:   "several_cwds",
			params: `{"cwd":["/w2","/w1"]}`,
			want:   []string{"d3", "d2", "d1"},
		},
		{
			name:   "null_cwd_is_no_filter",
			params: `{"cwd":null}`,
			want:   []string{"d3", "d2", "d1"},
		},
		{
			name:   "search_term_is_case_sensitive",
			params: `{"searchTerm":"BETA"}`,
			want:   []string{},
		},
		{
			name:   "search_term_matches",
			params: `{"searchTerm":"beta"}`,
			want:   []string{"d2"},
		},
		{
			name:   "search_term_and_cwd",
			params: `{"searchTerm":"a","cwd":"/w2"}`,
			want:   []string{"d3"},
		},
		{
			name:           "mapped_cwd_on_remote_server",
			params:         `{"cwd":"/c/w1"}`,
			isRemoteServer: true,
			pathMappings:   remoteMappings,
			want:           []string{"d2", "d1"},
			wantCwd:        "/c/w1",
		},
		{
			name:           "unmapped_cwds_on_remote_server_are_dropped",
			params:         `{"cwd":["/w2","/c/w1"]}`,
			isRemoteServer: true,
			pathMappings:   remoteMappings,
			want:           []string{"d2", "d1"},
		},
		{
			name:           "no_mapped_cwd_on_remote_server_is_no_filter",
			params:         `{"cwd":"/w2"}`,
			isRemoteServer: true,
			pathMappings:   remoteMappings,
			want:           []string{"d3", "d2", "d1"},
		},
		{
			name:   "archived_lists_only_archived",
			params: `{"archived":true}`,
			want:   []string{"d4"},
		},
		{
			name:   "archived_with_cwd",
			params: `{"archived":true,"cwd":"/w2"}`,
			want:   []string{},
		},
		{
			name:       "not_archived_lists",
			params:     `{"archived":false,"limit":1}`,
			want:       []string{"d3"},
			wantCursor: true,
		},
		{
			name:   "sub_agent_kinds_without_a_filter_are_empty",
			params: `{"sourceKinds":["subAgentThreadSpawn"]}`,
			want:   []string{},
		},
		{
			name:   "invalid_cursor",
			params: `{"cursor":"%%%"}`,
			wantError: &codexError{
				errorCodeInvalidParams,
				"invalid cursor: %%%",
			},
		},
		{
			name:   "unknown_sort_direction",
			params: `{"sortDirection":"up"}`,
			wantError: &codexError{
				errorCodeInvalidParams,
				"invalid sortDirection: up",
			},
		},
	}
	// 2. 读取根 Thread 列表，核对筛选结果、路径、模型、状态和分页游标。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var params codexThreadListParams
			if err := decodeCodexParams(json.RawMessage(tt.params), &params); err != nil {
				t.Fatal(err)
			}
			client := newClient()
			c := newHistoryConnection(client)
			c.isLocalServer, c.pathMappings = !tt.isRemoteServer, tt.pathMappings
			result, err := c.listThreads(t.Context(), params)
			if tt.wantError != nil {
				var protocolErr *codexError
				if !errors.As(err, &protocolErr) || *protocolErr != *tt.wantError {
					t.Fatalf("listThreads error = %v, want %+v", err, *tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !isSubAgentSourceKinds(params.SourceKinds) && (len(client.conversationRequests) != 1 || client.sessionsReads != 0) {
				t.Fatalf("list requested %d conversation pages and %d session pages, want one conversation page only", len(client.conversationRequests), client.sessionsReads)
			}
			page := result.(codexThreadListResult)
			if got := getIDs(page.Data); !slices.Equal(got, tt.want) || (page.NextCursor != nil) != tt.wantCursor || page.BackwardsCursor != nil {
				t.Fatalf("listThreads = %v cursor %v, want %v with cursor %v", got, page.NextCursor, tt.want, tt.wantCursor)
			}
			for _, thread := range page.Data {
				if thread.Status.Type != "notLoaded" || thread.Model != "local/default" || thread.Cwd == "" || thread.Source != codexSessionSource(codexAppServerSource("appServer")) {
					t.Fatalf("thread %s = status %q model %q cwd %q source %v, want a notLoaded appServer thread from the stored records", thread.ID, thread.Status.Type, thread.Model, thread.Cwd, thread.Source)
				}
				if tt.wantCwd != "" && thread.Cwd != tt.wantCwd {
					t.Fatalf("thread %s cwd = %q, want the client path %q", thread.ID, thread.Cwd, tt.wantCwd)
				}
			}
		})
	}
	// 3. 每页 2 条连续翻页，每个 Thread 只出现一次。
	t.Run("paging_walks_every_thread_once", func(t *testing.T) {
		c := newHistoryConnection(newClient())
		var walked []string
		params := codexThreadListParams{Limit: new(int)}
		*params.Limit = 2
		for pages := 0; ; pages++ {
			result, err := c.listThreads(t.Context(), params)
			if err != nil {
				t.Fatal(err)
			}
			page := result.(codexThreadListResult)
			walked = append(walked, getIDs(page.Data)...)
			if page.NextCursor == nil {
				break
			}
			if pages > 3 {
				t.Fatalf("walk did not end: %v", walked)
			}
			params.Cursor = page.NextCursor
		}
		if !slices.Equal(walked, []string{"d3", "d2", "d1"}) {
			t.Fatalf("pages = %v, want every thread once, newest first", walked)
		}
	})
}

// TestListThreadModels 检查根 Thread 模型和默认模型查询次数。
func TestListThreadModels(t *testing.T) {
	// 1. 分别列出已指定模型和未指定模型的多根 Session，核对列表模型和共用查询次数。
	for _, test := range []struct {
		name           string
		isModelUnset   bool
		wantModel      string
		wantModelReads int
	}{
		{
			name:      "stored_root_model",
			wantModel: "local/default",
		},
		{
			name:           "unset_models_share_default_lookup",
			isModelUnset:   true,
			wantModel:      "local/fallback",
			wantModelReads: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := newHistoryConversationClient(nil)
			client.defaultModel = defs.ModelRef{
				ProviderID: "local",
				ModelID:    "fallback",
			}
			if test.isModelUnset {
				client.sessions[0].Model = defs.ModelSelection{}
			}
			second := client.conversations[0]
			second.ID, second.RootSessionID = "second", "second-root"
			client.conversations = append(client.conversations, second)
			client.sessions = append(client.sessions,
				defs.Session{
					ID:             second.RootSessionID,
					ConversationID: second.ID,
					Model:          client.sessions[0].Model,
				},
				defs.Session{
					ID:              historyChildOneID,
					ConversationID:  historyConversationID,
					ParentSessionID: historyRootSessionID,
					Model: defs.ModelSelection{Ref: defs.ModelRef{
						ProviderID: "local",
						ModelID:    "child",
					}},
				},
			)
			result, err := newHistoryConnection(client).listThreads(t.Context(), codexThreadListParams{})
			if err != nil {
				t.Fatal(err)
			}
			page := result.(codexThreadListResult)
			if len(page.Data) != 2 || len(client.conversationRequests) != 1 || client.sessionsReads != 0 || client.modelReads != test.wantModelReads {
				t.Fatalf("listed %d threads, calls: conversations=%d sessions=%d models=%d, want two roots, one list, no session lookup and %d model lookups",
					len(page.Data), len(client.conversationRequests), client.sessionsReads, client.modelReads, test.wantModelReads)
			}
			for _, thread := range page.Data {
				if thread.Model != test.wantModel {
					t.Fatalf("thread model = %s, want %s", thread.Model, test.wantModel)
				}
			}
		})
	}
}

// TestThreadTitleOutput 检查根 Thread 和子 Thread 的标题、预览截到 256 个字符，存储的标题不变。
func TestThreadTitleOutput(t *testing.T) {
	// 1. 准备空、短、超长和含表情的 Unicode 标题。
	tests := []struct {
		name  string
		title string
		want  string
	}{
		{
			name:  "empty",
			title: "",
			want:  "",
		},
		{
			name:  "short",
			title: "修复登录",
			want:  "修复登录",
		},
		{
			name:  "at_limit",
			title: strings.Repeat("中", 256),
			want:  strings.Repeat("中", 256),
		},
		{
			name:  "long_ascii",
			title: strings.Repeat("a", 10000),
			want:  strings.Repeat("a", 256),
		},
		{
			name:  "long_chinese",
			title: strings.Repeat("标题", 200),
			want:  strings.Repeat("标题", 128),
		},
		{
			name:  "emoji_at_boundary",
			title: strings.Repeat("中", 255) + "😀末尾",
			want:  strings.Repeat("中", 255) + "😀",
		},
	}
	// 2. 读取根 Thread、子 Thread 和它们的列表，核对标题、预览和存储的标题。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newHistoryConversationClient(nil)
			client.conversations[0].Title = tt.title
			client.sessions = append(client.sessions, defs.Session{
				ID:              historyChildOneID,
				ConversationID:  historyConversationID,
				ParentSessionID: historyRootSessionID,
				AgentName:       "researcher",
				TaskName:        "调研",
			})
			c := newHistoryConnection(client)
			rootRead, err := c.readThread(t.Context(), codexThreadReadParams{ThreadID: string(historyConversationID)})
			if err != nil {
				t.Fatal(err)
			}
			childRead, err := c.readThread(t.Context(), codexThreadReadParams{ThreadID: string(historyChildOneID)})
			if err != nil {
				t.Fatal(err)
			}
			rootList, err := c.listThreads(t.Context(), codexThreadListParams{})
			if err != nil {
				t.Fatal(err)
			}
			parentThreadID := string(historyConversationID)
			childList, err := c.listThreads(t.Context(), codexThreadListParams{
				SourceKinds:    []string{"subAgentThreadSpawn"},
				ParentThreadID: &parentThreadID,
			})
			if err != nil {
				t.Fatal(err)
			}
			threads := []codexThread{rootRead.(codexThreadReadResult).Thread, childRead.(codexThreadReadResult).Thread}
			threads = append(threads, rootList.(codexThreadListResult).Data...)
			threads = append(threads, childList.(codexThreadListResult).Data...)
			if len(threads) != 4 {
				t.Fatalf("read/list returned %d threads, want 4", len(threads))
			}
			for _, thread := range threads {
				if thread.Preview != tt.want || (thread.Name == nil) != (tt.want == "") || (thread.Name != nil && *thread.Name != tt.want) {
					t.Fatalf("thread %s title = %v, preview = %q; want %q", thread.ID, thread.Name, thread.Preview, tt.want)
				}
			}
			if client.conversations[0].Title != tt.title {
				t.Fatal("reading thread titles changed the stored title")
			}
		})
	}
}

// TestListSubAgentThreads 检查子 Agent 的层级筛选和分页。
func TestListSubAgentThreads(t *testing.T) {
	// 1. 准备根、两个子 Session 和一个孙 Session，列出祖先与父 Thread 请求。
	newClient := func() *fakeHistoryClient {
		client := newHistoryConversationClient(nil)
		client.sessions = []defs.Session{
			{
				ID:             historyRootSessionID,
				ConversationID: historyConversationID,
				AgentName:      "assistant",
			},
			{
				ID:              historyChildOneID,
				ConversationID:  historyConversationID,
				AgentName:       "researcher",
				TaskName:        "调研",
				ParentSessionID: historyRootSessionID,
				Model: defs.ModelSelection{Ref: defs.ModelRef{
					ProviderID: "local",
					ModelID:    "second",
				}},
			},
			{
				ID:              historyChildTwoID,
				ConversationID:  historyConversationID,
				AgentName:       "coder",
				TaskName:        "实现",
				ParentSessionID: historyRootSessionID,
			},
			{
				ID:              historyGrandchildID,
				ConversationID:  historyConversationID,
				AgentName:       "coder",
				TaskName:        "子任务",
				ParentSessionID: historyChildOneID,
			},
		}
		return client
	}
	root := `"` + string(historyConversationID) + `"`
	tests := []struct {
		name       string
		params     string
		want       []string
		wantCursor string
		wantError  *codexError
	}{
		{
			name:   "ancestor_lists_every_descendant_newest_first",
			params: `{"sourceKinds":["subAgentThreadSpawn"],"ancestorThreadId":` + root + `}`,
			want: []string{
				string(historyGrandchildID) + ":子任务:" + string(historyChildOneID) + ":2",
				string(historyChildTwoID) + ":实现:" + string(historyConversationID) + ":1",
				string(historyChildOneID) + ":调研:" + string(historyConversationID) + ":1",
			},
		},
		{
			name:   "parent_lists_direct_children_only",
			params: `{"sourceKinds":["subAgent"],"parentThreadId":` + root + `,"sortDirection":"asc"}`,
			want: []string{
				string(historyChildOneID) + ":调研:" + string(historyConversationID) + ":1",
				string(historyChildTwoID) + ":实现:" + string(historyConversationID) + ":1",
			},
		},
		{
			name:       "paging_continues_after_the_last_session",
			params:     `{"sourceKinds":["subAgentThreadSpawn"],"ancestorThreadId":` + root + `,"limit":1,"cursor":"` + string(historyGrandchildID) + `"}`,
			want:       []string{string(historyChildTwoID) + ":实现:" + string(historyConversationID) + ":1"},
			wantCursor: string(historyChildTwoID),
		},
		{
			name:   "cursor_after_the_oldest_is_empty",
			params: `{"sourceKinds":["subAgent"],"ancestorThreadId":` + root + `,"cursor":"` + string(historyChildOneID) + `"}`,
			want:   []string{},
		},
		{
			name:   "no_filter_is_empty",
			params: `{"sourceKinds":["subAgentThreadSpawn","appServer"]}`,
			want:   []string{},
		},
		{
			name:   "both_filters_conflict",
			params: `{"sourceKinds":["subAgent"],"ancestorThreadId":` + root + `,"parentThreadId":` + root + `}`,
			wantError: &codexError{
				errorCodeInvalidParams,
				"ancestorThreadId and parentThreadId are mutually exclusive",
			},
		},
		{
			// 本连接没打开过根 Thread：子 Thread 按 SessionID 去 serve 找到，列出它的子 Agent；没有子 Agent 时是空列表。
			name:   "parent_is_a_child_thread_found_on_serve",
			params: `{"sourceKinds":["subAgentThreadSpawn"],"parentThreadId":"` + string(historyChildOneID) + `"}`,
			want:   []string{string(historyGrandchildID) + ":子任务:" + string(historyChildOneID) + ":2"},
		},
		{
			name:   "leaf_child_thread_has_no_sub_agents",
			params: `{"sourceKinds":["subAgentThreadSpawn"],"parentThreadId":"` + string(historyChildTwoID) + `"}`,
			want:   []string{},
		},
		{
			name:   "unknown_ancestor",
			params: `{"sourceKinds":["subAgent"],"ancestorThreadId":"missing"}`,
			wantError: &codexError{
				errorCodeInvalidRequest,
				"thread not loaded: missing",
			},
		},
		{
			name:   "invalid_cursor",
			params: `{"sourceKinds":["subAgent"],"ancestorThreadId":` + root + `,"cursor":"abc"}`,
			wantError: &codexError{
				errorCodeInvalidParams,
				"invalid cursor: abc",
			},
		},
	}
	// 2. 读取子 Thread 列表，核对父 ID、层级、来源、状态和分页游标。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var params codexThreadListParams
			if err := decodeCodexParams(json.RawMessage(tt.params), &params); err != nil {
				t.Fatal(err)
			}
			result, err := newHistoryConnection(newClient()).listThreads(t.Context(), params)
			if tt.wantError != nil {
				var protocolErr *codexError
				if !errors.As(err, &protocolErr) || *protocolErr != *tt.wantError {
					t.Fatalf("listThreads error = %v, want %+v", err, *tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			page := result.(codexThreadListResult)
			got := make([]string, 0, len(page.Data))
			for _, thread := range page.Data {
				source, isSubAgent := thread.Source.(codexSubAgentSource)
				if !isSubAgent || thread.ParentThreadID == nil || thread.AgentNickname == nil || thread.CanAcceptDirectInput || thread.SessionID != thread.ID || thread.Cwd != "/new" || thread.Status.Type != "notLoaded" {
					t.Fatalf("thread %+v, want a notLoaded sub-agent thread that takes no direct input in /new", thread)
				}
				spawn := source.SubAgent.ThreadSpawn
				if spawn.ParentThreadID != *thread.ParentThreadID || spawn.AgentNickname != *thread.AgentNickname || spawn.AgentRole != *thread.AgentRole {
					t.Fatalf("thread %s source %+v disagrees with parentThreadId/agentNickname/agentRole", thread.ID, spawn)
				}
				got = append(got, fmt.Sprintf("%s:%s:%s:%d", thread.ID, spawn.AgentNickname, spawn.ParentThreadID, spawn.Depth))
			}
			var gotCursor string
			if page.NextCursor != nil {
				gotCursor = *page.NextCursor
			}
			if !slices.Equal(got, tt.want) || gotCursor != tt.wantCursor {
				t.Fatalf("listThreads = %v cursor %q, want %v cursor %q", got, gotCursor, tt.want, tt.wantCursor)
			}
		})
	}
	// 3. 祖先是本连接登记过的子 Thread 时，只列出它的后代。
	t.Run("ancestor_can_be_a_child_thread", func(t *testing.T) {
		c := newHistoryConnection(newClient())
		c.childThreadConversations[historyChildOneID] = historyConversationID
		ancestor := string(historyChildOneID)
		result, err := c.listThreads(t.Context(), codexThreadListParams{
			SourceKinds:      []string{"subAgentThreadSpawn"},
			AncestorThreadID: &ancestor,
		})
		if err != nil {
			t.Fatal(err)
		}
		page := result.(codexThreadListResult)
		if len(page.Data) != 1 || page.Data[0].ID != string(historyGrandchildID) {
			t.Fatalf("descendants of the child thread = %+v, want only the grandchild", page.Data)
		}
	})
}

// TestGetThreadStatus 检查已加载 Thread 和已知子 Session 的状态。
func TestGetThreadStatus(t *testing.T) {
	// 1. 准备未加载、空闲、运行、systemError 和停止场景。
	childID := defs.SessionID("child-1")
	tests := []struct {
		name       string
		statusType string
		isLoaded   bool
		isFinished bool
		// threadID 是要问的 Thread，为空时是被跟随的那个 Thread 自己（thread-1）。
		threadID string
		// childInfo 非 nil 时，threadID 是 thread-1 所在对话里的子 Session，这是它的运行信息。
		childInfo *runtime.SessionRuntimeInfo
		want      codexThreadStatus
	}{
		{
			name: "not_followed",
			want: codexThreadStatus{Type: "notLoaded"},
		},
		{
			name:       "followed_idle",
			isLoaded:   true,
			statusType: codexThreadStatusIdle,
			want:       codexThreadStatus{Type: "idle"},
		},
		{
			name:       "followed_running_turn",
			isLoaded:   true,
			statusType: codexThreadStatusActive,
			want: codexThreadStatus{
				Type:        "active",
				ActiveFlags: []string{},
			},
		},
		{
			name:       "followed_after_rejected_resync",
			isLoaded:   true,
			statusType: codexThreadStatusSystemError,
			want:       codexThreadStatus{Type: "systemError"},
		},
		{
			name:       "followed_but_already_stopped",
			isLoaded:   true,
			isFinished: true,
			statusType: codexThreadStatusIdle,
			want:       codexThreadStatus{Type: "notLoaded"},
		},
		{
			name:       "unfollowed_child_of_a_followed_root_is_its_idle_session",
			isLoaded:   true,
			statusType: codexThreadStatusActive,
			threadID:   string(childID),
			childInfo:  &runtime.SessionRuntimeInfo{Session: defs.Session{ID: childID}},
			want:       codexThreadStatus{Type: "idle"},
		},
		{
			name:       "unfollowed_child_of_a_followed_root_is_its_running_session",
			isLoaded:   true,
			statusType: codexThreadStatusIdle,
			threadID:   string(childID),
			childInfo: &runtime.SessionRuntimeInfo{
				Session:      defs.Session{ID: childID},
				ActiveTurnID: "turn-9",
			},
			want: codexThreadStatus{
				Type:        "active",
				ActiveFlags: []string{},
			},
		},
		{
			name:      "child_of_an_unfollowed_root",
			threadID:  string(childID),
			childInfo: &runtime.SessionRuntimeInfo{Session: defs.Session{ID: childID}},
			want:      codexThreadStatus{Type: "notLoaded"},
		},
		{
			name:       "child_the_root_does_not_know",
			isLoaded:   true,
			statusType: codexThreadStatusIdle,
			threadID:   string(childID),
			want:       codexThreadStatus{Type: "notLoaded"},
		},
	}
	// 2. 按场景加载 Thread 和子 Session 状态，核对状态类型及 activeFlags。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newHistoryConnection(nil)
			thread := &connectionThread{
				threadID:             "thread-1",
				conversationID:       "thread-1",
				sessionID:            "root-1",
				threadStatusType:     tt.statusType,
				conversationSessions: map[defs.SessionID]runtime.SessionRuntimeInfo{},
				requests:             make(chan connectionThreadRequest),
				done:                 make(chan struct{}),
			}
			if tt.childInfo != nil {
				thread.conversationSessions[childID] = *tt.childInfo
			}
			if tt.threadID != "" {
				c.childThreadConversations[childID] = "thread-1"
			}
			if tt.isLoaded {
				c.loadedThreads["thread-1"] = thread
			}
			if tt.isFinished {
				close(thread.done)
			} else {
				// 像 connectionThread.Run 一样在自己的任务里串行处理连接交来的请求。
				go func() {
					for request := range thread.requests {
						thread.handleRequest(t.Context(), request)
					}
				}()
				t.Cleanup(func() { close(thread.requests) })
			}
			status := c.getThreadStatus(t.Context(), cmp.Or(tt.threadID, "thread-1"))
			if status.Type != tt.want.Type || !slices.Equal(status.ActiveFlags, tt.want.ActiveFlags) || (status.ActiveFlags == nil) != (tt.want.ActiveFlags == nil) {
				t.Fatalf("getThreadStatus = %+v, want %+v", status, tt.want)
			}
		})
	}
}

// TestGetHistoryThread 检查根 Thread 和子 Thread 的存储位置解析。
func TestGetHistoryThread(t *testing.T) {
	// 1. 准备已登记、未登记、未知 Thread 和查询失败场景。
	tests := []struct {
		name     string
		threadID string
		// isRegistered 子 Thread 是否已经登记在 childThreadConversations 里？本连接跟随根 Thread 时会登记。
		isRegistered bool
		// getSessionByIDErr 是按 SessionID 去 serve 找时的错误。
		getSessionByIDErr    error
		conversationRequests []runtimeclient.ListConversationsRequest
		modelReads           int
		wantSessionID        defs.SessionID
		// wantChildThread 读完之后 threadID 是不是登记在 childThreadConversations 里的子 Thread？
		wantChildThread bool
		wantError       *codexError
	}{
		{
			name:          "root_thread_reads_the_root_session",
			threadID:      string(historyConversationID),
			wantSessionID: historyRootSessionID,
		},
		{
			name:            "registered_child_thread_reads_its_own_session",
			threadID:        string(historyChildOneID),
			isRegistered:    true,
			wantSessionID:   historyChildOneID,
			wantChildThread: true,
		},
		{
			name:            "unregistered_child_thread_is_found_on_serve",
			threadID:        string(historyChildOneID),
			wantSessionID:   historyChildOneID,
			wantChildThread: true,
		},
		{
			name:     "root_session_id_is_not_a_thread",
			threadID: string(historyRootSessionID),
			wantError: &codexError{
				errorCodeInvalidRequest,
				"thread not loaded: " + string(historyRootSessionID),
			},
		},
		{
			name:     "unknown_thread",
			threadID: "missing",
			wantError: &codexError{
				errorCodeInvalidRequest,
				"thread not loaded: missing",
			},
		},
		{
			name:              "session_lookup_failure",
			threadID:          string(historyChildOneID),
			getSessionByIDErr: errors.New("storage offline"),
			wantError: &codexError{
				errorCodeInternal,
				"storage offline",
			},
		},
	}
	// 2. 解析 Thread，核对 Session、对话、子 Thread 登记及查询错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newHistoryConversationClient(nil)
			client.sessions = append(client.sessions, defs.Session{
				ID:              historyChildOneID,
				ConversationID:  historyConversationID,
				AgentName:       "researcher",
				ParentSessionID: historyRootSessionID,
			})
			client.getSessionByIDErr = tt.getSessionByIDErr
			c := newHistoryConnection(client)
			if tt.isRegistered {
				c.childThreadConversations[historyChildOneID] = historyConversationID
			}
			thread, err := c.getHistoryThread(t.Context(), tt.threadID)
			conversationID, isChildThread := c.childThreadConversations[defs.SessionID(tt.threadID)]
			if isChildThread != tt.wantChildThread || (isChildThread && conversationID != historyConversationID) {
				t.Fatalf("childThreadConversations[%s] = %q, %v, want registered %v", tt.threadID, conversationID, isChildThread, tt.wantChildThread)
			}
			if tt.wantError != nil {
				var protocolErr *codexError
				if !errors.As(err, &protocolErr) || *protocolErr != *tt.wantError {
					t.Fatalf("getHistoryThread error = %v, want %+v", err, *tt.wantError)
				}
				return
			}
			if err != nil || thread.sessionID != tt.wantSessionID || thread.conversation.ID != historyConversationID {
				t.Fatalf("getHistoryThread = %+v, %v, want the conversation with session %s", thread, err, tt.wantSessionID)
			}
		})
	}
}

// TestReadThread 检查 Thread 读取、完整 Turn 和默认模型。
func TestReadThread(t *testing.T) {
	// 1. 准备两轮日志及是否包含 Turn、是否指定模型的场景。
	log := []*defs.Item{newHistoryMessage("u1", "t1"), newHistoryReply("a1", "t1"), newHistoryMessage("u2", "t2"), newHistoryCall("c2", "t2", "call-2")}
	thread := `"threadId":"` + string(historyConversationID) + `"`
	tests := []struct {
		name   string
		params string
		// isModelUnset 根 Session 记录里是不是没有所选模型（旧记录的零值）？没有时按 Agent 默认模型 local/second 处理。
		isModelUnset bool
		wantTurns    []string
		wantModel    string
		wantEffort   string
	}{
		{
			name:       "without_turns",
			params:     `{` + thread + `}`,
			wantModel:  "local/default",
			wantEffort: "high",
		},
		{
			name:       "with_turns_reads_every_item_up_to_now",
			params:     `{` + thread + `,"includeTurns":true}`,
			wantTurns:  []string{"t1:completed:full:2", "t2:inProgress:full:2"},
			wantModel:  "local/default",
			wantEffort: "high",
		},
		{
			name:         "unset_model_is_the_agent_default",
			params:       `{` + thread + `}`,
			isModelUnset: true,
			wantModel:    "local/second",
		},
	}
	// 2. 读取 Thread，核对模型、强度、目录、完整 Turn 和 Session 查询次数。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newHistoryConversationClient(log)
			client.defaultModel = defs.ModelRef{
				ProviderID: "local",
				ModelID:    "second",
			}
			if tt.isModelUnset {
				client.sessions[0].Model = defs.ModelSelection{}
			}
			c := newHistoryConnection(client)
			var params codexThreadReadParams
			if err := decodeCodexParams(json.RawMessage(tt.params), &params); err != nil {
				t.Fatal(err)
			}
			result, err := c.readThread(t.Context(), params)
			if err != nil {
				t.Fatal(err)
			}
			read := result.(codexThreadReadResult).Thread
			got := make([]string, 0, len(read.Turns))
			for _, turn := range read.Turns {
				got = append(got, fmt.Sprintf("%s:%s:%s:%d", turn.ID, turn.Status, turn.ItemsView, len(turn.Items)))
			}
			var effort string
			if read.ReasoningEffort != nil {
				effort = *read.ReasoningEffort
			}
			if read.ID != params.ThreadID || read.SessionID != params.ThreadID || read.Status.Type != "notLoaded" || read.Name == nil || *read.Name != "History" ||
				read.Cwd != "/new" || read.Model != tt.wantModel || read.ModelProvider != "local" || effort != tt.wantEffort ||
				read.UpdatedAt != 5 || read.CLIVersion != "1.2.3" || !slices.Equal(got, tt.wantTurns) || read.Turns == nil {
				t.Fatalf("thread/read = %+v turns %v, want the stored root thread with model %s and turns %v", read, got, tt.wantModel, tt.wantTurns)
			}
			// 对话里的 Session 记录只读一次：Thread 对象和 turns 共用。
			if client.sessionsReads != 1 {
				t.Fatalf("read the sessions %d times, want once", client.sessionsReads)
			}
		})
	}
}

// TestReadThreadOfSubAgent 检查子 Thread 只显示分叉后的日志和自己的层级。
func TestReadThreadOfSubAgent(t *testing.T) {
	// 1. 创建父历史、分叉标记、子回复和子 Session 记录。
	newChildReply := func(id string, turnID defs.TurnID) *defs.Item {
		item := newTestReply(id, &defs.Address{
			Kind: defs.IdentityAgent,
			ID:   string(historyChildOneID),
		}, newTestText(id))
		item.TurnID = turnID
		return item
	}
	// 子 Session 的日志开头是从父 Session 复制来的历史（TurnID 是父的 t0），session_fork 之后才是它自己的。
	childLog := []*defs.Item{newHistoryMessage("p1", "t0"), newHistoryReply("p2", "t0"), newHistoryFork(), newChildReply("k1", "t1"), newChildReply("k2", "t1")}
	client := newHistoryConversationClient(nil)
	client.sessions = append(client.sessions, defs.Session{
		ID:              historyChildOneID,
		ConversationID:  historyConversationID,
		AgentName:       "researcher",
		TaskName:        "调研",
		ParentSessionID: historyRootSessionID,
		Model: defs.ModelSelection{Ref: defs.ModelRef{
			ProviderID: "local",
			ModelID:    "second",
		}},
	})
	client.turns = []defs.Turn{{
		ID:        "t1",
		SessionID: historyChildOneID,
		Status:    defs.TurnStatusCompleted,
	}}
	client.logs = map[defs.SessionID][]*defs.Item{historyChildOneID: childLog}
	setHistoryTurnRanges(client)
	c := newHistoryConnection(client)
	c.childThreadConversations[historyChildOneID] = historyConversationID

	// 2. 读取子 Thread，核对来源、模型和分叉后的 Turn。
	result, err := c.readThread(t.Context(), codexThreadReadParams{
		ThreadID:     string(historyChildOneID),
		IncludeTurns: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	thread := result.(codexThreadReadResult).Thread
	source, isSubAgent := thread.Source.(codexSubAgentSource)
	spawn := source.SubAgent.ThreadSpawn
	if !isSubAgent || thread.ID != string(historyChildOneID) || thread.SessionID != thread.ID || thread.Status.Type != "notLoaded" || thread.CanAcceptDirectInput ||
		spawn.ParentThreadID != string(historyConversationID) || spawn.Depth != 1 || spawn.AgentNickname != "调研" || spawn.AgentRole != "researcher" || thread.Model != "local/second" {
		t.Fatalf("thread/read of a sub-agent = %+v, want its own sub-agent thread", thread)
	}
	if len(thread.Turns) != 1 || len(thread.Turns[0].Items) != 2 {
		t.Fatalf("turns = %+v, want its one turn with the two items after the session fork", thread.Turns)
	}
	// 3. 分别正倒序读取子 Thread Item，核对只包含子回复。
	page, err := c.listThreadItems(t.Context(), codexThreadItemsListParams{ThreadID: string(historyChildOneID)})
	if err != nil {
		t.Fatal(err)
	}
	if got := summarizeEntries(page.(codexThreadItemsListResult).Data); !slices.Equal(got, []string{"t1:agent:k1", "t1:agent:k2"}) {
		t.Fatalf("items of the sub-agent = %v, want only those after the session fork, oldest first by default", got)
	}
	descending := "desc"
	page, err = c.listThreadItems(t.Context(), codexThreadItemsListParams{
		ThreadID:      string(historyChildOneID),
		SortDirection: &descending,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := summarizeEntries(page.(codexThreadItemsListResult).Data); !slices.Equal(got, []string{"t1:agent:k2", "t1:agent:k1"}) {
		t.Fatalf("descending items of the sub-agent = %v, want only those after the session fork, newest first", got)
	}
}

// TestReadItemsByTurnFollowStart 检查按 Turn 读取时的跟随起点。
func TestReadItemsByTurnFollowStart(t *testing.T) {
	// 1. 准备零起点、部分历史和无起点场景。
	log := []*defs.Item{newHistoryMessage("m1", "t1"), newHistoryMessage("m2", "t1")}
	tests := []struct {
		name        string
		followStart *historyReadEnd
		want        string
		isLogRead   bool
	}{
		// 开始跟随时什么都没保存：不读日志。
		{
			name:        "empty_follow_start_skips_the_log",
			followStart: newFollowStart(0, ""),
			want:        "map[]",
		},
		{
			name:        "follow_start_keeps_what_was_saved_before_it",
			followStart: newFollowStart(1, ""),
			want:        "map[t1:[m1]]",
			isLogRead:   true,
		},
		{
			name:      "no_follow_start_reads_to_the_log_end",
			want:      "map[t1:[m1 m2]]",
			isLogRead: true,
		},
	}
	// 2. 按 Turn 分组读取，核对各轮 Item ID 和是否访问日志。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newHistoryConversationClient(log)
			itemsByTurn, err := newHistoryConnection(client).readItemsByTurn(t.Context(), historyConversationID, historyRootSessionID, client.turns, tt.followStart)
			got := make(map[defs.TurnID][]string)
			for turnID, items := range itemsByTurn {
				got[turnID] = getItemIDs(items)
			}
			if err != nil || fmt.Sprint(got) != tt.want || (client.itemsReads > 0) != tt.isLogRead {
				t.Fatalf("readItemsByTurn = %v, %v after %d log reads, want %s with log read %v", got, err, client.itemsReads, tt.want, tt.isLogRead)
			}
		})
	}
}

// TestGetItemsWithoutRunningToolCalls 检查只移除指定活动 Turn 的未完成调用。
func TestGetItemsWithoutRunningToolCalls(t *testing.T) {
	// 1. 准备跨轮调用、完整配对和中断调用。
	interrupted := newInterruptedTestCall("c-3", "call-3", "bash", `{"comm`)
	tests := []struct {
		name         string
		items        []*defs.Item
		activeTurnID defs.TurnID
		want         []string
	}{
		{
			name:         "running_call_of_the_active_turn_dropped",
			items:        []*defs.Item{newTestMessage("m-1", testUser), newTestCall("c-1", "call-1", "bash", `{"command":"sleep 9"}`)},
			activeTurnID: "turn-1",
			want:         []string{"m-1"},
		},
		{
			name:         "running_call_of_another_turn_kept",
			items:        []*defs.Item{newTestCall("c-1", "call-1", "bash", `{}`)},
			activeTurnID: "turn-2",
			want:         []string{"c-1"},
		},
		{
			name:  "no_active_turn_drops_nothing",
			items: []*defs.Item{newTestCall("c-1", "call-1", "bash", `{}`)},
			want:  []string{"c-1"},
		},
		{
			name:         "finished_pair_kept",
			items:        []*defs.Item{newTestCall("c-1", "call-1", "bash", `{}`), newTestResult("call-1", "local", `{}`, nil)},
			activeTurnID: "turn-1",
			want:         []string{"c-1", "result-call-1"},
		},
		{
			name:         "only_the_call_without_a_result_dropped",
			items:        []*defs.Item{newTestCall("c-1", "call-1", "bash", `{}`), newTestCall("c-2", "call-2", "bash", `{}`), newTestResult("call-1", "local", `{}`, nil)},
			activeTurnID: "turn-1",
			want:         []string{"c-1", "result-call-1"},
		},
		{
			name:         "interrupted_call_kept",
			items:        []*defs.Item{interrupted},
			activeTurnID: "turn-1",
			want:         []string{"c-3"},
		},
		{
			name:         "empty",
			activeTurnID: "turn-1",
			want:         []string{},
		},
	}
	// 2. 过滤活动 Turn 的调用，核对保留的 Item ID。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getItemIDs(getItemsWithoutRunningToolCalls(tt.items, tt.activeTurnID)); !slices.Equal(got, tt.want) {
				t.Fatalf("getItemsWithoutRunningToolCalls = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestReadThreadTurns 检查完整 Turn 历史的终点截取和未完成工具过滤。
func TestReadThreadTurns(t *testing.T) {
	// 1. 准备两轮执行中调用和不同活动 Turn 终点。
	log := []*defs.Item{
		newHistoryMessage("u1", "t1"), newHistoryCall("c1", "t1", "call-1"), newHistoryMessage("u2", "t2"), newHistoryCall("c2", "t2", "call-2"),
	}
	tests := []struct {
		name        string
		followStart *historyReadEnd
		want        []string
	}{
		{
			name: "to_the_log_end_keeps_every_running_call",
			want: []string{"t1:2", "t2:2"},
		},
		{
			name:        "new_follower_gets_no_running_call_of_the_active_turn",
			followStart: newFollowStart(4, "t2"),
			want:        []string{"t1:2", "t2:1"},
		},
		{
			name:        "crash_leftover_in_another_turn_is_kept",
			followStart: newFollowStart(4, "t3"),
			want:        []string{"t1:2", "t2:2"},
		},
		{
			name:        "cut_at_the_follow_start",
			followStart: newFollowStart(2, ""),
			want:        []string{"t1:2", "t2:0"},
		},
	}
	// 2. 读取并转换完整 Turn，核对终点内每轮的 Item 条数。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newHistoryConversationClient(log)
			sessions, err := newHistoryConnection(client).readSessions(t.Context(), historyConversationID)
			if err != nil {
				t.Fatal(err)
			}
			turns, err := newHistoryConnection(client).readThreadTurns(t.Context(), historyConversationID, historyRootSessionID, sessions, tt.followStart)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0, len(turns))
			for _, turn := range turns {
				got = append(got, fmt.Sprintf("%s:%d", turn.ID, len(turn.Items)))
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("readThreadTurns = %v, want %v", got, tt.want)
			}
		})
	}
}

// setHistoryTurnRanges 按测试日志为每个 Turn 填写 Item 范围。
func setHistoryTurnRanges(client *fakeHistoryClient) {
	// 1. 清空范围，找到属于这一轮的首条流水号，非运行轮次记录末条流水号。
	for index := range client.turns {
		record := &client.turns[index]
		record.ItemsRange = defs.TurnItemRange{}
		for offset, item := range client.logs[record.SessionID] {
			if item.TurnID != record.ID {
				continue
			}
			if record.ItemsRange.FirstOrdinal == 0 {
				record.ItemsRange.FirstOrdinal = uint64(offset + 1)
			}
			if record.Status != defs.TurnStatusRunning {
				record.ItemsRange.LastOrdinal = uint64(offset + 1)
			}
		}
	}
}

// TestTurnHistoryDeepPagesReadOnlyPageRange 检查历史深页只读取本页 Turn 范围。
func TestTurnHistoryDeepPagesReadOnlyPageRange(t *testing.T) {
	// 1. 遍历正序、倒序和服务端短页场景。
	for _, tt := range []struct {
		name     string
		order    storage.Direction
		pageSize int
	}{
		{
			"ascending",
			storage.DirectionAsc,
			0,
		}, {
			"descending",
			storage.DirectionDesc,
			0,
		},
		{
			"short_pages",
			storage.DirectionDesc,
			7,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// 1. 为当前方向和页大小写入 90 轮，每轮四条日志。
			client := newHistoryConversationClient(nil)
			client.pageSize, client.turns = tt.pageSize, nil
			for index := range 90 {
				id := defs.TurnID(fmt.Sprintf("00000000-0000-7000-8000-%012d", index+1))
				client.turns = append(client.turns, defs.Turn{
					ID:        id,
					SessionID: historyRootSessionID,
					Status:    defs.TurnStatusCompleted,
					ItemsRange: defs.TurnItemRange{
						FirstOrdinal: uint64(index*4 + 1),
						LastOrdinal:  uint64(index*4 + 4),
					},
				})
				for offset := range 4 {
					client.logs[historyRootSessionID] = append(client.logs[historyRootSessionID], newHistoryMessage(fmt.Sprintf("%d-%d", index, offset), id))
				}
			}
			// 2. 设置每页 25 轮，连续读取三页并清零每页查询统计。
			connection := newHistoryConnection(client)
			limit, direction, view := 25, string(tt.order), codexTurnItemsFull
			params := codexThreadTurnsListParams{
				ThreadID:      string(historyConversationID),
				Limit:         &limit,
				SortDirection: &direction,
				ItemsView:     &view,
			}
			for pageIndex := range 3 {
				client.itemsReads, client.itemsReturned, client.itemsRequests = 0, 0, nil
				value, err := connection.listThreadTurns(t.Context(), params)
				if err != nil {
					t.Fatal(err)
				}
				page := value.(codexThreadTurnsListResult)
				// 3. 核对本页仅返回 100 条原始 Item，请求范围不超过本页，且每轮四条。
				wantRequests := 1
				if tt.pageSize > 0 {
					wantRequests = (100 + tt.pageSize - 1) / tt.pageSize
				}
				if len(page.Data) != 25 || client.itemsReturned != 100 || client.itemsReads != wantRequests {
					t.Fatalf("page %d: turns=%d raw=%d requests=%d, want 25/100/%d", pageIndex, len(page.Data), client.itemsReturned, client.itemsReads, wantRequests)
				}
				for _, request := range client.itemsRequests {
					if request.Order != storage.DirectionAsc || request.Limit > 100 {
						t.Fatalf("unbounded range request %+v", request)
					}
				}
				for _, turn := range page.Data {
					if len(turn.Items) != 4 {
						t.Fatalf("turn %s: %d items", turn.ID, len(turn.Items))
					}
				}
				params.Cursor = page.NextCursor
			}
		})
	}
}

// TestSpecifiedTurnItemRange 检查指定 Turn 范围的分页和终点限制。
func TestSpecifiedTurnItemRange(t *testing.T) {
	// 1. 遍历方向、游标、运行中范围、空范围和历史终点，核对 Item 或参数错误。
	for _, tt := range []struct {
		name        string
		order       storage.Direction
		cursor      storage.Ordinal
		open, empty bool
		end         *historyReadEnd
		want        []string
		invalid     bool
	}{
		{
			name:  "ascending",
			order: storage.DirectionAsc,
			want:  []string{"m3", "m4"},
		},
		{
			name:  "descending_at_tail",
			order: storage.DirectionDesc,
			want:  []string{"m4", "m3"},
		},
		{
			name:    "ascending_invalid_cursor",
			order:   storage.DirectionAsc,
			cursor:  99,
			invalid: true,
		},
		{
			name:    "descending_invalid_cursor",
			order:   storage.DirectionDesc,
			cursor:  99,
			invalid: true,
		},
		{
			name:  "open",
			order: storage.DirectionDesc,
			open:  true,
			want:  []string{"m4", "m3"},
		},
		{
			name:  "cutoff",
			order: storage.DirectionDesc,
			open:  true,
			end:   newFollowStart(3, ""),
			want:  []string{"m3"},
		},
		{
			name:  "zero_cutoff",
			order: storage.DirectionAsc,
			end:   newFollowStart(0, ""),
		},
		{
			name:  "empty",
			order: storage.DirectionAsc,
			empty: true,
		},
		{
			name:   "empty_ascending_valid_cursor",
			order:  storage.DirectionAsc,
			empty:  true,
			cursor: 4,
		},
		{
			name:   "empty_descending_valid_cursor",
			order:  storage.DirectionDesc,
			empty:  true,
			cursor: 4,
		},
		{
			name:    "empty_ascending_invalid_cursor",
			order:   storage.DirectionAsc,
			empty:   true,
			cursor:  99,
			invalid: true,
		},
		{
			name:    "empty_descending_invalid_cursor",
			order:   storage.DirectionDesc,
			empty:   true,
			cursor:  99,
			invalid: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// 1. 准备两轮日志和第二轮的范围，按场景设置运行中、空范围和游标。
			log := []*defs.Item{newHistoryMessage("m1", "t1"), newHistoryMessage("m2", "t1"), newHistoryMessage("m3", "t2"), newHistoryMessage("m4", "t2")}
			client := &fakeHistoryClient{
				pageSize: 1,
				logs:     map[defs.SessionID][]*defs.Item{historyRootSessionID: log},
			}
			record := defs.Turn{
				ID: "t2",
				ItemsRange: defs.TurnItemRange{
					FirstOrdinal: 3,
					LastOrdinal:  4,
				},
			}
			if tt.open {
				record.ItemsRange.LastOrdinal = 0
			}
			if tt.empty {
				record.ItemsRange = defs.TurnItemRange{}
			}
			query := itemsPageQuery{
				order:  tt.order,
				cursor: tt.cursor,
				limit:  1,
				turn:   &record,
			}
			// 2. 沿页游标读取，非法游标核对 -32602，其余汇总 Item ID。
			var got []string
			for range 5 {
				page, err := newHistoryConnection(client).readItemsPage(t.Context(), historyConversationID, historyRootSessionID, query, tt.end)
				if tt.invalid {
					var protocolErr *codexError
					if !errors.As(err, &protocolErr) || protocolErr.Code != errorCodeInvalidParams {
						t.Fatalf("cursor error = %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				got = append(got, getItemIDs(page.items)...)
				if page.nextCursor == 0 {
					break
				}
				query.cursor = page.nextCursor
			}
			// 3. 核对只返回目标范围和终点内的内容。
			if !slices.Equal(got, tt.want) {
				t.Fatalf("items=%v want %v", got, tt.want)
			}
		})
	}
}

// rangeHistoryClient 让范围读取经过真实 DAO 和文件日志，并统计返回的原始 Item。
type rangeHistoryClient struct {
	runtimeclient.Client
	query              dao.Query
	requests, returned int
}

// ListItems 经真实 DAO 读取日志页并统计返回条数。
func (c *rangeHistoryClient) ListItems(ctx context.Context, request *runtimeclient.ListItemsRequest) (runtimeclient.ListItemsResult, error) {
	// 1. 累计请求次数，读取 DAO 日志页，将 Item 和游标转换为 Client 结果并累计返回条数。
	c.requests++
	page, err := c.query.ListItems(ctx, dao.ListItemsRequest{
		ConversationID: request.ConversationID,
		SessionID:      request.SessionID,
		Order:          request.Order,
		Cursor:         request.Cursor,
		Limit:          request.Limit,
	})
	result := runtimeclient.ListItemsResult{NextCursor: page.Next}
	for _, logged := range page.Items {
		result.Items = append(result.Items, runtimeprotocol.LoggedItem{
			Ordinal: logged.Ordinal,
			Item:    logged.Item,
		})
	}
	c.returned += len(result.Items)
	return result, err
}

// openHistoryRangeStorage 创建真实 SQLite 和文件日志的测试 DAO。
func openHistoryRangeStorage(t testing.TB) (dao.Query, dao.Mutation, storage.LogStorage) {
	t.Helper()
	// 1. 在临时目录打开记录库和日志，创建 DAO 并登记数据库关闭。
	root := t.TempDir()
	database, err := storagesqlite.OpenDatabase(storagesqlite.NewDatabaseOpt(filepath.Join(root, "records.db"), dao.RecordNamespaces()...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	logs, err := storagefile.NewLogStorage(storagefile.NewLogStorageOpt(filepath.Join(root, "logs")))
	if err != nil {
		t.Fatal(err)
	}
	query, mutation, err := dao.NewDAO(dao.NewDAOOpt(database.Record(), logs))
	if err != nil {
		t.Fatal(err)
	}
	return query, mutation, logs
}

// TestTurnRangeAcrossForkSegments 检查分叉和多次分叉的 Turn 范围读取。
func TestTurnRangeAcrossForkSegments(t *testing.T) {
	// 1. 遍历单次分叉、多次分叉和来源缺失场景。
	for _, tt := range []struct {
		name          string
		missingSource bool
		forks         int
	}{
		{
			name:  "fork",
			forks: 1,
		}, {
			name:  "fork_of_fork",
			forks: 2,
		}, {
			name:          "missing_source_keeps_local_ordinals",
			forks:         2,
			missingSource: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// 1. 写入来源日志，并按场景分叉一次或两次，核对逻辑流水号。
			query, mutation, logs := openHistoryRangeStorage(t)
			var original []*defs.Item
			for index := range 6 {
				id := defs.TurnID("t1")
				if index >= 4 {
					id = "t2"
				}
				original = append(original, newHistoryMessage(fmt.Sprintf("m%d", index+1), id))
			}
			if _, err := mutation.AppendItems(t.Context(), "source", "root", original...); err != nil {
				t.Fatal(err)
			}
			source := defs.ConversationID("source")
			for index := range tt.forks {
				target := defs.ConversationID(fmt.Sprintf("fork%d", index))
				batch := []*defs.Item{{
					ID:   defs.NewItemID(),
					Kind: defs.ItemSessionFork,
					SessionFork: &defs.SessionFork{
						SourceConversationID: source,
						SourceSessionID:      "root",
						SourceEndOrdinal:     3,
					},
				}}
				batch = append(batch, original[2:]...)
				saved, err := mutation.AppendItems(t.Context(), target, "root", batch...)
				if err != nil {
					t.Fatal(err)
				}
				if saved[1].Ordinal != 3 || saved[len(saved)-1].Ordinal != 6 {
					t.Fatalf("fork %d ordinals=%+v", index, saved)
				}
				source = target
			}
			// 2. 按场景删除来源文件，再经真实 DAO 读取两轮范围。
			if tt.missingSource {
				if err := logs.DeletePath(t.Context(), "source"); err != nil {
					t.Fatal(err)
				}
			}
			client := &rangeHistoryClient{query: query}
			records := []defs.Turn{{
				ID: "t1",
				ItemsRange: defs.TurnItemRange{
					FirstOrdinal: 1,
					LastOrdinal:  4,
				},
			}, {
				ID: "t2",
				ItemsRange: defs.TurnItemRange{
					FirstOrdinal: 5,
					LastOrdinal:  6,
				},
			}}
			got, err := newHistoryConnection(client).readItemsByTurn(t.Context(), source, "root", records, nil)
			if err != nil {
				t.Fatal(err)
			}
			// 3. 核对来源缺失时保留本地流水号，读取缺失来源范围不会返回范围外本地 Item。
			want := []string{"m1", "m2", "m3", "m4"}
			if tt.missingSource {
				want = want[2:]
			}
			if !slices.Equal(getItemIDs(got["t1"]), want) || !slices.Equal(getItemIDs(got["t2"]), []string{"m5", "m6"}) {
				t.Fatalf("fork items=%v/%v", getItemIDs(got["t1"]), getItemIDs(got["t2"]))
			}
			if tt.missingSource {
				got, err := newHistoryConnection(client).readItemsByTurn(t.Context(), source, "root", []defs.Turn{{
					ID: "t1",
					ItemsRange: defs.TurnItemRange{
						FirstOrdinal: 1,
						LastOrdinal:  2,
					},
				}}, nil)
				if err != nil || len(got) != 0 {
					t.Fatalf("missing source range leaked local items: %+v %v", got, err)
				}
			}
		})
	}
}

// BenchmarkTurnRangeDeepPage 测量预热后经真实 DAO 读取一页 Turn 历史。
func BenchmarkTurnRangeDeepPage(b *testing.B) {
	// 1. 分别测量 100、1000 和 10000 轮历史中的预热页读取。
	for _, depth := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("warm_depth_%d", depth), func(b *testing.B) {
			// 1. 按本次深度写入每轮四条的真实日志。
			query, mutation, _ := openHistoryRangeStorage(b)
			var items []*defs.Item
			for index := range depth {
				for offset := range 4 {
					items = append(items, newHistoryMessage(fmt.Sprintf("%d-%d", index, offset), defs.TurnID(fmt.Sprintf("t%05d", index))))
				}
			}
			if _, err := mutation.AppendItems(b.Context(), historyConversationID, historyRootSessionID, items...); err != nil {
				b.Fatal(err)
			}
			client := &rangeHistoryClient{query: query}
			var records []defs.Turn
			for index := range 25 {
				records = append(records, defs.Turn{
					ID: defs.TurnID(fmt.Sprintf("t%05d", index)),
					ItemsRange: defs.TurnItemRange{
						FirstOrdinal: uint64(index*4 + 1),
						LastOrdinal:  uint64(index*4 + 4),
					},
				})
			}
			// 2. 先读取前 25 轮，预热文件行偏移。
			connection := newHistoryConnection(client)
			if _, err := connection.readItemsByTurn(b.Context(), historyConversationID, historyRootSessionID, records, nil); err != nil {
				b.Fatal(err)
			}
			// 3. 清零统计并开始计时，重复读取同一页。
			client.requests, client.returned = 0, 0
			b.ResetTimer()
			for range b.N {
				if _, err := connection.readItemsByTurn(b.Context(), historyConversationID, historyRootSessionID, records, nil); err != nil {
					b.Fatal(err)
				}
			}
			// 4. 停止计时，报告每页原始 Item 数和请求数。
			b.StopTimer()
			b.ReportMetric(float64(client.returned)/float64(b.N), "raw-items/page")
			b.ReportMetric(float64(client.requests)/float64(b.N), "requests/page")
		})
	}
}
