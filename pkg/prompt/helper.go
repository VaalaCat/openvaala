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

// cutTrailingXMLBlock 取出文本尾端的一块完整提示。
func cutTrailingXMLBlock(text, tag string) (rest, body string, found bool) {
	// 1. 从结尾找到最后一块的开标签和紧贴末尾的闭标签。
	open, end := "<"+tag+">\n", "</"+tag+">"
	if !strings.HasSuffix(text, end) {
		return text, "", false
	}
	start := strings.LastIndex(text, open)
	if start < 0 {
		return text, "", false
	}
	body, tail, closed := strings.Cut(text[start+len(open):], end)
	if !closed || tail != "" {
		return text, "", false
	}
	return strings.TrimSuffix(text[:start], "\n\n"), strings.ReplaceAll(body, "<\\/"+tag+">", end), true
}

// parseXMLFields 按提示块的字段顺序读取值并还原转义的结束标签。
func parseXMLFields(body string, names ...string) ([]string, bool) {
	// 1. 逐字段读取有完整起止标签的值。
	values := make([]string, 0, len(names))
	for _, name := range names {
		start, end := "<"+name+">", "</"+name+">"
		valueAndTail, found := strings.CutPrefix(body, start)
		if !found {
			return nil, false
		}
		value, tail, found := strings.Cut(valueAndTail, end+"\n")
		if !found {
			return nil, false
		}
		values = append(values, strings.ReplaceAll(value, "<\\/"+name+">", end))
		body = tail
	}
	return values, body == ""
}
