package setup

import (
	"github.com/qs3c/bkclaw/internal/api"
	"github.com/qs3c/bkclaw/internal/store"
	"github.com/qs3c/bkclaw/internal/workspace"
)

// Shared, single-responsibility collaborators. Each holds only the
// concrete instances its own methods need — none is a catch-all. Handlers
// that need a piece of cross-cutting behavior hold a pointer to the
// relevant service rather than re-implementing it. The composition root
// (Server.engine) builds one of each and shares it.
//
// Field names match the original Server fields (dataStore, userResolver,
// workspaceStore) so the method bodies that moved here are unchanged.

// agentGuard is the lowest layer: agent ownership/readability checks,
// agent resolution, and per-tenant cache invalidation. Depends only on
// the store (ownership rows) and the user resolver (live agent routing +
// cache busting).
type agentGuard struct {
	dataStore    store.AgentStore
	userResolver api.UserResolver
}

// configRepo reads and writes per-user / per-agent config (system
// settings, scoped providers/channels, agent-scope defaults). The scope
// authorization path checks agent ownership, so it borrows agentGuard.
type configRepo struct {
	dataStore agentConfigStore
	guard     *agentGuard
}

// workspaceRepo resolves session/project scopes and serves agent
// workspace + system files. It checks ownership via agentGuard and reads
// blobs from the workspace store.
type workspaceRepo struct {
	dataStore      workspaceRepoStore
	workspaceStore workspace.Store
	guard          *agentGuard
}

// channelRepo manages IM channel bindings: credential uniqueness, the
// per-binding rows, and hot (un)registration against the live runtime.
type channelRepo struct {
	dataStore    agentConfigStore
	userResolver api.UserResolver
	guard        *agentGuard
}
