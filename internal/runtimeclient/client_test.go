package runtimeclient_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/hook"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/VaalaCat/openvaala/pkg/runtimeserver"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/skill"
	"github.com/VaalaCat/openvaala/pkg/storage"
	storagefile "github.com/VaalaCat/openvaala/pkg/storage/file"
	storagesqlite "github.com/VaalaCat/openvaala/pkg/storage/sqlite"
	"github.com/VaalaCat/openvaala/pkg/telemetry/telemetrytest"
	"github.com/VaalaCat/openvaala/pkg/utils/syncmap"
	"github.com/VaalaCat/openvaala/pkg/workspace"
	"github.com/coder/websocket"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// clientBuildDirectory 是本包测试共用的编译目录，TestMain 创建，全部测试结束后删除。
var clientBuildDirectory string

// TestMain 准备共用编译目录并运行本包测试。
func TestMain(m *testing.M) {
	// 1. 创建临时目录，失败时打印错误并退出。
	directory, err := os.MkdirTemp("", "openvaala-runtimeclient-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	clientBuildDirectory = directory
	// 2. 运行全部测试，删除编译目录后使用测试退出码退出。
	code := m.Run()
	_ = os.RemoveAll(directory)
	os.Exit(code)
}

// buildFakeMCPServerOnce 在第一次用到时把 pkg/mcpclient 的假 MCP server 编译到 clientBuildDirectory；
// 测试本身带 -race 时同样带 -race 编译。
var buildFakeMCPServerOnce = sync.OnceValues(func() (string, error) {
	binary := filepath.Join(clientBuildDirectory, "mcpserver")
	arguments := []string{"build", "-o", binary}
	if isRaceEnabled() {
		arguments = append(arguments, "-race")
	}
	command := exec.Command("go", append(arguments, "../../pkg/mcpclient/testdata/mcpserver")...)
	command.Env = append(os.Environ(), "GOWORK=off")
	if output, err := command.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build fake MCP server: %w\n%s", err, output)
	}
	return binary, nil
})

// buildFakeMCPServer 返回共用的假 MCP server 程序。
func buildFakeMCPServer(t *testing.T) string {
	t.Helper()
	// 1. 调用一次性编译，编译失败时结束测试。
	binary, err := buildFakeMCPServerOnce()
	if err != nil {
		t.Fatal(err)
	}
	return binary
}

// isRaceEnabled 测试程序是否带 -race 编译？
func isRaceEnabled() bool {
	// 1. 读取构建信息，检查 -race=true 设置。
	info, ok := debug.ReadBuildInfo()
	return ok && slices.ContainsFunc(info.Settings, func(setting debug.BuildSetting) bool {
		return setting.Key == "-race" && setting.Value == "true"
	})
}

// TestClient 检查真实客户端请求、事件、断线恢复和历史读取。
func TestClient(t *testing.T) {
	// 1. 列出正常对话、同步、断线、工具、配置及存储查询的场景。
	tests := []struct {
		name string
		run  func(t *testing.T, h *clientHarness)
	}{
		{
			name: "open_and_stream",
			run: func(t *testing.T, h *clientHarness) {
				// 1. 打开对话，提交输入并返回两段模型文本。
				conversation := h.openNewConversation(t)
				h.submit(t, conversation, "你好")
				h.model.receive(t).reply <- clientModelResponse{chunks: []string{"你好", "！"}}
				events := h.collectEvents(t, isTurnFinished)
				// 2. 统计 Turn、保存流水号和根 Session 状态，核对只执行一轮且进入 running。
				turnStarted, turnFinished := 0, 0
				var savedOrdinals []storage.Ordinal
				var rootStates []session.SessionState
				for _, event := range events {
					switch event := event.(type) {
					case *runtimeclient.TurnStarted:
						turnStarted++
					case *runtimeclient.TurnFinished:
						turnFinished++
						if event.Status != defs.TurnStatusCompleted {
							t.Fatalf("turn finished = %+v", event)
						}
					case *runtimeclient.ItemsSaved:
						for _, logged := range event.Items {
							savedOrdinals = append(savedOrdinals, logged.Ordinal)
						}
					case *runtimeclient.SessionStatusChanged:
						if event.Info.Session.ID == conversation.RootSessionID {
							rootStates = append(rootStates, event.Info.State)
						}
					}
				}
				if turnStarted != 1 || turnFinished != 1 {
					t.Fatalf("content Turn events started=%d finished=%d, want 1 and 1", turnStarted, turnFinished)
				}
				if len(savedOrdinals) != 2 || savedOrdinals[0] != 1 || savedOrdinals[1] != 2 {
					t.Fatalf("saved ordinals = %v, want [1 2]", savedOrdinals)
				}
				// 根 Session 的运行信息先报 idle，Turn 开始后报 running。
				if !slices.Contains(rootStates, session.SessionStateRunning) {
					t.Fatalf("root status = %v, want running", rootStates)
				}
			},
		},
		{
			name: "open_existing_loads_history",
			run: func(t *testing.T, h *clientHarness) {
				// 1. 完成一轮，再次打开同一对话。
				conversation := h.openNewConversation(t)
				h.submit(t, conversation, "第一轮")
				h.model.receive(t).reply <- clientModelResponse{chunks: []string{"一"}}
				h.collectEvents(t, isTurnFinished)
				if _, err := h.client.OpenConversation(t.Context(), &runtimeclient.OpenConversationRequest{ConversationID: conversation.ID}); err != nil {
					t.Fatal(err)
				}
				// 2. 核对两条历史流水号和先于 SessionSynced 的完整 Session 状态。
				synced := h.collectEvents(t, isSessionSynced)
				history := synced[len(synced)-1].(*runtimeclient.SessionSynced)
				if len(history.Items) != 2 || history.Items[0].Ordinal != 1 || history.Items[1].Ordinal != 2 {
					t.Fatalf("history = %+v", history.Items)
				}
				// Session 状态由订阅响应的运行信息初始化，紧接在 SessionSynced 之前交付。
				status := synced[len(synced)-2].(*runtimeclient.ConversationSynced)
				if len(status.SessionInfos) != 1 || status.SessionInfos[0].Session.ID != conversation.RootSessionID ||
					status.SessionInfos[0].State != session.SessionStateIdle || status.SessionInfos[0].LastTurnStatus != defs.TurnStatusCompleted {
					t.Fatalf("synced sessions = %+v", status.SessionInfos)
				}
			},
		},
		{
			name: "reconnect_resumes_without_gap",
			run: func(t *testing.T, h *clientHarness) {
				// 1. 开始流式回复，读到首段后切断连接。
				conversation := h.openNewConversation(t)
				h.submit(t, conversation, "天气怎么样")
				release := make(chan struct{})
				h.model.receive(t).reply <- clientModelResponse{
					chunks:  []string{"今天", "晴"},
					release: release,
				}
				before := h.collectEvents(t, isItemDelta)
				lastOrdinal := getLastSavedOrdinal(before)
				h.proxy.cut()
				// 2. 等待重连同步，核对已保存内容不重发及生成中累计文本。
				var resumed *runtimeclient.SessionSynced
				for _, event := range h.collectEvents(t, isSessionSynced) {
					if synced, ok := event.(*runtimeclient.SessionSynced); ok {
						resumed = synced
					}
				}
				for _, logged := range resumed.Items {
					if logged.Ordinal <= lastOrdinal {
						t.Fatalf("resync re-delivered ordinal %d (last delivered %d)", logged.Ordinal, lastOrdinal)
					}
				}
				if len(resumed.StreamingItems) != 1 || itemText(resumed.StreamingItems[0].Item) != "今天" {
					t.Fatalf("streaming snapshot = %+v", resumed.StreamingItems)
				}
				// 3. 放行后续回复，核对新增保存流水号连续，增量和最终文本完整。
				text := itemText(resumed.StreamingItems[0].Item)
				close(release)
				var savedText string
				next := lastOrdinal + storage.Ordinal(len(resumed.Items)) + 1
				for _, event := range h.collectEvents(t, isTurnFinished) {
					switch event := event.(type) {
					case *runtimeclient.ItemChanged:
						if event.Type == agent.AgentEventItemDelta && event.ItemIndex == resumed.StreamingItems[0].ItemIndex {
							text += event.Delta.Text
						}
					case *runtimeclient.ItemsSaved:
						for _, logged := range event.Items {
							if logged.Ordinal != next {
								t.Fatalf("saved ordinal = %d, want %d", logged.Ordinal, next)
							}
							next++
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
			name: "reopen_current_conversation_mid_stream_no_duplicate",
			run: func(t *testing.T, h *clientHarness) {
				// 1. 开始流式回复，在首段之后重新打开同一对话。
				conversation := h.openNewConversation(t)
				h.submit(t, conversation, "天气怎么样")
				release, finish := make(chan struct{}), make(chan struct{})
				h.model.receive(t).reply <- clientModelResponse{
					chunks:  []string{"今天", "sunny"},
					release: release,
					finish:  finish,
				}
				h.collectEvents(t, isItemDelta)
				// 2. 扣留退订请求，放行旧订阅增量并等它暂存后恢复同步。
				hold := h.proxy.holdUpstream(`"sunny"`)
				opened := make(chan error, 1)
				go func() {
					_, err := h.client.OpenConversation(t.Context(), &runtimeclient.OpenConversationRequest{ConversationID: conversation.ID})
					opened <- err
				}()
				waitClosed(t, hold.arrived, "re-open request")
				close(release)
				waitClosed(t, hold.markerSeen, "old subscription delta")
				h.proxy.releaseUpstream()
				if err := <-opened; err != nil {
					t.Fatal(err)
				}
				// 3. 取得同步结果并放行结束，拼接后续增量，核对文本无重复。
				var snapshot *runtimeclient.SessionSynced
				for _, event := range h.collectEvents(t, isSessionSynced) {
					if synced, ok := event.(*runtimeclient.SessionSynced); ok {
						snapshot = synced
					}
				}
				if len(snapshot.StreamingItems) != 1 {
					t.Fatalf("streaming snapshot = %+v", snapshot.StreamingItems)
				}
				text := itemText(snapshot.StreamingItems[0].Item)
				close(finish)
				for _, event := range h.collectEvents(t, isTurnFinished) {
					if changed, ok := event.(*runtimeclient.ItemChanged); ok && changed.Type == agent.AgentEventItemDelta &&
						changed.ItemIndex == snapshot.StreamingItems[0].ItemIndex {
						text += changed.Delta.Text
					}
				}
				if text != "今天sunny" {
					t.Fatalf("assembled text = %q, want %q", text, "今天sunny")
				}
			},
		},
		{
			name: "submit_while_disconnected_runs_once",
			run: func(t *testing.T, h *clientHarness) {
				// 1. 暂停代理并断开连接，异步提交离线输入。
				conversation := h.openNewConversation(t)
				h.proxy.pause()
				h.proxy.cut()
				h.collectEvents(t, isConnectionState(runtimeclient.ConnectionReconnecting))
				submitted := make(chan error, 1)
				go func() {
					_, err := h.client.SubmitInput(t.Context(), &runtimeclient.SubmitInputRequest{
						ConversationID: conversation.ID,
						Content:        textContent("离线时发送"),
					})
					submitted <- err
				}()
				// 2. 放行重连，等同步后回复模型，核对提交成功且没有额外模型请求。
				h.proxy.resume()
				request := h.model.receive(t)
				// 排队提交可先于重新订阅执行；先等同步完成，再交付模型回复。
				h.collectEvents(t, isSessionSynced)
				request.reply <- clientModelResponse{chunks: []string{"收到"}}
				select {
				case err := <-submitted:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("SubmitInput did not return after reconnect")
				}
				h.collectEvents(t, isTurnFinished)
				h.model.requireNoRequest(t)
			},
		},
		{
			// 编码后超过 serve 读取上限（16 MiB）的请求不发出也不重发：立即返回普通错误，连接保持，之后的正常提交照常执行。
			name: "oversized_submit_fails_without_resend",
			run: func(t *testing.T, h *clientHarness) {
				// 1. 提交编码后超过 16 MiB 的内容。
				conversation := h.openNewConversation(t)
				oversized := strings.Repeat("a", 16<<20)
				submitCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				_, err := h.client.SubmitInput(submitCtx, &runtimeclient.SubmitInputRequest{
					ConversationID: conversation.ID,
					Content:        textContent(oversized),
				})
				// 2. 核对立即返回普通错误，再提交正常内容，核对连接未重连且只执行一次。
				if err == nil || errors.Is(err, runtimeclient.ErrConnectionLost) || submitCtx.Err() != nil {
					t.Fatalf("oversized SubmitInput error = %v (context error %v), want an immediate plain error", err, submitCtx.Err())
				}
				h.submit(t, conversation, "正常提交")
				h.model.receive(t).reply <- clientModelResponse{chunks: []string{"收到"}}
				events := h.collectEvents(t, isTurnFinished)
				if attempts := getReconnectAttempts(events); len(attempts) != 0 {
					t.Fatalf("reconnect attempts = %v, want the connection to stay up", attempts)
				}
				h.model.requireNoRequest(t)
			},
		},
		{
			// 服务端关闭订阅（例如订阅方过慢）后客户端重新同步，之后的 Turn 照常交付且不重复。
			name: "server_closed_subscription_resyncs",
			run: func(t *testing.T, h *clientHarness) {
				// 1. 伪造过慢订阅关闭，等客户端重新同步。
				conversation := h.openNewConversation(t)
				h.proxy.injectDownstream(t, map[string]any{"jsonrpc": "2.0", "method": "subscription/closed", "params": map[string]any{
					"conversation_id": conversation.ID,
					"session_id":      conversation.RootSessionID,
					"subscription":    "session",
					"reason":          "subscriber too slow",
				}})
				h.collectEvents(t, isSessionSynced)
				// 2. 提交新一轮，核对完成事件和流水号只交付一次。
				h.submit(t, conversation, "重新订阅后")
				h.model.receive(t).reply <- clientModelResponse{chunks: []string{"好"}}
				turnFinished := 0
				var savedOrdinals []storage.Ordinal
				for _, event := range h.collectEvents(t, isTurnFinished) {
					switch event := event.(type) {
					case *runtimeclient.TurnFinished:
						turnFinished++
					case *runtimeclient.ItemsSaved:
						for _, logged := range event.Items {
							savedOrdinals = append(savedOrdinals, logged.Ordinal)
						}
					}
				}
				h.model.requireNoRequest(t)
				if turnFinished != 1 || !slices.Equal(savedOrdinals, []storage.Ordinal{1, 2}) {
					t.Fatalf("turn finished = %d, saved ordinals = %v, want 1 and [1 2]", turnFinished, savedOrdinals)
				}
			},
		},
		{
			// 首次打开失败只让这次 OpenConversation 返回错误，连接保持可用。
			name: "open_unknown_conversation_fails",
			run: func(t *testing.T, h *clientHarness) {
				// 1. 打开不存在的对话，核对请求失败。
				if _, err := h.client.OpenConversation(t.Context(), &runtimeclient.OpenConversationRequest{ConversationID: "missing-conversation"}); err == nil {
					t.Fatal("OpenConversation(missing) succeeded")
				}
				// 2. 再创建并执行正常对话，核对连接仍可使用。
				conversation := h.openNewConversation(t)
				h.submit(t, conversation, "仍可使用")
				h.model.receive(t).reply <- clientModelResponse{chunks: []string{"是"}}
				h.collectEvents(t, isTurnFinished)
			},
		},
		{
			// 重连后同步持续失败时退避次数继续增长；同步成功后才清零。
			name: "sync_failure_keeps_backoff_growing",
			run: func(t *testing.T, h *clientHarness) {
				// 1. 持续切断重新同步，收集前两次重连。
				h.openNewConversation(t)
				h.proxy.cutOnDownstream(`"streaming_items"`)
				h.proxy.cut()
				// 2. 核对退避次数持续增长，同步成功后再次断线从一次开始。
				attempts := getReconnectAttempts(h.collectEvents(t, isReconnectAttempt(2)))
				if !slices.Equal(attempts, []int{1, 2}) {
					t.Fatalf("reconnect attempts = %v, want [1 2]", attempts)
				}
				h.proxy.cutOnDownstream("")
				h.collectEvents(t, isSessionSynced)
				h.proxy.cut()
				attempts = getReconnectAttempts(h.collectEvents(t, isConnectionState(runtimeclient.ConnectionReconnecting)))
				if !slices.Equal(attempts, []int{1}) {
					t.Fatalf("reconnect attempts after a successful sync = %v, want [1]", attempts)
				}
			},
		},
		{
			name: "child_status_from_conversation_subscription",
			run: func(t *testing.T, h *clientHarness) {
				// 1. 回复 spawn_agent，分别完成根和子任务。
				conversation := h.openNewConversation(t)
				h.submit(t, conversation, "委派")
				h.model.receive(t).reply <- clientModelResponse{calls: []llmkit.ToolCall{{
					ID:        "spawn",
					Name:      "spawn_agent",
					Arguments: `{"task_name":"review","message":"check"}`,
				}}}
				root, child := h.model.receiveRootAndTask(t, "review")
				root.reply <- clientModelResponse{chunks: []string{"已委派"}}
				child.reply <- clientModelResponse{chunks: []string{"子任务完成"}}
				// 2. 从对话订阅等子任务完成状态，核对新子 Session 的父 ID。
				events := h.collectEvents(t, func(event runtimeclient.ClientEvent) bool {
					status, ok := event.(*runtimeclient.SessionStatusChanged)
					return ok && status.Info.Session.TaskName == "review" && status.Info.State == session.SessionStateIdle &&
						status.Info.LastTurnStatus == defs.TurnStatusCompleted
				})
				// 界面按 ParentSessionID 建子 Agent 树：订阅之后才新建的子 Session，记录只能来自运行信息。
				if status := events[len(events)-1].(*runtimeclient.SessionStatusChanged); status.Info.Session.ParentSessionID != conversation.RootSessionID {
					t.Fatalf("child Session = %+v, want parent %s", status.Info.Session, conversation.RootSessionID)
				}
			},
		},
		{
			name: "reconnect_logs_warning",
			run: func(t *testing.T, h *clientHarness) {
				// 1. 打开对话后切断代理，等待重连事件。
				h.openNewConversation(t)
				h.proxy.cut()
				h.collectEvents(t, isConnectionState(runtimeclient.ConnectionReconnecting))
				// 2. 核对断线日志为 WARN 且重连次数为一。
				record := h.telemetry.WaitLogRecords(t, "runtime connection lost, reconnecting", 1)[0]
				if record["level"] != "WARN" || record["attempt"] != float64(1) {
					t.Fatalf("reconnect log = %v", record)
				}
			},
		},
		{
			name: "undecodable_notification_logs_drop",
			run: func(t *testing.T, h *clientHarness) {
				// 1. 注入参数不是对象的 Session 通知。
				h.openNewConversation(t)
				h.proxy.injectDownstream(t, map[string]any{"jsonrpc": "2.0", "method": "session/started", "params": "not-an-object"})
				// 2. 核对丢弃警告和通知方法名。
				record := h.telemetry.WaitLogRecords(t, "notification dropped", 1)[0]
				if record["level"] != "WARN" || record["rpc.method"] != "session/started" {
					t.Fatalf("notification dropped log = %v", record)
				}
			},
		},
		{
			name: "switching_conversation_drops_old_events",
			run: func(t *testing.T, h *clientHarness) {
				// 1. 先后打开两个对话，分别提交输入和回复。
				first := h.openNewConversation(t)
				second := h.openNewConversation(t)
				h.submit(t, first, "旧对话")
				h.model.receive(t).reply <- clientModelResponse{chunks: []string{"旧"}}
				h.submit(t, second, "新对话")
				h.model.receive(t).reply <- clientModelResponse{chunks: []string{"新"}}
				// 2. 只读取当前对话事件，核对旧对话事件未交付。
				for _, event := range h.collectEvents(t, isTurnFinished) {
					if getConversationID(event) == first.ID {
						t.Fatalf("event of the closed conversation delivered: %T", event)
					}
				}
			},
		},
		{
			name: "tool_progress_delivered",
			run: func(t *testing.T, h *clientHarness) {
				// 1. 向已打开对话注入工具 stdout 进度。
				conversation := h.openNewConversation(t)
				h.proxy.injectDownstream(t, map[string]any{"jsonrpc": "2.0", "method": "item/toolProgress", "params": map[string]any{
					"conversation_id": conversation.ID,
					"session_id":      conversation.RootSessionID,
					"subscription":    "session",
					"turn_id":         "t1",
					"item_index":      3,
					"tool_progress":   map[string]any{"stream": "stdout", "content": json.RawMessage(`"ok\n"`)},
				}})
				// 2. 等待 ToolProgress 并核对对话、Turn、Item 位置和 JSON 内容。
				events := h.collectEvents(t, func(event runtimeclient.ClientEvent) bool {
					_, ok := event.(*runtimeclient.ToolProgress)
					return ok
				})
				progress := events[len(events)-1].(*runtimeclient.ToolProgress)
				if progress.ConversationID != conversation.ID || progress.TurnID != "t1" || progress.ItemIndex != 3 ||
					progress.Progress.Stream != "stdout" || string(progress.Progress.Content) != `"ok\n"` {
					t.Fatalf("tool progress = %+v", progress)
				}
			},
		},
		{
			name: "get_hooks",
			run: func(t *testing.T, h *clientHarness) {
				// 1. 先查询空 hook 列表，再设置两类测试 hook。
				hooks, err := h.client.GetHooks(t.Context())
				if err != nil || hooks == nil || len(hooks) != 0 {
					t.Fatalf("GetHooks() without hooks = %+v, %v; want an empty list", hooks, err)
				}
				matcher, err := hook.NewHookMatcher("Bash|Read")
				if err != nil {
					t.Fatal(err)
				}
				allMatcher, err := hook.NewHookMatcher("")
				if err != nil {
					t.Fatal(err)
				}
				h.setHooks(t, []*hook.HookDefinition{
					{
						Key:          "go.yaml:PreToolUse:0",
						Event:        defs.HookEventPreToolUse,
						Matcher:      matcher,
						Command:      "check.sh --strict",
						Timeout:      30 * time.Second,
						SourcePath:   "/hooks/go.yaml",
						DisplayOrder: 1,
					},
					{
						Key:          "go.yaml:Stop:1",
						Event:        defs.HookEventStop,
						Matcher:      allMatcher,
						ScriptPath:   "/hooks/stop.sh",
						Args:         []string{"a"},
						Timeout:      10 * time.Second,
						SourcePath:   "/hooks/go.yaml",
						DisplayOrder: 2,
					},
				})
				// 2. 重新读取列表，核对哈希非空且不同，以及其他配置字段。
				hooks, err = h.client.GetHooks(t.Context())
				if err != nil || len(hooks) != 2 {
					t.Fatalf("GetHooks() = %+v, %v; want 2 hooks", hooks, err)
				}
				if hooks[0].CurrentHash == "" || hooks[0].CurrentHash == hooks[1].CurrentHash {
					t.Fatalf("current hashes = %q and %q, want two different non-empty hashes", hooks[0].CurrentHash, hooks[1].CurrentHash)
				}
				hooks[0].CurrentHash, hooks[1].CurrentHash = "", ""
				want := []*hook.HookInfo{
					{
						Key:            "go.yaml:PreToolUse:0",
						Event:          defs.HookEventPreToolUse,
						Matcher:        "Bash|Read",
						Handler:        "check.sh --strict",
						TimeoutSeconds: 30,
						SourcePath:     "/hooks/go.yaml",
						DisplayOrder:   1,
					},
					{
						Key:            "go.yaml:Stop:1",
						Event:          defs.HookEventStop,
						Handler:        "/hooks/stop.sh a",
						TimeoutSeconds: 10,
						SourcePath:     "/hooks/go.yaml",
						DisplayOrder:   2,
					},
				}
				if !reflect.DeepEqual(hooks, want) {
					t.Fatalf("GetHooks() = %+v, want %+v", hooks, want)
				}
			},
		},
		{
			name: "hook_runs_delivered",
			run: func(t *testing.T, h *clientHarness) {
				// 1. 设置 UserPromptSubmit hook，执行一轮。
				matcher, err := hook.NewHookMatcher("")
				if err != nil {
					t.Fatal(err)
				}
				h.setHooks(t, []*hook.HookDefinition{{
					Key:          "go.yaml:UserPromptSubmit:0",
					Event:        defs.HookEventUserPromptSubmit,
					Matcher:      matcher,
					Command:      `printf '{"systemMessage":"已检查"}'`,
					Timeout:      10 * time.Second,
					SourcePath:   "/hooks/go.yaml",
					DisplayOrder: 1,
				}})
				conversation := h.openNewConversation(t)
				h.submit(t, conversation, "你好")
				h.model.receive(t).reply <- clientModelResponse{chunks: []string{"好"}}
				// 2. 收集开始和完成记录，核对同一 Run、Turn、时间和完整输出。
				var turnID defs.TurnID
				var changes []*runtimeclient.HookRunChanged
				for _, event := range h.collectEvents(t, isTurnFinished) {
					switch event := event.(type) {
					case *runtimeclient.TurnStarted:
						turnID = event.TurnID
					case *runtimeclient.HookRunChanged:
						changes = append(changes, event)
					}
				}
				if len(changes) != 2 {
					t.Fatalf("HookRunChanged events = %+v, want hook/started then hook/completed", changes)
				}
				started, completed := changes[0], changes[1]
				if started.ConversationID != conversation.ID || completed.ConversationID != conversation.ID || started.TurnID != turnID || completed.TurnID != turnID {
					t.Fatalf("events target = %s/%s turn %s/%s, want conversation %s turn %s",
						started.ConversationID, completed.ConversationID, started.TurnID, completed.TurnID, conversation.ID, turnID)
				}
				if started.Run.ID == "" || started.Run.ID != completed.Run.ID || started.Run.Status != defs.HookRunRunning || completed.Run.Status != defs.HookRunCompleted {
					t.Fatalf("runs = %+v and %+v, want one run going from running to completed", started.Run, completed.Run)
				}
				if completed.Run.Event != defs.HookEventUserPromptSubmit || completed.Run.SystemMessage != "已检查" || completed.Run.SourcePath != "/hooks/go.yaml" ||
					completed.Run.Handler != `printf '{"systemMessage":"已检查"}'` || completed.Run.CompletedAt < completed.Run.StartedAt {
					t.Fatalf("completed run = %+v", completed.Run)
				}
			},
		},
		{
			// clientMCP/* 通知与打开的对话无关：没有打开对话时也立即发出事件，字段原样带出。
			name: "client_mcp_notifications_become_events_without_opened_conversation",
			run: func(t *testing.T, h *clientHarness) {
				// 1. 连接后注入 MCP 连接、消息和断开通知，不打开对话。
				h.collectEvents(t, isConnectionState(runtimeclient.ConnectionConnected))
				message := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
				for _, notification := range []map[string]any{
					{"method": "clientMCP/connect", "params": map[string]any{"stream_id": "s1", "server_id": "fake"}},
					{"method": "clientMCP/message", "params": map[string]any{"stream_id": "s1", "message": message}},
					{"method": "clientMCP/disconnect", "params": map[string]any{"stream_id": "s1"}},
				} {
					notification["jsonrpc"] = "2.0"
					h.proxy.injectDownstream(t, notification)
				}
				// 2. 按顺序收集事件，核对流 ID、server ID 和消息原文。
				var got []runtimeclient.ClientEvent
				h.collectEvents(t, func(event runtimeclient.ClientEvent) bool {
					switch event.(type) {
					case *runtimeclient.ClientMCPConnectRequested, *runtimeclient.ClientMCPMessageReceived, *runtimeclient.ClientMCPDisconnectRequested:
						got = append(got, event)
					}
					return len(got) == 3
				})
				want := []runtimeclient.ClientEvent{
					&runtimeclient.ClientMCPConnectRequested{
						StreamID: "s1",
						ServerID: "fake",
					},
					&runtimeclient.ClientMCPMessageReceived{
						StreamID: "s1",
						Message:  message,
					},
					&runtimeclient.ClientMCPDisconnectRequested{StreamID: "s1"},
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("events = %+v, want %+v", got, want)
				}
			},
		},
		{
			// 同步进行中通知先暂存，但 clientMCP/message 不暂存：同步被扣住时事件立即发出，ConversationSynced 还没到。
			name: "client_mcp_message_during_sync_is_not_staged",
			run: func(t *testing.T, h *clientHarness) {
				// 1. 扣留重新打开请求，在同步期间注入 MCP 消息。
				conversation := h.openNewConversation(t)
				hold := h.proxy.holdUpstream("unused")
				opened := make(chan error, 1)
				go func() {
					_, err := h.client.OpenConversation(t.Context(), &runtimeclient.OpenConversationRequest{ConversationID: conversation.ID})
					opened <- err
				}()
				waitClosed(t, hold.arrived, "re-open request")
				h.proxy.injectDownstream(t, map[string]any{"jsonrpc": "2.0", "method": "clientMCP/message", "params": map[string]any{
					"stream_id": "s1", "message": json.RawMessage(`{"jsonrpc":"2.0","method":"ping"}`),
				}})
				// 2. 核对消息先于 ConversationSynced 交付，再放行请求并检查成功。
				events := h.collectEvents(t, func(event runtimeclient.ClientEvent) bool {
					_, ok := event.(*runtimeclient.ClientMCPMessageReceived)
					return ok
				})
				for _, event := range events {
					if _, ok := event.(*runtimeclient.ConversationSynced); ok {
						t.Fatalf("events = %+v, want the MCP message before the sync finishes", events)
					}
				}
				h.proxy.releaseUpstream()
				if err := <-opened; err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// 登记请求和两条通知的线上格式：server_ids 为空时是 []，通知没有 id，closed 的 error 为空时省略；
			// serve 不认识的 stream_id 的通知被丢弃，连接保持可用。
			name: "client_mcp_requests_wire_format",
			run: func(t *testing.T, h *clientHarness) {
				// 1. 登记非空和空 MCP 列表，发送消息、带错和无错关闭通知。
				conversation := h.openNewConversation(t)
				ctx := t.Context()
				if err := h.client.SetClientMCPServers(ctx, conversation.ID, []string{"a", "b"}); err != nil {
					t.Fatal(err)
				}
				if err := h.client.SetClientMCPServers(ctx, conversation.ID, nil); err != nil {
					t.Fatal(err)
				}
				message := json.RawMessage(`{"jsonrpc":"2.0","method":"ping"}`)
				if err := h.client.SendClientMCPMessage(ctx, "unknown-stream", message); err != nil {
					t.Fatal(err)
				}
				if err := h.client.SendClientMCPClosed(ctx, "unknown-stream", errors.New("boom")); err != nil {
					t.Fatal(err)
				}
				if err := h.client.SendClientMCPClosed(ctx, "unknown-stream", nil); err != nil {
					t.Fatal(err)
				}
				if _, err := h.client.ListMCPServers(ctx, conversation.ID); err != nil {
					t.Fatal(err)
				}
				// 2. 收集上行帧，核对方法、参数、请求 ID 和省略字段。
				var frames []string
				for _, frame := range h.proxy.getUpstreamFrames() {
					if strings.Contains(frame, "ClientMCP") || strings.Contains(frame, "clientMCP") {
						frames = append(frames, frame)
					}
				}
				id := `"conversation_id":"` + string(conversation.ID) + `"`
				wantMethods := []string{"conversation/setClientMCPServers", "conversation/setClientMCPServers", "clientMCP/message", "clientMCP/closed", "clientMCP/closed"}
				wantParams := []string{
					`{` + id + `,"server_ids":["a","b"]}`,
					`{` + id + `,"server_ids":[]}`,
					`{"stream_id":"unknown-stream","message":{"jsonrpc":"2.0","method":"ping"}}`,
					`{"stream_id":"unknown-stream","error":"boom"}`,
					`{"stream_id":"unknown-stream"}`,
				}
				if len(frames) != len(wantMethods) {
					t.Fatalf("client MCP frames = %v, want %d frames", frames, len(wantMethods))
				}
				for index, frame := range frames {
					var decoded struct {
						ID     *json.RawMessage `json:"id"`
						Method string           `json:"method"`
						Params json.RawMessage  `json:"params"`
					}
					if err := json.Unmarshal([]byte(frame), &decoded); err != nil {
						t.Fatal(err)
					}
					isRequest := decoded.Method == "conversation/setClientMCPServers"
					if decoded.Method != wantMethods[index] || string(decoded.Params) != wantParams[index] || (decoded.ID != nil) != isRequest {
						t.Fatalf("frame %d = %s, want method %s params %s (request with id: %v)", index, frame, wantMethods[index], wantParams[index], isRequest)
					}
				}
			},
		},
		{
			// 断开期间 clientMCP 通知不等重连：立即返回 ErrConnectionLost（旧 stream_id 到新连接上 serve 也不认识）；重连后恢复可用。
			name: "client_mcp_notifications_fail_fast_while_disconnected",
			run: func(t *testing.T, h *clientHarness) {
				// 1. 暂停代理并断开连接，准备消息与关闭通知。
				h.openNewConversation(t)
				h.proxy.pause()
				h.proxy.cut()
				h.collectEvents(t, isConnectionState(runtimeclient.ConnectionReconnecting))
				message := json.RawMessage(`{"jsonrpc":"2.0","method":"ping"}`)
				senders := []struct {
					name string
					send func(ctx context.Context) error
				}{
					{
						"message",
						func(ctx context.Context) error { return h.client.SendClientMCPMessage(ctx, "s1", message) },
					},
					{
						"closed",
						func(ctx context.Context) error { return h.client.SendClientMCPClosed(ctx, "s1", errors.New("boom")) },
					},
				}
				// 2. 核对断线通知立即返回 ErrConnectionLost，再核对重连后均成功。
				for _, sender := range senders {
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					err := sender.send(ctx)
					timedOut := ctx.Err() != nil
					cancel()
					if !errors.Is(err, runtimeclient.ErrConnectionLost) || timedOut {
						t.Fatalf("%s while disconnected = %v (timed out %v), want an immediate ErrConnectionLost", sender.name, err, timedOut)
					}
				}
				h.proxy.resume()
				h.collectEvents(t, isConnectionState(runtimeclient.ConnectionConnected))
				for _, sender := range senders {
					if err := sender.send(t.Context()); err != nil {
						t.Fatalf("%s after reconnecting = %v, want nil", sender.name, err)
					}
				}
			},
		},
		{
			name: "list_and_set_session_model",
			run: func(t *testing.T, h *clientHarness) {
				// 1. 查询模型列表并为新对话设置第二个模型和 high 强度。
				list, err := h.client.ListModels(t.Context(), "assistant")
				if err != nil {
					t.Fatal(err)
				}
				if len(list.Models) != 2 || list.AgentDefaultModel != (defs.ModelRef{
					ProviderID: "test",
					ModelID:    "test",
				}) {
					t.Fatalf("model list = %+v", list)
				}
				conversation := h.openNewConversation(t)
				second := defs.ModelSelection{
					Ref: defs.ModelRef{
						ProviderID: "test",
						ModelID:    "second",
					},
					ReasoningEffort: "high",
				}
				record, err := h.client.SetSessionModel(t.Context(), &runtimeclient.SetSessionModelRequest{
					ConversationID: conversation.ID,
					SessionID:      conversation.RootSessionID,
					Model:          second,
				})
				if err != nil {
					t.Fatal(err)
				}
				// 2. 核对保存模型和状态事件，再检查不支持的 max 强度返回错误。
				if record.Model != second {
					t.Fatalf("saved model = %+v, want %+v", record.Model, second)
				}
				h.collectEvents(t, func(event runtimeclient.ClientEvent) bool {
					changed, ok := event.(*runtimeclient.SessionStatusChanged)
					return ok && changed.Info.Session.Model == second
				})
				// 推理强度不受支持时返回普通错误。
				_, err = h.client.SetSessionModel(t.Context(), &runtimeclient.SetSessionModelRequest{
					ConversationID: conversation.ID,
					SessionID:      conversation.RootSessionID,
					Model: defs.ModelSelection{
						Ref:             second.Ref,
						ReasoningEffort: "max",
					},
				})
				if err == nil {
					t.Fatal("unsupported reasoning effort accepted")
				}
			},
		},
		{
			name: "list_agents",
			run: func(t *testing.T, h *clientHarness) {
				// 1. 查询 Agent，核对名称、说明和默认模型。
				agents, err := h.client.ListAgents(t.Context())
				want := []runtime.AgentInfo{{Name: "assistant", Description: "主 Agent", DefaultModel: defs.ModelRef{ProviderID: "test", ModelID: "test"}}}
				if err != nil || !reflect.DeepEqual(agents, want) {
					t.Fatalf("agents = %+v, %v; want %+v", agents, err, want)
				}
			},
		},
		{
			name: "list_executors_and_resolve_directory",
			run: func(t *testing.T, h *clientHarness) {
				// 1. 查询 Executor，核对默认目录和配置顺序。
				definedExecutors, err := h.definitions.Executors.GetExecutors(t.Context(), "")
				if err != nil {
					t.Fatal(err)
				}
				serveDirectory := definedExecutors[0].GetDefinition().WorkspaceDirectory
				executors, err := h.client.ListExecutors(t.Context())
				wantExecutors := []conversation.ExecutorDetails{
					{
						ID:                 "local",
						Kind:               defs.ExecutorKindLocal,
						Description:        "本机",
						WorkspaceDirectory: serveDirectory,
					},
					{
						ID:                 "openvaala",
						Kind:               defs.ExecutorKindVirtual,
						Description:        "内置",
						WorkspaceDirectory: "/workspace",
					},
				}
				if err != nil || !reflect.DeepEqual(executors, wantExecutors) {
					t.Fatalf("executors = %+v, %v; want %+v", executors, err, wantExecutors)
				}
				// 2. 创建本机目录，分别解析带基准、不带基准和未知 Executor 的请求。
				for _, directory := range []string{filepath.Join(serveDirectory, "proj", "sub"), filepath.Join(serveDirectory, "lib")} {
					if err := os.MkdirAll(directory, 0700); err != nil {
						t.Fatal(err)
					}
				}
				for _, tt := range []struct {
					name      string
					request   runtimeclient.ResolveWorkspaceDirectoryRequest
					want      string
					wantError string
				}{
					{
						name: "with_base_directory",
						request: runtimeclient.ResolveWorkspaceDirectoryRequest{
							ExecutorID:    "local",
							Directory:     "sub",
							BaseDirectory: filepath.Join(serveDirectory, "proj"),
						},
						want: filepath.Join(serveDirectory, "proj", "sub"),
					},
					{
						name: "without_base_directory",
						request: runtimeclient.ResolveWorkspaceDirectoryRequest{
							ExecutorID: "local",
							Directory:  "lib",
						},
						want: filepath.Join(serveDirectory, "lib"),
					},
					{
						name: "unknown_executor",
						request: runtimeclient.ResolveWorkspaceDirectoryRequest{
							ExecutorID: "gone",
							Directory:  "lib",
						},
						wantError: `invalid executor settings: unknown executor "gone"`,
					},
				} {
					directory, err := h.client.ResolveWorkspaceDirectory(t.Context(), &tt.request)
					if tt.wantError != "" {
						if err == nil || !strings.Contains(err.Error(), tt.wantError) {
							t.Fatalf("%s error = %v, want containing %q", tt.name, err, tt.wantError)
						}
						continue
					}
					if err != nil || directory != tt.want {
						t.Fatalf("%s = %q, %v; want %q", tt.name, directory, err, tt.want)
					}
				}
			},
		},
		{
			name: "list_mcp_servers",
			run: func(t *testing.T, h *clientHarness) {
				// 1. 创建没有 MCP 的对话并查询连接列表。
				conversation := h.openNewConversation(t)
				servers, err := h.client.ListMCPServers(t.Context(), conversation.ID)
				if err != nil || len(servers) != 0 {
					t.Fatalf("ListMCPServers() of an open conversation without MCP = %+v, %v; want none", servers, err)
				}
				// 2. 核对未打开对话返回 conversation is not open。
				if _, err := h.client.ListMCPServers(t.Context(), "missing"); err == nil || !strings.Contains(err.Error(), "conversation is not open") {
					t.Fatalf("ListMCPServers() of a conversation that is not open error = %v, want containing %q", err, "conversation is not open")
				}
			},
		},
		{
			name: "context_usage_and_details",
			run: func(t *testing.T, h *clientHarness) {
				// 1. 完成一轮，查询上下文占用和 Session 详情。
				conversation := h.openNewConversation(t)
				h.submit(t, conversation, "你好")
				h.model.receive(t).reply <- clientModelResponse{chunks: []string{"好"}}
				h.collectEvents(t, isTurnFinished)
				usage, err := h.client.GetContextUsage(t.Context(), conversation.ID, conversation.RootSessionID)
				if err != nil {
					t.Fatal(err)
				}
				if usage.NextRequest.Source == "" || len(usage.Items) == 0 {
					t.Fatalf("context usage = %+v", usage)
				}
				details, err := h.client.GetSessionDetails(t.Context(), conversation.ID, conversation.RootSessionID)
				if err != nil {
					t.Fatal(err)
				}
				if details.Session.ID != conversation.RootSessionID || details.Model == nil ||
					details.Model.Ref != (defs.ModelRef{
						ProviderID: "test",
						ModelID:    "test",
					}) {
					t.Fatalf("session details = %+v", details)
				}
				// 2. 核对不存在的 Session 上下文查询失败。
				if _, err := h.client.GetContextUsage(t.Context(), conversation.ID, "missing"); err == nil {
					t.Fatal("context usage of an unknown session succeeded")
				}
			},
		},
		{
			name: "create_with_model_and_runtime_infos",
			run: func(t *testing.T, h *clientHarness) {
				// 1. 指定第二个模型创建并打开对话。
				second := defs.ModelSelection{Ref: defs.ModelRef{
					ProviderID: "test",
					ModelID:    "second",
				}}
				conversation, err := h.client.CreateConversation(t.Context(), &runtimeclient.CreateConversationRequest{
					AgentName: "assistant",
					Model:     second,
				})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := h.client.OpenConversation(t.Context(), &runtimeclient.OpenConversationRequest{ConversationID: conversation.ID}); err != nil {
					t.Fatal(err)
				}
				// 2. 读取同步记录，核对根 Session 的模型引用。
				events := h.collectEvents(t, func(event runtimeclient.ClientEvent) bool {
					_, ok := event.(*runtimeclient.ConversationSynced)
					return ok
				})
				synced := events[len(events)-1].(*runtimeclient.ConversationSynced)
				// 根 Session 尚未启动时，订阅记录的 State 仍为 stopped；此处只检查模型。
				if len(synced.SessionInfos) != 1 || synced.SessionInfos[0].Session.Model.Ref != second.Ref {
					t.Fatalf("synced infos = %+v", synced.SessionInfos)
				}
			},
		},
		{
			name: "turn_finished_input_ids",
			run: func(t *testing.T, h *clientHarness) {
				// 1. 提交输入并完成模型回复。
				conversation := h.openNewConversation(t)
				itemID, err := h.client.SubmitInput(t.Context(), &runtimeclient.SubmitInputRequest{
					ConversationID: conversation.ID,
					Content:        textContent("你好"),
				})
				if err != nil {
					t.Fatal(err)
				}
				h.model.receive(t).reply <- clientModelResponse{chunks: []string{"好"}}
				// 2. 核对完成事件携带本次返回的输入 Item ID。
				events := h.collectEvents(t, isTurnFinished)
				finished := events[len(events)-1].(*runtimeclient.TurnFinished)
				if !slices.Equal(finished.InputItemIDs, []defs.ItemID{itemID}) {
					t.Fatalf("turn finished input item ids = %v, want [%s]", finished.InputItemIDs, itemID)
				}
			},
		},
		{
			name: "list_conversations_cursor",
			run: func(t *testing.T, h *clientHarness) {
				// 1. 创建并持久化三个对话。
				var created []defs.ConversationID
				for range 3 {
					conversation, err := h.client.CreateConversation(t.Context(), &runtimeclient.CreateConversationRequest{AgentName: "assistant"})
					if err != nil {
						t.Fatal(err)
					}
					// 新对话在第一条输入时才写进共用库、出现在对话列表里。
					h.submitDirectly(t, conversation, "保存")
					h.model.receive(t).reply <- clientModelResponse{chunks: []string{"好"}}
					created = append(created, conversation.ID)
				}
				// 2. 按两条一页读取，核对顺序和末页空游标。
				first, err := h.client.ListConversations(t.Context(), &runtimeclient.ListConversationsRequest{Limit: 2})
				if err != nil {
					t.Fatal(err)
				}
				if got := getConversationIDs(first.Conversations); !slices.Equal(got, created[:2]) || first.NextCursor == (dao.RecordCursor[defs.ConversationID]{}) {
					t.Fatalf("first page = %v next %+v, want %v and a cursor", got, first.NextCursor, created[:2])
				}
				second, err := h.client.ListConversations(t.Context(), &runtimeclient.ListConversationsRequest{
					Cursor: first.NextCursor,
					Limit:  2,
				})
				if err != nil {
					t.Fatal(err)
				}
				if got := getConversationIDs(second.Conversations); !slices.Equal(got, created[2:]) || second.NextCursor != (dao.RecordCursor[defs.ConversationID]{}) {
					t.Fatalf("second page = %v next %+v, want %v and a zero cursor", got, second.NextCursor, created[2:])
				}
			},
		},
		{
			name: "error_sentinels",
			run: func(t *testing.T, h *clientHarness) {
				// 1. 开始长任务，记录正在执行的 Turn ID。
				busy := h.openNewConversation(t)
				h.submit(t, busy, "长任务")
				busyRequest := h.model.receive(t)
				runningTurnID := h.getSessionTurnIDs(t, busy.ID, busy.RootSessionID)[0]
				// 当前轮次结束后才打开不存在的对话；这个操作会清除当前打开记录。
				// 2. 列出忙碌、分叉、对话和 Session 查询的预期错误。
				tests := []struct {
					name     string
					trigger  func() error
					sentinel error
					wantText string
				}{
					{
						name: "compact_context_busy",
						trigger: func() error {
							_, err := h.client.CompactContext(t.Context(), busy.ID)
							return err
						},
						sentinel: session.ErrSessionBusy,
						wantText: "session is busy",
					},
					{
						name: "delete_conversation_busy",
						trigger: func() error {
							return h.client.DeleteConversation(t.Context(), &runtimeclient.DeleteConversationRequest{ConversationID: busy.ID})
						},
						sentinel: session.ErrSessionBusy,
						wantText: "session is busy",
					},
					{
						name: "fork_turn_not_found",
						trigger: func() error {
							_, err := h.client.ForkConversation(t.Context(), &runtimeclient.ForkConversationRequest{
								ConversationID: busy.ID,
								BeforeTurnID:   "missing-turn",
							})
							return err
						},
						sentinel: conversation.ErrForkTurnNotFound,
						wantText: conversation.ErrForkTurnNotFound.Error(),
					},
					{
						name: "fork_boundary_conflict",
						trigger: func() error {
							_, err := h.client.ForkConversation(t.Context(), &runtimeclient.ForkConversationRequest{
								ConversationID: busy.ID,
								BeforeTurnID:   runningTurnID,
								LastTurnID:     runningTurnID,
							})
							return err
						},
						sentinel: conversation.ErrForkBoundaryConflict,
						wantText: conversation.ErrForkBoundaryConflict.Error(),
					},
					{
						name: "session_busy",
						trigger: func() error {
							_, err := h.client.SubmitInput(t.Context(), &runtimeclient.SubmitInputRequest{
								ConversationID: busy.ID,
								Mode:           session.SessionInputModeIdleOnly,
								Content:        textContent("再来一个"),
							})
							busyRequest.reply <- clientModelResponse{chunks: []string{"好"}}
							h.collectEvents(t, isTurnFinished)
							return err
						},
						sentinel: session.ErrSessionBusy,
						wantText: "session is busy",
					},
					{
						name: "conversation_not_found",
						trigger: func() error {
							_, err := h.client.OpenConversation(t.Context(), &runtimeclient.OpenConversationRequest{ConversationID: "missing-conversation"})
							return err
						},
						sentinel: runtime.ErrConversationNotFound,
						wantText: "conversation not found: missing-conversation",
					},
					{
						name: "session_not_found",
						trigger: func() error {
							_, err := h.client.GetSessionByID(t.Context(), "missing-session")
							return err
						},
						sentinel: runtime.ErrSessionNotFound,
						wantText: "session not found: missing-session",
					},
				}
				// 3. 触发各请求，核对哨兵错误和原文。
				for _, tt := range tests {
					err := tt.trigger()
					if !errors.Is(err, tt.sentinel) || err.Error() != tt.wantText {
						t.Fatalf("%s error = %v, want %v with text %q", tt.name, err, tt.sentinel, tt.wantText)
					}
				}
			},
		},
		{
			// 新建带 Executor 设置；打开对话只看记录里的设置，重新同步只重发环境变量。
			name: "create_sends_settings_and_reopen_sends_environment",
			run: func(t *testing.T, h *clientHarness) {
				// 1. 携带目录和环境变量创建对话，打开后切断并重连。
				directory, environment := t.TempDir(), []string{"TOOL_ENV_MARK=a"}
				settings := defs.ExecutorSettings{WorkspaceDirectories: map[string]string{defs.LocalExecutorID: directory}}
				conversation, err := h.client.CreateConversation(t.Context(), &runtimeclient.CreateConversationRequest{
					AgentName:        "assistant",
					ExecutorSettings: settings,
					Environment:      environment,
				})
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(conversation.ExecutorSettings, settings) {
					t.Fatalf("created executor_settings = %+v, want %+v", conversation.ExecutorSettings, settings)
				}
				request := &runtimeclient.OpenConversationRequest{
					ConversationID: conversation.ID,
					Environment:    environment,
				}
				if _, err := h.client.OpenConversation(t.Context(), request); err != nil {
					t.Fatal(err)
				}
				h.collectEvents(t, isSessionSynced)
				h.proxy.cut()
				h.collectEvents(t, isSessionSynced)
				// 2. 核对创建帧带 Executor 设置，两次打开帧只带环境变量。
				wantSettings, _ := json.Marshal(settings)
				wantEnvironment, _ := json.Marshal(environment)
				creates, opens := 0, 0
				for _, frame := range h.proxy.getUpstreamFrames() {
					switch {
					case strings.Contains(frame, `"method":"conversation/create"`):
						creates++
						if !strings.Contains(frame, `"executor_settings":`+string(wantSettings)) {
							t.Fatalf("conversation/create frame = %s, want executor_settings %s", frame, wantSettings)
						}
					case strings.Contains(frame, `"method":"conversation/open"`):
						opens++
						if strings.Contains(frame, "workspace_directory") || strings.Contains(frame, "executor_settings") || !strings.Contains(frame, `"environment":`+string(wantEnvironment)) {
							t.Fatalf("conversation/open frame = %s, want only environment %s", frame, wantEnvironment)
						}
					}
				}
				if creates != 1 || opens != 2 {
					t.Fatalf("conversation/create frames = %d, conversation/open frames = %d; want 1 and 2 (first open and reconnect)", creates, opens)
				}
			},
		},
		{
			// 12 条已保存 Item 重新打开：历史每页 5 条，分 5、5、2 三页补齐。
			name: "history_paged_by_five",
			run: func(t *testing.T, h *clientHarness) {
				// 1. 完成六轮并重新打开，读取补齐历史。
				conversation := h.openNewConversation(t)
				for turn := range 6 {
					h.submit(t, conversation, fmt.Sprintf("第%d轮", turn+1))
					h.model.receive(t).reply <- clientModelResponse{chunks: []string{"好"}}
					h.collectEvents(t, isTurnFinished)
				}
				if _, err := h.client.OpenConversation(t.Context(), &runtimeclient.OpenConversationRequest{ConversationID: conversation.ID}); err != nil {
					t.Fatal(err)
				}
				synced := h.collectEvents(t, isSessionSynced)
				history := synced[len(synced)-1].(*runtimeclient.SessionSynced)
				// 2. 核对十二条流水号连续，以及三次历史请求均设置页大小五。
				var ordinals []storage.Ordinal
				for _, logged := range history.Items {
					ordinals = append(ordinals, logged.Ordinal)
				}
				if want := []storage.Ordinal{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}; !slices.Equal(ordinals, want) {
					t.Fatalf("history ordinals = %v, want %v", ordinals, want)
				}
				pages := 0
				for _, frame := range h.proxy.getUpstreamFrames() {
					if !strings.Contains(frame, `"method":"session/items/list"`) {
						continue
					}
					pages++
					if !strings.Contains(frame, `"limit":5`) {
						t.Fatalf("session/items/list frame = %s, want limit 5", frame)
					}
				}
				if pages != 3 {
					t.Fatalf("session/items/list frames = %d, want 3", pages)
				}
			},
		},
		{
			// 3 轮各带一张 6 MiB（base64 后）的图片：历史按字节截断后每页都装得下 Client 的读取上限，重新打开能读到全部图片。
			name: "large_history_loads",
			run: func(t *testing.T, h *clientHarness) {
				// 1. 完成三轮，每轮提交 6 MiB 图片。
				conversation := h.openNewConversation(t)
				var dataURLs []string
				for _, fill := range []string{"A", "B", "C"} {
					dataURL := "data:image/png;base64," + strings.Repeat(fill, 6<<20)
					dataURLs = append(dataURLs, dataURL)
					_, err := h.client.SubmitInput(t.Context(), &runtimeclient.SubmitInputRequest{
						ConversationID: conversation.ID,
						Mode:           session.SessionInputModeNormal,
						Content: []*defs.Content{
							{
								Kind: defs.ContentText,
								Text: &defs.TextContent{Text: "请看这张图"},
							},
							{
								Kind:  defs.ContentImage,
								Image: &defs.ImageContent{DataURL: dataURL},
							},
						},
					})
					if err != nil {
						t.Fatal(err)
					}
					h.model.receive(t).reply <- clientModelResponse{chunks: []string{"好"}}
					h.collectEvents(t, isTurnFinished)
				}
				// 2. 重新打开读取历史，核对全部图片按原顺序完整返回。
				if _, err := h.client.OpenConversation(t.Context(), &runtimeclient.OpenConversationRequest{ConversationID: conversation.ID}); err != nil {
					t.Fatal(err)
				}
				synced := h.collectEvents(t, isSessionSynced)
				history := synced[len(synced)-1].(*runtimeclient.SessionSynced)
				var images []string
				for _, logged := range history.Items {
					if logged.Item.Message == nil {
						continue
					}
					for _, content := range logged.Item.Message.Content {
						if content.Kind == defs.ContentImage {
							images = append(images, content.Image.DataURL)
						}
					}
				}
				if !slices.Equal(images, dataURLs) {
					t.Fatalf("history has %d images, want the 3 submitted images in order", len(images))
				}
			},
		},
		{
			// 3 轮共 6 条日志：每页 4 条分两页，再按降序读最新的 2 条。
			name: "list_items_paged",
			run: func(t *testing.T, h *clientHarness) {
				// 1. 完成三轮，准备每页四条的日志查询。
				conversation := h.openNewConversation(t)
				h.completeRounds(t, conversation, "一", "二", "三")
				request := &runtimeclient.ListItemsRequest{
					ConversationID: conversation.ID,
					SessionID:      conversation.RootSessionID,
					Limit:          4,
				}
				// 2. 读取两页再倒序读最新两条，核对流水号和游标。
				first, err := h.client.ListItems(t.Context(), request)
				if err != nil {
					t.Fatal(err)
				}
				if got := getOrdinals(first.Items); !slices.Equal(got, []storage.Ordinal{1, 2, 3, 4}) || first.NextCursor != 4 {
					t.Fatalf("first page = %v next %d, want [1 2 3 4] and cursor 4", got, first.NextCursor)
				}
				request.Cursor = first.NextCursor
				second, err := h.client.ListItems(t.Context(), request)
				if err != nil {
					t.Fatal(err)
				}
				if got := getOrdinals(second.Items); !slices.Equal(got, []storage.Ordinal{5, 6}) || second.NextCursor != 0 {
					t.Fatalf("second page = %v next %d, want [5 6] and a zero cursor", got, second.NextCursor)
				}
				request.Cursor, request.Order, request.Limit = 0, storage.DirectionDesc, 2
				latest, err := h.client.ListItems(t.Context(), request)
				if err != nil {
					t.Fatal(err)
				}
				if got := getOrdinals(latest.Items); !slices.Equal(got, []storage.Ordinal{6, 5}) {
					t.Fatalf("descending page = %v, want [6 5]", got)
				}
			},
		},
		{
			name: "list_turns_paged",
			run: func(t *testing.T, h *clientHarness) {
				// 1. 完成三轮，准备每页两轮的查询。
				conversation := h.openNewConversation(t)
				turnIDs := h.completeRounds(t, conversation, "一", "二", "三")
				request := &runtimeclient.ListTurnsRequest{
					ConversationID: conversation.ID,
					SessionID:      conversation.RootSessionID,
					Limit:          2,
				}
				// 2. 读取两页，核对 Turn ID、游标、状态和 Item 范围。
				first, err := h.client.ListTurns(t.Context(), request)
				if err != nil {
					t.Fatal(err)
				}
				if got := getTurnIDs(first.Turns); !slices.Equal(got, turnIDs[:2]) || first.NextCursor == (dao.RecordCursor[defs.TurnID]{}) {
					t.Fatalf("first page = %v next %+v, want %v and a cursor", got, first.NextCursor, turnIDs[:2])
				}
				request.Cursor = first.NextCursor
				second, err := h.client.ListTurns(t.Context(), request)
				if err != nil {
					t.Fatal(err)
				}
				if got := getTurnIDs(second.Turns); !slices.Equal(got, turnIDs[2:]) || second.NextCursor != (dao.RecordCursor[defs.TurnID]{}) {
					t.Fatalf("second page = %v next %+v, want %v and a zero cursor", got, second.NextCursor, turnIDs[2:])
				}
				if second.Turns[0].Status != defs.TurnStatusCompleted || second.Turns[0].SessionID != conversation.RootSessionID || second.Turns[0].ItemsRange != (defs.TurnItemRange{
					FirstOrdinal: 5,
					LastOrdinal:  6,
				}) {
					t.Fatalf("turn record = %+v", second.Turns[0])
				}
			},
		},
		{
			name: "get_turns",
			run: func(t *testing.T, h *clientHarness) {
				// 1. 完成三轮，按乱序 ID 和不存在 ID 查询。
				conversation := h.openNewConversation(t)
				turnIDs := h.completeRounds(t, conversation, "一", "二", "三")
				turns, err := h.client.GetTurns(t.Context(), &runtimeclient.GetTurnsRequest{
					ConversationID: conversation.ID,
					SessionID:      conversation.RootSessionID,
					TurnIDs:        []defs.TurnID{turnIDs[2], "missing", turnIDs[0]},
				})
				if err != nil {
					t.Fatal(err)
				}
				// 2. 核对按请求顺序返回已有 Turn，状态和 Item 范围正确。
				if got := getTurnIDs(turns); !slices.Equal(got, []defs.TurnID{turnIDs[2], turnIDs[0]}) {
					t.Fatalf("GetTurns() = %v, want %v in request order without the missing one", got, []defs.TurnID{turnIDs[2], turnIDs[0]})
				}
				if turns[0].Status != defs.TurnStatusCompleted || turns[0].SessionID != conversation.RootSessionID || turns[0].ItemsRange != (defs.TurnItemRange{
					FirstOrdinal: 5,
					LastOrdinal:  6,
				}) || turns[1].ItemsRange != (defs.TurnItemRange{
					FirstOrdinal: 1,
					LastOrdinal:  2,
				}) {
					t.Fatalf("turn record = %+v", turns[0])
				}
			},
		},
		{
			name: "compact_context_when_idle",
			run: func(t *testing.T, h *clientHarness) {
				// 1. 完成普通轮次后请求压缩上下文。
				conversation := h.openNewConversation(t)
				h.completeRounds(t, conversation, "第一轮")
				turnID, err := h.client.CompactContext(t.Context(), conversation.ID)
				if err != nil || turnID == "" {
					t.Fatalf("CompactContext() = %q, %v; want a turn id", turnID, err)
				}
				// 2. 回复压缩模型，核对完成事件使用返回的 Turn ID。
				h.model.receive(t).reply <- clientModelResponse{chunks: []string{"摘要"}}
				events := h.collectEvents(t, isTurnFinished)
				if finished := events[len(events)-1].(*runtimeclient.TurnFinished); finished.TurnID != turnID || finished.Status != defs.TurnStatusCompleted {
					t.Fatalf("turn finished = %+v, want completed compaction turn %s", finished, turnID)
				}
			},
		},
		{
			name: "get_conversation_without_opening",
			run: func(t *testing.T, h *clientHarness) {
				// 1. 只写入对话记录，不创建运行实例。
				stored := defs.Conversation{
					ID:            defs.NewConversationID(),
					RootSessionID: defs.NewSessionID(),
					Title:         "未打开",
				}
				if err := h.mutation.SetConversation(t.Context(), stored); err != nil {
					t.Fatal(err)
				}
				// 2. 读取记录后核对对话仍未打开，未知对话返回 ErrConversationNotFound。
				got, err := h.client.GetConversation(t.Context(), stored.ID)
				if err != nil || got.ID != stored.ID || got.RootSessionID != stored.RootSessionID || got.Title != stored.Title {
					t.Fatalf("GetConversation() = %+v, %v; want %+v", got, err, stored)
				}
				isOpen, err := h.runtime.IsConversationOpen(t.Context(), &runtime.GetConversationRequest{ConversationID: stored.ID})
				if err != nil || isOpen {
					t.Fatalf("conversation open = %v, %v after GetConversation, want not open", isOpen, err)
				}
				if _, err := h.client.GetConversation(t.Context(), "missing-conversation"); !errors.Is(err, runtime.ErrConversationNotFound) {
					t.Fatalf("GetConversation(missing) error = %v, want %v", err, runtime.ErrConversationNotFound)
				}
			},
		},
		{
			// 根 Session 与它 spawn 的子 Session 都在列表里，每页 1 条分两页。
			name: "list_sessions_root_and_child",
			run: func(t *testing.T, h *clientHarness) {
				// 1. 创建 review 子 Agent 并按每页一条查询 Session。
				conversation := h.openNewConversation(t)
				child := h.delegateReview(t, conversation)
				request := &runtimeclient.ListSessionsRequest{
					ConversationID: conversation.ID,
					Limit:          1,
				}
				// 2. 核对首末页分别为根子 Session，游标和任务层级正确。
				first, err := h.client.ListSessions(t.Context(), request)
				if err != nil {
					t.Fatal(err)
				}
				if len(first.Sessions) != 1 || first.Sessions[0].ID != conversation.RootSessionID || first.NextCursor == (dao.RecordCursor[defs.SessionID]{}) {
					t.Fatalf("first page = %+v next %+v, want the root Session and a cursor", first.Sessions, first.NextCursor)
				}
				request.Cursor = first.NextCursor
				second, err := h.client.ListSessions(t.Context(), request)
				if err != nil {
					t.Fatal(err)
				}
				if len(second.Sessions) != 1 || second.Sessions[0] != child || second.NextCursor != (dao.RecordCursor[defs.SessionID]{}) {
					t.Fatalf("second page = %+v next %+v, want the child Session %+v and a zero cursor", second.Sessions, second.NextCursor, child)
				}
				if child.TaskName != "review" || child.ParentSessionID != conversation.RootSessionID {
					t.Fatalf("child Session = %+v, want task review under the root", child)
				}
			},
		},
		{
			// 只凭子 SessionID 读到它的记录和所属对话，不需要先打开对话。
			name: "get_session_by_id",
			run: func(t *testing.T, h *clientHarness) {
				// 1. 创建 review 子 Agent。
				conversation := h.openNewConversation(t)
				child := h.delegateReview(t, conversation)
				// 2. 只凭子 Session ID 查询，核对所属对话、父 Session 和任务名。
				got, err := h.client.GetSessionByID(t.Context(), child.ID)
				if err != nil || got.ID != child.ID || got.ConversationID != conversation.ID || got.ParentSessionID != conversation.RootSessionID || got.TaskName != "review" {
					t.Fatalf("GetSessionByID(child) = %+v, %v, want the review Session %+v", got, err, child)
				}
			},
		},
	}
	// 2. 为每个场景启动真实链路，执行请求并检查事件和存储结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, startClientHarness(t, clientTestToken))
		})
	}
}

// TestClientWorkspaceDirectories 检查 Go 客户端经真实服务端读取目录候选和校验手填目录。
func TestClientWorkspaceDirectories(t *testing.T) {
	// 1. 通过 Go 客户端读取目录、空候选和错误响应。
	for _, tt := range []struct {
		name, executorID, directory, baseDirectory, wantDirectory string
		wantDirectories                                           []string
		wantInvalid                                               bool
	}{
		{name: "direct_directories", executorID: "local", directory: ".", wantDirectory: "{root}", wantDirectories: []string{"alpha", "zeta"}},
		{name: "base_directory", executorID: "local", directory: "..", baseDirectory: "{root}/alpha", wantDirectory: "{root}", wantDirectories: []string{"alpha", "zeta"}},
		{name: "empty_directory", executorID: "local", directory: "zeta", wantDirectory: "{root}/zeta", wantDirectories: []string{}},
		{name: "typed_symlink", executorID: "local", directory: "link", wantDirectory: "{root}/link", wantDirectories: []string{"nested"}},
		{name: "unknown_executor", executorID: "gone", wantInvalid: true},
		{name: "virtual_executor", executorID: "openvaala", wantInvalid: true},
		{name: "missing_directory", executorID: "local", directory: "missing", wantInvalid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := startClientHarness(t, clientTestToken)
			root := t.TempDir()
			for _, directory := range []string{"alpha/nested", "zeta"} {
				if err := os.MkdirAll(filepath.Join(root, directory), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(filepath.Join(root, "alpha"), filepath.Join(root, "link")); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "file"), []byte("data"), 0600); err != nil {
				t.Fatal(err)
			}
			replacer := strings.NewReplacer("{root}", root)
			baseDirectory := root
			if tt.baseDirectory != "" {
				baseDirectory = replacer.Replace(tt.baseDirectory)
			}
			// 2. 核对返回的目录和排序候选，设置错误保留 JSON-RPC 参数错误码。
			got, err := h.client.GetWorkspaceDirectories(t.Context(), &runtimeclient.GetWorkspaceDirectoriesRequest{
				ExecutorID:    tt.executorID,
				Directory:     tt.directory,
				BaseDirectory: baseDirectory,
			})
			if tt.wantInvalid {
				var responseError *runtimeclient.ResponseError
				if !errors.As(err, &responseError) || responseError.Code != runtimeprotocol.ErrorCodeInvalidParams {
					t.Fatalf("GetWorkspaceDirectories = %+v, %v; want invalid params", got, err)
				}
				// 3. 补全失败后手填有效目录仍可正常校验。
				if _, err := h.client.ResolveWorkspaceDirectory(t.Context(), &runtimeclient.ResolveWorkspaceDirectoryRequest{
					ExecutorID:    "local",
					Directory:     "link",
					BaseDirectory: root,
				}); err != nil {
					t.Fatalf("manual directory validation after failed candidates: %v", err)
				}
				return
			}
			if err != nil || got.Directory != replacer.Replace(tt.wantDirectory) || !reflect.DeepEqual(got.Directories, tt.wantDirectories) {
				t.Fatalf("GetWorkspaceDirectories = %+v, %v; want %q, %v", got, err, replacer.Replace(tt.wantDirectory), tt.wantDirectories)
			}
		})
	}
}

// TestClientTerminal 检查终端的打开、输入、尺寸、输出事件、关闭和不支持终端的 Executor。
func TestClientTerminal(t *testing.T) {
	// 1. 列出可交互 shell 和 virtual Executor 两个场景。
	for _, tt := range []struct {
		name    string
		request *runtimeclient.TerminalOpenRequest
		// wantOpenError 非空时打开应失败并包含这段文字。
		wantOpenError string
	}{
		{name: "local_shell_roundtrip", request: &runtimeclient.TerminalOpenRequest{Rows: 24, Cols: 80}},
		{name: "virtual_executor_rejected", request: &runtimeclient.TerminalOpenRequest{ExecutorID: "openvaala"}, wantOpenError: "does not support interactive commands"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SHELL", "/bin/bash")
			h := startClientHarness(t, clientTestToken)
			// 2. 打开终端；不支持时核对错误后结束。
			opened, err := h.client.OpenTerminal(t.Context(), tt.request)
			if tt.wantOpenError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantOpenError) {
					t.Fatalf("OpenTerminal = %+v, %v; want error containing %q", opened, err, tt.wantOpenError)
				}
				return
			}
			if err != nil || opened.TerminalID == "" || opened.ExecutorID != defs.LocalExecutorID {
				t.Fatalf("OpenTerminal = %+v, %v", opened, err)
			}
			// 3. 写入命令得到输出，修改尺寸后程序读到新尺寸。
			if err := h.client.WriteTerminal(opened.TerminalID, []byte("echo hi-$((20+22))\n")); err != nil {
				t.Fatal(err)
			}
			h.waitTerminalOutput(t, opened.TerminalID, "hi-42")
			if err := h.client.ResizeTerminal(opened.TerminalID, 30, 100); err != nil {
				t.Fatal(err)
			}
			if err := h.client.WriteTerminal(opened.TerminalID, []byte("echo size=$(stty size)\n")); err != nil {
				t.Fatal(err)
			}
			h.waitTerminalOutput(t, opened.TerminalID, "size=30 100")
			// 4. 关闭后收到该终端的 TerminalExited，终端已结束时再次关闭仍成功。
			if err := h.client.CloseTerminal(t.Context(), opened.TerminalID); err != nil {
				t.Fatal(err)
			}
			h.collectEvents(t, func(event runtimeclient.ClientEvent) bool {
				exited, ok := event.(*runtimeclient.TerminalExited)
				return ok && exited.TerminalID == opened.TerminalID
			})
			if err := h.client.CloseTerminal(t.Context(), opened.TerminalID); err != nil {
				t.Fatalf("CloseTerminal after exit = %v", err)
			}
		})
	}
}

// waitTerminalOutput 累计终端的输出事件直到包含 want，返回最后一批输出之后的位置。
func (h *clientHarness) waitTerminalOutput(t *testing.T, terminalID, want string) (nextCursor uint64) {
	t.Helper()
	// 1. 拼接该终端的 TerminalOutput 数据，直到累计输出包含目标文本。
	var output strings.Builder
	h.collectEvents(t, func(event runtimeclient.ClientEvent) bool {
		if terminalOutput, ok := event.(*runtimeclient.TerminalOutput); ok && terminalOutput.TerminalID == terminalID {
			output.Write(terminalOutput.Data)
			nextCursor = terminalOutput.NextCursor
		}
		return strings.Contains(output.String(), want)
	})
	return nextCursor
}

// waitTerminals 等到终端列表已知且满足条件，返回这份列表。
func (h *clientHarness) waitTerminals(t *testing.T, match func(terminals []runtimeprotocol.TerminalItem) bool) []runtimeprotocol.TerminalItem {
	t.Helper()
	events := h.collectEvents(t, func(event runtimeclient.ClientEvent) bool {
		changed, ok := event.(*runtimeclient.TerminalsChanged)
		return ok && changed.IsKnown && match(changed.Terminals)
	})
	return events[len(events)-1].(*runtimeclient.TerminalsChanged).Terminals
}

// hasTerminal 列表里是否有 terminalID 这个终端？
func hasTerminal(terminals []runtimeprotocol.TerminalItem, terminalID string) bool {
	return slices.ContainsFunc(terminals, func(item runtimeprotocol.TerminalItem) bool { return item.TerminalID == terminalID })
}

// TestClientTerminals 检查终端列表订阅、断线后接回和重置：初始列表、列表变化、serve 关闭订阅后恢复、终端在断线后继续运行并从已收到的位置续读。
func TestClientTerminals(t *testing.T) {
	for _, action := range []string{"live", "reconnect", "list_closed"} {
		t.Run(action, func(t *testing.T) {
			t.Setenv("SHELL", "/bin/bash")
			h := startClientHarness(t, clientTestToken)
			h.collectEvents(t, isConnectionState(runtimeclient.ConnectionConnected))
			// 1. 重复订阅合并为一次请求，初始列表已知且为空。
			for range 2 {
				if err := h.client.SubscribeTerminals(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if initial := h.waitTerminals(t, func([]runtimeprotocol.TerminalItem) bool { return true }); len(initial) != 0 {
				t.Fatalf("initial terminals = %v", initial)
			}
			subscriptions := 0
			for _, frame := range h.proxy.getUpstreamFrames() {
				if strings.Contains(frame, `"method":"terminal/subscribe"`) {
					subscriptions++
				}
			}
			if subscriptions != 1 {
				t.Fatalf("duplicate subscriptions = %d", subscriptions)
			}
			// 2. 打开带对话的终端后，列表交付它，并带着打开它的对话。
			opened, err := h.client.OpenTerminal(t.Context(), &runtimeclient.TerminalOpenRequest{Rows: 24, Cols: 80, ConversationID: "conversation-1"})
			if err != nil {
				t.Fatal(err)
			}
			listed := h.waitTerminals(t, func(terminals []runtimeprotocol.TerminalItem) bool { return hasTerminal(terminals, opened.TerminalID) })
			if len(listed) != 1 || listed[0].ConversationID != "conversation-1" || listed[0].ExecutorID != defs.LocalExecutorID {
				t.Fatalf("listed = %+v", listed)
			}
			if err := h.client.WriteTerminal(opened.TerminalID, []byte("echo hi-$((20+22))\n")); err != nil {
				t.Fatal(err)
			}
			cursor := h.waitTerminalOutput(t, opened.TerminalID, "hi-42")
			// 3. 断线或订阅被关闭后列表先变成未知，恢复后终端仍在列表里。
			switch action {
			case "reconnect":
				h.proxy.cut()
			case "list_closed":
				h.proxy.injectDownstream(t, map[string]any{"method": "terminal/listClosed", "params": map[string]string{"reason": "test closed"}})
			}
			if action != "live" {
				h.collectEvents(t, func(event runtimeclient.ClientEvent) bool {
					changed, ok := event.(*runtimeclient.TerminalsChanged)
					return ok && !changed.IsKnown && changed.Err != nil
				})
				h.waitTerminals(t, func(terminals []runtimeprotocol.TerminalItem) bool { return hasTerminal(terminals, opened.TerminalID) })
			}
			// 4. 重连后新连接没有接入，从已收到的位置接回：第一批从该位置接着发且不截断，之后的输出继续到达；从 0 接回则重放全部输出。
			if action == "reconnect" {
				attached, err := h.client.AttachTerminal(t.Context(), opened.TerminalID, cursor)
				if err != nil || attached.ExecutorID != defs.LocalExecutorID || attached.Rows != 24 || attached.Cols != 80 {
					t.Fatalf("AttachTerminal = %+v, %v", attached, err)
				}
				events := h.collectEvents(t, func(event runtimeclient.ClientEvent) bool {
					terminalOutput, ok := event.(*runtimeclient.TerminalOutput)
					return ok && terminalOutput.TerminalID == opened.TerminalID
				})
				first := events[len(events)-1].(*runtimeclient.TerminalOutput)
				if first.NextCursor != cursor+uint64(len(first.Data)) || first.Truncated || first.HasMore || strings.Contains(string(first.Data), "hi-42") {
					t.Fatalf("first batch after reconnect = %+v", first)
				}
				if err := h.client.WriteTerminal(opened.TerminalID, []byte("echo again-$((1+1))\n")); err != nil {
					t.Fatal(err)
				}
				h.waitTerminalOutput(t, opened.TerminalID, "again-2")
				if _, err := h.client.AttachTerminal(t.Context(), opened.TerminalID, 0); err != nil {
					t.Fatal(err)
				}
				h.waitTerminalOutput(t, opened.TerminalID, "hi-42")
			}
			// 5. 重置后接入的连接收到清屏序列；关闭后列表不再有它；终端不存在时接入和重置返回 -32004。
			if err := h.client.ResetTerminal(t.Context(), opened.TerminalID); err != nil {
				t.Fatal(err)
			}
			h.waitTerminalOutput(t, opened.TerminalID, "\x1b[2J")
			if err := h.client.CloseTerminal(t.Context(), opened.TerminalID); err != nil {
				t.Fatal(err)
			}
			h.waitTerminals(t, func(terminals []runtimeprotocol.TerminalItem) bool { return !hasTerminal(terminals, opened.TerminalID) })
			for name, call := range map[string]func() error{
				"attach": func() error {
					_, err := h.client.AttachTerminal(t.Context(), "missing", 0)
					return err
				},
				"reset": func() error { return h.client.ResetTerminal(t.Context(), "missing") },
			} {
				var responseErr *runtimeclient.ResponseError
				if err := call(); !errors.As(err, &responseErr) || responseErr.Code != runtimeprotocol.ErrorCodeTerminalNotFound {
					t.Fatalf("%s missing terminal = %v", name, err)
				}
			}
		})
	}
}

// TestClientConfigurationMethods 检查模型配置和模型目录请求的参数及结果。
func TestClientConfigurationMethods(t *testing.T) {
	// 1. 准备默认模型、配置写入结果及预期调用记录。
	noBusyConversations := runtime.SetDefinitionsResult{BusyConversationIDs: []defs.ConversationID{}}
	ref := defs.ModelRef{
		ProviderID: "gw",
		ModelID:    "m1",
	}
	tests := []struct {
		name string
		// call 调用一个方法，返回它的结果和错误。
		call func(t *testing.T, h *clientHarness) (any, error)
		// want 为 nil 时结果由 call 自己检查。
		want      any
		wantErr   string
		wantCalls []clientTestCall
	}{
		{
			// 默认模型在协议里是 provider/model 文本，客户端还原成模型引用。
			name: "list_providers",
			call: func(t *testing.T, h *clientHarness) (any, error) {
				providers, defaultModel, defaults, err := h.client.ListProviders(t.Context())
				return clientTestProviderList{
					providers:    providers,
					defaultModel: defaultModel,
					defaults:     defaults,
				}, err
			},
			want: clientTestProviderList{
				providers:    []defs.ProviderConfig{clientTestProvider},
				defaultModel: ref,
				defaults:     clientTestModelDefaults,
			},
			wantCalls: []clientTestCall{{method: "ListProviders"}},
		},
		{
			name: "set_provider",
			call: func(t *testing.T, h *clientHarness) (any, error) {
				return h.client.SetProvider(t.Context(), clientTestProvider, defs.ModelRef{})
			},
			want: noBusyConversations,
			wantCalls: []clientTestCall{{
				method:   "SetProvider",
				argument: clientTestProviderWrite{provider: clientTestProvider},
			}},
		},
		{
			// 同一次 provider/set 带上默认模型，serve 收到的是解析好的模型引用。
			name: "set_provider_with_default_model",
			call: func(t *testing.T, h *clientHarness) (any, error) {
				return h.client.SetProvider(t.Context(), clientTestProvider, ref)
			},
			want: noBusyConversations,
			wantCalls: []clientTestCall{{
				method: "SetProvider",
				argument: clientTestProviderWrite{
					provider:     clientTestProvider,
					defaultModel: ref,
				},
			}},
		},
		{
			name: "delete_provider",
			call: func(t *testing.T, h *clientHarness) (any, error) { return h.client.DeleteProvider(t.Context(), "gw") },
			want: noBusyConversations,
			wantCalls: []clientTestCall{{
				method:   "DeleteProvider",
				argument: "gw",
			}},
		},
		{
			name: "delete_missing_provider",
			call: func(t *testing.T, h *clientHarness) (any, error) {
				return h.client.DeleteProvider(t.Context(), "other")
			},
			want:    runtime.SetDefinitionsResult{},
			wantErr: "provider other is not configured",
			wantCalls: []clientTestCall{{
				method:   "DeleteProvider",
				argument: "other",
			}},
		},
		{
			name: "set_default_model",
			call: func(t *testing.T, h *clientHarness) (any, error) { return h.client.SetDefaultModel(t.Context(), ref) },
			want: noBusyConversations,
			wantCalls: []clientTestCall{{
				method:   "SetDefaultModel",
				argument: ref,
			}},
		},
		{
			// 根 Session 正在等模型回复：这次 reload 不让对话换上新配置，结果里列出这个对话。
			name: "reload_config_with_running_conversation",
			call: func(t *testing.T, h *clientHarness) (any, error) {
				conversation := h.openNewConversation(t)
				h.submit(t, conversation, "你好")
				request := h.model.receive(t)
				result, err := h.client.ReloadConfig(t.Context())
				request.reply <- clientModelResponse{chunks: []string{"好"}}
				h.collectEvents(t, isTurnFinished)
				if err != nil {
					return nil, err
				}
				want := runtime.SetDefinitionsResult{BusyConversationIDs: []defs.ConversationID{conversation.ID}}
				if !reflect.DeepEqual(result, want) {
					t.Fatalf("ReloadConfig() = %+v, want %+v", result, want)
				}
				return result, nil
			},
			wantCalls: []clientTestCall{{method: "Reload"}},
		},
		{
			name:      "list_catalog_providers",
			call:      func(t *testing.T, h *clientHarness) (any, error) { return h.client.ListCatalogProviders(t.Context()) },
			want:      []defs.CatalogProvider{clientTestCatalogProvider},
			wantCalls: []clientTestCall{{method: "ListCatalogProviders"}},
		},
		{
			name: "list_catalog_models",
			call: func(t *testing.T, h *clientHarness) (any, error) {
				return h.client.ListCatalogModels(t.Context(), "openai")
			},
			want: clientTestProvider.Models,
			wantCalls: []clientTestCall{{
				method:   "ListCatalogModels",
				argument: "openai",
			}},
		},
		{
			name: "fetch_provider_models",
			call: func(t *testing.T, h *clientHarness) (any, error) {
				return h.client.FetchProviderModels(t.Context(), clientTestProvider)
			},
			want: clientTestProvider.Models,
			wantCalls: []clientTestCall{{
				method:   "FetchProviderModels",
				argument: clientTestProvider,
			}},
		},
	}
	// 2. 逐项发送请求，核对调用参数、忙碌对话、返回结果和错误原文。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := startClientHarness(t, clientTestToken)
			got, err := tt.call(t, h)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if tt.want != nil && !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("result = %+v, want %+v", got, tt.want)
			}
			if calls := h.configurationCalls.drain(); !reflect.DeepEqual(calls, tt.wantCalls) {
				t.Fatalf("calls = %+v, want %+v", calls, tt.wantCalls)
			}
		})
	}
}

// TestClientConversationSynced 检查首次和重新同步的对话记录及所跟随 Session。
func TestClientConversationSynced(t *testing.T) {
	// 1. 列出首次打开、修改目录后同步和跟随子 Session 的场景。
	tests := []struct {
		name string
		run  func(t *testing.T, h *clientHarness, holding *resyncHoldingRuntime)
	}{
		{
			name: "open",
			run: func(t *testing.T, h *clientHarness, _ *resyncHoldingRuntime) {
				// 1. 创建并打开新对话。
				created, err := h.client.CreateConversation(t.Context(), &runtimeclient.CreateConversationRequest{AgentName: "assistant"})
				if err != nil {
					t.Fatal(err)
				}
				opened, err := h.client.OpenConversation(t.Context(), &runtimeclient.OpenConversationRequest{ConversationID: created.ID})
				if err != nil {
					t.Fatal(err)
				}
				// 2. 核对 ConversationSynced 携带打开返回的记录。
				if synced := h.waitConversationSynced(t); !reflect.DeepEqual(synced.Conversation, opened) {
					t.Fatalf("synced conversation = %+v, want the opened record %+v", synced.Conversation, opened)
				}
			},
		},
		{
			name: "resync_after_settings_change",
			run: func(t *testing.T, h *clientHarness, _ *resyncHoldingRuntime) {
				// 1. 修改已打开对话的 local 目录设置。
				conversation := h.openNewConversation(t)
				settings := defs.ExecutorSettings{WorkspaceDirectories: map[string]string{defs.LocalExecutorID: t.TempDir()}}
				saved, err := h.client.SetConversationExecutorSettings(t.Context(), conversation.ID, settings)
				if err != nil || !reflect.DeepEqual(saved.ExecutorSettings, settings) {
					t.Fatalf("SetConversationExecutorSettings() = %+v, %v; want settings %+v", saved.ExecutorSettings, err, settings)
				}
				// 2. 核对重新同步的对话记录携带新目录。
				synced := h.waitConversationSynced(t)
				if synced.Conversation.ID != conversation.ID || !reflect.DeepEqual(synced.Conversation.ExecutorSettings, settings) {
					t.Fatalf("resynced conversation = %+v, want %s with executor_settings %+v", synced.Conversation, conversation.ID, settings)
				}
			},
		},
		{
			// 重新同步被扣在 conversation/open 期间，根 Session 又向子 Agent 发了一轮：重新同步仍订阅子 Session，
			// SessionSynced 补齐子 Session 在重新同步期间保存的 Item。
			name: "resync_follows_child_after_settings_change",
			run: func(t *testing.T, h *clientHarness, holding *resyncHoldingRuntime) {
				// 1. 创建子任务，切换到跟随子 Session 并记录已保存位置。
				conversation := h.openNewConversation(t)
				child := h.delegateReview(t, conversation)
				request := &runtimeclient.OpenConversationRequest{
					ConversationID: conversation.ID,
					SessionID:      child.ID,
				}
				if _, err := h.client.OpenConversation(t.Context(), request); err != nil {
					t.Fatal(err)
				}
				opened := h.waitSessionSynced(t)
				// 2. 扣留修改目录触发的重新打开，在期间完成子任务的新一轮后放行。
				hold := &resyncHold{
					entered: make(chan struct{}),
					release: make(chan struct{}),
				}
				holding.hold.Store(hold)
				settings := defs.ExecutorSettings{WorkspaceDirectories: map[string]string{defs.LocalExecutorID: t.TempDir()}}
				if _, err := h.client.SetConversationExecutorSettings(t.Context(), conversation.ID, settings); err != nil {
					t.Fatal(err)
				}
				waitClosed(t, hold.entered, "resync conversation/open")
				h.submitDirectly(t, conversation, "再来一轮")
				h.replyReviewRound(t, child)
				close(hold.release)
				// 3. 核对同步仍跟随子 Session，补齐新 Item，且对话带新目录。
				events := h.collectEvents(t, isSessionSynced)
				resynced := events[len(events)-1].(*runtimeclient.SessionSynced)
				tail, err := h.client.ListItems(t.Context(), &runtimeclient.ListItemsRequest{
					ConversationID: conversation.ID,
					SessionID:      child.ID,
					Cursor:         opened.LastSavedOrdinal,
					Limit:          100,
				})
				if err != nil {
					t.Fatal(err)
				}
				wantOrdinals := getOrdinals(tail.Items)
				if len(wantOrdinals) == 0 || wantOrdinals[0] != opened.LastSavedOrdinal+1 ||
					!slices.Equal(getOrdinals(resynced.Items), wantOrdinals) || resynced.LastSavedOrdinal != wantOrdinals[len(wantOrdinals)-1] {
					t.Fatalf("resynced items = %v (last saved %d), want the child's items %v saved after %d",
						getOrdinals(resynced.Items), resynced.LastSavedOrdinal, wantOrdinals, opened.LastSavedOrdinal)
				}
				var synced *runtimeclient.ConversationSynced
				for _, event := range events {
					if candidate, ok := event.(*runtimeclient.ConversationSynced); ok {
						synced = candidate
					}
				}
				if synced == nil || !reflect.DeepEqual(synced.Conversation.ExecutorSettings, settings) {
					t.Fatalf("resynced conversation = %+v, want executor_settings %+v", synced, settings)
				}
			},
		},
	}
	// 2. 启动真实客户端链路，逐项执行并核对结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var holding *resyncHoldingRuntime
			h := startClientHarness(t, clientTestToken, func(opt *runtimeserver.RuntimeServerOpt) {
				holding = &resyncHoldingRuntime{Runtime: opt.Runtime}
				opt.Runtime = holding
			})
			tt.run(t, h, holding)
		})
	}
}

// TestClientConversationTitle 检查自动标题、手动改名和断线后的标题恢复。
func TestClientConversationTitle(t *testing.T) {
	// 1. 准备手动改名、清空和 Unicode 长度限制场景。
	tests := []struct {
		name  string
		title string
		want  string
	}{
		{
			name:  "rename",
			title: "手动标题",
			want:  "手动标题",
		},
		{name: "clear"},
		{
			name:  "unicode_limit",
			title: strings.Repeat("中😀", 150),
			want:  strings.Repeat("中😀", 128),
		},
	}
	// 2. 提交输入检查自动标题，手动修改后核对通知，离线改名后重连核对同步记录。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := startClientHarness(t, clientTestToken)
			conversation := h.openNewConversation(t)
			h.submit(t, conversation, "\n 修复\t 登录\n附加描述")
			events := h.collectEvents(t, func(event runtimeclient.ClientEvent) bool {
				_, ok := event.(*runtimeclient.ConversationTitleChanged)
				return ok
			})
			changed := events[len(events)-1].(*runtimeclient.ConversationTitleChanged)
			if changed.ConversationID != conversation.ID || changed.Title != "修复 登录" {
				t.Fatalf("automatic title = %+v", changed)
			}
			h.model.receive(t).reply <- clientModelResponse{chunks: []string{"好"}}
			h.collectEvents(t, isTurnFinished)
			if err := h.client.SetConversationTitle(t.Context(), conversation.ID, tt.title); err != nil {
				t.Fatal(err)
			}
			events = h.collectEvents(t, func(event runtimeclient.ClientEvent) bool {
				_, ok := event.(*runtimeclient.ConversationTitleChanged)
				return ok
			})
			changed = events[len(events)-1].(*runtimeclient.ConversationTitleChanged)
			if changed.ConversationID != conversation.ID || changed.Title != tt.want {
				t.Fatalf("explicit title = %+v, want %q", changed, tt.want)
			}
			h.proxy.pause()
			h.proxy.cut()
			h.collectEvents(t, isConnectionState(runtimeclient.ConnectionReconnecting))
			if err := h.runtime.SetConversationTitle(t.Context(), &runtime.SetConversationTitleRequest{
				ConversationID: conversation.ID,
				Title:          "离线时改名",
			}); err != nil {
				t.Fatal(err)
			}
			h.proxy.resume()
			synced := h.waitConversationSynced(t)
			if synced.Conversation.ID != conversation.ID || synced.Conversation.Title != "离线时改名" {
				t.Fatalf("resynced conversation = %+v", synced.Conversation)
			}
		})
	}
}

// resyncHold 是一次对 conversation/open 的扣留：处理函数到达时关闭 entered，等 release 关闭后才继续。
type resyncHold struct {
	entered chan struct{}
	release chan struct{}
}

// resyncHoldingRuntime 在 hold 非空时扣留下一次打开对话请求，等待 release 关闭或请求取消。
type resyncHoldingRuntime struct {
	runtime.Runtime
	hold atomic.Pointer[resyncHold]
}

// OpenConversation 扣留下一次打开请求，放行后调用 Runtime。
func (r *resyncHoldingRuntime) OpenConversation(ctx context.Context, request *runtime.OpenConversationRequest) (defs.Conversation, error) {
	// 1. 取走本次扣留并通知请求到达，等待放行或取消，再打开对话。
	if hold := r.hold.Swap(nil); hold != nil {
		close(hold.entered)
		select {
		case <-hold.release:
		case <-ctx.Done():
			return defs.Conversation{}, ctx.Err()
		}
	}
	return r.Runtime.OpenConversation(ctx, request)
}

// TestClientFollowSession 检查内容订阅只交付所跟随 Session 的事件。
func TestClientFollowSession(t *testing.T) {
	// 1. 列出跟随子 Session 和 SessionID 为空时跟随根 Session 的场景。
	tests := []struct {
		name        string
		followChild bool
		// wantTurns 是 replyReviewRound 里被跟随的 Session 完成的 Turn 数：子 Session 一个，根 Session 两个（转达与接收子任务结果）。
		wantTurns int
	}{
		{
			name:        "child_session",
			followChild: true,
			wantTurns:   1,
		},
		{
			name:        "root_session_when_empty",
			followChild: false,
			wantTurns:   2,
		},
	}
	// 2. 分别核对首次历史、后续内容事件和保存流水号属于所跟随 Session。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := startClientHarness(t, clientTestToken)
			conversation := h.openNewConversation(t)
			child := h.delegateReview(t, conversation)
			request := &runtimeclient.OpenConversationRequest{ConversationID: conversation.ID}
			followed, other := conversation.RootSessionID, child.ID
			if tt.followChild {
				request.SessionID = child.ID
				followed, other = child.ID, conversation.RootSessionID
			}
			if _, err := h.client.OpenConversation(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			synced := h.waitSessionSynced(t)
			history, err := h.client.ListItems(t.Context(), &runtimeclient.ListItemsRequest{
				ConversationID: conversation.ID,
				SessionID:      followed,
				Limit:          100,
			})
			if err != nil {
				t.Fatal(err)
			}
			historyOrdinals := getOrdinals(history.Items)
			if !slices.Equal(getOrdinals(synced.Items), historyOrdinals) || synced.LastSavedOrdinal != historyOrdinals[len(historyOrdinals)-1] {
				t.Fatalf("synced items = %v (last saved %d), want the followed Session's history %v", getOrdinals(synced.Items), synced.LastSavedOrdinal, historyOrdinals)
			}

			h.submit(t, conversation, "再来一轮")
			h.replyReviewRound(t, child)
			events := h.collectEvents(t, isTurnFinishedTimes(tt.wantTurns))
			followedTurns, otherTurns := h.getSessionTurnIDs(t, conversation.ID, followed), h.getSessionTurnIDs(t, conversation.ID, other)
			turnStarted, turnFinished, itemChanged := 0, 0, 0
			var savedOrdinals []storage.Ordinal
			checkTurn := func(event runtimeclient.ClientEvent, turnID defs.TurnID) {
				t.Helper()
				if getConversationID(event) != conversation.ID || !slices.Contains(followedTurns, turnID) || slices.Contains(otherTurns, turnID) {
					t.Fatalf("event %T of conversation %s turn %s is not from the followed Session %s", event, getConversationID(event), turnID, followed)
				}
			}
			for _, event := range events {
				switch event := event.(type) {
				case *runtimeclient.TurnStarted:
					turnStarted++
					checkTurn(event, event.TurnID)
				case *runtimeclient.TurnFinished:
					turnFinished++
					checkTurn(event, event.TurnID)
				case *runtimeclient.ItemChanged:
					itemChanged++
					checkTurn(event, event.TurnID)
				case *runtimeclient.ItemsSaved:
					checkTurn(event, event.TurnID)
					savedOrdinals = append(savedOrdinals, getOrdinals(event.Items)...)
				}
			}
			tail, err := h.client.ListItems(t.Context(), &runtimeclient.ListItemsRequest{
				ConversationID: conversation.ID,
				SessionID:      followed,
				Cursor:         synced.LastSavedOrdinal,
				Limit:          100,
			})
			if err != nil {
				t.Fatal(err)
			}
			if turnStarted != tt.wantTurns || turnFinished != tt.wantTurns || itemChanged == 0 || len(savedOrdinals) == 0 || !slices.Equal(savedOrdinals, getOrdinals(tail.Items)) {
				t.Fatalf("started=%d finished=%d item changes=%d saved=%v, want %d Turns of the followed Session saving %v",
					turnStarted, turnFinished, itemChanged, savedOrdinals, tt.wantTurns, getOrdinals(tail.Items))
			}
		})
	}
}

// TestClientSkipHistory 检查首次跳过历史和重连补齐。
func TestClientSkipHistory(t *testing.T) {
	// 1. 准备跳过与完整读取历史的预期流水号。
	tests := []struct {
		name              string
		skipHistory       bool
		wantFirstOrdinals []storage.Ordinal
	}{
		{
			name:              "skip_history",
			skipHistory:       true,
			wantFirstOrdinals: nil,
		},
		{
			name:              "full_history",
			skipHistory:       false,
			wantFirstOrdinals: []storage.Ordinal{1, 2, 3, 4},
		},
	}
	// 2. 检查首次同步内容，再检查新内容和离线期间保存内容均按流水号交付。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := startClientHarness(t, clientTestToken)
			conversation := h.openNewConversation(t)
			h.completeRounds(t, conversation, "第一轮", "第二轮")
			request := &runtimeclient.OpenConversationRequest{
				ConversationID: conversation.ID,
				SkipHistory:    tt.skipHistory,
			}
			if _, err := h.client.OpenConversation(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			first := h.waitSessionSynced(t)
			if got := getOrdinals(first.Items); !slices.Equal(got, tt.wantFirstOrdinals) || first.LastSavedOrdinal != 4 {
				t.Fatalf("first sync items = %v (last saved %d), want %v and last saved 4", got, first.LastSavedOrdinal, tt.wantFirstOrdinals)
			}

			h.submit(t, conversation, "第三轮")
			h.model.receive(t).reply <- clientModelResponse{chunks: []string{"好"}}
			var savedOrdinals []storage.Ordinal
			for _, event := range h.collectEvents(t, isTurnFinished) {
				if saved, ok := event.(*runtimeclient.ItemsSaved); ok {
					savedOrdinals = append(savedOrdinals, getOrdinals(saved.Items)...)
				}
			}
			if !slices.Equal(savedOrdinals, []storage.Ordinal{5, 6}) {
				t.Fatalf("saved ordinals after the sync = %v, want only the new items [5 6]", savedOrdinals)
			}

			// 断线期间保存的 Item：重连后的同步从上次交付的位置补齐，不再跳过。
			h.proxy.pause()
			h.proxy.cut()
			h.collectEvents(t, isConnectionState(runtimeclient.ConnectionReconnecting))
			h.submitDirectly(t, conversation, "离线时发送")
			h.model.receive(t).reply <- clientModelResponse{chunks: []string{"收到"}}
			h.waitTurnsCompleted(t, conversation.ID, conversation.RootSessionID, 3)
			h.proxy.resume()
			resynced := h.waitSessionSynced(t)
			if got := getOrdinals(resynced.Items); !slices.Equal(got, []storage.Ordinal{7, 8}) || resynced.LastSavedOrdinal != 8 {
				t.Fatalf("resync items = %v (last saved %d), want [7 8] and last saved 8", got, resynced.LastSavedOrdinal)
			}
		})
	}
}

// TestClientConversationSyncFailed 检查同步被拒绝和同步期间断线的处理。
func TestClientConversationSyncFailed(t *testing.T) {
	// 1. 准备服务端拒绝、对话删除和连接中断场景。
	tests := []struct {
		name string
		run  func(t *testing.T, h *clientHarness, refusing *openRefusingRuntime)
	}{
		{
			name: "serve_refuses_open",
			run: func(t *testing.T, h *clientHarness, refusing *openRefusingRuntime) {
				// 1. 打开对话后设置 Runtime 拒绝，注入订阅关闭。
				conversation := h.openNewConversation(t)
				refusing.isRefusing.Store(true)
				h.injectSubscriptionClosed(t, conversation)
				// 2. 核对同步失败事件后连接保留，模型查询成功且没有重复 initialize。
				events := h.collectEvents(t, func(event runtimeclient.ClientEvent) bool {
					_, ok := event.(*runtimeclient.ConversationSyncFailed)
					return ok
				})
				failed := events[len(events)-1].(*runtimeclient.ConversationSyncFailed)
				if failed.ConversationID != conversation.ID || failed.Err == nil || !strings.Contains(failed.Err.Error(), errOpenRefused.Error()) {
					t.Fatalf("sync failed = %+v, want conversation %s with the serve error %q", failed, conversation.ID, errOpenRefused)
				}
				if attempts := getReconnectAttempts(events); len(attempts) != 0 {
					t.Fatalf("reconnect attempts = %v, want the connection kept", attempts)
				}
				if _, err := h.client.ListModels(t.Context(), ""); err != nil {
					t.Fatal(err)
				}
				initializes := 0
				for _, frame := range h.proxy.getUpstreamFrames() {
					if strings.Contains(frame, `"method":"initialize"`) {
						initializes++
					}
				}
				if initializes != 1 {
					t.Fatalf("initialize frames = %d, want 1 (requests stay on the first connection)", initializes)
				}
			},
		},
		{
			// 别的客户端删除了打开着的对话：serve 关闭订阅，重新同步时 conversation/open 回复对话不存在，Client 放弃这个对话，连接保持。
			name: "conversation_deleted",
			run: func(t *testing.T, h *clientHarness, _ *openRefusingRuntime) {
				// 1. 持久化对话并等待空闲，直接删除对话。
				conversation := h.openNewConversation(t)
				h.completeRounds(t, conversation, "保存")
				h.waitRuntimeIdle(t)
				if err := h.runtime.DeleteConversation(t.Context(), &runtime.DeleteConversationRequest{ConversationID: conversation.ID}); err != nil {
					t.Fatal(err)
				}
				// 2. 核对同步失败带 ErrConversationNotFound，连接没有重连。
				events := h.collectEvents(t, func(event runtimeclient.ClientEvent) bool {
					_, ok := event.(*runtimeclient.ConversationSyncFailed)
					return ok
				})
				failed := events[len(events)-1].(*runtimeclient.ConversationSyncFailed)
				if failed.ConversationID != conversation.ID || !errors.Is(failed.Err, runtime.ErrConversationNotFound) {
					t.Fatalf("sync failed = %+v, want conversation %s with %v", failed, conversation.ID, runtime.ErrConversationNotFound)
				}
				if attempts := getReconnectAttempts(events); len(attempts) != 0 {
					t.Fatalf("reconnect attempts = %v, want the connection kept", attempts)
				}
			},
		},
		{
			name: "connection_lost_during_open_reconnects",
			run: func(t *testing.T, h *clientHarness, _ *openRefusingRuntime) {
				// 1. 重新同步时切断 conversation/open 响应。
				conversation := h.openNewConversation(t)
				// conversation/open 的结果带 root_session_id：它经过代理时切断连接。
				h.proxy.cutOnDownstream(`"root_session_id"`)
				h.injectSubscriptionClosed(t, conversation)
				h.collectEvents(t, isConnectionState(runtimeclient.ConnectionReconnecting))
				// 2. 停止切断并等待内容同步，核对未发出 ConversationSyncFailed。
				h.proxy.cutOnDownstream("")
				for _, event := range h.collectEvents(t, isSessionSynced) {
					if failed, ok := event.(*runtimeclient.ConversationSyncFailed); ok {
						t.Fatalf("sync failed = %+v, want a resync after reconnecting", failed)
					}
				}
			},
		},
	}
	// 2. 分别核对放弃对话但保留连接，或断线后重连并重新同步。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var refusing *openRefusingRuntime
			h := startClientHarness(t, clientTestToken, func(opt *runtimeserver.RuntimeServerOpt) {
				refusing = &openRefusingRuntime{Runtime: opt.Runtime}
				opt.Runtime = refusing
			})
			tt.run(t, h, refusing)
		})
	}
}

// errOpenRefused 是 openRefusingRuntime 拒绝时返回的错误。
var errOpenRefused = errors.New(`executor "gpu": connect refused`)

// openRefusingRuntime 在 isRefusing 为 true 时拒绝打开对话，返回 errOpenRefused。
type openRefusingRuntime struct {
	runtime.Runtime
	isRefusing atomic.Bool
}

// OpenConversation 拒绝打开或调用原 Runtime。
func (r *openRefusingRuntime) OpenConversation(ctx context.Context, request *runtime.OpenConversationRequest) (defs.Conversation, error) {
	// 1. 拒绝标记已设时返回 errOpenRefused，否则交给 Runtime 打开。
	if r.isRefusing.Load() {
		return defs.Conversation{}, errOpenRefused
	}
	return r.Runtime.OpenConversation(ctx, request)
}

// injectSubscriptionClosed 向客户端写入服务端关闭根 Session 订阅的通知。
func (h *clientHarness) injectSubscriptionClosed(t *testing.T, conversation defs.Conversation) {
	t.Helper()
	// 1. 填写对话、根 Session、订阅类型和关闭原因。
	h.proxy.injectDownstream(t, map[string]any{"jsonrpc": "2.0", "method": "subscription/closed", "params": map[string]any{
		"conversation_id": conversation.ID,
		"session_id":      conversation.RootSessionID,
		"subscription":    "session",
		"reason":          "conversation stopped",
	}})
}

// waitConversationSynced 读到 ConversationSynced 并返回这个事件。
func (h *clientHarness) waitConversationSynced(t *testing.T) *runtimeclient.ConversationSynced {
	t.Helper()
	// 1. 收集事件到首次对话同步完成，返回最后一条事件。
	events := h.collectEvents(t, func(event runtimeclient.ClientEvent) bool {
		_, ok := event.(*runtimeclient.ConversationSynced)
		return ok
	})
	return events[len(events)-1].(*runtimeclient.ConversationSynced)
}

// waitSessionSynced 读到 SessionSynced 并返回这个事件。
func (h *clientHarness) waitSessionSynced(t *testing.T) *runtimeclient.SessionSynced {
	t.Helper()
	// 1. 收集事件到首次内容同步完成，返回最后一条事件。
	events := h.collectEvents(t, isSessionSynced)
	return events[len(events)-1].(*runtimeclient.SessionSynced)
}

// TestClientConnectionServerID 检查连接及重连事件中的 serve ID。
func TestClientConnectionServerID(t *testing.T) {
	// 1. 准备有无 serve ID 和是否断线重连的场景。
	tests := []struct {
		name     string
		serverID string
		// isReconnecting 为 true 时，连上之后切断连接，再检查重连成功的那次 ConnectionChanged。
		isReconnecting bool
	}{
		{
			name:     "connected_reports_server_id",
			serverID: "abc",
		},
		{
			name:           "reconnected_reports_server_id_again",
			serverID:       "abc",
			isReconnecting: true,
		},
		{name: "connected_without_server_id_is_empty"},
	}
	// 2. 核对仅 connected 事件带 serve ID，其他状态为空。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := startClientHarness(t, clientTestToken, func(opt *runtimeserver.RuntimeServerOpt) { opt.ServerID = tt.serverID })
			events := h.collectEvents(t, isConnectionState(runtimeclient.ConnectionConnected))
			if tt.isReconnecting {
				h.proxy.cut()
				events = h.collectEvents(t, isConnectionState(runtimeclient.ConnectionReconnecting))
				events = append(events, h.collectEvents(t, isConnectionState(runtimeclient.ConnectionConnected))...)
			}
			for _, event := range events {
				changed, ok := event.(*runtimeclient.ConnectionChanged)
				if !ok {
					continue
				}
				wantServerID := ""
				if changed.State == runtimeclient.ConnectionConnected {
					wantServerID = tt.serverID
				}
				if changed.ServerID != wantServerID {
					t.Fatalf("%s ServerID = %q, want %q", changed.State, changed.ServerID, wantServerID)
				}
			}
		})
	}
}

// TestClientUnauthorized 检查错误 token 使客户端返回认证失败。
func TestClientUnauthorized(t *testing.T) {
	// 1. 启动错误 token 的客户端，等待返回 ErrUnauthorized，超时则失败。
	h := startClientHarness(t, "wrong-token")
	select {
	case err := <-h.done:
		if !errors.Is(err, runtimeclient.ErrUnauthorized) {
			t.Fatalf("Run() = %v, want ErrUnauthorized", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return on a rejected token")
	}
}

// TestRedactURL 检查 URL 中认证信息的移除。
func TestRedactURL(t *testing.T) {
	// 1. 准备普通、userinfo、query、fragment 和无效 URL。
	tests := []struct {
		name string
		url  string
		want string
	}{
		{
			name: "plain_url_unchanged",
			url:  "ws://127.0.0.1:8080/api/v1/openvaala",
			want: "ws://127.0.0.1:8080/api/v1/openvaala",
		},
		{
			name: "userinfo_dropped",
			url:  "ws://user:SECRET@host:8080/api",
			want: "ws://host:8080/api",
		},
		{
			name: "query_and_fragment_dropped",
			url:  "wss://host/api?token=SECRET#frag",
			want: "wss://host/api",
		},
		{
			name: "empty_query_marker_dropped",
			url:  "ws://host/api?",
			want: "ws://host/api",
		},
		{
			name: "unparsable_hidden",
			url:  "ws://127.0.0.1:bad/api?token=SECRET",
			want: "<unparsable url>",
		},
	}
	// 2. 逐项清理 URL，核对不含秘密信息且保留预期地址。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := runtimeclient.RedactURL(tt.url); got != tt.want {
				t.Fatalf("RedactURL(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}

// TestClientDialErrorRedactsURL 检查拨号日志和事件不泄露 URL 中的秘密。
func TestClientDialErrorRedactsURL(t *testing.T) {
	// 1. 准备已关闭端口和带秘密信息或无效端口的 URL。
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddress := listener.Addr().String()
	_ = listener.Close()
	tests := []struct {
		name          string
		url           string
		wantErrorPart string
	}{
		{
			name:          "query_token",
			url:           "ws://" + closedAddress + "/api/v1/openvaala?token=SECRET",
			wantErrorPart: "connection refused",
		},
		{
			name:          "userinfo_password",
			url:           "ws://user:SECRET@" + closedAddress + "/api/v1/openvaala",
			wantErrorPart: "connection refused",
		},
		{
			name:          "unparsable_url",
			url:           "ws://127.0.0.1:bad/api/v1/openvaala?token=SECRET",
			wantErrorPart: "invalid port",
		},
		{
			name:          "url_without_secret",
			url:           "ws://" + closedAddress + "/api/v1/openvaala",
			wantErrorPart: "connection refused",
		},
	}
	// 2. 启动客户端等待重连，核对事件和日志错误一致、保留失败原因且无秘密文本。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pipeline := telemetrytest.NewPipeline(t)
			opt := runtimeclient.NewClientOpt(tt.url, "")
			opt.Telemetry = pipeline.Telemetry
			client, err := runtimeclient.NewClient(opt)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- client.Run(ctx) }()
			t.Cleanup(func() {
				cancel()
				for range client.Events() {
				}
				<-done
			})

			var reconnecting *runtimeclient.ConnectionChanged
			timeout := time.After(10 * time.Second)
			for reconnecting == nil {
				select {
				case event := <-client.Events():
					if changed, ok := event.(*runtimeclient.ConnectionChanged); ok && changed.State == runtimeclient.ConnectionReconnecting {
						reconnecting = changed
					}
				case <-timeout:
					t.Fatal("ConnectionReconnecting did not arrive")
				}
			}
			record := pipeline.WaitLogRecords(t, "runtime connection lost, reconnecting", 1)[0]
			logged, _ := record["error"].(string)
			for name, text := range map[string]string{"log error": logged, "event error": reconnecting.Err.Error(), "log text": pipeline.GetLogText()} {
				if strings.Contains(text, "SECRET") {
					t.Fatalf("%s leaks the secret: %s", name, text)
				}
			}
			if !strings.Contains(logged, tt.wantErrorPart) || logged != reconnecting.Err.Error() {
				t.Fatalf("log error = %q, event error = %q, want both containing %q", logged, reconnecting.Err, tt.wantErrorPart)
			}
		})
	}
}

// runtimeclient 测试夹具 --------------------------------------------------------

const clientTestToken = "client-test-token"

type clientHarness struct {
	runtime runtime.Runtime
	// definitions 是 Runtime 启动时的定义，测试在它的基础上换 hook 再交给 Runtime.SetDefinitions。
	definitions *runtime.Definitions
	mutation    dao.Mutation
	model       *clientModel
	proxy       *cuttableProxy
	client      runtimeclient.Client
	// events 是 collectEvents 读取的事件流：默认是 client.Events()，startRelay 之后是转发循环放行的那部分。
	events    <-chan runtimeclient.ClientEvent
	cancel    context.CancelFunc
	done      chan error
	telemetry *telemetrytest.Pipeline
	// configurationCalls 是 serve 的 Configuration 和 ModelCatalog 按顺序收到的调用。
	configurationCalls clientTestCalls
}

// startClientHarness 启动真实 Runtime、Server、Client 和可控代理。
func startClientHarness(t *testing.T, token string, configure ...func(*runtimeserver.RuntimeServerOpt)) *clientHarness {
	t.Helper()
	// 1. 创建临时记录库和日志，使用临时 HOME 扫描 Skill。
	root := t.TempDir()
	// local 使用临时 HOME 下的 Skill 目录。
	t.Setenv("HOME", t.TempDir())
	database, err := storagesqlite.OpenDatabase(storagesqlite.NewDatabaseOpt(filepath.Join(root, "records.db"), dao.RecordNamespaces()...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	logs, err := storagefile.NewLogStorage(storagefile.NewLogStorageOpt(filepath.Join(root, "logs")))
	if err != nil {
		t.Fatal(err)
	}
	_, mutation, err := dao.NewDAO(dao.NewDAOOpt(database.Record(), logs))
	if err != nil {
		t.Fatal(err)
	}
	// 2. 启动受控模型和带两个模型、两个 Executor 的 Runtime。
	model := newClientModel(t)
	opt := runtime.NewRuntimeOpt(database.Record(), logs, &runtime.Definitions{
		Agents: []*agent.AgentDefinition{{
			Name:         "assistant",
			Description:  "主 Agent",
			Instructions: "Test assistant",
			Model: defs.ModelRef{
				ProviderID: "test",
				ModelID:    "test",
			},
		}},
		Models: []*defs.ModelDefinition{
			{
				Ref: defs.ModelRef{
					ProviderID: "test",
					ModelID:    "test",
				},
				Target: &llmkit.Target{
					Protocol: llmkit.ProtocolOpenAIChat,
					BaseURL:  model.server.URL,
					Model:    "test",
				},
			},
			{
				Ref: defs.ModelRef{
					ProviderID: "test",
					ModelID:    "second",
				},
				DisplayName: "Second",
				Target: &llmkit.Target{
					Protocol: llmkit.ProtocolOpenAIChat,
					BaseURL:  model.server.URL,
					Model:    "second",
				},
				ReasoningEfforts:       []string{"low", "high"},
				DefaultReasoningEffort: "low",
			},
		},
		Executors: executor.NewConfigExecutorProvider([]executor.Executor{
			executor.NewLocalExecutor(&defs.ExecutorDefinition{
				ID:                 defs.LocalExecutorID,
				Kind:               defs.ExecutorKindLocal,
				Description:        "本机",
				WorkspaceDirectory: root,
			}),
			executor.NewVirtualExecutor(executor.NewVirtualExecutorOpt(&defs.ExecutorDefinition{
				ID:                 "openvaala",
				Kind:               defs.ExecutorKindVirtual,
				Description:        "内置",
				WorkspaceDirectory: "/workspace",
			}, nil)),
		}),
	})
	rt, err := runtime.NewRuntime(opt)
	if err != nil {
		t.Fatal(err)
	}
	runtimeCtx, stopRuntime := context.WithCancel(context.Background())
	runtimeDone := make(chan error, 1)
	go func() { runtimeDone <- rt.Run(runtimeCtx) }()

	// 3. 创建 Server，应用测试配置并运行。
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	configurationCalls := make(clientTestCalls, 16)
	serverOpt := &runtimeserver.RuntimeServerOpt{
		Runtime:  rt,
		Listener: listener,
		Token:    clientTestToken,
		Configuration: &clientTestConfiguration{
			definitions: opt.Definitions,
			calls:       configurationCalls,
		},
		ModelCatalog: &clientTestModelCatalog{calls: configurationCalls},
	}
	for _, apply := range configure {
		apply(serverOpt)
	}
	server, err := runtimeserver.NewRuntimeServer(serverOpt)
	if err != nil {
		t.Fatal(err)
	}
	serverCtx, stopServer := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Run(serverCtx) }()

	// 4. 创建可切断代理和遥测管道，启动 Client。
	proxy := newCuttableProxy(t, listener.Addr().String())
	pipeline := telemetrytest.NewPipeline(t)
	clientOpt := runtimeclient.NewClientOpt("ws://"+proxy.address+"/api/v1/openvaala", token)
	clientOpt.Telemetry = pipeline.Telemetry
	client, err := runtimeclient.NewClient(clientOpt)
	if err != nil {
		t.Fatal(err)
	}
	clientCtx, cancel := context.WithCancel(context.Background())
	h := &clientHarness{
		runtime:            rt,
		definitions:        opt.Definitions,
		mutation:           mutation,
		model:              model,
		proxy:              proxy,
		client:             client,
		events:             client.Events(),
		cancel:             cancel,
		done:               make(chan error, 1),
		telemetry:          pipeline,
		configurationCalls: configurationCalls,
	}
	go func() { h.done <- client.Run(clientCtx) }()
	// 5. 登记清理：取消并排空客户端事件，依次停止并等待 Server、Runtime。
	t.Cleanup(func() {
		cancel()
		for range client.Events() {
		}
		stopServer()
		<-serverDone
		stopRuntime()
		<-runtimeDone
	})
	return h
}

// clientTestProvider 是测试 serve 配置里唯一的 Provider；模型目录里的模型也都是它的模型。
var clientTestProvider = defs.ProviderConfig{
	ID:       "gw",
	Protocol: "openai_chat",
	BaseURL:  "https://gw.example/v1",
	APIKey:   "sk-test",
	Models: map[string]*defs.ProviderModelConfig{"m1": {
		DisplayName:      "M1",
		ContextWindow:    128000,
		ReasoningEfforts: defs.OptionalStringList{},
	}},
}

// clientTestCatalogProvider 是测试模型目录里唯一的服务。
var clientTestCatalogProvider = defs.CatalogProvider{
	ID:         "openai",
	Name:       "OpenAI",
	Protocol:   "openai_responses",
	BaseURL:    "https://api.openai.com/v1",
	ModelCount: 1,
}

// clientTestCall 是测试假实现收到的一次调用：方法名和它唯一的业务参数，没有参数时为 nil。
type clientTestCall struct {
	method   string
	argument any
}

// clientTestCalls 按到达顺序保存假实现收到的调用；写满后丢弃新的。
type clientTestCalls chan clientTestCall

// record 记录一次假实现调用。
func (calls clientTestCalls) record(method string, argument any) {
	// 1. 尝试非阻塞写入方法名和参数，缓冲区满时丢弃。
	select {
	case calls <- clientTestCall{
		method:   method,
		argument: argument,
	}:
	default:
	}
}

// drain 按到达顺序取出尚未读取的假实现调用。
func (calls clientTestCalls) drain() []clientTestCall {
	// 1. 持续非阻塞读取，当前队列为空时返回已收集调用。
	var drained []clientTestCall
	for {
		select {
		case call := <-calls:
			drained = append(drained, call)
		default:
			return drained
		}
	}
}

// clientTestConfiguration 是测试用的 Configuration：把调用记进 calls；配置里只有 clientTestProvider，写方法和 Reload 返回 Runtime
// 构造时的三类定义；和 config.ConfigFiles 一样，删除没有的 Provider 时返回 provider <id> is not configured。
type clientTestConfiguration struct {
	definitions *runtime.Definitions
	calls       clientTestCalls
}

// ListProviders 返回测试 Provider、默认模型和模型默认配置。
func (c *clientTestConfiguration) ListProviders(context.Context) ([]defs.ProviderConfig, defs.ModelRef, defs.ModelDefaults, error) {
	// 1. 记录调用，返回唯一 Provider 和 m1 模型引用。
	c.calls.record("ListProviders", nil)
	return []defs.ProviderConfig{clientTestProvider}, defs.ModelRef{
		ProviderID: clientTestProvider.ID,
		ModelID:    "m1",
	}, clientTestModelDefaults, nil
}

// clientTestModelDefaults 是测试 serve 返回的模型默认配置。
var clientTestModelDefaults = defs.ModelDefaults{
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

// clientTestProviderList 是 Client.ListProviders 的三个结果。
type clientTestProviderList struct {
	providers    []defs.ProviderConfig
	defaultModel defs.ModelRef
	defaults     defs.ModelDefaults
}

// clientTestProviderWrite 是 clientTestConfiguration.SetProvider 收到的参数。
type clientTestProviderWrite struct {
	provider     defs.ProviderConfig
	defaultModel defs.ModelRef
}

// SetProvider 记录 Provider 写入并返回测试定义。
func (c *clientTestConfiguration) SetProvider(_ context.Context, provider defs.ProviderConfig, defaultModel defs.ModelRef) (*runtime.Definitions, error) {
	// 1. 保存 Provider 和默认模型参数，返回初始定义。
	c.calls.record("SetProvider", clientTestProviderWrite{
		provider:     provider,
		defaultModel: defaultModel,
	})
	return c.definitions, nil
}

// DeleteProvider 记录删除 Provider 请求。
func (c *clientTestConfiguration) DeleteProvider(_ context.Context, providerID string) (*runtime.Definitions, error) {
	// 1. 未配置的 ID 返回普通错误，否则返回初始定义。
	c.calls.record("DeleteProvider", providerID)
	if providerID != clientTestProvider.ID {
		return nil, fmt.Errorf("provider %s is not configured", providerID)
	}
	return c.definitions, nil
}

// SetDefaultModel 记录默认模型写入并返回测试定义。
func (c *clientTestConfiguration) SetDefaultModel(_ context.Context, ref defs.ModelRef) (*runtime.Definitions, error) {
	// 1. 保存模型引用，返回初始定义。
	c.calls.record("SetDefaultModel", ref)
	return c.definitions, nil
}

// Reload 记录重新加载并返回测试定义。
func (c *clientTestConfiguration) Reload(context.Context) (*runtime.Definitions, error) {
	// 1. 保存调用，返回初始定义。
	c.calls.record("Reload", nil)
	return c.definitions, nil
}

// clientTestModelCatalog 是测试用的 ModelCatalog：把调用记进 calls；目录里只有 clientTestCatalogProvider，
// ListCatalogModels 和 FetchProviderModels 都返回 clientTestProvider 的模型。
type clientTestModelCatalog struct {
	calls clientTestCalls
}

// ListCatalogProviders 返回唯一的测试模型目录服务。
func (c *clientTestModelCatalog) ListCatalogProviders(context.Context) ([]defs.CatalogProvider, error) {
	// 1. 记录调用，返回目录 Provider 列表。
	c.calls.record("ListCatalogProviders", nil)
	return []defs.CatalogProvider{clientTestCatalogProvider}, nil
}

// ListCatalogModels 返回测试模型目录的模型。
func (c *clientTestModelCatalog) ListCatalogModels(_ context.Context, catalogProviderID string) (map[string]*defs.ProviderModelConfig, error) {
	// 1. 记录目录服务 ID，返回测试模型。
	c.calls.record("ListCatalogModels", catalogProviderID)
	return clientTestProvider.Models, nil
}

// FetchProviderModels 返回测试 Provider 的模型。
func (c *clientTestModelCatalog) FetchProviderModels(_ context.Context, provider defs.ProviderConfig) (map[string]*defs.ProviderModelConfig, error) {
	// 1. 记录 Provider 参数，返回测试模型。
	c.calls.record("FetchProviderModels", provider)
	return clientTestProvider.Models, nil
}

// openNewConversation 创建、打开对话并等内容同步完成。
func (h *clientHarness) openNewConversation(t *testing.T) defs.Conversation {
	t.Helper()
	// 1. 创建 assistant 对话，打开后读取到同一对话的 SessionSynced。
	conversation, err := h.client.CreateConversation(t.Context(), &runtimeclient.CreateConversationRequest{AgentName: "assistant"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.OpenConversation(t.Context(), &runtimeclient.OpenConversationRequest{ConversationID: conversation.ID}); err != nil {
		t.Fatal(err)
	}
	h.collectEvents(t, func(event runtimeclient.ClientEvent) bool {
		synced, ok := event.(*runtimeclient.SessionSynced)
		return ok && synced.ConversationID == conversation.ID
	})
	return conversation
}

// setHooks 替换测试 Runtime 定义里的 hook。
func (h *clientHarness) setHooks(t *testing.T, hooks []*hook.HookDefinition) {
	t.Helper()
	// 1. 复制启动定义并替换 Hooks，提交给 Runtime，失败时结束测试。
	definitions := *h.definitions
	definitions.Hooks = hooks
	if _, err := h.runtime.SetDefinitions(t.Context(), &definitions); err != nil {
		t.Fatal(err)
	}
}

// textContent 创建普通文本 Content。
func textContent(text string) []*defs.Content {
	// 1. 返回一个文本内容列表。
	return []*defs.Content{{
		Kind: defs.ContentText,
		Text: &defs.TextContent{Text: text},
	}}
}

// submit 通过 Client 向对话提交普通用户文本。
func (h *clientHarness) submit(t *testing.T, conversation defs.Conversation, text string) {
	t.Helper()
	// 1. 填写对话 ID、普通模式和文本，提交失败时结束测试。
	request := &runtimeclient.SubmitInputRequest{
		ConversationID: conversation.ID,
		Mode:           session.SessionInputModeNormal,
		Content:        textContent(text),
	}
	if _, err := h.client.SubmitInput(t.Context(), request); err != nil {
		t.Fatal(err)
	}
}

// submitDirectly 绕过 Client 向 Runtime 提交用户文本。
func (h *clientHarness) submitDirectly(t *testing.T, conversation defs.Conversation, text string) {
	t.Helper()
	// 1. 填写独立 InputID、用户来源和普通模式，提交失败时结束测试。
	_, err := h.runtime.SubmitInput(t.Context(), &runtime.SubmitInputRequest{
		ConversationID: conversation.ID,
		InputID:        defs.InputID("direct-" + text),
		Mode:           session.SessionInputModeNormal,
		From:           &defs.Address{Kind: defs.IdentityUser},
		Content:        textContent(text),
	})
	if err != nil {
		t.Fatal(err)
	}
}

// completeRounds 逐轮提交文本并返回按创建顺序排列的 Turn ID。
func (h *clientHarness) completeRounds(t *testing.T, conversation defs.Conversation, texts ...string) []defs.TurnID {
	t.Helper()
	// 1. 每条文本提交后回复模型，等本轮完成并保存 Turn ID。
	var turnIDs []defs.TurnID
	for _, text := range texts {
		h.submit(t, conversation, text)
		h.model.receive(t).reply <- clientModelResponse{chunks: []string{"好"}}
		events := h.collectEvents(t, isTurnFinished)
		turnIDs = append(turnIDs, events[len(events)-1].(*runtimeclient.TurnFinished).TurnID)
	}
	return turnIDs
}

// delegateReview 创建 review 子 Agent，完成首轮并返回子 Session。
func (h *clientHarness) delegateReview(t *testing.T, conversation defs.Conversation) defs.Session {
	t.Helper()
	// 1. 向根 Session 提交委派输入，回复 spawn_agent 调用并读取根子模型请求。
	h.submit(t, conversation, "委派")
	h.model.receive(t).reply <- clientModelResponse{calls: []llmkit.ToolCall{{
		ID:        "spawn",
		Name:      "spawn_agent",
		Arguments: `{"task_name":"review","message":"check"}`,
	}}}
	root, review := h.model.receiveRootAndTask(t, "review")
	// 2. 读取 Session 列表找到 review，缺少记录时结束测试。
	list, err := h.client.ListSessions(t.Context(), &runtimeclient.ListSessionsRequest{ConversationID: conversation.ID})
	if err != nil {
		t.Fatal(err)
	}
	index := slices.IndexFunc(list.Sessions, func(record defs.Session) bool { return record.TaskName == "review" })
	if index < 0 {
		t.Fatalf("sessions = %+v, want a review Session", list.Sessions)
	}
	// 3. 先完成根 Turn，再完成子 Turn，回复根 Session 的结果处理轮并等全部空闲。
	root.reply <- clientModelResponse{chunks: []string{"已委派"}}
	h.collectEvents(t, isTurnFinished)
	review.reply <- clientModelResponse{chunks: []string{"子任务完成"}}
	h.model.receive(t).reply <- clientModelResponse{chunks: []string{"收到子任务结果"}}
	h.collectEvents(t, isTurnFinished)
	h.waitTurnsCompleted(t, conversation.ID, list.Sessions[index].ID, 1)
	h.waitRuntimeIdle(t)
	return list.Sessions[index]
}

// replyReviewRound 完成根 Session 向 review 子 Agent 发消息的全部轮次。
func (h *clientHarness) replyReviewRound(t *testing.T, child defs.Session) {
	t.Helper()
	// 1. 读取根子已完成轮数，回复 send_message 调用并读取两方模型请求。
	rootID := child.ParentSessionID
	rootCompleted, childCompleted := h.getCompletedTurnCount(t, child.ConversationID, rootID), h.getCompletedTurnCount(t, child.ConversationID, child.ID)
	h.model.receive(t).reply <- clientModelResponse{calls: []llmkit.ToolCall{{
		ID:        "send",
		Name:      "send_message",
		Arguments: fmt.Sprintf(`{"session_id":%q,"message":"again"}`, child.ID),
	}}}
	root, review := h.model.receiveRootAndTask(t, "review")
	// 2. 先完成根转达轮，再完成子任务轮，最后回复根接收结果轮并等待空闲。
	root.reply <- clientModelResponse{chunks: []string{"已转达"}}
	h.waitTurnsCompleted(t, child.ConversationID, rootID, rootCompleted+1)
	review.reply <- clientModelResponse{chunks: []string{"第二轮完成"}}
	h.model.receive(t).reply <- clientModelResponse{chunks: []string{"收到子任务结果"}}
	h.waitTurnsCompleted(t, child.ConversationID, rootID, rootCompleted+2)
	h.waitTurnsCompleted(t, child.ConversationID, child.ID, childCompleted+1)
	h.waitRuntimeIdle(t)
}

// getSessionTurnIDs 按创建顺序读取 Session 的全部 Turn ID。
func (h *clientHarness) getSessionTurnIDs(t *testing.T, conversationID defs.ConversationID, sessionID defs.SessionID) []defs.TurnID {
	t.Helper()
	// 1. 直接查询 Runtime 的正序 Turn 列表，失败时结束测试。
	page, err := h.runtime.ListTurns(t.Context(), &runtime.ListTurnsRequest{
		ConversationID: conversationID,
		SessionID:      sessionID,
		Order: storage.Order{
			By:        storage.OrderByKey,
			Direction: storage.DirectionAsc,
		},
		Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	return getTurnIDs(page.Items)
}

// getCompletedTurnCount 读取 Session 已完成的 Turn 数。
func (h *clientHarness) getCompletedTurnCount(t *testing.T, conversationID defs.ConversationID, sessionID defs.SessionID) int {
	t.Helper()
	// 1. 直接查询 Runtime，统计 completed 状态轮次。
	page, err := h.runtime.ListTurns(t.Context(), &runtime.ListTurnsRequest{
		ConversationID: conversationID,
		SessionID:      sessionID,
		Order: storage.Order{
			By:        storage.OrderByKey,
			Direction: storage.DirectionAsc,
		},
		Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	completed := 0
	for _, turn := range page.Items {
		if turn.Status == defs.TurnStatusCompleted {
			completed++
		}
	}
	return completed
}

// waitTurnsCompleted 等待 Session 至少完成指定数量的 Turn。
func (h *clientHarness) waitTurnsCompleted(t *testing.T, conversationID defs.ConversationID, sessionID defs.SessionID, count int) {
	t.Helper()
	// 1. 轮询完成数量，超过十秒则失败。
	deadline := time.Now().Add(10 * time.Second)
	for completed := h.getCompletedTurnCount(t, conversationID, sessionID); completed < count; completed = h.getCompletedTurnCount(t, conversationID, sessionID) {
		if time.Now().After(deadline) {
			t.Fatalf("session %s completed %d turns, want %d", sessionID, completed, count)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitRuntimeIdle 等待所有对话不再执行 Turn。
func (h *clientHarness) waitRuntimeIdle(t *testing.T) {
	t.Helper()
	// 1. 轮询 Runtime 空闲状态，查询失败或超过十秒则失败。
	deadline := time.Now().Add(10 * time.Second)
	for {
		isIdle, err := h.runtime.IsIdle(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if isIdle {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("runtime did not become idle")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// collectEvents 读取并返回截至首次匹配事件的所有事件。
func (h *clientHarness) collectEvents(t *testing.T, match func(runtimeclient.ClientEvent) bool) []runtimeclient.ClientEvent {
	t.Helper()
	// 1. 从测试事件流收集，匹配后返回，通道关闭或十秒未匹配则失败。
	var collected []runtimeclient.ClientEvent
	timeout := time.After(10 * time.Second)
	for {
		select {
		case event, open := <-h.events:
			if !open {
				t.Fatalf("events closed after %d events", len(collected))
			}
			collected = append(collected, event)
			if match(event) {
				return collected
			}
		case <-timeout:
			t.Fatalf("event did not arrive after %d events", len(collected))
		}
	}
}

// isTurnFinished 事件是否为 TurnFinished？
func isTurnFinished(event runtimeclient.ClientEvent) bool {
	// 1. 检查事件类型。
	_, ok := event.(*runtimeclient.TurnFinished)
	return ok
}

// isSessionSynced 事件是否为 SessionSynced？
func isSessionSynced(event runtimeclient.ClientEvent) bool {
	// 1. 检查事件类型。
	_, ok := event.(*runtimeclient.SessionSynced)
	return ok
}

// isTurnFinishedTimes 创建统计 TurnFinished 到达次数的匹配函数。
func isTurnFinishedTimes(times int) func(runtimeclient.ClientEvent) bool {
	// 1. 累计完成事件，达到指定次数时匹配。
	finished := 0
	return func(event runtimeclient.ClientEvent) bool {
		if isTurnFinished(event) {
			finished++
		}
		return finished >= times
	}
}

// isItemDelta 事件是否为 Item 文本增量？
func isItemDelta(event runtimeclient.ClientEvent) bool {
	// 1. 检查事件为 ItemChanged 且类型为 AgentEventItemDelta。
	changed, ok := event.(*runtimeclient.ItemChanged)
	return ok && changed.Type == agent.AgentEventItemDelta
}

// isConnectionState 创建匹配指定连接状态的函数。
func isConnectionState(state runtimeclient.ConnectionState) func(runtimeclient.ClientEvent) bool {
	// 1. 检查 ConnectionChanged 事件及状态。
	return func(event runtimeclient.ClientEvent) bool {
		changed, ok := event.(*runtimeclient.ConnectionChanged)
		return ok && changed.State == state
	}
}

// isReconnectAttempt 创建匹配重连次数和退避时长的函数。
func isReconnectAttempt(attempt int) func(runtimeclient.ClientEvent) bool {
	// 1. 检查重连状态、次数，以及按次数翻倍的等待时长。
	return func(event runtimeclient.ClientEvent) bool {
		changed, ok := event.(*runtimeclient.ConnectionChanged)
		return ok && changed.State == runtimeclient.ConnectionReconnecting && changed.Attempt == attempt &&
			changed.RetryIn == time.Duration(1<<(attempt-1))*time.Second
	}
}

// getReconnectAttempts 按事件顺序返回重连次数。
func getReconnectAttempts(events []runtimeclient.ClientEvent) []int {
	// 1. 筛选 reconnecting 的 ConnectionChanged，收集 Attempt。
	var attempts []int
	for _, event := range events {
		if changed, ok := event.(*runtimeclient.ConnectionChanged); ok && changed.State == runtimeclient.ConnectionReconnecting {
			attempts = append(attempts, changed.Attempt)
		}
	}
	return attempts
}

// getLastSavedOrdinal 返回事件中最后交付的已保存流水号。
func getLastSavedOrdinal(events []runtimeclient.ClientEvent) storage.Ordinal {
	// 1. 从保存事件和非空历史同步事件更新最后流水号。
	var last storage.Ordinal
	for _, event := range events {
		switch event := event.(type) {
		case *runtimeclient.ItemsSaved:
			last = event.Items[len(event.Items)-1].Ordinal
		case *runtimeclient.SessionSynced:
			if len(event.Items) > 0 {
				last = event.Items[len(event.Items)-1].Ordinal
			}
		}
	}
	return last
}

// getOrdinals 按原顺序返回已保存 Item 的流水号。
func getOrdinals(items []runtimeprotocol.LoggedItem) []storage.Ordinal {
	// 1. 遍历日志项收集流水号。
	ordinals := make([]storage.Ordinal, len(items))
	for index, logged := range items {
		ordinals[index] = logged.Ordinal
	}
	return ordinals
}

// getTurnIDs 按原顺序返回 Turn ID。
func getTurnIDs(turns []defs.Turn) []defs.TurnID {
	// 1. 遍历记录收集 ID。
	ids := make([]defs.TurnID, len(turns))
	for index, turn := range turns {
		ids[index] = turn.ID
	}
	return ids
}

// getConversationID 返回事件所属对话 ID。
func getConversationID(event runtimeclient.ClientEvent) defs.ConversationID {
	// 1. 按业务事件类型提取对话 ID，无对话身份的事件返回空。
	switch event := event.(type) {
	case *runtimeclient.SessionSynced:
		return event.ConversationID
	case *runtimeclient.ItemsSaved:
		return event.ConversationID
	case *runtimeclient.ItemChanged:
		return event.ConversationID
	case *runtimeclient.TurnStarted:
		return event.ConversationID
	case *runtimeclient.TurnFinished:
		return event.ConversationID
	case *runtimeclient.ConversationSynced:
		return event.ConversationID
	case *runtimeclient.SessionStatusChanged:
		return event.ConversationID
	case *runtimeclient.ConversationTitleChanged:
		return event.ConversationID
	}
	return ""
}

// getConversationIDs 按页内顺序返回列表项的对话 ID。
func getConversationIDs(conversations []dao.ConversationListEntry) []defs.ConversationID {
	// 1. 遍历列表项提取对话记录的 ID。
	ids := make([]defs.ConversationID, len(conversations))
	for index, conversation := range conversations {
		ids[index] = conversation.Conversation.ID
	}
	return ids
}

// waitClosed 等待测试通道关闭。
func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	// 1. 等待关闭信号，十秒未收到时结束测试并报告指定事项未到达。
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not arrive", what)
	}
}

// itemText 返回测试消息的首段文本。
func itemText(item *defs.Item) string {
	// 1. 读取 Message 第一个 Content 的文本。
	return item.Message.Content[0].Text.Text
}

// cuttableProxy 把 TCP 连接转发到 Server：cut 断开当前全部转发连接模拟网络中断，pause 期间拒绝新连接，
// holdUpstream 期间扣住客户端发往服务端的数据，cutOnDownstream 期间下行数据含 marker 时断开，
// injectDownstream 向客户端写入一帧伪造的服务端消息。
type cuttableProxy struct {
	address     string
	target      string
	connections syncmap.SyncMap[net.Conn, struct{}]
	// clientConnections 是面向客户端一侧的转发连接，injectDownstream 写入它们。
	clientConnections syncmap.SyncMap[net.Conn, struct{}]
	// resumeCh 关闭时放行新连接；pause 换成未关闭的通道，resume 关闭它。upstreamHold 非空时扣住上行数据。
	// 测试夹具用 Mutex 保护它之后的全部字段。
	mutex        sync.Mutex
	resumeCh     chan struct{}
	upstreamHold *upstreamHold
	cutMarker    []byte
	// upstreamFrames 按到达顺序保存客户端发往服务端的全部文本帧载荷，供测试确认客户端上行了什么。
	upstreamFrames []string
}

// upstreamHold 是一次上行扣留：arrived 在第一段被扣住的上行数据到达时关闭，markerSeen 在含 marker 的下行数据
// 转发给客户端后关闭，released 由 releaseUpstream 关闭以放行扣住的数据。
type upstreamHold struct {
	marker      []byte
	arrived     chan struct{}
	markerSeen  chan struct{}
	released    chan struct{}
	arrivedOnce sync.Once
	markerOnce  sync.Once
}

// newCuttableProxy 创建并运行可暂停、扣留和切断连接的代理。
func newCuttableProxy(t *testing.T, target string) *cuttableProxy {
	t.Helper()
	// 1. 监听本机临时端口，逐连接等待放行后转发，结束时关闭监听、放行上行并切断连接。
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxy := &cuttableProxy{
		address:  listener.Addr().String(),
		target:   target,
		resumeCh: make(chan struct{}),
	}
	close(proxy.resumeCh)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			<-proxy.getResumeChannel()
			go proxy.forward(conn)
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		proxy.releaseUpstream()
		proxy.cut()
	})
	return proxy
}

// forward 双向转发一条代理连接。
func (p *cuttableProxy) forward(client net.Conn) {
	// 1. 连接服务端，失败时关闭客户端；登记两端和面向客户端的连接。
	server, err := net.Dial("tcp", p.target)
	if err != nil {
		_ = client.Close()
		return
	}
	p.connections.Store(client, struct{}{})
	p.connections.Store(server, struct{}{})
	p.clientConnections.Store(client, struct{}{})
	done := make(chan struct{}, 2)
	decoder := &upstreamFrameDecoder{}
	// 2. 并行转发上行和下行，记录上行帧并执行扣留、断开和标记观察。
	go func() {
		p.copyStream(server, client, func(data []byte, isWritten bool) {
			if !isWritten {
				p.recordUpstreamFrames(decoder.feed(data))
			}
			p.waitUpstream(data, isWritten)
		})
		done <- struct{}{}
	}()
	go func() { p.copyStream(client, server, p.observeDownstream); done <- struct{}{} }()
	// 3. 等待任一方向结束，关闭两端并删除连接登记。
	<-done
	_ = client.Close()
	_ = server.Close()
	p.connections.Delete(client)
	p.connections.Delete(server)
	p.clientConnections.Delete(client)
}

// copyStream 转发字节流，每段写出前后各观察一次。
func (p *cuttableProxy) copyStream(destination, source net.Conn, observe func(data []byte, isWritten bool)) {
	// 1. 循环读取并写出数据，读取或写入失败时返回。
	buffer := make([]byte, 32<<10)
	for {
		n, err := source.Read(buffer)
		if n > 0 {
			observe(buffer[:n], false)
			if _, err := destination.Write(buffer[:n]); err != nil {
				return
			}
			observe(buffer[:n], true)
		}
		if err != nil {
			return
		}
	}
}

// recordUpstreamFrames 追加已解码的上行文本帧。
func (p *cuttableProxy) recordUpstreamFrames(frames []string) {
	// 1. 忽略空列表，加锁追加载荷。
	if len(frames) == 0 {
		return
	}
	p.mutex.Lock()
	defer p.mutex.Unlock()
	p.upstreamFrames = append(p.upstreamFrames, frames...)
}

// getUpstreamFrames 返回已记录上行帧的副本。
func (p *cuttableProxy) getUpstreamFrames() []string {
	// 1. 加锁复制列表。
	p.mutex.Lock()
	defer p.mutex.Unlock()
	return slices.Clone(p.upstreamFrames)
}

// upstreamFrameDecoder 从一条连接上客户端发往服务端的 TCP 字节流还原 WebSocket 文本帧载荷：
// 先跳过 HTTP 升级请求，之后按帧头长度切帧并去掉客户端帧的掩码（测试不启用压缩，消息都是单帧）。
type upstreamFrameDecoder struct {
	buffer     []byte
	isUpgraded bool
}

// feed 解码已完整到达的客户端 WebSocket 文本帧。
func (d *upstreamFrameDecoder) feed(data []byte) []string {
	// 1. 追加字节，首次跳过完整 HTTP 升级请求。
	d.buffer = append(d.buffer, data...)
	if !d.isUpgraded {
		end := bytes.Index(d.buffer, []byte("\r\n\r\n"))
		if end < 0 {
			return nil
		}
		d.buffer = d.buffer[end+4:]
		d.isUpgraded = true
	}
	// 2. 读取各帧长度和掩码，帧不完整时保留缓冲区，完整文本帧去掩码后返回。
	var payloads []string
	for len(d.buffer) >= 2 {
		length, offset := int(d.buffer[1]&0x7f), 2
		switch length {
		case 126:
			if len(d.buffer) < 4 {
				return payloads
			}
			length, offset = int(binary.BigEndian.Uint16(d.buffer[2:4])), 4
		case 127:
			if len(d.buffer) < 10 {
				return payloads
			}
			length, offset = int(binary.BigEndian.Uint64(d.buffer[2:10])), 10
		}
		maskKey := d.buffer[1]&0x80 != 0
		payloadStart := offset
		if maskKey {
			payloadStart += 4
		}
		if len(d.buffer) < payloadStart+length {
			return payloads
		}
		payload := bytes.Clone(d.buffer[payloadStart : payloadStart+length])
		if maskKey {
			for index := range payload {
				payload[index] ^= d.buffer[offset+index%4]
			}
		}
		if d.buffer[0]&0x0f == 0x1 {
			payloads = append(payloads, string(payload))
		}
		d.buffer = d.buffer[payloadStart+length:]
	}
	return payloads
}

// waitUpstream 在写出前等待上行扣留放行。
func (p *cuttableProxy) waitUpstream(_ []byte, isWritten bool) {
	// 1. 没有扣留或数据已写出时返回，否则只通知一次到达并等待 released。
	hold := p.getUpstreamHold()
	if hold == nil || isWritten {
		return
	}
	hold.arrivedOnce.Do(func() { close(hold.arrived) })
	<-hold.released
}

// observeDownstream 按标记切断连接或通知扣留期间的下行已送达。
func (p *cuttableProxy) observeDownstream(data []byte, isWritten bool) {
	// 1. 写出前遇到 cutMarker 时切断连接，写出后遇到扣留 marker 时只通知一次。
	if marker := p.getCutMarker(); !isWritten && len(marker) > 0 && bytes.Contains(data, marker) {
		p.cut()
		return
	}
	hold := p.getUpstreamHold()
	if hold != nil && isWritten && bytes.Contains(data, hold.marker) {
		hold.markerOnce.Do(func() { close(hold.markerSeen) })
	}
}

// holdUpstream 开始扣留上行数据并观察下行标记。
func (p *cuttableProxy) holdUpstream(marker string) *upstreamHold {
	// 1. 创建到达、标记和放行通道，加锁保存本次扣留。
	hold := &upstreamHold{
		marker:     []byte(marker),
		arrived:    make(chan struct{}),
		markerSeen: make(chan struct{}),
		released:   make(chan struct{}),
	}
	p.mutex.Lock()
	defer p.mutex.Unlock()
	p.upstreamHold = hold
	return hold
}

// releaseUpstream 清除并放行正在扣留的上行数据。
func (p *cuttableProxy) releaseUpstream() {
	// 1. 加锁取走扣留记录，存在时关闭 released。
	p.mutex.Lock()
	hold := p.upstreamHold
	p.upstreamHold = nil
	p.mutex.Unlock()
	if hold != nil {
		close(hold.released)
	}
}

// getUpstreamHold 返回本次上行扣留记录。
func (p *cuttableProxy) getUpstreamHold() *upstreamHold {
	// 1. 加锁读取扣留。
	p.mutex.Lock()
	defer p.mutex.Unlock()
	return p.upstreamHold
}

// cutOnDownstream 设置下行断开标记。
func (p *cuttableProxy) cutOnDownstream(marker string) {
	// 1. 加锁保存 marker 字节，空文本表示停止匹配。
	p.mutex.Lock()
	defer p.mutex.Unlock()
	p.cutMarker = []byte(marker)
}

// getCutMarker 返回当前下行断开标记。
func (p *cuttableProxy) getCutMarker() []byte {
	// 1. 加锁读取 marker。
	p.mutex.Lock()
	defer p.mutex.Unlock()
	return p.cutMarker
}

// injectDownstream 向空闲下行连接写入一帧伪造的 WebSocket 消息。
func (p *cuttableProxy) injectDownstream(t *testing.T, message any) {
	t.Helper()
	// 1. 编码载荷和未掩码文本帧，写给面向客户端的连接，非单连接或写入失败时结束测试。
	payload, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	frame := []byte{0x81}
	switch {
	case len(payload) < 126:
		frame = append(frame, byte(len(payload)))
	case len(payload) <= 0xffff:
		frame = append(frame, 126, byte(len(payload)>>8), byte(len(payload)))
	default:
		t.Fatalf("injected message too large: %d bytes", len(payload))
	}
	frame = append(frame, payload...)
	injected := 0
	p.clientConnections.Range(func(conn net.Conn, _ struct{}) bool {
		if _, err := conn.Write(frame); err != nil {
			t.Fatal(err)
		}
		injected++
		return true
	})
	if injected != 1 {
		t.Fatalf("injected into %d connections, want 1", injected)
	}
}

// cut 关闭全部转发连接。
func (p *cuttableProxy) cut() {
	// 1. 遍历已登记两端并关闭连接。
	p.connections.Range(func(conn net.Conn, _ struct{}) bool {
		_ = conn.Close()
		return true
	})
}

// pause 让新接受的连接等待放行。
func (p *cuttableProxy) pause() {
	// 1. 加锁替换为未关闭的放行通道。
	p.mutex.Lock()
	defer p.mutex.Unlock()
	p.resumeCh = make(chan struct{})
}

// resume 放行等待中的新连接。
func (p *cuttableProxy) resume() {
	// 1. 加锁关闭放行通道。
	p.mutex.Lock()
	defer p.mutex.Unlock()
	close(p.resumeCh)
}

// getResumeChannel 返回新连接的放行通道。
func (p *cuttableProxy) getResumeChannel() <-chan struct{} {
	// 1. 加锁读取通道。
	p.mutex.Lock()
	defer p.mutex.Unlock()
	return p.resumeCh
}

type clientModelRequest struct {
	body  map[string]any
	reply chan clientModelResponse
}

// clientModelResponse 是受控模型的一次回复：chunks 依次作为文本增量写出；release 非 nil 时写出第一段后等待它关闭；
// calls 是随后写出的工具调用；finish 非 nil 时写完内容后等待它关闭再结束回复。
type clientModelResponse struct {
	chunks  []string
	release <-chan struct{}
	calls   []llmkit.ToolCall
	finish  <-chan struct{}
}

type clientModel struct {
	server   *httptest.Server
	requests chan clientModelRequest
}

// newClientModel 创建可控制文本增量和工具回复的测试模型服务。
func newClientModel(t *testing.T) *clientModel {
	t.Helper()
	// 1. 创建请求队列和受控模型 HTTP 服务。
	model := &clientModel{requests: make(chan clientModelRequest, 128)}
	model.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 1. 解码请求，交给测试控制；无效 JSON 返回 HTTP 400。
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		request := clientModelRequest{
			body:  body,
			reply: make(chan clientModelResponse, 1),
		}
		model.requests <- request
		// 2. 等待测试回复或请求取消。
		var response clientModelResponse
		select {
		case response = <-request.reply:
		case <-r.Context().Done():
			return
		}
		// 3. 写出文本增量，第一段后按 release 等待放行或取消。
		w.Header().Set("Content-Type", "text/event-stream")
		for index, chunk := range response.chunks {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", chunk)
			if index == 0 && response.release != nil {
				w.(http.Flusher).Flush()
				select {
				case <-response.release:
				case <-r.Context().Done():
					return
				}
			}
		}
		// 4. 写出工具调用，按 finish 等待放行或取消，再写结束帧。
		for index, call := range response.calls {
			fmt.Fprintf(w,
				"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":%d,\"id\":%q,\"type\":\"function\",\"function\":{\"name\":%q,\"arguments\":%q}}]}}]}\n\n",
				index, call.ID, call.Name, call.Arguments,
			)
		}
		if response.finish != nil {
			w.(http.Flusher).Flush()
			select {
			case <-response.finish:
			case <-r.Context().Done():
				return
			}
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	// 2. 登记测试结束时关闭模型服务。
	t.Cleanup(model.server.Close)
	return model
}

// receive 读取下一个测试模型请求。
func (m *clientModel) receive(t *testing.T) clientModelRequest {
	t.Helper()
	// 1. 等待请求，十秒未到达则结束测试。
	select {
	case request := <-m.requests:
		return request
	case <-time.After(10 * time.Second):
		t.Fatal("model request did not arrive")
		return clientModelRequest{}
	}
}

// requireNoRequest 检查短时间内没有额外模型请求。
func (m *clientModel) requireNoRequest(t *testing.T) {
	t.Helper()
	// 1. 等待 200 毫秒，收到请求则结束测试。
	select {
	case request := <-m.requests:
		t.Fatalf("unexpected model request: %v", request.body)
	case <-time.After(200 * time.Millisecond):
	}
}

// receiveRootAndTask 读取同时到达的根和指定子任务模型请求。
func (m *clientModel) receiveRootAndTask(t *testing.T, task string) (root, child clientModelRequest) {
	t.Helper()
	// 1. 读取两个请求，按 TaskName 文本区分，缺少任一方则结束测试。
	for range 2 {
		request := m.receive(t)
		if strings.Contains(fmt.Sprint(request.body), "TaskName: "+task) {
			child = request
		} else {
			root = request
		}
	}
	if root.reply == nil || child.reply == nil {
		t.Fatal("missing root or child request")
	}
	return root, child
}

// TestClientSkillSubmissionReplay 检查断线重试的 Skill 输入只执行一次。
func TestClientSkillSubmissionReplay(t *testing.T) {
	// 1. 分别检查普通 Skill 提及和显式 skill 提及的断线重试。
	for _, text := range []string{"请用 /review \n 保留原文", "/review /skill:review"} {
		t.Run(text, func(t *testing.T) {
			// 1. 创建并加载 review Skill，设置 Runtime 定义。
			root := t.TempDir()
			path := filepath.Join(root, "review", "SKILL.md")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("---\nname: review\ndescription: review\n---\nPRIVATE_SKILL_BODY"), 0600); err != nil {
				t.Fatal(err)
			}
			work, err := workspace.NewLocalWorkspace(workspace.NewLocalWorkspaceOpt(root))
			if err != nil {
				t.Fatal(err)
			}
			skills, err := skill.LoadSkills(t.Context(), []skill.SkillDirectory{{
				Workspace: work,
				Path:      root,
				Strict:    true,
			}})
			if err != nil {
				t.Fatal(err)
			}
			h := startClientHarness(t, clientTestToken)
			definitions := *h.definitions
			definitions.Skills = skills
			if _, err := h.runtime.SetDefinitions(t.Context(), &definitions); err != nil {
				t.Fatal(err)
			}
			conversation := h.openNewConversation(t)
			// 2. 查询 Skill 目录，核对名称和正文未泄露。
			list, err := h.client.GetSkills(t.Context(), &runtimeclient.GetSkillsRequest{
				ConversationID: conversation.ID,
				AgentName:      "ignored",
			})
			if err != nil || len(list) != 1 || list[0].Name != "review" {
				t.Fatalf("list=%v err=%v", list, err)
			}
			body, _ := json.Marshal(list)
			if strings.Contains(string(body), "PRIVATE_SKILL_BODY") {
				t.Fatal("list leaks content")
			}
			// 3. 暂停重连，在提交响应经过代理时切断连接，再删除 Skill 文件。
			h.proxy.pause()
			isPaused := true
			defer func() {
				if isPaused {
					h.proxy.resume()
				}
			}()
			h.proxy.cutOnDownstream(`"item_id"`)
			submitted := make(chan error, 1)
			go func() {
				_, err := h.client.SubmitInput(t.Context(), &runtimeclient.SubmitInputRequest{
					ConversationID: conversation.ID,
					Content:        textContent(text),
				})
				submitted <- err
			}()
			request := h.model.receive(t)
			h.collectEvents(t, isConnectionState(runtimeclient.ConnectionReconnecting))
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			// 4. 恢复连接，等待同步和原请求重试完成，再结束模型轮次。
			h.proxy.cutOnDownstream("")
			h.proxy.resume()
			isPaused = false
			h.collectEvents(t, isSessionSynced)
			select {
			case err := <-submitted:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("replay did not finish")
			}
			request.reply <- clientModelResponse{chunks: []string{"done"}}
			h.collectEvents(t, isTurnFinished)
			// 5. 读取日志，核对输入和 Skill 指令各一条，新输入读取已删除文件时报错。
			page, err := h.runtime.ListItems(t.Context(), &runtime.ListItemsRequest{
				ConversationID: conversation.ID,
				SessionID:      conversation.RootSessionID,
				Order:          storage.DirectionAsc,
				Limit:          100,
			})
			if err != nil {
				t.Fatal(err)
			}
			inputCount, skillCount := 0, 0
			for _, logged := range page.Items {
				if logged.Item.Kind == defs.ItemMessage && itemText(logged.Item) == text {
					inputCount++
				}
				if logged.Item.Kind == defs.ItemSkillInstruction {
					skillCount++
				}
			}
			if inputCount != 1 || skillCount != 1 {
				t.Fatalf("inputs=%d skills=%d", inputCount, skillCount)
			}
			if _, err := h.client.SubmitInput(t.Context(), &runtimeclient.SubmitInputRequest{
				ConversationID: conversation.ID,
				Content:        textContent("/review"),
			}); err == nil {
				t.Fatal("new input did not reread deleted file")
			}
			h.model.requireNoRequest(t)
		})
	}
}

// TestClientMCPRelay 检查客户端 MCP 转发、工具调用、断线和进程重启。
func TestClientMCPRelay(t *testing.T) {
	// 1. 编译假 MCP server，列出工具、登记、断线、多对话和进程失败场景。
	binary := buildFakeMCPServer(t)
	tests := []struct {
		name string
		// command 是转发循环为每条流起的 MCP server 命令；为空时是假 MCP server。
		command string
		run     func(t *testing.T, h *clientHarness, relay *testRelay)
	}{
		{
			name: "tool_call_goes_through_the_client_connection",
			run: func(t *testing.T, h *clientHarness, relay *testRelay) {
				// 1. 登记客户端 MCP 并等待连接，提交工具调用。
				conversation := h.openNewConversation(t)
				h.setClientMCPServers(t, conversation.ID, "fake")
				h.waitClientMCPServer(t, conversation.ID, "fake", mcpclient.ConnectionStateConnected)
				h.submit(t, conversation, "回显")
				request := h.model.receive(t)
				if toolNames := getModelToolNames(request.body); !slices.Contains(toolNames, "mcp__fake__echo") {
					t.Fatalf("model tools = %v, want mcp__fake__echo", toolNames)
				}
				request.reply <- clientModelResponse{calls: []llmkit.ToolCall{{
					ID:        "echo",
					Name:      "mcp__fake__echo",
					Arguments: `{"text":"RELAY_ECHO_OK"}`,
				}}}
				// 2. 核对下一次模型请求含工具结果，完成本轮。
				next := h.model.receive(t)
				// 调用参数里出现一次，工具结果里再出现一次。
				if count := strings.Count(fmt.Sprint(next.body), "RELAY_ECHO_OK"); count < 2 {
					t.Fatalf("echo result missing from the next model request (%d occurrences): %v", count, next.body)
				}
				next.reply <- clientModelResponse{chunks: []string{"好"}}
				h.collectEvents(t, isTurnFinished)
			},
		},
		{
			name: "empty_registration_disconnects_the_stream_and_removes_tools",
			run: func(t *testing.T, h *clientHarness, relay *testRelay) {
				// 1. 登记客户端 MCP 后清空登记。
				conversation := h.openNewConversation(t)
				h.setClientMCPServers(t, conversation.ID, "fake")
				h.waitClientMCPServer(t, conversation.ID, "fake", mcpclient.ConnectionStateConnected)
				streamID := relay.waitConnect(t).StreamID
				h.setClientMCPServers(t, conversation.ID)
				// 2. 核对旧流断开，后续模型请求已无客户端工具。
				if disconnected := relay.waitDisconnect(t); disconnected != streamID {
					t.Fatalf("disconnected stream = %q, want %q", disconnected, streamID)
				}
				h.waitNoClientMCPServers(t, conversation.ID)
				h.submit(t, conversation, "没有工具了")
				request := h.model.receive(t)
				if toolNames := getModelToolNames(request.body); slices.Contains(toolNames, "mcp__fake__echo") {
					t.Fatalf("model tools = %v, want no client tools after the registration was removed", toolNames)
				}
				request.reply <- clientModelResponse{chunks: []string{"好"}}
				h.collectEvents(t, isTurnFinished)
			},
		},
		{
			// 客户端连接断开：serve 清掉这条连接的登记，进行中的调用以 mcp_server_unavailable 结束；重连后重新登记，新连接的流照常工作。
			name: "connection_loss_removes_registration_and_fails_in_flight_call",
			run: func(t *testing.T, h *clientHarness, relay *testRelay) {
				// 1. 开始 MCP 慢调用，调用到达转发流后切断客户端连接。
				conversation := h.openNewConversation(t)
				h.setClientMCPServers(t, conversation.ID, "fake")
				h.waitClientMCPServer(t, conversation.ID, "fake", mcpclient.ConnectionStateConnected)
				h.submit(t, conversation, "慢调用")
				h.model.receive(t).reply <- clientModelResponse{calls: []llmkit.ToolCall{{
					ID:        "slow",
					Name:      "mcp__fake__slow",
					Arguments: `{"seconds":30}`,
				}}}
				relay.waitToolCall(t)
				h.proxy.cut()
				// 2. 核对调用以真实连接原因失败，等待本轮结束和登记清空。
				failed := h.model.receive(t)
				// 失败原因里带着连接真实的结束原因（读失败或写失败都可能），不只是 context canceled。
				body := fmt.Sprint(failed.body)
				if !strings.Contains(body, "mcp_server_unavailable") || !strings.Contains(body, "client connection closed") || strings.Contains(body, "client connection closed: context canceled") {
					t.Fatalf("model request after the connection was lost = %s, want the call to fail with mcp_server_unavailable and a real connection end reason", body)
				}
				failed.reply <- clientModelResponse{chunks: []string{"好"}}
				h.waitTurnsCompleted(t, conversation.ID, conversation.RootSessionID, 1)
				h.waitNoClientMCPServers(t, conversation.ID)

				// 3. 重新登记并执行新一轮，核对工具恢复可用。
				h.setClientMCPServers(t, conversation.ID, "fake")
				h.waitClientMCPServer(t, conversation.ID, "fake", mcpclient.ConnectionStateConnected)
				h.submit(t, conversation, "重连后回显")
				request := h.model.receive(t)
				if toolNames := getModelToolNames(request.body); !slices.Contains(toolNames, "mcp__fake__echo") {
					t.Fatalf("model tools after re-registering = %v, want mcp__fake__echo", toolNames)
				}
				request.reply <- clientModelResponse{chunks: []string{"好"}}
				h.waitTurnsCompleted(t, conversation.ID, conversation.RootSessionID, 2)
			},
		},
		{
			// 同一个 server id 为两个对话登记：各有自己的流，消息按 stream_id 找到各自的传输，两个对话都能连上并拿到工具。
			name: "same_server_id_for_two_conversations",
			run: func(t *testing.T, h *clientHarness, relay *testRelay) {
				// 1. 为两个对话分别登记同名 server。
				first := h.openNewConversation(t)
				second, err := h.client.CreateConversation(t.Context(), &runtimeclient.CreateConversationRequest{AgentName: "assistant"})
				if err != nil {
					t.Fatal(err)
				}
				h.setClientMCPServers(t, first.ID, "fake")
				h.setClientMCPServers(t, second.ID, "fake")
				// 2. 核对两者均有工具，且分配不同 stream ID。
				for _, conversationID := range []defs.ConversationID{first.ID, second.ID} {
					server := h.waitClientMCPServer(t, conversationID, "fake", mcpclient.ConnectionStateConnected)
					if len(server.Tools) == 0 {
						t.Fatalf("conversation %s client MCP server = %+v, want its tools", conversationID, server)
					}
				}
				if a, b := relay.waitConnect(t).StreamID, relay.waitConnect(t).StreamID; a == b {
					t.Fatalf("both conversations connected on stream %q, want one stream each", a)
				}
			},
		},
		{
			// 本机 server 进程退出：转发循环发 clientMCP/closed，serve 立刻重连一次，换新的 stream_id，新进程起来后工具恢复。
			name: "local_server_exit_reconnects_on_a_new_stream",
			run: func(t *testing.T, h *clientHarness, relay *testRelay) {
				// 1. 登记 MCP 并等待首个流连接。
				conversation := h.openNewConversation(t)
				h.setClientMCPServers(t, conversation.ID, "fake")
				h.waitClientMCPServer(t, conversation.ID, "fake", mcpclient.ConnectionStateConnected)
				first := relay.waitConnect(t)
				// 2. 杀掉进程，核对重连使用新流 ID、同一 server 并恢复 connected。
				relay.killServer(t, first.StreamID)
				second := relay.waitConnect(t)
				if second.StreamID == first.StreamID || second.ServerID != "fake" {
					t.Fatalf("reconnect = %+v after %+v, want a new stream for the same server", second, first)
				}
				h.waitClientMCPServer(t, conversation.ID, "fake", mcpclient.ConnectionStateConnected)
			},
		},
		{
			// 客户端连不上本机 server：转发循环发 clientMCP/closed，serve 的连接状态是 failed 并带原因。
			name:    "client_connect_failure_is_reported_to_serve",
			command: "/nonexistent/mcp-server",
			run: func(t *testing.T, h *clientHarness, relay *testRelay) {
				// 1. 使用不存在的命令登记客户端 MCP。
				conversation := h.openNewConversation(t)
				h.setClientMCPServers(t, conversation.ID, "fake")
				// 2. 等待 failed 状态，核对包含本机连接失败原因。
				server := h.waitClientMCPServer(t, conversation.ID, "fake", mcpclient.ConnectionStateFailed)
				if !strings.Contains(server.Error, "/nonexistent/mcp-server") {
					t.Fatalf("client MCP server error = %q, want the client's connect error", server.Error)
				}
			},
		},
	}
	// 2. 逐项启动真实客户端链路和转发循环，核对工具、流 ID 和失败原因。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := startClientHarness(t, clientTestToken)
			command := tt.command
			if command == "" {
				command = binary
			}
			tt.run(t, h, h.startRelay(t, command))
		})
	}
}

// getModelToolNames 返回模型请求中的工具名。
func getModelToolNames(body map[string]any) []string {
	// 1. 从 tools 的 function 字段收集名称。
	var names []string
	tools, _ := body["tools"].([]any)
	for _, value := range tools {
		names = append(names, value.(map[string]any)["function"].(map[string]any)["name"].(string))
	}
	return names
}

// setClientMCPServers 替换本连接为对话登记的客户端 MCP server。
func (h *clientHarness) setClientMCPServers(t *testing.T, conversationID defs.ConversationID, serverIDs ...string) {
	t.Helper()
	// 1. 调用 Client，失败时结束测试。
	if err := h.client.SetClientMCPServers(t.Context(), conversationID, serverIDs); err != nil {
		t.Fatal(err)
	}
}

// waitClientMCPServer 等待指定客户端 MCP server 到达预期状态。
func (h *clientHarness) waitClientMCPServer(t *testing.T, conversationID defs.ConversationID, serverID string, state mcpclient.ConnectionState) mcpclient.ConnectionInfo {
	t.Helper()
	// 1. 轮询对话的客户端 MCP 列表，匹配 server ID 和状态后返回，超过十秒则失败。
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, server := range h.listClientMCPServers(t, conversationID) {
			if server.ServerID == serverID && server.State == state {
				return server.ConnectionInfo
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("client MCP servers = %+v, want %s in state %s", h.listClientMCPServers(t, conversationID), serverID, state)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitNoClientMCPServers 等待对话的客户端 MCP 列表为空。
func (h *clientHarness) waitNoClientMCPServers(t *testing.T, conversationID defs.ConversationID) {
	t.Helper()
	// 1. 轮询客户端 MCP 条目，空时返回，超过十秒则失败。
	deadline := time.Now().Add(10 * time.Second)
	for {
		servers := h.listClientMCPServers(t, conversationID)
		if len(servers) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("client MCP servers = %+v, want none", servers)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// listClientMCPServers 返回对话中 ExecutorID 为 client 的 MCP 连接。
func (h *clientHarness) listClientMCPServers(t *testing.T, conversationID defs.ConversationID) []conversation.MCPServerInfo {
	t.Helper()
	// 1. 查询 MCP 列表，失败时结束测试，再移除其他 Executor 条目。
	servers, err := h.client.ListMCPServers(t.Context(), conversationID)
	if err != nil {
		t.Fatal(err)
	}
	return slices.DeleteFunc(servers, func(server conversation.MCPServerInfo) bool { return server.ExecutorID != defs.ClientExecutorID })
}

// testRelay 扮演客户端一侧的转发循环（ACP 要做的事）：接管 Client 的事件流，每收到 ClientMCPConnectRequested，
// 就用 mcpclient.RunRelayStream 起一个 MCP server 进程并转发；serve 来的消息交给对应的流，serve 要求断开时关闭流，
// 与 serve 的连接断开重连时结束全部流；流因 server 一侧结束时发 clientMCP/closed。其余事件原样交给 collectEvents。
type testRelay struct {
	client  runtimeclient.Client
	command string
	// pidDirectory 里每条流有一个文件 <stream_id>.pid，是它的 MCP server 进程启动时写入的进程号；各流分开，进程同时启动时不会抢同一个文件。
	pidDirectory string
	ctx          context.Context
	tasks        sync.WaitGroup
	// streams 是还没结束的流；事件泵登记，流的 goroutine 结束时自己删除。
	streams     syncmap.SyncMap[string, *testRelayStream]
	connects    chan *runtimeclient.ClientMCPConnectRequested
	disconnects chan string
	toolCalls   chan string
}

// testRelayStream 是转发循环里的一条流。
type testRelayStream struct {
	incoming chan json.RawMessage
	cancel   context.CancelFunc
	// done 在流的 goroutine 结束（RunRelayStream 返回、已通知 serve）后关闭：事件泵投递消息时同时等它，不会卡在已经结束的流上。
	done chan struct{}
	// isDisconnected 在 serve 要求断开后为 true：这样结束的流不再向 serve 报告 closed。
	isDisconnected atomic.Bool
}

// startRelay 接管测试 Client 事件并启动 MCP 转发循环。
func (h *clientHarness) startRelay(t *testing.T, command string) *testRelay {
	t.Helper()
	// 1. 创建转发上下文、流登记和测试观察通道。
	ctx, cancel := context.WithCancel(context.Background())
	relay := &testRelay{
		client:       h.client,
		command:      command,
		pidDirectory: t.TempDir(),
		ctx:          ctx,
		connects:     make(chan *runtimeclient.ClientMCPConnectRequested, 16),
		disconnects:  make(chan string, 16),
		toolCalls:    make(chan string, 16),
	}
	source, passed := h.client.Events(), make(chan runtimeclient.ClientEvent, 256)
	h.events = passed
	// 2. 启动事件转发，消费 MCP 事件，其他事件交给 collectEvents，取消时退出。
	relay.tasks.Go(func() {
		for {
			var event runtimeclient.ClientEvent
			select {
			case event = <-source:
			case <-ctx.Done():
				return
			}
			if relay.handleEvent(event) {
				continue
			}
			select {
			case passed <- event:
			case <-ctx.Done():
				return
			}
		}
	})
	// 3. 登记清理：取消转发上下文并等全部转发任务结束。
	t.Cleanup(func() {
		cancel()
		relay.tasks.Wait()
	})
	return relay
}

// handleEvent 事件是否已由 MCP 转发循环处理？
func (r *testRelay) handleEvent(event runtimeclient.ClientEvent) bool {
	// 1. 按事件类型连接、投递或断开流；重连时取消并清除全部流，其余事件继续交付。
	switch event := event.(type) {
	case *runtimeclient.ClientMCPConnectRequested:
		r.startStream(event)
		sendRelayEvent(r.ctx, r.connects, event)
	case *runtimeclient.ClientMCPMessageReceived:
		stream, found := r.streams.Load(event.StreamID)
		if !found {
			return true
		}
		if strings.Contains(string(event.Message), `"tools/call"`) {
			sendRelayEvent(r.ctx, r.toolCalls, event.StreamID)
		}
		select {
		case stream.incoming <- event.Message:
		case <-stream.done:
		case <-r.ctx.Done():
		}
	case *runtimeclient.ClientMCPDisconnectRequested:
		if stream, found := r.streams.LoadAndDelete(event.StreamID); found {
			stream.isDisconnected.Store(true)
			close(stream.incoming)
		}
		sendRelayEvent(r.ctx, r.disconnects, event.StreamID)
	case *runtimeclient.ConnectionChanged:
		if event.State == runtimeclient.ConnectionReconnecting {
			r.streams.Range(func(streamID string, stream *testRelayStream) bool {
				stream.cancel()
				r.streams.Delete(streamID)
				return true
			})
		}
		return false
	default:
		return false
	}
	return true
}

// sendRelayEvent 交付 MCP 转发事件。
func sendRelayEvent[T any](ctx context.Context, ch chan<- T, value T) {
	// 1. 将事件写入通道，上下文取消时放弃发送。
	select {
	case ch <- value:
	case <-ctx.Done():
	}
}

// startStream 启动一条 MCP 进程转发流。
func (r *testRelay) startStream(event *runtimeclient.ClientMCPConnectRequested) {
	// 1. 创建流上下文和收件通道，登记流。
	ctx, cancel := context.WithCancel(r.ctx)
	stream := &testRelayStream{
		incoming: make(chan json.RawMessage),
		cancel:   cancel,
		done:     make(chan struct{}),
	}
	r.streams.Store(event.StreamID, stream)
	// 2. 启动进程转发，向 serve 写入使用转发循环上下文；正常断开之外的结束发送 closed，再取消、移除并关闭 done。
	r.tasks.Go(func() {
		defer func() {
			cancel()
			r.streams.Delete(event.StreamID)
			close(stream.done)
		}()
		send := func(message json.RawMessage) error {
			return r.client.SendClientMCPMessage(r.ctx, event.StreamID, message)
		}
		err := mcpclient.RunRelayStream(ctx, &mcp.CommandTransport{Command: exec.Command(r.command, "-pidfile", r.getPIDPath(event.StreamID))}, stream.incoming, send)
		if ctx.Err() == nil && !stream.isDisconnected.Load() {
			_ = r.client.SendClientMCPClosed(r.ctx, event.StreamID, err)
		}
	})
}

// getPIDPath 返回指定流的 MCP 进程号文件路径。
func (r *testRelay) getPIDPath(streamID string) string {
	// 1. 组合临时目录和 streamID.pid。
	return filepath.Join(r.pidDirectory, streamID+".pid")
}

// killServer 杀掉指定流的 MCP server 进程。
func (r *testRelay) killServer(t *testing.T, streamID string) {
	t.Helper()
	// 1. 读取进程号文件并解析 PID，发送 SIGKILL，任一步失败则结束测试。
	content, err := os.ReadFile(r.getPIDPath(streamID))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(content))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
}

// waitConnect 等待下一条 MCP 连接请求。
func (r *testRelay) waitConnect(t *testing.T) *runtimeclient.ClientMCPConnectRequested {
	t.Helper()
	// 1. 从观察通道读取，十秒未到达则结束测试。
	select {
	case event := <-r.connects:
		return event
	case <-time.After(10 * time.Second):
		t.Fatal("clientMCP/connect did not arrive")
		return nil
	}
}

// waitDisconnect 等待下一条 MCP 断开请求并返回流 ID。
func (r *testRelay) waitDisconnect(t *testing.T) string {
	t.Helper()
	// 1. 从观察通道读取，十秒未到达则结束测试。
	select {
	case streamID := <-r.disconnects:
		return streamID
	case <-time.After(10 * time.Second):
		t.Fatal("clientMCP/disconnect did not arrive")
		return ""
	}
}

// waitToolCall 等待工具调用到达转发流并返回流 ID。
func (r *testRelay) waitToolCall(t *testing.T) string {
	t.Helper()
	// 1. 从观察通道读取，十秒未到达则结束测试。
	select {
	case streamID := <-r.toolCalls:
		return streamID
	case <-time.After(10 * time.Second):
		t.Fatal("tools/call did not reach the client")
		return ""
	}
}

// TestClientConversationList 检查真实对话列表的筛选、分页和根 Session。
func TestClientConversationList(t *testing.T) {
	// 1. 遍历目录、大小写、归档、空结果和根记录缺失场景。
	for _, tt := range []struct {
		name        string
		directories []string
		searchTerm  string
		archived    bool
		missingRoot bool
		wantIDs     []defs.ConversationID
	}{
		{
			name:        "directory_and_title",
			directories: []string{"/project"},
			searchTerm:  "bug",
			wantIDs:     []defs.ConversationID{"a", "d"},
		},
		{
			name:        "multiple_directories",
			directories: []string{"/project", "/other"},
			searchTerm:  "bug",
			wantIDs:     []defs.ConversationID{"a", "b", "d"},
		},
		{
			name:       "case_sensitive",
			searchTerm: "BUG",
			wantIDs:    []defs.ConversationID{"c"},
		},
		{
			name:        "archived",
			directories: []string{"/project"},
			searchTerm:  "bug",
			archived:    true,
			wantIDs:     []defs.ConversationID{"e"},
		},
		{
			name:        "empty",
			directories: []string{"/missing"},
		},
		{
			name:        "root_missing_is_error",
			directories: []string{"/project"},
			searchTerm:  "bug",
			missingRoot: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// 1. 写入多个目录、标题和归档状态的对话及根 Session，按场景省略根记录。
			h := startClientHarness(t, clientTestToken)
			for _, row := range []struct {
				id        defs.ConversationID
				directory string
				title     string
				archived  int64
			}{
				{
					"a",
					"/project",
					"fix bug",
					0,
				},
				{
					"b",
					"/other",
					"fix bug",
					0,
				},
				{
					"c",
					"/project",
					"fix BUG",
					0,
				},
				{
					"d",
					"/project",
					"another bug",
					0,
				},
				{
					"e",
					"/project",
					"archived bug",
					1,
				},
			} {
				conversation := defs.Conversation{
					ID:               row.id,
					RootSessionID:    defs.SessionID("root-" + row.id),
					Title:            row.title,
					ArchivedAt:       row.archived,
					ExecutorSettings: defs.ExecutorSettings{WorkspaceDirectories: map[string]string{defs.LocalExecutorID: row.directory}},
				}
				if !(tt.missingRoot && row.id == "a") {
					root := defs.Session{
						ID:             conversation.RootSessionID,
						ConversationID: row.id,
						AgentName:      "assistant",
						Model: defs.ModelSelection{
							Ref: defs.ModelRef{
								ProviderID: "test",
								ModelID:    "second",
							},
							ReasoningEffort: "high",
						},
					}
					if err := h.mutation.SetSession(t.Context(), root); err != nil {
						t.Fatal(err)
					}
				}
				if err := h.mutation.SetConversation(t.Context(), conversation); err != nil {
					t.Fatal(err)
				}
			}
			// 2. 每页只查一条，沿游标读取筛选结果，核对根 Session 模型或缺少根记录错误。
			request := &runtimeclient.ListConversationsRequest{
				Directories: tt.directories,
				SearchTerm:  tt.searchTerm,
				Archived:    tt.archived,
				Limit:       1,
			}
			var got []defs.ConversationID
			for {
				page, err := h.client.ListConversations(t.Context(), request)
				if tt.missingRoot {
					var response *runtimeclient.ResponseError
					if !errors.As(err, &response) || response.Code != -32000 || !strings.Contains(response.Message, `root session "root-a" not found`) {
						t.Fatalf("ListConversations error=%v, want missing root error through the protocol", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range page.Conversations {
					got = append(got, entry.Conversation.ID)
					if entry.RootSession.ID != entry.Conversation.RootSessionID || entry.RootSession.ConversationID != entry.Conversation.ID ||
						entry.RootSession.Model.Ref.ModelID != "second" || entry.RootSession.Model.ReasoningEffort != "high" {
						t.Fatalf("entry=%+v, want corresponding root with the saved model", entry)
					}
				}
				if page.NextCursor.ID == "" {
					break
				}
				if page.NextCursor == request.Cursor {
					t.Fatalf("cursor did not progress: %+v", page.NextCursor)
				}
				request.Cursor = page.NextCursor
			}
			// 3. 核对完整分页结果的对话 ID 顺序。
			if !slices.Equal(got, tt.wantIDs) {
				t.Fatalf("listed IDs=%v, want %v", got, tt.wantIDs)
			}
		})
	}
}

// TestClientPendingInputOperations 检查真实 Go 客户端订阅与三种原子队列请求。
func TestClientPendingInputOperations(t *testing.T) {
	// 1. 分别运行转补充后取回、打断优先和永久撤回。
	for _, action := range []string{"steer_and_return", "interrupt_priority", "withdraw"} {
		t.Run(action, func(t *testing.T) {
			h := startClientHarness(t, clientTestToken)
			conversation := h.openNewConversation(t)
			h.submit(t, conversation, "first")
			h.model.receive(t)
			started := h.collectEvents(t, func(event runtimeclient.ClientEvent) bool { _, ok := event.(*runtimeclient.TurnStarted); return ok })
			turnID := started[len(started)-1].(*runtimeclient.TurnStarted).TurnID
			content := []*defs.Content{
				{Kind: defs.ContentText, Text: &defs.TextContent{Text: "queued"}},
				{Kind: defs.ContentText, Text: &defs.TextContent{Text: "context", IsContext: true}},
				{Kind: defs.ContentImage, Image: &defs.ImageContent{DataURL: "data:image/png;base64,a", Alt: "image"}},
			}
			itemID, err := h.client.SubmitInput(t.Context(), &runtimeclient.SubmitInputRequest{ConversationID: conversation.ID, Mode: session.SessionInputModeNormal, Content: content})
			if err != nil {
				t.Fatal(err)
			}
			h.collectEvents(t, func(event runtimeclient.ClientEvent) bool {
				pending, ok := event.(*runtimeclient.SessionPendingInputsChanged)
				return ok && len(pending.PendingInputs.Inputs) == 1 && pending.PendingInputs.Inputs[0].ItemID == itemID
			})
			// 2. 重建订阅读到完整待处理快照。
			if _, err := h.client.OpenConversation(t.Context(), &runtimeclient.OpenConversationRequest{ConversationID: conversation.ID}); err != nil {
				t.Fatal(err)
			}
			syncedEvents := h.collectEvents(t, func(event runtimeclient.ClientEvent) bool { _, ok := event.(*runtimeclient.SessionSynced); return ok })
			synced := syncedEvents[len(syncedEvents)-1].(*runtimeclient.SessionSynced)
			if len(synced.PendingInputs.Inputs) != 1 || synced.PendingInputs.SteerTurnID != turnID || synced.PendingInputs.Inputs[0].ImageCount != 1 {
				t.Fatalf("snapshot = %+v", synced.PendingInputs)
			}
			// 3. 调用客户端队列操作，检查返回内容和服务端通知。
			switch action {
			case "steer_and_return":
				if err := h.client.SteerInput(t.Context(), conversation.ID, itemID, turnID); err != nil {
					t.Fatal(err)
				}
				h.collectEvents(t, func(event runtimeclient.ClientEvent) bool {
					pending, ok := event.(*runtimeclient.SessionPendingInputsChanged)
					return ok && len(pending.PendingInputs.Inputs) == 1 && pending.PendingInputs.Inputs[0].IsSteer
				})
				returned, err := h.client.WithdrawInput(t.Context(), conversation.ID, itemID, true)
				if err != nil || !reflect.DeepEqual(returned, content) {
					t.Fatalf("returned = %+v, %v", returned, err)
				}
			case "interrupt_priority":
				if err := h.client.InterruptTurnWithInput(t.Context(), conversation.ID, turnID, itemID); err != nil {
					t.Fatal(err)
				}
				h.collectEvents(t, isTurnFinished)
				h.model.receive(t).reply <- clientModelResponse{chunks: []string{"done"}}
				finished := h.collectEvents(t, isTurnFinished)
				if result := finished[len(finished)-1].(*runtimeclient.TurnFinished); !slices.Equal(result.InputItemIDs, []defs.ItemID{itemID}) || result.TurnID == turnID {
					t.Fatalf("finished = %+v", result)
				}
				return
			case "withdraw":
				returned, err := h.client.WithdrawInput(t.Context(), conversation.ID, itemID, false)
				if err != nil || returned != nil {
					t.Fatalf("withdraw = %+v, %v", returned, err)
				}
			}
			h.collectEvents(t, func(event runtimeclient.ClientEvent) bool {
				pending, ok := event.(*runtimeclient.SessionPendingInputsChanged)
				return ok && len(pending.PendingInputs.Inputs) == 0 && pending.PendingInputs.SteerTurnID == turnID
			})
			if _, err := h.client.WithdrawInput(t.Context(), conversation.ID, itemID, true); err == nil {
				t.Fatal("withdrawn input accepted again")
			}
		})
	}
}

// TestClientRunningConversations 检查无阅读目标时的全局订阅、真实 Turn 集合及订阅恢复。
func TestClientRunningConversations(t *testing.T) {
	// 1. 分别保持连接、模拟订阅关闭及切断连接。
	for _, action := range []string{"live", "subscription_closed", "reconnect"} {
		t.Run(action, func(t *testing.T) {
			h := startClientHarness(t, clientTestToken)
			h.collectEvents(t, isConnectionState(runtimeclient.ConnectionConnected))
			for range 2 {
				if err := h.client.SubscribeRunningConversations(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			initial := h.collectEvents(t, func(event runtimeclient.ClientEvent) bool {
				e, ok := event.(*runtimeclient.RunningConversationsChanged)
				return ok && e.IsKnown
			})
			if ids := initial[len(initial)-1].(*runtimeclient.RunningConversationsChanged).ConversationIDs; len(ids) != 0 {
				t.Fatalf("initial running = %v", ids)
			}
			subscriptions := 0
			for _, frame := range h.proxy.getUpstreamFrames() {
				if strings.Contains(frame, `"method":"conversation/running/subscribe"`) {
					subscriptions++
				}
			}
			if subscriptions != 1 {
				t.Fatalf("duplicate subscriptions = %d", subscriptions)
			}
			// 2. 只创建、不跟随对话，执行中的 Turn 仍出现在全局集合。
			record, err := h.client.CreateConversation(t.Context(), &runtimeclient.CreateConversationRequest{AgentName: "assistant"})
			if err != nil {
				t.Fatal(err)
			}
			h.submit(t, record, "run")
			request := h.model.receive(t)
			h.collectEvents(t, func(event runtimeclient.ClientEvent) bool {
				e, ok := event.(*runtimeclient.RunningConversationsChanged)
				return ok && e.IsKnown && slices.Contains(e.ConversationIDs, record.ID)
			})
			switch action {
			case "subscription_closed":
				h.proxy.injectDownstream(t, map[string]any{"method": "conversation/runningClosed", "params": map[string]string{"reason": "test closed"}})
			case "reconnect":
				h.proxy.cut()
			}
			if action != "live" {
				h.collectEvents(t, func(event runtimeclient.ClientEvent) bool {
					e, ok := event.(*runtimeclient.RunningConversationsChanged)
					return ok && !e.IsKnown
				})
				h.collectEvents(t, func(event runtimeclient.ClientEvent) bool {
					e, ok := event.(*runtimeclient.RunningConversationsChanged)
					return ok && e.IsKnown && slices.Contains(e.ConversationIDs, record.ID)
				})
			}
			// 3. Turn 完成后收到已知的空集合，不需要打开阅读订阅。
			request.reply <- clientModelResponse{chunks: []string{"done"}}
			h.collectEvents(t, func(event runtimeclient.ClientEvent) bool {
				e, ok := event.(*runtimeclient.RunningConversationsChanged)
				return ok && e.IsKnown && len(e.ConversationIDs) == 0
			})
		})
	}
}

// TestClientContentSessionID 检查快照和全部跟随内容事件保留真实 Session 身份。
func TestClientContentSessionID(t *testing.T) {
	// 1. 表驱动覆盖快照及七类线上内容通知。
	for _, method := range []string{"snapshot", "items/saved", "item/delta", "turn/started", "turn/finished", "session/pendingInputsUpdated", "item/toolProgress", "hook/started"} {
		t.Run(method, func(t *testing.T) {
			h := startClientHarness(t, clientTestToken)
			record, err := h.client.CreateConversation(t.Context(), &runtimeclient.CreateConversationRequest{AgentName: "assistant"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := h.client.OpenConversation(t.Context(), &runtimeclient.OpenConversationRequest{ConversationID: record.ID, SkipHistory: true}); err != nil {
				t.Fatal(err)
			}
			synced := h.waitSessionSynced(t)
			if synced.SessionID != record.RootSessionID {
				t.Fatalf("snapshot session = %s", synced.SessionID)
			}
			if method == "snapshot" {
				return
			}
			// 2. 注入同对话其他 Session 及目标 Session 的通知，只有后者交付。
			for _, sessionID := range []defs.SessionID{"unfollowed", record.RootSessionID} {
				h.proxy.injectDownstream(t, map[string]any{"method": method, "params": map[string]any{
					"subscription": "session", "conversation_id": record.ID, "session_id": sessionID,
					"turn_id": "turn", "item_index": 0, "items": []map[string]any{{"ordinal": synced.LastSavedOrdinal + 1}},
					"tool_progress": map[string]any{}, "hook_run": map[string]any{},
				}})
			}
			events := h.collectEvents(t, func(event runtimeclient.ClientEvent) bool {
				switch event.(type) {
				case *runtimeclient.ItemsSaved, *runtimeclient.ItemChanged, *runtimeclient.TurnStarted, *runtimeclient.TurnFinished, *runtimeclient.SessionPendingInputsChanged, *runtimeclient.ToolProgress, *runtimeclient.HookRunChanged:
					return true
				}
				return false
			})
			var sessionID defs.SessionID
			switch e := events[len(events)-1].(type) {
			case *runtimeclient.ItemsSaved:
				sessionID = e.SessionID
			case *runtimeclient.ItemChanged:
				sessionID = e.SessionID
			case *runtimeclient.TurnStarted:
				sessionID = e.SessionID
			case *runtimeclient.TurnFinished:
				sessionID = e.SessionID
			case *runtimeclient.SessionPendingInputsChanged:
				sessionID = e.SessionID
			case *runtimeclient.ToolProgress:
				sessionID = e.SessionID
			case *runtimeclient.HookRunChanged:
				sessionID = e.SessionID
			}
			if sessionID != record.RootSessionID {
				t.Fatalf("%s session = %s", method, sessionID)
			}
		})
	}
}

// startProtocolClient 启动真实 WebSocket Client 和逐帧控制的协议服务，测试结束时取消并等待两端。
func startProtocolClient(t *testing.T, opt runtimeclient.ClientOpt, handle func(context.Context, *websocket.Conn, runtimeprotocol.JSONRPCRequest) bool) (runtimeclient.Client, <-chan *websocket.Conn) {
	t.Helper()
	// 1. 服务先让测试处理请求，未接管的 initialize 与查询返回基本响应。
	ctx, cancel := context.WithCancel(t.Context())
	connections := make(chan *websocket.Conn, 32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		select {
		case connections <- conn:
		case <-ctx.Done():
			return
		}
		for {
			_, frame, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var request runtimeprotocol.JSONRPCRequest
			if err := json.Unmarshal(frame, &request); err != nil {
				t.Error(err)
				return
			}
			if handle != nil && handle(ctx, conn, request) {
				continue
			}
			var result any = map[string]any{}
			if request.Method == "initialize" {
				result = runtimeprotocol.InitializeResult{ProtocolVersion: runtimeprotocol.ProtocolVersion, ServerID: "test"}
			}
			payload, _ := json.Marshal(runtimeprotocol.JSONRPCMessage{ID: request.ID, Result: result})
			if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
				return
			}
		}
	}))
	// 2. 启动 Client，收尾时持续排空业务事件，不等待 UI 回调。
	opt.URL = "ws" + strings.TrimPrefix(server.URL, "http")
	client, err := runtimeclient.NewClient(&opt)
	if err != nil {
		cancel()
		server.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		for range client.Events() {
		}
		<-done
		server.Close()
	})
	return client, connections
}

// waitProtocolEvent 持续消费事件，直到指定公开表现出现。
func waitProtocolEvent(t *testing.T, client runtimeclient.Client, matches func(runtimeclient.ClientEvent) bool) runtimeclient.ClientEvent {
	t.Helper()
	// 1. 五秒内保持消费，连接提前结束或超时则报告失败。
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case event, ok := <-client.Events():
			if !ok {
				t.Fatal("client stopped before expected event")
			}
			if matches(event) {
				return event
			}
		case <-timeout.C:
			t.Fatal("timed out waiting for client event")
		}
	}
}

// TestClientRequestPolicies 检查两个独立选项的全部组合及同 InputID 重试。
func TestClientRequestPolicies(t *testing.T) {
	// 1. 分别检查发出前断线与发出后失去响应。
	for _, disconnected := range []bool{false, true} {
		for _, disableRetry := range []bool{false, true} {
			for _, beforeSend := range []bool{false, true} {
				t.Run(fmt.Sprintf("fail_offline_%t_disable_retry_%t_before_%t", disconnected, disableRetry, beforeSend), func(t *testing.T) {
					var calls atomic.Int32
					var inputIDs syncmap.SyncMap[int32, string]
					gate := make(chan struct{})
					var handshakes atomic.Int32
					client, connections := startProtocolClient(t, runtimeclient.ClientOpt{FailRequestsWhenDisconnected: disconnected, DisableSubmitInputRetry: disableRetry}, func(ctx context.Context, conn *websocket.Conn, request runtimeprotocol.JSONRPCRequest) bool {
						if request.Method == "initialize" && handshakes.Add(0) >= 1 && beforeSend {
							select {
							case <-gate:
							case <-ctx.Done():
							}
							return false
						}
						if request.Method == "initialize" {
							handshakes.Add(1)
							return false
						}
						if request.Method != "conversation/submitInput" {
							return false
						}
						call := calls.Add(1)
						var params struct {
							InputID string `json:"input_id"`
						}
						_ = json.Unmarshal(request.Params, &params)
						inputIDs.Store(call, params.InputID)
						if !beforeSend && call == 1 {
							_ = conn.CloseNow()
							return true
						}
						payload, _ := json.Marshal(runtimeprotocol.JSONRPCMessage{ID: request.ID, Result: runtimeprotocol.SubmitInputResult{ItemID: "accepted"}})
						_ = conn.Write(ctx, websocket.MessageText, payload)
						return true
					})
					waitProtocolEvent(t, client, isConnectionState(runtimeclient.ConnectionConnected))
					if beforeSend {
						conn := <-connections
						_ = conn.CloseNow()
						waitProtocolEvent(t, client, isConnectionState(runtimeclient.ConnectionReconnecting))
					}
					// 2. 调用公开提交，断线前拒绝不重发；已发出后的重试仅由另一个选项控制。
					result := make(chan error, 1)
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()
					go func() {
						_, err := client.SubmitInput(ctx, &runtimeclient.SubmitInputRequest{ConversationID: "conversation", Content: textContent("hello")})
						result <- err
					}()
					if beforeSend {
						if disconnected {
							select {
							case err := <-result:
								if !errors.Is(err, runtimeclient.ErrConnectionLost) {
									t.Fatalf("offline submit: %v", err)
								}
							case <-ctx.Done():
								t.Fatal("offline submit waited")
							}
							if calls.Load() != 0 {
								t.Fatal("offline submit sent a request")
							}
							close(gate)
							return
						}
						select {
						case err := <-result:
							t.Fatalf("default submit returned early: %v", err)
						case <-time.After(30 * time.Millisecond):
						}
						close(gate)
					}
					var err error
					select {
					case err = <-result:
					case <-ctx.Done():
						t.Fatal("submit did not finish")
					}
					if !beforeSend && disableRetry {
						if !errors.Is(err, runtimeclient.ErrConnectionLost) || calls.Load() != 1 {
							t.Fatalf("no retry = %v, %d calls", err, calls.Load())
						}
					} else {
						if err != nil {
							t.Fatal(err)
						}
						if !beforeSend {
							first, _ := inputIDs.Load(1)
							second, _ := inputIDs.Load(2)
							if calls.Load() != 2 || first == "" || first != second {
								t.Fatalf("retry IDs %q %q, calls %d", first, second, calls.Load())
							}
						}
					}
				})
			}
		}
	}
}

// TestClientManualReconnect 检查跳过退避、连接中合并和已连接不重建连接。
func TestClientManualReconnect(t *testing.T) {
	// 1. 两种场景均通过真实 WebSocket 检查额外连接数量。
	for _, action := range []string{"connected_noop", "skip_backoff_and_merge"} {
		t.Run(action, func(t *testing.T) {
			gate := make(chan struct{})
			entered := make(chan struct{}, 1)
			var handshakes atomic.Int32
			client, connections := startProtocolClient(t, runtimeclient.ClientOpt{}, func(ctx context.Context, _ *websocket.Conn, r runtimeprotocol.JSONRPCRequest) bool {
				if r.Method == "initialize" && handshakes.Add(1) > 1 {
					entered <- struct{}{}
					select {
					case <-gate:
					case <-ctx.Done():
					}
				}
				return false
			})
			waitProtocolEvent(t, client, isConnectionState(runtimeclient.ConnectionConnected))
			first := <-connections
			if action == "skip_backoff_and_merge" {
				_ = first.CloseNow()
				waitProtocolEvent(t, client, isConnectionState(runtimeclient.ConnectionReconnecting))
				if err := client.Reconnect(t.Context()); err != nil {
					t.Fatal(err)
				}
				select {
				case <-entered:
				case <-time.After(800 * time.Millisecond):
					t.Fatal("manual reconnect did not skip one-second backoff")
				}
			}
			for range 10 {
				if err := client.Reconnect(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if action == "skip_backoff_and_merge" {
				close(gate)
				waitProtocolEvent(t, client, isConnectionState(runtimeclient.ConnectionConnected))
				<-connections
			}
			select {
			case <-connections:
				t.Fatal("unexpected duplicate connection")
			case <-time.After(80 * time.Millisecond):
			}
		})
	}
}

// TestClientRunningSubscriptionRecovery 检查全局订阅失败、通知早于任务结果及快速关闭时的恢复。
func TestClientRunningSubscriptionRecovery(t *testing.T) {
	// 1. 用线上响应顺序覆盖错误和快照竞争，正常查询不依赖全局集合成功。
	for _, mode := range []string{"request_error", "changed_after_response", "closed_after_response"} {
		t.Run(mode, func(t *testing.T) {
			var subscriptions atomic.Int32
			client, _ := startProtocolClient(t, runtimeclient.ClientOpt{}, func(ctx context.Context, conn *websocket.Conn, request runtimeprotocol.JSONRPCRequest) bool {
				if request.Method == "conversation/running/unsubscribe" && mode == "request_error" {
					_ = conn.Write(ctx, websocket.MessageText, []byte(`{"method":"conversation/runningChanged","params":{"conversation_ids":["stale"]}}`))
				}
				if request.Method != "conversation/running/subscribe" {
					return false
				}
				attempt := subscriptions.Add(1)
				response := map[string]any{"id": request.ID, "result": map[string]any{"conversation_ids": []string{"initial"}}}
				if mode == "request_error" && attempt == 1 {
					delete(response, "result")
					response["error"] = map[string]any{"code": -32603, "message": "temporary subscription error"}
				}
				payload, _ := json.Marshal(response)
				_ = conn.Write(ctx, websocket.MessageText, payload)
				if mode == "changed_after_response" {
					_ = conn.Write(ctx, websocket.MessageText, []byte(`{"method":"conversation/runningChanged","params":{"conversation_ids":["latest"]}}`))
				}
				if mode == "closed_after_response" && attempt == 1 {
					_ = conn.Write(ctx, websocket.MessageText, []byte(`{"method":"conversation/runningClosed","params":{"reason":"closed by server"}}`))
				}
				return true
			})
			waitProtocolEvent(t, client, isConnectionState(runtimeclient.ConnectionConnected))
			if err := client.SubscribeRunningConversations(t.Context()); err != nil {
				t.Fatal(err)
			}
			if mode != "changed_after_response" {
				waitProtocolEvent(t, client, func(event runtimeclient.ClientEvent) bool {
					changed, ok := event.(*runtimeclient.RunningConversationsChanged)
					return ok && !changed.IsKnown && changed.Err != nil
				})
				if _, err := client.ListModels(t.Context(), ""); err != nil {
					t.Fatalf("subscription error stopped ordinary request: %v", err)
				}
			}
			wanted := defs.ConversationID("initial")
			if mode == "changed_after_response" {
				wanted = "latest"
			}
			waitProtocolEvent(t, client, func(event runtimeclient.ClientEvent) bool {
				changed, ok := event.(*runtimeclient.RunningConversationsChanged)
				return ok && changed.IsKnown && slices.Equal(changed.ConversationIDs, []defs.ConversationID{wanted})
			})
			if mode == "request_error" {
				select {
				case event := <-client.Events():
					if changed, ok := event.(*runtimeclient.RunningConversationsChanged); ok && slices.Contains(changed.ConversationIDs, "stale") {
						t.Fatal("old subscription overwrote recovered collection")
					}
				case <-time.After(50 * time.Millisecond):
				}
			}
			if mode != "changed_after_response" && subscriptions.Load() != 2 {
				t.Fatalf("subscriptions = %d", subscriptions.Load())
			}
		})
	}
}

// TestClientRejectedProtocolControls 检查终止性协议错误不会被手动重连或观察操作复活。
func TestClientRejectedProtocolControls(t *testing.T) {
	// 1. initialize 拒绝后事件流关闭；所有新增公开控制入口返回已停止。
	for _, operation := range []string{"reconnect", "running", "frames", "clear", "pause"} {
		t.Run(operation, func(t *testing.T) {
			client, _ := startProtocolClient(t, runtimeclient.ClientOpt{}, func(ctx context.Context, conn *websocket.Conn, request runtimeprotocol.JSONRPCRequest) bool {
				payload, _ := json.Marshal(map[string]any{"id": request.ID, "error": map[string]any{"code": -32602, "message": "bad protocol"}})
				_ = conn.Write(ctx, websocket.MessageText, payload)
				return true
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			for {
				select {
				case _, ok := <-client.Events():
					if !ok {
						goto stopped
					}
				case <-ctx.Done():
					t.Fatal("rejected client did not stop")
				}
			}
		stopped:
			var err error
			switch operation {
			case "reconnect":
				err = client.Reconnect(ctx)
			case "running":
				err = client.SubscribeRunningConversations(ctx)
			case "frames":
				_, err = client.GetRPCFrames(ctx)
			case "clear":
				err = client.ClearRPCFrames(ctx)
			case "pause":
				err = client.SetRPCLogPaused(ctx, true)
			}
			if !errors.Is(err, runtimeclient.ErrClientStopped) {
				t.Fatalf("%s: %v", operation, err)
			}
		})
	}
}

// TestClientSessionSwitchQueuedEvents 检查同对话切换阅读目标后，尚未消费的旧快照仍保留原 Session。
func TestClientSessionSwitchQueuedEvents(t *testing.T) {
	// 1. 创建真实根子 Session，分别执行根到子及子到根的切换。
	for _, childFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("child_first_%t", childFirst), func(t *testing.T) {
			h := startClientHarness(t, clientTestToken)
			record := h.openNewConversation(t)
			child := h.delegateReview(t, record)
			first, second := record.RootSessionID, child.ID
			if childFirst {
				first, second = second, first
			}
			// 2. 两次打开之间不消费 Events，检查读取时不会用最新目标重写旧快照归属。
			for _, sessionID := range []defs.SessionID{first, second} {
				if _, err := h.client.OpenConversation(t.Context(), &runtimeclient.OpenConversationRequest{ConversationID: record.ID, SessionID: sessionID, SkipHistory: true}); err != nil {
					t.Fatal(err)
				}
			}
			firstSnapshot := h.waitSessionSynced(t)
			secondSnapshot := h.waitSessionSynced(t)
			if firstSnapshot.SessionID != first || secondSnapshot.SessionID != second {
				t.Fatalf("queued sessions = %s, %s; want %s, %s", firstSnapshot.SessionID, secondSnapshot.SessionID, first, second)
			}
		})
	}
}

// TestClientInitializeTimeout 检查 WebSocket 已连接但初始化不回应时会超时，并可手动跳过退避恢复。
func TestClientInitializeTimeout(t *testing.T) {
	// 1. 第一条连接不回复 initialize，第二条按正常协议初始化。
	for _, mode := range []string{"manual_after_timeout"} {
		t.Run(mode, func(t *testing.T) {
			var attempts atomic.Int32
			client, _ := startProtocolClient(t, runtimeclient.ClientOpt{}, func(_ context.Context, _ *websocket.Conn, request runtimeprotocol.JSONRPCRequest) bool {
				return request.Method == "initialize" && attempts.Add(1) == 1
			})
			// 2. 初始化期限结束后交付重连原因，手动操作能立即开启新的尝试。
			deadline := time.NewTimer(13 * time.Second)
			defer deadline.Stop()
			for {
				select {
				case event := <-client.Events():
					if changed, ok := event.(*runtimeclient.ConnectionChanged); ok && changed.State == runtimeclient.ConnectionReconnecting {
						if !errors.Is(changed.Err, context.DeadlineExceeded) {
							t.Fatalf("initialize timeout reason = %v", changed.Err)
						}
						if err := client.Reconnect(t.Context()); err != nil {
							t.Fatal(err)
						}
						waitProtocolEvent(t, client, isConnectionState(runtimeclient.ConnectionConnected))
						if attempts.Load() != 2 {
							t.Fatalf("attempts = %d", attempts.Load())
						}
						if _, err := client.ListModels(t.Context(), ""); err != nil {
							t.Fatal(err)
						}
						return
					}
				case <-deadline.C:
					t.Fatal("initialize never timed out")
				}
			}
		})
	}
}
