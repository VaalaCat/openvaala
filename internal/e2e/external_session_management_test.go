package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/VaalaCat/openvaala/pkg/sessiontoken"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
)

func TestExternalOptionsRPC(t *testing.T) {
	f := newExternalFixture(t)
	w := openExternalWire(t, f.config)
	for _, tt := range []struct {
		refresh bool
		starts  int
	}{{false, 1}, {false, 1}, {true, 2}} {
		var options defs.ExternalOptions
		w.call(t, "agent/getOptions", map[string]any{"agent_name": "codex-acp", "refresh": tt.refresh}, &options)
		audit, err := os.ReadFile(f.audit)
		if err != nil || options.Model == nil || options.Mode == nil || bytes.Count(audit, []byte(`"method":"session/new"`)) != tt.starts {
			t.Fatalf("options=%+v audit=%s err=%v", options, audit, err)
		}
	}
	var selected defs.ExternalOptions
	w.call(t, "agent/getOptions", map[string]any{"agent_name": "codex-acp", "selections": []defs.ExternalOptionSelection{{Kind: "model", Value: "large"}}}, &selected)
	if selected.Thought == nil || selected.Thought.CurrentValue != "high" {
		t.Fatalf("linked selections: %+v", selected)
	}
	c := f.create(t, "default")
	p := map[string]any{"conversation_id": c.ID, "session_id": c.RootSessionID, "kind": "model", "value": "large"}
	subscriber := openExternalWire(t, f.config)
	subscriber.call(t, "session/subscribe", p, nil)
	w.call(t, "session/setOption", p, nil)
	var updated runtimeprotocol.SessionExternalStateUpdatedParams
	if err := json.Unmarshal(subscriber.nextNotification(t, "session/externalStateUpdated").Params, &updated); err != nil || updated.Options.Model.CurrentValue != "large" || updated.Options.Thought.CurrentValue != "high" {
		t.Fatalf("option notification: %+v %v", updated, err)
	}
	var snapshot runtimeprotocol.SubscribeSessionResult
	w.call(t, "session/subscribe", p, &snapshot)
	if snapshot.Options == nil || snapshot.Options.Model.CurrentValue != "large" {
		t.Fatalf("model after set: %+v", snapshot.Options)
	}
	scoped := openExternalSessionWire(t, f.config, c.ID, c.RootSessionID)
	if resp := scoped.response(t, scoped.request(t, "session/setOption", p)); resp.Error == nil || !strings.Contains(resp.Error.Message, "permission denied") {
		t.Fatalf("self permission: %+v", resp.Error)
	}
	t.Run("probe failure permits creation", func(t *testing.T) {
		f := newExternalFixture(t, "-fail-probe")
		wire := openExternalWire(t, f.config)
		var opts defs.ExternalOptions
		wire.call(t, "agent/getOptions", map[string]string{"agent_name": "codex-acp"}, &opts)
		if opts.Model != nil || opts.Mode != nil {
			t.Fatalf("failed probe options: %+v", opts)
		}
		audit, err := os.ReadFile(f.audit)
		if err != nil || !bytes.Contains(audit, []byte(`"method":"session/new"`)) {
			t.Fatalf("missing probe request: %s, %v", audit, err)
		}
		f.create(t, "default")
	})

}

func TestExternalModelList(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "probe")
	f := newExternalFixture(t, "-new-gate", gate)
	w := openExternalWire(t, f.config)
	list := func(params map[string]any) []string {
		var r runtime.ModelList
		w.call(t, "model/list", params, &r)
		var out []string
		for _, m := range r.Models {
			out = append(out, m.Ref.ProviderID+"/"+m.Ref.ModelID)
		}
		return out
	}
	params := map[string]any{"agent_name": "assistant"}
	first := strings.Join(list(params), " ")
	if !strings.Contains(first, "codex-acp/default") || !strings.Contains(first, "local/default") {
		t.Fatal(first)
	}
	for range 5 {
		if got := list(params); !slices.Contains(got, "codex-acp/default") {
			t.Fatalf("blocked probe: %v", got)
		}
	}
	if err := os.WriteFile(gate, nil, 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(e2eTimeout)
	for !strings.Contains(strings.Join(list(params), " "), "codex-acp/large") {
		if time.Now().After(deadline) {
			t.Fatal("background probe did not fill cache")
		}
		time.Sleep(20 * time.Millisecond)
	}
	audit, _ := os.ReadFile(f.audit)
	if bytes.Count(audit, []byte(`"method":"session/new"`)) != 1 {
		t.Fatalf("probe count: %s", audit)
	}
	for _, c := range []defs.Conversation{f.create(t, "large"), f.openNewConversation(t).conversation} {
		models := list(map[string]any{"conversation_id": c.ID})
		if len(models) == 0 {
			t.Fatal("empty model list")
		}
		provider := strings.Split(models[0], "/")[0]
		for _, m := range models {
			if !strings.HasPrefix(m, provider+"/") {
				t.Fatal(models)
			}
		}
	}
	if err := os.Remove(filepath.Join(filepath.Dir(f.config), "models", "local.yaml")); err != nil {
		t.Fatal(err)
	}
	f.writeConfigFiles(t, map[string]string{"agents/assistant.yaml": "name: assistant\ninstructions: Test\n"})
	f.reloadConfig(t)
	if got := list(params); !slices.Equal(got, []string{"codex-acp/default"}) {
		t.Fatalf("zero native models: %v", got)
	}

}

func TestExternalImportPreview(t *testing.T) {
	f := newExternalFixture(t, "-sessions", "30")
	w := openExternalWire(t, f.config)
	p := map[string]any{"agent_name": "codex-acp", "query": "chat"}
	var result struct {
		Sessions []struct {
			SessionID string `json:"session_id"`
			Title     string
		}
		Next string `json:"next_cursor"`
	}
	w.call(t, "agent/listSessions", p, &result)
	if len(result.Sessions) != 20 || result.Next == "" {
		t.Fatalf("first page: %+v", result)
	}
	p["cursor"] = result.Next
	w.call(t, "agent/listSessions", p, &result)
	if len(result.Sessions) != 8 {
		t.Fatalf("last page: %+v", result)
	}
	audit, _ := os.ReadFile(f.audit)
	if bytes.Contains(audit, []byte("session/load")) {
		t.Fatal("listing loaded a session")
	}
	p = map[string]any{"agent_name": "codex-acp", "external_session_id": result.Sessions[0].SessionID}
	var preview struct {
		First string `json:"first_user_message"`
		Last  string `json:"last_user_message"`
	}
	w.call(t, "agent/previewSession", p, &preview)
	if preview.First != "hello world" || preview.Last != "latest" {
		t.Fatalf("preview: %+v", preview)
	}
	for _, title := range []string{result.Sessions[0].Title, ""} {
		p["title"] = title
		var out runtimeprotocol.ConversationResult
		w.call(t, "session/import", p, &out)
		if out.Conversation.Title != title {
			t.Fatalf("import title: %q", out.Conversation.Title)
		}
		items, err := f.control.ListItems(t.Context(), &runtimeclient.ListItemsRequest{ConversationID: out.Conversation.ID, SessionID: out.Conversation.RootSessionID})
		if err != nil || len(items.Items) == 0 {
			t.Fatalf("import items: %+v %v", items, err)
		}
	}
	f.configure(t, "-list=false", "-load=false")
	before, err := f.control.ListConversations(t.Context(), &runtimeclient.ListConversationsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ method, want string }{
		{"agent/listSessions", "external agent does not support session listing"},
		{"session/import", "external session cannot be restored"},
	} {
		got := w.response(t, w.request(t, tc.method, p))
		if got.Error == nil || !strings.Contains(got.Error.Message, tc.want) {
			t.Fatalf("%s: %+v", tc.method, got.Error)
		}
	}
	after, err := f.control.ListConversations(t.Context(), &runtimeclient.ListConversationsRequest{})
	if err != nil || len(after.Conversations) != len(before.Conversations) {
		t.Fatalf("failed import: %+v %v", after, err)
	}

}

// TestExternalForkModel 检查显式模型经过 fork 请求应用到新根的 ACP 会话。
func TestExternalForkModel(t *testing.T) {
	f := newExternalFixture(t, "-history-count", "1")
	source := f.create(t, "default")
	f.stopServe(t)
	f.startServe(t)
	wire := openExternalWire(t, f.config)
	for _, tc := range []struct {
		name, provider, model string
		wantModel, wantError  string
		wantModelSet          bool
	}{
		{name: "explicit model", provider: "codex-acp", model: "large", wantModel: "large", wantModelSet: true},
		{name: "backend default", provider: "codex-acp", model: "default", wantModel: "small"},
		{name: "other provider", provider: "local", model: "default", wantError: "cannot switch agent in an existing session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, err := os.ReadFile(f.audit)
			if err != nil {
				t.Fatal(err)
			}
			params := runtimeprotocol.ForkConversationParams{
				ConversationID: source.ID,
				Model:          defs.ModelSelection{Ref: defs.ModelRef{ProviderID: tc.provider, ModelID: tc.model}},
			}
			response := wire.response(t, wire.request(t, "conversation/fork", params))
			if tc.wantError != "" {
				if response.Error == nil || response.Error.Message != tc.wantError {
					t.Fatalf("fork error = %+v, want %q", response.Error, tc.wantError)
				}
				return
			}
			var fork runtimeprotocol.ConversationResult
			if err := json.Unmarshal(response.Result, &fork); response.Error != nil || err != nil {
				t.Fatalf("fork response = %+v, decode %v", response.Error, err)
			}
			var snapshot runtimeprotocol.SubscribeSessionResult
			wire.call(t, "session/subscribe", runtimeprotocol.SessionParams{
				ConversationID: fork.Conversation.ID, SessionID: fork.Conversation.RootSessionID,
			}, &snapshot)
			if snapshot.Options == nil || snapshot.Options.Model == nil || snapshot.Options.Model.CurrentValue != tc.wantModel {
				t.Errorf("fork model options = %+v, want model %q", snapshot.Options, tc.wantModel)
			}
			after, err := os.ReadFile(f.audit)
			if err != nil {
				t.Fatal(err)
			}
			if modelSet := bytes.Contains(after[len(before):], []byte(`"method":"session/set_config_option"`)); modelSet != tc.wantModelSet {
				t.Fatalf("fork option update sent = %t, want %t", modelSet, tc.wantModelSet)
			}
		})
	}
}

func TestExternalForkBoundary(t *testing.T) {
	history := filepath.Join(t.TempDir(), "input.json")
	f := newExternalFixture(t, "-history-count", "3", "-history-file", history)
	source := f.create(t, "default")
	f.stopServe(t)
	f.startServe(t)
	turns, err := f.control.ListTurns(t.Context(), &runtimeclient.ListTurnsRequest{ConversationID: source.ID, SessionID: source.RootSessionID})
	if err != nil || len(turns.Turns) != 3 {
		t.Fatalf("source: %+v %v", turns, err)
	}
	w := openExternalWire(t, f.config)
	var fork runtimeprotocol.ConversationResult
	w.call(t, "conversation/fork", map[string]any{"conversation_id": source.ID, "source_session_id": source.RootSessionID, "before_turn_id": turns.Turns[2].ID}, &fork)
	observer := openRuntimeObserver(t, f.config, fork.Conversation.ID)
	for _, msg := range []string{"continue", "again"} {
		observer.startTurn(t, msg)
		readTurnFinished(t, observer, defs.TurnStatusCompleted)
		data, err := os.ReadFile(history)
		var blocks []struct{ Text string }
		if err != nil || json.Unmarshal(data, &blocks) != nil || len(blocks) == 0 {
			t.Fatalf("prompt %s %v", data, err)
		}
		if msg == "continue" && (!strings.HasPrefix(blocks[0].Text, "<openvaala-chat-history-summary>") || !strings.Contains(blocks[0].Text, "[User] message 1") || strings.Contains(blocks[0].Text, "message 2")) {
			t.Fatal(blocks)
		}
		if msg == "again" && strings.Contains(blocks[0].Text, "history-summary") {
			t.Fatal(blocks)
		}
	}
	t.Run("spawn from native and read restored child", func(t *testing.T) {
		f := newExternalFixture(t)
		observer := f.openNewConversation(t)
		f.runTurn(t, observer, "finished source", e2eModelReply{text: "inherited answer"})
		observer.startTurn(t, "unfinished source")
		pending := f.model.next(t)
		cid := observer.conversation.ID
		wire := openExternalWire(t, f.config)
		var child conversation.SpawnAgentResult
		wire.call(t, "session/spawn", map[string]any{"conversation_id": cid, "agent_name": "codex-acp", "task_name": "external", "message": "external assignment", "fork_turns": "all", "notify_on_finish": false}, &child)
		texts := waitExternalPrompt(t, f.audit, "external assignment")
		pending.reply <- e2eModelReply{text: "done"}
		readTurnFinished(t, observer, defs.TurnStatusCompleted)
		if !strings.HasPrefix(texts[0], "<openvaala-chat-history-summary>") || !strings.Contains(texts[0], "[Assistant] inherited answer") || strings.Contains(texts[0], "unfinished source") {
			t.Fatalf("spawn prompt: %q", texts)
		}
		observer.stop()
		wire.close(t)
		f.stopServe(t)
		f.startServe(t)
		logs := callExternalMCP(t, f, cid, observer.conversation.RootSessionID, "read_session_history", map[string]string{"session_id": string(child.SessionID)})
		if !strings.Contains(logs, "hello world") || strings.Contains(logs, "session_fork") {
			t.Fatal(logs)
		}
	})
	t.Run("spawn from restored external and fork native child", func(t *testing.T) {
		f := newExternalFixture(t, "-prompt", "wait-cancel")
		wire := openExternalWire(t, f.config)
		var imported runtimeprotocol.ConversationResult
		wire.call(t, "session/import", map[string]string{"agent_name": "codex-acp", "external_session_id": "prior"}, &imported)
		cid := imported.Conversation.ID
		observer := openRuntimeObserver(t, f.config, cid)
		observer.startTurn(t, "unfinished source")
		waitExternalPrompt(t, f.audit, "unfinished source")
		var child conversation.SpawnAgentResult
		wire.call(t, "session/spawn", map[string]any{"conversation_id": cid, "agent_name": "assistant", "task_name": "native", "message": "native assignment", "fork_turns": "all", "notify_on_finish": false}, &child)
		req := f.model.next(t)
		encoded, _ := json.Marshal(req.body["messages"])
		var messages []struct {
			Role    string
			Content json.RawMessage
		}
		if err := json.Unmarshal(encoded, &messages); err != nil {
			t.Fatal(err)
		}
		var content []string
		for _, m := range messages {
			if m.Role != "user" {
				continue
			}
			var text string
			if json.Unmarshal(m.Content, &text) == nil {
				content = append(content, text)
				continue
			}
			var blocks []struct{ Text string }
			if err := json.Unmarshal(m.Content, &blocks); err != nil {
				t.Fatal(err)
			}
			for _, b := range blocks {
				content = append(content, b.Text)
			}
		}
		req.reply <- e2eModelReply{text: "native final"}
		if !slices.ContainsFunc(content, func(text string) bool {
			return strings.HasPrefix(text, "<openvaala-chat-history-summary>") && strings.Contains(text, "[User] hello world") && strings.Contains(text, "native assignment") && !strings.Contains(text, "unfinished source")
		}) {
			t.Fatalf("native input: %q", content)
		}
		deadline := time.Now().Add(e2eTimeout)
		for {
			turns, err := f.control.ListTurns(t.Context(), &runtimeclient.ListTurnsRequest{ConversationID: cid, SessionID: child.SessionID})
			if err != nil {
				t.Fatal(err)
			}
			if len(turns.Turns) > 0 && turns.Turns[0].Status == defs.TurnStatusCompleted {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("native child did not finish")
			}
			time.Sleep(20 * time.Millisecond)
		}
		var fork runtimeprotocol.ConversationResult
		wire.call(t, "conversation/fork", map[string]any{"conversation_id": cid, "source_session_id": child.SessionID}, &fork)
		root, err := f.control.GetSessionByID(t.Context(), fork.Conversation.RootSessionID)
		if err != nil || root.AgentName != "assistant" || root.External != nil {
			t.Fatalf("fork root: %+v %v", root, err)
		}
		if logs, _ := json.Marshal(f.listItems(t, fork.Conversation.ID, root.ID)); !bytes.Contains(logs, []byte("native final")) {
			t.Fatalf("native child fork: %s", logs)
		}
	})

}
func TestExternalSingleReload(t *testing.T) {
	for _, active := range []bool{false, true} {
		f := newExternalFixture(t, "-prompt", "wait-cancel")
		c := f.create(t, "default")
		w := openExternalWire(t, f.config)
		if active {
			observer := openRuntimeObserver(t, f.config, c.ID)
			observer.startTurn(t, "running")
			for {
				if _, ok := observer.next(t).(*runtimeclient.TurnStarted); ok {
					break
				}
			}
		}
		if active {
			// 全局定义因活动轮暂缓生效；单会话 reload 仍使用刚装配的命令。
			f.audit = filepath.Join(t.TempDir(), "latest-wire")
			f.configure(t, "-prompt", "wait-cancel")
		}
		p := runtimeprotocol.SessionParams{ConversationID: c.ID, SessionID: c.RootSessionID}
		w.call(t, "session/subscribe", p, nil)
		w.call(t, "session/reload", p, nil)
		w.nextNotification(t, "subscription/closed")
		var snapshot runtimeprotocol.SubscribeSessionResult
		w.call(t, "session/subscribe", p, &snapshot)
		if snapshot.Restoring || snapshot.Options == nil {
			t.Fatalf("reloaded snapshot: %+v", snapshot)
		}
		native := f.openNewConversation(t).conversation
		unsupported := w.response(t, w.request(t, "session/reload", runtimeprotocol.SessionParams{ConversationID: native.ID, SessionID: native.RootSessionID}))
		if unsupported.Error == nil || unsupported.Error.Message != "operation is not supported for native sessions" {
			t.Fatalf("native reload: %+v", unsupported.Error)
		}
		audit, _ := os.ReadFile(f.audit)
		if !bytes.Contains(audit, []byte("session/load")) {
			t.Fatalf("reload: %s", audit)
		}
	}
}
func TestExternalDetach(t *testing.T) {
	for _, tc := range []struct {
		name    string
		restart bool
	}{{"live", false}, {"restart before first input", true}} {
		t.Run(tc.name, func(t *testing.T) { testExternalDetach(t, tc.restart) })
	}
}

func testExternalDetach(t *testing.T, restart bool) {
	f := newExternalFixture(t, "-prompt", "wait-cancel")
	c := f.create(t, "default")
	root := openRuntimeObserver(t, f.config, c.ID)
	root.startTurn(t, "delegate")
	for {
		if _, ok := root.next(t).(*runtimeclient.TurnStarted); ok {
			break
		}
	}
	w := openExternalWire(t, f.config)
	var child conversation.SpawnAgentResult
	w.call(t, "session/spawn", map[string]any{"conversation_id": c.ID, "agent_name": "codex-acp", "task_name": "child", "message": "work"}, &child)
	p := runtimeprotocol.SessionParams{ConversationID: c.ID, SessionID: child.SessionID}
	// 等派活输入交给驱动，再在子会话活动轮内创建孙会话。
	waitExternalPrompt(t, f.audit, "work")
	var grandchild conversation.SpawnAgentResult
	w.call(t, "session/spawn", map[string]any{"conversation_id": c.ID, "source_session_id": child.SessionID, "agent_name": "assistant", "task_name": "grandchild", "message": "verify", "notify_on_finish": false}, &grandchild)
	f.model.next(t).reply <- e2eModelReply{text: "verified"}
	var scopedModels runtime.ModelList
	w.call(t, "model/list", p, &scopedModels)
	if len(scopedModels.Models) == 0 || slices.ContainsFunc(scopedModels.Models, func(m *defs.ModelDefinition) bool { return m.Ref.ProviderID != "codex-acp" }) {
		t.Fatalf("child models: %+v", scopedModels)
	}
	mcpOptions := callExternalMCP(t, f, c.ID, c.RootSessionID, "get_agent_options", map[string]string{"session_id": string(child.SessionID)})
	if !strings.Contains(mcpOptions, "backend-model") {
		t.Fatal(mcpOptions)
	}
	mcpSet := callExternalMCP(t, f, c.ID, c.RootSessionID, "set_agent_option", map[string]string{"session_id": string(child.SessionID), "kind": "model", "value": "large"})
	if strings.Contains(mcpSet, `"isError":true`) {
		t.Fatal(mcpSet)
	}
	waiter := openExternalWire(t, f.config)
	waitID := waiter.request(t, "session/wait", map[string]any{"conversation_id": c.ID, "timeout_seconds": 10})
	var out runtimeprotocol.ConversationResult
	w.call(t, "session/detach", p, &out)
	if out.Conversation.RootSessionID == child.SessionID || out.Conversation.ID == c.ID {
		t.Fatal(out)
	}
	var wait struct {
		DetachedConversationID defs.ConversationID `json:"detached_conversation_id"`
		DetachedSessionID      defs.SessionID      `json:"detached_session_id"`
	}
	response := waiter.response(t, waitID)
	if err := json.Unmarshal(response.Result, &wait); response.Error != nil || err != nil {
		t.Fatalf("pending wait: %+v, %v", response.Error, err)
	}
	if wait.DetachedConversationID != out.Conversation.ID || wait.DetachedSessionID != out.Conversation.RootSessionID {
		t.Fatalf("detach wake: %+v", wait)
	}
	for _, tc := range []struct {
		name string
		env  []string
		args []string
	}{
		{name: "operator explicit source", args: []string{string(c.RootSessionID)}},
		{name: "session token", env: getExternalCLIEnvironment(t, f, c)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"session", "wait", "--timeout", "2"}, tc.args...)
			output, err := runExternalCLI(t.Context(), f, tc.env, args...)
			want := fmt.Sprintf("%s %s\n", out.Conversation.ID, out.Conversation.RootSessionID)
			if err != nil || !strings.Contains(output, want) {
				t.Fatalf("wait after detach: %q, %v, want %q", output, err, want)
			}
		})
	}
	// 列表保留旧身份和转出目的地，CLI 在同一行标出目的地。
	var listed runtimeprotocol.ListSessionsResult
	w.call(t, "session/list", runtimeprotocol.ConversationParams{ConversationID: c.ID}, &listed)
	if !slices.ContainsFunc(listed.Sessions, func(r defs.Session) bool {
		return r.ID == child.SessionID && r.DetachedConversationID == out.Conversation.ID
	}) {
		t.Errorf("session/list: %+v", listed.Sessions)
	}
	mcpText := func(source defs.Conversation, tool string, args map[string]string, wantError bool) string {
		t.Helper()
		wire := callExternalMCP(t, f, source.ID, source.RootSessionID, tool, args)
		var msg struct {
			Result struct {
				Content []struct{ Text string }
				IsError bool
			}
		}
		if err := json.Unmarshal([]byte(wire), &msg); err != nil || msg.Result.IsError != wantError || len(msg.Result.Content) != 1 {
			t.Fatalf("%s: %s, %v", tool, wire, err)
		}
		return msg.Result.Content[0].Text
	}
	var agents conversation.ListAgentsResult
	listText := mcpText(c, "list_agents", map[string]string{"session_id": string(child.SessionID)}, false)
	if err := json.Unmarshal([]byte(listText), &agents); err != nil || len(agents.Agents) != 1 || agents.Agents[0].Session.DetachedConversationID != out.Conversation.ID {
		t.Errorf("list_agents: %s, %v", listText, err)
	}
	lines, err := runExternalCLI(t.Context(), f, getExternalCLIEnvironment(t, f, c), "session", "ls")
	if err != nil || !slices.ContainsFunc(strings.Split(lines, "\n"), func(line string) bool {
		return strings.HasPrefix(line, string(child.SessionID)+" ") && strings.HasSuffix(line, " detached_to="+string(out.Conversation.ID))
	}) {
		t.Errorf("session ls: %q, %v", lines, err)
	}

	redirect := fmt.Sprintf("session detached to conversation %s, new session %s. This session has left the conversation. You can no longer operate on it here. To continue, use the new conversation.", out.Conversation.ID, out.Conversation.RootSessionID)
	for _, method := range []string{"session/send", "session/stop", "session/reload", "session/wait"} {
		params := map[string]any{"conversation_id": c.ID, "session_id": child.SessionID, "message": "old"}
		if method == "session/wait" {
			params = map[string]any{"conversation_id": c.ID, "source_session_id": child.SessionID}
		}
		resp := w.response(t, w.request(t, method, params))
		if resp.Error == nil || resp.Error.Message != redirect {
			t.Errorf("%s: %+v", method, resp.Error)
		}
	}
	for _, tc := range []struct {
		name string
		args map[string]string
	}{
		{"get_agent_options", map[string]string{"session_id": string(child.SessionID)}},
		{"set_agent_option", map[string]string{"session_id": string(child.SessionID), "kind": "model", "value": "large"}},
		{"reload_agent", map[string]string{"session_id": string(child.SessionID)}},
	} {
		if got := mcpText(c, tc.name, tc.args, true); got != redirect {
			t.Errorf("%s: %s", tc.name, got)
		}
	}
	// 结束父轮后，后端的下一个真实 prompt 包含等待期间收到的整批 Agent 消息。
	w.call(t, "conversation/interruptTurn", runtimeprotocol.ConversationParams{ConversationID: c.ID}, nil)
	readTurnFinished(t, root, defs.TurnStatusInterrupted)
	root.startTurn(t, "collect parent inbox")
	delivery := strings.Join(waitExternalPrompt(t, f.audit, "collect parent inbox"), "\n")
	notice := fmt.Sprintf("用户已将子 Agent codex-acp（任务：child，旧会话 %s）转为独立对话。新对话 ID：%s，新会话 ID：%s。它以后不再向你回报，你也不能再给它派活或发消息。", child.SessionID, out.Conversation.ID, out.Conversation.RootSessionID)
	if strings.Count(delivery, "<openvaala-system-info>\n"+notice+"\n</openvaala-system-info>") != 1 || strings.Contains(delivery, "用 send_message 回复它") || strings.Contains(delivery, "openvaala-subagent-notification") || strings.Contains(delivery, "异常退出") {
		t.Errorf("detach reports: %s", delivery)
	}
	parent, err := f.control.GetSessionByID(t.Context(), grandchild.SessionID)
	if err != nil || parent.ParentSessionID != child.SessionID || parent.ConversationID != c.ID {
		t.Fatalf("grandchild: %+v %v", parent, err)
	}
	unsupported := w.response(t, w.request(t, "session/detach", runtimeprotocol.SessionParams{ConversationID: c.ID, SessionID: grandchild.SessionID}))
	if unsupported.Error == nil || unsupported.Error.Message != "operation is not supported for native sessions" {
		t.Fatalf("native detach: %+v", unsupported.Error)
	}
	root.stop()
	w.close(t)
	if restart {
		f.stopServe(t)
		f.startServe(t)
	}
	target := out.Conversation
	reopened := openRuntimeObserver(t, f.config, target.ID)
	reopened.startTurn(t, "detached continuation")
	first := strings.Join(waitExternalPrompt(t, f.audit, "detached continuation"), "\n")
	info := fmt.Sprintf("<openvaala-session-info>\n<session_id>%s</session_id>\n<detached_from_conversation_id>%s</detached_from_conversation_id>\n<detached_from_session_id>%s</detached_from_session_id>\n<hint>你已被用户转为独立对话，现在是主 Agent，没有父 Agent。你原来的子 Agent 留在原对话里。\n你运行在 OpenVaala 中。用 openvaala MCP 的 `get_skill` 工具读取 `openvaala` 查看协作方式（或运行 `openvaala skill get openvaala`）。沙箱内 CLI 无法连接 OpenVaala 时，请改用当前会话提供的 openvaala MCP 工具。</hint>\n</openvaala-session-info>", target.RootSessionID, c.ID, child.SessionID)
	if !strings.HasSuffix(first, info) {
		t.Errorf("new root prompt: %s", first)
	}

	scoped := openExternalSessionWire(t, f.config, target.ID, target.RootSessionID)
	hint := fmt.Sprintf(". This session was detached from conversation %s and can no longer operate on sessions there.", c.ID)
	for _, previous := range []defs.SessionID{c.RootSessionID, grandchild.SessionID} {
		for _, cid := range []defs.ConversationID{target.ID, c.ID} {
			response := scoped.response(t, scoped.request(t, "session/send", map[string]any{"conversation_id": cid, "session_id": previous, "message": "hello"}))
			if response.Error == nil || response.Error.Message != "permission denied"+hint {
				t.Errorf("session/send %s in %s: %+v", previous, cid, response.Error)
			}
		}
		for _, tc := range []struct {
			tool string
			args map[string]string
			want string
		}{
			{"send_message", map[string]string{"session_id": string(previous), "message": "hello"}, fmt.Sprintf("Session %s is outside the collaboration scope of %s: permission denied", previous, target.RootSessionID)},
			{"get_agent_options", map[string]string{"session_id": string(previous)}, "permission denied"},
			{"set_agent_option", map[string]string{"session_id": string(previous), "kind": "model", "value": "large"}, "permission denied"},
			{"reload_agent", map[string]string{"session_id": string(previous)}, "permission denied"},
		} {
			if got := mcpText(target, tc.tool, tc.args, true); got != tc.want+hint {
				t.Errorf("%s %s: %s", tc.tool, previous, got)
			}
		}
	}
	operator := openExternalWire(t, f.config)
	operator.call(t, "conversation/interruptTurn", runtimeprotocol.ConversationParams{ConversationID: target.ID}, nil)
	readTurnFinished(t, reopened, defs.TurnStatusInterrupted)
	reopened.startTurn(t, "second detached input")
	if second := strings.Join(waitExternalPrompt(t, f.audit, "second detached input"), "\n"); strings.Contains(second, "<openvaala-session-info>") {
		t.Errorf("second input: %s", second)
	}
}

func waitExternalPrompt(t *testing.T, audit, body string) []string {
	t.Helper()
	deadline := time.Now().Add(e2eTimeout)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(audit)
		for _, line := range bytes.Split(data, []byte("\n")) {
			var row struct {
				Method string
				Params struct{ Texts []string }
			}
			if json.Unmarshal(line, &row) == nil && row.Method == "prompt-summary" && strings.Contains(strings.Join(row.Params.Texts, "\n"), body) {
				return row.Params.Texts
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no ACP prompt containing %q", body)
	return nil
}

func callExternalMCP(t *testing.T, f *externalFixture, cid defs.ConversationID, sid defs.SessionID, name string, args map[string]string) string {
	t.Helper()
	server, err := getE2EServerConnection(getE2EServerConnectionPath(f.config))
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	endpoint.Scheme = "http"
	endpoint.Path = conversation.CollaborationMCPPath
	data, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint.String(), bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+sessiontoken.Issue(server.Token, cid, sid))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	result, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("MCP %s: %s %v", resp.Status, result, err)
	}
	return string(result)
}
