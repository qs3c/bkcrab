package store

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// New creates a Store. The database is mandatory; sqlite is the default
// for zero-config installs and stores its file under ~/.fastclaw/fastclaw.db.
// Postgres is the production target — pass a DSN.
func New(cfg *StorageConfig, homeDir string) (Store, error) {
	if cfg == nil || cfg.Type == "" {
		cfg = &StorageConfig{Type: StorageSQLite, AutoMigrate: true}
	}
	switch cfg.Type {
	case StoragePostgres, StorageSQLite:
		dsn := cfg.DSN
		if cfg.Type == StorageSQLite && dsn == "" {
			if err := os.MkdirAll(homeDir, 0o755); err != nil {
				return nil, fmt.Errorf("create %s: %w", homeDir, err)
			}
			// modernc.org/sqlite reads PRAGMA settings from `_pragma=`
			// query params; the legacy `_journal=WAL` form was silently
			// ignored, so the file was running in default rollback-journal
			// mode where any writer blocks every other connection. Under
			// load (cron scheduler firing while web traffic comes in) we
			// saw "database is locked (SQLITE_BUSY)" — fixed by enabling
			// WAL (concurrent reads + one writer) and a 5s busy_timeout
			// so contended writers wait instead of erroring out.
			dsn = "file:" + filepath.Join(homeDir, "fastclaw.db") +
				"?_pragma=journal_mode(WAL)" +
				"&_pragma=busy_timeout(5000)" +
				"&_pragma=synchronous(NORMAL)" +
				"&_pragma=foreign_keys(1)"
		}
		slog.Info("using database storage", "dialect", cfg.Type, "dsn", maskDSN(dsn))
		db, err := NewDBStore(string(cfg.Type), dsn)
		if err != nil {
			return nil, fmt.Errorf("open database: %w", err)
		}
		if cfg.AutoMigrate {
			slog.Info("running database migrations")
			if err := db.Migrate(context.Background()); err != nil {
				db.Close()
				return nil, fmt.Errorf("migrate: %w", err)
			}
		}
		return db, nil
	default:
		return nil, fmt.Errorf("unsupported storage type: %s (only sqlite/postgres supported)", cfg.Type)
	}
}

// maskDSN masks passwords in DSN strings for logging.
func maskDSN(dsn string) string {
	if len(dsn) > 20 {
		return dsn[:10] + "***" + dsn[len(dsn)-5:]
	}
	return "***"
}
