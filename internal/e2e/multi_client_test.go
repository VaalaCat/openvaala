package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/session"
	acp "github.com/coder/acp-go-sdk"
)

// TestMultiClient 检查四种客户端跟随同一对话时收到的内容一致。
func TestMultiClient(t *testing.T) {
	// 1. 创建共用服务和模型，轮流使用 Codex Unix、Codex stdio、runtimeclient 和 ACP 发起请求。
	binary := buildOpenVaala(t)
	model := newE2EModel(t)
	config := writeE2EConfig(t, model.server.URL)
	f := newMultiClientFixture(t, binary, config, model)
	for _, test := range []struct {
		name string
		// initiators 是轮流做发起方的连接种类，每种各跑一遍 run。
		initiators []multiClientKind
		run        func(t *testing.T, f *multiClientFixture, initiator multiClientKind)
	}{
		{
			name:       "idle_send",
			initiators: multiClientKinds,
			run: func(t *testing.T, f *multiClientFixture, initiator multiClientKind) {
				// 1. 空闲时接入四种连接，模型先回复再调用 bash，最后分段回答。
				record := f.createConversation(t)
				observers := f.attach(t, record, multiClientKinds)
				question := "question from " + string(initiator)
				observers[initiator].startTurn(t, question)
				f.model.next(t).reply <- e2eModelReply{
					text:      "checking",
					tool:      "bash",
					arguments: `{"command":"echo tool-output"}`,
				}
				f.model.next(t).reply <- e2eModelReply{text: "all| done"}
				want := []string{fmt.Sprintf("user %q", question), `assistant "checking"`, "tool bash completed", `assistant "all done"`, "turn completed"}
				if expected := f.assertRecords(t, record, observers); !slices.Equal(expected, want) {
					t.Fatalf("root session log translates to\n%s\nwant\n%s", strings.Join(expected, "\n"), strings.Join(want, "\n"))
				}
				// 2. 比较两条 Codex 用户消息，只有发起连接带 clientId。
				for _, kind := range []multiClientKind{multiClientCodexUnix, multiClientCodexStdio} {
					userMessage, _ := observers[kind].(*codexThreadObserver).live.getItem(t, "item/completed", "userMessage")
					wantClientID := any(nil)
					if kind == initiator {
						wantClientID = multiClientUserMessageID
					}
					if userMessage["clientId"] != wantClientID {
						t.Errorf("%s userMessage clientId = %v, want %v", kind, userMessage["clientId"], wantClientID)
					}
				}
			},
		},
		{
			name:       "join_mid_output",
			initiators: multiClientKinds,
			run: func(t *testing.T, f *multiClientFixture, initiator multiClientKind) {
				// 1. 发起方完成 bash 后暂停回复，其余三种连接在回复中途接入。
				record := f.createConversation(t)
				observers := f.attach(t, record, []multiClientKind{initiator})
				question := "question from " + string(initiator)
				observers[initiator].startTurn(t, question)
				f.model.next(t).reply <- e2eModelReply{
					tool:      "bash",
					arguments: `{"command":"echo tool-output"}`,
				}
				stream := make(chan string)
				f.model.next(t).reply <- e2eModelReply{stream: stream}
				writeModelStream(t, stream, "partial")
				waitRecordLine(t, observers[initiator], "assistant", "partial")
				joined := f.attach(t, record, slices.DeleteFunc(slices.Clone(multiClientKinds), func(kind multiClientKind) bool { return kind == initiator }))
				// 2. 检查新连接先收到累计回复，再补写剩余文字并比较完整记录。
				for kind, observer := range joined {
					waitRecordLine(t, observer, "assistant", "partial")
					observers[kind] = observer
				}
				writeModelStream(t, stream, " and the rest")
				close(stream)
				want := []string{fmt.Sprintf("user %q", question), "tool bash completed", `assistant "partial and the rest"`, "turn completed"}
				if expected := f.assertRecords(t, record, observers); !slices.Equal(expected, want) {
					t.Fatalf("root session log translates to\n%s\nwant\n%s", strings.Join(expected, "\n"), strings.Join(want, "\n"))
				}
				// 3. 检查接入的 Codex 连接先收到 Turn 开始，再收到回复开始和累计增量。
				for _, kind := range []multiClientKind{multiClientCodexUnix, multiClientCodexStdio} {
					if kind != initiator {
						assertCodexJoinedMidMessage(t, string(kind), observers[kind].(*codexThreadObserver).live, "partial")
					}
				}
			},
		},
		{
			name:       "join_mid_sub_agent_output",
			initiators: []multiClientKind{multiClientCodexUnix},
			run: func(t *testing.T, f *multiClientFixture, initiator multiClientKind) {
				// 1. 根 Agent 委派子 Agent，将子 Agent 回复暂停在“child ”。
				record := f.createConversation(t)
				starter := f.attach(t, record, []multiClientKind{initiator})[initiator].(*codexThreadObserver)
				starter.startTurn(t, "delegate")
				f.model.next(t).reply <- e2eModelReply{
					tool:      "spawn_agent",
					arguments: `{"task_name":"review","message":"check"}`,
				}
				afterSpawn, childRequest := f.model.nextRootAndTask(t, "review")
				childReply := make(chan string)
				childRequest.reply <- e2eModelReply{stream: childReply}
				afterSpawn.reply <- e2eModelReply{text: "delegated"}
				writeModelStream(t, childReply, "child ")
				// 2. 发起连接读取子 Thread 开始和第一段回复。
				childID := starter.wire.nextNotification(t, "thread/started").getThreadID()
				children := map[string]*codexThreadObserver{"starter": {
					wire:     starter.wire,
					threadID: childID,
				}}
				waitRecordLine(t, children["starter"], "assistant", "child ")
				// 3. 根 Turn 结束后接入两条 Codex 连接，比较子 Thread 的开始和累计内容。
				starter.readTurnEnd(t)
				for _, joiner := range []struct {
					kind        multiClientKind
					connections *codexConnections
				}{{
					kind:        multiClientCodexUnix,
					connections: f.codexUnix,
				}, {
					kind:        multiClientCodexStdio,
					connections: f.codexStdio,
				}} {
					wire := resumeCodexThread(t, joiner.connections.open(t), string(record.ID)).wire
					child := &codexThreadObserver{
						wire:     wire,
						threadID: childID,
					}
					waitRecordLine(t, child, "assistant", "child ")
					if child.live[0].Method != "thread/started" {
						t.Errorf("joined %s: first sub-agent message is %s, want thread/started", joiner.kind, child.live[0].Method)
					}
					assertCodexJoinedMidMessage(t, "joined "+string(joiner.kind), child.live, "child ")
					children["joined_"+string(joiner.kind)] = child
				}
				writeModelStream(t, childReply, "reply")
				close(childReply)
				// 4. 完成子 Agent 回复和根 Agent 后续 Turn，比较各连接与子 Session 日志。
				f.model.next(t).reply <- e2eModelReply{text: "noted"}
				for _, child := range children {
					child.readTurnEnd(t)
				}
				expected := f.getExpectedRecord(t, record.ID, defs.SessionID(childID))
				if want := []string{`assistant "child reply"`, "turn completed"}; !slices.Equal(expected, want) {
					t.Fatalf("sub-agent session log translates to\n%s\nwant\n%s", strings.Join(expected, "\n"), strings.Join(want, "\n"))
				}
				for name, child := range children {
					if got := child.getRecord(t); !slices.Equal(got, expected) {
						t.Errorf("%s sub-agent record =\n%s\nwant\n%s", name, strings.Join(got, "\n"), strings.Join(expected, "\n"))
					}
				}
			},
		},
		{
			name:       "join_while_tool_runs",
			initiators: multiClientKinds,
			run: func(t *testing.T, f *multiClientFixture, initiator multiClientKind) {
				// 1. 让 bash 等待 FIFO，调用保存后接入其余连接并检查运行中状态。
				record := f.createConversation(t)
				fifo := filepath.Join(record.ExecutorSettings.WorkspaceDirectories[defs.LocalExecutorID], "release.fifo")
				if err := syscall.Mkfifo(fifo, 0600); err != nil {
					t.Fatal(err)
				}
				observers := f.attach(t, record, []multiClientKind{initiator})
				question := "question from " + string(initiator)
				observers[initiator].startTurn(t, question)
				f.model.next(t).reply <- e2eModelReply{
					tool:      "bash",
					arguments: `{"command":"echo before; cat release.fifo; echo after"}`,
				}
				// 2. 等待 FIFO 读端打开，接入观察连接后写入放行文字。
				release := openE2EFIFOWriter(t, fifo)
				joined := f.attach(t, record, slices.DeleteFunc(slices.Clone(multiClientKinds), func(kind multiClientKind) bool { return kind == initiator }))
				for kind, observer := range joined {
					waitRecordLine(t, observer, "tool", "bash inProgress")
					observers[kind] = observer
				}
				if _, err := release.WriteString("released\n"); err != nil {
					t.Fatal(err)
				}
				if err := release.Close(); err != nil {
					t.Fatal(err)
				}
				f.model.next(t).reply <- e2eModelReply{text: "done"}
				want := []string{fmt.Sprintf("user %q", question), "tool bash completed", `assistant "done"`, "turn completed"}
				if expected := f.assertRecords(t, record, observers); !slices.Equal(expected, want) {
					t.Fatalf("root session log translates to\n%s\nwant\n%s", strings.Join(expected, "\n"), strings.Join(want, "\n"))
				}
				// 3. 比较每条连接接入后的实时输出；接入前的 before 不要求补发。
				for _, kind := range multiClientKinds {
					if output := observers[kind].getStreamedToolOutput(t); !strings.Contains(output, "released\nafter\n") {
						t.Errorf("%s streamed tool output %q, want the output produced after joining", kind, output)
					}
				}
			},
		},
		{
			name:       "initiator_disconnects",
			initiators: multiClientKinds,
			run: func(t *testing.T, f *multiClientFixture, initiator multiClientKind) {
				// 1. 接入四种连接，在发起方收到 partial 后断开发起方。
				record := f.createConversation(t)
				observers := f.attach(t, record, multiClientKinds)
				question := "question from " + string(initiator)
				observers[initiator].startTurn(t, question)
				stream := make(chan string)
				f.model.next(t).reply <- e2eModelReply{stream: stream}
				writeModelStream(t, stream, "partial")
				waitRecordLine(t, observers[initiator], "assistant", "partial")
				// 2. 断开发起连接，再写完回复并比较其余连接和已完成 Turn 记录。
				observers[initiator].close(t)
				if initiator == multiClientCodexUnix {
					waitLogMessages(t, f.logPath, "codex connection closed", f.codexUnix.openedCount-1)
				}
				delete(observers, initiator)
				writeModelStream(t, stream, " and the rest")
				close(stream)
				want := []string{fmt.Sprintf("user %q", question), `assistant "partial and the rest"`, "turn completed"}
				if expected := f.assertRecords(t, record, observers); !slices.Equal(expected, want) {
					t.Fatalf("root session log translates to\n%s\nwant\n%s", strings.Join(expected, "\n"), strings.Join(want, "\n"))
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, initiator := range test.initiators {
				t.Run(string(initiator), func(t *testing.T) { test.run(t, f, initiator) })
			}
		})
	}
}

// assertCodexJoinedMidMessage 检查中途接入时 Turn、回复开始和累计增量的顺序。
func assertCodexJoinedMidMessage(t *testing.T, name string, live codexTurnMessages, cumulative string) {
	t.Helper()
	// 1. 定位开始通知和首个增量，比较 Turn 开始顺序及累计文字。
	turnStartedAt := live.indexOf(isCodexMethod("turn/started"))
	_, itemStartedAt := live.getItem(t, "item/started", "agentMessage")
	firstDelta := live[live.indexOf(isCodexMethod("item/agentMessage/delta"))].getNotification(t).Delta
	if turnStartedAt < 0 || turnStartedAt > itemStartedAt || firstDelta != cumulative {
		t.Errorf("%s: turn/started at %d, agentMessage item/started at %d, first delta %q; want turn/started first and the cumulative delta %q",
			name, turnStartedAt, itemStartedAt, firstDelta, cumulative)
	}
}

// TestMultiClientSlowReader 检查慢连接被移除时其他连接仍能收完回复。
func TestMultiClientSlowReader(t *testing.T) {
	// 1. 构建程序，分别在 Unix 和 stdio 模式下运行。
	binary := buildOpenVaala(t)
	for _, mode := range []string{"unix", "stdio"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 打开两条连接并暂停其中一条的读取，再从另一条发起一轮流式回复。
			model := newE2EModel(t)
			config := writeE2EConfig(t, model.server.URL)
			connections := newCodexConnections(t, binary, config, mode)
			starter := connections.open(t)
			threadID := (&codexResumeFixture{
				connections: connections,
				model:       model,
			}).startThread(t, starter)
			stalled := connections.open(t)
			stalled.call(t, "thread/resume", map[string]any{"threadId": threadID, "excludeTurns": true}, nil)
			stalled.pauseReading()

			requestID := starter.request(t, "turn/start", map[string]any{"threadId": threadID, "input": []any{codexTextInput("write a lot")}})
			stream := make(chan string)
			model.next(t).reply <- e2eModelReply{stream: stream}
			logPath := getE2ELogPath(config)
			var written strings.Builder
			var starterMessages codexTurnMessages
			parts, received := 0, 0
			// 2. 分批输出长回复，直到日志出现慢订阅被移除。
			for countLogMessages(t, logPath, "subscriber removed") == 0 {
				if parts >= slowReaderMaxParts {
					t.Fatalf("serve did not drop the stalled subscriber after %d parts of %d bytes", parts, slowReaderPartBytes)
				}
				// 每批输出后等发起方收齐增量，再发送下一批。
				for range slowReaderBatchParts {
					part := fmt.Sprintf("<%06d>", parts)
					part += strings.Repeat("x", slowReaderPartBytes-len(part))
					writeModelStream(t, stream, part)
					written.WriteString(part)
					parts++
				}
				for received < written.Len() {
					messages := starter.readThreadUntil(t, threadID, requestID, isCodexMethod("item/agentMessage/delta"))
					received += len(messages[len(messages)-1].getNotification(t).Delta)
					starterMessages = append(starterMessages, messages...)
				}
			}
			t.Logf("serve dropped the stalled subscriber after %d parts of %d bytes", parts, slowReaderPartBytes)
			close(stream)
			starterMessages = append(starterMessages, starter.readTurn(t, threadID, requestID)...)
			assertCodexTurnCompleted(t, starterMessages, "completed")
			assertCodexReplyText(t, "starting connection", starterMessages, written.String())

			// 3. 恢复慢连接读取；检查完整回复或连接已关闭。
			stalled.resumeReading()
			var stalledMessages codexTurnMessages
			for {
				message, ok := stalled.nextUnlessClosed(t, "messages of "+threadID, func(message codexWireMessage) bool {
					return message.Method != "" && message.getThreadID() == threadID
				})
				if !ok {
					// 慢连接被断开时结束这个场景；stdio 模式等待进程退出并清除预期的断线错误。
					t.Logf("the stalled connection was closed after %d messages of the thread", len(stalledMessages))
					if mode == "stdio" {
						<-stalled.process.exited
						stalled.process.exitErr = nil
					}
					return
				}
				stalledMessages = append(stalledMessages, message)
				if message.Method == "turn/completed" {
					break
				}
			}
			assertCodexTurnCompleted(t, stalledMessages, "completed")
			assertCodexReplyText(t, "stalled connection", stalledMessages, written.String())
		})
	}
}

const (
	// slowReaderPartBytes 是长输出每段的字节数。
	slowReaderPartBytes = 4096
	// slowReaderBatchParts 是每批写多少段，每批之后看一次 serve 的日志。
	slowReaderBatchParts = 64
	// slowReaderMaxParts 是写到多少段还没有关闭订阅就判失败。
	slowReaderMaxParts = 4096
)

// writeModelStream 写出假模型下一段回复。
func writeModelStream(t *testing.T, stream chan<- string, text string) {
	t.Helper()
	// 1. 将文字送入流；超时未被读取时报告 Turn 提前结束。
	select {
	case stream <- text:
	case <-time.After(e2eTimeout):
		t.Fatalf("the model request stopped reading before %.20q: the turn ended early", text)
	}
}

// getE2ELogPath 返回临时 HOME 中进程共用的日志路径。
func getE2ELogPath(config string) string {
	// 1. 按配置目录拼接日志文件路径。
	return filepath.Join(filepath.Dir(config), ".local", "state", "openvaala", "openvaala.log")
}

// countLogMessages 返回日志中指定消息的条数。
func countLogMessages(t *testing.T, logPath, message string) int {
	t.Helper()
	// 1. 读取日志并统计 msg 字段；日志不存在时按空内容处理。
	content, err := os.ReadFile(logPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return strings.Count(string(content), fmt.Sprintf(`"msg":%q`, message))
}

// waitLogMessages 等待日志中指定消息达到要求条数。
func waitLogMessages(t *testing.T, logPath, message string, count int) {
	t.Helper()
	// 1. 间隔统计日志消息，超时则结束测试。
	deadline := time.Now().Add(e2eTimeout)
	for countLogMessages(t, logPath, message) < count {
		if time.Now().After(deadline) {
			t.Fatalf("log has %d %q, want %d", countLogMessages(t, logPath, message), message, count)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// assertCodexReplyText 检查完成回复和累积增量都与预期文字相同。
func assertCodexReplyText(t *testing.T, name string, messages codexTurnMessages, text string) {
	t.Helper()
	// 1. 读取完成 Item 和同 ID 的增量文字，分别与预期比较。
	reply, _ := messages.getItem(t, "item/completed", "agentMessage")
	completed, _ := reply["text"].(string)
	deltas := messages.getDeltaText(t, "item/agentMessage/delta", fmt.Sprint(reply["id"]))
	if completed != text || deltas != text {
		t.Errorf("%s: reply of %d bytes with %d bytes of deltas, want both to be the %d bytes written", name, len(completed), len(deltas), len(text))
	}
}

// ── 多连接夹具 ──

// multiClientKind 是跟随对话的一种连接。
type multiClientKind string

const (
	// multiClientCodexUnix 是监听模式 app-server 上的一条 WebSocket 连接，像 codex --remote。
	multiClientCodexUnix multiClientKind = "codex_unix"
	// multiClientCodexStdio 是一个 stdio 模式的 app-server 进程，像 Paseo。
	multiClientCodexStdio multiClientKind = "codex_stdio"
	// multiClientRuntime 是一个 runtimeclient，像 OpenVaala TUI。
	multiClientRuntime multiClientKind = "runtimeclient"
	// multiClientACP 是一个 openvaala acp 进程，像编辑器。
	multiClientACP multiClientKind = "acp"
)

// multiClientKinds 是全部连接种类，读取和比较按这个顺序。
var multiClientKinds = []multiClientKind{multiClientCodexUnix, multiClientCodexStdio, multiClientRuntime, multiClientACP}

const (
	// multiClientUserMessageID 是 Codex 发起方 turn/start 带的 clientUserMessageId。
	multiClientUserMessageID = "multi-client-message"
	// multiClientFenceText 是 fence 那一轮的用户输入：跟随的 ACP 连接读到它就知道之前那一轮的内容都到了。
	multiClientFenceText = "fence"
)

// multiClientFixture 是 TestMultiClient 各用例共用的假模型、两种 Codex 连接来源，以及测试自己的控制 Client：新建对话、
// 读 Session 日志和发 fence。
type multiClientFixture struct {
	binary, config        string
	logPath               string
	model                 *e2eModel
	codexUnix, codexStdio *codexConnections
	control               runtimeclient.Client
}

// newMultiClientFixture 创建共享服务的四类客户端测试环境。
func newMultiClientFixture(t *testing.T, binary, config string, model *e2eModel) *multiClientFixture {
	t.Helper()
	// 1. 准备两种 Codex 连接，打开首条连接启动服务，再连接控制 Client 并读取其事件。
	f := &multiClientFixture{
		binary:     binary,
		config:     config,
		logPath:    getE2ELogPath(config),
		model:      model,
		codexUnix:  newCodexConnections(t, binary, config, "unix"),
		codexStdio: newCodexConnections(t, binary, config, "stdio"),
	}
	f.codexUnix.open(t)
	f.control, _ = newE2ERuntimeClient(t, config)
	// 控制 Client 不打开对话，只有连接状态事件。
	go func() {
		for range f.control.Events() {
		}
	}()
	return f
}

// createConversation 创建使用新工作目录的测试对话。
func (f *multiClientFixture) createConversation(t *testing.T) defs.Conversation {
	t.Helper()
	// 1. 设置请求超时，使用 assistant 和新的 local 工作目录创建对话。
	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()
	record, err := f.control.CreateConversation(ctx, &runtimeclient.CreateConversationRequest{
		AgentName:        "assistant",
		ExecutorSettings: defs.ExecutorSettings{WorkspaceDirectories: map[string]string{defs.LocalExecutorID: newE2EWorkspace(t)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

// attach 打开指定种类的连接并跟随对话。
func (f *multiClientFixture) attach(t *testing.T, record defs.Conversation, kinds []multiClientKind) map[multiClientKind]multiClientObserver {
	t.Helper()
	// 1. Codex 恢复根 Thread，runtimeclient 打开对话，ACP 加载会话，等首次同步后返回。
	observers := make(map[multiClientKind]multiClientObserver, len(kinds))
	for _, kind := range kinds {
		switch kind {
		case multiClientCodexUnix:
			observers[kind] = resumeCodexThread(t, f.codexUnix.open(t), string(record.ID))
		case multiClientCodexStdio:
			observers[kind] = resumeCodexThread(t, f.codexStdio.open(t), string(record.ID))
		case multiClientRuntime:
			observers[kind] = openRuntimeObserver(t, f.config, record.ID)
		case multiClientACP:
			observers[kind] = loadACPSession(t, startACPWire(t, f.binary, f.config), record)
		}
	}
	return observers
}

// assertRecords 比较各客户端记录与 Session 日志。
func (f *multiClientFixture) assertRecords(t *testing.T, record defs.Conversation, observers map[multiClientKind]multiClientObserver) []string {
	t.Helper()
	// 1. 先读完带 Turn 结束通知的连接，识别需要 fence 输入的 ACP 跟随连接。
	isFenceNeeded := false
	for _, kind := range multiClientKinds {
		if observer, found := observers[kind]; found {
			if !observer.isTurnEndShown() {
				isFenceNeeded = true
				continue
			}
			observer.readTurnEnd(t)
		}
	}
	// 2. 从日志读取期望记录，需要时另发一轮 fence。
	expected := f.getExpectedRecord(t, record.ID, record.RootSessionID)
	if isFenceNeeded {
		f.sendFence(t, record.ID)
	}
	// 3. ACP 跟随方读到 fence 即停止，比较时移除它没有的 Turn 结束行。
	for _, kind := range multiClientKinds {
		observer, found := observers[kind]
		if !found {
			continue
		}
		want := expected
		if !observer.isTurnEndShown() {
			observer.readTurnEnd(t)
			want = slices.DeleteFunc(slices.Clone(expected), func(line string) bool { return strings.HasPrefix(line, "turn ") })
		}
		if got := observer.getRecord(t); !slices.Equal(got, want) {
			t.Errorf("%s record =\n%s\nwant\n%s", kind, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
	return expected
}

// sendFence 提交 fence 输入并让模型回复 fenced。
func (f *multiClientFixture) sendFence(t *testing.T, conversationID defs.ConversationID) {
	t.Helper()
	// 1. 通过控制 Client 按 normal 模式提交 fence，再回复假模型。
	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()
	content := []*defs.Content{{
		Kind: defs.ContentText,
		Text: &defs.TextContent{Text: multiClientFenceText},
	}}
	if _, err := f.control.SubmitInput(ctx, &runtimeclient.SubmitInputRequest{
		ConversationID: conversationID,
		Mode:           session.SessionInputModeNormal,
		Content:        content,
	}); err != nil {
		t.Fatal(err)
	}
	f.model.next(t).reply <- e2eModelReply{text: "fenced"}
}

// getExpectedRecord 读取 Session 日志和 Turn，返回统一格式的期望记录。
func (f *multiClientFixture) getExpectedRecord(t *testing.T, conversationID defs.ConversationID, sessionID defs.SessionID) []string {
	t.Helper()
	// 1. 按游标读完 Session 的全部 Item。
	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()
	var items []*defs.Item
	itemsRequest := &runtimeclient.ListItemsRequest{
		ConversationID: conversationID,
		SessionID:      sessionID,
	}
	for {
		page, err := f.control.ListItems(ctx, itemsRequest)
		if err != nil {
			t.Fatal(err)
		}
		for _, logged := range page.Items {
			items = append(items, logged.Item)
		}
		if page.NextCursor == 0 {
			break
		}
		itemsRequest.Cursor = page.NextCursor
	}
	// 2. 按游标读完 Turn 记录，再转换为统一记录。
	var turns []defs.Turn
	turnsRequest := &runtimeclient.ListTurnsRequest{
		ConversationID: conversationID,
		SessionID:      sessionID,
	}
	for {
		page, err := f.control.ListTurns(ctx, turnsRequest)
		if err != nil {
			t.Fatal(err)
		}
		turns = append(turns, page.Turns...)
		if page.NextCursor.ID == "" {
			break
		}
		turnsRequest.Cursor = page.NextCursor
	}
	return getLogRecord(items, turns, sessionID)
}

// getLogRecord 将日志转换为消息、工具和 Turn 结束记录。
func getLogRecord(items []*defs.Item, turns []defs.Turn, sessionID defs.SessionID) []string {
	// 1. 建立 Turn 状态索引和空记录。
	statuses := make(map[defs.TurnID]defs.TurnStatus, len(turns))
	for _, turn := range turns {
		statuses[turn.ID] = turn.Status
	}
	record := newMultiClientRecord()
	var turnID defs.TurnID
	addTurnEnd := func() {
		if status := statuses[turnID]; turnID != "" && status != "" && status != defs.TurnStatusRunning {
			record.addTurnEnd(string(status))
		}
	}
	// 2. 按日志顺序登记 Item，轮次变化和末尾补已结束 Turn 的终态；无 Turn Item 不切换轮次。
	for _, item := range items {
		if item.TurnID != "" && item.TurnID != turnID {
			addTurnEnd()
			turnID = item.TurnID
		}
		record.addLogItem(item, sessionID)
	}
	addTurnEnd()
	return record.getLines()
}

// waitRecordLine 读取连接直到记录出现指定种类和文字。
func waitRecordLine(t *testing.T, observer multiClientObserver, kind, text string) {
	t.Helper()
	// 1. 检查已有记录，未找到时继续读取下一条消息。
	for !slices.ContainsFunc(observer.getRecord(t), func(line string) bool {
		return strings.HasPrefix(line, kind+" ") && strings.Contains(line, text)
	}) {
		observer.readNext(t)
	}
}

// ── 记录 ──

// multiClientObserver 是跟随对话的一条连接：按到达顺序读本连接收到的这个对话的内容，归一成记录。
type multiClientObserver interface {
	// startTurn 由发起方调用：经本连接提交一轮输入 text。
	startTurn(t *testing.T, text string)
	// readNext 再读一条本连接收到的这个对话的消息或事件。
	readNext(t *testing.T)
	// readTurnEnd 读到本连接上这一轮结束为止；没有 Turn 结束通知的连接读到 fence 的用户消息为止。
	readTurnEnd(t *testing.T)
	// isTurnEndShown 本连接能不能看到这一轮结束？
	isTurnEndShown() bool
	// getRecord 返回到目前为止读到的内容归一后的记录，每项一行（multiClientEntry.String）。
	getRecord(t *testing.T) []string
	// getStreamedToolOutput 返回工具执行期间实时收到的执行输出，不含调用结束时一次给出的完整结果。
	getStreamedToolOutput(t *testing.T) string
	// close 断开本连接。
	close(t *testing.T)
}

// multiClientRecord 是一条连接收到的内容归一后的记录：按首次出现的顺序排列的用户消息（文本）、助手消息（首次出现时的文本加之后的
// 增量，重复或缺少的增量都会让文本不同）、工具调用（名字和最终状态）和 Turn 结束（状态）。推理、子 Agent 活动、用量和
// Thread 状态变化不记。
type multiClientRecord struct {
	entries []*multiClientEntry
	// entriesByKey 按键找到已经记下的项：消息和 Codex 的工具调用按 Item id，日志里的工具调用按 CallID，ACP 按 toolCallId。
	entriesByKey map[string]*multiClientEntry
}

// multiClientEntry 是记录里的一项：kind 是 user、assistant、tool 或 turn；text 是消息文本，name 是工具名，
// status 是工具调用的状态（inProgress、completed、failed）或 Turn 的终态。
type multiClientEntry struct {
	kind, text, name, status string
}

// newMultiClientRecord 创建带内容索引的空记录。
func newMultiClientRecord() *multiClientRecord {
	// 1. 初始化按键查找记录项的映射。
	return &multiClientRecord{entriesByKey: map[string]*multiClientEntry{}}
}

// String 将一条消息、工具或 Turn 状态写成文字。
func (e *multiClientEntry) String() string {
	// 1. 按记录种类格式化文本、工具名和状态。
	switch e.kind {
	case "user", "assistant":
		return fmt.Sprintf("%s %q", e.kind, e.text)
	case "tool":
		return fmt.Sprintf("tool %s %s", e.name, e.status)
	}
	return "turn " + e.status
}

// add 按首次出现顺序登记记录项并返回。
func (r *multiClientRecord) add(key string, entry *multiClientEntry) *multiClientEntry {
	// 1. 非空键已存在时返回原项，否则追加并登记键；空键总是追加。
	if existing := r.entriesByKey[key]; key != "" && existing != nil {
		return existing
	}
	r.entries = append(r.entries, entry)
	if key != "" {
		r.entriesByKey[key] = entry
	}
	return entry
}

// appendText 向已登记消息追加文本增量。
func (r *multiClientRecord) appendText(t *testing.T, key, text string) {
	t.Helper()
	// 1. 按键查找消息，不存在时结束测试，存在时追加文字。
	entry := r.entriesByKey[key]
	if entry == nil {
		t.Fatalf("delta %q for %q arrived before its item", text, key)
	}
	entry.text += text
}

// addTurnEnd 追加一条 Turn 结束状态。
func (r *multiClientRecord) addTurnEnd(status string) {
	// 1. 以空键登记新的 Turn 状态项。
	r.add("", &multiClientEntry{
		kind:   "turn",
		status: status,
	})
}

// getLines 按登记顺序返回每项一行的文字。
func (r *multiClientRecord) getLines() []string {
	// 1. 遍历记录项并调用 String 格式化。
	lines := make([]string, 0, len(r.entries))
	for _, entry := range r.entries {
		lines = append(lines, entry.String())
	}
	return lines
}

// addLogItem 将 Session 的消息和工具 Item 记入统一记录。
func (r *multiClientRecord) addLogItem(item *defs.Item, sessionID defs.SessionID) {
	// 1. 记录用户和本 Session 回复，工具按 CallID 配对；结果更新调用状态，其他内容跳过。
	switch item.Kind {
	case defs.ItemMessage:
		from := item.Message.From
		var text strings.Builder
		for _, content := range item.Message.Content {
			if content.Kind == defs.ContentText {
				text.WriteString(content.Text.Text)
			}
		}
		switch {
		case from != nil && from.Kind == defs.IdentityUser:
			r.add(string(item.ID), &multiClientEntry{
				kind: "user",
				text: text.String(),
			})
		case from != nil && from.Kind == defs.IdentityAgent && from.ID == string(sessionID):
			r.add(string(item.ID), &multiClientEntry{
				kind: "assistant",
				text: text.String(),
			})
		}
	case defs.ItemToolCall:
		call := item.ToolCall
		if call.CallID == "" {
			return
		}
		entry := r.add("call:"+call.CallID, &multiClientEntry{
			kind:   "tool",
			status: "inProgress",
		})
		entry.name = call.Name
		if call.Status == defs.ToolCallInterrupted {
			entry.status = "failed"
		}
	case defs.ItemToolResult:
		entry := r.entriesByKey["call:"+item.ToolResult.CallID]
		if entry == nil {
			return
		}
		entry.status = "completed"
		if item.ToolResult.Error != nil {
			entry.status = "failed"
		}
	}
}

// addCodexItem 将 Codex 消息和工具 Item 记入统一记录。
func (r *multiClientRecord) addCodexItem(item map[string]any) {
	// 1. 记录用户或助手消息；命令、文件变更和 MCP 调用分别更新工具名与状态。
	id := fmt.Sprint(item["id"])
	switch item["type"] {
	case "userMessage":
		var text strings.Builder
		for _, part := range item["content"].([]any) {
			if part := part.(map[string]any); part["type"] == "text" {
				text.WriteString(fmt.Sprint(part["text"]))
			}
		}
		r.add(id, &multiClientEntry{
			kind: "user",
			text: text.String(),
		})
	case "agentMessage":
		r.add(id, &multiClientEntry{
			kind: "assistant",
			text: fmt.Sprint(item["text"]),
		})
	case "commandExecution", "fileChange", "mcpToolCall":
		name := map[any]string{"commandExecution": "bash", "fileChange": "apply_patch"}[item["type"]]
		if name == "" {
			name = fmt.Sprint(item["tool"])
		}
		entry := r.add(id, &multiClientEntry{kind: "tool"})
		entry.name, entry.status = name, fmt.Sprint(item["status"])
	}
}

// ── Codex 观察者 ──

// codexThreadObserver 保存一条 Codex 连接跟随的 Thread：恢复响应中跟随起点之前的 Turn，加上之后按到达顺序
// 读到的这个 Thread 的消息；发起方还有 turn/start 的响应。
type codexThreadObserver struct {
	wire     *codexWire
	threadID string
	// history 是 thread/resume 响应里的 Turn；live 是之后读到的本 Thread 的消息。
	history []codexWireHistoryTurn
	live    codexTurnMessages
	// startRequestID 是本连接发起这一轮的 turn/start 的 id，不是发起方时为 nil。
	startRequestID json.RawMessage
}

// resumeCodexThread 恢复 Codex Thread 并保存响应中的历史。
func resumeCodexThread(t *testing.T, wire *codexWire, threadID string) *codexThreadObserver {
	t.Helper()
	// 1. 调用 thread/resume，保存历史 Turn 并返回观察连接。
	var resumed codexWireThreadRead
	wire.call(t, "thread/resume", map[string]any{"threadId": threadID}, &resumed)
	return &codexThreadObserver{
		wire:     wire,
		threadID: threadID,
		history:  resumed.Thread.Turns,
	}
}

// startTurn 提交 Codex Thread 的一轮输入。
func (o *codexThreadObserver) startTurn(t *testing.T, text string) {
	t.Helper()
	// 1. 发送 turn/start，记录请求 ID 并带上客户端消息 ID。
	o.startRequestID = o.wire.request(t, "turn/start", map[string]any{
		"threadId": o.threadID, "clientUserMessageId": multiClientUserMessageID, "input": []any{codexTextInput(text)},
	})
}

// readNext 读取 Thread 的下一批消息。
func (o *codexThreadObserver) readNext(t *testing.T) {
	t.Helper()
	// 1. 读取至下一条本 Thread 消息，并追加到实时记录。
	o.live = append(o.live, o.wire.readThreadUntil(t, o.threadID, o.startRequestID, func(codexWireMessage) bool { return true })...)
}

// readTurnEnd 读取 Turn 完成通知并检查 Item 事件顺序。
func (o *codexThreadObserver) readTurnEnd(t *testing.T) {
	t.Helper()
	// 1. 收集本轮剩余消息，检查 Item 仅开始一次且早于完成与增量。
	o.live = append(o.live, o.wire.readTurn(t, o.threadID, o.startRequestID)...)
	assertCodexTurnCompleted(t, o.live, o.live[len(o.live)-1].getNotification(t).Turn.Status)
}

// isTurnEndShown Codex 连接是否提供 Turn 结束通知？
func (o *codexThreadObserver) isTurnEndShown() bool {
	// 1. Codex 连接总会收到 turn/completed，直接返回 true。
	return true
}

// getRecord 合并历史与实时消息，返回统一记录。
func (o *codexThreadObserver) getRecord(t *testing.T) []string {
	t.Helper()
	// 1. 先登记历史 Turn 的 Item 和终态，再登记实时 Item、回复增量和 Turn 结束。
	record := newMultiClientRecord()
	for _, turn := range o.history {
		for _, item := range turn.Items {
			record.addCodexItem(item)
		}
		if turn.Status != "inProgress" {
			record.addTurnEnd(turn.Status)
		}
	}
	for _, message := range o.live {
		notification := message.getNotification(t)
		switch message.Method {
		case "item/started", "item/completed":
			record.addCodexItem(notification.Item)
		case "item/agentMessage/delta":
			record.appendText(t, notification.ItemID, notification.Delta)
		case "turn/completed":
			record.addTurnEnd(notification.Turn.Status)
		}
	}
	return record.getLines()
}

// getStreamedToolOutput 拼接 Codex 实时命令输出。
func (o *codexThreadObserver) getStreamedToolOutput(t *testing.T) string {
	t.Helper()
	// 1. 按到达顺序收集 outputDelta 的文字。
	var output strings.Builder
	for _, message := range o.live {
		if message.Method == "item/commandExecution/outputDelta" {
			output.WriteString(message.getNotification(t).Delta)
		}
	}
	return output.String()
}

// close 关闭 Codex 连接。
func (o *codexThreadObserver) close(t *testing.T) {
	t.Helper()
	// 1. 关闭 wire 并等待读取任务结束。
	o.wire.close(t)
}

// ── runtimeclient 观察者 ──

// startTurn 以用户身份向对话提交一条 normal 输入。
func (o *runtimeObserver) startTurn(t *testing.T, text string) {
	t.Helper()
	// 1. 设置超时并提交文本内容，失败时结束测试。
	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()
	content := []*defs.Content{{
		Kind: defs.ContentText,
		Text: &defs.TextContent{Text: text},
	}}
	if _, err := o.client.SubmitInput(ctx, &runtimeclient.SubmitInputRequest{
		ConversationID: o.conversation.ID,
		Mode:           session.SessionInputModeNormal,
		Content:        content,
	}); err != nil {
		t.Fatal(err)
	}
}

// readNext 读取 runtimeclient 的下一条事件。
func (o *runtimeObserver) readNext(t *testing.T) {
	t.Helper()
	// 1. 读取事件并保存到收到的事件列表。
	o.next(t)
}

// readTurnEnd 读取直到下一条 TurnFinished 事件。
func (o *runtimeObserver) readTurnEnd(t *testing.T) {
	t.Helper()
	// 1. 逐条读取事件，收到 TurnFinished 后返回。
	for {
		if _, isFinished := o.next(t).(*runtimeclient.TurnFinished); isFinished {
			return
		}
	}
}

// isTurnEndShown runtimeclient 是否提供 Turn 结束事件？
func (o *runtimeObserver) isTurnEndShown() bool {
	// 1. runtimeclient 总会收到 TurnFinished，直接返回 true。
	return true
}

// getRecord 将 runtimeclient 的同步、增量和保存事件转成统一记录。
func (o *runtimeObserver) getRecord(t *testing.T) []string {
	t.Helper()
	// 1. 按到达顺序记录 Item，用输出位置关联回复增量，Turn 开始时清空位置索引。
	sessionID := o.conversation.RootSessionID
	record := newMultiClientRecord()
	// itemIDs 是本轮各输出位置上的 Item，每轮开始时清空。
	itemIDs := map[int]defs.ItemID{}
	for _, event := range o.received {
		switch event := event.(type) {
		case *runtimeclient.SessionSynced:
			for _, logged := range event.Items {
				record.addLogItem(logged.Item, sessionID)
			}
			for _, streaming := range event.StreamingItems {
				itemIDs[streaming.ItemIndex] = streaming.Item.ID
				record.addLogItem(streaming.Item, sessionID)
			}
		case *runtimeclient.TurnStarted:
			clear(itemIDs)
		case *runtimeclient.ItemChanged:
			if event.Type == agent.AgentEventItemDelta {
				if event.Delta != nil && event.Delta.Text != "" {
					record.appendText(t, string(itemIDs[event.ItemIndex]), event.Delta.Text)
				}
				continue
			}
			if event.Item != nil {
				itemIDs[event.ItemIndex] = event.Item.ID
				record.addLogItem(event.Item, sessionID)
			}
		case *runtimeclient.ItemsSaved:
			for _, logged := range event.Items {
				record.addLogItem(logged.Item, sessionID)
			}
		case *runtimeclient.TurnFinished:
			record.addTurnEnd(string(event.Status))
		}
	}
	return record.getLines()
}

// getStreamedToolOutput 拼接 runtimeclient 的工具进度输出。
func (o *runtimeObserver) getStreamedToolOutput(t *testing.T) string {
	t.Helper()
	// 1. 按事件顺序读取 ToolProgress 并提取执行输出文字。
	var output strings.Builder
	for _, event := range o.received {
		if progress, isProgress := event.(*runtimeclient.ToolProgress); isProgress {
			output.WriteString(executor.GetExecutionOutputText(progress.Progress.Content))
		}
	}
	return output.String()
}

// close 停止 runtimeclient 观察连接。
func (o *runtimeObserver) close(t *testing.T) {
	t.Helper()
	// 1. 调用保存的停止函数。
	o.stop()
}

// ── ACP 观察者 ──

// acpSessionObserver 是 ACP 编辑器打开的一个会话（sessionId 就是对话 id）：session/load 回放的历史和之后的更新，按到达顺序。
// ACP 只有发起方的 session/prompt 响应表示一轮结束，跟随的会话靠 fence 的用户消息确定一轮的内容已经全部到达。
type acpSessionObserver struct {
	editor    *acpWire
	sessionID string
	// messages 是 session/load 之后按到达顺序读到的消息，load 的响应除外。
	messages codexTurnMessages
	// promptRequestID 是本会话发起这一轮的 session/prompt 的 id，不是发起方时为 nil。
	promptRequestID json.RawMessage
}

// loadACPSession 加载 ACP 会话并等待历史回放完成。
func loadACPSession(t *testing.T, editor *acpWire, record defs.Conversation) *acpSessionObserver {
	t.Helper()
	// 1. 按对话 ID 和工作目录调用 session/load，返回会话观察记录。
	editor.call(t, "session/load", map[string]any{
		"sessionId": string(record.ID), "cwd": record.ExecutorSettings.WorkspaceDirectories[defs.LocalExecutorID], "mcpServers": []any{},
	}, nil)
	return &acpSessionObserver{
		editor:    editor,
		sessionID: string(record.ID),
	}
}

// startTurn 提交 ACP 会话的一轮提示。
func (o *acpSessionObserver) startTurn(t *testing.T, text string) {
	t.Helper()
	// 1. 发送 session/prompt 并保存请求 ID。
	o.promptRequestID = o.editor.request(t, "session/prompt", map[string]any{
		"sessionId": o.sessionID, "prompt": []any{map[string]any{"type": "text", "text": text}},
	})
}

// readNext 读取 ACP 连接的下一条消息。
func (o *acpSessionObserver) readNext(t *testing.T) {
	t.Helper()
	// 1. 读取一条消息并追加到会话记录。
	o.messages = append(o.messages, o.editor.wire.next(t, "ACP message", func(codexWireMessage) bool { return true }))
}

// readTurnEnd 读取 prompt 响应或跟随会话的 fence 输入。
func (o *acpSessionObserver) readTurnEnd(t *testing.T) {
	t.Helper()
	// 1. 逐条读取，发起方等待本次响应，跟随方等待 fence 用户消息。
	for {
		o.readNext(t)
		last := o.messages[len(o.messages)-1]
		if o.promptRequestID != nil && isCodexResponse(o.promptRequestID)(last) {
			return
		}
		if o.promptRequestID == nil && last.Method == "session/update" && o.isFence(t, last) {
			return
		}
	}
}

// isTurnEndShown 这条 ACP 连接是否有 Turn 结束通知？只有发起 Turn 的连接有。
func (o *acpSessionObserver) isTurnEndShown() bool {
	// 1. 检查是否保存了 prompt 请求 ID。
	return o.promptRequestID != nil
}

// isFence 消息是否是 fence 用户输入？
func (o *acpSessionObserver) isFence(t *testing.T, message codexWireMessage) bool {
	t.Helper()
	// 1. 读取 UserMessageChunk，检查文字等于 fence 标记。
	chunk := getACPUpdate(t, message).UserMessageChunk
	return chunk != nil && chunk.Content.Text != nil && chunk.Content.Text.Text == multiClientFenceText
}

// getRecord 合并 ACP 文本分块、工具更新和 Turn 结果。
func (o *acpSessionObserver) getRecord(t *testing.T) []string {
	t.Helper()
	// 1. 建立记录并准备连续同类文本分块的拼接。
	record := newMultiClientRecord()
	// openChunk 是最近一条还能接上后续同类分块的消息，被工具调用或 Turn 结束打断后为 nil。
	var openChunk *multiClientEntry
	addChunk := func(kind string, block acp.ContentBlock) {
		if block.Text == nil {
			return
		}
		if openChunk == nil || openChunk.kind != kind {
			openChunk = record.add("", &multiClientEntry{kind: kind})
		}
		openChunk.text += block.Text.Text
	}
	// 2. 文本分块连续拼接，工具更新改写名称和状态，prompt 响应记 Turn 结束，fence 消息终止读取。
	for _, message := range o.messages {
		if o.promptRequestID != nil && isCodexResponse(o.promptRequestID)(message) {
			var result struct {
				StopReason acp.StopReason `json:"stopReason"`
			}
			status := "failed"
			if message.Error == nil && json.Unmarshal(message.Result, &result) == nil {
				status = map[acp.StopReason]string{acp.StopReasonEndTurn: "completed", acp.StopReasonCancelled: "interrupted"}[result.StopReason]
			}
			record.addTurnEnd(status)
			openChunk = nil
			continue
		}
		if message.Method != "session/update" {
			continue
		}
		if o.isFence(t, message) {
			break
		}
		update := getACPUpdate(t, message)
		switch {
		case update.UserMessageChunk != nil:
			addChunk("user", update.UserMessageChunk.Content)
		case update.AgentMessageChunk != nil:
			addChunk("assistant", update.AgentMessageChunk.Content)
		case update.ToolCall != nil:
			entry := record.add("acp:"+string(update.ToolCall.ToolCallId), &multiClientEntry{kind: "tool"})
			entry.name, entry.status = update.ToolCall.Title, getACPToolStatus(update.ToolCall.Status)
			openChunk = nil
		case update.ToolCallUpdate != nil:
			// 没有先发 tool_call 的更新也记一项，名字为空，比较时显出来。
			entry := record.add("acp:"+string(update.ToolCallUpdate.ToolCallId), &multiClientEntry{kind: "tool"})
			if update.ToolCallUpdate.Title != nil {
				entry.name = *update.ToolCallUpdate.Title
			}
			if update.ToolCallUpdate.Status != nil {
				entry.status = getACPToolStatus(*update.ToolCallUpdate.Status)
			}
			openChunk = nil
		}
	}
	return record.getLines()
}

// getStreamedToolOutput 返回 ACP 最近一次执行中工具更新的累计输出。
func (o *acpSessionObserver) getStreamedToolOutput(t *testing.T) string {
	t.Helper()
	// 1. 筛选 in_progress 更新，读取其中最新的文本内容。
	output := ""
	for _, message := range o.messages {
		if message.Method != "session/update" {
			continue
		}
		update := getACPUpdate(t, message).ToolCallUpdate
		if update == nil || update.Status == nil || *update.Status != acp.ToolCallStatusInProgress {
			continue
		}
		for _, content := range update.Content {
			if content.Content != nil && content.Content.Content.Text != nil {
				output = content.Content.Content.Text.Text
			}
		}
	}
	return output
}

// close 关闭 ACP 编辑器连接。
func (o *acpSessionObserver) close(t *testing.T) {
	t.Helper()
	// 1. 关闭 wire 并等待读取任务结束。
	o.editor.wire.close(t)
}

// getACPToolStatus 将 ACP 的等待和执行状态统一为 inProgress。
func getACPToolStatus(status acp.ToolCallStatus) string {
	// 1. pending 和 in_progress 返回 inProgress，其余状态原样返回。
	switch status {
	case acp.ToolCallStatusPending, acp.ToolCallStatusInProgress:
		return "inProgress"
	}
	return string(status)
}
