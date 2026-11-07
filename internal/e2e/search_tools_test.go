package e2e

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/defs"
)

// TestSearchToolsEndToEnd 检查只给 read_file、glob、grep 的 Agent 能搜到工作区里的文件，工具结果进入下一次模型请求。
func TestSearchToolsEndToEnd(t *testing.T) {
	// 1. 构建程序并启动服务，把 Agent 的工具限定为 read_file、glob、grep 后重启服务。
	binary := buildOpenVaala(t)
	fixture := newConversationManagementFixture(t, binary)
	fixture.stopServe(t)
	agentPath := filepath.Join(filepath.Dir(fixture.config), "agents", "assistant.yaml")
	payload, err := os.ReadFile(agentPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agentPath, append(payload, "tools: [read_file, glob, grep]\n"...), 0600); err != nil {
		t.Fatal(err)
	}
	fixture.startServe(t)
	// 2. 准备两种搜索：grep 找函数所在的行，glob 按通配符列文件。
	tests := []*struct {
		name       string
		tool       string
		arguments  string
		wantResult []string
	}{
		{name: "grep_finds_function_line", tool: "grep", arguments: `{"pattern":"func TargetFunc"}`, wantResult: []string{`"path":"target.go"`, `"line":3`}},
		{name: "glob_finds_file", tool: "glob", arguments: `{"pattern":"*.go"}`, wantResult: []string{`"paths":["target.go"]`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 1. 在含 target.go 的目录里新建对话，提交输入。
			workspace := t.TempDir()
			if err := os.WriteFile(filepath.Join(workspace, "target.go"), []byte("package main\n\nfunc TargetFunc() {}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			record, err := fixture.control.CreateConversation(t.Context(), &runtimeclient.CreateConversationRequest{
				AgentName:        "assistant",
				ExecutorSettings: defs.ExecutorSettings{WorkspaceDirectories: map[string]string{defs.LocalExecutorID: workspace}},
			})
			if err != nil {
				t.Fatal(err)
			}
			observer := openRuntimeObserver(t, fixture.config, record.ID)
			observer.startTurn(t, "find TargetFunc")
			// 2. 第一次请求带有 glob、grep，没有 bash；模型调用搜索工具。
			first := fixture.model.next(t)
			names := getModelToolNames(first.body)
			if !slices.Contains(names, "glob") || !slices.Contains(names, "grep") || slices.Contains(names, "bash") {
				t.Errorf("first model request tools = %v, want glob and grep without bash", names)
			}
			first.reply <- e2eModelReply{
				tool:      tt.tool,
				arguments: tt.arguments,
			}
			// 3. 第二次请求里的工具结果包含目标路径和行号，模型再回复文字后 Turn 正常结束。
			second := fixture.model.next(t)
			result := getModelToolResult(t, second.body)
			if result.Error != nil {
				t.Fatalf("tool result error = %+v, want none", result.Error)
			}
			for _, want := range tt.wantResult {
				if !strings.Contains(string(result.Content), want) {
					t.Errorf("tool result = %s, want containing %s", result.Content, want)
				}
			}
			second.reply <- e2eModelReply{text: "found"}
			readTurnFinished(t, observer, defs.TurnStatusCompleted)
		})
	}
}
