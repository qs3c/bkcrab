package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

var (
	ErrRAGFairQueueContractAttestationRequired = errors.New("store: fair queue contract requires all writers dual-write attestation")
	ErrRAGFairQueueExpandSchemaRequired        = errors.New("store: fair queue expand schema is incomplete")
	ErrRAGFairQueueContractNotConverged        = errors.New("store: fair queue contract backfill did not converge")
)

const (
	ragFairQueueContractPageSize  = 200
	ragFairQueueContractMaxPasses = 8
)

// FairQueueAdminStore is intentionally not the general Store. Its constructor
// rejects startup auto-migration and non-MySQL databases, so admin commands can
// inspect/apply a contract without accidentally booting the gateway migration
// path.
type FairQueueAdminStore struct {
	db                *DBStore
	writerFingerprint string
}

// RAGFairQueueAdminSource is the non-auto-migrating, expected-writer-bound
// store surface used by RAG recovery and safety operators. It deliberately
// exposes neither the underlying *sql.DB nor the general Store interface.
// Every method which reaches MySQL revalidates the bound writer on a pinned
// physical connection; writerFingerprint cached by FairQueueAdminStore is not
// an authority for these reads.
type RAGFairQueueAdminSource struct {
	admin          *FairQueueAdminStore
	rag            *RAGFairQueueStore
	expectedWriter string
}

func OpenFairQueueAdminStore(cfg StorageConfig) (*FairQueueAdminStore, error) {
	if cfg.Type != StorageMySQL {
		return nil, ErrFairQueueMySQLRequired
	}
	if cfg.AutoMigrate {
		return nil, errors.New("store: fair queue admin store requires autoMigrate=false")
	}
	if cfg.DSN == "" {
		return nil, errors.New("store: fair queue admin store requires a MySQL DSN")
	}
	db, err := NewDBStore(mysqlDialect, cfg.DSN)
	if err != nil {
		return nil, err
	}
	admin := &FairQueueAdminStore{db: db}
	identity, err := db.discoverFairQueueWriterIdentity(context.Background())
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	admin.writerFingerprint = identity.fingerprint
	return admin, nil
}

func (s *FairQueueAdminStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// WriterFingerprint returns the database-bound SHA-256 identity. It is safe to
// compare with Redis control metadata; it contains no DSN credentials.
func (s *FairQueueAdminStore) WriterFingerprint() string {
	if s == nil {
		return ""
	}
	return s.writerFingerprint
}

// BindRAGFairQueueSource creates the narrow admin/recovery facade for one
// expected writer. Binding validates shape and MySQL-only usage but performs no
// identity discovery; ReadWriterIdentity and every subsequent operation do a
// fresh pinned-connection verification.
func (s *FairQueueAdminStore) BindRAGFairQueueSource(
	expectedWriter string,
) (*RAGFairQueueAdminSource, error) {
	if s == nil || s.db == nil {
		return nil, ErrFairQueueMySQLRequired
	}
	rag, err := s.db.BindRAGFairQueueWriter(expectedWriter)
	if err != nil {
		return nil, err
	}
	return &RAGFairQueueAdminSource{
		admin: s, rag: rag, expectedWriter: expectedWriter,
	}, nil
}

func (s *RAGFairQueueAdminSource) validate() error {
	if s == nil || s.admin == nil || s.admin.db == nil || s.rag == nil ||
		s.rag.store != s.admin.db || s.rag.expectedWriter != s.expectedWriter {
		return ErrFairQueueUnsafeConnection
	}
	if !lowerHex64Pattern.MatchString(s.expectedWriter) {
		return ErrFairQueueWriterMismatch
	}
	if s.admin.db.dialect != mysqlDialect {
		return ErrFairQueueMySQLRequired
	}
	return nil
}

func (s *RAGFairQueueAdminSource) requireExpectedWriter(expectedWriter string) error {
	if err := s.validate(); err != nil {
		return err
	}
	if expectedWriter != s.expectedWriter {
		return ErrFairQueueWriterMismatch
	}
	return nil
}

// ReadWriterIdentity performs a fresh expected-writer check. In particular it
// never echoes FairQueueAdminStore.WriterFingerprint, which is only the
// construction-time observation used by CLI presentation.
func (s *RAGFairQueueAdminSource) ReadWriterIdentity(
	ctx context.Context,
) (FairQueueWriterIdentity, error) {
	if err := s.validate(); err != nil {
		return FairQueueWriterIdentity{}, err
	}
	var writer FairQueueWriterIdentity
	err := s.rag.withExpectedWriterConn(ctx,
		func(_ *sql.Conn, identity fairQueueMySQLIdentity) error {
			writer = FairQueueWriterIdentity{Fingerprint: identity.fingerprint}
			return nil
		})
	if err != nil {
		return FairQueueWriterIdentity{}, err
	}
	return writer, nil
}

// CheckSchemaAndInvariants returns the complete aggregate contract report from
// the bound writer. The adapter maps this losslessly to its domain-neutral
// readiness report; no cached startup inspection is substituted here.
func (s *RAGFairQueueAdminSource) CheckSchemaAndInvariants(
	ctx context.Context,
) (RAGFairQueueContractReport, error) {
	if err := s.validate(); err != nil {
		return RAGFairQueueContractReport{}, err
	}
	var report RAGFairQueueContractReport
	err := s.rag.withExpectedWriterConn(ctx,
		func(conn *sql.Conn, _ fairQueueMySQLIdentity) error {
			var err error
			report, err = s.admin.inspectRAGFairQueueContractOnConn(ctx, conn)
			return err
		})
	if err != nil {
		return RAGFairQueueContractReport{}, err
	}
	return report, nil
}

func (d *DBStore) countValidRunningRAGIndexTasksOn(
	ctx context.Context,
	session ragFairQueueSession,
) (int64, error) {
	if d == nil || session == nil {
		return 0, ErrFairQueueUnsafeConnection
	}
	materialized := ""
	if d.dialect == "postgres" {
		materialized = "MATERIALIZED "
	}
	var count int64
	err := session.QueryRowContext(ctx, fmt.Sprintf(`WITH rag_fair_clock AS %s(SELECT %s AS observed_db_now)
		SELECT COUNT(*) FROM rag_index_tasks t
		JOIN rag_documents d ON d.id=t.doc_id
		JOIN rag_kbs kb ON kb.id=d.kb_id AND kb.user_id=t.user_id
		JOIN users u ON u.id=kb.user_id
		JOIN rag_document_versions v ON v.doc_id=t.doc_id AND v.doc_version=t.doc_version
		CROSS JOIN rag_fair_clock
		WHERE `+ragFairQueueValidRunningPredicate, materialized, d.ragNowExpr())).Scan(&count)
	if err != nil {
		return 0, err
	}
	if count < 0 {
		return 0, ErrFairQueueUnsafeConnection
	}
	return count, nil
}

// CountValidRunning uses exactly the same canonical RUNNING predicate and
// database clock as recovery snapshots and the final MySQL capacity gate.
func (s *RAGFairQueueAdminSource) CountValidRunning(ctx context.Context) (int64, error) {
	if err := s.validate(); err != nil {
		return 0, err
	}
	var count int64
	err := s.rag.withExpectedWriterConn(ctx,
		func(conn *sql.Conn, _ fairQueueMySQLIdentity) error {
			var err error
			count, err = s.admin.db.countValidRunningRAGIndexTasksOn(ctx, conn)
			return err
		})
	if err != nil {
		return 0, err
	}
	return count, nil
}

// The recovery delegates retain Task 9's bounded keyset pages and their
// writer-bound snapshot withholding semantics.
func (s *RAGFairQueueAdminSource) CaptureRAGFairQueueHighWater(
	ctx context.Context,
) (int64, error) {
	if err := s.validate(); err != nil {
		return 0, err
	}
	return s.rag.CaptureRAGFairQueueHighWater(ctx)
}

func (s *RAGFairQueueAdminSource) ListCanonicalRAGTenantsPage(
	ctx context.Context,
	highWater int64,
	afterUserID string,
	limit int,
) ([]string, string, error) {
	if err := s.validate(); err != nil {
		return nil, afterUserID, err
	}
	return s.rag.ListCanonicalRAGTenantsPage(ctx, highWater, afterUserID, limit)
}

func (s *RAGFairQueueAdminSource) ListDispatchedRAGIndexTasksPage(
	ctx context.Context,
	highWater, afterTaskID int64,
	limit int,
) ([]RAGIndexTaskRecord, int64, error) {
	if err := s.validate(); err != nil {
		return nil, afterTaskID, err
	}
	return s.rag.ListDispatchedRAGIndexTasksPage(ctx, highWater, afterTaskID, limit)
}

func (s *RAGFairQueueAdminSource) ListValidRunningRAGIndexTasksPage(
	ctx context.Context,
	highWater, afterTaskID int64,
	limit int,
) ([]RAGIndexTaskRunningSnapshot, int64, error) {
	if err := s.validate(); err != nil {
		return nil, afterTaskID, err
	}
	return s.rag.ListValidRunningRAGIndexTasksPage(ctx, highWater, afterTaskID, limit)
}

// The journal bridge keeps store-native records/proposals intact. In
// particular WithFairQueueOperationStartFence passes through the exact start
// session that owns the named lock, so adapter Read/BeginSpecial calls cannot
// accidentally hop to another pooled connection.
func (s *RAGFairQueueAdminSource) ReadFairQueueOperation(
	ctx context.Context,
	resource, expectedWriter string,
) (FairQueueOperationRecord, bool, error) {
	if err := s.requireExpectedWriter(expectedWriter); err != nil {
		return FairQueueOperationRecord{}, false, err
	}
	record, found, err := s.admin.db.ReadFairQueueOperation(ctx, resource, expectedWriter)
	if err != nil {
		return FairQueueOperationRecord{}, false, err
	}
	return record, found, nil
}

func (s *RAGFairQueueAdminSource) WithFairQueueOperationStartFence(
	ctx context.Context,
	resource, expectedWriter string,
	fn func(*FairQueueOperationStartSession) error,
) error {
	if err := s.requireExpectedWriter(expectedWriter); err != nil {
		return err
	}
	return s.admin.db.WithFairQueueOperationStartFence(ctx, resource, expectedWriter, fn)
}

func (s *RAGFairQueueAdminSource) requireOperationWriter(
	expected FairQueueOperationRecord,
) error {
	if err := s.validate(); err != nil {
		return err
	}
	if expected.CurrentWriterFingerprint != s.expectedWriter {
		return ErrFairQueueWriterMismatch
	}
	return nil
}

func (s *RAGFairQueueAdminSource) SetFairQueueOperationRepairHighWater(
	ctx context.Context,
	expected FairQueueOperationRecord,
	highWater string,
) (FairQueueOperationRecord, error) {
	if err := s.requireOperationWriter(expected); err != nil {
		return FairQueueOperationRecord{}, err
	}
	record, err := s.admin.db.SetFairQueueOperationRepairHighWater(ctx, expected, highWater)
	if err != nil {
		return FairQueueOperationRecord{}, err
	}
	return record, nil
}

func (s *RAGFairQueueAdminSource) MarkFairQueueOperationRepairPassComplete(
	ctx context.Context,
	expected FairQueueOperationRecord,
) (FairQueueOperationRecord, error) {
	if err := s.requireOperationWriter(expected); err != nil {
		return FairQueueOperationRecord{}, err
	}
	record, err := s.admin.db.MarkFairQueueOperationRepairPassComplete(ctx, expected)
	if err != nil {
		return FairQueueOperationRecord{}, err
	}
	return record, nil
}

func (s *RAGFairQueueAdminSource) MarkFairQueueOperationForceDeletePassComplete(
	ctx context.Context,
	expected FairQueueOperationRecord,
) (FairQueueOperationRecord, error) {
	if err := s.requireOperationWriter(expected); err != nil {
		return FairQueueOperationRecord{}, err
	}
	record, err := s.admin.db.MarkFairQueueOperationForceDeletePassComplete(ctx, expected)
	if err != nil {
		return FairQueueOperationRecord{}, err
	}
	return record, nil
}

func (s *RAGFairQueueAdminSource) CommitFairQueueOperationReady(
	ctx context.Context,
	expected FairQueueOperationRecord,
) (FairQueueOperationRecord, error) {
	if err := s.requireOperationWriter(expected); err != nil {
		return FairQueueOperationRecord{}, err
	}
	record, err := s.admin.db.CommitFairQueueOperationReady(ctx, expected)
	if err != nil {
		return FairQueueOperationRecord{}, err
	}
	return record, nil
}

func (s *RAGFairQueueAdminSource) CompleteFairQueueOperation(
	ctx context.Context,
	expected FairQueueOperationRecord,
) (FairQueueOperationRecord, error) {
	if err := s.requireOperationWriter(expected); err != nil {
		return FairQueueOperationRecord{}, err
	}
	record, err := s.admin.db.CompleteFairQueueOperation(ctx, expected)
	if err != nil {
		return FairQueueOperationRecord{}, err
	}
	return record, nil
}

// RAGFairQueueContractAttestation is an operator assertion, not a database
// inference. True means pre-expand writers have been stopped and every
// remaining writer is known to dual-write tenant/generation fields.
type RAGFairQueueContractAttestation struct {
	AllWritersDualWrite bool
}

// RAGFairQueueContractReport deliberately contains aggregates only. Raw task
// IDs and keyset cursors must never be emitted by the admin CLI.
type RAGFairQueueContractReport struct {
	ExpandSchemaReady              bool
	UserIDNullable                 bool
	TaskCount                      int64
	MissingUserIDCount             int64
	UnresolvedOwnerCount           int64
	OwnerMismatchCount             int64
	NonPositiveGenerationCount     int64
	ExhaustedGenerationCount       int64
	PendingGenerationMismatchCount int64
	RunningGenerationMismatchCount int64
	PendingDispatchMarkerCount     int64
	RunningDispatchMarkerCount     int64
	RemainingCount                 int64
	PagesScanned                   int64
	RowsChanged                    int64
	Contracted                     bool
}

type ragFairQueueContractQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type ragFairQueueColumnDefinition struct {
	nullable, dataType, columnType string
	defaultValue                   sql.NullString
	characterMaximumLength         sql.NullInt64
	datetimePrecision              sql.NullInt64
}

func ragFairQueueExpandDefinitionsReady(
	columns map[string]ragFairQueueColumnDefinition,
) (ready, userIDNullable bool) {
	user, hasUser := columns["user_id"]
	dispatched, hasDispatched := columns["dispatched_at"]
	generation, hasGeneration := columns["dispatch_generation"]
	userIDNullable = hasUser && strings.EqualFold(user.nullable, "YES")
	validUser := hasUser &&
		(strings.EqualFold(user.nullable, "YES") || strings.EqualFold(user.nullable, "NO")) &&
		strings.EqualFold(user.dataType, "varchar") &&
		strings.EqualFold(user.columnType, "varchar(120)") &&
		user.characterMaximumLength.Valid && user.characterMaximumLength.Int64 == 120
	validDispatched := hasDispatched && strings.EqualFold(dispatched.nullable, "YES") &&
		strings.EqualFold(dispatched.dataType, "datetime") &&
		strings.EqualFold(dispatched.columnType, "datetime(6)") &&
		dispatched.datetimePrecision.Valid && dispatched.datetimePrecision.Int64 == 6
	validGeneration := hasGeneration && strings.EqualFold(generation.nullable, "NO") &&
		strings.EqualFold(generation.dataType, "bigint") &&
		strings.EqualFold(generation.columnType, "bigint") &&
		generation.defaultValue.Valid && generation.defaultValue.String == "1"
	return validUser && validDispatched && validGeneration, userIDNullable
}

func inspectRAGFairQueueExpandColumns(
	ctx context.Context,
	queryer ragFairQueueContractQueryer,
) (ready, userIDNullable bool, err error) {
	columns := map[string]ragFairQueueColumnDefinition{}
	for _, column := range []string{"user_id", "dispatched_at", "dispatch_generation"} {
		var item ragFairQueueColumnDefinition
		err := queryer.QueryRowContext(ctx, `SELECT is_nullable,data_type,column_type,column_default,
			character_maximum_length,datetime_precision
			FROM information_schema.columns
			WHERE table_schema=DATABASE() AND table_name='rag_index_tasks' AND column_name=?`,
			column).Scan(&item.nullable, &item.dataType, &item.columnType, &item.defaultValue,
			&item.characterMaximumLength, &item.datetimePrecision)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return false, false, err
		}
		columns[column] = item
	}
	ready, userIDNullable = ragFairQueueExpandDefinitionsReady(columns)
	return ready, userIDNullable, nil
}

func queryRAGFairQueueContractAggregates(
	ctx context.Context,
	queryer ragFairQueueContractQueryer,
) (report RAGFairQueueContractReport, err error) {
	err = queryer.QueryRowContext(ctx, `SELECT
		COUNT(*),
		COALESCE(SUM(CASE WHEN t.user_id IS NULL OR TRIM(t.user_id)='' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN d.id IS NULL OR k.id IS NULL OR TRIM(k.user_id)='' OR
			owner_user.id IS NULL THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN k.id IS NOT NULL AND TRIM(k.user_id)<>'' AND
			t.user_id IS NOT NULL AND TRIM(t.user_id)<>'' AND HEX(t.user_id)<>HEX(k.user_id) THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN t.dispatch_generation<=0 OR t.claim_generation<0 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN t.status IN ('PENDING','RUNNING') AND
			(t.dispatch_generation=9223372036854775807 OR t.claim_generation=9223372036854775807)
			THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN t.status='PENDING' AND
			t.dispatch_generation<=t.claim_generation THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN t.status='RUNNING' AND
			t.dispatch_generation<>t.claim_generation THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN t.status='PENDING' AND t.dispatched_at IS NOT NULL THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN t.status='RUNNING' AND t.dispatched_at IS NOT NULL THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN
			t.user_id IS NULL OR TRIM(t.user_id)='' OR d.id IS NULL OR k.id IS NULL OR TRIM(k.user_id)='' OR
			owner_user.id IS NULL OR
			(k.id IS NOT NULL AND TRIM(k.user_id)<>'' AND t.user_id IS NOT NULL AND TRIM(t.user_id)<>'' AND HEX(t.user_id)<>HEX(k.user_id)) OR
			t.dispatch_generation<=0 OR t.claim_generation<0 OR
			(t.status IN ('PENDING','RUNNING') AND
				(t.dispatch_generation=9223372036854775807 OR t.claim_generation=9223372036854775807)) OR
			(t.status='PENDING' AND t.dispatch_generation<=t.claim_generation) OR
			(t.status='RUNNING' AND t.dispatch_generation<>t.claim_generation) OR
			(t.status='PENDING' AND t.dispatched_at IS NOT NULL)
			THEN 1 ELSE 0 END),0)
	FROM rag_index_tasks t
	LEFT JOIN rag_documents d ON d.id=t.doc_id
	LEFT JOIN rag_kbs k ON k.id=d.kb_id
	LEFT JOIN users owner_user ON owner_user.id=k.user_id AND HEX(owner_user.id)=HEX(k.user_id)`).Scan(
		&report.TaskCount, &report.MissingUserIDCount, &report.UnresolvedOwnerCount,
		&report.OwnerMismatchCount, &report.NonPositiveGenerationCount,
		&report.ExhaustedGenerationCount,
		&report.PendingGenerationMismatchCount, &report.RunningGenerationMismatchCount,
		&report.PendingDispatchMarkerCount, &report.RunningDispatchMarkerCount,
		&report.RemainingCount)
	return report, err
}

func (s *FairQueueAdminStore) inspectRAGFairQueueContractOnConn(
	ctx context.Context,
	conn *sql.Conn,
) (RAGFairQueueContractReport, error) {
	ready, nullable, err := inspectRAGFairQueueExpandColumns(ctx, conn)
	if err != nil {
		return RAGFairQueueContractReport{}, err
	}
	report := RAGFairQueueContractReport{
		ExpandSchemaReady: ready,
		UserIDNullable:    nullable,
	}
	if !ready {
		return report, nil
	}
	aggregates, err := queryRAGFairQueueContractAggregates(ctx, conn)
	if err != nil {
		return RAGFairQueueContractReport{}, err
	}
	aggregates.ExpandSchemaReady = true
	aggregates.UserIDNullable = nullable
	aggregates.PagesScanned = (aggregates.TaskCount + ragFairQueueContractPageSize - 1) /
		ragFairQueueContractPageSize
	aggregates.Contracted = !nullable && aggregates.RemainingCount == 0
	return aggregates, nil
}

func (s *FairQueueAdminStore) CheckRAGFairQueueContract(
	ctx context.Context,
) (report RAGFairQueueContractReport, err error) {
	if s == nil || s.db == nil || !lowerHex64Pattern.MatchString(s.writerFingerprint) {
		return report, ErrFairQueueUnsafeConnection
	}
	err = s.db.withFairQueueExpectedWriterConn(ctx, s.writerFingerprint,
		func(conn *sql.Conn, _ fairQueueMySQLIdentity) error {
			var inspectErr error
			report, inspectErr = s.inspectRAGFairQueueContractOnConn(ctx, conn)
			return inspectErr
		})
	return report, err
}

func (s *FairQueueAdminStore) ApplyRAGFairQueueContract(
	ctx context.Context,
	attestation RAGFairQueueContractAttestation,
) (report RAGFairQueueContractReport, err error) {
	// Check attestation before obtaining a connection or executing any statement.
	if !attestation.AllWritersDualWrite {
		return report, ErrRAGFairQueueContractAttestationRequired
	}
	if s == nil || s.db == nil || !lowerHex64Pattern.MatchString(s.writerFingerprint) {
		return report, ErrFairQueueUnsafeConnection
	}
	err = s.db.withFairQueueExpectedWriterConn(ctx, s.writerFingerprint,
		func(conn *sql.Conn, startIdentity fairQueueMySQLIdentity) error {
			initial, inspectErr := s.inspectRAGFairQueueContractOnConn(ctx, conn)
			if inspectErr != nil {
				return inspectErr
			}
			if !initial.ExpandSchemaReady {
				return ErrRAGFairQueueExpandSchemaRequired
			}
			if initial.Contracted {
				report = initial
				return nil
			}

			var pages, changed int64
			converged := false
			for pass := 0; pass < ragFairQueueContractMaxPasses; pass++ {
				var cursor int64
				var passChanged int64
				for {
					next, pageChanged, done, pageErr := s.db.backfillRAGFairQueueTasksPage(
						ctx, conn, cursor, ragFairQueueContractPageSize)
					if pageErr != nil {
						return pageErr
					}
					pages++
					changed += pageChanged
					passChanged += pageChanged
					if done {
						break
					}
					if next <= cursor {
						return fmt.Errorf("%w: non-advancing keyset page", ErrRAGFairQueueContractNotConverged)
					}
					cursor = next
				}
				current, inspectErr := s.inspectRAGFairQueueContractOnConn(ctx, conn)
				if inspectErr != nil {
					return inspectErr
				}
				if current.RemainingCount == 0 {
					converged = true
					break
				}
				if passChanged == 0 {
					return fmt.Errorf("%w: %d rows remain", ErrRAGFairQueueContractNotConverged,
						current.RemainingCount)
				}
			}
			if !converged {
				return ErrRAGFairQueueContractNotConverged
			}

			beforeDDL, identityErr := readFairQueueMySQLIdentity(ctx, conn)
			if identityErr != nil || !sameFairQueueMySQLSession(startIdentity, beforeDDL) {
				if identityErr != nil {
					return errors.Join(ErrFairQueueUnsafeConnection, identityErr)
				}
				return ErrFairQueueWriterMismatch
			}
			preDDL, inspectErr := s.inspectRAGFairQueueContractOnConn(ctx, conn)
			if inspectErr != nil {
				return inspectErr
			}
			if preDDL.RemainingCount != 0 {
				return ErrRAGFairQueueContractNotConverged
			}
			if preDDL.UserIDNullable {
				if _, alterErr := conn.ExecContext(ctx, `ALTER TABLE rag_index_tasks
					MODIFY COLUMN user_id VARCHAR(120) NOT NULL`); alterErr != nil {
					return alterErr
				}
			}

			report, inspectErr = s.inspectRAGFairQueueContractOnConn(ctx, conn)
			if inspectErr != nil {
				return inspectErr
			}
			report.PagesScanned = pages
			report.RowsChanged = changed
			if !report.Contracted {
				return ErrRAGFairQueueContractNotConverged
			}
			endIdentity, identityErr := readFairQueueMySQLIdentity(ctx, conn)
			if identityErr != nil || !sameFairQueueMySQLSession(startIdentity, endIdentity) {
				if identityErr != nil {
					return errors.Join(ErrFairQueueUnsafeConnection, identityErr)
				}
				return ErrFairQueueWriterMismatch
			}
			return nil
		})
	return report, err
}
