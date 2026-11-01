package defs

import (
	"encoding/json"
	"slices"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
)

// ReasoningStatus 是推理内容已经完成还是被中断。
type ReasoningStatus string

const (
	ReasoningCompleted   ReasoningStatus = "completed"
	ReasoningInterrupted ReasoningStatus = "interrupted"
)

// Reasoning 是一段模型推理：正文、摘要、加密内容和完成状态。
type Reasoning struct {
	Status    ReasoningStatus `json:"status"`
	Summary   []string        `json:"summary,omitempty"`
	Content   []string        `json:"content,omitempty"`
	Encrypted string          `json:"encrypted,omitempty"`
	RawJSON   json.RawMessage `json:"raw_json,omitempty"`
}

// ToLLMKit 把推理内容转换为 llmkit 的推理内容。
func (reasoning *Reasoning) ToLLMKit() *llmkit.ReasoningContent {
	// 1. 复制摘要、正文、加密内容和原始 JSON，不包含结束状态。
	return &llmkit.ReasoningContent{
		Summary:   slices.Clone(reasoning.Summary),
		Content:   slices.Clone(reasoning.Content),
		Encrypted: reasoning.Encrypted,
		RawJSON:   slices.Clone(reasoning.RawJSON),
	}
}

// FromLLMKit 用一段完整的 llmkit 推理内容和给定状态替换推理记录。
func (reasoning *Reasoning) FromLLMKit(content *llmkit.ReasoningContent, status llmkit.ReasoningStatus) {
	// 1. 复制完整内容并写入给定状态，不累加流式增量。
	*reasoning = Reasoning{
		Status:    ReasoningStatus(status),
		Summary:   slices.Clone(content.Summary),
		Content:   slices.Clone(content.Content),
		Encrypted: content.Encrypted,
		RawJSON:   slices.Clone(content.RawJSON),
	}
}
