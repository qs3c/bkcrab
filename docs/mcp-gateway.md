# MCP Gateway Runtime

BkCrab runs agent MCP servers through per-user lucky-aeon MCP Gateway containers.

## Environment

| Variable | Default |
| --- | --- |
| `BKCRAB_MCP_GATEWAY_ENABLED` | `true` |
| `BKCRAB_MCP_GATEWAY_IMAGE` | `ghcr.io/lucky-aeon/mcp-gateway:latest` |
| `BKCRAB_MCP_GATEWAY_RUNTIME_DIR` | `$BKCRAB_HOME/mcp-gateways` |
| `BKCRAB_MCP_GATEWAY_CONTAINER_PORT` | `8080` |
| `BKCRAB_MCP_GATEWAY_PROTOCOL` | `all` |
| `BKCRAB_MCP_GATEWAY_IDLE_TTL_SEC` | `1800` |

## Behavior

- Each user gets one gateway container when one of their agents has enabled MCP servers.
- Agent stdio MCP servers run inside that user's gateway container.
- Remote HTTP MCP servers are deployed into the gateway by URL.
- `Authorization: Bearer <token>` is mapped to the gateway's `MCP_REMOTE_AUTH_ACCESS_TOKEN` env value for upstream bearer auth.
- Other custom HTTP headers are rejected in V1 because the selected gateway does not expose generic downstream header configuration.
- Public agents share owner MCP only when `shareMcpConfig` is enabled.
