package eval

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/rag/objects"
)

func TestCatalogAdaptersPinnedNetworkSmoke(t *testing.T) {
	if os.Getenv("BKCRAB_CATALOG_NETWORK_TEST") != "1" {
		t.Skip("set BKCRAB_CATALOG_NETWORK_TEST=1 to verify pinned public datasets")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	objectStore := objects.NewLocalFS(t.TempDir())
	requests := []CatalogImportOptions{
		{CatalogID: CatalogMultiDoc2Dial, Track: DatasetTrackTextRAG, Split: "validation", SampleSize: 2, Seed: 42},
		{CatalogID: CatalogTATQA, Track: DatasetTrackTextRAG, Split: "dev", SampleSize: 2, Seed: 42},
		{CatalogID: CatalogOpenRAGBench, Track: DatasetTrackPDFE2E, Split: "arxiv", SampleSize: 1, Seed: 42, CorpusLimit: 1, EvidenceTypes: []string{"text"}},
	}
	for _, options := range requests {
		t.Run(options.CatalogID, func(t *testing.T) {
			preset, _ := CatalogPresetByID(options.CatalogID)
			source, err := NewCatalogHTTPSource(preset, objectStore, nil)
			if err != nil {
				t.Fatal(err)
			}
			adapter, _ := CatalogAdapterFor(options.CatalogID)
			prepared, err := adapter.Prepare(ctx, source, options)
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.Close()
			if len(prepared.Dataset.Cases) != options.SampleSize || len(prepared.Documents) == 0 {
				t.Fatalf("prepared cases=%d documents=%d", len(prepared.Dataset.Cases), len(prepared.Documents))
			}
			if report := ValidateDataset(prepared.Dataset, DefaultValidationLimits()); !report.Valid {
				t.Fatalf("prepared dataset failed validation: %+v", report)
			}
		})
	}
}
