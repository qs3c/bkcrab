package eval

import (
	"context"
	"io"

	"github.com/qs3c/bkcrab/internal/store"
)

// AdminStore is the persistence boundary used by the HTTP-facing evaluation
// application service. HTTP handlers never assemble SQL, object keys, or
// evaluator payloads and can only use this bounded service surface.
type AdminStore interface {
	AnalysisStore
	ExportStore
	CreateRAGEvalDataset(context.Context, *store.RAGEvalDatasetRecord) error
	GetRAGEvalDataset(context.Context, string) (*store.RAGEvalDatasetRecord, error)
	ListRAGEvalDatasets(context.Context, string, int) ([]store.RAGEvalDatasetRecord, error)
	ListRAGEvalDatasetVersions(context.Context, string, string, int) ([]store.RAGEvalDatasetVersionRecord, error)
	GetRAGEvalDatasetVersion(context.Context, string) (*store.RAGEvalDatasetVersionRecord, error)
	CreateRAGEvalProfile(context.Context, *store.RAGEvalProfileRecord) error
	ListRAGEvalProfiles(context.Context, string, int) ([]store.RAGEvalProfileRecord, error)
	GetRAGEvalProfile(context.Context, string) (*store.RAGEvalProfileRecord, error)
	ListRAGEvalRuns(context.Context, string, int) ([]store.RAGEvalRunRecord, error)
	RequestCancelRAGEvalRun(context.Context, string) (bool, error)
	ActiveRAGPolicy(context.Context, string) (*store.RAGPolicyRecord, error)
	ListRAGPolicyAudits(context.Context, string, int) ([]store.RAGPolicyAuditRecord, error)
}

type AdminService struct{ store AdminStore }

func NewAdminService(value AdminStore) *AdminService {
	if value == nil {
		return nil
	}
	return &AdminService{store: value}
}

func (s *AdminService) CreateDataset(ctx context.Context, record *store.RAGEvalDatasetRecord) error {
	return s.store.CreateRAGEvalDataset(ctx, record)
}
func (s *AdminService) GetDataset(ctx context.Context, id string) (*store.RAGEvalDatasetRecord, error) {
	return s.store.GetRAGEvalDataset(ctx, id)
}
func (s *AdminService) ListDatasets(ctx context.Context, cursor string, limit int) ([]store.RAGEvalDatasetRecord, error) {
	return s.store.ListRAGEvalDatasets(ctx, cursor, limit)
}
func (s *AdminService) ListDatasetVersions(ctx context.Context, id, cursor string, limit int) ([]store.RAGEvalDatasetVersionRecord, error) {
	return s.store.ListRAGEvalDatasetVersions(ctx, id, cursor, limit)
}
func (s *AdminService) GetDatasetVersion(ctx context.Context, id string) (*store.RAGEvalDatasetVersionRecord, error) {
	return s.store.GetRAGEvalDatasetVersion(ctx, id)
}
func (s *AdminService) CreateProfile(ctx context.Context, record *store.RAGEvalProfileRecord) error {
	return s.store.CreateRAGEvalProfile(ctx, record)
}
func (s *AdminService) GetProfile(ctx context.Context, id string) (*store.RAGEvalProfileRecord, error) {
	return s.store.GetRAGEvalProfile(ctx, id)
}
func (s *AdminService) ListProfiles(ctx context.Context, cursor string, limit int) ([]store.RAGEvalProfileRecord, error) {
	return s.store.ListRAGEvalProfiles(ctx, cursor, limit)
}
func (s *AdminService) GetRun(ctx context.Context, id string) (*store.RAGEvalRunRecord, error) {
	return s.store.GetRAGEvalRun(ctx, id)
}
func (s *AdminService) ListRuns(ctx context.Context, cursor string, limit int) ([]store.RAGEvalRunRecord, error) {
	return s.store.ListRAGEvalRuns(ctx, cursor, limit)
}
func (s *AdminService) CancelRun(ctx context.Context, id string) (bool, error) {
	return s.store.RequestCancelRAGEvalRun(ctx, id)
}
func (s *AdminService) ListCaseResults(ctx context.Context, runID, cursor string, limit int) ([]store.RAGEvalCaseResultRecord, error) {
	return s.store.ListRAGEvalCaseResults(ctx, runID, cursor, limit)
}
func (s *AdminService) Compare(ctx context.Context, baselineID, candidateID, metric string) (PairedDelta, error) {
	return (AnalysisService{Store: s.store}).CompareRunMetric(ctx, baselineID, candidateID, metric)
}
func (s *AdminService) Export(ctx context.Context, actorID, runID string, format ExportFormat, includeTraces bool, dst io.Writer, authorizer ExportAuthorizer) error {
	return (ExportService{Store: s.store, Authorizer: authorizer}).Export(ctx, actorID, runID, format, includeTraces, dst)
}
func (s *AdminService) ActivePolicy(ctx context.Context, kind string) (*store.RAGPolicyRecord, error) {
	return s.store.ActiveRAGPolicy(ctx, kind)
}
func (s *AdminService) PolicyAudits(ctx context.Context, kind string, limit int) ([]store.RAGPolicyAuditRecord, error) {
	return s.store.ListRAGPolicyAudits(ctx, kind, limit)
}
