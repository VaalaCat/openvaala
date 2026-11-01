package mcpclient

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/utils/jsonutil"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// modelToolNamePrefix 和 modelToolNameSeparator 组成模型看到的工具名：mcp__<server id>__<工具原名>。
	modelToolNamePrefix    = "mcp__"
	modelToolNameSeparator = "__"
	// maxModelToolNameLength 是模型工具名的长度上限。
	maxModelToolNameLength = 64
	// hashedModelToolNameKeptLength 是带哈希的工具名保留的前缀长度：加上 "_" 和 modelToolNameHashLength 位哈希正好 64。
	hashedModelToolNameKeptLength = 55
	modelToolNameHashLength       = 8
	// maxToolResultBytes 是一次工具结果文本的字节上限，超过的部分被截断。
	maxToolResultBytes = 1024 * 1024
)

// getModelToolName 生成模型使用的 MCP 工具名。
func getModelToolName(serverID, toolName string) string {
	// 1. 拼接全名并替换非法字符，超过 64 字符时使用前缀和原名哈希。
	originalName := getOriginalModelToolName(serverID, toolName)
	name := sanitizeModelToolName(originalName)
	if len(name) <= maxModelToolNameLength {
		return name
	}
	return getHashedModelToolName(name, originalName)
}

// getModelToolNames 按工具顺序生成模型使用的名称。
func getModelToolNames(serverID string, toolNames []string) []string {
	// 1. 逐个生成名称，字符替换后重名的工具改用哈希名，再记录结果。
	modelNames := make([]string, 0, len(toolNames))
	usedNames := make(map[string]struct{}, len(toolNames))
	for _, toolName := range toolNames {
		modelName := getModelToolName(serverID, toolName)
		if _, used := usedNames[modelName]; used {
			modelName = getHashedModelToolName(modelName, getOriginalModelToolName(serverID, toolName))
		}
		usedNames[modelName] = struct{}{}
		modelNames = append(modelNames, modelName)
	}
	return modelNames
}

// GetToolNameParts 把 mcp__<server id>__<工具名> 形式的模型工具名拆成 server ID 和工具名；不是这个形式时 ok 为 false。
func GetToolNameParts(name string) (serverID, toolName string, ok bool) {
	// 1. 去掉 mcp__ 前缀，在第一个 __ 处拆出非空的 server ID 和工具名。
	rest, found := strings.CutPrefix(name, modelToolNamePrefix)
	if !found {
		return "", "", false
	}
	serverID, toolName, found = strings.Cut(rest, modelToolNameSeparator)
	if !found || serverID == "" || toolName == "" {
		return "", "", false
	}
	return serverID, toolName, true
}

// getOriginalModelToolName 拼接尚未替换字符和缩短的完整工具名。
func getOriginalModelToolName(serverID, toolName string) string {
	// 1. 连接前缀、server ID、分隔符和原工具名。
	return modelToolNamePrefix + serverID + modelToolNameSeparator + toolName
}

// sanitizeModelToolName 把模型工具名里的非法字符换成下划线。
func sanitizeModelToolName(name string) string {
	// 1. 保留英文字母、数字、下划线和连字符，其他字符替换为下划线。
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		default:
			return '_'
		}
	}, name)
}

// getHashedModelToolName 返回带原名哈希的缩短工具名。
func getHashedModelToolName(sanitizedName, originalName string) string {
	// 1. 取替换字符后名称的前 55 个字符，拼上下划线和原名 SHA-256 的前 8 位十六进制。
	sum := sha256.Sum256([]byte(originalName))
	hash := hex.EncodeToString(sum[:])[:modelToolNameHashLength]
	return sanitizedName[:min(len(sanitizedName), hashedModelToolNameKeptLength)] + "_" + hash
}

// getModelInputSchema 给缺少顶层 type 的 MCP 工具输入 schema 补上 "type":"object"。
func getModelInputSchema(schema json.RawMessage) (json.RawMessage, error) {
	// 1. 解码 schema，有顶层 type 时保留原文，否则补 type 为 object 后编码。
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(schema, &fields); err != nil {
		return nil, fmt.Errorf("decode MCP tool input schema: %w", err)
	}
	if _, found := fields["type"]; found {
		return schema, nil
	}
	if fields == nil {
		fields = make(map[string]json.RawMessage, 1)
	}
	fields["type"] = json.RawMessage(`"object"`)
	return jsonutil.MarshalWithoutHTMLEscape(fields)
}

// newConnectionTools 筛选 server 工具并转换为模型工具名和输入 schema。
func newConnectionTools(definition *ServerDefinition, serverTools []*mcp.Tool) ([]ConnectionTool, error) {
	// 1. 按 ToolNames 筛选原工具；nil 表示全部。
	selectedTools := make([]*mcp.Tool, 0, len(serverTools))
	for _, tool := range serverTools {
		if definition.ToolNames == nil || slices.Contains(definition.ToolNames, tool.Name) {
			selectedTools = append(selectedTools, tool)
		}
	}
	// 2. 按顺序生成模型名称，编码 schema 并补齐顶层 type。
	originalNames := make([]string, 0, len(selectedTools))
	for _, tool := range selectedTools {
		originalNames = append(originalNames, tool.Name)
	}
	modelNames := getModelToolNames(definition.ID, originalNames)
	tools := make([]ConnectionTool, 0, len(selectedTools))
	for index, tool := range selectedTools {
		schema, err := jsonutil.MarshalWithoutHTMLEscape(tool.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("encode input schema of MCP tool %q: %w", tool.Name, err)
		}
		if schema, err = getModelInputSchema(schema); err != nil {
			return nil, fmt.Errorf("MCP tool %q: %w", tool.Name, err)
		}
		tools = append(tools, ConnectionTool{
			Name:         modelNames[index],
			OriginalName: tool.Name,
			Description:  tool.Description,
			InputSchema:  schema,
		})
	}
	return tools, nil
}

// newModelToolDefinitions 把连接的工具转成模型工具定义，并填入 server ID。
func newModelToolDefinitions(serverID string, tools []ConnectionTool) []*defs.ToolDefinition {
	// 1. 按工具顺序复制名称、说明、schema 和 server ID。
	definitions := make([]*defs.ToolDefinition, 0, len(tools))
	for _, tool := range tools {
		definitions = append(definitions, &defs.ToolDefinition{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: tool.InputSchema,
			MCPServerID: serverID,
		})
	}
	return definitions
}

// getPluginName 返回声明 server 的插件名称。
func getPluginName(definition *ServerDefinition) string {
	// 1. 读取插件名称，无插件时返回空字符串。
	if definition.Plugin == nil {
		return ""
	}
	return definition.Plugin.Name
}

// getProcessCommand 生成启动 stdio server 的 bash 命令。
func getProcessCommand(definition *ServerDefinition) string {
	// 1. 以 exec 开头，环境变量按键排序并逐项引用，再追加引用后的命令及参数。
	words := []string{"exec"}
	if len(definition.Env) > 0 {
		words = append(words, "env")
		for _, key := range slices.Sorted(maps.Keys(definition.Env)) {
			words = append(words, quoteShellWord(key+"="+definition.Env[key]))
		}
	}
	words = append(words, quoteShellWord(definition.Command))
	for _, argument := range definition.Args {
		words = append(words, quoteShellWord(argument))
	}
	return strings.Join(words, " ")
}

// quoteShellWord 把文本编码为 bash 的单个参数。
func quoteShellWord(word string) string {
	// 1. 用单引号包裹文本，并转义其中的单引号。
	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}

// newToolResult 把 MCP 调用结果转成工具正文或工具错误。
func newToolResult(callID string, result *mcp.CallToolResult) *defs.ToolResult {
	// 1. 收集文本和文本资源，图片、音频、二进制资源及链接改为占位文本。
	parts := make([]string, 0, len(result.Content))
	hasText := false
	for _, content := range result.Content {
		switch content := content.(type) {
		case *mcp.TextContent:
			hasText = true
			parts = append(parts, content.Text)
		case *mcp.EmbeddedResource:
			if content.Resource == nil {
				continue
			}
			if len(content.Resource.Blob) > 0 {
				parts = append(parts, "[resource: "+content.Resource.URI+"]")
				continue
			}
			hasText = true
			parts = append(parts, content.Resource.Text)
		case *mcp.ResourceLink:
			parts = append(parts, "[resource: "+content.URI+"]")
		case *mcp.ImageContent:
			parts = append(parts, fmt.Sprintf("[image omitted: %s, %s]", content.MIMEType, formatByteSize(len(content.Data))))
		case *mcp.AudioContent:
			parts = append(parts, fmt.Sprintf("[audio omitted: %s, %s]", content.MIMEType, formatByteSize(len(content.Data))))
		}
	}
	// 2. 用换行连接内容；没有文本时改用 structuredContent。
	body := strings.Join(parts, "\n")
	bodyIsJSON := false
	if !hasText && result.StructuredContent != nil {
		structured, err := jsonutil.MarshalWithoutHTMLEscape(result.StructuredContent)
		if err != nil {
			return newToolErrorResult(callID, "mcp_request_failed", fmt.Errorf("encode structured content: %w", err))
		}
		body, bodyIsJSON = string(structured), true
	}

	// 3. 超过 1 MB 时按完整 UTF-8 字符截断，再按 isError 生成错误或正文。
	truncated := len(body) > maxToolResultBytes
	if truncated {
		// 超限的 structuredContent 截断后按文本编码。
		body, bodyIsJSON = truncateUTF8(body, maxToolResultBytes), false
	}
	if result.IsError {
		return &defs.ToolResult{
			CallID: callID,
			Error: &defs.ToolError{
				Code:    "mcp_tool_error",
				Message: body,
			},
			Truncated: truncated,
		}
	}
	content := json.RawMessage(body)
	if !bodyIsJSON {
		content = encodeTextContent(body)
	}
	return &defs.ToolResult{
		CallID:    callID,
		Content:   content,
		Truncated: truncated,
	}
}

// newToolErrorResult 创建未取得 server 结果的失败记录。
func newToolErrorResult(callID, code string, err error) *defs.ToolResult {
	// 1. 填入调用 ID、错误码和原因，Content 保持为空。
	return &defs.ToolResult{
		CallID: callID,
		Error: &defs.ToolError{
			Code:    code,
			Message: err.Error(),
		},
	}
}

// encodeTextContent 把文本编码为 JSON 字符串。
func encodeTextContent(text string) json.RawMessage {
	// 1. 编码文本并返回 JSON，保留 HTML 字符原样；字符串编码失败时 panic。
	encoded, err := jsonutil.MarshalWithoutHTMLEscape(text)
	if err != nil {
		panic(err)
	}
	return encoded
}

// truncateUTF8 返回不超过字节上限的完整 UTF-8 前缀。
func truncateUTF8(text string, maxBytes int) string {
	// 1. 未超限时原样返回，否则回退到字符起点再截断。
	if len(text) <= maxBytes {
		return text
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

// formatByteSize 把字节数格式化为 B、KB 或 MB。
func formatByteSize(size int) string {
	// 1. 小于 1 KB 使用 B，小于 1 MB 使用四舍五入的 KB，其余使用四舍五入的 MB。
	const kilobyte, megabyte = 1024, 1024 * 1024
	switch {
	case size < kilobyte:
		return fmt.Sprintf("%d B", size)
	case size < megabyte:
		return fmt.Sprintf("%d KB", (size+kilobyte/2)/kilobyte)
	default:
		return fmt.Sprintf("%d MB", (size+megabyte/2)/megabyte)
	}
}
