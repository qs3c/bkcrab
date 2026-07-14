package setup

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/qs3c/bkcrab/internal/auth"
	"github.com/qs3c/bkcrab/internal/rag"
	"github.com/qs3c/bkcrab/internal/store"
)

func (s *Server) requireRAG(w http.ResponseWriter) bool {
	if s.rag != nil {
		return true
	}
	message := "RAG 未配置（需要 Milvus 与 embedding 配置）"
	jsonResponse(w, http.StatusServiceUnavailable, map[string]any{
		"ok": false, "error": message, "message": message,
	})
	return false
}

func ragIdentity(r *http.Request) (auth.Identity, bool) {
	identity, ok := auth.FromContext(r.Context())
	return identity, ok && identity.EffectiveUserID() != ""
}

// ragOwnerID returns an empty owner only for platform administrators, which is
// the service's explicit privileged path. Everyone else is tenant-scoped.
func ragOwnerID(identity auth.Identity) string {
	if identity.CanAdminPlatform() {
		return ""
	}
	return identity.EffectiveUserID()
}

func writeRAGError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, rag.ErrForbidden):
		status = http.StatusForbidden
	case errors.Is(err, rag.ErrNotFound), errors.Is(err, store.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, rag.ErrQuota):
		status = http.StatusRequestEntityTooLarge
	case strings.Contains(err.Error(), "不支持的文件类型"),
		strings.Contains(err.Error(), "不能为空"),
		strings.Contains(err.Error(), "必须小于"),
		strings.Contains(err.Error(), "大小不能"):
		status = http.StatusBadRequest
	}
	jsonResponse(w, status, map[string]any{"ok": false, "error": err.Error()})
}

func (s *Server) handleListRAGKBs(w http.ResponseWriter, r *http.Request) {
	if !s.requireRAG(w) {
		return
	}
	identity, ok := ragIdentity(r)
	if !ok {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "unauthorized"})
		return
	}
	kbs, err := s.rag.ListKBs(r.Context(), identity.EffectiveUserID())
	if err != nil {
		writeRAGError(w, err)
		return
	}
	if kbs == nil {
		kbs = []store.RAGKBRecord{}
	}
	jsonResponse(w, http.StatusOK, kbs)
}

func (s *Server) handleCreateRAGKB(w http.ResponseWriter, r *http.Request) {
	if !s.requireRAG(w) || !s.requireWritable(w, r) {
		return
	}
	identity, ok := ragIdentity(r)
	if !ok {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "unauthorized"})
		return
	}
	var request struct {
		Name         string `json:"name"`
		Description  string `json:"description"`
		ChunkSize    int    `json:"chunkSize"`
		ChunkOverlap int    `json:"chunkOverlap"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON body"})
		return
	}
	kb, err := s.rag.CreateKB(r.Context(), identity.EffectiveUserID(), request.Name, request.Description, request.ChunkSize, request.ChunkOverlap)
	if err != nil {
		writeRAGError(w, err)
		return
	}
	jsonResponse(w, http.StatusCreated, kb)
}

func (s *Server) handleGetRAGKB(w http.ResponseWriter, r *http.Request) {
	if !s.requireRAG(w) {
		return
	}
	identity, ok := ragIdentity(r)
	if !ok {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "unauthorized"})
		return
	}
	kb, err := s.rag.GetKB(r.Context(), ragOwnerID(identity), r.PathValue("id"))
	if err != nil {
		writeRAGError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, kb)
}

func (s *Server) handleUpdateRAGKB(w http.ResponseWriter, r *http.Request) {
	if !s.requireRAG(w) || !s.requireWritable(w, r) {
		return
	}
	identity, ok := ragIdentity(r)
	if !ok {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "unauthorized"})
		return
	}
	ownerID := ragOwnerID(identity)
	current, err := s.rag.GetKB(r.Context(), ownerID, r.PathValue("id"))
	if err != nil {
		writeRAGError(w, err)
		return
	}
	var request struct {
		Name         *string `json:"name"`
		Description  *string `json:"description"`
		ChunkSize    *int    `json:"chunkSize"`
		ChunkOverlap *int    `json:"chunkOverlap"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON body"})
		return
	}
	name, description := current.Name, current.Description
	chunkSize, chunkOverlap := current.ChunkSize, current.ChunkOverlap
	if request.Name != nil {
		name = *request.Name
	}
	if request.Description != nil {
		description = *request.Description
	}
	if request.ChunkSize != nil {
		chunkSize = *request.ChunkSize
	}
	if request.ChunkOverlap != nil {
		chunkOverlap = *request.ChunkOverlap
	}
	kb, err := s.rag.UpdateKB(r.Context(), ownerID, current.ID, name, description, chunkSize, chunkOverlap)
	if err != nil {
		writeRAGError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, kb)
}

func (s *Server) handleDeleteRAGKB(w http.ResponseWriter, r *http.Request) {
	if !s.requireRAG(w) || !s.requireWritable(w, r) {
		return
	}
	identity, ok := ragIdentity(r)
	if !ok {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "unauthorized"})
		return
	}
	if err := s.rag.DeleteKB(r.Context(), ragOwnerID(identity), r.PathValue("id")); err != nil {
		writeRAGError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleUploadRAGDocument(w http.ResponseWriter, r *http.Request) {
	if !s.requireRAG(w) || !s.requireWritable(w, r) {
		return
	}
	identity, ok := ragIdentity(r)
	if !ok {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "unauthorized"})
		return
	}
	maxBody := int64(s.rag.MaxFileMB()+1) * 1024 * 1024
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := r.ParseMultipartForm(maxBody); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			jsonResponse(w, http.StatusRequestEntityTooLarge, map[string]any{"ok": false, "error": "上传文件超过大小限制"})
		} else {
			jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid multipart form: " + err.Error()})
		}
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "multipart field file is required"})
		return
	}
	defer file.Close()
	doc, err := s.rag.UploadDocument(r.Context(), ragOwnerID(identity), r.PathValue("id"), header.Filename, file, header.Size)
	if err != nil {
		writeRAGError(w, err)
		return
	}
	jsonResponse(w, http.StatusAccepted, doc)
}

func (s *Server) handleListRAGDocuments(w http.ResponseWriter, r *http.Request) {
	if !s.requireRAG(w) {
		return
	}
	identity, ok := ragIdentity(r)
	if !ok {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "unauthorized"})
		return
	}
	docs, err := s.rag.ListDocuments(r.Context(), ragOwnerID(identity), r.PathValue("id"))
	if err != nil {
		writeRAGError(w, err)
		return
	}
	if docs == nil {
		docs = []store.RAGDocumentRecord{}
	}
	jsonResponse(w, http.StatusOK, docs)
}

func (s *Server) handleDeleteRAGDocument(w http.ResponseWriter, r *http.Request) {
	if !s.requireRAG(w) || !s.requireWritable(w, r) {
		return
	}
	identity, ok := ragIdentity(r)
	if !ok {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "unauthorized"})
		return
	}
	if err := s.rag.DeleteDocument(r.Context(), ragOwnerID(identity), r.PathValue("id"), r.PathValue("docId")); err != nil {
		writeRAGError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleReindexRAGDocument(w http.ResponseWriter, r *http.Request) {
	if !s.requireRAG(w) || !s.requireWritable(w, r) {
		return
	}
	identity, ok := ragIdentity(r)
	if !ok {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "unauthorized"})
		return
	}
	if err := s.rag.ReindexDocument(r.Context(), ragOwnerID(identity), r.PathValue("id"), r.PathValue("docId")); err != nil {
		writeRAGError(w, err)
		return
	}
	jsonResponse(w, http.StatusAccepted, map[string]any{"ok": true})
}

func (s *Server) handleRAGSearch(w http.ResponseWriter, r *http.Request) {
	if !s.requireRAG(w) {
		return
	}
	identity, ok := ragIdentity(r)
	if !ok {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "unauthorized"})
		return
	}
	var request struct {
		Query string `json:"query"`
		TopN  int    `json:"topN"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON body"})
		return
	}
	hits, err := s.rag.Search(r.Context(), ragOwnerID(identity), []string{r.PathValue("id")}, request.Query, request.TopN)
	if err != nil {
		writeRAGError(w, err)
		return
	}
	if hits == nil {
		hits = []rag.Hit{}
	}
	jsonResponse(w, http.StatusOK, map[string]any{"hits": hits})
}
