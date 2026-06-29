package setup

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/gateway"
	"github.com/qs3c/bkcrab/internal/toolproviders"
)

// categoryCatalog 是管理 UI 的权威来源，用于定义哪些工具类别存在以及哪些提供者可以支持它们。
// 扩展此列表（一旦新提供者在 toolproviders 包中存在）即可使它们自动出现在 UI 中。
type categoryCatalog struct {
	Name      string            `json:"name"`  // e.g. "web_search"
	Label     string            `json:"label"` // human-friendly name
	Providers []providerCatalog `json:"providers"`
}

type providerCatalog struct {
	Name     string   `json:"name"`     // "exa"
	Label    string   `json:"label"`    // "Exa"
	NeedsKey bool     `json:"needsKey"` // API key required?
	NeedsURL bool     `json:"needsUrl"` // endpoint required (self-hosted)?
	Models   []string `json:"models"`   // suggested "<provider>/<model>" suffixes
}

// builtinCatalog 列出了二进制文件知道如何运行的每个工具类别 + 提供者对。
// 运行时 Registry 中不存在的提供者会在响应时被过滤掉，因此乐观地列出它们是安全的。
var builtinCatalog = []categoryCatalog{
	{
		Name:  "web_search",
		Label: "Web Search",
		Providers: []providerCatalog{
			{Name: "exa", Label: "Exa", NeedsKey: true, Models: []string{"auto", "neural", "keyword"}},
			{Name: "brave", Label: "Brave Search", NeedsKey: true, Models: []string{"web"}},
			{Name: "searxng", Label: "SearxNG (self-hosted)", NeedsURL: true, Models: []string{"default"}},
			// "none" 是哨兵值：选中时，web_search 完全不暴露给模型。没有外部后端 — 模型使用自身的原生搜索（如果有）。
			{Name: "none", Label: "None (rely on model's native search)", Models: []string{"default"}},
		},
	},
	{
		Name:  "web_fetch",
		Label: "Web Fetch",
		Providers: []providerCatalog{
			// Direct 直接使用 Go 的 net/http — 无需密钥。
			// Jina 的免费层无需密钥即可使用（有限速）；密钥字段会显示出来，以便管理员粘贴密钥以提高配额，
			// 但链运行时将空白视为有效，因为该提供者实现了 CredentialFree。
			{Name: "direct", Label: "Direct (built-in)", Models: []string{"default"}},
			{Name: "jina", Label: "Jina Reader", NeedsKey: true, Models: []string{"default"}},
			{Name: "firecrawl", Label: "Firecrawl", NeedsKey: true, Models: []string{"default"}},
		},
	},
	{
		Name:  "image_gen",
		Label: "Image Generation",
		Providers: []providerCatalog{
			{Name: "openai", Label: "OpenAI", NeedsKey: true, Models: []string{"gpt-image-1", "dall-e-3"}},
			{Name: "replicate", Label: "Replicate", NeedsKey: true, Models: []string{"flux-schnell", "flux-dev", "flux-pro", "sdxl", "ideogram"}},
			{Name: "fal", Label: "Fal", NeedsKey: true, Models: []string{"flux-dev", "flux-schnell", "flux-pro"}},
			// "none" 是哨兵值：选中时，image_gen 完全不暴露给模型。模型回退到自身的原生图像生成能力（如果有）。
			{Name: "none", Label: "None (rely on model's native image gen)", Models: []string{"default"}},
		},
	},
	{
		Name:  "tts",
		Label: "Text-to-Speech",
		Providers: []providerCatalog{
			{Name: "openai", Label: "OpenAI", NeedsKey: true, Models: []string{"tts-1", "tts-1-hd"}},
			{Name: "elevenlabs", Label: "ElevenLabs", NeedsKey: true, Models: []string{"eleven_multilingual_v2", "eleven_turbo_v2_5", "eleven_flash_v2_5"}},
			{Name: "fish", Label: "Fish Audio", NeedsKey: true, Models: []string{"s1", "speech-1.5", "speech-1.6"}},
			{Name: "minimax", Label: "MiniMax", NeedsKey: true, Models: []string{"speech-02-hd", "speech-02-turbo"}},
			// "none" 是哨兵值：选中时，tts 完全不暴露给模型。模型回退到自身的原生音频能力（如果有）。
			{Name: "none", Label: "None (rely on model's native audio)", Models: []string{"default"}},
		},
	},
}

// handleGetTools 返回类别 + 提供者目录以及用户当前的 toolProviders/tools 设置。UI 据此渲染表单。
func (s *Server) handleGetTools(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.loadUserConfig(r)
	if err != nil {
		cfg = &config.Config{}
	}

	reg := gateway.ToolProviderRegistry()
	cats := make([]categoryCatalog, 0, len(builtinCatalog))
	for _, c := range builtinCatalog {
		filtered := make([]providerCatalog, 0, len(c.Providers))
		known := map[string]bool{}
		for _, p := range c.Providers {
			if reg.Get(c.Name, p.Name) == nil {
				continue
			}
			filtered = append(filtered, p)
			known[p.Name] = true
		}
		// 追加不在静态内置目录中的插件注册的提供者。我们无法知道它们是需要密钥还是 URL
		//（插件尚未声明这一点），因此我们同时提供两个字段 — 管理员填写插件所需的任意一个。
		for _, extra := range reg.Names(c.Name) {
			if known[extra] {
				continue
			}
			filtered = append(filtered, providerCatalog{
				Name:     extra,
				Label:    extra + " (plugin)",
				NeedsKey: true,
				NeedsURL: true,
				Models:   []string{"default"},
			})
		}
		cc := c
		cc.Providers = filtered
		cats = append(cats, cc)
	}

	// 将提供者作为带键的对象返回（便于 UI 进行合并编辑）。
	// apiKey 完整返回给管理员 — UI 决定是否屏蔽它 — 但云端调用者会在下面看到 403，因此这仅限本地。
	providers := map[string]config.ToolProviderCfg{}
	for name, pc := range cfg.ToolProviders {
		providers[name] = pc
	}
	tools := map[string]config.ToolCategoryCfg{}
	for name, cc := range cfg.Tools {
		tools[name] = cc
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"categories":    cats,
		"toolProviders": providers,
		"tools":         tools,
	})
}

// handleSaveTools 原子地更新 bkcrab.json 的 toolProviders 和 tools 部分。
// 仅允许管理员/本地用户 — 云端租户通过单独的路径获取自己的设置（尚未接入）。
// 保存后，运行中的 agent 会被热重载，以便链立即获取新密钥。
func (s *Server) handleSaveTools(w http.ResponseWriter, r *http.Request) {
	// requireSuperAdmin 中间件已对此路由进行门控；无需进一步检查。
	var req struct {
		ToolProviders map[string]config.ToolProviderCfg `json:"toolProviders"`
		Tools         map[string]config.ToolCategoryCfg `json:"tools"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}
	if err := validateToolChains(req.Tools); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	cfg, err := s.loadUserConfig(r)
	if err != nil {
		cfg = &config.Config{}
	}
	cfg.ToolProviders = req.ToolProviders
	cfg.Tools = req.Tools
	if err := s.saveUserConfig(r, cfg); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	// 提示解析器丢弃调用者缓存的用户空间；下次访问时从数据库重新加载新的工具/提供者配置。
	s.invalidateUser(s.effectiveUserID(r))
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// validateToolChains 检查每个 "<provider>/<model>" 引用是否指定了运行时目录中实际注册的提供者。
// 在此处捕获拼写错误可避免 agent 启动时静默地出现"没有可用提供者"。
// 运行时注册表是唯一的事实来源，因此插件提供的提供者与内置提供者以相同方式验证。
func validateToolChains(tools map[string]config.ToolCategoryCfg) error {
	reg := gateway.ToolProviderRegistry()
	for cat, cfg := range tools {
		for _, ref := range cfg.Chain() {
			name, _ := splitRef(ref)
			if reg.Get(cat, name) == nil {
				return fmt.Errorf("unknown provider %q for category %q", ref, cat)
			}
		}
	}
	return nil
}

func splitRef(ref string) (string, string) {
	for i := 0; i < len(ref); i++ {
		if ref[i] == '/' {
			return ref[:i], ref[i+1:]
		}
	}
	return ref, ""
}

// 当包增长时抑制未使用导入的警告。
var _ = toolproviders.ErrNoResults
