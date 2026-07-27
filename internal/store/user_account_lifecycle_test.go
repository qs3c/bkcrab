package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func openUserLifecycleDialectStores(t *testing.T, dialect, dsn string) (*DBStore, *DBStore) {
	t.Helper()
	primary, err := NewDBStore(dialect, dsn)
	if err != nil {
		t.Fatalf("open primary %s store: %v", dialect, err)
	}
	t.Cleanup(func() { _ = primary.Close() })
	if err := primary.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate %s store: %v", dialect, err)
	}

	competitor, err := NewDBStore(dialect, dsn)
	if err != nil {
		t.Fatalf("open competing %s store: %v", dialect, err)
	}
	t.Cleanup(func() { _ = competitor.Close() })
	return primary, competitor
}

func createUserLifecycleAdminPair(t *testing.T, db *DBStore, suffix string) (string, string) {
	t.Helper()
	ids := [2]string{"u_admin_a_" + suffix, "u_admin_b_" + suffix}
	for _, id := range ids {
		if err := db.CreateUser(context.Background(), &UserRecord{
			ID: id, Username: id, Email: id + "@example.test", Role: "super_admin",
			Status: "active", AgentQuota: -1,
		}); err != nil {
			t.Fatalf("create lifecycle admin %s: %v", id, err)
		}
	}
	t.Cleanup(func() {
		_, _ = db.db.ExecContext(context.Background(), fmt.Sprintf(
			`DELETE FROM users WHERE id IN (%s, %s)`, db.ph(1), db.ph(2)), ids[0], ids[1])
	})
	return ids[0], ids[1]
}

func assertOneLifecycleMutationWon(t *testing.T, errorsByMutation []error) {
	t.Helper()
	var succeeded, rejected int
	for _, err := range errorsByMutation {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrLastActiveSuperAdmin):
			rejected++
		default:
			t.Fatalf("concurrent mutation returned unexpected error: %v", err)
		}
	}
	if succeeded != 1 || rejected != 1 {
		t.Fatalf("concurrent results: success=%d last-admin=%d, want 1 and 1; errors=%v",
			succeeded, rejected, errorsByMutation)
	}
}

func runConcurrentUserLifecycleMutations(
	t *testing.T,
	first, second func(context.Context) error,
) []error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, mutation := range []func(context.Context) error{first, second} {
		mutation := mutation
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- mutation(ctx)
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	errorsByMutation := make([]error, 0, 2)
	for err := range results {
		errorsByMutation = append(errorsByMutation, err)
	}
	return errorsByMutation
}

func assertOneFixtureAdminActive(t *testing.T, db *DBStore, firstID, secondID string) {
	t.Helper()
	var active int
	err := db.db.QueryRowContext(context.Background(), fmt.Sprintf(`SELECT COUNT(*) FROM users
		WHERE id IN (%s, %s) AND LOWER(role)='super_admin' AND LOWER(status)='active'`,
		db.ph(1), db.ph(2)), firstID, secondID).Scan(&active)
	if err != nil {
		t.Fatalf("count fixture admins: %v", err)
	}
	if active != 1 {
		t.Fatalf("active fixture super-admins = %d, want 1", active)
	}
}

func TestUserLifecycleLastActiveSuperAdminDialectConcurrency(t *testing.T) {
	tests := []struct {
		name, dialect, dsn string
	}{
		{
			name: "sqlite", dialect: "sqlite",
			dsn: "file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "user-lifecycle.db")) +
				"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)",
		},
		{name: "postgres", dialect: "postgres", dsn: os.Getenv("BKCRAB_TEST_POSTGRES_DSN")},
		{name: "mysql", dialect: "mysql", dsn: os.Getenv("BKCRAB_TEST_MYSQL_DSN")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.dsn == "" {
				t.Skip("BKCRAB_TEST_" + strings.ToUpper(test.name) + "_DSN is not set")
			}
			primary, competitor := openUserLifecycleDialectStores(t, test.dialect, test.dsn)

			// The invariant is installation-wide. External dialect tests therefore
			// require their usual isolated CI database instead of deleting or
			// modifying administrators that belong to another test installation.
			var existing int
			if err := primary.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM users
				WHERE LOWER(role)='super_admin' AND LOWER(status)='active'`).Scan(&existing); err != nil {
				t.Fatalf("count pre-existing admins: %v", err)
			}
			if existing != 0 {
				t.Skipf("dialect database is not isolated: found %d active super-admin(s)", existing)
			}

			t.Run("two deletes", func(t *testing.T) {
				suffix := fmt.Sprintf("%s_%d", test.name, time.Now().UnixNano())
				firstID, secondID := createUserLifecycleAdminPair(t, primary, suffix)
				errs := runConcurrentUserLifecycleMutations(t,
					func(ctx context.Context) error {
						_, err := primary.MarkUserDeleting(ctx, firstID)
						return err
					},
					func(ctx context.Context) error {
						_, err := competitor.MarkUserDeleting(ctx, secondID)
						return err
					})
				assertOneLifecycleMutationWon(t, errs)
				assertOneFixtureAdminActive(t, primary, firstID, secondID)
			})

			t.Run("disable races demotion", func(t *testing.T) {
				suffix := fmt.Sprintf("%s_%d", test.name, time.Now().UnixNano())
				firstID, secondID := createUserLifecycleAdminPair(t, primary, suffix)
				first, err := primary.GetUser(context.Background(), firstID)
				if err != nil {
					t.Fatal(err)
				}
				second, err := competitor.GetUser(context.Background(), secondID)
				if err != nil {
					t.Fatal(err)
				}
				first.Status = "disabled"
				second.Role = "user"

				errs := runConcurrentUserLifecycleMutations(t,
					func(ctx context.Context) error {
						return primary.UpdateUser(ctx, first)
					},
					func(ctx context.Context) error {
						return competitor.UpdateUser(ctx, second)
					})
				assertOneLifecycleMutationWon(t, errs)
				assertOneFixtureAdminActive(t, primary, firstID, secondID)
			})

			t.Run("delete races disable", func(t *testing.T) {
				suffix := fmt.Sprintf("%s_%d", test.name, time.Now().UnixNano())
				firstID, secondID := createUserLifecycleAdminPair(t, primary, suffix)
				second, err := competitor.GetUser(context.Background(), secondID)
				if err != nil {
					t.Fatal(err)
				}
				second.Status = "disabled"

				errs := runConcurrentUserLifecycleMutations(t,
					func(ctx context.Context) error {
						_, err := primary.MarkUserDeleting(ctx, firstID)
						return err
					},
					func(ctx context.Context) error {
						return competitor.UpdateUser(ctx, second)
					})
				assertOneLifecycleMutationWon(t, errs)
				assertOneFixtureAdminActive(t, primary, firstID, secondID)
			})
		})
	}
}

func TestUserLifecycleRejectsSequentialLastAdminMutations(t *testing.T) {
	db := newTestSQLite(t)
	ctx := context.Background()
	admin := &UserRecord{
		ID: "u_only_admin", Username: "only-admin", Email: "only-admin@example.test",
		Role: "super_admin", Status: "active", AgentQuota: -1,
	}
	if err := db.CreateUser(ctx, admin); err != nil {
		t.Fatal(err)
	}

	if _, err := db.MarkUserDeleting(ctx, admin.ID); !errors.Is(err, ErrLastActiveSuperAdmin) {
		t.Fatalf("mark last admin deleting error = %v, want ErrLastActiveSuperAdmin", err)
	}
	admin, err := db.GetUser(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	admin.Status = "disabled"
	if err := db.UpdateUser(ctx, admin); !errors.Is(err, ErrLastActiveSuperAdmin) {
		t.Fatalf("disable last admin error = %v, want ErrLastActiveSuperAdmin", err)
	}

	current, err := db.GetUser(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !activeSuperAdmin(current) {
		t.Fatalf("rejected mutations changed the last admin: role=%q status=%q", current.Role, current.Status)
	}
}
