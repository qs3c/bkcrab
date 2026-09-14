#!/bin/sh
# Run as root from this directory after reviewing workspace-mount.sh.
set -eu
test "$(id -u)" = 0
cd "$(dirname "$0")"
install -d -m 755 /usr/local/lib/bkcrab
install -m 755 workspace-mount.sh /usr/local/lib/bkcrab/workspace-mount.sh
install -m 644 watchdog.py /usr/local/lib/bkcrab/watchdog.py
install -m 755 backup.sh /usr/local/lib/bkcrab/backup.sh
install -m 644 bkcrab-backup.service bkcrab-backup.timer /etc/systemd/system/
install -m 644 bkcrab-workspaces.service bkcrab-watchdog.service bkcrab-watchdog.timer /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now bkcrab-workspaces.service
# Do not let Docker auto-start the gateway on an unmounted fallback directory.
install -d -m 755 /etc/systemd/system/docker.service.d
printf '[Unit]\nRequires=bkcrab-workspaces.service\nAfter=bkcrab-workspaces.service\n' > /etc/systemd/system/docker.service.d/bkcrab-workspaces.conf
systemctl daemon-reload
systemctl enable --now bkcrab-watchdog.timer
systemctl enable --now bkcrab-backup.timer
