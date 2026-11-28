package session_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/session"
)

func TestSessionInboxNativeHandoff(t *testing.T) {
	for _, test := range []struct {
		name    string
		senders [][]defs.IdentityKind
	}{
		{name: "separate_user_turns", senders: [][]defs.IdentityKind{{defs.IdentityUser}, {defs.IdentityUser}}},
		{name: "agent_batch", senders: [][]defs.IdentityKind{{defs.IdentityAgent, defs.IdentityAgent, defs.IdentityAgent}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			query, mutation := openSessionDAO(t)
			inbox := newSessionInbox(t, query, mutation)
			var batches [][]defs.ItemID
			for _, senders := range test.senders {
				var ids []defs.ItemID
				for _, sender := range senders {
					item := sessionInput("input")
					item.Message.From.Kind = sender
					ids = append(ids, addInboxInput(t, inbox, item, ""))
				}
				batches = append(batches, ids)
			}

			for _, ids := range batches {
				turnID := defs.NewTurnID()
				opening, _, err := inbox.TakeTurnInput(t.Context(), turnID)
				if err != nil || len(opening) != 1 || opening[0].Items[0].ID != ids[0] {
					t.Fatalf("TakeTurnInput() = %+v, %v; want opening %s", opening, err, ids[0])
				}
				if got := inboxItemIDs(inbox.GetUnreadSteer()); !slices.Equal(got, ids[1:]) {
					t.Fatalf("unread = %v, want %v", got, ids[1:])
				}
				if err := inbox.ConfirmSteer(t.Context(), turnID, ids[:1]); err != nil {
					t.Fatal(err)
				}
				if len(ids) > 1 {
					borrowed := inbox.TakeSteer(turnID)
					if len(borrowed) != 2 || borrowed[0][0].ID != ids[1] || borrowed[1][0].ID != ids[2] || len(inbox.GetUnreadSteer()) != 0 {
						t.Fatalf("TakeSteer() = %+v, want borrowed %v", borrowed, ids[1:])
					}
					if err := inbox.ConfirmSteer(t.Context(), turnID, ids[1:2]); err != nil {
						t.Fatal(err)
					}
				}
				if err := inbox.ReturnUnconfirmed(t.Context(), turnID); err != nil {
					t.Fatal(err)
				}
				if len(ids) > 1 {
					turnID := defs.NewTurnID()
					batch, err := inbox.TakeWholeBatch(t.Context(), turnID)
					if err != nil || !slices.Equal(inboxBatchIDs(batch), ids[2:]) {
						t.Fatalf("requeued batch = %v, %v; want %v", inboxBatchIDs(batch), err, ids[2:])
					}
				}
			}
		})
	}
}

func TestSessionInboxExternalHandoff(t *testing.T) {
	for _, test := range []struct {
		name        string
		startFailed bool
		senders     []defs.IdentityKind
	}{
		{name: "confirm_started_batch", senders: []defs.IdentityKind{defs.IdentityAgent, defs.IdentityAgent}},
		{name: "clear_failed_batch", startFailed: true, senders: []defs.IdentityKind{defs.IdentityAgent, defs.IdentityAgent}},
		{name: "three_queued_user_inputs", senders: []defs.IdentityKind{defs.IdentityUser, defs.IdentityUser, defs.IdentityUser}},
		{name: "queued_users_and_agents", senders: []defs.IdentityKind{defs.IdentityUser, defs.IdentityUser, defs.IdentityAgent}},
	} {
		t.Run(test.name, func(t *testing.T) {
			query, mutation := openSessionDAO(t)
			fault := &inboxCheckpointMutation{Mutation: mutation}
			inbox := newSessionInbox(t, query, fault)
			ctx := t.Context()
			var want []defs.ItemID
			for _, sender := range test.senders {
				item := sessionInput("input")
				item.Message.From.Kind = sender
				want = append(want, addInboxInput(t, inbox, item, ""))
			}
			turnID := defs.NewTurnID()

			fault.fail = true
			if _, err := inbox.TakeWholeBatch(ctx, turnID); !errors.Is(err, errInboxCheckpoint) {
				t.Fatalf("TakeWholeBatch() error = %v", err)
			}
			fault.fail = false
			batch, err := inbox.TakeWholeBatch(ctx, turnID)
			if err != nil || !slices.Equal(inboxBatchIDs(batch), want) || len(inbox.GetUnreadSteer()) != 0 {
				t.Fatalf("TakeWholeBatch() = %v, %v; want borrowed %v", inboxBatchIDs(batch), err, want)
			}

			settle := func() error { return inbox.ConfirmSteer(ctx, turnID, want) }
			if test.startFailed {
				for _, group := range batch {
					for _, item := range group.Items {
						item.TurnID = turnID
					}
					if _, err := mutation.AppendItems(ctx, "inbox-conversation", "inbox-session", group.Items...); err != nil {
						t.Fatal(err)
					}
				}
				if err := mutation.SetTurn(ctx, defs.Turn{ID: turnID, ConversationID: "inbox-conversation", SessionID: "inbox-session", Status: defs.TurnStatusFailed}); err != nil {
					t.Fatal(err)
				}
				settle = func() error { return inbox.ClearFailedTurnInputs(ctx, turnID) }
			}
			fault.fail = true
			if err := settle(); !errors.Is(err, errInboxCheckpoint) {
				t.Fatalf("settle error = %v", err)
			}
			checkpoint, err := query.GetSessionCheckpoint(ctx, "inbox-conversation", "inbox-session")
			if err != nil || checkpoint == nil || checkpoint.TurnInputs == nil || checkpoint.TurnInputs.TurnID != turnID {
				t.Fatalf("checkpoint after failure = %+v, %v", checkpoint, err)
			}
			fault.fail = false
			if err := settle(); err != nil {
				t.Fatal(err)
			}
			checkpoint, err = query.GetSessionCheckpoint(ctx, "inbox-conversation", "inbox-session")
			if err != nil || checkpoint == nil || checkpoint.TurnInputs != nil {
				t.Fatalf("settled checkpoint = %+v, %v", checkpoint, err)
			}
			next := addInboxInput(t, inbox, sessionInput("next"), "")
			batch, err = inbox.TakeWholeBatch(ctx, defs.NewTurnID())
			if err != nil || !slices.Equal(inboxBatchIDs(batch), []defs.ItemID{next}) {
				t.Fatalf("next turn = %v, %v; want only %s", inboxBatchIDs(batch), err, next)
			}
			if test.startFailed {
				if got := inboxItemIDs(readSessionLog(t, query, "inbox-conversation", "inbox-session")); !slices.Equal(got, want) {
					t.Fatalf("failed turn items = %v, want %v", got, want)
				}
			}
		})
	}
}

func TestSessionInboxInputIDAndWithdraw(t *testing.T) {
	for _, test := range []struct {
		name      string
		withSkill bool
	}{
		{name: "queued_message"},
		{name: "restored_message_and_skill", withSkill: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			query, mutation := openSessionDAO(t)
			ctx := t.Context()
			inbox := newSessionInbox(t, query, mutation)
			item := sessionInput("first")
			items := []*defs.Item{item}
			if test.withSkill {
				items = append(items, &defs.Item{ID: defs.NewItemID(), Kind: defs.ItemSkillInstruction, SkillInstruction: &defs.SkillInstruction{InputItemID: item.ID, Name: "test", Text: "skill body"}})
				checkpoint := &defs.SessionCheckpoint{PendingUserInputs: []defs.SessionInputBatch{{{InputID: "retry", Items: items}}}}
				if err := mutation.AppendSessionCheckpoint(ctx, "inbox-conversation", "inbox-session", checkpoint); err != nil {
					t.Fatal(err)
				}
				if err := inbox.Load(ctx); err != nil {
					t.Fatal(err)
				}
			} else {
				addInboxInput(t, inbox, item, "retry")
			}
			if got := addInboxInput(t, inbox, sessionInput("duplicate"), "retry"); got != item.ID {
				t.Fatalf("duplicate got %s, want %s", got, item.ID)
			}
			got, err := inbox.Withdraw(ctx, item.ID)
			if err != nil || !slices.Equal(inboxItemIDs(got), inboxItemIDs(items)) {
				t.Fatalf("Withdraw() = %v, %v; want %v", inboxItemIDs(got), err, inboxItemIDs(items))
			}
			if batch, err := inbox.TakeWholeBatch(ctx, defs.NewTurnID()); err != nil || len(batch) != 0 {
				t.Fatalf("remaining batch = %v, %v", inboxBatchIDs(batch), err)
			}
		})
	}
}

func newSessionInbox(t *testing.T, query dao.Query, mutation dao.Mutation) *session.SessionInbox {
	t.Helper()
	inbox, err := session.NewSessionInbox(&session.SessionInboxOpt{
		ConversationID: "inbox-conversation", SessionID: "inbox-session",
		Query: query, Mutation: mutation,
	})
	if err != nil {
		t.Fatal(err)
	}
	return inbox
}

func addInboxInput(t *testing.T, inbox *session.SessionInbox, item *defs.Item, id defs.InputID) defs.ItemID {
	t.Helper()
	itemID, err := inbox.Add(t.Context(), &session.SessionInput{Mode: session.SessionInputModeNormal, Item: item, InputID: id}, "")
	if err != nil {
		t.Fatal(err)
	}
	return itemID
}

func inboxItemIDs(items []*defs.Item) []defs.ItemID {
	ids := make([]defs.ItemID, len(items))
	for i, item := range items {
		ids[i] = item.ID
	}
	return ids
}

func inboxBatchIDs(batch defs.SessionInputBatch) []defs.ItemID {
	ids := make([]defs.ItemID, len(batch))
	for i, group := range batch {
		ids[i] = group.Items[0].ID
	}
	return ids
}

var errInboxCheckpoint = errors.New("checkpoint write failed")

type inboxCheckpointMutation struct {
	dao.Mutation
	fail bool
}

func (m *inboxCheckpointMutation) AppendSessionCheckpoint(ctx context.Context, cid defs.ConversationID, sid defs.SessionID, checkpoint *defs.SessionCheckpoint) error {
	if m.fail {
		return errInboxCheckpoint
	}
	return m.Mutation.AppendSessionCheckpoint(ctx, cid, sid, checkpoint)
}
