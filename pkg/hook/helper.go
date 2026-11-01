package hook

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/workspace"
)

const (
	// maxOutputBytes 是命令 stdout、stderr 各自最多保留的字节数，超出部分丢弃。
	maxOutputBytes = 64 << 10
	// truncatedNotice 跟在被截断的输出后面。
	truncatedNotice = "[output truncated]"
	// scriptCacheDirectory 是脚本在目标机器上的缓存目录，相对那台机器上用户的主目录。
	scriptCacheDirectory = ".cache/openvaala/hooks"
	// scriptHashDirectoryLength 是缓存目录下，以脚本哈希命名的子目录取哈希十六进制的前几位。
	scriptHashDirectoryLength = 16
	// pluginCacheDirectory 是插件目录在非 local Executor 上的缓存目录，相对那台机器上用户的主目录；
	// 其下每个插件目录以 <插件名>-<内容哈希前 pluginHashLength 位> 命名。
	pluginCacheDirectory = ".cache/openvaala/plugins"
	pluginHashLength     = 16
)

// pluginSkippedNames 是计算哈希和复制插件时共同跳过的条目名。
var pluginSkippedNames = []string{".git"}

// ── 事件匹配 ──

// getMatchedDefinitions 选出命中事件的 hook，按 DisplayOrder 返回。
func getMatchedDefinitions(definitions []*HookDefinition, event *HookEvent) []*HookDefinition {
	// 1. 工具事件匹配工具名，SessionStart 匹配来源，子 Agent 事件匹配 Agent 名，UserPromptSubmit 和 Stop 不检查 matcher。
	var matched []*HookDefinition
	for _, definition := range definitions {
		if definition.Event != event.Name {
			continue
		}
		switch event.Name {
		case defs.HookEventPreToolUse, defs.HookEventPostToolUse:
			if !definition.Matcher.IsMatched(event.ToolCall.Name) {
				continue
			}
		case defs.HookEventSessionStart:
			if !definition.Matcher.IsMatched(event.Source) {
				continue
			}
		case defs.HookEventSubagentStart, defs.HookEventSubagentStop:
			if !definition.Matcher.IsMatched(event.Session.AgentName) {
				continue
			}
		}
		matched = append(matched, definition)
	}
	// 2. 按 DisplayOrder 稳定排序。
	slices.SortStableFunc(matched, func(a, b *HookDefinition) int {
		return cmp.Compare(a.DisplayOrder, b.DisplayOrder)
	})
	return matched
}

// ── 命令的输入输出 ──

// newCommandInput 把事件编码为命令标准输入的 JSON。
func newCommandInput(request *HookEventRequest) ([]byte, error) {
	// 1. 填入事件名、对话 ID、Turn ID、工作目录，子 Session 另带 Agent ID 和名字。
	event := request.Event
	fields := map[string]any{
		"hook_event_name": event.Name,
		"session_id":      event.Session.ConversationID,
		"turn_id":         event.TurnID,
		"cwd":             request.Workspace.GetRootDirectory(),
	}
	if event.Session.ParentSessionID != "" {
		fields["agent_id"] = event.Session.ID
		fields["agent_type"] = event.Session.AgentName
	}
	// 2. 按事件补上来源、用户输入、工具调用和结果，或 stop_hook_active 和最后一条回复。
	switch event.Name {
	case defs.HookEventSessionStart:
		fields["source"] = event.Source
	case defs.HookEventUserPromptSubmit:
		fields["prompt"] = event.Prompt
	case defs.HookEventPreToolUse, defs.HookEventPostToolUse:
		fields["tool_name"] = event.ToolCall.Name
		fields["tool_input"] = getJSONOrText([]byte(event.ToolCall.Arguments))
		fields["tool_use_id"] = event.ToolCall.CallID
		if event.Name == defs.HookEventPostToolUse {
			fields["tool_response"] = getJSONOrText(event.ToolResult.Content)
		}
	case defs.HookEventStop, defs.HookEventSubagentStop:
		fields["stop_hook_active"] = event.IsStopHookActive
		fields["last_assistant_message"] = event.LastAssistantMessage
	}

	// 3. 编码 JSON，<、> 和 & 保持原样不转义。
	var input bytes.Buffer
	encoder := json.NewEncoder(&input)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(fields); err != nil {
		return nil, err
	}
	return input.Bytes(), nil
}

// getJSONOrText 内容是合法 JSON 时原样嵌入，否则当作字符串。
func getJSONOrText(raw []byte) any {
	// 1. 检查 JSON 是否合法。
	if json.Valid(raw) {
		return json.RawMessage(raw)
	}
	return string(raw)
}

// parseCommandOutput 解析退出码 0 时 stdout 里的 JSON。
func parseCommandOutput(text string) (*commandOutput, error) {
	// 1. 解码成 commandOutput。
	var output commandOutput
	if err := json.Unmarshal([]byte(text), &output); err != nil {
		return nil, err
	}
	return &output, nil
}

// isBlockableEvent 这个事件能不能被 hook 拦下？
func isBlockableEvent(name defs.HookEventName) bool {
	// 1. SessionStart 和 SubagentStart 不能，其他都能。
	return name != defs.HookEventSessionStart && name != defs.HookEventSubagentStart
}

// isPlainTextContextEvent 这个事件是否把纯文本 stdout 当作附加上下文？
func isPlainTextContextEvent(name defs.HookEventName) bool {
	// 1. 只有 SessionStart、SubagentStart 和 UserPromptSubmit 是。
	return name == defs.HookEventSessionStart || name == defs.HookEventSubagentStart || name == defs.HookEventUserPromptSubmit
}

// limitedBuffer 保存输出的前 maxOutputBytes 字节和截断标记，超出部分丢弃但仍报告写入成功。
type limitedBuffer struct {
	content     bytes.Buffer
	isTruncated bool
}

// Write 写入输出，超过上限的部分丢弃，返回值总是完整长度。
func (b *limitedBuffer) Write(data []byte) (int, error) {
	// 1. 保留前 64 KiB，超出部分丢弃并标记截断。
	written := len(data)
	if remaining := maxOutputBytes - b.content.Len(); len(data) > remaining {
		data = data[:remaining]
		b.isTruncated = true
	}
	b.content.Write(data)
	return written, nil
}

// getText 返回保留下来的输出文本。
func (b *limitedBuffer) getText() string {
	// 1. 去掉首尾空白，被截断过时在末尾加上截断提示。
	text := strings.TrimSpace(b.content.String())
	if b.isTruncated {
		text += "\n" + truncatedNotice
	}
	return text
}

// ── 运行结果 ──

// formatExitError 生成命令以非零退出码结束时的错误说明。
func formatExitError(code int, stderr string) string {
	// 1. 写入退出码，有 stderr 时附在后面。
	message := fmt.Sprintf("exit status %d", code)
	if stderr == "" {
		return message
	}
	return message + ": " + stderr
}

// ── 配置展示 ──

// getHandler 生成 hook 列表和运行记录里显示的命令文本。
func getHandler(definition *HookDefinition) string {
	// 1. command 返回原文，script 返回路径加空格连接的参数。
	if definition.ScriptPath == "" {
		return definition.Command
	}
	return strings.Join(append([]string{definition.ScriptPath}, definition.Args...), " ")
}

// getDefinitionHash 计算 hook 定义的 SHA-256 十六进制哈希。
func getDefinitionHash(definition *HookDefinition) string {
	// 1. 事件、matcher、命令、脚本、超时、on_failure 和参数用 NUL 分隔后计算哈希。
	fields := []string{
		string(definition.Event),
		definition.Matcher.String(),
		definition.Command,
		definition.ScriptPath,
		strconv.FormatInt(int64(definition.Timeout), 10),
		strconv.FormatBool(definition.IsBlockedOnFailure),
	}
	fields = append(fields, definition.Args...)
	sum := sha256.Sum256([]byte(strings.Join(fields, "\x00")))
	return hex.EncodeToString(sum[:])
}

// ── 脚本拷贝 ──

// getFileHash 计算文件权限和内容的 SHA-256 哈希。
func getFileHash(ctx context.Context, fileWorkspace workspace.Workspace, filePath string) (string, error) {
	// 1. 读取文件权限并打开文件。
	metadata, err := fileWorkspace.GetMetadata(ctx, filePath, true)
	if err != nil {
		return "", err
	}
	reader, err := fileWorkspace.OpenFileReader(ctx, filePath)
	if err != nil {
		return "", err
	}
	defer reader.Close()
	// 2. 依次哈希权限和文件内容，返回十六进制哈希。
	hasher := sha256.New()
	fmt.Fprintf(hasher, "%04o\x00", metadata.Mode.Perm())
	if _, err := io.Copy(hasher, reader); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// getScriptTargetPath 生成脚本在目标机器上的缓存路径。
func getScriptTargetPath(homeDirectory, scriptHash, scriptPath string) string {
	// 1. 用 / 拼接主目录、缓存目录、哈希前 16 位和本机文件名。
	return path.Join(homeDirectory, scriptCacheDirectory, scriptHash[:scriptHashDirectoryLength], filepath.Base(scriptPath))
}

// quoteScriptCommand 生成带 shell 转义的脚本命令行。
func quoteScriptCommand(targetPath string, args []string) string {
	// 1. 分别转义脚本路径和每个参数，再用空格连起来。
	words := []string{workspace.QuoteShellWord(targetPath)}
	for _, argument := range args {
		words = append(words, workspace.QuoteShellWord(argument))
	}
	return strings.Join(words, " ")
}

// ── 插件目录 ──

// getPluginHash 计算插件目录的 SHA-256 内容哈希。
func getPluginHash(ctx context.Context, source workspace.Workspace, directory string) (string, error) {
	// 1. 按相对路径列出文件，跳过 .git 和指向目录的链接，指向文件的链接按文件算。
	files, err := workspace.ListDirectoryFiles(ctx, source, directory, pluginSkippedNames)
	if err != nil {
		return "", err
	}
	// 2. 依次哈希相对路径、是否目录、权限，文件再加上内容，返回十六进制哈希。
	hasher := sha256.New()
	for _, file := range files {
		fmt.Fprintf(hasher, "%s\x00%t\x00%04o\x00", file.RelativePath, file.IsDirectory, file.Mode.Perm())
		if file.IsDirectory {
			continue
		}
		reader, err := source.OpenFileReader(ctx, filepath.Join(directory, file.RelativePath))
		if err != nil {
			return "", err
		}
		_, err = io.Copy(hasher, reader)
		reader.Close()
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// getPluginCommandLine 生成目标机器上执行插件 hook 的命令行。
func getPluginCommandLine(root string, definition *HookDefinition) string {
	// 1. script 换成目标插件根目录下同样的相对路径并转义参数，command 保留原文。
	commandLine := definition.Command
	if definition.ScriptPath != "" {
		relativePath := strings.TrimPrefix(definition.ScriptPath, definition.Plugin.Directory)
		commandLine = quoteScriptCommand(path.Join(root, filepath.ToSlash(relativePath)), definition.Args)
	}
	// 2. 在命令前导出 OPENVAALA_PLUGIN_ROOT。
	return "export OPENVAALA_PLUGIN_ROOT=" + workspace.QuoteShellWord(root) + "; " + commandLine
}
