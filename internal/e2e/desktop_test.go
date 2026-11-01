//go:build desktop_e2e

package e2e

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/defs"
)

// buildDesktopOnce 编译带原生自动化入口和完整 CLI 的桌面程序，本包测试共用该产物。
var buildDesktopOnce = sync.OnceValues(func() (string, error) {
	// 1. 并行开发可显式指定已构建的真实程序；默认始终编译当前源码。
	if binary := os.Getenv("OPENVAALA_DESKTOP_E2E_BINARY"); binary != "" {
		absolute, err := filepath.Abs(binary)
		if err != nil {
			return "", err
		}
		info, err := os.Stat(absolute)
		if err != nil {
			return "", fmt.Errorf("desktop test binary: %w", err)
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("desktop test binary is not a regular file: %s", absolute)
		}
		return absolute, nil
	}
	// 2. 使用已有测试构建目录和 race 设置，自动化只进入本次测试程序。
	binary := filepath.Join(e2eBuildDirectory, "openvaala-desktop")
	arguments := []string{"build", "-tags", "desktop_e2e", "-ldflags", "-X main.version=desktop-e2e", "-o", binary}
	if isRaceEnabled() {
		arguments = append(arguments, "-race")
	}
	if output, err := exec.Command("go", append(arguments, "../../cmd/openvaala-desktop")...).CombinedOutput(); err != nil {
		return "", fmt.Errorf("build desktop: %w\n%s", err, output)
	}
	return binary, nil
})

// TestDesktopCLI 检查真实桌面程序在没有图形会话时保留完整命令树、配置查询和退出状态。
func TestDesktopCLI(t *testing.T) {
	// 1. 编译一次，并关闭显示与自动化环境；每个命令使用独立临时 HOME。
	binary, err := buildDesktopOnce()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("MYGO_AUTOMATION", "")
	cases := []struct {
		name      string
		arguments []string
		want      []string
		exitCode  int
	}{
		{"help", []string{"--help"}, []string{"app", "tui", "serve", "acp", "app-server", "codex", "web", "config", "plugin", "reindex"}, 0},
		{"version", []string{"--version"}, []string{"openvaala version "}, 0},
		{"configuration", []string{"config", "show"}, []string{"agent_name: assistant", "127.0.0.1:"}, 0},
		{"unknown_command", []string{"not-a-command"}, []string{"unknown command"}, 1},
		{"unknown_flag", []string{"--not-a-flag"}, []string{"invalid flags"}, 1},
	}
	for _, command := range []string{"app", "tui", "serve", "acp", "app-server", "codex", "web", "config", "plugin", "reindex", "completion"} {
		cases = append(cases, struct {
			name      string
			arguments []string
			want      []string
			exitCode  int
		}{command + "_help", []string{command, "--help"}, []string{"openvaala " + command}, 0})
	}
	// 2. 执行实际进程，检查输出和退出码，不启动 GUI。
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			config := writeE2EConfig(t, "http://127.0.0.1:1")
			ctx, cancel := context.WithTimeout(t.Context(), e2eTimeout)
			defer cancel()
			command := exec.CommandContext(ctx, binary, append([]string{"--config-file", config}, test.arguments...)...)
			command.Env = getE2EEnvironment(filepath.Dir(config))
			command.Dir = filepath.Dir(config)
			output, err := command.CombinedOutput()
			exitCode := 0
			if err != nil {
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatalf("execute desktop CLI: %v\n%s", err, output)
				}
				exitCode = exit.ExitCode()
			}
			if ctx.Err() != nil || exitCode != test.exitCode {
				t.Fatalf("exit = %d, timeout = %v; want %d\n%s", exitCode, ctx.Err(), test.exitCode, output)
			}
			for _, want := range test.want {
				if !strings.Contains(string(output), want) {
					t.Fatalf("output lacks %q:\n%s", want, output)
				}
			}
		})
	}
}

// TestDesktopCLIProtocols 检查桌面程序真正运行 serve、ACP 和 app-server，标准输出保持线上协议。
func TestDesktopCLIProtocols(t *testing.T) {
	// 1. 不提供显示服务，复用真实后台与协议测试夹具。
	binary, err := buildDesktopOnce()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("MYGO_AUTOMATION", "")
	for _, protocol := range []string{"serve", "acp", "app-server"} {
		t.Run(protocol, func(t *testing.T) {
			model := newE2EModel(t)
			config := writeE2EConfig(t, model.server.URL)
			// 2. serve 用真实 Go Client 查询配置，两个 stdio 协议完成初始化后正常关闭。
			switch protocol {
			case "serve":
				startE2EServe(t, binary, config)
				client, stop := newE2ERuntimeClient(t, config)
				defer stop()
				eventsDone := make(chan struct{})
				go func() {
					defer close(eventsDone)
					for range client.Events() {
					}
				}()
				ctx, cancel := context.WithTimeout(t.Context(), e2eTimeout)
				defer cancel()
				models, err := client.ListModels(ctx, "assistant")
				if err != nil || len(models.Models) != 2 {
					t.Fatalf("serve model list = %+v, %v", models, err)
				}
				stop()
				<-eventsDone
			case "acp":
				wire := startACPWire(t, binary, config)
				wire.wire.close(t)
				wire.wire.process.stop(t)
			case "app-server":
				wire := startAppServer(t, binary, config, "stdio")
				var initialized struct {
					UserAgent string `json:"userAgent"`
					CodexHome string `json:"codexHome"`
				}
				wire.call(t, "initialize", codexClientInfo, &initialized)
				if !strings.HasPrefix(initialized.UserAgent, "openvaala/") || initialized.CodexHome != filepath.Dir(config) {
					t.Fatalf("app-server initialize = %+v", initialized)
				}
				wire.notify(t, "initialized", nil)
				wire.close(t)
				wire.process.stop(t)
			}
		})
	}
}

// desktopAutomation 是测试进程到 MyGo 原生自动化 socket 的顺序 NDJSON 连接。
type desktopAutomation struct {
	connection net.Conn
	encoder    *json.Encoder
	decoder    *json.Decoder
	nextID     int
}

// desktopAutomationNode 保存原生无障碍树里的可见状态和操作目标。
type desktopAutomationNode struct {
	ID     string `json:"id"`
	Parent string `json:"parent"`
	Bounds struct {
		X      float64 `json:"x"`
		Y      float64 `json:"y"`
		Width  float64 `json:"width"`
		Height float64 `json:"height"`
	} `json:"bounds"`
	Role        string   `json:"role"`
	Key         any      `json:"key"`
	Label       string   `json:"label"`
	Value       string   `json:"value"`
	Description string   `json:"description"`
	States      []string `json:"states"`
	Actions     []string `json:"actions"`
}

// connectDesktopAutomation 等待本测试启动的桌面提供自动化连接，进程提前退出时报告 stderr。
func connectDesktopAutomation(t *testing.T, process *e2eProcess, socketPath string) *desktopAutomation {
	t.Helper()
	// 1. 在有限期限内拨号，路径必须由测试临时目录提供。
	deadline := time.Now().Add(e2eTimeout)
	for {
		connection, err := net.DialTimeout("unix", socketPath, 100*time.Millisecond)
		if err == nil {
			t.Cleanup(func() { _ = connection.Close() })
			return &desktopAutomation{connection: connection, encoder: json.NewEncoder(connection), decoder: json.NewDecoder(connection)}
		}
		if time.Now().After(deadline) {
			t.Fatalf("desktop automation not ready: %v\n%s", err, process.stderr)
		}
		select {
		case <-process.exited:
			t.Fatalf("desktop exited before automation: %v\n%s", process.exitErr, process.stderr)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// call 发送一条 MyGo 自动化请求，校验响应身份并解码公开结果。
func (a *desktopAutomation) call(t *testing.T, method string, params, result any) {
	t.Helper()
	// 1. 发送实际请求并把协议错误报告为验收失败。
	payload, err := a.request(method, params)
	if err != nil {
		t.Fatal(err)
	}
	if result != nil {
		if err := json.Unmarshal(payload, result); err != nil {
			t.Fatalf("automation %s result: %v\n%s", method, err, payload)
		}
	}
}

// request 通过顺序 NDJSON 校验请求身份，返回完整结果或普通错误，供正常断开时的清理使用。
func (a *desktopAutomation) request(method string, params any) (json.RawMessage, error) {
	// 1. 每次调用设置期限，截图等大结果不使用 Scanner 限额。
	a.nextID++
	if err := a.connection.SetDeadline(time.Now().Add(e2eTimeout)); err != nil {
		return nil, err
	}
	request := struct {
		ID     int    `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params,omitempty"`
	}{a.nextID, method, params}
	if err := a.encoder.Encode(request); err != nil {
		return nil, fmt.Errorf("automation %s write: %w", method, err)
	}
	// 2. 检查身份和框架错误，不把未完成动作当成成功。
	var response struct {
		ID     int             `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := a.decoder.Decode(&response); err != nil {
		return nil, fmt.Errorf("automation %s read: %w", method, err)
	}
	if response.ID != request.ID {
		return nil, fmt.Errorf("automation %s response id %d, want %d", method, response.ID, request.ID)
	}
	if response.Error != nil {
		return nil, fmt.Errorf("automation %s: %s: %s", method, response.Error.Code, response.Error.Message)
	}
	return response.Result, nil
}

// writeDesktopE2EConfig 在既有 E2E 配置里补齐桌面默认 Agent，不修改共用 fixture 的 assistant。
func writeDesktopE2EConfig(t *testing.T, modelURL string) string {
	t.Helper()
	// 1. 复用原配置和模型，复制 assistant 的真实模型设置，只换 Agent 名称。
	config := writeE2EConfig(t, modelURL)
	definition, err := os.ReadFile(filepath.Join(filepath.Dir(config), "agents", "assistant.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	definition = []byte(strings.Replace(string(definition), "name: assistant", "name: default", 1))
	if err := os.WriteFile(filepath.Join(filepath.Dir(config), "agents", "default.yaml"), definition, 0600); err != nil {
		t.Fatal(err)
	}
	return config
}

// TestDesktopGUIChat 从真实原生控件发送输入，验证后端模型请求、输出显示和退出后的共享服务。
func TestDesktopGUIChat(t *testing.T) {
	// 1. 只在图形测试会话执行，普通 CLI 验收不误启动窗口。
	if runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		t.Skip("run with xvfb-run or a graphical session")
	}
	binary, err := buildDesktopOnce()
	if err != nil {
		t.Fatal(err)
	}
	for _, send := range []string{"button", "enter"} {
		t.Run(send, func(t *testing.T) {
			model := newE2EModel(t)
			config := writeDesktopE2EConfig(t, model.server.URL)
			var arguments []string
			if send == "button" {
				arguments = []string{"app"}
			}
			process, automation, window := startDesktopGUI(t, binary, config, arguments...)
			automation.capture(t, window, "startup")
			automation.waitNode(t, window, func(node desktopAutomationNode) bool {
				return node.Role == "popUpButton" && fmt.Sprint(node.Key) == "model" && !slices.Contains(node.States, "disabled")
			})
			// 2. 只通过公开无障碍动作编辑输入框，等待发送控件真正启用。
			input := automation.waitNode(t, window, func(node desktopAutomationNode) bool {
				return node.Role == "textField" && fmt.Sprint(node.Key) == "composer-text"
			})
			text := "native desktop " + send + " 中文消息"
			if send == "button" {
				automation.call(t, "act", map[string]any{"window": window, "id": input.ID, "action": "setValue", "text": text}, nil)
			} else {
				automation.call(t, "act", map[string]any{"window": window, "id": input.ID, "action": "focus"}, nil)
				automation.call(t, "idle", map[string]any{"window": window, "timeout": 5000}, nil)
				automation.call(t, "input", map[string]any{"window": window, "events": []map[string]any{{"type": "text", "text": text}}}, nil)
			}
			button := automation.waitNode(t, window, func(node desktopAutomationNode) bool {
				return node.Role == "button" && (node.Label == "发送" || node.Label == "Send") && !slices.Contains(node.States, "disabled")
			})
			if send == "button" {
				automation.call(t, "act", map[string]any{"window": window, "id": button.ID, "action": "press"}, nil)
			} else {
				automation.call(t, "act", map[string]any{"window": window, "id": input.ID, "action": "focus"}, nil)
				automation.call(t, "input", map[string]any{"window": window, "events": []map[string]any{{"type": "key", "key": "Enter"}}}, nil)
			}
			request := model.next(t)
			payload, _ := json.Marshal(request.body)
			if !strings.Contains(string(payload), text) {
				t.Fatalf("model did not receive native input: %s", payload)
			}
			request.reply <- e2eModelReply{text: "原生回复成功 " + send}
			automation.waitNode(t, window, func(node desktopAutomationNode) bool {
				return strings.Contains(node.Label, "原生回复成功 "+send) || strings.Contains(node.Value, "原生回复成功 "+send)
			})
			automation.capture(t, window, "conversation")
			// 3. 查询使用同一 HOME 的真实后端；GUI 退出后服务仍能查询原对话。
			client, stop := newE2ERuntimeClient(t, config)
			defer stop()
			eventsDone := make(chan struct{})
			go func() {
				defer close(eventsDone)
				for range client.Events() {
				}
			}()
			ctx, cancel := context.WithTimeout(t.Context(), e2eTimeout)
			defer cancel()
			conversations, err := client.ListConversations(ctx, &runtimeclient.ListConversationsRequest{})
			if err != nil || len(conversations.Conversations) != 1 {
				t.Fatalf("conversations = %+v, %v", conversations, err)
			}
			process.stop(t)
			after, err := client.ListConversations(ctx, &runtimeclient.ListConversationsRequest{})
			if err != nil || len(after.Conversations) != 1 || after.Conversations[0].Conversation.ID != conversations.Conversations[0].Conversation.ID {
				t.Fatalf("GUI exit stopped shared backend or lost history: %+v, %v", after, err)
			}
			stop()
			<-eventsDone
		})
	}
}

// startDesktopGUI 启动独立 HOME 的原生桌面，等待唯一内容窗口和自动化连接。
func startDesktopGUI(t *testing.T, binary, config string, arguments ...string) (*e2eProcess, *desktopAutomation, int) {
	t.Helper()
	// 1. socket 使用短的私有临时目录，不超过 Unix socket 地址长度。
	directory, err := os.MkdirTemp("", "ov-desktop-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socketPath := filepath.Join(directory, "automation.sock")
	t.Setenv("TMPDIR", directory)
	t.Setenv("MYGO_AUTOMATION", socketPath)
	t.Setenv("GSETTINGS_BACKEND", "memory")
	stopServeOnCleanup(t, binary, config)
	process, _ := startE2EProcess(t, binary, config, append(slices.Clone(arguments), "--native"), false)
	automation := connectDesktopAutomation(t, process, socketPath)
	// 失败验收也通过真实确认按钮放弃本测试草稿，再由共用进程夹具检查退出码。
	t.Cleanup(func() {
		select {
		case <-process.exited:
			return
		default:
		}
		defer func() {
			select {
			case <-process.exited:
			case <-time.After(5 * time.Second):
			}
		}()
		_, err := automation.request("menu", map[string]any{"path": desktopQuitMenuPath()})
		if err != nil {
			_ = process.command.Process.Signal(syscall.SIGTERM)
		}
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case <-process.exited:
				return
			default:
			}
			raw, err := automation.request("tree", nil)
			if err != nil {
				return
			}
			var tree struct {
				Nodes []desktopAutomationNode `json:"nodes"`
			}
			if json.Unmarshal(raw, &tree) != nil {
				return
			}
			for _, node := range tree.Nodes {
				if node.Role == "button" && (node.Label == "放弃并退出" || node.Label == "Discard and quit") {
					_, _ = automation.request("act", map[string]any{"id": node.ID, "action": "press"})
					select {
					case <-process.exited:
					case <-time.After(5 * time.Second):
					}
					return
				}
			}
			time.Sleep(30 * time.Millisecond)
		}
	})
	// 2. 等待真实原生窗口；不把 WebView 或多个主窗口当成成功。
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
			if len(result.Windows) != 1 || result.Windows[0].Kind != "content" {
				t.Fatalf("native windows = %+v", result.Windows)
			}
			return process, automation, result.Windows[0].ID
		}
		if time.Now().After(deadline) {
			t.Fatal("desktop window did not appear")
		}
		time.Sleep(30 * time.Millisecond)
	}
}

// waitNode 读取真实原生树，等待指定可见控件状态出现。
func (a *desktopAutomation) waitNode(t *testing.T, window int, matches func(desktopAutomationNode) bool) desktopAutomationNode {
	t.Helper()
	// 1. 使用树里的实际状态等待，不调用业务 handler。
	deadline := time.Now().Add(e2eTimeout)
	for {
		var tree struct {
			Nodes []desktopAutomationNode `json:"nodes"`
		}
		a.call(t, "tree", map[string]any{"window": window}, &tree)
		for _, node := range tree.Nodes {
			if matches(node) {
				return node
			}
		}
		if time.Now().After(deadline) {
			payload, _ := json.MarshalIndent(tree, "", "  ")
			t.Fatalf("native node did not reach expected state:\n%s", payload)
		}
		time.Sleep(40 * time.Millisecond)
	}
}

// capture 保存真实原生截图与树；未指定验收目录时只留在测试临时目录。
func (a *desktopAutomation) capture(t *testing.T, window int, name string) {
	t.Helper()
	// 1. 等待最近输入的绘制；持续流式动画不要求完全停止。
	if _, err := a.request("idle", map[string]any{"window": window, "timeout": 250}); err != nil && !strings.Contains(err.Error(), "timeout") {
		t.Fatal(err)
	}
	var image struct {
		PNG    string `json:"png"`
		Width  int    `json:"width"`
		Height int    `json:"height"`
	}
	a.call(t, "capture", map[string]any{"window": window}, &image)
	png, err := base64.StdEncoding.DecodeString(image.PNG)
	if err != nil || len(png) == 0 || image.Width < 640 || image.Height < 480 {
		t.Fatalf("native capture %dx%d: %v", image.Width, image.Height, err)
	}
	directory := os.Getenv("OPENVAALA_DESKTOP_E2E_ARTIFACTS")
	if directory == "" {
		directory = t.TempDir()
	}
	directory = filepath.Join(directory, strings.ReplaceAll(t.Name(), "/", "-"))
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, name+".png"), png, 0600); err != nil {
		t.Fatal(err)
	}
	// 2. 同时保留无障碍树，报告截图具体位置。
	var tree json.RawMessage
	a.call(t, "tree", map[string]any{"window": window}, &tree)
	if err := os.WriteFile(filepath.Join(directory, name+".json"), tree, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("native capture: %s", filepath.Join(directory, name+".png"))
}

// TestDesktopGUIQueueAndDraft 验证排队、取回、跨对话草稿及详情观察使用同一真实后台。
func TestDesktopGUIQueueAndDraft(t *testing.T) {
	if runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		t.Skip("run with xvfb-run or a graphical session")
	}
	binary, err := buildDesktopOnce()
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"retrieve_and_switch"} {
		t.Run(scenario, func(t *testing.T) {
			model := newE2EModel(t)
			config := writeDesktopE2EConfig(t, model.server.URL)
			process, a, window := startDesktopGUI(t, binary, config)
			a.waitNode(t, window, func(n desktopAutomationNode) bool {
				return n.Role == "popUpButton" && fmt.Sprint(n.Key) == "model" && !slices.Contains(n.States, "disabled")
			})
			// 1. 第一轮停在模型处，第二条输入通过排队按钮提交。
			a.setText(t, window, "composer-text", "native queue original")
			a.press(t, window, "发送")
			first := model.next(t)
			a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "button" && n.Label == "停止" })
			a.setText(t, window, "composer-text", "native queued draft")
			a.press(t, window, "排队")
			// 排队栏默认折叠，展开后才逐条列出排队输入。
			a.press(t, window, "1 条排队")
			a.waitNode(t, window, func(n desktopAutomationNode) bool {
				return n.Role == "text" && strings.Contains(n.Label, "native queued draft")
			})
			a.press(t, window, "取回")
			a.waitNode(t, window, func(n desktopAutomationNode) bool {
				return n.Role == "textField" && fmt.Sprint(n.Key) == "composer-text" && n.Value == "native queued draft"
			})
			// 2. 新对话另留草稿，返回原对话后仍是取回的内容。
			a.press(t, window, "新对话")
			a.waitNode(t, window, func(n desktopAutomationNode) bool {
				return n.Role == "textField" && fmt.Sprint(n.Key) == "composer-text" && n.Value == ""
			})
			a.setText(t, window, "composer-text", "another target draft")
			a.press(t, window, "native queue original")
			a.waitNode(t, window, func(n desktopAutomationNode) bool {
				return n.Role == "textField" && fmt.Sprint(n.Key) == "composer-text" && n.Value == "native queued draft"
			})
			a.setText(t, window, "composer-text", "")
			first.reply <- e2eModelReply{text: "queue round finished"}
			a.waitNode(t, window, func(n desktopAutomationNode) bool { return strings.Contains(n.Label, "queue round finished") })
			a.waitNode(t, window, func(n desktopAutomationNode) bool {
				return strings.Contains(n.Description, "default · idle · completed")
			})
			var finalTree struct {
				Nodes []desktopAutomationNode `json:"nodes"`
			}
			a.call(t, "tree", map[string]any{"window": window}, &finalTree)
			for _, node := range finalTree.Nodes {
				if node.Role == "button" && node.Label == "取回" {
					t.Fatal("retrieved queue entry remained after turn completed")
				}
			}
			// 3. 全局 RPC 面板显示真实帧，切页不清空；队列取回后不触发第二个模型请求。
			a.press(t, window, "RPC")
			a.setText(t, window, "rpc-method", "conversation/submitInput")
			a.waitNode(t, window, func(n desktopAutomationNode) bool {
				return n.Role == "button" && strings.Contains(n.Label, "conversation/submitInput")
			})
			a.capture(t, window, "queue-rpc")
			select {
			case request := <-model.requests:
				t.Fatalf("retrieved input still ran: %+v", request.body)
			case <-time.After(150 * time.Millisecond):
			}
			// 4. 通过公开菜单退出，有其他目标草稿时用真实确认按钮放弃。
			_, quitErr := a.request("menu", map[string]any{"window": window, "path": desktopQuitMenuPath()})
			if quitErr != nil {
				t.Fatalf("application quit menu: %v", quitErr)
			}
			// 确认退出会在按下动作返回前关闭窗口，动作报错是预期的，以进程退出为准。
			confirm := a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "button" && n.Label == "放弃并退出" })
			_, _ = a.request("act", map[string]any{"window": window, "id": confirm.ID, "action": "press"})
			select {
			case <-process.exited:
				if process.exitErr != nil {
					t.Fatalf("GUI quit: %v\n%s", process.exitErr, process.stderr)
				}
			case <-time.After(e2eTimeout):
				t.Fatal("GUI quit did not exit")
			}
			client, stop := newE2ERuntimeClient(t, config)
			defer stop()
			done := make(chan struct{})
			go func() {
				defer close(done)
				for range client.Events() {
				}
			}()
			result, err := client.ListConversations(t.Context(), &runtimeclient.ListConversationsRequest{})
			if err != nil || len(result.Conversations) != 1 {
				t.Fatalf("quit altered shared history: %+v, %v", result, err)
			}
			stop()
			<-done
		})
	}
}

// setText 按当前树中的输入控件 ID 编辑完整文本。
func (a *desktopAutomation) setText(t *testing.T, window int, key, value string) {
	t.Helper()
	// 1. 选取输入角色，使用当前帧实际 ID，不复用旧节点值。
	node := a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "textField" && fmt.Sprint(n.Key) == key })
	a.call(t, "act", map[string]any{"window": window, "id": node.ID, "action": "setValue", "text": value}, nil)
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "textField" && fmt.Sprint(n.Key) == key && n.Value == value
	})
	// 树里先出现新文字，应用要再画一帧才把它记入草稿；等画完，后面的切换才不会抢在记录之前。
	if _, err := a.request("idle", map[string]any{"window": window, "timeout": 500}); err != nil && !strings.Contains(err.Error(), "timeout") {
		t.Fatal(err)
	}
}

// press 等待实际按钮可用，滚到可见位置并通过无障碍动作按下。
func (a *desktopAutomation) press(t *testing.T, window int, label string) {
	t.Helper()
	// 1. 虚拟行可能在切换后失效；仅当框架确认尚未执行动作时重新定位。
	deadline := time.Now().Add(e2eTimeout)
	for {
		node := a.waitNode(t, window, func(n desktopAutomationNode) bool {
			return (n.Role == "button" || n.Role == "menuButton" || n.Role == "tab" || n.Role == "radio") && n.Label == label && !slices.Contains(n.States, "disabled")
		})
		_, err := a.request("act", map[string]any{"window": window, "id": node.ID, "action": "scrollIntoView"})
		if err == nil {
			_, err = a.request("act", map[string]any{"window": window, "id": node.ID, "action": "press"})
		}
		if err == nil {
			return
		}
		if !strings.Contains(err.Error(), ": no_element:") || time.Now().After(deadline) {
			t.Fatalf("press %q: %v", label, err)
		}
	}
}

// TestDesktopGUIStartupAndCancel 检查连接前草稿保留，以及取消退出后仍能实际发送消息。
func TestDesktopGUIStartupAndCancel(t *testing.T) {
	// 1. 两条路径均运行真实桌面和后台，只从控件输入与模型请求观察结果。
	if runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		t.Skip("run with xvfb-run or a graphical session")
	}
	binary, err := buildDesktopOnce()
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"cold_input", "cancel_signal_close"} {
		t.Run(scenario, func(t *testing.T) {
			model := newE2EModel(t)
			config := writeDesktopE2EConfig(t, model.server.URL)
			var pausedServe *e2eProcess
			if scenario == "cold_input" {
				pausedServe, _ = startE2EProcess(t, binary, config, []string{"serve"}, false)
				connectionFile := filepath.Join(filepath.Dir(config), ".local", "state", "openvaala", "runtime-server.yaml")
				deadline := time.Now().Add(e2eTimeout)
				for {
					if _, err := os.Stat(connectionFile); err == nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("owned serve did not publish connection")
					}
					select {
					case <-pausedServe.exited:
						t.Fatalf("owned serve exited: %v", pausedServe.exitErr)
					case <-time.After(20 * time.Millisecond):
					}
				}
				if err := pausedServe.command.Process.Signal(syscall.SIGSTOP); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = pausedServe.command.Process.Signal(syscall.SIGCONT) }()
			}
			process, a, window := startDesktopGUI(t, binary, config)
			text := "native retained " + scenario
			if scenario == "cold_input" {
				a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "text" && n.Label == "连接中…" })
				a.setText(t, window, "composer-text", text)
				if err := pausedServe.command.Process.Signal(syscall.SIGCONT); err != nil {
					t.Fatal(err)
				}
			}
			a.waitNode(t, window, func(n desktopAutomationNode) bool {
				return n.Role == "popUpButton" && fmt.Sprint(n.Key) == "model" && !slices.Contains(n.States, "disabled")
			})
			if scenario == "cancel_signal_close" {
				a.setText(t, window, "composer-text", text)
				a.waitNode(t, window, func(n desktopAutomationNode) bool {
					return n.Role == "button" && n.Label == "发送" && !slices.Contains(n.States, "disabled")
				})
				a.call(t, "idle", map[string]any{"window": window, "timeout": 5000}, nil)
				a.capture(t, window, "dirty-before-signal")
				if err := process.command.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
				a.press(t, window, "取消")
			}
			// 2. 验证原草稿保持，继续编辑、发送并从模型收到完整文本。
			a.waitNode(t, window, func(n desktopAutomationNode) bool {
				return n.Role == "textField" && fmt.Sprint(n.Key) == "composer-text" && n.Value == text
			})
			text += " 中文继续"
			a.setText(t, window, "composer-text", text)
			a.press(t, window, "发送")
			request := model.next(t)
			body, _ := json.Marshal(request.body)
			if !strings.Contains(string(body), text) {
				t.Fatalf("retained draft did not reach model: %s", body)
			}
			request.reply <- e2eModelReply{text: "retained draft replied"}
			a.waitNode(t, window, func(n desktopAutomationNode) bool { return strings.Contains(n.Label, "retained draft replied") })
			a.capture(t, window, scenario)
			// 3. 从系统应用菜单退出，确认取消后的应用生命周期仍能正常结束。
			if _, err := a.request("menu", map[string]any{"window": window, "path": desktopQuitMenuPath()}); err != nil && !strings.Contains(err.Error(), "EOF") {
				t.Fatal(err)
			}
			select {
			case <-process.exited:
				if process.exitErr != nil {
					t.Fatalf("GUI exit: %v\n%s", process.exitErr, process.stderr)
				}
			case <-time.After(e2eTimeout):
				t.Fatal("GUI did not exit after menu Quit")
			}
		})
	}
}

// TestDesktopGUIProviders 从真实设置控件保存 Provider，校验重复 ID 和可选值的持久结果。
func TestDesktopGUIProviders(t *testing.T) {
	// 1. 每个场景使用单独 HOME，查询仍通过同一个真实后台公开接口。
	if runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		t.Skip("run with xvfb-run or a graphical session")
	}
	binary, err := buildDesktopOnce()
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"duplicate_id", "optional_values", "rename_default", "rename_referenced", "rapid_selection_apply", "rapid_text_apply"} {
		t.Run(scenario, func(t *testing.T) {
			model := newE2EModel(t)
			config := writeDesktopE2EConfig(t, model.server.URL)
			if scenario == "rename_default" {
				for _, name := range []string{"assistant", "default"} {
					path := filepath.Join(filepath.Dir(config), "agents", name+".yaml")
					contents, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					contents = []byte(strings.Replace(string(contents), "model:\n  provider: local\n  model: default\n", "", 1))
					if err := os.WriteFile(path, contents, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if scenario == "rapid_selection_apply" {
				appData := filepath.Join(filepath.Dir(config), ".config", "OpenVaala")
				if runtime.GOOS == "darwin" {
					appData = filepath.Join(filepath.Dir(config), "Library", "Application Support", "OpenVaala")
				}
				if err := os.MkdirAll(appData, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(appData, "preferences.json"), []byte(`{"width":1280,"height":1600}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, a, window := startDesktopGUI(t, binary, config)
			a.waitNode(t, window, func(n desktopAutomationNode) bool {
				return n.Role == "popUpButton" && fmt.Sprint(n.Key) == "model" && !slices.Contains(n.States, "disabled")
			})
			client, stop := newE2ERuntimeClient(t, config)
			defer stop()
			done := make(chan struct{})
			go func() {
				defer close(done)
				for range client.Events() {
				}
			}()
			defer func() { stop(); <-done }()
			before, _, _, err := client.ListProviders(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			a.press(t, window, "设置")
			a.press(t, window, "模型服务")
			switch scenario {
			case "duplicate_id":
				// 2. 新增同名服务显示字段错误，原模型集合仍保持完整。
				a.press(t, window, "新增服务")
				a.setField(t, window, "Provider ID", "local")
				a.press(t, window, "应用")
				a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "text" && n.Label == "模型服务 ID 已存在" })
				after, _, _, err := client.ListProviders(t.Context())
				beforeJSON, _ := json.Marshal(before)
				afterJSON, _ := json.Marshal(after)
				if err != nil || string(beforeJSON) != string(afterJSON) {
					t.Fatalf("duplicate provider overwrote configuration: %s -> %s, %v", beforeJSON, afterJSON, err)
				}
				a.capture(t, window, "duplicate-id")
			case "rapid_text_apply":
				// 文本最后一次提交与点击应用在同一批原生输入中，保存应包含本批文本。
				a.press(t, window, "local")
				checkbox := a.waitNode(t, window, func(n desktopAutomationNode) bool {
					return n.Role == "switch" && n.Label == "继承推理强度列表"
				})
				a.call(t, "act", map[string]any{"window": window, "id": checkbox.ID, "action": "press"}, nil)
				a.waitNode(t, window, func(n desktopAutomationNode) bool {
					return n.Role == "button" && n.Label == "应用" && !slices.Contains(n.States, "disabled")
				})
				field := a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "textField" && n.Label == "Base URL" })
				a.call(t, "act", map[string]any{"window": window, "id": field.ID, "action": "scrollIntoView"}, nil)
				a.call(t, "act", map[string]any{"window": window, "id": field.ID, "action": "focus"}, nil)
				a.call(t, "act", map[string]any{"window": window, "id": field.ID, "action": "selectText", "from": 0, "to": len([]rune(field.Value))}, nil)
				apply := a.waitNode(t, window, func(n desktopAutomationNode) bool {
					return n.Role == "button" && n.Label == "应用" && !slices.Contains(n.States, "disabled")
				})
				want := model.server.URL + "/batched-final-edit"
				a.call(t, "input", map[string]any{"window": window, "events": []map[string]any{
					{"type": "text", "text": want},
					{"type": "click", "x": apply.Bounds.X + apply.Bounds.Width/2, "y": apply.Bounds.Y + apply.Bounds.Height/2},
				}}, nil)
				a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "text" && n.Label == "模型服务已保存" })
				visible := a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "textField" && n.Label == "Base URL" })
				providers, _, _, err := client.ListProviders(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(providers)
				t.Logf("batched Base URL: before=%s after=%s saved_provider=%s", field.Value, visible.Value, raw)
				if !slices.ContainsFunc(providers, func(provider defs.ProviderConfig) bool { return provider.ID == "local" }) {
					t.Fatalf("saved provider local missing: %s", raw)
				}
				for _, provider := range providers {
					if provider.ID == "local" && provider.BaseURL != want {
						t.Fatalf("same-batch text was not persisted: visible=%s persisted=%s want=%s", visible.Value, provider.BaseURL, want)
					}
				}
				if visible.Value != want {
					t.Fatalf("same-batch text not visible: %s, want %s", visible.Value, want)
				}
				a.capture(t, window, "rapid-text-apply")
			case "rapid_selection_apply":
				// 选项指针输入后立即激活真实应用按钮，两动作间不等待选值提交。
				a.press(t, window, "local")
				checkbox := a.waitNode(t, window, func(n desktopAutomationNode) bool {
					return n.Role == "switch" && n.Label == "继承推理强度列表"
				})
				a.call(t, "act", map[string]any{"window": window, "id": checkbox.ID, "action": "press"}, nil)
				apply := a.waitNode(t, window, func(n desktopAutomationNode) bool {
					return n.Role == "button" && n.Label == "应用" && !slices.Contains(n.States, "disabled")
				})
				thinking := a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "popUpButton" && fmt.Sprint(n.Key) == "thinking" })
				t.Logf("before rapid select: id=%s value=%s", thinking.ID, thinking.Value)
				a.call(t, "act", map[string]any{"window": window, "id": thinking.ID, "action": "press"}, nil)
				popup := a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "popup" })
				option := a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Parent == popup.ID && n.Label == "禁用思考" })
				a.call(t, "input", map[string]any{"window": window, "events": []map[string]any{
					{"type": "click", "x": option.Bounds.X + option.Bounds.Width/2, "y": option.Bounds.Y + option.Bounds.Height/2},
				}}, nil)
				a.call(t, "act", map[string]any{"window": window, "id": apply.ID, "action": "press"}, nil)
				a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "text" && n.Label == "模型服务已保存" })
				selected := a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.ID == thinking.ID })
				providers, _, _, err := client.ListProviders(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(providers)
				t.Logf("after rapid select: value=%s saved_provider=%s", selected.Value, raw)
				a.capture(t, window, "rapid-selection-apply")
				for _, provider := range providers {
					if provider.ID == "local" {
						saved := provider.Models["default"].Thinking
						if saved == nil || *saved {
							t.Fatalf("rapid selection was not saved: visible=%s persisted=%s", selected.Value, raw)
						}
					}
				}
			case "optional_values":
				// 3. 显式禁用、启用和继承分别落为 false、true 和 nil；空推理列表保留显式空值。
				a.press(t, window, "local")
				inherit := a.waitNode(t, window, func(n desktopAutomationNode) bool {
					return n.Role == "switch" && n.Label == "继承推理强度列表"
				})
				a.call(t, "act", map[string]any{"window": window, "id": inherit.ID, "action": "scrollIntoView"}, nil)
				a.call(t, "act", map[string]any{"window": window, "id": inherit.ID, "action": "press"}, nil)
				for index, thinking := range []struct {
					label string
					want  string
				}{{"禁用思考", "false"}, {"启用思考", "true"}, {"继承思考设置", "inherit"}} {
					if index > 0 {
						a.dismissToast(t, window, "模型服务已保存")
					}
					a.selectOption(t, window, "thinking", thinking.label, thinking.want)
					a.press(t, window, "应用")
					a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "text" && n.Label == "模型服务已保存" })
					providers, _, _, err := client.ListProviders(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					got := "missing"
					for _, provider := range providers {
						if provider.ID == "local" {
							selected := provider.Models["default"]
							if selected.Thinking == nil {
								got = "inherit"
							} else {
								got = fmt.Sprint(*selected.Thinking)
							}
							if selected.ReasoningEfforts == nil || len(selected.ReasoningEfforts) != 0 {
								t.Fatalf("explicit empty reasoning efforts = %#v", selected.ReasoningEfforts)
							}
						}
					}
					if got != thinking.want {
						t.Fatalf("saved thinking = %s, want %s", got, thinking.want)
					}
				}
				a.capture(t, window, "optional-values")
			case "rename_default", "rename_referenced":
				// 4. 设置默认模型后改其本地别名，同次应用选择替代默认引用。
				a.selectOption(t, window, "global-default", "local/default", "local/default")
				a.press(t, window, "local")
				a.setField(t, window, "本地别名", "renamed-default")
				a.selectOption(t, window, "provider-default-model", "renamed-default", "default")
				a.press(t, window, "应用")
				if scenario == "rename_referenced" {
					a.waitNode(t, window, func(n desktopAutomationNode) bool {
						return n.Role == "text" && strings.Contains(n.Label, `model "default" is not configured`)
					})
					after, _, _, err := client.ListProviders(t.Context())
					beforeJSON, _ := json.Marshal(before)
					afterJSON, _ := json.Marshal(after)
					if err != nil || string(beforeJSON) != string(afterJSON) {
						t.Fatalf("rejected provider rename did not restore configuration: %s -> %s, %v", beforeJSON, afterJSON, err)
					}
					return
				}
				a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "text" && n.Label == "模型服务已保存" })
				providers, defaultModel, _, err := client.ListProviders(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if defaultModel.String() != "local/renamed-default" {
					t.Fatalf("default model = %s", defaultModel.String())
				}
				for _, provider := range providers {
					if provider.ID == "local" {
						if provider.Models["default"] != nil || provider.Models["renamed-default"] == nil || provider.Models["renamed-default"].UpstreamModel != "default" {
							t.Fatalf("renamed provider models = %+v", provider.Models)
						}
					}
				}
				a.capture(t, window, "renamed-default")
			}
		})
	}
}

// setField 按当前表单标签编辑首个匹配的原生输入字段。
func (a *desktopAutomation) setField(t *testing.T, window int, label, value string) {
	t.Helper()
	// 1. 通过角色与标签定位当前字段，滚动后写入控件。
	node := a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "textField" && n.Label == label })
	a.call(t, "act", map[string]any{"window": window, "id": node.ID, "action": "scrollIntoView"}, nil)
	a.call(t, "act", map[string]any{"window": window, "id": node.ID, "action": "setValue", "text": value}, nil)
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "textField" && n.Label == label && n.Value == value
	})
}

// selectOption 通过原生下拉菜单选择实际可用项。
func (a *desktopAutomation) selectOption(t *testing.T, window int, key, label, value string) {
	t.Helper()
	// 1. 打开当前下拉控件，按可访问树中的选项执行选择。
	node := a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "popUpButton" && fmt.Sprint(n.Key) == key && !slices.Contains(n.States, "disabled")
	})
	popupsBefore := a.countPopups(t, window)
	a.call(t, "act", map[string]any{"window": window, "id": node.ID, "action": "scrollIntoView"}, nil)
	a.call(t, "act", map[string]any{"window": window, "id": node.ID, "action": "press"}, nil)
	popup := a.waitTopmostPopup(t, window, popupsBefore+1)
	option := a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Parent == popup.ID && n.Label == label })
	a.call(t, "input", map[string]any{"window": window, "events": []map[string]any{{"type": "click", "x": option.Bounds.X + option.Bounds.Width/2, "y": option.Bounds.Y + option.Bounds.Height/2}}}, nil)
	// 2. 等待公开选择值提交并关闭 popup，再允许调用方按应用。
	deadline := time.Now().Add(e2eTimeout)
	for {
		var tree struct {
			Nodes []desktopAutomationNode `json:"nodes"`
		}
		a.call(t, "tree", map[string]any{"window": window}, &tree)
		selected, popups := false, 0
		for _, candidate := range tree.Nodes {
			if candidate.ID == node.ID && candidate.Value == value {
				selected = true
			}
			if candidate.Role == "popup" {
				popups++
			}
		}
		if selected && popups == popupsBefore {
			return
		}
		if time.Now().After(deadline) {
			raw, _ := json.Marshal(tree)
			t.Fatalf("select %s did not commit %q: %s", key, value, raw)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

// TestDesktopGUIAgentNavigation 验证真实子 Agent 流式内容与快速根子阅读切换。
func TestDesktopGUIAgentNavigation(t *testing.T) {
	// 1. 控件创建对话，模型通过真实工具派生子 Agent。
	if runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		t.Skip("run with xvfb-run or a graphical session")
	}
	binary, err := buildDesktopOnce()
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"root_task_root"} {
		t.Run(scenario, func(t *testing.T) {
			model := newE2EModel(t)
			config := writeDesktopE2EConfig(t, model.server.URL)
			_, a, window := startDesktopGUI(t, binary, config)
			a.waitNode(t, window, func(n desktopAutomationNode) bool {
				return n.Role == "popUpButton" && fmt.Sprint(n.Key) == "model" && !slices.Contains(n.States, "disabled")
			})
			a.setText(t, window, "composer-text", "native agent navigation")
			a.press(t, window, "发送")
			model.next(t).reply <- e2eModelReply{tool: "spawn_agent", arguments: `{"task_name":"native-review","message":"check native task","fork_turns":"none"}`}
			rootRequest, taskRequest := model.nextRootAndTask(t, "native-review")
			rootChunks := make(chan string, 4)
			rootRequest.reply <- e2eModelReply{stream: rootChunks}
			rootChunks <- "root delegation complete"
			chunks := make(chan string, 4)
			taskRequest.reply <- e2eModelReply{stream: chunks}
			chunks <- "task native streaming"
			a.waitNode(t, window, func(n desktopAutomationNode) bool { return strings.Contains(n.Label, "root delegation complete") })
			client, stop := newE2ERuntimeClient(t, config)
			defer stop()
			done := make(chan struct{})
			taskDeltaProcessed := make(chan struct{})
			var taskDeltaOnce sync.Once
			go func() {
				defer close(done)
				for event := range client.Events() {
					if changed, ok := event.(*runtimeclient.ItemChanged); ok && changed.Delta != nil {
						raw, _ := json.Marshal(changed.Delta)
						if strings.Contains(string(raw), "task-only-followup") {
							taskDeltaOnce.Do(func() { close(taskDeltaProcessed) })
						}
					}
				}
			}()
			defer func() { stop(); <-done }()
			conversations, err := client.ListConversations(t.Context(), &runtimeclient.ListConversationsRequest{})
			if err != nil || len(conversations.Conversations) != 1 {
				t.Fatalf("agent conversations = %+v, %v", conversations, err)
			}
			conversation := conversations.Conversations[0].Conversation
			sessions, err := client.ListSessions(t.Context(), &runtimeclient.ListSessionsRequest{ConversationID: conversation.ID})
			if err != nil {
				t.Fatal(err)
			}
			taskID := ""
			for _, session := range sessions.Sessions {
				if session.TaskName == "native-review" {
					taskID = string(session.ID)
				}
			}
			if taskID == "" {
				t.Fatalf("no real task session: %+v", sessions)
			}
			if _, err := client.OpenConversation(t.Context(), &runtimeclient.OpenConversationRequest{ConversationID: conversation.ID, SessionID: defs.SessionID(taskID), SkipHistory: true}); err != nil {
				t.Fatal(err)
			}
			// 2. 点击树及消息来源中的真实 Session 链接，在任务仍输出时快速切换。
			t.Logf("streaming task %s → root %s → task → root", taskID, conversation.RootSessionID)
			a.press(t, window, "Session "+taskID)
			a.waitNode(t, window, func(n desktopAutomationNode) bool { return strings.Contains(n.Label, "task native streaming") })
			a.press(t, window, "Session "+string(conversation.RootSessionID))
			a.press(t, window, "Session "+taskID)
			a.press(t, window, "Session "+string(conversation.RootSessionID))
			chunks <- " task-only-followup"
			select {
			case <-taskDeltaProcessed:
			case <-time.After(e2eTimeout):
				t.Fatal("task followup was not processed by real runtime")
			}
			rootChunks <- " root-causal-barrier"
			a.waitNode(t, window, func(n desktopAutomationNode) bool { return strings.Contains(n.Label, "root-causal-barrier") })
			var rootTree struct {
				Nodes []desktopAutomationNode `json:"nodes"`
			}
			a.call(t, "tree", map[string]any{"window": window}, &rootTree)
			for _, node := range rootTree.Nodes {
				if strings.Contains(node.Label, "task-only-followup") {
					t.Fatalf("task delta leaked into root reading target: %+v", node)
				}
			}
			a.press(t, window, "Session "+taskID)
			a.waitNode(t, window, func(n desktopAutomationNode) bool { return strings.Contains(n.Label, "task-only-followup") })
			a.capture(t, window, "task-reading")
			// 2b. 子 Session 的标题栏是面包屑「主 Agent › 当前」，输入区只有只读说明；点「主 Agent」回到根 Session。
			a.waitNode(t, window, func(n desktopAutomationNode) bool {
				return n.Role == "text" && n.Label == "native-review" && n.Bounds.Y < 80
			})
			a.waitNode(t, window, func(n desktopAutomationNode) bool {
				return n.Role == "text" && strings.HasPrefix(n.Label, "子 Agent 只读")
			})
			for _, node := range a.getNodes(t, window) {
				key := fmt.Sprint(node.Key)
				if key == "composer-text" || key == "model" || key == "work-location" || node.Label == "压缩上下文" || node.Label == "上下文用量" {
					t.Fatalf("child session still shows composer control %q (%s)", node.Label, key)
				}
			}
			a.press(t, window, "主 Agent")
			a.waitNode(t, window, func(n desktopAutomationNode) bool {
				return n.Role == "textField" && fmt.Sprint(n.Key) == "composer-text"
			})
			a.waitNode(t, window, func(n desktopAutomationNode) bool { return strings.Contains(n.Label, "root delegation complete") })
			if slices.ContainsFunc(a.getNodes(t, window), func(n desktopAutomationNode) bool { return strings.HasPrefix(fmt.Sprint(n.Key), "crumb-") }) {
				t.Fatal("breadcrumbs are still shown on the root session")
			}
			// 3. 结束任务后，根 Agent 收到真实结果并继续运行。
			close(chunks)
			close(rootChunks)
			model.replyAll(t, e2eModelReply{text: "root received task completion"})
			a.press(t, window, "Session "+string(conversation.RootSessionID))
			a.waitNode(t, window, func(n desktopAutomationNode) bool { return strings.Contains(n.Label, "root received task completion") })
			a.capture(t, window, "root-reading")
		})
	}
}

// TestDesktopGUIFork 检查对话菜单分叉历史，后续输入只写入新对话。
func TestDesktopGUIFork(t *testing.T) {
	if runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		t.Skip("run with xvfb-run or a graphical session")
	}
	binary, err := buildDesktopOnce()
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"completed_history"} {
		t.Run(scenario, func(t *testing.T) {
			// 1. 真实模型完成一轮后，从对话菜单分叉。
			model := newE2EModel(t)
			config := writeDesktopE2EConfig(t, model.server.URL)
			_, a, window := startDesktopGUI(t, binary, config)
			a.waitNode(t, window, func(n desktopAutomationNode) bool {
				return n.Role == "popUpButton" && fmt.Sprint(n.Key) == "model" && !slices.Contains(n.States, "disabled")
			})
			a.setText(t, window, "composer-text", "native fork origin")
			a.press(t, window, "发送")
			model.next(t).reply <- e2eModelReply{text: "original completed response"}
			a.waitNode(t, window, func(n desktopAutomationNode) bool {
				return strings.Contains(n.Description, "default · idle · completed")
			})
			client, stop := newE2ERuntimeClient(t, config)
			defer stop()
			done := make(chan struct{})
			go func() {
				defer close(done)
				for range client.Events() {
				}
			}()
			defer func() { stop(); <-done }()
			before, err := client.ListConversations(t.Context(), &runtimeclient.ListConversationsRequest{})
			if err != nil || len(before.Conversations) != 1 {
				t.Fatalf("before fork: %+v, %v", before, err)
			}
			original := before.Conversations[0].Conversation
			// 主题菜单在无障碍树里，直接按条目点击，不用坐标或 xdotool。
			a.openTitleMenu(t, window)
			a.clickMenuItem(t, window, "分叉")
			a.waitNode(t, window, func(n desktopAutomationNode) bool {
				return n.Role == "button" && strings.HasPrefix(n.Label, "Session ") && n.Label != "Session "+string(original.RootSessionID)
			})
			a.waitNode(t, window, func(n desktopAutomationNode) bool { return strings.Contains(n.Label, "original completed response") })
			// 2. 向分叉发送新消息，原对话历史仍保持原样。
			a.setText(t, window, "composer-text", "native fork exclusive followup")
			a.press(t, window, "发送")
			model.next(t).reply <- e2eModelReply{text: "fork completed response"}
			a.waitNode(t, window, func(n desktopAutomationNode) bool { return strings.Contains(n.Label, "fork completed response") })
			after, err := client.ListConversations(t.Context(), &runtimeclient.ListConversationsRequest{})
			if err != nil || len(after.Conversations) != 2 {
				t.Fatalf("after fork: %+v, %v", after, err)
			}
			for _, entry := range after.Conversations {
				items, err := client.ListItems(t.Context(), &runtimeclient.ListItemsRequest{ConversationID: entry.Conversation.ID, SessionID: entry.Conversation.RootSessionID})
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(items.Items)
				if strings.Contains(string(raw), "native fork exclusive followup") != (entry.Conversation.ID != original.ID) {
					t.Fatalf("fork input landed in wrong conversation %s: %s", entry.Conversation.ID, raw)
				}
			}
			a.capture(t, window, "forked-history")
			// 3. 悬停才出现的「从这里分叉」：对第二条用户消息分叉，新对话只含第一轮。
			var tree struct {
				Nodes []desktopAutomationNode `json:"nodes"`
			}
			a.call(t, "tree", map[string]any{"window": window}, &tree)
			var forkButtons []desktopAutomationNode
			for _, node := range tree.Nodes {
				if node.Role == "button" && node.Label == "从这里分叉" {
					forkButtons = append(forkButtons, node)
				}
			}
			if len(forkButtons) != 2 {
				t.Fatalf("fork-from-here buttons = %d, want one per user message", len(forkButtons))
			}
			a.call(t, "act", map[string]any{"window": window, "id": forkButtons[1].ID, "action": "press"}, nil)
			var forkedID defs.ConversationID
			for _, entry := range after.Conversations {
				if entry.Conversation.ID != original.ID {
					forkedID = entry.Conversation.ID
				}
			}
			deadline := time.Now().Add(e2eTimeout)
			for {
				list, err := client.ListConversations(t.Context(), &runtimeclient.ListConversationsRequest{})
				if err != nil {
					t.Fatal(err)
				}
				var child *defs.Conversation
				for _, entry := range list.Conversations {
					if entry.Conversation.ForkedFromID == forkedID {
						child = &entry.Conversation
					}
				}
				if child != nil {
					items, err := client.ListItems(t.Context(), &runtimeclient.ListItemsRequest{ConversationID: child.ID, SessionID: child.RootSessionID})
					if err != nil {
						t.Fatal(err)
					}
					raw, _ := json.Marshal(items.Items)
					if !strings.Contains(string(raw), "native fork origin") || strings.Contains(string(raw), "native fork exclusive followup") {
						t.Fatalf("fork before the second turn copied wrong history: %s", raw)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("fork from message did not create a conversation: %+v", list)
				}
				time.Sleep(50 * time.Millisecond)
			}
		})
	}
}

// TestDesktopGUIAppearance 检查真实外观选择、窄窗口抽屉、状态栏面板按钮、命令面板，以及偏好落盘。
func TestDesktopGUIAppearance(t *testing.T) {
	// 1. 窗口尺寸来自本测试 HOME 的已保存偏好，主题通过真实设置控件选择。
	if runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		t.Skip("run with xvfb-run or a graphical session")
	}
	binary, err := buildDesktopOnce()
	if err != nil {
		t.Fatal(err)
	}
	for _, appearance := range []struct {
		theme, label, scale string
		width               int
	}{{"light", "浅色", "1", 1280}, {"dark", "深色", "1", 1280}, {"dark", "深色", "1", 800}, {"light", "浅色", "2", 1280}} {
		t.Run(fmt.Sprintf("%s_%d_scale%s", appearance.theme, appearance.width, appearance.scale), func(t *testing.T) {
			t.Setenv("GDK_SCALE", appearance.scale)
			width := appearance.width
			model := newE2EModel(t)
			config := writeDesktopE2EConfig(t, model.server.URL)
			appData := filepath.Join(filepath.Dir(config), ".config", "OpenVaala")
			if runtime.GOOS == "darwin" {
				appData = filepath.Join(filepath.Dir(config), "Library", "Application Support", "OpenVaala")
			}
			if err := os.MkdirAll(appData, 0700); err != nil {
				t.Fatal(err)
			}
			preferencesPath := filepath.Join(appData, "preferences.json")
			// 宽窗口使用默认布局；窄窗口侧栏和右 Dock 是抽屉，保存为关闭以免启动时遮住内容。
			workspace := `"sidebar_width":200`
			if width < 900 {
				workspace += `,"sidebar_open":false,"right":{"open":false,"active_panel":"agent"}`
			}
			preferences := fmt.Sprintf(`{"width":%d,"height":620,"workspace":{%s}}`, width, workspace)
			if err := os.WriteFile(preferencesPath, []byte(preferences), 0600); err != nil {
				t.Fatal(err)
			}
			process, a, window := startDesktopGUI(t, binary, config)
			a.waitNode(t, window, func(n desktopAutomationNode) bool {
				return n.Role == "popUpButton" && fmt.Sprint(n.Key) == "model" && !slices.Contains(n.States, "disabled")
			})
			a.press(t, window, "设置")
			if width < 900 {
				// 窄窗口里设置导航在侧栏抽屉里，进入设置时抽屉打开；选一项后抽屉收起。
				a.press(t, window, "通用")
				a.waitNodeAbsent(t, window, func(n desktopAutomationNode) bool { return n.Role == "button" && n.Label == "Executor" })
			}
			// 外观是分段控件：点对应的一段，等它变成选中。
			a.clickNode(t, window, a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "radio" && n.Label == appearance.label }), "")
			a.waitNode(t, window, func(n desktopAutomationNode) bool {
				return n.Role == "radio" && n.Label == appearance.label && slices.Contains(n.States, "checked")
			})
			// 回到对话：窄窗口里导航在侧栏抽屉里，用 Esc 返回，不依赖抽屉。
			a.pressKey(t, window, "Escape")
			if width < 900 {
				// 抽屉打开后标题栏在遮罩之外、不在无障碍树里，所以先记下标题栏文字的位置。
				titleBar := a.waitNode(t, window, func(n desktopAutomationNode) bool {
					return n.Role == "text" && n.Label == "OpenVaala" && n.Bounds.Y < 36
				})
				a.press(t, window, "切换侧栏")
				// 抽屉从标题栏下缘开始；真实点击标题栏文字落在标题栏上而不是遮罩上，抽屉保持打开。
				search := a.waitNode(t, window, func(n desktopAutomationNode) bool {
					return n.Role == "textField" && fmt.Sprint(n.Key) == "conversation-search"
				})
				const titleBarHeight = 36
				if search.Bounds.Y < titleBarHeight || titleBar.Bounds.Y+titleBar.Bounds.Height > titleBarHeight {
					t.Fatalf("drawer content y = %.1f, title bar text = %+v: the drawer must start below the %d high title bar", search.Bounds.Y, titleBar.Bounds, titleBarHeight)
				}
				a.capture(t, window, appearance.theme+"-narrow-drawer")
				a.clickNode(t, window, titleBar, "")
				a.call(t, "idle", map[string]any{"window": window, "timeout": 5000}, nil)
				if _, found := a.findNodeByKey(t, window, "textField", "conversation-search"); !found {
					t.Fatal("clicking the title bar closed the drawer: the scrim covers the title bar")
				}
			}
			a.press(t, window, "新对话")
			a.call(t, "idle", map[string]any{"window": window, "timeout": 5000}, nil)
			a.capture(t, window, appearance.theme+"-workbench")
			// 2. 状态栏按钮打开右 Dock 的 RPC 面板；命令面板经 Ctrl+K 对应的菜单项打开。
			if width >= 900 {
				a.press(t, window, "RPC")
				a.waitNode(t, window, func(n desktopAutomationNode) bool {
					return n.Role == "button" && n.Label == "暂停"
				})
				a.capture(t, window, appearance.theme+"-rpc-dock")
				// 快捷键注册为菜单加速键，自动化经原生菜单项触发，与按下 Ctrl+K 走同一处理。
				a.call(t, "menu", map[string]any{"window": window, "path": []string{"转到", "命令面板"}}, nil)
				a.waitNode(t, window, func(n desktopAutomationNode) bool {
					return n.Role == "textField" && fmt.Sprint(n.Key) == "command-search"
				})
				a.capture(t, window, appearance.theme+"-command-palette")
				a.call(t, "input", map[string]any{"window": window, "events": []map[string]any{{"type": "key", "key": "Escape"}}}, nil)
				// 3. 侧栏与右 Dock 折叠后输入区仍可编辑；再点当前面板按钮关闭右 Dock。
				a.press(t, window, "切换侧栏")
				a.press(t, window, "RPC")
			} else {
				a.capture(t, window, appearance.theme+"-sidebar-sheet")
				a.call(t, "input", map[string]any{"window": window, "events": []map[string]any{{"type": "key", "key": "Escape"}}}, nil)
			}
			a.setText(t, window, "composer-text", "窄窗口中文草稿")
			a.capture(t, window, appearance.theme+"-focused-composer")
			a.setText(t, window, "composer-text", "")
			a.waitNode(t, window, func(n desktopAutomationNode) bool {
				return n.Role == "button" && n.Label == "发送" && slices.Contains(n.States, "disabled")
			})
			a.call(t, "idle", map[string]any{"window": window, "timeout": 5000}, nil)
			process.stop(t)
			saved, err := os.ReadFile(preferencesPath)
			if err != nil {
				t.Fatal(err)
			}
			var result struct {
				Theme     string `json:"theme"`
				Width     int    `json:"width"`
				Workspace struct {
					SidebarOpen bool `json:"sidebar_open"`
					Right       struct {
						Open bool `json:"open"`
					} `json:"right"`
					Bottom struct {
						Open bool `json:"open"`
					} `json:"bottom"`
				} `json:"workspace"`
			}
			if err := json.Unmarshal(saved, &result); err != nil {
				t.Fatal(err)
			}
			// 宽窗口：侧栏和右 Dock 已折叠；窄窗口：抽屉均已关闭；下 Dock 在终端面板加入前不打开。
			if result.Theme != appearance.theme || result.Width != width || result.Workspace.SidebarOpen || result.Workspace.Right.Open || result.Workspace.Bottom.Open {
				t.Fatalf("saved appearance = %s", saved)
			}
		})
	}
}

// TestDesktopGUITerminal 检查状态栏终端按钮打开下方面板、自动新开本机终端，键盘输入经 serve 的 PTY 执行并回显输出，并能关闭标签。
func TestDesktopGUITerminal(t *testing.T) {
	// 1. 只在图形测试会话执行；等待连上本机服务，终端才会自动打开。
	if runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		t.Skip("run with xvfb-run or a graphical session")
	}
	binary, err := buildDesktopOnce()
	if err != nil {
		t.Fatal(err)
	}
	// 用 POSIX sh 作为 shell：临时 HOME 里没有 zsh 配置，zsh 会先问首次配置问题而吃掉按键。
	t.Setenv("SHELL", "/bin/sh")
	model := newE2EModel(t)
	config := writeDesktopE2EConfig(t, model.server.URL)
	_, a, window := startDesktopGUI(t, binary, config)
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "popUpButton" && fmt.Sprint(n.Key) == "model" && !slices.Contains(n.States, "disabled")
	})
	// 2. 状态栏的「终端」按钮打开下 Dock，面板自动新开一个终端。
	a.press(t, window, "终端")
	terminal := a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "group" && strings.HasPrefix(fmt.Sprint(n.Key), "terminal-") && n.Label == "终端"
	})
	a.call(t, "act", map[string]any{"window": window, "id": terminal.ID, "action": "focus"}, nil)
	a.call(t, "idle", map[string]any{"window": window, "timeout": 5000}, nil)
	// 3. 等 shell 提示符出现后输入命令并回车：算术展开让输出行和命令回显不同，只有真实输出才含 native-42。
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Key == terminal.Key && strings.Contains(n.Value, "$")
	})
	a.call(t, "input", map[string]any{"window": window, "events": []map[string]any{{"type": "text", "text": "echo native-$((40+2))"}}}, nil)
	a.call(t, "input", map[string]any{"window": window, "events": []map[string]any{{"type": "key", "key": "Enter"}}}, nil)
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Key == terminal.Key && strings.Contains(n.Value, "native-42")
	})
	a.capture(t, window, "terminal")
	// 3b. 退出 shell 后终端右下角出现「重新启动」；点击后同一个标签里重新出现提示符，旧输出不在了。
	a.typeText(t, window, "exit")
	a.pressKey(t, window, "Enter")
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Key == terminal.Key && strings.Contains(n.Value, "进程已退出")
	})
	a.capture(t, window, "terminal-exited")
	a.press(t, window, "重新启动")
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Key == terminal.Key && strings.Contains(n.Value, "$") && !strings.Contains(n.Value, "native-42") && !strings.Contains(n.Value, "进程已退出")
	})
	if slices.ContainsFunc(a.getNodes(t, window), func(n desktopAutomationNode) bool { return n.Role == "button" && n.Label == "重新启动" }) {
		t.Fatal("restart button is still shown for the running terminal")
	}
	a.capture(t, window, "terminal-restarted")
	// 4. 新建第二个标签后关闭第一个（不是最后一个）：应用仍然存活，只剩第二个标签。
	a.press(t, window, "新建终端")
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "group" && fmt.Sprint(n.Key) == "terminal-2" && n.Label == "终端"
	})
	// 两个标签的位置相同，第二个标题带「 (2)」，第一个不带。
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "button" && strings.HasPrefix(n.Label, "local · ") && strings.HasSuffix(n.Label, " (2)")
	})
	a.capture(t, window, "terminal-two-tabs")
	// 关闭按钮按标签顺序出现在原生树里，第一个属于第一个标签。
	var tabsTree struct {
		Nodes []desktopAutomationNode `json:"nodes"`
	}
	a.call(t, "tree", map[string]any{"window": window}, &tabsTree)
	closeButtons := []desktopAutomationNode{}
	for _, node := range tabsTree.Nodes {
		if node.Role == "button" && node.Label == "关闭终端" {
			closeButtons = append(closeButtons, node)
		}
	}
	if len(closeButtons) != 2 {
		t.Fatalf("terminal close buttons = %d", len(closeButtons))
	}
	a.call(t, "act", map[string]any{"window": window, "id": closeButtons[0].ID, "action": "press"}, nil)
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "group" && fmt.Sprint(n.Key) == "terminal-2" && n.Label == "终端"
	})
	// 5. 再取一次原生树：窗口仍能响应，第一个终端已消失，只剩一个关闭按钮。
	a.call(t, "idle", map[string]any{"window": window, "timeout": 5000}, nil)
	a.call(t, "tree", map[string]any{"window": window}, &tabsTree)
	remainingTerminals, remainingCloseButtons := 0, 0
	for _, node := range tabsTree.Nodes {
		switch {
		case node.Role == "group" && fmt.Sprint(node.Key) == "terminal-1":
			t.Fatal("closed first terminal is still shown")
		case node.Role == "group" && strings.HasPrefix(fmt.Sprint(node.Key), "terminal-") && node.Label == "终端":
			remainingTerminals++
		case node.Role == "button" && node.Label == "关闭终端":
			remainingCloseButtons++
		}
	}
	if remainingTerminals != 1 || remainingCloseButtons != 1 {
		t.Fatalf("terminals = %d, close buttons = %d", remainingTerminals, remainingCloseButtons)
	}
	a.capture(t, window, "terminal-first-closed")
	// 6. Ctrl+` 对应的「视图 › 打开或聚焦终端」：Dock 关着时打开它并把焦点交给终端标签。
	// 手测：在真实键盘上按 Ctrl+`，终端有焦点和没有焦点时都应打开或聚焦终端（自动化只能走菜单，按键由 GTK 加速键处理）。
	a.press(t, window, "终端")
	deadline := time.Now().Add(e2eTimeout)
	for slices.ContainsFunc(a.getNodes(t, window), func(n desktopAutomationNode) bool { return fmt.Sprint(n.Key) == "terminal-2" }) {
		if time.Now().After(deadline) {
			t.Fatal("terminal dock did not close")
		}
		time.Sleep(40 * time.Millisecond)
	}
	if _, err := a.request("menu", map[string]any{"window": window, "path": []string{"视图", "打开或聚焦终端"}}); err != nil {
		t.Fatal(err)
	}
	a.waitFocusedKey(t, window, "terminal-2")
}

// TestDesktopGUIConversationSwitch 检查快速切换后，两条真实外部连接的并发输入只更新当前阅读目标。
func TestDesktopGUIConversationSwitch(t *testing.T) {
	if runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		t.Skip("run with xvfb-run or a graphical session")
	}
	binary, err := buildDesktopOnce()
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"a_b_a_with_external_writers"} {
		t.Run(scenario, func(t *testing.T) {
			// 1. 通过原生控件分别创建 A、B 两个有历史的对话。
			model := newE2EModel(t)
			config := writeDesktopE2EConfig(t, model.server.URL)
			_, a, window := startDesktopGUI(t, binary, config)
			a.waitNode(t, window, func(n desktopAutomationNode) bool {
				return n.Role == "popUpButton" && fmt.Sprint(n.Key) == "model" && !slices.Contains(n.States, "disabled")
			})
			for _, name := range []string{"A", "B"} {
				if name == "B" {
					a.press(t, window, "新对话")
				}
				a.setText(t, window, "composer-text", "native target "+name)
				a.press(t, window, "发送")
				model.next(t).reply <- e2eModelReply{text: "initial target " + name}
				a.waitNode(t, window, func(n desktopAutomationNode) bool {
					return strings.Contains(n.Description, "default · idle · completed")
				})
			}
			senderA, stopA := newE2ERuntimeClient(t, config)
			defer stopA()
			senderB, stopB := newE2ERuntimeClient(t, config)
			defer stopB()
			doneA, doneB := make(chan struct{}), make(chan struct{})
			aDeltaProcessed := make(chan struct{})
			var aDeltaOnce sync.Once
			go func() {
				defer close(doneA)
				for event := range senderA.Events() {
					if changed, ok := event.(*runtimeclient.ItemChanged); ok && changed.Delta != nil {
						raw, _ := json.Marshal(changed.Delta)
						if strings.Contains(string(raw), "A-followup") {
							aDeltaOnce.Do(func() { close(aDeltaProcessed) })
						}
					}
				}
			}()
			go func() {
				defer close(doneB)
				for range senderB.Events() {
				}
			}()
			defer func() { stopA(); stopB(); <-doneA; <-doneB }()
			conversations, err := senderA.ListConversations(t.Context(), &runtimeclient.ListConversationsRequest{})
			if err != nil || len(conversations.Conversations) != 2 {
				t.Fatalf("targets: %+v, %v", conversations, err)
			}
			for _, entry := range conversations.Conversations {
				if entry.Conversation.Title == "native target A" {
					if _, err := senderA.OpenConversation(t.Context(), &runtimeclient.OpenConversationRequest{ConversationID: entry.Conversation.ID, SessionID: entry.Conversation.RootSessionID, SkipHistory: true}); err != nil {
						t.Fatal(err)
					}
				}
			}
			// 2. 窗口快速 A/B/A 切换，随后两个外部 Client 分别提交新输入。
			for _, title := range []string{"native target A", "native target B", "native target A"} {
				a.press(t, window, title)
			}
			a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "text" && n.Label == "initial target A" })
			for _, entry := range conversations.Conversations {
				sender := senderA
				text := "external-A"
				if entry.Conversation.Title == "native target B" {
					sender = senderB
					text = "external-B"
				}
				if _, err := sender.SubmitInput(t.Context(), &runtimeclient.SubmitInputRequest{ConversationID: entry.Conversation.ID, Content: []*defs.Content{{Kind: defs.ContentText, Text: &defs.TextContent{Text: text}}}}); err != nil {
					t.Fatal(err)
				}
			}
			streams := map[string]chan string{}
			for range 2 {
				request := model.next(t)
				raw, _ := json.Marshal(request.body)
				name := "A"
				if strings.Contains(string(raw), "external-B") {
					name = "B"
				}
				stream := make(chan string, 4)
				streams[name] = stream
				request.reply <- e2eModelReply{stream: stream}
				stream <- "live-target-" + name
			}
			if len(streams) != 2 {
				t.Fatalf("external inputs did not run on separate targets: %v", streams)
			}
			a.waitNode(t, window, func(n desktopAutomationNode) bool {
				return n.Role == "text" && strings.Contains(n.Label, "live-target-A")
			})
			// 3. 切到 B 后继续两边流式输出，当前窗口只展示 B 的后续增量。
			a.press(t, window, "native target B")
			a.waitNode(t, window, func(n desktopAutomationNode) bool {
				return n.Role == "text" && strings.Contains(n.Label, "live-target-B")
			})
			streams["A"] <- " A-followup"
			select {
			case <-aDeltaProcessed:
			case <-time.After(e2eTimeout):
				t.Fatal("target A followup was not processed by real runtime")
			}
			streams["B"] <- " B-followup"
			a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "text" && strings.Contains(n.Label, "B-followup") })
			var tree struct {
				Nodes []desktopAutomationNode `json:"nodes"`
			}
			a.call(t, "tree", map[string]any{"window": window}, &tree)
			for _, node := range tree.Nodes {
				if node.Role == "text" && (strings.Contains(node.Label, "live-target-A") || strings.Contains(node.Label, "A-followup")) {
					t.Fatalf("A delta leaked into target B: %+v", node)
				}
			}
			close(streams["A"])
			close(streams["B"])
			a.capture(t, window, "external-target-b")
			a.press(t, window, "native target A")
			a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "text" && strings.Contains(n.Label, "A-followup") })
		})
	}
}

// getNodes 读取当前窗口的无障碍树节点。
func (a *desktopAutomation) getNodes(t *testing.T, window int) []desktopAutomationNode {
	t.Helper()
	var tree struct {
		Nodes []desktopAutomationNode `json:"nodes"`
	}
	a.call(t, "tree", map[string]any{"window": window}, &tree)
	return tree.Nodes
}

// sendInput 向窗口内容发送输入事件，和 desktopdrive 的 click、key 一样。
func (a *desktopAutomation) sendInput(t *testing.T, window int, events ...map[string]any) {
	t.Helper()
	a.call(t, "input", map[string]any{"window": window, "events": events}, nil)
}

// clickNode 在控件中心点击；菜单条目没有按压动作，只能点击。
func (a *desktopAutomation) clickNode(t *testing.T, window int, node desktopAutomationNode, button string) {
	t.Helper()
	event := map[string]any{"type": "click", "x": node.Bounds.X + node.Bounds.Width/2, "y": node.Bounds.Y + node.Bounds.Height/2}
	if button != "" {
		event["button"] = button
	}
	a.sendInput(t, window, event)
}

// waitSettledNode 返回 matches 选中的控件，位置取自窗口画完已请求的帧之后：
// 列表刷新的结果先请求一帧、帧画完才重排，不等这一帧就读边界，读到的是重排前的位置，坐标点击会落空。
func (a *desktopAutomation) waitSettledNode(t *testing.T, window int, matches func(desktopAutomationNode) bool) desktopAutomationNode {
	t.Helper()
	deadline := time.Now().Add(e2eTimeout)
	for {
		// 持续动画（运行中的转圈）让窗口一直有待画的帧，idle 超时时按已有位置继续，由两次读取一致来确认。
		a.settle(t, window)
		first := a.waitNode(t, window, matches)
		a.settle(t, window)
		second := a.waitNode(t, window, matches)
		if first.Bounds == second.Bounds {
			return second
		}
		if time.Now().After(deadline) {
			t.Fatalf("node bounds keep moving: %+v -> %+v", first.Bounds, second.Bounds)
		}
	}
}

// settle 等窗口画完已请求的帧；持续动画不要求完全停止。
func (a *desktopAutomation) settle(t *testing.T, window int) {
	t.Helper()
	if _, err := a.request("idle", map[string]any{"window": window, "timeout": 500}); err != nil && !strings.Contains(err.Error(), "timeout") {
		t.Fatal(err)
	}
}

// rightClickAt 像真实指针那样先移动、等悬停画完，再在同一点右键。
func (a *desktopAutomation) rightClickAt(t *testing.T, window int, x, y float64) {
	t.Helper()
	a.sendInput(t, window, map[string]any{"type": "pointerMove", "x": x, "y": y})
	a.settle(t, window)
	a.sendInput(t, window, map[string]any{"type": "click", "x": x, "y": y, "button": "right"})
}

// rightClickNode 在 matches 选中的控件中心右键。
func (a *desktopAutomation) rightClickNode(t *testing.T, window int, matches func(desktopAutomationNode) bool) {
	t.Helper()
	node := a.waitSettledNode(t, window, matches)
	a.rightClickAt(t, window, node.Bounds.X+node.Bounds.Width/2, node.Bounds.Y+node.Bounds.Height/2)
}

// openTitleMenu 点开对话标题栏右侧的「对话操作」菜单；侧栏行也有同名按钮，标题栏的在最右边。
func (a *desktopAutomation) openTitleMenu(t *testing.T, window int) {
	t.Helper()
	deadline := time.Now().Add(e2eTimeout)
	for {
		var title *desktopAutomationNode
		nodes := a.getNodes(t, window)
		for index := range nodes {
			if nodes[index].Role == "button" && nodes[index].Label == "对话操作" && (title == nil || nodes[index].Bounds.X > title.Bounds.X) {
				title = &nodes[index]
			}
		}
		if title != nil {
			a.clickNode(t, window, *title, "")
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("title conversation menu button missing")
		}
		time.Sleep(40 * time.Millisecond)
	}
}

// clickMenuItem 点击已打开的主题菜单里的条目。
func (a *desktopAutomation) clickMenuItem(t *testing.T, window int, label string) {
	t.Helper()
	item := a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "menuItem" && n.Label == label })
	a.clickNode(t, window, item, "")
}

// dismissToast 按下指定标题的通知上的「关闭」按钮，等通知真正消失。
func (a *desktopAutomation) dismissToast(t *testing.T, window int, title string) {
	t.Helper()
	toast := a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "status" && strings.HasPrefix(n.Label, title) })
	closeButton := a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "button" && n.Label == "关闭" && n.Parent == toast.ID
	})
	a.call(t, "act", map[string]any{"window": window, "id": closeButton.ID, "action": "press"}, nil)
	deadline := time.Now().Add(e2eTimeout)
	for slices.ContainsFunc(a.getNodes(t, window), func(n desktopAutomationNode) bool { return n.Role == "status" && n.ID == toast.ID }) {
		if time.Now().After(deadline) {
			t.Fatalf("toast %q did not close", title)
		}
		time.Sleep(40 * time.Millisecond)
	}
}

// sendDesktopMessage 在输入框写入文字并点发送。
func (a *desktopAutomation) sendDesktopMessage(t *testing.T, window int, text string) {
	t.Helper()
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "popUpButton" && fmt.Sprint(n.Key) == "model" && !slices.Contains(n.States, "disabled")
	})
	a.setText(t, window, "composer-text", text)
	a.press(t, window, "发送")
}

// TestDesktopGUIConversationActions 检查对话菜单（⋯ 与右键）、Enter 提交的重命名弹窗、归档通知和撤销，以及后端拒绝时的中文失败通知。
func TestDesktopGUIConversationActions(t *testing.T) {
	if runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		t.Skip("run with xvfb-run or a graphical session")
	}
	binary, err := buildDesktopOnce()
	if err != nil {
		t.Fatal(err)
	}
	model := newE2EModel(t)
	config := writeDesktopE2EConfig(t, model.server.URL)
	_, a, window := startDesktopGUI(t, binary, config)
	// 1. 发一条消息得到真实对话。
	a.sendDesktopMessage(t, window, "menu actions")
	model.next(t).reply <- e2eModelReply{text: "menu actions reply"}
	a.waitNode(t, window, func(n desktopAutomationNode) bool { return strings.Contains(n.Label, "menu actions reply") })
	client, stop := newE2ERuntimeClient(t, config)
	defer stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range client.Events() {
		}
	}()
	defer func() { stop(); <-done }()
	list, err := client.ListConversations(t.Context(), &runtimeclient.ListConversationsRequest{})
	if err != nil || len(list.Conversations) != 1 {
		t.Fatalf("conversations = %+v, %v", list, err)
	}
	conversation := list.Conversations[0].Conversation
	// 2. 侧栏行右键和标题栏 ⋯ 打开主题菜单；导出只在当前对话的标题栏菜单里。
	for _, tt := range []struct {
		name   string
		open   func()
		want   []string
		absent []string
	}{
		{"sidebar right click", func() {
			a.rightClickNode(t, window, func(n desktopAutomationNode) bool {
				return n.Role == "button" && fmt.Sprint(n.Key) == string(conversation.ID)
			})
		}, []string{"重命名", "分叉", "归档", "删除"}, []string{"导出 Markdown"}},
		{"title menu", func() { a.openTitleMenu(t, window) }, []string{"重命名", "分叉", "导出 Markdown", "归档", "删除"}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.open()
			for _, label := range tt.want {
				a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "menuItem" && n.Label == label })
			}
			for _, node := range a.getNodes(t, window) {
				if node.Role == "menuItem" && slices.Contains(tt.absent, node.Label) {
					t.Fatalf("unexpected menu item %q", node.Label)
				}
			}
			a.capture(t, window, "menu-"+strings.ReplaceAll(tt.name, " ", "-"))
			a.sendInput(t, window, map[string]any{"type": "key", "key": "Escape"})
		})
	}
	// 3. 重命名弹窗里按 Enter 提交，标题和后端同时更新，并出现成功通知。
	a.openTitleMenu(t, window)
	a.clickMenuItem(t, window, "重命名")
	a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "dialog" && n.Label == "重命名对话" })
	a.setText(t, window, "rename", "renamed with enter")
	a.sendInput(t, window, map[string]any{"type": "key", "key": "Enter"})
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "status" && strings.HasPrefix(n.Label, "已重命名")
	})
	a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "text" && n.Label == "renamed with enter" })
	// 4. 归档的通知带「撤销」，撤销后对话回到普通列表。
	a.dismissToast(t, window, "已重命名")
	a.openTitleMenu(t, window)
	a.clickMenuItem(t, window, "归档")
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "status" && strings.HasPrefix(n.Label, "已归档")
	})
	a.press(t, window, "撤销")
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "status" && strings.HasPrefix(n.Label, "已恢复")
	})
	archived, err := client.ListConversations(t.Context(), &runtimeclient.ListConversationsRequest{Archived: true})
	if err != nil || len(archived.Conversations) != 0 {
		t.Fatalf("undo left the conversation archived: %+v, %v", archived, err)
	}
	// 5. 别的客户端删掉这个对话后，在界面里归档它，失败通知用中文说明，原文在详情里。
	a.dismissToast(t, window, "已恢复")
	if err := client.DeleteConversation(t.Context(), &runtimeclient.DeleteConversationRequest{ConversationID: conversation.ID}); err != nil {
		t.Fatal(err)
	}
	a.openTitleMenu(t, window)
	a.clickMenuItem(t, window, "归档")
	failure := a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "status" && strings.HasPrefix(n.Label, "对话不存在，可能已在别处被删除")
	})
	a.capture(t, window, "failure-toast")
	if !strings.Contains(failure.Label, "not found") {
		t.Fatalf("failure toast detail is not the raw error: %q", failure.Label)
	}
}

// TestDesktopGUIAlignment 检查对话列右侧操作图标的右缘在一条线上，以及设置页文字按钮和上一行文字同在对齐线上。
func TestDesktopGUIAlignment(t *testing.T) {
	if runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		t.Skip("run with xvfb-run or a graphical session")
	}
	binary, err := buildDesktopOnce()
	if err != nil {
		t.Fatal(err)
	}
	model := newE2EModel(t)
	config := writeDesktopE2EConfig(t, model.server.URL)
	_, a, window := startDesktopGUI(t, binary, config)
	// 1. 一轮含工具调用和最终回复的对话。
	a.sendDesktopMessage(t, window, "alignment check")
	model.next(t).reply <- e2eModelReply{tool: "bash", arguments: `{"command":"echo aligned && true"}`}
	model.next(t).reply <- e2eModelReply{text: "alignment final reply"}
	a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "button" && n.Label == "复制结果" })
	a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "button" && n.Label == "从这里分叉" })
	a.capture(t, window, "alignment")
	// 2. 标题栏 ⋯、活动行调试按钮、用户消息分叉按钮、最终回复脚注按钮：右缘差不超过 1px。
	rightEdges := map[string][]float64{}
	var titleMenu *desktopAutomationNode
	nodes := a.getNodes(t, window)
	for index, node := range nodes {
		if node.Role != "button" || node.Bounds.X < 500 {
			continue
		}
		switch node.Label {
		case "对话操作":
			if titleMenu == nil || node.Bounds.X > titleMenu.Bounds.X {
				titleMenu = &nodes[index]
			}
		case "调试信息", "从这里分叉":
			rightEdges[node.Label] = append(rightEdges[node.Label], node.Bounds.X+node.Bounds.Width)
		}
	}
	if titleMenu == nil {
		t.Fatal("title menu button missing")
	}
	reference := titleMenu.Bounds.X + titleMenu.Bounds.Width
	// 最终回复脚注的两个 20 见方按钮是「复制结果」「调试信息」，「调试信息」在最右；用户消息卡片的最右按钮是「从这里分叉」。
	for _, label := range []string{"调试信息", "从这里分叉"} {
		if len(rightEdges[label]) < 1 {
			t.Fatalf("no %q button in the conversation column: %v", label, rightEdges)
		}
	}
	for label, edges := range rightEdges {
		for _, edge := range edges {
			if math.Abs(edge-reference) > 1 {
				t.Errorf("%q right edge %.1f differs from the title menu edge %.1f by more than 1px: %v", label, edge, reference, rightEdges)
			}
		}
	}
	// 3. 设置页「通用」：各设置行的标题文字在同一条左对齐线上，右侧控件（分段、下拉、开关、按钮）右缘在同一条线上。
	if _, err := a.request("menu", map[string]any{"window": window, "path": []string{"转到", "设置"}}); err != nil {
		t.Fatal(err)
	}
	a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "button" && n.Label == "恢复默认外观" })
	var titleXs []float64
	for _, label := range []string{"外观", "配色", "减少动画", "恢复默认外观"} {
		row := a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "text" && n.Label == label && n.Bounds.X > 250 })
		titleXs = append(titleXs, row.Bounds.X)
	}
	controlEdges := []float64{}
	for _, find := range []func(n desktopAutomationNode) bool{
		func(n desktopAutomationNode) bool { return n.Role == "radioGroup" && fmt.Sprint(n.Key) == "appearance" },
		func(n desktopAutomationNode) bool { return n.Role == "popUpButton" && fmt.Sprint(n.Key) == "palette" },
		func(n desktopAutomationNode) bool { return n.Role == "switch" && fmt.Sprint(n.Key) == "reduce-motion" },
		func(n desktopAutomationNode) bool { return n.Role == "button" && n.Label == "恢复默认外观" },
	} {
		node := a.waitNode(t, window, find)
		controlEdges = append(controlEdges, node.Bounds.X+node.Bounds.Width)
	}
	for index := 1; index < len(titleXs); index++ {
		if math.Abs(titleXs[index]-titleXs[0]) > 1 || math.Abs(controlEdges[index]-controlEdges[0]) > 1 {
			t.Fatalf("settings rows are not aligned: title x %v, control right edges %v", titleXs, controlEdges)
		}
	}
	a.capture(t, window, "settings-general")
}

// writeDesktopWorkLocationFixtures 在配置目录写两个技能、一个 sandbox Executor 和一个启动失败的 MCP server，
// 返回 sandbox 的根目录；根目录下有 projects/alpha、projects/alpha-two、projects/beta 三个子目录。
func writeDesktopWorkLocationFixtures(t *testing.T, config string) string {
	t.Helper()
	// 1. sandbox 的根目录和它下面的项目目录。
	home := filepath.Dir(config)
	sandbox := t.TempDir()
	for _, directory := range []string{"alpha", "alpha-two", "beta"} {
		if err := os.MkdirAll(filepath.Join(sandbox, "projects", directory), 0700); err != nil {
			t.Fatal(err)
		}
	}
	// 2. 技能、Executor 和 MCP 配置文件。
	files := map[string]string{
		"skills/display-check/SKILL.md": "---\nname: display-check\ndescription: 检查消息中的技能展开选择\n---\n# 技能展示检查\n",
		"skills/release-notes/SKILL.md": "---\nname: release-notes\ndescription: Draft release notes from a list of changes\n---\n# Release notes\n",
		"executors/sandbox.yaml":        fmt.Sprintf("id: sandbox\nkind: local\ndescription: 第二个本地执行器\nworkspace_directory: %q\n", sandbox),
		"mcp_servers/broken.yaml":       "id: broken\ntype: stdio\ncommand: /nonexistent\n",
	}
	for name, content := range files {
		path := filepath.Join(home, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return sandbox
}

// writeDesktopWindowPreferences 给本测试的桌面写窗口尺寸偏好；窄窗口时侧栏和右 Dock 保存为关闭，免得启动时遮住内容。
func writeDesktopWindowPreferences(t *testing.T, config string, width, height int, docksOpen bool) {
	t.Helper()
	appData := filepath.Join(filepath.Dir(config), ".config", "OpenVaala")
	if runtime.GOOS == "darwin" {
		appData = filepath.Join(filepath.Dir(config), "Library", "Application Support", "OpenVaala")
	}
	if err := os.MkdirAll(appData, 0700); err != nil {
		t.Fatal(err)
	}
	workspace := `"sidebar_width":240`
	if !docksOpen {
		workspace += `,"sidebar_open":false,"right":{"open":false,"active_panel":"agent"}`
	}
	preferences := fmt.Sprintf(`{"width":%d,"height":%d,"workspace":{%s}}`, width, height, workspace)
	if err := os.WriteFile(filepath.Join(appData, "preferences.json"), []byte(preferences), 0600); err != nil {
		t.Fatal(err)
	}
}

// findNodeByKey 返回当前树里指定角色和键的控件；没有时 found 为假。
func (a *desktopAutomation) findNodeByKey(t *testing.T, window int, role, key string) (desktopAutomationNode, bool) {
	t.Helper()
	for _, node := range a.getNodes(t, window) {
		if node.Role == role && fmt.Sprint(node.Key) == key {
			return node, true
		}
	}
	return desktopAutomationNode{}, false
}

// focusTextField 给指定键的输入框键盘焦点，再往里打字或按键都像真实键盘。
func (a *desktopAutomation) focusTextField(t *testing.T, window int, key string) {
	t.Helper()
	node := a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "textField" && fmt.Sprint(n.Key) == key })
	a.call(t, "act", map[string]any{"window": window, "id": node.ID, "action": "focus"}, nil)
	// 树的 focus 字段是当前有键盘焦点的控件 ID。
	deadline := time.Now().Add(e2eTimeout)
	for {
		var tree struct {
			Focus string `json:"focus"`
		}
		a.call(t, "tree", map[string]any{"window": window}, &tree)
		if tree.Focus == node.ID {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("text field %s did not take the keyboard focus (focus=%s)", key, tree.Focus)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

// typeText 把文字输入给当前焦点控件。
func (a *desktopAutomation) typeText(t *testing.T, window int, text string) {
	t.Helper()
	a.sendInput(t, window, map[string]any{"type": "text", "text": text})
}

// pressKey 按一个键，写法同 desktopdrive 的 key，例如 Enter、Tab、ctrl+Enter。
func (a *desktopAutomation) pressKey(t *testing.T, window int, key string) {
	t.Helper()
	a.sendInput(t, window, map[string]any{"type": "key", "key": key})
}

// countPopups 返回当前树里的浮层数；表单浮层里再开下拉时，下拉是最后一个浮层。
func (a *desktopAutomation) countPopups(t *testing.T, window int) int {
	t.Helper()
	count := 0
	for _, node := range a.getNodes(t, window) {
		if node.Role == "popup" {
			count++
		}
	}
	return count
}

// waitTopmostPopup 等到浮层数达到 want，返回树里最后一个浮层。
func (a *desktopAutomation) waitTopmostPopup(t *testing.T, window, want int) desktopAutomationNode {
	t.Helper()
	deadline := time.Now().Add(e2eTimeout)
	for {
		var popup *desktopAutomationNode
		count := 0
		nodes := a.getNodes(t, window)
		for index := range nodes {
			if nodes[index].Role == "popup" {
				popup, count = &nodes[index], count+1
			}
		}
		if popup != nil && count >= want {
			return *popup
		}
		if time.Now().After(deadline) {
			t.Fatalf("popup did not open: %d popups, want %d", count, want)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

// listSelectOptions 打开指定键的下拉，返回选项文字（不含搜索框），再按 Esc 关闭。
func (a *desktopAutomation) listSelectOptions(t *testing.T, window int, key string) []string {
	t.Helper()
	node := a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "popUpButton" && fmt.Sprint(n.Key) == key && !slices.Contains(n.States, "disabled")
	})
	popupsBefore := a.countPopups(t, window)
	a.call(t, "act", map[string]any{"window": window, "id": node.ID, "action": "press"}, nil)
	popup := a.waitTopmostPopup(t, window, popupsBefore+1)
	labels := []string{}
	for _, candidate := range a.getNodes(t, window) {
		if candidate.Parent == popup.ID && candidate.Label != "" && candidate.Role != "textField" {
			labels = append(labels, candidate.Label)
		}
	}
	a.pressKey(t, window, "Escape")
	deadline := time.Now().Add(e2eTimeout)
	for a.countPopups(t, window) > popupsBefore {
		if time.Now().After(deadline) {
			t.Fatalf("select %s popup did not close", key)
		}
		time.Sleep(30 * time.Millisecond)
	}
	return labels
}

// countRPCRequests 打开 RPC 面板并按方法筛选，返回该方法已发出的请求数。
func (a *desktopAutomation) countRPCRequests(t *testing.T, window int, method string) int {
	t.Helper()
	a.press(t, window, "RPC")
	a.setText(t, window, "rpc-method", method)
	a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "button" && n.Label == "暂停" })
	if _, err := a.request("idle", map[string]any{"window": window, "timeout": 500}); err != nil && !strings.Contains(err.Error(), "timeout") {
		t.Fatal(err)
	}
	count := 0
	for _, node := range a.getNodes(t, window) {
		if node.Role == "button" && strings.Contains(node.Label, " sent "+method+" ") {
			count++
		}
	}
	return count
}

// TestDesktopGUISlashCompletion 检查输入 / 时技能补全浮层：每项有名称和描述，无匹配有提示，打开时输入区布局不变，同一来源只请求一次 skill/list。
func TestDesktopGUISlashCompletion(t *testing.T) {
	if runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		t.Skip("run with xvfb-run or a graphical session")
	}
	binary, err := buildDesktopOnce()
	if err != nil {
		t.Fatal(err)
	}
	model := newE2EModel(t)
	config := writeDesktopE2EConfig(t, model.server.URL)
	writeDesktopWorkLocationFixtures(t, config)
	_, a, window := startDesktopGUI(t, binary, config)
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "popUpButton" && fmt.Sprint(n.Key) == "model" && !slices.Contains(n.States, "disabled")
	})
	// 1. 记下打开浮层之前输入框和发送按钮的位置；占位符是新文案。
	input, _ := a.findNodeByKey(t, window, "textField", "composer-text")
	if input.Description != "" && strings.Contains(input.Description, "skill") {
		t.Fatalf("old placeholder is still shown: %+v", input)
	}
	send := a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "button" && n.Label == "发送" })
	// 2. 输入 /：技能名和描述都显示，位置不变。
	a.focusTextField(t, window, "composer-text")
	a.typeText(t, window, "/")
	a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "listItem" && n.Label == "/display-check" })
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "text" && n.Label == "检查消息中的技能展开选择"
	})
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "text" && n.Label == "Draft release notes from a list of changes"
	})
	a.capture(t, window, "slash-menu")
	afterInput, _ := a.findNodeByKey(t, window, "textField", "composer-text")
	afterSend := a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "button" && n.Label == "发送" })
	if afterInput.Bounds != input.Bounds || afterSend.Bounds != send.Bounds {
		t.Fatalf("composer moved when the menu opened: input %+v -> %+v, send %+v -> %+v", input.Bounds, afterInput.Bounds, send.Bounds, afterSend.Bounds)
	}
	// 3. 继续输入缩小范围，再输入到没有匹配：显示「没有匹配的 Skill」。
	a.typeText(t, window, "rel")
	a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "listItem" && n.Label == "/release-notes" })
	if slices.ContainsFunc(a.getNodes(t, window), func(n desktopAutomationNode) bool { return n.Role == "listItem" && n.Label == "/display-check" }) {
		t.Fatal("prefix did not filter the skills")
	}
	a.typeText(t, window, "zzz")
	a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "text" && n.Label == "没有匹配的 Skill" })
	a.capture(t, window, "slash-no-match")
	// 4. Esc 关闭浮层；路径里的斜杠不触发；Tab 补全后光标词不再以 / 开头。
	a.pressKey(t, window, "Escape")
	a.setText(t, window, "composer-text", "")
	a.focusTextField(t, window, "composer-text")
	a.typeText(t, window, "see a/b")
	if slices.ContainsFunc(a.getNodes(t, window), func(n desktopAutomationNode) bool { return n.Role == "list" && fmt.Sprint(n.Key) == "kit-completion" }) {
		t.Fatal("path text opened the skill menu")
	}
	a.setText(t, window, "composer-text", "")
	a.focusTextField(t, window, "composer-text")
	a.typeText(t, window, "/disp")
	a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "listItem" && n.Label == "/display-check" })
	a.pressKey(t, window, "Tab")
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "textField" && fmt.Sprint(n.Key) == "composer-text" && n.Value == "/display-check "
	})
	// 5. 这些输入加起来只请求了一次 skill/list。
	if count := a.countRPCRequests(t, window, "skill/list"); count != 1 {
		t.Fatalf("skill/list requests = %d, want 1", count)
	}
}

// TestDesktopGUIWorkLocation 检查新对话的工作位置表单（带标签的 Agent、Executor、目录，目录边输边给建议）和已有对话的设置弹窗。
func TestDesktopGUIWorkLocation(t *testing.T) {
	if runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		t.Skip("run with xvfb-run or a graphical session")
	}
	binary, err := buildDesktopOnce()
	if err != nil {
		t.Fatal(err)
	}
	model := newE2EModel(t)
	config := writeDesktopE2EConfig(t, model.server.URL)
	sandbox := writeDesktopWorkLocationFixtures(t, config)
	_, a, window := startDesktopGUI(t, binary, config)
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "popUpButton" && fmt.Sprint(n.Key) == "model" && !slices.Contains(n.States, "disabled")
	})
	// 1. 工作位置按钮打开表单：三个字段都有标签，没有「候选」「解析」，Executor 里没有内置 openvaala。
	a.press(t, window, "工作位置")
	for _, label := range []string{"Agent", "Executor", "目录"} {
		a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "text" && n.Label == label })
	}
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "popUpButton" && fmt.Sprint(n.Key) == "new-agent" && n.Value == "default"
	})
	for _, node := range a.getNodes(t, window) {
		if node.Role == "button" && (node.Label == "候选" || node.Label == "解析") {
			t.Fatalf("old button %q is still shown", node.Label)
		}
	}
	if options := a.listSelectOptions(t, window, "new-executor"); !slices.Equal(options, []string{"local", "sandbox"}) {
		t.Fatalf("executor options = %v, want [local sandbox]", options)
	}
	a.selectOption(t, window, "new-executor", "sandbox", "sandbox")
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "text" && n.Label == "第二个本地执行器"
	})
	// 2. 表单在输入区上方：目录输入框的下缘不低于消息输入框的上缘。
	directory := a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "textField" && fmt.Sprint(n.Key) == "new-directory"
	})
	composerInput, _ := a.findNodeByKey(t, window, "textField", "composer-text")
	if directory.Bounds.Y+directory.Bounds.Height > composerInput.Bounds.Y {
		t.Fatalf("form overlaps the composer: directory %+v, composer input %+v", directory.Bounds, composerInput.Bounds)
	}
	// 3. 目录边输入边给建议：输到 projects/al 列出 alpha/ 和 alpha-two/，不含 beta/；Enter 选中后进入下一层。
	a.focusTextField(t, window, "new-directory")
	// 指针停在侧栏空白处：浮层出现在指针下面时，悬停会抢走键盘的高亮项。
	a.sendInput(t, window, map[string]any{"type": "pointerMove", "x": 100, "y": 400})
	a.typeText(t, window, sandbox+"/projects/al")
	a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "listItem" && n.Label == "alpha/" })
	a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "listItem" && n.Label == "alpha-two/" })
	if slices.ContainsFunc(a.getNodes(t, window), func(n desktopAutomationNode) bool { return n.Role == "listItem" && n.Label == "beta/" }) {
		t.Fatal("suggestions were not filtered by the typed prefix")
	}
	a.capture(t, window, "new-conversation-location")
	a.pressKey(t, window, "Enter")
	alpha := filepath.Join(sandbox, "projects", "alpha")
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "textField" && fmt.Sprint(n.Key) == "new-directory" && n.Value == alpha+"/"
	})
	// 失焦时目录被解析成绝对路径（去掉末尾斜杠）。
	a.pressKey(t, window, "Tab")
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "textField" && fmt.Sprint(n.Key) == "new-directory" && n.Value == alpha
	})
	// 4. 创建对话后，标题栏、工具栏和侧栏都显示 Executor 和目录名。
	a.sendDesktopMessage(t, window, "work location create")
	model.next(t).reply <- e2eModelReply{text: "work location reply"}
	a.waitNode(t, window, func(n desktopAutomationNode) bool { return strings.Contains(n.Label, "work location reply") })
	titleButton := a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "button" && fmt.Sprint(n.Key) == "title-work-location"
	})
	// 标题栏按钮的悬停说明是完整的 Executor 和目录；侧栏行的第二行是「目录名 · 相对时间」。
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.ID == titleButton.ID && n.Description == "sandbox · "+alpha
	})
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "button" && n.Label == "work location create" && strings.HasPrefix(n.Description, "alpha · ")
	})
	a.capture(t, window, "new-conversation-created")
	client, stop := newE2ERuntimeClient(t, config)
	defer stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range client.Events() {
		}
	}()
	defer func() { stop(); <-done }()
	list, err := client.ListConversations(t.Context(), &runtimeclient.ListConversationsRequest{})
	if err != nil || len(list.Conversations) != 1 {
		t.Fatalf("conversations = %+v, %v", list, err)
	}
	settings := list.Conversations[0].Conversation.ExecutorSettings
	if settings.DefaultExecutorID != "sandbox" || settings.WorkspaceDirectories["sandbox"] != alpha {
		t.Fatalf("executor settings = %+v, want sandbox in %s", settings, alpha)
	}
	// 5. 已有对话的弹窗：Agent 只读，没有 openvaala；MCP 页显示失败状态和原因；Skill 页显示描述。
	a.press(t, window, "工作位置")
	dialog := a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "dialog" && n.Label == "工作位置" })
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "textField" && fmt.Sprint(n.Key) == "location-agent" && n.Value == "default"
	})
	for _, node := range a.getNodes(t, window) {
		if strings.Contains(node.Label, "openvaala") {
			t.Fatalf("builtin executor is shown in the dialog: %+v", node)
		}
	}
	a.press(t, window, "MCP")
	// 失败的服务一行，悬停说明是失败原因（状态点和状态文字在截图里核对）。
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Label == "broken" && strings.Contains(n.Description, "/nonexistent")
	})
	a.capture(t, window, "location-dialog-mcp")
	a.press(t, window, "Skill")
	a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "button" && n.Label == "display-check" })
	a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "button" && n.Label == "release-notes" })
	a.capture(t, window, "location-dialog-skills")
	// 6. 回到工作位置页把默认 Executor 改回 local 并保存：设置生效，标题栏随之更新。
	tab := a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "radio" && n.Label == "工作位置" })
	a.call(t, "act", map[string]any{"window": window, "id": tab.ID, "action": "press"}, nil)
	radio := a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "radio" && n.Label == "local" })
	a.call(t, "act", map[string]any{"window": window, "id": radio.ID, "action": "press"}, nil)
	a.press(t, window, "保存")
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "status" && strings.HasPrefix(n.Label, "工作位置已保存")
	})
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.ID == titleButton.ID && strings.HasPrefix(n.Description, "local · ")
	})
	for slices.ContainsFunc(a.getNodes(t, window), func(n desktopAutomationNode) bool { return n.ID == dialog.ID }) {
		time.Sleep(30 * time.Millisecond)
	}
	list, err = client.ListConversations(t.Context(), &runtimeclient.ListConversationsRequest{})
	if err != nil || len(list.Conversations) != 1 || list.Conversations[0].Conversation.ExecutorSettings.DefaultExecutorID != "local" {
		t.Fatalf("saved settings = %+v, %v", list, err)
	}
}

// TestDesktopGUIComposerToolbar 检查输入区工具栏在窄窗口里：控件互不重叠、不超出窗口；输入区窄于 480 时推理强度和工作位置收进「更多」。
func TestDesktopGUIComposerToolbar(t *testing.T) {
	if runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		t.Skip("run with xvfb-run or a graphical session")
	}
	binary, err := buildDesktopOnce()
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name          string
		width, height int
		docksOpen     bool
		folded        bool
	}{
		// 640 宽：侧栏和右 Dock 是抽屉，输入区 640 宽，不折叠；1000 宽：两侧 Dock 展开，输入区 400 宽，折叠。
		{"window_640", 640, 620, false, false},
		{"window_1000_docks_open", 1000, 700, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			model := newE2EModel(t)
			config := writeDesktopE2EConfig(t, model.server.URL)
			writeDesktopWindowPreferences(t, config, tt.width, tt.height, tt.docksOpen)
			_, a, window := startDesktopGUI(t, binary, config)
			a.waitNode(t, window, func(n desktopAutomationNode) bool {
				return n.Role == "popUpButton" && fmt.Sprint(n.Key) == "model" && !slices.Contains(n.States, "disabled")
			})
			// 选带推理档位的模型，才有推理强度可显示。
			if options := a.listSelectOptions(t, window, "model"); !slices.Equal(options, []string{"Agent 默认", "local", "default", "Second"}) {
				t.Fatalf("model options = %v", options)
			}
			a.selectOption(t, window, "model", "Second", "local/second")
			a.waitNode(t, window, func(n desktopAutomationNode) bool {
				return n.Role == "popUpButton" && fmt.Sprint(n.Key) == "model" && n.Value == "local/second"
			})
			if _, err := a.request("idle", map[string]any{"window": window, "timeout": 500}); err != nil && !strings.Contains(err.Error(), "timeout") {
				t.Fatal(err)
			}
			a.capture(t, window, "toolbar")
			// 工具栏上的控件：左侧添加图片，右侧模型、推理强度或「更多」、工作位置、发送。
			var controls []desktopAutomationNode
			for _, node := range a.getNodes(t, window) {
				switch {
				case node.Role == "button" && (node.Label == "添加图片" || node.Label == "发送" || node.Label == "更多"):
				case node.Role == "popUpButton" && (fmt.Sprint(node.Key) == "model" || fmt.Sprint(node.Key) == "reasoning"):
				case node.Role == "button" && fmt.Sprint(node.Key) == "work-location":
				case node.Role == "menuButton" && node.Label == "更多":
				default:
					continue
				}
				controls = append(controls, node)
			}
			if tt.folded {
				if slices.ContainsFunc(controls, func(n desktopAutomationNode) bool {
					return fmt.Sprint(n.Key) == "reasoning" || fmt.Sprint(n.Key) == "work-location"
				}) {
					t.Fatalf("reasoning or work location is still shown beside the model: %+v", controls)
				}
				if !slices.ContainsFunc(controls, func(n desktopAutomationNode) bool { return n.Label == "更多" }) {
					t.Fatalf("the more menu is missing: %+v", controls)
				}
			} else if !slices.ContainsFunc(controls, func(n desktopAutomationNode) bool { return fmt.Sprint(n.Key) == "reasoning" }) ||
				!slices.ContainsFunc(controls, func(n desktopAutomationNode) bool { return fmt.Sprint(n.Key) == "work-location" }) {
				t.Fatalf("reasoning or work location is missing: %+v", controls)
			}
			// 控件互不重叠，也都在窗口内（输入区铺满中间区，最右边是中间区右缘）。
			slices.SortFunc(controls, func(x, y desktopAutomationNode) int { return int(x.Bounds.X - y.Bounds.X) })
			for index, node := range controls {
				if node.Bounds.X < 0 || node.Bounds.X+node.Bounds.Width > float64(tt.width) {
					t.Errorf("%q (%+v) is outside the window", node.Label, node.Bounds)
				}
				if index > 0 {
					previous := controls[index-1]
					if node.Bounds.X < previous.Bounds.X+previous.Bounds.Width {
						t.Errorf("%q %+v overlaps %q %+v", node.Label, node.Bounds, previous.Label, previous.Bounds)
					}
				}
			}
			// 折叠时「更多」里有推理强度和工作位置。
			if tt.folded {
				more := a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Label == "更多" && n.Role != "text" })
				a.clickNode(t, window, more, "")
				for _, label := range []string{"推理：自动", "推理：低", "推理：高"} {
					a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "menuItem" && n.Label == label })
				}
				a.capture(t, window, "toolbar-more-menu")
			}
		})
	}
}

// desktopQuitMenuPath 返回中文原生系统菜单中的应用退出项。
func desktopQuitMenuPath() []string {
	// 1. macOS 的应用角色把应用名拼入退出标签。
	label := "退出"
	if runtime.GOOS == "darwin" {
		label += "OpenVaala"
	}
	return []string{"OpenVaala", label}
}

// getSidebarRow 返回侧栏里标题为 title 的对话行按钮；没有时返回 false。
func (a *desktopAutomation) getSidebarRow(t *testing.T, window int, title string) (desktopAutomationNode, bool) {
	t.Helper()
	for _, node := range a.getNodes(t, window) {
		if node.Role == "button" && node.Label == title && node.Bounds.X < 240 && fmt.Sprint(node.Key) != "title-rename" {
			return node, true
		}
	}
	return desktopAutomationNode{}, false
}

// waitNodeAbsent 等待树里不再有满足条件的节点。
func (a *desktopAutomation) waitNodeAbsent(t *testing.T, window int, matches func(desktopAutomationNode) bool) {
	t.Helper()
	deadline := time.Now().Add(e2eTimeout)
	for slices.ContainsFunc(a.getNodes(t, window), matches) {
		if time.Now().After(deadline) {
			t.Fatal("native node is still present")
		}
		time.Sleep(40 * time.Millisecond)
	}
}

// waitSidebarRow 等待侧栏里出现（present 为真）或消失标题为 title 的对话行。
func (a *desktopAutomation) waitSidebarRow(t *testing.T, window int, title string, present bool) {
	t.Helper()
	deadline := time.Now().Add(e2eTimeout)
	for {
		if _, found := a.getSidebarRow(t, window, title); found == present {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("sidebar row %q present=%v did not settle", title, present)
		}
		time.Sleep(40 * time.Millisecond)
	}
}

// openSidebarRowMenu 右键点击侧栏里标题为 title 的对话行，等右键菜单出现；位置在列表刷新画完之后读取，不重复点击。
func (a *desktopAutomation) openSidebarRowMenu(t *testing.T, window int, title, firstItem string) {
	t.Helper()
	a.rightClickNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "button" && n.Label == title && n.Bounds.X < 240 && fmt.Sprint(n.Key) != "title-rename"
	})
	a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "menuItem" && n.Label == firstItem })
}

// pressSidebarView 点击侧栏标题栏的归档开关：label 是开关此刻的名称（「显示归档」或「返回对话」）。
func (a *desktopAutomation) pressSidebarView(t *testing.T, window int, label string) {
	t.Helper()
	a.press(t, window, label)
}

// waitFocusedKey 等待键盘焦点落在指定键的控件上；树的 focus 字段是当前有键盘焦点的控件 ID。
func (a *desktopAutomation) waitFocusedKey(t *testing.T, window int, key string) {
	t.Helper()
	deadline := time.Now().Add(e2eTimeout)
	for {
		var tree struct {
			Focus string                  `json:"focus"`
			Nodes []desktopAutomationNode `json:"nodes"`
		}
		a.call(t, "tree", map[string]any{"window": window}, &tree)
		for _, node := range tree.Nodes {
			if fmt.Sprint(node.Key) == key && node.ID == tree.Focus {
				return
			}
		}
		if time.Now().After(deadline) {
			focused := "none"
			for _, node := range tree.Nodes {
				if node.ID == tree.Focus {
					focused = fmt.Sprintf("%s %q key=%v", node.Role, node.Label, node.Key)
				}
			}
			t.Fatalf("keyboard focus did not reach %q (focus=%s %s)", key, tree.Focus, focused)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

// waitConversationTitle 等待标题栏的标题按钮显示 title。
func (a *desktopAutomation) waitConversationTitle(t *testing.T, window int, title string) desktopAutomationNode {
	t.Helper()
	return a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "button" && fmt.Sprint(n.Key) == "title-rename" && n.Label == title
	})
}

// TestDesktopGUIConversationList 检查对话列表：归档后在侧栏「归档」里恢复；标题栏点击改名（Enter 保存、Esc 取消、失焦保存）；搜索的清除按钮、Esc 清空以及上下键加 Enter 打开。
func TestDesktopGUIConversationList(t *testing.T) {
	if runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		t.Skip("run with xvfb-run or a graphical session")
	}
	binary, err := buildDesktopOnce()
	if err != nil {
		t.Fatal(err)
	}
	model := newE2EModel(t)
	config := writeDesktopE2EConfig(t, model.server.URL)
	_, a, window := startDesktopGUI(t, binary, config)
	// 1. 两个有标题的对话：标题取第一条用户消息。
	for _, name := range []string{"list alpha", "list beta"} {
		if name == "list beta" {
			a.press(t, window, "新对话")
		}
		a.sendDesktopMessage(t, window, name)
		model.next(t).reply <- e2eModelReply{text: name + " reply"}
		a.waitNode(t, window, func(n desktopAutomationNode) bool { return strings.Contains(n.Label, name+" reply") })
		a.waitSidebarRow(t, window, name, true)
	}
	// 2. 归档 alpha：从侧栏对话列表消失，切到「归档」后出现在那里，且不显示 beta。
	a.openSidebarRowMenu(t, window, "list alpha", "归档")
	a.clickMenuItem(t, window, "归档")
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "status" && strings.HasPrefix(n.Label, "已归档")
	})
	a.waitSidebarRow(t, window, "list alpha", false)
	a.dismissToast(t, window, "已归档")
	a.capture(t, window, "conversation-list")
	a.pressSidebarView(t, window, "显示归档")
	// 标题栏和其他面板一致：标题在 x=12，两个 28 见方的图标靠右、36 高。
	archivedTitle := a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "text" && n.Label == "归档" && n.Bounds.X < 100
	})
	toggle := a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "button" && n.Label == "返回对话" })
	plus := a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "button" && n.Label == "新对话" && n.Bounds.X < 300
	})
	if archivedTitle.Bounds.X != 12 || toggle.Bounds.Width != 28 || plus.Bounds.Width != 28 || plus.Bounds.X <= toggle.Bounds.X || toggle.Bounds.Y != plus.Bounds.Y {
		t.Fatalf("sidebar header layout: title=%+v toggle=%+v plus=%+v", archivedTitle.Bounds, toggle.Bounds, plus.Bounds)
	}
	a.waitSidebarRow(t, window, "list alpha", true)
	if _, found := a.getSidebarRow(t, window, "list beta"); found {
		t.Fatal("archived view lists an unarchived conversation")
	}
	a.capture(t, window, "archived-list")
	// 3. 在归档列表里恢复 alpha：归档列表回到空状态，对话列表里两个都在。
	a.openSidebarRowMenu(t, window, "list alpha", "恢复")
	a.clickMenuItem(t, window, "恢复")
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "status" && strings.HasPrefix(n.Label, "已恢复")
	})
	a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "text" && n.Label == "没有归档的对话" })
	a.capture(t, window, "archived-empty")
	a.dismissToast(t, window, "已恢复")
	a.pressSidebarView(t, window, "返回对话")
	a.waitSidebarRow(t, window, "list alpha", true)
	a.waitSidebarRow(t, window, "list beta", true)
	// 4. 标题栏改名：点标题进入编辑，Esc 取消不保存；Enter 保存并提示；失焦同样保存。
	a.press(t, window, "list alpha")
	editTitle := func(current, text string) {
		title := a.waitConversationTitle(t, window, current)
		a.clickNode(t, window, title, "")
		a.focusTextField(t, window, "title-edit")
		a.setText(t, window, "title-edit", text)
	}
	editTitle("list alpha", "discarded title")
	a.pressKey(t, window, "Escape")
	a.waitConversationTitle(t, window, "list alpha")
	a.waitFocusedKey(t, window, "composer-text")
	editTitle("list alpha", "inline renamed")
	a.pressKey(t, window, "Enter")
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "status" && strings.HasPrefix(n.Label, "已重命名")
	})
	a.waitConversationTitle(t, window, "inline renamed")
	a.waitSidebarRow(t, window, "inline renamed", true)
	a.dismissToast(t, window, "已重命名")
	editTitle("inline renamed", "blur saved")
	composer := a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "textField" && fmt.Sprint(n.Key) == "composer-text"
	})
	a.clickNode(t, window, composer, "")
	a.waitConversationTitle(t, window, "blur saved")
	a.waitSidebarRow(t, window, "blur saved", true)
	a.dismissToast(t, window, "已重命名")
	// 5. 搜索：输入后出现清除按钮、列表只剩匹配项；上下键移动高亮，Enter 打开；Esc 清空。
	a.waitConversationTitle(t, window, "blur saved")
	a.focusTextField(t, window, "conversation-search")
	a.typeText(t, window, "beta")
	a.waitSidebarRow(t, window, "blur saved", false)
	a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "button" && n.Label == "清除搜索" })
	a.pressKey(t, window, "Down")
	a.pressKey(t, window, "Enter")
	a.waitConversationTitle(t, window, "list beta")
	a.focusTextField(t, window, "conversation-search")
	a.pressKey(t, window, "Escape")
	a.waitSidebarRow(t, window, "blur saved", true)
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "textField" && fmt.Sprint(n.Key) == "conversation-search" && n.Value == ""
	})
	// 清除按钮：再输入一次，点它清空。
	a.typeText(t, window, "zzz")
	clearButton := a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "button" && n.Label == "清除搜索" })
	a.clickNode(t, window, clearButton, "")
	a.waitSidebarRow(t, window, "blur saved", true)
	a.capture(t, window, "search-cleared")
}

// TestDesktopGUIRightPanels 检查消息右键的主题菜单，以及右侧面板：上下文事实行距 28、原始数据默认收起，RPC 工具栏在 360 宽下输入框不被挤窄，
// Turn 行显示序号和输入摘要并跳到对应消息，调试面板空状态，打开设置时右 Dock 收起、离开后恢复。
func TestDesktopGUIRightPanels(t *testing.T) {
	if runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		t.Skip("run with xvfb-run or a graphical session")
	}
	binary, err := buildDesktopOnce()
	if err != nil {
		t.Fatal(err)
	}
	model := newE2EModel(t)
	config := writeDesktopE2EConfig(t, model.server.URL)
	_, a, window := startDesktopGUI(t, binary, config)
	// 1. 三轮对话，每轮回复很长，让消息区需要滚动。
	filler := strings.Repeat("filler line\n\n", 30)
	for index, name := range []string{"turn one", "turn two", "turn three"} {
		a.sendDesktopMessage(t, window, name)
		model.next(t).reply <- e2eModelReply{text: fmt.Sprintf("answer %d\n\n%s", index+1, filler)}
		a.waitNode(t, window, func(n desktopAutomationNode) bool {
			return strings.HasPrefix(n.Label, fmt.Sprintf("answer %d", index+1))
		})
		a.waitNode(t, window, func(n desktopAutomationNode) bool {
			return strings.Contains(n.Description, "default · idle · completed")
		})
	}
	// 3. 上下文面板：事实行 28 一档，原始数据默认收起，点开后展开。
	a.press(t, window, "上下文")
	labels := []string{"预计 token", "上下文窗口", "来源", "压缩阈值", "系统提示", "工具定义"}
	a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "text" && n.Label == "工具定义" })
	positions := map[string]float64{}
	for _, node := range a.getNodes(t, window) {
		if node.Role == "text" && slices.Contains(labels, node.Label) && node.Bounds.X > 900 {
			positions[node.Label] = node.Bounds.Y
		}
	}
	for index := 1; index < len(labels); index++ {
		if got := positions[labels[index]] - positions[labels[index-1]]; got != 28 {
			t.Fatalf("context fact rows %q → %q are %.1f apart, want 28: %v", labels[index-1], labels[index], got, positions)
		}
	}
	raw := a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "disclosure" && n.Label == "原始数据" })
	if slices.Contains(raw.States, "expanded") {
		t.Fatal("raw context data is expanded by default")
	}
	a.capture(t, window, "context-panel")
	a.call(t, "act", map[string]any{"window": window, "id": raw.ID, "action": "press"}, nil)
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "disclosure" && n.Label == "原始数据" && slices.Contains(n.States, "expanded")
	})
	// 4. RPC 面板：筛选框一行两个、方向和操作按钮另一行，右 Dock 默认 360 宽，输入框不窄于 120。
	a.press(t, window, "RPC")
	method := a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "textField" && fmt.Sprint(n.Key) == "rpc-method" })
	conversationFilter := a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "textField" && fmt.Sprint(n.Key) == "rpc-conversation"
	})
	direction := a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "popUpButton" && fmt.Sprint(n.Key) == "rpc-direction"
	})
	if method.Bounds.Width < 120 || conversationFilter.Bounds.Width < 120 || method.Bounds.X+method.Bounds.Width > conversationFilter.Bounds.X || direction.Bounds.Y <= method.Bounds.Y {
		t.Fatalf("RPC toolbar layout: method=%+v conversation=%+v direction=%+v", method.Bounds, conversationFilter.Bounds, direction.Bounds)
	}
	for _, label := range []string{"暂停", "清空", "导出 JSONL"} {
		button := a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "button" && n.Label == label })
		if button.Bounds.Y != direction.Bounds.Y+(direction.Bounds.Height-button.Bounds.Height)/2 {
			t.Fatalf("%q is not on the direction row: %+v vs %+v", label, button.Bounds, direction.Bounds)
		}
	}
	a.capture(t, window, "rpc-panel")
	// 5. Turn 面板：行是「#序号 输入摘要」；消息区在最底部时点 #1，第一条用户消息被滚到消息区顶部。
	a.press(t, window, "Turn")
	for _, title := range []string{"#3 turn three", "#2 turn two", "#1 turn one"} {
		a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "button" && n.Label == title })
	}
	a.capture(t, window, "turn-panel")
	a.press(t, window, "#1 turn one")
	first := a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "listItem" && strings.HasPrefix(n.Label, "turn one ") && n.Bounds.X > 240
	})
	if first.Bounds.Y > 140 {
		t.Fatalf("turn one message is at y=%.0f after jumping, want it at the top of the message area", first.Bounds.Y)
	}
	a.capture(t, window, "turn-jump")
	// 6. 消息文字上右键（此时消息区在第一轮）：主题菜单带「调试信息」；用户消息还有「复制消息」，选中文字后第一项是 kit 加的「复制」。
	user := a.waitSettledNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "text" && n.Label == "turn one" && n.Bounds.X > 240
	})
	a.rightClickAt(t, window, user.Bounds.X+10, user.Bounds.Y+user.Bounds.Height/2)
	a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "menuItem" && n.Label == "复制消息" })
	a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "menuItem" && n.Label == "调试信息" })
	a.capture(t, window, "message-menu-user")
	a.pressKey(t, window, "Escape")
	a.waitFocusedKey(t, window, "composer-text")
	// 双击选中一个词再右键。
	reply := a.waitSettledNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "text" && strings.HasPrefix(n.Label, "answer 1") })
	x, y := reply.Bounds.X+15, reply.Bounds.Y+reply.Bounds.Height/2
	a.sendInput(t, window, map[string]any{"type": "click", "x": x, "y": y, "clicks": 2})
	a.rightClickAt(t, window, x, y)
	a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "menuItem" && n.Label == "复制" })
	a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "menuItem" && n.Label == "调试信息" })
	if slices.ContainsFunc(a.getNodes(t, window), func(n desktopAutomationNode) bool { return n.Role == "menuItem" && n.Label == "复制消息" }) {
		t.Fatal("assistant message menu offers 复制消息")
	}
	a.capture(t, window, "message-menu-selection")
	a.pressKey(t, window, "Escape")
	a.waitFocusedKey(t, window, "composer-text")
	// 7. 调试面板没选 Item 或 Turn 时是空状态。
	a.press(t, window, "调试")
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "text" && n.Label == "选择 Item 或 Turn 查看调试信息"
	})
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "text" && strings.HasPrefix(n.Label, "点击消息下方")
	})
	a.capture(t, window, "debug-empty")
	// 8. 打开设置时右 Dock 收起，没有「归档」页签；再点一次回到对话，右 Dock 恢复。
	hasDock := func() bool {
		return slices.ContainsFunc(a.getNodes(t, window), func(n desktopAutomationNode) bool { return strings.HasPrefix(fmt.Sprint(n.Key), "dock-close-") })
	}
	if !hasDock() {
		t.Fatal("right dock is closed before opening settings")
	}
	a.press(t, window, "设置")
	a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "button" && n.Label == "通用" })
	if hasDock() || slices.ContainsFunc(a.getNodes(t, window), func(n desktopAutomationNode) bool { return n.Role == "tab" }) {
		t.Fatal("settings shows the right dock or a tab strip")
	}
	a.capture(t, window, "settings-no-dock")
	a.press(t, window, "设置")
	a.waitNode(t, window, func(n desktopAutomationNode) bool { return strings.HasPrefix(fmt.Sprint(n.Key), "dock-close-") })
}

// desktopCatalogFixture 是 models.dev 缓存里的一个小目录：一个兼容 OpenAI 的服务，让服务目录里有内容可选且不联网。
const desktopCatalogFixture = `{"compat":{"id":"compat","name":"Compat Gateway","npm":"@ai-sdk/openai-compatible","api":"https://compat.example/v1","env":["COMPAT_API_KEY"],"models":{"plain":{"id":"plain","name":"Plain Model","reasoning":false,"modalities":{"input":["text"],"output":["text"]},"limit":{"context":128000,"output":8192}}}}}`

// writeDesktopCatalogCache 在测试 HOME 里写 models.dev 缓存，服务目录不联网。
func writeDesktopCatalogCache(t *testing.T, config string) {
	t.Helper()
	path := filepath.Join(filepath.Dir(config), ".cache", "openvaala", "models-dev.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(desktopCatalogFixture), 0600); err != nil {
		t.Fatal(err)
	}
}

// hasNodeLabel 当前树里是否有指定标签的控件？
func (a *desktopAutomation) hasNodeLabel(t *testing.T, window int, role, label string) bool {
	t.Helper()
	return slices.ContainsFunc(a.getNodes(t, window), func(n desktopAutomationNode) bool {
		return (role == "" || n.Role == role) && n.Label == label
	})
}

// waitLabel 等待有指定标签的控件出现并返回它。
func (a *desktopAutomation) waitLabel(t *testing.T, window int, label string) desktopAutomationNode {
	t.Helper()
	return a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Label == label })
}

// requireDesktopDisplay 没有图形会话时跳过。
func requireDesktopDisplay(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		t.Skip("run with xvfb-run or a graphical session")
	}
	binary, err := buildDesktopOnce()
	if err != nil {
		t.Fatal(err)
	}
	return binary
}

// TestDesktopGUISettingsPages 检查设置页：打开后左侧栏换成设置导航（没有对话列表），五页各自渲染内容，点开 Executor 项才显示详情，Esc 回到对话并恢复侧栏和右 Dock。
func TestDesktopGUISettingsPages(t *testing.T) {
	binary := requireDesktopDisplay(t)
	model := newE2EModel(t)
	config := writeDesktopE2EConfig(t, model.server.URL)
	writeDesktopWorkLocationFixtures(t, config)
	_, a, window := startDesktopGUI(t, binary, config)
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "popUpButton" && fmt.Sprint(n.Key) == "model" && !slices.Contains(n.States, "disabled")
	})
	// 1. 新对话的空状态居中显示在对话区。
	a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "text" && n.Label == "想一起完成什么？" })
	// 2. 打开设置：导航五项加返回按钮，没有对话搜索框，右 Dock 收起。
	a.press(t, window, "设置")
	for _, label := range []string{"通用", "模型服务", "Executor", "Hook", "Skill"} {
		a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "button" && n.Label == label })
	}
	a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "button" && n.Label == "返回对话" })
	if _, found := a.findNodeByKey(t, window, "textField", "conversation-search"); found {
		t.Fatal("conversation list is shown while settings are open")
	}
	a.capture(t, window, "settings-general")
	// 3. 五页各一次：页内容出现。
	for _, page := range []struct{ nav, content string }{
		{"通用", "完成时发送系统通知"},
		{"模型服务", "全局默认模型"},
		{"Executor", "来自 config.yaml，编辑后点重载配置"},
		{"Hook", "来自 config.yaml，编辑后点重载配置"},
		{"Skill", "只显示这个 Agent 可用的 Skill"},
	} {
		a.press(t, window, page.nav)
		a.waitLabel(t, window, page.content)
		a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "button" && n.Label == "重载配置" })
		a.capture(t, window, "settings-"+page.nav)
	}
	// 4. Skill 页的项点开才有详情，原始数据默认收起。
	a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "button" && n.Label == "display-check" })
	if a.hasNodeLabel(t, window, "disclosure", "原始数据") {
		t.Fatal("raw data is shown before a skill is opened")
	}
	a.press(t, window, "display-check")
	raw := a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "disclosure" && n.Label == "原始数据" })
	if slices.Contains(raw.States, "expanded") {
		t.Fatal("raw data is expanded by default")
	}
	a.capture(t, window, "settings-skill-open")
	// 5. 重载配置给出成功通知。
	a.press(t, window, "重载配置")
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "status" && strings.HasPrefix(n.Label, "配置已重载")
	})
	// 6. Esc 回到对话：侧栏恢复对话列表，右 Dock 恢复。
	a.pressKey(t, window, "Escape")
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "textField" && fmt.Sprint(n.Key) == "conversation-search"
	})
	a.waitNode(t, window, func(n desktopAutomationNode) bool { return strings.HasPrefix(fmt.Sprint(n.Key), "dock-close-") })
	if a.hasNodeLabel(t, window, "button", "返回对话") {
		t.Fatal("settings navigation is still shown after Esc")
	}
	// 7. 侧栏收起时打开设置：侧栏临时显示设置导航，回到对话后恢复收起。
	a.call(t, "menu", map[string]any{"window": window, "path": []string{"视图", "切换侧栏"}}, nil)
	a.waitNodeAbsent(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "textField" && fmt.Sprint(n.Key) == "conversation-search"
	})
	a.press(t, window, "设置")
	a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "button" && n.Label == "模型服务" })
	a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "button" && n.Label == "返回对话" })
	a.capture(t, window, "settings-collapsed-sidebar")
	a.pressKey(t, window, "Escape")
	a.waitNodeAbsent(t, window, func(n desktopAutomationNode) bool { return n.Role == "button" && n.Label == "返回对话" })
	if _, found := a.findNodeByKey(t, window, "textField", "conversation-search"); found {
		t.Fatal("the sidebar stayed open after leaving settings")
	}
}

// TestDesktopGUIOnboarding 检查没有任何模型服务时：对话区引导添加，按钮打开模型服务页并展开服务目录；
// 发送失败显示中文提示和同一个入口；从目录加一个服务、添加模型并应用后引导消失。
func TestDesktopGUIOnboarding(t *testing.T) {
	binary := requireDesktopDisplay(t)
	config := writeE2EConfigWithoutModels(t)
	writeDesktopCatalogCache(t, config)
	_, a, window := startDesktopGUI(t, binary, config)
	// 1. 连上服务且没有模型：空状态是引导，文字和主按钮居中在对话区。
	add := a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "button" && n.Label == "添加模型服务" })
	guide := a.waitLabel(t, window, "还没有模型服务")
	if add.Bounds.Y < guide.Bounds.Y+guide.Bounds.Height {
		t.Fatalf("the add button %+v is not below the guide text %+v", add.Bounds, guide.Bounds)
	}
	a.capture(t, window, "onboarding")
	// 2. 发送失败：中文提示加同一个按钮，没有英文 RPC 原文。
	a.setText(t, window, "composer-text", "你好")
	a.press(t, window, "发送")
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "text" && n.Label == "还没有模型服务" && n.Bounds.Y > 500
	})
	if slices.ContainsFunc(a.getNodes(t, window), func(n desktopAutomationNode) bool {
		return strings.Contains(n.Label, "no model is configured") || strings.Contains(n.Label, "code -32000")
	}) {
		t.Fatal("the raw RPC error is shown")
	}
	a.capture(t, window, "onboarding-send-failed")
	// 3. 按钮打开模型服务页，服务目录已展开。
	a.press(t, window, "添加模型服务")
	a.waitLabel(t, window, "还没有模型服务，从服务目录选一个，或新增服务")
	a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "button" && n.Label == "Compat Gateway" })
	a.capture(t, window, "onboarding-catalog")
	// 4. 选目录里的服务，加一个模型，应用；回到对话后引导消失。
	a.press(t, window, "Compat Gateway")
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "textField" && n.Label == "Provider ID" && n.Value == "compat"
	})
	a.setText(t, window, "manual-model", "plain")
	a.press(t, window, "添加模型")
	a.press(t, window, "应用")
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "status" && strings.HasPrefix(n.Label, "模型服务已保存")
	})
	a.pressKey(t, window, "Escape")
	a.waitLabel(t, window, "想一起完成什么？")
	if a.hasNodeLabel(t, window, "button", "添加模型服务") && a.hasNodeLabel(t, window, "text", "添加一个模型服务后就能开始对话") {
		t.Fatal("onboarding is still shown after adding a provider")
	}
}

// TestDesktopGUIConnectionBanner 检查服务停止后对话区顶部的横幅：本地化原因加「重试」；服务重新启动后重试恢复，横幅消失。
func TestDesktopGUIConnectionBanner(t *testing.T) {
	binary := requireDesktopDisplay(t)
	model := newE2EModel(t)
	config := writeDesktopE2EConfig(t, model.server.URL)
	_, a, window := startDesktopGUI(t, binary, config)
	// 1. 冷启动连接过程中和连接正常后都没有横幅：从窗口出现起逐次读树，直到模型下拉可用。
	for {
		nodes := a.getNodes(t, window)
		if slices.ContainsFunc(nodes, func(n desktopAutomationNode) bool { return fmt.Sprint(n.Key) == "connection-banner" }) {
			a.capture(t, window, "connection-banner-on-cold-start")
			t.Fatal("banner shown during the initial connection")
		}
		if slices.ContainsFunc(nodes, func(n desktopAutomationNode) bool {
			return n.Role == "popUpButton" && fmt.Sprint(n.Key) == "model" && !slices.Contains(n.States, "disabled")
		}) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	a.capture(t, window, "connection-cold-start-connected")
	// 2. 停掉本机服务：横幅出现，原因是中文，带「重试」。
	stopServe(t, binary, config)
	banner := a.waitNode(t, window, func(n desktopAutomationNode) bool { return fmt.Sprint(n.Key) == "connection-banner" })
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "button" && n.Label == "重试" && n.Parent == banner.ID
	})
	if !slices.ContainsFunc(a.getNodes(t, window), func(n desktopAutomationNode) bool {
		return n.Role == "text" && (n.Label == "与本机服务的连接已断开" || n.Label == "无法连接本机服务") && n.Parent != ""
	}) {
		t.Fatal("banner has no localized reason")
	}
	// 状态栏只显示连接状态：有短状态文字，「重试」只有横幅里的一个，状态栏里没有错误原文。
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "text" && (n.Label == "正在重连…" || n.Label == "连接失败") && n.Bounds.Y > 500
	})
	retries := 0
	for _, n := range a.getNodes(t, window) {
		if n.Role == "button" && n.Label == "重试" {
			retries++
		}
		if n.Bounds.Y > 500 && strings.Contains(n.Label, "dial ") {
			t.Fatalf("the status bar shows the raw error: %+v", n)
		}
	}
	if retries != 1 {
		t.Fatalf("retry buttons = %d, want only the banner's", retries)
	}
	a.capture(t, window, "connection-banner")
	// 3. 重新启动服务后，客户端自己重连：不点任何按钮，横幅消失，状态回到已连接。
	startE2EProcess(t, binary, config, []string{"serve"}, false)
	waitBannerGone := func(what string) {
		deadline := time.Now().Add(e2eTimeout)
		for slices.ContainsFunc(a.getNodes(t, window), func(n desktopAutomationNode) bool { return fmt.Sprint(n.Key) == "connection-banner" }) {
			if time.Now().After(deadline) {
				a.capture(t, window, "connection-banner-stuck")
				t.Fatalf("banner did not disappear after the service restarted (%s)", what)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	waitBannerGone("automatic reconnect")
	a.waitLabel(t, window, "已连接")
	a.capture(t, window, "connection-recovered")
	// 4. 再停一次服务，横幅出现后重新启动并点一次「重试」；客户端自己的重连可能更早完成，两者都应让横幅消失。
	stopServe(t, binary, config)
	a.waitNode(t, window, func(n desktopAutomationNode) bool { return fmt.Sprint(n.Key) == "connection-banner" })
	startE2EProcess(t, binary, config, []string{"serve"}, false)
	for _, node := range a.getNodes(t, window) {
		if node.Role == "button" && node.Label == "重试" && !slices.Contains(node.States, "disabled") {
			_, _ = a.request("act", map[string]any{"window": window, "id": node.ID, "action": "press"})
			break
		}
	}
	waitBannerGone("manual retry")
	a.waitLabel(t, window, "已连接")
}

// TestDesktopGUICommandPalette 检查命令面板：标题不带冒号，输入过滤后 Enter 执行第一项；命令很多时列表可滚动，最后一项能滚进可见区。
func TestDesktopGUICommandPalette(t *testing.T) {
	binary := requireDesktopDisplay(t)
	model := newE2EModel(t)
	config := writeDesktopE2EConfig(t, model.server.URL)
	_, a, window := startDesktopGUI(t, binary, config)
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "popUpButton" && fmt.Sprint(n.Key) == "model" && !slices.Contains(n.States, "disabled")
	})
	openPalette := func() {
		a.call(t, "menu", map[string]any{"window": window, "path": []string{"转到", "命令面板"}}, nil)
		a.waitNode(t, window, func(n desktopAutomationNode) bool {
			return n.Role == "textField" && fmt.Sprint(n.Key) == "command-search"
		})
	}
	// 1. 初始列表：命令标题没有冒号，快捷键显示在右侧；列表比面板高，最后一项在可见区之外，滚到它后进入可见区。
	openPalette()
	search := a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "textField" && fmt.Sprint(n.Key) == "command-search"
	})
	for _, node := range a.getNodes(t, window) {
		if strings.HasPrefix(fmt.Sprint(node.Key), "command-") && node.Role == "button" && strings.ContainsAny(node.Label, ":：") {
			t.Fatalf("command title %q has a colon", node.Label)
		}
	}
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "button" && n.Label == "Agent 面板" && fmt.Sprint(n.Key) == "command-panel.agent"
	})
	last := a.waitNode(t, window, func(n desktopAutomationNode) bool { return fmt.Sprint(n.Key) == "command-switch-language" })
	listBottom := search.Bounds.Y + search.Bounds.Height + 360 + 8
	if last.Bounds.Y+last.Bounds.Height <= listBottom-1 {
		t.Fatalf("the last command is already visible (y=%.0f, list bottom %.0f): the list does not need to scroll", last.Bounds.Y, listBottom)
	}
	a.sendInput(t, window, map[string]any{"type": "wheel", "x": search.Bounds.X + 100, "y": search.Bounds.Y + 150, "dx": 0, "dy": 3000})
	deadline := time.Now().Add(e2eTimeout)
	for {
		scrolled := a.waitNode(t, window, func(n desktopAutomationNode) bool { return fmt.Sprint(n.Key) == "command-switch-language" })
		if scrolled.Bounds.Y+scrolled.Bounds.Height <= listBottom+1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("scrolling did not bring the last command into view: %+v, list bottom %.0f", scrolled.Bounds, listBottom)
		}
		time.Sleep(50 * time.Millisecond)
	}
	a.capture(t, window, "command-palette-scrolled")
	// 2. 过滤「RPC 面板」，Enter 执行第一项：右 Dock 切到 RPC 面板。
	a.focusTextField(t, window, "command-search")
	a.typeText(t, window, "RPC 面板")
	for deadline := time.Now().Add(e2eTimeout); a.hasNodeLabel(t, window, "button", "Agent 面板"); {
		if time.Now().After(deadline) {
			t.Fatal("filter kept an unrelated command")
		}
		time.Sleep(50 * time.Millisecond)
	}
	a.waitNode(t, window, func(n desktopAutomationNode) bool { return fmt.Sprint(n.Key) == "command-panel.rpc" })
	a.pressKey(t, window, "Enter")
	a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "button" && n.Label == "暂停" })
	// 3. 切换主题命令：循环到浅色并有通知。
	openPalette()
	a.focusTextField(t, window, "command-search")
	a.typeText(t, window, "切换主题")
	for deadline := time.Now().Add(e2eTimeout); a.hasNodeLabel(t, window, "button", "切换语言"); {
		if time.Now().After(deadline) {
			t.Fatal("filter kept an unrelated command")
		}
		time.Sleep(50 * time.Millisecond)
	}
	a.pressKey(t, window, "Enter")
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "status" && strings.HasPrefix(n.Label, "主题：")
	})
	// 4. 「搜索对话」把键盘焦点交给侧栏搜索框，输入框不抢回焦点。
	openPalette()
	a.focusTextField(t, window, "command-search")
	a.typeText(t, window, "搜索对话")
	// 过滤生效（别的命令消失）后 Enter 才执行「搜索对话」，否则会执行列表里的第一项。
	a.waitNodeAbsent(t, window, func(n desktopAutomationNode) bool { return fmt.Sprint(n.Key) == "command-new-conversation" })
	a.waitNode(t, window, func(n desktopAutomationNode) bool { return fmt.Sprint(n.Key) == "command-search-conversations" })
	a.pressKey(t, window, "Enter")
	a.waitFocusedKey(t, window, "conversation-search")
	a.settle(t, window)
	a.waitFocusedKey(t, window, "conversation-search")
}

// TestDesktopGUIEnglishUI 检查英文界面：对话区、命令面板、设置的通用、模型服务和 Hook 页没有中文（用户和服务数据除外）。
func TestDesktopGUIEnglishUI(t *testing.T) {
	binary := requireDesktopDisplay(t)
	model := newE2EModel(t)
	config := writeDesktopE2EConfig(t, model.server.URL)
	appData := filepath.Join(filepath.Dir(config), ".config", "OpenVaala")
	if runtime.GOOS == "darwin" {
		appData = filepath.Join(filepath.Dir(config), "Library", "Application Support", "OpenVaala")
	}
	if err := os.MkdirAll(appData, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appData, "preferences.json"), []byte(`{"language":"en","width":1280,"height":820}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, a, window := startDesktopGUI(t, binary, config)
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "popUpButton" && fmt.Sprint(n.Key) == "model" && !slices.Contains(n.States, "disabled")
	})
	han := regexp.MustCompile(`\p{Han}`)
	assertEnglish := func(step string) {
		t.Helper()
		a.call(t, "idle", map[string]any{"window": window, "timeout": 1000}, nil)
		for _, node := range a.getNodes(t, window) {
			values := []string{node.Label, node.Description}
			if node.Role != "textField" {
				values = append(values, node.Value)
			}
			for _, value := range values {
				if han.MatchString(value) {
					t.Errorf("%s: %s %q contains Chinese: %q", step, node.Role, node.Label, value)
				}
			}
		}
	}
	// 1. 对话区和命令面板。
	a.waitLabel(t, window, "What would you like to work on?")
	assertEnglish("conversation")
	a.call(t, "menu", map[string]any{"window": window, "path": []string{"Go", "Command palette"}}, nil)
	a.waitNode(t, window, func(n desktopAutomationNode) bool {
		return n.Role == "textField" && fmt.Sprint(n.Key) == "command-search"
	})
	assertEnglish("command palette")
	a.capture(t, window, "english-command-palette")
	a.pressKey(t, window, "Escape")
	// 2. 设置：通用、模型服务（含表单）、Hook。
	a.press(t, window, "Settings")
	a.waitLabel(t, window, "Send a system notification when finished")
	assertEnglish("settings general")
	a.capture(t, window, "english-settings-general")
	a.press(t, window, "Providers")
	a.press(t, window, "local")
	a.waitNode(t, window, func(n desktopAutomationNode) bool { return n.Role == "textField" && n.Label == "Provider ID" })
	assertEnglish("settings providers")
	a.capture(t, window, "english-settings-providers")
	a.press(t, window, "Hooks")
	a.waitLabel(t, window, "From config.yaml. Reload the configuration after editing.")
	assertEnglish("settings hooks")
}
