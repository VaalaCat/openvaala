package world

import (
	"bytes"
	"cmp"
	"context"
	"crypto/ecdh"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/storage"
	"github.com/VaalaCat/openvaala/pkg/worldserver"
)

func readWorldMessage(ctx context.Context, channel SecureChannel) (wireMessage, error) {
	var message wireMessage
	payload, err := channel.ReadMessage(ctx)
	if err != nil {
		return message, err
	}
	err = json.Unmarshal(payload, &message)
	return message, err
}

func exchangeChannelKey(private *ecdh.PrivateKey, peer []byte) ([]byte, error) {
	// 1. 解析对端 X25519 公钥并计算共享密钥。
	public, err := ecdh.X25519().NewPublicKey(peer)
	if err != nil {
		return nil, err
	}
	return private.ECDH(public)
}

func channelNonce(sequence uint64) [12]byte {
	// 1. 在 4 字节零前缀后写入 8 字节小端序号。
	var nonce [12]byte
	binary.LittleEndian.PutUint64(nonce[4:], sequence)
	return nonce
}

// GetWorldSocketURL 补齐控制面或直连 WebSocket 路径。
func GetWorldSocketURL(base, path string) (string, error) {
	u, err := worldserver.ParseWorldBaseURL(base, true)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	}
	if !strings.Contains(u.Host, "%") {
		u.Host = strings.ToLower(u.Host)
	}
	if u.Scheme == "ws" && u.Port() == "80" || u.Scheme == "wss" && u.Port() == "443" {
		u.Host = strings.TrimSuffix(u.Host, ":"+u.Port())
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(u.Path, path) {
		u.Path += path
	}
	return u.String(), nil
}

// relayInstancesURL 保留入口的部署前缀，使用对应的 HTTP 协议取实例列表。
func relayInstancesURL(base string) (string, error) {
	u, err := worldserver.ParseWorldBaseURL(base, true)
	if err != nil {
		return "", err
	}
	u.Scheme = strings.Replace(u.Scheme, "ws", "http", 1)
	return strings.TrimRight(u.String(), "/") + "/world/v1/relay/instances", nil
}

// selectRelayInstances 用实例地址原文和挂靠者签名公钥计算 HRW，按得分降序取前两项。
func selectRelayInstances(instances []string, key []byte) []string {
	type rankedInstance struct {
		base  string
		score [sha256.Size]byte
	}
	ranked := make([]rankedInstance, 0, len(instances))
	for _, base := range instances {
		input := append([]byte("openvaala-relay-hrw/v1\x00"+base+"\x00"), key...)
		ranked = append(ranked, rankedInstance{base, sha256.Sum256(input)})
	}
	slices.SortFunc(ranked, func(a, b rankedInstance) int {
		if order := bytes.Compare(b.score[:], a.score[:]); order != 0 {
			return order
		}
		return cmp.Compare(a.base, b.base)
	})
	selected := make([]string, min(len(ranked), relayInstanceCount))
	for i := range selected {
		selected[i] = ranked[i].base
	}
	return selected
}

// isRelaySelectionAttached 选中的实例是否都已确认挂靠？
func isRelaySelectionAttached(selected []string, attachments map[string]*relayAttachment) bool {
	for _, instance := range selected {
		if a := attachments[instance]; a == nil || !a.attached {
			return false
		}
	}
	return true
}

func messageText(item *defs.Item) string {
	// 1. 按原次序连接文本内容。
	if item == nil || item.Message == nil {
		return ""
	}
	var b strings.Builder
	for _, part := range item.Message.Content {
		if part != nil && part.Kind == defs.ContentText && part.Text != nil {
			b.WriteString(part.Text.Text)
		}
	}
	return b.String()
}

func isTargetAdvertised(targets []worldserver.AdvertisedTarget, name string) bool {
	// 1. 是否通告了同名目标？
	return slices.ContainsFunc(targets, func(t worldserver.AdvertisedTarget) bool { return t.Name == name })
}

type worldError struct{ code, text string }

func (e *worldError) Error() string { return e.text }

// ErrorCode 返回 World 请求的错误码。
func ErrorCode(err error) string {
	// 1. 提取调用方可以判断的错误码。
	var e *worldError
	if errors.As(err, &e) {
		return e.code
	}
	if errors.Is(err, ErrInvalidWorldAddress) {
		return "invalid_world_address"
	}
	return "world_request_failed"
}
func isTerminalOutboundStatus(status WorldMessageStatus) bool {
	// 1. 发件是否已经结束？
	return status == WorldMessageStatusReplied || status == WorldMessageStatusFailed || status == WorldMessageStatusCancelled
}

func worldInput(conversationID defs.ConversationID, messageID, body string) *runtime.SubmitInputRequest {
	// 1. 消息标识同时作为本次对话输入的去重标识。
	return &runtime.SubmitInputRequest{
		ConversationID: conversationID,
		InputID:        defs.InputID(messageID),
		Mode:           session.SessionInputModeNormal,
		From:           &defs.Address{Kind: defs.IdentityUser, ID: "user"},
		Content:        []*defs.Content{{Kind: defs.ContentText, Text: &defs.TextContent{Text: body}}},
	}
}

var errEmptyClientCursorID = &worldError{"invalid_arguments", "cursor_id must be a single nonempty value"}

func clientPage(limitParam *int, id *string) (int, storage.RecordCursor, error) {
	limit := 50
	var cursor storage.RecordCursor
	if limitParam != nil {
		limit = *limitParam
		if limit < 1 || limit > 200 {
			return 0, cursor, &worldError{"invalid_arguments", "limit must be an integer between 1 and 200"}
		}
	}
	if id != nil {
		if *id == "" {
			return 0, cursor, errEmptyClientCursorID
		}
		cursor.Key = storage.RecordKey(*id)
	}
	return limit, cursor, nil
}

func outboundEnvelope(from, to Address, message WorldMessage) wireMessage {
	return wireMessage{
		Kind: message.Kind, MessageID: message.MessageID, ThreadID: message.ThreadID,
		From: from, To: to, Subject: message.Subject, Body: message.Body,
		InReplyTo: message.InReplyTo, HopLimit: message.HopLimit,
	}
}

func domainNotTrusted(domain string) error {
	return &worldError{"world_domain_not_trusted", fmt.Sprintf("domain %q is not trusted", domain)}
}
func identityUnverified(addr string) error {
	return &worldError{"world_identity_unverified", fmt.Sprintf("cannot verify %q: its domain control plane is unreachable", addr)}
}
func senderNotAllowed(addr string) error {
	return &worldError{"world_sender_not_allowed", fmt.Sprintf("sender %q is not allowed to message this node", addr)}
}

func domainNodeIdentity(local worldserver.DomainIdentity) NodeIdentity {
	return NodeIdentity{
		NodeID:     worldserver.GetNodeIDFromSignPublic(local.SignPublic),
		SignPublic: local.SignPublic, SignPrivate: local.SignPrivate,
		BoxPublic: local.BoxPublic, BoxPrivate: local.BoxPrivate,
	}
}

// GetJoinNodeName 未指定节点名时使用本机短主机名。
func GetJoinNodeName(name string) (string, error) {
	if name != "" {
		return name, nil
	}
	host, err := os.Hostname()
	short, _, _ := strings.Cut(host, ".")
	return strings.ToLower(short), err
}
