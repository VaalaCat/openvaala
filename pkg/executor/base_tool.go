package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
	"github.com/invopop/jsonschema"
	schemavalidator "github.com/santhosh-tekuri/jsonschema/v6"
)

// Tool 提供工具定义并执行单次调用。
type Tool interface {
	// GetDefinition 返回工具定义的副本。
	GetDefinition() *defs.ToolDefinition
	// Execute 执行一次调用并返回结果；参数错误、执行失败和取消都写入 ToolResult.Error，返回值非 nil 且 CallID 与调用一致。
	// progress 由 ToolExecutor 持有并在 Execute 返回后关闭；工具只发送，不需要进度的工具忽略它。
	Execute(ctx context.Context, call *defs.Item, progress chan<- *ExecutionProgressEvent) *ToolExecutionResult
}

// BaseTool 保存参数结构体 T 生成的工具定义和校验规则。
// T 必须是结构体；没有 omitempty 的字段必填，default 只补缺失字段，未声明的顶层参数会被忽略。
// 多次调用可共享工具定义，每次解析返回独立参数。
type BaseTool[T any] struct {
	definition          *defs.ToolDefinition
	inputSchemaCompiled *schemavalidator.Schema
}

// BaseToolOpt 保存工具名称和说明。
type BaseToolOpt struct {
	Name        string
	Description string
}

// NewBaseTool 根据参数结构体生成工具定义并编译参数校验规则。
func NewBaseTool[T any](opt *BaseToolOpt) (*BaseTool[T], error) {
	// 1. 校验工具名称和参数结构体类型。
	if opt == nil || strings.TrimSpace(opt.Name) == "" {
		return nil, errors.New("base tool requires a name")
	}
	argumentType := reflect.TypeFor[T]()
	if argumentType.Kind() != reflect.Struct {
		return nil, errors.New("base tool arguments must be a struct")
	}
	// 2. 从字段标签生成输入 schema。
	reflector := &jsonschema.Reflector{
		ExpandedStruct: true,
		Anonymous:      true,
	}
	schema := reflector.ReflectFromType(argumentType)
	inputSchemaJSON, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("generate tool %q schema: %w", opt.Name, err)
	}
	// 3. 编译 schema，保存工具定义和校验规则。
	document, err := schemavalidator.UnmarshalJSON(bytes.NewReader(inputSchemaJSON))
	if err != nil {
		return nil, fmt.Errorf("read tool %q schema: %w", opt.Name, err)
	}
	compiler := schemavalidator.NewCompiler()
	compiler.AssertFormat()
	const schemaURL = "urn:openvaala:tool-arguments"
	if err := compiler.AddResource(schemaURL, document); err != nil {
		return nil, err
	}
	inputSchemaCompiled, err := compiler.Compile(schemaURL)
	if err != nil {
		return nil, fmt.Errorf("compile tool %q schema: %w", opt.Name, err)
	}
	return &BaseTool[T]{
		definition: &defs.ToolDefinition{
			Name:        opt.Name,
			Description: opt.Description,
			InputSchema: inputSchemaJSON,
		},
		inputSchemaCompiled: inputSchemaCompiled,
	}, nil
}

// GetDefinition 返回工具定义的副本。
func (t *BaseTool[T]) GetDefinition() *defs.ToolDefinition {
	// 1. 复制工具定义。
	return t.definition.Clone()
}

// ParseArguments 解析并校验工具参数，返回独立的参数结构体。
func (t *BaseTool[T]) ParseArguments(input string) (*T, error) {
	// 1. 解码参数 JSON。
	document, err := schemavalidator.UnmarshalJSON(strings.NewReader(input))
	if err != nil {
		return nil, fmt.Errorf("decode tool %q arguments: %w", t.definition.Name, err)
	}
	// 2. 删除未声明的顶层参数，补齐默认值并校验。
	dropUndeclaredArguments(t.inputSchemaCompiled, document)
	applyArgumentDefaults(t.inputSchemaCompiled, document)
	if err := t.inputSchemaCompiled.Validate(document); err != nil {
		return nil, fmt.Errorf("validate tool %q arguments: %w", t.definition.Name, err)
	}
	// 3. 把处理后的参数转成 T。
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	arguments := new(T)
	if err := json.Unmarshal(encoded, arguments); err != nil {
		return nil, fmt.Errorf("decode tool %q arguments: %w", t.definition.Name, err)
	}
	return arguments, nil
}

// mcpTool 是某个 MCP 连接提供的一个工具：保存工具定义和提供它的连接；连接断开时调用返回 mcp_server_unavailable。
type mcpTool struct {
	definition *defs.ToolDefinition
	connection mcpclient.Connection
}

// GetDefinition 返回 MCP 工具定义的副本。
func (t *mcpTool) GetDefinition() *defs.ToolDefinition {
	// 1. 复制工具定义。
	return t.definition.Clone()
}

// Execute 在绑定的 MCP 连接上调用工具。
func (t *mcpTool) Execute(ctx context.Context, call *defs.Item, _ chan<- *ExecutionProgressEvent) *ToolExecutionResult {
	// 1. 调用 MCP 工具并包装执行结果，错误写入结果。
	return &ToolExecutionResult{Result: t.connection.CallTool(ctx, call)}
}
