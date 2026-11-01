package tui

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/VaalaCat/openvaala/pkg/storage"
	"github.com/charmbracelet/x/ansi"
)

// TestTranscript 检查历史、流式回复、推理、轮次和同步后的显示。
func TestTranscript(t *testing.T) {
	// 1. 列出已保存内容、增量、中断、重连和 Turn 分隔线场景。
	tests := []struct {
		name string
		run  func(t *testing.T, tr *transcript)
	}{
		{
			name: "history_user_input_and_reply",
			run: func(t *testing.T, tr *transcript) {
				// 1. 保存用户输入和两段回复，比较历史行、输入列表和已保存状态。
				printed := tr.handleItemsSaved(logItems(userItem("u1", "你好"), replyItem("r1", "第一段\n\n第二段")))
				requirePrinted(t, printed, "› 你好", "", "● 第一段", "", "  第二段", "")
				if !slices.Equal(tr.getUserInputs(), []string{"你好"}) || !tr.isItemSaved("r1") {
					t.Fatalf("inputs = %v, saved r1 = %v", tr.getUserInputs(), tr.isItemSaved("r1"))
				}
			},
		},
		{
			name: "reply_streams_block_by_block",
			run: func(t *testing.T, tr *transcript) {
				// 1. 逐次发送增量、完成和保存，比较稳定块与实时尾部。
				requirePrinted(t, tr.handleItemChanged(itemStarted(0, replyItem("r1", ""))))
				requirePrinted(t, tr.handleItemChanged(itemDelta(0, "第一段\n\n第")))
				requireLive(t, tr, "● 第一段", "", "  第")
				requirePrinted(t, tr.handleItemChanged(itemDelta(0, "二段\n")), "● 第一段", "")
				requireLive(t, tr, "  第二段")
				requirePrinted(t, tr.handleItemChanged(itemCompleted(0, replyItem("r1", "第一段\n\n第二段\n"))), "  第二段", "")
				requireLive(t, tr)
				requirePrinted(t, tr.handleItemsSaved(logItems(replyItem("r1", "第一段\n\n第二段\n"))))
			},
		},
		{
			name: "interrupted_after_print_marks_unsaved",
			run: func(t *testing.T, tr *transcript) {
				// 1. 先写出首段再中断，检查未保存提示和清空的尾部。
				tr.handleItemChanged(itemStarted(0, replyItem("r1", "")))
				requirePrinted(t, tr.handleItemChanged(itemDelta(0, "第一段\n\n第二\n")), "● 第一段", "")
				requirePrinted(t, tr.handleItemChanged(itemInterrupted(0)), "已中断 · 以上内容未保存", "")
				requireLive(t, tr)
			},
		},
		{
			name: "interrupted_before_print_only_clears_tail",
			run: func(t *testing.T, tr *transcript) {
				// 1. 在首段定稿前中断，检查没有历史提示且尾部清空。
				tr.handleItemChanged(itemStarted(0, replyItem("r1", "")))
				tr.handleItemChanged(itemDelta(0, "半句"))
				requirePrinted(t, tr.handleItemChanged(itemInterrupted(0)))
				requireLive(t, tr)
			},
		},
		{
			name: "duplicate_ordinal_skipped",
			run: func(t *testing.T, tr *transcript) {
				// 1. 重复保存同一流水号，检查只写出一次。
				items := logItems(userItem("u1", "一次"))
				requirePrinted(t, tr.handleItemsSaved(items), "› 一次", "")
				requirePrinted(t, tr.handleItemsSaved(items))
			},
		},
		{
			name: "subagent_notification_and_agent_message",
			run: func(t *testing.T, tr *transcript) {
				// 1. 同步子 Agent 的名字，保存结果通知和普通消息并检查最近结果。
				tr.statuses.handleSynced([]runtime.SessionRuntimeInfo{newRootInfo(""), newChildInfo("s2", "检查", "")})
				notification := agentItem("a1", "s2", "全部通过\n细节", &defs.SubagentNotification{
					SourceTurnID: "t9",
					Status:       defs.TurnStatusCompleted,
				})
				failed := agentItem("a2", "s2", "出错了", &defs.SubagentNotification{
					SourceTurnID: "t10",
					Status:       defs.TurnStatusFailed,
				})
				plain := agentItem("a3", "s2", "看一下这个", nil)
				requirePrinted(t, tr.handleItemsSaved(logItems(notification, failed, plain)),
					"← 子 Agent 检查 完成 全部通过", "", "← 子 Agent 检查 失败 出错了", "", "← 检查：看一下这个", "")
				if tr.getSubagentResult("s2") != "出错了" {
					t.Fatalf("latest result = %q", tr.getSubagentResult("s2"))
				}
			},
		},
		{
			name: "other_items",
			run: func(t *testing.T, tr *transcript) {
				// 1. 准备摘要、错误、模型切换和推理内容，检查各类提示。
				tr.modelNames[defs.ModelRef{
					ProviderID: "openai",
					ModelID:    "gpt-y",
				}] = "GPT Y"
				items := logItems(
					&defs.Item{
						ID:        "x1",
						TurnID:    "t1",
						Kind:      defs.ItemReasoning,
						Reasoning: &defs.Reasoning{Summary: []string{"分析", "问题"}},
					},
					&defs.Item{
						ID:             "x2",
						Kind:           defs.ItemContextSummary,
						ContextSummary: &defs.ContextSummary{Text: "摘要"},
					},
					&defs.Item{
						ID:         "x3",
						TurnID:     "t1",
						Kind:       defs.ItemModelError,
						ModelError: &defs.ModelError{Message: "rate limited"},
					},
					&defs.Item{
						ID:     "x4",
						TurnID: "t2",
						Kind:   defs.ItemModelChange,
						ModelChange: &defs.ModelChange{
							PreviousModel: defs.ModelRef{
								ProviderID: "openai",
								ModelID:    "gpt-x",
							},
							Model: defs.ModelRef{
								ProviderID: "openai",
								ModelID:    "gpt-y",
							},
						},
					},
					&defs.Item{
						ID:     "x5",
						TurnID: "t3",
						Kind:   defs.ItemModelChange,
						ModelChange: &defs.ModelChange{
							PreviousModel: defs.ModelRef{
								ProviderID: "openai",
								ModelID:    "gpt-y",
							},
							Model: defs.ModelRef{
								ProviderID: "local",
								ModelID:    "qwen",
							},
						},
					},
					&defs.Item{
						ID:          "x6",
						Kind:        defs.ItemSessionFork,
						SessionFork: &defs.SessionFork{SourceSessionID: "s0"},
					},
					&defs.Item{
						ID:        "x7",
						TurnID:    "t3",
						Kind:      defs.ItemReasoning,
						Reasoning: &defs.Reasoning{},
					},
				)
				// 2. 检查 t2、t3 首条内容前的分隔线；无 Turn ID 的摘要不增加分隔线。
				divider := strings.Repeat("─", 60)
				requirePrinted(t, tr.handleItemsSaved(items),
					"∴ 分析问题", "", "≡ 上下文已压缩", "", "✗ 模型错误：rate limited", "", divider, "", "已切换到 GPT Y", "", divider, "", "已切换到 local/qwen", "")
			},
		},
		{
			name: "turn_interrupted_line",
			run: func(t *testing.T, tr *transcript) {
				// 1. 保存用户输入和 Turn 中断记录，检查中断提示。
				items := logItems(userItem("u1", "做 A"), &defs.Item{
					ID:     "x1",
					TurnID: "t1",
					Kind:   defs.ItemTurnInterrupted,
				})
				requirePrinted(t, tr.handleItemsSaved(items), "› 做 A", "", "已中断", "")
			},
		},
		{
			name: "executors_change_line",
			run: func(t *testing.T, tr *transcript) {
				// 1. 保存 Executor 变更记录，检查默认项和移除项提示。
				items := logItems(
					&defs.Item{
						ID:     "x1",
						TurnID: "t1",
						Kind:   defs.ItemExecutorsChange,
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
					},
				)
				requirePrinted(t, tr.handleItemsSaved(items), "Executor 已变更：默认 gpu:/data · 移除 openvaala", "")
			},
		},
		{
			name: "turn_failed_writes_error",
			run: func(t *testing.T, tr *transcript) {
				// 1. 依次结束带原因和无原因的失败 Turn，检查错误文字与分隔线。
				tr.handleTurnStarted("t1")
				requirePrinted(t, tr.handleTurnFinished(&runtimeclient.TurnFinished{
					TurnID: "t1",
					Status: defs.TurnStatusFailed,
					Error:  "boom",
				}),
					"✗ Turn 失败：boom", "")
				requirePrinted(t, tr.handleTurnFinished(&runtimeclient.TurnFinished{
					TurnID: "t2",
					Status: defs.TurnStatusInterrupted,
				}))
				// 2. 检查新轮次失败提示前的分隔线和默认错误文字。
				requirePrinted(t, tr.handleTurnFinished(&runtimeclient.TurnFinished{
					TurnID: "t3",
					Status: defs.TurnStatusFailed,
				}),
					strings.Repeat("─", 60), "", "✗ Turn 失败：未知原因", "")
			},
		},
		{
			name: "sync_marks_unsaved_reply",
			run: func(t *testing.T, tr *transcript) {
				// 1. 生成部分回复后同步没有生成中 Item 的结果，检查未保存标记。
				tr.handleItemChanged(itemStarted(0, replyItem("r1", "")))
				tr.handleItemChanged(itemDelta(0, "第一段\n\n第二\n"))
				requirePrinted(t, tr.handleSessionSynced(&runtimeclient.SessionSynced{}, ""), "已中断 · 以上内容未保存", "")
				requireLive(t, tr)
			},
		},
		{
			name: "sync_saved_reply_writes_rest_once",
			run: func(t *testing.T, tr *transcript) {
				// 1. 生成部分回复后同步已保存全文，检查只补写剩余内容。
				tr.handleItemChanged(itemStarted(0, replyItem("r1", "")))
				tr.handleItemChanged(itemDelta(0, "第一段\n\n第二\n"))
				saved := &runtimeclient.SessionSynced{Items: logItems(replyItem("r1", "第一段\n\n第二段完整\n"))}
				requirePrinted(t, tr.handleSessionSynced(saved, ""), "  第二段完整", "")
			},
		},
		{
			name: "sync_snapshot_continues_reply",
			run: func(t *testing.T, tr *transcript) {
				// 1. 同步带有仍在生成的回复的结果，再追加增量并检查定稿与尾部。
				tr.handleItemChanged(itemStarted(0, replyItem("r1", "")))
				tr.handleItemChanged(itemDelta(0, "第一段\n\n第二\n"))
				snapshot := &runtimeclient.SessionSynced{StreamingItems: []runtimeprotocol.StreamingItem{{
					ItemIndex: 2,
					Item:      replyItem("r1", "第一段\n\n第二段\n\n第三"),
				}}}
				requirePrinted(t, tr.handleSessionSynced(snapshot, "t1"))
				requireLive(t, tr, "  第二段", "", "  第三")
				requirePrinted(t, tr.handleItemChanged(itemDelta(2, "段\n")), "  第二段", "")
			},
		},
		{
			name: "reasoning_flag",
			run: func(t *testing.T, tr *transcript) {
				// 1. 依次交付推理开始和结束，检查思考状态。
				reasoning := &defs.Item{
					ID:          "x1",
					TurnID:      "t1",
					ModelCallID: "m1",
					Kind:        defs.ItemReasoning,
					Reasoning:   &defs.Reasoning{},
				}
				tr.handleItemChanged(itemStarted(1, reasoning))
				if !tr.isReasoning() {
					t.Fatal("reasoning not reported")
				}
				tr.handleItemChanged(itemCompleted(1, reasoning))
				if tr.isReasoning() {
					t.Fatal("finished reasoning still reported")
				}
			},
		},
		{
			// 已完成推理同步后不显示“思考中”，回复开始时写出推理摘要，保存后移除生成中记录。
			name: "sync_snapshot_completed_reasoning_is_not_thinking",
			run: func(t *testing.T, tr *transcript) {
				// 1. 同步已完成推理和非空回复，检查思考状态、推理输出和保存后移除。
				reasoning := &defs.Item{
					ID:          "x1",
					TurnID:      "t1",
					ModelCallID: "m1",
					Kind:        defs.ItemReasoning,
					Reasoning: &defs.Reasoning{
						Status:  defs.ReasoningCompleted,
						Summary: []string{"分析"},
					},
				}
				snapshot := &runtimeclient.SessionSynced{StreamingItems: []runtimeprotocol.StreamingItem{
					{
						ItemIndex: 1,
						Item:      reasoning,
					}, {
						ItemIndex: 2,
						Item:      replyItem("r1", "第一段\n\n第二"),
					}}}
				requirePrinted(t, tr.handleSessionSynced(snapshot, "t1"), "∴ 分析", "")
				if tr.isReasoning() {
					t.Fatal("completed reasoning in the snapshot reported as thinking")
				}
				requireLive(t, tr, "● 第一段", "", "  第二")
				requirePrinted(t, tr.handleItemsSaved(logItems(reasoning, replyItem("r1", "第一段\n\n第二段\n"))),
					"● 第一段", "", "  第二段", "")
				if len(tr.streamingItems) != 0 {
					t.Fatalf("streaming items after save = %d", len(tr.streamingItems))
				}
				requireLive(t, tr)
			},
		},
		{
			// 生成中推理同步后显示“思考中”，保存结束状态后移除生成中记录。
			name: "sync_snapshot_running_reasoning_is_thinking_until_saved",
			run: func(t *testing.T, tr *transcript) {
				// 1. 同步生成中推理，再保存结束状态，检查思考状态清除。
				running := &defs.Item{
					ID:          "x1",
					TurnID:      "t1",
					ModelCallID: "m1",
					Kind:        defs.ItemReasoning,
					Reasoning:   &defs.Reasoning{Summary: []string{"分析"}},
				}
				tr.handleSessionSynced(&runtimeclient.SessionSynced{StreamingItems: []runtimeprotocol.StreamingItem{{
					ItemIndex: 1,
					Item:      running,
				}}}, "t1")
				if !tr.isReasoning() {
					t.Fatal("running reasoning in the snapshot not reported")
				}
				saved := &defs.Item{
					ID:          "x1",
					TurnID:      "t1",
					ModelCallID: "m1",
					Kind:        defs.ItemReasoning,
					Reasoning: &defs.Reasoning{
						Status:  defs.ReasoningInterrupted,
						Summary: []string{"分析"},
					},
				}
				tr.handleItemsSaved(logItems(saved))
				if tr.isReasoning() || len(tr.streamingItems) != 0 {
					t.Fatalf("reasoning = %v, streaming items = %d after save", tr.isReasoning(), len(tr.streamingItems))
				}
			},
		},
		{
			name: "width_change_rerenders_tail",
			run: func(t *testing.T, tr *transcript) {
				// 1. 生成长回复后缩窄宽度，检查实时尾部每行未超宽。
				tr.handleItemChanged(itemStarted(0, replyItem("r1", "")))
				tr.handleItemChanged(itemDelta(0, strings.Repeat("很长的句子", 10)))
				tr.setRenderer(mustNewMarkdownRenderer(markdownStyleNoTTY, 20), 10)
				for _, line := range tr.getLiveLines("◐") {
					if width := ansi.StringWidth(line); width > 20 {
						t.Fatalf("live line %q width %d after resize", line, width)
					}
				}
			},
		},
		{
			name: "history_entries_keep_first_seen_order",
			run: func(t *testing.T, tr *transcript) {
				// 1. 交错交付输入、推理、工具和回复，检查历史顺序及完整原文。
				tr.handleItemsSaved(logItems(userItem("u1", "看看")))
				tr.handleTurnStarted("t1")
				tr.handleItemChanged(itemStarted(0, reasoningItem("x1", "想", defs.ReasoningCompleted)))
				tr.handleItemChanged(itemStarted(1, toolCallItem("c1", "call-1", "bash", `{"command":"ls"}`)))
				tr.handleItemChanged(itemStarted(2, toolResultItem("c2", "call-1", nil)))
				tr.handleItemChanged(itemStarted(3, replyItem("r1", "")))
				tr.handleItemChanged(itemDelta(3, "第一段\n\n"))
				tr.handleItemChanged(itemDelta(3, "第二段"))
				tr.handleTurnFinished(&runtimeclient.TurnFinished{
					TurnID: "t1",
					Status: defs.TurnStatusFailed,
					Error:  "boom",
				})
				var kinds []historyEntryKind
				for _, entry := range tr.history {
					kinds = append(kinds, entry.kind)
				}
				if want := []historyEntryKind{historyUserInput, historyStepGroup, historyReply, historyLine}; !slices.Equal(kinds, want) {
					t.Fatalf("kinds = %v, want %v", kinds, want)
				}
				if got := tr.history[2].reply.source.String(); got != "第一段\n\n第二段" {
					t.Fatalf("reply source = %q", got)
				}
				if line := tr.history[3].line; len(line) != 1 || line[0].text != "✗ Turn 失败：boom" {
					t.Fatalf("line = %+v", line)
				}
			},
		},
		{
			name: "reasoning_only_group_lines_are_not_history_lines",
			run: func(t *testing.T, tr *transcript) {
				// 1. 保存纯推理组和摘要，检查历史单元各自的种类。
				tr.handleItemsSaved(logItems(reasoningItem("x1", "想", defs.ReasoningCompleted),
					&defs.Item{
						ID:             "x2",
						TurnID:         "t1",
						Kind:           defs.ItemContextSummary,
						ContextSummary: &defs.ContextSummary{Text: "摘要"},
					}))
				if len(tr.history) != 2 || tr.history[0].kind != historyStepGroup || tr.history[1].kind != historyLine {
					t.Fatalf("history = %+v", tr.history)
				}
			},
		},
		{
			// 摘要只在保存后显示压缩提示，生成中和中断未保存时不显示。
			name: "context_summary_shows_only_after_saved",
			run: func(t *testing.T, tr *transcript) {
				// 1. 发送摘要开始、完成、保存和中断事件，比较输出与生成中记录。
				started := &defs.Item{
					ID:             "x1",
					TurnID:         "t1",
					Kind:           defs.ItemContextSummary,
					ContextSummary: &defs.ContextSummary{CoveredThroughItemID: "u1"},
				}
				completed := &defs.Item{
					ID:     "x1",
					TurnID: "t1",
					Kind:   defs.ItemContextSummary,
					ContextSummary: &defs.ContextSummary{
						Text:                 "摘要",
						CoveredThroughItemID: "u1",
					},
				}
				requirePrinted(t, tr.handleItemChanged(itemStarted(0, started)))
				requireLive(t, tr)
				requirePrinted(t, tr.handleItemChanged(itemCompleted(0, completed)))
				requirePrinted(t, tr.handleItemsSaved(logItems(completed)), "≡ 上下文已压缩", "")
				requirePrinted(t, tr.handleItemChanged(itemStarted(1, started)))
				requirePrinted(t, tr.handleItemChanged(itemInterrupted(1)))
				requireLive(t, tr)
				if len(tr.streamingItems) != 0 {
					t.Fatalf("streaming items = %+v, want none after the summary finished or was interrupted", tr.streamingItems)
				}
			},
		},
		{
			name: "turn_divider_between_turns",
			run: func(t *testing.T, tr *transcript) {
				// 1. 保存多轮输入和插话，检查分隔线只出现在轮次之间。
				items := logItems(
					userItem("u1", "第一轮"),
					replyItem("r1", "好"),
					withTurn(userItem("u2", "插话"), "t1"),
					withTurn(userItem("u3", "第二轮"), "t2"),
					withTurn(replyItem("r2", "也好"), "t2"),
				)
				divider := strings.Repeat("─", 60)
				requirePrinted(t, tr.handleItemsSaved(items),
					"› 第一轮", "", "● 好", "", "› 插话", "", divider, "", "› 第二轮", "", "● 也好", "")
				var kinds []historyEntryKind
				for _, entry := range tr.history {
					kinds = append(kinds, entry.kind)
				}
				want := []historyEntryKind{historyUserInput, historyReply, historyUserInput, historyTurnDivider, historyUserInput, historyReply}
				if !slices.Equal(kinds, want) {
					t.Fatalf("kinds = %v, want %v", kinds, want)
				}
			},
		},
		{
			name: "items_without_turn_do_not_trigger_divider",
			run: func(t *testing.T, tr *transcript) {
				// 1. 在同一轮输入与回复之间保存无 Turn 的摘要，检查它不增加分隔线。
				items := logItems(userItem("u1", "第一轮"),
					&defs.Item{
						ID:             "x1",
						Kind:           defs.ItemContextSummary,
						ContextSummary: &defs.ContextSummary{Text: "摘要"},
					},
					replyItem("r1", "好"))
				requirePrinted(t, tr.handleItemsSaved(items), "› 第一轮", "", "≡ 上下文已压缩", "", "● 好", "")
			},
		},
		{
			// 新轮次回复登记时写一次分隔线，增量、完成和保存不重复写。
			name: "reply_opening_a_turn_writes_divider_once",
			run: func(t *testing.T, tr *transcript) {
				// 1. 以回复开启新轮次，检查开始、增量、完成和保存合计只写一次分隔线。
				tr.handleItemsSaved(logItems(userItem("u1", "第一轮"), replyItem("r1", "好")))
				tr.handleTurnStarted("t2")
				divider := strings.Repeat("─", 60)
				requirePrinted(t, tr.handleItemChanged(itemStarted(0, withTurn(replyItem("r2", ""), "t2"))), divider, "")
				requirePrinted(t, tr.handleItemChanged(itemDelta(0, "第一段\n\n第")))
				requirePrinted(t, tr.handleItemChanged(itemDelta(0, "二段\n")), "● 第一段", "")
				requirePrinted(t, tr.handleItemChanged(itemCompleted(0, withTurn(replyItem("r2", "第一段\n\n第二段"), "t2"))), "  第二段", "")
				requirePrinted(t, tr.handleItemsSaved([]runtimeprotocol.LoggedItem{{
					Ordinal: 3,
					Item:    withTurn(replyItem("r2", "第一段\n\n第二段"), "t2"),
				}}))
			},
		},
		{
			// 前一轮的步骤组先写出，之后是分隔线和新轮次回复。
			name: "previous_turn_group_is_written_before_divider",
			run: func(t *testing.T, tr *transcript) {
				// 1. 保留前一轮工具组，再开启新轮回复，检查工具组先于分隔线写出。
				tr.handleItemsSaved(logItems(userItem("u1", "第一轮"),
					toolCallItem("c1", "call-1", "read_file", `{"path":"a"}`), toolResultItem("c2", "call-1", nil)))
				requirePrinted(t, tr.handleItemChanged(itemStarted(0, withTurn(replyItem("r2", "读完了\n"), "t2"))),
					"✓ 工具 1 次", "  └─ read_file 1 次", "", strings.Repeat("─", 60), "")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, newTestTranscript())
		})
	}
}

// hookRunItem 创建指定 Turn 的 hook 运行 Item。
func hookRunItem(id defs.ItemID, turnID defs.TurnID, run *defs.HookRun) *defs.Item {
	// 1. 将运行 ID 设为 Item ID，再组装 hook_run Item。
	run.ID = defs.HookRunID(id)
	return &defs.Item{
		ID:      id,
		TurnID:  turnID,
		Kind:    defs.ItemHookRun,
		HookRun: run,
	}
}

// TestTranscriptHookRun 检查 hook 记录的提示、工具名和历史回放。
func TestTranscriptHookRun(t *testing.T) {
	// 1. 构造各类 hook 结果和期望历史行。
	preToolUse := func(status defs.HookRunStatus) *defs.HookRun {
		return &defs.HookRun{
			Event:      defs.HookEventPreToolUse,
			CallID:     "call-1",
			ExecutorID: "local",
			Handler:    "./check.sh",
			Status:     status,
		}
	}
	tests := []struct {
		name  string
		items []*defs.Item
		// want 是依次处理 items 时写进 scrollback 的全部行；Turn t1 在处理之前已经开始。
		want []string
	}{
		{
			// 仅补模型上下文的 hook 不显示主视图提示，前后工具仍属于同一组。
			name: "completed_without_system_message_is_hidden_and_keeps_step_group",
			items: []*defs.Item{
				toolCallItem("c1", "call-1", "bash", `{"command":"ls"}`),
				hookRunItem("h1", "t1", &defs.HookRun{
					Event:             defs.HookEventPreToolUse,
					CallID:            "call-1",
					Status:            defs.HookRunCompleted,
					AdditionalContext: "注意",
				}),
				toolResultItem("c2", "call-1", nil),
				toolCallItem("c3", "call-2", "bash", `{"command":"pwd"}`),
				hookRunItem("h2", "t1", &defs.HookRun{
					Event:             defs.HookEventPostToolUse,
					CallID:            "call-2",
					Status:            defs.HookRunCompleted,
					AdditionalContext: "好",
				}),
				toolResultItem("c4", "call-2", nil),
				replyItem("r1", "完成\n"),
			},
			want: []string{"✓ 工具 2 次", "  └─ bash 2 次", "", "● 完成", ""},
		},
		{
			name: "blocked_pre_tool_use_names_the_tool_of_the_same_call",
			items: []*defs.Item{
				toolCallItem("c1", "call-1", "bash", `{"command":"rm -rf /"}`),
				hookRunItem("h1", "t1", withBlockReason(preToolUse(defs.HookRunBlocked), "不要用 rm -rf")),
			},
			want: []string{"hook PreToolUse 拦下 bash：不要用 rm -rf", ""},
		},
		{
			name: "blocked_without_known_call_has_no_tool_name",
			items: []*defs.Item{
				hookRunItem("h1", "t1", withBlockReason(preToolUse(defs.HookRunBlocked), "不要用 rm -rf")),
			},
			want: []string{"hook PreToolUse 拦下：不要用 rm -rf", ""},
		},
		{
			name: "blocked_prompt_and_stop_have_no_tool_name",
			items: []*defs.Item{
				hookRunItem("h1", "t1", withBlockReason(&defs.HookRun{
					Event:  defs.HookEventUserPromptSubmit,
					Status: defs.HookRunBlocked,
				}, "缺少工单号")),
				hookRunItem("h2", "t1", withBlockReason(&defs.HookRun{
					Event:  defs.HookEventSubagentStop,
					Status: defs.HookRunBlocked,
				}, "还没跑测试")),
				hookRunItem("h3", "t1", &defs.HookRun{
					Event:  defs.HookEventStop,
					Status: defs.HookRunBlocked,
				}),
			},
			want: []string{"hook UserPromptSubmit 拦下：缺少工单号", "", "hook SubagentStop 拦下：还没跑测试", "", "hook Stop 拦下", ""},
		},
		{
			name: "failed_shows_error_message",
			items: []*defs.Item{
				toolCallItem("c1", "call-1", "bash", `{}`),
				hookRunItem("h1", "t1", &defs.HookRun{
					Event:        defs.HookEventPostToolUse,
					CallID:       "call-1",
					Status:       defs.HookRunFailed,
					ErrorMessage: "timed out after 60s",
				}),
			},
			want: []string{"hook PostToolUse 出错：timed out after 60s", ""},
		},
		{
			name: "stopped_shows_stop_reason_when_given",
			items: []*defs.Item{
				hookRunItem("h1", "t1", &defs.HookRun{
					Event:      defs.HookEventStop,
					Status:     defs.HookRunStopped,
					StopReason: "预算用完",
				}),
				hookRunItem("h2", "t1", &defs.HookRun{
					Event:  defs.HookEventSessionStart,
					Status: defs.HookRunStopped,
				}),
			},
			want: []string{"hook Stop 结束本轮：预算用完", "", "hook SessionStart 结束本轮", ""},
		},
		{
			name: "completed_with_system_message_shows_it",
			items: []*defs.Item{
				hookRunItem("h1", "t1", &defs.HookRun{
					Event:         defs.HookEventSessionStart,
					Status:        defs.HookRunCompleted,
					SystemMessage: "已加载 3 条记忆",
				}),
			},
			want: []string{"hook SessionStart：已加载 3 条记忆", ""},
		},
		{
			// 状态提示和系统消息各占一行，状态在前。
			name: "blocked_with_system_message_shows_two_lines",
			items: []*defs.Item{
				toolCallItem("c1", "call-1", "bash", `{}`),
				hookRunItem("h1", "t1", withSystemMessage(withBlockReason(preToolUse(defs.HookRunBlocked), "不要用 rm -rf"), "已记入审计日志")),
			},
			want: []string{"hook PreToolUse 拦下 bash：不要用 rm -rf", "", "hook PreToolUse：已记入审计日志", ""},
		},
		{
			// 原因里的换行和终端控制先处理再显示：换行写成空格，控制序列去掉。
			name: "reason_is_single_line_without_terminal_controls",
			items: []*defs.Item{
				hookRunItem("h1", "t1", withBlockReason(&defs.HookRun{
					Event:  defs.HookEventUserPromptSubmit,
					Status: defs.HookRunBlocked,
				}, "第一行\n\x1b]52;c;ZXZpbA==\x07第二行")),
			},
			want: []string{"hook UserPromptSubmit 拦下：第一行 第二行", ""},
		},
		{
			// 新轮次的输入被拦下时，分隔线写在拦截提示前。
			name: "blocked_prompt_of_new_turn_follows_divider",
			items: []*defs.Item{
				withTurn(replyItem("r1", "好"), "t0"),
				hookRunItem("h1", "t1", withBlockReason(&defs.HookRun{
					Event:  defs.HookEventUserPromptSubmit,
					Status: defs.HookRunBlocked,
				}, "缺少工单号")),
			},
			want: []string{"● 好", "", strings.Repeat("─", 60), "", "hook UserPromptSubmit 拦下：缺少工单号", ""},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 1. 开始 Turn 并逐条保存 Item，比较首次写出的历史行。
			tr := newTestTranscript()
			tr.handleTurnStarted("t1")
			var printed []string
			for _, item := range tt.items {
				printed = append(printed, tr.handleItemsSaved([]runtimeprotocol.LoggedItem{{
					Ordinal: tr.lastSavedOrdinal + 1,
					Item:    item,
				}})...)
			}
			requirePrinted(t, printed, tt.want...)
			// 2. 调整宽度后回放历史，比较提示行与首次输出。
			tr.setRenderer(mustNewMarkdownRenderer(markdownStyleNoTTY, 60), 10)
			requirePrinted(t, tr.getScrollbackLines(), tt.want...)
		})
	}
}

// withBlockReason 设置测试 hook 的拦截原因。
func withBlockReason(run *defs.HookRun, reason string) *defs.HookRun {
	// 1. 替换 BlockReason 并返回同一运行记录。
	run.BlockReason = reason
	return run
}

// withSystemMessage 设置测试 hook 的系统消息。
func withSystemMessage(run *defs.HookRun, message string) *defs.HookRun {
	// 1. 替换 SystemMessage 并返回同一运行记录。
	run.SystemMessage = message
	return run
}

// TestTranscriptHookRunStyles 检查 hook 错误提示和普通提示的颜色。
func TestTranscriptHookRunStyles(t *testing.T) {
	// 1. 创建有色样式，分别比较失败、拦截、结束和系统消息提示。
	colored := newTUIStyles("dark", nil)
	tests := []struct {
		name  string
		run   *defs.HookRun
		text  string
		style lipgloss.Style
	}{
		{
			name: "failed_warning",
			run: &defs.HookRun{
				Event:        defs.HookEventPostToolUse,
				Status:       defs.HookRunFailed,
				ErrorMessage: "boom",
			},
			text:  "hook PostToolUse 出错：boom",
			style: colored.warning,
		},
		{
			name: "blocked_faint",
			run: &defs.HookRun{
				Event:       defs.HookEventStop,
				Status:      defs.HookRunBlocked,
				BlockReason: "再想想",
			},
			text:  "hook Stop 拦下：再想想",
			style: colored.faint,
		},
		{
			name: "stopped_faint",
			run: &defs.HookRun{
				Event:      defs.HookEventStop,
				Status:     defs.HookRunStopped,
				StopReason: "够了",
			},
			text:  "hook Stop 结束本轮：够了",
			style: colored.faint,
		},
		{
			name: "system_message_faint",
			run: &defs.HookRun{
				Event:         defs.HookEventSessionStart,
				Status:        defs.HookRunCompleted,
				SystemMessage: "你好",
			},
			text:  "hook SessionStart：你好",
			style: colored.faint,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := newTranscript(mustNewMarkdownRenderer(markdownStyleNoTTY, 60), colored, newSessionStatuses("s1"), make(map[defs.ModelRef]string), 10,
				func() time.Time { return testNow }, func() bool { return false }, func() string { return "" })
			printed := tr.handleItemsSaved(logItems(hookRunItem("h1", "t1", tt.run)))
			if len(printed) != 2 || printed[0] != tt.style.Render(tt.text) {
				t.Fatalf("printed = %q, want %q", printed, tt.style.Render(tt.text))
			}
		})
	}
}

// TestTranscriptScrollbackReplay 检查不同宽度的历史回放和实时尾部。
func TestTranscriptScrollbackReplay(t *testing.T) {
	// 1. 分别准备生成中、已完成和被丢弃的回复场景。
	for _, tt := range []struct {
		name      string
		width     int
		finished  bool
		discarded bool
	}{
		{
			name:  "streaming_narrow",
			width: 20,
		},
		{
			name:  "streaming_wide",
			width: 80,
		},
		{
			name:     "completed",
			width:    20,
			finished: true,
		},
		{
			name:      "interrupted",
			width:     20,
			discarded: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// 1. 保存一条用户输入，生成一段回复，按场景完成或中断它。
			tr := newTestTranscript()
			tr.handleItemsSaved(logItems(userItem("u1", "保持输入")))
			tr.handleItemChanged(itemStarted(0, replyItem("r1", "")))
			tr.handleItemChanged(itemDelta(0, "定稿段\n\n实时尾部\n"))
			if tt.finished {
				tr.handleItemChanged(itemCompleted(0, replyItem("r1", "定稿段\n\n实时尾部\n")))
			}
			if tt.discarded {
				tr.handleItemChanged(itemInterrupted(0))
			}
			// 2. 修改宽度后重复回放，检查已定稿内容、实时尾部和行宽。
			tr.setRenderer(mustNewMarkdownRenderer(markdownStyleNoTTY, tt.width), 10)
			live := strings.Join(tr.getLiveLines(""), "\n")
			for range 2 {
				lines := tr.getScrollbackLines()
				text := ansi.Strip(strings.Join(lines, "\n"))
				for _, marker := range []string{"保持输入", "定稿段"} {
					if strings.Count(text, marker) != 1 {
						t.Fatalf("replay duplicated or lost %q: %q", marker, text)
					}
				}
				if strings.Contains(text, "实时尾部") != tt.finished {
					t.Fatalf("replay includes wrong reply prefix: %q", text)
				}
				if strings.Join(tr.getLiveLines(""), "\n") != live {
					t.Fatal("replay changed the streaming tail")
				}
				for _, line := range lines {
					if ansi.StringWidth(line) > tt.width {
						t.Fatalf("replayed line exceeds width %d: %q", tt.width, line)
					}
				}
			}
		})
	}
}

// TestTranscriptStripsTerminalControls 检查回复、推理和工具文字显示前的终端控制清理。
func TestTranscriptStripsTerminalControls(t *testing.T) {
	// 1. 准备含剪贴板序列、未复位样式、未完成转义和回车的文字。
	arguments := func(name, value string) string {
		encoded, err := json.Marshal(map[string]string{name: value})
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	controls := []struct {
		name string
		text string
	}{
		{
			name: "osc52_clipboard",
			text: "前\x1b]52;c;ZXZpbA==\x07后",
		},
		{
			name: "style_without_reset",
			text: "前\x1b[31m后",
		},
		{
			name: "unterminated_csi",
			text: "前后\x1b[3",
		},
		{
			name: "carriage_return",
			text: "前\r后",
		},
	}
	sources := []struct {
		name string
		// emit 把 text 放进一种来源，返回写进 scrollback 与实时区域的全部行。
		emit func(tr *transcript, text string) []string
	}{
		{
			name: "reply",
			emit: func(tr *transcript, text string) []string {
				lines := tr.handleItemChanged(itemStarted(0, replyItem("r1", "")))
				lines = append(lines, tr.handleItemChanged(itemDelta(0, text))...)
				lines = append(lines, tr.getLiveLines("◐")...)
				return append(lines, tr.handleItemsSaved(logItems(replyItem("r1", text)))...)
			},
		},
		{
			name: "reasoning_summary",
			emit: func(tr *transcript, text string) []string {
				items := logItems(&defs.Item{
					ID:        "x1",
					TurnID:    "t1",
					Kind:      defs.ItemReasoning,
					Reasoning: &defs.Reasoning{Summary: []string{text}},
				})
				return tr.handleSessionSynced(&runtimeclient.SessionSynced{Items: items}, "")
			},
		},
		{
			name: "tool_argument_on_running_line",
			emit: func(tr *transcript, text string) []string {
				tr.handleTurnStarted("t1")
				lines := tr.handleItemChanged(itemStarted(0, toolCallItem("x1", "call-1", "bash", arguments("command", text))))
				return append(lines, tr.getLiveLines("◐")...)
			},
		},
		{
			name: "tool_argument_and_error_on_failed_line",
			emit: func(tr *transcript, text string) []string {
				tr.handleTurnStarted("t1")
				lines := tr.handleItemsSaved(logItems(toolCallItem("x1", "call-1", "read_file", arguments("path", text)),
					toolResultItem("x2", "call-1", &defs.ToolError{
						Code:    "io",
						Message: text,
					})))
				return append(lines, tr.handleTurnFinished(&runtimeclient.TurnFinished{
					TurnID: "t1",
					Status: defs.TurnStatusCompleted,
				})...)
			},
		},
		{
			name: "tool_name_on_running_line",
			emit: func(tr *transcript, text string) []string {
				tr.handleTurnStarted("t1")
				lines := tr.handleItemChanged(itemStarted(0, toolCallItem("x1", "call-1", text, `{}`)))
				return append(lines, tr.getLiveLines("◐")...)
			},
		},
		{
			name: "subagent_task_name",
			emit: func(tr *transcript, text string) []string {
				tr.statuses.handleSynced([]runtime.SessionRuntimeInfo{newRootInfo(""), newChildInfo("s2", text, "")})
				return tr.handleItemsSaved(logItems(agentItem("a1", "s2", "全部通过", &defs.SubagentNotification{
					SourceTurnID: "t9",
					Status:       defs.TurnStatusCompleted,
				})))
			},
		},
	}
	// 2. 将每种控制文字放入不同来源，检查可见文字保留且没有终端控制泄漏。
	for _, source := range sources {
		for _, control := range controls {
			t.Run(source.name+"/"+control.name, func(t *testing.T) {
				output := strings.Join(source.emit(newTestTranscript(), control.text), "\n")
				if strings.ContainsAny(output, "\x1b\r\x07") {
					t.Fatalf("terminal controls reached the output: %q", output)
				}
				if !strings.Contains(output, "前") || !strings.Contains(output, "后") {
					t.Fatalf("visible text lost: %q", output)
				}
			})
		}
	}
}

// TestSubagentNotificationStyles 检查子 Agent 通知只给状态词添加状态颜色。
func TestSubagentNotificationStyles(t *testing.T) {
	// 1. 创建有色和无色样式，列出完成、失败和中断的通知。
	plain, colored := newTUIStyles(markdownStyleNoTTY, nil), newTUIStyles("dark", nil)
	tests := []struct {
		name   string
		status defs.TurnStatus
		text   string
		// wantPlain 是不上色时的一行；want 是有颜色时的一行：只有状态词上色，其余暗色。
		wantPlain, want string
	}{
		{
			name:      "completed_success",
			status:    defs.TurnStatusCompleted,
			text:      "全部通过\n细节",
			wantPlain: "← 子 Agent 检查 完成 全部通过",
			want:      colored.faint.Render("← 子 Agent 检查 ") + colored.success.Render("完成") + colored.faint.Render(" 全部通过"),
		},
		{
			name:      "failed_failure",
			status:    defs.TurnStatusFailed,
			text:      "出错了",
			wantPlain: "← 子 Agent 检查 失败 出错了",
			want:      colored.faint.Render("← 子 Agent 检查 ") + colored.failure.Render("失败") + colored.faint.Render(" 出错了"),
		},
		{
			name:      "interrupted_warning_without_body",
			status:    defs.TurnStatusInterrupted,
			wantPlain: "← 子 Agent 检查 中断",
			want:      colored.faint.Render("← 子 Agent 检查 ") + colored.warning.Render("中断"),
		},
	}
	// notify 在宽度为 width 的 transcript 里处理一条子 Agent 通知，返回写进 scrollback 的一行与展开视图重新渲染的各行。
	notify := func(styles tuiStyles, width int, status defs.TurnStatus, text string) (string, []string) {
		tr := newTranscript(mustNewMarkdownRenderer(markdownStyleNoTTY, width), styles, newSessionStatuses("s1"), make(map[defs.ModelRef]string), 10,
			func() time.Time { return testNow }, func() bool { return false }, func() string { return "" })
		tr.statuses.handleSynced([]runtime.SessionRuntimeInfo{newRootInfo(""), newChildInfo("s2", "检查", "")})
		printed := tr.handleItemsSaved(logItems(agentItem("a1", "s2", text, &defs.SubagentNotification{
			SourceTurnID: "t9",
			Status:       status,
		})))
		return printed[0], tr.formatExpandedEntry(tr.history[len(tr.history)-1])
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 1. 比较 60 列下无色、有色的通知行，以及有色时展开视图的那一行。
			if got, _ := notify(plain, 60, tt.status, tt.text); got != tt.wantPlain {
				t.Fatalf("plain printed = %q, want %q", got, tt.wantPlain)
			}
			got, expanded := notify(colored, 60, tt.status, tt.text)
			if got != tt.want || len(expanded) != 1 || expanded[0] != tt.want {
				t.Fatalf("printed = %q, expanded = %q, want %q", got, expanded, tt.want)
			}
			// 2. 比较窄屏展开视图，有色和无色文字的折行相同。
			_, plainRows := notify(plain, 12, tt.status, tt.text)
			_, coloredRows := notify(colored, 12, tt.status, tt.text)
			if got, want := ansi.Strip(strings.Join(coloredRows, "\n")), strings.Join(plainRows, "\n"); got != want || want != strings.Join(wrapText(tt.wantPlain, 12), "\n") {
				t.Fatalf("narrow expanded = %q, plain = %q", got, want)
			}
		})
	}
}

// testNow 是测试 transcript 的固定时钟。
var testNow = time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)

// newTestTranscript 创建测试用的 transcript。
func newTestTranscript() *transcript {
	// 1. 使用根 Session s1、固定时刻、六十列宽度和无颜色样式创建 transcript。
	return newTranscript(mustNewMarkdownRenderer(markdownStyleNoTTY, 60), newTUIStyles(markdownStyleNoTTY, nil), newSessionStatuses("s1"),
		make(map[defs.ModelRef]string), 10, func() time.Time { return testNow }, func() bool { return false }, func() string { return "" })
}

// logItems 给测试 Item 按顺序编上流水号。
func logItems(items ...*defs.Item) []runtimeprotocol.LoggedItem {
	// 1. 依次包装 Item，流水号取列表位置加一。
	logged := make([]runtimeprotocol.LoggedItem, len(items))
	for index, item := range items {
		logged[index] = runtimeprotocol.LoggedItem{
			Ordinal: storage.Ordinal(index + 1),
			Item:    item,
		}
	}
	return logged
}

// userItem 创建第一轮的用户文字消息。
func userItem(id defs.ItemID, text string) *defs.Item {
	// 1. 设置用户来源和文本内容，Turn ID 使用 t1。
	return &defs.Item{
		ID:     id,
		TurnID: "t1",
		Kind:   defs.ItemMessage,
		Message: &defs.Message{
			From: &defs.Address{Kind: defs.IdentityUser},
			Content: []*defs.Content{{
				Kind: defs.ContentText,
				Text: &defs.TextContent{Text: text},
			}},
		},
	}
}

// replyItem 创建根 Agent 在第一轮的模型回复。
func replyItem(id defs.ItemID, text string) *defs.Item {
	// 1. 设置来源 s1、模型调用 m1 和文本内容。
	return &defs.Item{
		ID:          id,
		TurnID:      "t1",
		ModelCallID: "m1",
		Kind:        defs.ItemMessage,
		Message: &defs.Message{
			From: &defs.Address{
				Kind: defs.IdentityAgent,
				ID:   "s1",
			},
			Content: []*defs.Content{{
				Kind: defs.ContentText,
				Text: &defs.TextContent{Text: text},
			}},
		},
	}
}

// agentItem 创建指定 Agent 来源的消息和可选结果通知。
func agentItem(id defs.ItemID, from defs.SessionID, text string, notification *defs.SubagentNotification) *defs.Item {
	// 1. 组装来源、文本和子 Agent 结果通知。
	return &defs.Item{
		ID:     id,
		TurnID: "t1",
		Kind:   defs.ItemMessage,
		Message: &defs.Message{
			From: &defs.Address{
				Kind: defs.IdentityAgent,
				ID:   string(from),
			},
			Content: []*defs.Content{{
				Kind: defs.ContentText,
				Text: &defs.TextContent{Text: text},
			}},
			SubagentNotification: notification,
		},
	}
}

// itemStarted 创建测试 Item 的开始事件。
func itemStarted(index int, item *defs.Item) *runtimeclient.ItemChanged {
	// 1. 带上 Item、所属 Turn 和输出位置，事件类型设为开始。
	return &runtimeclient.ItemChanged{
		Type:      agent.AgentEventItemStarted,
		TurnID:    item.TurnID,
		ItemIndex: index,
		Item:      item,
	}
}

// itemDelta 创建指定输出位置的文本增量事件。
func itemDelta(index int, text string) *runtimeclient.ItemChanged {
	// 1. 将文字放入 AgentItemDelta，事件类型设为增量。
	return &runtimeclient.ItemChanged{
		Type:      agent.AgentEventItemDelta,
		ItemIndex: index,
		Delta:     &agent.AgentItemDelta{Text: text},
	}
}

// itemCompleted 创建测试 Item 的完成事件。
func itemCompleted(index int, item *defs.Item) *runtimeclient.ItemChanged {
	// 1. 带上 Item、所属 Turn 和输出位置，事件类型设为完成。
	return &runtimeclient.ItemChanged{
		Type:      agent.AgentEventItemCompleted,
		TurnID:    item.TurnID,
		ItemIndex: index,
		Item:      item,
	}
}

// itemInterrupted 创建不带保存内容的中断事件。
func itemInterrupted(index int) *runtimeclient.ItemChanged {
	// 1. 设置输出位置和中断类型，不附带 Item。
	return &runtimeclient.ItemChanged{
		Type:      agent.AgentEventItemInterrupted,
		ItemIndex: index,
	}
}

// requirePrinted 比较去掉样式后的定稿行。
func requirePrinted(t *testing.T, printed []string, want ...string) {
	t.Helper()
	// 1. 逐行移除 ANSI 样式，再与期望列表比较。
	got := make([]string, len(printed))
	for index, line := range printed {
		got[index] = ansi.Strip(line)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("printed = %q, want %q", got, want)
	}
}

// requireLive 比较去掉样式后的实时尾部。
func requireLive(t *testing.T, tr *transcript, want ...string) {
	t.Helper()
	// 1. 读取实时行并交给定稿行比较函数检查。
	requirePrinted(t, tr.getLiveLines("◐"), want...)
}
