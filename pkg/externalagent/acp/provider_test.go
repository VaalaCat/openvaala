package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/externalagent"
	"github.com/VaalaCat/openvaala/pkg/workspace"
)

var fakeBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "openvaala-fakeacp-")
	if err != nil {
		panic(err)
	}
	fakeBinary = filepath.Join(dir, "fakeacp")
	cmd := exec.Command("go", "build", "-o", fakeBinary, "../../../internal/e2e/fakeacp")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		os.RemoveAll(dir)
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type fixture struct {
	provider externalagent.ExternalAgentProvider
	ws       workspace.Workspace
	dir, log string
}

// fixtureWorkspace 把测试子进程和附件都限制在本用例的临时目录。
type fixtureWorkspace struct{ workspace.Workspace }

func (w fixtureWorkspace) GetHomeDirectory(context.Context) (string, error) {
	return w.GetRootDirectory(), nil
}

func setup(t *testing.T, args ...string) fixture {
	t.Helper()
	dir := t.TempDir()
	// 命令只在目标环境的 PATH 上存在，并含有需要 shell 引用的字符。
	name := "peer 'fixture"
	if err := os.Symlink(fakeBinary, filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + dir + ":" + os.Getenv("PATH"), "TEST_OVERLAY=executor"}
	ws, err := workspace.NewLocalWorkspace(workspace.NewLocalWorkspaceOpt(dir).WithEnvironment(env))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	log := filepath.Join(dir, "wire.jsonl")
	p := NewACPAgentProvider(defs.ACPCommand{Command: name, Args: append([]string{"-record", log}, args...), Env: map[string]string{"TEST_OVERLAY": "config", "OPENVAALA_SESSION_ID": "config-id"}})
	return fixture{p, fixtureWorkspace{ws}, dir, log}
}

type wireCall struct {
	Method string
	Params json.RawMessage
}

func (f fixture) calls(t *testing.T, method string) []wireCall {
	t.Helper()
	data, err := os.ReadFile(f.log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var result []wireCall
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		var c wireCall
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			t.Fatal(err)
		}
		if c.Method == method {
			result = append(result, c)
		}
	}
	return result
}

func TestACPProbe(t *testing.T) {
	for _, tc := range []string{"cache", "search", "list pages", "directory pages", "config mode", "limit", "no list", "preview", "no load", "timeout"} {
		t.Run(tc, func(t *testing.T) {
			args := []string{}
			switch tc {
			case "config mode":
				args = []string{"-config-mode"}
			case "no list":
				args = []string{"-list=false"}
			case "no load":
				args = []string{"-load=false"}
			case "timeout":
				args = []string{"-stall", "initialize"}
			case "list pages", "directory pages":
				args = []string{"-sessions", "23"}
			case "limit":
				args = []string{"-sessions", "521"}
			}
			f := setup(t, args...)
			p := f.provider
			ctx := t.Context()
			switch tc {
			case "config mode":
				opts, err := p.GetOptions(ctx, externalagent.ExternalAgentOptionsRequest{Workspace: f.ws, ExecutorID: "test", Cwd: f.dir})
				if err != nil || opts.Mode == nil || opts.Mode.Kind != "config" || opts.Mode.ID != "workflow" {
					t.Fatalf("config mode %+v: %v", opts.Mode, err)
				}
			case "cache":
				if _, ok := p.GetCachedOptions("ssh", f.dir); ok || len(f.calls(t, "initialize")) > 0 {
					t.Fatal("cold cache started process")
				}
				req := externalagent.ExternalAgentOptionsRequest{Workspace: f.ws, ExecutorID: "ssh", Cwd: f.dir}
				opts, err := p.GetOptions(ctx, req)
				if err != nil || opts.Model == nil || opts.Model.ID != "backend-model" || opts.Thought == nil || len(opts.Others) < 1 {
					t.Fatalf("options %+v %v", opts, err)
				}
				opts.Model.Options[0].Label = "edited by caller"
				opts, err = p.GetOptions(ctx, req)
				if err != nil || opts.Model.Options[0].Label == "edited by caller" || len(f.calls(t, "initialize")) != 1 {
					t.Fatalf("cache %v", err)
				}
				req.Selections = []defs.ExternalOptionSelection{{Kind: "model", Value: "large"}}
				opts, err = p.GetOptions(ctx, req)
				if err != nil || opts.Model.CurrentValue != "large" || opts.Thought.CurrentValue != "high" {
					t.Fatalf("selection %+v %v", opts, err)
				}
				cached, ok := p.GetCachedOptions("ssh", f.dir)
				if !ok || cached.Model.CurrentValue != "small" {
					t.Fatal(cached)
				}
				req.ForceRefresh = true
				_, err = p.GetOptions(ctx, req)
				if err != nil || len(f.calls(t, "initialize")) != 3 {
					t.Fatalf("refresh %v", err)
				}
			case "list pages", "directory pages":
				q := ""
				if tc == "directory pages" {
					q = strings.ToUpper(f.dir)
				}
				var ids []string
				var cursor string
				for {
					rows, next, err := p.ListSessions(ctx, f.ws, f.dir, q, cursor)
					if err != nil {
						t.Fatal(err)
					}
					for _, row := range rows {
						ids = append(ids, row.SessionID)
					}
					if next == "" {
						break
					}
					if len(ids) > 23 || next == cursor {
						t.Fatal("repeated page", next)
					}
					cursor = next
				}
				if len(ids) != 23 {
					t.Fatal(ids)
				}
				for i, id := range ids {
					if id != fmt.Sprintf("id-%d", i) {
						t.Fatal(ids)
					}
				}
			case "search", "no list", "limit":
				query := "needle"
				if tc == "limit" {
					query = "absent"
				}
				list, cursor, err := p.ListSessions(ctx, f.ws, f.dir, query, "")
				if tc == "no list" {
					if err == nil || err.Error() != "external agent does not support session listing" {
						t.Fatal(err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if tc == "limit" {
					if len(list) != 0 || cursor == "" {
						t.Fatalf("scan limit %v %q", list, cursor)
					}
					return
				}
				ids := []string{}
				for _, entry := range list {
					ids = append(ids, entry.SessionID)
					if entry.UpdatedAt == 0 {
						t.Fatal(entry)
					}
				}
				if !slices.Equal(ids, []string{"id-2", "id-4"}) {
					t.Fatal(ids)
				}
			case "preview", "no load":
				preview, err := p.PreviewSession(ctx, f.ws, defs.ExternalSession{Cwd: f.dir, SessionID: "imported"})
				if tc == "no load" {
					if err == nil {
						t.Fatal("unexpected load")
					}
					return
				}
				if err != nil || preview.FirstUserMessage != "hello world" || preview.LastUserMessage != "latest" {
					t.Fatalf("%+v %v", preview, err)
				}
			case "timeout":
				started := time.Now()
				_, err := p.GetOptions(ctx, externalagent.ExternalAgentOptionsRequest{Workspace: f.ws, Cwd: f.dir})
				if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) < 19*time.Second || time.Since(started) > 25*time.Second {
					t.Fatalf("probe timeout after %s: %v", time.Since(started), err)
				}
			}
		})
	}
}
