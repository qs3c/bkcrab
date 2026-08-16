package sidecar

import (
	"context"
	"fmt"
	"strings"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag/document"
	"github.com/qs3c/bkcrab/internal/rag/telemetry"
)

// Pool routes immutable document sources to their pinned parser engine while
// retaining one default client for the shared PDF implementation.
type Pool struct {
	defaultEngine string
	clients       map[string]*Client
}

func NewPool(defaultEngine string, clients map[string]*Client) *Pool {
	defaultEngine = strings.ToLower(strings.TrimSpace(defaultEngine))
	if defaultEngine == "" {
		defaultEngine = OfficeEngineMarkItDown
	}
	copyClients := make(map[string]*Client, len(clients))
	for engine, client := range clients {
		engine = strings.ToLower(strings.TrimSpace(engine))
		if client != nil && (engine == OfficeEngineMarkItDown || engine == OfficeEngineAnyDoc) {
			copyClients[engine] = client
		}
	}
	if len(copyClients) == 0 {
		return nil
	}
	return &Pool{defaultEngine: defaultEngine, clients: copyClients}
}

func (p *Pool) client(engine string) (*Client, error) {
	if p == nil {
		return nil, fmt.Errorf("%w: parser pool is not configured", ErrCapabilityUnavailable)
	}
	engine = strings.ToLower(strings.TrimSpace(engine))
	if engine == "" {
		engine = p.defaultEngine
	}
	client := p.clients[engine]
	if client == nil {
		return nil, fmt.Errorf("%w: parser engine %q is not configured", ErrCapabilityUnavailable, engine)
	}
	return client, nil
}

func (p *Pool) ConvertOffice(ctx context.Context, source document.Source) (*BundleHandle, error) {
	client, err := p.client(source.ParserEngine)
	if err != nil {
		return nil, err
	}
	return client.ConvertOffice(ctx, source)
}

func (p *Pool) AnalyzePDF(ctx context.Context, source document.Source) (*BundleHandle, error) {
	client, err := p.client(source.ParserEngine)
	if err != nil {
		return nil, err
	}
	return client.AnalyzePDF(ctx, source)
}

func (p *Pool) RenderPDF(ctx context.Context, source document.Source, pages []int) (*BundleHandle, error) {
	client, err := p.client(source.ParserEngine)
	if err != nil {
		return nil, err
	}
	return client.RenderPDF(ctx, source, pages)
}

func (p *Pool) StartHealthProbe(ctx context.Context) {
	if p == nil {
		return
	}
	for _, client := range p.clients {
		client.StartHealthProbe(ctx)
	}
}

func (p *Pool) SetRecorder(recorder telemetry.Recorder) {
	if p == nil {
		return
	}
	for _, client := range p.clients {
		client.SetRecorder(recorder)
	}
}

func (p *Pool) HealthSnapshot() config.RAGParserHealthSnapshot {
	client, err := p.client("")
	if err != nil {
		return config.RAGParserHealthSnapshot{}
	}
	return client.HealthSnapshot()
}

func (p *Pool) HealthSnapshots() map[string]config.RAGParserHealthSnapshot {
	if p == nil {
		return map[string]config.RAGParserHealthSnapshot{}
	}
	out := make(map[string]config.RAGParserHealthSnapshot, len(p.clients))
	for engine, client := range p.clients {
		out[engine] = client.HealthSnapshot()
	}
	return out
}

func (p *Pool) ParserAvailable(engine string, cfg config.RAGCfg) bool {
	client, err := p.client(engine)
	if err != nil {
		return false
	}
	engine = strings.ToLower(strings.TrimSpace(engine))
	parserCfg := cfg
	parserCfg.ParserSidecar.Engine = engine
	parserCfg.ParserSidecar.Endpoint = parserCfg.ParserSidecar.EndpointForEngine(engine)
	return parserCfg.RuntimeCapabilities(client.HealthSnapshot()).Office.Available
}
