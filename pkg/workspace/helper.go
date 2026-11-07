package workspace

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/creack/pty"
)

// newFileMetadata 提取文件类型、大小、权限和修改时间。
func newFileMetadata(info fs.FileInfo) *FileMetadata {
	// 1. 读取 FileInfo 并填写元数据。
	return &FileMetadata{
		IsDirectory: info.IsDir(),
		IsFile:      info.Mode().IsRegular(),
		IsSymlink:   info.Mode()&fs.ModeSymlink != 0,
		Size:        info.Size(),
		Mode:        info.Mode().Perm(),
		ModifiedAt:  info.ModTime(),
	}
}

// newDirectoryEntries 把文件系统目录项转成按名称排序的条目。
func newDirectoryEntries(entries []fs.DirEntry) []*DirectoryEntry {
	// 1. 提取名称和类型，再按名称排序。
	directoryEntries := make([]*DirectoryEntry, 0, len(entries))
	for _, entry := range entries {
		directoryEntries = append(directoryEntries, &DirectoryEntry{
			Name:        entry.Name(),
			IsDirectory: entry.IsDir(),
			IsFile:      entry.Type().IsRegular(),
		})
	}
	slices.SortFunc(directoryEntries, func(a, b *DirectoryEntry) int { return strings.Compare(a.Name, b.Name) })
	return directoryEntries
}

// getRelativePath 计算目标相对工作区根目录的路径。
func getRelativePath(root, path string) (string, error) {
	// 1. 清理路径并计算相对路径，越界的 .. 及其下级路径返回错误。
	relative, err := filepath.Rel(root, filepath.Clean(path))
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", ErrOutsideWorkspace
	}
	return relative, nil
}

// getAbsolutePath 解析目标的规范化绝对路径。
func getAbsolutePath(root, target string) string {
	// 1. 绝对路径直接清理，其他路径与根目录拼接。
	if filepath.IsAbs(target) {
		return filepath.Clean(target)
	}
	return filepath.Join(root, target)
}

// readFileContent 从指定偏移读取一页内容并报告文件大小和 EOF。
func readFileContent(file io.ReadSeeker, size, offset int64, limit int) (*FileContent, error) {
	// 1. 按 limit 和已知剩余大小分配缓冲，定位到 offset。
	bufferSize := limit
	if size > 0 && size-offset < int64(limit) {
		bufferSize = int(max(size-offset, 0))
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	// 2. 读取缓冲长度的内容，把 EOF 和不足一页作为读到末尾处理。
	content := make([]byte, bufferSize)
	count, err := io.ReadFull(file, content)
	reachedEOF := errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
	if err != nil && !reachedEOF {
		return nil, err
	}
	return &FileContent{
		Content: content[:count],
		Size:    size,
		EOF:     reachedEOF || (size > 0 && offset+int64(count) >= size),
	}, nil
}

// getOutputWriter 选择命令输出的写入端。
func getOutputWriter(writer io.Writer) io.Writer {
	// 1. nil 时返回 io.Discard，否则返回原 writer。
	if writer == nil {
		return io.Discard
	}
	return writer
}

// closeFiles 关闭传入的文件。
func closeFiles(files ...*os.File) {
	// 1. 逐个关闭文件，忽略关闭错误。
	for _, file := range files {
		_ = file.Close()
	}
}

// newPollableTerminal 在 Linux 上把 PTY master 换成由运行时轮询的非阻塞文件。
func newPollableTerminal(terminal *os.File) (*os.File, error) {
	// 1. 非 Linux 返回原文件；Linux 在复制后关闭原文件，失败也关闭。
	if runtime.GOOS != "linux" {
		return terminal, nil
	}
	defer terminal.Close()
	// 2. 持 ForkLock 复制 fd，并设为 close-on-exec。
	syscall.ForkLock.RLock()
	fd, err := syscall.Dup(int(terminal.Fd()))
	if err == nil {
		syscall.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, err
	}
	// 3. 设置非阻塞模式，再用 os.NewFile 登记运行时轮询。
	if err := syscall.SetNonblock(fd, true); err != nil {
		_ = syscall.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), terminal.Name()), nil
}

// setTerminalSize 在 PTY master 上设置行列数；经 SyscallConn 与 Close 同步，master 已关闭时返回错误，不改变文件的阻塞模式。
func setTerminalSize(terminal *os.File, size *pty.Winsize) error {
	// 1. 在 Control 持有 fd 期间调用 TIOCSWINSZ。
	connection, err := terminal.SyscallConn()
	if err != nil {
		return err
	}
	var ioctlErr error
	if err := connection.Control(func(fd uintptr) {
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(size))); errno != 0 {
			ioctlErr = errno
		}
	}); err != nil {
		return err
	}
	return ioctlErr
}

// parseSSHAddress 从 [user@]host[:port] 解析出 SSH 用户名和 host:port。
func parseSSHAddress(address string) (string, string, error) {
	// 1. 拆分可选用户名和主机端口，省略端口时使用 22，再检查主机名。
	userName, host := "", address
	if index := strings.LastIndex(address, "@"); index >= 0 {
		userName, host = address[:index], address[index+1:]
	}
	hostName, port, err := net.SplitHostPort(host)
	if err != nil {
		hostName, port = strings.Trim(host, "[]"), "22"
	}
	if strings.TrimSpace(hostName) == "" {
		return "", "", fmt.Errorf("ssh address %q requires a host", address)
	}
	// 2. 用户名缺失时读取本机用户，返回规范化的 host:port。
	if userName == "" {
		current, err := user.Current()
		if err != nil {
			return "", "", fmt.Errorf("ssh address %q has no user: %w", address, err)
		}
		userName = current.Username
	}
	return userName, net.JoinHostPort(hostName, port), nil
}

// newSSHLogger 返回带远端主机和端口的日志对象。
func newSSHLogger(pipeline *telemetry.Telemetry, hostPort string) *slog.Logger {
	// 1. 拆分规范化地址，给遥测日志补充地址和数字端口。
	host, port, _ := net.SplitHostPort(hostPort)
	portNumber, _ := strconv.Atoi(port)
	return telemetry.GetLogger(telemetry.ContextWithTelemetry(context.Background(), pipeline)).With("server.address", host, "server.port", portNumber)
}

// getSSHDialError 整理 SSH 拨号的失败原因。
func getSSHDialError(ctx context.Context, err error) error {
	// 1. ctx 已结束时同时包装 ctx 错误和拨号错误，否则返回原错误。
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%w: %w", ctxErr, err)
	}
	return err
}

// QuoteShellWord 把文本编码为 bash 的单个参数。
func QuoteShellWord(word string) string {
	// 1. 用单引号包裹文本，转义文本中的单引号。
	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}

// getTerminalSize 返回 TTY 命令的终端尺寸；行或列为零时按 24×80。
func getTerminalSize(rows, cols uint16) *pty.Winsize {
	// 1. 行列数为零时各自取默认值。
	if rows == 0 {
		rows = 24
	}
	if cols == 0 {
		cols = 80
	}
	return &pty.Winsize{Rows: rows, Cols: cols}
}

// getSSHProcessCommand 生成远端 bash 命令，记录命令进程组号。
func getSSHProcessCommand(request *ProcessRequest, pgidPath string) string {
	// 1. 引用工作目录、命令和进程组号文件路径。
	directory := QuoteShellWord(request.Directory)
	command := QuoteShellWord(request.Command)
	pgidFile := QuoteShellWord(pgidPath)
	// 2. 非 TTY 命令由 setsid 启动新进程组，记录 $!，读取会话输入；bash 退出后清理整个进程组。
	script := "cd -- " + directory + " || exit 1\n" +
		"setsid bash -c " + command + " <&0 &\n" +
		"pid=$!\n" +
		"echo \"$pid\" > " + pgidFile + "\n" +
		"wait \"$pid\" 2>/dev/null\n" + // 丢弃 wait 输出的作业状态
		"code=$?\n" +
		"kill -KILL -- -\"$pid\" 2>/dev/null\n" +
		"exit \"$code\"\n"
	if request.TTY {
		// 3. TTY 命令记录 /proc/$$/stat 第五列的进程组号，再 exec 命令，沿用这个进程组和 PTY。
		script = "export COLORTERM=truecolor\n" +
			"read -r _ _ _ _ pgid _ < /proc/$$/stat\n" +
			"echo \"$pgid\" > " + pgidFile + "\n" +
			"cd -- " + directory + " || exit 1\n" +
			"exec bash -c " + command + "\n"
	}
	return getSSHBashCommand(script)
}

// getSSHTerminateCommand 生成结束远端进程组并报告是否成功的命令。
func getSSHTerminateCommand(pgidPath string) string {
	// 1. 生成等待进程组号文件最多约两秒的脚本；号码无效退出 1，SIGKILL 成功退出 0。
	// kill 失败后用 kill -0 检查，进程组仍存在退出 1，否则退出 0。
	pgidFile := QuoteShellWord(pgidPath)
	script := "for _ in $(seq 20); do [ -s " + pgidFile + " ] && break; sleep 0.1; done\n" +
		"pgid=$(cat " + pgidFile + " 2>/dev/null)\n" +
		"case \"$pgid\" in ''|*[!0-9]*) exit 1 ;; esac\n" +
		"kill -KILL -- -\"$pgid\" 2>/dev/null && exit 0\n" +
		"kill -0 -- -\"$pgid\" 2>/dev/null && exit 1\n" +
		"exit 0\n"
	return getSSHBashCommand(script)
}

// getSSHBashCommand 把脚本编码为 base64，生成在远端 bash 里解码执行的命令。
func getSSHBashCommand(script string) string {
	// 1. 编码脚本，拼入 bash 解码并 eval 的命令；远端须有 base64。
	encoded := base64.StdEncoding.EncodeToString([]byte(script))
	return "bash -c 'eval \"$(echo " + encoded + " | base64 -d)\"'"
}

// GetFileText 分块读取文件全文并返回文本。
func GetFileText(ctx context.Context, source Workspace, path string) (string, error) {
	// 1. 每次读取最多 1 MB，追加内容直到 EOF；未到 EOF 却读不到内容时返回 io.ErrNoProgress。
	var text strings.Builder
	for offset := int64(0); ; {
		part, err := source.ReadFile(ctx, path, offset, 1<<20)
		if err != nil {
			return "", err
		}
		text.Write(part.Content)
		if part.EOF {
			return text.String(), nil
		}
		if len(part.Content) == 0 {
			return "", io.ErrNoProgress
		}
		offset += int64(len(part.Content))
	}
}

// splitMemoryPath 把清理后的记忆工作区路径拆成第一段名字和后面的相对路径；根目录的名字为空。
func splitMemoryPath(cleaned string) (name, relative string) {
	// 1. 去掉开头的 /，按第一个 / 拆开。
	name, relative, _ = strings.Cut(strings.TrimPrefix(cleaned, "/"), "/")
	return name, relative
}

// isMemoryTopDirectory 清理后的路径是不是记忆工作区的根目录、user 目录或 project 目录？
func isMemoryTopDirectory(cleaned string) bool {
	// 1. 这三个目录由记忆工作区自己提供，不对应可以删除或覆盖的真实文件。
	return cleaned == "/" || cleaned == "/user" || cleaned == "/project"
}
