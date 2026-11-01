package tui

import (
	"fmt"
	"image/color"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/creack/pty"
)

// TestMarkdownRenderer 检查 Markdown 内容、缩进和显示宽度。
func TestMarkdownRenderer(t *testing.T) {
	// 1. 准备段落、标题、代码、列表和引用样例。
	paragraph := strings.Repeat("今天天气晴朗气温十八到二十五度非常适合出门散步", 3)
	tests := []struct {
		name         string
		source       string
		width        int
		wantText     []string
		wantFullText string // 去掉每行缩进后拼接应等于该文本
		wantIndent   int    // 每一行（含折行后的续行）至少保留的缩进空格数
	}{
		{
			name:         "cjk_paragraph_wraps_by_width",
			source:       paragraph,
			width:        20,
			wantText:     []string{"今天天气"},
			wantFullText: paragraph,
			wantIndent:   2,
		},
		{
			name:     "heading_and_code",
			source:   "## 结论\n\n```go\nfunc main() {}\n```\n",
			width:    40,
			wantText: []string{"结论", "func main() {}"},
		},
		{
			name:     "list_item",
			source:   "2. 第二项\n",
			width:    40,
			wantText: []string{"2. 第二项"},
		},
		{
			name:     "quote",
			source:   "> 引用\n",
			width:    40,
			wantText: []string{"引用"},
		},
		{
			name:   "blank_renders_nothing",
			source: "\n\n",
			width:  40,
		},
		{
			name:     "code_tabs_keep_indent",
			source:   "```go\nfunc f() int {\n\tif true {\n\t\treturn 1\n\t}\n}\n```\n",
			width:    40,
			wantText: []string{"        return 1", "    if true {"},
		},
	}
	// 2. 分别以无颜色和深色样式渲染，检查文字、宽度及缩进。
	for _, style := range []string{markdownStyleNoTTY, "dark"} {
		for _, tt := range tests {
			t.Run(style+"/"+tt.name, func(t *testing.T) {
				renderer, err := newMarkdownRenderer(style, tt.width)
				if err != nil {
					t.Fatal(err)
				}
				lines := renderer.render(tt.source)
				if len(tt.wantText) == 0 {
					if len(lines) != 0 {
						t.Fatalf("lines = %q, want none", lines)
					}
					return
				}
				if len(lines) == 0 {
					t.Fatal("no lines rendered")
				}
				plains := make([]string, len(lines))
				for index, line := range lines {
					plains[index] = ansi.Strip(line)
					if strings.Contains(line, "\t") {
						t.Fatalf("line %q keeps a tab", line)
					}
					if width := ansi.StringWidth(line); width > tt.width {
						t.Fatalf("line %q width %d exceeds %d", line, width, tt.width)
					}
					if plains[index] != strings.TrimRight(plains[index], " ") {
						t.Fatalf("line %q keeps trailing padding", line)
					}
					if strings.HasSuffix(line, "\x1b[0m") && strings.TrimSpace(plains[index]) == "" {
						t.Fatalf("line %q is an empty escape sequence", line)
					}
				}
				if strings.TrimSpace(plains[0]) == "" || strings.TrimSpace(plains[len(plains)-1]) == "" {
					t.Fatalf("first or last line is blank: %q", lines)
				}
				joined := strings.Join(plains, "\n")
				for _, want := range tt.wantText {
					if !strings.Contains(joined, want) {
						t.Fatalf("rendered %q lacks %q", plains, want)
					}
				}
				if tt.wantIndent > 0 {
					indent := strings.Repeat(" ", tt.wantIndent)
					var body strings.Builder
					for _, plain := range plains {
						if !strings.HasPrefix(plain, indent) {
							t.Fatalf("line %q lost its %d-space indent", plain, tt.wantIndent)
						}
						body.WriteString(strings.TrimLeft(plain, " "))
					}
					if body.String() != tt.wantFullText {
						t.Fatalf("wrapped text = %q, want %q", body.String(), tt.wantFullText)
					}
				}
			})
		}
	}
}

// TestMarkdownRendererStripsTerminalControls 检查模型原文中的终端控制已清理。
func TestMarkdownRendererStripsTerminalControls(t *testing.T) {
	// 1. 准备带 OSC、未闭合样式、回车和代码制表符的原文。
	tests := []struct {
		name     string
		source   string
		wantText string
	}{
		{
			name:     "osc52_in_paragraph",
			source:   "前\x1b]52;c;ZXZpbA==\x07后\n",
			wantText: "前后",
		},
		{
			name:     "style_without_reset",
			source:   "前\x1b[31m后\n",
			wantText: "前后",
		},
		{
			name:     "carriage_return_in_list",
			source:   "- 进度 10%\r完成\n",
			wantText: "完成",
		},
		{
			name:     "control_in_code_block",
			source:   "```\n\tx\x1b]0;title\x07 := 1\r\n```\n",
			wantText: "    x := 1",
		},
	}
	// 2. 分别渲染两种样式，检查控制字符已移除且正文保留。
	for _, style := range []string{markdownStyleNoTTY, "dark"} {
		renderer := mustNewMarkdownRenderer(style, 60)
		for _, tt := range tests {
			t.Run(style+"/"+tt.name, func(t *testing.T) {
				output := strings.Join(renderer.render(tt.source), "\n")
				if strings.ContainsAny(output, "\r\x07") || strings.Contains(output, "\x1b]") ||
					(style == markdownStyleNoTTY && strings.Contains(output, "\x1b")) {
					t.Fatalf("terminal controls reached the output: %q", output)
				}
				if plain := ansi.Strip(output); !strings.Contains(plain, tt.wantText) || strings.Contains(plain, "[31m") {
					t.Fatalf("visible text = %q, want it to contain %q", plain, tt.wantText)
				}
			})
		}
	}
}

// TestMarkdownRendererLineBreaking 检查中英文折行、列表引用续行和表格排版。
func TestMarkdownRendererLineBreaking(t *testing.T) {
	// 1. 准备固定宽度的长段落、代码、列表和表格。
	const width = 40
	long := strings.Repeat("很长", 20)
	tests := []struct {
		name   string
		source string
		check  func(t *testing.T, plains []string)
	}{
		{
			name:   "mixed_cjk_paragraph_fills_lines",
			source: "月球被地球潮汐锁定，自转周期与公转周期同为约 27.3 个地球日，因此始终以同一面朝向地球。",
			check: func(t *testing.T, plains []string) {
				for _, plain := range plains[:len(plains)-1] {
					if got := ansi.StringWidth(plain); got < width-2 {
						t.Fatalf("line %q width %d leaves a ragged gap, lines %q", plain, got, plains)
					}
				}
				joined := strings.ReplaceAll(strings.Join(plains, ""), " ", "")
				if want := "月球被地球潮汐锁定，自转周期与公转周期同为约27.3个地球日，因此始终以同一面朝向地球。"; joined != want {
					t.Fatalf("text = %q, want %q", joined, want)
				}
			},
		},
		{
			name:   "list_continuation_aligned",
			source: "- 列表" + long + "\n",
			check: func(t *testing.T, plains []string) {
				requireContinuationPrefix(t, plains, "  • ", "    ")
			},
		},
		{
			name:   "ordered_list_continuation_aligned",
			source: "1. 有序" + long + "\n",
			check: func(t *testing.T, plains []string) {
				requireContinuationPrefix(t, plains, "  1. ", "     ")
			},
		},
		{
			name:   "quote_continuation_repeats_bar",
			source: "> 引用" + long + "\n",
			check: func(t *testing.T, plains []string) {
				bar := plains[0][:len("  ")+len(strings.SplitN(strings.TrimLeft(plains[0], " "), " ", 2)[0])+1]
				requireContinuationPrefix(t, plains, bar, bar)
			},
		},
		{
			// 代码块按宽度硬折：以 "| " 开头的代码行续行不重复 "| "，续行只保留代码块的空格缩进。
			name:   "code_bar_line_not_repeated",
			source: "```md\n| a | b | " + strings.Repeat("c", 80) + " |\n```\n",
			check: func(t *testing.T, plains []string) {
				requireContinuationPrefix(t, plains, "    | a | b | c", "    ")
				for _, plain := range plains[1:] {
					if strings.Contains(plain, "|") && !strings.HasSuffix(plain, "c |") {
						t.Fatalf("continuation %q repeats the bar, lines %q", plain, plains)
					}
				}
			},
		},
		{
			// 代码行在宽度处硬折而不是在空格处折：空格原样保留，去掉缩进后拼接等于原文。
			name:   "code_line_keeps_spaces",
			source: "```\nkey:   " + strings.Repeat("v", 50) + "\n```\n",
			check: func(t *testing.T, plains []string) {
				var body strings.Builder
				for _, plain := range plains {
					body.WriteString(strings.TrimPrefix(plain, "    "))
				}
				if want := "key:   " + strings.Repeat("v", 50); body.String() != want {
					t.Fatalf("code text = %q, want %q, lines %q", body.String(), want, plains)
				}
			},
		},
		{
			// 缩进 4 个以上空格的围栏属于列表项，留在列表的上下文中渲染：不显示围栏行，缩进与列表一致。
			name:   "list_nested_fence_4_spaces",
			source: "- 步骤一\n\n    ```bash\n    ls -la\n    ```\n\n- 步骤二\n",
			check: func(t *testing.T, plains []string) {
				requireLines(t, plains, []string{"  • 步骤一", "    ls -la", "", "  • 步骤二"})
			},
		},
		{
			name:   "list_nested_fence_6_spaces",
			source: "1. 步骤一\n\n      ```bash\n      ls -la\n      ```\n\n2. 步骤二\n",
			check: func(t *testing.T, plains []string) {
				requireLines(t, plains, []string{"  1. 步骤一", "    ls -la", "", "  2. 步骤二"})
			},
		},
		{
			name:   "table_fits_width",
			source: "说明\n\n| 列 | 说明 |\n|---|---|\n| a | 这是一个" + long + "的单元格 |\n\n结尾\n",
			check: func(t *testing.T, plains []string) {
				joined := strings.Join(plains, "\n")
				for _, want := range []string{"说明", "单元格", "结尾"} {
					if !strings.Contains(joined, want) {
						t.Fatalf("rendered %q lacks %q", plains, want)
					}
				}
				// 表格按宽度排版：每一行（含单元格内折行）都保留列分隔线，而不是被整行折断。
				for _, plain := range plains[1 : len(plains)-1] {
					if plain != "" && !strings.ContainsAny(plain, "|│┼") {
						t.Fatalf("table line %q lost its column separator, lines %q", plain, plains)
					}
				}
			},
		},
	}
	// 2. 用两种样式渲染，检查每行宽度、标点和各类续行。
	for _, style := range []string{markdownStyleNoTTY, "dark"} {
		for _, tt := range tests {
			t.Run(style+"/"+tt.name, func(t *testing.T) {
				renderer, err := newMarkdownRenderer(style, width)
				if err != nil {
					t.Fatal(err)
				}
				lines := renderer.render(tt.source)
				if len(lines) < 2 {
					t.Fatalf("lines = %q, want wrapped lines", lines)
				}
				plains := make([]string, len(lines))
				for index, line := range lines {
					plains[index] = ansi.Strip(line)
					if got := ansi.StringWidth(line); got > width {
						t.Fatalf("line %q width %d exceeds %d", plains[index], got, width)
					}
					if body := strings.TrimLeft(plains[index], " "); strings.HasPrefix(body, "。") || strings.HasPrefix(body, "，") {
						t.Fatalf("line %q starts with closing punctuation", plains[index])
					}
				}
				tt.check(t, plains)
			})
		}
	}
}

// requireLines 检查渲染结果逐行等于预期。
func requireLines(t *testing.T, plains, want []string) {
	// 1. 比较行列表，不同则报告实际和预期内容。
	t.Helper()
	if !slices.Equal(plains, want) {
		t.Fatalf("lines = %q, want %q", plains, want)
	}
}

// requireContinuationPrefix 检查首行前缀和续行缩进。
func requireContinuationPrefix(t *testing.T, plains []string, first, continuation string) {
	// 1. 检查第一行以指定文字开头。
	t.Helper()
	if !strings.HasPrefix(plains[0], first) {
		t.Fatalf("first line %q lacks prefix %q", plains[0], first)
	}
	// 2. 检查续行前缀，并检查正文紧接前缀。
	for _, plain := range plains[1:] {
		if !strings.HasPrefix(plain, continuation) || strings.HasPrefix(plain[len(continuation):], " ") {
			t.Fatalf("continuation %q is not aligned to %q, lines %q", plain, continuation, plains)
		}
	}
}

// TestResolveTerminalAppearance 检查终端样式选择和背景色读取。
func TestResolveTerminalAppearance(t *testing.T) {
	// 1. 准备深浅背景应答、颜色能力和非终端输入输出。
	const deviceAttributes = "\x1b[?62;c"
	darkReply := "\x1b]11;rgb:1e1e/1e1e/1e1e\x07" + deviceAttributes
	lightReply := "\x1b]11;rgb:ffff/ffff/ffff\x07" + deviceAttributes
	tests := []struct {
		name  string
		style string
		// isTerminal 输入输出是否用模拟终端（pty）？
		isTerminal bool
		// terminalType 是本场景的 TERM 值。
		terminalType string
		// reply 是模拟终端对背景色与设备属性查询的应答。
		reply          string
		wantStyle      string
		wantBackground color.Color
	}{
		{
			name:      "explicit_style_kept_without_terminal",
			style:     "dark",
			wantStyle: "dark",
		},
		{
			name:      "auto_without_terminal_is_notty",
			style:     markdownStyleAuto,
			wantStyle: markdownStyleNoTTY,
		},
		{
			name:           "dark_background",
			style:          markdownStyleAuto,
			isTerminal:     true,
			terminalType:   "xterm-256color",
			reply:          darkReply,
			wantStyle:      "dark",
			wantBackground: lipgloss.Color("#1e1e1e"),
		},
		{
			name:           "light_background",
			style:          markdownStyleAuto,
			isTerminal:     true,
			terminalType:   "xterm-256color",
			reply:          lightReply,
			wantStyle:      "light",
			wantBackground: lipgloss.Color("#ffffff"),
		},
		{
			name:           "explicit_style_kept_with_background",
			style:          "dark",
			isTerminal:     true,
			terminalType:   "xterm-256color",
			reply:          lightReply,
			wantStyle:      "dark",
			wantBackground: lipgloss.Color("#ffffff"),
		},
		// 终端只应答设备属性时使用深色样式，不使用底色。
		{
			name:         "background_unanswered",
			style:        markdownStyleAuto,
			isTerminal:   true,
			terminalType: "xterm-256color",
			reply:        deviceAttributes,
			wantStyle:    "dark",
		},
		// 16 色终端不使用底色，文字样式按背景亮度选择。
		{
			name:         "sixteen_color_terminal",
			style:        markdownStyleAuto,
			isTerminal:   true,
			terminalType: "xterm",
			reply:        lightReply,
			wantStyle:    "light",
		},
	}
	// 2. 创建对应终端环境并查询外观，比较样式和背景色。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var input io.Reader = strings.NewReader("")
			var output io.Writer = io.Discard
			if tt.isTerminal {
				// 清空颜色检测使用的环境变量和 terminfo 路径，再设置本场景的 TERM。
				for _, name := range []string{
					"COLORTERM", "TMUX", "NO_COLOR", "CLICOLOR", "CLICOLOR_FORCE", "TTY_FORCE", "WT_SESSION", "GOOGLE_CLOUD_SHELL",
					"TERMINFO", "TERMINFO_DIRS",
				} {
					t.Setenv(name, "")
				}
				t.Setenv("TERM", tt.terminalType)
				terminal := startFakeTerminal(t, tt.reply)
				input, output = terminal, terminal
			}
			gotStyle, gotBackground := resolveTerminalAppearance(tt.style, input, output)
			if gotStyle != tt.wantStyle || !isSameColor(gotBackground, tt.wantBackground) {
				t.Fatalf("got (%q, %v), want (%q, %v)", gotStyle, gotBackground, tt.wantStyle, tt.wantBackground)
			}
		})
	}
}

// startFakeTerminal 创建接收设备属性查询后返回指定应答的模拟终端。
func startFakeTerminal(t *testing.T, reply string) *os.File {
	// 1. 打开 PTY，登记测试结束时关闭两端并等待模拟终端退出。
	t.Helper()
	control, terminal, err := pty.Open()
	if err != nil {
		t.Skipf("pty unavailable: %v", err)
	}
	done := make(chan struct{})
	t.Cleanup(func() {
		_ = control.Close()
		_ = terminal.Close()
		<-done
	})
	// 2. 启动读取查询的任务，收到设备属性查询后写回应答。
	go func() {
		defer close(done)
		var query []byte
		buffer := make([]byte, 256)
		for !strings.Contains(string(query), ansi.RequestPrimaryDeviceAttributes) {
			count, err := control.Read(buffer)
			if err != nil {
				return
			}
			query = append(query, buffer[:count]...)
		}
		_, _ = control.WriteString(reply)
	}()
	return terminal
}

// TestNewTUIStylesSurfaces 检查输入、选中项和 diff 的底色与文字颜色。
func TestNewTUIStylesSurfaces(t *testing.T) {
	// 1. 准备深浅背景、无背景和无颜色样式。
	dark, light := lipgloss.Color("#1e1e1e"), lipgloss.Color("#ffffff")
	tests := []struct {
		name          string
		markdownStyle string
		background    color.Color
		// want* 为 nil 表示不带底色（lipgloss.NoColor）。
		wantSurface, wantSelection, wantAdded, wantRemoved color.Color
	}{
		{
			name:          "no_background",
			markdownStyle: "dark",
		},
		{
			name:          "notty_ignores_background",
			markdownStyle: markdownStyleNoTTY,
			background:    dark,
		},
		{
			name:          "dark_background",
			markdownStyle: "dark",
			background:    dark,
			wantSurface:   lipgloss.Lighten(dark, 0.08),
			wantSelection: lipgloss.Lighten(dark, 0.16),
			wantAdded:     lipgloss.Color("#213A2B"),
			wantRemoved:   lipgloss.Color("#4A221D"),
		},
		{
			name:          "light_background",
			markdownStyle: "light",
			background:    light,
			wantSurface:   lipgloss.Darken(light, 0.04),
			wantSelection: lipgloss.Darken(light, 0.10),
			wantAdded:     lipgloss.Color("#dafbe1"),
			wantRemoved:   lipgloss.Color("#ffebe9"),
		},
	}
	// 2. 生成样式并比较各底色、前景色和背景标记。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			styles := newTUIStyles(tt.markdownStyle, tt.background)
			for _, each := range []struct {
				name  string
				style lipgloss.Style
				want  color.Color
			}{
				{
					"surface",
					styles.surface,
					tt.wantSurface,
				}, {
					"selection",
					styles.selection,
					tt.wantSelection,
				},
				{
					"diffAdded",
					styles.diffAdded,
					tt.wantAdded,
				}, {
					"diffRemoved",
					styles.diffRemoved,
					tt.wantRemoved,
				},
			} {
				want := each.want
				if want == nil {
					want = lipgloss.NoColor{}
				}
				if got := each.style.GetBackground(); !isSameColor(got, want) {
					t.Fatalf("%s background = %v, want %v", each.name, got, each.want)
				}
			}
			// 底色不改变文字颜色：选中行仍用强调色，diff 增删行仍用成功与失败色。
			for _, pair := range [][2]lipgloss.Style{{styles.selection, styles.accent}, {styles.diffAdded, styles.success}, {styles.diffRemoved, styles.failure}} {
				if got, want := pair[0].GetForeground(), pair[1].GetForeground(); !isSameColor(got, want) {
					t.Fatalf("foreground = %v, want %v", got, want)
				}
			}
			if styles.hasBackground != (tt.wantSurface != nil) {
				t.Fatalf("hasBackground = %v", styles.hasBackground)
			}
			if tt.wantSurface == nil && (styles.surface.Render("x") != "x" || styles.selection.Render("x") != styles.accent.Render("x")) {
				t.Fatalf("styles without background changed: surface %q, selection %q", styles.surface.Render("x"), styles.selection.Render("x"))
			}
		})
	}
}

// isSameColor 两个颜色是否相同？
func isSameColor(got, want color.Color) bool {
	// 1. nil 只和 nil 相同，NoColor 只和 NoColor 相同。
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	_, isGotNoColor := got.(lipgloss.NoColor)
	_, isWantNoColor := want.(lipgloss.NoColor)
	if isGotNoColor || isWantNoColor {
		return isGotNoColor == isWantNoColor
	}
	// 2. 读取两组 RGBA 分量并逐项比较。
	gotR, gotG, gotB, gotA := got.RGBA()
	wantR, wantG, wantB, wantA := want.RGBA()
	return gotR == wantR && gotG == wantG && gotB == wantB && gotA == wantA
}

// TestSplitMarkdownBlocks 检查顶层块类型和原文切分位置。
func TestSplitMarkdownBlocks(t *testing.T) {
	// 1. 准备段落、列表、引用、代码围栏和表格。
	tests := []struct {
		name   string
		source string
		want   []markdownBlock
	}{
		{
			name:   "blank",
			source: "\n\n",
			want:   nil,
		},
		{
			name:   "paragraph_list_quote",
			source: "\n段落\n\n1. A\n\n1. B\n\n> q1\n>\n> q2\n",
			want: []markdownBlock{
				{
					source: "\n段落\n\n",
					kind:   markdownBlockText,
				},
				{
					source: "1. A\n\n1. B\n\n",
					kind:   markdownBlockText,
				},
				{
					source: "> q1\n>\n> q2\n",
					kind:   markdownBlockText,
				},
			},
		},
		{
			name:   "code_and_table",
			source: "```go\nx\n```\n\n    indented\n\n| a |\n|---|\n| 1 |\n",
			want: []markdownBlock{
				{
					source: "```go\nx\n```\n\n",
					kind:   markdownBlockCode,
				},
				{
					source: "    indented\n\n",
					kind:   markdownBlockCode,
				},
				{
					source: "| a |\n|---|\n| 1 |\n",
					kind:   markdownBlockTable,
				},
			},
		},
		{
			name:   "indented_list_starts_at_line_start",
			source: "前\n\n  - 缩进的列表\n",
			want: []markdownBlock{{
				source: "前\n\n",
				kind:   markdownBlockText,
			}, {
				source: "  - 缩进的列表\n",
				kind:   markdownBlockText,
			}},
		},
		{
			// 表格紧跟段落时使用表头行位置切分。
			name:   "table_directly_after_paragraph",
			source: "说明：\n| a | b |\n|---|---|\n| 1 | 2 |\n",
			want: []markdownBlock{
				{
					source: "说明：\n",
					kind:   markdownBlockText,
				},
				{
					source: "| a | b |\n|---|---|\n| 1 | 2 |\n",
					kind:   markdownBlockTable,
				},
			},
		},
		{
			name:   "backtick_in_info_string_is_not_a_fence",
			source: "```foo``` 段落\n第二行\n第三行\n",
			want: []markdownBlock{{
				source: "```foo``` 段落\n第二行\n第三行\n",
				kind:   markdownBlockText,
			}},
		},
		{
			name:   "fence_with_indented_fence_lines_is_one_block",
			source: "```md\n- a\n\n    ```bash\n    ls\n    ```\n",
			want: []markdownBlock{{
				source: "```md\n- a\n\n    ```bash\n    ls\n    ```\n",
				kind:   markdownBlockCode,
			}},
		},
	}
	// 2. 逐项比较切分后的原文和块类型。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := splitMarkdownBlocks(tt.source); !slices.Equal(got, tt.want) {
				t.Fatalf("blocks = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestMarkdownStream 检查增量定稿、围栏识别和实时尾部。
func TestMarkdownStream(t *testing.T) {
	// 1. 准备代码行生成方法和各类增量输入场景。
	codeLines := func(count int) string {
		var text strings.Builder
		for index := range count {
			fmt.Fprintf(&text, "line%d\n", index)
		}
		return text.String()
	}
	tests := []struct {
		name string
		run  func(t *testing.T, s *markdownStream)
	}{
		{
			name: "paragraph_finalized_when_next_block_starts",
			run: func(t *testing.T, s *markdownStream) {
				// "第二" 这一行还没写完，第一段要等它完整后才定稿。
				requireStable(t, s, "第一段\n\n第二", "")
				requireStable(t, s, "段\n", "第一段\n\n")
				if got := s.getLiveSource(10); got != "第二段\n" {
					t.Fatalf("live = %q", got)
				}
			},
		},
		{
			// 最后一行没写完时不参与切分："1" 可能是 "1. B" 的开头。
			name: "ordered_list_with_blank_line_stays_together",
			run: func(t *testing.T, s *markdownStream) {
				requireStable(t, s, "1. A\n\n1", "")
				requireStable(t, s, ". B\n\n后", "")
				requireStable(t, s, "文\n", "1. A\n\n1. B\n\n")
				if got := s.getLiveSource(10); got != "后文\n" {
					t.Fatalf("live = %q", got)
				}
			},
		},
		{
			name: "unclosed_fence_scans_only_new_lines",
			run: func(t *testing.T, s *markdownStream) {
				requireStable(t, s, "前文\n\n```go\n", "前文\n\n")
				if s.openFence != "```" {
					t.Fatalf("open fence = %q", s.openFence)
				}
				requireStable(t, s, codeLines(50), "")
				scanned := s.fenceScannedLength
				requireStable(t, s, "x := 1\n", "")
				if s.fenceScannedLength != scanned+len("x := 1\n") {
					t.Fatalf("scanned %d → %d, want only the new line", scanned, s.fenceScannedLength)
				}
				// 围栏闭合后定稿到该行末尾，后续空行留给下一块。
				requireStable(t, s, "```\n\n后", "```go\n"+codeLines(50)+"x := 1\n```\n")
				requireStable(t, s, "\n", "")
				if s.openFence != "" || s.getLiveSource(10) != "\n后\n" {
					t.Fatalf("open fence = %q, live = %q", s.openFence, s.getLiveSource(10))
				}
			},
		},
		{
			name: "unclosed_fence_live_source_keeps_last_lines",
			run: func(t *testing.T, s *markdownStream) {
				requireStable(t, s, "```go\n"+codeLines(100)+"partial", "")
				if got, want := s.getLiveSource(3), "```go\nline98\nline99\npartial\n```\n"; got != want {
					t.Fatalf("live = %q, want %q", got, want)
				}
			},
		},
		{
			// 同一增量内闭合的围栏定稿到该行末尾，后续空行留给下一块。
			name: "fence_closed_in_same_delta",
			run: func(t *testing.T, s *markdownStream) {
				requireStable(t, s, "```\na\n```\n\n后\n", "```\na\n```\n")
				if s.openFence != "" || s.getLiveSource(10) != "\n后\n" {
					t.Fatalf("open fence = %q, live = %q", s.openFence, s.getLiveSource(10))
				}
			},
		},
		{
			// 闭合围栏行写完即定稿，不等待下一行写完。
			name: "closed_fence_finalized_before_next_line_completes",
			run: func(t *testing.T, s *markdownStream) {
				requireStable(t, s, "前文\n\n```go\nx\n```", "前文\n\n")
				requireStable(t, s, "\n下", "```go\nx\n```\n")
				if got := s.getLiveSource(10); got != "下" {
					t.Fatalf("live = %q", got)
				}
				if s.getFinalizedLength() != len("前文\n\n```go\nx\n```\n") {
					t.Fatalf("finalized = %d", s.getFinalizedLength())
				}
			},
		},
		{
			name: "consecutive_closed_fences_finalized_together",
			run: func(t *testing.T, s *markdownStream) {
				requireStable(t, s, "```\na\n```\n~~~\nb\n~~~\n\n第二段\n\n第三段\n", "```\na\n```\n~~~\nb\n~~~\n\n第二段\n\n")
				if got := s.getLiveSource(10); got != "第三段\n" {
					t.Fatalf("live = %q", got)
				}
			},
		},
		{
			name: "snapshot_with_closed_fence_finalizes_on_first_take",
			run: func(t *testing.T, _ *markdownStream) {
				s := newMarkdownStream("```\na\n```\n下", 0)
				requireStable(t, s, "", "```\na\n```\n")
			},
		},
		{
			// 4 个反引号的围栏里，3 个反引号的行是正文。
			name: "longer_fence_ignores_shorter_closing_line",
			run: func(t *testing.T, s *markdownStream) {
				requireStable(t, s, "````md\n```go\nx\n```\n", "")
				if s.openFence != "````" {
					t.Fatalf("open fence = %q", s.openFence)
				}
				requireStable(t, s, "````\n下", "````md\n```go\nx\n```\n````\n")
			},
		},
		{
			// 围栏行可以缩进 1–3 个空格；缩进 4 个以上的是缩进代码块，不是围栏。
			name: "fence_indent_up_to_three_spaces",
			run: func(t *testing.T, s *markdownStream) {
				requireStable(t, s, "  ```go\n  x\n", "")
				if s.openFence != "```" {
					t.Fatalf("open fence = %q", s.openFence)
				}
				requireStable(t, s, "  ```\n下", "  ```go\n  x\n  ```\n")
				requireStable(t, s, "段\n\n    ```\n    y\n    ```\n", "下段\n\n")
				if s.openFence != "" {
					t.Fatalf("indented code block tracked as fence %q", s.openFence)
				}
			},
		},
		{
			// 缩进四格及以上的反引号行作为围栏正文。
			name: "indented_fence_line_inside_fence_is_content",
			run: func(t *testing.T, s *markdownStream) {
				requireStable(t, s, "```md\n- a\n\n    ```bash\n    ls\n    ```\n", "")
				if s.openFence != "```" {
					t.Fatalf("open fence = %q", s.openFence)
				}
				requireStable(t, s, "```\n下", "```md\n- a\n\n    ```bash\n    ls\n    ```\n```\n")
			},
		},
		{
			// 围栏信息串含反引号时，该行作为带行内代码的段落。
			name: "backtick_in_info_string_is_not_a_fence",
			run: func(t *testing.T, s *markdownStream) {
				requireStable(t, s, "```foo``` 段落\n第二行\n", "")
				if s.openFence != "" {
					t.Fatalf("open fence = %q", s.openFence)
				}
				requireStable(t, s, "\n后\n", "```foo``` 段落\n第二行\n\n")
			},
		},
		{
			name: "take_rest_finalizes_everything",
			run: func(t *testing.T, s *markdownStream) {
				requireStable(t, s, "第一段\n\n第二段\n\n第三段没写完", "第一段\n\n")
				if rest := s.takeRest(); rest != "第二段\n\n第三段没写完" {
					t.Fatalf("rest = %q", rest)
				}
				if s.getFinalizedLength() != len("第一段\n\n第二段\n\n第三段没写完") || s.getLiveSource(10) != "" {
					t.Fatalf("finalized = %d, live = %q", s.getFinalizedLength(), s.getLiveSource(10))
				}
			},
		},
		{
			name: "snapshot_keeps_finalized_prefix",
			run: func(t *testing.T, _ *markdownStream) {
				s := newMarkdownStream("第一段\n\n第二段", len("第一段\n\n"))
				if s.getLiveSource(10) != "第二段" || s.getFinalizedLength() != len("第一段\n\n") {
					t.Fatalf("live = %q, finalized = %d", s.getLiveSource(10), s.getFinalizedLength())
				}
				short := newMarkdownStream("短", 100)
				if short.getFinalizedLength() != len("短") || short.getLiveSource(10) != "" {
					t.Fatal("finalized length beyond the source not clamped")
				}
			},
		},
	}
	// 2. 每个场景用一个新的空 Markdown 流运行。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, newMarkdownStream("", 0))
		})
	}
}

// requireStable 追加增量并检查本次定稿原文。
func requireStable(t *testing.T, s *markdownStream, text, want string) {
	// 1. 追加文本，取出可定稿块并与预期比较。
	t.Helper()
	s.appendText(text)
	if got := s.takeStableBlocks(); got != want {
		t.Fatalf("after %q stable = %q, want %q", text, got, want)
	}
}

// streamMaterials 保存流式渲染对照使用的段落、列表、引用、表格和代码素材。
var streamMaterials = map[string]string{
	"paragraphs":              "第一段文字，说明背景。\n\n第二段 with English words and `code`.\n\n第三段结尾。\n",
	"ordered_list_with_blank": "步骤：\n\n1. 第一步\n\n1. 第二步\n\n1. 第三步\n\n完成。\n",
	"list_item_code":          "- 运行：\n\n  ```bash\n  go test ./...\n  ```\n- 检查输出\n\n结论段落。\n",
	"quote_paragraphs":        "> 引用第一段\n>\n> 引用第二段\n\n正文。\n",
	"table":                   "| 名称 | 数量 |\n|------|------|\n| 苹果 | 3 |\n| 香蕉 | 5 |\n\n表后段落。\n",
	"code_block":              "示例：\n\n```go\nfunc main() {\n\tfmt.Println(\"你好\")\n}\n```\n\n结束。\n",
	"headings_and_rule":       "# 标题\n\n## 小节\n\n内容。\n\n---\n\n尾声\n",
	"no_trailing_newline":     "第一段\n\n第二段没有换行",
	"tilde_fence":             "说明：\n\n~~~python\nprint(\"a\")\n~~~\n\n后文。\n",
	"four_backtick_fence":     "示例：\n\n````md\n```go\nx := 1\n```\n````\n\n后文。\n",
	"indented_fence_1_to_3":   "前文\n\n  ```go\n  x := 1\n  ```\n\n 后文。\n\n   ~~~\n   y\n   ~~~\n尾声\n",
	"fence_body_indented_4":   "```md\n- a\n\n    ```bash\n    ls\n    ```\n```\n\n后文。\n",
	"backtick_info_string":    "```foo``` 段落\n第二行\n第三行\n\n后文。\n",
	"adjacent_fences":         "```\na\n```\n```\nb\n```\n后文。\n",
	"table_after_paragraph":   "说明：\n| a | b |\n|---|---|\n| 1 | 2 |\n\n后文。\n",
	"long_cjk_wraps":          strings.Repeat("中文长段落需要在宽度处折行，", 8) + "\n\n- 列表项也很长很长很长很长很长很长很长很长很长很长很长\n",
	// goldmark 只把 ASCII 空格、制表符与换行当空白：NBSP 跟在围栏后不是闭合围栏，U+3000 开头的行不是围栏。
	"nbsp_after_closing_fence":         "```\ncode\n```\u00a0\n后文。\n\n```\n真正闭合\n```\n\n结尾。\n",
	"ideographic_space_before_opening": "\u3000```go\nx := 1\n```\n\n后文。\n",
}

// streamChunkers 是把素材切成增量的方式；按字符切，不拆开 UTF-8。
var streamChunkers = map[string]func(string) []string{
	"rune_1":  func(text string) []string { return splitEveryRunes(text, 1) },
	"runes_7": func(text string) []string { return splitEveryRunes(text, 7) },
	"lines":   func(text string) []string { return strings.SplitAfter(text, "\n") },
	"whole":   func(text string) []string { return []string{text} },
}

// splitEveryRunes 按固定字符数切分字符串。
func splitEveryRunes(text string, count int) []string {
	// 1. 按字符切片取出每一段，保留完整 UTF-8 字符。
	runes := []rune(text)
	var chunks []string
	for start := 0; start < len(runes); start += count {
		chunks = append(chunks, string(runes[start:min(start+count, len(runes))]))
	}
	return chunks
}

// TestMarkdownStreamMatchesFullRender 检查流式分段渲染与整段渲染结果相同。
func TestMarkdownStreamMatchesFullRender(t *testing.T) {
	// 1. 组合样式、宽度、素材和分段方式，读取整段渲染结果。
	for _, style := range []string{markdownStyleNoTTY, "dark"} {
		for _, width := range []int{40, 80} {
			renderer, err := newMarkdownRenderer(style, width)
			if err != nil {
				t.Fatal(err)
			}
			for materialName, material := range streamMaterials {
				want := renderer.render(material)
				for chunkerName, chunker := range streamChunkers {
					t.Run(fmt.Sprintf("%s/%d/%s/%s", style, width, materialName, chunkerName), func(t *testing.T) {
						// 1. 逐段输入并渲染已定稿块，最后渲染尾部，再与整段结果逐行比较。
						stream := newMarkdownStream("", 0)
						var got []string
						emit := func(source string) {
							lines := renderer.render(source)
							if len(lines) == 0 {
								return
							}
							if len(got) > 0 {
								got = append(got, "")
							}
							got = append(got, lines...)
						}
						for _, chunk := range chunker(material) {
							stream.appendText(chunk)
							emit(stream.takeStableBlocks())
						}
						emit(stream.takeRest())
						if !slices.Equal(got, want) {
							t.Fatalf("streamed:\n%s\n--- full:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
						}
					})
				}
			}
		}
	}
}

// BenchmarkMarkdownStreamDelta 测量不同长度回复新增一段文字的处理耗时。
func BenchmarkMarkdownStreamDelta(b *testing.B) {
	// 1. 创建渲染器；对一千行和一万行的回复，先测量段落正文的增量。
	renderer, err := newMarkdownRenderer(markdownStyleNoTTY, 100)
	if err != nil {
		b.Fatal(err)
	}
	for _, lineCount := range []int{1000, 10000} {
		b.Run(fmt.Sprintf("paragraphs_%d", lineCount), func(b *testing.B) {
			stream := newMarkdownStream("", 0)
			for index := range lineCount {
				stream.appendText(fmt.Sprintf("第 %d 行文字，用来填充很长的回复。\n", index))
				if index%5 == 4 {
					stream.appendText("\n")
				}
				stream.takeStableBlocks()
			}
			for index := 0; b.Loop(); index++ {
				stream.appendText("新增文字 ")
				if index%20 == 19 {
					stream.appendText("\n\n")
				}
				stream.takeStableBlocks()
				renderer.render(stream.getLiveSource(40))
			}
		})
		// 2. 测量闭合代码围栏后的正文增量，每五十次暂停计时重建流。
		b.Run(fmt.Sprintf("closed_fence_%d", lineCount), func(b *testing.B) {
			var code strings.Builder
			code.WriteString("```go\n")
			for index := range lineCount {
				fmt.Fprintf(&code, "x%d := %d\n", index, index)
			}
			code.WriteString("```\n下面解释：")
			var stream *markdownStream
			for index := 0; b.Loop(); index++ {
				if index%50 == 0 {
					b.StopTimer()
					stream = newMarkdownStream("", 0)
					stream.appendText(code.String())
					stream.takeStableBlocks()
					b.StartTimer()
				}
				stream.appendText("新增文字 ")
				stream.takeStableBlocks()
				renderer.render(stream.getLiveSource(40))
			}
		})
		// 3. 测量持续增长且尚未闭合的代码围栏。
		b.Run(fmt.Sprintf("unclosed_fence_%d", lineCount), func(b *testing.B) {
			stream := newMarkdownStream("", 0)
			stream.appendText("```go\n")
			for index := range lineCount {
				stream.appendText(fmt.Sprintf("x%d := %d\n", index, index))
				stream.takeStableBlocks()
			}
			for index := 0; b.Loop(); index++ {
				stream.appendText(fmt.Sprintf("y%d := %d\n", index, index))
				stream.takeStableBlocks()
				renderer.render(stream.getLiveSource(40))
			}
		})
	}
}
