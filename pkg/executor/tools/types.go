package tools

import (
	"crypto/sha256"
	"os"
	"time"
)

// ── ApplyPatch 解析与文件准备 ──

// patchReplacement 是原文件里要替换的一段：start、end 是字节区间，text 是换上的新文本。
type patchReplacement struct {
	start int
	end   int
	text  string
}

// patchSection 是补丁里一个文件的一段改动：连续的增删行和前后的上下文。
type patchSection struct {
	oldLines []string
	newLines []string
	// diffLines 是补丁里本段的原始行，从 "@@" 行开始，含上下文行、"+"、"-" 行；GetPatchFileChanges 用它还原每个文件的 diff。
	diffLines []string
	// addedLineCount、deletedLineCount 是本段以 "+"、"-" 开头的行数，上下文行不计。
	addedLineCount   int
	deletedLineCount int
}

// patchChange 是补丁对一个文件的操作和内容。
type patchChange struct {
	kind patchKind
	path string
	// content 是新增文件的各行，不含换行；更新和删除为空。
	content []string
	// sections 是更新文件的各段改动；新增和删除为空。
	sections []*patchSection
}

// preparedChange 是准备好待提交的一个文件变更：新内容，以及读原文件时记下的大小、修改时间和摘要。
type preparedChange struct {
	kind       patchKind
	path       string
	fullPath   string
	content    []byte
	mode       os.FileMode
	size       int64
	modifiedAt time.Time
	digest     [sha256.Size]byte
	// changeStartLines 是更新文件里各段改动在原文件中的起始行号（从 1 开始），顺序和补丁里的段一致；新增和删除为空。
	changeStartLines []int
}
