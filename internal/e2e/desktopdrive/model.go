package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// modelReply 是假模型的一次回复：依次写出推理、文字和一次工具调用；ChunkDelayMs 大于 0 时文字按小段吐出，
// 每段之间等这么久；Hold 时写完后挂起，直到请求被取消（例如 Turn 被中断）；Status 非零时返回 HTTP 错误。
type modelReply struct {
	Text         string `json:"text,omitempty"`
	Reasoning    string `json:"reasoning,omitempty"`
	Tool         string `json:"tool,omitempty"`
	Arguments    string `json:"arguments,omitempty"`
	ChunkDelayMs int    `json:"chunkDelayMs,omitempty"`
	Hold         bool   `json:"hold,omitempty"`
	Status       int    `json:"status,omitempty"`
	Error        string `json:"error,omitempty"`
}

// fakeModel 是 OpenAI Chat Completions 兼容的假模型：按顺序取排好的回复，队列空时流式回显最后一条用户消息；
// GET /v1/models 返回设置的模型 id。
type fakeModel struct {
	// replies 是排好的回复，按 FIFO 交给之后的模型请求。
	replies chan modelReply
	// modelIDs 是 GET /v1/models 列出的模型 id。
	modelIDs atomic.Pointer[[]string]
	// toolCalls 为每次工具调用生成不同的 call id。
	toolCalls atomic.Int32
	// requestLock 保护 lastUserText 和 requestCount。
	requestLock sync.Mutex
	// lastUserText 是最近一次模型请求里最后一条用户消息的文字。
	lastUserText string
	// requestCount 是已经收到的模型请求数。
	requestCount int
}

// newFakeModel 创建回复队列为空、模型列表为空的假模型。
func newFakeModel() *fakeModel {
	// 1. 队列容量远大于一次会话会排的回复数，追加时不会阻塞。
	model := &fakeModel{replies: make(chan modelReply, 1024)}
	model.modelIDs.Store(&[]string{})
	return model
}

// newModelHandler 返回假模型的 HTTP 入口：/v1 给被测程序，其余是 model 子命令的控制接口。
func newModelHandler(model *fakeModel) http.Handler {
	// 1. 注册 OpenAI 兼容接口和控制接口。
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", model.handleChatCompletions)
	mux.HandleFunc("GET /v1/models", model.handleListModels)
	mux.HandleFunc("POST /replies", model.handleAppendReplies)
	mux.HandleFunc("POST /replies/reset", model.handleResetReplies)
	mux.HandleFunc("POST /models", model.handleSetModels)
	mux.HandleFunc("GET /requests", model.handleGetRequests)
	return mux
}

// handleChatCompletions 取队首回复，回一次流式 Chat Completions。
func (m *fakeModel) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	// 1. 记录请求，取这次的回复；队列空时回显用户消息。
	userText := m.recordRequest(r)
	reply := m.takeReply(userText)
	if reply.Status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(reply.Status)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": reply.Error, "type": "server_error", "code": "fixture_model_error"}})
		return
	}
	// 2. 写推理和文字：有吐字间隔时分段刷新，请求取消就停。
	w.Header().Set("Content-Type", "text/event-stream")
	flusher := w.(http.Flusher)
	if reply.Reasoning != "" {
		writeDelta(w, map[string]any{"reasoning_content": reply.Reasoning})
		flusher.Flush()
	}
	if reply.ChunkDelayMs > 0 {
		runes := []rune(reply.Text)
		for start := 0; start < len(runes); start += 4 {
			writeDelta(w, map[string]any{"content": string(runes[start:min(start+4, len(runes))])})
			flusher.Flush()
			select {
			case <-time.After(time.Duration(reply.ChunkDelayMs) * time.Millisecond):
			case <-r.Context().Done():
				return
			}
		}
	} else if reply.Text != "" {
		writeDelta(w, map[string]any{"content": reply.Text})
	}
	// 3. 写工具调用；Hold 时等请求取消。
	if reply.Tool != "" {
		writeDelta(w, map[string]any{"tool_calls": []map[string]any{{
			"index":    0,
			"id":       fmt.Sprintf("call-%d", m.toolCalls.Add(1)),
			"type":     "function",
			"function": map[string]string{"name": reply.Tool, "arguments": reply.Arguments},
		}}})
	}
	if reply.Hold {
		flusher.Flush()
		<-r.Context().Done()
		return
	}
	// 4. 写结束原因、用量和流结束标记。
	fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":120,\"completion_tokens\":4,\"total_tokens\":124}}\n\n")
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// recordRequest 解码请求体，记下最后一条用户消息并返回它。
func (m *fakeModel) recordRequest(r *http.Request) string {
	// 1. 请求体只取 messages 里最后一条 user 消息，content 是字符串或内容片段数组。
	var body struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	payload, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(payload, &body)
	text := ""
	for _, message := range body.Messages {
		if message.Role != "user" {
			continue
		}
		var plain string
		var parts []struct {
			Text string `json:"text"`
		}
		switch {
		case json.Unmarshal(message.Content, &plain) == nil:
			text = plain
		case json.Unmarshal(message.Content, &parts) == nil:
			texts := make([]string, 0, len(parts))
			for _, part := range parts {
				texts = append(texts, part.Text)
			}
			text = strings.Join(texts, "")
		}
	}
	// 2. 在锁内保存。
	m.requestLock.Lock()
	defer m.requestLock.Unlock()
	m.lastUserText = text
	m.requestCount++
	return text
}

// getPendingReplyCount 返回还没被模型请求取走的排队回复数。
func (m *fakeModel) getPendingReplyCount() int {
	return len(m.replies)
}

// handleGetRequests 返回已收到的模型请求数和最近一次的用户消息。
func (m *fakeModel) handleGetRequests(w http.ResponseWriter, _ *http.Request) {
	// 1. 在锁内读取。
	m.requestLock.Lock()
	defer m.requestLock.Unlock()
	writeJSON(w, map[string]any{"count": m.requestCount, "lastUserText": m.lastUserText, "pendingReplies": len(m.replies)})
}

// handleListModels 按 OpenAI /models 的格式列出设置的模型 id。
func (m *fakeModel) handleListModels(w http.ResponseWriter, _ *http.Request) {
	// 1. 每个 id 写成一条 model 记录。
	models := []map[string]string{}
	for _, id := range *m.modelIDs.Load() {
		models = append(models, map[string]string{"id": id, "object": "model"})
	}
	writeJSON(w, map[string]any{"object": "list", "data": models})
}

// handleAppendReplies 把请求里的回复按顺序追加到队列末尾。
func (m *fakeModel) handleAppendReplies(w http.ResponseWriter, r *http.Request) {
	// 1. 解码后逐条入队。
	var request struct {
		Replies []modelReply `json:"replies"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	for _, reply := range request.Replies {
		m.replies <- reply
	}
	writeJSON(w, map[string]any{})
}

// handleResetReplies 清空还没被取走的回复；已经在回的请求不受影响。
func (m *fakeModel) handleResetReplies(w http.ResponseWriter, _ *http.Request) {
	// 1. 不等待地取完队列。
	for {
		select {
		case <-m.replies:
		default:
			writeJSON(w, map[string]any{})
			return
		}
	}
}

// handleSetModels 换掉 GET /v1/models 列出的模型 id。
func (m *fakeModel) handleSetModels(w http.ResponseWriter, r *http.Request) {
	// 1. 解码后整体替换。
	var request struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ids := append([]string{}, request.IDs...)
	m.modelIDs.Store(&ids)
	writeJSON(w, map[string]any{})
}

// takeReply 取出队首回复；队列空时流式回显用户消息，让桌面能看到逐段出现的文字。
func (m *fakeModel) takeReply(userText string) modelReply {
	// 1. 不等待：没有排好的回复就回显。
	select {
	case reply := <-m.replies:
		return reply
	default:
		return modelReply{Text: "fake model: " + userText, ChunkDelayMs: 15}
	}
}

// writeDelta 写一条只带 delta 的 SSE 事件。
func writeDelta(w http.ResponseWriter, delta map[string]any) {
	// 1. 编码成 choices[0].delta。
	payload, _ := json.Marshal(map[string]any{"choices": []map[string]any{{"delta": delta}}})
	fmt.Fprintf(w, "data: %s\n\n", payload)
}

// writeJSON 写 200 和 JSON 正文。
func writeJSON(w http.ResponseWriter, value any) {
	// 1. 设置类型后编码。
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

// runModelCommand 是 model 子命令：经运行中的控制接口排回复、排错误、清队列、换模型列表或查看收到的请求。
func runModelCommand(arguments []string) error {
	// 1. 第一个参数选动作，其余交给动作自己的 flag。
	if len(arguments) == 0 {
		return fmt.Errorf("model: need an action: reply, tool, error, reset, models, requests")
	}
	action, rest := arguments[0], arguments[1:]
	flags, statePath := newFlags("model " + action)
	delay := flags.Int("delay-ms", 0, "reply: stream the text in small pieces, waiting this long between pieces")
	hold := flags.Bool("hold", false, "reply, tool: hang after writing until the Turn is interrupted")
	reasoning := flags.String("reasoning", "", "reply: reasoning text written before the answer")
	status := flags.Int("status", 500, "error: HTTP status")
	positional, err := parseFlags(flags, rest)
	if err != nil {
		return err
	}
	state, err := loadState(*statePath)
	if err != nil {
		return err
	}
	post := func(path string, body any) error {
		payload, _ := json.Marshal(body)
		response, err := http.Post(state.Control+path, "application/json", strings.NewReader(string(payload)))
		if err != nil {
			return err
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode != http.StatusOK {
			message, _ := io.ReadAll(response.Body)
			return fmt.Errorf("model control %s: %s %s", path, response.Status, message)
		}
		return nil
	}
	queue := func(reply modelReply) error {
		if err := post("/replies", map[string]any{"replies": []modelReply{reply}}); err != nil {
			return err
		}
		fmt.Println("queued next model reply")
		return nil
	}
	// 2. 按动作调用控制接口。
	switch action {
	case "reply":
		if len(positional) == 0 {
			return fmt.Errorf("model reply <text> [--delay-ms N] [--reasoning text] [--hold]")
		}
		return queue(modelReply{Text: strings.ReplaceAll(strings.Join(positional, " "), `\n`, "\n"), Reasoning: *reasoning, ChunkDelayMs: *delay, Hold: *hold})
	case "tool":
		if len(positional) < 1 {
			return fmt.Errorf("model tool <name> [json-arguments], for example: model tool bash '{\"command\":\"ls\"}'")
		}
		arguments := "{}"
		if len(positional) > 1 {
			arguments = strings.Join(positional[1:], " ")
		}
		return queue(modelReply{Tool: positional[0], Arguments: arguments, Hold: *hold})
	case "error":
		message := "fake model error"
		if len(positional) > 0 {
			message = strings.Join(positional, " ")
		}
		return queue(modelReply{Status: *status, Error: message})
	case "reset":
		if err := post("/replies/reset", map[string]any{}); err != nil {
			return err
		}
		fmt.Println("cleared queued model replies")
		return nil
	case "models":
		return post("/models", map[string]any{"ids": positional})
	case "requests":
		response, err := http.Get(state.Control + "/requests")
		if err != nil {
			return err
		}
		defer func() { _ = response.Body.Close() }()
		_, err = io.Copy(os.Stdout, response.Body)
		return err
	}
	return fmt.Errorf("model: unknown action %q", action)
}
