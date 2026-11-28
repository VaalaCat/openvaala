// Package acp 通过目标 Workspace 的 stdio 连接外部 ACP Agent。
package acp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/externalagent"
	"github.com/VaalaCat/openvaala/pkg/utils/syncmap"
	"github.com/VaalaCat/openvaala/pkg/workspace"
	sdk "github.com/coder/acp-go-sdk"
)

type acpAgentProvider struct {
	command defs.ACPCommand
	options syncmap.SyncMap[optionsCacheKey, defs.ExternalOptions]
}

// NewACPAgentProvider 保存配置，只在探测或运行会话时启动进程。
func NewACPAgentProvider(command defs.ACPCommand) externalagent.ExternalAgentProvider {
	return &acpAgentProvider{command: *command.Clone()}
}

// GetCachedOptions 只读指定位置的无 selections 结果。原生 Session 无对应的后端探测缓存。
func (p *acpAgentProvider) GetCachedOptions(executorID, cwd string) (defs.ExternalOptions, bool) {
	options, found := p.options.Load(optionsCacheKey{executorID: executorID, cwd: cwd})
	return cloneOptions(options), found
}

// GetOptions 在临时会话逐项应用选择并读取联动配置。原生 Session 从模型定义取得选项。
func (p *acpAgentProvider) GetOptions(ctx context.Context, req externalagent.ExternalAgentOptionsRequest) (defs.ExternalOptions, error) {
	key := optionsCacheKey{executorID: req.ExecutorID, cwd: req.Cwd}
	if len(req.Selections) > 0 {
		encoded, _ := json.Marshal(req.Selections)
		key.selections = string(encoded)
	}
	if !req.ForceRefresh {
		if opts, ok := p.options.Load(key); ok {
			return cloneOptions(opts), nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	s := &acpAgentSession{command: p.command, ctx: ctx}
	conn, err := s.startACPConnection(ctx, req.Workspace, req.Cwd, nil)
	if err != nil {
		return defs.ExternalOptions{}, err
	}
	defer conn.close()
	s.connection = conn
	create, err := mergeParamOverwrite(sdk.NewSessionRequest{Cwd: req.Cwd, McpServers: []sdk.McpServer{}}, sdk.AgentMethodSessionNew, p.command.ParamOverwrite)
	if err != nil {
		return defs.ExternalOptions{}, err
	}
	created, err := conn.client.NewSession(ctx, create)
	if err != nil {
		return defs.ExternalOptions{}, rpcError(err)
	}
	s.setSessionResponse(created.SessionId, created.Modes, created.ConfigOptions)
	for _, selection := range req.Selections {
		if err := s.SetOption(ctx, selection); err != nil {
			return defs.ExternalOptions{}, err
		}
	}
	s.protocolMu.Lock()
	options := externalOptions(s.modes, s.config)
	s.protocolMu.Unlock()
	p.options.Store(key, cloneOptions(options))
	return options, nil
}

// ListSessions 按工作目录分页列举后端会话。原生 Session 的列表由宿主 DAO 提供。
func (p *acpAgentProvider) ListSessions(ctx context.Context, ws workspace.Workspace, cwd, query, cursor string) ([]externalagent.ExternalSessionInfo, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	s := &acpAgentSession{command: p.command, ctx: ctx}
	c, err := s.startACPConnection(ctx, ws, cwd, nil)
	if err != nil {
		return nil, "", err
	}
	defer c.close()
	if c.capabilities.SessionCapabilities.List == nil {
		return nil, "", errors.New("external agent does not support session listing")
	}
	search := strings.ToLower(strings.TrimSpace(query))
	var rows []externalagent.ExternalSessionInfo
	skip := 0
	if search != "" {
		cursor, skip, err = parseSearchCursor(cursor)
		if err != nil {
			return nil, "", err
		}
	}
	scanned := 0
	for {
		req := sdk.ListSessionsRequest{Cwd: &cwd}
		if cursor != "" {
			req.Cursor = &cursor
		}
		req, err := mergeParamOverwrite(req, sdk.AgentMethodSessionList, p.command.ParamOverwrite)
		if err != nil {
			return nil, "", err
		}
		page, err := c.client.ListSessions(ctx, req)
		if err != nil {
			return nil, "", rpcError(err)
		}
		for i := skip; i < len(page.Sessions); i++ {
			entry := page.Sessions[i]
			title := ptrValue(entry.Title)
			if search == "" || strings.Contains(strings.ToLower(title), search) || strings.Contains(strings.ToLower(entry.Cwd), search) {
				stamp, _ := time.Parse(time.RFC3339Nano, ptrValue(entry.UpdatedAt))
				var updated int64
				if !stamp.IsZero() {
					updated = stamp.UnixMilli()
				}
				rows = append(rows, externalagent.ExternalSessionInfo{SessionID: string(entry.SessionId), Cwd: entry.Cwd, Title: title, UpdatedAt: updated})
			}
			scanned++
			if search != "" && (scanned == 500 || len(rows) == 20) {
				if i+1 < len(page.Sessions) {
					return rows, searchCursor(cursor, i+1), nil
				}
				return rows, ptrValue(page.NextCursor), nil
			}
		}
		next := ptrValue(page.NextCursor)
		if search == "" || next == "" {
			return rows, next, nil
		}
		if next == cursor {
			return nil, "", errors.New("ACP session list did not advance cursor")
		}
		cursor, skip = next, 0
	}
}

// PreviewSession 只保留回放中的首末用户消息。原生 Session 直接查询本机历史。
func (p *acpAgentProvider) PreviewSession(ctx context.Context, ws workspace.Workspace, b defs.ExternalSession) (externalagent.ExternalSessionPreview, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	s := &acpAgentSession{command: p.command, ctx: ctx, replaying: true, sessionID: sdk.SessionId(b.SessionID)}
	c, err := s.startACPConnection(ctx, ws, b.Cwd, nil)
	if err != nil {
		return externalagent.ExternalSessionPreview{}, err
	}
	defer c.close()
	if !c.capabilities.LoadSession {
		return externalagent.ExternalSessionPreview{}, errors.New("external agent does not support session loading")
	}
	load, err := mergeParamOverwrite(sdk.LoadSessionRequest{Cwd: b.Cwd, SessionId: s.sessionID, McpServers: []sdk.McpServer{}}, sdk.AgentMethodSessionLoad, p.command.ParamOverwrite)
	if err != nil {
		return externalagent.ExternalSessionPreview{}, err
	}
	_, err = c.client.LoadSession(ctx, load)
	if err != nil {
		return externalagent.ExternalSessionPreview{}, rpcError(err)
	}
	s.protocolMu.Lock()
	defer s.protocolMu.Unlock()
	s.flushChunk(defs.ReasoningCompleted)
	return s.preview, nil
}

// OpenSession 与原生 NewSession 一样只保存依赖，Run 使用 Binding 决定新建或恢复。
func (p *acpAgentProvider) OpenSession(req externalagent.ExternalSessionOpenRequest) externalagent.ExternalAgentSession {
	return &acpAgentSession{command: p.command, openReq: req}
}
