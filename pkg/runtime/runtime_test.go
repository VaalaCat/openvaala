package runtime_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
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
	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/skill"
	"github.com/VaalaCat/openvaala/pkg/storage"
	storagefile "github.com/VaalaCat/openvaala/pkg/storage/file"
	storagesqlite "github.com/VaalaCat/openvaala/pkg/storage/sqlite"
	"github.com/VaalaCat/openvaala/pkg/telemetry/telemetrytest"
	"github.com/VaalaCat/openvaala/pkg/utils/syncmap"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/crypto/ssh"
)

// runtimeBuildDirectory 是本包测试共用的编译目录，TestMain 创建，全部测试结束后删除。
var runtimeBuildDirectory string

// TestMain 管理本包测试使用的共享编译目录。
func TestMain(m *testing.M) {
	// 1. 创建临时编译目录并保存路径。
	directory, err := os.MkdirTemp("", "openvaala-runtime-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	runtimeBuildDirectory = directory
	// 2. 运行本包测试，删除目录后返回测试退出码。
	code := m.Run()
	_ = os.RemoveAll(directory)
	os.Exit(code)
}

// buildFakeMCPServerOnce 在第一次用到时把 pkg/mcpclient 的假 MCP server 编译到 runtimeBuildDirectory；
// 测试本身带 -race 时同样带 -race 编译。
var buildFakeMCPServerOnce = sync.OnceValues(func() (string, error) {
	// 1. 取得包根目录和输出路径，测试带 -race 时也以 -race 编译假 MCP server。
	binary := filepath.Join(runtimeBuildDirectory, "mcpserver")
	arguments := []string{"build", "-o", binary}
	if isRaceEnabled() {
		arguments = append(arguments, "-race")
	}
	command := exec.Command("go", append(arguments, "../mcpclient/testdata/mcpserver")...)
	command.Env = append(os.Environ(), "GOWORK=off")
	if output, err := command.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build fake MCP server: %w\n%s", err, output)
	}
	return binary, nil
})

// buildFakeMCPServer 取得本包共享的假 MCP server 二进制。
func buildFakeMCPServer(t *testing.T) string {
	t.Helper()
	// 1. 调用只编译一次的构造，编译失败则结束测试。
	binary, err := buildFakeMCPServerOnce()
	if err != nil {
		t.Fatal(err)
	}
	return binary
}

// isRaceEnabled 测试二进制是否带 -race 编译？
func isRaceEnabled() bool {
	// 1. 读取编译信息，检查 -race 是否为 true。
	info, ok := debug.ReadBuildInfo()
	return ok && slices.ContainsFunc(info.Settings, func(setting debug.BuildSetting) bool {
		return setting.Key == "-race" && setting.Value == "true"
	})
}

// TestNewRuntime 检查 Runtime 必需依赖和空闲时长校验。
func TestNewRuntime(t *testing.T) {
	// 1. 创建共享存储、测试模型及待校验配置。
	store := openRuntimeStore(t)
	valid := newTestRuntimeOpt(store, newRuntimeModel(t))
	tests := []struct {
		name    string
		opt     *runtime.RuntimeOpt
		wantErr bool
	}{
		{
			name:    "nil_options",
			opt:     nil,
			wantErr: true,
		},
		{
			name:    "missing_records",
			opt:     runtime.NewRuntimeOpt(nil, store.opt.Logs, valid.Definitions),
			wantErr: true,
		},
		{
			name:    "missing_logs",
			opt:     runtime.NewRuntimeOpt(store.opt.Records, nil, valid.Definitions),
			wantErr: true,
		},
		{
			name:    "missing_definitions",
			opt:     runtime.NewRuntimeOpt(store.opt.Records, store.opt.Logs, nil),
			wantErr: true,
		},
		{
			name:    "missing_agents",
			opt:     runtime.NewRuntimeOpt(store.opt.Records, store.opt.Logs, &runtime.Definitions{Models: valid.Definitions.Models}),
			wantErr: true,
		},
		{
			name: "no_models",
			opt:  runtime.NewRuntimeOpt(store.opt.Records, store.opt.Logs, &runtime.Definitions{Agents: valid.Definitions.Agents}),
		},
		{
			name:    "zero_unsaved_idle_timeout",
			opt:     newTestRuntimeOpt(store, newRuntimeModel(t)).WithUnsavedConversationIdleTimeout(0),
			wantErr: true,
		},
		{
			name: "valid",
			opt:  valid,
		},
	}
	// 2. 逐项创建 Runtime，检查缺失依赖、无效空闲时长报错及有效配置创建成功。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := runtime.NewRuntime(tt.opt); (err != nil) != tt.wantErr {
				t.Fatalf("NewRuntime() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestRuntime 检查多对话输入、中断、重启和并发打开。
func TestRuntime(t *testing.T) {
	// 1. 列出多对话输入、中断、重启和并发打开的测试场景。
	tests := []struct {
		name string
		run  func(t *testing.T, h *runtimeHarness)
	}{
		{
			name: "create_and_submit",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				itemID := h.submit(t, conv.ID, "你好", "")
				h.model.receive(t).reply <- runtimeModelResponse{chunks: []string{"你好！"}}
				logged := h.waitLog(t, conv.ID, conv.RootSessionID, 2)
				if logged[0].Item.ID != itemID || logged[1].Item.Kind != defs.ItemMessage {
					t.Fatalf("log = %+v", logged)
				}
			},
		},
		{
			name: "concurrent_conversations_do_not_block",
			run: func(t *testing.T, h *runtimeHarness) {
				held, quick := h.create(t), h.create(t)
				h.submit(t, held.ID, "慢任务", "")
				h.model.receive(t).reply <- runtimeModelResponse{
					chunks:  []string{"处理中"},
					release: make(chan struct{}),
				}
				h.submit(t, quick.ID, "快任务", "")
				h.model.receive(t).reply <- runtimeModelResponse{chunks: []string{"完成"}}
				h.waitLog(t, quick.ID, quick.RootSessionID, 2)
			},
		},
		{
			name: "duplicate_input_id_runs_once",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				first := h.submit(t, conv.ID, "只执行一次", "retry-1")
				second := h.submit(t, conv.ID, "只执行一次", "retry-1")
				if first != second {
					t.Fatalf("retry ItemID = %s, want %s", second, first)
				}
				h.model.receive(t).reply <- runtimeModelResponse{chunks: []string{"好"}}
				h.waitLog(t, conv.ID, conv.RootSessionID, 2)
				h.model.requireNoRequest(t)
			},
		},
		{
			name: "interrupt_active_turn",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				request := &runtime.InterruptTurnRequest{ConversationID: conv.ID}
				if turnID, err := h.rt.InterruptTurn(t.Context(), request); err != nil || turnID != "" {
					t.Fatalf("idle InterruptTurn() = %q, %v", turnID, err)
				}
				h.submit(t, conv.ID, "长任务", "")
				h.model.receive(t).reply <- runtimeModelResponse{
					chunks:  []string{"处理中"},
					release: make(chan struct{}),
				}
				turnID, err := h.rt.InterruptTurn(t.Context(), request)
				if err != nil || turnID == "" {
					t.Fatalf("InterruptTurn() = %q, %v", turnID, err)
				}
				h.waitTurnStatus(t, conv.RootSessionID, turnID, defs.TurnStatusInterrupted)
			},
		},
		{
			name: "missing_conversation_rejected",
			run: func(t *testing.T, h *runtimeHarness) {
				if _, err := h.rt.SubmitInput(t.Context(), newSubmitInputRequest(defs.NewConversationID(), "找不到", "")); err == nil {
					t.Fatal("SubmitInput() to a missing Conversation error = nil")
				}
				conv := h.create(t)
				h.submit(t, conv.ID, "仍可使用", "")
				h.model.receive(t).reply <- runtimeModelResponse{chunks: []string{"好"}}
				h.waitLog(t, conv.ID, conv.RootSessionID, 2)
			},
		},
		{
			name: "run_waits_conversations",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				h.submit(t, conv.ID, "长任务", "")
				h.model.receive(t).reply <- runtimeModelResponse{
					chunks:  []string{"处理中"},
					release: make(chan struct{}),
				}
				if err := h.stop(t); !errors.Is(err, context.Canceled) {
					t.Fatalf("Run() = %v, want context.Canceled", err)
				}
				page, err := h.store.query.ListTurns(t.Context(), dao.ListTurnsRequest{
					SessionID: conv.RootSessionID,
					Order:     storage.Order{Direction: storage.DirectionDesc},
					Limit:     1,
				})
				if err != nil || len(page.Items) != 1 || page.Items[0].Status != defs.TurnStatusInterrupted {
					t.Fatalf("latest Turn = %+v, %v; want interrupted before Run returned", page.Items, err)
				}
				if _, err := h.rt.SubmitInput(t.Context(), newSubmitInputRequest(conv.ID, "停止后", "")); !errors.Is(err, runtime.ErrRuntimeStopped) {
					t.Fatalf("SubmitInput() after stop = %v, want ErrRuntimeStopped", err)
				}
			},
		},
		{
			name: "load_existing_conversation_after_restart",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				// 第一条输入才把新对话写进共用库，重启后才能打开。
				h.completeRound(t, conv, "开始", 2)
				h.stop(t)
				restarted := startTestRuntime(t, h.store, h.model)
				if _, err := restarted.rt.OpenConversation(t.Context(), &runtime.OpenConversationRequest{ConversationID: conv.ID}); err != nil {
					t.Fatal(err)
				}
				restarted.submit(t, conv.ID, "继续", "")
				restarted.model.receive(t).reply <- runtimeModelResponse{chunks: []string{"继续好"}}
				restarted.waitLog(t, conv.ID, conv.RootSessionID, 4)
			},
		},
		{
			// 冷打开只读记录：打开前后记录的最后写入时间不变。
			name: "cold_open_does_not_write_record",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				h.completeRound(t, conv, "开始", 2)
				h.stop(t)
				restarted := startTestRuntime(t, h.store, h.model)
				before, err := restarted.rt.GetConversation(t.Context(), &runtime.GetConversationRequest{ConversationID: conv.ID})
				if err != nil {
					t.Fatal(err)
				}
				// 时间戳精确到毫秒：等到下一毫秒，打开时若写了记录，最后写入时间一定会变。
				for time.Now().UnixMilli() <= before.UpdatedAt {
					time.Sleep(time.Millisecond)
				}
				if _, err := restarted.rt.OpenConversation(t.Context(), &runtime.OpenConversationRequest{ConversationID: conv.ID}); err != nil {
					t.Fatal(err)
				}
				after, err := restarted.rt.GetConversation(t.Context(), &runtime.GetConversationRequest{ConversationID: conv.ID})
				if err != nil || after.UpdatedAt != before.UpdatedAt {
					t.Fatalf("UpdatedAt after open = %d, %v; want %d", after.UpdatedAt, err, before.UpdatedAt)
				}
			},
		},
		{
			// 对话记录的默认 Executor 是不回 SSH 握手的远端：打开它在调用方 goroutine 里卡到 ctx 截止后失败，期间别的对话照常收输入。
			name: "open_with_unresponsive_ssh_does_not_block_runtime",
			run: func(t *testing.T, h *runtimeHarness) {
				h.stop(t)
				// remote 先是本机目录：建好、保存一个默认 Executor 为 remote 的对话，再在 remote 换成不回握手的远端后重启。
				local := newLocalExecutor(t, defs.LocalExecutorID)
				first := startTestRuntimeWithOpt(t, h.store, h.model, newExecutorRuntimeOpt(h.store, h.model, local, newLocalExecutor(t, "remote")))
				stuck, err := first.rt.CreateConversation(t.Context(), &runtime.CreateConversationRequest{
					AgentName:        "assistant",
					ExecutorSettings: defs.ExecutorSettings{DefaultExecutorID: "remote"},
				})
				if err != nil {
					t.Fatal(err)
				}
				first.completeRound(t, stuck, "保存", 2)
				first.stop(t)
				listener := newUnresponsiveSSHExecutor(t, "remote")
				restarted := startTestRuntimeWithOpt(t, h.store, h.model, newExecutorRuntimeOpt(h.store, h.model, local, listener.executor))
				other := restarted.create(t)
				openCtx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
				defer cancel()
				opened := make(chan error, 1)
				go func() {
					_, err := restarted.rt.OpenConversation(openCtx, &runtime.OpenConversationRequest{ConversationID: stuck.ID})
					opened <- err
				}()
				listener.waitAccepted(t)
				submitted := make(chan error, 1)
				go func() {
					_, err := restarted.rt.SubmitInput(t.Context(), newSubmitInputRequest(other.ID, "别等它", ""))
					submitted <- err
				}()
				select {
				case err := <-submitted:
					if err != nil {
						t.Fatalf("SubmitInput() while another conversation opens = %v", err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("SubmitInput() was blocked by the conversation that is opening")
				}
				restarted.model.receive(t).reply <- runtimeModelResponse{chunks: []string{"好"}}
				select {
				case err := <-opened:
					if err == nil {
						t.Fatal("OpenConversation() with an unresponsive default executor succeeded")
					}
				case <-time.After(5 * time.Second):
					t.Fatal("OpenConversation() did not return after its ctx ended")
				}
				if isOpen, err := restarted.rt.IsConversationOpen(t.Context(), &runtime.GetConversationRequest{ConversationID: stuck.ID}); err != nil || isOpen {
					t.Fatalf("IsConversationOpen() = %v, %v; want false", isOpen, err)
				}
			},
		},
		{
			// 对话已在运行时再打开它：直接返回运行中的记录，不装配也不访问默认 Executor；本机目录被删掉后照样成功。
			name: "open_running_does_not_touch_default_executor",
			run: func(t *testing.T, h *runtimeHarness) {
				h.stop(t)
				running := startTestRuntimeWithOpt(t, h.store, h.model, newExecutorRuntimeOpt(h.store, h.model, newLocalExecutor(t, defs.LocalExecutorID)))
				directory := t.TempDir()
				conv, err := running.rt.CreateConversation(t.Context(), &runtime.CreateConversationRequest{
					AgentName:        "assistant",
					ExecutorSettings: defs.ExecutorSettings{WorkspaceDirectories: map[string]string{defs.LocalExecutorID: directory}},
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.RemoveAll(directory); err != nil {
					t.Fatal(err)
				}
				opened, err := running.rt.OpenConversation(t.Context(), &runtime.OpenConversationRequest{ConversationID: conv.ID})
				if err != nil || opened.ID != conv.ID || !reflect.DeepEqual(opened.ExecutorSettings, conv.ExecutorSettings) {
					t.Fatalf("OpenConversation() on the running conversation = %+v, %v; want its record %+v", opened, err, conv)
				}
			},
		},
		{
			// 两个调用方同时打开同一个对话：都成功，拿到同一个对话，运行表里只有一个实例。
			name: "concurrent_open_returns_same_conversation",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				h.completeRound(t, conv, "开始", 2)
				h.stop(t)
				restarted := startTestRuntime(t, h.store, h.model)
				type openResult struct {
					record defs.Conversation
					err    error
				}
				results := make(chan openResult, 2)
				var opens sync.WaitGroup
				for range 2 {
					opens.Go(func() {
						record, err := restarted.rt.OpenConversation(t.Context(), &runtime.OpenConversationRequest{ConversationID: conv.ID})
						results <- openResult{
							record: record,
							err:    err,
						}
					})
				}
				opens.Wait()
				close(results)
				for result := range results {
					if result.err != nil || result.record.ID != conv.ID {
						t.Fatalf("OpenConversation() = %+v, %v; want %s", result.record, result.err, conv.ID)
					}
				}
				if isOpen, err := restarted.rt.IsConversationOpen(t.Context(), &runtime.GetConversationRequest{ConversationID: conv.ID}); err != nil || !isOpen {
					t.Fatalf("IsConversationOpen() = %v, %v; want true", isOpen, err)
				}
				if statuses := restarted.subscribeConversation(t, conv.ID); len(statuses.Sessions) != 1 {
					t.Fatalf("subscribed Sessions = %+v, want only the root", statuses.Sessions)
				}
				restarted.submit(t, conv.ID, "继续", "")
				restarted.model.receive(t).reply <- runtimeModelResponse{chunks: []string{"好"}}
				restarted.waitLog(t, conv.ID, conv.RootSessionID, 4)
				restarted.model.requireNoRequest(t)
			},
		},
	}
	// 2. 启动独立 Runtime，提交和中断输入，检查多对话请求、去重、重启历史及并发打开结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, startTestRuntime(t, openRuntimeStore(t), newRuntimeModel(t)))
		})
	}
}

// TestRuntimeSetConversationTitle 检查手动标题按 Unicode 字符截取。
func TestRuntimeSetConversationTitle(t *testing.T) {
	// 1. 列出手动标题按 Unicode 字符截取的测试场景。
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
			title: strings.Repeat("a", 256),
			want:  strings.Repeat("a", 256),
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
	// 2. 逐项新建对话并说完一轮，保存标题后检查存储里的标题按 256 个 Unicode 字符截取。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := startTestRuntime(t, openRuntimeStore(t), newRuntimeModel(t))
			conv := h.create(t)
			h.submit(t, conv.ID, "你好", "")
			h.model.receive(t).reply <- runtimeModelResponse{chunks: []string{"你好"}}
			h.waitLog(t, conv.ID, conv.RootSessionID, 2)
			if err := h.rt.SetConversationTitle(t.Context(), &runtime.SetConversationTitleRequest{
				ConversationID: conv.ID,
				Title:          tt.title,
			}); err != nil {
				t.Fatal(err)
			}
			record, found, err := h.store.query.GetConversation(t.Context(), conv.ID)
			if err != nil || !found || record.Title != tt.want {
				t.Fatalf("saved title = %q, %v; want %q", record.Title, err, tt.want)
			}
		})
	}
}

// TestRuntimeAutomaticConversationTitle 检查用户正文自动命名和已有标题保留。
func TestRuntimeAutomaticConversationTitle(t *testing.T) {
	// 1. 列出用户正文自动命名和已有标题保留的测试场景。
	text := func(value string) *defs.Content {
		return &defs.Content{
			Kind: defs.ContentText,
			Text: &defs.TextContent{Text: value},
		}
	}
	contextText := text("ATTACHED_DOCUMENT")
	contextText.Text.IsContext = true
	tests := []struct {
		name    string
		content []*defs.Content
		from    defs.IdentityKind
		preset  string
		mode    session.SessionInputMode
		want    string
		wantErr bool
	}{
		{
			name:    "first_nonempty_line",
			content: []*defs.Content{text(" \n\t 修复\t 登录  \n详细描述")},
			want:    "修复 登录",
		},
		{
			name:    "unicode_limit",
			content: []*defs.Content{text(strings.Repeat("中😀", 25))},
			want:    strings.Repeat("中😀", 20),
		},
		{
			name:    "skip_attached_material",
			content: []*defs.Content{contextText, text("用户正文")},
			want:    "用户正文",
		},
		{
			name:    "context_only",
			content: []*defs.Content{contextText},
		},
		{
			name: "image_only",
			content: []*defs.Content{{
				Kind:  defs.ContentImage,
				Image: &defs.ImageContent{DataURL: "data:image/png;base64,aGVsbG8="},
			}},
		},
		{
			name:    "agent_message",
			content: []*defs.Content{text("内部通知")},
			from:    defs.IdentityAgent,
		},
		{
			name:    "existing_title",
			content: []*defs.Content{text("用户请求")},
			preset:  "手动标题",
			want:    "手动标题",
		},
		{
			name:    "rejected_input",
			content: []*defs.Content{text("不能命名")},
			mode:    "invalid",
			wantErr: true,
		},
	}
	// 2. 提交各类内容，检查只用正文首个非空行命名、不覆盖已有标题及失败输入不命名。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := startTestRuntime(t, openRuntimeStore(t), newRuntimeModel(t))
			conv := h.create(t)
			if tt.preset != "" {
				if err := h.rt.SetConversationTitle(t.Context(), &runtime.SetConversationTitleRequest{
					ConversationID: conv.ID,
					Title:          tt.preset,
				}); err != nil {
					t.Fatal(err)
				}
			}
			watcher := h.subscribeConversation(t, conv.ID)
			request := newSubmitInputRequest(conv.ID, "", "")
			request.Content = tt.content
			if tt.from != "" {
				request.From.Kind = tt.from
			}
			if tt.mode != "" {
				request.Mode = tt.mode
			}
			_, err := h.rt.SubmitInput(t.Context(), request)
			if (err != nil) != tt.wantErr {
				t.Fatalf("SubmitInput = %v, want error %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				h.model.receive(t).reply <- runtimeModelResponse{chunks: []string{"好"}}
				collectUntil(t, watcher.Events, isTurnCompletedInfo)
			}
			record, err := h.rt.GetConversation(t.Context(), &runtime.GetConversationRequest{ConversationID: conv.ID})
			if err != nil || record.Title != tt.want {
				t.Fatalf("title = %q, %v; want %q", record.Title, err, tt.want)
			}
			// 没有原始文本时，下一条有效用户文本仍可命名；已有标题则始终保留。
			h.submit(t, conv.ID, "后续用户正文", "")
			h.model.receive(t).reply <- runtimeModelResponse{chunks: []string{"好"}}
			want := tt.want
			if want == "" {
				want = "后续用户正文"
			}
			record, err = h.rt.GetConversation(t.Context(), &runtime.GetConversationRequest{ConversationID: conv.ID})
			if err != nil || record.Title != want {
				t.Fatalf("later title = %q, %v; want %q", record.Title, err, want)
			}
		})
	}
}

// TestRuntimeConversationTitleOutput 检查各查询入口的标题截取且不改原存储。
func TestRuntimeConversationTitleOutput(t *testing.T) {
	// 1. 列出标题长短不同的测试场景。
	tests := []struct {
		name  string
		title string
		want  string
	}{
		{
			name:  "short",
			title: "周报",
			want:  "周报",
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
	// 2. 写入超长标题并重启，检查各查询入口截取标题且原存储不变。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, model := openRuntimeStore(t), newRuntimeModel(t)
			first := startTestRuntime(t, store, model)
			conv := first.create(t)
			first.completeRound(t, conv, "你好", 2)
			first.stop(t)
			// 停掉 Runtime 后写入超长标题，再重启从各入口读取。
			conv.Title = tt.title
			if err := store.mutation.SetConversation(t.Context(), conv); err != nil {
				t.Fatal(err)
			}
			h := startTestRuntime(t, store, model)
			read, err := h.rt.GetConversation(t.Context(), &runtime.GetConversationRequest{ConversationID: conv.ID})
			if err != nil || read.Title != tt.want {
				t.Fatalf("read title = %q, %v; want %q", read.Title, err, tt.want)
			}
			page, err := h.rt.ListConversations(t.Context(), &dao.ListConversationsRequest{
				Order: storage.Order{
					By:        storage.OrderByUpdatedAt,
					Direction: storage.DirectionDesc,
				},
				Limit: 10,
			})
			if err != nil || len(page.Items) != 1 || page.Items[0].Conversation.Title != tt.want {
				t.Fatalf("listed conversations = %+v, %v; want title %q", page.Items, err, tt.want)
			}
			opened, err := h.rt.OpenConversation(t.Context(), &runtime.OpenConversationRequest{ConversationID: conv.ID})
			if err != nil || opened.Title != tt.want {
				t.Fatalf("opened title = %q, %v; want %q", opened.Title, err, tt.want)
			}
			details, err := h.rt.GetSessionDetails(t.Context(), &runtime.GetSessionDetailsRequest{
				ConversationID: conv.ID,
				SessionID:      conv.RootSessionID,
			})
			if err != nil || details.Title != tt.want {
				t.Fatalf("session details title = %q, %v; want %q", details.Title, err, tt.want)
			}
			stored, found, err := h.store.query.GetConversation(t.Context(), conv.ID)
			if err != nil || !found || stored.Title != tt.title {
				t.Fatalf("stored title changed after queries: %q, %v; want %q", stored.Title, err, tt.title)
			}
		})
	}
}

// TestRuntimeQueries 检查对话、Session 和日志的公开查询。
func TestRuntimeQueries(t *testing.T) {
	// 1. 列出对话、Session 和日志的公开查询的测试场景。
	tests := []struct {
		name string
		run  func(t *testing.T, h *runtimeHarness)
	}{
		{
			name: "title_saved",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				if err := h.rt.SetConversationTitle(t.Context(), &runtime.SetConversationTitleRequest{
					ConversationID: conv.ID,
					Title:          "周报",
				}); err != nil {
					t.Fatal(err)
				}
				record, err := h.rt.GetConversation(t.Context(), &runtime.GetConversationRequest{ConversationID: conv.ID})
				if err != nil || record.Title != "周报" || record.RootSessionID != conv.RootSessionID {
					t.Fatalf("GetConversation() = %+v, %v", record, err)
				}
			},
		},
		{
			name: "missing_conversation",
			run: func(t *testing.T, h *runtimeHarness) {
				if _, err := h.rt.GetConversation(t.Context(), &runtime.GetConversationRequest{ConversationID: defs.NewConversationID()}); err == nil {
					t.Fatal("GetConversation() of a missing Conversation error = nil")
				}
			},
		},
		{
			name: "list_conversations_and_sessions",
			run: func(t *testing.T, h *runtimeHarness) {
				first, second := h.create(t), h.create(t)
				// 第一条输入之后才进对话列表。
				h.completeRound(t, first, "一", 2)
				h.completeRound(t, second, "二", 2)
				conversations, err := h.rt.ListConversations(t.Context(), &dao.ListConversationsRequest{
					Order: storage.Order{Direction: storage.DirectionAsc},
					Limit: 10,
				})
				if err != nil || len(conversations.Items) != 2 {
					t.Fatalf("ListConversations() = %+v, %v", conversations, err)
				}
				sessions, err := h.rt.ListSessions(t.Context(), &dao.ListSessionsRequest{
					ConversationID: first.ID,
					Order:          storage.Order{Direction: storage.DirectionAsc},
					Limit:          10,
				})
				if err != nil || len(sessions.Items) != 1 || sessions.Items[0].ID != first.RootSessionID {
					t.Fatalf("ListSessions() = %+v, %v", sessions, err)
				}
			},
		},
		{
			name: "get_session_by_id",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				h.submit(t, conv.ID, "委派", "")
				h.model.receive(t).reply <- runtimeModelResponse{calls: []llmkit.ToolCall{{
					ID:        "spawn",
					Name:      "spawn_agent",
					Arguments: `{"task_name":"review","message":"check"}`,
				}}}
				h.model.receiveRootAndTask(t, "review")
				childID := h.getChildSessionID(t, conv)
				// 等子 Session 的第一条日志（分叉截取点）写下，再只凭 SessionID 查。
				h.waitLog(t, conv.ID, childID, 1)
				record, err := h.rt.GetSessionByID(t.Context(), &runtime.GetSessionByIDRequest{SessionID: childID})
				if err != nil || record.ID != childID || record.ConversationID != conv.ID || record.ParentSessionID != conv.RootSessionID || record.TaskName != "review" {
					t.Fatalf("GetSessionByID(child) = %+v, %v, want the review Session of %s", record, err, conv.ID)
				}
			},
		},
		{
			name: "get_session_by_id_missing",
			run: func(t *testing.T, h *runtimeHarness) {
				if _, err := h.rt.GetSessionByID(t.Context(), &runtime.GetSessionByIDRequest{SessionID: defs.NewSessionID()}); !errors.Is(err, runtime.ErrSessionNotFound) {
					t.Fatalf("GetSessionByID(missing) error = %v, want %v", err, runtime.ErrSessionNotFound)
				}
			},
		},
		{
			name: "list_items_rejects_foreign_session",
			run: func(t *testing.T, h *runtimeHarness) {
				a, b := h.create(t), h.create(t)
				request := &runtime.ListItemsRequest{
					ConversationID: a.ID,
					SessionID:      b.RootSessionID,
					Order:          storage.DirectionAsc,
					Limit:          10,
				}
				if _, err := h.rt.ListItems(t.Context(), request); err == nil {
					t.Fatal("ListItems() of a foreign Session error = nil")
				}
			},
		},
	}
	// 2. 每个场景用独立 Runtime 运行，检查对话、Session 和日志的查询结果与未知对象错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, startTestRuntime(t, openRuntimeStore(t), newRuntimeModel(t)))
		})
	}
}

// TestRuntimeSessionDetails 检查 Session 已加载配置和模型选择。
func TestRuntimeSessionDetails(t *testing.T) {
	// 1. 列出 Session 已加载配置和模型选择的测试场景。
	tests := []struct {
		name string
		run  func(t *testing.T, h *runtimeHarness)
	}{
		{
			// 标题取自对话事件循环里的最新记录，不取 Conversation 构造时的那份。
			name: "title_from_saved_record",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				if err := h.rt.SetConversationTitle(t.Context(), &runtime.SetConversationTitleRequest{
					ConversationID: conv.ID,
					Title:          "周报",
				}); err != nil {
					t.Fatal(err)
				}
				details, err := h.rt.GetSessionDetails(t.Context(), &runtime.GetSessionDetailsRequest{
					ConversationID: conv.ID,
					SessionID:      conv.RootSessionID,
				})
				if err != nil {
					t.Fatal(err)
				}
				if details.Title != "周报" || details.ConversationID != conv.ID || details.Session.ID != conv.RootSessionID ||
					details.Model.Ref != testModelRef || details.CompactModel.Ref != testModelRef {
					t.Fatalf("details = %+v", details)
				}
				if !slices.Contains(details.Tools, "spawn_agent") {
					t.Fatalf("Tools = %v, want the collaboration tools", details.Tools)
				}
			},
		},
		{
			// 建对话请求里的模型选择交给 Conversation 校验并保存到根 Session。
			name: "create_with_model_selection",
			run: func(t *testing.T, h *runtimeHarness) {
				if _, err := h.rt.CreateConversation(t.Context(), &runtime.CreateConversationRequest{
					AgentName: "assistant",
					Model: defs.ModelSelection{Ref: defs.ModelRef{
						ProviderID: "test",
						ModelID:    "gone",
					}},
				}); err == nil || !strings.Contains(err.Error(), "test/gone") {
					t.Fatalf("CreateConversation(unknown model) error = %v, want one naming test/gone", err)
				}
				conv, err := h.rt.CreateConversation(t.Context(), &runtime.CreateConversationRequest{
					AgentName: "assistant",
					Model:     defs.ModelSelection{Ref: testModelRef},
				})
				if err != nil {
					t.Fatal(err)
				}
				details, err := h.rt.GetSessionDetails(t.Context(), &runtime.GetSessionDetailsRequest{
					ConversationID: conv.ID,
					SessionID:      conv.RootSessionID,
				})
				if err != nil || details.Session.Model != (defs.ModelSelection{Ref: testModelRef}) {
					t.Fatalf("details model = %+v, %v; want %+v", details.Session.Model, err, defs.ModelSelection{Ref: testModelRef})
				}
			},
		},
		{
			name: "unknown_session",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				if _, err := h.rt.GetSessionDetails(t.Context(), &runtime.GetSessionDetailsRequest{
					ConversationID: conv.ID,
					SessionID:      defs.NewSessionID(),
				}); err == nil {
					t.Fatal("GetSessionDetails() of an unknown Session error = nil")
				}
			},
		},
		{
			name: "unknown_conversation",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				if _, err := h.rt.GetSessionDetails(t.Context(), &runtime.GetSessionDetailsRequest{
					ConversationID: defs.NewConversationID(),
					SessionID:      conv.RootSessionID,
				}); err == nil {
					t.Fatal("GetSessionDetails() of an unknown Conversation error = nil")
				}
			},
		},
	}
	// 2. 每个场景用独立 Runtime 运行，检查标题、模型、工具列表和未知对象错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := startTestRuntime(t, openRuntimeStore(t), newRuntimeModel(t))
			tt.run(t, h)
			// 检查配置查询没有引发模型请求。
			h.model.requireNoRequest(t)
		})
	}
}

// TestRuntimeListModels 检查模型公开字段及 Agent 默认模型。
func TestRuntimeListModels(t *testing.T) {
	// 1. 列出模型公开字段及 Agent 默认模型的测试场景。
	h := startTestRuntime(t, openRuntimeStore(t), newRuntimeModel(t))
	tests := []struct {
		name        string
		agentName   string
		wantDefault defs.ModelRef
		wantErr     string
	}{
		{
			name:      "with_agent_default",
			agentName: "assistant",
			wantDefault: defs.ModelRef{
				ProviderID: "test",
				ModelID:    "test",
			},
		},
		{name: "without_agent"},
		{
			name:      "unknown_agent",
			agentName: "missing",
			wantErr:   `agent "missing" is not configured`,
		},
	}
	// 2. 查询模型目录，检查公开字段、Agent 默认模型和未知 Agent 错误，不返回连接凭证。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			list, err := h.rt.ListModels(t.Context(), &runtime.ListModelsRequest{AgentName: tt.agentName})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ListModels() = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || list.AgentDefaultModel != tt.wantDefault || len(list.Models) != 2 {
				t.Fatalf("ListModels() = %+v, %v", list, err)
			}
			for _, model := range list.Models {
				if model.Target != nil {
					t.Fatalf("model %s exposes its target", model.Ref)
				}
			}
		})
	}
}

// TestRuntimeSetDefinitions 检查定义替换、空闲对话更新和忙碌对话保留。
func TestRuntimeSetDefinitions(t *testing.T) {
	// 1. 准备待新增的模型和不同运行状态下的定义替换场景。
	addedModelRef := defs.ModelRef{
		ProviderID: "added",
		ModelID:    "c",
	}
	bModelRef := defs.ModelRef{
		ProviderID: "test",
		ModelID:    "b",
	}
	// newModel 返回指向受控模型的模型定义，upstream 是请求体里的上游模型名。
	newModel := func(h *runtimeHarness, ref defs.ModelRef, upstream string) *defs.ModelDefinition {
		return &defs.ModelDefinition{
			Ref: ref,
			Target: &llmkit.Target{
				Protocol: llmkit.ProtocolOpenAIChat,
				BaseURL:  h.model.server.URL,
				Model:    upstream,
			},
		}
	}
	// requireModelRequest 要求下一次模型请求用上游模型 upstream，回复后等到根 Session 日志至少有 wantLogCount 条。
	requireModelRequest := func(t *testing.T, h *runtimeHarness, conv defs.Conversation, upstream string, wantLogCount int) {
		t.Helper()
		request := h.model.receive(t)
		if request.body["model"] != upstream {
			t.Fatalf("model request uses %v, want %q", request.body["model"], upstream)
		}
		request.reply <- runtimeModelResponse{chunks: []string{"好"}}
		h.waitLog(t, conv.ID, conv.RootSessionID, wantLogCount)
	}
	// newReloadedDefinitions 返回测试 Agent 和模型目录，加上 executors 和共用的 skills，和 reload 重新读出的整份定义一样。
	newReloadedDefinitions := func(h *runtimeHarness, executors []*defs.ExecutorDefinition, skills ...*skill.Skill) *runtime.Definitions {
		definitions := newTestRuntimeOpt(h.store, h.model).Definitions
		definitions.Executors, definitions.Skills = executors, skills
		return definitions
	}
	tests := []struct {
		name string
		// withoutModels Runtime 启动时是否没有模型？没有时 assistant 的默认模型是零值，和没配模型时的内置 default Agent 一样。
		withoutModels bool
		// executors 不为 nil 时返回 Runtime 启动时的 Executor 定义。
		executors func(t *testing.T) []*defs.ExecutorDefinition
		run       func(t *testing.T, h *runtimeHarness)
	}{
		{
			name:          "no_models_until_one_is_added",
			withoutModels: true,
			run: func(t *testing.T, h *runtimeHarness) {
				if _, err := h.rt.CreateConversation(t.Context(), &runtime.CreateConversationRequest{AgentName: "assistant"}); !errors.Is(err, runtime.ErrNoModelConfigured) {
					t.Fatalf("CreateConversation() error = %v, want %v", err, runtime.ErrNoModelConfigured)
				}
				h.setDefinitions(t, testModelRef, newModel(h, testModelRef, "test"))
				conv := h.create(t)
				h.submit(t, conv.ID, "你好", "")
				requireModelRequest(t, h, conv, "test", 2)
			},
		},
		{
			name: "added_model_reaches_idle_conversations",
			run: func(t *testing.T, h *runtimeHarness) {
				saved := h.create(t)
				statuses := h.subscribeConversation(t, saved.ID)
				h.completeRound(t, saved, "保存", 2)
				collectUntil(t, statuses.Events, isTurnCompletedInfo)
				unsaved := h.create(t)
				result := h.setDefinitions(t, testModelRef, newModel(h, testModelRef, "test"), newModel(h, addedModelRef, "model-c"))
				if len(result.BusyConversationIDs) != 0 {
					t.Fatalf("BusyConversationIDs = %v, want none", result.BusyConversationIDs)
				}
				// 对话原地换上新定义，reload 之前的订阅随之关闭。
				waitEventsClosed(t, statuses.Events)
				list, err := h.rt.ListModels(t.Context(), &runtime.ListModelsRequest{})
				if err != nil || !slices.ContainsFunc(list.Models, func(model *defs.ModelDefinition) bool { return model.Ref == addedModelRef }) {
					t.Fatalf("ListModels() = %+v, %v; want %s listed", list.Models, err, addedModelRef)
				}
				for _, conv := range []defs.Conversation{saved, unsaved} {
					request := &runtime.SetSessionModelRequest{
						ConversationID: conv.ID,
						SessionID:      conv.RootSessionID,
						Model:          defs.ModelSelection{Ref: addedModelRef},
					}
					if _, err := h.rt.SetSessionModel(t.Context(), request); err != nil {
						t.Fatalf("SetSessionModel(%s) of Conversation %s = %v", addedModelRef, conv.ID, err)
					}
				}
				// 没保存的对话换定义后仍用它的内存库：第一条输入照常保存并发给新模型。
				h.submit(t, unsaved.ID, "用新模型", "")
				requireModelRequest(t, h, unsaved, "model-c", 2)
			},
		},
		{
			name: "running_conversation_is_busy",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				statuses := h.subscribeConversation(t, conv.ID)
				h.submit(t, conv.ID, "长任务", "")
				request := h.model.receive(t)
				collectUntil(t, statuses.Events, func(info runtime.SessionRuntimeInfo) bool { return info.State == session.SessionStateRunning })
				result := h.setDefinitions(t, testModelRef, newModel(h, testModelRef, "test"), newModel(h, addedModelRef, "model-c"))
				if !reflect.DeepEqual(result.BusyConversationIDs, []defs.ConversationID{conv.ID}) {
					t.Fatalf("BusyConversationIDs = %v, want [%s]", result.BusyConversationIDs, conv.ID)
				}
				// 没有换定义：旧订阅仍在，这一轮照常结束。
				request.reply <- runtimeModelResponse{chunks: []string{"完成"}}
				collectUntil(t, statuses.Events, isTurnCompletedInfo)
			},
		},
		{
			// 只有后台命令在运行时，reload 同样跳过该对话，保留命令。
			name:      "reload_skips_conversation_with_command",
			executors: newLocalExecutors,
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				watcher := h.subscribeSession(t, conv.ID, conv.RootSessionID)
				started := h.runCommandToolRound(t, conv, watcher, "call-bash", "bash", `{"command":"sleep 30","wait_seconds":0}`)
				if started.ToolError != nil || started.Status != "running" {
					t.Fatalf("bash result = %+v, want a running command", started)
				}
				result := h.setDefinitions(t, testModelRef, newModel(h, testModelRef, "test"), newModel(h, addedModelRef, "model-c"))
				if !slices.Equal(result.BusyConversationIDs, []defs.ConversationID{conv.ID}) {
					t.Fatalf("BusyConversationIDs = %v, want [%s]", result.BusyConversationIDs, conv.ID)
				}
				// 没有换定义：旧订阅仍在，命令还在跑。
				read := h.runCommandToolRound(t, conv, watcher, "call-read", "read_command_output", `{"command_id":"`+started.CommandID+`","wait_seconds":0}`)
				if read.ToolError != nil || read.Status != "running" {
					t.Fatalf("read_command_output result = %+v, want the command still running", read)
				}
			},
		},
		{
			name: "removed_model_rejects_input",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				h.setDefinitions(t, bModelRef, newModel(h, bModelRef, "model-b"))
				_, err := h.rt.SubmitInput(t.Context(), newSubmitInputRequest(conv.ID, "还用旧模型", ""))
				if want := "model test/test is not configured; choose another model"; err == nil || err.Error() != want {
					t.Fatalf("SubmitInput() error = %v, want %q", err, want)
				}
			},
		},
		{
			name: "no_models_rejects_opening_a_stopped_conversation",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				// 第一条输入才把新对话写进共用库，重启后才能打开。
				h.completeRound(t, conv, "保存", 2)
				h.stop(t)
				restarted := startTestRuntime(t, h.store, h.model)
				restarted.setDefinitions(t, defs.ModelRef{})
				_, err := restarted.rt.OpenConversation(t.Context(), &runtime.OpenConversationRequest{ConversationID: conv.ID})
				if !errors.Is(err, runtime.ErrNoModelConfigured) {
					t.Fatalf("OpenConversation() error = %v, want %v", err, runtime.ErrNoModelConfigured)
				}
			},
		},
		{
			// 新目录里没有 Agent 的默认模型，对话换定义失败：按旧目录继续运行，订阅不关。
			name: "reload_failure_keeps_previous",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				statuses := h.subscribeConversation(t, conv.ID)
				result := h.setDefinitions(t, testModelRef, newModel(h, addedModelRef, "model-c"))
				if len(result.BusyConversationIDs) != 0 {
					t.Fatalf("BusyConversationIDs = %v, want none", result.BusyConversationIDs)
				}
				h.submit(t, conv.ID, "旧目录", "")
				requireModelRequest(t, h, conv, "test", 2)
				collectUntil(t, statuses.Events, isTurnCompletedInfo)
			},
		},
		{
			// 空闲对话原地换上新定义：同一个实例一直登记着，订阅以 ErrConversationReconfigured 关闭，下一轮用新的系统指令。
			name: "reconfigures_idle_conversation_in_place",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				statuses := h.subscribeConversation(t, conv.ID)
				// 换定义期间一直查对话是否打开，记下有没有查到没打开的时候。
				stopWatching := make(chan struct{})
				watched := make(chan bool, 1)
				go func() {
					alwaysOpen := true
					for {
						select {
						case <-stopWatching:
							watched <- alwaysOpen
							return
						case <-t.Context().Done():
							return
						default:
						}
						isOpen, err := h.rt.IsConversationOpen(t.Context(), &runtime.GetConversationRequest{ConversationID: conv.ID})
						alwaysOpen = alwaysOpen && err == nil && isOpen
					}
				}()
				result := h.setDefinitionsRequest(t, &runtime.Definitions{
					Agents: []*agent.AgentDefinition{{
						Name:         "assistant",
						Description:  "主 Agent",
						Instructions: "NEW_INSTRUCTIONS",
						Model:        testModelRef,
					}},
					Models: []*defs.ModelDefinition{newModel(h, testModelRef, "test")},
				})
				waitEventsClosed(t, statuses.Events)
				close(stopWatching)
				if !<-watched {
					t.Fatal("IsConversationOpen() reported the conversation closed while it was reconfigured")
				}
				if len(result.BusyConversationIDs) != 0 {
					t.Fatalf("BusyConversationIDs = %v, want none", result.BusyConversationIDs)
				}
				if reason := statuses.GetCloseReason(); !errors.Is(reason, runtime.ErrConversationReconfigured) {
					t.Fatalf("subscription close reason = %v, want %v", reason, runtime.ErrConversationReconfigured)
				}
				h.submit(t, conv.ID, "新指令", "")
				request := h.model.receive(t)
				if !strings.Contains(fmt.Sprint(request.body["messages"]), "NEW_INSTRUCTIONS") {
					t.Fatalf("system instructions do not carry the new definition: %v", request.body["messages"])
				}
				request.reply <- runtimeModelResponse{chunks: []string{"好"}}
				h.waitLog(t, conv.ID, conv.RootSessionID, 2)
			},
		},
		{
			// reload 加一个 virtual Executor 和一个共用 skill：已打开的空闲对话不用重新打开就有它们。
			name:      "reload_applies_new_executor_and_skill",
			executors: newLocalExecutors,
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				statuses := h.subscribeConversation(t, conv.ID)
				extra := &defs.ExecutorDefinition{
					ID:                 "extra",
					Kind:               defs.ExecutorKindVirtual,
					Description:        "新加的",
					WorkspaceDirectory: "/workspace",
				}
				added := &skill.Skill{
					Name:        "added",
					Description: "新加的技能",
					Path:        "/skills/added/SKILL.md",
				}
				h.setDefinitionsRequest(t, newReloadedDefinitions(h, append(newLocalExecutors(t), extra), added))
				waitEventsClosed(t, statuses.Events)
				if reason := statuses.GetCloseReason(); !errors.Is(reason, runtime.ErrConversationReconfigured) {
					t.Fatalf("subscription close reason = %v, want %v", reason, runtime.ErrConversationReconfigured)
				}
				details, err := h.rt.GetSessionDetails(t.Context(), &runtime.GetSessionDetailsRequest{
					ConversationID: conv.ID,
					SessionID:      conv.RootSessionID,
				})
				if err != nil || !slices.ContainsFunc(details.Executors, func(executor conversation.ExecutorDetails) bool { return executor.ID == "extra" }) {
					t.Fatalf("session executors = %+v, %v; want extra", details.Executors, err)
				}
				skills, err := h.rt.GetSkills(t.Context(), &runtime.GetSkillsRequest{ConversationID: conv.ID})
				if err != nil || !slices.ContainsFunc(skills, func(info skill.SkillInfo) bool { return info.Name == "added" }) {
					t.Fatalf("GetSkills() = %+v, %v; want added", skills, err)
				}
			},
		},
		{
			// 对话 A 的默认 Executor 在 reload 后变成不回 SSH 握手的远端：A 在自己的 goroutine 里卡到 ctx 截止，换定义失败、保留原配置；
			// 卡住期间对话 B 换完定义、照常收输入，新建对话也不受影响。
			name: "reload_does_not_block_other_conversations",
			executors: func(t *testing.T) []*defs.ExecutorDefinition {
				return append(newLocalExecutors(t), newLocalExecutor(t, "remote"))
			},
			run: func(t *testing.T, h *runtimeHarness) {
				stuck, err := h.rt.CreateConversation(t.Context(), &runtime.CreateConversationRequest{
					AgentName:        "assistant",
					ExecutorSettings: defs.ExecutorSettings{DefaultExecutorID: "remote"},
				})
				if err != nil {
					t.Fatal(err)
				}
				other := h.create(t)
				otherStatuses := h.subscribeConversation(t, other.ID)
				listener := newUnresponsiveSSHExecutor(t, "remote")
				reloadCtx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
				defer cancel()
				reloaded := make(chan error, 1)
				go func() {
					_, err := h.rt.SetDefinitions(reloadCtx, newReloadedDefinitions(h, append(newLocalExecutors(t), listener.executor)))
					reloaded <- err
				}()
				listener.waitAccepted(t)
				start := time.Now()
				waitEventsClosed(t, otherStatuses.Events)
				h.submit(t, other.ID, "别等它", "")
				if _, err := h.rt.CreateConversation(t.Context(), &runtime.CreateConversationRequest{AgentName: "assistant"}); err != nil {
					t.Fatalf("CreateConversation() during the reload = %v", err)
				}
				if elapsed := time.Since(start); elapsed > 2*time.Second {
					t.Fatalf("other conversation and CreateConversation took %v during the reload, want within 2s", elapsed)
				}
				h.model.receive(t).reply <- runtimeModelResponse{chunks: []string{"好"}}
				select {
				case err := <-reloaded:
					if err != nil {
						t.Fatalf("SetDefinitions() = %v", err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("SetDefinitions did not return after the stuck conversation's ctx ended")
				}
				details, err := h.rt.GetSessionDetails(t.Context(), &runtime.GetSessionDetailsRequest{
					ConversationID: stuck.ID,
					SessionID:      stuck.RootSessionID,
				})
				if err != nil || len(details.Executors) == 0 || details.Executors[0].ID != "remote" || details.Executors[0].WorkspaceDirectory == "/project" {
					t.Fatalf("stuck conversation executors = %+v, %v; want the previous local remote kept", details.Executors, err)
				}
			},
		},
		{
			name:      "ephemeral_stays_without_executors_after_reload",
			executors: newLocalExecutors,
			run: func(t *testing.T, h *runtimeHarness) {
				conv, err := h.rt.CreateConversation(t.Context(), &runtime.CreateConversationRequest{
					AgentName: "assistant",
					Ephemeral: true,
				})
				if err != nil {
					t.Fatal(err)
				}
				statuses := h.subscribeConversation(t, conv.ID)
				extra := &defs.ExecutorDefinition{
					ID:                 "extra",
					Kind:               defs.ExecutorKindVirtual,
					Description:        "新加的",
					WorkspaceDirectory: "/workspace",
				}
				h.setDefinitionsRequest(t, newReloadedDefinitions(h, append(newLocalExecutors(t), extra)))
				waitEventsClosed(t, statuses.Events)
				details, err := h.rt.GetSessionDetails(t.Context(), &runtime.GetSessionDetailsRequest{
					ConversationID: conv.ID,
					SessionID:      conv.RootSessionID,
				})
				if err != nil || len(details.Executors) != 0 {
					t.Fatalf("ephemeral executors = %+v, %v; want none", details.Executors, err)
				}
			},
		},
	}
	// 2. 在各运行状态替换定义，检查空闲对话新配置、忙碌对话列表、失败保留及实例继续运行。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, model := openRuntimeStore(t), newRuntimeModel(t)
			opt := newTestRuntimeOpt(store, model)
			if tt.withoutModels {
				opt.Definitions.Agents[0].Model, opt.Definitions.Models = defs.ModelRef{}, nil
			}
			if tt.executors != nil {
				opt.Definitions.Executors = tt.executors(t)
			}
			tt.run(t, startTestRuntimeWithOpt(t, store, model, opt))
		})
	}
}

// setDefinitions 更新测试 Agent 的默认模型和模型目录。
func (h *runtimeHarness) setDefinitions(t *testing.T, agentModel defs.ModelRef, models ...*defs.ModelDefinition) runtime.SetDefinitionsResult {
	t.Helper()
	// 1. 创建只有 assistant 和指定模型目录的定义并提交。
	return h.setDefinitionsRequest(t, &runtime.Definitions{
		Agents: []*agent.AgentDefinition{{
			Name:         "assistant",
			Description:  "主 Agent",
			Instructions: "Test assistant",
			Model:        agentModel,
		}},
		Models: models,
	})
}

// setDefinitionsRequest 提交定义替换并检查结果。
func (h *runtimeHarness) setDefinitionsRequest(t *testing.T, request *runtime.Definitions) runtime.SetDefinitionsResult {
	t.Helper()
	// 1. 启动定义替换任务并收集结果。
	type setResult struct {
		result runtime.SetDefinitionsResult
		err    error
	}
	done := make(chan setResult, 1)
	go func() {
		result, err := h.rt.SetDefinitions(t.Context(), request)
		done <- setResult{
			result: result,
			err:    err,
		}
	}()
	// 2. 等待最多 5 秒，返回结果，超时或业务失败时结束测试。
	select {
	case set := <-done:
		if set.err != nil {
			t.Fatalf("SetDefinitions() error = %v", set.err)
		}
		return set.result
	case <-time.After(5 * time.Second):
		t.Fatal("SetDefinitions did not return within 5 seconds")
		return runtime.SetDefinitionsResult{}
	}
}

// TestRuntimeMCPServers 检查执行环境 MCP 的启动、换配置和退出。
func TestRuntimeMCPServers(t *testing.T) {
	// 1. 编译假 MCP server，列出连接启动、工具调用、换配置和进程退出场景。
	binary := buildFakeMCPServer(t)
	connectedFake := []mcpServerState{{
		executorID: defs.LocalExecutorID,
		serverID:   "fake",
		state:      mcpclient.ConnectionStateConnected,
	}}
	tests := []struct {
		name string
		// unsavedIdleTimeout 非零时替换没保存的对话没人用后的丢弃等待时长。
		unsavedIdleTimeout time.Duration
		run                func(t *testing.T, f *mcpRuntimeFixture)
	}{
		{
			// remote 绑定的 ssh-dev 不在对话中，unused 未被 Agent 使用；两者均不建立连接或启动进程。
			name: "used_servers_connect_without_input",
			run: func(t *testing.T, f *mcpRuntimeFixture) {
				f.waitMCPServers(t, connectedFake)
				if pid := f.waitPID(t, "fake", 0); !isProcessAlive(pid) {
					t.Fatalf("fake server process %d is not running", pid)
				}
				for _, name := range []string{"unused", "remote"} {
					if _, err := os.Stat(f.getPIDPath(name)); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("%s server was started: %v", name, err)
					}
				}
			},
		},
		{
			// 没有 ssh-dev 的 remote 不影响这一轮：模型看得到 fake 的工具、看不到 remote 的，调用 fake 的 echo 照常拿到结果。
			name: "turn_uses_fake_without_remote",
			run: func(t *testing.T, f *mcpRuntimeFixture) {
				conv := f.conversation
				watcher := f.h.subscribeSession(t, conv.ID, conv.RootSessionID)
				f.h.submit(t, conv.ID, "调用 echo", "")
				request := f.model.receive(t)
				toolNames := getRequestToolNames(request.body)
				if !slices.Contains(toolNames, "mcp__fake__echo") || slices.ContainsFunc(toolNames, func(name string) bool {
					return strings.HasPrefix(name, "mcp__remote__") || strings.HasPrefix(name, "mcp__unused__")
				}) {
					t.Fatalf("model tools = %v, want fake tools only", toolNames)
				}
				request.reply <- runtimeModelResponse{calls: []llmkit.ToolCall{{
					ID:        "echo",
					Name:      "mcp__fake__echo",
					Arguments: `{"text":"MCP_ECHO_OK"}`,
				}}}
				next := f.model.receive(t)
				if !strings.Contains(fmt.Sprint(next.body), "MCP_ECHO_OK") {
					t.Fatalf("echo result missing from the next model request: %v", next.body)
				}
				next.reply <- runtimeModelResponse{chunks: []string{"好"}}
				events := collectUntil(t, watcher.Events, isTurnFinished)
				if finished := getSessionEvent(events[len(events)-1]).(*session.SessionTurnFinished); finished.Status != defs.TurnStatusCompleted {
					t.Fatalf("Turn = %+v, want completed", finished)
				}
			},
		},
		{
			// 只换模型目录时 fake 的定义不变，对话照样换配置：旧执行环境停止，旧进程结束；新执行环境起新进程，重新连上。
			name: "model_change_restarts_process",
			run: func(t *testing.T, f *mcpRuntimeFixture) {
				f.waitMCPServers(t, connectedFake)
				pid := f.waitPID(t, "fake", 0)
				statuses := f.h.subscribeConversation(t, f.conversation.ID)
				added := &defs.ModelDefinition{
					Ref: defs.ModelRef{
						ProviderID: "added",
						ModelID:    "c",
					},
					Target: &llmkit.Target{
						Protocol: llmkit.ProtocolOpenAIChat,
						BaseURL:  f.model.server.URL,
						Model:    "model-c",
					},
				}
				if result := f.h.setDefinitionsRequest(t, f.newDefinitions("fake", added)); len(result.BusyConversationIDs) != 0 {
					t.Fatalf("BusyConversationIDs = %v, want none", result.BusyConversationIDs)
				}
				// 订阅关闭，说明对话已经换上新配置。
				waitEventsClosed(t, statuses.Events)
				waitProcessExited(t, pid)
				newPID := f.waitPID(t, "fake", pid)
				f.waitMCPServers(t, connectedFake)
				if !isProcessAlive(newPID) {
					t.Fatalf("fake server process %d of the reconfigured conversation is not running", newPID)
				}
			},
		},
		{
			// fake 的 Args 变了：对话换配置，旧进程结束、按新定义起新进程；extra 没分到 MCP server，仍然没有。
			name: "changed_args_replace_process",
			run: func(t *testing.T, f *mcpRuntimeFixture) {
				f.waitMCPServers(t, connectedFake)
				oldPID := f.waitPID(t, "fake", 0)
				if result := f.h.setDefinitionsRequest(t, f.newDefinitions("fake-2")); len(result.BusyConversationIDs) != 0 {
					t.Fatalf("BusyConversationIDs = %v, want none", result.BusyConversationIDs)
				}
				newPID := f.waitPID(t, "fake-2", 0)
				waitProcessExited(t, oldPID)
				f.waitMCPServers(t, connectedFake)
				if newPID == oldPID || !isProcessAlive(newPID) {
					t.Fatalf("fake server process after reload = %d (alive %v), want a new running process", newPID, isProcessAlive(newPID))
				}
			},
		},
		{
			// 正在跑的对话不换定义：列进 BusyConversationIDs，执行环境和进程都不动，新定义的进程不启动。
			name: "busy_conversation_keeps_process",
			run: func(t *testing.T, f *mcpRuntimeFixture) {
				conv := f.conversation
				f.waitMCPServers(t, connectedFake)
				pid := f.waitPID(t, "fake", 0)
				statuses := f.h.subscribeConversation(t, conv.ID)
				f.h.submit(t, conv.ID, "长任务", "")
				request := f.model.receive(t)
				collectUntil(t, statuses.Events, func(info runtime.SessionRuntimeInfo) bool { return info.State == session.SessionStateRunning })
				result := f.h.setDefinitionsRequest(t, f.newDefinitions("fake-2"))
				if !slices.Equal(result.BusyConversationIDs, []defs.ConversationID{conv.ID}) {
					t.Fatalf("BusyConversationIDs = %v, want [%s]", result.BusyConversationIDs, conv.ID)
				}
				request.reply <- runtimeModelResponse{chunks: []string{"完成"}}
				collectUntil(t, statuses.Events, isTurnCompletedInfo)
				if got := f.waitPID(t, "fake", 0); got != pid || !isProcessAlive(pid) {
					t.Fatalf("fake server process = %d (alive %v), want %d kept", got, isProcessAlive(pid), pid)
				}
				if _, err := os.Stat(f.getPIDPath("fake-2")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("server with the new definition was started for a busy conversation: %v", err)
				}
				f.waitMCPServers(t, connectedFake)
			},
		},
		{
			// 换 Executor 设置：新执行环境起新进程，旧执行环境停掉、旧进程结束。
			name: "executor_settings_change_stops_old_process",
			run: func(t *testing.T, f *mcpRuntimeFixture) {
				f.waitMCPServers(t, connectedFake)
				oldPID := f.waitPID(t, "fake", 0)
				request := &runtime.SetConversationExecutorSettingsRequest{ConversationID: f.conversation.ID}
				if _, err := f.h.setExecutorSettings(t, request); err != nil {
					t.Fatalf("SetConversationExecutorSettings() error = %v", err)
				}
				newPID := f.waitPID(t, "fake", oldPID)
				waitProcessExited(t, oldPID)
				f.waitMCPServers(t, connectedFake)
				if !isProcessAlive(newPID) {
					t.Fatalf("fake server process %d of the new environments is not running", newPID)
				}
			},
		},
		{
			// 没保存的对话没人用后到时丢弃，对话最终结束：它的执行环境停掉，进程结束。
			name:               "discarded_conversation_stops_process",
			unsavedIdleTimeout: 100 * time.Millisecond,
			run: func(t *testing.T, f *mcpRuntimeFixture) {
				subscriptionCtx, unsubscribe := context.WithCancel(t.Context())
				if _, err := f.h.rt.SubscribeConversation(subscriptionCtx, &runtime.SubscribeConversationRequest{ConversationID: f.conversation.ID}); err != nil {
					t.Fatal(err)
				}
				f.waitMCPServers(t, connectedFake)
				pid := f.waitPID(t, "fake", 0)
				unsubscribe()
				waitConversationDiscarded(t, f.h, f.conversation.ID)
				waitProcessExited(t, pid)
			},
		},
		{
			// Runtime 退出时等对话和它的执行环境停完，进程随之结束。
			name: "runtime_exit_stops_process",
			run: func(t *testing.T, f *mcpRuntimeFixture) {
				f.waitMCPServers(t, connectedFake)
				pid := f.waitPID(t, "fake", 0)
				if err := f.h.stop(t); !errors.Is(err, context.Canceled) {
					t.Fatalf("Runtime.Run() = %v, want context.Canceled", err)
				}
				waitProcessExited(t, pid)
			},
		},
		{
			// 冷打开：重启后按记录里的设置打开已保存的对话，同样不等输入就连上。
			name: "cold_open_starts_process",
			run: func(t *testing.T, f *mcpRuntimeFixture) {
				f.h.completeRound(t, f.conversation, "保存", 2)
				oldPID := f.waitPID(t, "fake", 0)
				f.h.stop(t)
				waitProcessExited(t, oldPID)
				f.h = startTestRuntimeWithOpt(t, f.store, f.model, runtime.NewRuntimeOpt(f.store.opt.Records, f.store.opt.Logs, f.newDefinitions("fake")))
				request := &runtime.OpenConversationRequest{ConversationID: f.conversation.ID}
				if _, err := f.h.rt.OpenConversation(t.Context(), request); err != nil {
					t.Fatal(err)
				}
				f.waitMCPServers(t, connectedFake)
				if pid := f.waitPID(t, "fake", oldPID); !isProcessAlive(pid) {
					t.Fatalf("fake server process %d is not running after the cold open", pid)
				}
			},
		},
		{
			// 一次 GetMCPServers 同时返回执行环境的 MCP server（ExecutorID 是环境 ID）和客户端登记的 server（ExecutorID 是 client），环境在前。
			name: "environment_and_client_servers_listed_together",
			run: func(t *testing.T, f *mcpRuntimeFixture) {
				f.waitMCPServers(t, connectedFake)
				f.setClientMCPServers(t, "c1", f.newClientServer(clientFakeServerID, "client"))
				f.waitMCPServers(t, []mcpServerState{connectedFake[0], {
					executorID: defs.ClientExecutorID,
					serverID:   clientFakeServerID,
					state:      mcpclient.ConnectionStateConnected,
				}})
			},
		},
		{
			name: "conversation_not_open",
			run: func(t *testing.T, f *mcpRuntimeFixture) {
				_, err := f.h.rt.GetMCPServers(t.Context(), &runtime.GetConversationRequest{ConversationID: defs.NewConversationID()})
				if !errors.Is(err, runtime.ErrConversationNotOpen) {
					t.Fatalf("GetMCPServers() error = %v, want %v", err, runtime.ErrConversationNotOpen)
				}
			},
		},
	}
	// 2. 运行执行环境 MCP 场景，检查连接状态、工具名、换配置前后的进程及对话退出清理。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, model := openRuntimeStore(t), newRuntimeModel(t)
			f := &mcpRuntimeFixture{
				store:        store,
				model:        model,
				binary:       binary,
				pidDirectory: t.TempDir(),
				executors:    newMCPTestExecutors(t),
			}
			opt := runtime.NewRuntimeOpt(store.opt.Records, store.opt.Logs, f.newDefinitions("fake"))
			if tt.unsavedIdleTimeout != 0 {
				opt.WithUnsavedConversationIdleTimeout(tt.unsavedIdleTimeout)
			}
			f.h = startTestRuntimeWithOpt(t, store, model, opt)
			conv, err := f.h.rt.CreateConversation(t.Context(), &runtime.CreateConversationRequest{AgentName: "assistant"})
			if err != nil {
				t.Fatal(err)
			}
			f.conversation = conv
			tt.run(t, f)
		})
	}
}

// mcpRuntimeFixture 是 MCP 测试一个用例的 Runtime 和对话。
type mcpRuntimeFixture struct {
	store        *runtimeStore
	model        *runtimeModel
	h            *runtimeHarness
	conversation defs.Conversation
	// binary 是编译好的假 MCP server。
	binary string
	// pidDirectory 是各 MCP server 启动时写进程号文件的目录。
	pidDirectory string
	// executors 是定义里的本机 Executor local 和 extra，各用自己的临时目录。
	executors []*defs.ExecutorDefinition
}

// mcpServerState 是 GetMCPServers 一项里测试关心的部分。
type mcpServerState struct {
	executorID string
	serverID   string
	state      mcpclient.ConnectionState
}

// newDefinitions 创建测试 Agent、模型和执行环境 MCP 定义。
func (f *mcpRuntimeFixture) newDefinitions(fakePIDName string, extraModels ...*defs.ModelDefinition) *runtime.Definitions {
	// 1. 使用 local 和 extra，追加模型，Agent 只用 fake 和 remote；fake 绑定 local，unused 绑定两环境，remote 绑定 ssh-dev。
	definitions := newTestRuntimeOpt(f.store, f.model).Definitions
	definitions.Executors = f.executors
	definitions.Agents[0].MCPServerIDs = defs.OptionalStringList{"fake", "remote"}
	definitions.Models = append(definitions.Models, extraModels...)
	definitions.MCPServers = []*mcpclient.ServerDefinition{
		f.newServerDefinition("fake", []string{defs.LocalExecutorID}, fakePIDName),
		f.newServerDefinition("unused", []string{defs.LocalExecutorID, "extra"}, "unused"),
		f.newServerDefinition("remote", []string{"ssh-dev"}, "remote"),
	}
	return definitions
}

// newServerDefinition 创建写出进程号的 stdio 假 MCP server 定义。
func (f *mcpRuntimeFixture) newServerDefinition(id string, executorIDs []string, pidName string) *mcpclient.ServerDefinition {
	// 1. 填入 Executor、启动命令、进程号文件和超时。
	return &mcpclient.ServerDefinition{
		ID:             id,
		Type:           mcpclient.ServerTransportStdio,
		Command:        f.binary,
		Args:           []string{"-pidfile", f.getPIDPath(pidName)},
		ExecutorIDs:    executorIDs,
		StartupTimeout: 5 * time.Second,
		ToolTimeout:    30 * time.Second,
	}
}

// newMCPTestExecutors 创建各有独立目录的 local 和 extra Executor。
func newMCPTestExecutors(t *testing.T) []*defs.ExecutorDefinition {
	t.Helper()
	// 1. 返回两个本机 Executor 定义。
	return []*defs.ExecutorDefinition{newLocalExecutor(t, defs.LocalExecutorID), newLocalExecutor(t, "extra")}
}

// getPIDPath 返回假 MCP server 的进程号文件路径。
func (f *mcpRuntimeFixture) getPIDPath(name string) string {
	// 1. 在夹具目录中拼接名称和 .pid 后缀。
	return filepath.Join(f.pidDirectory, name+".pid")
}

// waitPID 等待进程号文件出现且不等于前一次进程号。
func (f *mcpRuntimeFixture) waitPID(t *testing.T, name string, previous int) int {
	t.Helper()
	// 1. 在 5 秒内轮询文件并解析新进程号，超时则结束测试。
	deadline := time.Now().Add(5 * time.Second)
	for {
		content, err := os.ReadFile(f.getPIDPath(name))
		if err == nil {
			if pid, err := strconv.Atoi(string(content)); err == nil && pid != previous {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s server did not write a new process ID (previous %d): %q, %v", name, previous, content, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// getMCPServerStates 读取测试关心的 MCP 连接状态。
func (f *mcpRuntimeFixture) getMCPServerStates(t *testing.T) []mcpServerState {
	t.Helper()
	// 1. 查询对话的 MCP 连接。
	servers, err := f.h.rt.GetMCPServers(t.Context(), &runtime.GetConversationRequest{ConversationID: f.conversation.ID})
	if err != nil {
		t.Fatalf("GetMCPServers() error = %v", err)
	}
	// 2. 提取 Executor、server ID 和状态后返回。
	states := make([]mcpServerState, 0, len(servers))
	for _, server := range servers {
		states = append(states, mcpServerState{
			executorID: server.ExecutorID,
			serverID:   server.ServerID,
			state:      server.State,
		})
	}
	return states
}

// waitMCPServers 等待对话 MCP 连接状态与预期相同。
func (f *mcpRuntimeFixture) waitMCPServers(t *testing.T, want []mcpServerState) {
	t.Helper()
	// 1. 在 5 秒内轮询连接状态，不匹配则继续等待。
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := f.getMCPServerStates(t)
		if slices.Equal(got, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("MCP servers = %+v, want %+v", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// isProcessAlive 指定进程是否仍存在？
func isProcessAlive(pid int) bool {
	// 1. 向进程发送信号 0，成功时返回 true。
	return syscall.Kill(pid, 0) == nil
}

// waitProcessExited 等待指定进程结束。
func waitProcessExited(t *testing.T, pid int) {
	t.Helper()
	// 1. 在 5 秒内轮询进程，不再存在时返回，超时则结束测试。
	deadline := time.Now().Add(5 * time.Second)
	for isProcessAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("server process %d is still running", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// commandTransport 保存假 MCP server 命令，每次 Connect 使用新的 exec.Cmd 和命令传输。
type commandTransport struct {
	command   string
	arguments []string
}

// Connect 启动新的假 MCP server 命令并建立连接。
func (t *commandTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	// 1. 本次创建独立 exec.Cmd，交给 MCP 命令传输建立连接。
	return (&mcp.CommandTransport{Command: exec.Command(t.command, t.arguments...)}).Connect(ctx)
}

// clientFakeServerID 是客户端登记的假 MCP server 的 ID，模型看到的工具名是 mcp__clientfake__<原名>。
const clientFakeServerID = "clientfake"

// newClientServer 创建客户端登记的假 MCP server。
func (f *mcpRuntimeFixture) newClientServer(id, pidName string) conversation.ClientMCPServer {
	// 1. 填入 ID 和命令传输，启动时写出指定进程号文件。
	return conversation.ClientMCPServer{
		ID: id,
		Transport: &commandTransport{
			command:   f.binary,
			arguments: []string{"-pidfile", f.getPIDPath(pidName)},
		},
	}
}

// setClientMCPServers 以指定客户端 ID 替换对话的 MCP 登记。
func (f *mcpRuntimeFixture) setClientMCPServers(t *testing.T, clientID string, servers ...conversation.ClientMCPServer) {
	t.Helper()
	// 1. 提交全部 server，失败则结束测试。
	request := &runtime.SetClientMCPServersRequest{
		ConversationID: f.conversation.ID,
		ClientID:       clientID,
		Servers:        servers,
	}
	if err := f.h.rt.SetClientMCPServers(t.Context(), request); err != nil {
		t.Fatalf("SetClientMCPServers() error = %v", err)
	}
}

// getClientStates 返回 MCP 列表中属于客户端的条目。
func (f *mcpRuntimeFixture) getClientStates(t *testing.T) []mcpServerState {
	t.Helper()
	// 1. 移除 ExecutorID 不是 client 的连接状态。
	return slices.DeleteFunc(f.getMCPServerStates(t), func(state mcpServerState) bool { return state.executorID != defs.ClientExecutorID })
}

// waitClientServers 等待客户端 MCP 连接状态匹配预期。
func (f *mcpRuntimeFixture) waitClientServers(t *testing.T, want ...mcpServerState) {
	t.Helper()
	// 1. 在 5 秒内轮询客户端条目，超时则结束测试。
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := f.getClientStates(t)
		if slices.Equal(got, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("client MCP servers = %+v, want %+v", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// runClientTurn 运行一轮可选 MCP 工具调用，返回工具名和后续请求。
func (f *mcpRuntimeFixture) runClientTurn(t *testing.T, watcher *runtime.SessionSubscription, calls ...llmkit.ToolCall) (toolNames []string, nextRequestBody string) {
	t.Helper()
	// 1. 提交输入，取出首次模型请求中的工具名。
	conv := f.conversation
	f.h.submit(t, conv.ID, "这一轮", "")
	request := f.model.receive(t)
	toolNames = getRequestToolNames(request.body)
	// 2. 指定工具调用时先回复调用，再取出带工具结果的后续请求。
	if len(calls) > 0 {
		request.reply <- runtimeModelResponse{calls: calls}
		request = f.model.receive(t)
		nextRequestBody = fmt.Sprint(request.body)
	}
	// 3. 发送最终回复，等待本轮成功结束后返回请求信息。
	request.reply <- runtimeModelResponse{chunks: []string{"好"}}
	events := collectUntil(t, watcher.Events, isTurnFinished)
	if finished := getSessionEvent(events[len(events)-1]).(*session.SessionTurnFinished); finished.Status != defs.TurnStatusCompleted {
		t.Fatalf("Turn = %+v, want completed", finished)
	}
	return toolNames, nextRequestBody
}

// hasClientFakeTools 工具列表中是否包含客户端假 server 的 echo？
func hasClientFakeTools(toolNames []string) bool {
	// 1. 查找带客户端 server 前缀的 echo 工具名。
	return slices.Contains(toolNames, "mcp__"+clientFakeServerID+"__echo")
}

// TestRuntimeClientMCPServers 检查客户端 MCP 登记、工具使用和进程收尾。
func TestRuntimeClientMCPServers(t *testing.T) {
	// 1. 编译假 MCP server，列出客户端登记替换、工具调用和进程退出场景。
	binary := buildFakeMCPServer(t)
	connectedClient := mcpServerState{
		executorID: defs.ClientExecutorID,
		serverID:   clientFakeServerID,
		state:      mcpclient.ConnectionStateConnected,
	}
	tests := []struct {
		name string
		// agentMCPServerIDs 是 Agent 的 mcp_servers，nil 表示省略。
		agentMCPServerIDs defs.OptionalStringList
		// unsavedIdleTimeout 非零时替换没保存的对话没人用后的丢弃等待时长。
		unsavedIdleTimeout time.Duration
		run                func(t *testing.T, f *mcpRuntimeFixture)
	}{
		{
			// 登记不换对话配置：登记前建的订阅一直开着，登记前后的 Turn 都由它读到；下一轮有客户端的工具并能调用。
			name: "registration_reaches_next_turn_without_rebuild",
			run: func(t *testing.T, f *mcpRuntimeFixture) {
				watcher := f.h.subscribeSession(t, f.conversation.ID, f.conversation.RootSessionID)
				if toolNames, _ := f.runClientTurn(t, watcher); hasClientFakeTools(toolNames) {
					t.Fatalf("model tools before registration = %v, want no client tools", toolNames)
				}
				f.setClientMCPServers(t, "c1", f.newClientServer(clientFakeServerID, "client"))
				f.waitClientServers(t, connectedClient)
				call := llmkit.ToolCall{
					ID:        "echo",
					Name:      "mcp__" + clientFakeServerID + "__echo",
					Arguments: `{"text":"CLIENT_ECHO_OK"}`,
				}
				toolNames, next := f.runClientTurn(t, watcher, call)
				if !hasClientFakeTools(toolNames) {
					t.Fatalf("model tools after registration = %v, want the client server's tools", toolNames)
				}
				if !strings.Contains(next, "CLIENT_ECHO_OK") {
					t.Fatalf("echo result missing from the next model request: %s", next)
				}
			},
		},
		{
			name: "empty_registration_removes_tools_and_stops_process",
			run: func(t *testing.T, f *mcpRuntimeFixture) {
				watcher := f.h.subscribeSession(t, f.conversation.ID, f.conversation.RootSessionID)
				f.setClientMCPServers(t, "c1", f.newClientServer(clientFakeServerID, "client"))
				f.waitClientServers(t, connectedClient)
				pid := f.waitPID(t, "client", 0)
				if toolNames, _ := f.runClientTurn(t, watcher); !hasClientFakeTools(toolNames) {
					t.Fatalf("model tools = %v, want the client server's tools", toolNames)
				}
				f.setClientMCPServers(t, "c1")
				waitProcessExited(t, pid)
				f.waitClientServers(t)
				if toolNames, _ := f.runClientTurn(t, watcher); hasClientFakeTools(toolNames) {
					t.Fatalf("model tools after removal = %v, want no client tools", toolNames)
				}
			},
		},
		{
			// 同一条客户端连接再登记一次：旧连接的进程结束，新进程起来，工具仍在。
			name: "replacing_registration_swaps_process",
			run: func(t *testing.T, f *mcpRuntimeFixture) {
				watcher := f.h.subscribeSession(t, f.conversation.ID, f.conversation.RootSessionID)
				f.setClientMCPServers(t, "c1", f.newClientServer(clientFakeServerID, "client-1"))
				f.waitClientServers(t, connectedClient)
				oldPID := f.waitPID(t, "client-1", 0)
				f.setClientMCPServers(t, "c1", f.newClientServer(clientFakeServerID, "client-2"))
				newPID := f.waitPID(t, "client-2", 0)
				waitProcessExited(t, oldPID)
				f.waitClientServers(t, connectedClient)
				if !isProcessAlive(newPID) {
					t.Fatalf("new client server process %d is not running", newPID)
				}
				if toolNames, _ := f.runClientTurn(t, watcher); !hasClientFakeTools(toolNames) {
					t.Fatalf("model tools = %v, want the client server's tools", toolNames)
				}
			},
		},
		{
			// 两条客户端连接各自登记，互不影响：移除一条只停它的进程。
			name: "clients_register_independently",
			run: func(t *testing.T, f *mcpRuntimeFixture) {
				f.setClientMCPServers(t, "c1", f.newClientServer("first", "first"))
				f.setClientMCPServers(t, "c2", f.newClientServer("second", "second"))
				first, second := f.waitPID(t, "first", 0), f.waitPID(t, "second", 0)
				f.waitClientServers(t,
					mcpServerState{
						executorID: defs.ClientExecutorID,
						serverID:   "first",
						state:      mcpclient.ConnectionStateConnected,
					},
					mcpServerState{
						executorID: defs.ClientExecutorID,
						serverID:   "second",
						state:      mcpclient.ConnectionStateConnected,
					})
				f.setClientMCPServers(t, "c1")
				waitProcessExited(t, first)
				f.waitClientServers(t, mcpServerState{
					executorID: defs.ClientExecutorID,
					serverID:   "second",
					state:      mcpclient.ConnectionStateConnected,
				})
				if !isProcessAlive(second) {
					t.Fatalf("process %d of the other client was stopped", second)
				}
			},
		},
		{
			// Agent 写了 mcp_servers 时只认配置里的 id：客户端的连接仍然登记并可见，但模型没有它的工具。
			name:              "agent_mcp_servers_excludes_client_tools",
			agentMCPServerIDs: defs.OptionalStringList{"configured"},
			run: func(t *testing.T, f *mcpRuntimeFixture) {
				watcher := f.h.subscribeSession(t, f.conversation.ID, f.conversation.RootSessionID)
				f.setClientMCPServers(t, "c1", f.newClientServer(clientFakeServerID, "client"))
				f.waitClientServers(t, connectedClient)
				if toolNames, _ := f.runClientTurn(t, watcher); hasClientFakeTools(toolNames) {
					t.Fatalf("model tools = %v, want no client tools when the agent lists mcp_servers", toolNames)
				}
			},
		},
		{
			// reload 原地换配置：旧订阅关闭，客户端登记和进程不动，下一轮仍有客户端的工具。
			name: "reload_keeps_registration_and_process",
			run: func(t *testing.T, f *mcpRuntimeFixture) {
				f.setClientMCPServers(t, "c1", f.newClientServer(clientFakeServerID, "client"))
				f.waitClientServers(t, connectedClient)
				pid := f.waitPID(t, "client", 0)
				statuses := f.h.subscribeConversation(t, f.conversation.ID)
				added := &defs.ModelDefinition{
					Ref: defs.ModelRef{
						ProviderID: "added",
						ModelID:    "c",
					},
					Target: &llmkit.Target{
						Protocol: llmkit.ProtocolOpenAIChat,
						BaseURL:  f.model.server.URL,
						Model:    "model-c",
					},
				}
				definitions := newTestRuntimeOpt(f.store, f.model).Definitions
				definitions.Models = append(definitions.Models, added)
				f.h.setDefinitionsRequest(t, definitions)
				waitEventsClosed(t, statuses.Events)
				if got := f.getClientStates(t); !slices.Equal(got, []mcpServerState{connectedClient}) {
					t.Fatalf("client MCP servers right after reload = %+v, want %+v without reconnecting", got, connectedClient)
				}
				if !isProcessAlive(pid) {
					t.Fatalf("client server process %d was stopped by the reload", pid)
				}
				watcher := f.h.subscribeSession(t, f.conversation.ID, f.conversation.RootSessionID)
				if toolNames, _ := f.runClientTurn(t, watcher); !hasClientFakeTools(toolNames) {
					t.Fatalf("model tools after reload = %v, want the client server's tools", toolNames)
				}
			},
		},
		{
			name: "executor_settings_change_keeps_registration_and_process",
			run: func(t *testing.T, f *mcpRuntimeFixture) {
				f.setClientMCPServers(t, "c1", f.newClientServer(clientFakeServerID, "client"))
				f.waitClientServers(t, connectedClient)
				pid := f.waitPID(t, "client", 0)
				statuses := f.h.subscribeConversation(t, f.conversation.ID)
				request := &runtime.SetConversationExecutorSettingsRequest{ConversationID: f.conversation.ID}
				if _, err := f.h.setExecutorSettings(t, request); err != nil {
					t.Fatalf("SetConversationExecutorSettings() error = %v", err)
				}
				waitEventsClosed(t, statuses.Events)
				if got := f.getClientStates(t); !slices.Equal(got, []mcpServerState{connectedClient}) {
					t.Fatalf("client MCP servers after the settings change = %+v, want %+v", got, connectedClient)
				}
				if !isProcessAlive(pid) {
					t.Fatalf("client server process %d was stopped by the settings change", pid)
				}
				watcher := f.h.subscribeSession(t, f.conversation.ID, f.conversation.RootSessionID)
				if toolNames, _ := f.runClientTurn(t, watcher); !hasClientFakeTools(toolNames) {
					t.Fatalf("model tools after the settings change = %v, want the client server's tools", toolNames)
				}
			},
		},
		{
			// 没保存的对话没人用后到时丢弃，对话最终结束：客户端登记的进程结束。
			name:               "discarded_conversation_stops_client_process",
			unsavedIdleTimeout: 100 * time.Millisecond,
			run: func(t *testing.T, f *mcpRuntimeFixture) {
				subscriptionCtx, unsubscribe := context.WithCancel(t.Context())
				if _, err := f.h.rt.SubscribeConversation(subscriptionCtx, &runtime.SubscribeConversationRequest{ConversationID: f.conversation.ID}); err != nil {
					t.Fatal(err)
				}
				f.setClientMCPServers(t, "c1", f.newClientServer(clientFakeServerID, "client"))
				f.waitClientServers(t, connectedClient)
				pid := f.waitPID(t, "client", 0)
				unsubscribe()
				waitConversationDiscarded(t, f.h, f.conversation.ID)
				waitProcessExited(t, pid)
			},
		},
		{
			name: "runtime_exit_stops_client_process",
			run: func(t *testing.T, f *mcpRuntimeFixture) {
				f.setClientMCPServers(t, "c1", f.newClientServer(clientFakeServerID, "client"))
				f.waitClientServers(t, connectedClient)
				pid := f.waitPID(t, "client", 0)
				if err := f.h.stop(t); !errors.Is(err, context.Canceled) {
					t.Fatalf("Runtime.Run() = %v, want context.Canceled", err)
				}
				waitProcessExited(t, pid)
			},
		},
		{
			name: "conversation_not_open",
			run: func(t *testing.T, f *mcpRuntimeFixture) {
				request := &runtime.SetClientMCPServersRequest{
					ConversationID: defs.NewConversationID(),
					ClientID:       "c1",
					Servers:        []conversation.ClientMCPServer{f.newClientServer(clientFakeServerID, "client")},
				}
				if err := f.h.rt.SetClientMCPServers(t.Context(), request); !errors.Is(err, runtime.ErrConversationNotOpen) {
					t.Fatalf("SetClientMCPServers() error = %v, want %v", err, runtime.ErrConversationNotOpen)
				}
				if _, err := os.Stat(f.getPIDPath("client")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("client server was started for a conversation that is not open: %v", err)
				}
			},
		},
		{
			// 登记里有建不出连接的 server（没有 ID）：报错，这条客户端连接之前的登记原样保留，不启动新进程。
			name: "invalid_server_changes_nothing",
			run: func(t *testing.T, f *mcpRuntimeFixture) {
				f.setClientMCPServers(t, "c1", f.newClientServer(clientFakeServerID, "client-1"))
				f.waitClientServers(t, connectedClient)
				pid := f.waitPID(t, "client-1", 0)
				request := &runtime.SetClientMCPServersRequest{
					ConversationID: f.conversation.ID,
					ClientID:       "c1",
					Servers:        []conversation.ClientMCPServer{f.newClientServer("other", "client-2"), f.newClientServer("", "client-3")},
				}
				if err := f.h.rt.SetClientMCPServers(t.Context(), request); err == nil {
					t.Fatal("SetClientMCPServers() error = nil, want an error for the server without an ID")
				}
				f.waitClientServers(t, connectedClient)
				if !isProcessAlive(pid) {
					t.Fatalf("previous client server process %d was stopped by a rejected registration", pid)
				}
				for _, name := range []string{"client-2", "client-3"} {
					if _, err := os.Stat(f.getPIDPath(name)); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("server %s was started by a rejected registration: %v", name, err)
					}
				}
			},
		},
	}
	// 2. 登记、替换和移除客户端 server，检查下一轮工具、客户端独立性、配置更新保留及进程退出。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, model := openRuntimeStore(t), newRuntimeModel(t)
			f := &mcpRuntimeFixture{
				store:        store,
				model:        model,
				binary:       binary,
				pidDirectory: t.TempDir(),
				executors:    newMCPTestExecutors(t),
			}
			opt := newTestRuntimeOpt(store, model)
			opt.Definitions.Agents[0].MCPServerIDs = tt.agentMCPServerIDs
			opt.Definitions.Executors = f.executors
			if tt.unsavedIdleTimeout != 0 {
				opt.WithUnsavedConversationIdleTimeout(tt.unsavedIdleTimeout)
			}
			f.h = startTestRuntimeWithOpt(t, store, model, opt)
			conv, err := f.h.rt.CreateConversation(t.Context(), &runtime.CreateConversationRequest{AgentName: "assistant"})
			if err != nil {
				t.Fatal(err)
			}
			f.conversation = conv
			tt.run(t, f)
		})
	}
}

// getRequestToolNames 从模型请求提取全部工具名。
func getRequestToolNames(body map[string]any) []string {
	// 1. 按请求顺序读取 tools 中的 function.name。
	var names []string
	tools, _ := body["tools"].([]any)
	for _, value := range tools {
		names = append(names, value.(map[string]any)["function"].(map[string]any)["name"].(string))
	}
	return names
}

// TestRuntimeTelemetry 检查主任务委派与结果汇总的遥测。
func TestRuntimeTelemetry(t *testing.T) {
	// 1. 创建内存遥测和 Runtime。
	p := telemetrytest.NewPipeline(t)
	store, model := openRuntimeStore(t), newRuntimeModel(t)
	opt := newTestRuntimeOpt(store, model)
	opt.Telemetry = p.Telemetry
	h := startTestRuntimeWithOpt(t, store, model, opt)

	// 2. 启动根任务并让它委派子任务。
	conv := h.create(t)
	h.submit(t, conv.ID, "请委派一个审查任务", "")
	h.model.receive(t).reply <- runtimeModelResponse{
		calls: []llmkit.ToolCall{{
			ID:        "spawn",
			Name:      "spawn_agent",
			Arguments: `{"task_name":"review","message":"检查登录模块"}`,
		}},
		usage: &runtimeModelUsage{
			input:  100,
			output: 10,
		},
	}
	root, child := h.model.receiveRootAndTask(t, "review")
	root.reply <- runtimeModelResponse{
		chunks: []string{"已委派"},
		usage: &runtimeModelUsage{
			input:  150,
			output: 20,
		},
	}
	// 3. 等待根 Session 第一轮结束，再让子任务完成；子任务结果作为新输入开启根 Session 的第二轮，回复汇总。
	p.WaitEndedSpans(t, "invoke_agent assistant", 1)
	child.reply <- runtimeModelResponse{
		chunks: []string{"子任务完成"},
		usage: &runtimeModelUsage{
			input:  80,
			output: 5,
		},
	}
	h.model.receive(t).reply <- runtimeModelResponse{
		chunks: []string{"已汇总"},
		usage: &runtimeModelUsage{
			input:  200,
			output: 30,
		},
	}

	// 4. 等待三个 Turn 和对应工具、模型 span 结束，读取最终指标与日志；chat 和 tool 的 span、指标与日志先于所属 Turn span 写完。
	turnSpans := p.WaitEndedSpans(t, "invoke_agent assistant", 3)
	spawnSpan := p.WaitEndedSpans(t, "execute_tool spawn_agent", 1)[0]
	chatSpans := p.WaitEndedSpans(t, "chat test", 4)
	childID := h.getChildSessionID(t, conv)
	rootTurns := filterSpans(turnSpans, "openvaala.session.id", string(conv.RootSessionID))
	childTurns := filterSpans(turnSpans, "openvaala.session.id", string(childID))
	if len(rootTurns) != 2 || len(childTurns) != 1 {
		t.Fatalf("root turns = %d, child turns = %d; want 2 and 1", len(rootTurns), len(childTurns))
	}

	// 5. 准备父子 span、用量、日志内容和活跃对话数断言。
	tests := []struct {
		name  string
		check func(t *testing.T)
	}{
		{
			name: "child_turn_under_spawn_tool",
			check: func(t *testing.T) {
				if spawnSpan.Parent().SpanID() != rootTurns[0].SpanContext().SpanID() {
					t.Fatalf("spawn_agent parent = %s, want first root turn", spawnSpan.Parent().SpanID())
				}
				if childTurns[0].Parent().SpanID() != spawnSpan.SpanContext().SpanID() ||
					childTurns[0].SpanContext().TraceID() != rootTurns[0].SpanContext().TraceID() {
					t.Fatal("child turn is not in the root trace under spawn_agent")
				}
			},
		},
		{
			name: "chat_spans_under_turns",
			check: func(t *testing.T) {
				turnSpanIDs := make(map[trace.SpanID]bool)
				for _, span := range turnSpans {
					turnSpanIDs[span.SpanContext().SpanID()] = true
				}
				for _, span := range chatSpans {
					if !turnSpanIDs[span.Parent().SpanID()] || telemetrytest.GetSpanAttribute(span, "openvaala.model_call.id") == "" {
						t.Fatalf("chat span %v is not under a turn or lacks a model call ID", span.Attributes())
					}
				}
			},
		},
		{
			name: "token_usage_sums",
			check: func(t *testing.T) {
				input := p.GetMetricSum(t, "gen_ai.client.inference.usage.input_tokens")
				output := p.GetMetricSum(t, "gen_ai.client.inference.usage.output_tokens")
				if input != 530 || output != 65 {
					t.Fatalf("tokens = %v input, %v output; want 530 and 65", input, output)
				}
			},
		},
		{
			name: "log_identity",
			check: func(t *testing.T) {
				for _, record := range p.WaitLogRecords(t, "turn finished", 3) {
					if record["gen_ai.conversation.id"] != string(conv.ID) || record["openvaala.session.id"] == nil || record["openvaala.turn.id"] == nil {
						t.Fatalf("turn finished log lacks identity: %v", record)
					}
				}
				for _, record := range p.WaitLogRecords(t, "model call finished", 4) {
					if record["openvaala.model_call.id"] == nil || record["gen_ai.conversation.id"] != string(conv.ID) {
						t.Fatalf("model call log lacks identity: %v", record)
					}
				}
				tools := p.GetLogRecords(t, "tool call finished")
				if len(tools) != 1 || tools[0]["gen_ai.tool.call.id"] != "spawn" {
					t.Fatalf("tool call logs = %v", tools)
				}
			},
		},
		{
			name: "conversation_started_log",
			check: func(t *testing.T) {
				records := p.GetLogRecords(t, "conversation started")
				if len(records) != 1 || records[0]["gen_ai.conversation.id"] != string(conv.ID) {
					t.Fatalf("conversation started logs = %v", records)
				}
			},
		},
		{
			name: "logs_have_no_prompt_content",
			check: func(t *testing.T) {
				for _, text := range []string{"请委派一个审查任务", "检查登录模块", "子任务完成", "已汇总"} {
					if strings.Contains(p.GetLogText(), text) {
						t.Fatalf("logs contain %q", text)
					}
				}
			},
		},
		{
			name: "spans_carry_conversation_content",
			check: func(t *testing.T) {
				if !strings.Contains(telemetrytest.GetSpanAttribute(rootTurns[0], "gen_ai.input.messages"), "请委派一个审查任务") ||
					!strings.Contains(telemetrytest.GetSpanAttribute(rootTurns[0], "gen_ai.output.messages"), "已委派") {
					t.Fatalf("root turn content = %v", rootTurns[0].Attributes())
				}
				if !strings.Contains(telemetrytest.GetSpanAttribute(spawnSpan, "gen_ai.tool.call.arguments"), "检查登录模块") {
					t.Fatalf("spawn_agent arguments = %q", telemetrytest.GetSpanAttribute(spawnSpan, "gen_ai.tool.call.arguments"))
				}
				if !hasSpanAttributeText(chatSpans, "gen_ai.input.messages", "检查登录模块") {
					t.Error("no chat span carries the child task message in gen_ai.input.messages")
				}
				if !hasSpanAttributeText(chatSpans, "gen_ai.output.messages", "已汇总") {
					t.Error("no chat span carries the model reply in gen_ai.output.messages")
				}
			},
		},
		{
			name: "turn_summary_and_steps",
			check: func(t *testing.T) {
				root := rootTurns[0]
				if telemetrytest.GetSpanAttribute(root, "openvaala.turn.step_count") != "2" ||
					telemetrytest.GetSpanAttribute(root, "openvaala.turn.tool_calls") != "1" ||
					telemetrytest.GetSpanAttribute(root, "gen_ai.usage.input_tokens") != "250" ||
					telemetrytest.GetSpanAttribute(spawnSpan, "openvaala.turn.step") != "1" {
					t.Fatalf("root turn = %v, spawn = %v", root.Attributes(), spawnSpan.Attributes())
				}
			},
		},
		{
			name: "active_conversation_gauge",
			check: func(t *testing.T) {
				if got := p.GetMetricSum(t, "openvaala.conversation.active"); got != 1 {
					t.Fatalf("active conversations = %v, want 1", got)
				}
			},
		},
	}
	// 6. 逐个核对遥测，再停止 Runtime 检查对话计数归零。
	for _, tt := range tests {
		t.Run(tt.name, tt.check)
	}

	// 停止 Runtime 后：运行中对话数回到 0，对话以正常停止（INFO）收尾。
	t.Run("stop_releases_conversation", func(t *testing.T) {
		h.stop(t)
		if got := p.GetMetricSum(t, "openvaala.conversation.active"); got != 0 {
			t.Fatalf("active conversations after stop = %v, want 0", got)
		}
		stopped := p.GetLogRecords(t, "conversation stopped")
		if len(stopped) != 1 || stopped[0]["level"] != "INFO" || stopped[0]["gen_ai.conversation.id"] != string(conv.ID) {
			t.Fatalf("conversation stopped logs = %v", stopped)
		}
	})
}

// hasSpanAttributeText 是否有 span 的指定属性包含目标文本？
func hasSpanAttributeText(spans []sdktrace.ReadOnlySpan, key, text string) bool {
	// 1. 逐个读取属性并匹配文本。
	return slices.ContainsFunc(spans, func(span sdktrace.ReadOnlySpan) bool {
		return strings.Contains(telemetrytest.GetSpanAttribute(span, key), text)
	})
}

// filterSpans 返回指定属性等于目标文本的 span。
func filterSpans(spans []sdktrace.ReadOnlySpan, key, value string) []sdktrace.ReadOnlySpan {
	// 1. 按原顺序筛选匹配的 span。
	var found []sdktrace.ReadOnlySpan
	for _, span := range spans {
		if telemetrytest.GetSpanAttribute(span, key) == value {
			found = append(found, span)
		}
	}
	return found
}

// ── Runtime 测试夹具 ──

// runtimeStore 是一份测试用的共用存储：sqlite 记录库、文件日志和用它们建的 DAO。
type runtimeStore struct {
	// root 是数据目录：metadata.db 与 conversations/ 都在其中。
	root     string
	opt      *dao.DAOOpt
	query    dao.Query
	mutation dao.Mutation
}

// openRuntimeStore 创建测试独立的记录库、对话日志和 DAO。
func openRuntimeStore(t *testing.T) *runtimeStore {
	t.Helper()
	// 1. 创建数据目录和 sqlite 记录库，登记测试后的关闭操作。
	root := t.TempDir()
	database, err := storagesqlite.OpenDatabase(storagesqlite.NewDatabaseOpt(filepath.Join(root, "metadata.db"), dao.RecordNamespaces()...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	// 2. 创建文件日志存储，再构造 DAO 并返回夹具。
	logs, err := storagefile.NewLogStorage(storagefile.NewLogStorageOpt(filepath.Join(root, "conversations")))
	if err != nil {
		t.Fatal(err)
	}
	opt := dao.NewDAOOpt(database.Record(), logs)
	query, mutation, err := dao.NewDAO(opt)
	if err != nil {
		t.Fatal(err)
	}
	return &runtimeStore{
		root:     root,
		opt:      opt,
		query:    query,
		mutation: mutation,
	}
}

// testModelRef 是测试 Runtime 模型目录中第一个模型的引用，也是 Agent 的默认模型；目录中另有可切换的 test/b。
var testModelRef = defs.ModelRef{
	ProviderID: "test",
	ModelID:    "test",
}

// newTestRuntimeOpt 创建 assistant 与两个受控模型的测试配置。
func newTestRuntimeOpt(store *runtimeStore, model *runtimeModel) *runtime.RuntimeOpt {
	// 1. 设置默认 test/test 和可切换的 test/b 模型。
	return runtime.NewRuntimeOpt(store.opt.Records, store.opt.Logs, &runtime.Definitions{
		Agents: []*agent.AgentDefinition{{
			Name:         "assistant",
			Description:  "主 Agent",
			Instructions: "Test assistant",
			Model:        testModelRef,
		}},
		Models: []*defs.ModelDefinition{
			{
				Ref: testModelRef,
				Target: &llmkit.Target{
					Protocol: llmkit.ProtocolOpenAIChat,
					BaseURL:  model.server.URL,
					Model:    "test",
				},
			},
			{
				Ref: defs.ModelRef{
					ProviderID: "test",
					ModelID:    "b",
				},
				Target: &llmkit.Target{
					Protocol: llmkit.ProtocolOpenAIChat,
					BaseURL:  model.server.URL,
					Model:    "model-b",
				},
				ContextWindow:          64000,
				ReasoningEfforts:       []string{"low", "high"},
				DefaultReasoningEffort: "low",
			},
		},
	})
}

// runtimeModelRequest 是受控模型收到的一次请求：body 是解析后的请求体，测试往 reply 写回复。
type runtimeModelRequest struct {
	body  map[string]any
	reply chan runtimeModelResponse
}

// runtimeModelResponse 是受控模型的一次回复。
type runtimeModelResponse struct {
	// reasoning 非空时先作为推理增量写出。
	reasoning string
	// chunks 依次作为文本增量写出。
	chunks []string
	// release 非 nil 时写出第一段文本后等它关闭，让回复停在"生成到一半"。
	release <-chan struct{}
	// calls 是文本之后写出的工具调用。
	calls []llmkit.ToolCall
	// usage 非 nil 时在结束块之后写出 token 用量。
	usage *runtimeModelUsage
}

// runtimeModelUsage 是受控模型在流末尾上报的 token 用量。
type runtimeModelUsage struct {
	input  int
	output int
}

// runtimeModel 是测试逐次控制回复的流式模型服务：每个请求放进 requests，等测试回复。
type runtimeModel struct {
	server   *httptest.Server
	requests chan runtimeModelRequest
}

// newRuntimeModel 启动由测试逐次控制回复的流式模型服务。
func newRuntimeModel(t *testing.T) *runtimeModel {
	t.Helper()
	// 1. 创建请求队列和 HTTP 服务。
	model := &runtimeModel{requests: make(chan runtimeModelRequest, 128)}
	model.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 1. 解析并交出模型请求，等待测试回复或请求取消。
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		request := runtimeModelRequest{
			body:  body,
			reply: make(chan runtimeModelResponse, 1),
		}
		model.requests <- request
		var response runtimeModelResponse
		select {
		case response = <-request.reply:
		case <-r.Context().Done():
			return
		}
		// 2. 依次写出推理、文本、工具调用、结束块和可选用量。
		w.Header().Set("Content-Type", "text/event-stream")
		if response.reasoning != "" {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":%q}}]}\n\n", response.reasoning)
		}
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
		for index, call := range response.calls {
			fmt.Fprintf(w,
				"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":%d,\"id\":%q,\"type\":\"function\",\"function\":{\"name\":%q,\"arguments\":%q}}]}}]}\n\n",
				index, call.ID, call.Name, call.Arguments,
			)
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		if response.usage != nil {
			fmt.Fprintf(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":%d,\"total_tokens\":%d}}\n\n",
				response.usage.input, response.usage.output, response.usage.input+response.usage.output)
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	// 2. 登记模型服务关闭操作并返回夹具。
	t.Cleanup(model.server.Close)
	return model
}

// receive 等待模型的下一次请求。
func (m *runtimeModel) receive(t *testing.T) runtimeModelRequest {
	t.Helper()
	// 1. 最多等待 5 秒，返回请求，超时则结束测试。
	select {
	case request := <-m.requests:
		return request
	case <-time.After(5 * time.Second):
		t.Fatal("model request did not arrive")
		return runtimeModelRequest{}
	}
}

// requireNoRequest 检查短暂等待内没有新的模型请求。
func (m *runtimeModel) requireNoRequest(t *testing.T) {
	t.Helper()
	// 1. 等待 100 毫秒，收到意外请求则结束测试。
	select {
	case request := <-m.requests:
		t.Fatalf("unexpected model request: %v", request.body)
	case <-time.After(100 * time.Millisecond):
	}
}

// runtimeHarness 是一个在后台运行的测试 Runtime，带上它的存储、受控模型和 Run 的结束结果。
type runtimeHarness struct {
	store  *runtimeStore
	model  *runtimeModel
	rt     runtime.Runtime
	cancel context.CancelFunc
	// done 收 Run 的返回值；runErr 是第一次 stop 读到的返回值。
	done   chan error
	runErr error
	// ended stop 是否已经等到 Run 返回？
	ended bool
}

// startTestRuntime 使用默认测试配置启动 Runtime。
func startTestRuntime(t *testing.T, store *runtimeStore, model *runtimeModel) *runtimeHarness {
	t.Helper()
	// 1. 创建默认配置并交给启动夹具。
	return startTestRuntimeWithOpt(t, store, model, newTestRuntimeOpt(store, model))
}

// startTestRuntimeWithOpt 按指定配置启动 Runtime 并登记测试收尾。
func startTestRuntimeWithOpt(t *testing.T, store *runtimeStore, model *runtimeModel, opt *runtime.RuntimeOpt) *runtimeHarness {
	t.Helper()
	// 1. 创建 Runtime，失败则结束测试。
	rt, err := runtime.NewRuntime(opt)
	if err != nil {
		t.Fatal(err)
	}
	// 2. 创建运行上下文和夹具，启动 Run，测试结束时取消并等待。
	ctx, cancel := context.WithCancel(t.Context())
	h := &runtimeHarness{
		store:  store,
		model:  model,
		rt:     rt,
		cancel: cancel,
		done:   make(chan error, 1),
	}
	go func() { h.done <- rt.Run(ctx) }()
	t.Cleanup(func() { h.stop(t) })
	return h
}

// stop 停止测试 Runtime 并等待结束。
func (h *runtimeHarness) stop(t *testing.T) error {
	t.Helper()
	// 1. 首次调用取消并等待最多 10 秒，记录结束状态和返回错误；重复调用返回首次结果。
	if !h.ended {
		h.cancel()
		select {
		case h.runErr = <-h.done:
		case <-time.After(10 * time.Second):
			t.Fatal("Runtime.Run did not return")
		}
		h.ended = true
	}
	return h.runErr
}

// create 创建 assistant 对话并检查成功。
func (h *runtimeHarness) create(t *testing.T) defs.Conversation {
	t.Helper()
	// 1. 请求创建对话，失败则结束测试，成功返回记录。
	record, err := h.rt.CreateConversation(t.Context(), &runtime.CreateConversationRequest{AgentName: "assistant"})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

// submit 向指定对话提交用户文本并返回输入 ItemID。
func (h *runtimeHarness) submit(t *testing.T, conversationID defs.ConversationID, text string, inputID defs.InputID) defs.ItemID {
	t.Helper()
	// 1. 提交普通模式输入，检查无错误且 ItemID 非空。
	itemID, err := h.rt.SubmitInput(t.Context(), newSubmitInputRequest(conversationID, text, inputID))
	if err != nil || itemID == "" {
		t.Fatalf("SubmitInput() = %q, %v", itemID, err)
	}
	return itemID
}

// newSubmitInputRequest 创建普通模式的用户文本输入。
func newSubmitInputRequest(conversationID defs.ConversationID, text string, inputID defs.InputID) *runtime.SubmitInputRequest {
	// 1. 填入对话、InputID、用户地址和文本正文。
	return &runtime.SubmitInputRequest{
		ConversationID: conversationID,
		InputID:        inputID,
		Mode:           session.SessionInputModeNormal,
		From: &defs.Address{
			Kind: defs.IdentityUser,
			ID:   "user",
		},
		Content: []*defs.Content{{
			Kind: defs.ContentText,
			Text: &defs.TextContent{Text: text},
		}},
	}
}

// waitLog 等待 Session 日志达到指定条数。
func (h *runtimeHarness) waitLog(t *testing.T, conversationID defs.ConversationID, sessionID defs.SessionID, count int) []dao.LoggedItem {
	t.Helper()
	// 1. 在 5 秒内按正序读取最多 100 条日志，达到条数后返回。
	deadline := time.Now().Add(5 * time.Second)
	for {
		page, err := h.rt.ListItems(t.Context(), &runtime.ListItemsRequest{
			ConversationID: conversationID,
			SessionID:      sessionID,
			Order:          storage.DirectionAsc,
			Limit:          100,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) >= count {
			return page.Items
		}
		if time.Now().After(deadline) {
			t.Fatalf("log has %d items, want %d", len(page.Items), count)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitTurnStatus 等待指定 Turn 到达目标状态。
func (h *runtimeHarness) waitTurnStatus(t *testing.T, sessionID defs.SessionID, turnID defs.TurnID, status defs.TurnStatus) {
	t.Helper()
	// 1. 在 5 秒内轮询 Turn 记录，未到目标状态则继续等待。
	deadline := time.Now().Add(5 * time.Second)
	for {
		record, found, err := h.store.query.GetTurn(t.Context(), sessionID, turnID)
		if err != nil {
			t.Fatal(err)
		}
		if found && record.Status == status {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Turn %s = %+v, want status %q", turnID, record, status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestRuntimeGetSkills 检查对话和草稿 Agent 的 Skill 筛选。
func TestRuntimeGetSkills(t *testing.T) {
	// 1. 分别测试草稿、运行中和重启后对话的 Skill 目录。
	for _, tt := range []struct {
		name string
		// conversation 是否先建对话再按对话查？running 时对话有 Turn 在跑，reopen 时重启 Runtime 后再打开，unknown 时查不存在的 Agent。
		conversation, running, reopen, unknown bool
		// projectSkill 草稿请求的 local 目录下是否有项目 skill proj？有时 assistant 也点名它。
		projectSkill bool
		wantNames    []string
	}{
		{
			name:      "draft",
			wantNames: []string{"review"},
		},
		{
			name:         "persisted_conversation",
			conversation: true,
			wantNames:    []string{"review"},
		},
		{
			name:         "running_conversation",
			conversation: true,
			running:      true,
			wantNames:    []string{"review"},
		},
		{
			name:         "reopened_runtime",
			conversation: true,
			reopen:       true,
			wantNames:    []string{"review"},
		},
		{
			name:    "unknown_agent",
			unknown: true,
		},
		{
			name:         "draft_scans_project_directory",
			projectSkill: true,
			wantNames:    []string{"proj", "review"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store, model := openRuntimeStore(t), newRuntimeModel(t)
			opt := newExecutorRuntimeOpt(store, model, newLocalExecutors(t)...)
			opt.Definitions.Skills = []*skill.Skill{{
				Name:        "review",
				Description: "Review code",
				Path:        "/skills/review/SKILL.md",
				Executor:    "local",
			}, {Name: "tests"}}
			opt.Definitions.Agents[0].SkillNames = []string{"review"}
			request := &runtime.GetSkillsRequest{AgentName: "assistant"}
			if tt.projectSkill {
				project := t.TempDir()
				writeRuntimeTestSkill(t, filepath.Join(project, ".agents", "skills"), "proj")
				opt.Definitions.Agents[0].SkillNames = []string{"review", "proj"}
				request.ExecutorSettings = defs.ExecutorSettings{WorkspaceDirectories: map[string]string{defs.LocalExecutorID: project}}
			}
			h := startTestRuntimeWithOpt(t, store, model, opt)
			if tt.conversation {
				record := h.create(t)
				request.ConversationID = record.ID
				request.AgentName = "ignored"
				if tt.running {
					h.submit(t, record.ID, "hold", "")
					model.receive(t)
				}
				if tt.reopen {
					// 第一条输入才把新对话写进共用库，重启后才能打开。
					h.completeRound(t, record, "保存", 2)
					if err := h.stop(t); !errors.Is(err, context.Canceled) {
						t.Fatal(err)
					}
					h = startTestRuntimeWithOpt(t, store, model, opt)
					if _, err := h.rt.OpenConversation(t.Context(), &runtime.OpenConversationRequest{ConversationID: record.ID}); err != nil {
						t.Fatal(err)
					}
				}
			}
			if tt.unknown {
				request.AgentName = "unknown"
			}
			got, err := h.rt.GetSkills(t.Context(), request)
			if tt.unknown {
				if err == nil {
					t.Fatal("unknown agent accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, info := range got {
				names = append(names, info.Name)
			}
			if !slices.Equal(names, tt.wantNames) || got[len(got)-1].Executor != "local" || got[len(got)-1].Path != "/skills/review/SKILL.md" {
				t.Fatalf("skills = %+v, want %v with review from the shared skills", got, tt.wantNames)
			}
			model.requireNoRequest(t)
		})
	}
}

// writeRuntimeTestSkill 在指定目录写入一份测试 Skill。
func writeRuntimeTestSkill(t *testing.T, directory, name string) {
	t.Helper()
	// 1. 创建 Skill 目录，写入名称和说明的 SKILL.md。
	path := filepath.Join(directory, name, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\nname: "+name+"\ndescription: "+name+" 技能\n---\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

// TestRuntimeListAgents 检查初始和 reload 后的 Agent 列表：顺序、说明、默认模型，且不含系统指令。
func TestRuntimeListAgents(t *testing.T) {
	// 1. 列出初始定义和 reload 换掉 Agent 的测试场景。
	tests := []struct {
		name   string
		reload bool
		want   []runtime.AgentInfo
	}{
		{
			name: "initial_definitions",
			want: []runtime.AgentInfo{{Name: "assistant", Description: "主 Agent", DefaultModel: defs.ModelRef{ProviderID: "test", ModelID: "test"}}},
		},
		{
			name:   "reload_replaces_agents_and_omits_unset_model",
			reload: true,
			want: []runtime.AgentInfo{
				{Name: "assistant", Description: "主 Agent", DefaultModel: defs.ModelRef{ProviderID: "test", ModelID: "test"}},
				{Name: "reviewer"},
			},
		},
	}
	// 2. 查询后与预期逐项比较。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, model := openRuntimeStore(t), newRuntimeModel(t)
			h := startTestRuntimeWithOpt(t, store, model, newTestRuntimeOpt(store, model))
			if tt.reload {
				definitions := newTestRuntimeOpt(store, model).Definitions
				definitions.Agents = append(definitions.Agents, &agent.AgentDefinition{Name: "reviewer", Instructions: "secret"})
				h.setDefinitionsRequest(t, definitions)
			}
			got, err := h.rt.ListAgents(t.Context())
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ListAgents() = %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
}

// TestRuntimeListExecutors 检查初始和 reload 后的 Executor 目录。
func TestRuntimeListExecutors(t *testing.T) {
	// 1. 列出初始定义和 reload 换掉 Executor 的测试场景。
	tests := []struct {
		name   string
		reload bool
	}{
		{name: "initial_definitions"},
		{
			name:   "reload_replaces_executors",
			reload: true,
		},
	}
	// 2. 查询初始和替换后的目录，检查顺序、说明和默认工作目录，不连接远端 Executor。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, model := openRuntimeStore(t), newRuntimeModel(t)
			local := newLocalExecutor(t, defs.LocalExecutorID)
			builtin := &defs.ExecutorDefinition{
				ID:                 "openvaala",
				Kind:               defs.ExecutorKindVirtual,
				Description:        "内置",
				WorkspaceDirectory: "/workspace",
			}
			remote := &defs.ExecutorDefinition{
				ID:                 "remote",
				Kind:               defs.ExecutorKindSSH,
				Description:        "离线",
				WorkspaceDirectory: "/project",
				SSH:                &defs.SSHExecutorDefinition{Address: "test@127.0.0.1:1"},
			}
			h := startTestRuntimeWithOpt(t, store, model, newExecutorRuntimeOpt(store, model, local, builtin, remote))
			want := []conversation.ExecutorDetails{
				{
					ID:                 defs.LocalExecutorID,
					Description:        local.Description,
					WorkspaceDirectory: local.WorkspaceDirectory,
				},
				{
					ID:                 "openvaala",
					Description:        "内置",
					WorkspaceDirectory: "/workspace",
				},
				{
					ID:                 "remote",
					Description:        "离线",
					WorkspaceDirectory: "/project",
				},
			}
			if tt.reload {
				extra := &defs.ExecutorDefinition{
					ID:                 "extra",
					Kind:               defs.ExecutorKindVirtual,
					Description:        "新加的",
					WorkspaceDirectory: "/scratch",
				}
				definitions := newTestRuntimeOpt(store, model).Definitions
				definitions.Executors = []*defs.ExecutorDefinition{local, builtin, extra}
				h.setDefinitionsRequest(t, definitions)
				want[2] = conversation.ExecutorDetails{
					ID:                 "extra",
					Description:        "新加的",
					WorkspaceDirectory: "/scratch",
				}
			}
			got, err := h.rt.ListExecutors(t.Context())
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("ListExecutors() = %+v, %v; want %+v", got, err, want)
			}
		})
	}
}

// TestRuntimeOpenLegacyConversation 检查缺少 Executor 设置的记录在打开时补齐但不回写。
func TestRuntimeOpenLegacyConversation(t *testing.T) {
	// 1. 列出缺少 Executor 设置的记录在打开时补齐但不回写的测试场景。
	tests := []struct {
		name     string
		settings defs.ExecutorSettings
	}{
		{name: "zero_settings"},
		{
			name:     "settings_without_local_directory",
			settings: defs.ExecutorSettings{ExecutorIDs: []string{defs.LocalExecutorID}},
		},
	}
	// 2. 写入缺少 Executor 设置的记录后冷打开，检查补齐 local 目录但不回写存储。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, model := openRuntimeStore(t), newRuntimeModel(t)
			opt := newExecutorRuntimeOpt(store, model, newLocalExecutors(t)...)
			h := startTestRuntimeWithOpt(t, store, model, opt)
			legacy := h.create(t)
			// 第一条输入才把新对话写进共用库，重启后才能打开。
			h.completeRound(t, legacy, "保存", 2)
			h.stop(t)
			record, _, err := store.query.GetConversation(t.Context(), legacy.ID)
			if err != nil {
				t.Fatal(err)
			}
			record.ExecutorSettings = tt.settings
			if err := store.mutation.SetConversation(t.Context(), record); err != nil {
				t.Fatal(err)
			}
			restarted := startTestRuntimeWithOpt(t, store, model, opt)
			if _, err := restarted.rt.OpenConversation(t.Context(), &runtime.OpenConversationRequest{ConversationID: legacy.ID}); err != nil {
				t.Fatal(err)
			}
			details, err := restarted.rt.GetSessionDetails(t.Context(), &runtime.GetSessionDetailsRequest{
				ConversationID: legacy.ID,
				SessionID:      legacy.RootSessionID,
			})
			if localDirectory := opt.Definitions.Executors[0].WorkspaceDirectory; err != nil || len(details.Executors) != 1 || details.Executors[0].WorkspaceDirectory != localDirectory {
				t.Fatalf("session executors = %+v, %v; want local at %s", details.Executors, err, localDirectory)
			}
			saved, err := restarted.rt.GetConversation(t.Context(), &runtime.GetConversationRequest{ConversationID: legacy.ID})
			if err != nil || !reflect.DeepEqual(saved.ExecutorSettings, tt.settings) {
				t.Fatalf("saved settings = %+v, %v; want %+v unchanged", saved.ExecutorSettings, err, tt.settings)
			}
		})
	}
}

// TestRuntimeIsConversationOpen 检查新建和不存在对话的打开状态。
func TestRuntimeIsConversationOpen(t *testing.T) {
	// 1. 列出新建和不存在对话的打开状态的测试场景。
	tests := []struct {
		name string
		// conversationID 返回要查询的对话：刚新建的，或者根本不存在的。
		conversationID func(t *testing.T, h *runtimeHarness) defs.ConversationID
		want           bool
	}{
		{
			name:           "created",
			conversationID: func(t *testing.T, h *runtimeHarness) defs.ConversationID { return h.create(t).ID },
			want:           true,
		},
		{
			name:           "missing",
			conversationID: func(*testing.T, *runtimeHarness) defs.ConversationID { return defs.NewConversationID() },
		},
	}
	// 2. 分别查询已创建和不存在对话，检查打开标志和错误结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := startTestRuntime(t, openRuntimeStore(t), newRuntimeModel(t))
			got, err := h.rt.IsConversationOpen(t.Context(), &runtime.GetConversationRequest{ConversationID: tt.conversationID(t, h)})
			if err != nil || got != tt.want {
				t.Fatalf("IsConversationOpen() = %v, %v; want %v", got, err, tt.want)
			}
		})
	}
}

// TestRuntimeSetConversationExecutorSettings 检查改 Executor 设置后对话原地换环境、失败保留原设置、后台命令停止。
func TestRuntimeSetConversationExecutorSettings(t *testing.T) {
	// 1. 准备设置夹具和断言，列出换环境、忙碌、失败保留和后台命令停止的测试场景。
	type settingsFixture struct {
		conversation defs.Conversation
		// previous 是新建对话时的设置。
		previous defs.ExecutorSettings
		// request 是改成 second 为默认、local 目录不变的请求。
		request *runtime.SetConversationExecutorSettingsRequest
		// opt 是启动 Runtime 用的配置。
		opt *runtime.RuntimeOpt
	}
	// requireSavedSettings 要求已保存的记录带 want。
	requireSavedSettings := func(t *testing.T, h *runtimeHarness, conversationID defs.ConversationID, want defs.ExecutorSettings) {
		t.Helper()
		saved, err := h.rt.GetConversation(t.Context(), &runtime.GetConversationRequest{ConversationID: conversationID})
		if err != nil || !reflect.DeepEqual(saved.ExecutorSettings, want) {
			t.Fatalf("saved executor settings = %+v, %v; want %+v", saved.ExecutorSettings, err, want)
		}
	}
	// requireDefaultExecutor 要求运行中对话的根 Session 以 want 为默认 Executor。
	requireDefaultExecutor := func(t *testing.T, h *runtimeHarness, conv defs.Conversation, want string) {
		t.Helper()
		details, err := h.rt.GetSessionDetails(t.Context(), &runtime.GetSessionDetailsRequest{
			ConversationID: conv.ID,
			SessionID:      conv.RootSessionID,
		})
		if err != nil || len(details.Executors) == 0 || details.Executors[0].ID != want {
			t.Fatalf("session executors = %+v, %v; want %q first", details.Executors, err, want)
		}
	}
	tests := []struct {
		name string
		run  func(t *testing.T, h *runtimeHarness, fixture settingsFixture)
	}{
		{
			name: "idle_reconfigures_in_place",
			run: func(t *testing.T, h *runtimeHarness, fixture settingsFixture) {
				conv := fixture.conversation
				statuses := h.subscribeConversation(t, conv.ID)
				record, err := h.setExecutorSettings(t, fixture.request)
				if err != nil || !reflect.DeepEqual(record.ExecutorSettings, fixture.request.ExecutorSettings) {
					t.Fatalf("SetConversationExecutorSettings() = %+v, %v; want settings %+v", record.ExecutorSettings, err, fixture.request.ExecutorSettings)
				}
				waitEventsClosed(t, statuses.Events)
				if reason := statuses.GetCloseReason(); !errors.Is(reason, runtime.ErrConversationReconfigured) {
					t.Fatalf("old subscription close reason = %v, want %v", reason, runtime.ErrConversationReconfigured)
				}
				requireSavedSettings(t, h, conv.ID, fixture.request.ExecutorSettings)
				requireDefaultExecutor(t, h, conv, "second")
				watcher := h.subscribeSession(t, conv.ID, conv.RootSessionID)
				h.submit(t, conv.ID, "换了位置之后", "")
				h.model.receive(t).reply <- runtimeModelResponse{chunks: []string{"好"}}
				events := collectUntil(t, watcher.Events, isTurnFinished)
				if finished := getSessionEvent(events[len(events)-1]).(*session.SessionTurnFinished); finished.Status != defs.TurnStatusCompleted {
					t.Fatalf("Turn after reconfiguring = %+v, want completed", finished)
				}
			},
		},
		{
			name: "busy_changes_nothing",
			run: func(t *testing.T, h *runtimeHarness, fixture settingsFixture) {
				conv := fixture.conversation
				statuses := h.subscribeConversation(t, conv.ID)
				h.submit(t, conv.ID, "长任务", "")
				request := h.model.receive(t)
				collectUntil(t, statuses.Events, func(info runtime.SessionRuntimeInfo) bool { return info.State == session.SessionStateRunning })
				if _, err := h.setExecutorSettings(t, fixture.request); !errors.Is(err, session.ErrSessionBusy) {
					t.Fatalf("SetConversationExecutorSettings() error = %v, want %v", err, session.ErrSessionBusy)
				}
				requireSavedSettings(t, h, conv.ID, fixture.previous)
				// 旧订阅仍在：这一轮的结束照常推送过来。
				request.reply <- runtimeModelResponse{chunks: []string{"完成"}}
				collectUntil(t, statuses.Events, isTurnCompletedInfo)
				requireDefaultExecutor(t, h, conv, defs.LocalExecutorID)
			},
		},
		{
			// 只有后台命令在跑时照常换环境：旧的 Session 停下时停掉它的命令，新的根 Session 从空的命令集合开始，旧编号失效。
			name: "settings_change_stops_commands",
			run: func(t *testing.T, h *runtimeHarness, fixture settingsFixture) {
				conv := fixture.conversation
				watcher := h.subscribeSession(t, conv.ID, conv.RootSessionID)
				started := h.runCommandToolRound(t, conv, watcher, "call-bash", "bash", `{"command":"echo $$; exec sleep 30","wait_seconds":1}`)
				pid, err := strconv.Atoi(strings.TrimSpace(started.Output))
				if started.ToolError != nil || started.Status != "running" || err != nil {
					t.Fatalf("bash result = %+v, want a running command that printed its process ID", started)
				}
				if _, err := h.setExecutorSettings(t, fixture.request); err != nil {
					t.Fatalf("SetConversationExecutorSettings() error = %v", err)
				}
				// 换环境时已停下 Session 并等到它的命令结束。
				if isProcessAlive(pid) {
					t.Fatalf("command process %d is still running after reconfiguring", pid)
				}
				// 新环境的默认 Executor 换成了 second，命令是在 local 上启动的，按 local 查。
				watcher = h.subscribeSession(t, conv.ID, conv.RootSessionID)
				listed := h.runCommandToolRound(t, conv, watcher, "call-list", "list_commands", `{"executor":"local"}`)
				if listed.ToolError != nil || len(listed.Commands) != 0 {
					t.Fatalf("list_commands result = %+v, want no commands", listed)
				}
				read := h.runCommandToolRound(t, conv, watcher, "call-read", "read_command_output", `{"command_id":"`+started.CommandID+`","executor":"local","wait_seconds":0}`)
				if read.ToolError == nil || read.ToolError.Code != "command_not_found" {
					t.Fatalf("read_command_output result = %+v, want command_not_found", read)
				}
			},
		},
		{
			name: "settings_change_closed_conversation_opens_with_new_settings",
			run: func(t *testing.T, h *runtimeHarness, fixture settingsFixture) {
				// 第一条输入才把新对话写进共用库，重启后才能打开。
				h.completeRound(t, fixture.conversation, "保存", 2)
				h.stop(t)
				restarted := startTestRuntimeWithOpt(t, h.store, h.model, fixture.opt)
				record, err := restarted.setExecutorSettings(t, fixture.request)
				if err != nil || !reflect.DeepEqual(record.ExecutorSettings, fixture.request.ExecutorSettings) {
					t.Fatalf("SetConversationExecutorSettings() = %+v, %v; want settings %+v", record.ExecutorSettings, err, fixture.request.ExecutorSettings)
				}
				isOpen, err := restarted.rt.IsConversationOpen(t.Context(), &runtime.GetConversationRequest{ConversationID: fixture.conversation.ID})
				if err != nil || !isOpen {
					t.Fatalf("IsConversationOpen() = %v, %v; want true", isOpen, err)
				}
				requireSavedSettings(t, restarted, fixture.conversation.ID, fixture.request.ExecutorSettings)
				requireDefaultExecutor(t, restarted, fixture.conversation, "second")
			},
		},
		{
			// 新的默认 Executor 根目录不存在：装配失败，什么都不改。
			name: "invalid_settings_keep_previous",
			run: func(t *testing.T, h *runtimeHarness, fixture settingsFixture) {
				conv := fixture.conversation
				statuses := h.subscribeConversation(t, conv.ID)
				fixture.request.ExecutorSettings.WorkspaceDirectories["second"] = filepath.Join(t.TempDir(), "missing")
				_, err := h.setExecutorSettings(t, fixture.request)
				if want := `executor "second": `; err == nil || !strings.HasPrefix(err.Error(), want) {
					t.Fatalf("SetConversationExecutorSettings() error = %v, want one starting with %q", err, want)
				}
				requireSavedSettings(t, h, conv.ID, fixture.previous)
				requireDefaultExecutor(t, h, conv, defs.LocalExecutorID)
				// 对话按原来的环境继续运行，旧订阅也没关：提交的输入照常完成。
				h.submit(t, conv.ID, "还在原来的位置", "")
				h.model.receive(t).reply <- runtimeModelResponse{chunks: []string{"好"}}
				collectUntil(t, statuses.Events, isTurnCompletedInfo)
			},
		},
		{
			// 客户端登记的 MCP 不随环境换掉：改设置之后 GetMCPServers 仍列出它。
			name: "client_mcp_kept",
			run: func(t *testing.T, h *runtimeHarness, fixture settingsFixture) {
				conv := fixture.conversation
				server := conversation.ClientMCPServer{
					ID:        clientFakeServerID,
					Transport: &commandTransport{command: buildFakeMCPServer(t)},
				}
				request := &runtime.SetClientMCPServersRequest{
					ConversationID: conv.ID,
					ClientID:       "c1",
					Servers:        []conversation.ClientMCPServer{server},
				}
				if err := h.rt.SetClientMCPServers(t.Context(), request); err != nil {
					t.Fatalf("SetClientMCPServers() error = %v", err)
				}
				if _, err := h.setExecutorSettings(t, fixture.request); err != nil {
					t.Fatalf("SetConversationExecutorSettings() error = %v", err)
				}
				servers, err := h.rt.GetMCPServers(t.Context(), &runtime.GetConversationRequest{ConversationID: conv.ID})
				if err != nil || !slices.ContainsFunc(servers, func(info conversation.MCPServerInfo) bool {
					return info.ExecutorID == defs.ClientExecutorID && info.ServerID == clientFakeServerID
				}) {
					t.Fatalf("GetMCPServers() = %+v, %v; want the client server %s", servers, err, clientFakeServerID)
				}
			},
		},
	}
	// 2. 建好 local 和 second 两个 Executor 和对话，按场景改设置，检查默认 Executor、订阅关闭、失败时原设置保留及后台命令停止。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, model := openRuntimeStore(t), newRuntimeModel(t)
			local, second := newLocalExecutor(t, defs.LocalExecutorID), newLocalExecutor(t, "second")
			opt := newExecutorRuntimeOpt(store, model, local, second)
			// Agent 名为 default，点名共用 skill deploy。
			opt.Definitions.Agents[0].Name = "default"
			opt.Definitions.Agents[0].SkillNames = []string{"deploy"}
			opt.Definitions.Skills = []*skill.Skill{{
				Name:        "deploy",
				Description: "部署",
				Path:        "/skills/deploy/SKILL.md",
				Executor:    defs.LocalExecutorID,
			}}
			h := startTestRuntimeWithOpt(t, store, model, opt)
			previous := defs.ExecutorSettings{WorkspaceDirectories: map[string]string{defs.LocalExecutorID: local.WorkspaceDirectory}}
			conv, err := h.rt.CreateConversation(t.Context(), &runtime.CreateConversationRequest{
				AgentName:        "default",
				ExecutorSettings: previous,
			})
			if err != nil {
				t.Fatal(err)
			}
			tt.run(t, h, settingsFixture{
				conversation: conv,
				previous:     previous,
				opt:          opt,
				request: &runtime.SetConversationExecutorSettingsRequest{
					ConversationID: conv.ID,
					ExecutorSettings: defs.ExecutorSettings{
						DefaultExecutorID:    "second",
						WorkspaceDirectories: map[string]string{defs.LocalExecutorID: local.WorkspaceDirectory, "second": second.WorkspaceDirectory},
					},
				},
			})
		})
	}
}

// setExecutorSettings 提交 Executor 设置变更并等待结果。
func (h *runtimeHarness) setExecutorSettings(t *testing.T, request *runtime.SetConversationExecutorSettingsRequest) (defs.Conversation, error) {
	t.Helper()
	// 1. 启动设置变更任务，将记录和错误写入结果通道。
	type setResult struct {
		record defs.Conversation
		err    error
	}
	done := make(chan setResult, 1)
	go func() {
		record, err := h.rt.SetConversationExecutorSettings(t.Context(), request)
		done <- setResult{
			record: record,
			err:    err,
		}
	}()
	// 2. 等待最多 5 秒，超时则结束测试。
	select {
	case result := <-done:
		return result.record, result.err
	case <-time.After(5 * time.Second):
		t.Fatal("SetConversationExecutorSettings did not return within 5 seconds")
		return defs.Conversation{}, nil
	}
}

// TestRuntimeRunningConversations 检查全局运行集合、主子 Turn 和订阅关闭。
func TestRuntimeRunningConversations(t *testing.T) {
	// 1. 通过真实 Runtime 入口运行各场景。
	for _, scenario := range []string{"root", "multiple", "child", "background_command", "cancel", "runtime_stop", "conversation_exit", "subscribe_while_finishing"} {
		t.Run(scenario, func(t *testing.T) {
			store, model := openRuntimeStore(t), newRuntimeModel(t)
			if scenario == "conversation_exit" {
				store.opt.Logs = &unsettledTurnLogs{LogStorage: store.opt.Logs}
			}
			h := startTestRuntimeWithOpt(t, store, model, newExecutorRuntimeOpt(store, model, newLocalExecutor(t, defs.LocalExecutorID)))
			conv := h.create(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			subscription, err := h.rt.SubscribeRunningConversations(ctx)
			if err != nil || subscription.Initial.ConversationIDs == nil || len(subscription.Initial.ConversationIDs) != 0 {
				t.Fatalf("initial = %+v, %v", subscription, err)
			}
			if scenario == "cancel" || scenario == "runtime_stop" {
				want := context.Canceled
				if scenario == "cancel" {
					cancel()
				} else {
					h.stop(t)
					want = runtime.ErrRuntimeStopped
				}
				waitEventsClosed(t, subscription.Events)
				if !errors.Is(subscription.GetCloseReason(), want) {
					t.Fatalf("close reason = %v, want %v", subscription.GetCloseReason(), want)
				}
				return
			}
			// 2. 后台命令仍让 IsIdle 返回 false，但完整运行集合不包含它。
			if scenario == "background_command" {
				watcher := h.subscribeSession(t, conv.ID, conv.RootSessionID)
				result := h.runCommandToolRound(t, conv, watcher, "background", "bash", `{"command":"sleep 60","wait_seconds":0}`)
				if result.Status != "running" {
					t.Fatalf("background command = %+v", result)
				}
				waitRunningConversations(t, subscription.Events, nil)
				late, err := h.rt.SubscribeRunningConversations(ctx)
				if err != nil || len(late.Initial.ConversationIDs) != 0 {
					t.Fatalf("background initial = %+v, %v", late, err)
				}
				if idle, err := h.rt.IsIdle(t.Context()); err != nil || idle {
					t.Fatalf("IsIdle = %v, %v; want background command busy", idle, err)
				}
				return
			}
			// 3. 主 Turn 开始后集合包含对话，主结束而子 Turn 运行时仍包含。
			watcher := h.subscribeSession(t, conv.ID, conv.RootSessionID)
			h.submit(t, conv.ID, "运行", "")
			request := model.receive(t)
			waitRunningConversations(t, subscription.Events, []defs.ConversationID{conv.ID})
			if scenario == "multiple" {
				other := h.create(t)
				h.submit(t, other.ID, "第二个对话", "")
				otherRequest := model.receive(t)
				waitRunningConversations(t, subscription.Events, []defs.ConversationID{conv.ID, other.ID})
				request.reply <- runtimeModelResponse{chunks: []string{"结束"}}
				collectUntil(t, watcher.Events, isTurnFinished)
				waitRunningConversations(t, subscription.Events, []defs.ConversationID{other.ID})
				otherRequest.reply <- runtimeModelResponse{chunks: []string{"结束"}}
				waitRunningConversations(t, subscription.Events, nil)
				return
			}
			if scenario == "child" {
				request.reply <- runtimeModelResponse{calls: []llmkit.ToolCall{{ID: "spawn", Name: "spawn_agent", Arguments: `{"task_name":"review","message":"check"}`}}}
				root, child := model.receiveRootAndTask(t, "review")
				root.reply <- runtimeModelResponse{chunks: []string{"主任务结束"}}
				collectUntil(t, watcher.Events, isTurnFinished)
				select {
				case running := <-subscription.Events:
					t.Fatalf("root finish changed running set while child runs: %+v", running)
				case <-time.After(50 * time.Millisecond):
				}
				late, err := h.rt.SubscribeRunningConversations(ctx)
				if err != nil || !slices.Equal(late.Initial.ConversationIDs, []defs.ConversationID{conv.ID}) {
					t.Fatalf("child running = %+v, %v", late, err)
				}
				child.reply <- runtimeModelResponse{chunks: []string{"子任务结束"}}
				model.receive(t).reply <- runtimeModelResponse{chunks: []string{"汇总"}}
			} else {
				request.reply <- runtimeModelResponse{chunks: []string{"结束"}}
			}
			if scenario == "conversation_exit" {
				waitEventsClosed(t, watcher.Events)
				waitRunningConversations(t, subscription.Events, nil)
				late, err := h.rt.SubscribeRunningConversations(ctx)
				if err != nil || len(late.Initial.ConversationIDs) != 0 {
					t.Fatalf("stopped conversation initial = %+v, %v", late, err)
				}
				return
			}
			// 4. 完成期间登记的新订阅最终也收敛到空集合。
			if scenario == "subscribe_while_finishing" {
				late, err := h.rt.SubscribeRunningConversations(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if len(late.Initial.ConversationIDs) != 0 {
					waitRunningConversations(t, late.Events, nil)
				}
			}
			collectUntil(t, watcher.Events, isTurnFinished)
			waitRunningConversations(t, subscription.Events, nil)
		})
	}
}

// waitRunningConversations 等待完整运行集合与预期成员一致。
func waitRunningConversations(t *testing.T, events <-chan runtime.RunningConversations, want []defs.ConversationID) {
	t.Helper()
	// 1. 读取到预期集合或超时。
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case running, open := <-events:
			if !open {
				t.Fatal("running subscription closed")
			}
			if slices.Equal(running.ConversationIDs, want) {
				return
			}
		case <-timer.C:
			t.Fatalf("running set did not become %v", want)
		}
	}
}

// TestRuntimeRunningConversationsScanCancellation 检查初始与后续读取取消时不返回假空集合。
func TestRuntimeRunningConversationsScanCancellation(t *testing.T) {
	// 1. 分别在首次读取和已建立订阅后的重读期间取消上下文。
	for _, phase := range []string{"initial", "update"} {
		t.Run(phase, func(t *testing.T) {
			store, model := openRuntimeStore(t), newRuntimeModel(t)
			logs := &runningScanBlockingLogs{LogStorage: store.opt.Logs, entered: make(chan struct{}, 1), release: make(chan struct{})}
			store.opt.Logs = logs
			h := startTestRuntime(t, store, model)
			defer close(logs.release)
			conv, other := h.create(t), h.create(t)
			var subscription *runtime.RunningConversationsSubscription
			if phase == "update" {
				var err error
				subscription, err = h.rt.SubscribeRunningConversations(t.Context())
				if err != nil {
					t.Fatal(err)
				}
			}
			h.submit(t, conv.ID, "运行", "")
			model.receive(t)
			if phase == "update" {
				waitRunningConversations(t, subscription.Events, []defs.ConversationID{conv.ID})
			}
			if err := h.rt.SetConversationTitle(t.Context(), &runtime.SetConversationTitleRequest{ConversationID: conv.ID}); err != nil {
				t.Fatal(err)
			}
			// 2. 让一个已运行对话的事件循环停在真实标题写入中。
			logs.path = storage.StreamPath(conv.ID)
			logs.block.Store(true)
			titleDone := make(chan error, 1)
			go func() {
				_, err := h.rt.SubmitInput(t.Context(), newSubmitInputRequest(conv.ID, "阻塞", ""))
				titleDone <- err
			}()
			t.Cleanup(func() {
				select {
				case <-titleDone:
				case <-time.After(5 * time.Second):
					t.Error("input did not finish")
				}
			})
			select {
			case <-logs.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("title write did not block")
			}
			if phase == "initial" {
				ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
				defer cancel()
				initial, err := h.rt.SubscribeRunningConversations(ctx)
				if !errors.Is(err, context.DeadlineExceeded) || initial != nil {
					t.Fatalf("canceled initial = %+v, %v", initial, err)
				}
			} else {
				// 3. 另一个对话开始 Turn，触发完整集合重读，再取消 Runtime。
				h.submit(t, other.ID, "另一个任务", "")
				model.receive(t)
				h.cancel()
				for events := subscription.Events; events != nil; {
					select {
					case running, open := <-events:
						if !open {
							events = nil
						} else if len(running.ConversationIDs) == 0 {
							t.Fatal("canceled scan published a false empty set")
						}
					case <-time.After(5 * time.Second):
						t.Fatal("running subscription did not close")
					}
				}
				if subscription.GetCloseReason() == nil {
					t.Fatal("canceled update did not close subscription")
				}
			}
		})
	}
}

// runningScanBlockingLogs 在测试要求时暂停 metadata 写入，直到测试放行或上下文结束。
type runningScanBlockingLogs struct {
	storage.LogStorage
	block   atomic.Bool
	path    storage.StreamPath
	entered chan struct{}
	release chan struct{}
}

// Append 在开启暂停后通知测试，再等待放行。
func (l *runningScanBlockingLogs) Append(ctx context.Context, path storage.StreamPath, streamID storage.StreamID, payloads ...json.RawMessage) ([]storage.LogEntryInfo, error) {
	// 1. 暂停 metadata，其他日志照常追加。
	if l.block.Load() && path == l.path && streamID == dao.MetadataStreamID {
		select {
		case l.entered <- struct{}{}:
		default:
		}
		select {
		case <-l.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return l.LogStorage.Append(ctx, path, streamID, payloads...)
}

// TestRuntimeIsIdle 检查根 Turn、子 Turn 和后台命令的忙闲状态。
func TestRuntimeIsIdle(t *testing.T) {
	// 1. 准备空闲查询断言，列出根任务、子任务和后台命令场景。
	requireIdle := func(t *testing.T, h *runtimeHarness, want bool) {
		t.Helper()
		got, err := h.rt.IsIdle(t.Context())
		if err != nil || got != want {
			t.Fatalf("IsIdle() = %v, %v, want %v", got, err, want)
		}
	}
	tests := []struct {
		name string
		run  func(t *testing.T, h *runtimeHarness)
	}{
		{
			name: "no_open_conversations",
			run: func(t *testing.T, h *runtimeHarness) {
				requireIdle(t, h, true)
			},
		},
		{
			name: "root_turn_running",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				infos := h.subscribeConversation(t, conv.ID)
				requireIdle(t, h, true)
				h.submit(t, conv.ID, "hold", "")
				request := h.model.receive(t)
				collectUntil(t, infos.Events, func(info runtime.SessionRuntimeInfo) bool { return info.State == session.SessionStateRunning })
				requireIdle(t, h, false)
				request.reply <- runtimeModelResponse{chunks: []string{"done"}}
				collectUntil(t, infos.Events, isTurnCompletedInfo)
				requireIdle(t, h, true)
			},
		},
		{
			// 根 Session 已空闲时，只要子 Agent 的 Session 还在执行 Turn 就不是空闲。
			name: "child_turn_running",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				infos := h.subscribeConversation(t, conv.ID)
				rootWatcher := h.subscribeSession(t, conv.ID, conv.RootSessionID)
				h.submit(t, conv.ID, "委派", "")
				h.model.receive(t).reply <- runtimeModelResponse{calls: []llmkit.ToolCall{{
					ID:        "spawn",
					Name:      "spawn_agent",
					Arguments: `{"task_name":"review","message":"check"}`,
				}}}
				root, child := h.model.receiveRootAndTask(t, "review")
				root.reply <- runtimeModelResponse{chunks: []string{"已委派"}}
				collectUntil(t, rootWatcher.Events, isTurnFinished)
				collectUntil(t, infos.Events, func(info runtime.SessionRuntimeInfo) bool {
					return info.Session.ParentSessionID == conv.RootSessionID && info.State == session.SessionStateRunning
				})
				requireIdle(t, h, false)
				child.reply <- runtimeModelResponse{chunks: []string{"子任务完成"}}
				collectUntil(t, infos.Events, func(info runtime.SessionRuntimeInfo) bool {
					return info.Session.ParentSessionID == conv.RootSessionID && isTurnCompletedInfo(info)
				})
				// 子任务结果作为通知触发根 Session 的下一轮，这一轮结束后才空闲。
				h.model.receive(t).reply <- runtimeModelResponse{chunks: []string{"已汇总"}}
				collectUntil(t, rootWatcher.Events, isTurnFinished)
				requireIdle(t, h, true)
			},
		},
		{
			// Turn 已结束、Session 运行阶段回到 idle，只要后台命令还在跑就不是空闲；命令停下后恢复空闲。
			name: "background_command_blocks_idle",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				infos := h.subscribeConversation(t, conv.ID)
				watcher := h.subscribeSession(t, conv.ID, conv.RootSessionID)
				started := h.runCommandToolRound(t, conv, watcher, "call-bash", "bash", `{"command":"sleep 30","wait_seconds":0}`)
				if started.ToolError != nil || started.Status != "running" || started.CommandID == "" {
					t.Fatalf("bash result = %+v, want a running command", started)
				}
				collectUntil(t, infos.Events, isTurnCompletedInfo)
				requireIdle(t, h, false)
				stopped := h.runCommandToolRound(t, conv, watcher, "call-stop", "stop_command", `{"command_id":"`+started.CommandID+`"}`)
				if stopped.ToolError != nil || stopped.StopReason != "stopped" {
					t.Fatalf("stop_command result = %+v, want stopped", stopped)
				}
				requireIdle(t, h, true)
			},
		},
		{
			name: "runtime_stopped",
			run: func(t *testing.T, h *runtimeHarness) {
				h.stop(t)
				if _, err := h.rt.IsIdle(t.Context()); !errors.Is(err, runtime.ErrRuntimeStopped) {
					t.Fatalf("IsIdle() error = %v, want %v", err, runtime.ErrRuntimeStopped)
				}
			},
		},
	}
	// 2. 运行根 Turn、子 Turn 和后台命令，检查各阶段的 IsIdle 结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, model := openRuntimeStore(t), newRuntimeModel(t)
			tt.run(t, startTestRuntimeWithOpt(t, store, model, newExecutorRuntimeOpt(store, model, newLocalExecutors(t)...)))
		})
	}
}

// newLocalExecutor 创建以临时目录为工作目录的本机 Executor。
func newLocalExecutor(t *testing.T, id string) *defs.ExecutorDefinition {
	t.Helper()
	// 1. 将 HOME 设为临时目录，并填入指定 ID、说明和独立工作目录。
	t.Setenv("HOME", t.TempDir())
	return &defs.ExecutorDefinition{
		ID:                 id,
		Kind:               defs.ExecutorKindLocal,
		Description:        id + " 测试环境",
		WorkspaceDirectory: t.TempDir(),
	}
}

// newLocalExecutors 创建只包含 local 的测试 Executor 列表。
func newLocalExecutors(t *testing.T) []*defs.ExecutorDefinition {
	t.Helper()
	// 1. 创建 local 并放入列表。
	return []*defs.ExecutorDefinition{newLocalExecutor(t, defs.LocalExecutorID)}
}

// newExecutorRuntimeOpt 创建带指定 Executor 定义的测试配置。
func newExecutorRuntimeOpt(store *runtimeStore, model *runtimeModel, executors ...*defs.ExecutorDefinition) *runtime.RuntimeOpt {
	// 1. 创建默认配置并替换 Executor 列表。
	opt := newTestRuntimeOpt(store, model)
	opt.Definitions.Executors = executors
	return opt
}

// unresponsiveSSHListener 是只接受 TCP 连接、从不回 SSH 握手的监听。
type unresponsiveSSHListener struct {
	// executor 是指向它的 SSH Executor 定义，带一把临时私钥。
	executor *defs.ExecutorDefinition
	// accepted 在第一次接受连接时关闭。
	accepted chan struct{}
}

// newUnresponsiveSSHExecutor 创建接受 TCP 但不回复 SSH 握手的测试 Executor。
func newUnresponsiveSSHExecutor(t *testing.T, id string) *unresponsiveSSHListener {
	t.Helper()
	// 1. 禁用 SSH agent，生成临时私钥并写入文件。
	t.Setenv("SSH_AUTH_SOCK", "")
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		t.Fatal(err)
	}
	identityFile := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(identityFile, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	// 2. 监听本机端口，创建指向该地址的 SSH Executor。
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := &unresponsiveSSHListener{
		executor: &defs.ExecutorDefinition{
			ID:                 id,
			Kind:               defs.ExecutorKindSSH,
			Description:        id + " 测试环境",
			WorkspaceDirectory: "/project",
			SSH: &defs.SSHExecutorDefinition{
				Address:      "test@" + listener.Addr().String(),
				IdentityFile: identityFile,
			},
		},
		accepted: make(chan struct{}),
	}
	// 3. 启动接收任务，登记连接，首次接受时关闭 accepted。
	var connections syncmap.SyncMap[net.Conn, struct{}]
	var acceptOnce sync.Once
	var tasks sync.WaitGroup
	tasks.Go(func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Store(connection, struct{}{})
			acceptOnce.Do(func() { close(l.accepted) })
		}
	})
	// 4. 测试结束时关闭监听、等待接收任务并关闭全部连接。
	t.Cleanup(func() {
		_ = listener.Close()
		tasks.Wait()
		connections.Range(func(connection net.Conn, _ struct{}) bool {
			_ = connection.Close()
			return true
		})
	})
	return l
}

// waitAccepted 等待测试监听接受第一条连接。
func (l *unresponsiveSSHListener) waitAccepted(t *testing.T) {
	t.Helper()
	// 1. 最多等待 5 秒，超时则结束测试。
	select {
	case <-l.accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("unresponsive SSH listener accepted no connection")
	}
}

// runtimeCommandResult 是命令工具结果里测试关心的字段。
type runtimeCommandResult struct {
	CommandID  string `json:"command_id"`
	Status     string `json:"status"`
	StopReason string `json:"stop_reason"`
	Output     string `json:"output"`
	Commands   []struct {
		CommandID string `json:"command_id"`
	} `json:"commands"`
	// ToolError 是这次工具调用的错误。
	ToolError *defs.ToolError `json:"-"`
}

// runCommandToolRound 运行一轮命令工具调用并读取保存后的工具结果。
func (h *runtimeHarness) runCommandToolRound(t *testing.T, conv defs.Conversation, watcher *runtime.SessionSubscription, callID, name, arguments string) runtimeCommandResult {
	t.Helper()
	// 1. 提交输入，让模型调用指定工具，再回复最终文本。
	h.submit(t, conv.ID, "run "+name, "")
	h.model.receive(t).reply <- runtimeModelResponse{calls: []llmkit.ToolCall{{
		ID:        callID,
		Name:      name,
		Arguments: arguments,
	}}}
	h.model.receive(t).reply <- runtimeModelResponse{chunks: []string{"done"}}
	// 2. 读到 Turn 结束，在保存事件中按 CallID 查找结果并解析，未找到则结束测试。
	for _, event := range collectUntil(t, watcher.Events, isTurnFinished) {
		saved, ok := getSessionEvent(event).(*session.SessionItemsSaved)
		if !ok {
			continue
		}
		for _, logged := range saved.Items {
			if logged.Item.Kind != defs.ItemToolResult || logged.Item.ToolResult.CallID != callID {
				continue
			}
			var result runtimeCommandResult
			if err := json.Unmarshal(logged.Item.ToolResult.Content, &result); err != nil {
				t.Fatalf("%s result %s: %v", name, logged.Item.ToolResult.Content, err)
			}
			result.ToolError = logged.Item.ToolResult.Error
			return result
		}
	}
	t.Fatalf("no saved result for %s", callID)
	return runtimeCommandResult{}
}

// completeRound 提交用户文本并完成一轮模型回复。
func (h *runtimeHarness) completeRound(t *testing.T, conv defs.Conversation, text string, wantLogCount int) defs.ItemID {
	t.Helper()
	// 1. 提交输入、回复测试文本，等待根日志达到指定条数，返回输入 ItemID。
	itemID := h.submit(t, conv.ID, text, "")
	h.model.receive(t).reply <- runtimeModelResponse{chunks: []string{"回复：" + text}}
	h.waitLog(t, conv.ID, conv.RootSessionID, wantLogCount)
	return itemID
}

// compactContextWhenIdle 等待根 Session 空闲后启动手动压缩。
func (h *runtimeHarness) compactContextWhenIdle(t *testing.T, conversationID defs.ConversationID) defs.TurnID {
	t.Helper()
	// 1. 在 5 秒内重试 ErrSessionBusy；成功返回压缩 TurnID，其余错误或超时结束测试。
	deadline := time.Now().Add(5 * time.Second)
	for {
		turnID, err := h.rt.CompactContext(t.Context(), &runtime.CompactContextRequest{ConversationID: conversationID})
		if err == nil && turnID != "" {
			return turnID
		}
		if !errors.Is(err, session.ErrSessionBusy) || time.Now().After(deadline) {
			t.Fatalf("CompactContext() = %q, %v", turnID, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestRuntimeCompactContext 检查手动压缩的摘要、用量和后续历史。
func TestRuntimeCompactContext(t *testing.T) {
	// 1. 列出手动压缩的摘要、用量和后续历史的测试场景。
	tests := []struct {
		name string
		run  func(t *testing.T, h *runtimeHarness)
	}{
		{
			name: "two_rounds_summarize_history_and_next_turn_writes_no_changes",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				infos := h.subscribeConversation(t, conv.ID)
				h.completeRound(t, conv, "第一轮", 2)
				secondID := h.completeRound(t, conv, "第二轮", 4)
				turnID := h.compactContextWhenIdle(t, conv.ID)
				summaryRequest := h.model.receive(t)
				if summaryRequest.body["tool_choice"] != "none" {
					t.Fatalf("summary request = %v, want tools disabled", summaryRequest.body)
				}
				summaryRequest.reply <- runtimeModelResponse{chunks: []string{"摘要内容"}}
				h.waitTurnStatus(t, conv.RootSessionID, turnID, defs.TurnStatusCompleted)

				logged := h.waitLog(t, conv.ID, conv.RootSessionID, 5)
				if len(logged) != 5 || logged[4].Item.Kind != defs.ItemContextSummary {
					t.Fatalf("log after compaction = %+v, want one new context summary", logged)
				}
				summary := logged[4].Item
				// 最近一组模型输出（第二轮回复）留在原文里，摘要覆盖到它之前的最后一条原始 Item。
				if summary.TurnID != turnID || summary.ContextSummary.Text != "摘要内容" || summary.ContextSummary.CoveredThroughItemID != secondID {
					t.Fatalf("summary = %+v %+v, want Turn %s covering %s", summary, summary.ContextSummary, turnID, secondID)
				}

				// 压缩的 Turn 不调用普通模型，由它自己按压缩后的上下文补发测量：占用是这次新测量，
				// 只含摘要和保留的第二轮回复，不是压缩前的测量加上之后保存的 Item，摘要和被压缩的原文都不重复计入。
				compacted := getLastInfo(collectUntil(t, infos.Events, func(info runtime.SessionRuntimeInfo) bool {
					return info.LastTurnID == turnID && isTurnCompletedInfo(info)
				}))
				usage, err := h.rt.GetSessionContextUsage(t.Context(), &runtime.GetSessionContextUsageRequest{
					ConversationID: conv.ID,
					SessionID:      conv.RootSessionID,
				})
				if err != nil {
					t.Fatal(err)
				}
				var itemIDs []defs.ItemID
				itemTokens := 0
				for _, item := range usage.Items {
					itemIDs = append(itemIDs, item.ItemID)
					itemTokens += item.Tokens
				}
				if wantIDs := []defs.ItemID{summary.ID, logged[3].Item.ID}; !slices.Equal(itemIDs, wantIDs) {
					t.Fatalf("context items after compaction = %v, want the summary and the retained reply %v", itemIDs, wantIDs)
				}
				if want := usage.SystemTokens + usage.ToolTokens + itemTokens; usage.NextRequest.Tokens != want || compacted.Context.Tokens != want {
					t.Fatalf("next request tokens = %d, info tokens = %d, want the fresh measurement %d", usage.NextRequest.Tokens, compacted.Context.Tokens, want)
				}

				h.submit(t, conv.ID, "第三轮", "")
				request := h.model.receive(t)
				payload, _ := json.Marshal(request.body["messages"])
				if !strings.Contains(string(payload), "摘要内容") || strings.Contains(string(payload), "第一轮") || !strings.Contains(string(payload), "第三轮") {
					t.Fatalf("next turn messages = %s", payload)
				}
				request.reply <- runtimeModelResponse{chunks: []string{"三"}}
				logged = h.waitLog(t, conv.ID, conv.RootSessionID, 7)
				for _, entry := range logged[5:] {
					if entry.Item.Kind != defs.ItemMessage {
						t.Fatalf("next turn wrote %q, want only messages", entry.Item.Kind)
					}
				}
			},
		},
		{
			name: "short_conversation_summarizes_through_last_item",
			run: func(t *testing.T, h *runtimeHarness) {
				// 模型 b 的窗口足够大，一轮对话全部落在保留部分里，手动压缩仍把全部历史压成摘要。
				conv, err := h.rt.CreateConversation(t.Context(), &runtime.CreateConversationRequest{
					AgentName: "assistant",
					Model: defs.ModelSelection{Ref: defs.ModelRef{
						ProviderID: "test",
						ModelID:    "b",
					}},
				})
				if err != nil {
					t.Fatal(err)
				}
				h.completeRound(t, conv, "只有一轮", 2)
				turnID := h.compactContextWhenIdle(t, conv.ID)
				h.model.receive(t).reply <- runtimeModelResponse{chunks: []string{"短摘要"}}
				h.waitTurnStatus(t, conv.RootSessionID, turnID, defs.TurnStatusCompleted)
				logged := h.waitLog(t, conv.ID, conv.RootSessionID, 3)
				summary := logged[2].Item
				if summary.Kind != defs.ItemContextSummary || summary.ContextSummary.CoveredThroughItemID != logged[1].Item.ID {
					t.Fatalf("summary = %+v, want covering the last original item %s", summary, logged[1].Item.ID)
				}
			},
		},
		{
			name: "busy_root_session",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				h.submit(t, conv.ID, "长任务", "")
				h.model.receive(t).reply <- runtimeModelResponse{
					chunks:  []string{"处理中"},
					release: make(chan struct{}),
				}
				turnID, err := h.rt.CompactContext(t.Context(), &runtime.CompactContextRequest{ConversationID: conv.ID})
				if !errors.Is(err, session.ErrSessionBusy) || turnID != "" {
					t.Fatalf("CompactContext() = %q, %v; want ErrSessionBusy", turnID, err)
				}
			},
		},
		{
			name: "missing_conversation_rejected",
			run: func(t *testing.T, h *runtimeHarness) {
				if _, err := h.rt.CompactContext(t.Context(), &runtime.CompactContextRequest{ConversationID: defs.NewConversationID()}); err == nil {
					t.Fatal("CompactContext() of a missing Conversation error = nil")
				}
			},
		},
	}
	// 2. 在空闲或忙碌时请求压缩，检查错误、摘要 Turn、用量和后续模型历史。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, startTestRuntime(t, openRuntimeStore(t), newRuntimeModel(t)))
		})
	}
}

// TestRuntimeListTurns 检查 Turn 分页、批量查询和必需参数。
func TestRuntimeListTurns(t *testing.T) {
	// 1. 列出 Turn 分页、批量查询和必需参数的测试场景。
	tests := []struct {
		name string
		run  func(t *testing.T, h *runtimeHarness)
	}{
		{
			name: "pages_in_descending_creation_order",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				for round, text := range []string{"一", "二", "三"} {
					h.completeRound(t, conv, text, 2*(round+1))
				}
				order := storage.Order{Direction: storage.DirectionDesc}
				first, err := h.rt.ListTurns(t.Context(), &runtime.ListTurnsRequest{
					ConversationID: conv.ID,
					SessionID:      conv.RootSessionID,
					Order:          order,
					Limit:          2,
				})
				if err != nil || len(first.Items) != 2 || first.Next == (dao.RecordCursor[defs.TurnID]{}) {
					t.Fatalf("first page = %+v, %v; want two Turns and a next cursor", first, err)
				}
				second, err := h.rt.ListTurns(t.Context(), &runtime.ListTurnsRequest{
					ConversationID: conv.ID,
					SessionID:      conv.RootSessionID,
					Order:          order,
					Cursor:         first.Next,
					Limit:          2,
				})
				if err != nil || len(second.Items) != 1 || second.Next != (dao.RecordCursor[defs.TurnID]{}) {
					t.Fatalf("second page = %+v, %v; want one Turn and no next cursor", second, err)
				}
				turns := append(first.Items, second.Items...)
				for index, turn := range turns {
					if turn.SessionID != conv.RootSessionID {
						t.Fatalf("Turn %d = %+v, want root Session %s", index, turn, conv.RootSessionID)
					}
					if index > 0 && turns[index-1].ID <= turn.ID {
						t.Fatalf("Turn IDs %s then %s are not in descending order", turns[index-1].ID, turn.ID)
					}
				}
			},
		},
		{
			name: "lists_without_opening_conversation",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				h.completeRound(t, conv, "一轮", 2)
				h.stop(t)
				restarted := startTestRuntime(t, h.store, h.model)
				if open, err := restarted.rt.IsConversationOpen(t.Context(), &runtime.GetConversationRequest{ConversationID: conv.ID}); err != nil || open {
					t.Fatalf("IsConversationOpen() = %v, %v, want false", open, err)
				}
				page, err := restarted.rt.ListTurns(t.Context(), &runtime.ListTurnsRequest{
					ConversationID: conv.ID,
					SessionID:      conv.RootSessionID,
					Order:          storage.Order{Direction: storage.DirectionAsc},
					Limit:          10,
				})
				if err != nil || len(page.Items) != 1 || page.Items[0].SessionID != conv.RootSessionID {
					t.Fatalf("ListTurns() = %+v, %v; want the root Session's Turn", page, err)
				}
			},
		},
		{
			name: "gets_turns_in_request_order_without_opening_conversation",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				for round, text := range []string{"一", "二", "三"} {
					h.completeRound(t, conv, text, 2*(round+1))
				}
				h.stop(t)
				restarted := startTestRuntime(t, h.store, h.model)
				page, err := restarted.rt.ListTurns(t.Context(), &runtime.ListTurnsRequest{
					ConversationID: conv.ID,
					SessionID:      conv.RootSessionID,
					Order:          storage.Order{Direction: storage.DirectionAsc},
					Limit:          10,
				})
				if err != nil || len(page.Items) != 3 {
					t.Fatalf("ListTurns() = %+v, %v; want three Turns", page, err)
				}
				first, third := page.Items[0], page.Items[2]
				got, err := restarted.rt.GetTurns(t.Context(), &runtime.GetTurnsRequest{
					ConversationID: conv.ID,
					SessionID:      conv.RootSessionID,
					TurnIDs:        []defs.TurnID{third.ID, "missing", first.ID},
				})
				if err != nil || len(got) != 2 || got[0].ID != third.ID || got[1].ID != first.ID {
					t.Fatalf("GetTurns() = %+v, %v; want the third then the first Turn", got, err)
				}
			},
		},
		{
			name: "requires_conversation_id",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				if _, err := h.rt.ListTurns(t.Context(), &runtime.ListTurnsRequest{
					SessionID: conv.RootSessionID,
					Limit:     10,
				}); err == nil {
					t.Fatal("ListTurns() without a conversation ID error = nil")
				}
				if _, err := h.rt.GetTurns(t.Context(), &runtime.GetTurnsRequest{
					SessionID: conv.RootSessionID,
					TurnIDs:   []defs.TurnID{"t1"},
				}); err == nil {
					t.Fatal("GetTurns() without a conversation ID error = nil")
				}
			},
		},
	}
	// 2. 运行多轮对话，检查 Turn 双向分页、批量 ID 顺序及必需参数错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, startTestRuntime(t, openRuntimeStore(t), newRuntimeModel(t)))
		})
	}
}

// TestRuntimeUnsavedConversation 检查普通新对话的保存和临时对话的保留与丢弃。
func TestRuntimeUnsavedConversation(t *testing.T) {
	// 1. 设置短空闲时长，准备普通和临时对话的使用场景。
	const idleTimeout = 200 * time.Millisecond
	requireAlive := func(t *testing.T, h *runtimeHarness, id defs.ConversationID) {
		t.Helper()
		if _, err := h.rt.GetConversation(t.Context(), &runtime.GetConversationRequest{ConversationID: id}); err != nil {
			t.Fatalf("GetConversation() = %v; want the unsaved conversation kept", err)
		}
	}
	// requireSaved 要求共用库里有（isSaved）或没有对话的记录、根 Session 记录和 metadata Stream，对话列表里也相应有或没有它。
	requireSaved := func(t *testing.T, h *runtimeHarness, conv defs.Conversation, isSaved bool) {
		t.Helper()
		page, err := h.rt.ListConversations(t.Context(), &dao.ListConversationsRequest{
			Order: storage.Order{Direction: storage.DirectionAsc},
			Limit: 10,
		})
		if err != nil || slices.ContainsFunc(page.Items, func(entry dao.ConversationListEntry) bool { return entry.Conversation.ID == conv.ID }) != isSaved {
			t.Fatalf("ListConversations() = %+v, %v; want listed = %v", page.Items, err, isSaved)
		}
		if _, found, err := h.store.query.GetConversation(t.Context(), conv.ID); err != nil || found != isSaved {
			t.Fatalf("shared GetConversation() found = %v, %v; want %v", found, err, isSaved)
		}
		if _, found, err := h.store.query.GetSession(t.Context(), conv.ID, conv.RootSessionID); err != nil || found != isSaved {
			t.Fatalf("shared GetSession(root) found = %v, %v; want %v", found, err, isSaved)
		}
		paths, err := h.store.opt.Logs.GetStreamPaths(t.Context(), dao.MetadataStreamID)
		if err != nil || slices.Contains(paths, storage.StreamPath(conv.ID)) != isSaved {
			t.Fatalf("shared metadata paths = %v, %v; want contains %s = %v", paths, err, conv.ID, isSaved)
		}
	}
	tests := []struct {
		name string
		// ephemeral 是否建临时对话？否则建普通对话。
		ephemeral bool
		// idleTimeout 是没保存的对话的丢弃等待时长。
		idleTimeout time.Duration
		// isSubscribed 是否建好就订阅运行信息？订阅期间保留对话。
		isSubscribed bool
		run          func(t *testing.T, h *runtimeHarness, conv defs.Conversation, statuses *runtime.ConversationSubscription, unsubscribe context.CancelFunc)
	}{
		{
			name:         "stored_apart_from_shared_dao",
			ephemeral:    true,
			idleTimeout:  idleTimeout,
			isSubscribed: true,
			run: func(t *testing.T, h *runtimeHarness, conv defs.Conversation, statuses *runtime.ConversationSubscription, _ context.CancelFunc) {
				h.submit(t, conv.ID, "起个标题", "")
				h.model.receive(t).reply <- runtimeModelResponse{chunks: []string{`{"title":"修复登录"}`}}
				collectUntil(t, statuses.Events, isTurnCompletedInfo)
				page, err := h.rt.ListConversations(t.Context(), &dao.ListConversationsRequest{
					Order: storage.Order{Direction: storage.DirectionAsc},
					Limit: 10,
				})
				if err != nil || slices.ContainsFunc(page.Items, func(entry dao.ConversationListEntry) bool { return entry.Conversation.ID == conv.ID }) {
					t.Fatalf("ListConversations() = %+v, %v; want no ephemeral conversation", page.Items, err)
				}
				requireAlive(t, h, conv.ID)
				h.waitLog(t, conv.ID, conv.RootSessionID, 2)
				if _, found, err := h.store.query.GetConversation(t.Context(), conv.ID); err != nil || found {
					t.Fatalf("shared GetConversation() found = %v, %v; want not found", found, err)
				}
				shared, err := h.store.query.ListItems(t.Context(), dao.ListItemsRequest{
					ConversationID: conv.ID,
					SessionID:      conv.RootSessionID,
					Order:          storage.DirectionAsc,
					Limit:          10,
				})
				if err != nil || len(shared.Items) != 0 {
					t.Fatalf("shared ListItems() = %+v, %v; want an empty root Session log", shared.Items, err)
				}
			},
		},
		{
			name:         "kept_while_subscribed",
			ephemeral:    true,
			idleTimeout:  idleTimeout,
			isSubscribed: true,
			run: func(t *testing.T, h *runtimeHarness, conv defs.Conversation, _ *runtime.ConversationSubscription, unsubscribe context.CancelFunc) {
				time.Sleep(5 * idleTimeout)
				requireAlive(t, h, conv.ID)
				unsubscribe()
				waitConversationDiscarded(t, h, conv.ID)
			},
		},
		{
			name:         "kept_while_session_subscribed",
			ephemeral:    true,
			idleTimeout:  idleTimeout,
			isSubscribed: true,
			run: func(t *testing.T, h *runtimeHarness, conv defs.Conversation, _ *runtime.ConversationSubscription, unsubscribe context.CancelFunc) {
				sessionCtx, unsubscribeSession := context.WithCancel(t.Context())
				defer unsubscribeSession()
				if _, err := h.rt.SubscribeSession(sessionCtx, &runtime.SubscribeSessionRequest{
					ConversationID: conv.ID,
					SessionID:      conv.RootSessionID,
				}); err != nil {
					t.Fatal(err)
				}
				unsubscribe()
				time.Sleep(5 * idleTimeout)
				requireAlive(t, h, conv.ID)
				unsubscribeSession()
				waitConversationDiscarded(t, h, conv.ID)
			},
		},
		{
			// 标题 Turn 超过空闲时长仍在运行，没有订阅者时也保留对话。
			name:         "kept_while_turn_running",
			ephemeral:    true,
			idleTimeout:  idleTimeout,
			isSubscribed: true,
			run: func(t *testing.T, h *runtimeHarness, conv defs.Conversation, statuses *runtime.ConversationSubscription, unsubscribe context.CancelFunc) {
				release := make(chan struct{})
				h.submit(t, conv.ID, "慢慢起标题", "")
				h.model.receive(t).reply <- runtimeModelResponse{
					chunks:  []string{`{"title":`, `"慢"}`},
					release: release,
				}
				collectUntil(t, statuses.Events, func(info runtime.SessionRuntimeInfo) bool { return info.State == session.SessionStateRunning })
				unsubscribe()
				time.Sleep(5 * idleTimeout)
				requireAlive(t, h, conv.ID)
				close(release)
				waitConversationDiscarded(t, h, conv.ID)
			},
		},
		{
			name:         "new_conversation_is_not_saved_before_input",
			idleTimeout:  idleTimeout,
			isSubscribed: true,
			run: func(t *testing.T, h *runtimeHarness, conv defs.Conversation, _ *runtime.ConversationSubscription, _ context.CancelFunc) {
				requireSaved(t, h, conv, false)
				if _, err := os.Stat(filepath.Join(h.store.root, "conversations", string(conv.ID))); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("conversation directory stat error = %v, want not exist", err)
				}
				h.subscribeSession(t, conv.ID, conv.RootSessionID)
				if _, err := h.rt.GetSessionDetails(t.Context(), &runtime.GetSessionDetailsRequest{
					ConversationID: conv.ID,
					SessionID:      conv.RootSessionID,
				}); err != nil {
					t.Fatal(err)
				}
				requireAlive(t, h, conv.ID)
			},
		},
		{
			// 设置更新后旧订阅关闭，本例使用较长空闲时长再提交输入。
			name:        "first_input_saves_settings_made_before_it",
			idleTimeout: 5 * time.Second,
			run: func(t *testing.T, h *runtimeHarness, conv defs.Conversation, _ *runtime.ConversationSubscription, _ context.CancelFunc) {
				model := defs.ModelSelection{
					Ref: defs.ModelRef{
						ProviderID: "test",
						ModelID:    "b",
					},
					ReasoningEffort: "high",
				}
				if _, err := h.rt.SetSessionModel(t.Context(), &runtime.SetSessionModelRequest{
					ConversationID: conv.ID,
					SessionID:      conv.RootSessionID,
					Model:          model,
				}); err != nil {
					t.Fatal(err)
				}
				if err := h.rt.SetConversationTitle(t.Context(), &runtime.SetConversationTitleRequest{
					ConversationID: conv.ID,
					Title:          "先起的标题",
				}); err != nil {
					t.Fatal(err)
				}
				settings := defs.ExecutorSettings{
					DefaultExecutorID:    "second",
					WorkspaceDirectories: map[string]string{"second": t.TempDir()},
				}
				applied, err := h.setExecutorSettings(t, &runtime.SetConversationExecutorSettingsRequest{
					ConversationID:   conv.ID,
					ExecutorSettings: settings,
				})
				if err != nil {
					t.Fatal(err)
				}
				// 换上的设置补上了 local 目录。
				settings = applied.ExecutorSettings
				requireSaved(t, h, conv, false)
				h.submit(t, conv.ID, "第一句", "")
				requireSaved(t, h, conv, true)
				request := h.model.receive(t)
				if request.body["model"] != "model-b" {
					t.Fatalf("first request model = %v, want model-b", request.body["model"])
				}
				request.reply <- runtimeModelResponse{chunks: []string{"好"}}
				h.waitLog(t, conv.ID, conv.RootSessionID, 2)
				saved, _, err := h.store.query.GetConversation(t.Context(), conv.ID)
				if err != nil || saved.Title != "先起的标题" || !reflect.DeepEqual(saved.ExecutorSettings, settings) {
					t.Fatalf("shared Conversation = %+v, %v; want the title and settings set before the first input", saved, err)
				}
				root, _, err := h.store.query.GetSession(t.Context(), conv.ID, conv.RootSessionID)
				if err != nil || root.Model != model {
					t.Fatalf("shared root Session model = %+v, %v; want %+v", root.Model, err, model)
				}
			},
		},
		{
			name:         "compact_as_first_action_saves_it",
			idleTimeout:  idleTimeout,
			isSubscribed: true,
			run: func(t *testing.T, h *runtimeHarness, conv defs.Conversation, _ *runtime.ConversationSubscription, _ context.CancelFunc) {
				turnID, err := h.rt.CompactContext(t.Context(), &runtime.CompactContextRequest{ConversationID: conv.ID})
				if err != nil || turnID == "" {
					t.Fatalf("CompactContext() = %q, %v", turnID, err)
				}
				requireSaved(t, h, conv, true)
				h.waitTurnStatus(t, conv.RootSessionID, turnID, defs.TurnStatusCompleted)
			},
		},
		{
			name:        "unused_conversation_is_discarded",
			idleTimeout: 50 * time.Millisecond,
			run: func(t *testing.T, h *runtimeHarness, conv defs.Conversation, _ *runtime.ConversationSubscription, _ context.CancelFunc) {
				waitConversationDiscarded(t, h, conv.ID)
				requireSaved(t, h, conv, false)
			},
		},
	}
	// 2. 创建普通或临时对话，检查首条输入和压缩前后的保存、列表可见性及空闲丢弃。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, model := openRuntimeStore(t), newRuntimeModel(t)
			opt := newExecutorRuntimeOpt(store, model, newLocalExecutor(t, defs.LocalExecutorID), newLocalExecutor(t, "second"))
			h := startTestRuntimeWithOpt(t, store, model, opt.WithUnsavedConversationIdleTimeout(tt.idleTimeout))
			conv, err := h.rt.CreateConversation(t.Context(), &runtime.CreateConversationRequest{
				AgentName: "assistant",
				Ephemeral: tt.ephemeral,
			})
			if err != nil {
				t.Fatal(err)
			}
			subscriptionCtx, unsubscribe := context.WithCancel(t.Context())
			defer unsubscribe()
			var statuses *runtime.ConversationSubscription
			if tt.isSubscribed {
				if statuses, err = h.rt.SubscribeConversation(subscriptionCtx, &runtime.SubscribeConversationRequest{ConversationID: conv.ID}); err != nil {
					t.Fatal(err)
				}
			}
			tt.run(t, h, conv, statuses, unsubscribe)
		})
	}
}

// waitConversationDiscarded 等待未保存对话被丢弃。
func waitConversationDiscarded(t *testing.T, h *runtimeHarness, id defs.ConversationID) {
	t.Helper()
	// 1. 在 5 秒内轮询到 ErrConversationNotFound，超时则结束测试。
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := h.rt.GetConversation(t.Context(), &runtime.GetConversationRequest{ConversationID: id})
		if errors.Is(err, runtime.ErrConversationNotFound) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("GetConversation() error = %v, want ErrConversationNotFound", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestRuntimeConversationManagement 检查分叉、归档、删除和来源删除后的续聊。
func TestRuntimeConversationManagement(t *testing.T) {
	// 1. 准备分叉时换用的模型 b，列出分叉、归档和删除场景。
	modelB := defs.ModelSelection{
		Ref: defs.ModelRef{
			ProviderID: "test",
			ModelID:    "b",
		},
		ReasoningEffort: "high",
	}
	tests := []struct {
		name string
		run  func(t *testing.T, h *runtimeHarness)
	}{
		{
			name: "fork_and_open",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				watcher := h.subscribeSession(t, conv.ID, conv.RootSessionID)
				h.runRootTurn(t, conv, watcher, "第一轮", runtimeModelResponse{chunks: []string{"回复一"}})
				h.runRootTurn(t, conv, watcher, "第二轮", runtimeModelResponse{chunks: []string{"回复二"}})
				forked, err := h.rt.ForkConversation(t.Context(), &runtime.ForkConversationRequest{ConversationID: conv.ID})
				if err != nil || forked.ID == conv.ID || forked.ForkedFromID != conv.ID || forked.MessageCount != 4 {
					t.Fatalf("ForkConversation() = %+v, %v; want a new open conversation forked from %s with 4 messages", forked, err, conv.ID)
				}
				if isOpen, err := h.rt.IsConversationOpen(t.Context(), &runtime.GetConversationRequest{ConversationID: forked.ID}); err != nil || !isOpen {
					t.Fatalf("IsConversationOpen(forked) = %v, %v; want true", isOpen, err)
				}
				if !h.isConversationListed(t, forked.ID, false) {
					t.Fatal("forked conversation is not listed")
				}
				source, copied := h.listRootItems(t, conv), h.listRootItems(t, forked)
				if !slices.EqualFunc(source, copied, func(a, b dao.LoggedItem) bool { return a.Ordinal == b.Ordinal && a.Item.ID == b.Item.ID }) {
					t.Fatalf("forked items = %+v, want the source items %+v", copied, source)
				}
				h.submit(t, forked.ID, "第三轮", "")
				request := h.model.receive(t)
				payload, _ := json.Marshal(request.body["messages"])
				for _, text := range []string{"第一轮", "回复一", "第二轮", "回复二", "第三轮"} {
					if !strings.Contains(string(payload), text) {
						t.Fatalf("forked request messages = %s, want %q", payload, text)
					}
				}
				request.reply <- runtimeModelResponse{chunks: []string{"回复三"}}
				h.waitLog(t, forked.ID, forked.RootSessionID, 6)
			},
		},
		{
			// 来源第 3 轮还在跑：在它之前分叉不受影响，来源这一轮之后照常完成。
			name: "fork_before_running_turn",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				watcher := h.subscribeSession(t, conv.ID, conv.RootSessionID)
				first := h.runRootTurn(t, conv, watcher, "第一轮", runtimeModelResponse{chunks: []string{"回复一"}})
				second := h.runRootTurn(t, conv, watcher, "第二轮", runtimeModelResponse{chunks: []string{"回复二"}})
				h.submit(t, conv.ID, "第三轮", "")
				held := h.model.receive(t)
				turnIDs := h.listRootTurnIDs(t, conv)
				if len(turnIDs) != 3 {
					t.Fatalf("source Turns = %v, want 3", turnIDs)
				}
				third := turnIDs[2]
				forked, err := h.rt.ForkConversation(t.Context(), &runtime.ForkConversationRequest{
					ConversationID: conv.ID,
					BeforeTurnID:   third,
				})
				if err != nil {
					t.Fatal(err)
				}
				if got := h.listRootTurnIDs(t, forked); !slices.Equal(got, []defs.TurnID{first, second}) {
					t.Fatalf("forked Turns = %v, want %v", got, []defs.TurnID{first, second})
				}
				items := h.listRootItems(t, forked)
				if len(items) != 4 || slices.ContainsFunc(items, func(logged dao.LoggedItem) bool { return logged.Item.TurnID == third }) {
					t.Fatalf("forked items = %+v, want the 4 items of the first two Turns", items)
				}
				held.reply <- runtimeModelResponse{chunks: []string{"回复三"}}
				h.waitTurnStatus(t, conv.RootSessionID, third, defs.TurnStatusCompleted)
			},
		},
		{
			name: "fork_first_turn",
			run: func(t *testing.T, h *runtimeHarness) {
				conv, err := h.rt.CreateConversation(t.Context(), &runtime.CreateConversationRequest{
					AgentName: "assistant",
					Model:     modelB,
				})
				if err != nil {
					t.Fatal(err)
				}
				watcher := h.subscribeSession(t, conv.ID, conv.RootSessionID)
				first := h.runRootTurn(t, conv, watcher, "第一轮", runtimeModelResponse{chunks: []string{"回复一"}})
				forked, err := h.rt.ForkConversation(t.Context(), &runtime.ForkConversationRequest{
					ConversationID: conv.ID,
					BeforeTurnID:   first,
				})
				if err != nil || forked.ID == conv.ID || forked.ForkedFromID != "" {
					t.Fatalf("ForkConversation() = %+v, %v; want a new conversation", forked, err)
				}
				if isOpen, err := h.rt.IsConversationOpen(t.Context(), &runtime.GetConversationRequest{ConversationID: forked.ID}); err != nil || !isOpen {
					t.Fatalf("IsConversationOpen(forked) = %v, %v; want true", isOpen, err)
				}
				if h.isConversationListed(t, forked.ID, false) {
					t.Fatal("conversation forked before the first Turn is listed, want unsaved")
				}
				sourceRoot, _, err := h.store.query.GetSession(t.Context(), conv.ID, conv.RootSessionID)
				if err != nil {
					t.Fatal(err)
				}
				sessions, err := h.rt.ListSessions(t.Context(), &dao.ListSessionsRequest{
					ConversationID: forked.ID,
					Order:          storage.Order{Direction: storage.DirectionAsc},
					Limit:          10,
				})
				if err != nil || len(sessions.Items) != 1 {
					t.Fatalf("forked Sessions = %+v, %v; want only the root", sessions.Items, err)
				}
				if root := sessions.Items[0]; root.AgentName != sourceRoot.AgentName || root.Model != modelB || sourceRoot.Model != modelB {
					t.Fatalf("forked root = %+v, source root = %+v; want the same agent and model %+v", root, sourceRoot, modelB)
				}
			},
		},
		{
			name: "fork_model_change",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				watcher := h.subscribeSession(t, conv.ID, conv.RootSessionID)
				h.runRootTurn(t, conv, watcher, "第一轮", runtimeModelResponse{
					reasoning: "旧模型的推理",
					chunks:    []string{"回复一"},
				})
				forked, err := h.rt.ForkConversation(t.Context(), &runtime.ForkConversationRequest{
					ConversationID: conv.ID,
					Model:          modelB,
				})
				if err != nil {
					t.Fatal(err)
				}
				h.submit(t, forked.ID, "换模型之后", "")
				request := h.model.receive(t)
				payload, _ := json.Marshal(request.body["messages"])
				if request.body["model"] != "model-b" || strings.Contains(string(payload), "旧模型的推理") ||
					!strings.Contains(string(payload), "回复一") || !strings.Contains(string(payload), "换模型之后") {
					t.Fatalf("forked request model = %v, messages = %s; want model-b with the history but without the old reasoning", request.body["model"], payload)
				}
				items := h.listRootItems(t, forked)
				index := slices.IndexFunc(items, func(logged dao.LoggedItem) bool { return logged.Item.Kind == defs.ItemModelChange })
				if want := (defs.ModelChange{
					PreviousModel: testModelRef,
					Model:         modelB.Ref,
				}); index < 0 || *items[index].Item.ModelChange != want {
					t.Fatalf("forked items = %+v, want a model change %+v", items, want)
				}
				request.reply <- runtimeModelResponse{chunks: []string{"好"}}
			},
		},
		{
			name: "archive",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				h.completeRound(t, conv, "第一轮", 2)
				requireConversation(t, h, conv.ID, func(record defs.Conversation) bool { return record.MessageCount == 2 })
				// 重启后对话没在运行，归档直接保存。
				h.stop(t)
				h = startTestRuntime(t, h.store, h.model)
				archived, err := h.rt.SetConversationArchived(t.Context(), &runtime.SetConversationArchivedRequest{
					ConversationID: conv.ID,
					Archived:       true,
				})
				if err != nil || archived.ArchivedAt == 0 || archived.MessageCount != 2 {
					t.Fatalf("SetConversationArchived(true) = %+v, %v; want archived with 2 messages", archived, err)
				}
				h.requireListed(t, conv.ID, true)
				lines := h.countMetadataLines(t, conv.ID)
				again, err := h.rt.SetConversationArchived(t.Context(), &runtime.SetConversationArchivedRequest{
					ConversationID: conv.ID,
					Archived:       true,
				})
				if err != nil || again.ArchivedAt != archived.ArchivedAt {
					t.Fatalf("SetConversationArchived(true) again = %+v, %v; want ArchivedAt %d unchanged", again, err, archived.ArchivedAt)
				}
				if got := h.countMetadataLines(t, conv.ID); got != lines {
					t.Fatalf("metadata lines after archiving twice = %d, want %d", got, lines)
				}
				if _, err := h.rt.OpenConversation(t.Context(), &runtime.OpenConversationRequest{ConversationID: conv.ID}); err != nil {
					t.Fatal(err)
				}
				watcher := h.subscribeSession(t, conv.ID, conv.RootSessionID)
				h.runRootTurn(t, conv, watcher, "归档之后", runtimeModelResponse{chunks: []string{"照常回复"}})
				requireConversation(t, h, conv.ID, func(record defs.Conversation) bool {
					return record.MessageCount == 4 && record.ArchivedAt == archived.ArchivedAt
				})
				h.requireListed(t, conv.ID, true)
				// 对话打开着时取消归档由它自己的事件循环保存。
				restored, err := h.rt.SetConversationArchived(t.Context(), &runtime.SetConversationArchivedRequest{ConversationID: conv.ID})
				if err != nil || restored.ArchivedAt != 0 || restored.MessageCount != 4 {
					t.Fatalf("SetConversationArchived(false) = %+v, %v; want unarchived with 4 messages", restored, err)
				}
				h.requireListed(t, conv.ID, false)
			},
		},
		{
			// 打开着的对话保存记录后，内存里的最后写入时间跟着刷新：归档回复和再次打开返回的记录都不是登记时的旧值。
			name: "archive_open_conversation_refreshes_updated_at",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				h.completeRound(t, conv, "第一轮", 2)
				h.stop(t)
				h = startTestRuntime(t, h.store, h.model)
				opened, err := h.rt.OpenConversation(t.Context(), &runtime.OpenConversationRequest{ConversationID: conv.ID})
				if err != nil {
					t.Fatal(err)
				}
				// 时间戳精确到毫秒：等到下一毫秒，保存后的最后写入时间一定晚于登记时读到的。
				for time.Now().UnixMilli() <= opened.UpdatedAt {
					time.Sleep(time.Millisecond)
				}
				archived, err := h.rt.SetConversationArchived(t.Context(), &runtime.SetConversationArchivedRequest{
					ConversationID: conv.ID,
					Archived:       true,
				})
				if err != nil {
					t.Fatal(err)
				}
				saved, err := h.rt.GetConversation(t.Context(), &runtime.GetConversationRequest{ConversationID: conv.ID})
				if err != nil {
					t.Fatal(err)
				}
				if archived.UpdatedAt <= opened.UpdatedAt || archived.UpdatedAt < saved.UpdatedAt {
					t.Fatalf("archived UpdatedAt = %d; want after the opened %d and not before the stored %d", archived.UpdatedAt, opened.UpdatedAt, saved.UpdatedAt)
				}
				again, err := h.rt.OpenConversation(t.Context(), &runtime.OpenConversationRequest{ConversationID: conv.ID})
				if err != nil || again.UpdatedAt != archived.UpdatedAt {
					t.Fatalf("OpenConversation() on the open conversation = %+v, %v; want UpdatedAt %d", again, err, archived.UpdatedAt)
				}
			},
		},
		{
			// 归档和这一轮结束时的消息计数都由对话自己的事件循环读改写，互不覆盖。
			name: "archive_while_running",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				watcher := h.subscribeSession(t, conv.ID, conv.RootSessionID)
				h.runRootTurn(t, conv, watcher, "第一轮", runtimeModelResponse{chunks: []string{"回复一"}})
				h.submit(t, conv.ID, "第二轮", "")
				held := h.model.receive(t)
				archived, err := h.rt.SetConversationArchived(t.Context(), &runtime.SetConversationArchivedRequest{
					ConversationID: conv.ID,
					Archived:       true,
				})
				if err != nil || archived.ArchivedAt == 0 || archived.MessageCount != 2 {
					t.Fatalf("SetConversationArchived(true) = %+v, %v; want archived with 2 messages", archived, err)
				}
				held.reply <- runtimeModelResponse{chunks: []string{"回复二"}}
				collectUntil(t, watcher.Events, isTurnFinished)
				saved, err := h.rt.GetConversation(t.Context(), &runtime.GetConversationRequest{ConversationID: conv.ID})
				if err != nil || saved.ArchivedAt != archived.ArchivedAt || saved.MessageCount != 4 {
					t.Fatalf("Conversation after the Turn = %+v, %v; want ArchivedAt %d and 4 messages", saved, err, archived.ArchivedAt)
				}
				h.requireListed(t, conv.ID, true)
			},
		},
		{
			name: "delete_busy",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				statuses := h.subscribeConversation(t, conv.ID)
				h.submit(t, conv.ID, "长任务", "")
				held := h.model.receive(t)
				collectUntil(t, statuses.Events, func(info runtime.SessionRuntimeInfo) bool { return info.State == session.SessionStateRunning })
				if err := h.rt.DeleteConversation(t.Context(), &runtime.DeleteConversationRequest{ConversationID: conv.ID}); !errors.Is(err, session.ErrSessionBusy) {
					t.Fatalf("DeleteConversation() error = %v, want %v", err, session.ErrSessionBusy)
				}
				if _, err := h.rt.GetConversation(t.Context(), &runtime.GetConversationRequest{ConversationID: conv.ID}); err != nil {
					t.Fatalf("GetConversation() after a busy delete = %v, want the conversation kept", err)
				}
				// 实例和订阅都没动：这一轮照常完成。
				held.reply <- runtimeModelResponse{chunks: []string{"完成"}}
				collectUntil(t, statuses.Events, isTurnCompletedInfo)
			},
		},
		{
			name: "delete_while_subscribed",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				watcher := h.subscribeSession(t, conv.ID, conv.RootSessionID)
				h.runRootTurn(t, conv, watcher, "第一轮", runtimeModelResponse{chunks: []string{"回复一"}})
				if err := h.rt.DeleteConversation(t.Context(), &runtime.DeleteConversationRequest{ConversationID: conv.ID}); err != nil {
					t.Fatal(err)
				}
				waitEventsClosed(t, watcher.Events)
				if reason := watcher.GetCloseReason(); reason == nil || !strings.Contains(reason.Error(), "has stopped") {
					t.Fatalf("subscription close reason = %v, want the conversation stopped", reason)
				}
				if _, err := h.rt.OpenConversation(t.Context(), &runtime.OpenConversationRequest{ConversationID: conv.ID}); !errors.Is(err, runtime.ErrConversationNotFound) {
					t.Fatalf("OpenConversation() after delete error = %v, want %v", err, runtime.ErrConversationNotFound)
				}
				if _, err := os.Stat(filepath.Join(h.store.root, "conversations", string(conv.ID))); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("conversation directory stat error = %v, want not exist", err)
				}
				if h.isConversationListed(t, conv.ID, false) {
					t.Fatal("deleted conversation is still listed")
				}
			},
		},
		{
			// 来源压缩过，分叉只复制摘要之后的部分；删掉来源后分叉出的对话照常打开和继续，历史从复制的部分开始。
			name: "delete_source_keeps_fork",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				watcher := h.subscribeSession(t, conv.ID, conv.RootSessionID)
				h.runRootTurn(t, conv, watcher, "第一轮", runtimeModelResponse{chunks: []string{"回复一"}})
				h.runRootTurn(t, conv, watcher, "第二轮", runtimeModelResponse{chunks: []string{"回复二"}})
				h.compactContextWhenIdle(t, conv.ID)
				h.model.receive(t).reply <- runtimeModelResponse{chunks: []string{"摘要内容"}}
				collectUntil(t, watcher.Events, isTurnFinished)
				h.runRootTurn(t, conv, watcher, "第三轮", runtimeModelResponse{chunks: []string{"回复三"}})
				forked, err := h.rt.ForkConversation(t.Context(), &runtime.ForkConversationRequest{ConversationID: conv.ID})
				if err != nil {
					t.Fatal(err)
				}
				marker := h.readFirstLogItem(t, forked)
				if marker.Kind != defs.ItemSessionFork || marker.SessionFork.SourceConversationID != conv.ID {
					t.Fatalf("forked first log line = %+v, want a fork marker pointing at %s", marker, conv.ID)
				}
				if err := h.rt.DeleteConversation(t.Context(), &runtime.DeleteConversationRequest{ConversationID: conv.ID}); err != nil {
					t.Fatal(err)
				}
				// 重启后从存储打开，不沿用删除前的运行实例。
				h.stop(t)
				h = startTestRuntime(t, h.store, h.model)
				if _, err := h.rt.OpenConversation(t.Context(), &runtime.OpenConversationRequest{ConversationID: forked.ID}); err != nil {
					t.Fatal(err)
				}
				if items := h.listRootItems(t, forked); len(items) == 0 || int64(items[0].Ordinal) != marker.SessionFork.SourceEndOrdinal {
					t.Fatalf("forked items after deleting the source = %+v, want starting at %d", items, marker.SessionFork.SourceEndOrdinal)
				}
				forkedWatcher := h.subscribeSession(t, forked.ID, forked.RootSessionID)
				h.submit(t, forked.ID, "删除来源之后", "")
				request := h.model.receive(t)
				payload, _ := json.Marshal(request.body["messages"])
				if !strings.Contains(string(payload), "摘要内容") || !strings.Contains(string(payload), "删除来源之后") {
					t.Fatalf("forked request messages = %s, want the summary and the new input", payload)
				}
				request.reply <- runtimeModelResponse{chunks: []string{"好"}}
				collectUntil(t, forkedWatcher.Events, isTurnFinished)
			},
		},
		{
			// 还没保存的新对话和临时对话不在共用库里，分叉、归档、删除都按对话不存在处理，实例不受影响。
			name: "unsaved_conversations_rejected",
			run: func(t *testing.T, h *runtimeHarness) {
				for _, isEphemeral := range []bool{false, true} {
					conv, err := h.rt.CreateConversation(t.Context(), &runtime.CreateConversationRequest{
						AgentName: "assistant",
						Ephemeral: isEphemeral,
					})
					if err != nil {
						t.Fatal(err)
					}
					if _, err := h.rt.ForkConversation(t.Context(), &runtime.ForkConversationRequest{ConversationID: conv.ID}); !errors.Is(err, runtime.ErrConversationNotFound) {
						t.Fatalf("ForkConversation(ephemeral=%v) error = %v, want %v", isEphemeral, err, runtime.ErrConversationNotFound)
					}
					if _, err := h.rt.SetConversationArchived(t.Context(), &runtime.SetConversationArchivedRequest{
						ConversationID: conv.ID,
						Archived:       true,
					}); !errors.Is(err, runtime.ErrConversationNotFound) {
						t.Fatalf("SetConversationArchived(ephemeral=%v) error = %v, want %v", isEphemeral, err, runtime.ErrConversationNotFound)
					}
					if err := h.rt.DeleteConversation(t.Context(), &runtime.DeleteConversationRequest{ConversationID: conv.ID}); !errors.Is(err, runtime.ErrConversationNotFound) {
						t.Fatalf("DeleteConversation(ephemeral=%v) error = %v, want %v", isEphemeral, err, runtime.ErrConversationNotFound)
					}
					if isOpen, err := h.rt.IsConversationOpen(t.Context(), &runtime.GetConversationRequest{ConversationID: conv.ID}); err != nil || !isOpen {
						t.Fatalf("IsConversationOpen(ephemeral=%v) = %v, %v; want still open", isEphemeral, isOpen, err)
					}
					if h.isConversationListed(t, conv.ID, true) {
						t.Fatalf("unsaved conversation (ephemeral=%v) is in the archived list", isEphemeral)
					}
				}
			},
		},
	}
	// 2. 执行分叉、模型切换、归档和删除，检查复制历史、持久记录、订阅关闭及来源删除后的续聊。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, startTestRuntime(t, openRuntimeStore(t), newRuntimeModel(t)))
		})
	}
}

// runRootTurn 运行一轮输入和指定回复，等待结束后返回 TurnID。
func (h *runtimeHarness) runRootTurn(t *testing.T, conv defs.Conversation, watcher *runtime.SessionSubscription, text string, response runtimeModelResponse) defs.TurnID {
	t.Helper()
	// 1. 提交输入并让模型按指定内容回复。
	h.submit(t, conv.ID, text, "")
	h.model.receive(t).reply <- response
	// 2. 等待根 Session 的 Turn 结束；此时终态和消息数已保存，返回 TurnID。
	events := collectUntil(t, watcher.Events, isTurnFinished)
	return getSessionEvent(events[len(events)-1]).(*session.SessionTurnFinished).TurnID
}

// listRootItems 正序读取根 Session 的一页日志。
func (h *runtimeHarness) listRootItems(t *testing.T, conv defs.Conversation) []dao.LoggedItem {
	t.Helper()
	// 1. 正序查询根 Session 最多 100 条日志，失败时结束测试，成功时返回本页 Item。
	page, err := h.rt.ListItems(t.Context(), &runtime.ListItemsRequest{
		ConversationID: conv.ID,
		SessionID:      conv.RootSessionID,
		Order:          storage.DirectionAsc,
		Limit:          100,
	})
	if err != nil {
		t.Fatal(err)
	}
	return page.Items
}

// listRootTurnIDs 按开始顺序返回根 Session 的 TurnID。
func (h *runtimeHarness) listRootTurnIDs(t *testing.T, conv defs.Conversation) []defs.TurnID {
	t.Helper()
	// 1. 按正序查询最多 100 条 Turn。
	page, err := h.rt.ListTurns(t.Context(), &runtime.ListTurnsRequest{
		ConversationID: conv.ID,
		SessionID:      conv.RootSessionID,
		Order:          storage.Order{Direction: storage.DirectionAsc},
		Limit:          100,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 2. 提取各条 Turn 的 ID。
	turnIDs := make([]defs.TurnID, 0, len(page.Items))
	for _, turn := range page.Items {
		turnIDs = append(turnIDs, turn.ID)
	}
	return turnIDs
}

// isConversationListed 指定对话是否在对应归档状态的列表中？
func (h *runtimeHarness) isConversationListed(t *testing.T, id defs.ConversationID, archived bool) bool {
	t.Helper()
	// 1. 读取指定归档状态的前 100 条对话并查找 ID。
	page, err := h.rt.ListConversations(t.Context(), &dao.ListConversationsRequest{
		Order:    storage.Order{Direction: storage.DirectionAsc},
		Limit:    100,
		Archived: archived,
	})
	if err != nil {
		t.Fatal(err)
	}
	return slices.ContainsFunc(page.Items, func(entry dao.ConversationListEntry) bool { return entry.Conversation.ID == id })
}

// requireListed 检查对话只出现在指定归档状态的列表中。
func (h *runtimeHarness) requireListed(t *testing.T, id defs.ConversationID, archived bool) {
	t.Helper()
	// 1. 分别查询指定状态和相反状态，不符合则结束测试。
	if !h.isConversationListed(t, id, archived) || h.isConversationListed(t, id, !archived) {
		t.Fatalf("conversation %s listed as archived = %v, %v; want only archived = %v",
			id, h.isConversationListed(t, id, true), h.isConversationListed(t, id, false), archived)
	}
}

// countMetadataLines 返回对话 metadata.jsonl 的行数。
func (h *runtimeHarness) countMetadataLines(t *testing.T, id defs.ConversationID) int {
	t.Helper()
	// 1. 读取 metadata 文件并统计换行符。
	data, err := os.ReadFile(filepath.Join(h.store.root, "conversations", string(id), "metadata.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

// readFirstLogItem 直接读取根日志首行，包含 DAO 隐藏的分叉标记。
func (h *runtimeHarness) readFirstLogItem(t *testing.T, conv defs.Conversation) *defs.Item {
	t.Helper()
	// 1. 从日志存储读取首条记录。
	page, err := h.store.opt.Logs.List(t.Context(), storage.ListLogEntriesRequest{
		Path:     storage.StreamPath(conv.ID),
		StreamID: storage.StreamID(conv.RootSessionID),
		Order:    storage.DirectionAsc,
		Limit:    1,
	})
	if err != nil || len(page.Items) == 0 {
		t.Fatalf("first log line = %+v, %v", page.Items, err)
	}
	// 2. 解析 Item 并返回，读取或解析失败则结束测试。
	var item defs.Item
	if err := json.Unmarshal(page.Items[0].Payload, &item); err != nil {
		t.Fatal(err)
	}
	return &item
}
