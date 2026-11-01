package tui

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/agent/agent_context"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
)

// TestContextPanel 检查上下文汇总、逐条显示、排序和刷新。
func TestContextPanel(t *testing.T) {
	// 1. 准备测量用量及面板打开操作。
	usage := runtime.SessionContextUsage{
		Window:          128000,
		ThresholdTokens: 100000,
		NextRequest: runtime.ContextUsage{
			Window: 128000,
			Tokens: 42000,
			Source: runtime.ContextUsageSourceEstimated,
		},
		LastRequest: &runtime.LastRequestUsage{
			ModelCallID: "m1",
			Model: defs.ModelRef{
				ProviderID: "openai",
				ModelID:    "gpt-x",
			},
			Usage: defs.TokenUsage{
				Input:     39800,
				Output:    1200,
				CacheRead: 30000,
			},
			EstimatedTokens: 38700,
		},
		SystemTokens: 1200,
		ToolTokens:   8000,
		Items: []agent_context.ItemTokens{
			{
				ItemID:   "u1",
				TurnID:   "t1",
				Category: agent_context.ContextCategoryUserInput,
				Tokens:   500,
			},
			{
				ItemID:   "r1",
				TurnID:   "t1",
				Category: agent_context.ContextCategoryAssistantText,
				Tokens:   30000,
			},
			{
				ItemID:   "x9",
				TurnID:   "t1",
				Category: agent_context.ContextCategoryToolResult,
				Tokens:   2300,
			},
		},
	}
	open := func(h *tuiHarness) {
		h.client.set(func(c *fakeClient) { c.contextUsage = usage })
		h.openDraftConversation("开场")
		// 保存首条输入，移除 input-1 的排队记录。
		h.emit(&runtimeclient.ItemsSaved{
			ConversationID: "c1",
			TurnID:         "t1",
			Items: logItems(
				userItem("input-1", "开场"), userItem("u1", "你好呀"), replyItem("r1", "很长的回答")),
		})
		h.typeText("/context")
		h.press("enter")
		h.waitFor("usage loaded", func() bool { return strings.Contains(h.view(), "估算偏差") })
	}
	isUsageLoaded := func(h *tuiHarness) bool {
		opened, ok := h.model.activePanel.(*contextPanel)
		return ok && opened.usage != nil
	}
	// 2. 列出汇总页、逐条页、没有最近请求、刷新、刷新失败、预览清理和未测量场景。
	tests := []struct {
		name string
		run  func(t *testing.T, h *tuiHarness)
	}{
		{
			name: "summary_page",
			run: func(t *testing.T, h *tuiHarness) {
				open(h)
				view := h.view()
				for _, want := range []string{"42K / 128K  33%", "下一次请求  ≈42K（估算）", "压缩阈值    100K", "实际输入    39.8K", "估算偏差    +1.1K", "回复", "工具定义"} {
					if !strings.Contains(view, want) {
						t.Fatalf("summary lacks %q:\n%s", want, view)
					}
				}
				requireGolden(t, "context_panel_summary_80x40", view)
			},
		},
		{
			name: "items_page_sorting_and_preview",
			run: func(t *testing.T, h *tuiHarness) {
				open(h)
				h.press("tab")
				rows := getPanelBodyLines(h.view())
				// 第一行是表头，条目从第二行开始。
				if len(rows) < 4 || !strings.Contains(rows[1], "回复") || !strings.Contains(rows[1], "很长的回答") {
					t.Fatalf("items sorted by tokens = %q", rows)
				}
				if !strings.Contains(strings.Join(rows, "\n"), "—") {
					t.Fatalf("item without history should preview as —: %q", rows)
				}
				if !strings.Contains(h.view(), "r 刷新") {
					t.Fatalf("items page hint lacks r 刷新:\n%s", h.view())
				}
				h.press("s")
				rows = getPanelBodyLines(h.view())
				if !strings.Contains(rows[1], "用户输入") || !strings.Contains(rows[1], "你好呀") {
					t.Fatalf("items in time order = %q", rows)
				}
				h.press("shift+tab")
				if !strings.Contains(h.view(), "估算偏差") {
					t.Fatal("shift+tab did not return to the summary page")
				}
			},
		},
		{
			name: "no_last_request",
			run: func(t *testing.T, h *tuiHarness) {
				withoutLast := usage
				withoutLast.LastRequest = nil
				h.client.set(func(c *fakeClient) { c.contextUsage = withoutLast })
				h.openDraftConversation("开场")
				h.typeText("/context")
				h.press("enter")
				h.waitFor("usage loaded", func() bool { return isUsageLoaded(h) })
				rows := getPanelBodyLines(h.view())
				index := slices.Index(rows, "最近请求")
				if index < 0 || index+1 >= len(rows) || rows[index+1] != "—" || strings.Contains(h.view(), "估算偏差") {
					t.Fatalf("last request section = %q", rows)
				}
			},
		},
		{
			name: "refresh_with_r",
			run: func(t *testing.T, h *tuiHarness) {
				open(h)
				h.press("r")
				h.waitFor("refreshed", func() bool { return len(h.client.getCalls("GetContextUsage")) == 2 })
				if call := h.client.getCalls("GetContextUsage")[0]; call != "GetContextUsage c1 s1" {
					t.Fatalf("call = %q", call)
				}
			},
		},
		{
			name: "error_keeps_previous_data",
			run: func(t *testing.T, h *tuiHarness) {
				open(h)
				opened := h.model.activePanel.(*contextPanel)
				h.press("r")
				h.send(panelResultMsg{
					seq: h.model.panelRequestSeq,
					err: errors.New("timeout"),
				})
				if opened.usage == nil || !strings.Contains(h.view(), "✗ timeout") || !strings.Contains(h.view(), "估算偏差") {
					t.Fatalf("usage = %v, view = %q", opened.usage, h.view())
				}
			},
		},
		{
			// 条目预览来自模型或工具文本，\r 与转义序列不能漏进面板。
			name: "items_page_sanitizes_preview",
			run: func(t *testing.T, h *tuiHarness) {
				dirty := runtime.SessionContextUsage{Items: []agent_context.ItemTokens{
					{
						ItemID:   "r1",
						TurnID:   "t1",
						Category: agent_context.ContextCategoryAssistantText,
						Tokens:   10,
					},
				}}
				h.client.set(func(c *fakeClient) { c.contextUsage = dirty })
				h.openDraftConversation("开场")
				h.emit(&runtimeclient.ItemsSaved{
					ConversationID: "c1",
					TurnID:         "t1",
					Items: logItems(
						userItem("input-1", "开场"), replyItem("r1", "\x1b[31m红字\r被覆盖\x1b[1")),
				})
				h.typeText("/context")
				h.press("enter")
				h.waitFor("usage loaded", func() bool { return isUsageLoaded(h) })
				h.press("tab")
				rows := getPanelBodyLines(h.view())
				if len(rows) != 2 || !strings.HasPrefix(strings.TrimSpace(rows[0]), "类别") || !strings.HasSuffix(rows[1], "红字") {
					t.Fatalf("rows = %q", rows)
				}
				if content := h.model.View().Content; strings.Contains(content, "\r") || strings.Contains(content, "被覆盖") {
					t.Fatalf("raw text leaked into the panel: %q", content)
				}
			},
		},
		{
			// 尚未测量时，汇总页和逐条页都显示提示。
			name: "unmeasured_shows_hint_on_both_pages",
			run: func(t *testing.T, h *tuiHarness) {
				h.client.set(func(c *fakeClient) { c.contextUsage = runtime.SessionContextUsage{} })
				h.openDraftConversation("开场")
				h.typeText("/context")
				h.press("enter")
				h.waitFor("usage loaded", func() bool { return isUsageLoaded(h) })
				if view := h.view(); !strings.Contains(view, "还没有测量，发出第一次请求后显示") || strings.Contains(view, "按类别") {
					t.Fatalf("summary page:\n%s", view)
				}
				h.press("tab", "down", "end", "s")
				if rows := getPanelBodyLines(h.view()); len(rows) != 1 || rows[0] != "还没有测量，发出第一次请求后显示" {
					t.Fatalf("rows = %q", rows)
				}
			},
		},
	}
	// 3. 每个场景在一个新的 80x40 TUI 里操作并比较显示内容。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, newTUIHarness(t, 80, 40))
		})
	}
}

// TestFormatContextSummary 检查汇总字段、类别统计和未知用量提示。
func TestFormatContextSummary(t *testing.T) {
	// 1. 准备实际基准、窗口未知、分类统计和中断类别的用量。
	tests := []struct {
		name  string
		usage runtime.SessionContextUsage
		want  []string
		deny  []string
	}{
		{
			name: "actual_source_with_cache_write",
			usage: runtime.SessionContextUsage{
				Window:          200000,
				ThresholdTokens: 160000,
				NextRequest: runtime.ContextUsage{
					Window: 200000,
					Tokens: 39800,
					Source: runtime.ContextUsageSourceActual,
				},
				LastRequest: &runtime.LastRequestUsage{
					Usage: defs.TokenUsage{
						Input:      1000,
						Output:     20,
						CacheWrite: 300,
					},
					EstimatedTokens: 1500,
				},
			},
			want: []string{
				"占用", strings.Repeat("█", 6) + strings.Repeat("░", 18) + "│" + strings.Repeat("░", 5) + " 39.8K / 200K 20%",
				"下一次请求 39.8K（实际基准 + 估算）", "压缩阈值 160K",
				"最近请求", "实际输入 1K 缓存读 0", "输出 20 缓存写 300", "估算偏差 −500（实际输入比它自己的估算少）",
			},
			deny: []string{"≈39.8K"},
		},
		{
			name: "unknown_window_and_threshold_without_measurement",
			usage: runtime.SessionContextUsage{
				NextRequest: runtime.ContextUsage{Source: runtime.ContextUsageSourceUnknown},
			},
			want: []string{"窗口未知", "下一次请求 —（未知）", "压缩阈值 —", "最近请求\n—", "还没有测量，发出第一次请求后显示"},
			deny: []string{"按类别", "█", "%"},
		},
		{
			name: "bars_cover_the_window_of_twenty_cells",
			usage: runtime.SessionContextUsage{
				SystemTokens: 500,
				ToolTokens:   500,
				Items: []agent_context.ItemTokens{
					{
						ItemID:   "a",
						Category: agent_context.ContextCategoryReasoning,
						Tokens:   1000,
					},
					{
						ItemID:   "b",
						Category: agent_context.ContextCategoryModelChange,
						Tokens:   0,
					},
				},
			},
			want: []string{
				"system ≈500 " + strings.Repeat("█", 5) + strings.Repeat("░", 15) + " 25%",
				"工具定义 ≈500 " + strings.Repeat("█", 5) + strings.Repeat("░", 15) + " 25%",
				"推理 ≈1K " + strings.Repeat("█", 10) + strings.Repeat("░", 10) + " 50%",
			},
			deny: []string{"模型切换"},
		},
		{
			name: "turn_interrupted_category_label",
			usage: runtime.SessionContextUsage{
				Window:          1000,
				ThresholdTokens: 800,
				NextRequest: runtime.ContextUsage{
					Window: 1000,
					Tokens: 100,
					Source: runtime.ContextUsageSourceEstimated,
				},
				Items: []agent_context.ItemTokens{{
					ItemID:   "a",
					Category: agent_context.ContextCategoryTurnInterrupted,
					Tokens:   100,
				}},
			},
			want: []string{"中断 ≈100"},
			deny: []string{"turn_interrupted"},
		},
	}
	// 2. 生成汇总，合并连续空白后检查应显示和不应显示的内容。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines := formatContextSummary(tt.usage, 76, newTUIStyles(markdownStyleNoTTY, nil))
			for index, line := range lines {
				lines[index] = strings.Join(strings.Fields(line), " ")
			}
			text := strings.Join(lines, "\n")
			for _, want := range tt.want {
				if !strings.Contains(text, want) {
					t.Errorf("summary lacks %q:\n%s", want, text)
				}
			}
			for _, deny := range tt.deny {
				if strings.Contains(text, deny) {
					t.Errorf("summary contains %q:\n%s", deny, text)
				}
			}
		})
	}
}

// TestFormatContextBar 检查占用条填充、阈值标记和颜色。
func TestFormatContextBar(t *testing.T) {
	// 1. 准备低于阈值、用量未知和超过阈值三种占用条。
	plain, colored := newTUIStyles(markdownStyleNoTTY, nil), newTUIStyles("dark", nil)
	usage := func(tokens, threshold int, source runtime.ContextUsageSource) runtime.SessionContextUsage {
		return runtime.SessionContextUsage{
			Window:          200000,
			ThresholdTokens: threshold,
			NextRequest: runtime.ContextUsage{
				Window: 200000,
				Tokens: tokens,
				Source: source,
			},
		}
	}
	tests := []struct {
		name   string
		usage  runtime.SessionContextUsage
		styles tuiStyles
		want   string
	}{
		{
			name:   "below_threshold",
			usage:  usage(39800, 160000, runtime.ContextUsageSourceEstimated),
			styles: plain,
			want:   strings.Repeat("█", 6) + strings.Repeat("░", 18) + "│" + strings.Repeat("░", 5) + "  39.8K / 200K  20%",
		},
		{
			name:   "unknown_next_request",
			usage:  usage(0, 0, runtime.ContextUsageSourceUnknown),
			styles: plain,
			want:   strings.Repeat("░", 30) + "  — / 200K",
		},
		{
			name:   "over_threshold_uses_warning",
			usage:  usage(170000, 160000, runtime.ContextUsageSourceEstimated),
			styles: colored,
			want:   colored.warning.Render(strings.Repeat("█", 24)+"│"+"█") + colored.faint.Render(strings.Repeat("░", 4)) + "  170K / 200K  85%",
		},
	}
	// 2. 逐场景比较占用条。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatContextBar(tt.usage, tt.styles); got != tt.want {
				t.Fatalf("bar = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestFormatDeviation 检查估算偏差的数值和增减说明。
func TestFormatDeviation(t *testing.T) {
	// 1. 准备正、负和零偏差。
	tests := []struct {
		name      string
		deviation int
		want      string
	}{
		{
			name:      "actual_above_estimate",
			deviation: 1100,
			want:      "+1.1K（实际输入比它自己的估算多）",
		},
		{
			name:      "actual_below_estimate",
			deviation: -500,
			want:      "−500（实际输入比它自己的估算少）",
		},
		{
			name:      "matches_estimate",
			deviation: 0,
			want:      "+0（与估算一致）",
		},
	}
	// 2. 比较各偏差的显示文字。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatDeviation(tt.deviation); got != tt.want {
				t.Fatalf("formatDeviation(%d) = %q, want %q", tt.deviation, got, tt.want)
			}
		})
	}
}

// TestFormatSignedTokens 检查 token 数的正负号和数量缩写。
func TestFormatSignedTokens(t *testing.T) {
	// 1. 准备正、负、零和大数量。
	tests := []struct {
		tokens int
		want   string
	}{
		{
			tokens: 1100,
			want:   "+1.1K",
		},
		{
			tokens: -500,
			want:   "−500",
		},
		{
			tokens: 0,
			want:   "+0",
		},
		{
			tokens: -1_500_000,
			want:   "−1.5M",
		},
	}
	// 2. 逐项比较带符号的 token 文字。
	for _, tt := range tests {
		if got := formatSignedTokens(tt.tokens); got != tt.want {
			t.Errorf("formatSignedTokens(%d) = %q, want %q", tt.tokens, got, tt.want)
		}
	}
}

// TestGetItemPreview 检查各类 Item 的单行预览。
func TestGetItemPreview(t *testing.T) {
	// 1. 准备错误、推理、模型和 Executor 变化记录。
	failedResult := toolResultItem("i3", "c1", &defs.ToolError{
		Code:    "exit",
		Message: "\n没有权限\n第二行",
	})
	reasoning := &defs.Item{
		ID:   "i4",
		Kind: defs.ItemReasoning,
		Reasoning: &defs.Reasoning{
			Summary: []string{"先想想"},
			Content: []string{"再看看"},
		},
	}
	modelChange := &defs.Item{
		ID:   "i7",
		Kind: defs.ItemModelChange,
		ModelChange: &defs.ModelChange{
			PreviousModel: defs.ModelRef{
				ProviderID: "a",
				ModelID:    "x",
			},
			Model: defs.ModelRef{
				ProviderID: "b",
				ModelID:    "y",
			},
		},
	}
	executorsChange := &defs.Item{
		ID:   "i8",
		Kind: defs.ItemExecutorsChange,
		ExecutorsChange: &defs.ExecutorsChange{
			PreviousExecutors: []defs.TurnExecutor{{
				ID:                 "local",
				WorkspaceDirectory: "/a",
			}, {
				ID:                 "openvaala",
				WorkspaceDirectory: "/workspace",
			}},
			Executors: []defs.TurnExecutor{{
				ID:                 "gpu",
				WorkspaceDirectory: "/data",
			}, {
				ID:                 "local",
				WorkspaceDirectory: "/a",
			}},
		},
	}
	// 2. 列出各类 Item、终端控制和长正文的预期预览。
	tests := []struct {
		name string
		item *defs.Item
		want string
	}{
		{
			name: "nil_item",
			item: nil,
			want: "",
		},
		{
			name: "message_first_line",
			item: userItem("i1", "\n你好\n第二行"),
			want: "你好",
		},
		{
			name: "message_with_carriage_return_and_ansi",
			item: replyItem("i1", "\x1b[31m红字\r被覆盖\x1b[1"),
			want: "红字",
		},
		{
			name: "reasoning_text_prefers_summary",
			item: reasoning,
			want: "先想想",
		},
		{
			name: "tool_call_name_and_main_argument",
			item: toolCallItem("i2", "c1", "bash", `{"command":"ls -la\nmore"}`),
			want: "bash ls -la",
		},
		{
			name: "tool_call_without_argument",
			item: toolCallItem("i2", "c1", "wait_agent", `{}`),
			want: "wait_agent",
		},
		{
			name: "tool_result_output",
			item: toolResultItem("i3", "c1", nil),
			want: `{"ok":true}`,
		},
		{
			name: "tool_result_error",
			item: failedResult,
			want: "✗ 没有权限",
		},
		{
			name: "tool_result_with_carriage_return",
			item: &defs.Item{
				Kind:       defs.ItemToolResult,
				ToolResult: &defs.ToolResult{Content: json.RawMessage("\"进度 10%\r进度 100%\"")},
			},
			want: `"进度 10%`,
		},
		{
			name: "context_summary",
			item: &defs.Item{
				Kind:           defs.ItemContextSummary,
				ContextSummary: &defs.ContextSummary{Text: "  摘要正文\n后文"},
			},
			want: "摘要正文",
		},
		{
			name: "model_error",
			item: &defs.Item{
				Kind:       defs.ItemModelError,
				ModelError: &defs.ModelError{Message: "rate limited\x1b[0m"},
			},
			want: "rate limited",
		},
		{
			name: "model_change",
			item: modelChange,
			want: "切换到 b/y",
		},
		{
			name: "turn_interrupted",
			item: &defs.Item{Kind: defs.ItemTurnInterrupted},
			want: "已中断",
		},
		{
			name: "executors_change",
			item: executorsChange,
			want: "Executor 已变更：默认 gpu:/data · 移除 openvaala",
		},
		// 只显示第一行：先截到前 512 字节（不切断字符）再清理，超长正文不在每一帧整段处理。
		{
			name: "long_text_cut_before_first_line",
			item: userItem("i1", strings.Repeat("长", 1000)),
			want: strings.Repeat("长", 170),
		},
		{
			name: "long_tail_after_first_line",
			item: userItem("i1", "第一行\n"+strings.Repeat("尾", 100000)),
			want: "第一行",
		},
	}
	// 3. 逐项比较预览内容。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getItemPreview(tt.item, newSessionStatuses("s1"), false); got != tt.want {
				t.Errorf("getItemPreview() = %q, want %q", got, tt.want)
			}
		})
	}
}

// getPanelBodyLines 返回去掉边框和首尾空白的面板正文行。
func getPanelBodyLines(view string) []string {
	// 1. 选取边框内的行，移除左右边框和空白。
	var lines []string
	for line := range strings.SplitSeq(view, "\n") {
		if body, found := strings.CutPrefix(line, "│ "); found {
			lines = append(lines, strings.TrimSpace(strings.TrimSuffix(strings.TrimRight(body, " "), "│")))
		}
	}
	return lines
}
