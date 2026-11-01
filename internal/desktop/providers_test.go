package desktop

import (
	"context"
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
