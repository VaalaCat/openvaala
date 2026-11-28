package config

import (
	"bufio"
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/world"
	"github.com/VaalaCat/openvaala/pkg/worldserver"
	"gopkg.in/yaml.v3"
)

// ConfigFiles 读写启动时加载的那份配置的文件，记着 LoadConfigOpt，重新加载时用同一个配置路径和 flag。
// 写方法写完都重新加载整份配置：加载失败时把文件恢复成写之前的样子（新建的删掉）并返回加载错误；
// 成功时返回新的整份定义（见 GetRuntimeDefinitions）。没有加锁，两处同时写时后写的可能覆盖先写的。
type ConfigFiles struct {
	// DataDirectory 保存运行中服务的数据目录，重载时继续使用。
	DataDirectory string
	// ExtraTools 在配置重载和写后重建时继续使用启动时装配的工具。
	ExtraTools executor.ToolProvider
	opt        LoadConfigOpt
}

// NewConfigFiles 创建 ConfigFiles，保存一份配置加载参数。
func NewConfigFiles(opt *LoadConfigOpt) *ConfigFiles {
	// 1. 复制参数，之后的读写都用同一个路径和 flag。
	return &ConfigFiles{opt: *opt}
}

// GetAgent 按名读取可编辑的 Agent 定义；default 没有文件时返回内置定义。
func (f *ConfigFiles) GetAgent(_ context.Context, name string) (*defs.AgentConfig, error) {
	if !isValidID(name) {
		return nil, fmt.Errorf("agent %q is not configured", name)
	}
	cfg, _, err := LoadConfigSettings(&f.opt)
	if err != nil {
		return nil, err
	}
	definition, err := loadAgentDefinition(filepath.Join(cfg.Agents.Directory, name+".yaml"))
	if errors.Is(err, os.ErrNotExist) {
		if name == defaultAgentName {
			return newBuiltinDefaultAgent(), nil
		}
		return nil, fmt.Errorf("agent %q is not configured", name)
	}
	return definition, err
}

// SetAgent 原子写入整份 Agent 配置并重载。
func (f *ConfigFiles) SetAgent(ctx context.Context, definition defs.AgentConfig) (*runtime.Definitions, error) {
	if !isValidID(definition.Name) {
		return nil, errors.New("invalid agent name")
	}
	cfg, err := LoadConfig(&f.opt)
	if err != nil {
		return nil, err
	}
	content, err := encodeYAML(&definition)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(cfg.Agents.Directory, definition.Name+".yaml")
	original, err := readOriginalFile(path)
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(path, content); err != nil {
		return nil, err
	}
	return f.reloadOrRestore(ctx, original)
}

// DeleteAgent 删除 Agent 文件并重载。
func (f *ConfigFiles) DeleteAgent(ctx context.Context, name string) (*runtime.Definitions, error) {
	if name == defaultAgentName {
		return nil, errors.New("agent default cannot be deleted")
	}
	if !isValidID(name) {
		return nil, errors.New("invalid agent name")
	}
	cfg, err := LoadConfig(&f.opt)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(cfg.Agents.Directory, name+".yaml")
	original, err := readOriginalFile(path)
	if err != nil {
		return nil, err
	}
	if !original.existed {
		return nil, fmt.Errorf("agent %q is not configured", name)
	}
	if err := os.Remove(path); err != nil {
		return nil, &Error{Path: path, Reason: "cannot remove file"}
	}
	return f.reloadOrRestore(ctx, original)
}

// ListProviders 按 ID 顺序返回各 Provider 文件里原样的配置、当前默认模型和模型默认配置。
func (f *ConfigFiles) ListProviders(_ context.Context) (providers []defs.ProviderConfig, defaultModel defs.ModelRef, defaults defs.ModelDefaults, err error) {
	// 1. 加载配置并取默认模型。
	cfg, err := LoadConfig(&f.opt)
	if err != nil {
		return nil, defs.ModelRef{}, defs.ModelDefaults{}, err
	}
	modelConfig, err := cfg.getDefaultModel()
	if err != nil {
		return nil, defs.ModelRef{}, defs.ModelDefaults{}, err
	}
	// 2. 读取全部 Provider 文件，api_key 原样保留，按 ID 排序；没有任何模型时默认模型是零值。
	paths, err := getDefinitionFiles(cfg.Models.Directory)
	if err != nil {
		return nil, defs.ModelRef{}, defs.ModelDefaults{}, err
	}
	providers = make([]defs.ProviderConfig, 0, len(paths))
	for _, path := range paths {
		var provider defs.ProviderConfig
		if err := readYAML(path, &provider); err != nil {
			return nil, defs.ModelRef{}, defs.ModelDefaults{}, err
		}
		providers = append(providers, provider)
	}
	slices.SortFunc(providers, func(a, b defs.ProviderConfig) int { return strings.Compare(a.ID, b.ID) })
	// 3. 返回同次加载的默认配置，未填写输入能力时使用 text。
	defaults = *cfg.Models.Defaults
	if len(defaults.Capabilities.Input) == 0 {
		defaults.Capabilities.Input = defs.OptionalStringList{string(defs.ModelInputText)}
	}
	return providers, getModelRef(modelConfig), defaults, nil
}

// SetProvider 写入整份 Provider；defaultModel 非零时一起改默认模型。
func (f *ConfigFiles) SetProvider(ctx context.Context, provider defs.ProviderConfig, defaultModel defs.ModelRef) (*runtime.Definitions, error) {
	// 1. 校验 Provider ID，加载配置，把 Provider 编码成 YAML。
	if !isValidID(provider.ID) {
		return nil, errors.New("invalid provider id")
	}
	cfg, err := LoadConfig(&f.opt)
	if err != nil {
		return nil, err
	}
	content, err := encodeYAML(&provider)
	if err != nil {
		return nil, err
	}
	// 2. 记下 Provider 文件原来的内容；defaultModel 非零且和现在不同时，准备好配置文件的改动。
	path := filepath.Join(cfg.Models.Directory, provider.ID+".yaml")
	original, err := readOriginalFile(path)
	if err != nil {
		return nil, err
	}
	var configOriginal *originalFile
	var configContent []byte
	if defaultModel != (defs.ModelRef{}) {
		configOriginal, configContent, err = f.getDefaultModelEdit(defaultModel)
		if err != nil {
			return nil, err
		}
	}
	// 3. 先写 Provider，需要时再写配置文件，只重新加载一次；第二个文件写失败或加载失败时恢复已经改过的文件。
	if err := writeFileAtomic(path, content); err != nil {
		return nil, err
	}
	if configOriginal == nil {
		return f.reloadOrRestore(ctx, original)
	}
	if err := writeFileAtomic(configOriginal.path, configContent); err != nil {
		return nil, errors.Join(err, original.restore())
	}
	return f.reloadOrRestore(ctx, original, configOriginal)
}

// DeleteProvider 删除 Provider 文件；默认模型属于这个 Provider 时一起去掉 default_model。
func (f *ConfigFiles) DeleteProvider(ctx context.Context, providerID string) (*runtime.Definitions, error) {
	// 1. 校验 ID，加载配置，记下 Provider 文件原来的内容，文件不存在时报错。
	if !isValidID(providerID) {
		return nil, errors.New("invalid provider id")
	}
	cfg, err := LoadConfig(&f.opt)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(cfg.Models.Directory, providerID+".yaml")
	original, err := readOriginalFile(path)
	if err != nil {
		return nil, err
	}
	if !original.existed {
		return nil, fmt.Errorf("provider %s is not configured", providerID)
	}
	// 2. 默认模型属于这个 Provider 时准备去掉 default_model，其他配置和注释保留。
	configOriginal, configContent, err := f.getDefaultModelRemoval(providerID)
	if err != nil {
		return nil, err
	}
	// 3. 删除文件（符号链接只删链接本身），需要时写配置文件；之后失败时恢复改过的文件。
	if err := os.Remove(path); err != nil {
		return nil, &Error{
			Path:   path,
			Reason: "cannot remove file",
		}
	}
	if configOriginal == nil {
		return f.reloadOrRestore(ctx, original)
	}
	if err := writeFileAtomic(configOriginal.path, configContent); err != nil {
		return nil, errors.Join(err, original.restore())
	}
	return f.reloadOrRestore(ctx, original, configOriginal)
}

// SetDefaultModel 修改配置文件里的 models.default_model 并重新加载。
func (f *ConfigFiles) SetDefaultModel(ctx context.Context, ref defs.ModelRef) (*runtime.Definitions, error) {
	// 1. 准备 default_model 一项的改动，和原值相同时只重新加载。
	original, content, err := f.getDefaultModelEdit(ref)
	if err != nil {
		return nil, err
	}
	if original == nil {
		return f.Reload(ctx)
	}
	// 2. 写入后重新加载，加载失败时恢复原文件（原来没有就删掉）。
	if err := writeFileAtomic(original.path, content); err != nil {
		return nil, err
	}
	return f.reloadOrRestore(ctx, original)
}

// isBuiltinExecutorID 检查 ID 是否为保留的内置 Executor。
func isBuiltinExecutorID(id string) bool {
	return id == defs.LocalExecutorID || id == BuiltinExecutorID || id == defs.MemoryExecutorID
}

// ListExecutorConfigs 按 ID 顺序返回 executors/*.yaml 里的原样配置；目录不存在时返回空列表。
func (f *ConfigFiles) ListExecutorConfigs(_ context.Context) (executors []defs.ExecutorConfig, err error) {
	// 1. 加载当前配置取得 executors 目录。
	cfg, err := LoadConfig(&f.opt)
	if err != nil {
		return nil, err
	}
	paths, err := getDefinitionFiles(cfg.Executors.Directory)
	if err != nil {
		return nil, err
	}
	// 2. 逐个读取 YAML 原样返回（不展开私钥路径里的 ~），按 ID 排序。
	executors = make([]defs.ExecutorConfig, 0, len(paths))
	for _, path := range paths {
		var item defs.ExecutorConfig
		if err := readYAML(path, &item); err != nil {
			return nil, err
		}
		executors = append(executors, item)
	}
	slices.SortFunc(executors, func(a, b defs.ExecutorConfig) int { return strings.Compare(a.ID, b.ID) })
	return executors, nil
}

// SetExecutor 原子写入 executors/<id>.yaml 并重新加载配置；失败时恢复原文件。
func (f *ConfigFiles) SetExecutor(ctx context.Context, executorCfg defs.ExecutorConfig) (*runtime.Definitions, error) {
	// 1. 校验 Executor ID 非法或保留，提前拒绝不碰文件。
	executorCfg.ID = strings.TrimSpace(executorCfg.ID)
	if !isValidID(executorCfg.ID) {
		return nil, errors.New("invalid executor id")
	}
	if isBuiltinExecutorID(executorCfg.ID) {
		return nil, fmt.Errorf("builtin executor %s cannot be modified", executorCfg.ID)
	}
	if executorCfg.Kind == defs.ExecutorKindSSH {
		if executorCfg.SSH == nil || strings.TrimSpace(executorCfg.SSH.Address) == "" {
			return nil, errors.New("ssh address is required")
		}
		executorCfg.SSH = &defs.SSHExecutorConfig{
			Address:      strings.TrimSpace(executorCfg.SSH.Address),
			IdentityFile: strings.TrimSpace(executorCfg.SSH.IdentityFile),
		}
	}
	executorCfg.Description = strings.TrimSpace(executorCfg.Description)
	executorCfg.WorkspaceDirectory = strings.TrimSpace(executorCfg.WorkspaceDirectory)
	// 2. 加载配置、编码 YAML、记录原文件快照后原子写入并热重载，失败自动回滚。
	cfg, err := LoadConfig(&f.opt)
	if err != nil {
		return nil, err
	}
	content, err := encodeYAML(&executorCfg)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(cfg.Executors.Directory, executorCfg.ID+".yaml")
	original, err := readOriginalFile(path)
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(path, content); err != nil {
		return nil, err
	}
	return f.reloadOrRestore(ctx, original)
}

// DeleteExecutor 删除 executors/<id>.yaml 并重新加载配置；文件不存在时返回错误。
func (f *ConfigFiles) DeleteExecutor(ctx context.Context, executorID string) (*runtime.Definitions, error) {
	// 1. 校验 ID 且拒绝内置 Executor。
	executorID = strings.TrimSpace(executorID)
	if !isValidID(executorID) {
		return nil, errors.New("invalid executor id")
	}
	if isBuiltinExecutorID(executorID) {
		return nil, fmt.Errorf("builtin executor %s cannot be modified", executorID)
	}
	cfg, err := LoadConfig(&f.opt)
	if err != nil {
		return nil, err
	}
	// 2. 读取快照检查存在性，删除后重新加载，失败自动恢复。
	path := filepath.Join(cfg.Executors.Directory, executorID+".yaml")
	original, err := readOriginalFile(path)
	if err != nil {
		return nil, err
	}
	if !original.existed {
		return nil, fmt.Errorf("executor %s is not configured", executorID)
	}
	if err := os.Remove(path); err != nil {
		return nil, &Error{
			Path:   path,
			Reason: "cannot remove file",
		}
	}
	return f.reloadOrRestore(ctx, original)
}

// TestSSHExecutor 用临时 SSH 参数拨号并解析工作目录，返回规范后的绝对目录，不写入任何文件。
func (f *ConfigFiles) TestSSHExecutor(ctx context.Context, sshCfg defs.SSHExecutorConfig, workspaceDirectory string) (resolvedDirectory string, err error) {
	// 1. 校验地址、远端绝对目录与可选私钥路径。
	address := strings.TrimSpace(sshCfg.Address)
	if address == "" {
		return "", errors.New("ssh address is required")
	}
	workspaceDirectory = strings.TrimSpace(workspaceDirectory)
	if workspaceDirectory == "" || !filepath.IsAbs(workspaceDirectory) {
		return "", errors.New("ssh executor workspace_directory must be an absolute remote path")
	}
	home := f.opt.Home
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	identityFile := strings.TrimSpace(sshCfg.IdentityFile)
	if identityFile != "" {
		executorsDir := ""
		if cfg, loadErr := LoadConfig(&f.opt); loadErr == nil {
			executorsDir = cfg.Executors.Directory
		}
		identityFile = expandPath(identityFile, home, executorsDir)
		if info, statErr := os.Stat(identityFile); statErr != nil || !info.Mode().IsRegular() {
			return "", fmt.Errorf("ssh identity_file %s must be an existing file", identityFile)
		}
	}
	// 2. 创建临时 SSHExecutor 校验登录与目录，ResolveWorkspaceDirectory 内部负责关闭工作区。
	const testExecutorID = "ssh-test"
	exec := executor.NewSSHExecutor(executor.NewSSHExecutorOpt(&defs.ExecutorDefinition{
		ID:                 testExecutorID,
		Kind:               defs.ExecutorKindSSH,
		WorkspaceDirectory: workspaceDirectory,
	}, address, identityFile))
	return conversation.ResolveWorkspaceDirectory(ctx, []executor.Executor{exec}, testExecutorID, workspaceDirectory, workspaceDirectory)
}

// ListSSHConfigHosts 读取 serve 本机 ~/.ssh/config 里的非通配符 Host 项；文件缺失或不可读时记警告并返回空列表。
func (f *ConfigFiles) ListSSHConfigHosts(_ context.Context) (hosts []defs.SSHConfigHost, err error) {
	home := f.opt.Home
	if home == "" {
		home, err = os.UserHomeDir()
		if err != nil {
			slog.Warn("cannot locate user home for ssh config", "error", err)
			return []defs.SSHConfigHost{}, nil
		}
	}
	path := filepath.Join(home, ".ssh", "config")
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("cannot read ssh config for host suggestions", "path", path, "error", err)
		}
		return []defs.SSHConfigHost{}, nil
	}
	return parseSSHConfigHosts(string(data)), nil
}

// parseSSHConfigHosts 按 OpenSSH 首项优先规则解析 ~/.ssh/config，跳过包含通配符或取反的 Host 模式。
func parseSSHConfigHosts(content string) []defs.SSHConfigHost {
	type hostEntry struct {
		alias        string
		hostName     string
		user         string
		port         string
		identityFile string
	}
	var (
		order   []string
		entries = map[string]*hostEntry{}
		active  []*hostEntry
	)
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// 1. 拆分关键字与参数（支持空白或等号分隔）。
		key, rest := splitSSHConfigDirective(line)
		if key == "" || rest == "" {
			continue
		}
		switch strings.ToLower(key) {
		case "match":
			active = nil
		case "host":
			active = nil
			for _, pattern := range strings.Fields(rest) {
				alias := strings.Trim(pattern, `"`)
				if alias == "" || strings.ContainsAny(alias, "*?!") {
					continue
				}
				entry, exists := entries[alias]
				if !exists {
					entry = &hostEntry{alias: alias}
					entries[alias] = entry
					order = append(order, alias)
				}
				active = append(active, entry)
			}
		case "hostname":
			value := unquoteSSHConfigValue(rest)
			for _, entry := range active {
				if entry.hostName == "" {
					entry.hostName = value
				}
			}
		case "user":
			value := unquoteSSHConfigValue(rest)
			for _, entry := range active {
				if entry.user == "" {
					entry.user = value
				}
			}
		case "port":
			value := unquoteSSHConfigValue(rest)
			for _, entry := range active {
				if entry.port == "" {
					entry.port = value
				}
			}
		case "identityfile":
			value := unquoteSSHConfigValue(rest)
			for _, entry := range active {
				if entry.identityFile == "" {
					entry.identityFile = value
				}
			}
		}
	}
	// 2. 按首次出现顺序拼出 [user@]host[:port]。
	result := make([]defs.SSHConfigHost, 0, len(order))
	for _, alias := range order {
		entry := entries[alias]
		host := entry.hostName
		if host == "" {
			host = entry.alias
		}
		if entry.port != "" && entry.port != "22" {
			host = net.JoinHostPort(host, entry.port)
		}
		address := host
		if entry.user != "" {
			address = entry.user + "@" + host
		}
		result = append(result, defs.SSHConfigHost{
			Alias:        entry.alias,
			Address:      address,
			IdentityFile: entry.identityFile,
		})
	}
	return result
}

// splitSSHConfigDirective 把一行 ~/.ssh/config 拆成指令名和参数部分。
func splitSSHConfigDirective(line string) (key, value string) {
	sep := strings.IndexAny(line, " \t=")
	if sep < 0 {
		return "", ""
	}
	key = strings.TrimSpace(line[:sep])
	rest := strings.TrimSpace(line[sep:])
	rest = strings.TrimLeft(rest, "=")
	return key, strings.TrimSpace(rest)
}

// unquoteSSHConfigValue 去掉行尾注释（引号外）和包裹双引号。
func unquoteSSHConfigValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && value[0] == '"' {
		if end := strings.IndexByte(value[1:], '"'); end >= 0 {
			return value[1 : 1+end]
		}
	}
	if comment := strings.IndexByte(value, '#'); comment >= 0 {
		value = strings.TrimSpace(value[:comment])
	}
	return strings.Trim(value, `"`)
}

// getDefaultModelEdit 读取配置文件，返回原文件和改了 default_model 之后的内容；默认模型没变时都返回 nil。
func (f *ConfigFiles) getDefaultModelEdit(ref defs.ModelRef) (*originalFile, []byte, error) {
	// 1. 找到配置文件并记下原内容，默认模型没变时直接返回。
	path, err := GetConfigPath(f.opt.Home, f.opt.Path)
	if err != nil {
		return nil, nil, err
	}
	original, err := readOriginalFile(path)
	if err != nil {
		return nil, nil, err
	}
	if getContentDefaultModel(original.content) == ref.String() {
		return nil, nil, nil
	}
	// 2. 只改 models.default_model，文件不存在时新建这一项；YAML 解析失败时返回带路径的错误。
	content, err := setDefaultModelContent(original.content, ref)
	if err != nil {
		return nil, nil, &Error{
			Path:   path,
			Reason: "invalid YAML",
		}
	}
	return original, content, nil
}

// GetWorldSettings 读取实际保存的选项；未入网时建议使用本机主机名。
func (f *ConfigFiles) GetWorldSettings() (WorldSettings, error) {
	cfg, _, err := LoadConfigSettings(&f.opt)
	if err != nil {
		return WorldSettings{}, err
	}
	node, server := cfg.World.Node, cfg.World.Server
	name, err := world.GetJoinNodeName(node.Name)
	if err != nil {
		return WorldSettings{}, err
	}
	_, err = os.Stat(filepath.Join(cmp.Or(f.DataDirectory, cfg.GetDataDirectory()), "worldserver", "world.db"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return WorldSettings{}, err
	}
	return WorldSettings{
		RemoteAccess: cfg.World.RemoteAccess,
		Relays:       append([]string{}, cfg.World.Relays...), RelayMode: cfg.World.RelayMode, WebURL: cfg.World.WebURL,
		Node: WorldNodeSettings{
			Enabled: node.Enabled, Name: name, Domain: node.Domain, ServerURL: node.ServerURL,
			DirectURLs:       append([]string{}, node.DirectURLs...),
			DefaultTarget:    node.DefaultTarget,
			AdvertiseTargets: append([]worldserver.AdvertisedTarget{}, node.AdvertiseTargets...),
		},
		Server: WorldServerSettings{Enabled: server.Enabled, Domain: server.Domain, IsDatabaseCreated: err == nil},
	}, nil
}

// PrepareWorldSettings 校验待保存的字段，入网时由控制面在注册后确定节点身份。
func (f *ConfigFiles) PrepareWorldSettings(settings WorldSettings, joining bool) (*WorldSettingsUpdate, error) {
	cfg, err := LoadConfig(&f.opt)
	if err != nil {
		return nil, err
	}
	top := map[string]any{}
	if settings.RemoteAccess != cfg.World.RemoteAccess {
		top["remote_access"] = settings.RemoteAccess
	}
	if settings.RelayMode != cfg.World.RelayMode {
		top["relay_mode"] = settings.RelayMode
	}
	if settings.WebURL != cfg.World.WebURL {
		top["web_url"] = settings.WebURL
	}
	if !slices.Equal(settings.Relays, cfg.World.Relays) {
		top["relays"] = settings.Relays
	}
	cfg.World.Relays, cfg.World.RelayMode, cfg.World.WebURL = settings.Relays, settings.RelayMode, settings.WebURL
	n := &cfg.World.Node
	fields := map[string]any{"default_target": settings.Node.DefaultTarget}
	// 未编辑的列表保留原 YAML 节点及各行注释。
	if !slices.Equal(n.DirectURLs, settings.Node.DirectURLs) {
		fields["direct_urls"] = settings.Node.DirectURLs
	}
	if !slices.Equal(n.AdvertiseTargets, settings.Node.AdvertiseTargets) {
		fields["advertise_targets"] = settings.Node.AdvertiseTargets
	}
	n.Enabled = settings.Node.Enabled && !joining
	n.DirectURLs = settings.Node.DirectURLs
	n.DefaultTarget = settings.Node.DefaultTarget
	n.AdvertiseTargets = settings.Node.AdvertiseTargets
	cfg.World.Server.Enabled, cfg.World.Server.Domain = settings.Server.Enabled, settings.Server.Domain
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.World.Server.Domain != "" && !worldserver.IsValidDomain(cfg.World.Server.Domain) {
		return nil, errors.New("world server requires a valid domain and a non-empty admin token")
	}
	targets := cfg.WorldNodeWithDefaults()
	for _, target := range targets.AdvertiseTargets {
		if cfg.agentDefinitions[target.Name] == nil {
			return nil, fmt.Errorf("unknown agent %s in world settings", target.Name)
		}
	}
	if targets.DefaultTarget != "" && cfg.agentDefinitions[targets.DefaultTarget] == nil {
		return nil, fmt.Errorf("unknown agent %s in world settings", targets.DefaultTarget)
	}
	if err := world.ValidateAdvertisedTargets(targets.DefaultTarget, targets.AdvertiseTargets); err != nil {
		return nil, err
	}
	return &WorldSettingsUpdate{NodeEnabled: settings.Node.Enabled, fields: top, node: fields, server: settings.Server}, nil
}

// SaveWorldSettings 将已校验的设置与控制面返回的注册信息一次写回。
func (f *ConfigFiles) SaveWorldSettings(update *WorldSettingsUpdate, join *worldserver.RegisterNodeResponse, serverURL string) error {
	update.node["enabled"] = update.NodeEnabled
	if join != nil {
		update.node["name"], update.node["domain"], update.node["server_url"] = join.NodeName, join.Domain, serverURL
	}
	if err := f.writeWorldSettings(update.fields, map[string]map[string]any{"node": update.node, "server": {"enabled": update.server.Enabled, "domain": update.server.Domain}}); err != nil {
		return fmt.Errorf("save world settings: %w", err)
	}
	return nil
}

// SetWorldNodeRegistration 写入注册结果，保留其他配置及节点开放目标。
func (f *ConfigFiles) SetWorldNodeRegistration(name, domain, serverURL string) error {
	// 1. 启用节点并记录远端已确认的名称、域名和控制面地址。
	return f.writeWorldSettings(nil, map[string]map[string]any{"node": {"enabled": true, "name": name, "domain": domain, "server_url": serverURL}})
}

// SetWorldAdminToken 将首次生成的控制面管理凭据保存到私有配置文件。
func (f *ConfigFiles) SetWorldAdminToken(token string) error {
	// 1. 只更新控制面管理凭据。
	return f.writeWorldSettings(nil, map[string]map[string]any{"server": {"admin_token": token}})
}

func (f *ConfigFiles) writeWorldSettings(top map[string]any, sections map[string]map[string]any) error {
	// 1. 读取原配置，在缺少配置时创建空文档。
	path, err := GetConfigPath(f.opt.Home, f.opt.Path)
	if err != nil {
		return err
	}
	original, err := readOriginalFile(path)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(original.content, &doc); err != nil {
		return err
	}
	if len(doc.Content) == 0 {
		doc.Kind = yaml.DocumentNode
		doc.Content = []*yaml.Node{{HeadComment: string(original.content)}}
	}
	// 2. 仅更新本次操作涉及的字段，原子写回权限为 0600 的文件。
	root := doc.Content[0]
	toBlockMapping(root)
	world := getOrAddMappingEntry(root, "world")
	toBlockMapping(world)
	writeFields := func(group *yaml.Node, values map[string]any) error {
		for _, key := range slices.Sorted(maps.Keys(values)) {
			entry := getOrAddMappingEntry(group, key)
			head, line, foot := entry.HeadComment, entry.LineComment, entry.FootComment
			if err := entry.Encode(values[key]); err != nil {
				return err
			}
			entry.HeadComment, entry.LineComment, entry.FootComment = head, line, foot
		}
		return nil
	}
	if err := writeFields(world, top); err != nil {
		return err
	}
	for _, section := range slices.Sorted(maps.Keys(sections)) {
		group := getOrAddMappingEntry(world, section)
		toBlockMapping(group)
		if err := writeFields(group, sections[section]); err != nil {
			return err
		}
	}
	content, err := encodeYAML(&doc)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, content)
}

// getDefaultModelRemoval 默认模型属于 providerID 时，返回原配置文件和去掉 default_model 之后的内容；否则都返回 nil。
func (f *ConfigFiles) getDefaultModelRemoval(providerID string) (*originalFile, []byte, error) {
	// 1. 找到配置文件并记下原内容，默认模型解析不了或不属于这个 Provider 时直接返回。
	path, err := GetConfigPath(f.opt.Home, f.opt.Path)
	if err != nil {
		return nil, nil, err
	}
	original, err := readOriginalFile(path)
	if err != nil {
		return nil, nil, err
	}
	ref, err := defs.ParseModelRef(getContentDefaultModel(original.content))
	if err != nil || ref.ProviderID != providerID {
		return nil, nil, nil
	}
	// 2. 去掉 models.default_model，保留其他内容和注释。
	content, err := removeDefaultModelContent(original.content)
	if err != nil {
		return nil, nil, &Error{
			Path:   path,
			Reason: "invalid YAML",
		}
	}
	return original, content, nil
}

// Reload 重新加载配置，返回整份运行定义。
func (f *ConfigFiles) Reload(ctx context.Context) (*runtime.Definitions, error) {
	// 1. 用保存的路径和 flag 加载配置，再组装运行定义。
	cfg, err := LoadConfig(&f.opt)
	if err != nil {
		return nil, err
	}
	if f.DataDirectory != "" {
		cfg.DataDirectory = f.DataDirectory
	}
	definitions, err := cfg.GetRuntimeDefinitions(ctx)
	if err != nil {
		return nil, err
	}
	definitions.ExtraTools = f.ExtraTools
	return definitions, nil
}

// reloadOrRestore 重新加载改过的配置；加载失败时把 originals 逐个恢复。
func (f *ConfigFiles) reloadOrRestore(ctx context.Context, originals ...*originalFile) (*runtime.Definitions, error) {
	// 1. 重新加载整份定义，成功时直接返回，失败时逐个恢复原文件并合并错误。
	definitions, err := f.Reload(ctx)
	if err != nil {
		for _, original := range originals {
			if restoreErr := original.restore(); restoreErr != nil {
				err = errors.Join(err, restoreErr)
			}
		}
		return nil, err
	}
	return definitions, nil
}

// originalFile 记住一个配置文件写之前的内容，加载失败时据此恢复。
type originalFile struct {
	path string
	// existed 写之前文件是否存在？不存在时恢复就是删除。
	existed bool
	content []byte
	// linkTarget 是 path 本身为符号链接时它指向的位置；删除链接后据此把链接建回去。
	linkTarget string
}

// readOriginalFile 记下文件写之前的内容和符号链接目标。
func readOriginalFile(path string) (*originalFile, error) {
	// 1. 读取文件，不存在时只记路径，其他读取失败时返回配置错误。
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &originalFile{path: path}, nil
	}
	if err != nil {
		return nil, &Error{
			Path:   path,
			Reason: "cannot read file",
		}
	}
	// 2. 记下原内容；path 是符号链接时记下它指向的位置，不是链接时 Readlink 报错，linkTarget 留空。
	original := &originalFile{
		path:    path,
		existed: true,
		content: content,
	}
	original.linkTarget, _ = os.Readlink(path)
	return original, nil
}

// restore 把文件恢复成写之前的内容或链接。
func (o *originalFile) restore() error {
	// 1. 原来没有这个文件时删掉新建的，已经没有了也算成功。
	if !o.existed {
		if err := os.Remove(o.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return &Error{
				Path:   o.path,
				Reason: "cannot remove file",
			}
		}
		return nil
	}
	// 2. 原来的符号链接被删掉时（链接指向的文件没动过）把链接建回去，其他情况原子写回旧内容。
	if _, err := os.Lstat(o.path); errors.Is(err, os.ErrNotExist) && o.linkTarget != "" {
		if err := os.Symlink(o.linkTarget, o.path); err != nil {
			return &Error{
				Path:   o.path,
				Reason: "cannot restore symbolic link",
			}
		}
		return nil
	}
	return writeFileAtomic(o.path, o.content)
}

// getContentDefaultModel 从配置文件内容里读出 models.default_model。
func getContentDefaultModel(content []byte) string {
	// 1. 没写或解析失败时返回空串。
	var current struct {
		Models struct {
			DefaultModel string `yaml:"default_model"`
		} `yaml:"models"`
	}
	if yaml.Unmarshal(content, &current) != nil {
		return ""
	}
	return current.Models.DefaultModel
}

// setDefaultModelContent 在配置文件内容里写入 models.default_model，保留其他配置和注释。
func setDefaultModelContent(content []byte, ref defs.ModelRef) ([]byte, error) {
	// 1. 解析 YAML，空文件或只有注释时新建空文档。
	var document yaml.Node
	if err := yaml.Unmarshal(content, &document); err != nil {
		return nil, err
	}
	hasNoNode := document.Kind == 0
	if hasNoNode {
		document = yaml.Node{
			Kind:    yaml.DocumentNode,
			Content: []*yaml.Node{{}},
		}
	}
	// 2. 需要时补上 models 和 default_model 节点，只改默认模型的值，再编码。
	root := document.Content[0]
	toBlockMapping(root)
	models := getOrAddMappingEntry(root, "models")
	toBlockMapping(models)
	defaultModel := getOrAddMappingEntry(models, "default_model")
	defaultModel.Kind, defaultModel.Tag, defaultModel.Value = yaml.ScalarNode, "!!str", ref.String()
	edited, err := encodeYAML(&document)
	if err != nil {
		return nil, err
	}
	// 3. 原文件没有节点时把新内容接在原注释后面，原内容末尾没换行就先补一个。
	if !hasNoNode {
		return edited, nil
	}
	if len(content) > 0 && content[len(content)-1] != '\n' {
		content = append(slices.Clone(content), '\n')
	}
	return slices.Concat(content, edited), nil
}

// removeDefaultModelContent 从配置文件内容里删掉 models.default_model。
func removeDefaultModelContent(content []byte) ([]byte, error) {
	// 1. 解析 YAML，删掉这一项后编码，其他内容和注释保留。
	var document yaml.Node
	if err := yaml.Unmarshal(content, &document); err != nil {
		return nil, err
	}
	models := getOrAddMappingEntry(document.Content[0], "models")
	deleteMappingEntry(models, "default_model")
	return encodeYAML(&document)
}

// setPluginDisabledContent 在配置文件内容里把插件名加入或移出 plugins.disabled，返回改后的内容和有没有变化。
func setPluginDisabledContent(content []byte, name string, isDisabled bool) ([]byte, bool, error) {
	// 1. 读出禁用列表，已经是目标状态时原样返回，标记没变化。
	var current struct {
		Plugins struct {
			Disabled []string `yaml:"disabled"`
		} `yaml:"plugins"`
	}
	if err := yaml.Unmarshal(content, &current); err != nil {
		return nil, false, err
	}
	if slices.Contains(current.Plugins.Disabled, name) == isDisabled {
		return content, false, nil
	}
	// 2. 解析 YAML 节点，空配置时新建文档，需要时补上 plugins.disabled。
	var document yaml.Node
	if err := yaml.Unmarshal(content, &document); err != nil {
		return nil, false, err
	}
	if document.Kind == 0 {
		document = yaml.Node{
			Kind:    yaml.DocumentNode,
			Content: []*yaml.Node{{}},
		}
	}
	root := document.Content[0]
	toBlockMapping(root)
	plugins := getOrAddMappingEntry(root, "plugins")
	toBlockMapping(plugins)
	disabled := getOrAddMappingEntry(plugins, "disabled")
	// 3. 禁用时追加名字，启用时删掉名字，再编码并标记有变化。
	if isDisabled {
		if disabled.Kind != yaml.SequenceNode {
			// disabled: 后面没写值时是空标量，改成块风格的空序列再追加。
			disabled.Kind, disabled.Tag, disabled.Value, disabled.Style = yaml.SequenceNode, "!!seq", "", 0
		}
		disabled.Content = append(disabled.Content, &yaml.Node{
			Kind:  yaml.ScalarNode,
			Tag:   "!!str",
			Value: name,
		})
	} else {
		disabled.Content = slices.DeleteFunc(disabled.Content, func(item *yaml.Node) bool { return item.Value == name })
	}
	edited, err := encodeYAML(&document)
	if err != nil {
		return nil, false, err
	}
	return edited, true, nil
}

// InitConfig 创建缺少的配置文件和 Agent、模型、插件目录，返回配置文件路径。
func InitConfig(opt *LoadConfigOpt) (string, error) {
	// 1. 检查参数并确定配置文件路径，文件不存在时创建，已有的不动。
	if opt == nil {
		return "", errors.New("configuration options are required")
	}
	path, err := GetConfigPath(opt.Home, opt.Path)
	if err != nil {
		return "", err
	}
	if err := createConfigFile(path); err != nil {
		return "", err
	}
	// 2. 用确定的路径加载配置。
	opt = &LoadConfigOpt{
		Home:  opt.Home,
		Path:  path,
		Flags: opt.Flags,
	}
	cfg, err := LoadConfig(opt)
	if err != nil {
		return "", err
	}
	// 3. 创建 Agent、模型和插件目录，权限为 0700。
	for _, directory := range []string{cfg.Agents.Directory, cfg.Models.Directory, cfg.Plugins.Directory} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			return "", &Error{
				Path:   directory,
				Reason: "cannot create directory",
			}
		}
	}
	return path, nil
}

// createConfigFile 配置文件不存在时写入一份空配置：先写临时文件，再用硬链接放到目标路径。
func createConfigFile(path string) error {
	// 1. 文件已存在时直接返回，否则创建权限为 0700 的配置目录。
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return &Error{
			Path:   path,
			Reason: "cannot inspect file",
		}
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return &Error{
			Path:   path,
			Reason: "cannot create directory",
		}
	}
	// 2. 创建临时文件，写入 {}，同步并关闭。
	temporary, err := os.CreateTemp(directory, ".config-*")
	if err != nil {
		return &Error{
			Path:   path,
			Reason: "cannot create file",
		}
	}
	defer os.Remove(temporary.Name())
	defer temporary.Close()
	if _, err = temporary.WriteString("{}\n"); err != nil {
		return &Error{
			Path:   path,
			Reason: "cannot write file",
		}
	}
	if err = temporary.Sync(); err != nil {
		return &Error{
			Path:   path,
			Reason: "cannot sync file",
		}
	}
	if err = temporary.Close(); err != nil {
		return &Error{
			Path:   path,
			Reason: "cannot close file",
		}
	}
	// 3. 用硬链接放到目标路径，硬链接不会覆盖已有文件，别人先建好了就用别人的；返回前删除临时文件。
	if err = os.Link(temporary.Name(), path); err != nil && !errors.Is(err, os.ErrExist) {
		return &Error{
			Path:   path,
			Reason: "cannot publish file",
		}
	}
	return nil
}

// writeFileAtomic 先写同目录的临时文件，再改名替换配置文件。
func writeFileAtomic(path string, content []byte) error {
	// 1. path 是符号链接时改写它指向的真实文件，链接本身保留。
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	// 2. 创建权限为 0700 的目标目录和 0600 的临时文件。
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return &Error{
			Path:   directory,
			Reason: "cannot create directory",
		}
	}
	temporary, err := os.CreateTemp(directory, ".write-*")
	if err != nil {
		return &Error{
			Path:   path,
			Reason: "cannot create file",
		}
	}
	defer os.Remove(temporary.Name())
	defer temporary.Close()
	// 3. 写入、同步并关闭临时文件。
	if _, err = temporary.Write(content); err != nil {
		return &Error{
			Path:   path,
			Reason: "cannot write file",
		}
	}
	if err = temporary.Sync(); err != nil {
		return &Error{
			Path:   path,
			Reason: "cannot sync file",
		}
	}
	if err = temporary.Close(); err != nil {
		return &Error{
			Path:   path,
			Reason: "cannot close file",
		}
	}
	// 4. 改名替换目标文件，返回前清理临时文件。
	if err = os.Rename(temporary.Name(), path); err != nil {
		return &Error{
			Path:   path,
			Reason: "cannot replace file",
		}
	}
	return nil
}

// ConfiguredTokenDigest 标记手工配置的令牌是否在重置后发生过更改。
func ConfiguredTokenDigest(token string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(token)))
}

// SaveRuntimeToken 原子更新本机连接文件，保留地址、服务标识和可执行文件信息。
func SaveRuntimeToken(path, configuredToken, token string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var fields map[string]any
	if err := yaml.Unmarshal(data, &fields); err != nil {
		return err
	}
	if fields == nil {
		return errors.New("empty runtime server file")
	}
	fields["token"] = token
	fields["reset_for"] = ConfiguredTokenDigest(configuredToken)
	data, err = yaml.Marshal(fields)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data)
}
