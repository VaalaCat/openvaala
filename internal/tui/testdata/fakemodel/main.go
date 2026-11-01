// fakemodel 是 tmux 冒烟测试用的假模型服务：兼容 OpenAI Chat 流式接口，按最后一条用户输入里的“工具”“长”“慢”关键词
// 返回不同的回复。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

// chatRequest 是 OpenAI Chat 请求中用到的字段。
type chatRequest struct {
	Messages []chatMessage `json:"messages"`
}

// chatMessage 是请求里的一条消息：content 可能是字符串，也可能是分段文字数组。
type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

const longMarkdown = "# 冒烟长文\n\n这是第一段，用来验证流式定稿：段落写完之后会立刻进入 scrollback。\n\n" +
	"## 步骤\n\n1. 第一步\n\n1. 第二步，编号应当延续为 2\n\n1. 第三步\n\n" +
	"```go\nfunc main() {\n\tfmt.Println(\"你好，世界\")\n}\n```\n\n" +
	"| 名称 | 数量 |\n|------|------|\n| 苹果 | 3 |\n| 香蕉 | 5 |\n\n" +
	"> 引用第一段\n>\n> 引用第二段\n\n最后一段，中文很长很长很长很长很长很长很长很长很长很长很长很长很长很长很长很长很长很长，用来检查断行。\n"

// main 启动冒烟测试的 HTTP 模型服务。
func main() {
	// 1. 读取监听地址并建立 TCP 监听。
	addr := flag.String("addr", "127.0.0.1:18765", "listen address；端口写 0 时由系统分配")
	flag.Parse()
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	// 2. 打印实际监听地址并开始处理模型请求。
	fmt.Printf("listening on %s\n", listener.Addr())
	http.HandleFunc("/", handleChat)
	log.Fatal(http.Serve(listener, nil))
}

// handleChat 读取聊天请求，按最后一条用户输入返回文本或工具调用。
func handleChat(w http.ResponseWriter, r *http.Request) {
	// 1. 解析请求，拒绝无效请求和空消息列表。
	var request chatRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Messages) == 0 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// 2. 最后一条是工具结果时写结论；输入含“工具”时先写一句再调用 bash，结束原因为 tool_calls；
	// 含“长”时把长 Markdown 按每段 12 个字符、段间隔 150ms 写出；含“慢”时每 200ms 写一小段；其余回显输入。
	w.Header().Set("Content-Type", "text/event-stream")
	input := getLastUserText(request.Messages)
	finishReason := "stop"
	switch {
	case request.Messages[len(request.Messages)-1].Role == "tool":
		writeText(w, r, []string{"工具跑完了。\n\n", "结论：一切正常。"}, 30*time.Millisecond)
	case strings.Contains(input, "工具"):
		writeText(w, r, []string{"我先跑个命令。"}, 0)
		writeToolCall(w, "call_smoke", "bash", `{"command":"echo smoke-ok && sleep 1"}`)
		finishReason = "tool_calls"
	case strings.Contains(input, "长"):
		writeText(w, r, splitRunes(longMarkdown, 12), 150*time.Millisecond)
	case strings.Contains(input, "慢"):
		writeText(w, r, strings.SplitAfter(strings.Repeat("慢慢来，", 40), "，"), 200*time.Millisecond)
	default:
		writeText(w, r, []string{"收到：", input}, 0)
	}
	// 3. 写出结束原因和流结束标记。
	_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":%q}]}\n\ndata: [DONE]\n\n", finishReason)
}

// getLastUserText 返回最后一条用户消息的文本。
func getLastUserText(messages []chatMessage) string {
	// 1. 从后往前查找用户消息，读取字符串或拼接分段文字。
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role != "user" {
			continue
		}
		var text string
		if json.Unmarshal(messages[index].Content, &text) == nil {
			return text
		}
		var parts []struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(messages[index].Content, &parts)
		var joined strings.Builder
		for _, part := range parts {
			joined.WriteString(part.Text)
		}
		return joined.String()
	}
	return ""
}

// writeText 按段写出文本增量。
func writeText(w http.ResponseWriter, r *http.Request, chunks []string, delay time.Duration) {
	// 1. 逐段写出文本，段间等待 delay；等待时收到请求取消就返回。
	for _, chunk := range chunks {
		writeEvent(w, map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": chunk}}}})
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
}

// writeToolCall 写出一个完整的工具调用。
func writeToolCall(w http.ResponseWriter, callID, name, arguments string) {
	// 1. 组装工具调用并写出增量事件。
	call := map[string]any{"index": 0, "id": callID, "type": "function", "function": map[string]any{"name": name, "arguments": arguments}}
	writeEvent(w, map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{call}}}}})
}

// writeEvent 编码并立即刷新一条 SSE 事件。
func writeEvent(w http.ResponseWriter, event any) {
	// 1. 编码事件，写入响应并刷新缓冲。
	encoded, _ := json.Marshal(event)
	fmt.Fprintf(w, "data: %s\n\n", encoded)
	w.(http.Flusher).Flush()
}

// splitRunes 按字符数将文本切段。
func splitRunes(text string, count int) []string {
	// 1. 按 Unicode 字符切片，不拆开 UTF-8，每段最多 count 个字符。
	runes := []rune(text)
	var chunks []string
	for start := 0; start < len(runes); start += count {
		chunks = append(chunks, string(runes[start:min(start+count, len(runes))]))
	}
	return chunks
}
