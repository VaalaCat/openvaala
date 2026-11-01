package modelcatalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
)

const (
	// catalogMaxAge 是 models.dev 数据（内存里的和缓存文件里的）不重新下载的最长时间。
	catalogMaxAge = 24 * time.Hour
	// catalogDownloadTimeout 是下载 models.dev 的超时；原文约 6MB。
	catalogDownloadTimeout = 30 * time.Second
	// catalogMaxBytes 是 models.dev 响应体的大小上限，超出的部分读不到，按 JSON 不完整处理。
	catalogMaxBytes = 64 << 20
	// upstreamModelsTimeout 是请求上游 /models 的超时。
	upstreamModelsTimeout = 10 * time.Second
	// upstreamModelsMaxBytes 是上游 /models 响应体的大小上限，超出的部分读不到，按 JSON 不完整处理。
	upstreamModelsMaxBytes = 8 << 20
	// defaultSourceURL 是 models.dev 的数据地址。
	defaultSourceURL = "https://models.dev/api.json"
)

// Catalog 给首次引导提供候选的模型服务和模型：已知服务来自 models.dev（缓存在本机文件），自定义服务从上游 /models 拉 id，
// 再按 id 去 models.dev 补字段；拿不到 models.dev 时，自定义服务的模型只按 id 列出。
type Catalog struct {
	opt CatalogOpt
	// data 是解析好的 models.dev 数据和按模型 id 建的索引，过期后整份替换。
	data atomic.Pointer[catalogData]
}

// CatalogOpt 是 Catalog 的缓存文件、数据地址和 HTTP 客户端。
type CatalogOpt struct {
	// CacheFile 是 models.dev 的缓存文件，serve 传 os.UserCacheDir()/openvaala/models-dev.json。
	CacheFile string
	// SourceURL 默认 https://models.dev/api.json；测试换成 httptest 地址。
	SourceURL string
	// HTTPClient 下载 models.dev 和请求上游 /models 都用它，默认 http.DefaultClient。
	HTTPClient *http.Client
}

// NewCatalogOpt 创建配置，数据地址和 HTTP 客户端用默认值。
func NewCatalogOpt(cacheFile string) *CatalogOpt {
	// 1. 记下缓存文件路径，填入默认数据地址和 http.DefaultClient。
	return &CatalogOpt{
		CacheFile:  cacheFile,
		SourceURL:  defaultSourceURL,
		HTTPClient: http.DefaultClient,
	}
}

// WithSourceURL 换掉 models.dev 的数据地址。
func (opt *CatalogOpt) WithSourceURL(sourceURL string) *CatalogOpt {
	// 1. 更新数据地址并返回配置。
	opt.SourceURL = sourceURL
	return opt
}

// WithHTTPClient 换掉下载 models.dev 和请求上游 /models 用的 HTTP 客户端。
func (opt *CatalogOpt) WithHTTPClient(client *http.Client) *CatalogOpt {
	// 1. 更新 HTTP 客户端并返回配置。
	opt.HTTPClient = client
	return opt
}

// NewCatalog 校验配置并创建 Catalog。
func NewCatalog(opt *CatalogOpt) (*Catalog, error) {
	// 1. 检查配置、缓存路径、数据地址和 HTTP 客户端。
	if opt == nil {
		return nil, errors.New("model catalog requires options")
	}
	if opt.CacheFile == "" || opt.SourceURL == "" || opt.HTTPClient == nil {
		return nil, errors.New("model catalog requires a cache file, a source URL and an HTTP client")
	}
	// 2. 复制一份配置保存。
	return &Catalog{opt: *opt}, nil
}

// ListCatalogProviders 按服务 ID 顺序返回 models.dev 里能对上协议和 base_url 的模型服务。
func (c *Catalog) ListCatalogProviders(ctx context.Context) ([]defs.CatalogProvider, error) {
	// 1. 取 models.dev 数据，失败时返回错误。
	data, err := c.getData(ctx)
	if err != nil {
		return nil, err
	}
	// 2. 逐个转成 CatalogProvider，带上能列出的模型数。
	providers := make([]defs.CatalogProvider, 0, len(data.providers))
	for _, provider := range data.providers {
		providers = append(providers, defs.CatalogProvider{
			ID:         provider.id,
			Name:       provider.name,
			Protocol:   provider.protocol,
			BaseURL:    provider.baseURL,
			ModelCount: len(provider.models),
		})
	}
	return providers, nil
}

// ListCatalogModels 返回 models.dev 里某个服务的模型配置，键是写进 Provider 文件的模型 ID。
func (c *Catalog) ListCatalogModels(ctx context.Context, catalogProviderID string) (map[string]*defs.ProviderModelConfig, error) {
	// 1. 取 models.dev 数据并找到这个服务，找不到时返回错误。
	data, err := c.getData(ctx)
	if err != nil {
		return nil, err
	}
	index, found := slices.BinarySearchFunc(data.providers, catalogProviderID, func(provider *catalogProvider, id string) int {
		return strings.Compare(provider.id, id)
	})
	if !found {
		return nil, fmt.Errorf("catalog provider %s is not available", catalogProviderID)
	}
	// 2. 给各上游模型 ID 分配不重名的配置键。
	provider := data.providers[index]
	ids := make([]string, len(provider.models))
	for i, model := range provider.models {
		ids[i] = model.id
	}
	keys := defs.GetProviderModelKeys(ids)
	// 3. 转换模型字段和协议；键和上游 ID 不同时，把上游 ID 写进 UpstreamModel。
	models := make(map[string]*defs.ProviderModelConfig, len(provider.models))
	for _, model := range provider.models {
		config := newProviderModelConfig(model.model)
		config.Protocol = model.protocol
		if keys[model.id] != model.id {
			config.UpstreamModel = model.id
		}
		models[keys[model.id]] = config
	}
	return models, nil
}

// FetchProviderModels 从上游 /models 读模型 ID，models.dev 里有同名模型时补上字段。
func (c *Catalog) FetchProviderModels(ctx context.Context, provider defs.ProviderConfig) (map[string]*defs.ProviderModelConfig, error) {
	// 1. 请求上游模型 ID，失败时错误带上 base_url。
	ids, err := c.getUpstreamModelIDs(ctx, provider)
	if err != nil {
		return nil, fmt.Errorf("fetch models from %s: %w", provider.BaseURL, err)
	}
	// 2. 读取 models.dev；失败时记日志，模型只按 ID 列出。
	data, err := c.getData(ctx)
	if err != nil {
		telemetry.GetLogger(ctx).Warn("models.dev catalog unavailable, upstream models are listed by id only", "error", err)
	}
	// 3. 分配配置键，按 ID 匹配 models.dev 的字段；键和上游 ID 不同时，把上游 ID 写进 UpstreamModel。
	keys := defs.GetProviderModelKeys(ids)
	models := make(map[string]*defs.ProviderModelConfig, len(ids))
	for _, id := range ids {
		config := &defs.ProviderModelConfig{}
		if data != nil {
			if entry, found := data.getModelEntry(id); found {
				config = newProviderModelConfig(entry.model)
			}
		}
		if keys[id] != id {
			config.UpstreamModel = id
		}
		models[keys[id]] = config
	}
	return models, nil
}

// getUpstreamModelIDs 请求上游 /models，返回去重、排序后的模型 ID。
func (c *Catalog) getUpstreamModelIDs(ctx context.Context, provider defs.ProviderConfig) ([]string, error) {
	// 1. 在 10 秒内请求上游 /models，检查 HTTP 状态。
	ctx, cancel := context.WithTimeout(ctx, upstreamModelsTimeout)
	defer cancel()
	request, err := newUpstreamModelsRequest(ctx, provider)
	if err != nil {
		return nil, err
	}
	response, err := c.opt.HTTPClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s", response.Status)
	}
	// 2. 最多读取 8 MiB 并解析模型 ID。
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, upstreamModelsMaxBytes)).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	// 3. 去掉空 ID 和重复项，按 ID 排序。
	unique := make(map[string]struct{}, len(body.Data))
	for _, model := range body.Data {
		if model.ID != "" {
			unique[model.ID] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(unique)), nil
}

// getData 返回 models.dev 数据：内存里的不满 24 小时就直接用，否则重新加载并替换。
func (c *Catalog) getData(ctx context.Context) (*catalogData, error) {
	// 1. 内存里的数据不满 24 小时时直接返回。
	if data := c.data.Load(); data != nil && time.Since(data.loadedAt) < catalogMaxAge {
		return data, nil
	}
	// 2. 重新加载，成功后整份替换内存里的数据。
	data, err := c.loadData(ctx)
	if err != nil {
		return nil, err
	}
	c.data.Store(data)
	return data, nil
}

// loadData 加载 models.dev 数据：缓存文件不满 24 小时就用它，否则下载，下载失败时退回旧缓存。
func (c *Catalog) loadData(ctx context.Context) (*catalogData, error) {
	// 1. 缓存文件能解析且不满 24 小时时直接用。
	cached, cacheErr := c.readCache()
	if cacheErr == nil && time.Since(cached.loadedAt) < catalogMaxAge {
		return cached, nil
	}
	// 2. 下载新数据；下载失败又没有能用的旧缓存时返回错误。
	downloaded, downloadErr := c.download(ctx)
	if downloadErr == nil {
		return downloaded, nil
	}
	if cacheErr != nil {
		return nil, fmt.Errorf("cannot load models.dev catalog: %w", downloadErr)
	}
	// 3. 下载失败时记警告并用旧缓存，loadedAt 记为现在，24 小时内不再重试。
	telemetry.GetLogger(ctx).Warn("models.dev download failed, using the old cache file", "cache_file", c.opt.CacheFile, "error", downloadErr)
	cached.loadedAt = time.Now()
	return cached, nil
}

// readCache 读取并解析缓存文件，用文件修改时间作为 loadedAt。
func (c *Catalog) readCache() (*catalogData, error) {
	// 1. 读取文件信息和内容。
	info, err := os.Stat(c.opt.CacheFile)
	if err != nil {
		return nil, err
	}
	content, err := os.ReadFile(c.opt.CacheFile)
	if err != nil {
		return nil, err
	}
	return parseCatalogData(content, info.ModTime())
}

// download 下载并解析 models.dev 数据，再写入缓存文件。
func (c *Catalog) download(ctx context.Context) (*catalogData, error) {
	// 1. 在 30 秒内下载数据并检查 HTTP 状态。
	downloadCtx, cancel := context.WithTimeout(ctx, catalogDownloadTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(downloadCtx, http.MethodGet, c.opt.SourceURL, nil)
	if err != nil {
		return nil, err
	}
	response, err := c.opt.HTTPClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s", response.Status)
	}
	// 2. 最多读取 64 MiB 并解析。
	content, err := io.ReadAll(io.LimitReader(response.Body, catalogMaxBytes))
	if err != nil {
		return nil, err
	}
	data, err := parseCatalogData(content, time.Now())
	if err != nil {
		return nil, fmt.Errorf("decode catalog: %w", err)
	}
	// 3. 写入缓存文件，失败时只记警告，照样返回数据。
	if err = c.writeCache(content); err != nil {
		telemetry.GetLogger(ctx).Warn("models.dev cache file not written", "cache_file", c.opt.CacheFile, "error", err)
	}
	return data, nil
}

// writeCache 先写同目录的临时文件，再改名替换缓存文件。
func (c *Catalog) writeCache(content []byte) error {
	// 1. 创建权限为 0700 的缓存目录和权限为 0600 的临时文件。
	directory := filepath.Dir(c.opt.CacheFile)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".models-dev-*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	// 2. 写入并关闭临时文件。
	_, err = temporary.Write(content)
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	// 3. 改名替换缓存文件，返回前删掉没用上的临时文件。
	return os.Rename(temporary.Name(), c.opt.CacheFile)
}

// ── models.dev 数据 ──

// catalogData 是解析好的 models.dev 数据，构造后只读。
type catalogData struct {
	// loadedAt 是这份数据取自 models.dev 的时间：刚下载的是当时，从缓存文件读出的是文件的修改时间；
	// 下载失败而改用过期缓存时记为改用的当时，之后 24 小时内不再重试下载。
	loadedAt time.Time
	// providers 是能对应上协议和 base_url 的服务，按服务 id 排序。
	providers []*catalogProvider
	// modelsByID 按模型 id 索引全部服务（包括不能列出的）里的模型，同一个 id 下的条目按服务 id 排序。
	modelsByID map[string][]catalogModelEntry
}

// catalogProvider 是能列出的一个服务。
type catalogProvider struct {
	id       string
	name     string
	protocol string
	baseURL  string
	// models 是能对应上协议的模型，按模型 id 排序。
	models []catalogModel
}

// catalogModel 是能列出的服务里的一个模型。
type catalogModel struct {
	id string
	// protocol 是要写进模型配置的协议；和服务的协议相同时为空。
	protocol string
	model    *modelsDevModel
}

// catalogModelEntry 是某个服务里叫这个 id 的模型。
type catalogModelEntry struct {
	providerID string
	model      *modelsDevModel
}

// parseCatalogData 解析 models.dev 数据并建立服务列表和模型索引。
func parseCatalogData(content []byte, loadedAt time.Time) (*catalogData, error) {
	// 1. 解析 JSON。
	var providers map[string]modelsDevProvider
	if err := json.Unmarshal(content, &providers); err != nil {
		return nil, err
	}
	data := &catalogData{
		loadedAt:   loadedAt,
		modelsByID: make(map[string][]catalogModelEntry),
	}
	// 2. 按服务 ID、模型 ID 的顺序把全部服务的模型放进索引；服务列表只收能对上协议和地址的服务，以及其中协议能对上的模型。
	for _, providerID := range slices.Sorted(maps.Keys(providers)) {
		source := providers[providerID]
		protocol, baseURL, listed := getCatalogEndpoint(providerID, source)
		provider := &catalogProvider{
			id:       providerID,
			name:     source.Name,
			protocol: protocol,
			baseURL:  baseURL,
		}
		for _, modelID := range slices.Sorted(maps.Keys(source.Models)) {
			model := source.Models[modelID]
			data.modelsByID[modelID] = append(data.modelsByID[modelID], catalogModelEntry{
				providerID: providerID,
				model:      &model,
			})
			if !listed {
				continue
			}
			if modelProtocol, supported := getModelProtocol(protocol, &model); supported {
				provider.models = append(provider.models, catalogModel{
					id:       modelID,
					protocol: modelProtocol,
					model:    &model,
				})
			}
		}
		if listed {
			data.providers = append(data.providers, provider)
		}
	}
	return data, nil
}

// getModelEntry 按模型 id 找一条 models.dev 模型记录；同名的有多条时选上限最常见的那组。
func (d *catalogData) getModelEntry(id string) (catalogModelEntry, bool) {
	// 1. 先按完整 id 找，找不到时再按最后一个 / 后面的部分找。
	entries := d.modelsByID[id]
	if len(entries) == 0 {
		if slash := strings.LastIndex(id, "/"); slash >= 0 {
			entries = d.modelsByID[id[slash+1:]]
		}
	}
	// 2. 还找不到时直接返回，找到时从同名记录里选一条。
	if len(entries) == 0 {
		return catalogModelEntry{}, false
	}
	return getMajorityEntry(entries), true
}

// modelsDevProvider 是 api.json 里的一个服务，只列出用到的字段。
type modelsDevProvider struct {
	Name string `json:"name"`
	// API 是服务的 base_url，可以没有。
	API string `json:"api"`
	// NPM 是服务用的 SDK 包名，决定协议。
	NPM    string                    `json:"npm"`
	Models map[string]modelsDevModel `json:"models"`
}

// modelsDevModel 是 api.json 里的一个模型，只列出用到的字段。
type modelsDevModel struct {
	Name string `json:"name"`
	// Reasoning 没写时为 nil。
	Reasoning        *bool                      `json:"reasoning"`
	ReasoningOptions []modelsDevReasoningOption `json:"reasoning_options"`
	// Limit 没写时是零值。
	Limit modelsDevLimit `json:"limit"`
	// Modalities 没写时 Input 为空。
	Modalities struct {
		// Input 是模型能接收的输入，例如 text、image、pdf。
		Input []string `json:"input"`
	} `json:"modalities"`
	// Provider 是模型自己的 SDK，和服务不同时按它选协议。
	Provider struct {
		NPM string `json:"npm"`
	} `json:"provider"`
}

// modelsDevReasoningOption 是模型的一种推理设置；type 为 effort 时 values 是可选的推理强度。
type modelsDevReasoningOption struct {
	Type string `json:"type"`
	// Values 里可能混有 null，只取字符串。
	Values []any `json:"values"`
}

// modelsDevLimit 是模型的窗口和输出上限；0 表示没有。可以直接做 map 的 key，同一个组的模型上限相同。
type modelsDevLimit struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}
