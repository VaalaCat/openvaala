package tools_test

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/executor/tools"
	"github.com/VaalaCat/openvaala/pkg/workspace"
)

// TestReadFile 验证文本分页、路径访问和读取错误。
func TestReadFile(t *testing.T) {
	// 1. 准备文本内容、分页位置及各类路径和错误参数。
	tests := []*struct {
		name       string
		arguments  string
		content    string
		offset     int64
		nextOffset int64
		eof        bool
		errorCode  string

		contentContains string
		errorMessage    string
		linuxOnly       bool
	}{
		{
			name:       "whole_file",
			arguments:  `{"path":"note.txt"}`,
			content:    "1: a\n2: 你好\n3: \n4: z",
			nextOffset: 5,
			eof:        true,
		},
		{
			name:       "utf8_page",
			arguments:  `{"path":"note.txt","limit":2}`,
			content:    "1: a\n2: 你好\n",
			nextOffset: 3,
		},
		{
			name:       "next_page",
			arguments:  `{"path":"note.txt","offset":3,"limit":2}`,
			content:    "3: \n4: z",
			offset:     3,
			nextOffset: 5,
			eof:        true,
		},
		{
			name:       "empty_file",
			arguments:  `{"path":"empty.txt"}`,
			nextOffset: 1,
			eof:        true,
		},
		{
			name:       "past_end",
			arguments:  `{"path":"note.txt","offset":100}`,
			offset:     100,
			nextOffset: 100,
			eof:        true,
		},
		{
			name:       "crlf_last_line",
			arguments:  `{"path":"crlf.txt","offset":2,"limit":1}`,
			content:    "2: 你好\r\n",
			offset:     2,
			nextOffset: 3,
			eof:        true,
		},
		{
			name:      "missing_file",
			arguments: `{"path":"missing.txt"}`,
			errorCode: "read_failed",
		},
		// read_file 可读取父目录、绝对路径及工作区外的符号链接目标。
		{
			name:       "parent_path",
			arguments:  `{"path":"../outside.txt"}`,
			content:    "1: secret",
			nextOffset: 2,
			eof:        true,
		},
		{
			name:       "absolute_path",
			arguments:  `{"path":"{{base}}/outside.txt"}`,
			content:    "1: secret",
			nextOffset: 2,
			eof:        true,
		},
		{
			name:       "symlink_outside_workspace",
			arguments:  `{"path":"link.txt"}`,
			content:    "1: secret",
			nextOffset: 2,
			eof:        true,
		},
		{
			name:            "proc_file",
			arguments:       `{"path":"/proc/self/status"}`,
			contentContains: "Name:",
			linuxOnly:       true,
		},
		{
			name:         "non_utf8_file",
			arguments:    `{"path":"binary.bin"}`,
			errorCode:    "read_failed",
			errorMessage: "bash",
		},
		{
			name:      "directory",
			arguments: `{"path":"."}`,
			errorCode: "read_failed",
		},
		{
			name:         "line_too_long",
			arguments:    `{"path":"long.txt"}`,
			errorCode:    "read_failed",
			errorMessage: "use bash",
		},
		{
			name:      "zero_offset",
			arguments: `{"path":"note.txt","offset":0}`,
			errorCode: "invalid_arguments",
		},
		{
			name:      "negative_offset",
			arguments: `{"path":"note.txt","offset":-1}`,
			errorCode: "invalid_arguments",
		},
		{
			name:      "zero_limit",
			arguments: `{"path":"note.txt","limit":0}`,
			errorCode: "invalid_arguments",
		},
		{
			name:      "invalid_limit",
			arguments: `{"path":"note.txt","limit":-1}`,
			errorCode: "invalid_arguments",
		},
		{
			name:      "limit_above_maximum",
			arguments: `{"path":"note.txt","limit":2001}`,
			errorCode: "invalid_arguments",
		},
		// 未声明的参数会被忽略。
		{
			name:       "unknown_argument_ignored",
			arguments:  `{"path":"note.txt","extra":true}`,
			content:    "1: a\n2: 你好\n3: \n4: z",
			nextOffset: 5,
			eof:        true,
		},
	}
	// 2. 创建文件和符号链接，执行读取并核对正文、行号、末尾状态或错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.linuxOnly && runtime.GOOS != "linux" {
				t.Skip("requires /proc")
			}
			base := t.TempDir()
			root := filepath.Join(base, "workspace")
			if err := os.MkdirAll(root, 0700); err != nil {
				t.Fatal(err)
			}
			for path, content := range map[string]string{
				filepath.Join(root, "note.txt"):    "a\n你好\n\nz",
				filepath.Join(root, "empty.txt"):   "",
				filepath.Join(root, "crlf.txt"):    "a\r\n你好\r\n",
				filepath.Join(root, "long.txt"):    strings.Repeat("x", 64<<10+1),
				filepath.Join(root, "binary.bin"):  "\xff\xfe\x00",
				filepath.Join(base, "outside.txt"): "secret",
			} {
				if err := os.WriteFile(path, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(filepath.Join(base, "outside.txt"), filepath.Join(root, "link.txt")); err != nil {
				t.Fatal(err)
			}
			tool, err := tools.NewReadFileTool(tools.NewReadFileToolOpt(newTestWorkspace(t, root)))
			if err != nil {
				t.Fatal(err)
			}
			arguments := strings.ReplaceAll(tt.arguments, "{{base}}", base)
			call := &defs.ToolCall{
				CallID:    "read",
				Name:      "read_file",
				Arguments: arguments,
			}
			result := runTool(t, tool, call)
			if tt.errorCode != "" {
				if result.Error == nil || result.Error.Code != tt.errorCode || !strings.Contains(result.Error.Message, tt.errorMessage) {
					t.Fatalf("error = %+v", result.Error)
				}
				return
			}
			if result.Error != nil {
				t.Fatal(result.Error)
			}
			decoded := &struct {
				Content    string `json:"content"`
				Offset     int64  `json:"offset"`
				NextOffset int64  `json:"next_offset"`
				EOF        bool   `json:"eof"`
			}{}
			if err := json.Unmarshal(result.Content, decoded); err != nil {
				t.Fatal(err)
			}
			if tt.contentContains != "" {
				if !strings.Contains(decoded.Content, tt.contentContains) {
					t.Fatalf("content = %q, want containing %q", decoded.Content, tt.contentContains)
				}
				return
			}
			if decoded.Content != tt.content || decoded.Offset != cmp.Or(tt.offset, 1) || decoded.NextOffset != tt.nextOffset || decoded.EOF != tt.eof {
				t.Fatalf("result = %+v", decoded)
			}
		})
	}
}

// TestReadFilePagination 验证本机和虚拟工作区按续读行号拼回完整文本，覆盖默认行数和字节上限。
func TestReadFilePagination(t *testing.T) {
	// 1. 准备带末尾换行、无末尾换行、默认分页和字节数超限的文件。
	for _, kind := range []string{"local", "virtual"} {
		for _, tt := range []struct {
			name          string
			text          string
			limit         int
			wantPages     int
			wantTruncated bool
		}{
			{
				name:      "with_final_newline",
				text:      "one\n你好\n\nlast\n",
				limit:     2,
				wantPages: 2,
			},
			{
				name:      "without_final_newline",
				text:      "one\n你好\n\nlast",
				limit:     2,
				wantPages: 2,
			},
			{
				name:      "default_line_limit",
				text:      strings.Repeat("一行\n", 201),
				wantPages: 2,
			},
			{
				name:          "byte_limit",
				text:          strings.Repeat(strings.Repeat("你", 10000)+"\n", 3),
				limit:         2000,
				wantPages:     2,
				wantTruncated: true,
			},
		} {
			t.Run(kind+"/"+tt.name, func(t *testing.T) {
				toolWorkspace := newTestWorkspace(t, t.TempDir())
				if kind == "virtual" {
					virtualWorkspace, err := workspace.NewVirtualWorkspace(workspace.NewVirtualWorkspaceOpt("", nil))
					if err != nil {
						t.Fatal(err)
					}
					toolWorkspace = virtualWorkspace
				}
				path := filepath.Join(toolWorkspace.GetRootDirectory(), "pages.txt")
				if err := toolWorkspace.WriteFile(t.Context(), path, []byte(tt.text), 0600, true); err != nil {
					t.Fatal(err)
				}
				tool, err := tools.NewReadFileTool(tools.NewReadFileToolOpt(toolWorkspace))
				if err != nil {
					t.Fatal(err)
				}
				// 2. 用返回的 next_offset 连续读取，核对每一页的行号、大小和结束状态。
				var combined strings.Builder
				offset := int64(1)
				truncated := false
				for page := 1; page <= tt.wantPages; page++ {
					arguments := map[string]any{"path": "pages.txt", "offset": offset}
					if tt.limit != 0 {
						arguments["limit"] = tt.limit
					}
					encoded, err := json.Marshal(arguments)
					if err != nil {
						t.Fatal(err)
					}
					result := runTool(t, tool, &defs.ToolCall{
						CallID:    "read",
						Name:      "read_file",
						Arguments: string(encoded),
					})
					if result.Error != nil {
						t.Fatal(result.Error)
					}
					var decoded struct {
						Content    string `json:"content"`
						Offset     int64  `json:"offset"`
						NextOffset int64  `json:"next_offset"`
						Size       int64  `json:"size"`
						EOF        bool   `json:"eof"`
						Truncated  bool   `json:"truncated"`
					}
					if err := json.Unmarshal(result.Content, &decoded); err != nil {
						t.Fatal(err)
					}
					if decoded.Offset != offset || decoded.NextOffset <= offset || decoded.Size != int64(len(tt.text)) ||
						decoded.EOF != (page == tt.wantPages) || len(decoded.Content) > 64<<10 || decoded.Truncated != result.Truncated {
						t.Fatalf("page %d: offset=%d next=%d size=%d eof=%v bytes=%d truncated=%v", page,
							decoded.Offset, decoded.NextOffset, decoded.Size, decoded.EOF, len(decoded.Content), decoded.Truncated)
					}
					combined.WriteString(decoded.Content)
					offset = decoded.NextOffset
					truncated = truncated || decoded.Truncated
				}
				// 3. 核对分页拼接结果，每一行只出现一次且保留原始换行。
				var want strings.Builder
				for index, line := range strings.SplitAfter(tt.text, "\n") {
					if line != "" {
						fmt.Fprintf(&want, "%d: %s", index+1, line)
					}
				}
				if combined.String() != want.String() || truncated != tt.wantTruncated {
					t.Fatalf("joined output matches=%v, truncated=%v, want %v", combined.String() == want.String(), truncated, tt.wantTruncated)
				}
			})
		}
	}
}

// testImageLimit 是 TestReadFileImage 默认的单张图片上限，和配置 tools.max_image_kb 的默认值一致。
const testImageLimit = 500 << 10

// minimalWebPBase64 是 1×1 的无损 WebP。
const minimalWebPBase64 = "UklGRhoAAABXRUJQVlA4TA0AAAAvAAAAEAcQERGIiP4HAA=="

// TestReadFileImage 验证图片读取、格式识别、压缩及上限错误。
func TestReadFileImage(t *testing.T) {
	// 1. 准备不同格式、尺寸、透明度及损坏图片。
	tests := []*struct {
		name string
		// path 是工作区里的文件名；arguments 为空时读取 path。
		path          string
		arguments     string
		file          func(t *testing.T) []byte
		maxImageBytes func(file []byte) int // nil 时取 testImageLimit

		errorEquals   string
		errorPrefix   string
		errorContains string

		wantText       string // 非空时按文本读取，没有 Images
		wantMIME       string // Content 里的 mime_type，描述原文件
		wantSentMIME   string // 交给模型的图片格式；为空时与 wantMIME 相同
		wantAsIs       bool
		wantCompressed bool
		wantWidth      int
		wantHeight     int
		checkImage     func(t *testing.T, data []byte)
	}{
		{
			name:       "small_png",
			path:       "small.png",
			file:       func(t *testing.T) []byte { return encodePNG(t, newGradientImage(64, 32)) },
			wantMIME:   "image/png",
			wantAsIs:   true,
			wantWidth:  64,
			wantHeight: 32,
		},
		{
			name:           "noisy_png_is_compressed",
			path:           "noisy.png",
			file:           func(t *testing.T) []byte { return encodePNG(t, newNoiseImage(2880, 1800, 1)) },
			wantMIME:       "image/png",
			wantSentMIME:   "image/jpeg",
			wantCompressed: true,
			wantWidth:      2880,
			wantHeight:     1800,
		},
		{
			name: "tall_png_over_edge_limit_is_compressed",
			path: "tall.png",
			file: func(t *testing.T) []byte {
				data := encodePNG(t, newGradientImage(100, 9000))
				if len(data) > testImageLimit {
					t.Fatalf("fixture is %d bytes, must stay under the byte limit so only the edge forces compression", len(data))
				}
				return data
			},
			wantMIME:       "image/png",
			wantSentMIME:   "image/jpeg",
			wantCompressed: true,
			wantWidth:      100,
			wantHeight:     9000,
			checkImage: func(t *testing.T, data []byte) {
				sent, err := jpeg.DecodeConfig(bytes.NewReader(data))
				if err != nil {
					t.Fatal(err)
				}
				if sent.Width != 88 || sent.Height != 8000 {
					t.Fatalf("sent size = %dx%d, want 88x8000", sent.Width, sent.Height)
				}
			},
		},
		{
			name:           "transparent_png_gets_white_background",
			path:           "transparent.png",
			file:           func(t *testing.T) []byte { return encodePNG(t, newTransparentBorderedImage(600, 600, 40)) },
			maxImageBytes:  func([]byte) int { return 20 << 10 },
			wantMIME:       "image/png",
			wantSentMIME:   "image/jpeg",
			wantCompressed: true,
			wantWidth:      600,
			wantHeight:     600,
			checkImage: func(t *testing.T, data []byte) {
				decoded, err := jpeg.Decode(bytes.NewReader(data))
				if err != nil {
					t.Fatal(err)
				}
				bounds := decoded.Bounds()
				red, green, blue, _ := decoded.At(bounds.Dx()/2, bounds.Dy()/2).RGBA()
				for _, channel := range []uint32{red, green, blue} {
					if channel>>8 < 240 {
						t.Fatalf("center pixel rgb = %d %d %d, want near white", red>>8, green>>8, blue>>8)
					}
				}
			},
		},
		{
			name:          "image_at_limit_is_sent_as_is",
			path:          "limit.png",
			file:          func(t *testing.T) []byte { return encodePNG(t, newGradientImage(64, 32)) },
			maxImageBytes: func(file []byte) int { return len(file) },
			wantMIME:      "image/png",
			wantAsIs:      true,
			wantWidth:     64,
			wantHeight:    32,
		},
		{
			name: "webp",
			path: "pixel.webp",
			file: func(t *testing.T) []byte {
				data, err := base64.StdEncoding.DecodeString(minimalWebPBase64)
				if err != nil {
					t.Fatal(err)
				}
				return data
			},
			wantMIME:   "image/webp",
			wantAsIs:   true,
			wantWidth:  1,
			wantHeight: 1,
		},
		{
			name: "gif",
			path: "icon.gif",
			file: func(t *testing.T) []byte {
				var encoded bytes.Buffer
				frame := image.NewPaletted(image.Rect(0, 0, 30, 20), color.Palette{color.Black, color.White})
				if err := gif.Encode(&encoded, frame, nil); err != nil {
					t.Fatal(err)
				}
				return encoded.Bytes()
			},
			wantMIME:   "image/gif",
			wantAsIs:   true,
			wantWidth:  30,
			wantHeight: 20,
		},
		{
			name:     "png_name_with_text_content",
			path:     "shot.png",
			file:     func(t *testing.T) []byte { return []byte("hello") },
			wantText: "1: hello",
		},
		{
			name:        "broken_png",
			path:        "broken.png",
			file:        func(t *testing.T) []byte { return append([]byte("\x89PNG\r\n\x1a\n"), "garbage"...) },
			errorPrefix: "cannot decode image:",
		},
		{
			name:       "line_range_does_not_crop_image",
			path:       "a.png",
			arguments:  `{"path":"a.png","offset":10,"limit":1}`,
			file:       func(t *testing.T) []byte { return encodePNG(t, newGradientImage(64, 32)) },
			wantMIME:   "image/png",
			wantAsIs:   true,
			wantWidth:  64,
			wantHeight: 32,
		},
		{
			name:          "tiny_limit",
			path:          "noisy.png",
			file:          func(t *testing.T) []byte { return encodePNG(t, newNoiseImage(200, 200, 1)) },
			maxImageBytes: func([]byte) int { return 100 },
			errorEquals:   "image cannot be compressed below 1 KB",
		},
		{
			name: "image_file_over_read_limit",
			path: "huge.png",
			file: func(t *testing.T) []byte {
				data := make([]byte, 20<<20+1)
				copy(data, newPNGHeader(1, 1))
				return data
			},
			errorEquals: "image file is larger than 20 MB",
		},
		{
			name:        "image_over_pixel_limit",
			path:        "wide.png",
			file:        func(t *testing.T) []byte { return newPNGHeader(9000, 9000) },
			errorEquals: "image has 9000×9000 pixels, more than 64000000",
		},
	}
	// 2. 写入图片并执行读取，核对原图信息、发送字节和压缩结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			file := tt.file(t)
			if err := os.WriteFile(filepath.Join(root, tt.path), file, 0600); err != nil {
				t.Fatal(err)
			}
			limit := testImageLimit
			if tt.maxImageBytes != nil {
				limit = tt.maxImageBytes(file)
			}
			tool, err := tools.NewReadFileTool(tools.NewReadFileToolOpt(newTestWorkspace(t, root)).WithMaxImageBytes(limit))
			if err != nil {
				t.Fatal(err)
			}
			arguments := tt.arguments
			if arguments == "" {
				arguments = `{"path":"` + tt.path + `"}`
			}
			result := runTool(t, tool, &defs.ToolCall{
				CallID:    "read",
				Name:      "read_file",
				Arguments: arguments,
			})
			if tt.errorEquals != "" || tt.errorPrefix != "" || tt.errorContains != "" {
				if result.Error == nil || result.Error.Code != "read_failed" || len(result.Images) != 0 {
					t.Fatalf("result = %+v", result)
				}
				message := result.Error.Message
				if (tt.errorEquals != "" && message != tt.errorEquals) ||
					!strings.HasPrefix(message, tt.errorPrefix) || !strings.Contains(message, tt.errorContains) {
					t.Fatalf("error message = %q", message)
				}
				return
			}
			if result.Error != nil {
				t.Fatal(result.Error)
			}
			decoded := &struct {
				Content    string `json:"content"`
				Path       string `json:"path"`
				MimeType   string `json:"mime_type"`
				Size       int64  `json:"size"`
				Width      int    `json:"width"`
				Height     int    `json:"height"`
				Compressed *bool  `json:"compressed"`
			}{}
			if err := json.Unmarshal(result.Content, decoded); err != nil {
				t.Fatal(err)
			}
			if tt.wantText != "" {
				if decoded.Content != tt.wantText || len(result.Images) != 0 {
					t.Fatalf("result = %+v images = %d", decoded, len(result.Images))
				}
				return
			}
			if len(result.Images) != 1 || result.Images[0].Alt != tt.path {
				t.Fatalf("images = %+v", result.Images)
			}
			sentMIME := cmp.Or(tt.wantSentMIME, tt.wantMIME)
			encoded, found := strings.CutPrefix(result.Images[0].DataURL, "data:"+sentMIME+";base64,")
			if !found {
				t.Fatalf("data url prefix = %.40q, want mime %s", result.Images[0].DataURL, sentMIME)
			}
			data, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantAsIs && !bytes.Equal(data, file) {
				t.Fatalf("image bytes changed: got %d bytes, want %d bytes", len(data), len(file))
			}
			if len(data) > limit {
				t.Fatalf("image is %d bytes, limit %d", len(data), limit)
			}
			if decoded.Path != tt.path || decoded.MimeType != tt.wantMIME || decoded.Size != int64(len(file)) ||
				decoded.Width != tt.wantWidth || decoded.Height != tt.wantHeight ||
				(decoded.Compressed != nil) != tt.wantCompressed {
				t.Fatalf("content = %s", result.Content)
			}
			if tt.checkImage != nil {
				tt.checkImage(t, data)
			}
		})
	}
}

// TestNewReadFileToolRejectsImageLimit 验证图片大小上限必须大于零。
func TestNewReadFileToolRejectsImageLimit(t *testing.T) {
	// 1. 逐项构造零、负数和默认上限的工具，核对构造错误。
	tests := []*struct {
		name         string
		opt          *tools.ReadFileToolOpt
		errorMessage string
	}{
		{
			name:         "zero",
			opt:          tools.NewReadFileToolOpt(newTestWorkspace(t, t.TempDir())).WithMaxImageBytes(0),
			errorMessage: "read_file requires a positive image size limit",
		},
		{
			name:         "negative",
			opt:          tools.NewReadFileToolOpt(newTestWorkspace(t, t.TempDir())).WithMaxImageBytes(-1),
			errorMessage: "read_file requires a positive image size limit",
		},
		{
			name: "default",
			opt:  tools.NewReadFileToolOpt(newTestWorkspace(t, t.TempDir())),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tools.NewReadFileTool(tt.opt)
			if tt.errorMessage == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || err.Error() != tt.errorMessage {
				t.Fatalf("error = %v, want %q", err, tt.errorMessage)
			}
		})
	}
}

// encodePNG 把测试图片编码为 PNG。
func encodePNG(t *testing.T, source image.Image) []byte {
	// 1. 编码图片并返回字节，失败时结束测试。
	t.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, source); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

// newPNGHeader 生成只有签名和尺寸 IHDR 块的 PNG 数据，不含像素。
func newPNGHeader(width, height uint32) []byte {
	// 1. 写入 PNG 签名、宽高和 IHDR 参数，再补充校验和。
	header := make([]byte, 0, 33)
	header = append(header, "\x89PNG\r\n\x1a\n"...)
	header = binary.BigEndian.AppendUint32(header, 13)
	header = append(header, "IHDR"...)
	header = binary.BigEndian.AppendUint32(header, width)
	header = binary.BigEndian.AppendUint32(header, height)
	header = append(header, 8, 2, 0, 0, 0) // 位深 8、RGB、默认压缩、过滤和隔行方式
	return binary.BigEndian.AppendUint32(header, crc32.ChecksumIEEE(header[12:]))
}

// newGradientImage 生成不透明的渐变图片。
func newGradientImage(width, height int) *image.NRGBA {
	// 1. 逐像素按横纵坐标设置颜色。
	source := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			source.SetNRGBA(x, y, color.NRGBA{
				R: uint8(x * 4),
				G: uint8(y * 8),
				B: 128,
				A: 255,
			})
		}
	}
	return source
}

// newNoiseImage 按固定随机种子生成不透明噪点图片。
func newNoiseImage(width, height int, seed int64) *image.NRGBA {
	// 1. 随机填充像素，将所有 alpha 字节设为 255。
	source := image.NewNRGBA(image.Rect(0, 0, width, height))
	rand.New(rand.NewSource(seed)).Read(source.Pix)
	for index := 3; index < len(source.Pix); index += 4 {
		source.Pix[index] = 255
	}
	return source
}

// newTransparentBorderedImage 生成透明中心和噪点边框的图片。
func newTransparentBorderedImage(width, height, border int) *image.NRGBA {
	// 1. 创建透明图片，只复制指定宽度的噪点边框。
	noise := newNoiseImage(width, height, 2)
	source := image.NewNRGBA(noise.Bounds())
	for y := range height {
		for x := range width {
			if x < border || y < border || x >= width-border || y >= height-border {
				source.SetNRGBA(x, y, noise.NRGBAAt(x, y))
			}
		}
	}
	return source
}

// newTestWorkspace 创建指定根目录的本机工作区。
func newTestWorkspace(t *testing.T, root string) workspace.Workspace {
	// 1. 构造本机工作区，失败时结束测试。
	t.Helper()
	instance, err := workspace.NewLocalWorkspace(workspace.NewLocalWorkspaceOpt(root))
	if err != nil {
		t.Fatal(err)
	}
	return instance
}

// runTool 用只包含这个工具的执行器运行一次调用。
func runTool(t *testing.T, tool executor.Tool, call *defs.ToolCall) *defs.ToolResult {
	// 1. 创建工具执行器并执行调用。
	t.Helper()
	instance, err := executor.NewToolExecutor(executor.NewToolExecutorOpt(nil, tool))
	if err != nil {
		t.Fatal(err)
	}
	return runExecutorCall(t, instance, call)
}

// runExecutorCall 执行工具调用，收集并返回对应结果。
func runExecutorCall(t *testing.T, instance executor.ToolExecutor, call *defs.ToolCall) *defs.ToolResult {
	// 1. 以五秒超时启动调用。
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	events := make(chan executor.ExecutorEvent)
	var tasks sync.WaitGroup
	tasks.Go(func() {
		instance.Run(ctx, []*defs.Item{{
			Kind:     defs.ItemToolCall,
			ToolCall: call,
		}}, events)
	})
	// 2. 收集完成事件，等待执行结束并核对调用 ID。
	var result *defs.ToolResult
	for event := range events {
		if finished, ok := event.(*executor.ExecutionFinishedEvent); ok {
			result = finished.Result
		}
	}
	tasks.Wait()
	if result == nil || result.CallID != call.CallID {
		t.Fatalf("result = %+v", result)
	}
	return result
}

// TestReadFileKeepsContentWhenRulesFail 验证目录规则读取失败时仍返回文件正文。
func TestReadFileKeepsContentWhenRulesFail(t *testing.T) {
	// 1. 分别模拟权限错误和远端断开，核对文件正文仍成功返回。
	for _, tt := range []struct {
		name string
		err  error
	}{
		{
			name: "permission_denied",
			err:  os.ErrPermission,
		},
		{
			name: "remote_disconnected",
			err:  errors.New("connection lost"),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("file-content-marker"), 0600); err != nil {
				t.Fatal(err)
			}
			work := &unreadableAgentsWorkspace{
				Workspace: newTestWorkspace(t, root),
				err:       tt.err,
			}
			tool, err := tools.NewReadFileTool(tools.NewReadFileToolOpt(work))
			if err != nil {
				t.Fatal(err)
			}
			result := runTool(t, tool, &defs.ToolCall{
				CallID:    "read",
				Name:      "read_file",
				Arguments: `{"path":"note.txt"}`,
			})
			if result.Error != nil || !strings.Contains(string(result.Content), "file-content-marker") {
				t.Fatalf("read_file result = %+v", result)
			}
		})
	}
}

// unreadableAgentsWorkspace 读 AGENTS.md 时返回 err，其他文件照常从真实工作区读取。
type unreadableAgentsWorkspace struct {
	workspace.Workspace
	err error
}

// ReadFile 模拟目录规则文件的读取失败。
func (w *unreadableAgentsWorkspace) ReadFile(ctx context.Context, path string, offset int64, limit int) (*workspace.FileContent, error) {
	// 1. AGENTS.md 返回读取失败，其他路径交给真实工作区。
	if filepath.Base(path) == "AGENTS.md" {
		return nil, w.err
	}
	return w.Workspace.ReadFile(ctx, path, offset, limit)
}
