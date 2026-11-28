package world

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/VaalaCat/openvaala/pkg/worldserver"
)

// LoadOrCreateIdentity 从节点私有目录读取身份，首次启用时原子创建。
func LoadOrCreateIdentity(path string) (NodeIdentity, error) {
	// 1. 创建私有目录，检查已有目录权限。
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return NodeIdentity{}, err
	}
	info, err := os.Stat(dir)
	if err != nil {
		return NodeIdentity{}, err
	}
	if info.Mode().Perm() != 0700 {
		return NodeIdentity{}, fmt.Errorf("world identity directory %q requires mode 0700", dir)
	}

	// 2. 已有身份直接读取，只在身份文件缺失时生成新密钥。
	id, err := readIdentity(path)
	if !errors.Is(err, os.ErrNotExist) {
		return id, err
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return NodeIdentity{}, err
	}
	box, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return NodeIdentity{}, err
	}
	id = NodeIdentity{
		NodeID:      worldserver.GetNodeIDFromSignPublic(public),
		SignPublic:  public,
		SignPrivate: private,
		BoxPublic:   box.PublicKey().Bytes(),
		BoxPrivate:  box.Bytes(),
	}
	// 3. 写完并关闭 0600 临时文件，再用硬链接原子发布；同时创建者复用胜出的身份。
	temp, err := os.CreateTemp(dir, ".identity-*")
	if err != nil {
		return NodeIdentity{}, err
	}
	defer os.Remove(temp.Name())
	writeErr := json.NewEncoder(temp).Encode(id)
	syncErr := temp.Sync()
	closeErr := temp.Close()
	if writeErr != nil {
		return NodeIdentity{}, writeErr
	}
	if syncErr != nil {
		return NodeIdentity{}, syncErr
	}
	if closeErr != nil {
		return NodeIdentity{}, closeErr
	}
	if err = os.Link(temp.Name(), path); errors.Is(err, os.ErrExist) {
		return readIdentity(path)
	}
	if err != nil {
		return NodeIdentity{}, err
	}
	return id, nil
}

// readIdentity 读取本机私钥文件并核对身份。
func readIdentity(path string) (NodeIdentity, error) {
	// 1. 确认文件权限后读取完整 JSON。
	f, err := os.Open(path)
	if err != nil {
		return NodeIdentity{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return NodeIdentity{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return NodeIdentity{}, fmt.Errorf("world identity %q requires a regular file with mode 0600", path)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return NodeIdentity{}, err
	}
	var id NodeIdentity
	if err = json.Unmarshal(data, &id); err != nil {
		return NodeIdentity{}, fmt.Errorf("decode world identity: %w", err)
	}
	// 2. 检查两组密钥及标识。
	if _, err = id.validateKeys(); err != nil {
		return NodeIdentity{}, err
	}
	return id, nil
}

// validateKeys 核对节点标识及密钥配对，返回可用于握手的静态私钥。
func (id NodeIdentity) validateKeys() (*ecdh.PrivateKey, error) {
	// 1. 从私钥种子核对完整签名密钥及节点标识。
	if len(id.SignPrivate) != ed25519.PrivateKeySize || len(id.SignPublic) != ed25519.PublicKeySize ||
		id.NodeID != worldserver.GetNodeIDFromSignPublic(id.SignPublic) {
		return nil, errors.New("invalid world signing identity")
	}
	derived := ed25519.NewKeyFromSeed(id.SignPrivate[:ed25519.SeedSize])
	if !bytes.Equal(derived, id.SignPrivate) || !bytes.Equal(derived[ed25519.SeedSize:], id.SignPublic) {
		return nil, errors.New("world signing key pair mismatch")
	}
	// 2. 从静态私钥核对加密公钥。
	box, err := ecdh.X25519().NewPrivateKey(id.BoxPrivate)
	if err != nil || !bytes.Equal(box.PublicKey().Bytes(), id.BoxPublic) {
		return nil, errors.New("invalid world box key pair")
	}
	return box, nil
}

// Sign 使用本机签名私钥签署消息。
func (id NodeIdentity) Sign(message []byte) ([]byte, error) {
	// 1. 校验私钥长度并签名。
	if len(id.SignPrivate) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid world signing private key")
	}
	return ed25519.Sign(id.SignPrivate, message), nil
}

// RegisterNode 用节点身份向控制面注册，返回控制面确认的节点名和域。
func RegisterNode(ctx context.Context, identity NodeIdentity, serverURL, nodeName, joinToken string) (worldserver.RegisterNodeResponse, error) {
	var result worldserver.RegisterNodeResponse
	req := worldserver.RegisterNodeRequest{NodeName: nodeName, JoinToken: joinToken, SignPublic: identity.SignPublic, BoxPublic: identity.BoxPublic}
	signature, err := identity.Sign(worldserver.RegisterSignBytes(req))
	if err != nil {
		return result, err
	}
	req.Signature = signature
	payload, err := json.Marshal(req)
	if err != nil {
		return result, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(serverURL, "/")+"/world/v1/nodes/register", bytes.NewReader(payload))
	if err != nil {
		return result, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, err := io.ReadAll(response.Body)
		if err != nil {
			return result, err
		}
		return result, fmt.Errorf("world request: %s %s", response.Status, bytes.TrimSpace(body))
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return result, err
	}
	if result.NodeID != identity.NodeID || result.NodeName != nodeName || result.Domain == "" {
		return result, errors.New("invalid world registration response")
	}
	return result, nil
}
