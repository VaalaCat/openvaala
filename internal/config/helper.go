package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/docs"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/prompt"
	"github.com/VaalaCat/openvaala/pkg/workspace"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

// UnmarshalJSON 读取单个技能路径或路径数组。
func (paths *pluginSkillPaths) UnmarshalJSON(content []byte) error {
	// 1. 缺省值不添加路径，字符串转成单元素列表，其他值必须是字符串数组。
	if bytes.Equal(bytes.TrimSpace(content), []byte("null")) {
		*paths = nil
		return nil
	}
	var path string
	if err := json.Unmarshal(content, &path); err == nil {
		*paths = []string{path}
		return nil
	}
	var values []string
	if err := json.Unmarshal(content, &values); err != nil {
		return errors.New("skills must be a path or an array of paths")
	}
	*paths = values
	return nil
}

// expandPath 展开开头的 ~，相对路径接到 base 后面，返回清理过的路径。
func expandPath(path, home, base string) string {
	// 1. ~ 和 ~/ 换成主目录，绝对路径只清理，相对路径接到 base 后。
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(base, path)
}

// bindConfigFields 按 Config 字段的 default、flag 标签登记默认值并绑定 flag。
func bindConfigFields(settings *viper.Viper, fields reflect.Type, prefix string, flags *pflag.FlagSet) error {
	// 1. 遍历导出字段，结构体递归处理；map 和结构体切片只从文件读取，跳过；其他字段登记 default 标签的默认值，并绑定 flag 标签指定的 flag。
	for index := 0; index < fields.NumField(); index++ {
		field := fields.Field(index)
		if !field.IsExported() {
			continue
		}
		key := prefix + field.Tag.Get("mapstructure")
		fieldType := field.Type
		if fieldType.Kind() == reflect.Pointer {
			fieldType = fieldType.Elem()
		}
		if fieldType.Kind() == reflect.Struct {
			if err := bindConfigFields(settings, fieldType, key+".", flags); err != nil {
				return err
			}
			continue
		}
		if fieldType.Kind() == reflect.Map || (fieldType.Kind() == reflect.Slice && fieldType.Elem().Kind() == reflect.Struct) {
			continue
		}
		settings.SetDefault(key, field.Tag.Get("default"))
		if flags != nil {
			if flag := flags.Lookup(field.Tag.Get("flag")); flag != nil {
				if err := settings.BindPFlag(key, flag); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// pathParts 是按整段目录名拆开的绝对路径。
type pathParts struct {
	// drive 是 Windows 路径的大写盘符，Unix 路径为空。
	drive string
	// segments 是从根目录往下的各段目录名。
	segments []string
}

// isAbsoluteClientPath 客户端路径是否为绝对路径？
func isAbsoluteClientPath(value string) bool {
	// 1. Windows 路径要求盘符后紧跟分隔符，其他路径要求以 / 开头。
	if IsWindowsPath(value) {
		return len(value) > 2 && (value[2] == '\\' || value[2] == '/')
	}
	return strings.HasPrefix(value, "/")
}

// splitPath 把绝对路径拆成盘符和目录名。
func splitPath(value string) pathParts {
	// 1. Windows 盘符转大写，统一分隔符，再清理路径并拆成完整目录名。
	parts := pathParts{}
	if IsWindowsPath(value) {
		parts.drive = strings.ToUpper(value[:1])
		value = strings.ReplaceAll(value[2:], `\`, "/")
	}
	cleaned := path.Clean("/" + value)
	if cleaned != "/" {
		parts.segments = strings.Split(cleaned[1:], "/")
	}
	return parts
}

// getPathKey 生成比较路径是否相同用的键：盘符大写、分隔符统一为 /、去掉末尾分隔符。
func getPathKey(value string) string {
	// 1. 拆开路径，用大写盘符和 / 重新拼接目录名。
	parts := splitPath(value)
	return parts.drive + ":/" + strings.Join(parts.segments, "/")
}

// getPathMapping 找前缀覆盖目标路径的最长映射，返回这条映射、前缀后剩下的目录名和是否找到。
func getPathMapping(mappings PathMappings, target string, isServerPath bool) (PathMapping, []string, bool) {
	// 1. 拆开目标路径。
	targetParts := splitPath(target)
	var best PathMapping
	var bestRest []string
	bestLength := -1
	// 2. isServerPath 为 true 时拿 Server 当前缀，否则拿 Client；按盘符和整段目录名比较，留下最长的匹配和后面剩下的目录名。
	for _, mapping := range mappings {
		prefix := mapping.Client
		if isServerPath {
			prefix = mapping.Server
		}
		prefixParts := splitPath(prefix)
		length := len(prefixParts.segments)
		if length <= bestLength || prefixParts.drive != targetParts.drive || length > len(targetParts.segments) ||
			!slices.Equal(targetParts.segments[:length], prefixParts.segments) {
			continue
		}
		best, bestRest, bestLength = mapping, targetParts.segments[length:], length
	}
	return best, bestRest, bestLength >= 0
}

// joinUnixPath 把目录名接到 Unix 路径后面。
func joinUnixPath(base string, segments []string) string {
	// 1. 用 / 连接 base 和各段目录名。
	return path.Join(append([]string{base}, segments...)...)
}

// joinWindowsPath 把目录名接到 Windows 路径后面。
func joinWindowsPath(base string, segments []string) string {
	// 1. 分隔符统一成反斜杠并去掉末尾分隔符，再接上目录名。
	base = strings.TrimRight(strings.ReplaceAll(base, "/", `\`), `\`)
	if len(segments) > 0 {
		return base + `\` + strings.Join(segments, `\`)
	}
	// 2. 盘符根目录补回末尾反斜杠，其余直接返回。
	if len(base) == 2 {
		return base + `\`
	}
	return base
}

// getTelemetryLogFile 确定日志文件路径。
func getTelemetryLogFile(path, home, base string) string {
	// 1. 未配置时使用 ~/.local/state/openvaala/openvaala.log，否则按配置文件位置展开。
	if path == "" {
		return filepath.Join(home, ".local", "state", "openvaala", "openvaala.log")
	}
	return expandPath(path, home, base)
}

// readYAML 读取并严格解析 YAML 文件。
func readYAML(path string, target any) error {
	// 1. 打开文件严格解析，内容无效时返回带路径的固定错误。
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := decodeYAML(file, target); err != nil {
		return fmt.Errorf("%s: invalid YAML, field type or unknown field", path)
	}
	return nil
}

// decodeYAML 严格解析一份 YAML 文档。
func decodeYAML(input io.Reader, target any) error {
	// 1. 拒绝未知字段和重复键，解析到 target。
	decoder := yaml.NewDecoder(input)
	decoder.KnownFields(true)
	if err := decoder.Decode(target); err != nil {
		return err
	}
	// 2. 再读一次，后面还有文档时报错。
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("expected one YAML document")
	}
	return nil
}

// encodeYAML 把值编码成两空格缩进的 YAML。
func encodeYAML(value any) ([]byte, error) {
	// 1. 编码并关闭编码器，返回文档内容。
	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

// toBlockMapping 把空节点或空映射改成块风格的空映射。
func toBlockMapping(node *yaml.Node) {
	// 1. 已经有键的映射不动，其他节点改成空的块风格映射。
	if node.Kind == yaml.MappingNode && len(node.Content) > 0 {
		return
	}
	node.Kind, node.Tag, node.Value, node.Style = yaml.MappingNode, "!!map", "", 0
}

// getOrAddMappingEntry 返回 YAML 映射里 key 对应的值节点，没有这个键时在末尾追加。
func getOrAddMappingEntry(mapping *yaml.Node, key string) *yaml.Node {
	// 1. 找到键时返回它的值，否则在末尾追加字符串键和空值节点。
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			return mapping.Content[index+1]
		}
	}
	value := &yaml.Node{}
	mapping.Content = append(mapping.Content, &yaml.Node{
		Kind:  yaml.ScalarNode,
		Tag:   "!!str",
		Value: key,
	}, value)
	return value
}

// deleteMappingEntry 删除 YAML 映射里的 key 和它的值。
func deleteMappingEntry(mapping *yaml.Node, key string) {
	// 1. 找到键后删除这一对，没找到时不动。
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			mapping.Content = slices.Delete(mapping.Content, index, index+2)
			return
		}
	}
}

// getProviderProtocol 把 Provider 协议名换成规范写法，返回是否支持。
func getProviderProtocol(name string) (llmkit.Protocol, bool) {
	// 1. openai-responses 换成规范写法，再匹配 OpenAI Chat、OpenAI Responses 或 Claude。
	if name == "openai-responses" {
		name = string(llmkit.ProtocolOpenAIResponses)
	}
	protocol := llmkit.Protocol(name)
	switch protocol {
	case llmkit.ProtocolOpenAIChat, llmkit.ProtocolOpenAIResponses, llmkit.ProtocolClaude:
		return protocol, true
	}
	return "", false
}

// getDefaultEndpointSuffix 返回协议的默认请求路径；协议已经校验过。
func getDefaultEndpointSuffix(protocol llmkit.Protocol) string {
	// 1. 按协议返回 /chat/completions、/responses 或 /messages，未知协议直接 panic。
	switch protocol {
	case llmkit.ProtocolOpenAIChat:
		return "/chat/completions"
	case llmkit.ProtocolOpenAIResponses:
		return "/responses"
	case llmkit.ProtocolClaude:
		return "/messages"
	}
	panic("unsupported provider protocol " + string(protocol))
}

// isValidProviderURL base_url 是否合法？
func isValidProviderURL(value string) bool {
	// 1. 要求 http 或 https、主机非空，不能带用户信息，也不能出现 ? 和 #。
	address, err := url.Parse(value)
	return err == nil && (address.Scheme == "http" || address.Scheme == "https") && address.Host != "" &&
		address.User == nil && !strings.ContainsAny(value, "?#")
}

// isValidEndpointURL endpoints 里的完整请求地址是否合法？
func isValidEndpointURL(value string) bool {
	// 1. 先按 base_url 的规则检查，再要求请求路径非空且不是根目录 /。
	if !isValidProviderURL(value) {
		return false
	}
	address, _ := url.Parse(value)
	return address.Path != "" && address.Path != "/"
}

// splitProviderURL 把校验过的完整请求地址拆成 scheme://host 和请求路径。
func splitProviderURL(value string) (baseURL, endpointPath string) {
	// 1. 请求路径保留原来的转义。
	address, _ := url.Parse(value)
	return address.Scheme + "://" + address.Host, address.EscapedPath()
}

// isValidID ID 能不能用作单个配置文件名？
func isValidID(id string) bool {
	// 1. 拒绝空串、. 和 ..，其他只允许字母、数字、-、_ 和 .。
	if id == "" || id == "." || id == ".." {
		return false
	}
	for _, char := range id {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' || char == '.' {
			continue
		}
		return false
	}
	return true
}

// isInsidePluginDirectory 这个相对路径清理后还在插件目录里吗？
func isInsidePluginDirectory(path string) bool {
	// 1. ~ 开头、绝对路径和用 .. 越出目录的路径都不算。
	return !strings.HasPrefix(path, "~") && filepath.IsLocal(path)
}

// isSameOrInsideDirectory 已解析过链接的绝对路径 path 是 directory 本身或在它里面吗？
func isSameOrInsideDirectory(directory, path string) bool {
	// 1. 算出相对路径，检查它没有越出 directory。
	relativePath, err := filepath.Rel(directory, path)
	return err == nil && filepath.IsLocal(relativePath)
}

// getDefinitionFiles 按文件名顺序返回目录下直属的 .yaml 文件，子目录里的不算。
func getDefinitionFiles(directory string) ([]string, error) {
	// 1. 读取目录，目录不存在时返回空列表，其他读取失败时返回错误。
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// 2. 收集扩展名为 .yaml 的文件。
	var paths []string
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".yaml" {
			paths = append(paths, filepath.Join(directory, entry.Name()))
		}
	}
	return paths, nil
}

// builtinSkillDirectory 是 builtin Executor 中存放内置 skill 的目录。
const builtinSkillDirectory = "/skills"

// builtinDocsSkillName 是由 OpenVaala 文档组成的内置 skill 的目录名。
const builtinDocsSkillName = "openvaala"

// getBuiltinExecutorDefinition 组装内置 Executor：装着 openvaala 文档 skill 的 virtual Executor。
func getBuiltinExecutorDefinition() (*defs.ExecutorDefinition, error) {
	// 1. 读取嵌入的文档，放到 /skills/openvaala 下对应的路径。
	files := make(map[string][]byte)
	err := fs.WalkDir(docs.Files, ".", func(filePath string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		content, err := docs.Files.ReadFile(filePath)
		if err != nil {
			return err
		}
		files[path.Join(builtinSkillDirectory, builtinDocsSkillName, filePath)] = content
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read builtin skills: %w", err)
	}
	// 2. 返回 ID 为 openvaala 的定义，工作目录为 /workspace。
	return &defs.ExecutorDefinition{
		ID:                 BuiltinExecutorID,
		Kind:               defs.ExecutorKindVirtual,
		Description:        prompt.BuiltinExecutorDescription,
		WorkspaceDirectory: virtualWorkspaceDirectory,
		Files:              files,
	}, nil
}

// getExecutorDefinition 把 Executor 配置转成运行定义。
func getExecutorDefinition(executorConfig *ExecutorConfig) *defs.ExecutorDefinition {
	// 1. 复制字段，有 SSH 段时另建连接参数。
	definition := &defs.ExecutorDefinition{
		ID:                 executorConfig.ID,
		Kind:               executorConfig.Kind,
		Description:        executorConfig.Description,
		WorkspaceDirectory: executorConfig.WorkspaceDirectory,
	}
	if executorConfig.SSH != nil {
		definition.SSH = &defs.SSHExecutorDefinition{
			Address:      executorConfig.SSH.Address,
			IdentityFile: executorConfig.SSH.IdentityFile,
		}
	}
	return definition
}

// getVirtualFiles 把文件路径到内容的映射转成虚拟工作区文件。
func getVirtualFiles(files map[string][]byte) map[string]*workspace.VirtualFile {
	// 1. 每个路径对应一个装着原内容的 VirtualFile。
	virtualFiles := make(map[string]*workspace.VirtualFile, len(files))
	for filePath, content := range files {
		virtualFiles[filePath] = &workspace.VirtualFile{Content: content}
	}
	return virtualFiles
}

// Clone 复制 Agent 配置。
func (definition *AgentConfig) Clone() *AgentConfig {
	// 1. nil 原样返回，否则复制模型引用和工具、skill、MCP server 列表。
	if definition == nil {
		return nil
	}
	return &AgentConfig{
		Name:         definition.Name,
		Description:  definition.Description,
		Instructions: definition.Instructions,
		Model:        definition.Model.Clone(),
		CompactModel: definition.CompactModel.Clone(),
		Tools:        slices.Clone(definition.Tools),
		Skills:       slices.Clone(definition.Skills),
		MCPServers:   slices.Clone(definition.MCPServers),
	}
}

// Clone 复制 Agent 的模型引用。
func (reference *AgentModelConfig) Clone() *AgentModelConfig {
	// 1. nil 原样返回，否则复制 Provider 和模型名。
	if reference == nil {
		return nil
	}
	return &AgentModelConfig{
		Provider: reference.Provider,
		Model:    reference.Model,
	}
}

// getModelRef 把 Agent 的模型配置转成模型目录里的引用。
func getModelRef(reference *AgentModelConfig) defs.ModelRef {
	// 1. 没配时返回零值，否则填入 Provider ID 和模型 ID。
	if reference == nil {
		return defs.ModelRef{}
	}
	return defs.ModelRef{
		ProviderID: reference.Provider,
		ModelID:    reference.Model,
	}
}

// validateReasoningEfforts 校验推理强度列表。
func validateReasoningEfforts(efforts []string) error {
	// 1. 有空白值或重复值时返回错误。
	for index, effort := range efforts {
		if strings.TrimSpace(effort) == "" || slices.Contains(efforts[:index], effort) {
			return errors.New("reasoning_efforts must not contain blank or duplicate values")
		}
	}
	return nil
}

// getDefaultReasoningEffort 定出模型的默认推理强度。
func getDefaultReasoningEffort(efforts []string, modelDefault, globalDefault string) (string, error) {
	// 1. 模型写了默认值时它必须在列表里，不在就报错。
	switch {
	case modelDefault != "":
		if !slices.Contains(efforts, modelDefault) {
			return "", fmt.Errorf("default_reasoning_effort %q is not in reasoning_efforts", modelDefault)
		}
		return modelDefault, nil
	// 2. 模型没写时：列表为空返回空串；全局默认值在列表里就用它，否则用列表第一项。
	case len(efforts) == 0:
		return "", nil
	case slices.Contains(efforts, globalDefault):
		return globalDefault, nil
	}
	return efforts[0], nil
}

// getMessageRoles 校验 merge_message_roles 并转成消息角色。
func getMessageRoles(roles []string) ([]llmkit.Role, error) {
	// 1. 只接受不重复的 system、developer、user 和 assistant，tool 不行。
	allowed := []llmkit.Role{llmkit.RoleSystem, llmkit.RoleDeveloper, llmkit.RoleUser, llmkit.RoleAssistant}
	converted := make([]llmkit.Role, 0, len(roles))
	for _, role := range roles {
		value := llmkit.Role(role)
		if !slices.Contains(allowed, value) || slices.Contains(converted, value) {
			return nil, fmt.Errorf("merge_message_roles must be distinct values of system, developer, user or assistant, got %q", role)
		}
		converted = append(converted, value)
	}
	return converted, nil
}

// getReasoningContext 校验 reasoning_context 并转成思考上下文模式。
func getReasoningContext(protocol llmkit.Protocol, value string) (llmkit.ReasoningContext, error) {
	// 1. 没写返回空模式，请求不发送。
	if value == "" {
		return llmkit.ReasoningContextUnknown, nil
	}
	// 2. 只接受 all_turns 和 current_turn。
	mode := llmkit.ReasoningContext(value)
	if mode != llmkit.ReasoningContextAllTurns && mode != llmkit.ReasoningContextCurrentTurn {
		return llmkit.ReasoningContextUnknown, fmt.Errorf("reasoning_context must be all_turns or current_turn, got %q", value)
	}
	// 3. 只有 openai-responses 协议的请求能带这个模式。
	if protocol != llmkit.ProtocolOpenAIResponses {
		return llmkit.ReasoningContextUnknown, fmt.Errorf("reasoning_context requires %s protocol", llmkit.ProtocolOpenAIResponses)
	}
	return mode, nil
}

// getModelInputs 校验 capabilities.input 并转成模型输入类型。
func getModelInputs(inputs []string) ([]defs.ModelInput, error) {
	// 1. 只接受 text、image、audio、video、pdf，重复的只留第一次出现的。
	converted := make([]defs.ModelInput, 0, len(inputs))
	for _, input := range inputs {
		value := defs.ModelInput(input)
		if !value.IsKnown() {
			return nil, fmt.Errorf("unknown input capability %q", input)
		}
		if !slices.Contains(converted, value) {
			converted = append(converted, value)
		}
	}
	// 2. 一个都没写时返回 [text]。
	if len(converted) == 0 {
		return []defs.ModelInput{defs.ModelInputText}, nil
	}
	return converted, nil
}
