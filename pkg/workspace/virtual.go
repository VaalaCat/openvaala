package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"

	gobash "github.com/mark3labs/go-bash"
	gobashfs "github.com/mark3labs/go-bash/fs"
)

// virtualWorkspaceImpl 是 go-bash 内存文件系统上的工作区：文件与命令都只存在于本进程内存，写入限制在根目录内。
// 同一工作区的命令由 go-bash 串行执行。
type virtualWorkspaceImpl struct {
	opt   *VirtualWorkspaceOpt
	shell *gobash.Bash
}

// VirtualWorkspaceOpt 保存虚拟工作区的根目录和初始文件。
type VirtualWorkspaceOpt struct {
	// RootDirectory 是工作区在虚拟文件系统中的绝对路径，省略时为 /workspace。
	RootDirectory string
	// Files 是创建时写入的文件，键为绝对路径，父目录自动创建。
	Files map[string]*VirtualFile
}

// NewVirtualWorkspaceOpt 保存虚拟根目录和初始文件列表。
func NewVirtualWorkspaceOpt(rootDirectory string, files map[string]*VirtualFile) *VirtualWorkspaceOpt {
	// 1. 复制初始文件映射并保存根目录。
	return &VirtualWorkspaceOpt{
		RootDirectory: rootDirectory,
		Files:         maps.Clone(files),
	}
}

// NewVirtualWorkspace 创建内存工作区及初始文件。
func NewVirtualWorkspace(opt *VirtualWorkspaceOpt) (Workspace, error) {
	// 1. 缺省根目录设为 /workspace，检查根目录为绝对路径。
	if opt == nil {
		return nil, errors.New("virtual workspace options are required")
	}
	rootDirectory := opt.RootDirectory
	if rootDirectory == "" {
		rootDirectory = "/workspace"
	}
	if !path.IsAbs(rootDirectory) {
		return nil, errors.New("virtual workspace root directory must be absolute")
	}
	rootDirectory = path.Clean(rootDirectory)
	// 2. 校验初始文件绝对路径及内容，转换为立即写入或首次读取加载的条目。
	fileInits := map[string]gobashfs.FileInit{rootDirectory: {Dir: true}}
	for filePath, file := range opt.Files {
		if !path.IsAbs(filePath) || file == nil {
			return nil, fmt.Errorf("virtual file %q requires an absolute path and content", filePath)
		}
		fileInits[path.Clean(filePath)] = gobashfs.FileInit{
			Content: file.Content,
			Lazy:    file.Load,
		}
	}
	// 3. 创建 go-bash 文件系统和解释器，保存清理后的根目录。
	shell, err := gobash.New(gobash.BashOptions{
		Files: fileInits,
		Cwd:   rootDirectory,
	})
	if err != nil {
		return nil, err
	}
	return &virtualWorkspaceImpl{
		opt:   NewVirtualWorkspaceOpt(rootDirectory, nil),
		shell: shell,
	}, nil
}

// GetRootDirectory 返回虚拟工作区根目录。
func (w *virtualWorkspaceImpl) GetRootDirectory() string {
	// 1. 读取配置中的根目录。
	return w.opt.RootDirectory
}

// GetHomeDirectory 拒绝虚拟工作区的主目录查询。
func (w *virtualWorkspaceImpl) GetHomeDirectory(context.Context) (string, error) {
	// 1. 返回 errors.ErrUnsupported。
	return "", errors.ErrUnsupported
}

// ReadFile 读取虚拟普通文件的一页内容。
func (w *virtualWorkspaceImpl) ReadFile(ctx context.Context, filePath string, offset int64, limit int) (*FileContent, error) {
	// 1. 检查取消，打开文件并检查普通文件类型，再按偏移读取。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := w.shell.FS().OpenFile(filePath, os.O_RDONLY, 0)
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
	return readFileContent(file, info.Size(), offset, limit)
}

// WriteFile 写入虚拟工作区内的文件。
func (w *virtualWorkspaceImpl) WriteFile(ctx context.Context, filePath string, content []byte, mode fs.FileMode, createOnly bool) error {
	// 1. 检查取消和可写路径；父目录必须存在，createOnly 时拒绝已存在目标。
	if err := ctx.Err(); err != nil {
		return err
	}
	writablePath, err := w.getWritablePath(filePath)
	if err != nil {
		return err
	}
	fileSystem := w.shell.FS()
	if _, err := fileSystem.Stat(path.Dir(writablePath)); err != nil {
		return err
	}
	if _, err := fileSystem.Lstat(writablePath); createOnly && err == nil {
		return fmt.Errorf("%s: %w", writablePath, fs.ErrExist)
	}
	return fileSystem.WriteFile(writablePath, content, mode)
}

// OpenFileReader 打开虚拟文件的读取端。
func (w *virtualWorkspaceImpl) OpenFileReader(ctx context.Context, filePath string) (io.ReadCloser, error) {
	// 1. 检查取消，展开文件路径并打开，路径可以位于根目录外。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := w.shell.FS().Open(getAbsolutePath(w.opt.RootDirectory, filePath))
	if err != nil {
		return nil, err
	}
	return file, nil
}

// CreateFileWriter 创建虚拟文件写入端并设置权限。
func (w *virtualWorkspaceImpl) CreateFileWriter(ctx context.Context, filePath string, mode fs.FileMode) (io.WriteCloser, error) {
	// 1. 检查取消，创建父目录，再覆盖文件并设置权限。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	target := getAbsolutePath(w.opt.RootDirectory, filePath)
	fileSystem := w.shell.FS()
	if err := fileSystem.MkdirAll(path.Dir(target), 0o755); err != nil {
		return nil, err
	}
	file, err := fileSystem.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode.Perm())
	if err != nil {
		return nil, err
	}
	if err := fileSystem.Chmod(target, mode.Perm()); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// GetMetadata 读取虚拟路径元数据。
func (w *virtualWorkspaceImpl) GetMetadata(ctx context.Context, filePath string, followSymlinks bool) (*FileMetadata, error) {
	// 1. 检查取消，按参数选择 Stat 或 Lstat 并转换结果。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stat := w.shell.FS().Lstat
	if followSymlinks {
		stat = w.shell.FS().Stat
	}
	info, err := stat(filePath)
	if err != nil {
		return nil, err
	}
	return newFileMetadata(info), nil
}

// ReadDirectory 读取并排序虚拟目录的直接子项。
func (w *virtualWorkspaceImpl) ReadDirectory(ctx context.Context, directoryPath string) ([]*DirectoryEntry, error) {
	// 1. 检查取消，读取目录后转换条目。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := w.shell.FS().ReadDir(directoryPath)
	if err != nil {
		return nil, err
	}
	return newDirectoryEntries(entries), nil
}

// CreateDirectory 在虚拟工作区内递归创建目录。
func (w *virtualWorkspaceImpl) CreateDirectory(ctx context.Context, directoryPath string) error {
	// 1. 检查取消和可写路径，再创建目录。
	if err := ctx.Err(); err != nil {
		return err
	}
	writablePath, err := w.getWritablePath(directoryPath)
	if err != nil {
		return err
	}
	return w.shell.FS().MkdirAll(writablePath, 0o755)
}

// Remove 删除虚拟工作区内的文件或空目录。
func (w *virtualWorkspaceImpl) Remove(ctx context.Context, filePath string) error {
	// 1. 检查取消和可写路径，再删除目标。
	if err := ctx.Err(); err != nil {
		return err
	}
	writablePath, err := w.getWritablePath(filePath)
	if err != nil {
		return err
	}
	return w.shell.FS().Remove(writablePath)
}

// StartProcess 在 go-bash 中后台执行非 TTY 命令。
func (w *virtualWorkspaceImpl) StartProcess(ctx context.Context, request *ProcessRequest) (Process, error) {
	// 1. 检查取消、TTY 和工作目录，TTY 返回 ErrTTYUnsupported。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.TTY {
		return nil, ErrTTYUnsupported
	}
	if info, err := w.shell.FS().Stat(request.Directory); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("process directory %s is not a directory", request.Directory)
	}
	// 2. 创建不随启动请求取消的执行上下文，后台运行命令。
	processCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	process := &virtualProcessImpl{
		cancel:   cancel,
		finished: make(chan struct{}),
	}
	go process.run(processCtx, w.shell, request)
	return process, nil
}

// Dial 拒绝虚拟工作区的网络拨号。
func (w *virtualWorkspaceImpl) Dial(context.Context, string, string) (net.Conn, error) {
	// 1. 返回 errors.ErrUnsupported。
	return nil, errors.ErrUnsupported
}

// GetHTTPProxy 总是返回 nil，虚拟工作区没有网络。
func (w *virtualWorkspaceImpl) GetHTTPProxy(*http.Request) (*url.URL, error) {
	// 1. 返回 nil。
	return nil, nil
}

// getWritablePath 校验目标路径和解析符号链接后的路径都在工作区内，返回清理后的目标路径。
func (w *virtualWorkspaceImpl) getWritablePath(target string) (string, error) {
	// 1. 检查规范路径，再解析已有目标或父目录的符号链接并再次检查范围。
	cleaned := path.Clean(target)
	if _, err := getRelativePath(w.opt.RootDirectory, cleaned); err != nil {
		return "", err
	}
	resolved := cleaned
	if realPath, err := w.shell.FS().Realpath(cleaned); err == nil {
		resolved = realPath
	} else if parent, err := w.shell.FS().Realpath(path.Dir(cleaned)); err == nil {
		resolved = path.Join(parent, path.Base(cleaned))
	}
	if _, err := getRelativePath(w.opt.RootDirectory, resolved); err != nil {
		return "", err
	}
	return cleaned, nil
}

// virtualProcessImpl 是在 go-bash 解释器中运行的一条命令；Terminate 取消解释器的 ctx，解释器在命令之间检查并停止。
type virtualProcessImpl struct {
	cancel   context.CancelFunc
	finished chan struct{}
	exitCode int
	err      error
}

// run 执行虚拟命令并保存退出结果。
func (p *virtualProcessImpl) run(ctx context.Context, shell *gobash.Bash, request *ProcessRequest) {
	// 1. 设置结束时的取消和通道关闭，运行解释器并接入输入输出。
	defer close(p.finished)
	defer p.cancel()
	result, err := shell.Exec(ctx, request.Command, gobash.ExecOptions{
		Cwd:    request.Directory,
		Stdin:  request.Stdin,
		Stdout: getOutputWriter(request.Stdout),
		Stderr: getOutputWriter(request.Stderr),
	})
	// 2. 取消时保存 -1，执行失败时保存错误，正常结束保存解释器退出码。
	switch {
	case ctx.Err() != nil:
		p.exitCode = -1
	case err != nil:
		p.exitCode, p.err = -1, err
	default:
		p.exitCode = result.ExitCode
	}
}

// Wait 等待虚拟命令完成并返回退出结果。
func (p *virtualProcessImpl) Wait() (int, error) {
	// 1. 等待 finished 后读取退出码和错误。
	<-p.finished
	return p.exitCode, p.err
}

// Terminate 取消虚拟命令的执行。
func (p *virtualProcessImpl) Terminate() error {
	// 1. 取消执行上下文并返回 nil，解释器在命令之间检查并停止。
	p.cancel()
	return nil
}

// Close 取消虚拟命令的执行上下文。
func (p *virtualProcessImpl) Close() error {
	// 1. 调用取消函数并返回 nil。
	p.cancel()
	return nil
}

// ResizeTerminal 拒绝虚拟命令的终端尺寸修改。
func (p *virtualProcessImpl) ResizeTerminal(rows, cols uint16) error {
	// 1. 返回 ErrTTYUnsupported。
	return ErrTTYUnsupported
}

// WriteTerminalInput 拒绝虚拟命令的 TTY 输入。
func (p *virtualProcessImpl) WriteTerminalInput([]byte) (int, error) {
	// 1. 返回零字节和 ErrTTYInputUnsupported。
	return 0, ErrTTYInputUnsupported
}
