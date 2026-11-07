package tools_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/executor/tools"
	"github.com/VaalaCat/openvaala/pkg/workspace"
)

// searchRoute 是 glob、grep 的一条搜索路径：同一组用例在四条路径上都要得到同样的结果。
type searchRoute struct {
	name string
	// requiredCommands 是这条路径依赖的命令，测试机上找不到就跳过。
	requiredCommands []string
	// isGitRepository 测试数据是不是 git 仓库？是的话数据里多出 .gitignore 和被它忽略的 node_modules/d.go、gen.go。
	isGitRepository bool
	// isRipgrepHidden 是否把 rg 从命令的 PATH 里去掉？去掉后探测找不到 rg，走内联脚本。
	isRipgrepHidden bool
	// isVirtual 是否用 virtual 工作区？是的话只遍历。
	isVirtual bool
}

// searchCase 是一条搜索用例和它的期望。
type searchCase struct {
	name string
	tool string
	args string

	wantPaths        []string
	wantFirst        *searchMatch
	wantTextContains string
	wantNoPath       string
	wantNoPathPrefix string
	wantEmpty        bool
	wantCount        int
	wantTruncated    bool
	wantErr          string
	wantNoFile       string
	wantTextLen      int
	wantTextSuffix   string
}

// searchMatch 是结果里的一条匹配行。
type searchMatch struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// searchOutput 是 glob、grep 结果正文的并集。
type searchOutput struct {
	Paths     []string      `json:"paths"`
	Matches   []searchMatch `json:"matches"`
	Truncated bool          `json:"truncated"`
}

// TestSearchTools 验证 rg、git、find 三种命令和 virtual 遍历四条路径对同一组输入给出同样的结果。
func TestSearchTools(t *testing.T) {
	// 1. 准备四条搜索路径和同一组用例。
	routes := []*searchRoute{
		{name: "ripgrep", requiredCommands: []string{"rg", "git"}, isGitRepository: true},
		{name: "git_script", requiredCommands: []string{"find", "grep", "git", "cut"}, isGitRepository: true, isRipgrepHidden: true},
		{name: "find_script", requiredCommands: []string{"find", "grep", "git", "cut"}, isRipgrepHidden: true},
		{name: "walk", isVirtual: true},
	}
	tests := []*searchCase{
		{name: "glob_go_files", tool: "glob", args: `{"pattern":"**/*.go"}`, wantPaths: []string{"a b.go", "a.go", "sub/b.go", "sub/inner/e.go"}},
		{name: "glob_basename_any_depth", tool: "glob", args: `{"pattern":"*.go"}`, wantPaths: []string{"a b.go", "a.go", "sub/b.go", "sub/inner/e.go"}},
		{name: "glob_in_path", tool: "glob", args: `{"pattern":"*.go","path":"sub"}`, wantPaths: []string{"sub/b.go", "sub/inner/e.go"}},
		// 带 / 的通配符相对 path 匹配，不是相对工作区根目录。
		{name: "glob_relative_to_path", tool: "glob", args: `{"pattern":"inner/*.go","path":"sub"}`, wantPaths: []string{"sub/inner/e.go"}},
		// 隐藏文件、.gitignore 忽略的文件和 .git 目录，不因为通配符写得宽就被列出来。
		{name: "glob_hidden_root_file_skipped", tool: "glob", args: `{"pattern":".dot.go"}`, wantEmpty: true},
		{name: "glob_all_files", tool: "glob", args: `{"pattern":"**/*"}`, wantPaths: []string{"a b.go", "a.go", "bin.dat", "long.js", "quote.txt", "sub/b.go", "sub/inner/e.go", "wide.js"}, wantNoPathPrefix: ".git/"},
		{name: "glob_hidden_start_path", tool: "glob", args: `{"pattern":"**/*.go","path":".hidden"}`, wantPaths: []string{".hidden/c.go"}},
		{name: "glob_outside_root_absolute", tool: "glob", args: `{"pattern":"*.go","path":"{{outside}}"}`, wantPaths: []string{"{{outside}}/o.go"}},
		{name: "glob_not_directory", tool: "glob", args: `{"pattern":"*","path":"a.go"}`, wantErr: "is not a directory"},
		{name: "glob_invalid_pattern", tool: "glob", args: `{"pattern":"["}`, wantErr: "invalid glob pattern"},
		{name: "limit_truncates", tool: "glob", args: `{"pattern":"**/*.go","limit":1}`, wantCount: 1, wantTruncated: true},
		{name: "limit_zero_rejected", tool: "glob", args: `{"pattern":"**/*.go","limit":0}`, wantErr: "limit"},
		{name: "grep_limit_truncates", tool: "grep", args: `{"pattern":"func","limit":1}`, wantCount: 1, wantTruncated: true},
		{name: "grep_word_class", tool: "grep", args: `{"pattern":"func \\w+"}`, wantFirst: &searchMatch{Path: "a b.go", Line: 1}},
		{name: "grep_digit_class", tool: "grep", args: `{"pattern":"\\d+","glob":"a.go"}`, wantTextContains: "x := 42"},
		{name: "grep_escaped_paren_allowed", tool: "grep", args: `{"pattern":"Foo\\(?"}`, wantTextContains: "func Foo()"},
		{name: "grep_glob_any_depth", tool: "grep", args: `{"pattern":"package","glob":"*.go"}`, wantFirst: &searchMatch{Path: "sub/b.go", Line: 1}},
		{name: "grep_glob_relative_to_path", tool: "grep", args: `{"pattern":"InnerFunc","path":"sub","glob":"inner/*.go"}`, wantFirst: &searchMatch{Path: "sub/inner/e.go", Line: 3}},
		{name: "grep_glob_hidden_root_file_skipped", tool: "grep", args: `{"pattern":"Dot","glob":"*.go"}`, wantEmpty: true},
		{name: "grep_glob_gitignored_file_skipped", tool: "grep", args: `{"pattern":"Generated","glob":"*.go"}`, wantEmpty: true},
		{name: "grep_glob_all_files_skips_git_directory", tool: "grep", args: `{"pattern":"ref","glob":"**/*"}`, wantEmpty: true},
		{name: "grep_file_path", tool: "grep", args: `{"pattern":"func","path":"a.go"}`, wantFirst: &searchMatch{Path: "a.go", Line: 1}},
		{name: "grep_nested_file_path", tool: "grep", args: `{"pattern":"InnerFunc","path":"sub/inner/e.go"}`, wantFirst: &searchMatch{Path: "sub/inner/e.go", Line: 3}},
		// git 仓库里已 git add、又从磁盘上删掉的文件不能出现；glob_go_files 的完整路径列表也在核对这一点。
		{name: "deleted_file_skipped", tool: "grep", args: `{"pattern":"Gone"}`, wantEmpty: true},
		{name: "grep_case_sensitive", tool: "grep", args: `{"pattern":"foo"}`, wantEmpty: true},
		{name: "ignore_case", tool: "grep", args: `{"pattern":"foo","ignore_case":true}`, wantTextContains: "Foo"},
		{name: "grep_no_match", tool: "grep", args: `{"pattern":"no_such_text"}`, wantEmpty: true},
		{name: "hidden_skipped", tool: "grep", args: `{"pattern":"Hidden"}`, wantEmpty: true},
		{name: "gitignored_skipped", tool: "grep", args: `{"pattern":"Ignored"}`, wantEmpty: true},
		{name: "binary_skipped", tool: "grep", args: `{"pattern":"."}`, wantNoPath: "bin.dat"},
		{name: "inline_flags_rejected", tool: "grep", args: `{"pattern":"(?i)foo"}`, wantErr: "uses a (?...) group"},
		{name: "non_capturing_group_rejected", tool: "grep", args: `{"pattern":"a(?:b)"}`, wantErr: "use ignore_case"},
		{name: "invalid_regex", tool: "grep", args: `{"pattern":"("}`, wantErr: "invalid grep pattern"},
		{name: "grep_invalid_glob", tool: "grep", args: `{"pattern":"x","glob":"["}`, wantErr: "invalid glob pattern"},
		{name: "missing_path", tool: "grep", args: `{"pattern":"x","path":"nope"}`, wantErr: "does not exist"},
		// Review Focus：参数里的 shell 元字符只能当作字面值。
		{name: "shell_metacharacters", tool: "grep", args: `{"pattern":"it's \\$\\(touch pwned\\)"}`, wantTextContains: "$(touch pwned)", wantNoFile: "pwned"},
		{name: "long_line", tool: "grep", args: `{"pattern":"needle","glob":"long.js"}`, wantTextLen: 500, wantTextSuffix: "…"},
		// 字符数不到 500 的多字节行原样返回，rg 对它加的截断提示不能留在文本里。
		{name: "wide_line_under_limit", tool: "grep", args: `{"pattern":"needle","glob":"wide.js"}`, wantTextLen: 300, wantTextSuffix: "needle"},
	}
	// 2. 每条路径各起一个工作区，跑完整组用例。
	for _, route := range routes {
		t.Run(route.name, func(t *testing.T) {
			for _, command := range route.requiredCommands {
				if _, err := exec.LookPath(command); err != nil {
					t.Skipf("%s not installed", command)
				}
			}
			searchTools, rootDirectory, outsideDirectory := newSearchRouteTools(t, route)
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					arguments := strings.ReplaceAll(tt.args, "{{outside}}", outsideDirectory)
					result := runTool(t, searchTools[tt.tool], &defs.ToolCall{CallID: "search", Name: tt.tool, Arguments: arguments})
					if tt.wantErr != "" {
						if result.Error == nil || !strings.Contains(result.Error.Message, tt.wantErr) {
							t.Fatalf("error = %+v, want containing %q", result.Error, tt.wantErr)
						}
						return
					}
					if result.Error != nil {
						t.Fatal(result.Error)
					}
					if bytes.Contains(result.Content, []byte("null")) {
						t.Fatalf("content = %s, want no null arrays", result.Content)
					}
					var output searchOutput
					if err := json.Unmarshal(result.Content, &output); err != nil {
						t.Fatal(err)
					}
					checkSearchOutput(t, tt, &output, rootDirectory, outsideDirectory)
				})
			}
		})
	}
}

// checkSearchOutput 按用例的期望逐项核对搜索结果。
func checkSearchOutput(t *testing.T, tt *searchCase, output *searchOutput, rootDirectory, outsideDirectory string) {
	// 1. 路径、第一条匹配、文本、数量和截断标记。
	t.Helper()
	if output.Truncated != tt.wantTruncated {
		t.Fatalf("truncated = %v, want %v", output.Truncated, tt.wantTruncated)
	}
	if tt.wantPaths != nil {
		want := make([]string, 0, len(tt.wantPaths))
		for _, path := range tt.wantPaths {
			want = append(want, strings.ReplaceAll(path, "{{outside}}", outsideDirectory))
		}
		if !slices.Equal(output.Paths, want) {
			t.Fatalf("paths = %q, want %q", output.Paths, want)
		}
	}
	if tt.wantFirst != nil && (len(output.Matches) == 0 || output.Matches[0].Path != tt.wantFirst.Path || output.Matches[0].Line != tt.wantFirst.Line) {
		t.Fatalf("matches = %+v, want first %+v", output.Matches, tt.wantFirst)
	}
	if tt.wantTextContains != "" && !slices.ContainsFunc(output.Matches, func(match searchMatch) bool { return strings.Contains(match.Text, tt.wantTextContains) }) {
		t.Fatalf("matches = %+v, want text containing %q", output.Matches, tt.wantTextContains)
	}
	if tt.wantEmpty && (len(output.Paths) != 0 || len(output.Matches) != 0) {
		t.Fatalf("output = %+v, want empty", output)
	}
	if tt.wantCount != 0 && len(output.Paths)+len(output.Matches) != tt.wantCount {
		t.Fatalf("output = %+v, want %d results", output, tt.wantCount)
	}
	// 2. 不该出现的路径、不该创建的文件、截断后的文本。
	if tt.wantNoPath != "" {
		isPresent := slices.Contains(output.Paths, tt.wantNoPath) ||
			slices.ContainsFunc(output.Matches, func(match searchMatch) bool { return match.Path == tt.wantNoPath })
		if isPresent {
			t.Fatalf("output = %+v, want no %q", output, tt.wantNoPath)
		}
	}
	if tt.wantNoPathPrefix != "" {
		isPresent := slices.ContainsFunc(output.Paths, func(path string) bool { return strings.HasPrefix(path, tt.wantNoPathPrefix) }) ||
			slices.ContainsFunc(output.Matches, func(match searchMatch) bool { return strings.HasPrefix(match.Path, tt.wantNoPathPrefix) })
		if isPresent {
			t.Fatalf("output = %+v, want nothing under %q", output, tt.wantNoPathPrefix)
		}
	}
	if tt.wantNoFile != "" && rootDirectory != "" {
		if _, err := os.Stat(filepath.Join(rootDirectory, tt.wantNoFile)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat %s error = %v, want not exist", tt.wantNoFile, err)
		}
	}
	if tt.wantTextSuffix != "" {
		if len(output.Matches) != 1 {
			t.Fatalf("matches = %d, want 1", len(output.Matches))
		}
		text := output.Matches[0].Text
		if !strings.HasSuffix(text, tt.wantTextSuffix) || utf8.RuneCountInString(strings.TrimSuffix(text, tt.wantTextSuffix)) != tt.wantTextLen {
			t.Fatalf("text has %d characters, suffix %q, want %d characters then %q", utf8.RuneCountInString(text), text[max(0, len(text)-4):], tt.wantTextLen, tt.wantTextSuffix)
		}
	}
}

// getSearchFixtureFiles 返回测试数据：键是相对路径，值是文件内容；git 仓库里多一个 .gitignore、被它忽略的文件和一个会被删掉的文件。
func getSearchFixtureFiles(isGitRepository bool) map[string]string {
	// 1. 所有路径共有的文件：普通文件、带空格的文件名、隐藏文件和隐藏目录、二进制文件、超长行、字节数超过 500 但字符数不到 500 的多字节行、带 shell 元字符的文本。
	files := map[string]string{
		"a.go":           "func Foo() {\n\tx := 42\n}\n",
		"a b.go":         "func Spaced()\n",
		"sub/b.go":       "package b\n",
		"sub/inner/e.go": "package inner\n\nfunc InnerFunc() {}\n",
		".dot.go":        "func Dot()\n",
		".hidden/c.go":   "func Hidden()\n",
		"bin.dat":        "\x00\x01func Binary()\n",
		"long.js":        strings.Repeat("x", 100<<10) + "needle" + strings.Repeat("y", 100<<10) + "\n",
		"quote.txt":      "it's $(touch pwned)\n",
		"wide.js":        strings.Repeat("你", 300) + "needle\n",
	}
	// 2. git 仓库里被 .gitignore 忽略的目录和单个文件，以及加入索引后又被删掉的 gone.go。
	if isGitRepository {
		files[".gitignore"] = "node_modules/\ngen.go\n"
		files["node_modules/d.go"] = "func Ignored()\n"
		files["gen.go"] = "func Generated()\n"
		files["gone.go"] = "func Gone()\n"
	}
	return files
}

// newSearchFixture 在临时目录里写好测试数据；isGitRepository 为 true 时 git init、把文件加入索引，再从磁盘上删掉 gone.go。
func newSearchFixture(t *testing.T, isGitRepository bool) string {
	// 1. 写入文件。
	t.Helper()
	directory := t.TempDir()
	for name, content := range getSearchFixtureFiles(isGitRepository) {
		filePath := filepath.Join(directory, name)
		if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// 2. 初始化 git 仓库并加入索引，之后删掉 gone.go：它留在索引里，磁盘上已经没有了。
	if isGitRepository {
		for _, arguments := range [][]string{{"init", "-q"}, {"add", "-A"}} {
			command := exec.Command("git", arguments...)
			command.Dir = directory
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v: %s", arguments, err, output)
			}
		}
		if err := os.Remove(filepath.Join(directory, "gone.go")); err != nil {
			t.Fatal(err)
		}
	}
	return directory
}

// newSearchRouteTools 创建一条搜索路径的工作区和 glob、grep；返回工作区根目录和根目录外一个放着 o.go 的目录。
func newSearchRouteTools(t *testing.T, route *searchRoute) (map[string]executor.Tool, string, string) {
	// 1. virtual：用同样的文件建内存工作区，根目录外放 /other/o.go，工具只遍历。
	var work workspace.Workspace
	var rootDirectory, outsideDirectory string
	var err error
	var opt *tools.SearchToolsOpt
	if route.isVirtual {
		files := map[string]*workspace.VirtualFile{"/other/o.go": {Content: []byte("package o\n")}}
		for name, content := range getSearchFixtureFiles(false) {
			files["/workspace/"+name] = &workspace.VirtualFile{Content: []byte(content)}
		}
		if work, err = workspace.NewVirtualWorkspace(workspace.NewVirtualWorkspaceOpt("/workspace", files)); err != nil {
			t.Fatal(err)
		}
		outsideDirectory = "/other"
		opt = tools.NewSearchToolsOpt(work).WithWalkOnly()
	} else {
		// 2. 本机：写好测试数据；需要时把 PATH 换成只有 find、grep、git、cut 的目录，让探测找不到 rg。
		rootDirectory = newSearchFixture(t, route.isGitRepository)
		outsideDirectory = t.TempDir()
		if err := os.WriteFile(filepath.Join(outsideDirectory, "o.go"), []byte("package o\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		workspaceOpt := workspace.NewLocalWorkspaceOpt(rootDirectory)
		if route.isRipgrepHidden {
			workspaceOpt.WithEnvironment([]string{"PATH=" + newToolBinDirectory(t, "find", "grep", "git", "cut"), "HOME=" + t.TempDir()})
		}
		if work, err = workspace.NewLocalWorkspace(workspaceOpt); err != nil {
			t.Fatal(err)
		}
		opt = tools.NewSearchToolsOpt(work)
	}
	// 3. 创建工具并按名字索引。
	created, err := tools.NewSearchTools(opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 2 || created[0].GetDefinition().Name != "glob" || created[1].GetDefinition().Name != "grep" {
		t.Fatalf("tools = %+v, want glob then grep", created)
	}
	return map[string]executor.Tool{"glob": created[0], "grep": created[1]}, rootDirectory, outsideDirectory
}

// newToolBinDirectory 创建一个目录，里面只有指向指定命令的软链接。
func newToolBinDirectory(t *testing.T, commands ...string) string {
	// 1. 逐个找到命令的真实位置并建立软链接。
	t.Helper()
	directory := t.TempDir()
	for _, command := range commands {
		target, err := exec.LookPath(command)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(directory, command)); err != nil {
			t.Fatal(err)
		}
	}
	return directory
}

// failingStartWorkspace 让前 failedStarts 次 StartProcess 失败，模拟连不上 Executor。
type failingStartWorkspace struct {
	workspace.Workspace

	failedStarts int
	startCount   int
}

// StartProcess 前几次返回连接错误，之后交给真实工作区。
func (w *failingStartWorkspace) StartProcess(ctx context.Context, request *workspace.ProcessRequest) (workspace.Process, error) {
	// 1. 计数，前 failedStarts 次返回错误。
	w.startCount++
	if w.startCount <= w.failedStarts {
		return nil, errors.New("connection refused")
	}
	return w.Workspace.StartProcess(ctx, request)
}

// TestSearchToolsProbeRetriesAfterFailure 验证连不上 Executor 时只让这一次调用报错，探测结果不缓存，下次调用重新探测。
func TestSearchToolsProbeRetriesAfterFailure(t *testing.T) {
	// 1. 准备连续失败的启动次数。
	tests := []*struct {
		name         string
		failedStarts int
	}{
		{name: "one_failed_start", failedStarts: 1},
		{name: "two_failed_starts", failedStarts: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 2. 前 failedStarts 次调用返回 search_failed，之后恢复正常。
			rootDirectory := newSearchFixture(t, false)
			local, err := workspace.NewLocalWorkspace(workspace.NewLocalWorkspaceOpt(rootDirectory))
			if err != nil {
				t.Fatal(err)
			}
			created, err := tools.NewSearchTools(tools.NewSearchToolsOpt(&failingStartWorkspace{Workspace: local, failedStarts: tt.failedStarts}))
			if err != nil {
				t.Fatal(err)
			}
			for call := 1; call <= tt.failedStarts+1; call++ {
				result := runTool(t, created[0], &defs.ToolCall{CallID: "glob", Name: "glob", Arguments: `{"pattern":"*.go"}`})
				if call <= tt.failedStarts {
					if result.Error == nil || result.Error.Code != "search_failed" || !strings.Contains(result.Error.Message, "connection refused") {
						t.Fatalf("call %d error = %+v, want search_failed", call, result.Error)
					}
					continue
				}
				if result.Error != nil || !strings.Contains(string(result.Content), `"a.go"`) {
					t.Fatalf("call %d result = %+v", call, result)
				}
			}
		})
	}
}

// TestSearchToolsIgnoreRepositoryGitConfig 验证仓库自己的 git 配置不影响 git 路径：颜色、grep 路径写法照常解析，fsmonitor 命令不会被执行。
func TestSearchToolsIgnoreRepositoryGitConfig(t *testing.T) {
	for _, command := range []string{"find", "grep", "git"} {
		if _, err := exec.LookPath(command); err != nil {
			t.Skipf("%s not installed", command)
		}
	}
	// 1. 准备会改变 git 输出或执行命令的仓库配置，以及每种配置下都要得到正确结果的搜索。
	configTests := []*struct {
		name string
		// configs 是写进 .git/config 的 key、value；{{fsmonitor}} 换成会创建标记文件的脚本路径。
		configs [][2]string
	}{
		{name: "color_ui_always", configs: [][2]string{{"color.ui", "always"}}},
		{name: "color_grep_always", configs: [][2]string{{"color.grep", "always"}}},
		{name: "grep_full_name", configs: [][2]string{{"grep.fullName", "true"}}},
		{name: "fsmonitor_command", configs: [][2]string{{"core.fsmonitor", "{{fsmonitor}}"}, {"core.untrackedCache", "true"}}},
	}
	searchTests := []*searchCase{
		{name: "glob_in_subdirectory", tool: "glob", args: `{"pattern":"*.go","path":"sub"}`, wantPaths: []string{"sub/b.go", "sub/inner/e.go"}},
		{name: "grep_in_subdirectory", tool: "grep", args: `{"pattern":"InnerFunc","path":"sub"}`, wantFirst: &searchMatch{Path: "sub/inner/e.go", Line: 3}},
		{name: "grep_in_root", tool: "grep", args: `{"pattern":"InnerFunc"}`, wantFirst: &searchMatch{Path: "sub/inner/e.go", Line: 3}},
		{name: "deleted_file_skipped", tool: "grep", args: `{"pattern":"Gone"}`, wantEmpty: true},
	}
	// 2. 每种配置各建一个 git 仓库，写入配置后跑完整组搜索，最后确认 fsmonitor 脚本没有被执行。
	for _, configTest := range configTests {
		t.Run(configTest.name, func(t *testing.T) {
			rootDirectory := newSearchFixture(t, true)
			markerPath := filepath.Join(t.TempDir(), "fsmonitor-ran")
			fsmonitorPath := filepath.Join(t.TempDir(), "fsmonitor.sh")
			if err := os.WriteFile(fsmonitorPath, []byte("#!/bin/sh\n: > "+markerPath+"\nprintf 'token\\0'\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			for _, config := range configTest.configs {
				command := exec.Command("git", "config", config[0], strings.ReplaceAll(config[1], "{{fsmonitor}}", fsmonitorPath))
				command.Dir = rootDirectory
				if output, err := command.CombinedOutput(); err != nil {
					t.Fatalf("git config %v: %v: %s", config, err, output)
				}
			}
			work, err := workspace.NewLocalWorkspace(workspace.NewLocalWorkspaceOpt(rootDirectory).
				WithEnvironment([]string{"PATH=" + newToolBinDirectory(t, "find", "grep", "git", "cut"), "HOME=" + t.TempDir()}))
			if err != nil {
				t.Fatal(err)
			}
			created, err := tools.NewSearchTools(tools.NewSearchToolsOpt(work))
			if err != nil {
				t.Fatal(err)
			}
			searchTools := map[string]executor.Tool{"glob": created[0], "grep": created[1]}
			for _, tt := range searchTests {
				t.Run(tt.name, func(t *testing.T) {
					result := runTool(t, searchTools[tt.tool], &defs.ToolCall{CallID: "search", Name: tt.tool, Arguments: tt.args})
					if result.Error != nil {
						t.Fatal(result.Error)
					}
					var output searchOutput
					if err := json.Unmarshal(result.Content, &output); err != nil {
						t.Fatal(err)
					}
					checkSearchOutput(t, tt, &output, rootDirectory, "")
				})
			}
			if _, err := os.Stat(markerPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stat fsmonitor marker error = %v, want not exist", err)
			}
		})
	}
}
