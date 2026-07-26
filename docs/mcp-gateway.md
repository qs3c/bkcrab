# MCP Gateway Runtime

BkCrab stores MCP servers as user-owned resources and runs them through per-user
lucky-aeon MCP Gateway containers. Agents store only an allow-list of resource
IDs, never connection credentials.

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

- MCP resources are managed from the main `/mcp/` page, alongside knowledge bases.
- Each agent receives explicit access to selected resource IDs from its MCP settings page.
- Each user gets one gateway container when one of their agents uses an enabled MCP resource.
- Every deployment sends the user's complete enabled resource set, so loading one agent cannot remove another agent's servers.
- The agent runtime filters the aggregated tool list by resource ID before registering tools. An agent cannot see or call tools from ungranted resources.
- User stdio MCP servers run inside that user's gateway container.
- Remote HTTP MCP servers are deployed into the gateway by URL.
- `Authorization: Bearer <token>` is mapped to the gateway's `MCP_REMOTE_AUTH_ACCESS_TOKEN` env value for upstream bearer auth.
- Other custom HTTP headers are rejected in V1 because the selected gateway does not expose generic downstream header configuration.
- Public agents use the owner's gateway but still receive only the resource IDs explicitly granted to that agent.
- Deleting a resource revokes it from every agent before removing its connection details.
