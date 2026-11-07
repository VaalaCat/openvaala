// Command webuifixture 为 WebUI 的 Playwright 端到端测试准备真实环境：用 --openvaala 指定的已编译二进制
// （嵌入 www/out/web 里刚构建的页面），在临时 HOME 启动 serve 和可控的假模型，把连接信息写进 --out 指定的状态文件，阻塞到收到 SIGTERM、SIGINT 或 stdin 管道关闭，
// 退出前停掉 serve 并删除临时目录。只给 Playwright 用，不进 go test。
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/session"
)

const (
	// serveStartTimeout 是等 serve 健康检查通过的最长时间。
	serveStartTimeout = 30 * time.Second
	// healthzTimeout 是单次健康检查请求的超时。
	healthzTimeout = time.Second
	// serveStopTimeout 是 SIGTERM 之后等 serve 退出的最长时间，超时后 SIGKILL。
	serveStopTimeout = 10 * time.Second
	// seedTimeout 是预置数据等所有 Turn 结束的最长时间。
	seedTimeout = 2 * time.Minute
)

// fixtureState 是写进状态文件的连接信息，由 www/e2e/fixture.ts 读取。
type fixtureState struct {
	// URL 是 serve 的回环地址。
	URL string `json:"url"`
	// Token 是 serve 的访问令牌。
	Token string `json:"token"`
	// LANURL 是 serve 的非回环地址（非安全上下文）；没有非回环 IPv4 时为空。
	LANURL string `json:"lanURL"`
	// Control 是控制接口和假模型所在的地址；假模型的 base_url 是 Control + "/v1"。
	Control string `json:"control"`
	// WorkspaceDirectory 和 SandboxDirectory 是两个本地 Executor 的默认根目录。
	WorkspaceDirectory string `json:"workspaceDirectory"`
	SandboxDirectory   string `json:"sandboxDirectory"`
}

// webUIFixture 拥有临时目录和正在运行的 serve 进程。
type webUIFixture struct {
	// binary 是调用方编译好的 openvaala，多份 fixture 共用。
	binary string
	home   string
	// token 是本次运行随机生成的 serve 访问令牌。
	token  string
	config string
	// serveLog 是 serve 的 stdout、stderr，重启后接着写。
	serveLog *os.File
	// servePort 是 serve 监听的端口，重启后不变。
	servePort int

	// serveLock 串行化 serve 的启动、重启和停止。
	serveLock sync.Mutex
	// serveCommand 是正在运行的 serve，停止后为 nil；serveExited 在它退出时关闭。
	serveCommand *exec.Cmd
	serveExited  chan struct{}
}

// main 读取 --openvaala、--out 并运行 fixture，出错时以非零状态退出。
func main() {
	binary := flag.String("openvaala", "", "path of the compiled openvaala binary")
	out := flag.String("out", "", "path of the JSON state file to write once serve is ready")
	flag.Parse()
	if *binary == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "webuifixture: --openvaala and --out are required")
		os.Exit(2)
	}
	if err := run(*binary, *out); err != nil {
		fmt.Fprintln(os.Stderr, "webuifixture:", err)
		os.Exit(1)
	}
}

// run 准备环境、写状态文件并阻塞到被要求退出：起假模型和控制接口，写配置，用 binary 起 serve；
// 退出时依次停 serve、关控制接口、删临时目录。
func run(binary string, out string) error {
	// 1. SIGTERM、SIGINT 时退出；stdin 是管道或 socket 时它关闭（Playwright 进程没了）也退出。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if info, err := os.Stdin.Stat(); err == nil && info.Mode()&(os.ModeNamedPipe|os.ModeSocket) != 0 {
		go func() {
			_, _ = io.Copy(io.Discard, os.Stdin)
			stop()
		}()
	}
	// 2. 建临时目录。
	root, err := os.MkdirTemp("", "openvaala-webui-e2e-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(root) }()
	fixture := &webUIFixture{
		binary: binary,
		home:   filepath.Join(root, "home"),
		token:  rand.Text(),
	}
	fixture.config = filepath.Join(fixture.home, ".config", "openvaala", "config.yaml")
	// 3. 在回环地址起假模型和控制接口。
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	control := "http://" + listener.Addr().String()
	model := newFakeModel()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", model.handleChatCompletions)
	mux.HandleFunc("GET /v1/models", model.handleListModels)
	mux.HandleFunc("POST /replies", model.handleAppendReplies)
	mux.HandleFunc("POST /replies/reset", model.handleResetReplies)
	mux.HandleFunc("POST /models", model.handleSetModels)
	mux.HandleFunc("POST /restart", fixture.handleRestart)
	mux.HandleFunc("POST /seed", fixture.handleSeed)
	mux.HandleFunc("POST /input", fixture.handleInput)
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()
	defer func() { _ = server.Close() }()
	// 4. 选端口，写配置，起 serve；serve 停在 run 返回之前。
	if fixture.servePort, err = getFreePort(); err != nil {
		return err
	}
	if err := fixture.writeConfig(root, control+"/v1"); err != nil {
		return err
	}
	if fixture.serveLog, err = os.Create(filepath.Join(root, "serve.log")); err != nil {
		return err
	}
	defer func() { _ = fixture.serveLog.Close() }()
	defer fixture.stopServe()
	if err := fixture.startServe(); err != nil {
		return err
	}
	// 5. 写状态文件，等待退出。
	state := fixtureState{
		URL:                "http://127.0.0.1:" + strconv.Itoa(fixture.servePort),
		Token:              fixture.token,
		Control:            control,
		WorkspaceDirectory: filepath.Join(root, "workspace"),
		SandboxDirectory:   filepath.Join(root, "sandbox"),
	}
	if ip := getLANAddress(); ip != "" {
		state.LANURL = "http://" + net.JoinHostPort(ip, strconv.Itoa(fixture.servePort))
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := os.WriteFile(out+".tmp", payload, 0600); err != nil {
		return err
	}
	if err := os.Rename(out+".tmp", out); err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

// writeConfig 在临时 HOME 写 config.yaml（默认模型固定为 local/default，新增的模型服务不会顶替它）、
// 指向假模型的 models/local.yaml 和空的 models.dev 缓存（不联网）。
func (f *webUIFixture) writeConfig(root, modelBaseURL string) error {
	// 1. 准备工作目录和各文件内容。
	workspace := filepath.Join(root, "workspace")
	sandbox := filepath.Join(root, "sandbox")
	for _, directory := range []string{filepath.Join(workspace, "projects", "project-one"), filepath.Join(workspace, "manual"), filepath.Join(sandbox, "saved"), filepath.Join(sandbox, "next")} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			return err
		}
	}
	if err := os.Symlink(filepath.Join(workspace, "projects", "project-one"), filepath.Join(workspace, "project-link")); err != nil {
		return err
	}
	configDirectory := filepath.Dir(f.config)
	files := map[string]string{
		f.config: fmt.Sprintf("models:\n  default_model: local/default\nserver:\n  listen: %q\n  token: %q\n  workspace_directory: %q\n",
			"0.0.0.0:"+strconv.Itoa(f.servePort), f.token, workspace),
		filepath.Join(configDirectory, "models", "local.yaml"): fmt.Sprintf(
			"id: local\nprotocol: openai_chat\nbase_url: %s\nmodels:\n  default:\n    context_window: 32000\n", modelBaseURL),
		filepath.Join(f.home, ".cache", "openvaala", "models-dev.json"):       "{}",
		filepath.Join(workspace, "readme.txt"):                                "workspace fixture",
		filepath.Join(configDirectory, "skills", "display-check", "SKILL.md"): "---\nname: display-check\ndescription: 检查消息中的技能展开选择\n---\n# 技能展示检查\n\n保持已展开的技能正文。\n",
		filepath.Join(configDirectory, "hooks", "display-boundary.yaml"):      "hooks:\n  SessionStart:\n    - hooks:\n        - type: command\n          command: if test -f .display-hook-boundary; then printf '真实 Hook 分组检查'; fi\n",
		filepath.Join(configDirectory, "executors", "sandbox.yaml"):           fmt.Sprintf("id: sandbox\nkind: local\ndescription: 第二个本地执行器，用于核对工作目录和各自保存的目录；它与 local 使用不同的默认根目录。\nworkspace_directory: %q\n", sandbox),
	}
	for index := range 8 {
		id := fmt.Sprintf("laboratory-%02d-with-a-long-executor-name", index+1)
		files[filepath.Join(configDirectory, "executors", id+".yaml")] = fmt.Sprintf("id: %s\nkind: local\ndescription: 用于核对多执行器长表单的本地工作区。这段说明包含足够长的自然语言，应该自然换行并最多展示两行，工作目录另起一行，不能替代说明。\nworkspace_directory: %q\n", id, workspace)
	}
	// 2. 建父目录并写入。
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			return err
		}
	}
	return nil
}

// startServe 启动 serve 并等到健康检查通过；serve 先退出或超时时返回带日志的错误。
func (f *webUIFixture) startServe() error {
	f.serveLock.Lock()
	defer f.serveLock.Unlock()
	// 1. 用临时 HOME 和清理过的环境启动 serve，后台等待退出。
	command := exec.Command(f.binary, "--config-file", f.config, "serve")
	command.Env = getServeEnvironment(f.home)
	command.Dir = f.home
	command.Stdout = f.serveLog
	command.Stderr = f.serveLog
	if err := command.Start(); err != nil {
		return err
	}
	exited := make(chan struct{})
	go func() {
		_ = command.Wait()
		close(exited)
	}()
	f.serveCommand, f.serveExited = command, exited
	// 2. 轮询健康检查。
	healthz := "http://127.0.0.1:" + strconv.Itoa(f.servePort) + "/api/v1/healthz"
	client := &http.Client{Timeout: healthzTimeout}
	deadline := time.After(serveStartTimeout)
	for {
		if response, err := client.Get(healthz); err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-exited:
			return fmt.Errorf("serve exited before serving\n%s", f.readServeLog())
		case <-deadline:
			return fmt.Errorf("serve did not become healthy in %s\n%s", serveStartTimeout, f.readServeLog())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// stopServe 给 serve 发 SIGTERM 并等它退出，超时后 SIGKILL；没有运行时什么都不做。
func (f *webUIFixture) stopServe() {
	f.serveLock.Lock()
	defer f.serveLock.Unlock()
	// 1. 发 SIGTERM，等退出或超时。
	if f.serveCommand == nil {
		return
	}
	_ = f.serveCommand.Process.Signal(syscall.SIGTERM)
	select {
	case <-f.serveExited:
	case <-time.After(serveStopTimeout):
		_ = f.serveCommand.Process.Kill()
		<-f.serveExited
	}
	f.serveCommand, f.serveExited = nil, nil
}

// handleRestart 停掉 serve 再在同一端口启动，健康检查通过后返回。
func (f *webUIFixture) handleRestart(w http.ResponseWriter, _ *http.Request) {
	// 1. 依次停止、启动。
	f.stopServe()
	if err := f.startServe(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{})
}

// seedRequest 是 POST /seed 的请求：Conversations 个各一轮的对话，再加一个 LongConversationTurns 轮的长对话。
type seedRequest struct {
	Conversations         int `json:"conversations"`
	LongConversationTurns int `json:"longConversationTurns"`
}

// handleSeed 经 runtimeclient 建对话并提交输入，等所有 Turn 结束后返回长对话的 ID：
// 每条输入由假模型回复，一轮是一条用户消息加一条回复；长对话最后建，在列表最前面。
func (f *webUIFixture) handleSeed(w http.ResponseWriter, r *http.Request) {
	// 1. 解码请求，连上 serve。
	var request seedRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), seedTimeout)
	defer cancel()
	client, err := runtimeclient.NewClient(runtimeclient.NewClientOpt(
		"ws://127.0.0.1:"+strconv.Itoa(f.servePort)+"/api/v1/openvaala", f.token))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_ = client.Run(ctx)
	}()
	go func() {
		for range client.Events() {
		}
	}()
	defer func() {
		cancel()
		<-runDone
	}()
	// 2. 建短对话和长对话；同一对话里排队的普通输入各开一轮。
	turnsByConversation := map[defs.ConversationID]int{}
	for index := range request.Conversations {
		id, err := createConversation(ctx, client, []string{fmt.Sprintf("预置对话 %03d", index+1)})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		turnsByConversation[id] = 1
	}
	longInputs := make([]string, request.LongConversationTurns)
	for index := range longInputs {
		longInputs[index] = fmt.Sprintf("长对话第 %d 轮", index+1)
	}
	longConversationID, err := createConversation(ctx, client, longInputs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	turnsByConversation[longConversationID] = len(longInputs)
	// 3. 等每个对话的消息数达到轮数的两倍。
	for id, turns := range turnsByConversation {
		if err := waitMessageCount(ctx, client, id, 2*turns); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	writeJSON(w, map[string]any{"longConversationID": longConversationID})
}

// inputRequest 由测试控制面提交完整 Content，或原子取回仍待处理的输入。
type inputRequest struct {
	Operation      string                   `json:"operation"`
	ConversationID defs.ConversationID      `json:"conversationID"`
	ItemID         defs.ItemID              `json:"itemID"`
	Content        []*defs.Content          `json:"content"`
	Mode           session.SessionInputMode `json:"mode"`
}

// handleInput 经真实客户端提交、取回输入或读取消息内容。
func (f *webUIFixture) handleInput(w http.ResponseWriter, r *http.Request) {
	// 1. 解析请求并启动本次独占的客户端。
	var request inputRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	client, err := runtimeclient.NewClient(runtimeclient.NewClientOpt("ws://127.0.0.1:"+strconv.Itoa(f.servePort)+"/api/v1/openvaala", f.token))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	runDone, eventsDone := make(chan struct{}), make(chan struct{})
	go func() { defer close(runDone); _ = client.Run(ctx) }()
	go func() {
		defer close(eventsDone)
		for range client.Events() {
		}
	}()
	defer func() { cancel(); <-runDone; <-eventsDone }()
	// 2. 直接调用输入或查询公开方法。
	switch request.Operation {
	case "executorSettings":
		conversation, err := client.GetConversation(ctx, request.ConversationID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, conversation.ExecutorSettings)
	case "submit":
		itemID, err := client.SubmitInput(ctx, &runtimeclient.SubmitInputRequest{ConversationID: request.ConversationID, Mode: request.Mode, Content: request.Content})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"itemID": itemID})
	case "retrieve":
		content, err := client.WithdrawInput(ctx, request.ConversationID, request.ItemID, true)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"content": content})
	case "messages":
		conversation, err := client.GetConversation(ctx, request.ConversationID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		items, err := client.ListItems(ctx, &runtimeclient.ListItemsRequest{ConversationID: request.ConversationID, SessionID: conversation.RootSessionID, Limit: 1000})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		messages := []*defs.Message{}
		for _, logged := range items.Items {
			if logged.Item.Message != nil {
				messages = append(messages, logged.Item.Message)
			}
		}
		writeJSON(w, map[string]any{"messages": messages})
	default:
		http.Error(w, "unknown input operation", http.StatusBadRequest)
	}
}

// createConversation 用 default Agent 建一个对话，按顺序提交 inputs。
func createConversation(ctx context.Context, client runtimeclient.Client, inputs []string) (defs.ConversationID, error) {
	// 1. 建对话，逐条以普通方式提交。
	conversation, err := client.CreateConversation(ctx, &runtimeclient.CreateConversationRequest{AgentName: "default"})
	if err != nil {
		return "", err
	}
	for _, input := range inputs {
		if _, err := client.SubmitInput(ctx, &runtimeclient.SubmitInputRequest{
			ConversationID: conversation.ID,
			Content:        []*defs.Content{{Kind: defs.ContentText, Text: &defs.TextContent{Text: input}}},
		}); err != nil {
			return "", err
		}
	}
	return conversation.ID, nil
}

// waitMessageCount 轮询对话记录，直到消息数达到 want 或 ctx 结束。
func waitMessageCount(ctx context.Context, client runtimeclient.Client, id defs.ConversationID, want int) error {
	// 1. 每 100ms 读一次对话记录。
	for {
		conversation, err := client.GetConversation(ctx, id)
		if err != nil {
			return err
		}
		if conversation.MessageCount >= want {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("conversation %s has %d messages, want %d: %w", id, conversation.MessageCount, want, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// readServeLog 返回 serve 日志的内容，用于启动失败的错误信息。
func (f *webUIFixture) readServeLog() string {
	// 1. 读整个日志文件。
	content, _ := os.ReadFile(f.serveLog.Name())
	return string(content)
}

// getServeEnvironment 返回 serve 的环境：HOME 换成临时目录，SHELL 固定为 /bin/sh（终端里的 shell 不受开发机配置影响，
// 临时 HOME 里没有 rc 文件，zsh 会弹出新用户向导），去掉 OPENVAALA_*、XDG_*、CODEX_HOME 和 PASEO_HOME。
func getServeEnvironment(home string) []string {
	// 1. 复制当前环境并过滤。
	environment := []string{"HOME=" + home, "SHELL=/bin/sh"}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if name == "HOME" || name == "SHELL" || name == "CODEX_HOME" || name == "PASEO_HOME" || strings.HasPrefix(name, "OPENVAALA_") || strings.HasPrefix(name, "XDG_") {
			continue
		}
		environment = append(environment, entry)
	}
	return environment
}

// getFreePort 返回系统分配的一个空闲 TCP 端口。
func getFreePort() (int, error) {
	// 1. 监听 0 端口，记下端口后关闭。
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = listener.Close() }()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

// getLANAddress 返回第一个已启用网卡上的非回环 IPv4 地址，没有时为空。
func getLANAddress() string {
	// 1. 依次查已启用、非回环网卡的地址。
	interfaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, each := range interfaces {
		if each.Flags&net.FlagUp == 0 || each.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, err := each.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			if network, ok := address.(*net.IPNet); ok && network.IP.To4() != nil && !network.IP.IsLoopback() {
				return network.IP.String()
			}
		}
	}
	return ""
}
