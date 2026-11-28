package acp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/externalagent"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
	"github.com/VaalaCat/openvaala/pkg/prompt"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	sdk "github.com/coder/acp-go-sdk"
)

// mergeParamOverwrite 将同一方法的配置依次合并，并按 SDK 请求类型解码。
func mergeParamOverwrite[T any](req T, method string, overwrites ...map[string]map[string]any) (T, error) {
	var fields map[string]any
	for _, byMethod := range overwrites {
		patch := byMethod[method]
		if len(patch) == 0 {
			continue
		}
		if fields == nil {
			raw, err := json.Marshal(req)
			if err != nil {
				return req, err
			}
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.UseNumber()
			if err := decoder.Decode(&fields); err != nil {
				return req, err
			}
		}
		mergeParamFields(fields, patch)
	}
	if fields == nil {
		return req, nil
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return req, err
	}
	var merged T
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	err = decoder.Decode(&merged)
	return merged, err
}

func mergeParamFields(fields, patch map[string]any) {
	for key, value := range patch {
		if object, ok := value.(map[string]any); ok {
			nested, _ := fields[key].(map[string]any)
			if nested == nil {
				nested = make(map[string]any)
			}
			mergeParamFields(nested, object)
			fields[key] = nested
		} else {
			fields[key] = value
		}
	}
}

func (s *acpAgentSession) getParamOverwrites() []map[string]map[string]any {
	if s.openReq.Ephemeral {
		return []map[string]map[string]any{s.command.ParamOverwrite, s.command.EphemeralParamOverwrite}
	}
	return []map[string]map[string]any{s.command.ParamOverwrite}
}

type processLog struct{ ctx context.Context }

func (w processLog) Write(p []byte) (int, error) {
	if msg := strings.TrimSpace(string(p)); msg != "" {
		telemetry.GetLogger(w.ctx).Info("ACP stderr", "message", msg)
	}
	return len(p), nil
}

// isPromptFrame 是否是 SDK 发送给后端的 Prompt 请求？
func isPromptFrame(frame []byte) bool {
	// SDK 的 JSON-RPC 信封先编码 method，再编码 params；前缀之外不可能有顶层 method。
	head, _, _ := bytes.Cut(frame, []byte(`,"params":`))
	return bytes.Contains(head, []byte(`"method":"session/prompt"`))
}

func ptrValue[T any](p *T) (v T) {
	if p != nil {
		return *p
	}
	return v
}

func rpcError(err error) error {
	var re *sdk.RequestError
	if errors.As(err, &re) {
		return fmt.Errorf("%s (code %d)", re.Message, re.Code)
	}
	return err
}

// cloneOptions 隔离调用方对组和选项切片的修改。
func cloneOptions(o defs.ExternalOptions) defs.ExternalOptions {
	copyGroup := func(g *defs.ExternalOptionGroup) *defs.ExternalOptionGroup {
		if g == nil {
			return nil
		}
		group := *g
		group.Options = slices.Clone(group.Options)
		return &group
	}
	o.Mode, o.Model, o.Thought = copyGroup(o.Mode), copyGroup(o.Model), copyGroup(o.Thought)
	o.Others = slices.Clone(o.Others)
	for i := range o.Others {
		o.Others[i].Options = slices.Clone(o.Others[i].Options)
	}
	return o
}

func externalOptions(modes *sdk.SessionModeState, config []sdk.SessionConfigOption) defs.ExternalOptions {
	var opts defs.ExternalOptions
	for _, o := range config {
		var group defs.ExternalOptionGroup
		group.Kind = "config"
		var category sdk.SessionConfigOptionCategory
		switch {
		case o.Select != nil:
			v := o.Select
			group.ID, group.Label, group.CurrentValue = string(v.Id), v.Name, string(v.CurrentValue)
			category = ptrValue(v.Category)
			var choices []sdk.SessionConfigSelectOption
			if v.Options.Ungrouped != nil {
				choices = *v.Options.Ungrouped
			}
			if v.Options.Grouped != nil {
				for _, g := range *v.Options.Grouped {
					choices = append(choices, g.Options...)
				}
			}
			for _, choice := range choices {
				group.Options = append(group.Options, defs.ExternalOptionChoice{Value: string(choice.Value), Label: choice.Name, Description: ptrValue(choice.Description)})
			}
		case o.Boolean != nil:
			v := o.Boolean
			group.ID, group.Label, group.CurrentValue = string(v.Id), v.Name, strconv.FormatBool(v.CurrentValue)
			category = ptrValue(v.Category)
			group.Options = []defs.ExternalOptionChoice{{Value: "false", Label: "false"}, {Value: "true", Label: "true"}}
		default:
			continue
		}
		switch {
		case category == sdk.SessionConfigOptionCategoryMode && opts.Mode == nil:
			opts.Mode = &group
		case category == sdk.SessionConfigOptionCategoryModel && opts.Model == nil:
			opts.Model = &group
		case category == sdk.SessionConfigOptionCategoryThoughtLevel && opts.Thought == nil:
			opts.Thought = &group
		default:
			opts.Others = append(opts.Others, group)
		}
	}
	if opts.Mode == nil && modes != nil {
		group := &defs.ExternalOptionGroup{Kind: "mode", ID: "mode", Label: "Mode", CurrentValue: string(modes.CurrentModeId)}
		for _, m := range modes.AvailableModes {
			group.Options = append(group.Options, defs.ExternalOptionChoice{Value: string(m.Id), Label: m.Name, Description: ptrValue(m.Description)})
		}
		opts.Mode = group
	}
	return opts
}

func mcpServers(req externalagent.ExternalSessionOpenRequest, httpSupported bool) ([]sdk.McpServer, []string, error) {
	servers := []sdk.McpServer{}
	var names []string
	for _, d := range req.MCPServers {
		if d == nil {
			continue
		}
		switch d.Type {
		case mcpclient.ServerTransportStdio:
			env := []sdk.EnvVariable{}
			for _, k := range slices.Sorted(maps.Keys(d.Env)) {
				env = append(env, sdk.EnvVariable{Name: k, Value: d.Env[k]})
			}
			servers = append(servers, sdk.McpServer{Stdio: &sdk.McpServerStdio{Name: d.ID, Command: d.Command, Args: append([]string{}, d.Args...), Env: env}})
		case mcpclient.ServerTransportHTTP:
			if !httpSupported {
				continue
			}
			servers = append(servers, httpServer(d.ID, d.URL, d.Headers))
		default:
			continue
		}
		names = append(names, d.ID)
	}
	if req.LocalMCPURL != "" {
		token := req.LaunchEnv["OPENVAALA_TOKEN"]
		if httpSupported {
			var headers map[string]string
			if token != "" {
				headers = map[string]string{"Authorization": "Bearer " + token}
			}
			servers = append(servers, httpServer("openvaala", req.LocalMCPURL, headers))
		} else {
			command := "openvaala"
			if req.ExecutorKind == defs.ExecutorKindLocal {
				var err error
				command, err = os.Executable()
				if err != nil {
					return nil, nil, fmt.Errorf("locate local MCP bridge executable: %w", err)
				}
			}
			servers = append(servers, sdk.McpServer{Stdio: &sdk.McpServerStdio{
				Name: "openvaala", Command: command, Args: []string{"mcp-bridge"},
				Env: []sdk.EnvVariable{
					{Name: "OPENVAALA_MCP_URL", Value: req.LocalMCPURL},
					{Name: "OPENVAALA_TOKEN", Value: token},
				},
			}})
		}
		names = append(names, "openvaala")
	}
	return servers, names, nil
}

func httpServer(name, url string, values map[string]string) sdk.McpServer {
	headers := []sdk.HttpHeader{}
	for _, k := range slices.Sorted(maps.Keys(values)) {
		headers = append(headers, sdk.HttpHeader{Name: k, Value: values[k]})
	}
	return sdk.McpServer{Http: &sdk.McpServerHttpInline{Type: "http", Name: name, Url: url, Headers: headers}}
}

// 只有在一页后端结果中途达到搜索上限时才需要记住页内偏移。
func searchCursor(cursor string, skip int) string {
	data, _ := json.Marshal(listPosition{Cursor: cursor, Skip: skip})
	return "ov-search:" + base64.RawURLEncoding.EncodeToString(data)
}

func parseSearchCursor(cursor string) (string, int, error) {
	raw, ok := strings.CutPrefix(cursor, "ov-search:")
	if !ok {
		return cursor, 0, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	var pos listPosition
	if err != nil || json.Unmarshal(data, &pos) != nil || pos.Skip <= 0 {
		return "", 0, errors.New("invalid ACP search cursor")
	}
	return pos.Cursor, pos.Skip, nil
}

func (c *protocolChunk) flushUserText() {
	if c.text.Len() == 0 {
		return
	}
	text := c.text.String()
	c.text.Reset()
	content := c.item.Message.Content
	c.item.Message.Content = append(content, &defs.Content{Kind: defs.ContentText, Text: &defs.TextContent{Text: text}})
}

// attachmentBlock 保留文本块边界，后端可把相邻分片拼为一条用户消息。
func attachmentBlock(file defs.FileRef) sdk.ContentBlock {
	return sdk.TextBlock("\n\n" + prompt.FormatAttachment(file))
}

// restoreUserContent 从整条回放消息的两端移除宿主提示，仅将附件后缀中的文件引用还原。
func restoreUserContent(parts []*defs.Content) []*defs.Content {
	if first := parts[0].Text; first != nil {
		first.Text = prompt.StripChatHistorySummary(first.Text)
		if first.Text == "" && len(parts) > 1 {
			parts = parts[1:]
		}
	}
	if last := parts[len(parts)-1].Text; last != nil {
		last.Text = prompt.StripSessionInfo(last.Text)
	}

	// 内联图片也属于附件后缀；遇到正文后，前面的文本和图片原样保留。
	var suffix []*defs.Content
	for i := len(parts) - 1; i >= 0; i-- {
		part := parts[i]
		if part.Text == nil {
			suffix = append(suffix, part)
			continue
		}
		body, files := prompt.ParseAttachments(part.Text.Text)
		for _, file := range slices.Backward(files) {
			suffix = append(suffix, &defs.Content{Kind: defs.ContentFile, File: &file})
		}
		if body != "" || len(parts) == 1 && len(files) == 0 {
			part.Text.Text = body
			slices.Reverse(suffix)
			return append(parts[:i+1], suffix...)
		}
	}
	slices.Reverse(suffix)
	return suffix
}

// RequestPermission 按优先级自动选择后端提供的许可选项。
func (s *acpAgentSession) RequestPermission(_ context.Context, req sdk.RequestPermissionRequest) (sdk.RequestPermissionResponse, error) {
	// 1. 依次查找允许始终执行和允许执行一次的首个选项。
	for _, kind := range []sdk.PermissionOptionKind{sdk.PermissionOptionKindAllowAlways, sdk.PermissionOptionKindAllowOnce} {
		for _, option := range req.Options {
			if option.Kind == kind {
				return sdk.RequestPermissionResponse{Outcome: sdk.NewRequestPermissionOutcomeSelected(option.OptionId)}, nil
			}
		}
	}
	return sdk.RequestPermissionResponse{Outcome: sdk.NewRequestPermissionOutcomeCancelled()}, nil
}

// 客户端未声明 ACP 文件与终端能力；收到这些 RPC 时按协议回复方法不可用。
func (s *acpAgentSession) ReadTextFile(context.Context, sdk.ReadTextFileRequest) (sdk.ReadTextFileResponse, error) {
	return sdk.ReadTextFileResponse{}, sdk.NewMethodNotFound("fs/read_text_file")
}
func (s *acpAgentSession) WriteTextFile(context.Context, sdk.WriteTextFileRequest) (sdk.WriteTextFileResponse, error) {
	return sdk.WriteTextFileResponse{}, sdk.NewMethodNotFound("fs/write_text_file")
}
func (s *acpAgentSession) CreateTerminal(context.Context, sdk.CreateTerminalRequest) (sdk.CreateTerminalResponse, error) {
	return sdk.CreateTerminalResponse{}, sdk.NewMethodNotFound("terminal/create")
}
func (s *acpAgentSession) KillTerminal(context.Context, sdk.KillTerminalRequest) (sdk.KillTerminalResponse, error) {
	return sdk.KillTerminalResponse{}, sdk.NewMethodNotFound("terminal/kill")
}
func (s *acpAgentSession) TerminalOutput(context.Context, sdk.TerminalOutputRequest) (sdk.TerminalOutputResponse, error) {
	return sdk.TerminalOutputResponse{}, sdk.NewMethodNotFound("terminal/output")
}
func (s *acpAgentSession) ReleaseTerminal(context.Context, sdk.ReleaseTerminalRequest) (sdk.ReleaseTerminalResponse, error) {
	return sdk.ReleaseTerminalResponse{}, sdk.NewMethodNotFound("terminal/release")
}
func (s *acpAgentSession) WaitForTerminalExit(context.Context, sdk.WaitForTerminalExitRequest) (sdk.WaitForTerminalExitResponse, error) {
	return sdk.WaitForTerminalExitResponse{}, sdk.NewMethodNotFound("terminal/wait_for_exit")
}
