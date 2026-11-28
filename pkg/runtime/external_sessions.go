package runtime

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/externalagent"
	"github.com/VaalaCat/openvaala/pkg/storage"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
)

// getExternalAgentProbe 定位定义、Executor 与缓存键，不打开工作区或连接后端。
func (d *runtimeDefinitions) getExternalAgentProbe(ctx context.Context, location ExternalAgentLocation, cid defs.ConversationID) (externalAgentProbe, error) {
	var p externalAgentProbe
	index := slices.IndexFunc(d.conversationDefinitions.Agents, func(a *agent.AgentDefinition) bool { return a.Name == location.AgentName })
	if index < 0 {
		return p, fmt.Errorf("agent %q is not configured", location.AgentName)
	}
	definition := d.conversationDefinitions.Agents[index]
	provider, ok := d.externalProviders.Load(location.AgentName)
	if !ok {
		var err error
		provider, err = conversation.NewExternalAgentProvider(*definition)
		if err != nil {
			return p, err
		}
		provider, _ = d.externalProviders.LoadOrStore(location.AgentName, provider)
	}
	executors, err := d.conversationDefinitions.Executors.GetExecutors(ctx, cid)
	if err != nil {
		return p, err
	}
	for _, e := range executors {
		def := e.GetDefinition()
		if def.Kind == defs.ExecutorKindMemory || location.ExecutorID != "" && def.ID != location.ExecutorID {
			continue
		}
		location.ExecutorID = def.ID
		if location.Cwd == "" {
			location.Cwd = def.WorkspaceDirectory
		} else if !filepath.IsAbs(location.Cwd) {
			location.Cwd = filepath.Join(def.WorkspaceDirectory, location.Cwd)
		}
		p = externalAgentProbe{ExternalAgentLocation: location, provider: provider, agentType: definition.Type, executor: e}
		return p, nil
	}
	return p, fmt.Errorf("unavailable executor %q", location.ExecutorID)
}

// requestOptionsProbe 将探测交给 Runtime；请求取消只结束本次等待。
func (r *runtimeImpl) requestOptionsProbe(ctx context.Context, d *runtimeDefinitions, p externalAgentProbe, req AgentOptionsRequest) (*optionsProbeResult, error) {
	change := &optionsProbeRequested{definitions: d, probe: p, request: req, reply: make(chan *optionsProbeResult, 1)}
	select {
	case r.requests <- change:
		return <-change.reply, nil
	case <-r.stopped:
		return nil, ErrRuntimeStopped
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *runtimeImpl) handleOptionsProbeRequested(ctx context.Context, req *optionsProbeRequested) {
	request := req.request
	request.ExternalAgentLocation = req.probe.ExternalAgentLocation
	// 强制刷新复用已经发出的同一次真实探测。
	request.Refresh = false
	encoded, _ := json.Marshal(request)
	key := string(encoded)
	result, already := req.definitions.optionProbes.LoadOrStore(key, &optionsProbeResult{done: make(chan struct{})})
	req.reply <- result
	if already {
		return
	}
	r.tasks.Go(func() {
		defer func() { req.definitions.optionProbes.Delete(key); close(result.done) }()
		p := req.probe
		if !req.request.Refresh && len(request.Selections) == 0 {
			if opts, ok := p.provider.GetCachedOptions(p.ExecutorID, p.Cwd); ok {
				result.options = opts
				return
			}
		}
		ws, err := p.executor.NewWorkspace(ctx, &executor.NewWorkspaceRequest{RootDirectory: p.Cwd})
		if err == nil {
			defer ws.Close()
			result.options, err = p.provider.GetOptions(ctx, externalagent.ExternalAgentOptionsRequest{Workspace: ws, ExecutorID: p.ExecutorID, Cwd: p.Cwd, Selections: request.Selections, ForceRefresh: req.request.Refresh})
		}
		if err != nil {
			telemetry.GetLogger(ctx).Warn("probe external agent options failed", "agent", p.AgentName, "error", err)
		}
	})
}

// GetAgentOptions 预加载选项；协议探测失败时保留空选项，用户仍可新建会话。
func (r *runtimeImpl) GetAgentOptions(ctx context.Context, req AgentOptionsRequest) (defs.ExternalOptions, error) {
	d := r.definitions.Load()
	p, err := d.getExternalAgentProbe(ctx, req.ExternalAgentLocation, "")
	if err != nil {
		return defs.ExternalOptions{}, err
	}
	result, err := r.requestOptionsProbe(ctx, d, p, req)
	if err != nil {
		return defs.ExternalOptions{}, err
	}
	select {
	case <-result.done:
		return result.options, nil
	case <-ctx.Done():
		return defs.ExternalOptions{}, ctx.Err()
	}
}

// ListExternalSessions 打开所选位置的临时工作区，分页读取可导入会话。
func (r *runtimeImpl) ListExternalSessions(ctx context.Context, req ExternalSessionsRequest) (ExternalSessionsResult, error) {
	p, err := r.definitions.Load().getExternalAgentProbe(ctx, req.ExternalAgentLocation, "")
	if err != nil {
		return ExternalSessionsResult{}, err
	}
	ws, err := p.executor.NewWorkspace(ctx, &executor.NewWorkspaceRequest{RootDirectory: p.Cwd})
	if err != nil {
		return ExternalSessionsResult{}, err
	}
	defer ws.Close()
	rows, next, err := p.provider.ListSessions(ctx, ws, p.Cwd, req.Query, req.Cursor)
	return ExternalSessionsResult{Sessions: rows, NextCursor: next}, err
}

// PreviewExternalSession 只回放选中的一条外部会话。
func (r *runtimeImpl) PreviewExternalSession(ctx context.Context, req ExternalSessionRequest) (externalagent.ExternalSessionPreview, error) {
	p, err := r.definitions.Load().getExternalAgentProbe(ctx, req.ExternalAgentLocation, "")
	if err != nil {
		return externalagent.ExternalSessionPreview{}, err
	}
	if req.ExternalSessionID == "" {
		return externalagent.ExternalSessionPreview{}, errors.New("external_session_id is required")
	}
	ws, err := p.executor.NewWorkspace(ctx, &executor.NewWorkspaceRequest{RootDirectory: p.Cwd})
	if err != nil {
		return externalagent.ExternalSessionPreview{}, err
	}
	defer ws.Close()
	return p.provider.PreviewSession(ctx, ws, defs.ExternalSession{SessionID: req.ExternalSessionID, ExecutorID: p.ExecutorID, Cwd: p.Cwd})
}

// ListModels 按草稿位置或目标会话选目录，外部选项未命中时先给出默认入口。
func (r *runtimeImpl) ListModels(ctx context.Context, request *ListModelsRequest) (ModelList, error) {
	d := r.definitions.Load()
	var list ModelList
	list.Models = make([]*defs.ModelDefinition, 0)
	var record defs.Session
	var err error
	if request.ConversationID != "" {
		id := request.SessionID
		if id == "" {
			c, err := r.GetConversation(ctx, &GetConversationRequest{ConversationID: request.ConversationID})
			if err != nil {
				return list, err
			}
			id = c.RootSessionID
		}
		record, err = r.GetSession(ctx, request.ConversationID, id)
	} else if request.SessionID != "" {
		record, err = r.GetSessionByID(ctx, &GetSessionByIDRequest{SessionID: request.SessionID})
	}
	if err != nil {
		return list, err
	}
	name := cmp.Or(record.AgentName, request.AgentName)
	if name != "" {
		ref, ok := d.agentDefaultModels[name]
		if !ok {
			return list, fmt.Errorf("agent %q is not configured", name)
		}
		list.AgentDefaultModel = ref
	}
	if record.External == nil {
		for _, m := range d.conversationDefinitions.Models {
			list.Models = append(list.Models, m.ClonePublic())
		}
		if record.ID != "" {
			return list, nil
		}
	}
	for _, definition := range d.conversationDefinitions.Agents {
		if !definition.Type.IsExternal() || record.ID != "" && definition.Name != record.AgentName {
			continue
		}
		location := ExternalAgentLocation{AgentName: definition.Name, ExecutorID: request.ExecutorID, Cwd: request.Cwd}
		var opts defs.ExternalOptions
		var found bool
		if record.External != nil {
			location.ExecutorID, location.Cwd = record.External.ExecutorID, record.External.Cwd
			running, err := r.getRunningConversation(ctx, record.ConversationID)
			if err == nil {
				opts, found = running.conversation.GetLiveExternalOptions(record.ID)
			} else if !errors.Is(err, ErrConversationNotOpen) {
				return list, err
			}
		}
		if !found {
			p, err := d.getExternalAgentProbe(ctx, location, record.ConversationID)
			if err == nil {
				opts, found = p.provider.GetCachedOptions(p.ExecutorID, p.Cwd)
				if !found {
					if _, err = r.requestOptionsProbe(ctx, d, p, AgentOptionsRequest{}); err != nil {
						return list, err
					}
				}
			}
		}
		choices := []defs.ExternalOptionChoice{{Value: "default", Label: "default"}}
		if opts.Model != nil && len(opts.Model.Options) > 0 {
			choices = opts.Model.Options
		}
		icon := definition.GetIconKey()
		for _, choice := range choices {
			list.Models = append(list.Models, &defs.ModelDefinition{Ref: defs.ModelRef{ProviderID: definition.Name, ModelID: choice.Value}, DisplayName: choice.Label, Icon: icon})
		}
	}
	return list, nil
}

// ImportSession 用调用方选定的绑定和标题建立新的持久对话。
func (r *runtimeImpl) ImportSession(ctx context.Context, req ImportSessionRequest) (defs.Conversation, error) {
	p, err := r.definitions.Load().getExternalAgentProbe(ctx, req.ExternalAgentLocation, "")
	if err != nil {
		return defs.Conversation{}, err
	}
	if req.ExternalSessionID == "" {
		return defs.Conversation{}, errors.New("external_session_id is required")
	}
	b := defs.ExternalSession{AgentType: p.agentType, ExecutorID: p.ExecutorID, Cwd: p.Cwd, SessionID: req.ExternalSessionID}
	return r.openImportedConversation(ctx, defs.Session{AgentName: p.AgentName, External: &b}, req.Title, defs.ExecutorSettings{DefaultExecutorID: p.ExecutorID, WorkspaceDirectories: map[string]string{p.ExecutorID: p.Cwd}})
}

// openImportedConversation 先显式保存独立根的绑定，再等待恢复。导入与转出共用这条失败清理路径。
func (r *runtimeImpl) openImportedConversation(ctx context.Context, root defs.Session, title string, settings defs.ExecutorSettings) (defs.Conversation, error) {
	c := defs.Conversation{ID: defs.NewConversationID(), RootSessionID: defs.NewSessionID(), Title: defs.TruncateConversationTitle(title), ExecutorSettings: settings}
	root.ID, root.ConversationID = c.RootSessionID, c.ID
	cleanup := context.WithoutCancel(ctx)
	if err := r.mutation.SetSession(ctx, root); err != nil {
		return defs.Conversation{}, errors.Join(err, r.opt.Logs.DeletePath(cleanup, storage.StreamPath(c.ID)))
	}
	if err := r.mutation.SetConversation(ctx, c); err != nil {
		return defs.Conversation{}, errors.Join(err, r.mutation.DeleteConversation(cleanup, c.ID), r.mutation.DeleteSession(cleanup, c.ID, root.ID), r.opt.Logs.DeletePath(cleanup, storage.StreamPath(c.ID)))
	}
	run, err := r.getOrOpenConversation(ctx, &OpenConversationRequest{ConversationID: c.ID})
	if err == nil {
		_, _, err = run.conversation.GetSessionHistoryDAO(ctx, root.ID, true)
		if err == nil {
			var restored defs.Conversation
			restored, err = run.getRecord(ctx)
			if err == nil {
				return restored, nil
			}
		}
		run.cancel()
		<-run.stopped
	}
	return defs.Conversation{}, errors.Join(err, r.mutation.DeleteConversation(cleanup, c.ID))
}

// DetachSession 静默停止旧子会话，独立根恢复成功后才发布永久跳转。
func (r *runtimeImpl) DetachSession(ctx context.Context, cid defs.ConversationID, id defs.SessionID) (defs.Conversation, error) {
	run, err := r.getOrOpenConversation(ctx, &OpenConversationRequest{ConversationID: cid})
	if err != nil {
		return defs.Conversation{}, err
	}
	record, err := run.conversation.PrepareSessionDetach(ctx, id)
	if err != nil {
		return defs.Conversation{}, err
	}
	cleanup := context.WithoutCancel(ctx)
	root := defs.Session{
		AgentName: record.AgentName, External: record.External,
		DetachedFromConversationID: record.ConversationID, DetachedFromSessionID: record.ID,
	}
	target, err := r.openImportedConversation(ctx, root, record.TaskName, run.conversation.GetExecutorSettings())
	updated, finishErr := run.conversation.FinishSessionDetach(cleanup, id, target)
	if finishErr != nil && target.ID != "" {
		finishErr = errors.Join(finishErr, r.DeleteConversation(cleanup, &DeleteConversationRequest{ConversationID: target.ID}))
	}
	if err = errors.Join(err, finishErr); err != nil {
		return defs.Conversation{}, err
	}
	_ = run.sendRequest(cleanup, &sessionRecordReplaceRequested{record: updated})
	return target, nil
}
