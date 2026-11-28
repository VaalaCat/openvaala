package world

import (
	"bytes"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/VaalaCat/openvaala/pkg/worldserver"
)

func testNodeIdentity(t *testing.T) NodeIdentity {
	t.Helper()
	// 1. 在独立目录生成测试节点身份。
	id, err := LoadOrCreateIdentity(filepath.Join(t.TempDir(), "world", "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestLoadOrCreateIdentity(t *testing.T) {
	// 1. 同时首次启用的调用方应读到同一份完整身份。
	dir := filepath.Join(t.TempDir(), "world")
	path := filepath.Join(dir, "identity.json")
	var calls sync.WaitGroup
	results := make([]NodeIdentity, 8)
	for i := range results {
		calls.Go(func() {
			var err error
			results[i], err = LoadOrCreateIdentity(path)
			if err != nil {
				t.Error(err)
			}
		})
	}
	calls.Wait()
	id, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range results {
		if !reflect.DeepEqual(id, result) {
			t.Fatal("identity changed between calls")
		}
	}
	// 2. 核对标识生成规则及目录、文件权限。
	sum := sha256.Sum256(id.SignPublic)
	if want := "nd_" + strings.ToLower(base32.StdEncoding.EncodeToString(sum[:])[:16]); id.NodeID != want {
		t.Fatalf("node ID %q, want %q", id.NodeID, want)
	}
	for name, mode := range map[string]os.FileMode{dir: 0700, path: 0600} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode {
			t.Fatalf("%s: mode %o", name, info.Mode().Perm())
		}
	}

}

func TestRejectCorruptIdentity(t *testing.T) {
	// 1. 在合法身份中分别破坏标识、密钥长度或配对关系。
	for _, tc := range []struct {
		name   string
		damage func(*NodeIdentity)
	}{
		{
			name:   "node id",
			damage: func(id *NodeIdentity) { id.NodeID = "nd_other" },
		},
		{
			name:   "short signing key",
			damage: func(id *NodeIdentity) { id.SignPrivate = id.SignPrivate[:31] },
		},
		{
			name:   "short signing public key",
			damage: func(id *NodeIdentity) { id.SignPublic = nil },
		},
		{
			name:   "short box key",
			damage: func(id *NodeIdentity) { id.BoxPrivate = nil },
		},
		{
			name:   "mismatched signing seed",
			damage: func(id *NodeIdentity) { id.SignPrivate[0] ^= 1 },
		},
		{
			name:   "mismatched box public key",
			damage: func(id *NodeIdentity) { id.BoxPublic[0] ^= 1 },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 1. 写入损坏的身份文件。
			id := testNodeIdentity(t)
			tc.damage(&id)
			data, err := json.Marshal(id)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "identity.json")
			if err = os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			// 2. 读取失败时应保留原文件。
			if _, err = LoadOrCreateIdentity(path); err == nil {
				t.Fatal("accepted corrupt identity")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(data, after) {
				t.Fatalf("identity changed after failure: %v", err)
			}
		})
	}
}

func TestRegisterNode(t *testing.T) {
	for _, code := range []string{"valid", "expired"} {
		t.Run(code, func(t *testing.T) {
			id := testNodeIdentity(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req worldserver.RegisterNodeRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.NodeName != "one" || len(req.Signature) == 0 || r.URL.Path != "/world/v1/nodes/register" {
					t.Errorf("register: %+v %v", req, err)
				}
				if req.JoinToken != "valid" {
					http.Error(w, "world_join_token_invalid", http.StatusForbidden)
					return
				}
				json.NewEncoder(w).Encode(worldserver.RegisterNodeResponse{NodeName: "one", NodeID: id.NodeID, Domain: "team"})
			}))
			defer server.Close()
			result, err := RegisterNode(t.Context(), id, server.URL, "one", code)
			if code == "valid" {
				if err != nil || result.Domain != "team" {
					t.Fatal(result, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "world_join_token_invalid") {
				t.Fatal(err)
			}
		})
	}
}
