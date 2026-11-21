package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/internal/config"
	"github.com/VaalaCat/openvaala/internal/modelcatalog"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/runtimeserver"
	"github.com/VaalaCat/openvaala/pkg/storage"
	storagefile "github.com/VaalaCat/openvaala/pkg/storage/file"
	storagesqlite "github.com/VaalaCat/openvaala/pkg/storage/sqlite"
)

const (
	restartInterruptConfigEnv = "OPENVAALA_E2E_INTERRUPT_CONFIG"
	restartInterruptMarker    = "before-interrupted-append"
)

// TestTurnRestartRecovery 在真实 serve 进程重启后检查四种未结束 Turn 的恢复结果。
func TestTurnRestartRecovery(t *testing.T) {
	// 1. 测试子进程用真实 Runtime Server 运行，在保存 interrupted 之前暂停。
	if configPath := os.Getenv(restartInterruptConfigEnv); configPath != "" {
		runRestartInterruptServer(t, configPath)
		return
	}
	// 2. 每种中断位置独立运行，重启时沿用该用例的数据目录。
	binary := buildOpenVaala(t)
	for _, test := range []struct {
		name string
		run  func(*testing.T, string)
	}{
		{name: "tool_side_effect_without_result", run: testRestartToolSideEffect},
		{name: "reply_saved_before_stop_hook_finishes", run: testRestartSavedReply},
		{name: "partial_reply", run: testRestartPartialReply},
		{name: "shutdown_vs_user_stop", run: testRestartShutdownAndUserStop},
	} {
		t.Run(test.name, func(t *testing.T) { test.run(t, binary) })
	}
}

// testRestartToolSideEffect 检查工具已经写入文件但没有结果时只补未知结果，不重放工具。
func testRestartToolSideEffect(t *testing.T, binary string) {
	// 1. 运行 bash 工具，写一行后停在测试控制的连接上。
	model := &restartModel{e2eModel: newE2EModel(t)}
	configPath := writeE2EConfig(t, model.server.URL)
	serve := startTurnRestartServe(t, binary, configPath, nil)
	observer, conversation := newRestartConversation(t, configPath)
	workspace := conversation.ExecutorSettings.WorkspaceDirectories[defs.LocalExecutorID]
	reviewPath := filepath.Join(workspace, "review.txt")
	blocker := newRestartCommandBlocker(t)
	command := fmt.Sprintf("printf 'review-42\\n' >> %q\n%s", reviewPath, blocker.command())
	observer.startTurn(t, "审查这个改动")
	firstRequest := model.next(t)
	firstRequest.reply <- e2eModelReply{tool: "bash", arguments: fmt.Sprintf(`{"command":%q,"wait_seconds":60}`, command)}
	toolConn := blocker.waitBlocked(t)
	turn := waitRestartTurn(t, observer.client, conversation, defs.TurnStatusRunning)
	items := listRestartItems(t, observer.client, conversation)
	toolCall := findRestartItem(items, func(item *defs.Item) bool { return item.Kind == defs.ItemToolCall && item.TurnID == turn.ID })
	if toolCall == nil || toolCall.ToolCall == nil || toolCall.ToolCall.CallID == "" {
		t.Fatal("tool call with its original CallID was not persisted before the side effect")
	}
	callID := toolCall.ToolCall.CallID
	if slices.ContainsFunc(items, func(item *defs.Item) bool { return item.Kind == defs.ItemToolResult }) {
		t.Fatal("tool result was already persisted before the crash")
	}
	checkRestartFile(t, reviewPath, "review-42\n")

	// 2. 强杀服务，再关闭测试连接放行原 bash；重启后只打开原对话。
	observer.stop()
	stopTurnRestartServe(t, serve, syscall.SIGKILL)
	if err := toolConn.Close(); err != nil {
		t.Fatal(err)
	}
	startTurnRestartServe(t, binary, configPath, serve)
	model.checkRequests(t, 1)
	reopened := openRuntimeObserver(t, configPath, conversation.ID)
	recoveredRequest := model.next(t)
	unknown := getModelToolResult(t, recoveredRequest.body)
	var outcome struct {
		Code string `json:"code"`
	}
	if getModelToolCallID(recoveredRequest.body) != callID || unknown.Error != nil || json.Unmarshal(unknown.Content, &outcome) != nil || outcome.Code != "outcome_unknown" {
		t.Fatalf("recovered tool result = %+v, content %s; want CallID %s and outcome_unknown without an error", unknown, unknown.Content, callID)
	}

	// 3. 回复文本并确认只有原轮结束，工具没有再次写文件。
	recoveredRequest.reply <- e2eModelReply{text: "已检查现场，继续完成审查"}
	checkRestartTurnFinished(t, reopened, conversation, turn.ID, defs.TurnStatusCompleted)
	model.checkRequests(t, 2)
	checkRestartFile(t, reviewPath, "review-42\n")
	items = listRestartItems(t, reopened.client, conversation)
	if count := countRestartItems(items, func(item *defs.Item) bool {
		return item.Kind == defs.ItemToolResult && item.TurnID == turn.ID && item.ToolResult != nil && item.ToolResult.CallID == callID
	}); count != 1 {
		t.Fatalf("ToolResult count for %s = %d, want one", callID, count)
	}
	t.Logf("TurnID=%s CallID=%s: model requests=2, side effects=1", turn.ID, callID)
}

// testRestartSavedReply 检查回复完成事实已落盘时恢复只重跑 Stop hook，不重新调用模型。
func testRestartSavedReply(t *testing.T, binary string) {
	// 1. 保存模型回复，等 Stop hook 阻塞并核对回复的完成标记已落盘。
	model := &restartModel{e2eModel: newE2EModel(t)}
	configPath := writeE2EConfig(t, model.server.URL)
	blocker := newRestartCommandBlocker(t)
	writeRestartStopHook(t, filepath.Dir(configPath), blocker.command())
	serve := startTurnRestartServe(t, binary, configPath, nil)
	observer, conversation := newRestartConversation(t, configPath)
	observer.startTurn(t, "检查审查结果")
	model.next(t).reply <- e2eModelReply{text: "审查完成"}
	firstHook := blocker.waitBlocked(t)
	turn := waitRestartTurn(t, observer.client, conversation, defs.TurnStatusRunning)
	items := listRestartItems(t, observer.client, conversation)
	savedReply := func(item *defs.Item) bool { return item.TurnID == turn.ID && restartItemText(item) == "审查完成" }
	if item := findRestartItem(items, savedReply); item == nil || item.ModelCallID == "" || item.ModelCallResult != defs.ModelCallResultReply {
		t.Fatalf("reply or its completion marker is absent before crash: %+v", item)
	}
	if count := countRestartItems(items, savedReply); count != 1 {
		t.Fatalf("saved reply count before kill = %d, want one", count)
	}

	// 2. 强杀后重开原对话，放行重新运行的 Stop hook。
	observer.stop()
	stopTurnRestartServe(t, serve, syscall.SIGKILL)
	if err := firstHook.Close(); err != nil {
		t.Fatal(err)
	}
	startTurnRestartServe(t, binary, configPath, serve)
	model.checkRequests(t, 1)
	reopened := openRuntimeObserver(t, configPath, conversation.ID)
	secondHook := blocker.waitBlocked(t)
	if _, err := io.WriteString(secondHook, "continue\n"); err != nil {
		t.Fatal(err)
	}

	// 3. 核对原轮的终态、唯一回复和模型请求总数。
	checkRestartTurnFinished(t, reopened, conversation, turn.ID, defs.TurnStatusCompleted)
	model.checkRequests(t, 1)
	items = listRestartItems(t, reopened.client, conversation)
	if count := countRestartItems(items, savedReply); count != 1 {
		t.Fatalf("saved reply count after recovery = %d, want one", count)
	}
	t.Logf("TurnID=%s: model requests=1, saved replies=1", turn.ID)
}

// testRestartPartialReply 检查只写入一条推理摘要时同一步换调用 ID 重试，并保留一条旧错误。
func testRestartPartialReply(t *testing.T, binary string) {
	// 1. 用首段文字结束推理摘要，但保持整个模型流未结束。
	const prompt = "检查这项变更"
	const oldOutput = "第一次尝试"
	model := &restartModel{e2eModel: newE2EModel(t)}
	configPath := writeE2EConfig(t, model.server.URL)
	serve := startTurnRestartServe(t, binary, configPath, nil)
	observer, conversation := newRestartConversation(t, configPath)
	observer.startTurn(t, prompt)
	firstRequest := model.next(t)
	checkPartialRestartRequest(t, firstRequest, prompt, oldOutput)
	firstRequest.reply <- e2eModelReply{reasoning: oldOutput, text: "尚未完成", hold: true}
	turn := waitRestartTurn(t, observer.client, conversation, defs.TurnStatusRunning)
	isOldReasoning := func(item *defs.Item) bool {
		return item.TurnID == turn.ID && item.Kind == defs.ItemReasoning && item.Reasoning != nil && slices.Contains(item.Reasoning.Summary, oldOutput)
	}
	oldReasoning := waitRestartItem(t, observer.client, conversation, isOldReasoning)
	oldCheckpoint := readRestartCheckpoint(t, configPath, conversation, turn.ID)
	if oldReasoning.ModelCallID == "" || oldCheckpoint.Request.ModelCallID != oldReasoning.ModelCallID || oldReasoning.ModelCallResult != "" {
		t.Fatalf("reasoning item %+v must belong to the unfinished model request %+v", oldReasoning, oldCheckpoint.Request)
	}

	// 2. 强杀重开并在回复第二个模型请求之前对比两个 checkpoint。
	observer.stop()
	stopTurnRestartServe(t, serve, syscall.SIGKILL)
	startTurnRestartServe(t, binary, configPath, serve)
	model.checkRequests(t, 1)
	reopened := openRuntimeObserver(t, configPath, conversation.ID)
	recoveredRequest := model.next(t)
	checkPartialRestartRequest(t, recoveredRequest, prompt, oldOutput)
	checkPartialRestartRequest(t, recoveredRequest, prompt, "尚未完成")
	newCheckpoint := readRestartCheckpoint(t, configPath, conversation, turn.ID)
	if newCheckpoint.Request.ModelCallID == oldCheckpoint.Request.ModelCallID || newCheckpoint.Step != oldCheckpoint.Step || newCheckpoint.Request.ModelCallThroughItemID != oldCheckpoint.Request.ModelCallThroughItemID {
		t.Fatalf("recovery checkpoint = %+v / %+v, want same step and input cutoff and new ModelCallID vs %+v / %+v", newCheckpoint, newCheckpoint.Request, oldCheckpoint, oldCheckpoint.Request)
	}

	// 3. 核对旧推理、旧错误、新回复和原轮终态。
	recoveredRequest.reply <- e2eModelReply{text: "最终回复"}
	checkRestartTurnFinished(t, reopened, conversation, turn.ID, defs.TurnStatusCompleted)
	model.checkRequests(t, 2)
	items := listRestartItems(t, reopened.client, conversation)
	for _, expected := range []struct {
		name    string
		matches func(*defs.Item) bool
	}{
		{"old ModelError", func(item *defs.Item) bool {
			return item.TurnID == turn.ID && item.Kind == defs.ItemModelError && item.ModelCallID == oldReasoning.ModelCallID
		}},
		{"old reasoning", isOldReasoning},
		{"new reply", func(item *defs.Item) bool {
			return item.TurnID == turn.ID && item.ModelCallID == newCheckpoint.Request.ModelCallID && restartItemText(item) == "最终回复"
		}},
	} {
		if count := countRestartItems(items, expected.matches); count != 1 {
			t.Fatalf("%s count = %d, want one", expected.name, count)
		}
	}
	t.Logf("TurnID=%s step=%d cutoff=%s: ModelCallID %s -> %s, model requests=2", turn.ID, newCheckpoint.Step, newCheckpoint.Request.ModelCallThroughItemID, oldCheckpoint.Request.ModelCallID, newCheckpoint.Request.ModelCallID)
}

// testRestartShutdownAndUserStop 对照服务正常退出和用户停止回执后的强杀。
func testRestartShutdownAndUserStop(t *testing.T, binary string) {
	for _, test := range []struct {
		name       string
		isUserStop bool
	}{
		{name: "normal_shutdown"},
		{name: "user_stop", isUserStop: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			// 1. 发起原轮并挂起模型；两条路径使用独立 HOME。
			model := &restartModel{e2eModel: newE2EModel(t)}
			configPath := writeE2EConfig(t, model.server.URL)
			firstBinary := binary
			if test.isUserStop {
				firstBinary = ""
			}
			serve := startTurnRestartServe(t, firstBinary, configPath, nil)
			observer, conversation := newRestartConversation(t, configPath)
			observer.startTurn(t, "检查当前状态")
			model.next(t).reply <- e2eModelReply{hold: true}
			turn := waitRestartTurn(t, observer.client, conversation, defs.TurnStatusRunning)

			// 2. 用户停止时等成功回执和 Append 前屏障，再强杀；正常关停只发送 SIGTERM。
			if test.isUserStop {
				ctx, cancel := context.WithTimeout(t.Context(), e2eTimeout)
				defer cancel()
				stoppedTurnID, err := observer.client.InterruptTurn(ctx, conversation.ID)
				if err != nil || stoppedTurnID != turn.ID {
					t.Fatalf("InterruptTurn() = %s, %v; want successful receipt for %s", stoppedTurnID, err, turn.ID)
				}
				marker := filepath.Join(filepath.Dir(configPath), restartInterruptMarker)
				waitRestartFile(t, marker, string(conversation.ID)+"/"+string(turn.ID))
				observer.stop()
				stopTurnRestartServe(t, serve, syscall.SIGKILL)
			} else {
				observer.stop()
				stopTurnRestartServe(t, serve, syscall.SIGTERM)
			}

			// 3. 确认新进程就绪时未请求模型，再只打开原对话，分别核对该轮的结果。
			startTurnRestartServe(t, binary, configPath, serve)
			model.checkRequests(t, 1)
			reopened := openRuntimeObserver(t, configPath, conversation.ID)
			if test.isUserStop {
				checkRestartTurnFinished(t, reopened, conversation, turn.ID, defs.TurnStatusInterrupted)
				model.checkRequests(t, 1)
				items := listRestartItems(t, reopened.client, conversation)
				if count := countRestartItems(items, func(item *defs.Item) bool { return item.Kind == defs.ItemTurnInterrupted && item.TurnID == turn.ID }); count != 1 {
					t.Fatalf("turn_interrupted item count = %d, want one", count)
				}
				t.Logf("TurnID=%s: model requests 1 -> 1, interrupted", turn.ID)
			} else {
				model.next(t).reply <- e2eModelReply{text: "正常恢复完成"}
				checkRestartTurnFinished(t, reopened, conversation, turn.ID, defs.TurnStatusCompleted)
				model.checkRequests(t, 2)
				t.Logf("TurnID=%s: model requests 1 -> 2, completed", turn.ID)
			}
		})
	}
}

// startTurnRestartServe 启动服务、核对新 PID 并等待 healthz。
func startTurnRestartServe(t *testing.T, binary, configPath string, previous *e2eProcess) *e2eProcess {
	t.Helper()
	// 1. 记录已有连接文件，启动真实 serve 或带 Append 屏障的测试子进程。
	home := filepath.Dir(configPath)
	connectionPath := getE2EServerConnectionPath(configPath)
	previousFile, err := os.Stat(connectionPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var command *exec.Cmd
	environment := getE2EEnvironment(home)
	if binary == "" {
		testBinary, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		command = exec.Command(testBinary, "-test.run=^TestTurnRestartRecovery$")
		environment = append(environment, restartInterruptConfigEnv+"="+configPath)
	} else {
		command = exec.Command(binary, "--config-file", configPath, "serve")
	}
	command.Env = environment
	command.Dir = home
	process := &e2eProcess{
		command: command,
		stderr:  &e2eOutput{},
	}
	command.Stdout = process.stderr
	process.start(t, nil)
	t.Cleanup(func() { stopTurnRestartServe(t, process, syscall.SIGKILL) })

	// 2. 核对旧进程已退出、新 PID 不同，再等新连接文件和健康检查。
	if previous != nil {
		previousPID, newPID := previous.command.Process.Pid, command.Process.Pid
		select {
		case <-previous.exited:
		default:
			t.Fatalf("previous serve PID %d has not exited", previousPID)
		}
		if previousPID == newPID {
			t.Fatalf("new serve reused PID %d", newPID)
		}
		t.Logf("service PIDs: %d -> %d", previousPID, newPID)
	}
	client := &http.Client{Timeout: time.Second}
	process.waitServeHealthy(t, connectionPath, previousFile, client, 20*time.Millisecond)
	return process
}

// stopTurnRestartServe 发送指定信号、等待退出并核对退出结果和 race 报告。
func stopTurnRestartServe(t *testing.T, process *e2eProcess, signal syscall.Signal) {
	t.Helper()
	process.stopOnce.Do(func() {
		// 1. 向命令句柄发信号并等待 Wait 返回。
		err := process.command.Process.Signal(signal)
		pid := process.command.Process.Pid
		select {
		case <-process.exited:
		case <-time.After(e2eTimeout):
			killErr := process.command.Process.Kill()
			select {
			case <-process.exited:
			case <-time.After(time.Second):
				t.Fatalf("serve PID %d did not exit after %s or kill fallback: signal=%v, kill=%v\n%s", pid, signal, err, killErr, process.stderr)
			}
			t.Fatalf("serve PID %d did not exit after %s: %v\n%s", pid, signal, err, process.stderr)
		}
		// 2. 核对服务日志，以及强杀的退出信号或正常关停的退出码。
		log := process.stderr.String()
		if strings.Contains(log, "WARNING: DATA RACE") {
			t.Fatalf("serve PID %d reported a data race\n%s", pid, log)
		}
		status, _ := process.command.ProcessState.Sys().(syscall.WaitStatus)
		if signal == syscall.SIGKILL && status.Signaled() && status.Signal() == signal && err == nil {
			return
		}
		if signal == syscall.SIGTERM && err == nil && process.exitErr == nil {
			return
		}
		t.Fatalf("stop serve PID %d: signal=%s, send=%v, wait=%v\n%s", pid, signal, err, process.exitErr, log)
	})
}

// newRestartConversation 创建并打开用来恢复的持久对话。
func newRestartConversation(t *testing.T, configPath string) (*runtimeObserver, defs.Conversation) {
	t.Helper()
	// 1. 创建对话和独立工作区。
	client, stop := newE2ERuntimeClient(t, configPath)
	go func() {
		for range client.Events() {
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), e2eTimeout)
	defer cancel()
	record, err := client.CreateConversation(ctx, &runtimeclient.CreateConversationRequest{
		AgentName:        "assistant",
		ExecutorSettings: defs.ExecutorSettings{WorkspaceDirectories: map[string]string{defs.LocalExecutorID: newE2EWorkspace(t)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 2. 关闭创建用的客户端，由观察者打开。
	stop()
	return openRuntimeObserver(t, configPath, record.ID), record
}

// listRestartItems 读取一个 Session 的全部 Item。
func listRestartItems(t *testing.T, client runtimeclient.Client, conversation defs.Conversation) []*defs.Item {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), e2eTimeout)
	defer cancel()
	var items []*defs.Item
	request := &runtimeclient.ListItemsRequest{ConversationID: conversation.ID, SessionID: conversation.RootSessionID, Limit: 1000}
	for {
		page, err := client.ListItems(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		for _, logged := range page.Items {
			items = append(items, logged.Item)
		}
		if page.NextCursor == 0 {
			return items
		}
		request.Cursor = page.NextCursor
	}
}

// waitRestartTurn 等待唯一 Turn 记录出现并核对它的状态。
func waitRestartTurn(t *testing.T, client runtimeclient.Client, conversation defs.Conversation, status defs.TurnStatus) defs.Turn {
	t.Helper()
	// 1. 一次至多读两轮；出现第二轮或状态不符即结束测试。
	request := &runtimeclient.ListTurnsRequest{
		ConversationID: conversation.ID,
		SessionID:      conversation.RootSessionID,
		Limit:          2,
	}
	for deadline := time.Now().Add(e2eTimeout); ; time.Sleep(20 * time.Millisecond) {
		ctx, cancel := context.WithTimeout(t.Context(), e2eTimeout)
		page, err := client.ListTurns(ctx, request)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		turns := page.Turns
		if page.NextCursor.ID != "" || len(turns) > 1 || (len(turns) == 1 && turns[0].Status != status) {
			t.Fatalf("Turn records = %+v; want one %s Turn", turns, status)
		}
		if len(turns) == 1 && turns[0].ID != "" {
			return turns[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("no Turn with status %s", status)
		}
	}
}

// waitRestartItem 等待 Item 日志中出现满足条件的第一条 Item。
func waitRestartItem(t *testing.T, client runtimeclient.Client, conversation defs.Conversation, matches func(*defs.Item) bool) *defs.Item {
	t.Helper()
	// 1. 重读日志，直到目标 Item 落盘。
	for deadline := time.Now().Add(e2eTimeout); ; time.Sleep(20 * time.Millisecond) {
		items := listRestartItems(t, client, conversation)
		if item := findRestartItem(items, matches); item != nil {
			return item
		}
		if time.Now().After(deadline) {
			t.Fatalf("Item log did not reach the expected checkpoint: %+v", items)
		}
	}
}

// waitRestartFile 等待测试子进程写完屏障标记。
func waitRestartFile(t *testing.T, path, want string) {
	t.Helper()
	// 1. 等文件出现并写全；命中标记时服务仍停在屏障上。
	for deadline := time.Now().Add(e2eTimeout); ; time.Sleep(20 * time.Millisecond) {
		payload, err := os.ReadFile(path)
		if err == nil && string(payload) == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("file %s = %q, %v; want %q", path, payload, err, want)
		}
	}
}

// checkRestartFile 核对工具留下的文件内容。
func checkRestartFile(t *testing.T, path, want string) {
	t.Helper()
	// 1. 只读取一次工具现场。
	if got, err := os.ReadFile(path); err != nil || string(got) != want {
		t.Fatalf("file %s = %q, %v; want %q", path, got, err, want)
	}
}

// findRestartItem 返回第一条符合条件的 Item。
func findRestartItem(items []*defs.Item, predicate func(*defs.Item) bool) *defs.Item {
	index := slices.IndexFunc(items, predicate)
	if index >= 0 {
		return items[index]
	}
	return nil
}

// countRestartItems 统计符合条件的 Item。
func countRestartItems(items []*defs.Item, predicate func(*defs.Item) bool) int {
	count := 0
	for _, item := range items {
		if predicate(item) {
			count++
		}
	}
	return count
}

// restartItemText 拼接消息 Item 的正文。
func restartItemText(item *defs.Item) string {
	if item == nil || item.Message == nil {
		return ""
	}
	var text strings.Builder
	for _, content := range item.Message.Content {
		if content != nil && content.Text != nil {
			text.WriteString(content.Text.Text)
		}
	}
	return text.String()
}

// getModelToolCallID 返回模型请求中工具消息携带的原始 CallID。
func getModelToolCallID(body map[string]any) string {
	messages, _ := body["messages"].([]any)
	for _, message := range messages {
		fields, _ := message.(map[string]any)
		if fields["role"] == "tool" {
			callID, _ := fields["tool_call_id"].(string)
			return callID
		}
	}
	return ""
}

// readRestartCheckpoint 读取发出实际模型请求前已落盘的最新 Turn checkpoint。
func readRestartCheckpoint(t *testing.T, configPath string, conversation defs.Conversation, turnID defs.TurnID) defs.TurnCheckpoint {
	t.Helper()
	// 1. 按行倒读真实日志，取原 Turn 的最新进度。
	path := filepath.Join(config.GetDataDirectory(filepath.Dir(configPath)), "conversations", string(conversation.ID), "checkpoint-"+string(conversation.RootSessionID)+".jsonl")
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(payload), []byte("\n"))
	for index := len(lines) - 1; index >= 0; index-- {
		var line struct {
			Payload struct {
				Turn *defs.TurnCheckpoint `json:"turn"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(lines[index], &line); err != nil {
			t.Fatalf("decode checkpoint line %d: %v", index+1, err)
		}
		checkpoint := line.Payload.Turn
		if checkpoint == nil || checkpoint.TurnID != turnID {
			continue
		}
		if checkpoint.Phase != defs.TurnCheckpointPhaseRequest || checkpoint.Step != 1 || checkpoint.Request == nil ||
			checkpoint.Request.Purpose != defs.ModelCallPurposeConversation || checkpoint.Request.ModelCallID == "" || checkpoint.Request.ModelCallThroughItemID == "" {
			t.Fatalf("checkpoint = %+v; want first-step request with model call and input cutoff", checkpoint)
		}
		return *checkpoint
	}
	t.Fatalf("no checkpoint for Turn %s", turnID)
	return defs.TurnCheckpoint{}
}

// checkPartialRestartRequest 核对模型收到原输入、没有旧输出。
func checkPartialRestartRequest(t *testing.T, request e2eModelRequest, input, oldOutput string) {
	t.Helper()
	// 1. 比对实际请求的 messages，不输出整段系统提示。
	payload, err := json.Marshal(request.body["messages"])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(payload, []byte(input)) || bytes.Contains(payload, []byte(oldOutput)) {
		t.Fatalf("model messages: original input present=%t, old output %q present=%t", bytes.Contains(payload, []byte(input)), oldOutput, bytes.Contains(payload, []byte(oldOutput)))
	}
}

// checkRestartTurnFinished 核对结束事件和存储中的唯一 Turn。
func checkRestartTurnFinished(t *testing.T, observer *runtimeObserver, conversation defs.Conversation, turnID defs.TurnID, status defs.TurnStatus) {
	t.Helper()
	// 1. 等同一个 TurnID 的事件和终态记录。
	if recoveredTurnID := readTurnFinished(t, observer, status); recoveredTurnID != turnID {
		t.Fatalf("recovered TurnID = %s, want original %s", recoveredTurnID, turnID)
	}
	if turn := waitRestartTurn(t, observer.client, conversation, status); turn.ID != turnID {
		t.Fatalf("persisted TurnID = %s, want original %s", turn.ID, turnID)
	}
}

// restartModel 追踪测试取走的请求；共享模型的队列保留所有尚未取走的实际请求。
type restartModel struct {
	*e2eModel
	// received 是测试已从模型请求队列取走的次数。
	received int
}

func (m *restartModel) next(t *testing.T) e2eModelRequest {
	t.Helper()
	request := m.e2eModel.next(t)
	m.received++
	return request
}

func (m *restartModel) checkRequests(t *testing.T, want int) {
	t.Helper()
	if count := m.received + len(m.requests); count != want {
		t.Fatalf("model received %d HTTP requests, want %d", count, want)
	}
}

// restartCommandBlocker 用回环连接阻塞测试启动的工具或 Hook；连接关闭后 bash 不再等待。
type restartCommandBlocker struct {
	// listener 等工具或 Hook 连入。
	listener *net.TCPListener
}

func newRestartCommandBlocker(t *testing.T) restartCommandBlocker {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return restartCommandBlocker{listener: listener}
}

func (b restartCommandBlocker) command() string {
	return fmt.Sprintf("exec 3<>/dev/tcp/127.0.0.1/%d || exit 1\nprintf 'blocked\\n' >&3\nread -r -t 60 release <&3", b.listener.Addr().(*net.TCPAddr).Port)
}

// waitBlocked 等待工具或 Hook 打开回环连接并发送标记。
func (b restartCommandBlocker) waitBlocked(t *testing.T) *net.TCPConn {
	t.Helper()
	deadline := time.Now().Add(e2eTimeout)
	if err := b.listener.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	connection, err := b.listener.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	if err := connection.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	if marker, err := bufio.NewReader(connection).ReadString('\n'); err != nil || marker != "blocked\n" {
		t.Fatalf("command blocker marker = %q, %v; want blocked", marker, err)
	}
	return connection
}

// writeRestartStopHook 写入等待测试连接放行的 Stop hook。
func writeRestartStopHook(t *testing.T, home, command string) {
	t.Helper()
	// 1. 写入 bash 脚本和 Stop 配置。
	if err := os.MkdirAll(filepath.Join(home, "hooks"), 0700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/bash\ncat >/dev/null\n" + command + "\n"
	if err := os.WriteFile(filepath.Join(home, "hooks", "stop.sh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	hooks := "hooks:\n  Stop:\n    - hooks:\n        - type: command\n          script: stop.sh\n"
	if err := os.WriteFile(filepath.Join(home, "hooks", "stop.yaml"), []byte(hooks), 0600); err != nil {
		t.Fatal(err)
	}
}

// runRestartInterruptServer 用真实持久化、Runtime 和 WebSocket 服务运行测试子进程。
func runRestartInterruptServer(t *testing.T, configPath string) {
	t.Helper()
	// 1. 使用与 serve 相同的配置、SQLite 索引和文件日志，仅在 Append 上设置测试屏障。
	home := filepath.Dir(configPath)
	loadOpt := &config.LoadConfigOpt{Home: home, Path: configPath}
	cfg, err := config.LoadConfig(loadOpt)
	if err != nil {
		t.Fatal(err)
	}
	definitions, err := cfg.GetRuntimeDefinitions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	dataDirectory := config.GetDataDirectory(home)
	if err := os.MkdirAll(dataDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	database, err := storagesqlite.OpenDatabase(storagesqlite.NewDatabaseOpt(filepath.Join(dataDirectory, "metadata.db"), dao.RecordNamespaces()...))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	logs, err := storagefile.NewLogStorage(storagefile.NewLogStorageOpt(filepath.Join(dataDirectory, "conversations")))
	if err != nil {
		t.Fatal(err)
	}
	barrier := &restartInterruptLogStorage{LogStorage: logs, markerPath: filepath.Join(home, restartInterruptMarker)}
	rt, err := runtime.NewRuntime(runtime.NewRuntimeOpt(database.Record(), barrier, definitions))
	if err != nil {
		t.Fatal(err)
	}

	// 2. 绑定回环端口，创建真实 Runtime Server 和客户端连接文件。
	listener, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	const token = "turn-restart-e2e"
	serverOpt := runtimeserver.NewRuntimeServerOpt(rt, cfg.Server.Listen, token)
	serverOpt.Listener = listener
	serverOpt.ServerID = "turn-restart-e2e"
	serverOpt.Configuration = config.NewConfigFiles(loadOpt)
	serverOpt.ModelCatalog, err = modelcatalog.NewCatalog(modelcatalog.NewCatalogOpt(filepath.Join(home, "models-dev.json")))
	if err != nil {
		t.Fatal(err)
	}
	server, err := runtimeserver.NewRuntimeServer(serverOpt)
	if err != nil {
		t.Fatal(err)
	}
	connectionPath := getE2EServerConnectionPath(configPath)
	if err := os.MkdirAll(filepath.Dir(connectionPath), 0700); err != nil {
		t.Fatal(err)
	}
	connection := fmt.Sprintf("url: ws://%s/api/v1/openvaala\ntoken: %s\nserver_id: %s\n", listener.Addr(), token, serverOpt.ServerID)
	if err := os.WriteFile(connectionPath, []byte(connection), 0600); err != nil {
		t.Fatal(err)
	}

	// 3. 运行 Runtime 和 Server；父测试在屏障处强杀子进程。
	ctx, cancel := context.WithCancel(t.Context())
	runtimeDone := make(chan error, 1)
	go func() { runtimeDone <- rt.Run(ctx) }()
	serverErr := server.Run(ctx)
	cancel()
	t.Fatalf("interrupt barrier service exited before kill: server=%v runtime=%v", serverErr, <-runtimeDone)
}

// restartInterruptLogStorage 在写入 interrupted Turn 的 metadata 前停住测试子进程。
type restartInterruptLogStorage struct {
	// LogStorage 执行屏障之外的所有文件日志操作。
	storage.LogStorage
	// markerPath 保存命中屏障的对话和 Turn ID。
	markerPath string
}

// Append 转交给真实文件日志，唯独 interrupted 终态停在写入之前。
func (s *restartInterruptLogStorage) Append(ctx context.Context, path storage.StreamPath, streamID storage.StreamID, payloads ...json.RawMessage) ([]storage.LogEntryInfo, error) {
	// 1. 在目标 metadata 行进入真实 Append 前通知父测试并阻塞。
	if streamID == dao.MetadataStreamID {
		for _, payload := range payloads {
			var metadata struct {
				Kind string     `json:"kind"`
				Turn *defs.Turn `json:"turn"`
			}
			if err := json.Unmarshal(payload, &metadata); err != nil {
				return nil, err
			}
			if metadata.Kind == "turn_saved" && metadata.Turn != nil && metadata.Turn.Status == defs.TurnStatusInterrupted {
				if err := os.WriteFile(s.markerPath, []byte(string(path)+"/"+string(metadata.Turn.ID)), 0600); err != nil {
					return nil, err
				}
				select {}
			}
		}
	}
	// 2. 其他日志正常写入。
	return s.LogStorage.Append(ctx, path, streamID, payloads...)
}
