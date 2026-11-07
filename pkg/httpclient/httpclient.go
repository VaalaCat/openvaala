package httpclient

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/VaalaCat/openvaala/pkg/defs"
)

// HTTPClientOpt 是统一客户端的配置，构造后只读。
type HTTPClientOpt struct {
	// Headers 是请求里没有同名 header 时补上的模板，值可含 ${OPENVAALA_*}。
	Headers map[string]string
	// Transport 为 nil 时用 http.DefaultTransport；MCP 传入走 Executor 拨号和代理的 transport。
	Transport http.RoundTripper
	// IsCrossOriginRedirectAllowed 是否允许跳到别的域名？默认不允许；只有图片下载打开。
	IsCrossOriginRedirectAllowed bool
}

// NewHTTPClientOpt 创建不带客户端级 header、使用 http.DefaultTransport 且拒绝跨域重定向的配置。
func NewHTTPClientOpt() *HTTPClientOpt {
	// 1. 返回零值配置。
	return &HTTPClientOpt{}
}

// WithHeaders 设置请求里没有同名 header 时补上的模板并返回本配置。
func (opt *HTTPClientOpt) WithHeaders(headers map[string]string) *HTTPClientOpt {
	// 1. 保存 header 模板。
	opt.Headers = headers
	return opt
}

// WithTransport 设置底层 transport 并返回本配置。
func (opt *HTTPClientOpt) WithTransport(transport http.RoundTripper) *HTTPClientOpt {
	// 1. 保存底层 transport。
	opt.Transport = transport
	return opt
}

// WithCrossOriginRedirect 允许跳到别的域名并返回本配置。
func (opt *HTTPClientOpt) WithCrossOriginRedirect() *HTTPClientOpt {
	// 1. 打开跨域重定向。
	opt.IsCrossOriginRedirectAllowed = true
	return opt
}

// NewHTTPClient 创建发外部请求的客户端：Transport 换成补 header、填变量的 headerTransport；默认拒绝跨域重定向。
func NewHTTPClient(opt *HTTPClientOpt) *http.Client {
	// 1. 按配置选择重定向规则：允许跨域时只保留跳数上限。
	checkRedirect := checkSameOriginRedirect
	if opt.IsCrossOriginRedirectAllowed {
		checkRedirect = checkRedirectLimit
	}
	// 2. 每次创建新的客户端，底层 transport 不在这里复制。
	return &http.Client{
		Transport:     &headerTransport{opt: opt},
		CheckRedirect: checkRedirect,
	}
}

// headerTransport 给请求补上客户端级 header，并填写全部 header 里的 ${OPENVAALA_*}。
type headerTransport struct {
	opt *HTTPClientOpt
}

// RoundTrip 补全 header、填写变量后交给底层 transport。
func (t *headerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	// 1. 复制请求，把 opt.Headers 里请求还没有的条目加上。
	cloned := request.Clone(request.Context())
	for name, value := range t.opt.Headers {
		if cloned.Header.Values(name) == nil {
			cloned.Header.Set(name, value)
		}
	}
	// 2. 填写全部 header 里的变量，取不到变量或值为空的整条删掉。
	fillRequestHeaders(cloned.Header, defs.GetSessionCtxMeta(request.Context()))
	// 3. 交给底层 transport 发出。
	transport := t.opt.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	return transport.RoundTrip(cloned)
}

// fillRequestHeaders 填写 header 里的 ${OPENVAALA_*}，并删掉取不到值或值为空的整条 header（User-Agent 改为空值）。
func fillRequestHeaders(header http.Header, meta defs.SessionCtxMeta) {
	for name, values := range header {
		// 1. 一条 header 的任何一个值取不到，整条删掉。
		filledValues := make([]string, len(values))
		isFillable := true
		for i, value := range values {
			filledValues[i], isFillable = fillHeaderValue(value, meta)
			if !isFillable {
				break
			}
		}
		if !isFillable {
			// User-Agent 保留为空值，避免 net/http 补上默认的 Go-http-client；其余整条删除。
			if name == "User-Agent" {
				header[name] = []string{""}
			} else {
				delete(header, name)
			}
			continue
		}
		// 2. 全部取到后换成填好的值。
		header[name] = filledValues
	}
}

// maxRedirects 是一次请求最多跟随的重定向跳数。
const maxRedirects = 10

// checkRedirectLimit 在已经跟随 maxRedirects 跳后停止重定向。
func checkRedirectLimit(_ *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	return nil
}

// checkSameOriginRedirect 在重定向目标的 scheme 或 host 与第一跳不同时拒绝跟随，并受跳数上限约束。
func checkSameOriginRedirect(request *http.Request, via []*http.Request) error {
	// 1. 先检查跳数上限。
	if err := checkRedirectLimit(request, via); err != nil {
		return err
	}
	// 2. 和第一跳比较 scheme 与 host，不分大小写。
	origin := via[0].URL
	if !strings.EqualFold(request.URL.Scheme, origin.Scheme) ||
		!strings.EqualFold(request.URL.Host, origin.Host) {
		return fmt.Errorf("refusing cross-origin redirect from %s://%s to %s://%s",
			origin.Scheme, origin.Host, request.URL.Scheme, request.URL.Host)
	}
	return nil
}

// MergeHeaders 用 destination 同名（不分大小写）覆盖 global，返回 header 名统一写法后的新表；值为 "" 的条目保留，发请求时删掉。
func MergeHeaders(global, destination map[string]string) map[string]string {
	// 1. 先放 global，再放 destination，同名的后者覆盖前者。
	merged := make(map[string]string, len(global)+len(destination))
	for name, value := range global {
		merged[http.CanonicalHeaderKey(name)] = value
	}
	for name, value := range destination {
		merged[http.CanonicalHeaderKey(name)] = value
	}
	return merged
}

// ValidateHeaderTemplate 检查 header 值里的 ${OPENVAALA_*} 是否都认识；返回 unknown variable OPENVAALA_X 形式的错误。
func ValidateHeaderTemplate(value string) error {
	// 1. 按出现顺序检查，遇到第一个不认识的变量就返回。
	for _, match := range headerVariablePattern.FindAllStringSubmatch(value, -1) {
		name := match[1]
		if _, known := headerVariables[name]; !known {
			return fmt.Errorf("unknown variable %s", name)
		}
	}
	return nil
}
