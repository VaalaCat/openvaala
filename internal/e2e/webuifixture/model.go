package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
)

// modelReply 是假模型的一次回复：依次写出推理、文字和一次工具调用；ChunkDelayMs 大于 0 时文字逐字吐出，
// 每个字之间等这么久；Hold 时写完后挂起，直到请求被取消（例如 Turn 被中断）；Status 非零时返回 HTTP 错误。
type modelReply struct {
	Text            string          `json:"text,omitempty"`
	Reasoning       string          `json:"reasoning,omitempty"`
	ReasoningChunks []string        `json:"reasoningChunks,omitempty"`
	Tool            string          `json:"tool,omitempty"`
	Arguments       string          `json:"arguments,omitempty"`
	ArgumentChunks  []string        `json:"argumentChunks,omitempty"`
	ChunkDelayMs    int             `json:"chunkDelayMs,omitempty"`
	Hold            bool            `json:"hold,omitempty"`
	ToolDelayMs     int             `json:"toolDelayMs,omitempty"`
	FinishDelayMs   int             `json:"finishDelayMs,omitempty"`
	Tools           []modelToolCall `json:"tools,omitempty"`
	Status          int             `json:"status,omitempty"`
	Error           string          `json:"error,omitempty"`
}

// modelToolCall 是一次模型回复中的工具调用，按列表顺序发送。
type modelToolCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// fakeModel 是 OpenAI Chat Completions 兼容的假模型：按顺序取测试排好的回复，队列空时回 ok；
// GET /v1/models 返回测试设置的模型 id。
type fakeModel struct {
	// replies 是排好的回复，按 FIFO 交给之后的模型请求。
	replies chan modelReply
	// modelIDs 是 GET /v1/models 列出的模型 id。
	modelIDs atomic.Pointer[[]string]
	// toolCalls 为每次工具调用生成不同的 call id。
	toolCalls atomic.Int32
}

// newFakeModel 创建回复队列为空、模型列表为空的假模型。
func newFakeModel() *fakeModel {
	// 1. 队列容量远大于一次测试会排的回复数，追加时不会阻塞。
	model := &fakeModel{replies: make(chan modelReply, 1024)}
	model.modelIDs.Store(&[]string{})
	return model
}

// handleChatCompletions 取队首回复，回一次流式 Chat Completions。
func (m *fakeModel) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	// 1. 取这次的回复。
	reply := m.takeReply()
	if reply.Status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(reply.Status)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": reply.Error, "type": "server_error", "code": "fixture_model_error"}})
		return
	}
	// 2. 写推理和文字：有吐字间隔时逐字刷新，请求取消就停。
	w.Header().Set("Content-Type", "text/event-stream")
	flusher := w.(http.Flusher)
	if reply.Reasoning != "" {
		writeDelta(w, map[string]any{"reasoning_content": reply.Reasoning})
	}
	for _, chunk := range reply.ReasoningChunks {
		writeDelta(w, map[string]any{"reasoning_content": chunk})
		flusher.Flush()
		select {
		case <-time.After(time.Duration(reply.ChunkDelayMs) * time.Millisecond):
		case <-r.Context().Done():
			return
		}
	}
	if reply.ChunkDelayMs > 0 {
		for _, char := range reply.Text {
			writeDelta(w, map[string]any{"content": string(char)})
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
	// 3. 写工具调用，延迟时先刷新思考；Hold 时等请求取消。
	if reply.ToolDelayMs > 0 {
		flusher.Flush()
		select {
		case <-time.After(time.Duration(reply.ToolDelayMs) * time.Millisecond):
		case <-r.Context().Done():
			return
		}
	}
	tools := reply.Tools
	if reply.Tool != "" {
		tools = append([]modelToolCall{{Name: reply.Tool, Arguments: reply.Arguments}}, tools...)
	}
	for index, tool := range tools {
		callID := fmt.Sprintf("call-%d", m.toolCalls.Add(1))
		writeDelta(w, map[string]any{"tool_calls": []map[string]any{{
			"index":    index,
			"id":       callID,
			"type":     "function",
			"function": map[string]string{"name": tool.Name, "arguments": tool.Arguments},
		}}})
		if index == 0 {
			for _, chunk := range reply.ArgumentChunks {
				flusher.Flush()
				select {
				case <-time.After(time.Duration(reply.ChunkDelayMs) * time.Millisecond):
				case <-r.Context().Done():
					return
				}
				writeDelta(w, map[string]any{"tool_calls": []map[string]any{{
					"index":    index,
					"function": map[string]string{"arguments": chunk},
				}}})
			}
		}
	}
	if reply.FinishDelayMs > 0 {
		flusher.Flush()
		select {
		case <-time.After(time.Duration(reply.FinishDelayMs) * time.Millisecond):
		case <-r.Context().Done():
			return
		}
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

// handleListModels 按 OpenAI /models 的格式列出测试设置的模型 id。
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

// takeReply 取出队首回复，队列空时回 ok。
func (m *fakeModel) takeReply() modelReply {
	// 1. 不等待：没有排好的回复就用默认文字。
	select {
	case reply := <-m.replies:
		return reply
	default:
		return modelReply{Text: "ok"}
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
