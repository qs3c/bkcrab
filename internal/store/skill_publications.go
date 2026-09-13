package store

import (
	"context"
	"encoding/json"
	"fmt"
)

// SkillPublication points to a completely uploaded immutable package. A deleted
// row is retained so an old local directory cannot resurrect the package.
type SkillPublication struct {
	Slug     string          `json:"slug"`
	Manifest json.RawMessage `json:"manifest"`
	Deleted  bool            `json:"deleted"`
}

func (d *DBStore) SaveSkillPublication(ctx context.Context, owner string, p SkillPublication) error {
	defer d.afterContextWrite(ctx, cacheScope{"skillcatalog", owner, "", ""})
	if owner == "" || p.Slug == "" || !json.Valid(p.Manifest) {
		return fmt.Errorf("invalid skill publication")
	}
	query := fmt.Sprintf("INSERT INTO skill_publications(owner,slug,manifest,deleted) VALUES (%s,%s,%s,%s)", d.ph(1), d.ph(2), d.ph(3), d.ph(4))
	if d.dialect == mysqlDialect {
		query += " ON DUPLICATE KEY UPDATE manifest=VALUES(manifest),deleted=VALUES(deleted)"
	} else {
		query += " ON CONFLICT(owner,slug) DO UPDATE SET manifest=excluded.manifest,deleted=excluded.deleted"
	}
	_, err := d.db.ExecContext(ctx, query, owner, p.Slug, string(p.Manifest), p.Deleted)
	return err
}
func (d *DBStore) ListSkillPublications(ctx context.Context, owner string) ([]SkillPublication, error) {
	return cachedRead(d, ctx, cacheScope{"skillcatalog", owner, "", ""}, func() ([]SkillPublication, error) {
		rows, err := d.db.QueryContext(ctx, "SELECT slug,manifest,deleted FROM skill_publications WHERE owner="+d.ph(1)+" ORDER BY slug", owner)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []SkillPublication
		for rows.Next() {
			var p SkillPublication
			var raw string
			if err = rows.Scan(&p.Slug, &raw, &p.Deleted); err != nil {
				return nil, err
			}
			p.Manifest = json.RawMessage(raw)
			out = append(out, p)
		}
		return out, rows.Err()
	})
}

// Published skill pointers remain authoritative when the Redis cache is turned
// off. Disabling acceleration must never revert to stale legacy object paths.
func (d *DBStore) HasSkillPublications(ctx context.Context) (bool, error) {
	exists, err := d.tableExists(ctx, "skill_publications")
	if err != nil || !exists {
		return false, err
	}
	var count int
	err = d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM (SELECT owner FROM skill_publications LIMIT 1) p").Scan(&count)
	return count > 0, err
}

// Import never overwrites a package published concurrently with legacy loading.
func (d *DBStore) ImportSkillPublication(ctx context.Context, owner string, p SkillPublication) error {
	defer d.afterContextWrite(ctx, cacheScope{"skillcatalog", owner, "", ""})
	query := fmt.Sprintf("INSERT INTO skill_publications(owner,slug,manifest,deleted) VALUES (%s,%s,%s,%s)", d.ph(1), d.ph(2), d.ph(3), d.ph(4))
	if d.dialect == mysqlDialect {
		query += " ON DUPLICATE KEY UPDATE owner=owner"
	} else {
		query += " ON CONFLICT(owner,slug) DO NOTHING"
	}
	_, err := d.db.ExecContext(ctx, query, owner, p.Slug, string(p.Manifest), p.Deleted)
	return err
}
