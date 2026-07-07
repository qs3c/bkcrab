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
