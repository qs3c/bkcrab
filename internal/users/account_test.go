package users

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/store"
)

type testRAGUserCleaner struct {
	calls []string
	fn    func(context.Context, string) error
}

func (c *testRAGUserCleaner) CleanupRAGUser(ctx context.Context, userID string) error {
	c.calls = append(c.calls, userID)
	if c.fn != nil {
		return c.fn(ctx, userID)
	}
	return nil
}

func newAccountsTestStore(t *testing.T) *store.DBStore {
	t.Helper()
	dsn := "file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "accounts.db")) +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	st, err := store.NewDBStore("sqlite", dsn)
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}
	if err := st.Migrate(context.Background()); err != nil {
		_ = st.Close()
		t.Fatalf("migrate sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func createAccountsTestUser(t *testing.T, st store.Store, id, role, status string) {
	t.Helper()
	err := st.CreateUser(context.Background(), &store.UserRecord{
		ID:           id,
		Username:     id,
		Email:        id + "@example.test",
		PasswordHash: "test-only",
		Role:         role,
		Status:       status,
		AgentQuota:   -1,
	})
	if err != nil {
		t.Fatalf("create user %s: %v", id, err)
	}
}

func createAccountsTestKB(t *testing.T, st store.Store, userID, kbID string) {
	t.Helper()
	err := st.CreateRAGKB(context.Background(), &store.RAGKBRecord{
		ID:            kbID,
		UserID:        userID,
		Name:          "test kb",
		EmbedProvider: "system",
		EmbedModel:    "test-embedding",
		EmbedDims:     4,
		ChunkSize:     512,
		ChunkOverlap:  64,
		ParseMode:     store.RAGParseModeStandard,
		Status:        "ACTIVE",
	})
	if err != nil {
		t.Fatalf("create RAG KB %s: %v", kbID, err)
	}
}

func TestAccountsDeleteRAGUserRequiresCleanerAfterTombstone(t *testing.T) {
	ctx := context.Background()
	st := newAccountsTestStore(t)
	createAccountsTestUser(t, st, "u_target", RoleSuperAdmin, StatusActive)
	createAccountsTestUser(t, st, "u_remaining_admin", RoleSuperAdmin, StatusActive)
	createAccountsTestKB(t, st, "u_target", "kb_target")
	accounts, err := NewAccounts(st)
	if err != nil {
		t.Fatal(err)
	}

	err = accounts.Delete(ctx, "u_target")
	if !errors.Is(err, ErrRAGUserCleanerRequired) {
		t.Fatalf("Delete error = %v, want ErrRAGUserCleanerRequired", err)
	}
	user, err := st.GetUser(ctx, "u_target")
	if err != nil {
		t.Fatalf("deleting user should remain durable: %v", err)
	}
	if user.Status != StatusDeleting {
		t.Fatalf("user status = %q, want %q", user.Status, StatusDeleting)
	}
	if _, err := st.GetRAGKB(ctx, "kb_target"); err != nil {
		t.Fatalf("RAG data should remain for a later cleaner retry: %v", err)
	}
}

func TestAccountsDeleteProvisioningRAGUserRequiresCleaner(t *testing.T) {
	ctx := context.Background()
	st := newAccountsTestStore(t)
	createAccountsTestUser(t, st, "u_provisioning", RoleUser, StatusActive)
	if _, err := st.BeginRAGKBProvisioning(ctx, &store.RAGKBRecord{
		ID: "kb_provisioning", UserID: "u_provisioning", Name: "provisioning",
		EmbedProvider: "system", EmbedModel: "test-embedding", EmbedDims: 4,
		ChunkSize: 512, ChunkOverlap: 64, ParseMode: store.RAGParseModeStandard,
	}, "account-test-worker", time.Minute, 20); err != nil {
		t.Fatal(err)
	}
	accounts, err := NewAccounts(st)
	if err != nil {
		t.Fatal(err)
	}

	err = accounts.Delete(ctx, "u_provisioning")
	if !errors.Is(err, ErrRAGUserCleanerRequired) {
		t.Fatalf("Delete error=%v, want cleaner requirement", err)
	}
	user, getErr := st.GetUser(ctx, "u_provisioning")
	if getErr != nil || user.Status != StatusDeleting {
		t.Fatalf("provisioning owner tombstone=%+v err=%v", user, getErr)
	}
	if kb, getErr := st.GetRAGKB(ctx, "kb_provisioning"); getErr != nil ||
		kb.Status != store.RAGKBStatusProvisioning {
		t.Fatalf("provisioning cleanup handle=%+v err=%v", kb, getErr)
	}
}

func TestAccountsDeleteRetriesCleanerAndHardDeletesOnlyAfterSuccess(t *testing.T) {
	ctx := context.Background()
	st := newAccountsTestStore(t)
	createAccountsTestUser(t, st, "u_target", RoleSuperAdmin, StatusActive)
	createAccountsTestUser(t, st, "u_remaining_admin", RoleSuperAdmin, StatusActive)
	createAccountsTestKB(t, st, "u_target", "kb_target")
	accounts, err := NewAccounts(st)
	if err != nil {
		t.Fatal(err)
	}

	cleanupErr := errors.New("temporary vector cleanup failure")
	observedDeleting := false
	cleaner := &testRAGUserCleaner{fn: func(ctx context.Context, userID string) error {
		user, getErr := st.GetUser(ctx, userID)
		observedDeleting = getErr == nil && user.Status == StatusDeleting
		return cleanupErr
	}}
	accounts.SetRAGUserCleaner(cleaner)

	firstErr := accounts.Delete(ctx, "u_target")
	if !errors.Is(firstErr, cleanupErr) || !errors.Is(firstErr, ErrRAGUserCleanupIncomplete) {
		t.Fatalf("first Delete error = %v, want cleanup cause and safe sentinel", firstErr)
	}
	if firstErr.Error() != ErrRAGUserCleanupIncomplete.Error() {
		t.Fatalf("cleanup error leaked backend detail: %q", firstErr)
	}
	if !observedDeleting {
		t.Fatal("cleaner ran before the durable user deleting tombstone was visible")
	}
	user, err := st.GetUser(ctx, "u_target")
	if err != nil {
		t.Fatalf("failed cleanup must preserve user row: %v", err)
	}
	if user.Status != StatusDeleting {
		t.Fatalf("user status after cleanup failure = %q, want %q", user.Status, StatusDeleting)
	}
	if _, err := accounts.Update(ctx, "u_target", "", "", StatusActive, nil); err == nil {
		t.Fatal("deleting user was reactivated while durable cleanup was incomplete")
	}

	cleaner.fn = func(ctx context.Context, _ string) error {
		if _, err := st.MarkRAGKBDeleting(ctx, "kb_target"); err != nil {
			return err
		}
		return st.DeleteRAGKB(ctx, "kb_target")
	}
	if err := accounts.Delete(ctx, "u_target"); err != nil {
		t.Fatalf("retry Delete: %v", err)
	}
	if len(cleaner.calls) != 2 {
		t.Fatalf("cleaner calls = %v, want two idempotent attempts", cleaner.calls)
	}
	if _, err := st.GetUser(ctx, "u_target"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("user should be hard-deleted after successful cleanup, got %v", err)
	}
}

func TestAccountsDeleteWithoutCleanerAllowsUserWithNoRAGData(t *testing.T) {
	ctx := context.Background()
	st := newAccountsTestStore(t)
	createAccountsTestUser(t, st, "u_target", RoleUser, StatusActive)
	accounts, err := NewAccounts(st)
	if err != nil {
		t.Fatal(err)
	}

	if err := accounts.Delete(ctx, "u_target"); err != nil {
		t.Fatalf("Delete user without RAG data: %v", err)
	}
	if _, err := st.GetUser(ctx, "u_target"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("user should be deleted, got %v", err)
	}
}

func TestAccountsDeletePreservesLastActiveSuperAdminGuard(t *testing.T) {
	ctx := context.Background()
	st := newAccountsTestStore(t)
	createAccountsTestUser(t, st, "u_admin", RoleSuperAdmin, StatusActive)
	createAccountsTestKB(t, st, "u_admin", "kb_admin")
	accounts, err := NewAccounts(st)
	if err != nil {
		t.Fatal(err)
	}
	cleaner := &testRAGUserCleaner{}
	accounts.SetRAGUserCleaner(cleaner)

	err = accounts.Delete(ctx, "u_admin")
	if err == nil || err.Error() != "users.Delete: refusing to remove the last active super_admin" {
		t.Fatalf("Delete error = %v, want last-admin guard", err)
	}
	if !errors.Is(err, store.ErrLastActiveSuperAdmin) {
		t.Fatalf("Delete error lost store last-admin sentinel: %v", err)
	}
	user, getErr := st.GetUser(ctx, "u_admin")
	if getErr != nil {
		t.Fatalf("guarded admin should remain: %v", getErr)
	}
	if user.Status != StatusActive {
		t.Fatalf("guard must run before tombstone, status = %q", user.Status)
	}
	if len(cleaner.calls) != 0 {
		t.Fatalf("cleaner called despite last-admin guard: %v", cleaner.calls)
	}
}

func TestAccountsUpdatePreservesLastActiveSuperAdminGuard(t *testing.T) {
	tests := []struct {
		name, role, status string
	}{
		{name: "demotion", role: RoleUser},
		{name: "disable", status: StatusDisabled},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			st := newAccountsTestStore(t)
			createAccountsTestUser(t, st, "u_admin", RoleSuperAdmin, StatusActive)
			accounts, err := NewAccounts(st)
			if err != nil {
				t.Fatal(err)
			}

			_, err = accounts.Update(ctx, "u_admin", "", test.role, test.status, nil)
			if err == nil || err.Error() != "users.Update: refusing to remove the last active super_admin" {
				t.Fatalf("Update error = %v, want last-admin guard", err)
			}
			if !errors.Is(err, store.ErrLastActiveSuperAdmin) {
				t.Fatalf("Update error lost store last-admin sentinel: %v", err)
			}

			current, getErr := st.GetUser(ctx, "u_admin")
			if getErr != nil {
				t.Fatal(getErr)
			}
			if current.Role != RoleSuperAdmin || current.Status != StatusActive {
				t.Fatalf("guarded admin changed: role=%q status=%q", current.Role, current.Status)
			}
		})
	}
}
