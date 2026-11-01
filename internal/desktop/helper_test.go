package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/VaalaCat/mygo-kit"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/skill"
	"github.com/VaalaCat/openvaala/pkg/storage"
)

// getTestTranslator 返回指定界面语言的文案查询函数。
func getTestTranslator(language string) func(string) string {
	translations := loadTranslations()[language]
	return func(key string) string { return translations[key] }
}

// TestFormatRelativeTime 检查相对时间按档位和界面语言本地化。
func TestFormatRelativeTime(t *testing.T) {
	now := time.Date(2026, 10, 31, 15, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) int64 { return now.Add(-d).UnixMilli() }
	for _, tt := range []struct {
		name      string
		updatedAt int64
		zh, en    string
	}{
		{"seconds", ago(10 * time.Second), "刚刚", "just now"},
		{"future clock skew", now.Add(time.Minute).UnixMilli(), "刚刚", "just now"},
		{"minutes", ago(3 * time.Minute), "3 分钟前", "3m ago"},
		{"hours", ago(2 * time.Hour), "2 小时前", "2h ago"},
		{"yesterday", ago(30 * time.Hour), "昨天", "yesterday"},
		{"days", ago(3 * 24 * time.Hour), "3 天前", "3d ago"},
		{"weeks", ago(15 * 24 * time.Hour), "2 周前", "2w ago"},
		{"years", ago(800 * 24 * time.Hour), "2 年前", "2y ago"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatRelativeTime(tt.updatedAt, now, getTestTranslator("zh-CN")); got != tt.zh {
				t.Fatalf("zh=%q want %q", got, tt.zh)
			}
			if got := formatRelativeTime(tt.updatedAt, now, getTestTranslator("en")); got != tt.en {
				t.Fatalf("en=%q want %q", got, tt.en)
			}
		})
	}
}

// TestGetErrorMessage 检查已知错误映射成本地化说明，其余落到通用说明。
func TestGetErrorMessage(t *testing.T) {
	d := &Desktop{translations: loadTranslations(), preferences: preferences{Language: "zh-CN"}}
	for _, tt := range []struct {
		name string
		err  error
		want string
	}{
		{"no model", &runtimeclient.ResponseError{Code: -32000, Message: "no model is configured; run openvaala to set up a model provider"}, "还没有模型服务"},
		{"executor not found", &runtimeclient.ResponseError{Code: -32000, Message: `executor "ssh" is not configured; available: local`}, "找不到这个 Executor，请检查工作位置"},
		{"conversation not found", fmt.Errorf("conversation/archive: %w", runtime.ErrConversationNotFound), "对话不存在，可能已在别处被删除"},
		{"session busy", fmt.Errorf("x: %w", session.ErrSessionBusy), "对话正在运行，请稍后再试"},
		{"connection lost", fmt.Errorf("conversation/create: %w", runtimeclient.ErrConnectionLost), "与本机服务的连接已断开"},
		{"timeout", fmt.Errorf("wait: %w", context.DeadlineExceeded), "请求超时"},
		{"unknown", &runtimeclient.ResponseError{Code: -32000, Message: "disk is full"}, "操作失败"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := d.getErrorMessage(tt.err); got != tt.want {
				t.Fatalf("message=%q want %q", got, tt.want)
			}
		})
	}
	d.preferences.Language = "en"
	if got := d.getErrorMessage(runtimeclient.ErrConnectionLost); !strings.Contains(got, "connection") {
		t.Fatalf("english message=%q", got)
	}
}

// TestMarshalJSONView 检查 JSON 视图不转义 HTML 字符，原始 JSON 保留原字节并缩进。
func TestMarshalJSONView(t *testing.T) {
	for _, tt := range []struct {
		name    string
		value   any
		want    string
		wantErr bool
	}{
		{"raw message keeps ampersand", json.RawMessage(`{"command":"echo a && ls <dir>"}`), "{\n  \"command\": \"echo a && ls <dir>\"\n}", false},
		{"struct is not escaped", map[string]string{"text": "a & b < c"}, "{\n  \"text\": \"a & b < c\"\n}", false},
		{"invalid raw message", json.RawMessage(`{"a":`), "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := marshalJSONView(tt.value)
			if (err != nil) != tt.wantErr || string(got) != tt.want {
				t.Fatalf("got %q, %v; want %q, error=%v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

// TestGetFirstStringArgument 检查通用工具摘要按书写顺序取第一个字符串参数。
func TestGetFirstStringArgument(t *testing.T) {
	for _, tt := range []struct{ name, arguments, want string }{
		{"first string", `{"query":"find me","limit":3}`, "find me"},
		{"skips non strings", `{"limit":3,"flags":["a"],"path":"/tmp/x","name":"y"}`, "/tmp/x"},
		{"no string", `{"limit":3}`, ""},
		{"not an object", `[1,2]`, ""},
		{"empty", ``, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := getFirstStringArgument(tt.arguments); got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

// TestGetFacts 检查事实行按给定顺序只保留存在且非空的字段。
func TestGetFacts(t *testing.T) {
	// 1. 缺失和 null 字段不出现，数字与布尔按原值格式化。
	source := map[string]any{"cwd": "/work", "size": float64(42), "eof": true, "missing": nil}
	for _, tt := range []struct {
		name string
		keys []string
		want []kit.Fact
	}{
		{"ordered", []string{"size", "cwd", "eof"}, []kit.Fact{{Label: "size", Value: "42"}, {Label: "cwd", Value: "/work"}, {Label: "eof", Value: "true"}}},
		{"skips absent and null", []string{"missing", "absent", "cwd"}, []kit.Fact{{Label: "cwd", Value: "/work"}}},
		{"none", []string{"absent"}, []kit.Fact{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := getFacts(source, tt.keys...); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("facts=%v want %v", got, tt.want)
			}
		})
	}
}

// TestGetSlashQuery 检查只有光标所在的词以 / 开头时才触发技能补全，并取到光标为止的部分。
func TestGetSlashQuery(t *testing.T) {
	for _, tt := range []struct {
		name      string
		text      string
		caret     int
		wantQuery string
		wantSlash bool
	}{
		{"slash only", "/", 1, "", true},
		{"prefix", "/re", 3, "re", true},
		{"caret inside word", "/review", 3, "re", true},
		{"second word", "check /pd", 9, "pd", true},
		{"after slash word", "/pdf ", 5, "", false},
		{"path is not a trigger", "see a/b", 7, "", false},
		{"absolute path still starts with slash", "/usr/bin", 8, "usr/bin", true},
		{"plain text", "hello", 5, "", false},
		{"chinese before slash", "中文 /re 后文", 6, "re", true},
		{"empty", "", 0, "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			query, isSlash := getSlashQuery(tt.text, tt.caret)
			if query != tt.wantQuery || isSlash != tt.wantSlash {
				t.Fatalf("query=%q isSlash=%v want %q %v", query, isSlash, tt.wantQuery, tt.wantSlash)
			}
		})
	}
}

// TestGetSkillMatches 检查技能按名称或命名空间后的短名前缀匹配，保持目录顺序。
func TestGetSkillMatches(t *testing.T) {
	skills := []skill.SkillInfo{{Name: "pdf-reader"}, {Name: "plugin:review"}, {Name: "Release-Notes"}}
	for _, tt := range []struct {
		name  string
		query string
		want  []string
	}{
		{"all", "", []string{"pdf-reader", "plugin:review", "Release-Notes"}},
		{"prefix", "pd", []string{"pdf-reader"}},
		{"short name after namespace", "rev", []string{"plugin:review"}},
		{"case insensitive", "rel", []string{"Release-Notes"}},
		{"none", "zzz", []string{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := []string{}
			for _, match := range getSkillMatches(skills, tt.query) {
				got = append(got, match.Name)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("matches=%v want %v", got, tt.want)
			}
		})
	}
}

// TestGetSkillCatalogKey 检查已有对话只按对话缓存，草稿按 Agent、Executor 和目录各自缓存。
func TestGetSkillCatalogKey(t *testing.T) {
	for _, tt := range []struct {
		name string
		a, b skillCatalogKey
		same bool
	}{
		{"conversation ignores draft fields", getSkillCatalogKey("c1", "default", "local", "/a"), getSkillCatalogKey("c1", "other", "ssh", "/b"), true},
		{"different conversations", getSkillCatalogKey("c1", "", "", ""), getSkillCatalogKey("c2", "", "", ""), false},
		{"same draft", getSkillCatalogKey("", "default", "local", "/a"), getSkillCatalogKey("", "default", "local", "/a"), true},
		{"draft directory differs", getSkillCatalogKey("", "default", "local", "/a"), getSkillCatalogKey("", "default", "local", "/b"), false},
		{"draft executor differs", getSkillCatalogKey("", "default", "local", "/a"), getSkillCatalogKey("", "default", "sandbox", "/a"), false},
		{"draft agent differs", getSkillCatalogKey("", "default", "local", "/a"), getSkillCatalogKey("", "reviewer", "local", "/a"), false},
		{"draft differs from conversation", getSkillCatalogKey("", "default", "local", ""), getSkillCatalogKey("c1", "default", "local", ""), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if (tt.a == tt.b) != tt.same {
				t.Fatalf("%+v == %+v is %v, want %v", tt.a, tt.b, tt.a == tt.b, tt.same)
			}
		})
	}
}

// TestSplitDirectoryInput 检查目录输入拆成要列出的上级目录和同层名称前缀。
func TestSplitDirectoryInput(t *testing.T) {
	for _, tt := range []struct{ input, wantParent, wantPrefix string }{
		{"", ".", ""},
		{"pro", ".", "pro"},
		{"/home/user/pro", "/home/user/", "pro"},
		{"/home/user/", "/home/user/", ""},
		{"~", "~/", ""},
		{"..", "../", ""},
		{" /tmp/x ", "/tmp/", "x"},
	} {
		t.Run(tt.input, func(t *testing.T) {
			parent, prefix := splitDirectoryInput(tt.input)
			if parent != tt.wantParent || prefix != tt.wantPrefix {
				t.Fatalf("parent=%q prefix=%q want %q %q", parent, prefix, tt.wantParent, tt.wantPrefix)
			}
		})
	}
}

// TestGetWorkLocationLabel 检查按钮文字是 Executor 加目录名，目录名过长时截断。
func TestGetWorkLocationLabel(t *testing.T) {
	for _, tt := range []struct {
		name, executor, directory string
		max                       int
		want                      string
	}{
		{"no directory", "local", "", 16, "local"},
		{"directory name", "local", "/work/project", 16, "local · project"},
		{"truncated", "sandbox", "/work/a-very-long-directory-name", 10, "sandbox · a-very-lo…"},
		{"not truncated at limit", "local", "/work/0123456789", 10, "local · 0123456789"},
		{"unlimited", "local", "/work/a-very-long-directory-name", 0, "local · a-very-long-directory-name"},
		{"chinese name", "local", "/work/中文项目目录名字很长很长", 6, "local · 中文项目目…"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := getWorkLocationLabel(tt.executor, tt.directory, tt.max); got != tt.want {
				t.Fatalf("label=%q want %q", got, tt.want)
			}
		})
	}
}

// TestGetModelOptions 检查模型选项第一项是 Agent 默认，其后按服务分组，分组标题是 Header，每个模型的说明是服务名。
func TestGetModelOptions(t *testing.T) {
	models := []*defs.ModelDefinition{
		{Ref: defs.ModelRef{ProviderID: "a", ModelID: "one"}},
		{Ref: defs.ModelRef{ProviderID: "b", ModelID: "x"}, DisplayName: "X Model"},
		{Ref: defs.ModelRef{ProviderID: "a", ModelID: "two"}, DisplayName: "Two"},
	}
	got := getModelOptions(models, "默认")
	want := []kit.SelectOption{
		{Value: "", Label: "默认"},
		{Label: "a", Header: true},
		{Value: "a/one", Label: "one", Description: "a"},
		{Value: "a/two", Label: "Two", Description: "a"},
		{Label: "b", Header: true},
		{Value: "b/x", Label: "X Model", Description: "b"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("options=%+v want %+v", got, want)
	}
}

// TestGetReasoningEfforts 检查推理强度取所选模型的档位，没选模型时取 Agent 默认模型的。
func TestGetReasoningEfforts(t *testing.T) {
	list := runtime.ModelList{
		AgentDefaultModel: defs.ModelRef{ProviderID: "a", ModelID: "think"},
		Models: []*defs.ModelDefinition{
			{Ref: defs.ModelRef{ProviderID: "a", ModelID: "think"}, ReasoningEfforts: []string{"low", "high"}},
			{Ref: defs.ModelRef{ProviderID: "a", ModelID: "plain"}},
		},
	}
	for _, tt := range []struct {
		name, model string
		list        runtime.ModelList
		want        []string
	}{
		{"selected model", "a/think", list, []string{"low", "high"}},
		{"model without levels", "a/plain", list, nil},
		{"agent default model", "", list, []string{"low", "high"}},
		{"unknown model", "a/gone", list, nil},
		{"no agent default", "", runtime.ModelList{Models: list.Models}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := getReasoningEfforts(tt.list, tt.model); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("efforts=%v want %v", got, tt.want)
			}
		})
	}
}

// TestGetPreviousUserMessage 检查从最近一条往前取自己发的文字消息。
func TestGetPreviousUserMessage(t *testing.T) {
	user := func(text string) viewItem {
		return viewItem{Item: &defs.Item{Kind: defs.ItemMessage, Message: &defs.Message{From: &defs.Address{Kind: defs.IdentityUser}, Content: []*defs.Content{{Kind: defs.ContentText, Text: &defs.TextContent{Text: text}}}}}}
	}
	reply := viewItem{Item: &defs.Item{Kind: defs.ItemMessage, Message: &defs.Message{Content: []*defs.Content{{Kind: defs.ContentText, Text: &defs.TextContent{Text: "assistant"}}}}}}
	items := []viewItem{user("first"), reply, user("second"), reply}
	for _, tt := range []struct {
		offset int
		want   string
		found  bool
	}{{1, "second", true}, {2, "first", true}, {3, "", false}} {
		got, found := getPreviousUserMessage(items, tt.offset)
		if got != tt.want || found != tt.found {
			t.Fatalf("offset %d: got %q %v want %q %v", tt.offset, got, found, tt.want, tt.found)
		}
	}
}

// TestGetDirectoryErrorDetail 检查目录错误只保留 serve 给出的原因。
func TestGetDirectoryErrorDetail(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want string
	}{
		{"prefixed", errors.New("executor/resolveDirectory: invalid executor settings: not a directory: /tmp/x"), "not a directory: /tmp/x"},
		{"other", errors.New("connection lost"), "connection lost"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := getDirectoryErrorDetail(tt.err); got != tt.want {
				t.Fatalf("detail=%q want %q", got, tt.want)
			}
		})
	}
}

// TestGetUniqueTitles 检查同名标题从第二个起追加序号，已带后缀的标题也不会撞名。
func TestGetUniqueTitles(t *testing.T) {
	for _, tt := range []struct {
		name   string
		titles []string
		want   []string
	}{
		{"no titles", nil, []string{}},
		{"all different", []string{"local · a", "local · b"}, []string{"local · a", "local · b"}},
		{"two the same", []string{"local · 001", "local · 001"}, []string{"local · 001", "local · 001 (2)"}},
		{"three the same around another", []string{"x", "y", "x", "x"}, []string{"x", "y", "x (2)", "x (3)"}},
		{"explicit suffix is taken", []string{"x", "x (2)", "x"}, []string{"x", "x (2)", "x (3)"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := getUniqueTitles(tt.titles); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("titles=%q want %q", got, tt.want)
			}
		})
	}
}

// TestGetSessionCrumbs 检查面包屑从主 Agent 排到当前 Session：中间各级用任务名，其次 Agent 名；父链断开或成环时不死循环。
func TestGetSessionCrumbs(t *testing.T) {
	info := func(id, parent, task, agent string) runtime.SessionRuntimeInfo {
		return runtime.SessionRuntimeInfo{Session: defs.Session{ID: defs.SessionID(id), ParentSessionID: defs.SessionID(parent), TaskName: task, AgentName: agent}}
	}
	tree := []runtime.SessionRuntimeInfo{info("root", "", "", "default"), info("a", "root", "review", "worker"), info("b", "a", "", "helper")}
	for _, tt := range []struct {
		name    string
		infos   []runtime.SessionRuntimeInfo
		current defs.SessionID
		want    []string
	}{
		{"root only", tree, "root", []string{"主 Agent"}},
		{"child", tree, "a", []string{"主 Agent", "review"}},
		{"grandchild falls back to agent name", tree, "b", []string{"主 Agent", "review", "helper"}},
		{"unknown current", tree, "missing", []string{}},
		{"broken parent chain starts at the highest known level", []runtime.SessionRuntimeInfo{info("b", "gone", "", "helper")}, "b", []string{"helper"}},
		{"cyclic parents stop", []runtime.SessionRuntimeInfo{info("x", "y", "x-task", ""), info("y", "x", "y-task", "")}, "x", []string{"y-task", "x-task"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			names := []string{}
			for _, crumb := range getSessionCrumbs(tt.infos, "root", tt.current, "主 Agent") {
				names = append(names, crumb.Name)
			}
			if !reflect.DeepEqual(names, tt.want) {
				t.Fatalf("crumbs=%q want %q", names, tt.want)
			}
		})
	}
}

// TestGetTurnInputSummary 检查 Turn 的输入摘要：范围内第一条用户消息优先，排队插入在范围之前时取之前最近的一条，过长截断。
func TestGetTurnInputSummary(t *testing.T) {
	user := func(ordinal storage.Ordinal, text string) viewItem {
		return viewItem{Ordinal: ordinal, Item: &defs.Item{ID: defs.ItemID(fmt.Sprint(ordinal)), Kind: defs.ItemMessage, Message: &defs.Message{From: &defs.Address{Kind: defs.IdentityUser}, Content: []*defs.Content{{Kind: defs.ContentText, Text: &defs.TextContent{Text: text}}}}}}
	}
	assistant := viewItem{Ordinal: 3, Item: &defs.Item{ID: "reply", Kind: defs.ItemMessage, Message: &defs.Message{Content: []*defs.Content{{Kind: defs.ContentText, Text: &defs.TextContent{Text: "回复"}}}}}}
	items := []viewItem{user(1, "第一问\n换行"), assistant, user(4, "第二问"), user(8, "第三问")}
	for _, tt := range []struct {
		name string
		turn defs.Turn
		max  int
		want string
	}{
		{"first turn", defs.Turn{ItemsRange: defs.TurnItemRange{FirstOrdinal: 1, LastOrdinal: 3}}, 60, "第一问 换行"},
		{"second turn", defs.Turn{ItemsRange: defs.TurnItemRange{FirstOrdinal: 4, LastOrdinal: 7}}, 60, "第二问"},
		{"running turn has no last ordinal", defs.Turn{ItemsRange: defs.TurnItemRange{FirstOrdinal: 8}}, 60, "第三问"},
		{"input queued before the turn range", defs.Turn{ItemsRange: defs.TurnItemRange{FirstOrdinal: 5, LastOrdinal: 7}}, 60, "第二问"},
		{"turn without saved items", defs.Turn{}, 60, ""},
		{"truncated", defs.Turn{ItemsRange: defs.TurnItemRange{FirstOrdinal: 1, LastOrdinal: 3}}, 2, "第一…"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := getTurnInputSummary(tt.turn, items, tt.max); got != tt.want {
				t.Fatalf("summary=%q want %q", got, tt.want)
			}
		})
	}
}

// TestGetProtocolLabel 检查协议枚举显示成可读名称，未知协议原样显示。
func TestGetProtocolLabel(t *testing.T) {
	for _, tt := range []struct{ protocol, want string }{
		{"openai_chat", "OpenAI Chat"}, {"openai_responses", "OpenAI Responses"}, {"claude", "Claude"}, {"other", "other"},
	} {
		t.Run(tt.protocol, func(t *testing.T) {
			if got := getProtocolLabel(tt.protocol); got != tt.want {
				t.Fatalf("label = %q, want %q", got, tt.want)
			}
		})
	}
}
