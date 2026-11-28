package externalagent

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/docs"
	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
	"github.com/VaalaCat/openvaala/pkg/prompt"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/workspace"
	"go.opentelemetry.io/otel"
)

type fakeProvider struct {
	ExternalAgentProvider
	opened chan ExternalSessionOpenRequest
	driver *fakeDriver
}

func (p *fakeProvider) OpenSession(req ExternalSessionOpenRequest) ExternalAgentSession {
	p.opened <- req
	return p.driver
}

type fakeDelivery struct {
	id    defs.TurnID
	items []*defs.Item
	reply chan error
}

type fakeDriver struct {
	connected      chan chan<- ExternalSessionEvent
	exited         chan struct{}
	crash          chan error
	starts, steers chan fakeDelivery
	interrupts     chan struct{}
	options        chan defs.ExternalOptionSelection
	optionResult   chan error
	// 模拟提示已写入，但调用方直到进程退出才收到成功回执。
	promptWrittenBeforeExit bool
}

func newFakeProvider() *fakeProvider {
	d := &fakeDriver{connected: make(chan chan<- ExternalSessionEvent, 1), exited: make(chan struct{}), crash: make(chan error, 1),
		starts: make(chan fakeDelivery, 1), steers: make(chan fakeDelivery, 1), interrupts: make(chan struct{}, 1), options: make(chan defs.ExternalOptionSelection, 1), optionResult: make(chan error, 1)}
	return &fakeProvider{opened: make(chan ExternalSessionOpenRequest, 4), driver: d}
}

func (d *fakeDriver) Run(ctx context.Context, events chan<- ExternalSessionEvent) error {
	defer close(d.exited)
	d.connected <- events
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-d.crash:
		return err
	}
}

func fakeDeliver(ctx context.Context, calls chan<- fakeDelivery, id defs.TurnID, items []*defs.Item) error {
	reply := make(chan error, 1)
	select {
	case calls <- fakeDelivery{id, items, reply}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *fakeDriver) StartTurn(ctx context.Context, id defs.TurnID, items []*defs.Item) error {
	err := fakeDeliver(ctx, d.starts, id, items)
	if d.promptWrittenBeforeExit && errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func (d *fakeDriver) SteerInput(ctx context.Context, id defs.TurnID, items []*defs.Item) error {
	return fakeDeliver(ctx, d.steers, id, items)
}

func (d *fakeDriver) InterruptTurn(ctx context.Context) error {
	d.interrupts <- struct{}{}
	return nil
}
func (d *fakeDriver) SetOption(ctx context.Context, sel defs.ExternalOptionSelection) error {
	d.options <- sel
	select {
	case err := <-d.optionResult:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

type externalHarness struct {
	ExternalSession
	record   defs.Session
	provider *fakeProvider
	events   chan session.SessionEvent
	updates  chan<- ExternalSessionEvent
	done     chan error
	cancel   context.CancelFunc
	logs     *bytes.Buffer
}

func newHarness(t *testing.T, record defs.Session, summary string, ws workspace.Workspace) *externalHarness {
	t.Helper()
	if ws == nil {
		var err error
		ws, err = workspace.NewLocalWorkspace(workspace.NewLocalWorkspaceOpt(t.TempDir()))
		if err != nil {
			t.Fatal(err)
		}
	}
	p := newFakeProvider()
	opt := &ExternalSessionOpt{AgentType: defs.AgentTypeACP, Session: record, ForkSummary: summary,
		Launch: ExternalSessionLaunch{Provider: p, Workspace: ws, SkillWorkspace: ws, InitialOptions: []defs.ExternalOptionSelection{{Kind: "mode", Value: "initial"}}}}
	s, err := NewExternalSession(opt)
	if err != nil {
		t.Fatal(err)
	}
	logs := new(bytes.Buffer)
	pipe, err := telemetry.NewTelemetry(telemetry.NewTelemetryOpt(otel.GetTracerProvider(), otel.GetMeterProvider(), slog.New(slog.NewTextHandler(logs, nil))))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(telemetry.ContextWithTelemetry(t.Context(), pipe))
	h := &externalHarness{ExternalSession: s, record: record, provider: p, events: make(chan session.SessionEvent, 256), done: make(chan error, 1), cancel: cancel, logs: logs}
	go func() {
		h.done <- s.Run(ctx, h.events)
		close(h.done)
	}()
	t.Cleanup(func() {
		cancel()
		for range h.events {
		}
		recv(t, h.done)
	})
	h.updates = recv(t, p.driver.connected)
	return h
}

func recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(9 * time.Second):
		t.Fatal("timed out waiting for event")
		var zero T
		return zero
	}
}

func send[T any](t *testing.T, ch chan<- T, value T) {
	t.Helper()
	select {
	case ch <- value:
	case <-time.After(9 * time.Second):
		t.Fatal("timed out sending event")
	}
}

func nextEvent[T session.SessionEvent](t *testing.T, h *externalHarness) T {
	t.Helper()
	for {
		event := recv(t, h.events)
		if e, ok := event.(T); ok {
			return e
		}
		if event == nil {
			t.Fatal("session stopped")
		}
	}
}

func newRecord() defs.Session {
	return defs.Session{ID: defs.NewSessionID(), ConversationID: defs.NewConversationID(), AgentName: "external"}
}

func message(from defs.IdentityKind, text string) *defs.Item {
	return &defs.Item{ID: defs.NewItemID(), Kind: defs.ItemMessage, Message: &defs.Message{From: &defs.Address{Kind: from}, Content: []*defs.Content{{Kind: defs.ContentText, Text: &defs.TextContent{Text: text}}}}}
}

func ready() *ExternalSessionReadyEvent {
	return &ExternalSessionReadyEvent{Binding: defs.ExternalSession{AgentType: defs.AgentTypeACP, SessionID: "remote", Cwd: "/project", ExecutorID: "ssh"}, MCPServers: []string{"actual"}}
}

func stream(id defs.TurnID, index int, item *defs.Item) *ExternalAgentStreamEvent {
	return &ExternalAgentStreamEvent{Replaying: id == "", TurnID: id, Event: &agent.AgentEvent{Type: agent.AgentEventItemCompleted, ItemIndex: index, Item: item}}
}

func (h *externalHarness) history(t *testing.T) []dao.LoggedItem {
	t.Helper()
	q, restoring := h.GetHistoryDAO()
	if q == nil || restoring {
		t.Fatal("history unavailable")
	}
	page, err := q.ListItems(t.Context(), dao.ListItemsRequest{ConversationID: h.record.ConversationID, SessionID: h.record.ID, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return page.Items
}

func (h *externalHarness) input(t *testing.T, text string) defs.ItemID {
	t.Helper()
	id, err := h.HandleInput(t.Context(), &session.SessionInput{Item: message(defs.IdentityUser, text), Mode: session.SessionInputModeNormal})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func awaitRestore(t *testing.T, h *externalHarness) *session.SessionHistoryRestored {
	t.Helper()
	for {
		switch e := recv(t, h.events).(type) {
		case *session.SessionHistoryRestored:
			return e
		case *session.SessionAgentEvent, *session.SessionTurnStarted, *session.SessionItemsSaved, *session.SessionTurnFinished, nil:
			t.Fatalf("unexpected replay event %T", e)
		}
	}
}

func TestExternalSessionReplay(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		child, bound, summary, partial bool
	}{
		{"ordinary root", false, false, false, false}, {"fork root", false, false, true, false}, {"imported root", false, true, false, false}, {"detached new root", false, true, true, false},
		{"child", true, false, false, false}, {"child with summary", true, false, true, false}, {"reopen after partial replay", true, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := newRecord()
			if tc.child {
				record.ParentSessionID = "parent"
				record.ParentTurnID = "source"
			}
			if tc.bound {
				record.External = &ready().Binding
			}
			var summary string
			if tc.summary {
				summary = "summary"
			}
			if tc.partial {
				h := newHarness(t, record, summary, nil)
				send(t, h.updates, ExternalSessionEvent(stream("", 0, message(defs.IdentityUser, "partial"))))
				send(t, h.updates, ExternalSessionEvent(stream("", 1, message(defs.IdentityAgent, "answer"))))
				h.input(t, "queued before failed restore")
				h.provider.driver.crash <- errors.New("load failed")
				event := awaitRestore(t, h)
				if event.Err == nil || event.Err.Error() != "external session cannot be restored: load failed" || event.LastSavedOrdinal != 0 {
					t.Fatalf("restore failure %+v", event)
				}
				if err := recv(t, h.done); err == nil {
					t.Fatal("failed restore succeeded")
				}
				if q, _ := h.GetHistoryDAO(); q != nil {
					t.Fatal("partial history published")
				}
			}
			h := newHarness(t, record, summary, nil)
			if _, restoring := h.GetHistoryDAO(); !restoring {
				t.Fatal("missing restoring flag")
			}
			send(t, h.updates, ExternalSessionEvent(&ExternalAgentStreamEvent{Replaying: true, Event: &agent.AgentEvent{Type: agent.AgentEventItemStarted, Item: message(defs.IdentityUser, "")}}))
			for _, part := range []string{"chunk", " two"} {
				send(t, h.updates, ExternalSessionEvent(&ExternalAgentStreamEvent{Replaying: true, Event: &agent.AgentEvent{Type: agent.AgentEventItemDelta, Delta: &agent.AgentItemDelta{Text: part}}}))
			}
			send(t, h.updates, ExternalSessionEvent(stream("", 0, nil)))
			send(t, h.updates, ExternalSessionEvent(stream("", 1, message(defs.IdentityAgent, "answer"))))
			send(t, h.updates, ExternalSessionEvent(stream("", 0, message(defs.IdentityUser, "again"))))
			send(t, h.updates, ExternalSessionEvent(ready()))
			event := awaitRestore(t, h)
			rows := h.history(t)
			if event.Err != nil || event.LastSavedOrdinal != int64(len(rows)) {
				t.Fatalf("restore %+v, rows %d", event, len(rows))
			}
			if tc.child {
				fork := rows[0].Item
				if fork.Kind != defs.ItemSessionFork || fork.TurnID != "" || fork.ModelCallID != "" || *fork.SessionFork != (defs.SessionFork{SourceSessionID: "parent"}) {
					t.Fatalf("fork %+v", fork)
				}
				rows = rows[1:]
			}
			if len(rows) != 3 || rows[0].Item.Message.Content[0].Text.Text != "chunk two" || rows[0].Item.TurnID != rows[1].Item.TurnID || rows[1].Item.TurnID == rows[2].Item.TurnID {
				t.Fatalf("history %+v", rows)
			}
			if tc.bound {
				h.input(t, "continue")
				delivery := recv(t, h.provider.driver.starts)
				body := delivery.items[0].Message.Content
				if body[len(body)-1].Text.Text != prompt.FormatSessionInfo(record) {
					t.Fatal("replay consumed new session identity")
				}
				delivery.reply <- nil
			}
		})
	}
}

func TestExternalSessionInput(t *testing.T) {
	for _, outcome := range []string{"success", "send error", "process exit", "exit after write", "reopen fails"} {
		t.Run(outcome, func(t *testing.T) {
			summary := "<openvaala-chat-history-summary>\n<hint>以下是先前对话的历史摘要，并非新的用户输入。</hint>\n[User] earlier\n</openvaala-chat-history-summary>"
			h := newHarness(t, newRecord(), summary, nil)
			exits := outcome == "process exit" || outcome == "exit after write" || outcome == "reopen fails"
			h.provider.driver.promptWrittenBeforeExit = outcome == "exit after write"
			var ids []defs.ItemID
			file := &defs.Content{Kind: defs.ContentFile, File: &defs.FileRef{Name: "paper.pdf", Path: "/data/paper.pdf", MIME: "application/pdf", Size: 3}}
			first := message(defs.IdentityUser, "one")
			first.Message.Content = append(first.Message.Content, file)
			firstID, err := h.HandleInput(t.Context(), &session.SessionInput{Mode: session.SessionInputModeNormal, Item: first})
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, firstID)
			literalInfo := prompt.FormatSessionInfo(defs.Session{ID: "user-supplied"})
			for _, s := range []string{"two", literalInfo} {
				ids = append(ids, h.input(t, s))
			}
			send(t, h.updates, ExternalSessionEvent(ready()))
			start := recv(t, h.provider.driver.starts)
			var sentIDs []defs.ItemID
			for _, i := range start.items {
				sentIDs = append(sentIDs, i.ID)
			}
			if !slices.Equal(ids, sentIDs) {
				t.Fatalf("batch %v", sentIDs)
			}
			var parts []string
			for _, it := range start.items {
				for _, c := range it.Message.Content {
					if c.File != nil {
						parts = append(parts, prompt.FormatAttachment(*c.File))
					} else {
						parts = append(parts, c.Text.Text)
					}
				}
			}
			assembled := strings.Join(parts, "\n\n")
			wantParts := []string{summary, "one", "two", literalInfo, prompt.FormatAttachment(*file.File), prompt.FormatSessionInfo(h.record)}
			if assembled != strings.Join(wantParts, "\n\n") {
				t.Fatalf("first prompt order: %q", assembled)
			}
			body, files := prompt.ParseAttachments(prompt.StripSessionInfo(prompt.StripChatHistorySummary(assembled)))
			if body != "one\n\ntwo\n\n"+literalInfo || !slices.Equal(files, []defs.FileRef{*file.File}) {
				t.Fatalf("replay %q, %+v", body, files)
			}
			q, _ := h.GetHistoryDAO()
			cp, err := q.GetSessionCheckpoint(t.Context(), h.record.ConversationID, h.record.ID)
			if err != nil || cp == nil || cp.TurnInputs == nil || cp.TurnInputs.TurnID != start.id {
				t.Fatalf("pending %+v %v", cp, err)
			}
			want := defs.TurnStatusFailed
			switch outcome {
			case "success":
				want = defs.TurnStatusCompleted
				send(t, h.updates, ExternalSessionEvent(&ExternalTurnFinishedEvent{TurnID: start.id, Status: want}))
				status, err := h.GetStatus(t.Context())
				if err != nil || status.ActiveTurnID != start.id {
					t.Fatalf("early finish %+v %v", status, err)
				}
				start.reply <- nil
			case "send error":
				start.reply <- errors.New("prompt failed (code -1)")
			case "process exit", "exit after write", "reopen fails":
				h.provider.driver.crash <- errors.New("pipe closed")
			}
			finished := nextEvent[*session.SessionTurnFinished](t, h)
			if !slices.Equal(finished.InputItemIDs, ids) || finished.Status != want || len(h.history(t)) != 3 {
				t.Fatalf("finished %+v", finished)
			}
			cp, err = q.GetSessionCheckpoint(t.Context(), h.record.ConversationID, h.record.ID)
			if err != nil || cp != nil {
				t.Fatalf("settled %+v %v", cp, err)
			}
			if exits {
				record, _, err := q.GetTurn(t.Context(), h.record.ID, start.id)
				if err != nil || record.Error != "external agent process exited: pipe closed" {
					t.Fatalf("turn %+v %v", record, err)
				}
				h.provider.driver = newFakeProvider().driver
			}
			nextID := h.input(t, "next")
			if exits {
				recv(t, h.provider.opened)
				open := recv(t, h.provider.opened)
				if open.Binding == nil || open.Binding.SessionID != "remote" || open.InitialOptions != nil {
					t.Fatalf("reopen %+v", open)
				}
				h.updates = recv(t, h.provider.driver.connected)
				if outcome == "reopen fails" {
					h.provider.driver.crash <- errors.New("history missing")
					end := nextEvent[*session.SessionTurnFinished](t, h)
					if end.Err == nil || end.Err.Error() != "external session cannot be restored: history missing" || !slices.Equal(end.InputItemIDs, []defs.ItemID{nextID}) || end.Status != defs.TurnStatusFailed {
						t.Fatalf("restore failure %+v", end)
					}
					if _, err := h.HandleInput(t.Context(), &session.SessionInput{Item: message(defs.IdentityUser, "new"), Mode: session.SessionInputModeNormal}); err == nil || err.Error() != end.Err.Error() {
						t.Fatalf("enqueue after restore failure: %v", err)
					}
					if after, _ := h.GetHistoryDAO(); after != q {
						t.Fatal("lost complete history")
					}
					return
				}
				send(t, h.updates, ExternalSessionEvent(stream("", 0, message(defs.IdentityUser, "old"))))
				send(t, h.updates, ExternalSessionEvent(ready()))
				if after, _ := h.GetHistoryDAO(); after != q {
					t.Fatal("crash replaced DAO")
				}
			}
			next := recv(t, h.provider.driver.starts)
			if len(next.items) != 1 || next.items[0].ID != nextID {
				t.Fatalf("redelivered input %+v", next)
			}
			var retryParts []string
			for _, c := range next.items[0].Message.Content {
				retryParts = append(retryParts, c.Text.Text)
			}
			expected := []string{"next"}
			if outcome != "success" && outcome != "exit after write" {
				expected = []string{summary, "next", prompt.FormatSessionInfo(h.record)}
			}
			if !slices.Equal(retryParts, expected) {
				t.Fatalf("next prompt %q, want %q", retryParts, expected)
			}

			next.reply <- nil
		})
	}
}

func TestExternalSessionAppendContextItems(t *testing.T) {
	h := newHarness(t, newRecord(), "", nil)
	send(t, h.updates, ExternalSessionEvent(ready()))
	nextEvent[*session.SessionHistoryRestored](t, h)
	note := &defs.Item{
		ID: defs.NewItemID(), Kind: defs.ItemClientInstruction,
		ClientInstruction: &defs.ClientInstruction{Text: "side instruction"},
	}
	if err := h.AppendContextItems(t.Context(), []*defs.Item{note}); err != nil {
		t.Fatal(err)
	}
	status, err := h.GetStatus(t.Context())
	if err != nil || status.PendingContextItemCount != 1 || status.ActiveTurnID != "" || status.PendingInputCount != 0 {
		t.Fatalf("idle status: %+v, %v", status, err)
	}
	noteText := prompt.FormatClientInstruction(*note.ClientInstruction)
	note.ClientInstruction.Text = "caller mutated this item"
	for _, tc := range []struct {
		input            string
		want             []string
		appendDuringTurn bool
	}{
		{"question", []string{noteText, "question", prompt.FormatSessionInfo(h.record)}, true},
		{"follow up", []string{"while sending", "follow up"}, false},
		{"last", []string{"last"}, false},
	} {
		id := h.input(t, tc.input)
		start := recv(t, h.provider.driver.starts)
		status, err := h.GetStatus(t.Context())
		if err != nil || status.ActiveTurnID != start.id || status.PendingContextItemCount != 0 {
			t.Fatalf("before StartTurn acknowledgement: %+v, %v", status, err)
		}
		var body []string
		for _, item := range start.items {
			for _, part := range item.Message.Content {
				body = append(body, part.Text.Text)
			}
		}
		if !slices.Equal(body, tc.want) {
			t.Fatalf("prompt = %q, want %q", body, tc.want)
		}
		if tc.appendDuringTurn {
			rows := h.history(t)
			if len(rows) != 2 || rows[0].Item.ID != note.ID || rows[1].Item.ID != id {
				t.Fatalf("opening history = %+v", rows)
			}
			if err := h.AppendContextItems(t.Context(), []*defs.Item{message(defs.IdentityUser, "while sending")}); err != nil {
				t.Fatal(err)
			}
		}
		start.reply <- nil
		send(t, h.updates, ExternalSessionEvent(&ExternalTurnFinishedEvent{TurnID: start.id, Status: defs.TurnStatusCompleted}))
		if done := nextEvent[*session.SessionTurnFinished](t, h); !slices.Equal(done.InputItemIDs, []defs.ItemID{id}) {
			t.Fatalf("turn input IDs = %v", done.InputItemIDs)
		}
	}
}

func TestExternalSessionLifecycle(t *testing.T) {
	for _, name := range []string{"groups", "no final text", "steer fallback", "interrupt timeout", "idle reload", "running reload", "retry failed reload", "option before response", "unsupported", "unread agent message", "steer delivered", "start fails before steer", "tool updates by call id", "shutdown during turn", "duplicate run", "crash drafts", "timeout drafts"} {
		t.Run(name, func(t *testing.T) {
			next := newFakeProvider()
			h := newHarness(t, newRecord(), "", nil)
			d := h.provider.driver
			readyEvent := ready()
			readyEvent.Capabilities.SupportsSteer = name == "steer delivered" || name == "start fails before steer"
			send(t, h.updates, ExternalSessionEvent(readyEvent))
			binding := nextEvent[*session.SessionExternalBindingUpdated](t, h)
			state := nextEvent[*session.SessionExternalStateUpdated](t, h)
			if binding.Binding.SessionID != "remote" || !slices.Equal(state.MCPServers, []string{"actual"}) {
				t.Fatalf("ready %+v %+v", binding, state)
			}
			awaitRestore(t, h)
			late := ready().Binding
			late.SessionID = "late"
			send(t, h.updates, ExternalSessionEvent(&ExternalBindingUpdatedEvent{Binding: late}))
			if nextEvent[*session.SessionExternalBindingUpdated](t, h).Binding.SessionID != "late" {
				t.Fatal("lost late binding")
			}
			if name == "duplicate run" {
				if err := h.Run(t.Context(), h.events); err == nil {
					t.Fatal("second Run accepted")
				}
				send(t, h.updates, ExternalSessionEvent(&ExternalCommandsUpdatedEvent{Commands: []defs.ExternalCommand{{Name: "still-running"}}}))
				if nextEvent[*session.SessionExternalStateUpdated](t, h).Commands[0].Name != "still-running" {
					t.Fatal("missing command update")
				}
				return
			}
			if name == "option before response" {
				result := make(chan error, 1)
				go func() { result <- h.SetOption(t.Context(), defs.ExternalOptionSelection{Kind: "mode", Value: "new"}) }()
				sel := recv(t, d.options)
				send(t, h.updates, ExternalSessionEvent(&ExternalOptionsUpdatedEvent{Options: defs.ExternalOptions{Mode: &defs.ExternalOptionGroup{CurrentValue: sel.Value}}}))
				if nextEvent[*session.SessionExternalStateUpdated](t, h).Options.Mode.CurrentValue != "new" {
					t.Fatal("option lost")
				}
				d.optionResult <- nil
				if err := recv(t, result); err != nil {
					t.Fatal(err)
				}
				go func() { result <- h.SetOption(t.Context(), sel) }()
				recv(t, d.options)
				h.cancel()
				recv(t, h.done)
				if err := recv(t, result); err == nil {
					t.Fatal("cancelled option succeeded")
				}
				return
			}
			if name == "unsupported" {
				_, compactErr := h.CompactContext(t.Context())
				for _, err := range []error{compactErr, h.SelectModel(t.Context(), defs.ModelSelection{})} {
					if !errors.Is(err, ErrExternalUnsupported) || err.Error() != "operation is not supported for acp sessions" {
						t.Fatalf("unsupported: %v", err)
					}
				}
				return
			}
			var start fakeDelivery
			if name != "idle reload" && name != "retry failed reload" {
				h.input(t, "go")
				start = recv(t, d.starts)
				if name != "start fails before steer" {
					start.reply <- nil
				}
			}
			switch name {
			case "crash drafts", "timeout drafts":
				drafts := []*defs.Item{{Kind: defs.ItemReasoning, Reasoning: &defs.Reasoning{}}, {Kind: defs.ItemToolCall, ToolCall: &defs.ToolCall{CallID: "pending", Name: "read"}}}
				for i, item := range drafts {
					send(t, h.updates, ExternalSessionEvent(&ExternalAgentStreamEvent{TurnID: start.id, Event: &agent.AgentEvent{Type: agent.AgentEventItemStarted, ItemIndex: i, Item: item}}))
				}
				if name == "crash drafts" {
					d.crash <- errors.New("disconnect")
				} else if _, err := h.InterruptTurn(t.Context()); err != nil {
					t.Fatal(err)
				}
				for i := 0; i < 2; {
					event := nextEvent[*session.SessionAgentEvent](t, h).Event
					if event.Type != agent.AgentEventItemInterrupted {
						continue
					}
					if i == 0 && event.Item.Reasoning.Status != defs.ReasoningInterrupted || i == 1 && event.Item.ToolCall.Status != defs.ToolCallInterrupted {
						t.Fatal("live draft status", event.Item)
					}
					i++
				}
				nextEvent[*session.SessionTurnFinished](t, h)
				rows := h.history(t)
				if rows[1].Item.Reasoning.Status != defs.ReasoningInterrupted || rows[2].Item.ToolCall.Status != defs.ToolCallInterrupted {
					t.Fatal("saved draft status", rows)
				}
			case "tool updates by call id":
				send(t, h.updates, ExternalSessionEvent(&ExternalAgentStreamEvent{TurnID: start.id, Event: &agent.AgentEvent{Type: agent.AgentEventItemStarted, ItemIndex: 4, Item: &defs.Item{Kind: defs.ItemToolCall, ToolCall: &defs.ToolCall{CallID: "tool1", Name: "read"}}}}))
				send(t, h.updates, ExternalSessionEvent(stream(start.id, 5, message(defs.IdentityAgent, "answer"))))
				send(t, h.updates, ExternalSessionEvent(stream(start.id, 6, &defs.Item{Kind: defs.ItemToolCall, ToolCall: &defs.ToolCall{CallID: "tool1", Name: "read", Arguments: "{}"}})))
				send(t, h.updates, ExternalSessionEvent(&ExternalTurnFinishedEvent{TurnID: start.id, Status: defs.TurnStatusCompleted}))
				end := nextEvent[*session.SessionTurnFinished](t, h)
				if len(end.Outputs) != 2 || end.Outputs[0].ToolCall.Arguments != "{}" || end.Outputs[1].Message == nil || end.FinalModelCallID != end.Outputs[1].ModelCallID {
					t.Fatalf("tool updates %+v", end)
				}
				rows := h.history(t)
				if len(rows) != 3 || rows[1].Item.ToolCall == nil || rows[2].Item.Message == nil {
					t.Fatalf("out of order %v", rows)
				}
			case "unread agent message":
				got := make(chan []*defs.Item, 1)
				go func() {
					items, _ := h.WaitSteer(t.Context())
					got <- items
				}()
				queued := message(defs.IdentityAgent, "report")
				_, err := h.HandleInput(t.Context(), &session.SessionInput{Mode: session.SessionInputModeSteer, Item: queued})
				if err != nil {
					t.Fatal(err)
				}
				unread := recv(t, got)
				if len(unread) != 1 || unread[0].ID != queued.ID {
					t.Fatalf("unread %+v", unread)
				}
				select {
				case <-d.interrupts:
					t.Fatal("agent message interrupted")
				default:
				}
				send(t, h.updates, ExternalSessionEvent(&ExternalTurnFinishedEvent{TurnID: start.id, Status: defs.TurnStatusCompleted}))
				second := recv(t, d.starts)
				if len(second.items) != 1 || second.items[0].ID != queued.ID {
					t.Fatal(second.items)
				}
				second.reply <- nil
			case "start fails before steer", "steer delivered":
				item := message(defs.IdentityUser, "extra")
				id, err := h.HandleInput(t.Context(), &session.SessionInput{Mode: session.SessionInputModeSteer, Item: item})
				if err != nil {
					t.Fatal(err)
				}
				if name == "start fails before steer" {
					start.reply <- errors.New("send failed")
					end := nextEvent[*session.SessionTurnFinished](t, h)
					if len(end.InputItemIDs) != 1 {
						t.Fatal(end.InputItemIDs)
					}
					second := recv(t, d.starts)
					if len(second.items) != 1 || second.items[0].ID != id {
						t.Fatal(second.items)
					}
					second.reply <- nil
				} else {
					steer := recv(t, d.steers)
					send(t, h.updates, ExternalSessionEvent(&ExternalTurnFinishedEvent{TurnID: start.id, Status: defs.TurnStatusCompleted}))
					steer.reply <- nil
					end := nextEvent[*session.SessionTurnFinished](t, h)
					if len(end.InputItemIDs) != 2 || end.InputItemIDs[1] != id {
						t.Fatal(end.InputItemIDs)
					}
				}
			case "shutdown during turn":
				h.cancel()
				end := nextEvent[*session.SessionTurnFinished](t, h)
				if end.TurnID != start.id || end.Status != defs.TurnStatusInterrupted {
					t.Fatalf("shutdown %+v", end)
				}
			case "groups", "no final text":
				send(t, h.updates, ExternalSessionEvent(stream(start.id, 0, message(defs.IdentityAgent, "working"))))
				send(t, h.updates, ExternalSessionEvent(stream(start.id, 1, &defs.Item{Kind: defs.ItemToolCall, ToolCall: &defs.ToolCall{CallID: "call", Name: "tool", Arguments: "{}"}})))
				send(t, h.updates, ExternalSessionEvent(stream(start.id, 2, &defs.Item{Kind: defs.ItemToolResult, ToolResult: &defs.ToolResult{CallID: "call"}})))
				var usage *defs.TokenUsage
				if name == "groups" {
					send(t, h.updates, ExternalSessionEvent(stream(start.id, 3, message(defs.IdentityAgent, "answer"))))
					usage = &defs.TokenUsage{Input: 123}
				}
				send(t, h.updates, ExternalSessionEvent(&ExternalTurnFinishedEvent{TurnID: start.id, Status: defs.TurnStatusCompleted, Usage: usage}))
				end := nextEvent[*session.SessionTurnFinished](t, h)
				q, _ := h.GetHistoryDAO()
				turn, _, err := q.GetTurn(t.Context(), h.record.ID, start.id)
				if err != nil || len(end.Outputs) < 3 || turn.FinalModelCallID != end.FinalModelCallID {
					t.Fatalf("end %+v %+v %v", end, turn, err)
				}
				if name == "groups" {
					if end.FinalModelCallID == "" || end.FinalModelCallID != end.Outputs[3].ModelCallID || end.FinalModelCallID == end.Outputs[0].ModelCallID || turn.Usage != *usage {
						t.Fatalf("grouping %+v", turn)
					}
				} else if end.FinalModelCallID != "" || turn.Usage != (defs.TokenUsage{}) {
					t.Fatalf("last tool %+v", turn)
				}
			case "steer fallback", "interrupt timeout":
				if name == "steer fallback" {
					id := h.input(t, "instead")
					if err := h.SteerInput(t.Context(), id, start.id); err != nil {
						t.Fatal(err)
					}
				} else if id, err := h.InterruptTurn(t.Context()); err != nil || id != start.id {
					t.Fatalf("interrupt %s %v", id, err)
				}
				recv(t, d.interrupts)
				since := time.Now()
				if name == "steer fallback" {
					send(t, h.updates, ExternalSessionEvent(&ExternalTurnFinishedEvent{TurnID: start.id, Status: defs.TurnStatusInterrupted}))
				}
				end := nextEvent[*session.SessionTurnFinished](t, h)
				rows := h.history(t)
				if end.Status != defs.TurnStatusInterrupted || len(rows) < 2 || rows[1].Item.Kind != defs.ItemTurnInterrupted {
					t.Fatalf("interrupted %+v", end)
				}
				if name == "steer fallback" {
					second := recv(t, d.starts)
					if len(second.items) != 1 || second.items[0].Message.Content[0].Text.Text != "instead" {
						t.Fatalf("next %+v", second)
					}
					second.reply <- nil
				} else {
					q, _ := h.GetHistoryDAO()
					checkpoint, err := q.GetSessionCheckpoint(t.Context(), h.record.ConversationID, h.record.ID)
					if err != nil || checkpoint == nil || !checkpoint.IsAgentMessagesPaused || checkpoint.StopRequestedTurnID != "" {
						t.Fatalf("root pause checkpoint %+v: %v", checkpoint, err)
					}

					recv(t, d.exited)
					if time.Since(since) < 4500*time.Millisecond {
						t.Fatal("early timeout")
					}
				}
			case "idle reload", "running reload", "retry failed reload":
				q, _ := h.GetHistoryDAO()
				result := make(chan error, 1)
				ws, err := workspace.NewLocalWorkspace(workspace.NewLocalWorkspaceOpt(t.TempDir()))
				if err != nil {
					t.Fatal(err)
				}
				launch := ExternalSessionLaunch{
					Provider: next, Workspace: ws, SkillWorkspace: ws,
					ExecutorID: "creation-only", Cwd: "/creation-only", InitialOptions: []defs.ExternalOptionSelection{{Kind: "mode", Value: "initial"}},
					LaunchEnv: map[string]string{"SETTING": "latest"}, LocalMCPURL: "http://test.invalid/new",
					MCPServers: []*mcpclient.ServerDefinition{{ID: "latest"}}, AttachmentDirectory: "/latest/attachments",
				}
				go func() {
					result <- h.Reload(t.Context(), launch)
				}()
				if name == "running reload" {
					recv(t, d.interrupts)
					send(t, h.updates, ExternalSessionEvent(&ExternalTurnFinishedEvent{TurnID: start.id, Status: defs.TurnStatusInterrupted}))
				}
				open := recv(t, next.opened)
				select {
				case <-d.exited:
				default:
					t.Fatal("old process still running")
				}
				if open.Binding == nil || open.Binding.SessionID != late.SessionID || open.ExecutorID != late.ExecutorID || open.Cwd != late.Cwd || len(open.InitialOptions) > 0 || open.Workspace != ws || open.LaunchEnv["SETTING"] != "latest" || open.LocalMCPURL != launch.LocalMCPURL || open.AttachmentDirectory != launch.AttachmentDirectory || len(open.MCPServers) != 1 || open.MCPServers[0].ID != "latest" {
					t.Fatalf("reload request %+v", open)
				}
				updates := recv(t, next.driver.connected)
				if name == "retry failed reload" {
					badConfig := errors.New("invalid agent configuration")
					next.driver.crash <- badConfig
					if err := recv(t, result); !errors.Is(err, badConfig) {
						t.Fatalf("bad reload = %v", err)
					}
					_, err := h.HandleInput(t.Context(), &session.SessionInput{Item: message(defs.IdentityUser, "blocked"), Mode: session.SessionInputModeNormal})
					if !errors.Is(err, badConfig) {
						t.Fatalf("input after failed reload = %v", err)
					}
					next = newFakeProvider()
					launch.Provider = next
					go func() { result <- h.Reload(t.Context(), launch) }()
					if open := recv(t, next.opened); open.Binding == nil || open.Binding.SessionID != late.SessionID {
						t.Fatalf("retry binding = %+v", open.Binding)
					}
					updates = recv(t, next.driver.connected)
				}
				before := h.history(t)
				send(t, updates, ExternalSessionEvent(stream("", 0, message(defs.IdentityUser, "old"))))
				send(t, updates, ExternalSessionEvent(ready()))
				if err := recv(t, result); err != nil {
					t.Fatal(err)
				}
				after, _ := h.GetHistoryDAO()
				if q != after || len(h.history(t)) != len(before) {
					t.Fatal("reload changed timeline")
				}
				h.input(t, "next")
				second := recv(t, next.driver.starts)
				second.reply <- nil
				rows := h.history(t)
				if int(rows[len(rows)-1].Ordinal) != len(before)+1 {
					t.Fatal("ordinal jumped")
				}
			}
		})
	}
}

type skillWorkspace struct {
	workspace.Workspace
	writes []string
	fail   bool
}

func (w *skillWorkspace) WriteFile(ctx context.Context, path string, data []byte, mode fs.FileMode, createOnly bool) error {
	w.writes = append(w.writes, path)
	if w.fail {
		return fs.ErrPermission
	}
	return w.Workspace.WriteFile(ctx, path, data, mode, createOnly)
}

func TestExternalSkillSync(t *testing.T) {
	for _, mode := range []string{"new", "identical", "write error"} {
		t.Run(mode, func(t *testing.T) {
			ws, err := workspace.NewLocalWorkspace(workspace.NewLocalWorkspaceOpt(t.TempDir()))
			if err != nil {
				t.Fatal(err)
			}
			stub := &skillWorkspace{Workspace: ws, fail: mode == "write error"}
			paths := []string{}
			for _, prefix := range []string{".claude", ".agents"} {
				path := filepath.Join(ws.GetRootDirectory(), prefix, "skills", "openvaala", "SKILL.md")
				paths = append(paths, path)
				if mode == "identical" {
					if err := ws.CreateDirectory(t.Context(), filepath.Dir(path)); err != nil {
						t.Fatal(err)
					}
					if err := ws.WriteFile(t.Context(), path, []byte(docs.ExternalAgentSkill), 0644, false); err != nil {
						t.Fatal(err)
					}
				}
			}
			h := newHarness(t, newRecord(), "", stub)
			if mode == "identical" {
				if len(stub.writes) != 0 {
					t.Fatal(stub.writes)
				}
			} else if !slices.Equal(stub.writes, paths) {
				t.Fatalf("writes before Run %v", stub.writes)
			}
			if mode == "write error" && !strings.Contains(h.logs.String(), "WARN") {
				t.Fatalf("missing warning: %s", h.logs.String())
			}
			if mode == "new" {
				for _, path := range paths {
					content, err := ws.ReadFile(t.Context(), path, 0, len(docs.ExternalAgentSkill)+1)
					if err != nil || string(content.Content) != docs.ExternalAgentSkill {
						t.Fatalf("skill %s %v", path, err)
					}
				}
			}
			send(t, h.updates, ExternalSessionEvent(ready()))
			awaitRestore(t, h)
		})
	}
}
