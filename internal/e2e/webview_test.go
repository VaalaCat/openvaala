//go:build desktop_e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
)

// TestDesktopWebView 验证两种桌面同时连接一个服务、页面收发消息、重复启动和退出后的任务执行。
func TestDesktopWebView(t *testing.T) {
	// 1. 编译真实桌面；无图形会话时跳过窗口验收。
	if runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		t.Skip("run with xvfb-run or a graphical session")
	}
	binary, err := buildDesktopOnce()
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		name            string
		webArguments    []string
		nativeArguments []string
	}{
		{
			name:            "default_and_root_native",
			nativeArguments: []string{"--native"},
		},
		{
			name:            "app_and_app_native",
			webArguments:    []string{"app"},
			nativeArguments: []string{"app", "--native"},
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 2. 两种模式及其第二个实例使用相同 HOME、TMPDIR 和配置，只分开自动化 socket。
			model := newE2EModel(t)
			config := writeDesktopE2EConfig(t, model.server.URL)
			directory, err := os.MkdirTemp("", "ov-dual-e2e-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(directory) })
			t.Setenv("TMPDIR", directory)
			t.Setenv("GSETTINGS_BACKEND", "memory")
			stopServeOnCleanup(t, binary, config)
			webProcess, web, webWindow := startDesktopMode(t, binary, config, directory, "web", "page", scenario.webArguments)
			web.call(t, "idle", map[string]any{"window": webWindow, "timeout": 20000}, nil)
			web.waitPage(t, webWindow, `Boolean(document.querySelector('textarea[aria-label]'))`)
			// 新对话的页面外框不出现横向滚动范围。
			const pageFitsViewport = `Array.from(document.querySelectorAll('body, [data-slot="sidebar-inset"], [data-slot="sidebar-inset"] > div')).every(node => node.scrollWidth <= node.clientWidth)`
			web.waitPage(t, webWindow, pageFitsViewport)
			serverFile := filepath.Join(filepath.Dir(config), ".local", "state", "openvaala", "runtime-server.yaml")
			serverBefore, err := os.ReadFile(serverFile)
			if err != nil {
				t.Fatal(err)
			}
			nativeProcess, native, nativeWindow := startDesktopMode(t, binary, config, directory, "native", "content", scenario.nativeArguments)
			native.waitNode(t, nativeWindow, func(node desktopAutomationNode) bool {
				return node.Role == "popUpButton" && fmt.Sprint(node.Key) == "model" && !slices.Contains(node.States, "disabled")
			})
			serverAfter, err := os.ReadFile(serverFile)
			if err != nil || string(serverAfter) != string(serverBefore) {
				t.Fatalf("desktop modes did not reuse the same serve: %v\nbefore=%s\nafter=%s", err, serverBefore, serverAfter)
			}
			for _, duplicate := range []struct {
				name       string
				arguments  []string
				automation *desktopAutomation
				window     int
				kind       string
			}{
				{
					name:       "web",
					arguments:  scenario.webArguments,
					automation: web,
					window:     webWindow,
					kind:       "page",
				},
				{
					name:       "native",
					arguments:  scenario.nativeArguments,
					automation: native,
					window:     nativeWindow,
					kind:       "content",
				},
			} {
				t.Setenv("MYGO_AUTOMATION", filepath.Join(directory, duplicate.name+"-second.sock"))
				second, _ := startE2EProcess(t, binary, config, duplicate.arguments, false)
				waitDesktopExit(t, second)
				var windows struct {
					Windows []struct {
						ID      int    `json:"id"`
						Kind    string `json:"kind"`
						Visible bool   `json:"visible"`
					} `json:"windows"`
				}
				duplicate.automation.call(t, "windows", nil, &windows)
				if len(windows.Windows) != 1 || windows.Windows[0].ID != duplicate.window || windows.Windows[0].Kind != duplicate.kind || !windows.Windows[0].Visible {
					t.Fatalf("second %s launch changed its original window: %+v", duplicate.name, windows.Windows)
				}
			}

			// 3. 在真实 WebKit DOM 输入并点击发送，模型收到完整消息，页面显示流式结果。
			text := "webview 中文消息 " + scenario.name
			web.submitPageMessage(t, webWindow, text)
			request := model.next(t)
			payload, _ := json.Marshal(request.body)
			if !strings.Contains(string(payload), text) {
				t.Fatalf("model did not receive WebView input: %s", payload)
			}
			reply := "WebView 真实模型回复 " + scenario.name
			request.reply <- e2eModelReply{text: reply + "\n\n```text\n" + strings.Repeat("long-code-line-", 60) + "\n```"}
			encodedReply, _ := json.Marshal(reply)
			web.waitPage(t, webWindow, "document.body.innerText.includes("+string(encodedReply)+")")
			// 长代码只在输出块内部横向滚动，已有对话的页面外框保持原宽度。
			web.waitPage(t, webWindow, `(() => {
const output = document.querySelector('[data-output-scroll]');
if (!output || !['auto', 'scroll'].includes(getComputedStyle(output).overflowX) || output.scrollWidth <= output.clientWidth) return false;
output.scrollLeft = 100;
const scrolled = output.scrollLeft > 0;
output.scrollLeft = 0;
return scrolled;
})()`)
			web.waitPage(t, webWindow, pageFitsViewport)
			web.capturePage(t, webWindow, "reply")

			// 4. 第二轮停在模型请求处，从应用菜单关闭两个窗口，模型继续完成并写入原对话。
			web.waitPage(t, webWindow, `Boolean(document.querySelector('button[aria-label="Send"],button[aria-label="发送"]'))`)
			web.submitPageMessage(t, webWindow, "continue after both windows close")
			pending := model.next(t)
			client, stop := newE2ERuntimeClient(t, config)
			eventsDone := make(chan struct{})
			go func() {
				defer close(eventsDone)
				for range client.Events() {
				}
			}()
			defer func() { stop(); <-eventsDone }()
			ctx, cancel := context.WithTimeout(t.Context(), e2eTimeout)
			defer cancel()
			conversations, err := client.ListConversations(ctx, &runtimeclient.ListConversationsRequest{})
			if err != nil || len(conversations.Conversations) != 1 {
				t.Fatalf("shared conversations = %+v, %v", conversations, err)
			}
			conversation := conversations.Conversations[0].Conversation
			quitMenu := "File"
			quitLabel := "Quit"
			if runtime.GOOS == "darwin" {
				quitMenu = "OpenVaala Web"
				quitLabel += " OpenVaala Web"
			}
			_, _ = web.request("menu", map[string]any{"window": webWindow, "path": []string{quitMenu, quitLabel}})
			waitDesktopExit(t, webProcess)
			_, _ = native.request("menu", map[string]any{"window": nativeWindow, "path": desktopQuitMenuPath()})
			waitDesktopExit(t, nativeProcess)
			pending.reply <- e2eModelReply{text: "completed after both desktop windows closed"}
			deadline := time.Now().Add(e2eTimeout)
			for {
				items, err := client.ListItems(ctx, &runtimeclient.ListItemsRequest{
					ConversationID: conversation.ID,
					SessionID:      conversation.RootSessionID,
				})
				if err != nil {
					t.Fatalf("closing desktop windows stopped shared serve: %v", err)
				}
				body, _ := json.Marshal(items.Items)
				if strings.Contains(string(body), "completed after both desktop windows closed") {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("turn did not finish after window exit: %s", body)
				}
				time.Sleep(40 * time.Millisecond)
			}
		})
	}
}

// startDesktopMode 在调用方提供的共用临时目录中启动一种桌面，检查窗口种类。
func startDesktopMode(t *testing.T, binary, config, directory, name, kind string, arguments []string) (*e2eProcess, *desktopAutomation, int) {
	t.Helper()
	// 1. 只切换自动化 socket，不改变 HOME 和单实例锁目录。
	socketPath := filepath.Join(directory, name+".sock")
	t.Setenv("MYGO_AUTOMATION", socketPath)
	process, _ := startE2EProcess(t, binary, config, arguments, false)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("%s desktop stderr:\n%s", name, process.stderr)
		}
	})
	automation := connectDesktopAutomation(t, process, socketPath)
	// 2. 等待实际内容窗口或页面窗口，其他窗口数量和种类均报告失败。
	deadline := time.Now().Add(e2eTimeout)
	for {
		var result struct {
			Windows []struct {
				ID   int    `json:"id"`
				Kind string `json:"kind"`
			} `json:"windows"`
		}
		automation.call(t, "windows", nil, &result)
		if len(result.Windows) > 0 {
			if len(result.Windows) != 1 || result.Windows[0].Kind != kind {
				t.Fatalf("%s windows = %+v, want one %s window", name, result.Windows, kind)
			}
			return process, automation, result.Windows[0].ID
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s desktop window did not appear\n%s", name, process.stderr)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

// waitDesktopExit 等待已请求退出的桌面进程正常结束。
func waitDesktopExit(t *testing.T, process *e2eProcess) {
	t.Helper()
	// 1. 正常退出必须返回零，超时报告进程日志。
	select {
	case <-process.exited:
		if process.exitErr != nil {
			t.Fatalf("desktop exit: %v\n%s", process.exitErr, process.stderr)
		}
	case <-time.After(e2eTimeout):
		t.Fatalf("desktop did not exit\n%s", process.stderr)
	}
}

// waitPage 等待实际 WebView 页面中的 DOM 条件成立，超时输出页面可见文本。
func (a *desktopAutomation) waitPage(t *testing.T, window int, expression string) {
	t.Helper()
	// 1. 通过 MyGo 的页面自动化读取条件，不直接调用前端业务对象。
	deadline := time.Now().Add(e2eTimeout)
	for {
		var result struct {
			Value bool `json:"value"`
		}
		a.call(t, "eval", map[string]any{"window": window, "script": expression}, &result)
		if result.Value {
			return
		}
		if time.Now().After(deadline) {
			var body json.RawMessage
			a.call(t, "eval", map[string]any{"window": window, "script": "document.body.innerText"}, &body)
			t.Fatalf("WebView condition %s did not become true:\n%s", expression, body)
		}
		time.Sleep(40 * time.Millisecond)
	}
}

// submitPageMessage 编辑页面输入框并按下可用的发送按钮。
func (a *desktopAutomation) submitPageMessage(t *testing.T, window int, text string) {
	t.Helper()
	// 1. 使用浏览器原生输入属性和 input 事件写入 React 控件。
	encoded, _ := json.Marshal(text)
	script := `const input = document.querySelector('textarea[aria-label]');
input.focus();
Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value').set.call(input, ` + string(encoded) + `);
input.dispatchEvent(new Event('input', {bubbles: true}));`
	a.call(t, "eval", map[string]any{"window": window, "script": script}, nil)
	// 2. 等待页面允许提交后点击，后续从真实模型请求确认提交结果。
	a.waitPage(t, window, `Boolean(document.querySelector('textarea[aria-label]').closest('form').querySelector('button[type="submit"]:not(:disabled):not([aria-disabled="true"])'))`)
	a.call(t, "eval", map[string]any{"window": window, "script": `document.querySelector('textarea[aria-label]').closest('form').querySelector('button[type="submit"]:not(:disabled):not([aria-disabled="true"])').click()`}, nil)
}

// capturePage 保存真实 WebKit 截图和页面文本。
func (a *desktopAutomation) capturePage(t *testing.T, window int, name string) {
	t.Helper()
	// 1. 截图由运行中的系统 WebView 生成。
	var image struct {
		PNG []byte `json:"png"`
	}
	a.call(t, "capture", map[string]any{"window": window}, &image)
	if len(image.PNG) == 0 {
		t.Fatal("empty WebView screenshot")
	}
	directory := os.Getenv("OPENVAALA_DESKTOP_E2E_ARTIFACTS")
	if directory == "" {
		directory = t.TempDir()
	}
	directory = filepath.Join(directory, strings.ReplaceAll(t.Name(), "/", "-"))
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, name+".png"), image.PNG, 0600); err != nil {
		t.Fatal(err)
	}
	// 2. 同时记录页面文本，便于对照模型回复和截图。
	var body json.RawMessage
	a.call(t, "eval", map[string]any{"window": window, "script": "document.body.innerText"}, &body)
	if err := os.WriteFile(filepath.Join(directory, name+".json"), body, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("WebView capture: %s", filepath.Join(directory, name+".png"))
}
