package workspace

import (
	"fmt"
	"path/filepath"
)

// Factory 构建已配置的 Store。从构造函数中提取出来，
// 以便在网关启动和实际后端实现之间有一个配置驱动的入口点。
//
// Type 是面向用户的选择器——下面预设名称之一。对于每个兼容 S3 的预设
//（aws-s3, cloudflare-r2, …），工厂从 Region / AccountID 填充合理的
// 默认端点，使运维人员无需记住 URL；传递显式的 S3.Endpoint 可覆盖。
type Factory struct {
	// Type 选择后端。有效值：
	//   "", "local"      — LocalDir 的 Pod 本地文件系统（默认）
	//   "aws-s3"         — AWS S3 (Region → s3.<region>.amazonaws.com)
	//   "cloudflare-r2"  — Cloudflare R2（需要 AccountID）
	//   "backblaze-b2"   — Backblaze B2 S3-compat（需要 Region）
	//   "aliyun-oss"     — 阿里云 OSS（需要 Region；通过提供的标志使用
	//                      -internal 后缀用于同区域集群）
	//   "minio"          — 自托管 MinIO（需要显式 Endpoint）
	//   "s3"             — 任何其他 S3 兼容存储；你需要提供 Endpoint
	Type string

	// LocalDir 是本地后端存储文件的位置。为空时回退到 defaultLocalDir。
	LocalDir string

	// S3 包含 S3 兼容凭证/端点/存储桶。用于所有非 "local" 的 Type。
	// 将 S3.Endpoint 留空以让工厂从 Type + Region + AccountID 计算。
	S3 S3Config

	// R2 / OSS 特定的配置项保持在顶层，以便 YAML 读起来像
	// 一个"选一个，填好"的块——镜像 ConfigMap 为运维人员构建的方式。
	AccountID    string // Cloudflare R2
	AliyunIntern bool   // 优先使用 OSS 内网端点（同区域 ACK 无出站费用）
}

// New 返回由 f 描述的 Store。未知类型回退到本地文件系统，
// 使配置错误优雅降级而不是在启动时崩溃。
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
		// AWS S3 和大多数托管提供商严格要求 SSL；只有本地 MinIO
		// 通常运行明文。自动启用，除非运维人员在 "minio" 预设中
		// 明确另有说明。
		if f.Type != "minio" && !s3.UseSSL {
			s3.UseSSL = true
		}
		return NewS3(s3)
	default:
		return nil, fmt.Errorf("workspace: unknown type %q", f.Type)
	}
}

// defaultEndpoint 将提供商预设 + region/account 映射到众所周知的
// S3 兼容端点主机名。保持 ConfigMap 简洁：运维人员按名称选择提供商，
// 而不是查找每个供应商的 URL 模式。
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
		// R2 没有真正的 "region"；account ID 是租户定位器。
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
		// 没有预设——调用者必须提供 Endpoint。只有当他们没有提供时
		// 才会到达这里，这是一个我们清楚暴露的用户错误。
		return "", fmt.Errorf("%s backend requires an explicit endpoint", providerType)
	}
	return "", fmt.Errorf("workspace: no endpoint preset for %q", providerType)
}
