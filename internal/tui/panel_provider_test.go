package tui

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
)

// TestProviderPanel 检查服务添加引导、编辑、模型选择和默认模型保存。
func TestProviderPanel(t *testing.T) {
	// 1. 准备目录服务、已有网关、Agent 指定模型和面板操作方法。
	thinking := true
	catalog := []defs.CatalogProvider{
		{
			ID:         "openai",
			Name:       "OpenAI",
			Protocol:   "openai_responses",
			BaseURL:    "https://api.openai.com/v1",
			ModelCount: 30,
		},
		{
			ID:         "deepseek",
			Name:       "DeepSeek",
			Protocol:   "openai_chat",
			BaseURL:    "https://api.deepseek.com/v1",
			ModelCount: 3,
		},
	}
	newCatalogModels := func() map[string]*defs.ProviderModelConfig {
		return map[string]*defs.ProviderModelConfig{
			"deepseek-chat": {
				DisplayName:     "DeepSeek Chat",
				ContextWindow:   128000,
				MaxOutputTokens: 8000,
			},
			"deepseek-reasoner": {
				DisplayName:            "DeepSeek Reasoner",
				ContextWindow:          128000,
				MaxOutputTokens:        64000,
				Thinking:               &thinking,
				ReasoningEfforts:       defs.OptionalStringList{"low", "high"},
				DefaultReasoningEffort: "high",
			},
			"deepseek-v4": {DisplayName: "DeepSeek V4"},
		}
	}
	// gateway 是已配置的 Provider：模型自己写了协议和推理强度，claude 协议走另一个地址，写了请求头（含一条空值）；它的 c1 是当前默认模型。
	gateway := defs.ProviderConfig{
		ID:        "gw",
		Protocol:  "openai_chat",
		BaseURL:   "https://gw.example/v1",
		APIKey:    "gw-secret-key",
		Endpoints: map[string]string{"claude": "https://claude.example/v1/messages"},
		Headers:   map[string]string{"X-Team": "infra", "Session-Id": ""},
		Models: map[string]*defs.ProviderModelConfig{
			"c1": {
				Protocol:         "claude",
				DisplayName:      "C1",
				ContextWindow:    200000,
				ReasoningEfforts: defs.OptionalStringList{"high"},
			},
		},
	}
	gatewayModel := defs.ModelRef{
		ProviderID: "gw",
		ModelID:    "c1",
	}
	withoutModels := func(c *fakeClient) {
		c.modelList = runtime.ModelList{}
		c.catalogProviders = catalog
		c.catalogModels = newCatalogModels()
	}
	withGateway := func(c *fakeClient) {
		c.providers, c.providersDefaultModel = []defs.ProviderConfig{gateway}, gatewayModel
		c.modelList = runtime.ModelList{
			Models: []*defs.ModelDefinition{{
				Ref:         gatewayModel,
				DisplayName: "C1",
			}},
			AgentDefaultModel: gatewayModel,
		}
		c.catalogProviders = catalog
		c.fetchedModels = map[string]*defs.ProviderModelConfig{"c1": {DisplayName: "Upstream C1"}, "n1": {}}
	}
	// agentModel 是 TUI 所用 Agent 自己写的 model，和模型配置的默认模型 gw/c1 不同。
	agentModel := defs.ModelRef{
		ProviderID: "own",
		ModelID:    "m",
	}
	withAgentModel := func(c *fakeClient) {
		withGateway(c)
		c.modelList = runtime.ModelList{
			Models:            []*defs.ModelDefinition{{Ref: gatewayModel}, {Ref: agentModel}},
			AgentDefaultModel: agentModel,
		}
	}
	connect := func(h *tuiHarness) {
		h.emit(&runtimeclient.ConnectionChanged{State: runtimeclient.ConnectionConnected})
	}
	// openOnboarding 连上没有模型的 serve，等引导自动打开并列出模型目录。
	openOnboarding := func(h *tuiHarness) {
		connect(h)
		h.waitFor("onboarding catalog listed", func() bool { return strings.Contains(h.view(), "DeepSeek") })
	}
	// fillDeepSeek 在引导里选中 DeepSeek，在 key 一栏粘贴带换行的 key，进入第 3 步并等模型列表出来。
	fillDeepSeek := func(h *tuiHarness) {
		openOnboarding(h)
		h.typeText("deep")
		h.press("enter", "tab", "tab", "tab")
		h.send(tea.PasteMsg{Content: "sk-abc\n"})
		h.press("enter")
		h.waitFor("models listed", func() bool { return strings.Contains(h.view(), "deepseek-reasoner") })
	}
	// openProviderList 连上已有 gw 的 serve，用 /provider 打开第一层，等 Provider 列出来。
	openProviderList := func(h *tuiHarness) {
		connect(h)
		h.waitFor("model list loaded", func() bool { return h.model.agentDefaultModel != (defs.ModelRef{}) })
		h.typeText("/provider")
		h.press("enter")
		h.waitFor("providers listed", func() bool {
			return strings.Contains(h.view(), "+ 添加") && strings.Contains(h.view(), "1 个模型")
		})
	}
	// editGateway 打开第一层，选中 gw 进入编辑表单（第 2 步）。
	editGateway := func(h *tuiHarness) {
		openProviderList(h)
		h.press("down", "enter")
	}
	waitModelsListed := func(h *tuiHarness, modelID string) {
		h.waitFor("models listed", func() bool { return strings.Contains(h.view(), modelID) })
	}
	// openLongModels 在引导里选 DeepSeek 进入第 3 步，断言长 id、长显示名的模型行仍完整显示窗口和输出两列。
	openLongModels := func(t *testing.T, h *tuiHarness) {
		openOnboarding(h)
		h.typeText("deep")
		h.press("enter", "enter")
		waitModelsListed(h, "anthropic")
		assertProviderModelRows(t, h)
	}
	waitClosed := func(h *tuiHarness) {
		h.waitFor("panel closed", func() bool { return h.model.activePanel == nil })
	}
	getProviderWrites := func(h *tuiHarness) []fakeProviderWrite {
		var writes []fakeProviderWrite
		h.client.set(func(c *fakeClient) { writes = slices.Clone(c.providerWrites) })
		return writes
	}
	// getViewLine 返回实时区域里第一行包含 text 的行。
	getViewLine := func(h *tuiHarness, text string) string {
		for line := range strings.SplitSeq(h.view(), "\n") {
			if strings.Contains(line, text) {
				return line
			}
		}
		return ""
	}
	// 2. 列出引导、表单、模型上限、配置保存和删除场景。
	tests := []struct {
		name string
		// width 是终端宽度，0 时为 100。
		width  int
		preset func(c *fakeClient)
		run    func(t *testing.T, h *tuiHarness)
	}{
		{
			name:   "connect_without_models_opens_onboarding_until_dismissed",
			preset: withoutModels,
			run: func(t *testing.T, h *tuiHarness) {
				openOnboarding(h)
				if view := h.view(); !strings.Contains(view, "添加模型服务 1/4") || !strings.Contains(view, "还没有模型，/provider 添加") {
					t.Fatalf("view:\n%s", view)
				}
				h.press("esc")
				if h.model.activePanel != nil {
					t.Fatal("esc did not close the onboarding")
				}
				// 第二次连上时模型目录仍然为空；默认模型只用来确认这次结果已经处理完。
				reconnected := defs.ModelRef{
					ProviderID: "later",
					ModelID:    "check",
				}
				h.client.set(func(c *fakeClient) { c.modelList = runtime.ModelList{AgentDefaultModel: reconnected} })
				connect(h)
				h.waitFor("second model list handled", func() bool { return h.model.agentDefaultModel == reconnected })
				if h.model.activePanel != nil || !strings.Contains(h.view(), "还没有模型，/provider 添加") {
					t.Fatalf("panel = %v, view:\n%s", h.model.activePanel, h.view())
				}
			},
		},
		{
			name: "connect_with_models_does_not_open_onboarding",
			run: func(t *testing.T, h *tuiHarness) {
				connect(h)
				h.waitFor("model list handled", func() bool { return len(h.model.modelDisplayNames) > 0 })
				if h.model.activePanel != nil || strings.Contains(h.view(), "还没有模型") {
					t.Fatalf("panel = %v, view:\n%s", h.model.activePanel, h.view())
				}
			},
		},
		{
			name:   "catalog_lists_custom_first_and_prefills_the_form",
			preset: withoutModels,
			run: func(t *testing.T, h *tuiHarness) {
				openOnboarding(h)
				rows := getPanelBodyLines(h.view())
				if len(rows) < 4 || !strings.HasPrefix(rows[1], "› 自定义") || !strings.HasPrefix(rows[2], "DeepSeek") || !strings.HasPrefix(rows[3], "OpenAI") {
					t.Fatalf("rows = %q", rows)
				}
				h.typeText("deep")
				rows = getPanelBodyLines(h.view())
				if len(rows) != 2 || rows[0] != "搜索：deep▏" || !strings.HasPrefix(rows[1], "› DeepSeek") {
					t.Fatalf("filtered rows = %q", rows)
				}
				h.press("enter")
				view := h.view()
				for _, want := range []string{"添加模型服务 2/4", "deepseek▏", "‹ openai_chat ›", "https://api.deepseek.com/v1"} {
					if !strings.Contains(view, want) {
						t.Fatalf("form misses %q:\n%s", want, view)
					}
				}
				h.press("tab", "right")
				if !strings.Contains(h.view(), "‹ openai_responses ›") {
					t.Fatalf("protocol line = %q", getViewLine(h, "协议"))
				}
			},
		},
		{
			name: "catalog_failure_leaves_only_custom",
			preset: func(c *fakeClient) {
				withoutModels(c)
				c.catalogProvidersErr = errors.New("models.dev unreachable")
			},
			run: func(t *testing.T, h *tuiHarness) {
				connect(h)
				h.waitFor("catalog failure shown", func() bool { return strings.Contains(h.view(), "models.dev unreachable") })
				rows := getPanelBodyLines(h.view())
				if strings.Contains(h.view(), "DeepSeek") || !slices.ContainsFunc(rows, func(row string) bool { return strings.HasPrefix(row, "› 自定义") }) {
					t.Fatalf("rows = %q", rows)
				}
			},
		},
		{
			// 目录还在加载时选了「自定义」再返回：没拿到目录也没有失败原因，重新拉取。
			name: "returning_to_catalog_refetches_when_nothing_arrived",
			preset: func(c *fakeClient) {
				withoutModels(c)
				c.catalogGate = make(chan struct{})
			},
			run: func(t *testing.T, h *tuiHarness) {
				connect(h)
				h.waitFor("onboarding opened", func() bool { return strings.Contains(h.view(), "正在获取模型目录") })
				h.press("enter")
				if !strings.Contains(h.view(), "添加模型服务 2/4") {
					t.Fatalf("custom was not chosen while loading:\n%s", h.view())
				}
				h.press("esc")
				h.client.set(func(c *fakeClient) { close(c.catalogGate) })
				h.waitFor("catalog listed", func() bool { return strings.Contains(h.view(), "DeepSeek") })
				if calls := h.client.getCalls("ListCatalogProviders"); len(calls) != 2 {
					t.Fatalf("catalog calls = %v", calls)
				}
			},
		},
		{
			// 只选一个模型且没有默认模型时直接保存并设为默认；粘贴 key 去掉换行，界面只显示末四位。
			name:   "single_model_skips_default_step_and_saves_trimmed_key",
			preset: withoutModels,
			run: func(t *testing.T, h *tuiHarness) {
				openOnboarding(h)
				h.typeText("deep")
				h.press("enter", "tab", "tab", "tab")
				h.send(tea.PasteMsg{Content: "sk-abc\n"})
				if view := h.view(); !strings.Contains(view, "…-abc") || strings.Contains(view, "sk-abc") {
					t.Fatalf("key line = %q", getViewLine(h, "API key"))
				}
				h.press("enter")
				waitModelsListed(h, "deepseek-reasoner")
				h.press("space", "enter")
				waitClosed(h)
				writes := getProviderWrites(h)
				if len(writes) != 1 || writes[0].provider.APIKey != "sk-abc" || !slices.Equal(slices.Sorted(maps.Keys(writes[0].provider.Models)), []string{"deepseek-chat"}) ||
					writes[0].defaultModel != (defs.ModelRef{
						ProviderID: "deepseek",
						ModelID:    "deepseek-chat",
					}) {
					t.Fatalf("provider writes = %+v", writes)
				}
				if calls := h.client.getCalls("SetDefaultModel"); len(calls) != 0 {
					t.Fatalf("default model calls = %v", calls)
				}
				if h.model.notice.text != "已保存 deepseek" {
					t.Fatalf("notice = %q", h.model.notice.text)
				}
			},
		},
		{
			// 打字（包括 e）都进过滤词，换过滤词保留勾选；Ctrl+E 改上限后该模型自动勾上；输入不是正整数时停在编辑层。
			name:   "model_step_keeps_checks_and_edits_limits",
			preset: withoutModels,
			run: func(t *testing.T, h *tuiHarness) {
				fillDeepSeek(h)
				h.typeText("chat")
				h.press("space", "backspace", "backspace", "backspace", "backspace")
				h.typeText("ek-v4")
				if line := getViewLine(h, "deepseek-v4"); !strings.Contains(line, "○ deepseek-v4") || strings.Count(line, "—") != 2 || strings.Contains(h.view(), "deepseek-chat") {
					t.Fatalf("v4 line = %q, view:\n%s", line, h.view())
				}
				h.press("backspace", "backspace", "backspace", "backspace", "backspace")
				if line := getViewLine(h, "deepseek-chat"); !strings.Contains(line, "● deepseek-chat") {
					t.Fatalf("chat line after changing the filter = %q", line)
				}
				h.press("down", "down", "ctrl+e")
				h.typeText("400000")
				h.press("enter")
				if line := getViewLine(h, "deepseek-v4"); !strings.Contains(line, "● deepseek-v4") || !strings.Contains(line, "400K") {
					t.Fatalf("edited v4 line = %q", line)
				}
				h.press("ctrl+e")
				h.typeText("abc")
				h.press("enter")
				if view := h.view(); !strings.Contains(view, "只能填正整数") || !strings.Contains(view, "上下文窗口") {
					t.Fatalf("invalid limit view:\n%s", view)
				}
				h.press("esc")
				if line := getViewLine(h, "deepseek-v4"); !strings.Contains(line, "400K") {
					t.Fatalf("v4 line after cancel = %q", line)
				}
			},
		},
		{
			name:  "model_columns_fit_80_columns",
			width: 80,
			preset: func(c *fakeClient) {
				withoutModels(c)
				c.catalogModels = map[string]*defs.ProviderModelConfig{
					"anthropic/claude-sonnet-4.5-20250929-extended-thinking": {
						DisplayName:     "Claude Sonnet 4.5 (20250929) Extended Thinking",
						ContextWindow:   200000,
						MaxOutputTokens: 64000,
					},
					"x": {},
				}
			},
			run: openLongModels,
		},
		{
			name:  "model_columns_fit_40_columns",
			width: 40,
			preset: func(c *fakeClient) {
				withoutModels(c)
				c.catalogModels = map[string]*defs.ProviderModelConfig{
					"anthropic/claude-sonnet-4.5-20250929-extended-thinking": {
						DisplayName:     "Claude Sonnet 4.5 (20250929) Extended Thinking",
						ContextWindow:   200000,
						MaxOutputTokens: 64000,
					},
					"x": {},
				}
			},
			run: openLongModels,
		},
		{
			name: "catalog_models_failure_switches_to_manual_ids",
			preset: func(c *fakeClient) {
				withoutModels(c)
				c.catalogModelsErr = errors.New("catalog models down")
			},
			run: func(t *testing.T, h *tuiHarness) {
				openOnboarding(h)
				h.typeText("deep")
				h.press("enter", "enter")
				h.waitFor("manual input shown", func() bool { return strings.Contains(h.view(), "catalog models down") })
				h.typeText("m1, m2,")
				h.press("enter")
				if view := h.view(); !strings.Contains(view, "添加模型服务 4/4") || !strings.Contains(view, "deepseek/m1") || !strings.Contains(view, "deepseek/m2") {
					t.Fatalf("view:\n%s", view)
				}
			},
		},
		{
			// 手动输入的 id 按模型目录的规则换成 key：换过的写 upstream_model，换完撞名的加 -2。
			name: "custom_fetch_failure_takes_manual_ids_as_valid_keys",
			preset: func(c *fakeClient) {
				withoutModels(c)
				c.fetchModelsErr = errors.New("connection refused")
			},
			run: func(t *testing.T, h *tuiHarness) {
				openOnboarding(h)
				h.press("enter")
				h.typeText("gw")
				h.press("tab", "tab")
				h.typeText("http://gw.local/v1")
				h.press("enter")
				h.waitFor("manual input shown", func() bool { return strings.Contains(h.view(), "connection refused") })
				if calls := h.client.getCalls("FetchProviderModels"); !slices.Equal(calls, []string{"FetchProviderModels gw openai_chat http://gw.local/v1"}) {
					t.Fatalf("fetch calls = %v", calls)
				}
				h.typeText(" a/b ,a:b, m1")
				h.press("enter")
				for _, want := range []string{"添加模型服务 4/4", "gw/a-b", "gw/a-b-2", "gw/m1"} {
					if !strings.Contains(h.view(), want) {
						t.Fatalf("default step misses %q:\n%s", want, h.view())
					}
				}
				h.press("enter")
				waitClosed(h)
				wantModels := map[string]*defs.ProviderModelConfig{"a-b": {UpstreamModel: "a/b"}, "a-b-2": {UpstreamModel: "a:b"}, "m1": {}}
				if writes := getProviderWrites(h); len(writes) != 1 || !reflect.DeepEqual(writes[0].provider.Models, wantModels) || writes[0].defaultModel.String() != "gw/a-b" {
					t.Fatalf("provider writes = %+v", writes)
				}
			},
		},
		{
			name:   "complete_writes_checked_models_and_default_once",
			preset: withoutModels,
			run: func(t *testing.T, h *tuiHarness) {
				fillDeepSeek(h)
				h.press("space", "down", "ctrl+e", "backspace", "backspace", "backspace", "backspace", "backspace", "backspace")
				h.typeText("400000")
				h.press("enter", "enter")
				if view := h.view(); !strings.Contains(view, "添加模型服务 4/4") || strings.Contains(view, "保持当前默认模型") {
					t.Fatalf("default step:\n%s", view)
				}
				h.press("down", "enter")
				waitClosed(h)
				reasoner := *newCatalogModels()["deepseek-reasoner"]
				reasoner.ContextWindow = 400000
				want := fakeProviderWrite{
					provider: defs.ProviderConfig{
						ID:       "deepseek",
						Protocol: "openai_chat",
						BaseURL:  "https://api.deepseek.com/v1",
						APIKey:   "sk-abc",
						Models:   map[string]*defs.ProviderModelConfig{"deepseek-chat": newCatalogModels()["deepseek-chat"], "deepseek-reasoner": &reasoner},
					},
					defaultModel: defs.ModelRef{
						ProviderID: "deepseek",
						ModelID:    "deepseek-reasoner",
					},
				}
				if writes := getProviderWrites(h); len(writes) != 1 || !reflect.DeepEqual(writes[0], want) {
					t.Fatalf("provider writes = %+v, want %+v", writes, want)
				}
				if calls := h.client.getCalls("SetProvider", "SetDefaultModel"); !slices.Equal(calls, []string{"SetProvider deepseek deepseek/deepseek-reasoner"}) {
					t.Fatalf("calls = %v", calls)
				}
				if h.model.notice.text != "已保存 deepseek" {
					t.Fatalf("notice = %q", h.model.notice.text)
				}
			},
		},
		{
			// 编辑已有 Provider：已有模型都已勾选，模型自己的协议、推理强度和 Provider 的 endpoints、headers 原样写回；key 留空沿用原 key；
			// 第 4 步第一项是保持当前默认模型，写入时带上它。
			name:   "edit_keeps_model_fields_endpoints_headers_and_key",
			preset: withGateway,
			run: func(t *testing.T, h *tuiHarness) {
				openProviderList(h)
				if line := getViewLine(h, "默认模型"); !strings.Contains(line, "gw/c1") {
					t.Fatalf("default line = %q", line)
				}
				h.press("down", "enter")
				if view := h.view(); !strings.Contains(view, "编辑模型服务 2/4") || !strings.Contains(view, "留空沿用（…-key）") {
					t.Fatalf("edit form:\n%s", view)
				}
				h.press("enter")
				waitModelsListed(h, "n1")
				if line := getViewLine(h, " c1 "); !strings.Contains(line, "● c1") || !strings.Contains(line, "200K") {
					t.Fatalf("c1 line = %q", line)
				}
				h.press("enter")
				if !strings.Contains(h.view(), "保持当前默认模型 (gw/c1)") {
					t.Fatalf("default step:\n%s", h.view())
				}
				h.press("enter")
				waitClosed(h)
				want := fakeProviderWrite{
					provider:     gateway,
					defaultModel: gatewayModel,
				}
				if writes := getProviderWrites(h); len(writes) != 1 || !reflect.DeepEqual(writes[0], want) {
					t.Fatalf("provider writes = %+v, want %+v", writes, want)
				}
				if calls := h.client.getCalls("SetDefaultModel"); len(calls) != 0 {
					t.Fatalf("default model calls = %v", calls)
				}
			},
		},
		{
			name:   "edit_key_hint_fits_40_columns",
			width:  40,
			preset: withGateway,
			run: func(t *testing.T, h *tuiHarness) {
				editGateway(h)
				if line := getViewLine(h, "API key"); !strings.Contains(line, "留空沿用（…-key）") {
					t.Fatalf("key line = %q", line)
				}
			},
		},
		{
			name:   "edit_with_pasted_key_replaces_the_old_key",
			preset: withGateway,
			run: func(t *testing.T, h *tuiHarness) {
				editGateway(h)
				h.press("tab", "tab")
				h.typeText("typed")
				h.send(tea.PasteMsg{Content: " sk-new\n"})
				if line := getViewLine(h, "API key"); !strings.Contains(line, "…-new") || strings.Contains(line, "留空沿用") {
					t.Fatalf("key line = %q", line)
				}
				h.press("enter")
				waitModelsListed(h, "n1")
				h.press("enter", "enter")
				waitClosed(h)
				if writes := getProviderWrites(h); len(writes) != 1 || writes[0].provider.APIKey != "sk-new" {
					t.Fatalf("provider writes = %+v", writes)
				}
			},
		},
		{
			// 去掉当前默认模型 c1、改勾 n1：不再提供「保持当前默认模型」，只剩 n1 一项时跳过第 4 步，同一次写入把默认模型换成 n1。
			name:   "edit_dropping_the_default_model_pins_the_new_one",
			preset: withGateway,
			run: func(t *testing.T, h *tuiHarness) {
				editGateway(h)
				h.press("enter")
				waitModelsListed(h, "n1")
				h.press("space", "down", "space", "enter")
				waitClosed(h)
				writes := getProviderWrites(h)
				if len(writes) != 1 || !slices.Equal(slices.Sorted(maps.Keys(writes[0].provider.Models)), []string{"n1"}) || writes[0].defaultModel.String() != "gw/n1" {
					t.Fatalf("provider writes = %+v", writes)
				}
			},
		},
		{
			name:   "add_with_an_existing_id_stays_on_the_form",
			preset: withGateway,
			run: func(t *testing.T, h *tuiHarness) {
				openProviderList(h)
				h.press("down", "down", "enter")
				h.waitFor("catalog listed", func() bool { return strings.Contains(h.view(), "DeepSeek") })
				h.press("enter")
				h.typeText("gw")
				h.press("tab", "tab")
				h.typeText("http://gw.local/v1")
				h.press("enter")
				if view := h.view(); !strings.Contains(view, "已有 gw，请换一个 ID，或在列表里编辑它") || !strings.Contains(view, "添加模型服务 2/4") {
					t.Fatalf("view:\n%s", view)
				}
				if calls := h.client.getCalls("FetchProviderModels", "SetProvider"); len(calls) != 0 {
					t.Fatalf("calls = %v", calls)
				}
			},
		},
		{
			// Agent 自己写了 model：第一层「默认模型」一行和第 4 步「保持当前默认模型」都是模型配置的默认模型 gw/c1，不是 Agent 的；
			// 保存后通知说明新对话仍用 Agent 指定的模型。
			name:   "keep_option_pins_models_default_when_agent_has_own_model",
			preset: withAgentModel,
			run: func(t *testing.T, h *tuiHarness) {
				openProviderList(h)
				if line := getViewLine(h, "默认模型"); !strings.Contains(line, "gw/c1") || strings.Contains(line, "own/m") {
					t.Fatalf("default line = %q", line)
				}
				h.press("down", "enter", "enter")
				waitModelsListed(h, "n1")
				h.press("enter")
				if !strings.Contains(h.view(), "保持当前默认模型 (gw/c1)") {
					t.Fatalf("default step:\n%s", h.view())
				}
				h.press("enter")
				waitClosed(h)
				if writes := getProviderWrites(h); len(writes) != 1 || writes[0].defaultModel != gatewayModel {
					t.Fatalf("provider writes = %+v", writes)
				}
				if want := "已保存 gw；新对话仍用 Agent 指定的 own/m"; h.model.notice.text != want {
					t.Fatalf("notice = %q, want %q", h.model.notice.text, want)
				}
			},
		},
		{
			name:   "default_model_row_marks_models_default_and_notes_agent_model",
			preset: withAgentModel,
			run: func(t *testing.T, h *tuiHarness) {
				openProviderList(h)
				h.press("enter")
				if line := getViewLine(h, "gw/c1"); !strings.Contains(line, "● gw/c1") {
					t.Fatalf("chooser line = %q, view:\n%s", line, h.view())
				}
				h.press("enter")
				want := "默认模型：gw/c1；新对话仍用 Agent 指定的 own/m"
				h.waitFor("default model notice", func() bool { return h.model.notice.text == want })
				if calls := h.client.getCalls("SetDefaultModel"); !slices.Equal(calls, []string{"SetDefaultModel gw/c1"}) {
					t.Fatalf("calls = %v", calls)
				}
			},
		},
		{
			// 引导直接打开时也先读 provider/list：已有 Provider 文件（还没有模型）时，添加同 ID 停在第 2 步。
			name: "onboarding_rejects_an_existing_id",
			preset: func(c *fakeClient) {
				withoutModels(c)
				c.providers = []defs.ProviderConfig{{
					ID:      "gw",
					BaseURL: "https://gw.example/v1",
				}}
			},
			run: func(t *testing.T, h *tuiHarness) {
				openOnboarding(h)
				h.press("enter")
				h.typeText("gw")
				h.press("tab", "tab")
				h.typeText("http://gw.local/v1")
				h.press("enter")
				if view := h.view(); !strings.Contains(view, "已有 gw，请换一个 ID，或在列表里编辑它") || !strings.Contains(view, "添加模型服务 2/4") {
					t.Fatalf("view:\n%s", view)
				}
				if calls := h.client.getCalls("FetchProviderModels", "SetProvider"); len(calls) != 0 {
					t.Fatalf("calls = %v", calls)
				}
			},
		},
		{
			name: "delete_needs_a_second_press_and_shows_the_error",
			preset: func(c *fakeClient) {
				withGateway(c)
				// 和真的 Client 一样，serve 的 JSON-RPC 错误带着方法名和错误码；界面只显示服务端原文。
				c.configWriteErr = fmt.Errorf("provider/delete: %w", &runtimeclient.ResponseError{
					Code:    -32000,
					Message: `agent "coder": model gw/c1 is not configured`,
				})
			},
			run: func(t *testing.T, h *tuiHarness) {
				openProviderList(h)
				h.press("down", "d")
				if !strings.Contains(h.view(), "再按 d 删除 gw") || len(h.client.getCalls("DeleteProvider")) != 0 {
					t.Fatalf("calls = %v, view:\n%s", h.client.getCalls(""), h.view())
				}
				h.press("d")
				want := `删除失败：agent "coder": model gw/c1 is not configured`
				h.waitFor("delete failure shown", func() bool { return h.model.notice.text == want })
				if calls := h.client.getCalls("DeleteProvider"); !slices.Equal(calls, []string{"DeleteProvider gw"}) || !strings.Contains(h.view(), want) {
					t.Fatalf("calls = %v, view:\n%s", calls, h.view())
				}
			},
		},
	}
	// 3. 每个场景按预设配置客户端，在一个新的 TUI 里运行（宽度默认 100，高度 40）。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var presets []func(c *fakeClient)
			if tt.preset != nil {
				presets = append(presets, tt.preset)
			}
			width := tt.width
			if width == 0 {
				width = 100
			}
			tt.run(t, newTUIHarness(t, width, 40, presets...))
		})
	}
}

// assertProviderModelRows 检查模型行完整显示窗口和输出上限列。
func assertProviderModelRows(t *testing.T, h *tuiHarness) {
	// 1. 收集模型选择行，再比较长 ID 模型和无上限模型的两列文字。
	t.Helper()
	var rows []string
	for _, row := range getPanelBodyLines(h.view()) {
		if strings.Contains(row, "● ") || strings.Contains(row, "○ ") {
			rows = append(rows, row)
		}
	}
	if len(rows) != 2 || !strings.HasSuffix(rows[0], "200K     64K") || !strings.HasSuffix(rows[1], "—       —") {
		t.Fatalf("model rows = %q, view:\n%s", rows, h.view())
	}
}
