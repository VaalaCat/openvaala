package world

import (
	"errors"
	"fmt"
	"github.com/VaalaCat/openvaala/pkg/worldserver"
	"regexp"

	"strings"
)

// ErrInvalidWorldAddress 标识无法解析的联邦地址。
var ErrInvalidWorldAddress = errors.New("invalid_world_address")

var nodeOrTargetPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// ParseAddress 解析节点与可选目标，并补全同域简写。
func ParseAddress(raw, defaultDomain string) (Address, error) {
	// 1. 切分目标及域名，简写使用调用方提供的域名。
	nodeAddress, target, hasTarget := strings.Cut(raw, "/")
	node, domain, hasDomain := strings.Cut(nodeAddress, "@")
	if !hasDomain {
		domain = defaultDomain
	}
	// 2. 校验节点、域名和显式指定的目标。
	if !nodeOrTargetPattern.MatchString(node) || !worldserver.IsValidDomain(domain) || (hasTarget && !nodeOrTargetPattern.MatchString(target)) {
		return Address{}, fmt.Errorf("%w: invalid world address %q", ErrInvalidWorldAddress, raw)
	}
	return Address{
		Node:   node,
		Domain: domain,
		Target: target,
	}, nil
}

// NodeAddress 返回节点的完整地址。
func (a Address) NodeAddress() string {
	// 1. 拼接节点名与域名。
	return a.Node + "@" + a.Domain
}

// String 返回含目标的完整地址。
func (a Address) String() string {
	// 1. 指定目标时追加目标路径。
	if a.Target != "" {
		return a.NodeAddress() + "/" + a.Target
	}
	return a.NodeAddress()
}
