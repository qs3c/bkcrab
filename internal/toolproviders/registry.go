// Package toolproviders 是与外部服务（网络搜索、图像生成、TTS 等）通信的插件层。
// 每个类别向 LLM 公开一个工具（例如 "web_search"），由主提供商和有序的回退链支持。
// LLM 永远看不到各个提供商。
//
// 提供商是无状态的：每次调用的配置（API 密钥、端点等）通过 Request.Config 传入，
// 因此同一个提供商实例可以安全地处理多个租户。
package toolproviders

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Provider 是某个类别的单个后端。实现是纯 Go 的；
// 以后可以添加子进程/插件 Provider，使用相同的接口。
type Provider interface {
	Category() string
	Name() string
	// Execute 运行提供商。由类别决定 Args/Response 形状的含义——
	// 同一类别下的所有提供商共享相同的契约。
	Execute(ctx context.Context, req Request) (Response, error)
}

// CredentialFree 是一个可选的 Provider 标记，用于那些无需任何租户配置即可工作的后端
//（内置的 web_fetch direct 获取器是典型示例：它直接使用 http.DefaultClient）。
// 链的可用性/跳过规则将这些提供商视为始终可用，因此管理员可以在 UI 中选择它们，
// 而无需输入虚假的 API 密钥。
type CredentialFree interface {
	CredentialFree() bool
}

func providerCredentialFree(p Provider) bool {
	cf, ok := p.(CredentialFree)
	return ok && cf.CredentialFree()
}

// Request 携带 LLM 提供的参数以及解析后的每个租户的配置。
type Request struct {
	Args   map[string]any
	Config ProviderConfig
}

// ProviderConfig 保存为单个调用解析的凭据/端点。
// 它来自 bkcrab.json 的 toolProviders.<name>，可被代理自身的配置覆盖。
type ProviderConfig struct {
	APIKey   string
	Endpoint string
	Options  map[string]string
	// Model 是 "<provider>/<model>" 引用中斜杠后的部分。
	// 当用户只写了 "<provider>" 时为空。
	Model string
}

// Response 是工具可见的文本加上可选的结构化负载。
type Response struct {
	Text string
	Raw  any
}

// Registry 保存所有已注册的 Provider 实现，以 "<category>/<name>" 为键。
// 预热后读操作是并发安全的，因此在 init() 中注册是可以的。
type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
}

// NewRegistry 返回一个空的 Registry。
func NewRegistry() *Registry {
	return &Registry{providers: make(map[string]Provider)}
}

// Register 安装提供商 p。后续使用相同键的注册会覆盖，以便测试可以替换实现。
func (r *Registry) Register(p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[key(p.Category(), p.Name())] = p
}

// Get 返回在 category/name 下注册的 Provider，如果不存在则返回 nil。
func (r *Registry) Get(category, name string) Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.providers[key(category, name)]
}

// Names 返回某个类别中所有已注册的提供商名称，已排序。
func (r *Registry) Names(category string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	prefix := category + "/"
	for k := range r.providers {
		if strings.HasPrefix(k, prefix) {
			out = append(out, strings.TrimPrefix(k, prefix))
		}
	}
	sort.Strings(out)
	return out
}

func key(category, name string) string { return category + "/" + name }

// ---------- 链式执行 ----------

// Chain 是工具类别的每次调用配置：要尝试的 "<provider>/<model>" 引用的有序列表、
// 是否回退，以及解析每个提供商凭据（可能限于租户范围）的查找函数。
// Chain 构建成本低，可为每个代理创建和丢弃。
type Chain struct {
	Category     string
	Order        []string // 例如 ["exa/auto", "brave/web", "searxng/default"]
	AutoFallback bool
	Registry     *Registry
	// GetConfig 返回指定提供商名称的配置。允许按代理/按租户覆盖，而 Chain 不拥有任何状态。
	GetConfig func(providerName string) ProviderConfig
}

// Available 报告 Order 中是否至少有一个提供商已注册并且具有可用的配置（非空的 APIKey 或 Endpoint）。
// 在工具注册时用于决定是否向 LLM 展示该类别的工具——
// 反映 OpenClaw 的规则：缺少凭据则隐藏工具。
func (c *Chain) Available() bool {
	for _, ref := range c.Order {
		name, _ := parseRef(ref)
		if c.Registry == nil {
			continue
		}
		p := c.Registry.Get(c.Category, name)
		if p == nil {
			continue
		}
		cfg := c.GetConfig(name)
		if cfg.APIKey != "" || cfg.Endpoint != "" || providerCredentialFree(p) {
			return true
		}
	}
	return false
}

// Execute 运行链：依次尝试 Order 中的每个提供商，直到其中一个成功。
// 在以下情况下，提供商被跳过（视为可重试的未命中）：
//   - 尚未注册
//   - 其配置没有 APIKey 或 Endpoint
//   - 返回可重试的错误（网络问题、超时、429、5xx、ErrNoResults）
// 任何其他错误都会终止链（以便配置错误快速暴露）。
// 当 AutoFallback 为 false 时，只尝试第一个配置的提供商。
func (c *Chain) Execute(ctx context.Context, args map[string]any) (Response, error) {
	if c.Registry == nil {
		return Response{}, fmt.Errorf("tool chain for %q has no registry", c.Category)
	}
	if len(c.Order) == 0 {
		return Response{}, fmt.Errorf("no providers configured for %q", c.Category)
	}
	var errs []error
	for i, ref := range c.Order {
		name, model := parseRef(ref)
		p := c.Registry.Get(c.Category, name)
		if p == nil {
			errs = append(errs, fmt.Errorf("%s: provider not registered", ref))
			if !c.AutoFallback {
				break
			}
			continue
		}
		cfg := c.GetConfig(name)
		cfg.Model = model
		if cfg.APIKey == "" && cfg.Endpoint == "" && !providerCredentialFree(p) {
			errs = append(errs, fmt.Errorf("%s: no API key configured", ref))
			if !c.AutoFallback {
				break
			}
			continue
		}
		resp, err := p.Execute(ctx, Request{Args: args, Config: cfg})
		if err == nil && !isEmpty(resp) {
			return resp, nil
		}
		if err == nil {
			err = ErrNoResults
		}
		errs = append(errs, fmt.Errorf("%s: %w", ref, err))
		// 遇到第一个不可重试的错误时停止；否则继续回退。
		if !isRetriable(err) || !c.AutoFallback || i == len(c.Order)-1 {
			if !isRetriable(err) {
				return Response{}, errors.Join(errs...)
			}
		}
	}
	return Response{}, errors.Join(errs...)
}

// parseRef 将 "exa/auto" 拆分为 ("exa", "auto")。单独的 "exa" 返回 ("exa", "")。
func parseRef(ref string) (name, model string) {
	if i := strings.IndexByte(ref, '/'); i >= 0 {
		return ref[:i], ref[i+1:]
	}
	return ref, ""
}

func isEmpty(r Response) bool { return strings.TrimSpace(r.Text) == "" && r.Raw == nil }

// ---------- 错误分类 ----------

// ErrNoResults 在提供商的请求成功但结果集为空时返回。
// 链执行将其视为可重试，以便下一个提供商有机会。
var ErrNoResults = errors.New("no results")

// RetriableError 标记一类应触发回退到下一个提供商的错误。
// 提供商在网络故障、上游 5xx 和 429 以及超时情况下返回此类错误。
// 其他错误（如参数格式错误）是致命的。
type RetriableError struct{ Err error }

func (r *RetriableError) Error() string { return r.Err.Error() }
func (r *RetriableError) Unwrap() error { return r.Err }

// Retry 包装错误，使链将其视为可重试。
func Retry(err error) error {
	if err == nil {
		return nil
	}
	return &RetriableError{Err: err}
}

func isRetriable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrNoResults) {
		return true
	}
	var re *RetriableError
	return errors.As(err, &re)
}
