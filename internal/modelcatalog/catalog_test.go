package modelcatalog_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/internal/modelcatalog"
	"github.com/VaalaCat/openvaala/pkg/defs"
)

// upstreamRequest 是上游 /models 收到的一次请求。
type upstreamRequest struct {
	Method string
	Path   string
	Query  string
	Header http.Header
}

// catalogFixture 用一个 httptest 服务同时提供 /api.json（models.dev）和 /v1/models（上游），记录请求次数和请求头。
type catalogFixture struct {
	server *httptest.Server

	mu                sync.Mutex
	catalogBody       []byte
	catalogStatus     int
	catalogRequests   int
	upstreamIDs       []string
	upstreamBody      string
	upstreamStatus    int
	upstreamRequested []upstreamRequest
}

// newCatalogFixture 启动测试 HTTP 服务：/api.json 默认返回 testdata/api.json，上游 /v1/models 默认没有模型。
func newCatalogFixture(t *testing.T) *catalogFixture {
	t.Helper()
	// 1. 读取 testdata/api.json，启动服务并登记关闭。
	body, err := os.ReadFile(filepath.Join("testdata", "api.json"))
	if err != nil {
		t.Fatal(err)
	}
	fixture := &catalogFixture{
		catalogBody:    body,
		catalogStatus:  http.StatusOK,
		upstreamStatus: http.StatusOK,
	}
	fixture.server = httptest.NewServer(http.HandlerFunc(fixture.handle))
	t.Cleanup(fixture.server.Close)
	return fixture
}

// handle 处理 models.dev 和上游 /models 的测试请求。
func (f *catalogFixture) handle(w http.ResponseWriter, r *http.Request) {
	// 1. 持锁处理：/api.json 计数后按设置的状态码和内容返回；其他路径记下请求，/v1/models 返回 upstreamBody 或由 upstreamIDs 拼出的列表。
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/api.json" {
		f.catalogRequests++
		if f.catalogStatus != http.StatusOK {
			http.Error(w, "catalog unavailable", f.catalogStatus)
			return
		}
		_, _ = w.Write(f.catalogBody)
		return
	}
	f.upstreamRequested = append(f.upstreamRequested, upstreamRequest{
		Method: r.Method,
		Path:   r.URL.Path,
		Query:  r.URL.RawQuery,
		Header: r.Header.Clone(),
	})
	if r.URL.Path != "/v1/models" {
		http.NotFound(w, r)
		return
	}
	if f.upstreamStatus != http.StatusOK {
		http.Error(w, "upstream unavailable", f.upstreamStatus)
		return
	}
	if f.upstreamBody != "" {
		_, _ = w.Write([]byte(f.upstreamBody))
		return
	}
	data := make([]map[string]string, 0, len(f.upstreamIDs))
	for _, id := range f.upstreamIDs {
		data = append(data, map[string]string{"id": id})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

// setCatalogResponse 设置 /api.json 的响应。
func (f *catalogFixture) setCatalogResponse(status int, body []byte) {
	// 1. 持锁更新状态码，body 非 nil 时替换响应内容。
	f.mu.Lock()
	defer f.mu.Unlock()
	f.catalogStatus = status
	if body != nil {
		f.catalogBody = body
	}
}

// getCatalogRequests 返回 /api.json 被请求的次数。
func (f *catalogFixture) getCatalogRequests() int {
	// 1. 持锁读取计数。
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.catalogRequests
}

// getUpstreamRequests 返回上游收到的请求记录。
func (f *catalogFixture) getUpstreamRequests() []upstreamRequest {
	// 1. 持锁复制上游请求列表。
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]upstreamRequest(nil), f.upstreamRequested...)
}

// newCatalog 创建指向测试服务的 Catalog。
func (f *catalogFixture) newCatalog(t *testing.T, cacheFile string) *modelcatalog.Catalog {
	t.Helper()
	// 1. 用测试服务地址、HTTP 客户端和 cacheFile 创建，内存里还没有数据。
	catalog, err := modelcatalog.NewCatalog(modelcatalog.NewCatalogOpt(cacheFile).
		WithSourceURL(f.server.URL + "/api.json").
		WithHTTPClient(f.server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

// newCacheFile 返回临时目录下的缓存文件路径，目录还没创建。
func newCacheFile(t *testing.T) string {
	t.Helper()
	// 1. 路径是 <临时目录>/openvaala/models-dev.json。
	return filepath.Join(t.TempDir(), "openvaala", "models-dev.json")
}

// ptr 返回值的指针。
func ptr[T any](value T) *T {
	// 1. 返回传入值的地址。
	return &value
}

// toJSON 把值编码成 JSON 文本。
func toJSON(t *testing.T, value any) string {
	t.Helper()
	// 1. 编码失败时结束测试。
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// TestNewCatalog 验证 NewCatalog 校验配置，创建时不碰缓存目录也不下载。
func TestNewCatalog(t *testing.T) {
	// 1. 准备合法配置、空缓存路径和 nil 配置。
	tests := []struct {
		name    string
		opt     func(cacheFile string) *modelcatalog.CatalogOpt
		wantErr string
	}{
		{
			name: "valid_options",
			opt:  func(cacheFile string) *modelcatalog.CatalogOpt { return modelcatalog.NewCatalogOpt(cacheFile) },
		},
		{
			name:    "empty_cache_file",
			opt:     func(string) *modelcatalog.CatalogOpt { return modelcatalog.NewCatalogOpt("") },
			wantErr: "cache file",
		},
		{
			name:    "nil_options",
			opt:     func(string) *modelcatalog.CatalogOpt { return nil },
			wantErr: "options",
		},
	}
	// 2. 创建 Catalog，比较返回值和错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newCatalogFixture(t)
			cacheDirectory := filepath.Join(t.TempDir(), "cache")
			opt := tt.opt(filepath.Join(cacheDirectory, "models-dev.json"))
			if opt != nil && opt.CacheFile != "" {
				opt.WithSourceURL(fixture.server.URL + "/api.json")
			}
			catalog, err := modelcatalog.NewCatalog(opt)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || catalog != nil {
					t.Fatalf("NewCatalog() = %v, %v, want error containing %q", catalog, err, tt.wantErr)
				}
				return
			}
			if err != nil || catalog == nil {
				t.Fatalf("NewCatalog() = %v, %v", catalog, err)
			}
			if _, statErr := os.Stat(cacheDirectory); !os.IsNotExist(statErr) {
				t.Fatalf("NewCatalog touched the cache directory: %v", statErr)
			}
			if requests := fixture.getCatalogRequests(); requests != 0 {
				t.Fatalf("NewCatalog requested the catalog %d times", requests)
			}
		})
	}
}

// TestCatalogListCatalogProviders 验证只列出能对上协议和 base_url 的服务：协议按 SDK 包定，OpenAI 和 Anthropic 有内置地址，SDK 对不上的模型不计数。
func TestCatalogListCatalogProviders(t *testing.T) {
	// 1. 准备 models.dev 数据样例。
	tests := []struct {
		name string
		// catalogBody 为空时用 testdata/api.json。
		catalogBody string
		want        []defs.CatalogProvider
	}{
		{
			// testdata 里的 googly（SDK 对应不上）、no-api（没有 api）、templated（api 带 ${HOST} 占位符）和 vote-* 都不出现。
			name: "protocol_by_npm_and_builtin_base_urls",
			want: []defs.CatalogProvider{
				{
					ID:         "anthropic",
					Name:       "Anthropic",
					Protocol:   "claude",
					BaseURL:    "https://api.anthropic.com/v1",
					ModelCount: 1,
				},
				{
					ID:         "compat",
					Name:       "Compat Gateway",
					Protocol:   "openai_chat",
					BaseURL:    "https://compat.example/v1",
					ModelCount: 9,
				},
				{
					ID:         "openai",
					Name:       "OpenAI",
					Protocol:   "openai_responses",
					BaseURL:    "https://api.openai.com/v1",
					ModelCount: 1,
				},
			},
		},
		{
			name:        "explicit_api_wins_over_builtin_base_url",
			catalogBody: `{"openai":{"name":"OpenAI","npm":"@ai-sdk/openai","api":"https://proxy.example/v1","models":{"m":{"name":"M"}}}}`,
			want: []defs.CatalogProvider{
				{
					ID:         "openai",
					Name:       "OpenAI",
					Protocol:   "openai_responses",
					BaseURL:    "https://proxy.example/v1",
					ModelCount: 1,
				},
			},
		},
		{
			name:        "services_without_usable_base_url_or_protocol_are_hidden",
			catalogBody: `{"other":{"name":"Other","npm":"@ai-sdk/openai-compatible","models":{"m":{}}},"cloud":{"name":"Cloud","npm":"@ai-sdk/google","api":"https://cloud.example","models":{"m":{}}},"bare":{"name":"Bare","models":{"m":{}}}}`,
			want:        []defs.CatalogProvider{},
		},
		{
			name:        "models_with_unknown_sdk_are_not_counted",
			catalogBody: `{"mixed":{"name":"Mixed","npm":"@ai-sdk/openai-compatible","api":"https://mixed.example/v1","models":{"a":{},"b":{"provider":{"npm":"@ai-sdk/google"}},"c":{"provider":{"npm":"@ai-sdk/anthropic"}}}}}`,
			want: []defs.CatalogProvider{
				{
					ID:         "mixed",
					Name:       "Mixed",
					Protocol:   "openai_chat",
					BaseURL:    "https://mixed.example/v1",
					ModelCount: 2,
				},
			},
		},
	}
	// 2. 列出服务，比较服务、模型数和顺序。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newCatalogFixture(t)
			if tt.catalogBody != "" {
				fixture.setCatalogResponse(http.StatusOK, []byte(tt.catalogBody))
			}
			got, err := fixture.newCatalog(t, newCacheFile(t)).ListCatalogProviders(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ListCatalogProviders() = %s, want %s", toJSON(t, got), toJSON(t, tt.want))
			}
		})
	}
}

// TestCatalogListCatalogModels 验证 ListCatalogModels 转出的模型配置：字段、推理强度、配置键和模型协议；不能列出的服务报错。
func TestCatalogListCatalogModels(t *testing.T) {
	// 1. 准备服务 ID 和期望的模型配置。
	tests := []struct {
		name       string
		providerID string
		want       map[string]*defs.ProviderModelConfig
		wantErr    string
	}{
		{
			name:       "fields_keys_and_protocols",
			providerID: "compat",
			want: map[string]*defs.ProviderModelConfig{
				// modalities.input 的值写进 capabilities.input；没写 modalities 的模型（如 no-limit）留空。
				"plain": {
					DisplayName:     "Plain Model",
					ContextWindow:   128000,
					MaxOutputTokens: 8192,
					Thinking:        ptr(false),
					Capabilities:    defs.ProviderModelCapabilities{Input: defs.OptionalStringList{"text"}},
				},
				"think-medium": {
					DisplayName:            "Think Medium",
					ContextWindow:          200000,
					MaxOutputTokens:        64000,
					Thinking:               ptr(true),
					ReasoningEfforts:       defs.OptionalStringList{"minimal", "low", "medium", "high"},
					DefaultReasoningEffort: "medium",
					Capabilities:           defs.ProviderModelCapabilities{Input: defs.OptionalStringList{"text", "image", "pdf"}},
				},
				// efforts 里的 null 被去掉，没有 medium 时默认取中间一项。
				"think-no-medium": {
					DisplayName:            "Think No Medium",
					ContextWindow:          256000,
					MaxOutputTokens:        32000,
					Thinking:               ptr(true),
					ReasoningEfforts:       defs.OptionalStringList{"low", "high", "max"},
					DefaultReasoningEffort: "high",
				},
				"only-null-efforts": {
					DisplayName:     "Only Null Efforts",
					ContextWindow:   64000,
					MaxOutputTokens: 4096,
					Thinking:        ptr(true),
				},
				// 缺 limit 的模型窗口和输出上限留空。
				"no-limit": {
					DisplayName: "No Limit",
					Thinking:    ptr(false),
				},
				// id 含文件名不允许的字符时换 key，撞名的后出现者加 -2。
				"a-b-c": {
					UpstreamModel:   "a/b:c",
					DisplayName:     "Slash Colon",
					ContextWindow:   1000,
					MaxOutputTokens: 100,
					Thinking:        ptr(false),
				},
				"a-b-c-2": {
					UpstreamModel:   "a:b:c",
					DisplayName:     "Colon Colon",
					ContextWindow:   2000,
					MaxOutputTokens: 200,
					Thinking:        ptr(false),
				},
				// 模型自己的 SDK 和服务不同时写模型协议；SDK 对应不上的模型 unknown-sdk 不出现。
				"claude-style": {
					Protocol:        "claude",
					DisplayName:     "Claude Style",
					ContextWindow:   300000,
					MaxOutputTokens: 16000,
					Thinking:        ptr(false),
				},
				// modalities.input 里不认识的值不写进 capabilities.input，其余保持原顺序。
				"same-sdk": {
					DisplayName:     "Same SDK",
					ContextWindow:   4000,
					MaxOutputTokens: 400,
					Thinking:        ptr(false),
					Capabilities:    defs.ProviderModelCapabilities{Input: defs.OptionalStringList{"image", "text"}},
				},
			},
		},
		{
			name:       "openai_efforts_without_null",
			providerID: "openai",
			want: map[string]*defs.ProviderModelConfig{
				"gpt-test": {
					DisplayName:            "GPT Test",
					ContextWindow:          400000,
					MaxOutputTokens:        128000,
					Thinking:               ptr(true),
					ReasoningEfforts:       defs.OptionalStringList{"low", "medium", "high"},
					DefaultReasoningEffort: "medium",
				},
			},
		},
		{
			name:       "budget_tokens_options_give_no_efforts",
			providerID: "anthropic",
			want: map[string]*defs.ProviderModelConfig{
				"claude-test": {
					DisplayName:     "Claude Test",
					ContextWindow:   200000,
					MaxOutputTokens: 64000,
					Thinking:        ptr(true),
				},
			},
		},
		{
			name:       "unlisted_service_with_unknown_sdk",
			providerID: "googly",
			wantErr:    "catalog provider googly is not available",
		},
		{
			name:       "unlisted_service_without_api",
			providerID: "no-api",
			wantErr:    "catalog provider no-api is not available",
		},
		{
			name:       "unlisted_service_with_env_placeholder_in_api",
			providerID: "templated",
			wantErr:    "catalog provider templated is not available",
		},
		{
			name:       "missing_service",
			providerID: "missing",
			wantErr:    "catalog provider missing is not available",
		},
	}
	// 2. 列出服务的模型，比较结果或错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newCatalogFixture(t)
			got, err := fixture.newCatalog(t, newCacheFile(t)).ListCatalogModels(context.Background(), tt.providerID)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("ListCatalogModels() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ListCatalogModels() = %s, want %s", toJSON(t, got), toJSON(t, tt.want))
			}
		})
	}
}

// TestCatalogFetchProviderModels 验证用上游模型 ID 去 models.dev 补字段：同名多条时选上限最常见的一组，找不到时按 / 后面的部分再找，配置键不重名。
func TestCatalogFetchProviderModels(t *testing.T) {
	// 1. 准备上游模型 ID 和期望的模型配置。
	tests := []struct {
		name        string
		upstreamIDs []string
		want        map[string]*defs.ProviderModelConfig
	}{
		{
			// 三个服务里：vote-b、vote-c 是 (1000,100)，vote-a 是 (2000,200)；取人数多的一组，显示名取组内按服务 id 排序的第一个。
			name:        "majority_group_wins",
			upstreamIDs: []string{"shared-majority"},
			want: map[string]*defs.ProviderModelConfig{
				"shared-majority": {
					DisplayName:            "Majority From B",
					ContextWindow:          1000,
					MaxOutputTokens:        100,
					Thinking:               ptr(true),
					ReasoningEfforts:       defs.OptionalStringList{"low", "medium", "high"},
					DefaultReasoningEffort: "medium",
				},
			},
		},
		{
			name:        "tie_takes_smaller_context",
			upstreamIDs: []string{"shared-tie"},
			want: map[string]*defs.ProviderModelConfig{
				"shared-tie": {
					DisplayName:     "Tie Small From B",
					ContextWindow:   1000,
					MaxOutputTokens: 100,
					Thinking:        ptr(false),
				},
			},
		},
		{
			name:        "service_without_limit_does_not_vote",
			upstreamIDs: []string{"shared-no-limit"},
			want: map[string]*defs.ProviderModelConfig{
				"shared-no-limit": {
					DisplayName:     "Limit From B",
					ContextWindow:   500,
					MaxOutputTokens: 50,
					Thinking:        ptr(false),
				},
			},
		},
		{
			name:        "exact_id_with_rewritten_key",
			upstreamIDs: []string{"a/b:c"},
			want: map[string]*defs.ProviderModelConfig{
				"a-b-c": {
					UpstreamModel:   "a/b:c",
					DisplayName:     "Slash Colon",
					ContextWindow:   1000,
					MaxOutputTokens: 100,
					Thinking:        ptr(false),
				},
			},
		},
		{
			name:        "falls_back_to_part_after_last_slash",
			upstreamIDs: []string{"vendor/known-id"},
			want: map[string]*defs.ProviderModelConfig{
				"vendor-known-id": {
					UpstreamModel:          "vendor/known-id",
					DisplayName:            "Known ID",
					ContextWindow:          3000,
					MaxOutputTokens:        300,
					Thinking:               ptr(true),
					ReasoningEfforts:       defs.OptionalStringList{"low", "medium", "high"},
					DefaultReasoningEffort: "medium",
				},
			},
		},
		{
			name:        "unknown_ids_only_have_keys",
			upstreamIDs: []string{"totally-unknown", "x/unknown"},
			want: map[string]*defs.ProviderModelConfig{
				"totally-unknown": {},
				"x-unknown":       {UpstreamModel: "x/unknown"},
			},
		},
		{
			name:        "colliding_keys_get_numeric_suffixes",
			upstreamIDs: []string{"p:q", "p q", "p/q"},
			want: map[string]*defs.ProviderModelConfig{
				"p-q":   {UpstreamModel: "p q"},
				"p-q-2": {UpstreamModel: "p/q"},
				"p-q-3": {UpstreamModel: "p:q"},
			},
		},
		{
			name:        "duplicate_and_empty_ids_are_ignored",
			upstreamIDs: []string{"totally-unknown", "", "totally-unknown"},
			want: map[string]*defs.ProviderModelConfig{
				"totally-unknown": {},
			},
		},
	}
	// 2. 拉取模型，比较补完字段后的配置。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newCatalogFixture(t)
			fixture.upstreamIDs = tt.upstreamIDs
			provider := defs.ProviderConfig{
				ID:       "custom",
				Protocol: "openai_chat",
				BaseURL:  fixture.server.URL + "/v1",
				APIKey:   "k",
			}
			got, err := fixture.newCatalog(t, newCacheFile(t)).FetchProviderModels(context.Background(), provider)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("FetchProviderModels() = %s, want %s", toJSON(t, got), toJSON(t, tt.want))
			}
		})
	}
}

// TestCatalogFetchProviderModelsUpstreamRequest 验证请求上游 /models 的方法、路径、查询参数和认证头随协议变化，Provider 的 headers 会发出并覆盖同名认证头。
func TestCatalogFetchProviderModelsUpstreamRequest(t *testing.T) {
	// 1. 准备协议、地址、Provider headers 和认证样例。
	tests := []struct {
		name            string
		protocol        string
		baseURLTail     string
		providerHeaders map[string]string
		wantQuery       string
		wantHeaders     map[string]string
		wantAbsent      []string
	}{
		{
			name:        "openai_chat_uses_bearer_token",
			protocol:    "openai_chat",
			wantHeaders: map[string]string{"Authorization": "Bearer k"},
			wantAbsent:  []string{"X-Api-Key", "Anthropic-Version"},
		},
		{
			name:        "openai_responses_uses_bearer_token",
			protocol:    "openai_responses",
			wantHeaders: map[string]string{"Authorization": "Bearer k"},
			wantAbsent:  []string{"X-Api-Key", "Anthropic-Version"},
		},
		{
			name:        "omitted_protocol_defaults_to_openai_responses",
			protocol:    "",
			wantHeaders: map[string]string{"Authorization": "Bearer k"},
			wantAbsent:  []string{"X-Api-Key", "Anthropic-Version"},
		},
		{
			name:        "claude_uses_api_key_and_version",
			protocol:    "claude",
			wantQuery:   "limit=1000",
			wantHeaders: map[string]string{"X-Api-Key": "k", "Anthropic-Version": "2023-06-01"},
			wantAbsent:  []string{"Authorization"},
		},
		{
			name:        "trailing_slash_in_base_url_does_not_double_slash",
			protocol:    "openai_chat",
			baseURLTail: "/",
			wantHeaders: map[string]string{"Authorization": "Bearer k"},
		},
		{
			name:            "provider_headers_sent_and_override_auth",
			protocol:        "openai_chat",
			providerHeaders: map[string]string{"X-Title": "MyTool", "Authorization": "Bearer override"},
			wantHeaders:     map[string]string{"X-Title": "MyTool", "Authorization": "Bearer override"},
		},
		{
			name:            "provider_headers_override_claude_auth",
			protocol:        "claude",
			providerHeaders: map[string]string{"X-Api-Key": "override"},
			wantQuery:       "limit=1000",
			wantHeaders:     map[string]string{"X-Api-Key": "override", "Anthropic-Version": "2023-06-01"},
		},
	}
	// 2. 拉取模型，比较上游收到的那次请求。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newCatalogFixture(t)
			fixture.upstreamIDs = []string{"some-model"}
			provider := defs.ProviderConfig{
				ID:       "custom",
				Protocol: tt.protocol,
				BaseURL:  fixture.server.URL + "/v1" + tt.baseURLTail,
				APIKey:   "k",
				Headers:  tt.providerHeaders,
			}
			if _, err := fixture.newCatalog(t, newCacheFile(t)).FetchProviderModels(context.Background(), provider); err != nil {
				t.Fatal(err)
			}
			requests := fixture.getUpstreamRequests()
			if len(requests) != 1 {
				t.Fatalf("upstream requests = %d, want 1", len(requests))
			}
			request := requests[0]
			if request.Method != http.MethodGet || request.Path != "/v1/models" || request.Query != tt.wantQuery {
				t.Fatalf("upstream request = %s %s?%s, want GET /v1/models?%s", request.Method, request.Path, request.Query, tt.wantQuery)
			}
			for name, want := range tt.wantHeaders {
				if got := request.Header.Get(name); got != want {
					t.Fatalf("header %s = %q, want %q", name, got, want)
				}
			}
			for _, name := range tt.wantAbsent {
				if got := request.Header.Get(name); got != "" {
					t.Fatalf("header %s = %q, want absent", name, got)
				}
			}
		})
	}
}

// TestCatalogFetchProviderModelsUpstreamFailure 验证上游 /models 出错、返回非法 JSON 或超过大小上限时报错，错误带 base_url。
func TestCatalogFetchProviderModelsUpstreamFailure(t *testing.T) {
	// 1. 准备 HTTP 失败、非法 JSON 和超限上游响应。
	tests := []struct {
		name           string
		upstreamStatus int
		upstreamBody   string
		wantContains   []string
	}{
		{
			name:           "server_error",
			upstreamStatus: http.StatusInternalServerError,
			wantContains:   []string{"fetch models from ", "/v1: ", "500"},
		},
		{
			name:           "invalid_json",
			upstreamStatus: http.StatusOK,
			upstreamBody:   "not json",
			wantContains:   []string{"fetch models from ", "/v1: "},
		},
		{
			// 响应体超过 8MB 的部分读不到；去掉上限时这是合法 JSON，会成功。
			name:           "body_over_size_limit",
			upstreamStatus: http.StatusOK,
			upstreamBody:   `{"data":[{"id":"` + strings.Repeat("a", 9<<20) + `"}]}`,
			wantContains:   []string{"fetch models from ", "/v1: ", "decode response"},
		},
	}
	// 2. 拉取模型，核对返回 nil 和错误内容。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newCatalogFixture(t)
			fixture.upstreamStatus = tt.upstreamStatus
			fixture.upstreamBody = tt.upstreamBody
			provider := defs.ProviderConfig{
				ID:       "custom",
				Protocol: "openai_chat",
				BaseURL:  fixture.server.URL + "/v1",
				APIKey:   "k",
			}
			got, err := fixture.newCatalog(t, newCacheFile(t)).FetchProviderModels(context.Background(), provider)
			if err == nil || got != nil {
				t.Fatalf("FetchProviderModels() = %v, %v, want error", got, err)
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

// TestCatalogFetchProviderModelsWithoutCatalog 验证 models.dev 下载失败时，上游模型只按 ID 列出。
func TestCatalogFetchProviderModelsWithoutCatalog(t *testing.T) {
	// 1. 让 /api.json 返回 500 后拉取模型，核对只有配置键。
	fixture := newCatalogFixture(t)
	fixture.setCatalogResponse(http.StatusInternalServerError, nil)
	fixture.upstreamIDs = []string{"known-id", "p/q"}
	provider := defs.ProviderConfig{
		ID:       "custom",
		Protocol: "openai_chat",
		BaseURL:  fixture.server.URL + "/v1",
		APIKey:   "k",
	}
	got, err := fixture.newCatalog(t, newCacheFile(t)).FetchProviderModels(context.Background(), provider)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]*defs.ProviderModelConfig{
		"known-id": {},
		"p-q":      {UpstreamModel: "p/q"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FetchProviderModels() = %s, want %s", toJSON(t, got), toJSON(t, want))
	}
}

// TestCatalogCache 验证 models.dev 数据的内存缓存和缓存文件：不满 24 小时不重新下载，过期或损坏时下载，下载失败时用旧缓存。
func TestCatalogCache(t *testing.T) {
	// 1. 准备新的、过期的和损坏的缓存文件，以及下载的响应。
	const cacheMaxAge = 24 * time.Hour
	tests := []struct {
		name string
		// cacheContent 非 nil 时在调用前写入缓存文件，mtime 为 cacheAge 之前。
		cacheContent []byte
		cacheAge     time.Duration
		// catalogStatus、catalogBody 是服务端的响应；catalogBody 为空时用 testdata/api.json。
		catalogStatus int
		catalogBody   []byte
		callTwice     bool
		// removeCacheAfterFirstCall 在第一次调用后删掉缓存文件，让第二次调用只能用内存里的数据。
		removeCacheAfterFirstCall bool

		wantErr      string
		wantProvider int
		wantRequests int
		// wantCache 非空时断言缓存文件的内容；wantFreshCache 断言缓存文件被这次调用刷新过。
		wantCache      []byte
		wantFreshCache bool
	}{
		{
			name:           "download_writes_cache_file",
			catalogStatus:  http.StatusOK,
			callTwice:      true,
			wantProvider:   3,
			wantRequests:   1,
			wantFreshCache: true,
		},
		{
			name:                      "fresh_memory_data_needs_neither_cache_file_nor_download",
			catalogStatus:             http.StatusOK,
			callTwice:                 true,
			removeCacheAfterFirstCall: true,
			wantProvider:              3,
			wantRequests:              1,
		},
		{
			name:          "fresh_cache_file_needs_no_download",
			cacheContent:  []byte(`{"openai":{"name":"OpenAI","npm":"@ai-sdk/openai","models":{}}}`),
			cacheAge:      time.Hour,
			catalogStatus: http.StatusInternalServerError,
			wantProvider:  1,
			wantRequests:  0,
		},
		{
			name:           "stale_cache_is_replaced_by_download",
			cacheContent:   []byte(`{}`),
			cacheAge:       cacheMaxAge + time.Hour,
			catalogStatus:  http.StatusOK,
			wantProvider:   3,
			wantRequests:   1,
			wantFreshCache: true,
		},
		{
			name:          "stale_cache_is_used_when_download_fails",
			cacheContent:  []byte(`{"openai":{"name":"OpenAI","npm":"@ai-sdk/openai","models":{}}}`),
			cacheAge:      cacheMaxAge + time.Hour,
			catalogStatus: http.StatusInternalServerError,
			wantProvider:  1,
			wantRequests:  1,
			wantCache:     []byte(`{"openai":{"name":"OpenAI","npm":"@ai-sdk/openai","models":{}}}`),
		},
		{
			// 改用旧缓存的数据按刚取到处理：第二次调用不再请求；缓存文件本身不动。
			name:          "failed_download_marks_old_cache_data_as_loaded_now",
			cacheContent:  []byte(`{"openai":{"name":"OpenAI","npm":"@ai-sdk/openai","models":{}}}`),
			cacheAge:      cacheMaxAge + time.Hour,
			catalogStatus: http.StatusInternalServerError,
			callTwice:     true,
			wantProvider:  1,
			wantRequests:  1,
			wantCache:     []byte(`{"openai":{"name":"OpenAI","npm":"@ai-sdk/openai","models":{}}}`),
		},
		{
			name:          "invalid_download_keeps_stale_cache",
			cacheContent:  []byte(`{"openai":{"name":"OpenAI","npm":"@ai-sdk/openai","models":{}}}`),
			cacheAge:      cacheMaxAge + time.Hour,
			catalogStatus: http.StatusOK,
			catalogBody:   []byte("not json"),
			wantProvider:  1,
			wantRequests:  1,
			wantCache:     []byte(`{"openai":{"name":"OpenAI","npm":"@ai-sdk/openai","models":{}}}`),
		},
		{
			// 响应体超过 64MB 的部分读不到；去掉上限时这是合法 JSON，会成功。
			name:          "download_over_size_limit_is_rejected",
			catalogStatus: http.StatusOK,
			catalogBody:   []byte(`{"big":{"name":"` + strings.Repeat("a", 65<<20) + `"}}`),
			wantErr:       "cannot load models.dev catalog",
			wantRequests:  1,
		},
		{
			name:          "no_cache_and_download_fails",
			catalogStatus: http.StatusInternalServerError,
			wantErr:       "cannot load models.dev catalog",
			wantRequests:  1,
		},
		{
			name:           "corrupt_fresh_cache_is_replaced_by_download",
			cacheContent:   []byte("not json"),
			cacheAge:       time.Hour,
			catalogStatus:  http.StatusOK,
			wantProvider:   3,
			wantRequests:   1,
			wantFreshCache: true,
		},
		{
			name:          "corrupt_stale_cache_and_download_fails",
			cacheContent:  []byte("not json"),
			cacheAge:      cacheMaxAge + time.Hour,
			catalogStatus: http.StatusInternalServerError,
			wantErr:       "cannot load models.dev catalog",
			wantRequests:  1,
		},
	}
	// 2. 按用例调用一到两次，比较结果、下载次数和缓存文件。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newCatalogFixture(t)
			fixture.setCatalogResponse(tt.catalogStatus, tt.catalogBody)
			cacheFile := newCacheFile(t)
			if tt.cacheContent != nil {
				if err := os.MkdirAll(filepath.Dir(cacheFile), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(cacheFile, tt.cacheContent, 0600); err != nil {
					t.Fatal(err)
				}
				modifiedAt := time.Now().Add(-tt.cacheAge)
				if err := os.Chtimes(cacheFile, modifiedAt, modifiedAt); err != nil {
					t.Fatal(err)
				}
			}
			catalog := fixture.newCatalog(t, cacheFile)
			calls := 1
			if tt.callTwice {
				calls = 2
			}
			for call := range calls {
				providers, err := catalog.ListCatalogProviders(context.Background())
				if tt.wantErr != "" {
					if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
						t.Fatalf("ListCatalogProviders() error = %v, want containing %q", err, tt.wantErr)
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				if len(providers) != tt.wantProvider {
					t.Fatalf("providers = %s, want %d of them", toJSON(t, providers), tt.wantProvider)
				}
				if tt.removeCacheAfterFirstCall && call == 0 {
					if err = os.Remove(cacheFile); err != nil {
						t.Fatal(err)
					}
				}
			}
			if requests := fixture.getCatalogRequests(); requests != tt.wantRequests {
				t.Fatalf("catalog requests = %d, want %d", requests, tt.wantRequests)
			}
			if tt.wantCache != nil {
				content, err := os.ReadFile(cacheFile)
				if err != nil || string(content) != string(tt.wantCache) {
					t.Fatalf("cache file = %q, %v, want %q", content, err, tt.wantCache)
				}
			}
			if !tt.wantFreshCache {
				return
			}
			info, err := os.Stat(cacheFile)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0600 || time.Since(info.ModTime()) > time.Minute {
				t.Fatalf("cache file mode = %v, modified %v ago, want 0600 and just written", info.Mode().Perm(), time.Since(info.ModTime()))
			}
			directory, err := os.Stat(filepath.Dir(cacheFile))
			if err != nil || directory.Mode().Perm() != 0700 {
				t.Fatalf("cache directory = %v, %v, want mode 0700", directory, err)
			}
			content, err := os.ReadFile(cacheFile)
			if err != nil || !json.Valid(content) || !strings.Contains(string(content), "Compat Gateway") {
				t.Fatalf("cache file does not hold the downloaded catalog: %v", err)
			}
		})
	}
}
