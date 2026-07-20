package setup

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/qs3c/bkcrab/internal/rag"
	"github.com/qs3c/bkcrab/internal/store"
)

func (s *Server) handleRAGAsset(w http.ResponseWriter, r *http.Request) {
	s.serveRAGAsset(w, r, rag.AssetDisplay)
}

func (s *Server) handleRAGAssetThumbnail(w http.ResponseWriter, r *http.Request) {
	s.serveRAGAsset(w, r, rag.AssetThumbnail)
}

func (s *Server) serveRAGAsset(w http.ResponseWriter, r *http.Request, variant rag.AssetVariant) {
	if !s.requireRAG(w) {
		return
	}
	identity, ok := ragIdentity(r)
	if !ok {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "unauthorized"})
		return
	}

	descriptor, err := s.rag.AuthorizeAsset(r.Context(), ragOwnerID(identity), r.PathValue("assetId"), variant)
	if err != nil {
		writeRAGAssetError(w, err)
		return
	}
	setRAGAssetHeaders(w.Header(), descriptor)
	if requestETagMatches(r.Header.Get("If-None-Match"), descriptor.ETag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	reader, err := s.rag.OpenAuthorizedAsset(r.Context(), descriptor)
	if err != nil {
		clearRAGAssetHeaders(w.Header())
		writeRAGAssetError(w, err)
		return
	}
	defer reader.Close()
	if _, err := io.Copy(w, reader); err != nil {
		slog.Warn("stream RAG asset", "asset", r.PathValue("assetId"), "variant", variant, "error", err)
	}
}

func setRAGAssetHeaders(header http.Header, descriptor *rag.AuthorizedAsset) {
	header.Set("Content-Type", descriptor.MIMEType)
	header.Set("Content-Disposition", "inline")
	header.Set("Cache-Control", "private, no-cache")
	header.Set("ETag", descriptor.ETag)
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Cross-Origin-Resource-Policy", "same-origin")
}

func clearRAGAssetHeaders(header http.Header) {
	for _, name := range []string{
		"Content-Type", "Content-Disposition", "Cache-Control", "ETag",
		"X-Content-Type-Options", "Cross-Origin-Resource-Policy",
	} {
		header.Del(name)
	}
}

func requestETagMatches(ifNoneMatch, current string) bool {
	for _, candidate := range strings.Split(ifNoneMatch, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" {
			return true
		}
		candidate = strings.TrimSpace(strings.TrimPrefix(candidate, "W/"))
		if candidate == current {
			return true
		}
	}
	return false
}

func writeRAGAssetError(w http.ResponseWriter, err error) {
	if errors.Is(err, rag.ErrNotFound) || errors.Is(err, rag.ErrForbidden) || errors.Is(err, store.ErrNotFound) {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "resource not found"})
		return
	}
	jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "resource unavailable"})
}
