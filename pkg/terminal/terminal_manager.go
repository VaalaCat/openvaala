// Package terminal 管后台命令和交互终端的进程、输出、输入、时限和容量，不认识模型、JSON 和对话。
package terminal

import (
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/VaalaCat/openvaala/pkg/utils/syncmap"
)

// TerminalManager 是一组命令的集合：启动、查找、容量与淘汰；创建它的 Session 或 Runtime 结束时停止并等待全部命令。
type TerminalManager interface {
	// Run 阻塞到 ctx 结束；之后不再接受新命令，停止并等待全部 Terminal 后返回。只能调用一次。
	Run(ctx context.Context) error
	// StartTerminal 启动一条命令；进程启动成功才返回 Terminal，失败时不返回编号。命令不随 ctx 取消。
	// request.OwnsWorkspace 为 true 时，容量已满、管理器已停止、启动失败也会关闭工作区。
	StartTerminal(ctx context.Context, request *TerminalRequest) (Terminal, error)
	// GetTerminal 按编号查 environmentID 上启动成功、还没淘汰的命令；否则返回 ErrTerminalNotFound。
	GetTerminal(environmentID, terminalID string) (Terminal, error)
	// ListTerminals 按启动先后返回 environmentID 上启动成功、还没淘汰的命令。
	ListTerminals(environmentID string) []Terminal
	// GetTerminalByID 按编号查启动成功、还没淘汰的命令；否则返回 ErrTerminalNotFound。
	GetTerminalByID(terminalID string) (result Terminal, err error)
	// ListAllTerminals 按启动先后返回启动成功、还没淘汰的全部命令。
	ListAllTerminals() (terminals []Terminal)
	// IsBusy 是否还有没结束的命令？正在启动的也算。
	IsBusy() bool
}

// terminalManagerImpl 保存一组登记的全部 Terminal，不用事件循环：查询直接读登记表，容量上限按 Best-Effort 检查，
// 同一批并发启动的命令可能同时通过检查。
type terminalManagerImpl struct {
	opt *TerminalManagerOpt
	// terminals 是已登记的 Terminal（含正在启动和启动失败的），以编号为键；查询直接读。
	terminals syncmap.SyncMap[string, *terminalImpl]
	// stopping 在 Run 的 ctx 结束时关闭：之后不再登记新命令，各 Terminal 据此停止进程。
	stopping chan struct{}
	// tasks 是各 Terminal 的 run，Run 返回前等待。
	tasks sync.WaitGroup
}

// TerminalManagerOpt 是命令容量和结束通知配置。
type TerminalManagerOpt struct {
	// MaxTerminals 是最多保留的命令数，所有环境合计；正在启动、启动失败和已结束未淘汰的都算。
	MaxTerminals int
	// MaxOutputBytes 是每条命令保留的输出末尾。
	MaxOutputBytes int
	// TerminalFinished 在命令结束后收到一次唤醒，通道已有未处理的唤醒时合并；为 nil 时不通知。
	TerminalFinished chan<- struct{}
}

// NewTerminalManagerOpt 创建命令容量和输出保留量的默认配置。
func NewTerminalManagerOpt() *TerminalManagerOpt {
	// 1. 设置最多保留 64 条命令，每条保留末尾 1 MiB 输出。
	return &TerminalManagerOpt{
		MaxTerminals:   64,
		MaxOutputBytes: 1 << 20,
	}
}

// WithTerminalFinished 设置命令结束时的唤醒通道并返回本配置。
func (o *TerminalManagerOpt) WithTerminalFinished(terminalFinished chan<- struct{}) *TerminalManagerOpt {
	// 1. 保存唤醒通道。
	o.TerminalFinished = terminalFinished
	return o
}

// NewTerminalManager 校验并保存命令管理配置。
func NewTerminalManager(opt *TerminalManagerOpt) (TerminalManager, error) {
	// 1. 检查容量和输出上限为正，复制配置并创建停止信号。
	if opt == nil || opt.MaxTerminals <= 0 || opt.MaxOutputBytes <= 0 {
		return nil, errors.New("terminal manager requires positive MaxTerminals and MaxOutputBytes")
	}
	managerOpt := *opt
	return &terminalManagerImpl{
		opt:      &managerOpt,
		stopping: make(chan struct{}),
	}, nil
}

// Run 等待 ctx 结束，再停止并等待全部命令。
func (m *terminalManagerImpl) Run(ctx context.Context) error {
	// 1. 等待 ctx 结束，关闭停止信号，不再登记新命令。
	<-ctx.Done()
	close(m.stopping)
	// 2. 等待全部命令任务结束后返回 nil；单条停止失败由 Terminal 记警告。
	m.tasks.Wait()
	return nil
}

// StartTerminal 登记并启动命令，等待启动结果。
func (m *terminalManagerImpl) StartTerminal(ctx context.Context, request *TerminalRequest) (Terminal, error) {
	// 1. 拒绝已停止的管理器，容量已满时尝试淘汰最早结束的命令；被拒绝时关闭独占的工作区。
	if isChannelClosed(m.stopping) {
		request.CloseWorkspace(ctx)
		return nil, ErrTerminalManagerStopped
	}
	if err := m.evictFinishedTerminal(); err != nil {
		request.CloseWorkspace(ctx)
		return nil, err
	}
	// 2. 登记命令并启动任务；命令不随本次调用的 ctx 取消。
	terminal := newTerminal(newTerminalID(), request, m.opt)
	m.terminals.Store(terminal.id, terminal)
	m.tasks.Go(func() { terminal.run(context.WithoutCancel(ctx), m.stopping) })
	// 3. 等待启动结果；ctx 先结束时返回错误，命令仍可在启动成功后从列表找回。
	select {
	case <-terminal.started:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if terminal.startErr != nil {
		return nil, terminal.startErr
	}
	return terminal, nil
}

// GetTerminal 按环境和编号查询命令。
func (m *terminalManagerImpl) GetTerminal(environmentID, terminalID string) (Terminal, error) {
	// 1. 按编号查，要求命令属于指定环境，否则返回 ErrTerminalNotFound。
	terminal, err := m.GetTerminalByID(terminalID)
	if err != nil {
		return nil, err
	}
	if terminal.GetInfo().EnvironmentID != environmentID {
		return nil, ErrTerminalNotFound
	}
	return terminal, nil
}

// ListTerminals 按启动先后返回指定环境的命令。
func (m *terminalManagerImpl) ListTerminals(environmentID string) []Terminal {
	// 1. 从全部已启动的命令里筛选属于指定环境的。
	var terminals []Terminal
	for _, terminal := range m.ListAllTerminals() {
		if terminal.GetInfo().EnvironmentID == environmentID {
			terminals = append(terminals, terminal)
		}
	}
	return terminals
}

// GetTerminalByID 按编号查询命令。
func (m *terminalManagerImpl) GetTerminalByID(terminalID string) (result Terminal, err error) {
	// 1. 查登记表，要求命令已成功启动且未淘汰，否则返回 ErrTerminalNotFound。
	terminal, found := m.terminals.Load(terminalID)
	if !found || !terminal.isStartSucceeded() {
		return nil, ErrTerminalNotFound
	}
	return terminal, nil
}

// ListAllTerminals 按启动先后返回全部命令。
func (m *terminalManagerImpl) ListAllTerminals() (terminals []Terminal) {
	// 1. 筛选已成功启动的命令。
	var startedTerminals []*terminalImpl
	for _, terminal := range m.terminals.Values() {
		if terminal.isStartSucceeded() {
			startedTerminals = append(startedTerminals, terminal)
		}
	}
	// 2. 按登记时间排序并返回 Terminal 列表。
	slices.SortFunc(startedTerminals, func(a, b *terminalImpl) int { return a.createdAt.Compare(b.createdAt) })
	terminals = make([]Terminal, 0, len(startedTerminals))
	for _, terminal := range startedTerminals {
		terminals = append(terminals, terminal)
	}
	return terminals
}

// IsBusy 是否还有没结束的命令？正在启动的也算。
func (m *terminalManagerImpl) IsBusy() bool {
	// 1. 检查登记表中是否有正在启动或尚未结束的命令，即结束信号尚未关闭的记录。
	return slices.ContainsFunc(m.terminals.Values(), func(terminal *terminalImpl) bool {
		return !isChannelClosed(terminal.finished)
	})
}

// evictFinishedTerminal 淘汰已结束的命令。
func (m *terminalManagerImpl) evictFinishedTerminal() error {
	// 1. 尚有容量时直接返回。
	if m.terminals.Len() < m.opt.MaxTerminals {
		return nil
	}
	// 2. 查找结束最早的记录，启动失败也算已结束。
	var earliestFinished *terminalImpl
	for _, terminal := range m.terminals.Values() {
		if !isChannelClosed(terminal.finished) {
			continue
		}
		if earliestFinished == nil || terminal.finishedAt.Before(earliestFinished.finishedAt) {
			earliestFinished = terminal
		}
	}
	// 3. 全部尚未结束时返回 ErrTerminalLimitReached，否则删除选中记录；并发重复删除不报错。
	if earliestFinished == nil {
		return ErrTerminalLimitReached
	}
	m.terminals.Delete(earliestFinished.id)
	return nil
}
