package httpclient

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/VaalaCat/openvaala/pkg/defs"
)

// TestHTTPClientHeaders 验证客户端补 header、填写变量和删除取不到值的 header。
func TestHTTPClientHeaders(t *testing.T) {
	// 1. 准备客户端 header、请求 header、ctx 身份和期望收到的 header。
	buildUserAgent := "openvaala/" + defs.GetBuildVersion() + " (" + runtime.GOOS + "; " + runtime.GOARCH + ")"
	tests := []struct {
		name           string
		clientHeaders  map[string]string
		requestHeaders map[string]string
		meta           defs.SessionCtxMeta
		want           map[string]string
		absent         []string
	}{
		{
			name:          "fills_absent_header",
			clientHeaders: map[string]string{"X-Team": "infra"},
			want:          map[string]string{"X-Team": "infra"},
		},
		{
			name:           "request_header_wins",
			clientHeaders:  map[string]string{"X-Team": "infra"},
			requestHeaders: map[string]string{"X-Team": "mine"},
			want:           map[string]string{"X-Team": "mine"},
		},
		{
			name: "fills_session_variables",
			clientHeaders: map[string]string{
				"Session-Id":      "${OPENVAALA_SESSION_ID}",
				"Conversation-Id": "${OPENVAALA_CONVERSATION_ID}",
			},
			meta: defs.SessionCtxMeta{ConversationID: "c1", SessionID: "s1"},
			want: map[string]string{"Session-Id": "s1", "Conversation-Id": "c1"},
		},
		{
			name:          "fills_parent_session_and_agent_name",
			clientHeaders: map[string]string{"X-Parent": "${OPENVAALA_PARENT_SESSION_ID}", "X-Agent": "${OPENVAALA_AGENT_NAME}"},
			meta:          defs.SessionCtxMeta{SessionID: "s2", ParentSessionID: "s1", AgentName: "worker"},
			want:          map[string]string{"X-Parent": "s1", "X-Agent": "worker"},
		},
		{
			name:          "no_meta_drops_header",
			clientHeaders: map[string]string{"Session-Id": "${OPENVAALA_SESSION_ID}"},
			absent:        []string{"Session-Id"},
		},
		{
			name:          "empty_variable_drops_header",
			clientHeaders: map[string]string{"X-Parent": "${OPENVAALA_PARENT_SESSION_ID}"},
			meta:          defs.SessionCtxMeta{SessionID: "s1"},
			absent:        []string{"X-Parent"},
		},
		{
			name:          "empty_value_drops_header",
			clientHeaders: map[string]string{"Session-Id": ""},
			meta:          defs.SessionCtxMeta{SessionID: "s1"},
			absent:        []string{"Session-Id"},
		},
		{
			name:          "any_empty_variable_drops_whole_header",
			clientHeaders: map[string]string{"X-Ids": "${OPENVAALA_SESSION_ID}/${OPENVAALA_PARENT_SESSION_ID}"},
			meta:          defs.SessionCtxMeta{SessionID: "s1"},
			absent:        []string{"X-Ids"},
		},
		{
			name:          "empty_user_agent_not_sent",
			clientHeaders: map[string]string{"User-Agent": ""},
			absent:        []string{"User-Agent"},
		},
		{
			name:          "unfillable_user_agent_not_sent",
			clientHeaders: map[string]string{"User-Agent": "openvaala/${OPENVAALA_SESSION_ID}"},
			absent:        []string{"User-Agent"},
		},
		{
			name:          "fills_build_variables",
			clientHeaders: map[string]string{"User-Agent": "openvaala/${OPENVAALA_VERSION} (${OPENVAALA_OS}; ${OPENVAALA_ARCH})"},
			want:          map[string]string{"User-Agent": buildUserAgent},
		},
		{
			name:           "resolves_request_header_templates",
			requestHeaders: map[string]string{"X-Title": "app-${OPENVAALA_AGENT_NAME}"},
			meta:           defs.SessionCtxMeta{AgentName: "assistant"},
			want:           map[string]string{"X-Title": "app-assistant"},
		},
		{
			name:           "empty_request_header_dropped",
			requestHeaders: map[string]string{"X-Empty": ""},
			absent:         []string{"X-Empty"},
		},
		{
			name:           "unknown_variable_in_request_drops_header",
			requestHeaders: map[string]string{"X-Bad": "${OPENVAALA_FOO}"},
			absent:         []string{"X-Bad"},
		},
		{
			name:          "unknown_variable_in_client_drops_header",
			clientHeaders: map[string]string{"X-Bad": "a-${OPENVAALA_FOO}"},
			absent:        []string{"X-Bad"},
		},
		{
			name:          "lowercase_variable_drops_header",
			clientHeaders: map[string]string{"X-Bad": "${OPENVAALA_session_id}"},
			meta:          defs.SessionCtxMeta{SessionID: "s1"},
			absent:        []string{"X-Bad"},
		},
		{
			name:          "hyphenated_variable_drops_header",
			clientHeaders: map[string]string{"X-Bad": "${OPENVAALA_SESSION-ID}"},
			meta:          defs.SessionCtxMeta{SessionID: "s1"},
			absent:        []string{"X-Bad"},
		},
		{
			name:          "literal_dollar_brace_kept",
			clientHeaders: map[string]string{"X-Literal": "${FOO}"},
			want:          map[string]string{"X-Literal": "${FOO}"},
		},
		{
			name:           "caller_request_unchanged",
			clientHeaders:  map[string]string{"X-Team": "infra"},
			requestHeaders: map[string]string{"X-Title": "${OPENVAALA_AGENT_NAME}"},
			meta:           defs.SessionCtxMeta{AgentName: "a"},
			want:           map[string]string{"X-Team": "infra", "X-Title": "a"},
		},
	}
	// 2. 逐个用例经 httptest 服务器收请求，比较收到的 header，并确认调用方的请求没被改动。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			received := make(chan http.Header, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received <- r.Header.Clone()
			}))
			defer server.Close()

			ctx := defs.ContextWithSessionCtxMeta(context.Background(), tt.meta)
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
			if err != nil {
				t.Fatalf("NewRequestWithContext() error = %v", err)
			}
			for name, value := range tt.requestHeaders {
				request.Header.Set(name, value)
			}
			originalHeader := request.Header.Clone()

			client := NewHTTPClient(NewHTTPClientOpt().WithHeaders(tt.clientHeaders))
			response, err := client.Do(request)
			if err != nil {
				t.Fatalf("Do() error = %v", err)
			}
			_ = response.Body.Close()
			got := <-received

			for name, want := range tt.want {
				if values := got.Values(name); len(values) != 1 || values[0] != want {
					t.Errorf("header %s = %q, want %q", name, values, want)
				}
			}
			for _, name := range tt.absent {
				if values := got.Values(name); values != nil {
					t.Errorf("header %s = %q, want absent", name, values)
				}
			}
			if !maps.EqualFunc(request.Header, originalHeader, slices.Equal) {
				t.Errorf("caller request header = %v, want unchanged %v", request.Header, originalHeader)
			}
		})
	}
}

// roundTripFunc 让测试用函数充当底层 transport。
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

// TestHTTPClientTransport 验证自定义底层 transport 收到的是补全并填好变量的请求副本。
func TestHTTPClientTransport(t *testing.T) {
	// 1. 准备客户端 header、ctx 身份和期望的 Session-Id。
	tests := []struct {
		name          string
		clientHeaders map[string]string
		meta          defs.SessionCtxMeta
		wantSession   string
	}{
		{
			name:          "custom_transport_receives_filled_headers",
			clientHeaders: map[string]string{"Session-Id": "${OPENVAALA_SESSION_ID}"},
			meta:          defs.SessionCtxMeta{SessionID: "s1"},
			wantSession:   "s1",
		},
		{
			name:          "custom_transport_receives_no_unfilled_header",
			clientHeaders: map[string]string{"Session-Id": "${OPENVAALA_SESSION_ID}"},
		},
	}
	// 2. 用函数 transport 记录收到的请求，比较 Session-Id 并确认调用方的请求没被改动。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var seen *http.Request
			transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
				seen = request
				return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: request}, nil
			})
			client := NewHTTPClient(NewHTTPClientOpt().WithHeaders(tt.clientHeaders).WithTransport(transport))

			ctx := defs.ContextWithSessionCtxMeta(context.Background(), tt.meta)
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.invalid/", nil)
			if err != nil {
				t.Fatalf("NewRequestWithContext() error = %v", err)
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatalf("Do() error = %v", err)
			}
			_ = response.Body.Close()

			if seen == nil {
				t.Fatal("custom transport was not called")
			}
			if got := seen.Header.Get("Session-Id"); got != tt.wantSession {
				t.Errorf("Session-Id = %q, want %q", got, tt.wantSession)
			}
			if len(request.Header) != 0 {
				t.Errorf("caller request header = %v, want unchanged", request.Header)
			}
		})
	}
}

// TestHTTPClientRedirect 验证同域重定向照常跟随，跨域重定向默认拒绝、打开后跟随，跳数超限停止。
func TestHTTPClientRedirect(t *testing.T) {
	// 1. 准备重定向路径、是否允许跨域，以及期望的状态、错误和目标服务器收到的请求数。
	tests := []struct {
		name       string
		path       string
		allowed    bool
		wantStatus int
		wantErr    string
		wantBHits  int32
	}{
		{name: "same_origin_followed", path: "/same", wantStatus: http.StatusOK},
		{name: "cross_origin_refused", path: "/cross", wantErr: "refusing cross-origin redirect from http://{A} to http://{B}"},
		{name: "cross_origin_allowed", path: "/cross", allowed: true, wantStatus: http.StatusOK, wantBHits: 1},
		{name: "redirect_loop_stopped", path: "/loop", wantErr: "stopped after 10 redirects"},
		{name: "redirect_loop_stopped_when_cross_origin_allowed", path: "/loop", allowed: true, wantErr: "stopped after 10 redirects"},
	}
	// 2. 起两个服务器 A、B，从 A 发请求并检查结果与 B 收到的请求数。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var bHits atomic.Int32
			serverB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				bHits.Add(1)
			}))
			defer serverB.Close()
			serverA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/same":
					http.Redirect(w, r, "/final", http.StatusFound)
				case "/cross":
					http.Redirect(w, r, serverB.URL, http.StatusFound)
				case "/loop":
					http.Redirect(w, r, "/loop", http.StatusFound)
				}
			}))
			defer serverA.Close()

			opt := NewHTTPClientOpt()
			if tt.allowed {
				opt.WithCrossOriginRedirect()
			}
			response, err := NewHTTPClient(opt).Get(serverA.URL + tt.path)
			if tt.wantErr != "" {
				wantErr := strings.NewReplacer(
					"{A}", strings.TrimPrefix(serverA.URL, "http://"),
					"{B}", strings.TrimPrefix(serverB.URL, "http://"),
				).Replace(tt.wantErr)
				if err == nil || !strings.Contains(err.Error(), wantErr) {
					t.Fatalf("Get() error = %v, want containing %q", err, wantErr)
				}
			} else {
				if err != nil {
					t.Fatalf("Get() error = %v", err)
				}
				defer response.Body.Close()
				if response.StatusCode != tt.wantStatus {
					t.Errorf("status = %d, want %d", response.StatusCode, tt.wantStatus)
				}
			}
			if got := bHits.Load(); got != tt.wantBHits {
				t.Errorf("server B hits = %d, want %d", got, tt.wantBHits)
			}
		})
	}
}

// TestMergeHeaders 验证 header 合并：后者不分大小写覆盖前者，名字统一写法，不改动输入。
func TestMergeHeaders(t *testing.T) {
	// 1. 准备全局、目标 header 和合并后的期望结果。
	tests := []struct {
		name        string
		global      map[string]string
		destination map[string]string
		want        map[string]string
	}{
		{
			name:        "destination_overrides_case_insensitive",
			global:      map[string]string{"Session-Id": "${OPENVAALA_SESSION_ID}"},
			destination: map[string]string{"session-id": ""},
			want:        map[string]string{"Session-Id": ""},
		},
		{
			name:        "keeps_both_distinct_headers",
			global:      map[string]string{"X-Team": "infra"},
			destination: map[string]string{"X-Title": "app"},
			want:        map[string]string{"X-Team": "infra", "X-Title": "app"},
		},
		{
			name:   "keys_canonical",
			global: map[string]string{"x-team": "infra"},
			want:   map[string]string{"X-Team": "infra"},
		},
		{
			name: "nil_inputs",
			want: map[string]string{},
		},
	}
	// 2. 合并后比较结果，并确认输入没被改动。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			globalBefore := maps.Clone(tt.global)
			destinationBefore := maps.Clone(tt.destination)

			got := MergeHeaders(tt.global, tt.destination)

			if !maps.Equal(got, tt.want) {
				t.Errorf("MergeHeaders() = %v, want %v", got, tt.want)
			}
			if !maps.Equal(tt.global, globalBefore) || !maps.Equal(tt.destination, destinationBefore) {
				t.Errorf("MergeHeaders() changed its inputs: global %v, destination %v", tt.global, tt.destination)
			}
		})
	}
}

// TestValidateHeaderTemplate 验证 header 模板里不认识的 OPENVAALA_ 变量会被报告，其余 ${...} 原样放行。
func TestValidateHeaderTemplate(t *testing.T) {
	// 1. 准备模板和期望的错误文本。
	tests := []struct {
		name    string
		value   string
		wantErr string
	}{
		{name: "known_variables", value: "openvaala/${OPENVAALA_VERSION} ${OPENVAALA_AGENT_NAME}"},
		{name: "all_known_variables", value: "${OPENVAALA_VERSION}${OPENVAALA_OS}${OPENVAALA_ARCH}${OPENVAALA_CONVERSATION_ID}${OPENVAALA_SESSION_ID}${OPENVAALA_PARENT_SESSION_ID}${OPENVAALA_AGENT_NAME}"},
		{name: "unknown_variable", value: "${OPENVAALA_SESION_ID}", wantErr: "unknown variable OPENVAALA_SESION_ID"},
		{name: "first_unknown_variable_reported", value: "${OPENVAALA_SESSION_ID} ${OPENVAALA_FOO} ${OPENVAALA_BAR}", wantErr: "unknown variable OPENVAALA_FOO"},
		{name: "lowercase_variable", value: "${OPENVAALA_session_id}", wantErr: "unknown variable OPENVAALA_session_id"},
		{name: "hyphenated_variable", value: "${OPENVAALA_SESSION-ID}", wantErr: "unknown variable OPENVAALA_SESSION-ID"},
		{name: "empty_variable_name", value: "${OPENVAALA_}", wantErr: "unknown variable OPENVAALA_"},
		{name: "non_openvaala_kept", value: "${FOO}"},
		{name: "plain_text", value: "infra"},
		{name: "empty_value", value: ""},
	}
	// 2. 逐个校验并比较错误文本。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateHeaderTemplate(tt.value)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateHeaderTemplate() error = %v, want nil", err)
				}
				return
			}
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("ValidateHeaderTemplate() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
