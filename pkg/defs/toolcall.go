package defs

import (
	"encoding/json"
	"errors"
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

// MemoryExecutorID 是内置的记忆 Executor 的 ID；配置文件里不能用这个 ID。
const MemoryExecutorID = "memory"

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

// ToolErrorMaxStepsReached 是最后一步的工具拒绝错误码。
const ToolErrorMaxStepsReached = "max_steps_reached"

// ToolErrorHookStopped 是前置工具 hook 要求结束本轮时写入的错误码。
const ToolErrorHookStopped = "hook_stopped"

// ErrMaxStepsToolRequested 表示最后一步禁用了工具，模型仍然请求工具。
var ErrMaxStepsToolRequested = errors.New("agent reached max steps but model still requested tools")

// NewRejectedToolResult 为达到步数上限的调用创建拒绝结果。
func NewRejectedToolResult(callID string) *ToolResult {
	// 1. 使用原调用 ID 和步数上限错误。
	return &ToolResult{CallID: callID, Error: &ToolError{Code: ToolErrorMaxStepsReached, Message: ErrMaxStepsToolRequested.Error()}}
}

// unknownToolResultContent 是恢复时写入的未知结果正文。
const unknownToolResultContent = `{"code":"outcome_unknown","message":"执行结果未知，请先检查现场，不要直接重复原操作"}`

// NewUnknownToolResult 为已放行但缺少结果的调用创建未知结果。
func NewUnknownToolResult(callID string) *ToolResult {
	// 1. 使用原调用 ID，正文提示检查现场，不设置工具错误或执行器。
	return &ToolResult{CallID: callID, Content: json.RawMessage(unknownToolResultContent)}
}

// IsOutcomeUnknown 这条结果是否由 Turn 在核对已放行的工具批时补写？
func (result *ToolResult) IsOutcomeUnknown() bool {
	// 1. 检查恢复专用正文；实际执行环境和工具错误均为空。
	return result.Error == nil && result.Executor == "" && string(result.Content) == unknownToolResultContent
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
	// ProviderData 是模型厂商附在这次调用上的签名等数据；下一次请求原样还给 llmkit，否则 Gemini 等会拒绝或丢失签名。
	ProviderData *llmkit.ToolCallProviderData `json:"provider_data,omitempty"`
}

// ToLLMKit 将工具调用转换为 llmkit 的工具调用。
func (call *ToolCall) ToLLMKit() *llmkit.ToolCall {
	// 1. 复制 CallID、工具名、完整参数，以及深拷贝后的厂商数据。
	return &llmkit.ToolCall{
		ID:           call.CallID,
		Name:         call.Name,
		Arguments:    call.Arguments,
		ProviderData: CloneToolCallProviderData(call.ProviderData),
	}
}

// FromLLMKit 读取模型发出的完整工具调用。
func (call *ToolCall) FromLLMKit(modelCall *llmkit.ToolCall) {
	// 1. 复制调用 ID、工具名、参数和深拷贝后的厂商数据，将状态设为 ready。
	*call = ToolCall{
		CallID:       modelCall.ID,
		Name:         modelCall.Name,
		Arguments:    modelCall.Arguments,
		Status:       ToolCallReady,
		ProviderData: CloneToolCallProviderData(modelCall.ProviderData),
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
