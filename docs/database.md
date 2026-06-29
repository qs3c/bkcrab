# Database

BkCrab uses MySQL by default and does not fall back to SQLite when the MySQL
configuration is missing or unavailable.

Required environment variables:

```bash
BKCRAB_STORAGE_TYPE=mysql
BKCRAB_STORAGE_DSN='bkcrab:password@tcp(mysql.example.com:3306)/bkcrab?parseTime=true&loc=UTC&charset=utf8mb4'
BKCRAB_STORAGE_AUTO_MIGRATE=true
```

`parseTime=true` is enforced by the application. Configure `tls=true` or a
registered MySQL TLS profile for managed production databases.

PostgreSQL and SQLite providers remain available for compatibility and tests,
but they must be selected explicitly. An empty storage type always means
MySQL, and an empty MySQL DSN is a startup error.

To copy a legacy SQLite database into MySQL:

```bash
bkcrab-migrate-storage \
  --sqlite /path/to/bkcrab.db \
  --mysql 'bkcrab:password@tcp(mysql.example.com:3306)/bkcrab?parseTime=true' \
  --replace
```
