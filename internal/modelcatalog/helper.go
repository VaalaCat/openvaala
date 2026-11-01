package modelcatalog

import (
	"cmp"
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/pkg/defs"
)

const (
	// anthropicVersion 是 claude 协议请求必带的版本头。
	anthropicVersion = "2023-06-01"
	// claudeModelsPageLimit 是 claude 协议 /models 一页取的个数，Anthropic 允许的最大值。
	claudeModelsPageLimit = 1000
)

// builtinBaseURLs 是 models.dev 里没有 api 的两个服务的 base_url。
var builtinBaseURLs = map[string]string{
	"openai":    "https://api.openai.com/v1",
	"anthropic": "https://api.anthropic.com/v1",
}

// getProtocolByNPM 返回 SDK 包对应的模型协议，以及是否支持。
func getProtocolByNPM(npm string) (protocol string, supported bool) {
	// 1. 匹配 OpenAI Responses、Claude 或 OpenAI Chat，其他包不支持。
	switch npm {
	case "@ai-sdk/openai":
		return string(llmkit.ProtocolOpenAIResponses), true
	case "@ai-sdk/anthropic":
		return string(llmkit.ProtocolClaude), true
	case "@ai-sdk/openai-compatible":
		return string(llmkit.ProtocolOpenAIChat), true
	default:
		return "", false
	}
}

// getCatalogEndpoint 返回 models.dev 服务的协议、base_url，以及能不能列出。
func getCatalogEndpoint(providerID string, provider modelsDevProvider) (protocol, baseURL string, listed bool) {
	// 1. 按 SDK 包匹配协议，匹配不上时不列出。
	protocol, supported := getProtocolByNPM(provider.NPM)
	if !supported {
		return "", "", false
	}
	// 2. 优先用服务自己的地址，OpenAI 和 Anthropic 可以用内置地址；没有地址或地址含 ${ 占位符时不列出。
	baseURL = cmp.Or(provider.API, builtinBaseURLs[providerID])
	return protocol, baseURL, baseURL != "" && !strings.Contains(baseURL, "${")
}

// getModelProtocol 返回要写进模型配置的协议（和服务协议相同时为空），以及是否支持。
func getModelProtocol(providerProtocol string, model *modelsDevModel) (protocol string, supported bool) {
	// 1. 模型没写 SDK 时沿用服务协议，否则匹配模型自己的协议。
	if model.Provider.NPM == "" {
		return "", true
	}
	protocol, supported = getProtocolByNPM(model.Provider.NPM)
	// 2. 和服务协议相同时留空，匹配不上时不支持。
	if !supported || protocol == providerProtocol {
		return "", supported
	}
	return protocol, true
}

// newProviderModelConfig 把 models.dev 的模型转成模型配置字段。
func newProviderModelConfig(model *modelsDevModel) *defs.ProviderModelConfig {
	// 1. 填入显示名、窗口、输出上限和推理开关。
	config := &defs.ProviderModelConfig{
		DisplayName:     model.Name,
		ContextWindow:   model.Limit.Context,
		MaxOutputTokens: model.Limit.Output,
	}
	if model.Reasoning != nil {
		thinking := *model.Reasoning
		config.Thinking = &thinking
	}
	// 2. 按原顺序保留认识的输入类型：text、image、audio、video、pdf。
	for _, input := range model.Modalities.Input {
		if defs.ModelInput(input).IsKnown() {
			config.Capabilities.Input = append(config.Capabilities.Input, input)
		}
	}
	// 3. 取第一个 effort 设置，去掉 null 和空字符串，再定默认强度。
	for _, option := range model.ReasoningOptions {
		if option.Type != "effort" {
			continue
		}
		var efforts defs.OptionalStringList
		for _, value := range option.Values {
			if effort, isString := value.(string); isString && effort != "" {
				efforts = append(efforts, effort)
			}
		}
		if len(efforts) > 0 {
			config.ReasoningEfforts = efforts
			config.DefaultReasoningEffort = getDefaultReasoningEffort(efforts)
		}
		break
	}
	return config
}

// getDefaultReasoningEffort 从非空的推理强度列表里选默认值。
func getDefaultReasoningEffort(efforts []string) string {
	// 1. 有 medium 就选它，否则选中间一项。
	if slices.Contains(efforts, "medium") {
		return "medium"
	}
	return efforts[len(efforts)/2]
}

// getMajorityEntry 从同名模型里选一条：按窗口和输出上限分组，取最优先的那组里按服务 ID 排第一的。
func getMajorityEntry(entries []catalogModelEntry) catalogModelEntry {
	// 1. 统计每组上限有多少个服务。
	votes := make(map[modelsDevLimit]int, len(entries))
	for _, entry := range entries {
		votes[entry.model.Limit]++
	}
	// 2. 依次比较：有上限的组优先，服务多的组优先，窗口小的组优先；同组保留按服务 ID 排在前面的。
	best := entries[0]
	for _, entry := range entries[1:] {
		if isLimitPreferred(entry.model.Limit, best.model.Limit, votes) {
			best = entry
		}
	}
	return best
}

// isLimitPreferred candidate 这组上限是否比 current 更优先？
func isLimitPreferred(candidate, current modelsDevLimit, votes map[modelsDevLimit]int) bool {
	// 1. 同一组不算；不同组先比有没有上限，再比服务数，最后窗口小的优先。
	if candidate == current {
		return false
	}
	candidateKnown, currentKnown := candidate != modelsDevLimit{}, current != modelsDevLimit{}
	if candidateKnown != currentKnown {
		return candidateKnown
	}
	if votes[candidate] != votes[current] {
		return votes[candidate] > votes[current]
	}
	return candidate.Context < current.Context
}

// newUpstreamModelsRequest 构造请求上游 /models 的 GET 请求。
func newUpstreamModelsRequest(ctx context.Context, provider defs.ProviderConfig) (*http.Request, error) {
	// 1. 拼出 /models 地址；Claude 协议加 limit=1000，没写协议时按 OpenAI Responses。
	modelsURL := strings.TrimRight(provider.BaseURL, "/") + "/models"
	isClaude := cmp.Or(provider.Protocol, string(llmkit.ProtocolOpenAIResponses)) == string(llmkit.ProtocolClaude)
	if isClaude {
		modelsURL += "?limit=" + strconv.Itoa(claudeModelsPageLimit)
	}
	// 2. 创建请求；Claude 带版本头，有密钥时 Claude 填 x-api-key，其他协议填 Bearer。
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return nil, err
	}
	if isClaude {
		request.Header.Set("anthropic-version", anthropicVersion)
		if provider.APIKey != "" {
			request.Header.Set("x-api-key", provider.APIKey)
		}
		return request, nil
	}
	if provider.APIKey != "" {
		request.Header.Set("Authorization", "Bearer "+provider.APIKey)
	}
	return request, nil
}
