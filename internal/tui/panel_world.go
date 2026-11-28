package tui

import (
	"cmp"
	"context"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/world"
)

const (
	worldStatusInterval = 30 * time.Second
	worldRequestTimeout = 5 * time.Second
)

// worldPanel 保存联系人列表、待发送内容和转发的对话。
type worldPanel struct {
	conversationID defs.ConversationID
	fetch          panelFetch
	list           listCursor
	recipients     []worldRecipient
	message        *composer
}

type worldRecipient struct {
	address string
	name    string
	online  bool
}

// worldStatusLoadedMsg 由状态请求完成时发出，序号对应本次运行时连接。
type worldStatusLoadedMsg struct {
	seq    int
	status world.NodeStatus
	err    error
}

// resetWorldStatus 清理上一条运行时连接的状态探测和联系人面板。
func (m *tuiModel) resetWorldStatus() {
	if m.cancelWorldStatus != nil {
		m.cancelWorldStatus()
		m.cancelWorldStatus = nil
	}
	m.worldStatus, m.worldStatusSeq = world.NodeStatus{}, 0
	if _, ok := m.activePanel.(*worldPanel); ok {
		m.closePanel()
	}
}

// loadWorldStatus 查询当前运行时服务是否启用联系人及其在线状态。
func (m *tuiModel) loadWorldStatus(delay time.Duration) tea.Cmd {
	client := m.opt.Client
	if client == nil {
		return nil
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.cancelWorldStatus = cancel
	seq := m.nextRequestSeq()
	m.worldStatusSeq = seq
	return func() tea.Msg {
		defer cancel()
		if delay > 0 {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return nil
			case <-timer.C:
			}
		}
		msg := worldStatusLoadedMsg{seq: seq}
		requestCtx, stop := context.WithTimeout(ctx, worldRequestTimeout)
		defer stop()
		msg.status, msg.err = client.GetWorldStatus(requestCtx)
		return msg
	}
}

// handleWorldStatusLoaded 更新在线状态，并在服务启用或探测暂时失败时继续刷新。
func (m *tuiModel) handleWorldStatusLoaded(msg worldStatusLoadedMsg) tea.Cmd {
	if msg.seq != m.worldStatusSeq {
		return nil
	}
	if msg.err != nil {
		// 暂时的探测失败只降级在线状态，不关闭用户正在编辑的面板。
		if m.worldStatus.Enabled {
			m.worldStatus.Connected = false
		}
	} else if !msg.status.Enabled {
		m.resetWorldStatus()
		return nil
	} else {
		m.worldStatus = msg.status
	}
	return m.loadWorldStatus(worldStatusInterval)
}

// openWorldPanel 打开联系人列表，转发时保存源对话。
func (m *tuiModel) openWorldPanel(conversationID defs.ConversationID) tea.Cmd {
	p := &worldPanel{conversationID: conversationID}
	p.fetch.start()
	return m.openPanel(p, p.getPeersFetch(m.opt.Client))
}

func (p *worldPanel) getTitle() string {
	if p.conversationID != "" {
		return "转发到联系人"
	}
	return "联系人"
}

func (p *worldPanel) getPeersFetch(client runtimeclient.Client) func(context.Context) (any, error) {
	return func(ctx context.Context) (any, error) {
		ctx, cancel := context.WithTimeout(ctx, worldRequestTimeout)
		defer cancel()
		return client.ListWorldPeers(ctx)
	}
}

// handleKey 处理联系人选择、消息编辑和提交。
func (p *worldPanel) handleKey(m *tuiModel, msg tea.KeyPressMsg) (panelKeyResult, tea.Cmd) {
	key := msg.String()
	if p.message != nil {
		if p.fetch.isLoading {
			return panelKeyHandled, nil
		}
		switch key {
		case "esc":
			p.message = nil
			p.fetch.finish(nil)
			return panelKeyHandled, nil
		case "enter":
			return panelKeyHandled, p.send(m)
		default:
			return panelKeyHandled, p.message.handleKey(msg)
		}
	}
	switch {
	case key == "esc":
		return panelKeyClosed, nil
	case p.fetch.isLoading:
		return panelKeyHandled, nil
	case key == "ctrl+r":
		p.fetch.start()
		return panelKeyHandled, m.fetchForPanel(p.getPeersFetch(m.opt.Client))
	case key == "enter" && len(p.recipients) > 0 && strings.TrimSpace(p.recipients[p.list.selected].address) != "":
		p.message = newComposer(m.styles)
		p.message.textarea.SetVirtualCursor(true)
		return panelKeyHandled, p.message.focus()
	case p.list.handleKey(key, len(p.recipients), m.getPanelPageSize()):
	case key == "backspace" && len(p.recipients) > 0 && p.list.selected == len(p.recipients)-1:
		row := &p.recipients[p.list.selected]
		row.address = trimLastRune(row.address)
	case msg.Text != "" && len(p.recipients) > 0:
		p.list.selected = len(p.recipients) - 1
		p.recipients[p.list.selected].address += msg.Text
	}
	return panelKeyHandled, nil
}

func (p *worldPanel) handlePaste(_ *tuiModel, text string) tea.Cmd {
	if p.message != nil && !p.fetch.isLoading {
		p.message.handlePaste(text)
	} else if !p.fetch.isLoading && len(p.recipients) > 0 {
		p.list.selected = len(p.recipients) - 1
		p.recipients[p.list.selected].address += strings.TrimSpace(text)
	}
	return nil
}

// send 提交消息，或使用当前对话最近一条助手回复发起转发。
func (p *worldPanel) send(m *tuiModel) tea.Cmd {
	text := strings.TrimSpace(p.message.expandPastes(p.message.getValue()))
	if p.conversationID == "" && text == "" {
		return nil
	}
	client := m.opt.Client
	to, conversationID := p.recipients[p.list.selected].address, p.conversationID
	p.fetch.start()
	return m.fetchForPanel(func(ctx context.Context) (any, error) {
		ctx, cancel := context.WithTimeout(ctx, worldRequestTimeout)
		defer cancel()
		if conversationID != "" {
			return client.ForwardWorldConversation(ctx, world.ForwardRequest{
				To: to, ConversationID: conversationID, Note: text,
			})
		}
		return client.SendWorldMessage(ctx, world.SendMessageRequest{To: to, Message: text})
	})
}

// handleResult 显示请求错误、保存联系人列表或提示投递状态。
func (p *worldPanel) handleResult(m *tuiModel, msg panelResultMsg) tea.Cmd {
	p.fetch.finish(msg.err)
	if msg.err != nil {
		return nil
	}
	if p.message == nil {
		p.recipients = nil
		for _, peer := range msg.value.(world.ListPeersResult).Peers {
			p.recipients = append(p.recipients, worldRecipient{peer.Address, peer.NodeName, peer.Online})
			for _, target := range peer.Targets {
				p.recipients = append(p.recipients, worldRecipient{peer.Address + "/" + target.Name, "  " + target.Name, peer.Online})
			}
		}
		p.recipients = append(p.recipients, worldRecipient{})
		p.list.clamp(len(p.recipients))
		return nil
	}
	delivery := msg.value.(world.WorldMessage)
	m.closePanel()
	text := formatWorldDeliveryStatus(string(delivery.Status))
	if delivery.ErrorText != "" {
		text += "：" + normalizePastedText(delivery.ErrorText)
	}
	return m.showNotice(text, delivery.Status == "failed", noticeDuration)
}

// render 显示联系人列表或选中联系人的消息编辑区。
func (p *worldPanel) render(m *tuiModel, width, maxHeight int) []string {
	var lines []string
	if status := p.fetch.formatStatusLine(m); status != "" {
		lines = append(lines, status)
	}
	if p.message != nil {
		recipient := p.recipients[p.list.selected]
		label := "消息"
		if p.conversationID != "" {
			label = "附言（可选）"
		}
		lines = append(lines, recipient.address+" · "+label)
		p.message.setSize(max(1, width-4), max(1, maxHeight-2-len(lines)))
		lines = append(lines, strings.Split(p.message.view(), "\n")...)
		return renderPanelFrame(p.getTitle(), []keyHint{{key: "enter", action: "发送"}, {key: "esc", action: "返回"}}, lines, width, maxHeight, m.styles)
	}
	rows := make([]string, len(p.recipients))
	for i, recipient := range p.recipients {
		if i == len(p.recipients)-1 {
			rows[i] = "输入地址 · " + cmp.Or(recipient.address, "节点@域[/目标]")
			continue
		}
		mark := m.styles.warning.Render("● 离线")
		if recipient.online {
			mark = getStateStyle(stateConnected, m.styles).Render("● 在线")
		}
		name := ""
		if recipient.name != "" {
			name = recipient.name + " · "
		}
		rows[i] = mark + "  " + name + recipient.address
	}
	if len(rows) == 0 && !p.fetch.isLoading && p.fetch.err == nil {
		lines = append(lines, m.styles.faint.Render("没有联系人"))
	}
	lines = append(lines, renderListRows(m, rows, &p.list, maxHeight-2-len(lines), width-4)...)
	hints := []keyHint{{key: "enter", action: "选择"}, {key: "ctrl+r", action: "刷新"}, {key: "esc", action: "关闭"}}
	return renderPanelFrame(p.getTitle(), hints, lines, width, maxHeight, m.styles)
}

func formatWorldDeliveryStatus(status string) string {
	switch status {
	case "pending_offline":
		return "等待对方上线"
	case "sending":
		return "发送中"
	case "unknown":
		return "正在核对状态"
	case "remote_running":
		return "对方处理中"
	case "replied":
		return "已回复"
	case "failed":
		return "发送失败"
	case "cancelled":
		return "已取消"
	default:
		return "已提交"
	}
}
