package store

import (
	"context"
	"fmt"
	"strings"
)

// SQL triggers maintain a coalescing transactional outbox, including raw SQL,
// cascades where supported, and older writer processes. A rollback rolls back
// both the business write and revision. Tombstones survive source deletion.
func (d *DBStore) migrateContextCache(ctx context.Context) error {
	ddl := `CREATE TABLE IF NOT EXISTS context_cache_changes (
 kind VARCHAR(32) NOT NULL, s1 VARCHAR(191) NOT NULL, s2 VARCHAR(191) NOT NULL, s3 VARCHAR(191) NOT NULL,
 revision BIGINT NOT NULL DEFAULT 1, applied BIGINT NOT NULL DEFAULT 0, dirty BOOLEAN NOT NULL DEFAULT TRUE,
 PRIMARY KEY(kind,s1,s2,s3))`
	if d.dialect == mysqlDialect {
		ddl += " ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin"
	}
	if _, err := d.db.ExecContext(ctx, ddl); err != nil {
		return err
	}
	if d.dialect == mysqlDialect {
		var n int
		if err := d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='context_cache_changes' AND INDEX_NAME='context_cache_dirty'").Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := d.db.ExecContext(ctx, "CREATE INDEX context_cache_dirty ON context_cache_changes(dirty)"); err != nil {
				return err
			}
		}
	} else {
		if _, err := d.db.ExecContext(ctx, "CREATE INDEX IF NOT EXISTS context_cache_dirty ON context_cache_changes(dirty)"); err != nil {
			return err
		}
	}

	publicationDDL := `CREATE TABLE IF NOT EXISTS skill_publications (owner VARCHAR(191) NOT NULL, slug VARCHAR(191) NOT NULL, manifest TEXT NOT NULL, deleted BOOLEAN NOT NULL DEFAULT FALSE, PRIMARY KEY(owner,slug))`
	if d.dialect == mysqlDialect {
		publicationDDL = strings.Replace(publicationDDL, "manifest TEXT", "manifest MEDIUMTEXT", 1) + " ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin"
	}
	if _, err := d.db.ExecContext(ctx, publicationDDL); err != nil {
		return err
	}

	sources := []struct{ table, kind, a, b, c string }{
		{"sessions", "session", "user_id", "agent_id", "session_key"},
		{"agent_files", "file", "agent_id", "user_id", "filename"},
		{"skill_usage", "skillstate", "agent_id", "", ""},
		{"agents", "agent", "id", "", ""},
		{"skill_publications", "skillcatalog", "owner", "", ""},
	}
	for _, s := range sources {
		for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
			ref := "NEW"
			if event == "DELETE" {
				ref = "OLD"
			}
			expr := func(col string) string {
				if col == "" {
					return "''"
				}
				return ref + "." + col
			}
			vals := fmt.Sprintf("'%s',%s,%s,%s", s.kind, expr(s.a), expr(s.b), expr(s.c))
			name := "ctxcache_" + s.table + "_" + strings.ToLower(event)
			stmt := "INSERT INTO context_cache_changes(kind,s1,s2,s3,revision,applied) VALUES (" + vals + ",1,0)"
			if d.dialect == mysqlDialect {
				stmt += " ON DUPLICATE KEY UPDATE revision=revision+1,dirty=TRUE"
				var count int
				if err := d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA=DATABASE() AND TRIGGER_NAME=?", name).Scan(&count); err != nil {
					return err
				}
				if count > 0 {
					continue
				}
				stmt = "CREATE TRIGGER " + name + " AFTER " + event + " ON " + s.table + " FOR EACH ROW " + stmt
			} else {
				stmt += " ON CONFLICT(kind,s1,s2,s3) DO UPDATE SET revision=context_cache_changes.revision+1,dirty=TRUE"
				if d.dialect == "postgres" {
					fn := "CREATE OR REPLACE FUNCTION " + name + "_fn() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN " + stmt + "; RETURN " + ref + "; END; $$"
					if _, err := d.db.ExecContext(ctx, fn); err != nil {
						return err
					}
					var count int
					if err := d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM pg_trigger WHERE tgname=$1", name).Scan(&count); err != nil {
						return err
					}
					if count > 0 {
						continue
					}
					stmt = "CREATE TRIGGER " + name + " AFTER " + event + " ON " + s.table + " FOR EACH ROW EXECUTE FUNCTION " + name + "_fn()"
				} else {
					stmt = "CREATE TRIGGER IF NOT EXISTS " + name + " AFTER " + event + " ON " + s.table + " BEGIN " + stmt + "; END"
				}
			}
			if _, err := d.db.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
	}
	return nil
}
