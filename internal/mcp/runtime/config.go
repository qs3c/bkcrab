package runtime

import (
	"path/filepath"
	"time"

	"github.com/qs3c/bkcrab/internal/config"
)

const defaultImage = "ghcr.io/lucky-aeon/mcp-gateway:latest"

type Config struct {
	Enabled       bool
	Image         string
	RuntimeDir    string
	ContainerPort int
	Protocol      string
	IdleTTL       time.Duration
	// DeployTimeout 限制一次 Deploy（含 docker 拉镜像/启动 + /deploy 调用）的总时长，
	// 使卡死的 docker 守护进程或网关无法无限期阻塞 agent 构建（进而阻塞用户空间加载锁）。
	DeployTimeout time.Duration
	// RequestTimeout 是网关 HTTP 客户端（/deploy 及运行时工具调用）的单请求超时。
	RequestTimeout time.Duration
}

func FromEnv(env config.EnvMCPGateway) Config {
	home, _ := config.HomeDir()
	dir := env.RuntimeDir
	if dir == "" {
		dir = filepath.Join(home, "mcp-gateways")
	}
	image := env.Image
	if image == "" {
		image = defaultImage
	}
	port := env.ContainerPort
	if port == 0 {
		port = 8080
	}
	protocol := env.Protocol
	if protocol == "" {
		protocol = "all"
	}
	idle := time.Duration(env.IdleTTLSec) * time.Second
	if idle <= 0 {
		idle = 30 * time.Minute
	}
	return Config{Enabled: env.Enabled, Image: image, RuntimeDir: dir, ContainerPort: port, Protocol: protocol, IdleTTL: idle}
}
