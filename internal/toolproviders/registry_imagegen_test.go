package toolproviders

import (
	"context"
	"testing"
)

type resolveTestProvider struct {
	category       string
	name           string
	credentialFree bool
}

func (p resolveTestProvider) Category() string { return p.category }
func (p resolveTestProvider) Name() string     { return p.name }
func (p resolveTestProvider) CredentialFree() bool {
	return p.credentialFree
}
func (p resolveTestProvider) Execute(context.Context, Request) (Response, error) {
	return Response{}, nil
}

func TestRegistryResolveProviderReferencesPreservesOrderAndRefreshesConfig(t *testing.T) {
	registry := NewRegistry()
	registry.Register(resolveTestProvider{category: "image_gen", name: "alpha"})
	registry.Register(resolveTestProvider{category: "image_gen", name: "beta"})

	configs := map[string]ProviderConfig{
		"alpha": {APIKey: "alpha-v1", Endpoint: "https://alpha.invalid", Options: map[string]string{"region": "one"}},
		"beta":  {APIKey: "beta-v1"},
	}
	resolved, err := registry.ResolveProviderReferences("image_gen", []string{"alpha/model-a", "beta/model-b"}, func(name string) ProviderConfig {
		return configs[name]
	})
	if err != nil {
		t.Fatalf("resolve references: %v", err)
	}
	if len(resolved) != 2 || resolved[0].Name != "alpha" || resolved[0].Model != "model-a" || resolved[1].Name != "beta" {
		t.Fatalf("order/model not preserved: %#v", resolved)
	}
	if !resolved[0].Configured || resolved[0].Config.APIKey != "alpha-v1" {
		t.Fatalf("alpha config not resolved: %#v", resolved[0])
	}

	// Returned options must not alias the mutable configuration snapshot.
	resolved[0].Config.Options["region"] = "mutated"
	if configs["alpha"].Options["region"] != "one" {
		t.Fatal("resolved options alias source configuration")
	}

	configs["alpha"] = ProviderConfig{APIKey: "alpha-v2"}
	refreshed, err := registry.ResolveProviderReferences("image_gen", []string{"alpha/model-a"}, func(name string) ProviderConfig {
		return configs[name]
	})
	if err != nil {
		t.Fatalf("refresh references: %v", err)
	}
	if got := refreshed[0].Config.APIKey; got != "alpha-v2" {
		t.Fatalf("hot config refresh: want alpha-v2, got %q", got)
	}
}

func TestRegistryResolveProviderReferencesValidatesRefsAndAvailability(t *testing.T) {
	registry := NewRegistry()
	registry.Register(resolveTestProvider{category: "image_gen", name: "configured"})
	registry.Register(resolveTestProvider{category: "image_gen", name: "free", credentialFree: true})

	tests := []struct {
		name string
		refs []string
	}{
		{name: "empty", refs: nil},
		{name: "blank provider", refs: []string{"/model"}},
		{name: "blank model", refs: []string{"configured/"}},
		{name: "unknown", refs: []string{"missing/model"}},
		{name: "duplicate", refs: []string{"configured/model", "configured/model"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := registry.ResolveProviderReferences("image_gen", tt.refs, func(string) ProviderConfig { return ProviderConfig{} }); err == nil {
				t.Fatalf("invalid refs accepted: %#v", tt.refs)
			}
		})
	}

	resolved, err := registry.ResolveProviderReferences("image_gen", []string{"configured/model", "free/model"}, func(string) ProviderConfig {
		return ProviderConfig{}
	})
	if err != nil {
		t.Fatalf("resolve availability: %v", err)
	}
	if resolved[0].Configured {
		t.Fatal("provider without endpoint/key reported configured")
	}
	if !resolved[1].Configured {
		t.Fatal("credential-free provider reported unavailable")
	}
	providerDefault, err := registry.ResolveProviderReferences("image_gen", []string{"free"}, func(string) ProviderConfig { return ProviderConfig{} })
	if err != nil || providerDefault[0].Model != "" {
		t.Fatalf("provider-only legacy reference should remain valid: %#v, %v", providerDefault, err)
	}
}
