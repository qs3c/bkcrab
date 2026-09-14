# Single-server sandbox reliability

This deployment keeps Compose. Its budgets and pool admission are **single-gateway** controls; do not enable multiple gateway writers against the same DinD pool or quota ledger.

## Budgets

Enable `deploy/docker/docker-compose.reliability.yml` after installing its host filesystem service:

| Setting | Default |
| --- | --- |
| Live sandbox containers, global / per user | 4 / 2 |
| Waiting operations / maximum wait | 32 / 60 seconds |
| Each sandbox CPU / RAM / PIDs | 1 / 2 GiB / 256 |
| Gateway CPU / RAM | 2 / 2 GiB |
| Entire DinD CPU / RAM | 6 / 9 GiB |
| User workspace and skill files on disk | 10 GiB / 100,000 inodes |
| User objects in MinIO, across owned agents | 10 GiB / 100,000 objects |
| Shared workspace filesystem | 40 GiB |
| Shared DinD image and writable-layer filesystem | 20 GiB |

Local and MinIO limits apply separately, because they store separate copies. MinIO accounting covers objects written through this gateway's workspace Store; direct administrator writes and separate services using S3 directly are outside it. RAM tmpfs mounts and `/tmp` in the sandbox are not part of the per-user workspace project quota; writable container layers are bounded by the shared DinD filesystem. CPU limits are ceilings, not reservations.

The ext4 project quota is kernel-enforced during `git clone`, including background processes. A short-lived trusted helper assigns the authenticated agent owner's project ID before sandbox startup. The registry remains outside sandbox mounts. A missing quota image, unsupported filesystem, or failed quota assignment prevents sandbox creation. The host root partition is never reformatted: the service creates two new, preallocated loopback filesystem image files in `/srv/bkcrab-storage`.

Capacity is reserved before creation. Calls on one sandbox are serialized. Active calls are not idle-evicted. Under pressure an idle sandbox can be reclaimed early, including its background processes; active calls wait with cancellation and a deadline. This is bounded admission, not strict FIFO fairness. Failed removal retains the capacity reservation. On gateway restart, labeled containers from its previous process are removed while their workspace mounts survive; in-flight agent turns do not resume automatically.

## Persistence

Docker uploads files after mutating tools and before eviction. Sync uses bounded buffers, includes `.git`, compares file contents, skips symbolic links and special files, and reports upload failure while preserving local files. Restore streams missing files and does not overwrite newer local files with stale MinIO objects. Sync has a 30-second budget per operation; a large first upload can require subsequent attempts. This is not a transactional filesystem snapshot. Files deleted only by shell are not automatically deleted from MinIO and can reappear on a later restore; use the workspace deletion API when deleting persisted artifacts.

## Install and migrate

Run from the server checkout after committing/pushing the source and pulling it there. Preserve all existing Compose overlays and `.env` secrets. Build the gateway and quota helper **before** stopping production. Check available disk for 60 GiB of reserved images plus existing data and backups.

1. Install `deploy/docker/reliability/install-host.sh` as root. Review the fixed image and mount paths first. It installs `bkcrab-workspaces.service` and a Docker dependency so boot cannot silently start on an unmounted fallback directory. It also installs the watchdog timer.
2. Stop the watchdog timer during maintenance. Stop the gateway and DinD before copying any Docker storage. Preserve original named volumes as rollback sources.
3. Copy the old `bkcrab-data` workspace subtree to `/srv/bkcrab-storage/workspaces`, its optional `users` subtree to `workspaces/.users`, and the old `sandbox-docker-data` contents to `/srv/bkcrab-storage/docker`, using `cp -a` with stopped writers. Do not remove the source volumes.
4. Append `-f deploy/docker/docker-compose.reliability.yml` to the existing Compose file list. Run `docker compose ... config --quiet`, then `up -d --build sandbox-docker quota-image-init bkcrab`. The quota initializer loads the helper image into DinD.
5. Verify `/livez`, `/readyz`, Docker resource limits and mounts, a real sandbox execution, project quota rejection, and MinIO round-trip. Re-enable the watchdog timer.

The first rollout from legacy sandbox code must remove old `bkcrab=sandbox` containers in the **dedicated DinD daemon** after stopping the gateway; they predate the new pool-owner labels. Never apply a broad cleanup to the host Docker daemon.

## Recovery and monitoring

Existing `unless-stopped` policies restart exited processes. The host watchdog probes `/livez` every 30 seconds; it allows 180 seconds startup time, requires three consecutive failures, waits five minutes between restart attempts, and permits three attempts per hour. `/readyz` failure logs an alert but does not restart dependency-affected processes. Stopped/paused containers preserve operator intent. It restarts only the uniquely identified gateway service in the configured Compose project.

Inspect `journalctl -u bkcrab-watchdog.service`, `systemctl list-timers bkcrab-watchdog.timer`, disk/inode usage of both mounts, DinD OOM events, and the gateway logs for capacity waits or persistence failures. Repeated failure requires operator intervention; restarting cannot fix a full disk or broken database.

## Backup and rollback

Before deploying, retain the old gateway image tag, database dump, source commit, effective Compose file list, and named volumes. For a consistent full backup, use a maintenance window: stop gateway and other writers, take a transactional MySQL dump, copy MinIO data through its supported backup workflow, and archive the mounted workspace tree (including `.quota.json`), then resume. Keep an off-host copy and test restoration; files on this server do not protect against server/disk loss.

To roll back after new writes, stop gateway/DinD and watchdog, copy new workspace/user data back to their original named-volume subtrees, remove the reliability overlay, and start the previous image with its original overlays. Keep the bounded filesystems until restoration has been verified. Do not delete filesystem images while mounted. Returning to the old deployment removes the new resource/quota protections.
