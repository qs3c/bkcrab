package plugin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/qs3c/bkclaw/internal/toolproviders"
)

// pluginProvider 将 {plugin, category, name} 三元组包装为
// toolproviders.Provider。每次调用都会通过 JSON-RPC 消息往返到
// 插件子进程，因此 Execute 比进程内提供者慢——
// 适用于偶尔/自定义的提供者，有意不用于高频率的默认场景。
type pluginProvider struct {
	mgr      *Manager
	pluginID string
	category string
	name     string
}

func (p *pluginProvider) Category() string { return p.category }
func (p *pluginProvider) Name() string     { return p.name }

func (p *pluginProvider) Execute(ctx context.Context, req toolproviders.Request) (toolproviders.Response, error) {
	params := ProviderExecuteParams{
		Category: p.category,
		Name:     p.name,
		Args:     req.Args,
		Config: ProviderConfigWire{
			APIKey:   req.Config.APIKey,
			Endpoint: req.Config.Endpoint,
			Options:  req.Config.Options,
			Model:    req.Config.Model,
		},
	}
	res, err := p.mgr.ExecuteProvider(ctx, p.pluginID, params)
	if err != nil {
		// 网络/插件级别的错误始终可重试——链中的另一个
		// 提供者仍可能成功。
		return toolproviders.Response{}, toolproviders.Retry(fmt.Errorf("plugin %s: %w", p.pluginID, err))
	}
	if res.Error != "" {
		errOut := errors.New(res.Error)
		if res.Retriable {
			return toolproviders.Response{}, toolproviders.Retry(errOut)
		}
		return toolproviders.Response{}, errOut
	}
	if res.Text == "" {
		return toolproviders.Response{}, toolproviders.ErrNoResults
	}
	return toolproviders.Response{Text: res.Text}, nil
}

// RegisterPluginProviders 询问每个正在运行的工具插件它填充了哪些提供者
// 槽位，并在 reg 中注册每个槽位。冲突（相同 category/name 已注册）
// 会替换较早的条目，因此插件可以有意图地覆盖内置提供者。
//
// 未实现 provider.list 的插件是无害的空操作：
// ListProviders 辅助函数会吞掉"未知方法"错误。
func RegisterPluginProviders(ctx context.Context, mgr *Manager, reg *toolproviders.Registry) {
	if mgr == nil || reg == nil {
		return
	}
	for _, inst := range mgr.ToolPlugins() {
		defs, err := mgr.ListProviders(ctx, inst.Manifest.ID)
		if err != nil {
			slog.Warn("plugin: provider.list failed", "plugin", inst.Manifest.ID, "error", err)
			continue
		}
		for _, d := range defs {
			if d.Category == "" || d.Name == "" {
				continue
			}
			reg.Register(&pluginProvider{mgr: mgr, pluginID: inst.Manifest.ID, category: d.Category, name: d.Name})
			slog.Info("plugin: registered tool provider", "plugin", inst.Manifest.ID, "category", d.Category, "name", d.Name)
		}
	}
}
