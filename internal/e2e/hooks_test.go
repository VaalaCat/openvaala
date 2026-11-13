package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/format"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
)

// hooksGuardScript 是 PreToolUse 的 guard.sh：把收到的 stdin 存进 INPUT_DIRECTORY，命令里有 rm -rf 时退出码 2 拦下，原因写 stderr。
const hooksGuardScript = `#!/bin/bash
input=$(cat)
printf '%s' "$input" > "INPUT_DIRECTORY/guard.json"
case "$input" in
*"rm -rf"*)
  echo "rm -rf is not allowed here, use trash instead" >&2
  exit 2
  ;;
esac
`

// hooksFormatScript 是 PostToolUse 的 format.sh：把收到的 stdin 存进 INPUT_DIRECTORY，用 tool_response.added 里的路径（相对工作区根目录，
// hook 的工作目录就是它）跑 gofmt -w，再以 JSON 输出 additionalContext。只用 grep 和 tr 取字段。
const hooksFormatScript = `#!/bin/bash
input=$(cat)
printf '%s' "$input" > "INPUT_DIRECTORY/format.json"
added=$(printf '%s' "$input" | grep -o '"added":\[[^]]*\]' | grep -o '"[^"]*"' | tail -n +2 | tr -d '"')
for path in $added; do
  GOFMT_COMMAND -w "$path"
done
echo '{"hookSpecificOutput":{"hookEventName":"PostToolUse","additionalContext":"format.sh formatted '"$added"'"}}'
`

// hooksPromptScript 是 UserPromptSubmit 的 prompt.sh：把收到的 stdin 存进 INPUT_DIRECTORY，输出的纯文本作为补充上下文交给模型。
const hooksPromptScript = `#!/bin/bash
input=$(cat)
printf '%s' "$input" > "INPUT_DIRECTORY/prompt.json"
echo "prompt.sh saw the user prompt"
`

// hooksGuardYAML 是 guard.yaml：PreToolUse 对 bash 跑 guard.sh，PostToolUse 对 apply_patch 跑 format.sh。
const hooksGuardYAML = `hooks:
  PreToolUse:
    - matcher: bash
      hooks:
        - type: command
          script: guard.sh
  PostToolUse:
    - matcher: apply_patch
      hooks:
        - type: command
          script: format.sh
`

// hooksPromptYAML 是 prompt.yaml：每次 UserPromptSubmit 都跑 prompt.sh；reload 用例运行时才写入。
const hooksPromptYAML = `hooks:
  UserPromptSubmit:
    - hooks:
        - type: command
          script: prompt.sh
`

// hooksUnformattedGo 是 apply_patch 新建的 a.go：故意没有格式化。
const hooksUnformattedGo = "package main\nfunc  main(){\nprintln( \"hi\" )\n}\n"

// TestHooksEndToEnd 检查 hook 拦截、格式化、通知、列表和重新加载。
func TestHooksEndToEnd(t *testing.T) {
	// 1. 创建临时模型、配置、工作区和 hook 脚本。
	binary := buildOpenVaala(t)
	model := newE2EModel(t)
	config := writeE2EConfig(t, model.server.URL)
	home := filepath.Dir(config)
	f := &hooksFixture{
		home:      home,
		model:     model,
		workspace: newE2EWorkspace(t),
		gofmtPath: getGofmtPath(t),
	}
	f.writeHookFiles(t)
	if err := os.MkdirAll(filepath.Join(f.workspace, "build"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.workspace, "build", "keep.txt"), []byte("KEEP"), 0600); err != nil {
		t.Fatal(err)
	}

	// 2. 启动 serve 并登记清理（控制 Client 和 app-server 先退出，serve 最后停止），连上控制 Client 和 stdio app-server，开一个 Thread。
	startE2EServe(t, binary, config)
	stopServeOnCleanup(t, binary, config)
	f.control, _ = newE2ERuntimeClient(t, config)
	go func() {
		for range f.control.Events() {
		}
	}()
	f.wire = startAppServer(t, binary, config, "stdio")
	f.wire.call(t, "initialize", codexClientInfo, nil)
	f.threadID = (&codexTurnFixture{
		wire:  f.wire,
		model: model,
	}).startThread(t, f.workspace, nil).Thread.ID

	// 3. 按顺序运行各用例，共用同一个 Thread 和假模型。
	for _, test := range []struct {
		name string
		run  func(t *testing.T, f *hooksFixture)
	}{
		{
			name: "pre_tool_use_blocks_rm_rf",
			run: func(t *testing.T, f *hooksFixture) {
				// 1. 调用 rm -rf build，检查 hook_blocked 结果、保留文件和 hook 开始与拦截通知。
				requestID := f.startTurn(t, "remove build")
				f.model.next(t).reply <- e2eModelReply{
					tool:      "bash",
					arguments: `{"command":"rm -rf build"}`,
				}
				second := f.model.next(t)
				result := getModelToolResult(t, second.body)
				if result.Error == nil || result.Error.Code != "hook_blocked" ||
					result.Error.Message != "blocked by PreToolUse hook: rm -rf is not allowed here, use trash instead" {
					t.Errorf("tool result sent back to the model = %+v, want hook_blocked with guard.sh's reason", result.Error)
				}
				second.reply <- e2eModelReply{text: "I will not delete it"}
				messages := f.wire.readTurn(t, f.threadID, requestID)
				assertCodexTurnCompleted(t, messages, "completed")
				if _, err := os.Stat(filepath.Join(f.workspace, "build", "keep.txt")); err != nil {
					t.Errorf("build/keep.txt after the blocked rm -rf: %v, want it to be still there", err)
				}
				notifications := getHookNotifications(t, messages)
				run := assertHookRunNotifications(t, notifications, "preToolUse", "blocked", f.getHookSourcePath("guard.yaml"))
				if !slices.Contains(run.completed.Run.Entries, hooksWireEntry{
					Kind: "feedback",
					Text: "rm -rf is not allowed here, use trash instead",
				}) {
					t.Errorf("blocked hook/completed entries = %+v, want the feedback guard.sh wrote", run.completed.Run.Entries)
				}
				// 2. 检查脚本收到的事件、对话、Turn、工作区、工具和参数，以及缓存副本。
				input := getHookInput(t, f.getInputPath("guard.json"))
				toolInput, _ := input["tool_input"].(map[string]any)
				if input["hook_event_name"] != "PreToolUse" || input["session_id"] != f.threadID || input["turn_id"] != run.started.TurnID ||
					input["cwd"] != f.workspace || input["tool_name"] != "bash" || input["tool_use_id"] != "call-1" || toolInput["command"] != "rm -rf build" {
					t.Errorf("guard.sh stdin = %v, want PreToolUse of bash rm -rf build in thread %s, turn %s and cwd %s", input, f.threadID, run.started.TurnID, f.workspace)
				}
				f.assertScriptCopied(t, "guard.sh", hooksGuardScript)
			},
		},
		{
			name: "post_tool_use_formats_new_file",
			run: func(t *testing.T, f *hooksFixture) {
				// 1. 用 apply_patch 新建 a.go，检查工具结果后的 hook 补充上下文。
				requestID := f.startTurn(t, "write a.go")
				patch := "*** Begin Patch\n*** Add File: a.go\n" + prefixLines(hooksUnformattedGo, "+") + "*** End Patch"
				arguments, err := json.Marshal(map[string]string{"patch": patch})
				if err != nil {
					t.Fatal(err)
				}
				f.model.next(t).reply <- e2eModelReply{
					tool:      "apply_patch",
					arguments: string(arguments),
				}
				second := f.model.next(t)
				if result := getModelToolResult(t, second.body); result.Error != nil || !strings.Contains(string(result.Content), `"added":["a.go"]`) {
					t.Errorf("apply_patch result sent back to the model = content %s, error %+v, want a.go added and no error", result.Content, result.Error)
				}
				messages := getModelMessageTexts(second.body)
				toolIndex := slices.IndexFunc(messages, func(message modelMessageText) bool { return message.role == "tool" })
				hookIndex := slices.IndexFunc(messages, func(message modelMessageText) bool {
					return message.role == "user" && strings.Contains(message.text, "<openvaala-hook>") && strings.Contains(message.text, "<event>PostToolUse</event>") &&
						strings.Contains(message.text, "format.sh formatted a.go")
				})
				if toolIndex < 0 || hookIndex < toolIndex {
					t.Errorf("model request messages = %+v, want the PostToolUse openvaala-hook user message after the tool result", messages)
				}
				second.reply <- e2eModelReply{text: "a.go is written"}
				turnMessages := f.wire.readTurn(t, f.threadID, requestID)
				assertCodexTurnCompleted(t, turnMessages, "completed")

				// 2. 检查文件格式、完成通知、脚本输入和缓存副本。
				written, err := os.ReadFile(filepath.Join(f.workspace, "a.go"))
				if err != nil {
					t.Fatal(err)
				}
				if f.gofmtPath == "" {
					t.Log("no gofmt found, skipping the formatting assertion")
				} else if want, err := format.Source([]byte(hooksUnformattedGo)); err != nil || string(written) != string(want) || string(written) == hooksUnformattedGo {
					t.Errorf("a.go = %q, want the gofmt-formatted %q (format error %v)", written, want, err)
				}
				run := assertHookRunNotifications(t, getHookNotifications(t, turnMessages), "postToolUse", "completed", f.getHookSourcePath("guard.yaml"))
				if !slices.Contains(run.completed.Run.Entries, hooksWireEntry{
					Kind: "context",
					Text: "format.sh formatted a.go",
				}) {
					t.Errorf("postToolUse hook/completed entries = %+v, want the additionalContext format.sh printed", run.completed.Run.Entries)
				}
				input := getHookInput(t, f.getInputPath("format.json"))
				toolResponse, _ := input["tool_response"].(map[string]any)
				if input["hook_event_name"] != "PostToolUse" || input["tool_name"] != "apply_patch" || input["tool_use_id"] != "call-2" || input["cwd"] != f.workspace ||
					!slices.Equal(toAnySlice(toolResponse["added"]), []any{"a.go"}) {
					t.Errorf("format.sh stdin = %v, want PostToolUse of apply_patch call-2 in %s with tool_response.added [a.go]", input, f.workspace)
				}
				f.assertScriptCopied(t, "format.sh", hooksFormatScript)
			},
		},
		{
			name: "hooks_list_shows_configured_hooks",
			run: func(t *testing.T, f *hooksFixture) {
				// 1. 查询 hook 列表，按配置顺序比较 guard.yaml 中的两个 hook。
				assertListedHooks(t, f.listHooks(t), []hooksExpectedListing{
					{
						key:       "guard:PreToolUse:0",
						eventName: "preToolUse",
						matcher:   "bash",
						command:   f.getHookSourcePath("guard.sh"),
					},
					{
						key:       "guard:PostToolUse:0",
						eventName: "postToolUse",
						matcher:   "apply_patch",
						command:   f.getHookSourcePath("format.sh"),
					},
				})
			},
		},
		{
			name: "user_prompt_submit_hook_after_reload",
			run: func(t *testing.T, f *hooksFixture) {
				// 1. 新增 UserPromptSubmit hook 并重新加载，检查列表里多了这个 hook。
				if err := os.WriteFile(filepath.Join(f.home, "hooks", "prompt.sh"), []byte(f.expandScript(hooksPromptScript)), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(f.home, "hooks", "prompt.yaml"), []byte(hooksPromptYAML), 0600); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
				defer cancel()
				result, err := f.control.ReloadConfig(ctx)
				if err != nil || len(result.PendingConversationIDs) != 0 {
					t.Fatalf("config/reload = %+v, error %v, want it to succeed with no busy conversation", result, err)
				}
				assertListedHooks(t, f.listHooks(t), []hooksExpectedListing{
					{
						key:       "guard:PreToolUse:0",
						eventName: "preToolUse",
						matcher:   "bash",
						command:   f.getHookSourcePath("guard.sh"),
					},
					{
						key:       "guard:PostToolUse:0",
						eventName: "postToolUse",
						matcher:   "apply_patch",
						command:   f.getHookSourcePath("format.sh"),
					},
					{
						key:       "prompt:UserPromptSubmit:0",
						eventName: "userPromptSubmit",
						command:   f.getHookSourcePath("prompt.sh"),
					},
				})

				// 2. 提交下一轮，检查用户输入之后的 hook 上下文、通知和脚本输入。
				requestID := f.startTurn(t, "hello from the user")
				request := f.model.next(t)
				messages := getModelMessageTexts(request.body)
				inputIndex := slices.IndexFunc(messages, func(message modelMessageText) bool {
					return message.role == "user" && strings.Contains(message.text, "hello from the user")
				})
				hookIndex := slices.IndexFunc(messages, func(message modelMessageText) bool {
					return message.role == "user" && strings.Contains(message.text, "<openvaala-hook>") && strings.Contains(message.text, "<event>UserPromptSubmit</event>") &&
						strings.Contains(message.text, "prompt.sh saw the user prompt")
				})
				if inputIndex < 0 || hookIndex < inputIndex {
					t.Errorf("model request messages = %+v, want the UserPromptSubmit openvaala-hook user message after the input", messages)
				}
				request.reply <- e2eModelReply{text: "hi"}
				turnMessages := f.wire.readTurn(t, f.threadID, requestID)
				assertCodexTurnCompleted(t, turnMessages, "completed")
				run := assertHookRunNotifications(t, getHookNotifications(t, turnMessages), "userPromptSubmit", "completed", f.getHookSourcePath("prompt.yaml"))
				if !slices.Contains(run.completed.Run.Entries, hooksWireEntry{
					Kind: "context",
					Text: "prompt.sh saw the user prompt",
				}) {
					t.Errorf("userPromptSubmit hook/completed entries = %+v, want the plain text prompt.sh printed as context", run.completed.Run.Entries)
				}
				input := getHookInput(t, f.getInputPath("prompt.json"))
				if input["hook_event_name"] != "UserPromptSubmit" || input["prompt"] != "hello from the user" || input["session_id"] != f.threadID || input["cwd"] != f.workspace {
					t.Errorf("prompt.sh stdin = %v, want UserPromptSubmit with the prompt in thread %s and cwd %s", input, f.threadID, f.workspace)
				}
				f.assertScriptCopied(t, "prompt.sh", hooksPromptScript)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) { test.run(t, f) })
	}
}

// hooksFixture 是 TestHooksEndToEnd 各用例共用的环境：配置所在的临时 HOME、假模型、serve 上的一个 Thread（经 stdio app-server 连接）和控制 Client。
type hooksFixture struct {
	// home 是临时 HOME；hook 的配置和脚本在 home/hooks，脚本收到的 stdin 存在 home/hook-inputs，拷贝在 home/.cache/openvaala/hooks。
	home      string
	model     *e2eModel
	workspace string
	// gofmtPath 是 gofmt 的绝对路径，找不到时为空。
	gofmtPath string
	control   runtimeclient.Client
	wire      *codexWire
	threadID  string
}

// writeHookFiles 写入测试 hook 配置、脚本和输入保存目录。
func (f *hooksFixture) writeHookFiles(t *testing.T) {
	t.Helper()
	// 1. 建立输入目录，展开脚本模板并按文件种类设置权限后写入。
	if err := os.MkdirAll(filepath.Join(f.home, "hook-inputs"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"guard.yaml": hooksGuardYAML, "guard.sh": f.expandScript(hooksGuardScript), "format.sh": f.expandScript(hooksFormatScript)} {
		path := filepath.Join(f.home, "hooks", name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0600)
		if strings.HasSuffix(name, ".sh") {
			mode = 0700
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
}

// expandScript 将模板中的输入目录和 gofmt 命令替换为测试环境的值。
func (f *hooksFixture) expandScript(script string) string {
	// 1. 缺少 gofmt 时使用 true，再替换模板中的路径和命令。
	gofmtCommand := f.gofmtPath
	if gofmtCommand == "" {
		gofmtCommand = "true"
	}
	return strings.NewReplacer("INPUT_DIRECTORY", filepath.Join(f.home, "hook-inputs"), "GOFMT_COMMAND", gofmtCommand).Replace(script)
}

// getHookSourcePath 返回测试 hook 源文件路径。
func (f *hooksFixture) getHookSourcePath(name string) string {
	// 1. 拼接临时 HOME、hooks 和文件名。
	return filepath.Join(f.home, "hooks", name)
}

// getInputPath 返回 hook 保存标准输入的文件路径。
func (f *hooksFixture) getInputPath(name string) string {
	// 1. 拼接临时 HOME、hook-inputs 和文件名。
	return filepath.Join(f.home, "hook-inputs", name)
}

// startTurn 提交测试 Thread 输入并返回请求 ID。
func (f *hooksFixture) startTurn(t *testing.T, text string) json.RawMessage {
	t.Helper()
	// 1. 通过 turn/start 发送文本输入。
	return f.wire.request(t, "turn/start", map[string]any{"threadId": f.threadID, "input": []any{codexTextInput(text)}})
}

// listHooks 读取工作区的 hook 列表。
func (f *hooksFixture) listHooks(t *testing.T) []hooksWireListedHook {
	t.Helper()
	// 1. 请求 hooks/list，检查只返回指定工作区且没有警告和错误。
	var listed struct {
		Data []struct {
			Cwd      string                `json:"cwd"`
			Hooks    []hooksWireListedHook `json:"hooks"`
			Warnings []string              `json:"warnings"`
			Errors   []any                 `json:"errors"`
		} `json:"data"`
	}
	f.wire.call(t, "hooks/list", map[string]any{"cwds": []string{f.workspace}}, &listed)
	if len(listed.Data) != 1 || listed.Data[0].Cwd != f.workspace || len(listed.Data[0].Warnings) != 0 || len(listed.Data[0].Errors) != 0 {
		t.Fatalf("hooks/list = %+v, want one entry for %s without warnings and errors", listed, f.workspace)
	}
	return listed.Data[0].Hooks
}

// assertScriptCopied 检查脚本副本的内容、执行权限和缓存位置。
func (f *hooksFixture) assertScriptCopied(t *testing.T, name, template string) {
	t.Helper()
	// 1. 按源文件权限和内容计算缓存目录，检查临时 HOME 内副本。
	content := f.expandScript(template)
	source, err := os.Stat(f.getHookSourcePath(name))
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(append([]byte(fmt.Sprintf("%04o\x00", source.Mode().Perm())), content...))
	hashDirectory := hex.EncodeToString(hash[:])[:16]
	copied := filepath.Join(f.home, ".cache", "openvaala", "hooks", hashDirectory, name)
	info, err := os.Stat(copied)
	if err != nil {
		t.Errorf("copied script %s: %v, want it under the temp HOME cache", name, err)
		return
	}
	if payload, err := os.ReadFile(copied); err != nil || string(payload) != content || info.Mode().Perm()&0100 == 0 {
		t.Errorf("copied script %s = mode %v, content equal %v (read error %v), want the local content and an executable mode", name, info.Mode(), string(payload) == content, err)
	}
	// 2. 检查真实 HOME 的缓存目录没有这个脚本。
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	realCopy := filepath.Join(realHome, ".cache", "openvaala", "hooks", hashDirectory)
	if _, err := os.Stat(realCopy); !os.IsNotExist(err) {
		t.Errorf("stat %s = %v, want the script not to be copied under the real HOME", realCopy, err)
	}
}

// getGofmtPath 查找 gofmt 的绝对路径。
func getGofmtPath(t *testing.T) string {
	t.Helper()
	// 1. 先查询 PATH，再检查 GOROOT 的 bin 目录；未找到时返回空。
	if path, err := exec.LookPath("gofmt"); err == nil {
		return path
	}
	if output, err := exec.Command("go", "env", "GOROOT").Output(); err == nil {
		path := filepath.Join(strings.TrimSpace(string(output)), "bin", "gofmt")
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}

// ── hook 通知与 hooks/list ──

// hooksWireEntry 是 hook/started、hook/completed 里 run.entries 的一段输出。
type hooksWireEntry struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// hooksWireNotification 是 hook/started 或 hook/completed 里测试关心的字段。
type hooksWireNotification struct {
	method   string
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	Run      struct {
		ID            string           `json:"id"`
		EventName     string           `json:"eventName"`
		HandlerType   string           `json:"handlerType"`
		ExecutionMode string           `json:"executionMode"`
		Scope         string           `json:"scope"`
		SourcePath    string           `json:"sourcePath"`
		Source        string           `json:"source"`
		Status        string           `json:"status"`
		CompletedAt   *int64           `json:"completedAt"`
		Entries       []hooksWireEntry `json:"entries"`
	} `json:"run"`
}

// hooksWireRun 是同一次 hook 运行的开始和结束通知。
type hooksWireRun struct {
	started, completed hooksWireNotification
}

// hooksWireListedHook 是 hooks/list 里一个 hook 测试关心的字段；没写 matcher 时 Matcher 为 nil，用户自己的 hook 的 PluginID 为 nil。
type hooksWireListedHook struct {
	Key         string  `json:"key"`
	EventName   string  `json:"eventName"`
	Command     string  `json:"command"`
	Matcher     *string `json:"matcher"`
	TimeoutSec  int     `json:"timeoutSec"`
	SourcePath  string  `json:"sourcePath"`
	Source      string  `json:"source"`
	PluginID    *string `json:"pluginId"`
	Enabled     bool    `json:"enabled"`
	IsManaged   bool    `json:"isManaged"`
	CurrentHash string  `json:"currentHash"`
	TrustStatus string  `json:"trustStatus"`
}

// hooksExpectedListing 是 hooks/list 里一个 hook 期望的 key、事件、matcher（空串表示没写）和脚本路径。
type hooksExpectedListing struct {
	key, eventName, matcher, command string
}

// getHookNotifications 按到达顺序提取 hook 开始和完成通知。
func getHookNotifications(t *testing.T, messages codexTurnMessages) []hooksWireNotification {
	t.Helper()
	// 1. 筛选两类 hook 通知并解码参数。
	var notifications []hooksWireNotification
	for _, message := range messages {
		if message.Method != "hook/started" && message.Method != "hook/completed" {
			continue
		}
		notification := hooksWireNotification{method: message.Method}
		if err := json.Unmarshal(message.Params, &notification); err != nil {
			t.Fatalf("%s params %s: %v", message.Method, message.Params, err)
		}
		notifications = append(notifications, notification)
	}
	return notifications
}

// assertHookRunNotifications 检查同一次 hook 的开始和完成通知。
func assertHookRunNotifications(t *testing.T, notifications []hooksWireNotification, eventName, status, sourcePath string) hooksWireRun {
	t.Helper()
	// 1. 要求先开始后完成，事件和 ID 相同，状态从 running 变为指定状态并带完成时间。
	if len(notifications) != 2 || notifications[0].method != "hook/started" || notifications[1].method != "hook/completed" {
		t.Fatalf("hook notifications = %+v, want one hook/started followed by one hook/completed for %s", notifications, eventName)
	}
	run := hooksWireRun{
		started:   notifications[0],
		completed: notifications[1],
	}
	started, completed := run.started.Run, run.completed.Run
	if started.ID == "" || started.ID != completed.ID || started.EventName != eventName || completed.EventName != eventName ||
		started.Status != "running" || completed.Status != status || completed.CompletedAt == nil || started.CompletedAt != nil {
		t.Errorf("hook run started %+v, completed %+v, want the same run of %s going from running to %s", started, completed, eventName, status)
	}
	// 2. 检查对话、Turn、脚本来源及同步命令运行信息。
	if run.started.TurnID == "" || run.started.TurnID != run.completed.TurnID || run.started.ThreadID != run.completed.ThreadID {
		t.Errorf("hook notifications = %+v, want the same thread and turn", notifications)
	}
	if completed.SourcePath != sourcePath || completed.HandlerType != "command" || completed.ExecutionMode != "sync" || completed.Scope != "turn" || completed.Source != "user" {
		t.Errorf("hook/completed run = %+v, want a sync command hook of scope turn from %s", completed, sourcePath)
	}
	return run
}

// assertListedHooks 检查 hook 列表的顺序、配置和信任状态。
func assertListedHooks(t *testing.T, got []hooksWireListedHook, want []hooksExpectedListing) {
	t.Helper()
	// 1. 逐项比较事件、matcher、命令、来源和哈希，检查默认超时为六十秒、启用、可信且非托管。
	if len(got) != len(want) {
		t.Fatalf("hooks/list = %+v, want %d hooks", got, len(want))
	}
	for index, hook := range got {
		matcher := ""
		if hook.Matcher != nil {
			matcher = *hook.Matcher
		}
		sourceName := strings.SplitN(want[index].key, ":", 2)[0] + ".yaml"
		if hook.Key != want[index].key || hook.EventName != want[index].eventName || matcher != want[index].matcher || hook.Command != want[index].command ||
			filepath.Base(hook.SourcePath) != sourceName || hook.Source != "user" || hook.TimeoutSec != 60 || !hook.Enabled || hook.IsManaged || hook.TrustStatus != "trusted" || hook.CurrentHash == "" {
			t.Errorf("hooks/list[%d] = %+v, want %+v from %s with default timeout, enabled, trusted and not managed", index, hook, want[index], sourceName)
		}
	}
}

// ── 假模型请求与脚本输入 ──

// modelMessageText 是假模型请求里的一条消息：角色和全部文字。
type modelMessageText struct {
	role, text string
}

// getModelMessageTexts 按顺序读取模型消息的角色和文字。
func getModelMessageTexts(body map[string]any) []modelMessageText {
	// 1. 直接读取字符串内容，分段内容则按顺序拼接各段 text。
	messages, _ := body["messages"].([]any)
	texts := make([]modelMessageText, 0, len(messages))
	for _, message := range messages {
		fields, _ := message.(map[string]any)
		role, _ := fields["role"].(string)
		var text strings.Builder
		switch content := fields["content"].(type) {
		case string:
			text.WriteString(content)
		case []any:
			for _, part := range content {
				partFields, _ := part.(map[string]any)
				partText, _ := partFields["text"].(string)
				text.WriteString(partText)
			}
		}
		texts = append(texts, modelMessageText{
			role: role,
			text: text.String(),
		})
	}
	return texts
}

// getHookInput 读取 hook 保存的标准输入 JSON。
func getHookInput(t *testing.T, path string) map[string]any {
	t.Helper()
	// 1. 读取文件并解码对象，读取或解码失败时结束测试。
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var input map[string]any
	if err := json.Unmarshal(payload, &input); err != nil {
		t.Fatalf("hook stdin %s: %v", payload, err)
	}
	return input
}

// prefixLines 给每个非空文本段添加行前缀。
func prefixLines(text, prefix string) string {
	// 1. 按保留换行的方式拆行，逐行添加前缀后拼接。
	var prefixed strings.Builder
	for _, line := range strings.SplitAfter(text, "\n") {
		if line != "" {
			prefixed.WriteString(prefix + line)
		}
	}
	return prefixed.String()
}

// toAnySlice 读取 JSON 数组的元素。
func toAnySlice(value any) []any {
	// 1. 将传入值转换为 any 切片，不是数组时返回 nil。
	elements, _ := value.([]any)
	return elements
}
