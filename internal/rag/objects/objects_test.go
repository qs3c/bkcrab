package objects

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/workspace"
)

func TestKeyShape(t *testing.T) {
	k := Key("u1", "kb1", "doc1", "手册 v2.pdf")
	if k != "rag/u1/kb1/doc1/手册 v2.pdf" {
		t.Fatalf("key = %q", k)
	}
	if strings.Contains(Key("u1", "kb1", "doc1", "../../etc/passwd"), "..") {
		t.Fatal("文件名必须被清洗,不能带路径穿越")
	}
	if got := Key("u1", "kb1", "doc1", `folder\guide.md`); got != "rag/u1/kb1/doc1/guide.md" {
		t.Fatalf("Windows 路径文件名未清洗: %q", got)
	}
}

func TestLocalFSRoundTrip(t *testing.T) {
	s := NewLocalFS(t.TempDir())
	ctx := context.Background()
	key := Key("u1", "kb1", "doc1", "a.md")
	if err := s.Put(ctx, key, bytes.NewReader([]byte("hello")), 5, "text/markdown"); err != nil {
		t.Fatal(err)
	}
	rc, err := s.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(rc)
	if err != nil {
		_ = rc.Close()
		t.Fatal(err)
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	if string(b) != "hello" {
		t.Fatalf("content = %q", b)
	}
	if err := s.DeletePrefix(ctx, "rag/u1/kb1/"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, key); err == nil {
		t.Fatal("删除前缀后应取不到")
	}
}

func TestLocalFSOverwriteAndPathGuards(t *testing.T) {
	s := NewLocalFS(t.TempDir())
	ctx := context.Background()
	key := Key("u1", "kb1", "doc1", "a.txt")
	if err := s.Put(ctx, key, strings.NewReader("old"), 3, "text/plain"); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, key, strings.NewReader("new"), 3, "text/plain"); err != nil {
		t.Fatalf("覆盖写入失败: %v", err)
	}
	rc, err := s.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(b) != "new" {
		t.Fatalf("覆盖后 content=%q err=%v", b, err)
	}

	if err := s.Put(ctx, "../escape", strings.NewReader("x"), 1, ""); err == nil {
		t.Fatal("Put 必须拒绝路径穿越")
	}
	if _, err := s.Get(ctx, `..\escape`); err == nil {
		t.Fatal("Get 必须拒绝 Windows 路径穿越")
	}
	for _, prefix := range []string{"rag/u1", "rag/u1/", "../u1/kb1/"} {
		if err := s.DeletePrefix(ctx, prefix); err == nil {
			t.Fatalf("DeletePrefix 应拒绝不安全前缀 %q", prefix)
		}
	}
}

func TestNewS3UsesWorkspaceConfigWithoutNetwork(t *testing.T) {
	if _, err := NewS3(workspace.S3Config{}); err == nil {
		t.Fatal("缺失必要 S3 配置应报错")
	}
	s, err := NewS3(workspace.S3Config{
		Endpoint:  "localhost:9000",
		Region:    "test-region",
		Bucket:    "documents",
		Prefix:    "/tenant/dev/",
		AccessKey: "access",
		SecretKey: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := s.objectKey("rag/u1/kb1/doc1/a.md"), "tenant/dev/rag/u1/kb1/doc1/a.md"; got != want {
		t.Fatalf("S3 object key = %q, want %q", got, want)
	}
	if got, want := s.objectPrefix("rag/u1/kb1/"), "tenant/dev/rag/u1/kb1/"; got != want {
		t.Fatalf("S3 prefix = %q, want %q", got, want)
	}
}
