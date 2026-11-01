package defs_test

import (
	"reflect"
	"testing"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/pkg/defs"
)

// TestParseModelRef 检查模型标识解析与重新拼接。
func TestParseModelRef(t *testing.T) {
	// 1. 准备合法和缺失服务或模型 ID 的样例。
	tests := []struct {
		name    string
		text    string
		want    defs.ModelRef
		wantErr bool
	}{
		{
			name: "valid",
			text: "openai/gpt-5",
			want: defs.ModelRef{
				ProviderID: "openai",
				ModelID:    "gpt-5",
			},
		},
		{
			name: "dotted_ids",
			text: "local.v2/my_model-1.5",
			want: defs.ModelRef{
				ProviderID: "local.v2",
				ModelID:    "my_model-1.5",
			},
		},
		{
			name:    "empty",
			text:    "",
			wantErr: true,
		},
		{
			name:    "missing_slash",
			text:    "openai",
			wantErr: true,
		},
		{
			name:    "missing_provider",
			text:    "/gpt-5",
			wantErr: true,
		},
		{
			name:    "missing_model",
			text:    "openai/",
			wantErr: true,
		},
	}
	// 2. 检查解析值、错误和重新拼接结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := defs.ParseModelRef(tt.text)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("ParseModelRef(%q) = %+v, %v; want %+v, error %v", tt.text, got, err, tt.want, tt.wantErr)
			}
			if err == nil && got.String() != tt.text {
				t.Fatalf("String() = %q, want %q", got.String(), tt.text)
			}
		})
	}
}

// TestModelDefinitionReasoningEffort 检查模型推理强度的支持与默认值。
func TestModelDefinitionReasoningEffort(t *testing.T) {
	// 1. 准备带可选强度和无可选强度的模型。
	withEfforts := &defs.ModelDefinition{
		ReasoningEfforts:       []string{"low", "high"},
		DefaultReasoningEffort: "high",
	}
	withoutEfforts := &defs.ModelDefinition{}
	tests := []struct {
		name          string
		model         *defs.ModelDefinition
		requested     string
		wantSupported bool
		wantEffort    string
	}{
		{
			name:          "empty_uses_default",
			model:         withEfforts,
			requested:     "",
			wantSupported: true,
			wantEffort:    "high",
		},
		{
			name:          "supported",
			model:         withEfforts,
			requested:     "low",
			wantSupported: true,
			wantEffort:    "low",
		},
		{
			name:          "unsupported_falls_back",
			model:         withEfforts,
			requested:     "medium",
			wantSupported: false,
			wantEffort:    "high",
		},
		{
			name:          "no_efforts_empty",
			model:         withoutEfforts,
			requested:     "",
			wantSupported: true,
			wantEffort:    "",
		},
		{
			name:          "no_efforts_rejects_value",
			model:         withoutEfforts,
			requested:     "low",
			wantSupported: false,
			wantEffort:    "",
		},
	}
	// 2. 检查强度是否支持及实际返回值。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.model.IsReasoningEffortSupported(tt.requested); got != tt.wantSupported {
				t.Fatalf("IsReasoningEffortSupported(%q) = %v, want %v", tt.requested, got, tt.wantSupported)
			}
			if got := tt.model.GetReasoningEffort(tt.requested); got != tt.wantEffort {
				t.Fatalf("GetReasoningEffort(%q) = %q, want %q", tt.requested, got, tt.wantEffort)
			}
		})
	}
}

// TestModelDefinitionIsImageInputSupported 检查模型是否支持图片输入。
func TestModelDefinitionIsImageInputSupported(t *testing.T) {
	// 1. 准备仅文本、文本图片和空能力列表。
	tests := []struct {
		name  string
		input []defs.ModelInput
		want  bool
	}{
		{
			name:  "text_only",
			input: []defs.ModelInput{defs.ModelInputText},
			want:  false,
		},
		{
			name:  "text_and_image",
			input: []defs.ModelInput{defs.ModelInputText, defs.ModelInputImage},
			want:  true,
		},
		{
			name:  "empty",
			input: nil,
			want:  false,
		},
	}
	// 2. 检查图片输入能力判断。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := &defs.ModelDefinition{Capabilities: defs.ModelCapabilities{Input: tt.input}}
			if got := model.IsImageInputSupported(); got != tt.want {
				t.Fatalf("IsImageInputSupported() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestModelDefinitionClone 检查模型定义副本及公开副本。
func TestModelDefinitionClone(t *testing.T) {
	// 1. 准备带连接凭证、请求头和能力列表的模型。
	original := &defs.ModelDefinition{
		Ref: defs.ModelRef{
			ProviderID: "p",
			ModelID:    "m",
		},
		Target: &llmkit.Target{
			Model:   "upstream",
			APIKey:  "secret",
			Headers: map[string][]string{"X-Key": {"v"}},
		},
		ReasoningEfforts:  []string{"low"},
		MergeMessageRoles: []llmkit.Role{llmkit.RoleUser},
		Capabilities:      defs.ModelCapabilities{Input: []defs.ModelInput{defs.ModelInputText}},
	}
	tests := []struct {
		name  string
		clone func(*defs.ModelDefinition) *defs.ModelDefinition
		check func(t *testing.T, got *defs.ModelDefinition)
	}{
		{
			name:  "clone_is_independent",
			clone: (*defs.ModelDefinition).Clone,
			check: func(t *testing.T, got *defs.ModelDefinition) {
				if !reflect.DeepEqual(got, original) {
					t.Fatalf("Clone() = %+v, want %+v", got, original)
				}
				got.Target.Headers["X-Key"][0] = "changed"
				got.ReasoningEfforts[0] = "changed"
				got.MergeMessageRoles[0] = llmkit.RoleAssistant
				got.Capabilities.Input[0] = defs.ModelInputImage
				if original.Target.Headers["X-Key"][0] != "v" || original.ReasoningEfforts[0] != "low" || original.MergeMessageRoles[0] != llmkit.RoleUser ||
					original.Capabilities.Input[0] != defs.ModelInputText {
					t.Fatal("Clone shares nested values with the original")
				}
			},
		},
		{
			name:  "public_clone_drops_target",
			clone: (*defs.ModelDefinition).ClonePublic,
			check: func(t *testing.T, got *defs.ModelDefinition) {
				if got.Target != nil || got.Ref != original.Ref || original.Target == nil {
					t.Fatalf("ClonePublic() = %+v", got)
				}
			},
		},
		{
			name:  "nil_receiver",
			clone: func(*defs.ModelDefinition) *defs.ModelDefinition { return (*defs.ModelDefinition)(nil).Clone() },
			check: func(t *testing.T, got *defs.ModelDefinition) {
				if got != nil {
					t.Fatalf("nil Clone() = %+v", got)
				}
			},
		},
		{
			name:  "nil_receiver_public",
			clone: func(*defs.ModelDefinition) *defs.ModelDefinition { return (*defs.ModelDefinition)(nil).ClonePublic() },
			check: func(t *testing.T, got *defs.ModelDefinition) {
				if got != nil {
					t.Fatalf("nil ClonePublic() = %+v", got)
				}
			},
		},
	}
	// 2. 检查副本独立、公开副本不含 Target 及 nil 接收者。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { tt.check(t, tt.clone(original)) })
	}
}

// TestGetSelectedModel 检查模型选择、默认模型及缺失模型。
func TestGetSelectedModel(t *testing.T) {
	// 1. 准备模型目录、选择和默认值样例。
	selected := &defs.ModelDefinition{
		Ref: defs.ModelRef{
			ProviderID: "p",
			ModelID:    "selected",
		},
		ReasoningEfforts:       []string{"low", "high"},
		DefaultReasoningEffort: "low",
	}
	agentDefault := &defs.ModelDefinition{Ref: defs.ModelRef{
		ProviderID: "p",
		ModelID:    "default",
	}}
	models := map[defs.ModelRef]*defs.ModelDefinition{selected.Ref: selected, agentDefault.Ref: agentDefault}
	tests := []struct {
		name         string
		selection    defs.ModelSelection
		defaultModel defs.ModelRef
		wantModel    *defs.ModelDefinition
		wantEffort   string
		wantFound    bool
	}{
		{
			name: "selected_with_effort",
			selection: defs.ModelSelection{
				Ref:             selected.Ref,
				ReasoningEffort: "high",
			},
			defaultModel: agentDefault.Ref,
			wantModel:    selected,
			wantEffort:   "high",
			wantFound:    true,
		},
		{
			name:         "selected_default_effort",
			selection:    defs.ModelSelection{Ref: selected.Ref},
			defaultModel: agentDefault.Ref,
			wantModel:    selected,
			wantEffort:   "low",
			wantFound:    true,
		},
		{
			name:         "zero_selection_uses_default",
			selection:    defs.ModelSelection{},
			defaultModel: agentDefault.Ref,
			wantModel:    agentDefault,
			wantFound:    true,
		},
		{
			name: "removed_model_is_not_found",
			selection: defs.ModelSelection{
				Ref: defs.ModelRef{
					ProviderID: "p",
					ModelID:    "gone",
				},
				ReasoningEffort: "high",
			},
			defaultModel: agentDefault.Ref,
		},
		{
			name:         "zero_selection_without_default_is_not_found",
			selection:    defs.ModelSelection{},
			defaultModel: defs.ModelRef{},
		},
		{
			name:      "zero_selection_with_removed_default_is_not_found",
			selection: defs.ModelSelection{},
			defaultModel: defs.ModelRef{
				ProviderID: "p",
				ModelID:    "gone",
			},
		},
	}
	// 2. 检查返回模型、实际强度和是否找到。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model, effort, found := defs.GetSelectedModel(models, tt.selection, tt.defaultModel)
			if model != tt.wantModel || effort != tt.wantEffort || found != tt.wantFound {
				t.Fatalf("GetSelectedModel() = %v, %q, %v; want %v, %q, %v", model, effort, found, tt.wantModel, tt.wantEffort, tt.wantFound)
			}
		})
	}
}

// TestGetCompactModel 检查压缩模型的窗口选择。
func TestGetCompactModel(t *testing.T) {
	// 1. 准备本轮模型及上下文窗口更大、相同、更小的压缩模型。
	turnModel := &defs.ModelDefinition{
		Ref: defs.ModelRef{
			ProviderID: "p",
			ModelID:    "turn",
		},
		ContextWindow: 200_000,
	}
	larger := &defs.ModelDefinition{
		Ref: defs.ModelRef{
			ProviderID: "p",
			ModelID:    "larger",
		},
		ContextWindow: 400_000,
	}
	equal := &defs.ModelDefinition{
		Ref: defs.ModelRef{
			ProviderID: "p",
			ModelID:    "equal",
		},
		ContextWindow: 200_000,
	}
	smaller := &defs.ModelDefinition{
		Ref: defs.ModelRef{
			ProviderID: "p",
			ModelID:    "smaller",
		},
		ContextWindow: 100_000,
	}
	models := map[defs.ModelRef]*defs.ModelDefinition{turnModel.Ref: turnModel, larger.Ref: larger, equal.Ref: equal, smaller.Ref: smaller}
	tests := []struct {
		name         string
		compactModel defs.ModelRef
		want         *defs.ModelDefinition
	}{
		{
			name:         "not_configured_uses_turn_model",
			compactModel: defs.ModelRef{},
			want:         turnModel,
		},
		{
			name:         "larger_compact_model",
			compactModel: larger.Ref,
			want:         larger,
		},
		{
			name:         "equal_window_compact_model",
			compactModel: equal.Ref,
			want:         equal,
		},
		{
			name:         "compact_model_smaller_than_turn_model",
			compactModel: smaller.Ref,
			want:         turnModel,
		},
	}
	// 2. 检查按窗口和目录结果选择压缩模型。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := defs.GetCompactModel(models, tt.compactModel, turnModel); got != tt.want {
				t.Fatalf("GetCompactModel() = %v, want %v", got.Ref, tt.want.Ref)
			}
		})
	}
}
