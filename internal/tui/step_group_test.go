package tui

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/charmbracelet/x/ansi"
)

// TestGetToolMainArgument 检查不同工具的主参数读取。
func TestGetToolMainArgument(t *testing.T) {
	// 1. 准备 Session 名称及各类工具参数。
	statuses := newSessionStatuses("s1")
	statuses.handleSynced([]runtime.SessionRuntimeInfo{newRootInfo(""), newChildInfo("s2", "检查", "")})
	tests := []struct {
		name      string
		tool      string
		arguments string
		want      string
	}{
		{
			name:      "bash_first_line",
			tool:      "bash",
			arguments: `{"command":"go test ./...\necho done"}`,
			want:      "go test ./...",
		},
		{
			name:      "read_file_path",
			tool:      "read_file",
			arguments: `{"path":"internal/tui/tui.go","offset":10}`,
			want:      "internal/tui/tui.go",
		},
		{
			name:      "apply_patch_files",
			tool:      "apply_patch",
			arguments: patchArguments("*** Begin Patch\n*** Add File: a.go\n+x\n*** Delete File: b.go\n*** End Patch\n"),
			want:      "a.go, b.go",
		},
		{
			name:      "apply_patch_invalid",
			tool:      "apply_patch",
			arguments: `{"patch":"nope"}`,
			want:      "",
		},
		{
			name:      "spawn_agent_task",
			tool:      "spawn_agent",
			arguments: `{"task_name":"检查","message":"看看"}`,
			want:      "检查",
		},
		{
			name:      "send_message_target_name",
			tool:      "send_message",
			arguments: `{"session_id":"s2","message":"继续"}`,
			want:      "检查",
		},
		{
			name:      "wait_agent_has_none",
			tool:      "wait_agent",
			arguments: `{"timeout_seconds":30}`,
			want:      "",
		},
		{
			name:      "other_first_string",
			tool:      "web_search",
			arguments: `{"limit":3,"query":"bubbletea","site":"x"}`,
			want:      "bubbletea",
		},
		{
			name:      "incomplete_json",
			tool:      "web_search",
			arguments: `{"query":`,
			want:      "",
		},
	}
	// 2. 逐项比较主参数显示文字。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getToolMainArgument(defs.ToolCall{
				Name:      tt.tool,
				Arguments: tt.arguments,
			}, statuses); got != tt.want {
				t.Fatalf("main argument = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestGetToolNodes 检查工具与 Executor 分组和排序。
func TestGetToolNodes(t *testing.T) {
	// 1. 准备已完成和运行中的调用生成方法。
	// executed 是结果记录了 Executor 的调用；running 是还没有结果的调用。
	executed := func(name, executorID string) *toolCall {
		call := newTestCall(name, `{"command":"x"}`, 0, 0, nil)
		call.result.Executor = executorID
		return call
	}
	running := func(name, arguments string) *toolCall {
		return &toolCall{call: defs.ToolCall{
			CallID:    name + arguments,
			Name:      name,
			Arguments: arguments,
		}}
	}
	// 2. 列出工具分组、次数、默认 Executor 和协作工具场景。
	tests := []struct {
		name              string
		group             *stepGroup
		defaultExecutorID string
		want              []string
	}{
		{
			name: "grouped_by_tool_and_executor",
			group: newTestGroup(
				executed("read_file", "local"), executed("bash", "gpu"), executed("bash", "gpu"), executed("bash", "gpu"),
				newTestCall("spawn_agent", `{"task_name":"检查"}`, 0, 0, nil),
			),
			want: []string{"bash@gpu 3 次", "read_file@local 1 次", "spawn_agent 1 次"},
		},
		{
			name:  "same_tool_on_two_executors_is_two_nodes",
			group: newTestGroup(executed("bash", "local"), executed("bash", "gpu"), executed("bash", "gpu")),
			want:  []string{"bash@gpu 2 次", "bash@local 1 次"},
		},
		{
			// MCP 工具在工具树里也写成 "server · 工具"，同一个 server 的不同工具各占一个节点。
			name:  "mcp_tools_show_server_and_tool",
			group: newTestGroup(executed("mcp__fake__echo", "local"), executed("mcp__fake__echo", "local"), executed("mcp__fake__add", "local")),
			want:  []string{"fake · echo@local 2 次", "fake · add@local 1 次"},
		},
		{
			name:  "equal_counts_keep_first_appearance",
			group: newTestGroup(executed("bash", "local"), executed("bash", "gpu")),
			want:  []string{"bash@local 1 次", "bash@gpu 1 次"},
		},
		{
			// 已有结果未记录 Executor 时只写工具名，不使用默认 Executor。
			name:              "old_result_without_executor_stays_bare",
			group:             newTestGroup(newTestCall("bash", `{"command":"ls"}`, 0, 0, nil), executed("bash", "gpu")),
			defaultExecutorID: "sandbox",
			want:              []string{"bash 1 次", "bash@gpu 1 次"},
		},
		{
			name: "running_call_uses_argument_then_default_executor",
			group: newTestGroup(
				running("bash", `{"command":"a"}`), running("bash", `{"command":"b","executor":"sandbox"}`), running("bash", `{"command":"c"}`),
			),
			defaultExecutorID: "gpu",
			want:              []string{"bash@gpu 2 次", "bash@sandbox 1 次"},
		},
		{
			name:  "running_call_without_any_executor_is_bare",
			group: newTestGroup(running("bash", `{"command":"a"}`)),
			want:  []string{"bash 1 次"},
		},
		{
			name: "agent_side_tools_never_show_executor",
			group: newTestGroup(
				running("spawn_agent", `{"task_name":"检查","executor":"gpu"}`), running("send_message", `{"session_id":"s2","executor":"gpu"}`),
				running("interrupt_agent", `{"session_id":"s2","executor":"gpu"}`), running("list_agents", `{"executor":"gpu"}`),
				running("read_session_history", `{"session_id":"s2","executor":"gpu"}`), running("wait_agent", `{"timeout_seconds":30,"executor":"gpu"}`),
			),
			defaultExecutorID: "gpu",
			want: []string{"spawn_agent 1 次", "send_message 1 次", "interrupt_agent 1 次", "list_agents 1 次",
				"read_session_history 1 次", "wait_agent 1 次"},
		},
	}
	// 3. 生成工具树并比较节点标题顺序。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodes := getToolNodes(tt.group, "", 80, newTUIStyles(markdownStyleNoTTY, nil), newSessionStatuses("s1"), tt.defaultExecutorID)
			got := make([]string, len(nodes))
			for index, node := range nodes {
				got[index] = ansi.Strip(node.lines[0])
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("nodes = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestFormatStepGroupSummary 检查已完成步骤组的次数、错误、改动和耗时汇总。
func TestFormatStepGroupSummary(t *testing.T) {
	// 1. 准备成功、失败、命令失败、未执行、补丁合并、改动折行和推理计时场景。
	tests := []struct {
		name  string
		width int
		group *stepGroup
		want  []string
	}{
		{
			name:  "success_with_changed_file",
			width: 100,
			group: newTestGroup(
				newTestCall("read_file", `{"path":"a.go"}`, 1000, 2000, nil),
				newTestCall("bash", `{"command":"ls"}`, 2000, 3000, nil),
				newTestCall("read_file", `{"path":"b.go"}`, 3000, 4000, nil),
				newTestCall("apply_patch", patchArguments("*** Begin Patch\n*** Update File: internal/tui/tui.go\n@@\n-a\n-b\n+c\n*** End Patch\n"), 4000, 13400, nil),
			),
			want: []string{"✓ 工具 4 次 · 12.4s", "  ├─ read_file 2 次", "  ├─ bash 1 次", "  └─ apply_patch 1 次",
				"     └─ 改动 internal/tui/tui.go +1 −2"},
		},
		{
			name:  "failures_capped_at_three",
			width: 100,
			group: newTestGroup(
				newTestCall("bash", `{"command":"a"}`, 0, 0, &defs.ToolError{
					Code:    "exit",
					Message: "exit status 1\nmore",
				}),
				newTestCall("bash", `{"command":"b"}`, 0, 0, &defs.ToolError{Message: "boom"}),
				newTestCall("bash", `{"command":"c"}`, 0, 0, &defs.ToolError{Message: "boom"}),
				newTestCall("bash", `{"command":"d"}`, 0, 0, &defs.ToolError{Message: "boom"}),
			),
			want: []string{"✗ 工具 4 次（失败 4）", "  └─ bash 4 次", "     ├─ ✗ a：exit status 1", "     ├─ ✗ b：boom",
				"     ├─ ✗ c：boom", "     └─ 另有 1 个失败"},
		},
		{
			// 退出码非 0 和 failed 的命令算失败，退出码 0 和还在运行的不算。
			name:  "failed_commands_count_as_failures",
			width: 100,
			group: newTestGroup(
				newTestCommandCall("a", `{"command_id":"cmd_1","status":"exited","exit_code":2,"output":"oops\n","next_cursor":5,"has_more":false}`),
				newTestCommandCall("b", `{"command_id":"cmd_2","status":"failed","exit_code":null,"error":"connection lost","output":"","next_cursor":0,"has_more":false}`),
				newTestCommandCall("c", `{"command_id":"cmd_3","status":"exited","exit_code":0,"output":"ok\n","next_cursor":3,"has_more":false}`),
				newTestCommandCall("d", `{"command_id":"cmd_4","status":"running","exit_code":null,"output":"","next_cursor":0,"has_more":false}`),
			),
			want: []string{"✗ 工具 4 次（失败 2）", "  └─ bash 4 次", "     ├─ ✗ a：exit 2", "     └─ ✗ b：failed: connection lost"},
		},
		{
			// stop_command 停下的命令（退出码 -1）不算失败，超时被停下的仍算。
			name:  "stopped_command_is_not_a_failure_but_timed_out_is",
			width: 100,
			group: newTestGroup(
				newTestCommandCall("a", `{"command_id":"cmd_1","status":"exited","exit_code":-1,"stop_reason":"stopped","output":"tail\n","next_cursor":5,"has_more":false}`),
				newTestCommandCall("b", `{"command_id":"cmd_2","status":"exited","exit_code":-1,"stop_reason":"timed_out","output":"","next_cursor":0,"has_more":false}`),
			),
			want: []string{"✗ 工具 2 次（失败 1）", "  └─ bash 2 次", "     └─ ✗ b：exit -1"},
		},
		{
			name:  "not_executed_counts_as_interrupted",
			width: 100,
			group: func() *stepGroup {
				group := newTestGroup(newTestCall("read_file", `{"path":"a"}`, 1000, 0, nil))
				group.getCalls()[0].result = nil
				group.markNotExecuted()
				return group
			}(),
			want: []string{"✗ 工具 1 次（中断 1）", "  └─ read_file 1 次"},
		},
		{
			name:  "deleted_file_and_merged_counts",
			width: 100,
			group: newTestGroup(
				newTestCall("apply_patch", patchArguments("*** Begin Patch\n*** Delete File: old.txt\n*** Update File: a.go\n@@\n-x\n+y\n*** End Patch\n"), 0, 0, nil),
				newTestCall("apply_patch", patchArguments("*** Begin Patch\n*** Update File: a.go\n@@\n-y\n+z\n+w\n*** End Patch\n"), 0, 0, nil),
				newTestCall("apply_patch", patchArguments("*** Begin Patch\n*** Add File: failed.go\n+x\n*** End Patch\n"), 0, 0, &defs.ToolError{Message: "rejected"}),
			),
			want: []string{"✗ 工具 3 次（失败 1）", "  └─ apply_patch 3 次", "     ├─ ✗ failed.go：rejected", "     └─ 改动 删除 old.txt, a.go +3 −2"},
		},
		{
			name:  "changed_files_wrap",
			width: 30,
			group: newTestGroup(newTestCall("apply_patch",
				patchArguments("*** Begin Patch\n*** Add File: aaaa.go\n+x\n*** Add File: bbbb.go\n+x\n*** Add File: cccc.go\n+x\n*** End Patch\n"), 0, 0, nil)),
			want: []string{"✓ 工具 1 次", "  └─ apply_patch 1 次", "     └─ 改动 aaaa.go +1 −0,", "             bbbb.go +1 −0,", "             cccc.go +1 −0"},
		},
		{
			name:  "reasoning_counted_and_timed",
			width: 100,
			group: &stepGroup{
				turnID: "t1",
				steps: []*groupStep{
					{reasoning: &groupReasoning{
						itemID:  "x1",
						summary: []string{"想"},
						savedAt: 1000,
					}},
					{call: newTestCall("bash", `{"command":"ls"}`, 2000, 3000, nil)},
					{reasoning: &groupReasoning{
						itemID:  "x2",
						savedAt: 4000,
					}},
					{call: newTestCall("read_file", `{"path":"a"}`, 5000, 6000, nil)},
				},
			},
			want: []string{"✓ 思考 2 次 · 工具 2 次 · 5.0s", "  ├─ bash 1 次", "  └─ read_file 1 次"},
		},
		{
			name:  "failure_without_main_argument",
			width: 100,
			group: newTestGroup(newTestCall("wait_agent", `{"timeout_seconds":30}`, 0, 0, &defs.ToolError{Message: "timeout"})),
			want:  []string{"✗ 工具 1 次（失败 1）", "  └─ wait_agent 1 次", "     └─ ✗ timeout"},
		},
	}
	// 2. 生成各组汇总并逐行比较。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines := formatStepGroupSummary(tt.group, tt.width, newTUIStyles(markdownStyleNoTTY, nil), newSessionStatuses("s1"), "")
			requirePrinted(t, lines, tt.want...)
		})
	}
}

// TestFormatRunningStepGroup 检查运行中调用、进度和推理节点。
func TestFormatRunningStepGroup(t *testing.T) {
	// 1. 准备运行调用和生成中推理样例。
	running := func(name, arguments, progress string) *toolCall {
		return &toolCall{
			call: defs.ToolCall{
				CallID:    name + arguments,
				Name:      name,
				Arguments: arguments,
			},
			progress: progress,
		}
	}
	generating := func(text string) *groupStep {
		return &groupStep{reasoning: &groupReasoning{
			itemID:       "x9",
			summary:      []string{text},
			isGenerating: true,
		}}
	}
	// 2. 列出未知耗时、运行调用上限和推理场景。
	tests := []struct {
		name  string
		group *stepGroup
		width int
		want  []string
	}{
		{
			name: "history_group_without_start_time_omits_elapsed",
			group: &stepGroup{
				turnID: "t1",
				steps:  []*groupStep{{call: running("bash", `{"command":"ls"}`, "")}},
			},
			width: 80,
			want:  []string{"◐ 工具 1 次 · 运行中", "  └─ bash 1 次", "     └─ ◐ ls"},
		},
		{
			name: "only_two_running_calls_listed_under_their_tool",
			group: &stepGroup{
				turnID:    "t1",
				startedAt: testNow.Add(-65 * time.Second),
				steps: []*groupStep{
					{call: newTestCall("read_file", `{"path":"done"}`, 0, 0, nil)},
					{call: running("bash", `{"command":"a"}`, "")}, {call: running("bash", `{"command":"b"}`, "line b")},
					{call: running("bash", `{"command":"c"}`, "")},
				},
			},
			width: 80,
			want:  []string{"◐ 工具 4 次 · 运行中 1m05s", "  ├─ bash 3 次", "  │  ├─ ◐ a", "  │  └─ ◐ b  ▏line b", "  └─ read_file 1 次"},
		},
		{
			name: "generating_reasoning_is_last_node",
			group: &stepGroup{
				turnID: "t1",
				steps: []*groupStep{
					{call: running("bash", `{"command":"go test"}`, "")}, generating("first line\nso the field is description"),
				},
			},
			width: 80,
			want:  []string{"◐ 思考 1 次 · 工具 1 次 · 运行中", "  ├─ bash 1 次", "  │  └─ ◐ go test", "  └─ ◐ 思考中：so the field is description"},
		},
		{
			name: "reasoning_only_running",
			group: &stepGroup{
				turnID: "t1",
				steps:  []*groupStep{generating("想一想")},
			},
			width: 80,
			want:  []string{"◐ 思考 1 次 · 运行中", "  └─ ◐ 思考中：想一想"},
		},
	}
	// 3. 生成运行组显示行并比较。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requirePrinted(t, formatRunningStepGroup(tt.group, testNow, "◐", tt.width, newTUIStyles(markdownStyleNoTTY, nil), newSessionStatuses("s1"), ""), tt.want...)
		})
	}
}

// TestFormatRunningStepGroupFitsWidth 检查窄终端中的工具树宽度和推理末尾。
func TestFormatRunningStepGroupFitsWidth(t *testing.T) {
	// 1. 准备长命令和生成中推理。
	const reasoningText = "abcdefghijklmnopqrstuvwxyzABCD"
	group := &stepGroup{
		turnID:    "t1",
		startedAt: testNow,
		steps: []*groupStep{
			{call: &toolCall{call: defs.ToolCall{
				CallID:    "call-1",
				Name:      "bash",
				Arguments: `{"command":"go test -race ./... -run VeryLongName"}`,
			}}},
			{reasoning: &groupReasoning{
				itemID:       "x1",
				summary:      []string{reasoningText},
				isGenerating: true,
			}},
		},
	}
	// 2. 按二十列渲染，检查各行宽度并保留推理末尾。
	lines := formatRunningStepGroup(group, testNow, "◐", 20, newTUIStyles(markdownStyleNoTTY, nil), newSessionStatuses("s1"), "")
	var reasoningLine string
	for _, line := range lines {
		plain := ansi.Strip(line)
		if width := ansi.StringWidth(plain); width > 20 {
			t.Fatalf("line %q width %d exceeds 20", plain, width)
		}
		if strings.Contains(plain, "思考中") {
			reasoningLine = plain
		}
	}
	if !strings.Contains(reasoningLine, "思考中：…") || !strings.HasSuffix(reasoningLine, "CD") {
		t.Fatalf("reasoning line = %q, want the tail of the text after 思考中：…", reasoningLine)
	}
}

// TestStepGroupStyles 检查步骤组的状态标记、计数和树线样式。
func TestStepGroupStyles(t *testing.T) {
	// 1. 生成失败、成功、中断和运行中的步骤组。
	colored, statuses := newTUIStyles("dark", nil), newSessionStatuses("s1")
	failedSummary := formatStepGroupSummary(newTestGroup(
		newTestCall("bash", `{"command":"a"}`, 0, 0, nil),
		newTestCall("bash", `{"command":"b"}`, 0, 0, &defs.ToolError{Message: "boom"}),
	), 80, colored, statuses, "")
	succeededSummary := formatStepGroupSummary(newTestGroup(newTestCall("bash", `{"command":"a"}`, 0, 0, nil)), 80, colored, statuses, "")
	interruptedSummary := formatStepGroupSummary(newTestGroup(
		newTestCall("bash", `{"command":"a"}`, 0, 0, &defs.ToolError{Message: "boom"}),
		&toolCall{
			call: defs.ToolCall{
				CallID:    "call-2",
				Name:      "bash",
				Arguments: `{"command":"b"}`,
			},
			isNotExecuted: true,
		},
	), 80, colored, statuses, "")
	spinner := colored.accent.Render("◐")
	running := formatRunningStepGroup(&stepGroup{
		turnID: "t1",
		steps: []*groupStep{
			{call: &toolCall{
				call: defs.ToolCall{
					CallID:    "call-1",
					Name:      "bash",
					Arguments: `{"command":"ls"}`,
				},
				progress: "ok",
			}},
		},
	}, testNow, spinner, 80, colored, statuses, "")
	// 2. 列出汇总、树线、失败节点和进度行的预期样式。
	tests := []struct {
		name  string
		lines []string
		index int
		want  string
	}{
		// 失败数用失败色，中断数用警告色，其余文字和分隔用暗色。
		{
			name:  "headline_failure_mark_and_failure_count",
			lines: failedSummary,
			index: 0,
			want:  colored.failure.Render("✗") + " " + colored.faint.Render("工具 2 次") + colored.faint.Render("（") + colored.failure.Render("失败 1") + colored.faint.Render("）"),
		},
		{
			name:  "headline_failure_and_interrupted_counts",
			lines: interruptedSummary,
			index: 0,
			want: colored.failure.Render("✗") + " " + colored.faint.Render("工具 2 次") + colored.faint.Render("（") + colored.failure.Render("失败 1") +
				colored.faint.Render(" · ") + colored.warning.Render("中断 1") + colored.faint.Render("）"),
		},
		{
			name:  "headline_success_mark_and_faint_text",
			lines: succeededSummary,
			index: 0,
			want:  colored.success.Render("✓") + " " + colored.faint.Render("工具 1 次"),
		},
		{
			name:  "tool_line_faint_connector_and_label",
			lines: failedSummary,
			index: 1,
			want:  colored.faint.Render("  └─ ") + colored.faint.Render("bash 2 次"),
		},
		{
			name:  "failure_node_failure_mark_and_faint_text",
			lines: failedSummary,
			index: 2,
			want:  colored.faint.Render("     └─ ") + colored.failure.Render("✗") + " " + colored.faint.Render("b：boom"),
		},
		{
			name:  "running_header_spinner_and_faint_text",
			lines: running,
			index: 0,
			want:  spinner + " " + colored.faint.Render("工具 1 次 · 运行中"),
		},
		{
			name:  "running_tool_line_faint",
			lines: running,
			index: 1,
			want:  colored.faint.Render("  └─ ") + colored.faint.Render("bash 1 次"),
		},
		{
			name:  "running_call_spinner_and_faint_text",
			lines: running,
			index: 2,
			want:  colored.faint.Render("     └─ ") + spinner + colored.faint.Render(" ls  ▏ok"),
		},
	}
	// 3. 逐行比较颜色和正文。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.index >= len(tt.lines) || tt.lines[tt.index] != tt.want {
				t.Fatalf("lines = %q, want line %d = %q", tt.lines, tt.index, tt.want)
			}
		})
	}
}

// TestToolProgressIgnored 检查无对应调用和非文本进度不覆盖已有输出。
func TestToolProgressIgnored(t *testing.T) {
	// 1. 登记运行中的调用。
	tr := newTestTranscript()
	tr.handleTurnStarted("t1")
	tr.handleItemChanged(itemStarted(0, toolCallItem("c1", "call-1", "bash", `{"command":"ls"}`)))
	// 2. 依次写入有效、未知调用、非文本和空进度；后三种都不改变已有的最新一行。
	tr.handleToolProgress(&runtimeclient.ToolProgress{
		ItemIndex: 0,
		Progress: agent.AgentToolProgress{
			CallID:  "call-1",
			Content: json.RawMessage(`"first\n"`),
		},
	})
	tr.handleToolProgress(&runtimeclient.ToolProgress{
		ItemIndex: 0,
		Progress: agent.AgentToolProgress{
			CallID:  "call-7",
			Content: json.RawMessage(`"other\n"`),
		},
	})
	tr.handleToolProgress(&runtimeclient.ToolProgress{
		ItemIndex: 0,
		Progress: agent.AgentToolProgress{
			CallID:  "call-1",
			Content: json.RawMessage(`{"n":1}`),
		},
	})
	tr.handleToolProgress(&runtimeclient.ToolProgress{
		ItemIndex: 0,
		Progress: agent.AgentToolProgress{
			CallID:  "call-1",
			Content: json.RawMessage(`"\n"`),
		},
	})
	if got := tr.stepGroups[0].getCalls()[0].progress; got != "first" {
		t.Fatalf("progress = %q, want %q", got, "first")
	}
}

// TestStepGroupFlow 检查步骤组登记、关闭、打印和历史同步。
func TestStepGroupFlow(t *testing.T) {
	// 1. 准备工具与回复交错、迟到结果、中断和推理变化场景。
	tests := []struct {
		name string
		run  func(t *testing.T, tr *transcript)
	}{
		{
			name: "group_prints_before_reply_that_closes_it",
			run: func(t *testing.T, tr *transcript) {
				tr.handleTurnStarted("t1")
				call := toolCallItem("c1", "call-1", "bash", `{"command":"go test ./..."}`)
				requirePrinted(t, tr.handleItemChanged(itemStarted(0, call)))
				tr.handleItemChanged(itemCompleted(0, call))
				result := toolResultItem("c2", "call-1", nil)
				tr.handleItemChanged(itemStarted(1, result))
				requirePrinted(t, tr.handleItemChanged(itemCompleted(1, result)))
				tr.handleItemChanged(itemStarted(2, replyItem("r1", "")))
				requirePrinted(t, tr.handleItemChanged(itemDelta(2, "结论\n\n下")), "✓ 工具 1 次", "  └─ bash 1 次", "")
				requirePrinted(t, tr.handleItemChanged(itemDelta(2, "一段\n")), "● 结论", "")
			},
		},
		{
			name: "result_after_close_flushes_late",
			run: func(t *testing.T, tr *transcript) {
				tr.handleTurnStarted("t1")
				tr.handleItemChanged(itemStarted(0, toolCallItem("c1", "call-1", "read_file", `{"path":"a"}`)))
				tr.handleItemChanged(itemStarted(1, replyItem("r1", "")))
				requirePrinted(t, tr.handleItemChanged(itemDelta(1, "先说一句\n\n")))
				requirePrinted(t, tr.handleItemChanged(itemStarted(2, toolResultItem("c2", "call-1", nil))), "✓ 工具 1 次", "  └─ read_file 1 次", "")
			},
		},
		{
			name: "turn_finished_marks_not_executed",
			run: func(t *testing.T, tr *transcript) {
				tr.handleTurnStarted("t1")
				tr.handleItemChanged(itemStarted(0, toolCallItem("c1", "call-1", "bash", `{"command":"sleep 9"}`)))
				requirePrinted(t, tr.handleTurnFinished(&runtimeclient.TurnFinished{
					TurnID: "t1",
					Status: defs.TurnStatusInterrupted,
				}),
					"✗ 工具 1 次（中断 1）", "  └─ bash 1 次", "")
				if tr.stepGroups[0].getCalls()[0].getState() != toolCallNotExecuted {
					t.Fatal("call without result not marked as not executed")
				}
			},
		},
		{
			name: "steer_input_closes_group",
			run: func(t *testing.T, tr *transcript) {
				tr.handleTurnStarted("t1")
				tr.handleItemChanged(itemStarted(0, toolCallItem("c1", "call-1", "bash", `{"command":"ls"}`)))
				tr.handleItemChanged(itemStarted(1, toolResultItem("c2", "call-1", nil)))
				requirePrinted(t, tr.handleItemsSaved(logItems(userItem("u1", "顺便看看日志"))), "✓ 工具 1 次", "  └─ bash 1 次", "", "› 顺便看看日志", "")
			},
		},
		{
			name: "reasoning_does_not_close_group",
			run: func(t *testing.T, tr *transcript) {
				tr.handleTurnStarted("t1")
				tr.handleItemChanged(itemStarted(0, toolCallItem("c1", "call-1", "bash", `{"command":"ls"}`)))
				tr.handleItemChanged(itemStarted(1, &defs.Item{
					ID:          "x1",
					TurnID:      "t1",
					ModelCallID: "m2",
					Kind:        defs.ItemReasoning,
					Reasoning:   &defs.Reasoning{},
				}))
				tr.handleItemChanged(itemStarted(2, toolCallItem("c3", "call-2", "bash", `{"command":"pwd"}`)))
				if groups := tr.stepGroups; len(groups) != 1 || len(groups[0].steps) != 3 || len(groups[0].getCalls()) != 2 {
					t.Fatalf("groups = %d", len(groups))
				}
			},
		},
		{
			name: "history_replay_keeps_order",
			run: func(t *testing.T, tr *transcript) {
				items := logItems(
					userItem("u1", "第一轮"),
					toolCallItem("c1", "call-1", "read_file", `{"path":"a"}`),
					toolResultItem("c2", "call-1", nil),
					replyItem("r1", "读完了"),
					withTurn(userItem("u2", "第二轮"), "t2"),
					withTurn(toolCallItem("c3", "call-2", "bash", `{"command":"ls"}`), "t2"),
				)
				requirePrinted(t, tr.handleSessionSynced(&runtimeclient.SessionSynced{Items: items}, ""),
					"› 第一轮", "", "✓ 工具 1 次", "  └─ read_file 1 次", "", "● 读完了", "", strings.Repeat("─", 60), "",
					"› 第二轮", "", "✗ 工具 1 次（中断 1）", "  └─ bash 1 次", "")
			},
		},
		{
			name: "active_turn_group_waits_after_sync",
			run: func(t *testing.T, tr *transcript) {
				items := logItems(toolCallItem("c1", "call-1", "bash", `{"command":"go test ./..."}`))
				requirePrinted(t, tr.handleSessionSynced(&runtimeclient.SessionSynced{Items: items}, "t1"))
				requirePrinted(t, tr.handleItemsSaved([]runtimeprotocol.LoggedItem{{
					Ordinal: 2,
					Item:    toolResultItem("c2", "call-1", nil),
				}}))
				requirePrinted(t, tr.handleTurnFinished(&runtimeclient.TurnFinished{
					TurnID: "t1",
					Status: defs.TurnStatusCompleted,
				}), "✓ 工具 1 次", "  └─ bash 1 次", "")
			},
		},
		{
			name: "running_group_live_lines_with_progress",
			run: func(t *testing.T, tr *transcript) {
				tr.handleTurnStarted("t1")
				tr.now = func() time.Time { return testNow }
				tr.handleItemChanged(itemStarted(0, toolCallItem("c1", "call-1", "bash", `{"command":"go test -race ./..."}`)))
				tr.handleItemChanged(itemStarted(1, toolCallItem("c2", "call-2", "read_file", `{"path":"go.mod"}`)))
				tr.handleToolProgress(&runtimeclient.ToolProgress{
					ItemIndex: 0,
					Progress: agent.AgentToolProgress{
						CallID:  "call-1",
						Stream:  "stdout",
						Content: json.RawMessage(`"ok  internal/tui  1.2s\n\n"`),
					},
				})
				tr.now = func() time.Time { return testNow.Add(8 * time.Second) }
				requireLive(t, tr, "◐ 工具 2 次 · 运行中 8s", "  ├─ bash 1 次", "  │  └─ ◐ go test -race ./...  ▏ok  internal/tui  1.2s", "  └─ read_file 1 次", "     └─ ◐ go.mod")
			},
		},
		{
			name: "progress_of_call_saved_before_open",
			run: func(t *testing.T, tr *transcript) {
				// 中途打开时调用从历史补齐，按 CallID 接收进度；没有开始时刻，不显示耗时。
				items := logItems(withTurn(toolCallItem("c1", "call-1", "bash", `{"command":"go test ./..."}`), "t1"))
				requirePrinted(t, tr.handleSessionSynced(&runtimeclient.SessionSynced{Items: items}, "t1"))
				tr.handleToolProgress(&runtimeclient.ToolProgress{
					ItemIndex: 3,
					Progress: agent.AgentToolProgress{
						CallID:  "call-1",
						Stream:  "stdout",
						Content: json.RawMessage(`"ok  internal/tui  1.2s\n"`),
					},
				})
				requireLive(t, tr, "◐ 工具 1 次 · 运行中", "  └─ bash 1 次", "     └─ ◐ go test ./...  ▏ok  internal/tui  1.2s")
			},
		},
		{
			name: "reasoning_and_tools_share_one_group",
			run: func(t *testing.T, tr *transcript) {
				tr.handleTurnStarted("t1")
				tr.handleItemChanged(itemStarted(0, reasoningItem("x1", "", "")))
				tr.handleItemChanged(itemCompleted(0, reasoningItem("x1", "看目录", defs.ReasoningCompleted)))
				tr.handleItemChanged(itemStarted(1, toolCallItem("c1", "call-1", "bash", `{"command":"ls"}`)))
				tr.handleItemChanged(itemStarted(2, toolResultItem("c2", "call-1", nil)))
				tr.handleItemChanged(itemStarted(3, reasoningItem("x2", "再看", defs.ReasoningCompleted)))
				tr.handleItemChanged(itemStarted(4, toolCallItem("c3", "call-2", "bash", `{"command":"pwd"}`)))
				tr.handleItemChanged(itemStarted(5, toolResultItem("c4", "call-2", nil)))
				tr.handleItemChanged(itemStarted(6, replyItem("r1", "")))
				requirePrinted(t, tr.handleItemChanged(itemDelta(6, "结论\n\n下")), "✓ 思考 2 次 · 工具 2 次", "  └─ bash 2 次", "")
				var kinds []string
				for _, step := range tr.stepGroups[0].steps {
					if step.reasoning != nil {
						kinds = append(kinds, "reasoning:"+string(step.reasoning.itemID))
						continue
					}
					kinds = append(kinds, "call:"+step.call.call.CallID)
				}
				if want := []string{"reasoning:x1", "call:call-1", "reasoning:x2", "call:call-2"}; !slices.Equal(kinds, want) {
					t.Fatalf("steps = %v, want %v", kinds, want)
				}
			},
		},
		{
			name: "reasoning_only_group_writes_reasoning_lines",
			run: func(t *testing.T, tr *transcript) {
				tr.handleTurnStarted("t1")
				tr.handleItemChanged(itemStarted(0, reasoningItem("x1", "", "")))
				tr.handleItemChanged(itemCompleted(0, reasoningItem("x1", "先想想", defs.ReasoningCompleted)))
				tr.handleItemChanged(itemStarted(1, replyItem("r1", "")))
				requirePrinted(t, tr.handleItemChanged(itemDelta(1, "答案\n\n")), "∴ 先想想", "")
			},
		},
		{
			name: "reasoning_deltas_accumulate_and_show_summary_once",
			run: func(t *testing.T, tr *transcript) {
				tr.handleTurnStarted("t1")
				tr.handleItemChanged(itemStarted(0, reasoningItem("x1", "", "")))
				tr.handleItemChanged(reasoningDelta(0, []string{"Let me"}, nil))
				tr.handleItemChanged(reasoningDelta(0, nil, []string{"Let me"}))
				tr.handleItemChanged(reasoningDelta(0, []string{" look"}, []string{" look"}))
				reasoning := tr.reasonings["x1"]
				if got := reasoning.getText(); got != "Let me look" || !reasoning.isGenerating {
					t.Fatalf("text = %q, generating = %v", got, reasoning.isGenerating)
				}
			},
		},
		{
			name: "discarded_reasoning_leaves_no_step",
			run: func(t *testing.T, tr *transcript) {
				tr.handleTurnStarted("t1")
				tr.handleItemChanged(itemStarted(0, reasoningItem("x1", "", "")))
				tr.handleItemChanged(reasoningDelta(0, []string{"半截"}, nil))
				requirePrinted(t, tr.handleItemChanged(itemInterrupted(0)))
				if len(tr.stepGroups[0].steps) != 0 {
					t.Fatalf("steps = %d", len(tr.stepGroups[0].steps))
				}
				requireLive(t, tr)
				tr.handleItemChanged(itemStarted(1, replyItem("r1", "")))
				requirePrinted(t, tr.handleItemChanged(itemDelta(1, "好\n\n")))
			},
		},
		{
			name: "single_line_notice_closes_group",
			run: func(t *testing.T, tr *transcript) {
				items := logItems(
					reasoningItem("x1", "试试", defs.ReasoningCompleted),
					&defs.Item{
						ID:         "x2",
						TurnID:     "t1",
						Kind:       defs.ItemModelError,
						ModelError: &defs.ModelError{Message: "rate limited"},
					},
				)
				requirePrinted(t, tr.handleItemsSaved(items), "∴ 试试", "", "✗ 模型错误：rate limited", "")
			},
		},
		{
			name: "history_replay_groups_reasoning_with_tools",
			run: func(t *testing.T, tr *transcript) {
				items := logItems(
					userItem("u1", "第一轮"),
					reasoningItem("x1", "看一下", defs.ReasoningCompleted),
					toolCallItem("c1", "call-1", "read_file", `{"path":"a"}`),
					toolResultItem("c2", "call-1", nil),
					replyItem("r1", "读完了"),
				)
				requirePrinted(t, tr.handleSessionSynced(&runtimeclient.SessionSynced{Items: items}, ""),
					"› 第一轮", "", "✓ 思考 1 次 · 工具 1 次", "  └─ read_file 1 次", "", "● 读完了", "")
			},
		},
	}
	// 2. 每个场景用一个新的测试 transcript 发送事件，比较打印内容、实时显示和步骤顺序。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, newTestTranscript())
		})
	}
}

// newTestGroup 创建由指定工具调用组成的已关闭步骤组。
func newTestGroup(calls ...*toolCall) *stepGroup {
	// 1. 按参数顺序把调用添加为步骤。
	group := &stepGroup{turnID: "t1"}
	for _, call := range calls {
		group.steps = append(group.steps, &groupStep{call: call})
	}
	return group
}

// newTestCall 创建有结果的工具调用样例。
func newTestCall(name, arguments string, callSavedAt, resultSavedAt int64, toolError *defs.ToolError) *toolCall {
	// 1. 写入工具参数、结果错误和保存时间；零时间表示未知。
	return &toolCall{
		call: defs.ToolCall{
			CallID:    name + arguments,
			Name:      name,
			Arguments: arguments,
		},
		result:        &defs.ToolResult{Error: toolError},
		callSavedAt:   callSavedAt,
		resultSavedAt: resultSavedAt,
	}
}

// newTestCommandCall 创建带指定命令结果的 bash 调用样例。
func newTestCommandCall(command, content string) *toolCall {
	// 1. 创建命令调用，再写入原始 JSON 结果。
	call := newTestCall("bash", `{"command":"`+command+`"}`, 0, 0, nil)
	call.result.Content = json.RawMessage(content)
	return call
}

// patchArguments 把补丁正文编码成工具参数 JSON。
func patchArguments(patch string) string {
	// 1. 编码 patch 字段并返回字符串。
	encoded, _ := json.Marshal(map[string]string{"patch": patch})
	return string(encoded)
}

// toolCallItem 创建指定工具和参数的调用 Item。
func toolCallItem(id defs.ItemID, callID, name, arguments string) *defs.Item {
	// 1. 写入固定 Turn、模型调用及已就绪工具调用。
	return &defs.Item{
		ID:          id,
		TurnID:      "t1",
		ModelCallID: "m1",
		Kind:        defs.ItemToolCall,
		ToolCall: &defs.ToolCall{
			CallID:    callID,
			Name:      name,
			Arguments: arguments,
			Status:    defs.ToolCallReady,
		},
	}
}

// toolResultItem 创建与指定 CallID 配对的结果 Item。
func toolResultItem(id defs.ItemID, callID string, toolError *defs.ToolError) *defs.Item {
	// 1. 写入固定成功正文和可选工具错误。
	return &defs.Item{
		ID:          id,
		TurnID:      "t1",
		ModelCallID: "m1",
		Kind:        defs.ItemToolResult,
		ToolResult: &defs.ToolResult{
			CallID:  callID,
			Content: json.RawMessage(`{"ok":true}`),
			Error:   toolError,
		},
	}
}

// reasoningItem 创建推理 Item 样例。
func reasoningItem(id defs.ItemID, summary string, status defs.ReasoningStatus) *defs.Item {
	// 1. 写入推理状态和非空摘要，空状态表示生成中，再生成 Turn t1 的 Item。
	reasoning := &defs.Reasoning{Status: status}
	if summary != "" {
		reasoning.Summary = []string{summary}
	}
	return &defs.Item{
		ID:          id,
		TurnID:      "t1",
		ModelCallID: "m1",
		Kind:        defs.ItemReasoning,
		Reasoning:   reasoning,
	}
}

// reasoningDelta 创建同次到达的推理摘要与正文增量事件。
func reasoningDelta(index int, summary, content []string) *runtimeclient.ItemChanged {
	// 1. 把 Item 序号、摘要和正文写入增量事件。
	return &runtimeclient.ItemChanged{
		Type:      agent.AgentEventItemDelta,
		ItemIndex: index,
		Delta: &agent.AgentItemDelta{
			ReasoningSummary: summary,
			ReasoningContent: content,
		},
	}
}

// withTurn 把 Item 改到指定 Turn 并返回。
func withTurn(item *defs.Item, turnID defs.TurnID) *defs.Item {
	// 1. 替换 Turn ID 后返回同一 Item。
	item.TurnID = turnID
	return item
}
