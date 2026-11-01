package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/VaalaCat/openvaala/internal/config"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor/tools"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	acp "github.com/coder/acp-go-sdk"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"
)

// writeText 将文本完整写入输出流。
func writeText(output io.Writer, text string) error {
	// 1. 写入文本，写入出错或没写完时返回错误。
	written, err := io.WriteString(output, text)
	if err != nil {
		return fmt.Errorf("cannot write output: %w", err)
	}
	if written != len(text) {
		return fmt.Errorf("cannot write output: %w", io.ErrShortWrite)
	}
	return nil
}

// decodeACPMessage 解析 ACP 请求，检查协议版本、方法和请求 ID。
func decodeACPMessage(data []byte) (acpMessage, *acpError) {
	// 1. 检查 JSON 格式并解析请求，格式错误返回 -32700，无效请求返回 -32600。
	var message acpMessage
	if !json.Valid(data) {
		return message, &acpError{
			-32700,
			"parse error",
		}
	}
	if err := json.Unmarshal(data, &message); err != nil || message.JSONRPC != "2.0" || message.Method == "" {
		return acpMessage{}, &acpError{
			-32600,
			"invalid request",
		}
	}
	// 2. 请求带 ID 时仅接受字符串或数字。
	if len(message.ID) != 0 {
		var id any
		_ = json.Unmarshal(message.ID, &id)
		switch id.(type) {
		case string, float64:
		default:
			return acpMessage{}, &acpError{
				-32600,
				"invalid request id",
			}
		}
	}
	return message, nil
}

// decodeACPParams 把请求的 params 解析到 target，params 必须是 JSON 对象。
func decodeACPParams(data json.RawMessage, target any) error {
	// 1. params 为空、不是 JSON 对象或解析失败时返回 -32602。
	if len(data) == 0 || len(bytes.TrimSpace(data)) == 0 || bytes.TrimSpace(data)[0] != '{' {
		return &acpError{
			-32602,
			"params must be an object",
		}
	}
	if err := json.Unmarshal(data, target); err != nil {
		return &acpError{
			-32602,
			err.Error(),
		}
	}
	return nil
}

// acpResponse 把处理结果或错误转换为 ACP 响应。
func acpResponse(id json.RawMessage, result any, err error) acpMessage {
	// 1. 沿用请求 ID（没有时用 null）创建响应，没有错误时直接返回成功响应。
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	response := acpMessage{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
	if err == nil {
		return response
	}
	// 2. 有错误时清除结果并按错误类型设置错误码：协议错误原样返回，会话忙为 -32001，找不到对话为 -32602，其余为 -32603。
	response.Result = nil
	var protocolError *acpError
	switch {
	case errors.As(err, &protocolError):
		response.Error = protocolError
	case errors.Is(err, session.ErrSessionBusy):
		response.Error = &acpError{
			-32001,
			err.Error(),
		}
	case errors.Is(err, runtime.ErrConversationNotFound):
		response.Error = &acpError{
			-32602,
			"session not found",
		}
	default:
		response.Error = &acpError{
			-32603,
			err.Error(),
		}
	}
	return response
}

// getACPPromptContent 把 ACP 输入块转换为文本、图片和上下文文本。
func getACPPromptContent(blocks []acp.ContentBlock) ([]*defs.Content, error) {
	// 1. 拒绝空输入。
	if len(blocks) == 0 {
		return nil, &acpError{
			-32602,
			"prompt is empty",
		}
	}
	// 2. 逐块转换：文本原样保留，图片转成 data URL，链接和嵌入资源转成上下文文本（嵌入的图片仍按图片处理）；
	// 类型、编码或 MIME 不支持时返回 -32602。
	content := make([]*defs.Content, 0, len(blocks))
	for _, block := range blocks {
		var text string
		isContext := false
		switch {
		case block.Text != nil:
			text = block.Text.Text
		case block.Image != nil:
			image, err := getACPImageContent(block.Image.Data, block.Image.MimeType)
			if err != nil {
				return nil, err
			}
			content = append(content, image)
			continue
		case block.ResourceLink != nil:
			text, isContext = block.ResourceLink.Name+"\n"+block.ResourceLink.Uri, true
		case block.Resource != nil:
			resource := block.Resource.Resource
			isContext = true
			if resource.TextResourceContents != nil {
				text = resource.TextResourceContents.Uri + "\n" + resource.TextResourceContents.Text
			} else if resource.BlobResourceContents != nil {
				blob := resource.BlobResourceContents
				mimeType := ""
				if blob.MimeType != nil {
					mimeType = *blob.MimeType
				}
				if strings.HasPrefix(mimeType, "image/") {
					image, err := getACPImageContent(blob.Blob, mimeType)
					if err != nil {
						return nil, err
					}
					image.Image.Alt = blob.Uri
					content = append(content, image)
					continue
				}
				if !strings.HasPrefix(mimeType, "text/") && mimeType != "application/json" {
					return nil, &acpError{
						-32602,
						"unsupported embedded resource mime type",
					}
				}
				data, err := base64.StdEncoding.DecodeString(blob.Blob)
				if err != nil || !utf8.Valid(data) {
					return nil, &acpError{
						-32602,
						"invalid embedded text",
					}
				}
				text = blob.Uri + "\n" + string(data)
			} else {
				return nil, &acpError{
					-32602,
					"embedded resource is missing",
				}
			}
		default:
			return nil, &acpError{
				-32602,
				"unsupported prompt content",
			}
		}
		content = append(content, &defs.Content{
			Kind: defs.ContentText,
			Text: &defs.TextContent{
				Text:      text,
				IsContext: isContext,
			},
		})
	}
	return content, nil
}

// getACPImageContent 检查 Base64 图片，转换为 data URL 图片内容。
func getACPImageContent(data, mimeType string) (*defs.Content, error) {
	// 1. 数据为空、不是合法 Base64 或 MIME 不是 image/ 开头时返回 -32602，否则构造图片内容。
	if _, err := base64.StdEncoding.DecodeString(data); err != nil || !strings.HasPrefix(mimeType, "image/") || data == "" {
		return nil, &acpError{
			-32602,
			"invalid image content",
		}
	}
	return &defs.Content{
		Kind:  defs.ContentImage,
		Image: &defs.ImageContent{DataURL: "data:" + mimeType + ";base64," + data},
	}, nil
}

// getACPImageBlock 把 data URL 图片转换为 ACP 图片块。
func getACPImageBlock(image *defs.ImageContent) acp.ContentBlock {
	// 1. 拆分 MIME 类型和图片数据，构造 ACP 图片块。
	mimeType, data, _ := image.GetDataURLParts()
	return acp.ImageBlock(data, mimeType)
}

// getACPMessageUpdate 把一段内容转换为用户或 Agent 的消息更新。
func getACPMessageUpdate(content *defs.Content, isUser bool) acp.SessionUpdate {
	// 1. 将图片或文本构造为 ACP 内容块。
	var block acp.ContentBlock
	if content.Kind == defs.ContentImage {
		block = getACPImageBlock(content.Image)
	} else {
		block = acp.TextBlock(content.Text.Text)
	}
	// 2. 根据消息来源返回用户或 Agent 更新。
	if isUser {
		return acp.UpdateUserMessage(block)
	}
	return acp.UpdateAgentMessage(block)
}

// getACPConfigOptions 生成编辑器里会话的模型、推理强度和 Executor 下拉选项。
func getACPConfigOptions(models []*defs.ModelDefinition, selected defs.ModelSelection, executors []conversation.ExecutorDetails) []acp.SessionConfigOption {
	// 1. 列出模型选项（没有显示名时用模型引用），找到当前选中的模型。
	modelOptions := acp.SessionConfigSelectOptionsUngrouped{}
	var selectedDefinition *defs.ModelDefinition
	for _, model := range models {
		name := model.DisplayName
		if name == "" {
			name = model.Ref.String()
		}
		modelOptions = append(modelOptions, acp.SessionConfigSelectOption{
			Name:  name,
			Value: acp.SessionConfigValueId(model.Ref.String()),
		})
		if model.Ref == selected.Ref {
			selectedDefinition = model
		}
	}
	result := []acp.SessionConfigOption{{Select: &acp.SessionConfigOptionSelect{
		Id:           "model",
		Name:         "Model",
		Type:         "select",
		Category:     acp.Ptr(acp.SessionConfigOptionCategory("model")),
		CurrentValue: acp.SessionConfigValueId(selected.Ref.String()),
		Options:      acp.SessionConfigSelectOptions{Ungrouped: &modelOptions},
	}}}
	// 2. 所选模型支持推理强度时添加对应选项。
	if selectedDefinition != nil && len(selectedDefinition.ReasoningEfforts) > 0 {
		options := acp.SessionConfigSelectOptionsUngrouped{}
		for _, effort := range selectedDefinition.ReasoningEfforts {
			options = append(options, acp.SessionConfigSelectOption{
				Name:  effort,
				Value: acp.SessionConfigValueId(effort),
			})
		}
		result = append(result, acp.SessionConfigOption{Select: &acp.SessionConfigOptionSelect{
			Id:           "reasoning_effort",
			Name:         "Reasoning effort",
			Type:         "select",
			Category:     acp.Ptr(acp.SessionConfigOptionCategory("thought_level")),
			CurrentValue: acp.SessionConfigValueId(selected.ReasoningEffort),
			Options:      acp.SessionConfigSelectOptions{Ungrouped: &options},
		}})
	}
	// 3. 存在 Executor 列表时添加选项，第一个 Executor 作为当前默认值。
	if len(executors) > 0 {
		options := acp.SessionConfigSelectOptionsUngrouped{}
		for _, details := range executors {
			option := acp.SessionConfigSelectOption{
				Name:  details.ID,
				Value: acp.SessionConfigValueId(details.ID),
			}
			if details.Description != "" {
				option.Description = acp.Ptr(details.Description)
			}
			options = append(options, option)
		}
		result = append(result, acp.SessionConfigOption{Select: &acp.SessionConfigOptionSelect{
			Id:           "executor",
			Name:         "Executor",
			Type:         "select",
			CurrentValue: acp.SessionConfigValueId(executors[0].ID),
			Options:      acp.SessionConfigSelectOptions{Ungrouped: &options},
		}})
	}
	return result
}

// getACPToolTitle 生成编辑器显示的工具标题。
func getACPToolTitle(name string) string {
	// 1. MCP 工具名拆成 server id 和原工具名，标题写成「server id: 工具名」；拆不开时返回原工具名。
	if serverID, toolName, ok := mcpclient.GetToolNameParts(name); ok {
		return serverID + ": " + toolName
	}
	return name
}

// acpInvalidMCPServerIDChars 匹配 server id 不能用的字符，acpRepeatedUnderscores 匹配连续的下划线。
var (
	acpInvalidMCPServerIDChars = regexp.MustCompile(`[^A-Za-z0-9_-]`)
	acpRepeatedUnderscores     = regexp.MustCompile(`_+`)
)

// getACPClientMCPServers 筛出编辑器给的 MCP server，为每个生成合法且不重复的 server id。
func getACPClientMCPServers(ctx context.Context, servers []acp.McpServer) []acpClientMCPServer {
	// 1. 保留 stdio、http 和 sse，其他类型或空名称记录警告后跳过；[A-Za-z0-9_-] 以外的字符换为下划线，连续下划线合并，重名追加 -2、-3 等后缀。
	result := make([]acpClientMCPServer, 0, len(servers))
	for _, server := range servers {
		name, isSupported := getACPMCPServerName(server)
		if !isSupported {
			telemetry.GetLogger(ctx).Warn("ACP MCP server skipped: only stdio, http and sse are supported", "name", name)
			continue
		}
		baseID := acpRepeatedUnderscores.ReplaceAllString(acpInvalidMCPServerIDChars.ReplaceAllString(name, "_"), "_")
		if baseID == "" {
			telemetry.GetLogger(ctx).Warn("ACP MCP server skipped: the name is empty")
			continue
		}
		id := baseID
		for count := 2; slices.ContainsFunc(result, func(added acpClientMCPServer) bool { return added.ID == id }); count++ {
			id = fmt.Sprintf("%s-%d", baseID, count)
		}
		result = append(result, acpClientMCPServer{
			ID:     id,
			Server: server,
		})
	}
	// 2. 记一条 Info 日志：收到的数量和按顺序排列的登记摘要，列表为空也记。
	summaries := make([]string, 0, len(result))
	for _, server := range result {
		summaries = append(summaries, getACPMCPServerSummary(server))
	}
	telemetry.GetLogger(ctx).Info("ACP MCP servers received", "received", len(servers), "registered", summaries)
	return result
}

// getACPMCPServerSummary 生成一个 MCP server 写进日志的摘要。
func getACPMCPServerSummary(server acpClientMCPServer) string {
	// 1. 按传输类型返回 id、类型和 stdio 命令或 URL 的 scheme://host；命令参数、环境变量、请求头和 URL 的其他部分都不写。
	switch {
	case server.Server.Stdio != nil:
		return server.ID + " stdio " + server.Server.Stdio.Command
	case server.Server.Http != nil:
		return server.ID + " http " + getURLOrigin(server.Server.Http.Url)
	case server.Server.Sse != nil:
		return server.ID + " sse " + getURLOrigin(server.Server.Sse.Url)
	}
	panic("ACP MCP server is not stdio, http or sse")
}

// getURLOrigin 返回 URL 的 scheme://host。
func getURLOrigin(rawURL string) string {
	// 1. 解析 URL 并拼出 scheme://host；解析失败或没有 host 时返回 (invalid url)。
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return "(invalid url)"
	}
	return parsed.Scheme + "://" + parsed.Host
}

// getACPMCPServerName 返回 MCP server 的名字，以及它的传输类型是否支持。
func getACPMCPServerName(server acp.McpServer) (name string, isSupported bool) {
	// 1. stdio、http、sse 返回名字和支持；acp 返回名字和不支持，未知类型返回空名字和不支持。
	switch {
	case server.Stdio != nil:
		return server.Stdio.Name, true
	case server.Http != nil:
		return server.Http.Name, true
	case server.Sse != nil:
		return server.Sse.Name, true
	case server.Acp != nil:
		return server.Acp.Name, false
	}
	return "", false
}

// getACPClientMCPTransport 按编辑器给的配置，创建连接本机 MCP server 的传输。
func getACPClientMCPTransport(server acp.McpServer, directory string, environment []string) mcp.Transport {
	// 1. stdio 在 directory 下运行命令，环境是 environment 加上配置的变量；HTTP 和 SSE 给每个请求加上配置的请求头。
	switch {
	case server.Stdio != nil:
		command := exec.Command(server.Stdio.Command, server.Stdio.Args...)
		command.Dir = directory
		command.Env = slices.Clone(environment)
		for _, variable := range server.Stdio.Env {
			command.Env = append(command.Env, variable.Name+"="+variable.Value)
		}
		return &mcp.CommandTransport{Command: command}
	case server.Http != nil:
		return &mcp.StreamableClientTransport{
			Endpoint:   server.Http.Url,
			HTTPClient: &http.Client{Transport: &acpHeaderRoundTripper{headers: server.Http.Headers}},
		}
	case server.Sse != nil:
		return &mcp.SSEClientTransport{
			Endpoint:   server.Sse.Url,
			HTTPClient: &http.Client{Transport: &acpHeaderRoundTripper{headers: server.Sse.Headers}},
		}
	}
	panic("ACP MCP server is not stdio, http or sse")
}

// acpHeaderRoundTripper 给发出的每个请求加上编辑器为 MCP server 配置的 headers，同名的覆盖原有的值。
type acpHeaderRoundTripper struct {
	headers []acp.HttpHeader
}

// RoundTrip 为请求添加编辑器配置的请求头后发送。
func (r *acpHeaderRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	// 1. 复制请求，覆盖同名请求头，再交给默认 HTTP 传输。
	request = request.Clone(request.Context())
	for _, header := range r.headers {
		request.Header.Set(header.Name, header.Value)
	}
	return http.DefaultTransport.RoundTrip(request)
}

// getACPToolKind 按工具名称返回编辑器显示的工具种类。
func getACPToolKind(name string) acp.ToolKind {
	// 1. 文件读取归为 read，补丁归为 edit，命令工具归为 execute，其余归为 other。
	switch {
	case name == "read_file":
		return acp.ToolKindRead
	case name == "apply_patch":
		return acp.ToolKindEdit
	case slices.Contains(tools.GetCommandToolNames(), name):
		return acp.ToolKindExecute
	default:
		return acp.ToolKindOther
	}
}

// getACPToolLocations 生成 read_file 和 apply_patch 涉及的文件绝对路径。
func getACPToolLocations(call *defs.ToolCall, executors []conversation.ExecutorDetails) []acp.ToolCallLocation {
	// 1. 解析工具参数，选出参数指定的 Executor，没指定时用第一个；参数无效、没有 Executor 或找不到指定的时返回空。
	var args struct {
		Path     string `json:"path"`
		Patch    string `json:"patch"`
		Executor string `json:"executor"`
	}
	if json.Unmarshal([]byte(call.Arguments), &args) != nil || len(executors) == 0 {
		return nil
	}
	selected := executors[0]
	if args.Executor != "" {
		index := slices.IndexFunc(executors, func(details conversation.ExecutorDetails) bool { return details.ID == args.Executor })
		if index < 0 {
			return nil
		}
		selected = executors[index]
	}
	// 2. 从 read_file 参数或 apply_patch 变更中收集文件路径。
	var paths []string
	switch call.Name {
	case "read_file":
		if args.Path != "" {
			paths = []string{args.Path}
		}
	case "apply_patch":
		changes, err := tools.GetPatchFileChanges(args.Patch)
		if err != nil {
			return nil
		}
		for _, file := range changes {
			paths = append(paths, file.Path)
		}
	}
	// 3. 按 Executor 工作目录补全相对路径并整理路径。
	result := make([]acp.ToolCallLocation, 0, len(paths))
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			path = filepath.Join(selected.WorkspaceDirectory, path)
		}
		result = append(result, acp.ToolCallLocation{Path: filepath.Clean(path)})
	}
	return result
}

// getACPReasoningText 拼出推理文字：有摘要用摘要，没有时用正文。
func getACPReasoningText(summary, content []string) string {
	// 1. 有摘要时拼接摘要，否则拼接正文。
	if len(summary) > 0 {
		return strings.Join(summary, "")
	}
	return strings.Join(content, "")
}

// getRuntimeServerFilePath 返回本机 serve 连接文件的路径。
func getRuntimeServerFilePath() (string, error) {
	// 1. 读取用户主目录，拼接 .local/state/openvaala/runtime-server.yaml。
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate user directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "openvaala", "runtime-server.yaml"), nil
}

// getRuntimeServerFile 读取并解析本机连接文件 runtime-server.yaml。
func getRuntimeServerFile() (file RuntimeServerConnection, found bool, err error) {
	// 1. 定位并读取连接文件，不存在时 found 为 false，其他读取失败返回错误。
	path, err := getRuntimeServerFilePath()
	if err != nil {
		return RuntimeServerConnection{}, false, err
	}
	payload, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return RuntimeServerConnection{}, false, nil
	}
	if err != nil {
		return RuntimeServerConnection{}, false, fmt.Errorf("read runtime server file: %w", err)
	}
	// 2. 解析 YAML，成功时返回连接信息和 found=true。
	if err := yaml.Unmarshal(payload, &file); err != nil {
		return RuntimeServerConnection{}, false, fmt.Errorf("parse runtime server file %s: %w", path, err)
	}
	return file, true, nil
}

// getRuntimeServerHTTPURL 把 serve 的 WebSocket 地址换成 path 对应的 HTTP 地址。
func getRuntimeServerHTTPURL(runtimeURL, path string) (string, error) {
	// 1. 解析地址，将 ws 转为 http、wss 转为 https，其他协议返回错误。
	parsed, err := url.Parse(runtimeURL)
	if err != nil {
		return "", fmt.Errorf("parse runtime server url: %w", err)
	}
	switch parsed.Scheme {
	case "ws":
		parsed.Scheme = "http"
	case "wss":
		parsed.Scheme = "https"
	default:
		return "", fmt.Errorf("runtime server url %q must start with ws:// or wss://", runtimeURL)
	}
	// 2. 路径换成 path，去掉查询参数。
	parsed.Path, parsed.RawPath, parsed.RawQuery = path, "", ""
	return parsed.String(), nil
}

// getCurrentExecutable 读取当前可执行文件路径和修改时间。
func getCurrentExecutable() (path string, modifiedAt time.Time, err error) {
	// 1. 定位当前可执行文件并解析软链接，失败时返回零值和错误。
	executable, err := os.Executable()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("locate executable: %w", err)
	}
	path, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("resolve executable %s: %w", executable, err)
	}
	// 2. 读取文件修改时间，失败时返回零值和错误。
	info, err := os.Stat(path)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("stat executable %s: %w", path, err)
	}
	return path, info.ModTime(), nil
}

// GetConfigFlags 收集显式传入的 --config-file、--agents-dir、--models-dir 和 --agent，写成 --name=value。
func GetConfigFlags(flags *pflag.FlagSet) ([]string, error) {
	// 1. 跳过没传的参数；只有配置文件转成绝对路径，目录参数保留原值。
	var configFlags []string
	for _, name := range []string{"config-file", "agents-dir", "models-dir", "agent"} {
		flag := flags.Lookup(name)
		if flag == nil || !flag.Changed {
			continue
		}
		value := flag.Value.String()
		if name == "config-file" {
			var err error
			if value, err = getAbsolutePath(value); err != nil {
				return nil, fmt.Errorf("--%s: %w", name, err)
			}
		}
		configFlags = append(configFlags, "--"+name+"="+value)
	}
	return configFlags, nil
}

// getAbsolutePath 将相对路径转换为绝对路径。
func getAbsolutePath(path string) (string, error) {
	// 1. 保留空串、~ 和 ~/ 路径，其余按当前目录转换为绝对路径。
	if path == "" || path == "~" || strings.HasPrefix(path, "~/") {
		return path, nil
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve path %s: %w", path, err)
	}
	return absolute, nil
}

// getRuntimeServerEnvironment 生成自动拉起的 serve 使用的环境变量。
func getRuntimeServerEnvironment(environment []string) ([]string, error) {
	// 1. 复制环境，将 OPENVAALA_CONFIG 中的相对路径改为绝对路径。
	result := slices.Clone(environment)
	for index, entry := range result {
		value, isConfig := strings.CutPrefix(entry, "OPENVAALA_CONFIG=")
		if !isConfig {
			continue
		}
		value, err := getAbsolutePath(value)
		if err != nil {
			return nil, fmt.Errorf("OPENVAALA_CONFIG: %w", err)
		}
		result[index] = "OPENVAALA_CONFIG=" + value
	}
	return result, nil
}

// newCodexAppLaunchCommand 创建用 /usr/bin/open 启动 Codex 桌面应用的命令，后端改用当前 openvaala。
func newCodexAppLaunchCommand(ctx context.Context, args *AppArgs, applicationPath, directory string) *exec.Cmd {
	// 1. CODEX_CLI_PATH 设为当前 openvaala，本地连接经它跳过 Codex daemon；CODEX_APP_SERVER_FORCE_CLI 不设置，
	// durable 云端连接照常用原来的传输。再带上全部 OPENVAALA_ 环境变量，其中 OPENVAALA_CONFIG 换成已解析的绝对路径。
	environment := map[string]string{
		"CODEX_CLI_PATH": args.Executable,
	}
	for _, entry := range os.Environ() {
		name, value, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "OPENVAALA_") {
			environment[name] = value
		}
	}
	if _, found := environment["OPENVAALA_CONFIG"]; found {
		environment["OPENVAALA_CONFIG"] = args.ConfigPath
	}
	// 2. 把显式传入的配置参数和 --url 换成对应的 OPENVAALA_ 环境变量。
	for _, flag := range args.ConfigFlags {
		name, _, _ := strings.Cut(flag, "=")
		switch name {
		case "--config-file":
			environment["OPENVAALA_CONFIG"] = args.ConfigPath
		case "--agents-dir":
			environment["OPENVAALA_AGENTS_DIRECTORY"] = args.Config.Agents.Directory
		case "--models-dir":
			environment["OPENVAALA_MODELS_DIRECTORY"] = args.Config.Models.Directory
		case "--agent":
			environment["OPENVAALA_RUN_AGENT_NAME"] = args.Config.Run.AgentName
		}
	}
	if args.RuntimeURL != "" {
		environment["OPENVAALA_CLIENT_URL"] = args.RuntimeURL
	}
	// 3. 按名字排序写成 --env 参数，工作目录编码进新对话深链接 codex://threads/new。
	arguments := []string{"-a", applicationPath}
	for _, name := range slices.Sorted(maps.Keys(environment)) {
		arguments = append(arguments, "--env", name+"="+environment[name])
	}
	arguments = append(arguments, "codex://threads/new?"+url.Values{"path": {directory}}.Encode())
	return exec.CommandContext(ctx, "/usr/bin/open", arguments...)
}

// isRuntimeServerFromCurrentExecutable 连接文件记录的 serve 是不是由当前可执行文件启动的？
func isRuntimeServerFromCurrentExecutable(ctx context.Context, file RuntimeServerConnection) bool {
	// 1. 连接文件没有可执行文件记录时记录警告并返回 true。
	if file.Executable == "" || file.ExecutableModifiedAt.IsZero() {
		telemetry.GetLogger(ctx).Warn("runtime server file has no executable record; skipping the executable check")
		return true
	}
	// 2. 读取当前可执行文件的路径和修改时间，读不到时记录警告并返回 true，否则两项都相同才返回 true。
	path, modifiedAt, err := getCurrentExecutable()
	if err != nil {
		telemetry.GetLogger(ctx).Warn("cannot locate the current executable; skipping the executable check", "error", err)
		return true
	}
	return path == file.Executable && modifiedAt.Equal(file.ExecutableModifiedAt)
}

// isSameRuntimeServer runtimeURL 指向的是不是连接文件里那个 serve？
func isSameRuntimeServer(runtimeURL, fileURL string) bool {
	// 1. 解析两个 URL，解析失败或端口不同时返回 false。
	parsedRuntime, err := url.Parse(runtimeURL)
	if err != nil {
		return false
	}
	parsedFile, err := url.Parse(fileURL)
	if err != nil {
		return false
	}
	if parsedRuntime.Port() != parsedFile.Port() {
		return false
	}
	// 2. 主机名相同，或 runtimeURL 的主机是 127.0.0.1、::1、localhost 时返回 true；本机的局域网 IP 不算。
	host := parsedRuntime.Hostname()
	return strings.EqualFold(host, parsedFile.Hostname()) || host == "127.0.0.1" || host == "::1" || strings.EqualFold(host, "localhost")
}

// isTokenInURL URL 是否带 token 查询参数？空值也算带了，URL 解析失败时算没带。
func isTokenInURL(runtimeURL string) bool {
	// 1. 解析 URL，检查查询参数里有没有 token。
	parsed, err := url.Parse(runtimeURL)
	return err == nil && parsed.Query().Has("token")
}

// writeRuntimeServerNotice 本机 serve 由另一个可执行文件启动时，向 output 写一行提示。
func writeRuntimeServerNotice(ctx context.Context, output io.Writer, connection runtimeConnection) {
	// 1. serve 不是由另一个可执行文件启动时直接返回；写提示失败时记录警告。
	if !connection.IsFromOtherExecutable {
		return
	}
	if _, err := fmt.Fprintln(output, runtimeServerExecutableChangedNotice); err != nil {
		telemetry.GetLogger(ctx).Warn("cannot show the runtime server notice", "error", err)
	}
}

// getPluginSourceText 把插件安装记录拼成「来源#目录@ref」文字。
func getPluginSourceText(record *config.PluginInstallRecord) string {
	// 1. 在来源后面追加非空的 #目录和 @ref。
	text := record.Source
	if record.Path != "" {
		text += "#" + record.Path
	}
	if record.Ref != "" {
		text += "@" + record.Ref
	}
	return text
}
