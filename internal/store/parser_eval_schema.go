package store

import (
	"context"
	"fmt"
)

var parserEvaluationSchemaTables = []string{
	"parser_eval_runs",
	"parser_eval_documents",
}

// migrateParserEvaluationSchema is deliberately small: all structured step
// results are closed JSON values on these two rows, while large evidence stays
// in the existing object store.
func (d *DBStore) migrateParserEvaluationSchema(ctx context.Context) error {
	id, text, timestamp := "TEXT", "TEXT", "TIMESTAMP"
	if d.dialect == mysqlDialect {
		id, text, timestamp = "VARCHAR(128)", "LONGTEXT", "DATETIME(6)"
	}
	statements := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS parser_eval_runs (
			id %s PRIMARY KEY,
			status VARCHAR(32) NOT NULL,
			stage VARCHAR(64) NOT NULL,
			progress_json %s NOT NULL,
			execution_snapshot_json %s NOT NULL,
			summary_json %s NOT NULL,
			created_by %s NOT NULL,
			error_code VARCHAR(128) NOT NULL,
			error_message %s NOT NULL,
			created_at %s NOT NULL,
			updated_at %s NOT NULL,
			started_at %s NULL,
			finished_at %s NULL,
			expires_at %s NOT NULL,
			lease_owner VARCHAR(255) NOT NULL,
			lease_until %s NULL,
			fence_token BIGINT NOT NULL DEFAULT 0,
			cancel_requested_at %s NULL
		)`, id, text, text, text, id, text, timestamp, timestamp, timestamp, timestamp, timestamp, timestamp, timestamp),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS parser_eval_documents (
			id %s PRIMARY KEY,
			run_id %s NOT NULL,
			ordinal BIGINT NOT NULL,
			file_name VARCHAR(512) NOT NULL,
			format VARCHAR(16) NOT NULL,
			media_type VARCHAR(255) NOT NULL,
			size_bytes BIGINT NOT NULL,
			sha256 VARCHAR(64) NOT NULL,
			source_object_key %s NOT NULL,
			status VARCHAR(32) NOT NULL,
			stage VARCHAR(64) NOT NULL,
			truth_json %s NOT NULL,
			markitdown_result_json %s NOT NULL,
			anydoc_result_json %s NOT NULL,
			judge_result_json %s NOT NULL,
			error_code VARCHAR(128) NOT NULL,
			error_message %s NOT NULL,
			created_at %s NOT NULL,
			updated_at %s NOT NULL,
			UNIQUE(run_id, ordinal)
		)`, id, id, text, text, text, text, text, text, timestamp, timestamp),
	}
	for _, statement := range statements {
		if d.dialect == mysqlDialect {
			statement += " ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE " + mysqlRAGSchemaCollation
		}
		if err := d.execDDL(ctx, statement); err != nil {
			return err
		}
	}
	for _, statement := range []string{
		`CREATE INDEX IF NOT EXISTS idx_parser_eval_runs_claim ON parser_eval_runs(status, lease_until, created_at, id)`,
		`CREATE INDEX IF NOT EXISTS idx_parser_eval_runs_expiry ON parser_eval_runs(status, expires_at, id)`,
		`CREATE INDEX IF NOT EXISTS idx_parser_eval_documents_run ON parser_eval_documents(run_id, ordinal)`,
	} {
		if err := d.execDDL(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}
