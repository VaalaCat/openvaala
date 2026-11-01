package desktop

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/VaalaCat/mygo-kit"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

type rpcRow struct {
	Frame                  runtimeclient.RPCFrame
	Method, ConversationID string
	ID                     string // JSON-RPC 请求 ID，通知帧为空
	Latency                time.Duration
	IsError                bool
}

// loadRPCFrames 获取 Client 唯一持有的有界日志副本。
func (d *Desktop) loadRPCFrames() {
	if d.client == nil || d.rpcLoading {
		return
	}
	d.rpcLoading = true
	client := d.client
	d.tasks.Go(func() {
		frames, err := client.GetRPCFrames(d.ctx)
		d.window.Update(func() {
			if d.closed {
				return
			}
			d.rpcLoading = false
			if err != nil {
				d.detailsError = err
				return
			}
			d.rpcFrames, d.rpcRows = frames, pairRPCFrames(frames)
		})
	})
}

// rpcLogView 虚拟绘制筛选帧，详情和导出均使用脱敏数据。
func (d *Desktop) rpcLogView(c *ui.Context) {
	// 1. 工具栏两行各 36 高：第一行是两个筛选框，第二行是方向下拉加图标按钮，360 宽的 Dock 里输入框也不会被挤窄。
	t := kit.GetTheme(c)
	ui.Column(c).Shrink(0).Gap(0).BorderWidth(0, 0, 1, 0).BorderColor(t.Colors.BorderSubtle).Children(func() {
		ui.Row(c).Height(t.Metrics.HeaderHeight).Shrink(0).Padding(0, t.Space(3)).Gap(t.Space(2)).AlignItems(ui.Center).Children(func() {
			kit.TextInput(c.Key("rpc-method"), &d.rpcMethod).Label(d.t("rpc.method.filter")).Placeholder(d.t("method")).Grow(1).MinWidth(0)
			kit.TextInput(c.Key("rpc-conversation"), &d.rpcConversation).Label(d.t("rpc.conversation.filter")).Placeholder(d.t("conversation.id")).Grow(1).MinWidth(0)
		})
		ui.Row(c).Height(t.Metrics.HeaderHeight).Shrink(0).Padding(0, t.Space(1), 0, t.Space(3)).Gap(t.Space(1)).AlignItems(ui.Center).Children(func() {
			kit.Select(c.Key("rpc-direction"), &d.rpcDirection, kit.SelectOpt{Options: selectOptions([]string{"", "sent", "received"}, []string{d.t("all.directions"), d.t("rpc.sent"), d.t("rpc.received")}), Label: d.t("direction"), Placeholder: d.t("direction"), Bordered: true}).Changed()
			ui.Spacer(c)
			d.rpcActionsView(c)
		})
	})
	filtered := []rpcRow{}
	for _, row := range d.rpcRows {
		if d.rpcMethod != "" && !strings.Contains(strings.ToLower(row.Method), strings.ToLower(d.rpcMethod)) {
			continue
		}
		if d.rpcDirection != "" && string(row.Frame.Direction) != d.rpcDirection {
			continue
		}
		if d.rpcConversation != "" && row.ConversationID != d.rpcConversation {
			continue
		}
		filtered = append(filtered, row)
	}
	d.rpcList.Key = func(i int) any { return filtered[i].Frame.Sequence }
	// 2. 面板窄于 560 时每行只留方向、方法和时间；宽度取上一帧列表的实际宽度，首帧未知按窄处理。
	// 列表缓存已构建的行，宽窄切换时换列表键让它重建，否则缓存里的行会保留旧布局（方法名被挤成「…」）。
	isWide := d.rpcListWidth >= rpcWideWidth
	list := ui.List(c.Key(fmt.Sprintf("rpc-frames-%t", isWide)), &d.rpcList, len(filtered), func(i int) {
		row := filtered[i]
		d.rpcRowView(c, row, isWide)
		if d.rpcSelected == row.Frame.Sequence {
			ui.Column(c).Padding(t.Space(1), t.Space(3), t.Space(2), t.Space(3)).Children(func() { d.jsonView(c, row.Frame.Sequence, "RPC", maskRPCPayload(row.Frame.Payload)) })
		}
	}).Grow(1).Padding(t.Space(1), 0)
	d.rpcListWidth = list.Bounds().W
}

// rpcActionsView 绘制 RPC 工具栏第二行的图标按钮：暂停或继续记录、清空、导出；暂停和清空直接更新 Client。
func (d *Desktop) rpcActionsView(c *ui.Context) {
	t := kit.GetTheme(c)
	disabled := d.client == nil || d.clientStopped
	label, icon := d.t("pause"), kit.IconCirclePause
	if d.rpcPaused {
		label, icon = d.t("resume"), kit.IconPlay
	}
	pause := kit.IconButtonTip(c.Key("rpc-pause"), icon, label).Disabled(disabled)
	if d.rpcPaused {
		pause.TextColor(t.Colors.Accent)
	}
	if pause.Clicked() {
		paused, client := !d.rpcPaused, d.client
		d.tasks.Go(func() {
			err := client.SetRPCLogPaused(d.ctx, paused)
			d.window.Update(func() {
				if d.closed {
					return
				}
				if err != nil {
					d.detailsError = err
					return
				}
				d.rpcPaused = paused
			})
		})
	}
	if kit.IconButtonTip(c.Key("rpc-clear"), kit.IconTrash, d.t("clear")).Disabled(disabled).Clicked() {
		client := d.client
		d.tasks.Go(func() {
			err := client.ClearRPCFrames(d.ctx)
			d.window.Update(func() {
				if d.closed {
					return
				}
				if err != nil {
					d.detailsError = err
					return
				}
				d.rpcFrames, d.rpcRows = nil, nil
			})
		})
	}
	if kit.IconButtonTip(c.Key("rpc-export"), kit.IconDownload, d.t("export.jsonl")).Clicked() {
		d.exportRPC()
	}
}

// rpcWideWidth 是 RPC 行显示 ID、大小和耗时所需的最小面板宽度。
const rpcWideWidth = 560

// rpcRowView 绘制 20 高的 RPC 行：方向、方法、右侧时间；宽面板再加 #id、大小和耗时。
func (d *Desktop) rpcRowView(c *ui.Context, row rpcRow, wide bool) {
	// 1. 发送用强调色，接收用语法紫；出错帧的方法名用危险色。
	t := kit.GetTheme(c)
	label := fmt.Sprintf("%d %s %s · %d B", row.Frame.Sequence, row.Frame.Direction, row.Method, len(row.Frame.Payload))
	if row.Latency > 0 {
		label += fmt.Sprintf(" · %d ms", row.Latency.Milliseconds())
	}
	button := ui.ButtonBase(c.Key(row.Frame.Sequence)).Height(t.Space(5)).Padding(0, t.Space(3)).Gap(t.Space(2)).AlignItems(ui.Center).Radius(0).Label(label)
	t.TextRole(button, kit.TextCode)
	if d.rpcSelected == row.Frame.Sequence || button.Hovered() {
		button.Background(t.Colors.Hover)
	}
	button.Children(func() {
		glyph, glyphColor := "↑", t.Colors.Accent
		if row.Frame.Direction != runtimeclient.RPCSent {
			glyph, glyphColor = "↓", t.Syntax.Keyword
		}
		ui.Text(c, glyph).Width(t.Space(3)).Shrink(0).TextColor(glyphColor)
		method := t.Colors.Text
		if row.IsError {
			method = t.Colors.Danger
		}
		ui.Text(c, row.Method).SingleLine().Grow(1).Shrink(1).MinWidth(0).TextColor(method)
		muted := func(value string, width float32) {
			ui.Text(c, value).SingleLine().Shrink(0).Width(width).TextAlign(ui.End).TextColor(t.Colors.Placeholder)
		}
		muted(row.Frame.Time.Format("15:04:05.000"), t.Space(24))
		if !wide {
			return
		}
		id := ""
		if row.ID != "" {
			id = "#" + row.ID
		}
		muted(id, t.Space(12))
		muted(fmt.Sprintf("%d B", len(row.Frame.Payload)), t.Space(16))
		latency := ""
		if row.Latency > 0 {
			latency = fmt.Sprintf("%d ms", row.Latency.Milliseconds())
		}
		muted(latency, t.Space(16))
	})
	if button.Clicked() {
		d.rpcSelected = row.Frame.Sequence
	}
}

// pairRPCFrames 使用连接编号和请求 ID 配对，重连 ID 重用不串记录。
func pairRPCFrames(frames []runtimeclient.RPCFrame) []rpcRow {
	requests := map[string]rpcRow{}
	rows := make([]rpcRow, 0, len(frames))
	for _, frame := range frames {
		var payload struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				ConversationID string `json:"conversation_id"`
			} `json:"params"`
			Error json.RawMessage `json:"error"`
		}
		_ = json.Unmarshal(frame.Payload, &payload)
		row := rpcRow{Frame: frame, Method: payload.Method, ConversationID: payload.Params.ConversationID, ID: strings.Trim(string(payload.ID), `"`), IsError: len(payload.Error) > 0}
		key := fmt.Sprintf("%d:%s", frame.ConnectionID, payload.ID)
		if len(payload.ID) > 0 {
			if frame.Direction == runtimeclient.RPCSent {
				requests[key] = row
			} else if request, found := requests[key]; found {
				row.Method = request.Method
				row.ConversationID = request.ConversationID
				row.Latency = frame.Time.Sub(request.Frame.Time)
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// maskRPCPayload 递归遮罩凭据，保留错误和聊天内容原文。
func maskRPCPayload(data []byte) any {
	var value any
	if json.Unmarshal(data, &value) != nil {
		return string(data)
	}
	var mask func(any)
	mask = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			for key, entry := range value {
				lower := strings.ToLower(key)
				if lower == "api_key" || lower == "token" || lower == "authorization" {
					if text, ok := entry.(string); ok && text != "" {
						value[key] = "••••••"
					}
				} else {
					mask(entry)
				}
			}
		case []any:
			for _, entry := range value {
				mask(entry)
			}
		}
	}
	mask(value)
	return value
}

// exportRPC 由用户明确选择文件位置，取消不报保存失败。
func (d *Desktop) exportRPC() {
	// 1. 固定用户点击时的脱敏副本，再打开系统保存对话框。
	var data strings.Builder
	for _, frame := range d.rpcFrames {
		line, _ := json.Marshal(struct {
			ConnectionID, Sequence uint64
			Time                   time.Time
			Direction              runtimeclient.RPCDirection
			Payload                any
		}{frame.ConnectionID, frame.Sequence, frame.Time, frame.Direction, maskRPCPayload(frame.Payload)})
		data.Write(line)
		data.WriteByte('\n')
	}
	content := data.String()
	win := d.window
	d.tasks.Go(func() {
		path, err := mygo.Dialog.Save(mygo.SaveDialogOptions{Parent: win, DefaultPath: "openvaala-rpc.jsonl"})
		if err == nil && path != "" {
			err = os.WriteFile(path, []byte(content), 0600)
		}
		win.Update(func() {
			if !d.closed && err != nil {
				d.showFailure(err)
			}
		})
	})
}
