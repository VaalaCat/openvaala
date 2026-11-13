package desktop

import (
	"context"
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/egoist/mygo/ui"
)

type providerSaveForbidden struct{ runtimeclient.Client }

func (providerSaveForbidden) SetProvider(context.Context, defs.ProviderConfig, defs.ModelRef) (runtime.SetDefinitionsResult, error) {
	panic("duplicate or renamed provider must not be submitted")
}

// TestProviderIdentityEditing 检查原生编辑器拒绝重名创建，并将既有服务 ID 设为只读。
func TestProviderIdentityEditing(t *testing.T) {
	// 1. 从实际表单控件输入，不以字段存在替代保存行为。
	for _, edit := range []bool{false, true} {
		name := "duplicate create"
		if edit {
			name = "read only edit"
		}
		t.Run(name, func(t *testing.T) {
			d := testDesktop()
			d.client = providerSaveForbidden{}
			d.page = "providers"
			d.providers = []defs.ProviderConfig{{ID: "existing"}}
			d.providerDraft = providerToDraft(defs.ProviderConfig{ID: "existing", Protocol: "openai_chat"})
			d.providerBaseline = providerToDraft(defs.ProviderConfig{Protocol: "openai_chat"})
			if edit {
				d.providerDraft.EditingID = "existing"
				d.providerBaseline = cloneProviderDraft(d.providerDraft)
			}
			tester := ui.NewTester(d.view, 1280, 820)
			if edit {
				if err := tester.Click("Provider ID"); err != nil {
					t.Fatal(err)
				}
				tester.Key(ui.Cmd, ui.KeyA)
				tester.Type("changed")
				if d.providerDraft.ID != "existing" {
					t.Fatal("existing provider ID changed")
				}
			} else {
				if err := tester.Click("应用"); err != nil {
					t.Fatal(err)
				}
				if !tester.HasText("模型服务 ID 已存在") {
					t.Fatalf("duplicate not rejected: %q", tester.Texts())
				}
			}
		})
	}
}

// TestProviderDraftValues 检查继承、显式 false、空列表与数量原文。
func TestProviderDraftValues(t *testing.T) {
	// 1. 使用具体表单转换验证保存后的公开配置值。
	for _, tt := range []struct {
		name, thinking, quantity string
		inherit                  bool
		wantErr                  bool
	}{{"inherit", "inherit", "256K", true, false}, {"explicit empty", "false", "1.5M", false, false}, {"invalid raw", "true", "oops", false, true}} {
		t.Run(tt.name, func(t *testing.T) {
			draft := providerToDraft(defs.ProviderConfig{ID: "provider", Models: map[string]*defs.ProviderModelConfig{"model": {}}})
			model := draft.Models[0]
			model.Thinking = tt.thinking
			model.ContextWindow = tt.quantity
			model.InheritReasoning = tt.inherit
			model.InheritInputs = tt.inherit
			model.InheritMerge = tt.inherit
			provider, err := draftToProvider(draft, defs.ModelDefaults{})
			if (err != nil) != tt.wantErr {
				t.Fatalf("error=%v", err)
			}
			if model.ContextWindow != tt.quantity {
				t.Fatal("raw input changed")
			}
			if err != nil {
				return
			}
			saved := provider.Models["model"]
			if tt.inherit {
				if saved.Thinking != nil || saved.ReasoningEfforts != nil {
					t.Fatal("inheritance lost")
				}
			} else {
				if saved.Thinking == nil || *saved.Thinking || saved.ReasoningEfforts == nil || len(saved.ReasoningEfforts) != 0 {
					t.Fatalf("explicit empty/false lost: %+v", saved)
				}
			}
		})
	}
}

// TestProviderModelDescription 检查模型描述在设置表单中的编辑和保存。
func TestProviderModelDescription(t *testing.T) {
	// 1. 从已保存配置生成实际控件，编辑后写回完整模型配置。
	for _, tt := range []struct {
		name, before, after string
		edit                bool
	}{
		{
			name: "empty",
		},
		{
			name:   "preserve",
			before: "Fast extraction",
			after:  "Fast extraction",
		},
		{
			name:  "add",
			after: "Careful code review",
			edit:  true,
		},
		{
			name:   "clear",
			before: "Fast extraction",
			edit:   true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			d.navigate("providers")
			d.providerDraft = providerToDraft(defs.ProviderConfig{
				ID: "provider",
				Models: map[string]*defs.ProviderModelConfig{
					"model": {Description: tt.before},
				},
			})
			d.providerBaseline = cloneProviderDraft(d.providerDraft)
			model := d.providerDraft.Models[0]
			tester := ui.NewTester(d.view, 1280, 2400)
			if tt.edit {
				title, found := tester.Find("模型描述")
				if !found {
					t.Fatal("model description row missing")
				}
				// 设置标题在左，输入框在同一行右侧。
				tester.ClickAt(907, title.Y+title.H/2)
				tester.Key(ui.Cmd, ui.KeyA)
				tester.Key(0, ui.KeyBackspace)
				tester.Type(tt.after)
			}
			model.DisplayName = "Renamed"
			provider, err := draftToProvider(d.providerDraft, defs.ModelDefaults{})
			if err != nil {
				t.Fatal(err)
			}
			if got := provider.Models["model"].Description; got != tt.after {
				t.Fatalf("description = %q, want %q", got, tt.after)
			}
		})
	}
}

// TestProviderDraftHeaders 检查请求头行与配置的互相转换：行按名称排序，保存时名称去空白、空名称的行忽略、空值照常保留。
func TestProviderDraftHeaders(t *testing.T) {
	// 1. 从配置生成草稿，追加编辑行后写回，比较保存的 headers。
	for _, tt := range []struct {
		name    string
		headers map[string]string
		added   []*providerHeaderDraft
		want    map[string]string
	}{
		{"round_trip", map[string]string{"X-Title": "MyTool", "Session-Id": ""}, nil, map[string]string{"X-Title": "MyTool", "Session-Id": ""}},
		{"skip_empty_name_trim_name", nil, []*providerHeaderDraft{{Name: "  ", Value: "x"}, {Name: " X-Team ", Value: "infra"}}, map[string]string{"X-Team": "infra"}},
		{"only_blank_rows_omit_headers", nil, []*providerHeaderDraft{{}}, nil},
		{"no_headers", nil, nil, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			draft := providerToDraft(defs.ProviderConfig{ID: "provider", Headers: tt.headers, Models: map[string]*defs.ProviderModelConfig{"model": {}}})
			names := make([]string, len(draft.Headers))
			for index, header := range draft.Headers {
				names[index] = header.Name
			}
			if !slices.IsSorted(names) || len(names) != len(tt.headers) {
				t.Fatalf("header rows = %v, want sorted names of %v", names, tt.headers)
			}
			// 2. 刚载入的草稿与基线逐字段相等，基线是独立的副本。
			baseline := cloneProviderDraft(draft)
			if !reflect.DeepEqual(draft, baseline) {
				t.Fatalf("loaded draft differs from baseline: %+v vs %+v", draft.Headers, baseline.Headers)
			}
			if len(baseline.Headers) > 0 {
				baseline.Headers[0].Value = "changed"
				if draft.Headers[0].Value == "changed" {
					t.Fatal("baseline shares header rows with the draft")
				}
			}
			// 3. 追加编辑行后写回配置。
			draft.Headers = append(draft.Headers, tt.added...)
			provider, err := draftToProvider(draft, defs.ModelDefaults{})
			if err != nil {
				t.Fatal(err)
			}
			if !maps.Equal(provider.Headers, tt.want) || (provider.Headers == nil) != (tt.want == nil) {
				t.Fatalf("headers = %#v, want %#v", provider.Headers, tt.want)
			}
		})
	}
}

// TestProviderHeadersForm 检查服务表单的请求头小节：已有行显示，添加和删除作用在草稿上，名称输入写回对应行。
func TestProviderHeadersForm(t *testing.T) {
	// 1. 每个用例从一条已保存的请求头开始，点击实际控件。
	for _, tt := range []struct {
		name   string
		action func(t *testing.T, tester *ui.Tester)
		want   []providerHeaderDraft
	}{
		{"add", func(t *testing.T, tester *ui.Tester) {
			// 添加行的标题和按钮同名，按钮与删除按钮右对齐、同宽，取删除按钮的横坐标和标题所在行的纵坐标点击。
			title, found := tester.Find("添加请求头")
			remove, foundRemove := tester.Find("删除请求头")
			if !found || !foundRemove {
				t.Fatalf("add or remove header missing in %q", tester.Texts())
			}
			tester.ClickAt(remove.X+remove.W/2, title.Y+title.H/2)
		}, []providerHeaderDraft{{"X-Title", "MyTool"}, {}}},
		{"remove", func(t *testing.T, tester *ui.Tester) {
			if err := tester.Click("删除请求头"); err != nil {
				t.Fatal(err)
			}
		}, nil},
		{"edit_name", func(t *testing.T, tester *ui.Tester) {
			if err := tester.Click("名称"); err != nil {
				t.Fatal(err)
			}
			tester.Key(ui.Cmd, ui.KeyA)
			tester.Type("X-Team")
		}, []providerHeaderDraft{{"X-Team", "MyTool"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			d.navigate("providers")
			d.providerDraft = providerToDraft(defs.ProviderConfig{ID: "sample", Protocol: "openai_chat", Headers: map[string]string{"X-Title": "MyTool"}})
			d.providerBaseline = cloneProviderDraft(d.providerDraft)
			tester := ui.NewTester(d.view, 1280, 1600)
			tester.SetLocale("zh-Hans")
			for _, text := range []string{"请求头", "添加请求头", "值可写 ${OPENVAALA_SESSION_ID} 等变量；值留空表示不发全局同名的 header。"} {
				if !tester.HasText(text) {
					t.Fatalf("missing %q in %q", text, tester.Texts())
				}
			}
			tt.action(t, tester)
			var got []providerHeaderDraft
			for _, header := range d.providerDraft.Headers {
				got = append(got, *header)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("header rows = %+v, want %+v", got, tt.want)
			}
			if !d.isProviderDirty() {
				t.Fatal("editing header rows did not mark the provider dirty")
			}
		})
	}
}

// providerFetchRecorder 记录拉取上游模型时收到的 Provider。
type providerFetchRecorder struct {
	runtimeclient.Client
	provider defs.ProviderConfig
}

func (r *providerFetchRecorder) FetchProviderModels(_ context.Context, provider defs.ProviderConfig) (map[string]*defs.ProviderModelConfig, error) {
	r.provider = provider
	return nil, nil
}

// TestProviderFetchModelsHeaders 检查拉取上游模型用正在编辑的请求头，未保存的修改也生效。
func TestProviderFetchModelsHeaders(t *testing.T) {
	for _, tt := range []struct {
		name    string
		headers []*providerHeaderDraft
		want    map[string]string
	}{
		{"edited_rows", []*providerHeaderDraft{{Name: " X-Title ", Value: "MyTool"}, {Name: "Session-Id"}, {Name: " ", Value: "dropped"}}, map[string]string{"X-Title": "MyTool", "Session-Id": ""}},
		{"no_rows", nil, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			recorder := &providerFetchRecorder{}
			d.client = recorder
			d.providerDraft = providerToDraft(defs.ProviderConfig{ID: "provider", Protocol: "openai_chat", BaseURL: "https://upstream.example/v1"})
			d.providerDraft.Headers = tt.headers
			d.fetchProviderModels()
			d.tasks.Wait()
			if !maps.Equal(recorder.provider.Headers, tt.want) || (recorder.provider.Headers == nil) != (tt.want == nil) {
				t.Fatalf("fetched headers = %#v, want %#v", recorder.provider.Headers, tt.want)
			}
			if recorder.provider.BaseURL != "https://upstream.example/v1" {
				t.Fatalf("fetched provider = %+v", recorder.provider)
			}
		})
	}
}

// TestNumberUnit 检查十进制单位和非法数量。
func TestNumberUnit(t *testing.T) {
	for _, tt := range []struct {
		raw   string
		want  int
		error bool
	}{{"", 0, false}, {"256K", 256000, false}, {"1.5M", 1500000, false}, {"0", 0, false}, {"-1", 0, true}, {"1.5", 0, true}, {"abc", 0, true}} {
		t.Run(tt.raw, func(t *testing.T) {
			got, err := parseNumberUnit(tt.raw)
			if got != tt.want || (err != nil) != tt.error {
				t.Fatalf("got=%d err=%v", got, err)
			}
		})
	}
}

// TestProviderDefaultReference 检查默认模型随稳定行别名变化并支持替代选择。
func TestProviderDefaultReference(t *testing.T) {
	// 1. 本次 Provider 与默认引用一次提交，删除默认模型不能静默改为零值。
	for _, tt := range []struct {
		name, selected, want string
		remove               bool
		wantErr              bool
	}{{"rename", "main", "renamed", false, false}, {"removed", "main", "", true, true}, {"replacement", "replacement", "replacement", true, false}} {
		t.Run(tt.name, func(t *testing.T) {
			draft := providerToDraft(defs.ProviderConfig{ID: "provider", Models: map[string]*defs.ProviderModelConfig{"main": {}}})
			draft.Models[0].Alias = "renamed"
			if tt.remove {
				draft.Models = nil
			}
			addManualModels(draft, "replacement")
			draft.DefaultModelKey = tt.selected
			provider, err := draftToProvider(draft, defs.ModelDefaults{})
			if err != nil {
				t.Fatal(err)
			}
			ref, err := getProviderDefaultModel(draft, defs.ModelRef{ProviderID: "provider", ModelID: "main"})
			if (err != nil) != tt.wantErr {
				t.Fatalf("error=%v", err)
			}
			if err == nil {
				if ref.ModelID != tt.want || provider.Models[ref.ModelID] == nil {
					t.Fatalf("reference=%+v provider=%+v", ref, provider)
				}
			}
		})
	}
}

// TestProviderModelReselection 检查移除后重选目录模型仍保留未保存的完整原文。
func TestProviderModelReselection(t *testing.T) {
	// 1. 使用目录复选框移除和恢复，不把手填值替换为目录默认值。
	for _, raw := range []string{"1.5M", "invalid raw"} {
		t.Run(raw, func(t *testing.T) {
			d := testDesktop()
			d.providerDraft = providerToDraft(defs.ProviderConfig{ID: "provider", Models: map[string]*defs.ProviderModelConfig{"upstream-model": {}}})
			d.providerBaseline = cloneProviderDraft(d.providerDraft)
			d.providerDraft.Models[0].ContextWindow = raw
			d.providerDraft.Models[0].Thinking = "false"
			d.catalogModels = map[string]*defs.ProviderModelConfig{"upstream-model": {ContextWindow: 1024}}
			tester := ui.NewTester(func(c *ui.Context) { d.applyAppearance(c); ui.Scroll(c).Fill().Children(func() { d.providersView(c) }) }, 900, 1200)
			if err := tester.Click("upstream-model"); err != nil {
				t.Fatal(err)
			}
			if len(d.providerDraft.Models) != 0 {
				t.Fatal("model not removed")
			}
			if err := tester.Click("upstream-model"); err != nil {
				t.Fatal(err)
			}
			if len(d.providerDraft.Models) != 1 || d.providerDraft.Models[0].ContextWindow != raw || d.providerDraft.Models[0].Thinking != "false" {
				t.Fatalf("model draft lost: %+v", d.providerDraft.Models)
			}
		})
	}
}

// TestProviderUnsavedNavigation 检查切换服务的放弃确认及取消后原文保留。
func TestProviderUnsavedNavigation(t *testing.T) {
	// 1. 实际点击列表目标与确认按钮，不在原表单上覆盖草稿。
	for _, confirm := range []bool{false, true} {
		name := "cancel"
		if confirm {
			name = "discard"
		}
		t.Run(name, func(t *testing.T) {
			d := testDesktop()
			d.page = "providers"
			d.providers = []defs.ProviderConfig{{ID: "first"}, {ID: "second"}}
			d.openProvider(d.providers[0], "first", "")
			d.providerDraft.BaseURL = "invalid raw url"
			tester := ui.NewTester(d.view, 1280, 820)
			tester.SetLocale("zh-Hans")
			if err := tester.Click("second"); err != nil {
				t.Fatal(err)
			}
			if !tester.HasText("放弃未保存设置？") {
				t.Fatal("no unsaved confirmation")
			}
			label := "取消"
			if confirm {
				label = "放弃"
			}
			if err := tester.Click(label); err != nil {
				t.Fatal(err)
			}
			if confirm {
				if d.providerDraft.ID != "second" {
					t.Fatal("did not navigate")
				}
			} else if d.providerDraft.ID != "first" || d.providerDraft.BaseURL != "invalid raw url" {
				t.Fatal("cancel lost original draft")
			}
		})
	}
}

// TestProvidersPage 检查模型服务页：列表为空时服务目录直接展开；有服务时行上显示协议、模型数量和默认标记，目录默认收起，「服务目录」展开它；协议显示可读名称。
func TestProvidersPage(t *testing.T) {
	catalog := []defs.CatalogProvider{{ID: "compat", Name: "Compat Gateway", Protocol: "openai_chat", BaseURL: "https://compat.example/v1"}}
	for _, tt := range []struct {
		name    string
		prepare func(d *Desktop)
		want    []string
		missing []string
	}{
		{"empty list expands the catalog", func(d *Desktop) {}, []string{"还没有模型服务，从服务目录选一个，或新增服务", "Compat Gateway", "OpenAI Chat"}, nil},
		{"list rows", func(d *Desktop) {
			d.providers = []defs.ProviderConfig{{ID: "local", Protocol: "claude", BaseURL: "http://local/v1", Models: map[string]*defs.ProviderModelConfig{"a": {}, "b": {}}}}
			d.defaultModel = defs.ModelRef{ProviderID: "local", ModelID: "a"}
		}, []string{"local", "默认", "2 个模型", "Claude", "全局默认模型"}, []string{"Compat Gateway"}},
		{"catalog opened by the header button", func(d *Desktop) {
			d.providers = []defs.ProviderConfig{{ID: "local", Protocol: "openai_responses"}}
			d.catalogOpen = true
		}, []string{"local", "OpenAI Responses", "Compat Gateway"}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			d.page = "providers"
			d.catalogProviders, d.catalogRequested = catalog, true
			tt.prepare(d)
			tester := ui.NewTester(d.view, 1280, 820)
			tester.SetLocale("zh-Hans")
			for _, text := range tt.want {
				if !tester.HasText(text) {
					t.Fatalf("missing %q in %q", text, tester.Texts())
				}
			}
			for _, text := range tt.missing {
				if tester.HasText(text) {
					t.Fatalf("unexpected %q in %q", text, tester.Texts())
				}
			}
		})
	}
}

// TestProviderFormSwitches 检查服务表单：继承项是带一句说明的开关，关闭后才出现对应输入；应用和撤销在页头。
func TestProviderFormSwitches(t *testing.T) {
	d := testDesktop()
	d.navigate("providers")
	d.providerDraft = providerToDraft(defs.ProviderConfig{ID: "sample", Protocol: "openai_chat", Models: map[string]*defs.ProviderModelConfig{"model": {}}})
	d.providerBaseline = cloneProviderDraft(d.providerDraft)
	tester := ui.NewTester(d.view, 1280, 1600)
	tester.SetLocale("zh-Hans")
	for _, text := range []string{"继承推理强度列表", "使用全局的推理强度档位", "继承输入类型", "继承消息合并角色", "应用", "撤销", "OpenAI Chat"} {
		if !tester.HasText(text) {
			t.Fatalf("missing %q in %q", text, tester.Texts())
		}
	}
	if tester.HasText("推理强度（逗号分隔，空白表示不支持）") {
		t.Fatal("reasoning efforts input shown while inheriting")
	}
	d.providerDraft.Models[0].InheritReasoning = false
	tester.Frame()
	if !tester.HasText("推理强度（逗号分隔，空白表示不支持）") {
		t.Fatal("reasoning efforts input missing after turning inheritance off")
	}
}
