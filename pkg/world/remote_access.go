package world

import (
	"cmp"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/VaalaCat/openvaala/pkg/worldserver"
)

// RemoteAccessTarget 是远程访问链接中公布的节点身份和拨号入口。
type RemoteAccessTarget struct {
	Name      string                  `json:"name,omitempty"`
	Token     string                  `json:"-"`
	Node      string                  `json:"node,omitempty"`
	Domain    string                  `json:"domain,omitempty"`
	PublicKey ed25519.PublicKey       `json:"public_key"`
	Entries   worldserver.PeerEntries `json:"entries"`
}

func (t RemoteAccessTarget) validate() error {
	if (t.Node != "" && !nodeOrTargetPattern.MatchString(t.Node)) || (t.Domain != "" && !worldserver.IsValidDomain(t.Domain)) || len(t.PublicKey) != ed25519.PublicKeySize || !t.Entries.IsValid() {
		return errors.New("invalid world remote access target")
	}
	return nil
}

// ConnectionTimeout 包含直连、各 relay 和最后的认证初始化预算。
func (t RemoteAccessTarget) ConnectionTimeout() time.Duration {
	return 2*time.Second + time.Duration(len(t.Entries.Relays))*(relayInstancesTimeout+relayInstanceCount*relayDialTimeout) + 10*time.Second
}

// ParseWebOrigin 读取 World 远程访问页面的 HTTP 或 HTTPS 来源。
func ParseWebOrigin(raw string) (*url.URL, error) {
	page, err := worldserver.ParseWorldBaseURL(raw, false)
	if err != nil {
		return nil, err
	}
	if path := page.EscapedPath(); path != "" && path != "/" {
		return nil, errors.New("world web_url must be an origin")
	}
	return page, nil
}

// GetDisplayName 返回链接携带的名称或目标公钥的短指纹。
func (t RemoteAccessTarget) GetDisplayName() string {
	if t.Name != "" {
		return t.Name
	}
	if t.Node != "" && t.Domain != "" {
		return t.Node + "@" + t.Domain
	}
	return base64.RawURLEncoding.EncodeToString(t.PublicKey)[:8]
}

// IsLocalPageURL 是否只能通过本机或内网地址打开页面？
func IsLocalPageURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
	}
	return !strings.Contains(host, ".") || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local")
}

// SelectRemoteAccessPage 优先使用指定来源，然后尝试外网 HTTPS relay、HTTPS 直连和其余可用地址。
func SelectRemoteAccessPage(webURL string, relays, direct []string) string {
	if webURL != "" {
		return webURL
	}
	var fallback, secureDirect string
	for group, candidates := range [][]string{relays, direct} {
		for _, raw := range candidates {
			u, err := worldserver.ParseWorldBaseURL(raw, true)
			if err != nil {
				continue
			}
			u.Scheme = strings.Replace(u.Scheme, "ws", "http", 1)
			u.Path, u.RawPath = "", ""
			origin := u.String()
			if u.Scheme == "https" && !IsLocalPageURL(origin) {
				return origin
			}
			if group == 1 && u.Scheme == "https" && secureDirect == "" {
				secureDirect = origin
			}
			if fallback == "" {
				fallback = origin
			}
		}
	}
	return cmp.Or(secureDirect, fallback)
}

// FormatRemoteAccessURL 将目标和登录令牌放入页面 fragment。
func FormatRemoteAccessURL(webURL string, target RemoteAccessTarget) (string, error) {
	if err := target.validate(); err != nil {
		return "", err
	}
	page, err := ParseWebOrigin(SelectRemoteAccessPage(webURL, target.Entries.Relays, target.Entries.DirectURLs))
	if err != nil {
		return "", err
	}
	page.Path = "/web/v1/"
	payload, err := json.Marshal(target)
	if err != nil {
		return "", err
	}
	link := page.String() + "#world=" + base64.RawURLEncoding.EncodeToString(payload)
	if target.Token != "" {
		link += "&token=" + url.QueryEscape(target.Token)
	}
	return link, nil
}

// ParseRemoteAccessURL 读取 fragment 的目标和可选登录令牌。
func ParseRemoteAccessURL(rawURL string) (RemoteAccessTarget, error) {
	var target RemoteAccessTarget
	_, fragment, ok := strings.Cut(rawURL, "#")
	if !ok || !strings.HasPrefix(fragment, "world=") {
		return target, errors.New("missing world remote access target")
	}
	values, err := url.ParseQuery(fragment)
	if err != nil {
		return target, errors.New("invalid world remote access fragment")
	}
	data, err := base64.RawURLEncoding.DecodeString(values.Get("world"))
	if err != nil || json.Unmarshal(data, &target) != nil {
		return target, errors.New("invalid world remote access payload")
	}
	target.Token = values.Get("token")
	return target, target.validate()
}
