package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
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
	Runner     CommandRunner
	DockerHost string
}

func NewCLIClient() *CLIClient {
	return &CLIClient{
		Runner:     execRunner{},
		DockerHost: os.Getenv("DOCKER_HOST"),
	}
}

func NewDockerCLIClient() *CLIClient {
	return NewCLIClient()
}

func (c *CLIClient) Ensure(ctx context.Context, spec ContainerSpec) (ContainerRef, error) {
	if c.Runner == nil {
		c.Runner = execRunner{}
	}
	protocol := gatewayProtocol(spec.Protocol)
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
	publishHost := "127.0.0.1"
	if c.remoteDaemonHost() != "" {
		// The Docker daemon may run in a sibling DinD container. Binding the
		// published port to that daemon's loopback interface would make it
		// unreachable from bkcrab, so expose it on the daemon container's
		// network interface and connect through DOCKER_HOST instead.
		publishHost = "0.0.0.0"
	}
	out, err := c.Runner.Run(ctx, "docker", "run", "-d",
		"--name", spec.Name,
		"-p", fmt.Sprintf("%s::%d", publishHost, spec.ContainerPort),
		"-v", volume,
		"--restart", "unless-stopped",
		spec.Image,
		"-cfg", "/app/vm/config.json",
		"-protocol", protocol,
		"-yes",
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
	var host, portText string
	var parseErr error
	for _, mapping := range strings.Fields(string(out)) {
		host, portText, parseErr = net.SplitHostPort(mapping)
		if parseErr == nil {
			break
		}
	}
	if portText == "" {
		if parseErr == nil {
			parseErr = fmt.Errorf("no published port found")
		}
		return ContainerRef{}, fmt.Errorf("parse docker port %q: %w", strings.TrimSpace(string(out)), parseErr)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return ContainerRef{}, fmt.Errorf("parse docker host port %q: %w", portText, err)
	}
	if remoteHost := c.remoteDaemonHost(); remoteHost != "" {
		host = remoteHost
	} else if host == "" || host == "::" || host == "0.0.0.0" || host == "[::]" {
		host = "127.0.0.1"
	}
	return ContainerRef{
		ID:           id,
		Name:         name,
		BaseURL:      "http://" + net.JoinHostPort(host, strconv.Itoa(port)),
		ExternalPort: port,
		Running:      true,
	}, nil
}

func (c *CLIClient) remoteDaemonHost() string {
	raw := strings.TrimSpace(c.DockerHost)
	if raw == "" {
		return ""
	}
	endpoint, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	switch strings.ToLower(endpoint.Scheme) {
	case "tcp", "http", "https", "ssh":
		return endpoint.Hostname()
	default:
		return ""
	}
}

func writeGatewayConfig(spec ContainerSpec) error {
	if err := os.MkdirAll(spec.ConfigDir, 0o755); err != nil {
		return fmt.Errorf("create gateway config dir: %w", err)
	}
	protocol := gatewayProtocol(spec.Protocol)
	cfg := map[string]any{
		"LogLevel":        0,
		"WorkspacePath":   "/app/vm",
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

func gatewayProtocol(protocol string) string {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "", "all", "streamable-http", "streamable_http", "streamhttp":
		// BkCrab connects to the gateway through /stream. Gateway v2.1.0
		// does not register that route when its protocol is "all", so use
		// the gateway's streamhttp spelling explicitly.
		return "streamhttp"
	default:
		return strings.ToLower(strings.TrimSpace(protocol))
	}
}
