package plugin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/fastclaw-ai/fastclaw/internal/toolproviders"
)

// pluginProvider wraps a {plugin, category, name} triple as a
// toolproviders.Provider. Each call round-trips a JSON-RPC message to the
// plugin subprocess, so Execute is slower than an in-process provider —
// fine for occasional/custom providers, intentionally not used for
// high-volume defaults.
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
		// Network / plugin-level errors are always retriable — another
		// provider in the chain may still succeed.
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

// RegisterPluginProviders asks every running tool plugin which provider
// slots it fills and registers each one in reg. Conflicts (same
// category/name already registered) replace the earlier entry, so plugins
// can intentionally override built-ins.
//
// Plugins that don't implement provider.list are harmless no-ops: the
// ListProviders helper swallows the "unknown method" error.
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
