package tools

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/prompt"
	"github.com/VaalaCat/openvaala/pkg/terminal"
	"github.com/VaalaCat/openvaala/pkg/utils/jsonutil"
	"github.com/VaalaCat/openvaala/pkg/workspace"
)

// bashToolImpl 在环境的工作区后台启动命令，等 wait_seconds 后返回命令当时的状态和第一页输出。
type bashToolImpl struct {
	*executor.BaseTool[bashArguments]

	opt *CommandToolsOpt
}

// readCommandOutputToolImpl 按编号读本环境命令从 cursor 起的一页输出。
type readCommandOutputToolImpl struct {
	*executor.BaseTool[readCommandOutputArguments]

	opt *CommandToolsOpt
}

// writeStdinToolImpl 把字符写进本环境 interactive 命令的终端。
type writeStdinToolImpl struct {
	*executor.BaseTool[writeStdinArguments]

	opt *CommandToolsOpt
}

// stopCommandToolImpl 停止本环境的命令及其进程组。
type stopCommandToolImpl struct {
	*executor.BaseTool[stopCommandArguments]

	opt *CommandToolsOpt
}

// listCommandsToolImpl 列出本环境启动成功、还没淘汰的命令。
type listCommandsToolImpl struct {
	*executor.BaseTool[listCommandsArguments]

	opt *CommandToolsOpt
}

// bashArguments 是 bash 的调用参数。
type bashArguments struct {
	Command        string `json:"command" jsonschema:"description=Bash 命令,minLength=1"`
	CWD            string `json:"cwd,omitempty" jsonschema:"description=命令工作目录：相对路径以工作区根目录为基准，也接受位于根目录内的绝对路径；省略或 \".\" 表示根目录，不能越出根目录。,minLength=1,default=."`
	Interactive    bool   `json:"interactive,omitempty" jsonschema:"description=是否使用 PTY；只有 interactive 命令能用 write_stdin 输入,default=false"`
	WaitSeconds    int    `json:"wait_seconds,omitempty" jsonschema:"description=最多等待命令结束的秒数；到期返回 running，命令继续在后台运行；0 表示启动后立即返回,minimum=0,default=10"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"description=从启动起的总运行时限（秒），到期停止命令；0 表示不限,minimum=0,default=600"`
}

// readCommandOutputArguments 是 read_command_output 的调用参数。
type readCommandOutputArguments struct {
	CommandID   string `json:"command_id" jsonschema:"description=bash 返回的 command_id,minLength=1"`
	Cursor      uint64 `json:"cursor,omitempty" jsonschema:"description=从这个输出位置读；续读时用上次结果的 next_cursor,minimum=0,default=0"`
	WaitSeconds int    `json:"wait_seconds,omitempty" jsonschema:"description=没有新输出时最多等待的秒数,minimum=0,default=10"`
}

// writeStdinArguments 是 write_stdin 的调用参数。
type writeStdinArguments struct {
	CommandID string `json:"command_id" jsonschema:"description=bash 返回的 command_id，命令须以 interactive=true 启动,minLength=1"`
	Chars     string `json:"chars" jsonschema:"description=原样写入终端的字符，不会自动加换行"`
}

// stopCommandArguments 是 stop_command 的调用参数。
type stopCommandArguments struct {
	CommandID string `json:"command_id" jsonschema:"description=bash 返回的 command_id,minLength=1"`
}

// listCommandsArguments 是 list_commands 的调用参数，没有字段。
type listCommandsArguments struct{}

// CommandResult 是 bash、read_command_output 结果的 content：命令当时的状态，和从 cursor 起的一页输出。
// 界面用 ParseCommandResult 从工具结果里取出它。
type CommandResult struct {
	CommandID string `json:"command_id"`
	Status    string `json:"status"`
	// ExitCode 只有 exited 有；被信号终止（含停止）时为 -1。
	ExitCode   *int   `json:"exit_code"`
	StopReason string `json:"stop_reason,omitempty"`
	// Error 是 failed 的原因，其他状态为空。
	Error      string `json:"error,omitempty"`
	Output     string `json:"output"`
	NextCursor uint64 `json:"next_cursor"`
	HasMore    bool   `json:"has_more"`
	Truncated  bool   `json:"truncated,omitempty"`
}

// writeStdinResult 是 write_stdin 的结果：写入后的命令状态和实际写入的字节数。
type writeStdinResult struct {
	CommandID    string `json:"command_id"`
	Status       string `json:"status"`
	WrittenBytes int    `json:"written_bytes"`
}

// stopCommandResult 是 stop_command 的结果：命令停下后的最终状态。
type stopCommandResult struct {
	CommandID  string `json:"command_id"`
	Status     string `json:"status"`
	ExitCode   *int   `json:"exit_code"`
	StopReason string `json:"stop_reason,omitempty"`
}

// listCommandsResult 是 list_commands 的结果：本环境还没淘汰的命令，按启动顺序排列。
type listCommandsResult struct {
	Commands []commandInfo `json:"commands"`
}

// commandInfo 是 list_commands 里的一条命令。
type commandInfo struct {
	CommandID string `json:"command_id"`
	Command   string `json:"command"`
	// CWD 是命令的绝对工作目录。
	CWD         string `json:"cwd"`
	Interactive bool   `json:"interactive"`
	Status      string `json:"status"`
	ExitCode    *int   `json:"exit_code"`
	StopReason  string `json:"stop_reason,omitempty"`
}

// CommandToolsOpt 保存五个命令工具共用的环境和命令集合。
type CommandToolsOpt struct {
	// EnvironmentID 是命令所属的执行环境；查找、列出命令都只看这个环境。
	EnvironmentID string
	// Workspace 是启动命令的工作区；cwd 以它的根目录为基准。
	Workspace workspace.Workspace
	// TerminalManager 是本 Session 的命令集合，由 Session 持有。
	TerminalManager terminal.TerminalManager
}

// NewCommandToolsOpt 保存命令所属环境、工作区和 Session 的命令集合。
func NewCommandToolsOpt(environmentID string, environmentWorkspace workspace.Workspace, terminalManager terminal.TerminalManager) *CommandToolsOpt {
	// 1. 创建命令工具共用的配置。
	return &CommandToolsOpt{
		EnvironmentID:   environmentID,
		Workspace:       environmentWorkspace,
		TerminalManager: terminalManager,
	}
}

// NewCommandTools 创建绑定同一环境和命令集合的五个命令工具。
func NewCommandTools(opt *CommandToolsOpt) ([]executor.Tool, error) {
	// 1. 校验环境 ID、工作区和命令集合。
	if opt == nil || opt.EnvironmentID == "" || opt.Workspace == nil || opt.TerminalManager == nil {
		return nil, errors.New("command tools require an environment ID, a workspace and a terminal manager")
	}
	// 2. 为五个工具生成参数定义，并绑定同一份配置。
	toolsOpt := *opt
	bash, err := executor.NewBaseTool[bashArguments](&executor.BaseToolOpt{
		Name:        "bash",
		Description: prompt.BashDescription,
	})
	if err != nil {
		return nil, err
	}
	readCommandOutput, err := executor.NewBaseTool[readCommandOutputArguments](&executor.BaseToolOpt{
		Name:        "read_command_output",
		Description: prompt.ReadCommandOutputDescription,
	})
	if err != nil {
		return nil, err
	}
	writeStdin, err := executor.NewBaseTool[writeStdinArguments](&executor.BaseToolOpt{
		Name:        "write_stdin",
		Description: prompt.WriteStdinDescription,
	})
	if err != nil {
		return nil, err
	}
	stopCommand, err := executor.NewBaseTool[stopCommandArguments](&executor.BaseToolOpt{
		Name:        "stop_command",
		Description: prompt.StopCommandDescription,
	})
	if err != nil {
		return nil, err
	}
	listCommands, err := executor.NewBaseTool[listCommandsArguments](&executor.BaseToolOpt{
		Name:        "list_commands",
		Description: prompt.ListCommandsDescription,
	})
	if err != nil {
		return nil, err
	}
	return []executor.Tool{
		&bashToolImpl{
			BaseTool: bash,
			opt:      &toolsOpt,
		},
		&readCommandOutputToolImpl{
			BaseTool: readCommandOutput,
			opt:      &toolsOpt,
		},
		&writeStdinToolImpl{
			BaseTool: writeStdin,
			opt:      &toolsOpt,
		},
		&stopCommandToolImpl{
			BaseTool: stopCommand,
			opt:      &toolsOpt,
		},
		&listCommandsToolImpl{
			BaseTool: listCommands,
			opt:      &toolsOpt,
		},
	}, nil
}

// GetCommandToolNames 返回五个命令工具的名称。
func GetCommandToolNames() []string {
	// 1. 按创建顺序返回工具名称列表。
	return []string{"bash", "read_command_output", "write_stdin", "stop_command", "list_commands"}
}

// Execute 启动后台命令并读取首批输出。
func (t *bashToolImpl) Execute(ctx context.Context, call *defs.Item, progress chan<- *executor.ExecutionProgressEvent) *executor.ToolExecutionResult {
	// 1. 解析参数，把工作目录转成根目录内的相对路径。
	callID := call.ToolCall.CallID
	arguments, err := t.ParseArguments(call.ToolCall.Arguments)
	if err != nil {
		return newInvalidArgumentsResult(callID, err)
	}
	rootDirectory := t.opt.Workspace.GetRootDirectory()
	directory, err := normalizeBashDirectory(rootDirectory, arguments.CWD)
	if err != nil {
		return newInvalidArgumentsResult(callID, err)
	}
	// 2. 已取消的调用直接返回，否则启动命令；命令不随本次调用结束而停止。
	if err := ctx.Err(); err != nil {
		return encodeContextError(callID, err)
	}
	command, err := t.opt.TerminalManager.StartTerminal(ctx, &terminal.TerminalRequest{
		EnvironmentID: t.opt.EnvironmentID,
		Workspace:     t.opt.Workspace,
		Command:       arguments.Command,
		Directory:     filepath.Join(rootDirectory, directory),
		Interactive:   arguments.Interactive,
		Timeout:       time.Duration(arguments.TimeoutSeconds) * time.Second,
	})
	if err != nil {
		return newStartTerminalErrorResult(ctx, callID, err)
	}
	// 3. 在 wait_seconds 内转发输出；结束、到期或取消后读取当前结果。
	waitCommandOutput(ctx, callID, command, time.Now().Add(time.Duration(arguments.WaitSeconds)*time.Second), progress)
	// 4. 用不受取消影响的上下文从头读取第一页，不等待新输出，返回编号、状态和第一页输出。
	output, err := command.ReadOutput(context.WithoutCancel(ctx), 0, 0)
	if err != nil {
		panic(err)
	}
	return encodeCommandResult(callID, output)
}

// waitCommandOutput 在首次等待期间转发命令输出。
func waitCommandOutput(ctx context.Context, callID string, command terminal.Terminal, deadline time.Time, progress chan<- *executor.ExecutionProgressEvent) {
	// 1. 从头按页读取输出，读取因 ctx 结束失败时返回。
	var cursor uint64
	for {
		output, err := command.ReadOutput(ctx, cursor, time.Until(deadline))
		if err != nil {
			return
		}
		// 2. 有新文本时发送 terminal 进度。
		if output.Text != "" {
			content, _ := jsonutil.MarshalWithoutHTMLEscape(output.Text)
			select {
			case progress <- &executor.ExecutionProgressEvent{
				CallID:  callID,
				Stream:  executor.ExecutionStreamTypeTerminal,
				Content: content,
			}:
			case <-ctx.Done():
				return
			}
		}
		// 3. 更新续读位置；输出已读完或等待到期时返回。
		cursor = output.NextCursor
		isOutputComplete := output.Info.Status != terminal.TerminalStatusRunning && !output.HasMore
		if isOutputComplete || !time.Now().Before(deadline) {
			return
		}
	}
}

// Execute 读取本 Session、本环境命令的一页输出。
func (t *readCommandOutputToolImpl) Execute(ctx context.Context, call *defs.Item, _ chan<- *executor.ExecutionProgressEvent) *executor.ToolExecutionResult {
	// 1. 解析参数并查找命令，未找到时返回 command_not_found。
	callID := call.ToolCall.CallID
	arguments, err := t.ParseArguments(call.ToolCall.Arguments)
	if err != nil {
		return newInvalidArgumentsResult(callID, err)
	}
	command, err := t.opt.TerminalManager.GetTerminal(t.opt.EnvironmentID, arguments.CommandID)
	if err != nil {
		return newCommandNotFoundResult(callID, arguments.CommandID)
	}
	// 2. 从 cursor 读取一页，区分无效位置和调用取消，返回命令状态及输出。
	output, err := command.ReadOutput(ctx, arguments.Cursor, time.Duration(arguments.WaitSeconds)*time.Second)
	switch {
	case errors.Is(err, terminal.ErrInvalidCursor):
		return encodeToolResult(callID, nil, &defs.ToolError{
			Code:    "invalid_cursor",
			Message: err.Error(),
		})
	case err != nil:
		return encodeContextError(callID, err)
	}
	return encodeCommandResult(callID, output)
}

// Execute 写入交互命令的终端，返回实际写入字节数。
func (t *writeStdinToolImpl) Execute(ctx context.Context, call *defs.Item, _ chan<- *executor.ExecutionProgressEvent) *executor.ToolExecutionResult {
	// 1. 解析参数并查找本 Session、本环境的命令。
	callID := call.ToolCall.CallID
	arguments, err := t.ParseArguments(call.ToolCall.Arguments)
	if err != nil {
		return newInvalidArgumentsResult(callID, err)
	}
	command, err := t.opt.TerminalManager.GetTerminal(t.opt.EnvironmentID, arguments.CommandID)
	if err != nil {
		return newCommandNotFoundResult(callID, arguments.CommandID)
	}
	// 2. chars 非空时写入终端；缓冲写满时等待程序读取或命令结束。
	writtenBytes := 0
	if arguments.Chars != "" {
		writtenBytes, err = command.WriteInput(ctx, arguments.Chars)
	}
	switch {
	case errors.Is(err, terminal.ErrInputUnavailable):
		return encodeToolResult(callID, nil, &defs.ToolError{
			Code:    "stdin_unavailable",
			Message: fmt.Sprintf("command %s was not started with interactive=true", arguments.CommandID),
		})
	case errors.Is(err, terminal.ErrTerminalFinished):
		return encodeToolResult(callID, nil, &defs.ToolError{
			Code:    "command_finished",
			Message: fmt.Sprintf("command %s has finished and no longer accepts input", arguments.CommandID),
		})
	// 调用取消时返回 canceled；已开始的写入仍等程序读取或命令结束。
	case err != nil && ctx.Err() != nil:
		return encodeContextError(callID, ctx.Err())
	}
	// 3. 返回当前状态和实际字节数，部分写入失败时附带 command_input_failed。
	result := &writeStdinResult{
		CommandID:    arguments.CommandID,
		Status:       string(command.GetInfo().Status),
		WrittenBytes: writtenBytes,
	}
	if err != nil {
		return encodeToolResult(callID, result, &defs.ToolError{
			Code:    "command_input_failed",
			Message: fmt.Sprintf("wrote %d bytes: %v", writtenBytes, err),
		})
	}
	return encodeToolResult(callID, result, nil)
}

// Execute 停止命令进程组，等待收尾并返回最终状态。
func (t *stopCommandToolImpl) Execute(ctx context.Context, call *defs.Item, _ chan<- *executor.ExecutionProgressEvent) *executor.ToolExecutionResult {
	// 1. 解析参数并查找本 Session、本环境的命令。
	callID := call.ToolCall.CallID
	arguments, err := t.ParseArguments(call.ToolCall.Arguments)
	if err != nil {
		return newInvalidArgumentsResult(callID, err)
	}
	command, err := t.opt.TerminalManager.GetTerminal(t.opt.EnvironmentID, arguments.CommandID)
	if err != nil {
		return newCommandNotFoundResult(callID, arguments.CommandID)
	}
	// 2. 停止命令并等待结果；无法确认停止时返回 command_stop_failed，可再次请求停止。
	info, err := command.Stop(ctx)
	// 调用取消时返回 canceled，已经接受的停止请求继续执行。
	if err != nil && ctx.Err() != nil {
		return encodeContextError(callID, ctx.Err())
	}
	if err != nil {
		return encodeToolResult(callID, nil, &defs.ToolError{
			Code:    "command_stop_failed",
			Message: err.Error() + "; command termination was not confirmed",
		})
	}
	return encodeToolResult(callID, newStopCommandResult(info), nil)
}

// Execute 按启动顺序列出本环境尚未淘汰的命令。
func (t *listCommandsToolImpl) Execute(_ context.Context, call *defs.Item, _ chan<- *executor.ExecutionProgressEvent) *executor.ToolExecutionResult {
	// 1. 解析空参数对象。
	callID := call.ToolCall.CallID
	if _, err := t.ParseArguments(call.ToolCall.Arguments); err != nil {
		return newInvalidArgumentsResult(callID, err)
	}
	// 2. 按环境列出命令，整理状态后编码返回。
	commands := t.opt.TerminalManager.ListTerminals(t.opt.EnvironmentID)
	result := &listCommandsResult{Commands: make([]commandInfo, 0, len(commands))}
	for _, command := range commands {
		result.Commands = append(result.Commands, newCommandInfo(command.GetInfo()))
	}
	return encodeToolResult(callID, result, nil)
}
