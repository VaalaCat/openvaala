package acp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/externalagent"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
	"github.com/VaalaCat/openvaala/pkg/prompt"
	"github.com/VaalaCat/openvaala/pkg/workspace"
	sdk "github.com/coder/acp-go-sdk"
)

type live struct {
	externalagent.ExternalAgentSession
	events chan externalagent.ExternalSessionEvent
	done   chan error
	cancel context.CancelFunc
}

func open(t *testing.T, p externalagent.ExternalAgentProvider, req externalagent.ExternalSessionOpenRequest) *live {
	t.Helper()
	s := p.OpenSession(req)
	ctx, cancel := context.WithCancel(t.Context())
	l := &live{s, make(chan externalagent.ExternalSessionEvent, 512), make(chan error, 1), cancel}
	go func() { l.done <- s.Run(ctx, l.events); close(l.done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-l.done:
		case <-time.After(5 * time.Second):
			t.Error("process did not stop")
		}
	})
	return l
}
func event[T externalagent.ExternalSessionEvent](t *testing.T, l *live) T {
	t.Helper()
	for {
		select {
		case e := <-l.events:
			if v, ok := e.(T); ok {
				return v
			}
		case err := <-l.done:
			t.Fatalf("driver stopped: %v", err)
		case <-time.After(10 * time.Second):
			t.Fatal("missing driver event")
		}
	}
}

type launchCommandWorkspace struct {
	workspace.Workspace
	command string
}

func (w *launchCommandWorkspace) StartProcess(ctx context.Context, req *workspace.ProcessRequest) (workspace.Process, error) {
	w.command = req.Command
	return w.Workspace.StartProcess(ctx, req)
}

func TestACPTransport(t *testing.T) {
	cases := []struct {
		name                           string
		load, http, localMCP, filtered bool
	}{
		{name: "new", http: true, localMCP: true},
		{name: "load", load: true, http: true, localMCP: true},
		{name: "no http new", localMCP: true},
		{name: "no http load", load: true, localMCP: true},
		{name: "no local endpoint", http: true},
		{name: "filtered new", filtered: true},
		{name: "filtered load", load: true, filtered: true},
		{name: "missing executable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t, fmt.Sprintf("-http=%t", tc.http))
			stdio := &mcpclient.ServerDefinition{ID: "files", Type: mcpclient.ServerTransportStdio, Command: "fixture-command", Args: []string{"a b"}, Env: map[string]string{"F": "V"}, Required: true}
			if tc.filtered {
				stdio.ToolNames = []string{"one"}
			}
			work := &launchCommandWorkspace{Workspace: f.ws}
			req := externalagent.ExternalSessionOpenRequest{
				Workspace: work, Cwd: f.dir, ExecutorID: "test-executor",
				LaunchEnv:      map[string]string{"OPENVAALA_SESSION_ID": "host-id", "TEST_OVERLAY": "launch", "OPENVAALA_TOKEN": "fixture-session-token"},
				InitialOptions: []defs.ExternalOptionSelection{{Kind: "model", Value: "large"}},
				MCPServers:     []*mcpclient.ServerDefinition{stdio, {ID: "http-service", Type: mcpclient.ServerTransportHTTP, URL: "http://localhost/optional", Headers: map[string]string{"X-A": "B"}}},
			}
			if tc.localMCP {
				req.LocalMCPURL = "http://localhost/mcp"
			}
			method := "session/new"
			if tc.load {
				method = "session/load"
				req.Binding = &defs.ExternalSession{Cwd: f.dir, SessionID: "old", ExecutorID: "test-executor", AgentType: defs.AgentTypeACP}
			}
			if tc.name == "missing executable" {
				l := open(t, NewACPAgentProvider(defs.ACPCommand{Command: "/missing/acp"}), req)
				if err := <-l.done; err == nil || !strings.Contains(err.Error(), "127") {
					t.Fatalf("missing command exit: %v", err)
				}
				return
			}
			l := open(t, f.provider, req)
			ready := event[*externalagent.ExternalSessionReadyEvent](t, l)
			calls := f.calls(t, method)
			if len(calls) != 1 {
				t.Fatal(calls)
			}
			var sent struct {
				McpServers []struct {
					Name, Command, URL string
					Args               []string
					Env, Headers       []struct{ Name, Value string }
				}
			}
			if err := json.Unmarshal(calls[0].Params, &sent); err != nil {
				t.Fatal(err)
			}
			var wireNames []string
			for _, server := range sent.McpServers {
				wireNames = append(wireNames, server.Name)
				switch server.Name {
				case "files":
					if server.Command != "fixture-command" || !slices.Equal(server.Args, []string{"a b"}) || len(server.Env) != 1 || server.Env[0].Name != "F" || server.Env[0].Value != "V" {
						t.Fatal(server)
					}
				case "http-service":
					if server.URL != "http://localhost/optional" || len(server.Headers) != 1 || server.Headers[0].Name != "X-A" || server.Headers[0].Value != "B" {
						t.Fatal(server)
					}
				case "openvaala":
					if tc.http {
						if server.URL != req.LocalMCPURL || len(server.Headers) != 1 || server.Headers[0].Name != "Authorization" || server.Headers[0].Value != "Bearer fixture-session-token" {
							t.Fatal("local HTTP MCP connection parameters")
						}
					} else if server.Command != "openvaala" || !slices.Equal(server.Args, []string{"mcp-bridge"}) ||
						len(server.Env) != 2 ||
						server.Env[0].Name != "OPENVAALA_MCP_URL" || server.Env[0].Value != req.LocalMCPURL ||
						server.Env[1].Name != "OPENVAALA_TOKEN" || server.Env[1].Value != req.LaunchEnv["OPENVAALA_TOKEN"] {
						t.Fatal("local stdio MCP connection parameters")
					}
				}
			}
			want := []string{"files"}
			if tc.http {
				want = append(want, "http-service")
			}
			if tc.localMCP {
				want = append(want, "openvaala")
			}
			if !slices.Equal(wireNames, ready.MCPServers) || !slices.Equal(wireNames, want) {
				t.Fatalf("wire %v, Ready %v, want %v", wireNames, ready.MCPServers, want)
			}
			if (len(f.calls(t, "session/set_config_option")) == 0) != tc.load {
				t.Fatal("initial options on wrong path")
			}
			if strings.Contains(work.command, req.LaunchEnv["OPENVAALA_TOKEN"]) {
				t.Fatal("session token in process arguments")
			}
			env := f.calls(t, "started")
			if len(env) != 1 || !bytes.Contains(env[0].Params, []byte(`"fixtureToken":true`)) || !bytes.Contains(env[0].Params, []byte("host-id")) || !bytes.Contains(env[0].Params, []byte("launch")) || !bytes.Contains(env[0].Params, []byte(f.dir+":"+os.Getenv("PATH"))) {
				t.Fatal("environment overlay")
			}
		})
	}
}

func TestACPModeOptions(t *testing.T) {
	for _, tc := range []struct {
		name, kind, id, method string
		args                   []string
	}{
		{name: "legacy modes", kind: "mode", id: "mode", method: "session/set_mode"},
		{name: "config mode", kind: "config", id: "workflow", method: "session/set_config_option", args: []string{"-config-mode"}},
		{name: "both reports", kind: "config", id: "workflow", method: "session/set_config_option", args: []string{"-config-mode", "-legacy-mode"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t, tc.args...)
			l := open(t, f.provider, externalagent.ExternalSessionOpenRequest{Workspace: f.ws, Cwd: f.dir})
			ready := event[*externalagent.ExternalSessionReadyEvent](t, l)
			if mode := ready.Options.Mode; mode == nil || mode.Kind != tc.kind || mode.ID != tc.id || mode.CurrentValue != "plan" {
				t.Errorf("mode = %+v, want %s:%s=plan", mode, tc.kind, tc.id)
			}
			if len(ready.Options.Others) != 1 || ready.Options.Others[0].ID != "secondary" {
				t.Errorf("other options = %+v, want only secondary", ready.Options.Others)
			}
			if err := l.SetOption(t.Context(), defs.ExternalOptionSelection{Kind: "mode", Value: "code"}); err != nil {
				t.Fatal(err)
			}
			updated := event[*externalagent.ExternalOptionsUpdatedEvent](t, l)
			if mode := updated.Options.Mode; mode == nil || mode.CurrentValue != "code" {
				t.Errorf("updated mode = %+v, want code", mode)
			}
			for _, method := range []string{"session/set_mode", "session/set_config_option"} {
				calls := f.calls(t, method)
				if len(calls) != 0 && method != tc.method || len(calls) != 1 && method == tc.method {
					t.Errorf("%s calls = %+v, want one %s call", method, calls, tc.method)
				}
			}
		})
	}
}

func TestACPReplayAttachmentSuffix(t *testing.T) {
	file := defs.FileRef{Name: "sample.txt", Path: "/sample.txt", Size: 12}
	markup := prompt.FormatAttachment(file)
	for _, tc := range []struct {
		name          string
		tail          string
		wantFirstKind defs.ContentKind
	}{
		{"file on both sides of an inline image", markup, defs.ContentFile},
		{"later body ends the suffix", "more text\n\n" + markup, defs.ContentText},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "history")
			data, err := json.Marshal([]sdk.ContentBlock{
				sdk.TextBlock(markup), sdk.ImageBlock("YQ==", "image/png"),
				sdk.TextBlock(tc.tail), sdk.TextBlock("\n\n" + prompt.FormatSessionInfo(defs.Session{ID: "old"})),
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			f := setup(t, "-history-file", path)
			l := open(t, f.provider, externalagent.ExternalSessionOpenRequest{Workspace: f.ws, Cwd: f.dir, Binding: &defs.ExternalSession{SessionID: "old", Cwd: f.dir}})
			parts := event[*externalagent.ExternalAgentStreamEvent](t, l).Event.Item.Message.Content
			if len(parts) < 3 || parts[0].Kind != tc.wantFirstKind || parts[1].Image == nil || parts[len(parts)-1].File == nil || *parts[len(parts)-1].File != file {
				t.Fatalf("replayed content = %+v", parts)
			}
			if tc.wantFirstKind == defs.ContentText && (len(parts) != 4 || parts[0].Text.Text != markup || parts[2].Text.Text != "more text") {
				t.Fatalf("body changed during replay: %+v", parts)
			}
		})
	}
}

func TestACPTurn(t *testing.T) {
	for _, tc := range []string{"end_turn", "max_tokens", "max_turn_requests", "refusal", "cancelled", "error", "exit", "ignore cancel", "image threshold", "image disabled", "image cumulative", "image directory missing", "live burst", "batch replay", "long replay", "replay", "updates", "wait-cancel", "permission always", "permission once", "permission none", "attachment replay"} {
		t.Run(tc, func(t *testing.T) {
			args := []string{"-prompt", tc, fmt.Sprintf("-image=%t", tc != "image disabled")}
			if perm, ok := strings.CutPrefix(tc, "permission "); ok {
				args = append(args, "-permission", perm)
			}
			if tc == "long replay" {
				args = append(args, "-history-count", "5000")
			}
			if tc == "live burst" {
				args = append(args, "-prompt-chunks", "3000")
			}
			if tc == "attachment replay" || tc == "batch replay" {
				args = append(args, "-history-file", filepath.Join(t.TempDir(), "history"))
			}
			f := setup(t, args...)
			req := externalagent.ExternalSessionOpenRequest{Workspace: f.ws, Cwd: f.dir, ExecutorID: "local"}
			if tc == "attachment replay" || strings.HasPrefix(tc, "image ") && tc != "image directory missing" {
				req.ExecutorID = "custom-workstation"
				kind := defs.ExecutorKindLocal
				dataDir := filepath.Join(t.TempDir(), "configured")
				root := dataDir
				if tc == "image disabled" {
					kind = defs.ExecutorKindSSH
					root = filepath.Join(f.dir, ".cache", "openvaala")
				}
				var err error
				req.AttachmentDirectory, err = executor.GetConversationAttachmentDirectory(t.Context(), f.ws, &defs.ExecutorDefinition{ID: req.ExecutorID, Kind: kind}, dataDir, "test-conversation")
				if err != nil || req.AttachmentDirectory != filepath.Join(root, "attachments", "test-conversation") {
					t.Fatalf("attachment directory %q, %v", req.AttachmentDirectory, err)
				}
			}
			if tc == "replay" || tc == "long replay" {
				req.Binding = &defs.ExternalSession{Cwd: f.dir, SessionID: "history"}
			}
			l := open(t, f.provider, req)
			if tc == "replay" || tc == "long replay" {
				var texts []string
				for {
					select {
					case e := <-l.events:
						switch u := e.(type) {
						case *externalagent.ExternalSessionReadyEvent:
							if tc == "long replay" {
								if len(texts) != 5000 || texts[4999] != "message 4999" {
									t.Fatalf("long replay delivered %d messages", len(texts))
								}
							} else if !slices.Equal(texts, []string{"hello world", "latest"}) {
								t.Fatal(texts)
							}
							return
						case *externalagent.ExternalAgentStreamEvent:
							if u.Replaying && u.Event.Type == agent.AgentEventItemCompleted && u.Event.Item.Message != nil && u.Event.Item.Message.From.Kind == defs.IdentityUser {
								texts = append(texts, u.Event.Item.Message.Content[0].Text.Text)
								if tc == "long replay" && len(texts)%100 == 0 {
									time.Sleep(5 * time.Millisecond)
								}
							}
						}
					case err := <-l.done:
						t.Fatalf("replay ended: %v", err)
					case <-time.After(5 * time.Second):
						t.Fatal("replay stalled")
					}
				}
			}
			ready := event[*externalagent.ExternalSessionReadyEvent](t, l)
			if len(ready.Commands) != 1 || ready.Commands[0].Name != "status" {
				t.Fatal(ready.Commands)
			}
			if tc == "updates" {
				for _, choice := range []defs.ExternalOptionSelection{{Kind: "mode", Value: "code"}, {Kind: "model", Value: "large"}} {
					if err := l.SetOption(t.Context(), choice); err != nil {
						t.Fatal(err)
					}
				}
				for {
					e := event[*externalagent.ExternalOptionsUpdatedEvent](t, l)
					if e.Options.Model != nil && e.Options.Mode != nil && e.Options.Model.CurrentValue == "large" && e.Options.Mode.CurrentValue == "code" {
						break
					}
				}
			}
			item := &defs.Item{ID: defs.NewItemID(), Kind: defs.ItemMessage, Message: &defs.Message{Content: []*defs.Content{{Kind: defs.ContentText, Text: &defs.TextContent{Text: "/status args"}}}}}
			if strings.HasPrefix(tc, "image ") {
				sizes := []int{5 << 20, 5<<20 + 1, (10 << 20) - 1}
				if tc == "image directory missing" {
					sizes = []int{5<<20 + 1}
				}
				if tc == "image cumulative" {
					sizes = []int{4 << 20, 4 << 20}
				}
				for _, size := range sizes {
					item.Message.Content = append(item.Message.Content, &defs.Content{Kind: defs.ContentImage, Image: &defs.ImageContent{Alt: "pic.png", DataURL: "data:image/png;base64," + base64.StdEncoding.EncodeToString(make([]byte, size))}})
				}
			}
			if tc == "attachment replay" {
				item.Message.Content = []*defs.Content{
					{Kind: defs.ContentText, Text: &defs.TextContent{Text: "<openvaala-chat-history-summary>\n<hint>以下是先前对话的历史摘要，并非新的用户输入。</hint>\n[User] hello\n</openvaala-chat-history-summary>"}},
					{Kind: defs.ContentText, Text: &defs.TextContent{Text: "visible"}},
					{Kind: defs.ContentImage, Image: &defs.ImageContent{DataURL: "data:image/png;base64,YQ=="}},
					{Kind: defs.ContentFile, File: &defs.FileRef{Name: "book.pdf", MIME: "application/pdf", Size: 10, Path: "/data/book.pdf"}},
					{Kind: defs.ContentImage, Image: &defs.ImageContent{Alt: "large.png", DataURL: "data:image/png;base64," + base64.StdEncoding.EncodeToString(make([]byte, (10<<20)-1))}},
					{Kind: defs.ContentText, Text: &defs.TextContent{Text: prompt.FormatSessionInfo(defs.Session{ID: "test"})}},
				}
			}
			sendCtx, cancelSend := context.WithCancel(t.Context())
			defer cancelSend()
			items := []*defs.Item{item}
			if tc == "batch replay" {
				item.Message.Content[0].Text.Text = "one"
				item.Message.Content = append(item.Message.Content, &defs.Content{Kind: defs.ContentFile, File: &defs.FileRef{Name: "a", Path: "/a", Size: 3}})
				item.Message.Content = append(item.Message.Content, &defs.Content{Kind: defs.ContentImage, Image: &defs.ImageContent{DataURL: "data:image/png;base64,YQ=="}})

				items = append(items, &defs.Item{Kind: defs.ItemMessage, Message: &defs.Message{Content: []*defs.Content{
					{Kind: defs.ContentText, Text: &defs.TextContent{Text: "two"}},
					{Kind: defs.ContentText, Text: &defs.TextContent{Text: prompt.FormatSessionInfo(defs.Session{ID: "pasted"})}},
				}}})
				var err error
				items, err = externalagent.PrepareExternalPrompt(items, "", &defs.Session{ID: "host"})
				if err != nil {
					t.Fatal(err)
				}
			}
			err := l.StartTurn(sendCtx, "turn", items)
			if tc == "image directory missing" {
				if err == nil || !strings.Contains(err.Error(), "attachment directory") {
					t.Fatalf("missing assembly directory: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc == "wait-cancel" {
				cancelSend()
				if err := l.InterruptTurn(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if tc == "exit" {
				select {
				case err := <-l.done:
					if err == nil || !strings.Contains(err.Error(), "23") {
						t.Fatalf("missing exit code: %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("exit hung")
				}
				return
			}
			if tc == "ignore cancel" {
				if err := l.InterruptTurn(t.Context()); err != nil {
					t.Fatal(err)
				}
				l.cancel()
				if err := <-l.done; !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				return
			}
			var measured, commands bool
			toolResults := map[string]string{}
			var reasoning defs.ReasoningStatus
			var deltaCount int
			for {
				select {
				case e := <-l.events:
					switch e := e.(type) {
					case *externalagent.ExternalCommandsUpdatedEvent:
						commands = len(e.Commands) == 1 && e.Commands[0].Name == "more"
					case *externalagent.ExternalAgentStreamEvent:
						if e.Event.Item != nil && e.Event.Item.ToolResult != nil {
							r := e.Event.Item.ToolResult
							toolResults[r.CallID] = string(r.Content)
						}
						if v := e.Event; v.Item != nil && v.Item.Reasoning != nil && v.Type != agent.AgentEventItemStarted {
							reasoning = v.Item.Reasoning.Status
						}
						if tc == "live burst" && e.Event.Delta != nil && e.Event.Delta.Text == "x" {
							deltaCount++
							if deltaCount%100 == 1 {
								time.Sleep(8 * time.Millisecond)
							}
						}
						if e.Event.Type == agent.AgentEventContextMeasured {
							m := e.Event.Measurement
							measured = m.IsUsageBased && m.RequestTokens == 24 && m.Window == 100
						}
					case *externalagent.ExternalTurnFinishedEvent:
						if e.TurnID != "turn" {
							t.Fatal(e)
						}
						switch tc {
						case "error":
							if e.Status != defs.TurnStatusFailed || e.Err == nil || e.Err.Error() != "fixture failure (code -32008)" {
								t.Fatalf("%+v", e)
							}
						case "cancelled", "wait-cancel":
							if e.Status != defs.TurnStatusInterrupted {
								t.Fatal(e)
							}
						default:
							if e.Status != defs.TurnStatusCompleted || !measured || e.Usage == nil || e.Usage.Input != 12 {
								t.Fatalf("%+v usage %v", e, measured)
							}
						}
						if tc == "updates" {
							summaries := f.calls(t, "prompt-summary")
							if len(summaries) != 1 || !bytes.Contains(summaries[0].Params, []byte(`"/status args"`)) {
								t.Fatal("slash arguments changed")
							}
						}
						if tc == "live burst" && deltaCount != 3000 {
							t.Fatalf("received %d live chunks", deltaCount)
						}
						if tc == "cancelled" && reasoning != defs.ReasoningInterrupted || tc == "updates" && reasoning != defs.ReasoningCompleted {
							t.Fatalf("reasoning %q", reasoning)
						}
						if tc == "updates" && (toolResults["tool1"] != `"done"` || !strings.Contains(toolResults["tool2"], "stored") || !commands) {
							t.Fatalf("tool/command notifications %v %v", toolResults, commands)
						}
						if strings.HasPrefix(tc, "permission ") {
							r := f.calls(t, "permission-result")
							if len(r) != 1 {
								t.Fatal(r)
							}
							var result struct {
								Outcome struct{ Outcome, OptionID string }
							}
							if err := json.Unmarshal(r[0].Params, &result); err != nil {
								t.Fatal(err)
							}
							outcome, option := "selected", "a"
							switch tc {
							case "permission once":
								option = "o"
							case "permission none":
								outcome, option = "cancelled", ""
							}
							if result.Outcome.Outcome != outcome || result.Outcome.OptionID != option {
								t.Fatalf("permission %v, want %s %q", result.Outcome, outcome, option)
							}
						}
						if tc == "attachment replay" || tc == "batch replay" {
							l.cancel()
							<-l.done
							req.Binding = &ready.Binding
							l2 := open(t, f.provider, req)
							e := event[*externalagent.ExternalAgentStreamEvent](t, l2)
							if e.Event.Item == nil || e.Event.Item.Message == nil {
								t.Fatal(e)
							}
							blocks := e.Event.Item.Message.Content
							if tc == "batch replay" {
								if len(blocks) != 3 || blocks[0].Text == nil || blocks[0].Text.Text != "one\n\ntwo\n\n"+prompt.FormatSessionInfo(defs.Session{ID: "pasted"}) || blocks[1].File == nil || blocks[1].File.Path != "/a" || blocks[2].Image == nil {
									t.Fatal("body and file lost on batch replay")
								}
								return
							}
							if len(blocks) != 4 || blocks[0].Text.Text != "visible" || blocks[1].Image == nil || blocks[2].File == nil || blocks[2].File.Name != "book.pdf" || blocks[3].File == nil || blocks[3].File.Size != (10<<20)-1 || filepath.Dir(filepath.Dir(blocks[3].File.Path)) != req.AttachmentDirectory {
								t.Fatalf("replayed blocks %+v", blocks)
							}
						}
						if strings.HasPrefix(tc, "image ") {
							calls := f.calls(t, "prompt-summary")
							if len(calls) != 1 {
								t.Fatal(calls)
							}
							p := string(calls[0].Params)
							want := 0
							if tc == "image threshold" || tc == "image cumulative" {
								want = 1
							}
							var summary struct {
								Texts     []string
								LineBytes int
							}
							if err := json.Unmarshal(calls[0].Params, &summary); err != nil {
								t.Fatal(err)
							}
							if summary.LineBytes > 10<<20 {
								t.Fatalf("ACP frame %d", summary.LineBytes)
							}
							var sizes []int64
							for _, text := range summary.Texts {
								_, files := prompt.ParseAttachments(text)
								for _, f := range files {
									info, err := os.Stat(f.Path)
									if err != nil || info.Size() != f.Size || filepath.Dir(filepath.Dir(f.Path)) != req.AttachmentDirectory {
										t.Fatal(f, err)
									}
									sizes = append(sizes, f.Size)
								}
							}
							expected := []int64{5<<20 + 1, (10 << 20) - 1}
							if tc == "image cumulative" {
								expected = []int64{4 << 20}
							}
							if want == 0 {
								expected = append([]int64{5 << 20}, expected...)
							}
							if !slices.Equal(sizes, expected) {
								t.Fatalf("saved image sizes %v", sizes)
							}
							if !strings.Contains(p, fmt.Sprintf("\"images\":%d", want)) || !strings.Contains(p, "openvaala-attachment") {
								t.Fatal(p)
							}
						}
						return
					}
				case <-time.After(10 * time.Second):
					t.Fatal("prompt stalled")
				}
			}
		})
	}
}
