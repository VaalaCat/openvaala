// Package prompt 生成发给模型的内置提示文本：系统提示里的各个块、工具说明，以及插进对话的通知。
package prompt

import (
	"fmt"
	"slices"
	"strings"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/skill"
)

// local、内置和 memory Executor 给模型看的说明。
const (
	LocalExecutorDescription   = "Agent 本机当前目录"
	BuiltinExecutorDescription = "OpenVaala 内置的内存虚拟环境，可做临时工作，不影响真实磁盘；/skills 存放内置 skill；运行脚本用 bash <文件>，不能直接执行 ./脚本 或外部程序"
	MemoryExecutorDescription  = "用于跨对话记忆：记下用户的偏好、项目约定和验证过的经验，供以后的对话读取；user/ 所有项目通用，project/ 只属于当前项目；只能读写文件，不能运行命令"
)

// 内置 default Agent 的用途说明和系统指令；配置里没有 default Agent 时使用，agents/default.yaml 没写的字段也沿用它们。
const (
	DefaultAgentDescription  = "通用助手"
	DefaultAgentInstructions = "你是 OpenVaala 的通用助手。理解用户目标，在工作区里用可用的工具完成任务；回答准确、简洁，改动前先弄清现状。"
)

// FormatSessionIdentity 生成系统提示中的 Session 身份块。
func FormatSessionIdentity(session defs.Session) string {
	// 1. 写入 Session ID、任务名和父 Session ID。
	return formatXMLBlock("openvaala-session", fmt.Sprintf("SessionID: %s\nTaskName: %s\nParentSessionID: %s\n", session.ID, session.TaskName, session.ParentSessionID))
}

// FormatSkillInstructions 生成系统提示里的 skill 列表；没有 skill 时返回空串。
func FormatSkillInstructions(skills []*skill.Skill) string {
	// 1. 没有 skill 时直接返回。
	if len(skills) == 0 {
		return ""
	}
	// 2. 写入读取说明，按传入顺序列出名字、描述和路径，带 Executor 的也写上。
	var instructions strings.Builder
	instructions.WriteString("以下 skill 为特定任务提供说明。任务符合某项描述时，先用 read_file 完整读取它的 SKILL.md 再行动（条目带 executor 时，调用 read_file 传入同一 executor）；SKILL.md 中的相对路径以其所在目录为基准。\n")
	for _, listed := range skills {
		fmt.Fprintf(&instructions, "- %s: %s\n  path: %s\n", listed.Name, listed.Description, listed.Path)
		if listed.Executor != "" {
			fmt.Fprintf(&instructions, "  executor: %s\n", listed.Executor)
		}
	}
	return formatXMLBlock("openvaala-skills", instructions.String())
}

// FormatExecutorInstructions 生成系统提示里的 Executor 列表，第一项是默认 Executor；没有 Executor 时返回空串。
func FormatExecutorInstructions(executors []ExecutorEntry) string {
	// 1. 没有 Executor 时直接返回，否则记下第一项是默认的。
	if len(executors) == 0 {
		return ""
	}
	defaultID := executors[0].ID
	// 2. 写入 Executor 的用法和先读后改的提示。
	var instructions strings.Builder
	instructions.WriteString("Executor 是工具实际运行的环境（一台机器或一个虚拟环境），各自有独立的文件系统和命令行：读写文件和运行命令只作用于所选 Executor，不同 Executor 之间的文件互不可见。可在多个 Executor 上运行的工具带有 executor 参数，用它选择执行位置；省略时使用参数说明中的默认 Executor。\n")
	instructions.WriteString("修改文件前，先用 read_file 读取目标文件，收到结果后再修改。\n")
	// 3. 按 ID 排序列出说明和工作目录，标出默认的。
	for _, named := range slices.SortedFunc(slices.Values(executors), func(a, b ExecutorEntry) int {
		return strings.Compare(a.ID, b.ID)
	}) {
		label := named.ID
		if named.ID == defaultID {
			label += "（默认）"
		}
		fmt.Fprintf(&instructions, "- %s: %s\n  workspace: %s\n", label, named.Description, named.WorkspaceDirectory)
	}
	return formatXMLBlock("openvaala-executors", instructions.String())
}

// FormatMemoryInstructions 生成 memory 可用时追加到系统指令里的记忆规则。
func FormatMemoryInstructions() string {
	// 1. 逐条写入 memory Executor 的使用规则。
	var rules strings.Builder
	rules.WriteString("memory Executor 的使用规则：\n")
	rules.WriteString("每个目录里的 MEMORY.md 是目录：每个主题一行，写成「- [主题](文件名.md) — 一句能直接用的结论」，详情写在同一目录的主题文件里。\n")
	rules.WriteString("每轮开始时两份 MEMORY.md 会自动放进上下文，主题文件按需用 read_file 或 grep 读。\n")
	rules.WriteString("只记这几类：用户明确要求记住的、用户确认过的偏好和约定、验证过的操作经验（写明在什么条件下验证过）；不记任务进度、临时状态和自己的猜测。\n")
	rules.WriteString("写之前先读 MEMORY.md：同一主题就更新原来的文件和那一行，不要重复追加；内容过时或被用户否定时，改掉或删掉。MEMORY.md 超过 8 KiB 时，合并相近的主题。\n")
	rules.WriteString("这里只能读写文件，不能运行命令；用 apply_patch 修改，路径写相对路径，例如 project/testing.md。写入失败时要告诉用户没记住，不能说已经记下了。\n")
	return formatXMLBlock("openvaala-memory", rules.String())
}
