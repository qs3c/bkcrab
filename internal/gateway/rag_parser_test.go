package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/config"
)

func TestRAGParserClientConstructionDoesNotProbeAndBackgroundSnapshotPublishesOfficeGoldens(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.URL.Path != "/healthz" {
			http.NotFound(response, request)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{
  "protocolVersion":"rag-parser/v1",
  "serviceVersion":"test-build",
  "limits":{"maxInputBytes":1024,"maxOutputBytes":4096},
  "capabilities":{
    "office":{"enabled":true,"formats":["docx","pptx","xlsx"],"markitdownVersion":"0.1.6","wrapperVersion":"office-wrapper-v1"},
    "pdf":{"enabled":true,"engine":"pypdfium2","engineVersion":"5.12.1"}
  }
}`))
	}))
	defer server.Close()

	cfg := config.RAGCfg{
		Features:      config.RAGFeatureCfg{OfficeParsingEnabled: true},
		ParserSidecar: config.RAGParserSidecarCfg{Endpoint: server.URL, TimeoutMS: 1000},
		Limits:        config.RAGLimitsCfg{MaxFileMB: 1, MaxExtractedBytes: 4096},
	}
	client, err := newRAGParserClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if client == nil || calls.Load() != 0 {
		t.Fatalf("client=%v synchronous health calls=%d", client, calls.Load())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.StartHealthProbe(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() == 0 || !client.HealthSnapshot().Healthy {
		if time.Now().After(deadline) {
			t.Fatal("background health probe did not publish a snapshot")
		}
		time.Sleep(time.Millisecond)
	}
	snapshot := client.HealthSnapshot()
	if !snapshot.PDF.Enabled || !snapshot.PDF.LicenseApproved {
		t.Fatalf("approved PDF engine was not published: %+v", snapshot.PDF)
	}
	if !snapshot.Office.DOCXGolden || !snapshot.Office.PPTXGolden || !snapshot.Office.XLSXGolden {
		t.Fatalf("checked-in converter goldens were not published: %+v", snapshot.Office)
	}
	if state := cfg.RuntimeCapabilities(snapshot); !state.Office.Available || state.Office.Reason != "" {
		t.Fatalf("Office capability=%+v", state.Office)
	}
}
