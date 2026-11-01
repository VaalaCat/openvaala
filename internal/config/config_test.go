package config_test

import (
	"cmp"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/internal/config"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
	"github.com/VaalaCat/openvaala/pkg/prompt"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"
)

// TestLoadConfigPrecedence 验证 flag 覆盖环境变量、环境变量覆盖文件，相对目录按配置文件所在目录展开。
func TestLoadConfigPrecedence(t *testing.T) {
	// 1. 准备文件、环境变量和 flag 的组合。
	tests := []struct {
		name          string
		file          string
		env           map[string]string
		flags         []string
		wantAgentName string
	}{
		{
			name: "flags_override_environment_and_file",
			file: `run:
  agent_name: file-agent
`,
			env: map[string]string{
				"OPENVAALA_RUN_AGENT_NAME": "env-agent",
			},
			flags:         []string{"--agent=flag-agent"},
			wantAgentName: "flag-agent",
		},
		{
			name: "environment_overrides_file",
			file: `run:
  agent_name: file-agent
`,
			env: map[string]string{
				"OPENVAALA_RUN_AGENT_NAME": "env-agent",
			},
			wantAgentName: "env-agent",
		},
		{
			name:          "file_value",
			file:          "run:\n  agent_name: file-agent\n",
			wantAgentName: "file-agent",
		},
	}
	// 2. 加载配置，比较最终的 run.agent_name 和 agents 目录。
	for _, tt := range tests {
		t.Run(tt.name,
			func(t *testing.T) {
				directory := t.TempDir()
				path := filepath.Join(directory, "config.yaml")
				if err := os.WriteFile(path, []byte(tt.file), 0600); err != nil {
					t.Fatal(err)
				}
				for key, value := range tt.env {
					t.Setenv(key, value)
				}
				flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
				flags.String("agent", "", "")
				if err := flags.Parse(tt.flags); err != nil {
					t.Fatal(err)
				}
				loaded, err := config.LoadConfig(&config.LoadConfigOpt{
					Home:  directory,
					Path:  path,
					Flags: flags,
				})
				if err != nil {
					t.Fatal(err)
				}
				if loaded.Run.AgentName != tt.wantAgentName {
					t.Fatalf("incorrect precedence: %+v", loaded)
				}
				if loaded.Agents.Directory != filepath.Join(directory, "agents") {
					t.Fatal("relative directory was not resolved beside config")
				}
			})
	}
}

// TestLoadConfigRejectsInvalidInput 验证非法的配置文件加载失败，错误里不带文件里的值。
func TestLoadConfigRejectsInvalidInput(t *testing.T) {
	// 1. 准备未知字段、重复键、类型错误、多文档、空值和 YAML 合并等样例。
	tests := []struct {
		name    string
		payload string
	}{
		{
			name:    "unknown_field",
			payload: "unknown: secret-value\n",
		},
		{
			name:    "unknown_nested_field",
			payload: "run:\n  unknown: {}\n",
		},
		{
			name:    "duplicate_field",
			payload: "run:\n  agent_name: a\n  agent_name: b\n",
		},
		{
			name:    "numeric_directory",
			payload: "agents:\n  directory: 123\n",
		},
		{
			name:    "multiple_documents",
			payload: "{}\n---\n{}\n",
		},
		{
			name:    "empty_agent_name",
			payload: "run:\n  agent_name: ''\n",
		},
		{
			name:    "yaml_merge",
			payload: "defaults: &defaults {}\n<<: *defaults\n",
		},
		{
			name:    "empty_file",
			payload: "",
		},
	}
	// 2. 逐个加载，要求报错且错误里没有 secret-value。
	for _, tt := range tests {
		t.Run(tt.name,
			func(t *testing.T) {
				directory := t.TempDir()
				path := filepath.Join(directory, "config.yaml")
				if err := os.WriteFile(path, []byte(tt.payload), 0600); err != nil {
					t.Fatal(err)
				}
				_, err := config.LoadConfig(&config.LoadConfigOpt{
					Home: directory,
					Path: path,
				})
				if err == nil || strings.Contains(err.Error(), "secret-value") {
					t.Fatalf("expected safe validation error, got %v", err)
				}
			})
	}
}

// TestLoadConfigPathAndIsolation 验证配置文件的选择：OPENVAALA_CONFIG 生效，显式路径优先于它，显式路径不存在时报错，前一次加载不影响后一次。
func TestLoadConfigPathAndIsolation(t *testing.T) {
	// 1. 在同一临时目录写入 first.yaml、second.yaml，用 OPENVAALA_CONFIG 指向 first.yaml。
	directory := t.TempDir()
	for _, name := range []string{"first.yaml", "second.yaml"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("run:\n  agent_name: "+name+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("OPENVAALA_CONFIG", filepath.Join(directory, "first.yaml"))
	tests := []struct {
		name          string
		path          string
		wantAgentName string
		wantErr       bool
	}{
		{
			name:          "environment_path",
			wantAgentName: "first.yaml",
		},
		{
			name:          "explicit_path_overrides_environment",
			path:          "~/second.yaml",
			wantAgentName: "second.yaml",
		},
		{
			name:    "explicit_missing_file",
			path:    filepath.Join(directory, "missing.yaml"),
			wantErr: true,
		},
		{
			name:          "previous_load_does_not_change_selection",
			wantAgentName: "first.yaml",
		},
	}
	// 2. 按顺序逐个加载，比较选中的配置文件或错误。
	for _, tt := range tests {
		t.Run(tt.name,
			func(t *testing.T) {
				loaded, err := config.LoadConfig(&config.LoadConfigOpt{
					Home: directory,
					Path: tt.path,
				})
				if (err != nil) != tt.wantErr {
					t.Fatalf("LoadConfig() error = %v, wantErr %v", err, tt.wantErr)
				}
				if tt.wantErr {
					return
				}
				if loaded.Run.AgentName != tt.wantAgentName {
					t.Fatalf("agent name = %q, want %q", loaded.Run.AgentName, tt.wantAgentName)
				}
			})
	}
}

// TestConfigDefinitionsSnapshot 验证加载后的定义不受源文件删除和调用方修改影响，序列化 Config 时不带 api_key。
func TestConfigDefinitionsSnapshot(t *testing.T) {
	// 1. 准备 default Agent 期望的指令和模型。
	tests := []struct {
		name             string
		agentName        string
		wantInstructions string
		wantModel        string
	}{
		{
			name:             "loaded_definitions_survive_source_removal",
			agentName:        "default",
			wantInstructions: "Be concise.",
			wantModel:        "MyModel",
		},
	}
	// 2. 加载后删掉源文件，查询定义；修改返回值后再查一次，核对保存的定义不变。
	for _, tt := range tests {
		t.Run(tt.name,
			func(t *testing.T) {
				directory := writeDefinitionConfig(t)
				cfg, err := config.LoadConfig(&config.LoadConfigOpt{
					Home: directory,
					Path: filepath.Join(directory, "config.yaml"),
				})
				if err != nil {
					t.Fatal(err)
				}
				// 1. 删除源文件并改掉目录设置，核对查到的仍是加载时的配置。
				if err := os.RemoveAll(filepath.Join(directory, "agents")); err != nil {
					t.Fatal(err)
				}
				if err := os.RemoveAll(filepath.Join(directory, "models")); err != nil {
					t.Fatal(err)
				}
				cfg.Agents.Directory = filepath.Join(directory, "absent-agents")
				cfg.Models.Directory = filepath.Join(directory, "absent-models")
				definition, err := cfg.GetAgent(tt.agentName)
				if err != nil {
					t.Fatal(err)
				}
				ref := defs.ModelRef{
					ProviderID: definition.Model.Provider,
					ModelID:    definition.Model.Model,
				}
				model := findModel(cfg, ref)
				if definition.Instructions != tt.wantInstructions || model == nil || model.Target.Model != tt.wantModel ||
					model.Target.Protocol != llmkit.ProtocolOpenAIResponses || model.Target.APIKey != "secret-value" {
					t.Fatalf("configuration snapshot lost fields")
				}

				definition.Model.Model = "modified"
				definition.Instructions = "modified"
				definition, err = cfg.GetAgent(tt.agentName)
				if err != nil {
					t.Fatal(err)
				}
				if definition.Model.Model != tt.wantModel || definition.Instructions != tt.wantInstructions {
					t.Fatal("caller changed stored Agent configuration")
				}
				model.Target.Model = "modified"
				if again := findModel(cfg, ref); again == nil || again.Target.Model != tt.wantModel {
					t.Fatal("caller changed stored model configuration")
				}
				if _, err := cfg.GetAgent("../agent"); err == nil {
					t.Fatal("missing Agent was accepted")
				}
				if findModel(cfg, defs.ModelRef{
					ProviderID: "local",
					ModelID:    "mymodel",
				}) != nil {
					t.Fatal("case-sensitive model lookup accepted a different ID")
				}
				payload, err := yaml.Marshal(cfg)
				if err != nil || strings.Contains(string(payload), "secret-value") {
					t.Fatal("application settings output exposed model credentials")
				}
			})
	}
}

// TestConfigDefinitionsRejectInvalidFilesAtLoad 验证非法的 Provider、Agent、Executor、MCP server 和 hook 文件在加载时报错，错误里不带密钥。
func TestConfigDefinitionsRejectInvalidFilesAtLoad(t *testing.T) {
	// 1. 准备非法 Provider、Agent、Executor、MCP 和 hook 样例。
	const (
		stdioServer = "id: a\ntype: stdio\ncommand: uvx\n"
		httpServer  = "id: a\ntype: http\nurl: https://example.com/mcp\n"
		// preToolUseHook 是 hooks 文件的开头：PreToolUse 下一个没有 matcher 的组，组里第一项 hook 已写好 type，后面接它的其他字段。
		preToolUseHook = "hooks:\n  PreToolUse:\n    - hooks:\n        - type: command\n"
	)
	tests := []struct {
		name    string
		body    string            // models/local.yaml 的内容；为空时保留有效的默认内容
		files   map[string]string // 相对配置目录的其他文件，覆盖同名文件
		wantErr string            // 为空时只要求加载失败
		// wantFile 非空时错误必须带这个文件的相对路径。
		wantFile string
	}{
		{
			name:     "mcp_server_id_mismatches_filename",
			files:    map[string]string{"mcp_servers/a.yaml": "id: b\ntype: stdio\ncommand: uvx\n"},
			wantErr:  "mcp server id must match filename",
			wantFile: "mcp_servers/a.yaml",
		},
		{
			name:     "mcp_server_id_with_double_underscore",
			files:    map[string]string{"mcp_servers/a__b.yaml": "id: a__b\ntype: stdio\ncommand: uvx\n"},
			wantErr:  "mcp server id must contain only letters, digits, - and _, without __",
			wantFile: "mcp_servers/a__b.yaml",
		},
		{
			name:     "mcp_server_id_with_dot",
			files:    map[string]string{"mcp_servers/a.b.yaml": "id: a.b\ntype: stdio\ncommand: uvx\n"},
			wantErr:  "mcp server id must contain only letters, digits, - and _, without __",
			wantFile: "mcp_servers/a.b.yaml",
		},
		{
			name:     "mcp_server_type_grpc",
			files:    map[string]string{"mcp_servers/a.yaml": "id: a\ntype: grpc\ncommand: uvx\n"},
			wantErr:  `mcp server type "grpc" is not supported; use stdio, http or sse`,
			wantFile: "mcp_servers/a.yaml",
		},
		{
			name:     "mcp_server_type_omitted",
			files:    map[string]string{"mcp_servers/a.yaml": "id: a\ncommand: uvx\n"},
			wantErr:  `mcp server type "" is not supported; use stdio, http or sse`,
			wantFile: "mcp_servers/a.yaml",
		},
		{
			name:     "mcp_stdio_without_command",
			files:    map[string]string{"mcp_servers/a.yaml": "id: a\ntype: stdio\n"},
			wantErr:  "stdio mcp server requires command and does not use url or headers",
			wantFile: "mcp_servers/a.yaml",
		},
		{
			name:     "mcp_stdio_with_url",
			files:    map[string]string{"mcp_servers/a.yaml": "id: a\ntype: stdio\ncommand: uvx\nurl: https://example.com/mcp\n"},
			wantErr:  "stdio mcp server requires command and does not use url or headers",
			wantFile: "mcp_servers/a.yaml",
		},
		{
			name:     "mcp_stdio_with_headers",
			files:    map[string]string{"mcp_servers/a.yaml": "id: a\ntype: stdio\ncommand: uvx\nheaders:\n  k: v\n"},
			wantErr:  "stdio mcp server requires command and does not use url or headers",
			wantFile: "mcp_servers/a.yaml",
		},
		{
			name:     "mcp_http_without_url",
			files:    map[string]string{"mcp_servers/a.yaml": "id: a\ntype: http\n"},
			wantErr:  "http and sse mcp servers require an http or https url and do not use command, args or env",
			wantFile: "mcp_servers/a.yaml",
		},
		{
			name:     "mcp_http_with_ftp_url",
			files:    map[string]string{"mcp_servers/a.yaml": "id: a\ntype: http\nurl: ftp://x\n"},
			wantErr:  "http and sse mcp servers require an http or https url and do not use command, args or env",
			wantFile: "mcp_servers/a.yaml",
		},
		{
			name:     "mcp_http_with_command",
			files:    map[string]string{"mcp_servers/a.yaml": "id: a\ntype: http\nurl: https://example.com/mcp\ncommand: uvx\n"},
			wantErr:  "http and sse mcp servers require an http or https url and do not use command, args or env",
			wantFile: "mcp_servers/a.yaml",
		},
		{
			name:     "mcp_sse_with_env",
			files:    map[string]string{"mcp_servers/a.yaml": "id: a\ntype: sse\nurl: https://example.com/sse\nenv:\n  k: v\n"},
			wantErr:  "http and sse mcp servers require an http or https url and do not use command, args or env",
			wantFile: "mcp_servers/a.yaml",
		},
		{
			name:     "mcp_executor_not_configured",
			files:    map[string]string{"mcp_servers/a.yaml": stdioServer + "executors: [nope]\n"},
			wantErr:  `mcp server executor "nope" is not configured`,
			wantFile: "mcp_servers/a.yaml",
		},
		{
			name:     "mcp_executor_builtin_openvaala",
			files:    map[string]string{"mcp_servers/a.yaml": stdioServer + "executors: [openvaala]\n"},
			wantErr:  `mcp server cannot run on executor "openvaala"`,
			wantFile: "mcp_servers/a.yaml",
		},
		{
			name: "mcp_executor_virtual",
			files: map[string]string{
				"executors/mem.yaml": "id: mem\nkind: virtual\ndescription: 内存环境\n",
				"mcp_servers/a.yaml": stdioServer + "executors: [mem]\n",
			},
			wantErr:  `mcp server cannot run on executor "mem"`,
			wantFile: "mcp_servers/a.yaml",
		},
		{
			name:     "mcp_negative_startup_timeout",
			files:    map[string]string{"mcp_servers/a.yaml": httpServer + "startup_timeout_seconds: -1\n"},
			wantErr:  "startup_timeout_seconds and tool_timeout_seconds must not be negative",
			wantFile: "mcp_servers/a.yaml",
		},
		{
			name:     "mcp_negative_tool_timeout",
			files:    map[string]string{"mcp_servers/a.yaml": httpServer + "tool_timeout_seconds: -1\n"},
			wantErr:  "startup_timeout_seconds and tool_timeout_seconds must not be negative",
			wantFile: "mcp_servers/a.yaml",
		},
		{
			name:     "mcp_http_url_credentials_not_echoed_for_wrong_field",
			files:    map[string]string{"mcp_servers/a.yaml": "id: a\ntype: http\nurl: https://user:secret-value@example.com/mcp\ncommand: uvx\n"},
			wantErr:  "http and sse mcp servers require an http or https url and do not use command, args or env",
			wantFile: "mcp_servers/a.yaml",
		},
		{
			name:     "mcp_http_url_credentials_not_echoed_for_unparsable_url",
			files:    map[string]string{"mcp_servers/a.yaml": "id: a\ntype: http\nurl: \"https://user:secret-value@exa mple.com/mcp\"\n"},
			wantErr:  "http and sse mcp servers require an http or https url and do not use command, args or env",
			wantFile: "mcp_servers/a.yaml",
		},
		{
			name:     "mcp_http_url_credentials_not_echoed_for_later_failure",
			files:    map[string]string{"mcp_servers/a.yaml": "id: a\ntype: http\nurl: https://user:secret-value@example.com/mcp\nstartup_timeout_seconds: -1\n"},
			wantErr:  "startup_timeout_seconds and tool_timeout_seconds must not be negative",
			wantFile: "mcp_servers/a.yaml",
		},
		{
			name:     "mcp_server_unknown_field",
			files:    map[string]string{"mcp_servers/a.yaml": stdioServer + "unknown: x\n"},
			wantErr:  "invalid YAML, field type or unknown field",
			wantFile: "mcp_servers/a.yaml",
		},
		{
			name:     "agent_references_unconfigured_mcp_server",
			files:    map[string]string{"agents/default.yaml": "name: default\ninstructions: Be concise.\nmodel:\n  provider: local\n  model: MyModel\nmcp_servers: [ghost]\n"},
			wantErr:  `mcp server "ghost" is not configured`,
			wantFile: "agents/default.yaml",
		},
		{
			name:     "hook_unknown_event",
			files:    map[string]string{"hooks/go.yaml": "hooks:\n  Foo:\n    - hooks:\n        - type: command\n          command: echo\n"},
			wantErr:  `hooks.Foo: unknown hook event "Foo"`,
			wantFile: "hooks/go.yaml",
		},
		{
			name:     "hook_unsupported_type",
			files:    map[string]string{"hooks/go.yaml": preToolUseHook + "          command: echo\n        - type: prompt\n          command: echo\n"},
			wantErr:  `hooks.PreToolUse[0].hooks[1]: unsupported hook type "prompt"`,
			wantFile: "hooks/go.yaml",
		},
		{
			name:     "hook_command_and_script_both_set",
			files:    map[string]string{"hooks/go.yaml": preToolUseHook + "          command: echo\n          script: guard.sh\n", "hooks/guard.sh": "#!/bin/sh\n"},
			wantErr:  "hooks.PreToolUse[0].hooks[0]: exactly one of command and script is required",
			wantFile: "hooks/go.yaml",
		},
		{
			name:     "hook_command_and_script_both_missing",
			files:    map[string]string{"hooks/go.yaml": preToolUseHook},
			wantErr:  "hooks.PreToolUse[0].hooks[0]: exactly one of command and script is required",
			wantFile: "hooks/go.yaml",
		},
		{
			name:     "hook_args_with_command",
			files:    map[string]string{"hooks/go.yaml": preToolUseHook + "          command: echo\n          args: [--strict]\n"},
			wantErr:  "hooks.PreToolUse[0].hooks[0]: args requires script",
			wantFile: "hooks/go.yaml",
		},
		{
			name:     "hook_script_not_found",
			files:    map[string]string{"hooks/go.yaml": preToolUseHook + "          script: guard.sh\n"},
			wantErr:  "/hooks/guard.sh: no such file or directory",
			wantFile: "hooks/go.yaml",
		},
		{
			name:     "hook_invalid_matcher",
			files:    map[string]string{"hooks/go.yaml": "hooks:\n  PreToolUse:\n    - matcher: \"(\"\n      hooks:\n        - type: command\n          command: echo\n"},
			wantErr:  `hooks.PreToolUse[0]: invalid matcher "(": error parsing regexp: missing closing )`,
			wantFile: "hooks/go.yaml",
		},
		{
			name:     "hook_on_failure_block_on_stop",
			files:    map[string]string{"hooks/go.yaml": "hooks:\n  Stop:\n    - hooks:\n        - type: command\n          command: echo\n          on_failure: block\n"},
			wantErr:  "hooks.Stop[0].hooks[0]: on_failure is only supported for PreToolUse and UserPromptSubmit",
			wantFile: "hooks/go.yaml",
		},
		{
			name:     "hook_on_failure_unknown_value",
			files:    map[string]string{"hooks/go.yaml": preToolUseHook + "          command: echo\n          on_failure: maybe\n"},
			wantErr:  "hooks.PreToolUse[0].hooks[0]: on_failure must be allow or block",
			wantFile: "hooks/go.yaml",
		},
		{
			name:     "hook_timeout_zero",
			files:    map[string]string{"hooks/go.yaml": preToolUseHook + "          command: echo\n          timeout: 0\n"},
			wantErr:  "hooks.PreToolUse[0].hooks[0]: timeout must be positive",
			wantFile: "hooks/go.yaml",
		},
		{
			name:     "hook_timeout_negative",
			files:    map[string]string{"hooks/go.yaml": preToolUseHook + "          command: echo\n          timeout: -5\n"},
			wantErr:  "hooks.PreToolUse[0].hooks[0]: timeout must be positive",
			wantFile: "hooks/go.yaml",
		},
		{
			name:     "hook_unknown_field",
			files:    map[string]string{"hooks/go.yaml": preToolUseHook + "          command: echo\n          unknown: x\n"},
			wantErr:  "invalid YAML, field type or unknown field",
			wantFile: "hooks/go.yaml",
		},
		{
			name: "mismatched_id",
			body: "id: wrong\n",
		},
		{
			name: "bad_url",
			body: "id: local\nbase_url: secret-value\nmodels:\n  MyModel: {}\n",
		},
		{
			name: "bad_key_type",
			body: "id: local\nbase_url: https://example.com\napi_key: [secret-value]\nmodels:\n  MyModel: {}\n",
		},
		{
			name: "unknown_field",
			body: "id: local\nunknown: secret-value\n",
		},
		{
			name: "multiple_documents",
			body: "{}\n---\n{}\n",
		},
		{
			name: "unused_bad_model",
			body: "id: local\nbase_url: https://example.com\nmodels:\n  MyModel: {}\n  unused:\n    upstream_model: '  '\n",
		},
		{
			name:    "endpoints_unknown_protocol",
			body:    "id: local\nbase_url: https://example.com/v1\napi_key: secret-value\nendpoints:\n  gemini: https://other.example/v1/generate\nmodels:\n  MyModel: {}\n",
			wantErr: "invalid provider endpoints",
		},
		{
			name:    "endpoints_non_http_scheme",
			body:    "id: local\nbase_url: https://example.com/v1\napi_key: secret-value\nendpoints:\n  claude: ftp://x\nmodels:\n  MyModel: {}\n",
			wantErr: "invalid provider endpoints",
		},
		{
			name:    "endpoints_with_query",
			body:    "id: local\nbase_url: https://example.com/v1\napi_key: secret-value\nendpoints:\n  claude: https://other.example/v1/messages?a=1\nmodels:\n  MyModel: {}\n",
			wantErr: "invalid provider endpoints",
		},
		{
			name:    "endpoints_without_path",
			body:    "id: local\nbase_url: https://example.com/v1\napi_key: secret-value\nendpoints:\n  claude: https://other.example\nmodels:\n  MyModel: {}\n",
			wantErr: "invalid provider endpoints",
		},
		{
			name:    "endpoints_root_path",
			body:    "id: local\nbase_url: https://example.com/v1\napi_key: secret-value\nendpoints:\n  claude: https://other.example/\nmodels:\n  MyModel: {}\n",
			wantErr: "invalid provider endpoints",
		},
		{
			name:    "endpoints_with_empty_fragment",
			body:    "id: local\nbase_url: https://example.com/v1\napi_key: secret-value\nendpoints:\n  claude: https://other.example/v1/messages#\nmodels:\n  MyModel: {}\n",
			wantErr: "invalid provider endpoints",
		},
		{
			name:    "base_url_with_empty_query",
			body:    "id: local\nbase_url: https://example.com/v1?\napi_key: secret-value\nmodels:\n  MyModel: {}\n",
			wantErr: "invalid provider base_url",
		},
		{
			name:    "model_unknown_protocol",
			body:    "id: local\nbase_url: https://example.com/v1\napi_key: secret-value\nmodels:\n  MyModel:\n    protocol: gemini\n",
			wantErr: "unsupported provider protocol",
		},
	}
	// 2. 逐个加载，要求报错、错误含 wantErr 且没有 secret-value；wantFile 非空时还要带这个文件的路径。
	for _, tt := range tests {
		t.Run(tt.name,
			func(t *testing.T) {
				directory := writeDefinitionConfig(t)
				files := tt.files
				if tt.body != "" {
					files = map[string]string{"models/local.yaml": tt.body}
				}
				writeDefinitionFiles(t, directory, files)
				_, err := config.LoadConfig(&config.LoadConfigOpt{
					Home: directory,
					Path: filepath.Join(directory, "config.yaml"),
				})
				if err == nil || strings.Contains(err.Error(), "secret-value") || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("LoadConfig() = %v, want safe error containing %q", err, tt.wantErr)
				}
				if tt.wantFile != "" && !strings.Contains(err.Error(), filepath.Join(directory, tt.wantFile)) {
					t.Fatalf("LoadConfig() = %v, want the path of %s", err, tt.wantFile)
				}
			})
	}
}

// TestConfigProviderEndpoints 验证模型请求地址：base_url 接上协议默认路径，模型协议覆盖 Provider 协议，endpoints 只替换对应协议的地址。
func TestConfigProviderEndpoints(t *testing.T) {
	// 1. 准备协议和地址样例。
	type wantTarget struct {
		protocol     llmkit.Protocol
		baseURL      string
		endpointPath string
	}
	tests := []struct {
		name     string
		provider string
		want     map[string]wantTarget // 模型 id 到期望的 Target
	}{
		{
			name:     "base_url_with_version_prefix",
			provider: "protocol: openai_chat\nbase_url: https://gw.example/v1\nmodels:\n  MyModel: {}\n",
			want: map[string]wantTarget{"MyModel": {
				llmkit.ProtocolOpenAIChat,
				"https://gw.example",
				"/v1/chat/completions",
			}},
		},
		{
			name:     "trailing_slash_does_not_double",
			provider: "protocol: openai_chat\nbase_url: https://gw.example/v1/\nmodels:\n  MyModel: {}\n",
			want: map[string]wantTarget{"MyModel": {
				llmkit.ProtocolOpenAIChat,
				"https://gw.example",
				"/v1/chat/completions",
			}},
		},
		{
			name:     "omitted_protocol_is_openai_responses",
			provider: "base_url: https://gw.example:8443/v1\nmodels:\n  MyModel: {}\n",
			want: map[string]wantTarget{"MyModel": {
				llmkit.ProtocolOpenAIResponses,
				"https://gw.example:8443",
				"/v1/responses",
			}},
		},
		{
			name: "model_protocol_overrides_provider",
			provider: "protocol: openai_chat\nbase_url: https://gw.example/v1\nmodels:\n  MyModel: {}\n" +
				"  claude-model:\n    protocol: claude\n  responses-model:\n    protocol: openai_responses\n  legacy-model:\n    protocol: openai-responses\n",
			want: map[string]wantTarget{
				"MyModel": {
					llmkit.ProtocolOpenAIChat,
					"https://gw.example",
					"/v1/chat/completions",
				},
				"claude-model": {
					llmkit.ProtocolClaude,
					"https://gw.example",
					"/v1/messages",
				},
				"responses-model": {
					llmkit.ProtocolOpenAIResponses,
					"https://gw.example",
					"/v1/responses",
				},
				"legacy-model": {
					llmkit.ProtocolOpenAIResponses,
					"https://gw.example",
					"/v1/responses",
				},
			},
		},
		{
			name: "endpoint_replaces_host_and_path_for_its_protocol_only",
			provider: "protocol: openai_chat\nbase_url: https://gw.example/v1\nendpoints:\n  claude: https://other.example/anthropic/v1/messages\nmodels:\n  MyModel: {}\n" +
				"  claude-model:\n    protocol: claude\n",
			want: map[string]wantTarget{
				"MyModel": {
					llmkit.ProtocolOpenAIChat,
					"https://gw.example",
					"/v1/chat/completions",
				},
				"claude-model": {
					llmkit.ProtocolClaude,
					"https://other.example",
					"/anthropic/v1/messages",
				},
			},
		},
	}
	// 2. 加载 Provider，逐个比较模型的协议、BaseURL 和 EndpointPath。
	for _, tt := range tests {
		t.Run(tt.name,
			func(t *testing.T) {
				directory := writeDefinitionConfig(t)
				body := "id: local\napi_key: secret-value\n" + tt.provider
				if err := os.WriteFile(filepath.Join(directory, "models/local.yaml"), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
				cfg, err := config.LoadConfig(&config.LoadConfigOpt{
					Home: directory,
					Path: filepath.Join(directory, "config.yaml"),
				})
				if err != nil {
					t.Fatal(err)
				}
				for modelID, want := range tt.want {
					model := findModel(cfg, defs.ModelRef{
						ProviderID: "local",
						ModelID:    modelID,
					})
					if model == nil {
						t.Fatalf("model %s not in catalog", modelID)
					}
					got := wantTarget{
						model.Target.Protocol,
						model.Target.BaseURL,
						model.Target.EndpointPath,
					}
					if got != want {
						t.Fatalf("model %s target = %+v, want %+v", modelID, got, want)
					}
				}
			})
	}
}

// TestConfigDefinitionsValidateAllAgentReferences 验证没被 run.agent_name 选中的 Agent 引用了不存在的 Provider 或模型时，加载也报错。
func TestConfigDefinitionsValidateAllAgentReferences(t *testing.T) {
	// 1. 写入带无效引用的 Agent。
	tests := []struct {
		name      string
		reference string
	}{
		{
			name:      "missing_provider",
			reference: "provider: missing\n  model: MyModel",
		},
		{
			name:      "missing_model",
			reference: "provider: local\n  model: missing",
		},
	}
	// 2. 加载配置，要求错误指向 unused.yaml。
	for _, tt := range tests {
		t.Run(tt.name,
			func(t *testing.T) {
				directory := writeDefinitionConfig(t)
				body := "name: unused\ninstructions: Another agent.\nmodel:\n  " + tt.reference + "\n"
				if err := os.WriteFile(filepath.Join(directory, "agents/unused.yaml"), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
				_, err := config.LoadConfig(&config.LoadConfigOpt{
					Home: directory,
					Path: filepath.Join(directory, "config.yaml"),
				})
				if err == nil || !strings.Contains(err.Error(), "unused.yaml") {
					t.Fatalf("expected unselected Agent reference error, got %v", err)
				}
			})
	}
}

// TestConfigDefinitionsLoadAllModelsAndIsolateSnapshots 验证 Agent 没引用的模型也会加载，两次加载的结果互不影响。
func TestConfigDefinitionsLoadAllModelsAndIsolateSnapshots(t *testing.T) {
	// 1. 准备第二次加载前换上的 Provider 文件。
	tests := []struct {
		name            string
		replacementFile string
		wantOldModel    string
		wantNewModel    string
	}{
		{
			name: "reload_preserves_previous_snapshot",
			replacementFile: `id: local
base_url: https://example.com
models:
  MyModel:
    upstream_model: replacement
`,
			wantOldModel: "MyModel",
			wantNewModel: "replacement",
		},
	}
	// 2. 核对没引用的模型也加载了；改写 Provider 文件再加载一次，比较前后两次的结果。
	for _, tt := range tests {
		t.Run(tt.name,
			func(t *testing.T) {
				directory := writeDefinitionConfig(t)
				opt := &config.LoadConfigOpt{
					Home: directory,
					Path: filepath.Join(directory, "config.yaml"),
				}
				first, err := config.LoadConfig(opt)
				if err != nil {
					t.Fatal(err)
				}
				if unused := findModel(first, defs.ModelRef{
					ProviderID: "local",
					ModelID:    "unused",
				}); unused == nil || unused.Target.Model != "another-upstream" {
					t.Fatalf("unreferenced model not loaded: %+v", unused)
				}
				body := tt.replacementFile
				if err := os.WriteFile(filepath.Join(directory, "models/local.yaml"), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
				second, err := config.LoadConfig(opt)
				if err != nil {
					t.Fatal(err)
				}
				ref := defs.ModelRef{
					ProviderID: "local",
					ModelID:    "MyModel",
				}
				oldModel, newModel := findModel(first, ref), findModel(second, ref)
				if oldModel == nil || newModel == nil || oldModel.Target.Model != tt.wantOldModel || newModel.Target.Model != tt.wantNewModel {
					t.Fatal("separate configuration loads shared state")
				}
			})
	}
}

// writeDefinitionConfig 写入定义测试使用的基础配置。
func writeDefinitionConfig(t *testing.T) string {
	t.Helper()
	// 1. 在临时目录写入空的 config.yaml、default Agent 和 local Provider（含 MyModel 与 unused 两个模型）。
	directory := t.TempDir()
	writeDefinitionFiles(t, directory, map[string]string{
		"config.yaml":         "{}\n",
		"agents/default.yaml": "name: default\ninstructions: Be concise.\nmodel:\n  provider: local\n  model: MyModel\n",
		"models/local.yaml":   "id: local\nbase_url: https://example.com\napi_key: secret-value\nmodels:\n  MyModel: {}\n  unused:\n    upstream_model: another-upstream\n",
	})
	return directory
}

// writeDefinitionFiles 按相对路径写入定义测试文件。
func writeDefinitionFiles(t *testing.T, directory string, files map[string]string) {
	t.Helper()
	// 1. 逐文件创建父目录并写入文本内容，覆盖已有同名文件。
	for name, body := range files {
		path := filepath.Join(directory, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

// findModel 在 GetModels 的结果里找指定引用的模型，找不到时返回 nil。
func findModel(cfg *config.Config, ref defs.ModelRef) *defs.ModelDefinition {
	// 1. 按 Provider ID 和模型 ID 匹配。
	models := cfg.GetModels()
	index := slices.IndexFunc(models, func(model *defs.ModelDefinition) bool { return model.Ref == ref })
	if index < 0 {
		return nil
	}
	return models[index]
}

// TestLoadConfigDefaultsDoNotCreateFiles 验证没有配置文件时用默认目录加载，也不创建任何文件。
func TestLoadConfigDefaultsDoNotCreateFiles(t *testing.T) {
	// 1. 准备显式给出主目录和取系统主目录两种情况。
	tests := []struct {
		name          string
		useSystemHome bool
	}{
		{name: "explicit_home"},
		{
			name:          "system_home",
			useSystemHome: true,
		},
	}
	// 2. 在空主目录加载，比较各目录默认值，并核对主目录里没有新文件。
	for _, tt := range tests {
		t.Run(tt.name,
			func(t *testing.T) {
				home := t.TempDir()
				opt := &config.LoadConfigOpt{Home: home}
				if tt.useSystemHome {
					t.Setenv("HOME", home)
					opt.Home = ""
				}
				cfg, err := config.LoadConfig(opt)
				if err != nil {
					t.Fatal(err)
				}
				if cfg.Run.AgentName != "default" ||
					cfg.Plugins.Directory != filepath.Join(home, ".config/openvaala/plugins") ||
					cfg.Agents.Directory != filepath.Join(home, ".config/openvaala/agents") ||
					cfg.Models.Directory != filepath.Join(home, ".config/openvaala/models") ||
					cfg.Skills.Directory != filepath.Join(home, ".config/openvaala/skills") ||
					cfg.Executors.Directory != filepath.Join(home, ".config/openvaala/executors") ||
					cfg.MCPServers.Directory != filepath.Join(home, ".config/openvaala/mcp_servers") ||
					cfg.Hooks.Directory != filepath.Join(home, ".config/openvaala/hooks") {
					t.Fatalf("unexpected defaults: %+v", cfg)
				}
				entries, err := os.ReadDir(home)
				if err != nil || len(entries) != 0 {
					t.Fatalf("loading created files: %v %v", entries, err)
				}
			})
	}
}

// TestLoadConfigRejectsInvalidEnvironment 验证环境变量给出非法值时加载报错，错误指出字段。
func TestLoadConfigRejectsInvalidEnvironment(t *testing.T) {
	// 1. 逐例设置非法环境变量。
	tests := []struct {
		name           string
		key            string
		value          string
		wantErrorField string
	}{
		{
			name:           "empty_directory_does_not_fall_back",
			key:            "OPENVAALA_AGENTS_DIRECTORY",
			wantErrorField: "agents.directory",
		},
	}
	// 2. 设置各环境变量，核对加载错误。
	for _, tt := range tests {
		t.Run(tt.name,
			func(t *testing.T) {
				t.Setenv(tt.key, tt.value)
				_, err := config.LoadConfig(&config.LoadConfigOpt{Home: t.TempDir()})
				if err == nil || strings.Contains(err.Error(), "secret-value") {
					t.Fatalf("expected safe configuration error, got %v", err)
				}
				if !strings.Contains(err.Error(), tt.wantErrorField) {
					t.Fatalf("error = %v, want field %q", err, tt.wantErrorField)
				}
			})
	}
}

// TestLoadConfigExpandsHomePaths 验证配置文件路径和目录设置里的 ~ 会展开成主目录。
func TestLoadConfigExpandsHomePaths(t *testing.T) {
	// 1. 准备绝对路径和 ~ 开头的配置文件路径。
	tests := []struct {
		name       string
		configPath string
	}{
		{
			name:       "absolute_path",
			configPath: "config.yaml",
		},
		{
			name:       "home_relative_path",
			configPath: "~/config.yaml",
		},
	}
	// 2. 加载配置，比较 agents 目录展开后的绝对路径。
	for _, tt := range tests {
		t.Run(tt.name,
			func(t *testing.T) {
				home := t.TempDir()
				t.Setenv("HOME", home)
				path := filepath.Join(home, "config.yaml")
				if err := os.WriteFile(path, []byte("agents:\n  directory: ~/my-agents\n"), 0600); err != nil {
					t.Fatal(err)
				}
				requested := tt.configPath
				if !strings.HasPrefix(requested, "~/") {
					requested = filepath.Join(home, requested)
				}
				cfg, err := config.LoadConfig(&config.LoadConfigOpt{Path: requested})
				if err != nil {
					t.Fatal(err)
				}
				if cfg.Agents.Directory != filepath.Join(home, "my-agents") {
					t.Fatalf("unexpected expanded directory: %s", cfg.Agents.Directory)
				}
			})
	}
}

// TestConfigAgentDefinitions 验证 Agent 的摘要模型、工具和 skill 选择；摘要模型窗口不能比主模型小，改返回值不影响 Config。
func TestConfigAgentDefinitions(t *testing.T) {
	// 1. 逐个写入模型和 Agent 文件，核对加载出的 Agent 定义或错误。
	for _, tt := range []struct {
		name             string
		summaryWindow    string
		compactModel     string
		tools            string
		skills           string
		wantErr          string
		wantCompactModel string
		wantToolNames    []string
		wantSkillNames   []string
	}{
		{
			name:             "compact_model_resolved",
			summaryWindow:    "2000",
			compactModel:     "summary",
			wantCompactModel: "summary",
		},
		{
			name:          "compact_model_omitted",
			summaryWindow: "2000",
		},
		// 摘要模型窗口 0 取默认 256000，大于主模型的 1000。
		{
			name:             "compact_window_default",
			summaryWindow:    "0",
			compactModel:     "summary",
			wantCompactModel: "summary",
		},
		{
			name:          "compact_model_missing",
			summaryWindow: "2000",
			compactModel:  "missing",
			wantErr:       "compact_model",
		},
		{
			name:          "compact_window_too_small",
			summaryWindow: "500",
			compactModel:  "summary",
			wantErr:       "smaller than model context window",
		},
		{
			name:          "tools_selected",
			summaryWindow: "2000",
			tools:         "tools: [read_file, bash]\n",
			wantToolNames: []string{"read_file", "bash"},
		},
		{
			name:           "skills_selected",
			summaryWindow:  "2000",
			skills:         "skills: [code-review]\n",
			wantSkillNames: []string{"code-review"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			directory := writeDefinitionConfig(t)
			models := "id: local\nbase_url: https://example.com\napi_key: secret-value\nmodels:\n" +
				"  MyModel:\n    upstream_model: main-upstream\n    context_window: 1000\n" +
				"  summary:\n    upstream_model: summary-upstream\n    context_window: " + tt.summaryWindow + "\n"
			agentBody := "name: default\ndescription: 通用助手\ninstructions: Be concise.\nmodel:\n  provider: local\n  model: MyModel\n"
			if tt.compactModel != "" {
				agentBody += "compact_model:\n  provider: local\n  model: " + tt.compactModel + "\n"
			}
			agentBody += tt.tools
			agentBody += tt.skills
			for name, body := range map[string]string{"models/local.yaml": models, "agents/default.yaml": agentBody} {
				if err := os.WriteFile(filepath.Join(directory, name), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := config.LoadConfig(&config.LoadConfigOpt{
				Home: directory,
				Path: filepath.Join(directory, "config.yaml"),
			})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || !strings.Contains(err.Error(), "default.yaml") {
					t.Fatalf("LoadConfig = %v, want error containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			definitions := cfg.GetAgentDefinitions()
			if len(definitions) != 1 {
				t.Fatalf("definitions = %d, want 1", len(definitions))
			}
			definition := definitions[0]
			if definition.Name != "default" || definition.Description != "通用助手" || definition.Instructions != "Be concise." ||
				definition.Model != (defs.ModelRef{
					ProviderID: "local",
					ModelID:    "MyModel",
				}) {
				t.Fatalf("definition = %+v", definition)
			}
			if tt.wantCompactModel == "" {
				if definition.CompactModel != (defs.ModelRef{}) {
					t.Fatalf("compact model = %s, want zero", definition.CompactModel)
				}
			} else if definition.CompactModel.ModelID != tt.wantCompactModel {
				t.Fatalf("compact model = %s, want %s", definition.CompactModel, tt.wantCompactModel)
			}
			if !slices.Equal(definition.ToolNames, tt.wantToolNames) {
				t.Fatalf("tool names = %v, want %v", definition.ToolNames, tt.wantToolNames)
			}
			if !slices.Equal(definition.SkillNames, tt.wantSkillNames) {
				t.Fatalf("skill names = %v, want %v", definition.SkillNames, tt.wantSkillNames)
			}
			// 1. 修改返回值后再查一次，核对保存的定义不变。
			if len(definition.ToolNames) > 0 {
				definition.ToolNames[0] = "changed"
			}
			if len(definition.SkillNames) > 0 {
				definition.SkillNames[0] = "changed"
			}
			if again := cfg.GetAgentDefinitions(); !slices.Equal(again[0].ToolNames, tt.wantToolNames) || !slices.Equal(again[0].SkillNames, tt.wantSkillNames) {
				t.Fatal("caller modified agent definition snapshot")
			}
		})
	}
}

// TestConfigDefaultAgent 验证 default Agent 和默认模型：没有 default.yaml 时用内置定义，default.yaml 只覆盖写了的字段；没写 model 的 Agent 用 default_model 或排序后的第一个模型。
func TestConfigDefaultAgent(t *testing.T) {
	// 1. 准备 Provider、Agent 文件、default_model 和环境变量的组合。
	const providerA = "id: a\nbase_url: https://a.example/v1\nmodels:\n  m2: {}\n  m1: {}\n"
	// providerB 的模型 id 排在 a/m1 之前，验证默认模型先按 Provider id 再按模型 id 排序。
	const providerB = "id: b\nbase_url: https://b.example/v1\nmodels:\n  aa: {}\n"
	type wantAgent struct {
		name         string
		description  string
		instructions string
		model        defs.ModelRef
		toolNames    []string
	}
	builtin := func(model defs.ModelRef) wantAgent {
		return wantAgent{
			name:         "default",
			description:  prompt.DefaultAgentDescription,
			instructions: prompt.DefaultAgentInstructions,
			model:        model,
		}
	}
	modelA := func(modelID string) defs.ModelRef {
		return defs.ModelRef{
			ProviderID: "a",
			ModelID:    modelID,
		}
	}
	tests := []struct {
		name        string
		files       map[string]string
		environment map[string]string
		wantErr     string // 为空时要求加载成功
		want        []wantAgent
	}{
		{
			name:  "builtin_default_without_agents_directory",
			files: map[string]string{"models/a.yaml": providerA, "models/b.yaml": providerB},
			want:  []wantAgent{builtin(modelA("m1"))},
		},
		{
			name: "default_file_only_sets_model",
			files: map[string]string{
				"models/a.yaml":       providerA,
				"agents/default.yaml": "model: {provider: a, model: m2}\n",
			},
			want: []wantAgent{builtin(modelA("m2"))},
		},
		{
			name: "default_file_overrides_instructions_and_tools",
			files: map[string]string{
				"models/a.yaml":       providerA,
				"agents/default.yaml": "instructions: 自定义\ntools: [bash]\n",
			},
			want: []wantAgent{{
				name:         "default",
				description:  prompt.DefaultAgentDescription,
				instructions: "自定义",
				model:        modelA("m1"),
				toolNames:    []string{"bash"},
			}},
		},
		{
			name: "default_file_may_repeat_its_name",
			files: map[string]string{
				"models/a.yaml":       providerA,
				"agents/default.yaml": "name: default\ndescription: 自定义说明\n",
			},
			want: []wantAgent{{
				name:         "default",
				description:  "自定义说明",
				instructions: prompt.DefaultAgentInstructions,
				model:        modelA("m1"),
			}},
		},
		{
			name: "default_file_with_other_name_rejected",
			files: map[string]string{
				"models/a.yaml":       providerA,
				"agents/default.yaml": "name: other\n",
			},
			wantErr: "default.yaml: agent name must be valid and match filename",
		},
		{
			name: "default_file_with_blank_instructions_rejected",
			files: map[string]string{
				"models/a.yaml":       providerA,
				"agents/default.yaml": "instructions: '  '\n",
			},
			wantErr: "default.yaml: agent instructions must not be empty",
		},
		{
			name: "default_file_with_unknown_field_rejected",
			files: map[string]string{
				"models/a.yaml":       providerA,
				"agents/default.yaml": "unknown: x\n",
			},
			wantErr: "default.yaml",
		},
		{
			name: "default_model_applies_to_agents_without_model",
			files: map[string]string{
				"config.yaml":         "models:\n  default_model: a/m2\n",
				"models/a.yaml":       providerA,
				"agents/worker.yaml":  "name: worker\ninstructions: 干活\n",
				"agents/default.yaml": "tools: [bash]\n",
			},
			want: []wantAgent{
				{
					name:         "default",
					description:  prompt.DefaultAgentDescription,
					instructions: prompt.DefaultAgentInstructions,
					model:        modelA("m2"),
					toolNames:    []string{"bash"},
				},
				{
					name:         "worker",
					instructions: "干活",
					model:        modelA("m2"),
				},
			},
		},
		{
			name: "default_model_from_environment",
			files: map[string]string{
				"models/a.yaml": providerA,
			},
			environment: map[string]string{"OPENVAALA_MODELS_DEFAULT_MODEL": "a/m2"},
			want:        []wantAgent{builtin(modelA("m2"))},
		},
		{
			name: "agent_model_beats_default_model",
			files: map[string]string{
				"config.yaml":        "models:\n  default_model: a/m2\n",
				"models/a.yaml":      providerA,
				"agents/worker.yaml": "name: worker\ninstructions: 干活\nmodel: {provider: a, model: m1}\n",
			},
			want: []wantAgent{
				builtin(modelA("m2")),
				{
					name:         "worker",
					instructions: "干活",
					model:        modelA("m1"),
				},
			},
		},
		{
			name: "default_model_missing_rejected",
			files: map[string]string{
				"config.yaml":   "models:\n  default_model: a/missing\n",
				"models/a.yaml": providerA,
			},
			wantErr: "models.default_model: model a/missing is not configured",
		},
		{
			name: "default_model_malformed_rejected",
			files: map[string]string{
				"config.yaml":   "models:\n  default_model: a\n",
				"models/a.yaml": providerA,
			},
			wantErr: "models.default_model",
		},
		{
			name:    "default_model_without_providers_rejected",
			files:   map[string]string{"config.yaml": "models:\n  default_model: a/m1\n"},
			wantErr: "models.default_model: model a/m1 is not configured",
		},
		{
			name:  "no_providers_leaves_default_agent_without_model",
			files: map[string]string{},
			want:  []wantAgent{builtin(defs.ModelRef{})},
		},
		{
			name: "no_providers_keeps_default_file_without_model",
			files: map[string]string{
				"agents/default.yaml": "instructions: 自定义\n",
			},
			want: []wantAgent{{
				name:         "default",
				description:  prompt.DefaultAgentDescription,
				instructions: "自定义",
			}},
		},
		{
			name: "agent_model_not_in_catalog_rejected",
			files: map[string]string{
				"models/a.yaml":      providerA,
				"agents/worker.yaml": "name: worker\ninstructions: 干活\nmodel: {provider: a, model: missing}\n",
			},
			wantErr: "worker.yaml: model \"missing\" is not configured in provider \"a\"",
		},
		{
			name: "agent_model_without_providers_rejected",
			files: map[string]string{
				"agents/worker.yaml": "name: worker\ninstructions: 干活\nmodel: {provider: a, model: m1}\n",
			},
			wantErr: "worker.yaml: model \"m1\" is not configured in provider \"a\"",
		},
		{
			name: "compact_model_without_providers_rejected",
			files: map[string]string{
				"agents/worker.yaml": "name: worker\ninstructions: 干活\ncompact_model: {provider: a, model: m1}\n",
			},
			wantErr: "worker.yaml: compact_model: model \"m1\" is not configured in provider \"a\"",
		},
		{
			name: "other_agent_still_requires_instructions",
			files: map[string]string{
				"models/a.yaml":      providerA,
				"agents/worker.yaml": "name: worker\n",
			},
			wantErr: "worker.yaml: agent instructions must not be empty",
		},
	}
	// 2. 加载配置，逐个比较 Agent 的名字、说明、指令、模型和工具，或核对错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			directory := t.TempDir()
			files := maps.Clone(tt.files)
			if _, found := files["config.yaml"]; !found {
				files["config.yaml"] = "{}\n"
			}
			for name, body := range files {
				path := filepath.Join(directory, name)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			for key, value := range tt.environment {
				t.Setenv(key, value)
			}
			cfg, err := config.LoadConfig(&config.LoadConfigOpt{
				Home: directory,
				Path: filepath.Join(directory, "config.yaml"),
			})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("LoadConfig = %v, want error containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			definitions := cfg.GetAgentDefinitions()
			if len(definitions) != len(tt.want) {
				t.Fatalf("agent definitions = %d, want %d", len(definitions), len(tt.want))
			}
			for index, want := range tt.want {
				definition := definitions[index]
				if definition.Name != want.name || definition.Description != want.description || definition.Instructions != want.instructions ||
					definition.Model != want.model || !slices.Equal(definition.ToolNames, want.toolNames) ||
					definition.CompactModel != (defs.ModelRef{}) || len(definition.SkillNames) != 0 {
					t.Fatalf("agent definition = %+v, want %+v", definition, want)
				}
				agentConfig, err := cfg.GetAgent(want.name)
				if err != nil {
					t.Fatal(err)
				}
				if (agentConfig.Model == nil) != (want.model == defs.ModelRef{}) || agentConfig.Instructions != want.instructions {
					t.Fatalf("GetAgent(%q) = %+v, want %+v", want.name, agentConfig, want)
				}
			}
		})
	}
}

// TestConfigExecutors 验证 Executor 文件加载：local、virtual、ssh 的工作目录和私钥路径处理，保留 ID 和非法配置报错。
func TestConfigExecutors(t *testing.T) {
	// 1. 逐个写入 local、ssh 和 virtual 的 Executor 文件，核对加载结果或错误。
	for _, tt := range []struct {
		name          string
		file          string
		body          string // {{workspace}} 替换为已存在的工作区目录
		wantErr       string
		wantWorkspace string // 以 / 开头时为虚拟或远端路径原样比较，否则相对配置目录
		wantKind      defs.ExecutorKind
		wantSSH       *config.SSHExecutorConfig // IdentityFile 相对配置目录
	}{
		{
			name:          "local_executor_loaded",
			file:          "sandbox.yaml",
			body:          "id: sandbox\nkind: local\ndescription: 试验目录\nworkspace_directory: {{workspace}}\n",
			wantWorkspace: "ws",
		},
		{
			name:          "tilde_workspace",
			file:          "sandbox.yaml",
			body:          "id: sandbox\nkind: local\ndescription: 试验目录\nworkspace_directory: ~/ws\n",
			wantWorkspace: "ws",
		},
		{
			name:    "reserved_local",
			file:    "local.yaml",
			body:    "id: local\nkind: local\ndescription: 本机\nworkspace_directory: {{workspace}}\n",
			wantErr: "reserved",
		},
		{
			name:    "reserved_client",
			file:    "client.yaml",
			body:    "id: client\nkind: local\ndescription: 客户端\nworkspace_directory: {{workspace}}\n",
			wantErr: `executor id "client" is reserved`,
		},
		{
			name:    "unsupported_kind",
			file:    "sandbox.yaml",
			body:    "id: sandbox\nkind: docker\ndescription: 容器\nworkspace_directory: {{workspace}}\n",
			wantErr: "not supported",
		},
		{
			name:    "empty_description",
			file:    "sandbox.yaml",
			body:    "id: sandbox\nkind: local\ndescription: \"  \"\nworkspace_directory: {{workspace}}\n",
			wantErr: "description",
		},
		{
			name:    "missing_workspace",
			file:    "sandbox.yaml",
			body:    "id: sandbox\nkind: local\ndescription: 试验目录\nworkspace_directory: {{workspace}}/missing\n",
			wantErr: "existing directory",
		},
		{
			name:          "virtual_default_workspace",
			file:          "sandbox.yaml",
			body:          "id: sandbox\nkind: virtual\ndescription: 试验目录\n",
			wantKind:      "virtual",
			wantWorkspace: "/workspace",
		},
		{
			name:          "virtual_custom_workspace",
			file:          "sandbox.yaml",
			body:          "id: sandbox\nkind: virtual\ndescription: 试验目录\nworkspace_directory: /data\n",
			wantKind:      "virtual",
			wantWorkspace: "/data",
		},
		{
			name:    "virtual_relative_rejected",
			file:    "sandbox.yaml",
			body:    "id: sandbox\nkind: virtual\ndescription: 试验目录\nworkspace_directory: data\n",
			wantErr: "absolute",
		},
		{
			name:          "ssh_executor_loaded",
			file:          "sandbox.yaml",
			body:          "id: sandbox\nkind: ssh\ndescription: 试验目录\nworkspace_directory: /home/me/project\nssh:\n  address: me@devbox:2222\n  identity_file: ~/key\n",
			wantKind:      "ssh",
			wantWorkspace: "/home/me/project",
			wantSSH: &config.SSHExecutorConfig{
				Address:      "me@devbox:2222",
				IdentityFile: "key",
			},
		},
		{
			name:          "ssh_without_identity_file",
			file:          "sandbox.yaml",
			body:          "id: sandbox\nkind: ssh\ndescription: 试验目录\nworkspace_directory: /srv\nssh:\n  address: devbox\n",
			wantKind:      "ssh",
			wantWorkspace: "/srv",
			wantSSH:       &config.SSHExecutorConfig{Address: "devbox"},
		},
		{
			name:          "ssh_relative_identity_file",
			file:          "sandbox.yaml",
			body:          "id: sandbox\nkind: ssh\ndescription: 试验目录\nworkspace_directory: /srv\nssh:\n  address: devbox\n  identity_file: ../key\n",
			wantKind:      "ssh",
			wantWorkspace: "/srv",
			wantSSH: &config.SSHExecutorConfig{
				Address:      "devbox",
				IdentityFile: "key",
			},
		},
		{
			name:    "ssh_missing_address",
			file:    "sandbox.yaml",
			body:    "id: sandbox\nkind: ssh\ndescription: 试验目录\nworkspace_directory: /srv\n",
			wantErr: "ssh.address",
		},
		{
			name:    "ssh_relative_workspace",
			file:    "sandbox.yaml",
			body:    "id: sandbox\nkind: ssh\ndescription: 试验目录\nworkspace_directory: ~/project\nssh:\n  address: devbox\n",
			wantErr: "absolute",
		},
		{
			name:    "ssh_missing_identity_file",
			file:    "sandbox.yaml",
			body:    "id: sandbox\nkind: ssh\ndescription: 试验目录\nworkspace_directory: /srv\nssh:\n  address: devbox\n  identity_file: ~/missing\n",
			wantErr: "identity_file",
		},
		{
			name:    "reserved_openvaala",
			file:    "openvaala.yaml",
			body:    "id: openvaala\nkind: virtual\ndescription: 内置\n",
			wantErr: "reserved",
		},
		{
			name:    "id_mismatch",
			file:    "other.yaml",
			body:    "id: sandbox\nkind: local\ndescription: 试验目录\nworkspace_directory: {{workspace}}\n",
			wantErr: "match filename",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			directory := writeDefinitionConfig(t)
			workspace := filepath.Join(directory, "ws")
			if err := os.MkdirAll(workspace, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, "key"), []byte("private key"), 0600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(directory, "executors", tt.file)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(strings.ReplaceAll(tt.body, "{{workspace}}", workspace)), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.LoadConfig(&config.LoadConfigOpt{
				Home: directory,
				Path: filepath.Join(directory, "config.yaml"),
			})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || !strings.Contains(err.Error(), tt.file) {
					t.Fatalf("LoadConfig error = %v, want %q in %s", err, tt.wantErr, tt.file)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wantWorkspace := tt.wantWorkspace
			if !strings.HasPrefix(wantWorkspace, "/") {
				wantWorkspace = filepath.Join(directory, tt.wantWorkspace)
			}
			wantKind := tt.wantKind
			if wantKind == "" {
				wantKind = defs.ExecutorKindLocal
			}
			executors := cfg.GetExecutors()
			if len(executors) != 1 || executors[0].ID != "sandbox" || executors[0].Kind != wantKind ||
				executors[0].Description != "试验目录" || executors[0].WorkspaceDirectory != wantWorkspace {
				t.Fatalf("executors = %+v", executors)
			}
			if tt.wantSSH != nil {
				wantIdentityFile := ""
				if tt.wantSSH.IdentityFile != "" {
					wantIdentityFile = filepath.Join(directory, tt.wantSSH.IdentityFile)
				}
				if got := executors[0].SSH; got == nil || got.Address != tt.wantSSH.Address || got.IdentityFile != wantIdentityFile {
					t.Fatalf("ssh = %+v", got)
				}
				// 1. 修改返回的 SSH 段后再查一次，核对保存的配置不变。
				executors[0].SSH.Address = "changed"
				if cfg.GetExecutors()[0].SSH.Address != tt.wantSSH.Address {
					t.Fatal("caller modified ssh snapshot")
				}
			}
			// 2. 修改返回值后再查一次，核对保存的配置不变。
			executors[0].Description = "changed"
			if cfg.GetExecutors()[0].Description != "试验目录" {
				t.Fatal("caller modified executor snapshot")
			}
		})
	}
}

// TestConfigRuntimeDefinitions 验证 GetRuntimeDefinitions 组装的 Executor 和 skill：Executor 的顺序和字段，skill 目录的优先级和插件前缀；skills/ 里的坏 skill 报错，插件里的跳过。
func TestConfigRuntimeDefinitions(t *testing.T) {
	// 1. 准备 Executor、skill 和插件文件。
	// newSkillFile 返回带 YAML 文件头的 SKILL.md 内容。
	newSkillFile := func(name string) string {
		return "---\nname: " + name + "\ndescription: Skill " + name + ".\n---\nBody.\n"
	}
	tests := []struct {
		name string
		// files 是相对配置目录的文件，测试里 Home 就是配置目录；config.yaml 不给时用最小配置。
		files map[string]string
		// wantErr 非空时 GetRuntimeDefinitions 必须失败，错误含这段文字。
		wantErr string
		check   func(t *testing.T, home string, definitions *runtime.Definitions)
	}{
		{
			name: "executors_in_order",
			files: map[string]string{
				"config.yaml":            "server:\n  workspace_directory: ~/work\n",
				"executors/ssh-dev.yaml": "id: ssh-dev\nkind: ssh\ndescription: 开发机\nworkspace_directory: /srv/app\nssh:\n  address: me@dev:22\n  identity_file: ~/.ssh/id_ed25519\n",
				"executors/gpu.yaml":     "id: gpu\nkind: virtual\ndescription: 实验环境\n",
				".ssh/id_ed25519":        "private key",
			},
			check: func(t *testing.T, home string, definitions *runtime.Definitions) {
				var ids []string
				for _, executor := range definitions.Executors {
					ids = append(ids, executor.ID)
				}
				if want := []string{defs.LocalExecutorID, config.BuiltinExecutorID, "gpu", "ssh-dev"}; !slices.Equal(ids, want) {
					t.Fatalf("executor ids = %v, want %v", ids, want)
				}
				local, builtin, gpu, sshDev := definitions.Executors[0], definitions.Executors[1], definitions.Executors[2], definitions.Executors[3]
				if local.Kind != defs.ExecutorKindLocal || local.WorkspaceDirectory != filepath.Join(home, "work") || local.Description != prompt.LocalExecutorDescription {
					t.Fatalf("local = %+v, want local kind, the configured server.workspace_directory and the local description", local)
				}
				if builtin.Kind != defs.ExecutorKindVirtual || builtin.WorkspaceDirectory != "/workspace" || builtin.Description != prompt.BuiltinExecutorDescription {
					t.Fatalf("builtin = %+v, want virtual kind rooted at /workspace with the builtin description", builtin)
				}
				if content, found := builtin.Files["/skills/openvaala/SKILL.md"]; !found || len(content) == 0 {
					t.Fatalf("builtin files = %v, want /skills/openvaala/SKILL.md", slices.Sorted(maps.Keys(builtin.Files)))
				}
				if gpu.Kind != defs.ExecutorKindVirtual || gpu.Description != "实验环境" || gpu.WorkspaceDirectory != "/workspace" || gpu.SSH != nil || len(gpu.Files) != 0 {
					t.Fatalf("gpu = %+v, want a virtual executor without ssh settings or preloaded files", gpu)
				}
				wantSSH := &defs.SSHExecutorDefinition{
					Address:      "me@dev:22",
					IdentityFile: filepath.Join(home, ".ssh/id_ed25519"),
				}
				if sshDev.Kind != defs.ExecutorKindSSH || sshDev.WorkspaceDirectory != "/srv/app" || !reflect.DeepEqual(sshDev.SSH, wantSSH) {
					t.Fatalf("ssh-dev = %+v (ssh %+v), want ssh kind in /srv/app with %+v", sshDev, sshDev.SSH, wantSSH)
				}
			},
		},
		{
			name: "local_directory_defaults_to_working_directory",
			check: func(t *testing.T, _ string, definitions *runtime.Definitions) {
				workingDirectory, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				if got := definitions.Executors[0].WorkspaceDirectory; got != workingDirectory {
					t.Fatalf("local directory = %q, want the working directory %q", got, workingDirectory)
				}
			},
		},
		{
			// skills.directory 先于 builtin，同名的 builtin skill 被盖住；插件的 skill 带插件名前缀；整体按名字排序。
			name: "skills_order",
			files: mergeFiles(map[string]string{
				"skills/shared/SKILL.md":    newSkillFile("shared"),
				"skills/openvaala/SKILL.md": newSkillFile("openvaala"),
			}, getPluginFiles("plugins/demo", "name: demo\n", map[string]string{"skills/lint/SKILL.md": newSkillFile("lint")})),
			check: func(t *testing.T, home string, definitions *runtime.Definitions) {
				type skillInfo struct{ name, path, executor string }
				var got []skillInfo
				for _, loaded := range definitions.Skills {
					got = append(got, skillInfo{
						loaded.Name,
						loaded.Path,
						loaded.Executor,
					})
				}
				want := []skillInfo{
					{
						"demo:lint",
						filepath.Join(home, "plugins/demo/skills/lint/SKILL.md"),
						defs.LocalExecutorID,
					},
					{
						"openvaala",
						filepath.Join(home, "skills/openvaala/SKILL.md"),
						defs.LocalExecutorID,
					},
					{
						"shared",
						filepath.Join(home, "skills/shared/SKILL.md"),
						defs.LocalExecutorID,
					},
				}
				if !slices.Equal(got, want) {
					t.Fatalf("skills = %+v, want %+v", got, want)
				}
			},
		},
		{
			name: "builtin_skill_loaded_when_not_shadowed",
			check: func(t *testing.T, _ string, definitions *runtime.Definitions) {
				if len(definitions.Skills) != 1 {
					t.Fatalf("skills = %+v, want only the builtin skill", definitions.Skills)
				}
				if loaded := definitions.Skills[0]; loaded.Name != "openvaala" || loaded.Path != "/skills/openvaala/SKILL.md" || loaded.Executor != config.BuiltinExecutorID {
					t.Fatalf("skill = %+v, want openvaala from the builtin executor", loaded)
				}
			},
		},
		{
			name:    "invalid_skill_in_skills_directory_fails",
			files:   map[string]string{"skills/bad/SKILL.md": "no frontmatter\n"},
			wantErr: "skills/bad/SKILL.md",
		},
		{
			name: "invalid_skill_in_plugin_skipped",
			files: getPluginFiles("plugins/demo", "name: demo\n", map[string]string{
				"skills/lint/SKILL.md": newSkillFile("lint"),
				"skills/bad/SKILL.md":  "no frontmatter\n",
			}),
			check: func(t *testing.T, _ string, definitions *runtime.Definitions) {
				var names []string
				for _, loaded := range definitions.Skills {
					names = append(names, loaded.Name)
				}
				if want := []string{"demo:lint", "openvaala"}; !slices.Equal(names, want) {
					t.Fatalf("skill names = %v, want %v", names, want)
				}
			},
		},
	}
	// 2. 加载配置并组装运行定义，按用例检查结果或错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := writeDefinitionConfig(t)
			writeDefinitionFiles(t, home, tt.files)
			cfg, err := config.LoadConfig(&config.LoadConfigOpt{
				Home: home,
				Path: filepath.Join(home, "config.yaml"),
			})
			if err != nil {
				t.Fatal(err)
			}
			definitions, err := cfg.GetRuntimeDefinitions(t.Context())
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("GetRuntimeDefinitions() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, home, definitions)
		})
	}
}

// TestConfigMCPServers 验证 MCP server 加载：按 ID 排序、跳过 disabled、补默认 Executor 和超时，返回的是副本。
func TestConfigMCPServers(t *testing.T) {
	// 1. 准备 stdio、http、sse 和 disabled 的 MCP server 文件。
	const remoteExecutor = "id: remote\nkind: ssh\ndescription: 远端开发机\nworkspace_directory: /srv\nssh:\n  address: devbox\n"
	tests := []struct {
		name  string
		files map[string]string
		want  []*mcpclient.ServerDefinition
	}{
		{
			name: "enabled_servers_sorted_by_id_with_defaults",
			files: map[string]string{
				"executors/remote.yaml": remoteExecutor,
				"mcp_servers/b.yaml":    "id: b\ntype: stdio\ncommand: uvx\nargs: [serena, start-mcp-server]\nenv:\n  KEY: value\n",
				"mcp_servers/a.yaml":    "id: a\ntype: http\nurl: https://example.com/mcp\ndisabled: true\n",
				"mcp_servers/c.yaml": "id: c\ntype: sse\nurl: https://example.com/sse\nheaders:\n  Authorization: Bearer x\n" +
					"executors: [local, remote]\ntools: [read]\nrequired: true\nstartup_timeout_seconds: 5\ntool_timeout_seconds: 7\n",
			},
			want: []*mcpclient.ServerDefinition{
				{
					ID:             "b",
					Type:           mcpclient.ServerTransportStdio,
					Command:        "uvx",
					Args:           []string{"serena", "start-mcp-server"},
					Env:            map[string]string{"KEY": "value"},
					ExecutorIDs:    []string{"local"},
					StartupTimeout: 30 * time.Second,
					ToolTimeout:    300 * time.Second,
				},
				{
					ID:             "c",
					Type:           mcpclient.ServerTransportSSE,
					URL:            "https://example.com/sse",
					Headers:        map[string]string{"Authorization": "Bearer x"},
					ExecutorIDs:    []string{"local", "remote"},
					ToolNames:      []string{"read"},
					Required:       true,
					StartupTimeout: 5 * time.Second,
					ToolTimeout:    7 * time.Second,
				},
			},
		},
		{
			name: "empty_tools_list_provides_no_tools",
			files: map[string]string{
				"mcp_servers/my-server_1.yaml": "id: my-server_1\ntype: http\nurl: http://127.0.0.1:8080/mcp?mode=full\ntools: []\n",
			},
			want: []*mcpclient.ServerDefinition{
				{
					ID:             "my-server_1",
					Type:           mcpclient.ServerTransportHTTP,
					URL:            "http://127.0.0.1:8080/mcp?mode=full",
					ExecutorIDs:    []string{"local"},
					ToolNames:      []string{},
					StartupTimeout: 30 * time.Second,
					ToolTimeout:    300 * time.Second,
				},
			},
		},
		{
			name: "no_servers_configured",
			want: []*mcpclient.ServerDefinition{},
		},
	}
	// 2. 加载配置，比较 GetMCPServers 的结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			directory := writeDefinitionConfig(t)
			writeDefinitionFiles(t, directory, tt.files)
			cfg, err := config.LoadConfig(&config.LoadConfigOpt{
				Home: directory,
				Path: filepath.Join(directory, "config.yaml"),
			})
			if err != nil {
				t.Fatal(err)
			}
			got := cfg.GetMCPServers()
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("GetMCPServers() = %+v, want %+v", got, tt.want)
			}
			// 1. 修改返回值后再查一次，核对保存的定义不变。
			for _, server := range got {
				for _, list := range [][]string{server.Args, server.ExecutorIDs, server.ToolNames} {
					for index := range list {
						list[index] = "changed"
					}
				}
				for _, values := range []map[string]string{server.Env, server.Headers} {
					for key := range values {
						values[key] = "changed"
					}
				}
			}
			if again := cfg.GetMCPServers(); !reflect.DeepEqual(again, tt.want) {
				t.Fatalf("caller modified MCP server snapshot: %+v", again)
			}
		})
	}
}

// TestConfigHooks 验证 hooks/ 的加载：文件按名字、事件按固定顺序排列，Key 在同一事件里跨 matcher 组编号，DisplayOrder 是整体顺序。
func TestConfigHooks(t *testing.T) {
	// 1. 准备各事件的 hook 文件和期望字段。
	// wantHook 的 scriptPath、sourcePath 相对配置目录（Home），matcher 是配置原文。
	type wantHook struct {
		key                string
		event              defs.HookEventName
		matcher            string
		command            string
		scriptPath         string
		args               []string
		timeout            time.Duration
		isBlockedOnFailure bool
		sourcePath         string
	}
	tests := []struct {
		name string
		// files 是相对配置目录的文件；内容里的 {dir} 换成配置目录的绝对路径。
		files map[string]string
		want  []wantHook
	}{
		{
			name: "files_sorted_by_name_events_in_fixed_order_and_keys_numbered_across_matcher_groups",
			files: map[string]string{
				"hooks/b.yaml": "hooks:\n" +
					"  SubagentStop:\n    - matcher: worker\n      hooks:\n        - type: command\n          command: echo done\n" +
					"  UserPromptSubmit:\n    - hooks:\n        - type: command\n          script: \"{dir}/hooks/abs.sh\"\n          timeout: 600\n          on_failure: block\n",
				"hooks/a.yaml": "hooks:\n" +
					"  Stop:\n    - hooks:\n        - type: command\n          command: ~/bin/check-tests.sh\n" +
					"  PreToolUse:\n" +
					"    - matcher: bash\n      hooks:\n" +
					"        - type: command\n          script: guard.sh\n          args: [--strict, \"a b\"]\n          timeout: 10\n          on_failure: block\n" +
					"        - type: command\n          command: echo second\n" +
					"    - matcher: apply_patch|Write\n      hooks:\n" +
					"        - type: command\n          script: ~/x.sh\n          on_failure: allow\n" +
					"  SessionStart:\n    - matcher: \"*\"\n      hooks:\n        - type: command\n          command: echo start\n",
				"hooks/guard.sh": "#!/bin/sh\n",
				"hooks/abs.sh":   "#!/bin/sh\n",
				"x.sh":           "#!/bin/sh\n",
				// 不是 .yaml 的文件和子目录里的文件不读。
				"hooks/notes.txt":  "not: a hook file\n",
				"hooks/sub/c.yaml": "not: valid\n",
			},
			want: []wantHook{
				{
					key:        "a:SessionStart:0",
					event:      defs.HookEventSessionStart,
					matcher:    "*",
					command:    "echo start",
					timeout:    time.Minute,
					sourcePath: "hooks/a.yaml",
				},
				{
					key:                "a:PreToolUse:0",
					event:              defs.HookEventPreToolUse,
					matcher:            "bash",
					scriptPath:         "hooks/guard.sh",
					args:               []string{"--strict", "a b"},
					timeout:            10 * time.Second,
					isBlockedOnFailure: true,
					sourcePath:         "hooks/a.yaml",
				},
				{
					key:        "a:PreToolUse:1",
					event:      defs.HookEventPreToolUse,
					matcher:    "bash",
					command:    "echo second",
					timeout:    time.Minute,
					sourcePath: "hooks/a.yaml",
				},
				{
					key:        "a:PreToolUse:2",
					event:      defs.HookEventPreToolUse,
					matcher:    "apply_patch|Write",
					scriptPath: "x.sh",
					timeout:    time.Minute,
					sourcePath: "hooks/a.yaml",
				},
				{
					key:        "a:Stop:0",
					event:      defs.HookEventStop,
					command:    "~/bin/check-tests.sh",
					timeout:    time.Minute,
					sourcePath: "hooks/a.yaml",
				},
				{
					key:                "b:UserPromptSubmit:0",
					event:              defs.HookEventUserPromptSubmit,
					scriptPath:         "hooks/abs.sh",
					timeout:            600 * time.Second,
					isBlockedOnFailure: true,
					sourcePath:         "hooks/b.yaml",
				},
				{
					key:        "b:SubagentStop:0",
					event:      defs.HookEventSubagentStop,
					matcher:    "worker",
					command:    "echo done",
					timeout:    time.Minute,
					sourcePath: "hooks/b.yaml",
				},
			},
		},
		{
			name: "every_event_in_fixed_order_regardless_of_written_order",
			files: map[string]string{
				"hooks/all.yaml": "hooks:\n" +
					"  SubagentStop:\n    - hooks:\n        - {type: command, command: e6}\n" +
					"  SubagentStart:\n    - hooks:\n        - {type: command, command: e5}\n" +
					"  Stop:\n    - hooks:\n        - {type: command, command: e4}\n" +
					"  PostToolUse:\n    - hooks:\n        - {type: command, command: e3}\n" +
					"  PreToolUse:\n    - hooks:\n        - {type: command, command: e2}\n" +
					"  UserPromptSubmit:\n    - hooks:\n        - {type: command, command: e1}\n" +
					"  SessionStart:\n    - hooks:\n        - {type: command, command: e0}\n",
			},
			want: []wantHook{
				{
					key:        "all:SessionStart:0",
					event:      defs.HookEventSessionStart,
					command:    "e0",
					timeout:    time.Minute,
					sourcePath: "hooks/all.yaml",
				},
				{
					key:        "all:UserPromptSubmit:0",
					event:      defs.HookEventUserPromptSubmit,
					command:    "e1",
					timeout:    time.Minute,
					sourcePath: "hooks/all.yaml",
				},
				{
					key:        "all:PreToolUse:0",
					event:      defs.HookEventPreToolUse,
					command:    "e2",
					timeout:    time.Minute,
					sourcePath: "hooks/all.yaml",
				},
				{
					key:        "all:PostToolUse:0",
					event:      defs.HookEventPostToolUse,
					command:    "e3",
					timeout:    time.Minute,
					sourcePath: "hooks/all.yaml",
				},
				{
					key:        "all:Stop:0",
					event:      defs.HookEventStop,
					command:    "e4",
					timeout:    time.Minute,
					sourcePath: "hooks/all.yaml",
				},
				{
					key:        "all:SubagentStart:0",
					event:      defs.HookEventSubagentStart,
					command:    "e5",
					timeout:    time.Minute,
					sourcePath: "hooks/all.yaml",
				},
				{
					key:        "all:SubagentStop:0",
					event:      defs.HookEventSubagentStop,
					command:    "e6",
					timeout:    time.Minute,
					sourcePath: "hooks/all.yaml",
				},
			},
		},
		{
			name: "hooks_directory_from_config_relative_to_config_file",
			files: map[string]string{
				"config.yaml":      "hooks:\n  directory: my_hooks\n",
				"my_hooks/a.yaml":  "hooks:\n  PostToolUse:\n    - matcher: Write\n      hooks:\n        - type: command\n          script: fmt.sh\n",
				"my_hooks/fmt.sh":  "#!/bin/sh\n",
				"hooks/ignored.sh": "#!/bin/sh\n",
			},
			want: []wantHook{
				{
					key:        "a:PostToolUse:0",
					event:      defs.HookEventPostToolUse,
					matcher:    "Write",
					scriptPath: "my_hooks/fmt.sh",
					timeout:    time.Minute,
					sourcePath: "my_hooks/a.yaml",
				},
			},
		},
		{name: "missing_directory_yields_no_hooks"},
		{
			name:  "empty_hooks_section_yields_no_hooks",
			files: map[string]string{"hooks/a.yaml": "hooks: {}\n"},
		},
	}
	// 2. 加载配置，比较每个 hook 的字段和 DisplayOrder。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			directory := writeDefinitionConfig(t)
			files := make(map[string]string, len(tt.files))
			for name, body := range tt.files {
				files[name] = strings.ReplaceAll(body, "{dir}", directory)
			}
			writeDefinitionFiles(t, directory, files)
			cfg, err := config.LoadConfig(&config.LoadConfigOpt{
				Home: directory,
				Path: filepath.Join(directory, "config.yaml"),
			})
			if err != nil {
				t.Fatal(err)
			}
			got := cfg.GetHooks()
			if len(got) != len(tt.want) {
				t.Fatalf("GetHooks() has %d hooks, want %d: %+v", len(got), len(tt.want), got)
			}
			for index, definition := range got {
				want := tt.want[index]
				if definition.Matcher == nil {
					t.Fatalf("hook %s has no matcher", definition.Key)
				}
				if want.scriptPath != "" {
					want.scriptPath = filepath.Join(directory, want.scriptPath)
				}
				actual := wantHook{
					key:                definition.Key,
					event:              definition.Event,
					matcher:            definition.Matcher.String(),
					command:            definition.Command,
					scriptPath:         definition.ScriptPath,
					args:               definition.Args,
					timeout:            definition.Timeout,
					isBlockedOnFailure: definition.IsBlockedOnFailure,
					sourcePath:         definition.SourcePath,
				}
				want.sourcePath = filepath.Join(directory, want.sourcePath)
				if !reflect.DeepEqual(actual, want) {
					t.Fatalf("hook %d = %+v, want %+v", index, actual, want)
				}
				if definition.DisplayOrder != index {
					t.Fatalf("hook %s DisplayOrder = %d, want %d", definition.Key, definition.DisplayOrder, index)
				}
			}
			// 1. 修改返回值后再查一次，核对保存的定义不变。
			for _, definition := range got {
				for index := range definition.Args {
					definition.Args[index] = "changed"
				}
				definition.Command = "changed"
			}
			if again := cfg.GetHooks(); len(again) != len(tt.want) {
				t.Fatalf("GetHooks() again has %d hooks, want %d", len(again), len(tt.want))
			} else {
				for index, definition := range again {
					if definition.Command == "changed" || slices.Contains(definition.Args, "changed") {
						t.Fatalf("caller modified hook snapshot %d: %+v", index, definition)
					}
				}
			}
		})
	}
}

// demoPluginManifest 是 demo 插件的 manifest：SessionStart 的 script hook 和用 ${OPENVAALA_PLUGIN_ROOT} 指向插件文件的 stdio MCP server demo-docs。
const demoPluginManifest = "name: demo\nversion: 0.1.0\n" +
	"hooks:\n  SessionStart:\n    - hooks:\n        - type: command\n          script: ./hooks/start.sh\n          args: [SessionStart]\n" +
	"mcp_servers:\n  - id: demo-docs\n    type: stdio\n    command: ${OPENVAALA_PLUGIN_ROOT}/bin/server\n" +
	"    args: [--data, \"${OPENVAALA_PLUGIN_ROOT}/data\"]\n    env:\n      DATA_DIR: ${OPENVAALA_PLUGIN_ROOT}/data\n"

// getPluginFiles 生成一个插件的测试文件：root 下的 manifest 和其他文件。
func getPluginFiles(root, manifest string, pluginFiles map[string]string) map[string]string {
	// 1. manifest 和插件文件的路径都接到 root 下。
	files := map[string]string{filepath.Join(root, ".openvaala-plugin", "plugin.yaml"): manifest}
	for name, body := range pluginFiles {
		files[filepath.Join(root, name)] = body
	}
	return files
}

// getDemoPluginFiles 生成 demo 插件的测试文件。
func getDemoPluginFiles(root string) map[string]string {
	// 1. 组装 demo 插件的 manifest、skill 和 hook 脚本。
	return getPluginFiles(root, demoPluginManifest, map[string]string{
		"skills/greet/SKILL.md": "---\nname: greet\ndescription: Greet the user.\n---\nSay hello.\n",
		"hooks/start.sh":        "#!/bin/sh\n",
	})
}

// getLoadedPlugins 从 hook 和 MCP server 上收集加载出的插件，按名字排序。
func getLoadedPlugins(t *testing.T, cfg *config.Config) []*defs.Plugin {
	t.Helper()
	// 1. 收集插件，同名的内容必须一致，再按名字去重排序。
	pluginsByName := make(map[string]*defs.Plugin)
	add := func(plugin *defs.Plugin) {
		if plugin == nil {
			return
		}
		if loaded, found := pluginsByName[plugin.Name]; found && !reflect.DeepEqual(*loaded, *plugin) {
			t.Fatalf("plugin %s is loaded twice differently: %+v and %+v", plugin.Name, loaded, plugin)
		}
		pluginsByName[plugin.Name] = plugin
	}
	for _, definition := range cfg.GetHooks() {
		add(definition.Plugin)
	}
	for _, server := range cfg.GetMCPServers() {
		add(server.Plugin)
	}
	plugins := make([]*defs.Plugin, 0, len(pluginsByName))
	for _, name := range slices.Sorted(maps.Keys(pluginsByName)) {
		plugins = append(plugins, pluginsByName[name])
	}
	return plugins
}

// mergeFiles 合并多组测试文件。
func mergeFiles(fileSets ...map[string]string) map[string]string {
	// 1. 按传入顺序放进新 map，同一路径以后面的为准。
	files := map[string]string{}
	for _, fileSet := range fileSets {
		maps.Copy(files, fileSet)
	}
	return files
}

// TestLoadConfigPlugins 验证插件的发现和加载：skill 目录、hook、MCP server 挂上插件，禁用和不是插件的条目跳过，非法 manifest 报错。
func TestLoadConfigPlugins(t *testing.T) {
	// 1. 准备插件 manifest、插件文件、禁用名单和错误样例。
	const (
		userHook = "hooks:\n  SessionStart:\n    - hooks:\n        - type: command\n          command: echo user\n"
		// scriptHookManifest 是名为 demo 的插件 manifest 开头，后面接 SessionStart hook 的 script 值。
		scriptHookManifest = "name: demo\nhooks:\n  SessionStart:\n    - hooks:\n        - type: command\n          script: "
		commandHook        = "hooks:\n  SessionStart:\n    - hooks:\n        - type: command\n          command: echo plugin\n"
	)
	// wantHook 是 GetHooks 里一个 hook 要核对的字段；scriptPath 相对配置目录。
	type wantHook struct {
		key        string
		pluginName string
		scriptPath string
	}
	// wantMCPServer 是 GetMCPServers 里一个 server 要核对的字段；command、args、env 值里的 {dir} 换成配置目录。
	type wantMCPServer struct {
		id         string
		command    string
		args       []string
		env        map[string]string
		pluginName string
	}
	tests := []struct {
		name string
		// files 是相对配置目录的文件，测试里 Home 就是配置目录。
		files map[string]string
		// symlinks 是相对配置目录的符号链接到它指向的目录（也相对配置目录）。
		symlinks map[string]string
		// wantErr 非空时加载必须失败，错误带这段文字和出错位置：wantErrPath（相对配置目录）为空时是出错插件的 manifest 路径。
		wantErr     string
		wantErrPath string
		// wantPlugins 是 hook 和 MCP server 挂着的插件，Directory、SkillDirectories 相对配置目录。
		wantPlugins    []defs.Plugin
		wantHooks      []wantHook
		wantMCPServers []wantMCPServer
	}{
		{
			name: "plugin_skills_hooks_and_mcp_server_follow_user_definitions",
			files: mergeFiles(getDemoPluginFiles("plugins/demo"), map[string]string{
				"hooks/user.yaml": userHook,
				// Agent 可以引用插件声明的 MCP server。
				"agents/worker.yaml": "name: worker\ninstructions: Work.\nmcp_servers: [demo-docs]\n",
			}),
			wantPlugins: []defs.Plugin{{
				Name:             "demo",
				Version:          "0.1.0",
				Directory:        "plugins/demo",
				SkillDirectories: []string{"plugins/demo/skills"},
			}},
			wantHooks: []wantHook{
				{key: "user:SessionStart:0"},
				{
					key:        "plugin:demo:SessionStart:0",
					pluginName: "demo",
					scriptPath: "plugins/demo/hooks/start.sh",
				},
			},
			wantMCPServers: []wantMCPServer{{
				id:         "demo-docs",
				command:    "{dir}/plugins/demo/bin/server",
				args:       []string{"--data", "{dir}/plugins/demo/data"},
				env:        map[string]string{"DATA_DIR": "{dir}/plugins/demo/data", "OPENVAALA_PLUGIN_ROOT": "{dir}/plugins/demo"},
				pluginName: "demo",
			}},
		},
		{
			name: "plugins_without_skills_sorted_by_name",
			files: mergeFiles(
				getPluginFiles("plugins/b-tools", "name: b-tools\n"+commandHook, nil),
				getPluginFiles("plugins/a-tools", "name: a-tools\n"+commandHook, nil),
				map[string]string{"hooks/user.yaml": userHook},
			),
			wantPlugins: []defs.Plugin{{
				Name:      "a-tools",
				Directory: "plugins/a-tools",
			}, {
				Name:      "b-tools",
				Directory: "plugins/b-tools",
			}},
			wantHooks: []wantHook{
				{key: "user:SessionStart:0"},
				{
					key:        "plugin:a-tools:SessionStart:0",
					pluginName: "a-tools",
				},
				{
					key:        "plugin:b-tools:SessionStart:0",
					pluginName: "b-tools",
				},
			},
		},
		{
			name: "install_record_sets_installed_at",
			files: getPluginFiles("plugins/demo", "name: demo\n"+commandHook, map[string]string{
				".openvaala-install.yaml": "source: https://example.com/demo.git\nref: main\ncommit: abc123\ninstalled_at: 2026-10-23T10:00:00Z\n",
			}),
			wantPlugins: []defs.Plugin{{
				Name:        "demo",
				Directory:   "plugins/demo",
				InstalledAt: time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC),
			}},
			wantHooks: []wantHook{{
				key:        "plugin:demo:SessionStart:0",
				pluginName: "demo",
			}},
		},
		{
			name: "disabled_plugins_are_not_read",
			files: mergeFiles(getDemoPluginFiles("plugins/demo"), getPluginFiles("plugins/broken", "name: other\n", nil), map[string]string{
				"config.yaml":     "plugins:\n  disabled: [demo, broken, missing]\n",
				"hooks/user.yaml": userHook,
			}),
			wantHooks: []wantHook{{key: "user:SessionStart:0"}},
		},
		{
			name: "user_mcp_server_overrides_plugin_server_with_same_id",
			files: mergeFiles(getDemoPluginFiles("plugins/demo"), map[string]string{
				"mcp_servers/demo-docs.yaml": "id: demo-docs\ntype: stdio\ncommand: user-server\n",
			}),
			wantPlugins: []defs.Plugin{{
				Name:             "demo",
				Version:          "0.1.0",
				Directory:        "plugins/demo",
				SkillDirectories: []string{"plugins/demo/skills"},
			}},
			wantHooks: []wantHook{{
				key:        "plugin:demo:SessionStart:0",
				pluginName: "demo",
				scriptPath: "plugins/demo/hooks/start.sh",
			}},
			wantMCPServers: []wantMCPServer{{
				id:      "demo-docs",
				command: "user-server",
			}},
		},
		{
			name: "symlinked_plugin_loaded_and_other_entries_skipped",
			files: mergeFiles(
				getPluginFiles("code/linked", "name: linked\nhooks:\n  SessionStart:\n    - hooks:\n        - type: command\n          script: hooks/start.sh\n",
					map[string]string{"hooks/start.sh": "#!/bin/sh\n", "skills/greet/SKILL.md": "---\nname: greet\ndescription: Greet.\n---\nHi.\n"}),
				// 名字以 . 开头的（例如 install 的临时目录）、没有 manifest 的目录、普通文件和坏链接都跳过，manifest 写坏了也不读。
				getPluginFiles("plugins/.install-x", "name: broken\nunknown: field\n", nil),
				map[string]string{"plugins/no-manifest/README.md": "not a plugin\n", "plugins/notes.txt": "not a plugin\n"},
			),
			symlinks: map[string]string{"plugins/linked": "code/linked", "plugins/broken": "code/missing"},
			wantPlugins: []defs.Plugin{{
				Name:             "linked",
				Directory:        "plugins/linked",
				SkillDirectories: []string{"plugins/linked/skills"},
			}},
			// 插件目录不解析符号链接，脚本路径以 plugins/linked 开头。
			wantHooks: []wantHook{{
				key:        "plugin:linked:SessionStart:0",
				pluginName: "linked",
				scriptPath: "plugins/linked/hooks/start.sh",
			}},
		},
		{
			name:    "name_differs_from_directory",
			files:   getPluginFiles("plugins/demo", "name: other\n", nil),
			wantErr: `plugin name "other" must match directory "demo"`,
		},
		{
			name:    "reserved_name",
			files:   getPluginFiles("plugins/skill", "name: skill\n", nil),
			wantErr: `plugin name "skill" is reserved`,
		},
		{
			name:    "skills_directory_outside_plugin",
			files:   mergeFiles(getPluginFiles("plugins/demo", "name: demo\nskills: ../shared\n", nil), map[string]string{"plugins/shared/a/SKILL.md": "x\n"}),
			wantErr: "plugin skills directory must be inside the plugin directory",
		},
		{
			name:    "hook_script_escapes_plugin_with_dot_dot",
			files:   mergeFiles(getPluginFiles("plugins/demo", scriptHookManifest+"../x.sh\n", nil), map[string]string{"plugins/x.sh": "#!/bin/sh\n"}),
			wantErr: "hooks.SessionStart[0].hooks[0]: plugin hook script must be inside the plugin directory",
		},
		{
			name:    "hook_script_absolute",
			files:   getPluginFiles("plugins/demo", scriptHookManifest+"/tmp/x.sh\n", nil),
			wantErr: "plugin hook script must be inside the plugin directory",
		},
		{
			name:    "hook_script_in_home",
			files:   mergeFiles(getPluginFiles("plugins/demo", scriptHookManifest+"~/x.sh\n", nil), map[string]string{"x.sh": "#!/bin/sh\n"}),
			wantErr: "plugin hook script must be inside the plugin directory",
		},
		{
			name:    "mcp_server_sets_executors",
			files:   getPluginFiles("plugins/demo", "name: demo\nmcp_servers:\n  - {id: demo-docs, type: stdio, command: server, executors: [local]}\n", nil),
			wantErr: "plugin mcp server cannot set executors; override it in mcp_servers/demo-docs.yaml",
		},
		{
			name: "two_plugins_declare_same_mcp_server",
			files: mergeFiles(
				getPluginFiles("plugins/a", "name: a\nmcp_servers:\n  - {id: demo-docs, type: stdio, command: server}\n", nil),
				getPluginFiles("plugins/b", "name: b\nmcp_servers:\n  - {id: demo-docs, type: stdio, command: server}\n", nil),
			),
			wantErr: `mcp server "demo-docs" is declared by plugins "a" and "b"`,
		},
		{
			name:    "plugin_declares_same_mcp_server_twice",
			files:   getPluginFiles("plugins/a", "name: a\nmcp_servers:\n  - {id: demo-docs, type: stdio, command: server}\n  - {id: demo-docs, type: stdio, command: other}\n", nil),
			wantErr: `mcp_servers[1]: mcp server "demo-docs" is declared twice in plugin "a"`,
		},
		{
			// 链接指向自己，读不出条目本身：不当作坏链接跳过，报错并带上条目路径。
			name:        "plugin_entry_symlink_loop",
			files:       getDemoPluginFiles("plugins/demo"),
			symlinks:    map[string]string{"plugins/loop": "plugins/loop"},
			wantErr:     "too many levels of symbolic links",
			wantErrPath: "plugins/loop",
		},
	}
	// 2. 加载配置，比较加载出的插件、hook 和 MCP server，或核对错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			directory := writeDefinitionConfig(t)
			writeDefinitionFiles(t, directory, tt.files)
			for link, target := range tt.symlinks {
				if err := os.Symlink(filepath.Join(directory, target), filepath.Join(directory, link)); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := config.LoadConfig(&config.LoadConfigOpt{
				Home: directory,
				Path: filepath.Join(directory, "config.yaml"),
			})
			if tt.wantErr != "" {
				wantErrPath := "/.openvaala-plugin/plugin.yaml: "
				if tt.wantErrPath != "" {
					wantErrPath = filepath.Join(directory, tt.wantErrPath) + ": "
				}
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || !strings.Contains(err.Error(), wantErrPath) {
					t.Fatalf("LoadConfig() error = %v, want %q and %q", err, wantErrPath, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}

			plugins := getLoadedPlugins(t, cfg)
			gotPlugins := make([]defs.Plugin, 0, len(plugins))
			for _, plugin := range plugins {
				gotPlugins = append(gotPlugins, *plugin)
			}
			wantPlugins := make([]defs.Plugin, 0, len(tt.wantPlugins))
			for _, plugin := range tt.wantPlugins {
				plugin.Directory = filepath.Join(directory, plugin.Directory)
				for index, skillDirectory := range plugin.SkillDirectories {
					plugin.SkillDirectories[index] = filepath.Join(directory, skillDirectory)
				}
				wantPlugins = append(wantPlugins, plugin)
			}
			if !reflect.DeepEqual(gotPlugins, wantPlugins) {
				t.Fatalf("loaded plugins = %+v, want %+v", gotPlugins, wantPlugins)
			}
			// isSamePlugin hook 或 MCP server 挂的插件是不是加载出的同名插件？
			isSamePlugin := func(plugin *defs.Plugin) bool {
				index := slices.IndexFunc(gotPlugins, func(got defs.Plugin) bool { return got.Name == plugin.Name })
				return index >= 0 && reflect.DeepEqual(*plugin, gotPlugins[index])
			}

			hooks := cfg.GetHooks()
			if len(hooks) != len(tt.wantHooks) {
				t.Fatalf("GetHooks() has %d hooks, want %d: %+v", len(hooks), len(tt.wantHooks), hooks)
			}
			for index, definition := range hooks {
				want := tt.wantHooks[index]
				if want.scriptPath != "" {
					want.scriptPath = filepath.Join(directory, want.scriptPath)
				}
				got := wantHook{
					key:        definition.Key,
					scriptPath: definition.ScriptPath,
				}
				if definition.Plugin != nil {
					got.pluginName = definition.Plugin.Name
				}
				if got != want || definition.DisplayOrder != index {
					t.Fatalf("hook %d = %+v with DisplayOrder %d, want %+v", index, got, definition.DisplayOrder, want)
				}
				if definition.Plugin == nil {
					continue
				}
				// pkg/hook 去掉 ScriptPath 的 Plugin.Directory 前缀得到脚本在插件里的位置。
				if !isSamePlugin(definition.Plugin) || definition.SourcePath != filepath.Join(definition.Plugin.Directory, ".openvaala-plugin", "plugin.yaml") ||
					(definition.ScriptPath != "" && !strings.HasPrefix(definition.ScriptPath, definition.Plugin.Directory+string(filepath.Separator))) {
					t.Fatalf("plugin hook %s = %+v, want the loaded plugin, its manifest as source and a script under %s", definition.Key, definition, definition.Plugin.Directory)
				}
			}

			servers := cfg.GetMCPServers()
			if len(servers) != len(tt.wantMCPServers) {
				t.Fatalf("GetMCPServers() has %d servers, want %d: %+v", len(servers), len(tt.wantMCPServers), servers)
			}
			replacer := strings.NewReplacer("{dir}", directory)
			for index, server := range servers {
				want := tt.wantMCPServers[index]
				want.command = replacer.Replace(want.command)
				if want.args != nil {
					want.args = slices.Clone(want.args)
					for argumentIndex, argument := range want.args {
						want.args[argumentIndex] = replacer.Replace(argument)
					}
				}
				if want.env != nil {
					want.env = maps.Clone(want.env)
					for key, value := range want.env {
						want.env[key] = replacer.Replace(value)
					}
				}
				got := wantMCPServer{
					id:      server.ID,
					command: server.Command,
					args:    server.Args,
					env:     server.Env,
				}
				if server.Plugin != nil {
					got.pluginName = server.Plugin.Name
				}
				if !reflect.DeepEqual(got, want) || !slices.Equal(server.ExecutorIDs, []string{defs.LocalExecutorID}) {
					t.Fatalf("mcp server %d = %+v on %v, want %+v on [local]", index, got, server.ExecutorIDs, want)
				}
				if server.Plugin != nil && !isSamePlugin(server.Plugin) {
					t.Fatalf("mcp server %s plugin = %+v, want the loaded plugin", server.ID, server.Plugin)
				}
			}
		})
	}
}

// TestConfigAgentMCPServers 验证 Agent 的 mcp_servers：没写是全部、[] 是不用，disabled 的 server 也能引用。
func TestConfigAgentMCPServers(t *testing.T) {
	// 1. 准备 Agent 文件里不同的 mcp_servers 写法。
	const agentFile = "name: default\ninstructions: Be concise.\nmodel:\n  provider: local\n  model: MyModel\n"
	tests := []struct {
		name          string
		agentFile     string // 为空时不写 agents/default.yaml，使用内置 default Agent
		wantServerIDs defs.OptionalStringList
	}{
		{
			name:          "omitted_means_all_servers",
			agentFile:     agentFile,
			wantServerIDs: nil,
		},
		{
			name:          "empty_list_means_no_servers",
			agentFile:     agentFile + "mcp_servers: []\n",
			wantServerIDs: defs.OptionalStringList{},
		},
		{
			name:          "selected_server",
			agentFile:     agentFile + "mcp_servers: [b]\n",
			wantServerIDs: defs.OptionalStringList{"b"},
		},
		{
			name:          "disabled_server_counts_as_configured",
			agentFile:     agentFile + "mcp_servers: [a]\n",
			wantServerIDs: defs.OptionalStringList{"a"},
		},
		{
			name:          "builtin_default_agent_means_all_servers",
			wantServerIDs: nil,
		},
	}
	// 2. 加载配置，比较 default Agent 的 MCPServerIDs，nil 和空列表要分开。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			directory := writeDefinitionConfig(t)
			writeDefinitionFiles(t, directory, map[string]string{
				"mcp_servers/a.yaml": "id: a\ntype: http\nurl: https://example.com/mcp\ndisabled: true\n",
				"mcp_servers/b.yaml": "id: b\ntype: stdio\ncommand: uvx\n",
			})
			if tt.agentFile == "" {
				if err := os.Remove(filepath.Join(directory, "agents/default.yaml")); err != nil {
					t.Fatal(err)
				}
			} else {
				writeDefinitionFiles(t, directory, map[string]string{"agents/default.yaml": tt.agentFile})
			}
			cfg, err := config.LoadConfig(&config.LoadConfigOpt{
				Home: directory,
				Path: filepath.Join(directory, "config.yaml"),
			})
			if err != nil {
				t.Fatal(err)
			}
			getServerIDs := func() defs.OptionalStringList {
				definitions := cfg.GetAgentDefinitions()
				if len(definitions) != 1 || definitions[0].Name != "default" {
					t.Fatalf("GetAgentDefinitions() = %+v, want only the default Agent", definitions)
				}
				return definitions[0].MCPServerIDs
			}
			got := getServerIDs()
			if (got == nil) != (tt.wantServerIDs == nil) || !slices.Equal(got, tt.wantServerIDs) {
				t.Fatalf("MCPServerIDs = %#v, want %#v", got, tt.wantServerIDs)
			}
			// 1. 修改返回值后再查一次，核对保存的定义不变。
			for index := range got {
				got[index] = "changed"
			}
			if again := getServerIDs(); (again == nil) != (tt.wantServerIDs == nil) || !slices.Equal(again, tt.wantServerIDs) {
				t.Fatalf("caller modified Agent snapshot: %#v", again)
			}
		})
	}
}

// TestLoadConfigServer 验证 server 段：默认值、文件和环境变量覆盖，workspace_directory 展开 ~ 且必须是绝对路径。
func TestLoadConfigServer(t *testing.T) {
	// 1. 准备 listen、token 和 workspace_directory 的样例。
	tests := []struct {
		name       string
		file       string
		env        map[string]string
		wantListen string
		wantToken  string
		// wantWorkspaceDirectory 里的 {home} 换成用例的主目录。
		wantWorkspaceDirectory string
		wantErr                string
	}{
		{
			name:       "defaults",
			file:       "{}\n",
			wantListen: "127.0.0.1:8790",
		},
		{
			name:       "file_values",
			file:       "server:\n  listen: 0.0.0.0:9000\n  token: lan-secret\n",
			wantListen: "0.0.0.0:9000",
			wantToken:  "lan-secret",
		},
		{
			name:       "environment_token",
			file:       "server:\n  token: file-secret\n",
			env:        map[string]string{"OPENVAALA_SERVER_TOKEN": "env-secret"},
			wantListen: "127.0.0.1:8790",
			wantToken:  "env-secret",
		},
		{
			name:    "empty_listen_rejected",
			file:    "server:\n  listen: \"\"\n",
			wantErr: "server.listen",
		},
		{
			name:                   "workspace_directory_absolute",
			file:                   "server:\n  workspace_directory: /srv/work/\n",
			wantListen:             "127.0.0.1:8790",
			wantWorkspaceDirectory: "/srv/work",
		},
		{
			name:                   "workspace_directory_home_expanded",
			file:                   "server:\n  workspace_directory: ~/x\n",
			wantListen:             "127.0.0.1:8790",
			wantWorkspaceDirectory: "{home}/x",
		},
		{
			name:                   "workspace_directory_home_only",
			file:                   "server:\n  workspace_directory: '~'\n",
			wantListen:             "127.0.0.1:8790",
			wantWorkspaceDirectory: "{home}",
		},
		{
			name:                   "workspace_directory_environment_overrides_file",
			file:                   "server:\n  workspace_directory: /srv/file\n",
			env:                    map[string]string{"OPENVAALA_SERVER_WORKSPACE_DIRECTORY": "~/env"},
			wantListen:             "127.0.0.1:8790",
			wantWorkspaceDirectory: "{home}/env",
		},
		{
			name:    "workspace_directory_relative_rejected",
			file:    "server:\n  workspace_directory: work/dir\n",
			wantErr: "server.workspace_directory must be an absolute path",
		},
		{
			name:    "workspace_directory_relative_to_config_rejected",
			file:    "server:\n  workspace_directory: ./work\n",
			wantErr: "server.workspace_directory must be an absolute path",
		},
	}
	// 2. 加载配置，比较 listen、token 和 workspace_directory，或核对错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "config.yaml")
			if err := os.WriteFile(path, []byte(tt.file), 0600); err != nil {
				t.Fatal(err)
			}
			for key, value := range tt.env {
				t.Setenv(key, value)
			}
			loaded, err := config.LoadConfig(&config.LoadConfigOpt{
				Home: directory,
				Path: path,
			})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("LoadConfig() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wantWorkspaceDirectory := strings.ReplaceAll(tt.wantWorkspaceDirectory, "{home}", directory)
			if loaded.Server.Listen != tt.wantListen || loaded.Server.Token != tt.wantToken || loaded.Server.WorkspaceDirectory != wantWorkspaceDirectory {
				t.Fatalf("server = %+v, want listen %q token %q workspace directory %q", loaded.Server, tt.wantListen, tt.wantToken, wantWorkspaceDirectory)
			}
		})
	}
}

// TestLoadConfigClient 验证 client 段：url、token 的覆盖顺序，path_mappings 的路径和重复检查。
func TestLoadConfigClient(t *testing.T) {
	// 1. 准备 url、token 和 path_mappings 的样例。
	tests := []struct {
		name         string
		file         string
		env          map[string]string
		wantURL      string
		wantToken    string
		wantMappings config.PathMappings
		wantErr      string
	}{
		{
			name: "defaults",
			file: "{}\n",
		},
		{
			name: "file_values",
			file: "client:\n  url: ws://10.0.0.5:8790/api/v1/openvaala\n  token: remote-secret\n" +
				"  path_mappings:\n    - client: /Users/me/code\n      server: /srv/code\n    - client: /Users/me/docs\n      server: /srv/docs\n",
			wantURL:   "ws://10.0.0.5:8790/api/v1/openvaala",
			wantToken: "remote-secret",
			wantMappings: config.PathMappings{
				{
					Client: "/Users/me/code",
					Server: "/srv/code",
				},
				{
					Client: "/Users/me/docs",
					Server: "/srv/docs",
				},
			},
		},
		{
			name: "windows_client_path",
			file: "client:\n  path_mappings:\n    - client: 'C:\\Users\\me'\n      server: /mnt/c/Users/me\n",
			wantMappings: config.PathMappings{{
				Client: `C:\Users\me`,
				Server: "/mnt/c/Users/me",
			}},
		},
		{
			name:    "environment_url_overrides_file",
			file:    "client:\n  url: ws://file-host:8790/api/v1/openvaala\n  path_mappings:\n    - client: /a\n      server: /b\n",
			env:     map[string]string{"OPENVAALA_CLIENT_URL": "ws://env-host:8790/api/v1/openvaala"},
			wantURL: "ws://env-host:8790/api/v1/openvaala",
			wantMappings: config.PathMappings{{
				Client: "/a",
				Server: "/b",
			}},
		},
		{
			name:      "environment_token_overrides_file",
			file:      "client:\n  token: file-secret\n",
			env:       map[string]string{"OPENVAALA_CLIENT_TOKEN": "env-secret"},
			wantToken: "env-secret",
		},
		{
			name:    "relative_client_path_rejected",
			file:    "client:\n  path_mappings:\n    - client: code\n      server: /srv/code\n",
			wantErr: "client.path_mappings[0]: client and server must be absolute paths",
		},
		{
			name: "relative_server_path_rejected_with_index",
			file: "client:\n  path_mappings:\n    - client: /a\n      server: /b\n" +
				"    - client: /c\n      server: srv/c\n",
			wantErr: "client.path_mappings[1]: client and server must be absolute paths",
		},
		{
			name:    "drive_without_separator_rejected",
			file:    "client:\n  path_mappings:\n    - client: 'C:Users'\n      server: /srv\n",
			wantErr: "client.path_mappings[0]: client and server must be absolute paths",
		},
		{
			name:    "windows_server_path_rejected",
			file:    "client:\n  path_mappings:\n    - client: /a\n      server: 'C:\\srv'\n",
			wantErr: "client.path_mappings[0]: client and server must be absolute paths",
		},
		{
			name:    "missing_server_rejected",
			file:    "client:\n  path_mappings:\n    - client: /a\n",
			wantErr: "client.path_mappings[0]: client and server must be absolute paths",
		},
		{
			name:    "unknown_mapping_field_rejected",
			file:    "client:\n  path_mappings:\n    - client: /a\n      server: /b\n      extra: x\n",
			wantErr: "invalid YAML, field type or unknown field",
		},
		{
			name:    "duplicate_client_rejected",
			file:    "client:\n  path_mappings:\n    - client: /a\n      server: /x\n    - client: /a\n      server: /y\n",
			wantErr: `client.path_mappings: client "/a" is used by more than one mapping`,
		},
		{
			name:    "duplicate_client_trailing_separator_rejected",
			file:    "client:\n  path_mappings:\n    - client: /a\n      server: /x\n    - client: /a/\n      server: /y\n",
			wantErr: `client.path_mappings: client "/a/" is used by more than one mapping`,
		},
		{
			name:    "duplicate_windows_client_with_other_separator_and_drive_case_rejected",
			file:    "client:\n  path_mappings:\n    - client: 'C:\\Users\\me'\n      server: /x\n    - client: 'c:/Users/me/'\n      server: /y\n",
			wantErr: `client.path_mappings: client "c:/Users/me/" is used by more than one mapping`,
		},
		{
			name:    "duplicate_windows_client_reports_value_as_written",
			file:    "client:\n  path_mappings:\n    - client: 'c:/work'\n      server: /x\n    - client: 'C:\\work'\n      server: /y\n",
			wantErr: `client.path_mappings: client "C:\\work" is used by more than one mapping`,
		},
		{
			name:    "duplicate_server_rejected",
			file:    "client:\n  path_mappings:\n    - client: /a\n      server: /srv\n    - client: /b\n      server: /srv\n",
			wantErr: `client.path_mappings: server "/srv" is used by more than one mapping`,
		},
		{
			name:    "duplicate_server_trailing_separator_rejected",
			file:    "client:\n  path_mappings:\n    - client: /a\n      server: /srv\n    - client: /b\n      server: /srv/\n",
			wantErr: `client.path_mappings: server "/srv/" is used by more than one mapping`,
		},
		{
			name: "nested_client_prefixes_accepted",
			file: "client:\n  path_mappings:\n    - client: /srv\n      server: /x\n    - client: /srv/demo\n      server: /y\n",
			wantMappings: config.PathMappings{
				{
					Client: "/srv",
					Server: "/x",
				},
				{
					Client: "/srv/demo",
					Server: "/y",
				},
			},
		},
		{
			name: "nested_server_prefixes_accepted",
			file: "client:\n  path_mappings:\n    - client: /a\n      server: /srv\n    - client: /b\n      server: /srv/demo\n",
			wantMappings: config.PathMappings{
				{
					Client: "/a",
					Server: "/srv",
				},
				{
					Client: "/b",
					Server: "/srv/demo",
				},
			},
		},
		{
			name: "same_directory_on_different_drives_accepted",
			file: "client:\n  path_mappings:\n    - client: 'C:\\work'\n      server: /x\n    - client: 'D:\\work'\n      server: /y\n",
			wantMappings: config.PathMappings{
				{
					Client: `C:\work`,
					Server: "/x",
				},
				{
					Client: `D:\work`,
					Server: "/y",
				},
			},
		},
	}
	// 2. 加载配置，比较 url、token 和 path_mappings，或核对错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "config.yaml")
			if err := os.WriteFile(path, []byte(tt.file), 0600); err != nil {
				t.Fatal(err)
			}
			for key, value := range tt.env {
				t.Setenv(key, value)
			}
			loaded, err := config.LoadConfig(&config.LoadConfigOpt{
				Home: directory,
				Path: path,
			})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("LoadConfig() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Client.URL != tt.wantURL || loaded.Client.Token != tt.wantToken ||
				!slices.Equal(loaded.Client.PathMappings, tt.wantMappings) {
				t.Fatalf("client = %+v, want url %q token %q mappings %v", *loaded.Client, tt.wantURL, tt.wantToken, tt.wantMappings)
			}
		})
	}
}

// TestPathMappings 验证客户端路径和 serve 路径的互相转换：按整段目录名取最长前缀，Windows 路径的分隔符和盘符大小写，只用和客户端平台一致的映射。
func TestPathMappings(t *testing.T) {
	// 1. 准备映射、输入路径和期望结果。
	unixMapping := config.PathMapping{
		Client: "/a/code",
		Server: "/srv/code",
	}
	parentMapping := config.PathMapping{
		Client: "/a",
		Server: "/srv/parent",
	}
	windowsMapping := config.PathMapping{
		Client: `C:\Users\me`,
		Server: "/mnt/c/Users/me",
	}
	tests := []struct {
		name     string
		mappings config.PathMappings
		// toClient 为 true 时调用 GetClientPath，否则调用 GetServerPath；isWindowsClient 是 GetClientPath 的客户端平台。
		toClient        bool
		isWindowsClient bool
		input           string
		want            string
		wantFound       bool // 只对 GetServerPath 有意义
	}{
		{
			name:      "server_exact_directory",
			mappings:  config.PathMappings{unixMapping},
			input:     "/a/code",
			want:      "/srv/code",
			wantFound: true,
		},
		{
			name:      "server_nested_path",
			mappings:  config.PathMappings{unixMapping},
			input:     "/a/code/x/y.go",
			want:      "/srv/code/x/y.go",
			wantFound: true,
		},
		{
			name:      "server_trailing_slash",
			mappings:  config.PathMappings{unixMapping},
			input:     "/a/code/",
			want:      "/srv/code",
			wantFound: true,
		},
		{
			name:     "server_sibling_prefix_not_covered",
			mappings: config.PathMappings{unixMapping},
			input:    "/a/code2",
		},
		{
			name:     "server_shorter_path_not_covered",
			mappings: config.PathMappings{unixMapping},
			input:    "/a",
		},
		{
			name:     "server_unmapped_path",
			mappings: config.PathMappings{unixMapping},
			input:    "/b/code",
		},
		{
			name:     "server_unix_is_case_sensitive",
			mappings: config.PathMappings{unixMapping},
			input:    "/A/code",
		},
		{
			name:  "server_no_mappings",
			input: "/a/code",
		},
		{
			name:      "server_longest_prefix_first_listed",
			mappings:  config.PathMappings{unixMapping, parentMapping},
			input:     "/a/code/x",
			want:      "/srv/code/x",
			wantFound: true,
		},
		{
			name:      "server_longest_prefix_last_listed",
			mappings:  config.PathMappings{parentMapping, unixMapping},
			input:     "/a/code/x",
			want:      "/srv/code/x",
			wantFound: true,
		},
		{
			name:      "server_shorter_prefix_for_other_directory",
			mappings:  config.PathMappings{parentMapping, unixMapping},
			input:     "/a/other/x",
			want:      "/srv/parent/other/x",
			wantFound: true,
		},
		{
			name: "server_root_target",
			mappings: config.PathMappings{{
				Client: "/home/me",
				Server: "/",
			}},
			input:     "/home/me/x",
			want:      "/x",
			wantFound: true,
		},
		{
			name:      "server_windows_backslashes",
			mappings:  config.PathMappings{windowsMapping},
			input:     `C:\Users\me\proj\main.go`,
			want:      "/mnt/c/Users/me/proj/main.go",
			wantFound: true,
		},
		{
			name:      "server_windows_forward_slashes",
			mappings:  config.PathMappings{windowsMapping},
			input:     "C:/Users/me/proj",
			want:      "/mnt/c/Users/me/proj",
			wantFound: true,
		},
		{
			name:      "server_windows_mixed_separators",
			mappings:  config.PathMappings{windowsMapping},
			input:     `C:/Users\me/proj`,
			want:      "/mnt/c/Users/me/proj",
			wantFound: true,
		},
		{
			name:      "server_windows_drive_letter_ignores_case",
			mappings:  config.PathMappings{windowsMapping},
			input:     `c:\Users\me\proj`,
			want:      "/mnt/c/Users/me/proj",
			wantFound: true,
		},
		{
			name: "server_windows_mapping_drive_letter_ignores_case",
			mappings: config.PathMappings{{
				Client: "c:/work",
				Server: "/srv/work",
			}},
			input:     `C:\work\a`,
			want:      "/srv/work/a",
			wantFound: true,
		},
		{
			name:      "server_windows_exact_directory",
			mappings:  config.PathMappings{windowsMapping},
			input:     `C:\Users\me\`,
			want:      "/mnt/c/Users/me",
			wantFound: true,
		},
		{
			name:     "server_windows_sibling_prefix_not_covered",
			mappings: config.PathMappings{windowsMapping},
			input:    `C:\Users\me2\proj`,
		},
		{
			name:     "server_windows_directory_names_are_case_sensitive",
			mappings: config.PathMappings{windowsMapping},
			input:    `c:\users\me\x`,
		},
		{
			name: "server_windows_mapping_trailing_backslash",
			mappings: config.PathMappings{{
				Client: `C:\Users\me\`,
				Server: "/mnt/c/Users/me",
			}},
			input:     `C:\Users\me\x`,
			want:      "/mnt/c/Users/me/x",
			wantFound: true,
		},
		{
			name:     "server_windows_other_drive",
			mappings: config.PathMappings{windowsMapping},
			input:    `D:\Users\me`,
		},
		{
			name:     "server_windows_mapping_does_not_cover_unix_path",
			mappings: config.PathMappings{windowsMapping},
			input:    "/Users/me",
		},
		{
			name:     "server_unix_mapping_does_not_cover_windows_path",
			mappings: config.PathMappings{unixMapping},
			input:    `C:\a\code`,
		},
		{
			name: "server_windows_drive_root",
			mappings: config.PathMappings{{
				Client: `C:\`,
				Server: "/mnt/c",
			}},
			input:     `C:\Users\me`,
			want:      "/mnt/c/Users/me",
			wantFound: true,
		},
		{
			name: "server_windows_longest_prefix",
			mappings: config.PathMappings{{
				Client: `C:\`,
				Server: "/mnt/c",
			}, windowsMapping},
			input:     `C:\Users\me\x`,
			want:      "/mnt/c/Users/me/x",
			wantFound: true,
		},

		{
			name:     "client_exact_directory",
			mappings: config.PathMappings{unixMapping},
			toClient: true,
			input:    "/srv/code",
			want:     "/a/code",
		},
		{
			name: "client_server_mapping_trailing_slash",
			mappings: config.PathMappings{{
				Client: "/a/code",
				Server: "/srv/code/",
			}},
			toClient: true,
			input:    "/srv/code/x",
			want:     "/a/code/x",
		},
		{
			name:     "client_nested_path",
			mappings: config.PathMappings{unixMapping},
			toClient: true,
			input:    "/srv/code/x/y.go",
			want:     "/a/code/x/y.go",
		},
		{
			name:     "client_sibling_prefix_returned_unchanged",
			mappings: config.PathMappings{unixMapping},
			toClient: true,
			input:    "/srv/code2/x",
			want:     "/srv/code2/x",
		},
		{
			name:     "client_unmapped_path_returned_unchanged",
			mappings: config.PathMappings{unixMapping},
			toClient: true,
			input:    "/var/log",
			want:     "/var/log",
		},
		{
			name:     "client_no_mappings",
			toClient: true,
			input:    "/srv/code",
			want:     "/srv/code",
		},
		{
			name: "client_unix_client_trailing_slash",
			mappings: config.PathMappings{{
				Client: "/a/code/",
				Server: "/srv/code",
			}},
			toClient: true,
			input:    "/srv/code/x",
			want:     "/a/code/x",
		},
		{
			name: "client_longest_prefix",
			mappings: config.PathMappings{{
				Client: "/c/parent",
				Server: "/srv",
			}, {
				Client: "/c/code",
				Server: "/srv/code",
			}},
			toClient: true,
			input:    "/srv/code/x",
			want:     "/c/code/x",
		},
		{
			name:            "client_windows_path",
			mappings:        config.PathMappings{windowsMapping},
			toClient:        true,
			isWindowsClient: true,
			input:           "/mnt/c/Users/me/proj/main.go",
			want:            `C:\Users\me\proj\main.go`,
		},
		{
			name:            "client_windows_exact_directory",
			mappings:        config.PathMappings{windowsMapping},
			toClient:        true,
			isWindowsClient: true,
			input:           "/mnt/c/Users/me",
			want:            `C:\Users\me`,
		},
		{
			name: "client_windows_prefix_written_with_forward_slashes",
			mappings: config.PathMappings{{
				Client: "c:/work",
				Server: "/srv/work",
			}},
			toClient:        true,
			isWindowsClient: true,
			input:           "/srv/work/a/b",
			want:            `c:\work\a\b`,
		},
		{
			name: "client_windows_drive_root",
			mappings: config.PathMappings{{
				Client: `C:\`,
				Server: "/mnt/c",
			}},
			toClient:        true,
			isWindowsClient: true,
			input:           "/mnt/c/Users",
			want:            `C:\Users`,
		},
		{
			name: "client_windows_drive_root_exact",
			mappings: config.PathMappings{{
				Client: `C:\`,
				Server: "/mnt/c",
			}},
			toClient:        true,
			isWindowsClient: true,
			input:           "/mnt/c",
			want:            `C:\`,
		},
		{
			name:            "client_windows_sibling_prefix_returned_unchanged",
			mappings:        config.PathMappings{windowsMapping},
			toClient:        true,
			isWindowsClient: true,
			input:           "/mnt/c/Users/me2",
			want:            "/mnt/c/Users/me2",
		},
		{
			name: "client_windows_longest_prefix",
			mappings: config.PathMappings{{
				Client: `C:\`,
				Server: "/mnt/c",
			}, windowsMapping},
			toClient:        true,
			isWindowsClient: true,
			input:           "/mnt/c/Users/me/x",
			want:            `C:\Users\me\x`,
		},
		// 同一份配置给 WSL 里的 Codex TUI 和 Windows 版 Codex app 用：各自只用和自己平台一致的映射。
		{
			name:     "client_unix_client_skips_windows_mapping",
			mappings: config.PathMappings{windowsMapping},
			toClient: true,
			input:    "/mnt/c/Users/me/x",
			want:     "/mnt/c/Users/me/x",
		},
		{
			name:            "client_windows_client_skips_unix_mapping",
			mappings:        config.PathMappings{unixMapping},
			toClient:        true,
			isWindowsClient: true,
			input:           "/srv/code/x",
			want:            "/srv/code/x",
		},
		{
			name:     "client_both_platforms_unix_client",
			mappings: config.PathMappings{unixMapping, windowsMapping},
			toClient: true,
			input:    "/srv/code/x",
			want:     "/a/code/x",
		},
		{
			name:     "client_both_platforms_unix_client_other_prefix",
			mappings: config.PathMappings{unixMapping, windowsMapping},
			toClient: true,
			input:    "/mnt/c/Users/me/x",
			want:     "/mnt/c/Users/me/x",
		},
		{
			name:            "client_both_platforms_windows_client",
			mappings:        config.PathMappings{unixMapping, windowsMapping},
			toClient:        true,
			isWindowsClient: true,
			input:           "/mnt/c/Users/me/x",
			want:            `C:\Users\me\x`,
		},
		{
			name:            "client_both_platforms_windows_client_other_prefix",
			mappings:        config.PathMappings{unixMapping, windowsMapping},
			toClient:        true,
			isWindowsClient: true,
			input:           "/srv/code/x",
			want:            "/srv/code/x",
		},
		// 两边的 serve 前缀互相嵌套：最长前缀只在和客户端平台一致的映射里找。
		{
			name: "client_nested_windows_inside_unix_unix_client",
			mappings: config.PathMappings{{
				Client: "/home/me",
				Server: "/srv",
			}, {
				Client: `C:\code`,
				Server: "/srv/win",
			}},
			toClient: true,
			input:    "/srv/win/x",
			want:     "/home/me/win/x",
		},
		{
			name: "client_nested_windows_inside_unix_windows_client",
			mappings: config.PathMappings{{
				Client: "/home/me",
				Server: "/srv",
			}, {
				Client: `C:\code`,
				Server: "/srv/win",
			}},
			toClient:        true,
			isWindowsClient: true,
			input:           "/srv/win/x",
			want:            `C:\code\x`,
		},
		{
			name: "client_nested_windows_inside_unix_windows_client_outside",
			mappings: config.PathMappings{{
				Client: "/home/me",
				Server: "/srv",
			}, {
				Client: `C:\code`,
				Server: "/srv/win",
			}},
			toClient:        true,
			isWindowsClient: true,
			input:           "/srv/other",
			want:            "/srv/other",
		},
		{
			name: "client_nested_unix_inside_windows_unix_client",
			mappings: config.PathMappings{{
				Client: `C:\`,
				Server: "/srv",
			}, {
				Client: "/home/me",
				Server: "/srv/home",
			}},
			toClient: true,
			input:    "/srv/x",
			want:     "/srv/x",
		},
		{
			name: "client_nested_unix_inside_windows_windows_client",
			mappings: config.PathMappings{{
				Client: `C:\`,
				Server: "/srv",
			}, {
				Client: "/home/me",
				Server: "/srv/home",
			}},
			toClient:        true,
			isWindowsClient: true,
			input:           "/srv/home/x",
			want:            `C:\home\x`,
		},
	}
	// 2. 按 toClient 调用 GetClientPath 或 GetServerPath，比较结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.toClient {
				if got := tt.mappings.GetClientPath(tt.input, tt.isWindowsClient); got != tt.want {
					t.Fatalf("GetClientPath(%q, %v) = %q, want %q", tt.input, tt.isWindowsClient, got, tt.want)
				}
				return
			}
			got, found := tt.mappings.GetServerPath(tt.input)
			if got != tt.want || found != tt.wantFound {
				t.Fatalf("GetServerPath(%q) = %q, %v, want %q, %v", tt.input, got, found, tt.want, tt.wantFound)
			}
		})
	}
}

// TestLoadConfigTelemetry 验证 telemetry 段的默认值、文件和环境变量覆盖，以及非法值报错。
func TestLoadConfigTelemetry(t *testing.T) {
	// 1. 准备日志级别、日志文件、OTLP 地址和请求头、内容采集的样例。
	defaultLogFile := func(home, _ string) string {
		return filepath.Join(home, ".local", "state", "openvaala", "openvaala.log")
	}
	tests := []struct {
		name         string
		section      string
		env          map[string]string
		wantLevel    slog.Level
		wantLogFile  func(home, configDirectory string) string
		wantEndpoint string
		wantHeaders  map[string]string
		// wantCaptureContent 是 telemetry.capture_content 的生效值，默认开启。
		wantCaptureContent bool
		wantErr            string
	}{
		{
			name:               "defaults",
			wantLevel:          slog.LevelInfo,
			wantLogFile:        defaultLogFile,
			wantCaptureContent: true,
		},
		{
			name: "file_values",
			section: "telemetry:\n  log_level: warn\n  log_file: logs/openvaala.log\n" +
				"  otlp_endpoint: http://127.0.0.1:4318\n  otlp_headers:\n    Authorization: Bearer token\n",
			wantLevel:    slog.LevelWarn,
			wantLogFile:  func(_, configDirectory string) string { return filepath.Join(configDirectory, "logs", "openvaala.log") },
			wantEndpoint: "http://127.0.0.1:4318",
			// Viper 把键统一转为小写；HTTP 头不区分大小写。
			wantHeaders:        map[string]string{"authorization": "Bearer token"},
			wantCaptureContent: true,
		},
		{
			name:               "environment_overrides_file",
			section:            "telemetry:\n  log_level: warn\n",
			env:                map[string]string{"OPENVAALA_TELEMETRY_LOG_LEVEL": "debug"},
			wantLevel:          slog.LevelDebug,
			wantLogFile:        defaultLogFile,
			wantCaptureContent: true,
		},
		{
			name:        "capture_content_disabled_in_file",
			section:     "telemetry:\n  capture_content: false\n",
			wantLevel:   slog.LevelInfo,
			wantLogFile: defaultLogFile,
		},
		{
			name:        "capture_content_disabled_by_environment",
			env:         map[string]string{"OPENVAALA_TELEMETRY_CAPTURE_CONTENT": "false"},
			wantLevel:   slog.LevelInfo,
			wantLogFile: defaultLogFile,
		},
		{
			name:    "invalid_level_rejected",
			section: "telemetry:\n  log_level: verbose\n",
			wantErr: "telemetry.log_level",
		},
		{
			name:    "endpoint_without_scheme_rejected",
			section: "telemetry:\n  otlp_endpoint: 127.0.0.1:4318\n",
			wantErr: "telemetry.otlp_endpoint",
		},
		{
			name:    "endpoint_with_unsupported_scheme_rejected",
			section: "telemetry:\n  otlp_endpoint: grpc://127.0.0.1:4317\n",
			wantErr: "telemetry.otlp_endpoint",
		},
		{
			name:    "endpoint_without_host_rejected",
			section: "telemetry:\n  otlp_endpoint: http:///v1/traces\n",
			wantErr: "telemetry.otlp_endpoint",
		},
		// 文件级严格解码先于字段转换执行，类型错误报告为 invalid YAML。
		{
			name:    "invalid_capture_content_rejected",
			section: "telemetry:\n  capture_content: sometimes\n",
			wantErr: "invalid YAML, field type or unknown field",
		},
		// 环境变量不经过文件级解码，只在字段转换时拒绝无法解析的取值。
		{
			name:    "invalid_capture_content_env_rejected",
			env:     map[string]string{"OPENVAALA_TELEMETRY_CAPTURE_CONTENT": "sometimes"},
			wantErr: "invalid field type",
		},
	}
	// 2. 加载配置，比较各项生效值，或核对错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home, configDirectory := t.TempDir(), t.TempDir()
			path := filepath.Join(configDirectory, "config.yaml")
			if err := os.WriteFile(path, []byte("run:\n  agent_name: assistant\n"+tt.section), 0600); err != nil {
				t.Fatal(err)
			}
			for key, value := range tt.env {
				t.Setenv(key, value)
			}
			cfg, err := config.LoadConfig(&config.LoadConfigOpt{
				Home: home,
				Path: path,
			})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("LoadConfig() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.GetTelemetryLogLevel() != tt.wantLevel || cfg.Telemetry.LogFile != tt.wantLogFile(home, configDirectory) ||
				cfg.Telemetry.OTLPEndpoint != tt.wantEndpoint || !maps.Equal(cfg.Telemetry.OTLPHeaders, tt.wantHeaders) ||
				cfg.Telemetry.CaptureContent != tt.wantCaptureContent {
				t.Fatalf("telemetry = %+v, level %v", *cfg.Telemetry, cfg.GetTelemetryLogLevel())
			}
		})
	}
}

// TestConfigModelDefinitions 验证模型字段覆盖全局默认、全局默认覆盖内置默认，以及推理强度和合并角色的校验。
func TestConfigModelDefinitions(t *testing.T) {
	// 1. 准备全局默认和模型字段的组合。
	ref := defs.ModelRef{
		ProviderID: "local",
		ModelID:    "MyModel",
	}
	tests := []struct {
		name     string
		defaults string // config.yaml 的 models.defaults 段，空表示不写
		model    string // MyModel 下的字段，每行带四个空格缩进
		want     func(t *testing.T, model *defs.ModelDefinition)
		wantErr  string
	}{
		{
			name: "builtin_defaults",
			want: func(t *testing.T, model *defs.ModelDefinition) {
				if model.DisplayName != "MyModel" || model.ContextWindow != 256000 || model.MaxOutputTokens != 32000 || !model.ThinkingEnabled ||
					len(model.ReasoningEfforts) != 0 || model.DefaultReasoningEffort != "" || len(model.MergeMessageRoles) != 0 ||
					model.ReasoningContext != llmkit.ReasoningContextUnknown {
					t.Fatalf("model = %+v", model)
				}
			},
		},
		{
			name:     "global_defaults_override_builtin",
			defaults: "    context_window: 200000\n    max_output_tokens: 8000\n    thinking: false\n    reasoning_efforts: [low, medium, high]\n    default_reasoning_effort: medium\n    merge_message_roles: [user]\n",
			want: func(t *testing.T, model *defs.ModelDefinition) {
				if model.ContextWindow != 200000 || model.MaxOutputTokens != 8000 || model.ThinkingEnabled ||
					!slices.Equal(model.ReasoningEfforts, []string{"low", "medium", "high"}) || model.DefaultReasoningEffort != "medium" ||
					!slices.Equal(model.MergeMessageRoles, []llmkit.Role{llmkit.RoleUser}) {
					t.Fatalf("model = %+v", model)
				}
			},
		},
		{
			name:     "model_fields_override_global",
			defaults: "    reasoning_efforts: [low, medium, high]\n    default_reasoning_effort: medium\n",
			model:    "    display_name: My Model\n    context_window: 100000\n    max_output_tokens: 4000\n    thinking: false\n    reasoning_efforts: [low, high]\n    default_reasoning_effort: high\n    merge_message_roles: [user, assistant]\n",
			want: func(t *testing.T, model *defs.ModelDefinition) {
				if model.DisplayName != "My Model" || model.ContextWindow != 100000 || model.MaxOutputTokens != 4000 || model.ThinkingEnabled ||
					!slices.Equal(model.ReasoningEfforts, []string{"low", "high"}) || model.DefaultReasoningEffort != "high" ||
					!slices.Equal(model.MergeMessageRoles, []llmkit.Role{llmkit.RoleUser, llmkit.RoleAssistant}) {
					t.Fatalf("model = %+v", model)
				}
			},
		},
		{
			name:     "empty_list_overrides_global",
			defaults: "    reasoning_efforts: [low, high]\n    default_reasoning_effort: high\n    merge_message_roles: [user]\n",
			model:    "    reasoning_efforts: []\n    merge_message_roles: []\n",
			want: func(t *testing.T, model *defs.ModelDefinition) {
				if len(model.ReasoningEfforts) != 0 || model.DefaultReasoningEffort != "" || len(model.MergeMessageRoles) != 0 {
					t.Fatalf("model = %+v", model)
				}
			},
		},
		{
			name:     "global_default_not_in_model_list_uses_first",
			defaults: "    default_reasoning_effort: medium\n",
			model:    "    reasoning_efforts: [low, high]\n",
			want: func(t *testing.T, model *defs.ModelDefinition) {
				if model.DefaultReasoningEffort != "low" {
					t.Fatalf("default effort = %q, want low", model.DefaultReasoningEffort)
				}
			},
		},
		{
			name:    "default_not_in_list",
			model:   "    reasoning_efforts: [low]\n    default_reasoning_effort: high\n",
			wantErr: "default_reasoning_effort",
		},
		{
			name:    "default_without_list",
			model:   "    reasoning_efforts: []\n    default_reasoning_effort: low\n",
			wantErr: "default_reasoning_effort",
		},
		{
			name:    "duplicate_effort",
			model:   "    reasoning_efforts: [low, low]\n",
			wantErr: "reasoning_efforts",
		},
		{
			name:    "blank_effort",
			model:   "    reasoning_efforts: ['', low]\n",
			wantErr: "reasoning_efforts",
		},
		{
			name:    "negative_window",
			model:   "    context_window: -1\n",
			wantErr: "context_window",
		},
		{
			name:    "tool_role_rejected",
			model:   "    merge_message_roles: [tool]\n",
			wantErr: "merge_message_roles",
		},
		{
			name:  "reasoning_context_all_turns",
			model: "    reasoning_context: all_turns\n",
			want: func(t *testing.T, model *defs.ModelDefinition) {
				if model.ReasoningContext != llmkit.ReasoningContextAllTurns {
					t.Fatalf("reasoning context = %q, want all_turns", model.ReasoningContext)
				}
			},
		},
		{
			name:  "reasoning_context_current_turn_with_model_protocol",
			model: "    protocol: openai_responses\n    reasoning_context: current_turn\n",
			want: func(t *testing.T, model *defs.ModelDefinition) {
				if model.ReasoningContext != llmkit.ReasoningContextCurrentTurn {
					t.Fatalf("reasoning context = %q, want current_turn", model.ReasoningContext)
				}
			},
		},
		{
			name:    "reasoning_context_unknown_value",
			model:   "    reasoning_context: sometimes\n",
			wantErr: `model MyModel: reasoning_context must be all_turns or current_turn, got "sometimes"`,
		},
		{
			name:    "reasoning_context_rejected_for_openai_chat",
			model:   "    protocol: openai_chat\n    reasoning_context: all_turns\n",
			wantErr: "model MyModel: reasoning_context requires openai_responses protocol",
		},
		{
			name:    "reasoning_context_rejected_for_claude",
			model:   "    protocol: claude\n    reasoning_context: current_turn\n",
			wantErr: "model MyModel: reasoning_context requires openai_responses protocol",
		},
		{
			name:     "global_window_must_be_positive",
			defaults: "    context_window: -5\n",
			wantErr:  "models.defaults",
		},
		{
			name:     "global_role_rejected",
			defaults: "    merge_message_roles: [tool]\n",
			wantErr:  "models.defaults",
		},
	}
	// 2. 加载配置，比较模型定义，或核对错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			directory := writeDefinitionConfig(t)
			configBody := "{}\n"
			if tt.defaults != "" {
				configBody = "models:\n  defaults:\n" + tt.defaults
			}
			modelBody := "id: local\nbase_url: https://example.com\napi_key: secret-value\nmodels:\n  MyModel:\n    upstream_model: upstream\n" + tt.model
			for name, body := range map[string]string{"config.yaml": configBody, "models/local.yaml": modelBody} {
				if err := os.WriteFile(filepath.Join(directory, name), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := config.LoadConfig(&config.LoadConfigOpt{
				Home: directory,
				Path: filepath.Join(directory, "config.yaml"),
			})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || strings.Contains(err.Error(), "secret-value") {
					t.Fatalf("LoadConfig() = %v, want error containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			index := slices.IndexFunc(cfg.GetModels(), func(model *defs.ModelDefinition) bool { return model.Ref == ref })
			if index < 0 {
				t.Fatalf("model %s not in catalog", ref)
			}
			model := cfg.GetModels()[index]
			if model.Target == nil || model.Target.Model != "upstream" || model.Target.APIKey != "secret-value" || model.Target.Protocol != llmkit.ProtocolOpenAIResponses {
				t.Fatalf("target = %+v", model.Target)
			}
			tt.want(t, model)
		})
	}
}

// TestConfigImageSettings 验证 tools.max_image_kb 和模型输入类型：默认值、覆盖顺序、去重和非法值报错。
func TestConfigImageSettings(t *testing.T) {
	// 1. 准备 max_image_kb 和 capabilities.input 的样例。
	ref := defs.ModelRef{
		ProviderID: "local",
		ModelID:    "MyModel",
	}
	tests := []struct {
		name       string
		configBody string // config.yaml 的内容，空表示 {}
		model      string // MyModel 下的字段，每行带四个空格缩进
		// wantMaxImageKB 为 0 时不断言（默认值 500 由 "defaults" 用例覆盖）。
		wantMaxImageKB int
		wantInput      []defs.ModelInput
		wantImage      bool
		wantErr        string
	}{
		{
			name:           "defaults",
			wantMaxImageKB: 500,
			wantInput:      []defs.ModelInput{defs.ModelInputText},
		},
		{
			name:           "max_image_kb_from_file",
			configBody:     "tools:\n  max_image_kb: 800\n",
			wantMaxImageKB: 800,
			wantInput:      []defs.ModelInput{defs.ModelInputText},
		},
		{
			name:      "model_input_with_image",
			model:     "    capabilities:\n      input: [text, image]\n",
			wantInput: []defs.ModelInput{defs.ModelInputText, defs.ModelInputImage},
			wantImage: true,
		},
		{
			name:       "global_input_used_when_model_omits_capabilities",
			configBody: "models:\n  defaults:\n    capabilities:\n      input: [text, image]\n",
			wantInput:  []defs.ModelInput{defs.ModelInputText, defs.ModelInputImage},
			wantImage:  true,
		},
		{
			name:       "model_input_overrides_global",
			configBody: "models:\n  defaults:\n    capabilities:\n      input: [text, image]\n",
			model:      "    capabilities:\n      input: [text]\n",
			wantInput:  []defs.ModelInput{defs.ModelInputText},
		},
		{
			name:       "model_empty_input_overrides_global_with_text",
			configBody: "models:\n  defaults:\n    capabilities:\n      input: [text, image]\n",
			model:      "    capabilities:\n      input: []\n",
			wantInput:  []defs.ModelInput{defs.ModelInputText},
		},
		{
			name:      "duplicates_removed_in_first_seen_order",
			model:     "    capabilities:\n      input: [image, text, image, pdf, text]\n",
			wantInput: []defs.ModelInput{defs.ModelInputImage, defs.ModelInputText, defs.ModelInputPDF},
			wantImage: true,
		},
		{
			name:      "all_known_values_accepted",
			model:     "    capabilities:\n      input: [text, image, audio, video, pdf]\n",
			wantInput: []defs.ModelInput{defs.ModelInputText, defs.ModelInputImage, defs.ModelInputAudio, defs.ModelInputVideo, defs.ModelInputPDF},
			wantImage: true,
		},
		{
			name:    "unknown_model_input_rejected",
			model:   "    capabilities:\n      input: [text, smell]\n",
			wantErr: `model MyModel: unknown input capability "smell"`,
		},
		{
			name:       "unknown_global_input_rejected",
			configBody: "models:\n  defaults:\n    capabilities:\n      input: [smell]\n",
			wantErr:    `models.defaults: unknown input capability "smell"`,
		},
		{
			name:       "zero_max_image_kb_rejected",
			configBody: "tools:\n  max_image_kb: 0\n",
			wantErr:    "tools.max_image_kb must be positive",
		},
		{
			name:       "negative_max_image_kb_rejected",
			configBody: "tools:\n  max_image_kb: -1\n",
			wantErr:    "tools.max_image_kb must be positive",
		},
	}
	// 2. 加载配置，比较 MaxImageKB 和模型输入类型，或核对错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			directory := writeDefinitionConfig(t)
			configBody := cmp.Or(tt.configBody, "{}\n")
			modelBody := "id: local\nbase_url: https://example.com\napi_key: secret-value\nmodels:\n  MyModel:\n    upstream_model: upstream\n" + tt.model
			writeDefinitionFiles(t, directory, map[string]string{"config.yaml": configBody, "models/local.yaml": modelBody})
			cfg, err := config.LoadConfig(&config.LoadConfigOpt{
				Home: directory,
				Path: filepath.Join(directory, "config.yaml"),
			})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("LoadConfig() = %v, want error containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantMaxImageKB != 0 && cfg.Tools.MaxImageKB != tt.wantMaxImageKB {
				t.Fatalf("Tools.MaxImageKB = %d, want %d", cfg.Tools.MaxImageKB, tt.wantMaxImageKB)
			}
			model := findModel(cfg, ref)
			if model == nil {
				t.Fatalf("model %s not in catalog", ref)
			}
			if !slices.Equal(model.Capabilities.Input, tt.wantInput) || model.IsImageInputSupported() != tt.wantImage {
				t.Fatalf("Capabilities.Input = %v, IsImageInputSupported() = %v, want %v, %v",
					model.Capabilities.Input, model.IsImageInputSupported(), tt.wantInput, tt.wantImage)
			}
		})
	}
}

// TestConfigGetModels 验证 GetModels 按 Provider、模型 ID 排序，返回副本，也包括没被引用的模型。
func TestConfigGetModels(t *testing.T) {
	// 1. 再加一个 other Provider，加载配置，准备检查项。
	directory := writeDefinitionConfig(t)
	if err := os.WriteFile(filepath.Join(directory, "models/other.yaml"), []byte("id: other\nbase_url: https://example.org\nmodels:\n  alpha: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(&config.LoadConfigOpt{
		Home: directory,
		Path: filepath.Join(directory, "config.yaml"),
	})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		check func(t *testing.T, models []*defs.ModelDefinition)
	}{
		{
			name: "sorted_by_provider_then_model",
			check: func(t *testing.T, models []*defs.ModelDefinition) {
				var refs []string
				for _, model := range models {
					refs = append(refs, model.Ref.String())
				}
				if !slices.Equal(refs, []string{"local/MyModel", "local/unused", "other/alpha"}) {
					t.Fatalf("refs = %v", refs)
				}
			},
		},
		{
			name: "copies_are_independent",
			check: func(t *testing.T, models []*defs.ModelDefinition) {
				models[0].Target.Model = "changed"
				models[0].ContextWindow = 1
				if again := cfg.GetModels()[0]; again.Target.Model != "MyModel" || again.ContextWindow != 256000 {
					t.Fatal("caller modified the model catalog")
				}
			},
		},
		{
			name: "unreferenced_models_loaded",
			check: func(t *testing.T, models []*defs.ModelDefinition) {
				if models[1].Target.Model != "another-upstream" {
					t.Fatalf("unused model = %+v", models[1].Target)
				}
			},
		},
	}
	// 2. 每个检查项重新取一次 GetModels。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { tt.check(t, cfg.GetModels()) })
	}
}
