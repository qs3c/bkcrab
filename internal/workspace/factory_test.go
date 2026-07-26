package workspace

import "testing"

func TestResolveS3ConfigExpandsManagedPresets(t *testing.T) {
	tests := []struct {
		name     string
		factory  Factory
		endpoint string
		useSSL   bool
	}{
		{
			name:     "aws",
			factory:  Factory{Type: "aws-s3", S3: S3Config{Region: "cn-north-1"}},
			endpoint: "s3.cn-north-1.amazonaws.com",
			useSSL:   true,
		},
		{
			name:     "cloudflare",
			factory:  Factory{Type: "cloudflare-r2", AccountID: "acct"},
			endpoint: "acct.r2.cloudflarestorage.com",
			useSSL:   true,
		},
		{
			name:     "aliyun internal",
			factory:  Factory{Type: "aliyun-oss", AliyunIntern: true, S3: S3Config{Region: "cn-hangzhou"}},
			endpoint: "oss-cn-hangzhou-internal.aliyuncs.com",
			useSSL:   true,
		},
		{
			name:     "minio keeps explicit transport",
			factory:  Factory{Type: "minio", S3: S3Config{Endpoint: "minio:9000"}},
			endpoint: "minio:9000",
			useSSL:   false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := test.factory.ResolveS3Config()
			if err != nil {
				t.Fatal(err)
			}
			if got.Endpoint != test.endpoint || got.UseSSL != test.useSSL {
				t.Fatalf("resolved endpoint=%q useSSL=%v", got.Endpoint, got.UseSSL)
			}
		})
	}
}

func TestResolveS3ConfigRejectsLocalBackend(t *testing.T) {
	if _, err := (Factory{Type: "local"}).ResolveS3Config(); err == nil {
		t.Fatal("local backend must not resolve as S3")
	}
}
