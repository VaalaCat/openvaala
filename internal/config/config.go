// Package config 加载应用设置和 Agent、Provider 定义。
package config

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/hook"
	"github.com/VaalaCat/openvaala/pkg/httpclient"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
	"github.com/VaalaCat/openvaala/pkg/prompt"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/skill"
	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// Config 是加载好的应用设置，以及校验过的 Agent、模型、Executor、MCP server、hook 和插件定义；加载后只读，可以并发查询。
type Config struct {
	Agents *struct {
		Directory string `mapstructure:"directory" yaml:"directory" default:"agents" flag:"agents-dir"`
	} `mapstructure:"agents" yaml:"agents"`
	Models *struct {
		Directory string `mapstructure:"directory" yaml:"directory" default:"models" flag:"models-dir"`
		// DefaultModel 是没写 model 的 Agent 使用的模型，格式 provider_id/model_id；为空时取模型目录里按 Provider id、模型 id 排序的第一个。
		DefaultModel string `mapstructure:"default_model" yaml:"default_model"`
		// Defaults 是模型配置省略字段时使用的值；default 标签就是系统内置默认值。
		Defaults *defs.ModelDefaults `mapstructure:"defaults" yaml:"defaults"`
	} `mapstructure:"models" yaml:"models"`
	Skills *struct {
		Directory string `mapstructure:"directory" yaml:"directory" default:"skills"`
	} `mapstructure:"skills" yaml:"skills"`
	Executors *struct {
		Directory string `mapstructure:"directory" yaml:"directory" default:"executors"`
	} `mapstructure:"executors" yaml:"executors"`
	MCPServers *struct {
		Directory string `mapstructure:"directory" yaml:"directory" default:"mcp_servers"`
	} `mapstructure:"mcp_servers" yaml:"mcp_servers"`
	Hooks *struct {
		Directory string `mapstructure:"directory" yaml:"directory" default:"hooks"`
	} `mapstructure:"hooks" yaml:"hooks"`
	Plugins *struct {
		Directory string `mapstructure:"directory" yaml:"directory" default:"plugins"`
		// Disabled 是不加载的插件名：它们的 skill、hook、MCP server 都没有；写了不存在的名字不报错。
		Disabled []string `mapstructure:"disabled" yaml:"disabled"`
	} `mapstructure:"plugins" yaml:"plugins"`
	Tools *struct {
		// MaxImageKB 是 read_file 交给模型的单张图片上限（KB），超过就压缩。
		MaxImageKB int `mapstructure:"max_image_kb" yaml:"max_image_kb" default:"500"`
	} `mapstructure:"tools" yaml:"tools"`
	Run *struct {
		AgentName string `mapstructure:"agent_name" yaml:"agent_name" default:"default" flag:"agent"`
	} `mapstructure:"run" yaml:"run"`
	Server *struct {
		// Listen 是 Runtime Server 的监听地址；局域网访问时设为 0.0.0.0:<port>。
		Listen string `mapstructure:"listen" yaml:"listen" default:"127.0.0.1:8790"`
		// Token 是 Runtime Server 的访问令牌，明文保存；为空时 serve 生成并复用 runtime-server.yaml 里的 token。
		Token string `mapstructure:"token" yaml:"token"`
		// WorkspaceDirectory 是新对话没指定目录时 local Executor 用的目录；为空时用 serve 的启动目录。支持 ~/ 开头。
		WorkspaceDirectory string `mapstructure:"workspace_directory" yaml:"workspace_directory"`
		// ConversationIdleTimeout 是已保存的对话没有订阅者、也没有 Turn 或后台命令在跑后，等多久停止运行实例；0 表示不停止。
		ConversationIdleTimeout time.Duration `mapstructure:"conversation_idle_timeout" yaml:"conversation_idle_timeout" default:"30m"`
	} `mapstructure:"server" yaml:"server"`
	Client *struct {
		// URL 是客户端命令（TUI、acp、app-server、codex）连接的 Runtime Server；为空时用本机 serve，没在跑就拉起。
		URL string `mapstructure:"url" yaml:"url"`
		// Token 是连接 token；URL 自带 ?token= 时以 URL 为准。
		Token string `mapstructure:"token" yaml:"token"`
		// PathMappings 把客户端路径换成 serve 路径，ACP、app-server 收发时转换，TUI 只转换启动时的 cwd；只能在配置文件中设置。
		PathMappings PathMappings `mapstructure:"path_mappings" yaml:"path_mappings"`
	} `mapstructure:"client" yaml:"client"`
	HTTP *struct {
		// Headers 是发给所有外部请求的 header 模板，按名覆盖内置默认；值为 "" 表示不发。只能在配置文件中设置。
		Headers map[string]string `mapstructure:"headers" yaml:"headers"`
	} `mapstructure:"http" yaml:"http"`
	Telemetry *struct {
		// LogLevel 是日志最低级别：debug、info、warn 或 error，同时作用于日志文件、stderr 与 OTLP。
		LogLevel string `mapstructure:"log_level" yaml:"log_level" default:"info"`
		// LogFile 是日志文件路径；为空时为 ~/.local/state/openvaala/openvaala.log，相对路径按配置文件所在目录展开。
		LogFile string `mapstructure:"log_file" yaml:"log_file"`
		// OTLPEndpoint 是 OTLP HTTP 的基础地址，例如 http://127.0.0.1:4318；各信号路径 /v1/traces、/v1/metrics、/v1/logs
		// 会自动追加在其后，不要自带信号路径；为空时不导出 trace、指标与日志。
		OTLPEndpoint string `mapstructure:"otlp_endpoint" yaml:"otlp_endpoint"`
		// OTLPHeaders 是导出时附带的请求头，例如鉴权；只能在配置文件中设置，键名会被统一转为小写。
		OTLPHeaders map[string]string `mapstructure:"otlp_headers" yaml:"otlp_headers"`
		// CaptureContent 是否把对话内容（系统指令、消息、模型输出、工具参数与结果）写入 trace？默认开启；只在配置了
		// otlp_endpoint 时随 trace 导出，日志与指标不含内容。开启时读到的敏感文件内容也可能经工具结果发往 OTLP 后端，不需要时设为 false。
		CaptureContent bool `mapstructure:"capture_content" yaml:"capture_content" default:"true"`
	} `mapstructure:"telemetry" yaml:"telemetry"`
	// home 是加载这份配置用的用户主目录，加载后只读。
	home string
	// httpHeaders 是 http.headers 合并内置默认后的结果，加载后只读。
	httpHeaders map[string]string
	// agentDefinitions 是按名字索引的 Agent 配置，没有 default.yaml 时也有内置 default Agent；加载后只读。
	agentDefinitions map[string]*AgentConfig
	// models 是按引用索引的模型目录，加载后只读。
	models map[defs.ModelRef]*defs.ModelDefinition
	// executorDefinitions 是 executors/ 里按 ID 索引的 Executor，不含 local 和内置 Executor；加载后只读。
	executorDefinitions map[string]*ExecutorConfig
	// mcpServerDefinitions 是按 ID 索引的 MCP server，包括 disabled 的和插件声明的；加载后只读。
	mcpServerDefinitions map[string]*MCPServerConfig
	// hookDefinitions 先放 hooks/ 里各文件的 hook，再按插件名顺序放插件的 hook；加载后只读。
	hookDefinitions []*hook.HookDefinition
	// plugins 是启用的插件，按名字排列，加载后只读。
	plugins []*defs.Plugin
}

// PathMapping 是一条目录映射：Client 是客户端看到的目录，Server 是 serve 上的同一个目录。
type PathMapping struct {
	Client string `mapstructure:"client" yaml:"client"`
	Server string `mapstructure:"server" yaml:"server"`
}

// PathMappings 是客户端路径和 serve 路径的映射列表，按整段目录名匹配前缀，多条覆盖时取前缀最长的一条。
// Client 以盘符开头（例如 C:\Users\me）时按 Windows 路径处理：比较时不分 \ 和 /，盘符不分大小写。
type PathMappings []PathMapping

// GetServerPath 把客户端路径换成 serve 路径；没有映射覆盖这个路径时返回 false。
func (m PathMappings) GetServerPath(clientPath string) (string, bool) {
	// 1. 找目录前缀最长的映射，没找到时直接返回。
	mapping, rest, found := getPathMapping(m, clientPath, false)
	if !found {
		return "", false
	}
	// 2. 把剩下的目录名用 / 接到 serve 目录后面。
	return joinUnixPath(mapping.Server, rest), true
}

// GetClientPath 把 serve 路径换成客户端平台上的路径；没有映射覆盖时原样返回。
func (m PathMappings) GetClientPath(serverPath string, isWindowsClient bool) string {
	// 1. 只留下和客户端平台一致的映射：Windows 客户端用盘符路径，其他平台用 Unix 路径。
	platformMappings := slices.DeleteFunc(slices.Clone(m), func(mapping PathMapping) bool { return IsWindowsPath(mapping.Client) != isWindowsClient })
	// 2. 找目录前缀最长的映射，没找到时原样返回；找到后按客户端平台的分隔符拼接。
	mapping, rest, found := getPathMapping(platformMappings, serverPath, true)
	if !found {
		return serverPath
	}
	if !isWindowsClient {
		return joinUnixPath(mapping.Client, rest)
	}
	return joinWindowsPath(mapping.Client, rest)
}

// IsWindowsPath 路径是否带 Windows 盘符？
func IsWindowsPath(value string) bool {
	// 1. 检查首字符为英文字母、第二个字符为冒号，例如 C:\Users\me 中的 C:。
	if len(value) < 2 || value[1] != ':' {
		return false
	}
	letter := value[0]
	return letter >= 'a' && letter <= 'z' || letter >= 'A' && letter <= 'Z'
}

// LoadConfigOpt 指定从哪里加载配置；零值加载当前用户的默认配置。
type LoadConfigOpt struct {
	// Home 是用户主目录，为空时取系统用户目录。
	Home string
	// Path 是配置文件路径，为空时先看 OPENVAALA_CONFIG，再用 ~/.config/openvaala/config.yaml。
	Path string
	// Flags 是命令行 flag，显式设置的值覆盖文件和环境变量。
	Flags *pflag.FlagSet
}

// LoadConfig 加载应用设置，再加载各类运行定义。
func LoadConfig(opt *LoadConfigOpt) (*Config, error) {
	// 1. 按默认值、文件、环境变量、显式 flag 逐级覆盖设置。
	cfg, _, err := loadConfigSettings(opt)
	if err != nil {
		return nil, err
	}
	// 2. 取得主目录（没传时和加载设置一样取系统用户目录），加载 Agent、模型、Executor、MCP、hook 和插件定义。
	home := opt.Home
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	cfg.home = home
	if err := cfg.loadDefinitions(home); err != nil {
		return nil, err
	}
	return cfg, nil
}

// GetDataDirectory 返回主目录下的本机数据目录。
func GetDataDirectory(home string) string {
	// 1. 拼出 <home>/.local/share/openvaala。
	return filepath.Join(home, ".local", "share", "openvaala")
}

// loadConfigSettings 解析并校验应用设置，返回设置和配置文件的绝对路径。
func loadConfigSettings(opt *LoadConfigOpt) (*Config, string, error) {
	// 1. 检查加载参数，缺少主目录时取系统用户目录，再确定配置路径。
	if opt == nil {
		return nil, "", errors.New("configuration options are required")
	}
	if opt.Home == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, "", fmt.Errorf("cannot locate user directory: %w", err)
		}
		opt = &LoadConfigOpt{
			Home:  home,
			Path:  opt.Path,
			Flags: opt.Flags,
		}
	}
	path, explicit, err := GetConfigPath(opt.Home, opt.Path)
	if err != nil {
		return nil, "", err
	}

	// 2. 注册默认值、环境变量和 flag 映射。
	settings := viper.New()
	settings.SetConfigType("yaml")
	settings.SetEnvPrefix("OPENVAALA")
	settings.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	settings.AllowEmptyEnv(true)
	settings.AutomaticEnv()
	if err := bindConfigFields(settings, reflect.TypeFor[Config](), "", opt.Flags); err != nil {
		return nil, "", err
	}

	// 3. 读取配置文件；默认文件缺失时沿用默认值，显式文件缺失时报错，读取后严格校验字段再合并。
	payload, err := os.ReadFile(path)
	if err != nil && (!errors.Is(err, os.ErrNotExist) || explicit) {
		return nil, "", &Error{
			Path:   path,
			Reason: "cannot read file",
		}
	}
	if err == nil {
		// 按 Config 检查文件字段，包括未知的空配置组。
		fileConfig := &Config{}
		if err := decodeYAML(bytes.NewReader(payload), fileConfig); err != nil {
			return nil, "", &Error{
				Path:   path,
				Reason: "invalid YAML, field type or unknown field",
			}
		}
		if err := settings.ReadConfig(bytes.NewReader(payload)); err != nil {
			return nil, "", &Error{
				Path:   path,
				Reason: "invalid configuration",
			}
		}
	}

	// 4. 解出最终设置并校验，通过后把 http.headers 合并进内置默认 header；默认值和环境变量里的字符串转成布尔和整数，转不了时报类型错误。
	cfg := &Config{}
	if err := settings.UnmarshalExact(cfg,
		func(decoder *mapstructure.DecoderConfig) {
			decoder.WeaklyTypedInput = false
			decoder.DecodeHook = mapstructure.ComposeDecodeHookFunc(decoder.DecodeHook,
				mapstructure.StringToBoolHookFunc(), mapstructure.StringToIntHookFunc())
		},
	); err != nil {
		return nil, "", &Error{
			Path:   path,
			Reason: "unknown field or invalid field type: " + err.Error(),
		}
	}
	if err := cfg.Validate(); err != nil {
		return nil, "", fmt.Errorf("%s: %w", path, err)
	}
	// 没写 http 组时 HTTP 为 nil，按没有用户 header 处理。
	var userHeaders map[string]string
	if cfg.HTTP != nil {
		userHeaders = cfg.HTTP.Headers
	}
	cfg.httpHeaders = httpclient.MergeHeaders(defaultHTTPHeaders, userHeaders)
	// 5. 按配置文件位置展开实体目录和日志路径。
	cfg.Agents.Directory = expandPath(cfg.Agents.Directory, opt.Home, filepath.Dir(path))
	cfg.Models.Directory = expandPath(cfg.Models.Directory, opt.Home, filepath.Dir(path))
	cfg.Skills.Directory = expandPath(cfg.Skills.Directory, opt.Home, filepath.Dir(path))
	cfg.Executors.Directory = expandPath(cfg.Executors.Directory, opt.Home, filepath.Dir(path))
	cfg.MCPServers.Directory = expandPath(cfg.MCPServers.Directory, opt.Home, filepath.Dir(path))
	cfg.Hooks.Directory = expandPath(cfg.Hooks.Directory, opt.Home, filepath.Dir(path))
	cfg.Plugins.Directory = expandPath(cfg.Plugins.Directory, opt.Home, filepath.Dir(path))
	cfg.Telemetry.LogFile = getTelemetryLogFile(cfg.Telemetry.LogFile, opt.Home, filepath.Dir(path))
	// 6. 展开 server.workspace_directory 的 ~，展开后不是绝对路径时报错。
	if cfg.Server.WorkspaceDirectory != "" {
		cfg.Server.WorkspaceDirectory = expandPath(cfg.Server.WorkspaceDirectory, opt.Home, "")
		if !filepath.IsAbs(cfg.Server.WorkspaceDirectory) {
			return nil, "", fmt.Errorf("%s: server.workspace_directory must be an absolute path", path)
		}
	}
	return cfg, path, nil
}

// Validate 校验应用设置。
func (cfg *Config) Validate() error {
	// 1. 检查配置组、实体目录和监听地址。
	if cfg == nil || cfg.Agents == nil || cfg.Models == nil || cfg.Models.Defaults == nil || cfg.Skills == nil || cfg.Executors == nil || cfg.MCPServers == nil || cfg.Hooks == nil || cfg.Plugins == nil || cfg.Tools == nil || cfg.Run == nil || cfg.Server == nil || cfg.Client == nil || cfg.Telemetry == nil {
		return errors.New("configuration groups are required")
	}
	if strings.TrimSpace(cfg.Agents.Directory) == "" {
		return errors.New("agents.directory must not be empty")
	}
	if strings.TrimSpace(cfg.Models.Directory) == "" {
		return errors.New("models.directory must not be empty")
	}
	if strings.TrimSpace(cfg.Skills.Directory) == "" {
		return errors.New("skills.directory must not be empty")
	}
	if strings.TrimSpace(cfg.Executors.Directory) == "" {
		return errors.New("executors.directory must not be empty")
	}
	if strings.TrimSpace(cfg.MCPServers.Directory) == "" {
		return errors.New("mcp_servers.directory must not be empty")
	}
	if strings.TrimSpace(cfg.Hooks.Directory) == "" {
		return errors.New("hooks.directory must not be empty")
	}
	if strings.TrimSpace(cfg.Plugins.Directory) == "" {
		return errors.New("plugins.directory must not be empty")
	}
	if strings.TrimSpace(cfg.Server.Listen) == "" {
		return errors.New("server.listen must not be empty")
	}
	if cfg.Server.ConversationIdleTimeout < 0 {
		return errors.New("server.conversation_idle_timeout must not be negative")
	}
	// 2. 检查客户端与服务端映射均为绝对路径，且各自没有重复目录。
	clients, servers := map[string]bool{}, map[string]bool{}
	for index, mapping := range cfg.Client.PathMappings {
		if !isAbsoluteClientPath(mapping.Client) || !strings.HasPrefix(mapping.Server, "/") {
			return fmt.Errorf("client.path_mappings[%d]: client and server must be absolute paths", index)
		}
		clientKey, serverKey := getPathKey(mapping.Client), getPathKey(mapping.Server)
		if clients[clientKey] {
			return fmt.Errorf("client.path_mappings: client %q is used by more than one mapping", mapping.Client)
		}
		if servers[serverKey] {
			return fmt.Errorf("client.path_mappings: server %q is used by more than one mapping", mapping.Server)
		}
		clients[clientKey], servers[serverKey] = true, true
	}
	// 3. 检查图片大小、Agent 名、日志级别和 OTLP 地址。
	if cfg.Tools.MaxImageKB <= 0 {
		return errors.New("tools.max_image_kb must be positive")
	}
	if !isValidID(cfg.Run.AgentName) {
		return errors.New("run.agent_name must be a valid definition name")
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.Telemetry.LogLevel)); err != nil {
		return errors.New("telemetry.log_level must be debug, info, warn or error")
	}
	if cfg.Telemetry.OTLPEndpoint != "" {
		endpoint, err := url.Parse(cfg.Telemetry.OTLPEndpoint)
		// 出错时只返回固定提示，不带原 URL。
		if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" {
			return errors.New("telemetry.otlp_endpoint must be an http or https URL such as http://127.0.0.1:4318")
		}
	}
	// 4. 检查 http.headers 里的变量都认识；没写 http 组时 HTTP 为 nil。
	if cfg.HTTP != nil {
		if err := validateHeaderTemplates(cfg.HTTP.Headers); err != nil {
			return fmt.Errorf("http.headers.%w", err)
		}
	}
	// 5. 检查模型默认窗口、输出上限、推理强度、合并角色和输入类型。
	defaults := cfg.Models.Defaults
	if defaults.ContextWindow <= 0 || defaults.MaxOutputTokens <= 0 {
		return errors.New("models.defaults.context_window and max_output_tokens must be positive")
	}
	if err := validateReasoningEfforts(defaults.ReasoningEfforts); err != nil {
		return fmt.Errorf("models.defaults: %w", err)
	}
	if _, err := getMessageRoles(defaults.MergeMessageRoles); err != nil {
		return fmt.Errorf("models.defaults: %w", err)
	}
	if _, err := getModelInputs(defaults.Capabilities.Input); err != nil {
		return fmt.Errorf("models.defaults: %w", err)
	}
	return nil
}

// GetConfigPath 确定配置文件的绝对路径，并返回它是不是调用方或 OPENVAALA_CONFIG 显式指定的。
func GetConfigPath(home, requested string) (string, bool, error) {
	// 1. 取得主目录，必须是绝对路径。
	if home == "" {
		directory, err := os.UserHomeDir()
		if err != nil {
			return "", false, fmt.Errorf("cannot locate user directory: %w", err)
		}
		home = directory
	}
	if !filepath.IsAbs(home) {
		return "", false, errors.New("an absolute user directory is required")
	}
	// 2. 优先用调用方给的路径，其次 OPENVAALA_CONFIG；环境变量设了但为空时报错。
	explicit := requested != ""
	if requested == "" {
		if value, present := os.LookupEnv("OPENVAALA_CONFIG"); present {
			if strings.TrimSpace(value) == "" {
				return "", false, errors.New("OPENVAALA_CONFIG must not be empty")
			}
			requested, explicit = value, true
		}
	}
	// 3. 未指定时使用 ~/.config/openvaala/config.yaml，展开 ~ 并转为绝对路径。
	if requested == "" {
		requested = filepath.Join(home, ".config", "openvaala", "config.yaml")
	}
	path, err := filepath.Abs(expandPath(requested, home, "."))
	return path, explicit, err
}

// GetTelemetryLogLevel 返回配置中的日志最低级别。
func (cfg *Config) GetTelemetryLogLevel() slog.Level {
	// 1. 解析日志级别，Validate 已经检查过它。
	var level slog.Level
	_ = level.UnmarshalText([]byte(cfg.Telemetry.LogLevel))
	return level
}

// GetHTTPHeaders 返回 http.headers 合并内置默认后的 header 模板副本。
func (cfg *Config) GetHTTPHeaders() map[string]string {
	// 1. 返回副本，调用方修改不影响保存的值。
	return maps.Clone(cfg.httpHeaders)
}

// GetAgent 按名字返回 Agent 配置的副本。
func (cfg *Config) GetAgent(name string) (*AgentConfig, error) {
	// 1. 查找 Agent，没配置时返回错误，找到时返回副本。
	definition, found := cfg.agentDefinitions[name]
	if !found {
		return nil, fmt.Errorf("agent %q is not configured", name)
	}
	return definition.Clone(), nil
}

// GetAgentDefinitions 按名字顺序返回各 Agent 运行定义的副本。
func (cfg *Config) GetAgentDefinitions() []*agent.AgentDefinition {
	// 1. 按名字顺序把每个 Agent 配置转成运行定义；没配的主模型或摘要模型是零值。
	definitions := make([]*agent.AgentDefinition, 0, len(cfg.agentDefinitions))
	for _, name := range slices.Sorted(maps.Keys(cfg.agentDefinitions)) {
		agentConfig := cfg.agentDefinitions[name]
		definition := &agent.AgentDefinition{
			Name:         agentConfig.Name,
			Description:  agentConfig.Description,
			Instructions: agentConfig.Instructions,
			Model:        getModelRef(agentConfig.Model),
			CompactModel: getModelRef(agentConfig.CompactModel),
			ToolNames:    agentConfig.Tools,
			SkillNames:   agentConfig.Skills,
			MCPServerIDs: agentConfig.MCPServers,
			MaxSteps:     agentConfig.MaxSteps,
		}
		definitions = append(definitions, definition.Clone())
	}
	return definitions
}

// GetModels 按 Provider ID、模型 ID 顺序返回模型定义的副本。
func (cfg *Config) GetModels() []*defs.ModelDefinition {
	// 1. 排好模型引用，逐个复制定义。
	refs := cfg.getSortedModelRefs()
	models := make([]*defs.ModelDefinition, 0, len(refs))
	for _, ref := range refs {
		models = append(models, cfg.models[ref].Clone())
	}
	return models
}

// getSortedModelRefs 返回按 Provider ID、模型 ID 排序的模型引用。
func (cfg *Config) getSortedModelRefs() []defs.ModelRef {
	// 1. 先比较 Provider ID，相同时再比较模型 ID。
	return slices.SortedFunc(maps.Keys(cfg.models), func(a, b defs.ModelRef) int {
		return cmp.Or(strings.Compare(a.ProviderID, b.ProviderID), strings.Compare(a.ModelID, b.ModelID))
	})
}

// getDefaultModel 取没写 model 的 Agent 使用的默认模型；模型目录为空时返回 nil。
func (cfg *Config) getDefaultModel() (*AgentModelConfig, error) {
	// 1. 配了 models.default_model 时，它必须能解析并且在模型目录里。
	if cfg.Models.DefaultModel != "" {
		ref, err := defs.ParseModelRef(cfg.Models.DefaultModel)
		if err != nil {
			return nil, fmt.Errorf("models.default_model: %w", err)
		}
		if _, found := cfg.models[ref]; !found {
			return nil, fmt.Errorf("models.default_model: model %s is not configured", ref)
		}
		return &AgentModelConfig{
			Provider: ref.ProviderID,
			Model:    ref.ModelID,
		}, nil
	}
	// 2. 没配时取排序后的第一个模型，目录为空时返回 nil。
	refs := cfg.getSortedModelRefs()
	if len(refs) == 0 {
		return nil, nil
	}
	return &AgentModelConfig{
		Provider: refs[0].ProviderID,
		Model:    refs[0].ModelID,
	}, nil
}

// newModelDefinition 用模型配置覆盖全局默认值，生成完整的模型定义。
func (cfg *Config) newModelDefinition(provider *defs.ProviderConfig, modelID string, model *defs.ProviderModelConfig) (*defs.ModelDefinition, error) {
	// 1. 模型没写协议时用 Provider 的协议；Provider 给这个协议配了完整地址就用它，没有就在 base_url 后接默认路径。
	defaults := cfg.Models.Defaults
	protocol := llmkit.Protocol(cmp.Or(model.Protocol, provider.Protocol))
	endpoint, found := provider.Endpoints[string(protocol)]
	if !found {
		endpoint = strings.TrimRight(provider.BaseURL, "/") + getDefaultEndpointSuffix(protocol)
	}
	baseURL, endpointPath := splitProviderURL(endpoint)
	// 2. 填入模型引用、显示名、用途说明、请求目标和输出设置，模型写了的字段覆盖全局默认；窗口和输出上限不能为负。
	definition := &defs.ModelDefinition{
		Ref: defs.ModelRef{
			ProviderID: provider.ID,
			ModelID:    modelID,
		},
		DisplayName: cmp.Or(model.DisplayName, modelID),
		Description: model.Description,
		Target: &llmkit.Target{
			Protocol:     protocol,
			BaseURL:      baseURL,
			EndpointPath: endpointPath,
			APIKey:       provider.APIKey,
			Model:        model.UpstreamModel,
			Headers:      getTargetHeaders(httpclient.MergeHeaders(cfg.httpHeaders, provider.Headers)),
		},
		ContextWindow:   cmp.Or(model.ContextWindow, defaults.ContextWindow),
		MaxOutputTokens: cmp.Or(model.MaxOutputTokens, defaults.MaxOutputTokens),
		ThinkingEnabled: defaults.Thinking,
	}
	if model.Thinking != nil {
		definition.ThinkingEnabled = *model.Thinking
	}
	if definition.ContextWindow < 0 || definition.MaxOutputTokens < 0 {
		return nil, fmt.Errorf("model %s: context_window and max_output_tokens must not be negative", modelID)
	}
	// 3. 取推理强度列表（模型写了就用模型的）并校验，再定默认推理强度。
	efforts := defaults.ReasoningEfforts
	if model.ReasoningEfforts != nil {
		efforts = model.ReasoningEfforts
	}
	if err := validateReasoningEfforts(efforts); err != nil {
		return nil, fmt.Errorf("model %s: %w", modelID, err)
	}
	defaultEffort, err := getDefaultReasoningEffort(efforts, model.DefaultReasoningEffort, defaults.DefaultReasoningEffort)
	if err != nil {
		return nil, fmt.Errorf("model %s: %w", modelID, err)
	}
	// 4. 同样取合并消息角色和输入类型并校验；reasoning_context 只看模型自己写的值，一并校验后写入定义。
	roles := defaults.MergeMessageRoles
	if model.MergeMessageRoles != nil {
		roles = model.MergeMessageRoles
	}
	mergeRoles, err := getMessageRoles(roles)
	if err != nil {
		return nil, fmt.Errorf("model %s: %w", modelID, err)
	}
	reasoningContext, err := getReasoningContext(protocol, model.ReasoningContext)
	if err != nil {
		return nil, fmt.Errorf("model %s: %w", modelID, err)
	}
	inputs := defaults.Capabilities.Input
	if model.Capabilities.Input != nil {
		inputs = model.Capabilities.Input
	}
	modelInputs, err := getModelInputs(inputs)
	if err != nil {
		return nil, fmt.Errorf("model %s: %w", modelID, err)
	}
	definition.ReasoningEfforts = slices.Clone(efforts)
	definition.DefaultReasoningEffort = defaultEffort
	definition.MergeMessageRoles = mergeRoles
	definition.ReasoningContext = reasoningContext
	definition.Capabilities.Input = modelInputs
	return definition, nil
}

// GetExecutors 按 ID 顺序返回 executors/ 里 Executor 配置的副本，不含 local 和内置 Executor。
func (cfg *Config) GetExecutors() []*ExecutorConfig {
	// 1. 按 ID 顺序复制配置，SSH 段另外复制一份。
	executors := make([]*ExecutorConfig, 0, len(cfg.executorDefinitions))
	for _, id := range slices.Sorted(maps.Keys(cfg.executorDefinitions)) {
		definition := *cfg.executorDefinitions[id]
		if definition.SSH != nil {
			sshConfig := *definition.SSH
			definition.SSH = &sshConfig
		}
		executors = append(executors, &definition)
	}
	return executors
}

// GetMCPServers 按 ID 顺序返回没被 disabled 的 MCP server 运行定义。
func (cfg *Config) GetMCPServers() []*mcpclient.ServerDefinition {
	// 1. 按 ID 顺序遍历，跳过 disabled 的 server。
	servers := make([]*mcpclient.ServerDefinition, 0, len(cfg.mcpServerDefinitions))
	for _, id := range slices.Sorted(maps.Keys(cfg.mcpServerDefinitions)) {
		server := cfg.mcpServerDefinitions[id]
		if server.Disabled {
			continue
		}
		// 2. 复制配置：没写 executors 时用 local，没写超时时启动等 30 秒、工具调用等 300 秒；插件指针直接共用。
		executorIDs := slices.Clone(server.Executors)
		if len(executorIDs) == 0 {
			executorIDs = []string{defs.LocalExecutorID}
		}
		// stdio 不发 HTTP 请求，没有 header；http、sse 用全局 header 加上自己的，同名的以自己的为准。
		var headers map[string]string
		if mcpclient.ServerTransportType(server.Type) != mcpclient.ServerTransportStdio {
			headers = httpclient.MergeHeaders(cfg.httpHeaders, server.Headers)
		}
		servers = append(servers, &mcpclient.ServerDefinition{
			ID:             server.ID,
			Type:           mcpclient.ServerTransportType(server.Type),
			Command:        server.Command,
			Args:           slices.Clone(server.Args),
			Env:            maps.Clone(server.Env),
			URL:            server.URL,
			Headers:        headers,
			ExecutorIDs:    executorIDs,
			ToolNames:      slices.Clone(server.Tools),
			Required:       server.Required,
			StartupTimeout: cmp.Or(time.Duration(server.StartupTimeoutSeconds)*time.Second, mcpclient.DefaultStartupTimeout),
			ToolTimeout:    cmp.Or(time.Duration(server.ToolTimeoutSeconds)*time.Second, mcpclient.DefaultToolTimeout),
			Plugin:         server.Plugin,
		})
	}
	return servers
}

// GetHooks 按加载顺序返回 hook 定义的副本：先 hooks/ 目录，再插件。
func (cfg *Config) GetHooks() []*hook.HookDefinition {
	// 1. 逐个复制定义和参数，插件指针直接共用。
	hooks := make([]*hook.HookDefinition, len(cfg.hookDefinitions))
	for index, definition := range cfg.hookDefinitions {
		copied := *definition
		copied.Args = slices.Clone(definition.Args)
		hooks[index] = &copied
	}
	return hooks
}

// GetRuntimeDefinitions 组装 Runtime 使用的整份定义：Executor、共用 skill、Agent、模型、MCP server 和 hook。
func (cfg *Config) GetRuntimeDefinitions(ctx context.Context) (*runtime.Definitions, error) {
	// 1. 先放 local、内置 Executor 和 memory，再按 ID 顺序追加 executors/ 里的 Executor。
	localExecutor, err := cfg.newLocalExecutor()
	if err != nil {
		return nil, err
	}
	builtinExecutor, err := newBuiltinExecutor()
	if err != nil {
		return nil, err
	}
	executors := []executor.Executor{localExecutor, builtinExecutor, cfg.newMemoryExecutor()}
	for _, executorConfig := range cfg.GetExecutors() {
		executors = append(executors, newConfiguredExecutor(executorConfig))
	}
	// 2. 加载共用 skill，和 Agent、模型、MCP server、hook、Executor 一起返回。
	skills, err := cfg.loadSharedSkills(ctx, localExecutor, builtinExecutor)
	if err != nil {
		return nil, err
	}
	return &runtime.Definitions{
		Agents:     cfg.GetAgentDefinitions(),
		Models:     cfg.GetModels(),
		MCPServers: cfg.GetMCPServers(),
		Hooks:      cfg.GetHooks(),
		Executors:  executor.NewConfigExecutorProvider(executors),
		Skills:     skills,
	}, nil
}

// newLocalExecutor 创建 local Executor。
func (cfg *Config) newLocalExecutor() (executor.Executor, error) {
	// 1. 工作目录用 server.workspace_directory，没写时取 serve 的启动目录。
	workspaceDirectory := cfg.Server.WorkspaceDirectory
	if workspaceDirectory == "" {
		var err error
		if workspaceDirectory, err = os.Getwd(); err != nil {
			return nil, fmt.Errorf("locate working directory: %w", err)
		}
	}
	return executor.NewLocalExecutor(&defs.ExecutorDefinition{
		ID:                 defs.LocalExecutorID,
		Kind:               defs.ExecutorKindLocal,
		Description:        prompt.LocalExecutorDescription,
		WorkspaceDirectory: workspaceDirectory,
	}), nil
}

// newMemoryExecutor 创建每个对话自带的 memory Executor：没有目录，不能当默认 Executor。
func (cfg *Config) newMemoryExecutor() executor.Executor {
	// 1. 记忆根目录是数据目录下的 memory，第一次写入时才建；工作区由 Conversation 按默认 Executor 打开。
	return executor.NewMemoryExecutor(executor.NewMemoryExecutorOpt(&defs.ExecutorDefinition{
		ID:          defs.MemoryExecutorID,
		Kind:        defs.ExecutorKindMemory,
		Description: prompt.MemoryExecutorDescription,
	}, filepath.Join(GetDataDirectory(cfg.home), "memory")))
}

// loadSharedSkills 依次加载 skills/ 目录、内置目录和插件目录里的 skill，所有对话共用；同名的以先扫描到的为准。
func (cfg *Config) loadSharedSkills(ctx context.Context, localExecutor, builtinExecutor executor.Executor) ([]*skill.Skill, error) {
	// 1. 打开本机工作区，以及装着内置文件的虚拟工作区，返回时关闭。
	localWorkspace, err := localExecutor.NewWorkspace(ctx, &executor.NewWorkspaceRequest{RootDirectory: localExecutor.GetDefinition().WorkspaceDirectory})
	if err != nil {
		return nil, err
	}
	defer closeWorkspace(ctx, localWorkspace)
	builtinWorkspace, err := builtinExecutor.NewWorkspace(ctx, &executor.NewWorkspaceRequest{})
	if err != nil {
		return nil, err
	}
	defer closeWorkspace(ctx, builtinWorkspace)
	// 2. 先放 skills/ 目录，再放内置目录，两处的 skill 格式错误都报错。
	directories := []skill.SkillDirectory{
		{
			Workspace:  localWorkspace,
			ExecutorID: defs.LocalExecutorID,
			Path:       cfg.Skills.Directory,
			Strict:     true,
		},
		{
			Workspace:  builtinWorkspace,
			ExecutorID: BuiltinExecutorID,
			Path:       builtinSkillDirectory,
			Strict:     true,
		},
	}
	// 3. 追加插件的 skill 目录，名字加 "<插件名>:" 前缀，格式错误的跳过。
	for _, plugin := range cfg.plugins {
		for _, directory := range plugin.SkillDirectories {
			directories = append(directories, skill.SkillDirectory{
				Workspace:  localWorkspace,
				ExecutorID: defs.LocalExecutorID,
				Path:       directory,
				NamePrefix: plugin.Name + ":",
			})
		}
	}
	return skill.LoadSkills(ctx, directories)
}

// loadDefinitions 依次加载并校验模型、Executor、MCP server、hook、插件和 Agent 定义。
func (cfg *Config) loadDefinitions(home string) error {
	// 1. 读取 Provider 文件，套用全局默认值，建立模型目录。
	cfg.models = make(map[defs.ModelRef]*defs.ModelDefinition)
	providerFiles, err := getDefinitionFiles(cfg.Models.Directory)
	if err != nil {
		return err
	}
	for _, path := range providerFiles {
		provider, err := loadProviderDefinition(path)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		for modelID, model := range provider.Models {
			definition, err := cfg.newModelDefinition(provider, modelID, model)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			cfg.models[definition.Ref] = definition
		}
	}

	// 2. 加载 Executor，再加载引用这些 Executor 的 MCP server。
	cfg.executorDefinitions = make(map[string]*ExecutorConfig)
	executorFiles, err := getDefinitionFiles(cfg.Executors.Directory)
	if err != nil {
		return err
	}
	for _, path := range executorFiles {
		definition, err := loadExecutorDefinition(path, home)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		cfg.executorDefinitions[definition.ID] = definition
	}

	cfg.mcpServerDefinitions = make(map[string]*MCPServerConfig)
	mcpServerFiles, err := getDefinitionFiles(cfg.MCPServers.Directory)
	if err != nil {
		return err
	}
	for _, path := range mcpServerFiles {
		definition, err := loadMCPServerDefinition(path, cfg.executorDefinitions)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		cfg.mcpServerDefinitions[definition.ID] = definition
	}

	// 3. 加载 hook 文件，再加载插件（追加插件的 hook 和 MCP server），最后按顺序给全部 hook 编 DisplayOrder。
	cfg.hookDefinitions = nil
	hookFiles, err := getDefinitionFiles(cfg.Hooks.Directory)
	if err != nil {
		return err
	}
	for _, path := range hookFiles {
		definitions, err := loadHookFile(path, home)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		cfg.hookDefinitions = append(cfg.hookDefinitions, definitions...)
	}
	if err := cfg.loadPlugins(home); err != nil {
		return err
	}
	for index, definition := range cfg.hookDefinitions {
		definition.DisplayOrder = index
	}

	// 4. 取默认模型并加载 Agent：没写 model 的用默认模型，再检查主模型、摘要模型和 MCP server 引用都已配置。
	defaultModel, err := cfg.getDefaultModel()
	if err != nil {
		return err
	}
	cfg.agentDefinitions = make(map[string]*AgentConfig)
	agentFiles, err := getDefinitionFiles(cfg.Agents.Directory)
	if err != nil {
		return err
	}
	for _, path := range agentFiles {
		definition, err := loadAgentDefinition(path)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if definition.Model == nil {
			definition.Model = defaultModel.Clone()
		}
		var model *defs.ModelDefinition
		if definition.Model != nil {
			var found bool
			model, found = cfg.models[getModelRef(definition.Model)]
			if !found {
				return fmt.Errorf("%s: model %q is not configured in provider %q", path, definition.Model.Model, definition.Model.Provider)
			}
		}
		if definition.CompactModel != nil {
			compactModel, found := cfg.models[getModelRef(definition.CompactModel)]
			if !found {
				return fmt.Errorf("%s: compact_model: model %q is not configured in provider %q", path, definition.CompactModel.Model, definition.CompactModel.Provider)
			}
			// 摘要模型的上下文窗口不能比主模型小。
			if compactModel.ContextWindow < model.ContextWindow {
				return fmt.Errorf("%s: compact_model context window %d is smaller than model context window %d",
					path, compactModel.ContextWindow, model.ContextWindow)
			}
		}
		// disabled 的 MCP server 也算配置过。
		for _, serverID := range definition.MCPServers {
			if _, found := cfg.mcpServerDefinitions[serverID]; !found {
				return fmt.Errorf("%s: mcp server %q is not configured", path, serverID)
			}
		}
		cfg.agentDefinitions[definition.Name] = definition
	}
	// 5. 没有 default Agent 时补上内置定义，模型用默认模型。
	if _, found := cfg.agentDefinitions[defaultAgentName]; !found {
		builtinAgent := newBuiltinDefaultAgent()
		builtinAgent.Model = defaultModel.Clone()
		cfg.agentDefinitions[defaultAgentName] = builtinAgent
	}
	return nil
}

// newBuiltinDefaultAgent 创建内置 default Agent 配置。
func newBuiltinDefaultAgent() *AgentConfig {
	// 1. 填入内置名字、用途说明和指令；工具、skill 和 MCP server 不填，即全部可用。
	return &AgentConfig{
		Name:         defaultAgentName,
		Description:  prompt.DefaultAgentDescription,
		Instructions: prompt.DefaultAgentInstructions,
	}
}

// loadAgentDefinition 读取并校验 Agent 文件。
func loadAgentDefinition(path string) (*AgentConfig, error) {
	// 1. default.yaml 从内置 default Agent 起步，其他文件从空配置起步，再读文件覆盖。
	name := strings.TrimSuffix(filepath.Base(path), ".yaml")
	definition := &AgentConfig{}
	if name == defaultAgentName {
		definition = newBuiltinDefaultAgent()
	}
	if err := readYAML(path, definition); err != nil {
		return nil, err
	}
	// 2. 检查名字和文件名一致、指令非空；模型引用没写 model 时填 default。
	if !isValidID(definition.Name) || definition.Name != name {
		return nil, errors.New("agent name must be valid and match filename")
	}
	if strings.TrimSpace(definition.Instructions) == "" {
		return nil, errors.New("agent instructions must not be empty")
	}
	if definition.MaxSteps < 0 {
		return nil, errors.New("agent max_steps must not be negative")
	}
	if definition.Model != nil && definition.Model.Model == "" {
		definition.Model.Model = "default"
	}
	if definition.CompactModel != nil && definition.CompactModel.Model == "" {
		definition.CompactModel.Model = "default"
	}
	return definition, nil
}

// loadProviderDefinition 读取并校验 Provider 及其模型配置。
func loadProviderDefinition(path string) (*defs.ProviderConfig, error) {
	// 1. 读取文件，检查 Provider ID 与文件名一致。
	provider := &defs.ProviderConfig{}
	if err := readYAML(path, provider); err != nil {
		return nil, err
	}
	id := strings.TrimSuffix(filepath.Base(path), ".yaml")
	if !isValidID(provider.ID) || provider.ID != id {
		return nil, errors.New("provider id must be valid and match filename")
	}
	// 2. 把协议换成规范写法（没写时用 OpenAI Responses），检查 base_url。
	protocol, supported := getProviderProtocol(cmp.Or(provider.Protocol, string(llmkit.ProtocolOpenAIResponses)))
	if !supported {
		return nil, errors.New("unsupported provider protocol")
	}
	provider.Protocol = string(protocol)
	if !isValidProviderURL(provider.BaseURL) {
		return nil, errors.New("invalid provider base_url")
	}
	// 3. 检查 endpoints 的键是规范协议名（不接受 openai-responses 这种别名），值是完整的 HTTP 请求地址。
	for protocolName, endpoint := range provider.Endpoints {
		endpointProtocol, supported := getProviderProtocol(protocolName)
		if !supported || string(endpointProtocol) != protocolName || !isValidEndpointURL(endpoint) {
			return nil, errors.New("invalid provider endpoints")
		}
	}
	// 4. 检查 headers 里的变量都认识。
	if err := validateHeaderTemplates(provider.Headers); err != nil {
		return nil, fmt.Errorf("headers.%w", err)
	}
	// 5. 检查模型列表、模型 ID 和协议；没写 upstream_model 时用模型 ID，写了空白时报错。
	if len(provider.Models) == 0 {
		return nil, errors.New("provider models must not be empty")
	}
	for id, model := range provider.Models {
		if model == nil {
			model = &defs.ProviderModelConfig{}
		}
		if !isValidID(id) {
			return nil, errors.New("invalid provider model id")
		}
		if model.Protocol != "" {
			modelProtocol, supported := getProviderProtocol(model.Protocol)
			if !supported {
				return nil, errors.New("unsupported provider protocol")
			}
			model.Protocol = string(modelProtocol)
		}
		if model.UpstreamModel == "" {
			model.UpstreamModel = id
		}
		if strings.TrimSpace(model.UpstreamModel) == "" {
			return nil, errors.New("upstream_model must not be blank")
		}
		provider.Models[id] = model
	}
	return provider, nil
}

// loadExecutorDefinition 读取并校验一个 Executor 文件。
func loadExecutorDefinition(path, home string) (*ExecutorConfig, error) {
	// 1. 读取配置，检查 ID 和文件名一致、不是保留的 local、client、openvaala、memory，说明非空。
	definition := &ExecutorConfig{}
	if err := readYAML(path, definition); err != nil {
		return nil, err
	}
	name := strings.TrimSuffix(filepath.Base(path), ".yaml")
	if !isValidID(definition.ID) || definition.ID != name {
		return nil, errors.New("executor id must be valid and match filename")
	}
	if definition.ID == defs.LocalExecutorID || definition.ID == defs.ClientExecutorID || definition.ID == BuiltinExecutorID ||
		definition.ID == defs.MemoryExecutorID {
		return nil, fmt.Errorf("executor id %q is reserved", definition.ID)
	}
	if strings.TrimSpace(definition.Description) == "" {
		return nil, errors.New("executor description must not be empty")
	}
	// 2. 按 kind 检查：local 展开 ~ 后必须是已有的绝对目录；virtual 默认 /workspace；ssh 要求远端绝对路径和地址，写了私钥时文件必须存在。
	switch definition.Kind {
	case defs.ExecutorKindLocal:
		definition.WorkspaceDirectory = expandPath(definition.WorkspaceDirectory, home, "")
		if !filepath.IsAbs(definition.WorkspaceDirectory) {
			return nil, errors.New("executor workspace_directory must be an absolute path or start with ~")
		}
		if info, err := os.Stat(definition.WorkspaceDirectory); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("executor workspace_directory %s must be an existing directory", definition.WorkspaceDirectory)
		}
	case defs.ExecutorKindVirtual:
		if definition.WorkspaceDirectory == "" {
			definition.WorkspaceDirectory = virtualWorkspaceDirectory
		}
		if !filepath.IsAbs(definition.WorkspaceDirectory) {
			return nil, errors.New("virtual executor workspace_directory must be an absolute path")
		}
	case defs.ExecutorKindSSH:
		if !filepath.IsAbs(definition.WorkspaceDirectory) {
			return nil, errors.New("ssh executor workspace_directory must be an absolute remote path")
		}
		if definition.SSH == nil || strings.TrimSpace(definition.SSH.Address) == "" {
			return nil, errors.New("ssh executor requires ssh.address")
		}
		// 私钥路径展开 ~，相对路径按 Executor 文件所在目录算。
		if definition.SSH.IdentityFile != "" {
			definition.SSH.IdentityFile = expandPath(definition.SSH.IdentityFile, home, filepath.Dir(path))
			if info, err := os.Stat(definition.SSH.IdentityFile); err != nil || !info.Mode().IsRegular() {
				return nil, fmt.Errorf("ssh identity_file %s must be an existing file", definition.SSH.IdentityFile)
			}
		}
	default:
		return nil, fmt.Errorf("executor kind %q is not supported; use %q, %q or %q", definition.Kind, defs.ExecutorKindLocal, defs.ExecutorKindVirtual, defs.ExecutorKindSSH)
	}
	return definition, nil
}

// loadMCPServerDefinition 读取并校验 mcp_servers/ 下的一个文件。
func loadMCPServerDefinition(path string, executors map[string]*ExecutorConfig) (*MCPServerConfig, error) {
	// 1. 读取配置，检查 ID 和文件名一致，再校验传输方式和 Executor。
	definition := &MCPServerConfig{}
	if err := readYAML(path, definition); err != nil {
		return nil, err
	}
	if definition.ID != strings.TrimSuffix(filepath.Base(path), ".yaml") {
		return nil, errors.New("mcp server id must match filename")
	}
	if err := validateMCPServerConfig(definition, executors); err != nil {
		return nil, err
	}
	return definition, nil
}

// validateMCPServerConfig 校验 mcp_servers/ 文件或插件声明的 MCP server。
func validateMCPServerConfig(definition *MCPServerConfig, executors map[string]*ExecutorConfig) error {
	// 1. 检查 ID 只含字母、数字、- 和 _，不含 __。
	if !isValidID(definition.ID) || strings.Contains(definition.ID, ".") || strings.Contains(definition.ID, "__") {
		return errors.New("mcp server id must contain only letters, digits, - and _, without __")
	}
	// 2. 检查 stdio 命令或 HTTP/SSE 地址，拒绝混用字段。
	switch mcpclient.ServerTransportType(definition.Type) {
	case mcpclient.ServerTransportStdio:
		if strings.TrimSpace(definition.Command) == "" || definition.URL != "" || len(definition.Headers) > 0 {
			return errors.New("stdio mcp server requires command and does not use url or headers")
		}
	case mcpclient.ServerTransportHTTP, mcpclient.ServerTransportSSE:
		address, err := url.Parse(definition.URL)
		if err != nil || (address.Scheme != "http" && address.Scheme != "https") || address.Host == "" ||
			definition.Command != "" || len(definition.Args) > 0 || len(definition.Env) > 0 {
			// 出错时只返回固定提示，不带原 URL。
			return errors.New("http and sse mcp servers require an http or https url and do not use command, args or env")
		}
	default:
		return fmt.Errorf("mcp server type %q is not supported; use stdio, http or sse", definition.Type)
	}
	// 3. 检查 headers 里的变量都认识。
	if err := validateHeaderTemplates(definition.Headers); err != nil {
		return fmt.Errorf("headers.%w", err)
	}
	// 4. 检查每个 Executor 是 local 或已配置的非 virtual Executor，内置 Executor 不能用。
	for _, executorID := range definition.Executors {
		if executorID == defs.LocalExecutorID {
			continue
		}
		if executorID == BuiltinExecutorID {
			return fmt.Errorf("mcp server cannot run on executor %q", executorID)
		}
		executor, found := executors[executorID]
		if !found {
			return fmt.Errorf("mcp server executor %q is not configured", executorID)
		}
		if executor.Kind == defs.ExecutorKindVirtual {
			return fmt.Errorf("mcp server cannot run on executor %q", executorID)
		}
	}
	// 5. 检查启动与工具调用超时均不为负数。
	if definition.StartupTimeoutSeconds < 0 || definition.ToolTimeoutSeconds < 0 {
		return errors.New("mcp server startup_timeout_seconds and tool_timeout_seconds must not be negative")
	}
	return nil
}

// hookEvents 是支持的 hook 事件，也是同一文件里各事件 hook 的排列顺序，和 yaml 里事件的书写顺序无关。
var hookEvents = []defs.HookEventName{
	defs.HookEventSessionStart,
	defs.HookEventUserPromptSubmit,
	defs.HookEventPreToolUse,
	defs.HookEventPostToolUse,
	defs.HookEventStop,
	defs.HookEventSubagentStart,
	defs.HookEventSubagentStop,
}

// defaultHookTimeout 是 hook 省略 timeout 时最多运行的时间。
const defaultHookTimeout = 60 * time.Second

// loadHookFile 读取一个 hook 文件，转成运行定义。
func loadHookFile(path, home string) ([]*hook.HookDefinition, error) {
	// 1. 读取 YAML，用文件名做 Key 前缀、文件所在目录做脚本路径的基准，加载各组 hook。
	file := &HookFileConfig{}
	if err := readYAML(path, file); err != nil {
		return nil, err
	}
	return loadHookGroups(file.Hooks, home, filepath.Dir(path), strings.TrimSuffix(filepath.Base(path), ".yaml"), path)
}

// loadHookGroups 校验各事件的 matcher 组，按固定事件顺序生成 hook 定义。
func loadHookGroups(groups map[string][]HookMatcherConfig, home, scriptDirectory, keyPrefix, sourcePath string) ([]*hook.HookDefinition, error) {
	// 1. 检查事件名都是支持的事件。
	for _, name := range slices.Sorted(maps.Keys(groups)) {
		if !slices.Contains(hookEvents, defs.HookEventName(name)) {
			return nil, fmt.Errorf("hooks.%s: unknown hook event %q", name, name)
		}
	}
	// 2. 按固定事件、matcher 组、hook 的顺序生成定义，错误带事件、组和条目位置；Key 是 <keyPrefix>:<事件>:<序号>，序号在同一事件里跨组连续。
	var definitions []*hook.HookDefinition
	for _, event := range hookEvents {
		eventHookCount := 0
		for groupIndex, group := range groups[string(event)] {
			matcher, err := hook.NewHookMatcher(group.Matcher)
			if err != nil {
				return nil, fmt.Errorf("hooks.%s[%d]: invalid matcher %q: %w", event, groupIndex, group.Matcher, err)
			}
			for handlerIndex, handler := range group.Hooks {
				definition, err := newHookDefinition(event, &handler, home, scriptDirectory)
				if err != nil {
					return nil, fmt.Errorf("hooks.%s[%d].hooks[%d]: %w", event, groupIndex, handlerIndex, err)
				}
				definition.Key = fmt.Sprintf("%s:%s:%d", keyPrefix, event, eventHookCount)
				definition.Matcher = matcher
				definition.SourcePath = sourcePath
				definitions = append(definitions, definition)
				eventHookCount++
			}
		}
	}
	return definitions, nil
}

// newHookDefinition 校验一个 hook，生成运行定义。
func newHookDefinition(event defs.HookEventName, handler *HookHandlerConfig, home, directory string) (*hook.HookDefinition, error) {
	// 1. 检查 type 是 command，command 和 script 正好写了一个，args 只和 script 一起用。
	if handler.Type != "command" {
		return nil, fmt.Errorf("unsupported hook type %q", handler.Type)
	}
	hasCommand, hasScript := strings.TrimSpace(handler.Command) != "", strings.TrimSpace(handler.Script) != ""
	if hasCommand == hasScript {
		return nil, errors.New("exactly one of command and script is required")
	}
	if len(handler.Args) > 0 && !hasScript {
		return nil, errors.New("args requires script")
	}
	// 2. 填入事件、命令、参数和默认 60 秒超时；script 相对 directory（hook 文件所在目录或插件根目录）展开，文件必须存在。
	definition := &hook.HookDefinition{
		Event:   event,
		Command: handler.Command,
		Args:    handler.Args,
		Timeout: defaultHookTimeout,
	}
	if hasScript {
		definition.ScriptPath = expandPath(handler.Script, home, directory)
		if _, err := os.Stat(definition.ScriptPath); err != nil {
			// 去掉 PathError 的 stat 前缀和路径，只写脚本路径和原始原因。
			return nil, fmt.Errorf("script %s: %w", definition.ScriptPath, errors.Unwrap(err))
		}
	}
	// 3. 写了 timeout 时必须为正；on_failure 只能是 allow 或 block，且只用于 PreToolUse 和 UserPromptSubmit。
	if handler.Timeout != nil {
		if *handler.Timeout <= 0 {
			return nil, errors.New("timeout must be positive")
		}
		definition.Timeout = time.Duration(*handler.Timeout) * time.Second
	}
	switch handler.OnFailure {
	case "":
	case "allow", "block":
		if event != defs.HookEventPreToolUse && event != defs.HookEventUserPromptSubmit {
			return nil, errors.New("on_failure is only supported for PreToolUse and UserPromptSubmit")
		}
		definition.IsBlockedOnFailure = handler.OnFailure == "block"
	default:
		return nil, errors.New("on_failure must be allow or block")
	}
	return definition, nil
}
