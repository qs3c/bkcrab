package enrich

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func multimodalEnrichmentFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "multimodal", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestMultimodalTableAndCodeInjectionFixturesStayInsideTypedEnhancement(t *testing.T) {
	t.Parallel()
	table, err := decodeEnhancement(multimodalEnrichmentFixture(t, "table-enrichment-injection.json"), BlockTable, DefaultSchemaLimits())
	if err != nil {
		t.Fatal(err)
	}
	code, err := decodeEnhancement(multimodalEnrichmentFixture(t, "code-enrichment-injection.json"), BlockCode, DefaultSchemaLimits())
	if err != nil {
		t.Fatal(err)
	}
	if table.Table == nil || code.Code == nil || !strings.Contains(table.Text(), "SYSTEM") || !strings.Contains(code.Text(), "tool") {
		t.Fatalf("adversarial content did not remain typed text: table=%+v code=%+v", table, code)
	}
	for _, value := range []string{table.Text(), code.Text()} {
		if strings.Contains(strings.ToLower(value), "image_url") || strings.Contains(strings.ToLower(value), "data:image") {
			t.Fatalf("enhancement introduced a visual content part marker: %q", value)
		}
	}
}
