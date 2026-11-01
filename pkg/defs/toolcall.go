package defs

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/pkg/utils/jsonutil"
)

// ToolDefinition 是一个工具的名称、描述和参数 schema。
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	// MCPServerID 是提供这个工具的 MCP server；内置工具和协作工具为空。不发给模型。
	MCPServerID string `json:"mcp_server_id,omitempty"`
}

// ToLLMKit 把工具定义转换为发给模型的 llmkit 工具。
func (tool *ToolDefinition) ToLLMKit() *llmkit.Tool {
	// 1. 复制工具名、描述和参数 schema，不包含 MCPServerID。
	return &llmkit.Tool{
		Type:        "function",
		Name:        tool.Name,
		Description: tool.Description,
		InputSchema: slices.Clone(tool.InputSchema),
	}
}

// LocalExecutorID 是 OpenVaala 自带的本机 Executor 的 ID；配置文件里不能用这个 ID。
const LocalExecutorID = "local"

// ClientExecutorID 是对话里客户端登记的 MCP server 所属的虚拟 Executor 的 ID；配置文件里不能用这个 ID。
const ClientExecutorID = "client"

// ToolError 是工具执行失败时的错误码和消息。
type ToolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ToolResult 是一次工具调用的结果，用 CallID 对应模型发出的那次调用。
type ToolResult struct {
	CallID string `json:"call_id,omitempty"`
	// Executor 是实际执行这次调用的 Executor；Agent 侧工具（spawn_agent 等）和没能分派的调用为空。
	Executor string          `json:"executor,omitempty"`
	Content  json.RawMessage `json:"content,omitempty"`
	// Images 是工具读到、要给模型看的图片；Alt 写模型传入的路径。发给模型时放进这串工具结果之后的一条用户消息。
	Images []*ImageContent `json:"images,omitempty"`
	Error  *ToolError      `json:"error,omitempty"`
	// Truncated 工具输出是否超长被截断了？
	Truncated bool `json:"truncated,omitempty"`
}

// ToolCallStatus 是工具调用参数已经完整还是被中断。
type ToolCallStatus string

const (
	ToolCallReady       ToolCallStatus = "ready"
	ToolCallInterrupted ToolCallStatus = "interrupted"
)

// ToolCall 是模型发出的一次工具调用及参数。
type ToolCall struct {
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	// Status 为空时参数还在生成。
	Status ToolCallStatus `json:"status,omitempty"`
}

// ToLLMKit 将工具调用转换为 llmkit 的工具调用。
func (call *ToolCall) ToLLMKit() *llmkit.ToolCall {
	// 1. 复制 CallID、工具名和完整参数。
	return &llmkit.ToolCall{
		ID:        call.CallID,
		Name:      call.Name,
		Arguments: call.Arguments,
	}
}

// FromLLMKit 读取模型发出的完整工具调用。
func (call *ToolCall) FromLLMKit(modelCall *llmkit.ToolCall) {
	// 1. 复制调用 ID、工具名和参数，将状态设为 ready。
	*call = ToolCall{
		CallID:    modelCall.ID,
		Name:      modelCall.Name,
		Arguments: modelCall.Arguments,
		Status:    ToolCallReady,
	}
}

// GetModelContent 将工具结果编码为模型可见的 JSON 正文。
func (result *ToolResult) GetModelContent() ([]byte, error) {
	// 1. 复制结果，去掉调用 ID、Executor 和图片后编码，保留错误及截断信息。
	body := result.Clone()
	body.CallID = ""
	body.Executor = ""
	body.Images = nil
	return jsonutil.MarshalWithoutHTMLEscape(body)
}

// ToLLMKit 把工具结果转换为发给模型的 tool 消息。
func (result *ToolResult) ToLLMKit() (*llmkit.Message, error) {
	// 1. 要求结果带 CallID，再编码模型可见正文。
	if result == nil || result.CallID == "" {
		return nil, fmt.Errorf("tool result requires a call ID")
	}
	content, err := result.GetModelContent()
	if err != nil {
		return nil, fmt.Errorf("encode tool result: %w", err)
	}
	// 2. 组装 tool 消息，将 CallID 放在消息字段中。
	return &llmkit.Message{
		Role:       llmkit.RoleTool,
		ToolCallID: result.CallID,
		Content: []llmkit.ContentBlock{
			{
				Type: llmkit.ContentTypeText,
				Text: string(content),
			},
		},
	}, nil
}

// FromLLMKit 从 tool 消息解码 OpenVaala 自己编码的工具结果 JSON。
func (result *ToolResult) FromLLMKit(message *llmkit.Message) error {
	// 1. 检查 tool 角色和 CallID，要求仅有一个 JSON 对象文本块。
	if message == nil || message.Role != llmkit.RoleTool || message.ToolCallID == "" || len(message.ToolCalls) != 0 || len(message.RawJSON) != 0 || len(message.Content) != 1 {
		return fmt.Errorf("invalid tool result message")
	}
	block := &message.Content[0]
	if block.Type != llmkit.ContentTypeText || len(block.RawJSON) != 0 || !json.Valid([]byte(block.Text)) || !strings.HasPrefix(strings.TrimSpace(block.Text), "{") {
		return fmt.Errorf("tool result requires a JSON object text block")
	}
	// 2. 解码已声明字段，拒绝未知字段和正文内的 CallID。
	decoded := &ToolResult{}
	decoder := json.NewDecoder(strings.NewReader(block.Text))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(decoded); err != nil {
		return fmt.Errorf("decode tool result: %w", err)
	}
	if decoded.CallID != "" {
		return fmt.Errorf("tool result call ID must be supplied by the message, not its content")
	}
	// 3. 解码成功后从消息字段补上 CallID，再替换结果。
	decoded.CallID = message.ToolCallID
	*result = *decoded
	return nil
}
