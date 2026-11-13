package defs

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
)

// ModelRef 是模型目录中一个模型的标识：Provider 文件的 id 加这个文件里的模型 id。
type ModelRef struct {
	ProviderID string `json:"provider_id"`
	ModelID    string `json:"model_id"`
}

// String 返回 provider_id/model_id 形式的模型名称。
func (ref ModelRef) String() string {
	// 1. 用斜杠连接 Provider ID 和模型 ID。
	return ref.ProviderID + "/" + ref.ModelID
}

// ParseModelRef 将 provider_id/model_id 解析为模型标识。
func ParseModelRef(text string) (ModelRef, error) {
	// 1. 按第一个斜杠拆分，缺少斜杠或任一段为空时返回错误。
	providerID, modelID, found := strings.Cut(text, "/")
	if !found || providerID == "" || modelID == "" {
		return ModelRef{}, fmt.Errorf("model %q must be provider_id/model_id", text)
	}
	return ModelRef{
		ProviderID: providerID,
		ModelID:    modelID,
	}, nil
}

// ModelDefinition 是模型目录中的一项：连接参数与请求参数，默认值已解析，构造后只读。
type ModelDefinition struct {
	Ref         ModelRef `json:"ref"`
	DisplayName string   `json:"display_name"`
	// Description 是给 Agent 选模型时看的用途说明。
	Description string `json:"description,omitempty"`
	// Target 是连接参数，含凭证，不序列化。
	Target          *llmkit.Target `json:"-"`
	ContextWindow   int            `json:"context_window"`
	MaxOutputTokens int            `json:"max_output_tokens"`
	ThinkingEnabled bool           `json:"thinking_enabled"`
	// ReasoningEfforts 是可选的推理强度；为空表示不支持选择，请求不带强度。
	ReasoningEfforts       []string `json:"reasoning_efforts"`
	DefaultReasoningEffort string   `json:"default_reasoning_effort,omitempty"`
	// MergeMessageRoles 是发送前需要合并相邻同角色消息的角色；为空表示不合并。
	MergeMessageRoles []llmkit.Role `json:"merge_message_roles,omitempty"`
	// ReasoningContext 是请求要求保留的思考上下文；为空表示请求不发送，服务端按默认处理，只用于 openai-responses 协议。
	ReasoningContext llmkit.ReasoningContext `json:"reasoning_context,omitempty"`
	Capabilities     ModelCapabilities       `json:"capabilities"`
}

// ModelInput 是模型能接收的一种输入。
type ModelInput string

const (
	ModelInputText  ModelInput = "text"
	ModelInputImage ModelInput = "image"
	ModelInputAudio ModelInput = "audio"
	ModelInputVideo ModelInput = "video"
	ModelInputPDF   ModelInput = "pdf"
)

// IsKnown 这是 OpenVaala 认识的输入种类吗？
func (input ModelInput) IsKnown() bool {
	// 1. text、image、audio、video 和 pdf 是认识的种类。
	switch input {
	case ModelInputText, ModelInputImage, ModelInputAudio, ModelInputVideo, ModelInputPDF:
		return true
	}
	return false
}

// ModelCapabilities 是模型的能力；目前只有 Input 有代码在读。
type ModelCapabilities struct {
	// Input 是模型能接收的输入，例如 [text, image]。
	Input []ModelInput `json:"input"`
}

// IsImageInputSupported 这个模型能接收图片吗？
func (model *ModelDefinition) IsImageInputSupported() bool {
	// 1. 输入能力列表里有 image 时能接收图片。
	return slices.Contains(model.Capabilities.Input, ModelInputImage)
}

// IsReasoningEffortSupported 这个模型支持指定推理强度吗？
func (model *ModelDefinition) IsReasoningEffortSupported(effort string) bool {
	// 1. 空强度（用默认强度）和列表里声明过的强度都支持。
	return effort == "" || slices.Contains(model.ReasoningEfforts, effort)
}

// GetReasoningEffort 返回本次请求实际用的推理强度。
func (model *ModelDefinition) GetReasoningEffort(requested string) string {
	// 1. 请求的强度不为空且在支持列表里时用它，否则用默认强度。
	if requested != "" && slices.Contains(model.ReasoningEfforts, requested) {
		return requested
	}
	return model.DefaultReasoningEffort
}

// Clone 深拷贝模型定义。
func (model *ModelDefinition) Clone() *ModelDefinition {
	// 1. nil 返回 nil，否则复制模型字段和连接参数，逐项复制请求头的值。
	if model == nil {
		return nil
	}
	cloned := *model
	if model.Target != nil {
		target := *model.Target
		target.Headers = maps.Clone(model.Target.Headers)
		for key, values := range target.Headers {
			target.Headers[key] = slices.Clone(values)
		}
		cloned.Target = &target
	}
	// 2. 复制推理强度、合并角色和输入能力列表。
	cloned.ReasoningEfforts = slices.Clone(model.ReasoningEfforts)
	cloned.MergeMessageRoles = slices.Clone(model.MergeMessageRoles)
	cloned.Capabilities.Input = slices.Clone(model.Capabilities.Input)
	return &cloned
}

// ClonePublic 返回不含连接参数和凭证的模型副本。
func (model *ModelDefinition) ClonePublic() *ModelDefinition {
	// 1. nil 返回 nil，否则深拷贝后清空 Target。
	if model == nil {
		return nil
	}
	public := model.Clone()
	public.Target = nil
	return public
}

// ModelSelection 是一次模型选择：用哪个模型、用多大的推理强度。
type ModelSelection struct {
	Ref ModelRef `json:"ref"`
	// ReasoningEffort 是选的推理强度；为空时用模型的默认强度。
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

// GetSelectedModel 从模型目录找出选中的模型和实际推理强度；没选模型时用 defaultModel。
func GetSelectedModel(models map[ModelRef]*ModelDefinition, selection ModelSelection, defaultModel ModelRef) (
	model *ModelDefinition, reasoningEffort string, found bool) {
	// 1. 没选模型时使用默认模型。
	ref := selection.Ref
	if ref == (ModelRef{}) {
		ref = defaultModel
	}
	// 2. 查找模型并算出实际推理强度，目录里没有时返回未找到。
	model, found = models[ref]
	if !found {
		return nil, "", false
	}
	return model, model.GetReasoningEffort(selection.ReasoningEffort), true
}

// GetCompactModel 返回本轮压缩使用的模型。
func GetCompactModel(models map[ModelRef]*ModelDefinition, compactModel ModelRef, turnModel *ModelDefinition) *ModelDefinition {
	// 1. 配置的压缩模型在目录里且上下文窗口不小于本轮模型时用它，否则用本轮模型。
	if configured, found := models[compactModel]; found && configured.ContextWindow >= turnModel.ContextWindow {
		return configured
	}
	return turnModel
}
