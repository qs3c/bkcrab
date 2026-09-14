#!/bin/bash
# Online operational backup. For a transactionally consistent full restore
# point, quiesce all writers first (see docs/sandbox-reliability.md).
set -euo pipefail
umask 077
root=/srv/bkcrab-backups
mkdir -p "$root"
available=$(df --output=avail -B1 "$root" | tail -1)
(( available >= 10737418240 )) || { echo 'Backup skipped: less than 10 GiB free'; exit 1; }
backup="$root/$(date -u +%Y%m%dT%H%M%SZ)"
mkdir "$backup"
complete=false
trap 'if [ "$complete" != true ]; then rm -f -- "$backup/mysql.sql.gz" "$backup/files.tar.gz"; fi' EXIT
docker exec bkcrab-mysql-1 sh -c 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysqldump -uroot --single-transaction --routines --events --all-databases' |
    gzip -1 > "$backup/mysql.sql.gz"
docker run --rm --network none --memory 256m --cpus 1 \
    -v bkcrab_bkcrab-data:/source/appdata:ro \
    -v bkcrab_minio-data:/source/minio:ro \
    -v /srv/bkcrab-storage/workspaces:/source/workspaces:ro \
    alpine:3.21 tar -C /source -cf - appdata minio workspaces |
    gzip -1 > "$backup/files.tar.gz"
docker inspect bkcrab-bkcrab-1 --format '{{.Image}}' > "$backup/gateway-image.txt"
install -m 600 /home/xavier/Desktop/github/bkcrab/deploy/docker/.env "$backup/deployment.env"
touch "$backup/.complete"
complete=true
# Only rotate completed backups created by this script, never arbitrary paths.
python3 - "$root" <<'PY'
from pathlib import Path
import re, shutil, sys
root = Path(sys.argv[1]).resolve()
backups = sorted(p for p in root.iterdir() if not p.is_symlink() and p.is_dir()
                 and re.fullmatch(r'\d{8}T\d{6}Z', p.name) and (p / '.complete').is_file())
for p in backups[:-3]:
    if p.resolve().parent != root:
        raise RuntimeError('backup path escaped root')
    shutil.rmtree(p)
PY
echo "Backup completed: $backup"
