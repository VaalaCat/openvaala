package worldserver

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrDomainKeyConflict = errors.New("domain is already bound to a different public key")
	ErrDomainNotTrusted  = errors.New("domain is not trusted")
)

// PeerNotFoundError 携带所属域查不到的完整成员地址。
type PeerNotFoundError string

func (e PeerNotFoundError) Error() string {
	return fmt.Sprintf("world peer %q is not in directory", string(e))
}

// GetDomains 返回已导入的名片及本域可以公开的入口。
func (s *worldServerImpl) GetDomains(ctx context.Context) (ListDomainsResult, error) {
	db, err := s.getAdminDB(ctx)
	if err != nil {
		return ListDomainsResult{}, err
	}
	domains, err := getPeerDomains(db)
	if err != nil {
		return ListDomainsResult{}, err
	}
	card, err := s.selfCard()
	return ListDomainsResult{Domain: s.opt.Domain, Domains: domains, SelfCard: card}, err
}

func (s *worldServerImpl) selfCard() (string, error) {
	entries := s.opt.Entries
	if len(entries.DirectURLs)+len(entries.Relays) == 0 {
		return "", nil
	}
	if s.relay != nil {
		entries.Relays = slices.Concat(entries.DirectURLs, entries.Relays)
	}
	card := PeerDomain{Domain: s.opt.Domain, PublicKey: s.identity.SignPublic, Entries: entries}
	payload, err := json.Marshal(card)
	return card.Domain + "#" + base64.RawURLEncoding.EncodeToString(payload), err
}

// AddDomain 导入经过校验的名片并通知本域成员更新通讯录。
func (s *worldServerImpl) AddDomain(ctx context.Context, card string) (PeerDomain, bool, error) {
	peer, previous, err := s.importCard(ctx, card, DomainPendingOutbound, nil)
	if err != nil {
		return peer, false, err
	}
	if peer.Status == DomainPendingOutbound || previous == DomainPendingInbound {
		s.sendTrust(ctx, peer)
	}
	return peer, previous == "", nil
}

// importCard 只用握手验签得到的公钥确认来信名片，管理员主动导入时使用其选定的名片。
func (s *worldServerImpl) importCard(ctx context.Context, card, status string, caller ed25519.PublicKey) (PeerDomain, string, error) {
	domain, encoded, _ := strings.Cut(card, "#")
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	var peer PeerDomain
	if err != nil || json.Unmarshal(data, &peer) != nil || peer.Domain != domain || !IsValidDomain(domain) || len(peer.PublicKey) != ed25519.PublicKeySize || !peer.Entries.IsValid() || bytes.Equal(peer.PublicKey, s.identity.SignPublic) || (caller != nil && !bytes.Equal(caller, peer.PublicKey)) {
		return PeerDomain{}, "", &InvalidAdminRequestError{errors.New("invalid domain card")}
	}
	if domain == s.opt.Domain {
		return PeerDomain{}, "", ErrDomainKeyConflict
	}
	peer.Status, peer.AddedAt = status, 0
	db, err := s.getAdminDB(ctx)
	if err != nil {
		return PeerDomain{}, "", err
	}
	peer, previous, changed, err := importPeerDomain(db, peer)
	if err == nil && changed {
		err = s.submitWorldServerEvent(ctx, worldServerDirectoryChanged{})
	}
	return peer, previous, err
}

// sendTrust 只尝试当前这次域连接，发送失败保留已保存的状态。
func (s *worldServerImpl) sendTrust(ctx context.Context, peer PeerDomain) {
	if s.opt.Channels == nil {
		return
	}
	msg := s.trustDecision(peer.Status)
	if peer.Status == DomainPendingOutbound {
		card, err := s.selfCard()
		if err != nil || card == "" {
			return
		}
		msg = DomainTrustMessage{Op: "trust_request", Card: card}
	}
	if err := s.opt.Channels.SendTrust(ctx, s.identity, peer, msg, s); err != nil {
		telemetry.GetLogger(ctx).Debug("send World trust message", "domain", peer.Domain, "error", err)
	}
}

func (s *worldServerImpl) trustDecision(status string) DomainTrustMessage {
	switch status {
	case DomainMutual:
		return DomainTrustMessage{Op: "trust_response", Domain: s.opt.Domain, Decision: "accepted"}
	case DomainRejected:
		return DomainTrustMessage{Op: "trust_response", Domain: s.opt.Domain, Decision: "rejected"}
	}
	return DomainTrustMessage{}
}

// GetPendingTrustCard 按已认证的通道公钥查找本域仍待对方接受的邀请。
func (s *worldServerImpl) GetPendingTrustCard(ctx context.Context, caller ed25519.PublicKey) (string, error) {
	db, err := s.getAdminDB(ctx)
	if err != nil {
		return "", err
	}
	var pending int64
	if err := db.Model(&PeerDomain{}).Where("public_key = ? AND status = ?", []byte(caller), DomainPendingOutbound).Count(&pending).Error; err != nil || pending == 0 {
		return "", err
	}
	return s.selfCard()
}

// ReceiveTrust 按签名身份处理邀请或决定，已作出的决定交给当前通道回送。
func (s *worldServerImpl) ReceiveTrust(ctx context.Context, caller ed25519.PublicKey, msg DomainTrustMessage) (DomainTrustMessage, error) {
	if msg.Op == "trust_request" {
		peer, _, err := s.importCard(ctx, msg.Card, DomainPendingInbound, caller)
		if err != nil {
			telemetry.GetLogger(ctx).Warn("World trust invitation rejected", "error", err)
			return DomainTrustMessage{}, err
		}
		return s.trustDecision(peer.Status), nil
	}
	status := DomainMutual
	switch msg.Decision {
	case "accepted":
	case "rejected":
		status = DomainRejected
	default:
		return DomainTrustMessage{}, errors.New("invalid trust decision")
	}
	db, err := s.getAdminDB(ctx)
	if err != nil {
		return DomainTrustMessage{}, err
	}
	result := db.Model(&PeerDomain{}).Where("domain = ? AND public_key = ? AND status = ?", msg.Domain, []byte(caller), DomainPendingOutbound).Update("status", status)
	if result.Error != nil {
		return DomainTrustMessage{}, result.Error
	}
	if result.RowsAffected == 0 {
		return DomainTrustMessage{}, ErrDomainNotTrusted
	}
	return DomainTrustMessage{}, s.submitWorldServerEvent(ctx, worldServerDirectoryChanged{})
}

// DecideDomain 由管理员接受或拒绝本域收到的邀请。
func (s *worldServerImpl) DecideDomain(ctx context.Context, domain string, accept bool) error {
	status := DomainRejected
	if accept {
		status = DomainMutual
	}
	db, err := s.getAdminDB(ctx)
	if err != nil {
		return err
	}
	var peer PeerDomain
	result := db.Model(&peer).Clauses(clause.Returning{}).Where("domain = ? AND status = ?", domain, DomainPendingInbound).Update("status", status)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return &InvalidAdminRequestError{errors.New("no inbound invitation for this domain")}
	}
	if err = s.submitWorldServerEvent(ctx, worldServerDirectoryChanged{}); err != nil {
		return err
	}
	s.sendTrust(ctx, peer)
	return nil
}

// DeleteDomain 删除域名片并推送新的信任列表。
func (s *worldServerImpl) DeleteDomain(ctx context.Context, domain string) error {
	if !IsValidDomain(domain) {
		return &InvalidAdminRequestError{errors.New("invalid domain")}
	}
	db, err := s.getAdminDB(ctx)
	if err != nil {
		return err
	}
	if err = db.Delete(&PeerDomain{}, "domain = ?", domain).Error; err != nil {
		return err
	}
	return s.submitWorldServerEvent(context.WithoutCancel(ctx), worldServerDirectoryChanged{})
}

// GetMember 核对请求域的公钥，向互通域提供本域有效成员的身份和公告。
func (s *worldServerImpl) GetMember(ctx context.Context, caller ed25519.PublicKey, node string) (DomainMember, error) {
	db, err := s.getAdminDB(ctx)
	if err != nil {
		return DomainMember{}, err
	}
	var peer PeerDomain
	err = db.Select("domain").Take(&peer, "public_key = ? AND status = ?", []byte(caller), DomainMutual).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return DomainMember{}, ErrDomainNotTrusted
	}
	if err != nil {
		return DomainMember{}, err
	}
	reply := make(chan worldServerMemberResult, 1)
	if err = s.submitWorldServerEvent(ctx, worldServerMemberRequested{node, reply}); err != nil {
		return DomainMember{}, err
	}
	select {
	case r := <-reply:
		return r.member, r.err
	case <-ctx.Done():
		return DomainMember{}, ctx.Err()
	}
}

// lookupDomainMember 仅按本域名片表中的目标和公钥代查外域成员。
func (s *worldServerImpl) lookupDomainMember(ctx context.Context, req DomainLookup) DomainLookupResult {
	result := DomainLookupResult{RequestID: req.RequestID}
	addr := req.Node + "@" + req.Domain
	var peer PeerDomain
	err := s.db.WithContext(ctx).Take(&peer, "domain = ? AND status = ?", req.Domain, DomainMutual).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		result.Code, result.Error = "world_domain_not_trusted", fmt.Sprintf("domain %q is not trusted", req.Domain)
		return result
	}
	if err == nil && s.opt.Channels != nil {
		result.DomainKey = peer.PublicKey
		var member DomainMember
		member, err = s.opt.Channels.Lookup(ctx, s.identity, peer, req.Node, s)
		if err == nil {
			result.Member = &member
			return result
		}
		var missing PeerNotFoundError
		if errors.As(err, &missing) {
			result.Code, result.Error = "world_peer_not_found", err.Error()
			return result
		}
	}
	result.Code, result.Error = "world_identity_unverified", fmt.Sprintf("cannot verify %q: its domain control plane is unreachable", addr)
	return result
}

// ServeDomainDirect 在本域运行期间接受发往域身份的直连查询。
func (s *worldServerImpl) ServeDomainDirect(c *gin.Context) {
	s.requestsMu.Lock()
	ctx := s.runCtx
	if ctx == nil || ctx.Err() != nil || s.opt.Channels == nil {
		s.requestsMu.Unlock()
		c.Status(http.StatusServiceUnavailable)
		return
	}
	s.tasks.Add(1)
	s.requestsMu.Unlock()
	defer s.tasks.Done()
	if c.Query("target") != relayKey(s.identity.SignPublic) {
		c.Status(http.StatusNotFound)
		return
	}
	socket, err := websocket.Accept(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	socket.SetReadLimit(ChannelFrameLimit)
	_ = s.opt.Channels.ServeDirect(ctx, socket, s.identity, s)
}

func (s *worldServerImpl) handleGetDomains(c *gin.Context) {
	result, err := s.GetDomains(c.Request.Context())
	writeAdminResponse(c, http.StatusOK, result, err)
}
func (s *worldServerImpl) handleAddDomain(c *gin.Context) {
	var req AddDomainRequest
	if c.ShouldBindJSON(&req) != nil {
		writeAdminResponse(c, 0, nil, &InvalidAdminRequestError{errors.New("invalid domain card")})
		return
	}
	peer, created, err := s.AddDomain(c.Request.Context(), req.Card)
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeAdminResponse(c, status, peer, err)
}
func (s *worldServerImpl) handleDeleteDomain(c *gin.Context) {
	writeAdminResponse(c, http.StatusNoContent, nil, s.DeleteDomain(c.Request.Context(), c.Param("domain")))
}
