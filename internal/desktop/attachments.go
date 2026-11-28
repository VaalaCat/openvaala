package desktop

import (
	"cmp"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	kit "github.com/VaalaCat/mygo-kit"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/egoist/mygo/ui"
)

const attachmentLimit = 10 << 20

func (draft *conversationDraft) filesReady() bool {
	for _, file := range draft.Files {
		if file.loading || file.err != nil && file.data == nil {
			return false
		}
	}
	return true
}

func uploadDraftFile(ctx context.Context, client runtimeclient.Client, conversationID defs.ConversationID, executorID string, file defs.FileRef, data []byte) (defs.FileRef, error) {
	return client.UploadAttachment(ctx, &runtimeprotocol.UploadAttachmentParams{ConversationID: conversationID, ExecutorID: executorID, Name: file.Name, MIME: file.MIME, Data: base64.StdEncoding.EncodeToString(data)})
}

// addDraftFiles 保留选择顺序；已有对话立即上传普通文件，新建时保存本地内容直到发送。
func (d *Desktop) addDraftFiles(key draftKey, draft *conversationDraft, executorID string, paths []string) {
	if len(paths) == 0 {
		return
	}
	if draft.Files == nil {
		draft.Files = make(map[*defs.Content]*draftFile)
	}
	client := d.client
	limitError := fmt.Errorf("%s", d.t("file.too.large"))
	regularError := fmt.Errorf("%s", d.t("file.not.regular"))
	for _, path := range paths {
		part := &defs.Content{Kind: defs.ContentFile, File: &defs.FileRef{Name: filepath.Base(path)}}
		state := &draftFile{loading: true}
		draft.Files[part] = state
		draft.Content = append(draft.Content, part)
		name := part.File.Name
		d.tasks.Go(func() {
			file := defs.FileRef{Name: name, MIME: mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))}
			var data []byte
			var images []imageAttachment
			info, err := os.Stat(path)
			if err == nil {
				file.Size = info.Size()
				switch {
				case !info.Mode().IsRegular():
					err = regularError
				case file.Size > attachmentLimit:
					err = limitError
				default:
					var f *os.File
					f, err = os.Open(path)
					if err == nil {
						data, err = io.ReadAll(io.LimitReader(f, attachmentLimit+1))
						_ = f.Close()
						file.Size = int64(len(data))
						if file.Size > attachmentLimit {
							err = limitError
						}
					}
				}
			}
			if err == nil {
				detected := http.DetectContentType(data)
				if file.MIME == "" || strings.HasPrefix(detected, "image/") {
					file.MIME = detected
				}
				if strings.HasPrefix(file.MIME, "image/") {
					images, err = prepareImageAttachments(d.ctx, [][]byte{data}, []string{name})
					data = nil
				} else if key.ConversationID != "" && client != nil {
					uploaded, uploadErr := uploadDraftFile(d.ctx, client, key.ConversationID, executorID, file, data)
					err = uploadErr
					if err == nil {
						file = uploaded
						data = nil
					}
				}
			} else {
				data = nil
			}
			d.window.Update(func() {
				if d.closed || draft.Files[part] != state {
					return
				}
				state.loading, state.err, state.data = false, err, data
				part.File = &file
				if err != nil {
					return
				}
				if len(images) > 0 {
					img := images[0]
					if draftImageBytes(draft.Content)+img.Bytes > attachmentLimit {
						state.err = fmt.Errorf("%s", d.t("images.exceed.10.mib"))
						return
					}
					*part = *img.Content
					d.images[part.Image.DataURL] = &imageResource{Source: img.Bitmap}
				}
				if data == nil {
					delete(draft.Files, part)
				}
			})
		})
	}
}

// attachmentStrip 在草稿和用户消息中使用同一行附件，图片保留原图预览。
func (d *Desktop) attachmentStrip(c *ui.Context, content []*defs.Content, draft *conversationDraft) {
	var parts []*defs.Content
	for _, part := range content {
		if part != nil && (part.Image != nil || part.File != nil) {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return
	}
	t := kit.GetTheme(c)
	color := t.Colors.Panel
	if draft != nil {
		color = t.Colors.Editor
	}
	strip := ui.Box(c.Key("attachment-strip")).Height(40).MinWidth(0).Clip()
	scroll := ui.Local(strip, "scroll", func() ui.ScrollState { return ui.ScrollState{} })
	last := ui.Local(strip, "last-file", func() *defs.Content { return nil })
	strip.Children(func() {
		scroller := ui.ScrollHorizontal(c).Fill().ScrollbarInsets(0, 0, -16, 0).TrackScroll(scroll)
		scroller.HandleInput(func(ev ui.InputEvent) bool {
			if ev.Kind != ui.InputScroll || ev.DX != 0 || ev.DY == 0 || scroll.MaxX == 0 {
				return false
			}
			x := max(0, min(scroll.MaxX, scroll.X+ev.DY))
			if x == scroll.X {
				return false
			}
			scroll.X = x
			return true
		})
		scroller.Children(func() {
			ui.Row(c).Gap(8).Children(func() {
				for _, part := range parts {
					var state *draftFile
					if draft != nil {
						state = draft.Files[part]
					}
					card := d.attachmentCard(c, part, draft, state)
					if draft != nil && part == parts[len(parts)-1] && part != *last {
						card.ScrollIntoView()
					}
				}
			})
		})
	})
	*last = parts[len(parts)-1]
	strip.DrawOver(func(p *ui.Painter, r ui.Rect) {
		left, right := r, r
		left.W = 16
		right.X += right.W - 16
		right.W = 16
		if scroll.X > 0 {
			p.FillGradient(left, ui.LinearGradient{From: color, To: color.Alpha(0), Angle: 90}, 0)
		}
		if scroll.X < scroll.MaxX {
			p.FillGradient(right, ui.LinearGradient{From: color.Alpha(0), To: color, Angle: 90}, 0)
		}
	})
}

func (d *Desktop) attachmentCard(c *ui.Context, part *defs.Content, draft *conversationDraft, state *draftFile) ui.Element {
	t := kit.GetTheme(c)
	// 节点快照可以异步读取。键只记录附件身份，不引用仍在读取或上传的内容。
	key := fmt.Sprintf("attachment-%p", part)
	var card ui.Element
	var name string
	if part.Image != nil {
		name = cmp.Or(part.Image.Alt, d.t("image.files"))
		card = ui.ButtonBase(c.Key(key)).Size(40, 40).Padding(0).Radius(t.Metrics.Radius).Clip().Label(name)
		image := d.getImageResource(part.Image.DataURL, name)
		card.Children(func() {
			if image.Source != nil {
				ui.Image(c, image.Source).Fill().Fit(ui.Cover)
			} else {
				ui.Icon(c, kit.IconImagePlus).Size(16, 16)
			}
		})
		if card.Clicked() {
			d.imagePreview, d.imagePreviewOpen = part.Image.DataURL, true
			d.imagePreviewState = kit.ImagePreviewState{Fit: true}
		}
	} else {
		file := part.File
		name = file.Name
		card = ui.Row(c.Key(key)).Role(ui.RoleListItem).Label(name).Height(40).Shrink(0).MaxWidth(200).Padding(0, 8).Gap(8).AlignItems(ui.Center).Radius(t.Metrics.Radius).Background(t.Colors.Elevated).Border(1, t.Colors.Border)
		hint := file.Path
		if state != nil && state.err != nil {
			hint = externalErrorText(state.err)
			card.BorderColor(t.Colors.Danger)
		}
		card.Children(func() {
			if state != nil && state.loading && (draft == nil || d.readingTarget.ConversationID != "") {
				kit.Spinner(c, 16)
			} else {
				ui.Icon(c, fileIcon(file.MIME)).Size(16, 16)
			}
			ui.Column(c).MinWidth(0).Shrink(1).Children(func() {
				t.TextRole(ui.Text(c, name).SingleLine(), kit.TextUI)
				detail := attachmentSize(file.Size)
				ink := t.Colors.Muted
				if state != nil && state.err != nil {
					detail, ink = hint, t.Colors.Danger
				}
				t.TextRole(ui.Text(c, detail).SingleLine(), kit.TextCaption).TextColor(ink)
			})
		})
		if hint != "" {
			kit.Tooltip(c, card, hint)
		}
	}
	if draft != nil && !draft.Pending && (card.Hovered() || card.FocusWithin()) {
		card.Children(func() {
			if kit.SmallIconButton(c, kit.IconX, fmt.Sprintf(d.t("remove.file"), name)).Draw(nil).Absolute().Top(0).Right(0).Background(t.Colors.Elevated).Clicked() {
				draft.Content = slices.DeleteFunc(draft.Content, func(item *defs.Content) bool { return item == part })
				delete(draft.Files, part)
			}
		})
	}
	return card.Transition(composerTransition)
}

func draftImageBytes(content []*defs.Content) int {
	total := 0
	for _, part := range content {
		if part != nil && part.Image != nil {
			_, encoded, _ := strings.Cut(part.Image.DataURL, ",")
			total += base64.RawStdEncoding.DecodedLen(len(strings.TrimRight(encoded, "=")))
		}
	}
	return total
}

func attachmentSize(bytes int64) string {
	if bytes >= 1<<20 {
		return fmt.Sprintf("%.1f MB", float64(bytes)/(1<<20))
	}
	if bytes >= 1<<10 {
		return fmt.Sprintf("%.1f KB", float64(bytes)/(1<<10))
	}
	return fmt.Sprintf("%d B", bytes)
}

func fileIcon(mime string) *ui.SVG {
	switch {
	case strings.Contains(mime, "zip") || strings.Contains(mime, "compressed") || strings.Contains(mime, "tar"):
		return kit.IconArchive
	case strings.Contains(mime, "json") || strings.Contains(mime, "xml") || strings.Contains(mime, "javascript"):
		return kit.IconBraces
	case strings.Contains(mime, "sheet") || strings.Contains(mime, "csv"):
		return kit.IconListTree
	case strings.HasPrefix(mime, "text/"):
		return kit.IconFileText
	default:
		return kit.IconFiles
	}
}
