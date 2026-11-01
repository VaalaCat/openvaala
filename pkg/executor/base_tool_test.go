package executor_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/agent/agent_context"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
)

type greetingArguments struct {
	Name        string                 `json:"name" jsonschema:"description=被问候的名字,minLength=1,example=Vaala,example=小明"`
	Count       int                    `json:"count,omitempty" jsonschema:"minimum=1,maximum=3,default=1"`
	Preferences *greetingPreferences   `json:"preferences,omitempty"`
	Guests      []*greetingPreferences `json:"guests,omitempty"`
}

type greetingPreferences struct {
	Next     *greetingPreferences `json:"next,omitempty"`
	Language string               `json:"language,omitempty" jsonschema:"enum=zh,enum=en,default=zh"`
}

// TestBaseToolArguments 验证参数校验、默认值和多次解析的数据独立性。
func TestBaseToolArguments(t *testing.T) {
	// 1. 创建问候工具并准备各类参数。
	tool, err := executor.NewBaseTool[greetingArguments](&executor.BaseToolOpt{
		Name:        "greet",
		Description: "按指定次数问候用户",
	})
	if err != nil {
		t.Fatal(err)
	}
	tests := []*struct {
		name     string
		input    string
		expected *greetingArguments
	}{
		{
			name:  "required_name_and_default_count",
			input: `{"name":"Vaala"}`,
			expected: &greetingArguments{
				Name:  "Vaala",
				Count: 1,
			},
		},
		{
			name:  "explicit_values",
			input: `{"name":"Alice","count":3}`,
			expected: &greetingArguments{
				Name:  "Alice",
				Count: 3,
			},
		},
		{
			name:  "nested_defaults",
			input: `{"name":"Bob","preferences":{},"guests":[{}, {"language":"en"}]}`,
			expected: &greetingArguments{
				Name:        "Bob",
				Count:       1,
				Preferences: &greetingPreferences{Language: "zh"},
				Guests:      []*greetingPreferences{{Language: "zh"}, {Language: "en"}},
			},
		},
		{
			name:  "missing_required_name",
			input: `{}`,
		},
		{
			name:  "invalid_range",
			input: `{"name":"Vaala","count":0}`,
		},
		// 未声明的顶层字段会被忽略，已声明字段仍接受校验。
		{
			name:  "unknown_field_ignored",
			input: `{"name":"Vaala","extra":true}`,
			expected: &greetingArguments{
				Name:  "Vaala",
				Count: 1,
			},
		},
		{
			name:  "wrong_type",
			input: `{"name":"Vaala","count":"2"}`,
		},
		{
			name:  "invalid_enum",
			input: `{"name":"Vaala","preferences":{"language":"other"}}`,
		},
	}
	// 2. 逐项核对解析结果，再修改结果并重新解析。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			arguments, err := tool.ParseArguments(tt.input)
			if tt.expected == nil {
				if err == nil {
					t.Fatal("invalid arguments accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(arguments, tt.expected) {
				t.Fatalf("arguments = %+v, want %+v", arguments, tt.expected)
			}
			arguments.Count = 99
			if arguments.Preferences != nil {
				arguments.Preferences.Language = "changed"
			}
			again, err := tool.ParseArguments(tt.input)
			if err != nil || !reflect.DeepEqual(again, tt.expected) {
				t.Fatalf("parse state leaked: %+v, %v", again, err)
			}
		})
	}
}

// TestBaseToolDefinition 验证字段标签生成的工具定义及返回副本。
func TestBaseToolDefinition(t *testing.T) {
	// 1. 逐项创建工具，核对名称、说明、必填字段和范围，再确认改动配置和返回的定义都不影响工具。
	tests := []*struct {
		name        string
		description string
	}{
		{
			name:        "greet",
			description: "问候用户",
		},
		{
			name:        "welcome",
			description: "欢迎用户",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opt := &executor.BaseToolOpt{
				Name:        tt.name,
				Description: tt.description,
			}
			tool, err := executor.NewBaseTool[greetingArguments](opt)
			if err != nil {
				t.Fatal(err)
			}
			opt.Name = "changed"
			definition := tool.GetDefinition()
			if definition.Name != tt.name || definition.Description != tt.description {
				t.Fatalf("definition = %+v", definition)
			}
			schema := &struct {
				Required   []string `json:"required"`
				Properties map[string]*struct {
					Description string `json:"description"`
					Default     int    `json:"default"`
					Minimum     int    `json:"minimum"`
					Maximum     int    `json:"maximum"`
				} `json:"properties"`
			}{}
			if err := json.Unmarshal(definition.InputSchema, schema); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(schema.Required, []string{"name"}) {
				t.Fatalf("required = %v", schema.Required)
			}
			if schema.Properties["name"].Description != "被问候的名字" {
				t.Fatal("description missing")
			}
			count := schema.Properties["count"]
			if count.Default != 1 || count.Minimum != 1 || count.Maximum != 3 {
				t.Fatalf("count schema = %+v", count)
			}
			definition.InputSchema[0] = 'x'
			if !json.Valid(tool.GetDefinition().InputSchema) {
				t.Fatal("definition modified BaseTool schema")
			}
		})
	}
}

// TestBaseToolRun 验证同一工具处理不同参数及重复调用的结果。
func TestBaseToolRun(t *testing.T) {
	// 1. 创建问候工具和工具执行器。
	baseTool, err := executor.NewBaseTool[greetingArguments](&executor.BaseToolOpt{
		Name:        "greet",
		Description: "按指定次数问候用户",
	})
	if err != nil {
		t.Fatal(err)
	}
	instance, err := executor.NewToolExecutor(executor.NewToolExecutorOpt(nil, &greetingTool{
		BaseTool: baseTool,
	}))
	if err != nil {
		t.Fatal(err)
	}
	tests := []*struct {
		name     string
		calls    []*defs.ToolCall
		expected map[string][]string
	}{
		{
			name: "two_names_with_independent_parameters",
			calls: []*defs.ToolCall{
				{
					CallID:    "alice",
					Name:      "greet",
					Arguments: `{"name":"Alice"}`,
				},
				{
					CallID:    "bob",
					Name:      "greet",
					Arguments: `{"name":"Bob","count":2}`,
				},
			},
			expected: map[string][]string{
				"alice": {"你好，Alice"},
				"bob":   {"你好，Bob", "你好，Bob"},
			},
		},
		{
			name: "reuse_registered_tool",
			calls: []*defs.ToolCall{
				{
					CallID:    "vaala",
					Name:      "greet",
					Arguments: `{"name":"Vaala","count":3}`,
				},
			},
			expected: map[string][]string{
				"vaala": {"你好，Vaala", "你好，Vaala", "你好，Vaala"},
			},
		},
	}
	// 2. 执行各批调用，收集完成事件并核对问候文本。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			events := make(chan executor.ExecutorEvent)
			var tasks sync.WaitGroup
			tasks.Go(func() { instance.Run(ctx, executorCallItems(tt.calls), events) })
			greetings := make(map[string][]string)
			for event := range events {
				switch event := event.(type) {
				case *executor.ExecutionFinishedEvent:
					if event.Result == nil || event.Result.Error != nil {
						t.Errorf("finished event = %+v", event)
						continue
					}
					var content []string
					if err := json.Unmarshal(event.Result.Content, &content); err != nil {
						t.Error(err)
					}
					greetings[event.Result.CallID] = content
				}
			}
			tasks.Wait()
			if !reflect.DeepEqual(greetings, tt.expected) {
				t.Fatalf("greetings = %#v, want %#v", greetings, tt.expected)
			}
		})
	}
}

type greetingTool struct {
	*executor.BaseTool[greetingArguments]
}

// Execute 解析问候参数并返回指定次数的问候文本。
func (t *greetingTool) Execute(ctx context.Context, call *defs.Item, _ chan<- *executor.ExecutionProgressEvent) *executor.ToolExecutionResult {
	// 1. 创建结果，解析参数并处理参数错误和取消。
	result := &defs.ToolResult{
		CallID:  call.ToolCall.CallID,
		Content: json.RawMessage("null"),
	}
	arguments, err := t.ParseArguments(call.ToolCall.Arguments)
	if err != nil {
		result.Error = &defs.ToolError{
			Code:    "invalid_arguments",
			Message: err.Error(),
		}
		return &executor.ToolExecutionResult{Result: result}
	}
	if err := ctx.Err(); err != nil {
		result.Error = &defs.ToolError{
			Code:    "canceled",
			Message: err.Error(),
		}
		return &executor.ToolExecutionResult{Result: result}
	}
	// 2. 生成问候列表并编码为结果正文。
	greetings := make([]string, arguments.Count)
	for index := range greetings {
		greetings[index] = "你好，" + arguments.Name
	}
	result.Content, _ = json.Marshal(greetings)
	return &executor.ToolExecutionResult{Result: result}
}

// TestBaseToolModelRequest 验证模型请求中包含工具说明和参数样例。
func TestBaseToolModelRequest(t *testing.T) {
	// 1. 为不同工具名称和说明创建模型服务，运行 Agent 并核对请求。
	tests := []*struct {
		name        string
		description string
	}{
		{
			name:        "greet",
			description: "按指定次数问候用户",
		},
		{
			name:        "welcome",
			description: "欢迎指定的用户",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name,
			func(t *testing.T) {
				requests := make(chan json.RawMessage, 1)
				server := httptest.NewServer(http.HandlerFunc(
					func(w http.ResponseWriter, r *http.Request) {
						body, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						requests <- body
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"收到\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
					}))
				defer server.Close()
				baseTool, err := executor.NewBaseTool[greetingArguments](&executor.BaseToolOpt{
					Name:        tt.name,
					Description: tt.description,
				})
				if err != nil {
					t.Fatal(err)
				}
				toolExecutor, err := executor.NewToolExecutor(executor.NewToolExecutorOpt(nil, &greetingTool{BaseTool: baseTool}))
				if err != nil {
					t.Fatal(err)
				}
				model := &defs.ModelDefinition{
					Ref: defs.ModelRef{
						ProviderID: "test",
						ModelID:    "tool-doc-test",
					},
					Target: &llmkit.Target{
						Protocol: llmkit.ProtocolOpenAIChat,
						BaseURL:  server.URL,
						Model:    "tool-doc-test",
					},
				}
				history, err := agent_context.NewAgentContext(agent_context.NewAgentContextOpt(defs.SessionID("tool-doc-test"), defs.TurnID("tool-doc-turn"), llmkit.NewClient(llmkit.ClientOptions{}), model, model))
				if err != nil {
					t.Fatal(err)
				}
				if err := history.Load(&agent_context.ContextLoadParam{System: "使用工具问候用户"}); err != nil {
					t.Fatal(err)
				}
				opt := agent.NewAgentOpt(defs.TurnID("tool-doc-turn"), defs.SessionID("tool-doc-test"), llmkit.NewClient(llmkit.ClientOptions{}), model, history)
				opt.ToolExecutor = toolExecutor
				instance, err := agent.NewAgent(opt)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				if _, err := instance.Run(ctx, nil); err != nil {
					t.Fatal(err)
				}
				request := &struct {
					Tools []*struct {
						Function *struct {
							Name        string `json:"name"`
							Description string `json:"description"`
							Parameters  *struct {
								Properties map[string]*struct {
									Description string   `json:"description"`
									Examples    []string `json:"examples"`
								} `json:"properties"`
							} `json:"parameters"`
						} `json:"function"`
					} `json:"tools"`
				}{}
				select {
				case body := <-requests:
					if err := json.Unmarshal(body, request); err != nil {
						t.Fatal(err)
					}
				default:
					t.Fatal("model did not receive a request")
				}
				if len(request.Tools) != 1 || request.Tools[0].Function == nil {
					t.Fatalf("model tools = %+v", request.Tools)
				}
				function := request.Tools[0].Function
				if function.Name != tt.name || function.Description != tt.description {
					t.Fatalf("model tool name=%q description=%q", function.Name, function.Description)
				}
				if function.Parameters == nil || function.Parameters.Properties["name"] == nil {
					t.Fatal("model received no name parameter")
				}
				nameParameter := function.Parameters.Properties["name"]
				if nameParameter.Description != "被问候的名字" {
					t.Fatalf("model name description = %q", nameParameter.Description)
				}
				if !reflect.DeepEqual(nameParameter.Examples, []string{"Vaala", "小明"}) {
					t.Fatalf("model name examples = %v", nameParameter.Examples)
				}
			})
	}
}
