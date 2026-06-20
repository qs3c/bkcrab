# Memory Management Tool Design

> Status: design approved, pending implementation plan
> Date: 2026-06-20
> Related code: `internal/agent/memory.go`, `internal/agent/context.go`, `internal/agent/tools/file.go`, `internal/agent/tools/registry.go`, `internal/store/database.go`, `internal/store/store.go`, `internal/privacy/`

## 1. Context

BkClaw now has two reasonably complete memory extraction triggers:

- Model-initiated persistence: the model is instructed to update `USER.md` and `MEMORY.md` during the turn.
- Cadence persistence: every N completed, unextracted turns are claimed via `turn_status` and `extraction_id`, then replayed from `session_messages` into `AutoPersistMemory`.

The remaining problem is memory file maintenance. Today, the model-initiated path uses generic file tools. When the model calls `write_file("USER.md", ...)`, `write_file("MEMORY.md", ...)`, or `edit_file(...)`, the runtime routes those bare system filenames to `agent_files` through `systemFileStore` when available. The data is therefore usually stored in the database, not just on disk, but the mutation surface is still the generic file tool surface.

That creates several issues:

- No hard capacity limit, so memory can grow without bound.
- No structured entry model, so duplicate and stale entries are hard to detect or remove.
- `SaveMemoryWithScan` and `SaveUserFile` log safety threats but still write them.
- Generic file writes can overwrite the entire memory blob and lose unrelated entries.
- Model-initiated writes and cadence extraction both mutate the same files, but not through a shared manager.

Hermes has a useful pattern here: a dedicated `memory` tool with add, replace, remove, and batch operations; strict safety scanning; duplicate handling; size budgets; and a pending approval layer. BkClaw should adopt the dedicated managed-memory boundary first, without taking on a full provider plugin architecture yet.

## 2. Decision

Use a dedicated `memory` tool as the only model-visible way to read or mutate `USER.md` and `MEMORY.md`.

Generic `read_file`, `write_file`, and `edit_file` must refuse bare `USER.md` and `MEMORY.md` paths, including absolute paths whose basename is one of those files. The refusal should tell the model to use the `memory` tool. This applies even for admin/owner callers inside model tool execution; admin UI and setup handlers can continue to use store APIs directly.

The existing storage location remains `agent_files`:

```text
agent_files(agent_id, user_id, filename)
filename = USER.md or MEMORY.md
user_id  = current chatter user id
```

This keeps the current per-chatter isolation model and avoids a table migration in the first implementation. A later `memory_entries` table remains possible if search, per-entry audit, or vector retrieval becomes a requirement.

## 3. Goals

1. Block generic file-tool reads and writes to `USER.md` and `MEMORY.md`.
2. Provide a dedicated model tool for list, add, replace, remove, and atomic batch operations.
3. Use the same memory manager for model-initiated writes and cadence extraction.
4. Enforce hard size budgets per target.
5. Reject unsafe memory content before it can enter persistent memory.
6. Deduplicate entries deterministically.
7. Make replacement and deletion precise through unique substring matching.
8. Preserve existing memories and migrate them safely into the managed format.
9. Keep changes scoped to the current `agent_files` persistence model.

Non-goals for this phase:

- No external memory provider/plugin system.
- No semantic search or vector store.
- No approval queue in v1.
- No per-entry database table unless the `agent_files` approach proves insufficient.

## 4. Memory Tool Surface

Register one built-in tool named `memory`.

Input shape:

```json
{
  "target": "user | memory",
  "action": "list | add | replace | remove",
  "content": "entry text for add/replace",
  "old_text": "short unique substring for replace/remove",
  "operations": [
    {"action": "remove", "old_text": "stale substring"},
    {"action": "replace", "old_text": "old substring", "content": "new entry"},
    {"action": "add", "content": "new entry"}
  ]
}
```

Rules:

- `target=user` maps to `USER.md`.
- `target=memory` maps to `MEMORY.md`.
- `operations` is preferred for any multi-step maintenance. It is atomic and all-or-nothing.
- `list` returns current managed entries plus usage.
- `add` trims content, rejects empty entries, blocks unsafe content, and no-ops exact duplicates.
- `replace` and `remove` require `old_text` to match exactly one distinct entry.
- If `old_text` matches multiple distinct entries, the tool returns short previews and refuses the mutation.
- If the final state would exceed the configured budget, the tool refuses and returns current entries so the model can consolidate in one follow-up batch.

The success response should be terminal and compact:

```json
{
  "success": true,
  "done": true,
  "target": "memory",
  "entry_count": 12,
  "usage": "38% - 4560/12000 chars",
  "message": "Applied 3 operation(s)."
}
```

It should not echo every entry on success, because that invites repeated self-maintenance loops.

## 5. Managed Format

The manager should own serialization. Context building must render managed entries, not trust raw file bytes.

Use a stable marker and delimiter inside the existing `agent_files.content` blob:

```markdown
<!-- bkclaw-memory:v1 target=memory -->
entry one

§

entry two
```

Rationale:

- Existing `agent_files` remains the only persistence table.
- The content is still human-readable enough for admin/debug inspection.
- The section delimiter allows multiline entries.
- The marker lets the manager distinguish managed content from legacy free-form Markdown.

Legacy content without the marker is imported on first managed read:

- Bullet lines become individual entries when possible.
- `## Auto-persisted` headings are treated as grouping noise, not entries.
- Remaining non-empty paragraphs become entries.
- Exact duplicate entries are removed, preserving the first occurrence.
- The manager should not rewrite legacy content on a read-only `list`.
- The first successful mutation rewrites the target into v1 managed format.

If legacy parsing encounters content that cannot be safely round-tripped, the manager should expose it as a legacy entry and require a batch replacement before further additions when the budget would be exceeded. No content should be silently dropped.

## 6. Store Contract

Add a store-level atomic mutation method for agent files, because generic read-then-save is not safe under concurrent turns:

```go
type AgentFileMutator func(current []byte, exists bool) (next []byte, delete bool, err error)

MutateAgentFile(ctx context.Context, agentID, userID, filename string, fn AgentFileMutator) ([]byte, error)
```

For `DBStore`, implement this in one transaction:

1. Lock or serialize the `(agent_id, user_id, filename)` row.
2. Read current content if present.
3. Run the mutator.
4. Upsert or delete.
5. Commit.

Backend details:

- PostgreSQL/MySQL: `SELECT ... FOR UPDATE` when the row exists. If the row does not exist, perform the insert in the same transaction and rely on the primary key to serialize competing creates.
- SQLite: use the existing transaction pattern and WAL/busy timeout behavior. If lock contention appears in tests, use an immediate write transaction for this method.

The mutator must be pure and fast: parse, validate, and serialize only. It must not call LLMs or external services while the DB transaction is open.

For no-store filesystem mode, provide a file-lock plus atomic rename implementation, mirroring Hermes' lock/temp-file/replace approach.

## 7. Safety Model

Memory writes are blocking, not warn-only.

Introduce a strict memory scanner in `internal/privacy`, either by extending `Scan` with a strict mode or adding `ScanMemoryStrict`. It should cover at least:

- Prompt injection: ignore/disregard prior instructions, role hijack, system prompt override, remove filters.
- Exfiltration: output full context, send results to a URL, read secret files, curl/wget secret patterns.
- Persistence abuse: authorized_keys, shell pipe installers, agent config modification instructions.
- Hardcoded credentials: existing API key/private key/token patterns.
- Invisible Unicode: include current zero-width characters and directional isolates.

On writes:

- Any threat rejects the operation.
- In `batch`, one unsafe add/replace rejects the whole batch.

On context load:

- Unsafe legacy or managed entries must not enter the system prompt.
- The rendered snapshot should include a placeholder such as:

```text
[BLOCKED: MEMORY.md entry contained threat pattern(s): prompt_injection. Use memory(remove) to delete the original.]
```

The raw entry remains in storage so it can be inspected and deleted.

## 8. File Tool Changes

Update generic file tools so model calls cannot operate on managed memory files.

Blocked paths:

- `USER.md`
- `MEMORY.md`
- absolute paths whose basename is `USER.md` or `MEMORY.md`

Blocked tools:

- `read_file`
- `write_file`
- `edit_file`

Refusal text should be model-facing:

```text
[refused: USER.md and MEMORY.md are managed memory resources. Use the memory tool with target="user" or target="memory" to list, add, replace, remove, or batch-edit entries.]
```

This is stricter than the current identity-file gate. `USER.md` and `MEMORY.md` are not private agent identity files, but they are prompt-injected persistent state and therefore need a dedicated mutation and read path.

## 9. Prompt Changes

Update `ContextBuilder` memory instructions:

- Remove instructions telling the model to call `write_file` or `edit_file` for `USER.md` and `MEMORY.md`.
- Tell it to use `memory(target="user", action="add", ...)` for identity/profile facts.
- Tell it to use `memory(target="memory", action="add", ...)` for recurring topics, decisions, environment facts, and stable preferences.
- Tell it to use one `operations` batch when consolidating duplicates or making room.
- Tell it that raw file tools will refuse managed memory files.

The prompt should keep the existing distinction:

- `USER.md` / `target=user`: who the current chatter is.
- `MEMORY.md` / `target=memory`: ongoing context and durable facts about work with this chatter.

## 10. Auto-Persist Changes

`AutoPersistMemory` should no longer append Markdown sections and call `SaveMemoryWithScan` / `SaveUserFile` directly.

Instead:

1. The LLM extraction still returns `memory_facts` and `user_notes`.
2. Each fact/note becomes a managed add operation.
3. Apply them through the same memory manager used by the `memory` tool.
4. If an operation is duplicate, treat it as success/no-op.
5. If the target is full, log a structured warning and skip the new entries for v1.

Optional follow-up:

- Add an automatic consolidation path when auto-persist overflows: ask the extraction model to produce a smaller final entry set from current entries plus candidates, then apply one manager-owned replace-all operation. This is useful, but it should not block the first safe boundary implementation.

## 11. Configuration

Add memory management settings under the existing memory config namespace:

```yaml
memory:
  managed:
    enabled: true
    user_char_limit: 4000
    memory_char_limit: 12000
```

Behavior:

- `enabled=true` is the default for new installs.
- When enabled, file tools refuse `USER.md` and `MEMORY.md`.
- The dedicated `memory` tool is registered.
- Auto-persist uses the manager.

For backward compatibility, existing raw memory content remains readable through the manager and is canonicalized on first mutation.

## 12. Test Plan

Store and manager tests:

- Parse empty, managed, and legacy memory content.
- Deduplicate exact duplicates on load.
- Add duplicate is no-op.
- Add over limit returns current entries and does not write.
- Replace/remove require unique substring.
- Batch is all-or-nothing.
- Unsafe add/replace rejects the whole write.
- Concurrent `MutateAgentFile` calls do not drop entries.

Tool tests:

- `memory list/add/replace/remove/batch` success and error paths.
- Invalid target/action validation.
- Success response does not echo all entries.
- Overflow response includes entries for consolidation.

File tool tests:

- `read_file("USER.md")` and `read_file("MEMORY.md")` refuse.
- `write_file` and `edit_file` refuse both bare and absolute basename paths.
- Nested workspace files such as `notes/MEMORY.md` remain ordinary workspace files and are not blocked.

Agent integration tests:

- System prompt includes rendered managed memory entries.
- Unsafe entries are replaced with blocked placeholders in the prompt.
- Model tool registry includes `memory`.
- Context prompt no longer instructs `write_file` for managed memory.
- Auto-persist writes through manager and respects duplicate/no-op behavior.

Regression tests:

- Existing cadence extraction still claims/reset batches correctly.
- Existing per-chatter isolation remains intact: public-agent visitors do not see owner memories.

## 13. Implementation Order

1. Add manager parsing, serialization, validation, and strict scanner tests.
2. Add `MutateAgentFile` to the store interface and `DBStore`.
3. Register the `memory` tool and wire it to current agent/chatter scope.
4. Block generic file tools for `USER.md` and `MEMORY.md`.
5. Update context prompt instructions and prompt rendering.
6. Route `AutoPersistMemory` writes through the manager.
7. Run focused store, tool, agent, and full Go tests.

## 14. Deferred Decisions

These decisions are intentionally scoped out of the first implementation:

- `memory list` will show redacted snippets for blocked entries, not raw unsafe content. The snippet must be sufficient for targeted deletion with `old_text`.
- Admin UI row-level editing is deferred. The first implementation keeps storage compatible and exposes managed edits through the tool and manager APIs.
- Auto-persist LLM consolidation on overflow is deferred. The first implementation logs and skips overflowed auto-persist candidates after duplicate handling.

## 15. Summary

Treat `USER.md` and `MEMORY.md` as managed memory resources, not ordinary files. Keep the current `agent_files` persistence model, but put a strict manager in front of it: one `memory` tool, atomic updates, hard limits, duplicate handling, blocking safety scans, unique-substring replace/remove, and all-or-nothing batch edits. Then make both model-initiated writes and cadence extraction use that same path, while generic file tools refuse direct access to managed memory files.
