package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	vaalaruntime "github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/storage"
	storagefile "github.com/VaalaCat/openvaala/pkg/storage/file"
)

// TestConversationManagement 检查分叉、回退、归档和删除后的对话历史及索引。
func TestConversationManagement(t *testing.T) {
	// 1. 构建程序，逐项运行回退、子 Agent、归档和删除场景。
	binary := buildOpenVaala(t)
	for _, test := range []struct {
		name string
		run  func(t *testing.T, f *conversationManagementFixture)
	}{
		{
			name: "rewind",
			run: func(t *testing.T, f *conversationManagementFixture) {
				// 1. 来源对话运行四轮，第三轮之前压缩，在第四轮之前分叉。
				source := f.openNewConversation(t)
				f.runTurn(t, source, "第一轮", e2eModelReply{
					text:  "一",
					usage: true,
				})
				f.runTurn(t, source, "第二轮", e2eModelReply{
					text:  "二",
					usage: true,
				})
				f.compact(t, source, "前两轮的摘要")
				f.runTurn(t, source, "第三轮", e2eModelReply{
					text:  "三",
					usage: true,
				})
				sourceFourthRequest, fourth := f.runTurn(t, source, "第四轮", e2eModelReply{
					text:  "四",
					usage: true,
				})
				sourceID, sourceRootID := source.conversation.ID, source.conversation.RootSessionID
				sourceItems := f.listItems(t, sourceID, sourceRootID)

				forked, err := f.control.ForkConversation(t.Context(), &runtimeclient.ForkConversationRequest{
					ConversationID:   sourceID,
					BeforeTurnID:     fourth,
					ExecutorSettings: source.conversation.ExecutorSettings,
				})
				if err != nil {
					t.Fatal(err)
				}
				if forked.ID == sourceID || forked.ForkedFromID != sourceID {
					t.Fatalf("forked = %+v, want a new conversation forked from %s", forked, sourceID)
				}

				// 2. 从头读取新对话历史，比较第四轮之前的 Item 和流水号。
				fourthStart := slices.IndexFunc(sourceItems, func(logged runtimeprotocol.LoggedItem) bool { return logged.Item.TurnID == fourth })
				if fourthStart < 0 {
					t.Fatalf("source history has no item of the fourth turn %s", fourth)
				}
				wantItems := getLoggedItemKeys(sourceItems[:fourthStart])
				if got := getLoggedItemKeys(f.listItems(t, forked.ID, forked.RootSessionID)); !slices.Equal(got, wantItems) {
					t.Fatalf("forked history =\n%s\nwant the source history before the fourth turn\n%s", strings.Join(got, "\n"), strings.Join(wantItems, "\n"))
				}

				// 3. 检查新对话根 Session 的用量等于来源前三轮及压缩的总和。
				var wantUsage defs.TokenUsage
				for _, turn := range f.listTurns(t, sourceID, sourceRootID) {
					if turn.ID != fourth {
						wantUsage = wantUsage.Add(turn.Usage)
					}
				}
				if wantUsage == (defs.TokenUsage{}) {
					t.Fatal("source turns before the fourth turn report no usage")
				}
				forkedObserver := openRuntimeObserver(t, f.config, forked.ID)
				if got := getRootSessionUsage(t, forkedObserver); got != wantUsage {
					t.Fatalf("forked root session usage = %+v, want %+v", got, wantUsage)
				}

				// 4. 向新对话发送修改后的第四轮，检查模型请求中的摘要和未覆盖历史。
				forkedRequest, _ := f.runTurn(t, forkedObserver, "改过的第四轮", e2eModelReply{text: "新的四"})
				wantMessages := getConversationMessageTexts(sourceFourthRequest.body)
				wantMessages[len(wantMessages)-1] = modelMessageText{
					role: "user",
					text: "改过的第四轮",
				}
				gotMessages := getConversationMessageTexts(forkedRequest.body)
				if !slices.Equal(gotMessages, wantMessages) {
					t.Fatalf("forked model messages = %+v, want %+v", gotMessages, wantMessages)
				}
				if !slices.ContainsFunc(gotMessages, func(message modelMessageText) bool { return strings.Contains(message.text, "前两轮的摘要") }) {
					t.Fatalf("forked model messages = %+v, want the summary", gotMessages)
				}

				// 5. 检查来源对话历史保持不变。
				if got, want := getLoggedItemKeys(f.listItems(t, sourceID, sourceRootID)), getLoggedItemKeys(sourceItems); !slices.Equal(got, want) {
					t.Fatalf("source history after the fork =\n%s\nwant unchanged\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
				}
			},
		},
		{
			name: "sub_agent",
			run: func(t *testing.T, f *conversationManagementFixture) {
				// 1. 第二轮启动 review 子 Agent，根 Session 在第三轮接收子 Agent 的结果。
				source := f.openNewConversation(t)
				f.runTurn(t, source, "你好", e2eModelReply{text: "hi"})
				source.startTurn(t, "委派")
				f.model.next(t).reply <- e2eModelReply{
					tool:      "spawn_agent",
					arguments: `{"task_name":"review","message":"check"}`,
				}
				afterSpawn, review := f.model.nextRootAndTask(t, "review")
				afterSpawn.reply <- e2eModelReply{text: "已委派"}
				readTurnFinished(t, source, defs.TurnStatusCompleted)
				review.reply <- e2eModelReply{text: "子任务完成"}
				f.model.next(t).reply <- e2eModelReply{text: "收到子任务结果"}
				readTurnFinished(t, source, defs.TurnStatusCompleted)
				sourceID, sourceRootID := source.conversation.ID, source.conversation.RootSessionID
				f.waitTurnsCompleted(t, sourceID, sourceRootID, 3)

				// 2. 检查来源对话包含 review Session 和子 Agent 结果。
				sourceSessions := f.listSessions(t, sourceID)
				childIndex := slices.IndexFunc(sourceSessions, func(record defs.Session) bool { return record.TaskName == "review" })
				if len(sourceSessions) != 2 || childIndex < 0 {
					t.Fatalf("source sessions = %+v, want the root and the review sub-agent", sourceSessions)
				}
				childID := string(sourceSessions[childIndex].ID)
				sourceItems := f.listItems(t, sourceID, sourceRootID)
				childResultIndex := slices.IndexFunc(sourceItems, func(logged runtimeprotocol.LoggedItem) bool {
					message := logged.Item.Message
					return message != nil && message.From != nil && message.From.Kind == defs.IdentityAgent && message.From.ID == childID
				})
				if childResultIndex < 0 {
					t.Fatalf("source root history has no message from the sub-agent %s", childID)
				}

				// 3. 分叉全部根历史，检查只复制根 Session，并保留子 Agent 结果消息。
				forked, err := f.control.ForkConversation(t.Context(), &runtimeclient.ForkConversationRequest{
					ConversationID:   sourceID,
					ExecutorSettings: source.conversation.ExecutorSettings,
				})
				if err != nil {
					t.Fatal(err)
				}
				if sessions := f.listSessions(t, forked.ID); len(sessions) != 1 || sessions[0].ID != forked.RootSessionID {
					t.Fatalf("forked sessions = %+v, want only the root session %s", sessions, forked.RootSessionID)
				}
				forkedItems := getLoggedItemKeys(f.listItems(t, forked.ID, forked.RootSessionID))
				if want := getLoggedItemKeys(sourceItems); !slices.Equal(forkedItems, want) {
					t.Fatalf("forked history =\n%s\nwant the whole source root history\n%s", strings.Join(forkedItems, "\n"), strings.Join(want, "\n"))
				}
				if childResult := getLoggedItemKeys(sourceItems[childResultIndex : childResultIndex+1])[0]; !slices.Contains(forkedItems, childResult) {
					t.Fatalf("forked history = %v, want the sub-agent result %s", forkedItems, childResult)
				}
			},
		},
		{
			name: "archive",
			run: func(t *testing.T, f *conversationManagementFixture) {
				// 1. 创建两段对话，归档其中一段并检查两种列表。
				archived, kept := f.openNewConversation(t), f.openNewConversation(t)
				f.runTurn(t, archived, "归档前", e2eModelReply{text: "好"})
				f.runTurn(t, kept, "保留", e2eModelReply{text: "好"})
				record, err := f.control.SetConversationArchived(t.Context(), &runtimeclient.SetConversationArchivedRequest{
					ConversationID: archived.conversation.ID,
					Archived:       true,
				})
				if err != nil {
					t.Fatal(err)
				}
				if record.ID != archived.conversation.ID || record.ArchivedAt == 0 {
					t.Fatalf("archived = %+v, want a non-zero ArchivedAt", record)
				}
				f.requireListed(t, []defs.ConversationID{kept.conversation.ID}, []defs.ConversationID{archived.conversation.ID})

				// 2. 停止服务并重建索引，检查归档对话仍只在归档列表。
				archived.stop()
				kept.stop()
				f.stopServe(t)
				if output := f.reindex(t); output != "rebuilt 2 conversations\n" {
					t.Fatalf("reindex stdout = %q, want only the rebuilt count", output)
				}
				f.startServe(t)
				f.requireListed(t, []defs.ConversationID{kept.conversation.ID}, []defs.ConversationID{archived.conversation.ID})
				// 3. 打开归档对话，检查归档时间未变。
				opened, err := f.control.OpenConversation(t.Context(), &runtimeclient.OpenConversationRequest{ConversationID: archived.conversation.ID})
				if err != nil || opened.ArchivedAt != record.ArchivedAt {
					t.Fatalf("open archived = %+v, %v, want the archived record", opened, err)
				}
			},
		},
		{
			name: "delete",
			run: func(t *testing.T, f *conversationManagementFixture) {
				// 1. 创建待删除、删除中断和保留的三段对话。
				deleted, interrupted, kept := f.openNewConversation(t), f.openNewConversation(t), f.openNewConversation(t)
				f.runTurn(t, deleted, "要删的", e2eModelReply{text: "好"})
				f.runTurn(t, interrupted, "删到一半的", e2eModelReply{text: "好"})
				f.runTurn(t, kept, "保留", e2eModelReply{text: "好"})
				deletedID, interruptedID, keptID := deleted.conversation.ID, interrupted.conversation.ID, kept.conversation.ID

				// 2. 运行 Turn 时请求删除，检查返回 Session 忙碌。
				deleted.startTurn(t, "长任务")
				request := f.model.next(t)
				err := f.control.DeleteConversation(t.Context(), &runtimeclient.DeleteConversationRequest{ConversationID: deletedID})
				if !errors.Is(err, session.ErrSessionBusy) {
					t.Fatalf("delete while a turn is running = %v, want %v", err, session.ErrSessionBusy)
				}
				request.reply <- e2eModelReply{text: "好"}
				readTurnFinished(t, deleted, defs.TurnStatusCompleted)

				// 3. 空闲时删除对话，检查目录、查询结果和列表。
				callWhenIdle(t, func(ctx context.Context) error {
					return f.control.DeleteConversation(ctx, &runtimeclient.DeleteConversationRequest{ConversationID: deletedID})
				})
				f.requireDirectoryRemoved(t, deletedID)
				f.requireNotFound(t, deletedID)
				f.requireListed(t, []defs.ConversationID{interruptedID, keptID}, nil)

				// 4. 追加删除 WAL 后重建索引，检查残留目录被删且删除提示只输出一次。
				deleted.stop()
				interrupted.stop()
				kept.stop()
				f.stopServe(t)
				f.appendConversationDeleted(t, interruptedID)
				if output, want := f.reindex(t), fmt.Sprintf("deleted conversation %s\nrebuilt 1 conversations\n", interruptedID); output != want {
					t.Fatalf("reindex stdout = %q, want %q", output, want)
				}
				f.requireDirectoryRemoved(t, interruptedID)
				if output := f.reindex(t); output != "rebuilt 1 conversations\n" {
					t.Fatalf("second reindex stdout = %q, want only the rebuilt count", output)
				}
				f.startServe(t)
				f.requireNotFound(t, deletedID)
				f.requireNotFound(t, interruptedID)
				f.requireListed(t, []defs.ConversationID{keptID}, nil)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) { test.run(t, newConversationManagementFixture(t, binary)) })
	}
}

// conversationManagementFixture 是 TestConversationManagement 一个用例的环境：临时 HOME 里直接运行的 serve、假模型和不打开对话的控制 Client。
type conversationManagementFixture struct {
	binary, config string
	model          *e2eModel
	control        runtimeclient.Client
	stopControl    func()
}

// newConversationManagementFixture 创建临时配置、模型、服务和控制 Client。
func newConversationManagementFixture(t *testing.T, binary string) *conversationManagementFixture {
	t.Helper()
	// 1. 创建模型和配置，启动服务并连接控制 Client。
	model := newE2EModel(t)
	f := &conversationManagementFixture{
		binary: binary,
		config: writeE2EConfig(t, model.server.URL),
		model:  model,
	}
	startE2EServe(t, binary, f.config)
	// 清理时先断开控制 Client，再停止服务。
	stopServeOnCleanup(t, binary, f.config)
	f.connectControl(t)
	return f
}

// connectControl 连接控制 Client，并在后台持续读掉它的事件。
func (f *conversationManagementFixture) connectControl(t *testing.T) {
	t.Helper()
	// 1. 创建 Client，保存停止函数并持续读取事件。
	client, stopClient := newE2ERuntimeClient(t, f.config)
	f.control, f.stopControl = client, stopClient
	go func() {
		for range client.Events() {
		}
	}()
}

// stopServe 断开控制 Client 并停止服务。
func (f *conversationManagementFixture) stopServe(t *testing.T) {
	t.Helper()
	// 1. 先停止 Client，再等服务退出并释放数据目录锁。
	f.stopControl()
	stopServe(t, f.binary, f.config)
}

// startServe 重启服务并连接新的控制 Client。
func (f *conversationManagementFixture) startServe(t *testing.T) {
	t.Helper()
	// 1. 启动服务后重新建立控制连接。
	startE2EServe(t, f.binary, f.config)
	f.connectControl(t)
}

// reindex 运行索引重建并返回标准输出。
func (f *conversationManagementFixture) reindex(t *testing.T) string {
	t.Helper()
	// 1. 在临时 HOME 内运行 reindex，检查成功且标准错误为空。
	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, f.binary, "--config-file", f.config, "reindex")
	command.Env = getE2EEnvironment(filepath.Dir(f.config))
	var stderr strings.Builder
	command.Stderr = &stderr
	stdout, err := command.Output()
	if err != nil || stderr.Len() != 0 {
		t.Fatalf("reindex: %v\nstdout: %s\nstderr: %s", err, stdout, stderr.String())
	}
	return string(stdout)
}

// getConversationDirectory 返回临时数据目录中的对话路径。
func (f *conversationManagementFixture) getConversationDirectory(conversationID defs.ConversationID) string {
	// 1. 将配置所在目录、数据目录和对话 ID 拼成路径。
	return filepath.Join(filepath.Dir(f.config), ".local", "share", "openvaala", "conversations", string(conversationID))
}

// appendConversationDeleted 在对话 WAL 中追加删除记录。
func (f *conversationManagementFixture) appendConversationDeleted(t *testing.T, conversationID defs.ConversationID) {
	t.Helper()
	// 1. 打开对话日志，只在 metadata 流追加 conversation_deleted。
	logs, err := storagefile.NewLogStorage(storagefile.NewLogStorageOpt(filepath.Dir(f.getConversationDirectory(conversationID))))
	if err != nil {
		t.Fatal(err)
	}
	payload := json.RawMessage(`{"kind":"conversation_deleted","deleted_conversation_id":"` + string(conversationID) + `"}`)
	if _, err := logs.Append(t.Context(), storage.StreamPath(conversationID), dao.MetadataStreamID, payload); err != nil {
		t.Fatal(err)
	}
}

// requireDirectoryRemoved 检查对话目录已经删除。
func (f *conversationManagementFixture) requireDirectoryRemoved(t *testing.T, conversationID defs.ConversationID) {
	t.Helper()
	// 1. 读取目录状态，要求返回文件不存在。
	if _, err := os.Stat(f.getConversationDirectory(conversationID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("conversation directory of %s: %v, want it removed", conversationID, err)
	}
}

// requireNotFound 检查查询和打开对话都返回不存在。
func (f *conversationManagementFixture) requireNotFound(t *testing.T, conversationID defs.ConversationID) {
	t.Helper()
	// 1. 分别调用读取记录和打开对话，检查错误为对话不存在。
	if _, err := f.control.GetConversation(t.Context(), conversationID); !errors.Is(err, vaalaruntime.ErrConversationNotFound) {
		t.Fatalf("read %s = %v, want %v", conversationID, err, vaalaruntime.ErrConversationNotFound)
	}
	if _, err := f.control.OpenConversation(t.Context(), &runtimeclient.OpenConversationRequest{ConversationID: conversationID}); !errors.Is(err, vaalaruntime.ErrConversationNotFound) {
		t.Fatalf("open %s = %v, want %v", conversationID, err, vaalaruntime.ErrConversationNotFound)
	}
}

// requireListed 检查未归档和已归档列表中的对话与根 Session。
func (f *conversationManagementFixture) requireListed(t *testing.T, listed, archived []defs.ConversationID) {
	t.Helper()
	// 1. 分别查询两种列表，逐项检查根 Session 和按创建顺序排列的对话 ID。
	for _, want := range []struct {
		archived bool
		ids      []defs.ConversationID
	}{{
		archived: false,
		ids:      listed,
	}, {
		archived: true,
		ids:      archived,
	}} {
		page, err := f.control.ListConversations(t.Context(), &runtimeclient.ListConversationsRequest{
			Limit:    100,
			Archived: want.archived,
		})
		if err != nil {
			t.Fatal(err)
		}
		got := make([]defs.ConversationID, len(page.Conversations))
		for index, entry := range page.Conversations {
			got[index] = entry.Conversation.ID
			if entry.RootSession.ID != entry.Conversation.RootSessionID || entry.RootSession.ConversationID != entry.Conversation.ID || entry.RootSession.ParentSessionID != "" {
				t.Fatalf("listed entry = %+v, want its own root Session", entry)
			}
		}
		if !slices.Equal(got, want.ids) {
			t.Fatalf("conversations listed with archived=%v = %v, want %v", want.archived, got, want.ids)
		}
	}
}

// openNewConversation 新建对话并用独立 Client 跟随根 Session。
func (f *conversationManagementFixture) openNewConversation(t *testing.T) *runtimeObserver {
	t.Helper()
	// 1. 使用新的本地工作目录创建对话，再打开观察连接。
	record, err := f.control.CreateConversation(t.Context(), &runtimeclient.CreateConversationRequest{
		AgentName:        "assistant",
		ExecutorSettings: defs.ExecutorSettings{WorkspaceDirectories: map[string]string{defs.LocalExecutorID: newE2EWorkspace(t)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return openRuntimeObserver(t, f.config, record.ID)
}

// runTurn 提交输入并让模型回复，返回请求和完成的 Turn ID。
func (f *conversationManagementFixture) runTurn(t *testing.T, observer *runtimeObserver, text string, reply e2eModelReply) (e2eModelRequest, defs.TurnID) {
	t.Helper()
	// 1. 发送输入，交给假模型指定回复，再检查 Turn 正常完成。
	observer.startTurn(t, text)
	request := f.model.next(t)
	request.reply <- reply
	return request, readTurnFinished(t, observer, defs.TurnStatusCompleted)
}

// compact 执行一次手动压缩并等待完成。
func (f *conversationManagementFixture) compact(t *testing.T, observer *runtimeObserver, summary string) {
	t.Helper()
	// 1. 空闲时提交压缩请求，让模型返回摘要和用量，再等 Turn 完成。
	callWhenIdle(t, func(ctx context.Context) error {
		_, err := f.control.CompactContext(ctx, observer.conversation.ID)
		return err
	})
	f.model.next(t).reply <- e2eModelReply{
		text:  summary,
		usage: true,
	}
	readTurnFinished(t, observer, defs.TurnStatusCompleted)
}

// listItems 按正序读完 Session 的全部日志。
func (f *conversationManagementFixture) listItems(t *testing.T, conversationID defs.ConversationID, sessionID defs.SessionID) []runtimeprotocol.LoggedItem {
	t.Helper()
	// 1. 从头查询 Item，使用每页 NextCursor 续读到末尾。
	var items []runtimeprotocol.LoggedItem
	request := &runtimeclient.ListItemsRequest{
		ConversationID: conversationID,
		SessionID:      sessionID,
	}
	for {
		page, err := f.control.ListItems(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, page.Items...)
		if page.NextCursor == 0 {
			return items
		}
		request.Cursor = page.NextCursor
	}
}

// listTurns 按 Turn ID 正序读完 Session 的 Turn 记录。
func (f *conversationManagementFixture) listTurns(t *testing.T, conversationID defs.ConversationID, sessionID defs.SessionID) []defs.Turn {
	t.Helper()
	// 1. 从头查询 Turn，使用每页 NextCursor 续读到末尾。
	var turns []defs.Turn
	request := &runtimeclient.ListTurnsRequest{
		ConversationID: conversationID,
		SessionID:      sessionID,
	}
	for {
		page, err := f.control.ListTurns(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		turns = append(turns, page.Turns...)
		if page.NextCursor.ID == "" {
			return turns
		}
		request.Cursor = page.NextCursor
	}
}

// listSessions 列出对话的 Session 记录。
func (f *conversationManagementFixture) listSessions(t *testing.T, conversationID defs.ConversationID) []defs.Session {
	t.Helper()
	// 1. 查询至多一百条 Session，失败时结束测试。
	page, err := f.control.ListSessions(t.Context(), &runtimeclient.ListSessionsRequest{
		ConversationID: conversationID,
		Limit:          100,
	})
	if err != nil {
		t.Fatal(err)
	}
	return page.Sessions
}

// waitTurnsCompleted 等待指定数量的 Turn 记录全部完成。
func (f *conversationManagementFixture) waitTurnsCompleted(t *testing.T, conversationID defs.ConversationID, sessionID defs.SessionID, count int) {
	t.Helper()
	// 1. 反复读取 Turn 数量与状态，超时则结束测试。
	deadline := time.Now().Add(e2eTimeout)
	for {
		turns := f.listTurns(t, conversationID, sessionID)
		if len(turns) == count && !slices.ContainsFunc(turns, func(turn defs.Turn) bool { return turn.Status != defs.TurnStatusCompleted }) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("turns = %+v, want %d completed turns", turns, count)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// readTurnFinished 等待下一条 TurnFinished，检查状态并返回 Turn ID。
func readTurnFinished(t *testing.T, observer *runtimeObserver, wantStatus defs.TurnStatus) defs.TurnID {
	t.Helper()
	// 1. 持续读取事件，遇到 TurnFinished 后检查结束状态。
	for {
		finished, isFinished := observer.next(t).(*runtimeclient.TurnFinished)
		if !isFinished {
			continue
		}
		if finished.Status != wantStatus {
			t.Fatalf("turn %s finished as %q (%s), want %q", finished.TurnID, finished.Status, finished.Error, wantStatus)
		}
		return finished.TurnID
	}
}

// getRootSessionUsage 从首次同步事件读取根 Session 累计用量。
func getRootSessionUsage(t *testing.T, observer *runtimeObserver) defs.TokenUsage {
	t.Helper()
	// 1. 等待 ConversationSynced，再从运行信息中查找根 Session。
	for {
		synced, isSynced := observer.next(t).(*runtimeclient.ConversationSynced)
		if !isSynced {
			continue
		}
		for _, info := range synced.SessionInfos {
			if info.Session.ID == observer.conversation.RootSessionID {
				return info.Usage
			}
		}
		t.Fatalf("session infos = %+v, want the root session %s", synced.SessionInfos, observer.conversation.RootSessionID)
	}
}

// callWhenIdle 在 Session 忙碌时反复重试调用，直到成功。
func callWhenIdle(t *testing.T, call func(ctx context.Context) error) {
	t.Helper()
	// 1. 设置截止时间，只对 ErrSessionBusy 间隔重试；超时或遇到其他错误时结束测试。
	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()
	for {
		err := call(ctx)
		if err == nil {
			return
		}
		if !errors.Is(err, session.ErrSessionBusy) || ctx.Err() != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// getLoggedItemKeys 将日志条目转换为流水号和 Item ID。
func getLoggedItemKeys(items []runtimeprotocol.LoggedItem) []string {
	// 1. 按条目顺序拼接流水号和 ID。
	keys := make([]string, len(items))
	for index, logged := range items {
		keys[index] = fmt.Sprintf("%d %s", logged.Ordinal, logged.Item.ID)
	}
	return keys
}

// getConversationMessageTexts 返回模型请求中非 system 消息的角色与文字。
func getConversationMessageTexts(body map[string]any) []modelMessageText {
	// 1. 提取消息文字并移除 system 消息。
	return slices.DeleteFunc(getModelMessageTexts(body), func(message modelMessageText) bool { return message.role == "system" })
}
