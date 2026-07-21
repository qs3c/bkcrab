package sidecar

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/qs3c/bkcrab/internal/rag/telemetry"
)

type sidecarTelemetryCollector struct {
	mu     sync.Mutex
	events []telemetry.Event
}

func (c *sidecarTelemetryCollector) Record(_ context.Context, event telemetry.Event) {
	c.mu.Lock()
	c.events = append(c.events, event)
	c.mu.Unlock()
}

func TestSidecarTelemetryReportsBoundedCountsAndTypedErrors(t *testing.T) {
	data := []byte("office bytes contain TOP SECRET body")
	source := testDocumentSource(data, "docx", func() io.ReadCloser {
		return io.NopCloser(bytes.NewReader(data))
	})
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/healthz":
			response.Header().Set("Content-Type", "application/json")
			_, _ = response.Write(healthyResponse(t))
		case "/v1/office/convert":
			_, _ = io.Copy(io.Discard, request.Body)
			response.Header().Set("Content-Type", "application/x-tar")
			_, _ = response.Write(officeResponseTar(t, source))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	collector := &sidecarTelemetryCollector{}
	client, err := NewClient(ClientConfig{Endpoint: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	client.SetRecorder(collector)
	if _, err := client.ProbeHealth(context.Background()); err != nil {
		t.Fatal(err)
	}
	bundle, err := client.ConvertOffice(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	defer bundle.Close()

	collector.mu.Lock()
	events := append([]telemetry.Event(nil), collector.events...)
	collector.mu.Unlock()
	if len(events) != 1 {
		t.Fatalf("events=%+v want one sidecar call", events)
	}
	fields := events[0].Fields
	if events[0].Name != telemetry.EventParserSidecarCall || fields.Operation != "office-convert" ||
		fields.DocID != source.DocID || fields.Format != "docx" || fields.Outcome != "ok" || fields.WarningCount != 0 {
		t.Fatalf("unexpected sidecar telemetry: %+v", events[0])
	}
	if bytes.Contains([]byte(fields.DocID+fields.ErrorCode), []byte("TOP SECRET")) {
		t.Fatalf("source body leaked into telemetry: %+v", fields)
	}
}
