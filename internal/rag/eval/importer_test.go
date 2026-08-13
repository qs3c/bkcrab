package eval

import (
	"context"
	"testing"
)

func TestCanonicalImporterRegistryAndMemoryBoundary(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(CanonicalImporterName, CanonicalImporter{}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(CanonicalImporterName, CanonicalImporter{}); err == nil {
		t.Fatal("duplicate importer registered")
	}
	if names := registry.Names(); len(names) != 1 || names[0] != CanonicalImporterName {
		t.Fatalf("registry names=%v", names)
	}
	importer, ok := registry.Lookup(CanonicalImporterName)
	if !ok {
		t.Fatal("canonical importer missing")
	}
	source := validDataset()
	normalized, err := importer.Normalize(context.Background(), ImportSource{Type: CanonicalImporterName, Payload: source})
	if err != nil {
		t.Fatal(err)
	}
	normalized.Corpus[0].FileName = "mutated"
	if source.Corpus[0].FileName == "mutated" {
		t.Fatal("canonical importer returned caller-owned memory")
	}
	report, err := importer.Validate(context.Background(), ImportSource{Type: CanonicalImporterName, Payload: source})
	if err != nil || !report.Valid || len(report.Coverage) == 0 {
		t.Fatalf("import report=%+v err=%v", report, err)
	}
	if _, err := importer.Normalize(context.Background(), ImportSource{Type: "jsonl", Payload: source}); err == nil {
		t.Fatal("canonical importer accepted an undecided external format")
	}
	invalid := source
	invalid.Cases = append([]Case(nil), source.Cases...)
	invalid.Cases[0].Tags = []string{string([]byte{0xff})}
	report, err = importer.Validate(context.Background(), ImportSource{Type: CanonicalImporterName, Payload: invalid})
	if err != nil || report.Valid || !validationHasCode(ValidationReport{Issues: report.Issues}, "invalid_utf8") {
		t.Fatalf("import validation lost invalid UTF-8: report=%+v err=%v", report, err)
	}
}
