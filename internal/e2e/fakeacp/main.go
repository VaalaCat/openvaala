// Command fakeacp 是经真实 stdio 与测试客户端对话的 ACP 后端。
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/prompt"
)

// wireMessage 是测试后端读写的一条 JSON-RPC 消息。
type wireMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
}
type object = map[string]any

func main() {
	record := flag.String("record", "", "写测试用 JSONL 方法记录")
	load := flag.Bool("load", true, "声明恢复能力")
	list := flag.Bool("list", true, "声明列举能力")
	httpCap := flag.Bool("http", true, "声明 HTTP MCP")
	image := flag.Bool("image", true, "声明图片能力")
	behavior := flag.String("prompt", "end_turn", "prompt 结束原因或 error、exit、wait-cancel、ignore cancel")
	historyFile := flag.String("history-file", "", "保存最近一次 prompt 并在 load 时回放")
	promptChunks := flag.Int("prompt-chunks", 0, "连续发送的实时文字分片数")
	historyCount := flag.Int("history-count", 0, "生成指定条数的回放用户消息")
	newID := flag.String("new-id", "fake-session", "新建会话返回的 ID")
	newGate := flag.String("new-gate", "", "等待测试创建文件后再响应新建")
	promptGate := flag.String("prompt-gate", "", "等待测试创建文件后再结束 prompt")
	loadGate := flag.String("load-gate", "", "等待测试创建文件后再回放")
	failNew := flag.Bool("fail-new", false, "拒绝新建会话，仍可恢复已有会话")
	failProbe := flag.Bool("fail-probe", false, "拒绝没有宿主会话 ID 的选项探测")
	failLoadFile := flag.String("fail-load-file", "", "文件存在时在回放首条后返回失败")
	exitPromptFile := flag.String("exit-prompt-file", "", "文件存在时在处理 prompt 时退出")
	stall := flag.String("stall", "", "暂停指定方法")
	configMode := flag.Bool("config-mode", false, "用配置项报告模式")
	legacyMode := flag.Bool("legacy-mode", false, "同时报告旧模式字段")
	number := flag.Int("sessions", 5, "可列举的会话数")
	exerciseMCP := flag.Bool("exercise-mcp", false, "每轮调用本地 MCP 工具")
	perm := flag.String("permission", "", "请求权限：always、once 或 none")
	flag.Parse()
	var audit *os.File
	if *record != "" {
		var err error
		audit, err = os.OpenFile(*record, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			panic(err)
		}
		defer audit.Close()
	}
	logCall := func(method string, p any) {
		if audit != nil {
			if err := json.NewEncoder(audit).Encode(object{"method": method, "params": p}); err != nil {
				panic(err)
			}
		}
	}
	// 测试记录仅含验收用的变量。
	logCall("started", object{"path": os.Getenv("PATH"), "overlay": os.Getenv("TEST_OVERLAY"), "session": os.Getenv("OPENVAALA_SESSION_ID"), "conversation": os.Getenv("OPENVAALA_CONVERSATION_ID"), "server": os.Getenv("OPENVAALA_URL"), "hasToken": os.Getenv("OPENVAALA_TOKEN") != "", "fixtureToken": os.Getenv("OPENVAALA_TOKEN") == "fixture-session-token"})
	output := json.NewEncoder(os.Stdout)
	write := func(m object) {
		m["jsonrpc"] = "2.0"
		if err := output.Encode(m); err != nil {
			os.Exit(1)
		}
	}
	id := *newID
	mode := "plan"
	model := "small"
	thought := "low"
	configs := func() []object {
		groups := []object{}
		for _, g := range [][4]string{{"backend-model", "model", model, "large"}, {"effort", "thought_level", thought, "high"}, {"secondary", "model", "small", "large"}} {
			groups = append(groups, object{"id": g[0], "name": g[0], "type": "select", "category": g[1], "currentValue": g[2], "options": []object{{"name": g[2], "value": g[2]}, {"name": g[3], "value": g[3]}}})
		}
		if *configMode {
			groups = append(groups, object{"id": "workflow", "name": "Workflow", "type": "select", "category": "mode", "currentValue": mode, "options": []object{{"value": "plan", "name": "Plan"}, {"value": "code", "name": "Code"}}})
		}
		return groups
	}
	modes := func() object {
		if *configMode && !*legacyMode {
			return nil
		}
		return object{"currentModeId": mode, "availableModes": []object{{"id": "plan", "name": "Plan"}, {"id": "code", "name": "Code"}}}
	}
	notify := func(kind string, u object) {
		u["sessionUpdate"] = kind
		write(object{"method": "session/update", "params": object{"sessionId": id, "update": u}})
	}
	chunk := func(kind, msgID, text string) {
		notify(kind, object{"messageId": msgID, "content": object{"type": "text", "text": text}})
	}
	var promptID, openParams json.RawMessage
	end := func(reason string) {
		write(object{"id": promptID, "result": object{"stopReason": reason, "usage": object{"inputTokens": 12, "outputTokens": 2, "totalTokens": 14}}})
		promptID = nil
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 65536), 10<<20)
	for scanner.Scan() {
		var m wireMessage
		if err := json.Unmarshal(scanner.Bytes(), &m); err != nil {
			panic(err)
		}
		if m.Method == "" {
			logCall("permission-result", m.Result)
			if promptID != nil {
				end("end_turn")
			}
			continue
		}
		if m.Method != "session/prompt" {
			logCall(m.Method, m.Params)
		}
		if m.Method == *stall {
			continue
		}
		var result any
		switch m.Method {
		case "initialize":
			caps := object{}
			if *list {
				caps["list"] = object{}
			}
			result = object{"protocolVersion": 1, "agentCapabilities": object{"loadSession": *load, "sessionCapabilities": caps, "mcpCapabilities": object{"http": *httpCap}, "promptCapabilities": object{"image": *image}}, "authMethods": []any{}}
		case "session/new", "session/load":
			openParams = m.Params
			if m.Method == "session/new" && (*failNew || (*failProbe && os.Getenv("OPENVAALA_SESSION_ID") == "")) {
				write(object{"id": m.ID, "error": object{"code": -32007, "message": "fixture rejected new session"}})
				continue
			}
			gate := *newGate
			if m.Method == "session/load" {
				gate = *loadGate
			}
			waitForGate(gate)
			if m.Method == "session/load" {
				var p struct{ SessionID string }
				json.Unmarshal(m.Params, &p)
				id = p.SessionID
				if !*load {
					write(object{"id": m.ID, "error": object{"code": -32601, "message": "load unsupported"}})
					continue
				}
				if _, err := os.Stat(*failLoadFile); err == nil {
					chunk("user_message_chunk", "partial", "first partial")
					chunk("user_message_chunk", "partial-next", "second partial")
					write(object{"id": m.ID, "error": object{"code": -32008, "message": "fixture partial replay"}})
					continue
				}
				if *historyCount > 0 {
					for i := range *historyCount {
						chunk("user_message_chunk", strconv.Itoa(i), fmt.Sprintf("message %d", i))
					}
				} else if *historyFile != "" {
					data, err := os.ReadFile(*historyFile)
					if err != nil {
						panic(err)
					}
					var blocks []json.RawMessage
					if err := json.Unmarshal(data, &blocks); err != nil {
						panic(err)
					}
					for _, b := range blocks {
						notify("user_message_chunk", object{"messageId": "saved", "content": b})
					}
				} else {
					chunk("user_message_chunk", "one", "hello ")
					chunk("user_message_chunk", "one", "world")
					chunk("agent_message_chunk", "answer", "history answer")
					notify("future_update", object{"content": "extension payload"})
					chunk("user_message_chunk", "two", "latest\n\n"+prompt.FormatSessionInfo(defs.Session{ID: "previous"}))
				}
			}
			notify("available_commands_update", object{"availableCommands": []object{{"name": "status", "description": "fixture command"}}})
			result = object{"sessionId": id, "modes": modes(), "configOptions": configs()}
		case "session/list":
			var p struct{ Cursor, Cwd string }
			json.Unmarshal(m.Params, &p)
			offset, _ := strconv.Atoi(p.Cursor)
			rows := []object{}
			for i := offset; i < min(offset+5, *number); i++ {
				title := fmt.Sprintf("chat %d", i)
				if i == 2 || i == 4 {
					title = "Needle"
				}
				rows = append(rows, object{"sessionId": fmt.Sprintf("id-%d", i), "cwd": p.Cwd, "title": title, "updatedAt": "2025-01-03T12:00:00Z"})
			}
			page := object{"sessions": rows}
			if offset+len(rows) < *number {
				page["nextCursor"] = strconv.Itoa(offset + len(rows))
			}
			result = page
		case "session/set_mode":
			var p struct{ ModeID string }
			json.Unmarshal(m.Params, &p)
			mode = p.ModeID
			notify("current_mode_update", object{"currentModeId": mode})
			result = object{}
		case "session/set_config_option":
			var p struct {
				ConfigID string
				Value    string
			}
			json.Unmarshal(m.Params, &p)
			switch p.ConfigID {
			case "backend-model":
				model = p.Value
				if model == "large" {
					thought = "high"
				}
			case "workflow":
				mode = p.Value
			case "effort":
				thought = p.Value
			default:
				write(object{"id": m.ID, "error": object{"code": -32602, "message": "unknown option"}})
				continue
			}
			notify("config_option_update", object{"configOptions": configs()})
			result = object{"configOptions": configs()}
		case "session/prompt":
			promptID = m.ID
			if *exerciseMCP {
				outputs, err := exerciseLocalMCP(openParams)
				if err != nil {
					write(object{"id": m.ID, "error": object{"code": -32008, "message": err.Error()}})
					continue
				}
				logCall("stdio-mcp-results", outputs)
			}
			if *historyFile != "" {
				var history struct{ Prompt json.RawMessage }
				if err := json.Unmarshal(m.Params, &history); err != nil {
					panic(err)
				}
				if err := os.WriteFile(*historyFile, history.Prompt, 0600); err != nil {
					panic(err)
				}
			}
			var p struct{ Prompt []struct{ Type, Text string } }
			json.Unmarshal(m.Params, &p)
			texts := []string{}
			images := 0
			for _, b := range p.Prompt {
				if b.Type == "image" {
					images++
				} else {
					texts = append(texts, b.Text)
				}
			}
			logCall("prompt-summary", object{"images": images, "texts": texts, "lineBytes": len(scanner.Bytes()) + 1})
			waitForGate(*promptGate)
			if _, err := os.Stat(*exitPromptFile); err == nil {
				os.Exit(23)
			}
			switch *behavior {
			case "exit":
				os.Exit(23)
			case "error":
				write(object{"id": m.ID, "error": object{"code": -32008, "message": "fixture failure"}})
				continue
			case "ignore cancel", "wait-cancel":
				continue
			}
			notify("usage_update", object{"used": 24, "size": 100})
			chunk("agent_thought_chunk", "thought", "thinking")
			if *behavior == "updates" {
				notify("tool_call", object{"toolCallId": "tool1", "title": "fixture", "status": "in_progress", "rawInput": object{"file": "sample"}})
				notify("tool_call_update", object{"toolCallId": "tool1", "rawOutput": "old"})
				notify("tool_call_update", object{"toolCallId": "tool1", "rawOutput": "done"})
				notify("tool_call", object{"toolCallId": "tool2", "title": "second", "content": []object{{"type": "content", "content": object{"type": "text", "text": "stored"}}}})
				notify("tool_call_update", object{"toolCallId": "tool1", "status": "completed"})
				notify("tool_call_update", object{"toolCallId": "tool2", "status": "failed"})
				notify("available_commands_update", object{"availableCommands": []object{{"name": "more", "description": "live command"}}})
			}
			if *behavior == "cancelled" {
				end("cancelled")
				continue
			}
			for range *promptChunks {
				chunk("agent_message_chunk", "answer", "x")
			}
			chunk("agent_message_chunk", "answer", "OK")
			if *perm != "" {
				options := []object{{"kind": "reject_once", "optionId": "r", "name": "Reject"}}
				if *perm == "always" || *perm == "once" {
					options = append(options, object{"kind": "allow_once", "optionId": "o", "name": "Once"})
				}
				if *perm == "always" {
					options = append(options, object{"kind": "allow_always", "optionId": "a", "name": "Always"})
				}
				write(object{"id": "permission-1", "method": "session/request_permission", "params": object{"sessionId": id, "toolCall": object{"toolCallId": "fixture"}, "options": options}})
				continue
			}
			switch *behavior {
			case "max_tokens", "max_turn_requests", "refusal", "cancelled":
				end(*behavior)
			default:
				end("end_turn")
			}
			continue
		case "session/cancel":
			if *behavior != "ignore cancel" && promptID != nil {
				end("cancelled")
			}
			continue
		default:
			if len(m.ID) > 0 {
				write(object{"id": m.ID, "error": object{"code": -32601, "message": "unknown method"}})
			}
			continue
		}
		write(object{"id": m.ID, "result": result})
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, time.Now(), err)
		os.Exit(1)
	}
}

func waitForGate(path string) {
	for path != "" {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
}
