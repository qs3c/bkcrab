package store

import (
	"context"
	"testing"
)

func TestRAGPolicyImmutableCASAndSingleActive(t *testing.T) {
	st := openTestDB(t)
	defer st.Close()
	ctx := context.Background()
	for version := int64(1); version <= 2; version++ {
		if err := st.CreateRAGPolicy(ctx, &RAGPolicyRecord{Kind: RAGPolicyRuntime, Version: version, PolicyJSON: `{}`, Fingerprint: "fingerprint", CreatedBy: "admin"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CreateRAGPolicy(ctx, &RAGPolicyRecord{Kind: RAGPolicyRuntime, Version: 1, PolicyJSON: `{}`, Fingerprint: "changed", CreatedBy: "admin"}); err == nil {
		t.Fatal("immutable policy revision was overwritten")
	}
	if ok, err := st.ActivateRAGPolicy(ctx, RAGPolicyRuntime, 0, 1, "admin", "", "initial", RAGPolicyAuditPublish); err != nil || !ok {
		t.Fatalf("activate v1=%v %v", ok, err)
	}
	if ok, err := st.ActivateRAGPolicy(ctx, RAGPolicyRuntime, 0, 2, "admin", "", "second active", RAGPolicyAuditPublish); err != nil || ok {
		t.Fatalf("second initial active=%v %v", ok, err)
	}
	assertSingleActivePolicy(t, st, RAGPolicyRuntime, 1)
	if ok, err := st.ActivateRAGPolicy(ctx, RAGPolicyRuntime, 1, 2, "admin", "", "publish v2", RAGPolicyAuditPublish); err != nil || !ok {
		t.Fatalf("activate v2=%v %v", ok, err)
	}
	assertSingleActivePolicy(t, st, RAGPolicyRuntime, 2)
	if ok, err := st.ActivateRAGPolicy(ctx, RAGPolicyRuntime, 2, 1, "admin", "", "rollback", RAGPolicyAuditRollback); err != nil || !ok {
		t.Fatalf("rollback v1=%v %v", ok, err)
	}
	assertSingleActivePolicy(t, st, RAGPolicyRuntime, 1)
	var audits int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM rag_policy_audit_log WHERE policy_kind=?`, RAGPolicyRuntime).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 3 {
		t.Fatalf("policy audit count=%d, want 3", audits)
	}
}

func assertSingleActivePolicy(t *testing.T, st *DBStore, kind string, wantVersion int64) {
	t.Helper()
	table, err := policyTable(kind)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	var version int64
	if err := st.DB().QueryRow(`SELECT COUNT(*),COALESCE(MAX(version),0) FROM `+table+` WHERE status='ACTIVE'`).Scan(&count, &version); err != nil {
		t.Fatal(err)
	}
	if count != 1 || version != wantVersion {
		t.Fatalf("active policy count/version=%d/%d, want 1/%d", count, version, wantVersion)
	}
}
