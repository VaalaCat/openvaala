package worldserver

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
	_ "modernc.org/sqlite"
)

var (
	errInvalidJoinToken = errors.New("world_join_token_invalid")
	errNodeRevoked      = errors.New("world_node_revoked")
	errNodeConflict     = errors.New("world_node_conflict")
)

// TableName 返回域身份表名。
func (ServerMetaRecord) TableName() string {
	// 1. 使用控制面固定表名。
	return "server_meta"
}

// TableName 返回入网码表名。
func (JoinTokenRecord) TableName() string {
	// 1. 使用控制面固定表名。
	return "join_tokens"
}

// TableName 返回节点表名。
func (NodeRecord) TableName() string {
	// 1. 使用控制面固定表名。
	return "nodes"
}

// TableName 返回权限规则表名。
func (ACLRuleRecord) TableName() string {
	// 1. 使用控制面固定表名。
	return "acl_rules"
}

// openSQLiteDB 打开并初始化控制面数据库。
func openSQLiteDB(path, domain string, staticACL []ACLRule) (_ *gorm.DB, closeDB func() error, err error) {
	// 1. 创建私有目录和数据库文件。
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, nil, err
	}
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, err
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, nil, err
	}
	if info.Mode().Perm() != 0o700 {
		return nil, nil, fmt.Errorf("world database directory %q must have permissions 0700", dir)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, nil, err
	}
	if err = errors.Join(file.Chmod(0o600), file.Close()); err != nil {
		return nil, nil, err
	}

	// 2. 用纯 Go SQLite 打开 WAL 数据库，每个写事务先取得写锁。
	fileURL := url.URL{
		Scheme:   "file",
		Path:     path,
		RawQuery: "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_txlock=immediate",
	}
	dsn := fileURL.String()
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, sqlDB.Close())
		}
	}()
	sqlDB.SetMaxOpenConns(1)
	db, err := gorm.Open(sqlite.Dialector{
		DriverName: "sqlite",
		DSN:        dsn,
		Conn:       sqlDB,
	}, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return nil, nil, err
	}

	// 3. 创建业务表，首次启动一起写入域身份和初始规则。
	if err = db.AutoMigrate(&ServerMetaRecord{}, &JoinTokenRecord{}, &NodeRecord{}, &ACLRuleRecord{}, &PeerDomain{}); err != nil {
		return nil, nil, err
	}
	if err = db.Transaction(func(tx *gorm.DB) error {
		// 1. 读取域身份，已初始化时校验配置域名。
		var meta ServerMetaRecord
		err := tx.First(&meta, 1).Error
		if err == nil {
			if meta.Domain != domain {
				return fmt.Errorf("world database belongs to domain %q", meta.Domain)
			}
			if len(meta.BoxPrivate) == 0 {
				box, err := ecdh.X25519().GenerateKey(rand.Reader)
				if err != nil {
					return err
				}
				return tx.Model(&meta).Updates(map[string]any{"box_private": box.Bytes(), "box_public": box.PublicKey().Bytes()}).Error
			}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		// 2. 首次启动时建立域签名身份和初始规则。
		public, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		box, err := ecdh.X25519().GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		meta = ServerMetaRecord{
			ID:             1,
			Domain:         domain,
			DomainIdentity: DomainIdentity{SignPublic: public, SignPrivate: private, BoxPublic: box.PublicKey().Bytes(), BoxPrivate: box.Bytes()},
		}
		if err := tx.Create(&meta).Error; err != nil {
			return err
		}
		if len(staticACL) == 0 {
			return nil
		}
		return tx.Create(aclRuleRecords(staticACL)).Error
	}); err != nil {
		return nil, nil, err
	}
	return db, sqlDB.Close, nil
}

// createJoinToken 签发入网码并只保存其哈希。
func createJoinToken(db *gorm.DB, req CreateJoinTokenRequest, serverURL string) (CreateJoinTokenResponse, error) {
	// 1. 生成入网码明文及独立的管理标识。
	token := serverURL + "#" + rand.Text()
	record := JoinTokenRecord{
		ID:        uuid.NewString(),
		TokenHash: joinTokenHash(token),
		Owner:     req.Owner,
		Tags:      append(datatypes.JSONSlice[string]{}, req.Tags...),
		Reusable:  req.Reusable,
		ExpiresAt: req.ExpiresAt,
	}

	// 2. 写入记录并返回一次性明文。
	if err := db.Create(&record).Error; err != nil {
		return CreateJoinTokenResponse{}, err
	}
	return CreateJoinTokenResponse{
		JoinTokenInfo: joinTokenInfo(record),
		Token:         token,
	}, nil
}

// getJoinTokens 按创建顺序读取入网码状态。
func getJoinTokens(db *gorm.DB) ([]JoinTokenInfo, error) {
	// 1. 读取记录并提取管理面可见字段。
	var records []JoinTokenRecord
	if err := db.Order("created_at, id").Find(&records).Error; err != nil {
		return nil, err
	}
	tokens := make([]JoinTokenInfo, 0, len(records))
	for _, record := range records {
		tokens = append(tokens, joinTokenInfo(record))
	}
	return tokens, nil
}

// revokeJoinToken 吊销指定入网码。
func revokeJoinToken(db *gorm.DB, id string) error {
	// 1. 只在首次吊销时记录时间。
	return db.Model(&JoinTokenRecord{}).Where("id = ? AND revoked_at = 0", id).
		Update("revoked_at", time.Now().UnixMilli()).Error
}

// registerNode 同事务核销入网码并登记已验签的节点。
func registerNode(db *gorm.DB, req RegisterNodeRequest) (NodeRecord, error) {
	var node NodeRecord
	err := db.Transaction(func(tx *gorm.DB) error {
		// 1. 读取未过期、未吊销且仍可使用的入网码。
		var token JoinTokenRecord
		err := tx.Where("token_hash = ? AND expires_at > ? AND revoked_at = 0 AND (reusable OR used_count = 0)",
			joinTokenHash(req.JoinToken), time.Now().UnixMilli()).Take(&token).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errInvalidJoinToken
		}
		if err != nil {
			return err
		}

		// 2. 递增核销次数并写入唯一的节点身份和名称。
		if err := tx.Model(&token).UpdateColumn("used_count", gorm.Expr("used_count + 1")).Error; err != nil {
			return err
		}
		node = NodeRecord{
			NodeID:     GetNodeIDFromSignPublic(req.SignPublic),
			NodeName:   req.NodeName,
			Owner:      token.Owner,
			Tags:       token.Tags,
			SignPublic: req.SignPublic,
			BoxPublic:  req.BoxPublic,
		}
		if err := tx.Where("node_id = ? AND revoked_at <> 0", node.NodeID).Delete(&NodeRecord{}).Error; err != nil {
			return err
		}
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&node)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return errNodeConflict
		}
		return nil
	})
	return node, err
}

// getNode 读取指定节点的身份与吊销状态。
func getNode(db *gorm.DB, nodeID string) (NodeRecord, error) {
	// 1. 按身份标识读取记录。
	var node NodeRecord
	err := db.Take(&node, "node_id = ?", nodeID).Error
	return node, err
}

// getNodes 按节点名顺序列出节点。
func getNodes(db *gorm.DB, includeRevoked bool) ([]NodeRecord, error) {
	// 1. 根据调用方要求选取节点并排序。
	if !includeRevoked {
		db = db.Where("revoked_at = 0")
	}
	nodes := make([]NodeRecord, 0)
	err := db.Order("node_name").Find(&nodes).Error
	return nodes, err
}

// revokeNodes 按节点标识或归属吊销节点，并返回被吊销节点的名称和签名公钥。
func revokeNodes(db *gorm.DB, nodeID, owner string) ([]NodeRecord, error) {
	// 1. 只更新匹配的未吊销记录，并取回用于清理控制连接和域内挂靠的身份。
	db = db.Where("revoked_at = 0")
	if nodeID != "" {
		db = db.Where("node_id = ?", nodeID)
	} else {
		db = db.Where("owner = ?", owner)
	}
	var nodes []NodeRecord
	err := db.Model(&nodes).Clauses(clause.Returning{Columns: []clause.Column{{Name: "node_name"}, {Name: "sign_public"}}}).
		Update("revoked_at", time.Now().UnixMilli()).Error
	if err != nil {
		return nil, err
	}
	return nodes, nil
}

// getACL 按优先级和提交顺序读取规则。
func getACL(db *gorm.DB) ([]ACLRule, error) {
	// 1. 读取有序规则并转换为协议结构。
	var records []ACLRuleRecord
	if err := db.Order("priority, id").Find(&records).Error; err != nil {
		return nil, err
	}
	rules := make([]ACLRule, 0, len(records))
	for _, record := range records {
		rules = append(rules, ACLRule{
			Priority: record.Priority,
			From:     record.From,
			To:       record.To,
			Action:   record.Action,
		})
	}
	return rules, nil
}

// replaceACL 在一个事务内用提交的规则替换整份 ACL。
func replaceACL(db *gorm.DB, rules []ACLRule) error {
	// 1. 在同一事务删除旧规则并按提交顺序写入新规则。
	return db.Transaction(func(tx *gorm.DB) error {
		// 1. 删除全部旧规则。
		if err := tx.Exec("DELETE FROM acl_rules").Error; err != nil {
			return err
		}
		if len(rules) == 0 {
			return nil
		}
		// 2. 保存替换后的规则列表。
		return tx.Create(aclRuleRecords(rules)).Error
	})
}

// aclRuleRecords 将协议规则转换为持久化记录。
func aclRuleRecords(rules []ACLRule) []ACLRuleRecord {
	// 1. 按提交顺序转换规则。
	records := make([]ACLRuleRecord, 0, len(rules))
	for _, rule := range rules {
		records = append(records, ACLRuleRecord{
			Priority: rule.Priority,
			From:     datatypes.JSONSlice[string](rule.From),
			To:       datatypes.JSONSlice[string](rule.To),
			Action:   rule.Action,
		})
	}
	return records
}

// validateJoinTokenRequest 校验入网码的归属与过期时间。
func validateJoinTokenRequest(req CreateJoinTokenRequest) error {
	// 1. 要求非空归属及未来的过期时间。
	if strings.TrimSpace(req.Owner) == "" || req.ExpiresAt <= time.Now().UnixMilli() {
		return errors.New("join token requires an owner and a future expires_at")
	}
	return nil
}

// getPeerDomains 返回按域名排序的互通域快照。
func getPeerDomains(db *gorm.DB) ([]PeerDomain, error) {
	domains := []PeerDomain{}
	err := db.Order("domain").Find(&domains).Error
	return domains, err
}

// importPeerDomain 在同一事务内绑定域公钥，同钥名片可以更新进入方式。
func importPeerDomain(db *gorm.DB, peer PeerDomain) (record PeerDomain, previous string, changed bool, err error) {
	err = db.Transaction(func(tx *gorm.DB) error {
		var saved PeerDomain
		err := tx.Take(&saved, "domain = ?", peer.Domain).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			changed = true
			return tx.Create(&peer).Error
		}
		if err != nil {
			return err
		}
		if !bytes.Equal(saved.PublicKey, peer.PublicKey) {
			return ErrDomainKeyConflict
		}
		previous = saved.Status
		switch {
		case saved.Status == DomainMutual, peer.Status == DomainPendingInbound && saved.Status == DomainRejected:
			peer.Status = saved.Status
		case saved.Status == DomainPendingInbound && peer.Status == DomainPendingOutbound, saved.Status == DomainPendingOutbound && peer.Status == DomainPendingInbound:
			peer.Status = DomainMutual
		}
		if peer.Status == saved.Status && slices.Equal(saved.Entries.DirectURLs, peer.Entries.DirectURLs) && slices.Equal(saved.Entries.Relays, peer.Entries.Relays) {
			peer = saved
			return nil
		}
		changed = true
		peer.AddedAt = saved.AddedAt
		return tx.Save(&peer).Error
	})
	return peer, previous, changed, err
}
