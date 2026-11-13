package defs

import (
	"slices"
	"strconv"
	"strings"
)

// ModelDefaults 是模型配置省略字段时使用的值，default 标签给出系统内置默认值。
type ModelDefaults struct {
	ContextWindow          int      `mapstructure:"context_window" yaml:"context_window" default:"256000" json:"context_window"`
	MaxOutputTokens        int      `mapstructure:"max_output_tokens" yaml:"max_output_tokens" default:"32000" json:"max_output_tokens"`
	Thinking               bool     `mapstructure:"thinking" yaml:"thinking" default:"true" json:"thinking"`
	ReasoningEfforts       []string `mapstructure:"reasoning_efforts" yaml:"reasoning_efforts" json:"reasoning_efforts"`
	DefaultReasoningEffort string   `mapstructure:"default_reasoning_effort" yaml:"default_reasoning_effort" json:"default_reasoning_effort"`
	MergeMessageRoles      []string `mapstructure:"merge_message_roles" yaml:"merge_message_roles" json:"merge_message_roles"`
	// Capabilities 是模型配置省略 capabilities 时使用的能力；Input 为空时按 [text]。
	Capabilities ProviderModelCapabilities `mapstructure:"capabilities" yaml:"capabilities" json:"capabilities"`
}

// ProviderConfig 是一个模型服务（models/<id>.yaml）：地址、凭证、默认协议和模型。
type ProviderConfig struct {
	ID string `yaml:"id" json:"id"`
	// Protocol 是模型没写 protocol 时用的协议：openai_chat、openai_responses 或 claude；省略为 openai_responses。
	Protocol string `yaml:"protocol,omitempty" json:"protocol,omitempty"`
	// BaseURL 带版本前缀，例如 https://api.openai.com/v1；各协议默认在后面拼 /chat/completions、/responses、/messages。
	BaseURL string `yaml:"base_url" json:"base_url"`
	// Endpoints 按协议写完整请求地址，写了的协议不再用 BaseURL 拼，可以是别的 host。
	Endpoints map[string]string `yaml:"endpoints,omitempty" json:"endpoints,omitempty"`
	APIKey    string            `yaml:"api_key" json:"api_key"`
	// Headers 是只发给这个 Provider 的 header 模板，同名覆盖 http.headers；值为 "" 表示不发。
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	// Models 是模型 id 到模型配置的映射；值为 nil 时全部用默认。
	Models map[string]*ProviderModelConfig `yaml:"models" json:"models"`
}

// ProviderModelConfig 将本地模型别名映射为上游模型名，给出用途说明和请求参数；省略的请求参数取 config.yaml 的 models.defaults。
type ProviderModelConfig struct {
	// Protocol 省略时用 Provider 的 protocol。
	Protocol      string `yaml:"protocol,omitempty" json:"protocol,omitempty"`
	UpstreamModel string `yaml:"upstream_model,omitempty" json:"upstream_model,omitempty"`
	// DisplayName 省略时用模型 id。
	DisplayName string `yaml:"display_name,omitempty" json:"display_name,omitempty"`
	// Description 是给 Agent 选模型时看的用途说明。
	Description     string `yaml:"description,omitempty" json:"description,omitempty"`
	ContextWindow   int    `yaml:"context_window,omitempty" json:"context_window,omitempty"`
	MaxOutputTokens int    `yaml:"max_output_tokens,omitempty" json:"max_output_tokens,omitempty"`
	Thinking        *bool  `yaml:"thinking,omitempty" json:"thinking,omitempty"`
	// ReasoningEfforts 为 nil 时继承全局默认，[] 表示明确不支持选择。
	ReasoningEfforts       OptionalStringList `yaml:"reasoning_efforts,omitempty" json:"reasoning_efforts,omitzero"`
	DefaultReasoningEffort string             `yaml:"default_reasoning_effort,omitempty" json:"default_reasoning_effort,omitempty"`
	// MergeMessageRoles 为 nil 时继承全局默认，[] 表示明确不合并。
	MergeMessageRoles OptionalStringList `yaml:"merge_message_roles,omitempty" json:"merge_message_roles,omitzero"`
	// ReasoningContext 是请求要求保留的思考上下文，可写 all_turns 或 current_turn；省略时请求不发送、
	// 服务端按默认处理，不从 models.defaults 继承，只用于 openai-responses 协议。
	ReasoningContext string                    `yaml:"reasoning_context,omitempty" json:"reasoning_context,omitempty"`
	Capabilities     ProviderModelCapabilities `yaml:"capabilities,omitempty" json:"capabilities,omitzero"`
}

// ProviderModelCapabilities 是模型配置里的 capabilities 段。
type ProviderModelCapabilities struct {
	// Input 为 nil 时取 config.yaml 的 models.defaults.capabilities.input。
	Input OptionalStringList `mapstructure:"input" yaml:"input,omitempty" json:"input,omitzero"`
}

// GetProviderModelKey 将上游模型 ID 转成配置文件可用的 Key。
func GetProviderModelKey(id string) string {
	// 1. 保留字母、数字、点、下划线和短横线，其余字符替换为短横线。
	return strings.Map(func(char rune) rune {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '.' || char == '_' || char == '-' {
			return char
		}
		return '-'
	}, id)
}

// GetProviderModelKeys 为一组上游模型 ID 分配不重名的配置 Key。
func GetProviderModelKeys(ids []string) map[string]string {
	// 1. 先占用无需替换字符的模型 ID。
	keys := make(map[string]string, len(ids))
	usedKeys := make(map[string]bool, len(ids))
	for _, id := range ids {
		if GetProviderModelKey(id) == id {
			keys[id], usedKeys[id] = id, true
		}
	}
	// 2. 其余 ID 排序后转换，撞名时依次追加 -2、-3；跳过重复 ID。
	for _, id := range slices.Sorted(slices.Values(ids)) {
		if _, found := keys[id]; found {
			continue
		}
		base := GetProviderModelKey(id)
		key := base
		for suffix := 2; usedKeys[key]; suffix++ {
			key = base + "-" + strconv.Itoa(suffix)
		}
		keys[id], usedKeys[key] = key, true
	}
	return keys
}

// OptionalStringList 是可以不写的字符串列表：nil 表示没写、沿用默认，[] 表示明确为空。
// 重新序列化成 YAML 或 JSON 时，nil 不写出这个字段，[] 写出空列表。
type OptionalStringList []string

// IsZero 列表是不是没填？YAML omitempty 和 JSON omitzero 按它省略字段。
func (list OptionalStringList) IsZero() bool {
	// 1. 只有 nil 算没填，空列表算明确填了空。
	return list == nil
}

// CatalogProvider 是模型目录里的一个服务，供引导第 1 步选择；选中后预先填好 ID、协议和 base_url。
type CatalogProvider struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Protocol   string `json:"protocol"`
	BaseURL    string `json:"base_url"`
	ModelCount int    `json:"model_count"`
}
