package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"

	"github.com/VaalaCat/openvaala/pkg/telemetry"
)

// AgentsInstructionFile 是从工作区读到的一份 AGENTS.md。
type AgentsInstructionFile struct {
	// Path 是这份 AGENTS.md 清理后的绝对路径。
	Path string
	// Text 是文件全文。
	Text string
	// IsGlobal 是否为用户主目录下、适用于本 Executor 全部工作区的规则？
	IsGlobal bool
}

// GetWorkspaceAgentsInstructions 先读取用户全局规则，再从最近的 Git 根目录到工作区根目录读取沿途规则；全局规则读取失败只记警告。
func GetWorkspaceAgentsInstructions(ctx context.Context, work Workspace) ([]AgentsInstructionFile, error) {
	// 1. 从工作区所在机器的用户主目录读取全局规则；不支持主目录或文件不存在时跳过，其他错误记警告。
	var instructions []AgentsInstructionFile
	homeDirectory, err := work.GetHomeDirectory(ctx)
	switch {
	case errors.Is(err, errors.ErrUnsupported):
	case err != nil:
		telemetry.GetLogger(ctx).Warn("global AGENTS.md home directory unavailable", "workspace", work.GetRootDirectory(), "error", err)
	default:
		path := filepath.Join(homeDirectory, ".agents", "AGENTS.md")
		text, err := GetFileText(ctx, work, path)
		switch {
		case err == nil:
			instructions = append(instructions, AgentsInstructionFile{
				Path:     path,
				Text:     text,
				IsGlobal: true,
			})
		case !errors.Is(err, fs.ErrNotExist):
			telemetry.GetLogger(ctx).Warn("global AGENTS.md read failed", "path", path, "error", err)
		}
	}
	// 2. 从工作区根目录逐级往上查找 .git 文件或目录，没找到时以工作区根目录为起点；查询 .git 出错（不是不存在）时返回错误。
	directory := filepath.Clean(work.GetRootDirectory())
	root := directory
	for parent := directory; ; parent = filepath.Dir(parent) {
		marker := filepath.Join(parent, ".git")
		metadata, err := work.GetMetadata(ctx, marker, true)
		if err == nil && (metadata.IsDirectory || metadata.IsFile) {
			root = parent
			break
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("inspect %s: %w", marker, err)
		}
		if filepath.Dir(parent) == parent {
			break
		}
	}
	// 3. 将沿途的目录规则排在全局规则之后。
	directoryInstructions, err := getDirectoryAgentsInstructions(ctx, work, root, directory)
	if err != nil {
		return nil, err
	}
	return append(instructions, directoryInstructions...), nil
}

// GetFileAgentsInstructions 按从工作区根目录到文件所在目录的顺序读取沿途的 AGENTS.md。
func GetFileAgentsInstructions(ctx context.Context, work Workspace, path string) ([]AgentsInstructionFile, error) {
	// 1. 展开并清理文件路径，所在目录在工作区外时返回空列表。
	root := filepath.Clean(work.GetRootDirectory())
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	path = filepath.Clean(path)
	if _, err := getRelativePath(root, filepath.Dir(path)); err != nil {
		return nil, nil
	}
	// 2. 读取工作区根目录到文件所在目录的规则。
	return getDirectoryAgentsInstructions(ctx, work, root, filepath.Dir(path))
}

// getDirectoryAgentsInstructions 按从 root 到 directory 的顺序读取每一层的 AGENTS.md。
func getDirectoryAgentsInstructions(ctx context.Context, work Workspace, root, directory string) ([]AgentsInstructionFile, error) {
	// 1. 收集 directory 到 root 的每一层目录，再反转成从 root 开始的顺序。
	var directories []string
	for parent := directory; ; parent = filepath.Dir(parent) {
		directories = append(directories, parent)
		if parent == root {
			break
		}
	}
	slices.Reverse(directories)
	// 2. 逐目录读取 AGENTS.md，跳过不存在的文件，其他错误立即返回。
	var instructions []AgentsInstructionFile
	for _, parent := range directories {
		path := filepath.Join(parent, "AGENTS.md")
		text, err := GetFileText(ctx, work, path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read AGENTS.md %s: %w", path, err)
		}
		instructions = append(instructions, AgentsInstructionFile{
			Path: path,
			Text: text,
		})
	}
	return instructions, nil
}
