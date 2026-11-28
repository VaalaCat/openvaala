// Package sessiontoken 签发和验证绑定到一个会话的令牌。
package sessiontoken

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"sync/atomic"

	"github.com/VaalaCat/openvaala/pkg/defs"
)

// Secret 保存当前操作者令牌，运行中的会话签发时读取同一份密钥。
type Secret struct{ value atomic.Pointer[string] }

// NewSecret 用当前操作者令牌初始化会话签发与验证共用的密钥。
func NewSecret(token string) *Secret {
	secret := &Secret{}
	secret.Replace(token)
	return secret
}

// Replace 更新密钥，后续签发和验证使用新令牌。
func (s *Secret) Replace(token string) { s.value.Store(&token) }

// GetToken 返回当前操作者令牌；未配置密钥时返回空串。
func (s *Secret) GetToken() string {
	if s == nil {
		return ""
	}
	if token := s.value.Load(); token != nil {
		return *token
	}
	return ""
}

// Issue 用 serve 的令牌签发会话 Token。
func Issue(secret string, conversationID defs.ConversationID, sessionID defs.SessionID) string {
	// 1. 检查 ID 可用作签名和令牌的独立字段。
	conversation, session := string(conversationID), string(sessionID)
	if secret == "" || conversation == "" || session == "" ||
		strings.ContainsAny(conversation, ".|") || strings.ContainsAny(session, ".|") {
		return ""
	}
	// 2. 签名绑定的两个 ID，返回点分隔的令牌。
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(conversation + "|" + session))
	return conversation + "." + session + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Verify 验证会话 Token 并返回它绑定的两个 ID。
func Verify(secret, token string) (defs.ConversationID, defs.SessionID, bool) {
	// 1. 解析 ID 段并比较重新签发的令牌。
	conversation, rest, _ := strings.Cut(token, ".")
	session, _, found := strings.Cut(rest, ".")
	conversationID, sessionID := defs.ConversationID(conversation), defs.SessionID(session)
	if !found || !hmac.Equal([]byte(token), []byte(Issue(secret, conversationID, sessionID))) {
		return "", "", false
	}
	return conversationID, sessionID, true
}
