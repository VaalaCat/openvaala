package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/VaalaCat/openvaala/pkg/world"
	"github.com/VaalaCat/openvaala/pkg/worldserver"
)

func TestFinishSubmittedInput(t *testing.T) {
	for _, tc := range []struct {
		name          string
		savedAfter    int
		wantSubscribe bool
		events        []runtimeclient.ClientEvent
	}{
		{name: "already completed", savedAfter: 1},
		{name: "completed while subscribing", savedAfter: 2, wantSubscribe: true},
		{name: "ignore adjacent input", savedAfter: 3, wantSubscribe: true, events: []runtimeclient.ClientEvent{
			&runtimeclient.TurnFinished{SessionID: "session", TurnID: "other", Status: defs.TurnStatusCompleted, InputItemIDs: []defs.ItemID{"other"}},
			&runtimeclient.TurnFinished{SessionID: "session", TurnID: "mine", Status: defs.TurnStatusCompleted, InputItemIDs: []defs.ItemID{"input"}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := make(chan runtimeclient.ClientEvent, len(tc.events))
			for _, event := range tc.events {
				events <- event
			}
			close(events)
			client := &submittedInputClient{savedAfter: tc.savedAfter, events: events}
			var output bytes.Buffer
			err := finishSubmittedInput(t.Context(), client, &output, runtimeprotocol.SessionParams{ConversationID: "chat", SessionID: "session"}, "input", false)
			if err != nil || output.String() != "my final answer\n" || client.subscribed != tc.wantSubscribe {
				t.Fatalf("output=%q, subscribed=%t, err=%v", output.String(), client.subscribed, err)
			}
		})
	}
}

type submittedInputClient struct {
	runtimeclient.Client
	scans, savedAfter int
	subscribed        bool
	events            chan runtimeclient.ClientEvent
}

func (c *submittedInputClient) Events() <-chan runtimeclient.ClientEvent { return c.events }

func (c *submittedInputClient) SubscribeSession(context.Context, *runtimeprotocol.SessionParams) (runtimeprotocol.SubscribeSessionResult, error) {
	c.subscribed = true
	return runtimeprotocol.SubscribeSessionResult{LastSavedOrdinal: 4}, nil
}

func (c *submittedInputClient) ListItems(_ context.Context, request *runtimeclient.ListItemsRequest) (runtimeclient.ListItemsResult, error) {
	if request.Order == "desc" {
		c.scans++
		if c.scans >= c.savedAfter {
			return runtimeclient.ListItemsResult{Items: []runtimeprotocol.LoggedItem{{Ordinal: 1, Item: &defs.Item{ID: "input", TurnID: "mine"}}}}, nil
		}
		return runtimeclient.ListItemsResult{}, nil
	}
	message := func(turn defs.TurnID, model defs.ModelCallID, text string) *defs.Item {
		return &defs.Item{TurnID: turn, ModelCallID: model, Message: &defs.Message{From: &defs.Address{Kind: defs.IdentityAgent}, Content: []*defs.Content{{Kind: defs.ContentText, Text: &defs.TextContent{Text: text}}}}}
	}
	return runtimeclient.ListItemsResult{Items: []runtimeprotocol.LoggedItem{
		{Ordinal: 2, Item: message("mine", "draft", "early answer")},
		{Ordinal: 3, Item: message("mine", "final", "my final answer")},
		{Ordinal: 4, Item: message("other", "other", "adjacent answer")},
	}}, nil
}

func (c *submittedInputClient) GetTurns(_ context.Context, request *runtimeclient.GetTurnsRequest) ([]defs.Turn, error) {
	turn := defs.Turn{ID: request.TurnIDs[0], Status: defs.TurnStatusCompleted, FinalModelCallID: "final", ItemsRange: defs.TurnItemRange{FirstOrdinal: 1, LastOrdinal: 3}}
	return []defs.Turn{turn}, nil
}

func TestWorldSessionPageURL(t *testing.T) {
	link, err := world.FormatRemoteAccessURL("https://page.test", world.RemoteAccessTarget{
		Node: "b", Domain: "home.test", PublicKey: make([]byte, 32), Entries: worldserver.PeerEntries{Relays: []string{"https://relay.test"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, url, want string }{
		{"world", link, strings.Replace(link, "/web/v1/", "/web/v1/s/server/c/chat/sessions/session", 1)},
		{"websocket", "wss://serve.test/api/v1/openvaala", "https://serve.test/web/v1/s/server/c/chat/sessions/session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := getSessionPageURL(runtimeConnection{URL: tc.url, Token: "secret"}, "server", runtimeprotocol.SessionParams{ConversationID: "chat", SessionID: "session"})
			if err != nil || got != tc.want {
				t.Fatalf("session page = %q, want %q: %v", got, tc.want, err)
			}
		})
	}
}
