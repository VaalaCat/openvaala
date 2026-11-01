package tui

import (
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/session"
)

// TestAgentsPanel 检查子 Agent 树、用量和最近结果的显示。
func TestAgentsPanel(t *testing.T) {
	// 1. 准备打开面板的方法：同步根 Session、运行中的子 Session 和它的子任务、失败的子 Session，保存子 Agent 结果后执行 /agents。
	open := func(h *tuiHarness, result string) {
		h.openDraftConversation("开场")
		root := newRootInfo("t1")
		root.Usage = defs.TokenUsage{
			Input:  12000,
			Output: 3000,
		}
		child := newChildInfo("s2", "检查", "")
		child.Usage = defs.TokenUsage{
			Input:  5000,
			Output: 800,
		}
		grandchild := newChildInfo("s4", "子任务", "")
		grandchild.Session.ParentSessionID = "s2"
		done := newChildInfo("s3", "写文档", "")
		done.LastTurnStatus = defs.TurnStatusFailed
		h.emit(&runtimeclient.ConversationSynced{
			ConversationID: "c1",
			SessionInfos:   []runtime.SessionRuntimeInfo{root, child, grandchild, done},
		})
		running := child
		running.ActiveTurnID, running.State = "t9", session.SessionStateRunning
		h.emit(&runtimeclient.SessionStatusChanged{
			ConversationID: "c1",
			Info:           running,
		})
		h.now = h.now.Add(12 * time.Second)
		// 保存首条输入和结果通知，移除 input-1 的排队记录。
		h.emit(&runtimeclient.ItemsSaved{
			ConversationID: "c1",
			Items: logItems(
				userItem("input-1", "开场"),
				agentItem("a1", "s2", result, &defs.SubagentNotification{
					SourceTurnID: "t8",
					Status:       defs.TurnStatusCompleted,
				})),
		})
		h.typeText("/agents")
		h.press("enter")
	}
	// 2. 列出列表与合计、子任务没有结果、结果行数、控制字符、开始时间未知和空列表场景。
	tests := []struct {
		name string
		run  func(t *testing.T, h *tuiHarness)
	}{
		{
			name: "rows_total_and_latest_result",
			run: func(t *testing.T, h *tuiHarness) {
				open(h, "检查完成，没有问题\n细节略")
				view := h.view()
				for _, want := range []string{"检查  运行中  12s  gpt-x·high  ctx 33%  入 5K · 出 800", "  子任务", "写文档  失败", "合计  入 17K · 出 3.8K", "检查完成，没有问题"} {
					if !strings.Contains(view, want) {
						t.Fatalf("agents panel lacks %q:\n%s", want, view)
					}
				}
				requireGolden(t, "agents_panel_80x30", view)
			},
		},
		{
			name: "grandchild_has_no_result",
			run: func(t *testing.T, h *tuiHarness) {
				open(h, "检查完成，没有问题\n细节略")
				h.press("down")
				if view := h.view(); !strings.Contains(view, "最近结果：—") {
					t.Fatalf("grandchild result = %q", view)
				}
			},
		},
		{
			name: "result_is_limited_to_three_lines",
			run: func(t *testing.T, h *tuiHarness) {
				open(h, "第一行\n第二行\n第三行\n第四行")
				view := h.view()
				if !strings.Contains(view, "第三行") || strings.Contains(view, "第四行") {
					t.Fatalf("result lines not limited to %d:\n%s", agentResultLineLimit, view)
				}
			},
		},
		{
			name: "result_is_sanitized",
			run: func(t *testing.T, h *tuiHarness) {
				open(h, "正常\r\n\x1b[41m红底\x1b[0m")
				view, raw := h.view(), h.model.View().Content
				if strings.ContainsAny(view, "\r\x1b") || strings.Contains(raw, "\r") || strings.Contains(raw, "[41m") {
					t.Fatalf("result leaked control sequences: %q", raw)
				}
				if !strings.Contains(view, "正常") || !strings.Contains(view, "红底") {
					t.Fatalf("sanitized result lost its text:\n%s", view)
				}
			},
		},
		{
			name: "unknown_start_omits_elapsed",
			run: func(t *testing.T, h *tuiHarness) {
				h.openDraftConversation("开场")
				running := newChildInfo("s2", "检查", "t9")
				// 同步时已在运行：TUI 没有观察到开始，不知道耗时。
				h.emit(&runtimeclient.ConversationSynced{
					ConversationID: "c1",
					SessionInfos:   []runtime.SessionRuntimeInfo{newRootInfo(""), running},
				})
				h.typeText("/agents")
				h.press("enter")
				if view := h.view(); !strings.Contains(view, "检查  运行中  gpt-x·high  ctx 33%") {
					t.Fatalf("running row without start time:\n%s", view)
				}
			},
		},
		{
			name: "draft_has_no_subagents",
			run: func(t *testing.T, h *tuiHarness) {
				h.typeText("/agents")
				h.press("enter")
				if view := h.view(); !strings.Contains(view, "还没有子 Agent") {
					t.Fatalf("view = %q", view)
				}
			},
		},
	}
	// 3. 每个场景在一个新的 80x30 TUI 里运行。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, newTUIHarness(t, 80, 30))
		})
	}
}

// TestFormatSessionState 检查 Session 状态文字和对应颜色。
func TestFormatSessionState(t *testing.T) {
	// 1. 准备无颜色和有颜色的状态样式及各类 Session 状态。
	plain, colored := newTUIStyles(markdownStyleNoTTY, nil), newTUIStyles("dark", nil)
	tests := []struct {
		name string
		info runtime.SessionRuntimeInfo
		want string
		// wantStyle 是状态文字对应的颜色样式。
		wantStyle lipgloss.Style
	}{
		{
			name: "running",
			info: runtime.SessionRuntimeInfo{
				ActiveTurnID: "t1",
				State:        session.SessionStateRunning,
			},
			want:      "运行中",
			wantStyle: colored.accent,
		},
		{
			name:      "stopping",
			info:      runtime.SessionRuntimeInfo{State: session.SessionStateStopping},
			want:      "已停止",
			wantStyle: colored.faint,
		},
		{
			name: "stopped",
			info: runtime.SessionRuntimeInfo{
				State:          session.SessionStateStopped,
				LastTurnStatus: defs.TurnStatusCompleted,
			},
			want:      "已停止",
			wantStyle: colored.faint,
		},
		{
			name: "idle_completed",
			info: runtime.SessionRuntimeInfo{
				State:          session.SessionStateIdle,
				LastTurnStatus: defs.TurnStatusCompleted,
			},
			want:      "完成",
			wantStyle: colored.success,
		},
		{
			name: "idle_failed",
			info: runtime.SessionRuntimeInfo{
				State:          session.SessionStateIdle,
				LastTurnStatus: defs.TurnStatusFailed,
			},
			want:      "失败",
			wantStyle: colored.failure,
		},
		{
			name: "idle_interrupted",
			info: runtime.SessionRuntimeInfo{
				State:          session.SessionStateIdle,
				LastTurnStatus: defs.TurnStatusInterrupted,
			},
			want:      "已中断",
			wantStyle: colored.warning,
		},
		{
			name:      "idle_never_ran",
			info:      runtime.SessionRuntimeInfo{State: session.SessionStateIdle},
			want:      "空闲",
			wantStyle: colored.faint,
		},
	}
	// 2. 检查每种状态的文字和着色结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatSessionState(tt.info, plain); got != tt.want {
				t.Fatalf("formatSessionState = %q, want %q", got, tt.want)
			}
			if got, want := formatSessionState(tt.info, colored), tt.wantStyle.Render(tt.want); got != want {
				t.Fatalf("colored formatSessionState = %q, want %q", got, want)
			}
		})
	}
}
