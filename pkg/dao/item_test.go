package dao_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/storage"
)

// TestItemQueryAndMutationAppendAndList 检查 Item 追加后的双向分页。
func TestItemQueryAndMutationAppendAndList(t *testing.T) {
	// 1. 准备不同方向、游标和页大小样例。
	tests := []struct {
		name     string
		request  dao.ListItemsRequest
		wantIDs  []defs.ItemID
		wantOrd  []storage.Ordinal
		wantNext storage.Ordinal
	}{
		{
			name: "ascending_first_page_has_more",
			request: dao.ListItemsRequest{
				Order: storage.DirectionAsc,
				Limit: 2,
			},
			wantIDs:  []defs.ItemID{"1", "2"},
			wantOrd:  []storage.Ordinal{1, 2},
			wantNext: 2,
		},
		{
			name: "ascending_excludes_cursor",
			request: dao.ListItemsRequest{
				Order:  storage.DirectionAsc,
				Cursor: 2,
				Limit:  3,
			},
			wantIDs: []defs.ItemID{"3", "4", "5"},
			wantOrd: []storage.Ordinal{3, 4, 5},
		},
		{
			name: "descending_from_tail",
			request: dao.ListItemsRequest{
				Order: storage.DirectionDesc,
				Limit: 2,
			},
			wantIDs:  []defs.ItemID{"5", "4"},
			wantOrd:  []storage.Ordinal{5, 4},
			wantNext: 4,
		},
		{
			name: "descending_excludes_cursor",
			request: dao.ListItemsRequest{
				Order:  storage.DirectionDesc,
				Cursor: 4,
				Limit:  3,
			},
			wantIDs: []defs.ItemID{"3", "2", "1"},
			wantOrd: []storage.Ordinal{3, 2, 1},
		},
		{
			name: "exact_limit_has_no_next_page",
			request: dao.ListItemsRequest{
				Order: storage.DirectionAsc,
				Limit: 5,
			},
			wantIDs: []defs.ItemID{"1", "2", "3", "4", "5"},
			wantOrd: []storage.Ordinal{1, 2, 3, 4, 5},
		},
	}
	// 2. 分两批追加后检查 Item ID、序号和下一页位置。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query, mutation, _ := openDAO(t)
			conversationID := defs.NewConversationID()
			sessionID := defs.NewSessionID()
			first, err := mutation.AppendItems(t.Context(), conversationID, sessionID, &defs.Item{ID: "1"}, &defs.Item{ID: "2"})
			if err != nil || !slices.Equal(getLoggedOrdinals(first), []storage.Ordinal{1, 2}) {
				t.Fatalf("AppendItems(first batch) = %v, %v", first, err)
			}
			second, err := mutation.AppendItems(t.Context(), conversationID, sessionID, &defs.Item{ID: "3"}, &defs.Item{ID: "4"}, &defs.Item{ID: "5"})
			if err != nil || !slices.Equal(getLoggedOrdinals(second), []storage.Ordinal{3, 4, 5}) {
				t.Fatalf("AppendItems(second batch) = %v, %v", second, err)
			}

			tt.request.ConversationID, tt.request.SessionID = conversationID, sessionID
			page, err := query.ListItems(t.Context(), tt.request)
			if err != nil {
				t.Fatal(err)
			}
			gotIDs := make([]defs.ItemID, len(page.Items))
			gotOrd := make([]storage.Ordinal, len(page.Items))
			for index, logged := range page.Items {
				gotIDs[index], gotOrd[index] = logged.Item.ID, logged.Ordinal
			}
			if !slices.Equal(gotIDs, tt.wantIDs) || !slices.Equal(gotOrd, tt.wantOrd) || page.Next != tt.wantNext {
				t.Fatalf("ListItems() = IDs %v, Ordinals %v, Next %d; want IDs %v, Ordinals %v, Next %d",
					gotIDs, gotOrd, page.Next, tt.wantIDs, tt.wantOrd, tt.wantNext)
			}
		})
	}
}

// TestListItemsWithForkSource 检查分叉日志接续来源历史。
func TestListItemsWithForkSource(t *testing.T) {
	// 1. 准备正反序翻页、游标落在段内、段边界、来源被删、继续追加、再次分叉和子 Agent 日志的样例。
	prepareSourceDeleted := func(t *testing.T, history forkedItemHistory) itemLogLocation {
		if err := history.logs.DeletePath(t.Context(), storage.StreamPath(history.source.conversationID)); err != nil {
			t.Fatal(err)
		}
		return history.fork
	}
	// prepareForkOfFork 往 B 追加 b-71，再分叉出 C：C 第 1 行接续 B 到 50 之前，再原样复制 B 的 50–71。
	prepareForkOfFork := func(t *testing.T, history forkedItemHistory) itemLogLocation {
		appended := appendItemToFork(t, history)
		return writeForkedLog(t, history.mutation, history.fork, 50, append(slices.Clone(history.sourceItems[49:]), appended))
	}
	// prepareExactBoundary 另写一个接续 A 到 31 之前的分叉，自己正好 40 条（A 的 31–70）：每页 10 条时这一段正好读到第 2 行。
	prepareExactBoundary := func(t *testing.T, history forkedItemHistory) itemLogLocation {
		return writeForkedLog(t, history.mutation, history.source, 31, history.sourceItems[30:])
	}
	tests := []struct {
		name string
		// prepare 在基础数据之上补充写入或删除，返回要读的日志；为 nil 时读 B。
		prepare func(t *testing.T, history forkedItemHistory) itemLogLocation
		order   storage.Direction
		cursor  storage.Ordinal
		limit   int
		// isOnePage 是否只读一页？是时检查 wantNext；否则每页 limit 条一直读到 Next 为 0。
		isOnePage    bool
		wantNext     storage.Ordinal
		wantOrdinals []storage.Ordinal
		wantIDs      []defs.ItemID
		// segmentStarts 是各段第一条的序号：同一页的 Item 不能跨过其中任何一个。
		segmentStarts   []storage.Ordinal
		wantLastOrdinal storage.Ordinal
	}{
		{
			name:            "ascending_pages",
			order:           storage.DirectionAsc,
			limit:           10,
			wantOrdinals:    getOrdinalRange(1, 70),
			wantIDs:         getItemIDRange("a", 1, 70),
			segmentStarts:   []storage.Ordinal{30},
			wantLastOrdinal: 70,
		},
		{
			name:            "descending_pages",
			order:           storage.DirectionDesc,
			limit:           10,
			wantOrdinals:    getOrdinalRange(70, 1),
			wantIDs:         getItemIDRange("a", 70, 1),
			segmentStarts:   []storage.Ordinal{30},
			wantLastOrdinal: 70,
		},
		{
			name:            "ascending_cursor_in_source_stops_at_segment_end",
			order:           storage.DirectionAsc,
			cursor:          25,
			limit:           10,
			isOnePage:       true,
			wantNext:        29,
			wantOrdinals:    getOrdinalRange(26, 29),
			wantIDs:         getItemIDRange("a", 26, 29),
			wantLastOrdinal: 70,
		},
		{
			name:            "descending_cursor_in_fork_stops_at_segment_end",
			order:           storage.DirectionDesc,
			cursor:          35,
			limit:           10,
			isOnePage:       true,
			wantNext:        30,
			wantOrdinals:    getOrdinalRange(34, 30),
			wantIDs:         getItemIDRange("a", 34, 30),
			wantLastOrdinal: 70,
		},
		{
			name:            "descending_large_first_page_reads_only_fork",
			order:           storage.DirectionDesc,
			limit:           256,
			isOnePage:       true,
			wantNext:        30,
			wantOrdinals:    getOrdinalRange(70, 30),
			wantIDs:         getItemIDRange("a", 70, 30),
			wantLastOrdinal: 70,
		},
		{
			name:            "exact_boundary_descending",
			prepare:         prepareExactBoundary,
			order:           storage.DirectionDesc,
			limit:           10,
			wantOrdinals:    getOrdinalRange(70, 1),
			wantIDs:         getItemIDRange("a", 70, 1),
			segmentStarts:   []storage.Ordinal{31},
			wantLastOrdinal: 70,
		},
		{
			name:            "exact_boundary_ascending",
			prepare:         prepareExactBoundary,
			order:           storage.DirectionAsc,
			limit:           10,
			wantOrdinals:    getOrdinalRange(1, 70),
			wantIDs:         getItemIDRange("a", 1, 70),
			segmentStarts:   []storage.Ordinal{31},
			wantLastOrdinal: 70,
		},
		{
			name: "append_continues_ordinals",
			prepare: func(t *testing.T, history forkedItemHistory) itemLogLocation {
				appendItemToFork(t, history)
				return history.fork
			},
			order:           storage.DirectionAsc,
			limit:           10,
			wantOrdinals:    getOrdinalRange(1, 71),
			wantIDs:         append(getItemIDRange("a", 1, 70), "b-71"),
			segmentStarts:   []storage.Ordinal{30},
			wantLastOrdinal: 71,
		},
		{
			name:            "source_deleted_ascending",
			prepare:         prepareSourceDeleted,
			order:           storage.DirectionAsc,
			limit:           10,
			wantOrdinals:    getOrdinalRange(30, 70),
			wantIDs:         getItemIDRange("a", 30, 70),
			wantLastOrdinal: 70,
		},
		{
			name:            "source_deleted_descending",
			prepare:         prepareSourceDeleted,
			order:           storage.DirectionDesc,
			limit:           10,
			wantOrdinals:    getOrdinalRange(70, 30),
			wantIDs:         getItemIDRange("a", 70, 30),
			wantLastOrdinal: 70,
		},
		{
			name:            "fork_of_fork_ascending",
			prepare:         prepareForkOfFork,
			order:           storage.DirectionAsc,
			limit:           10,
			wantOrdinals:    getOrdinalRange(1, 71),
			wantIDs:         append(getItemIDRange("a", 1, 70), "b-71"),
			segmentStarts:   []storage.Ordinal{30, 50},
			wantLastOrdinal: 71,
		},
		{
			name:            "fork_of_fork_descending",
			prepare:         prepareForkOfFork,
			order:           storage.DirectionDesc,
			limit:           10,
			wantOrdinals:    getOrdinalRange(71, 1),
			wantIDs:         append([]defs.ItemID{"b-71"}, getItemIDRange("a", 70, 1)...),
			segmentStarts:   []storage.Ordinal{30, 50},
			wantLastOrdinal: 71,
		},
		{
			name: "sub_agent_log_without_marker",
			prepare: func(t *testing.T, history forkedItemHistory) itemLogLocation {
				agentLog := itemLogLocation{
					conversationID: history.source.conversationID,
					sessionID:      defs.NewSessionID(),
				}
				inherited := &defs.Item{
					ID:          "d-fork",
					Kind:        defs.ItemSessionFork,
					SessionFork: &defs.SessionFork{SourceSessionID: history.source.sessionID},
				}
				if _, err := history.mutation.AppendItems(t.Context(), agentLog.conversationID, agentLog.sessionID,
					&defs.Item{ID: "d-1"}, &defs.Item{ID: "d-2"}, inherited, &defs.Item{ID: "d-4"}); err != nil {
					t.Fatal(err)
				}
				return agentLog
			},
			order:           storage.DirectionAsc,
			limit:           3,
			wantOrdinals:    getOrdinalRange(1, 4),
			wantIDs:         []defs.ItemID{"d-1", "d-2", "d-fork", "d-4"},
			wantLastOrdinal: 4,
		},
	}
	// 2. 按页读取并检查每页不跨段、序号连续、Item ID 及最新序号。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			history := openForkedItemHistory(t)
			target := history.fork
			if tt.prepare != nil {
				target = tt.prepare(t, history)
			}
			var gotOrdinals []storage.Ordinal
			var gotIDs []defs.ItemID
			cursor := tt.cursor
			for pageCount := 0; ; pageCount++ {
				if pageCount == 100 {
					t.Fatalf("ListItems() kept returning Next after %d pages", pageCount)
				}
				page, err := history.query.ListItems(t.Context(), dao.ListItemsRequest{
					ConversationID: target.conversationID,
					SessionID:      target.sessionID,
					Order:          tt.order,
					Cursor:         cursor,
					Limit:          tt.limit,
				})
				if err != nil {
					t.Fatalf("ListItems(cursor %d) error = %v", cursor, err)
				}
				// 有下一页时，本页仍应返回 Item。
				if len(page.Items) == 0 && page.Next != 0 {
					t.Fatalf("ListItems(cursor %d) returned an empty page with Next %d", cursor, page.Next)
				}
				pageOrdinals := getLoggedOrdinals(page.Items)
				if isPageAcrossSegments(pageOrdinals, tt.segmentStarts) {
					t.Fatalf("ListItems(cursor %d) page %v mixes segments starting at %v", cursor, pageOrdinals, tt.segmentStarts)
				}
				gotOrdinals = append(gotOrdinals, pageOrdinals...)
				for _, logged := range page.Items {
					gotIDs = append(gotIDs, logged.Item.ID)
				}
				if tt.isOnePage {
					if page.Next != tt.wantNext {
						t.Fatalf("ListItems() Next = %d, want %d", page.Next, tt.wantNext)
					}
					break
				}
				if page.Next == 0 {
					break
				}
				cursor = page.Next
			}
			if !slices.Equal(gotOrdinals, tt.wantOrdinals) || !slices.Equal(gotIDs, tt.wantIDs) {
				t.Fatalf("ListItems() = Ordinals %v, IDs %v; want Ordinals %v, IDs %v", gotOrdinals, gotIDs, tt.wantOrdinals, tt.wantIDs)
			}
			last, err := dao.GetLastOrdinal(t.Context(), history.query, target.conversationID, target.sessionID)
			if err != nil || last != tt.wantLastOrdinal {
				t.Fatalf("GetLastOrdinal() = %d, %v; want %d", last, err, tt.wantLastOrdinal)
			}
		})
	}
}

// forkedItemHistory 是接续读取用例的基础数据：A 的根 Session 写 70 条 a-01…a-70；
// B 第 1 行接续 A 到 30 之前，再原样复制 A 的 30–70。
type forkedItemHistory struct {
	query       dao.Query
	mutation    dao.Mutation
	logs        storage.LogStorage
	sourceItems []*defs.Item
	source      itemLogLocation
	fork        itemLogLocation
}

// itemLogLocation 定位一个 Session 的 Item 日志。
type itemLogLocation struct {
	conversationID defs.ConversationID
	sessionID      defs.SessionID
}

// openForkedItemHistory 写入来源 A 及接续来源的分叉 B。
func openForkedItemHistory(t *testing.T) forkedItemHistory {
	t.Helper()
	// 1. 打开 DAO 并创建来源日志位置。
	records, logs := openStorage(t)
	query, mutation, err := dao.NewDAO(dao.NewDAOOpt(records, logs))
	if err != nil {
		t.Fatal(err)
	}
	history := forkedItemHistory{
		query:    query,
		mutation: mutation,
		logs:     logs,
		source: itemLogLocation{
			conversationID: defs.NewConversationID(),
			sessionID:      defs.NewSessionID(),
		},
	}
	// 2. 写入 A 的 70 条 Item。
	for _, id := range getItemIDRange("a", 1, 70) {
		history.sourceItems = append(history.sourceItems, &defs.Item{ID: id})
	}
	if _, err := mutation.AppendItems(t.Context(), history.source.conversationID, history.source.sessionID, history.sourceItems...); err != nil {
		t.Fatal(err)
	}
	// 3. B 首行接续 A 的前 29 条，复制第 30 至 70 条。
	history.fork = writeForkedLog(t, mutation, history.source, 30, history.sourceItems[29:])
	return history
}

// writeForkedLog 创建包含接续标记和复制 Item 的根 Session 日志。
func writeForkedLog(t *testing.T, mutation dao.Mutation, source itemLogLocation, end int64, copied []*defs.Item) itemLogLocation {
	t.Helper()
	// 1. 创建新日志位置及来源序号小于 end 的接续标记。
	fork := itemLogLocation{
		conversationID: defs.NewConversationID(),
		sessionID:      defs.NewSessionID(),
	}
	marker := &defs.Item{
		ID:   defs.NewItemID(),
		Kind: defs.ItemSessionFork,
		SessionFork: &defs.SessionFork{
			SourceSessionID:      source.sessionID,
			SourceConversationID: source.conversationID,
			SourceEndOrdinal:     end,
		},
	}
	// 2. 同批追加标记和复制内容，检查复制项序号从 end 开始。
	saved, err := mutation.AppendItems(t.Context(), fork.conversationID, fork.sessionID, append([]*defs.Item{marker}, copied...)...)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := getLoggedOrdinals(saved[1:]), getOrdinalRange(int(end), int(end)+len(copied)-1); !slices.Equal(got, want) {
		t.Fatalf("AppendItems(fork) copied Ordinals = %v, want %v", got, want)
	}
	return fork
}

// appendItemToFork 往分叉 B 追加一条 Item。
func appendItemToFork(t *testing.T, history forkedItemHistory) *defs.Item {
	t.Helper()
	// 1. 追加 b-71，检查返回序号继续为 71。
	item := &defs.Item{ID: "b-71"}
	saved, err := history.mutation.AppendItems(t.Context(), history.fork.conversationID, history.fork.sessionID, item)
	if err != nil || len(saved) != 1 || saved[0].Ordinal != 71 {
		t.Fatalf("AppendItems(B) = %+v, %v; want Ordinal 71", saved, err)
	}
	return item
}

// isPageAcrossSegments 这一页的序号跨过任一段的起点了吗？
func isPageAcrossSegments(ordinals []storage.Ordinal, segmentStarts []storage.Ordinal) bool {
	// 1. 空页返回 false，否则检查是否有段起点位于最小序号之后、最大序号以内。
	if len(ordinals) == 0 {
		return false
	}
	low, high := slices.Min(ordinals), slices.Max(ordinals)
	return slices.ContainsFunc(segmentStarts, func(start storage.Ordinal) bool { return low < start && start <= high })
}

// getOrdinalRange 返回两个端点均包含的连续序号。
func getOrdinalRange(from, to int) []storage.Ordinal {
	// 1. 按端点选择递增或递减步长，再依次收集序号。
	step := 1
	if from > to {
		step = -1
	}
	var ordinals []storage.Ordinal
	for value := from; value != to+step; value += step {
		ordinals = append(ordinals, storage.Ordinal(value))
	}
	return ordinals
}

// getItemIDRange 返回指定前缀和连续编号组成的 Item ID。
func getItemIDRange(prefix string, from, to int) []defs.ItemID {
	// 1. 按连续序号生成 prefix-01 格式的 ID，支持倒序。
	var ids []defs.ItemID
	for _, ordinal := range getOrdinalRange(from, to) {
		ids = append(ids, defs.ItemID(fmt.Sprintf("%s-%02d", prefix, ordinal)))
	}
	return ids
}

// TestItemLogsAreSeparatedByConversation 检查同一 Session 在不同对话下的日志。
func TestItemLogsAreSeparatedByConversation(t *testing.T) {
	// 1. 准备两个对话都写入或只有一个写入的样例。
	tests := []struct {
		name       string
		appendedIn []defs.ItemID // 依次写入对话 A、对话 B 的 Item ID，空串表示这个对话不写入
		wantInA    []defs.ItemID
		wantInB    []defs.ItemID
	}{
		{
			name:       "same_session_in_two_conversations",
			appendedIn: []defs.ItemID{"a1", "b1"},
			wantInA:    []defs.ItemID{"a1"},
			wantInB:    []defs.ItemID{"b1"},
		},
		{
			name:       "only_one_conversation_written",
			appendedIn: []defs.ItemID{"a1", ""},
			wantInA:    []defs.ItemID{"a1"},
			wantInB:    []defs.ItemID{},
		},
	}
	// 2. 检查各对话的 Item 分别返回且日志从序号 1 开始。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query, mutation, _ := openDAO(t)
			sessionID := defs.NewSessionID()
			conversations := []defs.ConversationID{defs.NewConversationID(), defs.NewConversationID()}
			for index, itemID := range tt.appendedIn {
				if itemID == "" {
					continue
				}
				if _, err := mutation.AppendItems(t.Context(), conversations[index], sessionID, &defs.Item{ID: itemID}); err != nil {
					t.Fatal(err)
				}
			}
			for index, want := range [][]defs.ItemID{tt.wantInA, tt.wantInB} {
				page, err := query.ListItems(t.Context(), dao.ListItemsRequest{
					ConversationID: conversations[index],
					SessionID:      sessionID,
					Order:          storage.DirectionAsc,
					Limit:          10,
				})
				if err != nil {
					t.Fatal(err)
				}
				got := make([]defs.ItemID, len(page.Items))
				for position, logged := range page.Items {
					got[position] = logged.Item.ID
					if logged.Ordinal != storage.Ordinal(position+1) {
						t.Fatalf("conversation %d Ordinals = %+v, want each log to start at 1", index, page.Items)
					}
				}
				if !slices.Equal(got, want) {
					t.Fatalf("conversation %d Items = %v, want %v", index, got, want)
				}
			}
		})
	}
}

// TestAppendItemsReturnsSavedCopies 检查 Item 追加返回带存储时间的副本。
func TestAppendItemsReturnsSavedCopies(t *testing.T) {
	// 1. 准备单条和批量 Item 及调用方原始时间。
	tests := []struct {
		name  string
		items []*defs.Item
	}{
		{
			name:  "single",
			items: []*defs.Item{{ID: "1"}},
		},
		{
			name: "batch_shares_time",
			items: []*defs.Item{{ID: "1"}, {
				ID:        "2",
				UpdatedAt: 42,
			}},
		},
	}
	// 2. 追加后检查副本独立、批内时间相同、原 Item 未改并可回读。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query, mutation, _ := openDAO(t)
			conversationID := defs.NewConversationID()
			sessionID := defs.NewSessionID()
			callerTimes := make([]int64, len(tt.items))
			for index, item := range tt.items {
				callerTimes[index] = item.UpdatedAt
			}
			before := time.Now().UnixMilli()
			saved, err := mutation.AppendItems(t.Context(), conversationID, sessionID, tt.items...)
			if err != nil || len(saved) != len(tt.items) {
				t.Fatalf("AppendItems() = %+v, %v", saved, err)
			}
			for index, logged := range saved {
				if logged.Item == tt.items[index] || logged.Item.ID != tt.items[index].ID || logged.Item.UpdatedAt < before || logged.Item.UpdatedAt != saved[0].Item.UpdatedAt {
					t.Fatalf("saved[%d] = %+v, want a copy with the storage time", index, logged.Item)
				}
			}
			for index, item := range tt.items {
				if item.UpdatedAt != callerTimes[index] {
					t.Fatalf("AppendItems() changed caller Item %d UpdatedAt to %d, want %d", index, item.UpdatedAt, callerTimes[index])
				}
			}
			page, err := query.ListItems(t.Context(), dao.ListItemsRequest{
				ConversationID: conversationID,
				SessionID:      sessionID,
				Limit:          10,
			})
			if err != nil || page.Items[0].Item.UpdatedAt != saved[0].Item.UpdatedAt {
				t.Fatalf("ListItems() = %+v, %v", page, err)
			}
		})
	}
}

// getLoggedOrdinals 提取保存结果的对外序号。
func getLoggedOrdinals(items []dao.LoggedItem) []storage.Ordinal {
	// 1. 按输入顺序收集 Ordinal。
	ordinals := make([]storage.Ordinal, len(items))
	for index, logged := range items {
		ordinals[index] = logged.Ordinal
	}
	return ordinals
}

// TestItemMutationRejectsInvalidBatch 检查无效 Item 批次不留下日志。
func TestItemMutationRejectsInvalidBatch(t *testing.T) {
	// 1. 准备空批次、nil、缺失 ID、无效分叉和无来源图片。
	tests := []struct {
		name  string
		items []*defs.Item
		// wantErr 非空时，错误文本必须包含它。
		wantErr string
	}{
		{name: "empty_batch"},
		{
			name:  "nil_item",
			items: []*defs.Item{nil},
		},
		{
			name:  "missing_id",
			items: []*defs.Item{{}},
		},
		{
			name: "invalid_item_after_valid",
			items: []*defs.Item{
				{ID: "valid"},
				{
					ID:          "fork",
					Kind:        defs.ItemSessionFork,
					SessionFork: &defs.SessionFork{},
				},
			},
		},
		{
			name: "tool_result_image_without_source",
			items: []*defs.Item{
				{
					ID:   "result",
					Kind: defs.ItemToolResult,
					ToolResult: &defs.ToolResult{
						CallID: "call-1",
						Images: []*defs.ImageContent{{DataURL: ""}},
					},
				},
			},
			wantErr: "tool result image source is missing",
		},
		{
			name: "tool_result_nil_image",
			items: []*defs.Item{
				{
					ID:   "result",
					Kind: defs.ItemToolResult,
					ToolResult: &defs.ToolResult{
						CallID: "call-1",
						Images: []*defs.ImageContent{nil},
					},
				},
			},
			wantErr: "tool result image source is missing",
		},
	}
	// 2. 检查追加错误及空日志。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query, mutation, _ := openDAO(t)
			conversationID := defs.NewConversationID()
			sessionID := defs.NewSessionID()
			_, err := mutation.AppendItems(t.Context(), conversationID, sessionID, tt.items...)
			if err == nil {
				t.Fatal("AppendItems() error = nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("AppendItems() error = %v, want it to contain %q", err, tt.wantErr)
			}
			page, err := query.ListItems(t.Context(), dao.ListItemsRequest{
				ConversationID: conversationID,
				SessionID:      sessionID,
				Order:          storage.DirectionAsc,
				Limit:          10,
			})
			if err != nil || len(page.Items) != 0 {
				t.Fatalf("rejected batch left Items %+v, %v", page.Items, err)
			}
		})
	}
}

// TestItemQueryRejectsInvalidRequest 检查 Item 列表请求校验。
func TestItemQueryRejectsInvalidRequest(t *testing.T) {
	// 1. 准备空日志及零页大小、未知方向和越过末尾的游标。
	query, _, _ := openDAO(t)
	conversationID := defs.NewConversationID()
	sessionID := defs.NewSessionID()
	empty, err := query.ListItems(t.Context(), dao.ListItemsRequest{
		ConversationID: conversationID,
		SessionID:      sessionID,
		Order:          storage.DirectionAsc,
		Limit:          1,
	})
	if err != nil || len(empty.Items) != 0 || empty.Next != 0 {
		t.Fatalf("empty ListItems() = %+v, %v", empty, err)
	}
	tests := []struct {
		name    string
		request dao.ListItemsRequest
	}{
		{
			name: "zero_limit",
			request: dao.ListItemsRequest{
				ConversationID: conversationID,
				SessionID:      sessionID,
				Order:          storage.DirectionAsc,
			},
		},
		{
			name: "unsupported_order",
			request: dao.ListItemsRequest{
				ConversationID: conversationID,
				SessionID:      sessionID,
				Order:          storage.Direction("unsupported"),
				Limit:          1,
			},
		},
		{
			name: "cursor_beyond_tail",
			request: dao.ListItemsRequest{
				ConversationID: conversationID,
				SessionID:      sessionID,
				Order:          storage.DirectionAsc,
				Cursor:         1,
				Limit:          1,
			},
		},
	}
	// 2. 检查各无效请求返回错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := query.ListItems(t.Context(), tt.request); err == nil {
				t.Fatal("ListItems() error = nil")
			}
		})
	}
}

// TestItemQueryScanItems 检查 Item 跨页迭代和提前停止。
func TestItemQueryScanItems(t *testing.T) {
	// 1. 准备 600 条 Item 及各方向和停止位置。
	const itemCount = 600
	tests := []struct {
		name          string
		order         storage.Direction
		stopAfter     int
		wantFirstID   defs.ItemID
		wantCount     int
		wantListCalls int64
	}{
		{
			name:          "ascending_all_pages",
			order:         storage.DirectionAsc,
			wantFirstID:   "item-0001",
			wantCount:     itemCount,
			wantListCalls: 3,
		},
		{
			name:          "descending_all_pages",
			order:         storage.DirectionDesc,
			wantFirstID:   "item-0600",
			wantCount:     itemCount,
			wantListCalls: 3,
		},
		{
			name:          "break_in_first_page",
			order:         storage.DirectionDesc,
			stopAfter:     10,
			wantFirstID:   "item-0600",
			wantCount:     10,
			wantListCalls: 1,
		},
		{
			name:          "break_at_first_page_end",
			order:         storage.DirectionAsc,
			stopAfter:     256,
			wantFirstID:   "item-0001",
			wantCount:     256,
			wantListCalls: 1,
		},
	}
	// 2. 检查返回数量、首项、顺序、去重及底层读取次数。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			records, rawLogs := openStorage(t)
			logs := &countingLogStorage{LogStorage: rawLogs}
			query, mutation, err := dao.NewDAO(dao.NewDAOOpt(records, logs))
			if err != nil {
				t.Fatal(err)
			}
			conversationID := defs.NewConversationID()
			sessionID := defs.NewSessionID()
			items := make([]*defs.Item, itemCount)
			for index := range items {
				items[index] = &defs.Item{ID: defs.ItemID(fmt.Sprintf("item-%04d", index+1))}
			}
			if _, err := mutation.AppendItems(t.Context(), conversationID, sessionID, items...); err != nil {
				t.Fatal(err)
			}
			logs.listCalls.Store(0) // 只数 Scan 翻页的读取，不算写入时找接续标记的那一次

			var gotIDs []defs.ItemID
			for item, err := range query.ScanItems(t.Context(), conversationID, sessionID, tt.order) {
				if err != nil {
					t.Fatal(err)
				}
				gotIDs = append(gotIDs, item.ID)
				if len(gotIDs) == tt.stopAfter {
					break
				}
			}
			if len(gotIDs) != tt.wantCount || gotIDs[0] != tt.wantFirstID || logs.listCalls.Load() != tt.wantListCalls {
				t.Fatalf("ScanItems() = %d Items starting %q with %d List calls, want %d starting %q with %d",
					len(gotIDs), gotIDs[0], logs.listCalls.Load(), tt.wantCount, tt.wantFirstID, tt.wantListCalls)
			}
			sorted := slices.IsSortedFunc(gotIDs, func(a, b defs.ItemID) int {
				if tt.order == storage.DirectionDesc {
					return strings.Compare(string(b), string(a))
				}
				return strings.Compare(string(a), string(b))
			})
			if !sorted || len(slices.Compact(slices.Clone(gotIDs))) != len(gotIDs) {
				t.Fatalf("ScanItems() IDs out of order or duplicated")
			}
		})
	}
}

// TestItemQueryRejectsDamagedStoredItem 检查损坏 Item 的读取错误。
func TestItemQueryRejectsDamagedStoredItem(t *testing.T) {
	// 1. 准备分叉和摘要缺少字段、缺少 ID 和 null 载荷。
	tests := []struct {
		name      string
		payload   string
		wantInErr string
	}{
		{
			name:      "fork_without_source",
			payload:   `{"id":"damaged-fork","kind":"session_fork","session_fork":{"source_session_id":""}}`,
			wantInErr: "damaged-fork",
		},
		{
			name:      "summary_without_boundary",
			payload:   `{"id":"damaged-summary","turn_id":"turn","model_call_id":"compact","kind":"context_summary","context_summary":{"text":"summary"}}`,
			wantInErr: "damaged-summary",
		},
		{
			name:    "missing_id",
			payload: `{"kind":"message"}`,
		},
		{
			name:    "null_item",
			payload: `null`,
		},
	}
	// 2. 绕过 DAO 写入，检查列表和迭代均返回错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			records, logs := openStorage(t)
			query, _, err := dao.NewDAO(dao.NewDAOOpt(records, logs))
			if err != nil {
				t.Fatal(err)
			}
			conversationID := defs.NewConversationID()
			sessionID := defs.NewSessionID()
			if _, err := logs.Append(t.Context(), storage.StreamPath(conversationID), storage.StreamID(sessionID), json.RawMessage(tt.payload)); err != nil {
				t.Fatal(err)
			}
			_, err = query.ListItems(t.Context(), dao.ListItemsRequest{
				ConversationID: conversationID,
				SessionID:      sessionID,
				Order:          storage.DirectionAsc,
				Limit:          10,
			})
			if err == nil || !strings.Contains(err.Error(), tt.wantInErr) {
				t.Fatalf("ListItems() error = %v, want damaged Item error containing %q", err, tt.wantInErr)
			}
			for _, err := range query.ScanItems(t.Context(), conversationID, sessionID, storage.DirectionAsc) {
				if err == nil {
					t.Fatal("ScanItems() accepted damaged Item")
				}
			}
		})
	}
}

// TestItemContextSummaryStorage 检查摘要 Item 保存与字段校验。
func TestItemContextSummaryStorage(t *testing.T) {
	// 1. 分别写入有效摘要、空白正文及缺少覆盖末项的摘要，检查错误或保存后的完整字段。
	for _, tt := range []struct {
		name     string
		text     string
		boundary defs.ItemID
		wantErr  bool
	}{
		{
			"valid",
			"summary",
			"B",
			false,
		}, {
			"blank",
			" \n",
			"B",
			true,
		}, {
			"no_boundary",
			"summary",
			"",
			true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			query, mutation, _ := openDAO(t)
			conversationID := defs.NewConversationID()
			sid := defs.NewSessionID()
			item := &defs.Item{
				ID:          "S",
				TurnID:      "turn",
				ModelCallID: "compact",
				Kind:        defs.ItemContextSummary,
				ContextSummary: &defs.ContextSummary{
					Text:                 tt.text,
					CoveredThroughItemID: tt.boundary,
				},
			}
			_, err := mutation.AppendItems(t.Context(), conversationID, sid, item)
			if (err != nil) != tt.wantErr {
				t.Fatalf("AppendItems = %v", err)
			}
			if tt.wantErr {
				return
			}
			page, err := query.ListItems(t.Context(), dao.ListItemsRequest{
				ConversationID: conversationID,
				SessionID:      sid,
				Order:          storage.DirectionAsc,
				Limit:          10,
			})
			if err != nil || len(page.Items) != 1 {
				t.Fatalf("ListItems = %+v, %v", page, err)
			}
			got := page.Items[0].Item
			if got.Kind != defs.ItemContextSummary || got.ContextSummary.Text != tt.text || got.ContextSummary.CoveredThroughItemID != tt.boundary {
				t.Fatalf("summary changed: %+v", got)
			}
		})
	}
}

// TestItemSessionForkStorage 检查继承分叉标记保存。
func TestItemSessionForkStorage(t *testing.T) {
	// 1. 分别写入带来源和空来源标记，检查空来源报错且有效来源回读不变。
	for _, source := range []defs.SessionID{"parent", ""} {
		t.Run(string(source), func(t *testing.T) {
			q, m, _ := openDAO(t)
			conversationID := defs.NewConversationID()
			sid := defs.NewSessionID()
			item := &defs.Item{
				ID:          defs.NewItemID(),
				Kind:        defs.ItemSessionFork,
				SessionFork: &defs.SessionFork{SourceSessionID: source},
			}
			_, err := m.AppendItems(t.Context(), conversationID, sid, item)
			if (err != nil) != (source == "") {
				t.Fatalf("append=%v", err)
			}
			if err != nil {
				return
			}
			page, err := q.ListItems(t.Context(), dao.ListItemsRequest{
				ConversationID: conversationID,
				SessionID:      sid,
				Order:          storage.DirectionAsc,
				Limit:          1,
			})
			if err != nil || page.Items[0].Item.SessionFork.SourceSessionID != source {
				t.Fatalf("page=%+v %v", page, err)
			}
		})
	}
}

// TestItemModelChangeStorage 检查模型切换记录保存。
func TestItemModelChangeStorage(t *testing.T) {
	// 1. 准备不同模型、缺少 Turn 和相同模型样例。
	refA, refB := defs.ModelRef{
		ProviderID: "p",
		ModelID:    "a",
	}, defs.ModelRef{
		ProviderID: "p",
		ModelID:    "b",
	}
	tests := []struct {
		name    string
		item    *defs.Item
		wantErr bool
	}{
		{
			name: "valid",
			item: &defs.Item{
				ID:     defs.NewItemID(),
				TurnID: "T",
				Kind:   defs.ItemModelChange,
				ModelChange: &defs.ModelChange{
					PreviousModel: refA,
					Model:         refB,
				},
			},
		},
		{
			name: "missing_turn",
			item: &defs.Item{
				ID:   defs.NewItemID(),
				Kind: defs.ItemModelChange,
				ModelChange: &defs.ModelChange{
					PreviousModel: refA,
					Model:         refB,
				},
			},
			wantErr: true,
		},
		{
			name: "same_models",
			item: &defs.Item{
				ID:     defs.NewItemID(),
				TurnID: "T",
				Kind:   defs.ItemModelChange,
				ModelChange: &defs.ModelChange{
					PreviousModel: refA,
					Model:         refA,
				},
			},
			wantErr: true,
		},
	}
	// 2. 检查保存校验及有效切换回读不变。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, m, _ := openDAO(t)
			conversationID := defs.NewConversationID()
			sid := defs.NewSessionID()
			_, err := m.AppendItems(t.Context(), conversationID, sid, tt.item)
			if (err != nil) != tt.wantErr {
				t.Fatalf("AppendItems() = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			page, err := q.ListItems(t.Context(), dao.ListItemsRequest{
				ConversationID: conversationID,
				SessionID:      sid,
				Order:          storage.DirectionAsc,
				Limit:          1,
			})
			if err != nil || *page.Items[0].Item.ModelChange != *tt.item.ModelChange {
				t.Fatalf("page = %+v, %v", page, err)
			}
		})
	}
}

// TestItemExecutorsChangeStorage 检查 Executor 切换记录保存。
func TestItemExecutorsChangeStorage(t *testing.T) {
	// 1. 准备前后列表、空新列表、身份缺失和列表相同样例。
	previous := []defs.TurnExecutor{{
		ID:                 "local",
		WorkspaceDirectory: "/a",
	}}
	current := []defs.TurnExecutor{{
		ID:                 "gpu",
		WorkspaceDirectory: "/data",
	}, {
		ID:                 "local",
		WorkspaceDirectory: "/a",
	}}
	tests := []struct {
		name    string
		item    *defs.Item
		wantErr bool
	}{
		{
			name: "valid",
			item: &defs.Item{
				ID:     defs.NewItemID(),
				TurnID: "T",
				Kind:   defs.ItemExecutorsChange,
				ExecutorsChange: &defs.ExecutorsChange{
					PreviousExecutors: previous,
					Executors:         current,
				},
			},
		},
		{
			name: "valid_with_empty_current",
			item: &defs.Item{
				ID:              defs.NewItemID(),
				TurnID:          "T",
				Kind:            defs.ItemExecutorsChange,
				ExecutorsChange: &defs.ExecutorsChange{PreviousExecutors: previous},
			},
		},
		{
			name: "empty_previous",
			item: &defs.Item{
				ID:              defs.NewItemID(),
				TurnID:          "T",
				Kind:            defs.ItemExecutorsChange,
				ExecutorsChange: &defs.ExecutorsChange{Executors: current},
			},
			wantErr: true,
		},
		{
			name: "same_executors",
			item: &defs.Item{
				ID:     defs.NewItemID(),
				TurnID: "T",
				Kind:   defs.ItemExecutorsChange,
				ExecutorsChange: &defs.ExecutorsChange{
					PreviousExecutors: previous,
					Executors:         previous,
				},
			},
			wantErr: true,
		},
		{
			name: "with_model_call",
			item: &defs.Item{
				ID:          defs.NewItemID(),
				TurnID:      "T",
				ModelCallID: "M",
				Kind:        defs.ItemExecutorsChange,
				ExecutorsChange: &defs.ExecutorsChange{
					PreviousExecutors: previous,
					Executors:         current,
				},
			},
			wantErr: true,
		},
		{
			name: "missing_turn",
			item: &defs.Item{
				ID:   defs.NewItemID(),
				Kind: defs.ItemExecutorsChange,
				ExecutorsChange: &defs.ExecutorsChange{
					PreviousExecutors: previous,
					Executors:         current,
				},
			},
			wantErr: true,
		},
	}
	// 2. 检查保存校验及有效前后列表回读不变。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, m, _ := openDAO(t)
			conversationID := defs.NewConversationID()
			sid := defs.NewSessionID()
			_, err := m.AppendItems(t.Context(), conversationID, sid, tt.item)
			if (err != nil) != tt.wantErr {
				t.Fatalf("AppendItems() = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			page, err := q.ListItems(t.Context(), dao.ListItemsRequest{
				ConversationID: conversationID,
				SessionID:      sid,
				Order:          storage.DirectionAsc,
				Limit:          1,
			})
			if err != nil {
				t.Fatal(err)
			}
			got, want := page.Items[0].Item.ExecutorsChange, tt.item.ExecutorsChange
			if !slices.Equal(got.PreviousExecutors, want.PreviousExecutors) || !slices.Equal(got.Executors, want.Executors) {
				t.Fatalf("stored change = %+v, want %+v", got, want)
			}
		})
	}
}

// countingLogStorage 包装真实日志存储，统计 List 调用次数。
type countingLogStorage struct {
	storage.LogStorage
	listCalls atomic.Int64
}

// List 记录日志分页读取次数。
func (logs *countingLogStorage) List(ctx context.Context, request storage.ListLogEntriesRequest) (storage.Page[storage.LogEntry, storage.Ordinal], error) {
	// 1. 递增计数后调用底层列表读取。
	logs.listCalls.Add(1)
	return logs.LogStorage.List(ctx, request)
}

// TestGetLastOrdinal 检查最新 Item 的序号。
func TestGetLastOrdinal(t *testing.T) {
	// 1. 准备空日志和追加三条的样例。
	tests := []struct {
		name  string
		items int
		want  storage.Ordinal
	}{
		{
			name:  "empty_log",
			items: 0,
			want:  0,
		},
		{
			name:  "after_appends",
			items: 3,
			want:  3,
		},
	}
	// 2. 检查最新序号为零或追加数量。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query, mutation, _ := openDAO(t)
			conversationID := defs.NewConversationID()
			sessionID := defs.NewSessionID()
			for index := range tt.items {
				if _, err := mutation.AppendItems(t.Context(), conversationID, sessionID, &defs.Item{ID: defs.ItemID(fmt.Sprint(index + 1))}); err != nil {
					t.Fatal(err)
				}
			}
			got, err := dao.GetLastOrdinal(t.Context(), query, conversationID, sessionID)
			if err != nil || got != tt.want {
				t.Fatalf("GetLastOrdinal() = %d, %v; want %d", got, err, tt.want)
			}
		})
	}
}

// TestInstructionItemsPersist 检查 Skill 和 AGENTS.md 指令保存。
func TestInstructionItemsPersist(t *testing.T) {
	// 1. 分别追加两种指令并回读，检查所属 Turn、指令种类和完整载荷。
	for _, item := range []*defs.Item{
		{
			ID:     "skill",
			TurnID: "turn",
			Kind:   defs.ItemSkillInstruction,
			SkillInstruction: &defs.SkillInstruction{
				InputItemID: "input",
				Name:        "review",
				Executor:    "local",
				Path:        "/skills/review/SKILL.md",
				Text:        "review code",
			},
		},
		{
			ID:     "agents",
			TurnID: "turn",
			Kind:   defs.ItemAgentsInstruction,
			AgentsInstruction: &defs.AgentsInstruction{
				Executor: "local",
				Path:     "/repo/AGENTS.md",
				Text:     "run tests",
			},
		},
	} {
		t.Run(string(item.Kind), func(t *testing.T) {
			query, mutation, _ := openDAO(t)
			conversationID, sessionID := defs.NewConversationID(), defs.NewSessionID()
			if _, err := mutation.AppendItems(t.Context(), conversationID, sessionID, item); err != nil {
				t.Fatal(err)
			}
			page, err := query.ListItems(t.Context(), dao.ListItemsRequest{
				ConversationID: conversationID,
				SessionID:      sessionID,
				Order:          storage.DirectionAsc,
				Limit:          10,
			})
			if err != nil || len(page.Items) != 1 {
				t.Fatalf("items = %+v, %v", page, err)
			}
			got := page.Items[0].Item
			if got.Kind != item.Kind || got.TurnID != item.TurnID {
				t.Fatalf("item = %+v", got)
			}
			if item.SkillInstruction != nil && *got.SkillInstruction != *item.SkillInstruction {
				t.Fatal("skill contents changed")
			}
			if item.AgentsInstruction != nil && *got.AgentsInstruction != *item.AgentsInstruction {
				t.Fatal("agents contents changed")
			}
		})
	}
}
