package store

import (
	"strings"
	"testing"

	mysqlDriver "github.com/go-sql-driver/mysql"
)

func TestNewRequiresMySQLDSNByDefault(t *testing.T) {
	_, err := New(nil, t.TempDir())
	if err == nil {
		t.Fatal("expected missing MySQL DSN to fail")
	}
	if !strings.Contains(err.Error(), "SQLite fallback is disabled") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNormalizeMySQLDSNForcesTimeParsing(t *testing.T) {
	dsn, err := normalizeMySQLDSN("user:pass@tcp(localhost:3306)/bkcrab")
	if err != nil {
		t.Fatalf("normalize DSN: %v", err)
	}
	if !strings.Contains(dsn, "parseTime=true") {
		t.Fatalf("parseTime not enabled: %s", dsn)
	}
	cfg, err := mysqlDriver.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse normalized DSN: %v", err)
	}
	if cfg.Loc.String() != "UTC" {
		t.Fatalf("UTC location not enabled: %s", cfg.Loc)
	}
}
