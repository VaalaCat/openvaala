package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
	"github.com/VaalaCat/openvaala/pkg/prompt"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/workspace"
	schemavalidator "github.com/santhosh-tekuri/jsonschema/v6"
)

// dropUndeclaredArguments 删除 schema 未声明的顶层参数。
func dropUndeclaredArguments(schema *schemavalidator.Schema, document any) {
	// 1. 仅处理对象参数，删除未声明的字段。
	object, ok := document.(map[string]any)
	if !ok {
		return
	}
	for name := range object {
		if _, declared := schema.Properties[name]; !declared {
			delete(object, name)
		}
	}
}

// applyArgumentDefaults 按 schema 递归补齐缺失参数，保留已提供的值。
func applyArgumentDefaults(schema *schemavalidator.Schema, document any) {
	// 1. 跳过空参数，先应用引用和 allOf 中的默认值。
	if document == nil {
		return
	}
	if schema.Ref != nil {
		applyArgumentDefaults(schema.Ref, document)
	}
	for _, part := range schema.AllOf {
		applyArgumentDefaults(part, document)
	}
	// 2. 按对象属性、额外属性和数组元素递归补齐默认值。
	switch document := document.(type) {
	case map[string]any:
		for name, property := range schema.Properties {
			value, exists := document[name]
			if !exists && property.Default != nil {
				value = cloneArgumentDefault(*property.Default)
				document[name] = value
			}
			applyArgumentDefaults(property, value)
		}
		if property, ok := schema.AdditionalProperties.(*schemavalidator.Schema); ok {
			for name, value := range document {
				if _, declared := schema.Properties[name]; !declared {
					applyArgumentDefaults(property, value)
				}
			}
		}
	case []any:
		if schema.Items2020 != nil {
			for _, value := range document {
				applyArgumentDefaults(schema.Items2020, value)
			}
		}
	}
}

// cloneArgumentDefault 递归复制默认值中的对象和数组。
func cloneArgumentDefault(value any) any {
	// 1. 按值的类型复制对象、数组，其他值原样返回。
	switch value := value.(type) {
	case map[string]any:
		cloned := make(map[string]any, len(value))
		for key, entry := range value {
			cloned[key] = cloneArgumentDefault(entry)
		}
		return cloned
	case []any:
		cloned := make([]any, len(value))
		for index, entry := range value {
			cloned[index] = cloneArgumentDefault(entry)
		}
		return cloned
	default:
		return value
	}
}

// newToolErrorResult 创建失败的工具结果。
func newToolErrorResult(callID, code string, err error) *defs.ToolResult {
	// 1. 填入调用 ID、错误码和错误文本，正文设为 JSON null。
	return &defs.ToolResult{
		CallID:  callID,
		Content: json.RawMessage("null"),
		Error: &defs.ToolError{
			Code:    code,
			Message: err.Error(),
		},
	}
}

// getToolErrorCode 提取工具结果中的错误码。
func getToolErrorCode(result *defs.ToolResult) string {
	// 1. 有错误时读取错误码，否则返回空字符串。
	if result.Error == nil {
		return ""
	}
	return result.Error.Code
}

// getToolResultContent 把模型收到的工具正文转成遥测内容。
func getToolResultContent(result *defs.ToolResult) any {
	// 1. 取得包含内容、错误和截断标记的正文，编码失败返回 nil，否则解码为遥测字段。
	encoded, err := result.GetModelContent()
	if err != nil {
		return nil
	}
	return telemetry.DecodeJSONContent(encoded)
}

// logUndispatchedCall 记录没有分发执行的工具调用。
func logUndispatchedCall(ctx context.Context, call *defs.Item, errorCode string, attrs ...any) {
	// 1. 补充工具身份和附加字段，写入 Warn 日志。
	ctx = telemetry.ContextWithAttrs(ctx, telemetry.ToolName(call.ToolCall.Name), telemetry.ToolCallID(call.ToolCall.CallID))
	fields := append([]any{"error.type", errorCode}, attrs...)
	telemetry.GetLogger(ctx).Warn("tool call not dispatched", fields...)
}

// executorArgument 是多个执行环境提供的同名工具上加入的参数名，模型用它选择执行环境。
const executorArgument = "executor"

// GetTurnExecutors 按环境顺序返回 Turn 记录所需的执行环境信息。
func GetTurnExecutors(environments []ExecutionEnvironment) []defs.TurnExecutor {
	// 1. 读取环境标识、说明和类型；有工作区时读取根目录。
	var turnExecutors []defs.TurnExecutor
	for _, environment := range environments {
		turnExecutor := defs.TurnExecutor{
			ID:          environment.GetID(),
			Description: environment.GetDescription(),
			IsDynamic:   environment.IsDynamic(),
		}
		if environmentWorkspace := environment.GetWorkspace(); environmentWorkspace != nil {
			turnExecutor.WorkspaceDirectory = environmentWorkspace.GetRootDirectory()
		}
		turnExecutors = append(turnExecutors, turnExecutor)
	}
	return turnExecutors
}

// takeExecutorArgument 从工具参数里取出 executor，返回环境 ID 和去掉 executor 后的参数。
func takeExecutorArgument(arguments string) (string, string, error) {
	// 1. 解码对象参数；不是 JSON 对象时原样返回。
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(arguments), &object); err != nil || object == nil {
		return "", arguments, nil
	}
	// 2. 读取 executor 字符串；没有 executor 时原样返回参数，不是字符串时报错。
	raw, found := object[executorArgument]
	if !found {
		return "", arguments, nil
	}
	var executorID string
	if err := json.Unmarshal(raw, &executorID); err != nil {
		return "", "", errors.New("executor must be a string")
	}
	// 3. 删除 executor，编码并返回剩余参数。
	delete(object, executorArgument)
	encoded, err := json.Marshal(object)
	if err != nil {
		return "", "", err
	}
	return executorID, string(encoded), nil
}

// validateToolDefinitions 校验工具名称、输入 schema 和组内重名。
func validateToolDefinitions(tools []Tool) error {
	// 1. 逐个检查工具及其定义，并记录已经出现的名称。
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		if tool == nil {
			return errors.New("executor tool is missing")
		}
		definition := tool.GetDefinition()
		if definition == nil || definition.Name == "" || !json.Valid(definition.InputSchema) {
			return errors.New("tool requires a name and valid JSON input schema")
		}
		if slices.Contains(names, definition.Name) {
			return fmt.Errorf("duplicate tool %q", definition.Name)
		}
		names = append(names, definition.Name)
	}
	return nil
}

// findRoute 在工具表里按工具名找到这个工具；没有时返回 nil。
func (r *toolRoutes) findRoute(name string) *toolRoute {
	// 1. 遍历工具表并匹配名称。
	for _, route := range r.routes {
		if route.definition.Name == name {
			return route
		}
	}
	return nil
}

// findProvider 查找指定环境的工具提供方；这个环境不提供时第二个返回值为 false。
func (r *toolRoute) findProvider(environmentID string) (toolProvider, bool) {
	// 1. 遍历提供方并匹配环境 ID。
	for _, provider := range r.providers {
		if provider.environment != nil && provider.environment.GetID() == environmentID {
			return provider, true
		}
	}
	return toolProvider{}, false
}

// isExecutorArgumentDeclared 原始 schema 是否声明了顶层 executor 字段？
func (r *toolRoute) isExecutorArgumentDeclared() bool {
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(r.definition.InputSchema, &schema); err != nil {
		return false
	}
	_, declared := schema.Properties[executorArgument]
	return declared || slices.Contains(schema.Required, executorArgument)
}

// hasDefaultProvider 默认环境是否提供这个工具？
func (r *toolRoutes) hasDefaultProvider(route *toolRoute) bool {
	return r.defaultID != "" && route.providers[0].environment.GetID() == r.defaultID
}

// getProviderDescriptions 按提供方顺序格式化环境 ID 和说明。
func (r *toolRoute) getProviderDescriptions() string {
	descriptions := make([]string, 0, len(r.providers))
	for _, provider := range r.providers {
		environment := provider.environment
		label := environment.GetID()
		if description := environment.GetDescription(); description != "" {
			label += " (" + description + ")"
		}
		descriptions = append(descriptions, label)
	}
	return strings.Join(descriptions, ", ")
}

// getDefinitionWithExecutorParameter 返回带 executor 路由参数的工具定义副本。
func (r *toolRoute) getDefinitionWithExecutorParameter(defaultID string, isRequired bool) (*defs.ToolDefinition, error) {
	// 1. 解码工具输入 schema，补齐 properties 对象。
	var schema map[string]any
	if err := json.Unmarshal(r.definition.InputSchema, &schema); err != nil {
		return nil, fmt.Errorf("decode tool %q schema: %w", r.definition.Name, err)
	}
	if schema == nil {
		return nil, fmt.Errorf("tool %q input schema must be an object", r.definition.Name)
	}
	properties, _ := schema["properties"].(map[string]any)
	if properties == nil {
		properties = make(map[string]any)
		schema["properties"] = properties
	}

	// 2. 写入环境枚举和参数说明；默认环境不提供工具时标记必填。
	providerIDs := make([]string, 0, len(r.providers))
	for _, provider := range r.providers {
		providerIDs = append(providerIDs, provider.environment.GetID())
	}
	description := prompt.FormatExecutorParameterDescription(defaultID)
	if isRequired {
		description = prompt.FormatRequiredExecutorParameterDescription()
		required, _ := schema["required"].([]any)
		if !slices.Contains(required, executorArgument) {
			schema["required"] = append(required, executorArgument)
		}
	}
	properties[executorArgument] = map[string]any{
		"type":        "string",
		"enum":        providerIDs,
		"description": description,
	}

	// 3. 编码 schema 并放入工具定义副本。
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(schema); err != nil {
		return nil, err
	}
	definition := r.definition.Clone()
	definition.InputSchema = bytes.TrimSpace(encoded.Bytes())
	return definition, nil
}

// GetExecutionOutputText 提取执行进度的文本。
func GetExecutionOutputText(content json.RawMessage) string {
	// 1. 尝试解码为字符串，失败时返回原始内容。
	var text string
	if json.Unmarshal(content, &text) == nil {
		return text
	}
	return string(content)
}

// GetToolResultText 提取工具结果的文本。
func GetToolResultText(content json.RawMessage) string {
	// 1. 尝试读取对象中的 content 字符串。
	var result struct {
		Content *string `json:"content"`
	}
	if json.Unmarshal(content, &result) == nil && result.Content != nil {
		return *result.Content
	}
	// 2. 尝试读取 JSON 字符串，其他内容原样返回。
	var text string
	if json.Unmarshal(content, &text) == nil {
		return text
	}
	return string(content)
}

// getConnectionInfos 按连接顺序返回各连接此刻的状态。
func getConnectionInfos(connections []mcpclient.Connection) []mcpclient.ConnectionInfo {
	// 1. 逐个读取连接状态；还没连上的连接也立刻返回状态。
	infos := make([]mcpclient.ConnectionInfo, 0, len(connections))
	for _, connection := range connections {
		infos = append(infos, connection.GetInfo())
	}
	return infos
}

// getVirtualFiles 把预置文件内容转成虚拟工作区文件。
func getVirtualFiles(files map[string][]byte) map[string]*workspace.VirtualFile {
	// 1. 按路径把内容填进虚拟文件。
	virtualFiles := make(map[string]*workspace.VirtualFile, len(files))
	for filePath, content := range files {
		virtualFiles[filePath] = &workspace.VirtualFile{Content: content}
	}
	return virtualFiles
}
