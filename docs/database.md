# Database

BkClaw uses MySQL by default and does not fall back to SQLite when the MySQL
configuration is missing or unavailable.

Required environment variables:

```bash
BKCLAW_STORAGE_TYPE=mysql
BKCLAW_STORAGE_DSN='bkclaw:password@tcp(mysql.example.com:3306)/bkclaw?parseTime=true&loc=UTC&charset=utf8mb4'
BKCLAW_STORAGE_AUTO_MIGRATE=true
```

`parseTime=true` is enforced by the application. Configure `tls=true` or a
registered MySQL TLS profile for managed production databases.

PostgreSQL and SQLite providers remain available for compatibility and tests,
but they must be selected explicitly. An empty storage type always means
MySQL, and an empty MySQL DSN is a startup error.

To copy a legacy SQLite database into MySQL:

```bash
bkclaw-migrate-storage \
  --sqlite /path/to/bkclaw.db \
  --mysql 'bkclaw:password@tcp(mysql.example.com:3306)/bkclaw?parseTime=true' \
  --replace
```
