package rag

import (
	"context"
	"testing"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag/document"
	"github.com/qs3c/bkcrab/internal/rag/parse"
	"github.com/qs3c/bkcrab/internal/rag/parse/sidecar"
)

type servicePrimitiveExtractor struct{}

func (*servicePrimitiveExtractor) ConvertOffice(context.Context, document.Source) (*sidecar.BundleHandle, error) {
	return nil, sidecar.ErrCapabilityUnavailable
}

func (*servicePrimitiveExtractor) AnalyzePDF(context.Context, document.Source) (*sidecar.BundleHandle, error) {
	return nil, sidecar.ErrCapabilityUnavailable
}

func (*servicePrimitiveExtractor) RenderPDF(context.Context, document.Source, []int) (*sidecar.BundleHandle, error) {
	return nil, sidecar.ErrCapabilityUnavailable
}

func TestServiceInjectsPrimitiveExtractorIntoDefaultParserFacade(t *testing.T) {
	primitives := &servicePrimitiveExtractor{}
	service := New(Deps{Cfg: config.RAGCfg{}, Primitives: primitives})
	if service.primitives != primitives {
		t.Fatalf("service primitive extractor=%T, want injected fake", service.primitives)
	}
	local, ok := service.parser.(*parse.LocalParser)
	if !ok {
		t.Fatalf("service parser=%T, want *parse.LocalParser", service.parser)
	}
	if local.Primitives != primitives {
		t.Fatalf("service parser primitives=%T", local.Primitives)
	}
	if local.MaxPages != service.cfg.Limits.MaxPagesPerDocument {
		t.Fatalf("parser max pages=%d, want %d", local.MaxPages, service.cfg.Limits.MaxPagesPerDocument)
	}
}
