package tools

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestTranslatePortablePattern 验证只替换方括号外的 \d \D \w \W \s \S，其余写法保持不变。
func TestTranslatePortablePattern(t *testing.T) {
	// 1. 准备要替换的、方括号内的、转义反斜杠和不认识的转义。
	tests := []*struct {
		name    string
		pattern string
		want    string
	}{
		{name: "digit", pattern: `\d+`, want: `[0-9]+`},
		{name: "non_digit", pattern: `\D`, want: `[^0-9]`},
		{name: "word_and_non_word", pattern: `\w\W`, want: `[[:alnum:]_][^[:alnum:]_]`},
		{name: "space_and_non_space", pattern: `\s\S`, want: `[[:space:]][^[:space:]]`},
		{name: "inside_bracket_unchanged", pattern: `[\d]`, want: `[\d]`},
		{name: "negated_bracket_unchanged", pattern: `[^\d]\d`, want: `[^\d][0-9]`},
		{name: "bracket_with_posix_class", pattern: `[[:digit:]\d]\d`, want: `[[:digit:]\d][0-9]`},
		{name: "leading_close_bracket_is_literal", pattern: `[]\d]\d`, want: `[]\d][0-9]`},
		{name: "escaped_backslash_then_d", pattern: `\\d`, want: `\\d`},
		{name: "escaped_backslash_then_digit_class", pattern: `\\\d`, want: `\\[0-9]`},
		{name: "other_escapes_unchanged", pattern: `\.\(\bfoo`, want: `\.\(\bfoo`},
		{name: "trailing_backslash", pattern: `a\`, want: `a\`},
	}
	// 2. 逐项核对。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := translatePortablePattern(tt.pattern); got != tt.want {
				t.Fatalf("translatePortablePattern(%q) = %q, want %q", tt.pattern, got, tt.want)
			}
		})
	}
}

// TestIsExtensionGroupPattern 验证只有未转义、不在方括号里的 (? 才算扩展分组。
func TestIsExtensionGroupPattern(t *testing.T) {
	// 1. 准备内联标记、命名分组、转义的括号和方括号里的括号。
	tests := []*struct {
		name    string
		pattern string
		want    bool
	}{
		{name: "inline_flag", pattern: `(?i)foo`, want: true},
		{name: "non_capturing_group", pattern: `a(?:b)`, want: true},
		{name: "plain_group", pattern: `(a|b)?`, want: false},
		{name: "escaped_open_paren", pattern: `Foo\(?`, want: false},
		{name: "inside_bracket", pattern: `[(?]`, want: false},
		{name: "after_escaped_backslash", pattern: `\\(?i)`, want: true},
	}
	// 2. 逐项核对。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isExtensionGroupPattern(tt.pattern); got != tt.want {
				t.Fatalf("isExtensionGroupPattern(%q) = %v, want %v", tt.pattern, got, tt.want)
			}
		})
	}
}

// TestIsHiddenPath 验证任何一段以 . 开头的路径都是隐藏路径，. 本身除外。
func TestIsHiddenPath(t *testing.T) {
	// 1. 准备隐藏文件、隐藏目录和普通路径。
	tests := []*struct {
		name string
		path string
		want bool
	}{
		{name: "hidden_file", path: ".env", want: true},
		{name: "hidden_directory", path: "a/.git/config", want: true},
		{name: "plain", path: "a/b.go", want: false},
		{name: "dot_segment", path: "./a/b.go", want: false},
		{name: "dot_in_name", path: "a/b.test.go", want: false},
	}
	// 2. 逐项核对。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isHiddenPath(tt.path); got != tt.want {
				t.Fatalf("isHiddenPath(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

// TestIsSearchGlobMatch 验证不含 / 的通配符按文件名匹配任意深度，含 / 的按整个相对路径匹配。
func TestIsSearchGlobMatch(t *testing.T) {
	// 1. 准备文件名通配符、整路径通配符和 **。
	tests := []*struct {
		name    string
		pattern string
		path    string
		want    bool
	}{
		{name: "basename_at_root", pattern: "*.go", path: "a.go", want: true},
		{name: "basename_nested", pattern: "*.go", path: "sub/b.go", want: true},
		{name: "basename_mismatch", pattern: "*.go", path: "sub/b.js", want: false},
		{name: "double_star_at_root", pattern: "**/*.go", path: "a.go", want: true},
		{name: "double_star_nested", pattern: "**/*.go", path: "x/y/b.go", want: true},
		{name: "anchored_directory", pattern: "src/*.go", path: "src/a.go", want: true},
		{name: "anchored_directory_not_nested", pattern: "src/*.go", path: "x/src/a.go", want: false},
		{name: "braces", pattern: "{src,lib}/**/*.py", path: "lib/a/b.py", want: true},
	}
	// 2. 逐项核对。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isSearchGlobMatch(tt.pattern, tt.path); got != tt.want {
				t.Fatalf("isSearchGlobMatch(%q, %q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
			}
		})
	}
}

// TestQuoteShellArgument 验证参数用单引号包起来，里面的单引号被转义。
func TestQuoteShellArgument(t *testing.T) {
	// 1. 准备普通值、带单引号和带 shell 元字符的值。
	tests := []*struct {
		name  string
		value string
		want  string
	}{
		{name: "plain", value: "abc", want: `'abc'`},
		{name: "single_quote", value: `it's`, want: `'it'\''s'`},
		{name: "metacharacters", value: `$(x) ; "y" \`, want: `'$(x) ; "y" \'`},
		{name: "empty", value: "", want: `''`},
	}
	// 2. 逐项核对。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := quoteShellArgument(tt.value); got != tt.want {
				t.Fatalf("quoteShellArgument(%q) = %q, want %q", tt.value, got, tt.want)
			}
		})
	}
}

// TestTruncateMatchText 验证超过 maxMatchTextChars 个字符时按字符截断并加 …，不会截坏多字节字符。
func TestTruncateMatchText(t *testing.T) {
	// 1. 准备短行、刚好到上限、超过上限和多字节字符。
	tests := []*struct {
		name      string
		line      string
		wantChars int
		wantCut   bool
	}{
		{name: "short_line_without_newline", line: "abc\r\n", wantChars: 3},
		{name: "exactly_limit", line: strings.Repeat("x", maxMatchTextChars), wantChars: maxMatchTextChars},
		{name: "over_limit", line: strings.Repeat("x", maxMatchTextChars+1), wantChars: maxMatchTextChars, wantCut: true},
		{name: "multibyte_over_limit", line: strings.Repeat("你", maxMatchTextChars+5), wantChars: maxMatchTextChars, wantCut: true},
	}
	// 2. 逐项核对字符数、省略号和有效 UTF-8。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateMatchText([]byte(tt.line))
			text, isCut := strings.CutSuffix(got, "…")
			if isCut != tt.wantCut || utf8.RuneCountInString(text) != tt.wantChars || !utf8.ValidString(got) {
				t.Fatalf("truncateMatchText(%d bytes) = %d chars, cut = %v, want %d chars, cut = %v",
					len(tt.line), utf8.RuneCountInString(text), isCut, tt.wantChars, tt.wantCut)
			}
		})
	}
}
