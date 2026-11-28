package worldserver

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/coder/websocket/wsjson"
)

func TestWorldPeerDomainCards(t *testing.T) {
	server := serveWorldForTest(t)
	id, observerKey := registerWorldNode(t, server, "observer", "team", nil)
	observer := connectWorldNode(t, server, id, observerKey)
	writeWorldControl(t, observer, ControlTypePing, nil)
	readWorldControl(t, observer, ControlTypePong)
	const path = "/admin/domains"
	key, _, _ := ed25519.GenerateKey(rand.Reader)
	card := func(domain string, key []byte, relay string) string {
		data, _ := json.Marshal(PeerDomain{Domain: domain, PublicKey: key, Entries: PeerEntries{Relays: []string{relay}}})
		return domain + "#" + base64.RawURLEncoding.EncodeToString(data)
	}
	const relay = "https://relay.example"
	peer := card("peer.test", key, relay)
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	for _, tc := range []struct {
		name, method, suffix, token, card string
		status                            int
	}{
		{"admin required", "GET", "", "", "", 401},
		{"empty directory", "GET", "", testAdminToken, "", 200},
		{"domain mismatch", "POST", "", testAdminToken, "other" + peer, 400},
		{"invalid key", "POST", "", testAdminToken, card("peer.test", key[:31], relay), 400},
		{"own domain", "POST", "", testAdminToken, card(testDomain, key, relay), 409},
		{"add", "POST", "", testAdminToken, peer, 201},
		{"repeat", "POST", "", testAdminToken, peer, 200},
		{"update entries", "POST", "", testAdminToken, card("peer.test", key, "https://new.example"), 200},
		{"conflict", "POST", "", testAdminToken, card("peer.test", other, relay), 409},
		{"listed", "GET", "", testAdminToken, "", 200},
		{"delete", "DELETE", "/peer.test", testAdminToken, "", 204},
		{"replace", "POST", "", testAdminToken, card("peer.test", other, relay), 201},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := worldHTTP(t, server, tc.method, path+tc.suffix, tc.token, map[string]string{"card": tc.card}, tc.status)
			switch tc.name {
			case "add", "update entries":
				saved := decodeWorldJSON[PeerDomain](t, data)
				directory := receiveWorldPayload[Directory](t, observer, ControlTypeDirectory)
				if len(directory.Domains) != 0 || saved.Status != DomainPendingOutbound {
					t.Fatalf("directory = %+v, saved = %+v", directory.Domains, saved)
				}
			case "repeat":
				writeWorldControl(t, observer, ControlTypePing, nil)
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				var next ControlEnvelope
				if err := wsjson.Read(ctx, observer, &next); err != nil || next.Type != ControlTypePong {
					t.Fatalf("after duplicate import: frame = %s, error = %v", next.Type, err)
				}
			}
			if tc.status == 409 && decodeWorldJSON[map[string]string](t, data)["code"] != "world_domain_name_taken" {
				t.Fatal(string(data))
			}
			if tc.status == 400 && decodeWorldJSON[map[string]string](t, data)["code"] != "invalid_arguments" {
				t.Fatal(string(data))
			}
			if tc.method == "GET" && tc.status == 200 {
				result := decodeWorldJSON[ListDomainsResult](t, data)
				if result.Domain != testDomain || result.Domains == nil || result.SelfCard != "" {
					t.Fatal(result)
				}
				if tc.name == "listed" && (len(result.Domains) != 1 || !bytes.Equal(result.Domains[0].PublicKey, key) || !slices.Equal(result.Domains[0].Entries.Relays, []string{"https://new.example"})) {
					t.Fatal(result)
				}
			}
		})
	}
}

func TestWorldDomainLookup(t *testing.T) {
	base, server := serveWorldDirectoryForTest(t)
	id, memberKey := registerWorldNode(t, base, "bob", "team", nil)
	socket := connectWorldNode(t, base, id, memberKey)
	entries := PeerEntries{DirectURLs: []string{"https://bob.test"}}
	writeWorldControl(t, socket, ControlTypeNodeAnnounce, NodeAnnounce{Entries: entries})
	waitForWorldNode(t, socket, id, func(n DirectoryNode) bool { return slices.Equal(n.Entries.DirectURLs, entries.DirectURLs) })
	domainKey, _, _ := ed25519.GenerateKey(rand.Reader)
	data, _ := json.Marshal(PeerDomain{Domain: "peer.test", PublicKey: domainKey, Entries: entries})
	if _, _, err := server.AddDomain(t.Context(), "peer.test#"+base64.RawURLEncoding.EncodeToString(data)); err != nil {
		t.Fatal(err)
	}
	if _, err := server.GetMember(t.Context(), domainKey, "bob"); !errors.Is(err, ErrDomainNotTrusted) {
		t.Fatal(err)
	}
	if _, err := server.ReceiveTrust(t.Context(), domainKey, DomainTrustMessage{Op: "trust_response", Domain: "peer.test", Decision: "accepted"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, node string
		caller     ed25519.PublicKey
		want       error
	}{
		{"trusted", "bob", domainKey, nil},
		{"member key", "bob", memberKey.Public().(ed25519.PublicKey), ErrDomainNotTrusted},
		{"unknown node", "absent", domainKey, PeerNotFoundError("absent@" + testDomain)},
		{"revoked", "bob", domainKey, PeerNotFoundError("bob@" + testDomain)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "revoked" {
				if err := server.RevokeNode(t.Context(), id); err != nil {
					t.Fatal(err)
				}
			}
			got, err := server.GetMember(t.Context(), tc.caller, tc.node)
			if !errors.Is(err, tc.want) {
				t.Fatalf("member=%+v err=%v", got, err)
			}
			if err == nil && (got.Owner != "team" || !bytes.Equal(got.PublicKey, memberKey.Public().(ed25519.PublicKey)) || !slices.Equal(got.Entries.DirectURLs, entries.DirectURLs)) {
				t.Fatal(got)
			}
		})
	}
}

func TestWorldDomainInvitations(t *testing.T) {
	key, _, _ := ed25519.GenerateKey(rand.Reader)
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	entries := PeerEntries{DirectURLs: []string{"https://guest"}}
	data, _ := json.Marshal(PeerDomain{Domain: "guest", PublicKey: key, Entries: entries})
	card := "guest#" + base64.RawURLEncoding.EncodeToString(data)
	for _, tc := range []struct{ initial, op, want string }{
		{"", "import", DomainPendingOutbound}, {"", "request", DomainPendingInbound},
		{DomainPendingOutbound, "import", DomainPendingOutbound}, {DomainPendingOutbound, "request", DomainMutual},
		{DomainPendingInbound, "import", DomainMutual}, {DomainPendingInbound, "request", DomainPendingInbound},
		{DomainMutual, "request", DomainMutual}, {DomainMutual, "import", DomainMutual},
		{DomainRejected, "request", DomainRejected}, {DomainRejected, "import", DomainPendingOutbound},
	} {
		t.Run(tc.initial+"/"+tc.op, func(t *testing.T) {
			_, s := serveWorldDirectoryForTest(t)
			ctx := t.Context()
			invitation := DomainTrustMessage{Op: "trust_request", Card: card}
			if _, err := s.ReceiveTrust(ctx, other, invitation); err == nil {
				t.Fatal("accepted mismatched signing identity")
			}
			if tc.initial == DomainPendingOutbound {
				if _, _, err := s.AddDomain(ctx, card); err != nil {
					t.Fatal(err)
				}
			}
			if tc.initial != "" && tc.initial != DomainPendingOutbound {
				if _, err := s.ReceiveTrust(ctx, key, invitation); err != nil {
					t.Fatal(err)
				}
				if tc.initial == DomainMutual || tc.initial == DomainRejected {
					if err := s.DecideDomain(ctx, "guest", tc.initial == DomainMutual); err != nil {
						t.Fatal(err)
					}
				}
			}
			var err error
			if tc.op == "request" {
				var reply DomainTrustMessage
				reply, err = s.ReceiveTrust(ctx, key, invitation)
				want := map[string]string{DomainMutual: "accepted", DomainRejected: "rejected"}[tc.want]
				if reply.Decision != want {
					t.Fatalf("reply = %+v, want decision %q", reply, want)
				}
			} else {
				_, _, err = s.AddDomain(ctx, card)
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.GetDomains(ctx)
			if err != nil || len(got.Domains) != 1 || got.Domains[0].Status != tc.want {
				t.Fatal(got, err)
			}
			_, err = s.GetMember(ctx, key, "absent")
			if errors.Is(err, ErrDomainNotTrusted) != (tc.want != DomainMutual) {
				t.Fatal(tc.want, err)
			}
		})
	}
}
