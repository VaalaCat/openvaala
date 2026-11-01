package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"gopkg.in/yaml.v3"
)

// ConfigFiles 读写启动时加载的那份配置的文件，记着 LoadConfigOpt，重新加载时用同一个配置路径和 flag。
// 写方法写完都重新加载整份配置：加载失败时把文件恢复成写之前的样子（新建的删掉）并返回加载错误；
// 成功时返回新的整份定义（见 GetRuntimeDefinitions）。没有加锁，两处同时写时后写的可能覆盖先写的。
type ConfigFiles struct {
	opt LoadConfigOpt
}

// NewConfigFiles 创建 ConfigFiles，保存一份配置加载参数。
func NewConfigFiles(opt *LoadConfigOpt) *ConfigFiles {
	// 1. 复制参数，之后的读写都用同一个路径和 flag。
	return &ConfigFiles{opt: *opt}
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

// getDefaultModelEdit 读取配置文件，返回原文件和改了 default_model 之后的内容；默认模型没变时都返回 nil。
func (f *ConfigFiles) getDefaultModelEdit(ref defs.ModelRef) (*originalFile, []byte, error) {
	// 1. 找到配置文件并记下原内容，默认模型没变时直接返回。
	path, _, err := GetConfigPath(f.opt.Home, f.opt.Path)
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

// getDefaultModelRemoval 默认模型属于 providerID 时，返回原配置文件和去掉 default_model 之后的内容；否则都返回 nil。
func (f *ConfigFiles) getDefaultModelRemoval(providerID string) (*originalFile, []byte, error) {
	// 1. 找到配置文件并记下原内容，默认模型解析不了或不属于这个 Provider 时直接返回。
	path, _, err := GetConfigPath(f.opt.Home, f.opt.Path)
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
	return cfg.GetRuntimeDefinitions(ctx)
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
	path, _, err := GetConfigPath(opt.Home, opt.Path)
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
