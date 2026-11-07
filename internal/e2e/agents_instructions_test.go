package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/defs"
)

// TestAgentsInstructionsEndToEnd 检查全局与项目规则进入模型、持久化及服务重启后的恢复。
func TestAgentsInstructionsEndToEnd(t *testing.T) {
	// 1. 构建程序，分别验收全局规则可读和不可读的情况。
	binary := buildOpenVaala(t)
	for _, test := range []struct {
		name         string
		isUnreadable bool
	}{
		{name: "global_and_project_rules"},
		{name: "unreadable_global_does_not_block", isUnreadable: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			// 1. 在隔离 HOME 中写全局规则，并在独立工作区写项目规则。
			fixture := newConversationManagementFixture(t, binary)
			globalPath := filepath.Join(filepath.Dir(fixture.config), ".agents", "AGENTS.md")
			if err := os.MkdirAll(filepath.Dir(globalPath), 0700); err != nil {
				t.Fatal(err)
			}
			globalContent := "GLOBAL_RULE_E2E: Prefer English responses."
			if test.isUnreadable {
				if err := os.Mkdir(globalPath, 0700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(globalPath, []byte(globalContent), 0600); err != nil {
				t.Fatal(err)
			}
			workspace := newE2EWorkspace(t)
			projectPath := filepath.Join(workspace, "AGENTS.md")
			projectContent := "PROJECT_RULE_E2E: Answer in Chinese in this project."
			if err := os.WriteFile(projectPath, []byte(projectContent), 0600); err != nil {
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
			var wantInstructions []defs.AgentsInstruction
			if !test.isUnreadable {
				wantInstructions = append(wantInstructions, defs.AgentsInstruction{
					Executor: defs.LocalExecutorID,
					Path:     globalPath,
					Text:     globalContent,
					IsGlobal: true,
				})
			}
			wantInstructions = append(wantInstructions, defs.AgentsInstruction{
				Executor: defs.LocalExecutorID,
				Path:     projectPath,
				Text:     projectContent,
			})

			// 2. 首轮及重启后的下一轮都检查模型请求和已保存的规则，规则不重复写入历史。
			for _, phase := range []string{"initial", "reopened_after_restart"} {
				if phase == "reopened_after_restart" {
					conversationID := observer.conversation.ID
					observer.stop()
					fixture.stopServe(t)
					fixture.startServe(t)
					observer = openRuntimeObserver(t, fixture.config, conversationID)
				}
				request, _ := fixture.runTurn(t, observer, phase, e2eModelReply{text: "规则已收到"})
				var instructionMessages []modelMessageText
				for _, message := range getModelMessageTexts(request.body) {
					if strings.Contains(message.text, "GLOBAL_RULE_E2E") || strings.Contains(message.text, "PROJECT_RULE_E2E") {
						instructionMessages = append(instructionMessages, message)
					}
				}
				if len(instructionMessages) != len(wantInstructions) {
					t.Fatalf("%s model rule messages = %+v, want %d", phase, instructionMessages, len(wantInstructions))
				}
				for index, want := range wantInstructions {
					message := instructionMessages[index]
					if message.role != "user" || !strings.Contains(message.text, want.Path) || !strings.Contains(message.text, want.Text) {
						t.Errorf("%s model rule message %d = %+v, want %s followed by its content", phase, index, message, want.Path)
					}
					if want.IsGlobal {
						if !strings.Contains(message.text, "适用于此 Executor 的所有工作区") || !strings.Contains(message.text, "项目目录规则优先") || strings.Contains(message.text, "适用于此文件所在目录") {
							t.Errorf("%s global model rule = %q, want all workspaces with project rules taking precedence", phase, message.text)
						}
					} else if !strings.Contains(message.text, "适用于此文件所在目录及后代") {
						t.Errorf("%s project model rule = %q, want directory scope", phase, message.text)
					}
				}

				var savedInstructions []defs.AgentsInstruction
				for _, logged := range fixture.listItems(t, observer.conversation.ID, observer.conversation.RootSessionID) {
					if logged.Item.Kind != defs.ItemAgentsInstruction {
						continue
					}
					instruction := logged.Item.AgentsInstruction
					if instruction == nil {
						t.Fatal("saved agents instruction has no payload")
					}
					savedInstructions = append(savedInstructions, *instruction)
					payload, err := json.Marshal(instruction)
					if err != nil {
						t.Fatal(err)
					}
					if strings.Contains(string(payload), `"is_global":true`) != instruction.IsGlobal ||
						(!instruction.IsGlobal && strings.Contains(string(payload), `"is_global"`)) {
						t.Errorf("%s saved rule JSON = %s, want is_global only for global rules", phase, payload)
					}
				}
				if !slices.Equal(savedInstructions, wantInstructions) {
					t.Errorf("%s saved rules = %+v, want %+v", phase, savedInstructions, wantInstructions)
				}
			}
			// 3. 全局文件不可读时记录警告，项目规则和两轮对话均照常完成。
			if test.isUnreadable {
				records := getE2ELogRecords(t, fixture.config, "global AGENTS.md read failed")
				if !slices.ContainsFunc(records, func(record map[string]any) bool {
					return record["level"] == "WARN" && record["path"] == globalPath
				}) {
					t.Errorf("global rule warnings = %+v, want a warning for %s", records, globalPath)
				}
			}
		})
	}
}
