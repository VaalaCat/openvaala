package main

import (
	"os"
	"strings"
	"sync"
	"testing"
)

// generateOnce 让两个测试共用一次生成结果。
var generateOnce = sync.OnceValues(generateProtocolTypes)

// TestGenerateProtocolTypes 检查生成结果包含协议版本、方法表、通知表、嵌入继承和错误码等关键声明。
func TestGenerateProtocolTypes(t *testing.T) {
	output, err := generateOnce()
	if err != nil {
		t.Fatalf("generateProtocolTypes() error = %v", err)
	}

	cases := []struct{ name, want string }{
		{"header", "// 由 internal/protocolgen 生成，不要手改"},
		{"protocol_version", `export const ProtocolVersion = "5"`},
		{"agent_list_method", `"agent/list": ListAgentsResult`},
		{"agent_info_fields", "export interface AgentInfo {\n    name: string;\n    description?: string;"},
		{"terminal_open_method", `"terminal/open": TerminalOpenParams`},
		{"terminal_data_base64_string", "export interface TerminalOutputParams {\n    terminal_id: string;\n    data: string;"},
		{"params_interface", "export interface SubmitInputParams"},
		{"method_params", `"conversation/submitInput": SubmitInputParams`},
		{"method_result_pointer_stripped", `"conversation/create": ConversationResult`},
		{"skill_list_slice", `"skill/list": SkillInfo[]`},
		{"notification", `"item/delta": ItemEventParams`},
		{"running_params", `"conversation/running/subscribe": EmptyParams`},
		{"running_result", `"conversation/running/subscribe": RunningConversations`},
		{"running_unsubscribe", `"conversation/running/unsubscribe": EmptyResult`},
		{"running_changed", `"conversation/runningChanged": RunningConversations`},
		{"running_closed", `"conversation/runningClosed": RunningConversationsClosedParams`},
		{"item_kind_union", `"turn_interrupted"`},
		{"embedded_extends", "extends NotificationTarget"},
		{"error_code_session_busy", "export const ErrorCodeSessionBusy = -32001"},
		{"error_code_invalid_request", "export const ErrorCodeInvalidRequest = -32600"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(output, tc.want) {
				t.Errorf("generated output does not contain %q", tc.want)
			}
		})
	}
}

// TestGeneratedProtocolTypesUpToDate 检查仓库里的 protocol.gen.ts 与当前协议类型的生成结果一致。
func TestGeneratedProtocolTypesUpToDate(t *testing.T) {
	want, err := generateOnce()
	if err != nil {
		t.Fatalf("generateProtocolTypes() error = %v", err)
	}
	got, err := os.ReadFile("../../www/src/lib/api/protocol.gen.ts")
	if err != nil {
		t.Fatalf("read protocol.gen.ts: %v", err)
	}
	if string(got) != want {
		t.Error("protocol.gen.ts is stale; run pnpm --dir www generate:types")
	}
}
