package runtimeserver_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/VaalaCat/openvaala/pkg/sessiontoken"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestExternalCollaborationMCP 经 HTTP 检查会话身份、工具发现、协作调用和错误回传。
func TestExternalCollaborationMCP(t *testing.T) {
	hostMCP := mcp.NewServer(&mcp.Implementation{Name: "host-fixture", Version: "1"}, nil)
	hostMCP.AddTool(&mcp.Tool{
		Name: "host_fixture", InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	})
	host := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return hostMCP }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}))
	t.Cleanup(host.Close)
	h := startTestServer(t)
	definitions := newTestRuntimeOpt(h.store, h.model, h.defaultDirectory).Definitions
	definitions.MCPServers = []*mcpclient.ServerDefinition{{
		ID: "host-fixture", Type: mcpclient.ServerTransportHTTP, URL: host.URL, ExecutorIDs: []string{"local"},
		Required: true, StartupTimeout: 5 * time.Second, ToolTimeout: 5 * time.Second,
	}}
	if _, err := h.runtime.SetDefinitions(t.Context(), definitions); err != nil {
		t.Fatal(err)
	}
	operator := h.dial(t)
	operator.initialize(t)
	record := operator.createConversation(t)
	instance, err := h.runtime.GetCollaborationConversation(t.Context(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !slices.ContainsFunc(instance.GetMCPServers(), func(server conversation.MCPServerInfo) bool {
		return server.ServerID == "host-fixture" && server.State == mcpclient.ConnectionStateConnected
	}) {
		if time.Now().After(deadline) {
			t.Fatal("host fixture never connected")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.WriteFile(filepath.Join(h.defaultDirectory, "mcp-fixture.txt"), []byte("fixture-body"), 0600); err != nil {
		t.Fatal(err)
	}
	const imageData = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+/lpoAAAAASUVORK5CYII="
	image, err := base64.StdEncoding.DecodeString(imageData)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.defaultDirectory, "mcp-fixture.png"), image, 0600); err != nil {
		t.Fatal(err)
	}
	token := sessiontoken.Issue(testToken, record.ID, record.RootSessionID)
	type mcpResponse struct {
		Error  json.RawMessage
		Result struct {
			Tools   []struct{ Name string }
			IsError bool
			Content []struct {
				Text     string
				Type     string
				MIMEType string `json:"mimeType"`
				Data     string
			}
		}
	}
	call := func(t *testing.T, bearer, method, params string, wantStatus int) mcpResponse {
		t.Helper()
		body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": json.RawMessage(params)})
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.httpServer.URL+conversation.CollaborationMCPPath, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := h.httpServer.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != wantStatus {
			t.Fatalf("HTTP %s", resp.Status)
		}
		if wantStatus != 200 {
			return mcpResponse{}
		}
		var rpc mcpResponse
		if err := json.NewDecoder(resp.Body).Decode(&rpc); err != nil || rpc.Error != nil {
			t.Fatalf("RPC: %s %v", rpc.Error, err)
		}
		return rpc
	}
	for _, tc := range []struct {
		name, bearer, method, params string
		wantStatus                   int
	}{
		{"missing identity", "", "tools/list", "{}", 401},
		{"operator token", testToken, "tools/list", "{}", 401},
		{"list tools", token, "tools/list", "{}", 200},
		{"list executor tools", token, "tools/call", `{"name":"list_executors"}`, 200},
		{"definitions", token, "tools/call", `{"name":"list_agent_definitions","arguments":{}}`, 200},
		{"descendants", token, "tools/call", `{"name":"list_agents","arguments":{}}`, 200},
		{"omitted arguments", token, "tools/call", `{"name":"list_agents"}`, 200},
		{"workspace file", token, "tools/call", `{"name":"read_file","arguments":{"executor":"local","path":"mcp-fixture.txt"}}`, 200},
		{"workspace image", token, "tools/call", `{"name":"read_file","arguments":{"executor":"local","path":"mcp-fixture.png"}}`, 200},
		{"workspace command", token, "tools/call", `{"name":"bash","arguments":{"executor":"local","command":"printf fixture-body","wait_seconds":5}}`, 200},
		{"history outside subtree", token, "tools/call", `{"name":"read_session_history","arguments":{"session_id":"outside"}}`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rpc := call(t, tc.bearer, tc.method, tc.params, tc.wantStatus)
			if tc.wantStatus != http.StatusOK {
				return
			}
			if tc.method == "tools/list" {
				var mcpToolNames []string
				for _, d := range rpc.Result.Tools {
					mcpToolNames = append(mcpToolNames, d.Name)
				}
				for _, expected := range []string{"spawn_agent", "send_message", "interrupt_agent", "list_agents", "read_session_history", "wait_agent", "list_executors", "list_agent_definitions"} {
					if !slices.Contains(mcpToolNames, expected) {
						t.Fatalf("missing %s in %v", expected, mcpToolNames)
					}
				}
				if slices.ContainsFunc(mcpToolNames, func(name string) bool { return strings.Contains(name, "host_fixture") }) {
					t.Fatalf("host MCP tool advertised: %v", mcpToolNames)
				}
				return
			}
			if tc.name == "workspace image" {
				content := rpc.Result.Content
				if rpc.Result.IsError || len(content) != 2 || content[0].Type != "text" ||
					content[1].Type != "image" || content[1].MIMEType != "image/png" || content[1].Data != imageData {
					t.Fatalf("image result = %+v", rpc.Result)
				}
				return
			}
			if len(rpc.Result.Content) != 1 || rpc.Result.IsError != (tc.name == "history outside subtree") {
				t.Fatalf("tool result %+v", rpc.Result)
			}
			switch tc.name {
			case "list executor tools":
				var listed struct {
					Executors []struct {
						ID    string
						Tools []string
					}
				}
				if err := json.Unmarshal([]byte(rpc.Result.Content[0].Text), &listed); err != nil {
					t.Fatal(err)
				}
				local := slices.IndexFunc(listed.Executors, func(executor struct {
					ID    string
					Tools []string
				}) bool {
					return executor.ID == "local"
				})
				if local < 0 || !slices.Contains(listed.Executors[local].Tools, "read_file") || !slices.Contains(listed.Executors[local].Tools, "bash") {
					t.Fatalf("local tools missing: %+v", listed.Executors)
				}
				advertised := call(t, tc.bearer, "tools/list", "{}", http.StatusOK)
				for _, name := range listed.Executors[local].Tools {
					if !slices.ContainsFunc(advertised.Result.Tools, func(tool struct{ Name string }) bool { return tool.Name == name }) {
						t.Fatalf("list_executors advertised %s outside tools/list", name)
					}
				}
			case "workspace file", "workspace command":
				if !strings.Contains(rpc.Result.Content[0].Text, "fixture-body") {
					t.Fatalf("fixture content: %s", rpc.Result.Content[0].Text)
				}
			case "definitions":
				var defs runtimeprotocol.ListAgentsResult
				if err := json.Unmarshal([]byte(rpc.Result.Content[0].Text), &defs); err != nil || len(defs.Agents) == 0 || defs.Agents[0].Type == "" {
					t.Fatalf("definitions %+v %v", defs, err)
				}
			case "descendants", "omitted arguments":
				var result conversation.ListAgentsResult
				if err := json.Unmarshal([]byte(rpc.Result.Content[0].Text), &result); err != nil || result.AppliedFilter.Limit != 20 || len(result.Agents) != 0 {
					t.Fatalf("descendants %+v %v", result, err)
				}
			}
		})
	}
}

// callSessionMCPTool 经过 HTTP 使用会话令牌调用本地 MCP 工具。
func callSessionMCPTool(t *testing.T, h *serverHarness, token, name string, args any) mcp.CallToolResult {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.httpServer.URL+conversation.CollaborationMCPPath, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := h.httpServer.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var rpc struct {
		Error  json.RawMessage
		Result mcp.CallToolResult
	}
	if err := json.NewDecoder(resp.Body).Decode(&rpc); err != nil || resp.StatusCode != http.StatusOK || rpc.Error != nil {
		t.Fatalf("MCP %s: HTTP %d, RPC %s, decode %v", name, resp.StatusCode, rpc.Error, err)
	}
	return rpc.Result
}

func TestSessionTerminalTools(t *testing.T) {
	t.Setenv("SHELL", "/bin/bash")
	h := startTestServer(t)
	operator := h.dial(t)
	operator.initialize(t)
	own, other := operator.createConversation(t), operator.createConversation(t)
	foreign := openTerminal(t, operator, map[string]any{"conversation_id": other.ID})
	token := sessiontoken.Issue(testToken, own.ID, own.RootSessionID)
	conn, _, err := h.dialWith(t, "/api/v1/openvaala", http.Header{"Authorization": {"Bearer " + token}})
	if err != nil {
		t.Fatal(err)
	}
	scoped := newTestClient(t, conn)
	scoped.initialize(t)
	for _, tc := range []struct {
		method string
		args   map[string]any
	}{
		{"terminal/open", map[string]any{"conversation_id": other.ID}},
		{"terminal/attach", map[string]any{"terminal_id": foreign.TerminalID}},
		{"terminal/input", map[string]any{"terminal_id": foreign.TerminalID, "data": []byte("echo forbidden\n")}},
		{"terminal/close", map[string]any{"terminal_id": foreign.TerminalID}},
		{"terminal/capture", map[string]any{"terminal_id": foreign.TerminalID}},
	} {
		t.Run(tc.method, func(t *testing.T) {
			res := scoped.call(t, tc.method, tc.args)
			if res.Error == nil || res.Error.Message != "permission denied" {
				t.Fatalf("RPC error = %+v", res.Error)
			}
		})
	}
	call := func(tool string, args any) string {
		t.Helper()
		result := callSessionMCPTool(t, h, token, tool, args)
		if result.IsError || len(result.Content) != 1 {
			t.Fatalf("%s = %+v", tool, result)
		}
		return result.Content[0].(*mcp.TextContent).Text
	}
	var opened runtimeprotocol.TerminalOpenResult
	if err := json.Unmarshal([]byte(call("terminal_open", map[string]any{})), &opened); err != nil || opened.TerminalID == "" {
		t.Fatalf("terminal_open = %+v, %v", opened, err)
	}
	for _, tc := range []struct {
		name   string
		notify bool
	}{
		{name: "request without attachment"},
		{name: "notification on attachment", notify: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.notify {
				scoped.callResult(t, "terminal/attach", &runtimeprotocol.TerminalAttachParams{TerminalID: opened.TerminalID}, nil)
			}
			keys := "printf '%s%s\\n' input -written\n"
			if tc.notify {
				scoped.writeTerminal(t, opened.TerminalID, keys)
				scoped.waitTerminalOutput(t, opened.TerminalID, "input-written")
				return
			}
			scoped.callResult(t, "terminal/input", &runtimeprotocol.TerminalInputParams{
				TerminalID: opened.TerminalID, Data: []byte("echo request-completed\n"),
			}, nil)
		})
	}
	var terminals runtimeprotocol.TerminalListResult
	scoped.callResult(t, "terminal/subscribe", nil, &terminals)
	if len(terminals.Terminals) != 1 || terminals.Terminals[0].TerminalID != opened.TerminalID {
		t.Fatalf("scoped terminals = %+v", terminals)
	}
	if list := call("terminal_list", map[string]any{}); !strings.Contains(list, opened.TerminalID) || strings.Contains(list, foreign.TerminalID) {
		t.Fatalf("MCP terminals = %s", list)
	}
	for _, tool := range []string{"terminal_close", "terminal_capture", "terminal_send_keys"} {
		t.Run(tool, func(t *testing.T) {
			args := map[string]any{"terminal_id": foreign.TerminalID}
			if tool == "terminal_send_keys" {
				args["keys"] = "echo forbidden\n"
			}
			result := callSessionMCPTool(t, h, token, tool, args)
			if !result.IsError || len(result.Content) != 1 || result.Content[0].(*mcp.TextContent).Text != "permission denied" {
				t.Fatalf("foreign %s = %+v", tool, result)
			}
		})
	}
	call("terminal_send_keys", map[string]any{"terminal_id": opened.TerminalID, "keys": "printf '\\033[31m%s\\033[0m\\n' first second third; read -r\n"})
	deadline := time.Now().Add(5 * time.Second)
	for {
		captured := call("terminal_capture", map[string]any{"terminal_id": opened.TerminalID, "lines": 2})
		if strings.Contains(captured, "second\nthird") {
			if strings.Contains(captured, "first") || strings.ContainsRune(captured, '\x1b') {
				t.Fatalf("capture = %q", captured)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("capture = %q, want recent plain text", captured)
		}
		time.Sleep(10 * time.Millisecond)
	}
	call("terminal_close", map[string]any{"terminal_id": opened.TerminalID})
	var status runtimeprotocol.TerminalListResult
	scoped.callResult(t, "terminal/unsubscribe", nil, nil)
	scoped.callResult(t, "terminal/subscribe", nil, &status)
	if len(status.Terminals) != 0 {
		t.Fatalf("closed terminal still listed: %+v", status)
	}
}
