package cli

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

// webUIPath 是 serve 提供页面的路径。
const webUIPath = "/web/v1/"

// newWebCommand 创建 web 命令：打印 Web 界面地址并尝试用浏览器打开。
func newWebCommand() *cobra.Command {
	// 1. 注册 web 命令和 --url 参数。
	command := &cobra.Command{
		Use:   "web",
		Short: "Open the Web UI of the runtime server",
		Args:  cobra.NoArgs,
		RunE:  BuildCLIHandler(runWebCommand),
	}
	addURLFlag(command.Flags())
	return command
}

// runWebCommand 选择 serve，打印页面地址并尝试打开浏览器。
func runWebCommand(ctx context.Context, args *AppArgs) error {
	// 1. 选择 serve：本机没有 serve 时拉起常驻的（不带 --idle-timeout）；本机 serve 由另一个可执行文件启动时向 stderr 写提示。
	connection, err := getRuntimeConnection(ctx, args, 0)
	if err != nil {
		return err
	}
	writeRuntimeServerNotice(ctx, args.Stderr, connection)
	// 2. 生成页面地址，第一行输出主地址，其余每行输出一个 Network: 地址。
	addresses, err := getWebUIAddresses(connection, args.Config.Server.Listen, getUpInterfaceAddrs())
	if err != nil {
		return err
	}
	lines := []string{addresses[0]}
	for _, address := range addresses[1:] {
		lines = append(lines, "Network: "+address)
	}
	if err := writeText(args.Stdout, strings.Join(lines, "\n")+"\n"); err != nil {
		return err
	}
	// 3. 尝试用浏览器打开主地址。
	openBrowser(addresses[0])
	return nil
}

// GetWebUIURL 将服务连接地址转成 WebUI 地址，并把登录令牌放在 fragment 中。
func GetWebUIURL(runtimeURL, token string) (string, error) {
	// 1. 解析连接地址；未提供令牌时读取地址里的 token 参数。
	connectionURL, err := url.Parse(runtimeURL)
	if err != nil {
		return "", fmt.Errorf("parse runtime server url: %w", err)
	}
	if token == "" {
		token = connectionURL.Query().Get("token")
	}
	// 2. 清除原 fragment，生成 HTTP 页面地址及转义后的登录令牌。
	connectionURL.Fragment, connectionURL.RawFragment = "", ""
	pageURL, err := getRuntimeServerHTTPURL(connectionURL.String(), webUIPath)
	if err != nil {
		return "", err
	}
	return pageURL + "#token=" + url.QueryEscape(token), nil
}

// getWebUIAddresses 生成页面地址：第一项是连接地址对应的主地址，其余是本机 serve 监听全部网卡时每个非回环 IPv4 地址对应的局域网地址。
func getWebUIAddresses(connection runtimeConnection, listen string, interfaceAddrs []net.Addr) ([]string, error) {
	// 1. 生成连接地址对应的主地址。
	mainAddress, err := GetWebUIURL(connection.URL, connection.Token)
	if err != nil {
		return nil, err
	}
	addresses := []string{mainAddress}
	// 2. 只有本机 serve 且它监听全部网卡（host 为空、0.0.0.0 或 ::）时才列出局域网地址。
	if connection.Source != runtimeConnectionSourceLocal {
		return addresses, nil
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, fmt.Errorf("parse server.listen %q: %w", listen, err)
	}
	listenIP := net.ParseIP(host)
	isAllInterfaces := host == "" || (listenIP != nil && listenIP.IsUnspecified())
	if !isAllInterfaces {
		return addresses, nil
	}
	// 3. 每个非回环 IPv4 地址换掉主地址的 host，端口沿用连接地址的端口。
	pageURL, err := url.Parse(mainAddress)
	if err != nil {
		return nil, err
	}
	for _, addr := range interfaceAddrs {
		var ip net.IP
		switch addr := addr.(type) {
		case *net.IPNet:
			ip = addr.IP
		case *net.IPAddr:
			ip = addr.IP
		}
		if ip.To4() == nil || ip.IsLoopback() {
			continue
		}
		pageURL.Host = net.JoinHostPort(ip.String(), pageURL.Port())
		addresses = append(addresses, pageURL.String())
	}
	return addresses, nil
}

// getUpInterfaceAddrs 返回 up 状态网卡的地址，读取失败的网卡直接跳过。
func getUpInterfaceAddrs() []net.Addr {
	// 1. 列出网卡，收集 up 状态网卡的地址。
	interfaces, _ := net.Interfaces()
	var addrs []net.Addr
	for _, networkInterface := range interfaces {
		if networkInterface.Flags&net.FlagUp == 0 {
			continue
		}
		interfaceAddrs, _ := networkInterface.Addrs()
		addrs = append(addrs, interfaceAddrs...)
	}
	return addrs
}

// openBrowser 尽力用系统默认浏览器打开 pageURL：macOS 用 open，其他系统用 xdg-open；启动后不等待，所有错误都忽略。
func openBrowser(pageURL string) {
	// 1. 启动打开命令，标准流不设置，即 /dev/null。
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	_ = exec.Command(name, pageURL).Start()
}
