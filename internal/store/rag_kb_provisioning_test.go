package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func testRAGProvisioningKB(userID, kbID string) *RAGKBRecord {
	return &RAGKBRecord{
		ID: kbID, UserID: userID, Name: "provisioning",
		EmbedProvider: "system", EmbedModel: "embed-v1", EmbedDims: 4,
		ChunkSize: 512, ChunkOverlap: 64, ParseMode: RAGParseModeStandard,
	}
}

func TestRAGKBProvisioningFenceBlocksCleanupAndStaleActivation(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	ensureRAGLifecycleUser(t, st, "u_kb_provision", "active")
	ctx := context.Background()
	kb := testRAGProvisioningKB("u_kb_provision", "kb_provision_fence")
	fence, err := st.BeginRAGKBProvisioning(ctx, kb, "provision-worker", time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := st.GetRAGKB(ctx, kb.ID)
	if err != nil || stored.Status != RAGKBStatusProvisioning {
		t.Fatalf("provisioning KB=%+v err=%v", stored, err)
	}
	if ready, err := st.IsRAGKBCleanupReady(ctx, kb.ID); err != nil || ready {
		t.Fatalf("active provisioning cleanup ready=%v err=%v", ready, err)
	}
	if _, err := st.BeginRAGKBProvisioning(ctx,
		testRAGProvisioningKB(kb.UserID, "kb_over_quota"), "other-worker", time.Minute, 1,
	); !errors.Is(err, ErrRAGKBQuotaExceeded) {
		t.Fatalf("concurrent quota error=%v", err)
	}
	stale := *fence
	stale.Generation++
	if ok, err := st.HeartbeatRAGKBProvisioning(ctx, stale, time.Minute); err != nil || ok {
		t.Fatalf("stale heartbeat ok=%v err=%v", ok, err)
	}

	marked, err := st.MarkRAGKBDeleting(ctx, kb.ID)
	if err != nil || marked.Status != RAGKBStatusDeleting {
		t.Fatalf("mark deleting=%+v err=%v", marked, err)
	}
	if ok, err := st.HeartbeatRAGKBProvisioning(ctx, *fence, time.Minute); err != nil || ok {
		t.Fatalf("heartbeat crossed KB tombstone ok=%v err=%v", ok, err)
	}
	if err := st.DeleteRAGKB(ctx, kb.ID); !errors.Is(err, ErrRAGKBProvisioningActive) {
		t.Fatalf("finalizer before provisioning quiescence err=%v", err)
	}
	aborted, ok, err := st.AbortRAGKBProvisioning(ctx, *fence)
	if err != nil || !ok || aborted.Status != RAGKBStatusDeleting {
		t.Fatalf("abort=%+v ok=%v err=%v", aborted, ok, err)
	}
	if ready, err := st.IsRAGKBCleanupReady(ctx, kb.ID); err != nil || !ready {
		t.Fatalf("cleanup after abort ready=%v err=%v", ready, err)
	}
	if active, ok, err := st.ActivateRAGKBProvisioning(ctx, *fence); err != nil || ok || active != nil {
		t.Fatalf("stale activation=%+v ok=%v err=%v", active, ok, err)
	}
	if err := st.DeleteRAGKB(ctx, kb.ID); err != nil {
		t.Fatalf("delete quiesced KB: %v", err)
	}
}

func TestRAGKBProvisioningActivationIsFencedAndAtomic(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	ensureRAGLifecycleUser(t, st, "u_kb_activate", "active")
	ctx := context.Background()
	kb := testRAGProvisioningKB("u_kb_activate", "kb_provision_activate")
	fence, err := st.BeginRAGKBProvisioning(ctx, kb, "activate-worker", time.Minute, 2)
	if err != nil {
		t.Fatal(err)
	}
	active, ok, err := st.ActivateRAGKBProvisioning(ctx, *fence)
	if err != nil || !ok || active == nil || active.Status != "active" {
		t.Fatalf("activate=%+v ok=%v err=%v", active, ok, err)
	}
	if ready, err := st.IsRAGKBCleanupReady(ctx, kb.ID); err != nil || !ready {
		t.Fatalf("active KB retained provisioning lease ready=%v err=%v", ready, err)
	}
	if marked, ok, err := st.AbortRAGKBProvisioning(ctx, *fence); err != nil || ok || marked != nil {
		t.Fatalf("stale abort changed active KB marked=%+v ok=%v err=%v", marked, ok, err)
	}
	stored, err := st.GetRAGKB(ctx, kb.ID)
	if err != nil || stored.Status != "active" {
		t.Fatalf("stored active KB=%+v err=%v", stored, err)
	}
}

func TestRAGKBExpiredProvisioningBecomesDurableDeletingHandle(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	ensureRAGLifecycleUser(t, st, "u_kb_expired", "active")
	ctx := context.Background()
	kb := testRAGProvisioningKB("u_kb_expired", "kb_provision_expired")
	fence, err := st.BeginRAGKBProvisioning(ctx, kb, "crashed-worker", time.Minute, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, fmt.Sprintf(`UPDATE rag_kbs
		SET provisioning_lease_until=%s WHERE id=%s`, st.ph(1), st.ph(2)),
		time.Now().UTC().Add(-time.Minute), kb.ID); err != nil {
		t.Fatal(err)
	}
	candidates, err := st.ListExpiredRAGKBProvisions(ctx, 10)
	if err != nil || len(candidates) != 1 || candidates[0].KBID != kb.ID {
		t.Fatalf("expired candidates=%+v err=%v", candidates, err)
	}
	marked, ok, err := st.ExpireRAGKBProvisioning(ctx, candidates[0])
	if err != nil || !ok || marked == nil || marked.Status != RAGKBStatusDeleting {
		t.Fatalf("expire=%+v ok=%v err=%v", marked, ok, err)
	}
	if active, ok, err := st.ActivateRAGKBProvisioning(ctx, *fence); err != nil || ok || active != nil {
		t.Fatalf("crashed worker activated after expiry active=%+v ok=%v err=%v", active, ok, err)
	}
	if err := st.DeleteRAGKB(ctx, kb.ID); err != nil {
		t.Fatal(err)
	}
}
