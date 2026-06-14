package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// New 创建一个Store实例。MySQL是运行时默认选项，需要显式提供
// DSN。PostgreSQL和SQLite仅在明确选择时可用；
// 不会自动回退到SQLite。
func New(cfg *StorageConfig, homeDir string) (Store, error) {
	if cfg == nil || cfg.Type == "" {
		cfg = &StorageConfig{Type: StorageMySQL, AutoMigrate: true}
	}
	switch cfg.Type {
	case StorageMySQL, StoragePostgres, StorageSQLite:
		dsn := cfg.DSN
		if cfg.Type == StorageMySQL && dsn == "" {
			return nil, errors.New("mysql storage requires BKCLAW_STORAGE_DSN; SQLite fallback is disabled")
		}
		if cfg.Type == StorageSQLite && dsn == "" {
			if err := os.MkdirAll(homeDir, 0o755); err != nil {
				return nil, fmt.Errorf("create %s: %w", homeDir, err)
			}
			// modernc.org/sqlite 从 `_pragma=` 查询参数读取 PRAGMA 设置；
			// 旧的 `_journal=WAL` 形式被静默忽略，因此文件运行在默认的回滚日志
			// 模式下，任何写入者都会阻塞所有其他连接。在高负载下
			//（cron 调度器触发的同时有 web 流量进入），我们看到了
			// "database is locked (SQLITE_BUSY)"——通过启用 WAL
			//（并发读取 + 一个写入者）和 5 秒 busy_timeout 修复了这个问题，
			// 这样竞争的写入者会等待而不是直接报错。
			dsn = "file:" + filepath.Join(homeDir, "bkclaw.db") +
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
		return nil, fmt.Errorf("unsupported storage type: %s (supported: mysql, postgres, sqlite)", cfg.Type)
	}
}

// maskDSN 遮蔽 DSN 字符串中的密码，用于日志记录。
func maskDSN(dsn string) string {
	if len(dsn) > 20 {
		return dsn[:10] + "***" + dsn[len(dsn)-5:]
	}
	return "***"
}
