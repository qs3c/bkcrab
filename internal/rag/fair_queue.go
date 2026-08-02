package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/qs3c/bkcrab/internal/fairqueue"
	"github.com/qs3c/bkcrab/internal/store"
)

const (
	RAGFairQueueResource = "rag.index"
	RAGFairQueueTaskType = "rag_index"
)

// RAGFairQueueSource is the expected-writer-bound task surface. Implementations
// must retain the store's pinned-connection and full-guard CAS semantics.
type RAGFairQueueSource interface {
	ExpectedWriterFingerprint() string
	ListDispatchableRAGIndexTasksPage(context.Context, int64, int) ([]store.RAGIndexTaskDispatchCandidate, int64, error)
	GetDispatchableRAGIndexTaskByID(context.Context, int64) (*store.RAGIndexTaskDispatchCandidate, error)
	MarkRAGIndexTaskDispatched(context.Context, store.RAGIndexTaskDispatchCandidate) (bool, error)
	ArmExpiredRAGIndexTasksPage(context.Context, int64, int) ([]store.RAGIndexTaskDispatchCandidate, int64, error)
	CaptureRAGBrokerRepairHighWater(context.Context) (int64, error)
	ListBrokerBackedRAGCandidatesPage(context.Context, int64, int64, int) ([]store.RAGIndexTaskDispatchCandidate, int64, error)
	RearmRAGCandidateAfterBrokerLoss(context.Context, store.RAGIndexTaskDispatchCandidate) (*store.RAGIndexTaskDispatchCandidate, bool, error)
	ClaimRAGIndexTaskByID(context.Context, int64, string, int64, string, time.Duration, store.RAGFairQueueClaimLimits) (store.RAGFairQueueClaimResult, error)
	GetRAGPoisonRepairCandidate(context.Context, int64, int64) (*store.RAGIndexTaskDispatchCandidate, error)
	RearmRAGPoisonCandidate(context.Context, store.RAGIndexTaskDispatchCandidate) (*store.RAGIndexTaskDispatchCandidate, bool, error)
	GetRAGIndexTask(context.Context, int64) (*store.RAGIndexTaskRecord, error)
}

// RAGFairQueueAdminSource is opened with auto-migration disabled and bound to
// one expected writer. It owns recovery snapshots and writer-rebind checks.
type RAGFairQueueAdminSource interface {
	ReadWriterIdentity(context.Context) (store.FairQueueWriterIdentity, error)
	CheckSchemaAndInvariants(context.Context) (store.RAGFairQueueContractReport, error)
	CountValidRunning(context.Context) (int64, error)
	CaptureRAGFairQueueHighWater(context.Context) (int64, error)
	ListCanonicalRAGTenantsPage(context.Context, int64, string, int) ([]string, string, error)
	ListDispatchedRAGIndexTasksPage(context.Context, int64, int64, int) ([]store.RAGIndexTaskRecord, int64, error)
	ListValidRunningRAGIndexTasksPage(context.Context, int64, int64, int) ([]store.RAGIndexTaskRunningSnapshot, int64, error)
}

type RAGFairQueueRunner interface {
	RunFairClaim(context.Context, *store.RAGIndexClaim) error
}

type RAGFairQueueJournalStartSession interface {
	Read(context.Context) (store.FairQueueOperationRecord, bool, error)
	BeginSpecial(context.Context, *store.FairQueueOperationRecord, store.FairQueueOperationProposal) (store.FairQueueOperationRecord, error)
}

// RAGFairQueueJournalStore is intentionally fakeable. The concrete store
// callback has an invariant function type, so NewRAGFairQueueStoreJournal
// supplies the one thin bridge which preserves its pinned physical session.
type RAGFairQueueJournalStore interface {
	ReadFairQueueOperation(context.Context, string, string) (store.FairQueueOperationRecord, bool, error)
	WithFairQueueOperationStartFence(context.Context, string, string, func(RAGFairQueueJournalStartSession) error) error
	SetFairQueueOperationRepairHighWater(context.Context, store.FairQueueOperationRecord, string) (store.FairQueueOperationRecord, error)
	MarkFairQueueOperationRepairPassComplete(context.Context, store.FairQueueOperationRecord) (store.FairQueueOperationRecord, error)
	MarkFairQueueOperationForceDeletePassComplete(context.Context, store.FairQueueOperationRecord) (store.FairQueueOperationRecord, error)
	CommitFairQueueOperationReady(context.Context, store.FairQueueOperationRecord) (store.FairQueueOperationRecord, error)
	CompleteFairQueueOperation(context.Context, store.FairQueueOperationRecord) (store.FairQueueOperationRecord, error)
}

type ragFairQueueStoreJournal struct {
	source *store.RAGFairQueueAdminSource
}

func NewRAGFairQueueStoreJournal(source *store.RAGFairQueueAdminSource) (RAGFairQueueJournalStore, error) {
	if source == nil {
		return nil, errors.New("rag: nil fair queue journal source")
	}
	return &ragFairQueueStoreJournal{source: source}, nil
}

func (j *ragFairQueueStoreJournal) ReadFairQueueOperation(ctx context.Context, resource, writer string) (store.FairQueueOperationRecord, bool, error) {
	return j.source.ReadFairQueueOperation(ctx, resource, writer)
}
func (j *ragFairQueueStoreJournal) WithFairQueueOperationStartFence(ctx context.Context, resource, writer string, fn func(RAGFairQueueJournalStartSession) error) error {
	if fn == nil {
		return store.ErrFairQueueOperationInvalid
	}
	return j.source.WithFairQueueOperationStartFence(ctx, resource, writer, func(session *store.FairQueueOperationStartSession) error {
		return fn(session)
	})
}
func (j *ragFairQueueStoreJournal) SetFairQueueOperationRepairHighWater(ctx context.Context, expected store.FairQueueOperationRecord, highWater string) (store.FairQueueOperationRecord, error) {
	return j.source.SetFairQueueOperationRepairHighWater(ctx, expected, highWater)
}
func (j *ragFairQueueStoreJournal) MarkFairQueueOperationRepairPassComplete(ctx context.Context, expected store.FairQueueOperationRecord) (store.FairQueueOperationRecord, error) {
	return j.source.MarkFairQueueOperationRepairPassComplete(ctx, expected)
}
func (j *ragFairQueueStoreJournal) MarkFairQueueOperationForceDeletePassComplete(ctx context.Context, expected store.FairQueueOperationRecord) (store.FairQueueOperationRecord, error) {
	return j.source.MarkFairQueueOperationForceDeletePassComplete(ctx, expected)
}
func (j *ragFairQueueStoreJournal) CommitFairQueueOperationReady(ctx context.Context, expected store.FairQueueOperationRecord) (store.FairQueueOperationRecord, error) {
	return j.source.CommitFairQueueOperationReady(ctx, expected)
}
func (j *ragFairQueueStoreJournal) CompleteFairQueueOperation(ctx context.Context, expected store.FairQueueOperationRecord) (store.FairQueueOperationRecord, error) {
	return j.source.CompleteFairQueueOperation(ctx, expected)
}

type RAGFairQueueAdapterOptions struct {
	WorkerID            string
	LeaseDuration       time.Duration
	ClaimLimits         store.RAGFairQueueClaimLimits
	ClaimRecheckTimeout time.Duration
}

type RAGFairQueueAdapter struct {
	source  RAGFairQueueSource
	runner  RAGFairQueueRunner
	admin   RAGFairQueueAdminSource
	journal *RAGFairQueueOperationJournal
	options RAGFairQueueAdapterOptions
	writer  string
}

func NewRAGFairQueueAdapter(
	source RAGFairQueueSource,
	runner RAGFairQueueRunner,
	admin RAGFairQueueAdminSource,
	journal RAGFairQueueJournalStore,
	options RAGFairQueueAdapterOptions,
) (*RAGFairQueueAdapter, error) {
	if nilInterface(source) || nilInterface(runner) || nilInterface(admin) || nilInterface(journal) {
		return nil, errors.New("rag: fair queue adapter dependencies are required")
	}
	writer := source.ExpectedWriterFingerprint()
	if err := (fairqueue.WriterIdentity{Fingerprint: writer}).Validate(); err != nil {
		return nil, errors.Join(fairqueue.ErrAuthoritativeWriterMismatch, err)
	}
	if options.WorkerID == "" || options.WorkerID != strings.TrimSpace(options.WorkerID) || len(options.WorkerID) > 96 {
		return nil, errors.New("rag: fair queue worker ID must be 1..96 trimmed bytes")
	}
	if options.LeaseDuration <= 0 || options.ClaimLimits.GlobalConcurrency <= 0 ||
		options.ClaimLimits.PerUserBurstConcurrency <= 0 ||
		options.ClaimLimits.PerUserBurstConcurrency > options.ClaimLimits.GlobalConcurrency {
		return nil, errors.New("rag: invalid fair queue claim limits")
	}
	if options.ClaimRecheckTimeout == 0 {
		options.ClaimRecheckTimeout = 2 * time.Second
	}
	if options.ClaimRecheckTimeout <= 0 || options.ClaimRecheckTimeout > time.Minute {
		return nil, errors.New("rag: invalid fair queue claim recheck timeout")
	}
	operationJournal, err := NewRAGFairQueueOperationJournal(journal)
	if err != nil {
		return nil, err
	}
	return &RAGFairQueueAdapter{
		source: source, runner: runner, admin: admin, journal: operationJournal,
		options: options, writer: writer,
	}, nil
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return rv.IsNil()
	default:
		return false
	}
}

func (a *RAGFairQueueAdapter) OperationJournal() fairqueue.OperationJournal { return a.journal }
func (a *RAGFairQueueAdapter) ExpectedWriterFingerprint() string {
	if a == nil {
		return ""
	}
	return a.writer
}

func (a *RAGFairQueueAdapter) ResourceRegistration(config fairqueue.ResourceConfig) (fairqueue.ResourceRegistration, error) {
	if a == nil {
		return fairqueue.ResourceRegistration{}, errors.New("rag: nil fair queue adapter")
	}
	if config.Key == "" {
		config.Key = RAGFairQueueResource
	}
	if config.Key != RAGFairQueueResource {
		return fairqueue.ResourceRegistration{}, fmt.Errorf("rag: fair queue resource must be %q", RAGFairQueueResource)
	}
	config.ValidateTaskID = fairqueue.ValidateRAGIndexTaskID
	if err := config.Validate(); err != nil {
		return fairqueue.ResourceRegistration{}, err
	}
	return fairqueue.ResourceRegistration{
		Config: config, Preparer: a, DispatchSource: a, ExpiredRearmSource: a,
		RecoverySource: a, WriterFingerprint: a.writer,
	}, nil
}

func formatRAGFairQueueID(id int64) string { return strconv.FormatInt(id, 10) }

func parseRAGFairQueueID(value string, allowEmpty bool) (int64, error) {
	if allowEmpty && value == "" {
		return 0, nil
	}
	if !fairqueue.ValidateRAGIndexTaskID(value) {
		return 0, fmt.Errorf("%w: invalid canonical RAG task ID %q", fairqueue.ErrInvalidModel, value)
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 || formatRAGFairQueueID(id) != value {
		return 0, fmt.Errorf("%w: invalid canonical RAG task ID %q", fairqueue.ErrInvalidModel, value)
	}
	return id, nil
}

func parseRAGHighWater(value string) (int64, error) {
	if value == "0" {
		return 0, nil
	}
	return parseRAGFairQueueID(value, false)
}

func normalizeRAGCursor(after string, next int64, items int) (string, error) {
	if next < 0 {
		return "", authoritativeStateError("negative store cursor")
	}
	if next == 0 && after == "" {
		if items != 0 {
			return "", authoritativeStateError("non-empty page did not advance")
		}
		return "", nil
	}
	nextCursor := formatRAGFairQueueID(next)
	if nextCursor == after {
		if items != 0 {
			return "", authoritativeStateError("non-empty page did not advance")
		}
		return "", nil
	}
	return nextCursor, nil
}

func mapRAGFairQueueStoreError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, store.ErrFairQueueWriterMismatch), errors.Is(err, store.ErrFairQueueUnsafeConnection):
		return errors.Join(fairqueue.ErrAuthoritativeWriterMismatch, err)
	case errors.Is(err, store.ErrRAGFairQueueCanonicalOwner),
		errors.Is(err, store.ErrRAGIndexTaskDispatchGuard),
		errors.Is(err, store.ErrRAGDispatchGenerationExhausted),
		errors.Is(err, store.ErrRAGDocumentVersionMismatch),
		errors.Is(err, store.ErrRAGDocumentVersionConflict),
		errors.Is(err, store.ErrRAGDocumentSourceConflict),
		errors.Is(err, store.ErrRAGDocumentAILedgerCorrupt):
		return errors.Join(fairqueue.ErrAuthoritativeStateCorrupt, err)
	case errors.Is(err, store.ErrFairQueueOperationInvalid):
		return errors.Join(fairqueue.ErrInvalidOperationRecord, err)
	default:
		return err
	}
}

func mapRAGFairQueuePersistedJournalError(err error) error {
	mapped := mapRAGFairQueueStoreError(err)
	if errors.Is(mapped, fairqueue.ErrInvalidOperationRecord) {
		return errors.Join(fairqueue.ErrAuthoritativeStateCorrupt, mapped)
	}
	return mapped
}

func ragFatalStoreError(err error) error {
	if err == nil {
		return nil
	}
	mapped := mapRAGFairQueueStoreError(err)
	if errors.Is(mapped, fairqueue.ErrAuthoritativeWriterMismatch) ||
		errors.Is(mapped, fairqueue.ErrAuthoritativeStateCorrupt) {
		return mapped
	}
	return nil
}

func authoritativeStateError(message string) error {
	return errors.Join(fairqueue.ErrAuthoritativeStateCorrupt, errors.New("rag: "+message))
}

func sameRAGTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func validRAGTimestampGuard(raw store.RAGIndexTaskTimestampGuard, value *time.Time) bool {
	if raw.IsNull {
		return raw.Raw == "" && value == nil
	}
	if raw.Raw == "" || value == nil {
		return false
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04:05.999999999 -0700 MST",
	} {
		if parsed, err := time.Parse(layout, raw.Raw); err == nil && parsed.Equal(*value) {
			return true
		}
	}
	return false
}

func validateRAGStoreCandidate(candidate store.RAGIndexTaskDispatchCandidate) error {
	task, guard := candidate.Task, candidate.Guard
	if task.ID <= 0 || task.DocID == "" || task.DocVersion <= 0 ||
		fairqueue.ValidateTenantID(task.UserID) != nil ||
		(task.Status != "PENDING" && task.Status != "RUNNING") ||
		task.RetryCount < 0 || task.MaxRetry < 0 || task.RetryCount > task.MaxRetry ||
		task.DispatchGeneration <= 0 || task.DispatchGeneration > math.MaxInt64 || task.ClaimGeneration < 0 ||
		task.DispatchGeneration <= task.ClaimGeneration ||
		guard.TaskID != task.ID || guard.DocID != task.DocID || guard.DocVersion != task.DocVersion ||
		guard.UserID != task.UserID || guard.Status != task.Status ||
		guard.DispatchGeneration != task.DispatchGeneration || guard.ClaimGeneration != task.ClaimGeneration ||
		guard.RetryCount != task.RetryCount || guard.LeaseOwner != task.LeaseOwner ||
		!sameRAGTime(guard.NextRunAt, task.NextRunAt) || !sameRAGTime(guard.LeaseUntil, task.LeaseUntil) ||
		!sameRAGTime(guard.DispatchedAt, task.DispatchedAt) ||
		!validRAGTimestampGuard(guard.NextRunAtRaw, guard.NextRunAt) ||
		!validRAGTimestampGuard(guard.LeaseUntilRaw, guard.LeaseUntil) ||
		!validRAGTimestampGuard(guard.DispatchedAtRaw, guard.DispatchedAt) {
		return authoritativeStateError("canonical dispatch candidate contradicts its guard")
	}
	return nil
}

func validateRAGRearmedCandidate(original, updated store.RAGIndexTaskDispatchCandidate) error {
	if original.Task.DispatchGeneration == math.MaxInt64 {
		return authoritativeStateError("canonical generation exhausted during repair")
	}
	expected := original
	expected.Task.DispatchGeneration++
	expected.Task.DispatchedAt = nil
	expected.Guard.DispatchGeneration++
	expected.Guard.DispatchedAt = nil
	expected.Guard.DispatchedAtRaw = store.RAGIndexTaskTimestampGuard{IsNull: true}
	if !reflect.DeepEqual(expected, updated) {
		return authoritativeStateError("repair changed fields outside generation and publish marker")
	}
	return validateRAGStoreCandidate(updated)
}

func ragMessageFromCandidate(candidate store.RAGIndexTaskDispatchCandidate) fairqueue.Message {
	taskID := formatRAGFairQueueID(candidate.Task.ID)
	return fairqueue.Message{
		Version: fairqueue.MessageVersion1, Resource: RAGFairQueueResource,
		TenantID: candidate.Task.UserID, TaskType: RAGFairQueueTaskType, TaskID: taskID,
		DispatchToken: fairqueue.DispatchToken{
			Resource: RAGFairQueueResource, TaskID: taskID,
			Generation: uint64(candidate.Task.DispatchGeneration),
		},
	}
}

func encodeRAGDispatchCandidate(candidate store.RAGIndexTaskDispatchCandidate) (fairqueue.DispatchCandidate, error) {
	if err := validateRAGStoreCandidate(candidate); err != nil {
		return fairqueue.DispatchCandidate{}, err
	}
	raw, err := json.Marshal(candidate)
	if err != nil {
		return fairqueue.DispatchCandidate{}, authoritativeStateError("cannot encode canonical dispatch guard")
	}
	converted := fairqueue.DispatchCandidate{Message: ragMessageFromCandidate(candidate), Guard: string(raw)}
	if err := converted.Validate(); err != nil {
		return fairqueue.DispatchCandidate{}, errors.Join(fairqueue.ErrAuthoritativeStateCorrupt, err)
	}
	return converted, nil
}

func decodeRAGDispatchCandidate(candidate fairqueue.DispatchCandidate) (store.RAGIndexTaskDispatchCandidate, error) {
	if err := candidate.Validate(); err != nil {
		return store.RAGIndexTaskDispatchCandidate{}, err
	}
	if err := rejectDuplicateRAGGuardFields([]byte(candidate.Guard)); err != nil {
		return store.RAGIndexTaskDispatchCandidate{}, errors.Join(fairqueue.ErrAuthoritativeStateCorrupt, err)
	}
	decoder := json.NewDecoder(strings.NewReader(candidate.Guard))
	decoder.DisallowUnknownFields()
	var decoded store.RAGIndexTaskDispatchCandidate
	if err := decoder.Decode(&decoded); err != nil {
		return decoded, errors.Join(fairqueue.ErrAuthoritativeStateCorrupt, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("trailing JSON value")
		}
		return decoded, errors.Join(fairqueue.ErrAuthoritativeStateCorrupt, err)
	}
	if err := validateRAGStoreCandidate(decoded); err != nil {
		return decoded, err
	}
	if candidate.Message != ragMessageFromCandidate(decoded) {
		return decoded, authoritativeStateError("wire identity contradicts opaque canonical guard")
	}
	return decoded, nil
}

func rejectDuplicateRAGGuardFields(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := walkUniqueRAGGuardValue(decoder); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func walkUniqueRAGGuardValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object key is not a string")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate JSON field %q", key)
			}
			seen[key] = struct{}{}
			if err := walkUniqueRAGGuardValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("unterminated JSON object")
		}
		return nil
	case '[':
		for decoder.More() {
			if err := walkUniqueRAGGuardValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("unterminated JSON array")
		}
		return nil
	default:
		return errors.New("unexpected JSON delimiter")
	}
}

func (a *RAGFairQueueAdapter) ListDispatchCandidates(ctx context.Context, after string, limit int) ([]fairqueue.DispatchCandidate, string, error) {
	afterID, err := parseRAGFairQueueID(after, true)
	if err != nil {
		return nil, after, err
	}
	page, next, err := a.source.ListDispatchableRAGIndexTasksPage(ctx, afterID, limit)
	if err != nil {
		return nil, after, mapRAGFairQueueStoreError(err)
	}
	if err := validateRAGOrderedCandidates(page, afterID, 0, false, next); err != nil {
		return nil, after, err
	}
	converted, err := convertRAGCandidates(page)
	if err != nil {
		return nil, after, err
	}
	nextCursor, err := normalizeRAGCursor(after, next, len(converted))
	if err != nil {
		return nil, after, err
	}
	return converted, nextCursor, nil
}

func (a *RAGFairQueueAdapter) GetDispatchableByID(ctx context.Context, taskID string) (fairqueue.DispatchCandidate, bool, error) {
	id, err := parseRAGFairQueueID(taskID, false)
	if err != nil {
		return fairqueue.DispatchCandidate{}, false, err
	}
	candidate, err := a.source.GetDispatchableRAGIndexTaskByID(ctx, id)
	if fatal := ragFatalStoreError(err); fatal != nil {
		return fairqueue.DispatchCandidate{}, false, fatal
	}
	if errors.Is(err, store.ErrNotFound) || (err == nil && candidate == nil) {
		return fairqueue.DispatchCandidate{}, false, nil
	}
	if err != nil {
		return fairqueue.DispatchCandidate{}, false, mapRAGFairQueueStoreError(err)
	}
	converted, err := encodeRAGDispatchCandidate(*candidate)
	if err != nil {
		return fairqueue.DispatchCandidate{}, false, err
	}
	if converted.Message.TaskID != taskID {
		return fairqueue.DispatchCandidate{}, false, authoritativeStateError("by-ID source returned a different task")
	}
	return converted, true, nil
}

func (a *RAGFairQueueAdapter) MarkDispatched(ctx context.Context, candidate fairqueue.DispatchCandidate) (bool, error) {
	decoded, err := decodeRAGDispatchCandidate(candidate)
	if err != nil {
		return false, err
	}
	changed, err := a.source.MarkRAGIndexTaskDispatched(ctx, decoded)
	if fatal := ragFatalStoreError(err); fatal != nil {
		return false, fatal
	}
	if errors.Is(err, store.ErrRAGIndexTaskDispatchStale) {
		return false, nil
	}
	return changed, mapRAGFairQueueStoreError(err)
}

func (a *RAGFairQueueAdapter) RearmExpiredPage(ctx context.Context, after string, limit int) ([]fairqueue.DispatchCandidate, string, error) {
	afterID, err := parseRAGFairQueueID(after, true)
	if err != nil {
		return nil, after, err
	}
	page, next, err := a.source.ArmExpiredRAGIndexTasksPage(ctx, afterID, limit)
	if err != nil {
		return nil, after, mapRAGFairQueueStoreError(err)
	}
	if err := validateRAGOrderedCandidates(page, afterID, 0, false, next); err != nil {
		return nil, after, err
	}
	converted, err := convertRAGCandidates(page)
	if err != nil {
		return nil, after, err
	}
	nextCursor, err := normalizeRAGCursor(after, next, len(converted))
	if err != nil {
		return nil, after, err
	}
	return converted, nextCursor, nil
}

func convertRAGCandidates(page []store.RAGIndexTaskDispatchCandidate) ([]fairqueue.DispatchCandidate, error) {
	converted := make([]fairqueue.DispatchCandidate, 0, len(page))
	for i := range page {
		candidate, err := encodeRAGDispatchCandidate(page[i])
		if err != nil {
			return nil, err
		}
		converted = append(converted, candidate)
	}
	return converted, nil
}

func validateRAGOrderedCandidates(page []store.RAGIndexTaskDispatchCandidate, after, high int64, bounded bool, next int64) error {
	previous := after
	for i := range page {
		id := page[i].Task.ID
		if id <= previous || (bounded && id > high) {
			return authoritativeStateError("canonical candidate page is unordered or escaped its high water")
		}
		previous = id
	}
	if next < previous || (bounded && next > high) {
		return authoritativeStateError("canonical candidate page cursor precedes its records")
	}
	return nil
}

type ragFairQueuePreparedTask struct {
	runner RAGFairQueueRunner
	claim  *store.RAGIndexClaim
}

func (p *ragFairQueuePreparedTask) Run(ctx context.Context) error {
	if p == nil || nilInterface(p.runner) || p.claim == nil {
		return authoritativeStateError("invalid prepared RAG claim")
	}
	return mapRAGFairQueueStoreError(p.runner.RunFairClaim(ctx, p.claim))
}

func (a *RAGFairQueueAdapter) Prepare(ctx context.Context, request fairqueue.PrepareRequest) (fairqueue.PreparedTask, fairqueue.PrepareResult, error) {
	if err := request.Validate(); err != nil {
		return nil, fairqueue.PrepareResult{}, err
	}
	if request.RegisteredResource != RAGFairQueueResource {
		return nil, fairqueue.PrepareResult{}, authoritativeStateError("delivery reached the wrong resource adapter")
	}
	if request.Message == nil || request.Message.TaskType != RAGFairQueueTaskType {
		return a.preparePoison(ctx, request)
	}
	message := *request.Message
	id, err := parseRAGFairQueueID(message.TaskID, false)
	if err != nil {
		return nil, fairqueue.PrepareResult{}, err
	}
	claimResult, claimErr := a.source.ClaimRAGIndexTaskByID(
		ctx, id, message.TenantID, int64(message.DispatchToken.Generation),
		a.options.WorkerID, a.options.LeaseDuration, a.options.ClaimLimits,
	)
	if claimErr != nil {
		freshErr := a.freshReadAfterClaimError(ctx, id, message)
		return nil, fairqueue.PrepareResult{}, mapRAGFairQueueStoreError(errors.Join(claimErr, freshErr))
	}
	if claimResult.Disposition != store.RAGFairQueueClaimed && claimResult.Claim != nil {
		return nil, fairqueue.PrepareResult{}, authoritativeStateError("non-claimed disposition carried a claim")
	}
	switch claimResult.Disposition {
	case store.RAGFairQueueClaimed:
		if err := a.validateClaim(message, claimResult.Claim); err != nil {
			return nil, fairqueue.PrepareResult{}, err
		}
		prepared := &ragFairQueuePreparedTask{runner: a.runner, claim: claimResult.Claim}
		result := fairqueue.PrepareResult{
			Disposition: fairqueue.PrepareClaimed, DeliveryAction: fairqueue.DeliveryPromoteThenAckRun,
			CanonicalEffect: fairqueue.CanonicalClaimCommitted,
			Claim:           &fairqueue.ClaimRef{TenantID: message.TenantID, TaskID: message.TaskID, ClaimGeneration: message.DispatchToken.Generation},
		}
		return prepared, result, nil
	case store.RAGFairQueueClaimCapacityDeferred:
		return nil, prepareResult(fairqueue.PrepareCapacityDeferred, fairqueue.DeliveryNackRequeue, fairqueue.CanonicalNone), nil
	case store.RAGFairQueueClaimDuplicateStale:
		return nil, prepareResult(fairqueue.PrepareDuplicateStaleTerminal, fairqueue.DeliveryAckRelease, fairqueue.CanonicalNone), nil
	case store.RAGFairQueueClaimCanonicalTerminal:
		return nil, prepareResult(fairqueue.PrepareCanonicalRepairedTerminal, fairqueue.DeliveryAckRelease, fairqueue.CanonicalTerminalRepairCommitted), nil
	case store.RAGFairQueueClaimCanonicalRetry:
		return nil, prepareResult(fairqueue.PrepareCanonicalRepairedRetry, fairqueue.DeliveryAckRelease, fairqueue.CanonicalRetryRepairCommitted), nil
	case store.RAGFairQueueClaimPoison, store.RAGFairQueueClaimPoisonRepaired:
		return a.preparePoison(ctx, request)
	default:
		return nil, fairqueue.PrepareResult{}, authoritativeStateError("unknown canonical claim disposition")
	}
}

func prepareResult(disposition fairqueue.PrepareDisposition, action fairqueue.DeliveryAction, effect fairqueue.CanonicalEffect) fairqueue.PrepareResult {
	return fairqueue.PrepareResult{Disposition: disposition, DeliveryAction: action, CanonicalEffect: effect}
}

func (a *RAGFairQueueAdapter) validateClaim(message fairqueue.Message, claim *store.RAGIndexClaim) error {
	if claim == nil {
		return authoritativeStateError("claimed disposition omitted its claim")
	}
	task, fence := claim.Task, claim.Fence
	if task.ID <= 0 || formatRAGFairQueueID(task.ID) != message.TaskID || task.UserID != message.TenantID ||
		task.DocID == "" || task.DocVersion <= 0 || task.Status != "RUNNING" ||
		task.DispatchGeneration != int64(message.DispatchToken.Generation) ||
		task.ClaimGeneration != int64(message.DispatchToken.Generation) || task.LeaseOwner != a.options.WorkerID ||
		task.LeaseUntil == nil || task.HeartbeatAt == nil || task.DispatchedAt == nil || task.NextRunAt != nil ||
		task.LeaseUntil.IsZero() || task.HeartbeatAt.IsZero() || task.DispatchedAt.IsZero() ||
		!task.LeaseUntil.After(*task.HeartbeatAt) || task.HeartbeatAt.Before(*task.DispatchedAt) ||
		task.RetryCount < 0 || task.MaxRetry < 0 || task.RetryCount > task.MaxRetry ||
		fence.TaskID != task.ID || fence.DocID != task.DocID || fence.DocVersion != task.DocVersion ||
		fence.ClaimGeneration != task.ClaimGeneration || fence.LeaseOwner != task.LeaseOwner ||
		fence.ExpectedWriterFingerprint != a.writer || claim.Version.DocID != task.DocID ||
		claim.Version.DocVersion != task.DocVersion || claim.Version.Status != store.RAGDocumentVersionRunning {
		return authoritativeStateError("claimed task contradicts delivery or execution fence")
	}
	return nil
}

func (a *RAGFairQueueAdapter) freshReadAfterClaimError(parent context.Context, taskID int64, message fairqueue.Message) error {
	base := context.Background()
	if parent != nil {
		base = context.WithoutCancel(parent)
	}
	ctx, cancel := context.WithTimeout(base, a.options.ClaimRecheckTimeout)
	defer cancel()
	task, err := a.source.GetRAGIndexTask(ctx, taskID)
	if fatal := ragFatalStoreError(err); fatal != nil {
		return fatal
	}
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if task == nil {
		return nil
	}
	if err := validateRAGIdentityTask(*task, taskID); err != nil {
		return err
	}
	// The exact identity check is deliberate even though an uncertain outcome
	// still returns an error/NACK. It proves we never infer commit from a stale
	// tenant, worker, or generation before leaving the claim boundary.
	if task.Status == "RUNNING" && task.UserID == message.TenantID &&
		task.DispatchGeneration == int64(message.DispatchToken.Generation) &&
		task.ClaimGeneration == int64(message.DispatchToken.Generation) &&
		task.LeaseOwner == a.options.WorkerID {
		return nil
	}
	return nil
}

type ragPoisonLocator struct {
	id, generation int64
	bodyTenant     string
	header         bool
}

type ragPoisonCapture struct {
	locator   ragPoisonLocator
	candidate *store.RAGIndexTaskDispatchCandidate
	settled   bool
}

func (a *RAGFairQueueAdapter) preparePoison(ctx context.Context, request fairqueue.PrepareRequest) (fairqueue.PreparedTask, fairqueue.PrepareResult, error) {
	locators := make([]ragPoisonLocator, 0, 2)
	index := make(map[string]int)
	add := func(token fairqueue.DispatchToken, bodyTenant string, header bool) {
		if token.Resource != RAGFairQueueResource || token.Generation == 0 || token.Generation > math.MaxInt64 {
			return
		}
		id, err := parseRAGFairQueueID(token.TaskID, false)
		if err != nil {
			return
		}
		key := token.TaskID + "/" + strconv.FormatUint(token.Generation, 10)
		if existing, ok := index[key]; ok {
			if header {
				locators[existing].header = true
			}
			if locators[existing].bodyTenant == "" {
				locators[existing].bodyTenant = bodyTenant
			}
			return
		}
		index[key] = len(locators)
		locators = append(locators, ragPoisonLocator{id: id, generation: int64(token.Generation), bodyTenant: bodyTenant, header: header})
	}
	if request.BodyCandidate != nil {
		add(request.BodyCandidate.DispatchToken, request.BodyCandidate.TenantID, false)
	}
	if request.HeaderToken != nil {
		add(*request.HeaderToken, "", true)
	}

	// Capture and constrain every locator before the first canonical mutation.
	captures := make([]ragPoisonCapture, 0, len(locators))
	for _, locator := range locators {
		capture, err := a.capturePoisonLocator(ctx, request, locator)
		if err != nil {
			return nil, fairqueue.PrepareResult{}, mapRAGFairQueueStoreError(err)
		}
		captures = append(captures, capture)
	}
	settled := false
	for _, capture := range captures {
		if capture.settled {
			settled = true
		}
		if capture.candidate == nil {
			continue
		}
		updated, changed, err := a.source.RearmRAGPoisonCandidate(ctx, *capture.candidate)
		if err != nil {
			return nil, fairqueue.PrepareResult{}, mapRAGFairQueueStoreError(err)
		}
		settled = true
		if !changed {
			if updated != nil {
				return nil, fairqueue.PrepareResult{}, authoritativeStateError("stale poison repair returned a candidate")
			}
			continue
		}
		if updated == nil {
			return nil, fairqueue.PrepareResult{}, authoritativeStateError("poison repair changed without a candidate")
		}
		if err := validateRAGRearmedCandidate(*capture.candidate, *updated); err != nil {
			return nil, fairqueue.PrepareResult{}, err
		}
	}
	effect := fairqueue.CanonicalNone
	if settled {
		effect = fairqueue.CanonicalPoisonRepairSettled
	}
	return nil, prepareResult(fairqueue.PreparePoisonPermanentInvalidMessage, fairqueue.DeliveryConfirmDLQThenAck, effect), nil
}

func (a *RAGFairQueueAdapter) capturePoisonLocator(ctx context.Context, request fairqueue.PrepareRequest, locator ragPoisonLocator) (ragPoisonCapture, error) {
	capture := ragPoisonCapture{locator: locator}
	candidate, err := a.source.GetRAGPoisonRepairCandidate(ctx, locator.id, locator.generation)
	if fatal := ragFatalStoreError(err); fatal != nil {
		return capture, fatal
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return capture, err
	}
	if err == nil && candidate != nil {
		if err := validateRAGStoreCandidate(*candidate); err != nil {
			return capture, err
		}
		if candidate.Task.ID != locator.id || candidate.Task.DispatchGeneration != locator.generation {
			return capture, authoritativeStateError("poison lookup returned a different canonical locator")
		}
		allowed, err := poisonTenantAllowed(request, locator, candidate.Task.UserID)
		if err != nil {
			return capture, err
		}
		if allowed {
			capture.candidate, capture.settled = candidate, true
		}
		return capture, nil
	}

	// A terminal, already-claimed, or superseded generation is settled even
	// when it no longer satisfies the repair-candidate predicate. A missing task
	// remains explicitly unlocatable.
	task, readErr := a.source.GetRAGIndexTask(ctx, locator.id)
	if fatal := ragFatalStoreError(readErr); fatal != nil {
		return capture, fatal
	}
	if errors.Is(readErr, store.ErrNotFound) || (readErr == nil && task == nil) {
		return capture, nil
	}
	if readErr != nil {
		return capture, readErr
	}
	if err := validateRAGIdentityTask(*task, locator.id); err != nil {
		return capture, err
	}
	allowed, err := poisonTenantAllowed(request, locator, task.UserID)
	if err != nil {
		return capture, err
	}
	if allowed {
		capture.settled = true
	}
	return capture, nil
}

func validateRAGIdentityTask(task store.RAGIndexTaskRecord, expectedID int64) error {
	if task.ID != expectedID || task.ID <= 0 || task.DocID == "" || task.DocVersion <= 0 ||
		fairqueue.ValidateTenantID(task.UserID) != nil || task.Status == "" || task.Status != strings.TrimSpace(task.Status) ||
		task.RetryCount < 0 || task.MaxRetry < 0 || task.RetryCount > task.MaxRetry ||
		task.DispatchGeneration <= 0 || task.DispatchGeneration > math.MaxInt64 ||
		task.ClaimGeneration < 0 || task.DispatchGeneration < task.ClaimGeneration {
		return authoritativeStateError("canonical task identity read is malformed")
	}
	return nil
}

func poisonTenantAllowed(request fairqueue.PrepareRequest, locator ragPoisonLocator, userID string) (bool, error) {
	if err := fairqueue.ValidateTenantID(userID); err != nil {
		return false, authoritativeStateError("canonical poison candidate has an invalid owner")
	}
	hash, err := fairqueue.TenantHash(RAGFairQueueResource, userID)
	if err != nil {
		return false, authoritativeStateError("canonical poison owner cannot be hashed")
	}
	if hash != request.QueueTenantHash {
		return false, nil
	}
	if !locator.header && locator.bodyTenant != userID {
		return false, nil
	}
	return true, nil
}

func (a *RAGFairQueueAdapter) CaptureHighWater(ctx context.Context) (string, error) {
	high, err := a.admin.CaptureRAGFairQueueHighWater(ctx)
	if err != nil {
		return "", mapRAGFairQueueStoreError(err)
	}
	if high < 0 {
		return "", authoritativeStateError("negative recovery high water")
	}
	return formatRAGFairQueueID(high), nil
}

func (a *RAGFairQueueAdapter) ListKnownTenants(ctx context.Context, highWater, after string, limit int) (fairqueue.RecoveryPage[fairqueue.TenantRef], error) {
	high, err := parseRAGHighWater(highWater)
	if err != nil {
		return fairqueue.RecoveryPage[fairqueue.TenantRef]{}, err
	}
	tenants, next, err := a.admin.ListCanonicalRAGTenantsPage(ctx, high, after, limit)
	if err != nil {
		return fairqueue.RecoveryPage[fairqueue.TenantRef]{}, mapRAGFairQueueStoreError(err)
	}
	items := make([]fairqueue.TenantRef, 0, len(tenants))
	seen := make(map[string]struct{}, len(tenants))
	for _, tenant := range tenants {
		if err := fairqueue.ValidateTenantID(tenant); err != nil {
			return fairqueue.RecoveryPage[fairqueue.TenantRef]{}, authoritativeStateError("invalid canonical tenant")
		}
		if _, duplicate := seen[tenant]; duplicate {
			return fairqueue.RecoveryPage[fairqueue.TenantRef]{}, authoritativeStateError("duplicate canonical tenant")
		}
		seen[tenant] = struct{}{}
		items = append(items, fairqueue.TenantRef{TenantID: tenant})
	}
	if high == 0 && len(items) != 0 {
		return fairqueue.RecoveryPage[fairqueue.TenantRef]{}, authoritativeStateError("empty high water returned tenants")
	}
	page := fairqueue.RecoveryPage[fairqueue.TenantRef]{Items: items, NextCursor: next}
	if next == after {
		if len(items) != 0 {
			return fairqueue.RecoveryPage[fairqueue.TenantRef]{}, authoritativeStateError("known-tenant page did not advance")
		}
		page.NextCursor, page.Done = "", true
	}
	if err := page.Validate(after, limit); err != nil {
		return fairqueue.RecoveryPage[fairqueue.TenantRef]{}, authoritativeStateError("invalid known-tenant recovery page")
	}
	return page, nil
}

func (a *RAGFairQueueAdapter) ListDispatched(ctx context.Context, highWater, after string, limit int) (fairqueue.RecoveryPage[fairqueue.DispatchedRef], error) {
	high, afterID, err := parseRAGRecoveryWindow(highWater, after)
	if err != nil {
		return fairqueue.RecoveryPage[fairqueue.DispatchedRef]{}, err
	}
	rows, next, err := a.admin.ListDispatchedRAGIndexTasksPage(ctx, high, afterID, limit)
	if err != nil {
		return fairqueue.RecoveryPage[fairqueue.DispatchedRef]{}, mapRAGFairQueueStoreError(err)
	}
	if err := validateRAGOrderedTasks(rows, afterID, high, next); err != nil {
		return fairqueue.RecoveryPage[fairqueue.DispatchedRef]{}, err
	}
	items := make([]fairqueue.DispatchedRef, 0, len(rows))
	for _, task := range rows {
		validGeneration := task.DispatchGeneration > 0 && task.DispatchGeneration <= math.MaxInt64 &&
			task.ClaimGeneration >= 0 && ((task.Status == "PENDING" && task.DispatchGeneration > task.ClaimGeneration) ||
			(task.Status == "RUNNING" && task.ClaimGeneration > 0 && task.DispatchGeneration >= task.ClaimGeneration))
		if task.ID <= 0 || task.DocID == "" || task.DocVersion <= 0 || !validGeneration ||
			task.DispatchedAt == nil || fairqueue.ValidateTenantID(task.UserID) != nil {
			return fairqueue.RecoveryPage[fairqueue.DispatchedRef]{}, authoritativeStateError("invalid dispatched recovery row")
		}
		items = append(items, fairqueue.DispatchedRef{TenantID: task.UserID, Token: fairqueue.DispatchToken{
			Resource: RAGFairQueueResource, TaskID: formatRAGFairQueueID(task.ID), Generation: uint64(task.DispatchGeneration),
		}})
	}
	return finishRAGIDRecoveryPage(items, high, after, next, limit)
}

func (a *RAGFairQueueAdapter) ListValidRunning(ctx context.Context, highWater, after string, limit int) (fairqueue.RecoveryPage[fairqueue.RunningLease], error) {
	high, afterID, err := parseRAGRecoveryWindow(highWater, after)
	if err != nil {
		return fairqueue.RecoveryPage[fairqueue.RunningLease]{}, err
	}
	rows, next, err := a.admin.ListValidRunningRAGIndexTasksPage(ctx, high, afterID, limit)
	if err != nil {
		return fairqueue.RecoveryPage[fairqueue.RunningLease]{}, mapRAGFairQueueStoreError(err)
	}
	plainRows := make([]store.RAGIndexTaskRecord, 0, len(rows))
	for i := range rows {
		plainRows = append(plainRows, rows[i].Task)
	}
	if err := validateRAGOrderedTasks(plainRows, afterID, high, next); err != nil {
		return fairqueue.RecoveryPage[fairqueue.RunningLease]{}, err
	}
	items := make([]fairqueue.RunningLease, 0, len(rows))
	for _, row := range rows {
		task := row.Task
		if task.ID <= 0 || task.Status != "RUNNING" || task.ClaimGeneration <= 0 || task.ClaimGeneration > math.MaxInt64 ||
			task.DispatchGeneration != task.ClaimGeneration || task.DocID == "" || task.DocVersion <= 0 ||
			task.LeaseOwner == "" || task.LeaseOwner != strings.TrimSpace(task.LeaseOwner) || task.HeartbeatAt == nil ||
			task.LeaseUntil == nil || task.NextRunAt != nil || !task.LeaseUntil.After(row.ObservedDBNow) ||
			fairqueue.ValidateTenantID(task.UserID) != nil {
			return fairqueue.RecoveryPage[fairqueue.RunningLease]{}, authoritativeStateError("invalid running recovery row")
		}
		lease := fairqueue.RunningLease{
			TenantID: task.UserID, TaskID: formatRAGFairQueueID(task.ID), ClaimGeneration: uint64(task.ClaimGeneration),
			LeaseUntil: *task.LeaseUntil, ObservedDBNow: row.ObservedDBNow,
		}
		if err := lease.Validate(); err != nil {
			return fairqueue.RecoveryPage[fairqueue.RunningLease]{}, authoritativeStateError("invalid running lease snapshot")
		}
		items = append(items, lease)
	}
	return finishRAGIDRecoveryPage(items, high, after, next, limit)
}

func parseRAGRecoveryWindow(highWater, after string) (int64, int64, error) {
	high, err := parseRAGHighWater(highWater)
	if err != nil {
		return 0, 0, err
	}
	afterID, err := parseRAGFairQueueID(after, true)
	if err != nil {
		return 0, 0, err
	}
	if afterID > high {
		return 0, 0, fmt.Errorf("%w: cursor exceeds high water", fairqueue.ErrInvalidModel)
	}
	return high, afterID, nil
}

func validateRAGOrderedTasks(rows []store.RAGIndexTaskRecord, after, high, next int64) error {
	previous := after
	for i := range rows {
		if rows[i].ID <= previous || rows[i].ID > high {
			return authoritativeStateError("canonical recovery page is unordered or escaped its high water")
		}
		previous = rows[i].ID
	}
	if next < previous || next > high {
		return authoritativeStateError("canonical recovery cursor precedes its records")
	}
	return nil
}

func finishRAGIDRecoveryPage[T any](items []T, high int64, after string, next int64, limit int) (fairqueue.RecoveryPage[T], error) {
	if next < 0 || next > high {
		return fairqueue.RecoveryPage[T]{}, authoritativeStateError("recovery cursor escaped high water")
	}
	done := next >= high
	page := fairqueue.RecoveryPage[T]{Items: items, Done: done}
	if !done {
		page.NextCursor = formatRAGFairQueueID(next)
	}
	if !done && page.NextCursor == after {
		return fairqueue.RecoveryPage[T]{}, authoritativeStateError("recovery page did not advance")
	}
	if err := page.Validate(after, limit); err != nil {
		return fairqueue.RecoveryPage[T]{}, authoritativeStateError("invalid recovery page")
	}
	return page, nil
}

func (a *RAGFairQueueAdapter) CaptureRepairHighWater(ctx context.Context) (string, error) {
	high, err := a.source.CaptureRAGBrokerRepairHighWater(ctx)
	if err != nil {
		return "", mapRAGFairQueueStoreError(err)
	}
	if high < 0 {
		return "", authoritativeStateError("negative broker repair high water")
	}
	return formatRAGFairQueueID(high), nil
}

func (a *RAGFairQueueAdapter) ListBrokerBackedCandidates(ctx context.Context, highWater, after string, limit int) (fairqueue.RecoveryPage[fairqueue.DispatchCandidate], error) {
	high, afterID, err := parseRAGRecoveryWindow(highWater, after)
	if err != nil {
		return fairqueue.RecoveryPage[fairqueue.DispatchCandidate]{}, err
	}
	rows, next, err := a.source.ListBrokerBackedRAGCandidatesPage(ctx, high, afterID, limit)
	if err != nil {
		return fairqueue.RecoveryPage[fairqueue.DispatchCandidate]{}, mapRAGFairQueueStoreError(err)
	}
	if err := validateRAGOrderedCandidates(rows, afterID, high, true, next); err != nil {
		return fairqueue.RecoveryPage[fairqueue.DispatchCandidate]{}, err
	}
	for i := range rows {
		if rows[i].Guard.DispatchedAt == nil || rows[i].Guard.DispatchedAtRaw.IsNull {
			return fairqueue.RecoveryPage[fairqueue.DispatchCandidate]{}, authoritativeStateError("broker-backed candidate lacks a publish marker")
		}
	}
	items, err := convertRAGCandidates(rows)
	if err != nil {
		return fairqueue.RecoveryPage[fairqueue.DispatchCandidate]{}, err
	}
	return finishRAGIDRecoveryPage(items, high, after, next, limit)
}

func (a *RAGFairQueueAdapter) RearmAfterBrokerLoss(ctx context.Context, candidate fairqueue.DispatchCandidate) (fairqueue.DispatchCandidate, bool, error) {
	decoded, err := decodeRAGDispatchCandidate(candidate)
	if err != nil {
		return fairqueue.DispatchCandidate{}, false, err
	}
	updated, changed, err := a.source.RearmRAGCandidateAfterBrokerLoss(ctx, decoded)
	if err != nil {
		return fairqueue.DispatchCandidate{}, false, mapRAGFairQueueStoreError(err)
	}
	if !changed {
		if updated != nil {
			return fairqueue.DispatchCandidate{}, false, authoritativeStateError("stale broker repair returned a candidate")
		}
		return fairqueue.DispatchCandidate{}, false, nil
	}
	if updated == nil {
		return fairqueue.DispatchCandidate{}, false, authoritativeStateError("broker repair changed without a candidate")
	}
	if err := validateRAGRearmedCandidate(decoded, *updated); err != nil {
		return fairqueue.DispatchCandidate{}, false, err
	}
	converted, err := encodeRAGDispatchCandidate(*updated)
	if err != nil {
		return fairqueue.DispatchCandidate{}, false, err
	}
	return converted, true, nil
}

func (a *RAGFairQueueAdapter) ReadWriterIdentity(ctx context.Context) (fairqueue.WriterIdentity, error) {
	identity, err := a.admin.ReadWriterIdentity(ctx)
	if err != nil {
		return fairqueue.WriterIdentity{}, mapRAGFairQueueStoreError(err)
	}
	converted := fairqueue.WriterIdentity{Fingerprint: identity.Fingerprint}
	if err := converted.Validate(); err != nil {
		return fairqueue.WriterIdentity{}, authoritativeStateError("admin returned an invalid writer identity")
	}
	if converted.Fingerprint != a.writer {
		return fairqueue.WriterIdentity{}, errors.Join(fairqueue.ErrAuthoritativeWriterMismatch, errors.New("rag: fresh writer differs from bound writer"))
	}
	return converted, nil
}

func (a *RAGFairQueueAdapter) CheckSchemaAndInvariants(ctx context.Context) (fairqueue.WriterReadinessReport, error) {
	identity, err := a.ReadWriterIdentity(ctx)
	if err != nil {
		return fairqueue.WriterReadinessReport{}, err
	}
	report, err := a.admin.CheckSchemaAndInvariants(ctx)
	if err != nil {
		return fairqueue.WriterReadinessReport{}, mapRAGFairQueueStoreError(err)
	}
	if _, err := a.ReadWriterIdentity(ctx); err != nil {
		return fairqueue.WriterReadinessReport{}, err
	}
	if err := validateRAGContractReport(report); err != nil {
		return fairqueue.WriterReadinessReport{}, err
	}
	counts := []int64{
		report.MissingUserIDCount, report.UnresolvedOwnerCount, report.OwnerMismatchCount,
		report.NonPositiveGenerationCount, report.ExhaustedGenerationCount,
		report.PendingGenerationMismatchCount, report.RunningGenerationMismatchCount,
		report.PendingDispatchMarkerCount,
	}
	for _, count := range counts {
		if count < 0 {
			return fairqueue.WriterReadinessReport{}, authoritativeStateError("negative contract aggregate")
		}
	}
	owner, ok := safeRAGCountSum(report.MissingUserIDCount, report.UnresolvedOwnerCount, report.OwnerMismatchCount)
	if !ok {
		return fairqueue.WriterReadinessReport{}, authoritativeStateError("owner aggregate overflow")
	}
	// A PENDING row keeps dispatched_at after its Rabbit publish is confirmed
	// and before it is claimed. That marker is live delivery state, not a
	// generation invariant violation; the migration report retains the count
	// without using it to close writer readiness.
	generation, ok := safeRAGCountSum(report.NonPositiveGenerationCount, report.ExhaustedGenerationCount,
		report.PendingGenerationMismatchCount, report.RunningGenerationMismatchCount)
	if !ok {
		return fairqueue.WriterReadinessReport{}, authoritativeStateError("generation aggregate overflow")
	}
	converted := fairqueue.WriterReadinessReport{
		Writer: identity, SchemaReady: report.ExpandSchemaReady && !report.UserIDNullable,
		OwnerInvariantViolationCount: owner, GenerationViolationCount: generation,
	}
	if err := converted.Validate(); err != nil {
		return fairqueue.WriterReadinessReport{}, authoritativeStateError("invalid readiness report")
	}
	return converted, nil
}

func validateRAGContractReport(report store.RAGFairQueueContractReport) error {
	counts := []int64{
		report.TaskCount, report.MissingUserIDCount, report.UnresolvedOwnerCount,
		report.OwnerMismatchCount, report.NonPositiveGenerationCount,
		report.ExhaustedGenerationCount, report.PendingGenerationMismatchCount,
		report.RunningGenerationMismatchCount, report.PendingDispatchMarkerCount,
		report.RunningDispatchMarkerCount, report.RemainingCount,
		report.PagesScanned, report.RowsChanged,
	}
	for _, count := range counts {
		if count < 0 {
			return authoritativeStateError("negative contract aggregate")
		}
	}
	for _, count := range counts[1:11] {
		if count > report.TaskCount {
			return authoritativeStateError("contract violation aggregate exceeds task count")
		}
	}
	if report.RowsChanged > report.TaskCount ||
		report.Contracted != (report.ExpandSchemaReady && !report.UserIDNullable && report.RemainingCount == 0) {
		return authoritativeStateError("contract completion aggregates are inconsistent")
	}
	return nil
}

func safeRAGCountSum(values ...int64) (int64, bool) {
	var sum int64
	for _, value := range values {
		if value < 0 || sum > math.MaxInt64-value {
			return 0, false
		}
		sum += value
	}
	return sum, true
}

func (a *RAGFairQueueAdapter) CountValidRunning(ctx context.Context) (int64, error) {
	count, err := a.admin.CountValidRunning(ctx)
	if err != nil {
		return 0, mapRAGFairQueueStoreError(err)
	}
	if count < 0 {
		return 0, authoritativeStateError("negative valid-running count")
	}
	return count, nil
}

type RAGFairQueueOperationJournal struct{ backend RAGFairQueueJournalStore }

func NewRAGFairQueueOperationJournal(backend RAGFairQueueJournalStore) (*RAGFairQueueOperationJournal, error) {
	if nilInterface(backend) {
		return nil, errors.New("rag: fair queue operation journal is required")
	}
	return &RAGFairQueueOperationJournal{backend: backend}, nil
}

func (j *RAGFairQueueOperationJournal) Read(ctx context.Context, resource, expectedWriter string) (fairqueue.RecoveryOperationRecord, bool, error) {
	if err := validateRAGJournalBoundary(resource, expectedWriter); err != nil {
		return fairqueue.RecoveryOperationRecord{}, false, err
	}
	record, found, err := j.backend.ReadFairQueueOperation(ctx, resource, expectedWriter)
	if err != nil {
		return fairqueue.RecoveryOperationRecord{}, false, mapRAGFairQueuePersistedJournalError(err)
	}
	if !found {
		return fairqueue.RecoveryOperationRecord{}, false, nil
	}
	converted, err := fairOperationFromStore(record)
	if err != nil {
		return fairqueue.RecoveryOperationRecord{}, false, mapRAGFairQueuePersistedJournalError(err)
	}
	if err == nil && (converted.Resource != resource || converted.CurrentWriterFingerprint != expectedWriter) {
		return fairqueue.RecoveryOperationRecord{}, false, authoritativeStateError("journal read escaped its resource or writer")
	}
	return converted, err == nil, err
}

type ragFairQueueOperationStartSession struct {
	backend  RAGFairQueueJournalStartSession
	resource string
	writer   string
}

func (s ragFairQueueOperationStartSession) Read(ctx context.Context) (fairqueue.RecoveryOperationRecord, bool, error) {
	record, found, err := s.backend.Read(ctx)
	if err != nil {
		return fairqueue.RecoveryOperationRecord{}, false, mapRAGFairQueuePersistedJournalError(err)
	}
	if !found {
		return fairqueue.RecoveryOperationRecord{}, false, nil
	}
	converted, err := fairOperationFromStore(record)
	if err != nil {
		return fairqueue.RecoveryOperationRecord{}, false, mapRAGFairQueuePersistedJournalError(err)
	}
	if err == nil && (converted.Resource != s.resource || converted.CurrentWriterFingerprint != s.writer) {
		return fairqueue.RecoveryOperationRecord{}, false, authoritativeStateError("journal start read escaped its fence")
	}
	return converted, err == nil, err
}
func (s ragFairQueueOperationStartSession) BeginSpecial(ctx context.Context, expected *fairqueue.RecoveryOperationRecord, proposal fairqueue.RecoveryOperationRecord) (fairqueue.RecoveryOperationRecord, error) {
	if proposal.Resource != s.resource || proposal.CurrentWriterFingerprint != s.writer {
		return fairqueue.RecoveryOperationRecord{}, fairqueue.ErrInvalidOperationRecord
	}
	var storeExpected *store.FairQueueOperationRecord
	if expected != nil {
		converted, err := storeOperationFromFair(*expected, false)
		if err != nil {
			return fairqueue.RecoveryOperationRecord{}, err
		}
		storeExpected = &converted
	}
	storeProposal, err := storeProposalFromFair(proposal)
	if err != nil {
		return fairqueue.RecoveryOperationRecord{}, err
	}
	record, err := s.backend.BeginSpecial(ctx, storeExpected, storeProposal)
	if err != nil {
		return fairqueue.RecoveryOperationRecord{}, mapRAGFairQueuePersistedJournalError(err)
	}
	converted, err := fairOperationFromStore(record)
	if err != nil {
		return fairqueue.RecoveryOperationRecord{}, mapRAGFairQueuePersistedJournalError(err)
	}
	if !sameRAGOperationIdentity(converted, proposal) || converted.Phase != fairqueue.OperationActive {
		return fairqueue.RecoveryOperationRecord{}, authoritativeStateError("journal begin returned a different operation")
	}
	return converted, nil
}

func (j *RAGFairQueueOperationJournal) WithStartFence(ctx context.Context, resource, expectedWriter string, fn func(fairqueue.OperationStartSession) error) error {
	if fn == nil {
		return fairqueue.ErrInvalidOperationRecord
	}
	if err := validateRAGJournalBoundary(resource, expectedWriter); err != nil {
		return err
	}
	err := j.backend.WithFairQueueOperationStartFence(ctx, resource, expectedWriter, func(session RAGFairQueueJournalStartSession) error {
		if nilInterface(session) {
			return authoritativeStateError("journal start fence returned a nil session")
		}
		return fn(ragFairQueueOperationStartSession{backend: session, resource: resource, writer: expectedWriter})
	})
	return mapRAGFairQueueStoreError(err)
}

func (j *RAGFairQueueOperationJournal) SetRepairHighWater(ctx context.Context, expected fairqueue.RecoveryOperationRecord, highWater string) (fairqueue.RecoveryOperationRecord, error) {
	return j.mutate(ctx, expected, func(record store.FairQueueOperationRecord) (store.FairQueueOperationRecord, error) {
		return j.backend.SetFairQueueOperationRepairHighWater(ctx, record, highWater)
	})
}
func (j *RAGFairQueueOperationJournal) MarkRepairPassComplete(ctx context.Context, expected fairqueue.RecoveryOperationRecord) (fairqueue.RecoveryOperationRecord, error) {
	return j.mutate(ctx, expected, func(record store.FairQueueOperationRecord) (store.FairQueueOperationRecord, error) {
		return j.backend.MarkFairQueueOperationRepairPassComplete(ctx, record)
	})
}
func (j *RAGFairQueueOperationJournal) MarkForceDeletePassComplete(ctx context.Context, expected fairqueue.RecoveryOperationRecord) (fairqueue.RecoveryOperationRecord, error) {
	return j.mutate(ctx, expected, func(record store.FairQueueOperationRecord) (store.FairQueueOperationRecord, error) {
		return j.backend.MarkFairQueueOperationForceDeletePassComplete(ctx, record)
	})
}
func (j *RAGFairQueueOperationJournal) CommitReady(ctx context.Context, expected fairqueue.RecoveryOperationRecord) (fairqueue.RecoveryOperationRecord, error) {
	return j.mutate(ctx, expected, func(record store.FairQueueOperationRecord) (store.FairQueueOperationRecord, error) {
		return j.backend.CommitFairQueueOperationReady(ctx, record)
	})
}
func (j *RAGFairQueueOperationJournal) Complete(ctx context.Context, expected fairqueue.RecoveryOperationRecord) (fairqueue.RecoveryOperationRecord, error) {
	return j.mutate(ctx, expected, func(record store.FairQueueOperationRecord) (store.FairQueueOperationRecord, error) {
		return j.backend.CompleteFairQueueOperation(ctx, record)
	})
}
func (j *RAGFairQueueOperationJournal) mutate(_ context.Context, expected fairqueue.RecoveryOperationRecord, fn func(store.FairQueueOperationRecord) (store.FairQueueOperationRecord, error)) (fairqueue.RecoveryOperationRecord, error) {
	converted, err := storeOperationFromFair(expected, false)
	if err != nil {
		return fairqueue.RecoveryOperationRecord{}, err
	}
	result, err := fn(converted)
	if err != nil {
		return fairqueue.RecoveryOperationRecord{}, mapRAGFairQueuePersistedJournalError(err)
	}
	convertedResult, err := fairOperationFromStore(result)
	if err != nil {
		return fairqueue.RecoveryOperationRecord{}, mapRAGFairQueuePersistedJournalError(err)
	}
	if !sameRAGOperationIdentity(convertedResult, expected) || convertedResult.Version < expected.Version {
		return fairqueue.RecoveryOperationRecord{}, authoritativeStateError("journal mutation returned a different operation")
	}
	return convertedResult, nil
}

func validateRAGJournalBoundary(resource, writer string) error {
	if err := fairqueue.ValidateResource(resource); err != nil {
		return errors.Join(fairqueue.ErrInvalidOperationRecord, err)
	}
	if err := (fairqueue.WriterIdentity{Fingerprint: writer}).Validate(); err != nil {
		return errors.Join(fairqueue.ErrAuthoritativeWriterMismatch, err)
	}
	return nil
}

func sameRAGOperationIdentity(left, right fairqueue.RecoveryOperationRecord) bool {
	return left.Resource == right.Resource && left.OperationID == right.OperationID && left.Kind == right.Kind &&
		left.CurrentWriterFingerprint == right.CurrentWriterFingerprint &&
		left.OriginalWriterFingerprint == right.OriginalWriterFingerprint &&
		left.TargetWriterFingerprint == right.TargetWriterFingerprint && sameRAGTime(left.ForceNotBefore, right.ForceNotBefore)
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}
func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func fairOperationFromStore(record store.FairQueueOperationRecord) (fairqueue.RecoveryOperationRecord, error) {
	kind, err := fairKindFromStore(record.Kind)
	if err != nil {
		return fairqueue.RecoveryOperationRecord{}, err
	}
	phase, err := fairPhaseFromStore(record.Phase)
	if err != nil {
		return fairqueue.RecoveryOperationRecord{}, err
	}
	converted := fairqueue.RecoveryOperationRecord{
		Resource: record.Resource, OperationID: record.OperationID, Kind: kind, Phase: phase,
		CurrentWriterFingerprint:  record.CurrentWriterFingerprint,
		OriginalWriterFingerprint: record.OriginalWriterFingerprint,
		TargetWriterFingerprint:   record.TargetWriterFingerprint,
		RepairHighWater:           cloneString(record.RepairHighWater), RepairPassComplete: record.RepairPassComplete,
		ForceNotBefore: cloneTime(record.ForceNotBefore), ForceDeletePassComplete: record.ForceDeletePassComplete,
		Version: record.Version, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
	}
	if err := converted.ValidatePersisted(); err != nil {
		return fairqueue.RecoveryOperationRecord{}, errors.Join(fairqueue.ErrInvalidOperationRecord, err)
	}
	return converted, nil
}

func storeOperationFromFair(record fairqueue.RecoveryOperationRecord, proposal bool) (store.FairQueueOperationRecord, error) {
	if proposal {
		if err := record.ValidateProposal(); err != nil {
			return store.FairQueueOperationRecord{}, err
		}
	} else if err := record.ValidatePersisted(); err != nil {
		return store.FairQueueOperationRecord{}, err
	}
	kind, err := storeKindFromFair(record.Kind)
	if err != nil {
		return store.FairQueueOperationRecord{}, err
	}
	phase := store.FairQueueOperationPhase("")
	if !proposal {
		phase, err = storePhaseFromFair(record.Phase)
		if err != nil {
			return store.FairQueueOperationRecord{}, err
		}
	}
	return store.FairQueueOperationRecord{
		Resource: record.Resource, OperationID: record.OperationID, Kind: kind, Phase: phase,
		CurrentWriterFingerprint:  record.CurrentWriterFingerprint,
		OriginalWriterFingerprint: record.OriginalWriterFingerprint,
		TargetWriterFingerprint:   record.TargetWriterFingerprint,
		RepairHighWater:           cloneString(record.RepairHighWater), RepairPassComplete: record.RepairPassComplete,
		ForceNotBefore: cloneTime(record.ForceNotBefore), ForceDeletePassComplete: record.ForceDeletePassComplete,
		Version: record.Version, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
	}, nil
}

func storeProposalFromFair(record fairqueue.RecoveryOperationRecord) (store.FairQueueOperationProposal, error) {
	converted, err := storeOperationFromFair(record, true)
	if err != nil {
		return store.FairQueueOperationProposal{}, err
	}
	return store.FairQueueOperationProposal{
		Resource: converted.Resource, OperationID: converted.OperationID, Kind: converted.Kind,
		CurrentWriterFingerprint:  converted.CurrentWriterFingerprint,
		OriginalWriterFingerprint: converted.OriginalWriterFingerprint,
		TargetWriterFingerprint:   converted.TargetWriterFingerprint,
		ForceNotBefore:            cloneTime(converted.ForceNotBefore),
	}, nil
}

func fairKindFromStore(kind store.FairQueueOperationKind) (fairqueue.RecoveryKind, error) {
	switch kind {
	case store.FairQueueOperationRabbitRepair:
		return fairqueue.RecoveryRabbitRepair, nil
	case store.FairQueueOperationWriterRebind:
		return fairqueue.RecoveryWriterRebind, nil
	case store.FairQueueOperationForceRebuild:
		return fairqueue.RecoveryForceRebuild, nil
	default:
		return "", fairqueue.ErrInvalidOperationRecord
	}
}
func storeKindFromFair(kind fairqueue.RecoveryKind) (store.FairQueueOperationKind, error) {
	switch kind {
	case fairqueue.RecoveryRabbitRepair:
		return store.FairQueueOperationRabbitRepair, nil
	case fairqueue.RecoveryWriterRebind:
		return store.FairQueueOperationWriterRebind, nil
	case fairqueue.RecoveryForceRebuild:
		return store.FairQueueOperationForceRebuild, nil
	default:
		return "", fairqueue.ErrInvalidOperationRecord
	}
}
func fairPhaseFromStore(phase store.FairQueueOperationPhase) (fairqueue.OperationPhase, error) {
	switch phase {
	case store.FairQueueOperationActive:
		return fairqueue.OperationActive, nil
	case store.FairQueueOperationReadyCommitted:
		return fairqueue.OperationReadyCommitted, nil
	case store.FairQueueOperationCompleted:
		return fairqueue.OperationCompleted, nil
	default:
		return "", fairqueue.ErrInvalidOperationRecord
	}
}
func storePhaseFromFair(phase fairqueue.OperationPhase) (store.FairQueueOperationPhase, error) {
	switch phase {
	case fairqueue.OperationActive:
		return store.FairQueueOperationActive, nil
	case fairqueue.OperationReadyCommitted:
		return store.FairQueueOperationReadyCommitted, nil
	case fairqueue.OperationCompleted:
		return store.FairQueueOperationCompleted, nil
	default:
		return "", fairqueue.ErrInvalidOperationRecord
	}
}

// RAGFairQueueDispatchTarget is implemented by fairqueue.Runtime. The bridge
// keeps Service notification independent of that concrete type.
type RAGFairQueueDispatchTarget interface {
	TryDispatch(context.Context, string, string) (bool, error)
}

type RAGFairQueueNotifier struct{ target RAGFairQueueDispatchTarget }

func NewRAGFairQueueNotifier(target RAGFairQueueDispatchTarget) (*RAGFairQueueNotifier, error) {
	if nilInterface(target) {
		return nil, errors.New("rag: fair queue dispatch target is required")
	}
	return &RAGFairQueueNotifier{target: target}, nil
}
func (n *RAGFairQueueNotifier) TryDispatch(ctx context.Context, taskID int64) error {
	if n == nil || taskID <= 0 {
		return errors.New("rag: invalid fair queue dispatch notification")
	}
	_, err := n.target.TryDispatch(ctx, RAGFairQueueResource, formatRAGFairQueueID(taskID))
	return err
}

var (
	_ fairqueue.DispatchSource     = (*RAGFairQueueAdapter)(nil)
	_ fairqueue.ExpiredRearmSource = (*RAGFairQueueAdapter)(nil)
	_ fairqueue.TaskPreparer       = (*RAGFairQueueAdapter)(nil)
	_ fairqueue.RecoverySource     = (*RAGFairQueueAdapter)(nil)
	_ fairqueue.BrokerRepairSource = (*RAGFairQueueAdapter)(nil)
	_ fairqueue.WriterRebindSource = (*RAGFairQueueAdapter)(nil)
	_ fairqueue.OperationJournal   = (*RAGFairQueueOperationJournal)(nil)
)
