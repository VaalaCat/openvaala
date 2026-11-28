//go:build desktop_e2e

package e2e

import (
	"bufio"
	"fmt"
	"image/color"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
)

// TestDesktopGUIExternalConversation 检查原生窗口中的 ACP 控件与真实服务端的回放、选项和会话操作。
func TestDesktopGUIExternalConversation(t *testing.T) {
	binary := requireDesktopDisplay(t)
	peer, err := buildFakeACPOnce()
	if err != nil {
		t.Fatal(err)
	}
	for _, scene := range []string{"models", "options", "import", "manual", "commands", "menu", "tree", "files"} {
		t.Run(scene, func(t *testing.T) {
			model := newE2EModel(t)
			config := writeDesktopE2EConfig(t, model.server.URL)
			writeDesktopWorkLocationFixtures(t, config)
			executable := filepath.Join(t.TempDir(), "codex")
			if err := os.Symlink(peer, executable); err != nil {
				t.Fatal(err)
			}
			name := "default"
			if scene == "tree" {
				name = "codex-acp"
			}
			args := "[\"-sessions\", \"30\"]"
			var promptLog string
			if scene == "files" {
				promptLog = filepath.Join(t.TempDir(), "prompts.jsonl")
				args = fmt.Sprintf("[-record, %q]", promptLog)
			}
			if scene == "manual" {
				args = "[\"-list=false\"]"
			}
			path := filepath.Join(filepath.Dir(config), "agents", name+".yaml")
			if err := os.WriteFile(path, []byte(fmt.Sprintf("name: %s\ntype: acp\nacp:\n  command: %q\n  args: %s\n", name, executable, args)), 0600); err != nil {
				t.Fatal(err)
			}
			_, a, window := startDesktopGUI(t, binary, config)
			a.waitNode(t, window, func(n desktopAutomationNode) bool {
				return n.Role == "popUpButton" && fmt.Sprint(n.Key) == "model" && !slices.Contains(n.States, "disabled")
			})
			waitValue := func(key, value string) {
				a.waitNode(t, window, func(n desktopAutomationNode) bool { return fmt.Sprint(n.Key) == key && n.Value == value })
			}
			switch scene {
			case "models":
				waitValue("model", "default/small")
				a.sendDesktopMessage(t, window, "model scope")
				a.waitLabel(t, window, "OK")
				node := a.waitNode(t, window, func(n desktopAutomationNode) bool {
					return n.Role == "popUpButton" && n.Value == "default/small" && !slices.Contains(n.States, "disabled")
				})
				a.call(t, "act", map[string]any{"window": window, "id": node.ID, "action": "press"}, nil)
				a.waitLabel(t, window, "large")
				if a.hasNodeLabel(t, window, "listItem", "local") {
					t.Fatal("external session lists native models")
				}
				a.capture(t, window, "desktop-t10-models")
			case "options":
				waitValue("model", "default/small")
				a.selectOption(t, window, "model", "large", "default/large")
				waitValue("reasoning", "high")
				a.press(t, window, "更多")
				waitValue("external-mode", "plan")
				a.capture(t, window, "desktop-t10-options")
				a.selectOption(t, window, "external-mode", "Code", "code")
				a.pressKey(t, window, "Escape")
				a.sendDesktopMessage(t, window, "options")
				a.waitLabel(t, window, "OK")
				a.press(t, window, "更多")
				waitValue("external-mode", "code")
			case "import", "manual":
				waitValue("reasoning", "low")
				a.press(t, window, "导入已有会话")
				if scene == "manual" {
					a.waitLabel(t, window, "external agent does not support session listing")
					a.setText(t, window, "external-session-id", "id-2")
					a.capture(t, window, "desktop-t10-import-manual")
				} else {
					a.press(t, window, "加载更多")
					a.setText(t, window, "external-session-search", "chat 12")
					a.press(t, window, "chat 12")
					a.waitLabel(t, window, "hello world")
					a.waitLabel(t, window, "latest")
					a.capture(t, window, "desktop-t10-import")
				}
				a.press(t, window, "导入")
				a.waitLabel(t, window, "hello world")
			case "commands", "menu":
				a.sendDesktopMessage(t, window, "ACP conversation")
				a.waitLabel(t, window, "OK")
				switch scene {
				case "commands":
					a.focusTextField(t, window, "composer-text")
					a.typeText(t, window, "/")
					a.waitLabel(t, window, "/status")
					if a.hasNodeLabel(t, window, "listItem", "/display-check") {
						t.Fatal("external menu contains a native skill")
					}
					a.capture(t, window, "desktop-t10-commands")
					a.typeText(t, window, "stat")
					a.pressKey(t, window, "Tab")
					a.waitNodeAbsent(t, window, func(n desktopAutomationNode) bool { return n.Role == "listItem" && n.Label == "/status" })
					a.pressKey(t, window, "Enter")
					// 补全会在命令后留一个空格。验证发出的正文，候选的标题没有这个空格。
					a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "text" && n.Label == "/status " })
				case "menu":
					treeRow := a.waitSettledNode(t, window, func(n desktopAutomationNode) bool { return strings.HasPrefix(n.Label, "Session ") })
					a.sendInput(t, window, map[string]any{"type": "pointerMove", "x": treeRow.Bounds.X + 20, "y": treeRow.Bounds.Y + 12})
					// ListRow 的无障碍节点包含整行，行尾操作按真实指针命中。
					a.settle(t, window)
					a.sendInput(t, window, map[string]any{"type": "click", "x": treeRow.Bounds.X + treeRow.Bounds.Width - 22, "y": treeRow.Bounds.Y + treeRow.Bounds.Height/2})
					a.waitLabel(t, window, "重载 Agent")
					a.pressKey(t, window, "Escape")
					a.rightClickNode(t, window, func(n desktopAutomationNode) bool { return fmt.Sprint(n.Key) == "title-rename" })
					a.waitLabel(t, window, "重载 Agent")
					a.capture(t, window, "desktop-t10-root-menu")
					a.clickMenuItem(t, window, "重载 Agent")
					if got := a.countRPCRequests(t, window, "session/reload"); got != 1 {
						t.Fatalf("reload RPCs = %d", got)
					}
					a.waitLabel(t, window, "OK")
				}
			case "files":
				image := filepath.Join(t.TempDir(), "chart.png")
				if err := os.WriteFile(image, newE2EPNG(t, color.RGBA{R: 35, G: 129, B: 178, A: 255}), 0600); err != nil {
					t.Fatal(err)
				}
				files := []string{image}
				paste := "Ctrl+V"
				if runtime.GOOS == "darwin" {
					paste = "Cmd+V"
				}
				for i := 0; i < 10; i++ {
					file := filepath.Join(t.TempDir(), fmt.Sprintf("report-%02d.txt", i))
					if err := os.WriteFile(file, []byte("report"), 0600); err != nil {
						t.Fatal(err)
					}
					files = append(files, file)
				}
				setDesktopFileClipboard(t, files)
				a.focusTextField(t, window, "composer-text")
				a.pressKey(t, window, paste)
				a.waitLabel(t, window, "report-09.txt")
				w := openExternalWire(t, config)
				var list runtimeprotocol.ListConversationsResult
				w.call(t, "conversation/list", map[string]any{}, &list)
				if len(list.Conversations) != 0 {
					t.Fatal("selecting files created a conversation")
				}
				last := a.waitSettledNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "listItem" && n.Label == "report-09.txt" })
				a.sendInput(t, window, map[string]any{"type": "wheel", "x": last.Bounds.X + 4, "y": last.Bounds.Y + 16, "dy": -120, "dx": 0})
				a.settle(t, window)
				a.capture(t, window, "desktop-t10-draft-files")
				a.setText(t, window, "composer-text", "附件正文")
				a.press(t, window, "发送")
				// ACP 收到正文时，新草稿的附件已上传完毕，可以读取提交后的节点树。
				waitExternalPrompt(t, promptLog, "附件正文")
				a.waitLabel(t, window, "OK")
				a.waitLabel(t, window, "report-00.txt")
				a.waitLabel(t, window, "附件正文")
				a.capture(t, window, "desktop-t10-files")
				a.press(t, window, "chart.png")
				a.waitLabel(t, window, "原始大小")
				a.press(t, window, "关闭")
				existing := filepath.Join(t.TempDir(), "existing.txt")
				if err := os.WriteFile(existing, []byte("existing"), 0600); err != nil {
					t.Fatal(err)
				}
				setDesktopFileClipboard(t, []string{existing})
				a.focusTextField(t, window, "composer-text")
				a.pressKey(t, window, paste)
				a.waitNode(t, window, func(n desktopAutomationNode) bool {
					return n.Role == "listItem" && n.Label == "existing.txt" && strings.Contains(filepath.ToSlash(n.Description), "/attachments/")
				})
			case "tree":
				a.sendDesktopMessage(t, window, "派一个后台任务")
				model.next(t).reply <- e2eModelReply{tool: "spawn_agent", arguments: `{"agent_name":"codex-acp","task_name":"review","message":"review","background":true,"labels":{"area":"desktop"}}`}
				model.next(t).reply <- e2eModelReply{text: "delegated"}
				a.press(t, window, "1 个后台 Agent")
				a.waitNode(t, window, func(n desktopAutomationNode) bool { return strings.Contains(n.Description, "area=desktop") })
				a.rightClickNode(t, window, func(n desktopAutomationNode) bool {
					return strings.HasPrefix(n.Label, "Session ") && strings.Contains(n.Description, "codex-acp")
				})
				a.waitLabel(t, window, "转为独立对话")
				a.capture(t, window, "desktop-t10-menu")
				a.clickMenuItem(t, window, "转为独立对话")
				a.waitNode(t, window, func(n desktopAutomationNode) bool { return strings.Contains(n.Description, "已转为独立对话") })
				a.capture(t, window, "desktop-t10-tree")
				old := a.waitNode(t, window, func(n desktopAutomationNode) bool { return strings.Contains(n.Description, "已转为独立对话") })
				a.clickNode(t, window, old, "left")
				a.waitConversationTitle(t, window, "review")
			}
		})
	}
}

// setDesktopFileClipboard 使用同一图形会话的独立剪贴板所有者，直至本次测试结束。
func setDesktopFileClipboard(t *testing.T, paths []string) {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "clipboard")
	if output, err := exec.Command("go", "build", "-o", binary, "./testdata/desktopclipboard").CombinedOutput(); err != nil {
		t.Fatalf("build clipboard: %v: %s", err, output)
	}
	cmd := exec.CommandContext(t.Context(), binary, paths...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	cmd.WaitDelay = 2 * time.Second
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Wait() })
	ready := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(stdout).ReadString('\n'); ready <- line }()
	select {
	case line := <-ready:
		if strings.TrimSpace(line) != "ready" {
			t.Fatal("clipboard owner failed")
		}
	case <-time.After(e2eTimeout):
		t.Fatal("clipboard owner timed out")
	}
}
