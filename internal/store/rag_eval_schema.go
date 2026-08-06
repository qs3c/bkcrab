package store

import (
	"context"
	"fmt"
)

// migrateRAGEvaluationSchema is an additive expansion. It never backfills or
// locks the large document/chunk tables and remains safe while the feature is
// disabled.
func (d *DBStore) migrateRAGEvaluationSchema(ctx context.Context) error {
	id, text, boolean := "TEXT", "TEXT", "BOOLEAN"
	if d.dialect == mysqlDialect {
		id, text, boolean = "VARCHAR(128)", "LONGTEXT", "BOOLEAN"
	}
	statements := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS rag_eval_datasets (
			id %s PRIMARY KEY,name VARCHAR(255) NOT NULL,description %s NOT NULL,created_by %s NOT NULL,
			created_at TIMESTAMP NOT NULL,updated_at TIMESTAMP NOT NULL,deleted_at TIMESTAMP NULL)`, id, text, id),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS rag_eval_dataset_versions (
			id %s PRIMARY KEY,dataset_id %s NOT NULL,version BIGINT NOT NULL,status VARCHAR(32) NOT NULL,
			source_type VARCHAR(64) NOT NULL,manifest_object_key %s NOT NULL,corpus_sha256 VARCHAR(64) NOT NULL,
			case_count BIGINT NOT NULL DEFAULT 0,document_count BIGINT NOT NULL DEFAULT 0,total_bytes BIGINT NOT NULL DEFAULT 0,
			validation_report_json %s NOT NULL,created_by %s NOT NULL,created_at TIMESTAMP NOT NULL,ready_at TIMESTAMP NULL,
			UNIQUE(dataset_id,version))`, id, id, text, text, id),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS rag_eval_corpus_documents (
			id %s PRIMARY KEY,dataset_version_id %s NOT NULL,external_id VARCHAR(255) NOT NULL,file_name VARCHAR(512) NOT NULL,
			media_type VARCHAR(255) NOT NULL,size_bytes BIGINT NOT NULL,sha256 VARCHAR(64) NOT NULL,object_key %s NOT NULL,
			metadata_json %s NOT NULL,UNIQUE(dataset_version_id,external_id))`, id, id, text, text),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS rag_eval_cases (
			id %s PRIMARY KEY,dataset_version_id %s NOT NULL,external_id VARCHAR(255) NOT NULL,user_input %s NOT NULL,
			reference_answer %s NOT NULL,reference_contexts_json %s NOT NULL,reference_context_ids_json %s NOT NULL,
			history_json %s NOT NULL,expected_abstention %s NOT NULL,tags_json %s NOT NULL,metadata_json %s NOT NULL,
			UNIQUE(dataset_version_id,external_id))`, id, id, text, text, text, text, text, boolean, text, text),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS rag_eval_profiles (
			id %s PRIMARY KEY,name VARCHAR(255) NOT NULL,profile_json %s NOT NULL,fingerprint VARCHAR(64) NOT NULL,
			created_by %s NOT NULL,created_at TIMESTAMP NOT NULL)`, id, text, id),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS rag_eval_runs (
			id %s PRIMARY KEY,dataset_version_id %s NOT NULL,baseline_run_id %s NULL,mode VARCHAR(32) NOT NULL,
			profile_id %s NOT NULL,status VARCHAR(32) NOT NULL,stage VARCHAR(64) NOT NULL,progress_json %s NOT NULL,
			execution_snapshot_json %s NOT NULL,index_generation_id %s NULL,requested_metrics_json %s NOT NULL,
			error_code VARCHAR(128) NOT NULL,error_message %s NOT NULL,created_by %s NOT NULL,created_at TIMESTAMP NOT NULL,
			started_at TIMESTAMP NULL,finished_at TIMESTAMP NULL,lease_owner VARCHAR(255) NOT NULL,lease_until TIMESTAMP NULL,
			fence_token BIGINT NOT NULL DEFAULT 0,cancel_requested_at TIMESTAMP NULL,deleted_at TIMESTAMP NULL)`, id, id, id, id, text, text, id, text, text, id),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS rag_eval_case_results (
			run_id %s NOT NULL,case_id %s NOT NULL,response %s NOT NULL,contexts_json %s NOT NULL,citations_json %s NOT NULL,
			search_trace_json %s NOT NULL,answer_trace_json %s NOT NULL,status VARCHAR(32) NOT NULL,error_code VARCHAR(128) NOT NULL,
			error_message %s NOT NULL,latency_ms BIGINT NOT NULL,usage_json %s NOT NULL,PRIMARY KEY(run_id,case_id))`, id, id, text, text, text, text, text, text, text),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS rag_eval_metric_results (
			run_id %s NOT NULL,case_id %s NOT NULL,metric_name VARCHAR(128) NOT NULL,metric_version VARCHAR(64) NOT NULL,
			status VARCHAR(32) NOT NULL,value DOUBLE PRECISION NULL,reason %s NOT NULL,details_json %s NOT NULL,
			PRIMARY KEY(run_id,case_id,metric_name,metric_version))`, id, id, text, text),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS rag_eval_run_aggregates (
			run_id %s NOT NULL,metric_name VARCHAR(128) NOT NULL,slice_key VARCHAR(128) NOT NULL,slice_value VARCHAR(255) NOT NULL,
			count_value BIGINT NOT NULL,scored_count BIGINT NOT NULL,skipped_count BIGINT NOT NULL,error_count BIGINT NOT NULL,
			mean DOUBLE PRECISION NULL,median DOUBLE PRECISION NULL,p50 DOUBLE PRECISION NULL,p95 DOUBLE PRECISION NULL,details_json %s NOT NULL,
			PRIMARY KEY(run_id,metric_name,slice_key,slice_value))`, id, text),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS rag_eval_usage (
			id %s PRIMARY KEY,run_id %s NOT NULL,case_id %s NOT NULL,stage VARCHAR(64) NOT NULL,provider VARCHAR(128) NOT NULL,
			model VARCHAR(255) NOT NULL,input_tokens BIGINT NOT NULL,output_tokens BIGINT NOT NULL,estimated_cost_usd DOUBLE PRECISION NOT NULL,
			actual_cost_usd DOUBLE PRECISION NOT NULL,idempotency_key VARCHAR(255) NOT NULL UNIQUE,created_at TIMESTAMP NOT NULL)`, id, id, id),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS rag_ingestion_policies (
			version BIGINT PRIMARY KEY,policy_json %s NOT NULL,fingerprint VARCHAR(64) NOT NULL,status VARCHAR(32) NOT NULL,
			source_eval_run_id %s NULL,created_by %s NOT NULL,note %s NOT NULL,created_at TIMESTAMP NOT NULL,activated_at TIMESTAMP NULL)`, text, id, id, text),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS rag_runtime_policies (
			version BIGINT PRIMARY KEY,policy_json %s NOT NULL,fingerprint VARCHAR(64) NOT NULL,status VARCHAR(32) NOT NULL,
			source_eval_run_id %s NULL,created_by %s NOT NULL,note %s NOT NULL,created_at TIMESTAMP NOT NULL,activated_at TIMESTAMP NULL)`, text, id, id, text),
		`CREATE TABLE IF NOT EXISTS rag_policy_active_pointers (
			policy_kind VARCHAR(32) PRIMARY KEY,active_version BIGINT NOT NULL,updated_at TIMESTAMP NOT NULL)`,
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS rag_kb_index_generations (
			id %s PRIMARY KEY,kb_id %s NOT NULL,policy_version BIGINT NOT NULL,collection_key VARCHAR(255) NOT NULL UNIQUE,
			embedding_model VARCHAR(255) NOT NULL,embedding_dims BIGINT NOT NULL,status VARCHAR(32) NOT NULL,document_count BIGINT NOT NULL DEFAULT 0,
			chunk_count BIGINT NOT NULL DEFAULT 0,error_code VARCHAR(128) NOT NULL,error_message %s NOT NULL,created_by %s NOT NULL,
			created_at TIMESTAMP NOT NULL,activated_at TIMESTAMP NULL,retired_at TIMESTAMP NULL,rollback_until TIMESTAMP NULL,
			lease_owner VARCHAR(255) NOT NULL,lease_until TIMESTAMP NULL,fence_token BIGINT NOT NULL DEFAULT 0)`, id, id, text, id),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS rag_kb_generation_documents (
			generation_id %s NOT NULL,doc_id %s NOT NULL,doc_version BIGINT NOT NULL,status VARCHAR(32) NOT NULL,
			error_code VARCHAR(128) NOT NULL,error_message %s NOT NULL,PRIMARY KEY(generation_id,doc_id))`, id, id, text),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS rag_kb_policy_sync_tasks (
			id %s PRIMARY KEY,kb_id %s NOT NULL,source_generation_id %s NULL,target_generation_id %s NOT NULL,target_policy_version BIGINT NOT NULL,
			status VARCHAR(32) NOT NULL,progress_json %s NOT NULL,estimate_json %s NOT NULL,requested_by %s NOT NULL,
			cancel_requested_at TIMESTAMP NULL,lease_owner VARCHAR(255) NOT NULL,lease_until TIMESTAMP NULL,fence_token BIGINT NOT NULL DEFAULT 0,
			retry_count BIGINT NOT NULL DEFAULT 0,error_code VARCHAR(128) NOT NULL,error_message %s NOT NULL,created_at TIMESTAMP NOT NULL,
			started_at TIMESTAMP NULL,finished_at TIMESTAMP NULL)`, id, id, id, id, text, text, id, text),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS rag_policy_audit_log (
			id %s PRIMARY KEY,policy_kind VARCHAR(32) NOT NULL,from_version BIGINT NOT NULL,to_version BIGINT NOT NULL,
			action VARCHAR(32) NOT NULL,actor_id %s NOT NULL,source_eval_run_id %s NULL,target_kb_id %s NULL,note %s NOT NULL,created_at TIMESTAMP NOT NULL)`, id, id, id, id, text),
	}
	for _, statement := range statements {
		if err := d.execDDL(ctx, statement); err != nil {
			return err
		}
	}
	for _, column := range []struct{ name, ddl string }{{"pinned_policy_version", "BIGINT NULL"}, {"active_generation_id", id + " NULL"}} {
		if err := d.addRAGColumnIfMissing(ctx, "rag_kbs", column.name, column.ddl); err != nil {
			return err
		}
	}
	return nil
}
