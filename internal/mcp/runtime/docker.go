package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type DockerClient interface {
	Ensure(ctx context.Context, spec ContainerSpec) (ContainerRef, error)
	Stop(ctx context.Context, name string) error
}

type ContainerSpec struct {
	Name          string
	Image         string
	ConfigDir     string
	ContainerPort int
	Protocol      string
}

type ContainerRef struct {
	ID           string
	Name         string
	BaseURL      string
	ExternalPort int
	Running      bool
}

type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.CombinedOutput()
}

type CLIClient struct {
	Runner CommandRunner
}

func NewCLIClient() *CLIClient {
	return &CLIClient{Runner: execRunner{}}
}

func NewDockerCLIClient() *CLIClient {
	return NewCLIClient()
}

func (c *CLIClient) Ensure(ctx context.Context, spec ContainerSpec) (ContainerRef, error) {
	if c.Runner == nil {
		c.Runner = execRunner{}
	}
	if err := writeGatewayConfig(spec); err != nil {
		return ContainerRef{}, err
	}
	ref, exists, err := c.inspect(ctx, spec.Name, spec.ContainerPort)
	if err != nil {
		return ContainerRef{}, err
	}
	if exists {
		if !ref.Running {
			if out, err := c.Runner.Run(ctx, "docker", "start", spec.Name); err != nil {
				return ContainerRef{}, fmt.Errorf("docker start %s: %w: %s", spec.Name, err, string(out))
			}
		}
		return c.resolvePort(ctx, spec.Name, spec.ContainerPort, ref.ID)
	}

	volume := filepath.Clean(spec.ConfigDir) + ":/app/vm"
	out, err := c.Runner.Run(ctx, "docker", "run", "-d",
		"--name", spec.Name,
		"-p", fmt.Sprintf("127.0.0.1::%d", spec.ContainerPort),
		"-v", volume,
		"--restart", "unless-stopped",
		spec.Image,
	)
	if err != nil {
		return ContainerRef{}, fmt.Errorf("docker run %s: %w: %s", spec.Name, err, string(out))
	}
	return c.resolvePort(ctx, spec.Name, spec.ContainerPort, strings.TrimSpace(string(out)))
}

func (c *CLIClient) Stop(ctx context.Context, name string) error {
	if c.Runner == nil {
		c.Runner = execRunner{}
	}
	out, err := c.Runner.Run(ctx, "docker", "stop", name)
	if err != nil {
		return fmt.Errorf("docker stop %s: %w: %s", name, err, string(out))
	}
	return nil
}

func (c *CLIClient) inspect(ctx context.Context, name string, port int) (ContainerRef, bool, error) {
	out, err := c.Runner.Run(ctx, "docker", "inspect", name)
	if err != nil {
		return ContainerRef{}, false, nil
	}
	var rows []struct {
		ID    string `json:"Id"`
		Name  string `json:"Name"`
		State struct {
			Running bool `json:"Running"`
		} `json:"State"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return ContainerRef{}, false, fmt.Errorf("parse docker inspect: %w", err)
	}
	if len(rows) == 0 {
		return ContainerRef{}, false, nil
	}
	ref := ContainerRef{ID: rows[0].ID, Name: strings.TrimPrefix(rows[0].Name, "/"), Running: rows[0].State.Running}
	if ref.Running {
		portRef, err := c.resolvePort(ctx, name, port, ref.ID)
		if err != nil {
			return ContainerRef{}, true, err
		}
		ref.BaseURL = portRef.BaseURL
		ref.ExternalPort = portRef.ExternalPort
	}
	return ref, true, nil
}

func (c *CLIClient) resolvePort(ctx context.Context, name string, containerPort int, id string) (ContainerRef, error) {
	out, err := c.Runner.Run(ctx, "docker", "port", name, fmt.Sprintf("%d/tcp", containerPort))
	if err != nil {
		return ContainerRef{}, fmt.Errorf("docker port %s: %w: %s", name, err, string(out))
	}
	host, portText, err := net.SplitHostPort(strings.TrimSpace(string(out)))
	if err != nil {
		return ContainerRef{}, fmt.Errorf("parse docker port %q: %w", strings.TrimSpace(string(out)), err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return ContainerRef{}, fmt.Errorf("parse docker host port %q: %w", portText, err)
	}
	if host == "" || host == "::" || host == "0.0.0.0" || host == "[::]" {
		host = "127.0.0.1"
	}
	return ContainerRef{
		ID:           id,
		Name:         name,
		BaseURL:      fmt.Sprintf("http://%s:%d", host, port),
		ExternalPort: port,
		Running:      true,
	}, nil
}

func writeGatewayConfig(spec ContainerSpec) error {
	if err := os.MkdirAll(spec.ConfigDir, 0o755); err != nil {
		return fmt.Errorf("create gateway config dir: %w", err)
	}
	protocol := spec.Protocol
	if protocol == "" {
		protocol = "all"
	}
	cfg := map[string]any{
		"LogLevel":        0,
		"WorkspacePath":   "./vm",
		"Bind":            fmt.Sprintf("[::]:%d", spec.ContainerPort),
		"Auth":            map[string]any{"Enabled": false, "ApiKey": "bkcrab-local"},
		"GatewayProtocol": protocol,
		"McpServiceMgrConfig": map[string]any{
			"McpServiceRetryCount": 3,
		},
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal gateway config: %w", err)
	}
	if err := os.WriteFile(filepath.Join(spec.ConfigDir, "config.json"), data, 0o600); err != nil {
		return fmt.Errorf("write gateway config: %w", err)
	}
	return nil
}
