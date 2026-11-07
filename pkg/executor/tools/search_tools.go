package tools

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/prompt"
	"github.com/VaalaCat/openvaala/pkg/workspace"
)

// workspaceSearcher 在一个工作区里找文件、搜内容；第一次使用时探测 rg 和 git，之后按探测结果选择搜索方式。
type workspaceSearcher struct {
	opt *SearchToolsOpt
	// probeLock 保护下面三个探测结果；探测只在第一次搜索时跑一次命令。
	probeLock sync.Mutex
	// isProbed 是否已经探测完？连不上 Executor、探测命令没跑完都不算探测完，下次调用重新探测。
	isProbed bool
	// ripgrepPath、gitPath 是探测到的命令路径；空串表示这台机器上没有。
	ripgrepPath string
	gitPath     string
}

// globToolImpl 是 glob 工具：按通配符列出工作区里的文件路径。
type globToolImpl struct {
	*executor.BaseTool[globArguments]

	searcher *workspaceSearcher
}

// grepToolImpl 是 grep 工具：用正则逐行搜索文件内容。
type grepToolImpl struct {
	*executor.BaseTool[grepArguments]

	searcher *workspaceSearcher
}

// SearchToolsOpt 是搜索工具的工作区和搜索方式。
type SearchToolsOpt struct {
	Workspace workspace.Workspace
	// IsWalkOnly 是否只用 Workspace 遍历？virtual 和 memory 为 true，不探测、不启动进程。
	IsWalkOnly bool
}

// NewSearchToolsOpt 创建通过探测选择搜索方式的配置。
func NewSearchToolsOpt(work workspace.Workspace) *SearchToolsOpt {
	// 1. 保存工作区，默认通过探测选择搜索方式。
	return &SearchToolsOpt{Workspace: work}
}

// WithWalkOnly 改为只用 Workspace 遍历。
func (opt *SearchToolsOpt) WithWalkOnly() *SearchToolsOpt {
	// 1. 标记不探测、不启动进程。
	opt.IsWalkOnly = true
	return opt
}

// NewSearchTools 创建共用同一个 workspaceSearcher 的 glob 和 grep。
func NewSearchTools(opt *SearchToolsOpt) ([]executor.Tool, error) {
	// 1. 校验工作区，复制配置，两个工具共用同一个搜索器。
	if opt == nil || opt.Workspace == nil {
		return nil, errors.New("search tools require a workspace")
	}
	searcher := &workspaceSearcher{opt: &SearchToolsOpt{
		Workspace:  opt.Workspace,
		IsWalkOnly: opt.IsWalkOnly,
	}}
	// 2. 生成两个工具的参数定义。
	globBase, err := executor.NewBaseTool[globArguments](&executor.BaseToolOpt{
		Name:        "glob",
		Description: prompt.GlobDescription,
	})
	if err != nil {
		return nil, err
	}
	grepBase, err := executor.NewBaseTool[grepArguments](&executor.BaseToolOpt{
		Name:        "grep",
		Description: prompt.GrepDescription,
	})
	if err != nil {
		return nil, err
	}
	return []executor.Tool{
		&globToolImpl{BaseTool: globBase, searcher: searcher},
		&grepToolImpl{BaseTool: grepBase, searcher: searcher},
	}, nil
}

// globArguments 是 glob 的参数。
type globArguments struct {
	Pattern string `json:"pattern" jsonschema:"description=路径通配符，相对 path，例如 **/*.go、src/**/test_*.py,minLength=1"`
	Path    string `json:"path,omitempty" jsonschema:"description=从哪个目录开始找：相对工作区根目录，也可以用绝对路径；省略时是工作区根目录"`
	Limit   int    `json:"limit,omitempty" jsonschema:"description=最多返回的路径数,minimum=1,maximum=1000,default=100"`
}

// grepArguments 是 grep 的参数。
type grepArguments struct {
	Pattern    string `json:"pattern" jsonschema:"description=逐行匹配的正则，只用 rg 和 POSIX 扩展正则共有的写法,minLength=1"`
	Path       string `json:"path,omitempty" jsonschema:"description=搜索的目录或文件：相对工作区根目录，也可以用绝对路径；省略时是工作区根目录"`
	Glob       string `json:"glob,omitempty" jsonschema:"description=只搜路径匹配这个通配符的文件，相对 path，例如 *.go"`
	IgnoreCase bool   `json:"ignore_case,omitempty" jsonschema:"description=是否忽略大小写"`
	Limit      int    `json:"limit,omitempty" jsonschema:"description=最多返回的匹配行数,minimum=1,maximum=1000,default=100"`
}

// globResult 是 glob 返回给模型的结果。
type globResult struct {
	// Paths 按路径排序；在工作区根目录内的写成相对路径，否则写绝对路径。
	Paths       []string `json:"paths"`
	IsTruncated bool     `json:"truncated"`
}

// grepResult 是 grep 返回给模型的结果。
type grepResult struct {
	// Matches 按路径和行号排序。
	Matches     []grepMatch `json:"matches"`
	IsTruncated bool        `json:"truncated"`
}

// grepMatch 是一条匹配行。
type grepMatch struct {
	Path string `json:"path"`
	Line int    `json:"line"` // 从 1 开始
	Text string `json:"text"` // 超过 500 个字符时截断，末尾加 …
}

const (
	maxMatchTextChars = 500
	// ripgrepOmittedMarker 是 rg 用 --max-columns-preview 截断超长行后，接在行尾的提示。
	ripgrepOmittedMarker = " [... omitted end of long line]"
	// binaryProbeBytes 是遍历时判断二进制文件读的开头长度，开头里有 NUL 字节就跳过整个文件。
	binaryProbeBytes = 8 << 10
	// maxScriptLineBytes 是 git grep、find + grep 在远端每行输出保留的字节数，路径、行号和 500 个字符的正文都放得下。
	maxScriptLineBytes = 4 << 10
	// maxSearchStderrBytes 是搜索命令出错时，错误信息里带上的 stderr 末尾长度。
	maxSearchStderrBytes = 2 << 10
	// probeCommand 在 Executor 上打印 rg 和 git 的路径，找不到的命令打印空值。
	probeCommand = `printf 'rg=%s\ngit=%s\n' "$(command -v rg)" "$(command -v git)"`
	// gitCommand 是所有 git 调用的开头：不执行仓库配置里的 fsmonitor 命令，输出不带颜色，grep 路径相对 -C 的目录而不是仓库根目录。
	gitCommand = "git -c core.fsmonitor=false -c color.ui=never -c grep.fullName=false"
)

// searchRequest 是一次已经校验过的搜索。
type searchRequest struct {
	// path 是搜索起点的绝对路径：glob 一定是目录，grep 可以是文件。
	path string
	// isFile 搜索起点是不是文件？是文件时 glob 不起作用。
	isFile bool
	// pattern 是 grep 的正则；glob 工具不用。
	pattern string
	// glob 是相对 path 的路径通配符：glob 工具里是 pattern 参数，grep 里是 glob 参数，可以为空。
	glob         string
	isIgnoreCase bool
	limit        int
}

// searchMethod 是一次搜索实际用的方式。
type searchMethod int

const (
	// searchMethodWalk 逐层遍历 Workspace。
	searchMethodWalk searchMethod = iota
	// searchMethodRipgrep 用 rg。
	searchMethodRipgrep
	// searchMethodGit 用 git ls-files、git grep；path 不在 git 仓库里时改用 find。
	searchMethodGit
	// searchMethodFind 用 find、find + grep。
	searchMethodFind
)

// invalidSearchPathError 表示 path 参数本身有问题：不存在，或 glob 的 path 不是目录。
type invalidSearchPathError struct {
	message string
}

// Error 返回给模型看的说明。
func (e *invalidSearchPathError) Error() string {
	// 1. 直接返回说明文字。
	return e.message
}

// searchCommandError 表示搜索命令以 0、1 以外的退出码结束。
type searchCommandError struct {
	exitCode int
	stderr   string
}

// Error 返回退出码和 stderr 的末尾。
func (e *searchCommandError) Error() string {
	// 1. 带上退出码和 stderr。
	return fmt.Sprintf("search command exited with code %d: %s", e.exitCode, e.stderr)
}

// tailBuffer 只保留写入内容的最后 maxSearchStderrBytes 个字节。
type tailBuffer struct {
	data []byte
}

// Write 追加内容，超出上限时丢弃最早的部分。
func (b *tailBuffer) Write(p []byte) (int, error) {
	// 1. 追加后只保留末尾。
	b.data = append(b.data, p...)
	if extra := len(b.data) - maxSearchStderrBytes; extra > 0 {
		b.data = b.data[extra:]
	}
	return len(p), nil
}

// Execute 按通配符列出文件路径。
func (t *globToolImpl) Execute(ctx context.Context, call *defs.Item, _ chan<- *executor.ExecutionProgressEvent) *executor.ToolExecutionResult {
	// 1. 解析参数并校验通配符。
	callID := call.ToolCall.CallID
	arguments, err := t.ParseArguments(call.ToolCall.Arguments)
	if err != nil {
		return newInvalidArgumentsResult(callID, err)
	}
	if !doublestar.ValidatePattern(arguments.Pattern) {
		return newInvalidArgumentsResult(callID, fmt.Errorf("invalid glob pattern %q", arguments.Pattern))
	}
	// 2. 确认搜索起点是目录，列出匹配的路径。
	searchPath, _, err := t.searcher.getSearchPath(ctx, arguments.Path, true)
	if err != nil {
		return newSearchFailedResult(ctx, callID, err)
	}
	result, err := t.searcher.searchFiles(ctx, &searchRequest{
		path:  searchPath,
		glob:  arguments.Pattern,
		limit: arguments.Limit,
	})
	if err != nil {
		return newSearchFailedResult(ctx, callID, err)
	}
	execution := encodeToolResult(callID, result, nil)
	execution.Result.Truncated = result.IsTruncated
	return execution
}

// Execute 用正则逐行搜索文件内容。
func (t *grepToolImpl) Execute(ctx context.Context, call *defs.Item, _ chan<- *executor.ExecutionProgressEvent) *executor.ToolExecutionResult {
	// 1. 解析参数；正则要能编译，且不带 (? 开头的分组；通配符要合法。
	callID := call.ToolCall.CallID
	arguments, err := t.ParseArguments(call.ToolCall.Arguments)
	if err != nil {
		return newInvalidArgumentsResult(callID, err)
	}
	if _, err := regexp.Compile(arguments.Pattern); err != nil {
		return newInvalidArgumentsResult(callID, fmt.Errorf("invalid grep pattern %q: %w", arguments.Pattern, err))
	}
	if isExtensionGroupPattern(arguments.Pattern) {
		return newInvalidArgumentsResult(callID, fmt.Errorf("grep pattern %q uses a (?...) group; POSIX regex does not support it, use ignore_case for case-insensitive matching", arguments.Pattern))
	}
	if arguments.Glob != "" && !doublestar.ValidatePattern(arguments.Glob) {
		return newInvalidArgumentsResult(callID, fmt.Errorf("invalid glob pattern %q", arguments.Glob))
	}
	// 2. 确认搜索起点存在；起点是文件时 glob 不起作用。
	searchPath, isDirectory, err := t.searcher.getSearchPath(ctx, arguments.Path, false)
	if err != nil {
		return newSearchFailedResult(ctx, callID, err)
	}
	request := &searchRequest{
		path:         searchPath,
		isFile:       !isDirectory,
		pattern:      arguments.Pattern,
		glob:         arguments.Glob,
		isIgnoreCase: arguments.IgnoreCase,
		limit:        arguments.Limit,
	}
	if request.isFile {
		request.glob = ""
	}
	// 3. 搜索并返回匹配行。
	result, err := t.searcher.searchMatches(ctx, request)
	if err != nil {
		return newSearchFailedResult(ctx, callID, err)
	}
	execution := encodeToolResult(callID, result, nil)
	execution.Result.Truncated = result.IsTruncated
	return execution
}

// getSearchPath 把 path 参数转成绝对路径并确认它存在；第二个返回值表示它是不是目录。
func (s *workspaceSearcher) getSearchPath(ctx context.Context, value string, isDirectoryRequired bool) (string, bool, error) {
	// 1. 展开路径并读取元数据，不存在属于 path 参数的问题，其他错误原样返回。
	absolutePath := resolveSearchPath(s.opt.Workspace.GetRootDirectory(), value)
	metadata, err := s.opt.Workspace.GetMetadata(ctx, absolutePath, true)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, &invalidSearchPathError{message: fmt.Sprintf("search path %q does not exist", value)}
	}
	if err != nil {
		return "", false, err
	}
	// 2. glob 要求起点是目录。
	if isDirectoryRequired && !metadata.IsDirectory {
		return "", false, &invalidSearchPathError{message: fmt.Sprintf("search path %q is not a directory", value)}
	}
	return absolutePath, metadata.IsDirectory, nil
}

// searchFiles 按选定的搜索方式列出匹配的文件，路径写成相对工作区根目录的写法并排序。
func (s *workspaceSearcher) searchFiles(ctx context.Context, request *searchRequest) (*globResult, error) {
	// 1. 选择搜索方式并列出路径，各方式最多返回 limit + 1 条。
	method, err := s.getSearchMethod(ctx)
	if err != nil {
		return nil, err
	}
	var paths []string
	switch method {
	case searchMethodRipgrep:
		paths, err = s.searchFilesWithRipgrep(ctx, request)
	case searchMethodGit:
		paths, err = s.searchFilesWithGit(ctx, request)
	case searchMethodFind:
		paths, err = s.searchFilesWithFind(ctx, request)
	default:
		paths, err = s.searchFilesWithWalk(ctx, request)
	}
	if err != nil {
		return nil, err
	}
	// 2. 写成相对路径并排序；多出来的那一条说明结果被截断。
	rootDirectory := s.opt.Workspace.GetRootDirectory()
	for index, absolutePath := range paths {
		paths[index] = getDisplayPath(rootDirectory, absolutePath)
	}
	slices.Sort(paths)
	result := &globResult{Paths: paths}
	if len(paths) > request.limit {
		result.Paths = paths[:request.limit]
		result.IsTruncated = true
	}
	return result, nil
}

// searchMatches 按选定的搜索方式搜索文件内容，路径写成相对工作区根目录的写法并按路径和行号排序。
func (s *workspaceSearcher) searchMatches(ctx context.Context, request *searchRequest) (*grepResult, error) {
	// 1. 选择搜索方式并搜索，各方式最多返回 limit + 1 条。
	method, err := s.getSearchMethod(ctx)
	if err != nil {
		return nil, err
	}
	var matches []grepMatch
	switch method {
	case searchMethodRipgrep:
		matches, err = s.searchMatchesWithRipgrep(ctx, request)
	case searchMethodGit:
		matches, err = s.searchMatchesWithGit(ctx, request)
	case searchMethodFind:
		matches, err = s.searchMatchesWithFind(ctx, request)
	default:
		matches, err = s.searchMatchesWithWalk(ctx, request)
	}
	if err != nil {
		return nil, err
	}
	// 2. 写成相对路径并排序；多出来的那一条说明结果被截断。
	rootDirectory := s.opt.Workspace.GetRootDirectory()
	for index := range matches {
		matches[index].Path = getDisplayPath(rootDirectory, matches[index].Path)
	}
	slices.SortFunc(matches, func(a, b grepMatch) int {
		return cmp.Or(strings.Compare(a.Path, b.Path), cmp.Compare(a.Line, b.Line))
	})
	result := &grepResult{Matches: matches}
	if len(matches) > request.limit {
		result.Matches = matches[:request.limit]
		result.IsTruncated = true
	}
	return result, nil
}

// getSearchMethod 返回这个工作区的搜索方式；第一次调用时在 Executor 上探测 rg 和 git。
func (s *workspaceSearcher) getSearchMethod(ctx context.Context) (searchMethod, error) {
	// 1. 只用遍历的工作区不探测。
	if s.opt.IsWalkOnly {
		return searchMethodWalk, nil
	}
	// 2. 持锁探测一次；命令没跑完不算探测完，下次调用重新探测。
	s.probeLock.Lock()
	defer s.probeLock.Unlock()
	if !s.isProbed {
		var ripgrepPath, gitPath string
		err := s.runSearchCommand(ctx, s.opt.Workspace.GetRootDirectory(), probeCommand, func(reader *bufio.Reader) (bool, error) {
			output, err := io.ReadAll(reader)
			for line := range strings.Lines(string(output)) {
				name, value, _ := strings.Cut(strings.TrimSpace(line), "=")
				switch name {
				case "rg":
					ripgrepPath = value
				case "git":
					gitPath = value
				}
			}
			return false, err
		})
		if err != nil {
			return searchMethodWalk, err
		}
		s.ripgrepPath, s.gitPath, s.isProbed = ripgrepPath, gitPath, true
	}
	// 3. 有 rg 用 rg，其次 git，都没有用 find。
	switch {
	case s.ripgrepPath != "":
		return searchMethodRipgrep, nil
	case s.gitPath != "":
		return searchMethodGit, nil
	}
	return searchMethodFind, nil
}

// searchFilesWithRipgrep 用 rg --files 列出文件，rg 自己跳过隐藏路径和 .gitignore 忽略的文件，再按 glob 过滤。
func (s *workspaceSearcher) searchFilesWithRipgrep(ctx context.Context, request *searchRequest) ([]string, error) {
	// 1. 以 path 为工作目录运行，输出是 ./ 开头的相对路径；glob 不交给 rg，由 Go 过滤，免得 rg 的 -g 覆盖它的隐藏和忽略规则。
	command := "rg --no-config --files --null --sort path -- ."
	return s.collectPaths(ctx, request.path, command, request.limit, func(rawPath string) (string, bool) {
		relativePath := strings.TrimPrefix(rawPath, "./")
		return filepath.Join(request.path, relativePath), isSearchGlobMatch(request.glob, relativePath)
	})
}

// searchMatchesWithRipgrep 用 rg 搜索内容，每行输出是 path\0line:text。
func (s *workspaceSearcher) searchMatchesWithRipgrep(ctx context.Context, request *searchRequest) ([]grepMatch, error) {
	// 1. 拼出命令；超长行由 rg 只输出开头 maxMatchTextChars+1 个字符；可选的忽略大小写放在正则之前，glob 不交给 rg，由 Go 过滤。
	command := fmt.Sprintf("rg --no-config --null --line-number --no-heading --with-filename --color never --sort path --max-columns %d --max-columns-preview", maxMatchTextChars+1)
	if request.isIgnoreCase {
		command += " -i"
	}
	// 2. 以搜索目录为工作目录运行；path 是文件时以它所在的目录为工作目录，只搜这个文件。
	directory, target := request.path, "."
	if request.isFile {
		directory, target = filepath.Dir(request.path), filepath.Base(request.path)
	}
	command += " -e " + quoteShellArgument(request.pattern) + " -- " + quoteShellArgument(target)
	return s.collectMatches(ctx, directory, command, request.limit, readRipgrepMatch, func(match grepMatch) (grepMatch, bool) {
		relativePath := strings.TrimPrefix(match.Path, "./")
		if request.glob != "" && !isSearchGlobMatch(request.glob, relativePath) {
			return grepMatch{}, false
		}
		match.Path = filepath.Join(directory, relativePath)
		return match, true
	})
}

// searchFilesWithGit 在 git 仓库里用 git ls-files 列出文件；path 不在仓库里时改用 find。
func (s *workspaceSearcher) searchFilesWithGit(ctx context.Context, request *searchRequest) ([]string, error) {
	// 1. 确认 path 在 git 仓库里。
	isRepository, err := s.isGitWorkTree(ctx, request.path)
	if err != nil {
		return nil, err
	}
	if !isRepository {
		return s.searchFilesWithFind(ctx, request)
	}
	// 2. 索引里还有、工作区已经删掉的文件不能列出来。
	deletedPaths, err := s.getGitDeletedPaths(ctx, request.path)
	if err != nil {
		return nil, err
	}
	// 3. 输出是相对 path 的路径：去掉已删除的文件和隐藏路径，再按 glob 过滤。
	command := gitCommand + " -C " + quoteShellArgument(request.path) + " ls-files -z --cached --others --exclude-standard"
	return s.collectPaths(ctx, s.opt.Workspace.GetRootDirectory(), command, request.limit, func(relativePath string) (string, bool) {
		if _, isDeleted := deletedPaths[relativePath]; isDeleted || isHiddenPath(relativePath) || !isSearchGlobMatch(request.glob, relativePath) {
			return "", false
		}
		return filepath.Join(request.path, relativePath), true
	})
}

// searchMatchesWithGit 在 git 仓库里用 git grep 搜索内容；path 不在仓库里时改用 find + grep。
func (s *workspaceSearcher) searchMatchesWithGit(ctx context.Context, request *searchRequest) ([]grepMatch, error) {
	// 1. 确认 path 在 git 仓库里；path 是文件时 git -C 进不去，也会改用 find。
	isRepository, err := s.isGitWorkTree(ctx, request.path)
	if err != nil {
		return nil, err
	}
	if !isRepository {
		return s.searchMatchesWithFind(ctx, request)
	}
	// 2. 每行输出是 path\0line\0text，路径相对 path：去掉隐藏路径，再按 glob 过滤。
	command := gitCommand + " -C " + quoteShellArgument(request.path) + " grep --color=never --untracked --exclude-standard -I -n -z -E"
	if request.isIgnoreCase {
		command += " -i"
	}
	command += " -e " + quoteShellArgument(translatePortablePattern(request.pattern))
	command = limitScriptLineBytes(command)
	return s.collectMatches(ctx, s.opt.Workspace.GetRootDirectory(), command, request.limit, readGitMatch, func(match grepMatch) (grepMatch, bool) {
		if isHiddenPath(match.Path) || (request.glob != "" && !isSearchGlobMatch(request.glob, match.Path)) {
			return grepMatch{}, false
		}
		match.Path = filepath.Join(request.path, match.Path)
		return match, true
	})
}

// searchFilesWithFind 用 find 列出文件，隐藏的文件和目录由 find 跳过。
func (s *workspaceSearcher) searchFilesWithFind(ctx context.Context, request *searchRequest) ([]string, error) {
	// 1. -mindepth 1 让搜索起点自己不被当作隐藏目录跳过；输出是以 NUL 结尾的绝对路径，按 glob 过滤。
	command := "find " + quoteShellArgument(request.path) + " -mindepth 1 -name '.*' -prune -o -type f -print0"
	return s.collectPaths(ctx, s.opt.Workspace.GetRootDirectory(), command, request.limit, func(absolutePath string) (string, bool) {
		relativePath, err := filepath.Rel(request.path, absolutePath)
		return absolutePath, err == nil && isSearchGlobMatch(request.glob, relativePath)
	})
}

// searchMatchesWithFind 用 find + grep 搜索内容，每行输出是 path:line:text；path 是文件时直接 grep 这个文件。
func (s *workspaceSearcher) searchMatchesWithFind(ctx context.Context, request *searchRequest) ([]grepMatch, error) {
	// 1. grep 的参数；/dev/null 让只有一个文件时输出也带文件名。
	grepCommand := "grep -n -I -E"
	if request.isIgnoreCase {
		grepCommand += " -i"
	}
	grepCommand += " -e " + quoteShellArgument(translatePortablePattern(request.pattern)) + " /dev/null"
	// 2. 文件直接 grep；目录交给 find，隐藏的文件和目录由 find 跳过。
	command := grepCommand + " " + quoteShellArgument(request.path)
	if !request.isFile {
		command = "find " + quoteShellArgument(request.path) + " -mindepth 1 -name '.*' -prune -o -type f -exec " + grepCommand + " {} +"
	}
	command = limitScriptLineBytes(command)
	return s.collectMatches(ctx, s.opt.Workspace.GetRootDirectory(), command, request.limit, readFindMatch, func(match grepMatch) (grepMatch, bool) {
		relativePath, err := filepath.Rel(request.path, match.Path)
		return match, request.glob == "" || (err == nil && isSearchGlobMatch(request.glob, relativePath))
	})
}

// searchFilesWithWalk 逐层遍历 Workspace 列出匹配 glob 的文件。
func (s *workspaceSearcher) searchFilesWithWalk(ctx context.Context, request *searchRequest) ([]string, error) {
	// 1. 遍历文件，按 glob 过滤，收满 limit + 1 条就停。
	paths := []string{}
	err := s.walkFiles(ctx, request, func(filePath string) (bool, error) {
		relativePath, err := filepath.Rel(request.path, filePath)
		if err == nil && isSearchGlobMatch(request.glob, relativePath) {
			paths = append(paths, filePath)
		}
		return len(paths) > request.limit, nil
	})
	return paths, err
}

// searchMatchesWithWalk 逐层遍历 Workspace，用 Go 的正则逐行搜索通过 glob 的文件。
func (s *workspaceSearcher) searchMatchesWithWalk(ctx context.Context, request *searchRequest) ([]grepMatch, error) {
	// 1. 编译正则；忽略大小写时在前面加 (?i)，内联标记只由这里使用。
	expression := request.pattern
	if request.isIgnoreCase {
		expression = "(?i)" + expression
	}
	matcher, err := regexp.Compile(expression)
	if err != nil {
		return nil, err
	}
	// 2. 遍历文件，只搜通过 glob 的文件，收满 limit + 1 条就停。
	matches := []grepMatch{}
	err = s.walkFiles(ctx, request, func(filePath string) (bool, error) {
		relativePath, err := filepath.Rel(request.path, filePath)
		if request.glob != "" && (err != nil || !isSearchGlobMatch(request.glob, relativePath)) {
			return false, nil
		}
		fileMatches, err := s.matchFileLines(ctx, filePath, matcher, request.limit+1-len(matches))
		matches = append(matches, fileMatches...)
		return len(matches) > request.limit, err
	})
	return matches, err
}

// walkFiles 从搜索起点逐层往下遍历，对每个文件调用 visit；visit 返回 true 时提前结束。
func (s *workspaceSearcher) walkFiles(ctx context.Context, request *searchRequest, visit func(filePath string) (bool, error)) error {
	// 1. 搜索起点是文件时只访问它自己，否则遍历目录。
	if request.isFile {
		_, err := visit(request.path)
		return err
	}
	_, err := s.walkDirectory(ctx, request.path, visit)
	return err
}

// walkDirectory 按名称顺序遍历一个目录，跳过名字以 . 开头的文件和目录；返回 true 表示 visit 要求提前结束。
func (s *workspaceSearcher) walkDirectory(ctx context.Context, directory string, visit func(filePath string) (bool, error)) (bool, error) {
	// 1. 读取目录；子目录递归，普通文件交给 visit，符号链接等其他类型跳过。
	entries, err := s.opt.Workspace.ReadDirectory(ctx, directory)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name, ".") {
			continue
		}
		entryPath := filepath.Join(directory, entry.Name)
		var isEnough bool
		switch {
		case entry.IsDirectory:
			isEnough, err = s.walkDirectory(ctx, entryPath, visit)
		case entry.IsFile:
			isEnough, err = visit(entryPath)
		}
		if isEnough || err != nil {
			return isEnough, err
		}
	}
	return false, nil
}

// matchFileLines 逐行匹配一个文件，开头 binaryProbeBytes 字节里有 NUL 的二进制文件跳过；最多返回 maxMatches 条。
func (s *workspaceSearcher) matchFileLines(ctx context.Context, filePath string, matcher *regexp.Regexp, maxMatches int) ([]grepMatch, error) {
	// 1. 打开文件，检查开头是否是二进制。
	file, err := s.opt.Workspace.OpenFileReader(ctx, filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 64<<10)
	head, err := reader.Peek(binaryProbeBytes)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return nil, nil
	}
	// 2. 逐行匹配；整行读进内存，超长的行也能读完。
	var matches []grepMatch
	for lineNumber := 1; len(matches) < maxMatches; lineNumber++ {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 && matcher.Match(bytes.TrimRight(line, "\r\n")) {
			matches = append(matches, grepMatch{Path: filePath, Line: lineNumber, Text: truncateMatchText(line)})
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	return matches, ctx.Err()
}

// isGitWorkTree path 是不是在 git 仓库的工作区里？path 不是目录或没有 git 仓库都是 false。
func (s *workspaceSearcher) isGitWorkTree(ctx context.Context, directory string) (bool, error) {
	// 1. 运行 rev-parse；以非 0 退出码结束就是不在仓库里。
	var output []byte
	err := s.runSearchCommand(ctx, s.opt.Workspace.GetRootDirectory(), gitCommand+" -C "+quoteShellArgument(directory)+" rev-parse --is-inside-work-tree", func(reader *bufio.Reader) (bool, error) {
		var err error
		output, err = io.ReadAll(reader)
		return false, err
	})
	var exitError *searchCommandError
	if errors.As(err, &exitError) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(output)) == "true", nil
}

// getGitDeletedPaths 返回 git 仓库里索引中还有、工作区已经删掉的文件，路径相对 directory。
func (s *workspaceSearcher) getGitDeletedPaths(ctx context.Context, directory string) (map[string]struct{}, error) {
	// 1. 列出已删除的文件，路径输出以 NUL 结尾。
	deletedPaths := map[string]struct{}{}
	err := s.runSearchCommand(ctx, s.opt.Workspace.GetRootDirectory(), gitCommand+" -C "+quoteShellArgument(directory)+" ls-files -z --deleted", func(reader *bufio.Reader) (bool, error) {
		for {
			deletedPath, err := readNulRecord(reader)
			if errors.Is(err, io.EOF) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			deletedPaths[deletedPath] = struct{}{}
		}
	})
	return deletedPaths, err
}

// collectPaths 在 directory 运行输出 NUL 结尾路径的命令；selectPath 把每条路径换成绝对路径，返回 false 的丢弃。收满 limit + 1 条就结束命令。
func (s *workspaceSearcher) collectPaths(ctx context.Context, directory, command string, limit int, selectPath func(rawPath string) (string, bool)) ([]string, error) {
	// 1. 边读边解析，收满就返回 true 让命令结束。
	paths := []string{}
	err := s.runSearchCommand(ctx, directory, command, func(reader *bufio.Reader) (bool, error) {
		for len(paths) <= limit {
			rawPath, err := readNulRecord(reader)
			if errors.Is(err, io.EOF) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			if path, ok := selectPath(rawPath); ok {
				paths = append(paths, path)
			}
		}
		return true, nil
	})
	return paths, err
}

// collectMatches 在 directory 运行输出匹配行的命令；readMatch 读一条，selectMatch 把路径换成绝对路径，返回 false 的丢弃。收满 limit + 1 条就结束命令。
func (s *workspaceSearcher) collectMatches(ctx context.Context, directory, command string, limit int,
	readMatch func(reader *bufio.Reader) (grepMatch, error), selectMatch func(match grepMatch) (grepMatch, bool)) ([]grepMatch, error) {
	// 1. 边读边解析，收满就返回 true 让命令结束。
	matches := []grepMatch{}
	err := s.runSearchCommand(ctx, directory, command, func(reader *bufio.Reader) (bool, error) {
		for len(matches) <= limit {
			match, err := readMatch(reader)
			if errors.Is(err, io.EOF) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			if match, ok := selectMatch(match); ok {
				matches = append(matches, match)
			}
		}
		return true, nil
	})
	return matches, err
}

// runSearchCommand 在 directory 运行一条 bash 命令，把 stdout 交给 readOutput 边读边解析。
// readOutput 返回 true 表示已经够了，命令随即结束；退出码 0、1 都按正常结束处理，其他退出码返回 searchCommandError。
func (s *workspaceSearcher) runSearchCommand(ctx context.Context, directory, command string, readOutput func(reader *bufio.Reader) (bool, error)) error {
	// 1. 启动命令：stdout 接管道，stderr 只留末尾给错误信息。
	outputReader, outputWriter := io.Pipe()
	var stderr tailBuffer
	process, err := s.opt.Workspace.StartProcess(ctx, &workspace.ProcessRequest{
		Command:   command,
		Directory: directory,
		Stdout:    outputWriter,
		Stderr:    &stderr,
	})
	if err != nil {
		return err
	}
	// 2. 后台等命令退出并关闭管道写端；ctx 取消时结束命令。
	var tasks sync.WaitGroup
	var exitCode int
	var waitErr error
	exited := make(chan struct{})
	tasks.Go(func() {
		defer close(exited)
		exitCode, waitErr = process.Wait()
		_ = outputWriter.Close()
	})
	tasks.Go(func() {
		select {
		case <-ctx.Done():
			terminateSearchProcess(process)
		case <-exited:
		}
	})
	// 3. 边读边解析；够了或解析失败就结束命令，并关闭读端让命令的输出不再阻塞。
	isEnough, readErr := readOutput(bufio.NewReaderSize(outputReader, 64<<10))
	if isEnough || readErr != nil {
		terminateSearchProcess(process)
	}
	_ = outputReader.Close()
	tasks.Wait()
	// 4. 取消优先；解析够了的命令是我们结束的，退出码不看；0、1 是正常结束。
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case readErr != nil:
		return readErr
	case isEnough:
		return nil
	case waitErr != nil:
		return waitErr
	case exitCode == 0 || exitCode == 1:
		return nil
	}
	return &searchCommandError{exitCode: exitCode, stderr: strings.TrimSpace(string(stderr.data))}
}

// terminateSearchProcess 结束搜索命令；没能确认结束时放手这条命令，让 Wait 尽快返回。
func terminateSearchProcess(process workspace.Process) {
	// 1. 先 Terminate，失败再 Close。
	if err := process.Terminate(); err != nil {
		_ = process.Close()
	}
}

// readNulRecord 读一条以 NUL 结尾的记录；读到末尾时返回 io.EOF。
func readNulRecord(reader *bufio.Reader) (string, error) {
	// 1. 末尾没有 NUL 的残留内容也算一条记录。
	record, err := reader.ReadString(0)
	if err != nil && record == "" {
		return "", err
	}
	return strings.TrimSuffix(record, "\x00"), nil
}

// readRipgrepMatch 读 rg 的一条输出：path\0line:text。
func readRipgrepMatch(reader *bufio.Reader) (grepMatch, error) {
	// 1. 读路径，再读 line:text 一行。
	filePath, err := readNulRecord(reader)
	if err != nil {
		return grepMatch{}, err
	}
	rest, err := reader.ReadBytes('\n')
	if err != nil && len(rest) == 0 {
		return grepMatch{}, err
	}
	lineNumber, text, _ := bytes.Cut(rest, []byte(":"))
	line, err := strconv.Atoi(string(lineNumber))
	if err != nil {
		return grepMatch{}, fmt.Errorf("unexpected ripgrep output for %q", filePath)
	}
	// 2. 去掉行尾换行和 rg 加的截断提示，再按 maxMatchTextChars 截断。
	text = bytes.TrimSuffix(bytes.TrimRight(text, "\r\n"), []byte(ripgrepOmittedMarker))
	return grepMatch{Path: filePath, Line: line, Text: truncateMatchText(text)}, nil
}

// readGitMatch 读 git grep -z 的一条输出：path\0line\0text。
func readGitMatch(reader *bufio.Reader) (grepMatch, error) {
	// 1. 依次读路径、行号和正文。
	filePath, err := readNulRecord(reader)
	if err != nil {
		return grepMatch{}, err
	}
	lineNumber, err := readNulRecord(reader)
	if err != nil {
		return grepMatch{}, err
	}
	text, err := reader.ReadBytes('\n')
	if err != nil && len(text) == 0 {
		return grepMatch{}, err
	}
	line, err := strconv.Atoi(lineNumber)
	if err != nil {
		return grepMatch{}, fmt.Errorf("unexpected git grep output for %q", filePath)
	}
	return grepMatch{Path: filePath, Line: line, Text: truncateMatchText(text)}, nil
}

// readFindMatch 读 grep 的一条输出：path:line:text；路径里带冒号时，取第一处 :数字: 作为分隔。
func readFindMatch(reader *bufio.Reader) (grepMatch, error) {
	// 1. 读一整行。
	row, err := reader.ReadBytes('\n')
	if err != nil && len(row) == 0 {
		return grepMatch{}, err
	}
	// 2. 找第一处 :数字: ，前面是路径，后面是正文。
	for start := 0; start < len(row); {
		colon := bytes.IndexByte(row[start:], ':')
		if colon < 0 {
			break
		}
		colon += start
		end := colon + 1
		for end < len(row) && row[end] >= '0' && row[end] <= '9' {
			end++
		}
		if end > colon+1 && end < len(row) && row[end] == ':' {
			line, _ := strconv.Atoi(string(row[colon+1 : end]))
			return grepMatch{Path: string(row[:colon]), Line: line, Text: truncateMatchText(row[end+1:])}, nil
		}
		start = colon + 1
	}
	return grepMatch{}, fmt.Errorf("unexpected grep output %q", truncateMatchText(row))
}
