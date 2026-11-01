package tools

import (
	"context"
	"crypto/sha256"
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/prompt"
	"github.com/VaalaCat/openvaala/pkg/workspace"
)

// applyPatchToolImpl 是 apply_patch 工具：按 Begin Patch 格式在工作区里新增、更新或删除文件。
type applyPatchToolImpl struct {
	*executor.BaseTool[applyPatchArguments]

	opt *ApplyPatchToolOpt
}

// applyPatchArguments 是 apply_patch 的调用参数。
type applyPatchArguments struct {
	Patch string `json:"patch" jsonschema:"description=Begin Patch 格式的文件修改,minLength=1,maxLength=4194304"`
}

// applyPatchResult 是 apply_patch 的结果正文：实际新增、更新、删除的路径；没有完整应用时带 Problem。
type applyPatchResult struct {
	Outcome patchOutcome `json:"outcome"`
	Added   []string     `json:"added"`
	Updated []string     `json:"updated"`
	Deleted []string     `json:"deleted"`
	// ChangeStartLines 是每个更新文件里，各段改动在原文件中的起始行号（从 1 开始），顺序和补丁里的段一致；只包含真正改成功的文件。
	ChangeStartLines map[string][]int   `json:"change_start_lines,omitempty"`
	Problem          *applyPatchProblem `json:"problem,omitempty"`
}

// PatchFileKind 是补丁对一个文件的操作。
type PatchFileKind string

const (
	PatchFileAdd    PatchFileKind = "add"
	PatchFileUpdate PatchFileKind = "update"
	PatchFileDelete PatchFileKind = "delete"
)

// PatchFileChange 是补丁中一个文件的操作、增删行数和内容；删除整个文件时补丁里只有路径，行数为 0，没有内容和改动段。
type PatchFileChange struct {
	Path         string
	Kind         PatchFileKind
	AddedLines   int
	DeletedLines int
	// Content 是新增文件的内容，每行以换行结尾；更新和删除为空。
	Content string
	// Sections 是更新文件的各段改动，顺序和补丁一致；新增和删除为空。
	Sections []PatchSection
}

// PatchSection 是更新文件里的一段改动。
type PatchSection struct {
	// Header 是补丁里这一段的 "@@" 行（"@@" 或 "@@ <定位文字>"）。
	Header string
	// Lines 是这一段的上下文、删除、新增行，分别以 " "、"-"、"+" 开头，不含换行。
	Lines []string
}

// GetPatchFileChanges 解析补丁，按补丁顺序返回每个文件的操作、增删行数和内容。
func GetPatchFileChanges(patch string) ([]PatchFileChange, error) {
	// 1. 解析并校验补丁及相对路径。
	changes, err := parsePatchChanges(patch)
	if err != nil {
		return nil, err
	}
	// 2. 按文件操作整理增删行数、新增内容和更新段，保留补丁顺序。
	fileChanges := make([]PatchFileChange, len(changes))
	for index, change := range changes {
		fileChange := PatchFileChange{Path: change.path}
		switch change.kind {
		case patchAdd:
			fileChange.Kind, fileChange.AddedLines = PatchFileAdd, len(change.content)
			var content strings.Builder
			for _, line := range change.content {
				content.WriteString(line + "\n")
			}
			fileChange.Content = content.String()
		case patchUpdate:
			fileChange.Kind = PatchFileUpdate
			for _, section := range change.sections {
				fileChange.AddedLines += section.addedLineCount
				fileChange.DeletedLines += section.deletedLineCount
				fileChange.Sections = append(fileChange.Sections, PatchSection{
					Header: section.diffLines[0],
					Lines:  slices.Clone(section.diffLines[1:]),
				})
			}
		case patchDelete:
			fileChange.Kind = PatchFileDelete
		}
		fileChanges[index] = fileChange
	}
	return fileChanges, nil
}

// applyPatchProblem 是补丁没有完整应用时的问题。
type applyPatchProblem struct {
	Code patchProblemCode `json:"code"`
}

// ApplyPatchToolOpt 保存补丁工具使用的工作区。
type ApplyPatchToolOpt struct {
	// Workspace 是应用补丁的工作区；补丁路径以它的根目录为基准，只能修改工作区内的文件。
	Workspace workspace.Workspace
}

// NewApplyPatchToolOpt 保存应用补丁的工作区。
func NewApplyPatchToolOpt(toolWorkspace workspace.Workspace) *ApplyPatchToolOpt {
	// 1. 创建绑定工作区的配置。
	return &ApplyPatchToolOpt{Workspace: toolWorkspace}
}

// NewApplyPatchTool 创建工作区中的补丁工具。
func NewApplyPatchTool(opt *ApplyPatchToolOpt) (executor.Tool, error) {
	// 1. 校验工作区，生成补丁参数定义。
	if opt == nil || opt.Workspace == nil {
		return nil, errors.New("apply_patch requires a workspace")
	}
	baseTool, err := executor.NewBaseTool[applyPatchArguments](&executor.BaseToolOpt{
		Name:        "apply_patch",
		Description: prompt.ApplyPatchDescription,
	})
	if err != nil {
		return nil, err
	}

	return &applyPatchToolImpl{
		BaseTool: baseTool,
		opt:      NewApplyPatchToolOpt(opt.Workspace),
	}, nil
}

// Execute 解析并应用补丁，返回改动路径或错误。
func (t *applyPatchToolImpl) Execute(ctx context.Context, call *defs.Item, _ chan<- *executor.ExecutionProgressEvent) *executor.ToolExecutionResult {
	// 1. 解析调用参数和补丁，检查调用是否取消。
	callID := call.ToolCall.CallID
	arguments, err := t.ParseArguments(call.ToolCall.Arguments)
	if err != nil {
		return newInvalidArgumentsResult(callID, err)
	}
	changes, err := parsePatchChanges(arguments.Patch)
	if err != nil {
		return newInvalidArgumentsResult(callID, err)
	}
	if err := ctx.Err(); err != nil {
		return encodeContextError(callID, err)
	}
	// 2. 准备并提交变更；取消时返回 canceled 或 timeout，准备失败或第一项提交失败时返回 patch_failed。
	result, err := t.apply(ctx, changes)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return encodeContextError(callID, err)
	}
	if err != nil {
		return encodeToolResult(callID, nil, &defs.ToolError{
			Code:    "patch_failed",
			Message: err.Error(),
		})
	}
	// 3. 返回实际改动路径；部分成功时附带问题错误码。
	var toolError *defs.ToolError
	if result.Problem != nil {
		toolError = &defs.ToolError{
			Code:    string(result.Problem.Code),
			Message: "patch did not finish cleanly; see changed paths in result",
		}
	}
	return encodeToolResult(callID, result, toolError)
}

// apply 准备全部文件变更，再按顺序提交。
func (t *applyPatchToolImpl) apply(ctx context.Context, changes []*patchChange) (*applyPatchResult, error) {
	// 1. 读取和校验全部变更，成功后提交。
	prepared, err := t.preparePatch(ctx, changes)
	if err != nil {
		return nil, err
	}
	return t.commitPatch(ctx, prepared)
}

// patchOutcome 是补丁的应用结果：全部应用或只应用了一部分。
type patchOutcome string

// patchProblemCode 是补丁没有完整应用的原因。
type patchProblemCode string

// patchKind 是补丁对一个文件的操作：新增、更新或删除。
type patchKind string

const (
	patchOutcomeApplied          patchOutcome     = "applied"
	patchOutcomePartiallyApplied patchOutcome     = "partially_applied"
	patchProblemCommitFailed     patchProblemCode = "commit_failed"
)

const (
	patchAdd    patchKind = "add"
	patchUpdate patchKind = "update"
	patchDelete patchKind = "delete"
)

// apply_patch 的上限：补丁字节数、文件数、更新段总数、单个文件字节数和全部准备内容的字节数。
const (
	maxPatchBytes       = 4 * 1024 * 1024
	maxPatchChanges     = 128
	maxPatchSections    = 1024
	maxPatchedFileBytes = 16 * 1024 * 1024
	maxPreparedBytes    = 16 * 1024 * 1024
)

var (
	// ErrInvalidPatch 表示 apply_patch 输入不是受支持的 Begin/Add/Update/Delete/End patch 格式。
	ErrInvalidPatch = errors.New("invalid local workspace patch")
	// ErrPatchRejected 表示补丁格式有效，但目标状态、文件大小或完整行上下文不符合要求。
	ErrPatchRejected = errors.New("local workspace patch rejected")
)

// preparePatch 准备全部文件的变更内容。
func (t *applyPatchToolImpl) preparePatch(ctx context.Context, changes []*patchChange) ([]*preparedChange, error) {
	// 1. 逐项准备变更，检查取消和准备内容的总大小上限，任一项失败时返回错误。
	prepared := make([]*preparedChange, 0, len(changes))
	preparedBytes := 0
	for _, change := range changes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		preparedChange, err := t.prepareChange(ctx, change)
		if err != nil {
			return nil, err
		}
		if len(preparedChange.content) > maxPreparedBytes-preparedBytes {
			return nil, ErrPatchRejected
		}
		preparedBytes += len(preparedChange.content)
		prepared = append(prepared, preparedChange)
	}
	return prepared, nil
}

// prepareChange 读取单个目标文件，生成待提交的内容和原文件信息。
func (t *applyPatchToolImpl) prepareChange(ctx context.Context, change *patchChange) (*preparedChange, error) {
	// 1. 读取目标元数据，按新增、更新或删除准备变更。
	fullPath := filepath.Join(t.opt.Workspace.GetRootDirectory(), change.path)
	metadata, statErr := t.opt.Workspace.GetMetadata(ctx, fullPath, false)
	switch change.kind {
	case patchAdd:
		// 2. 新增要求目标不存在，按补丁内容生成文件正文。
		if !errors.Is(statErr, fs.ErrNotExist) {
			return nil, ErrPatchRejected
		}
		content := []byte{}
		if len(change.content) > 0 {
			content = []byte(strings.Join(change.content, "\n") + "\n")
		}
		return &preparedChange{
			kind:     patchAdd,
			path:     change.path,
			fullPath: fullPath,
			content:  content,
			mode:     0o644,
		}, nil
	case patchUpdate, patchDelete:
		// 3. 更新和删除要求目标为普通文件，不跟随符号链接；读取正文并记录大小、时间和摘要。
		if statErr != nil || !metadata.IsFile {
			return nil, ErrPatchRejected
		}
		content, err := t.readPatchFile(ctx, fullPath, metadata)
		if err != nil {
			return nil, err
		}
		prepared := &preparedChange{
			kind:       change.kind,
			path:       change.path,
			fullPath:   fullPath,
			mode:       metadata.Mode,
			size:       metadata.Size,
			modifiedAt: metadata.ModifiedAt,
			digest:     sha256.Sum256(content),
		}
		if change.kind == patchDelete {
			return prepared, nil
		}
		// 4. 更新文件时应用各段补丁，保存新内容和改动起始行号。
		updated, changeStartLines, err := applySections(string(content), change.sections)
		if err != nil {
			return nil, err
		}
		prepared.content, prepared.changeStartLines = []byte(updated), changeStartLines
		return prepared, nil
	default:
		return nil, ErrInvalidPatch
	}
}

// readPatchFile 读取普通文件全文并校验长度。
func (t *applyPatchToolImpl) readPatchFile(ctx context.Context, fullPath string, metadata *workspace.FileMetadata) ([]byte, error) {
	// 1. 拒绝超限文件，读取全文并检查 EOF 和元数据大小。
	if metadata.Size > maxPatchedFileBytes {
		return nil, ErrPatchRejected
	}
	content, err := t.opt.Workspace.ReadFile(ctx, fullPath, 0, maxPatchedFileBytes+1)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, ErrPatchRejected
	}
	if !content.EOF || int64(len(content.Content)) != metadata.Size {
		return nil, ErrPatchRejected
	}
	return content.Content, nil
}

// commitPatch 依次提交变更并返回实际改动路径。
func (t *applyPatchToolImpl) commitPatch(ctx context.Context, changes []*preparedChange) (*applyPatchResult, error) {
	// 1. 逐项检查取消并提交；第一项失败返回错误，之后失败返回 partially_applied。
	result := &applyPatchResult{Outcome: patchOutcomeApplied}
	for index, change := range changes {
		err := ctx.Err()
		if err == nil {
			err = t.commitChange(ctx, change)
		}
		if err != nil {
			if index == 0 {
				return nil, err
			}
			result.Outcome = patchOutcomePartiallyApplied
			result.Problem = &applyPatchProblem{Code: patchProblemCommitFailed}
			return result, nil
		}
		switch change.kind {
		case patchAdd:
			result.Added = append(result.Added, change.path)
		case patchUpdate:
			result.Updated = append(result.Updated, change.path)
			if result.ChangeStartLines == nil {
				result.ChangeStartLines = make(map[string][]int)
			}
			result.ChangeStartLines[change.path] = change.changeStartLines
		case patchDelete:
			result.Deleted = append(result.Deleted, change.path)
		}
	}
	return result, nil
}

// commitChange 提交一个已准备的文件变更。
func (t *applyPatchToolImpl) commitChange(ctx context.Context, change *preparedChange) error {
	// 1. 新增时创建父目录并只写新文件；更新和删除先复核原文件，再写入或删除。
	switch change.kind {
	case patchAdd:
		if err := t.opt.Workspace.CreateDirectory(ctx, filepath.Dir(change.fullPath)); err != nil {
			return err
		}
		return t.opt.Workspace.WriteFile(ctx, change.fullPath, change.content, change.mode, true)
	case patchUpdate:
		if err := t.verifyPreparedChange(ctx, change); err != nil {
			return err
		}
		return t.opt.Workspace.WriteFile(ctx, change.fullPath, change.content, change.mode, false)
	default:
		if err := t.verifyPreparedChange(ctx, change); err != nil {
			return err
		}
		return t.opt.Workspace.Remove(ctx, change.fullPath)
	}
}

// verifyPreparedChange 核对目标文件是否与准备时一致。
func (t *applyPatchToolImpl) verifyPreparedChange(ctx context.Context, change *preparedChange) error {
	// 1. 比较文件类型、大小和修改时间。
	metadata, err := t.opt.Workspace.GetMetadata(ctx, change.fullPath, false)
	if err != nil || !metadata.IsFile || metadata.Size != change.size || !metadata.ModifiedAt.Equal(change.modifiedAt) {
		return ErrPatchRejected
	}
	// 2. 重新读取内容并比较 SHA-256 摘要。
	content, err := t.readPatchFile(ctx, change.fullPath, metadata)
	if err != nil {
		return err
	}
	if sha256.Sum256(content) != change.digest {
		return ErrPatchRejected
	}
	return nil
}
