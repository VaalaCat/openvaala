package cli

import (
	"net"
	"slices"
	"testing"
)

// TestGetWebUIAddresses 检查页面地址的主地址、令牌来源和局域网地址。
func TestGetWebUIAddresses(t *testing.T) {
	// 1. 列出本机回环、本机监听全部网卡、远端和令牌写在地址里的场景。
	interfaceAddrs := []net.Addr{
		newTestInterfaceAddr(t, "192.168.1.20/24"),
		newTestInterfaceAddr(t, "127.0.0.1/8"),
		newTestInterfaceAddr(t, "fe80::1/64"),
		&net.IPAddr{IP: net.ParseIP("10.0.0.7")},
	}
	tests := []struct {
		name       string
		connection runtimeConnection
		listen     string
		// interfaceAddrs 为 nil 时用上面的网卡地址。
		interfaceAddrs []net.Addr
		want           []string
		wantErr        bool
	}{
		{
			name: "local_loopback",
			connection: runtimeConnection{
				URL:    "ws://127.0.0.1:8790/api/v1/openvaala",
				Token:  "t",
				Source: runtimeConnectionSourceLocal,
			},
			listen: "127.0.0.1:8790",
			want:   []string{"http://127.0.0.1:8790/web/v1/#token=t"},
		},
		{
			name: "local_all_interfaces",
			connection: runtimeConnection{
				URL:    "ws://127.0.0.1:8790/api/v1/openvaala",
				Token:  "t",
				Source: runtimeConnectionSourceLocal,
			},
			listen: "0.0.0.0:8790",
			want: []string{
				"http://127.0.0.1:8790/web/v1/#token=t",
				"http://192.168.1.20:8790/web/v1/#token=t",
				"http://10.0.0.7:8790/web/v1/#token=t",
			},
		},
		{
			name: "local_empty_host_lists_interfaces",
			connection: runtimeConnection{
				URL:    "ws://127.0.0.1:8790/api/v1/openvaala",
				Token:  "t",
				Source: runtimeConnectionSourceLocal,
			},
			listen: ":8790",
			want: []string{
				"http://127.0.0.1:8790/web/v1/#token=t",
				"http://192.168.1.20:8790/web/v1/#token=t",
				"http://10.0.0.7:8790/web/v1/#token=t",
			},
		},
		{
			name: "local_ipv6_wildcard_lists_interfaces",
			connection: runtimeConnection{
				URL:    "ws://127.0.0.1:8790/api/v1/openvaala",
				Token:  "t",
				Source: runtimeConnectionSourceLocal,
			},
			listen: "[::]:8790",
			want: []string{
				"http://127.0.0.1:8790/web/v1/#token=t",
				"http://192.168.1.20:8790/web/v1/#token=t",
				"http://10.0.0.7:8790/web/v1/#token=t",
			},
		},
		{
			name: "local_all_interfaces_without_usable_interface",
			connection: runtimeConnection{
				URL:    "ws://127.0.0.1:8790/api/v1/openvaala",
				Token:  "t",
				Source: runtimeConnectionSourceLocal,
			},
			listen:         "0.0.0.0:8790",
			interfaceAddrs: []net.Addr{newTestInterfaceAddr(t, "127.0.0.1/8"), newTestInterfaceAddr(t, "fe80::1/64")},
			want:           []string{"http://127.0.0.1:8790/web/v1/#token=t"},
		},
		{
			name: "remote_wss",
			connection: runtimeConnection{
				URL:    "wss://ov.example.com/api/v1/openvaala",
				Token:  "t",
				Source: runtimeConnectionSourceConfig,
			},
			listen: "0.0.0.0:8790",
			want:   []string{"https://ov.example.com/web/v1/#token=t"},
		},
		{
			name: "flag_url_never_lists_interfaces",
			connection: runtimeConnection{
				URL:    "ws://10.0.0.5:8790/api/v1/openvaala",
				Token:  "t",
				Source: runtimeConnectionSourceFlag,
			},
			listen: "0.0.0.0:8790",
			want:   []string{"http://10.0.0.5:8790/web/v1/#token=t"},
		},
		{
			name: "token_in_url",
			connection: runtimeConnection{
				URL:    "ws://127.0.0.1:8790/api/v1/openvaala?token=abc",
				Source: runtimeConnectionSourceFlag,
			},
			listen: "127.0.0.1:8790",
			want:   []string{"http://127.0.0.1:8790/web/v1/#token=abc"},
		},
		{
			name: "connection_token_wins_over_url_token",
			connection: runtimeConnection{
				URL:    "ws://127.0.0.1:8790/api/v1/openvaala?token=abc",
				Token:  "t",
				Source: runtimeConnectionSourceFlag,
			},
			listen: "127.0.0.1:8790",
			want:   []string{"http://127.0.0.1:8790/web/v1/#token=t"},
		},
		{
			name: "token_is_escaped",
			connection: runtimeConnection{
				URL:    "ws://127.0.0.1:8790/api/v1/openvaala",
				Token:  "a b&c#d",
				Source: runtimeConnectionSourceLocal,
			},
			listen: "127.0.0.1:8790",
			want:   []string{"http://127.0.0.1:8790/web/v1/#token=a+b%26c%23d"},
		},
		{
			name: "ipv6_host",
			connection: runtimeConnection{
				URL:    "ws://[::1]:8790/api/v1/openvaala",
				Token:  "t",
				Source: runtimeConnectionSourceFlag,
			},
			listen: "127.0.0.1:8790",
			want:   []string{"http://[::1]:8790/web/v1/#token=t"},
		},
		{
			name: "original_fragment_is_replaced",
			connection: runtimeConnection{
				URL:    "ws://127.0.0.1:8790/api/v1/openvaala?token=a%2Bb#old-fragment",
				Source: runtimeConnectionSourceLocal,
			},
			listen: "0.0.0.0:8790",
			want: []string{
				"http://127.0.0.1:8790/web/v1/#token=a%2Bb",
				"http://192.168.1.20:8790/web/v1/#token=a%2Bb",
				"http://10.0.0.7:8790/web/v1/#token=a%2Bb",
			},
		},
		{
			name: "unsupported_scheme",
			connection: runtimeConnection{
				URL:    "http://127.0.0.1:8790/api/v1/openvaala",
				Token:  "t",
				Source: runtimeConnectionSourceFlag,
			},
			listen:  "127.0.0.1:8790",
			wantErr: true,
		},
	}
	// 2. 逐场景检查桌面使用的公开地址生成入口，以及浏览器使用的地址列表。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pageURL, err := GetWebUIURL(tt.connection.URL, tt.connection.Token)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("GetWebUIURL() = %q, want an error", pageURL)
				}
			} else if err != nil || pageURL != tt.want[0] {
				t.Fatalf("GetWebUIURL() = %q, %v, want %q", pageURL, err, tt.want[0])
			}
			addrs := interfaceAddrs
			if tt.interfaceAddrs != nil {
				addrs = tt.interfaceAddrs
			}
			got, err := getWebUIAddresses(tt.connection, tt.listen, addrs)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("getWebUIAddresses() = %v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("getWebUIAddresses() = %v, want %v", got, tt.want)
			}
		})
	}
}

// newTestInterfaceAddr 按 CIDR 写法创建网卡地址，和 net.InterfaceAddrs 一样保留主机 IP 和掩码。
func newTestInterfaceAddr(t *testing.T, cidr string) net.Addr {
	t.Helper()
	// 1. 解析 CIDR，IP 取主机地址，掩码取网段掩码。
	ip, network, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatal(err)
	}
	return &net.IPNet{IP: ip, Mask: network.Mask}
}
