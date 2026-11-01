package tui

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/storage"
	"github.com/charmbracelet/x/ansi"
)

// newRootInfo 返回根 Session 的模型和上下文运行信息样例。
func newRootInfo(activeTurnID defs.TurnID) runtime.SessionRuntimeInfo {
	// 1. 按活动 Turn ID 选择运行状态，再写入模型和估算占用。
	state := session.SessionStateIdle
	if activeTurnID != "" {
		state = session.SessionStateRunning
	}
	return runtime.SessionRuntimeInfo{
		Session: defs.Session{
			ID:             "s1",
			ConversationID: "c1",
			AgentName:      "assistant",
			Model: defs.ModelSelection{
				Ref: defs.ModelRef{
					ProviderID: "openai",
					ModelID:    "gpt-x",
				},
				ReasoningEffort: "high",
			},
		},
		State:        state,
		ActiveTurnID: activeTurnID,
		Context: runtime.ContextUsage{
			Window: 128000,
			Tokens: 42000,
			Source: runtime.ContextUsageSourceEstimated,
		},
	}
}

// newChildInfo 返回根 Session 下指定子 Session 的运行信息样例。
func newChildInfo(id defs.SessionID, taskName string, activeTurnID defs.TurnID) runtime.SessionRuntimeInfo {
	// 1. 复用根 Session 样例，替换 Session ID、任务名和父 Session ID。
	info := newRootInfo(activeTurnID)
	info.Session.ID, info.Session.TaskName, info.Session.ParentSessionID = id, taskName, "s1"
	return info
}

// TestSessionStatuses 检查 Session 状态同步、运行计时和名称读取。
func TestSessionStatuses(t *testing.T) {
	// 1. 准备固定时间和状态变化场景。
	start := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		run  func(t *testing.T, s *sessionStatuses)
	}{
		{
			name: "change_to_running_starts_clock",
			run: func(t *testing.T, s *sessionStatuses) {
				s.handleChanged(newRootInfo("t1"), start)
				if since, found := s.getRunningSince("s1"); !found || !since.Equal(start) || !s.isRootRunning() {
					t.Fatalf("since = %v %v, running = %v", since, found, s.isRootRunning())
				}
				s.handleChanged(newRootInfo("t1"), start.Add(time.Minute))
				if since, _ := s.getRunningSince("s1"); !since.Equal(start) {
					t.Fatalf("same turn restarted the clock: %v", since)
				}
				s.handleChanged(newRootInfo("t2"), start.Add(2*time.Minute))
				if since, _ := s.getRunningSince("s1"); !since.Equal(start.Add(2 * time.Minute)) {
					t.Fatalf("new turn did not restart the clock: %v", since)
				}
				s.handleChanged(newRootInfo(""), start.Add(3*time.Minute))
				if _, found := s.getRunningSince("s1"); found || s.isRootRunning() {
					t.Fatal("idle session still has a start time")
				}
			},
		},
		{
			name: "synced_running_has_unknown_start",
			run: func(t *testing.T, s *sessionStatuses) {
				s.handleSynced([]runtime.SessionRuntimeInfo{newRootInfo("t1")})
				if _, found := s.getRunningSince("s1"); found || !s.isRootRunning() {
					t.Fatal("start time known after the first sync")
				}
			},
		},
		{
			name: "resync_keeps_start_of_same_turn",
			run: func(t *testing.T, s *sessionStatuses) {
				s.handleChanged(newRootInfo("t1"), start)
				s.handleChanged(newChildInfo("s2", "检查", "t9"), start)
				s.handleSynced([]runtime.SessionRuntimeInfo{newRootInfo("t1"), newChildInfo("s2", "检查", "t10")})
				if since, found := s.getRunningSince("s1"); !found || !since.Equal(start) {
					t.Fatal("resync dropped the start of the same turn")
				}
				if _, found := s.getRunningSince("s2"); found {
					t.Fatal("resync kept the start of a different turn")
				}
			},
		},
		{
			name: "names_and_subagent_count",
			run: func(t *testing.T, s *sessionStatuses) {
				child := newChildInfo("s2", "检查", "t2")
				unnamed := newChildInfo("session-0003", "", "")
				unnamed.Session.AgentName = "reviewer"
				s.handleSynced([]runtime.SessionRuntimeInfo{newRootInfo(""), child, unnamed})
				got := []string{s.getSessionName("s2"), s.getSessionName("session-0003"), s.getSessionName("unknown-session-id"), s.getSessionName("")}
				if want := []string{"检查", "reviewer", "ssion-id", ""}; !slices.Equal(got, want) {
					t.Fatalf("names = %q, want %q", got, want)
				}
				if count := s.getRunningSubagentCount(); count != 1 || !s.isAnyRunning() || s.isRootRunning() {
					t.Fatalf("running subagents = %d, any = %v, root = %v", count, s.isAnyRunning(), s.isRootRunning())
				}
			},
		},
		{
			name: "draft_has_no_root",
			run: func(t *testing.T, _ *sessionStatuses) {
				draft := newSessionStatuses("")
				if _, found := draft.getRoot(); found || draft.isRootRunning() || draft.isAnyRunning() {
					t.Fatal("draft reports a root session")
				}
			},
		},
	}
	// 2. 逐场景同步或更新 Session，检查计时和查询结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, newSessionStatuses("s1"))
		})
	}
}

// TestQueuedInputs 检查排队预览和输入移除。
func TestQueuedInputs(t *testing.T) {
	// 1. 准备输入保存记录和排队变化场景。
	saved := func(ids ...defs.ItemID) []runtimeprotocol.LoggedItem {
		var items []runtimeprotocol.LoggedItem
		for index, id := range ids {
			items = append(items, runtimeprotocol.LoggedItem{
				Ordinal: storage.Ordinal(index + 1),
				Item:    &defs.Item{ID: id},
			})
		}
		return items
	}
	tests := []struct {
		name  string
		build func(q *queuedInputs)
		want  []string
	}{
		{
			name:  "empty",
			build: func(*queuedInputs) {},
			want:  nil,
		},
		{
			name: "two_entries_first_lines",
			build: func(q *queuedInputs) {
				q.add("i1", "接下来检查 race\n第二行")
				q.add("i2", "再跑一遍")
			},
			want: []string{"  排队：接下来检查 race", "  排队：再跑一遍"},
		},
		{
			name: "overflow_counts_rest",
			build: func(q *queuedInputs) {
				for _, id := range []defs.ItemID{"i1", "i2", "i3", "i4"} {
					q.add(id, string(id))
				}
			},
			want: []string{"  排队：i1", "  排队：i2  +2"},
		},
		{
			name: "saved_entries_removed",
			build: func(q *queuedInputs) {
				q.add("i1", "一")
				q.add("i2", "二")
				q.handleItemsSaved(saved("i1", "x"))
			},
			want: []string{"  排队：二"},
		},
		{
			// 被 UserPromptSubmit 拦下的输入不保存，Turn 结束时按 InputItemIDs 移除。
			name: "turn_finished_removes_unsaved_inputs",
			build: func(q *queuedInputs) {
				q.add("i1", "一")
				q.add("i2", "二")
				q.handleTurnFinished([]defs.ItemID{"i1", "x"})
			},
			want: []string{"  排队：二"},
		},
		{
			name: "stopped_root_clears",
			build: func(q *queuedInputs) {
				q.add("i1", "一")
				stopped := newRootInfo("")
				stopped.State = session.SessionStateStopped
				q.handleRootChanged(stopped)
			},
			want: nil,
		},
		{
			// 两轮之间的空闲推送保留排队项。
			name: "idle_root_change_keeps",
			build: func(q *queuedInputs) {
				q.add("i1", "一")
				q.handleRootChanged(newRootInfo(""))
			},
			want: []string{"  排队：一"},
		},
		{
			name: "idle_root_sync_clears",
			build: func(q *queuedInputs) {
				q.add("i1", "一")
				q.handleRootSynced(newRootInfo(""))
			},
			want: nil,
		},
	}
	// 2. 构造队列并应用事件，比较预览行。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := &queuedInputs{}
			tt.build(q)
			got := q.getPreviewLines(80, newTUIStyles(markdownStyleNoTTY, nil))
			if !slices.Equal(got, tt.want) {
				t.Fatalf("preview = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestFooterLine 检查底栏内容和宽度不足时的省略顺序。
func TestFooterLine(t *testing.T) {
	// 1. 准备执行中换了模型的根 Session，以及不同宽度和 Session 信息的底栏场景。
	executing := newRootInfo("t1")
	executing.ActiveTurnModel = defs.ModelSelection{Ref: defs.ModelRef{
		ProviderID: "openai",
		ModelID:    "gpt-old",
	}}
	tests := []struct {
		name  string
		width int
		infos []runtime.SessionRuntimeInfo
		want  string
	}{
		{
			name:  "full",
			width: 120,
			infos: []runtime.SessionRuntimeInfo{newRootInfo(""), newChildInfo("s2", "检查", "t2")},
			want:  "gpt-x·high · ctx ≈42K/128K 33% · 子Agent 1 运行 · local:/work/project",
		},
		{
			name:  "drop_cwd",
			width: 50,
			infos: []runtime.SessionRuntimeInfo{newRootInfo(""), newChildInfo("s2", "检查", "t2")},
			want:  "gpt-x·high · ctx ≈42K/128K 33% · 子Agent 1 运行",
		},
		{
			name:  "drop_subagents",
			width: 35,
			infos: []runtime.SessionRuntimeInfo{newRootInfo(""), newChildInfo("s2", "检查", "t2")},
			want:  "gpt-x·high · ctx ≈42K/128K 33%",
		},
		{
			name:  "shorten_model",
			width: 26,
			infos: []runtime.SessionRuntimeInfo{newRootInfo("")},
			want:  "gpt-x… · ctx ≈42K/128K 33%",
		},
		{
			name:  "executing_other_model",
			width: 120,
			infos: []runtime.SessionRuntimeInfo{executing},
			want:  "执行 gpt-old · 下一 Turn gpt-x·high · ctx ≈42K/128K 33% · local:/work/project",
		},
		{
			name:  "draft_without_conversation",
			width: 80,
			want:  "默认模型 · local:/work/project",
		},
		{
			name:  "truncate_context_only",
			width: 10,
			infos: []runtime.SessionRuntimeInfo{newRootInfo("")},
			want:  "ctx ≈42K/…",
		},
	}
	// 2. 连接服务并同步信息，比较普通与着色底栏的文字。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTUIHarness(t, tt.width, 24, func(c *fakeClient) { c.executors = testExecutors })
			h.connectLocalServer()
			if tt.infos != nil {
				h.openDraftConversation("开场")
				h.emit(&runtimeclient.ConversationSynced{
					ConversationID: "c1",
					Conversation:   *h.model.conversation,
					SessionInfos:   tt.infos,
				})
			}
			if got := h.model.getFooterLine(); got != tt.want {
				t.Fatalf("footer = %q, want %q", got, tt.want)
			}
			// 上色之后省略规则不变：去掉样式后与不上色时逐字相同。
			h.model.styles = newTUIStyles("dark", nil)
			if got := ansi.Strip(h.model.getFooterLine()); got != tt.want {
				t.Fatalf("colored footer = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestFooterLineStyles 检查底栏连接、上下文和子 Agent 的颜色。
func TestFooterLineStyles(t *testing.T) {
	// 1. 准备状态和占用样例及预期样式。
	colored := newTUIStyles("dark", nil)
	separator := colored.faint.Render(" · ")
	full := newRootInfo("")
	full.Context.Tokens = 120000
	tests := []struct {
		name       string
		connection runtimeclient.ConnectionState
		infos      []runtime.SessionRuntimeInfo
		want       string
	}{
		// 模型正常色，上下文 33% 暗色，子 Agent 强调色，工作位置暗色。
		{
			name:       "connected_with_subagent",
			connection: runtimeclient.ConnectionConnected,
			infos:      []runtime.SessionRuntimeInfo{newRootInfo(""), newChildInfo("s2", "检查", "t2")},
			want:       "gpt-x·high" + separator + colored.faint.Render("ctx ≈42K/128K 33%") + separator + colored.accent.Render("子Agent 1 运行") + separator + colored.faint.Render("local:/work/project"),
		},
		{
			name:       "reconnecting_warning_and_context_failure",
			connection: runtimeclient.ConnectionReconnecting,
			infos:      []runtime.SessionRuntimeInfo{full},
			want:       colored.warning.Render("重连中") + separator + "gpt-x·high" + separator + colored.failure.Render("ctx ≈120K/128K 94%") + separator + colored.faint.Render("local:/work/project"),
		},
		// 还没连上过 serve 时不知道工作位置，省掉这一栏。
		{
			name:       "draft_connecting_faint",
			connection: runtimeclient.ConnectionConnecting,
			want:       colored.faint.Render("连接中") + separator + "默认模型",
		},
	}
	// 2. 同步状态并生成底栏，比较着色结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTUIHarness(t, 120, 24, func(c *fakeClient) { c.executors = testExecutors })
			if tt.connection != runtimeclient.ConnectionConnecting {
				h.connectLocalServer()
			}
			h.emit(&runtimeclient.ConnectionChanged{State: tt.connection})
			if tt.infos != nil {
				h.openDraftConversation("开场")
				h.emit(&runtimeclient.ConversationSynced{
					ConversationID: "c1",
					Conversation:   *h.model.conversation,
					SessionInfos:   tt.infos,
				})
			}
			h.model.styles = colored
			if got := h.model.getFooterLine(); got != tt.want {
				t.Fatalf("footer = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestFooterWorkLocation 检查底栏工作位置和本机目录缩写。
func TestFooterWorkLocation(t *testing.T) {
	// 1. 准备家目录、Executor 设置和查询错误。
	t.Setenv("HOME", "/home/tester")
	underHome := defs.ExecutorSettings{WorkspaceDirectories: map[string]string{"local": "/home/tester/code/app"}}
	tests := []struct {
		name     string
		serverID string
		listErr  error
		// reconnectErr 非 nil 时，第一次拉取成功后断线重连，重连后的拉取以它失败。
		reconnectErr error
		// settings 是打开的对话记录里的设置；为 nil 时停在草稿。
		settings *defs.ExecutorSettings
		want     string
	}{
		// 默认 Executor 没有记目录时取 Executor 列表里的默认根目录。
		{
			name:     "gpu_default_uses_its_root",
			serverID: "remote",
			settings: &defs.ExecutorSettings{DefaultExecutorID: "gpu"},
			want:     "gpu:/data/proj",
		},
		{
			name:     "local_on_local_server_abbreviates_home",
			serverID: tuiTestLocalServerID,
			settings: &underHome,
			want:     "local:~/code/app",
		},
		{
			name:     "local_on_remote_server_keeps_path",
			serverID: "remote",
			settings: &underHome,
			want:     "local:/home/tester/code/app",
		},
		// 远端 serve 的草稿没有 local 目录：显示 serve 的工作目录，不显示 TUI 的 cwd。
		{
			name:     "remote_draft_uses_serve_directory",
			serverID: "remote",
			want:     "local:/srv/serve",
		},
		{
			name:     "list_failure_shows_executor_id",
			serverID: "remote",
			listErr:  errors.New("boom"),
			settings: &defs.ExecutorSettings{DefaultExecutorID: "gpu"},
			want:     "gpu",
		},
		// 重连后拉取失败时丢掉上一次的列表（可能属于另一台 serve），同样只显示 ID。
		{
			name:         "list_failure_after_reconnect_drops_old_list",
			serverID:     "remote",
			reconnectErr: errors.New("boom"),
			settings:     &defs.ExecutorSettings{DefaultExecutorID: "gpu"},
			want:         "gpu",
		},
	}
	// 2. 连接或重连服务，同步对话设置后检查工作位置。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTUIHarness(t, 120, 24, func(c *fakeClient) { c.executors, c.listExecutorsErr = testExecutors, tt.listErr })
			h.emit(&runtimeclient.ConnectionChanged{
				State:    runtimeclient.ConnectionConnected,
				ServerID: tt.serverID,
			})
			h.waitFor("executors requested", func() bool {
				return len(h.model.executors) > 0 || tt.listErr != nil && len(h.client.getCalls("ListExecutors")) == 1
			})
			if tt.reconnectErr != nil {
				h.client.set(func(c *fakeClient) { c.listExecutorsErr = tt.reconnectErr })
				h.emit(&runtimeclient.ConnectionChanged{State: runtimeclient.ConnectionReconnecting})
				h.emit(&runtimeclient.ConnectionChanged{
					State:    runtimeclient.ConnectionConnected,
					ServerID: tt.serverID,
				})
				h.waitFor("old executors dropped", func() bool { return h.model.executors == nil })
			}
			if tt.settings != nil {
				h.openDraftConversation("开场")
				h.emit(&runtimeclient.ConversationSynced{
					ConversationID: "c1",
					Conversation: defs.Conversation{
						ID:               "c1",
						ExecutorSettings: *tt.settings,
					},
				})
			}
			if got := h.model.getFooterLine(); got != "默认模型 · "+tt.want {
				t.Fatalf("footer = %q, want the location %q", got, tt.want)
			}
		})
	}
}

// TestStatusLineStyles 检查生成、思考、重连和通知的状态样式。
func TestStatusLineStyles(t *testing.T) {
	// 1. 准备各状态操作和预期片段。
	colored := newTUIStyles("dark", nil)
	tests := []struct {
		name string
		run  func(h *tuiHarness)
		want string
	}{
		{
			name: "generating_accent",
			run:  func(h *tuiHarness) {},
			want: colored.accent.Render("生成中") + " 0s" + colored.faint.Render(" · ") + colored.hintKey.Render("esc esc") + colored.faint.Render(" 中断"),
		},
		{
			name: "thinking_agent",
			run: func(h *tuiHarness) {
				reasoning := &defs.Item{
					ID:          "x1",
					TurnID:      "t1",
					ModelCallID: "m1",
					Kind:        defs.ItemReasoning,
					Reasoning:   &defs.Reasoning{},
				}
				started := itemStarted(0, reasoning)
				started.ConversationID = "c1"
				h.emit(started)
			},
			want: colored.agent.Render("思考中") + " 0s",
		},
		{
			name: "reconnecting_warning",
			run: func(h *tuiHarness) {
				h.emit(&runtimeclient.ConnectionChanged{State: runtimeclient.ConnectionReconnecting})
			},
			want: colored.warning.Render("⚠ 重连中"),
		},
		// 运行中说明与临时通知之间的分隔也是暗色。
		{
			name: "notice_separator_faint",
			run:  func(h *tuiHarness) { h.press("esc") },
			want: colored.faint.Render(" 中断") + colored.faint.Render(" · ") + colored.faint.Render("再按 Esc 中断"),
		},
	}
	// 2. 运行根 Session 并应用状态操作，检查单行状态内容。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTUIHarness(t, 120, 24)
			h.openDraftConversation("开场")
			h.emit(&runtimeclient.SessionStatusChanged{
				ConversationID: "c1",
				Info:           newRootInfo("t1"),
			})
			tt.run(h)
			h.model.styles = colored
			if lines := h.model.getStatusLines(); len(lines) != 1 || !strings.Contains(lines[0], tt.want) {
				t.Fatalf("status = %q, want it to contain %q", lines, tt.want)
			}
		})
	}
}

// TestQueuedPreviewStyles 检查排队标签、正文、数量和截断样式。
func TestQueuedPreviewStyles(t *testing.T) {
	// 1. 准备排队输入、宽度和预期显示。
	plain, colored := newTUIStyles(markdownStyleNoTTY, nil), newTUIStyles("dark", nil)
	tests := []struct {
		name  string
		texts []string
		width int
		// want 是有颜色时的各行，为 nil 时不比较；wantPlain 是不上色时的各行，有颜色时去掉样式后也必须与它相同。
		want, wantPlain []string
	}{
		{
			name:      "label_accent_text_faint",
			texts:     []string{"一"},
			width:     80,
			want:      []string{"  " + colored.accent.Render("排队：") + colored.faint.Render("一")},
			wantPlain: []string{"  排队：一"},
		},
		{
			name:  "overflow_count_faint",
			texts: []string{"一", "二", "三"},
			width: 80,
			want: []string{
				"  " + colored.accent.Render("排队：") + colored.faint.Render("一"),
				"  " + colored.accent.Render("排队：") + colored.faint.Render("二") + colored.faint.Render("  +1"),
			},
			wantPlain: []string{"  排队：一", "  排队：二  +1"},
		},
		// 截断按显示宽度计算，与不上色时相同。
		{
			name:      "truncated_like_plain",
			texts:     []string{"很长很长很长很长的输入"},
			width:     14,
			wantPlain: []string{"  排队：很长…"},
		},
	}
	// 2. 生成普通与着色预览，比较文字及样式。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := &queuedInputs{}
			for index, text := range tt.texts {
				q.add(defs.ItemID(fmt.Sprint(index)), text)
			}
			coloredLines := q.getPreviewLines(tt.width, colored)
			stripped := make([]string, len(coloredLines))
			for index, line := range coloredLines {
				stripped[index] = ansi.Strip(line)
			}
			if got := q.getPreviewLines(tt.width, plain); !slices.Equal(got, tt.wantPlain) || !slices.Equal(stripped, tt.wantPlain) {
				t.Fatalf("plain preview = %q, colored stripped = %q, want %q", got, stripped, tt.wantPlain)
			}
			if tt.want != nil && !slices.Equal(coloredLines, tt.want) {
				t.Fatalf("preview = %q, want %q", coloredLines, tt.want)
			}
		})
	}
}

// TestFormatContextUsage 检查实际、估算和未知上下文的显示。
func TestFormatContextUsage(t *testing.T) {
	// 1. 准备不同来源和窗口用量。
	tests := []struct {
		name  string
		usage runtime.ContextUsage
		want  string
	}{
		{
			name: "actual",
			usage: runtime.ContextUsage{
				Window: 128000,
				Tokens: 39800,
				Source: runtime.ContextUsageSourceActual,
			},
			want: "39.8K/128K 31%",
		},
		{
			name: "estimated",
			usage: runtime.ContextUsage{
				Window: 128000,
				Tokens: 42000,
				Source: runtime.ContextUsageSourceEstimated,
			},
			want: "≈42K/128K 33%",
		},
		{
			name: "unknown",
			usage: runtime.ContextUsage{
				Window: 128000,
				Source: runtime.ContextUsageSourceUnknown,
			},
			want: "— / 128K",
		},
		{
			name: "unknown_window",
			usage: runtime.ContextUsage{
				Tokens: 900,
				Source: runtime.ContextUsageSourceActual,
			},
			want: "900 窗口未知",
		},
		{
			name:  "unknown_all",
			usage: runtime.ContextUsage{Source: runtime.ContextUsageSourceUnknown},
			want:  "— 窗口未知",
		},
	}
	// 2. 比较上下文用量文字。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatContextUsage(tt.usage); got != tt.want {
				t.Fatalf("context = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestSessionTree 检查子 Session 树顺序和整树用量。
func TestSessionTree(t *testing.T) {
	// 1. 准备根、子和孙 Session 及用量。
	root := newRootInfo("")
	root.Usage = defs.TokenUsage{
		Input:  1000,
		Output: 100,
	}
	child := newChildInfo("s2", "检查", "t2")
	child.Usage = defs.TokenUsage{
		Input:  500,
		Output: 50,
	}
	grandchild := newChildInfo("s4", "子任务", "")
	grandchild.Session.ParentSessionID = "s2"
	sibling := newChildInfo("s3", "写文档", "")
	tests := []struct {
		name          string
		rootSessionID defs.SessionID
		infos         []runtime.SessionRuntimeInfo
		wantRows      []string
		wantTotal     defs.TokenUsage
	}{
		{
			name:          "depth_first_sorted_by_id",
			rootSessionID: "s1",
			infos:         []runtime.SessionRuntimeInfo{root, sibling, grandchild, child},
			wantRows:      []string{"0:s2", "1:s4", "0:s3"},
			wantTotal: defs.TokenUsage{
				Input:  1500,
				Output: 150,
			},
		},
		{name: "draft_has_no_rows"},
	}
	// 2. 同步 Session，检查深度优先行顺序和总用量。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			statuses := newSessionStatuses(tt.rootSessionID)
			statuses.handleSynced(tt.infos)
			var got []string
			for _, row := range statuses.getSubagentRows() {
				got = append(got, fmt.Sprintf("%d:%s", row.depth, row.info.Session.ID))
			}
			if !slices.Equal(got, tt.wantRows) {
				t.Fatalf("rows = %v, want %v", got, tt.wantRows)
			}
			if total := statuses.getTotalUsage(); total.Input != tt.wantTotal.Input || total.Output != tt.wantTotal.Output {
				t.Fatalf("total = %+v, want %+v", total, tt.wantTotal)
			}
		})
	}
}
