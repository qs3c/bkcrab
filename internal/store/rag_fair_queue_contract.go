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
