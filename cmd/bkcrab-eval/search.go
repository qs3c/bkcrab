package main

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/qs3c/bkcrab/internal/toolproviders"
	"github.com/qs3c/bkcrab/internal/toolproviders/websearch"
)

// Evaluation uses the production providers without loading gateway/user state.
func searchChain(name, endpoint, key string) (*toolproviders.Chain, error) {
	if name == "" {
		if endpoint != "" || key != "" {
			return nil, fmt.Errorf("web search provider is required")
		}
		return nil, nil
	}
	switch name {
	case "exa":
		if endpoint != "" {
			u, err := url.Parse(endpoint)
			if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "/search/exa" || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() || key != "" {
				return nil, fmt.Errorf("exa relay requires an HTTP loopback /search/exa endpoint without credentials")
			}
			key = "relay-only"
		} else if key == "" {
			return nil, fmt.Errorf("exa search requires a credential or local relay")
		}
	case "brave":
		if key == "" || endpoint != "" {
			return nil, fmt.Errorf("%s search requires a credential and uses its built-in endpoint", name)
		}
	case "searxng":
		u, err := url.Parse(endpoint)
		if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(endpoint, "\r\n") {
			return nil, fmt.Errorf("searxng requires an HTTP(S) endpoint without credentials, query or fragment")
		}
		if key != "" {
			return nil, fmt.Errorf("searxng does not use an API key")
		}
	default:
		return nil, fmt.Errorf("unsupported web search provider %q", name)
	}
	r := toolproviders.NewRegistry()
	websearch.RegisterAll(r)
	return &toolproviders.Chain{
		Category: websearch.Category, Order: []string{name}, Registry: r,
		GetConfig: func(string) toolproviders.ProviderConfig {
			return toolproviders.ProviderConfig{APIKey: key, Endpoint: endpoint}
		},
	}, nil
}
