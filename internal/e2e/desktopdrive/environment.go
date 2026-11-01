package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/defs"
)

const (
	// serveStartTimeout 是等 serve 健康检查通过的最长时间。
	serveStartTimeout = 30 * time.Second
	// seedTimeout 是预置数据等所有 Turn 结束的最长时间。
	seedTimeout = 2 * time.Minute
	// processStopTimeout 是 SIGTERM 之后等进程退出的最长时间，超时后 SIGKILL。
	processStopTimeout = 8 * time.Second
)

// childProcess 是 daemon 启动的一个后台进程；它独占一个进程组，停止时整组一起收尾。
type childProcess struct {
	command *exec.Cmd
	// exited 在进程退出后关闭。
	exited chan struct{}
}

// startChild 把 command 放进新进程组启动，后台等待它退出。
func startChild(command *exec.Cmd) (*childProcess, error) {
	// 1. 独立进程组，桌面再起的 serve 等孙进程也能一起被停掉。
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return nil, err
	}
	child := &childProcess{command: command, exited: make(chan struct{})}
	go func() {
		_ = command.Wait()
		close(child.exited)
	}()
	return child, nil
}

// stop 给整个进程组发 SIGTERM 并等它退出，超时后 SIGKILL；已经退出时什么都不做。
func (c *childProcess) stop() {
	// 1. 先礼貌退出，再强杀。
	select {
	case <-c.exited:
		return
	default:
	}
	_ = syscall.Kill(-c.command.Process.Pid, syscall.SIGTERM)
	select {
	case <-c.exited:
	case <-time.After(processStopTimeout):
		_ = syscall.Kill(-c.command.Process.Pid, syscall.SIGKILL)
		<-c.exited
	}
}

// getChildEnvironment 返回子进程的环境：HOME 换成临时目录，去掉 OPENVAALA_*、XDG_*、CODEX_HOME、PASEO_HOME 和显示变量，再追加 extra。
func getChildEnvironment(home string, extra ...string) []string {
	// 1. 复制当前环境并过滤。
	environment := []string{"HOME=" + home}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch {
		case name == "HOME", name == "CODEX_HOME", name == "PASEO_HOME", name == "DISPLAY", name == "WAYLAND_DISPLAY", name == "MYGO_AUTOMATION":
			continue
		case strings.HasPrefix(name, "OPENVAALA_"), strings.HasPrefix(name, "XDG_"):
			continue
		}
		environment = append(environment, entry)
	}
	return append(environment, extra...)
}

// getFreeAddress 返回当前空闲的回环 TCP 地址。
func getFreeAddress() (string, error) {
	// 1. 监听系统分配的端口，记下地址后关闭。
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer func() { _ = listener.Close() }()
	return listener.Addr().String(), nil
}

// startXvfb 启动 Xvfb 并返回进程和它选定的 DISPLAY，例如 :99。
func startXvfb(width, height int, logFile *os.File) (*childProcess, string, error) {
	// 1. 用 -displayfd 让 Xvfb 自己挑空闲的显示号，并在就绪后写回。
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = reader.Close() }()
	command := exec.Command("Xvfb", "-displayfd", "3", "-screen", "0", fmt.Sprintf("%dx%dx24", max(width, 1600), max(height, 1000)), "-nolisten", "tcp", "-noreset")
	command.ExtraFiles = []*os.File{writer}
	command.Stdout, command.Stderr = logFile, logFile
	child, err := startChild(command)
	_ = writer.Close()
	if err != nil {
		return nil, "", fmt.Errorf("start Xvfb: %w", err)
	}
	// 2. 读显示号；Xvfb 先退出时管道关闭，读到 EOF。
	buffer := make([]byte, 32)
	count, err := reader.Read(buffer)
	number := strings.TrimSpace(string(buffer[:count]))
	if err != nil || number == "" {
		child.stop()
		return nil, "", fmt.Errorf("Xvfb did not report a display (see the Xvfb log): %v", err)
	}
	return child, ":" + number, nil
}

// writeEnvironmentFiles 在临时 HOME 写 config.yaml、指向假模型的模型服务、技能、第二个执行器、两个 MCP server 和桌面偏好；
// withModels 为 false 时不写模型服务，用来走首次引导。返回 config.yaml 路径。
func writeEnvironmentFiles(root, home, serveAddress, token, modelBaseURL, language string, width, height int, withModels bool, fakeMCP string) (string, error) {
	// 1. 建工作目录和沙箱目录。
	workspace := filepath.Join(root, "workspace")
	sandbox := filepath.Join(root, "sandbox")
	for _, directory := range []string{filepath.Join(workspace, "projects", "project-one"), filepath.Join(sandbox, "saved")} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			return "", err
		}
	}
	configDirectory := filepath.Join(home, ".config", "openvaala")
	config := filepath.Join(configDirectory, "config.yaml")
	// 2. 准备各文件内容：模型相关文件只在 withModels 时写，models.dev 缓存写一个小目录避免联网。
	files := map[string]string{
		filepath.Join(workspace, "readme.txt"):                                "workspace fixture",
		filepath.Join(configDirectory, "skills", "display-check", "SKILL.md"): "---\nname: display-check\ndescription: 检查消息中的技能展开选择\n---\n# 技能展示检查\n\n保持已展开的技能正文。\n",
		filepath.Join(configDirectory, "skills", "release-notes", "SKILL.md"): "---\nname: release-notes\ndescription: Draft release notes from a list of changes\n---\n# Release notes\n\nGroup changes by area and keep each entry to one line.\n",
		filepath.Join(configDirectory, "executors", "sandbox.yaml"):           fmt.Sprintf("id: sandbox\nkind: local\ndescription: 第二个本地执行器，使用不同的默认根目录。\nworkspace_directory: %q\n", sandbox),
		filepath.Join(home, ".cache", "openvaala", "models-dev.json"):         catalogFixture,
		filepath.Join(home, ".config", "OpenVaala", "preferences.json"):       fmt.Sprintf("{\"language\":%q,\"width\":%d,\"height\":%d}\n", language, width, height),
	}
	// MCP：broken 的命令不存在，演示失败原因；fake 是编译好的假 server，演示已连接。
	files[filepath.Join(configDirectory, "mcp_servers", "broken.yaml")] = "id: broken\ntype: stdio\ncommand: /nonexistent\n"
	if _, err := os.Stat(fakeMCP); err == nil {
		files[filepath.Join(configDirectory, "mcp_servers", "fake.yaml")] = fmt.Sprintf("id: fake\ntype: stdio\ncommand: %s\n", fakeMCP)
	}
	server := fmt.Sprintf("server:\n  listen: %q\n  token: %q\n  workspace_directory: %q\n", serveAddress, token, workspace)
	if withModels {
		files[config] = "models:\n  default_model: local/default\n" + server
		files[filepath.Join(configDirectory, "models", "local.yaml")] = fmt.Sprintf(
			"id: local\nprotocol: openai_chat\nbase_url: %s\nmodels:\n  default:\n    context_window: 32000\n    capabilities:\n      input: [text, image]\n"+
				"  second:\n    display_name: Second\n    context_window: 64000\n    reasoning_efforts: [low, high]\n    default_reasoning_effort: low\n", modelBaseURL)
	} else {
		files[config] = server
	}
	// 3. 建父目录并写入。
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return "", err
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			return "", err
		}
	}
	return config, nil
}

// waitServeHealthy 轮询 serve 的健康检查，serve 先退出或超时时返回错误。
func waitServeHealthy(serveAddress string, serve *childProcess) error {
	// 1. 每 50ms 查一次。
	client := &http.Client{Timeout: time.Second}
	deadline := time.After(serveStartTimeout)
	for {
		if response, err := client.Get("http://" + serveAddress + "/api/v1/healthz"); err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-serve.exited:
			return fmt.Errorf("serve exited before serving (see the serve log)")
		case <-deadline:
			return fmt.Errorf("serve did not become healthy in %s (see the serve log)", serveStartTimeout)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// seedTurn 是预置对话里的一轮：一条用户输入和模型依次给出的回复（工具调用在前，最后一条是文字）。
type seedTurn struct {
	input   string
	replies []modelReply
}

// seedConversations 经真实客户端建几个对话并等每一轮结束：一个短问答、一个带 bash 工具调用的、一个六轮的长对话；
// 最后建的在列表最前面。
func seedConversations(ctx context.Context, serveAddress, token, workspace string, model *fakeModel) error {
	// 1. 连上 serve。
	ctx, cancel := context.WithTimeout(ctx, seedTimeout)
	defer cancel()
	client, err := runtimeclient.NewClient(runtimeclient.NewClientOpt("ws://"+serveAddress+"/api/v1/openvaala", token))
	if err != nil {
		return err
	}
	runDone, eventsDone := make(chan struct{}), make(chan struct{})
	go func() { defer close(runDone); _ = client.Run(ctx) }()
	go func() {
		defer close(eventsDone)
		for range client.Events() {
		}
	}()
	defer func() { cancel(); <-runDone; <-eventsDone }()
	// 2. 准备各对话的输入和假模型回复。
	long := make([]seedTurn, 6)
	for index := range long {
		long[index] = seedTurn{
			input:   fmt.Sprintf("长对话第 %d 轮", index+1),
			replies: []modelReply{{Text: fmt.Sprintf("这是第 %d 轮的回复。\n\n- 要点一：保持主链清晰\n- 要点二：状态只有一个拥有者\n\n```go\nfmt.Println(%d)\n```", index+1, index+1)}},
		}
	}
	conversations := [][]seedTurn{
		long,
		{{
			input: "读取 readme.txt 并总结",
			replies: []modelReply{
				{Reasoning: "先读文件再总结。", Tool: "bash", Arguments: fmt.Sprintf("{\"command\":\"cat %s\"}", filepath.Join(workspace, "readme.txt"))},
				{Text: "readme.txt 的内容是 `workspace fixture`，说明这是一个测试工作区。"},
			},
		}},
		{{
			input:   "你好，介绍一下你自己",
			replies: []modelReply{{Text: "你好，我是 OpenVaala 的演示助手。\n\n我可以：\n\n1. 回答问题\n2. 读写工作区里的文件\n3. 运行命令"}},
		}},
	}
	// 3. 逐个对话、逐轮提交，等模型回复被取走且消息数达到预期再提交下一轮，保证回复顺序确定。
	for _, turns := range conversations {
		conversation, err := client.CreateConversation(ctx, &runtimeclient.CreateConversationRequest{AgentName: "default"})
		if err != nil {
			return err
		}
		for index, turn := range turns {
			for _, reply := range turn.replies {
				model.replies <- reply
			}
			if _, err := client.SubmitInput(ctx, &runtimeclient.SubmitInputRequest{
				ConversationID: conversation.ID,
				Content:        []*defs.Content{{Kind: defs.ContentText, Text: &defs.TextContent{Text: turn.input}}},
			}); err != nil {
				return err
			}
			if err := waitSeedTurn(ctx, client, model, conversation.ID, 2*(index+1)); err != nil {
				return err
			}
		}
	}
	return nil
}

// waitSeedTurn 轮询到对话消息数达到 want 且排好的回复都被取走，或 ctx 结束。
func waitSeedTurn(ctx context.Context, client runtimeclient.Client, model *fakeModel, id defs.ConversationID, want int) error {
	// 1. 每 100ms 读一次对话记录。
	for {
		conversation, err := client.GetConversation(ctx, id)
		if err != nil {
			return err
		}
		if conversation.MessageCount >= want && model.getPendingReplyCount() == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("seed conversation %s has %d messages, want %d: %w", id, conversation.MessageCount, want, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// catalogFixture 是 models.dev 缓存里的一个小目录：一个兼容 OpenAI 的服务，让服务目录里有内容可选。
const catalogFixture = `{"compat":{"id":"compat","name":"Compat Gateway","npm":"@ai-sdk/openai-compatible","api":"https://compat.example/v1","env":["COMPAT_API_KEY"],"models":{"plain":{"id":"plain","name":"Plain Model","reasoning":false,"modalities":{"input":["text"],"output":["text"]},"limit":{"context":128000,"output":8192}}}}}`
