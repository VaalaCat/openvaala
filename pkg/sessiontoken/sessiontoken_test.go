package sessiontoken_test

import (
	"strings"
	"testing"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/sessiontoken"
)

func TestSessionToken(t *testing.T) {
	const secret = "serve-token"
	conversationID, sessionID := defs.ConversationID("conversation"), defs.SessionID("session")
	token := sessiontoken.Issue(secret, conversationID, sessionID)
	prefix := string(conversationID) + "." + string(sessionID) + "."
	if token != "conversation.session.xf1uUMopkQ1jOgGHuTfwsoh3dkYXPsy6TlTCGKhU6EY" {
		t.Fatalf("token has unexpected format: %q", token)
	}
	signature := strings.TrimPrefix(token, prefix)
	first := "A"
	if signature[0] == 'A' {
		first = "B"
	}
	for _, test := range []struct {
		name   string
		secret string
		token  string
		wantOK bool
	}{
		{name: "original", secret: secret, token: token, wantOK: true},
		{name: "signature_changed", secret: secret, token: prefix + first + signature[1:]},
		{name: "conversation_changed", secret: secret, token: "other." + string(sessionID) + "." + signature},
		{name: "session_changed", secret: secret, token: string(conversationID) + ".other." + signature},
		{name: "secret_changed", secret: "other", token: token},
		{name: "extra_separator", secret: secret, token: token + ".extra"},
		{name: "empty_session", secret: secret, token: string(conversationID) + ".." + signature},
		{name: "operator_token", secret: secret, token: secret},
		{name: "missing_secret", token: token},
	} {
		t.Run(test.name, func(t *testing.T) {
			conversation, session, ok := sessiontoken.Verify(test.secret, test.token)
			if ok != test.wantOK {
				t.Fatalf("Verify() ok = %t, want %t", ok, test.wantOK)
			}
			if ok && (conversation != conversationID || session != sessionID) {
				t.Fatalf("Verify() = (%q, %q), want (%q, %q)", conversation, session, conversationID, sessionID)
			}
		})
	}
}

func TestSessionTokenIssueInvalidID(t *testing.T) {
	for _, test := range []struct {
		name           string
		conversationID defs.ConversationID
		sessionID      defs.SessionID
	}{
		{name: "dotted_conversation", conversationID: "conversation.other", sessionID: "session"},
		{name: "dotted_session", conversationID: "conversation", sessionID: "session.other"},
		{name: "signed_field_separator", conversationID: "conversation|other", sessionID: "session"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := sessiontoken.Issue("secret", test.conversationID, test.sessionID); got != "" {
				t.Fatalf("Issue() = %q, want an empty token", got)
			}
		})
	}
}
