package terminal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/workspace"
)

// Terminal 是一次命令执行：持有进程、输出、PTY 输入、总时限和退出结果，可以跨 Turn 存活。
type Terminal interface {
	// GetInfo 返回命令信息；结束前只有启动时的字段和 running。
	GetInfo() TerminalInfo
	// ReadOutput 返回 cursor 之后的输出：已有就立即返回，否则等新输出、命令结束或 wait 到期；只取消这次等待，不影响命令。
	ReadOutput(ctx context.Context, cursor uint64, wait time.Duration) (TerminalOutput, error)
	// WriteInput 把 chars 原样写进 PTY，返回实际写入的字节数；非 PTY 命令返回 ErrInputUnavailable，命令已结束返回 ErrTerminalFinished。
	// PTY 里的程序不读输入且 chars 超出 PTY 缓冲时，一直等到程序读走或写入失败；ctx 结束时立即返回 ctx 错误，
	// 已开始的写入留在后台，到程序读走或命令结束关闭终端时结束。
	WriteInput(ctx context.Context, chars string) (int, error)
	// Resize 修改 PTY 行列数，和当前尺寸相同时不处理；非 PTY 命令返回 ErrInputUnavailable，命令已结束返回 ErrTerminalFinished。
	// 和 WriteInput 按调用顺序依次执行。
	Resize(ctx context.Context, rows, cols uint16) (err error)
	// ClearOutput 丢掉保留的输出，只留 terminalClearSequence；命令已结束返回 ErrTerminalFinished。
	ClearOutput(ctx context.Context) (err error)
	// Stop 终止进程组并等到收尾完成，返回最终信息；已结束时直接返回。调用方取消等待不撤回已接受的停止。
	Stop(ctx context.Context) (TerminalInfo, error)
}

// terminalImpl 保存一条命令的进程、输出缓冲和退出结果：run 的事件循环是输出、尺寸、停止和退出结果的唯一修改者。命令结束前 ReadOutput、Stop、
// ClearOutput 经 terminalEvents 交给它，结束后直接读只读数据；WriteInput、Resize 经 terminalWrites 交给写入循环。
type terminalImpl struct {
	id        string
	request   *TerminalRequest
	opt       *TerminalManagerOpt
	createdAt time.Time
	// started 在进程启动成功或失败后关闭，startErr、process 在关闭前写入。
	started  chan struct{}
	startErr error
	process  workspace.Process
	// terminalEvents 承载读取、停止、清除输出请求，以及各任务报告的事实，只由 run 的事件循环读取。
	terminalEvents chan terminalEvent
	// terminalWrites 承载 PTY 输入和改尺寸，WriteInput、Resize 是发送方，交互命令的 writeTerminal 是唯一接收方。
	terminalWrites chan terminalWrite
	// writesStopped 由 run 在等待任务前关闭，让 writeTerminal 退出。
	writesStopped chan struct{}
	// tasks 是 run 启动的读输出、等待进程、写入循环和后台 Terminate 任务，run 写最终信息前等待。
	tasks sync.WaitGroup

	// 以下只由事件循环修改；finished 关闭后只读。
	output terminalOutputBuffer
	// outputChanged 每追加一次输出就关闭并换新，唤醒等待读取的调用方。
	outputChanged chan struct{}
	// info 结束前是启动时的字段和 running，行列数随改尺寸更新；结束时写入最终状态、退出码、停止原因和错误。
	info TerminalInfo
	// finishedAt 由 time.Now 取得，带单调时钟读数；管理器按它淘汰。
	finishedAt time.Time
	finished   chan struct{}
	// openOutputStreams 是还没结束的输出流数。
	openOutputStreams int
	// outputReadErr 是第一个读取失败的输出流的错误。
	outputReadErr error
	// processExit 是 Process.Wait 的结果；进程还没退出时为 nil。
	processExit *terminalProcessExited
	// stopReason 是 Terminate 已确认成功的停止原因，结束时写进 info；为空表示没有确认过停止。
	stopReason TerminalStopReason
	// terminatingReason 是正在后台进行的 Terminate 的停止原因；为空表示没有，同一时间最多一个。
	terminatingReason TerminalStopReason
	// stopResults 是等正在进行的 Terminate 的 Stop 回复通道（容量 1），它返回时统一回复。
	stopResults []chan error
	// isManagerStopRequested 管理器是否已经停止、要求停止这条命令？
	isManagerStopRequested bool
}

const (
	// maxOutputPageBytes 是一次读取最多返回的输出字节数。
	maxOutputPageBytes = 64 << 10
	// terminalTerminateTimeout 是 Terminate 的等待上限：超过后关闭独占的工作区，打断卡住的 SSH 连接。
	terminalTerminateTimeout = 5 * time.Second
	// defaultTerminalRows、defaultTerminalCols 是请求没有指定行列数时 PTY 的尺寸。
	defaultTerminalRows = 24
	defaultTerminalCols = 80
)

// newTerminal 创建尚未启动的命令记录。
func newTerminal(id string, request *TerminalRequest, opt *TerminalManagerOpt) *terminalImpl {
	// 1. 保存请求和配置，记录创建时间并创建启动、事件、写入、输出变化和结束通道。
	createdAt := time.Now()
	return &terminalImpl{
		id:             id,
		request:        request,
		opt:            opt,
		createdAt:      createdAt,
		started:        make(chan struct{}),
		terminalEvents: make(chan terminalEvent),
		terminalWrites: make(chan terminalWrite),
		writesStopped:  make(chan struct{}),
		outputChanged:  make(chan struct{}),
		info:           newTerminalInfo(id, request, createdAt),
		finished:       make(chan struct{}),
	}
}

// run 启动命令并处理事件，收齐退出和输出结果后结束。
func (t *terminalImpl) run(ctx context.Context, stopping <-chan struct{}) {
	// 1. 启动进程；失败时关闭独占的工作区，写入失败信息并关闭结束信号，再公布启动结果。
	process, outputPipes, err := t.startProcess(ctx)
	t.process, t.startErr = process, err
	if err != nil {
		t.info.Status, t.info.Err = TerminalStatusFailed, err
		t.request.CloseWorkspace(ctx)
		t.finishedAt = time.Now()
		close(t.finished)
		close(t.started)
		return
	}
	close(t.started)

	// 2. 启动输出读取和进程等待任务；交互命令再启动写入循环；按请求配置总时限。
	t.openOutputStreams = len(outputPipes)
	for _, outputPipe := range outputPipes {
		t.tasks.Go(func() { readTerminalOutput(outputPipe, t.terminalEvents) })
	}
	t.tasks.Go(func() { t.waitProcess(outputPipes) })
	if t.request.Interactive {
		rows, cols := t.info.Rows, t.info.Cols
		t.tasks.Go(func() { t.writeTerminal(rows, cols) })
	}
	var timeout <-chan time.Time
	if t.request.Timeout > 0 {
		timer := time.NewTimer(t.request.Timeout)
		defer timer.Stop()
		timeout = timer.C
	}
	// 3. 串行处理读取、停止、清除输出、改尺寸和退出事件；管理器停止或超时时请求停止命令。
	for t.processExit == nil || t.openOutputStreams > 0 || t.terminatingReason != "" {
		select {
		case event := <-t.terminalEvents:
			switch event := event.(type) {
			case terminalOutputRead:
				t.handleOutputRead(event)
			case terminalOutputClosed:
				t.handleOutputClosed(event)
			case terminalProcessExited:
				t.processExit = &event
			case terminalReadRequested:
				t.handleReadRequested(event)
			case terminalStopRequested:
				t.handleStopRequested(ctx, event)
			case terminalClearRequested:
				t.handleClearRequested()
			case terminalResized:
				t.handleResized(event)
			case terminalStopFinished:
				t.handleStopFinished(ctx, event)
			default:
				panic(fmt.Sprintf("unsupported terminal event %T", event))
			}
		case <-stopping:
			stopping = nil
			t.handleStopRequested(ctx, terminalStopRequested{reason: TerminalStoppedByManager})
		case <-timeout:
			timeout = nil
			t.handleStopRequested(ctx, terminalStopRequested{reason: TerminalStoppedByTimeout})
		}
	}
	// 4. 等进程退出、输出流关闭和 Terminate 返回；通知写入循环退出，再等全部任务并关闭输出读端。
	close(t.writesStopped)
	t.tasks.Wait()
	closeTerminalOutputReaders(outputPipes)

	// 5. 优先记录进程等待错误，再记录输出错误，否则写退出码；记录已确认的停止原因。
	switch {
	case t.processExit.err != nil:
		t.info.Status = TerminalStatusFailed
		t.info.Err = fmt.Errorf("wait for command: %w; remote command termination was not confirmed", t.processExit.err)
	case t.outputReadErr != nil:
		t.info.Status = TerminalStatusFailed
		t.info.Err = fmt.Errorf("read command output: %w", t.outputReadErr)
	default:
		exitCode := t.processExit.exitCode
		t.info.Status, t.info.ExitCode = TerminalStatusExited, &exitCode
	}
	t.info.StopReason = t.stopReason

	// 6. 关闭独占的工作区，关闭结束信号，再唤醒管理器的创建者。
	t.request.CloseWorkspace(ctx)
	t.finishedAt = time.Now()
	close(t.finished)
	t.wakeTerminalFinished()
}

// handleOutputRead 追加输出并通知等待读取的调用方。
func (t *terminalImpl) handleOutputRead(event terminalOutputRead) {
	// 1. 追加输出，超限时丢弃最早字节，关闭旧通知通道并创建新通道。
	t.output.append(event.data, t.opt.MaxOutputBytes)
	close(t.outputChanged)
	t.outputChanged = make(chan struct{})
}

// handleOutputClosed 记录输出流关闭及读取错误。
func (t *terminalImpl) handleOutputClosed(event terminalOutputClosed) {
	// 1. 减少未关闭流数，保留首个非空读取错误。
	t.openOutputStreams--
	if t.outputReadErr == nil {
		t.outputReadErr = event.err
	}
}

// handleReadRequested 回复指定游标的一页输出。
func (t *terminalImpl) handleReadRequested(request terminalReadRequested) {
	// 1. 读取当前输出，连同当前输出变化信号和错误回复调用方。
	output, err := t.readOutput(request.cursor)
	request.reply <- terminalOutputPage{
		output:        output,
		outputChanged: t.outputChanged,
		err:           err,
	}
}

// handleStopRequested 接收停止请求并安排进程终止。
func (t *terminalImpl) handleStopRequested(ctx context.Context, request terminalStopRequested) {
	// 1. 记录管理器停止请求及需要回复的调用方。
	if request.reason == TerminalStoppedByManager {
		t.isManagerStopRequested = true
	}
	if request.stopResult != nil {
		t.stopResults = append(t.stopResults, request.stopResult)
	}
	// 2. 已退出或已确认停止时回复成功；已有终止任务时等待，否则启动终止任务。
	switch {
	case t.processExit != nil || t.stopReason != "":
		t.replyStopResults(nil)
	case t.terminatingReason == "":
		t.startTerminate(ctx, request.reason)
	}
}

// handleStopFinished 记录终止结果并回复停止调用方。
func (t *terminalImpl) handleStopFinished(ctx context.Context, event terminalStopFinished) {
	// 1. 清除正在终止的原因，成功时记录已确认停止原因，再回复等待的调用方。
	t.terminatingReason = ""
	if event.err == nil {
		t.stopReason = event.reason
	}
	t.replyStopResults(event.err)
	// 2. 其他终止失败且管理器已要求停止时，按管理器停止再终止一次。
	if event.err != nil && t.isManagerStopRequested && event.reason != TerminalStoppedByManager && t.processExit == nil {
		t.startTerminate(ctx, TerminalStoppedByManager)
	}
}

// handleClearRequested 丢掉保留的输出，只留清屏序列，并通知等待读取的调用方。
func (t *terminalImpl) handleClearRequested() {
	// 1. 清除缓冲，关闭旧通知通道并创建新通道。
	t.output.clear()
	close(t.outputChanged)
	t.outputChanged = make(chan struct{})
}

// handleResized 记录 PTY 已改成的行列数。
func (t *terminalImpl) handleResized(event terminalResized) {
	// 1. 更新命令信息里的行列数。
	t.info.Rows, t.info.Cols = event.rows, event.cols
}

// startTerminate 启动后台进程终止任务。
func (t *terminalImpl) startTerminate(ctx context.Context, reason TerminalStopReason) {
	// 1. 记录本次终止原因，并把 Terminate 加入任务组。
	t.terminatingReason = reason
	t.tasks.Go(func() { t.terminateProcess(ctx, reason) })
}

// replyStopResults 回复全部等待中的停止调用方。
func (t *terminalImpl) replyStopResults(err error) {
	// 1. 向每个回复通道写入结果，再清空等待列表。
	for _, stopResult := range t.stopResults {
		stopResult <- err
	}
	t.stopResults = nil
}

// terminateProcess 终止进程组并向事件循环报告结果。
func (t *terminalImpl) terminateProcess(ctx context.Context, reason TerminalStopReason) {
	// 1. 调用 Terminate；超过 terminalTerminateTimeout 还没返回时关闭独占的工作区，返回前等关闭完成。
	workspaceClosed := make(chan struct{})
	closeTimer := time.AfterFunc(terminalTerminateTimeout, func() {
		defer close(workspaceClosed)
		t.request.CloseWorkspace(ctx)
	})
	err := t.process.Terminate()
	if !closeTimer.Stop() {
		<-workspaceClosed
	}
	// 2. 超时或管理器停止发起的失败记警告；管理器停止或独占工作区的命令失败时再调用 Close 放手进程。
	if err != nil {
		logger := telemetry.GetLogger(ctx)
		if reason != TerminalStoppedByRequest {
			logger.Warn("terminate command failed", "command_id", t.id, "stop_reason", reason, "error", err)
		}
		if reason == TerminalStoppedByManager || t.request.OwnsWorkspace {
			if closeErr := t.process.Close(); closeErr != nil {
				logger.Warn("close command failed", "command_id", t.id, "error", closeErr)
			}
		}
	}
	// 3. 把停止原因和终止错误交给事件循环。
	t.terminalEvents <- terminalStopFinished{
		reason: reason,
		err:    err,
	}
}

// writeTerminal 依次处理 terminalWrites 里的输入和改尺寸，直到 writesStopped 关闭；rows、cols 是 PTY 当前行列数。
func (t *terminalImpl) writeTerminal(rows, cols uint16) {
	for {
		select {
		case write := <-t.terminalWrites:
			switch write := write.(type) {
			case terminalInputWrite:
				// 1. 等进程读取输入或写入失败，再回复实际写入字节数和错误。
				count, err := t.process.WriteTerminalInput(write.data)
				write.written <- terminalInputWritten{
					count: count,
					err:   err,
				}
			case terminalResizeWrite:
				// 2. 尺寸变化时修改 PTY，成功后先交给事件循环更新 info，再回复调用方。
				if write.rows != rows || write.cols != cols {
					if err := t.process.ResizeTerminal(write.rows, write.cols); err != nil {
						write.resized <- err
						continue
					}
					rows, cols = write.rows, write.cols
					select {
					case t.terminalEvents <- terminalResized{rows: rows, cols: cols}:
					case <-t.writesStopped:
					}
				}
				write.resized <- nil
			default:
				panic(fmt.Sprintf("unsupported terminal write %T", write))
			}
		case <-t.writesStopped:
			return
		}
	}
}

// waitProcess 等待进程结束并报告退出结果。
func (t *terminalImpl) waitProcess(outputPipes []*terminalOutputPipe) {
	// 1. 等 Wait 返回后关闭输出写端，再把退出码和错误交给事件循环。
	exitCode, err := t.process.Wait()
	closeTerminalOutputWriters(outputPipes)
	t.terminalEvents <- terminalProcessExited{
		exitCode: exitCode,
		err:      err,
	}
}

// readTerminalOutput 读取输出流并发送输出与关闭事件。
func readTerminalOutput(outputPipe *terminalOutputPipe, terminalEvents chan<- terminalEvent) {
	// 1. 创建读取缓冲和待补齐字符的字节区。
	buffer := make([]byte, 16*1024)
	var pending []byte
	for {
		// 2. 读取字节并发送完整 UTF-8 前缀；遇到非法字节或流结束时原样发送剩余字节。
		count, err := outputPipe.reader.Read(buffer)
		if count > 0 {
			pending = append(pending, buffer[:count]...)
		}
		if len(pending) > 0 {
			complete, incomplete, invalid := completeUTF8Prefix(pending)
			if invalid || err != nil || !incomplete {
				complete = len(pending)
			}
			if complete > 0 {
				terminalEvents <- terminalOutputRead{data: slices.Clone(pending[:complete])}
				pending = slices.Clone(pending[complete:])
			}
		}
		// 3. 流结束时把 EOF 视为正常关闭，发送一次关闭事件后返回。
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = nil
			}
			terminalEvents <- terminalOutputClosed{err: err}
			return
		}
	}
}

// GetInfo 返回命令信息。
func (t *terminalImpl) GetInfo() TerminalInfo {
	// 1. 已结束时返回最终信息，否则返回启动字段、初始行列数和 running 状态。
	if isChannelClosed(t.finished) {
		return t.info
	}
	return newTerminalInfo(t.id, t.request, t.createdAt)
}

// ReadOutput 读取指定游标后的输出，必要时等待变化。
func (t *terminalImpl) ReadOutput(ctx context.Context, cursor uint64, wait time.Duration) (TerminalOutput, error) {
	// 1. 创建本次等待时限。
	waitTimer := time.NewTimer(wait)
	defer waitTimer.Stop()
	for {
		// 2. 向事件循环请求一页；已结束时直接读，ctx 结束时只取消本次等待。
		reply := make(chan terminalOutputPage, 1)
		select {
		case t.terminalEvents <- terminalReadRequested{
			cursor: cursor,
			reply:  reply,
		}:
		case <-t.finished:
			return t.readOutput(cursor)
		case <-ctx.Done():
			return TerminalOutput{}, ctx.Err()
		}
		// 3. 有输出或错误时立即返回，否则等输出变化、命令结束、时限或 ctx 结束。
		page := <-reply
		if page.err != nil || page.output.Text != "" {
			return page.output, page.err
		}
		select {
		case <-page.outputChanged:
		case <-t.finished:
		case <-waitTimer.C:
			return page.output, nil
		case <-ctx.Done():
			return TerminalOutput{}, ctx.Err()
		}
	}
}

// WriteInput 把 PTY 输入交给写入循环并等待写入结果。
func (t *terminalImpl) WriteInput(ctx context.Context, chars string) (int, error) {
	// 1. 非交互命令返回 ErrInputUnavailable，已结束命令返回 ErrTerminalFinished。
	if !t.request.Interactive {
		return 0, ErrInputUnavailable
	}
	if isChannelClosed(t.finished) {
		return 0, ErrTerminalFinished
	}
	// 2. 向写入循环提交原始输入字节，结束或取消时返回对应错误。
	inputWritten := make(chan terminalInputWritten, 1)
	select {
	case t.terminalWrites <- terminalInputWrite{
		data:    []byte(chars),
		written: inputWritten,
	}:
	case <-t.finished:
		return 0, ErrTerminalFinished
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	// 3. 等待写入结果；ctx 结束时返回错误，已开始的写入继续留在写入循环里。
	select {
	case written := <-inputWritten:
		return written.count, written.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// Resize 把改尺寸请求交给写入循环并等待结果。
func (t *terminalImpl) Resize(ctx context.Context, rows, cols uint16) (err error) {
	// 1. 非交互命令返回 ErrInputUnavailable，已结束命令返回 ErrTerminalFinished。
	if !t.request.Interactive {
		return ErrInputUnavailable
	}
	if isChannelClosed(t.finished) {
		return ErrTerminalFinished
	}
	// 2. 向写入循环提交新行列数，结束或取消时返回对应错误。
	resized := make(chan error, 1)
	select {
	case t.terminalWrites <- terminalResizeWrite{
		rows:    rows,
		cols:    cols,
		resized: resized,
	}:
	case <-t.finished:
		return ErrTerminalFinished
	case <-ctx.Done():
		return ctx.Err()
	}
	// 3. 等待修改结果；ctx 结束时返回错误，已提交的修改继续留在写入循环里。
	select {
	case err = <-resized:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ClearOutput 请求事件循环丢掉保留的输出。
func (t *terminalImpl) ClearOutput(ctx context.Context) (err error) {
	// 1. 向事件循环提交清除请求；事件循环按收到的顺序处理，之后的读取都看到清除结果。
	select {
	case t.terminalEvents <- terminalClearRequested{}:
		return nil
	case <-t.finished:
		return ErrTerminalFinished
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop 请求终止命令并等待最终结果。
func (t *terminalImpl) Stop(ctx context.Context) (TerminalInfo, error) {
	// 1. 向事件循环提交停止请求；已结束时返回最终信息，ctx 结束时返回当前信息和错误。
	stopResult := make(chan error, 1)
	select {
	case t.terminalEvents <- terminalStopRequested{
		reason:     TerminalStoppedByRequest,
		stopResult: stopResult,
	}:
	case <-t.finished:
		return t.info, nil
	case <-ctx.Done():
		return t.GetInfo(), ctx.Err()
	}
	// 2. 等 Terminate 结果；失败时返回错误和当前信息，命令继续受管。
	select {
	case err := <-stopResult:
		if err != nil {
			return t.GetInfo(), err
		}
	case <-ctx.Done():
		return t.GetInfo(), ctx.Err()
	}
	// 3. 终止成功后等命令收尾；ctx 结束只取消等待，不撤回停止请求。
	select {
	case <-t.finished:
		return t.info, nil
	case <-ctx.Done():
		return t.GetInfo(), ctx.Err()
	}
}

// readOutput 读取一页输出并附上同一时刻的命令信息。
func (t *terminalImpl) readOutput(cursor uint64) (TerminalOutput, error) {
	// 1. 从缓冲读取最多 64 KiB，游标错误直接返回，否则组装页面、已产生的输出总量和命令信息。
	text, nextCursor, hasMore, truncated, err := t.output.read(cursor, maxOutputPageBytes)
	if err != nil {
		return TerminalOutput{}, err
	}
	return TerminalOutput{
		Info:       t.info,
		Text:       text,
		NextCursor: nextCursor,
		EndCursor:  t.output.getEndOffset(),
		HasMore:    hasMore,
		Truncated:  truncated,
	}, nil
}

// wakeTerminalFinished 向管理器的创建者发一次命令结束的唤醒；没有配置通道或通道已有未处理的唤醒时跳过。
func (t *terminalImpl) wakeTerminalFinished() {
	// 1. 非阻塞发送，已有未处理的唤醒时合并。
	if t.opt.TerminalFinished == nil {
		return
	}
	select {
	case t.opt.TerminalFinished <- struct{}{}:
	default:
	}
}

// CloseWorkspace 关闭命令独占的工作区；不独占时不处理，可重复调用。
func (r *TerminalRequest) CloseWorkspace(ctx context.Context) {
	// 1. 独占的工作区才关闭，失败记 Warn。
	if !r.OwnsWorkspace {
		return
	}
	if err := r.Workspace.Close(); err != nil {
		telemetry.GetLogger(ctx).Warn("close terminal workspace failed", "environment_id", r.EnvironmentID, "error", err)
	}
}

// isStartSucceeded 进程是否已成功启动？
func (t *terminalImpl) isStartSucceeded() bool {
	// 1. 检查启动结果已公布且启动错误为空。
	return isChannelClosed(t.started) && t.startErr == nil
}

// startProcess 通过请求的工作区启动进程并连接输出管道。
func (t *terminalImpl) startProcess(ctx context.Context) (workspace.Process, []*terminalOutputPipe, error) {
	// 1. 组装命令、工作目录和初始行列数；交互命令使用一条输出流，普通命令分别连接 stdout 和 stderr。
	processRequest := &workspace.ProcessRequest{
		Command:   t.request.Command,
		Directory: t.request.Directory,
		TTY:       t.request.Interactive,
		Rows:      t.request.Rows,
		Cols:      t.request.Cols,
	}
	var outputPipes []*terminalOutputPipe
	if t.request.Interactive {
		outputPipes = []*terminalOutputPipe{newTerminalOutputPipe()}
		processRequest.Stdout = outputPipes[0].writer
	} else {
		outputPipes = []*terminalOutputPipe{newTerminalOutputPipe(), newTerminalOutputPipe()}
		processRequest.Stdout = outputPipes[0].writer
		processRequest.Stderr = outputPipes[1].writer
	}
	// 2. 由工作区检查目录并启动进程；失败时关闭管道两端。
	process, err := t.request.Workspace.StartProcess(ctx, processRequest)
	if err != nil {
		closeTerminalOutputWriters(outputPipes)
		closeTerminalOutputReaders(outputPipes)
		return nil, nil, err
	}
	return process, outputPipes, nil
}

// newTerminalOutputPipe 创建命令输出管道。
func newTerminalOutputPipe() *terminalOutputPipe {
	// 1. 创建管道并保存读写两端。
	reader, writer := io.Pipe()
	return &terminalOutputPipe{
		reader: reader,
		writer: writer,
	}
}

// closeTerminalOutputWriters 关闭全部命令输出写端。
func closeTerminalOutputWriters(outputPipes []*terminalOutputPipe) {
	// 1. 逐条关闭写端，忽略关闭错误。
	for _, outputPipe := range outputPipes {
		_ = outputPipe.writer.Close()
	}
}

// closeTerminalOutputReaders 关闭全部命令输出读端。
func closeTerminalOutputReaders(outputPipes []*terminalOutputPipe) {
	// 1. 逐条关闭读端，忽略关闭错误。
	for _, outputPipe := range outputPipes {
		_ = outputPipe.reader.Close()
	}
}

// append 追加输出并按容量裁剪最早的字节。
func (b *terminalOutputBuffer) append(data []byte, maxBytes int) {
	// 1. 追加字节，未超容量时直接返回。
	b.retained = append(b.retained, data...)
	overflow := len(b.retained) - maxBytes
	if overflow <= 0 {
		return
	}
	// 2. 从溢出位置向后最多查 UTFMax-1 字节，找到字符起点就从此保留，找不到时按字节丢弃。
	dropped := overflow
	for dropped < len(b.retained) && dropped-overflow < utf8.UTFMax-1 && !utf8.RuneStart(b.retained[dropped]) {
		dropped++
	}
	if dropped == len(b.retained) || !utf8.RuneStart(b.retained[dropped]) {
		dropped = overflow
	}
	// 3. 裁剪已丢弃部分，累计输出起始偏移，并记下溢出裁剪到的位置。
	b.retained = b.retained[dropped:]
	b.startOffset += uint64(dropped)
	b.overflowOffset = b.startOffset
}

// clear 丢掉保留的输出，只留清屏序列；overflowOffset 不变。
func (b *terminalOutputBuffer) clear() {
	// 1. 起始偏移移到已产生输出的末尾，保留内容换成清屏序列。
	b.startOffset = b.getEndOffset()
	b.retained = []byte(terminalClearSequence)
}

// getEndOffset 返回已产生的输出总字节数，包括已丢弃的部分。
func (b *terminalOutputBuffer) getEndOffset() uint64 {
	// 1. 起始偏移加上保留的字节数。
	return b.startOffset + uint64(len(b.retained))
}

// read 按游标读取缓冲中的一页。
func (b *terminalOutputBuffer) read(cursor uint64, maxBytes int) (text string, nextCursor uint64, hasMore, truncated bool, err error) {
	// 1. 计算总输出长度，游标超出时返回 ErrInvalidCursor。
	end := b.getEndOffset()
	if cursor > end {
		return "", 0, false, false, &invalidCursorError{
			cursor: cursor,
			end:    end,
		}
	}
	// 2. 游标已被丢弃时从最早保留处读取；只有被溢出裁剪丢弃的游标才标记 truncated，主动清除的不算。
	if cursor < b.startOffset {
		truncated = cursor < b.overflowOffset
		cursor = b.startOffset
	}
	// 3. 最多取 maxBytes，页尾回退至字符起点；非法 UTF-8 按字节切，返回下一游标和剩余标记。
	page := b.retained[cursor-b.startOffset:]
	if len(page) > maxBytes {
		page = page[:getPageEnd(page, maxBytes)]
	}
	nextCursor = cursor + uint64(len(page))
	return string(page), nextCursor, nextCursor < end, truncated, nil
}
