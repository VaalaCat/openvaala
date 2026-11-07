package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"math"
	"net/http"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/bmatcuk/doublestar/v4"
	"golang.org/x/image/draw"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/terminal"
	"github.com/VaalaCat/openvaala/pkg/utils/jsonutil"
)

// normalizePath 校验并规范相对路径。
func normalizePath(value string, allowRoot bool) (string, error) {
	// 1. 拒绝空值、反斜杠、NUL 和绝对路径，清理路径后拒绝越出根目录的跳转；allowRoot 为 false 时也拒绝根目录本身。
	if value == "" || strings.Contains(value, `\`) || strings.ContainsRune(value, 0) || path.IsAbs(value) {
		return "", errors.New("tool path must be relative to the root directory")
	}
	cleaned := path.Clean(value)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || (!allowRoot && cleaned == ".") {
		return "", errors.New("tool path escapes the root directory")
	}
	return cleaned, nil
}

// normalizeBashDirectory 把 bash 工作目录转成根目录内的相对路径。
func normalizeBashDirectory(rootDirectory, value string) (string, error) {
	// 1. 绝对路径先转为相对路径，再校验是否留在根目录内。
	relative := value
	if filepath.IsAbs(value) {
		var err error
		if relative, err = filepath.Rel(rootDirectory, value); err != nil {
			return "", normalizeBashDirectoryError(rootDirectory, value, err)
		}
	}
	normalized, err := normalizePath(relative, true)
	if err != nil {
		return "", normalizeBashDirectoryError(rootDirectory, value, err)
	}
	return normalized, nil
}

// normalizeBashDirectoryError 创建包含工作目录修正提示的错误。
func normalizeBashDirectoryError(rootDirectory, value string, cause error) error {
	// 1. 附带原路径、根目录及原错误。
	return fmt.Errorf("invalid bash cwd %q for workspace root %q: %w; omit cwd or use \".\" for the root, or a relative directory inside it", value, rootDirectory, cause)
}

// resolveReadPath 解析文件读取位置的绝对路径。
func resolveReadPath(rootDirectory, value string) (string, error) {
	// 1. 拒绝空路径和 NUL，清理绝对路径，相对路径按工作区根目录展开。
	if value == "" || strings.ContainsRune(value, 0) {
		return "", errors.New("read_file path must not be empty or contain NUL")
	}
	if filepath.IsAbs(value) {
		return filepath.Clean(value), nil
	}
	return filepath.Join(rootDirectory, value), nil
}

// encodeToolResult 编码工具正文并附带调用 ID 和错误。
func encodeToolResult(callID string, content any, toolError *defs.ToolError) *executor.ToolExecutionResult {
	// 1. 把正文编码为不转义 HTML 字符的 JSON，组装执行结果。
	encoded, err := jsonutil.MarshalWithoutHTMLEscape(content)
	if err != nil {
		panic(err)
	}
	return &executor.ToolExecutionResult{Result: &defs.ToolResult{
		CallID:  callID,
		Content: encoded,
		Error:   toolError,
	}}
}

// newInvalidArgumentsResult 返回参数无效的工具结果。
func newInvalidArgumentsResult(callID string, err error) *executor.ToolExecutionResult {
	// 1. 把解析、校验或路径错误写成 invalid_arguments。
	return encodeToolResult(callID, nil, &defs.ToolError{
		Code:    "invalid_arguments",
		Message: err.Error(),
	})
}

// newReadFailedResult 返回文件或图片读取失败的工具结果。
func newReadFailedResult(callID string, err error) *executor.ToolExecutionResult {
	// 1. 把错误写成 read_failed。
	return encodeToolResult(callID, nil, &defs.ToolError{
		Code:    "read_failed",
		Message: err.Error(),
	})
}

// newSearchFailedResult 返回 glob、grep 失败的工具结果：调用取消或超时、path 参数不对、搜索命令或读取失败分别用不同的错误码。
func newSearchFailedResult(ctx context.Context, callID string, err error) *executor.ToolExecutionResult {
	// 1. 取消优先；path 参数不对写成 invalid_arguments；其他写成 search_failed。
	var invalidPath *invalidSearchPathError
	switch {
	case ctx.Err() != nil:
		return encodeContextError(callID, ctx.Err())
	case errors.As(err, &invalidPath):
		return newInvalidArgumentsResult(callID, err)
	}
	return encodeToolResult(callID, nil, &defs.ToolError{
		Code:    "search_failed",
		Message: err.Error(),
	})
}

// encodeContextError 把取消或超时编码为工具错误。
func encodeContextError(callID string, err error) *executor.ToolExecutionResult {
	// 1. 按 DeadlineExceeded 选择 timeout，其他错误使用 canceled。
	code := "canceled"
	if errors.Is(err, context.DeadlineExceeded) {
		code = "timeout"
	}
	return encodeToolResult(callID, nil, &defs.ToolError{
		Code:    code,
		Message: err.Error(),
	})
}

// encodeCommandResult 把命令状态及输出页编码为工具结果。
func encodeCommandResult(callID string, output terminal.TerminalOutput) *executor.ToolExecutionResult {
	// 1. 填入命令状态、退出码、输出和续读信息。
	result := &CommandResult{
		CommandID:  output.Info.ID,
		Status:     string(output.Info.Status),
		ExitCode:   output.Info.ExitCode,
		StopReason: string(output.Info.StopReason),
		Output:     output.Text,
		NextCursor: output.NextCursor,
		HasMore:    output.HasMore,
		Truncated:  output.Truncated,
	}
	// 2. failed 状态附带错误文本，工具结果同步本页截断标记。
	if output.Info.Err != nil {
		result.Error = output.Info.Err.Error()
	}
	toolResult := encodeToolResult(callID, result, nil)
	toolResult.Result.Truncated = output.Truncated
	return toolResult
}

// ParseCommandResult 解析 bash 或 read_command_output 的结果正文；不是命令结果时第二个返回值为 false。
func ParseCommandResult(content json.RawMessage) (CommandResult, bool) {
	// 1. 解码命令结果，用指针区分缺失的 output，缺少 status 或 output 时返回 false。
	var parsed struct {
		CommandResult
		// Output 比 CommandResult.Output 浅一层，解码时由它接收 output，用指针区分没有 output 的结果。
		Output *string `json:"output"`
	}
	if json.Unmarshal(content, &parsed) != nil || parsed.Status == "" || parsed.Output == nil {
		return CommandResult{}, false
	}
	parsed.CommandResult.Output = *parsed.Output
	return parsed.CommandResult, true
}

// IsFailed 命令是否失败？
func (r CommandResult) IsFailed() bool {
	// 1. failed 或非零退出算失败，stop_command 主动停止不算；超时停止仍算失败。
	if r.Status == string(terminal.TerminalStatusFailed) {
		return true
	}
	isStoppedByTool := r.StopReason == string(terminal.TerminalStoppedByRequest)
	return r.ExitCode != nil && *r.ExitCode != 0 && !isStoppedByTool
}

// newStopCommandResult 提取停止命令后的最终状态。
func newStopCommandResult(info terminal.TerminalInfo) *stopCommandResult {
	// 1. 复制命令 ID、状态、退出码和停止原因。
	return &stopCommandResult{
		CommandID:  info.ID,
		Status:     string(info.Status),
		ExitCode:   info.ExitCode,
		StopReason: string(info.StopReason),
	}
}

// newCommandInfo 提取命令列表中的单条信息。
func newCommandInfo(info terminal.TerminalInfo) commandInfo {
	// 1. 复制命令、绝对工作目录、交互标记和运行状态。
	return commandInfo{
		CommandID:   info.ID,
		Command:     info.Command,
		CWD:         info.Directory,
		Interactive: info.Interactive,
		Status:      string(info.Status),
		ExitCode:    info.ExitCode,
		StopReason:  string(info.StopReason),
	}
}

// newStartTerminalErrorResult 把命令启动错误转成工具错误。
func newStartTerminalErrorResult(ctx context.Context, callID string, err error) *executor.ToolExecutionResult {
	// 1. 区分数量上限、集合停止和调用取消，其他错误记为 command_start_failed。
	switch {
	case errors.Is(err, terminal.ErrTerminalLimitReached):
		return encodeToolResult(callID, nil, &defs.ToolError{
			Code:    "command_limit_reached",
			Message: err.Error(),
		})
	case errors.Is(err, terminal.ErrTerminalManagerStopped):
		return encodeToolResult(callID, nil, &defs.ToolError{
			Code:    "terminal_stopped",
			Message: err.Error(),
		})
	case ctx.Err() != nil:
		return encodeContextError(callID, ctx.Err())
	}
	return encodeToolResult(callID, nil, &defs.ToolError{
		Code:    "command_start_failed",
		Message: err.Error(),
	})
}

// newCommandNotFoundResult 返回命令不属于本 Session、本环境或已被淘汰的错误。
func newCommandNotFoundResult(callID, commandID string) *executor.ToolExecutionResult {
	// 1. 用命令编号生成 command_not_found 结果。
	return encodeToolResult(callID, nil, &defs.ToolError{
		Code:    "command_not_found",
		Message: fmt.Sprintf("command %s is not available in this session and executor", commandID),
	})
}

// getImageMIMEType 识别受支持的图片 MIME 类型。
func getImageMIMEType(data []byte) string {
	// 1. 按文件头检测 MIME，返回 PNG、JPEG、GIF 或 WebP 类型，其他格式返回空字符串。
	switch mimeType := http.DetectContentType(data); mimeType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return mimeType
	}
	return ""
}

// jpegQuality 是压缩图片时的 JPEG 质量。
const jpegQuality = 80

// maxImageEdge 是 Anthropic 接受的图片长边上限。
const maxImageEdge = 8000

// compressImage 缩小并编码图片，使字节数和长边不超过上限。
func compressImage(source image.Image, maxBytes int) ([]byte, error) {
	// 1. 长边超限时按比例缩小目标宽高。
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if longEdge := max(width, height); longEdge > maxImageEdge {
		width = max(1, width*maxImageEdge/longEdge)
		height = max(1, height*maxImageEdge/longEdge)
	}
	// 2. 每轮从原图缩放并编码为 JPEG；仍超限时按字节比例继续缩小。
	for {
		encoded, err := encodeJPEG(source, width, height)
		if err != nil {
			return nil, err
		}
		if len(encoded) <= maxBytes {
			return encoded, nil
		}
		ratio := math.Sqrt(float64(maxBytes) / float64(len(encoded)))
		width = int(float64(width) * ratio)
		height = int(float64(height) * ratio)
		if width < 1 || height < 1 {
			return nil, fmt.Errorf("image cannot be compressed below %d KB", (maxBytes+1023)>>10)
		}
	}
}

// encodeJPEG 把图片缩放后铺在白底上，编码为 JPEG。
func encodeJPEG(source image.Image, width, height int) ([]byte, error) {
	// 1. 创建白底画布，缩放原图并按固定质量编码。
	canvas := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(canvas, canvas.Bounds(), image.White, image.Point{}, draw.Src)
	draw.CatmullRom.Scale(canvas, canvas.Bounds(), source, source.Bounds(), draw.Over, nil)
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, canvas, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return nil, err
	}
	return encoded.Bytes(), nil
}

// parsePatchChanges 校验补丁大小、语法和路径，返回文件变更。
func parsePatchChanges(patch string) ([]*patchChange, error) {
	// 1. 检查大小，解析补丁并规范变更路径。
	if len(patch) > maxPatchBytes {
		return nil, ErrInvalidPatch
	}
	changes, err := parsePatch(patch)
	if err != nil {
		return nil, err
	}
	if err := validatePatchPaths(changes); err != nil {
		return nil, err
	}
	return changes, nil
}

// parsePatch 解析 Begin Patch 文本中的文件变更。
func parsePatch(input string) ([]*patchChange, error) {
	// 1. 统一换行，校验补丁起始标记。
	input = strings.ReplaceAll(input, "\r\n", "\n")
	if strings.ContainsRune(input, '\r') {
		return nil, ErrInvalidPatch
	}
	lines := strings.Split(input, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) < 2 || lines[0] != "*** Begin Patch" {
		return nil, ErrInvalidPatch
	}
	// 2. 按新增、更新、删除读取文件段，检查文件数、更新段数和结束标记。
	var changes []*patchChange
	sectionCount := 0
	for index := 1; index < len(lines); {
		if lines[index] == "*** End Patch" {
			if index != len(lines)-1 || len(changes) == 0 {
				return nil, ErrInvalidPatch
			}
			return changes, nil
		}
		if len(changes) >= maxPatchChanges {
			return nil, ErrInvalidPatch
		}
		switch {
		case strings.HasPrefix(lines[index], "*** Add File: "):
			change, next, err := parseAdd(lines, index)
			if err != nil {
				return nil, err
			}
			changes = append(changes, change)
			index = next
		case strings.HasPrefix(lines[index], "*** Update File: "):
			change, next, err := parseUpdate(lines, index)
			if err != nil {
				return nil, err
			}
			if len(change.sections) > maxPatchSections-sectionCount {
				return nil, ErrInvalidPatch
			}
			sectionCount += len(change.sections)
			changes = append(changes, change)
			index = next
		case strings.HasPrefix(lines[index], "*** Delete File: "):
			filePath := strings.TrimSpace(strings.TrimPrefix(lines[index], "*** Delete File: "))
			if filePath == "" || index+1 >= len(lines) || !strings.HasPrefix(lines[index+1], "*** ") {
				return nil, ErrInvalidPatch
			}
			changes = append(changes, &patchChange{
				kind: patchDelete,
				path: filePath,
			})
			index++
		default:
			return nil, ErrInvalidPatch
		}
	}
	return nil, ErrInvalidPatch
}

// parseAdd 解析一个新增文件段。
func parseAdd(lines []string, header int) (*patchChange, int, error) {
	// 1. 读取非空文件路径，收集以加号开头的内容行。
	filePath := strings.TrimSpace(strings.TrimPrefix(lines[header], "*** Add File: "))
	if filePath == "" {
		return nil, 0, ErrInvalidPatch
	}
	change := &patchChange{
		kind: patchAdd,
		path: filePath,
	}
	index := header + 1
	for index < len(lines) && !strings.HasPrefix(lines[index], "*** ") {
		if !strings.HasPrefix(lines[index], "+") {
			return nil, 0, ErrInvalidPatch
		}
		change.content = append(change.content, lines[index][1:])
		index++
	}
	if index >= len(lines) {
		return nil, 0, ErrInvalidPatch
	}
	return change, index, nil
}

// parseUpdate 解析一个更新文件的各段上下文与增删行。
func parseUpdate(lines []string, header int) (*patchChange, int, error) {
	// 1. 读取文件路径并创建更新记录。
	filePath := strings.TrimSpace(strings.TrimPrefix(lines[header], "*** Update File: "))
	if filePath == "" {
		return nil, 0, ErrInvalidPatch
	}
	change := &patchChange{
		kind: patchUpdate,
		path: filePath,
	}
	// 2. 按 @@ 读取更新段，分别收集旧行、新行和原始 diff，检查段内容。
	index := header + 1
	for index < len(lines) && !strings.HasPrefix(lines[index], "*** ") {
		if !strings.HasPrefix(lines[index], "@@") {
			return nil, 0, ErrInvalidPatch
		}
		section := &patchSection{diffLines: []string{lines[index]}}
		index++
		lineCount := 0
		for index < len(lines) && !strings.HasPrefix(lines[index], "@@") && !strings.HasPrefix(lines[index], "*** ") {
			line := lines[index]
			if line == "" {
				return nil, 0, ErrInvalidPatch
			}
			switch line[0] {
			case ' ':
				section.oldLines = append(section.oldLines, line[1:])
				section.newLines = append(section.newLines, line[1:])
			case '-':
				section.oldLines = append(section.oldLines, line[1:])
				section.deletedLineCount++
			case '+':
				section.newLines = append(section.newLines, line[1:])
				section.addedLineCount++
			default:
				return nil, 0, ErrInvalidPatch
			}
			section.diffLines = append(section.diffLines, line)
			lineCount++
			index++
		}
		if lineCount == 0 || len(section.oldLines) == 0 {
			return nil, 0, ErrInvalidPatch
		}
		change.sections = append(change.sections, section)
	}
	if len(change.sections) == 0 || index >= len(lines) {
		return nil, 0, ErrInvalidPatch
	}
	return change, index, nil
}

// applySections 按顺序应用更新段，返回新内容和各段在原文件中的起始行号。
func applySections(content string, sections []*patchSection) (string, []int, error) {
	// 1. 顺序查找完整行上下文，记录替换区间和起始行号；找不到时拒绝补丁。
	replacements := make([]*patchReplacement, 0, len(sections))
	startLines := make([]int, 0, len(sections))
	searchCursor := 0
	outputBytes := len(content)
	for _, section := range sections {
		oldText := strings.Join(section.oldLines, "\n")
		newText := strings.Join(section.newLines, "\n")
		index := findLineBlock(content, oldText, searchCursor)
		if index < 0 {
			return "", nil, ErrPatchRejected
		}
		startLines = append(startLines, strings.Count(content[:index], "\n")+1)
		end := index + len(oldText)
		if len(section.newLines) == 0 && end < len(content) && content[end] == '\n' {
			end++
		}
		replacements = append(replacements, &patchReplacement{
			start: index,
			end:   end,
			text:  newText,
		})
		searchCursor = end
		outputBytes += len(newText) - (end - index)
	}
	// 2. 检查结果大小，再按替换区间拼接更新后的全文。
	if outputBytes > maxPatchedFileBytes {
		return "", nil, ErrPatchRejected
	}
	var updated strings.Builder
	updated.Grow(outputBytes)
	written := 0
	for _, replacement := range replacements {
		updated.WriteString(content[written:replacement.start])
		updated.WriteString(replacement.text)
		written = replacement.end
	}
	updated.WriteString(content[written:])
	return updated.String(), startLines, nil
}

// findLineBlock 从指定位置查找完整行文本块。
func findLineBlock(content, block string, start int) int {
	// 1. 逐次匹配文本，检查起点和终点是否在行首或行尾，未找到返回 -1。
	for start <= len(content) {
		relative := strings.Index(content[start:], block)
		if relative < 0 {
			return -1
		}
		index := start + relative
		end := index + len(block)
		if (index == 0 || content[index-1] == '\n') && (end == len(content) || content[end] == '\n') {
			return index
		}
		start = index + 1
	}
	return -1
}

// validatePatchPaths 校验并规范补丁的变更路径。
func validatePatchPaths(changes []*patchChange) error {
	// 1. 逐个校验相对路径，重复路径或父子路径冲突时返回 ErrInvalidPatch，否则保存规范路径。
	seen := make(map[string]struct{}, len(changes))
	for _, change := range changes {
		logical, err := normalizePath(change.path, false)
		if err != nil {
			return err
		}
		if _, exists := seen[logical]; exists {
			return ErrInvalidPatch
		}
		for existing := range seen {
			if strings.HasPrefix(existing, logical+"/") || strings.HasPrefix(logical, existing+"/") {
				return ErrInvalidPatch
			}
		}
		seen[logical] = struct{}{}
		change.path = logical
	}
	return nil
}

// quoteShellArgument 把参数用单引号包起来，交给 bash 时按字面值处理。
func quoteShellArgument(value string) string {
	// 1. 单引号里不能再有单引号：先结束引号，补一个转义的单引号，再重新开始引号。
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// limitScriptLineBytes 让脚本在远端把每行输出截到 maxScriptLineBytes 字节，并保留管道里搜索命令的退出码。
func limitScriptLineBytes(command string) string {
	// 1. pipefail 让没有匹配（1）和出错（2）照常传回来，cut 只截长度。
	return fmt.Sprintf("set -o pipefail; %s | cut -b 1-%d", command, maxScriptLineBytes)
}

// portableClassReplacements 是 \d \D \w \W \s \S 在 POSIX 扩展正则里的等价写法。
var portableClassReplacements = map[byte]string{
	'd': "[0-9]",
	'D': "[^0-9]",
	'w': "[[:alnum:]_]",
	'W': "[^[:alnum:]_]",
	's': "[[:space:]]",
	'S': "[^[:space:]]",
}

// translatePortablePattern 把方括号外的 \d \D \w \W \s \S 换成 POSIX 扩展正则也认识的字符类，其余内容保持不变。
func translatePortablePattern(pattern string) string {
	// 1. 逐字节扫描：方括号内原样复制；方括号外的反斜杠转义，属于六个简写时替换，否则连同下一个字符原样复制。
	var translated strings.Builder
	isInBracket := false
	for index := 0; index < len(pattern); index++ {
		char := pattern[index]
		switch {
		case isInBracket:
			if char == '[' && index+1 < len(pattern) && pattern[index+1] == ':' {
				if end := strings.Index(pattern[index+2:], ":]"); end >= 0 {
					translated.WriteString(pattern[index : index+end+4])
					index += end + 3
					continue
				}
			}
			isInBracket = char != ']'
			translated.WriteByte(char)
		case char == '\\' && index+1 < len(pattern):
			index++
			if replacement, ok := portableClassReplacements[pattern[index]]; ok {
				translated.WriteString(replacement)
				continue
			}
			translated.WriteByte(char)
			translated.WriteByte(pattern[index])
		case char == '[':
			isInBracket = true
			translated.WriteByte(char)
			// 方括号开头的 ^ 和紧跟着的 ] 都属于字符集本身，不结束方括号。
			if index+1 < len(pattern) && pattern[index+1] == '^' {
				index++
				translated.WriteByte('^')
			}
			if index+1 < len(pattern) && pattern[index+1] == ']' {
				index++
				translated.WriteByte(']')
			}
		default:
			translated.WriteByte(char)
		}
	}
	return translated.String()
}

// isExtensionGroupPattern 正则里是否有 (? 开头的分组或内联标记？转义的 \( 和方括号里的 ( 不算。
func isExtensionGroupPattern(pattern string) bool {
	// 1. 跳过转义字符和方括号字符集，其余位置遇到 (? 就返回 true。
	isInBracket := false
	for index := 0; index < len(pattern); index++ {
		switch char := pattern[index]; {
		case char == '\\':
			index++
		case isInBracket:
			isInBracket = char != ']'
		case char == '[':
			isInBracket = true
		case char == '(' && index+1 < len(pattern) && pattern[index+1] == '?':
			return true
		}
	}
	return false
}

// isHiddenPath 相对路径里是否有以 . 开头的目录或文件名？路径中的 . 本身不算。
func isHiddenPath(relativePath string) bool {
	// 1. 逐段检查，只要有一段以 . 开头且不是 . 就是隐藏路径。
	for segment := range strings.SplitSeq(relativePath, "/") {
		if segment != "." && strings.HasPrefix(segment, ".") {
			return true
		}
	}
	return false
}

// isSearchGlobMatch 相对路径是否匹配搜索通配符？不含 / 的通配符按文件名匹配任意深度（*.go 匹配 sub/b.go），含 / 的按整个相对路径匹配。
func isSearchGlobMatch(pattern, relativePath string) bool {
	// 1. 不含 / 时只取文件名，再用 doublestar 匹配；通配符已在调用前校验过。
	if !strings.Contains(pattern, "/") {
		relativePath = path.Base(relativePath)
	}
	return doublestar.MatchUnvalidated(pattern, relativePath)
}

// resolveSearchPath 把 glob、grep 的 path 参数转成绝对路径；省略时是工作区根目录。
func resolveSearchPath(rootDirectory, value string) string {
	// 1. 空值取根目录，绝对路径清理后直接用，相对路径按根目录展开。
	switch {
	case value == "":
		return rootDirectory
	case filepath.IsAbs(value):
		return filepath.Clean(value)
	}
	return filepath.Join(rootDirectory, value)
}

// getDisplayPath 把绝对路径写成相对工作区根目录的路径；在根目录之外时保留绝对路径。
func getDisplayPath(rootDirectory, absolutePath string) string {
	// 1. 能写成不越出根目录的相对路径就用相对路径。
	relative, err := filepath.Rel(rootDirectory, absolutePath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, "../") {
		return absolutePath
	}
	return relative
}

// truncateMatchText 把匹配行截成最多 maxMatchTextChars 个字符，被截断时末尾加 …。
func truncateMatchText(line []byte) string {
	// 1. 去掉行尾换行；按字符数找到截断位置，不把超长行整行转成 rune 切片。
	line = bytes.TrimRight(line, "\r\n")
	offset := 0
	for count := 0; count < maxMatchTextChars && offset < len(line); count++ {
		_, size := utf8.DecodeRune(line[offset:])
		offset += size
	}
	if offset >= len(line) {
		return string(line)
	}
	return string(line[:offset]) + "…"
}
