package worldserver

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSQLitePersistence(t *testing.T) {
	// 1. 初始化域数据库并验证文件权限、WAL 和四张业务表。
	dir := filepath.Join(t.TempDir(), "worldserver")
	path := filepath.Join(dir, "world.db")
	initialACL := []ACLRule{{
		From:   []string{"*"},
		To:     []string{"*"},
		Action: "allow",
	}}
	db, closeDB, err := openSQLiteDB(path, "example.com", initialACL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeDB() })
	for _, tt := range []struct {
		path string
		perm os.FileMode
	}{
		{
			path: dir,
			perm: 0o700,
		},
		{
			path: path,
			perm: 0o600,
		},
	} {
		info, err := os.Stat(tt.path)
		if err != nil || info.Mode().Perm() != tt.perm {
			t.Fatalf("stat %s = %v, %v", tt.path, info, err)
		}
	}
	var journalMode string
	if err := db.Raw("PRAGMA journal_mode").Scan(&journalMode).Error; err != nil || journalMode != "wal" {
		t.Fatalf("journal mode = %q, %v", journalMode, err)
	}
	var busyTimeout int
	if err := db.Raw("PRAGMA busy_timeout").Scan(&busyTimeout).Error; err != nil || busyTimeout != 5000 {
		t.Fatalf("busy timeout = %d, %v", busyTimeout, err)
	}
	var tables []string
	if err := db.Raw("SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name").
		Scan(&tables).Error; err != nil || !slices.Equal(tables, []string{"acl_rules", "join_tokens", "nodes", "peer_domains", "server_meta"}) {
		t.Fatalf("tables = %v, %v", tables, err)
	}
	var meta ServerMetaRecord
	if err := db.First(&meta, 1).Error; err != nil || len(meta.SignPrivate) != ed25519.PrivateKeySize ||
		!bytes.Equal(ed25519.PrivateKey(meta.SignPrivate).Public().(ed25519.PublicKey), meta.SignPublic) {
		t.Fatal("invalid persisted domain identity", err)
	}
	if rules, err := getACL(db); err != nil || len(rules) != 1 || rules[0].Action != "allow" {
		t.Fatalf("initial ACL = %v, %v", rules, err)
	}

	// 2. 清空 ACL 再重开库，检查显式配置和域密钥均持久化。
	if err := replaceACL(db, nil); err != nil {
		t.Fatal(err)
	}
	if err := closeDB(); err != nil {
		t.Fatal(err)
	}
	reopened, closeReopened, err := openSQLiteDB(path, "example.com", initialACL)
	if err != nil {
		t.Fatal(err)
	}
	defer closeReopened()
	var persisted ServerMetaRecord
	if err := reopened.First(&persisted, 1).Error; err != nil || !bytes.Equal(persisted.SignPublic, meta.SignPublic) {
		t.Fatal("domain identity changed on reopen", err)
	}
	if rules, err := getACL(reopened); err != nil || len(rules) != 0 {
		t.Fatalf("persisted ACL = %v, %v", rules, err)
	}
}

func TestRegisterNodeTokenTransaction(t *testing.T) {
	for _, tt := range []struct {
		name string
		// reusable 同一个入网码是否可以注册两个节点？
		reusable bool
		// expired 入网码是否已经过期？
		expired bool
		// revoked 入网码是否已被吊销？
		revoked bool
		uses    int
	}{
		{
			name: "single use under concurrent registration",
			uses: 1,
		},
		{
			name:     "reusable",
			reusable: true,
			uses:     2,
		},
		{
			name:    "expired",
			expired: true,
		},
		{
			name:    "revoked",
			revoked: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// 1. 创建测试入网码并按用例设置有效期或吊销状态。
			db, closeDB, err := openSQLiteDB(filepath.Join(t.TempDir(), "worldserver", "world.db"), "example.com", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer closeDB()
			expiration := time.Now().Add(time.Hour).UnixMilli()
			if tt.expired {
				expiration = time.Now().Add(-time.Hour).UnixMilli()
			}
			token, err := createJoinToken(db, CreateJoinTokenRequest{
				Owner:     "alice",
				Tags:      []string{"work"},
				Reusable:  tt.reusable,
				ExpiresAt: expiration,
			}, "https://control.test/deploy")
			if err != nil {
				t.Fatal(err)
			}
			if base, err := ParseJoinCode(token.Token); err != nil || base != "https://control.test/deploy" {
				t.Fatalf("issued join code base = %q, %v", base, err)
			}
			if tt.revoked {
				if err := revokeJoinToken(db, token.ID); err != nil {
					t.Fatal(err)
				}
			}

			// 2. 并发核销同一个入网码，统计成功登记数。
			results := make(chan error, 2)
			for _, name := range []string{"office", "laptop"} {
				go func() {
					public, _, err := ed25519.GenerateKey(rand.Reader)
					if err == nil {
						_, err = registerNode(db, RegisterNodeRequest{
							NodeName:   name,
							JoinToken:  strings.Replace(token.Token, "https://control.test/deploy", "http://local.test", 1),
							SignPublic: public,
							BoxPublic:  make([]byte, 32),
						})
					}
					results <- err
				}()
			}
			uses := 0
			for range 2 {
				err := <-results
				if err == nil {
					uses++
				} else if !errors.Is(err, errInvalidJoinToken) {
					t.Fatal(err)
				}
			}

			// 3. 比较节点数量、核销次数和持久化哈希。
			var record JoinTokenRecord
			if err := db.Take(&record, "id = ?", token.ID).Error; err != nil {
				t.Fatal(err)
			}
			nodes, err := getNodes(db, true)
			if err != nil || uses != tt.uses || record.UsedCount != tt.uses || len(nodes) != tt.uses {
				t.Fatalf("registrations=%d stored uses=%d nodes=%d, want %d each. Error: %v",
					uses, record.UsedCount, len(nodes), tt.uses, err)
			}
			if record.TokenHash != joinTokenHash(token.Token) || record.TokenHash == token.Token {
				t.Fatal("join token was not stored as a hash")
			}
		})
	}
}

func TestRegistrationConflictRollsBackToken(t *testing.T) {
	// 1. 用两个入网码提交同一个节点，第二次登记失败。
	db, closeDB, err := openSQLiteDB(filepath.Join(t.TempDir(), "worldserver", "world.db"), "example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	request := CreateJoinTokenRequest{
		Owner:     "alice",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	}
	first, err := createJoinToken(db, request, "https://control.test")
	if err != nil {
		t.Fatal(err)
	}
	second, err := createJoinToken(db, request, "https://control.test")
	if err != nil {
		t.Fatal(err)
	}
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	node := RegisterNodeRequest{
		NodeName:   "office",
		JoinToken:  first.Token,
		SignPublic: public,
		BoxPublic:  make([]byte, 32),
	}
	if _, err := registerNode(db, node); err != nil {
		t.Fatal(err)
	}
	node.JoinToken = second.Token
	if _, err := registerNode(db, node); !errors.Is(err, errNodeConflict) {
		t.Fatalf("duplicate registration error = %v", err)
	}

	// 2. 检查第二个入网码仍可使用。
	var stored JoinTokenRecord
	if err := db.Take(&stored, "id = ?", second.ID).Error; err != nil || stored.UsedCount != 0 {
		t.Fatalf("failed registration used token %d times, error: %v", stored.UsedCount, err)
	}
}

func TestPeerDomainPersistence(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(fmt.Sprintf("deleted=%v", deleted), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "domain", "world.db")
			db, closeDB, err := openSQLiteDB(path, "home", nil)
			if err != nil {
				t.Fatal(err)
			}
			peer := PeerDomain{Status: DomainPendingOutbound, Domain: "bob.org", PublicKey: make([]byte, 32), Entries: PeerEntries{Relays: []string{"https://x.test"}}}
			saved, added, _, err := importPeerDomain(db, peer)
			if err != nil || added != "" || saved.AddedAt <= 0 {
				t.Fatal(saved, added, err)
			}
			var before, after int
			if err = db.Raw("SELECT total_changes()").Scan(&before).Error; err != nil {
				t.Fatal(err)
			}
			repeated, created, changed, err := importPeerDomain(db, peer)
			if err != nil || created != DomainPendingOutbound || changed || !reflect.DeepEqual(repeated, saved) {
				t.Fatalf("duplicate import: record=%+v previous=%q changed=%t err=%v", repeated, created, changed, err)
			}
			if err = db.Raw("SELECT total_changes()").Scan(&after).Error; err != nil || after != before {
				t.Fatalf("duplicate import: SQLite changes %d -> %d, error %v", before, after, err)
			}
			var original ServerMetaRecord
			if err = db.First(&original).Error; err != nil {
				t.Fatal(err)
			}
			if deleted {
				if err = db.Delete(&saved).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err = closeDB(); err != nil {
				t.Fatal(err)
			}
			db, closeDB, err = openSQLiteDB(path, "home", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer closeDB()
			var meta ServerMetaRecord
			if err = db.First(&meta).Error; err != nil {
				t.Fatal(err)
			}
			box, err := ecdh.X25519().NewPrivateKey(meta.BoxPrivate)
			if err != nil || !bytes.Equal(box.PublicKey().Bytes(), original.BoxPublic) || !bytes.Equal(meta.BoxPublic, original.BoxPublic) {
				t.Fatal("domain exchange key", err)
			}
			peers, err := getPeerDomains(db)
			if err != nil || (deleted && len(peers) != 0) || (!deleted && (len(peers) != 1 || !reflect.DeepEqual(peers[0], saved))) {
				t.Fatal(peers, err)
			}
		})
	}
}
