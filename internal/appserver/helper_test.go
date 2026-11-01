package appserver

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/hook"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/session"
)

// TestGetCodexError 检查请求错误的 Codex 错误码和文本。
func TestGetCodexError(t *testing.T) {
	// 1. 列出协议、Session、对话和普通错误的转换结果。
	const threadID = "0199a8f0-0000-7000-8000-000000000001"
	tests := []struct {
		name     string
		err      error
		threadID string
		want     codexError
	}{
		{
			name: "protocol_error_kept",
			err: fmt.Errorf("wrapped: %w", &codexError{
				errorCodeMethodNotFound,
				"method not found: foo",
			}),
			want: codexError{
				-32601,
				"method not found: foo",
			},
		},
		{
			name:     "session_busy",
			err:      fmt.Errorf("%w: turn running", session.ErrSessionBusy),
			threadID: threadID,
			want: codexError{
				-32600,
				"session is busy",
			},
		},
		{
			name:     "conversation_not_found",
			err:      fmt.Errorf("open thread: %w", fmt.Errorf("%w: %s", runtime.ErrConversationNotFound, threadID)),
			threadID: threadID,
			want: codexError{
				-32600,
				"thread not found: " + threadID,
			},
		},
		{
			name: "invalid_params_kept",
			err: fmt.Errorf("conversation/create: %w", &runtimeclient.ResponseError{
				Code:    -32602,
				Message: "invalid executor settings: unknown executor \"ssh\"",
			}),
			want: codexError{
				-32602,
				"invalid executor settings: unknown executor \"ssh\"",
			},
		},
		{
			name: "other_serve_error",
			err: fmt.Errorf("model/list: %w", &runtimeclient.ResponseError{
				Code:    -32000,
				Message: "agent \"x\" is not configured",
			}),
			want: codexError{
				-32603,
				"agent \"x\" is not configured",
			},
		},
		{
			name: "local_error",
			err:  fmt.Errorf("model/list: %w", runtimeclient.ErrConnectionLost),
			want: codexError{
				-32603,
				"model/list: connection to runtime server lost",
			},
		},
		{
			name: "plain_error",
			err:  errors.New("boom"),
			want: codexError{
				-32603,
				"boom",
			},
		},
	}
	// 2. 转换每个错误，核对错误码和完整文本。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getCodexError(tt.err, tt.threadID); *got != tt.want {
				t.Fatalf("getCodexError(%v, %q) = %+v, want %+v", tt.err, tt.threadID, *got, tt.want)
			}
		})
	}
}

// TestGetWebSocketCloseReason 检查关闭原因截断长度和完整字符。
func TestGetWebSocketCloseReason(t *testing.T) {
	// 1. 准备短文本、超长文本和跨字节上限的中文。
	tests := []struct {
		name   string
		reason string
		want   string
	}{
		{
			name:   "short_kept",
			reason: "cannot start the openvaala runtime server: boom",
			want:   "cannot start the openvaala runtime server: boom",
		},
		{
			name:   "long_cut_to_limit",
			reason: strings.Repeat("x", 200),
			want:   strings.Repeat("x", 123),
		},
		// 第 123 字节落在「服」的中间：整个字符丢掉，不留半个字符。
		{
			name:   "multibyte_not_split",
			reason: strings.Repeat("x", 121) + "服务",
			want:   strings.Repeat("x", 121),
		},
	}
	// 2. 截取关闭原因，核对字节上限内的完整文本。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getWebSocketCloseReason(tt.reason); got != tt.want {
				t.Fatalf("getWebSocketCloseReason = %q (%d bytes), want %q", got, len(got), tt.want)
			}
		})
	}
}

// TestGetLoadedThreadsPage 检查已加载 Thread 的排序和分页。
func TestGetLoadedThreadsPage(t *testing.T) {
	// 1. 准备乱序 Thread ID、游标和页大小。
	threadIDs := []string{"c", "a", "b"}
	cursor := func(value string) *string { return &value }
	limit := func(value int) *int { return &value }
	tests := []struct {
		name       string
		threadIDs  []string
		cursor     *string
		limit      *int
		wantData   []string
		wantCursor *string
	}{
		{
			name:     "empty",
			wantData: []string{},
		},
		{
			name:      "all_sorted_without_limit",
			threadIDs: threadIDs,
			wantData:  []string{"a", "b", "c"},
		},
		{
			name:       "first_page",
			threadIDs:  threadIDs,
			limit:      limit(2),
			wantData:   []string{"a", "b"},
			wantCursor: cursor("b"),
		},
		{
			name:      "after_cursor",
			threadIDs: threadIDs,
			cursor:    cursor("b"),
			limit:     limit(2),
			wantData:  []string{"c"},
		},
		// 游标指向的 Thread 已经退订：从比它大的下一个开始。
		{
			name:      "cursor_no_longer_loaded",
			threadIDs: []string{"a", "c"},
			cursor:    cursor("b"),
			wantData:  []string{"c"},
		},
		{
			name:       "zero_limit_counts_as_one",
			threadIDs:  threadIDs,
			limit:      limit(0),
			wantData:   []string{"a"},
			wantCursor: cursor("a"),
		},
		{
			name:      "cursor_after_last",
			threadIDs: threadIDs,
			cursor:    cursor("c"),
			wantData:  []string{},
		},
	}
	// 2. 读取各页，核对 Thread ID 顺序和下一页游标。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getLoadedThreadsPage(tt.threadIDs, tt.cursor, tt.limit)
			if !reflect.DeepEqual(got.Data, tt.wantData) || !reflect.DeepEqual(got.NextCursor, tt.wantCursor) {
				t.Fatalf("getLoadedThreadsPage = %v next %v, want %v next %v", got.Data, got.NextCursor, tt.wantData, tt.wantCursor)
			}
		})
	}
}

// TestGetHistoryPageLimit 检查历史页大小的默认值和上下限。
func TestGetHistoryPageLimit(t *testing.T) {
	// 1. 准备省略、过小、正常和过大的页大小。
	limit := func(value int) *int { return &value }
	tests := []struct {
		name  string
		limit *int
		want  int
	}{
		{
			name: "omitted",
			want: 25,
		},
		{
			name:  "zero_counts_as_one",
			limit: limit(0),
			want:  1,
		},
		{
			name:  "negative_counts_as_one",
			limit: limit(-5),
			want:  1,
		},
		{
			name:  "kept",
			limit: limit(40),
			want:  40,
		},
		{
			name:  "capped",
			limit: limit(1000),
			want:  100,
		},
	}
	// 2. 计算每页条数，核对默认值、下限和上限。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getHistoryPageLimit(tt.limit); got != tt.want {
				t.Fatalf("getHistoryPageLimit = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestGetCodexHookMetadata 检查用户和插件 hook 的来源字段。
func TestGetCodexHookMetadata(t *testing.T) {
	// 1. 准备用户 hook 和插件 hook。
	demo := "demo"
	tests := []struct {
		name         string
		info         hook.HookInfo
		wantSource   string
		wantPluginID *string
	}{
		{
			name: "user_hook",
			info: hook.HookInfo{
				Key:   "go:Stop:0",
				Event: defs.HookEventStop,
			},
			wantSource: "user",
		},
		{
			name: "plugin_hook",
			info: hook.HookInfo{
				Key:        "demo:SessionStart:0",
				Event:      defs.HookEventSessionStart,
				PluginName: "demo",
			},
			wantSource:   "plugin",
			wantPluginID: &demo,
		},
	}
	// 2. 转换 hook 元数据，核对 source 和 pluginId。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getCodexHookMetadata(&tt.info)
			if got.Key != tt.info.Key || got.Source != tt.wantSource || !reflect.DeepEqual(got.PluginID, tt.wantPluginID) {
				t.Fatalf("getCodexHookMetadata() = key %q source %q pluginId %v; want key %q source %q pluginId %v",
					got.Key, got.Source, got.PluginID, tt.info.Key, tt.wantSource, tt.wantPluginID)
			}
		})
	}
}

// TestGetCodexMcpServerStatuses 检查 MCP 连接的插件来源。
func TestGetCodexMcpServerStatuses(t *testing.T) {
	// 1. 准备一条普通连接和一条插件声明的连接。
	demo := "demo"
	tests := []struct {
		name          string
		servers       []conversation.MCPServerInfo
		wantPluginIDs []*string
	}{
		{
			name: "plugin_declared_server_carries_its_plugin",
			servers: []conversation.MCPServerInfo{
				{
					ExecutorID: "local",
					ConnectionInfo: mcpclient.ConnectionInfo{
						ServerID: "fs",
						State:    mcpclient.ConnectionStateConnected,
					},
				},
				{
					ExecutorID: "local",
					ConnectionInfo: mcpclient.ConnectionInfo{
						ServerID:   "demo-docs",
						State:      mcpclient.ConnectionStateConnected,
						PluginName: "demo",
					},
				},
			},
			wantPluginIDs: []*string{nil, &demo},
		},
	}
	// 2. 转换连接状态，核对每条连接的 pluginId。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			statuses := getCodexMcpServerStatuses(tt.servers, "local")
			var gotPluginIDs []*string
			for _, status := range statuses {
				gotPluginIDs = append(gotPluginIDs, status.PluginID)
			}
			if !reflect.DeepEqual(gotPluginIDs, tt.wantPluginIDs) {
				t.Fatalf("getCodexMcpServerStatuses() pluginIds = %v, want %v", gotPluginIDs, tt.wantPluginIDs)
			}
		})
	}
}
