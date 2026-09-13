package skills

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/qs3c/bkcrab/internal/contextcache"
	"github.com/qs3c/bkcrab/internal/workspace"
	"github.com/redis/go-redis/v9"
)

type countedSkillStore struct {
	workspace.Store
	lists, gets int
}

func (s *countedSkillStore) List(ctx context.Context, a, p, c string) ([]workspace.ObjectInfo, error) {
	s.lists++
	return s.Store.List(ctx, a, p, c)
}
func (s *countedSkillStore) Get(ctx context.Context, a, p, c, k string) (io.ReadCloser, error) {
	s.gets++
	return s.Store.Get(ctx, a, p, c, k)
}

func TestSkillHydrationEqualSizeUpdateAndLastDeletion(t *testing.T) {
	for _, cached := range []bool{false, true} {
		name := "source"
		if cached {
			name = "redis"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			underlying := &countedSkillStore{Store: workspace.NewLocalFS(t.TempDir())}
			var ws workspace.Store = underlying
			if cached {
				url := os.Getenv("BKCRAB_TEST_CONTEXT_REDIS_URL")
				if url == "" {
					t.Skip("set BKCRAB_TEST_CONTEXT_REDIS_URL")
				}
				opts, err := redis.ParseURL(url)
				if err != nil {
					t.Fatal(err)
				}
				c := contextcache.New(redis.NewClient(opts), uuid.NewString())
				t.Cleanup(func() { c.Close() })
				ws = workspace.WithCache(ws, c, "skill test")
			}
			m, root := newTestManager(t)
			m.SetObjectStore(ws, "agent")
			if err := m.Create("test-skill", validSkill); err != nil {
				t.Fatal(err)
			}
			consumer := t.TempDir()
			hydrate := func() {
				t.Helper()
				if err := HydrateSkillsDown(ctx, ws, "agent", consumer); err != nil {
					t.Fatal(err)
				}
			}
			hydrate()
			firstLists, firstGets := underlying.lists, underlying.gets
			hydrate()
			if underlying.gets != firstGets {
				t.Fatal("unchanged skill downloaded again")
			}
			if cached && underlying.lists != firstLists {
				t.Fatal("warm hydration repeated object-store LIST")
			}
			updated := strings.Replace(validSkill, "first", "third", 1)
			if len(updated) != len(validSkill) {
				t.Fatal("fixture length changed")
			}
			if err := m.Update("test-skill", updated); err != nil {
				t.Fatal(err)
			}
			hydrate()
			got, err := os.ReadFile(filepath.Join(consumer, "test-skill", "SKILL.md"))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != updated {
				t.Fatal("equal-size update remained stale")
			}
			if _, err := os.Stat(filepath.Join(root, "test-skill", "SKILL.md")); err != nil {
				t.Fatal(err)
			}
			// An empty remote bucket removes only previously hydrated skills.
			if err := os.MkdirAll(filepath.Join(consumer, "local-only"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := m.Delete("test-skill"); err != nil {
				t.Fatal(err)
			}
			hydrate()
			if _, err := os.Stat(filepath.Join(consumer, "test-skill")); !os.IsNotExist(err) {
				t.Fatalf("deleted skill revived: %v", err)
			}
			if _, err := os.Stat(filepath.Join(consumer, "local-only")); err != nil {
				t.Fatal("local-only skill removed")
			}
		})
	}
}
