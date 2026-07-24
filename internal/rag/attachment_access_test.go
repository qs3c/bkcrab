package rag

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag/document"
	"github.com/qs3c/bkcrab/internal/rag/objects"
	"github.com/qs3c/bkcrab/internal/store"
)

type gapAttachmentStore struct {
	store.Store
	inVersion  bool
	attachment store.RAGAttachmentRecord
	doc        store.RAGDocumentRecord
	kb         store.RAGKBRecord
	user       store.UserRecord
}

func (s *gapAttachmentStore) GetRAGAttachment(context.Context, string) (*store.RAGAttachmentRecord, error) {
	value := s.attachment
	return &value, nil
}

func (s *gapAttachmentStore) GetRAGDocument(context.Context, string) (*store.RAGDocumentRecord, error) {
	value := s.doc
	return &value, nil
}

func (s *gapAttachmentStore) GetRAGKB(context.Context, string) (*store.RAGKBRecord, error) {
	value := s.kb
	return &value, nil
}

func (s *gapAttachmentStore) GetUser(context.Context, string) (*store.UserRecord, error) {
	value := s.user
	return &value, nil
}

func (s *gapAttachmentStore) IsRAGAttachmentInVersion(
	context.Context, string, int64, string,
) (bool, error) {
	return s.inVersion, nil
}

func TestAuthorizeAttachmentRequiresExactActiveVersionMembership(t *testing.T) {
	attachmentID := "att_" + strings.Repeat("a", 32)
	st := &gapAttachmentStore{
		attachment: store.RAGAttachmentRecord{
			ID: attachmentID, DocID: "doc_gap", ContentSHA256: strings.Repeat("b", 64),
			Kind: document.AttachmentKindVisioSource, FileName: "gap.vsdx",
			MIMEType: document.MIMETypeVSDX, ObjectKey: "rag/u/kb/doc/attachment.vsdx",
			ByteSize: 42, FirstSeenVersion: 1, LastSeenVersion: 3,
		},
		doc: store.RAGDocumentRecord{
			ID: "doc_gap", KBID: "kb_gap", Status: "DONE", ActiveVersion: 2,
		},
		kb:   store.RAGKBRecord{ID: "kb_gap", UserID: "u_gap", Status: "active"},
		user: store.UserRecord{ID: "u_gap", Status: "active"},
	}
	service := New(Deps{
		Store: st, Objects: objects.NewLocalFS(t.TempDir()), Cfg: config.RAGCfg{},
	})
	if _, err := service.AuthorizeAttachment(
		context.Background(), "u_gap", attachmentID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("gap version authorization err=%v", err)
	}
	st.inVersion = true
	descriptor, err := service.AuthorizeAttachment(
		context.Background(), "u_gap", attachmentID)
	if err != nil || descriptor == nil || descriptor.FileName != "gap.vsdx" {
		t.Fatalf("active membership descriptor=%+v err=%v", descriptor, err)
	}
}
