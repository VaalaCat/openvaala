package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/VaalaCat/openvaala/internal/config"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/defs"
)

// memoryIndexMarker 是记忆目录消息的开头，用来从模型请求里找出它们。
const memoryIndexMarker = "跨对话记忆目录"

// getMemoryDirectory 返回 serve 的记忆根目录：数据目录下的 memory。
func (f *conversationManagementFixture) getMemoryDirectory() string {
	// 1. 配置所在目录是 serve 的 HOME，数据目录在 .local/share/openvaala。
	return filepath.Join(filepath.Dir(f.config), ".local", "share", "openvaala", "memory")
}

// getProjectMemoryDirectory 返回默认 Executor 和它的根目录对应的项目记忆目录。
func (f *conversationManagementFixture) getProjectMemoryDirectory(executorID, rootDirectory string) string {
	// 1. 根目录里的 / 换成 -，放在 projects/<Executor> 下。
	return filepath.Join(f.getMemoryDirectory(), "projects", executorID, strings.ReplaceAll(rootDirectory, "/", "-"))
}

// writeMemoryFile 在记忆目录里写一个文件，父目录不存在时一并创建。
func writeMemoryFile(t *testing.T, path, content string) {
	t.Helper()
	// 1. 创建父目录再写文件。
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// getMemoryMessages 返回模型请求里的记忆目录消息，按出现的顺序。
func getMemoryMessages(body map[string]any) []modelMessageText {
	// 1. 记忆目录以 user 消息发给模型，开头是固定的说明文字。
	return slices.DeleteFunc(getModelMessageTexts(body), func(message modelMessageText) bool {
		return message.role != "user" || !strings.Contains(message.text, memoryIndexMarker)
	})
}

// openMemoryConversation 在指定设置下新建对话，并用独立 Client 跟随根 Session。
func (f *conversationManagementFixture) openMemoryConversation(t *testing.T, settings defs.ExecutorSettings) *runtimeObserver {
	t.Helper()
	// 1. 创建对话再打开观察连接。
	record, err := f.control.CreateConversation(t.Context(), &runtimeclient.CreateConversationRequest{
		AgentName:        "assistant",
		ExecutorSettings: settings,
	})
	if err != nil {
		t.Fatal(err)
	}
	return openRuntimeObserver(t, f.config, record.ID)
}

// TestMemoryEndToEnd 检查跨对话记忆：写入后新对话能带上、不串项目、读失败不影响对话、压缩后重新放、超长截断。
func TestMemoryEndToEnd(t *testing.T) {
	// 1. 构建程序；每个场景各起一个服务，因为用户级记忆目录是所有对话共用的。
	binary := buildOpenVaala(t)
	longIndex := strings.Repeat("- [topic](topic.md) — 一句能直接用的结论，补足长度\n", 200)
	tests := []struct {
		name string
		run  func(t *testing.T, f *conversationManagementFixture)
	}{
		{
			name: "projects_do_not_share_memory",
			run: func(t *testing.T, f *conversationManagementFixture) {
				// 1. 目录 X 下的对话 A 让 Agent 把两份记忆写进 memory Executor；这一轮开始时还没有记忆目录。
				directoryX, directoryY := newE2EWorkspace(t), newE2EWorkspace(t)
				settingsX := defs.ExecutorSettings{WorkspaceDirectories: map[string]string{defs.LocalExecutorID: directoryX}}
				observer := f.openMemoryConversation(t, settingsX)
				observer.startTurn(t, "记住：这个项目的测试用 table-driven，回复用中文")
				first := f.model.next(t)
				if memory := getMemoryMessages(first.body); len(memory) != 0 {
					t.Errorf("first turn memory messages = %+v, want none before anything is remembered", memory)
				}
				patch := "*** Begin Patch\n*** Add File: project/MEMORY.md\n+- 测试用 table-driven\n*** Add File: user/MEMORY.md\n+- 回复用中文\n*** End Patch"
				arguments, err := json.Marshal(map[string]string{"executor": defs.MemoryExecutorID, "patch": patch})
				if err != nil {
					t.Fatal(err)
				}
				first.reply <- e2eModelReply{tool: "apply_patch", arguments: string(arguments)}
				// 2. 工具结果没有错误，两份文件落在 serve 机器的记忆目录里。
				second := f.model.next(t)
				result := getModelToolResult(t, second.body)
				if result.Error != nil || !strings.Contains(string(result.Content), `"added":["project/MEMORY.md","user/MEMORY.md"]`) {
					t.Fatalf("apply_patch result = content %s error %+v, want both memory files added", result.Content, result.Error)
				}
				second.reply <- e2eModelReply{text: "记住了"}
				readTurnFinished(t, observer, defs.TurnStatusCompleted)
				projectPath := filepath.Join(f.getProjectMemoryDirectory(defs.LocalExecutorID, directoryX), "MEMORY.md")
				for path, want := range map[string]string{
					projectPath: "- 测试用 table-driven\n",
					filepath.Join(f.getMemoryDirectory(), "user", "MEMORY.md"): "- 回复用中文\n",
				} {
					if content, err := os.ReadFile(path); err != nil || string(content) != want {
						t.Fatalf("%s = %q, %v; want %q", path, content, err, want)
					}
				}
				// 3. 同一目录新开的对话 B 第一次请求里有两份，用户级在前，还能在 memory 里 grep 到刚写的那一行；另一个目录的对话 C 只有用户级。
				observerB := f.openMemoryConversation(t, settingsX)
				observerB.startTurn(t, "继续")
				requestB := f.model.next(t)
				requestB.reply <- e2eModelReply{tool: "grep", arguments: `{"executor":"memory","pattern":"table-driven"}`}
				searchB := f.model.next(t)
				if search := getModelToolResult(t, searchB.body); search.Error != nil || !strings.Contains(string(search.Content), `"path":"project/MEMORY.md"`) {
					t.Errorf("grep in memory result = content %s error %+v, want a match in project/MEMORY.md", search.Content, search.Error)
				}
				searchB.reply <- e2eModelReply{text: "好"}
				readTurnFinished(t, observerB, defs.TurnStatusCompleted)
				memoryB := getMemoryMessages(requestB.body)
				if len(memoryB) != 2 || !strings.Contains(memoryB[0].text, "Path: user/MEMORY.md") || !strings.Contains(memoryB[0].text, "- 回复用中文") ||
					!strings.Contains(memoryB[1].text, "Path: project/MEMORY.md") || !strings.Contains(memoryB[1].text, "- 测试用 table-driven") {
					t.Errorf("conversation B memory messages = %+v, want the user memory then the project memory", memoryB)
				}
				requestC, _ := f.runTurn(t, f.openMemoryConversation(t, defs.ExecutorSettings{
					WorkspaceDirectories: map[string]string{defs.LocalExecutorID: directoryY},
				}), "继续", e2eModelReply{text: "好"})
				memoryC := getMemoryMessages(requestC.body)
				if len(memoryC) != 1 || !strings.Contains(memoryC[0].text, "Path: user/MEMORY.md") {
					t.Errorf("conversation C memory messages = %+v, want only the user memory", memoryC)
				}
			},
		},
		{
			name: "conversation_with_only_virtual_executor",
			run: func(t *testing.T, f *conversationManagementFixture) {
				// 1. 默认 Executor 是 openvaala（virtual）的对话，不依赖本机目录，也有用户级记忆和 projects/openvaala/-workspace。
				writeMemoryFile(t, filepath.Join(f.getMemoryDirectory(), "user", "MEMORY.md"), "- 回复用中文\n")
				writeMemoryFile(t, filepath.Join(f.getProjectMemoryDirectory(config.BuiltinExecutorID, "/workspace"), "MEMORY.md"), "- 临时环境别装软件\n")
				observer := f.openMemoryConversation(t, defs.ExecutorSettings{ExecutorIDs: []string{config.BuiltinExecutorID, defs.MemoryExecutorID}})
				request, _ := f.runTurn(t, observer, "你好", e2eModelReply{text: "好"})
				memory := getMemoryMessages(request.body)
				if len(memory) != 2 || !strings.Contains(memory[0].text, "- 回复用中文") || !strings.Contains(memory[1].text, "- 临时环境别装软件") {
					t.Errorf("memory messages = %+v, want the user memory and the openvaala project memory", memory)
				}
			},
		},
		{
			name: "unreadable_memory_does_not_block_turn",
			run: func(t *testing.T, f *conversationManagementFixture) {
				// 1. user/MEMORY.md 是个目录，读取失败；project 的 MEMORY.md 正常。
				directory := newE2EWorkspace(t)
				if err := os.MkdirAll(filepath.Join(f.getMemoryDirectory(), "user", "MEMORY.md"), 0700); err != nil {
					t.Fatal(err)
				}
				writeMemoryFile(t, filepath.Join(f.getProjectMemoryDirectory(defs.LocalExecutorID, directory), "MEMORY.md"), "- 测试用 table-driven\n")
				observer := f.openMemoryConversation(t, defs.ExecutorSettings{WorkspaceDirectories: map[string]string{defs.LocalExecutorID: directory}})
				// 2. Turn 照常完成，请求里只有读得出来的那份，日志里有一条 Warn。
				request, _ := f.runTurn(t, observer, "你好", e2eModelReply{text: "好"})
				memory := getMemoryMessages(request.body)
				if len(memory) != 1 || !strings.Contains(memory[0].text, "Path: project/MEMORY.md") {
					t.Errorf("memory messages = %+v, want only the readable project memory", memory)
				}
				records := getE2ELogRecords(t, f.config, "memory index read failed")
				if !slices.ContainsFunc(records, func(record map[string]any) bool {
					return record["level"] == "WARN" && record["path"] == "user/MEMORY.md"
				}) {
					t.Errorf("read failure warnings = %+v, want a warning for user/MEMORY.md", records)
				}
			},
		},
		{
			name: "compaction_loads_memory_again_after_summary",
			run: func(t *testing.T, f *conversationManagementFixture) {
				// 1. 第一轮带上用户级记忆，第二轮历史里已经有它，不重复放。
				writeMemoryFile(t, filepath.Join(f.getMemoryDirectory(), "user", "MEMORY.md"), "- 回复用中文\n")
				observer := f.openMemoryConversation(t, defs.ExecutorSettings{WorkspaceDirectories: map[string]string{defs.LocalExecutorID: newE2EWorkspace(t)}})
				first, _ := f.runTurn(t, observer, "第一轮", e2eModelReply{text: "好"})
				second, _ := f.runTurn(t, observer, "第二轮", e2eModelReply{text: "好"})
				if len(getMemoryMessages(first.body)) != 1 || len(getMemoryMessages(second.body)) != 1 {
					t.Fatalf("memory messages: first %+v, second %+v; want one each", getMemoryMessages(first.body), getMemoryMessages(second.body))
				}
				// 2. 手动压缩以后，下一轮请求里记忆目录又出现一次，排在摘要之后。
				f.compact(t, observer, "对话摘要")
				third, _ := f.runTurn(t, observer, "第三轮", e2eModelReply{text: "好"})
				messages := getModelMessageTexts(third.body)
				summaryAt := slices.IndexFunc(messages, func(message modelMessageText) bool {
					return strings.Contains(message.text, "<openvaala-context-summary>")
				})
				memoryAt := slices.IndexFunc(messages, func(message modelMessageText) bool {
					return message.role == "user" && strings.Contains(message.text, memoryIndexMarker)
				})
				if summaryAt < 0 || memoryAt < summaryAt || len(getMemoryMessages(third.body)) != 1 {
					t.Errorf("third request messages = %+v, want one memory message after the summary", messages)
				}
			},
		},
		{
			name: "oversized_memory_is_cut_at_a_line",
			run: func(t *testing.T, f *conversationManagementFixture) {
				// 1. 用户级 MEMORY.md 有 10 KiB 以上，由很多整行组成。
				writeMemoryFile(t, filepath.Join(f.getMemoryDirectory(), "user", "MEMORY.md"), longIndex)
				if len(longIndex) <= 10<<10 {
					t.Fatalf("test file has %d bytes, want more than 10 KiB", len(longIndex))
				}
				observer := f.openMemoryConversation(t, defs.ExecutorSettings{WorkspaceDirectories: map[string]string{defs.LocalExecutorID: newE2EWorkspace(t)}})
				request, _ := f.runTurn(t, observer, "你好", e2eModelReply{text: "好"})
				// 2. 正文不超过 8 KiB，结尾是文件里完整的一行，并带续读提示。
				memory := getMemoryMessages(request.body)
				if len(memory) != 1 {
					t.Fatalf("memory messages = %+v, want one", memory)
				}
				_, body, _ := strings.Cut(memory[0].text, "\n\n")
				if len(body) > 8<<10 || !strings.HasPrefix(longIndex, body+"\n") {
					t.Errorf("memory body has %d bytes ending %q, want at most 8 KiB ending at a whole line of the file", len(body), body[max(0, len(body)-40):])
				}
				if !strings.Contains(memory[0].text, "目录超过 8 KiB") || !strings.Contains(memory[0].text, "read_file executor=memory path=user/MEMORY.md") {
					t.Errorf("memory message = %q, want the continue-reading hint", memory[0].text[:min(len(memory[0].text), 400)])
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, newConversationManagementFixture(t, binary))
		})
	}
}
