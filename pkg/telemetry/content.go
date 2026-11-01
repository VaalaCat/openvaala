package telemetry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
)

const (
	// contentAttributeLimit 是单个内容属性按 JSON 序列化后的大小上限（字节）。
	contentAttributeLimit = 64 << 10
	// contentFitRounds 是单条消息超限时，在每个 part 各一轮转文本与一轮截断之外额外允许的轮数；每轮处理当前最长的文本字段。
	contentFitRounds = 8
	// truncatedMarker 追加在被截断文本末尾，%d 是截掉的字符数。
	truncatedMarker = "…[truncated %d chars]"
)

// DecodeJSONContent 解析工具内容中的 JSON。
func DecodeJSONContent(raw []byte) any {
	// 1. 解码单个 JSON 值，数字保留为 json.Number；无效或空内容返回原文字符串。
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return string(raw)
	}
	// 2. 再次解码检查已到 EOF，含尾随数据时返回原文。
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return string(raw)
	}
	return value
}

// newMessagesValue 编码消息数组，并裁剪超限内容。
func newMessagesValue(messages []ChatMessage) (attribute.Value, bool) {
	// 1. 编码各消息，统计方括号、逗号和消息占用的字节数。
	values := make([]attribute.Value, len(messages))
	sizes := make([]int, len(messages))
	total := 2
	for index, message := range messages {
		values[index] = newMessageValue(message)
		sizes[index] = len(values[index].String()) + 1
		total += sizes[index]
	}
	// 2. 超过 64 KiB 时从最早消息开始整条移除。
	truncated := false
	first := 0
	for total > contentAttributeLimit && first < len(values)-1 {
		total -= sizes[first]
		first++
		truncated = true
	}
	values = values[first:]
	// 3. 仅剩一条仍超限时截断其中最长文本，返回编码值和截断标记。
	if total > contentAttributeLimit && len(values) == 1 {
		values[0] = newMessageValue(fitMessage(messages[first], contentAttributeLimit-2))
		truncated = true
	}
	return attribute.SliceValue(values...), truncated
}

// newTextPartsValue 编码系统指令数组，并裁剪超限文本。
func newTextPartsValue(texts []string) (attribute.Value, bool) {
	// 1. 将系统指令转换为 text part。
	message := ChatMessage{Parts: make([]MessagePart, 0, len(texts))}
	for _, text := range texts {
		message.Parts = append(message.Parts, MessagePart{
			Type:    PartTypeText,
			Content: text,
		})
	}
	// 2. 超限时截断最长文本，再编码为 part 数组并返回截断标记。
	truncated := len(newMessageValue(message).String()) > contentAttributeLimit
	if truncated {
		message = fitMessage(message, contentAttributeLimit)
	}
	parts := make([]attribute.Value, 0, len(message.Parts))
	for _, part := range message.Parts {
		parts = append(parts, newPartValue(part))
	}
	return attribute.SliceValue(parts...), truncated
}

// newToolDefinitionsValue 编码工具名称列表。
func newToolDefinitionsValue(names []string) attribute.Value {
	// 1. 每个工具只写 function 类型和名称，返回定义数组。
	definitions := make([]attribute.Value, 0, len(names))
	for _, name := range names {
		definitions = append(definitions, attribute.MapValue(attribute.String("type", genAIToolTypeFunction), attribute.String("name", name)))
	}
	return attribute.SliceValue(definitions...)
}

// newJSONContentValue 编码工具参数或结果，并裁剪超限文本。
func newJSONContentValue(value any) (attribute.Value, bool) {
	// 1. 字符串保留原文，其余转紧凑 JSON，超过 64 KiB 时截断并标记。
	text := getJSONText(value)
	if len(text) > contentAttributeLimit {
		return attribute.StringValue(truncateText(text, len(text)-contentAttributeLimit)), true
	}
	return attribute.StringValue(text), false
}

// newMessageValue 将消息编码为角色和内容段。
func newMessageValue(message ChatMessage) attribute.Value {
	// 1. 编码每个 part，再写入 role 和 parts；空角色也保留作大小计算。
	parts := make([]attribute.Value, 0, len(message.Parts))
	for _, part := range message.Parts {
		parts = append(parts, newPartValue(part))
	}
	return attribute.MapValue(attribute.String("role", message.Role), attribute.Slice("parts", parts...))
}

// newPartValue 将消息内容段编码为属性值。
func newPartValue(part MessagePart) attribute.Value {
	// 1. 写入类型和非空文本字段。
	fields := []attribute.KeyValue{attribute.String("type", part.Type)}
	for _, field := range []struct{ key, value string }{
		{
			"content",
			part.Content,
		}, {
			"id",
			part.ID,
		}, {
			"name",
			part.Name,
		},
		{
			"modality",
			part.Modality,
		}, {
			"mime_type",
			part.MimeType,
		}, {
			"uri",
			part.URI,
		},
	} {
		if field.value != "" {
			fields = append(fields, attribute.String(field.key, field.value))
		}
	}
	// 2. 有参数或结果时递归编码，nil 字段省略。
	if part.Arguments != nil {
		fields = append(fields, attribute.KeyValue{
			Key:   "arguments",
			Value: newAnyValue(part.Arguments),
		})
	}
	if part.Response != nil {
		fields = append(fields, attribute.KeyValue{
			Key:   "response",
			Value: newAnyValue(part.Response),
		})
	}
	return attribute.MapValue(fields...)
}

// newAnyValue 把 JSON 值递归转换为结构化遥测属性。
func newAnyValue(value any) attribute.Value {
	// 1. 按类型转换标量、数组和对象，数字优先用整数再用浮点数，对象键排序；其他类型转 JSON 文本。
	switch value := value.(type) {
	case nil:
		return attribute.Value{}
	case string:
		return attribute.StringValue(value)
	case bool:
		return attribute.BoolValue(value)
	case json.Number:
		if integer, err := value.Int64(); err == nil {
			return attribute.Int64Value(integer)
		}
		if number, err := value.Float64(); err == nil {
			return attribute.Float64Value(number)
		}
		return attribute.StringValue(value.String())
	case float64:
		return attribute.Float64Value(value)
	case []any:
		items := make([]attribute.Value, 0, len(value))
		for _, item := range value {
			items = append(items, newAnyValue(item))
		}
		return attribute.SliceValue(items...)
	case map[string]any:
		fields := make([]attribute.KeyValue, 0, len(value))
		for _, key := range slices.Sorted(maps.Keys(value)) {
			fields = append(fields, attribute.KeyValue{
				Key:   attribute.Key(key),
				Value: newAnyValue(value[key]),
			})
		}
		return attribute.MapValue(fields...)
	}
	return attribute.StringValue(getMarshaledText(value))
}

// fitMessage 逐轮裁剪消息中最长的文本字段。
func fitMessage(message ChatMessage, limit int) ChatMessage {
	// 1. 复制 part 列表，每轮度量编码大小并裁剪最长字段；最多执行两倍 part 数加 8 轮，仍超限时返回最后结果。
	message.Parts = slices.Clone(message.Parts)
	for range 2*len(message.Parts) + contentFitRounds {
		size := len(newMessageValue(message).String())
		if size <= limit || len(message.Parts) == 0 {
			return message
		}
		index := getLongestPartIndex(message.Parts)
		message.Parts[index] = truncatePart(message.Parts[index], size-limit)
	}
	return message
}

// getLongestPartIndex 返回可裁剪文本最长的内容段下标。
func getLongestPartIndex(parts []MessagePart) int {
	// 1. 遍历各段并比较可裁剪文本长度，相同时保留较早项。
	longest := 0
	for index, part := range parts {
		if getPartTextLength(part) > getPartTextLength(parts[longest]) {
			longest = index
		}
	}
	return longest
}

// getPartTextLength 返回内容段中最长文本的字节数。
func getPartTextLength(part MessagePart) int {
	// 1. 比较正文、参数文本和结果文本的长度。
	return max(len(part.Content), len(getJSONText(part.Arguments)), len(getJSONText(part.Response)))
}

// truncatePart 裁剪内容段中最长的文本字段。
func truncatePart(part MessagePart, excess int) MessagePart {
	// 1. 比较正文、参数和结果；字符串按 excess 截断，结构化参数或结果先只转文本，留下一轮重新度量。
	arguments, response := getJSONText(part.Arguments), getJSONText(part.Response)
	switch max(len(part.Content), len(arguments), len(response)) {
	case len(part.Content):
		part.Content = truncateText(part.Content, excess)
	case len(arguments):
		part.Arguments = truncateJSONField(part.Arguments, arguments, excess)
	default:
		part.Response = truncateJSONField(part.Response, response, excess)
	}
	return part
}

// truncateJSONField 将结构化字段转为文本，或裁剪已有文本。
func truncateJSONField(field any, text string, excess int) any {
	// 1. 已是字符串时截断 text，其余只返回文本形式。
	if _, isText := field.(string); isText {
		return truncateText(text, excess)
	}
	return text
}

// getJSONText 返回参数或结果的文本形式。
func getJSONText(value any) string {
	// 1. nil 返回空串，字符串原样返回，其他值按结构化属性编码为 JSON 文本。
	switch value := value.(type) {
	case nil:
		return ""
	case string:
		return value
	}
	return newAnyValue(value).String()
}

// getMarshaledText 将值序列化为不转义 HTML 的 JSON 文本。
func getMarshaledText(value any) string {
	// 1. 编码 JSON 并去掉末尾换行，无法编码时返回 fmt 文本。
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return fmt.Sprint(value)
	}
	return strings.TrimSuffix(buffer.String(), "\n")
}

// truncateText 裁剪文本末尾并附上被截掉的字符数。
func truncateText(text string, excess int) string {
	// 1. 按 excess 和截断说明预留字节，回退到 UTF-8 字符起点，再追加实际删去的字符数。
	keep := max(len(text)-excess-len(truncatedMarker)-16, 0)
	for keep > 0 && !utf8.RuneStart(text[keep]) {
		keep--
	}
	return text[:keep] + fmt.Sprintf(truncatedMarker, utf8.RuneCountInString(text[keep:]))
}
