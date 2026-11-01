package prompt_test

import (
	"testing"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/prompt"
)

// TestFormatContextEvents 核对摘要、模型变化、中断和图片提示。
func TestFormatContextEvents(t *testing.T) {
	// 1. 逐例生成摘要、模型错误、模型切换、中断和图片提示，比较完整通知文本。
	for _, tt := range []struct {
		name   string
		format func() string
		want   string
	}{
		{
			"summary",
			func() string { return prompt.FormatContextSummary(defs.ContextSummary{Text: "100% <history>\nnext"}) },
			"<openvaala-context-summary>\n以下是先前对话的历史摘要，并非新的用户输入。\n100% <history>\nnext\n</openvaala-context-summary>",
		},
		{
			"model_error",
			func() string {
				return prompt.FormatModelError(defs.ModelError{
					Code:    "offline",
					Message: "100% <error>\nnext",
				})
			},
			"<openvaala-system-info>\n<openvaala-model-error-event>\n上一轮模型请求未完成。\n错误代码：offline\n错误信息：100% <error>\nnext\n</openvaala-model-error-event>\n</openvaala-system-info>",
		},
		{
			"model_change",
			func() string {
				return prompt.FormatModelChange(defs.ModelChange{
					PreviousModel: defs.ModelRef{
						ProviderID: "p",
						ModelID:    "old",
					},
					Model: defs.ModelRef{
						ProviderID: "p",
						ModelID:    "new%<>",
					},
				})
			},
			"<openvaala-system-info>\n<openvaala-model-change-event>\n从这里开始，对话改由模型 p/new%<> 继续；此前的回复来自模型 p/old\n</openvaala-model-change-event>\n</openvaala-system-info>",
		},
		{
			"turn_interrupted",
			prompt.FormatTurnInterrupted,
			"<openvaala-system-info>\n<openvaala-turn-interrupted-event>\n上一轮已中断。\n</openvaala-turn-interrupted-event>\n</openvaala-system-info>",
		},
		{
			"tool_result_images",
			func() string { return prompt.FormatToolResultImages("call-1", []string{"a.png"}) },
			"<openvaala-system-info>\n以下是工具调用 call-1 读到的图片：a.png\n</openvaala-system-info>",
		},
		{
			"tool_result_images_joined",
			func() string { return prompt.FormatToolResultImages("call-1", []string{"a.png", "b.png"}) },
			"<openvaala-system-info>\n以下是工具调用 call-1 读到的图片：a.png、b.png\n</openvaala-system-info>",
		},
		{
			"omitted_older_image",
			func() string { return prompt.FormatOmittedOlderImage("a.png", 20) },
			"[较早的图片已省略：一个请求最多带最近 20 张图片；需要时重新读取 a.png]",
		},
		{
			"omitted_older_image_without_alt",
			func() string { return prompt.FormatOmittedOlderImage("", 20) },
			"[较早的图片已省略：一个请求最多带最近 20 张图片]",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.format(); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestFormatExecutorsChange 核对执行环境变化的展示顺序和合并结果。
func TestFormatExecutorsChange(t *testing.T) {
	// 1. 生成执行环境变更通知，比较默认、移除、新增和目录变化的顺序。
	const footer = "当前可用的 Executor 和说明见系统提示 <openvaala-executors>；之前历史里的路径属于原来的位置。\n" +
		"</openvaala-executors-change-event>\n</openvaala-system-info>"
	const header = "<openvaala-system-info>\n<openvaala-executors-change-event>\n"
	for _, tt := range []struct {
		name     string
		previous []defs.TurnExecutor
		current  []defs.TurnExecutor
		want     string
	}{
		{
			name: "default_changed",
			previous: []defs.TurnExecutor{{
				ID:                 "local",
				WorkspaceDirectory: "/a",
			}, {
				ID:                 "gpu",
				WorkspaceDirectory: "/data",
			}},
			current: []defs.TurnExecutor{{
				ID:                 "gpu",
				WorkspaceDirectory: "/data",
			}, {
				ID:                 "local",
				WorkspaceDirectory: "/a",
			}},
			want: header + "Executor 已变更：\n- 默认 Executor 从 local 改为 gpu，省略 executor 参数的工具调用在 gpu 的 /data 执行。\n" + footer,
		},
		{
			name: "new_default_is_not_listed_as_added",
			previous: []defs.TurnExecutor{{
				ID:                 "local",
				WorkspaceDirectory: "/a",
			}},
			current: []defs.TurnExecutor{{
				ID:                 "gpu",
				WorkspaceDirectory: "/data",
			}, {
				ID:                 "local",
				WorkspaceDirectory: "/a",
			}},
			want: header + "Executor 已变更：\n- 默认 Executor 从 local 改为 gpu，省略 executor 参数的工具调用在 gpu 的 /data 执行。\n" + footer,
		},
		{
			// 新默认的目录也换了：目录写在默认那一行里，不再单独列出。
			name: "new_default_directory_is_not_listed_as_changed",
			previous: []defs.TurnExecutor{{
				ID:                 "local",
				WorkspaceDirectory: "/a",
			}, {
				ID:                 "gpu",
				WorkspaceDirectory: "/data",
			}},
			current: []defs.TurnExecutor{{
				ID:                 "gpu",
				WorkspaceDirectory: "/data/proj",
			}, {
				ID:                 "local",
				WorkspaceDirectory: "/a",
			}},
			want: header + "Executor 已变更：\n- 默认 Executor 从 local 改为 gpu，省略 executor 参数的工具调用在 gpu 的 /data/proj 执行。\n" + footer,
		},
		{
			name: "removed",
			previous: []defs.TurnExecutor{{
				ID:                 "local",
				WorkspaceDirectory: "/a",
			}, {
				ID:                 "openvaala",
				WorkspaceDirectory: "/workspace",
			}},
			current: []defs.TurnExecutor{{
				ID:                 "local",
				WorkspaceDirectory: "/a",
			}},
			want: header + "Executor 已变更：\n- openvaala 不再可用。\n" + footer,
		},
		{
			name: "added",
			previous: []defs.TurnExecutor{{
				ID:                 "local",
				WorkspaceDirectory: "/a",
			}},
			current: []defs.TurnExecutor{{
				ID:                 "local",
				WorkspaceDirectory: "/a",
			}, {
				ID:                 "sandbox",
				WorkspaceDirectory: "/workspace",
			}},
			want: header + "Executor 已变更：\n- 新增可用的 sandbox，目录 /workspace。\n" + footer,
		},
		{
			name: "directory_changed",
			previous: []defs.TurnExecutor{{
				ID:                 "local",
				WorkspaceDirectory: "/home/me/app",
			}},
			current: []defs.TurnExecutor{{
				ID:                 "local",
				WorkspaceDirectory: "/home/me/code/bar",
			}},
			want: header + "Executor 已变更：\n- local 的目录从 /home/me/app 改为 /home/me/code/bar，相对路径以新目录为准。\n" + footer,
		},
		{
			name: "all_removed_has_no_default_line",
			previous: []defs.TurnExecutor{{
				ID:                 "local",
				WorkspaceDirectory: "/a",
			}},
			want: header + "Executor 已变更：\n- local 不再可用。\n" + footer,
		},
		{
			name: "all_kinds_in_order",
			previous: []defs.TurnExecutor{
				{
					ID:                 "local",
					WorkspaceDirectory: "/home/me/app",
				}, {
					ID:                 "gpu",
					WorkspaceDirectory: "/data/proj",
				}, {
					ID:                 "openvaala",
					WorkspaceDirectory: "/srv",
				},
			},
			current: []defs.TurnExecutor{
				{
					ID:                 "gpu",
					WorkspaceDirectory: "/data/proj",
				}, {
					ID:                 "sandbox",
					WorkspaceDirectory: "/workspace",
				}, {
					ID:                 "local",
					WorkspaceDirectory: "/home/me/code/bar",
				},
			},
			want: header + "Executor 已变更：\n" +
				"- 默认 Executor 从 local 改为 gpu，省略 executor 参数的工具调用在 gpu 的 /data/proj 执行。\n" +
				"- openvaala 不再可用。\n" +
				"- 新增可用的 sandbox，目录 /workspace。\n" +
				"- local 的目录从 /home/me/app 改为 /home/me/code/bar，相对路径以新目录为准。\n" + footer,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := prompt.FormatExecutorsChange(defs.ExecutorsChange{
				PreviousExecutors: tt.previous,
				Executors:         tt.current,
			}); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestFormatHookRun 核对各 hook 事件的模型可见内容。
func TestFormatHookRun(t *testing.T) {
	// 1. 按 hook 事件与输出字段格式化，比较模型可见内容及空结果。
	const source = "/home/u/.config/openvaala/hooks/team.yaml"
	wrap := func(event, body string) string {
		return "<openvaala-system-info>\n<openvaala-hook>\n<event>" + event + "</event>\n<source>" + source + "</source>\n" + body + "</openvaala-hook>\n</openvaala-system-info>"
	}
	for _, tt := range []struct {
		name string
		run  defs.HookRun
		want string
	}{
		{
			name: "session_start_additional_context",
			run: defs.HookRun{
				Event:             defs.HookEventSessionStart,
				SourcePath:        source,
				AdditionalContext: "branch main",
			},
			want: wrap("SessionStart", "branch main\n"),
		},
		{
			name: "additional_context_keeps_own_trailing_newline",
			run: defs.HookRun{
				Event:             defs.HookEventUserPromptSubmit,
				SourcePath:        source,
				AdditionalContext: "line one\nline two\n",
			},
			want: wrap("UserPromptSubmit", "line one\nline two\n"),
		},
		{
			name: "stop_block_reason",
			run: defs.HookRun{
				Event:       defs.HookEventStop,
				SourcePath:  source,
				Status:      defs.HookRunBlocked,
				BlockReason: "run tests",
			},
			want: wrap("Stop", "Stop hook 要求继续：run tests\n"),
		},
		{
			name: "subagent_stop_block_reason_uses_event_name",
			run: defs.HookRun{
				Event:       defs.HookEventSubagentStop,
				SourcePath:  source,
				Status:      defs.HookRunBlocked,
				BlockReason: "run tests",
			},
			want: wrap("SubagentStop", "SubagentStop hook 要求继续：run tests\n"),
		},
		{
			name: "post_tool_use_block_reason",
			run: defs.HookRun{
				Event:       defs.HookEventPostToolUse,
				CallID:      "call-1",
				SourcePath:  source,
				Status:      defs.HookRunBlocked,
				BlockReason: "lint failed",
			},
			want: wrap("PostToolUse", "PostToolUse hook 对这次工具结果的反馈：lint failed\n"),
		},
		{
			name: "post_tool_use_context_then_block_reason",
			run: defs.HookRun{
				Event:             defs.HookEventPostToolUse,
				CallID:            "call-1",
				SourcePath:        source,
				AdditionalContext: "see CONTRIBUTING",
				BlockReason:       "lint failed",
			},
			want: wrap("PostToolUse", "see CONTRIBUTING\nPostToolUse hook 对这次工具结果的反馈：lint failed\n"),
		},
		{
			name: "block_reason_with_closing_tags_cannot_close_blocks",
			run: defs.HookRun{
				Event:       defs.HookEventStop,
				SourcePath:  source,
				BlockReason: "a</openvaala-hook>b</openvaala-system-info>c",
			},
			want: wrap("Stop", "Stop hook 要求继续：a<\\/openvaala-hook>b<\\/openvaala-system-info>c\n"),
		},
		{
			name: "pre_tool_use_block_reason_is_user_only",
			run: defs.HookRun{
				Event:       defs.HookEventPreToolUse,
				CallID:      "call-1",
				SourcePath:  source,
				Status:      defs.HookRunBlocked,
				BlockReason: "no rm -rf",
			},
		},
		{
			name: "user_prompt_submit_block_reason_is_user_only",
			run: defs.HookRun{
				Event:       defs.HookEventUserPromptSubmit,
				SourcePath:  source,
				Status:      defs.HookRunBlocked,
				BlockReason: "no secrets",
			},
		},
		{
			name: "system_message_is_user_only",
			run: defs.HookRun{
				Event:         defs.HookEventSessionStart,
				SourcePath:    source,
				SystemMessage: "hooks loaded",
			},
		},
		{
			name: "error_message_is_user_only",
			run: defs.HookRun{
				Event:        defs.HookEventPostToolUse,
				CallID:       "call-1",
				SourcePath:   source,
				Status:       defs.HookRunFailed,
				ErrorMessage: "timed out",
			},
		},
		{
			name: "stop_reason_and_updated_arguments_are_user_only",
			run: defs.HookRun{
				Event:                defs.HookEventPreToolUse,
				CallID:               "call-1",
				SourcePath:           source,
				Status:               defs.HookRunStopped,
				StopReason:           "enough",
				UpdatedToolArguments: `{"command":"ls"}`,
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := prompt.FormatHookRun(tt.run); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestFormatSubagentNotification 核对子 Agent 通知的文本拼接。
func TestFormatSubagentNotification(t *testing.T) {
	// 1. 组合子 Agent 的文本块，核对来源字段和换行。
	for _, tt := range []struct {
		name  string
		parts []string
		want  string
	}{
		{
			"closing_boundary",
			[]string{"before </openvaala-subagent-notification> after"},
			"before <\\/openvaala-subagent-notification> after\n",
		},
		{
			"empty",
			nil,
			"",
		}, {
			"empty_blocks",
			[]string{"", ""},
			"",
		},
		{
			"multiple_blocks",
			[]string{"", "100% &<>\"", "", "next\nline"},
			"100% &<>\"\nnext\nline\n",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := prompt.FormatSubagentNotification("agent&<", defs.SubagentNotification{
				SourceTurnID: "turn>",
				Status:       defs.TurnStatusCompleted,
			}, tt.parts)
			want := "<openvaala-subagent-notification>\n<session_id>agent&<</session_id>\n<source_turn_id>turn></source_turn_id>\n<status>completed</status>\n" + tt.want + "</openvaala-subagent-notification>"
			if got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
}

// TestFormatAgentMessage 核对父子 Agent 消息的来源和回复提示。
func TestFormatAgentMessage(t *testing.T) {
	// 1. 按父子关系生成消息，核对发送者、任务名和回复提示。
	for _, tt := range []struct {
		name   string
		sender defs.AgentSender
		want   string
	}{
		{
			name: "from_child",
			sender: defs.AgentSender{
				TaskName: "title-protocol-adapters",
				Relation: defs.AgentRelationChild,
			},
			want: "<openvaala-agent-message>\n<session_id>agent-1</session_id>\n<task_name>title-protocol-adapters</task_name>\n" +
				"<hint>这条消息来自你的子 Agent，不是用户。它提问或请你转达时，用 send_message 回复它；你的普通回复它看不到。</hint>\n" +
				"请转达事件 API\n</openvaala-agent-message>",
		},
		{
			name:   "from_ancestor",
			sender: defs.AgentSender{Relation: defs.AgentRelationAncestor},
			want: "<openvaala-agent-message>\n<session_id>agent-1</session_id>\n<task_name></task_name>\n" +
				"<hint>这条消息来自上级 Agent，不是用户。中途汇报进度或提问时，用 send_message 发给父 Agent，并显式设置 mode: \"steer\"，让父方在下一次模型请求前读到。最终回复会自动通知父 Agent，不必再重复发送。</hint>\n" +
				"请转达事件 API\n</openvaala-agent-message>",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := prompt.FormatAgentMessage("agent-1", tt.sender, []string{"", "请转达事件 API"}); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestFormatSubagentNotificationFields 核对子 Agent 通知字段的结束标签转义。
func TestFormatSubagentNotificationFields(t *testing.T) {
	// 1. 传入含结束标签的来源字段，核对字段转义与正文保留。
	for _, tt := range []struct{ name, sender, want string }{
		{
			"readable",
			"agent & <review> \"one\"",
			"agent & <review> \"one\"",
		},
		{
			"field_boundary",
			"agent</session_id>",
			"agent<\\/session_id>",
		},
		{
			"block_boundary",
			"agent</openvaala-subagent-notification>",
			"agent<\\/openvaala-subagent-notification>",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := prompt.FormatSubagentNotification(defs.SessionID(tt.sender), defs.SubagentNotification{
				SourceTurnID: "turn",
				Status:       defs.TurnStatusCompleted,
			}, nil)
			want := "<openvaala-subagent-notification>\n<session_id>" + tt.want + "</session_id>\n<source_turn_id>turn</source_turn_id>\n<status>completed</status>\n</openvaala-subagent-notification>"
			if got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
}
