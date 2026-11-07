package workspace_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/pkg/workspace"
)

// workspaceFixture 是写入了相同初始文件的工作区：根目录下有 note.txt（abcdef）与 sub/inner.txt（inner），
// 工作区外的 outside 目录下有 secret.txt（secret）；ssh 工作区经 sshServer 访问本机临时目录。
type workspaceFixture struct {
	workspace workspace.Workspace
	root      string
	outside   string
	sshServer *sshTestServer
}

var workspaceKinds = []string{"local", "virtual", "ssh"}

// unreadableSourceWorkspace 是打开文件总会失败的源工作区，其余能力照常，模拟打包时读不了源文件。
type unreadableSourceWorkspace struct {
	workspace.Workspace
}

// OpenFileReader 返回源文件不可读的测试错误。
func (unreadableSourceWorkspace) OpenFileReader(context.Context, string) (io.ReadCloser, error) {
	// 1. 直接返回预设读取错误。
	return nil, errors.New("source file is unreadable")
}

// newWorkspaceFixture 创建带相同初始文件的本机、虚拟或 SSH 工作区。
func newWorkspaceFixture(t *testing.T, kind string) *workspaceFixture {
	// 1. 准备公共初始文件，虚拟类型直接初始化内存文件系统。
	t.Helper()
	files := map[string]string{"note.txt": "abcdef", "sub/inner.txt": "inner"}
	if kind == "virtual" {
		virtualFiles := map[string]*workspace.VirtualFile{"/outside/secret.txt": {Content: []byte("secret")}}
		for relative, content := range files {
			virtualFiles["/workspace/"+relative] = &workspace.VirtualFile{Content: []byte(content)}
		}
		instance, err := workspace.NewVirtualWorkspace(workspace.NewVirtualWorkspaceOpt("", virtualFiles))
		if err != nil {
			t.Fatal(err)
		}
		return &workspaceFixture{
			workspace: instance,
			root:      "/workspace",
			outside:   "/outside",
		}
	}
	// 2. 本机和 SSH 类型创建临时目录及文件；SSH 类型启动测试服务端。
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	files["../outside/secret.txt"] = "secret"
	for relative, content := range files {
		path := filepath.Join(root, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if kind == "ssh" {
		server := newSSHTestServer(t)
		instance, err := workspace.NewSSHWorkspace(workspace.NewSSHWorkspaceOpt(root, "tester@"+server.address, server.identityFile))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = instance.Close() })
		return &workspaceFixture{
			workspace: instance,
			root:      root,
			outside:   outside,
			sshServer: server,
		}
	}
	instance, err := workspace.NewLocalWorkspace(workspace.NewLocalWorkspaceOpt(root))
	if err != nil {
		t.Fatal(err)
	}
	return &workspaceFixture{
		workspace: instance,
		root:      root,
		outside:   outside,
	}
}

// readText 读取测试文件的文本。
func readText(t *testing.T, f *workspaceFixture, path string) string {
	// 1. 调用 ReadFile 读取前 1024 字节，失败时结束测试。
	t.Helper()
	content, err := f.workspace.ReadFile(t.Context(), path, 0, 1024)
	if err != nil {
		t.Fatal(err)
	}
	return string(content.Content)
}

// TestWorkspaceFiles 验证三类工作区的文件读写、目录和元数据。
func TestWorkspaceFiles(t *testing.T) {
	// 1. 逐项在各类工作区执行分页、写入、越界、目录及删除用例。
	for _, tt := range []struct {
		name string
		run  func(t *testing.T, f *workspaceFixture)
	}{
		{
			name: "read_range",
			run: func(t *testing.T, f *workspaceFixture) {
				content, err := f.workspace.ReadFile(t.Context(), f.root+"/note.txt", 1, 3)
				if err != nil || string(content.Content) != "bcd" || content.Size != 6 || content.EOF {
					t.Fatalf("content = %+v, %v", content, err)
				}
			},
		},
		{
			name: "read_tail",
			run: func(t *testing.T, f *workspaceFixture) {
				content, err := f.workspace.ReadFile(t.Context(), f.root+"/note.txt", 4, 10)
				if err != nil || string(content.Content) != "ef" || !content.EOF {
					t.Fatalf("content = %+v, %v", content, err)
				}
			},
		},
		{
			name: "read_outside",
			run: func(t *testing.T, f *workspaceFixture) {
				if got := readText(t, f, f.outside+"/secret.txt"); got != "secret" {
					t.Fatalf("content = %q", got)
				}
			},
		},
		{
			name: "read_directory_rejected",
			run: func(t *testing.T, f *workspaceFixture) {
				if _, err := f.workspace.ReadFile(t.Context(), f.root+"/sub", 0, 10); !errors.Is(err, workspace.ErrNotRegularFile) {
					t.Fatalf("err = %v", err)
				}
			},
		},
		{
			name: "write_new_file",
			run: func(t *testing.T, f *workspaceFixture) {
				if err := f.workspace.CreateDirectory(t.Context(), f.root+"/new/deep"); err != nil {
					t.Fatal(err)
				}
				if err := f.workspace.WriteFile(t.Context(), f.root+"/new/deep/x.txt", []byte("x"), 0o644, true); err != nil {
					t.Fatal(err)
				}
				if got := readText(t, f, f.root+"/new/deep/x.txt"); got != "x" {
					t.Fatalf("content = %q", got)
				}
			},
		},
		{
			name: "create_existing_root_directory",
			run: func(t *testing.T, f *workspaceFixture) {
				if err := f.workspace.CreateDirectory(t.Context(), f.root); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "create_only_existing",
			run: func(t *testing.T, f *workspaceFixture) {
				err := f.workspace.WriteFile(t.Context(), f.root+"/note.txt", []byte("z"), 0o644, true)
				if !errors.Is(err, fs.ErrExist) || readText(t, f, f.root+"/note.txt") != "abcdef" {
					t.Fatalf("err = %v", err)
				}
			},
		},
		{
			name: "overwrite",
			run: func(t *testing.T, f *workspaceFixture) {
				if err := f.workspace.WriteFile(t.Context(), f.root+"/note.txt", []byte("z"), 0o644, false); err != nil {
					t.Fatal(err)
				}
				if got := readText(t, f, f.root+"/note.txt"); got != "z" {
					t.Fatalf("content = %q", got)
				}
			},
		},
		{
			name: "write_outside_rejected",
			run: func(t *testing.T, f *workspaceFixture) {
				err := f.workspace.WriteFile(t.Context(), f.outside+"/x.txt", []byte("x"), 0o644, false)
				if !errors.Is(err, workspace.ErrOutsideWorkspace) {
					t.Fatalf("err = %v", err)
				}
			},
		},
		{
			name: "write_parent_escape_rejected",
			run: func(t *testing.T, f *workspaceFixture) {
				err := f.workspace.WriteFile(t.Context(), f.root+"/../escape.txt", []byte("x"), 0o644, false)
				if !errors.Is(err, workspace.ErrOutsideWorkspace) {
					t.Fatalf("err = %v", err)
				}
			},
		},
		{
			name: "metadata",
			run: func(t *testing.T, f *workspaceFixture) {
				note, err := f.workspace.GetMetadata(t.Context(), f.root+"/note.txt", false)
				if err != nil || !note.IsFile || note.IsDirectory || note.Size != 6 {
					t.Fatalf("note = %+v, %v", note, err)
				}
				sub, err := f.workspace.GetMetadata(t.Context(), f.root+"/sub", true)
				if err != nil || !sub.IsDirectory {
					t.Fatalf("sub = %+v, %v", sub, err)
				}
				if _, err := f.workspace.GetMetadata(t.Context(), f.root+"/missing", true); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("missing err = %v", err)
				}
			},
		},
		{
			name: "read_directory",
			run: func(t *testing.T, f *workspaceFixture) {
				entries, err := f.workspace.ReadDirectory(t.Context(), f.root)
				if err != nil {
					t.Fatal(err)
				}
				var got []string
				for _, entry := range entries {
					got = append(got, entry.Name+map[bool]string{true: "/", false: ""}[entry.IsDirectory])
				}
				if !slices.Equal(got, []string{"note.txt", "sub/"}) {
					t.Fatalf("entries = %v", got)
				}
			},
		},
		{
			name: "remove",
			run: func(t *testing.T, f *workspaceFixture) {
				if err := f.workspace.Remove(t.Context(), f.root+"/note.txt"); err != nil {
					t.Fatal(err)
				}
				if _, err := f.workspace.GetMetadata(t.Context(), f.root+"/note.txt", false); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("err = %v", err)
				}
			},
		},
	} {
		for _, kind := range workspaceKinds {
			t.Run(kind+"/"+tt.name, func(t *testing.T) {
				tt.run(t, newWorkspaceFixture(t, kind))
			})
		}
	}
}

// createStreamFile 通过写入端创建文件，并检查关闭结果。
func createStreamFile(t *testing.T, f *workspaceFixture, path, content string, mode fs.FileMode) {
	// 1. 打开写入端，写入全文并关闭。
	t.Helper()
	writer, err := f.workspace.CreateFileWriter(t.Context(), path, mode)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
}

// readStreamFile 通过读取端读取文件全文。
func readStreamFile(t *testing.T, f *workspaceFixture, path string) string {
	// 1. 打开文件，读取全部内容后关闭。
	t.Helper()
	reader, err := f.workspace.OpenFileReader(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	content, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

// requireFileMode 核对文件权限位。
func requireFileMode(t *testing.T, f *workspaceFixture, path string, want fs.FileMode) {
	// 1. 读取元数据并比较期望权限。
	t.Helper()
	metadata, err := f.workspace.GetMetadata(t.Context(), path, true)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Mode != want {
		t.Fatalf("mode of %s = %o, want %o", path, metadata.Mode, want)
	}
}

// TestWorkspaceCopyFile 验证流式文件读写及跨工作区复制。
func TestWorkspaceCopyFile(t *testing.T) {
	// 1. 准备大文件文本，在三类工作区核对流式读写、相对路径和权限。
	content := "#!/bin/sh\n" + strings.Repeat("echo hook script line\n", 50000)
	for _, tt := range []struct {
		name string
		run  func(t *testing.T, f *workspaceFixture)
	}{
		{
			name: "write_outside_root_and_read_back",
			run: func(t *testing.T, f *workspaceFixture) {
				path := f.outside + "/a/b/script.sh"
				createStreamFile(t, f, path, content, 0o755)
				if got := readStreamFile(t, f, path); got != content {
					t.Fatalf("content length = %d, want %d", len(got), len(content))
				}
				requireFileMode(t, f, path, 0o755)
			},
		},
		{
			name: "overwrite_existing_file",
			run: func(t *testing.T, f *workspaceFixture) {
				createStreamFile(t, f, f.root+"/note.txt", "z", 0o600)
				if got := readStreamFile(t, f, f.root+"/note.txt"); got != "z" {
					t.Fatalf("content = %q", got)
				}
			},
		},
		{
			name: "relative_path_resolves_against_root",
			run: func(t *testing.T, f *workspaceFixture) {
				createStreamFile(t, f, "hooks/run.sh", "relative", 0o700)
				if got := readStreamFile(t, f, f.root+"/hooks/run.sh"); got != "relative" {
					t.Fatalf("absolute read = %q", got)
				}
				if got := readStreamFile(t, f, "hooks/run.sh"); got != "relative" {
					t.Fatalf("relative read = %q", got)
				}
			},
		},
		{
			name: "read_missing",
			run: func(t *testing.T, f *workspaceFixture) {
				if _, err := f.workspace.OpenFileReader(t.Context(), f.root+"/missing.sh"); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("err = %v", err)
				}
			},
		},
	} {
		for _, kind := range workspaceKinds {
			t.Run(kind+"/"+tt.name, func(t *testing.T) {
				tt.run(t, newWorkspaceFixture(t, kind))
			})
		}
	}

	for _, tt := range []struct {
		name       string
		sourceKind string
		targetKind string
	}{
		{
			name:       "local_to_virtual",
			sourceKind: "local",
			targetKind: "virtual",
		},
		{
			name:       "local_to_ssh",
			sourceKind: "local",
			targetKind: "ssh",
		},
		{
			name:       "virtual_to_local",
			sourceKind: "virtual",
			targetKind: "local",
		},
	} {
		t.Run("copy/"+tt.name, func(t *testing.T) {
			source := newWorkspaceFixture(t, tt.sourceKind)
			target := newWorkspaceFixture(t, tt.targetKind)
			if err := source.workspace.WriteFile(t.Context(), source.root+"/tool.sh", []byte(content), 0o750, false); err != nil {
				t.Fatal(err)
			}
			targetPath := target.outside + "/copied/tool.sh"
			if err := workspace.CopyFile(t.Context(), source.workspace, source.root+"/tool.sh", target.workspace, targetPath); err != nil {
				t.Fatal(err)
			}
			if got := readStreamFile(t, target, targetPath); got != content {
				t.Fatalf("content length = %d, want %d", len(got), len(content))
			}
			requireFileMode(t, target, targetPath, 0o750)
		})
	}
	t.Run("copy/source_is_directory", func(t *testing.T) {
		source := newWorkspaceFixture(t, "local")
		target := newWorkspaceFixture(t, "virtual")
		err := workspace.CopyFile(t.Context(), source.workspace, source.root+"/sub", target.workspace, target.root+"/copied")
		if !errors.Is(err, workspace.ErrNotRegularFile) {
			t.Fatalf("err = %v", err)
		}
		if _, err := target.workspace.GetMetadata(t.Context(), target.root+"/copied", true); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("target created for a directory source: %v", err)
		}
	})
}

// sampleDirectoryFiles 是 writeSampleDirectory 写入的普通文件。
var sampleDirectoryFiles = []struct {
	relativePath string
	content      string
	mode         fs.FileMode
}{
	{
		relativePath: "a.txt",
		content:      "alpha",
		mode:         0o644,
	},
	{
		relativePath: "bin/run.sh",
		content:      "#!/bin/sh\necho run\n",
		mode:         0o755,
	},
	{
		relativePath: ".git/HEAD",
		content:      "ref: refs/heads/main\n",
		mode:         0o644,
	},
}

// writeSampleDirectory 写入样例文件并创建文件、目录和循环符号链接。
func writeSampleDirectory(t *testing.T, directory string) {
	// 1. 创建样例目录和文件，设置各自权限。
	t.Helper()
	for _, file := range sampleDirectoryFiles {
		path := filepath.Join(directory, file.relativePath)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(file.content), file.mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, file.mode); err != nil {
			t.Fatal(err)
		}
	}
	// 2. 创建指向文件、目录及自身的符号链接。
	for name, target := range map[string]string{"link-file": "a.txt", "link-dir": "bin", "loop": "."} {
		if err := os.Symlink(target, filepath.Join(directory, name)); err != nil {
			t.Fatal(err)
		}
	}
}

// requireNotExist 核对路径不存在，不跟随符号链接。
func requireNotExist(t *testing.T, f *workspaceFixture, path string) {
	// 1. 读取目标元数据，要求返回 fs.ErrNotExist。
	t.Helper()
	if _, err := f.workspace.GetMetadata(t.Context(), path, false); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s exists or cannot be checked: %v", path, err)
	}
}

// TestListDirectoryFiles 验证目录遍历的顺序、跳过名单和符号链接。
func TestListDirectoryFiles(t *testing.T) {
	// 1. 逐项写入样例目录，核对目录列表和权限。
	for _, tt := range []struct {
		name         string
		skippedNames []string
		want         []workspace.DirectoryFile
	}{
		{
			name:         "skip_git",
			skippedNames: []string{".git"},
			want: []workspace.DirectoryFile{
				{
					RelativePath: "a.txt",
					Mode:         0o644,
				},
				{
					RelativePath: "bin",
					Mode:         0o755,
					IsDirectory:  true,
				},
				{
					RelativePath: "bin/run.sh",
					Mode:         0o755,
				},
				{
					RelativePath: "link-file",
					Mode:         0o644,
				},
			},
		},
		{
			name: "keep_all",
			want: []workspace.DirectoryFile{
				{
					RelativePath: ".git",
					Mode:         0o755,
					IsDirectory:  true,
				},
				{
					RelativePath: ".git/HEAD",
					Mode:         0o644,
				},
				{
					RelativePath: "a.txt",
					Mode:         0o644,
				},
				{
					RelativePath: "bin",
					Mode:         0o755,
					IsDirectory:  true,
				},
				{
					RelativePath: "bin/run.sh",
					Mode:         0o755,
				},
				{
					RelativePath: "link-file",
					Mode:         0o644,
				},
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeSampleDirectory(t, root)
			got, err := workspace.ListDirectoryFiles(t.Context(), newWorkspaceFixture(t, "local").workspace, root, tt.skippedNames)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("files = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestWorkspaceCopyDirectory 验证目录复制、目标保留及打包或解包错误。
func TestWorkspaceCopyDirectory(t *testing.T) {
	// 1. 分别向本机和 SSH 工作区复制样例目录，核对文件、权限和错误。
	for _, tt := range []struct {
		name string
		run  func(t *testing.T, source workspace.Workspace, sourcePath string, f *workspaceFixture)
	}{
		{
			name: "copies_tree",
			run: func(t *testing.T, source workspace.Workspace, sourcePath string, f *workspaceFixture) {
				targetPath := f.outside + "/it's dir/plugin"
				if err := workspace.CopyDirectory(t.Context(), source, sourcePath, f.workspace, targetPath, []string{".git"}); err != nil {
					t.Fatal(err)
				}
				for _, file := range sampleDirectoryFiles {
					if file.relativePath == ".git/HEAD" {
						continue
					}
					path := targetPath + "/" + file.relativePath
					if got := readStreamFile(t, f, path); got != file.content {
						t.Fatalf("content of %s = %q, want %q", file.relativePath, got, file.content)
					}
					requireFileMode(t, f, path, file.mode)
				}
				requireFileMode(t, f, targetPath+"/bin", 0o755)
				linkFile, err := f.workspace.GetMetadata(t.Context(), targetPath+"/link-file", false)
				if err != nil || !linkFile.IsFile || linkFile.IsSymlink || linkFile.Mode != 0o644 {
					t.Fatalf("link-file = %+v, %v", linkFile, err)
				}
				if got := readStreamFile(t, f, targetPath+"/link-file"); got != "alpha" {
					t.Fatalf("content of link-file = %q", got)
				}
				for _, name := range []string{".git", "link-dir", "loop"} {
					requireNotExist(t, f, targetPath+"/"+name)
				}
				requireNotExist(t, f, targetPath+".partial")
			},
		},
		{
			name: "existing_target_is_kept",
			run: func(t *testing.T, source workspace.Workspace, sourcePath string, f *workspaceFixture) {
				targetPath := f.outside + "/plugin"
				if err := os.MkdirAll(targetPath, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(targetPath+"/marker", []byte("kept"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := workspace.CopyDirectory(t.Context(), source, sourcePath+"/missing", f.workspace, targetPath, nil); err != nil {
					t.Fatal(err)
				}
				if got := readStreamFile(t, f, targetPath+"/marker"); got != "kept" {
					t.Fatalf("marker = %q", got)
				}
			},
		},
		{
			name: "leftover_partial_is_replaced",
			run: func(t *testing.T, source workspace.Workspace, sourcePath string, f *workspaceFixture) {
				targetPath := f.outside + "/plugin"
				if err := os.MkdirAll(targetPath+".partial", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(targetPath+".partial/junk", []byte("junk"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := workspace.CopyDirectory(t.Context(), source, sourcePath, f.workspace, targetPath, []string{".git"}); err != nil {
					t.Fatal(err)
				}
				if got := readStreamFile(t, f, targetPath+"/a.txt"); got != "alpha" {
					t.Fatalf("content of a.txt = %q", got)
				}
				requireNotExist(t, f, targetPath+"/junk")
				requireNotExist(t, f, targetPath+".partial")
			},
		},
		{
			name: "extract_failure_leaves_no_target",
			run: func(t *testing.T, source workspace.Workspace, sourcePath string, f *workspaceFixture) {
				// 写入压缩后仍超过管道缓冲的大文件，模拟解包命令不读输入就退出。
				large := make([]byte, 4<<20)
				_, _ = rand.Read(large)
				if err := os.WriteFile(sourcePath+"/large.bin", large, 0o644); err != nil {
					t.Fatal(err)
				}
				parentPath := f.outside + "/regular-file"
				if err := os.WriteFile(parentPath, []byte("file"), 0o644); err != nil {
					t.Fatal(err)
				}
				targetPath := parentPath + "/plugin"
				err := workspace.CopyDirectory(t.Context(), source, sourcePath, f.workspace, targetPath, []string{".git"})
				if err == nil || !strings.Contains(err.Error(), "tar exited") {
					t.Fatalf("err = %v", err)
				}
				if _, err := os.Lstat(targetPath); err == nil {
					t.Fatalf("target %s exists after a failed copy", targetPath)
				}
			},
		},
		{
			name: "archive_failure_reports_its_error",
			run: func(t *testing.T, source workspace.Workspace, sourcePath string, f *workspaceFixture) {
				targetPath := f.outside + "/plugin"
				err := workspace.CopyDirectory(t.Context(), unreadableSourceWorkspace{source}, sourcePath, f.workspace, targetPath, []string{".git"})
				if err == nil || err.Error() != "copy directory to "+targetPath+": source file is unreadable" {
					t.Fatalf("err = %v", err)
				}
				requireNotExist(t, f, targetPath)
			},
		},
	} {
		for _, kind := range []string{"local", "ssh"} {
			t.Run(kind+"/"+tt.name, func(t *testing.T) {
				sourcePath := t.TempDir()
				writeSampleDirectory(t, sourcePath)
				tt.run(t, newWorkspaceFixture(t, "local").workspace, sourcePath, newWorkspaceFixture(t, kind))
			})
		}
	}
}

// TestWorkspaceGetHomeDirectory 验证本机和虚拟工作区的主目录查询。
func TestWorkspaceGetHomeDirectory(t *testing.T) {
	// 1. 取得本机主目录，分别核对查询结果和不支持错误。
	localHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		kind    string
		want    string
		wantErr error
	}{
		{
			kind: "local",
			want: localHome,
		},
		{
			kind:    "virtual",
			wantErr: errors.ErrUnsupported,
		},
	} {
		t.Run(tt.kind, func(t *testing.T) {
			got, err := newWorkspaceFixture(t, tt.kind).workspace.GetHomeDirectory(t.Context())
			if !errors.Is(err, tt.wantErr) || got != tt.want {
				t.Fatalf("GetHomeDirectory = %q, %v; want %q, %v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

// TestWorkspaceDial 验证本机拨号成功及虚拟工作区不支持拨号。
func TestWorkspaceDial(t *testing.T) {
	// 1. 启动 HTTP 服务，通过各工作区拨号并核对响应或错误。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()
	for _, tt := range []struct {
		kind       string
		wantStatus int
		wantErr    error
	}{
		{
			kind:       "local",
			wantStatus: http.StatusOK,
		},
		{
			kind:    "virtual",
			wantErr: errors.ErrUnsupported,
		},
	} {
		t.Run(tt.kind, func(t *testing.T) {
			f := newWorkspaceFixture(t, tt.kind)
			transport := &http.Transport{DialContext: f.workspace.Dial}
			defer transport.CloseIdleConnections()
			response, err := (&http.Client{Transport: transport}).Get("http://" + server.Listener.Addr().String())
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			defer response.Body.Close()
			if response.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", response.StatusCode, tt.wantStatus)
			}
		})
	}
}

// TestLocalWorkspaceSymlinkEscape 验证本机写入不能通过符号链接越出工作区。
func TestLocalWorkspaceSymlinkEscape(t *testing.T) {
	// 1. 创建指向工作区外的链接并核对链接元数据。
	f := newWorkspaceFixture(t, "local")
	if err := os.Symlink(f.outside, filepath.Join(f.root, "link")); err != nil {
		t.Fatal(err)
	}
	metadata, err := f.workspace.GetMetadata(t.Context(), f.root+"/link", false)
	if err != nil || !metadata.IsSymlink {
		t.Fatalf("metadata = %+v, %v", metadata, err)
	}
	// 2. 尝试经链接写入，核对操作失败且工作区外未创建文件。
	if err := f.workspace.WriteFile(t.Context(), f.root+"/link/x.txt", []byte("x"), 0o644, false); err == nil {
		t.Fatal("write through symlink escaped the workspace")
	}
	if _, err := os.Stat(filepath.Join(f.outside, "x.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("outside file err = %v", err)
	}
}

// runProcess 启动命令，可按参数立即终止，再等待结果。
func runProcess(t *testing.T, f *workspaceFixture, request *workspace.ProcessRequest, terminate bool) (int, error) {
	// 1. 启动命令，按 terminate 选择结束命令，最多等待五秒。
	t.Helper()
	process, err := f.workspace.StartProcess(t.Context(), request)
	if err != nil {
		return 0, err
	}
	if terminate {
		if err := process.Terminate(); err != nil {
			t.Fatal(err)
		}
	}
	return waitProcess(t, process)
}

// processExit 是一次 Process.Wait 的结果。
type processExit struct {
	code int
	err  error
}

// waitInBackground 后台等待命令结束并交回退出结果。
func waitInBackground(process workspace.Process) <-chan processExit {
	// 1. 启动 Wait，将退出码和错误写入缓冲通道。
	exited := make(chan processExit, 1)
	go func() {
		code, err := process.Wait()
		exited <- processExit{
			code: code,
			err:  err,
		}
	}()
	return exited
}

// waitProcess 等待测试命令退出。
func waitProcess(t *testing.T, process workspace.Process) (int, error) {
	// 1. 最多等待后台退出结果五秒，超时后调用 Terminate 并结束测试。
	t.Helper()
	select {
	case exit := <-waitInBackground(process):
		return exit.code, exit.err
	case <-time.After(5 * time.Second):
		_ = process.Terminate()
		t.Fatal("process did not finish")
		return 0, nil
	}
}

// writeTerminalInput 写入测试命令的终端并等待结果。
func writeTerminalInput(t *testing.T, process workspace.Process, data []byte) (int, error) {
	// 1. 后台写入字节，最多等待五秒取得字节数及错误，超时则结束测试。
	t.Helper()
	type writeResult struct {
		written int
		err     error
	}
	finished := make(chan writeResult, 1)
	go func() {
		written, err := process.WriteTerminalInput(data)
		finished <- writeResult{
			written: written,
			err:     err,
		}
	}()
	select {
	case result := <-finished:
		return result.written, result.err
	case <-time.After(5 * time.Second):
		t.Fatal("WriteTerminalInput did not return")
		return 0, nil
	}
}

// TestWorkspaceProcess 验证三类工作区的命令输入输出、目录、TTY 和终止。
func TestWorkspaceProcess(t *testing.T) {
	// 1. 逐项在各类工作区运行命令，核对输出、退出码和错误。
	for _, tt := range []struct {
		name       string
		command    string
		directory  string // 相对根目录
		stdin      string
		tty        bool
		terminate  bool
		wantStdout string
		wantStderr string
		wantCode   int
		wantErr    map[string]error // kind → 期望的启动或等待错误
	}{
		{
			name:       "output_and_exit",
			command:    "printf out; printf err >&2; exit 3",
			wantStdout: "out",
			wantStderr: "err",
			wantCode:   3,
		},
		{
			name:       "working_directory",
			command:    "cat inner.txt",
			directory:  "sub",
			wantStdout: "inner",
		},
		{
			name:       "stdin",
			command:    "cat",
			stdin:      "hello",
			wantStdout: "hello",
		},
		{
			name:      "terminate",
			command:   "sleep 30",
			terminate: true,
			wantCode:  -1,
		},
		{
			name:       "tty",
			command:    "printf t",
			tty:        true,
			wantStdout: "t",
			wantErr:    map[string]error{"virtual": workspace.ErrTTYUnsupported},
		},
	} {
		for _, kind := range workspaceKinds {
			t.Run(kind+"/"+tt.name, func(t *testing.T) {
				f := newWorkspaceFixture(t, kind)
				var stdout, stderr bytes.Buffer
				request := &workspace.ProcessRequest{
					Command:   tt.command,
					Directory: f.root + "/" + tt.directory,
					TTY:       tt.tty,
					Stdout:    &stdout,
					Stderr:    &stderr,
				}
				if tt.stdin != "" {
					request.Stdin = strings.NewReader(tt.stdin)
				}
				code, err := runProcess(t, f, request, tt.terminate)
				if wantErr := tt.wantErr[kind]; wantErr != nil {
					if !errors.Is(err, wantErr) {
						t.Fatalf("err = %v, want %v", err, wantErr)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if code != tt.wantCode || !strings.Contains(stdout.String(), tt.wantStdout) || stderr.String() != tt.wantStderr {
					t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
				}
			})
		}
	}
}

// TestLocalTerminalInput 验证本机 TTY 输入、非 TTY 拒绝及退出后写入。
func TestLocalTerminalInput(t *testing.T) {
	// 1. 逐项启动命令，在指定时机写入输入并核对输出和错误。
	for _, tt := range []struct {
		name    string
		command string
		tty     bool
		stdin   io.Reader
		// input 非空时写进终端。
		input string
		// isWriteAfterExit 是否等 Wait 返回后才写？
		isWriteAfterExit bool
		// isWriteRejected 写入是否应该失败？wantWriteErr 非 nil 时还要求是这个错误；不应失败时写入要完整成功。
		isWriteRejected bool
		wantWriteErr    error
		wantStartErr    error
		wantOutput      string
	}{
		{
			name:       "stdin_is_terminal",
			command:    "test -t 0 && echo tty",
			tty:        true,
			wantOutput: "tty",
		},
		{
			name:       "write_reaches_command",
			command:    "read line; echo got:$line",
			tty:        true,
			input:      "hi\n",
			wantOutput: "got:hi",
		},
		{
			name:            "non_tty_write_rejected",
			command:         "true",
			input:           "hi\n",
			isWriteRejected: true,
			wantWriteErr:    workspace.ErrTTYInputUnsupported,
		},
		{
			name:         "tty_with_stdin_rejected",
			command:      "true",
			tty:          true,
			stdin:        strings.NewReader("hi\n"),
			wantStartErr: workspace.ErrTTYStdinUnsupported,
		},
		{
			name:             "write_after_exit_fails",
			command:          "true",
			tty:              true,
			input:            "hi\n",
			isWriteAfterExit: true,
			isWriteRejected:  true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newWorkspaceFixture(t, "local")
			var output bytes.Buffer
			process, err := f.workspace.StartProcess(t.Context(), &workspace.ProcessRequest{
				Command:   tt.command,
				Directory: f.root,
				TTY:       tt.tty,
				Stdin:     tt.stdin,
				Stdout:    &output,
			})
			if tt.wantStartErr != nil {
				if !errors.Is(err, tt.wantStartErr) {
					t.Fatalf("StartProcess err = %v, want %v", err, tt.wantStartErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var written int
			var writeErr error
			if tt.input != "" && !tt.isWriteAfterExit {
				written, writeErr = writeTerminalInput(t, process, []byte(tt.input))
			}
			code, err := waitProcess(t, process)
			if err != nil || code != 0 || !strings.Contains(output.String(), tt.wantOutput) {
				t.Fatalf("code = %d, err = %v, output = %q", code, err, output.String())
			}
			if tt.isWriteAfterExit {
				written, writeErr = writeTerminalInput(t, process, []byte(tt.input))
			}
			switch {
			case tt.input == "":
			case tt.isWriteRejected && (writeErr == nil || (tt.wantWriteErr != nil && !errors.Is(writeErr, tt.wantWriteErr))):
				t.Fatalf("WriteTerminalInput = %d, %v; want rejected with %v", written, writeErr, tt.wantWriteErr)
			case !tt.isWriteRejected && (writeErr != nil || written != len(tt.input)):
				t.Fatalf("WriteTerminalInput = %d, %v; want %d, nil", written, writeErr, len(tt.input))
			}
		})
	}
}

// TestTerminalSize 验证本机和 SSH 的 TTY 命令按请求设置初始尺寸和终端环境变量，ResizeTerminal 后程序读到新尺寸，非 TTY 命令拒绝修改。
func TestTerminalSize(t *testing.T) {
	// 1. 逐项启动命令，按需在命令读到输入前修改尺寸，再放行命令并核对输出。
	for _, kind := range []string{"local", "ssh"} {
		for _, tt := range []struct {
			name    string
			command string
			tty     bool
			rows    uint16
			cols    uint16
			// resizeRows、resizeCols 非零时在放行命令前调用 ResizeTerminal。
			resizeRows   uint16
			resizeCols   uint16
			wantOutput   string
			wantResizeEr error
		}{
			{
				name:       "requested_initial_size",
				command:    "read line; stty size",
				tty:        true,
				rows:       40,
				cols:       120,
				wantOutput: "40 120",
			},
			{
				name:       "default_size_when_zero",
				command:    "read line; stty size",
				tty:        true,
				wantOutput: "24 80",
			},
			{
				name: "resize_changes_size",
				// SSH 的 window-change 请求与输入数据不保证先后，程序轮询到新尺寸再输出。
				command:    `read line; for i in $(seq 100); do size=$(stty size); [ "$size" = "30 100" ] && break; sleep 0.05; done; echo $size`,
				tty:        true,
				rows:       24,
				cols:       80,
				resizeRows: 30,
				resizeCols: 100,
				wantOutput: "30 100",
			},
			{
				name:       "terminal_environment",
				command:    "read line; echo $TERM $COLORTERM",
				tty:        true,
				wantOutput: "xterm-256color truecolor",
			},
			{
				name:         "non_tty_resize_rejected",
				command:      "read line || true",
				resizeRows:   30,
				resizeCols:   100,
				wantResizeEr: workspace.ErrTTYInputUnsupported,
			},
		} {
			t.Run(kind+"/"+tt.name, func(t *testing.T) {
				f := newWorkspaceFixture(t, kind)
				var output bytes.Buffer
				stdin, stdinWriter := io.Pipe()
				request := &workspace.ProcessRequest{
					Command:   tt.command,
					Directory: f.root,
					TTY:       tt.tty,
					Rows:      tt.rows,
					Cols:      tt.cols,
					Stdout:    &output,
				}
				if !tt.tty {
					request.Stdin = stdin
				}
				process, err := f.workspace.StartProcess(t.Context(), request)
				if err != nil {
					t.Fatal(err)
				}
				if tt.resizeRows != 0 {
					if err := process.ResizeTerminal(tt.resizeRows, tt.resizeCols); !errors.Is(err, tt.wantResizeEr) {
						t.Fatalf("ResizeTerminal = %v, want %v", err, tt.wantResizeEr)
					}
				}
				if tt.tty {
					if written, err := writeTerminalInput(t, process, []byte("go\n")); err != nil || written != 3 {
						t.Fatalf("WriteTerminalInput = %d, %v", written, err)
					}
				} else {
					_ = stdinWriter.Close()
				}
				code, err := waitProcess(t, process)
				if err != nil || code != 0 || !strings.Contains(output.String(), tt.wantOutput) {
					t.Fatalf("code = %d, err = %v, output = %q, want it to contain %q", code, err, output.String(), tt.wantOutput)
				}
			})
		}
	}
}

// TestLocalTerminalResizeAtExit 验证本机 TTY 命令结束的同时不断改尺寸：命令结束前改尺寸成功或返回错误，结束后返回错误，-race 下不报数据竞争。
func TestLocalTerminalResizeAtExit(t *testing.T) {
	// 1. 逐项启动命令，命令运行期间在另一个 goroutine 里持续改尺寸，Wait 返回后再改一次。
	for _, tt := range []struct {
		name    string
		command string
	}{
		{name: "resize_while_command_exits", command: "sleep 0.2"},
		{name: "command_exits_without_output", command: "true"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newWorkspaceFixture(t, "local")
			process, err := f.workspace.StartProcess(t.Context(), &workspace.ProcessRequest{
				Command:   tt.command,
				Directory: f.root,
				TTY:       true,
			})
			if err != nil {
				t.Fatal(err)
			}
			stopResizing, resizingDone := make(chan struct{}), make(chan struct{})
			go func() {
				defer close(resizingDone)
				for rows := uint16(30); ; rows++ {
					select {
					case <-stopResizing:
						return
					default:
						_ = process.ResizeTerminal(rows%40+10, 100)
					}
				}
			}()
			code, err := waitProcess(t, process)
			close(stopResizing)
			<-resizingDone
			if err != nil || code != 0 {
				t.Fatalf("code = %d, err = %v", code, err)
			}
			if err := process.ResizeTerminal(30, 100); err == nil {
				t.Fatal("ResizeTerminal after exit = nil, want an error")
			}
		})
	}
}

// TestLocalTerminate 验证本机终止和关闭会结束整个命令进程组。
func TestLocalTerminate(t *testing.T) {
	// 1. 分别启动普通命令和 TTY 命令，读取后台进程 PID 后终止并核对已退出。
	for _, tt := range []struct {
		name string
		tty  bool
		// isClose 是否用 Close 代替 Terminate 结束命令？
		isClose bool
	}{
		{name: "process_group"},
		{
			name: "tty_session",
			tty:  true,
		},
		{
			name:    "close_ends_process_group",
			isClose: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newWorkspaceFixture(t, "local")
			process, err := f.workspace.StartProcess(t.Context(), &workspace.ProcessRequest{
				Command:   "sh -c 'sleep 30 & echo $! > child.pid; sleep 30'",
				Directory: f.root,
				TTY:       tt.tty,
			})
			if err != nil {
				t.Fatal(err)
			}
			var childPID int
			waitForCondition(t, 5*time.Second, func() bool {
				content, err := os.ReadFile(filepath.Join(f.root, "child.pid"))
				childPID, _ = strconv.Atoi(strings.TrimSpace(string(content)))
				return err == nil && childPID > 0
			})
			stop := process.Terminate
			if tt.isClose {
				stop = process.Close
			}
			if err := stop(); err != nil {
				t.Fatalf("stop = %v", err)
			}
			if code, err := waitProcess(t, process); code != -1 || err != nil {
				t.Fatalf("code = %d, err = %v", code, err)
			}
			// 后台派生的 sleep 和命令在同一个进程组，一起结束。
			waitForCondition(t, 2*time.Second, func() bool { return isProcessGone(childPID) })
			if err := process.Terminate(); err != nil {
				t.Fatalf("Terminate after exit = %v", err)
			}
		})
	}
}

// TestLocalWorkspaceEnvironment 验证命令自定义环境变量及默认继承。
func TestLocalWorkspaceEnvironment(t *testing.T) {
	// 1. 准备自定义和继承当前进程环境的用例。
	tests := []struct {
		name        string
		environment []string
		parentMark  string // 测试进程里 TOOL_ENV_MARK 的值，Environment 为 nil 时应被继承
		want        string
	}{
		{
			name:        "custom_environment",
			environment: []string{"TOOL_ENV_MARK=x"},
			parentMark:  "parent",
			want:        "x",
		},
		{
			name:       "inherit_process_environment",
			parentMark: "parent",
			want:       "parent",
		},
	}
	// 2. 运行命令读取环境变量，核对覆盖或继承后的值。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TOOL_ENV_MARK", tt.parentMark)
			root := t.TempDir()
			instance, err := workspace.NewLocalWorkspace(workspace.NewLocalWorkspaceOpt(root).WithEnvironment(tt.environment))
			if err != nil {
				t.Fatal(err)
			}
			var stdout bytes.Buffer
			f := &workspaceFixture{
				workspace: instance,
				root:      root,
			}
			code, err := runProcess(t, f, &workspace.ProcessRequest{
				Command:   "echo $TOOL_ENV_MARK",
				Directory: root,
				Stdout:    &stdout,
				Stderr:    &bytes.Buffer{},
			}, false)
			if err != nil || code != 0 || strings.TrimSpace(stdout.String()) != tt.want {
				t.Fatalf("code=%d err=%v stdout=%q, want %q", code, err, stdout.String(), tt.want)
			}
		})
	}
}

// TestVirtualWorkspaceLazyFile 验证虚拟文件只在首次读取时加载一次。
func TestVirtualWorkspaceLazyFile(t *testing.T) {
	// 1. 创建记录加载次数的延迟文件。
	var loads atomic.Int32
	instance, err := workspace.NewVirtualWorkspace(workspace.NewVirtualWorkspaceOpt("/workspace", map[string]*workspace.VirtualFile{
		"/skills/demo/SKILL.md": {Load: func(context.Context) ([]byte, error) {
			loads.Add(1)
			return []byte("lazy body"), nil
		}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	// 2. 连续读取两次并核对内容，加载函数只执行一次。
	for range 2 {
		content, err := instance.ReadFile(t.Context(), "/skills/demo/SKILL.md", 0, 100)
		if err != nil || string(content.Content) != "lazy body" {
			t.Fatalf("content = %+v, %v", content, err)
		}
	}
	if loads.Load() != 1 {
		t.Fatalf("loads = %d, want 1", loads.Load())
	}
}
