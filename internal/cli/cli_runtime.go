package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/VaalaCat/openvaala/internal/config"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	storagefile "github.com/VaalaCat/openvaala/pkg/storage/file"
	storagesqlite "github.com/VaalaCat/openvaala/pkg/storage/sqlite"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/spf13/pflag"
)

const (
	// runtimeServerIdleTimeout 是 TUI、acp、app-server 和 codex 自动拉起的 serve 的空闲退出时间：没有连接且没有 Turn 在执行这么久后 serve 自己退出；
	// web 拉起的 serve 常驻。
	runtimeServerIdleTimeout = 10 * time.Second
	// runtimeServerStartTimeout 是客户端拉起 serve 后等待它就绪的最长时间。
	runtimeServerStartTimeout = 10 * time.Second
	// runtimeServerStartPollInterval 是等待 serve 就绪期间检查连接文件和 healthz 的间隔。
	runtimeServerStartPollInterval = time.Second
	// runtimeServerExecutableChangedNotice 是连上的本机 serve 不是由当前可执行文件启动时给用户的提示，acp、app-server、codex 和 web 写到 stderr，
	// TUI 显示在状态行。
	runtimeServerExecutableChangedNotice = "runtime server was started from a different openvaala executable; run openvaala serve stop to restart it with this one"
)

// addURLFlag 注册客户端连接 serve 的 --url 参数。
func addURLFlag(flags *pflag.FlagSet) {
	// 1. 注册连接地址参数；缺省时依次使用 client.url 和本机 serve。
	flags.String("url", "", "connect to a running runtime server (ws://host:port/api/v1/openvaala); default: client.url, then the local runtime server")
}

// getRuntimeConnection 选择客户端命令要连的 serve 地址和令牌；本机需要拉起 serve 时，idleTimeout 是它的空闲退出时间，0 表示常驻。
func getRuntimeConnection(ctx context.Context, args *AppArgs, idleTimeout time.Duration) (runtimeConnection, error) {
	// 1. 依次读取 --url、client.url；都为空时复用或拉起本机 serve，用连接文件里的地址和令牌。
	source, runtimeURL := runtimeConnectionSourceFlag, args.RuntimeURL
	if runtimeURL == "" {
		source, runtimeURL = runtimeConnectionSourceConfig, args.Config.Client.URL
	}
	if runtimeURL == "" {
		file, err := EnsureRuntimeServer(ctx, &RuntimeServerStartOpt{Executable: args.Executable, ConfigFlags: args.ConfigFlags, IdleTimeout: idleTimeout, LogFile: args.Config.Telemetry.LogFile})
		if err != nil {
			return runtimeConnection{}, err
		}
		return runtimeConnection{
			URL:                   file.URL,
			Token:                 file.Token,
			Source:                runtimeConnectionSourceLocal,
			LocalServerID:         file.ServerID,
			IsFromOtherExecutable: !isRuntimeServerFromCurrentExecutable(ctx, file),
		}, nil
	}
	// 2. 有显式地址时直接使用它；读取本机连接文件拿 server_id，读取失败时记录警告，连接按远端处理。
	file, found, err := getRuntimeServerFile()
	if err != nil {
		telemetry.GetLogger(ctx).Warn("cannot read the runtime server file; treating the runtime server as a remote one", "error", err)
	}
	connection := runtimeConnection{
		URL:           runtimeURL,
		Source:        source,
		LocalServerID: file.ServerID,
	}
	// 3. URL 带 token 参数时以它为准（空值也算）；不带时用 client.token，仍为空且地址指向本机 serve 时用连接文件里的 token。
	if !isTokenInURL(runtimeURL) {
		connection.Token = args.Config.Client.Token
		if connection.Token == "" && found && isSameRuntimeServer(runtimeURL, file.URL) {
			connection.Token = file.Token
		}
	}
	return connection, nil
}

// RuntimeServerStartOpt 保存自动启动本机 serve 使用的程序、参数、空闲时长和日志位置。
type RuntimeServerStartOpt struct {
	Executable  string
	ConfigFlags []string
	IdleTimeout time.Duration
	LogFile     string
}

// EnsureRuntimeServer 复用能访问的本机 serve，没有时在后台拉起并等待就绪；空闲时长为 0 时新进程常驻。
func EnsureRuntimeServer(ctx context.Context, opt *RuntimeServerStartOpt) (connection RuntimeServerConnection, err error) {
	// 1. 校验启动参数并读取本机连接文件，serve 能访问时直接返回。
	if opt == nil {
		return RuntimeServerConnection{}, errors.New("runtime server startup options are required")
	}
	file, isReachable, err := getReachableRuntimeServerFile(ctx)
	if err != nil {
		return RuntimeServerConnection{}, err
	}
	if isReachable {
		return file, nil
	}
	// 2. 在后台启动 serve，设定等待期限。
	exited, err := startRuntimeServer(opt)
	if err != nil {
		return RuntimeServerConnection{}, err
	}
	deadline := time.Now().Add(runtimeServerStartTimeout)
	// 3. 每秒或启动进程退出时检查一次连接文件，serve 能访问就返回（可能是别的进程拉起的）；
	// 启动进程已退出且没有进程持有数据目录锁时返回退出错误，过了期限返回超时错误。
	for {
		var exitErr error
		hasExited := false
		select {
		case <-ctx.Done():
			return RuntimeServerConnection{}, ctx.Err()
		case exitErr = <-exited:
			hasExited = true
		case <-time.After(runtimeServerStartPollInterval):
		}
		file, isReachable, err := getReachableRuntimeServerFile(ctx)
		if err != nil {
			return RuntimeServerConnection{}, err
		}
		if isReachable {
			return file, nil
		}
		if hasExited && !isDataDirectoryLocked() {
			status := "exit status 0"
			if exitErr != nil {
				status = exitErr.Error()
			}
			return RuntimeServerConnection{}, fmt.Errorf("runtime server exited (%s); run `openvaala serve` to see the error", status)
		}
		if !time.Now().Before(deadline) {
			if opt.LogFile == "" {
				return RuntimeServerConnection{}, fmt.Errorf("runtime server did not become ready within %s; run `openvaala serve` to see the error", runtimeServerStartTimeout)
			}
			return RuntimeServerConnection{}, fmt.Errorf("runtime server did not become ready within %s; see %s", runtimeServerStartTimeout, opt.LogFile)
		}
	}
}

// getReachableRuntimeServerFile 读取本机连接文件，返回文件内容和它指向的 serve 能否访问。
func getReachableRuntimeServerFile(ctx context.Context) (file RuntimeServerConnection, isReachable bool, err error) {
	// 1. 读取连接文件；读取或解析失败时返回错误，文件不存在或 serve 无法访问时返回 false。
	file, found, err := getRuntimeServerFile()
	if err != nil || !found {
		return RuntimeServerConnection{}, false, err
	}
	return file, isRuntimeServerReachable(ctx, file), nil
}

// startRuntimeServer 在后台启动 serve 进程，返回接收进程退出结果的通道；IdleTimeout 为 0 时常驻。
func startRuntimeServer(opt *RuntimeServerStartOpt) (exited <-chan error, err error) {
	// 1. 检查可执行文件路径，创建并启动 serve 命令。
	if opt.Executable == "" {
		return nil, errors.New("cannot start a runtime server: the openvaala executable is unknown")
	}
	command, err := newRuntimeServerCommand(opt)
	if err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start runtime server: %w", err)
	}
	// 2. 起一个 goroutine 等 serve 退出，把结果写进容量为 1 的通道，通道不关闭；这个 goroutine 随 serve 或客户端进程结束，调用方不等它。
	exitedChannel := make(chan error, 1)
	go func() { exitedChannel <- command.Wait() }()
	return exitedChannel, nil
}

// newRuntimeServerCommand 创建客户端在后台拉起 serve 的命令；IdleTimeout 大于 0 时带上 --idle-timeout，0 时不带，serve 常驻到 serve stop。
func newRuntimeServerCommand(opt *RuntimeServerStartOpt) (*exec.Cmd, error) {
	// 1. 读取用户主目录并整理配置环境变量。
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("locate user directory: %w", err)
	}
	environment, err := getRuntimeServerEnvironment(os.Environ())
	if err != nil {
		return nil, err
	}
	// 2. 带上配置参数和 --idle-timeout（为 0 时不带），工作目录设为主目录，放进独立的会话组；标准流不设置，即 /dev/null。
	serveArgs := append(slices.Clone(opt.ConfigFlags), "serve")
	if opt.IdleTimeout > 0 {
		serveArgs = append(serveArgs, "--idle-timeout", opt.IdleTimeout.String())
	}
	command := exec.Command(opt.Executable, serveArgs...)
	command.Dir = home
	command.Env = environment
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return command, nil
}

// startDetachedRun 在不随 ctx 取消的 context 里启动 run，返回停止函数。
func startDetachedRun(ctx context.Context, run func(context.Context) error) (stop func() error) {
	// 1. 创建保留 ctx 的值、但不随它取消的 context，启动 run。
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		runErr = run(runCtx)
	}()
	// 2. 返回停止函数：取消 run 并等它结束，返回它的错误，context.Canceled 不算错误。
	return func() error {
		cancel()
		<-done
		if errors.Is(runErr, context.Canceled) {
			return nil
		}
		return runErr
	}
}

// newRuntime 读取 Runtime 定义、打开存储并创建 Runtime，同时返回关闭存储的函数。
func newRuntime(ctx context.Context, cfg *config.Config, tel *telemetry.Telemetry) (runtime.Runtime, func() error, error) {
	// 1. 加载 Runtime 定义。
	definitions, err := cfg.GetRuntimeDefinitions(ctx)
	if err != nil {
		return nil, nil, err
	}
	// 2. 创建数据目录并打开存储。
	dataDirectory, err := ensureDataDirectory()
	if err != nil {
		return nil, nil, err
	}
	storageOpt, closeStorage, err := openStorage(dataDirectory)
	if err != nil {
		return nil, nil, err
	}
	// 3. 带上遥测和图片大小上限创建 Runtime；创建失败时关闭存储并返回错误。
	opt := runtime.NewRuntimeOpt(storageOpt.Records, storageOpt.Logs, definitions)
	opt.Telemetry = tel
	opt.MaxImageBytes = cfg.Tools.MaxImageKB << 10
	rt, err := runtime.NewRuntime(opt)
	if err != nil {
		return nil, nil, errors.Join(err, closeStorage())
	}
	return rt, closeStorage, nil
}

// ensureDataDirectory 创建并返回本机数据目录。
func ensureDataDirectory() (string, error) {
	// 1. 读取用户主目录，以 0700 创建 ~/.local/share/openvaala。
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate user directory: %w", err)
	}
	dataDirectory := filepath.Join(home, ".local", "share", "openvaala")
	if err := os.MkdirAll(dataDirectory, 0700); err != nil {
		return "", fmt.Errorf("create data directory: %w", err)
	}
	return dataDirectory, nil
}

// lockDataDirectory 独占锁定数据目录，返回释放锁的函数。
func lockDataDirectory(dataDirectory string) (unlock func() error, err error) {
	// 1. 以 0600 打开 runtime.lock。
	lockFile, err := os.OpenFile(filepath.Join(dataDirectory, "runtime.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open data directory lock: %w", err)
	}
	// 2. 不等待地尝试独占锁；失败时关闭文件，锁被别的进程占着时提示先 serve stop。
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		closeErr := lockFile.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another openvaala process is using %s; run openvaala serve stop first", dataDirectory)
		}
		return nil, errors.Join(fmt.Errorf("lock data directory: %w", err), closeErr)
	}
	// 3. 返回关闭锁文件的函数，关闭即释放锁。
	return lockFile.Close, nil
}

// isDataDirectoryLocked 是否有其他进程持有数据目录锁？
func isDataDirectoryLocked() bool {
	// 1. 定位并创建数据目录；失败时返回 false。
	dataDirectory, err := ensureDataDirectory()
	if err != nil {
		return false
	}
	// 2. 尝试取得锁；失败时返回 true，成功时释放锁并返回 false。
	unlock, err := lockDataDirectory(dataDirectory)
	if err != nil {
		return true
	}
	_ = unlock()
	return false
}

// openStorage 打开元数据索引和对话日志，返回 DAO 配置和关闭索引的函数。
func openStorage(dataDirectory string) (*dao.DAOOpt, func() error, error) {
	// 1. 打开 metadata.db。
	database, err := storagesqlite.OpenDatabase(
		storagesqlite.NewDatabaseOpt(filepath.Join(dataDirectory, "metadata.db"), dao.RecordNamespaces()...),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("open metadata index: %w", err)
	}
	// 2. 打开 conversations 日志目录；失败时关闭索引。
	logs, err := storagefile.NewLogStorage(
		storagefile.NewLogStorageOpt(filepath.Join(dataDirectory, "conversations")),
	)
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("open conversation log: %w", err), database.Close())
	}
	return dao.NewDAOOpt(database.Record(), logs), database.Close, nil
}
