# Deployment Guide

The gateway forwards requests to the retriever.

| Component | Port | Unit |
| --- | ---: | --- |
| Gateway | 8080 | TCP |
| Retriever | 9090 | TCP |

## Retry Policy

```go
func Retry(attempt int) bool {
    return attempt < 3
}
```

Retries stop after three attempts.
