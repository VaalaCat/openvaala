package jsonutil

import (
	"bytes"
	"encoding/json"
)

// MarshalWithoutHTMLEscape 将值编码为不转义 HTML 的 JSON。
func MarshalWithoutHTMLEscape(value any) ([]byte, error) {
	// 1. 关闭 HTML 转义并编码输入值，保留 <、>、& 字面量。
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	// 2. 去掉编码器附加的结尾换行。
	return bytes.TrimSuffix(encoded.Bytes(), []byte("\n")), nil
}
