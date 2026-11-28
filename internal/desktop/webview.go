package desktop

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/VaalaCat/openvaala/internal/cli"
	"github.com/egoist/mygo"
	"github.com/sourcegraph/conc"
)

// WebViewDesktop 使用系统 WebView 显示本机 serve 的 WebUI。
type WebViewDesktop struct {
	opt       WebViewDesktopOpt
	window    *mygo.Window
	loadError error
}

// WebViewDesktopOpt 指定 WebUI 使用的本机服务启动参数与可选远端目标。
type WebViewDesktopOpt struct {
	RuntimeServerStart cli.RuntimeServerStartOpt
	StartTarget        cli.DesktopStartTarget
}

// NewWebViewDesktop 校验并保存 WebView 桌面的启动参数。
func NewWebViewDesktop(opt *WebViewDesktopOpt) (*WebViewDesktop, error) {
	// 1. 检查并保存本机服务启动配置；指定远端目标时不要求本机可执行文件。
	if opt == nil || (opt.RuntimeServerStart.Executable == "" && strings.TrimSpace(opt.StartTarget.URL) == "") {
		return nil, errors.New("WebView desktop requires a runtime executable")
	}
	return &WebViewDesktop{opt: *opt}, nil
}

// Run 连接目标服务，在主 goroutine 运行 WebView 窗口并等待本进程任务退出。
func (d *WebViewDesktop) Run(ctx context.Context) error {
	// 1. 取得 WebView 模式的单实例身份，检查系统浏览器组件。
	mygo.App.SetName("OpenVaala Web")
	if !mygo.App.RequestSingleInstanceLock() {
		return nil
	}
	if !mygo.WebViewAvailable() {
		return errors.New("system WebView is unavailable; on Linux install WebKitGTK (libwebkit2gtk-4.1-0), or use `openvaala app --native`")
	}
	// 2. 指定了 --url / client.url 时直接加载远端 WebUI，否则复用或启动本机 serve。
	var runtimeURL, token string
	isRemote := strings.TrimSpace(d.opt.StartTarget.URL) != ""
	if isRemote {
		input, err := parseRemoteServerInput(d.opt.StartTarget.URL, d.opt.StartTarget.Token)
		if err != nil {
			return err
		}
		runtimeURL, token = input.URL, input.Token
		if input.WorldTarget != nil && input.ExtractedToken {
			runtimeURL = d.opt.StartTarget.URL
		}
	} else {
		connection, err := cli.EnsureRuntimeServer(ctx, &d.opt.RuntimeServerStart)
		if err != nil {
			return err
		}
		runtimeURL, token = connection.URL, connection.Token
	}
	address, err := cli.GetWebUIURL(runtimeURL, token)
	if err != nil {
		return err
	}
	pageURL, err := url.Parse(address)
	if err != nil {
		return err
	}
	requestURL := *pageURL
	requestURL.Fragment, requestURL.RawFragment = "", ""
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return err
	}
	httpClient := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := httpClient.Do(request)
	if err != nil {
		if isRemote {
			return fmt.Errorf("load remote WebUI: %w", err)
		}
		return fmt.Errorf("load local WebUI: %w", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/html") {
		if isRemote {
			return fmt.Errorf("the remote runtime server does not provide the WebUI (HTTP %d)", response.StatusCode)
		}
		return fmt.Errorf("the local runtime server does not provide the WebUI (HTTP %d); build with script/build-desktop.sh, then run `openvaala serve stop` before reopening the desktop", response.StatusCode)
	}
	// 3. 就绪后建立窗口和菜单，仅允许本机页面在窗口内导航。
	mygo.App.WhenReady(func() {
		d.window = mygo.NewWindow(mygo.WindowOptions{
			Title:     "OpenVaala",
			Width:     1280,
			Height:    800,
			MinWidth:  640,
			MinHeight: 480,
			StateKey:  "webui",
			Page:      mygo.PageOptions{Untrusted: true},
		})
		d.window.OnClosed(mygo.App.Quit)
		mygo.App.SetMenu(mygo.NewMenu([]*mygo.MenuItem{
			{Role: mygo.RoleAppMenu},
			{Role: mygo.RoleFileMenu},
			{Role: mygo.RoleEditMenu},
		}))
		page := d.window.Page()
		page.OnWillNavigate(func(event *mygo.NavigateEvent) {
			targetURL, err := url.Parse(event.URL)
			if err == nil && targetURL.Scheme == pageURL.Scheme && targetURL.Host == pageURL.Host {
				return
			}
			event.PreventDefault()
			if event.UserInitiated && err == nil && (targetURL.Scheme == "http" || targetURL.Scheme == "https") {
				_ = mygo.Shell.OpenExternal(event.URL)
			}
		})
		page.SetWindowOpenHandler(func(request mygo.WindowOpenRequest) *mygo.WindowOptions {
			targetURL, err := url.Parse(request.URL)
			if request.UserInitiated && err == nil && (targetURL.Scheme == "http" || targetURL.Scheme == "https") {
				_ = mygo.Shell.OpenExternal(request.URL)
			}
			return nil
		})
		page.OnDidFailLoad(func(loadError *mygo.LoadError) {
			d.loadError = fmt.Errorf("WebView failed to load the WebUI (code %d)", loadError.Code)
			mygo.App.Quit()
		})
		page.OnRenderProcessGone(func(reason string) {
			d.loadError = fmt.Errorf("WebView renderer stopped: %s", reason)
			mygo.App.Quit()
		})
		if err := page.LoadURL(address); err != nil {
			d.loadError = fmt.Errorf("open WebUI: %w", err)
			mygo.App.Quit()
		}
	})
	mygo.App.OnSecondInstance(func(_ []string, _ string) {
		if d.window != nil {
			d.window.Restore()
			d.window.Show()
			d.window.Focus()
		}
	})
	// 4. 响应调用方取消；窗口退出后等待本进程任务，保留共用 serve。
	var tasks conc.WaitGroup
	finished := make(chan struct{})
	tasks.Go(func() {
		select {
		case <-ctx.Done():
			mygo.App.Quit()
		case <-finished:
		}
	})
	err = mygo.App.Run()
	close(finished)
	tasks.Wait()
	return errors.Join(err, d.loadError)
}
