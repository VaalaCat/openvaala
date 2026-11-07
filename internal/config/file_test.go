package config_test

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/VaalaCat/openvaala/internal/config"
	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/hook"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/skill"
)

// TestInitConfigConcurrentAndPreservesExisting 验证并发调用 InitConfig 只建一份 0600 的配置文件，已有文件不被改动，不留临时文件。
func TestInitConfigConcurrentAndPreservesExisting(t *testing.T) {
	// 1. 准备没有配置文件和已有配置文件两种情况，各并发调用 8 次。
	tests := []struct {
		name            string
		initialContent  string
		concurrentCalls int
	}{
		{
			name:            "concurrent_creation",
			concurrentCalls: 8,
		},
		{
			name:            "concurrent_initialization_preserves_existing_file",
			initialContent:  "run:\n  agent_name: user-agent\n",
			concurrentCalls: 8,
		},
	}
	// 2. 并发初始化，再核对权限、已有内容、能否加载和临时文件。
	for _, tt := range tests {
		t.Run(tt.name,
			func(t *testing.T) {
				directory := t.TempDir()
				path := filepath.Join(directory, "config.yaml")
				if tt.initialContent != "" {
					if err := os.WriteFile(path, []byte(tt.initialContent), 0600); err != nil {
						t.Fatal(err)
					}
				}
				var tasks sync.WaitGroup
				for range tt.concurrentCalls {
					tasks.Add(1)
					go func() {
						defer tasks.Done()
						if _, err := config.InitConfig(&config.LoadConfigOpt{
							Home: directory,
							Path: path,
						}); err != nil {
							t.Error(err)
						}
					}()
				}
				tasks.Wait()
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != 0600 {
					t.Fatalf("permissions = %o, want 600", info.Mode().Perm())
				}
				got, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if tt.initialContent != "" && string(got) != tt.initialContent {
					t.Fatalf("existing content = %q, want %q", got, tt.initialContent)
				}
				if _, err := config.LoadConfig(&config.LoadConfigOpt{
					Home: directory,
					Path: path,
				}); err != nil {
					t.Fatalf("initialized configuration cannot be loaded: %v", err)
				}
				files, err := filepath.Glob(filepath.Join(directory, ".config-*"))
				if err != nil || len(files) != 0 {
					t.Fatalf("temporary files left: %v %v", files, err)
				}
			})
	}
}

// newConfigFilesProvider 创建测试用的 Provider 配置；给了 modelIDs 时只含这些模型。
func newConfigFilesProvider(id string, modelIDs ...string) defs.ProviderConfig {
	// 1. 填满 Provider 字段，默认带字段齐全的 m 和值为 nil 的 n；给了 modelIDs 时换成这些值为 nil 的模型。
	thinking := true
	provider := defs.ProviderConfig{
		ID:        id,
		Protocol:  "openai_chat",
		BaseURL:   "https://gw.example/v1",
		Endpoints: map[string]string{"claude": "https://claude.example/v1/messages"},
		APIKey:    "sk-" + id,
		Models: map[string]*defs.ProviderModelConfig{
			"m": {
				UpstreamModel:          "upstream-m",
				DisplayName:            "Model M",
				ContextWindow:          128000,
				MaxOutputTokens:        8000,
				Thinking:               &thinking,
				ReasoningEfforts:       defs.OptionalStringList{"low", "high"},
				DefaultReasoningEffort: "low",
				MergeMessageRoles:      defs.OptionalStringList{},
			},
			"n": nil,
		},
	}
	if len(modelIDs) > 0 {
		provider.Models = map[string]*defs.ProviderModelConfig{}
		for _, modelID := range modelIDs {
			provider.Models[modelID] = nil
		}
	}
	return provider
}

// newConfigFilesProviderYAML 生成测试用的 Provider 文件内容。
func newConfigFilesProviderYAML(id string, modelIDs ...string) string {
	// 1. 拼出 id、base_url、api_key 和模型列表。
	content := "id: " + id + "\nbase_url: https://gw.example/v1\napi_key: sk-" + id + "\nmodels:\n"
	for _, modelID := range modelIDs {
		content += "  " + modelID + ": {}\n"
	}
	return content
}

// readConfigFilesTree 读出目录下全部文件，返回相对路径到内容的映射。
func readConfigFilesTree(t *testing.T, directory string) map[string]string {
	t.Helper()
	// 1. 遍历目录以外的条目，读取时跟随文件链接。
	tree := map[string]string{}
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(directory, path)
		tree[filepath.ToSlash(relative)] = string(content)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

// assertConfigFilesMode 核对文件或目录的权限。
func assertConfigFilesMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	// 1. 读取文件信息并比较权限位。
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != want {
		t.Fatalf("%s permissions = %o, want %o", path, info.Mode().Perm(), want)
	}
}

// getConfigFilesLinkTarget 返回测试里符号链接指向的真实文件路径，在配置目录旁边的 dotfiles 目录下。
func getConfigFilesLinkTarget(configDirectory, relative string) string {
	// 1. 相对路径里的 / 换成 _ 作为文件名。
	return filepath.Join(filepath.Dir(configDirectory), "dotfiles", strings.ReplaceAll(relative, "/", "_"))
}

// assertConfigFilesSymlink 核对配置目录里的链接还是原来的符号链接，目标内容符合要求，目标旁边没有留下临时文件。
func assertConfigFilesSymlink(t *testing.T, configDirectory, relative string, isContentWanted func(content string) bool) {
	t.Helper()
	// 1. 检查链接和它的指向，读取目标内容交给 isContentWanted 判断。
	link := filepath.Join(configDirectory, filepath.FromSlash(relative))
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is no longer a symbolic link", relative)
	}
	target := getConfigFilesLinkTarget(configDirectory, relative)
	if got, err := os.Readlink(link); err != nil || got != target {
		t.Fatalf("%s points to %q (%v), want %q", relative, got, err, target)
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !isContentWanted(string(content)) {
		t.Fatalf("target of %s has unexpected content %q", relative, content)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(target), ".write-*")); len(leftovers) != 0 {
		t.Fatalf("temporary files left next to the target: %v", leftovers)
	}
}

// TestConfigFiles 验证 ConfigFiles 的读写：写 Provider、删 Provider、改默认模型都保留注释并只改相关文件，加载失败时恢复，符号链接写穿到真实文件。
func TestConfigFiles(t *testing.T) {
	// 1. 准备 Provider、默认模型、YAML 注释和符号链接的样例。
	ctx := context.Background()
	type configFilesResult struct {
		agents     []*agent.AgentDefinition
		models     []*defs.ModelDefinition
		mcpServers []*mcpclient.ServerDefinition
		hooks      []*hook.HookDefinition
		skills     []*skill.Skill
		executors  []executor.Executor
		// providers、defaultModel 是 ListProviders 的结果。
		providers    []defs.ProviderConfig
		defaultModel defs.ModelRef
		err          error
	}
	// newResult 把写方法和 Reload 返回的整份定义拆进 configFilesResult；出错时定义为 nil。
	newResult := func(definitions *runtime.Definitions, err error) configFilesResult {
		if definitions == nil {
			return configFilesResult{err: err}
		}
		executors, executorsErr := definitions.Executors.GetExecutors(ctx, "")
		return configFilesResult{
			agents:     definitions.Agents,
			models:     definitions.Models,
			mcpServers: definitions.MCPServers,
			hooks:      definitions.Hooks,
			skills:     definitions.Skills,
			executors:  executors,
			err:        errors.Join(err, executorsErr),
		}
	}
	// 每个用例在 <root>/home 下布置 files（相对配置所在目录）；useDefaultConfigPath 为 true 时不指定配置路径，
	// 配置目录就是 <root>/home/.config/openvaala。
	type configFilesCase struct {
		name                 string
		files                map[string]string
		useDefaultConfigPath bool
		// linkedFiles 是配置目录里的符号链接（相对路径）和它们指向的真实文件内容；真实文件在配置目录旁边的 dotfiles 目录下。
		linkedFiles map[string]string
		act         func(t *testing.T, configDirectory string, files *config.ConfigFiles) configFilesResult
		// wantErr 是完整的错误文本；wantLoadErr 是配置加载失败时错误文本应包含的片段。
		wantErr     string
		wantLoadErr string
		// wantFiles 非 nil 时，配置目录下的文件必须正好是这些（相对路径到内容）。
		wantFiles map[string]string
		check     func(t *testing.T, configDirectory string, files *config.ConfigFiles, result configFilesResult)
	}
	tests := []configFilesCase{
		{
			name:  "set_provider_creates_private_file_and_reads_back",
			files: map[string]string{"config.yaml": "{}\n"},
			act: func(t *testing.T, _ string, files *config.ConfigFiles) configFilesResult {
				return newResult(files.SetProvider(ctx, newConfigFilesProvider("p"), defs.ModelRef{}))
			},
			check: func(t *testing.T, configDirectory string, files *config.ConfigFiles, result configFilesResult) {
				assertConfigFilesMode(t, filepath.Join(configDirectory, "models"), 0700)
				assertConfigFilesMode(t, filepath.Join(configDirectory, "models", "p.yaml"), 0600)
				var refs []string
				for _, model := range result.models {
					refs = append(refs, model.Ref.String())
				}
				if !slices.Equal(refs, []string{"p/m", "p/n"}) {
					t.Fatalf("returned models = %v, want p/m and p/n", refs)
				}
				if len(result.agents) != 1 || result.agents[0].Name != "default" || result.agents[0].Model.String() != "p/m" {
					t.Fatalf("returned agents = %+v, want default agent on p/m", result.agents)
				}
				providers, defaultModel, _, err := files.ListProviders(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if want := []defs.ProviderConfig{newConfigFilesProvider("p")}; !reflect.DeepEqual(providers, want) || defaultModel.String() != "p/m" {
					t.Fatalf("ListProviders() = %+v, %v, want %+v and p/m", providers, defaultModel, want)
				}
				if tree := readConfigFilesTree(t, configDirectory); len(tree) != 2 {
					t.Fatalf("files = %v, want only config.yaml and models/p.yaml", slices.Sorted(maps.Keys(tree)))
				}
			},
		},
		{
			name:                 "set_provider_creates_directories_on_fresh_home",
			useDefaultConfigPath: true,
			act: func(t *testing.T, _ string, files *config.ConfigFiles) configFilesResult {
				return newResult(files.SetProvider(ctx, newConfigFilesProvider("p", "m"), defs.ModelRef{}))
			},
			check: func(t *testing.T, configDirectory string, _ *config.ConfigFiles, result configFilesResult) {
				assertConfigFilesMode(t, filepath.Join(configDirectory, "models"), 0700)
				assertConfigFilesMode(t, filepath.Join(configDirectory, "models", "p.yaml"), 0600)
				if tree := readConfigFilesTree(t, configDirectory); len(tree) != 1 {
					t.Fatalf("files = %v, want only models/p.yaml", slices.Sorted(maps.Keys(tree)))
				}
				if len(result.models) != 1 || result.models[0].Ref.String() != "p/m" {
					t.Fatalf("returned models = %+v, want p/m", result.models)
				}
			},
		},
		{
			name: "set_provider_replaces_models_and_list_providers_sorts_by_id",
			files: map[string]string{
				"config.yaml":      "{}\n",
				"models/a.yaml":    newConfigFilesProviderYAML("a", "m"),
				"models/a-b.yaml":  newConfigFilesProviderYAML("a-b", "m"),
				"models/zeta.yaml": newConfigFilesProviderYAML("zeta", "old"),
			},
			act: func(t *testing.T, _ string, files *config.ConfigFiles) configFilesResult {
				return newResult(files.SetProvider(ctx, newConfigFilesProvider("zeta", "fresh"), defs.ModelRef{}))
			},
			check: func(t *testing.T, _ string, files *config.ConfigFiles, result configFilesResult) {
				var refs []string
				for _, model := range result.models {
					refs = append(refs, model.Ref.String())
				}
				if !slices.Equal(refs, []string{"a/m", "a-b/m", "zeta/fresh"}) {
					t.Fatalf("returned models = %v", refs)
				}
				providers, _, _, err := files.ListProviders(ctx)
				if err != nil {
					t.Fatal(err)
				}
				var ids []string
				for _, provider := range providers {
					ids = append(ids, provider.ID)
				}
				if !slices.Equal(ids, []string{"a", "a-b", "zeta"}) {
					t.Fatalf("ListProviders ids = %v, want sorted by id", ids)
				}
			},
		},
		{
			name: "set_provider_writes_into_configured_models_directory",
			files: map[string]string{
				"config.yaml": "models:\n  directory: providers\n",
			},
			act: func(t *testing.T, _ string, files *config.ConfigFiles) configFilesResult {
				return newResult(files.SetProvider(ctx, newConfigFilesProvider("p", "m"), defs.ModelRef{}))
			},
			check: func(t *testing.T, configDirectory string, _ *config.ConfigFiles, _ configFilesResult) {
				if _, err := os.Stat(filepath.Join(configDirectory, "providers", "p.yaml")); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(filepath.Join(configDirectory, "models")); !os.IsNotExist(err) {
					t.Fatalf("models directory must not be created: %v", err)
				}
			},
		},
		{
			name: "set_provider_restores_file_when_default_model_is_removed",
			files: map[string]string{
				"config.yaml":   "# keep\nmodels:\n  default_model: p/b\n",
				"models/p.yaml": newConfigFilesProviderYAML("p", "a", "b"),
			},
			act: func(t *testing.T, _ string, files *config.ConfigFiles) configFilesResult {
				return newResult(files.SetProvider(ctx, newConfigFilesProvider("p", "a"), defs.ModelRef{}))
			},
			wantLoadErr: "models.default_model: model p/b is not configured",
			wantFiles: map[string]string{
				"config.yaml":   "# keep\nmodels:\n  default_model: p/b\n",
				"models/p.yaml": newConfigFilesProviderYAML("p", "a", "b"),
			},
		},
		{
			name:  "set_provider_removes_new_file_when_configuration_does_not_load",
			files: map[string]string{"config.yaml": "{}\n"},
			act: func(t *testing.T, _ string, files *config.ConfigFiles) configFilesResult {
				provider := newConfigFilesProvider("p")
				provider.BaseURL = "ftp://gw.example"
				return newResult(files.SetProvider(ctx, provider, defs.ModelRef{}))
			},
			wantLoadErr: "invalid provider base_url",
			wantFiles:   map[string]string{"config.yaml": "{}\n"},
		},
		{
			name: "set_provider_restores_file_when_compact_window_is_too_small",
			files: map[string]string{
				"config.yaml":           "models:\n  default_model: p/main\n",
				"models/p.yaml":         "id: p\nbase_url: https://gw.example/v1\napi_key: sk-p\nmodels:\n  main: {context_window: 128000}\n  compact: {context_window: 256000}\n",
				"agents/assistant.yaml": "name: assistant\ninstructions: Test\nmodel: {provider: p, model: main}\ncompact_model: {provider: p, model: compact}\n",
			},
			act: func(t *testing.T, _ string, files *config.ConfigFiles) configFilesResult {
				provider := newConfigFilesProvider("p", "main", "compact")
				provider.Models["main"] = &defs.ProviderModelConfig{ContextWindow: 256000}
				provider.Models["compact"] = &defs.ProviderModelConfig{ContextWindow: 128000}
				return newResult(files.SetProvider(ctx, provider, defs.ModelRef{
					ProviderID: "p",
					ModelID:    "compact",
				}))
			},
			wantLoadErr: "compact_model context window 128000 is smaller than model context window 256000",
			wantFiles: map[string]string{
				"config.yaml":           "models:\n  default_model: p/main\n",
				"models/p.yaml":         "id: p\nbase_url: https://gw.example/v1\napi_key: sk-p\nmodels:\n  main: {context_window: 128000}\n  compact: {context_window: 256000}\n",
				"agents/assistant.yaml": "name: assistant\ninstructions: Test\nmodel: {provider: p, model: main}\ncompact_model: {provider: p, model: compact}\n",
			},
		},
		{
			// 去掉 Provider 里当前的默认模型、同时选新的默认模型：两个文件一起写、只加载一次，配置文件只改 default_model 一项。
			name: "set_provider_with_default_model_replaces_the_pinned_default",
			files: map[string]string{
				"config.yaml":   "# keep\nmodels:\n  default_model: p/b # pinned\n",
				"models/p.yaml": newConfigFilesProviderYAML("p", "a", "b"),
			},
			act: func(t *testing.T, _ string, files *config.ConfigFiles) configFilesResult {
				return newResult(files.SetProvider(ctx, newConfigFilesProvider("p", "a"), defs.ModelRef{
					ProviderID: "p",
					ModelID:    "a",
				}))
			},
			check: func(t *testing.T, configDirectory string, files *config.ConfigFiles, result configFilesResult) {
				if tree := readConfigFilesTree(t, configDirectory); tree["config.yaml"] != "# keep\nmodels:\n  default_model: p/a # pinned\n" {
					t.Fatalf("config.yaml = %q", tree["config.yaml"])
				}
				if len(result.models) != 1 || result.models[0].Ref.String() != "p/a" || len(result.agents) != 1 || result.agents[0].Model.String() != "p/a" {
					t.Fatalf("returned models = %+v, agents = %+v, want only p/a", result.models, result.agents)
				}
				providers, defaultModel, _, err := files.ListProviders(ctx)
				if err != nil || !reflect.DeepEqual(providers, []defs.ProviderConfig{newConfigFilesProvider("p", "a")}) || defaultModel.String() != "p/a" {
					t.Fatalf("ListProviders() = %+v, %v, %v", providers, defaultModel, err)
				}
			},
		},
		{
			// 默认模型没变（「保持当前默认模型」）：config.yaml 一个字节都不改，手写的空行、注释和对齐都在。
			name: "set_provider_with_unchanged_default_model_keeps_config_bytes",
			files: map[string]string{
				"config.yaml":   "# my settings\n\nrun:\n  agent_name: default   # spaced comment\n\nmodels:\n  # pinned by hand\n  default_model:   p/a\n",
				"models/p.yaml": newConfigFilesProviderYAML("p", "a"),
			},
			act: func(t *testing.T, _ string, files *config.ConfigFiles) configFilesResult {
				return newResult(files.SetProvider(ctx, newConfigFilesProvider("p", "a", "b"), defs.ModelRef{
					ProviderID: "p",
					ModelID:    "a",
				}))
			},
			check: func(t *testing.T, configDirectory string, _ *config.ConfigFiles, result configFilesResult) {
				if tree := readConfigFilesTree(t, configDirectory); tree["config.yaml"] != "# my settings\n\nrun:\n  agent_name: default   # spaced comment\n\nmodels:\n  # pinned by hand\n  default_model:   p/a\n" {
					t.Fatalf("config.yaml = %q", tree["config.yaml"])
				}
				if len(result.models) != 2 || len(result.agents) != 1 || result.agents[0].Model.String() != "p/a" {
					t.Fatalf("returned models = %+v, agents = %+v", result.models, result.agents)
				}
			},
		},
		{
			name: "set_default_model_unchanged_keeps_config_bytes",
			files: map[string]string{
				"config.yaml":   "# my settings\n\nrun:\n  agent_name: default   # spaced comment\n\nmodels:\n  # pinned by hand\n  default_model:   p/a\n",
				"models/p.yaml": newConfigFilesProviderYAML("p", "a"),
			},
			act: func(t *testing.T, _ string, files *config.ConfigFiles) configFilesResult {
				return newResult(files.SetDefaultModel(ctx, defs.ModelRef{
					ProviderID: "p",
					ModelID:    "a",
				}))
			},
			wantFiles: map[string]string{
				"config.yaml":   "# my settings\n\nrun:\n  agent_name: default   # spaced comment\n\nmodels:\n  # pinned by hand\n  default_model:   p/a\n",
				"models/p.yaml": newConfigFilesProviderYAML("p", "a"),
			},
		},
		{
			name: "set_provider_with_default_model_restores_both_files_when_configuration_does_not_load",
			files: map[string]string{
				"config.yaml":   "# keep\nmodels:\n  default_model: p/b\n",
				"models/p.yaml": newConfigFilesProviderYAML("p", "a", "b"),
			},
			act: func(t *testing.T, _ string, files *config.ConfigFiles) configFilesResult {
				provider := newConfigFilesProvider("p", "a")
				provider.BaseURL = "ftp://gw.example"
				return newResult(files.SetProvider(ctx, provider, defs.ModelRef{
					ProviderID: "p",
					ModelID:    "a",
				}))
			},
			wantLoadErr: "invalid provider base_url",
			wantFiles: map[string]string{
				"config.yaml":   "# keep\nmodels:\n  default_model: p/b\n",
				"models/p.yaml": newConfigFilesProviderYAML("p", "a", "b"),
			},
		},
		{
			name: "list_providers_returns_explicit_default_model",
			files: map[string]string{
				"config.yaml":   "models:\n  default_model: b/y\n",
				"models/a.yaml": newConfigFilesProviderYAML("a", "x"),
				"models/b.yaml": newConfigFilesProviderYAML("b", "y"),
			},
			act: func(t *testing.T, _ string, files *config.ConfigFiles) configFilesResult {
				providers, defaultModel, _, err := files.ListProviders(ctx)
				return configFilesResult{
					providers:    providers,
					defaultModel: defaultModel,
					err:          err,
				}
			},
			check: func(t *testing.T, _ string, _ *config.ConfigFiles, result configFilesResult) {
				if len(result.providers) != 2 || result.defaultModel != (defs.ModelRef{
					ProviderID: "b",
					ModelID:    "y",
				}) {
					t.Fatalf("ListProviders() = %+v, %v; want two providers and b/y", result.providers, result.defaultModel)
				}
			},
		},
		{
			// 没写 default_model 时默认模型是按 Provider id、模型 id 排序的第一个，不看模型在文件里的顺序。
			name: "list_providers_returns_first_sorted_model_without_explicit_default",
			files: map[string]string{
				"config.yaml":   "{}\n",
				"models/b.yaml": newConfigFilesProviderYAML("b", "m"),
				"models/a.yaml": newConfigFilesProviderYAML("a", "z", "y"),
			},
			act: func(t *testing.T, _ string, files *config.ConfigFiles) configFilesResult {
				providers, defaultModel, _, err := files.ListProviders(ctx)
				return configFilesResult{
					providers:    providers,
					defaultModel: defaultModel,
					err:          err,
				}
			},
			check: func(t *testing.T, _ string, _ *config.ConfigFiles, result configFilesResult) {
				if len(result.providers) != 2 || result.defaultModel != (defs.ModelRef{
					ProviderID: "a",
					ModelID:    "y",
				}) {
					t.Fatalf("ListProviders() = %+v, %v; want two providers and a/y", result.providers, result.defaultModel)
				}
			},
		},
		{
			name:  "list_providers_without_models_has_no_default_model",
			files: map[string]string{"config.yaml": "{}\n"},
			act: func(t *testing.T, _ string, files *config.ConfigFiles) configFilesResult {
				providers, defaultModel, _, err := files.ListProviders(ctx)
				return configFilesResult{
					providers:    providers,
					defaultModel: defaultModel,
					err:          err,
				}
			},
			check: func(t *testing.T, _ string, _ *config.ConfigFiles, result configFilesResult) {
				if len(result.providers) != 0 || result.defaultModel != (defs.ModelRef{}) {
					t.Fatalf("ListProviders() = %+v, %v; want none", result.providers, result.defaultModel)
				}
			},
		},
		{
			// 删掉 default_model 所在的最后一个 Provider：同一次写入去掉 default_model，加载出空的模型目录。
			name: "delete_last_provider_drops_its_pinned_default_model",
			files: map[string]string{
				"config.yaml":   "# keep\nrun:\n  agent_name: default\nmodels:\n  directory: models\n  default_model: p/a # pinned\n",
				"models/p.yaml": newConfigFilesProviderYAML("p", "a"),
			},
			act: func(t *testing.T, _ string, files *config.ConfigFiles) configFilesResult {
				return newResult(files.DeleteProvider(ctx, "p"))
			},
			wantFiles: map[string]string{
				"config.yaml": "# keep\nrun:\n  agent_name: default\nmodels:\n  directory: models\n",
			},
			check: func(t *testing.T, _ string, files *config.ConfigFiles, result configFilesResult) {
				if len(result.models) != 0 || len(result.agents) != 1 || result.agents[0].Model != (defs.ModelRef{}) {
					t.Fatalf("returned models = %+v, agents = %+v; want an empty catalog", result.models, result.agents)
				}
				if providers, defaultModel, _, err := files.ListProviders(ctx); err != nil || len(providers) != 0 || defaultModel != (defs.ModelRef{}) {
					t.Fatalf("ListProviders() = %+v, %v, %v", providers, defaultModel, err)
				}
			},
		},
		{
			name: "delete_provider_keeps_default_model_of_another_provider",
			files: map[string]string{
				"config.yaml":   "models:\n  default_model:   q/m # pinned\n",
				"models/p.yaml": newConfigFilesProviderYAML("p", "m"),
				"models/q.yaml": newConfigFilesProviderYAML("q", "m"),
			},
			act: func(t *testing.T, _ string, files *config.ConfigFiles) configFilesResult {
				return newResult(files.DeleteProvider(ctx, "p"))
			},
			wantFiles: map[string]string{
				"config.yaml":   "models:\n  default_model:   q/m # pinned\n",
				"models/q.yaml": newConfigFilesProviderYAML("q", "m"),
			},
		},
		{
			name: "delete_provider_with_pinned_default_restores_both_files_when_configuration_does_not_load",
			files: map[string]string{
				"config.yaml":        "models:\n  default_model: p/m\n",
				"agents/worker.yaml": "name: worker\ninstructions: Work.\nmodel:\n  provider: p\n  model: m\n",
				"models/p.yaml":      newConfigFilesProviderYAML("p", "m"),
				"models/q.yaml":      newConfigFilesProviderYAML("q", "m"),
			},
			act: func(t *testing.T, _ string, files *config.ConfigFiles) configFilesResult {
				return newResult(files.DeleteProvider(ctx, "p"))
			},
			wantLoadErr: `model "m" is not configured in provider "p"`,
			wantFiles: map[string]string{
				"config.yaml":        "models:\n  default_model: p/m\n",
				"agents/worker.yaml": "name: worker\ninstructions: Work.\nmodel:\n  provider: p\n  model: m\n",
				"models/p.yaml":      newConfigFilesProviderYAML("p", "m"),
				"models/q.yaml":      newConfigFilesProviderYAML("q", "m"),
			},
		},
		{
			name: "delete_provider_removes_file",
			files: map[string]string{
				"config.yaml":   "{}\n",
				"models/p.yaml": newConfigFilesProviderYAML("p", "m"),
				"models/q.yaml": newConfigFilesProviderYAML("q", "m"),
			},
			act: func(t *testing.T, _ string, files *config.ConfigFiles) configFilesResult {
				return newResult(files.DeleteProvider(ctx, "p"))
			},
			wantFiles: map[string]string{
				"config.yaml":   "{}\n",
				"models/q.yaml": newConfigFilesProviderYAML("q", "m"),
			},
			check: func(t *testing.T, _ string, _ *config.ConfigFiles, result configFilesResult) {
				if len(result.models) != 1 || result.models[0].Ref.String() != "q/m" {
					t.Fatalf("returned models = %+v, want only q/m", result.models)
				}
			},
		},
		{
			name: "delete_provider_used_by_agent_restores_file",
			files: map[string]string{
				"config.yaml":         "{}\n",
				"agents/worker.yaml":  "name: worker\ninstructions: Work.\nmodel:\n  provider: p\n  model: m\n",
				"models/p.yaml":       newConfigFilesProviderYAML("p", "m"),
				"models/q.yaml":       newConfigFilesProviderYAML("q", "m"),
				"models/unused.yaml":  newConfigFilesProviderYAML("unused", "m"),
				"agents/helper.yaml":  "name: helper\ninstructions: Help.\n",
				"agents/default.yaml": "name: default\ninstructions: Be concise.\nmodel:\n  provider: q\n  model: m\n",
			},
			act: func(t *testing.T, _ string, files *config.ConfigFiles) configFilesResult {
				return newResult(files.DeleteProvider(ctx, "p"))
			},
			wantLoadErr: `model "m" is not configured in provider "p"`,
			wantFiles: map[string]string{
				"config.yaml":         "{}\n",
				"agents/worker.yaml":  "name: worker\ninstructions: Work.\nmodel:\n  provider: p\n  model: m\n",
				"models/p.yaml":       newConfigFilesProviderYAML("p", "m"),
				"models/q.yaml":       newConfigFilesProviderYAML("q", "m"),
				"models/unused.yaml":  newConfigFilesProviderYAML("unused", "m"),
				"agents/helper.yaml":  "name: helper\ninstructions: Help.\n",
				"agents/default.yaml": "name: default\ninstructions: Be concise.\nmodel:\n  provider: q\n  model: m\n",
			},
		},
		{
			name: "delete_missing_provider",
			files: map[string]string{
				"config.yaml":   "{}\n",
				"models/q.yaml": newConfigFilesProviderYAML("q", "m"),
			},
			act: func(t *testing.T, _ string, files *config.ConfigFiles) configFilesResult {
				return newResult(files.DeleteProvider(ctx, "x"))
			},
			wantErr: "provider x is not configured",
			wantFiles: map[string]string{
				"config.yaml":   "{}\n",
				"models/q.yaml": newConfigFilesProviderYAML("q", "m"),
			},
		},
		{
			name: "set_default_model_keeps_other_content_and_comments",
			files: map[string]string{
				"config.yaml":   "# my settings\nrun:\n  agent_name: default # keep me\nmodels:\n  # which model\n  default_model: p/a # old\n  defaults:\n    context_window: 100000\n# end\n",
				"models/p.yaml": newConfigFilesProviderYAML("p", "a", "b"),
			},
			act: func(t *testing.T, _ string, files *config.ConfigFiles) configFilesResult {
				return newResult(files.SetDefaultModel(ctx, defs.ModelRef{
					ProviderID: "p",
					ModelID:    "b",
				}))
			},
			wantFiles: map[string]string{
				"config.yaml":   "# my settings\nrun:\n  agent_name: default # keep me\nmodels:\n  # which model\n  default_model: p/b # old\n  defaults:\n    context_window: 100000\n# end\n",
				"models/p.yaml": newConfigFilesProviderYAML("p", "a", "b"),
			},
			check: func(t *testing.T, _ string, _ *config.ConfigFiles, result configFilesResult) {
				if len(result.agents) != 1 || result.agents[0].Model.String() != "p/b" {
					t.Fatalf("returned agents = %+v, want default agent on p/b", result.agents)
				}
			},
		},
		{
			name: "set_default_model_adds_models_group_to_empty_mapping",
			files: map[string]string{
				"config.yaml":   "{}\n",
				"models/p.yaml": newConfigFilesProviderYAML("p", "a"),
			},
			act: func(t *testing.T, _ string, files *config.ConfigFiles) configFilesResult {
				return newResult(files.SetDefaultModel(ctx, defs.ModelRef{
					ProviderID: "p",
					ModelID:    "a",
				}))
			},
			wantFiles: map[string]string{
				"config.yaml":   "models:\n  default_model: p/a\n",
				"models/p.yaml": newConfigFilesProviderYAML("p", "a"),
			},
		},
		{
			name: "set_default_model_adds_key_to_existing_models_group",
			files: map[string]string{
				"config.yaml":   "models:\n  directory: models\n",
				"models/p.yaml": newConfigFilesProviderYAML("p", "a"),
			},
			act: func(t *testing.T, _ string, files *config.ConfigFiles) configFilesResult {
				return newResult(files.SetDefaultModel(ctx, defs.ModelRef{
					ProviderID: "p",
					ModelID:    "a",
				}))
			},
			wantFiles: map[string]string{
				"config.yaml":   "models:\n  directory: models\n  default_model: p/a\n",
				"models/p.yaml": newConfigFilesProviderYAML("p", "a"),
			},
		},
		{
			name: "set_default_model_fills_empty_models_group",
			files: map[string]string{
				"config.yaml":   "models:\n",
				"models/p.yaml": newConfigFilesProviderYAML("p", "a"),
			},
			act: func(t *testing.T, _ string, files *config.ConfigFiles) configFilesResult {
				return newResult(files.SetDefaultModel(ctx, defs.ModelRef{
					ProviderID: "p",
					ModelID:    "a",
				}))
			},
			wantFiles: map[string]string{
				"config.yaml":   "models:\n  default_model: p/a\n",
				"models/p.yaml": newConfigFilesProviderYAML("p", "a"),
			},
		},
		{
			name: "set_default_model_appends_to_comment_only_config",
			files: map[string]string{
				"config.yaml":   "# nothing yet\n",
				"models/p.yaml": newConfigFilesProviderYAML("p", "a"),
			},
			act: func(t *testing.T, _ string, files *config.ConfigFiles) configFilesResult {
				return newResult(files.SetDefaultModel(ctx, defs.ModelRef{
					ProviderID: "p",
					ModelID:    "a",
				}))
			},
			wantFiles: map[string]string{
				"config.yaml":   "# nothing yet\nmodels:\n  default_model: p/a\n",
				"models/p.yaml": newConfigFilesProviderYAML("p", "a"),
			},
		},
		{
			name:                 "set_default_model_creates_missing_config",
			useDefaultConfigPath: true,
			files:                map[string]string{"models/p.yaml": newConfigFilesProviderYAML("p", "a")},
			act: func(t *testing.T, _ string, files *config.ConfigFiles) configFilesResult {
				return newResult(files.SetDefaultModel(ctx, defs.ModelRef{
					ProviderID: "p",
					ModelID:    "a",
				}))
			},
			wantFiles: map[string]string{
				"config.yaml":   "models:\n  default_model: p/a\n",
				"models/p.yaml": newConfigFilesProviderYAML("p", "a"),
			},
			check: func(t *testing.T, configDirectory string, _ *config.ConfigFiles, result configFilesResult) {
				assertConfigFilesMode(t, filepath.Join(configDirectory, "config.yaml"), 0600)
				if len(result.models) != 1 || result.models[0].Ref.String() != "p/a" {
					t.Fatalf("returned models = %+v, want p/a", result.models)
				}
			},
		},
		{
			name:                 "set_default_model_with_unknown_model_restores_existing_config",
			useDefaultConfigPath: true,
			files: map[string]string{
				"config.yaml":   "# keep\nmodels:\n  default_model: p/a\n",
				"models/p.yaml": newConfigFilesProviderYAML("p", "a"),
			},
			act: func(t *testing.T, _ string, files *config.ConfigFiles) configFilesResult {
				return newResult(files.SetDefaultModel(ctx, defs.ModelRef{
					ProviderID: "p",
					ModelID:    "gone",
				}))
			},
			wantLoadErr: "models.default_model: model p/gone is not configured",
			wantFiles: map[string]string{
				"config.yaml":   "# keep\nmodels:\n  default_model: p/a\n",
				"models/p.yaml": newConfigFilesProviderYAML("p", "a"),
			},
		},
		{
			name:                 "set_default_model_with_unknown_model_removes_new_config",
			useDefaultConfigPath: true,
			files:                map[string]string{"models/p.yaml": newConfigFilesProviderYAML("p", "a")},
			act: func(t *testing.T, _ string, files *config.ConfigFiles) configFilesResult {
				return newResult(files.SetDefaultModel(ctx, defs.ModelRef{
					ProviderID: "p",
					ModelID:    "gone",
				}))
			},
			wantLoadErr: "models.default_model: model p/gone is not configured",
			wantFiles:   map[string]string{"models/p.yaml": newConfigFilesProviderYAML("p", "a")},
		},
		{
			name:        "set_default_model_writes_through_symlinked_config",
			linkedFiles: map[string]string{"config.yaml": "# managed by dotfiles\nmodels:\n  default_model: p/a # old\n"},
			files:       map[string]string{"models/p.yaml": newConfigFilesProviderYAML("p", "a", "b")},
			act: func(t *testing.T, _ string, files *config.ConfigFiles) configFilesResult {
				return newResult(files.SetDefaultModel(ctx, defs.ModelRef{
					ProviderID: "p",
					ModelID:    "b",
				}))
			},
			check: func(t *testing.T, configDirectory string, _ *config.ConfigFiles, _ configFilesResult) {
				assertConfigFilesSymlink(t, configDirectory, "config.yaml", func(content string) bool {
					return content == "# managed by dotfiles\nmodels:\n  default_model: p/b # old\n"
				})
			},
		},
		{
			name:        "set_provider_writes_through_symlinked_provider_file",
			files:       map[string]string{"config.yaml": "{}\n"},
			linkedFiles: map[string]string{"models/p.yaml": newConfigFilesProviderYAML("p", "old")},
			act: func(t *testing.T, _ string, files *config.ConfigFiles) configFilesResult {
				return newResult(files.SetProvider(ctx, newConfigFilesProvider("p", "fresh"), defs.ModelRef{}))
			},
			check: func(t *testing.T, configDirectory string, files *config.ConfigFiles, result configFilesResult) {
				assertConfigFilesSymlink(t, configDirectory, "models/p.yaml", func(content string) bool {
					return strings.Contains(content, "fresh:") && !strings.Contains(content, "old:")
				})
				if len(result.models) != 1 || result.models[0].Ref.String() != "p/fresh" {
					t.Fatalf("returned models = %+v, want p/fresh", result.models)
				}
			},
		},
		{
			name:        "set_provider_restores_symlinked_provider_file_through_the_link",
			files:       map[string]string{"config.yaml": "{}\n"},
			linkedFiles: map[string]string{"models/p.yaml": newConfigFilesProviderYAML("p", "old")},
			act: func(t *testing.T, _ string, files *config.ConfigFiles) configFilesResult {
				provider := newConfigFilesProvider("p", "fresh")
				provider.BaseURL = "ftp://gw.example"
				return newResult(files.SetProvider(ctx, provider, defs.ModelRef{}))
			},
			wantLoadErr: "invalid provider base_url",
			check: func(t *testing.T, configDirectory string, _ *config.ConfigFiles, _ configFilesResult) {
				assertConfigFilesSymlink(t, configDirectory, "models/p.yaml", func(content string) bool {
					return content == newConfigFilesProviderYAML("p", "old")
				})
			},
		},
		{
			name: "delete_provider_removes_only_the_symlink",
			files: map[string]string{
				"config.yaml":   "{}\n",
				"models/q.yaml": newConfigFilesProviderYAML("q", "m"),
			},
			linkedFiles: map[string]string{"models/p.yaml": newConfigFilesProviderYAML("p", "m")},
			act: func(t *testing.T, _ string, files *config.ConfigFiles) configFilesResult {
				return newResult(files.DeleteProvider(ctx, "p"))
			},
			wantFiles: map[string]string{
				"config.yaml":   "{}\n",
				"models/q.yaml": newConfigFilesProviderYAML("q", "m"),
			},
			check: func(t *testing.T, configDirectory string, _ *config.ConfigFiles, _ configFilesResult) {
				content, err := os.ReadFile(getConfigFilesLinkTarget(configDirectory, "models/p.yaml"))
				if err != nil || string(content) != newConfigFilesProviderYAML("p", "m") {
					t.Fatalf("the file behind the link must stay untouched: %q %v", content, err)
				}
			},
		},
		{
			name: "delete_provider_used_by_agent_restores_the_symlink",
			files: map[string]string{
				"config.yaml":        "{}\n",
				"agents/worker.yaml": "name: worker\ninstructions: Work.\nmodel:\n  provider: p\n  model: m\n",
				"models/q.yaml":      newConfigFilesProviderYAML("q", "m"),
			},
			linkedFiles: map[string]string{"models/p.yaml": newConfigFilesProviderYAML("p", "m")},
			act: func(t *testing.T, _ string, files *config.ConfigFiles) configFilesResult {
				return newResult(files.DeleteProvider(ctx, "p"))
			},
			wantLoadErr: `model "m" is not configured in provider "p"`,
			check: func(t *testing.T, configDirectory string, _ *config.ConfigFiles, _ configFilesResult) {
				assertConfigFilesSymlink(t, configDirectory, "models/p.yaml", func(content string) bool {
					return content == newConfigFilesProviderYAML("p", "m")
				})
			},
		},
		{
			name: "reload_returns_manually_edited_models",
			files: map[string]string{
				"config.yaml":         "{}\n",
				"models/p.yaml":       newConfigFilesProviderYAML("p", "old"),
				"mcp_servers/fs.yaml": "id: fs\ntype: stdio\ncommand: uvx\n",
				"hooks/go.yaml":       "hooks:\n  Stop:\n    - hooks:\n        - type: command\n          command: echo stop\n",
			},
			act: func(t *testing.T, configDirectory string, files *config.ConfigFiles) configFilesResult {
				if err := os.WriteFile(filepath.Join(configDirectory, "models", "p.yaml"), []byte(newConfigFilesProviderYAML("p", "edited")), 0600); err != nil {
					t.Fatal(err)
				}
				return newResult(files.Reload(ctx))
			},
			check: func(t *testing.T, _ string, _ *config.ConfigFiles, result configFilesResult) {
				if len(result.models) != 1 || result.models[0].Ref.String() != "p/edited" {
					t.Fatalf("returned models = %+v, want p/edited", result.models)
				}
				if len(result.mcpServers) != 1 || result.mcpServers[0].ID != "fs" {
					t.Fatalf("returned MCP servers = %+v, want fs", result.mcpServers)
				}
				if len(result.hooks) != 1 || result.hooks[0].Key != "go:Stop:0" {
					t.Fatalf("returned hooks = %+v, want go:Stop:0", result.hooks)
				}
			},
		},
		{
			// 启用的插件拆进整份定义：它的 skill 带 "demo:" 前缀，hook 和 MCP server 挂着它。
			name:  "reload_returns_enabled_plugin_definitions",
			files: mergeFiles(map[string]string{"config.yaml": "{}\n", "models/p.yaml": newConfigFilesProviderYAML("p", "m")}, getDemoPluginFiles("plugins/demo")),
			act: func(t *testing.T, _ string, files *config.ConfigFiles) configFilesResult {
				return newResult(files.Reload(ctx))
			},
			check: func(t *testing.T, configDirectory string, _ *config.ConfigFiles, result configFilesResult) {
				if !slices.ContainsFunc(result.skills, func(loaded *skill.Skill) bool {
					return loaded.Name == "demo:greet" && loaded.Path == filepath.Join(configDirectory, "plugins", "demo", "skills", "greet", "SKILL.md")
				}) {
					t.Fatalf("returned skills = %+v, want demo:greet", result.skills)
				}
				if len(result.hooks) != 1 || result.hooks[0].Plugin == nil || result.hooks[0].Plugin.Name != "demo" {
					t.Fatalf("returned hooks = %+v, want the demo plugin hook", result.hooks)
				}
				if len(result.mcpServers) != 1 || result.mcpServers[0].ID != "demo-docs" || result.mcpServers[0].Plugin == nil {
					t.Fatalf("returned MCP servers = %+v, want demo-docs from the plugin", result.mcpServers)
				}
			},
		},
		{
			name:  "reload_returns_new_executor",
			files: map[string]string{"config.yaml": "{}\n", "models/p.yaml": newConfigFilesProviderYAML("p", "m")},
			act: func(t *testing.T, configDirectory string, files *config.ConfigFiles) configFilesResult {
				writeDefinitionFiles(t, configDirectory, map[string]string{"executors/extra.yaml": "id: extra\nkind: virtual\ndescription: 额外环境\n"})
				return newResult(files.Reload(ctx))
			},
			check: func(t *testing.T, _ string, _ *config.ConfigFiles, result configFilesResult) {
				var ids []string
				for _, listed := range result.executors {
					ids = append(ids, listed.GetDefinition().ID)
				}
				if want := []string{defs.LocalExecutorID, config.BuiltinExecutorID, defs.MemoryExecutorID, "extra"}; !slices.Equal(ids, want) {
					t.Fatalf("returned executor ids = %v, want %v", ids, want)
				}
			},
		},
		{
			name: "reload_reports_broken_files_without_touching_them",
			files: map[string]string{
				"config.yaml":   "{}\n",
				"models/p.yaml": "id: p\nbase_url: not-a-url\nmodels:\n  m: {}\n",
			},
			act: func(t *testing.T, _ string, files *config.ConfigFiles) configFilesResult {
				return newResult(files.Reload(ctx))
			},
			wantLoadErr: "invalid provider base_url",
			wantFiles: map[string]string{
				"config.yaml":   "{}\n",
				"models/p.yaml": "id: p\nbase_url: not-a-url\nmodels:\n  m: {}\n",
			},
		},
	}
	for _, id := range []string{"../x", "../../x", "a/b", "", ".."} {
		files := map[string]string{
			"config.yaml":   "{}\n",
			"models/q.yaml": newConfigFilesProviderYAML("q", "m"),
		}
		tests = append(tests,
			configFilesCase{
				name:  "set_provider_rejects_id_" + id,
				files: files,
				act: func(t *testing.T, _ string, configFiles *config.ConfigFiles) configFilesResult {
					return newResult(configFiles.SetProvider(ctx, newConfigFilesProvider(id), defs.ModelRef{}))
				},
				wantErr:   "invalid provider id",
				wantFiles: files,
			},
			configFilesCase{
				name:  "delete_provider_rejects_id_" + id,
				files: files,
				act: func(t *testing.T, _ string, configFiles *config.ConfigFiles) configFilesResult {
					return newResult(configFiles.DeleteProvider(ctx, id))
				},
				wantErr:   "invalid provider id",
				wantFiles: files,
			},
		)
	}
	// 2. 逐个执行操作，再核对错误、文件内容、主目录外没有新文件，以及用例自己的检查。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 1. 清除 OPENVAALA_CONFIG，再按样例写入配置文件和符号链接。
			t.Setenv("OPENVAALA_CONFIG", "")
			os.Unsetenv("OPENVAALA_CONFIG")
			root := t.TempDir()
			home := filepath.Join(root, "home")
			configDirectory, opt := home, &config.LoadConfigOpt{
				Home: home,
				Path: filepath.Join(home, "config.yaml"),
			}
			if tt.useDefaultConfigPath {
				configDirectory, opt = filepath.Join(home, ".config", "openvaala"), &config.LoadConfigOpt{Home: home}
			}
			for name, content := range tt.files {
				path := filepath.Join(configDirectory, filepath.FromSlash(name))
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			for name, content := range tt.linkedFiles {
				target := getConfigFilesLinkTarget(configDirectory, name)
				link := filepath.Join(configDirectory, filepath.FromSlash(name))
				for _, directory := range []string{filepath.Dir(target), filepath.Dir(link)} {
					if err := os.MkdirAll(directory, 0700); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(target, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
			}
			files := config.NewConfigFiles(opt)

			result := tt.act(t, configDirectory, files)

			switch {
			case tt.wantErr != "":
				if result.err == nil || result.err.Error() != tt.wantErr {
					t.Fatalf("error = %v, want %q", result.err, tt.wantErr)
				}
			case tt.wantLoadErr != "":
				if result.err == nil || !strings.Contains(result.err.Error(), tt.wantLoadErr) {
					t.Fatalf("error = %v, want it to contain %q", result.err, tt.wantLoadErr)
				}
			case result.err != nil:
				t.Fatalf("unexpected error: %v", result.err)
			}
			if tt.wantFiles != nil {
				if got := readConfigFilesTree(t, configDirectory); !reflect.DeepEqual(got, tt.wantFiles) {
					t.Fatalf("files = %q, want %q", got, tt.wantFiles)
				}
			}
			wantRootEntries := []string{"home"}
			if len(tt.linkedFiles) > 0 {
				wantRootEntries = []string{"dotfiles", "home"}
			}
			entries, err := os.ReadDir(root)
			var rootEntries []string
			for _, entry := range entries {
				rootEntries = append(rootEntries, entry.Name())
			}
			if err != nil || !slices.Equal(rootEntries, wantRootEntries) {
				t.Fatalf("files were created outside the home directory: %v %v", rootEntries, err)
			}
			if tt.check != nil {
				tt.check(t, configDirectory, files, result)
			}
		})
	}
}

// TestConfigFilesModelDefaults 验证 Provider 查询返回加载配置的模型默认值和原始推理强度。
func TestConfigFilesModelDefaults(t *testing.T) {
	// 1. 准备内置默认、文件覆盖、空输入能力和配置加载失败的样例。
	builtinDefaults := defs.ModelDefaults{
		ContextWindow:     256000,
		MaxOutputTokens:   32000,
		Thinking:          true,
		ReasoningEfforts:  []string{},
		MergeMessageRoles: []string{},
		Capabilities: defs.ProviderModelCapabilities{
			Input: defs.OptionalStringList{"text"},
		},
	}
	tests := []struct {
		name       string
		configYAML string
		modelYAML  string
		want       defs.ModelDefaults
		wantErr    string
	}{
		{
			name:       "builtin_defaults_without_models",
			configYAML: "{}\n",
			want:       builtinDefaults,
		},
		{
			name:       "builtin_defaults_with_model",
			configYAML: "{}\n",
			modelYAML:  "  m: {}\n",
			want:       builtinDefaults,
		},
		{
			name: "configured_defaults_keep_effort_outside_global_list",
			configYAML: "models:\n  defaults:\n    context_window: 180000\n    max_output_tokens: 12000\n" +
				"    thinking: false\n    reasoning_efforts: [low]\n    default_reasoning_effort: custom\n" +
				"    merge_message_roles: [user, assistant]\n    capabilities:\n      input: [text, image]\n",
			modelYAML: "  m:\n    reasoning_efforts: [low, custom]\n",
			want: defs.ModelDefaults{
				ContextWindow:          180000,
				MaxOutputTokens:        12000,
				Thinking:               false,
				ReasoningEfforts:       []string{"low"},
				DefaultReasoningEffort: "custom",
				MergeMessageRoles:      []string{"user", "assistant"},
				Capabilities: defs.ProviderModelCapabilities{
					Input: defs.OptionalStringList{"text", "image"},
				},
			},
		},
		{
			name:       "empty_capabilities_use_text",
			configYAML: "models:\n  defaults:\n    capabilities:\n      input: []\n",
			modelYAML:  "  m: {}\n",
			want:       builtinDefaults,
		},
		{
			name:       "invalid_defaults_return_loading_error",
			configYAML: "models:\n  defaults:\n    context_window: -1\n",
			wantErr:    "models.defaults.context_window and max_output_tokens must be positive",
		},
		{
			name:       "invalid_yaml_returns_loading_error",
			configYAML: "models: [\n",
			wantErr:    "invalid YAML",
		},
	}
	// 2. 从实际配置文件查询并比较全部默认值，随后核对模型是否继承了原始全局强度。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "config.yaml")
			if err := os.WriteFile(path, []byte(tt.configYAML), 0600); err != nil {
				t.Fatal(err)
			}
			if tt.modelYAML != "" {
				if err := os.Mkdir(filepath.Join(directory, "models"), 0700); err != nil {
					t.Fatal(err)
				}
				providerYAML := "id: p\nbase_url: https://gw.example/v1\napi_key: sk-p\nmodels:\n" + tt.modelYAML
				if err := os.WriteFile(filepath.Join(directory, "models", "p.yaml"), []byte(providerYAML), 0600); err != nil {
					t.Fatal(err)
				}
			}
			opt := &config.LoadConfigOpt{
				Home: directory,
				Path: path,
			}
			files := config.NewConfigFiles(opt)
			providers, defaultModel, defaults, err := files.ListProviders(t.Context())
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || providers != nil || defaultModel != (defs.ModelRef{}) || !reflect.DeepEqual(defaults, defs.ModelDefaults{}) {
					t.Fatalf("ListProviders() = %+v, %+v, %+v, %v; want loading error %q", providers, defaultModel, defaults, err, tt.wantErr)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(defaults, tt.want) {
				t.Fatalf("ListProviders() defaults = %+v, %v; want %+v", defaults, err, tt.want)
			}
			if tt.modelYAML == "" {
				if len(providers) != 0 || defaultModel != (defs.ModelRef{}) {
					t.Fatalf("ListProviders() = %+v, %v; want no models", providers, defaultModel)
				}
				return
			}
			if len(providers) != 1 || defaultModel.String() != "p/m" {
				t.Fatalf("ListProviders() = %+v, %v; want p/m", providers, defaultModel)
			}
			if defaults.DefaultReasoningEffort == "custom" {
				definitions, err := files.Reload(t.Context())
				if err != nil || len(definitions.Models) != 1 || definitions.Models[0].DefaultReasoningEffort != "custom" {
					t.Fatalf("Reload() = %+v, %v; want model effort custom", definitions, err)
				}
			}
		})
	}
}

// TestConfigFilesProviderModelRoundTrip 验证整份 Provider 写入保留未设置、false、空列表与协议地址，并沿用加载校验。
func TestConfigFilesProviderModelRoundTrip(t *testing.T) {
	// 1. 准备继承模型、显式覆盖和无效协议地址的样例。
	thinking := false
	tests := []struct {
		name      string
		model     *defs.ProviderModelConfig
		endpoints map[string]string
		wantErr   string
	}{
		{
			name: "nil_model_inherits_defaults",
		},
		{
			name:  "unset_fields_inherit_defaults",
			model: &defs.ProviderModelConfig{},
		},
		{
			name: "false_and_empty_lists_override_defaults",
			model: &defs.ProviderModelConfig{
				Protocol:          "claude",
				Thinking:          &thinking,
				ReasoningEfforts:  defs.OptionalStringList{},
				MergeMessageRoles: defs.OptionalStringList{},
				Capabilities: defs.ProviderModelCapabilities{
					Input: defs.OptionalStringList{},
				},
			},
			endpoints: map[string]string{
				"claude":      "https://claude.example/custom/messages",
				"openai_chat": "https://chat.example/custom/completions",
			},
		},
		{
			name:      "endpoint_must_be_http_or_https",
			endpoints: map[string]string{"claude": "ftp://claude.example/messages"},
			wantErr:   "endpoints",
		},
		{
			name:      "endpoint_must_have_host",
			endpoints: map[string]string{"claude": "https:///messages"},
			wantErr:   "endpoints",
		},
		{
			name:      "endpoint_protocol_must_be_known",
			endpoints: map[string]string{"unknown": "https://claude.example/messages"},
			wantErr:   "endpoints",
		},
	}
	// 2. 经 SetProvider 写入整份 YAML，再经 ListProviders 读取原始字段与真实默认值。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "config.yaml")
			content := "models:\n  defaults:\n    thinking: true\n    reasoning_efforts: [low, high]\n    default_reasoning_effort: high\n    merge_message_roles: [user]\n    capabilities:\n      input: [text, image]\n"
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			files := config.NewConfigFiles(&config.LoadConfigOpt{
				Home: directory,
				Path: path,
			})
			provider := defs.ProviderConfig{
				ID:        "p",
				Protocol:  "openai_chat",
				BaseURL:   "https://gw.example/v1",
				APIKey:    "sk-p",
				Endpoints: tt.endpoints,
				Models:    map[string]*defs.ProviderModelConfig{"m": tt.model},
			}
			definitions, err := files.SetProvider(t.Context(), provider, defs.ModelRef{
				ProviderID: "p",
				ModelID:    "m",
			})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("SetProvider() error = %v; want %q", err, tt.wantErr)
				}
				if _, err := os.Stat(filepath.Join(directory, "models", "p.yaml")); !os.IsNotExist(err) {
					t.Fatalf("invalid Provider file was not removed: %v", err)
				}
				if got, err := os.ReadFile(path); err != nil || string(got) != content {
					t.Fatalf("config after rejected Provider = %q, %v; want unchanged", got, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			providers, defaultModel, defaults, err := files.ListProviders(t.Context())
			if err != nil || !reflect.DeepEqual(providers, []defs.ProviderConfig{provider}) || defaultModel.String() != "p/m" || !defaults.Thinking || !slices.Equal(defaults.Capabilities.Input, []string{"text", "image"}) {
				t.Fatalf("ListProviders() = %+v, %v, %+v, %v; want original Provider and defaults", providers, defaultModel, defaults, err)
			}
			model := definitions.Models[0]
			if tt.model != nil && tt.model.Thinking != nil {
				if model.ThinkingEnabled || len(model.ReasoningEfforts) != 0 || len(model.MergeMessageRoles) != 0 || !slices.Equal(model.Capabilities.Input, []defs.ModelInput{defs.ModelInputText}) || model.Target.Protocol != "claude" || model.Target.BaseURL != "https://claude.example" || model.Target.EndpointPath != "/custom/messages" {
					t.Fatalf("explicit overrides = %+v, target %+v", model, model.Target)
				}
			} else if !model.ThinkingEnabled || !slices.Equal(model.ReasoningEfforts, []string{"low", "high"}) || len(model.MergeMessageRoles) != 1 || !slices.Equal(model.Capabilities.Input, []defs.ModelInput{defs.ModelInputText, defs.ModelInputImage}) {
				t.Fatalf("inherited defaults = %+v", model)
			}
		})
	}
}
