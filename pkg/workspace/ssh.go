package workspace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/pkg/sftp"
	"github.com/sourcegraph/conc"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// sshWorkspaceImpl 是远端机器上的工作区：并发调用共用一条连接，连接断开后由下一次调用重新拨号，Close 之后不再拨号。
type sshWorkspaceImpl struct {
	opt      *SSHWorkspaceOpt
	userName string
	hostPort string
	// logger 是本工作区连接事件的日志出口，带远端地址；连接的后台任务没有请求 ctx，统一使用它。
	logger *slog.Logger

	// connectionLock 保护 connection 和 closed。
	connectionLock sync.Mutex
	// connection 是当前的 SSH 连接；还没拨号或已经 Close 时为 nil。
	connection *sshConnection
	// closed 是否已经 Close？Close 之后的调用返回 net.ErrClosed。
	closed bool
}

// SSHWorkspaceOpt 是 SSH 工作区的远端根目录与连接参数。
type SSHWorkspaceOpt struct {
	// RootDirectory 是远端工作区根目录的绝对路径。
	RootDirectory string
	// Address 是 [user@]host[:port]；省略 user 时使用本机用户名，省略 port 时为 22。
	Address string
	// IdentityFile 是本机私钥路径，可为空。
	IdentityFile string
	// Telemetry 是遥测管道，用于连接建立、断开与保活失败的日志；nil 时使用全局兜底。
	Telemetry *telemetry.Telemetry
}

// NewSSHWorkspaceOpt 保存远端根目录、地址和私钥路径。
func NewSSHWorkspaceOpt(rootDirectory, address, identityFile string) *SSHWorkspaceOpt {
	// 1. 创建 SSH 工作区配置。
	return &SSHWorkspaceOpt{
		RootDirectory: rootDirectory,
		Address:       address,
		IdentityFile:  identityFile,
	}
}

// NewSSHWorkspace 校验根目录并解析 SSH 地址，保存工作区配置。
func NewSSHWorkspace(opt *SSHWorkspaceOpt) (Workspace, error) {
	// 1. 要求绝对根目录，解析用户名和端口并清理路径。
	if opt == nil || !filepath.IsAbs(opt.RootDirectory) {
		return nil, errors.New("ssh workspace requires an absolute root directory")
	}
	userName, hostPort, err := parseSSHAddress(opt.Address)
	if err != nil {
		return nil, err
	}
	cleaned := *opt
	cleaned.RootDirectory = filepath.Clean(opt.RootDirectory)
	return &sshWorkspaceImpl{
		opt:      &cleaned,
		userName: userName,
		hostPort: hostPort,
		logger:   newSSHLogger(opt.Telemetry, hostPort),
	}, nil
}

// GetRootDirectory 返回远端工作区根目录。
func (w *sshWorkspaceImpl) GetRootDirectory() string {
	// 1. 读取配置中的根目录。
	return w.opt.RootDirectory
}

// GetHomeDirectory 返回登录用户的 SFTP 初始目录。
func (w *sshWorkspaceImpl) GetHomeDirectory(ctx context.Context) (string, error) {
	// 1. 取得连接后查询 SFTP 当前目录。
	connection, err := w.getConnection(ctx)
	if err != nil {
		return "", err
	}
	return connection.files.Getwd()
}

// ReadFile 通过 SFTP 读取远端普通文件的一页内容。
func (w *sshWorkspaceImpl) ReadFile(ctx context.Context, path string, offset int64, limit int) (*FileContent, error) {
	// 1. 取得连接，检查目标类型并打开文件。
	connection, err := w.getConnection(ctx)
	if err != nil {
		return nil, err
	}
	info, err := connection.files.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, ErrNotRegularFile
	}
	file, err := connection.files.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	// 2. 从指定偏移读取一页并报告 EOF。
	return readFileContent(file, info.Size(), offset, limit)
}

// WriteFile 写入远端工作区内的完整文件。
func (w *sshWorkspaceImpl) WriteFile(ctx context.Context, path string, content []byte, mode fs.FileMode, createOnly bool) error {
	// 1. 检查目标在根目录内，取得连接后以临时文件提交写入。
	if _, err := getRelativePath(w.opt.RootDirectory, path); err != nil {
		return err
	}
	connection, err := w.getConnection(ctx)
	if err != nil {
		return err
	}
	return connection.writeFileAtomically(filepath.Clean(path), content, mode, createOnly)
}

// OpenFileReader 打开远端文件的读取端。
func (w *sshWorkspaceImpl) OpenFileReader(ctx context.Context, path string) (io.ReadCloser, error) {
	// 1. 取得连接，展开路径并打开文件，路径可以位于工作区外。
	connection, err := w.getConnection(ctx)
	if err != nil {
		return nil, err
	}
	file, err := connection.files.Open(getAbsolutePath(w.opt.RootDirectory, path))
	if err != nil {
		return nil, err
	}
	return file, nil
}

// CreateFileWriter 创建远端文件写入端并设置权限。
func (w *sshWorkspaceImpl) CreateFileWriter(ctx context.Context, path string, mode fs.FileMode) (io.WriteCloser, error) {
	// 1. 取得连接，展开路径、创建父目录并打开文件。
	connection, err := w.getConnection(ctx)
	if err != nil {
		return nil, err
	}
	target := getAbsolutePath(w.opt.RootDirectory, path)
	if err := connection.files.MkdirAll(filepath.Dir(target)); err != nil {
		return nil, err
	}
	file, err := connection.files.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return nil, err
	}
	// 2. 设置文件权限为 mode，失败时关闭文件。
	if err := file.Chmod(mode.Perm()); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// GetMetadata 通过 SFTP 读取路径元数据。
func (w *sshWorkspaceImpl) GetMetadata(ctx context.Context, path string, followSymlinks bool) (*FileMetadata, error) {
	// 1. 取得连接，按参数选择 Stat 或 Lstat 并转换结果。
	connection, err := w.getConnection(ctx)
	if err != nil {
		return nil, err
	}
	stat := connection.files.Lstat
	if followSymlinks {
		stat = connection.files.Stat
	}
	info, err := stat(path)
	if err != nil {
		return nil, err
	}
	return newFileMetadata(info), nil
}

// ReadDirectory 返回远端目录按名称排序的直接子项。
func (w *sshWorkspaceImpl) ReadDirectory(ctx context.Context, path string) ([]*DirectoryEntry, error) {
	// 1. 取得连接，读取 SFTP 目录并转换条目。
	connection, err := w.getConnection(ctx)
	if err != nil {
		return nil, err
	}
	infos, err := connection.files.ReadDir(path)
	if err != nil {
		return nil, err
	}
	entries := make([]fs.DirEntry, 0, len(infos))
	for _, info := range infos {
		entries = append(entries, fs.FileInfoToDirEntry(info))
	}
	return newDirectoryEntries(entries), nil
}

// CreateDirectory 在远端工作区内递归创建目录。
func (w *sshWorkspaceImpl) CreateDirectory(ctx context.Context, path string) error {
	// 1. 检查路径在根目录内，取得连接并创建目录。
	if _, err := getRelativePath(w.opt.RootDirectory, path); err != nil {
		return err
	}
	connection, err := w.getConnection(ctx)
	if err != nil {
		return err
	}
	return connection.files.MkdirAll(path)
}

// Remove 删除远端工作区内的文件或空目录。
func (w *sshWorkspaceImpl) Remove(ctx context.Context, path string) error {
	// 1. 检查路径在根目录内，取得连接并删除目标。
	if _, err := getRelativePath(w.opt.RootDirectory, path); err != nil {
		return err
	}
	connection, err := w.getConnection(ctx)
	if err != nil {
		return err
	}
	return connection.files.Remove(path)
}

// StartProcess 创建 SSH exec 会话运行远端 bash 命令。
func (w *sshWorkspaceImpl) StartProcess(ctx context.Context, request *ProcessRequest) (Process, error) {
	// 1. 拒绝 TTY 请求携带 Stdin，取得连接并检查工作目录。
	if request.TTY && request.Stdin != nil {
		return nil, ErrTTYStdinUnsupported
	}
	connection, err := w.getConnection(ctx)
	if err != nil {
		return nil, err
	}
	if info, err := connection.files.Stat(request.Directory); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("process directory %s is not a directory", request.Directory)
	}
	// 2. 生成远端进程组号文件名，打开 exec 会话并接入输出。
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return nil, err
	}
	pgidPath := "/tmp/openvaala-process-" + hex.EncodeToString(random) + ".pgid"
	session, err := connection.client.NewSession()
	if err != nil {
		return nil, err
	}
	session.Stdout = request.Stdout
	session.Stderr = request.Stderr
	process := &sshProcessImpl{
		connection: connection,
		session:    session,
		pgidPath:   pgidPath,
	}
	// 3. TTY 时按请求行列数申请 xterm-256color 终端，把会话的 StdinPipe 存为终端输入端；非 TTY 使用请求的 Stdin。
	if request.TTY {
		size := getTerminalSize(request.Rows, request.Cols)
		if err := session.RequestPty("xterm-256color", int(size.Rows), int(size.Cols), ssh.TerminalModes{}); err != nil {
			_ = session.Close()
			return nil, err
		}
		terminalInput, err := session.StdinPipe()
		if err != nil {
			_ = session.Close()
			return nil, err
		}
		process.terminalInput = terminalInput
	} else {
		session.Stdin = request.Stdin
	}
	// 4. 启动记录进程组号的远端命令，失败时关闭会话。
	if err := session.Start(getSSHProcessCommand(request, pgidPath)); err != nil {
		_ = session.Close()
		return nil, err
	}
	return process, nil
}

// Dial 通过 SSH 由远端建立网络连接并解析地址。
func (w *sshWorkspaceImpl) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	// 1. 取得连接，经 direct-tcpip 通道拨号。
	connection, err := w.getConnection(ctx)
	if err != nil {
		return nil, err
	}
	return connection.client.DialContext(ctx, network, address)
}

// GetHTTPProxy 总是返回 nil，远端请求直连。
func (w *sshWorkspaceImpl) GetHTTPProxy(*http.Request) (*url.URL, error) {
	// 1. 返回 nil。
	return nil, nil
}

// Close 关闭 SSH 工作区连接。
func (w *sshWorkspaceImpl) Close() error {
	// 1. 加锁标记工作区关闭并拒绝后续调用，再清除连接引用并关闭已有连接。
	w.connectionLock.Lock()
	defer w.connectionLock.Unlock()
	w.closed = true
	if w.connection == nil {
		return nil
	}
	connection := w.connection
	w.connection = nil
	return connection.close()
}

// getConnection 取得可用的 SSH 连接。
func (w *sshWorkspaceImpl) getConnection(ctx context.Context) (*sshConnection, error) {
	// 1. 检查取消并加锁，工作区关闭后返回 net.ErrClosed，可用连接直接复用。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	w.connectionLock.Lock()
	defer w.connectionLock.Unlock()
	if w.closed {
		return nil, net.ErrClosed
	}
	if w.connection != nil {
		if !w.connection.isClosed() {
			return w.connection, nil
		}
		// 2. 关闭断开的旧连接和 SFTP 会话，等待它的后台任务退出。
		w.logger.Warn("ssh connection lost, redialing")
		_ = w.connection.close()
		w.connection = nil
	}
	// 3. 重新拨号并保存连接。
	connection, err := dialSSHConnection(ctx, w.logger, w.userName, w.hostPort, w.opt.IdentityFile)
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", w.opt.Address, err)
	}
	w.connection = connection
	return connection, nil
}

const (
	// sshDialTimeout 限制拨号、SSH 握手与打开 SFTP 的总时长。
	sshDialTimeout = 15 * time.Second
	// sshKeepAliveInterval 是 keepalive 的发送间隔，也是等待响应的上限。
	sshKeepAliveInterval = 15 * time.Second
)

// sshConnection 是一条已认证的 SSH 连接和它上面的 SFTP 会话。
type sshConnection struct {
	client *ssh.Client
	files  *sftp.Client
	// done 在连接断开后关闭。
	done chan struct{}
	// tasks 是监视连接和 SFTP 会话结束、发送 keepalive 的后台任务，连接断开后全部退出。
	tasks conc.WaitGroup
	// logger 带远端地址，后台任务统一用它记录连接事件。
	logger *slog.Logger
}

// dialSSHConnection 建立认证后的 SSH 和 SFTP 连接，并启动连接监视及保活。
func dialSSHConnection(ctx context.Context, logger *slog.Logger, userName, hostPort, identityFile string) (*sshConnection, error) {
	// 1. 设置拨号总超时，收集认证密钥并建立 TCP 连接。
	dialContext, cancel := context.WithTimeout(ctx, sshDialTimeout)
	defer cancel()
	signers, closeAgent, err := getSSHSigners(dialContext, identityFile)
	if err != nil {
		return nil, err
	}
	defer closeAgent()
	networkConnection, err := (&net.Dialer{}).DialContext(dialContext, "tcp", hostPort)
	if err != nil {
		return nil, err
	}
	// 2. 拨号上下文结束时关闭连接，中断握手和 SFTP 初始化，返回前等待取消回调。
	interrupted := make(chan struct{})
	stopInterruptingDial := context.AfterFunc(dialContext, func() {
		_ = networkConnection.Close()
		close(interrupted)
	})
	defer func() {
		if !stopInterruptingDial() {
			<-interrupted
		}
	}()
	clientConnection, channels, requests, err := ssh.NewClientConn(networkConnection, hostPort, &ssh.ClientConfig{
		User:            userName,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signers...)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		_ = networkConnection.Close()
		return nil, getSSHDialError(ctx, err)
	}
	client := ssh.NewClient(clientConnection, channels, requests)
	files, err := sftp.NewClient(client)
	if err != nil {
		_ = client.Close()
		return nil, getSSHDialError(ctx, err)
	}
	// 3. 握手和 SFTP 初始化成功后启动连接、SFTP 和保活任务。
	connection := &sshConnection{
		client: client,
		files:  files,
		done:   make(chan struct{}),
		logger: logger,
	}
	connection.tasks.Go(func() {
		// 连接结束时记录关闭原因并关闭 done，正常 Close 也会记录。
		connection.logger.Info("ssh connection closed", "reason", client.Wait())
		close(connection.done)
	})
	connection.tasks.Go(func() {
		_ = files.Wait()
		// SFTP 单独结束时关闭 SSH 连接，下一次调用重新连接。
		_ = client.Close()
	})
	connection.tasks.Go(connection.keepAlive)
	connection.logger.Info("ssh connected")
	return connection, nil
}

// keepAlive 定期发送 SSH 保活并检查响应。
func (c *sshConnection) keepAlive() {
	// 1. 按间隔等待连接结束或保活时刻。
	ticker := time.NewTicker(sshKeepAliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
		}
		// 2. 发送保活并等待响应；请求失败或一个间隔内无响应时记 Warn 后关闭连接。
		replied := make(chan error, 1)
		c.tasks.Go(func() {
			_, _, err := c.client.SendRequest("keepalive@openssh.com", true, nil)
			replied <- err
		})
		select {
		case <-c.done:
			return
		case err := <-replied:
			if err == nil {
				continue
			}
			c.logger.Warn("ssh keepalive failed", "error", err)
		case <-time.After(sshKeepAliveInterval):
			c.logger.Warn("ssh keepalive failed", "error", "no reply within the keepalive interval")
		}
		_ = c.client.Close()
		return
	}
}

// isClosed SSH 连接是否已经断开？
func (c *sshConnection) isClosed() bool {
	// 1. 检查 done 是否关闭。
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// close 关闭 SSH 工作区的连接资源。
func (c *sshConnection) close() error {
	// 1. 先关闭底层连接，再关闭 SFTP 并等待任务；已关闭错误按成功处理。
	err := c.client.Close()
	_ = c.files.Close()
	c.tasks.Wait()
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// writeFileAtomically 通过 SFTP 写入临时文件后提交到目标。
func (c *sshConnection) writeFileAtomically(target string, content []byte, mode fs.FileMode, createOnly bool) error {
	// 1. 在目标所在目录创建随机临时文件。
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return err
	}
	temporary := filepath.Join(filepath.Dir(target), ".openvaala-write-"+hex.EncodeToString(random))
	file, err := c.files.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return err
	}
	// 2. 写入内容并忽略 fsync 错误，设置权限并关闭文件；写入、改权限或关闭失败时删除临时文件。
	_, writeErr := file.Write(content)
	_ = file.Sync()
	if err := errors.Join(writeErr, file.Chmod(mode.Perm()), file.Close()); err != nil {
		_ = c.files.Remove(temporary)
		return err
	}
	// 3. 覆盖时用 posix-rename，createOnly 时用硬链接；提交失败时删除临时文件。
	if !createOnly {
		if err := c.files.PosixRename(temporary, target); err != nil {
			_ = c.files.Remove(temporary)
			return err
		}
		return nil
	}
	err = c.files.Link(temporary, target)
	_ = c.files.Remove(temporary)
	if err != nil {
		// 目标已存在时把通用的 SFTP 错误换成 fs.ErrExist。
		if _, statErr := c.files.Lstat(target); statErr == nil {
			return fmt.Errorf("%s: %w", target, fs.ErrExist)
		}
		return err
	}
	return nil
}

// getSSHSigners 收集 SSH 认证密钥并返回 agent 连接的关闭函数。
func getSSHSigners(ctx context.Context, identityFile string) ([]ssh.Signer, func(), error) {
	// 1. 指定 identityFile 时读取该私钥，读取失败即返回错误。
	var signers []ssh.Signer
	if identityFile != "" {
		signer, err := readSSHSigner(identityFile)
		if err != nil {
			return nil, nil, fmt.Errorf("read identity file %s: %w", identityFile, err)
		}
		signers = append(signers, signer)
	}
	// 2. 连接 ssh-agent 并收集可用密钥，关闭函数留给握手结束后调用。
	closeAgent := func() {}
	if socket := os.Getenv("SSH_AUTH_SOCK"); socket != "" {
		if agentConnection, err := (&net.Dialer{}).DialContext(ctx, "unix", socket); err == nil {
			agentClosed := make(chan struct{})
			stopClosing := context.AfterFunc(ctx, func() {
				_ = agentConnection.Close()
				close(agentClosed)
			})
			closeAgent = func() {
				_ = agentConnection.Close()
				if !stopClosing() {
					<-agentClosed
				}
			}
			if agentSigners, err := agent.NewClient(agentConnection).Signers(); err == nil {
				signers = append(signers, agentSigners...)
			}
		}
	}
	// 3. 未指定私钥时读取默认私钥，缺失或带口令时跳过；没有任何密钥则关闭 agent 并报错。
	if identityFile == "" {
		home, _ := os.UserHomeDir()
		for _, name := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
			if signer, err := readSSHSigner(filepath.Join(home, ".ssh", name)); err == nil {
				signers = append(signers, signer)
			}
		}
	}
	if len(signers) == 0 {
		closeAgent()
		return nil, nil, errors.New("no ssh key found: set identity_file or add a key to ssh-agent")
	}
	return signers, closeAgent, nil
}

// readSSHSigner 读取并解析无口令私钥。
func readSSHSigner(path string) (ssh.Signer, error) {
	// 1. 读取文件，再解析 SSH 私钥。
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKey(key)
}

// sshProcessImpl 是在一个 exec 会话里运行的远端命令。
type sshProcessImpl struct {
	connection *sshConnection
	session    *ssh.Session
	// pgidPath 是记录命令进程组号的远端文件。
	pgidPath string
	// terminalInput 是 TTY 命令的会话输入端，非 TTY 时为 nil。
	terminalInput io.Writer
	// terminateLock 让 Terminate 依次执行；Wait 取得会话结果后也要等进行中的 Terminate 得出结论，再看 terminated。
	terminateLock sync.Mutex
	// terminated Terminate 是否已确认进程组结束？由 terminateLock 保护。
	terminated bool
	// waitFinished Wait 是否已取得命令的退出状态？连接断开、会话被 Close 等没拿到退出状态的情况不算。
	waitFinished atomic.Bool
}

// Wait 等待远端命令结束并返回退出结果。
func (p *sshProcessImpl) Wait() (int, error) {
	// 1. 等待会话；取得退出状态时标记完成，再删除进程组号文件。
	err := p.session.Wait()
	var exitError *ssh.ExitError
	if err == nil || errors.As(err, &exitError) {
		p.waitFinished.Store(true)
	}
	_ = p.connection.files.Remove(p.pgidPath)
	// 2. 等待进行中的 Terminate，再按终止标记、退出码或会话错误返回。
	p.terminateLock.Lock()
	terminated := p.terminated
	p.terminateLock.Unlock()
	switch {
	case terminated:
		return -1, nil
	case exitError != nil:
		return exitError.ExitStatus(), nil
	case err != nil:
		return -1, err
	}
	return 0, nil
}

// Terminate 结束远端命令的整个进程组。
func (p *sshProcessImpl) Terminate() error {
	// 1. 加锁检查是否已确认结束或已取得退出状态，是则直接返回。
	p.terminateLock.Lock()
	defer p.terminateLock.Unlock()
	if p.terminated || p.waitFinished.Load() {
		return nil
	}
	// 2. 新建会话执行 kill 脚本，建会话或命令失败时保留原会话并返回错误，可重试。
	session, err := p.connection.client.NewSession()
	if err != nil {
		return fmt.Errorf("open session to terminate remote process: %w", err)
	}
	err = session.Run(getSSHTerminateCommand(p.pgidPath))
	_ = session.Close()
	if err != nil {
		return fmt.Errorf("terminate remote process group: %w", err)
	}
	// 3. 确认进程组结束后标记 terminated，再关闭原会话。
	p.terminated = true
	_ = p.session.Close()
	return nil
}

// Close 关闭远端命令会话。
func (p *sshProcessImpl) Close() error {
	// 1. 关闭原会话而不确认远端进程组结束，忽略 EOF；未取得退出状态时 Wait 返回错误。
	if err := p.session.Close(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// ResizeTerminal 修改远端 TTY 命令的终端行列数。
func (p *sshProcessImpl) ResizeTerminal(rows, cols uint16) error {
	// 1. 非 TTY 返回错误；否则向会话发送 window-change 请求。
	if p.terminalInput == nil {
		return ErrTTYInputUnsupported
	}
	size := getTerminalSize(rows, cols)
	return p.session.WindowChange(int(size.Rows), int(size.Cols))
}

// WriteTerminalInput 写入远端命令的 TTY 输入端。
func (p *sshProcessImpl) WriteTerminalInput(data []byte) (int, error) {
	// 1. 非 TTY 返回错误；否则写入终端输入端，输入端关闭后写入返回错误。
	if p.terminalInput == nil {
		return 0, ErrTTYInputUnsupported
	}
	return p.terminalInput.Write(data)
}
