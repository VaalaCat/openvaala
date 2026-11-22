package desktop

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/VaalaCat/mygo-kit"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/egoist/mygo/ui"
)

const serverTestTimeout = 10 * time.Second

// applyStartTarget 把命令行 --url 或 config.yaml 的 client.url 解析成启动远端目标。
func (d *Desktop) applyStartTarget() {
	// 1. 启动参数没有指定远端地址时直接走偏好或本机默认。
	if strings.TrimSpace(d.opt.StartTarget.URL) == "" {
		return
	}
	parsedURL, parsedToken, defaultName, _, err := parseRemoteServerInput(d.opt.StartTarget.URL, d.opt.StartTarget.Token)
	if err != nil {
		d.startTargetError = err
		return
	}
	target := &remoteServer{
		ID:    "startup-remote",
		Name:  defaultName,
		URL:   parsedURL,
		Token: parsedToken,
	}
	if d.opt.StartTarget.IsOverride {
		d.serverOverride = target
		return
	}
	d.defaultRemote = target
}

// getServerMenuItems 返回左下角状态栏服务器菜单条目：本机、已保存远端列表、分隔线和「管理服务器…」。
func (d *Desktop) getServerMenuItems() []kit.MenuItem {
	// 1. 判断当前激活项，按统一顺序列出本机和所有远端。
	activeTarget, isLocal := d.getActiveServerTarget()
	items := []kit.MenuItem{
		{
			Icon:  checkedIcon(isLocal),
			Title: d.t("server.local"),
			OnSelect: func() {
				if !isLocal {
					d.switchServer(localServerID)
				}
			},
		},
	}
	// 2. 命令行 --url 或 config.yaml 默认远端正在生效时也在菜单里标出。
	if d.serverOverride != nil {
		label := d.serverOverride.Name
		if label == "" {
			label = getRemoteServerHost(d.serverOverride.URL)
		}
		items = append(items, kit.MenuItem{
			Icon:     kit.IconCheck,
			Title:    label,
			Shortcut: d.t("server.override.badge"),
			OnSelect: func() {},
		})
	}
	if d.defaultRemote != nil {
		isDefaultActive := !isLocal && activeTarget != nil && d.serverOverride == nil && d.preferences.ActiveServer == ""
		label := d.defaultRemote.Name
		if label == "" {
			label = getRemoteServerHost(d.defaultRemote.URL)
		}
		items = append(items, kit.MenuItem{
			Icon:     checkedIcon(isDefaultActive),
			Title:    label,
			Shortcut: d.t("server.config.default"),
			OnSelect: func() {
				if !isDefaultActive {
					d.switchServer("")
				}
			},
		})
	}
	for _, server := range d.preferences.RemoteServers {
		server := server
		isActive := !isLocal && d.serverOverride == nil && d.preferences.ActiveServer == server.ID
		title := server.Name
		if title == "" {
			title = getRemoteServerHost(server.URL)
		}
		items = append(items, kit.MenuItem{
			Icon:     checkedIcon(isActive),
			Title:    title,
			Shortcut: getRemoteServerHost(server.URL),
			OnSelect: func() {
				if !isActive {
					d.switchServer(server.ID)
				}
			},
		})
	}
	items = append(items,
		kit.MenuItem{Separator: true},
		kit.MenuItem{
			Icon:     kit.IconPlus,
			Title:    d.t("server.manage.ellipsis"),
			OnSelect: func() { d.openServersDialog() },
		},
	)
	return items
}

// checkedIcon 当前项被选中时返回勾选图标，否则留空但保留对齐槽位。
func checkedIcon(selected bool) *ui.SVG {
	if selected {
		return kit.IconCheck
	}
	return nil
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
		Token:     editing.Token,
		ParsedURL: editing.URL,
	}
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
	_, isLocal := d.getActiveServerTarget()
	// 1. 服务器列表容器：本机与已保存远端用淡色分割线隔开。
	ui.Column(c).Gap(0).Dividers(1, t.Colors.BorderSubtle).Children(func() {
		kit.ListRow(c.Key("server-row-local"), kit.ListRowOpt{
			Title: d.t("server.local"),
			Trailing: func() {
				if isLocal {
					kit.Badge(c, d.t("server.current"), kit.BadgeAccent)
					return
				}
				if kit.Button(c.Key("switch-local-row"), d.t("server.switch")).Clicked() {
					d.serversDialogOpen = false
					d.switchServer(localServerID)
				}
			},
		}).FillWidth()
		for _, server := range d.preferences.RemoteServers {
			server := server
			isActive := !isLocal && d.serverOverride == nil && d.preferences.ActiveServer == server.ID
			displayName := server.Name
			if displayName == "" {
				displayName = getRemoteServerHost(server.URL)
			}
			kit.ListRow(c.Key("server-row-"+server.ID), kit.ListRowOpt{
				Title:       displayName,
				Description: getRemoteServerHost(server.URL),
				Trailing: func() {
					if isActive {
						kit.Badge(c, d.t("server.current"), kit.BadgeAccent)
					} else if kit.Button(c.Key("switch-"+server.ID), d.t("server.switch")).Clicked() {
						d.serversDialogOpen = false
						d.switchServer(server.ID)
					}
					if kit.Button(c.Key("edit-"+server.ID), d.t("server.edit.action")).Clicked() {
						d.openServerForm(&server)
					}
					if kit.Button(c.Key("delete-"+server.ID), d.t("delete")).Clicked() {
						if isActive {
							d.serverDeleteConfirmID = server.ID
						} else {
							d.deleteRemoteServer(server.ID)
						}
					}
				},
			}).FillWidth()
		}
	})
	// 2. 删除当前活跃服务器时的行内二次确认，避免叠双层弹窗。
	if d.serverDeleteConfirmID != "" {
		if index := slices.IndexFunc(d.preferences.RemoteServers, func(s remoteServer) bool { return s.ID == d.serverDeleteConfirmID }); index >= 0 {
			target := d.preferences.RemoteServers[index]
			name := target.Name
			if name == "" {
				name = getRemoteServerHost(target.URL)
			}
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
	kit.Field(c.Key("server-name-field"), kit.FieldOpt{Label: d.t("server.name")}, func() ui.Element {
		return kit.TextInputWith(c.Key("server-name-input"), &d.serverForm.Name, kit.InputOpt{
			Placeholder: d.t("server.name.placeholder"),
		}).FillWidth().Label(d.t("server.name"))
	})
	kit.Field(c.Key("server-url-field"), kit.FieldOpt{Label: d.t("server.address"), Error: d.serverForm.URLError}, func() ui.Element {
		input := kit.TextInputWith(c.Key("server-url-input"), &d.serverForm.URL, kit.InputOpt{
			Placeholder: d.t("server.address.placeholder"),
			Invalid:     d.serverForm.URLError != "",
		}).FillWidth().Label(d.t("server.address"))
		if input.Changed() {
			d.handleServerAddressChanged()
		}
		return input
	})
	if d.serverForm.ParsedURL != "" && d.serverForm.URLError == "" && d.serverForm.ParsedURL != strings.TrimSpace(d.serverForm.URL) {
		kit.FieldMessage(c, kit.MessageDescription, "→ "+d.serverForm.ParsedURL)
	}
	kit.Field(c.Key("server-token-field"), kit.FieldOpt{Label: d.t("server.token")}, func() ui.Element {
		return ui.Row(c).FillWidth().Gap(t.Space(2)).AlignItems(ui.Center).Children(func() {
			tokenInput := kit.TextInputWith(c.Key("server-token-input"), &d.serverForm.Token, kit.InputOpt{}).Grow(1).Shrink(1).MinWidth(0).Label(d.t("server.token"))
			if !d.serverForm.ShowToken {
				tokenInput.Password()
			}
			if tokenInput.Changed() {
				d.serverForm.TokenAutoFilled = false
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
	if d.serverForm.TokenAutoFilled {
		kit.FieldMessage(c, kit.MessageDescription, d.t("server.token.autofilled"))
	}
	// 2. 独立测试连接区：不打断当前主连接。
	ui.Row(c).Gap(t.Space(2)).AlignItems(ui.Center).Padding(t.Space(1), 0, 0, 0).Children(func() {
		testLabel := d.t("server.test")
		if d.serverTestPending {
			testLabel = d.t("server.testing")
		}
		if kit.Button(c.Key("test-server-connection"), testLabel).Disabled(d.serverTestPending || strings.TrimSpace(d.serverForm.URL) == "").Clicked() {
			d.startServerConnectionTest()
		}
		if d.serverTestPending {
			kit.Spinner(c, kit.IconSizeDefault)
		} else if d.serverTestSuccess != "" {
			kit.StatusDot(c, t.Colors.Success)
			t.TextRole(ui.Text(c, d.serverTestSuccess), kit.TextCaption).TextColor(t.Colors.Success).Shrink(1)
		}
	})
	if d.serverTestError != nil {
		d.errorMessageView(c, d.serverTestError)
	}
	// 3. 底部动作：返回、仅保存、保存并切换。
	ui.Row(c).Justify(ui.SpaceBetween).AlignItems(ui.Center).Padding(t.Space(2), 0, 0, 0).Gap(t.Space(2)).Children(func() {
		if kit.Button(c.Key("server-form-back"), d.t("server.back")).Clicked() {
			d.cancelServerTest()
			d.serverFormOpen = false
		}
		ui.Row(c).Gap(t.Space(2)).AlignItems(ui.Center).Children(func() {
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
	if strings.TrimSpace(d.serverForm.URL) == "" {
		d.serverForm.ParsedURL = ""
		d.serverForm.URLError = ""
		return
	}
	parsedURL, parsedToken, _, extractedToken, err := parseRemoteServerInput(d.serverForm.URL, d.serverForm.Token)
	if err != nil {
		d.serverForm.ParsedURL = ""
		d.serverForm.URLError = err.Error()
		return
	}
	d.serverForm.URLError = ""
	d.serverForm.ParsedURL = parsedURL
	if extractedToken {
		d.serverForm.URL = parsedURL
		d.serverForm.Token = parsedToken
		d.serverForm.TokenAutoFilled = true
	}
}

// saveServerForm 校验并保存表单中的远端服务器档案；switchNow 为真时保存后立刻切换过去。
func (d *Desktop) saveServerForm(switchNow bool) {
	parsedURL, parsedToken, _, _, err := parseRemoteServerInput(d.serverForm.URL, d.serverForm.Token)
	if err != nil {
		d.serverForm.URLError = err.Error()
		return
	}
	d.serverForm.URLError = ""
	d.serverForm.ParsedURL = parsedURL
	name := strings.TrimSpace(d.serverForm.Name)
	id := d.serverForm.EditingID
	if id == "" {
		id = newRemoteServerID(d.preferences.RemoteServers)
		d.preferences.RemoteServers = append(d.preferences.RemoteServers, remoteServer{
			ID:    id,
			Name:  name,
			URL:   parsedURL,
			Token: parsedToken,
		})
	} else {
		updated := false
		for i := range d.preferences.RemoteServers {
			if d.preferences.RemoteServers[i].ID == id {
				d.preferences.RemoteServers[i] = remoteServer{
					ID:    id,
					Name:  name,
					URL:   parsedURL,
					Token: parsedToken,
				}
				updated = true
				break
			}
		}
		if !updated {
			d.preferences.RemoteServers = append(d.preferences.RemoteServers, remoteServer{
				ID:    id,
				Name:  name,
				URL:   parsedURL,
				Token: parsedToken,
			})
		}
	}
	d.cancelServerTest()
	d.savePreferencesWithToast()
	d.showSuccess(d.t("server.saved"), name)
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
	name := removed.Name
	if name == "" {
		name = getRemoteServerHost(removed.URL)
	}
	d.showSuccess(d.t("server.deleted"), name)
	if wasActive {
		d.serversDialogOpen = false
		d.switchServer(localServerID)
	}
}

// startServerConnectionTest 用独立临时 Client 对表单地址做一次 10 秒超时的连通与协议握手测试。
func (d *Desktop) startServerConnectionTest() {
	parsedURL, parsedToken, _, _, err := parseRemoteServerInput(d.serverForm.URL, d.serverForm.Token)
	if err != nil {
		d.serverForm.URLError = err.Error()
		return
	}
	d.serverForm.URLError = ""
	d.serverForm.ParsedURL = parsedURL
	d.cancelServerTest()
	testCtx, testCancel := context.WithTimeout(d.ctx, serverTestTimeout)
	d.serverTestCancel = testCancel
	d.serverTestRequest++
	request := d.serverTestRequest
	d.serverTestPending = true
	d.tasks.Go(func() {
		err := testRemoteServerConnection(testCtx, parsedURL, parsedToken)
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
func testRemoteServerConnection(ctx context.Context, serverURL, token string) error {
	// 1. 创建独立临时 Client，不干扰主工作区连接。
	opt := runtimeclient.NewClientOpt(serverURL, token)
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
