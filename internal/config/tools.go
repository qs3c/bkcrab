package config

// ResolveToolProviderCfg 返回指定名称的提供者配置，回退为空值（非 nil），
// 以便调用方无需做 nil 检查。
func (c *Config) ResolveToolProviderCfg(name string) ToolProviderCfg {
	if c == nil || c.ToolProviders == nil {
		return ToolProviderCfg{}
	}
	return c.ToolProviders[name]
}

// ResolveToolCategory 返回 categoryName（例如 "web_search"）的分类配置，
// 回退为空值。
func (c *Config) ResolveToolCategory(categoryName string) ToolCategoryCfg {
	if c == nil || c.Tools == nil {
		return ToolCategoryCfg{}
	}
	return c.Tools[categoryName]
}
