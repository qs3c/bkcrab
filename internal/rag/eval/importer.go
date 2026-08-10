package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

const CanonicalImporterName = "canonical-json"

type ImportSource struct {
	Type    string
	Payload any
}
type ImportReport struct {
	Valid    bool              `json:"valid"`
	Issues   []ValidationIssue `json:"issues"`
	Coverage []MetricCoverage  `json:"coverage"`
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
	name = strings.TrimSpace(name)
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

func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.importers))
	for name := range r.importers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

type CanonicalImporter struct{}

func canonicalDatasetSource(ctx context.Context, source ImportSource) (CanonicalDataset, error) {
	if err := ctx.Err(); err != nil {
		return CanonicalDataset{}, err
	}
	if source.Type != "" && source.Type != CanonicalImporterName {
		return CanonicalDataset{}, fmt.Errorf("canonical importer does not accept source type %q", source.Type)
	}
	value, ok := source.Payload.(CanonicalDataset)
	if !ok {
		return CanonicalDataset{}, errors.New("canonical importer requires CanonicalDataset payload")
	}
	return value, nil
}

func (CanonicalImporter) Normalize(ctx context.Context, source ImportSource) (CanonicalDataset, error) {
	value, err := canonicalDatasetSource(ctx, source)
	if err != nil {
		return CanonicalDataset{}, err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return CanonicalDataset{}, errors.New("canonical dataset contains non-JSON values")
	}
	var normalized CanonicalDataset
	if err = json.Unmarshal(encoded, &normalized); err != nil {
		return CanonicalDataset{}, errors.New("canonical dataset normalization failed")
	}
	return normalized, nil
}
func (CanonicalImporter) Validate(ctx context.Context, source ImportSource) (ImportReport, error) {
	value, err := canonicalDatasetSource(ctx, source)
	if err != nil {
		return ImportReport{}, err
	}
	report := ValidateDataset(value, DefaultValidationLimits())
	return ImportReport{Valid: report.Valid, Issues: report.Issues, Coverage: report.Coverage}, nil
}
