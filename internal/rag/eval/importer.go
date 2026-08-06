package eval

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

type ImportSource struct {
	Type    string
	Payload any
}
type ImportReport struct {
	Valid  bool              `json:"valid"`
	Issues []ValidationIssue `json:"issues"`
}

type DatasetImporter interface {
	Validate(context.Context, ImportSource) (ImportReport, error)
	Normalize(context.Context, ImportSource) (CanonicalDataset, error)
}

type Registry struct {
	mu        sync.RWMutex
	importers map[string]DatasetImporter
}

func NewRegistry() *Registry { return &Registry{importers: make(map[string]DatasetImporter)} }
func (r *Registry) Register(name string, importer DatasetImporter) error {
	if name == "" || importer == nil {
		return errors.New("importer name and implementation are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.importers[name]; ok {
		return fmt.Errorf("importer %q already registered", name)
	}
	r.importers[name] = importer
	return nil
}
func (r *Registry) Lookup(name string) (DatasetImporter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	value, ok := r.importers[name]
	return value, ok
}

type CanonicalImporter struct{}

func (CanonicalImporter) Normalize(_ context.Context, source ImportSource) (CanonicalDataset, error) {
	value, ok := source.Payload.(CanonicalDataset)
	if !ok {
		return CanonicalDataset{}, errors.New("canonical importer requires CanonicalDataset payload")
	}
	return value, nil
}
func (CanonicalImporter) Validate(ctx context.Context, source ImportSource) (ImportReport, error) {
	value, err := (CanonicalImporter{}).Normalize(ctx, source)
	if err != nil {
		return ImportReport{}, err
	}
	report := ValidateDataset(value, DefaultValidationLimits())
	return ImportReport{report.Valid, report.Issues}, nil
}
