package store

import (
	"context"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

const (
	ragFairQueueMySQLPlanWindowLimit = 4
	ragFairQueueMySQLPlanTenantLimit = 8
)

var ragFairQueueMySQLActualRowsRE = regexp.MustCompile(
	`\(actual time=[^)]* rows=([0-9]+(?:\.[0-9]+)?) loops=([0-9]+)\)`,
)

type ragFairQueueMySQLPlanTenant struct {
	userID string
	kbID   string
}

type ragFairQueueMySQLPlanDocument struct {
	id          string
	tenant      *ragFairQueueMySQLPlanTenant
	nextVersion int64
}

type ragFairQueueMySQLPlanTask struct {
	status             string
	dispatchGeneration int64
	claimGeneration    int64
	dispatchedAt       *time.Time
	leaseOwner         string
	leaseUntil         *time.Time
	heartbeatAt        *time.Time
	nextRunAt          *time.Time
}

type ragFairQueueMySQLPlanFixture struct {
	t      *testing.T
	store  *DBStore
	ctx    context.Context
	prefix string
	users  []string
	kbs    []string
	docs   []string
}

func openRAGFairQueueMySQL84PlanStore(t *testing.T) *DBStore {
	t.Helper()
	dsn := os.Getenv("BKCRAB_TEST_MYSQL_DSN")
	if strings.TrimSpace(dsn) == "" {
		t.Skip("BKCRAB_TEST_MYSQL_DSN is not set")
	}
	store, err := NewDBStore(mysqlDialect, dsn)
	if err != nil {
		t.Fatalf("open MySQL plan fixture: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate MySQL plan fixture: %v", err)
	}
	var version string
	if err := store.db.QueryRowContext(ctx, `SELECT VERSION()`).Scan(&version); err != nil {
		t.Fatalf("read MySQL version: %v", err)
	}
	if !strings.HasPrefix(version, "8.4.") {
		t.Fatalf("RAG fair-queue plan acceptance requires MySQL 8.4, got %q", version)
	}
	return store
}

func newRAGFairQueueMySQLPlanFixture(t *testing.T, store *DBStore) *ragFairQueueMySQLPlanFixture {
	t.Helper()
	fixture := &ragFairQueueMySQLPlanFixture{
		t:      t,
		store:  store,
		ctx:    context.Background(),
		prefix: "fqplan_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
	}
	t.Cleanup(fixture.cleanup)
	return fixture
}

func (f *ragFairQueueMySQLPlanFixture) addTenant(label string) *ragFairQueueMySQLPlanTenant {
	f.t.Helper()
	userID := f.prefix + "_u_" + label
	kbID := f.prefix + "_kb_" + label
	now := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)
	if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO users
		(id,username,email,avatar_url,status) VALUES (?,?,?,?,'active')`,
		userID, userID, userID+"@example.invalid", ""); err != nil {
		f.t.Fatalf("insert plan user %s: %v", label, err)
	}
	f.users = append(f.users, userID)
	if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO rag_kbs
		(id,user_id,name,description,embed_model,embed_dims,status,created_at,updated_at)
		VALUES (?,?,?,?,?,?,'active',?,?)`, kbID, userID, label, "", "plan-embed", 3,
		now, now); err != nil {
		f.t.Fatalf("insert plan KB %s: %v", label, err)
	}
	f.kbs = append(f.kbs, kbID)
	return &ragFairQueueMySQLPlanTenant{userID: userID, kbID: kbID}
}

func (f *ragFairQueueMySQLPlanFixture) addUsersWithoutTasks(labelPrefix string, count int) []string {
	f.t.Helper()
	allUserIDs := make([]string, 0, count)
	const batchSize = 200
	for start := 0; start < count; start += batchSize {
		end := start + batchSize
		if end > count {
			end = count
		}
		values := make([]string, 0, end-start)
		args := make([]any, 0, (end-start)*5)
		userIDs := make([]string, 0, end-start)
		for index := start; index < end; index++ {
			userID := fmt.Sprintf("%s_u_%s_%04d", f.prefix, labelPrefix, index)
			values = append(values, `(?,?,?,?,'active')`)
			args = append(args, userID, userID, userID+"@example.invalid", "")
			userIDs = append(userIDs, userID)
		}
		query := `INSERT INTO users (id,username,email,avatar_url,status) VALUES ` +
			strings.Join(values, ",")
		if _, err := f.store.db.ExecContext(f.ctx, query, args...); err != nil {
			f.t.Fatalf("insert %d no-task users: %v", end-start, err)
		}
		f.users = append(f.users, userIDs...)
		allUserIDs = append(allUserIDs, userIDs...)
	}
	return allUserIDs
}

func (f *ragFairQueueMySQLPlanFixture) addDocument(
	tenant *ragFairQueueMySQLPlanTenant,
	label string,
	status string,
	version int64,
) *ragFairQueueMySQLPlanDocument {
	f.t.Helper()
	docID := f.prefix + "_doc_" + label
	now := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)
	if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO rag_documents
		(id,kb_id,file_name,file_type,object_key,status,error_msg,version,uploaded_at)
		VALUES (?,?,?,?,?,?,?,?,?)`, docID, tenant.kbID, label+".txt", "txt",
		"plan/"+docID, status, "", version, now); err != nil {
		f.t.Fatalf("insert plan document %s: %v", label, err)
	}
	f.docs = append(f.docs, docID)
	return &ragFairQueueMySQLPlanDocument{id: docID, tenant: tenant, nextVersion: 1}
}

func ragFairQueueMySQLPlanTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return *value
}

func (f *ragFairQueueMySQLPlanFixture) addTasks(
	doc *ragFairQueueMySQLPlanDocument,
	seeds []ragFairQueueMySQLPlanTask,
) []int64 {
	f.t.Helper()
	if len(seeds) == 0 {
		return nil
	}
	firstVersion := doc.nextVersion
	createdAt := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)
	const batchSize = 200
	for start := 0; start < len(seeds); start += batchSize {
		end := start + batchSize
		if end > len(seeds) {
			end = len(seeds)
		}
		values := make([]string, 0, end-start)
		args := make([]any, 0, (end-start)*15)
		for index := start; index < end; index++ {
			seed := seeds[index]
			version := firstVersion + int64(index)
			values = append(values, `(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
			args = append(args,
				doc.id, version, doc.tenant.userID, seed.status, 0, 3,
				seed.dispatchGeneration, seed.claimGeneration,
				ragFairQueueMySQLPlanTime(seed.dispatchedAt), seed.leaseOwner,
				ragFairQueueMySQLPlanTime(seed.leaseUntil),
				ragFairQueueMySQLPlanTime(seed.heartbeatAt),
				ragFairQueueMySQLPlanTime(seed.nextRunAt), "", createdAt,
			)
		}
		query := `INSERT INTO rag_index_tasks
			(doc_id,doc_version,user_id,status,retry_count,max_retry,
			dispatch_generation,claim_generation,dispatched_at,lease_owner,
			lease_until,heartbeat_at,next_run_at,error_msg,created_at) VALUES ` +
			strings.Join(values, ",")
		if _, err := f.store.db.ExecContext(f.ctx, query, args...); err != nil {
			f.t.Fatalf("insert %d plan tasks for %s: %v", end-start, doc.id, err)
		}
	}
	doc.nextVersion += int64(len(seeds))
	rows, err := f.store.db.QueryContext(f.ctx, `SELECT id FROM rag_index_tasks
		WHERE doc_id=? AND doc_version>=? AND doc_version<? ORDER BY doc_version`,
		doc.id, firstVersion, doc.nextVersion)
	if err != nil {
		f.t.Fatalf("read plan task IDs for %s: %v", doc.id, err)
	}
	defer rows.Close()
	ids := make([]int64, 0, len(seeds))
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			f.t.Fatalf("scan plan task ID for %s: %v", doc.id, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("iterate plan task IDs for %s: %v", doc.id, err)
	}
	if len(ids) != len(seeds) {
		f.t.Fatalf("plan task IDs for %s = %d, want %d", doc.id, len(ids), len(seeds))
	}
	return ids
}

func (f *ragFairQueueMySQLPlanFixture) addRunningVersion(
	doc *ragFairQueueMySQLPlanDocument,
	version int64,
) {
	f.t.Helper()
	record := testRAGVersion(doc.id, version)
	record.Status = RAGDocumentVersionRunning
	if err := f.store.createRAGDocumentVersion(f.ctx, f.store.db, record); err != nil {
		f.t.Fatalf("insert running version %s/%d: %v", doc.id, version, err)
	}
}

func (f *ragFairQueueMySQLPlanFixture) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, docID := range f.docs {
		if _, err := f.store.db.ExecContext(ctx, `DELETE FROM rag_index_tasks WHERE doc_id=?`, docID); err != nil {
			f.t.Errorf("cleanup plan tasks for %s: %v", docID, err)
		}
		if _, err := f.store.db.ExecContext(ctx, `DELETE FROM rag_document_versions WHERE doc_id=?`, docID); err != nil {
			f.t.Errorf("cleanup plan versions for %s: %v", docID, err)
		}
		if _, err := f.store.db.ExecContext(ctx, `DELETE FROM rag_documents WHERE id=?`, docID); err != nil {
			f.t.Errorf("cleanup plan document %s: %v", docID, err)
		}
	}
	for _, kbID := range f.kbs {
		if _, err := f.store.db.ExecContext(ctx, `DELETE FROM rag_kbs WHERE id=?`, kbID); err != nil {
			f.t.Errorf("cleanup plan KB %s: %v", kbID, err)
		}
	}
	for _, userID := range f.users {
		if _, err := f.store.db.ExecContext(ctx, `DELETE FROM users WHERE id=?`, userID); err != nil {
			f.t.Errorf("cleanup plan user %s: %v", userID, err)
		}
	}
}

func ragFairQueueMySQLExplainTree(
	t *testing.T,
	store *DBStore,
	query string,
	args ...any,
) string {
	t.Helper()
	rows, err := store.db.QueryContext(context.Background(),
		"EXPLAIN ANALYZE FORMAT=TREE "+query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN ANALYZE: %v", err)
	}
	defer rows.Close()
	parts := make([]string, 0, 1)
	for rows.Next() {
		var tree string
		if err := rows.Scan(&tree); err != nil {
			t.Fatalf("scan EXPLAIN ANALYZE tree: %v", err)
		}
		parts = append(parts, tree)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate EXPLAIN ANALYZE tree: %v", err)
	}
	if len(parts) == 0 {
		t.Fatal("EXPLAIN ANALYZE returned no tree")
	}
	return strings.Join(parts, "\n")
}

func ragFairQueueMySQLLeafVisits(
	t *testing.T,
	tree string,
	label string,
	aliases ...string,
) int64 {
	t.Helper()
	var visits int64
	matched := 0
	for _, line := range strings.Split(tree, "\n") {
		normalized := strings.ToLower(strings.ReplaceAll(line, "`", ""))
		if !strings.Contains(normalized, "scan") && !strings.Contains(normalized, "lookup") {
			continue
		}
		usesAlias := false
		for _, alias := range aliases {
			if strings.Contains(normalized, " on "+strings.ToLower(alias)+" ") {
				usesAlias = true
				break
			}
		}
		if !usesAlias {
			continue
		}
		actual := ragFairQueueMySQLActualRowsRE.FindStringSubmatch(line)
		if len(actual) != 3 {
			if strings.Contains(normalized, "never executed") {
				continue
			}
			t.Fatalf("%s access node has no parseable actual rows:\n%s\nfull tree:\n%s",
				label, line, tree)
		}
		rowsPerLoop, err := strconv.ParseFloat(actual[1], 64)
		if err != nil {
			t.Fatalf("parse actual rows %q: %v", actual[1], err)
		}
		loops, err := strconv.ParseInt(actual[2], 10, 64)
		if err != nil {
			t.Fatalf("parse actual loops %q: %v", actual[2], err)
		}
		visits += int64(math.Ceil(rowsPerLoop * float64(loops)))
		matched++
	}
	if matched == 0 {
		t.Fatalf("EXPLAIN tree has no physical %s access node:\n%s", label, tree)
	}
	return visits
}

func ragFairQueueMySQLTaskLeafVisits(t *testing.T, tree string) int64 {
	t.Helper()
	return ragFairQueueMySQLLeafVisits(t, tree, "rag_index_tasks",
		"t", "first_task", "rag_index_tasks")
}

func ragFairQueueMySQLUserLeafVisits(t *testing.T, tree string) int64 {
	t.Helper()
	return ragFairQueueMySQLLeafVisits(t, tree, "users", "candidate_user", "users")
}

func ragFairQueueMySQLDocumentLeafVisits(t *testing.T, tree string) int64 {
	t.Helper()
	return ragFairQueueMySQLLeafVisits(t, tree, "rag_documents", "d", "rag_documents")
}

func ragFairQueueMySQLKBLeafVisits(t *testing.T, tree string) int64 {
	t.Helper()
	return ragFairQueueMySQLLeafVisits(t, tree, "rag_kbs", "kb", "rag_kbs")
}

func assertRAGFairQueueMySQLPlanBound(
	t *testing.T,
	store *DBStore,
	query string,
	args []any,
	maxVisits int64,
) {
	t.Helper()
	tree := ragFairQueueMySQLExplainTree(t, store, query, args...)
	if visits := ragFairQueueMySQLTaskLeafVisits(t, tree); visits > maxVisits {
		t.Fatalf("rag_index_tasks physical visits = %d, want <= %d\n%s", visits, maxVisits, tree)
	}
}

func assertRAGFairQueueMySQLCanonicalPlanBound(
	t *testing.T,
	store *DBStore,
	highWater int64,
	afterUserID string,
	rawOwnerIDs []string,
) {
	t.Helper()
	windowQuery, windowArgs := store.ragTenantOwnerWindowQuery(
		afterUserID, ragFairQueueMySQLPlanTenantLimit,
	)
	windowTree := ragFairQueueMySQLExplainTree(t, store, windowQuery, windowArgs...)
	if visits := ragFairQueueMySQLUserLeafVisits(t, windowTree); visits > 16 {
		t.Fatalf("raw owner users visits = %d, want <= 16\n%s", visits, windowTree)
	}

	filterQuery, filterArgs := store.ragCanonicalTenantFirstTasksQuery(highWater, rawOwnerIDs)
	filterTree := ragFairQueueMySQLExplainTree(t, store, filterQuery, filterArgs...)
	if visits := ragFairQueueMySQLTaskLeafVisits(t, filterTree); visits > 24 {
		t.Fatalf("canonical filter rag_index_tasks visits = %d, want <= 24\n%s", visits, filterTree)
	}
	if visits := ragFairQueueMySQLUserLeafVisits(t, filterTree); visits > 16 {
		t.Fatalf("canonical filter users visits = %d, want <= 16\n%s", visits, filterTree)
	}
	if visits := ragFairQueueMySQLDocumentLeafVisits(t, filterTree); visits > 16 {
		t.Fatalf("canonical filter rag_documents visits = %d, want <= 16\n%s", visits, filterTree)
	}
	if visits := ragFairQueueMySQLKBLeafVisits(t, filterTree); visits > 16 {
		t.Fatalf("canonical filter rag_kbs visits = %d, want <= 16\n%s", visits, filterTree)
	}
}

func assertRAGFairQueueMySQLTwoPhasePlan(
	t *testing.T,
	store *DBStore,
	afterID int64,
	highWater *int64,
	ids []int64,
	filteredQuery string,
	filteredArgs []any,
) {
	t.Helper()
	windowQuery, windowArgs := store.ragIndexTaskIDWindowQuery(
		afterID, highWater, ragFairQueueMySQLPlanWindowLimit,
	)
	assertRAGFairQueueMySQLPlanBound(t, store, windowQuery, windowArgs, 8)
	assertRAGFairQueueMySQLPlanBound(t, store, filteredQuery, filteredArgs, int64(len(ids)*2))
}

func assertRAGFairQueueMySQLIDs(t *testing.T, got, want []int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("task IDs = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("task IDs = %v, want %v", got, want)
		}
	}
}

// ragFairQueueMySQLCapacityCountQuery fixes the schema/query-shape contract for
// Task 9's final MySQL capacity gate. Task 2 deliberately does not expose an
// unused Store API, but the supporting indexes and canonical RUNNING predicate
// must already admit bounded global and per-user counts.
func ragFairQueueMySQLCapacityCountQuery(perUser bool) string {
	query := `WITH rag_fair_clock AS (SELECT UTC_TIMESTAMP(6) AS observed_db_now)
		SELECT COUNT(*) FROM rag_index_tasks t
		JOIN rag_documents d ON d.id=t.doc_id
		JOIN rag_kbs kb ON kb.id=d.kb_id AND kb.user_id=t.user_id
		JOIN users u ON u.id=kb.user_id
		JOIN rag_document_versions v ON v.doc_id=t.doc_id AND v.doc_version=t.doc_version
		CROSS JOIN rag_fair_clock
		WHERE ` + ragFairQueueValidRunningPredicate
	if perUser {
		query += ` AND t.user_id=?`
	}
	return query
}

func TestRAGFairQueueMySQL84PagePlans(t *testing.T) {
	store := openRAGFairQueueMySQL84PlanStore(t)
	fixture := newRAGFairQueueMySQLPlanFixture(t, store)
	past := time.Date(2000, 1, 1, 0, 0, 0, 123456000, time.UTC)
	future := time.Date(2100, 1, 1, 0, 0, 0, 654321000, time.UTC)

	dispatchTenant := fixture.addTenant("dispatch")
	dispatchDoc := fixture.addDocument(dispatchTenant, "dispatch", "PENDING", 1)
	dispatchAfter := fixture.addTasks(dispatchDoc, []ragFairQueueMySQLPlanTask{{
		status: "DONE", dispatchGeneration: 1, claimGeneration: 1,
	}})[0]
	dispatchIDs := fixture.addTasks(dispatchDoc, []ragFairQueueMySQLPlanTask{
		{status: "PENDING", dispatchGeneration: 1, claimGeneration: 0},
		{status: "PENDING", dispatchGeneration: 1, claimGeneration: 0, nextRunAt: &future},
		{status: "RUNNING", dispatchGeneration: 2, claimGeneration: 1,
			leaseOwner: "old-dispatch", leaseUntil: &past, heartbeatAt: &past},
		{status: "PENDING", dispatchGeneration: 1, claimGeneration: 0, dispatchedAt: &past},
	})

	expiredTenant := fixture.addTenant("expired")
	expiredDoc := fixture.addDocument(expiredTenant, "expired", "PROCESSING", 1)
	expiredAfter := fixture.addTasks(expiredDoc, []ragFairQueueMySQLPlanTask{{
		status: "DONE", dispatchGeneration: 1, claimGeneration: 1,
	}})[0]
	expiredIDs := fixture.addTasks(expiredDoc, []ragFairQueueMySQLPlanTask{
		{status: "RUNNING", dispatchGeneration: 1, claimGeneration: 1, dispatchedAt: &past,
			leaseOwner: "expired-1", leaseUntil: &past, heartbeatAt: &past},
		{status: "RUNNING", dispatchGeneration: 1, claimGeneration: 1, dispatchedAt: &past,
			leaseOwner: "expired-2", leaseUntil: &future, heartbeatAt: &past},
		{status: "RUNNING", dispatchGeneration: 2, claimGeneration: 1,
			leaseOwner: "expired-3", leaseUntil: &past, heartbeatAt: &past},
		{status: "RUNNING", dispatchGeneration: 1, claimGeneration: 1, dispatchedAt: &past,
			leaseOwner: "expired-4", leaseUntil: &past, heartbeatAt: &past, nextRunAt: &future},
	})

	dispatchedTenant := fixture.addTenant("dispatched")
	dispatchedDoc := fixture.addDocument(dispatchedTenant, "dispatched", "PENDING", 1)
	dispatchedAfter := fixture.addTasks(dispatchedDoc, []ragFairQueueMySQLPlanTask{{
		status: "DONE", dispatchGeneration: 1, claimGeneration: 1,
	}})[0]
	dispatchedIDs := fixture.addTasks(dispatchedDoc, []ragFairQueueMySQLPlanTask{
		{status: "PENDING", dispatchGeneration: 1, claimGeneration: 0, dispatchedAt: &past},
		{status: "PENDING", dispatchGeneration: 1, claimGeneration: 0},
		{status: "RUNNING", dispatchGeneration: 1, claimGeneration: 1, dispatchedAt: &past,
			leaseOwner: "dispatched-3", leaseUntil: &future, heartbeatAt: &past},
		{status: "DONE", dispatchGeneration: 1, claimGeneration: 1, dispatchedAt: &past},
	})
	dispatchedHighWater := dispatchedIDs[len(dispatchedIDs)-1]
	fixture.addTasks(dispatchedDoc, []ragFairQueueMySQLPlanTask{{
		status: "PENDING", dispatchGeneration: 1, claimGeneration: 0, dispatchedAt: &past,
	}})

	validTenant := fixture.addTenant("valid")
	validAnchorDoc := fixture.addDocument(validTenant, "valid_anchor", "DONE", 1)
	validAfter := fixture.addTasks(validAnchorDoc, []ragFairQueueMySQLPlanTask{{
		status: "DONE", dispatchGeneration: 1, claimGeneration: 1,
	}})[0]
	validDocs := []*ragFairQueueMySQLPlanDocument{
		fixture.addDocument(validTenant, "valid_ok", "PROCESSING", 1),
		fixture.addDocument(validTenant, "valid_version_mismatch", "PROCESSING", 2),
		fixture.addDocument(validTenant, "valid_status_mismatch", "PENDING", 1),
		fixture.addDocument(validTenant, "valid_expired", "PROCESSING", 1),
	}
	validIDs := make([]int64, 0, len(validDocs))
	for index, doc := range validDocs {
		leaseUntil := &future
		if index == len(validDocs)-1 {
			leaseUntil = &past
		}
		id := fixture.addTasks(doc, []ragFairQueueMySQLPlanTask{{
			status: "RUNNING", dispatchGeneration: 1, claimGeneration: 1,
			dispatchedAt: &past, leaseOwner: fmt.Sprintf("valid-%d", index),
			leaseUntil: leaseUntil, heartbeatAt: &past,
		}})[0]
		fixture.addRunningVersion(doc, 1)
		validIDs = append(validIDs, id)
	}
	validHighWater := validIDs[len(validIDs)-1]
	validLaterDoc := fixture.addDocument(validTenant, "valid_after_high_water", "PROCESSING", 1)
	fixture.addTasks(validLaterDoc, []ragFairQueueMySQLPlanTask{{
		status: "RUNNING", dispatchGeneration: 1, claimGeneration: 1,
		dispatchedAt: &past, leaseOwner: "valid-later", leaseUntil: &future, heartbeatAt: &past,
	}})
	fixture.addRunningVersion(validLaterDoc, 1)

	knownUsers := make([]string, 0, ragFairQueueMySQLPlanTenantLimit)
	var knownHighWater int64
	for index := 1; index <= ragFairQueueMySQLPlanTenantLimit; index++ {
		label := fmt.Sprintf("known_%03d", index)
		tenant := fixture.addTenant(label)
		doc := fixture.addDocument(tenant, label, "DONE", 1)
		taskCount := 1
		if index == 1 {
			taskCount = 2048
		}
		seeds := make([]ragFairQueueMySQLPlanTask, taskCount)
		for seedIndex := range seeds {
			seeds[seedIndex] = ragFairQueueMySQLPlanTask{
				status: "DONE", dispatchGeneration: 1, claimGeneration: 1,
			}
		}
		ids := fixture.addTasks(doc, seeds)
		knownHighWater = ids[len(ids)-1]
		knownUsers = append(knownUsers, tenant.userID)
	}
	// These users sort strictly between the known-page cursor and its first task
	// owner. The former users-driven query had to scan every one of them before
	// finding a tenant; the raw users page must stop after its fixed window and
	// advance across the gap even though canonical filtering returns no tenants.
	gapUsers := fixture.addUsersWithoutTasks("known_000_gap", 1024)
	lateKnownTenant := fixture.addTenant("known_009")
	lateKnownDoc := fixture.addDocument(lateKnownTenant, "known_009", "DONE", 1)
	fixture.addTasks(lateKnownDoc, []ragFairQueueMySQLPlanTask{{
		status: "DONE", dispatchGeneration: 1, claimGeneration: 1,
	}})

	for _, table := range []string{
		"rag_index_tasks", "rag_documents", "rag_kbs", "users", "rag_document_versions",
	} {
		if _, err := store.db.ExecContext(context.Background(), "ANALYZE TABLE "+table); err != nil {
			t.Fatalf("analyze %s: %v", table, err)
		}
	}

	t.Run("dispatch", func(t *testing.T) {
		query, args := store.ragDispatchableRAGIndexTasksByIDsQuery(dispatchIDs)
		assertRAGFairQueueMySQLTwoPhasePlan(t, store, dispatchAfter, nil, dispatchIDs, query, args)
		page, next, err := store.ListDispatchableRAGIndexTasksPage(
			context.Background(), dispatchAfter, ragFairQueueMySQLPlanWindowLimit,
		)
		if err != nil || next != dispatchIDs[len(dispatchIDs)-1] {
			t.Fatalf("dispatch page next=%d err=%v", next, err)
		}
		got := make([]int64, 0, len(page))
		for _, candidate := range page {
			got = append(got, candidate.Task.ID)
		}
		assertRAGFairQueueMySQLIDs(t, got, []int64{dispatchIDs[0], dispatchIDs[2]})
	})

	t.Run("expired rearm", func(t *testing.T) {
		query, args := store.ragExpiredRAGIndexTasksByIDsQuery(expiredIDs)
		assertRAGFairQueueMySQLTwoPhasePlan(t, store, expiredAfter, nil, expiredIDs, query, args)
		page, next, err := store.ArmExpiredRAGIndexTasksPage(
			context.Background(), expiredAfter, ragFairQueueMySQLPlanWindowLimit,
		)
		if err != nil || next != expiredIDs[len(expiredIDs)-1] {
			t.Fatalf("expired page next=%d err=%v", next, err)
		}
		got := make([]int64, 0, len(page))
		for _, candidate := range page {
			got = append(got, candidate.Task.ID)
			if candidate.Task.Status != "RUNNING" ||
				candidate.Task.DispatchGeneration != candidate.Task.ClaimGeneration+1 {
				t.Fatalf("armed candidate=%+v", candidate.Task)
			}
		}
		assertRAGFairQueueMySQLIDs(t, got, []int64{expiredIDs[0]})
	})

	t.Run("canonical tenants", func(t *testing.T) {
		afterUserID := gapUsers[len(gapUsers)-1]
		assertRAGFairQueueMySQLCanonicalPlanBound(
			t, store, knownHighWater, afterUserID, knownUsers,
		)
		users, next, err := store.ListCanonicalRAGTenantsPage(
			context.Background(), knownHighWater, afterUserID, ragFairQueueMySQLPlanTenantLimit,
		)
		if err != nil || next != knownUsers[len(knownUsers)-1] {
			t.Fatalf("known tenant page next=%q err=%v users=%v", next, err, users)
		}
		if len(users) != len(knownUsers) {
			t.Fatalf("known tenants=%v, want %v", users, knownUsers)
		}
		for index := range knownUsers {
			if users[index] != knownUsers[index] {
				t.Fatalf("known tenants=%v, want %v", users, knownUsers)
			}
		}
	})

	t.Run("canonical no-task window advances raw cursor", func(t *testing.T) {
		afterUserID := fixture.prefix + "_u_known_000"
		windowQuery, windowArgs := store.ragTenantOwnerWindowQuery(
			afterUserID, ragFairQueueMySQLPlanTenantLimit,
		)
		windowTree := ragFairQueueMySQLExplainTree(t, store, windowQuery, windowArgs...)
		if visits := ragFairQueueMySQLUserLeafVisits(t, windowTree); visits > 16 {
			t.Fatalf("no-task raw users visits = %d, want <= 16\n%s", visits, windowTree)
		}
		users, next, err := store.ListCanonicalRAGTenantsPage(
			context.Background(), knownHighWater, afterUserID, ragFairQueueMySQLPlanTenantLimit,
		)
		if err != nil || len(users) != 0 || next != gapUsers[ragFairQueueMySQLPlanTenantLimit-1] {
			t.Fatalf("empty canonical tenant page next=%q err=%v users=%v", next, err, users)
		}
	})

	t.Run("dispatched high water", func(t *testing.T) {
		query, args := store.ragDispatchedRAGIndexTasksByIDsQuery(dispatchedIDs)
		assertRAGFairQueueMySQLTwoPhasePlan(
			t, store, dispatchedAfter, &dispatchedHighWater, dispatchedIDs, query, args,
		)
		page, next, err := store.ListDispatchedRAGIndexTasksPage(
			context.Background(), dispatchedHighWater, dispatchedAfter,
			ragFairQueueMySQLPlanWindowLimit,
		)
		if err != nil || next != dispatchedHighWater {
			t.Fatalf("dispatched page next=%d highWater=%d err=%v", next, dispatchedHighWater, err)
		}
		got := make([]int64, 0, len(page))
		for _, task := range page {
			got = append(got, task.ID)
		}
		assertRAGFairQueueMySQLIDs(t, got, []int64{dispatchedIDs[0], dispatchedIDs[2]})
	})

	t.Run("valid running high water", func(t *testing.T) {
		query, args := store.ragValidRunningRAGIndexTasksByIDsQuery(validIDs)
		assertRAGFairQueueMySQLTwoPhasePlan(
			t, store, validAfter, &validHighWater, validIDs, query, args,
		)
		page, next, err := store.ListValidRunningRAGIndexTasksPage(
			context.Background(), validHighWater, validAfter, ragFairQueueMySQLPlanWindowLimit,
		)
		if err != nil || next != validHighWater {
			t.Fatalf("valid-running page next=%d highWater=%d err=%v", next, validHighWater, err)
		}
		got := make([]int64, 0, len(page))
		for _, snapshot := range page {
			got = append(got, snapshot.Task.ID)
			if snapshot.ObservedDBNow.IsZero() || snapshot.Task.LeaseUntil == nil ||
				!snapshot.Task.LeaseUntil.After(snapshot.ObservedDBNow) {
				t.Fatalf("invalid running snapshot=%+v", snapshot)
			}
		}
		assertRAGFairQueueMySQLIDs(t, got, []int64{validIDs[0]})
	})

	t.Run("future capacity count schema contract", func(t *testing.T) {
		for _, test := range []struct {
			name string
			args []any
		}{
			{name: "global"},
			{name: "per-user", args: []any{validTenant.userID}},
		} {
			t.Run(test.name, func(t *testing.T) {
				query := ragFairQueueMySQLCapacityCountQuery(len(test.args) != 0)
				assertRAGFairQueueMySQLPlanBound(t, store, query, test.args, 32)
				var count int64
				if err := store.db.QueryRowContext(context.Background(), query, test.args...).Scan(&count); err != nil {
					t.Fatalf("count valid RUNNING: %v", err)
				}
				if count != 2 {
					t.Fatalf("valid RUNNING count = %d, want 2", count)
				}
			})
		}
	})
}
