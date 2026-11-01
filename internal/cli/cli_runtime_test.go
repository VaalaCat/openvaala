package cli

import (
	"bytes"
	"cmp"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/internal/config"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"golang.org/x/crypto/ssh"
)

// TestServeRuntimeAssembly 检查 serve 组装后的工具、Skills、Executor、压缩和模型错误处理。
func TestServeRuntimeAssembly(t *testing.T) {
	// 1. 列出受控模型行为、文件配置和期望结果。
	tests := []struct {
		name string
		// setup 准备工作区、用户目录与配置目录中的文件，返回受控模型的回复方式；modelURL 是受控模型的地址。
		setup func(t *testing.T, home, workspace, configDirectory, modelURL string) modelReply
		// prompts 在同一对话中依次提交；为空时提交一次"读一下文件"。
		prompts []string
		// check 检查各 Turn 的结束事件与模型收到的全部请求（JSON 文本，按到达顺序）。
		check func(t *testing.T, finished []*runtimeclient.TurnFinished, requests []string)
		// wantErr 非空时 serve 应启动失败并带这段文字：skills.directory 里格式错误的 skill 在加载配置时报错。
		wantErr string
	}{
		{
			name: "workspace_tools_read_workspace_file",
			setup: func(t *testing.T, home, workspace, configDirectory, modelURL string) modelReply {
				writeTestFiles(t, map[string]string{filepath.Join(workspace, "notes.txt"): "workspace-marker"})
				return replyReadFile("notes.txt", "")
			},
			check: func(t *testing.T, _ []*runtimeclient.TurnFinished, requests []string) {
				first, second := requests[0], requests[1]
				for _, want := range []string{`"name":"bash"`, `"name":"read_file"`, `"name":"apply_patch"`} {
					if !strings.Contains(first, want) {
						t.Fatalf("first request lacks %s", want)
					}
				}
				if !strings.Contains(second, "workspace-marker") {
					t.Fatalf("read_file did not read the workspace file: %s", second)
				}
			},
		},
		{
			name: "workspace_tools_read_line_range",
			setup: func(t *testing.T, home, workspace, configDirectory, modelURL string) modelReply {
				writeTestFiles(t, map[string]string{filepath.Join(workspace, "notes.txt"): "before\n你好\nselected\nafter\n"})
				var calls atomic.Int32
				return func(w http.ResponseWriter, _ map[string]any) {
					w.Header().Set("Content-Type", "text/event-stream")
					if calls.Add(1) > 1 {
						fmt.Fprint(w, modelTestResponse)
						return
					}
					fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-read-lines","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"notes.txt\",\"offset\":2,\"limit\":2}"}}]}}]}`+"\n\n")
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
				}
			},
			check: func(t *testing.T, finished []*runtimeclient.TurnFinished, requests []string) {
				if len(requests) != 2 || finished[0].Status != defs.TurnStatusCompleted {
					t.Fatalf("model requests=%d, turn=%+v", len(requests), finished[0])
				}
				var request map[string]any
				if err := json.Unmarshal([]byte(requests[1]), &request); err != nil {
					t.Fatal(err)
				}
				result := getACPModelToolResult(t, request)
				if result.Error != nil {
					t.Fatal(result.Error)
				}
				var content struct {
					Content    string `json:"content"`
					Offset     int64  `json:"offset"`
					NextOffset int64  `json:"next_offset"`
					EOF        bool   `json:"eof"`
				}
				if err := json.Unmarshal(result.Content, &content); err != nil {
					t.Fatal(err)
				}
				if content.Content != "2: 你好\n3: selected\n" || content.Offset != 2 || content.NextOffset != 4 || content.EOF {
					t.Fatalf("model received read_file result = %+v", content)
				}
			},
		},
		{
			name: "workspace_skill_overrides_home",
			setup: func(t *testing.T, home, workspace, configDirectory, modelURL string) modelReply {
				workspaceSkill := filepath.Join(workspace, ".agents", "skills", "review", "SKILL.md")
				writeTestFiles(t, map[string]string{
					workspaceSkill: "---\nname: review\ndescription: workspace review\n---\nworkspace-skill-body\n",
					filepath.Join(home, ".agents", "skills", "review", "SKILL.md"): "---\nname: review\ndescription: home review\n---\nhome-skill-body\n",
					filepath.Join(home, ".claude", "skills", "zeta", "SKILL.md"):   "---\nname: zeta\ndescription: zeta skill\n---\n",
				})
				return replyReadFile(workspaceSkill, "")
			},
			check: func(t *testing.T, _ []*runtimeclient.TurnFinished, requests []string) {
				first, second := requests[0], requests[1]
				if !strings.Contains(first, "- review: workspace review") || strings.Contains(first, "home review") || !strings.Contains(first, "- zeta: zeta skill") {
					t.Fatalf("system prompt skill list is wrong: %s", first)
				}
				if !strings.Contains(second, "workspace-skill-body") {
					t.Fatalf("read_file did not return the workspace skill: %s", second)
				}
			},
		},
		{
			name: "configured_executor_selected",
			setup: func(t *testing.T, home, workspace, configDirectory, modelURL string) modelReply {
				sandbox := t.TempDir()
				writeTestFiles(t, map[string]string{
					filepath.Join(workspace, "notes.txt"):                       "local-marker",
					filepath.Join(sandbox, "notes.txt"):                         "sandbox-marker",
					filepath.Join(configDirectory, "executors", "sandbox.yaml"): "id: sandbox\nkind: local\ndescription: 试验目录\nworkspace_directory: " + sandbox + "\n",
				})
				return replyReadFile("notes.txt", "sandbox")
			},
			check: func(t *testing.T, _ []*runtimeclient.TurnFinished, requests []string) {
				first, second := requests[0], requests[1]
				if !strings.Contains(first, `"enum":["local","openvaala","sandbox"]`) {
					t.Fatalf("request lacks executor parameter: %s", first)
				}
				if !strings.Contains(second, "sandbox-marker") || strings.Contains(second, "local-marker") {
					t.Fatalf("read_file did not run on the selected executor: %s", second)
				}
			},
		},
		{
			name: "offline_ssh_does_not_fail_conversation",
			setup: func(t *testing.T, home, workspace, configDirectory, modelURL string) modelReply {
				t.Setenv("SSH_AUTH_SOCK", "")
				_, key, err := ed25519.GenerateKey(rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				block, err := ssh.MarshalPrivateKey(key, "")
				if err != nil {
					t.Fatal(err)
				}
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				address := listener.Addr().String()
				var connections atomic.Int32
				var tasks sync.WaitGroup
				tasks.Go(func() {
					for {
						connection, err := listener.Accept()
						if err != nil {
							return
						}
						connections.Add(1)
						_ = connection.Close()
					}
				})
				t.Cleanup(func() {
					_ = listener.Close()
					tasks.Wait()
				})
				identityPath := filepath.Join(home, ".ssh", "test_key")
				writeTestFiles(t, map[string]string{
					identityPath:                                               string(pem.EncodeToMemory(block)),
					filepath.Join(workspace, "notes.txt"):                      "local-file-marker",
					filepath.Join(workspace, "AGENTS.md"):                      "local-rules-marker",
					filepath.Join(configDirectory, "executors", "remote.yaml"): "id: remote\nkind: ssh\ndescription: 离线测试环境\nworkspace_directory: /project\nssh:\n  address: test@" + address + "\n  identity_file: " + identityPath + "\n",
				})
				localRead := replyReadFile("notes.txt", "local")
				remoteRead := replyReadFile("notes.txt", "remote")
				var calls atomic.Int32
				return func(w http.ResponseWriter, request map[string]any) {
					w.Header().Set("Content-Type", "text/event-stream")
					call := calls.Add(1)
					wantConnections := int32(0)
					if call >= 5 {
						wantConnections = 1
					}
					if got := connections.Load(); got != wantConnections {
						t.Errorf("model call %d: SSH connections = %d, want %d", call, got, wantConnections)
					}
					switch call {
					case 2:
						localRead(w, request)
					case 4:
						remoteRead(w, request)
					default:
						fmt.Fprint(w, modelTestResponse)
					}
				}
			},
			prompts: []string{"你好", "读本地文件", "读远端文件", "继续对话"},
			check: func(t *testing.T, finished []*runtimeclient.TurnFinished, requests []string) {
				for _, turn := range finished {
					if turn.Status != defs.TurnStatusCompleted {
						t.Fatalf("turn finished = %+v", turn)
					}
				}
				if len(requests) != 6 {
					t.Fatalf("model requests = %d, want 6", len(requests))
				}
				if !strings.Contains(requests[0], "local-rules-marker") || !strings.Contains(requests[2], "local-file-marker") {
					t.Fatal("local rules or file missing from model requests")
				}
				if !strings.Contains(requests[4], "read_failed") || !strings.Contains(requests[4], "ssh: handshake failed") {
					t.Fatalf("remote read error missing from model request: %s", requests[4])
				}
			},
		},
		{
			name: "builtin_skill_readable",
			setup: func(t *testing.T, home, workspace, configDirectory, modelURL string) modelReply {
				return replyReadFile("/skills/openvaala/SKILL.md", "openvaala")
			},
			check: func(t *testing.T, _ []*runtimeclient.TurnFinished, requests []string) {
				first, second := requests[0], requests[1]
				if !strings.Contains(first, "- openvaala:") || !strings.Contains(second, "name: openvaala") {
					t.Fatalf("builtin skill not listed or not readable:\n%s\n%s", first, second)
				}
			},
		},
		{
			name: "invalid_private_skill_fails",
			setup: func(t *testing.T, home, workspace, configDirectory, modelURL string) modelReply {
				writeTestFiles(t, map[string]string{filepath.Join(configDirectory, "skills", "broken", "SKILL.md"): "# 没有 frontmatter\n"})
				return replyReadFile("notes.txt", "")
			},
			wantErr: filepath.Join("skills", "broken", "SKILL.md"),
		},
		{
			// 历史超过配置窗口时调用压缩模型，后续请求使用摘要和近期内容。
			name: "context_window_compacts_history",
			setup: func(t *testing.T, home, workspace, configDirectory, modelURL string) modelReply {
				writeTestFiles(t, map[string]string{
					filepath.Join(configDirectory, "models", "local.yaml"): "id: local\nprotocol: openai_chat\nbase_url: " + modelURL + "\nmodels:\n" +
						"  default:\n    upstream_model: main-model\n    context_window: 10000\n" +
						"  summary:\n    upstream_model: summary-model\n    context_window: 10000\n",
					filepath.Join(configDirectory, "agents", "assistant.yaml"): "name: assistant\ninstructions: Be concise.\nmodel:\n  provider: local\n" +
						"compact_model:\n  provider: local\n  model: summary\n",
				})
				return replyCompacting()
			},
			prompts: []string{"当前电脑cpu是几核心的", "真的吗", "你深入仔细看看呢"},
			check: func(t *testing.T, finished []*runtimeclient.TurnFinished, requests []string) {
				for _, turn := range finished {
					if turn.Status != defs.TurnStatusCompleted {
						t.Fatalf("turn finished = %+v", turn)
					}
				}
				summaries, lastNormal := 0, ""
				for _, request := range requests {
					if strings.Contains(request, `"tool_choice":"none"`) {
						summaries++
						if !strings.Contains(request, `"model":"summary-model"`) {
							t.Fatalf("summary request does not use the compact model: %s", request)
						}
						continue
					}
					if !strings.Contains(request, `"model":"main-model"`) {
						t.Fatalf("normal request does not use the main model: %s", request)
					}
					lastNormal = request
				}
				if summaries != 1 {
					t.Fatalf("summary requests = %d, want 1", summaries)
				}
				if strings.Contains(lastNormal, "first chunk") || !strings.Contains(lastNormal, "CLI history summary") || !strings.Contains(lastNormal, "recent chunk") {
					t.Fatal("normal model request did not use compacted history")
				}
			},
		},
		{
			name: "model_failure_retries_same_request",
			setup: func(t *testing.T, home, workspace, configDirectory, modelURL string) modelReply {
				var calls atomic.Int32
				return func(w http.ResponseWriter, _ map[string]any) {
					if calls.Add(1) == 1 {
						http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, modelTestResponse)
				}
			},
			check: func(t *testing.T, finished []*runtimeclient.TurnFinished, requests []string) {
				if finished[0].Status != defs.TurnStatusCompleted || finished[0].Error != "" {
					t.Fatalf("turn finished = %+v, want recovery within the same turn", finished[0])
				}
				if len(requests) != 2 || requests[0] != requests[1] {
					t.Fatalf("retry requests = %v, want two identical requests", requests)
				}
			},
		},
		{
			// 明确不可重试的模型错误让 Turn 以 failed 结束，经 serve 与 runtimeclient 交付。
			name: "model_failure_fails_turn",
			setup: func(t *testing.T, home, workspace, configDirectory, modelURL string) modelReply {
				return func(w http.ResponseWriter, _ map[string]any) {
					http.Error(w, "injected model error", http.StatusBadRequest)
				}
			},
			check: func(t *testing.T, finished []*runtimeclient.TurnFinished, requests []string) {
				if finished[0].Status != defs.TurnStatusFailed || finished[0].Error == "" {
					t.Fatalf("turn finished = %+v, want failed with an error", finished[0])
				}
				if len(requests) != 1 {
					t.Fatalf("model requests = %d, want 1", len(requests))
				}
			},
		},
	}
	// 2. 准备工作区和模型服务，执行对话或启动失败场景，检查 Turn 和模型请求。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home, workspace := t.TempDir(), t.TempDir()
			t.Setenv("HOME", home)
			t.Chdir(workspace)
			model := &scriptedModel{requests: make(chan map[string]any, 16)}
			server := httptest.NewServer(model)
			defer server.Close()
			configPath := writeTestConfig(t, server.URL, "assistant")
			model.reply = tt.setup(t, home, workspace, filepath.Dir(configPath), server.URL)
			if tt.wantErr != "" {
				appendServerConfig(t, configPath, getFreeServeAddress(t), serveTestToken)
				if _, err := runCLI(t, configPath, "serve"); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("serve error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			prompts := tt.prompts
			if len(prompts) == 0 {
				prompts = []string{"读一下文件"}
			}
			finished := runServePrompts(t, configPath, prompts...)
			tt.check(t, finished, model.getRequestTexts())
		})
	}
}

// 组装链测试夹具 ----------------------------------------------------------------

// modelTestResponse 是受控模型的普通回复。
const modelTestResponse = `data: {"choices":[{"delta":{"content":"你好。"}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`

// writeTestConfig 写入测试应用、Agent 和模型配置并返回配置路径。
func writeTestConfig(t *testing.T, serverURL, agentName string) string {
	t.Helper()
	// 1. 在临时目录写入 config.yaml 及 Agent、模型文件。
	directory := t.TempDir()
	writeTestFiles(t, map[string]string{
		filepath.Join(directory, "config.yaml"):               "run:\n  agent_name: " + agentName + "\n",
		filepath.Join(directory, "agents", agentName+".yaml"): "name: " + agentName + "\ninstructions: Be concise.\nmodel:\n  provider: local\n",
		filepath.Join(directory, "models", "local.yaml"):      fmt.Sprintf("id: local\nprotocol: openai_chat\nbase_url: %s\nmodels:\n  default: {}\n", serverURL),
	})
	return filepath.Join(directory, "config.yaml")
}

// writeTestFiles 按指定路径写入测试文件。
func writeTestFiles(t *testing.T, files map[string]string) {
	t.Helper()
	// 1. 以 0700 创建父目录，以 0600 写入文件。
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

// requestText 将模型请求编码为不转义 HTML 字符的 JSON。
func requestText(request map[string]any) string {
	// 1. 关闭 HTML 转义，编码请求并返回文本。
	var text bytes.Buffer
	encoder := json.NewEncoder(&text)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(request); err != nil {
		panic(err)
	}
	return text.String()
}

// modelReply 决定受控模型如何回复一次请求。
type modelReply func(w http.ResponseWriter, request map[string]any)

// scriptedModel 是组装链测试的受控模型：按到达顺序记录请求，由 reply 回复。
type scriptedModel struct {
	reply    modelReply
	requests chan map[string]any
}

// ServeHTTP 读取并记录模型请求，再按测试配置回复。
func (m *scriptedModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 1. 解析 JSON，请求格式错误时返回 400；成功时记录请求并执行回复函数。
	var request map[string]any
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	m.requests <- request
	m.reply(w, request)
}

// getRequestTexts 取出已收到的模型请求文本。
func (m *scriptedModel) getRequestTexts() []string {
	// 1. 在 Turn 全部结束后不阻塞地读完请求通道，逐条编码后返回。
	var texts []string
	for {
		select {
		case request := <-m.requests:
			texts = append(texts, requestText(request))
		default:
			return texts
		}
	}
}

// replyReadFile 创建首次调用文件工具、后续直接回复的模型行为。
func replyReadFile(path, executor string) modelReply {
	// 1. 建立调用计数并返回回复函数。
	var calls atomic.Int32
	return func(w http.ResponseWriter, _ map[string]any) {
		// 2. 按调用次数选择普通回复或带可选 Executor 的 read_file 调用。
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) > 1 {
			fmt.Fprint(w, modelTestResponse)
			return
		}
		arguments := map[string]string{"path": path}
		if executor != "" {
			arguments["executor"] = executor
		}
		argumentsJSON, _ := json.Marshal(arguments)
		encodedArguments, _ := json.Marshal(string(argumentsJSON))
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":%q,\"type\":\"function\",\"function\":{\"name\":\"read_file\",\"arguments\":%s}}]}}]}\n\n", "call-read-"+executor, encodedArguments)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
	}
}

// replyCompacting 创建带长文本、工具调用和固定压缩摘要的模型行为。
func replyCompacting() modelReply {
	// 1. 建立普通请求计数并返回回复函数。
	var normalCalls atomic.Int32
	return func(w http.ResponseWriter, request map[string]any) {
		// 2. 摘要请求返回固定摘要，前两次普通请求输出长文本并调用 bash，其余直接回复。
		w.Header().Set("Content-Type", "text/event-stream")
		if request["tool_choice"] == "none" {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"CLI history summary\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		call := normalCalls.Add(1)
		if call > 2 {
			fmt.Fprint(w, modelTestResponse)
			return
		}
		text := strings.Repeat("first chunk ", 600)
		if call == 2 {
			text = strings.Repeat("recent chunk ", 2000)
		}
		encoded, _ := json.Marshal(text)
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%s}}]}\n\n", encoded)
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-%d\",\"type\":\"function\",\"function\":{\"name\":\"bash\",\"arguments\":\"{\\\"command\\\":\\\"printf context-test\\\"}\"}}]}}]}\n\n", call)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
	}
}

// buildTestBinary 把 openvaala 编译到测试临时目录并返回路径。
func buildTestBinary(t *testing.T) string {
	t.Helper()
	// 1. 执行 go build，编译失败时输出构建结果。
	binary := filepath.Join(t.TempDir(), "openvaala")
	build := exec.Command("go", "build", "-o", binary, "../../cmd/openvaala")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	return binary
}

// TestEnsureRuntimeServer 检查本机 serve 自动启动、复用和并发启动等待。
func TestEnsureRuntimeServer(t *testing.T) {
	// 1. 编译程序并列出启动场景。
	binary := buildTestBinary(t)
	tests := []struct {
		name string
		// run 在临时 HOME 下执行场景：args 指向编出来的二进制和空闲端口 listen；结束后统一执行 serve stop。
		run func(t *testing.T, args *RuntimeServerStartOpt, listen string)
	}{
		{
			name: "starts_serve_when_missing",
			run: func(t *testing.T, args *RuntimeServerStartOpt, listen string) {
				file, err := EnsureRuntimeServer(t.Context(), args)
				if err != nil {
					t.Fatal(err)
				}
				if want := "ws://" + listen + "/api/v1/openvaala"; file.URL != want {
					t.Fatalf("url = %q, want %q", file.URL, want)
				}
				if onDisk := getTestRuntimeServerFile(t); onDisk.URL != file.URL || onDisk.Token != file.Token {
					t.Fatalf("runtime server file = %+v, want the returned %+v", onDisk, file)
				}
				response, err := http.Get("http://" + listen + "/api/v1/healthz")
				if err != nil {
					t.Fatal(err)
				}
				_ = response.Body.Close()
				if response.StatusCode != http.StatusOK {
					t.Fatalf("healthz status = %d, want 200", response.StatusCode)
				}
			},
		},
		{
			name: "reuses_running_serve",
			run: func(t *testing.T, args *RuntimeServerStartOpt, listen string) {
				first, err := EnsureRuntimeServer(t.Context(), args)
				if err != nil {
					t.Fatal(err)
				}
				// 将第二次调用的可执行文件指向不存在的路径，检查已有 serve 仍可复用。
				reuse := *args
				reuse.Executable = filepath.Join(t.TempDir(), "missing")
				second, err := EnsureRuntimeServer(t.Context(), &reuse)
				if err != nil {
					t.Fatal(err)
				}
				if second.URL != first.URL || second.Token != first.Token {
					t.Fatalf("second = %+v, want the first %+v", second, first)
				}
			},
		},
		{
			name: "stale_file_restarts",
			run: func(t *testing.T, args *RuntimeServerStartOpt, listen string) {
				staleURL := "ws://" + getFreeServeAddress(t) + "/api/v1/openvaala"
				writeTestRuntimeServerFile(t, RuntimeServerConnection{
					URL:   staleURL,
					Token: "stale-token",
				})
				file, err := EnsureRuntimeServer(t.Context(), args)
				if err != nil {
					t.Fatal(err)
				}
				if want := "ws://" + listen + "/api/v1/openvaala"; file.URL != want {
					t.Fatalf("url = %q, want %q", file.URL, want)
				}
				if onDisk := getTestRuntimeServerFile(t); onDisk.URL != file.URL || onDisk.Token != file.Token {
					t.Fatalf("runtime server file = %+v, want the rewritten %+v", onDisk, file)
				}
			},
		},
		{
			name: "unknown_executable_fails",
			run: func(t *testing.T, args *RuntimeServerStartOpt, listen string) {
				args.Executable = ""
				_, err := EnsureRuntimeServer(t.Context(), args)
				if err == nil || !strings.Contains(err.Error(), "the openvaala executable is unknown") {
					t.Fatalf("error = %v, want the unknown executable error", err)
				}
			},
		},
		{
			name: "serve_exits_early",
			run: func(t *testing.T, args *RuntimeServerStartOpt, listen string) {
				occupied, err := net.Listen("tcp", listen)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = occupied.Close() })
				started := time.Now()
				_, err = EnsureRuntimeServer(t.Context(), args)
				if err == nil || !strings.Contains(err.Error(), "runtime server exited (exit status 1); run `openvaala serve` to see the error") {
					t.Fatalf("error = %v, want the serve exit error", err)
				}
				if elapsed := time.Since(started); elapsed > 10*time.Second {
					t.Fatalf("returned after %s, want within 10s", elapsed)
				}
			},
		},
		{
			name: "waits_for_serve_starting_elsewhere",
			run: func(t *testing.T, args *RuntimeServerStartOpt, listen string) {
				// 占用端口并持有数据目录锁，健康检查在 1 秒后可用，模拟另一个 serve 正在启动。
				occupied, err := net.Listen("tcp", listen)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = occupied.Close() })
				dataDirectory, err := ensureDataDirectory()
				if err != nil {
					t.Fatal(err)
				}
				unlock, err := lockDataDirectory(dataDirectory)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = unlock() })
				readyAt := time.Now().Add(time.Second)
				other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if time.Now().Before(readyAt) {
						w.WriteHeader(http.StatusServiceUnavailable)
					}
				}))
				t.Cleanup(other.Close)
				want := RuntimeServerConnection{
					URL:   "ws://" + strings.TrimPrefix(other.URL, "http://") + "/api/v1/openvaala",
					Token: "other-token",
				}
				writeTestRuntimeServerFile(t, want)
				file, err := EnsureRuntimeServer(t.Context(), args)
				if err != nil {
					t.Fatal(err)
				}
				if file.URL != want.URL || file.Token != want.Token {
					t.Fatalf("file = %+v, want the starting serve %+v", file, want)
				}
			},
		},
	}
	// 2. 在独立 HOME 下执行场景，结束时使用独立 context 停止 serve。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			listen := getFreeServeAddress(t)
			configPath := newServeConfig(t, "http://127.0.0.1:1", listen, "")
			cfg, err := config.LoadConfig(&config.LoadConfigOpt{Path: configPath})
			if err != nil {
				t.Fatal(err)
			}
			// 收尾时使用独立 context 停止 serve 进程。
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				if _, err := runCLIWithContext(ctx, configPath, "serve", "stop"); err != nil {
					t.Errorf("serve stop: %v", err)
				}
			})
			tt.run(t, &RuntimeServerStartOpt{
				LogFile:     cfg.Telemetry.LogFile,
				IdleTimeout: runtimeServerIdleTimeout,
				Executable:  binary,
				ConfigFlags: []string{"--config-file=" + configPath},
			}, listen)
		})
	}
}

// TestNewRuntimeServerCommandIdleTimeout 检查自动拉起 serve 的 --idle-timeout：为 0 时不带，大于 0 时写在 serve 之后。
func TestNewRuntimeServerCommandIdleTimeout(t *testing.T) {
	// 1. 列出常驻和带空闲超时的场景。
	tests := []struct {
		name        string
		idleTimeout time.Duration
		want        []string
	}{
		{
			name: "resident_without_idle_timeout",
			want: []string{"/opt/openvaala", "--agent=x", "serve"},
		},
		{
			name:        "idle_timeout_10s",
			idleTimeout: 10 * time.Second,
			want:        []string{"/opt/openvaala", "--agent=x", "serve", "--idle-timeout", "10s"},
		},
	}
	// 2. 逐场景创建命令，检查完整参数。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			command, err := newRuntimeServerCommand(&RuntimeServerStartOpt{
				Executable:  "/opt/openvaala",
				ConfigFlags: []string{"--agent=x"},
				IdleTimeout: tt.idleTimeout,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(command.Args, tt.want) {
				t.Fatalf("command args = %v, want %v", command.Args, tt.want)
			}
		})
	}
}

// TestIsRuntimeServerFromCurrentExecutable 检查 serve 的可执行文件路径和修改时间比较。
func TestIsRuntimeServerFromCurrentExecutable(t *testing.T) {
	// 1. 读取当前文件信息并列出匹配、不匹配和缺失信息场景。
	path, modifiedAt, err := getCurrentExecutable()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		file RuntimeServerConnection
		want bool
	}{
		{
			name: "same_executable",
			file: RuntimeServerConnection{
				Executable:           path,
				ExecutableModifiedAt: modifiedAt,
			},
			want: true,
		},
		{
			name: "other_path",
			file: RuntimeServerConnection{
				Executable:           path + ".old",
				ExecutableModifiedAt: modifiedAt,
			},
			want: false,
		},
		{
			name: "replaced_file",
			file: RuntimeServerConnection{
				Executable:           path,
				ExecutableModifiedAt: modifiedAt.Add(-time.Second),
			},
			want: false,
		},
		{
			name: "empty_fields",
			file: RuntimeServerConnection{},
			want: true,
		},
	}
	// 2. 检查各连接文件对应的文件比较结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRuntimeServerFromCurrentExecutable(t.Context(), tt.file); got != tt.want {
				t.Fatalf("isRuntimeServerFromCurrentExecutable(%+v) = %v, want %v", tt.file, got, tt.want)
			}
		})
	}
}

// TestGetRuntimeConnection 检查连接来源、token 优先级和本机标识。
func TestGetRuntimeConnection(t *testing.T) {
	// 1. 启动健康检查服务并准备本机连接文件样例。
	// 使用返回 HTTP 200 的服务模拟已有本机 serve。
	localServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(localServer.Close)
	localURL := "ws://" + strings.TrimPrefix(localServer.URL, "http://") + "/api/v1/openvaala"
	const (
		remoteURL = "ws://example.com:8790/api/v1/openvaala"
		configURL = "ws://10.0.0.5:8790/api/v1/openvaala"
	)
	localFile := &RuntimeServerConnection{
		URL:      localURL,
		Token:    "file-token",
		ServerID: "file-server-id",
	}
	otherExecutableFile := &RuntimeServerConnection{
		URL:                  localURL,
		Token:                "file-token",
		ServerID:             "file-server-id",
		Executable:           "/nonexistent/openvaala",
		ExecutableModifiedAt: time.Unix(1, 0),
	}
	unreachableURL := "ws://" + getFreeServeAddress(t) + "/api/v1/openvaala"
	tests := []struct {
		name       string
		runtimeURL string // --url
		clientURL  string
		// clientToken 是配置 client.token。
		clientToken string
		// file 是 runtime-server.yaml 的内容，为 nil 时不写文件；unreadableFile 为 true 时写入无法解析的内容。
		file           *RuntimeServerConnection
		unreadableFile bool
		want           runtimeConnection
	}{
		{
			name:       "flag_wins_over_client_url",
			runtimeURL: remoteURL,
			clientURL:  configURL,
			file:       localFile,
			want: runtimeConnection{
				URL:           remoteURL,
				Source:        runtimeConnectionSourceFlag,
				LocalServerID: "file-server-id",
			},
		},
		{
			name:      "client_url_wins_over_local_serve",
			clientURL: configURL,
			file:      localFile,
			want: runtimeConnection{
				URL:           configURL,
				Source:        runtimeConnectionSourceConfig,
				LocalServerID: "file-server-id",
			},
		},
		{
			name: "local_serve_when_no_url_is_given",
			file: localFile,
			want: runtimeConnection{
				URL:           localURL,
				Token:         "file-token",
				Source:        runtimeConnectionSourceLocal,
				LocalServerID: "file-server-id",
			},
		},
		{
			name:        "local_serve_uses_file_token_not_client_token",
			clientToken: "from-config",
			file:        localFile,
			want: runtimeConnection{
				URL:           localURL,
				Token:         "file-token",
				Source:        runtimeConnectionSourceLocal,
				LocalServerID: "file-server-id",
			},
		},
		{
			name: "local_serve_from_other_executable_is_flagged",
			file: otherExecutableFile,
			want: runtimeConnection{
				URL:                   localURL,
				Token:                 "file-token",
				Source:                runtimeConnectionSourceLocal,
				LocalServerID:         "file-server-id",
				IsFromOtherExecutable: true,
			},
		},
		{
			// 提示只针对本机这一级：--url 指向的 serve 不检查可执行文件。
			name:       "flag_url_is_not_checked_for_other_executable",
			runtimeURL: localURL,
			file:       otherExecutableFile,
			want: runtimeConnection{
				URL:           localURL,
				Token:         "file-token",
				Source:        runtimeConnectionSourceFlag,
				LocalServerID: "file-server-id",
			},
		},
		{
			name:        "url_token_leaves_token_empty",
			runtimeURL:  remoteURL + "?token=from-url",
			clientToken: "from-config",
			file:        localFile,
			want: runtimeConnection{
				URL:           remoteURL + "?token=from-url",
				Source:        runtimeConnectionSourceFlag,
				LocalServerID: "file-server-id",
			},
		},
		{
			name:        "empty_url_token_still_leaves_token_empty",
			runtimeURL:  remoteURL + "?token=",
			clientToken: "from-config",
			file:        localFile,
			want: runtimeConnection{
				URL:           remoteURL + "?token=",
				Source:        runtimeConnectionSourceFlag,
				LocalServerID: "file-server-id",
			},
		},
		{
			// URL 无法解析时使用 client.token，不补连接文件的 token。
			name:        "unparsable_url_uses_client_token",
			runtimeURL:  "ws://%zz",
			clientToken: "from-config",
			file:        localFile,
			want: runtimeConnection{
				URL:           "ws://%zz",
				Token:         "from-config",
				Source:        runtimeConnectionSourceFlag,
				LocalServerID: "file-server-id",
			},
		},
		{
			name:       "url_token_wins_over_file_token_for_local_serve",
			runtimeURL: localURL + "?token=from-url",
			file:       localFile,
			want: runtimeConnection{
				URL:           localURL + "?token=from-url",
				Source:        runtimeConnectionSourceFlag,
				LocalServerID: "file-server-id",
			},
		},
		{
			name:        "client_token_is_used_for_client_url",
			clientURL:   configURL,
			clientToken: "from-config",
			file:        localFile,
			want: runtimeConnection{
				URL:           configURL,
				Token:         "from-config",
				Source:        runtimeConnectionSourceConfig,
				LocalServerID: "file-server-id",
			},
		},
		{
			name:        "client_token_wins_over_file_token_for_local_serve_url",
			runtimeURL:  localURL,
			clientToken: "from-config",
			file:        localFile,
			want: runtimeConnection{
				URL:           localURL,
				Token:         "from-config",
				Source:        runtimeConnectionSourceFlag,
				LocalServerID: "file-server-id",
			},
		},
		{
			name:       "file_token_is_used_for_local_serve_url_without_any_token",
			runtimeURL: localURL,
			file:       localFile,
			want: runtimeConnection{
				URL:           localURL,
				Token:         "file-token",
				Source:        runtimeConnectionSourceFlag,
				LocalServerID: "file-server-id",
			},
		},
		{
			name:      "file_token_is_used_for_local_serve_client_url_without_any_token",
			clientURL: localURL,
			file:      localFile,
			want: runtimeConnection{
				URL:           localURL,
				Token:         "file-token",
				Source:        runtimeConnectionSourceConfig,
				LocalServerID: "file-server-id",
			},
		},
		{
			name:       "file_token_is_not_sent_to_other_servers",
			runtimeURL: remoteURL,
			file:       localFile,
			want: runtimeConnection{
				URL:           remoteURL,
				Source:        runtimeConnectionSourceFlag,
				LocalServerID: "file-server-id",
			},
		},
		{
			// 显式地址不可访问且连接文件不存在时，仍返回地址，不启动 serve。
			name:       "flag_url_never_starts_serve",
			runtimeURL: unreachableURL,
			want: runtimeConnection{
				URL:    unreachableURL,
				Source: runtimeConnectionSourceFlag,
			},
		},
		{
			name:      "client_url_never_starts_serve",
			clientURL: unreachableURL,
			want: runtimeConnection{
				URL:    unreachableURL,
				Source: runtimeConnectionSourceConfig,
			},
		},
		{
			// 连接文件无法读取时仍使用显式连接配置，本机标识和文件令牌为空。
			name:           "unreadable_file_has_no_local_server_id",
			runtimeURL:     localURL,
			clientToken:    "from-config",
			unreadableFile: true,
			want: runtimeConnection{
				URL:    localURL,
				Token:  "from-config",
				Source: runtimeConnectionSourceFlag,
			},
		},
	}
	// 2. 按场景写入连接文件和配置，检查选中的地址、令牌及本机标识。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			if tt.file != nil {
				writeTestRuntimeServerFile(t, *tt.file)
			}
			if tt.unreadableFile {
				writeTestFiles(t, map[string]string{getTestRuntimeServerFilePath(t): "url: [unclosed"})
			}
			cfg, err := config.LoadConfig(&config.LoadConfigOpt{Path: writeTestConfig(t, "http://127.0.0.1:1", "assistant")})
			if err != nil {
				t.Fatal(err)
			}
			cfg.Client.URL, cfg.Client.Token = tt.clientURL, tt.clientToken
			args := &AppArgs{
				Config:     cfg,
				RuntimeURL: tt.runtimeURL,
				Executable: filepath.Join(t.TempDir(), "missing"),
			}
			got, err := getRuntimeConnection(t.Context(), args, runtimeServerIdleTimeout)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("getRuntimeConnection() = %+v, want %+v", got, tt.want)
			}
			if tt.file == nil && !tt.unreadableFile {
				if _, found, err := getRuntimeServerFile(); err != nil || found {
					t.Fatalf("runtime server file found = %v, err = %v, want no serve to be started", found, err)
				}
			}
		})
	}
}

// TestIsSameRuntimeServer 检查 serve 地址和端口匹配。
func TestIsSameRuntimeServer(t *testing.T) {
	// 1. 列出回环、远端、不同端口和无效地址场景。
	tests := []struct {
		name       string
		runtimeURL string
		// fileURL 是 runtime-server.yaml 里的 url；为空时用 ws://127.0.0.1:8790/api/v1/openvaala。
		fileURL string
		want    bool
	}{
		{
			name:       "same_loopback_ip",
			runtimeURL: "ws://127.0.0.1:8790/api/v1/openvaala",
			want:       true,
		},
		{
			name:       "localhost",
			runtimeURL: "ws://localhost:8790/api/v1/openvaala",
			want:       true,
		},
		{
			name:       "ipv6_loopback",
			runtimeURL: "ws://[::1]:8790/api/v1/openvaala",
			want:       true,
		},
		{
			name:       "token_query_ignored",
			runtimeURL: "ws://127.0.0.1:8790/api/v1/openvaala?token=abc",
			want:       true,
		},
		{
			name:       "different_port",
			runtimeURL: "ws://127.0.0.1:8791/api/v1/openvaala",
			want:       false,
		},
		{
			name:       "lan_address",
			runtimeURL: "ws://192.168.1.5:8790/api/v1/openvaala",
			want:       false,
		},
		{
			name:       "remote_host",
			runtimeURL: "ws://example.com:8790/api/v1/openvaala",
			want:       false,
		},
		{
			name:       "unparsable_url",
			runtimeURL: "ws://%zz",
			want:       false,
		},
		{
			name:       "unparsable_file_url",
			runtimeURL: "ws://127.0.0.1:8790",
			fileURL:    "ws://%zz",
			want:       false,
		},
	}
	// 2. 比较地址并检查匹配结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fileURL := cmp.Or(tt.fileURL, "ws://127.0.0.1:8790/api/v1/openvaala")
			if got := isSameRuntimeServer(tt.runtimeURL, fileURL); got != tt.want {
				t.Fatalf("isSameRuntimeServer(%q, %q) = %v, want %v", tt.runtimeURL, fileURL, got, tt.want)
			}
		})
	}
}

// TestEnsureRuntimeServerTimeout 检查启动超时分别给出日志位置或直接运行 serve 的诊断提示。
func TestEnsureRuntimeServerTimeout(t *testing.T) {
	// 1. 持有真实数据目录锁，模拟另一个尚未就绪的启动进程。
	for _, logFile := range []string{"", "/tmp/openvaala-test.log"} {
		t.Run(map[bool]string{true: "without_log", false: "with_log"}[logFile == ""], func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			directory, err := ensureDataDirectory()
			if err != nil {
				t.Fatal(err)
			}
			unlock, err := lockDataDirectory(directory)
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			executable, err := exec.LookPath("true")
			if err != nil {
				t.Fatal(err)
			}
			// 2. 竞争启动进程立即结束但锁仍在，等待期限后返回明确诊断文本。
			_, err = EnsureRuntimeServer(t.Context(), &RuntimeServerStartOpt{Executable: executable, LogFile: logFile})
			want := "run `openvaala serve` to see the error"
			if logFile != "" {
				want = "see " + logFile
			}
			if err == nil || !strings.Contains(err.Error(), "did not become ready") || !strings.HasSuffix(err.Error(), want) {
				t.Fatalf("timeout = %v, want %s", err, want)
			}
		})
	}
}
