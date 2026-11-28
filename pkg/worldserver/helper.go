package worldserver

import (
	"cmp"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/gin-gonic/gin"
)

var (
	nodeNamePattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	domainLabelPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// RegisterSignBytes 构造节点注册签名的字节，绑定节点名、入网码及两把公钥。
func RegisterSignBytes(request RegisterNodeRequest) []byte {
	// 1. 按协议顺序编码带长度的字段。
	return signBytes("openvaala-world-register/v1", []byte(request.NodeName),
		[]byte(request.JoinToken), request.SignPublic, request.BoxPublic)
}

// ChallengePurpose 标识节点提交挑战应答的入口。
type ChallengePurpose string

const (
	ChallengeConnect     ChallengePurpose = "openvaala-world-connect/v1"
	ChallengeRelayAttach ChallengePurpose = "openvaala-world-relay-attach/v1"
)

// ChallengeSignBytes 将入口、挑战、节点标识和 Unix 秒时间戳绑定到证明。
func ChallengeSignBytes(purpose ChallengePurpose, challenge []byte, nodeID string, timestamp int64) []byte {
	// 1. 按协议顺序编码挑战、节点标识和时间戳。
	return signBytes(string(purpose), challenge, []byte(nodeID),
		binary.BigEndian.AppendUint64(nil, uint64(timestamp)))
}

// signBytes 按顺序用 8 字节大端长度定界各字段。
func signBytes(protocol string, fields ...[]byte) []byte {
	// 1. 写入协议标签和每个字段的长度及原值。
	data := append([]byte(protocol), 0)
	for _, field := range fields {
		data = binary.BigEndian.AppendUint64(data, uint64(len(field)))
		data = append(data, field...)
	}
	return data
}

// GetNodeIDFromSignPublic 从签名公钥摘要的前 80 位派生节点标识。
func GetNodeIDFromSignPublic(public []byte) string {
	// 1. 将摘要前 10 字节编码为小写 Base32。
	digest := sha256.Sum256(public)
	return "nd_" + strings.ToLower(base32.StdEncoding.EncodeToString(digest[:10]))
}

// ParseJoinCode 读取入网码中的控制面地址。
func ParseJoinCode(code string) (string, error) {
	base, secret, found := strings.Cut(code, "#")
	if _, err := ParseWorldBaseURL(base, false); err != nil || !found || secret == "" || strings.ContainsAny(secret, "# \t\r\n") {
		return "", errors.New("invalid World join code")
	}
	return base, nil
}

// joinTokenHash 只校验入网码的密钥部分，允许成员修改控制面地址。
func joinTokenHash(token string) string {
	_, secret, _ := strings.Cut(token, "#")
	hash := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(hash[:])
}

// IsValidDomain 是否为小写主机名或带可选端口的主机地址？
func IsValidDomain(domain string) bool {
	// 1. 解析方括号形式的 IPv6，或校验普通主机名后的端口。
	if strings.HasPrefix(domain, "[") {
		if strings.HasSuffix(domain, "]") {
			ip, err := netip.ParseAddr(domain[1 : len(domain)-1])
			return err == nil && ip.Is6() && ip.Zone() == ""
		}
		addr, err := netip.ParseAddrPort(domain)
		return err == nil && addr.Addr().Is6() && addr.Addr().Zone() == "" && addr.Port() != 0
	}
	host, port, hasPort := strings.Cut(domain, ":")
	if hasPort {
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil || n == 0 {
			return false
		}
	}

	// 2. 校验完整主机名与各级标签。
	if len(host) > 253 {
		return false
	}
	for label := range strings.SplitSeq(host, ".") {
		if !domainLabelPattern.MatchString(label) {
			return false
		}
	}
	return true
}

// controlEnvelope 编码控制面消息的信封。
func controlEnvelope(messageType string, payload any) (ControlEnvelope, error) {
	// 1. 编码有载荷的消息，心跳帧只包含类型。
	envelope := ControlEnvelope{Type: messageType}
	if payload == nil {
		return envelope, nil
	}
	data, err := json.Marshal(payload)
	envelope.Payload = data
	return envelope, err
}

// joinTokenInfo 返回管理员可查询的入网码状态。
func joinTokenInfo(record JoinTokenRecord) JoinTokenInfo {
	// 1. 提取归属、时间和使用状态。
	return JoinTokenInfo{
		ID:        record.ID,
		Owner:     record.Owner,
		Tags:      record.Tags,
		Reusable:  record.Reusable,
		UsedCount: record.UsedCount,
		ExpiresAt: record.ExpiresAt,
		RevokedAt: record.RevokedAt,
		CreatedAt: record.CreatedAt,
	}
}

// isValidAnnouncement 公告中的目标是否有合法的名称和可见性？
func isValidAnnouncement(announcement NodeAnnounce) bool {
	// 1. 校验每个目标的名称和可见范围。
	for _, target := range announcement.Targets {
		if !nodeNamePattern.MatchString(target.Name) ||
			(target.Visibility != "domain" && target.Visibility != "public") {
			return false
		}
	}
	return true
}

// cloneACL 复制规则及其选择器。
func cloneACL(rules []ACLRule) []ACLRule {
	// 1. 复制规则后逐条复制两侧选择器。
	cloned := slices.Clone(rules)
	for i := range cloned {
		cloned[i].From = slices.Clone(cloned[i].From)
		cloned[i].To = slices.Clone(cloned[i].To)
	}
	return cloned
}

// sortedACL 按优先级稳定排序规则。
func sortedACL(rules []ACLRule) []ACLRule {
	// 1. 对已提交的规则按优先级排序。
	slices.SortStableFunc(rules, func(a, b ACLRule) int { return cmp.Compare(a.Priority, b.Priority) })
	return rules
}

// writeHTTPError 返回控制面 HTTP 错误。
func writeHTTPError(c *gin.Context, status int, message string) {
	// 1. 使用统一的错误响应结构。
	c.JSON(status, gin.H{"error": message})
}

// writeStoreError 记录控制面存储错误并回复客户端。
func writeStoreError(c *gin.Context, err error) {
	// 1. 记录内部错误并返回通用错误响应。
	telemetry.GetLogger(c.Request.Context()).Warn("world control request failed", "error", err)
	writeHTTPError(c, http.StatusInternalServerError, "world control request failed")
}

func writeAdminResponse(c *gin.Context, status int, value any, err error) {
	var invalid *InvalidAdminRequestError
	switch {
	case errors.As(err, &invalid):
		c.JSON(http.StatusBadRequest, gin.H{"code": "invalid_arguments", "error": err.Error()})
	case errors.Is(err, ErrDomainKeyConflict):
		c.JSON(http.StatusConflict, gin.H{"code": "world_domain_name_taken", "error": err.Error()})
	case err != nil:
		writeStoreError(c, err)
	case status == http.StatusNoContent:
		c.Status(status)
	default:
		c.JSON(status, value)
	}
}

// ParseWorldBaseURL 解析网页或 WebSocket 入口的基址，保留部署前缀。
func ParseWorldBaseURL(base string, allowWebSocket bool) (*url.URL, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	validScheme := u.Scheme == "http" || u.Scheme == "https" || allowWebSocket && (u.Scheme == "ws" || u.Scheme == "wss")
	if !validScheme || u.Hostname() == "" || u.User != nil || strings.ContainsAny(base, "?#") {
		return nil, fmt.Errorf("invalid world base URL %q", base)
	}
	return u, nil
}

// IsValid 名片中是否至少有一项可用的拨号入口？
func (entries PeerEntries) IsValid() bool {
	if len(entries.DirectURLs)+len(entries.Relays) == 0 {
		return false
	}
	for _, base := range slices.Concat(entries.DirectURLs, entries.Relays) {
		if _, err := ParseWorldBaseURL(base, true); err != nil {
			return false
		}
	}
	return true
}

// GetAcceptURL 拼接本次接通通知对应的完整接受地址。
func (r RelayIncoming) GetAcceptURL(base string) string {
	return strings.TrimRight(base, "/") + "/world/v1/relay/accept?connection_id=" + url.QueryEscape(r.ConnectionID)
}
