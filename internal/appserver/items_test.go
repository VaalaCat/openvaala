package appserver

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/storage"
)

const (
	testRootSessionID  defs.SessionID = "s-root"
	testChildSessionID defs.SessionID = "s-child"
	testGrandSessionID defs.SessionID = "s-grand"
)

// newTestTranslator 创建有根、子和孙 Session 路径的测试翻译器。
func newTestTranslator(sessionID defs.SessionID) *codexItemTranslator {
	// 1. 设置 /root 层级路径，local 目录为 /work，ssh 目录为 /srv。
	return newCodexItemTranslator(
		sessionID,
		map[defs.SessionID]string{testRootSessionID: "/root", testChildSessionID: "/root/调研", testGrandSessionID: "/root/调研/子任务"},
		map[string]string{"local": "/work", "ssh": "/srv"},
		"local",
	)
}

// toJSON 将测试值编码为 JSON。
func toJSON(t *testing.T, value any) string {
	t.Helper()
	// 1. 标记辅助函数，编码并返回 JSON 文本，失败时结束测试。
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %T: %v", value, err)
	}
	return string(encoded)
}

// assertJSONEqual 比较 JSON 结构，忽略键顺序和空白。
func assertJSONEqual(t *testing.T, got any, want string) {
	t.Helper()
	// 1. 解码实际值和预期值，结构不同时输出格式化差异。
	var gotValue, wantValue any
	if err := json.Unmarshal([]byte(toJSON(t, got)), &gotValue); err != nil {
		t.Fatalf("decode got: %v", err)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("decode want %q: %v", want, err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		gotText, _ := json.MarshalIndent(gotValue, "", "  ")
		wantText, _ := json.MarshalIndent(wantValue, "", "  ")
		t.Fatalf("json mismatch\ngot:\n%s\nwant:\n%s", gotText, wantText)
	}
}

// newTestCall 创建已收齐参数的测试工具调用。
func newTestCall(id, callID, name string, arguments any) *defs.Item {
	// 1. 非字符串参数编码为 JSON，填写 Item、CallID、工具名和 Ready 状态。
	text, ok := arguments.(string)
	if !ok {
		encoded, _ := json.Marshal(arguments)
		text = string(encoded)
	}
	return &defs.Item{
		ID:     defs.ItemID(id),
		TurnID: "turn-1",
		Kind:   defs.ItemToolCall,
		ToolCall: &defs.ToolCall{
			CallID:    callID,
			Name:      name,
			Arguments: text,
			Status:    defs.ToolCallReady,
		},
	}
}

// newInterruptedTestCall 创建被中断的测试工具调用。
func newInterruptedTestCall(id, callID, name, arguments string) *defs.Item {
	// 1. 创建调用后将状态设为 Interrupted。
	call := newTestCall(id, callID, name, arguments)
	call.ToolCall.Status = defs.ToolCallInterrupted
	return call
}

// newTestResult 创建测试工具结果。
func newTestResult(callID, executorID, content string, toolError *defs.ToolError) *defs.Item {
	// 1. 填写 CallID、Executor、JSON 内容和工具错误。
	return &defs.Item{
		ID:     defs.ItemID("result-" + callID),
		TurnID: "turn-1",
		Kind:   defs.ItemToolResult,
		ToolResult: &defs.ToolResult{
			CallID:   callID,
			Executor: executorID,
			Content:  json.RawMessage(content),
			Error:    toolError,
		},
	}
}

// newTestMessage 创建指定发送方和内容的测试消息。
func newTestMessage(id string, from *defs.Address, contents ...*defs.Content) *defs.Item {
	// 1. 填写 Item 和 Message。
	return &defs.Item{
		ID:     defs.ItemID(id),
		TurnID: "turn-1",
		Kind:   defs.ItemMessage,
		Message: &defs.Message{
			From:    from,
			Content: contents,
		},
	}
}

// newTestReply 创建带模型调用 ID 的测试回复。
func newTestReply(id string, from *defs.Address, contents ...*defs.Content) *defs.Item {
	// 1. 创建消息并填入 ModelCallID。
	reply := newTestMessage(id, from, contents...)
	reply.ModelCallID = defs.ModelCallID("model-call-" + id)
	return reply
}

// newTestText 创建测试文本内容。
func newTestText(text string) *defs.Content {
	// 1. 填写文本类型和原文。
	return &defs.Content{
		Kind: defs.ContentText,
		Text: &defs.TextContent{Text: text},
	}
}

// getRepliesAroundToolCall 构造工具调用前后两次模型回复的日志。
func getRepliesAroundToolCall() []*defs.Item {
	// 1. 创建同次输出的回复和调用，追加工具结果及下一次模型回复。
	commentary := newTestMessage("m-1", testRootAgent, newTestText("let me check"))
	call := newTestCall("c-1", "call-1", "read_file", map[string]any{"path": "a"})
	commentary.ModelCallID, call.ModelCallID = "model-call-1", "model-call-1"
	finalAnswer := newTestMessage("m-2", testRootAgent, newTestText("done"))
	finalAnswer.ModelCallID = "model-call-2"
	return []*defs.Item{commentary, call, newTestResult("call-1", "local", `{"content":"a"}`, nil), finalAnswer}
}

var (
	testUser = &defs.Address{
		Kind: defs.IdentityUser,
		ID:   "user",
	}
	testRootAgent = &defs.Address{
		Kind: defs.IdentityAgent,
		ID:   string(testRootSessionID),
	}
	testChildAgent = &defs.Address{
		Kind: defs.IdentityAgent,
		ID:   string(testChildSessionID),
	}
)

// getReplyBeforeTurnInterrupted 构造用户输入、半截回复和中断标记的日志。
func getReplyBeforeTurnInterrupted() []*defs.Item {
	// 1. 为回复填写 ModelCallID，追加 turn_interrupted。
	reply := newTestMessage("m-2", testRootAgent, newTestText("half"))
	reply.ModelCallID = "model-call-1"
	interrupted := &defs.Item{
		ID:     "interrupted-1",
		TurnID: "turn-1",
		Kind:   defs.ItemTurnInterrupted,
	}
	return []*defs.Item{newTestMessage("m-1", testUser, newTestText("hi")), reply, interrupted}
}

// getReplyBeforeHookRun 构造回复之后补充输入被 hook 拦下的日志。
func getReplyBeforeHookRun() []*defs.Item {
	// 1. 为回复填写 ModelCallID，追加无模型调用 ID 的 blocked hook_run。
	reply := newTestMessage("m-2", testRootAgent, newTestText("done"))
	reply.ModelCallID = "model-call-1"
	blocked := &defs.Item{
		ID:     "hook-1",
		TurnID: "turn-1",
		Kind:   defs.ItemHookRun,
		HookRun: &defs.HookRun{
			ID:          "run-1",
			Event:       defs.HookEventUserPromptSubmit,
			Status:      defs.HookRunBlocked,
			BlockReason: "no",
		},
	}
	return []*defs.Item{newTestMessage("m-1", testUser, newTestText("hi")), reply, blocked}
}

// TestCodexItemTranslatorGetThreadItems 检查日志中各种 Item 的转换结果。
func TestCodexItemTranslatorGetThreadItems(t *testing.T) {
	// 1. 准备消息、工具、补丁、子 Agent、摘要和分叉日志及预期 JSON。
	bashOutput := `{"command_id":"cmd_000000000001","status":"exited","exit_code":0,"output":"hi\n","next_cursor":3,"has_more":false}`
	updatePatch := "*** Begin Patch\n*** Update File: b.txt\n@@\n keep\n-old\n+new\n*** End Patch"
	addDeletePatch := "*** Begin Patch\n*** Add File: a.txt\n+one\n*** Delete File: c.txt\n*** End Patch"
	twoSectionPatch := "*** Begin Patch\n*** Update File: b.txt\n@@\n keep\n-old\n+new\n+added\n@@ second\n tail\n-gone\n*** End Patch"
	childNotification := newTestMessage("note-1", testChildAgent, newTestText("done"))
	childNotification.Message.SubagentNotification = &defs.SubagentNotification{
		SourceTurnID: "turn-9",
		Status:       defs.TurnStatusCompleted,
	}
	reasoning := &defs.Item{
		ID:     "r-1",
		TurnID: "turn-1",
		Kind:   defs.ItemReasoning,
		Reasoning: &defs.Reasoning{
			Status:  defs.ReasoningCompleted,
			Summary: []string{"plan"},
		},
	}
	readImageResult := newTestResult("call-1", "local", `{"path":"a.png","mime_type":"image/png","size":4}`, nil)
	readImageResult.ToolResult.Images = []*defs.ImageContent{{
		DataURL: "data:image/png;base64,AAAA",
		Alt:     "a.png",
	}}
	tests := []struct {
		name      string
		sessionID defs.SessionID
		items     []*defs.Item
		want      string
	}{
		{
			name: "user_message_with_text_and_image",
			items: []*defs.Item{newTestMessage("m-1", testUser, newTestText("hi"), &defs.Content{
				Kind:  defs.ContentImage,
				Image: &defs.ImageContent{DataURL: "data:image/png;base64,AAAA"},
			})},
			want: `[{"type":"userMessage","id":"m-1","clientId":null,"content":[{"type":"text","text":"hi","text_elements":[]},{"type":"image","url":"data:image/png;base64,AAAA"}]}]`,
		},
		{
			name:  "own_reply_is_agent_message",
			items: []*defs.Item{newTestReply("m-2", testRootAgent, newTestText("你"), newTestText("好"))},
			want:  `[{"type":"agentMessage","id":"m-2","text":"你好","phase":"final_answer"}]`,
		},
		{
			// 分叉复制来的回复 From 还是来源对话的根 Session：按 ModelCallID 认出是模型输出的回复。
			name: "copied_reply_from_the_source_root_session_is_agent_message",
			items: []*defs.Item{newTestReply("m-2", &defs.Address{
				Kind: defs.IdentityAgent,
				ID:   "s-source-root",
			}, newTestText("copied"))},
			want: `[{"type":"agentMessage","id":"m-2","text":"copied","phase":"final_answer"}]`,
		},
		{
			// 没有模型调用的 Agent 消息（别的 Agent 发来的）不是回复，即使 From 是本 Session。
			name:  "agent_message_without_model_call_is_hidden",
			items: []*defs.Item{newTestMessage("m-2", testRootAgent, newTestText("relayed"))},
			want:  `[]`,
		},
		{
			// 同一次模型输出（ModelCallID）里有工具调用的回复是回合中途的话，最后一次输出只有回复，是这一轮的最终回答。
			name:  "reply_followed_by_tool_call_is_commentary",
			items: getRepliesAroundToolCall(),
			want: `[
				{"type":"agentMessage","id":"m-1","text":"let me check","phase":"commentary"},
				{"type":"mcpToolCall","id":"c-1","server":"local","tool":"read_file","status":"completed","arguments":{"path":"a"},"result":{"content":[{"type":"text","text":"a"}],"structuredContent":{"content":"a"}},"error":null},
				{"type":"agentMessage","id":"m-2","text":"done","phase":"final_answer"}
			]`,
		},
		{
			// 取消这一轮时写在最后的 turn_interrupted 不是输出：它前面的回复仍然是这一轮停在的那条，是最终回答。
			name:  "reply_followed_by_turn_interrupted_is_final_answer",
			items: getReplyBeforeTurnInterrupted(),
			want:  `[{"type":"userMessage","id":"m-1","clientId":null,"content":[{"type":"text","text":"hi","text_elements":[]}]},{"type":"agentMessage","id":"m-2","text":"half","phase":"final_answer"}]`,
		},
		{
			// 补充输入被 UserPromptSubmit 拦下时只留下 hook_run（没有 ModelCallID）：它不是输出，前面的回复仍是最终回答。
			name:  "reply_followed_by_hook_run_is_final_answer",
			items: getReplyBeforeHookRun(),
			want:  `[{"type":"userMessage","id":"m-1","clientId":null,"content":[{"type":"text","text":"hi","text_elements":[]}]},{"type":"agentMessage","id":"m-2","text":"done","phase":"final_answer"}]`,
		},
		{
			name:  "reasoning_keeps_summary_and_empty_content",
			items: []*defs.Item{reasoning},
			want:  `[{"type":"reasoning","id":"r-1","summary":["plan"],"content":[]}]`,
		},
		{
			// 命令照 Codex 的写法拼成 bash -lc <命令>：含单引号时按 POSIX shell 规则转义。
			name:  "bash_command_with_quotes_is_shell_quoted",
			items: []*defs.Item{newTestCall("c-1", "call-1", "bash", map[string]any{"command": "echo 'a b'"})},
			want:  `[{"type":"commandExecution","id":"c-1","command":"bash -lc 'echo '\"'\"'a b'\"'\"''","cwd":"/work","status":"inProgress","aggregatedOutput":null,"exitCode":null,"commandActions":[],"source":"agent","processId":null,"durationMs":null}]`,
		},
		{
			name:  "bash_success",
			items: []*defs.Item{newTestCall("c-1", "call-1", "bash", map[string]any{"command": "echo hi", "cwd": "sub"}), newTestResult("call-1", "local", bashOutput, nil)},
			want:  `[{"type":"commandExecution","id":"c-1","command":"bash -lc 'echo hi'","cwd":"/work/sub","status":"completed","aggregatedOutput":"hi\n","exitCode":0,"commandActions":[],"source":"agent","processId":null,"durationMs":null}]`,
		},
		{
			name:  "bash_interrupted_without_result",
			items: []*defs.Item{newInterruptedTestCall("c-1", "call-1", "bash", `{"command":"sleep 9"}`)},
			want:  `[{"type":"commandExecution","id":"c-1","command":"bash -lc 'sleep 9'","cwd":"/work","status":"failed","aggregatedOutput":null,"exitCode":null,"commandActions":[],"source":"agent","processId":null,"durationMs":null}]`,
		},
		{
			name:  "bash_running",
			items: []*defs.Item{newTestCall("c-1", "call-1", "bash", map[string]any{"command": "sleep 9"})},
			want:  `[{"type":"commandExecution","id":"c-1","command":"bash -lc 'sleep 9'","cwd":"/work","status":"inProgress","aggregatedOutput":null,"exitCode":null,"commandActions":[],"source":"agent","processId":null,"durationMs":null}]`,
		},
		{
			name: "bash_nonzero_exit_fails_with_output",
			items: []*defs.Item{
				newTestCall("c-1", "call-1", "bash", map[string]any{"command": "false"}),
				newTestResult("call-1", "local", `{"command_id":"cmd_000000000001","status":"exited","exit_code":2,"output":"boom\n","next_cursor":5,"has_more":false}`, nil),
			},
			want: `[{"type":"commandExecution","id":"c-1","command":"bash -lc false","cwd":"/work","status":"failed","aggregatedOutput":"boom\n","exitCode":2,"commandActions":[],"source":"agent","processId":null,"durationMs":null}]`,
		},
		{
			name: "bash_stopped_by_stop_command_completes",
			items: []*defs.Item{
				newTestCall("c-1", "call-1", "bash", map[string]any{"command": "sleep 9"}),
				newTestResult("call-1", "local", `{"command_id":"cmd_000000000001","status":"exited","exit_code":-1,"stop_reason":"stopped","output":"tail\n","next_cursor":5,"has_more":false}`, nil),
			},
			want: `[{"type":"commandExecution","id":"c-1","command":"bash -lc 'sleep 9'","cwd":"/work","status":"completed","aggregatedOutput":"tail\n","exitCode":-1,"commandActions":[],"source":"agent","processId":null,"durationMs":null}]`,
		},
		{
			name: "bash_timed_out_fails",
			items: []*defs.Item{
				newTestCall("c-1", "call-1", "bash", map[string]any{"command": "sleep 9"}),
				newTestResult("call-1", "local", `{"command_id":"cmd_000000000001","status":"exited","exit_code":-1,"stop_reason":"timed_out","output":"tail\n","next_cursor":5,"has_more":false}`, nil),
			},
			want: `[{"type":"commandExecution","id":"c-1","command":"bash -lc 'sleep 9'","cwd":"/work","status":"failed","aggregatedOutput":"tail\n","exitCode":-1,"commandActions":[],"source":"agent","processId":null,"durationMs":null}]`,
		},
		{
			name: "bash_still_running_completes_without_exit_code",
			items: []*defs.Item{
				newTestCall("c-1", "call-1", "bash", map[string]any{"command": "make"}),
				newTestResult("call-1", "local", `{"command_id":"cmd_000000000001","status":"running","exit_code":null,"output":"building\n","next_cursor":9,"has_more":false}`, nil),
			},
			want: `[{"type":"commandExecution","id":"c-1","command":"bash -lc make","cwd":"/work","status":"completed","aggregatedOutput":"building\n","exitCode":null,"commandActions":[],"source":"agent","processId":null,"durationMs":null}]`,
		},
		{
			name: "bash_command_failed_fails",
			items: []*defs.Item{
				newTestCall("c-1", "call-1", "bash", map[string]any{"command": "make", "executor": "ssh"}),
				newTestResult("call-1", "ssh", `{"command_id":"cmd_000000000001","status":"failed","exit_code":null,"error":"wait for command: connection lost","output":"building\n","next_cursor":9,"has_more":false}`, nil),
			},
			want: `[{"type":"commandExecution","id":"c-1","command":"bash -lc make","cwd":"/srv","status":"failed","aggregatedOutput":"building\nwait for command: connection lost","exitCode":null,"commandActions":[],"source":"agent","processId":null,"durationMs":null}]`,
		},
		{
			name: "bash_on_other_executor_keeps_absolute_cwd",
			items: []*defs.Item{
				newTestCall("c-1", "call-1", "bash", map[string]any{"command": "ls", "cwd": "/srv/app", "executor": "ssh"}),
				newTestResult("call-1", "ssh", `{"command_id":"cmd_000000000001","status":"exited","exit_code":0,"output":"","next_cursor":0,"has_more":false}`, nil),
			},
			want: `[{"type":"commandExecution","id":"c-1","command":"bash -lc ls","cwd":"/srv/app","status":"completed","aggregatedOutput":"","exitCode":0,"commandActions":[],"source":"agent","processId":null,"durationMs":null}]`,
		},
		{
			name: "bash_result_without_command_structure",
			items: []*defs.Item{
				newTestCall("c-1", "call-1", "bash", map[string]any{"command": "ls", "cwd": "../out", "executor": "ssh"}),
				newTestResult("call-1", "", `null`, &defs.ToolError{
					Code:    "invalid_arguments",
					Message: "invalid bash cwd",
				}),
			},
			want: `[{"type":"commandExecution","id":"c-1","command":"bash -lc ls","cwd":"/out","status":"failed","aggregatedOutput":"invalid bash cwd","exitCode":null,"commandActions":[],"source":"agent","processId":null,"durationMs":null}]`,
		},
		{
			name: "bash_start_failure_without_output_shows_the_error",
			items: []*defs.Item{
				newTestCall("c-1", "call-1", "bash", map[string]any{"command": "ls", "cwd": "missing"}),
				newTestResult("call-1", "local", `null`, &defs.ToolError{
					Code:    "command_start_failed",
					Message: "chdir /work/missing: no such file or directory",
				}),
			},
			want: `[{"type":"commandExecution","id":"c-1","command":"bash -lc ls","cwd":"/work/missing","status":"failed","aggregatedOutput":"chdir /work/missing: no such file or directory","exitCode":null,"commandActions":[],"source":"agent","processId":null,"durationMs":null}]`,
		},
		{
			name: "bash_failure_without_output_keeps_exit_code",
			items: []*defs.Item{
				newTestCall("c-1", "call-1", "bash", map[string]any{"command": "false"}),
				newTestResult("call-1", "local", `{"command_id":"cmd_000000000001","status":"exited","exit_code":1,"output":"","next_cursor":0,"has_more":false}`, nil),
			},
			want: `[{"type":"commandExecution","id":"c-1","command":"bash -lc false","cwd":"/work","status":"failed","aggregatedOutput":"","exitCode":1,"commandActions":[],"source":"agent","processId":null,"durationMs":null}]`,
		},
		{
			name:  "bash_interrupted_with_truncated_arguments_has_empty_command",
			items: []*defs.Item{newInterruptedTestCall("c-1", "call-1", "bash", `{"command":"sl`)},
			want:  `[{"type":"commandExecution","id":"c-1","command":"","cwd":"/work","status":"failed","aggregatedOutput":null,"exitCode":null,"commandActions":[],"source":"agent","processId":null,"durationMs":null}]`,
		},
		{
			// 结果里没有起始行（旧日志）：没有行号，各段照补丁原文。
			name: "apply_patch_update_without_start_lines_keeps_the_patch_sections",
			items: []*defs.Item{
				newTestCall("c-1", "call-1", "apply_patch", map[string]any{"patch": updatePatch}),
				newTestResult("call-1", "local", `{"outcome":"applied","added":[],"updated":["b.txt"],"deleted":[]}`, nil),
			},
			want: `[{"type":"fileChange","id":"c-1","status":"completed","changes":[{"path":"/work/b.txt","kind":{"type":"update"},"diff":"@@\n keep\n-old\n+new\n"}]}]`,
		},
		{
			// 两段改动：第一段从原文件第 3 行起，2 行变 3 行；第二段从原文件第 10 行起，新文件里往后挪 1 行，2 行变 1 行。
			name: "apply_patch_update_with_start_lines_is_a_numbered_unified_diff",
			items: []*defs.Item{
				newTestCall("c-1", "call-1", "apply_patch", map[string]any{"patch": twoSectionPatch}),
				newTestResult("call-1", "local", `{"outcome":"applied","added":[],"updated":["b.txt"],"deleted":[],"change_start_lines":{"b.txt":[3,10]}}`, nil),
			},
			want: `[{"type":"fileChange","id":"c-1","status":"completed","changes":[{"path":"/work/b.txt","kind":{"type":"update"},` +
				`"diff":"@@ -3,2 +3,3 @@\n keep\n-old\n+new\n+added\n@@ -10,2 +11,1 @@\n tail\n-gone\n"}]}]`,
		},
		{
			name: "apply_patch_partially_applied_fails",
			items: []*defs.Item{
				newTestCall("c-1", "call-1", "apply_patch", map[string]any{"patch": addDeletePatch, "executor": "ssh"}),
				newTestResult("call-1", "ssh", `{"outcome":"partially_applied","added":["a.txt"],"updated":[],"deleted":[]}`, &defs.ToolError{
					Code:    "commit_failed",
					Message: "patch did not finish cleanly",
				}),
			},
			want: `[{"type":"fileChange","id":"c-1","status":"failed","changes":[{"path":"/srv/a.txt","kind":{"type":"add"},"diff":"one\n"},{"path":"/srv/c.txt","kind":{"type":"delete"},"diff":""}]}]`,
		},
		{
			name:  "apply_patch_running_waits_for_its_result",
			items: []*defs.Item{newTestCall("c-1", "call-1", "apply_patch", map[string]any{"patch": updatePatch})},
			want:  `[]`,
		},
		{
			name: "apply_patch_invalid_patch_has_no_changes",
			items: []*defs.Item{
				newTestCall("c-1", "call-1", "apply_patch", map[string]any{"patch": "garbage"}),
				newTestResult("call-1", "", `null`, &defs.ToolError{
					Code:    "invalid_arguments",
					Message: "invalid local workspace patch",
				}),
			},
			want: `[{"type":"fileChange","id":"c-1","status":"failed","changes":[]}]`,
		},
		{
			name: "read_file_is_local_mcp_tool_call",
			items: []*defs.Item{
				newTestCall("c-1", "call-1", "read_file", map[string]any{"path": "a.txt"}),
				newTestResult("call-1", "local", `{"path":"a.txt","content":"line\n","eof":true}`, nil),
			},
			want: `[{"type":"mcpToolCall","id":"c-1","server":"local","tool":"read_file","status":"completed","arguments":{"path":"a.txt"},"result":{"content":[{"type":"text","text":"line\n"}],"structuredContent":{"path":"a.txt","content":"line\n","eof":true}},"error":null}]`,
		},
		{
			// 空正文仍输出 text 字段。
			name: "empty_text_result_keeps_the_text_field",
			items: []*defs.Item{
				newTestCall("c-1", "call-1", "read_file", map[string]any{"path": "empty.txt"}),
				newTestResult("call-1", "local", `{"path":"empty.txt","content":"","eof":true}`, nil),
			},
			want: `[{"type":"mcpToolCall","id":"c-1","server":"local","tool":"read_file","status":"completed","arguments":{"path":"empty.txt"},"result":{"content":[{"type":"text","text":""}],"structuredContent":{"path":"empty.txt","content":"","eof":true}},"error":null}]`,
		},
		{
			// 图片放进 content 的图片块；structuredContent 仍是工具结果的 Content，不含图片数据。
			name: "read_file_image_result",
			items: []*defs.Item{
				newTestCall("c-1", "call-1", "read_file", map[string]any{"path": "a.png"}),
				readImageResult,
			},
			want: `[{"type":"mcpToolCall","id":"c-1","server":"local","tool":"read_file","status":"completed","arguments":{"path":"a.png"},"result":{"content":[{"type":"text","text":"{\"path\":\"a.png\",\"mime_type\":\"image/png\",\"size\":4}"},{"type":"image","data":"AAAA","mimeType":"image/png"}],"structuredContent":{"path":"a.png","mime_type":"image/png","size":4}},"error":null}]`,
		},
		{
			name: "mcp_tool_shows_server_id_and_original_tool_name",
			items: []*defs.Item{
				newTestCall("c-1", "call-1", "mcp__fake__echo", map[string]any{"text": "hi"}),
				newTestResult("call-1", "local", `{"text":"hi"}`, nil),
			},
			want: `[{"type":"mcpToolCall","id":"c-1","server":"fake","tool":"echo","status":"completed","arguments":{"text":"hi"},"result":{"content":[{"type":"text","text":"{\"text\":\"hi\"}"}],"structuredContent":{"text":"hi"}},"error":null}]`,
		},
		{
			name:  "running_mcp_tool_shows_server_id_and_original_tool_name",
			items: []*defs.Item{newTestCall("c-1", "call-1", "mcp__fake__echo", map[string]any{"text": "hi"})},
			want:  `[{"type":"mcpToolCall","id":"c-1","server":"fake","tool":"echo","status":"inProgress","arguments":{"text":"hi"},"result":null,"error":null}]`,
		},
		{
			name:  "mcp_name_without_tool_part_is_unchanged",
			items: []*defs.Item{newTestCall("c-1", "call-1", "mcp__fake", "")},
			want:  `[{"type":"mcpToolCall","id":"c-1","server":"local","tool":"mcp__fake","status":"inProgress","arguments":{},"result":null,"error":null}]`,
		},
		{
			name: "tool_error_fails_with_message",
			items: []*defs.Item{
				newTestCall("c-1", "call-1", "read_file", map[string]any{"path": "missing.txt"}),
				newTestResult("call-1", "local", `null`, &defs.ToolError{
					Code:    "file_not_found",
					Message: "no such file",
				}),
			},
			want: `[{"type":"mcpToolCall","id":"c-1","server":"local","tool":"read_file","status":"failed","arguments":{"path":"missing.txt"},"result":null,"error":{"message":"no such file"}}]`,
		},
		{
			name:  "running_tool_uses_chosen_executor",
			items: []*defs.Item{newTestCall("c-1", "call-1", "read_file", map[string]any{"path": "a", "executor": "ssh"})},
			want:  `[{"type":"mcpToolCall","id":"c-1","server":"ssh","tool":"read_file","status":"inProgress","arguments":{"path":"a","executor":"ssh"},"result":null,"error":null}]`,
		},
		{
			name: "truncated_arguments_become_a_string",
			items: []*defs.Item{
				newInterruptedTestCall("c-1", "call-1", "read_file", `{"path":"a.t`),
			},
			want: `[{"type":"mcpToolCall","id":"c-1","server":"local","tool":"read_file","status":"failed","arguments":"{\"path\":\"a.t","result":null,"error":null}]`,
		},
		{
			name:  "empty_arguments_become_an_empty_object",
			items: []*defs.Item{newTestCall("c-1", "call-1", "custom_tool", "")},
			want:  `[{"type":"mcpToolCall","id":"c-1","server":"local","tool":"custom_tool","status":"inProgress","arguments":{},"result":null,"error":null}]`,
		},
		{
			name: "list_and_wait_agents_are_hidden",
			items: []*defs.Item{
				newTestCall("c-1", "call-1", "list_agents", `{}`), newTestResult("call-1", "", `{"sessions":[]}`, nil),
				newTestCall("c-2", "call-2", "wait_agent", `{}`), newTestResult("call-2", "", `{"reason":"timeout"}`, nil),
			},
			want: `[]`,
		},
		{
			name: "read_session_history_is_an_agent_mcp_tool_call",
			items: []*defs.Item{
				newTestCall("c-1", "call-1", "read_session_history", map[string]any{"session_id": "s-child"}),
				newTestResult("call-1", "", `{"session_id":"s-child","items":[],"next_cursor":0}`, nil),
			},
			want: `[{"type":"mcpToolCall","id":"c-1","server":"agent","tool":"read_session_history","status":"completed","arguments":{"session_id":"s-child"},"result":{"content":[{"type":"text","text":"{\"session_id\":\"s-child\",\"items\":[],\"next_cursor\":0}"}],"structuredContent":{"session_id":"s-child","items":[],"next_cursor":0}},"error":null}]`,
		},
		{
			name: "spawn_agent_success_is_started_activity",
			items: []*defs.Item{
				newTestCall("c-1", "call-1", "spawn_agent", map[string]any{"task_name": "调研", "message": "go"}),
				newTestResult("call-1", "", `{"session_id":"s-child","task_name":"调研","status":"accepted"}`, nil),
			},
			want: `[{"type":"subAgentActivity","id":"c-1","kind":"started","agentThreadId":"s-child","agentPath":"/root/调研"}]`,
		},
		{
			name:      "spawn_agent_from_child_extends_its_own_path",
			sessionID: testChildSessionID,
			items: []*defs.Item{
				newTestCall("c-1", "call-1", "spawn_agent", map[string]any{"task_name": "子任务", "message": "go"}),
				newTestResult("call-1", "", `{"session_id":"s-grand","task_name":"子任务","status":"accepted"}`, nil),
			},
			want: `[{"type":"subAgentActivity","id":"c-1","kind":"started","agentThreadId":"s-grand","agentPath":"/root/调研/子任务"}]`,
		},
		{
			name: "spawn_agent_failure_is_mcp_tool_call",
			items: []*defs.Item{
				newTestCall("c-1", "call-1", "spawn_agent", map[string]any{"task_name": "调研", "message": "go"}),
				newTestResult("call-1", "", `null`, &defs.ToolError{
					Code:    "collaboration_error",
					Message: "agent \"x\" is not configured",
				}),
			},
			want: `[{"type":"mcpToolCall","id":"c-1","server":"agent","tool":"spawn_agent","status":"failed","arguments":{"task_name":"调研","message":"go"},"result":null,"error":{"message":"agent \"x\" is not configured"}}]`,
		},
		{
			name:  "spawn_agent_without_result_is_not_shown_yet",
			items: []*defs.Item{newTestCall("c-1", "call-1", "spawn_agent", map[string]any{"task_name": "调研", "message": "go"})},
			want:  `[]`,
		},
		{
			name:  "spawn_agent_interrupted_without_result_fails",
			items: []*defs.Item{newInterruptedTestCall("c-1", "call-1", "spawn_agent", `{"task_name":"调研","message":"go"}`)},
			want:  `[{"type":"mcpToolCall","id":"c-1","server":"agent","tool":"spawn_agent","status":"failed","arguments":{"task_name":"调研","message":"go"},"result":null,"error":null}]`,
		},
		{
			name: "send_message_is_interacted_with_target_path_from_the_map",
			items: []*defs.Item{
				newTestCall("c-1", "call-1", "send_message", map[string]any{"session_id": "s-grand", "message": "hi"}),
				newTestResult("call-1", "", `{"session_id":"s-grand","mode":"normal","status":"accepted"}`, nil),
			},
			want: `[{"type":"subAgentActivity","id":"c-1","kind":"interacted","agentThreadId":"s-grand","agentPath":"/root/调研/子任务"}]`,
		},
		{
			name:      "send_message_to_parent_uses_the_root_path",
			sessionID: testChildSessionID,
			items: []*defs.Item{
				newTestCall("c-1", "call-1", "send_message", map[string]any{"session_id": "s-root", "message": "done"}),
				newTestResult("call-1", "", `{"session_id":"s-root","mode":"normal","status":"accepted"}`, nil),
			},
			want: `[{"type":"subAgentActivity","id":"c-1","kind":"interacted","agentThreadId":"s-root","agentPath":"/root"}]`,
		},
		{
			name: "send_message_to_unknown_session_falls_back_to_its_id",
			items: []*defs.Item{
				newTestCall("c-1", "call-1", "send_message", map[string]any{"session_id": "s-unknown", "message": "hi"}),
				newTestResult("call-1", "", `{"session_id":"s-unknown","mode":"normal","status":"accepted"}`, nil),
			},
			want: `[{"type":"subAgentActivity","id":"c-1","kind":"interacted","agentThreadId":"s-unknown","agentPath":"/root/s-unknown"}]`,
		},
		{
			name: "send_message_failure_is_mcp_tool_call",
			items: []*defs.Item{
				newTestCall("c-1", "call-1", "send_message", map[string]any{"session_id": "s-other", "message": "hi"}),
				newTestResult("call-1", "", `null`, &defs.ToolError{
					Code:    "collaboration_error",
					Message: "Session s-other is not a descendant",
				}),
			},
			want: `[{"type":"mcpToolCall","id":"c-1","server":"agent","tool":"send_message","status":"failed","arguments":{"session_id":"s-other","message":"hi"},"result":null,"error":{"message":"Session s-other is not a descendant"}}]`,
		},
		{
			name: "interrupt_agent_is_interrupted_activity",
			items: []*defs.Item{
				newTestCall("c-1", "call-1", "interrupt_agent", map[string]any{"session_id": "s-child"}),
				newTestResult("call-1", "", `{"session_id":"s-child","turn_id":"turn-9","status":"cancel_requested"}`, nil),
			},
			want: `[{"type":"subAgentActivity","id":"c-1","kind":"interrupted","agentThreadId":"s-child","agentPath":"/root/调研"}]`,
		},
		{
			name:  "subagent_notification_is_completed_activity_with_message_id",
			items: []*defs.Item{childNotification},
			want:  `[{"type":"subAgentActivity","id":"note-1","kind":"completed","agentThreadId":"s-child","agentPath":"/root/调研"}]`,
		},
		{
			name:  "other_agents_messages_are_hidden",
			items: []*defs.Item{newTestMessage("m-1", testChildAgent, newTestText("progress")), newTestMessage("m-2", nil, newTestText("no sender"))},
			want:  `[]`,
		},
		{
			name: "context_summary_is_context_compaction",
			items: []*defs.Item{{
				ID:     "sum-1",
				TurnID: "turn-1",
				Kind:   defs.ItemContextSummary,
				ContextSummary: &defs.ContextSummary{
					Text:                 "summary",
					CoveredThroughItemID: "m-1",
				},
			}},
			want: `[{"type":"contextCompaction","id":"sum-1"}]`,
		},
		{
			name: "bookkeeping_items_are_hidden",
			items: []*defs.Item{
				{
					ID:          "x-1",
					Kind:        defs.ItemModelChange,
					ModelChange: &defs.ModelChange{},
				},
				{
					ID:              "x-2",
					Kind:            defs.ItemExecutorsChange,
					ExecutorsChange: &defs.ExecutorsChange{},
				},
				{
					ID:                "x-3",
					Kind:              defs.ItemAgentsInstruction,
					AgentsInstruction: &defs.AgentsInstruction{Text: "rules"},
				},
				{
					ID:               "x-4",
					Kind:             defs.ItemSkillInstruction,
					SkillInstruction: &defs.SkillInstruction{Text: "skill"},
				},
				{
					ID:         "x-5",
					Kind:       defs.ItemModelError,
					ModelError: &defs.ModelError{},
				},
				{
					ID:   "x-6",
					Kind: defs.ItemHookRun,
					HookRun: &defs.HookRun{
						Event:       defs.HookEventStop,
						Status:      defs.HookRunBlocked,
						BlockReason: "run tests",
					},
				},
				newTestMessage("m-1", testUser, newTestText("after")),
			},
			want: `[{"type":"userMessage","id":"m-1","clientId":null,"content":[{"type":"text","text":"after","text_elements":[]}]}]`,
		},
		{
			name:      "child_session_drops_everything_before_the_fork",
			sessionID: testChildSessionID,
			items: []*defs.Item{
				newTestMessage("old-1", testUser, newTestText("copied from parent")),
				newTestMessage("old-2", testRootAgent, newTestText("copied reply")),
				newTestCall("old-3", "call-old", "read_file", map[string]any{"path": "a"}),
				newTestResult("call-old", "local", `{"content":"x"}`, nil),
				{
					ID:          "fork-1",
					Kind:        defs.ItemSessionFork,
					SessionFork: &defs.SessionFork{SourceSessionID: testRootSessionID},
				},
				newTestMessage("task-1", testRootAgent, newTestText("the task from the parent")),
				newTestReply("m-1", testChildAgent, newTestText("child reply")),
			},
			want: `[{"type":"agentMessage","id":"m-1","text":"child reply","phase":"final_answer"}]`,
		},
		{
			name: "parallel_calls_keep_call_order_and_pair_by_call_id",
			items: []*defs.Item{
				newTestCall("c-a", "call-a", "bash", map[string]any{"command": "echo a"}),
				newTestCall("c-b", "call-b", "read_file", map[string]any{"path": "b"}),
				newTestResult("call-b", "local", `{"content":"b"}`, nil),
				newTestResult("call-a", "local", `{"command_id":"cmd_000000000001","status":"exited","exit_code":0,"output":"a\n","next_cursor":2,"has_more":false}`, nil),
			},
			want: `[
				{"type":"commandExecution","id":"c-a","command":"bash -lc 'echo a'","cwd":"/work","status":"completed","aggregatedOutput":"a\n","exitCode":0,"commandActions":[],"source":"agent","processId":null,"durationMs":null},
				{"type":"mcpToolCall","id":"c-b","server":"local","tool":"read_file","status":"completed","arguments":{"path":"b"},"result":{"content":[{"type":"text","text":"b"}],"structuredContent":{"content":"b"}},"error":null}
			]`,
		},
		{
			name: "reused_call_id_pairs_each_result_with_the_latest_open_call",
			items: []*defs.Item{
				newTestCall("c-1", "call-1", "read_file", map[string]any{"path": "a"}),
				newTestResult("call-1", "local", `{"content":"a"}`, nil),
				newTestCall("c-2", "call-1", "read_file", map[string]any{"path": "b"}),
			},
			want: `[
				{"type":"mcpToolCall","id":"c-1","server":"local","tool":"read_file","status":"completed","arguments":{"path":"a"},"result":{"content":[{"type":"text","text":"a"}],"structuredContent":{"content":"a"}},"error":null},
				{"type":"mcpToolCall","id":"c-2","server":"local","tool":"read_file","status":"inProgress","arguments":{"path":"b"},"result":null,"error":null}
			]`,
		},
		{
			name:  "result_without_call_is_dropped",
			items: []*defs.Item{newTestResult("call-1", "local", `{"content":"a"}`, nil)},
			want:  `[]`,
		},
		{
			name:  "call_fragment_without_name_or_id_is_dropped",
			items: []*defs.Item{newInterruptedTestCall("c-1", "", "", `{"pa`), newInterruptedTestCall("c-2", "", "bash", `{"comm`)},
			want:  `[]`,
		},
	}
	// 2. 按场景选择 Session，转换日志并核对完整 Item JSON。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessionID := tt.sessionID
			if sessionID == "" {
				sessionID = testRootSessionID
			}
			entries := newTestTranslator(sessionID).getThreadItems(tt.items)
			threadItems := make([]codexThreadItem, len(entries))
			for index, entry := range entries {
				threadItems[index] = entry.Item
			}
			assertJSONEqual(t, threadItems, tt.want)
		})
	}
}

// TestCodexItemTranslatorEntries 检查条目所属 Turn 和开始完成时间。
func TestCodexItemTranslatorEntries(t *testing.T) {
	// 1. 准备已完成调用、执行中调用、已保存回复和草稿回复。
	call := newTestCall("c-1", "call-1", "read_file", map[string]any{"path": "a"})
	call.TurnID, call.UpdatedAt = "turn-7", 100
	result := newTestResult("call-1", "local", `{"content":"a"}`, nil)
	result.UpdatedAt = 250
	pendingCall := newTestCall("c-2", "call-2", "read_file", map[string]any{"path": "b"})
	pendingCall.TurnID, pendingCall.UpdatedAt = "turn-8", 300
	savedReply := newTestReply("m-1", testRootAgent, newTestText("saved"))
	savedReply.TurnID, savedReply.UpdatedAt = "turn-8", 400
	draftReply := newTestReply("m-2", testRootAgent, newTestText("draft"))
	draftReply.TurnID = "turn-9"
	tests := []struct {
		name  string
		items []*defs.Item
		want  string
	}{
		{
			name:  "tool_call_spans_call_to_result",
			items: []*defs.Item{call, result},
			want: `[{"turnId":"turn-7","startedAtMs":100,"completedAtMs":250,"item":{"type":"mcpToolCall","id":"c-1","server":"local","tool":"read_file","status":"completed",
				"arguments":{"path":"a"},"result":{"content":[{"type":"text","text":"a"}],"structuredContent":{"content":"a"}},"error":null}}]`,
		},
		{
			name:  "tool_call_without_result_is_not_completed",
			items: []*defs.Item{pendingCall},
			want: `[{"turnId":"turn-8","startedAtMs":300,"completedAtMs":null,"item":{"type":"mcpToolCall","id":"c-2","server":"local","tool":"read_file","status":"inProgress",
				"arguments":{"path":"b"},"result":null,"error":null}}]`,
		},
		{
			name:  "standalone_item_uses_its_save_time_for_both_ends",
			items: []*defs.Item{savedReply},
			want:  `[{"turnId":"turn-8","startedAtMs":400,"completedAtMs":400,"item":{"type":"agentMessage","id":"m-1","text":"saved","phase":"final_answer"}}]`,
		},
		{
			name:  "unsaved_item_has_no_times",
			items: []*defs.Item{draftReply},
			want:  `[{"turnId":"turn-9","startedAtMs":null,"completedAtMs":null,"item":{"type":"agentMessage","id":"m-2","text":"draft","phase":"final_answer"}}]`,
		},
	}
	// 2. 转换日志条目，核对 Turn ID、开始时间和完成时间的 JSON。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertJSONEqual(t, newTestTranslator(testRootSessionID).getThreadItems(tt.items), tt.want)
		})
	}
}

// TestCodexItemTranslatorLiveItems 检查实时开始、完成和独立 Item 的转换。
func TestCodexItemTranslatorLiveItems(t *testing.T) {
	// 1. 准备命令、协作、文件补丁和普通工具的开始与完成结果。
	bashCall := newTestCall("c-1", "call-1", "bash", map[string]any{"command": "echo hi"})
	bashResult := newTestResult("call-1", "local", `{"command_id":"cmd_000000000001","status":"exited","exit_code":0,"output":"hi\n","next_cursor":3,"has_more":false}`, nil)
	spawnCall := newTestCall("c-2", "call-2", "spawn_agent", map[string]any{"task_name": "调研", "message": "go"})
	spawnResult := newTestResult("call-2", "", `{"session_id":"s-child","task_name":"调研","status":"accepted"}`, nil)
	sendCall := newTestCall("c-3", "call-3", "send_message", map[string]any{"session_id": "s-child", "message": "hi"})
	waitCall := newTestCall("c-4", "call-4", "wait_agent", `{}`)
	readCall := newTestCall("c-5", "call-5", "read_file", map[string]any{"path": "a"})
	patchCall := newTestCall("c-6", "call-6", "apply_patch", map[string]any{"patch": "*** Begin Patch\n*** Add File: a.txt\n+one\n*** End Patch"})
	const bashRunning = `{"type":"commandExecution","id":"c-1","command":"bash -lc 'echo hi'","cwd":"/work","status":"inProgress","aggregatedOutput":null,"exitCode":null,"commandActions":[],"source":"agent","processId":null,"durationMs":null}`
	tests := []struct {
		name string
		run  func(translator *codexItemTranslator) codexThreadItem
		want string
	}{
		{
			name: "bash_started_is_in_progress",
			run:  func(tr *codexItemTranslator) codexThreadItem { return tr.getStartedItem(bashCall) },
			want: bashRunning,
		},
		{
			name: "bash_completed_has_output",
			run:  func(tr *codexItemTranslator) codexThreadItem { return tr.getCompletedItem(bashCall, bashResult) },
			want: `{"type":"commandExecution","id":"c-1","command":"bash -lc 'echo hi'","cwd":"/work","status":"completed","aggregatedOutput":"hi\n","exitCode":0,"commandActions":[],"source":"agent","processId":null,"durationMs":null}`,
		},
		{
			name: "interrupted_call_completes_as_failed",
			run:  func(tr *codexItemTranslator) codexThreadItem { return tr.getCompletedItem(bashCall, nil) },
			want: `{"type":"commandExecution","id":"c-1","command":"bash -lc 'echo hi'","cwd":"/work","status":"failed","aggregatedOutput":null,"exitCode":null,"commandActions":[],"source":"agent","processId":null,"durationMs":null}`,
		},
		{
			name: "mcp_tool_call_started",
			run:  func(tr *codexItemTranslator) codexThreadItem { return tr.getStartedItem(readCall) },
			want: `{"type":"mcpToolCall","id":"c-5","server":"local","tool":"read_file","status":"inProgress","arguments":{"path":"a"},"result":null,"error":null}`,
		},
		{
			name: "spawn_agent_waits_for_its_result",
			run:  func(tr *codexItemTranslator) codexThreadItem { return tr.getStartedItem(spawnCall) },
			want: `null`,
		},
		{
			name: "spawn_agent_completed_is_started_activity",
			run:  func(tr *codexItemTranslator) codexThreadItem { return tr.getCompletedItem(spawnCall, spawnResult) },
			want: `{"type":"subAgentActivity","id":"c-2","kind":"started","agentThreadId":"s-child","agentPath":"/root/调研"}`,
		},
		{
			name: "send_message_waits_for_its_result",
			run:  func(tr *codexItemTranslator) codexThreadItem { return tr.getStartedItem(sendCall) },
			want: `null`,
		},
		{
			name: "interrupted_send_message_completes_as_failed_mcp_tool_call",
			run:  func(tr *codexItemTranslator) codexThreadItem { return tr.getCompletedItem(sendCall, nil) },
			want: `{"type":"mcpToolCall","id":"c-3","server":"agent","tool":"send_message","status":"failed","arguments":{"session_id":"s-child","message":"hi"},"result":null,"error":null}`,
		},
		{
			name: "apply_patch_waits_for_its_result",
			run:  func(tr *codexItemTranslator) codexThreadItem { return tr.getStartedItem(patchCall) },
			want: `null`,
		},
		{
			name: "interrupted_apply_patch_completes_as_failed",
			run:  func(tr *codexItemTranslator) codexThreadItem { return tr.getCompletedItem(patchCall, nil) },
			want: `{"type":"fileChange","id":"c-6","status":"failed","changes":[{"path":"/work/a.txt","kind":{"type":"add"},"diff":"one\n"}]}`,
		},
		{
			name: "wait_agent_started_is_hidden",
			run:  func(tr *codexItemTranslator) codexThreadItem { return tr.getStartedItem(waitCall) },
			want: `null`,
		},
		{
			name: "wait_agent_completed_is_hidden",
			run:  func(tr *codexItemTranslator) codexThreadItem { return tr.getCompletedItem(waitCall, nil) },
			want: `null`,
		},
		{
			name: "streaming_reply_shows_the_text_so_far",
			run: func(tr *codexItemTranslator) codexThreadItem {
				return tr.getStandaloneItem(newTestReply("m-1", testRootAgent, newTestText("par")))
			},
			want: `{"type":"agentMessage","id":"m-1","text":"par"}`,
		},
		{
			name: "tool_result_is_not_a_standalone_item",
			run:  func(tr *codexItemTranslator) codexThreadItem { return tr.getStandaloneItem(bashResult) },
			want: `null`,
		},
	}
	// 2. 调用开始、完成或独立 Item 转换，核对完整 JSON。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertJSONEqual(t, tt.run(newTestTranslator(testRootSessionID)), tt.want)
		})
	}
}

// TestIsToolCallSplit 检查正倒序页的调用结果是否被拆开。
func TestIsToolCallSplit(t *testing.T) {
	// 1. 准备配对、并行、未完成和中断调用。
	callA, resultA := newTestCall("c-a", "call-a", "bash", `{}`), newTestResult("call-a", "local", `{}`, nil)
	callB, resultB := newTestCall("c-b", "call-b", "bash", `{}`), newTestResult("call-b", "local", `{}`, nil)
	cutCall := newInterruptedTestCall("c-i", "call-i", "bash", `{}`)
	reply := newTestMessage("m-1", testRootAgent, newTestText("text"))
	tests := []struct {
		name  string
		items []*defs.Item
		order storage.Direction
		want  bool
	}{
		{
			name:  "empty_page",
			order: storage.DirectionDesc,
			want:  false,
		},
		{
			name:  "desc_ends_with_result_whose_call_is_older",
			items: []*defs.Item{reply, resultA},
			order: storage.DirectionDesc,
			want:  true,
		},
		{
			name:  "desc_ends_with_call_of_its_result",
			items: []*defs.Item{reply, resultA, callA},
			order: storage.DirectionDesc,
			want:  false,
		},
		{
			name:  "desc_parallel_result_in_the_middle_lost_its_call",
			items: []*defs.Item{resultB, resultA, callB},
			order: storage.DirectionDesc,
			want:  true,
		},
		{
			name:  "desc_call_whose_result_is_newer_is_not_a_split",
			items: []*defs.Item{callA, reply},
			order: storage.DirectionDesc,
			want:  false,
		},
		{
			name:  "asc_ends_with_call_whose_result_is_newer",
			items: []*defs.Item{reply, callA},
			order: storage.DirectionAsc,
			want:  true,
		},
		{
			name:  "asc_call_followed_by_its_result",
			items: []*defs.Item{callA, resultA, reply},
			order: storage.DirectionAsc,
			want:  false,
		},
		{
			name:  "asc_parallel_call_in_the_middle_lost_its_result",
			items: []*defs.Item{callA, callB, resultA},
			order: storage.DirectionAsc,
			want:  true,
		},
		{
			name:  "asc_interrupted_call_never_gets_a_result",
			items: []*defs.Item{reply, cutCall},
			order: storage.DirectionAsc,
			want:  false,
		},
		{
			name:  "asc_result_whose_call_is_older_is_not_a_split",
			items: []*defs.Item{resultA, callB, resultB},
			order: storage.DirectionAsc,
			want:  false,
		},
	}
	// 2. 按指定方向检查配对，核对是否需要继续读取。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isToolCallSplit(tt.items, tt.order); got != tt.want {
				t.Fatalf("isToolCallSplit = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestGetAgentPath 检查任务名层级的 Agent 路径。
func TestGetAgentPath(t *testing.T) {
	// 1. 准备根、子和孙 Agent 的任务名层级。
	tests := []struct {
		name      string
		taskNames []string
		want      string
	}{
		{
			name: "root_agent",
			want: "/root",
		},
		{
			name:      "child",
			taskNames: []string{"调研"},
			want:      "/root/调研",
		},
		{
			name:      "grandchild",
			taskNames: []string{"调研", "子任务"},
			want:      "/root/调研/子任务",
		},
	}
	// 2. 拼接任务路径，核对根、子和孙 Agent 的文本。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getAgentPath(tt.taskNames); got != tt.want {
				t.Fatalf("getAgentPath(%v) = %q, want %q", tt.taskNames, got, tt.want)
			}
		})
	}
}

// TestGetAgentPaths 检查 Session 父链生成的 Agent 路径。
func TestGetAgentPaths(t *testing.T) {
	// 1. 准备乱序父子、缺少父记录和父链成环的 Session。
	tests := []struct {
		name     string
		sessions []defs.Session
		want     map[defs.SessionID]string
	}{
		{
			name:     "root_only",
			sessions: []defs.Session{{ID: "root"}},
			want:     map[defs.SessionID]string{"root": "/root"},
		},
		{
			name: "children_and_grandchild",
			// 子 Session 排在父 Session 前面，预期仍按父链生成路径。
			sessions: []defs.Session{
				{
					ID:              "grandchild",
					TaskName:        "写测试",
					ParentSessionID: "child",
				},
				{
					ID:              "child",
					TaskName:        "review",
					ParentSessionID: "root",
				},
				{ID: "root"},
				{
					ID:              "sibling",
					TaskName:        "docs",
					ParentSessionID: "root",
				},
			},
			want: map[defs.SessionID]string{"root": "/root", "child": "/root/review", "grandchild": "/root/review/写测试", "sibling": "/root/docs"},
		},
		{
			name: "missing_parent_ends_the_chain",
			sessions: []defs.Session{{
				ID:              "orphan",
				TaskName:        "lost",
				ParentSessionID: "gone",
			}},
			want: map[defs.SessionID]string{"orphan": "/root/lost"},
		},
		{
			name: "parent_cycle_ends",
			sessions: []defs.Session{{
				ID:              "a",
				TaskName:        "x",
				ParentSessionID: "b",
			}, {
				ID:              "b",
				TaskName:        "y",
				ParentSessionID: "a",
			}},
			want: map[defs.SessionID]string{"a": "/root/y/x", "b": "/root/x/y"},
		},
	}
	// 2. 生成各 Session 路径，核对整张映射。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getAgentPaths(tt.sessions); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("getAgentPaths = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestGetCodexInputContent 检查用户输入转换和输出 schema 的追加。
func TestGetCodexInputContent(t *testing.T) {
	// 1. 准备文本、图片、空 schema 和 null schema。
	const (
		schema      = `{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}`
		instruction = "Respond with only a JSON object that matches this JSON Schema. Do not wrap it in a code block or add any other text.\n"
		imageURL    = "data:image/png;base64,iVBORw0KGgo="
	)
	textContent := func(text string) *defs.Content {
		return &defs.Content{
			Kind: defs.ContentText,
			Text: &defs.TextContent{Text: text},
		}
	}
	imageContent := &defs.Content{
		Kind:  defs.ContentImage,
		Image: &defs.ImageContent{DataURL: imageURL},
	}
	tests := []struct {
		name         string
		input        []codexUserInputParam
		outputSchema json.RawMessage
		want         []*defs.Content
	}{
		{
			name: "text_with_schema",
			input: []codexUserInputParam{{
				Type: "text",
				Text: "Generate a title",
			}},
			outputSchema: json.RawMessage(schema),
			want:         []*defs.Content{textContent("Generate a title" + "\n\n" + instruction + schema)},
		},
		{
			name: "image_only_with_schema",
			input: []codexUserInputParam{{
				Type: "image",
				URL:  imageURL,
			}},
			outputSchema: json.RawMessage(schema),
			want:         []*defs.Content{imageContent, textContent(instruction + schema)},
		},
		{
			name: "without_schema",
			input: []codexUserInputParam{{
				Type: "text",
				Text: "hello",
			}},
			want: []*defs.Content{textContent("hello")},
		},
		{
			name: "null_schema",
			input: []codexUserInputParam{{
				Type: "text",
				Text: "hello",
			}},
			outputSchema: json.RawMessage("null"),
			want:         []*defs.Content{textContent("hello")},
		},
	}
	// 2. 转换输入，核对 Content 列表和是否返回错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := getCodexInputContent(tt.input, tt.outputSchema)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("getCodexInputContent = %s, want %s", toJSON(t, got), toJSON(t, tt.want))
			}
		})
	}
}
