// Package workspace 定义执行环境工具用的工作区：按路径读写文件、启动进程。
// 本机实现基于 os、os.Root 和真实 bash；SSH 实现经 SFTP 读写文件、在远端 bash 里运行命令；虚拟实现基于 go-bash 的内存文件系统和解释器。
// 路径都是工作区里的绝对路径；读取不限范围，WriteFile 等写入只能在工作区根目录之内，命令不受这个限制。
// 流式的 OpenFileReader、CreateFileWriter 和 CopyFile 还接受相对根目录的路径，写入也不限于根目录。
package workspace

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// Workspace 是一个 Executor 的工作区，工具按路径读写文件、启动命令；实现可被多个工具并发调用。
type Workspace interface {
	// GetRootDirectory 返回工作区根目录的绝对路径，工具以它为相对路径的基准。
	GetRootDirectory() string
	// GetHomeDirectory 返回工作区所在机器上当前用户的主目录：本机是 serve 进程的用户（os.UserHomeDir），
	// SSH 是登录用户（SFTP 的初始目录，sftp.Client.Getwd）；virtual 没有主目录，返回 errors.ErrUnsupported。
	GetHomeDirectory(ctx context.Context) (string, error)
	// ReadFile 跟随符号链接，从 offset 起读取最多 limit 字节，不足时读到文件末尾；目标不是普通文件时返回 ErrNotRegularFile。
	ReadFile(ctx context.Context, path string, offset int64, limit int) (*FileContent, error)
	// WriteFile 写入完整内容，父目录须已存在；目标在工作区外时返回 ErrOutsideWorkspace；
	// createOnly 且目标已存在时返回满足 fs.ErrExist 的错误。
	WriteFile(ctx context.Context, path string, content []byte, mode fs.FileMode, createOnly bool) error
	// OpenFileReader 打开文件顺序读取，跟随符号链接，不限于工作区根目录；path 为相对根目录的路径或绝对路径。
	// 文件不存在时返回满足 fs.ErrNotExist 的错误；读完由调用方 Close。ctx 只约束打开。
	OpenFileReader(ctx context.Context, path string) (io.ReadCloser, error)
	// CreateFileWriter 创建或覆盖一个文件并返回写入端，权限位为 mode（不受 umask 影响）；
	// path 为相对根目录的路径或绝对路径，可以在根目录外；父目录不存在时创建；Close 返回 nil 才算写完。ctx 只约束创建。
	CreateFileWriter(ctx context.Context, path string, mode fs.FileMode) (io.WriteCloser, error)
	// GetMetadata 返回路径的元数据；followSymlinks 为 false 时描述符号链接本身；不存在时返回满足 fs.ErrNotExist 的错误。
	GetMetadata(ctx context.Context, path string, followSymlinks bool) (*FileMetadata, error)
	// ReadDirectory 按名称顺序返回目录的直接子项，不跟随符号链接。
	ReadDirectory(ctx context.Context, path string) ([]*DirectoryEntry, error)
	// CreateDirectory 递归创建工作区内的目录，已存在时成功。
	CreateDirectory(ctx context.Context, path string) error
	// Remove 删除工作区内的一个文件或空目录。
	Remove(ctx context.Context, path string) error
	// StartProcess 在 request.Directory 启动一条 bash 命令；ctx 只约束启动，结束命令使用 Process.Terminate。
	// request.Stdin 非 nil 时，调用方不再写入后要关闭它的写端，命令才能结束读取。
	StartProcess(ctx context.Context, request *ProcessRequest) (Process, error)
	// Dial 从工作区所在的机器建立一条网络连接：本机直接连；SSH 经这条 SSH 连接转发，由远端发起，
	// 地址也在远端解析；virtual 返回 errors.ErrUnsupported。
	Dial(ctx context.Context, network, address string) (net.Conn, error)
	// GetHTTPProxy 返回从工作区所在机器发出 HTTP 请求时要经过的代理，签名同 http.Transport.Proxy：
	// 本机按 serve 进程的 HTTPS_PROXY、HTTP_PROXY、NO_PROXY（http.ProxyFromEnvironment）；
	// SSH 拿不到远端 shell 的代理设置，返回 nil 表示直连；virtual 返回 nil。
	GetHTTPProxy(request *http.Request) (*url.URL, error)
}

// Process 是 StartProcess 启动的一条命令。
type Process interface {
	// Wait 等待命令结束并返回退出码；返回时输出已全部写入请求中的 Writer。被 Terminate 结束时退出码为 -1。
	Wait() (int, error)
	// Terminate 结束命令所在的整个进程组；没能确认结束时返回错误，可以重试；进程组已不存在时返回 nil。
	Terminate() error
	// WriteTerminalInput 把 data 原样写进 TTY 命令的终端，返回实际写入的字节数；
	// 非 TTY 命令返回 ErrTTYInputUnsupported，命令已结束时返回写入错误。
	WriteTerminalInput(data []byte) (int, error)
	// ResizeTerminal 修改 TTY 命令的终端行列数；非 TTY 命令返回 ErrTTYInputUnsupported，virtual 命令返回 ErrTTYUnsupported。
	ResizeTerminal(rows, cols uint16) error
	// Close 放手这条命令：关闭本地这一端，让 Wait 尽快返回。SSH 关闭命令的会话，不证明远端已停止，Wait 随后返回错误；
	// 本机和 virtual 等同于 Terminate。只在不能无限等待的收尾路径上、Terminate 失败之后调用。
	Close() error
}

// CopyFile 跨工作区流式复制普通文件并保留权限。
func CopyFile(ctx context.Context, source Workspace, sourcePath string, target Workspace, targetPath string) error {
	// 1. 读取源文件元数据，拒绝非普通文件。
	metadata, err := source.GetMetadata(ctx, getAbsolutePath(source.GetRootDirectory(), sourcePath), true)
	if err != nil {
		return err
	}
	if !metadata.IsFile {
		return ErrNotRegularFile
	}
	// 2. 打开源文件和目标写入端，复制内容；目标写入不限于根目录，Close 成功后返回。
	reader, err := source.OpenFileReader(ctx, sourcePath)
	if err != nil {
		return err
	}
	defer reader.Close()
	writer, err := target.CreateFileWriter(ctx, targetPath, metadata.Mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(writer, reader); err != nil {
		_ = writer.Close()
		return err
	}
	return writer.Close()
}

// ListDirectoryFiles 遍历目录并按相对路径排序返回目录和普通文件。
func ListDirectoryFiles(ctx context.Context, source Workspace, root string, skippedNames []string) ([]DirectoryFile, error) {
	// 1. 逐目录读取条目，跳过 skippedNames；收目录和普通文件，指向文件的符号链接按文件收，指向目录的链接、坏链接和其他特殊文件跳过。
	var files []DirectoryFile
	pendingDirectories := []string{""}
	for len(pendingDirectories) > 0 {
		relativeDirectory := pendingDirectories[len(pendingDirectories)-1]
		pendingDirectories = pendingDirectories[:len(pendingDirectories)-1]
		entries, err := source.ReadDirectory(ctx, filepath.Join(root, relativeDirectory))
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if slices.Contains(skippedNames, entry.Name) {
				continue
			}
			relativePath := filepath.Join(relativeDirectory, entry.Name)
			metadata, err := source.GetMetadata(ctx, filepath.Join(root, relativePath), true)
			if err != nil && (entry.IsDirectory || entry.IsFile) {
				return nil, err
			}
			switch {
			case entry.IsDirectory:
				files = append(files, DirectoryFile{
					RelativePath: relativePath,
					Mode:         metadata.Mode,
					IsDirectory:  true,
				})
				pendingDirectories = append(pendingDirectories, relativePath)
			case err == nil && metadata.IsFile:
				files = append(files, DirectoryFile{
					RelativePath: relativePath,
					Mode:         metadata.Mode,
				})
			}
		}
	}
	// 2. 按相对路径排序后返回。
	slices.SortFunc(files, func(a, b DirectoryFile) int { return strings.Compare(a.RelativePath, b.RelativePath) })
	return files, nil
}

// CopyDirectory 跨工作区复制完整目录。
func CopyDirectory(ctx context.Context, source Workspace, sourcePath string,
	target Workspace, targetPath string, skippedNames []string) error {
	// 1. 目标已存在时直接返回，否则列出源目录内容并跳过指定名称。
	if _, err := target.GetMetadata(ctx, targetPath, true); err == nil {
		return nil
	}
	files, err := ListDirectoryFiles(ctx, source, sourcePath, skippedNames)
	if err != nil {
		return err
	}
	// 2. 在目标启动 tar 解包到 .partial 目录，再改名；目标机器须有 tar，已出现的目标目录保留。
	command := "P=" + QuoteShellWord(targetPath+".partial") + "; T=" + QuoteShellWord(targetPath) + "; " +
		`rm -rf "$P" && mkdir -p "$P" && tar -xzpf - -C "$P" && { if [ -d "$T" ]; then rm -rf "$P"; else mv "$P" "$T"; fi; }`
	archiveReader, archiveWriter := io.Pipe()
	var stderr bytes.Buffer
	process, err := target.StartProcess(ctx, &ProcessRequest{
		Command:   command,
		Directory: target.GetRootDirectory(),
		Stdin:     archiveReader,
		Stderr:    &stderr,
	})
	if err != nil {
		return err
	}
	// 3. 后台打包并写入管道，等待目标解包命令退出。
	var tasks sync.WaitGroup
	var archiveErr error
	tasks.Go(func() {
		archiveErr = writeDirectoryArchive(ctx, source, sourcePath, files, archiveWriter)
		archiveWriter.CloseWithError(archiveErr)
	})
	code, err := process.Wait()
	// 4. 命令退出后关闭读取端，再等待打包任务结束。
	_ = archiveReader.Close()
	tasks.Wait()
	if err != nil {
		return err
	}
	if code == 0 {
		return nil
	}
	// 5. 命令非零退出时优先返回非管道关闭的打包错误，否则返回退出码和 stderr。
	if archiveErr != nil && !errors.Is(archiveErr, io.ErrClosedPipe) {
		return fmt.Errorf("copy directory to %s: %w", targetPath, archiveErr)
	}
	return fmt.Errorf("copy directory to %s: tar exited with %d: %s", targetPath, code, strings.TrimSpace(stderr.String()))
}

// writeDirectoryArchive 按列表顺序写入 tar.gz，保留权限和修改时间。
func writeDirectoryArchive(ctx context.Context, source Workspace, sourcePath string, files []DirectoryFile, writer io.Writer) error {
	// 1. 创建 gzip 和 tar 写入端。
	compressor := gzip.NewWriter(writer)
	archive := tar.NewWriter(compressor)
	// 2. 逐项读取修改时间，目录写条目，文件写条目和正文。
	for _, file := range files {
		path := filepath.Join(sourcePath, file.RelativePath)
		metadata, err := source.GetMetadata(ctx, path, true)
		if err != nil {
			return err
		}
		if file.IsDirectory {
			header := &tar.Header{
				Typeflag: tar.TypeDir,
				Name:     file.RelativePath + "/",
				Mode:     int64(file.Mode),
				ModTime:  metadata.ModifiedAt,
			}
			if err := archive.WriteHeader(header); err != nil {
				return err
			}
			continue
		}
		reader, err := source.OpenFileReader(ctx, path)
		if err != nil {
			return err
		}
		header := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     file.RelativePath,
			Mode:     int64(file.Mode),
			Size:     metadata.Size,
			ModTime:  metadata.ModifiedAt,
		}
		err = archive.WriteHeader(header)
		if err == nil {
			_, err = io.Copy(archive, reader)
		}
		_ = reader.Close()
		if err != nil {
			return err
		}
	}
	// 3. 依次关闭 tar 和 gzip，返回关闭错误。
	return errors.Join(archive.Close(), compressor.Close())
}
