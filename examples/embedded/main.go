// embedded 演示宿主通过公开 Go API 运行对话，无需模型密钥、YAML 或独立服务。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/storage"
	storagesqlite "github.com/VaalaCat/openvaala/pkg/storage/sqlite"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// main 运行嵌入示例并报告失败。
func main() {
	// 1. 限定示例运行时长，输出最终结果。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := run(ctx, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run 装配宿主依赖，运行工具对话，关闭重开后读取持久结果。
func run(ctx context.Context, output io.Writer) error {
	// 1. 宿主持有存储和工作目录，Runtime 退出后再关闭它们。
	directory, err := os.MkdirTemp("", "openvaala-embedded-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	database, err := storagesqlite.OpenDatabase(storagesqlite.NewMemoryDatabaseOpt(dao.RecordNamespaces()...))
	if err != nil {
		return err
	}
	defer database.Close()
	model := defs.ModelRef{ProviderID: "host", ModelID: "demo"}
	definitions := &runtime.Definitions{
		Agents: []*agent.AgentDefinition{{Name: "assistant", Model: model, Instructions: "使用宿主工具回答。"}},
		Models: []*defs.ModelDefinition{{Ref: model, Target: &llmkit.Target{Protocol: llmkit.ProtocolOpenAIChat, Model: "demo"}}},
		Executors: executor.NewConfigExecutorProvider([]executor.Executor{
			executor.NewLocalExecutor(&defs.ExecutorDefinition{ID: "local", Kind: defs.ExecutorKindLocal, WorkspaceDirectory: directory}),
		}),
	}
	rt, err := runtime.NewRuntime(runtime.NewRuntimeOpt(database.Record(), database.Log(), definitions).WithModelClient(demoModelClient{}))
	if err != nil {
		return err
	}
	// 2. 启动 Runtime，所有返回路径都先取消并等 Run 结束。
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- rt.Run(runCtx) }()
	defer func() { cancel(); <-done }()
	record, err := rt.CreateConversation(ctx, &runtime.CreateConversationRequest{AgentName: "assistant"})
	if err != nil {
		return err
	}
	if err := rt.SetClientExecutor(ctx, &runtime.SetClientExecutorRequest{
		ConversationID: record.ID,
		ConnectionID:   "host",
		ExecutorID:     "host",
		Servers:        []conversation.ClientMCPServer{{ID: "business", Transport: businessTransport{}}},
	}); err != nil {
		return err
	}
	// 3. 先订阅再提交输入，等这一轮完成。
	subscription, err := rt.SubscribeSession(ctx, &runtime.SubscribeSessionRequest{ConversationID: record.ID, SessionID: record.RootSessionID})
	if err != nil {
		return err
	}
	if _, err := rt.SubmitInput(ctx, &runtime.SubmitInputRequest{
		ConversationID: record.ID, Mode: session.SessionInputModeNormal,
		From:    &defs.Address{Kind: defs.IdentityUser, ID: "demo-user"},
		Content: []*defs.Content{{Kind: defs.ContentText, Text: &defs.TextContent{Text: "查询项目状态"}}},
	}); err != nil {
		return err
	}
	var turnID defs.TurnID
	for turnID == "" {
		select {
		case event, ok := <-subscription.Events:
			if !ok {
				return fmt.Errorf("conversation stopped before reply: %w", subscription.GetCloseReason())
			}
			if wrapped, ok := event.(*conversation.ConversationSessionEvent); ok {
				if finished, ok := wrapped.Event.(*session.SessionTurnFinished); ok {
					if finished.Err != nil {
						return finished.Err
					}
					turnID = finished.TurnID
				}
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// 4. 关闭运行实例后重开，读取原历史与单轮用量；MCP 若需继续执行须重新登记。
	if err := rt.CloseConversation(ctx, &runtime.CloseConversationRequest{ConversationID: record.ID}); err != nil {
		return err
	}
	if _, err := rt.OpenConversation(ctx, &runtime.OpenConversationRequest{ConversationID: record.ID}); err != nil {
		return err
	}
	items, err := rt.ListItems(ctx, &runtime.ListItemsRequest{ConversationID: record.ID, SessionID: record.RootSessionID, Order: storage.DirectionAsc, Limit: 100})
	if err != nil {
		return err
	}
	for _, logged := range items.Items {
		if logged.Item.Kind == defs.ItemMessage && logged.Item.Message.From.Kind == defs.IdentityAgent {
			for _, content := range logged.Item.Message.Content {
				if content.Text != nil {
					fmt.Fprintln(output, content.Text.Text)
				}
			}
		}
	}
	turns, err := rt.GetTurns(ctx, &runtime.GetTurnsRequest{ConversationID: record.ID, SessionID: record.RootSessionID, TurnIDs: []defs.TurnID{turnID}})
	if err != nil {
		return err
	}
	if len(turns) != 1 {
		return errors.New("saved turn missing")
	}
	fmt.Fprintf(output, "status=%s usage=%+v\n", turns[0].Status, turns[0].Usage)
	return rt.CloseConversation(ctx, &runtime.CloseConversationRequest{ConversationID: record.ID})
}

// demoModelClient 根据工具结果生成确定性回复；实际宿主可替换为网关客户端。
type demoModelClient struct{}

// Call 先请求业务工具，读取工具结果后输出最终回复。
func (demoModelClient) Call(ctx context.Context, request llmkit.Request, target llmkit.Target, _ llmkit.CallOptions) (<-chan llmkit.Event, error) {
	// 1. 检查取消与上一条工具结果，不保存跨对话状态。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	last := request.Messages[len(request.Messages)-1]
	events := make(chan llmkit.Event, 3)
	finish := "tool_calls"
	if last.Role == llmkit.RoleTool {
		var text strings.Builder
		for _, content := range last.Content {
			text.WriteString(content.Text)
		}
		events <- llmkit.Event{Type: llmkit.EventContentDelta, Delta: &llmkit.DeltaPayload{Text: "宿主工具结果：" + text.String()}}
		finish = "stop"
	} else {
		events <- llmkit.Event{Type: llmkit.EventToolCallEnd, ToolCall: &llmkit.StreamingToolCall{CallID: "project-status", Name: "mcp__business__status", Arguments: "{}"}}
	}
	events <- llmkit.Event{Type: llmkit.EventDone, Model: target.Model, FinishReason: finish, Usage: &llmkit.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}}
	close(events)
	return events, nil
}

// businessTransport 将宿主业务工具接到进程内 MCP。
type businessTransport struct{}

// Connect 创建提供项目状态查询的进程内连接。
func (businessTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	// 1. 注册宿主业务工具，用内存传输连接客户端和服务端。
	server := mcp.NewServer(&mcp.Implementation{Name: "business", Version: "1.0"}, nil)
	server.AddTool(&mcp.Tool{Name: "status", Description: "查询项目状态", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "项目状态：就绪"}}}, nil
		})
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
		return nil, err
	}
	return clientTransport.Connect(ctx)
}
