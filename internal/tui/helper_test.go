package tui

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/charmbracelet/x/ansi"
)

// TestFormatElapsed 检查耗时文字的秒、分秒和小时格式。
func TestFormatElapsed(t *testing.T) {
	// 1. 列出负耗时、秒、分钟和小时场景，比较格式化文字。
	tests := []struct {
		name    string
		elapsed time.Duration
		want    string
	}{
		{
			name:    "negative_is_zero",
			elapsed: -time.Second,
			want:    "0s",
		},
		{
			name:    "seconds",
			elapsed: 12*time.Second + 900*time.Millisecond,
			want:    "12s",
		},
		{
			name:    "minutes",
			elapsed: 3*time.Minute + 5*time.Second,
			want:    "3m05s",
		},
		{
			name:    "hours",
			elapsed: time.Hour + 2*time.Minute + 30*time.Second,
			want:    "1h02m",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatElapsed(tt.elapsed); got != tt.want {
				t.Fatalf("formatElapsed(%v) = %q, want %q", tt.elapsed, got, tt.want)
			}
		})
	}
}

// TestGetDisplayDirectory 检查家目录缩写与其他目录原样显示。
func TestGetDisplayDirectory(t *testing.T) {
	// 1. 固定 HOME，比较家目录、子目录、相似前缀和空目录。
	t.Setenv("HOME", "/home/tester")
	tests := []struct {
		name      string
		directory string
		want      string
	}{
		{
			name:      "home_directory",
			directory: "/home/tester",
			want:      "~",
		},
		{
			name:      "under_home_directory",
			directory: "/home/tester/code/openvaala",
			want:      "~/code/openvaala",
		},
		{
			name:      "outside_home_directory",
			directory: "/srv/data",
			want:      "/srv/data",
		},
		{
			name:      "sibling_with_home_as_prefix",
			directory: "/home/tester2/code",
			want:      "/home/tester2/code",
		},
		{
			name:      "empty",
			directory: "",
			want:      "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getDisplayDirectory(tt.directory); got != tt.want {
				t.Fatalf("getDisplayDirectory(%q) = %q, want %q", tt.directory, got, tt.want)
			}
		})
	}
}

// TestFormatExecutorsChange 检查 Executor 变更说明的顺序和目录显示。
func TestFormatExecutorsChange(t *testing.T) {
	// 1. 固定 HOME，构造默认项、范围和目录变化。
	t.Setenv("HOME", "/home/me")
	allKindsPrevious := []defs.TurnExecutor{
		{
			ID:                 "local",
			WorkspaceDirectory: "/home/me/app",
		}, {
			ID:                 "gpu",
			WorkspaceDirectory: "/data/proj",
		}, {
			ID:                 "openvaala",
			WorkspaceDirectory: "/srv",
		},
	}
	allKindsCurrent := []defs.TurnExecutor{
		{
			ID:                 "gpu",
			WorkspaceDirectory: "/data/proj",
		}, {
			ID:                 "sandbox",
			WorkspaceDirectory: "/workspace",
		}, {
			ID:                 "local",
			WorkspaceDirectory: "/home/me/code/bar",
		},
	}
	tests := []struct {
		name          string
		previous      []defs.TurnExecutor
		current       []defs.TurnExecutor
		isLocalServer bool
		want          string
	}{
		{
			name: "default_and_removed",
			previous: []defs.TurnExecutor{{
				ID:                 "local",
				WorkspaceDirectory: "/a",
			}, {
				ID:                 "openvaala",
				WorkspaceDirectory: "/workspace",
			}},
			current: []defs.TurnExecutor{{
				ID:                 "gpu",
				WorkspaceDirectory: "/data",
			}, {
				ID:                 "local",
				WorkspaceDirectory: "/a",
			}},
			want: "Executor 已变更：默认 gpu:/data · 移除 openvaala",
		},
		{
			// 本机 serve 的 local 家目录缩写为 ~，其他 Executor 目录原样显示。
			name:          "all_kinds_in_order_on_local_server",
			previous:      allKindsPrevious,
			current:       allKindsCurrent,
			isLocalServer: true,
			want:          "Executor 已变更：默认 gpu:/data/proj · 移除 openvaala · 新增 sandbox · local 目录 ~/code/bar",
		},
		{
			// 远端 serve 的 local 目录原样显示。
			name:     "all_kinds_in_order_on_remote_server",
			previous: allKindsPrevious,
			current:  allKindsCurrent,
			want:     "Executor 已变更：默认 gpu:/data/proj · 移除 openvaala · 新增 sandbox · local 目录 /home/me/code/bar",
		},
		{
			// 默认项和它的目录同时变化时，目录只出现在默认项说明中。
			name: "new_default_directory_not_repeated",
			previous: []defs.TurnExecutor{{
				ID:                 "local",
				WorkspaceDirectory: "/a",
			}, {
				ID:                 "ssh",
				WorkspaceDirectory: "/y",
			}},
			current: []defs.TurnExecutor{{
				ID:                 "ssh",
				WorkspaceDirectory: "/x",
			}, {
				ID:                 "local",
				WorkspaceDirectory: "/a",
			}},
			want: "Executor 已变更：默认 ssh:/x",
		},
		{
			name: "local_default_on_local_server",
			previous: []defs.TurnExecutor{{
				ID:                 "gpu",
				WorkspaceDirectory: "/data/proj",
			}, {
				ID:                 "local",
				WorkspaceDirectory: "/home/me/app",
			}},
			current: []defs.TurnExecutor{{
				ID:                 "local",
				WorkspaceDirectory: "/home/me/app",
			}, {
				ID:                 "gpu",
				WorkspaceDirectory: "/data/proj",
			}},
			isLocalServer: true,
			want:          "Executor 已变更：默认 local:~/app",
		},
		{
			name: "all_removed",
			previous: []defs.TurnExecutor{{
				ID:                 "local",
				WorkspaceDirectory: "/a",
			}},
			want: "Executor 已变更：移除 local",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			change := &defs.ExecutorsChange{
				PreviousExecutors: tt.previous,
				Executors:         tt.current,
			}
			if got := formatExecutorsChange(change, tt.isLocalServer); got != tt.want {
				t.Fatalf("formatExecutorsChange() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestFormatActiveTime 检查本地活跃时间和跨日日期的显示。
func TestFormatActiveTime(t *testing.T) {
	// 1. 固定时区和时刻，列出分钟、小时、昨天、跨年和未知时间。
	location := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 10, 13, 15, 0, 0, 0, location)
	at := func(value time.Time) int64 { return value.UnixMilli() }
	at2 := func(year int, month time.Month, day, hour, minute int) int64 {
		return at(time.Date(year, month, day, hour, minute, 0, 0, location))
	}
	tests := []struct {
		name      string
		now       time.Time // 为零时使用上面的 now
		updatedAt int64
		want      string
	}{
		{
			name:      "unknown",
			updatedAt: 0,
			want:      "—",
		},
		{
			name:      "just_now",
			updatedAt: at(now.Add(-30 * time.Second)),
			want:      "刚刚",
		},
		{
			name:      "future_is_just_now",
			updatedAt: at(now.Add(10 * time.Second)),
			want:      "刚刚",
		},
		{
			name:      "minutes",
			updatedAt: at(now.Add(-5 * time.Minute)),
			want:      "5 分钟前",
		},
		{
			name:      "hours_today",
			updatedAt: at(now.Add(-3 * time.Hour)),
			want:      "3 小时前",
		},
		{
			name:      "yesterday",
			updatedAt: at(time.Date(2026, 10, 12, 14, 30, 0, 0, location)),
			want:      "昨天 14:30",
		},
		{
			name:      "this_year",
			updatedAt: at(time.Date(2026, 9, 28, 10, 2, 0, 0, location)),
			want:      "09-28 10:02",
		},
		{
			name:      "earlier_year",
			updatedAt: at(time.Date(2025, 12, 31, 23, 0, 0, 0, location)),
			want:      "2025-12-31",
		},
		{
			name:      "59_seconds_is_just_now",
			updatedAt: at(now.Add(-59 * time.Second)),
			want:      "刚刚",
		},
		{
			name:      "60_seconds_is_one_minute",
			updatedAt: at(now.Add(-60 * time.Second)),
			want:      "1 分钟前",
		},
		{
			name:      "59_minutes",
			updatedAt: at(now.Add(-59 * time.Minute)),
			want:      "59 分钟前",
		},
		{
			name:      "60_minutes_same_day_is_one_hour",
			updatedAt: at(now.Add(-60 * time.Minute)),
			want:      "1 小时前",
		},
		{
			name:      "yesterday_across_midnight",
			now:       time.Date(2026, 10, 13, 0, 30, 0, 0, location),
			updatedAt: at2(2026, 10, 12, 23, 0),
			want:      "昨天 23:00",
		},
		{
			name:      "yesterday_across_new_year",
			now:       time.Date(2026, 1, 1, 0, 10, 0, 0, location),
			updatedAt: at2(2025, 12, 31, 23, 0),
			want:      "昨天 23:00",
		},
		{
			name:      "two_days_across_new_year",
			now:       time.Date(2026, 1, 2, 9, 0, 0, 0, location),
			updatedAt: at2(2025, 12, 30, 9, 0),
			want:      "2025-12-30",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			current := tt.now
			if current.IsZero() {
				current = now
			}
			if got := formatActiveTime(tt.updatedAt, current); got != tt.want {
				t.Fatalf("formatActiveTime() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestFormatResumeRow 检查对话列表行的截断和信息列省略。
func TestFormatResumeRow(t *testing.T) {
	// 1. 准备对话标题、活跃时间、消息数和短 ID，列出不同宽度与标题场景。
	now := time.Date(2026, 10, 13, 15, 0, 0, 0, time.FixedZone("CST", 8*3600))
	conversation := defs.Conversation{
		ID:           "0199e3a1-75be-7a21-a2ce-cf424f845594",
		Title:        "周报整理",
		UpdatedAt:    now.Add(-2 * time.Minute).UnixMilli(),
		MessageCount: 12,
	}
	tests := []struct {
		name     string
		title    string // 为空时使用 conversation 的标题
		mark     string
		width    int
		wants    []string
		notWants []string
	}{
		{
			name:  "all_columns",
			width: 80,
			wants: []string{"周报整理", "2 分钟前", "12 条", "4f845594"},
		},
		{
			name:     "narrow_width_drops_id",
			width:    40,
			wants:    []string{"周报整理", "2 分钟前", "12 条"},
			notWants: []string{"4f845594"},
		},
		{
			name:     "narrow_width_drops_columns",
			width:    30,
			wants:    []string{"周报整理", "2 分钟前"},
			notWants: []string{"12 条", "4f845594"},
		},
		{
			name:  "tiny_width_stays_within",
			width: 16,
			wants: []string{"周报"},
		},
		{
			name:     "long_cjk_title_truncated_keeps_columns",
			title:    "整理本周的项目周报并同步给全部团队成员",
			width:    40,
			wants:    []string{"…  ", "2 分钟前", "12 条"},
			notWants: []string{"4f845594", "成员"},
		},
		{
			name:     "long_title_truncated_before_mark",
			title:    "整理本周的项目周报并同步给全部团队成员",
			mark:     " 分叉",
			width:    40,
			wants:    []string{"… 分叉  ", "2 分钟前", "12 条"},
			notWants: []string{"成员"},
		},
		{
			name:     "title_with_newline_and_tab_keeps_columns",
			title:    "周报\n整理\t完成",
			width:    40,
			wants:    []string{"周报 整理", "2 分钟前", "12 条"},
			notWants: []string{"4f845594", "\n", "\t"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := conversation
			if tt.title != "" {
				target.Title = tt.title
			}
			row := formatResumeRow(" ▸ ", target, tt.mark, now, tt.width)
			if ansi.StringWidth(row) > tt.width {
				t.Fatalf("row %q width %d exceeds %d", row, ansi.StringWidth(row), tt.width)
			}
			for _, want := range tt.wants {
				if !strings.Contains(row, want) {
					t.Fatalf("row %q lacks %q", row, want)
				}
			}
			for _, notWant := range tt.notWants {
				if strings.Contains(row, notWant) {
					t.Fatalf("row %q should omit %q", row, notWant)
				}
			}
		})
	}
}

// TestFormatTokens 检查 token 数量的单位和进位显示。
func TestFormatTokens(t *testing.T) {
	// 1. 比较整数、K、M 及 999_950 附近的取整结果。
	tests := []struct {
		tokens int
		want   string
	}{
		{
			0,
			"0",
		}, {
			999,
			"999",
		}, {
			1000,
			"1K",
		}, {
			1234,
			"1.2K",
		}, {
			39800,
			"39.8K",
		}, {
			128000,
			"128K",
		}, {
			1_500_000,
			"1.5M",
		},
		// 取整后满 1000K 的数量使用 M 单位。
		{
			999_949,
			"999.9K",
		}, {
			999_950,
			"1M",
		},
	}
	for _, tt := range tests {
		if got := formatTokens(tt.tokens); got != tt.want {
			t.Fatalf("formatTokens(%d) = %q, want %q", tt.tokens, got, tt.want)
		}
	}
}

// TestGetCodeFenceOpening 检查 Markdown 代码围栏起始行的识别。
func TestGetCodeFenceOpening(t *testing.T) {
	// 1. 分别比较反引号、波浪号、说明文字、普通文本和空行。
	tests := []struct {
		name string
		line string
		want string
	}{
		{
			name: "backtick_with_info",
			line: "```go",
			want: "```",
		},
		{
			name: "tilde",
			line: "~~~",
			want: "~~~",
		},
		{
			name: "longer_fence",
			line: "````md",
			want: "````",
		},
		{
			name: "tilde_info_may_contain_backtick",
			line: "~~~a`b",
			want: "~~~",
		},
		// 反引号围栏的说明文字含反引号时不识别为起始围栏。
		{
			name: "backtick_in_backtick_info",
			line: "```foo``` 段落",
			want: "",
		},
		{
			name: "two_backticks",
			line: "``x``",
			want: "",
		},
		{
			name: "plain_text",
			line: "文字",
			want: "",
		},
		{
			name: "empty",
			line: "",
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getCodeFenceOpening(tt.line); got != tt.want {
				t.Fatalf("getCodeFenceOpening(%q) = %q, want %q", tt.line, got, tt.want)
			}
		})
	}
}

// TestGetProgressLine 检查工具输出最后一行的清理和提取。
func TestGetProgressLine(t *testing.T) {
	// 1. 列出空行、样式、制表符、回车进度、非文本和空内容。
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "last_non_empty_line",
			content: `"ok  a  1.2s\n\n"`,
			want:    "ok  a  1.2s",
		},
		{
			name:    "strips_styles_and_expands_tabs",
			content: `"\u001b[31mFAIL\u001b[0m\tpkg\n"`,
			want:    "FAIL    pkg",
		},
		{
			name:    "blank_output",
			content: `"  \n\n"`,
			want:    "",
		},
		{
			name:    "progress_bar_keeps_latest_state",
			content: `"a 10%\ra 20%\n"`,
			want:    "a 20%",
		},
		{
			name:    "trailing_carriage_return",
			content: `"Downloading 20%\r"`,
			want:    "Downloading 20%",
		},
		{
			name:    "crlf_line_ending",
			content: `"ok\r\n"`,
			want:    "ok",
		},
		{
			name:    "styled_progress_bar",
			content: `"\u001b[32m 50%\u001b[0m\r\u001b[32m 60%\u001b[0m"`,
			want:    "60%",
		},
		{
			name:    "blank_after_last_carriage_return",
			content: `"abc\r  \n"`,
			want:    "abc",
		},
		{
			name:    "not_text",
			content: `{"exit_code":0}`,
			want:    "",
		},
		{
			name:    "empty_content",
			content: ``,
			want:    "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getProgressLine(json.RawMessage(tt.content)); got != tt.want {
				t.Fatalf("getProgressLine(%s) = %q, want %q", tt.content, got, tt.want)
			}
		})
	}
}

// TestFormatJSON 检查 JSON 缩进和非 JSON 文本保留。
func TestFormatJSON(t *testing.T) {
	// 1. 比较合法对象、普通文字和空文本。
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "object_is_indented",
			text: `{"a":1,"b":[true]}`,
			want: "{\n  \"a\": 1,\n  \"b\": [\n    true\n  ]\n}",
		},
		{
			name: "not_json_is_kept",
			text: "exit 1\nboom",
			want: "exit 1\nboom",
		},
		{
			name: "empty_is_kept",
			text: "",
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatJSON(tt.text); got != tt.want {
				t.Fatalf("formatJSON(%q) = %q, want %q", tt.text, got, tt.want)
			}
		})
	}
}

// TestFormatToolResultContent 检查命令结果的状态、退出码和输出显示。
func TestFormatToolResultContent(t *testing.T) {
	// 1. 列出已退出、运行中、失败的命令结果，以及非命令 JSON、停止命令的结果和空内容。
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "exited_command_shows_exit_code",
			content: `{"command_id":"cmd_1","status":"exited","exit_code":0,"output":"a\nb\n\n","next_cursor":5,"has_more":false}`,
			want:    "exit 0\na\nb",
		},
		{
			name:    "exited_command_without_output",
			content: `{"command_id":"cmd_1","status":"exited","exit_code":2,"output":"","next_cursor":0,"has_more":false}`,
			want:    "exit 2\n",
		},
		{
			name:    "running_command",
			content: `{"command_id":"cmd_1","status":"running","exit_code":null,"output":"building\n","next_cursor":9,"has_more":false}`,
			want:    "running\nbuilding",
		},
		{
			name:    "failed_command",
			content: `{"command_id":"cmd_1","status":"failed","exit_code":null,"error":"connection lost","output":"partial","next_cursor":7,"has_more":false}`,
			want:    "failed: connection lost\npartial",
		},
		{
			name:    "other_json_is_kept",
			content: `{"ok":true}`,
			want:    `{"ok":true}`,
		},
		{
			name:    "stop_command_result_is_kept",
			content: `{"command_id":"cmd_1","status":"exited","exit_code":-1,"stop_reason":"stopped"}`,
			want:    `{"command_id":"cmd_1","status":"exited","exit_code":-1,"stop_reason":"stopped"}`,
		},
		{
			name:    "empty_content",
			content: "",
			want:    "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatToolResultContent(json.RawMessage(tt.content)); got != tt.want {
				t.Fatalf("formatToolResultContent(%q) = %q, want %q", tt.content, got, tt.want)
			}
		})
	}
}

// TestGetReasoningText 检查推理文字优先使用摘要。
func TestGetReasoningText(t *testing.T) {
	// 1. 比较只有摘要、只有正文、两者相同、两者不同、都为空和空白摘要的结果。
	tests := []struct {
		name    string
		summary []string
		content []string
		want    string
	}{
		{
			name:    "summary_only",
			summary: []string{"分析", "问题"},
			want:    "分析问题",
		},
		{
			name:    "content_only",
			content: []string{"raw ", "thinking"},
			want:    "raw thinking",
		},
		{
			name:    "same_text_shown_once",
			summary: []string{"Let me look."},
			content: []string{"Let me look."},
			want:    "Let me look.",
		},
		{
			name:    "different_prefers_summary",
			summary: []string{"摘要"},
			content: []string{"正文"},
			want:    "摘要",
		},
		{
			name: "both_empty",
			want: "",
		},
		{
			name:    "blank_summary_falls_back",
			summary: []string{"  "},
			content: []string{" 正文 "},
			want:    "正文",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getReasoningText(tt.summary, tt.content); got != tt.want {
				t.Fatalf("getReasoningText = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestGetTextTail 检查末尾文字的截断和终端控制清理。
func TestGetTextTail(t *testing.T) {
	// 1. 列出多行、宽字符、制表符和控制字符，比较文字及显示宽度。
	tests := []struct {
		name  string
		text  string
		width int
		want  string
	}{
		{
			name:  "short",
			text:  "hello",
			width: 10,
			want:  "hello",
		},
		{
			name:  "last_non_empty_line",
			text:  "first\nsecond\n\n",
			width: 20,
			want:  "second",
		},
		{
			name:  "long_keeps_end",
			text:  "abcdefghij",
			width: 6,
			want:  "…fghij",
		},
		{
			name:  "wide_chars_keep_end",
			text:  "一二三四五",
			width: 5,
			want:  "…四五",
		},
		{
			name:  "strips_controls",
			text:  "\x1b[31mred\x1b[0m done\r\n",
			width: 20,
			want:  "red done",
		},
		{
			name:  "tabs_expanded",
			text:  "a\tb",
			width: 10,
			want:  "a    b",
		},
		{
			name:  "empty",
			text:  "",
			width: 10,
			want:  "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getTextTail(tt.text, tt.width)
			if got != tt.want || ansi.StringWidth(got) > tt.width {
				t.Fatalf("getTextTail = %q (width %d), want %q", got, ansi.StringWidth(got), tt.want)
			}
		})
	}
}

// TestFormatFieldRows 检查面板字段标签和值的对齐与折行。
func TestFormatFieldRows(t *testing.T) {
	// 1. 创建无颜色样式，分别渲染标签宽度、长值、带样式值和空字段。
	plain := newTUIStyles(markdownStyleNoTTY, nil)
	t.Run("labels_aligned_to_widest", func(t *testing.T) {
		rows := formatFieldRows([]panelField{{
			label: "标题",
			value: "修复",
		}, {
			label: "Session",
			value: "s1",
		}}, 0, 40, plain)
		requirePrinted(t, rows, "  标题     修复", "  Session  s1")
	})
	t.Run("panel_label_width_wider_than_section", func(t *testing.T) {
		// 1. 按传入的较大标签宽度补齐本节。
		rows := formatFieldRows([]panelField{{
			label: "ID",
			value: "c1",
		}, {
			label: "标题",
			value: "修复",
		}}, 8, 40, plain)
		requirePrinted(t, rows, "  ID        c1", "  标题      修复")
	})
	t.Run("section_label_wider_than_panel_width", func(t *testing.T) {
		rows := formatFieldRows([]panelField{{
			label: "Session",
			value: "s1",
		}}, 4, 40, plain)
		requirePrinted(t, rows, "  Session  s1")
	})
	t.Run("long_value_wraps_to_value_column", func(t *testing.T) {
		rows := formatFieldRows([]panelField{{
			label: "工具",
			value: "bash, read_file, apply_patch",
		}}, 0, 20, plain)
		if len(rows) < 2 || !strings.HasPrefix(rows[0], "  工具  bash,") {
			t.Fatalf("rows = %q", rows)
		}
		for _, row := range rows[1:] {
			if !strings.HasPrefix(row, strings.Repeat(" ", 8)) || row[8] == ' ' || ansi.StringWidth(row) > 20 {
				t.Fatalf("continuation not aligned to column 8: %q", row)
			}
		}
	})
	t.Run("styled_value_wraps_into_self_contained_rows", func(t *testing.T) {
		// 1. 渲染带样式长值，检查各行样式已经关闭且文字完整。
		colored := newTUIStyles("dark", nil)
		value := colored.faint.Render("/home/u/code/some/really/long/workspace/directory")
		rows := formatFieldRows([]panelField{{
			label: "local",
			value: value,
		}}, 0, 24, colored)
		if len(rows) < 3 {
			t.Fatalf("rows = %q", rows)
		}
		for _, row := range rows {
			if isStyleOpenAtEnd(row) {
				t.Fatalf("row ends with an open style: %q", row)
			}
		}
		if got := ansi.Strip(strings.Join(rows, "")); !strings.Contains(strings.ReplaceAll(got, " ", ""), "/home/u/code/some/really/long/workspace/directory") {
			t.Fatalf("wrapped text lost characters: %q", got)
		}
	})
	t.Run("empty_fields", func(t *testing.T) {
		if rows := formatFieldRows(nil, 8, 40, plain); len(rows) != 0 {
			t.Fatalf("rows = %q", rows)
		}
	})
}

// isStyleOpenAtEnd 行尾是否还有未关闭的 SGR 样式？
func isStyleOpenAtEnd(row string) bool {
	// 1. 提取 SGR 序列，检查最后一条是否为样式复位。
	sequences := regexp.MustCompile(`\x1b\[[0-9;:]*m`).FindAllString(row, -1)
	if len(sequences) == 0 {
		return false
	}
	last := sequences[len(sequences)-1]
	return last != "\x1b[m" && last != "\x1b[0m"
}

// TestGetFieldLabelWidth 检查跨分节的最大标签宽度。
func TestGetFieldLabelWidth(t *testing.T) {
	// 1. 比较多组标签、空分节和无分节的结果。
	tests := []struct {
		name   string
		groups [][]panelField
		want   int
	}{
		{
			name:   "widest_across_groups",
			groups: [][]panelField{{{label: "标题"}, {label: "ID"}}, {{label: "压缩模型"}}, {{label: "Session"}}},
			want:   8,
		},
		{
			name:   "empty_group_ignored",
			groups: [][]panelField{nil, {{label: "状态"}}},
			want:   4,
		},
		{
			name: "no_groups",
			want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getFieldLabelWidth(tt.groups...); got != tt.want {
				t.Fatalf("getFieldLabelWidth = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestStripTerminalControls 检查终端控制清理后保留文字、换行和制表符。
func TestStripTerminalControls(t *testing.T) {
	// 1. 列出剪贴板序列、样式、未完成转义和其他控制字符。
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "osc52_clipboard_is_dropped",
			text: "前\x1b]52;c;ZXZpbA==\x07后",
			want: "前后",
		},
		{
			name: "style_without_reset_is_dropped",
			text: "前\x1b[31m后",
			want: "前后",
		},
		{
			name: "unterminated_csi_at_end",
			text: "前后\x1b[3",
			want: "前后",
		},
		{
			name: "crlf_and_lone_cr_become_newlines",
			text: "a\r\nb\rc",
			want: "a\nb\nc",
		},
		// Markdown 制表符原样保留。
		{
			name: "tabs_are_kept",
			text: "\tcode\tx",
			want: "\tcode\tx",
		},
		{
			name: "other_controls_are_dropped",
			text: "a\x00\x07\x7f\u009bb",
			want: "ab",
		},
		{
			name: "plain_text_unchanged",
			text: "编译失败：第 3 行\n",
			want: "编译失败：第 3 行\n",
		},
		{
			name: "empty",
			text: "",
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stripTerminalControls(tt.text); got != tt.want {
				t.Fatalf("stripTerminalControls(%q) = %q, want %q", tt.text, got, tt.want)
			}
		})
	}
}

// TestSanitizeToolText 检查工具文本的换行、制表符和控制字符处理。
func TestSanitizeToolText(t *testing.T) {
	// 1. 比较回车、样式、转义、中文、制表符和空文字的输出。
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "crlf_and_lone_cr_become_newlines",
			text: "a\r\nb\rc\n",
			want: "a\nb\nc\n",
		},
		{
			name: "unterminated_style_is_dropped",
			text: "\x1b[41mboom\nnext",
			want: "boom\nnext",
		},
		{
			name: "unterminated_escape_at_end",
			text: "ok\x1b[41",
			want: "ok",
		},
		{
			name: "title_sequence_is_dropped",
			text: "a\x1b]0;title\x07b",
			want: "ab",
		},
		{
			name: "other_controls_are_dropped_and_tabs_expanded",
			text: "a\x00\x07\x7f\tb",
			want: "a    b",
		},
		{
			name: "chinese_is_kept",
			text: "编译失败：第 3 行\n",
			want: "编译失败：第 3 行\n",
		},
		{
			name: "empty",
			text: "",
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeToolText(tt.text); got != tt.want {
				t.Fatalf("sanitizeToolText(%q) = %q, want %q", tt.text, got, tt.want)
			}
		})
	}
}

// TestFormatKeyHints 检查无颜色和有颜色的按键提示。
func TestFormatKeyHints(t *testing.T) {
	// 1. 创建两种样式，列出按键、无按键说明和空提示列表。
	plain, colored := newTUIStyles(markdownStyleNoTTY, nil), newTUIStyles("dark", nil)
	tests := []struct {
		name   string
		hints  []keyHint
		styles tuiStyles
		want   string
	}{
		// 无颜色模式直接显示按键和操作文字。
		{
			name:   "plain_matches_handwritten_panel_hint",
			styles: plain,
			hints: []keyHint{{
				key:    "r",
				action: "刷新",
			}, {
				key:    "↑↓",
				action: "滚动",
			}, {
				key:    "esc",
				action: "关闭",
			}},
			want: "r 刷新 · ↑↓ 滚动 · esc 关闭",
		},
		{
			name:   "plain_hint_without_key",
			styles: plain,
			hints: []keyHint{{action: "打字过滤"}, {
				key:    "enter",
				action: "打开",
			}, {
				key:    "esc",
				action: "关闭",
			}},
			want: "打字过滤 · enter 打开 · esc 关闭",
		},
		{
			name:   "plain_key_with_space",
			styles: plain,
			hints: []keyHint{{
				key:    "esc esc",
				action: "中断",
			}},
			want: "esc esc 中断",
		},
		{
			name:   "no_hints",
			styles: colored,
			want:   "",
		},
		// 按键正常色加粗（不带前景色），功能与分隔暗色。
		{
			name:   "colored_key_bold_action_and_separator_faint",
			styles: colored,
			hints: []keyHint{{
				key:    "r",
				action: "刷新",
			}, {
				key:    "esc",
				action: "关闭",
			}},
			want: "\x1b[1mr\x1b[m" + colored.faint.Render(" 刷新") + colored.faint.Render(" · ") + "\x1b[1mesc\x1b[m" + colored.faint.Render(" 关闭"),
		},
		{
			name:   "colored_hint_without_key_is_faint",
			styles: colored,
			hints:  []keyHint{{action: "打字过滤"}},
			want:   colored.faint.Render("打字过滤"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatKeyHints(tt.hints, tt.styles); got != tt.want {
				t.Fatalf("hints = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestGetStateStyle 检查各类状态在有色和无色模式下的样式。
func TestGetStateStyle(t *testing.T) {
	// 1. 按运行、思考、完成、失败、中断、连接和空闲状态分组。
	plain, colored := newTUIStyles(markdownStyleNoTTY, nil), newTUIStyles("dark", nil)
	tests := []struct {
		name   string
		states []displayState
		want   lipgloss.Style
	}{
		{
			name:   "running_and_generating_accent",
			states: []displayState{stateRunning, stateGenerating},
			want:   colored.accent,
		},
		{
			name:   "thinking_agent",
			states: []displayState{stateThinking},
			want:   colored.agent,
		},
		{
			name:   "completed_and_connected_success",
			states: []displayState{stateCompleted, stateConnected},
			want:   colored.success,
		},
		{
			name:   "failed_failure",
			states: []displayState{stateFailed},
			want:   colored.failure,
		},
		{
			name:   "interrupted_not_executed_reconnecting_warning",
			states: []displayState{stateInterrupted, stateNotExecuted, stateReconnecting},
			want:   colored.warning,
		},
		{
			name:   "idle_stopped_connecting_faint",
			states: []displayState{stateIdle, stateStopped, stateConnecting},
			want:   colored.faint,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, state := range tt.states {
				if got, want := getStateStyle(state, colored).Render("x"), tt.want.Render("x"); got != want {
					t.Fatalf("state %d = %q, want %q", state, got, want)
				}
				// 没有颜色时任何状态都不加修饰。
				if got := getStateStyle(state, plain).Render("x"); got != "x" {
					t.Fatalf("plain state %d = %q", state, got)
				}
			}
		})
	}
}

// TestGetContextUsageStyle 检查上下文占用比例对应的样式。
func TestGetContextUsageStyle(t *testing.T) {
	// 1. 构造各阈值附近、四舍五入及来源或窗口未知的占用记录。
	colored := newTUIStyles("dark", nil)
	actual := func(tokens, window int) runtime.ContextUsage {
		return runtime.ContextUsage{
			Tokens: tokens,
			Window: window,
			Source: runtime.ContextUsageSourceActual,
		}
	}
	tests := []struct {
		name  string
		usage runtime.ContextUsage
		want  lipgloss.Style
	}{
		{
			name:  "below_70_faint",
			usage: actual(69, 100),
			want:  colored.faint,
		},
		{
			name:  "at_70_warning",
			usage: actual(70, 100),
			want:  colored.warning,
		},
		{
			name:  "at_89_warning",
			usage: actual(89, 100),
			want:  colored.warning,
		},
		{
			name:  "at_90_failure",
			usage: actual(90, 100),
			want:  colored.failure,
		},
		{
			name:  "over_window_failure",
			usage: actual(150, 100),
			want:  colored.failure,
		},
		// 按显示的四舍五入百分比判断：69.5% 显示为 70%。
		{
			name:  "rounded_like_display",
			usage: actual(695, 1000),
			want:  colored.warning,
		},
		{
			name: "unknown_source_faint",
			usage: runtime.ContextUsage{
				Tokens: 95,
				Window: 100,
				Source: runtime.ContextUsageSourceUnknown,
			},
			want: colored.faint,
		},
		{
			name: "unknown_window_faint",
			usage: runtime.ContextUsage{
				Tokens: 95,
				Source: runtime.ContextUsageSourceActual,
			},
			want: colored.faint,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, want := getContextUsageStyle(tt.usage, colored).Render("x"), tt.want.Render("x"); got != want {
				t.Fatalf("style = %q, want %q", got, want)
			}
		})
	}
}

// TestFillBackground 检查行宽补齐、截断和底色复位后的恢复。
func TestFillBackground(t *testing.T) {
	// 1. 准备底色样式，列出短行、带内部复位的行、超宽行和无底色行。
	bg := lipgloss.NewStyle().Background(lipgloss.Color("#303030"))
	tests := []struct {
		name      string
		line      string
		width     int
		style     lipgloss.Style
		wantPlain string // 去掉样式后的内容
		wantOpen  bool   // 是否整行带底色并在行内复位之后重新打开
	}{
		{
			name:      "pads_to_width",
			line:      "ab",
			width:     6,
			style:     bg,
			wantPlain: "ab    ",
			wantOpen:  true,
		},
		{
			name:      "reopens_after_inner_reset",
			line:      "x" + lipgloss.NewStyle().Faint(true).Render("y") + "z",
			width:     4,
			style:     bg,
			wantPlain: "xyz ",
			wantOpen:  true,
		},
		{
			name:      "wider_line_is_truncated",
			line:      "abcdef",
			width:     4,
			style:     bg,
			wantPlain: "abcd",
			wantOpen:  true,
		},
		{
			name:      "plain_style_only_pads",
			line:      "ab",
			width:     4,
			style:     lipgloss.NewStyle(),
			wantPlain: "ab  ",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fillBackground(tt.line, tt.width, tt.style)
			if ansi.Strip(got) != tt.wantPlain || ansi.StringWidth(got) != tt.width {
				t.Fatalf("got %q", got)
			}
			if !tt.wantOpen {
				if got != tt.wantPlain {
					t.Fatalf("plain style added escapes: %q", got)
				}
				return
			}
			requireBackgroundFilled(t, got, tt.style)
		})
	}
}

// requireBackgroundFilled 检查整行底色从行首持续到行尾。
func requireBackgroundFilled(t *testing.T, line string, style lipgloss.Style) {
	t.Helper()
	// 1. 检查行首打开底色，行内每次样式复位后重新打开底色。
	open := strings.TrimSuffix(style.Render(" "), " "+ansi.ResetStyle)
	if !strings.HasPrefix(line, open) {
		t.Fatalf("line does not start with the background: %q", line)
	}
	for index := strings.Index(line, ansi.ResetStyle); index >= 0 && index+len(ansi.ResetStyle) < len(line); {
		rest := line[index+len(ansi.ResetStyle):]
		if !strings.HasPrefix(rest, open) {
			t.Fatalf("background not reopened after a reset: %q", line)
		}
		next := strings.Index(rest, ansi.ResetStyle)
		if next < 0 {
			break
		}
		index += len(ansi.ResetStyle) + next
	}
}

// TestIsDarkColor 检查背景颜色的深浅判断。
func TestIsDarkColor(t *testing.T) {
	// 1. 比较黑白、浅色和一半亮度附近的颜色。
	tests := []struct {
		color string
		want  bool
	}{{
		"#1e1e1e",
		true,
	}, {
		"#000000",
		true,
	}, {
		"#ffffff",
		false,
	}, {
		"#fdf6e3",
		false,
	}, {
		"#7f7f7f",
		true,
	}, {
		"#808080",
		false,
	}}
	for _, tt := range tests {
		if got := isDarkColor(lipgloss.Color(tt.color)); got != tt.want {
			t.Errorf("isDarkColor(%s) = %v, want %v", tt.color, got, tt.want)
		}
	}
}

// TestSplitPrintChunks 检查定稿内容按终端占用行数分批。
func TestSplitPrintChunks(t *testing.T) {
	// 1. 列出整批、超限、宽字符、样式、单行超高和空输入场景。
	styled := lipgloss.NewStyle().Faint(true).Render("abcd")
	tests := []struct {
		name     string
		lines    []string
		width    int
		rowLimit int
		want     [][]string
	}{
		{
			name:     "fits_in_one_chunk",
			lines:    []string{"a", "b", "c"},
			width:    10,
			rowLimit: 3,
			want:     [][]string{{"a", "b", "c"}},
		},
		{
			name:     "splits_at_the_limit",
			lines:    []string{"1", "2", "3", "4", "5"},
			width:    10,
			rowLimit: 2,
			want:     [][]string{{"1", "2"}, {"3", "4"}, {"5"}},
		},
		// 超宽行占 1 + 显示宽度/width 行：四列终端中十列文字占三行，十二列中文占四行。
		{
			name:     "wide_lines_count_multiple_rows",
			lines:    []string{"abcdefghij", "x", "中文中文中文", "y"},
			width:    4,
			rowLimit: 4,
			want:     [][]string{{"abcdefghij", "x"}, {"中文中文中文"}, {"y"}},
		},
		{
			name:     "exact_width_is_one_row",
			lines:    []string{"abcd", "efgh"},
			width:    4,
			rowLimit: 2,
			want:     [][]string{{"abcd", "efgh"}},
		},
		{
			name:     "styles_do_not_count",
			lines:    []string{styled, styled},
			width:    4,
			rowLimit: 2,
			want:     [][]string{{styled, styled}},
		},
		{
			name:     "line_taller_than_limit_is_alone",
			lines:    []string{"a", "abcdefghijkl", "b"},
			width:    4,
			rowLimit: 2,
			want:     [][]string{{"a"}, {"abcdefghijkl"}, {"b"}},
		},
		{
			name:     "empty_input",
			lines:    nil,
			width:    10,
			rowLimit: 5,
			want:     nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := splitPrintChunks(tt.lines, tt.width, tt.rowLimit); !slices.EqualFunc(got, tt.want, slices.Equal[[]string]) {
				t.Fatalf("chunks = %q, want %q", got, tt.want)
			}
		})
	}
}
