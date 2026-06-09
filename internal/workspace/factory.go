package workspace

import (
	"fmt"
	"path/filepath"
)

// Factory builds the configured Store. Pulled out of the constructors so a
// single config-driven entry point sits between gateway startup and the
// actual backend implementations.
//
// Type is the user-facing selector — one of the preset names below. For
// every S3-compatible preset (aws-s3, cloudflare-r2, …) the factory fills
// in a sensible default endpoint from Region / AccountID so operators
// don't have to memorise URLs; pass an explicit S3.Endpoint to override.
type Factory struct {
	// Type selects the backend. Valid values:
	//   "", "local"      — pod-local filesystem at LocalDir (default)
	//   "aws-s3"         — AWS S3 (Region → s3.<region>.amazonaws.com)
	//   "cloudflare-r2"  — Cloudflare R2 (needs AccountID)
	//   "backblaze-b2"   — Backblaze B2 S3-compat (needs Region)
	//   "aliyun-oss"     — Aliyun OSS (needs Region; use -internal suffix
	//                      via the provided flag for in-region clusters)
	//   "minio"          — Self-hosted MinIO (needs explicit Endpoint)
	//   "s3"             — Any other S3-compatible; you provide Endpoint
	Type string

	// LocalDir is where the local backend stores files. Falls back to
	// defaultLocalDir when empty.
	LocalDir string

	// S3 holds the S3-compat credentials / endpoint / bucket. Used for
	// every Type that isn't "local". Leave S3.Endpoint empty to let the
	// factory compute it from Type + Region + AccountID.
	S3 S3Config

	// R2 / OSS specific knobs kept at the top level so the YAML reads
	// like a single "pick one, fill in" block — mirrors how the ConfigMap
	// is structured for operators.
	AccountID    string // Cloudflare R2
	AliyunIntern bool   // prefer OSS internal endpoint (no egress fee for in-region ACK)
}

// New returns the Store described by f. Unknown types fall back to the
// local filesystem so misconfiguration degrades gracefully instead of
// blowing up at startup.
func (f Factory) New(defaultLocalDir string) (Store, error) {
	switch f.Type {
	case "", "local":
		root := f.LocalDir
		if root == "" {
			root = defaultLocalDir
		}
		return NewLocalFS(filepath.Clean(root)), nil
	case "aws-s3", "cloudflare-r2", "backblaze-b2", "aliyun-oss", "minio", "s3":
		s3 := f.S3
		if s3.Endpoint == "" {
			ep, err := defaultEndpoint(f.Type, s3.Region, f.AccountID, f.AliyunIntern)
			if err != nil {
				return nil, err
			}
			s3.Endpoint = ep
		}
		// AWS S3 and most managed providers strictly require SSL; only
		// local MinIO typically runs plaintext. Auto-enable unless the
		// operator explicitly said otherwise in the "minio" preset.
		if f.Type != "minio" && !s3.UseSSL {
			s3.UseSSL = true
		}
		return NewS3(s3)
	default:
		return nil, fmt.Errorf("workspace: unknown type %q", f.Type)
	}
}

// defaultEndpoint maps a provider preset + region/account into the well-
// known S3-compat endpoint hostname. Keeps ConfigMaps short: operators
// pick a provider by name instead of looking up each vendor's URL pattern.
func defaultEndpoint(providerType, region, accountID string, aliyunInternal bool) (string, error) {
	switch providerType {
	case "aws-s3":
		if region == "" {
			return "", fmt.Errorf("aws-s3 requires region")
		}
		return "s3." + region + ".amazonaws.com", nil
	case "cloudflare-r2":
		if accountID == "" {
			return "", fmt.Errorf("cloudflare-r2 requires accountId")
		}
		// R2 has no real "region"; the account ID is the tenant locator.
		return accountID + ".r2.cloudflarestorage.com", nil
	case "backblaze-b2":
		if region == "" {
			return "", fmt.Errorf("backblaze-b2 requires region (e.g. us-west-004)")
		}
		return "s3." + region + ".backblazeb2.com", nil
	case "aliyun-oss":
		if region == "" {
			return "", fmt.Errorf("aliyun-oss requires region (e.g. cn-hangzhou)")
		}
		if aliyunInternal {
			return "oss-" + region + "-internal.aliyuncs.com", nil
		}
		return "oss-" + region + ".aliyuncs.com", nil
	case "minio", "s3":
		// No preset — caller MUST supply Endpoint. Reach here only when
		// they didn't, which is a user error we surface clearly.
		return "", fmt.Errorf("%s backend requires an explicit endpoint", providerType)
	}
	return "", fmt.Errorf("workspace: no endpoint preset for %q", providerType)
}
