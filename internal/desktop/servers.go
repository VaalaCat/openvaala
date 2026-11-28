package desktop

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/VaalaCat/mygo-kit"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/worldserver"
	"github.com/egoist/mygo/ui"
)

const serverTestTimeout = 10 * time.Second

// applyStartTarget 把命令行 --url 或 config.yaml 的 client.url 解析成启动远端目标。
func (d *Desktop) applyStartTarget() {
	// 1. 启动参数没有指定远端地址时直接走偏好或本机默认。
	if strings.TrimSpace(d.opt.StartTarget.URL) == "" {
		return
	}
	input, err := parseRemoteServerInput(d.opt.StartTarget.URL, d.opt.StartTarget.Token)
	if err != nil {
		d.startTargetError = err
		return
	}
	target := &remoteServer{
		ID:    "startup-remote",
		URL:   input.URL,
		Token: input.Token,
	}
	if input.ExtractedToken && input.WorldTarget != nil && d.opt.StartTarget.IsOverride {
		id := worldserver.GetNodeIDFromSignPublic(input.WorldTarget.PublicKey)
		target.ID = id
		idx := slices.IndexFunc(d.preferences.RemoteServers, func(s remoteServer) bool { return s.ID == id })
		if idx < 0 {
			d.preferences.RemoteServers = append(d.preferences.RemoteServers, *target)
		} else {
			saved := d.preferences.RemoteServers[idx]
			if prior, err := parseRemoteServerInput(saved.URL, ""); err == nil && saved.Name != prior.DefaultName {
				target.Name = saved.Name
			}
			d.preferences.RemoteServers[idx] = *target
		}
		d.preferences.ActiveServer = id
		d.startTargetError = d.savePreferences()
		return
	}
	if d.opt.StartTarget.IsOverride {
		d.serverOverride = target
		return
	}
	d.defaultRemote = target
}

// serverMenuView 用与管理弹窗相同的双行条目显示服务器，方向键在条目间移动。
func (d *Desktop) serverMenuView(c *ui.Context, trigger ui.Element) {
	wasOpen := d.serverMenuOpen
	if trigger.Clicked() || !wasOpen && (trigger.Shortcut(0, ui.KeyDown) || trigger.Shortcut(0, ui.KeyUp)) {
		d.serverMenuOpen = !d.serverMenuOpen
	}
	var selectedRow ui.Element
	kit.Popover(c, trigger, &d.serverMenuOpen, func() {
		active, _ := d.getActiveServerTarget()
		serverRow := func(id string, server *remoteServer) {
			selected := server == active
			row := d.serverRowView(c.Key(id), server, selected, nil).Role(ui.RoleMenuItemRadio).Checked(selected)
			if selected {
				selectedRow = row
			}
			if row.Clicked() {
				d.serverMenuOpen = false
				d.connectToServer(id, selected)
			}
		}
		serverRow(localServerID, &d.localServer)
		if d.serverOverride != nil {
			serverRow(d.serverOverride.ID, d.serverOverride)
		}
		if d.defaultRemote != nil {
			serverRow("", d.defaultRemote)
		}
		for i := range d.preferences.RemoteServers {
			server := &d.preferences.RemoteServers[i]
			serverRow(server.ID, server)
		}
		kit.Separator(c)
		if kit.ListRow(c.Key("manage"), kit.ListRowOpt{Title: d.t("server.manage.ellipsis")}).Role(ui.RoleMenuItem).Clicked() {
			d.serverMenuOpen = false
			d.openServersDialog()
		}
	}).Role(ui.RoleMenu).FocusGroup(ui.Vertical).MinWidth(160)
	trigger.Expanded(d.serverMenuOpen)
	if !wasOpen && d.serverMenuOpen {
		selectedRow.Focus()
	}
}

// serverRowView 在切换器和管理列表显示同一台服务器的名字、地址和断线状态。
func (d *Desktop) serverRowView(c *ui.Context, server *remoteServer, selected bool, trailing func()) ui.Element {
	name, address := getServerDisplay(*server)
	if selected && d.isServerDisconnected() {
		address = cmp.Or(address, name) + " · " + d.t("server.disconnected")
	}
	return kit.ListRow(c, kit.ListRowOpt{
		Title: name, Description: address, Selected: selected, Trailing: trailing,
	}).Description(address)
}

// connectToServer 点已断开的当前行时重新连接，点另一行时切换服务器。
func (d *Desktop) connectToServer(id string, selected bool) {
	if !selected {
		d.switchServer(id)
	} else if d.isServerDisconnected() {
		d.connect()
	}
}

// openServersDialog 打开「管理服务器」弹窗并回到列表态。
func (d *Desktop) openServersDialog() {
	d.cancelServerTest()
	d.serversDialogOpen = true
	d.serverFormOpen = false
	d.serverDeleteConfirmID = ""
}

// openServerForm 进入弹窗内的新增或编辑表单视图；editing 为 nil 时是新增。
func (d *Desktop) openServerForm(editing *remoteServer) {
	d.cancelServerTest()
	d.serverDeleteConfirmID = ""
	d.serverFormOpen = true
	if editing == nil {
		d.serverForm = serverFormDraft{}
		return
	}
	d.serverForm = serverFormDraft{
		EditingID: editing.ID,
		Name:      editing.Name,
		URL:       editing.URL,
		ParsedURL: editing.URL,
		Token:     editing.Token,
	}
	d.handleServerAddressChanged()
}

// cancelServerTest 取消进行中的连接测试并清理结果提示。
func (d *Desktop) cancelServerTest() {
	if d.serverTestCancel != nil {
		d.serverTestCancel()
		d.serverTestCancel = nil
	}
	d.serverTestRequest++
	d.serverTestPending = false
	d.serverTestSuccess = ""
	d.serverTestError = nil
}

// serversDialogView 绘制「管理服务器」单弹窗：列表视图与新增/编辑表单视图原地切换，不叠双层弹窗。
func (d *Desktop) serversDialogView(c *ui.Context) {
	if !d.serversDialogOpen {
		if d.serverTestPending {
			d.cancelServerTest()
		}
		return
	}
	title := d.t("server.manage")
	if d.serverFormOpen {
		if d.serverForm.EditingID == "" {
			title = d.t("server.add")
		} else {
			title = d.t("server.edit")
		}
	}
	kit.Dialog(c.Key("servers-dialog"), &d.serversDialogOpen, title, func() {
		if d.serverFormOpen {
			d.serverFormView(c)
		} else {
			d.serverListView(c)
		}
	})
	if !d.serversDialogOpen {
		d.cancelServerTest()
		d.serverFormOpen = false
		d.serverDeleteConfirmID = ""
	}
}

// serverListView 绘制「管理服务器」列表视图：本机默认项、远端列表、行内删除确认和底部操作。
func (d *Desktop) serverListView(c *ui.Context) {
	t := kit.GetTheme(c)
	active, _ := d.getActiveServerTarget()
	// 1. 点击服务器行连接，远端档案的编辑和删除留在行尾。
	ui.Column(c).Gap(0).Dividers(1, t.Colors.BorderSubtle).Children(func() {
		serverRow := func(id string, server *remoteServer, editable bool) {
			selected := server == active
			row := d.serverRowView(c.Key("server-row-"+id), server, selected, func() {
				if selected {
					kit.Badge(c, d.t("server.current"), kit.BadgeAccent)
				}
				if editable {
					if kit.Button(c.Key("edit-"+id), d.t("server.edit.action")).Clicked() {
						d.openServerForm(server)
					}
					if kit.Button(c.Key("delete-"+id), d.t("delete")).Clicked() {
						if selected {
							d.serverDeleteConfirmID = id
						} else {
							d.deleteRemoteServer(id)
						}
					}
				}
			}).FillWidth()
			if row.Clicked() {
				d.serversDialogOpen = false
				d.connectToServer(id, selected)
			}
		}
		serverRow(localServerID, &d.localServer, false)
		saved := d.preferences.RemoteServers
		for i := range saved {
			serverRow(saved[i].ID, &saved[i], true)
		}
	})
	// 2. 删除当前活跃服务器时的行内二次确认，避免叠双层弹窗。
	if d.serverDeleteConfirmID != "" {
		if index := slices.IndexFunc(d.preferences.RemoteServers, func(s remoteServer) bool { return s.ID == d.serverDeleteConfirmID }); index >= 0 {
			target := d.preferences.RemoteServers[index]
			name, _ := getServerDisplay(target)
			kit.Callout(c.Key("delete-active-server-confirm"), kit.CalloutWarning, fmt.Sprintf(d.t("server.delete.active.confirm"), name), "", func() {
				if kit.Button(c.Key("cancel-delete-server"), d.t("cancel")).Clicked() {
					d.serverDeleteConfirmID = ""
				}
				if kit.DangerButton(c.Key("confirm-delete-server"), d.t("server.delete.confirm.action")).Clicked() {
					d.serverDeleteConfirmID = ""
					d.deleteRemoteServer(target.ID)
				}
			})
		} else {
			d.serverDeleteConfirmID = ""
		}
	}
	// 3. 底部操作：左侧新增远端，右侧关闭。
	ui.Row(c).Justify(ui.SpaceBetween).AlignItems(ui.Center).Padding(t.Space(2), 0, 0, 0).Gap(t.Space(2)).Children(func() {
		if kit.Button(c.Key("add-remote-server"), "+ "+d.t("server.add")).Clicked() {
			d.openServerForm(nil)
		}
		if kit.Button(c.Key("close-servers-dialog"), d.t("close")).Clicked() {
			d.serversDialogOpen = false
		}
	})
}

// serverFormView 绘制新增/编辑远端服务器表单：粘贴自动拆出 URL 和 Token、测试连接和保存/切换按钮。
func (d *Desktop) serverFormView(c *ui.Context) {
	t := kit.GetTheme(c)
	// 1. 名称、连接地址与访问令牌。
	kit.Field(c.Key("server-url-field"), kit.FieldOpt{Label: d.t("server.address"), Error: d.serverForm.URLError}, func() ui.Element {
		input := kit.TextInputWith(c.Key("server-url-input"), &d.serverForm.URL, kit.InputOpt{
			Placeholder: d.t("server.address.placeholder"),
			Invalid:     d.serverForm.URLError != "",
		}).FillWidth()
		if input.Changed() {
			d.handleServerAddressChanged()
		}
		return input
	})
	if d.serverForm.ParsedURL != "" && d.serverForm.URLError == "" && d.serverForm.ParsedURL != strings.TrimSpace(d.serverForm.URL) {
		kit.FieldMessage(c, kit.MessageDescription, "→ "+d.serverForm.ParsedURL)
	}
	kit.Field(c.Key("server-name-field"), kit.FieldOpt{Label: d.t("server.name")}, func() ui.Element {
		return kit.TextInputWith(c.Key("server-name-input"), &d.serverForm.Name, kit.InputOpt{
			Placeholder: d.t("server.name.placeholder"),
		}).FillWidth().Label(d.t("server.name"))
	})
	if strings.TrimSpace(d.serverForm.URL) != "" && !d.serverForm.IsWorldLink {
		kit.Field(c.Key("server-token-field"), kit.FieldOpt{Label: d.t("server.token")}, func() ui.Element {
			return ui.Row(c).FillWidth().Gap(t.Space(2)).AlignItems(ui.Center).Children(func() {
				tokenInput := kit.TextInputWith(c.Key("server-token-input"), &d.serverForm.Token, kit.InputOpt{}).Grow(1).Shrink(1).MinWidth(0).Label(d.t("server.token"))
				if !d.serverForm.ShowToken {
					tokenInput.Password()
				}
				if tokenInput.Changed() {
					d.serverTestSuccess = ""
					d.serverTestError = nil
				}
				toggleLabel := d.t("server.token.show")
				if d.serverForm.ShowToken {
					toggleLabel = d.t("server.token.hide")
				}
				if kit.Button(c.Key("toggle-server-token"), toggleLabel).Shrink(0).Clicked() {
					d.serverForm.ShowToken = !d.serverForm.ShowToken
				}
			})
		})
	}
	// 2. 握手结果在底部操作区上方显示。
	if d.serverTestSuccess != "" {
		ui.Row(c).Gap(t.Space(2)).AlignItems(ui.Center).Children(func() {
			kit.StatusDot(c, t.Colors.Success)
			t.TextRole(ui.Text(c, d.serverTestSuccess), kit.TextCaption).TextColor(t.Colors.Success).Shrink(1)
		})
	}
	if d.serverTestError != nil {
		d.errorMessageView(c, d.serverTestError)
	}
	// 3. 左侧测试地址与令牌，右侧返回或保存。
	ui.Row(c).Justify(ui.SpaceBetween).AlignItems(ui.Center).Padding(t.Space(2), 0, 0, 0).Gap(t.Space(2)).Children(func() {
		ui.Row(c).Gap(t.Space(2)).AlignItems(ui.Center).Children(func() {
			testLabel := d.t("server.test")
			if d.serverTestPending {
				testLabel = d.t("server.testing")
			}
			if kit.Button(c.Key("test-server-connection"), testLabel).Disabled(d.serverTestPending || strings.TrimSpace(d.serverForm.URL) == "").Clicked() {
				d.startServerConnectionTest()
			}
			if d.serverTestPending {
				kit.Spinner(c, kit.IconSizeDefault)
			}
		})
		ui.Row(c).Gap(t.Space(2)).AlignItems(ui.Center).Children(func() {
			if kit.Button(c.Key("server-form-back"), d.t("server.back")).Clicked() {
				d.cancelServerTest()
				d.serverFormOpen = false
			}
			if kit.Button(c.Key("server-save-only"), d.t("server.save.only")).Disabled(strings.TrimSpace(d.serverForm.URL) == "").Clicked() {
				d.saveServerForm(false)
			}
			if kit.PrimaryButton(c.Key("server-save-switch"), d.t("server.save.and.switch")).Disabled(strings.TrimSpace(d.serverForm.URL) == "").Clicked() {
				d.saveServerForm(true)
			}
		})
	})
}

// handleServerAddressChanged 即时解析地址栏输入：规范 URL 并把链接中附带的 token 自动填入令牌框。
func (d *Desktop) handleServerAddressChanged() {
	d.serverTestSuccess = ""
	d.serverTestError = nil
	previousURL := d.serverForm.ParsedURL
	input, err := parseRemoteServerInput(d.serverForm.URL, d.serverForm.Token)
	d.serverForm.ParsedURL = input.URL
	d.serverForm.IsWorldLink = input.WorldTarget != nil
	d.serverForm.URLError = ""
	if err != nil && strings.TrimSpace(d.serverForm.URL) != "" {
		d.serverForm.URLError = err.Error()
	}
	if input.ExtractedToken {
		d.serverForm.URL = input.URL
		d.serverForm.Token = input.Token
	} else if previousURL != input.URL {
		d.serverForm.Token = ""
	}
	if target := input.WorldTarget; target != nil && strings.TrimSpace(d.serverForm.Name) == "" {
		d.serverForm.Name = target.Name
	}
}

// saveServerForm 校验并保存表单中的远端服务器档案；switchNow 为真时保存后立刻切换过去。
func (d *Desktop) saveServerForm(switchNow bool) {
	input, err := parseRemoteServerInput(d.serverForm.URL, d.serverForm.Token)
	if err != nil {
		d.serverForm.URLError = err.Error()
		return
	}
	d.serverForm.URLError = ""
	name := strings.TrimSpace(d.serverForm.Name)
	id := d.serverForm.EditingID
	if id == "" {
		id = newRemoteServerID(d.preferences.RemoteServers)
	}
	server := remoteServer{ID: id, Name: name, URL: input.URL, Token: input.Token}
	index := slices.IndexFunc(d.preferences.RemoteServers, func(saved remoteServer) bool { return saved.ID == id })
	if index >= 0 {
		if saved := d.preferences.RemoteServers[index]; saved.URL == server.URL {
			server.NodeAddr = saved.NodeAddr
		}
		d.preferences.RemoteServers[index] = server
	} else {
		d.preferences.RemoteServers = append(d.preferences.RemoteServers, server)
	}
	d.cancelServerTest()
	d.savePreferencesWithToast()
	d.showSuccess(d.t("server.saved"), cmp.Or(name, input.DefaultName))
	if switchNow {
		d.serversDialogOpen = false
		d.serverFormOpen = false
		d.switchServer(id)
		return
	}
	// 如果编辑的是当前正在连接的远端，也重新连接以应用新地址或新 Token。
	if d.serverOverride == nil && d.preferences.ActiveServer == id {
		d.forceSwitchServer(id)
	}
	d.serverFormOpen = false
}

// deleteRemoteServer 删除指定远端服务器档案；如果它正是当前连接，自动切回本机。
func (d *Desktop) deleteRemoteServer(id string) {
	index := slices.IndexFunc(d.preferences.RemoteServers, func(s remoteServer) bool { return s.ID == id })
	if index < 0 {
		return
	}
	removed := d.preferences.RemoteServers[index]
	d.preferences.RemoteServers = slices.Delete(d.preferences.RemoteServers, index, index+1)
	wasActive := d.serverOverride == nil && d.preferences.ActiveServer == id
	d.savePreferencesWithToast()
	name, _ := getServerDisplay(removed)
	d.showSuccess(d.t("server.deleted"), name)
	if wasActive {
		d.serversDialogOpen = false
		d.switchServer(localServerID)
	}
}

// startServerConnectionTest 用独立临时 Client 在拨号和初始化预算内测试表单中的服务器。
func (d *Desktop) startServerConnectionTest() {
	input, err := parseRemoteServerInput(d.serverForm.URL, d.serverForm.Token)
	if err != nil {
		d.serverForm.URLError = err.Error()
		return
	}
	d.serverForm.URLError = ""
	d.cancelServerTest()
	timeout := serverTestTimeout
	if input.WorldTarget != nil {
		timeout = input.WorldTarget.ConnectionTimeout()
	}
	testCtx, testCancel := context.WithTimeout(d.ctx, timeout)
	d.serverTestCancel = testCancel
	d.serverTestRequest++
	request := d.serverTestRequest
	d.serverTestPending = true
	d.tasks.Go(func() {
		err := testRemoteServerConnection(testCtx, input)
		testCancel()
		if d.window == nil {
			return
		}
		d.window.Update(func() {
			if d.closed || d.serverTestRequest != request {
				return
			}
			d.serverTestPending = false
			d.serverTestCancel = nil
			if err != nil {
				d.serverTestSuccess = ""
				d.serverTestError = err
				return
			}
			d.serverTestError = nil
			d.serverTestSuccess = d.t("server.test.success")
		})
	})
}

// testRemoteServerConnection 建立临时 Client 执行一次 initialize 握手，成功或首轮失败后立即关闭连接。
func testRemoteServerConnection(ctx context.Context, input remoteServerInput) error {
	// 1. 创建独立临时 Client，不干扰主工作区连接。
	opt := runtimeclient.NewClientOpt(input.URL, input.Token)
	opt.WorldTarget = input.WorldTarget
	opt.FailRequestsWhenDisconnected = true
	opt.DisableSubmitInputRetry = true
	client, err := runtimeclient.NewClient(opt)
	if err != nil {
		return err
	}
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	runDone := make(chan error, 1)
	go func() {
		runDone <- client.Run(runCtx)
	}()
	// 2. 等待首次连上、首次拨号失败或 Run 因鉴权/协议不兼容退出，结束后等待 goroutine 回收。
	var resultErr error
	decided := false
	for event := range client.Events() {
		changed, ok := event.(*runtimeclient.ConnectionChanged)
		if !ok || decided {
			continue
		}
		switch changed.State {
		case runtimeclient.ConnectionConnected:
			decided = true
			resultErr = nil
			cancelRun()
		case runtimeclient.ConnectionReconnecting:
			if changed.Err != nil {
				decided = true
				resultErr = changed.Err
				cancelRun()
			}
		}
	}
	runErr := <-runDone
	if decided {
		return resultErr
	}
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		return runErr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return runtimeclient.ErrConnectionLost
}
