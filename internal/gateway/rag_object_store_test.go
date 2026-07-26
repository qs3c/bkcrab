package gateway

import (
	"testing"

	"github.com/qs3c/bkcrab/internal/config"
	ragobjects "github.com/qs3c/bkcrab/internal/rag/objects"
)

func TestNewRAGObjectStoreFollowsWorkspaceBackend(t *testing.T) {
	remoteTypes := []string{"aws-s3", "cloudflare-r2", "backblaze-b2", "aliyun-oss", "s3", "minio"}
	for _, storeType := range remoteTypes {
		t.Run(storeType, func(t *testing.T) {
			cfg := config.ObjectStoreCfg{
				Type: storeType,
				S3: config.ObjectStoreS3Cfg{
					Endpoint: "objects.example.test", Region: "cn-hangzhou",
					Bucket: "bucket", AccessKey: "access", SecretKey: "secret",
				},
				AccountID: "account",
			}
			store, err := newRAGObjectStore(cfg, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := store.(*ragobjects.S3); !ok {
				t.Fatalf("backend %q created %T, want S3-compatible store", storeType, store)
			}
		})
	}

	local, err := newRAGObjectStore(config.ObjectStoreCfg{Type: "local"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := local.(*ragobjects.LocalFS); !ok {
		t.Fatalf("local backend created %T", local)
	}
}
