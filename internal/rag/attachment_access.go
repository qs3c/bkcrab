package rag

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/qs3c/bkcrab/internal/rag/document"
)

const (
	attachmentETagVersion = "attachment-v1"
)

var canonicalAttachmentID = regexp.MustCompile(`^att_[0-9a-f]{32}$`)

// AuthorizedAttachment is safe to pass through the HTTP layer. Object keys
// and ownership inputs stay private and are rechecked before streaming.
type AuthorizedAttachment struct {
	MIMEType string
	FileName string
	ByteSize int64
	ETag     string

	attachmentID string
	ownerID      string
}

func (s *Service) AuthorizeAttachment(
	ctx context.Context,
	ownerID, attachmentID string,
) (*AuthorizedAttachment, error) {
	resolved, err := s.authorizeAttachment(ctx, ownerID, attachmentID, false)
	if err != nil {
		return nil, err
	}
	return resolved.descriptor, nil
}

func (s *Service) OpenAuthorizedAttachment(
	ctx context.Context,
	descriptor *AuthorizedAttachment,
) (io.ReadCloser, error) {
	if descriptor == nil {
		return nil, ErrNotFound
	}
	resolved, err := s.authorizeAttachment(
		ctx, descriptor.ownerID, descriptor.attachmentID, true)
	if err != nil {
		return nil, err
	}
	if resolved.descriptor.ETag != descriptor.ETag ||
		resolved.descriptor.MIMEType != descriptor.MIMEType ||
		resolved.descriptor.FileName != descriptor.FileName ||
		resolved.descriptor.ByteSize != descriptor.ByteSize {
		return nil, ErrNotFound
	}
	return resolved.reader, nil
}

type resolvedAttachment struct {
	descriptor *AuthorizedAttachment
	reader     io.ReadCloser
}

func (s *Service) authorizeAttachment(
	ctx context.Context,
	ownerID, attachmentID string,
	openObject bool,
) (*resolvedAttachment, error) {
	if s == nil || s.st == nil || s.obj == nil ||
		!canonicalAttachmentID.MatchString(attachmentID) {
		return nil, ErrNotFound
	}
	attachment, err := s.st.GetRAGAttachment(ctx, attachmentID)
	if err != nil {
		return nil, assetLookupError(err)
	}
	doc, err := s.st.GetRAGDocument(ctx, attachment.DocID)
	if err != nil {
		return nil, assetLookupError(err)
	}

	kbLock := s.kbMutex(doc.KBID)
	kbLock.RLock()
	defer kbLock.RUnlock()
	docLock := s.docMutex(doc.ID)
	docLock.Lock()
	defer docLock.Unlock()

	attachment, err = s.st.GetRAGAttachment(ctx, attachmentID)
	if err != nil {
		return nil, assetLookupError(err)
	}
	doc, err = s.st.GetRAGDocument(ctx, attachment.DocID)
	if err != nil {
		return nil, assetLookupError(err)
	}
	kb, err := s.st.GetRAGKB(ctx, doc.KBID)
	if err != nil {
		return nil, assetLookupError(err)
	}
	user, err := s.st.GetUser(ctx, kb.UserID)
	if err != nil {
		return nil, assetLookupError(err)
	}
	inActiveVersion, err := s.st.IsRAGAttachmentInVersion(
		ctx, doc.ID, doc.ActiveVersion, attachment.ID)
	if err != nil {
		return nil, assetLookupError(err)
	}
	if ownerID != "" && kb.UserID != ownerID {
		return nil, ErrNotFound
	}
	if !strings.EqualFold(kb.Status, "active") ||
		strings.EqualFold(doc.Status, "deleting") ||
		!strings.EqualFold(user.Status, "active") ||
		!inActiveVersion ||
		attachment.Kind != document.AttachmentKindVisioSource ||
		attachment.MIMEType != document.MIMETypeVSDX ||
		!canonicalSHA256(attachment.ContentSHA256) ||
		attachment.ByteSize < 1 || strings.TrimSpace(attachment.ObjectKey) == "" ||
		!safeAttachmentFileName(attachment.FileName) {
		return nil, ErrNotFound
	}
	descriptor := &AuthorizedAttachment{
		MIMEType:     attachment.MIMEType,
		FileName:     attachment.FileName,
		ByteSize:     attachment.ByteSize,
		ETag:         fmt.Sprintf(`"%s-%s"`, attachmentETagVersion, attachment.ContentSHA256),
		attachmentID: attachment.ID,
		ownerID:      ownerID,
	}
	if !openObject {
		return &resolvedAttachment{descriptor: descriptor}, nil
	}
	reader, err := s.obj.Get(ctx, attachment.ObjectKey)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, errors.New("RAG attachment is temporarily unavailable")
	}
	return &resolvedAttachment{descriptor: descriptor, reader: reader}, nil
}

func safeAttachmentFileName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" || filepath.Base(name) != name ||
		strings.ContainsAny(name, `/\`+"\x00\r\n") ||
		!strings.HasSuffix(strings.ToLower(name), ".vsdx") {
		return false
	}
	return true
}
