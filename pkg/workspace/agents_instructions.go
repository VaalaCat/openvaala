package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
)

// AgentsInstructionFile 是从工作区读到的一份 AGENTS.md。
type AgentsInstructionFile struct {
	// Path 是这份 AGENTS.md 清理后的绝对路径。
	Path string
	// Text 是文件全文。
	Text string
}

// GetWorkspaceAgentsInstructions 从工作区根目录往上找最近的 Git 根目录，按从 Git 根目录到工作区根目录的顺序读取沿途的 AGENTS.md。
func GetWorkspaceAgentsInstructions(ctx context.Context, work Workspace) ([]AgentsInstructionFile, error) {
	// 1. 从工作区根目录逐级往上查找 .git 文件或目录，没找到时以工作区根目录为起点；查询 .git 出错（不是不存在）时返回错误。
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
	// 2. 按 Git 根目录到工作区根目录的顺序读取规则。
	return getDirectoryAgentsInstructions(ctx, work, root, directory)
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
