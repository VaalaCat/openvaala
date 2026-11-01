package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"io"
	"strings"
	"unicode/utf8"

	// 注册 image.Decode 能解码的格式；jpeg 在 helper.go 中引用。
	_ "image/gif"
	_ "image/png"

	_ "golang.org/x/image/webp"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/prompt"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/workspace"
)

// readFileToolImpl 是 read_file 工具：从工作区按行分页读 UTF-8 文本，或把整张图片交给模型。
type readFileToolImpl struct {
	*executor.BaseTool[readFileArguments]

	opt *ReadFileToolOpt
}

// readFileArguments 是 read_file 的调用参数。
type readFileArguments struct {
	Path   string `json:"path" jsonschema:"description=文件路径：相对路径以工作区为基准，也可使用绝对路径,minLength=1"`
	Offset int64  `json:"offset,omitempty" jsonschema:"description=起始行号（从 1 开始）,minimum=1,default=1"`
	Limit  int    `json:"limit,omitempty" jsonschema:"description=最多读取的行数；单次正文最多 64 KiB,minimum=1,maximum=2000,default=200"`
}

// readFileResult 保存带行号的文本、起始和续读行号、原文件字节数及是否读到末尾。
type readFileResult struct {
	Path       string `json:"path"`
	Offset     int64  `json:"offset"`
	NextOffset int64  `json:"next_offset"`
	Size       int64  `json:"size"`
	Content    string `json:"content"`
	Encoding   string `json:"encoding"`
	EOF        bool   `json:"eof"`
	// Truncated 是否因正文达到字节数上限而提前结束这一页？
	Truncated bool `json:"truncated,omitempty"`
}

// readImageResult 是读到图片时的结果正文，类型、大小、宽高都是原文件的。
type readImageResult struct {
	Path     string `json:"path"`
	MimeType string `json:"mime_type"`
	Size     int64  `json:"size"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	// Compressed 交给模型的图片是否压缩过？压缩过的图片和原文件不同；没压缩时不输出这个字段。
	Compressed bool `json:"compressed,omitempty"`
}

// defaultMaxImageBytes 是交给模型的单张图片上限的默认值，和配置 tools.max_image_kb 的默认值一致。
const defaultMaxImageBytes = 500 << 10

const (
	// maxReadFileBytes 是每页带行号文本的字节数上限。
	maxReadFileBytes = 64 << 10
	// maxImageFileBytes 是图片原文件的读取上限，超过时返回错误。
	maxImageFileBytes = 20 << 20
	// maxImagePixels 是图片解码前检查的像素总数上限，值为 8000×8000。
	maxImagePixels = maxImageEdge * maxImageEdge
)

// ReadFileToolOpt 保存读取工作区和交给模型的图片大小上限。
type ReadFileToolOpt struct {
	// Workspace 是读取文件的工作区；相对路径以它的根目录为基准。
	Workspace workspace.Workspace
	// MaxImageBytes 是交给模型的单张图片上限，超过就压缩；必须大于 0。
	MaxImageBytes int
}

// NewReadFileToolOpt 保存读取工作区和默认图片大小上限。
func NewReadFileToolOpt(toolWorkspace workspace.Workspace) *ReadFileToolOpt {
	// 1. 创建配置，单张图片默认上限为 500 KB。
	return &ReadFileToolOpt{
		Workspace:     toolWorkspace,
		MaxImageBytes: defaultMaxImageBytes,
	}
}

// WithMaxImageBytes 设置单张图片大小上限并返回本配置。
func (opt *ReadFileToolOpt) WithMaxImageBytes(maxImageBytes int) *ReadFileToolOpt {
	// 1. 保存图片字节数上限。
	opt.MaxImageBytes = maxImageBytes
	return opt
}

// NewReadFileTool 校验配置并创建文件读取工具。
func NewReadFileTool(opt *ReadFileToolOpt) (executor.Tool, error) {
	// 1. 校验工作区和图片上限，生成参数定义并保存配置。
	if opt == nil || opt.Workspace == nil {
		return nil, errors.New("read_file requires a workspace")
	}
	if opt.MaxImageBytes <= 0 {
		return nil, errors.New("read_file requires a positive image size limit")
	}
	baseTool, err := executor.NewBaseTool[readFileArguments](&executor.BaseToolOpt{
		Name:        "read_file",
		Description: prompt.ReadFileDescription,
	})
	if err != nil {
		return nil, err
	}

	return &readFileToolImpl{
		BaseTool: baseTool,
		opt: &ReadFileToolOpt{
			Workspace:     opt.Workspace,
			MaxImageBytes: opt.MaxImageBytes,
		},
	}, nil
}

// Execute 读取文本或图片，并附带工作区根目录到文件所在目录之间的 AGENTS.md。
func (t *readFileToolImpl) Execute(ctx context.Context, call *defs.Item, _ chan<- *executor.ExecutionProgressEvent) *executor.ToolExecutionResult {
	// 1. 解析参数和文件路径，检查调用是否取消。
	callID := call.ToolCall.CallID
	arguments, err := t.ParseArguments(call.ToolCall.Arguments)
	if err != nil {
		return newInvalidArgumentsResult(callID, err)
	}
	path, err := resolveReadPath(t.opt.Workspace.GetRootDirectory(), arguments.Path)
	if err != nil {
		return newInvalidArgumentsResult(callID, err)
	}
	if err := ctx.Err(); err != nil {
		return encodeContextError(callID, err)
	}
	// 2. 读取文件头识别图片，文本按行分页读取；图片读取不使用行号和行数。
	content, err := t.opt.Workspace.ReadFile(ctx, path, 0, 16)
	if errors.Is(err, workspace.ErrNotRegularFile) {
		err = ErrUnsupportedFile
	}
	if err != nil {
		return newReadFailedResult(callID, err)
	}
	var execution *executor.ToolExecutionResult
	if mimeType := getImageMIMEType(content.Content); mimeType != "" {
		execution = t.readImage(ctx, callID, path, arguments.Path, mimeType)
		if execution.Result.Error != nil {
			return execution
		}
	} else {
		result, err := t.readText(ctx, path, arguments, content.Size)
		if err != nil {
			return newReadFailedResult(callID, err)
		}
		execution = encodeToolResult(callID, result, nil)
		execution.Result.Truncated = result.Truncated
	}
	// 3. 读取工作区根目录到文件所在目录的 AGENTS.md；失败时记 Warn，仍返回已读到的文件内容。
	files, err := workspace.GetFileAgentsInstructions(ctx, t.opt.Workspace, path)
	if err != nil {
		telemetry.GetLogger(ctx).Warn("file agents instructions skipped", "path", path, "error", err)
		return execution
	}
	for _, file := range files {
		execution.AgentsInstructions = append(execution.AgentsInstructions, &defs.AgentsInstruction{
			Path: file.Path,
			Text: file.Text,
		})
	}
	return execution
}

// readImage 读取完整图片，按大小和长边上限压缩后返回图片结果。
func (t *readFileToolImpl) readImage(ctx context.Context, callID, path, logicalPath, mimeType string) *executor.ToolExecutionResult {
	// 1. 打开实际路径 path 并读取全文，拒绝超过文件大小上限的图片。
	reader, err := t.opt.Workspace.OpenFileReader(ctx, path)
	if err != nil {
		return newReadFailedResult(callID, err)
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, maxImageFileBytes+1))
	if err != nil {
		return newReadFailedResult(callID, err)
	}
	if len(data) > maxImageFileBytes {
		return newReadFailedResult(callID, fmt.Errorf("image file is larger than %d MB", maxImageFileBytes>>20))
	}
	// 2. 读取原图宽高并检查像素总数，结果中的路径使用传入的 logicalPath。
	imageConfig, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return newReadFailedResult(callID, fmt.Errorf("cannot decode image: %w", err))
	}
	if imageConfig.Width*imageConfig.Height > maxImagePixels {
		return newReadFailedResult(callID, fmt.Errorf("image has %d×%d pixels, more than %d", imageConfig.Width, imageConfig.Height, maxImagePixels))
	}
	result := &readImageResult{
		Path:     logicalPath,
		MimeType: mimeType,
		Size:     int64(len(data)),
		Width:    imageConfig.Width,
		Height:   imageConfig.Height,
	}
	// 3. 超过发送字节数或长边上限时压缩为 JPEG，再编码为图片 DataURL。
	sentMIMEType := mimeType
	if len(data) > t.opt.MaxImageBytes || max(imageConfig.Width, imageConfig.Height) > maxImageEdge {
		source, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			return newReadFailedResult(callID, fmt.Errorf("cannot decode image: %w", err))
		}
		if data, err = compressImage(source, t.opt.MaxImageBytes); err != nil {
			return newReadFailedResult(callID, err)
		}
		sentMIMEType = "image/jpeg"
		result.Compressed = true
	}
	execution := encodeToolResult(callID, result, nil)
	execution.Result.Images = []*defs.ImageContent{{
		DataURL: "data:" + sentMIMEType + ";base64," + base64.StdEncoding.EncodeToString(data),
		Alt:     logicalPath,
	}}
	return execution
}

// ErrUnsupportedFile 表示 read_file 目标不是普通文件，或读取范围内的内容不是 UTF-8 文本或受支持的图片。
var ErrUnsupportedFile = errors.New("read_file only reads regular UTF-8 text files or PNG/JPEG/GIF/WebP images; use bash to inspect other files")

// readText 顺序跳过起始行之前的内容，读取一页完整行并附上行号和续读位置。
func (t *readFileToolImpl) readText(ctx context.Context, path string, arguments *readFileArguments, size int64) (*readFileResult, error) {
	// 1. 打开文件顺序读取，保存起始行号和原文件大小。
	file, err := t.opt.Workspace.OpenFileReader(ctx, path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, maxReadFileBytes+1)
	result := &readFileResult{
		Path:       arguments.Path,
		Offset:     arguments.Offset,
		NextOffset: arguments.Offset,
		Size:       size,
		Encoding:   "utf-8",
	}
	// 2. 跳过前面的行，逐行校验 UTF-8 并添加行号；正文达到上限时把未加入的整行留给下一页。
	var content strings.Builder
	for lineNumber := int64(1); ; lineNumber++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if result.NextOffset-arguments.Offset == int64(arguments.Limit) {
			_, err := reader.Peek(1)
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
			result.EOF = errors.Is(err, io.EOF)
			break
		}
		line, err := reader.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			return nil, fmt.Errorf("line %d exceeds the 64 KiB read_file limit; use bash to inspect this line", lineNumber)
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if len(line) > 0 && lineNumber >= arguments.Offset {
			if !utf8.Valid(line) {
				return nil, ErrUnsupportedFile
			}
			numberedLine := fmt.Sprintf("%d: %s", lineNumber, line)
			if content.Len()+len(numberedLine) > maxReadFileBytes {
				if content.Len() == 0 {
					return nil, fmt.Errorf("line %d exceeds the 64 KiB read_file limit with its line number; use bash to inspect this line", lineNumber)
				}
				result.Truncated = true
				break
			}
			content.WriteString(numberedLine)
			result.NextOffset++
		}
		if errors.Is(err, io.EOF) {
			result.EOF = true
			break
		}
	}
	// 3. 返回本页正文；空文件或起始行超过文件末尾时正文为空且 EOF 为 true。
	result.Content = content.String()
	return result, nil
}
