package config

// ResolveToolProviderCfg returns the config for a provider name, falling back
// to an empty value (not nil) so callers don't need to nil-check.
func (c *Config) ResolveToolProviderCfg(name string) ToolProviderCfg {
	if c == nil || c.ToolProviders == nil {
		return ToolProviderCfg{}
	}
	return c.ToolProviders[name]
}

// ResolveToolCategory returns the category config for categoryName (e.g.
// "web_search"), falling back to an empty value.
func (c *Config) ResolveToolCategory(categoryName string) ToolCategoryCfg {
	if c == nil || c.Tools == nil {
		return ToolCategoryCfg{}
	}
	return c.Tools[categoryName]
}
