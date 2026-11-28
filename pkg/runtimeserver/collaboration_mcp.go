package runtimeserver

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/VaalaCat/openvaala/pkg/sessiontoken"
	"github.com/VaalaCat/openvaala/pkg/skill"
	"github.com/gin-gonic/gin"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// handleCollaborationMCP 为本次请求的签名身份装配工具，工具表及调用共用该会话的权限与工作位置。
func (s *runtimeServerImpl) handleCollaborationMCP(c *gin.Context) {
	cid, sid, ok := sessiontoken.Verify(s.opt.TokenSecret.GetToken(), getRequestToken(c))
	if !ok {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	ctx := c.Request.Context()
	instance, err := s.opt.Runtime.GetCollaborationConversation(ctx, cid)
	if err != nil {
		c.String(http.StatusNotFound, "%s", err)
		return
	}
	tools, err := instance.GetCollaborationTools(ctx, sid)
	if err != nil {
		c.String(http.StatusNotFound, "%s", err)
		return
	}
	definitions, err := tools.GetTools(ctx)
	if err != nil {
		c.String(http.StatusInternalServerError, "%s", err)
		return
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "openvaala", Version: "1"}, nil)
	for _, definition := range definitions {
		server.AddTool(&mcp.Tool{Name: definition.Name, Description: definition.Description, InputSchema: definition.InputSchema}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			finished := runCollaborationTool(ctx, tools, definition.Name, req.Params.Arguments)
			text := string(finished.Content)
			if failed := finished.Error; failed != nil {
				text = failed.Message
			}
			result := &mcp.CallToolResult{
				IsError: finished.Error != nil,
				Content: []mcp.Content{&mcp.TextContent{Text: text}},
			}
			for _, image := range finished.Images {
				mimeType, data, ok := image.GetDataURLParts()
				if !ok {
					continue
				}
				if decoded, err := base64.StdEncoding.DecodeString(data); err == nil {
					result.Content = append(result.Content, &mcp.ImageContent{MIMEType: mimeType, Data: decoded})
				}
			}
			return result, nil
		})
	}
	server.AddTool(&mcp.Tool{Name: "list_agent_definitions", Description: "列出可选 Agent 的名称、类型、说明和默认模型。", InputSchema: json.RawMessage(`{"type":"object","properties":{}}`)}, func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		agents, err := s.opt.Runtime.ListAgents(ctx)
		if err != nil {
			return nil, err
		}
		data, err := json.Marshal(runtimeprotocol.ListAgentsResult{Agents: agents})
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}}, err
	})
	// 定义级查询复用目录缓存，会话级查询和修改都先核对后代关系。
	checkDescendant := func(ctx context.Context, id defs.SessionID) error {
		allowed, err := s.opt.Runtime.IsSessionDescendant(ctx, cid, sid, id)
		if err != nil {
			return err
		}
		if !allowed {
			return collaborationPermissionError(ctx, s.opt.Runtime, cid, sid)
		}
		return nil
	}
	mcp.AddTool(server, &mcp.Tool{Name: "get_agent_options", Description: "按 agent_name 查定义选项，或按 session_id 查后代会话的实际选项。"}, func(ctx context.Context, _ *mcp.CallToolRequest, p struct {
		AgentName string         `json:"agent_name,omitempty"`
		SessionID defs.SessionID `json:"session_id,omitempty"`
	}) (*mcp.CallToolResult, defs.ExternalOptions, error) {
		if (p.AgentName == "") == (p.SessionID == "") {
			return nil, defs.ExternalOptions{}, errors.New("specify agent_name or session_id")
		}
		if p.SessionID == "" {
			opts, err := s.opt.Runtime.GetAgentOptions(ctx, runtime.AgentOptionsRequest{ExternalAgentLocation: runtime.ExternalAgentLocation{AgentName: p.AgentName}})
			return nil, opts, err
		}
		if err := checkDescendant(ctx, p.SessionID); err != nil {
			return nil, defs.ExternalOptions{}, err
		}
		opts, err := instance.GetExternalOptions(ctx, p.SessionID)
		return nil, opts, err
	})
	mcp.AddTool(server, &mcp.Tool{Name: "set_agent_option", Description: "修改指定后代外部会话的选项。"}, func(ctx context.Context, _ *mcp.CallToolRequest, p struct {
		SessionID defs.SessionID `json:"session_id"`
		defs.ExternalOptionSelection
	}) (*mcp.CallToolResult, any, error) {
		if err := checkDescendant(ctx, p.SessionID); err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{}, nil, instance.SetExternalOption(ctx, p.SessionID, p.ExternalOptionSelection)
	})
	mcp.AddTool(server, &mcp.Tool{Name: "reload_agent", Description: "按当前定义重新启动指定后代的外部驱动。"}, func(ctx context.Context, _ *mcp.CallToolRequest, p struct {
		SessionID defs.SessionID `json:"session_id"`
	}) (*mcp.CallToolResult, any, error) {
		if err := checkDescendant(ctx, p.SessionID); err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{}, nil, instance.ReloadExternalSession(ctx, p.SessionID)
	})
	registerTerminalMCPTools(server, s.opt.Runtime, &sessionCaller{conversationID: cid, sessionID: sid})
	registerSkillMCPTools(server, s.opt.Runtime, &sessionCaller{conversationID: cid, sessionID: sid})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	handler.ServeHTTP(c.Writer, c.Request)
}

// runCollaborationTool 执行来源会话已装配的工具，收集结果后返回。
func runCollaborationTool(ctx context.Context, tools executor.ToolExecutor, name string, arguments json.RawMessage) *defs.ToolResult {
	id := defs.NewItemID()
	call := &defs.Item{ID: id, Kind: defs.ItemToolCall, ToolCall: &defs.ToolCall{
		CallID: string(id), Name: name, Arguments: cmp.Or(string(arguments), "{}"),
	}}
	events := make(chan executor.ExecutorEvent)
	go func() {
		defer close(events)
		prepared := tools.PrepareCalls(ctx, []*defs.Item{call}, events)
		tools.Run(ctx, prepared[0], events)
	}()
	var result *defs.ToolResult
	for event := range events {
		if finished, ok := event.(*executor.ExecutionFinishedEvent); ok {
			result = finished.Result
		}
	}
	return result
}

// registerTerminalMCPTools 装配令牌所属对话的终端操作。
func registerTerminalMCPTools(server *mcp.Server, rt runtime.Runtime, caller *sessionCaller) {
	addCollaborationMCPTool(server, "terminal_list", "列出本对话正在运行的终端。",
		func(ctx context.Context, _ runtimeprotocol.EmptyParams) (*runtimeprotocol.TerminalListResult, error) {
			subCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			subscription, err := rt.SubscribeTerminals(subCtx)
			if err != nil {
				return nil, err
			}
			return &runtimeprotocol.TerminalListResult{Terminals: getScopedTerminalItems(subscription.Initial, caller)}, nil
		})
	addCollaborationMCPTool(server, "terminal_open", "打开本对话的交互终端。",
		func(ctx context.Context, params terminalOpenToolParams) (*runtimeprotocol.TerminalOpenResult, error) {
			opened, err := openConversationTerminal(ctx, rt, caller, &runtimeprotocol.TerminalOpenParams{
				ExecutorID: params.ExecutorID, Directory: params.Directory, Rows: params.Rows, Cols: params.Cols,
			})
			if err != nil {
				return nil, err
			}
			return &runtimeprotocol.TerminalOpenResult{
				TerminalID: opened.Terminal.GetInfo().ID, ExecutorID: opened.ExecutorID, Directory: opened.Directory,
			}, nil
		})
	addCollaborationMCPTool(server, "terminal_close", "结束本对话的指定终端。",
		func(ctx context.Context, params runtimeprotocol.TerminalParams) (*runtimeprotocol.EmptyResult, error) {
			return closeConversationTerminal(ctx, rt, caller, params.TerminalID)
		})
	addCollaborationMCPTool(server, "terminal_send_keys", "向指定终端写入按键文本，换行提交命令。",
		func(ctx context.Context, params terminalSendKeysToolParams) (*runtimeprotocol.EmptyResult, error) {
			return writeConversationTerminalInput(ctx, rt, caller, params.TerminalID, params.Keys)
		})
	addCollaborationMCPTool(server, "terminal_capture", "读取最近若干行终端输出，去除 ANSI 控制序列。",
		func(ctx context.Context, params runtimeprotocol.TerminalCaptureParams) (string, error) {
			return captureTerminal(ctx, rt, caller, &params)
		})
}

// registerSkillMCPTools 装配令牌绑定会话的 Skill 浏览和读取。
func registerSkillMCPTools(server *mcp.Server, rt runtime.Runtime, caller *sessionCaller) {
	addCollaborationMCPTool(server, "list_skills", "列出本会话可用的 Skill。",
		func(ctx context.Context, _ runtimeprotocol.EmptyParams) ([]skill.SkillInfo, error) {
			return rt.GetSkills(ctx, getSkillRequest(caller, runtimeprotocol.ListSkillsParams{}))
		})
	addCollaborationMCPTool(server, "search_skills", "按名称和说明搜索可用的 Skill。",
		func(ctx context.Context, params searchSkillsToolParams) ([]skill.SkillInfo, error) {
			return rt.GetSkills(ctx, getSkillRequest(caller, runtimeprotocol.ListSkillsParams{Query: params.Query}))
		})
	addCollaborationMCPTool(server, "get_skill", "读取指定 Skill 的正文或目录中的文件。",
		func(ctx context.Context, params getSkillToolParams) (string, error) {
			result, err := getSkill(ctx, rt, caller, &runtimeprotocol.GetSkillParams{Name: params.Name, File: params.File})
			if err != nil {
				return "", err
			}
			return result.Content, nil
		})
}

// addCollaborationMCPTool 使用 MCP 参数校验并将返回值编码为文本。
func addCollaborationMCPTool[In, Out any](server *mcp.Server, name, description string, call func(context.Context, In) (Out, error)) {
	mcp.AddTool(server, &mcp.Tool{Name: name, Description: description},
		func(ctx context.Context, _ *mcp.CallToolRequest, params In) (*mcp.CallToolResult, any, error) {
			value, err := call(ctx, params)
			if err != nil {
				return nil, nil, err
			}
			text, isText := any(value).(string)
			if !isText {
				data, err := json.Marshal(value)
				if err != nil {
					return nil, nil, err
				}
				text = string(data)
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
		})
}
