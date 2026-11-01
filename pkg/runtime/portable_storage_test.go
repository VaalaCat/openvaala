package runtime_test

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/storage"
)

// portableConversation 是通过公开查询读到的一个对话的完整持久内容。
type portableConversation struct {
	Conversation defs.Conversation
	Sessions     []defs.Session
	Turns        map[defs.SessionID][]defs.Turn
	Items        map[defs.SessionID][]dao.LoggedItem
}

// getPortableConversation 读出对话、全部 Session，以及每个 Session 的 Turn 和 Item。
func getPortableConversation(t *testing.T, store *runtimeStore, id defs.ConversationID) portableConversation {
	t.Helper()
	// 1. 读取对话记录。
	conversation, found, err := store.query.GetConversation(t.Context(), id)
	if err != nil || !found {
		t.Fatalf("GetConversation(%s) = %v, %v", id, found, err)
	}
	result := portableConversation{
		Conversation: conversation,
		Turns:        map[defs.SessionID][]defs.Turn{},
		Items:        map[defs.SessionID][]dao.LoggedItem{},
	}
	// 2. 收集全部 Session 及各自的 Turn 和 Item。
	for session, err := range store.query.ScanSessions(t.Context(), id, storage.Order{Direction: storage.DirectionAsc}) {
		if err != nil {
			t.Fatal(err)
		}
		result.Sessions = append(result.Sessions, session)
		turns, err := store.query.ListTurns(t.Context(), dao.ListTurnsRequest{
			SessionID: session.ID,
			Limit:     100,
		})
		if err != nil {
			t.Fatal(err)
		}
		result.Turns[session.ID] = turns.Items
		items, err := store.query.ListItems(t.Context(), dao.ListItemsRequest{
			ConversationID: id,
			SessionID:      session.ID,
			Order:          storage.DirectionAsc,
			Limit:          1000,
		})
		if err != nil {
			t.Fatal(err)
		}
		result.Items[session.ID] = items.Items
	}
	return result
}

// TestRuntimePortableConversation 把对话目录复制到另一份存储并重建索引后，持久内容和原来一致，打开后能接着聊。
func TestRuntimePortableConversation(t *testing.T) {
	// 1. 运行主任务和委派任务，保存完整对话。
	model := newRuntimeModel(t)
	source := openRuntimeStore(t)
	h := startTestRuntime(t, source, model)
	conv := h.create(t)
	rootWatcher := h.subscribeSession(t, conv.ID, conv.RootSessionID)
	h.submit(t, conv.ID, "委派", "")
	model.receive(t).reply <- runtimeModelResponse{calls: []llmkit.ToolCall{{
		ID:        "spawn",
		Name:      "spawn_agent",
		Arguments: `{"task_name":"review","message":"check"}`,
	}}}
	root, child := model.receiveRootAndTask(t, "review")
	childID := h.getChildSessionID(t, conv)
	childWatcher := h.subscribeSession(t, conv.ID, childID)
	root.reply <- runtimeModelResponse{chunks: []string{"已委派"}}
	collectUntil(t, rootWatcher.Events, isTurnFinished)
	child.reply <- runtimeModelResponse{chunks: []string{"子任务完成"}}
	collectUntil(t, childWatcher.Events, isTurnFinished)
	model.receive(t).reply <- runtimeModelResponse{chunks: []string{"已汇总"}}
	collectUntil(t, rootWatcher.Events, isTurnFinished)
	h.stop(t)
	want := getPortableConversation(t, source, conv.ID)
	if len(want.Sessions) != 2 {
		t.Fatalf("source Sessions = %d, want root and child", len(want.Sessions))
	}

	// 2. 复制对话目录，重建目标库索引。
	target := openRuntimeStore(t)
	from := filepath.Join(source.root, "conversations", string(conv.ID))
	to := filepath.Join(target.root, "conversations", string(conv.ID))
	if err := os.CopyFS(to, os.DirFS(from)); err != nil {
		t.Fatal(err)
	}
	result, err := dao.RebuildMetadataIndex(t.Context(), target.opt)
	if err != nil || result.RebuiltConversations != 1 || len(result.SkippedConversations) != 0 {
		t.Fatalf("RebuildMetadataIndex() = %+v, %v", result, err)
	}
	got := getPortableConversation(t, target, conv.ID)
	// 3. 比较持久内容，跳过根 Session 的索引时间。
	// 原索引时间来自暂存 metadata，重建后的时间来自第一条输入时重新追加的 metadata。
	for _, portable := range []*portableConversation{&got, &want} {
		for index := range portable.Sessions {
			if portable.Sessions[index].ID == conv.RootSessionID {
				portable.Sessions[index].UpdatedAt = 0
			}
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("copied conversation = %+v\nwant %+v", got, want)
	}

	// 4. 从目标库打开对话，检查续聊请求带有复制来的历史。
	resumed := startTestRuntime(t, target, model)
	if _, err := resumed.rt.OpenConversation(t.Context(), &runtime.OpenConversationRequest{ConversationID: conv.ID}); err != nil {
		t.Fatal(err)
	}
	watcher := resumed.subscribeSession(t, conv.ID, conv.RootSessionID)
	resumed.submit(t, conv.ID, "继续", "")
	request := model.receive(t)
	if history := fmt.Sprint(request.body["messages"]); !strings.Contains(history, "委派") || !strings.Contains(history, "继续") {
		t.Fatalf("resumed model request lacks copied history: %s", history)
	}
	request.reply <- runtimeModelResponse{chunks: []string{"好的"}}
	collectUntil(t, watcher.Events, isTurnFinished)
	rootItems := len(want.Items[conv.RootSessionID])
	resumed.waitLog(t, conv.ID, conv.RootSessionID, rootItems+2)
}
