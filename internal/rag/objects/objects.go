// Package objects stores original RAG documents. The interface is
// intentionally narrow: ingestion only needs put, get, and prefix deletion.
// Production uses S3-compatible storage; development and tests use LocalFS.
package objects

import (
	"context"
	"fmt"
	"io"
	"mime"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/qs3c/bkcrab/internal/workspace"
)

// Store is the original-document storage surface used by the RAG pipeline.
type Store interface {
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	DeletePrefix(ctx context.Context, prefix string) error
}

// Key returns rag/<user>/<kb>/<doc>/<fileName>. Only the file's base name is
// retained and parent-directory markers are removed to prevent path traversal.
func Key(userID, kbID, docID, fileName string) string {
	base := path.Base(strings.ReplaceAll(fileName, "\\", "/"))
	base = strings.ReplaceAll(base, "..", "_")
	if base == "" || base == "." || base == "/" {
		base = "file"
	}
	return path.Join("rag", userID, kbID, docID, base)
}

// LocalFS stores object keys below a local root directory.
type LocalFS struct {
	root string
}

// NewLocalFS constructs a local object store. Directories are created lazily
// by Put.
func NewLocalFS(root string) *LocalFS {
	root = filepath.Clean(root)
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	return &LocalFS{root: root}
}

func (s *LocalFS) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fullPath, err := s.resolveKey(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return fmt.Errorf("create object directory: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(fullPath), ".rag-upload-*")
	if err != nil {
		return fmt.Errorf("create temporary object: %w", err)
	}
	tmpName := tmp.Name()
	keepTemp := true
	defer func() {
		if keepTemp {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := io.Copy(tmp, &contextReader{ctx: ctx, r: r}); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write object %s: %w", key, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close object %s: %w", key, err)
	}
	if err := renameReplace(tmpName, fullPath); err != nil {
		return fmt.Errorf("commit object %s: %w", key, err)
	}
	keepTemp = false
	return nil
}

func (s *LocalFS) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fullPath, err := s.resolveKey(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(fullPath)
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (s *LocalFS) DeletePrefix(ctx context.Context, prefix string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cleanPrefix, err := validateDeletePrefix(prefix)
	if err != nil {
		return err
	}
	fullPath, err := s.resolveKey(strings.TrimSuffix(cleanPrefix, "/"))
	if err != nil {
		return err
	}
	if err := os.RemoveAll(fullPath); err != nil {
		return fmt.Errorf("delete object prefix %s: %w", prefix, err)
	}
	return nil
}

func (s *LocalFS) resolveKey(key string) (string, error) {
	clean, err := validateObjectKey(key)
	if err != nil {
		return "", err
	}
	fullPath := filepath.Join(s.root, filepath.FromSlash(clean))
	rel, err := filepath.Rel(s.root, fullPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("object key %q 超出存储根目录", key)
	}
	return fullPath, nil
}

// S3 stores original documents in the same S3-compatible service configured
// for workspaces. workspace.S3Config.Prefix is retained, with RAG's own
// "rag/" namespace nested below it.
type S3 struct {
	client *minio.Client
	bucket string
	prefix string
}

// NewS3 constructs an S3-compatible original-document store from the existing
// workspace configuration structure.
func NewS3(cfg workspace.S3Config) (*S3, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, fmt.Errorf("s3 config requires endpoint/bucket/accessKey/secretKey")
	}
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("s3 client: %w", err)
	}
	return &S3{
		client: client,
		bucket: cfg.Bucket,
		prefix: strings.Trim(cfg.Prefix, "/"),
	}, nil
}

func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	clean, err := validateObjectKey(key)
	if err != nil {
		return err
	}
	if contentType == "" {
		contentType = mime.TypeByExtension(path.Ext(clean))
		if contentType == "" {
			contentType = "application/octet-stream"
		}
	}
	_, err = s.client.PutObject(ctx, s.bucket, s.objectKey(clean), r, size, minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		return fmt.Errorf("s3 put %s: %w", key, err)
	}
	return nil
}

func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	clean, err := validateObjectKey(key)
	if err != nil {
		return nil, err
	}
	obj, err := s.client.GetObject(ctx, s.bucket, s.objectKey(clean), minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	// MinIO GetObject is lazy. Stat forces an immediate not-found or access
	// error so callers do not discover it only on their first read.
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		return nil, err
	}
	return obj, nil
}

func (s *S3) DeletePrefix(ctx context.Context, prefix string) error {
	cleanPrefix, err := validateDeletePrefix(prefix)
	if err != nil {
		return err
	}
	fullPrefix := s.objectPrefix(cleanPrefix)
	for object := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{
		Prefix:    fullPrefix,
		Recursive: true,
	}) {
		if object.Err != nil {
			return fmt.Errorf("s3 list prefix %s: %w", prefix, object.Err)
		}
		if object.Key == "" {
			continue
		}
		if err := s.client.RemoveObject(ctx, s.bucket, object.Key, minio.RemoveObjectOptions{}); err != nil {
			return fmt.Errorf("s3 remove %s: %w", object.Key, err)
		}
	}
	return nil
}

func (s *S3) objectKey(key string) string {
	if s.prefix == "" {
		return key
	}
	return s.prefix + "/" + key
}

func (s *S3) objectPrefix(prefix string) string {
	if s.prefix == "" {
		return prefix
	}
	return s.prefix + "/" + prefix
}

func validateObjectKey(key string) (string, error) {
	if key == "" {
		return "", fmt.Errorf("object key 不能为空")
	}
	raw := strings.ReplaceAll(key, "\\", "/")
	if path.IsAbs(raw) {
		return "", fmt.Errorf("object key %q 必须是相对路径", key)
	}
	for _, segment := range strings.Split(raw, "/") {
		if segment == "." || segment == ".." {
			return "", fmt.Errorf("object key %q 包含非法路径段", key)
		}
	}
	clean := path.Clean(raw)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("object key %q 非法", key)
	}
	osPath := filepath.FromSlash(clean)
	if filepath.IsAbs(osPath) || filepath.VolumeName(osPath) != "" {
		return "", fmt.Errorf("object key %q 必须是相对路径", key)
	}
	return clean, nil
}

func validateDeletePrefix(prefix string) (string, error) {
	if !strings.HasSuffix(prefix, "/") {
		return "", fmt.Errorf("delete prefix %q 必须以 / 结尾", prefix)
	}
	raw := strings.ReplaceAll(prefix, "\\", "/")
	raw = strings.TrimSuffix(raw, "/")
	segments := strings.Split(raw, "/")
	if len(segments) < 3 {
		return "", fmt.Errorf("delete prefix %q 至少需要三段", prefix)
	}
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return "", fmt.Errorf("delete prefix %q 包含非法路径段", prefix)
		}
	}
	clean, err := validateObjectKey(raw)
	if err != nil {
		return "", err
	}
	return clean + "/", nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// os.Rename replaces files atomically on POSIX. Windows can reject a rename
// over an existing file, so retry after removing the old destination there.
func renameReplace(from, to string) error {
	if err := os.Rename(from, to); err == nil {
		return nil
	} else if _, statErr := os.Stat(to); statErr != nil {
		// If there is no destination to replace, preserve the original rename
		// failure instead of attempting an unrelated removal.
		return err
	}
	if err := os.Remove(to); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Rename(from, to)
}

var _ Store = (*LocalFS)(nil)
var _ Store = (*S3)(nil)
