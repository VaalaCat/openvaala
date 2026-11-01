package config

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
	"github.com/VaalaCat/openvaala/pkg/workspace"
)

// pluginManifestPath 是 OpenVaala 原生 manifest 相对插件根目录的路径。
const pluginManifestPath = ".openvaala-plugin/plugin.yaml"

// errPluginManifestNotFound 表示目录里没有支持的插件描述文件。
var errPluginManifestNotFound = errors.New("not a plugin: missing supported plugin manifest")

// pluginInstallRecordName 是 plugin install 写在插件根目录下的安装记录文件名；手动放入的插件没有。
const pluginInstallRecordName = ".openvaala-install.yaml"

// pluginRootVariable 是插件 MCP server 的 command、args、env 里 ${OPENVAALA_PLUGIN_ROOT} 占位符的名字，
// 也是加给 server 的环境变量名，值是插件在 serve 本机的根目录。
const pluginRootVariable = "OPENVAALA_PLUGIN_ROOT"

// reservedPluginName 是插件不能使用的名字。
const reservedPluginName = "skill"

// pluginNamePattern 是插件名：小写字母、数字，以单个连字符分隔。
var pluginNamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// loadPlugins 按目录名顺序加载 plugins/ 里启用的插件。
func (cfg *Config) loadPlugins(home string) error {
	// 1. 清空已加载的插件，按目录名顺序读取插件目录；目录不存在时直接返回。
	cfg.plugins = nil
	entries, err := os.ReadDir(cfg.Plugins.Directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	// 2. 跳过隐藏的、禁用的、链接目标不存在的、不是目录的和没有 manifest 的条目，逐个加载插件；错误带 manifest 或条目路径。
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") || slices.Contains(cfg.Plugins.Disabled, name) {
			continue
		}
		directory := filepath.Join(cfg.Plugins.Directory, name)
		info, err := os.Stat(directory)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			// 去掉 PathError 自带的路径前缀，返回条目路径和原始原因。
			return fmt.Errorf("%s: %w", directory, errors.Unwrap(err))
		}
		if !info.IsDir() {
			continue
		}
		if err := cfg.loadPlugin(home, directory); errors.Is(err, errPluginManifestNotFound) {
			continue
		} else if err != nil {
			return err
		}
	}
	return nil
}

// loadPlugin 读取一个插件的 manifest，加载它的 skill 目录、hook 和 MCP server。
func (cfg *Config) loadPlugin(home, directory string) (err error) {
	// 1. 读取 manifest，检查名字合法且和目录名一致。
	manifest, skillDirectories, manifestPath, err := readPluginManifest(directory)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = fmt.Errorf("%s: %w", manifestPath, err)
		}
	}()
	if err := validatePluginName(manifest.Name); err != nil {
		return err
	}
	if manifest.Name != filepath.Base(directory) {
		return fmt.Errorf("plugin name %q must match directory %q", manifest.Name, filepath.Base(directory))
	}
	// 2. 创建插件对象，从安装记录读安装时间；Directory 用扫描到的路径，不解析符号链接。
	plugin := &defs.Plugin{
		Name:      manifest.Name,
		Version:   manifest.Version,
		Directory: directory,
	}
	record := &PluginInstallRecord{}
	if err := readYAML(filepath.Join(directory, pluginInstallRecordName), record); err == nil {
		plugin.InstalledAt = record.InstalledAt
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	// 3. 检查 skill 目录在插件目录里，目录存在时记下。
	for _, skills := range skillDirectories {
		if !isInsidePluginDirectory(skills) {
			return errors.New("plugin skills directory must be inside the plugin directory")
		}
		skillDirectory := filepath.Join(directory, skills)
		if info, err := os.Stat(skillDirectory); err == nil && info.IsDir() && !slices.Contains(plugin.SkillDirectories, skillDirectory) {
			plugin.SkillDirectories = append(plugin.SkillDirectories, skillDirectory)
		}
	}

	// 4. 先检查 hook 脚本没有越出插件目录，再加载 hook 并挂上插件。
	if err := validatePluginHookScripts(manifest.Hooks); err != nil {
		return err
	}
	hooks, err := loadHookGroups(manifest.Hooks, home, directory, "plugin:"+plugin.Name, manifestPath)
	if err != nil {
		return err
	}
	for _, definition := range hooks {
		definition.Plugin = plugin
	}
	cfg.hookDefinitions = append(cfg.hookDefinitions, hooks...)

	// 5. 校验插件的 MCP server（只能在 local 上跑），stdio 的替换插件根目录占位符；mcp_servers/ 里有同 ID 的用那份，插件之间或同一插件里重复 ID 时报错。
	for index, server := range manifest.MCPServers {
		if server == nil {
			return fmt.Errorf("mcp_servers[%d]: mcp server must not be empty", index)
		}
		// 插件不能写 executors，要换 Executor 就在 mcp_servers/ 下写同 ID 的配置。
		if len(server.Executors) > 0 {
			return fmt.Errorf("mcp_servers[%d]: plugin mcp server cannot set executors; override it in mcp_servers/%s.yaml", index, server.ID)
		}
		if err := validateMCPServerConfig(server, cfg.executorDefinitions); err != nil {
			return fmt.Errorf("mcp_servers[%d]: %w", index, err)
		}
		if mcpclient.ServerTransportType(server.Type) == mcpclient.ServerTransportStdio {
			replacePluginRoot(server, directory)
		}
		existing, found := cfg.mcpServerDefinitions[server.ID]
		switch {
		case found && existing.Plugin == nil:
			// mcp_servers/ 里有同 id 的文件：用用户的那份，跳过插件的。
			continue
		case found && existing.Plugin == plugin:
			return fmt.Errorf("mcp_servers[%d]: mcp server %q is declared twice in plugin %q", index, server.ID, plugin.Name)
		case found:
			return fmt.Errorf("mcp_servers[%d]: mcp server %q is declared by plugins %q and %q", index, server.ID, existing.Plugin.Name, plugin.Name)
		}
		server.Plugin = plugin
		cfg.mcpServerDefinitions[server.ID] = server
	}
	// 6. 记下加载好的插件。
	cfg.plugins = append(cfg.plugins, plugin)
	return nil
}

// readPluginManifest 按 OpenVaala、Codex、Claude 的顺序读取一份插件描述；外部格式只提供技能。
func readPluginManifest(directory string) (*PluginManifest, []string, string, error) {
	// 1. 选择第一个存在的 manifest；选中文件损坏时直接返回错误。
	for _, relativePath := range []string{pluginManifestPath, ".codex-plugin/plugin.json", ".claude-plugin/plugin.json"} {
		manifestPath := filepath.Join(directory, relativePath)
		content, err := os.ReadFile(manifestPath)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, manifestPath, err
		}
		// 2. 原生格式保留完整配置，自定义技能目录替换默认目录。
		if relativePath == pluginManifestPath {
			manifest := &PluginManifest{}
			if err := decodeYAML(bytes.NewReader(content), manifest); err != nil {
				return nil, nil, manifestPath, fmt.Errorf("%s: invalid YAML, field type or unknown field", manifestPath)
			}
			return manifest, []string{cmp.Or(manifest.Skills, "./skills/")}, manifestPath, nil
		}
		// 3. 外部格式的自定义目录追加在默认技能目录之后，其他组件字段不参与解析。
		manifest := &skillPluginManifest{}
		if err := json.Unmarshal(content, manifest); err != nil {
			return nil, nil, manifestPath, fmt.Errorf("%s: invalid JSON: %w", manifestPath, err)
		}
		return &PluginManifest{
			Name: manifest.Name, Version: manifest.Version, Description: manifest.Description,
		}, append([]string{"./skills/"}, manifest.Skills...), manifestPath, nil
	}
	return nil, nil, "", errPluginManifestNotFound
}

// validatePluginName 校验插件名。
func validatePluginName(name string) error {
	// 1. 只能有小写字母、数字和单个连字符，不能是保留名 skill。
	if !pluginNamePattern.MatchString(name) {
		return fmt.Errorf("plugin name %q must contain only lowercase letters, digits and single hyphens", name)
	}
	if name == reservedPluginName {
		return fmt.Errorf("plugin name %q is reserved", name)
	}
	return nil
}

// validatePluginHookScripts 检查插件的 hook 脚本路径都在插件目录里。
func validatePluginHookScripts(groups map[string][]HookMatcherConfig) error {
	// 1. 遍历各事件、matcher 组和 hook，脚本是绝对路径、~ 开头或用 .. 越出目录时返回带条目位置的错误。
	for _, event := range slices.Sorted(maps.Keys(groups)) {
		for groupIndex, group := range groups[event] {
			for handlerIndex, handler := range group.Hooks {
				if handler.Script != "" && !isInsidePluginDirectory(handler.Script) {
					return fmt.Errorf("hooks.%s[%d].hooks[%d]: plugin hook script must be inside the plugin directory", event, groupIndex, handlerIndex)
				}
			}
		}
	}
	return nil
}

// replacePluginRoot 把 stdio MCP server 里的 ${OPENVAALA_PLUGIN_ROOT} 换成插件根目录。
func replacePluginRoot(server *MCPServerConfig, directory string) {
	// 1. 替换 command、args 和 env 值里的占位符，再在 env 里加上 OPENVAALA_PLUGIN_ROOT。
	replacer := strings.NewReplacer("${"+pluginRootVariable+"}", directory)
	server.Command = replacer.Replace(server.Command)
	for index, argument := range server.Args {
		server.Args[index] = replacer.Replace(argument)
	}
	if server.Env == nil {
		server.Env = make(map[string]string, 1)
	}
	for key, value := range server.Env {
		server.Env[key] = replacer.Replace(value)
	}
	server.Env[pluginRootVariable] = directory
}

// InstallPlugin 从 git 地址或本机目录安装插件并检查配置，返回插件名和它是否在禁用名单里。
func InstallPlugin(ctx context.Context, opt *LoadConfigOpt, request *InstallPluginRequest) (string, bool, error) {
	// 1. 判断来源是不是本机目录，校验安装参数，只加载应用设置来找到插件目录。
	isLocalSource := isLocalPluginSource(request.Source)
	record, err := newPluginInstallRecord(request, isLocalSource)
	if err != nil {
		return "", false, err
	}
	cfg, _, err := loadConfigSettings(opt)
	if err != nil {
		return "", false, err
	}
	// 2. 创建安装用的临时目录，返回前删除。
	if err := os.MkdirAll(cfg.Plugins.Directory, 0700); err != nil {
		return "", false, err
	}
	installDirectory, err := os.MkdirTemp(cfg.Plugins.Directory, ".install-")
	if err != nil {
		return "", false, err
	}
	defer os.RemoveAll(installDirectory)

	// 3. 取得来源，已有同名插件时报错，再拷贝并移进插件目录。
	repositoryDirectory, name, err := fetchPluginSource(ctx, installDirectory, record, isLocalSource)
	if err != nil {
		return "", false, err
	}
	pluginDirectory := filepath.Join(cfg.Plugins.Directory, name)
	if _, err := os.Lstat(pluginDirectory); err == nil {
		return "", false, fmt.Errorf("plugin %q is already installed; use openvaala plugin update %s", name, name)
	}
	newDirectory := filepath.Join(installDirectory, "plugin")
	if err := copyPluginDirectory(ctx, repositoryDirectory, newDirectory, record); err != nil {
		return "", false, err
	}
	if err := os.Rename(newDirectory, pluginDirectory); err != nil {
		return "", false, err
	}
	// 4. 重新加载整份配置；插件在禁用名单里时加载会跳过它，单独再校验一次。任一步失败都删除新目录。
	loadedConfig, err := LoadConfig(opt)
	if err != nil {
		return "", false, errors.Join(err, os.RemoveAll(pluginDirectory))
	}
	isDisabled := slices.Contains(loadedConfig.Plugins.Disabled, name)
	if isDisabled {
		if err := loadedConfig.loadPlugin(opt.Home, pluginDirectory); err != nil {
			return "", false, errors.Join(err, os.RemoveAll(pluginDirectory))
		}
	}
	return name, isDisabled, nil
}

// UpdatePlugins 按安装记录更新插件，返回更新成功的插件名。
func UpdatePlugins(ctx context.Context, opt *LoadConfigOpt, names []string) ([]string, error) {
	// 1. 加载应用设置；没给名字时更新所有带安装记录的插件。
	cfg, _, err := loadConfigSettings(opt)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		names, err = getRecordedPluginNames(cfg.Plugins.Directory)
		if err != nil {
			return nil, err
		}
	}
	// 2. 逐个更新，某个失败时记下错误继续下一个；返回成功的名单和带插件名的合并错误。
	var updatedNames []string
	var errs []error
	for _, name := range names {
		if err := updatePlugin(ctx, opt, cfg.Plugins.Directory, name); err != nil {
			errs = append(errs, fmt.Errorf("plugin %q: %w", name, err))
			continue
		}
		updatedNames = append(updatedNames, name)
	}
	return updatedNames, errors.Join(errs...)
}

// UninstallPlugin 卸载插件，并把它从 plugins.disabled 里去掉。
func UninstallPlugin(opt *LoadConfigOpt, name string) error {
	// 1. 加载应用设置，找到插件，准备好去掉禁用名单里这个名字后的配置内容。
	cfg, path, err := loadConfigSettings(opt)
	if err != nil {
		return err
	}
	pluginDirectory, isInstalled := getInstalledPluginDirectory(cfg.Plugins.Directory, name)
	if !isInstalled {
		return fmt.Errorf("plugin %q is not installed", name)
	}
	original, err := readOriginalFile(path)
	if err != nil {
		return err
	}
	content, isChanged, err := setPluginDisabledContent(original.content, name, false)
	if err != nil {
		return &Error{
			Path:   path,
			Reason: "invalid YAML",
		}
	}
	// 2. 创建卸载用的临时目录，把插件移进去，返回前删除；插件是符号链接时只移动和删除链接本身。
	uninstallDirectory, err := os.MkdirTemp(cfg.Plugins.Directory, ".uninstall-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(uninstallDirectory)

	removedDirectory := filepath.Join(uninstallDirectory, name)
	if err := os.Rename(pluginDirectory, removedDirectory); err != nil {
		return err
	}
	// 3. 禁用名单有变化时写入配置文件，再重新加载整份配置；失败时把插件目录和配置文件都恢复。
	if isChanged {
		if err := writeFileAtomic(path, content); err != nil {
			return errors.Join(err, os.Rename(removedDirectory, pluginDirectory))
		}
	}
	if _, err := LoadConfig(opt); err != nil {
		err = errors.Join(err, os.Rename(removedDirectory, pluginDirectory))
		if isChanged {
			err = errors.Join(err, original.restore())
		}
		return err
	}
	return nil
}

// SetPluginDisabled 修改插件的禁用状态，返回配置有没有变化。
func SetPluginDisabled(opt *LoadConfigOpt, name string, isDisabled bool) (bool, error) {
	// 1. 只加载应用设置，检查插件已安装，读取原配置文件。
	cfg, path, err := loadConfigSettings(opt)
	if err != nil {
		return false, err
	}
	if _, isInstalled := getInstalledPluginDirectory(cfg.Plugins.Directory, name); !isInstalled {
		return false, fmt.Errorf("plugin %q is not installed", name)
	}
	original, err := readOriginalFile(path)
	if err != nil {
		return false, err
	}
	// 2. 修改 plugins.disabled，保留其他内容和注释；已经是目标状态时直接返回。
	content, isChanged, err := setPluginDisabledContent(original.content, name, isDisabled)
	if err != nil {
		return false, &Error{
			Path:   path,
			Reason: "invalid YAML",
		}
	}
	if !isChanged {
		return false, nil
	}
	// 3. 写入后重新加载整份配置，加载失败时恢复原文件。
	if err := writeFileAtomic(path, content); err != nil {
		return false, err
	}
	if _, err := LoadConfig(opt); err != nil {
		return false, errors.Join(err, original.restore())
	}
	return true, nil
}

// ListPlugins 按名字顺序列出插件目录里的条目和安装信息。
func ListPlugins(opt *LoadConfigOpt) ([]PluginListEntry, error) {
	// 1. 加载应用设置并读取插件目录，目录不存在时返回空列表。
	cfg, _, err := loadConfigSettings(opt)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(cfg.Plugins.Directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// 2. 跳过隐藏条目和不是目录的，读取 manifest、禁用状态和安装记录；manifest 缺失或损坏、安装记录读不了时写进这一项的 Error。
	var listEntries []PluginListEntry
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		directory := filepath.Join(cfg.Plugins.Directory, name)
		if info, err := os.Stat(directory); err != nil || !info.IsDir() {
			continue
		}
		listEntry := PluginListEntry{
			Name:       name,
			IsDisabled: slices.Contains(cfg.Plugins.Disabled, name),
		}
		manifest, _, _, err := readPluginManifest(directory)
		switch {
		case err != nil:
			listEntry.Error = err.Error()
		default:
			listEntry.Version = manifest.Version
		}
		record := &PluginInstallRecord{}
		switch err := readYAML(filepath.Join(directory, pluginInstallRecordName), record); {
		case err == nil:
			listEntry.Record = record
		case !errors.Is(err, os.ErrNotExist):
			listEntry.Error = cmp.Or(listEntry.Error, err.Error())
		}
		listEntries = append(listEntries, listEntry)
	}
	return listEntries, nil
}

// updatePlugin 按安装记录重新拉取一个插件，替换旧目录。
func updatePlugin(ctx context.Context, opt *LoadConfigOpt, pluginsDirectory, name string) error {
	// 1. 找到插件并读取安装记录，手动放入、没有记录的插件返回错误。
	pluginDirectory, isInstalled := getInstalledPluginDirectory(pluginsDirectory, name)
	if !isInstalled {
		return errors.New("not installed")
	}
	record := &PluginInstallRecord{}
	if err := readYAML(filepath.Join(pluginDirectory, pluginInstallRecordName), record); errors.Is(err, os.ErrNotExist) {
		return errors.New("not installed by openvaala plugin install")
	} else if err != nil {
		return err
	}
	// 2. 创建临时目录，按记录重新取得来源，检查新 manifest 的名字没变，再拷贝新版本。
	installDirectory, err := os.MkdirTemp(pluginsDirectory, ".install-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(installDirectory)

	// 安装记录里只有 git 来源才有 commit。
	repositoryDirectory, sourceName, err := fetchPluginSource(ctx, installDirectory, record, record.Commit == "")
	if err != nil {
		return err
	}
	if sourceName != name {
		return fmt.Errorf("the new version is named %q; uninstall it and install the new one", sourceName)
	}
	newDirectory := filepath.Join(installDirectory, "plugin")
	if err := copyPluginDirectory(ctx, repositoryDirectory, newDirectory, record); err != nil {
		return err
	}
	// 3. 把旧目录挪进临时目录，换上新目录，再重新加载配置；换入或加载失败时恢复旧目录，返回前清理临时目录。
	previousDirectory := filepath.Join(installDirectory, "previous")
	if err := os.Rename(pluginDirectory, previousDirectory); err != nil {
		return err
	}
	if err := os.Rename(newDirectory, pluginDirectory); err != nil {
		return errors.Join(err, os.Rename(previousDirectory, pluginDirectory))
	}
	if _, err := LoadConfig(opt); err != nil {
		return errors.Join(err, os.RemoveAll(pluginDirectory), os.Rename(previousDirectory, pluginDirectory))
	}
	return nil
}

// newPluginInstallRecord 把插件安装请求转成安装记录。
func newPluginInstallRecord(request *InstallPluginRequest, isLocalSource bool) (*PluginInstallRecord, error) {
	// 1. 清理仓库内的相对路径，仓库根目录记为空。
	path := filepath.Clean(request.Path)
	if path == "." {
		path = ""
	}
	record := &PluginInstallRecord{
		Source: request.Source,
		Ref:    request.Ref,
		Path:   path,
	}
	// 2. git 来源原样保留；本机来源不能带 Ref，Source 转成绝对路径。
	if !isLocalSource {
		return record, nil
	}
	if record.Ref != "" {
		return nil, errors.New("--ref requires a git source")
	}
	source, err := filepath.Abs(record.Source)
	if err != nil {
		return nil, err
	}
	record.Source = source
	return record, nil
}

// fetchPluginSource 取得插件来源并读取 manifest，返回来源仓库目录和插件名。
func fetchPluginSource(ctx context.Context, installDirectory string, record *PluginInstallRecord, isLocalSource bool) (string, string, error) {
	// 1. 检查 Path 是仓库内的相对路径。
	if record.Path != "" && !filepath.IsLocal(record.Path) {
		return "", "", errors.New("--path must be a relative path inside the repository")
	}
	// 2. 本机来源检查目录存在；git 来源浅克隆指定的分支或 tag，把 HEAD 提交写进 record.Commit。
	repositoryDirectory := record.Source
	if isLocalSource {
		if _, err := os.Stat(repositoryDirectory); errors.Is(err, os.ErrNotExist) {
			return "", "", fmt.Errorf("plugin source directory %s does not exist", repositoryDirectory)
		}
	} else {
		repositoryDirectory = filepath.Join(installDirectory, "repo")
		arguments := []string{"clone", "--quiet", "--depth", "1"}
		if record.Ref != "" {
			arguments = append(arguments, "--branch", record.Ref)
		}
		if _, err := runGit(ctx, installDirectory, append(arguments, "--", record.Source, repositoryDirectory)...); err != nil {
			return "", "", err
		}
		commit, err := runGit(ctx, repositoryDirectory, "rev-parse", "HEAD")
		if err != nil {
			return "", "", err
		}
		record.Commit = commit
	}
	// 3. 读取插件目录的 manifest 并校验名字，manifest 不存在时返回带 --path 提示的错误。
	manifest, _, manifestPath, err := readPluginManifest(filepath.Join(repositoryDirectory, record.Path))
	if errors.Is(err, errPluginManifestNotFound) {
		return "", "", fmt.Errorf("plugin not found at %s; use --path to point at the plugin directory", cmp.Or(record.Path, record.Source))
	} else if err != nil {
		return "", "", err
	}
	if err := validatePluginName(manifest.Name); err != nil {
		return "", "", fmt.Errorf("%s: %w", manifestPath, err)
	}
	return repositoryDirectory, manifest.Name, nil
}

// copyPluginDirectory 把来源插件目录拷到 targetDirectory，并写入安装记录。
func copyPluginDirectory(ctx context.Context, repositoryDirectory, targetDirectory string, record *PluginInstallRecord) error {
	// 1. 创建本机工作区，解析出仓库和来源插件目录的真实路径。
	local, err := workspace.NewLocalWorkspace(workspace.NewLocalWorkspaceOpt("/"))
	if err != nil {
		return err
	}
	realRepositoryDirectory, err := filepath.EvalSymlinks(repositoryDirectory)
	if err != nil {
		return err
	}
	sourceDirectory := filepath.Join(repositoryDirectory, record.Path)
	realSourceDirectory, err := filepath.EvalSymlinks(sourceDirectory)
	if err != nil {
		return err
	}
	// 2. 用 0755 创建插件目录并拷贝条目。
	if err := os.Mkdir(targetDirectory, 0755); err != nil {
		return err
	}
	if err := copyPluginEntries(ctx, local, realRepositoryDirectory, sourceDirectory, targetDirectory, []string{realSourceDirectory}); err != nil {
		return err
	}
	// 3. 记下当前时间作为安装时间，用 0600 写入安装记录。
	record.InstalledAt = time.Now().UTC()
	content, err := encodeYAML(record)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(targetDirectory, pluginInstallRecordName), content, 0600)
}

// copyPluginEntries 递归拷贝插件目录里的文件和子目录；walkDirectories 是从插件根目录到当前目录一路的真实路径。
func copyPluginEntries(ctx context.Context, local workspace.Workspace, realRepositoryDirectory, sourceDirectory, targetDirectory string, walkDirectories []string) error {
	// 1. 读取来源目录，跳过 .git、坏链接和特殊文件，普通文件和指向文件的链接按内容拷贝。
	entries, err := os.ReadDir(sourceDirectory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == ".git" {
			continue
		}
		sourcePath := filepath.Join(sourceDirectory, entry.Name())
		targetPath := filepath.Join(targetDirectory, entry.Name())
		isSymlink := entry.Type()&fs.ModeSymlink != 0
		info, err := os.Stat(sourcePath)
		if err != nil && isSymlink {
			// 读不到目标的坏链接不拷。
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			if err := workspace.CopyFile(ctx, local, sourcePath, local, targetPath); err != nil {
				return err
			}
			continue
		}
		if !info.IsDir() {
			// 设备文件、管道等特殊文件不拷。
			continue
		}
		// 2. 解析目录链接，指向仓库外或指向正在遍历的目录及其上级时跳过。
		realPath := filepath.Join(walkDirectories[len(walkDirectories)-1], entry.Name())
		if isSymlink {
			if realPath, err = filepath.EvalSymlinks(sourcePath); err != nil {
				return err
			}
			isAncestor := slices.ContainsFunc(walkDirectories, func(directory string) bool { return isSameOrInsideDirectory(realPath, directory) })
			if !isSameOrInsideDirectory(realRepositoryDirectory, realPath) || isAncestor {
				continue
			}
		}
		// 3. 子目录在原权限上加上属主读写执行权限，再递归拷贝。
		if err := os.Mkdir(targetPath, info.Mode().Perm()|0700); err != nil {
			return err
		}
		if err := copyPluginEntries(ctx, local, realRepositoryDirectory, sourcePath, targetPath, append(walkDirectories, realPath)); err != nil {
			return err
		}
	}
	return nil
}

// runGit 在 directory 里执行 git 命令，返回去掉首尾空白的标准输出。
func runGit(ctx context.Context, directory string, arguments ...string) (string, error) {
	// 1. 执行命令；找不到 git 时提示需要 git，执行失败时错误带上 stderr。
	var stdout, stderr bytes.Buffer
	command := exec.CommandContext(ctx, "git", arguments...)
	command.Dir, command.Stdout, command.Stderr = directory, &stdout, &stderr
	if err := command.Run(); errors.Is(err, exec.ErrNotFound) {
		return "", fmt.Errorf("git is required to install plugins from git: %v", err)
	} else if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", arguments[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// isLocalPluginSource 插件来源是不是已存在的本机目录？
func isLocalPluginSource(source string) bool {
	// 1. 检查来源存在且是目录。
	info, err := os.Stat(source)
	return err == nil && info.IsDir()
}

// getInstalledPluginDirectory 返回已安装插件在 plugins 目录里的路径（不解析符号链接），以及是否找到。
func getInstalledPluginDirectory(pluginsDirectory, name string) (string, bool) {
	// 1. 名字必须是单个不以 . 开头的条目名，且这个条目存在。
	isEntryName := filepath.IsLocal(name) && !strings.HasPrefix(name, ".") && filepath.Base(name) == name
	directory := filepath.Join(pluginsDirectory, name)
	if _, err := os.Lstat(directory); !isEntryName || err != nil {
		return "", false
	}
	return directory, true
}

// getRecordedPluginNames 按名字顺序返回带安装记录的插件名。
func getRecordedPluginNames(pluginsDirectory string) ([]string, error) {
	// 1. 读取插件目录，目录不存在时返回空列表。
	entries, err := os.ReadDir(pluginsDirectory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// 2. 跳过隐藏条目，收集有安装记录的插件名。
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if _, err := os.Stat(filepath.Join(pluginsDirectory, name, pluginInstallRecordName)); err == nil {
			names = append(names, name)
		}
	}
	return names, nil
}
