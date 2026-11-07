package e2e

import (
	"net/http"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"testing"

	"github.com/VaalaCat/openvaala/pkg/defs"
)

// TestHTTPHeaders 检查真实 serve 发给模型服务的请求头：内置默认、全局配置、Provider 配置，以及根 Agent 与子 Agent 各自的 Session 身份。
func TestHTTPHeaders(t *testing.T) {
	// 1. 在 config.yaml 配全局 header，在 Provider 文件配 Provider header，再启动服务。
	model := newE2EModel(t)
	f := &conversationManagementFixture{
		binary: buildOpenVaala(t),
		config: writeE2EConfig(t, model.server.URL),
		model:  model,
	}
	appendE2EConfig(t, f.config, "http:\n  headers:\n    X-Team: infra\n")
	appendE2EConfig(t, filepath.Join(filepath.Dir(f.config), "models", "local.yaml"), "headers:\n  X-Title: MyTool\n")
	startE2EServe(t, f.binary, f.config)
	stopServeOnCleanup(t, f.binary, f.config)
	f.connectControl(t)

	// 2. 根 Agent 委派 review 子 Agent：根 Agent 先收到 spawn_agent 的请求，随后根 Agent 的后续请求和子 Agent 的请求并发到达。
	observer := f.openNewConversation(t)
	observer.startTurn(t, "委派")
	root := f.model.next(t)
	root.reply <- e2eModelReply{
		tool:      "spawn_agent",
		arguments: `{"task_name":"review","message":"check"}`,
	}
	afterSpawn, review := f.model.nextRootAndTask(t, "review")
	afterSpawn.reply <- e2eModelReply{text: "已委派"}
	readTurnFinished(t, observer, defs.TurnStatusCompleted)
	review.reply <- e2eModelReply{text: "子任务完成"}
	f.model.next(t).reply <- e2eModelReply{text: "收到子任务结果"}
	readTurnFinished(t, observer, defs.TurnStatusCompleted)

	// 3. 从对话的 Session 列表找出 review 子 Session。
	conversationID, rootSessionID := string(observer.conversation.ID), string(observer.conversation.RootSessionID)
	sessions := f.listSessions(t, observer.conversation.ID)
	childIndex := slices.IndexFunc(sessions, func(record defs.Session) bool { return record.TaskName == "review" })
	if childIndex < 0 {
		t.Fatalf("sessions = %+v, want the review sub-agent", sessions)
	}
	childSessionID := string(sessions[childIndex].ID)
	for name, id := range map[string]string{"conversation id": conversationID, "root session id": rootSessionID, "review session id": childSessionID} {
		if id == "" {
			t.Fatalf("%s is empty, want a non-empty id", name)
		}
	}
	if childSessionID == rootSessionID {
		t.Fatalf("review session id = root session id %s, want different sessions", rootSessionID)
	}

	// 4. 逐项检查三次模型请求带的请求头。
	exact := func(value string) *regexp.Regexp { return regexp.MustCompile("^" + regexp.QuoteMeta(value) + "$") }
	userAgent := regexp.MustCompile(`^openvaala/\S+ \(` + regexp.QuoteMeta(runtime.GOOS) + `; ` + regexp.QuoteMeta(runtime.GOARCH) + `\)$`)
	for _, test := range []struct {
		name   string
		header http.Header
		key    string
		want   *regexp.Regexp
	}{
		{name: "root session id", header: root.header, key: "Session-Id", want: exact(rootSessionID)},
		{name: "root conversation id", header: root.header, key: "Conversation-Id", want: exact(conversationID)},
		{name: "root user agent", header: root.header, key: "User-Agent", want: userAgent},
		{name: "root provider header", header: root.header, key: "X-Title", want: exact("MyTool")},
		{name: "root global header", header: root.header, key: "X-Team", want: exact("infra")},
		{name: "root after spawn session id", header: afterSpawn.header, key: "Session-Id", want: exact(rootSessionID)},
		{name: "review session id", header: review.header, key: "Session-Id", want: exact(childSessionID)},
		{name: "review conversation id", header: review.header, key: "Conversation-Id", want: exact(conversationID)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := test.header.Get(test.key); !test.want.MatchString(got) {
				t.Fatalf("%s = %q, want to match %s", test.key, got, test.want)
			}
		})
	}
}
