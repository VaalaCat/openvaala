package desktop

import (
	"context"
	"fmt"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/VaalaCat/mygo-kit"
	"github.com/VaalaCat/openvaala/internal/cli"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/VaalaCat/openvaala/pkg/world"
	"github.com/VaalaCat/openvaala/pkg/worldserver"
	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
	"github.com/skip2/go-qrcode"
)

func (d *Desktop) loadWorldSettings() {
	if d.client == nil {
		return
	}
	if d.cancelWorldSettings != nil {
		d.cancelWorldSettings()
	}
	ctx, cancel := context.WithCancel(d.clientCtx)
	d.cancelWorldSettings = cancel
	d.worldStatus = world.NodeStatus{}
	d.worldQR = nil
	d.worldEditPage = false
	client := d.client
	d.settingsLoading = true
	d.tasks.Go(func() {
		result, err := client.GetWorldSettings(ctx)
		d.window.Update(func() {
			if ctx.Err() != nil {
				return
			}
			d.settingsLoading = false
			d.settingsError = err
			if err != nil {
				return
			}
			d.worldSaved = &result
			d.worldInviteCode = ""
			d.resetWorldDraft()
			if d.worldRestarting {
				d.worldRestarting = false
				d.showSuccess(d.t("world.restarted"), "")
			}
		})
		if err != nil || (!result.Running.NodeEnabled && !result.Running.ServerEnabled && !result.Running.RemoteAccessEnabled) {
			return
		}
		// 设置页打开期间刷新运行状态，保留用户正在编辑的设置草稿。
		for ctx.Err() == nil {
			status, err := client.GetWorldStatus(ctx)
			d.window.Update(func() {
				if ctx.Err() == nil {
					if err != nil {
						d.worldStatus = world.NodeStatus{}
						d.worldQR = nil
					} else {
						if status.RemoteAccessURL != d.worldStatus.RemoteAccessURL {
							d.worldQR = worldQRCode(status.RemoteAccessURL)
						}
						d.worldStatus = status
					}
				}
			})
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
		}
	})
}

func (d *Desktop) resetWorldDraft() {
	d.worldNewTarget = worldserver.AdvertisedTarget{Name: "default", Visibility: "domain"}
	d.worldNewDirectURL = ""
	d.worldNewRelay = ""
	d.worldDraft = d.worldSaved.Settings
	d.worldDraft.Relays = slices.Clone(d.worldDraft.Relays)
	n := &d.worldDraft.Node
	n.DirectURLs = slices.Clone(n.DirectURLs)
	n.AdvertiseTargets = slices.Clone(n.AdvertiseTargets)
	d.worldError = nil
}

func (d *Desktop) isWorldDirty() bool {
	return d.page == "world" && d.worldSaved != nil && !reflect.DeepEqual(d.worldSaved.Settings, d.worldDraft)
}

func (d *Desktop) worldSettingsView(c *ui.Context) {
	if d.worldSaved == nil {
		return
	}
	if d.worldRestarting {
		kit.InlineHint(c, d.t("world.restarting"), true)
		return
	}
	n, s := &d.worldDraft.Node, &d.worldDraft.Server
	registered := n.ServerURL != ""
	saved := d.worldSaved
	if d.worldError != nil {
		d.errorMessageView(c, d.worldError)
	}
	t := kit.GetTheme(c)
	switchRow := func(title string, value *bool, enabled, running, registered bool) {
		state, color := d.t("world.stopped"), t.Colors.Muted
		switch {
		case enabled != running:
			state, color = d.t("world.pending"), t.Colors.Warning
		case title == d.t("world.node") && d.worldStatus.Revoked:
			state, color = fmt.Sprintf(d.t("world.revoked"), n.Domain), t.Colors.Warning
		case !registered:
			state = d.t("world.unjoined")
		case running:
			state, color = d.t("world.running"), t.Colors.Success
		}
		hint := ""
		if !registered {
			hint = d.t("world.joinFirst")
		}
		kit.SettingRow(c, state, hint, func() {
			kit.StatusDot(c, color).Label(state)
			kit.Switch(c.Key(title), value, "").Label(title).Disabled(!registered || d.worldSaving)
		})
	}
	names := make([]string, 0, len(d.agents))
	for _, info := range d.agents {
		names = append(names, info.Name)
	}
	// 输入和附属按钮共用一列，和模型服务表单的控件起点对齐。
	controls := func(c *ui.Context, content func()) {
		ui.Row(c).Width(settingInputWidth).Gap(t.Space(2)).AlignItems(ui.Center).Children(content)
	}
	choice := func(c *ui.Context, label string, value *string, options []kit.SelectOption) {
		kit.Select(c, value, kit.SelectOpt{Label: label, Bordered: true, Options: options}).Grow(1).Shrink(1).MinWidth(0)
	}
	kit.SettingSection(c.Key("world-node"), d.t("world.node"), func() {
		switchRow(d.t("world.node"), &n.Enabled, saved.Settings.Node.Enabled, saved.Running.NodeEnabled, registered)
		if !registered {
			d.worldJoinFields(c)
			kit.SettingRow(c, d.t("world.joinInfo"), "", func() {
				if kit.PrimaryButton(c, d.t("world.join")).Disabled(!d.canJoinWorld() || d.worldSaving).Clicked() {
					d.confirmWorldSave(true)
				}
			})
			return
		}
		addr, detail := n.Name+"@"+n.Domain, n.ServerURL
		if live := d.worldStatus; live.Enabled {
			addr = live.NodeAddr
			label := d.t("world.disconnected")
			if live.Connected {
				label = d.t("connected")
			}
			detail = live.ServerURL + " · " + label
			if live.PendingOutboxCount > 0 {
				detail += " · " + fmt.Sprintf(d.t("world.pendingMessages"), live.PendingOutboxCount)
			}
		}
		action := d.t("world.change")
		if d.worldStatus.Revoked {
			action = d.t("world.rejoin")
			detail = d.t("world.revokedHelp")
		}
		kit.SettingRow(c.Key("world-identity"), addr, detail, func() {
			if kit.Button(c, action).Clicked() {
				d.worldChangeOpen = true
			}
		})
		kit.SettingRow(c, d.t("world.defaultAgent"), "", func() {
			controls(c, func() {
				choice(c.Key("world-default"), d.t("world.defaultAgent"), &n.DefaultTarget, selectOptions(append([]string{""}, names...), append([]string{d.t("world.auto")}, names...)))
			})
		})
	})
	kit.SettingSection(c.Key("world-remote"), d.t("world.remoteAccess"), func() {
		remote := &d.worldDraft.RemoteAccess
		prev := remote.Enabled
		switchRow(d.t("world.remoteAccess"), &remote.Enabled, saved.Settings.RemoteAccess.Enabled, saved.Running.RemoteAccessEnabled, true)
		if !prev && remote.Enabled && len(d.worldDraft.Relays) == 0 {
			d.worldDraft.Relays = []string{"https://open.vaa.la"}
		}
		if remote.Enabled {
			d.settingInputRow(c, "world-machine-name", d.t("world.machineName"), "", &remote.Name, settingInputOpt{})
			for _, relay := range d.worldStatus.RemoteAccessRelays {
				if relay.Connected {
					kit.InlineHint(c, strings.Replace(relay.URL, "ws", "http", 1)+" · "+d.t("connected"), false)
					break
				}
			}
			link := d.worldStatus.RemoteAccessURL
			if link != "" {
				ui.Row(c).Gap(t.Space(2)).Padding(t.Space(2), t.Space(3)).AlignItems(ui.Center).Children(func() {
					t.TextRole(ui.Text(c, link), kit.TextCode).SingleLine().Ellipsis("…").Grow(1).Shrink(1).MinWidth(0)
					if kit.IconButtonTip(c, kit.IconCopy, d.t("world.copyLink")).Clicked() {
						c.WriteClipboard(link)
					}
				})
				if d.worldQR != nil {
					ui.Image(c, d.worldQR).Width(160).Height(160).AlignSelf(ui.Start).Fit(ui.Contain)
				}
				kit.InlineHint(c, d.t("world.trustHint"), false)
			}
			page := d.worldDraft.WebURL
			if page == "" && link != "" {
				if u, err := url.Parse(link); err == nil {
					page = u.Scheme + "://" + u.Host
				}
			}
			if page == "" {
				page = world.SelectRemoteAccessPage("", d.worldDraft.Relays, n.DirectURLs)
			}
			label := page
			if world.IsLocalPageURL(page) && page != "" {
				label += " · " + d.t("world.localOnly")
			}
			kit.SettingRow(c, d.t("world.webURL"), label, func() {
				if kit.Button(c, d.t("world.editPage")).Clicked() {
					d.worldEditPage = !d.worldEditPage
				}
			})
			if d.worldEditPage {
				d.settingInputRow(c, "world-web-url", d.t("world.webURL"), "", &d.worldDraft.WebURL, settingInputOpt{})
			}
		}
	})
	kit.SettingSection(c.Key("world-relays"), d.t("world.relays"), func() {
		home := ""
		if saved.Running.NodeEnabled {
			home = n.ServerURL
		}
		relayURLs := append([]string{home}, d.worldDraft.Relays...)
		relayKey := func(address string) string { key, _ := world.GetWorldSocketURL(address, ""); return key }
		attachments := d.worldStatus.RemoteAccessRelays
		if saved.Running.NodeEnabled || !saved.Running.RemoteAccessEnabled {
			attachments = d.worldStatus.RelayAttachments
		}
		connected := map[string]bool{}
		for _, r := range attachments {
			if r.Connected {
				connected[relayKey(r.URL)] = true
			}
		}
		total, online := 0, 0
		for _, base := range relayURLs {
			if base != "" {
				total++
				if connected[relayKey(base)] {
					online++
				}
			}
		}
		kit.InlineHint(c, fmt.Sprintf(d.t("world.relaySummary"), total, online), false)
		kit.SettingRow(c, d.t("world.relayURL"), "", func() {
			controls(c, func() {
				kit.TextInput(c.Key("relay-url"), &d.worldNewRelay).Placeholder("https://open.vaa.la").Label(d.t("world.relayURL")).Grow(1).Shrink(1).MinWidth(0)
				raw := strings.TrimSpace(d.worldNewRelay)
				normalized := relayKey(raw)
				if kit.Button(c.Key("relay-add"), d.t("world.add")).Disabled(raw == "" || slices.ContainsFunc(relayURLs, func(existing string) bool { return relayKey(existing) == normalized })).Clicked() {
					d.worldDraft.Relays = append(d.worldDraft.Relays, raw)
					d.worldNewRelay = ""
				}
			})
		})
		remove := -1
		for i, base := range relayURLs {
			if base == "" {
				continue
			}
			row := c.Key("relay-" + strconv.Itoa(i))
			ui.Row(row).Gap(t.Space(2)).Padding(t.Space(2), t.Space(3)).AlignItems(ui.Center).Children(func() {
				color, label := t.Colors.Muted, d.t("world.disconnected")
				if connected[relayKey(base)] {
					color, label = t.Colors.Success, d.t("connected")
				}
				kit.StatusDot(row, color).Label(label)
				t.TextRole(ui.Text(row, base), kit.TextCode).SingleLine().Ellipsis("…").Grow(1).Shrink(1).MinWidth(0)
				if i == 0 {
					t.TextRole(ui.Text(row, d.t("world.homeRelay")), kit.TextCaption)
				} else if kit.IconButtonTip(row, kit.IconX, d.t("world.remove")).Clicked() {
					remove = i - 1
				}
			})
		}
		if remove >= 0 {
			d.worldDraft.Relays = slices.Delete(d.worldDraft.Relays, remove, remove+1)
		}
	})
	kit.SettingSection(c.Key("world-relay-mode"), d.t("world.relayMode"), func() {
		mode := d.worldDraft.RelayMode
		if mode == "" {
			mode = "off"
			if s.Enabled {
				mode = "domain"
			}
		}
		ui.Row(c).Gap(t.Space(2)).Padding(t.Space(2), t.Space(3)).Children(func() {
			for _, value := range []string{"off", "domain", "public"} {
				button := kit.Button
				if mode == value {
					button = kit.PrimaryButton
				}
				if button(c.Key(value), d.t("world.mode_"+value)).Disabled(value == "domain" && !s.Enabled).Clicked() {
					d.worldDraft.RelayMode = value
				}
			}
		})
	})
	kit.SettingSection(c.Key("world-server"), d.t("world.server"), func() {
		switchRow(d.t("world.server"), &s.Enabled, saved.Settings.Server.Enabled, saved.Running.ServerEnabled, true)
		if !s.Enabled && d.worldDraft.RelayMode == "domain" {
			d.worldDraft.RelayMode = "off"
		}
		hint := d.t("world.domainHelp")
		if saved.Settings.Server.IsDatabaseCreated {
			hint += " " + d.t("world.domainHint")
		}
		d.settingInputRow(c, "world-domain", d.t("world.domain"), hint, &s.Domain, settingInputOpt{Placeholder: "home"})
	})
	if registered {
		agents := selectOptions(names, names)
		visibility := selectOptions([]string{"domain", "public"}, []string{"domain", "public"})
		kit.SettingSection(c.Key("world-targets"), d.t("world.targets"), func() {
			kit.SettingRow(c, d.t("agent"), "", func() {
				controls(c, func() {
					choice(c.Key("world-target-name"), d.t("agent"), &d.worldNewTarget.Name, agents)
				})
			})
			kit.SettingRow(c, d.t("world.visibility"), "", func() {
				controls(c, func() {
					choice(c.Key("world-target-visibility"), d.t("world.visibility"), &d.worldNewTarget.Visibility, visibility)
					if kit.Button(c.Key("target-add"), d.t("world.add")).Label(d.t("world.add") + " · " + d.t("world.targets")).Clicked() {
						n.AdvertiseTargets = append(n.AdvertiseTargets, d.worldNewTarget)
					}
				})
			})
			if len(n.AdvertiseTargets) == 0 {
				kit.InlineHint(c, d.t("world.targetsEmpty"), false)
			}
			remove := -1
			for i := range n.AdvertiseTargets {
				target := &n.AdvertiseTargets[i]
				row := c.Key("target" + strconv.Itoa(i))
				ui.Row(row).Padding(t.Space(2), t.Space(3)).Gap(t.Space(2)).AlignItems(ui.Center).Children(func() {
					choice(row.Key("name"), d.t("agent"), &target.Name, agents)
					choice(row.Key("visibility"), d.t("world.visibility"), &target.Visibility, visibility)
					kit.TextInput(row.Key("description"), &target.Description).Label(d.t("world.description")).Placeholder(d.t("world.description")).Grow(2).Shrink(1).MinWidth(0)
					if kit.IconButtonTip(row.Key("remove"), kit.IconX, d.t("world.remove")).Clicked() {
						remove = i
					}
				})
			}
			if remove >= 0 {
				n.AdvertiseTargets = slices.Delete(n.AdvertiseTargets, remove, remove+1)
			}
		})
		kit.SettingSection(c.Key("world-urls"), d.t("world.direct"), func() {
			kit.SettingRow(c, d.t("world.direct"), "", func() {
				controls(c, func() {
					kit.TextInput(c.Key("new-address"), &d.worldNewDirectURL).Label(d.t("world.direct")).Grow(1).Shrink(1).MinWidth(0)
					url := strings.TrimSpace(d.worldNewDirectURL)
					if kit.Button(c.Key("url-add"), d.t("world.add")).Label(d.t("world.add") + " · " + d.t("world.direct")).Disabled(url == "").Clicked() {
						n.DirectURLs = append(n.DirectURLs, url)
						d.worldNewDirectURL = ""
					}
				})
			})
			kit.InlineHint(c, d.t("world.directHint"), false)
			remove := -1
			for i := range n.DirectURLs {
				row := c.Key("url" + strconv.Itoa(i))
				ui.Row(row).Padding(t.Space(2), t.Space(3)).Gap(t.Space(2)).AlignItems(ui.Center).Children(func() {
					kit.TextInput(row.Key("address"), &n.DirectURLs[i]).Label(d.t("world.direct")).Grow(1).Shrink(1).MinWidth(0)
					if kit.IconButtonTip(row.Key("remove"), kit.IconX, d.t("world.remove")).Clicked() {
						remove = i
					}
				})
			}
			if remove >= 0 {
				n.DirectURLs = slices.Delete(n.DirectURLs, remove, remove+1)
			}
		})
	}
	kit.SettingRow(c, d.t("world.adminDetails"), "", func() {
		if kit.Button(c, d.t("world.openWeb")).Clicked() {
			web, err := cli.GetWebUIURL(d.connectedServer.URL, d.connectedServer.Token)
			if err != nil {
				d.showFailure(err)
				return
			}
			u, err := url.Parse(web)
			if err == nil {
				u.Path = strings.TrimRight(u.Path, "/") + "/s/" + d.serverID + "/settings/world-admin"
				u.RawQuery = "tab=admin"
				err = mygo.Shell.OpenExternal(u.String())
			}
			if err != nil {
				d.showFailure(err)
			}
		}
	})
}

func (d *Desktop) worldJoinFields(c *ui.Context) {
	d.settingInputRow(c, "world-invite", d.t("world.invite"), "", &d.worldInviteCode, settingInputOpt{Password: true})

}

func (d *Desktop) canJoinWorld() bool {
	return strings.TrimSpace(d.worldInviteCode) != ""
}

func (d *Desktop) confirmWorldSave(join bool) {
	d.worldChangeOpen = false
	kind := "world-save"
	if join {
		kind = "world-join"
	}
	d.confirm = confirmation{Kind: kind}
	d.confirmError = nil
	d.confirmOpen = true
}

func (d *Desktop) worldRestartWarning() string {
	warning := d.t("world.warning")
	if d.worldSaved.Settings.Server.IsDatabaseCreated && d.worldDraft.Server.Domain != d.worldSaved.Settings.Server.Domain {
		warning += "\n" + fmt.Sprintf(d.t("world.archive"), d.worldDraft.Server.Domain)
	}
	return warning
}

func (d *Desktop) saveWorldSettings(join bool) {
	client, request := d.client, d.settingsRequest
	params := runtimeprotocol.SetWorldParams{Settings: d.worldDraft}
	params.Settings.Relays = slices.Clone(params.Settings.Relays)
	params.Settings.Node.AdvertiseTargets = slices.Clone(params.Settings.Node.AdvertiseTargets)
	params.Settings.Node.DirectURLs = slices.Clone(params.Settings.Node.DirectURLs)
	if join {
		params.Join = &runtimeprotocol.WorldJoinParams{InviteCode: d.worldInviteCode}
	}
	d.worldSaving = true
	d.worldError = nil
	d.confirmOpen = false
	d.tasks.Go(func() {
		_, err := client.SetWorldSettings(d.ctx, params)
		d.window.Update(func() {
			if d.closed || client != d.client || request != d.settingsRequest {
				return
			}
			d.worldSaving = false
			d.worldError = err
			d.worldRestarting = err == nil
		})
	})
}

// worldQRCode 将远程访问链接编码为桌面可以显示的图片。
func worldQRCode(link string) ui.ImageSource {
	if link == "" {
		return nil
	}
	data, err := qrcode.Encode(link, qrcode.Low, 320)
	if err != nil {
		return nil
	}
	image, _ := ui.DecodeImage(data)
	return image
}
