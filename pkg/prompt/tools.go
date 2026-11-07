package prompt

import (
	"fmt"
	"slices"
	"strings"

	"github.com/VaalaCat/openvaala/pkg/defs"
)

// 内置协作工具和工作区工具给模型看的用途说明。
const (
	SendMessageDescription = "向已知后代或直接父 Agent 发送任务、补充要求或信息。" +
		"normal 安排新 Turn，忙碌时排队；steer 补充活动 Turn，在下一次模型请求前读入，无可接收 Turn 时自动按 normal 安排。" +
		"进度汇报、提问和对当前任务的补充要求应显式设置 mode: \"steer\"；独立的后续任务用 normal。" +
		"返回 accepted 只表示已接收；steer 不会中断正在执行的模型或工具，需要中断时使用 interrupt_agent。" +
		"给后代发送时可以用 model、reasoning_effort 换它的模型，可选值见 spawn_agent 的 model 列表；不能改直接父方的模型。"
	InterruptAgentDescription = "请求取消指定后代当前正在执行的 Turn。" +
		"返回 cancel_requested 和选定的 turn_id，或无活动 Turn 时返回 no_active_turn；不等待取消完成。" +
		"排队输入和它的子任务继续运行；可通过 wait_agent 或 list_agents 查看后续结果与状态。"
	ListAgentsDescription = "使用空参数对象读取全部后代；sessions 中每项的 session 保存身份与父子关系，status 保存活动 Turn、排队数量和最近 Turn 状态，" +
		"usage 是已保存 Turn 的累计 token 用量，context_window 是该 Session 所选模型的上下文窗口。" +
		"使用 session.id 选择通信、等待或中断的目标；这是查询时的工作快照，不等待任务完成、不领取结果正文，也不启动历史任务。"
	ReadSessionHistoryDescription = "需要子任务的详细执行过程时，读取指定后代在 fork 之后自身已保存的消息、工具调用与结果。" +
		"从最新内容向更早历史分页，每页 Items 按时间正序返回；使用非零 next_cursor 继续，为 0 时结束。" +
		"返回资料省略加密推理、provider RawJSON 和图片 base64；查询不启动执行，也不影响结果通知。"
	WaitAgentDescription = "暂停当前工作，直到收到 steer 消息（Agent 消息或用户补充输入）、子任务结果通知或超时。normal 消息排队等待新 Turn，不会唤醒本次等待。" +
		"返回 reason：user_steer、subagent_result、agent_message 或 timeout（同时存在时按此顺序），from 为发送消息的 Agent SessionID；不返回正文。" +
		"消息正文会在下一次模型请求中读入；超时不会取消子任务，子任务每轮结束后的结果都会自动通知。"
	BashDescription = "在工作区后台启动一条 Bash 命令（省略 cwd 时在工作区根目录运行），最多等 wait_seconds 秒后返回 command_id、status 和从头开始的第一页输出（stdout、stderr 合并）。" +
		"wait_seconds 到期时命令不会停，status 为 running、exit_code 为 null，命令继续在后台运行；之后用 read_command_output 带上 next_cursor 续读，用 stop_command 停止。" +
		"命令结束后 status 为 exited 并给出 exit_code，退出码非 0 不算工具出错；无法确认命令结果时 status 为 failed，原因在 error。" +
		"timeout_seconds 从启动起计，到期停止命令，0 表示不限。" +
		"interactive 使用 PTY，之后可以用 write_stdin 输入。命令跨轮次保留，被中断后没拿到 command_id 时用 list_commands 找回。"
	ReadCommandOutputDescription = "读取 bash 启动的命令从 cursor 起的一页输出（最多 64 KiB）和命令当前的 status、exit_code。" +
		"已有输出时立即返回，否则最多等 wait_seconds 秒，直到有新输出或命令结束。读取不取走内容，同一个 cursor 可以重读；" +
		"用结果的 next_cursor 续读，has_more 为 true 表示还有已产生的输出没读完。每条命令只保留最后 1 MiB 输出，cursor 处的内容已被丢弃时从最早保留的位置读并标 truncated。"
	WriteStdinDescription = "把 chars 原样写进以 interactive=true 启动的命令的终端，不会自动加换行，需要回车时自己带上 \\n；普通命令的 stdin 是空的，不能写入。" +
		"返回实际写入的字节数，只写入一部分时不会自动重发。写入后用 read_command_output 查看命令的回显和输出。"
	StopCommandDescription = "终止 bash 启动的命令及其派生的整个进程组（不是发送 Ctrl-C），等到确认停止后返回最终 status、exit_code 和 stop_reason。" +
		"没能确认停止时返回错误，命令可能仍在运行，可以再试。"
	ListCommandsDescription = "按启动先后列出本 Session 在所选 Executor 上启动的命令，含 command_id、命令、工作目录、status 和 exit_code；" +
		"用来找回没拿到或忘记的 command_id。"
	ReadFileDescription = "按行读取 UTF-8 文本文件：offset 是从 1 开始的起始行号（默认 1），limit 是最多读取的行数（默认 200，最多 2000）。" +
		"content 中每行以“行号: ”开头，该前缀不属于原文件；相对路径以工作区为基准，也可使用绝对路径。" +
		"每页正文最多 64 KiB，按完整行返回；字节数超限时标 truncated，使用 next_offset 行号续读，eof 表示已到文件末尾。单行过长时报错，可用 bash 查看。" +
		"PNG、JPEG、GIF、WebP 图片会直接作为图片交给你查看，此时 offset、limit 不起作用。"
	ApplyPatchDescription = "使用 Begin Patch 格式新增、按完整行上下文更新或删除工作目录内的普通文件；结果列出实际修改的路径。"
	GlobDescription       = "按通配符列出工作区里的文件路径，不读文件内容：pattern 相对 path，例如 **/*.go、src/**/test_*.py；path 省略时从工作区根目录开始。" +
		"跳过 .git 和以 . 开头的文件、目录；在 git 仓库里还跳过 .gitignore 忽略的文件。结果按路径排序，最多 limit 条（默认 100，最多 1000），超出时 truncated 为 true。"
	GrepDescription = "用正则逐行搜索文件内容，返回 path、line（从 1 开始）和这一行的文本（超过 500 个字符截断）。" +
		"pattern 只用 rg 和 POSIX 扩展正则共有的写法：\\d、\\w、\\s 和它们的大写会自动换成等价的字符类；不支持 (?i) 这类内联标记，忽略大小写用 ignore_case。" +
		"glob 只搜路径匹配的文件，例如 *.go。跳过二进制文件，其余跳过规则同 glob。结果按路径和行号排序，最多 limit 条（默认 100，最多 1000）。"
)

// FormatSpawnAgentDescription 生成 spawn_agent 的说明，附上可选的 Agent 和模型列表。
func FormatSpawnAgentDescription(agents []AgentEntry, models []*defs.ModelDefinition) string {
	// 1. 写入怎么交代任务、继承历史和结果怎么通知。
	var description strings.Builder
	description.WriteString("将可独立执行的工作委派给一个直接子 Agent，子任务可以并行运行。" +
		"message 应提供目标、必要材料和预期交付物；默认不继承历史，需要时明确设置 fork_turns。" +
		"返回 session_id、task_name 和 accepted 表示首个任务已接收，不表示已经完成；子任务每个 Turn 结束后，结果以通知消息自动进入你的收件箱，在下一次模型请求前读入。" +
		"需要暂停等待时使用 wait_agent，它只在新消息到达时唤醒、不返回正文。")
	// 2. 按名字排序列出 Agent 和用途说明。
	description.WriteString("可选 agent_name（省略时与当前 Agent 相同）：")
	for _, definition := range slices.SortedFunc(slices.Values(agents), func(a, b AgentEntry) int {
		return strings.Compare(a.Name, b.Name)
	}) {
		description.WriteString("\n- " + definition.Name)
		if definition.Description != "" {
			description.WriteString("：" + definition.Description)
		}
	}
	// 3. 按模型引用排序列出窗口和可选的推理强度。
	description.WriteString("\n可选 model（省略时同一 Agent 继承你当前的模型，不同 Agent 使用它的默认模型）：")
	for _, model := range slices.SortedFunc(slices.Values(models), func(a, b *defs.ModelDefinition) int {
		return strings.Compare(a.Ref.String(), b.Ref.String())
	}) {
		fmt.Fprintf(&description, "\n- %s（%s）：窗口 %d", model.Ref, model.DisplayName, model.ContextWindow)
		if len(model.ReasoningEfforts) > 0 {
			fmt.Fprintf(&description, "，reasoning_effort 可选 %s，默认 %s", strings.Join(model.ReasoningEfforts, "|"), model.DefaultReasoningEffort)
		}
	}
	return description.String()
}

// FormatExecutorParameterDescription 生成工具 executor 参数的说明，写明默认 Executor。
func FormatExecutorParameterDescription(defaultExecutorID string) string {
	// 1. 指向系统提示里的 Executor 列表，写上省略时用的 Executor。
	return "执行此调用的 Executor，见系统提示 <openvaala-executors>；省略时使用 " + defaultExecutorID
}
