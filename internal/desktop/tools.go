package desktop

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/VaalaCat/mygo-kit"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/egoist/mygo/ui"
)

// toolView 把工具调用画成一行 ActivityCard，展开后按内置工具类别显示参数、输出与来源链接。
func (d *Desktop) toolView(c *ui.Context, call *defs.ToolCall, result *defs.ToolResult, view viewItem) {
	// 1. CallID 对应的结果独立于展开与调试选择。
	key := "tool-" + call.CallID
	var args map[string]any
	_ = json.Unmarshal([]byte(call.Arguments), &args)
	var content map[string]any
	if result != nil {
		_ = json.Unmarshal(result.Content, &content)
	}
	status := d.getToolActivityStatus(call, result, view)
	opt := kit.ActivityCardOpt{Icon: getToolIcon(call.Name), Title: call.Name, Summary: getToolPreview(call.Name, call.Arguments), Status: status, Shimmer: status == kit.ActivityRunning}
	t := kit.GetTheme(c)
	// 仅在有值时绘制事实行。
	showFacts := func(facts []kit.Fact) {
		if len(facts) > 0 {
			kit.Facts(c, facts...)
		}
	}
	d.activityItem(c, key, view, opt, nil, func() {
		// 2. 专用展示保留全参数 JSON 作为独立调试入口。
		switch call.Name {
		case "bash", "read_command_output", "write_stdin", "stop_command":
			if call.Name == "bash" {
				d.outputView(c, d.t("run.command"), d.t("copy.command"), stringField(args, "command"))
			}
			if call.Name == "write_stdin" {
				d.outputView(c, d.t("input"), d.t("copy.output"), stringField(args, "chars"))
			}
			showFacts(append(getFacts(args, "cwd"), getFacts(content, "command_id", "status", "exit_code", "stop_reason", "written_bytes", "next_cursor")...))
			if failure := stringField(content, "error"); failure != "" {
				kit.FieldMessage(c, kit.MessageError, failure).Selectable()
			}
			output := stringField(content, "output")
			if result == nil {
				output = d.toolProgress[call.CallID]
			}
			if output != "" {
				d.ansiOutputView(c, output)
			}
			if content["has_more"] == true || content["truncated"] == true {
				d.warningView(c, fmt.Sprintf(d.t("output.truncated.lines"), countOutputLines(output)))
			}
		case "list_commands":
			if commands, ok := content["commands"].([]any); ok {
				for _, entry := range commands {
					command, ok := entry.(map[string]any)
					if !ok {
						continue
					}
					ui.Column(c.Key(stringField(command, "command_id"))).Gap(4).Children(func() {
						t.TextRole(ui.Text(c, stringField(command, "command_id")+" · "+stringField(command, "status")), kit.TextUI).TextColor(t.Colors.Text)
						t.TextRole(ui.Text(c, stringField(command, "command")).Selectable(), kit.TextCode)
						showFacts(getFacts(command, "cwd", "exit_code", "stop_reason", "interactive"))
					})
				}
			}
		case "read_file":
			t.TextRole(ui.Text(c, stringField(args, "path")).Selectable(), kit.TextCode)
			if text, ok := content["content"].(string); ok {
				d.outputView(c, d.t("file.content"), d.t("copy.output"), text)
			}
			showFacts(getFacts(content, "offset", "next_offset", "size", "eof"))
			if content["truncated"] == true {
				text, _ := content["content"].(string)
				d.warningView(c, fmt.Sprintf(d.t("output.truncated.lines"), countOutputLines(text)))
			}
		case "apply_patch":
			patch := stringField(args, "patch")
			d.diffView(c, patch)
			if content["outcome"] == "partially_applied" {
				d.warningView(c, d.t("partially.applied"))
				d.jsonView(c, "applied-files", d.t("applied.files.and.error"), content)
			}
		case "spawn_agent":
			d.markdownView(c, stringField(args, "message"), false)
			showFacts(getFacts(args, "agent_name", "model", "reasoning_effort", "fork_turns"))
			sessionID := stringField(content, "session_id")
			for _, info := range d.sessionInfos {
				if info.Session.ParentCallID == call.CallID {
					sessionID = string(info.Session.ID)
				}
			}
			d.sessionLink(c, defs.SessionID(sessionID)).AlignSelf(ui.Start)
		case "send_message":
			d.sessionLink(c, defs.SessionID(stringField(args, "session_id"))).AlignSelf(ui.Start)
			d.markdownView(c, stringField(args, "message"), false)
			showFacts(getFacts(args, "mode", "model"))
		case "wait_agent":
			ui.Text(c, stringField(content, "reason"))
			if from, ok := content["from"].([]any); ok {
				for _, id := range from {
					if value, ok := id.(string); ok {
						d.sessionLink(c, defs.SessionID(value)).AlignSelf(ui.Start)
					}
				}
			}
		case "interrupt_agent":
			d.sessionLink(c, defs.SessionID(stringField(args, "session_id"))).AlignSelf(ui.Start)
			ui.Text(c, stringField(content, "status"))
		case "list_agents":
			if sessions, ok := content["sessions"].([]any); ok {
				for _, entry := range sessions {
					value, _ := entry.(map[string]any)
					sessionValue, _ := value["session"].(map[string]any)
					d.sessionLink(c, defs.SessionID(stringField(sessionValue, "id"))).AlignSelf(ui.Start)
					d.jsonView(c, "session-"+stringField(sessionValue, "id"), stringField(sessionValue, "task_name"), value["status"])
				}
			}
		case "read_session_history":
			d.sessionLink(c, defs.SessionID(stringField(args, "session_id"))).AlignSelf(ui.Start)
			d.jsonView(c, "history", d.t("history"), content["items"])
		default:
			if result != nil {
				d.jsonView(c, "result", d.t("result"), result.Content)
			}
		}
		if result != nil {
			if result.Error != nil {
				kit.FieldMessage(c, kit.MessageError, result.Error.Message).Selectable()
			}
			if result.Truncated {
				d.warningView(c, d.t("tool.result.truncated"))
			}
			for _, image := range result.Images {
				if image != nil {
					d.imageView(c, image.DataURL, image.Alt, 200)
				}
			}
		}
		d.jsonView(c, "full-arguments", d.t("full.arguments"), json.RawMessage(call.Arguments))
	})
}

// getToolActivityStatus 由调用与结果事实得到标题行状态：生成参数和执行中为 running，无结果且 Turn 已结束为 interrupted，成功为空。
func (d *Desktop) getToolActivityStatus(call *defs.ToolCall, result *defs.ToolResult, view viewItem) kit.ActivityStatus {
	// 1. 有结果时按结果判断成败。
	if result != nil {
		if isToolFailed(call.Name, result) {
			return kit.ActivityFailed
		}
		return ""
	}
	// 2. 无结果时区分被中断与仍在执行。
	if call.Status == defs.ToolCallInterrupted {
		return kit.ActivityInterrupted
	}
	turnRunning := d.turns[view.Item.TurnID].Status == defs.TurnStatusRunning || view.Item.TurnID != "" && d.getSessionInfo().ActiveTurnID == view.Item.TurnID
	if call.Status == defs.ToolCallReady && !turnRunning {
		return kit.ActivityInterrupted
	}
	return kit.ActivityRunning
}

// applyToolProgress 将实时输出关联到真正 CallID，不创建伪工具结果。
func (d *Desktop) applyToolProgress(event *runtimeclient.ToolProgress) {
	// 1. 实时输出只属于对应工具的阅读附加数据。
	if d.toolProgress == nil {
		d.toolProgress = map[string]string{}
	}
	var output string
	if json.Unmarshal(event.Progress.Content, &output) == nil {
		d.toolProgress[event.Progress.CallID] += output
		return
	}
	d.toolProgress[event.Progress.CallID] += string(event.Progress.Content)
}

func stringField(value map[string]any, key string) string {
	result, _ := value[key].(string)
	return result
}

// isToolFailed 工具错误或命令非零退出是否构成失败？
func isToolFailed(name string, result *defs.ToolResult) bool {
	// 1. 命令主动 stopped 与失败退出区分，其他工具使用实际 ToolError。
	if result == nil {
		return false
	}
	if result.Error != nil {
		return true
	}
	if name != "bash" && name != "read_command_output" && name != "stop_command" {
		return false
	}
	var command struct {
		Status     string `json:"status"`
		ExitCode   *int   `json:"exit_code"`
		StopReason string `json:"stop_reason"`
	}
	if json.Unmarshal(result.Content, &command) != nil {
		return false
	}
	return command.Status == "failed" || command.ExitCode != nil && *command.ExitCode != 0 && command.StopReason != "stopped"
}
func patchSummary(patch string) string {
	parts := []string{}
	added, deleted := 0, 0
	for _, line := range strings.Split(patch, "\n") {
		if strings.HasPrefix(line, "*** Add File: ") || strings.HasPrefix(line, "*** Update File: ") || strings.HasPrefix(line, "*** Delete File: ") {
			parts = append(parts, strings.TrimPrefix(line, "*** "))
		} else if strings.HasPrefix(line, "+") {
			added++
		} else if strings.HasPrefix(line, "-") {
			deleted++
		}
	}
	return strings.Join(parts, ", ") + fmt.Sprintf(" +%d −%d", added, deleted)
}

// diffView 将工具补丁转换为真实片段行，不把 @@ 上下文猜成完整文件行号。
func (d *Desktop) diffView(c *ui.Context, patch string) {
	// 1. 只解析工具格式的行种类，复制保留原始补丁文本。
	lines := []kit.PatchLine{}
	for _, text := range strings.Split(patch, "\n") {
		line := kit.PatchLine{Kind: kit.PatchContext, Text: text}
		switch {
		case strings.HasPrefix(text, "***") || strings.HasPrefix(text, "@@"):
			line.Kind = kit.PatchHeader
		case strings.HasPrefix(text, "+"):
			line.Kind = kit.PatchAdded
			line.Text = text[1:]
		case strings.HasPrefix(text, "-"):
			line.Kind = kit.PatchDeleted
			line.Text = text[1:]
		case strings.HasPrefix(text, " "):
			line.Text = text[1:]
		}
		lines = append(lines, line)
	}
	t := kit.GetTheme(c)
	root := ui.Column(c)
	state := ui.Local(root, patchScrollKey{}, func() ui.ListState { return ui.ListState{} })
	root.Children(func() {
		kit.PatchLines(c, state, lines, kit.PatchOpt{Title: patchSummary(patch), CopyLabel: d.t("copy.patch"), Source: patch}).Height(getOutputHeight(patch, 360, &t)).MinHeight(80)
	})
}

// ansiOutputView 使用通用 ANSI 阅读器，流式命令只在阅读位置贴底时跟随。
func (d *Desktop) ansiOutputView(c *ui.Context, text string) {
	// 1. 同一工具输出保留原生滚动状态，不执行控制序列。
	t := kit.GetTheme(c)
	root := ui.Column(c)
	state := ui.Local(root, commandOutputScrollKey{}, func() ui.ListState { return ui.ListState{FollowEnd: true} })
	root.Children(func() {
		kit.ANSIOutput(c, text, state, kit.ANSIOutputOpt{Title: d.t("output"), CopyLabel: d.t("copy.output")}).Height(getOutputHeight(text, 320, &t)).MinHeight(60)
	})
}

// getToolIcon 返回工具调用卡片使用的图标。
func getToolIcon(name string) *ui.SVG {
	switch name {
	case "bash", "read_command_output", "write_stdin", "stop_command", "list_commands":
		return kit.IconTerminal
	case "read_file":
		return kit.IconFileText
	case "apply_patch":
		return kit.IconPencil
	case "spawn_agent", "send_message", "wait_agent", "interrupt_agent", "list_agents", "read_session_history":
		return kit.IconBot
	default:
		return kit.IconWrench
	}
}

// getToolPreview 返回工具调用的单行参数摘要；没有专用摘要的工具（MCP 工具、未知工具）取第一个字符串参数。
func getToolPreview(name string, arguments string) string {
	var args map[string]any
	_ = json.Unmarshal([]byte(arguments), &args)
	var preview string
	switch name {
	case "bash":
		preview = stringField(args, "command")
	case "read_file":
		preview = stringField(args, "path")
	case "apply_patch":
		preview = patchSummary(stringField(args, "patch"))
	case "spawn_agent":
		preview = stringField(args, "task_name")
	case "send_message":
		preview = stringField(args, "message")
	default:
		preview = getFirstStringArgument(arguments)
	}
	return strings.ReplaceAll(preview, "\n", " ")
}

// warningView 绘制一行带警告图标的提示，用于输出被截断、补丁部分应用这类不影响阅读的告警。
func (d *Desktop) warningView(c *ui.Context, message string) {
	// 1. 图标与文字同色，垂直居中。
	t := kit.GetTheme(c)
	ui.Row(c).Gap(t.Space(2)).AlignItems(ui.Center).Children(func() {
		ui.Icon(c, kit.IconTriangleAlert).Size(kit.IconSizeDefault, kit.IconSizeDefault).Shrink(0).TextColor(t.Colors.Warning)
		kit.FieldMessage(c, kit.MessageWarning, message)
	})
}
