package defs

import (
	"fmt"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
)

// Message 是一条消息：发送方、接收方地址和内容。
type Message struct {
	From    *Address   `json:"from"`
	To      *Address   `json:"to"`
	Content []*Content `json:"content"`
	// AgentSender 由 Conversation 在 Agent 之间转交消息时写入，模型请求据此标明消息来自哪个 Agent；用户消息为 nil。
	AgentSender *AgentSender `json:"agent_sender,omitempty"`
	// SubagentNotification 由 Conversation 在子 Agent 的 Turn 结束、通知父 Agent 时写入；其他消息为 nil。
	SubagentNotification *SubagentNotification `json:"subagent_notification,omitempty"`
}

// ContentKind 区分文本和图片内容。
type ContentKind string

const (
	ContentText  ContentKind = "text"
	ContentImage ContentKind = "image"
)

// Content 是消息里的一项文本或图片，Kind 决定哪个字段有值。
type Content struct {
	Kind  ContentKind   `json:"kind"`
	Text  *TextContent  `json:"text,omitempty"`
	Image *ImageContent `json:"image,omitempty"`
}

// TextContent 是一段文字。
type TextContent struct {
	Text string `json:"text"`
	// IsContext 是不是附带材料？附带材料不参与用户命令解析。
	IsContext bool `json:"is_context,omitempty"`
}

// ImageContent 是一张图片：来源和说明。
type ImageContent struct {
	DataURL string `json:"data_url"`
	Alt     string `json:"alt,omitempty"`
}

// IdentityKind 区分用户和 Agent 地址。
type IdentityKind string

const (
	IdentityUser  IdentityKind = "user"
	IdentityAgent IdentityKind = "agent"
)

// Address 是消息的发送方或接收方。
type Address struct {
	Kind IdentityKind `json:"kind"`
	// ID 在 Agent 地址里是 Session ID。
	ID string `json:"id"`
}

// ToLLMKit 按调用方指定的模型角色转换消息。
func (message *Message) ToLLMKit(role llmkit.Role) (*llmkit.Message, error) {
	// 1. 检查消息和角色。
	if message == nil {
		return nil, fmt.Errorf("message is missing")
	}
	if err := validateMessageRole(role); err != nil {
		return nil, err
	}
	// 2. 逐项转换内容并组装模型消息。
	blocks := make([]llmkit.ContentBlock, 0, len(message.Content))
	for _, content := range message.Content {
		block, err := content.ToLLMKit()
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, *block)
	}
	return &llmkit.Message{
		Role:    role,
		Content: blocks,
	}, nil
}

// FromLLMKit 把只含文本和图片的模型消息转换为 Message，地址用调用方传入的 from、to。
func (message *Message) FromLLMKit(modelMessage *llmkit.Message, from, to *Address) error {
	// 1. 检查消息角色，拒绝原始 JSON 和工具调用。
	if modelMessage == nil {
		return fmt.Errorf("model message is missing")
	}
	if err := validateMessageRole(modelMessage.Role); err != nil {
		return err
	}
	if len(modelMessage.RawJSON) != 0 || len(modelMessage.ToolCalls) != 0 || modelMessage.ToolCallID != "" {
		return fmt.Errorf("message contains raw content or tool calls")
	}
	// 2. 逐项转换内容，转换失败时保留原消息。
	contents := make([]*Content, 0, len(modelMessage.Content))
	for index := range modelMessage.Content {
		content := &Content{}
		if err := content.FromLLMKit(&modelMessage.Content[index]); err != nil {
			return err
		}
		contents = append(contents, content)
	}
	// 3. 复制调用方传入的地址并替换完整消息。
	*message = Message{
		From:    from.Clone(),
		To:      to.Clone(),
		Content: contents,
	}
	return nil
}

// Validate 检查内容类型及对应的文本或图片载荷。
func (content *Content) Validate() error {
	// 1. 要求文本载荷存在，或图片载荷及 DataURL 非空；未知类型返回错误。
	if content == nil {
		return fmt.Errorf("content is missing")
	}
	switch content.Kind {
	case ContentText:
		if content.Text == nil {
			return fmt.Errorf("text content is missing")
		}
	case ContentImage:
		if content.Image == nil || content.Image.DataURL == "" {
			return fmt.Errorf("image source is missing")
		}
	default:
		return fmt.Errorf("unsupported content kind %q", content.Kind)
	}
	return nil
}

// ToLLMKit 将文本或图片转换为模型内容块。
func (content *Content) ToLLMKit() (*llmkit.ContentBlock, error) {
	// 1. 检查载荷，文本直接返回文字块。
	if err := content.Validate(); err != nil {
		return nil, err
	}
	if content.Kind == ContentText {
		return &llmkit.ContentBlock{
			Type: llmkit.ContentTypeText,
			Text: content.Text.Text,
		}, nil
	}
	// 2. 图片 data URL 拆成 MIME 类型和 base64，其他来源保留 URL。
	block := &llmkit.ContentBlock{Type: llmkit.ContentTypeImage}
	if mimeType, data, isDataURL := content.Image.GetDataURLParts(); isDataURL {
		block.MimeType, block.MediaB64 = mimeType, data
	} else {
		block.MediaURL = content.Image.DataURL
	}
	// 3. 将图片说明写入 alt 元数据。
	if content.Image.Alt != "" {
		block.Metadata = map[string]any{"alt": content.Image.Alt}
	}
	return block, nil
}

// FromLLMKit 将模型的文本或图片内容块转换为 Content。
func (content *Content) FromLLMKit(block *llmkit.ContentBlock) error {
	// 1. 检查内容块存在，拒绝原始 JSON。
	if block == nil {
		return fmt.Errorf("model content is missing")
	}
	if len(block.RawJSON) != 0 {
		return fmt.Errorf("raw model content cannot be represented as Content")
	}
	// 2. 按类型读取文本或图片；独立 base64 图片合成 data URL，保留 alt，未知类型返回错误。
	switch block.Type {
	case llmkit.ContentTypeText, llmkit.ContentTypeInputText, llmkit.ContentTypeOutputText:
		*content = Content{
			Kind: ContentText,
			Text: &TextContent{Text: block.Text},
		}
	case llmkit.ContentTypeImage, llmkit.ContentTypeImageURL:
		source := block.MediaURL
		if source == "" && block.MediaB64 != "" && block.MimeType != "" {
			source = "data:" + block.MimeType + ";base64," + block.MediaB64
		}
		if source == "" {
			return fmt.Errorf("image source is missing")
		}
		alt, _ := block.Metadata["alt"].(string)
		*content = Content{
			Kind: ContentImage,
			Image: &ImageContent{
				DataURL: source,
				Alt:     alt,
			},
		}
	default:
		return fmt.Errorf("unsupported model content type %q", block.Type)
	}
	return nil
}

// SubagentNotification 是子 Agent 的 Turn 结束并保存终态后发给父 Agent 的通知。
type SubagentNotification struct {
	// SourceTurnID 是子 Agent 结束的那个 Turn。
	SourceTurnID TurnID `json:"source_turn_id"`
	// Status 是那个 Turn 保存的终态。
	Status TurnStatus `json:"status"`
}

// AgentSender 是 Agent 消息发送方的任务名，以及它和接收方的关系。
type AgentSender struct {
	// TaskName 是发送方 Session 的任务名；主 Agent 没有任务名，为空。
	TaskName string        `json:"task_name,omitempty"`
	Relation AgentRelation `json:"relation"`
}

// AgentRelation 是 Agent 消息发送方相对接收方的位置。
type AgentRelation string

const (
	// AgentRelationChild 发送方是接收方的直接子 Agent，例如子任务向父 Agent 汇报或提问。
	AgentRelationChild AgentRelation = "child"
	// AgentRelationAncestor 发送方是接收方的父 Agent 或更上层的 Agent，例如 spawn_agent 交下的任务。
	AgentRelationAncestor AgentRelation = "ancestor"
)
