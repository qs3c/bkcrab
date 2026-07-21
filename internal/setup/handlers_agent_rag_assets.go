package setup

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/qs3c/bkcrab/internal/auth"
	"github.com/qs3c/bkcrab/internal/rag"
	"github.com/qs3c/bkcrab/internal/rag/document"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/users"
)

const (
	maxAssistantRAGAssetGrants     = 6
	maxAssistantRAGAssetGrantBytes = 256 << 10
)

func (s *Server) handleAgentRAGAsset(w http.ResponseWriter, r *http.Request) {
	s.serveAgentRAGAsset(w, r, rag.AssetDisplay)
}

func (s *Server) handleAgentRAGAssetThumbnail(w http.ResponseWriter, r *http.Request) {
	s.serveAgentRAGAsset(w, r, rag.AssetThumbnail)
}

func (s *Server) serveAgentRAGAsset(w http.ResponseWriter, r *http.Request, variant rag.AssetVariant) {
	if !s.requireRAG(w) {
		return
	}
	descriptor, err := s.authorizeAgentSessionRAGAsset(r, variant)
	if err != nil {
		writeRAGAssetError(w, err)
		return
	}

	// Agent/session/message authorization and the RAG service's current-state
	// tombstone checks must both finish before cache validators are considered.
	setRAGAssetHeaders(w.Header(), descriptor)
	if requestETagMatches(r.Header.Get("If-None-Match"), descriptor.ETag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	// OpenAuthorizedAsset repeats the asset -> document -> KB -> user checks
	// while the RAG mutation locks are held. It never exposes source objects.
	reader, err := s.rag.OpenAuthorizedAsset(r.Context(), descriptor)
	if err != nil {
		clearRAGAssetHeaders(w.Header())
		writeRAGAssetError(w, err)
		return
	}
	defer reader.Close()
	if _, err := io.Copy(w, reader); err != nil {
		slog.Warn("stream agent RAG asset",
			"agent", r.PathValue("agentId"),
			"session", r.PathValue("sessionId"),
			"asset", r.PathValue("assetId"),
			"variant", variant,
			"error", err,
		)
	}
}

func (s *Server) authorizeAgentSessionRAGAsset(r *http.Request, variant rag.AssetVariant) (*rag.AuthorizedAsset, error) {
	if s.dataStore == nil {
		return nil, rag.ErrNotFound
	}
	agentID := strings.TrimSpace(r.PathValue("agentId"))
	sessionID := strings.TrimSpace(r.PathValue("sessionId"))
	assetID := strings.TrimSpace(r.PathValue("assetId"))
	if agentID == "" || sessionID == "" || assetID == "" {
		return nil, rag.ErrNotFound
	}

	// resolveAgent is the canonical readable-agent gate. In particular, it
	// admits an authenticated visitor only for a public agent (or an explicit
	// API-key ACL) and resolves the handle in that visitor's UserSpace.
	if s.resolveAgent(r, agentID) == nil {
		return nil, rag.ErrNotFound
	}
	// A manager can briefly retain a stale in-memory handle after deletion.
	// Requiring the backing row keeps deletion revocation immediate.
	agentRecord, err := s.dataStore.GetAgent(r.Context(), agentID)
	if err != nil {
		return nil, agentRAGAssetLookupError(err)
	}
	if !currentAgentReadableForRAGAsset(r, agentRecord) {
		return nil, rag.ErrNotFound
	}

	userID := s.effectiveUserID(r)
	if userID == "" {
		return nil, rag.ErrNotFound
	}
	// The compound store key is the session ownership check: a public-agent
	// visitor can only name a session persisted under their effective user ID.
	if _, err := s.dataStore.GetSession(r.Context(), userID, agentID, sessionID); err != nil {
		return nil, agentRAGAssetLookupError(err)
	}
	messages, err := s.dataStore.ListSessionMessages(r.Context(), userID, agentID, sessionID)
	if err != nil {
		return nil, agentRAGAssetLookupError(err)
	}
	references := persistedAssistantRAGAssetRefs(messages, assetID)
	if len(references) == 0 {
		return nil, rag.ErrNotFound
	}

	// The persisted grant includes retrieval-time KB/document provenance. Do
	// not trust the asset ID alone: re-resolve its current catalog chain and
	// require one exact persisted tuple before asking the RAG service to expose
	// cache metadata.
	asset, err := s.dataStore.GetRAGAsset(r.Context(), assetID)
	if err != nil {
		return nil, agentRAGAssetLookupError(err)
	}
	doc, err := s.dataStore.GetRAGDocument(r.Context(), asset.DocID)
	if err != nil {
		return nil, agentRAGAssetLookupError(err)
	}
	kb, err := s.dataStore.GetRAGKB(r.Context(), doc.KBID)
	if err != nil {
		return nil, agentRAGAssetLookupError(err)
	}
	matched := false
	for _, ref := range references {
		if ref.Asset.ID == asset.ID && ref.DocID == doc.ID && ref.KBID == kb.ID {
			matched = true
			break
		}
	}
	if !matched {
		return nil, rag.ErrNotFound
	}

	// An empty owner is intentional here. Session-scoped access is granted by
	// the exact trusted assistant reference above; AuthorizeAsset then performs
	// the independent current-state asset/doc/KB/user and safe-raster checks.
	return s.rag.AuthorizeAsset(r.Context(), "", assetID, variant)
}

func currentAgentReadableForRAGAsset(r *http.Request, record *store.AgentRecord) bool {
	if record == nil {
		return false
	}
	identity, ok := auth.FromContext(r.Context())
	if !ok {
		return false
	}
	userID := identity.EffectiveUserID()
	if userID == "" {
		return false
	}
	if record.UserID == userID || identity.Role == users.RoleSuperAdmin {
		return true
	}
	if identity.AuthMethod == "apikey" && identity.CanAccessAgent(record.ID) {
		return true
	}
	return record.IsPublic
}

func persistedAssistantRAGAssetRefs(messages []store.SessionMessage, assetID string) []rag.RAGResourceRef {
	var matches []rag.RAGResourceRef
	for _, message := range messages {
		if message.Role != "assistant" || len(message.Metadata) == 0 {
			continue
		}
		value, ok := message.Metadata["ragResources"]
		if !ok {
			continue
		}
		raw, err := json.Marshal(value)
		if err != nil || len(raw) == 0 || len(raw) > maxAssistantRAGAssetGrantBytes {
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		var refs []rag.RAGResourceRef
		if err := decoder.Decode(&refs); err != nil || len(refs) == 0 || len(refs) > maxAssistantRAGAssetGrants {
			continue
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			continue
		}
		valid := true
		for _, ref := range refs {
			if !validPersistedRAGResourceRef(ref) {
				valid = false
				break
			}
		}
		if !valid {
			continue
		}
		for _, ref := range refs {
			if ref.Asset.ID == assetID {
				matches = append(matches, ref)
			}
		}
	}
	return matches
}

func validPersistedRAGResourceRef(ref rag.RAGResourceRef) bool {
	if strings.TrimSpace(ref.Asset.ID) == "" || ref.Asset.Kind != document.AssetKindImage ||
		strings.TrimSpace(ref.KBID) == "" || strings.TrimSpace(ref.DocID) == "" || ref.ChunkIndex < 0 ||
		ref.Asset.PageNum < 0 || ref.Asset.Width < 0 || ref.Asset.Height < 0 {
		return false
	}
	if ref.Asset.MIMEType != "" && !strings.HasPrefix(strings.ToLower(ref.Asset.MIMEType), "image/") {
		return false
	}
	if ref.Asset.Location.Kind != "" {
		if err := ref.Asset.Location.Validate(); err != nil {
			return false
		}
	}
	return ref.SourceLocation.Validate() == nil
}

func agentRAGAssetLookupError(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return rag.ErrNotFound
	}
	return err
}
