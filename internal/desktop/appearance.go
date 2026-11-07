package desktop

import (
	"embed"
	"time"

	"github.com/VaalaCat/mygo-kit"
	"github.com/egoist/mygo/ui"
)

//go:embed fonts/NotoSerif.ttf fonts/NotoSerif-OFL.txt
var fonts embed.FS

const (
	sidebarRowHeight  = 48 // 侧栏对话行：标题与说明两行
	itemFadeDuration  = 160 * time.Millisecond
	panelFadeDuration = 120 * time.Millisecond
	// conversationColumnWidth 是消息列与输入框内容的最大宽度，在中间区水平居中。
	conversationColumnWidth = 800
	// settingInputWidth 是设置行右侧输入框的宽度。
	settingInputWidth = 280
)

// registerFonts 注册 Zed 界面与代码字体，以及可选的 Noto Serif 正文字体。
func registerFonts() error {
	// 1. 先注册组件库内置的 IBM Plex Sans 与 Lilex。
	if err := kit.RegisterFonts(); err != nil {
		return err
	}
	// 2. 再注册设置里可选的衬线正文字体。
	data, err := fonts.ReadFile("fonts/NotoSerif.ttf")
	if err != nil {
		return err
	}
	return ui.RegisterFont(data, "Noto Serif")
}

// applyAppearance 将桌面外观偏好映射到通用工作台主题。
func (d *Desktop) applyAppearance(c *ui.Context) {
	// 1. 明暗跟随系统或用户选择，字体与密度采用既定正文角色。
	// 手测（macOS 真机）：设置 → 通用里主题选「跟随系统」，在系统设置里切换浅色/深色，不重启应用即时生效；
	// 在 Retina 屏上文字清晰、分隔线和边框是 1 物理像素的细线，拖到非 Retina 外接屏后仍清晰。
	dark := c.Theme().Dark
	if d.preferences.Theme != "system" {
		dark = d.preferences.Theme == "dark"
	}
	t := kit.OneLight()
	if dark {
		t = kit.OneDark()
	}
	if d.preferences.BodyFont != "" {
		t.Typography.Font = d.preferences.BodyFont
	}
	// 2. stone 覆盖中性色及强调色，blue 保留 One 主题的蓝色强调。
	if d.preferences.Palette == "stone" {
		if dark {
			t.Colors.Editor = ui.Hex("#1c1917")
			t.Colors.Panel = ui.Hex("#292524")
			t.Colors.Border = ui.Hex("#44403c")
			t.Colors.Text = ui.Hex("#fafaf9")
			t.Colors.Muted = ui.Hex("#a8a29e")
			t.Colors.Accent = ui.Hex("#d6d3d1")
			t.Colors.AccentText = ui.Hex("#1c1917")
		} else {
			t.Colors.Editor = ui.Hex("#fafaf9")
			t.Colors.Panel = ui.Hex("#f5f5f4")
			t.Colors.Border = ui.Hex("#d6d3d1")
			t.Colors.Text = ui.Hex("#1c1917")
			t.Colors.Muted = ui.Hex("#78716c")
			t.Colors.Accent = ui.Hex("#44403c")
			t.Colors.AccentText = ui.Hex("#fafaf9")
		}
		t.Colors.Elevated = t.Colors.Panel
		t.Colors.Input = t.Colors.Editor
		t.Colors.BorderSubtle = t.Colors.Border
		t.Colors.Hover = t.Colors.Panel
		t.Colors.Selected = t.Colors.Border
	}
	if d.preferences.Palette == "blue" {
		if dark {
			t.Colors.Accent = ui.Hex("#60a5fa")
		} else {
			t.Colors.Accent = ui.Hex("#2563eb")
		}
		t.Colors.AccentText = ui.Hex("#ffffff")
	}
	kit.ApplyTheme(c, t)
	t.TextRole(c.Root(), kit.TextUI)
}
