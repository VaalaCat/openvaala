package workspace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"syscall"

	"github.com/creack/pty"
)

// localWorkspaceImpl 是本机目录上的工作区：读取直接访问本机文件系统，写入经 os.Root 限制在根目录内，命令由真实 bash 执行。
type localWorkspaceImpl struct {
	opt *LocalWorkspaceOpt
}

// LocalWorkspaceOpt 保存本机工作区的根目录和命令环境变量。
type LocalWorkspaceOpt struct {
	// RootDirectory 是工作区根目录，必须是绝对路径。
	RootDirectory string
	// Environment 是执行命令用的环境变量，格式同 os.Environ；为 nil 时继承当前进程的环境。
	Environment []string
}

// NewLocalWorkspaceOpt 保存本机工作区根目录。
func NewLocalWorkspaceOpt(rootDirectory string) *LocalWorkspaceOpt {
	// 1. 创建根目录配置。
	return &LocalWorkspaceOpt{RootDirectory: rootDirectory}
}

// WithEnvironment 设置命令环境变量并返回本配置。
func (o *LocalWorkspaceOpt) WithEnvironment(environment []string) *LocalWorkspaceOpt {
	// 1. 保存环境变量列表。
	o.Environment = environment
	return o
}

// NewLocalWorkspace 校验并保存本机工作区配置。
func NewLocalWorkspace(opt *LocalWorkspaceOpt) (Workspace, error) {
	// 1. 要求根目录为绝对路径，清理路径后保存配置。
	if opt == nil || !filepath.IsAbs(opt.RootDirectory) {
		return nil, errors.New("local workspace requires an absolute root directory")
	}
	return &localWorkspaceImpl{opt: NewLocalWorkspaceOpt(filepath.Clean(opt.RootDirectory)).WithEnvironment(opt.Environment)}, nil
}

// GetRootDirectory 返回本机工作区根目录。
func (w *localWorkspaceImpl) GetRootDirectory() string {
	// 1. 读取配置中的根目录。
	return w.opt.RootDirectory
}

// GetHomeDirectory 返回本机当前用户的主目录。
func (w *localWorkspaceImpl) GetHomeDirectory(ctx context.Context) (string, error) {
	// 1. 先检查取消，再读取用户主目录。
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return os.UserHomeDir()
}

// ReadFile 按偏移读取本机普通文件的一页内容。
func (w *localWorkspaceImpl) ReadFile(ctx context.Context, path string, offset int64, limit int) (*FileContent, error) {
	// 1. 检查取消，以非阻塞方式打开文件并检查普通文件类型。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, ErrNotRegularFile
	}
	// 2. 定位并读取一页，附带文件大小和 EOF。
	return readFileContent(file, info.Size(), offset, limit)
}

// WriteFile 在工作区内写入完整文件。
func (w *localWorkspaceImpl) WriteFile(ctx context.Context, path string, content []byte, mode fs.FileMode, createOnly bool) error {
	// 1. 检查取消，打开受限根目录并写入临时文件后提交。
	if err := ctx.Err(); err != nil {
		return err
	}
	root, relative, err := w.openRoot(path)
	if err != nil {
		return err
	}
	defer root.Close()
	return writeFileAtomically(root, relative, content, mode, createOnly)
}

// OpenFileReader 打开本机文件的读取端。
func (w *localWorkspaceImpl) OpenFileReader(ctx context.Context, path string) (io.ReadCloser, error) {
	// 1. 检查取消，展开路径并打开文件，路径可以位于工作区外。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.Open(getAbsolutePath(w.opt.RootDirectory, path))
	if err != nil {
		return nil, err
	}
	return file, nil
}

// CreateFileWriter 创建本机文件的写入端。
func (w *localWorkspaceImpl) CreateFileWriter(ctx context.Context, path string, mode fs.FileMode) (io.WriteCloser, error) {
	// 1. 检查取消，展开路径并创建父目录，再创建或覆盖目标文件。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	target := getAbsolutePath(w.opt.RootDirectory, path)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode.Perm())
	if err != nil {
		return nil, err
	}
	// 2. 把文件权限设成 mode，不受 umask 影响。
	if err := file.Chmod(mode.Perm()); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// GetMetadata 读取路径元数据，按参数选择是否跟随符号链接。
func (w *localWorkspaceImpl) GetMetadata(ctx context.Context, path string, followSymlinks bool) (*FileMetadata, error) {
	// 1. 检查取消，选择 Stat 或 Lstat 并转换元数据。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stat := os.Lstat
	if followSymlinks {
		stat = os.Stat
	}
	info, err := stat(path)
	if err != nil {
		return nil, err
	}
	return newFileMetadata(info), nil
}

// ReadDirectory 返回按名称排序的直接目录项。
func (w *localWorkspaceImpl) ReadDirectory(ctx context.Context, path string) ([]*DirectoryEntry, error) {
	// 1. 检查取消，读取目录并转换条目。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	return newDirectoryEntries(entries), nil
}

// CreateDirectory 在工作区内递归创建目录。
func (w *localWorkspaceImpl) CreateDirectory(ctx context.Context, path string) error {
	// 1. 检查取消并打开受限根目录，按相对路径创建目录。
	if err := ctx.Err(); err != nil {
		return err
	}
	root, relative, err := w.openRoot(path)
	if err != nil {
		return err
	}
	defer root.Close()
	return root.MkdirAll(relative, 0o755)
}

// Remove 删除工作区内的文件或空目录。
func (w *localWorkspaceImpl) Remove(ctx context.Context, path string) error {
	// 1. 检查取消并打开受限根目录，删除目标后同步父目录。
	if err := ctx.Err(); err != nil {
		return err
	}
	root, relative, err := w.openRoot(path)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.Remove(relative); err != nil {
		return err
	}
	return syncDirectory(root, filepath.Dir(relative))
}

// StartProcess 启动独立进程组中的本机 bash 命令，接入输入输出。
func (w *localWorkspaceImpl) StartProcess(ctx context.Context, request *ProcessRequest) (Process, error) {
	// 1. 检查取消、TTY 输入和工作目录。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.TTY && request.Stdin != nil {
		return nil, ErrTTYStdinUnsupported
	}
	if info, err := os.Stat(request.Directory); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("process directory %s is not a directory", request.Directory)
	}
	// 2. 创建 bash 命令，设置工作目录及环境，TTY 时启动 24×80 的终端。
	command := exec.Command("bash", "-c", request.Command)
	command.Dir = request.Directory
	command.Env = w.opt.Environment // nil 时 exec 继承当前进程的环境
	process := &localProcessImpl{command: command}
	if request.TTY {
		// TTY 在新会话启动 bash，输入接 PTY，输出从 PTY master 复制；环境里声明 256 色和真彩色，重复变量以后者为准。
		if command.Env == nil {
			command.Env = os.Environ()
		}
		command.Env = append(slices.Clone(command.Env), "TERM=xterm-256color", "COLORTERM=truecolor")
		terminal, err := pty.StartWithSize(command, getTerminalSize(request.Rows, request.Cols))
		if err != nil {
			return nil, err
		}
		if process.terminal, err = newPollableTerminal(terminal); err != nil {
			_ = process.Terminate()
			_ = command.Wait()
			return nil, err
		}
		process.copyOutput(process.terminal, request.Stdout)
		return process, nil
	}
	// 3. 非 TTY 设置独立进程组，用管道分别复制 stdout 和 stderr。
	command.Stdin = request.Stdin
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stderr, stderrWriter, err := os.Pipe()
	if err != nil {
		closeFiles(stdout, stdoutWriter)
		return nil, err
	}
	command.Stdout = stdoutWriter
	command.Stderr = stderrWriter
	err = command.Start()
	closeFiles(stdoutWriter, stderrWriter)
	if err != nil {
		closeFiles(stdout, stderr)
		return nil, err
	}
	process.copyOutput(stdout, request.Stdout)
	process.copyOutput(stderr, request.Stderr)
	return process, nil
}

// Dial 从本机建立网络连接。
func (w *localWorkspaceImpl) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	// 1. 使用 ctx 拨号指定网络和地址。
	return (&net.Dialer{}).DialContext(ctx, network, address)
}

// GetHTTPProxy 按当前进程的 HTTP 代理环境变量选择代理。
func (w *localWorkspaceImpl) GetHTTPProxy(request *http.Request) (*url.URL, error) {
	// 1. 调用 ProxyFromEnvironment 读取 HTTPS_PROXY、HTTP_PROXY 和 NO_PROXY。
	return http.ProxyFromEnvironment(request)
}

// openRoot 打开工作区根目录并解析目标相对路径。
func (w *localWorkspaceImpl) openRoot(path string) (*os.Root, string, error) {
	// 1. 检查相对路径，越界时返回错误，否则打开 os.Root。
	relative, err := getRelativePath(w.opt.RootDirectory, path)
	if err != nil {
		return nil, "", err
	}
	root, err := os.OpenRoot(w.opt.RootDirectory)
	if err != nil {
		return nil, "", err
	}
	return root, relative, nil
}

// writeFileAtomically 通过临时文件提交目标内容。
func writeFileAtomically(root *os.Root, relative string, content []byte, mode fs.FileMode, createOnly bool) error {
	// 1. 在目标目录创建随机名称的临时文件。
	directory := filepath.Dir(relative)
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return err
	}
	temporary := filepath.Join(directory, ".openvaala-write-"+hex.EncodeToString(random))
	file, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm())
	if err != nil {
		return err
	}
	// 2. 写入并同步临时文件，失败时删除临时文件。
	_, writeErr := file.Write(content)
	if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		_ = root.Remove(temporary)
		return err
	}
	// 3. createOnly 用硬链接创建目标，其他情况改名覆盖；提交后同步目录。
	if createOnly {
		err = root.Link(temporary, relative)
		_ = root.Remove(temporary)
	} else if err = root.Rename(temporary, relative); err != nil {
		_ = root.Remove(temporary)
	}
	if err != nil {
		return err
	}
	return syncDirectory(root, directory)
}

// syncDirectory 同步并关闭目录句柄。
func syncDirectory(root *os.Root, directory string) error {
	// 1. 按目录打开目标，调用 Sync 并合并关闭错误。
	handle, err := root.OpenFile(directory, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	return errors.Join(handle.Sync(), handle.Close())
}

// localProcessImpl 是本机 bash 进程：输出由复制任务写入请求的 Writer，Wait 在 bash 退出后结束整个进程组并等待复制完成。
type localProcessImpl struct {
	command *exec.Cmd
	// terminal 是 TTY 命令的 PTY master，非 TTY 时为 nil：WriteTerminalInput 写它，输出复制任务读它并在复制结束后关闭。
	// Linux 上它由运行时轮询，关闭会打断程序没读走的写入。
	terminal     *os.File
	outputCopies sync.WaitGroup
}

// Wait 等待本机命令结束并返回退出结果。
func (p *localProcessImpl) Wait() (int, error) {
	// 1. 等待命令退出，结束进程组里还在运行的进程（失败不影响退出码），再等待输出复制结束。
	err := p.command.Wait()
	_ = p.Terminate()
	p.outputCopies.Wait()
	// 2. 返回 bash 退出码；未取得进程退出状态时返回错误。
	var exitError *exec.ExitError
	if err != nil && !errors.As(err, &exitError) {
		return -1, err
	}
	return p.command.ProcessState.ExitCode(), nil
}

// Terminate 结束本机命令的整个进程组。
func (p *localProcessImpl) Terminate() error {
	// 1. 按负 PID 发送 SIGKILL，进程组已不存在的 ESRCH 错误按成功处理。
	err := syscall.Kill(-p.command.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// Close 结束本机命令的进程组。
func (p *localProcessImpl) Close() error {
	// 1. 调用 Terminate。
	return p.Terminate()
}

// ResizeTerminal 修改本机 TTY 命令的终端行列数。
func (p *localProcessImpl) ResizeTerminal(rows, cols uint16) error {
	// 1. 非 TTY 返回错误；否则在 PTY master 上设置新尺寸，内核向前台进程组发送 SIGWINCH。
	if p.terminal == nil {
		return ErrTTYInputUnsupported
	}
	return pty.Setsize(p.terminal, getTerminalSize(rows, cols))
}

// WriteTerminalInput 写入本机命令的 TTY 终端。
func (p *localProcessImpl) WriteTerminalInput(data []byte) (int, error) {
	// 1. 非 TTY 返回错误；否则写入 PTY master，写满缓冲时等待读取，Linux 终端关闭后写入返回错误。
	if p.terminal == nil {
		return 0, ErrTTYInputUnsupported
	}
	return p.terminal.Write(data)
}

// copyOutput 后台复制本机命令的输出。
func (p *localProcessImpl) copyOutput(reader *os.File, writer io.Writer) {
	// 1. 选择输出 writer，启动复制任务并登记等待，复制结束后关闭读取端。
	writer = getOutputWriter(writer)
	p.outputCopies.Go(func() {
		defer reader.Close()
		_, _ = io.Copy(writer, reader)
	})
}
