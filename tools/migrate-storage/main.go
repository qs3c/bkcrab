package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/qs3c/bkclaw/internal/store"
)

type column struct {
	name     string
	dataType string
}

func main() {
	log.SetFlags(0)

	var sqlitePath string
	var postgresDSN string
	var replace bool
	flag.StringVar(&sqlitePath, "sqlite", "", "path to the source SQLite database")
	flag.StringVar(&postgresDSN, "postgres", "", "destination PostgreSQL DSN")
	flag.BoolVar(&replace, "replace", false, "truncate destination tables before copying")
	flag.Parse()

	if sqlitePath == "" || postgresDSN == "" {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(context.Background(), sqlitePath, postgresDSN, replace); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, sqlitePath, postgresDSN string, replace bool) error {
	absPath, err := filepath.Abs(sqlitePath)
	if err != nil {
		return fmt.Errorf("resolve SQLite path: %w", err)
	}
	if _, err := os.Stat(absPath); err != nil {
		return fmt.Errorf("open SQLite source: %w", err)
	}

	src, err := sql.Open("sqlite", "file:"+filepath.ToSlash(absPath)+"?mode=ro")
	if err != nil {
		return fmt.Errorf("open SQLite source: %w", err)
	}
	defer src.Close()
	if err := src.PingContext(ctx); err != nil {
		return fmt.Errorf("ping SQLite source: %w", err)
	}

	dstStore, err := store.NewDBStore("postgres", postgresDSN)
	if err != nil {
		return fmt.Errorf("open PostgreSQL destination: %w", err)
	}
	defer dstStore.Close()
	if err := dstStore.Migrate(ctx); err != nil {
		return fmt.Errorf("create PostgreSQL schema: %w", err)
	}
	dst := dstStore.DB()

	tables, err := sharedTables(ctx, src, dst)
	if err != nil {
		return err
	}
	if len(tables) == 0 {
		return errors.New("no shared application tables found")
	}

	if !replace {
		for _, table := range tables {
			n, err := countRows(ctx, dst, table)
			if err != nil {
				return err
			}
			if n != 0 {
				return fmt.Errorf("destination table %s is not empty; rerun with --replace to overwrite", table)
			}
		}
	}

	tx, err := dst.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin PostgreSQL transaction: %w", err)
	}
	defer tx.Rollback()

	if replace {
		quoted := make([]string, 0, len(tables))
		for _, table := range tables {
			quoted = append(quoted, quoteIdent(table))
		}
		if _, err := tx.ExecContext(ctx, "TRUNCATE TABLE "+strings.Join(quoted, ", ")+" CASCADE"); err != nil {
			return fmt.Errorf("truncate destination: %w", err)
		}
	}

	sourceCounts := make(map[string]int64, len(tables))
	for _, table := range tables {
		n, err := copyTable(ctx, src, tx, table)
		if err != nil {
			return err
		}
		sourceCounts[table] = n
		log.Printf("copied %-24s %d rows", table, n)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit PostgreSQL migration: %w", err)
	}

	for _, table := range tables {
		got, err := countRows(ctx, dst, table)
		if err != nil {
			return err
		}
		if got != sourceCounts[table] {
			return fmt.Errorf("row-count mismatch for %s: source=%d destination=%d", table, sourceCounts[table], got)
		}
	}
	log.Printf("migration complete: %d tables verified", len(tables))
	return nil
}

func sharedTables(ctx context.Context, src, dst *sql.DB) ([]string, error) {
	rows, err := dst.QueryContext(ctx, `
		SELECT table_name
		FROM information_schema.tables
		WHERE table_schema = 'public' AND table_type = 'BASE TABLE'
		ORDER BY table_name`)
	if err != nil {
		return nil, fmt.Errorf("list PostgreSQL tables: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return nil, err
		}
		var exists int
		if err := src.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&exists); err != nil {
			return nil, fmt.Errorf("check SQLite table %s: %w", table, err)
		}
		if exists > 0 {
			out = append(out, table)
		}
	}
	return out, rows.Err()
}

func copyTable(ctx context.Context, src *sql.DB, dst *sql.Tx, table string) (int64, error) {
	targetColumns, err := postgresColumns(ctx, dst, table)
	if err != nil {
		return 0, err
	}
	sourceColumns, err := sqliteColumns(ctx, src, table)
	if err != nil {
		return 0, err
	}

	var columns []column
	for _, target := range targetColumns {
		if sourceColumns[target.name] {
			columns = append(columns, target)
		}
	}
	if len(columns) == 0 {
		return 0, fmt.Errorf("table %s has no shared columns", table)
	}

	names := make([]string, len(columns))
	placeholders := make([]string, len(columns))
	for i, col := range columns {
		names[i] = quoteIdent(col.name)
		placeholders[i] = "$" + strconv.Itoa(i+1)
	}

	rows, err := src.QueryContext(ctx,
		"SELECT "+strings.Join(names, ", ")+" FROM "+quoteIdent(table))
	if err != nil {
		return 0, fmt.Errorf("read SQLite table %s: %w", table, err)
	}
	defer rows.Close()

	insertSQL := "INSERT INTO " + quoteIdent(table) + " (" + strings.Join(names, ", ") +
		") VALUES (" + strings.Join(placeholders, ", ") + ")"
	stmt, err := dst.PrepareContext(ctx, insertSQL)
	if err != nil {
		return 0, fmt.Errorf("prepare PostgreSQL table %s: %w", table, err)
	}
	defer stmt.Close()

	var count int64
	for rows.Next() {
		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return 0, fmt.Errorf("scan SQLite table %s: %w", table, err)
		}
		for i, col := range columns {
			values[i], err = convertValue(values[i], col.dataType)
			if err != nil {
				return 0, fmt.Errorf("convert %s.%s: %w", table, col.name, err)
			}
		}
		if _, err := stmt.ExecContext(ctx, values...); err != nil {
			return 0, fmt.Errorf("insert PostgreSQL table %s row %d: %w", table, count+1, err)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate SQLite table %s: %w", table, err)
	}
	return count, nil
}

func postgresColumns(ctx context.Context, tx *sql.Tx, table string) ([]column, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT column_name, data_type
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = $1
		ORDER BY ordinal_position`, table)
	if err != nil {
		return nil, fmt.Errorf("list PostgreSQL columns for %s: %w", table, err)
	}
	defer rows.Close()

	var out []column
	for rows.Next() {
		var col column
		if err := rows.Scan(&col.name, &col.dataType); err != nil {
			return nil, err
		}
		out = append(out, col)
	}
	return out, rows.Err()
}

func sqliteColumns(ctx context.Context, db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+quoteIdent(table)+")")
	if err != nil {
		return nil, fmt.Errorf("list SQLite columns for %s: %w", table, err)
	}
	defer rows.Close()

	out := make(map[string]bool)
	for rows.Next() {
		var cid, notNull, pk int
		var name, dataType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &pk); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

func convertValue(value any, dataType string) (any, error) {
	if value == nil {
		return nil, nil
	}
	switch dataType {
	case "boolean":
		switch v := value.(type) {
		case bool:
			return v, nil
		case int64:
			return v != 0, nil
		case float64:
			return v != 0, nil
		case []byte:
			return parseBool(string(v))
		case string:
			return parseBool(v)
		}
	case "timestamp without time zone", "timestamp with time zone":
		switch v := value.(type) {
		case time.Time:
			return v, nil
		case []byte:
			return parseTime(string(v))
		case string:
			return parseTime(v)
		}
	case "date":
		switch v := value.(type) {
		case time.Time:
			return v, nil
		case []byte:
			return string(v), nil
		}
	case "text", "character varying":
		if v, ok := value.([]byte); ok {
			return string(v), nil
		}
	}
	return value, nil
}

func parseBool(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "t", "yes":
		return true, nil
	case "0", "false", "f", "no", "":
		return false, nil
	default:
		return false, fmt.Errorf("invalid boolean %q", value)
	}
}

func parseTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999 -0700 MST",
		"2006-01-02 15:04:05 -0700 MST",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05",
	} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid timestamp %q", value)
}

func countRows(ctx context.Context, db *sql.DB, table string) (int64, error) {
	var count int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quoteIdent(table)).Scan(&count); err != nil {
		return 0, fmt.Errorf("count table %s: %w", table, err)
	}
	return count, nil
}

func quoteIdent(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}
