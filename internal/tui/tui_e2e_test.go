package tui

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/runtimeserver"
	"github.com/VaalaCat/openvaala/pkg/storage"
	storagefile "github.com/VaalaCat/openvaala/pkg/storage/file"
	storagesqlite "github.com/VaalaCat/openvaala/pkg/storage/sqlite"
	"github.com/VaalaCat/openvaala/pkg/telemetry/telemetrytest"
	"github.com/VaalaCat/openvaala/pkg/utils/syncmap"
)

// TestTUIEndToEnd 检查真实服务连接上的界面输入、工具与对话管理。
func TestTUIEndToEnd(t *testing.T) {
	// 1. 逐场景配置 Runtime 和界面，再驱动输入并核对模型请求与显示。
	tests := []struct {
		name string
		// agentName 是界面新建对话所用的 Agent；为空时用配置里的 assistant。每个用例各建一套 Runtime 与界面。
		agentName string
		// startupNotice 是界面启动后在状态行显示的提示；为空时不显示。
		startupNotice string
		// configure 的最后一个参数是界面交给服务端的工作目录，它不同于 serve 的默认工作目录。
		configure func(*testing.T, *runtime.RuntimeOpt, *runtimeserver.RuntimeServerOpt, string)
		run       func(t *testing.T, h *e2eHarness)
	}{
		{
			name: "skills_steer_rules_compaction_and_reopen",
			configure: func(t *testing.T, opt *runtime.RuntimeOpt, serverOpt *runtimeserver.RuntimeServerOpt, directory string) {
				opt.Definitions.Models[0].ContextWindow = 20000
				for path, text := range map[string]string{
					".agents/skills/review/SKILL.md": "---\nname: review\ndescription: review skill\n---\nREVIEW_BODY",
					".agents/skills/tests/SKILL.md":  "---\nname: tests\ndescription: test skill\n---\nTESTS_BODY",
					"AGENTS.md":                      "ROOT_RULE",
					"nested/AGENTS.md":               "NESTED_RULE",
					"nested/a.txt":                   "a",
					"nested/b.txt":                   "b",
				} {
					path = filepath.Join(directory, path)
					if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte(text), 0600); err != nil {
						t.Fatal(err)
					}
				}
			},
			run: func(t *testing.T, h *e2eHarness) {
				// 1. 补全并提交 Skills，运行中插话，再读取两个文件检查目录规则。
				h.waitFor("skills loaded", func() bool { return len(h.model.skills) == 2 })
				h.typeText("请用 /rev")
				h.press("tab")
				h.typeText("/tes")
				h.press("enter", "enter")
				first := h.receive(t)
				for _, want := range []string{"请用 /review /tests ", "REVIEW_BODY", "TESTS_BODY", "ROOT_RULE"} {
					if !strings.Contains(first.getText(), want) {
						t.Fatalf("missing %s: %s", want, first.getText())
					}
				}
				h.waitFor("root running", func() bool { return h.model.sessionStatuses.isRootRunning() })
				root, _ := h.model.sessionStatuses.getRoot()
				firstTurn := root.ActiveTurnID
				h.typeText("补充 /rev")
				h.press("tab", "enter")
				h.waitFor("steer accepted", func() bool { return h.model.notice.text == "已插入，将在下一步读入" })
				first.reply <- e2eModelResponse{calls: []llmkit.ToolCall{
					{
						ID:        "read-a",
						Name:      "read_file",
						Arguments: `{"path":"nested/a.txt"}`,
					},
					{
						ID:        "read-b",
						Name:      "read_file",
						Arguments: `{"path":"nested/b.txt"}`,
					},
				}}
				next := h.receive(t)
				if !strings.Contains(next.getText(), "补充 /review ") || !strings.Contains(next.getText(), "NESTED_RULE") {
					t.Fatalf("next request = %s", next.getText())
				}
				toolResults := 0
				for _, raw := range next.body["messages"].([]any) {
					message := raw.(map[string]any)
					if message["role"] == "tool" {
						toolResults++
					}
					body, _ := json.Marshal(message)
					if strings.Contains(string(body), "NESTED_RULE") && (message["role"] != "user" || toolResults != 2) {
						t.Fatalf("rule before tool results: %s", body)
					}
				}
				next.reply <- e2eModelResponse{chunks: []string{"完成检查\n\n" + strings.Repeat("longhistory ", 6000)}}
				h.waitFor("finished", func() bool {
					return strings.Contains(h.getPrinted(), "完成检查") && !h.model.sessionStatuses.isRootRunning()
				})
				conversation := *h.model.conversation
				readLog := func() []dao.LoggedItem {
					page, err := h.runtime.ListItems(t.Context(), &runtime.ListItemsRequest{
						ConversationID: conversation.ID,
						SessionID:      conversation.RootSessionID,
						Order:          storage.DirectionAsc,
						Limit:          100,
					})
					if err != nil {
						t.Fatal(err)
					}
					return page.Items
				}
				skillCount, ruleCount := 0, 0
				for _, entry := range readLog() {
					if entry.Item.TurnID != firstTurn {
						t.Fatalf("split turn: %+v", entry.Item)
					}
					if entry.Item.Kind == defs.ItemSkillInstruction {
						skillCount++
					}
					if entry.Item.Kind == defs.ItemAgentsInstruction {
						ruleCount++
					}
				}
				if skillCount != 3 || ruleCount != 2 {
					t.Fatalf("skills=%d rules=%d", skillCount, ruleCount)
				}
				// 2. 写出长历史后触发压缩，再读文件检查目录规则重新加载。
				h.typeText("压缩后重读")
				h.press("enter")
				compact := h.receive(t)
				if compact.body["tool_choice"] != "none" {
					t.Fatalf("expected compaction: %v", compact.body["tool_choice"])
				}
				compact.reply <- e2eModelResponse{chunks: []string{"先前已完成检查。"}}
				afterCompact := h.receive(t)
				if strings.Count(afterCompact.getText(), "ROOT_RULE") != 1 || strings.Contains(afterCompact.getText(), "NESTED_RULE") {
					t.Fatalf("rules after compaction = %s", afterCompact.getText())
				}
				afterCompact.reply <- e2eModelResponse{calls: []llmkit.ToolCall{{
					ID:        "reread",
					Name:      "read_file",
					Arguments: `{"path":"nested/a.txt"}`,
				}}}
				reread := h.receive(t)
				if strings.Count(reread.getText(), "NESTED_RULE") != 1 {
					t.Fatalf("deep rule did not reload: %s", reread.getText())
				}
				reread.reply <- e2eModelResponse{chunks: []string{"压缩后完成"}}
				h.waitFor("compacted turn finished", func() bool {
					return strings.Contains(h.getPrinted(), "压缩后完成") && !h.model.sessionStatuses.isRootRunning()
				})
				summaries, rootRules, nestedRules := 0, 0, 0
				for _, entry := range readLog() {
					if entry.Item.Kind == defs.ItemContextSummary {
						summaries++
					}
					if rule := entry.Item.AgentsInstruction; rule != nil {
						if rule.Text == "ROOT_RULE" {
							rootRules++
						}
						if rule.Text == "NESTED_RULE" {
							nestedRules++
						}
					}
				}
				if summaries != 1 || rootRules != 2 || nestedRules != 2 {
					t.Fatalf("summary=%d root=%d nested=%d", summaries, rootRules, nestedRules)
				}
				// 3. 用 /new 回到草稿再恢复对话，检查输入历史和后续请求中的目录规则。
				h.typeText("/new")
				h.press("enter")
				h.run(h.model.resumeConversation(conversation))
				h.waitFor("restored input history", func() bool { return len(h.model.composer.history) == 3 })
				if got := strings.Join(h.model.composer.history, "|"); got != "请用 /review /tests |补充 /review |压缩后重读" {
					t.Fatalf("history=%q", got)
				}
				h.typeText("再检查")
				h.press("enter")
				reopened := h.receive(t)
				if strings.Count(reopened.getText(), "ROOT_RULE") != 1 || strings.Count(reopened.getText(), "NESTED_RULE") != 1 {
					t.Fatalf("reopened rules=%s", reopened.getText())
				}
				reopened.reply <- e2eModelResponse{chunks: []string{"再次完成"}}
				h.waitFor("reopened finished", func() bool { return strings.Contains(h.getPrinted(), "再次完成") })
			},
		},
		{
			name: "stream_tool_group_then_reply",
			run: func(t *testing.T, h *e2eHarness) {
				// 1. 发送输入，依次返回工具前文字、bash 调用和最终回复，检查输出顺序。
				h.typeText("看看目录")
				h.press("enter")
				h.receive(t).reply <- e2eModelResponse{
					chunks: []string{"我先看看。"},
					calls: []llmkit.ToolCall{{
						ID:        "call_1",
						Name:      "bash",
						Arguments: `{"command":"echo hi"}`,
					}},
				}
				h.receive(t).reply <- e2eModelResponse{chunks: []string{"完成：hi\n\n", "结论段落。"}}
				h.waitFor("reply finished", func() bool { return strings.Contains(h.getPrinted(), "结论段落。") })
				requireOrder(t, h.getPrinted(), "› 看看目录", "● 我先看看。", "✓ 工具 1 次", "└─ bash@local 1 次", "完成：hi", "结论段落。")
				h.waitFor("root idle", func() bool { return !h.model.sessionStatuses.isRootRunning() })
				if footer := h.view(); !strings.Contains(footer, "ctx ") {
					t.Fatalf("footer lacks context usage:\n%s", footer)
				}
			},
		},
		{
			name: "interrupt_keeps_partial_reply",
			run: func(t *testing.T, h *e2eHarness) {
				// 1. 暂停长回复，连按两次 Esc，检查已保存的部分回复未标成未保存。
				h.typeText("写长文")
				h.press("enter")
				release := make(chan struct{})
				defer close(release)
				h.receive(t).reply <- e2eModelResponse{
					chunks:  []string{"第一段\n\n第二段\n", "不会到达"},
					release: release,
				}
				h.waitFor("first block printed", func() bool { return strings.Contains(h.getPrinted(), "● 第一段") })
				h.waitFor("root running", func() bool { return h.model.sessionStatuses.isRootRunning() })
				h.press("esc", "esc")
				h.waitFor("partial reply finalized", func() bool { return strings.Contains(h.getPrinted(), "第二段") })
				h.waitFor("root idle", func() bool { return !h.model.sessionStatuses.isRootRunning() })
				if strings.Contains(h.getPrinted(), "已中断 · 以上内容未保存") {
					t.Fatalf("saved partial reply was marked unsaved:\n%s", h.getPrinted())
				}
			},
		},
		{
			name: "steer_and_queued_input",
			run: func(t *testing.T, h *e2eHarness) {
				// 1. 暂停首轮，分别插话和排队，再放行并检查两轮分隔线与预览。
				h.typeText("第一条")
				h.press("enter")
				release := make(chan struct{})
				h.receive(t).reply <- e2eModelResponse{
					chunks:  []string{"处理中\n", "完毕"},
					release: release,
				}
				h.waitFor("root running", func() bool { return h.model.sessionStatuses.isRootRunning() })
				root, _ := h.model.sessionStatuses.getRoot()
				firstTurnID := root.ActiveTurnID
				h.typeText("补充说明")
				h.press("enter")
				h.waitFor("steer notice", func() bool { return h.model.notice.text == "已插入，将在下一步读入" })
				h.typeText("排队的")
				h.press("tab")
				h.waitFor("queued preview", func() bool { return strings.Contains(h.view(), "排队：排队的") })
				close(release)
				// 2. 暂停排队输入的回复，在运行期间检查它的排队预览已移除。
				queuedRelease := make(chan struct{})
				sawSteer, sawQueued := false, false
				h.waitForWithModel("queued turn running without preview", func() bool {
					root, _ := h.model.sessionStatuses.getRoot()
					return sawSteer && sawQueued && isSessionRunning(root) && root.ActiveTurnID != firstTurnID && !strings.Contains(h.view(), "排队：")
				}, func(request e2eModelRequest) e2eModelResponse {
					text := request.getText()
					sawSteer = sawSteer || strings.Contains(text, "补充说明")
					if strings.Contains(text, "排队的") {
						sawQueued = true
						return e2eModelResponse{
							chunks:  []string{"排队", "已处理"},
							release: queuedRelease,
						}
					}
					return e2eModelResponse{chunks: []string{"好"}}
				})
				close(queuedRelease)
				h.waitFor("queued turn finished", func() bool {
					root, found := h.model.sessionStatuses.getRoot()
					return found && !isSessionRunning(root) && root.LastTurnID != firstTurnID && strings.Contains(h.getPrinted(), "● 排队已处理")
				})
				// 3. 核对插话前没有分隔线，排队输入开启第二轮时恰有一条分隔线。
				divider := "\n" + strings.Repeat("─", h.model.width) + "\n"
				printed := h.getPrinted()
				requireOrder(t, printed, "› 补充说明", divider, "› 排队的", "● 排队已处理")
				if count := strings.Count(printed, divider); count != 1 {
					t.Fatalf("divider count = %d, want 1 in:\n%s", count, printed)
				}
			},
		},
		{
			// 排队两条输入：两个 Turn 之间服务端会推送一次没有活动 Turn 的运行信息，第一条排队的 Turn 运行期间第二条仍在预览里；
			// 第二条保存（开始执行）后预览移除。
			name: "two_queued_inputs_keep_preview_between_turns",
			run: func(t *testing.T, h *e2eHarness) {
				// 1. 首轮暂停时排入两条输入，逐轮放行并检查剩余预览。
				h.typeText("第一条")
				h.press("enter")
				firstRelease := make(chan struct{})
				h.receive(t).reply <- e2eModelResponse{
					chunks:  []string{"处理中\n", "完毕"},
					release: firstRelease,
				}
				h.waitFor("root running", func() bool { return h.model.sessionStatuses.isRootRunning() })
				root, _ := h.model.sessionStatuses.getRoot()
				firstTurnID := root.ActiveTurnID
				for _, text := range []string{"排队一", "排队二"} {
					h.typeText(text)
					h.press("tab")
					h.waitFor("queued "+text, func() bool { return strings.Contains(h.view(), "排队："+text) })
				}
				// 第一条排队的 Turn 写出第一段后停住；之后的 Turn 直接回复。
				secondRelease := make(chan struct{})
				replies := 0
				reply := func(e2eModelRequest) e2eModelResponse {
					replies++
					if replies == 1 {
						return e2eModelResponse{
							chunks:  []string{"第二轮", "完成"},
							release: secondRelease,
						}
					}
					return e2eModelResponse{chunks: []string{"第三轮"}}
				}
				close(firstRelease)
				// 新 Turn 的运行信息到达时，上一个 Turn 结束时的空闲推送已经处理过。
				h.waitForWithModel("first queued turn running", func() bool {
					root, _ := h.model.sessionStatuses.getRoot()
					return isSessionRunning(root) && root.ActiveTurnID != firstTurnID && strings.Contains(h.getPrinted(), "› 排队一")
				}, reply)
				if view := h.view(); !strings.Contains(view, "排队：排队二") || strings.Contains(view, "排队：排队一") {
					t.Fatalf("queue preview while the first queued turn runs:\n%s", view)
				}
				close(secondRelease)
				h.waitForWithModel("second queued input saved", func() bool { return strings.Contains(h.getPrinted(), "› 排队二") }, reply)
				if view := h.view(); strings.Contains(view, "排队：") {
					t.Fatalf("queue preview after the last queued input was saved:\n%s", view)
				}
				h.waitForWithModel("last queued turn finished", func() bool {
					return strings.Contains(h.getPrinted(), "● 第三轮") && !h.model.sessionStatuses.isRootRunning()
				}, reply)
				requireOrder(t, h.getPrinted(), "› 排队一", "● 第二轮完成", "› 排队二", "● 第三轮")
			},
		},
		{
			name: "switch_model_applies_to_next_turn",
			run: func(t *testing.T, h *e2eHarness) {
				// 1. 完成首轮后切换模型，检查页脚和下一轮请求使用新模型。
				h.typeText("开场")
				h.press("enter")
				if model := h.receive(t); model.body["model"] != "test" {
					t.Fatalf("first turn model = %v", model.body["model"])
				} else {
					model.reply <- e2eModelResponse{chunks: []string{"好"}}
				}
				h.waitFor("root idle", func() bool {
					root, found := h.model.sessionStatuses.getRoot()
					return found && !isSessionRunning(root) && root.LastTurnID != ""
				})
				h.typeText("/model")
				h.press("enter")
				h.waitFor("models listed", func() bool { return strings.Contains(h.view(), "Second") })
				h.press("down", "enter")
				h.waitFor("model switched", func() bool { return h.model.notice.text == "已切换到 Second" })
				h.waitFor("footer updated", func() bool { return strings.Contains(h.view(), "Second · ctx") })
				h.typeText("再来")
				h.press("enter")
				request := h.receive(t)
				if request.body["model"] != "second" {
					t.Fatalf("next turn model = %v", request.body["model"])
				}
				request.reply <- e2eModelResponse{chunks: []string{"换好了"}}
				h.waitFor("model change printed", func() bool { return strings.Contains(h.getPrinted(), "已切换到 Second") })
			},
		},
		{
			name: "reconnect_flushes_group_and_keeps_partial_reply",
			run: func(t *testing.T, h *e2eHarness) {
				// 1. 工具运行中断线并中断根 Turn，检查重连同步后工具组写出、运行状态被覆盖。
				h.typeText("跑一下")
				h.press("enter")
				h.receive(t).reply <- e2eModelResponse{calls: []llmkit.ToolCall{{
					ID:        "call_1",
					Name:      "bash",
					Arguments: `{"command":"echo hi"}`,
				}}}
				h.receive(t)
				h.waitFor("open tool group", func() bool { return strings.Contains(h.view(), "└─ bash@local 1 次") })
				h.disconnectThen(func() { h.interruptRootTurn(t) })
				h.waitFor("group flushed after resync", func() bool { return strings.Contains(h.getPrinted(), "✓ 工具 1 次") })
				h.waitFor("status overridden", func() bool {
					return !h.model.sessionStatuses.isRootRunning() && !strings.Contains(h.view(), "生成中")
				})

				// 2. 长回复写出首段后再断线并中断，检查重连后已保存的部分回复只写一次、没有标成未保存。
				h.typeText("写长文")
				h.press("enter")
				release := make(chan struct{})
				defer close(release)
				h.receive(t).reply <- e2eModelResponse{
					chunks:  []string{"第一段\n\n第二段\n", "不会到达"},
					release: release,
				}
				h.waitFor("first block printed", func() bool { return strings.Contains(h.getPrinted(), "● 第一段") })
				h.disconnectThen(func() { h.interruptRootTurn(t) })
				h.waitFor("partial reply finalized after resync", func() bool { return strings.Contains(h.getPrinted(), "第二段") })
				h.waitFor("root idle after resync", func() bool { return !h.model.sessionStatuses.isRootRunning() })
				if count := strings.Count(h.getPrinted(), "● 第一段"); count != 1 {
					t.Fatalf("first block printed %d times", count)
				}
				if strings.Contains(h.getPrinted(), "已中断 · 以上内容未保存") {
					t.Fatalf("saved partial reply was marked unsaved:\n%s", h.getPrinted())
				}
			},
		},
		{
			name: "panels_fetch_real_data",
			run: func(t *testing.T, h *e2eHarness) {
				// 1. 完成首轮，再分别打开上下文、状态和模型面板核对真实数据。
				h.typeText("开场")
				h.press("enter")
				h.receive(t).reply <- e2eModelResponse{chunks: []string{"好"}}
				h.waitFor("root idle", func() bool {
					root, found := h.model.sessionStatuses.getRoot()
					return found && root.LastTurnID != "" && !isSessionRunning(root)
				})
				for _, step := range []struct{ command, want string }{
					{
						"/context",
						"下一次请求",
					},
					{
						"/status",
						"主模型    Test  test/test",
					},
					{
						"/model",
						"Second",
					},
				} {
					h.typeText(step.command)
					h.press("enter")
					h.waitFor(step.command, func() bool { return strings.Contains(h.view(), step.want) })
					h.press("esc")
				}
			},
		},
		{
			name: "new_conversation_uses_workspace_directory",
			run: func(t *testing.T, h *e2eHarness) {
				// 1. 发送首条输入，读取服务端对话并核对 local 目录。
				h.typeText("你好")
				h.press("enter")
				h.receive(t).reply <- e2eModelResponse{chunks: []string{"好"}}
				h.waitFor("conversation opened", func() bool { return h.model.conversation != nil && !h.model.isOpeningDraft })
				record, err := h.runtime.GetConversation(t.Context(), &runtime.GetConversationRequest{ConversationID: h.model.conversation.ID})
				if err != nil {
					t.Fatal(err)
				}
				if got := record.ExecutorSettings.WorkspaceDirectories[defs.LocalExecutorID]; got != h.workspaceDirectory {
					t.Fatalf("conversation local directory = %q, want the TUI directory %q", got, h.workspaceDirectory)
				}
			},
		},
		{
			// serve 的 server_id 与界面的 LocalServerID 不同时按远端处理：新建对话不带界面的目录，落在 serve 的工作目录。
			name: "new_conversation_on_remote_server_uses_serve_directory",
			configure: func(_ *testing.T, _ *runtime.RuntimeOpt, serverOpt *runtimeserver.RuntimeServerOpt, _ string) {
				serverOpt.ServerID = "remote-server"
			},
			run: func(t *testing.T, h *e2eHarness) {
				// 1. 连接 server_id 不同的服务并创建对话，检查目录取自 serve。
				h.typeText("你好")
				h.press("enter")
				h.receive(t).reply <- e2eModelResponse{chunks: []string{"好"}}
				h.waitFor("conversation opened", func() bool { return h.model.conversation != nil && !h.model.isOpeningDraft })
				record, err := h.runtime.GetConversation(t.Context(), &runtime.GetConversationRequest{ConversationID: h.model.conversation.ID})
				if err != nil {
					t.Fatal(err)
				}
				if got := record.ExecutorSettings.WorkspaceDirectories[defs.LocalExecutorID]; got == "" || got == h.workspaceDirectory {
					t.Fatalf("conversation local directory = %q, want the serve directory instead of the TUI directory %q", got, h.workspaceDirectory)
				}
			},
		},
		{
			// /cd 到子目录：serve 让对话原地换上新设置，下一轮的命令在子目录里执行。
			name: "cd_changes_directory_of_next_turn",
			run: func(t *testing.T, h *e2eHarness) {
				// 1. 先检查 pwd，再执行 /cd sub，检查提示和下一轮的工作目录。
				subdirectory := filepath.Join(h.workspaceDirectory, "sub")
				if err := os.Mkdir(subdirectory, 0o755); err != nil {
					t.Fatal(err)
				}
				if result := h.runBashTurn(t, "pwd"); !strings.Contains(result, h.workspaceDirectory) || strings.Contains(result, subdirectory) {
					t.Fatalf("pwd before /cd = %s, want %s", result, h.workspaceDirectory)
				}
				h.typeText("/cd sub")
				h.press("enter")
				h.waitFor("directory changed", func() bool { return strings.HasPrefix(h.model.notice.text, "工作位置") })
				if want := "工作位置：" + formatWorkLocation(defs.LocalExecutorID, subdirectory, true) + "，下一轮生效"; h.model.notice.text != want {
					t.Fatalf("notice = %q, want %q", h.model.notice.text, want)
				}
				if result := h.runBashTurn(t, "pwd"); !strings.Contains(result, subdirectory) {
					t.Fatalf("pwd after /cd = %s, want %s", result, subdirectory)
				}
			},
		},
		{
			// TUI 不带环境变量，本机命令使用 serve 进程自己的环境。
			name: "commands_use_serve_environment",
			run: func(t *testing.T, h *e2eHarness) {
				// 1. 设置服务端环境变量并执行命令，检查工具结果包含这个值。
				t.Setenv("TOOL_ENV_MARK", "serve")
				if result := h.runBashTurn(t, `echo $TOOL_ENV_MARK`); !strings.Contains(result, "serve") {
					t.Fatalf("bash result = %s, want the serve environment", result)
				}
			},
		},
		{
			// 回退：第一轮模型输出之后插进的补充消息不算这一轮的开头；第二轮运行中两下 Esc 仍然中断，没读到的两条补充一起开第三轮。
			// 回退到第三轮之前，输入框是那两条原文用空行连起来；再从分叉出的对话回退到第一轮之前，得到一个没有历史的新对话。
			name: "rewind_restores_opening_batch",
			run: func(t *testing.T, h *e2eHarness) {
				// 1. 完成含插话的首轮；第二轮运行中插入两条补充后连按两下 Esc 中断，等两条补充一起开的第三轮结束。
				reply := func(e2eModelRequest) e2eModelResponse { return e2eModelResponse{chunks: []string{"好"}} }
				h.typeText("第一条")
				h.press("enter")
				firstRelease := make(chan struct{})
				h.receive(t).reply <- e2eModelResponse{
					chunks:  []string{"处理中\n", "完毕"},
					release: firstRelease,
				}
				h.waitFor("first turn running", func() bool { return h.model.sessionStatuses.isRootRunning() })
				h.typeText("补充说明")
				h.press("enter")
				h.waitFor("steer accepted", func() bool { return h.model.notice.text == "已插入，将在下一步读入" })
				close(firstRelease)
				h.waitForWithModel("first turn finished", func() bool {
					return strings.Contains(h.getPrinted(), "› 补充说明") && !h.model.sessionStatuses.isRootRunning()
				}, reply)

				h.typeText("第二条")
				h.press("enter")
				secondRelease := make(chan struct{})
				defer close(secondRelease)
				h.receive(t).reply <- e2eModelResponse{
					chunks:  []string{"写到一半\n\n", "不会到达"},
					release: secondRelease,
				}
				h.waitFor("second turn running", func() bool { return h.model.sessionStatuses.isRootRunning() })
				for _, text := range []string{"补充一", "补充二"} {
					h.typeText(text)
					h.press("enter")
					h.waitFor("steer "+text, func() bool { return h.model.notice.text == "已插入，将在下一步读入" })
				}
				h.press("esc", "esc")
				if h.model.activePanel != nil {
					t.Fatal("esc esc while running opened a panel")
				}
				h.waitForWithModel("interrupted and batch turn finished", func() bool {
					printed := h.getPrinted()
					return strings.Contains(printed, "已中断") && strings.Contains(printed, "› 补充二") && !h.model.sessionStatuses.isRootRunning()
				}, reply)

				// 2. 两下 Esc 打开回退面板检查列出的轮次，回退到第三轮之前，检查分叉出的对话、输入框原文和提示。
				h.press("esc", "esc")
				view := h.view()
				requireOrder(t, view, "回退重来", "› 补充一", "第二条", "第一条")
				if strings.Contains(view, "补充说明") || strings.Contains(view, "补充二") {
					t.Fatalf("rewind panel lists messages that did not open a turn:\n%s", view)
				}
				source := h.model.conversation.ID
				h.press("enter")
				h.waitFor("rewound before the batch turn", func() bool {
					return h.model.conversation.ID != source && len(h.model.transcript.getTurnOpeningInputs()) == 2
				})
				if h.model.composer.getValue() != "补充一\n\n补充二" || h.model.notice.text != "已在这条消息之前分叉，原对话保留" {
					t.Fatalf("composer = %q, notice = %q", h.model.composer.getValue(), h.model.notice.text)
				}
				forked, err := h.runtime.GetConversation(t.Context(), &runtime.GetConversationRequest{ConversationID: h.model.conversation.ID})
				if err != nil || forked.ForkedFromID != source {
					t.Fatalf("forked = %+v, err = %v", forked, err)
				}

				// 3. 清空输入框，从分叉出的对话回退到第一轮之前，检查得到没有历史的新对话。
				h.press("ctrl+c")
				h.press("esc", "esc")
				requireOrder(t, h.view(), "› 第二条", "第一条")
				h.press("down", "enter")
				h.waitFor("rewound before the first turn", func() bool {
					_, synced := h.model.sessionStatuses.getRoot()
					return h.model.conversation.ID != forked.ID && synced
				})
				if h.model.composer.getValue() != "第一条" {
					t.Fatalf("composer = %q", h.model.composer.getValue())
				}
				h.press("enter")
				request := h.receive(t)
				if text := request.getText(); !strings.Contains(text, "第一条") || strings.Contains(text, "完毕") || strings.Contains(text, "第二条") {
					t.Fatalf("request after rewinding to the first turn = %s", text)
				}
				request.reply <- e2eModelResponse{chunks: []string{"重新开始"}}
				h.waitFor("fresh turn finished", func() bool { return strings.Contains(h.getPrinted(), "● 重新开始") })
			},
		},
		{
			// /fork 复制全部轮次：标题不变，新对话带着历史继续；/resume 里分叉出的对话标「分叉」，原对话保留。
			name: "fork_keeps_title_and_history",
			run: func(t *testing.T, h *e2eHarness) {
				// 1. 完成首轮后执行 /fork，继续输入并检查复制的标题、历史和列表标记。
				h.typeText("开场")
				h.press("enter")
				h.receive(t).reply <- e2eModelResponse{chunks: []string{"好"}}
				h.waitFor("first turn finished", func() bool {
					root, found := h.model.sessionStatuses.getRoot()
					return found && root.LastTurnID != "" && !isSessionRunning(root) && h.model.conversation.Title == "开场"
				})
				source := h.model.conversation.ID
				h.typeText("/fork")
				h.press("enter")
				h.waitFor("forked", func() bool { return h.model.conversation.ID != source })
				if h.model.conversation.Title != "开场" || h.model.notice.text != "已分叉到新对话，原对话保留" || h.model.composer.getValue() != "" {
					t.Fatalf("title = %q, notice = %q, composer = %q", h.model.conversation.Title, h.model.notice.text, h.model.composer.getValue())
				}
				forkedID := h.model.conversation.ID
				h.typeText("继续")
				h.press("enter")
				request := h.receive(t)
				if text := request.getText(); !strings.Contains(text, "开场") || !strings.Contains(text, "继续") {
					t.Fatalf("forked request = %s", text)
				}
				request.reply <- e2eModelResponse{chunks: []string{"接着来"}}
				h.waitFor("forked turn finished", func() bool {
					return strings.Contains(h.getPrinted(), "● 接着来") && !h.model.sessionStatuses.isRootRunning()
				})
				h.typeText("/resume")
				h.press("enter")
				h.waitFor("listed", func() bool { return strings.Count(h.view(), "开场") == 2 })
				for _, line := range strings.Split(h.view(), "\n") {
					isForkedRow, isSourceRow := strings.Contains(line, lastRunes(string(forkedID), 8)), strings.Contains(line, lastRunes(string(source), 8))
					if (isForkedRow && !strings.Contains(line, "开场 分叉")) || (isSourceRow && strings.Contains(line, "分叉")) {
						t.Fatalf("fork mark on the wrong row %q:\n%s", line, h.view())
					}
				}
			},
		},
		{
			// /resume 里 Ctrl+A 归档后从「最近」消失、在「已归档」出现；Ctrl+D 两下删除；当前对话不能删除。
			name: "resume_archives_and_deletes",
			run: func(t *testing.T, h *e2eHarness) {
				// 1. 创建两个对话，在恢复面板归档和删除旧对话，再检查当前对话不能删除。
				var ids []defs.ConversationID
				for _, text := range []string{"甲对话", "乙对话"} {
					if h.model.conversation != nil {
						h.typeText("/new")
						h.press("enter")
					}
					h.typeText(text)
					h.press("enter")
					h.receive(t).reply <- e2eModelResponse{chunks: []string{"好"}}
					h.waitFor(text+" finished", func() bool {
						root, found := h.model.sessionStatuses.getRoot()
						return found && root.LastTurnID != "" && !isSessionRunning(root) && h.model.conversation.Title == text
					})
					ids = append(ids, h.model.conversation.ID)
				}
				h.typeText("/resume")
				h.press("enter")
				h.waitFor("recent listed", func() bool { return strings.Contains(h.view(), "甲对话") && strings.Contains(h.view(), "乙对话") })
				requireOrder(t, h.view(), "乙对话", "甲对话")
				h.press("down", "ctrl+a")
				h.waitFor("archived out of recent", func() bool {
					return !strings.Contains(h.view(), "甲对话") && strings.Contains(h.view(), "乙对话")
				})
				h.press("tab")
				h.waitFor("archived listed", func() bool {
					return strings.Contains(h.view(), "甲对话") && !strings.Contains(h.view(), "乙对话")
				})
				h.press("ctrl+d")
				if h.model.notice.text != "再按 Ctrl+D 删除" {
					t.Fatalf("notice = %q", h.model.notice.text)
				}
				h.press("ctrl+d")
				h.waitFor("deleted", func() bool { return strings.Contains(h.view(), "没有对话") })
				if _, err := h.runtime.GetConversation(t.Context(), &runtime.GetConversationRequest{ConversationID: ids[0]}); !errors.Is(err, runtime.ErrConversationNotFound) {
					t.Fatalf("deleted conversation read err = %v", err)
				}
				h.press("tab")
				h.waitFor("recent listed again", func() bool { return strings.Contains(h.view(), "乙对话") })
				h.press("ctrl+d", "ctrl+d")
				if h.model.notice.text != "当前对话不能删除，先 /new 或切到别的对话" || !strings.Contains(h.view(), "乙对话") {
					t.Fatalf("notice = %q, view:\n%s", h.model.notice.text, h.view())
				}
				if _, err := h.runtime.GetConversation(t.Context(), &runtime.GetConversationRequest{ConversationID: ids[1]}); err != nil {
					t.Fatalf("current conversation read err = %v", err)
				}
			},
		},
		{
			name:          "startup_notice_shown",
			startupNotice: "runtime server was started from a different openvaala executable",
			run: func(t *testing.T, h *e2eHarness) {
				// 1. 等待启动提示并核对它不是错误状态。
				h.waitFor("startup notice", func() bool {
					return strings.Contains(h.view(), "runtime server was started from a different openvaala executable")
				})
				if h.model.notice.isError {
					t.Fatalf("startup notice shown as an error: %+v", h.model.notice)
				}
			},
		},
		{
			// Agent 名无效时创建失败，草稿恢复且不发送模型请求。
			name:      "create_failure_restores_draft",
			agentName: "nobody",
			run: func(t *testing.T, h *e2eHarness) {
				// 1. 用不存在的 Agent 提交输入，检查错误、草稿恢复和没有模型请求。
				h.typeText("你好")
				h.press("enter")
				h.waitFor("draft restored", func() bool { return !h.model.isOpeningDraft && h.model.notice.isError })
				if !strings.Contains(h.model.notice.text, "新建对话失败") || !strings.Contains(h.model.notice.text, `"nobody" is not configured`) {
					t.Fatalf("notice = %q", h.model.notice.text)
				}
				if h.model.composer.getValue() != "你好" || h.model.conversation != nil {
					t.Fatalf("composer = %q, conversation = %v", h.model.composer.getValue(), h.model.conversation)
				}
				h.llm.requireNoRequest(t)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, newE2EHarness(t, cmp.Or(tt.agentName, "assistant"), tt.startupNotice, tt.configure))
		})
	}
}

// requireOrder 检查指定文字在文本中的先后顺序。
func requireOrder(t *testing.T, text string, wants ...string) {
	t.Helper()
	// 1. 从上一个匹配位置之后查找下一段文字，缺失时报告失败。
	position := 0
	for _, want := range wants {
		index := strings.Index(text[position:], want)
		if index < 0 {
			t.Fatalf("%q not found after position %d in:\n%s", want, position, text)
		}
		position += index + len(want)
	}
}

// ── 端到端夹具 ──

// e2eHarness 运行真实的 Runtime、RuntimeServer 与 Client（经可切断的 TCP 代理），由 tuiHarness 驱动界面。
type e2eHarness struct {
	*tuiHarness
	llm     *e2eModel
	proxy   *e2eProxy
	runtime runtime.Runtime
	// workspaceDirectory 是界面交给服务端的工作目录，不同于 serve 的默认工作目录。
	workspaceDirectory string
	// bashTurnCount 是 runBashTurn 已发起的轮数，每轮的工具调用 ID 按它编号，同一对话里不重复。
	bashTurnCount int
}

// newE2EHarness 启动端到端测试的 Runtime、服务、代理和界面。
func newE2EHarness(t *testing.T, agentName string, startupNotice string, configure func(*testing.T, *runtime.RuntimeOpt, *runtimeserver.RuntimeServerOpt, string)) *e2eHarness {
	t.Helper()
	// 1. 创建内存日志管道（日志不打到 stderr）、临时数据库、日志目录和受控模型。
	pipeline := telemetrytest.NewPipeline(t)
	root := t.TempDir()
	database, err := storagesqlite.OpenDatabase(storagesqlite.NewDatabaseOpt(filepath.Join(root, "records.db"), dao.RecordNamespaces()...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	logs, err := storagefile.NewLogStorage(storagefile.NewLogStorageOpt(filepath.Join(root, "logs")))
	if err != nil {
		t.Fatal(err)
	}
	model := newE2EModel(t)
	// 2. 使用临时 HOME，分别创建界面和 serve 的工作目录。
	t.Setenv("HOME", t.TempDir())
	workspaceDirectory, serveDirectory := filepath.Join(root, "workspace"), filepath.Join(root, "serve")
	for _, directory := range []string{workspaceDirectory, serveDirectory} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	opt := runtime.NewRuntimeOpt(database.Record(), logs, &runtime.Definitions{
		Agents: []*agent.AgentDefinition{{
			Name:         "assistant",
			Description:  "主 Agent",
			Instructions: "Test assistant",
			Model: defs.ModelRef{
				ProviderID: "test",
				ModelID:    "test",
			},
		}},
		Models: []*defs.ModelDefinition{
			{
				Ref: defs.ModelRef{
					ProviderID: "test",
					ModelID:    "test",
				},
				DisplayName:   "Test",
				ContextWindow: 128000,
				Target: &llmkit.Target{
					Protocol: llmkit.ProtocolOpenAIChat,
					BaseURL:  model.server.URL,
					Model:    "test",
				},
			},
			{
				Ref: defs.ModelRef{
					ProviderID: "test",
					ModelID:    "second",
				},
				DisplayName:   "Second",
				ContextWindow: 64000,
				Target: &llmkit.Target{
					Protocol: llmkit.ProtocolOpenAIChat,
					BaseURL:  model.server.URL,
					Model:    "second",
				},
			},
		},
		// local 带 read_file、apply_patch 和命令工具，界面没给目录时在 serve 的工作目录。
		Executors: executor.NewConfigExecutorProvider([]executor.Executor{executor.NewLocalExecutor(&defs.ExecutorDefinition{
			ID:                 defs.LocalExecutorID,
			Kind:               defs.ExecutorKindLocal,
			Description:        "本机",
			WorkspaceDirectory: serveDirectory,
		})}),
	})
	// 3. 配置相同 server_id，并应用本场景的模型与服务设置。
	serverOpt := &runtimeserver.RuntimeServerOpt{
		Configuration: e2eConfiguration{},
		ModelCatalog:  e2eModelCatalog{},
		ServerID:      tuiTestLocalServerID,
	}
	opt.Telemetry = pipeline.Telemetry
	if configure != nil {
		configure(t, opt, serverOpt, workspaceDirectory)
	}
	rt, err := runtime.NewRuntime(opt)
	if err != nil {
		t.Fatal(err)
	}
	// 4. 启动 Runtime 并登记停止；最终清理按 Client、Server、Runtime 的顺序等待退出。
	runtimeCtx, stopRuntime := context.WithCancel(context.Background())
	runtimeDone := make(chan error, 1)
	go func() { runtimeDone <- rt.Run(runtimeCtx) }()
	t.Cleanup(func() { stopRuntime(); <-runtimeDone })
	// 5. 启动 Runtime Server，登记取消并等待退出。
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serverOpt.Runtime, serverOpt.Listener, serverOpt.Token, serverOpt.Telemetry = rt, listener, "e2e-token", pipeline.Telemetry
	server, err := runtimeserver.NewRuntimeServer(serverOpt)
	if err != nil {
		t.Fatal(err)
	}
	serverCtx, stopServer := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Run(serverCtx) }()
	t.Cleanup(func() { stopServer(); <-serverDone })
	// 6. 通过可切断代理启动客户端，再创建界面并等待连接成功。
	proxy := newE2EProxy(t, listener.Addr().String())
	clientOpt := runtimeclient.NewClientOpt("ws://"+proxy.address+"/api/v1/openvaala", "e2e-token")
	clientOpt.Telemetry = pipeline.Telemetry
	client, err := runtimeclient.NewClient(clientOpt)
	if err != nil {
		t.Fatal(err)
	}
	clientCtx, stopClient := context.WithCancel(context.Background())
	clientDone := make(chan error, 1)
	go func() { clientDone <- client.Run(clientCtx) }()
	t.Cleanup(func() { stopClient(); <-clientDone })
	setStartupNotice := func(opt *TUIOpt) { opt.StartupNotice = startupNotice }
	h := &e2eHarness{
		tuiHarness:         newTUIHarnessWithClient(t, client, agentName, workspaceDirectory, 100, 40, setStartupNotice),
		llm:                model,
		proxy:              proxy,
		runtime:            rt,
		workspaceDirectory: workspaceDirectory,
	}
	h.waitFor("connected", func() bool { return h.model.connection.State == runtimeclient.ConnectionConnected })
	return h
}

// e2eConfiguration 内嵌空配置接口；调用配置读写方法时会 panic。
type e2eConfiguration struct{ runtimeserver.Configuration }

// e2eModelCatalog 内嵌空模型目录接口；调用目录方法时会 panic。
type e2eModelCatalog struct{ runtimeserver.ModelCatalog }

// runBashTurn 通过界面执行一轮 bash 并返回工具结果。
func (h *e2eHarness) runBashTurn(t *testing.T, command string) string {
	t.Helper()
	// 1. 编码命令并通过界面提交输入，生成本轮唯一的调用 ID。
	arguments, err := json.Marshal(map[string]string{"command": command})
	if err != nil {
		t.Fatal(err)
	}
	h.typeText("执行命令")
	h.press("enter")
	h.bashTurnCount++
	callID := fmt.Sprintf("call_bash_%d", h.bashTurnCount)
	h.receive(t).reply <- e2eModelResponse{calls: []llmkit.ToolCall{{
		ID:        callID,
		Name:      "bash",
		Arguments: string(arguments),
	}}}
	// 2. 从第二次模型请求读取工具结果，给出最终回复并等待本轮结束。
	second := h.receive(t)
	var result string
	for _, raw := range second.body["messages"].([]any) {
		if message, _ := raw.(map[string]any); message["role"] == "tool" {
			result, _ = message["content"].(string)
		}
	}
	second.reply <- e2eModelResponse{chunks: []string{"好"}}
	h.waitFor("bash turn finished", func() bool {
		root, found := h.model.sessionStatuses.getRoot()
		return found && root.LastTurnID != "" && !isSessionRunning(root)
	})
	return result
}

// disconnectThen 断开连接，在中断期间执行动作后恢复连接。
func (h *e2eHarness) disconnectThen(duringOutage func()) {
	// 1. 暂停新连接并切断已有连接，等待界面进入重连状态。
	h.t.Helper()
	h.proxy.pause()
	h.proxy.cut()
	h.waitFor("reconnecting", func() bool { return h.model.connection.State == runtimeclient.ConnectionReconnecting })
	// 2. 执行断线期间的动作，放行重连并等待连接成功。
	duringOutage()
	h.proxy.resume()
	h.waitFor("reconnected", func() bool { return h.model.connection.State == runtimeclient.ConnectionConnected })
}

// interruptRootTurn 直接请求 Runtime 中断根 Turn。
func (h *e2eHarness) interruptRootTurn(t *testing.T) {
	t.Helper()
	// 1. 使用当前对话 ID 发起中断，出错时报告测试失败。
	if _, err := h.runtime.InterruptTurn(t.Context(), &runtime.InterruptTurnRequest{ConversationID: h.model.conversation.ID}); err != nil {
		t.Fatal(err)
	}
}

// receive 处理界面消息并等待下一个模型请求。
func (h *e2eHarness) receive(t *testing.T) e2eModelRequest {
	t.Helper()
	// 1. 在十秒内接收模型请求或继续处理界面消息，超时输出画面。
	deadline := time.After(10 * time.Second)
	for {
		select {
		case request := <-h.llm.requests:
			return request
		case msg := <-h.msgs:
			h.send(msg)
		case <-deadline:
			t.Fatalf("model request did not arrive\nview:\n%s", h.view())
			return e2eModelRequest{}
		}
	}
}

// waitForWithModel 处理界面消息和模型请求直到条件成立。
func (h *e2eHarness) waitForWithModel(what string, cond func() bool, reply func(request e2eModelRequest) e2eModelResponse) {
	// 1. 在十秒内用给定回复处理模型请求，并继续驱动界面消息。
	h.t.Helper()
	deadline := time.After(10 * time.Second)
	for !cond() {
		select {
		case request := <-h.llm.requests:
			request.reply <- reply(request)
		case msg := <-h.msgs:
			h.send(msg)
		case <-deadline:
			h.t.Fatalf("timed out waiting for %s\nview:\n%s", what, h.view())
		}
	}
}

// e2eModel 是受控的 OpenAI Chat 兼容模型服务：每个请求进入 requests，由测试决定如何回复。
type e2eModel struct {
	server   *httptest.Server
	requests chan e2eModelRequest
}

// e2eModelRequest 是模型服务收到的一次请求：body 是解码后的请求体，测试往 reply 写这次的回复。
type e2eModelRequest struct {
	body  map[string]any
	reply chan e2eModelResponse
}

// e2eModelResponse 是模型服务的一次回复。
type e2eModelResponse struct {
	// chunks 依次作为文本增量写出。
	chunks []string
	// release 非 nil 时，写出第一段后等它关闭再继续。
	release <-chan struct{}
	// calls 是文本之后写出的工具调用。
	calls []llmkit.ToolCall
}

// newE2EModel 创建由测试控制回复的流式模型服务。
func newE2EModel(t *testing.T) *e2eModel {
	t.Helper()
	// 1. 创建请求队列并启动 HTTP 服务，每次解码请求后等待测试回复或请求取消。
	model := &e2eModel{requests: make(chan e2eModelRequest, 64)}
	model.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 1. 解码请求，放入队列并等待测试回复或请求取消。
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		request := e2eModelRequest{
			body:  body,
			reply: make(chan e2eModelResponse, 1),
		}
		model.requests <- request
		var response e2eModelResponse
		select {
		case response = <-request.reply:
		case <-r.Context().Done():
			return
		}
		// 2. 依次写出文本增量，首段后按需等待放行通道。
		w.Header().Set("Content-Type", "text/event-stream")
		for index, chunk := range response.chunks {
			writeE2EDelta(w, map[string]any{"content": chunk})
			if index == 0 && response.release != nil {
				select {
				case <-response.release:
				case <-r.Context().Done():
					return
				}
			}
		}
		// 3. 写出工具调用，再写出结束原因和流结束标记。
		for index, call := range response.calls {
			writeE2EDelta(w, map[string]any{"tool_calls": []map[string]any{{
				"index": index, "id": call.ID, "type": "function",
				"function": map[string]any{"name": call.Name, "arguments": call.Arguments},
			}}})
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(model.server.Close)
	return model
}

// writeE2EDelta 编码并刷出一条模型文本或工具增量。
func writeE2EDelta(w http.ResponseWriter, delta map[string]any) {
	// 1. 将增量包装成 OpenAI Chat 数据，写入响应并立即刷新。
	encoded, _ := json.Marshal(map[string]any{"choices": []map[string]any{{"delta": delta}}})
	fmt.Fprintf(w, "data: %s\n\n", encoded)
	w.(http.Flusher).Flush()
}

// requireNoRequest 检查短时间内没有模型请求。
func (m *e2eModel) requireNoRequest(t *testing.T) {
	t.Helper()
	// 1. 等待二百毫秒；收到请求则报告失败。
	select {
	case request := <-m.requests:
		t.Fatalf("unexpected model request: %v", request.body)
	case <-time.After(200 * time.Millisecond):
	}
}

// getText 返回模型请求全部消息的 JSON 文本。
func (r e2eModelRequest) getText() string {
	// 1. 编码 messages 并转成字符串。
	encoded, _ := json.Marshal(r.body["messages"])
	return string(encoded)
}

// e2eProxy 把客户端连接转发到 Server：cut 断开全部转发连接模拟网络中断，pause 之后的新连接等 resume 放行。
type e2eProxy struct {
	address     string
	target      string
	connections syncmap.SyncMap[net.Conn, struct{}]
	// 测试夹具用 Mutex 保护 resumeCh：关闭时放行新连接，pause 换成未关闭的通道。
	mutex    sync.Mutex
	resumeCh chan struct{}
}

// newE2EProxy 启动可暂停和切断连接的 TCP 代理。
func newE2EProxy(t *testing.T, target string) *e2eProxy {
	t.Helper()
	// 1. 监听空闲端口，创建已放行的新连接通道。
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxy := &e2eProxy{
		address:  listener.Addr().String(),
		target:   target,
		resumeCh: make(chan struct{}),
	}
	close(proxy.resumeCh)
	// 2. 后台接受连接，等待放行后逐条转发。
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				<-proxy.getResumeChannel()
				proxy.forward(conn)
			}()
		}
	}()
	// 3. 清理时关闭监听器，放行等待中的连接并切断已有连接。
	t.Cleanup(func() {
		_ = listener.Close()
		proxy.resume()
		proxy.cut()
	})
	return proxy
}

// forward 双向转发一条代理连接。
func (p *e2eProxy) forward(client net.Conn) {
	// 1. 连接目标服务，失败时关闭客户端连接。
	server, err := net.Dial("tcp", p.target)
	if err != nil {
		_ = client.Close()
		return
	}
	// 2. 记录两端连接并启动两个方向的复制。
	p.connections.Store(client, struct{}{})
	p.connections.Store(server, struct{}{})
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(server, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, server); done <- struct{}{} }()
	// 3. 任一方向结束后关闭两端，从连接表移除。
	<-done
	_ = client.Close()
	_ = server.Close()
	p.connections.Delete(client)
	p.connections.Delete(server)
}

// cut 关闭代理记录的全部连接。
func (p *e2eProxy) cut() {
	// 1. 遍历连接表并逐一关闭。
	p.connections.Range(func(conn net.Conn, _ struct{}) bool {
		_ = conn.Close()
		return true
	})
}

// pause 暂停代理接受的新连接。
func (p *e2eProxy) pause() {
	// 1. 在锁内换成尚未关闭的放行通道。
	p.mutex.Lock()
	defer p.mutex.Unlock()
	p.resumeCh = make(chan struct{})
}

// resume 放行等待中的新连接。
func (p *e2eProxy) resume() {
	// 1. 在锁内关闭放行通道；已经关闭时不重复操作。
	p.mutex.Lock()
	defer p.mutex.Unlock()
	select {
	case <-p.resumeCh:
	default:
		close(p.resumeCh)
	}
}

// getResumeChannel 返回代理的新连接放行通道。
func (p *e2eProxy) getResumeChannel() <-chan struct{} {
	// 1. 在锁内读取当前放行通道。
	p.mutex.Lock()
	defer p.mutex.Unlock()
	return p.resumeCh
}
