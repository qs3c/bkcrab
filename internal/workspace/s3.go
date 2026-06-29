package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3 将对象存储在任何兼容 S3 的存储桶中（AWS S3, MinIO, Cloudflare
// R2, Backblaze B2, ...）。通过键前缀工作：代理 foo 的文件 bar.pdf
// 位于 s3://<bucket>/<prefix>/foo/bar.pdf。
//
// 使用 NewS3 从配置块构建，而不是直接构造结构体——
// minio 客户端需要特定的端点解析。
type S3 struct {
	client *minio.Client
	bucket string
	prefix string // prepended to every key; can be "" for bucket root
}

// S3Config 包含 NewS3 所需的配置项。字段命名遵循 bkcrab.json
// 约定，以便通过 encoding/json 干净地往返。
type S3Config struct {
	Endpoint  string `json:"endpoint"`            // e.g. "s3.amazonaws.com", "<acct>.r2.cloudflarestorage.com"
	Region    string `json:"region,omitempty"`    // AWS region; "" for R2/MinIO
	Bucket    string `json:"bucket"`              // target bucket
	Prefix    string `json:"prefix,omitempty"`    // key prefix; useful for multi-env share
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
	UseSSL    bool   `json:"useSSL"`              // default false — most managed services enforce SSL anyway
}

// NewS3 构建 S3 Store。返回包装的错误而不是 panic，
// 以便网关在配置错误时可以回退到 LocalFS 而不会崩溃。
func NewS3(cfg S3Config) (*S3, error) {
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

// key 是 <prefix>/<agentID>[/projects/<pid>[/<sid>]|/sessions/<sid>]/<path>，
// 始终使用正斜杠。布局与 LocalFS.scopeDir 匹配——项目聊天在项目下
// 获得自己的按会话子目录，以便并发聊天不会冲突，且聊天可以
// 通过单个重命名移入/移出。完整表格请参见 LocalFS.scopeDir。
func (s *S3) key(agentID, projectID, sessionID, p string) string {
	parts := []string{}
	if s.prefix != "" {
		parts = append(parts, s.prefix)
	}
	parts = append(parts, agentID)
	switch {
	case projectID != "" && sessionID != "":
		parts = append(parts, "projects", projectID, sessionID)
	case projectID != "":
		parts = append(parts, "projects", projectID)
	case sessionID != "":
		parts = append(parts, "sessions", sessionID)
	}
	parts = append(parts, path.Clean("/"+p)[1:])
	return strings.Join(parts, "/")
}

// scopePrefix 返回列表前缀。两者都为空时列出整个代理子树
//（管理员文件浏览器）；设置 project/session 时缩小范围。
func (s *S3) scopePrefix(agentID, projectID, sessionID string) string {
	parts := []string{}
	if s.prefix != "" {
		parts = append(parts, s.prefix)
	}
	parts = append(parts, agentID)
	switch {
	case projectID != "" && sessionID != "":
		parts = append(parts, "projects", projectID, sessionID)
	case projectID != "":
		parts = append(parts, "projects", projectID)
	case sessionID != "":
		parts = append(parts, "sessions", sessionID)
	}
	return strings.Join(parts, "/") + "/"
}

func (s *S3) Put(ctx context.Context, agentID, projectID, sessionID, p string, r io.Reader, size int64, contentType string) error {
	if contentType == "" {
		contentType = mime.TypeByExtension(filepath.Ext(p))
		if contentType == "" {
			contentType = "application/octet-stream"
		}
	}
	_, err := s.client.PutObject(ctx, s.bucket, s.key(agentID, projectID, sessionID, p), r, size, minio.PutObjectOptions{
		ContentType: contentType,
	})
	return err
}

func (s *S3) Get(ctx context.Context, agentID, projectID, sessionID, p string) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, s.key(agentID, projectID, sessionID, p), minio.GetObjectOptions{})
	if err != nil {
		return nil, mapS3Err(err)
	}
	// Minio 的 GetObject 返回惰性读取器——通过 Stat 探测，
	// 以便调用者提前获得 NotFound 错误，而不是在第一次 Read 时。
	if _, statErr := obj.Stat(); statErr != nil {
		obj.Close()
		return nil, mapS3Err(statErr)
	}
	return obj, nil
}

func (s *S3) Stat(ctx context.Context, agentID, projectID, sessionID, p string) (*ObjectInfo, error) {
	info, err := s.client.StatObject(ctx, s.bucket, s.key(agentID, projectID, sessionID, p), minio.StatObjectOptions{})
	if err != nil {
		return nil, mapS3Err(err)
	}
	return &ObjectInfo{
		Path:        p,
		Size:        info.Size,
		ContentType: info.ContentType,
		ModTime:     info.LastModified.UTC(),
	}, nil
}

func (s *S3) List(ctx context.Context, agentID, projectID, sessionID string) ([]ObjectInfo, error) {
	prefix := s.scopePrefix(agentID, projectID, sessionID)
	var out []ObjectInfo
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		rel := strings.TrimPrefix(obj.Key, prefix)
		ctype := obj.ContentType
		if ctype == "" {
			ctype = mime.TypeByExtension(filepath.Ext(rel))
		}
		out = append(out, ObjectInfo{
			Path:        rel,
			Size:        obj.Size,
			ContentType: ctype,
			ModTime:     obj.LastModified.UTC(),
		})
	}
	return out, nil
}

func (s *S3) Delete(ctx context.Context, agentID, projectID, sessionID, p string) error {
	err := s.client.RemoveObject(ctx, s.bucket, s.key(agentID, projectID, sessionID, p), minio.RemoveObjectOptions{})
	if err != nil && !isS3NotFound(err) {
		return err
	}
	return nil
}

// Move 将源作用域下的每个对象重新定位到目标作用域。
// S3 没有原生重命名：每个对象都通过服务端复制（CopyObject——
// 字节从不经过网关往返），然后删除原对象。
// 拒绝覆盖非空目标，以防失误合并两个聊天的文件；
// 返回 ErrMoveDestinationExists。
//
// 非原子操作：循环中途崩溃会导致源/目标处于部分迁移状态。
// 调用者应将 Move 视为尽力而为，并通过重新运行来修复
//（幂等——第二次调用发现源缺失时干净退出）。
func (s *S3) Move(ctx context.Context, agentID, fromProjectID, fromSessionID, toProjectID, toSessionID string) error {
	srcPrefix := s.scopePrefix(agentID, fromProjectID, fromSessionID)
	dstPrefix := s.scopePrefix(agentID, toProjectID, toSessionID)
	if srcPrefix == dstPrefix {
		return nil
	}
	// 目标必须为空。
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{
		Prefix:    dstPrefix,
		Recursive: true,
		MaxKeys:   1,
	}) {
		if obj.Err != nil {
			return obj.Err
		}
		if obj.Key != "" {
			return ErrMoveDestinationExists
		}
	}
	// 将每个源对象复制到对应的目标键，然后删除源。
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{
		Prefix:    srcPrefix,
		Recursive: true,
	}) {
		if obj.Err != nil {
			return obj.Err
		}
		if obj.Key == "" {
			continue
		}
		dstKey := dstPrefix + strings.TrimPrefix(obj.Key, srcPrefix)
		dstOpt := minio.CopyDestOptions{Bucket: s.bucket, Object: dstKey}
		srcOpt := minio.CopySrcOptions{Bucket: s.bucket, Object: obj.Key}
		if _, err := s.client.CopyObject(ctx, dstOpt, srcOpt); err != nil {
			return fmt.Errorf("s3 copy %s -> %s: %w", obj.Key, dstKey, err)
		}
		if err := s.client.RemoveObject(ctx, s.bucket, obj.Key, minio.RemoveObjectOptions{}); err != nil && !isS3NotFound(err) {
			return fmt.Errorf("s3 remove src %s: %w", obj.Key, err)
		}
	}
	return nil
}

// SignedURL 是我们在云部署中使用 S3 的主要原因：下载请求可以
// 完全绕过网关。TTL 通常为几分钟；浏览器使用一次 URL 后即丢弃。
func (s *S3) SignedURL(ctx context.Context, agentID, projectID, sessionID, p string, ttl time.Duration) (string, error) {
	reqParams := url.Values{}
	u, err := s.client.PresignedGetObject(ctx, s.bucket, s.key(agentID, projectID, sessionID, p), ttl, reqParams)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

// mapS3Err 将 minio 的错误规范化到我们的 ErrNotFound，
// 以便调用者无需了解 SDK 即可进行简单的 errors.Is 检查。
func mapS3Err(err error) error {
	if err == nil {
		return nil
	}
	if isS3NotFound(err) {
		return ErrNotFound
	}
	return err
}

func isS3NotFound(err error) bool {
	errResp := minio.ToErrorResponse(err)
	if errResp.Code == "NoSuchKey" || errResp.Code == "NoSuchObject" || errResp.Code == "NotFound" {
		return true
	}
	// 某些提供商用不同的方式包装它。
	return errors.Is(err, ErrNotFound) || strings.Contains(strings.ToLower(err.Error()), "not found")
}
