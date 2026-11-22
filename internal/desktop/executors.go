package desktop

import (
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/VaalaCat/mygo-kit"
	"github.com/VaalaCat/openvaala/internal/config"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/egoist/mygo/ui"
)

var executorIDRegexp = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// isBuiltinExecutorID ID 是否为保留的内置 Executor？
func isBuiltinExecutorID(id string) bool {
	return id == defs.LocalExecutorID || id == config.BuiltinExecutorID || id == defs.MemoryExecutorID
}

// loadExecutorConfigs 读取 executors/*.yaml 里的原样 Executor 配置，保留正在编辑的未保存草稿。
func (d *Desktop) loadExecutorConfigs() {
	if d.client == nil || d.executorConfigsLoading {
		return
	}
	d.executorConfigsLoading = true
	client := d.client
	d.tasks.Go(func() {
		configs, err := client.ListExecutorConfigs(d.ctx)
		if d.window == nil {
			return
		}
		d.window.Update(func() {
			if d.closed || d.client != client {
				return
			}
			d.executorConfigsLoading = false
			d.executorConfigsError = err
			if err != nil {
				return
			}
			d.executorConfigs = configs
			if d.executorDraft != nil && d.executorDraft.EditingID != "" {
				for _, cfg := range configs {
					if cfg.ID == d.executorDraft.EditingID {
						fresh := executorToDraft(cfg)
						if d.isExecutorDirty() {
							if !reflect.DeepEqual(fresh, d.executorBaseline) {
								d.showToast(kit.ToastOpt{Title: d.t("configuration.changed.externally.your.draft.is.preserved"), Kind: kit.ToastInfo})
							}
						} else {
							d.executorDraft = fresh
							d.executorBaseline = cloneExecutorDraft(fresh)
						}
						break
					}
				}
			}
		})
	})
}

// loadSSHConfigHosts 读取 serve 本机 ~/.ssh/config 里的主机列表（弱依赖：失败静默当空列表处理）。
func (d *Desktop) loadSSHConfigHosts() {
	if d.client == nil {
		return
	}
	client := d.client
	d.tasks.Go(func() {
		hosts, err := client.ListSSHConfigHosts(d.ctx)
		if d.window == nil {
			return
		}
		d.window.Update(func() {
			if d.closed || d.client != client {
				return
			}
			d.sshConfigHostsLoaded = true
			if err != nil {
				d.sshConfigHosts = nil
				return
			}
			d.sshConfigHosts = hosts
		})
	})
}

// isExecutorDirty 当前 Executor 编辑表单相比基线是否有未保存改动？
func (d *Desktop) isExecutorDirty() bool {
	if d.executorDraft == nil {
		return false
	}
	return !reflect.DeepEqual(d.executorDraft, d.executorBaseline)
}

// executorToDraft 把 ExecutorConfig 转成编辑表单草稿。
func executorToDraft(cfg defs.ExecutorConfig) *executorDraft {
	draft := &executorDraft{
		EditingID:          cfg.ID,
		ID:                 cfg.ID,
		Description:        cfg.Description,
		WorkspaceDirectory: cfg.WorkspaceDirectory,
	}
	if cfg.SSH != nil {
		draft.Address = cfg.SSH.Address
		draft.IdentityFile = cfg.SSH.IdentityFile
	}
	return draft
}

// cloneExecutorDraft 复制一份 Executor 草稿作为回滚基线。
func cloneExecutorDraft(draft *executorDraft) *executorDraft {
	if draft == nil {
		return nil
	}
	cloned := *draft
	return &cloned
}

// openExecutor 打开新建或已有 SSH Executor 的编辑表单；当前草稿有未保存修改时先确认。
func (d *Desktop) openExecutor(cfg defs.ExecutorConfig, editingID string) {
	if d.isExecutorDirty() {
		d.confirm = confirmation{Kind: "select-executor", Executor: cfg, ExecutorID: editingID}
		d.confirmOpen = true
		return
	}
	draft := executorToDraft(cfg)
	draft.EditingID = editingID
	d.executorDraft = draft
	d.executorBaseline = cloneExecutorDraft(draft)
	d.executorError = nil
	d.executorTestSuccess = ""
	d.executorTestError = nil
	d.sshImportSelected = ""
}

// applySSHConfigHost把选中的 ~/.ssh/config 主机项填入新增表单的 ID、SSH 地址和私钥路径。
func (d *Desktop) applySSHConfigHost(alias string) {
	if d.executorDraft == nil || alias == "" {
		return
	}
	index := slices.IndexFunc(d.sshConfigHosts, func(h defs.SSHConfigHost) bool { return h.Alias == alias })
	if index < 0 {
		return
	}
	host := d.sshConfigHosts[index]
	if d.executorDraft.EditingID == "" && (strings.TrimSpace(d.executorDraft.ID) == "" || d.executorDraft.ID == d.sshImportSelected) {
		d.executorDraft.ID = host.Alias
		d.executorDraft.IDError = ""
	}
	d.sshImportSelected = alias
	d.executorDraft.Address = host.Address
	d.executorDraft.IdentityFile = host.IdentityFile
	d.executorTestSuccess = ""
	d.executorTestError = nil
}

// executorsSettingsView 绘制设置 → Executor 页：内置只读项、可编辑 SSH 项以及下方 SSH 编辑表单。
func (d *Desktop) executorsSettingsView(c *ui.Context) {
	// 1. 展示配置读取错误与加载提示。
	if d.executorConfigsError != nil {
		d.errorMessageView(c, d.executorConfigsError)
	}
	if len(d.executors) == 0 && len(d.executorConfigs) == 0 {
		if d.executorsLoading || d.executorConfigsLoading {
			kit.InlineHint(c, d.t("loading"), true)
			return
		}
		kit.FieldMessage(c, kit.MessageDescription, d.t("settings.executors.empty"))
	} else {
		configByID := make(map[string]defs.ExecutorConfig, len(d.executorConfigs))
		for _, cfg := range d.executorConfigs {
			configByID[cfg.ID] = cfg
		}
		editingID := ""
		if d.executorDraft != nil {
			editingID = d.executorDraft.EditingID
		}
		kit.SettingSection(c, d.t("settings.executors"), func() {
			for _, exec := range d.executors {
				exec := exec
				cfg, hasCfg := configByID[exec.ID]
				isBuiltin := isBuiltinExecutorID(exec.ID)
				if !isBuiltin && hasCfg && cfg.Kind == defs.ExecutorKindSSH {
					descParts := []string{}
					if cfg.SSH != nil && cfg.SSH.Address != "" {
						descParts = append(descParts, cfg.SSH.Address)
					}
					if cfg.WorkspaceDirectory != "" {
						descParts = append(descParts, cfg.WorkspaceDirectory)
					}
					row := kit.ListRow(c.Key("executor-ssh:"+cfg.ID), kit.ListRowOpt{
						Title:       cfg.ID,
						Description: strings.Join(descParts, " · "),
						Selected:    cfg.ID == editingID,
						Trailing: func() {
							kit.Badge(c, "SSH", kit.BadgeAccent)
						},
					}).FillWidth().Label(cfg.ID)
					if row.Clicked() {
						d.openExecutor(cfg, cfg.ID)
					}
					continue
				}
				source := "config.yaml"
				if isBuiltin {
					source = d.t("source.builtin")
				}
				d.settingsItemView(c, "executor:"+exec.ID, exec.ID, exec.Description, source, exec, func() {
					if exec.WorkspaceDirectory != "" {
						kit.Facts(c, kit.Fact{Label: d.t("workspace.directory"), Value: exec.WorkspaceDirectory})
					}
				})
			}
		})
	}
	// 2. 打开新增或选中已有 SSH Executor 时渲染编辑表单。
	if d.executorDraft != nil {
		d.executorFormView(c)
	}
}

// executorFormView 绘制 SSH Executor 表单：统一收敛在单组卡片里，减少嵌套与说明文字。
func (d *Desktop) executorFormView(c *ui.Context) {
	t := kit.GetTheme(c)
	draft := d.executorDraft
	isCreating := draft.EditingID == ""
	if d.executorError != nil {
		d.errorMessageView(c, d.executorError)
	}
	title := d.t("executor.add.ssh")
	if !isCreating {
		title = d.t("executor.edit.ssh")
	}
	kit.SettingSection(c.Key("executor-form"), title, func() {
		// 1. 新增态且 serve 本机有 ~/.ssh/config 主机时，首行提供一键导入下拉。
		if isCreating && len(d.sshConfigHosts) > 0 {
			values := make([]string, len(d.sshConfigHosts))
			labels := make([]string, len(d.sshConfigHosts))
			for i, host := range d.sshConfigHosts {
				values[i] = host.Alias
				labels[i] = fmt.Sprintf("%s (%s)", host.Alias, host.Address)
			}
			kit.SettingRow(c, d.t("executor.ssh.import"), "", func() {
				selected := d.sshImportSelected
				if d.settingSelectView(c, "ssh-config-host", d.t("executor.ssh.import.placeholder"), &selected, kit.SelectOpt{
					Options:    selectOptions(values, labels),
					Searchable: true,
				}) {
					d.applySSHConfigHost(selected)
				}
			})
		}
		// 2. 核心字段：ID、说明、地址、私钥、工作目录，用 placeholder 承载格式提示。
		if d.settingInputRow(c, "executor-id", "ID", "", &draft.ID, settingInputOpt{ReadOnly: !isCreating, Error: draft.IDError, Placeholder: "dev-box"}) {
			draft.IDError = ""
		}
		d.settingInputRow(c, "executor-description", d.t("executor.description"), "", &draft.Description, settingInputOpt{Placeholder: d.t("executor.description.placeholder")})
		if d.settingInputRow(c, "executor-ssh-address", d.t("executor.ssh.address"), "", &draft.Address, settingInputOpt{Placeholder: d.t("executor.ssh.address.placeholder")}) {
			d.executorTestSuccess = ""
			d.executorTestError = nil
		}
		if d.settingInputRow(c, "executor-ssh-identity", d.t("executor.ssh.identity"), "", &draft.IdentityFile, settingInputOpt{Placeholder: d.t("executor.ssh.identity.placeholder")}) {
			d.executorTestSuccess = ""
			d.executorTestError = nil
		}
		if d.settingInputRow(c, "executor-workspace-dir", d.t("executor.workspace.directory"), "", &draft.WorkspaceDirectory, settingInputOpt{Placeholder: d.t("executor.workspace.directory.placeholder")}) {
			d.executorTestSuccess = ""
			d.executorTestError = nil
		}
	})
	if d.executorTesting {
		kit.InlineHint(c, d.t("executor.testing"), true)
	} else if d.executorTestSuccess != "" {
		ui.Row(c).Gap(t.Space(2)).AlignItems(ui.Center).Children(func() {
			kit.StatusDot(c, t.Colors.Success)
			t.TextRole(ui.Text(c, fmt.Sprintf(d.t("executor.test.success"), d.executorTestSuccess)), kit.TextCaption).TextColor(t.Colors.Success).Shrink(1)
		})
	} else if d.executorTestError != nil {
		d.errorMessageView(c, d.executorTestError)
	}
	// 3. 编辑已有项时底部放危险删除操作。
	if !isCreating {
		kit.SettingSection(c.Key("executor-danger"), d.t("executor.danger.section"), func() {
			kit.SettingRow(c, d.t("executor.delete"), "", func() {
				if kit.DangerButton(c.Key("delete-executor"), d.t("executor.delete")).Disabled(d.client == nil || d.clientStopped || d.executorSaving).Clicked() {
					d.confirm = confirmation{Kind: "delete-executor", ExecutorID: draft.EditingID}
					d.confirmOpen = true
				}
			})
		})
	}
}

// testExecutor 调用 executor/test 测试当前表单的 SSH 连通性与远端目录，不写入任何文件。
func (d *Desktop) testExecutor() {
	if d.client == nil || d.executorDraft == nil || d.executorTesting {
		return
	}
	d.executorTestRequest++
	request := d.executorTestRequest
	d.executorTesting = true
	d.executorTestSuccess = ""
	d.executorTestError = nil
	sshCfg := defs.SSHExecutorConfig{
		Address:      strings.TrimSpace(d.executorDraft.Address),
		IdentityFile: strings.TrimSpace(d.executorDraft.IdentityFile),
	}
	dir := strings.TrimSpace(d.executorDraft.WorkspaceDirectory)
	client := d.client
	d.tasks.Go(func() {
		resolved, err := client.TestSSHExecutor(d.ctx, sshCfg, dir)
		if d.window == nil {
			return
		}
		d.window.Update(func() {
			if d.closed || d.client != client || d.executorTestRequest != request {
				return
			}
			d.executorTesting = false
			if err != nil {
				d.executorTestError = err
				return
			}
			d.executorTestSuccess = resolved
		})
	})
}

// saveExecutor 校验表单并调用 executor/set 原子写入并热重载，成功后刷新列表。
func (d *Desktop) saveExecutor() {
	if d.client == nil || d.executorDraft == nil || d.executorSaving {
		return
	}
	draft := d.executorDraft
	id := strings.TrimSpace(draft.ID)
	if draft.EditingID == "" {
		switch {
		case id == "":
			draft.IDError = d.t("executor.error.id.required")
			return
		case isBuiltinExecutorID(id):
			draft.IDError = d.t("executor.error.id.builtin")
			return
		case !executorIDRegexp.MatchString(id):
			draft.IDError = d.t("executor.error.id.invalid")
			return
		case slices.ContainsFunc(d.executorConfigs, func(e defs.ExecutorConfig) bool { return e.ID == id }):
			draft.IDError = d.t("executor.error.id.exists")
			return
		}
	}
	draft.IDError = ""
	cfg := defs.ExecutorConfig{
		ID:                 id,
		Kind:               defs.ExecutorKindSSH,
		Description:        strings.TrimSpace(draft.Description),
		WorkspaceDirectory: strings.TrimSpace(draft.WorkspaceDirectory),
		SSH: &defs.SSHExecutorConfig{
			Address:      strings.TrimSpace(draft.Address),
			IdentityFile: strings.TrimSpace(draft.IdentityFile),
		},
	}
	d.executorSaving = true
	d.executorError = nil
	client := d.client
	d.tasks.Go(func() {
		result, err := client.SetExecutor(d.ctx, cfg)
		if d.window == nil {
			return
		}
		d.window.Update(func() {
			if d.closed || d.client != client {
				return
			}
			d.executorSaving = false
			d.executorError = err
			if err != nil {
				return
			}
			savedDraft := executorToDraft(cfg)
			d.executorDraft = savedDraft
			d.executorBaseline = cloneExecutorDraft(savedDraft)
			clear(d.skillCatalogs)
			d.loadExecutors()
			d.loadExecutorConfigs()
			detail := ""
			if len(result.PendingConversationIDs) > 0 {
				detail = fmt.Sprintf("%s: %s", fmt.Sprintf(d.t("pending.conversations.apply.when.idle"), len(result.PendingConversationIDs)), joinConversationIDs(result.PendingConversationIDs))
			}
			d.showSuccess(d.t("executor.saved"), detail)
		})
	})
}
