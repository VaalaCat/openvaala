// Package hook 运行用户配置的 hook：按事件找出命中的 hook，在事件所在的 Executor 上并行运行，
// 解析它们的输出并合并成一个决定。匹配、运行、输出解析和合并都由本包负责，触发事件的各层只看合并后的结果。
package hook

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sourcegraph/conc/pool"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/utils/syncmap"
	"github.com/VaalaCat/openvaala/pkg/workspace"
)

// Hooks 是一份配置里的全部 hook；RunEvent 找出命中事件的 hook，在请求给出的 Executor 上并行运行，
// 每个 hook 开始和结束时各往 runUpdates 发一次运行记录，全部结束后返回合并结果。
type Hooks interface {
	// RunEvent 运行命中 request.Event 的全部 hook 并返回合并后的决定，没有命中时返回空决定。
	// 每个命中的 hook 先发一次 running 记录，结束时再发一次最终记录（同一个 ID）；RunEvent 返回后不再发送。
	// runUpdates 由调用方创建和关闭，接收方须在 RunEvent 运行期间持续接收；ctx 取消时运行中的命令被结束，记为 failed。
	RunEvent(ctx context.Context, request *HookEventRequest, runUpdates chan<- *defs.HookRun) *HookOutcome
	// GetHookInfos 返回配置的全部 hook，按 DisplayOrder 排列，供 hooks/list 显示。
	GetHookInfos() []*HookInfo
}

// hooksImpl 保存一版配置的 hook 定义，记着各 Executor 上已经拷过的脚本和插件目录。
type hooksImpl struct {
	opt *HooksOpt
	// localWorkspace 是 serve 本机根目录 / 的工作区，用来读 script hook 的脚本和插件目录。
	localWorkspace workspace.Workspace
	// copiedScripts 记下这个 serve 进程里每个 Executor 上已经拷过或正在拷的脚本；拷贝失败的会被删掉，下次运行重试。
	copiedScripts syncmap.SyncMap[scriptCopyKey, *scriptCopy]
	// copiedPlugins 记下这版配置里每个 Executor 上已经拷过或正在拷的插件目录；拷贝失败的会被删掉，下次运行重试。
	copiedPlugins syncmap.SyncMap[pluginCopyKey, *pluginCopy]
}

// HooksOpt 是一份配置里的全部 hook 定义，按配置顺序排列。
type HooksOpt struct {
	Definitions []*HookDefinition
}

// NewHooksOpt 用按配置顺序排列的 hook 定义创建 HooksOpt。
func NewHooksOpt(definitions []*HookDefinition) *HooksOpt {
	// 1. 保存定义列表。
	return &HooksOpt{Definitions: definitions}
}

// NewHooks 创建 Hooks，脚本和插件目录从本机 / 下读取。
func NewHooks(opt *HooksOpt) Hooks {
	// 1. 创建以本机 / 为根目录的工作区，保存 hook 配置。
	localWorkspace, err := workspace.NewLocalWorkspace(workspace.NewLocalWorkspaceOpt("/"))
	if err != nil {
		// 根目录 / 是绝对路径，本机工作区不会拒绝它。
		panic(err)
	}
	return &hooksImpl{
		opt:            opt,
		localWorkspace: localWorkspace,
	}
}

// RunEvent 并行运行命中事件的 hook，再合并决定。
func (h *hooksImpl) RunEvent(ctx context.Context, request *HookEventRequest, runUpdates chan<- *defs.HookRun) *HookOutcome {
	// 1. 按事件和 matcher 选出 hook，每个 hook 在结果里占一个位置。
	matched := getMatchedDefinitions(h.opt.Definitions, request.Event)
	outcomes := make([]*HookOutcome, len(matched))
	// 2. 并行运行并等全部结束，再按顺序合并结果。
	tasks := pool.New()
	for index, definition := range matched {
		tasks.Go(func() {
			outcomes[index] = h.runDefinition(ctx, request, definition, runUpdates)
		})
	}
	tasks.Wait()
	return mergeOutcomes(outcomes)
}

// GetHookInfos 返回按展示顺序排列的 hook 信息。
func (h *hooksImpl) GetHookInfos() []*HookInfo {
	// 1. 提取事件、matcher、显示命令、超时、来源、定义哈希和插件名。
	infos := make([]*HookInfo, 0, len(h.opt.Definitions))
	for _, definition := range h.opt.Definitions {
		pluginName := ""
		if definition.Plugin != nil {
			pluginName = definition.Plugin.Name
		}
		infos = append(infos, &HookInfo{
			Key:            definition.Key,
			Event:          definition.Event,
			Matcher:        definition.Matcher.String(),
			Handler:        getHandler(definition),
			TimeoutSeconds: int(definition.Timeout / time.Second),
			SourcePath:     definition.SourcePath,
			DisplayOrder:   definition.DisplayOrder,
			CurrentHash:    getDefinitionHash(definition),
			PluginName:     pluginName,
		})
	}
	// 2. 按 DisplayOrder 稳定排序。
	slices.SortStableFunc(infos, func(a, b *HookInfo) int { return a.DisplayOrder - b.DisplayOrder })
	return infos
}

// runDefinition 运行一个 hook，开始和结束时各发一次运行记录，返回它的决定。
func (h *hooksImpl) runDefinition(ctx context.Context, request *HookEventRequest, definition *HookDefinition, runUpdates chan<- *defs.HookRun) *HookOutcome {
	// 1. 创建 running 记录，工具事件带上 CallID，发送副本。
	run := &defs.HookRun{
		ID:           defs.NewHookRunID(),
		Event:        request.Event.Name,
		ExecutorID:   request.ExecutorID,
		SourcePath:   definition.SourcePath,
		Handler:      getHandler(definition),
		DisplayOrder: definition.DisplayOrder,
		Status:       defs.HookRunRunning,
		StartedAt:    time.Now().UnixMilli(),
	}
	if request.Event.ToolCall != nil {
		run.CallID = request.Event.ToolCall.CallID
	}
	runUpdates <- run.Clone()

	// 2. 执行命令，记下完成时间并写日志，发送最终副本，再转成决定。
	stderr := h.runCommand(ctx, request, definition, run)
	run.CompletedAt = time.Now().UnixMilli()
	logHookRun(ctx, run, stderr)
	runUpdates <- run.Clone()
	return newRunOutcome(definition, run, ctx.Err() != nil)
}

// runCommand 执行 hook 命令并把结果写入运行记录；退出码 0 时返回 stderr 供日志使用，其他情况返回空串。
func (h *hooksImpl) runCommand(ctx context.Context, request *HookEventRequest, definition *HookDefinition, run *defs.HookRun) (stderr string) {
	// 1. 把事件编码成 stdin 的 JSON，失败时记为 failed。
	input, err := newCommandInput(request)
	if err != nil {
		failHookRun(run, fmt.Sprintf("encode hook input: %v", err))
		return ""
	}
	// 2. 插件 hook 先取得插件在目标机器上的目录，失败原因以 copy plugin: 开头；普通 script 先拷到目标机器，再生成命令行。
	commandLine := definition.Command
	switch {
	case definition.Plugin != nil:
		pluginDirectory, err := h.getPluginDirectory(ctx, request, definition.Plugin)
		if err != nil {
			failHookRun(run, "copy plugin: "+err.Error())
			return ""
		}
		commandLine = getPluginCommandLine(pluginDirectory, definition)
	case definition.ScriptPath != "":
		targetPath, err := h.copyScript(ctx, request, definition)
		if err != nil {
			failHookRun(run, err.Error())
			return ""
		}
		commandLine = quoteScriptCommand(targetPath, definition.Args)
	}
	// 3. 在事件的工作区执行命令，启动、等待出错或被中断时记为 failed。
	result, err := runProcess(ctx, request.Workspace, commandLine, definition.Timeout, input)
	if err != nil {
		failHookRun(run, err.Error())
		return ""
	}
	// 4. 退出码 0 时解析 stdout，stderr 只写日志；能拦截的事件退出码 2 记为 blocked，以 stderr 为原因；其他退出码记为 failed。
	switch {
	case result.exitCode == 0:
		if err := applyCommandOutput(ctx, run, result.stdout.getText()); err != nil {
			failHookRun(run, err.Error())
		}
		return result.stderr.getText()
	case result.exitCode == 2 && isBlockableEvent(run.Event):
		run.Status = defs.HookRunBlocked
		run.BlockReason = result.stderr.getText()
		if run.BlockReason == "" {
			run.BlockReason = "hook exited with status 2"
		}
	default:
		failHookRun(run, formatExitError(result.exitCode, result.stderr.getText()))
	}
	return ""
}

// copyScript 把脚本拷到事件所在的 Executor，返回目标路径；同一 Executor 上同名同内容的脚本只拷一次。
func (h *hooksImpl) copyScript(ctx context.Context, request *HookEventRequest, definition *HookDefinition) (string, error) {
	// 1. 算本机脚本权限和内容的哈希，失败时返回 read script: 开头的错误。
	scriptHash, err := getFileHash(ctx, h.localWorkspace, definition.ScriptPath)
	if err != nil {
		return "", fmt.Errorf("read script: %w", err)
	}

	// 2. 按 Executor、文件名和哈希找拷贝任务；已有任务时等它完成或本次 ctx 取消。
	key := scriptCopyKey{
		executorID: request.ExecutorID,
		fileName:   filepath.Base(definition.ScriptPath),
		hash:       scriptHash,
	}
	copying, isRecorded := h.copiedScripts.LoadOrStore(key, &scriptCopy{done: make(chan struct{})})
	if isRecorded {
		select {
		case <-copying.done:
			return copying.targetPath, copying.err
		case <-ctx.Done():
			return "", fmt.Errorf("copy script: %w", ctx.Err())
		}
	}

	// 3. 第一个运行负责拷贝，失败时删掉任务记录，下次运行重新拷；再关闭 done，返回路径或 copy script: 开头的错误。
	copying.targetPath, copying.err = h.copyScriptToWorkspace(ctx, request.Workspace, definition.ScriptPath, scriptHash)
	if copying.err != nil {
		h.copiedScripts.Delete(key)
	}
	close(copying.done)
	return copying.targetPath, copying.err
}

// copyScriptToWorkspace 把本机脚本拷到目标机器的缓存目录。
func (h *hooksImpl) copyScriptToWorkspace(ctx context.Context, target workspace.Workspace, scriptPath, scriptHash string) (string, error) {
	// 1. 取得目标机器的主目录，算出按哈希命名的缓存路径。
	homeDirectory, err := target.GetHomeDirectory(ctx)
	if err != nil {
		return "", fmt.Errorf("copy script: %w", err)
	}
	targetPath := getScriptTargetPath(homeDirectory, scriptHash, scriptPath)
	// 2. 目标文件的权限和内容哈希相同时直接用，否则复制过去。
	if targetHash, err := getFileHash(ctx, target, targetPath); err == nil && targetHash == scriptHash {
		return targetPath, nil
	}
	if err := workspace.CopyFile(ctx, h.localWorkspace, scriptPath, target, targetPath); err != nil {
		return "", fmt.Errorf("copy script: %w", err)
	}
	return targetPath, nil
}

// getPluginDirectory 返回插件在事件所在 Executor 上的目录：local 用本机目录，其他 Executor 用拷过去的副本。
func (h *hooksImpl) getPluginDirectory(ctx context.Context, request *HookEventRequest, plugin *defs.Plugin) (string, error) {
	// 1. local 直接用本机插件目录。
	if request.ExecutorID == defs.LocalExecutorID {
		return plugin.Directory, nil
	}

	// 2. 其他 Executor 按插件名找拷贝任务，已有任务时等它完成或 ctx 取消。
	key := pluginCopyKey{
		executorID: request.ExecutorID,
		pluginName: plugin.Name,
	}
	copying, isRecorded := h.copiedPlugins.LoadOrStore(key, &pluginCopy{done: make(chan struct{})})
	if isRecorded {
		select {
		case <-copying.done:
			return copying.targetDirectory, copying.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	// 3. 第一个运行负责拷贝，失败时删掉任务记录，下次运行重新拷；再关闭 done 公布结果。
	copying.targetDirectory, copying.err = h.copyPluginToWorkspace(ctx, request.Workspace, plugin)
	if copying.err != nil {
		h.copiedPlugins.Delete(key)
	}
	close(copying.done)
	return copying.targetDirectory, copying.err
}

// copyPluginToWorkspace 把插件目录拷到目标机器上按内容哈希命名的缓存目录，返回这个目录；目录已存在时不再拷。
func (h *hooksImpl) copyPluginToWorkspace(ctx context.Context, target workspace.Workspace, plugin *defs.Plugin) (string, error) {
	// 1. 取得目标机器的主目录，再算本机插件目录的哈希。
	homeDirectory, err := target.GetHomeDirectory(ctx)
	if err != nil {
		return "", err
	}
	pluginHash, err := getPluginHash(ctx, h.localWorkspace, plugin.Directory)
	if err != nil {
		return "", err
	}
	// 2. 拷到缓存目录下的 <插件名>-<哈希前 16 位>，跳过 .git。
	targetDirectory := path.Join(homeDirectory, pluginCacheDirectory, plugin.Name+"-"+pluginHash[:pluginHashLength])
	if err := workspace.CopyDirectory(ctx, h.localWorkspace, plugin.Directory, target, targetDirectory, pluginSkippedNames); err != nil {
		return "", err
	}
	return targetDirectory, nil
}

// runProcess 在工作区运行命令，等它退出、超时或被取消。
func runProcess(ctx context.Context, executorWorkspace workspace.Workspace, command string, timeout time.Duration, input []byte) (*commandResult, error) {
	// 1. 设置超时，准备各保留 64 KiB 的 stdout、stderr 缓冲。
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result := &commandResult{
		stdout: &limitedBuffer{},
		stderr: &limitedBuffer{},
	}
	// 2. 在工作区根目录启动命令，stdin 是事件输入；启动失败时如果已取消或超时，返回中断或超时错误。
	process, err := executorWorkspace.StartProcess(runCtx, &workspace.ProcessRequest{
		Command:   command,
		Directory: executorWorkspace.GetRootDirectory(),
		Stdin:     bytes.NewReader(input),
		Stdout:    result.stdout,
		Stderr:    result.stderr,
	})
	if err != nil {
		if runCtx.Err() != nil {
			return nil, getInterruptionError(ctx, timeout)
		}
		return nil, fmt.Errorf("start hook: %w", err)
	}

	// 3. 后台等进程退出；正常退出时不管退出码是多少都返回命令结果。
	type processExit struct {
		code int
		err  error
	}
	exited := make(chan processExit, 1)
	go func() {
		code, err := process.Wait()
		exited <- processExit{
			code: code,
			err:  err,
		}
	}()
	select {
	case exit := <-exited:
		if exit.err != nil {
			return nil, fmt.Errorf("wait hook: %w", exit.err)
		}
		result.exitCode = exit.code
		return result, nil
	// 4. 超时或取消时结束进程和它派生的进程，结束失败时记警告并关闭进程；等后台 Wait 返回后报告中断原因。
	case <-runCtx.Done():
		if err := process.Terminate(); err != nil {
			logger := telemetry.GetLogger(ctx)
			logger.Warn("terminate process failed", "error", err)
			if err := process.Close(); err != nil {
				logger.Warn("close process failed", "error", err)
			}
		}
		<-exited
		return nil, getInterruptionError(ctx, timeout)
	}
}

// getInterruptionError 返回命令被中断或超时的错误。
func getInterruptionError(ctx context.Context, timeout time.Duration) error {
	// 1. 父 ctx 已结束时返回 interrupted，否则返回带时限的超时错误。
	if ctx.Err() != nil {
		return errors.New("interrupted")
	}
	return fmt.Errorf("timed out after %s", timeout)
}

// applyCommandOutput 解析退出码 0 的 hook 输出，写入运行记录。
func applyCommandOutput(ctx context.Context, run *defs.HookRun, stdout string) error {
	// 1. 记为 completed；stdout 为空时直接返回，不以 { 开头的文本只在 SessionStart、SubagentStart、UserPromptSubmit 上当作附加上下文。
	run.Status = defs.HookRunCompleted
	if stdout == "" {
		return nil
	}
	if !strings.HasPrefix(stdout, "{") {
		if isPlainTextContextEvent(run.Event) {
			run.AdditionalContext = stdout
		}
		return nil
	}

	// 2. 解析 JSON，PreToolUse 的 permissionDecision 只能是空、allow 或 deny。
	output, err := parseCommandOutput(stdout)
	if err != nil {
		return fmt.Errorf("invalid hook output: %w", err)
	}
	specific := output.HookSpecificOutput
	isDenied := false
	if run.Event == defs.HookEventPreToolUse {
		switch specific.PermissionDecision {
		case "", "allow":
		case "deny":
			isDenied = true
		default:
			return fmt.Errorf("unsupported permissionDecision %q", specific.PermissionDecision)
		}
	}

	// 3. deny 或能拦截的事件上的 block 都记为 blocked，没给原因时填默认说明。
	blockReason := ""
	if isDenied {
		blockReason = specific.PermissionDecisionReason
	}
	if output.Decision == "block" && isBlockableEvent(run.Event) {
		isDenied = true
		if blockReason == "" {
			blockReason = output.Reason
		}
	}
	if isDenied {
		run.Status = defs.HookRunBlocked
		run.BlockReason = blockReason
		if run.BlockReason == "" {
			run.BlockReason = "blocked by hook"
		}
	}
	// 4. 只有 PreToolUse 改写工具参数，其他事件的 updatedInput 记警告后忽略。
	if len(specific.UpdatedInput) > 0 && string(specific.UpdatedInput) != "null" {
		if run.Event == defs.HookEventPreToolUse {
			run.UpdatedToolArguments = string(specific.UpdatedInput)
		} else {
			telemetry.GetLogger(ctx).Warn("hook updatedInput ignored", "event", run.Event, "handler", run.Handler)
		}
	}
	// 5. 记下附加上下文和用户消息，continue 为 false 时记为 stopped 并写入停止原因。
	run.AdditionalContext = specific.AdditionalContext
	run.SystemMessage = output.SystemMessage
	if output.Continue != nil && !*output.Continue {
		run.Status = defs.HookRunStopped
		run.StopReason = output.StopReason
	}
	return nil
}

// logHookRun 为一次 hook 运行记一条日志。
func logHookRun(ctx context.Context, run *defs.HookRun, stderr string) {
	// 1. 带上事件、命令、Executor、状态、耗时，stderr 不为空时也带上。
	logger := telemetry.GetLogger(ctx).With(
		"event", run.Event,
		"handler", run.Handler,
		"executor", run.ExecutorID,
		"status", run.Status,
		"duration_ms", run.CompletedAt-run.StartedAt,
	)
	if stderr != "" {
		logger = logger.With("stderr", stderr)
	}
	// 2. failed 写 Warn，其余状态写 Info。
	if run.Status == defs.HookRunFailed {
		logger.Warn("hook failed", "error", run.ErrorMessage)
		return
	}
	logger.Info("hook finished")
}

// ── 运行结果 ──

// hookResultSeparator 分隔多个 hook 的拦下原因和结束原因。
const hookResultSeparator = "\n\n"

// failHookRun 把 hook 运行记为失败。
func failHookRun(run *defs.HookRun, message string) {
	// 1. 写入 failed 状态和错误原因。
	run.Status = defs.HookRunFailed
	run.ErrorMessage = message
}

// isTransparentRun 这次运行是否正常完成且没有任何要处理的输出？
func isTransparentRun(run *defs.HookRun) bool {
	// 1. 状态是 completed，且附加上下文、用户消息和改写参数都为空。
	return run.Status == defs.HookRunCompleted &&
		run.AdditionalContext == "" && run.SystemMessage == "" && run.UpdatedToolArguments == ""
}

// newRunOutcome 把一次运行的结果转成决定。
func newRunOutcome(definition *HookDefinition, run *defs.HookRun, isInterrupted bool) *HookOutcome {
	// 1. 取出拦下、参数改写和结束本轮的结果。
	outcome := &HookOutcome{
		IsBlocked:            run.BlockReason != "",
		BlockReason:          run.BlockReason,
		UpdatedToolArguments: run.UpdatedToolArguments,
		IsTurnStopped:        run.Status == defs.HookRunStopped,
		StopReason:           run.StopReason,
	}
	// 2. 配了 on_failure: block 时 hook 出错按拦下处理；本轮被中断不算，超时仍算出错。
	if run.Status == defs.HookRunFailed && definition.IsBlockedOnFailure && !isInterrupted {
		outcome.IsBlocked = true
		outcome.BlockReason = "hook failed: " + run.ErrorMessage
	}
	// 3. 有输出或没正常完成的运行记录要保存进历史。
	if !isTransparentRun(run) {
		outcome.SavedRuns = []*defs.HookRun{run}
	}
	return outcome
}

// mergeOutcomes 按顺序合并各 hook 的决定。
func mergeOutcomes(outcomes []*HookOutcome) *HookOutcome {
	// 1. 有一个拦下就算拦下、有一个结束本轮就算结束，收集原因和运行记录，工具参数取最后一个非空的改写。
	merged := &HookOutcome{}
	var blockReasons, stopReasons []string
	for _, outcome := range outcomes {
		merged.IsBlocked = merged.IsBlocked || outcome.IsBlocked
		merged.IsTurnStopped = merged.IsTurnStopped || outcome.IsTurnStopped
		if outcome.BlockReason != "" {
			blockReasons = append(blockReasons, outcome.BlockReason)
		}
		if outcome.StopReason != "" {
			stopReasons = append(stopReasons, outcome.StopReason)
		}
		if outcome.UpdatedToolArguments != "" {
			merged.UpdatedToolArguments = outcome.UpdatedToolArguments
		}
		merged.SavedRuns = append(merged.SavedRuns, outcome.SavedRuns...)
	}
	// 2. 拦下原因和结束原因各自用空行拼接。
	merged.BlockReason = strings.Join(blockReasons, hookResultSeparator)
	merged.StopReason = strings.Join(stopReasons, hookResultSeparator)
	return merged
}

// ── 事件匹配 ──

// toolNameAliases 是 matcher 里认的工具别名，对应 OpenVaala 的真名。
var toolNameAliases = map[string]string{
	"Bash":  "bash",
	"Write": "apply_patch",
	"Edit":  "apply_patch",
	"Read":  "read_file",
	"Agent": "spawn_agent",
}

// exactNamePattern 匹配只由字母、数字、_、- 和 | 组成的 matcher。
var exactNamePattern = regexp.MustCompile(`^[A-Za-z0-9_|-]+$`)

// HookMatcher 判断一个 hook 是否命中事件：不写、空串或 * 全部命中；
// 只由字母、数字、_、- 和 | 组成时按准确名字匹配（| 表示或，并认 Bash、Write、Edit、Read、Agent 这几个工具别名）；
// 其他写法按 Go 正则匹配，不加首尾锚点。
type HookMatcher struct {
	pattern string
	isAll   bool
	// names 是准确名字匹配时的全部名字，已包含别名对应的真名。
	names      map[string]struct{}
	expression *regexp.Regexp
}

// NewHookMatcher 解析 hook 的匹配表达式。
func NewHookMatcher(pattern string) (*HookMatcher, error) {
	// 1. 空串或 * 匹配全部；简单名字按 | 拆开并加上别名对应的真名；其他写法编译成不加首尾锚点的 Go 正则。
	matcher := &HookMatcher{pattern: pattern}
	switch {
	case pattern == "" || pattern == "*":
		matcher.isAll = true
	case exactNamePattern.MatchString(pattern):
		matcher.names = map[string]struct{}{}
		for _, name := range strings.Split(pattern, "|") {
			matcher.names[name] = struct{}{}
			if alias, ok := toolNameAliases[name]; ok {
				matcher.names[alias] = struct{}{}
			}
		}
	default:
		expression, err := regexp.Compile(pattern)
		if err != nil {
			return nil, err
		}
		matcher.expression = expression
	}
	return matcher, nil
}

// IsMatched 给定值中是否有任意一个命中 matcher？
func (m *HookMatcher) IsMatched(values ...string) bool {
	// 1. 匹配全部时直接返回 true，否则逐个值检查正则或准确名字。
	if m.isAll {
		return true
	}
	for _, value := range values {
		if m.expression != nil && m.expression.MatchString(value) {
			return true
		}
		if _, ok := m.names[value]; ok {
			return true
		}
	}
	return false
}

// String 返回 matcher 的配置原文。
func (m *HookMatcher) String() string {
	// 1. 返回保存的匹配表达式。
	return m.pattern
}
