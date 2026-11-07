package httpclient

import (
	"regexp"
	"runtime"
	"strings"

	"github.com/VaalaCat/openvaala/pkg/defs"
)

// headerVariablePattern 匹配 header 值里的 ${OPENVAALA_*}，其余 ${...} 当作普通文字；名字写错的 OPENVAALA_ 变量也会匹配，按不认识处理。
var headerVariablePattern = regexp.MustCompile(`\$\{(OPENVAALA_[^}]*)\}`)

// headerVariables 是认识的全部变量及其取值；变量名和取值的对应只写在这里。
var headerVariables = map[string]func(meta defs.SessionCtxMeta) string{
	"OPENVAALA_VERSION":           func(defs.SessionCtxMeta) string { return defs.GetBuildVersion() },
	"OPENVAALA_OS":                func(defs.SessionCtxMeta) string { return runtime.GOOS },
	"OPENVAALA_ARCH":              func(defs.SessionCtxMeta) string { return runtime.GOARCH },
	"OPENVAALA_CONVERSATION_ID":   func(meta defs.SessionCtxMeta) string { return string(meta.ConversationID) },
	"OPENVAALA_SESSION_ID":        func(meta defs.SessionCtxMeta) string { return string(meta.SessionID) },
	"OPENVAALA_PARENT_SESSION_ID": func(meta defs.SessionCtxMeta) string { return string(meta.ParentSessionID) },
	"OPENVAALA_AGENT_NAME":        func(meta defs.SessionCtxMeta) string { return meta.AgentName },
}

// fillHeaderValue 把 value 里的 ${OPENVAALA_*} 换成 meta 等来源的取值；
// value 为空、变量不认识或变量取到空值时 ok 为 false，表示这条 header 不该发。
func fillHeaderValue(value string, meta defs.SessionCtxMeta) (filled string, ok bool) {
	// 1. 值本身为空的 header 不发。
	if value == "" {
		return "", false
	}
	// 2. 逐个替换变量，遇到不认识或取到空值的变量就记下来。
	ok = true
	filled = headerVariablePattern.ReplaceAllStringFunc(value, func(match string) string {
		name := strings.TrimSuffix(strings.TrimPrefix(match, "${"), "}")
		getValue, known := headerVariables[name]
		if !known {
			ok = false
			return ""
		}
		variableValue := getValue(meta)
		if variableValue == "" {
			ok = false
		}
		return variableValue
	})
	return filled, ok
}
