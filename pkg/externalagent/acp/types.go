package acp

import (
	"strings"

	"github.com/VaalaCat/openvaala/pkg/defs"
	sdk "github.com/coder/acp-go-sdk"
)

// ── ACP Provider 查询位置 ──

type optionsCacheKey struct{ executorID, cwd, selections string }

type listPosition struct {
	Cursor string
	Skip   int
}

// ── ACP Session 通知分片 ──

type protocolChunk struct {
	role, id string
	index    int
	item     *defs.Item
	text     strings.Builder
}

type protocolToolCall struct {
	index     int
	item      *defs.Item
	rawOutput any
	content   []sdk.ToolCallContent
}
