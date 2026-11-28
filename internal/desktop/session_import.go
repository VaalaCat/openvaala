package desktop

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	kit "github.com/VaalaCat/mygo-kit"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/externalagent"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/egoist/mygo/ui"
)

// sessionImportDialog 保存本次导入打开时的工作位置和列表、预览的请求归属。
type sessionImportDialog struct {
	location                             runtime.ExternalAgentLocation
	icon                                 string
	query, cursor, selected, title       string
	sessions                             []externalagent.ExternalSessionInfo
	preview                              externalagent.ExternalSessionPreview
	listing, resolving, importing        bool
	listRequest, previewRequest          uint64
	listError, previewError, submitError error
}

func (d *Desktop) openSessionImport(agent *runtime.AgentInfo) {
	dialog := &sessionImportDialog{location: runtime.ExternalAgentLocation{AgentName: agent.Name, ExecutorID: d.newExecutor, Cwd: d.newDirectory}, icon: agent.Icon}
	d.importDialog = dialog
	if dialog.location.Cwd == "" {
		d.listImportSessions(dialog, false)
		return
	}
	dialog.resolving = true
	client, location := d.client, dialog.location
	d.tasks.Go(func() {
		cwd, err := client.ResolveWorkspaceDirectory(d.ctx, &runtimeclient.ResolveWorkspaceDirectoryRequest{ExecutorID: location.ExecutorID, Directory: location.Cwd})
		d.window.Update(func() {
			if d.closed || d.importDialog != dialog {
				return
			}
			dialog.resolving = false
			if err != nil {
				d.directoryError = err
				d.showFailure(err)
				d.importDialog = nil
				return
			}
			dialog.location.Cwd = cwd
			d.listImportSessions(dialog, false)
		})
	})
}

func (d *Desktop) listImportSessions(dialog *sessionImportDialog, more bool) {
	req := runtime.ExternalSessionsRequest{ExternalAgentLocation: dialog.location, Query: dialog.query}
	if more {
		req.Cursor = dialog.cursor
	} else {
		dialog.selected, dialog.title, dialog.cursor = "", "", ""
		dialog.sessions = nil
		dialog.previewRequest++
	}
	dialog.listing = true
	dialog.listError = nil
	dialog.listRequest++
	sequence, client := dialog.listRequest, d.client
	d.tasks.Go(func() {
		result, err := client.ListExternalSessions(d.ctx, &req)
		d.window.Update(func() {
			if d.closed || d.importDialog != dialog || dialog.listRequest != sequence {
				return
			}
			dialog.listing = false
			dialog.listError = err
			if err != nil {
				dialog.selected, dialog.title = "", ""
				return
			}
			dialog.sessions = append(dialog.sessions, result.Sessions...)
			slices.SortStableFunc(dialog.sessions, func(a, b externalagent.ExternalSessionInfo) int { return cmp.Compare(b.UpdatedAt, a.UpdatedAt) })
			dialog.cursor = result.NextCursor
		})
	})
}

func (d *Desktop) previewImportSession(dialog *sessionImportDialog, info externalagent.ExternalSessionInfo) {
	dialog.previewRequest++
	sequence, client := dialog.previewRequest, d.client
	dialog.preview = externalagent.ExternalSessionPreview{}
	dialog.previewError, dialog.submitError = nil, nil
	if dialog.selected == info.SessionID {
		dialog.selected, dialog.title = "", ""
		return
	}
	dialog.selected, dialog.title = info.SessionID, info.Title
	req := runtime.ExternalSessionRequest{ExternalAgentLocation: dialog.location, ExternalSessionID: info.SessionID}
	d.tasks.Go(func() {
		result, err := client.PreviewExternalSession(d.ctx, &req)
		d.window.Update(func() {
			if !d.closed && d.importDialog == dialog && sequence == dialog.previewRequest {
				dialog.preview, dialog.previewError = result, err
			}
		})
	})
}

func (d *Desktop) importSessionView(c *ui.Context) {
	dialog := d.importDialog
	if dialog == nil {
		return
	}
	t := kit.GetTheme(c)
	opt := kit.DialogOpt{Title: fmt.Sprintf(d.t("import.agent.session"), dialog.location.AgentName), ConfirmLabel: d.t("import"), ConfirmDisabled: dialog.importing || strings.TrimSpace(dialog.selected) == ""}
	opt.HeaderExtra = func() {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Icon(c, agentBrandIcon(dialog.icon)).Size(16, 16)
			t.TextRole(ui.Text(c, strings.TrimSpace(dialog.location.ExecutorID+"  "+cmp.Or(dialog.location.Cwd, getDefaultDirectory(d.executors, dialog.location.ExecutorID)))).SingleLine(), kit.TextCaption).TextColor(t.Colors.Muted)
		})
	}
	open := true
	confirmed := kit.DialogWith(c.Key("session-import"), &open, opt, func() {
		if dialog.listError != nil {
			kit.FieldMessage(c, kit.MessageError, externalErrorText(dialog.listError))
			kit.TextInputWith(c.Key("external-session-id"), &dialog.selected, kit.InputOpt{Placeholder: d.t("external.session.id")}).Label(d.t("external.session.id"))
		} else {
			search := kit.TextInputWith(c.Key("external-session-search"), &dialog.query, kit.InputOpt{Placeholder: d.t("search.sessions")}).Label(d.t("search.sessions")).Disabled(dialog.resolving)
			if search.Changed() {
				d.listImportSessions(dialog, false)
			}
			ui.Column(c).Gap(0).Children(func() {
				for _, info := range dialog.sessions {
					selected := info.SessionID == dialog.selected
					label := info.Title
					if label == "" {
						label = fmt.Sprintf("%s %s", d.t("untitled.session"), info.SessionID[:min(len(info.SessionID), 8)])
					}
					row := kit.ListRow(c.Key(info.SessionID), kit.ListRowOpt{Title: label, Description: info.Cwd, Selected: selected, Trailing: func() {
						if info.UpdatedAt != 0 {
							t.TextRole(ui.Text(c, time.UnixMilli(info.UpdatedAt).Local().Format("2006-01-02 15:04")), kit.TextCaption).TextColor(t.Colors.Muted)
						}
					}}).FillWidth()
					kit.Tooltip(c, row, info.Cwd)
					if row.Clicked() {
						d.previewImportSession(dialog, info)
					}
					if selected {
						ui.Column(c.Key("preview-"+info.SessionID)).Transition(composerTransition).Padding(0, 12, 8, 12).Background(t.Colors.Selected).Children(func() {
							ui.Column(c).BorderWidth(0, 0, 0, 1).BorderColor(t.Colors.Border).Padding(0, 0, 0, 8).Children(func() {
								if dialog.previewError != nil {
									kit.FieldMessage(c, kit.MessageError, externalErrorText(dialog.previewError))
									return
								}
								for _, msg := range []string{dialog.preview.FirstUserMessage, dialog.preview.LastUserMessage} {
									if msg != "" {
										t.TextRole(ui.Text(c, msg), kit.TextCaption).TextColor(t.Colors.Muted)
									}
								}
							})
						})
					}
				}
			})
			if dialog.listing || dialog.resolving {
				kit.InlineHint(c, d.t("loading"), true)
			} else if dialog.cursor != "" && kit.Button(c, d.t("load.more")).Clicked() {
				d.listImportSessions(dialog, true)
			}
		}
		if dialog.submitError != nil {
			d.errorMessageView(c, dialog.submitError)
		}
	})
	if confirmed {
		dialog.importing = true
		req := runtime.ImportSessionRequest{ExternalSessionRequest: runtime.ExternalSessionRequest{ExternalAgentLocation: dialog.location, ExternalSessionID: strings.TrimSpace(dialog.selected)}, Title: dialog.title}
		client := d.client
		d.tasks.Go(func() {
			result, err := client.ImportSession(d.ctx, &req)
			d.window.Update(func() {
				if d.closed || client != d.client {
					return
				}
				if err == nil {
					d.loadConversations(true)
				}
				if dialog != d.importDialog {
					return
				}
				dialog.submitError, dialog.importing = err, false
				if err == nil {
					d.importDialog = nil
					d.openConversationFromList(result.Conversation)
				}
			})
		})
	} else if !open {
		d.importDialog = nil
	}
}

func externalErrorText(err error) string {
	var response *runtimeclient.ResponseError
	if errors.As(err, &response) {
		return response.Message
	}
	return err.Error()
}
