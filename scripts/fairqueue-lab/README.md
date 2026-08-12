# Fair queue lab

`fairqueue-lab` creates multiple temporary BkCrab users, gives each user an
isolated RAG knowledge base, uploads documents concurrently, and records the
fair scheduler while the documents are indexed. It uses real HTTP sessions;
no RabbitMQ message or Redis key is fabricated.

The admin health endpoint is always sampled. For conclusive per-tenant proof,
also expose Redis and RabbitMQ management through SSH tunnels:

```bash
ssh -L 6379:127.0.0.1:6379 \
    -L 15672:127.0.0.1:15672 \
    root@192.168.1.72
```

In another terminal, provide secrets only through environment variables:

```bash
export BKCRAB_LAB_ADMIN_PASSWORD='<bkcrab-admin-password>'
export BKCRAB_LAB_REDIS_PASSWORD='<redis-password>'
export BKCRAB_LAB_RABBIT_USER='bkcrab'
export BKCRAB_LAB_RABBIT_PASSWORD='<rabbit-password>'

go run ./scripts/fairqueue-lab \
  -base-url http://192.168.1.72 \
  -admin-login admin \
  -users 3 \
  -documents 3 \
  -document-bytes 4096 \
  -redis-addr 127.0.0.1:6379 \
  -rabbit-management-url http://127.0.0.1:15672
```

An admin API key may replace the password by setting
`BKCRAB_LAB_ADMIN_TOKEN`. Reports are written with mode `0600` and contain no
passwords or tokens. Unless `-keep` is passed, temporary users and all their
RAG data are deleted when the run ends, including after most failures.

The verdict is deliberately conservative:

- `PASS` means fair mode stayed healthy, contention was observed, concurrency
  ceilings held, every tenant appeared in direct observations, all documents
  completed, no DLQ/failure occurred, and all temporary user/queue artifacts
  observed by the tool were safely removed.
- `INCONCLUSIVE` usually means the generated work finished too quickly or a
  direct Redis/RabbitMQ observer was omitted.
- `FAIL` means a safety ceiling, health condition, completion, or DLQ check
  failed.
