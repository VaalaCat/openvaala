package desktop

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/VaalaCat/mygo-kit"
)

// remoteServer 是原生桌面本地保存的一台远端 Runtime Server 档案。
type remoteServer struct {
	ID    string `json:"id"`
	Name  string `json:"name,omitempty"`
	URL   string `json:"url"`
	Token string `json:"token,omitempty"`
}

type preferences struct {
	Language  string `json:"language"`
	Theme     string `json:"theme"`
	Palette   string `json:"palette"`
	BodyFont  string `json:"body_font"`
	TitleFont string `json:"title_font"`
	// NotifyOnFinish 为真时，窗口不在前台的对话完成后发系统通知。
	NotifyOnFinish bool               `json:"notify_on_finish"`
	Width          int                `json:"width"`
	Height         int                `json:"height"`
	Workspace      kit.WorkspaceState `json:"workspace"`
	// ActiveServer 为空或 "local" 表示连接本机 serve，否则为 RemoteServers 中的服务器 ID。
	ActiveServer  string         `json:"active_server,omitempty"`
	RemoteServers []remoteServer `json:"remote_servers,omitempty"`
}

// 右 Dock 与下 Dock 的面板 ID。
const (
	panelAgent    = "agent"
	panelContext  = "context"
	panelTurns    = "turns"
	panelRPC      = "rpc"
	panelDebug    = "debug"
	panelTerminal = "terminal"
)

// 两个 Dock 各自的面板 ID，按状态栏按钮顺序排列。
var (
	rightPanelIDs  = []string{panelAgent, panelContext, panelTurns, panelRPC, panelDebug}
	bottomPanelIDs = []string{panelTerminal}
)

func defaultPreferences() preferences {
	return preferences{Language: "zh-CN", Theme: "system", Palette: "zed", NotifyOnFinish: true, Width: 1280, Height: 820, Workspace: kit.WorkspaceState{SidebarOpen: true, Right: kit.DockState{Open: true, ActivePanel: panelAgent}, Bottom: kit.DockState{ActivePanel: panelTerminal}}}
}

// isValidWorkspace 已保存的工作台布局是否只引用现有面板且尺寸不为负？
func isValidWorkspace(state kit.WorkspaceState) bool {
	return state.SidebarWidth >= 0 && state.Right.Size >= 0 && state.Bottom.Size >= 0 &&
		slices.Contains(rightPanelIDs, state.Right.ActivePanel) &&
		(state.Bottom.ActivePanel == "" || slices.Contains(bottomPanelIDs, state.Bottom.ActivePanel))
}

// loadPreferences 读取本机偏好；无文件时使用默认值。
func (d *Desktop) loadPreferences() error {
	// 1. 读取完整文件，损坏时保留默认值。
	if d.preferencesPath == "" {
		return nil
	}
	data, err := os.ReadFile(d.preferencesPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		d.preferencesBroken = true
		return err
	}
	p := defaultPreferences()
	if err = json.Unmarshal(data, &p); err != nil {
		d.preferencesBroken = true
		return err
	}
	if p.Width < 640 || p.Height < 480 || p.Width > 16384 || p.Height > 16384 || !isValidWorkspace(p.Workspace) || (p.Theme != "system" && p.Theme != "light" && p.Theme != "dark") || (p.Palette != "zed" && p.Palette != "stone" && p.Palette != "blue") || (p.Language != "zh-CN" && p.Language != "en") {
		d.preferencesBroken = true
		return fmt.Errorf("invalid desktop preferences; original file preserved")
	}
	// 旧版本保存的下 Dock 没有当前面板；补成唯一的终端面板，⌘J 打开时才会触发自动新开终端。
	if p.Workspace.Bottom.ActivePanel == "" {
		p.Workspace.Bottom.ActivePanel = panelTerminal
	}
	// 2. 远端服务器列表按弱依赖过滤：跳过损坏或重名条目，不因此拒绝整份偏好。
	if len(p.RemoteServers) > 0 {
		validServers := make([]remoteServer, 0, len(p.RemoteServers))
		seenIDs := make(map[string]bool, len(p.RemoteServers))
		for _, server := range p.RemoteServers {
			server.ID = strings.TrimSpace(server.ID)
			server.Name = strings.TrimSpace(server.Name)
			if server.ID == "" || server.ID == localServerID || seenIDs[server.ID] {
				continue
			}
			parsedURL, parsedToken, _, _, parseErr := parseRemoteServerInput(server.URL, server.Token)
			if parseErr != nil {
				continue
			}
			seenIDs[server.ID] = true
			validServers = append(validServers, remoteServer{
				ID:    server.ID,
				Name:  server.Name,
				URL:   parsedURL,
				Token: parsedToken,
			})
		}
		p.RemoteServers = validServers
	}
	p.ActiveServer = strings.TrimSpace(p.ActiveServer)
	if p.ActiveServer != "" && p.ActiveServer != localServerID && !slices.ContainsFunc(p.RemoteServers, func(s remoteServer) bool { return s.ID == p.ActiveServer }) {
		p.ActiveServer = ""
	}
	d.preferences = p
	return nil
}

// savePreferences 原子替换本机偏好文件。
func (d *Desktop) savePreferences() error {
	// 1. 编码本次内存偏好并写临时文件。
	if d.preferencesPath == "" {
		return nil
	}
	if d.preferencesBroken {
		return fmt.Errorf("desktop preferences file could not be loaded; original file preserved")
	}
	// 2. 设置页临时收起的右 Dock、临时显示的侧栏不算用户的选择，按原来的值保存。
	saved := d.preferences
	if d.rightDockHiddenBySettings {
		saved.Workspace.Right.Open = true
	}
	if d.sidebarShownBySettings {
		saved.Workspace.SidebarOpen = false
	}
	data, err := json.MarshalIndent(saved, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(d.preferencesPath)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".preferences-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	// 2. 将已写完的文件替换为偏好文件。
	return os.Rename(f.Name(), d.preferencesPath)
}
