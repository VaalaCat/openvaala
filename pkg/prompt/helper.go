package prompt

import "strings"

// formatXMLBlock 拼出带子字段和正文的 XML 提示块。
func formatXMLBlock(tag, body string, fields ...xmlField) string {
	// 1. 按传入顺序写入子字段，值里出现字段自己的结束标签时转义。
	var content strings.Builder
	for _, field := range fields {
		content.WriteByte('<')
		content.WriteString(field.name)
		content.WriteByte('>')
		content.WriteString(strings.ReplaceAll(field.value, "</"+field.name+">", "<\\/"+field.name+">"))
		content.WriteString("</" + field.name + ">\n")
	}
	// 2. 接上正文，转义其中的外层结束标签，再包上外层标签。
	content.WriteString(body)
	return "<" + tag + ">\n" + strings.ReplaceAll(content.String(), "</"+tag+">", "<\\/"+tag+">") + "</" + tag + ">"
}

// joinTextParts 拼接非空的文本块，每块后面加换行。
func joinTextParts(textParts []string) string {
	// 1. 跳过空文本。
	var content strings.Builder
	for _, text := range textParts {
		if text != "" {
			content.WriteString(text)
			content.WriteByte('\n')
		}
	}
	return content.String()
}
