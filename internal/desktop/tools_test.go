package desktop

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/egoist/mygo/ui"
)

// TestToolOutputContent 检查真实字符串进度及各种合法 JSON 根值不丢失。
func TestToolOutputContent(t *testing.T) {
	// 1. 命令进度使用工具实际发送的 JSON 字符串。
	t.Run("command progress", func(t *testing.T) {
		d := testDesktop()
		d.applyToolProgress(&runtimeclient.ToolProgress{Progress: agent.AgentToolProgress{CallID: "call", Content: json.RawMessage(`"hello native\n"`)}})
		call := &defs.ToolCall{CallID: "call", Name: "bash", Arguments: `{"command":"echo hello"}`, Status: defs.ToolCallReady}
		d.itemExpanded["tool-call"] = true
		tester := ui.NewTester(func(c *ui.Context) {
			d.applyAppearance(c)
			ui.Column(c).Fill().Children(func() { d.toolView(c, call, nil, viewItem{Item: &defs.Item{ToolCall: call}}) })
		}, 640, 800)
		if !tester.HasText("hello native") {
			t.Fatalf("missing live output: %q", tester.Texts())
		}
	})
	// 2. MCP 与未知工具允许对象、列表和标量；复制保留完整 JSON。
	for _, tt := range []struct{ name, raw, want string }{{"object", `{"value":"object output"}`, "object output"}, {"array", `["array output",1]`, "array output"}, {"string", `"string output"`, "string output"}, {"number", `42`, "42"}, {"null", `null`, "null"}} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			call := &defs.ToolCall{CallID: "unknown-call", Name: "mcp__fixture__unknown", Arguments: `{"argument":"input"}`, Status: defs.ToolCallReady}
			result := &defs.ToolResult{CallID: call.CallID, Content: json.RawMessage(tt.raw)}
			d.itemExpanded["tool-"+call.CallID] = true
			tester := ui.NewTester(func(c *ui.Context) {
				d.applyAppearance(c)
				ui.Column(c).Fill().Children(func() { d.toolView(c, call, result, viewItem{Item: &defs.Item{ToolCall: call}}) })
			}, 640, 900)
			if !tester.HasText(tt.want) {
				t.Fatalf("missing %s: %q", tt.want, tester.Texts())
			}
			if err := tester.Click("复制 JSON"); err != nil {
				t.Fatal(err)
			}
			var got, want any
			if err := json.Unmarshal([]byte(tester.Clipboard()), &got); err != nil {
				t.Fatal(err)
			}
			_ = json.Unmarshal([]byte(tt.raw), &want)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("copy=%s", tester.Clipboard())
			}
		})
	}
}

// TestCommandFailurePresentation 检查非零退出影响工具标题，不将主动停止判为失败。
func TestCommandFailurePresentation(t *testing.T) {
	for _, tt := range []struct {
		name, raw string
		failed    bool
	}{{"exit failure", `{"status":"exited","exit_code":1}`, true}, {"stopped", `{"status":"exited","exit_code":137,"stop_reason":"stopped"}`, false}, {"transport failure", `{"status":"failed"}`, true}, {"success", `{"status":"exited","exit_code":0}`, false}} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			call := &defs.ToolCall{CallID: "call", Name: "bash", Arguments: `{}`, Status: defs.ToolCallReady}
			result := &defs.ToolResult{CallID: "call", Content: json.RawMessage(tt.raw)}
			tester := ui.NewTester(func(c *ui.Context) {
				d.applyAppearance(c)
				ui.Column(c).Fill().Children(func() { d.toolView(c, call, result, viewItem{Item: &defs.Item{ToolCall: call}}) })
			}, 640, 480)
			tester.SetLocale("zh-Hans")
			if tester.HasText("失败") != tt.failed {
				t.Fatalf("texts=%q", tester.Texts())
			}
		})
	}
}

// TestToolDetailLayout 检查命令、路径详情先给等宽标签，再用两列事实行显示参数和结果字段。
func TestToolDetailLayout(t *testing.T) {
	// 1. 各工具展开后的小节标签与事实行都按公开文字出现。
	for _, tt := range []struct {
		name, args, result string
		want               []string
	}{
		{"bash", `{"command":"go test","cwd":"/work"}`, `{"command_id":"cmd-1","status":"exited","exit_code":0,"output":"PASS"}`, []string{"运行命令", "go test", "cwd", "/work", "command_id", "cmd-1", "输出", "PASS"}},
		{"read_file", `{"path":"a.go"}`, `{"content":"package a","offset":0,"next_offset":9,"size":9,"eof":true}`, []string{"a.go", "内容", "package a", "next_offset", "eof"}},
		{"spawn_agent", `{"message":"review","agent_name":"reviewer","model":"gpt"}`, `{}`, []string{"agent_name", "reviewer", "model", "gpt"}},
		{"send_message", `{"session_id":"s","message":"hi","mode":"steer"}`, `{}`, []string{"mode", "steer"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			call := &defs.ToolCall{CallID: "call", Name: tt.name, Arguments: tt.args, Status: defs.ToolCallReady}
			result := &defs.ToolResult{CallID: "call", Content: json.RawMessage(tt.result)}
			d.itemExpanded["tool-call"] = true
			tester := ui.NewTester(func(c *ui.Context) {
				d.applyAppearance(c)
				ui.Column(c).Fill().Children(func() { d.toolView(c, call, result, viewItem{Item: &defs.Item{ToolCall: call}}) })
			}, 800, 1200)
			tester.SetLocale("zh-Hans")
			for _, want := range tt.want {
				if !tester.HasText(want) {
					t.Fatalf("missing %q in %q", want, tester.Texts())
				}
			}
		})
	}
}
