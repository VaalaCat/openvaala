package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	ovruntime "github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/sessiontoken"
	"github.com/VaalaCat/openvaala/pkg/storage"
	"github.com/VaalaCat/openvaala/pkg/storage/sqlite"
	"github.com/VaalaCat/openvaala/pkg/world"
	"github.com/coder/websocket"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type externalFixture struct {
	*conversationManagementFixture
	peer, audit string
	settings    defs.ExecutorSettings
}

var buildFakeACPOnce = sync.OnceValues(func() (string, error) {
	path := filepath.Join(e2eBuildDirectory, "fakeacp")
	out, err := exec.Command("go", "build", "-o", path, "./fakeacp").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("build fakeacp: %w: %s", err, out)
	}
	return path, nil
})

func newExternalFixture(t *testing.T, args ...string) *externalFixture {
	t.Helper()
	peer, err := buildFakeACPOnce()
	if err != nil {
		t.Fatal(err)
	}
	f := &externalFixture{conversationManagementFixture: newConversationManagementFixture(t, buildOpenVaala(t)), peer: peer, audit: filepath.Join(t.TempDir(), "wire.jsonl")}
	f.configure(t, args...)
	return f
}
func (f *externalFixture) configure(t *testing.T, args ...string) {
	t.Helper()
	encoded, err := json.Marshal(append([]string{"-record", f.audit}, args...))
	if err != nil {
		t.Fatal(err)
	}
	f.writeConfigFiles(t, map[string]string{"agents/codex-acp.yaml": fmt.Sprintf("name: codex-acp\ntype: acp\nacp:\n  command: %q\n  args: %s\n", f.peer, encoded)})
	f.reloadConfig(t)
}
func (f *externalFixture) create(t *testing.T, model string) defs.Conversation {
	t.Helper()
	c, err := f.control.CreateConversation(t.Context(), &runtimeclient.CreateConversationRequest{AgentName: "assistant", ExecutorSettings: f.settings, Model: defs.ModelSelection{Ref: defs.ModelRef{ProviderID: "codex-acp", ModelID: model}}, InitialOptions: []defs.ExternalOptionSelection{{Kind: "model", Value: "small"}, {Kind: "mode", Value: "code"}}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.control.GetSessionByID(t.Context(), c.RootSessionID)
	if err != nil || r.External == nil || r.External.SessionID != "fake-session" {
		t.Fatalf("binding at create response: %+v %v", r, err)
	}
	return c
}

// openExternalWire 用真实 OpenVaala WebSocket 读取未经过客户端同步处理的订阅快照。
func openExternalWire(t *testing.T, config string) *codexWire {
	t.Helper()
	return openExternalSessionWire(t, config, "", "")
}

func openExternalSessionWire(t *testing.T, config string, cid defs.ConversationID, sid defs.SessionID) *codexWire {
	t.Helper()
	server, err := getE2EServerConnection(getE2EServerConnectionPath(config))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), e2eTimeout)
	defer cancel()
	token := server.Token
	if sid != "" {
		token = sessiontoken.Issue(server.Token, cid, sid)
	}
	conn, _, err := websocket.Dial(ctx, server.URL, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + token}}})
	if err != nil {
		t.Fatal(err)
	}
	wire := newCodexWire(&e2eProcess{})
	wire.conn = conn
	go wire.readWebSocket()
	t.Cleanup(func() { wire.close(t) })
	wire.call(t, "initialize", &runtimeprotocol.InitializeParams{ProtocolVersion: runtimeprotocol.ProtocolVersion}, nil)
	return wire
}

func newExternalHost(t *testing.T, args ...string) (world.WorldHost, ovruntime.Runtime, string) {
	t.Helper()
	dir := t.TempDir()
	peer, err := buildFakeACPOnce()
	if err != nil {
		t.Fatal(err)
	}
	audit := filepath.Join(dir, "audit")
	t.Setenv("HOME", dir)
	db, err := sqlite.OpenDatabase(sqlite.NewMemoryDatabaseOpt(dao.RecordNamespaces()...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	local := executor.NewLocalExecutor(&defs.ExecutorDefinition{ID: defs.LocalExecutorID, Kind: defs.ExecutorKindLocal, WorkspaceDirectory: dir})
	definition := &agent.AgentDefinition{
		Name: "peer",
		Type: defs.AgentTypeACP,
		ACP:  &defs.ACPCommand{Command: peer, Args: append([]string{"-record", audit}, args...)},
	}
	opt := ovruntime.NewRuntimeOpt(db.Record(), db.Log(), &ovruntime.Definitions{
		Agents:    []*agent.AgentDefinition{definition},
		Executors: executor.NewConfigExecutorProvider([]executor.Executor{local}),
	})
	opt.DataDirectory = dir
	rt, err := ovruntime.NewRuntime(opt)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- rt.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return world.NewRuntimeHost(rt), rt, audit
}

func waitForWorldACPCall(t *testing.T, audit, text string) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); {
		b, _ := os.ReadFile(audit)
		if strings.Contains(string(b), text) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("missing peer record %q", text)
}

func TestWorldExternalReady(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "restore")
	host, rt, audit := newExternalHost(t, "-load-gate", gate)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	opened, err := host.OpenSession(ctx, world.OpenSessionRequest{Target: "peer", Title: "inbox"})
	if err != nil {
		t.Fatal(err)
	}
	if err = rt.CloseConversation(ctx, &ovruntime.CloseConversationRequest{ConversationID: defs.ConversationID(opened.SessionID)}); err != nil {
		t.Fatal(err)
	}
	opened, err = host.OpenSession(ctx, world.OpenSessionRequest{ExistingSessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	submitted := make(chan struct{})
	type reply struct {
		body string
		err  error
	}
	result := make(chan reply, 1)
	go func() {
		body, err := host.RunInboundTurn(ctx, world.InboundTurnRequest{OpenedSession: opened, Message: world.WorldMessage{MessageID: "inbound", Body: "hello"}}, func() { close(submitted) })
		result <- reply{body, err}
	}()
	waitForWorldACPCall(t, audit, `"method":"session/load"`)
	select {
	case <-submitted:
		t.Fatal("submitted while the external history was restoring")
	case <-time.After(50 * time.Millisecond):
	}
	if err = os.WriteFile(gate, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if got := <-result; got.err != nil || got.body != "OK" {
		t.Fatalf("inbound result = %+v", got)
	}
}

func TestWorldExternalReplyDuringTurn(t *testing.T) {
	host, rt, audit := newExternalHost(t, "-prompt", "wait-cancel")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	opened, err := host.OpenSession(ctx, world.OpenSessionRequest{Target: "peer", Title: "outbox"})
	if err != nil {
		t.Fatal(err)
	}
	cid := defs.ConversationID(opened.SessionID)
	if _, err = rt.SubmitInput(ctx, &ovruntime.SubmitInputRequest{
		ConversationID: cid,
		Mode:           session.SessionInputModeNormal,
		From:           &defs.Address{Kind: defs.IdentityUser, ID: "user"},
		Content:        []*defs.Content{{Kind: defs.ContentText, Text: &defs.TextContent{Text: "held turn"}}},
	}); err != nil {
		t.Fatal(err)
	}
	waitExternalPrompt(t, audit, "held turn")
	if err = host.NotifyOutbound(ctx, world.WorldMessage{ConversationID: opened.SessionID, MessageID: "reply", ReplyBody: "async body"}); err != nil {
		t.Fatal(err)
	}
	sub, err := rt.SubscribeSession(ctx, &ovruntime.SubscribeSessionRequest{ConversationID: cid, SessionID: opened.RootSessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(sub.PendingInputs.Inputs) != 1 {
		t.Fatalf("pending replies = %+v", sub.PendingInputs)
	}
	if _, err = rt.InterruptTurn(ctx, &ovruntime.InterruptTurnRequest{ConversationID: cid}); err != nil {
		t.Fatal(err)
	}
	waitExternalPrompt(t, audit, "async body")
	b, err := os.ReadFile(audit)
	if err != nil || strings.Count(string(b), `"method":"session/cancel"`) != 1 {
		t.Fatalf("cancels after explicit interrupt: %s (%v)", b, err)
	}
}

// TestExternalDetachIdleParent 用真实派活、转出和用户续聊检查父会话的模型输入。
func TestExternalDetachIdleParent(t *testing.T) {
	for _, parent := range []string{"native", "external"} {
		t.Run(parent, func(t *testing.T) {
			gate := filepath.Join(t.TempDir(), "finish")
			f := newExternalFixture(t, "-prompt-gate", gate)
			var root *runtimeObserver
			if parent == "native" {
				root = f.openNewConversation(t)
			} else {
				root = openRuntimeObserver(t, f.config, f.create(t, "default").ID)
			}
			c := root.conversation
			root.startTurn(t, "delegate now")
			var first e2eModelRequest
			if parent == "native" {
				first = f.model.next(t)
			} else {
				waitExternalPrompt(t, f.audit, "delegate now")
			}
			w := openExternalWire(t, f.config)
			var child conversation.SpawnAgentResult
			w.call(t, "session/spawn", map[string]any{"conversation_id": c.ID, "agent_name": "codex-acp", "task_name": "background work", "message": "child task", "notify_on_finish": false}, &child)
			waitExternalPrompt(t, f.audit, "child task")
			if err := os.WriteFile(gate, nil, 0600); err != nil {
				t.Fatal(err)
			}
			if parent == "native" {
				first.reply <- e2eModelReply{text: "delegated"}
			}
			readTurnFinished(t, root, defs.TurnStatusCompleted)
			var target runtimeprotocol.ConversationResult
			w.call(t, "session/detach", runtimeprotocol.SessionParams{ConversationID: c.ID, SessionID: child.SessionID}, &target)
			checkTurns := func(want int) {
				t.Helper()
				turns, err := f.control.ListTurns(t.Context(), &runtimeclient.ListTurnsRequest{ConversationID: c.ID, SessionID: c.RootSessionID})
				if err != nil || len(turns.Turns) != want {
					t.Fatalf("parent turns = %+v, want %d, error %v", turns, want, err)
				}
			}
			checkTurns(1)
			const question = "parent follow up"
			root.startTurn(t, question)
			var prompt string
			if parent == "native" {
				request := f.model.next(t)
				for _, m := range getModelMessageTexts(request.body) {
					prompt += m.text + "\n"
				}
				request.reply <- e2eModelReply{text: "noted"}
			} else {
				prompt = strings.Join(waitExternalPrompt(t, f.audit, question), "\n")
			}
			readTurnFinished(t, root, defs.TurnStatusCompleted)
			checkTurns(2)
			notice := fmt.Sprintf("用户已将子 Agent codex-acp（任务：background work，旧会话 %s）转为独立对话。新对话 ID：%s，新会话 ID：%s。", child.SessionID, target.Conversation.ID, target.Conversation.RootSessionID)
			if strings.Count(prompt, notice) != 1 || strings.Index(prompt, notice) > strings.Index(prompt, question) {
				t.Fatalf("parent prompt missing ordered detach notice: %s", prompt)
			}
		})
	}
}

// TestExternalSideConversation 走 Web /btw 用的临时分叉、上下文追加和提问三个 RPC。
func TestExternalSideConversation(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("configured=%t", enabled), func(t *testing.T) {
			f := newExternalFixture(t)
			config, err := f.control.GetAgent(t.Context(), "codex-acp")
			if err != nil {
				t.Fatal(err)
			}
			config.ACP.ParamOverwrite = map[string]map[string]any{"session/new": {"_meta": map[string]any{"ordinary": true}}}
			if enabled {
				config.ACP.EphemeralParamOverwrite = map[string]map[string]any{"session/new": {"_meta": map[string]any{"temporary": true}}}
			}
			if _, err := f.control.SetAgent(t.Context(), config); err != nil {
				t.Fatal(err)
			}
			source := f.create(t, "large")
			root := openRuntimeObserver(t, f.config, source.ID)
			root.startTurn(t, "source history")
			readTurnFinished(t, root, defs.TurnStatusCompleted)
			w := openExternalWire(t, f.config)
			var snapshot runtimeprotocol.SubscribeSessionResult
			w.call(t, "session/subscribe", runtimeprotocol.SessionParams{ConversationID: source.ID, SessionID: source.RootSessionID}, &snapshot)
			if snapshot.Capabilities == nil || snapshot.Capabilities.SupportsEphemeralSession != enabled {
				t.Fatalf("capabilities = %+v", snapshot.Capabilities)
			}
			checkNew := func(temporary bool) {
				t.Helper()
				data, err := os.ReadFile(f.audit)
				if err != nil {
					t.Fatal(err)
				}
				for _, line := range slices.Backward(bytes.Split(bytes.TrimSpace(data), []byte("\n"))) {
					var frame struct {
						Method string
						Params struct {
							Meta map[string]bool `json:"_meta"`
						}
					}
					if err := json.Unmarshal(line, &frame); err != nil {
						t.Fatal(err)
					}
					if frame.Method == "session/new" {
						_, hasTemporary := frame.Params.Meta["temporary"]
						if !frame.Params.Meta["ordinary"] || hasTemporary != temporary || frame.Params.Meta["temporary"] != temporary {
							t.Fatalf("session/new _meta = %v, temporary = %t", frame.Params.Meta, temporary)
						}
						return
					}
				}
				t.Fatal("missing session/new")
			}
			checkNew(false)
			params := map[string]any{"conversation_id": source.ID, "ephemeral": true}
			if !enabled {
				reply := w.response(t, w.request(t, "conversation/fork", params))
				if reply.Error == nil || reply.Error.Message != "agent codex-acp does not support temporary sessions" {
					t.Fatalf("fork error = %+v", reply.Error)
				}
				return
			}
			var fork runtimeprotocol.ConversationResult
			w.call(t, "conversation/fork", params, &fork)
			checkNew(true)
			side := openRuntimeObserver(t, f.config, fork.Conversation.ID)
			boundary := defs.NewItemID()
			w.call(t, "conversation/appendContextItems", map[string]any{"conversation_id": fork.Conversation.ID, "items": []*defs.Item{{ID: boundary, Kind: defs.ItemClientInstruction, ClientInstruction: &defs.ClientInstruction{Text: "side boundary"}}}}, nil)
			for _, tc := range []struct {
				question string
				first    bool
			}{{"side question", true}, {"side follow up", false}} {
				side.startTurn(t, tc.question)
				readTurnFinished(t, side, defs.TurnStatusCompleted)
				prompt := strings.Join(waitExternalPrompt(t, f.audit, tc.question), "\n")
				if tc.first {
					page, err := f.control.ListTurns(t.Context(), &runtimeclient.ListTurnsRequest{ConversationID: fork.Conversation.ID, SessionID: fork.Conversation.RootSessionID})
					if err != nil || len(page.Turns) != 1 || page.Turns[0].Model.Ref.ModelID != "large" {
						t.Fatalf("side turn should use the source model large: %+v, err %v", page.Turns, err)
					}
					if !strings.Contains(prompt, "source history") || strings.Index(prompt, "source history") > strings.Index(prompt, "side boundary") || strings.Index(prompt, "side boundary") > strings.Index(prompt, tc.question) {
						t.Fatalf("first side prompt = %q", prompt)
					}
				} else if strings.Contains(prompt, "side boundary") {
					t.Fatalf("repeated boundary: %s", prompt)
				}
			}
			rows := f.listItems(t, fork.Conversation.ID, fork.Conversation.RootSessionID)
			if len(rows) < 2 || rows[0].Item.ID != boundary || rows[0].Item.Kind != defs.ItemClientInstruction || rows[1].Item.Message.Content[0].Text.Text != "side question" {
				t.Fatalf("side history = %+v", rows)
			}
		})
	}
}

func TestExternalAgentConversation(t *testing.T) {
	for _, tt := range []struct {
		model         string
		wantModelSets int
	}{{"large", 1}, {"default", 0}} {
		t.Run(tt.model, func(t *testing.T) {
			model := tt.model
			history := filepath.Join(t.TempDir(), "history")
			f := newExternalFixture(t, "-history-file", history)
			if model == "large" {
				if err := os.Remove(filepath.Join(filepath.Dir(f.config), "models", "local.yaml")); err != nil {
					t.Fatal(err)
				}
				f.writeConfigFiles(t, map[string]string{"agents/assistant.yaml": "name: assistant\ninstructions: Test\n"})
				f.reloadConfig(t)
			}
			c := f.create(t, model)
			wire, err := os.ReadFile(f.audit)
			if !bytes.Contains(wire, []byte(c.RootSessionID)) || !bytes.Contains(wire, []byte(c.ID)) || !bytes.Contains(wire, []byte(`"hasToken":true`)) {
				t.Fatal("session identity missing from ACP environment")
			}
			if err != nil || !strings.Contains(string(wire), "session/set_mode") || strings.Count(string(wire), `"method":"session/set_config_option"`) != tt.wantModelSets {
				t.Fatalf("initial options %s %v", wire, err)
			}
			observer := openRuntimeObserver(t, f.config, c.ID)
			observer.startTurn(t, "original")
			readTurnFinished(t, observer, defs.TurnStatusCompleted)
			usage, err := f.control.GetContextUsage(t.Context(), c.ID, c.RootSessionID)
			if err != nil || usage.NextRequest.Tokens != 24 || usage.NextRequest.Window != 100 || usage.NextRequest.Source != "actual" {
				t.Fatalf("context usage: %+v %v", usage, err)
			}
			rows, err := f.control.ListTurns(t.Context(), &runtimeclient.ListTurnsRequest{ConversationID: c.ID, SessionID: c.RootSessionID})
			if err != nil || len(rows.Turns) != 1 || rows.Turns[0].Usage.Input != 12 {
				t.Fatalf("session usage: %+v %v", rows, err)
			}
			statusWire := openExternalWire(t, f.config)
			var status runtimeprotocol.SubscribeConversationResult
			statusWire.call(t, "conversation/subscribe", runtimeprotocol.ConversationParams{ConversationID: c.ID}, &status)
			if len(status.Sessions) != 1 || status.Sessions[0].Usage.Input != 12 || status.Sessions[0].Context.Window != 100 {
				t.Fatalf("session list usage: %+v", status.Sessions)
			}
			statusWire.close(t)
			directory := f.getConversationDirectory(c.ID)
			if _, err := os.Stat(filepath.Join(directory, string(c.RootSessionID)+".jsonl")); !os.IsNotExist(err) {
				t.Fatalf("external Item stream on disk: %v", err)
			}
			metadata, err := os.ReadFile(filepath.Join(directory, "metadata.jsonl"))
			if err != nil || strings.Contains(string(metadata), "turn_saved") {
				t.Fatalf("on-disk Turn metadata: %v", err)
			}
			db, err := sql.Open("sqlite", "file:"+filepath.Join(filepath.Dir(filepath.Dir(directory)), "metadata.db")+"?mode=ro")
			if err != nil {
				t.Fatal(err)
			}
			var diskTurns int
			err = db.QueryRowContext(t.Context(), "select count(*) from turns").Scan(&diskTurns)
			db.Close()
			if err != nil || diskTurns != 0 {
				t.Fatalf("disk Turn index: %d %v", diskTurns, err)
			}
			observer.stop()
			f.stopServe(t)
			f.startServe(t)
			p, err := f.control.ListItems(t.Context(), &runtimeclient.ListItemsRequest{ConversationID: c.ID, SessionID: c.RootSessionID, Limit: 8})
			if err != nil || len(p.Items) != 1 {
				t.Fatalf("replayed history: %+v %v", p, err)
			}
			observer = openRuntimeObserver(t, f.config, c.ID)
			observer.startTurn(t, "next")
			readTurnFinished(t, observer, defs.TurnStatusCompleted)
			prompt, err := os.ReadFile(history)
			if err != nil || strings.Contains(string(prompt), "original") || !strings.Contains(string(prompt), "next") {
				t.Fatalf("second prompt %s %v", prompt, err)
			}
		})
	}
}
func TestExternalAgentStdioMCP(t *testing.T) {
	f := newExternalFixture(t, "-http=false", "-exercise-mcp")
	wantExecutable, err := os.Stat(f.binary)
	if err != nil {
		t.Fatal(err)
	}
	record := f.create(t, "default")
	for _, method := range []string{"session/new", "session/load"} {
		t.Run(method, func(t *testing.T) {
			if method == "session/load" {
				f.stopServe(t)
				f.startServe(t)
			}
			observer := openRuntimeObserver(t, f.config, record.ID)
			defer observer.stop()
			observer.startTurn(t, "call stdio tools")
			readTurnFinished(t, observer, defs.TurnStatusCompleted)
			data, err := os.ReadFile(f.audit)
			if err != nil {
				t.Fatal(err)
			}
			server, err := getE2EServerConnection(getE2EServerConnectionPath(f.config))
			if err != nil {
				t.Fatal(err)
			}
			token := sessiontoken.Issue(server.Token, record.ID, record.RootSessionID)
			var foundDefinition bool
			var calls int
			for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
				var row struct {
					Method string
					Params json.RawMessage
				}
				if err := json.Unmarshal(line, &row); err != nil {
					t.Fatal(err)
				}
				switch row.Method {
				case method:
					var open struct {
						MCPServers []struct {
							Name, Command string
							Args          []string
							Env           []struct{ Name, Value string }
						}
					}
					if err := json.Unmarshal(row.Params, &open); err != nil {
						t.Fatal(err)
					}
					for _, entry := range open.MCPServers {
						if entry.Name != "openvaala" {
							continue
						}
						if !filepath.IsAbs(entry.Command) {
							t.Fatalf("bridge command is not absolute: %q", entry.Command)
						}
						executable, err := os.Stat(entry.Command)
						if err != nil || !os.SameFile(executable, wantExecutable) {
							t.Fatalf("bridge command = %q, want %q: %v", entry.Command, f.binary, err)
						}
						if !slices.Equal(entry.Args, []string{"mcp-bridge"}) ||
							!slices.ContainsFunc(entry.Env, func(variable struct{ Name, Value string }) bool {
								return variable.Name == "OPENVAALA_TOKEN" && variable.Value == token
							}) {
							t.Fatal("unexpected stdio MCP definition")
						}
						foundDefinition = true
					}
				case "stdio-mcp-results":
					var outputs []string
					if err := json.Unmarshal(row.Params, &outputs); err != nil {
						t.Fatal(err)
					}
					if len(outputs) != 3 || !strings.Contains(outputs[0], `"applied_filter"`) ||
						!strings.Contains(outputs[1], `"terminals"`) || !strings.Contains(outputs[2], `"output":"stdio-mcp"`) {
						t.Fatalf("tool results: %q", outputs)
					}
					calls++
				}
			}
			wantCalls := 1
			if method == "session/load" {
				wantCalls = 2
			}
			if !foundDefinition || calls != wantCalls {
				t.Fatalf("found definition=%t, tool calls=%d, want %d", foundDefinition, calls, wantCalls)
			}
		})
	}
}

func TestExternalAgentRestore(t *testing.T) {
	t.Run("queue_during_restore", func(t *testing.T) {
		gate := filepath.Join(t.TempDir(), "go")
		f := newExternalFixture(t, "-load-gate", gate, "-history-count", "2")
		c := f.create(t, "default")
		f.stopServe(t)
		f.startServe(t)
		wire := openExternalWire(t, f.config)
		params := runtimeprotocol.SessionParams{ConversationID: c.ID, SessionID: c.RootSessionID}
		var snapshot runtimeprotocol.SubscribeSessionResult
		wire.call(t, "session/subscribe", params, &snapshot)
		if !snapshot.Restoring {
			t.Fatal("cold session was already ready")
		}
		ctx, cancel := context.WithTimeout(t.Context(), e2eTimeout)
		defer cancel()
		id, err := f.control.SubmitInput(ctx, &runtimeclient.SubmitInputRequest{ConversationID: c.ID, Content: []*defs.Content{{Kind: defs.ContentText, Text: &defs.TextContent{Text: "queued"}}}})
		if err != nil || id == "" {
			t.Fatalf("enqueue during restore: %s %v", id, err)
		}
		if err = os.WriteFile(gate, nil, 0600); err != nil {
			t.Fatal(err)
		}
		wire.nextNotification(t, "subscription/closed")
		for {
			turns, err := f.control.ListTurns(ctx, &runtimeclient.ListTurnsRequest{ConversationID: c.ID, SessionID: c.RootSessionID})
			if err != nil {
				t.Fatal(err)
			}
			if len(turns.Turns) == 3 && turns.Turns[2].Status == defs.TurnStatusCompleted {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal("queued turn did not finish")
			case <-time.After(20 * time.Millisecond):
			}
		}
	})

	t.Run("retry_partial_replay", func(t *testing.T) {
		marker := filepath.Join(t.TempDir(), "fail")
		f := newExternalFixture(t, "-fail-load-file", marker, "-history-count", "4")
		c := f.create(t, "default")
		f.stopServe(t)
		if err := os.WriteFile(marker, nil, 0600); err != nil {
			t.Fatal(err)
		}
		f.startServe(t)
		req := &runtimeclient.ListItemsRequest{ConversationID: c.ID, SessionID: c.RootSessionID}
		if _, err := f.control.ListItems(t.Context(), req); err == nil || !strings.Contains(err.Error(), "fixture partial replay") {
			t.Fatalf("partial load: %v", err)
		}
		if err := os.Remove(marker); err != nil {
			t.Fatal(err)
		}
		p, err := f.control.ListItems(t.Context(), req)
		if err != nil || len(p.Items) != 4 || p.Items[0].Ordinal != 1 || p.Items[3].Ordinal != 4 {
			t.Fatalf("retry has partial history: %+v %v", p, err)
		}
	})

	for _, count := range []int{5000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			f := newExternalFixture(t, "-history-count", fmt.Sprint(count))
			c := f.create(t, "default")
			f.stopServe(t)
			f.startServe(t)
			wire := openExternalWire(t, f.config)
			params := runtimeprotocol.SessionParams{ConversationID: c.ID, SessionID: c.RootSessionID}
			var snapshot runtimeprotocol.SubscribeSessionResult
			wire.call(t, "session/subscribe", params, &snapshot)
			if !snapshot.Restoring || snapshot.LastSavedOrdinal != 0 {
				t.Fatalf("during replay: %+v", snapshot)
			}
			p, err := f.control.ListItems(t.Context(), &runtimeclient.ListItemsRequest{ConversationID: c.ID, SessionID: c.RootSessionID, Order: storage.DirectionDesc, Limit: 20})
			if err != nil || len(p.Items) != 20 || p.NextCursor == 0 || int(p.Items[0].Ordinal) != count {
				t.Fatalf("latest replay page: %+v %v", p, err)
			}
			wire.nextNotification(t, "subscription/closed")
			snapshot = runtimeprotocol.SubscribeSessionResult{}
			wire.call(t, "session/subscribe", params, &snapshot)
			if snapshot.Restoring || int(snapshot.LastSavedOrdinal) != count || snapshot.Options == nil || snapshot.Capabilities == nil || len(snapshot.Commands) == 0 {
				t.Fatalf("after replay: %+v", snapshot)
			}

		})
	}
}
func TestExternalAgentRecoveryErrors(t *testing.T) {
	t.Parallel()
	t.Run("native_cold_subscription", func(t *testing.T) {
		f := newExternalFixture(t)
		observer := f.openNewConversation(t)
		c := observer.conversation
		f.runTurn(t, observer, "save", e2eModelReply{text: "done"})
		observer.stop()
		f.stopServe(t)
		f.startServe(t)
		wire := openExternalWire(t, f.config)
		params := runtimeprotocol.SessionParams{ConversationID: c.ID, SessionID: c.RootSessionID}
		var snapshot runtimeprotocol.SubscribeSessionResult
		wire.call(t, "session/subscribe", params, &snapshot)
		if snapshot.Restoring || snapshot.Options != nil || snapshot.LastSavedOrdinal == 0 {
			t.Fatalf("cold native snapshot: %+v", snapshot)
		}
		_, err := f.control.SetSessionModel(t.Context(), &runtimeclient.SetSessionModelRequest{ConversationID: c.ID, SessionID: c.RootSessionID, Model: defs.ModelSelection{Ref: defs.ModelRef{ProviderID: "codex-acp", ModelID: "large"}}})
		if err == nil || !strings.Contains(err.Error(), "cannot switch agent") {
			t.Fatalf("native switched to ACP: %v", err)
		}
	})
	t.Run("crash_then_manual_retry", func(t *testing.T) {
		marker := filepath.Join(t.TempDir(), "exit")
		if err := os.WriteFile(marker, nil, 0600); err != nil {
			t.Fatal(err)
		}
		f := newExternalFixture(t, "-exit-prompt-file", marker)
		c := f.create(t, "default")
		observer := openRuntimeObserver(t, f.config, c.ID)
		observer.startTurn(t, "first input")
		turn := readTurnFinished(t, observer, defs.TurnStatusFailed)
		page, err := f.control.ListItems(t.Context(), &runtimeclient.ListItemsRequest{ConversationID: c.ID, SessionID: c.RootSessionID})
		if err != nil || len(page.Items) == 0 || page.Items[0].Item.TurnID != turn || page.Items[0].Item.Message.Content[0].Text.Text != "first input" {
			t.Fatalf("failed turn lost input: %+v %v", page, err)
		}
		if err := os.Remove(marker); err != nil {
			t.Fatal(err)
		}
		observer.startTurn(t, "retry")
		readTurnFinished(t, observer, defs.TurnStatusCompleted)
	})
	t.Run("create_missing_command", func(t *testing.T) {
		f := newExternalFixture(t)
		f.peer = filepath.Join(t.TempDir(), "absent")
		f.configure(t)
		_, err := f.control.CreateConversation(t.Context(), &runtimeclient.CreateConversationRequest{AgentName: "codex-acp"})
		if err == nil {
			t.Fatal("create accepted missing ACP command")
		}
		list, err := f.control.ListConversations(t.Context(), &runtimeclient.ListConversationsRequest{})
		if err != nil || len(list.Conversations) != 0 {
			t.Fatalf("failed create left conversations: %+v %v", list, err)
		}
	})
	t.Run("first_binding_storage_failure", func(t *testing.T) {
		f := newExternalFixture(t)
		directory := f.getConversationDirectory("")
		db, err := sql.Open("sqlite", filepath.Join(filepath.Dir(directory), "metadata.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.ExecContext(t.Context(), `CREATE TRIGGER fail_first_binding BEFORE INSERT ON conversations BEGIN SELECT RAISE(ABORT,'fixture binding failure'); END`); err != nil {
			t.Fatal(err)
		}
		_, err = f.control.CreateConversation(t.Context(), &runtimeclient.CreateConversationRequest{AgentName: "codex-acp"})
		if err == nil || !strings.Contains(err.Error(), "fixture binding failure") {
			t.Fatalf("create returned %v", err)
		}
		for _, table := range []string{"sessions", "conversations"} {
			var count int
			if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 0 {
				t.Fatalf("partial %s: %d, %v", table, count, err)
			}
		}
		files, err := filepath.Glob(filepath.Join(directory, "*", "metadata.jsonl"))
		if err != nil || len(files) != 0 {
			t.Fatalf("partial WAL: %v %v", files, err)
		}
	})
	t.Run("backend_rejects_new_root", func(t *testing.T) {
		f := newExternalFixture(t)
		source := f.create(t, "default")
		f.configure(t, "-fail-new")
		for _, fork := range []bool{false, true} {
			var err error
			if fork {
				_, err = f.control.ForkConversation(t.Context(), &runtimeclient.ForkConversationRequest{ConversationID: source.ID})
			} else {
				_, err = f.control.CreateConversation(t.Context(), &runtimeclient.CreateConversationRequest{AgentName: "codex-acp"})
			}
			if err == nil || !strings.Contains(err.Error(), "fixture rejected new session") {
				t.Fatalf("fork=%t: %v", fork, err)
			}
		}
		list, err := f.control.ListConversations(t.Context(), &runtimeclient.ListConversationsRequest{})
		if err != nil || len(list.Conversations) != 1 || list.Conversations[0].Conversation.ID != source.ID {
			t.Fatalf("conversations after failed creation: %+v %v", list, err)
		}
	})
	t.Run("persisted_native_type", func(t *testing.T) {
		f := newExternalFixture(t)
		observer := f.openNewConversation(t)
		c := observer.conversation
		_, turnID := f.runTurn(t, observer, "original", e2eModelReply{text: "native answer"})
		observer.stop()
		f.stopServe(t)
		f.writeConfigFiles(t, map[string]string{"agents/assistant.yaml": fmt.Sprintf("name: assistant\ntype: acp\nacp:\n  command: %q\n", f.peer)})
		f.startServe(t)
		_, err := f.control.OpenConversation(t.Context(), &runtimeclient.OpenConversationRequest{ConversationID: c.ID})
		if err == nil || !strings.Contains(err.Error(), "agent assistant type changed from native to acp") {
			t.Fatalf("open changed type: %v", err)
		}
		items, err := f.control.ListItems(t.Context(), &runtimeclient.ListItemsRequest{ConversationID: c.ID, SessionID: c.RootSessionID})
		if err != nil || len(items.Items) != 2 || items.Items[0].Item.TurnID != turnID {
			t.Fatalf("native history: %+v %v", items, err)
		}
		r, err := f.control.GetSessionByID(t.Context(), c.RootSessionID)
		if err != nil || r.External != nil {
			t.Fatalf("native identity %+v %v", r, err)
		}
		_, err = f.control.ForkConversation(t.Context(), &runtimeclient.ForkConversationRequest{ConversationID: c.ID})
		if err == nil || !strings.Contains(err.Error(), "agent assistant type changed from native to acp") {
			t.Fatalf("fork changed type: %v", err)
		}
	})
	t.Run("persisted_external_type", func(t *testing.T) {
		f := newExternalFixture(t)
		c := f.create(t, "default")
		f.stopServe(t)
		f.writeConfigFiles(t, map[string]string{"agents/codex-acp.yaml": "name: codex-acp\ninstructions: Test\n"})
		f.startServe(t)
		_, err := f.control.ListItems(t.Context(), &runtimeclient.ListItemsRequest{ConversationID: c.ID, SessionID: c.RootSessionID})
		if err == nil || !strings.Contains(err.Error(), "agent codex-acp type changed from acp to native") {
			t.Fatalf("read changed type: %v", err)
		}
		r, err := f.control.GetSessionByID(t.Context(), c.RootSessionID)
		if err != nil || r.External == nil || r.External.SessionID != "fake-session" {
			t.Fatalf("external binding: %+v %v", r, err)
		}
	})
	t.Run("missing_command", func(t *testing.T) {
		f := newExternalFixture(t)
		c := f.create(t, "default")
		f.stopServe(t)
		f.peer = filepath.Join(t.TempDir(), "missing")
		f.writeConfigFiles(t, map[string]string{"agents/codex-acp.yaml": "name: codex-acp\ntype: acp\nacp:\n  command: " + f.peer + "\n"})
		f.startServe(t)
		_, err := f.control.ListItems(t.Context(), &runtimeclient.ListItemsRequest{ConversationID: c.ID, SessionID: c.RootSessionID})
		if err == nil || !strings.Contains(err.Error(), "restore") {
			t.Fatalf("missing program: %v", err)
		}
	})

	for _, args := range [][]string{{"-load=false"}, {"-history-file", "/missing/openvaala-session"}} {
		t.Run(args[0], func(t *testing.T) {
			f := newExternalFixture(t, args...)
			c := f.create(t, "default")
			f.stopServe(t)
			f.startServe(t)
			_, err := f.control.ListItems(t.Context(), &runtimeclient.ListItemsRequest{ConversationID: c.ID, SessionID: c.RootSessionID})
			_, forkErr := f.control.ForkConversation(t.Context(), &runtimeclient.ForkConversationRequest{ConversationID: c.ID})
			if forkErr == nil || !strings.Contains(forkErr.Error(), "restore") {
				t.Fatalf("fork did not report recovery failure: %v", forkErr)
			}
			if err == nil || !strings.Contains(err.Error(), "restore") {
				t.Fatalf("expected restore failure: %v", err)
			}
		})
	}
}
func TestExternalAgentLocations(t *testing.T) {
	t.Run("reloaded_history_projection", func(t *testing.T) {
		gate := filepath.Join(t.TempDir(), "ready")
		f := newExternalFixture(t)
		c := f.create(t, "default")
		observer := openRuntimeObserver(t, f.config, c.ID)
		observer.startTurn(t, "before reload")
		old := readTurnFinished(t, observer, defs.TurnStatusCompleted)
		observer.stop()
		f.configure(t, "-history-count", "3", "-load-gate", gate)
		wire := openExternalWire(t, f.config)
		var snapshot runtimeprotocol.SubscribeSessionResult
		target := runtimeprotocol.SessionParams{ConversationID: c.ID, SessionID: c.RootSessionID}
		wire.call(t, "session/subscribe", target, &snapshot)
		if !snapshot.Restoring {
			t.Fatal("expected restoration")
		}
		if err := os.WriteFile(gate, nil, 0600); err != nil {
			t.Fatal(err)
		}
		wire.nextNotification(t, "subscription/closed")
		var status runtimeprotocol.SubscribeConversationResult
		wire.call(t, "conversation/subscribe", runtimeprotocol.ConversationParams{ConversationID: c.ID}, &status)
		turns, err := f.control.ListTurns(t.Context(), &runtimeclient.ListTurnsRequest{ConversationID: c.ID, SessionID: c.RootSessionID})
		if err != nil || len(turns.Turns) != 3 || len(status.Sessions) != 1 {
			t.Fatalf("history: %+v %+v %v", turns, status, err)
		}
		info := status.Sessions[0]
		last := turns.Turns[len(turns.Turns)-1]
		if info.LastTurnID == old || info.LastTurnID != last.ID || info.LastTurnStatus != last.Status || info.Usage != (defs.TokenUsage{}) {
			t.Fatalf("restored info: %+v; last=%+v", info, last)
		}
	})
	t.Run("ssh_reload", func(t *testing.T) {
		f := newExternalFixture(t)
		remote := t.TempDir()
		address, key := startExternalTestSSH(t, remote)
		f.writeConfigFiles(t, map[string]string{"executors/remote.yaml": fmt.Sprintf("id: remote\nkind: ssh\ndescription: SSH test\nworkspace_directory: %q\nssh:\n  address: %q\n  identity_file: %q\n", remote, address, key)})
		f.reloadConfig(t)
		f.writeConfigFiles(t, map[string]string{
			"mcp_servers/ssh-tools.yaml": "id: ssh-tools\ntype: stdio\ncommand: true-mcp-fixture\nexecutors: [remote]\n",
			"mcp_servers/ssh-web.yaml":   "id: ssh-web\ntype: http\nurl: http://127.0.0.1:1/mcp\nexecutors: [remote]\n",
			"agents/assistant.yaml":      "name: assistant\ninstructions: Be concise.\nmodel: {provider: local, model: default}\nmcp_servers: []\n",
		})
		f.reloadConfig(t)
		f.settings = defs.ExecutorSettings{DefaultExecutorID: "remote"}
		c := f.create(t, "large")
		r, err := f.control.GetSessionByID(t.Context(), c.RootSessionID)
		if err != nil || r.External.ExecutorID != "remote" || r.External.Cwd != remote {
			t.Fatalf("SSH binding: %+v %v", r.External, err)
		}
		f.configure(t, "-http=false")
		beforeRead, err := os.ReadFile(f.audit)
		if err != nil || bytes.Contains(beforeRead, []byte("session/load")) {
			t.Fatalf("global reload launched an unused ACP peer: %v", err)
		}
		d, err := f.control.GetSessionDetails(t.Context(), c.ID, c.RootSessionID)
		if err != nil || d.Session.External.Cwd != remote || d.Capabilities == nil || !slices.Equal(d.MCPServers, []string{"ssh-tools", "openvaala"}) {
			t.Fatalf("reloaded SSH details: %+v %v", d, err)
		}
		audit, err := os.ReadFile(f.audit)
		if err != nil {
			t.Fatal(err)
		}
		var bridgeCommand string
		for _, line := range bytes.Split(bytes.TrimSpace(audit), []byte("\n")) {
			var entry struct {
				Method string
				Params struct {
					MCPServers []struct{ Name, Command string }
				}
			}
			if err := json.Unmarshal(line, &entry); err != nil {
				t.Fatal(err)
			}
			if entry.Method == "session/load" {
				for _, server := range entry.Params.MCPServers {
					if server.Name == "openvaala" {
						bridgeCommand = server.Command
					}
				}
			}
		}
		if bridgeCommand != "openvaala" {
			t.Fatalf("SSH bridge command = %q, want openvaala from PATH", bridgeCommand)
		}
	})

	for _, cold := range []bool{false, true} {
		t.Run(fmt.Sprintf("cold_%t", cold), func(t *testing.T) {
			f := newExternalFixture(t)
			c := f.create(t, "default")
			if cold {
				f.stopServe(t)
				f.startServe(t)
			} else {
				f.reloadConfig(t)
			}
			for _, provider := range []string{"codex-acp", "local", "other-agent"} {
				r, err := f.control.SetSessionModel(t.Context(), &runtimeclient.SetSessionModelRequest{ConversationID: c.ID, SessionID: c.RootSessionID, Model: defs.ModelSelection{Ref: defs.ModelRef{ProviderID: provider, ModelID: "large"}}})
				if provider == "codex-acp" {
					if err != nil || r.ID != c.RootSessionID || r.External == nil || r.Model != (defs.ModelSelection{}) {
						t.Fatalf("same agent: %+v %v", r, err)
					}
				} else if err == nil || !strings.Contains(err.Error(), "cannot switch agent in an existing session") {
					t.Fatalf("cross-agent %s: %v", provider, err)
				}
			}
		})
	}
}

// startExternalTestSSH 启动临时公钥认证服务，SFTP 和非 TTY 命令共享隔离的测试目录。
func startExternalTestSSH(t *testing.T, directory string) (string, string) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := ssh.MarshalPrivateKey(key, "e2e")
	if err != nil {
		t.Fatal(err)
	}
	identity := filepath.Join(directory, "id_ed25519")
	if err = os.WriteFile(identity, pem.EncodeToMemory(private), 0600); err != nil {
		t.Fatal(err)
	}
	allowed, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		if !bytes.Equal(k.Marshal(), allowed.Marshal()) {
			return nil, errors.New("unknown key")
		}
		return nil, nil
	}}
	cfg.AddHostKey(signer)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, channels, req, err := ssh.NewServerConn(conn, cfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(req)
				for c := range channels {
					if c.ChannelType() != "session" {
						c.Reject(ssh.UnknownChannelType, "unsupported")
						continue
					}
					ch, reqs, err := c.Accept()
					if err != nil {
						continue
					}
					go serveExternalSSHChannel(ch, reqs, directory)
				}
			}()
		}
	}()
	return "test@" + l.Addr().String(), identity
}

// serveExternalSSHChannel 在独立通道中提供 SFTP 或运行 exec 命令，命令退出后回传退出码。
func serveExternalSSHChannel(ch ssh.Channel, requests <-chan *ssh.Request, directory string) {
	defer ch.Close()
	for req := range requests {
		switch req.Type {
		case "subsystem":
			req.Reply(true, nil)
			srv, err := sftp.NewServer(ch, sftp.WithServerWorkingDirectory(directory))
			if err == nil {
				defer srv.Close()
				_ = srv.Serve()
			}
			return
		case "exec":
			var payload struct{ Command string }
			if ssh.Unmarshal(req.Payload, &payload) != nil {
				req.Reply(false, nil)
				return
			}
			cmd := exec.Command("sh", "-c", payload.Command)
			cmd.Dir = directory
			cmd.Env = append(os.Environ(), "HOME="+directory)
			cmd.Stdin = ch
			cmd.Stdout = ch
			cmd.Stderr = ch.Stderr()
			if err := cmd.Start(); err != nil {
				req.Reply(false, nil)
				return
			}
			req.Reply(true, nil)
			code := uint32(0)
			if err := cmd.Wait(); err != nil {
				code = 1
			}
			_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{code}))
			return
		default:
			req.Reply(false, nil)
		}
	}
}

func TestExternalCollaboration(t *testing.T) {
	t.Run("external parent waits for native child", func(t *testing.T) {
		f := newExternalFixture(t, "-prompt", "wait-cancel")
		c := f.create(t, "default")
		observer := openRuntimeObserver(t, f.config, c.ID)
		observer.startTurn(t, "delegate")
		for {
			if _, ok := observer.next(t).(*runtimeclient.TurnStarted); ok {
				break
			}
		}
		parent := openExternalSessionWire(t, f.config, c.ID, c.RootSessionID)
		var spawned conversation.SpawnAgentResult
		parent.call(t, "session/spawn", map[string]any{"conversation_id": c.ID, "agent_name": "assistant", "task_name": "review", "message": "work"}, &spawned)
		target := runtimeprotocol.SessionParams{ConversationID: c.ID, SessionID: spawned.SessionID}
		parent.call(t, "session/subscribe", target, nil)
		first := f.model.next(t)
		wait := openExternalSessionWire(t, f.config, c.ID, c.RootSessionID)
		waitID := wait.request(t, "session/wait", map[string]any{"conversation_id": c.ID, "timeout_seconds": 5})
		first.reply <- e2eModelReply{reasoning: "work in progress", text: "review complete"}
		assertFinished := func(expected defs.ItemID, status defs.TurnStatus) {
			t.Helper()
			var finished runtimeprotocol.TurnFinishedParams
			event := parent.nextNotification(t, "turn/finished")
			if err := json.Unmarshal(event.Params, &finished); err != nil || finished.Status != status || !slices.Equal(finished.InputItemIDs, []defs.ItemID{expected}) {
				t.Fatalf("turn/finished %+v for %s: %v", finished, expected, err)
			}
		}
		assertFinished(spawned.InputItemID, defs.TurnStatusCompleted)
		reply := wait.response(t, waitID)
		var result conversation.WaitAgentResult
		if err := json.Unmarshal(reply.Result, &result); err != nil || reply.Error != nil || result.Reason != "subagent_result" || result.Text != "review complete" || !slices.Equal(result.From, []defs.SessionID{spawned.SessionID}) {
			t.Fatalf("wait %+v: %v %v", result, reply.Error, err)
		}
		var second, replacement runtimeprotocol.SendSessionResult
		parent.call(t, "session/send", map[string]any{"conversation_id": c.ID, "session_id": spawned.SessionID, "message": "next"}, &second)
		_ = f.model.next(t)
		parent.call(t, "session/send", map[string]any{"conversation_id": c.ID, "session_id": spawned.SessionID, "message": "replace", "interrupt": true}, &replacement)
		assertFinished(second.InputItemID, defs.TurnStatusInterrupted)
		f.model.next(t).reply <- e2eModelReply{text: "replacement complete"}
		assertFinished(replacement.InputItemID, defs.TurnStatusCompleted)
		child := openExternalSessionWire(t, f.config, c.ID, spawned.SessionID)
		var sent runtimeprotocol.SendSessionResult
		child.call(t, "session/send", map[string]any{"conversation_id": c.ID, "session_id": c.RootSessionID, "message": "explicit parent message", "mode": "steer"}, &sent)
		if sent.InputItemID == "" {
			t.Fatal("missing direct-parent input ID")
		}
		for _, method := range []string{"session/stop", "session/items/list"} {
			denied := child.response(t, child.request(t, method, runtimeprotocol.SessionParams{ConversationID: c.ID, SessionID: c.RootSessionID}))
			if denied.Error == nil || denied.Error.Message != "permission denied" {
				t.Fatalf("%s: %+v", method, denied)
			}
		}
		parent.call(t, "session/update", map[string]any{"conversation_id": c.ID, "session_id": spawned.SessionID, "task_name": "renamed review", "background": true, "labels": map[string]string{"area": "qa"}}, nil)
		observer.stop()
		child.close(t)
		parent.close(t)
		wait.close(t)
		f.stopServe(t)
		f.startServe(t)
		reopened := openRuntimeObserver(t, f.config, c.ID)
		defer reopened.stop()
		stored, err := f.control.GetSessionByID(t.Context(), spawned.SessionID)
		if err != nil || stored.ParentSessionID != c.RootSessionID || stored.ParentTurnID == "" || stored.ParentCallID != "" || stored.TaskName != "renamed review" || !stored.Background || stored.Labels["area"] != "qa" {
			t.Fatalf("cold-open child %+v %v", stored, err)
		}
	})
	t.Run("native tool receives external final body", func(t *testing.T) {
		f := newExternalFixture(t, "-prompt", "updates")
		observer := f.openNewConversation(t)
		observer.startTurn(t, "delegate")
		f.model.next(t).reply <- e2eModelReply{tool: "spawn_agent", arguments: `{"agent_name":"codex-acp","task_name":"review","message":"do the review"}`}
		for i := 0; i < 3; i++ {
			req := f.model.next(t)
			body, err := json.Marshal(req.body["messages"])
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(body), "openvaala-subagent-notification") && strings.Contains(string(body), "OK") {
				if strings.Contains(string(body), "thinking") || strings.Contains(string(body), "fixture failure") {
					t.Fatalf("internal content in parent notification: %s", body)
				}
				req.reply <- e2eModelReply{text: "acknowledged"}
				readTurnFinished(t, observer, defs.TurnStatusCompleted)
				return
			}
			req.reply <- e2eModelReply{tool: "wait_agent", arguments: `{"timeout_seconds":5}`}
		}
		t.Fatal("external final answer never reached the native parent")
	})
}

// TestExternalSessionStartup 覆盖启动前元数据与真实驱动首次响应的边界。
func TestExternalSessionStartup(t *testing.T) {
	t.Run("empty_backend_id", func(t *testing.T) {
		f := newExternalFixture(t, "-new-id=")
		_, err := f.control.CreateConversation(t.Context(), &runtimeclient.CreateConversationRequest{AgentName: "codex-acp"})
		if err == nil || !strings.Contains(err.Error(), "empty sessionId") {
			t.Fatalf("create accepted empty id: %v", err)
		}
		page, err := f.control.ListConversations(t.Context(), &runtimeclient.ListConversationsRequest{})
		if err != nil || len(page.Conversations) != 0 {
			t.Fatalf("failed create: %+v %v", page, err)
		}
	})
	t.Run("changed_child_does_not_block_root", func(t *testing.T) {
		f := newExternalFixture(t, "-prompt", "wait-cancel")
		root := f.create(t, "default")
		observer := openRuntimeObserver(t, f.config, root.ID)
		observer.startTurn(t, "delegate")
		for {
			if _, ok := observer.next(t).(*runtimeclient.TurnStarted); ok {
				break
			}
		}
		w := openExternalWire(t, f.config)
		var child conversation.SpawnAgentResult
		w.call(t, "session/spawn", map[string]any{"conversation_id": root.ID, "agent_name": "assistant", "task_name": "native", "message": "child", "notify_on_finish": false}, &child)
		f.model.next(t).reply <- e2eModelReply{text: "done"}
		observer.stop()
		w.close(t)
		f.stopServe(t)
		f.writeConfigFiles(t, map[string]string{"agents/assistant.yaml": fmt.Sprintf("name: assistant\ntype: acp\nacp:\n  command: %q\n", f.peer)})
		f.startServe(t)
		items, err := f.control.ListItems(t.Context(), &runtimeclient.ListItemsRequest{ConversationID: root.ID, SessionID: root.RootSessionID})
		if err != nil || len(items.Items) == 0 {
			t.Fatalf("root history: %+v %v", items, err)
		}
		w = openExternalWire(t, f.config)
		resp := w.response(t, w.request(t, "session/send", map[string]any{"conversation_id": root.ID, "session_id": child.SessionID, "message": "start"}))
		if resp.Error == nil || !strings.Contains(resp.Error.Message, "type changed from native to acp") {
			t.Fatalf("changed child: %+v", resp.Error)
		}
	})
	t.Run("location_before_ready", func(t *testing.T) {
		gate := filepath.Join(t.TempDir(), "ready")
		f := newExternalFixture(t, "-new-gate", gate)
		root := f.openNewConversation(t)
		root.startTurn(t, "delegate")
		req := f.model.next(t)
		scoped := openExternalSessionWire(t, f.config, root.conversation.ID, root.conversation.RootSessionID)
		defer func() { _ = os.WriteFile(gate, nil, 0600); req.reply <- e2eModelReply{text: "done"} }()
		spawnID := scoped.request(t, "session/spawn", map[string]any{"conversation_id": root.conversation.ID, "agent_name": "codex-acp", "task_name": "child", "message": "work", "notify_on_finish": false})
		reader := openExternalSessionWire(t, f.config, root.conversation.ID, root.conversation.RootSessionID)
		var page runtimeprotocol.ListSessionsResult
		federationEventually(t, "child listed before Ready", func() bool {
			reader.call(t, "session/list", map[string]any{"conversation_id": root.conversation.ID, "filter": map[string]any{}}, &page)
			return len(page.Sessions) == 1
		})
		b := page.Sessions[0].External
		if b == nil || b.ExecutorID != defs.LocalExecutorID || b.Cwd != page.AppliedFilter.Cwd || b.SessionID != "" {
			t.Fatalf("unconfirmed binding: %+v", b)
		}
		if err := os.WriteFile(gate, nil, 0600); err != nil {
			t.Fatal(err)
		}
		var child conversation.SpawnAgentResult
		response := scoped.response(t, spawnID)
		if err := json.Unmarshal(response.Result, &child); response.Error != nil || err != nil || child.SessionID != page.Sessions[0].ID || child.InputItemID == "" {
			t.Fatalf("spawn after Ready: %+v, %v, %v", child, response.Error, err)
		}
	})
}

// TestInboundACPExternalLoad 从编辑器的 stdio 检查创建配置、换模型和冷开回放的响应边界。
func TestInboundACPExternalLoad(t *testing.T) {
	for _, scenario := range []string{"new_and_select", "cold_load"} {
		t.Run(scenario, func(t *testing.T) {
			f := newExternalFixture(t, "-history-count", "12")
			cfg, err := os.ReadFile(f.config)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(f.config, bytes.Replace(cfg, []byte("agent_name: assistant"), []byte("agent_name: codex-acp"), 1), 0600); err != nil {
				t.Fatal(err)
			}
			if scenario == "cold_load" {
				c := f.create(t, "default")
				f.stopServe(t)
				f.startServe(t)
				editor := startACPWire(t, f.binary, f.config)
				id := editor.request(t, "session/load", map[string]any{"sessionId": c.ID, "cwd": t.TempDir(), "mcpServers": []any{}})
				var texts []string
				for {
					msg := editor.wire.next(t, "load replay or response", func(m codexWireMessage) bool { return m.Method == "session/update" || bytes.Equal(m.ID, id) })
					if bytes.Equal(msg.ID, id) {
						if msg.Error != nil {
							t.Fatal(msg.Error)
						}
						break
					}
					update := getACPUpdate(t, msg)
					if chunk := update.UserMessageChunk; chunk != nil && chunk.Content.Text != nil {
						texts = append(texts, chunk.Content.Text.Text)
					}
				}
				if len(texts) != 12 {
					t.Fatalf("received before load response: %q", texts)
				}
				for i, txt := range texts {
					if txt != fmt.Sprintf("message %d", i) {
						t.Fatalf("replay[%d] = %q", i, txt)
					}
				}
				return
			}
			editor := startACPWire(t, f.binary, f.config)
			var result struct {
				SessionID     string
				ConfigOptions []struct {
					ID, CurrentValue string
					Options          []struct{ Value string }
				}
			}
			editor.call(t, "session/new", map[string]any{"cwd": t.TempDir(), "mcpServers": []any{}}, &result)
			if result.SessionID == "" {
				t.Fatal("missing session id")
			}
			checkModel := func(want string) {
				t.Helper()
				for _, opt := range result.ConfigOptions {
					if opt.ID != "model" {
						continue
					}
					if opt.CurrentValue != want || len(opt.Options) == 0 {
						t.Fatalf("model = %+v, want %s", opt, want)
					}
					for _, v := range opt.Options {
						if !strings.HasPrefix(v.Value, "codex-acp/") {
							t.Fatal(v)
						}
					}
					return
				}
				t.Fatal("missing model")
			}
			checkModel("codex-acp/small")
			params := map[string]any{"sessionId": result.SessionID, "configId": "model", "value": "codex-acp/large"}
			editor.call(t, "session/set_config_option", params, &result)
			checkModel("codex-acp/large")
			params["value"] = "local/default"
			resp := editor.wire.response(t, editor.request(t, "session/set_config_option", params))
			if resp.Error == nil || resp.Error.Message != "cannot switch agent in an existing session" {
				t.Fatalf("cross-agent selection: %+v", resp.Error)
			}
		})
	}
}

func TestAppServerExternalModel(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "new")
	f := newExternalFixture(t, "-new-gate", gate)
	if err := os.Remove(filepath.Join(filepath.Dir(f.config), "models", "local.yaml")); err != nil {
		t.Fatal(err)
	}
	f.writeConfigFiles(t, map[string]string{"agents/assistant.yaml": "name: assistant\ninstructions: Test\n"})
	f.reloadConfig(t)
	wire := startAppServer(t, f.binary, f.config, "stdio")
	wire.call(t, "initialize", codexClientInfo, nil)
	wire.notify(t, "initialized", nil)
	var list struct{ Data []struct{ ID string } }
	wire.call(t, "model/list", map[string]any{}, &list)
	if len(list.Data) != 1 || list.Data[0].ID != "codex-acp/default" {
		t.Fatalf("cold catalog: %+v", list)
	}
	var opened codexWireThreadStart
	startID := wire.request(t, "thread/start", map[string]any{"model": "codex-acp/small", "cwd": t.TempDir()})
	// 探测和真实会话都已进入 session/new，此时目录仍只有 default。
	federationEventually(t, "probe and thread start", func() bool {
		audit, _ := os.ReadFile(f.audit)
		return bytes.Count(audit, []byte(`"method":"session/new"`)) >= 2
	})
	if err := os.WriteFile(gate, nil, 0600); err != nil {
		t.Fatal(err)
	}
	response := wire.response(t, startID)
	if response.Error != nil {
		t.Fatal(response.Error)
	}
	if err := json.Unmarshal(response.Result, &opened); err != nil {
		t.Fatal(err)
	}
	if opened.Model != "codex-acp/small" || opened.Thread.Model != opened.Model {
		t.Fatalf("external thread model = %q / %q", opened.Model, opened.Thread.Model)
	}
	id := opened.Thread.ID
	for _, tt := range []struct{ model, wantErr string }{{"codex-acp/large", ""}, {"local/default", "cannot switch agent in an existing session"}} {
		p := map[string]any{"threadId": id, "model": tt.model, "input": []any{codexTextInput("editor")}}
		resp := wire.response(t, wire.request(t, "turn/start", p))
		if tt.wantErr != "" {
			if resp.Error == nil || resp.Error.Message != tt.wantErr {
				t.Fatalf("%s: %+v", tt.model, resp.Error)
			}
			continue
		}
		if resp.Error != nil {
			t.Fatal(resp.Error)
		}
		wire.nextThreadNotification(t, id, "turn/completed")
		var read struct{ Thread codexWireThread }
		wire.call(t, "thread/read", map[string]any{"threadId": id}, &read)
		if read.Thread.Model != tt.model {
			t.Fatalf("thread/read model %q, want %q", read.Thread.Model, tt.model)
		}
	}
}

// TestExternalClientRestoration 验证 Go 客户端从恢复快照到就绪快照的翻页边界，以及尾序相同时的重新订阅。
func TestExternalClientRestoration(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "ready")
	f := newExternalFixture(t, "-load-gate", gate, "-history-count", "64")
	c := f.create(t, "default")
	f.stopServe(t)
	f.startServe(t)
	observer := openRuntimeObserver(t, f.config, c.ID)
	nextSnapshot := func() *runtimeclient.SessionSynced {
		t.Helper()
		for {
			switch e := observer.next(t).(type) {
			case *runtimeclient.SessionSynced:
				return e
			case *runtimeclient.ConversationSyncFailed:
				t.Fatal(e.Err)
			}
		}
	}
	restoring := nextSnapshot()
	if !restoring.External || !restoring.Restoring || restoring.LastSavedOrdinal != 0 || len(restoring.Items) != 0 {
		t.Fatalf("restoring snapshot = %+v", restoring)
	}
	if err := os.WriteFile(gate, nil, 0600); err != nil {
		t.Fatal(err)
	}
	var tail storage.Ordinal
	for _, round := range []string{"ready", "same_tail_after_reload"} {
		if round == "same_tail_after_reload" {
			wire := openExternalWire(t, f.config)
			wire.call(t, "session/reload", runtimeprotocol.SessionParams{ConversationID: c.ID, SessionID: c.RootSessionID}, nil)
		}
		ready := nextSnapshot()
		if ready.Restoring || !ready.External || ready.Options == nil || len(ready.Items) == 0 || len(ready.Items) >= 64 || ready.Items[len(ready.Items)-1].Ordinal != ready.LastSavedOrdinal || (tail != 0 && tail != ready.LastSavedOrdinal) {
			t.Fatalf("%s: tail=%d, snapshot=%+v", round, tail, ready)
		}
		for i := 1; i < len(ready.Items); i++ {
			if ready.Items[i].Ordinal <= ready.Items[i-1].Ordinal {
				t.Fatalf("%s: unordered recent page", round)
			}
		}
		tail = ready.LastSavedOrdinal
	}
}

// runExternalCLI 启动独立 CLI 进程并返回合并输出。
func runExternalCLI(ctx context.Context, f *externalFixture, env []string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, f.binary, append([]string{"--config-file", f.config}, args...)...)
	command.Dir = filepath.Dir(f.config)
	command.Env = append(getE2EEnvironment(command.Dir), env...)
	body, err := command.CombinedOutput()
	return string(body), err
}

// getExternalCLIEnvironment 为外部会话构造 CLI 所需的连接环境。
func getExternalCLIEnvironment(t *testing.T, f *externalFixture, record defs.Conversation) []string {
	t.Helper()
	file, err := getE2EServerConnection(getE2EServerConnectionPath(f.config))
	if err != nil {
		t.Fatal(err)
	}
	return []string{
		"OPENVAALA_URL=" + file.URL,
		"OPENVAALA_TOKEN=" + sessiontoken.Issue(file.Token, record.ID, record.RootSessionID),
		"OPENVAALA_CONVERSATION_ID=" + string(record.ID),
		"OPENVAALA_SESSION_ID=" + string(record.RootSessionID),
	}
}

func TestExternalSessionCLI(t *testing.T) {
	t.Parallel()
	f := newExternalFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), e2eTimeout)
	defer cancel()
	for _, tc := range []struct {
		args    []string
		want    string
		wantErr bool
	}{
		{[]string{"session", "ls"}, "session ls requires a source session or --conversation", true},
		{[]string{"session", "wait"}, "session wait requires a source session or session id", true},
		{[]string{"session", "spawn", "--agent", "codex-acp", "say hello"}, "OK", false},
		{[]string{"session", "spawn", "--model", "codex-acp/large", "root model"}, "OK", false},
		{[]string{"session", "spawn", "--agent", "missing", "say hello"}, "use --model / model", true},
		{[]string{"session", "spawn", "--agent", "codex-acp", "--no-wait", "say hello"}, "accepted", false},
	} {
		t.Run(strings.Join(tc.args[1:], " "), func(t *testing.T) {
			body, err := runExternalCLI(ctx, f, nil, tc.args...)
			if (err != nil) != tc.wantErr || !strings.Contains(body, tc.want) {
				t.Fatalf("output = %q, error = %v", body, err)
			}
		})
	}
	t.Run("operator options and reload", func(t *testing.T) {
		record := f.create(t, "default")
		id := string(record.RootSessionID)
		t.Run("open deep link", func(t *testing.T) {
			dir := t.TempDir()
			browser := "xdg-open"
			if runtime.GOOS == "darwin" {
				browser = "open"
			}
			linkFile := filepath.Join(dir, "link")
			script := fmt.Sprintf("#!/bin/sh\nprintf '%%s' \"$1\" > %q\n", linkFile)
			if err := os.WriteFile(filepath.Join(dir, browser), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			env := []string{"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH")}
			if body, err := runExternalCLI(t.Context(), f, env, "session", "open", id); err != nil {
				t.Fatalf("open: %q, %v", body, err)
			}
			deadline := time.Now().Add(e2eTimeout)
			for {
				if link, err := os.ReadFile(linkFile); err == nil && len(link) > 0 {
					suffix := fmt.Sprintf("/c/%s/sessions/%s", record.ID, id)
					if !bytes.Contains(link, []byte("/web/v1/s/")) || !bytes.HasSuffix(link, []byte(suffix)) {
						t.Fatalf("deep link = %q", link)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("browser did not receive a deep link")
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
		for _, tc := range []struct {
			args []string
			want string
		}{
			{[]string{"session", "option", id}, "config:backend-model=small"},
			{[]string{"session", "option", id, "--set", "config:backend-model=large"}, "config:backend-model=large"},
			{[]string{"session", "reload", id}, ""},
		} {
			body, err := runExternalCLI(t.Context(), f, nil, tc.args...)
			if err != nil || !strings.Contains(body, tc.want) {
				t.Fatalf("%v: %q, %v", tc.args, body, err)
			}
		}
	})
	t.Run("native bash spawns a separate root", func(t *testing.T) {
		parent := f.openNewConversation(t)
		defer parent.stop()
		parent.startTurn(t, "run CLI")
		command := fmt.Sprintf("%q --config-file %q session spawn --agent assistant --no-wait 'from bash'", f.binary, f.config)
		arguments, err := json.Marshal(map[string]string{"command": command})
		if err != nil {
			t.Fatal(err)
		}
		f.model.next(t).reply <- e2eModelReply{tool: "bash", arguments: string(arguments)}
		for range 2 {
			request := f.model.next(t)
			request.reply <- e2eModelReply{text: "done"}
		}
		readTurnFinished(t, parent, defs.TurnStatusCompleted)
		page, err := f.control.ListConversations(t.Context(), &runtimeclient.ListConversationsRequest{})
		if err != nil {
			t.Fatal(err)
		}
		var spawned defs.Session
		for _, entry := range page.Conversations {
			if entry.Conversation.ID != parent.conversation.ID && entry.RootSession.AgentName == "assistant" {
				spawned = entry.RootSession
			}
		}
		if spawned.ID == "" || spawned.ParentSessionID != "" {
			t.Fatalf("spawned root = %+v", spawned)
		}
	})
	t.Run("operator can send to and stop spawned root", func(t *testing.T) {
		fixture := newExternalFixture(t, "-prompt", "wait-cancel")
		for _, tc := range []struct {
			action string
			extra  []string
		}{
			{action: "send", extra: []string{"--no-wait", "follow-up"}},
			{action: "stop"},
		} {
			t.Run(tc.action, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), e2eTimeout)
				defer cancel()
				prompt := "root for " + tc.action
				accepted, err := runExternalCLI(ctx, fixture, nil, "session", "spawn", "--agent", "codex-acp", "--no-wait", prompt)
				if err != nil {
					t.Fatalf("spawn: %q, %v", accepted, err)
				}
				var id string
				for _, field := range strings.Fields(accepted) {
					if value, ok := strings.CutPrefix(field, "session_id="); ok {
						id = value
					}
				}
				waitExternalPrompt(t, fixture.audit, prompt)
				output, err := runExternalCLI(ctx, fixture, nil, append([]string{"session", tc.action, id}, tc.extra...)...)
				if err != nil || strings.TrimSpace(output) == "" || strings.Contains(output, "no_active_turn") {
					t.Fatalf("%s root: %q, %v", tc.action, output, err)
				}
				if tc.action == "send" && !strings.Contains(output, "input_item_id=") {
					t.Fatalf("send receipt: %q", output)
				}
			})
		}
	})
	t.Run("session token spawns and sends", func(t *testing.T) {
		fixture := newExternalFixture(t, "-prompt", "wait-cancel")
		parent := fixture.create(t, "default")
		link := openRuntimeObserver(t, fixture.config, parent.ID)
		defer link.stop()
		link.startTurn(t, "delegate")
		for {
			if _, ok := link.next(t).(*runtimeclient.TurnStarted); ok {
				break
			}
		}
		env := getExternalCLIEnvironment(t, fixture, parent)
		run := func(args ...string) string {
			t.Helper()
			runCtx, cancel := context.WithTimeout(t.Context(), e2eTimeout)
			defer cancel()
			text, err := runExternalCLI(runCtx, fixture, env, args...)
			if err != nil {
				t.Fatalf("%v: %q, %v", args, text, err)
			}
			return text
		}
		type submission struct {
			output string
			err    error
		}
		spawned := make(chan submission, 1)
		spawnCtx, stopSpawn := context.WithTimeout(t.Context(), e2eTimeout)
		defer stopSpawn()
		go func() {
			output, err := runExternalCLI(spawnCtx, fixture, env, "session", "spawn", "--task", "review", "--agent", "assistant", "--fork-turns", "none", "first input")
			spawned <- submission{output, err}
		}()
		fixture.model.next(t).reply <- e2eModelReply{text: "first final"}
		if result := <-spawned; result.err != nil || strings.TrimSpace(result.output) != "first final" {
			t.Fatalf("spawn = %+v", result)
		}
		list := run("session", "ls")
		var child string
		for _, line := range strings.Split(list, "\n") {
			if strings.Contains(line, `task="review"`) {
				child = strings.Fields(line)[0]
			}
		}
		if child == "" || !strings.Contains(list, "since_hours=24") {
			t.Fatalf("list = %q", list)
		}
		promptFile := filepath.Join(t.TempDir(), "message.txt")
		if err := os.WriteFile(promptFile, []byte("second input from file"), 0600); err != nil {
			t.Fatal(err)
		}
		sent := make(chan submission, 1)
		go func() {
			output, err := runExternalCLI(spawnCtx, fixture, env, "session", "send", child, "--mode", "steer", "--prompt-file", promptFile)
			sent <- submission{output, err}
		}()
		request := fixture.model.next(t)
		body, _ := json.Marshal(request.body["messages"])
		if !bytes.Contains(body, []byte("second input from file")) {
			t.Fatalf("prompt file content missing: %s", body)
		}
		request.reply <- e2eModelReply{text: "second final"}
		if result := <-sent; result.err != nil || strings.TrimSpace(result.output) != "second final" {
			t.Fatalf("send = %+v", result)
		}
		t.Run("attach respects the source scope", func(t *testing.T) {
			attachCtx, cancel := context.WithTimeout(t.Context(), e2eTimeout)
			defer cancel()
			body, err := runExternalCLI(attachCtx, fixture, env, "session", "attach", string(parent.RootSessionID))
			if err == nil || !strings.Contains(body, "permission denied") {
				t.Fatalf("attach parent: %q, %v", body, err)
			}
			command := exec.CommandContext(attachCtx, fixture.binary, "--config-file", fixture.config, "session", "attach", child)
			command.Env = append(getE2EEnvironment(filepath.Dir(fixture.config)), env...)
			pipe, err := command.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { cancel(); _ = command.Wait() }()
			lines := bufio.NewScanner(pipe)
			for lines.Scan() {
				if strings.Contains(lines.Text(), "second final") {
					return
				}
			}
			t.Fatalf("attach child did not print its log: %v", lines.Err())
		})
		list, err := runExternalCLI(t.Context(), fixture, nil, "session", "ls", "--conversation", string(parent.ID))
		if err != nil || !strings.Contains(list, child) {
			t.Fatalf("operator list = %q, %v", list, err)
		}
		for _, tc := range []struct {
			args []string
			want string
		}{
			{[]string{"session", "update", child, "--task-name", "verified", "--label", "phase=done", "--background=true"}, "verified"},
			{[]string{"session", "ls", "--all", "--label", "phase=done"}, child},
			{[]string{"session", "logs", child, "--limit", "20"}, "second final"},
			{[]string{"session", "inspect", child}, "conversation_id=" + string(parent.ID)},
			{[]string{"session", "stop", child}, "no_active_turn"},
		} {
			if output := run(tc.args...); !strings.Contains(output, tc.want) {
				t.Fatalf("%v = %q, want %q", tc.args, output, tc.want)
			}
		}
		for _, tc := range []struct {
			flag    []string
			want    string
			exclude string
		}{
			{[]string{"--session-id", child}, child, ""},
			{[]string{"--session-id", "missing"}, "all=true", child},
			{[]string{"--status", "running"}, "all=true", child},
			{[]string{"--cwd", "*"}, "cwd=*", ""},
			{[]string{"--cwd", "/nonmatching-directory"}, "cwd=/nonmatching-directory", child},
			{[]string{"--since-hours", "0"}, "since_hours=0", ""},
			{[]string{"--limit", "1"}, "limit=1", ""},
		} {
			t.Run(strings.Join(tc.flag, " "), func(t *testing.T) {
				output := run(append([]string{"session", "ls", "--all"}, tc.flag...)...)
				if !strings.Contains(output, tc.want) || tc.exclude != "" && strings.Contains(output, tc.exclude) {
					t.Fatalf("filtered list = %q", output)
				}
			})
		}
		t.Run("logs cursor", func(t *testing.T) {
			last := run("session", "logs", child, "--limit", "1")
			_, cursor, found := strings.Cut(last, "next_cursor=")
			if !found {
				t.Fatalf("missing cursor: %s", last)
			}
			earlier := run("session", "logs", child, "--cursor", strings.TrimSpace(cursor))
			if !strings.Contains(earlier, "first final") || strings.Contains(earlier, "second final") {
				t.Fatalf("earlier page = %q", earlier)
			}
		})
		t.Run("invalid collaboration options", func(t *testing.T) {
			for _, tc := range []struct {
				args []string
				want string
			}{
				{[]string{"session", "wait", "--wait-for", "invalid"}, "invalid wait_for"},
				{[]string{"session", "spawn", "--agent", "assistant", "--fork-turns", "invalid", "test"}, "fork_turns"},
				{[]string{"session", "spawn", "--agent", "assistant", "--reasoning-effort", "invalid", "test"}, "reasoning effort"},
				{[]string{"session", "send", child, "--model", "invalid", "test"}, "must be provider_id/model_id"},
				{[]string{"session", "send", child, "--reasoning-effort", "invalid", "test"}, "reasoning effort"},
				{[]string{"session", "spawn", "--agent", "missing", "test"}, "Configured agents: assistant, codex-acp, default"},
			} {
				t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
					output, err := runExternalCLI(t.Context(), fixture, env, tc.args...)
					if err == nil || !strings.Contains(output, tc.want) {
						t.Fatalf("output=%q, err=%v", output, err)
					}
				})
			}
		})
		t.Run("sending to parent returns acceptance", func(t *testing.T) {
			childEnv := getExternalCLIEnvironment(t, fixture, defs.Conversation{ID: parent.ID, RootSessionID: defs.SessionID(child)})
			for _, tc := range []struct {
				name  string
				flags []string
			}{
				{name: "default wait"},
				{name: "no wait", flags: []string{"--no-wait"}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					args := append([]string{"session", "send", string(parent.RootSessionID), "message for parent"}, tc.flags...)
					output, err := runExternalCLI(t.Context(), fixture, childEnv, args...)
					if err != nil || !strings.Contains(output, "accepted ") || !strings.Contains(output, "input_item_id=") {
						t.Fatalf("send to parent: %q, %v", output, err)
					}
				})
			}
		})
		accepted := run("session", "spawn", "--agent", "codex-acp", "--model", "codex-acp/large",
			"--initial-option", "mode:mode=code", "--notify-on-finish=false", "--no-wait", "detach me")
		var externalChild string
		for _, field := range strings.Fields(accepted) {
			if id, ok := strings.CutPrefix(field, "session_id="); ok {
				externalChild = id
			}
		}
		if externalChild == "" {
			t.Fatal(accepted)
		}
		waitExternalPrompt(t, fixture.audit, "detach me")
		childRecord, err := fixture.control.GetSessionByID(t.Context(), defs.SessionID(externalChild))
		if err != nil || !childRecord.SuppressFinishReport {
			t.Fatalf("child notification setting: %+v, %v", childRecord, err)
		}
		options := run("session", "option", externalChild)
		for _, want := range []string{"config:backend-model=large", "mode:mode=code"} {
			if !strings.Contains(options, want) {
				t.Fatalf("options %q missing %q", options, want)
			}
		}
		detached, err := runExternalCLI(t.Context(), fixture, nil, "session", "detach", externalChild)
		if err != nil || !strings.Contains(detached, "conversation_id=") {
			t.Fatalf("detach: %q, %v", detached, err)
		}

	})
}

func TestExternalOperatorImport(t *testing.T) {
	f := newExternalFixture(t, "-sessions", "30")
	ctx, cancel := context.WithTimeout(t.Context(), e2eTimeout)
	defer cancel()
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--query", "Needle"}, "Needle"},
		{[]string{"--cursor", "5"}, "id-5"},
		{[]string{"id-2"}, "conversation_id"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			args := append([]string{"session", "import", "--agent", "codex-acp"}, tc.args...)
			body, err := runExternalCLI(ctx, f, nil, args...)
			if err != nil || !strings.Contains(body, tc.want) {
				t.Fatalf("output = %q, error = %v", body, err)
			}
		})
	}
}

func TestExternalToolCLI(t *testing.T) {
	f := newExternalFixture(t)
	agentFile := filepath.Join(filepath.Dir(f.config), "agents", "codex-acp.yaml")
	agent, err := os.ReadFile(agentFile)
	if err != nil {
		t.Fatal(err)
	}
	f.writeConfigFiles(t, map[string]string{
		"agents/codex-acp.yaml":                    string(agent) + "skills: [review-kit]\n",
		"skills/review-kit/reference/checklist.md": "Review checklist file.\n",
		"skills/review-kit/SKILL.md":               "---\nname: review-kit\ndescription: Verify review results\n---\nReview checklist.\n",
	})
	f.reloadConfig(t)
	ctx, cancel := context.WithTimeout(t.Context(), e2eTimeout)
	defer cancel()
	for _, tc := range []struct {
		args    []string
		want    string
		exclude string
	}{
		{args: []string{"executor", "ls"}, want: "local"},
		{args: []string{"agent", "ls"}, want: "codex-acp"},
		{args: []string{"agent", "options", "codex-acp"}, want: "config:backend-model=small"},
		{args: []string{"skill", "get", "openvaala", "reference/cli.md"}, want: "## Connecting to a runtime server"},
		{args: []string{"skill", "get", "openvaala", "zh/reference/cli.md"}, want: "## 连接 Runtime 服务"},
		{args: []string{"exec", "--executor", "local", "--", "printf", "task9 result"}, want: "task9 result"},
		{args: []string{"skill", "list"}, want: "openvaala", exclude: "review-kit"},
		{args: []string{"skill", "search", "Review", "--agent", "codex-acp"}, want: "review-kit"},
		{args: []string{"skill", "get", "openvaala"}, want: "从外部 Agent 协作"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			body, err := runExternalCLI(ctx, f, nil, tc.args...)
			if err != nil || !strings.Contains(body, tc.want) || tc.exclude != "" && strings.Contains(body, tc.exclude) {
				t.Fatalf("output = %q, error = %v", body, err)
			}
		})
	}
	t.Run("session terminals and scoped skills", func(t *testing.T) {
		record := f.create(t, "default")
		env := getExternalCLIEnvironment(t, f, record)
		run := func(args ...string) string {
			t.Helper()
			commandCtx, cancel := context.WithTimeout(t.Context(), e2eTimeout)
			defer cancel()
			text, err := runExternalCLI(commandCtx, f, env, args...)
			if err != nil {
				t.Fatalf("%v: %q, %v", args, text, err)
			}
			return text
		}
		id := strings.TrimSpace(run("terminal", "open", "--executor", "local", "--rows", "31", "--cols", "91"))
		if id == "" || !strings.Contains(run("terminal", "ls"), id) {
			t.Fatalf("terminal %q missing", id)
		}
		run("terminal", "send", id, "printf '\\033[31m%s\\033[0m\\n' first second third; stty size; cat\n")
		deadline := time.Now().Add(3 * time.Second)
		for {
			text := run("terminal", "capture", id, "--lines", "2")
			if text == "third\n31 91" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("capture=%q, full=%q", text, run("terminal", "capture", id))
			}
			time.Sleep(25 * time.Millisecond)
		}
		run("terminal", "close", id)
		if strings.Contains(run("terminal", "ls"), id) {
			t.Fatalf("closed terminal %s still listed", id)
		}
		if list := run("skill", "list"); !strings.Contains(list, "openvaala") || !strings.Contains(list, "review-kit") {
			t.Fatal(list)
		}
		if list := run("skill", "search", "Review"); !strings.Contains(list, "review-kit") {
			t.Fatal(list)
		}
		if text := run("skill", "get", "review-kit"); !strings.Contains(text, "Review checklist.") {
			t.Fatal(text)
		}
		if text := run("skill", "get", "openvaala"); !strings.Contains(text, "从外部 Agent 协作") {
			t.Fatal(text)
		}
		for _, tc := range []struct {
			args []string
			want string
		}{
			{[]string{"executor", "ls", "--refresh-tools"}, "local (default)"},
			{[]string{"executor", "tool", "list_commands"}, `"commands"`},
			{[]string{"executor", "tool", "bash", `{"command":"printf parity"}`}, `"output":"parity"`},
			{[]string{"executor", "tool", "read_file", `{"executor":"openvaala","path":"/skills/openvaala/reference/cli.md","offset":1,"limit":3}`}, "# CLI"},
			{[]string{"skill", "get", "review-kit", "reference/checklist.md"}, "Review checklist file."},
		} {
			t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
				if got := run(tc.args...); !strings.Contains(got, tc.want) {
					t.Fatalf("output=%q, want %q", got, tc.want)
				}
			})
		}
		for _, tc := range []struct {
			name, file, want string
		}{
			{"openvaala", "reference/cli.md", "Connecting to a runtime server"},
			{"review-kit", "reference/checklist.md", "Review checklist file."},
			{"review-kit", "../other/SKILL.md", "invalid skill file path"},
		} {
			t.Run("MCP "+tc.file, func(t *testing.T) {
				body := callExternalMCP(t, f, record.ID, record.RootSessionID, "get_skill", map[string]string{"name": tc.name, "file": tc.file})
				if !strings.Contains(body, tc.want) {
					t.Fatal(body)
				}
			})
		}
		if text := run("exec", "--executor", "local", "--", "printf", "scoped exec"); text != "scoped exec" {
			t.Fatal(text)
		}
	})
}
