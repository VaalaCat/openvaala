package e2e

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// TestCodexSideConversation 检查 Codex 的 /btw、/side：从运行中的主 Thread 分叉出临时 Thread，开发者指令和注入的消息排在提问之前，
// 分叉里有 Executor 工具，主 Thread 的历史和对话列表不受影响，关闭旁支时的中断和退订成功。
// 临时分叉没人用后由 serve 丢弃，等待时长不能通过配置缩短，丢弃由 pkg/runtime 的 TestRuntimeEphemeralFork 覆盖，这里只断言退订成功。
func TestCodexSideConversation(t *testing.T) {
	// 1. 编译程序，分别在 stdio 和 Unix 传输上运行。
	binary := buildOpenVaala(t)
	for _, mode := range []string{"stdio", "unix"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 启动 app-server 并握手，主 Thread 开一轮后停在模型输出一半。
			model := newE2EModel(t)
			config := writeE2EConfig(t, model.server.URL)
			wire := startAppServer(t, binary, config, mode)
			wire.call(t, "initialize", codexClientInfo, nil)
			fixture := &codexTurnFixture{
				wire:  wire,
				model: model,
			}
			workspace := newE2EWorkspace(t)
			mainID := fixture.startThread(t, workspace, nil).Thread.ID
			mainTurnID := fixture.startHeldTurn(t, mainID)

			// 2. 带开发者指令分叉出临时 Thread：回复带 ephemeral、来源和有 Executor 的沙箱，随后发 thread/started。
			var forked struct {
				Thread struct {
					ID           string  `json:"id"`
					Ephemeral    bool    `json:"ephemeral"`
					ForkedFromID *string `json:"forkedFromId"`
				} `json:"thread"`
				Sandbox struct {
					Type string `json:"type"`
				} `json:"sandbox"`
			}
			wire.call(t, "thread/fork", map[string]any{
				"threadId": mainID, "ephemeral": true, "developerInstructions": "side rules", "excludeTurns": true,
			}, &forked)
			sideID := forked.Thread.ID
			if sideID == "" || sideID == mainID || !forked.Thread.Ephemeral || forked.Thread.ForkedFromID == nil || *forked.Thread.ForkedFromID != mainID ||
				forked.Sandbox.Type != "dangerFullAccess" {
				t.Fatalf("thread/fork = %+v, want a new ephemeral thread forked from %s whose sandbox is dangerFullAccess because it has executors", forked, mainID)
			}
			if started := wire.nextThreadNotification(t, sideID, "thread/started").getNotification(t); started.Thread.ID != sideID {
				t.Fatalf("thread/started for %s, want %s", started.Thread.ID, sideID)
			}

			// 3. 注入一条 user 消息，再对分叉提问；模型先调 read_file 读工作区文件，再回复。
			var injected map[string]any
			wire.call(t, "thread/inject_items", map[string]any{
				"threadId": sideID,
				"items": []any{map[string]any{
					"type": "message", "role": "user",
					"content": []any{map[string]any{"type": "input_text", "text": "boundary"}},
				}},
			}, &injected)
			if len(injected) != 0 {
				t.Fatalf("thread/inject_items = %v, want {}", injected)
			}
			wire.request(t, "turn/start", map[string]any{"threadId": sideID, "input": []any{codexTextInput("what is in notes.txt?")}})
			first := model.next(t)
			first.reply <- e2eModelReply{tool: "read_file", arguments: `{"path":"` + workspace + `/notes.txt"}`}
			second := model.next(t)
			second.reply <- e2eModelReply{text: "side answer", hold: true}

			// 4. 分叉这一轮的第一次模型请求：提问之前依次是开发者指令和注入的消息，有 read_file；第二次请求带着文件内容。
			messages := getModelMessageTexts(first.body)
			wantTail := []modelMessageText{
				{role: "user", text: "客户端指令\n\nside rules"},
				{role: "user", text: "客户端指令\n\nboundary"},
				{role: "user", text: "what is in notes.txt?"},
			}
			if len(messages) < len(wantTail) || !slices.Equal(messages[len(messages)-len(wantTail):], wantTail) {
				t.Fatalf("side model messages = %+v, want the last three to be %+v", messages, wantTail)
			}
			if tools := getModelToolNames(first.body); !slices.Contains(tools, "read_file") {
				t.Fatalf("side model tools = %v, want read_file in an ephemeral fork", tools)
			}
			if !strings.Contains(fmt.Sprint(second.body), "WORKSPACE_CONTENT") {
				t.Fatalf("second side model request = %v, want the content of notes.txt", second.body)
			}
			wire.nextThreadNotification(t, sideID, "item/agentMessage/delta")

			// 5. 关闭旁支：中断分叉这一轮，再退订分叉。
			wire.call(t, "turn/interrupt", map[string]any{"threadId": sideID}, nil)
			if completed := wire.nextThreadNotification(t, sideID, "turn/completed").getNotification(t).Turn; completed.Status != "interrupted" {
				t.Fatalf("side turn/completed = %+v, want interrupted", completed)
			}
			var unsubscribed struct {
				Status string `json:"status"`
			}
			wire.call(t, "thread/unsubscribe", map[string]any{"threadId": sideID}, &unsubscribed)
			if unsubscribed.Status != "unsubscribed" {
				t.Fatalf("thread/unsubscribe = %+v, want unsubscribed", unsubscribed)
			}

			// 6. 主 Thread 的历史里没有旁支的指令、提问和回答，对话列表里没有旁支。
			var history json.RawMessage
			wire.call(t, "thread/read", map[string]any{"threadId": mainID, "includeTurns": true}, &history)
			for _, leaked := range []string{"side rules", "boundary", "notes.txt", "side answer", "WORKSPACE_CONTENT"} {
				if strings.Contains(string(history), leaked) {
					t.Fatalf("main thread history contains %q from the side conversation: %s", leaked, history)
				}
			}
			var threads codexWireThreadList
			wire.call(t, "thread/list", map[string]any{}, &threads)
			if ids := threads.getIDs(); !slices.Contains(ids, mainID) || slices.Contains(ids, sideID) {
				t.Fatalf("thread/list = %v, want the main thread %s and not the side conversation %s", ids, mainID, sideID)
			}

			// 7. 主 Thread 的运行不受旁支影响，中断后以 interrupted 收尾。
			wire.call(t, "turn/interrupt", map[string]any{"threadId": mainID, "turnId": mainTurnID}, nil)
			if completed := wire.nextThreadNotification(t, mainID, "turn/completed").getNotification(t).Turn; completed.ID != mainTurnID || completed.Status != "interrupted" {
				t.Fatalf("main turn/completed = %+v, want turn %s interrupted", completed, mainTurnID)
			}
		})
	}
}
