# Deployment Guide

The gateway forwards requests to the retriever.

| Component | Port | Unit |
| --- | ---: | --- |
| Gateway | 8080 | TCP |
| Retriever | 9090 | TCP |

![请求流程图](rag-asset://occ_page_0001_0001)

> 图片文字：Gateway → Retriever → Text LLM

## Retry Policy

```go
func Retry(attempt int) bool {
    return attempt < 3
}
```

Retries stop after three attempts.
