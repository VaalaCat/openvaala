package desktop

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPreferencesFailures 检查坏文件不被默认值覆盖，写入失败不丢本次选择。
func TestPreferencesFailures(t *testing.T) {
	// 1. 外部 JSON 在读取处验证一次，损坏保持原文。
	for _, tt := range []struct{ name, content string }{{"broken", "{broken"}, {"invalid size", `{"width":-1}`}, {"unknown theme", `{"theme":"unavailable"}`}, {"unknown right panel", `{"workspace":{"right":{"active_panel":"missing"}}}`}, {"negative dock size", `{"workspace":{"bottom":{"active_panel":"rpc","size":-1}}}`}} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "preferences.json")
			if err := os.WriteFile(path, []byte(tt.content), 0600); err != nil {
				t.Fatal(err)
			}
			d := testDesktop()
			d.preferencesPath = path
			if err := d.loadPreferences(); err == nil {
				t.Fatal("bad preferences accepted")
			}
			d.preferences.Palette = "blue"
			if err := d.savePreferences(); err == nil {
				t.Fatal("bad file overwritten")
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != tt.content {
				t.Fatalf("original changed: %s %v", data, err)
			}
			if d.preferences.Palette != "blue" {
				t.Fatal("memory preference lost")
			}
		})
	}
	// 2. 父路径不是目录时保存失败，本次内存值不变。
	t.Run("unwritable", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(path, []byte("file"), 0600); err != nil {
			t.Fatal(err)
		}
		d := testDesktop()
		d.preferencesPath = filepath.Join(path, "preferences.json")
		d.preferences.Language = "en"
		if err := d.savePreferences(); err == nil {
			t.Fatal("write unexpectedly succeeded")
		}
		if d.preferences.Language != "en" {
			t.Fatal("memory value lost")
		}
	})
}

// TestPalettePreferences 检查新安装使用 Zed，显式选择仍按用户偏好恢复。
func TestPalettePreferences(t *testing.T) {
	// 1. 各有效配色通过真实偏好文件读取，不引入隐式迁移。
	for _, palette := range []string{"", "zed", "stone", "blue"} {
		t.Run(palette, func(t *testing.T) {
			d := testDesktop()
			d.preferencesPath = filepath.Join(t.TempDir(), "preferences.json")
			if palette != "" {
				if err := os.WriteFile(d.preferencesPath, []byte(`{"palette":"`+palette+`"}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := d.loadPreferences(); err != nil {
				t.Fatal(err)
			}
			want := palette
			if want == "" {
				want = "zed"
			}
			if d.preferences.Palette != want {
				t.Fatalf("palette=%q want=%q", d.preferences.Palette, want)
			}
		})
	}
}

// TestWorkspacePreferences 检查工作台布局默认值、局部覆盖及旧字段被丢弃。
func TestWorkspacePreferences(t *testing.T) {
	// 1. 通过真实偏好文件读取，不迁移旧的 sidebar/details 字段。
	for _, tt := range []struct {
		name, content string
		sidebar       bool
		right         string
		bottomOpen    bool
	}{
		{"default", ``, true, "agent", false},
		{"old fields dropped", `{"sidebar":false,"details":false,"details_tab":2}`, true, "agent", false},
		{"saved layout", `{"workspace":{"sidebar_open":false,"right":{"open":true,"active_panel":"debug"},"bottom":{"open":true}}}`, false, "debug", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			d.preferencesPath = filepath.Join(t.TempDir(), "preferences.json")
			if tt.content != "" {
				if err := os.WriteFile(d.preferencesPath, []byte(tt.content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := d.loadPreferences(); err != nil {
				t.Fatal(err)
			}
			workspace := d.preferences.Workspace
			if workspace.SidebarOpen != tt.sidebar || workspace.Right.ActivePanel != tt.right || workspace.Bottom.Open != tt.bottomOpen {
				t.Fatalf("workspace = %+v", workspace)
			}
		})
	}
}
