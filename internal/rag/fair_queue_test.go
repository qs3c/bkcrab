package rag

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/fairqueue"
	"github.com/qs3c/bkcrab/internal/store"
)

const (
	testRAGWriter = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testRAGUser   = "user-1"
)

type fakeRAGFairQueueSource struct {
	list            func(context.Context, int64, int) ([]store.RAGIndexTaskDispatchCandidate, int64, error)
	get             func(context.Context, int64) (*store.RAGIndexTaskDispatchCandidate, error)
	mark            func(context.Context, store.RAGIndexTaskDispatchCandidate) (bool, error)
	rearmExpired    func(context.Context, int64, int) ([]store.RAGIndexTaskDispatchCandidate, int64, error)
	highWater       func(context.Context) (int64, error)
	tenants         func(context.Context, int64, string, int) ([]string, string, error)
	dispatched      func(context.Context, int64, int64, int) ([]store.RAGIndexTaskRecord, int64, error)
	running         func(context.Context, int64, int64, int) ([]store.RAGIndexTaskRunningSnapshot, int64, error)
	repairHighWater func(context.Context) (int64, error)
	broker          func(context.Context, int64, int64, int) ([]store.RAGIndexTaskDispatchCandidate, int64, error)
	rearmBroker     func(context.Context, store.RAGIndexTaskDispatchCandidate) (*store.RAGIndexTaskDispatchCandidate, bool, error)
	claim           func(context.Context, int64, string, int64, string, time.Duration, store.RAGFairQueueClaimLimits) (store.RAGFairQueueClaimResult, error)
	poison          func(context.Context, int64, int64) (*store.RAGIndexTaskDispatchCandidate, error)
	rearmPoison     func(context.Context, store.RAGIndexTaskDispatchCandidate) (*store.RAGIndexTaskDispatchCandidate, bool, error)
	readTask        func(context.Context, int64) (*store.RAGIndexTaskRecord, error)
	poisonLookups   []int64
	poisonRepairs   []int64
	poisonEvents    []string
	claimCalls      int
	readTaskCalls   int
}

func (f *fakeRAGFairQueueSource) ExpectedWriterFingerprint() string { return testRAGWriter }
func (f *fakeRAGFairQueueSource) ListDispatchableRAGIndexTasksPage(ctx context.Context, after int64, limit int) ([]store.RAGIndexTaskDispatchCandidate, int64, error) {
	if f.list == nil {
		return nil, after, nil
	}
	return f.list(ctx, after, limit)
}
func (f *fakeRAGFairQueueSource) GetDispatchableRAGIndexTaskByID(ctx context.Context, id int64) (*store.RAGIndexTaskDispatchCandidate, error) {
	if f.get == nil {
		return nil, nil
	}
	return f.get(ctx, id)
}
func (f *fakeRAGFairQueueSource) MarkRAGIndexTaskDispatched(ctx context.Context, candidate store.RAGIndexTaskDispatchCandidate) (bool, error) {
	if f.mark == nil {
		return false, nil
	}
	return f.mark(ctx, candidate)
}
func (f *fakeRAGFairQueueSource) ArmExpiredRAGIndexTasksPage(ctx context.Context, after int64, limit int) ([]store.RAGIndexTaskDispatchCandidate, int64, error) {
	if f.rearmExpired == nil {
		return nil, after, nil
	}
	return f.rearmExpired(ctx, after, limit)
}
func (f *fakeRAGFairQueueSource) CaptureRAGFairQueueHighWater(ctx context.Context) (int64, error) {
	if f.highWater == nil {
		return 0, nil
	}
	return f.highWater(ctx)
}
func (f *fakeRAGFairQueueSource) ListCanonicalRAGTenantsPage(ctx context.Context, high int64, after string, limit int) ([]string, string, error) {
	if f.tenants == nil {
		return nil, after, nil
	}
	return f.tenants(ctx, high, after, limit)
}
func (f *fakeRAGFairQueueSource) ListDispatchedRAGIndexTasksPage(ctx context.Context, high, after int64, limit int) ([]store.RAGIndexTaskRecord, int64, error) {
	if f.dispatched == nil {
		return nil, after, nil
	}
	return f.dispatched(ctx, high, after, limit)
}
func (f *fakeRAGFairQueueSource) ListValidRunningRAGIndexTasksPage(ctx context.Context, high, after int64, limit int) ([]store.RAGIndexTaskRunningSnapshot, int64, error) {
	if f.running == nil {
		return nil, after, nil
	}
	return f.running(ctx, high, after, limit)
}
func (f *fakeRAGFairQueueSource) CaptureRAGBrokerRepairHighWater(ctx context.Context) (int64, error) {
	if f.repairHighWater == nil {
		return 0, nil
	}
	return f.repairHighWater(ctx)
}
func (f *fakeRAGFairQueueSource) ListBrokerBackedRAGCandidatesPage(ctx context.Context, high, after int64, limit int) ([]store.RAGIndexTaskDispatchCandidate, int64, error) {
	if f.broker == nil {
		return nil, after, nil
	}
	return f.broker(ctx, high, after, limit)
}
func (f *fakeRAGFairQueueSource) RearmRAGCandidateAfterBrokerLoss(ctx context.Context, candidate store.RAGIndexTaskDispatchCandidate) (*store.RAGIndexTaskDispatchCandidate, bool, error) {
	if f.rearmBroker == nil {
		return nil, false, nil
	}
	return f.rearmBroker(ctx, candidate)
}
func (f *fakeRAGFairQueueSource) ClaimRAGIndexTaskByID(ctx context.Context, id int64, user string, generation int64, worker string, lease time.Duration, limits store.RAGFairQueueClaimLimits) (store.RAGFairQueueClaimResult, error) {
	f.claimCalls++
	if f.claim == nil {
		return store.RAGFairQueueClaimResult{Disposition: store.RAGFairQueueClaimDuplicateStale}, nil
	}
	return f.claim(ctx, id, user, generation, worker, lease, limits)
}
func (f *fakeRAGFairQueueSource) GetRAGPoisonRepairCandidate(ctx context.Context, id, generation int64) (*store.RAGIndexTaskDispatchCandidate, error) {
	f.poisonLookups = append(f.poisonLookups, id)
	f.poisonEvents = append(f.poisonEvents, "get:"+formatRAGFairQueueID(id))
	if f.poison == nil {
		return nil, store.ErrNotFound
	}
	return f.poison(ctx, id, generation)
}
func (f *fakeRAGFairQueueSource) RearmRAGPoisonCandidate(ctx context.Context, candidate store.RAGIndexTaskDispatchCandidate) (*store.RAGIndexTaskDispatchCandidate, bool, error) {
	f.poisonRepairs = append(f.poisonRepairs, candidate.Task.ID)
	f.poisonEvents = append(f.poisonEvents, "repair:"+formatRAGFairQueueID(candidate.Task.ID))
	if f.rearmPoison == nil {
		candidate.Task.DispatchGeneration++
		candidate.Guard.DispatchGeneration++
		return &candidate, true, nil
	}
	return f.rearmPoison(ctx, candidate)
}
func (f *fakeRAGFairQueueSource) GetRAGIndexTask(ctx context.Context, id int64) (*store.RAGIndexTaskRecord, error) {
	f.readTaskCalls++
	if f.readTask == nil {
		return nil, store.ErrNotFound
	}
	return f.readTask(ctx, id)
}

type fakeRAGFairQueueRunner struct {
	claim *store.RAGIndexClaim
	err   error
}

func (f *fakeRAGFairQueueRunner) RunFairClaim(_ context.Context, claim *store.RAGIndexClaim) error {
	f.claim = claim
	return f.err
}

type fakeRAGFairQueueAdmin struct {
	identity   store.FairQueueWriterIdentity
	report     store.RAGFairQueueContractReport
	count      int64
	err        error
	highWater  func(context.Context) (int64, error)
	tenants    func(context.Context, int64, string, int) ([]string, string, error)
	dispatched func(context.Context, int64, int64, int) ([]store.RAGIndexTaskRecord, int64, error)
	running    func(context.Context, int64, int64, int) ([]store.RAGIndexTaskRunningSnapshot, int64, error)
}

func (f *fakeRAGFairQueueAdmin) ReadWriterIdentity(context.Context) (store.FairQueueWriterIdentity, error) {
	return f.identity, f.err
}
func (f *fakeRAGFairQueueAdmin) CheckSchemaAndInvariants(context.Context) (store.RAGFairQueueContractReport, error) {
	return f.report, f.err
}
func (f *fakeRAGFairQueueAdmin) CountValidRunning(context.Context) (int64, error) {
	return f.count, f.err
}
func (f *fakeRAGFairQueueAdmin) CaptureRAGFairQueueHighWater(ctx context.Context) (int64, error) {
	if f.highWater == nil {
		return 0, nil
	}
	return f.highWater(ctx)
}
func (f *fakeRAGFairQueueAdmin) ListCanonicalRAGTenantsPage(ctx context.Context, high int64, after string, limit int) ([]string, string, error) {
	if f.tenants == nil {
		return nil, after, nil
	}
	return f.tenants(ctx, high, after, limit)
}
func (f *fakeRAGFairQueueAdmin) ListDispatchedRAGIndexTasksPage(ctx context.Context, high, after int64, limit int) ([]store.RAGIndexTaskRecord, int64, error) {
	if f.dispatched == nil {
		return nil, after, nil
	}
	return f.dispatched(ctx, high, after, limit)
}
func (f *fakeRAGFairQueueAdmin) ListValidRunningRAGIndexTasksPage(ctx context.Context, high, after int64, limit int) ([]store.RAGIndexTaskRunningSnapshot, int64, error) {
	if f.running == nil {
		return nil, after, nil
	}
	return f.running(ctx, high, after, limit)
}

type fakeRAGFairQueueJournalSession struct {
	record   store.FairQueueOperationRecord
	found    bool
	proposal store.FairQueueOperationProposal
}

func (f *fakeRAGFairQueueJournalSession) Read(context.Context) (store.FairQueueOperationRecord, bool, error) {
	return f.record, f.found, nil
}
func (f *fakeRAGFairQueueJournalSession) BeginSpecial(_ context.Context, _ *store.FairQueueOperationRecord, proposal store.FairQueueOperationProposal) (store.FairQueueOperationRecord, error) {
	f.proposal = proposal
	now := time.Now().UTC().Truncate(time.Millisecond)
	return store.FairQueueOperationRecord{
		Resource: proposal.Resource, OperationID: proposal.OperationID, Kind: proposal.Kind,
		Phase: store.FairQueueOperationActive, CurrentWriterFingerprint: proposal.CurrentWriterFingerprint,
		OriginalWriterFingerprint: proposal.OriginalWriterFingerprint,
		TargetWriterFingerprint:   proposal.TargetWriterFingerprint, ForceNotBefore: proposal.ForceNotBefore,
		Version: 1, CreatedAt: now, UpdatedAt: now,
	}, nil
}

type fakeRAGFairQueueJournal struct {
	record  store.FairQueueOperationRecord
	found   bool
	session *fakeRAGFairQueueJournalSession
}

func (f *fakeRAGFairQueueJournal) ReadFairQueueOperation(context.Context, string, string) (store.FairQueueOperationRecord, bool, error) {
	return f.record, f.found, nil
}
func (f *fakeRAGFairQueueJournal) WithFairQueueOperationStartFence(ctx context.Context, _ string, _ string, fn func(RAGFairQueueJournalStartSession) error) error {
	return fn(f.session)
}
func (f *fakeRAGFairQueueJournal) SetFairQueueOperationRepairHighWater(context.Context, store.FairQueueOperationRecord, string) (store.FairQueueOperationRecord, error) {
	return f.record, nil
}
func (f *fakeRAGFairQueueJournal) MarkFairQueueOperationRepairPassComplete(context.Context, store.FairQueueOperationRecord) (store.FairQueueOperationRecord, error) {
	return f.record, nil
}
func (f *fakeRAGFairQueueJournal) MarkFairQueueOperationForceDeletePassComplete(context.Context, store.FairQueueOperationRecord) (store.FairQueueOperationRecord, error) {
	return f.record, nil
}
func (f *fakeRAGFairQueueJournal) CommitFairQueueOperationReady(context.Context, store.FairQueueOperationRecord) (store.FairQueueOperationRecord, error) {
	return f.record, nil
}
func (f *fakeRAGFairQueueJournal) CompleteFairQueueOperation(context.Context, store.FairQueueOperationRecord) (store.FairQueueOperationRecord, error) {
	return f.record, nil
}

func testRAGCandidate(id, generation int64, user string) store.RAGIndexTaskDispatchCandidate {
	task := store.RAGIndexTaskRecord{
		ID: id, DocID: "doc-1", DocVersion: 2, UserID: user, Status: "PENDING",
		DispatchGeneration: generation, ClaimGeneration: generation - 1,
	}
	return store.RAGIndexTaskDispatchCandidate{
		Task: task,
		Guard: store.RAGIndexTaskDispatchGuard{
			TaskID: id, DocID: task.DocID, DocVersion: task.DocVersion, UserID: user,
			Status: task.Status, DispatchGeneration: generation,
			ClaimGeneration: generation - 1, RetryCount: task.RetryCount,
			NextRunAtRaw:    store.RAGIndexTaskTimestampGuard{IsNull: true},
			LeaseUntilRaw:   store.RAGIndexTaskTimestampGuard{IsNull: true},
			DispatchedAtRaw: store.RAGIndexTaskTimestampGuard{IsNull: true},
		},
	}
}

func testRAGMessage(id, generation int64, user string) fairqueue.Message {
	taskID := formatRAGFairQueueID(id)
	return fairqueue.Message{
		Version: fairqueue.MessageVersion1, Resource: RAGFairQueueResource,
		TenantID: user, TaskType: RAGFairQueueTaskType, TaskID: taskID,
		DispatchToken: fairqueue.DispatchToken{Resource: RAGFairQueueResource, TaskID: taskID, Generation: uint64(generation)},
	}
}

func testRAGPrepareRequest(t *testing.T, message fairqueue.Message) fairqueue.PrepareRequest {
	t.Helper()
	raw, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	version, resource, taskID, generation := int32(1), message.Resource, message.TaskID, int64(message.DispatchToken.Generation)
	hash, err := fairqueue.TenantHash(RAGFairQueueResource, message.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	return fairqueue.PrepareRequest{
		Message: &message, BodyCandidate: &message, HeaderToken: &message.DispatchToken,
		HeaderFacts:        fairqueue.StableHeaderFacts{ProtocolVersion: &version, Resource: &resource, TaskID: &taskID, DispatchGeneration: &generation},
		RegisteredResource: RAGFairQueueResource, QueueTenantHash: hash,
		PublishAttemptID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", RawBody: raw,
	}
}

func testRAGPoisonRequest(t *testing.T, body *fairqueue.Message, header *fairqueue.DispatchToken, queueUser string) fairqueue.PrepareRequest {
	t.Helper()
	hash, err := fairqueue.TenantHash(RAGFairQueueResource, queueUser)
	if err != nil {
		t.Fatal(err)
	}
	request := fairqueue.PrepareRequest{
		RegisteredResource: RAGFairQueueResource,
		QueueTenantHash:    hash,
		PublishAttemptID:   "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
	if body == nil {
		request.RawBody = []byte("{")
		request.DecodeErrorCode = "invalid_json"
	} else {
		raw, err := json.Marshal(*body)
		if err != nil {
			t.Fatal(err)
		}
		request.BodyCandidate, request.RawBody = body, raw
	}
	if header == nil {
		request.HeaderErrorCode = "missing_headers"
	} else {
		version, resource, taskID, generation := int32(1), header.Resource, header.TaskID, int64(header.Generation)
		request.HeaderToken = header
		request.HeaderFacts = fairqueue.StableHeaderFacts{
			ProtocolVersion: &version, Resource: &resource, TaskID: &taskID,
			DispatchGeneration: &generation,
		}
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("poison request: %v", err)
	}
	return request
}

func newTestRAGAdapter(t *testing.T, source *fakeRAGFairQueueSource, runner *fakeRAGFairQueueRunner) *RAGFairQueueAdapter {
	t.Helper()
	journal := &fakeRAGFairQueueJournal{session: &fakeRAGFairQueueJournalSession{}}
	adapter, err := NewRAGFairQueueAdapter(source, runner, &fakeRAGFairQueueAdmin{
		identity: store.FairQueueWriterIdentity{Fingerprint: testRAGWriter},
		report:   store.RAGFairQueueContractReport{ExpandSchemaReady: true, Contracted: true},
	}, journal, RAGFairQueueAdapterOptions{
		WorkerID: "rag-fair-test", LeaseDuration: time.Minute,
		ClaimLimits:         store.RAGFairQueueClaimLimits{GlobalConcurrency: 4, PerUserBurstConcurrency: 2},
		ClaimRecheckTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func TestRAGFairQueueDispatchGuardRoundTrip(t *testing.T) {
	want := testRAGCandidate(42, 3, testRAGUser)
	var marked store.RAGIndexTaskDispatchCandidate
	source := &fakeRAGFairQueueSource{
		list: func(_ context.Context, after int64, limit int) ([]store.RAGIndexTaskDispatchCandidate, int64, error) {
			if after != 0 || limit != 10 {
				t.Fatalf("list args = %d,%d", after, limit)
			}
			return []store.RAGIndexTaskDispatchCandidate{want}, 42, nil
		},
		mark: func(_ context.Context, got store.RAGIndexTaskDispatchCandidate) (bool, error) {
			marked = got
			return true, nil
		},
	}
	adapter := newTestRAGAdapter(t, source, &fakeRAGFairQueueRunner{})
	page, next, err := adapter.ListDispatchCandidates(context.Background(), "", 10)
	if err != nil || next != "42" || len(page) != 1 {
		t.Fatalf("list = %#v,%q,%v", page, next, err)
	}
	if got := page[0].Message; got != testRAGMessage(42, 3, testRAGUser) {
		t.Fatalf("message = %#v", got)
	}
	changed, err := adapter.MarkDispatched(context.Background(), page[0])
	if err != nil || !changed || !reflect.DeepEqual(marked, want) {
		t.Fatalf("mark = %v,%v %#v", changed, err, marked)
	}

	bad := page[0]
	bad.Message.TenantID = "other"
	if _, err := adapter.MarkDispatched(context.Background(), bad); err == nil {
		t.Fatal("MarkDispatched accepted message/guard identity mismatch")
	}
	bad = page[0]
	bad.Guard = `{"task":{"id":42,"id":42}}`
	if _, err := adapter.MarkDispatched(context.Background(), bad); err == nil {
		t.Fatal("MarkDispatched accepted duplicate JSON fields")
	}
}

func TestRAGFairQueuePrepareDispositionAndRun(t *testing.T) {
	message := testRAGMessage(42, 3, testRAGUser)
	request := testRAGPrepareRequest(t, message)
	claimedTask := testRAGCandidate(42, 3, testRAGUser).Task
	claimedTask.Status, claimedTask.ClaimGeneration, claimedTask.LeaseOwner = "RUNNING", 3, "rag-fair-test"
	claimedAt := time.Now().UTC()
	claimedTask.DispatchedAt, claimedTask.LeaseUntil, claimedTask.HeartbeatAt = ptrTime(claimedAt), ptrTime(claimedAt.Add(time.Minute)), ptrTime(claimedAt)
	claim := &store.RAGIndexClaim{
		Task: claimedTask, Version: store.RAGDocumentVersionRecord{DocID: "doc-1", DocVersion: 2, Status: store.RAGDocumentVersionRunning},
		Fence: store.IndexFence{TaskID: 42, DocID: "doc-1", DocVersion: 2, ClaimGeneration: 3,
			LeaseOwner: "rag-fair-test", ExpectedWriterFingerprint: testRAGWriter},
	}
	for _, test := range []struct {
		name             string
		storeDisposition store.RAGFairQueueClaimDisposition
		claim            *store.RAGIndexClaim
		want             fairqueue.PrepareDisposition
	}{
		{"claimed", store.RAGFairQueueClaimed, claim, fairqueue.PrepareClaimed},
		{"capacity", store.RAGFairQueueClaimCapacityDeferred, nil, fairqueue.PrepareCapacityDeferred},
		{"duplicate", store.RAGFairQueueClaimDuplicateStale, nil, fairqueue.PrepareDuplicateStaleTerminal},
		{"terminal", store.RAGFairQueueClaimCanonicalTerminal, nil, fairqueue.PrepareCanonicalRepairedTerminal},
		{"retry", store.RAGFairQueueClaimCanonicalRetry, nil, fairqueue.PrepareCanonicalRepairedRetry},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := &fakeRAGFairQueueSource{claim: func(context.Context, int64, string, int64, string, time.Duration, store.RAGFairQueueClaimLimits) (store.RAGFairQueueClaimResult, error) {
				return store.RAGFairQueueClaimResult{Disposition: test.storeDisposition, Claim: test.claim}, nil
			}}
			runner := &fakeRAGFairQueueRunner{}
			adapter := newTestRAGAdapter(t, source, runner)
			prepared, result, err := adapter.Prepare(context.Background(), request)
			if err != nil || result.Disposition != test.want {
				t.Fatalf("Prepare = %#v,%v", result, err)
			}
			if err := result.ValidateFor(request, prepared); err != nil {
				t.Fatalf("invalid result: %v", err)
			}
			if test.want == fairqueue.PrepareClaimed {
				if err := prepared.Run(context.Background()); err != nil {
					t.Fatal(err)
				}
				if runner.claim != claim {
					t.Fatalf("runner claim = %#v", runner.claim)
				}
			} else if prepared != nil {
				t.Fatal("non-claimed result returned runnable work")
			}
		})
	}
	t.Run("rejects-structurally-dead-running-claim", func(t *testing.T) {
		bad := *claim
		bad.Task = claim.Task
		bad.Task.HeartbeatAt = nil
		source := &fakeRAGFairQueueSource{claim: func(context.Context, int64, string, int64, string, time.Duration, store.RAGFairQueueClaimLimits) (store.RAGFairQueueClaimResult, error) {
			return store.RAGFairQueueClaimResult{Disposition: store.RAGFairQueueClaimed, Claim: &bad}, nil
		}}
		prepared, _, err := newTestRAGAdapter(t, source, &fakeRAGFairQueueRunner{}).Prepare(context.Background(), request)
		if prepared != nil || !errors.Is(err, fairqueue.ErrAuthoritativeStateCorrupt) {
			t.Fatalf("Prepare = %T, %v", prepared, err)
		}
	})
	for _, test := range []struct {
		name       string
		leaseUntil time.Time
	}{
		{"lease-equals-heartbeat", claimedAt},
		{"lease-precedes-heartbeat", claimedAt.Add(-time.Second)},
	} {
		t.Run("rejects-"+test.name, func(t *testing.T) {
			bad := *claim
			bad.Task = claim.Task
			bad.Task.LeaseUntil = ptrTime(test.leaseUntil)
			source := &fakeRAGFairQueueSource{claim: func(context.Context, int64, string, int64, string, time.Duration, store.RAGFairQueueClaimLimits) (store.RAGFairQueueClaimResult, error) {
				return store.RAGFairQueueClaimResult{Disposition: store.RAGFairQueueClaimed, Claim: &bad}, nil
			}}
			prepared, _, err := newTestRAGAdapter(t, source, &fakeRAGFairQueueRunner{}).Prepare(context.Background(), request)
			if prepared != nil || !errors.Is(err, fairqueue.ErrAuthoritativeStateCorrupt) {
				t.Fatalf("Prepare = %T, %v", prepared, err)
			}
		})
	}
}

func TestRAGFairQueuePreparedTaskMapsFatalStoreErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want error
	}{
		{"writer", store.ErrFairQueueUnsafeConnection, fairqueue.ErrAuthoritativeWriterMismatch},
		{"version", store.ErrRAGDocumentVersionMismatch, fairqueue.ErrAuthoritativeStateCorrupt},
		{"immutable version", store.ErrRAGDocumentVersionConflict, fairqueue.ErrAuthoritativeStateCorrupt},
		{"immutable source", store.ErrRAGDocumentSourceConflict, fairqueue.ErrAuthoritativeStateCorrupt},
		{"document ai ledger", store.ErrRAGDocumentAILedgerCorrupt, fairqueue.ErrAuthoritativeStateCorrupt},
	} {
		t.Run(test.name, func(t *testing.T) {
			prepared := &ragFairQueuePreparedTask{runner: &fakeRAGFairQueueRunner{err: test.err}, claim: &store.RAGIndexClaim{}}
			if err := prepared.Run(context.Background()); !errors.Is(err, test.want) {
				t.Fatalf("Run error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestRAGFairQueuePrepareClaimErrorFreshReadAndSafetyMapping(t *testing.T) {
	source := &fakeRAGFairQueueSource{
		claim: func(context.Context, int64, string, int64, string, time.Duration, store.RAGFairQueueClaimLimits) (store.RAGFairQueueClaimResult, error) {
			return store.RAGFairQueueClaimResult{}, errors.Join(store.ErrFairQueueUnsafeConnection, errors.New("commit outcome unknown"))
		},
		readTask: func(context.Context, int64) (*store.RAGIndexTaskRecord, error) {
			task := testRAGCandidate(42, 3, testRAGUser).Task
			task.Status, task.ClaimGeneration, task.LeaseOwner = "RUNNING", 3, "rag-fair-test"
			return &task, nil
		},
	}
	adapter := newTestRAGAdapter(t, source, &fakeRAGFairQueueRunner{})
	prepared, _, err := adapter.Prepare(context.Background(), testRAGPrepareRequest(t, testRAGMessage(42, 3, testRAGUser)))
	if prepared != nil || !errors.Is(err, fairqueue.ErrAuthoritativeWriterMismatch) {
		t.Fatalf("Prepare = %T, %v", prepared, err)
	}
	if source.readTaskCalls != 1 {
		t.Fatalf("fresh reads = %d", source.readTaskCalls)
	}
}

func TestRAGFairQueuePreparePoisonRepairsEveryIndependentLocator(t *testing.T) {
	body := testRAGMessage(42, 3, testRAGUser)
	header := testRAGMessage(43, 7, testRAGUser).DispatchToken
	request := testRAGPrepareRequest(t, body)
	request.Message = nil
	request.HeaderToken = &header
	*request.HeaderFacts.TaskID = header.TaskID
	*request.HeaderFacts.DispatchGeneration = int64(header.Generation)
	if err := request.Validate(); err != nil {
		t.Fatalf("poison request: %v", err)
	}
	source := &fakeRAGFairQueueSource{poison: func(_ context.Context, id, generation int64) (*store.RAGIndexTaskDispatchCandidate, error) {
		candidate := testRAGCandidate(id, generation, testRAGUser)
		return &candidate, nil
	}}
	adapter := newTestRAGAdapter(t, source, &fakeRAGFairQueueRunner{})
	prepared, result, err := adapter.Prepare(context.Background(), request)
	if err != nil || prepared != nil {
		t.Fatalf("Prepare = %T,%#v,%v", prepared, result, err)
	}
	if result.Disposition != fairqueue.PreparePoisonPermanentInvalidMessage || result.CanonicalEffect != fairqueue.CanonicalPoisonRepairSettled {
		t.Fatalf("result = %#v", result)
	}
	if !reflect.DeepEqual(source.poisonLookups, []int64{42, 43}) || !reflect.DeepEqual(source.poisonRepairs, []int64{42, 43}) {
		t.Fatalf("lookups=%v repairs=%v", source.poisonLookups, source.poisonRepairs)
	}
	if !reflect.DeepEqual(source.poisonEvents, []string{"get:42", "get:43", "repair:42", "repair:43"}) {
		t.Fatalf("capture/repair order = %v", source.poisonEvents)
	}
}

func TestRAGFairQueuePreparePoisonLocatorConstraints(t *testing.T) {
	body := testRAGMessage(42, 3, testRAGUser)
	header := body.DispatchToken
	for _, test := range []struct {
		name       string
		request    func(*testing.T) fairqueue.PrepareRequest
		candidate  *store.RAGIndexTaskDispatchCandidate
		readTask   *store.RAGIndexTaskRecord
		wantEffect fairqueue.CanonicalEffect
		wantGets   int
		wantRepair int
	}{
		{
			name: "body-only",
			request: func(t *testing.T) fairqueue.PrepareRequest {
				return testRAGPoisonRequest(t, &body, nil, testRAGUser)
			},
			candidate:  ptrRAGCandidate(testRAGCandidate(42, 3, testRAGUser)),
			wantEffect: fairqueue.CanonicalPoisonRepairSettled, wantGets: 1, wantRepair: 1,
		},
		{
			name: "header-only",
			request: func(t *testing.T) fairqueue.PrepareRequest {
				return testRAGPoisonRequest(t, nil, &header, testRAGUser)
			},
			candidate:  ptrRAGCandidate(testRAGCandidate(42, 3, testRAGUser)),
			wantEffect: fairqueue.CanonicalPoisonRepairSettled, wantGets: 1, wantRepair: 1,
		},
		{
			name: "same-row-deduplicated",
			request: func(t *testing.T) fairqueue.PrepareRequest {
				request := testRAGPrepareRequest(t, body)
				request.Message = nil
				request.HeaderFacts.ProtocolVersion = nil
				request.HeaderErrorCode = "missing_protocol"
				if err := request.Validate(); err != nil {
					t.Fatal(err)
				}
				return request
			},
			candidate:  ptrRAGCandidate(testRAGCandidate(42, 3, testRAGUser)),
			wantEffect: fairqueue.CanonicalPoisonRepairSettled, wantGets: 1, wantRepair: 1,
		},
		{
			name: "body-tenant-cannot-borrow-queue-authority",
			request: func(t *testing.T) fairqueue.PrepareRequest {
				other := testRAGMessage(42, 3, "other-user")
				return testRAGPoisonRequest(t, &other, nil, testRAGUser)
			},
			candidate:  ptrRAGCandidate(testRAGCandidate(42, 3, testRAGUser)),
			wantEffect: fairqueue.CanonicalNone, wantGets: 1,
		},
		{
			name: "header-rejected-by-queue-hash",
			request: func(t *testing.T) fairqueue.PrepareRequest {
				return testRAGPoisonRequest(t, nil, &header, testRAGUser)
			},
			candidate:  ptrRAGCandidate(testRAGCandidate(42, 3, "other-user")),
			wantEffect: fairqueue.CanonicalNone, wantGets: 1,
		},
		{
			name: "stale-canonical-is-settled",
			request: func(t *testing.T) fairqueue.PrepareRequest {
				return testRAGPoisonRequest(t, nil, &header, testRAGUser)
			},
			readTask:   ptrRAGTask(testRAGCandidate(42, 5, testRAGUser).Task),
			wantEffect: fairqueue.CanonicalPoisonRepairSettled, wantGets: 1,
		},
		{
			name: "unlocatable",
			request: func(t *testing.T) fairqueue.PrepareRequest {
				return testRAGPoisonRequest(t, nil, &header, testRAGUser)
			},
			wantEffect: fairqueue.CanonicalNone, wantGets: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := &fakeRAGFairQueueSource{}
			if test.candidate != nil {
				source.poison = func(context.Context, int64, int64) (*store.RAGIndexTaskDispatchCandidate, error) {
					return test.candidate, nil
				}
			}
			if test.readTask != nil {
				source.readTask = func(context.Context, int64) (*store.RAGIndexTaskRecord, error) {
					return test.readTask, nil
				}
			}
			adapter := newTestRAGAdapter(t, source, &fakeRAGFairQueueRunner{})
			prepared, result, err := adapter.Prepare(context.Background(), test.request(t))
			if err != nil || prepared != nil || result.Disposition != fairqueue.PreparePoisonPermanentInvalidMessage || result.CanonicalEffect != test.wantEffect {
				t.Fatalf("Prepare = %T, %#v, %v", prepared, result, err)
			}
			if len(source.poisonLookups) != test.wantGets || len(source.poisonRepairs) != test.wantRepair {
				t.Fatalf("lookups=%v repairs=%v", source.poisonLookups, source.poisonRepairs)
			}
		})
	}
}

func TestRAGFairQueueRecoveryBrokerAndReadinessMappings(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	candidate := testRAGCandidate(42, 3, testRAGUser)
	marker := now.Add(-time.Second)
	candidate.Task.DispatchedAt = ptrTime(marker)
	candidate.Guard.DispatchedAt = ptrTime(marker)
	candidate.Guard.DispatchedAtRaw = store.RAGIndexTaskTimestampGuard{
		Raw: marker.Format("2006-01-02 15:04:05.999999999"),
	}
	runningTask := candidate.Task
	runningTask.Status, runningTask.ClaimGeneration = "RUNNING", 3
	runningTask.LeaseOwner, runningTask.LeaseUntil, runningTask.HeartbeatAt = "rag-fair-test", ptrTime(now.Add(time.Minute)), ptrTime(now)
	source := &fakeRAGFairQueueSource{
		highWater: func(context.Context) (int64, error) { return 50, nil },
		tenants: func(context.Context, int64, string, int) ([]string, string, error) {
			return []string{testRAGUser}, testRAGUser, nil
		},
		dispatched: func(context.Context, int64, int64, int) ([]store.RAGIndexTaskRecord, int64, error) {
			return []store.RAGIndexTaskRecord{candidate.Task}, 42, nil
		},
		running: func(context.Context, int64, int64, int) ([]store.RAGIndexTaskRunningSnapshot, int64, error) {
			return []store.RAGIndexTaskRunningSnapshot{{Task: runningTask, ObservedDBNow: now}}, 42, nil
		},
		repairHighWater: func(context.Context) (int64, error) { return 50, nil },
		broker: func(context.Context, int64, int64, int) ([]store.RAGIndexTaskDispatchCandidate, int64, error) {
			return []store.RAGIndexTaskDispatchCandidate{candidate}, 42, nil
		},
		rearmBroker: func(_ context.Context, got store.RAGIndexTaskDispatchCandidate) (*store.RAGIndexTaskDispatchCandidate, bool, error) {
			if !reflect.DeepEqual(got, candidate) {
				t.Fatalf("broker guard changed: %#v", got)
			}
			updated := candidate
			updated.Task.DispatchGeneration++
			updated.Task.DispatchedAt = nil
			updated.Guard.DispatchGeneration++
			updated.Guard.DispatchedAt = nil
			updated.Guard.DispatchedAtRaw = store.RAGIndexTaskTimestampGuard{IsNull: true}
			return &updated, true, nil
		},
	}
	admin := &fakeRAGFairQueueAdmin{
		identity: store.FairQueueWriterIdentity{Fingerprint: testRAGWriter}, count: 1,
		report: store.RAGFairQueueContractReport{
			ExpandSchemaReady: true, TaskCount: 100, RemainingCount: 5,
			OwnerMismatchCount: 2, PendingGenerationMismatchCount: 3,
			PendingDispatchMarkerCount: 7, RunningDispatchMarkerCount: 99,
		},
		highWater: source.highWater, tenants: source.tenants,
		dispatched: source.dispatched, running: source.running,
	}
	journal := &fakeRAGFairQueueJournal{session: &fakeRAGFairQueueJournalSession{}}
	adapter, err := NewRAGFairQueueAdapter(source, &fakeRAGFairQueueRunner{}, admin, journal, RAGFairQueueAdapterOptions{
		WorkerID: "rag-fair-test", LeaseDuration: time.Minute,
		ClaimLimits: store.RAGFairQueueClaimLimits{GlobalConcurrency: 4, PerUserBurstConcurrency: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if high, err := adapter.CaptureHighWater(context.Background()); err != nil || high != "50" {
		t.Fatalf("high=%q err=%v", high, err)
	}
	known, err := adapter.ListKnownTenants(context.Background(), "50", "", 10)
	if err != nil || known.Done || known.NextCursor != testRAGUser || len(known.Items) != 1 {
		t.Fatalf("known=%#v err=%v", known, err)
	}
	dispatched, err := adapter.ListDispatched(context.Background(), "50", "", 10)
	if err != nil || len(dispatched.Items) != 1 || dispatched.Items[0].Token.Generation != 3 {
		t.Fatalf("dispatched=%#v err=%v", dispatched, err)
	}
	running, err := adapter.ListValidRunning(context.Background(), "50", "", 10)
	if err != nil || len(running.Items) != 1 || !running.Items[0].ObservedDBNow.Equal(now) {
		t.Fatalf("running=%#v err=%v", running, err)
	}
	broker, err := adapter.ListBrokerBackedCandidates(context.Background(), "50", "", 10)
	if err != nil || len(broker.Items) != 1 {
		t.Fatalf("broker=%#v err=%v", broker, err)
	}
	updated, changed, err := adapter.RearmAfterBrokerLoss(context.Background(), broker.Items[0])
	if err != nil || !changed || updated.Message.DispatchToken.Generation != 4 {
		t.Fatalf("rearm=%#v,%v,%v", updated, changed, err)
	}
	readiness, err := adapter.CheckSchemaAndInvariants(context.Background())
	if err != nil || readiness.SchemaReady != true || readiness.OwnerInvariantViolationCount != 2 || readiness.GenerationViolationCount != 3 {
		t.Fatalf("readiness=%#v err=%v", readiness, err)
	}
}

func TestRAGFairQueueOperationJournalLosslessStartBridge(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	high := "50"
	record := store.FairQueueOperationRecord{
		Resource: RAGFairQueueResource, OperationID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Kind: store.FairQueueOperationRabbitRepair, Phase: store.FairQueueOperationActive,
		CurrentWriterFingerprint: testRAGWriter, RepairHighWater: &high,
		Version: 2, CreatedAt: now, UpdatedAt: now,
	}
	session := &fakeRAGFairQueueJournalSession{record: record, found: true}
	backend := &fakeRAGFairQueueJournal{record: record, found: true, session: session}
	journal, err := NewRAGFairQueueOperationJournal(backend)
	if err != nil {
		t.Fatal(err)
	}
	got, found, err := journal.Read(context.Background(), RAGFairQueueResource, testRAGWriter)
	if err != nil || !found || got.RepairHighWater == nil || *got.RepairHighWater != high {
		t.Fatalf("Read=%#v,%v,%v", got, found, err)
	}
	err = journal.WithStartFence(context.Background(), RAGFairQueueResource, testRAGWriter, func(start fairqueue.OperationStartSession) error {
		read, found, err := start.Read(context.Background())
		if err != nil || !found || read.OperationID != got.OperationID {
			t.Fatalf("start read=%#v,%v,%v", read, found, err)
		}
		proposal := fairqueue.RecoveryOperationRecord{
			Resource: RAGFairQueueResource, OperationID: "cccccccccccccccccccccccccccccccc",
			Kind: fairqueue.RecoveryRabbitRepair, CurrentWriterFingerprint: testRAGWriter,
		}
		_, err = start.BeginSpecial(context.Background(), nil, proposal)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if session.proposal.OperationID != "cccccccccccccccccccccccccccccccc" || session.proposal.Kind != store.FairQueueOperationRabbitRepair {
		t.Fatalf("proposal=%#v", session.proposal)
	}
}

func ptrTime(value time.Time) *time.Time { return &value }
func ptrRAGCandidate(value store.RAGIndexTaskDispatchCandidate) *store.RAGIndexTaskDispatchCandidate {
	return &value
}
func ptrRAGTask(value store.RAGIndexTaskRecord) *store.RAGIndexTaskRecord { return &value }
